package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/crypto/sha3"
)

// ---------------------------------------------------------------------------
// On-chain price source (Robinhood Chain, chain id 4663).
//
// Prices come straight from the token's Uniswap pool:
//   - v2: Sync(reserve0, reserve1) events        → reserve ratio
//   - v3: Swap(…, sqrtPriceX96, …) events        → (sqrtPriceX96 / 2^96)^2
//   - v4: PoolManager Swap(id, …, sqrtPriceX96…) → same, filtered by pool id
//
// The pool is found by looking at who the token was transferred to/from around
// the call, so no indexer or factory address is needed. The pool price is in
// the pool's other asset (WETH, USDG, or a Stock Token on Long.xyz); it is
// converted to USD with a Chainlink feed (or 1.0 for stablecoins).
//
// Needs an archive RPC (historical eth_call + eth_getLogs).
// ---------------------------------------------------------------------------

type onchainConfig struct {
	RPCURL          string
	RPS             int    // requests per second (0 = no limit)
	MaxInflight     int    // requests in flight at the same time
	Parallel        int    // eth_getLogs chunks fetched at the same time within one scan
	LogCache        int    // eth_getLogs results kept in memory, in logs (0 = off); repeat calls of a token reuse them
	LogChunk        uint64 // max blocks per eth_getLogs (halved automatically when the node refuses)
	MinLogChunk     uint64
	DiscoveryBlocks uint64 // look this many blocks either side of the call for the token's transfers
	PoolManagerV4   string
	WETH            string
	Stables         map[string]bool   // lower-case addresses worth $1
	Feeds           map[string]string // quote token (lower) or "eth" → Chainlink aggregator
	EthUSDPool      string            // v3 WETH/stable pool, used when no ETH feed is configured
	PriceLookback   uint64            // how far back (blocks) to look for the last feed update / ETH swap

	// Optional Ethereum mainnet archive node: Chainlink feeds are read there at the
	// mainnet block matching the Robinhood Chain block's timestamp.
	MainnetRPCURL string
	MainnetRPS    int
	MainnetFeeds  map[string]string // quote token (lower) or "eth" → Chainlink feed on Ethereum mainnet

	EntryDelay time.Duration // realistic entry: the pool price this long after the post (default 60s)

	// RugLiqUSD: a pool whose quote side (the ETH/WETH, USDG, stock token …
	// it holds, valued in USD) is below this many USD is rugged
	// (SCOUT_RUG_LIQ_USD, default 500; 0 = the USD check is off, an empty pool
	// still counts).
	RugLiqUSD float64
}

// defaultRugLiqUSD: a pool's quote side below this (USD) = rugged (SCOUT_RUG_LIQ_USD).
const defaultRugLiqUSD = 500

// poolDepthUSD turns a quote-side USD value into what current_liquidity_usd
// stores: the pool's depth counted on both sides, 2 × the quote side (the
// column's meaning since before the rug guard). The rug threshold is compared
// with the quote side itself.
func poolDepthUSD(quoteSideUSD float64) float64 { return 2 * quoteSideUSD }

// Chainlink ETH/USD on Ethereum mainnet (8 decimals).
const mainnetEthUsdFeed = "0x5f4ec3df9cbd43714fe2740f5e3616155c5b8419"

const zeroAddr = "0x0000000000000000000000000000000000000000"

func loadOnchainConfig() (onchainConfig, error) {
	oc := onchainConfig{
		RPCURL:          env("SCOUT_RPC_URL", "http://localhost:8540"),
		RPS:             0, // no limit: the node is our own; MaxInflight bounds the load
		MaxInflight:     64,
		LogChunk:        200_000,
		MinLogChunk:     200,
		LogCache:        300_000,
		Parallel:        8,
		DiscoveryBlocks: 18_000,    // ≈ 30 min at ~10 blocks/s
		PriceLookback:   8_640_000, // ≈ 10 days
		PoolManagerV4:   strings.ToLower(env("SCOUT_V4_POOL_MANAGER", "0x8366a39cc670b4001a1121b8f6a443a643e40951")),
		WETH:            strings.ToLower(env("SCOUT_WETH", "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73")),
		EthUSDPool:      strings.ToLower(env("SCOUT_ETH_USD_POOL", "0x69BfaF19C9f377BB306a89aEd9F6B07e2c1a8d9a")),
		Stables:         map[string]bool{},
		Feeds:           map[string]string{},
		MainnetRPCURL:   env("SCOUT_MAINNET_RPC_URL", ""),
		MainnetRPS:      0,
		MainnetFeeds:    map[string]string{"eth": mainnetEthUsdFeed},
		EntryDelay:      envDur("SCOUT_ENTRY_DELAY", 60*time.Second),
	}
	if oc.EntryDelay < 0 {
		return oc, fmt.Errorf("SCOUT_ENTRY_DELAY: want a duration >= 0")
	}
	oc.RugLiqUSD = defaultRugLiqUSD
	if v := env("SCOUT_RUG_LIQ_USD", ""); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return oc, fmt.Errorf("SCOUT_RUG_LIQ_USD=%q: want a number >= 0", v)
		}
		oc.RugLiqUSD = f
	}
	if v := env("SCOUT_MAINNET_RPC_RPS", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return oc, fmt.Errorf("SCOUT_MAINNET_RPC_RPS=%q: want a number >= 0 (0 = no limit)", v)
		}
		oc.MainnetRPS = n
	}
	// SCOUT_MAINNET_CHAINLINK_FEEDS="eth=0xFeedOnEthereum,0xQuoteTokenOnRobinhood=0xFeedOnEthereum"
	if err := parseFeedMap(env("SCOUT_MAINNET_CHAINLINK_FEEDS", ""), "SCOUT_MAINNET_CHAINLINK_FEEDS", oc.MainnetFeeds); err != nil {
		return oc, err
	}
	num := func(key string, dst *uint64) error {
		if v := env(key, ""); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil || n == 0 {
				return fmt.Errorf("%s=%q: want a positive number", key, v)
			}
			*dst = n
		}
		return nil
	}
	if err := num("SCOUT_RPC_LOG_CHUNK", &oc.LogChunk); err != nil {
		return oc, err
	}
	if v := env("SCOUT_RPC_PARALLEL", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return oc, fmt.Errorf("SCOUT_RPC_PARALLEL=%q: want a number from 1 to 64", v)
		}
		oc.Parallel = n
	}
	if v := env("SCOUT_RPC_LOG_CACHE", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return oc, fmt.Errorf("SCOUT_RPC_LOG_CACHE=%q: want a number >= 0 (0 = off)", v)
		}
		oc.LogCache = n
	}
	if err := num("SCOUT_DISCOVERY_BLOCKS", &oc.DiscoveryBlocks); err != nil {
		return oc, err
	}
	if err := num("SCOUT_PRICE_LOOKBACK_BLOCKS", &oc.PriceLookback); err != nil {
		return oc, err
	}
	if v := env("SCOUT_RPC_RPS", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return oc, fmt.Errorf("SCOUT_RPC_RPS=%q: want a number >= 0 (0 = no limit)", v)
		}
		oc.RPS = n
	}
	if v := env("SCOUT_RPC_MAX_INFLIGHT", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1024 {
			return oc, fmt.Errorf("SCOUT_RPC_MAX_INFLIGHT=%q: want a number from 1 to 1024", v)
		}
		oc.MaxInflight = n
	}
	for _, a := range splitList(env("SCOUT_STABLES", "0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168")) { // USDG
		oc.Stables[a] = true
	}
	// SCOUT_CHAINLINK_FEEDS="eth=0xFeed,0xStockToken=0xFeed,…" (feeds on Robinhood Chain)
	if err := parseFeedMap(env("SCOUT_CHAINLINK_FEEDS", ""), "SCOUT_CHAINLINK_FEEDS", oc.Feeds); err != nil {
		return oc, err
	}
	return oc, nil
}

// parseFeedMap parses "token=feed,token=feed" into dst (keys and values lower-cased).
func parseFeedMap(v, name string, dst map[string]string) error {
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 || !strings.HasPrefix(strings.TrimSpace(kv[1]), "0x") {
			return fmt.Errorf("%s: bad entry %q (want token=feedAddress)", name, pair)
		}
		dst[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.ToLower(strings.TrimSpace(kv[1]))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Minimal ABI helpers
// ---------------------------------------------------------------------------

func keccak(s string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(s))
	return h.Sum(nil)
}

func topicOf(sig string) string    { return "0x" + hex.EncodeToString(keccak(sig)) }
func selectorOf(sig string) string { return "0x" + hex.EncodeToString(keccak(sig)[:4]) }

var (
	topicTransfer = topicOf("Transfer(address,address,uint256)")
	topicSyncV2   = topicOf("Sync(uint112,uint112)")
	topicSwapV3   = topicOf("Swap(address,address,int256,int256,uint160,uint128,int24)")
	topicSwapV4   = topicOf("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)")
	topicInitV4   = topicOf("Initialize(bytes32,address,address,uint24,int24,address,uint160,int24)")

	selDecimals    = selectorOf("decimals()")
	selSymbol      = selectorOf("symbol()")
	selName        = selectorOf("name()")
	selToken0      = selectorOf("token0()")
	selToken1      = selectorOf("token1()")
	selGetReserves = selectorOf("getReserves()")
	selSlot0       = selectorOf("slot0()")
	selBalanceOf   = selectorOf("balanceOf(address)")
	selLatestRound = selectorOf("latestRoundData()")

	// Chainlink: proxies don't emit events; the underlying aggregator(s) do.
	topicAnswerUpdated = topicOf("AnswerUpdated(int256,uint256,uint256)")
	selAggregator      = selectorOf("aggregator()")
	selPhaseID         = selectorOf("phaseId()")
	selPhaseAggregator = selectorOf("phaseAggregators(uint16)")
)

func unhex(s string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	return b
}

// word returns the i-th 32-byte word of ABI data as an unsigned integer.
func word(data []byte, i int) *big.Int {
	if len(data) < (i+1)*32 {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(data[i*32 : (i+1)*32])
}

// signedWord interprets the i-th word as a two's-complement int256.
func signedWord(data []byte, i int) *big.Int {
	v := word(data, i)
	if v.Bit(255) == 1 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v
}

func addrFromWord(b []byte) string {
	if len(b) < 32 {
		return zeroAddr
	}
	return "0x" + hex.EncodeToString(b[12:32])
}

func addrTopic(addr string) string {
	return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(addr), "0x")
}

func pow10(n int) float64 { return math.Pow(10, float64(n)) }

func bigToFloat(v *big.Int) float64 {
	f, _ := new(big.Float).SetInt(v).Float64()
	return f
}

// priceFromSqrtX96: price of token0 in token1 (decimal-adjusted).
func priceFromSqrtX96(sqrtP *big.Int, dec0, dec1 int) float64 {
	if sqrtP.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(sqrtP)
	f.Quo(f, new(big.Float).SetPrec(256).SetInt(new(big.Int).Lsh(big.NewInt(1), 96)))
	f.Mul(f, f)
	p, _ := f.Float64()
	return p * pow10(dec0-dec1)
}

// priceFromReserves: price of token0 in token1 (decimal-adjusted).
func priceFromReserves(r0, r1 *big.Int, dec0, dec1 int) float64 {
	if r0.Sign() == 0 {
		return 0
	}
	return bigToFloat(r1) / bigToFloat(r0) * pow10(dec0-dec1)
}

// ---------------------------------------------------------------------------
// JSON-RPC client
// ---------------------------------------------------------------------------

type rpcLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	TxHash      string   `json:"transactionHash"`
	LogIndex    string   `json:"logIndex"`
}

func (l rpcLog) block() uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(l.BlockNumber, "0x"), 16, 64)
	return n
}
func (l rpcLog) index() uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(l.LogIndex, "0x"), 16, 64)
	return n
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcClient struct {
	url      string
	http     *http.Client
	id       atomic.Int64
	requests atomic.Int64                // total JSON-RPC requests sent (for progress logs)
	inflight atomic.Pointer[rpcInflight] // the request being waited on right now (for heartbeat logs)
	head     atomic.Uint64               // newest block number seen (results near it are not cached)
	parallel int                         // log chunks fetched at once per scan
	slots    chan struct{}               // bounds the requests in flight

	sfMu sync.Mutex
	sf   map[string]*logFlight // identical eth_getLogs requests in flight: asked once, shared

	// eth_getLogs block ranges. Every scan starts at maxChunk (SCOUT_RPC_LOG_CHUNK)
	// and keeps its own size: only a "range / result too large" refusal halves
	// it, for that scan alone, and it grows back after a few answered ranges.
	// Timeouts and other passing errors are retried at the same size.
	maxChunk uint64
	minChunk uint64
	splits   atomic.Int64 // ranges split after a refusal (whole run, for -check output)
	notes    rangeNotes   // rate-limited log lines about range size changes
	logs     *logCache    // finished eth_getLogs chunks (nil = off)

	mu   sync.Mutex
	next time.Time
	gap  time.Duration

	cacheMu sync.Mutex
	times   map[uint64]int64 // block → timestamp
	anchors []uint64         // sorted blocks with known timestamps
	meta    map[string]tokenMeta
	labels  map[string]tokenLabel // name()/symbol() as read from the contract, for display
}

type tokenMeta struct {
	Decimals int
	Symbol   string // symbol(), or the start of the address when the token has none
	Name     string // name(), cleaned; "" when the token has none
}

// tokenLabel is what a token contract says about itself, cleaned for storage
// and display ("" = the contract has no such value).
type tokenLabel struct {
	Name   string
	Symbol string
}

const (
	maxTokenNameLen   = 100 // characters stored for a token name
	maxTokenSymbolLen = 32  // characters stored for a token symbol
)

// cleanTokenText makes a string read from an arbitrary contract safe to store:
// valid UTF-8, no control characters (and no text-direction overrides, which
// can make a name display as something else), trimmed, at most max characters.
func cleanTokenText(s string, max int) string {
	s = strings.TrimRight(s, "\x00") // bytes32-style values are zero-padded
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == unicode.ReplacementChar:
			return -1
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F:
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if rs := []rune(s); len(rs) > max {
		s = strings.TrimSpace(string(rs[:max]))
	}
	return s
}

// isNoSuchValue: the contract answered, but has no such function or value (a
// revert or an empty result) — as opposed to the node being unreachable.
func isNoSuchValue(err error) bool {
	if err == nil {
		return false
	}
	var re *rpcError
	if !errors.As(err, &re) {
		return err.Error() == "empty result"
	}
	// A node that is busy or rate limiting also answers with a JSON-RPC error.
	msg := strings.ToLower(re.Message)
	for _, transient := range []string{"rate", "limit", "too many", "timeout", "timed out", "capacity", "busy", "try again", "unavailable", "overloaded"} {
		if strings.Contains(msg, transient) {
			return false
		}
	}
	return true
}

// readTokenText calls a string getter (name() / symbol()). ok is false when the
// node could not be asked (try again later); a token without the value gives "".
func (c *rpcClient) readTokenText(ctx context.Context, addr, selector string, max int) (text string, ok bool) {
	b, err := c.ethCall(ctx, addr, selector, 0)
	if err != nil {
		return "", isNoSuchValue(err)
	}
	return cleanTokenText(decodeString(b), max), true
}

// tokenLabel returns the token's own name and symbol (cached; two eth_calls for
// a token not seen before). ok is false when the node could not be asked.
func (c *rpcClient) tokenLabel(ctx context.Context, addr string) (tokenLabel, bool) {
	addr = strings.ToLower(addr)
	c.cacheMu.Lock()
	l, hit := c.labels[addr]
	c.cacheMu.Unlock()
	if hit {
		return l, true
	}
	name, ok1 := c.readTokenText(ctx, addr, selName, maxTokenNameLen)
	sym, ok2 := c.readTokenText(ctx, addr, selSymbol, maxTokenSymbolLen)
	if !ok1 || !ok2 {
		return tokenLabel{}, false
	}
	l = tokenLabel{Name: name, Symbol: sym}
	c.cacheMu.Lock()
	c.labels[addr] = l
	c.cacheMu.Unlock()
	return l, true
}

// rpcInflight describes the request currently being waited on.
type rpcInflight struct {
	what    string
	since   time.Time
	attempt int
	lastErr string
	url     string // which node
}

func (f *rpcInflight) String() string {
	s := fmt.Sprintf("%s for %s", f.what, time.Since(f.since).Round(time.Second))
	if f.attempt > 1 {
		s += fmt.Sprintf(" (attempt %d, last error: %s)", f.attempt, f.lastErr)
	}
	return s
}

// waitingOn says what the client is waiting for right now ("" when idle).
func (c *rpcClient) waitingOn() string {
	if c == nil {
		return ""
	}
	f := c.inflight.Load()
	if f == nil {
		return ""
	}
	s := fmt.Sprintf("%s for %s", f.what, time.Since(f.since).Round(time.Second))
	if f.attempt > 1 {
		s += fmt.Sprintf(" (attempt %d, last error: %s)", f.attempt, f.lastErr)
	}
	return s
}

// describeRPC is a short human description of a request, e.g. "eth_getLogs blocks 100-200".
func describeRPC(method string, params []any) string {
	if method == "eth_getLogs" && len(params) == 1 {
		if m, ok := params[0].(map[string]any); ok {
			from, _ := m["fromBlock"].(string)
			to, _ := m["toBlock"].(string)
			if f, err1 := strconv.ParseUint(strings.TrimPrefix(from, "0x"), 16, 64); err1 == nil {
				if t, err2 := strconv.ParseUint(strings.TrimPrefix(to, "0x"), 16, 64); err2 == nil {
					return fmt.Sprintf("eth_getLogs blocks %d-%d (%d blocks)", f, t, t-f+1)
				}
			}
		}
	}
	return method
}

func newRPCClient(oc onchainConfig) *rpcClient {
	if oc.MaxInflight < 1 {
		oc.MaxInflight = 64
	}
	var gap time.Duration // 0 = no rate limit
	if oc.RPS > 0 {
		gap = time.Second / time.Duration(oc.RPS)
	}
	// Many requests run at once: keep enough connections open to the node
	// (Go's default keeps 2 per host and reopens the rest every time).
	tr := &http.Transport{MaxIdleConns: 2 * oc.MaxInflight, MaxIdleConnsPerHost: 2 * oc.MaxInflight, IdleConnTimeout: 90 * time.Second}
	if oc.LogChunk == 0 {
		oc.LogChunk = 200_000
	}
	if oc.MinLogChunk == 0 {
		oc.MinLogChunk = 200
	}
	c := &rpcClient{url: oc.RPCURL, http: &http.Client{Timeout: 60 * time.Second, Transport: tr},
		gap: gap, minChunk: oc.MinLogChunk, slots: make(chan struct{}, oc.MaxInflight),
		times: map[uint64]int64{}, meta: map[string]tokenMeta{}, labels: map[string]tokenLabel{}}
	c.maxChunk = oc.LogChunk
	if c.minChunk > c.maxChunk {
		c.minChunk = c.maxChunk // a configured maximum below the default minimum is respected
	}
	c.parallel = oc.Parallel
	if oc.LogCache > 0 {
		c.logs = newLogCache(oc.LogCache)
	}
	return c
}

func (c *rpcClient) wait(ctx context.Context) error {
	if c.gap <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	now := time.Now()
	start := c.next
	if start.Before(now) {
		start = now
	}
	c.next = start.Add(c.gap)
	c.mu.Unlock()
	return sleepCtx(ctx, time.Until(start))
}

func (c *rpcClient) call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	var lastErr error
	what, since := describeRPC(method, params), time.Now()
	defer c.inflight.Store(nil)
	slot := inflightSlotFrom(ctx) // this call's own "waiting on" marker (several calls share the client)
	if slot != nil {
		defer slot.Store(nil)
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := c.wait(ctx); err != nil {
			return err
		}
		c.requests.Add(1)
		if n := reqCounterFrom(ctx); n != nil {
			n.Add(1)
		}
		f := &rpcInflight{what: what, since: since, attempt: attempt + 1}
		if lastErr != nil {
			f.lastErr = lastErr.Error()
		}
		f.url = c.url
		c.inflight.Store(f)
		if slot != nil {
			slot.Store(f)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		resp, err := c.http.Do(req)
		if err != nil {
			<-c.slots
			lastErr = err
		} else {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBytes)+1))
			resp.Body.Close()
			<-c.slots
			if len(raw) > maxResponseBytes {
				// Cut off: the answer is too big to hold (for eth_getLogs: too many logs in the range).
				return fmt.Errorf("%s: %w", method, errResponseTooLarge)
			}
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("%s: HTTP %s", method, resp.Status)
			} else {
				var r struct {
					Result json.RawMessage `json:"result"`
					Error  *rpcError       `json:"error"`
				}
				if err := json.Unmarshal(raw, &r); err != nil {
					return fmt.Errorf("%s: bad response: %.120s", method, raw)
				}
				if r.Error != nil {
					return r.Error // not retried: the caller decides (e.g. shrink the log range)
				}
				if out == nil {
					return nil
				}
				return json.Unmarshal(r.Result, out)
			}
		}
		if err := sleepCtx(ctx, time.Duration(attempt+1)*rpcRetryBase); err != nil {
			return err
		}
	}
	return lastErr
}

var rpcRetryBase = 2 * time.Second

// maxResponseBytes: the largest JSON-RPC answer read (a var so tests can lower it).
var maxResponseBytes = 64 << 20

var errResponseTooLarge = errors.New("response too large")

func hexU64(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func blockTag(n uint64) string {
	if n == 0 {
		return "latest"
	}
	return hexU64(n)
}

func (c *rpcClient) blockNumber(ctx context.Context) (uint64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_blockNumber"); err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err == nil && n > c.head.Load() {
		c.head.Store(n)
	}
	return n, err
}

// ethCall runs a read-only call at a block (0 = latest). An empty result or a
// revert is returned as an error.
func (c *rpcClient) ethCall(ctx context.Context, to, data string, block uint64) ([]byte, error) {
	var s string
	if err := c.call(ctx, &s, "eth_call", map[string]string{"to": to, "data": data}, blockTag(block)); err != nil {
		return nil, err
	}
	b := unhex(s)
	if len(b) == 0 {
		return nil, errors.New("empty result")
	}
	return b, nil
}

func (c *rpcClient) getLogs(ctx context.Context, address string, topics []any, from, to uint64) ([]rpcLog, error) {
	var logs []rpcLog
	err := c.call(ctx, &logs, "eth_getLogs", map[string]any{
		"address": address, "topics": topics, "fromBlock": hexU64(from), "toBlock": hexU64(to)})
	return logs, err
}

// getLogsChunked walks [from, to] in block ranges and calls fn for each log in
// block order.
//
// Each scan starts with ranges of SCOUT_RPC_LOG_CHUNK blocks (the ceiling) and
// keeps its own size, so one scan's trouble never slows another scan down:
//   - the node refuses a range as too large (too many blocks or results), or the
//     range times out (see retryLogs): that range is asked again in halves, and
//     this scan carries on at the smaller size. Ranges of minChunk blocks are not
//     split any more: one that still fails fails the scan;
//   - after rangeGrowAfter ranges in a row are answered at the smaller size, the
//     size doubles again, up to the ceiling. The grown size is tried on one range
//     first. When the node refuses it or it times out, the next attempt waits
//     twice as long (up to rangeGrowAfterMax ranges, rangeGrowAfterMaxSlow after a
//     time-out), so a fixed range limit, or a stretch of blocks the node reads
//     slowly, is not hit over and over. Once a grown size is answered, or answers
//     come back in a fraction of the time the last time-out took (the scan has
//     reached blocks the node reads quickly), the wait is back to rangeGrowAfter;
//   - rate limits and busy answers are retried at the same size (see retryLogs);
//     they never make the ranges smaller.
func (c *rpcClient) getLogsChunked(ctx context.Context, address string, topics []any, from, to uint64, fn func(rpcLog)) error {
	progress := progressFrom(ctx)
	label := scanLabelFrom(ctx)
	events := 0
	type span struct{ from, to uint64 }
	type result struct {
		logs  []rpcLog
		err   error
		took  time.Duration // the last request for this range
		asked bool          // the node was asked (false: answered from memory or by another scan's request)
	}
	par := c.parallel
	if par < 1 {
		par = 1
	}
	minSize := c.minChunk
	if minSize < 1 {
		minSize = 1
	}
	size := c.maxChunk
	// probing: size was just doubled and not answered yet; untested: size changed
	// and not answered yet (one range at a time until it is); timedOutAfter: how
	// long the last range that timed out in this scan took to do so.
	streak, growAfter, probing, untested := 0, rangeGrowAfter, false, false
	var timedOutAfter time.Duration
	for cur := from; cur <= to; {
		// Plan the next few ranges. They end on multiples of the size, so two
		// scans of the same pool (repeat calls of a token) ask for identical
		// ranges and the second one is answered from memory.
		batchSize, batchProbing := size, probing
		want := par
		if untested {
			want = 1 // one request finds out whether the new size works, not a batch of them
		}
		var spans []span
		for b := cur; b <= to && len(spans) < want; {
			end := b - b%batchSize + batchSize - 1
			if end > to || end < b {
				end = to
			}
			spans = append(spans, span{b, end})
			if end == to {
				break
			}
			b = end + 1
		}
		// Fetch them at the same time; hand the logs to fn in block order.
		bctx, cancel := context.WithCancel(ctx)
		out := make([]chan result, len(spans))
		for i, sp := range spans {
			out[i] = make(chan result, 1)
			go func(ch chan result, sp span) {
				var r result
				// A grown size that times out is not asked again: half of it is known to work.
				r.logs, r.err = c.retryLogs(bctx, !batchProbing, func() ([]rpcLog, error) {
					t0 := time.Now()
					logs, asked, err := c.fetchLogs(bctx, address, topics, sp.from, sp.to, batchSize)
					r.took, r.asked = time.Since(t0), asked
					return logs, err
				})
				ch <- r
			}(out[i], sp)
		}
		done := false
		for i, sp := range spans {
			r := <-out[i]
			if r.err != nil {
				cancel()
				var refused *rangeRefusedError
				n := sp.to - sp.from + 1
				if !errors.As(r.err, &refused) {
					return fmt.Errorf("eth_getLogs %d-%d: %w", sp.from, sp.to, r.err)
				}
				if n <= minSize {
					log.Printf("%seth_getLogs blocks %d-%d (%d blocks) %s at the smallest range size (%d blocks); this scan stops here (%v)",
						labelPrefix(label), sp.from, sp.to, n, refused.reason(), minSize, refused.err)
					return fmt.Errorf("eth_getLogs %d-%d: %w", sp.from, sp.to, r.err)
				}
				// Only this scan goes smaller; the refused range is asked again in halves.
				next := batchSize
				for next >= n && next > minSize { // (a short last range: below its own length)
					next /= 2
				}
				if next < minSize {
					next = minSize
				}
				if refused.timedOut() && (r.asked || timedOutAfter == 0) {
					timedOutAfter = r.took
				}
				if probing {
					// The grown size failed too: wait longer before the next try.
					limit := rangeGrowAfterMax
					if refused.timedOut() {
						limit = rangeGrowAfterMaxSlow // each failed try costs a whole time-out
					}
					growAfter = max(min(growAfter*2, limit), growAfter)
				}
				size, streak, probing, untested = next, 0, false, true
				c.splits.Add(1)
				c.notes.say(true, "%seth_getLogs blocks %d-%d (%d blocks) %s (%v); this scan continues with %d-block ranges (max %d)",
					labelPrefix(label), sp.from, sp.to, n, refused.reason(), refused.err, size, c.maxChunk)
				cur = sp.from
				break
			}
			for _, l := range r.logs {
				fn(l)
			}
			events += len(r.logs)
			if batchSize == size {
				// Only a full-size range shows the size works (after a change the
				// first range is often a shorter piece up to a multiple of the size).
				if sp.to-sp.from+1 == size {
					untested = false
					if probing { // the grown size works: grow again at the normal pace
						probing, growAfter = false, rangeGrowAfter
					}
				}
				if timedOutAfter > 0 && r.asked && r.took*4 < timedOutAfter {
					// Answered in a fraction of the time the last time-out took:
					// these blocks are cheap for the node (e.g. past an unindexed stretch).
					growAfter = rangeGrowAfter
				}
				if streak++; size < c.maxChunk && streak >= growAfter {
					size, streak, probing, untested = min(size*2, c.maxChunk), 0, true, true
					c.notes.say(false, "%seth_getLogs ranges back up to %d blocks (max %d) after %d answered ranges",
						labelPrefix(label), size, c.maxChunk, growAfter)
				}
			}
			if progress != nil {
				progress(from, sp.to, to, events, size)
			}
			if sp.to == to {
				done = true
				break
			}
			cur = sp.to + 1
		}
		cancel()
		if done {
			break
		}
	}
	return nil
}

// rangeGrowAfter: answered ranges in a row after which a scan that had to go
// smaller doubles its range size again. Each failure of a grown size doubles
// this wait, up to rangeGrowAfterMax when it was refused as too large (a quick
// answer) or rangeGrowAfterMaxSlow when it timed out (each try costs the node's
// whole time limit, ~30 s on Nitro).
const (
	rangeGrowAfter        = 3
	rangeGrowAfterMax     = 48
	rangeGrowAfterMaxSlow = 192
)

// logsRetries: how often a busy or rate limiting node is asked for the same
// range (at the same size) before the scan gives up for this run.
const logsRetries = 4

// rangeRefusedError: the node will not answer this range as asked; split it.
type rangeRefusedError struct {
	err   error
	why   refusal
	tries int // requests for this range at this size (call()'s own retries not counted)
}

type refusal int

const (
	refusedTooLarge      refusal = iota // the node said the range or the result is too large
	refusedUnexplained                  // an unexplained error that came back on a retry
	refusedNodeTimeout                  // the node answered "request timed out" (or similar)
	refusedClientTimeout                // no answer within the client's time limit, call()'s retries used up
)

func (e *rangeRefusedError) Error() string { return e.err.Error() }
func (e *rangeRefusedError) Unwrap() error { return e.err }

func (e *rangeRefusedError) timedOut() bool {
	return e.why == refusedNodeTimeout || e.why == refusedClientTimeout
}

// reason: what happened, for log lines ("… blocks A-B (N blocks) <reason> (error) …").
func (e *rangeRefusedError) reason() string {
	switch e.why {
	case refusedUnexplained:
		return "failed twice with an error the node does not explain, treated as too large"
	case refusedNodeTimeout:
		if e.tries > 1 {
			return fmt.Sprintf("timed out on the node (%d tries)", e.tries)
		}
		return "timed out on the node"
	case refusedClientTimeout:
		return "got no answer in time (client time-out)"
	}
	return "refused as too large"
}

type logsErrKind int

const (
	logsOther         logsErrKind = iota // transport error (already retried in call) or cancelled
	logsTooLarge                         // range / result too large: split the range
	logsBusy                             // rate limit, busy node: same range again later
	logsTimedOut                         // the node gave up on the request: once more, then split
	logsClientTimeout                    // no answer within the client's time limit (call() retried it): split
	logsUnexplained                      // a JSON-RPC error that says neither
)

// classifyLogsErr sorts an eth_getLogs error. Rate limits and busy answers are
// load, whatever else they mention. A time-out is either load or a range the
// node cannot read within its time limit (Nitro / geth walk blocks outside the
// log index one by one, about 1 ms per block); retryLogs tells them apart by
// asking once more. Only a refusal that names the range or the size of the
// result counts as "too large".
func classifyLogsErr(err error) logsErrKind {
	if errors.Is(err, errResponseTooLarge) {
		return logsTooLarge
	}
	var re *rpcError
	if !errors.As(err, &re) {
		if isTimeout(err) {
			return logsClientTimeout
		}
		return logsOther
	}
	msg := strings.ToLower(re.Message)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(msg, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("rate limit", "ratelimit", "rate-limit", "request rate", "rate exceeded", "too many requests",
		"request limit", "requests per", "quota", "credits", "throttl", "busy", "capacity", "overloaded"):
		return logsBusy
	case has("timed out", "timeout", "time out", "deadline"):
		// "request timed out" (Nitro), "query timeout exceeded", "context deadline exceeded" (geth)
		return logsTimedOut
	case has("try again", "temporar", "unavailable", "not found", "syncing"):
		return logsBusy
	case has("range", "too large", "too big", "too wide", "more than", "too many", "exceed",
		"response size", "result size", "max results", "results limit", "result limit", "log limit"):
		return logsTooLarge
	}
	return logsUnexplained
}

// retryLogs runs one eth_getLogs request (fetch) and deals with its errors:
//   - "range / result too large": returned as *rangeRefusedError, the caller splits the range;
//   - busy or rate limiting (as a JSON-RPC error): asked again at the same size
//     after a pause, up to logsRetries times, then returned as it is: never split;
//   - the node timed out ("request timed out"): asked once more at the same size
//     when retryTimeout (it may have been a passing hiccup), then returned as
//     *rangeRefusedError: the range is too slow for the node, the caller splits it;
//   - no answer within the client's time limit, after call()'s own retries:
//     *rangeRefusedError as well, as a last resort;
//   - an error the node does not explain: asked once more; if it comes back, it
//     is treated as a refusal (nodes word their range limits in many ways);
//   - other transport errors (resets, HTTP 429 / 5xx) were already retried with
//     pauses by call(), at the same size, and are returned as they are.
func (c *rpcClient) retryLogs(ctx context.Context, retryTimeout bool, fetch func() ([]rpcLog, error)) ([]rpcLog, error) {
	unexplained, timedOut := false, false
	for attempt := 1; ; attempt++ {
		logs, err := fetch()
		if err == nil || ctx.Err() != nil {
			return logs, err
		}
		switch classifyLogsErr(err) {
		case logsTooLarge:
			return nil, &rangeRefusedError{err: err, why: refusedTooLarge, tries: attempt}
		case logsBusy:
			if attempt >= logsRetries {
				return nil, err
			}
		case logsTimedOut:
			if timedOut || !retryTimeout || attempt >= logsRetries {
				return nil, &rangeRefusedError{err: err, why: refusedNodeTimeout, tries: attempt}
			}
			timedOut = true
		case logsClientTimeout:
			return nil, &rangeRefusedError{err: err, why: refusedClientTimeout, tries: attempt}
		case logsUnexplained:
			if unexplained {
				return nil, &rangeRefusedError{err: err, why: refusedUnexplained, tries: attempt}
			}
			unexplained = true
		default:
			return nil, err
		}
		if err := sleepCtx(ctx, time.Duration(attempt)*rpcRetryBase); err != nil {
			return nil, err
		}
	}
}

// rangeNoteEvery: at most one log line per this interval about range sizes
// (several workers scan at once); the lines in between are counted.
var rangeNoteEvery = 30 * time.Second

type rangeNotes struct {
	mu            sync.Mutex
	last          time.Time
	split, growth int // changes not shown since the last line
}

func (n *rangeNotes) say(split bool, format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.last.IsZero() && time.Since(n.last) < rangeNoteEvery {
		if split {
			n.split++
		} else {
			n.growth++
		}
		return
	}
	line := fmt.Sprintf(format, args...)
	if n.split+n.growth > 0 {
		line += fmt.Sprintf(" [also %d range split(s) and %d grow-back(s) in all scans since the last such line]", n.split, n.growth)
	}
	n.last, n.split, n.growth = time.Now(), 0, 0
	log.Print(line)
}

func labelPrefix(label string) string {
	if label == "" {
		return ""
	}
	return label + ": "
}

// isTimeout: the request ran into the client's time limit (not a cancelled run).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// fetchLogs returns one chunk's logs in block order, from memory when this
// exact full chunk was fetched before. asked: this call sent the request to the
// node itself (false: from memory, or shared with another scan's request).
func (c *rpcClient) fetchLogs(ctx context.Context, address string, topics []any, from, to, size uint64) (logs []rpcLog, asked bool, err error) {
	key := ""
	if c.logs != nil && from%size == 0 && to-from+1 == size {
		key = logCacheKey(address, topics, from, to)
		if logs, ok := c.logs.get(key); ok {
			return logs, false, nil
		}
	}
	// Several workers often want the very same range at the same moment (repeat
	// calls of one token sit next to each other in the queue): ask the node once.
	fkey := logCacheKey(address, topics, from, to)
	for {
		c.sfMu.Lock()
		if c.sf == nil {
			c.sf = map[string]*logFlight{}
		}
		fl, waiting := c.sf[fkey]
		if !waiting {
			fl = &logFlight{done: make(chan struct{})}
			c.sf[fkey] = fl
		}
		c.sfMu.Unlock()
		if waiting {
			select {
			case <-fl.done:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
			if fl.err != nil && isCanceled(fl.err) && ctx.Err() == nil {
				continue // the asker was interrupted, we were not: ask ourselves
			}
			return fl.logs, false, fl.err
		}
		logs, err = c.getLogs(ctx, address, topics, from, to)
		if err == nil {
			sort.SliceStable(logs, func(i, j int) bool {
				if logs[i].block() != logs[j].block() {
					return logs[i].block() < logs[j].block()
				}
				return logs[i].index() < logs[j].index()
			})
			// Only settled history is kept: a chunk that reaches the chain's tip may still grow.
			if key != "" && to+1000 < c.head.Load() {
				c.logs.put(key, logs)
			}
		}
		fl.logs, fl.err = logs, err
		c.sfMu.Lock()
		delete(c.sf, fkey)
		c.sfMu.Unlock()
		close(fl.done)
		return logs, true, err
	}
}

// logFlight is one eth_getLogs request that other scans are waiting on too.
type logFlight struct {
	done chan struct{}
	logs []rpcLog
	err  error
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// Progress reporting for long log scans (a 30-day window is ~26M blocks).
type progressKey struct{}

// progressFunc: a scan of [from, to] has read through block done, found events
// so far, and asks for ranges of size blocks right now.
type progressFunc func(from, done, to uint64, events int, size uint64)

func progressFrom(ctx context.Context) progressFunc {
	p, _ := ctx.Value(progressKey{}).(progressFunc)
	return p
}

type scanLabelKey struct{}

// scanLabelFrom: who is scanning (e.g. "call 9163 [37/50]"), for log lines; "" if unknown.
func scanLabelFrom(ctx context.Context) string {
	s, _ := ctx.Value(scanLabelKey{}).(string)
	return s
}

// withScanProgress returns a context under which log scans print a progress
// line at most every `every` (and never for scans that finish sooner).
func withScanProgress(ctx context.Context, label string, every time.Duration) context.Context {
	last := time.Now()
	ctx = context.WithValue(ctx, scanLabelKey{}, label)
	return context.WithValue(ctx, progressKey{}, progressFunc(func(from, done, to uint64, events int, size uint64) {
		if time.Since(last) < every || done >= to {
			return
		}
		last = time.Now()
		pct := 100.0
		if to > from {
			pct = float64(done-from) / float64(to-from) * 100
		}
		log.Printf("%s: scanning blocks %d → %d: %.0f%% (at %d, %d events so far, %d-block ranges)", label, from, to, pct, done, events, size)
	}))
}

// blockTime returns a block's timestamp (cached).
func (c *rpcClient) blockTime(ctx context.Context, n uint64) (int64, error) {
	c.cacheMu.Lock()
	if t, ok := c.times[n]; ok {
		c.cacheMu.Unlock()
		return t, nil
	}
	c.cacheMu.Unlock()
	var blk *struct {
		Timestamp string `json:"timestamp"`
	}
	if err := c.call(ctx, &blk, "eth_getBlockByNumber", hexU64(n), false); err != nil {
		return 0, err
	}
	if blk == nil {
		return 0, fmt.Errorf("block %d not found", n)
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(blk.Timestamp, "0x"), 16, 64)
	if err != nil {
		return 0, err
	}
	c.cacheMu.Lock()
	if _, ok := c.times[n]; !ok {
		c.times[n] = ts
		i := sort.Search(len(c.anchors), func(i int) bool { return c.anchors[i] >= n })
		c.anchors = append(c.anchors, 0)
		copy(c.anchors[i+1:], c.anchors[i:])
		c.anchors[i] = n
	}
	c.cacheMu.Unlock()
	return ts, nil
}

// blockAt returns a block whose timestamp is at (or within ~1s before) ts:
// interpolation search between known (block, time) anchors. Block times on
// this chain are irregular, so the bracket is always kept.
func (c *rpcClient) blockAt(ctx context.Context, ts int64) (uint64, error) {
	latest, err := c.blockNumber(ctx)
	if err != nil {
		return 0, err
	}
	tLatest, err := c.blockTime(ctx, latest)
	if err != nil {
		return 0, err
	}
	if ts >= tLatest {
		return latest, nil
	}
	lo, hi := uint64(1), latest
	tLo, err := c.blockTime(ctx, lo)
	if err != nil {
		return 0, err
	}
	if ts <= tLo {
		return lo, nil
	}
	tHi := tLatest
	// tighten the bracket with cached anchors
	c.cacheMu.Lock()
	for _, a := range c.anchors {
		t := c.times[a]
		if t <= ts && a > lo {
			lo, tLo = a, t
		}
		if t > ts && a < hi {
			hi, tHi = a, t
		}
	}
	c.cacheMu.Unlock()
	for i := 0; i < 60 && hi-lo > 1 && ts-tLo > 1; i++ {
		// interpolate, but never land on an end of the bracket
		frac := float64(ts-tLo) / float64(tHi-tLo)
		mid := lo + uint64(frac*float64(hi-lo))
		if i%3 == 2 { // guard against slow convergence: bisect every third step
			mid = lo + (hi-lo)/2
		}
		if mid <= lo {
			mid = lo + 1
		}
		if mid >= hi {
			mid = hi - 1
		}
		t, err := c.blockTime(ctx, mid)
		if err != nil {
			return 0, err
		}
		if t <= ts {
			lo, tLo = mid, t
		} else {
			hi, tHi = mid, t
		}
	}
	return lo, nil
}

func decodeString(b []byte) string {
	if len(b) >= 64 { // dynamic string: offset, length, data
		off := int(word(b, 0).Uint64())
		if off+32 <= len(b) {
			n := int(new(big.Int).SetBytes(b[off : off+32]).Uint64())
			if off+32+n <= len(b) {
				return string(b[off+32 : off+32+n])
			}
		}
	}
	return strings.TrimRight(string(b), "\x00") // bytes32 symbol
}

// tokenInfo returns decimals, symbol and name (cached). Native ETH is the zero address.
func (c *rpcClient) tokenInfo(ctx context.Context, addr string) (tokenMeta, error) {
	addr = strings.ToLower(addr)
	if addr == zeroAddr {
		return tokenMeta{Decimals: 18, Symbol: "ETH"}, nil
	}
	c.cacheMu.Lock()
	if m, ok := c.meta[addr]; ok {
		c.cacheMu.Unlock()
		return m, nil
	}
	c.cacheMu.Unlock()
	b, err := c.ethCall(ctx, addr, selDecimals, 0)
	if err != nil {
		return tokenMeta{}, fmt.Errorf("decimals(%s): %w", addr, err)
	}
	m := tokenMeta{Decimals: int(word(b, 0).Uint64()), Symbol: addr[:8]}
	rawSym, symOK := "", false
	if sb, err := c.ethCall(ctx, addr, selSymbol, 0); err == nil {
		rawSym, symOK = decodeString(sb), true
		if s := strings.TrimSpace(rawSym); s != "" && len(s) <= 32 {
			m.Symbol = s
		}
	} else {
		symOK = isNoSuchValue(err)
	}
	// name() is optional: a token without one (or a node error) never fails this.
	name, nameOK := c.readTokenText(ctx, addr, selName, maxTokenNameLen)
	m.Name = name
	c.cacheMu.Lock()
	c.meta[addr] = m
	if symOK && nameOK { // both really answered: the fill pass need not ask again
		c.labels[addr] = tokenLabel{Name: name, Symbol: cleanTokenText(rawSym, maxTokenSymbolLen)}
	}
	c.cacheMu.Unlock()
	return m, nil
}

// ---------------------------------------------------------------------------
// Pool discovery and pricing
// ---------------------------------------------------------------------------

// onchainState is persisted per call (scout_call_tracking.onchain).
type onchainState struct {
	Kind       string `json:"kind"`              // v2 | v3 | v4
	Pool       string `json:"pool"`              // pair/pool address, or the v4 PoolManager
	PoolID     string `json:"pool_id,omitempty"` // v4 pool id
	TokenIs0   bool   `json:"token_is_0"`
	TokenDec   int    `json:"token_dec"`
	Quote      string `json:"quote"` // the pool's other asset (zero address = native ETH)
	QuoteSym   string `json:"quote_sym"`
	QuoteDec   int    `json:"quote_dec"`
	EntryBlock uint64 `json:"entry_block"`

	EntryPriceQ    float64         `json:"entry_price_q"` // entry price in quote units
	ScanBlock      uint64          `json:"scan_block"`    // swaps scanned through this block
	LastPriceQ     float64         `json:"last_price_q"`
	LastPriceBlock uint64          `json:"last_price_block"`
	RunMaxQ        float64         `json:"run_max_q"` // extremes since entry, in quote units
	RunMinQ        float64         `json:"run_min_q"`
	Done           map[string]bool `json:"done,omitempty"` // horizons already computed

	// v2: realistic entry (pool price EntryDelay after the post), extremes after
	// it, the quote asset's USD price at entry, and whether the pre-call stats
	// were stored.
	V             int     `json:"v"`
	LateBlock     uint64  `json:"late_block,omitempty"`
	LatePriceQ    float64 `json:"late_price_q,omitempty"`
	RunMaxLateQ   float64 `json:"run_max_late_q,omitempty"`
	RunMinLateQ   float64 `json:"run_min_late_q,omitempty"`
	EntryQuoteUSD float64 `json:"entry_quote_usd,omitempty"` // 0 = prices are in quote units
	// Extremes since the call / since the late entry in the price unit (USD when
	// available), each event converted at its own hour's rate. 0 = no trades yet.
	RunMaxU     float64 `json:"run_max_u,omitempty"`
	RunMinU     float64 `json:"run_min_u,omitempty"`
	RunMaxLateU float64 `json:"run_max_late_u,omitempty"`
	RunMinLateU float64 `json:"run_min_late_u,omitempty"`
	PreDone     bool    `json:"pre_done,omitempty"`

	// Latest-price pass (tracker_latest.go): its own cursor, apart from the
	// horizon scan above. Optional additions; they do not change the version.
	LatestBlock      uint64  `json:"latest_block,omitempty"`       // price events read through this block
	LatestPriceQ     float64 `json:"latest_price_q,omitempty"`     // pool price at LatestBlock, in quote units
	LatestTradeBlock uint64  `json:"latest_trade_block,omitempty"` // block of the trade that price comes from

	// Rug: the first block whose price event showed the pool's quote side below
	// SCOUT_RUG_LIQ_USD (or certainly empty). From that block on no price counts,
	// every horizon ending at or after it is -100%, and so is the latest return.
	// Once rugged, a call stays rugged. Optional additions; no version change.
	RugBlock        uint64   `json:"rug_block,omitempty"`
	RugLiquidityUSD *float64 `json:"rug_liq_usd,omitempty"` // the pool's quote side in USD at the rug (nil = unknown)
	// EntryLiqQ: the liquidity shown by the entry price's event, in quote units
	// (0 = not measured); checked against the threshold once the quote's USD
	// price at entry is known.
	EntryLiqQ float64 `json:"entry_liq_q,omitempty"`

	warnedJump bool // the 1e6× backstop was logged for this call in this run (not stored)
}

// onchainStateVersion: calls tracked with an older state are tracked again from
// scratch, so every call has the late entry, candles and pre-call stats.
const onchainStateVersion = 2

type onchainSource struct {
	cfg onchainConfig
	rpc *rpcClient

	// noState is set once the node refuses a historical eth_call (a full, non-archive
	// node): historical values are then read from event logs instead.
	noState atomic.Bool

	mu          sync.Mutex
	aggregators map[string][]string // feed proxy → aggregator contracts that emit AnswerUpdated

	mainnet *rpcClient // Ethereum archive node (nil unless SCOUT_MAINNET_RPC_URL is set)

	hourBlocks map[int64]uint64         // UTC hour → block at that time
	hourQuotes map[string]float64       // "quote|hour" → USD price of the quote asset
	quotePools map[string]*onchainState // quote asset → its own WETH/stable pool (nil = none)
}

func newOnchainSource(oc onchainConfig) *onchainSource {
	if oc.PriceLookback == 0 {
		oc.PriceLookback = 8_640_000
	}
	o := &onchainSource{cfg: oc, rpc: newRPCClient(oc), hourBlocks: map[int64]uint64{},
		hourQuotes: map[string]float64{}, quotePools: map[string]*onchainState{}}
	if oc.MainnetRPCURL != "" {
		o.mainnet = newRPCClient(onchainConfig{RPCURL: oc.MainnetRPCURL, RPS: oc.MainnetRPS})
	}
	return o
}

var (
	errNoPool   = errors.New("no pool found")
	errNoTrades = errors.New("no trades around the call time")
)

// discover finds the pool the token traded in around entryBlock.
func (o *onchainSource) discover(ctx context.Context, token string, entryBlock uint64) (*onchainState, error) {
	return o.discoverWith(ctx, token, entryBlock, nil)
}

// discoverWith is discover restricted to pools whose other asset passes accept
// (nil = any). Used to find a quote asset's own WETH / stablecoin pool.
func (o *onchainSource) discoverWith(ctx context.Context, token string, entryBlock uint64, accept func(quote string) bool) (*onchainState, error) {
	token = strings.ToLower(token)
	tm, err := o.rpc.tokenInfo(ctx, token)
	if err != nil {
		return nil, err
	}
	latest, err := o.rpc.blockNumber(ctx)
	if err != nil {
		return nil, err
	}
	// Widen the window until the token shows transfers (thinly traded tokens).
	type cand struct {
		addr string
		n    int
		txs  map[string]bool
	}
	var cands []*cand
	var from, to uint64
	for _, mult := range []uint64{1, 4, 16, 64} {
		span := o.cfg.DiscoveryBlocks * mult
		from = 1
		if entryBlock > span {
			from = entryBlock - span
		}
		to = entryBlock + span
		if to > latest {
			to = latest
		}
		byAddr := map[string]*cand{}
		err := o.rpc.getLogsChunked(ctx, token, []any{topicTransfer}, from, to, func(l rpcLog) {
			if len(l.Topics) < 3 {
				return
			}
			for _, t := range l.Topics[1:3] {
				a := addrFromWord(unhex(t))
				if a == zeroAddr || a == token {
					continue
				}
				c := byAddr[a]
				if c == nil {
					c = &cand{addr: a, txs: map[string]bool{}}
					byAddr[a] = c
				}
				c.n++
				c.txs[l.TxHash] = true
			}
		})
		if err != nil {
			return nil, err
		}
		cands = cands[:0]
		for _, c := range byAddr {
			cands = append(cands, c)
		}
		if len(cands) > 0 {
			break
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].n != cands[j].n {
			return cands[i].n > cands[j].n
		}
		return cands[i].addr < cands[j].addr
	})
	limit := 8
	if accept != nil {
		limit = 32 // the wanted pool is rarely the busiest counterparty
	}
	if len(cands) > limit {
		cands = cands[:limit]
	}
	for _, c := range cands {
		st := &onchainState{TokenDec: tm.Decimals, EntryBlock: entryBlock}
		var ok bool
		if c.addr == o.cfg.PoolManagerV4 {
			ok, err = o.resolveV4(ctx, st, token, c.txs, from, to, accept)
		} else {
			ok, err = o.resolveV2V3(ctx, st, token, c.addr)
			if ok && accept != nil && !accept(st.Quote) {
				ok = false
			}
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		qm, err := o.rpc.tokenInfo(ctx, st.Quote)
		if err != nil {
			return nil, err
		}
		st.QuoteDec, st.QuoteSym = qm.Decimals, qm.Symbol
		return st, nil
	}
	return nil, errNoPool
}

// resolveV2V3 checks whether addr is a v2 pair or v3 pool containing token.
func (o *onchainSource) resolveV2V3(ctx context.Context, st *onchainState, token, addr string) (bool, error) {
	b0, err := o.rpc.ethCall(ctx, addr, selToken0, 0)
	if err != nil {
		return false, nonRPC(err) // wallets and routers have no token0()
	}
	b1, err := o.rpc.ethCall(ctx, addr, selToken1, 0)
	if err != nil {
		return false, nonRPC(err)
	}
	t0, t1 := addrFromWord(b0), addrFromWord(b1)
	switch token {
	case t0:
		st.TokenIs0, st.Quote = true, t1
	case t1:
		st.TokenIs0, st.Quote = false, t0
	default:
		return false, nil
	}
	st.Pool = addr
	if r, err := o.rpc.ethCall(ctx, addr, selGetReserves, 0); err == nil && len(r) >= 64 {
		st.Kind = "v2"
		return true, nil
	}
	if r, err := o.rpc.ethCall(ctx, addr, selSlot0, 0); err == nil && len(r) >= 32 {
		st.Kind = "v3"
		return true, nil
	}
	return false, nil
}

// nonRPC keeps transport errors and drops contract-level ones (revert / no code).
func nonRPC(err error) error {
	var re *rpcError
	if errors.As(err, &re) || err.Error() == "empty result" {
		return nil
	}
	return err
}

// resolveV4 finds the v4 pool id the token traded in (the pool whose Swap
// events share transactions with the token's transfers) and its currencies.
func (o *onchainSource) resolveV4(ctx context.Context, st *onchainState, token string, txs map[string]bool, from, to uint64, accept func(string) bool) (bool, error) {
	counts := map[string]int{}
	err := o.rpc.getLogsChunked(ctx, o.cfg.PoolManagerV4, []any{topicSwapV4}, from, to, func(l rpcLog) {
		if len(l.Topics) >= 2 && txs[l.TxHash] {
			counts[l.Topics[1]]++
		}
	})
	if err != nil {
		return false, err
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		// Initialize(id, currency0, currency1, …): walk back from the window to find it.
		var c0, c1 string
		hi := to
		span := o.rpc.maxChunk // this search's own range size: a refusal here changes no other scan
		for step := 0; step < 400 && c0 == "" && hi > 0; step++ {
			lo := uint64(1)
			if hi > span {
				lo = hi - span + 1
			}
			logs, err := o.rpc.retryLogs(ctx, true, func() ([]rpcLog, error) {
				return o.rpc.getLogs(ctx, o.cfg.PoolManagerV4, []any{topicInitV4, id}, lo, hi)
			})
			if err != nil {
				var refused *rangeRefusedError
				if errors.As(err, &refused) && span > o.rpc.minChunk {
					span = max(span/2, o.rpc.minChunk)
					o.rpc.splits.Add(1)
					continue
				}
				return false, err
			}
			if len(logs) > 0 && len(logs[0].Topics) >= 4 {
				c0, c1 = addrFromWord(unhex(logs[0].Topics[2])), addrFromWord(unhex(logs[0].Topics[3]))
			}
			if lo == 1 {
				break
			}
			hi = lo - 1
		}
		switch token {
		case c0:
			st.TokenIs0, st.Quote = true, c1
		case c1:
			st.TokenIs0, st.Quote = false, c0
		default:
			continue // a multi-hop leg through another pool
		}
		if accept != nil && !accept(st.Quote) {
			continue
		}
		st.Kind, st.Pool, st.PoolID = "v4", o.cfg.PoolManagerV4, id
		return true, nil
	}
	return false, nil
}

// Uniswap v3/v4 ticks run from -maxTick to +maxTick. A swap that ends within
// tickBoundSlack ticks of either end ran out of in-range liquidity: its
// sqrtPriceX96 is the bound (about 2^±128 quote per token), not a price.
const (
	maxTick        = 887272
	tickBoundSlack = 100
)

// maxPriceJump: in the horizon scan and the latest pass, a price more than
// this many times the entry price is taken as invalid and skipped (backstop).
const maxPriceJump = 1e6

// poolEvent is what one price event (v2 Sync, v3/v4 Swap) says about the pool.
type poolEvent struct {
	price    float64 // the token's price in quote units after the event (0 = n/a or not usable)
	liqQ     float64 // the pool's quote side after the event, in quote units
	liqKnown bool    // liqQ was measured
	drained  bool    // the pool is certainly empty (no in-range liquidity, tick at the bound, zero v2 reserve)
}

// eventOfLog decodes a price event. Liquidity is ONE side of the pool, the
// quote side, in quote units (like liquidityUSD):
//   - v2: the quote reserve in the Sync event.
//   - v3/v4: the quote side of the in-range virtual reserves, from the
//     Swap's liquidity L and sqrtPriceX96 (raw sqrtP = sqrtPriceX96 / 2^96):
//     L·sqrtP when the quote is token1, L / sqrtP when it is token0, divided by
//     10^quote decimals. An approximation of the in-range liquidity only: it
//     ignores liquidity outside the current tick range and overstates what a
//     narrow range can actually pay out.
//
// A drained event has no usable price (price 0).
func (st *onchainState) eventOfLog(l rpcLog) poolEvent {
	data := unhex(l.Data)
	var ev poolEvent
	var p0in1 float64
	d0, d1 := st.TokenDec, st.QuoteDec
	if !st.TokenIs0 {
		d0, d1 = st.QuoteDec, st.TokenDec
	}
	switch st.Kind {
	case "v2":
		r0, r1 := word(data, 0), word(data, 1)
		tok, quo := r0, r1
		if !st.TokenIs0 {
			tok, quo = r1, r0
		}
		ev.drained = tok.Sign() == 0 || quo.Sign() == 0
		ev.liqQ, ev.liqKnown = bigToFloat(quo)/pow10(st.QuoteDec), true
		p0in1 = priceFromReserves(r0, r1, d0, d1)
	default: // v3 and v4: sqrtPriceX96, liquidity and tick are data words 2, 3 and 4
		sqrtP := word(data, 2)
		p0in1 = priceFromSqrtX96(sqrtP, d0, d1)
		if len(data) >= 5*32 {
			liq, tick := word(data, 3), signedWord(data, 4)
			ev.drained = liq.Sign() == 0 || sqrtP.Sign() == 0 ||
				new(big.Int).Abs(tick).Cmp(big.NewInt(maxTick-tickBoundSlack)) >= 0
			ev.liqKnown = true
			if liq.Sign() > 0 && sqrtP.Sign() > 0 {
				sp := new(big.Float).SetPrec(256).SetInt(sqrtP)
				sp.Quo(sp, new(big.Float).SetPrec(256).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))) // raw sqrt(token1 per token0)
				amt := new(big.Float).SetPrec(256).SetInt(liq)
				if st.TokenIs0 {
					amt.Mul(amt, sp) // quote = token1: L·sqrtP
				} else {
					amt.Quo(amt, sp) // quote = token0: L / sqrtP
				}
				// The accuracy flag is not needed: a rounded figure is fine for
				// a threshold check, and overflow gives +Inf (not a rug).
				f, _ := amt.Float64()
				ev.liqQ = f / pow10(st.QuoteDec)
			}
		}
	}
	if ev.drained || p0in1 <= 0 || math.IsInf(p0in1, 0) || math.IsNaN(p0in1) {
		return ev
	}
	ev.price = p0in1
	if !st.TokenIs0 {
		ev.price = 1 / p0in1
	}
	return ev
}

// rug tells whether the event shows the pool's quote side under minUSD, and
// the quote side in USD when it is known. qUSD is the quote asset's USD price;
// 0 = unknown, and then only the certain signals count (never a guess).
func (ev poolEvent) rug(qUSD, minUSD float64) (bool, *float64) {
	var liq *float64
	switch {
	case ev.liqKnown && ev.liqQ == 0:
		zero := 0.0
		liq = &zero
	case ev.liqKnown && qUSD > 0:
		v := ev.liqQ * qUSD
		liq = &v
	}
	if ev.drained {
		return true, liq
	}
	return liq != nil && minUSD > 0 && *liq < minUSD, liq
}

// priceOfLog returns the token's price in quote units after this event
// (0 = n/a, including the bound price of a drained pool).
func (st *onchainState) priceOfLog(l rpcLog) float64 {
	return st.eventOfLog(l).price
}

// implausible: a price more than maxPriceJump times the entry price (the
// backstop of the scan and the latest pass; logged once per call and run).
func (st *onchainState) implausible(p float64, block uint64) bool {
	if st.EntryPriceQ <= 0 || p <= st.EntryPriceQ*maxPriceJump {
		return false
	}
	if !st.warnedJump {
		st.warnedJump = true
		log.Printf("uniswap-%s pool %s: price %.6g %s at block %d is more than %.0e× the entry price %.6g — skipped as invalid",
			st.Kind, st.poolRef(), p, st.QuoteSym, block, maxPriceJump, st.EntryPriceQ)
	}
	return true
}

// poolRef names the pool: the v4 pool id, or the pool address.
func (st *onchainState) poolRef() string {
	if st.Kind == "v4" {
		return st.PoolID
	}
	return st.Pool
}

func (st *onchainState) logFilter() (string, []any) {
	switch st.Kind {
	case "v2":
		return st.Pool, []any{topicSyncV2}
	case "v4":
		return st.Pool, []any{topicSwapV4, st.PoolID}
	}
	return st.Pool, []any{topicSwapV3}
}

// scan folds price events in (from, to] into the running state.
// obs (optional) sees every price event, in order. The first event that shows
// the pool under the rug threshold sets RugBlock; neither it nor anything after
// it is folded in or shown to obs. Prices above maxPriceJump × entry are skipped.
func (o *onchainSource) scan(ctx context.Context, st *onchainState, to uint64, obs func(block uint64, p float64)) error {
	if to <= st.ScanBlock {
		return nil
	}
	addr, topics := st.logFilter()
	err := o.rpc.getLogsChunked(ctx, addr, topics, st.ScanBlock+1, to, func(l rpcLog) {
		if st.RugBlock > 0 && l.block() >= st.RugBlock {
			return // the pool was drained: nothing from the rug on counts
		}
		ev := st.eventOfLog(l)
		// Liquidity is valued at the quote's USD price at entry (no node
		// request); without one, only the certain signals count.
		if rug, liq := ev.rug(st.EntryQuoteUSD, o.cfg.RugLiqUSD); rug {
			st.RugBlock, st.RugLiquidityUSD = l.block(), liq
			return
		}
		p := ev.price
		if p <= 0 || st.implausible(p, l.block()) {
			return
		}
		st.LastPriceQ, st.LastPriceBlock = p, l.block()
		if l.block() <= st.LateBlock {
			st.LatePriceQ = p // the pool price EntryDelay after the post
		} else {
			if p > st.RunMaxLateQ {
				st.RunMaxLateQ = p
			}
			if st.RunMinLateQ == 0 || p < st.RunMinLateQ {
				st.RunMinLateQ = p
			}
		}
		if obs != nil {
			obs(l.block(), p)
		}
		if p > st.RunMaxQ {
			st.RunMaxQ = p
		}
		if st.RunMinQ == 0 || p < st.RunMinQ {
			st.RunMinQ = p
		}
	})
	if err != nil {
		return err
	}
	st.ScanBlock = to
	return nil
}

// entryPrice: the last trade price at or before the call; if the token had not
// traded yet, the first trade after it. Events of a drained pool give no price.
// When the pool traded before the call and the last event at or before the
// call shows it drained, the call is rugged from the start: RugBlock is that
// event's block, and the entry is the last usable price before it. The entry
// event's liquidity is kept in EntryLiqQ for the USD check once the quote's
// USD price at entry is known.
func (o *onchainSource) entryPrice(ctx context.Context, st *onchainState, latest uint64) error {
	addr, topics := st.logFilter()
	setEntry := func(p float64, block uint64, ev poolEvent, scanTo uint64) {
		st.EntryPriceQ, st.LastPriceQ, st.LastPriceBlock = p, p, block
		st.RunMaxQ, st.RunMinQ, st.ScanBlock = p, p, scanTo
		st.LatePriceQ, st.RunMaxLateQ, st.RunMinLateQ = p, 0, 0
		st.EntryLiqQ = 0
		if ev.liqKnown {
			st.EntryLiqQ = ev.liqQ
		}
	}
	rugAt := func(block uint64) {
		zero := 0.0
		st.RugBlock, st.RugLiquidityUSD = block, &zero
	}
	var drainedAt uint64                      // the last event so far showed a drained pool (0 = no)
	for _, mult := range []uint64{1, 8, 64} { // look back further for quiet pools
		span := o.cfg.DiscoveryBlocks * mult
		from := uint64(1)
		if st.EntryBlock > span {
			from = st.EntryBlock - span
		}
		var last float64
		var lastBlock uint64
		var lastEv poolEvent
		drainedAt = 0
		if err := o.rpc.getLogsChunked(ctx, addr, topics, from, st.EntryBlock, func(l rpcLog) {
			ev := st.eventOfLog(l)
			if ev.drained {
				drainedAt = l.block()
			} else if ev.price > 0 {
				last, lastBlock, lastEv, drainedAt = ev.price, l.block(), ev, 0
			}
		}); err != nil {
			return err
		}
		if last > 0 {
			setEntry(last, lastBlock, lastEv, st.EntryBlock)
			if drainedAt > 0 {
				rugAt(drainedAt)
			}
			return nil
		}
		if from == 1 {
			break
		}
	}
	// first trade after the call
	to := st.EntryBlock + o.cfg.DiscoveryBlocks*64
	if to > latest {
		to = latest
	}
	// Drained events before the pool's first real trade are a pool not funded
	// yet (a swap into an empty pool), not a rug: they are only skipped.
	var first float64
	var firstBlock uint64
	var firstEv poolEvent
	if err := o.rpc.getLogsChunked(ctx, addr, topics, st.EntryBlock+1, to, func(l rpcLog) {
		if first == 0 {
			if ev := st.eventOfLog(l); ev.price > 0 {
				first, firstBlock, firstEv = ev.price, l.block(), ev
			}
		}
	}); err != nil {
		return err
	}
	if first == 0 {
		return errNoTrades
	}
	setEntry(first, firstBlock, firstEv, firstBlock)
	return nil
}

// quoteUSD: USD value of one unit of the pool's quote asset at a block.
// ok=false when no source is configured (prices then stay in quote units).
func (o *onchainSource) quoteUSD(ctx context.Context, quote string, block uint64) (float64, bool, error) {
	p, ok, err := o.quoteUSDDirect(ctx, quote, block)
	if ok || err != nil {
		return p, ok, err
	}
	// No feed for this asset (e.g. VIRTUAL, a stock token): price it from its own
	// pool against WETH or a stablecoin.
	return o.quoteViaPool(ctx, quote, block)
}

// quoteUSDDirect: stablecoins, Chainlink feeds and the WETH/USDG pool.
func (o *onchainSource) quoteUSDDirect(ctx context.Context, quote string, block uint64) (float64, bool, error) {
	quote = strings.ToLower(quote)
	if o.cfg.Stables[quote] {
		return 1, true, nil
	}
	isETH := quote == zeroAddr || quote == o.cfg.WETH
	// 1. A Chainlink feed on Ethereum mainnet, read on the mainnet archive node at
	//    the block with the same timestamp (ETH/USD by default).
	if o.mainnet != nil {
		mf := o.cfg.MainnetFeeds[quote]
		if mf == "" && isETH {
			mf = o.cfg.MainnetFeeds["eth"]
		}
		if mf != "" {
			p, err := o.mainnetFeed(ctx, mf, block)
			return p, err == nil, err
		}
	}
	// 2. A Chainlink feed on Robinhood Chain.
	feed := o.cfg.Feeds[quote]
	if feed == "" && isETH {
		feed = o.cfg.Feeds["eth"]
	}
	if feed != "" {
		p, err := o.chainlink(ctx, feed, block)
		return p, err == nil, err
	}
	if isETH && o.cfg.EthUSDPool != "" {
		p, err := o.ethFromPool(ctx, block)
		return p, err == nil, err
	}
	return 0, false, nil
}

// mainnetBlockFor converts a Robinhood Chain block number into the Ethereum
// mainnet block number at the same moment. The two chains number their blocks
// independently, so the conversion goes through time: the Robinhood block's
// timestamp → the last mainnet block whose timestamp is not after it.
// rhBlock 0 means "latest" and maps to mainnet "latest" (returned as 0).
func (o *onchainSource) mainnetBlockFor(ctx context.Context, rhBlock uint64) (mainnetBlock uint64, rhTime, mainnetTime int64, err error) {
	if rhBlock == 0 {
		return 0, 0, 0, nil
	}
	if rhTime, err = o.rpc.blockTime(ctx, rhBlock); err != nil {
		return 0, 0, 0, fmt.Errorf("time of Robinhood block %d: %w", rhBlock, err)
	}
	if mainnetBlock, err = o.mainnet.blockAt(ctx, rhTime); err != nil {
		return 0, 0, 0, fmt.Errorf("mainnet block at time %d: %w", rhTime, err)
	}
	if mainnetTime, err = o.mainnet.blockTime(ctx, mainnetBlock); err != nil {
		return 0, 0, 0, err
	}
	// A mainnet block is at most ~12s old relative to the moment asked for. A
	// bigger gap means the mainnet node hasn't synced up to that time.
	if rhTime-mainnetTime > 10*60 {
		return 0, 0, 0, fmt.Errorf("mainnet node is behind: its newest block is %s older than Robinhood block %d (%s)",
			time.Duration(rhTime-mainnetTime)*time.Second, rhBlock, time.Unix(rhTime, 0).UTC().Format(time.RFC3339))
	}
	return mainnetBlock, rhTime, mainnetTime, nil
}

// mainnetFeed reads a Chainlink feed on Ethereum mainnet as of a Robinhood
// Chain block: that block's timestamp → the mainnet block at the same time →
// latestRoundData() there (needs a mainnet archive node, e.g. Erigon).
func (o *onchainSource) mainnetFeed(ctx context.Context, feed string, block uint64) (float64, error) {
	mb, _, _, err := o.mainnetBlockFor(ctx, block)
	if err != nil {
		return 0, err
	}
	fm, err := o.mainnet.tokenInfo(ctx, feed) // decimals() of the feed
	if err != nil {
		return 0, fmt.Errorf("mainnet chainlink %s: %w", feed, err)
	}
	b, err := o.mainnet.ethCall(ctx, feed, selLatestRound, mb)
	if err != nil {
		return 0, fmt.Errorf("mainnet chainlink %s at block %d: %w", feed, mb, err)
	}
	if len(b) < 64 {
		return 0, fmt.Errorf("mainnet chainlink %s: short answer", feed)
	}
	p := bigToFloat(signedWord(b, 1)) / pow10(fm.Decimals)
	if p <= 0 {
		return 0, fmt.Errorf("mainnet chainlink %s: non-positive answer", feed)
	}
	return p, nil
}

// chainlink returns a feed's price as of a block: latestRoundData() at that
// block on an archive node, else the last AnswerUpdated event at or before it.
func (o *onchainSource) chainlink(ctx context.Context, feed string, block uint64) (float64, error) {
	fm, err := o.rpc.tokenInfo(ctx, feed) // decimals() of the feed
	if err != nil {
		return 0, fmt.Errorf("chainlink %s: %w", feed, err)
	}
	var stateErr error
	if block == 0 || !o.noState.Load() {
		b, err := o.rpc.ethCall(ctx, feed, selLatestRound, block)
		if err == nil && len(b) >= 64 {
			if p := bigToFloat(signedWord(b, 1)) / pow10(fm.Decimals); p > 0 {
				return p, nil
			}
			return 0, fmt.Errorf("chainlink %s: non-positive answer", feed)
		}
		if block == 0 {
			return 0, fmt.Errorf("chainlink %s: %v", feed, err)
		}
		stateErr = err // historical state unavailable (full node): use the feed's events
	}
	aggs, err := o.feedAggregators(ctx, feed)
	if err != nil {
		return 0, err
	}
	var best *rpcLog
	for _, agg := range aggs {
		l, err := o.lastLogBefore(ctx, agg, []any{topicAnswerUpdated}, block)
		if err != nil {
			return 0, fmt.Errorf("chainlink %s: %w", feed, err)
		}
		if l != nil && (best == nil || l.block() > best.block()) {
			best = l
		}
	}
	if best == nil || len(best.Topics) < 2 {
		if stateErr != nil {
			return 0, fmt.Errorf("chainlink %s: no historical state on this node (%v) and no AnswerUpdated event found before block %d", feed, stateErr, block)
		}
		return 0, fmt.Errorf("chainlink %s: no AnswerUpdated event found before block %d", feed, block)
	}
	p := bigToFloat(signedWord(unhex(best.Topics[1]), 0)) / pow10(fm.Decimals)
	if p <= 0 {
		return 0, fmt.Errorf("chainlink %s: non-positive answer", feed)
	}
	if stateErr != nil && o.noState.CompareAndSwap(false, true) {
		log.Printf("prices: node has no historical state (%v) — reading Chainlink/ETH prices from event logs instead", stateErr)
	}
	return p, nil
}

// feedAggregators lists the contracts that emit a feed's AnswerUpdated events:
// every phase aggregator behind the proxy (or the address itself if it isn't a proxy).
func (o *onchainSource) feedAggregators(ctx context.Context, feed string) ([]string, error) {
	o.mu.Lock()
	if a, ok := o.aggregators[feed]; ok {
		o.mu.Unlock()
		return a, nil
	}
	o.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != zeroAddr && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if b, err := o.rpc.ethCall(ctx, feed, selPhaseID, 0); err == nil {
		for i := word(b, 0).Int64(); i >= 1 && len(out) < 10; i-- {
			if ab, err := o.rpc.ethCall(ctx, feed, selPhaseAggregator+fmt.Sprintf("%064x", i), 0); err == nil {
				add(addrFromWord(ab))
			} else if err := nonRPC(err); err != nil {
				return nil, err
			}
		}
	} else if err := nonRPC(err); err != nil {
		return nil, err
	}
	if ab, err := o.rpc.ethCall(ctx, feed, selAggregator, 0); err == nil {
		add(addrFromWord(ab))
	} else if err := nonRPC(err); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		add(feed) // not a proxy: the address emits the events itself
	}
	o.mu.Lock()
	if o.aggregators == nil {
		o.aggregators = map[string][]string{}
	}
	o.aggregators[feed] = out
	o.mu.Unlock()
	return out, nil
}

// lastLogBefore finds the latest matching log at or before block, searching
// backwards in windows that double each step (busy contracts answer in the
// first small window; quiet ones, e.g. a stock feed over a weekend, need more),
// up to PriceLookback blocks.
func (o *onchainSource) lastLogBefore(ctx context.Context, address string, topics []any, block uint64) (*rpcLog, error) {
	hi := block
	span := uint64(2000)
	limit := o.rpc.maxChunk // lowered when the node refuses a range (this search only)
	for hi > 0 && block-hi < o.cfg.PriceLookback {
		if span > limit {
			span = limit
		}
		lo := uint64(1)
		if hi > span {
			lo = hi - span + 1
		}
		logs, err := o.rpc.retryLogs(ctx, true, func() ([]rpcLog, error) { return o.rpc.getLogs(ctx, address, topics, lo, hi) })
		if err != nil {
			var refused *rangeRefusedError
			if errors.As(err, &refused) && span > o.rpc.minChunk { // range refused or timed out: retry smaller (this search only)
				span = max(span/2, o.rpc.minChunk)
				limit = span
				o.rpc.splits.Add(1)
				continue
			}
			return nil, err
		}
		var best *rpcLog
		for i := range logs {
			l := &logs[i]
			if best == nil || l.block() > best.block() || (l.block() == best.block() && l.index() > best.index()) {
				best = l
			}
		}
		if best != nil {
			return best, nil
		}
		if lo == 1 {
			break
		}
		hi = lo - 1
		span *= 2
	}
	return nil, nil
}

// ethFromPool: ETH/USD from a v3 WETH/stable pool as of a block: slot0() at
// that block on an archive node, else the pool's last Swap event at or before it.
func (o *onchainSource) ethFromPool(ctx context.Context, block uint64) (float64, error) {
	pool := o.cfg.EthUSDPool
	b0, err := o.rpc.ethCall(ctx, pool, selToken0, 0)
	if err != nil {
		return 0, fmt.Errorf("ETH/USD pool %s: %w", pool, err)
	}
	b1, err := o.rpc.ethCall(ctx, pool, selToken1, 0)
	if err != nil {
		return 0, err
	}
	t0, t1 := addrFromWord(b0), addrFromWord(b1)
	m0, err := o.rpc.tokenInfo(ctx, t0)
	if err != nil {
		return 0, err
	}
	m1, err := o.rpc.tokenInfo(ctx, t1)
	if err != nil {
		return 0, err
	}
	orient := func(p float64) (float64, error) { // p = token0 in token1
		if p <= 0 {
			return 0, errors.New("ETH/USD pool: zero price")
		}
		if t0 == o.cfg.WETH {
			return p, nil
		}
		return 1 / p, nil
	}
	var stateErr error
	if block == 0 || !o.noState.Load() {
		s, err := o.rpc.ethCall(ctx, pool, selSlot0, block)
		if err == nil {
			return orient(priceFromSqrtX96(word(s, 0), m0.Decimals, m1.Decimals))
		}
		if block == 0 {
			return 0, fmt.Errorf("ETH/USD pool slot0: %w", err)
		}
		stateErr = err
	}
	l, err := o.lastLogBefore(ctx, pool, []any{topicSwapV3}, block)
	if err != nil {
		return 0, fmt.Errorf("ETH/USD pool swaps: %w", err)
	}
	if l == nil {
		return 0, fmt.Errorf("ETH/USD pool %s: no historical state and no swap found before block %d", pool, block)
	}
	p, err := orient(priceFromSqrtX96(word(unhex(l.Data), 2), m0.Decimals, m1.Decimals))
	if err == nil && stateErr != nil && o.noState.CompareAndSwap(false, true) {
		log.Printf("prices: node has no historical state (%v) — reading Chainlink/ETH prices from event logs instead", stateErr)
	}
	return p, err
}

// liquidityUSD: the pool's quote side in USD = the quote asset the pool
// holds (balanceOf, every range of a v3 pool and uncollected fees included) ×
// qUSD. v2/v3 only: v4 pools share one contract. Compared with
// SCOUT_RUG_LIQ_USD as it is; current_liquidity_usd stores poolDepthUSD of it.
func (o *onchainSource) liquidityUSD(ctx context.Context, st *onchainState, qUSD float64) (float64, bool) {
	if st.Kind == "v4" || st.Quote == zeroAddr {
		return 0, false
	}
	b, err := o.rpc.ethCall(ctx, st.Quote, selBalanceOf+strings.TrimPrefix(addrTopic(st.Pool), "0x"), 0)
	if err != nil {
		return 0, false
	}
	return bigToFloat(word(b, 0)) / pow10(st.QuoteDec) * qUSD, true
}

// quoteSource names where a quote asset's USD price comes from (for diagnostics).
func (o *onchainSource) quoteSource(quote string) string {
	quote = strings.ToLower(quote)
	if d := o.quoteSourceDirect(quote); d != "none" {
		return d
	}
	o.mu.Lock()
	qp := o.quotePools[quote]
	o.mu.Unlock()
	if qp != nil {
		return "its " + qp.QuoteSym + " pool (uniswap-" + qp.Kind + ")"
	}
	return "none"
}

func (o *onchainSource) describe() string {
	limit := "no rate limit"
	if o.cfg.RPS > 0 {
		limit = fmt.Sprintf("%d req/s", o.cfg.RPS)
	}
	s := fmt.Sprintf("eth_getLogs ranges of up to %d blocks, %d ranges per scan, up to %d requests in flight, %s; ", o.rpc.maxChunk, o.cfg.Parallel, cap(o.rpc.slots), limit) + fmt.Sprintf("on-chain pools via %s (v4 PoolManager %s, %d Chainlink feed(s) on Robinhood Chain)", o.cfg.RPCURL, o.cfg.PoolManagerV4, len(o.cfg.Feeds))
	if o.mainnet != nil {
		s += fmt.Sprintf("; ETH/USD + %d other feed(s) from Ethereum mainnet via %s", len(o.cfg.MainnetFeeds)-1, o.cfg.MainnetRPCURL)
	}
	return s
}

func init() {
	// The event topics are derived from signatures; a typo would silently match nothing.
	if topicTransfer != "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" {
		log.Fatal("keccak self-check failed")
	}
}
