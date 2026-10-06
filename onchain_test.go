package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fake chain: JSON-RPC server with blocks, logs and eth_call handlers.
// ---------------------------------------------------------------------------

type fakeLog struct {
	addr   string
	topics []string
	data   string
	block  uint64
	tx     string
}

type fakeChain struct {
	mu       sync.Mutex
	t0       int64                // timestamp of block 1
	timeOf   func(n uint64) int64 // block → timestamp
	latest   uint64
	logs     []fakeLog
	calls    map[string]func(block uint64) (string, bool) // "to|selector"
	maxRange uint64                                       // eth_getLogs block-range cap (0 = none)
	// logsHook (optional) sees every eth_getLogs before the chain does and may
	// answer it with a JSON-RPC error (non-empty return) or sleep. Called without
	// f.mu held, possibly from several requests at once.
	logsHook func(addr string, from, to uint64) string
	fullNode bool     // no historical state: eth_call at old blocks fails
	callTags []string // block tag of every eth_call received
	count    map[string]int
	srv      *httptest.Server
}

func newFakeChain(t *testing.T, age time.Duration) *fakeChain {
	f := &fakeChain{t0: time.Now().Add(-age).Unix(), calls: map[string]func(uint64) (string, bool){}, count: map[string]int{}}
	f.timeOf = func(n uint64) int64 { return f.t0 + int64(n-1)/10 } // 10 blocks per second
	f.latest = uint64(time.Now().Unix()-f.t0) * 10
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChain) setLogsHook(h func(addr string, from, to uint64) string) {
	f.mu.Lock()
	f.logsHook = h
	f.mu.Unlock()
}

// blockAtTime: the last block with timestamp <= ts.
func (f *fakeChain) blockAtTime(ts time.Time) uint64 { return uint64(ts.Unix()-f.t0)*10 + 10 }

func (f *fakeChain) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	hook := f.logsHook
	f.mu.Unlock()
	if req.Method == "eth_getLogs" && hook != nil && len(req.Params) == 1 {
		var q struct{ Address, FromBlock, ToBlock string }
		json.Unmarshal(req.Params[0], &q)
		from, _ := strconv.ParseUint(strings.TrimPrefix(q.FromBlock, "0x"), 16, 64)
		to, _ := strconv.ParseUint(strings.TrimPrefix(q.ToBlock, "0x"), 16, 64)
		if msg := hook(strings.ToLower(q.Address), from, to); msg != "" {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": msg}})
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count[req.Method]++
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": v}) }
	fail := func(msg string) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": msg}})
	}
	num := func(s string) uint64 {
		if s == "latest" {
			return f.latest
		}
		n, _ := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
		return n
	}
	switch req.Method {
	case "eth_blockNumber":
		ok(hexU64(f.latest))
	case "eth_getBlockByNumber":
		var tag string
		json.Unmarshal(req.Params[0], &tag)
		n := num(tag)
		if n == 0 || n > f.latest {
			ok(nil)
			return
		}
		ok(map[string]string{"timestamp": hexU64(uint64(f.timeOf(n)))})
	case "eth_getLogs":
		var q struct {
			Address   string `json:"address"`
			Topics    []any  `json:"topics"`
			FromBlock string `json:"fromBlock"`
			ToBlock   string `json:"toBlock"`
		}
		json.Unmarshal(req.Params[0], &q)
		from, to := num(q.FromBlock), num(q.ToBlock)
		if f.maxRange > 0 && to-from+1 > f.maxRange {
			fail("block range too large")
			return
		}
		out := []map[string]any{}
		for i, l := range f.logs {
			if l.block < from || l.block > to || !strings.EqualFold(l.addr, q.Address) {
				continue
			}
			match := true
			for ti, want := range q.Topics {
				switch w := want.(type) {
				case string: // this topic must be w
					if ti >= len(l.topics) || !strings.EqualFold(l.topics[ti], w) {
						match = false
					}
				case []any: // any one of these (an OR list, as the node treats it)
					any1 := false
					for _, o := range w {
						if os, ok := o.(string); ok && ti < len(l.topics) && strings.EqualFold(l.topics[ti], os) {
							any1 = true
						}
					}
					if !any1 {
						match = false
					}
				} // nil: any value
			}
			if match {
				out = append(out, map[string]any{"address": l.addr, "topics": l.topics, "data": l.data,
					"blockNumber": hexU64(l.block), "transactionHash": l.tx, "logIndex": hexU64(uint64(i))})
			}
		}
		ok(out)
	case "eth_call":
		var c struct{ To, Data string }
		var tag string
		json.Unmarshal(req.Params[0], &c)
		json.Unmarshal(req.Params[1], &tag)
		sel := c.Data
		if len(sel) > 10 {
			sel = sel[:10]
		}
		f.callTags = append(f.callTags, tag)
		if f.fullNode && tag != "latest" {
			fail("missing trie node 6b1f… (path ) state 0x… is not available")
			return
		}
		h := f.calls[strings.ToLower(c.To)+"|"+sel]
		if h == nil {
			fail("execution reverted")
			return
		}
		res, good := h(num(tag))
		if !good {
			fail("execution reverted")
			return
		}
		ok(res)
	default:
		fail("unsupported " + req.Method)
	}
}

func w32(v *big.Int) string {
	if v.Sign() < 0 {
		v = new(big.Int).Add(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return fmt.Sprintf("%064x", v)
}
func wInt(n int64) string { return w32(big.NewInt(n)) }
func wAddr(a string) string {
	return strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(a), "0x")
}
func ret(words ...string) string { return "0x" + strings.Join(words, "") }

func (f *fakeChain) constCall(to, sel string, result string) {
	f.calls[strings.ToLower(to)+"|"+sel] = func(uint64) (string, bool) { return result, true }
}

func abiString(s string) string {
	b := []byte(s)
	padded := make([]byte, (len(b)+31)/32*32)
	copy(padded, b)
	return ret(wInt(32), wInt(int64(len(b))), hex.EncodeToString(padded))
}

func (f *fakeChain) addToken(addr string, dec int, sym string) {
	f.constCall(addr, selDecimals, ret(wInt(int64(dec))))
	f.constCall(addr, selSymbol, abiString(sym))
}

// sqrtX96 for a raw (not decimal-adjusted) price of token0 in token1.
func sqrtX96(raw float64) *big.Int {
	x := new(big.Float).SetPrec(256).SetFloat64(raw)
	x.Sqrt(x)
	x.Mul(x, new(big.Float).SetPrec(256).SetInt(new(big.Int).Lsh(big.NewInt(1), 96)))
	out, _ := x.Int(nil)
	return out
}

func (f *fakeChain) transfer(token, from, to string, block uint64, tx string) {
	f.logs = append(f.logs, fakeLog{addr: token, topics: []string{topicTransfer, addrTopic(from), addrTopic(to)}, data: ret(wInt(1)), block: block, tx: tx})
}

// fakeLiq is the in-range liquidity L of the fake v3/v4 swaps: deep enough that
// every test price leaves the pool far above the rug threshold.
var fakeLiq = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)

func (f *fakeChain) swapV3(pool string, block uint64, tx string, sqrtP *big.Int) {
	f.swapV3L(pool, block, tx, sqrtP, fakeLiq, 0)
}

// swapV3L is swapV3 with the pool's liquidity and tick after the swap.
func (f *fakeChain) swapV3L(pool string, block uint64, tx string, sqrtP, liq *big.Int, tick int64) {
	f.logs = append(f.logs, fakeLog{addr: pool, topics: []string{topicSwapV3, addrTopic("0xaa"), addrTopic("0xbb")},
		data: ret(wInt(-5), wInt(7), w32(sqrtP), w32(liq), wInt(tick)), block: block, tx: tx})
}
func (f *fakeChain) syncV2(pool string, block uint64, tx string, r0, r1 *big.Int) {
	f.logs = append(f.logs, fakeLog{addr: pool, topics: []string{topicSyncV2}, data: ret(w32(r0), w32(r1)), block: block, tx: tx})
}
func (f *fakeChain) swapV4(pm, id string, block uint64, tx string, sqrtP *big.Int) {
	f.swapV4L(pm, id, block, tx, sqrtP, fakeLiq, 0)
}

// swapV4L is swapV4 with the pool's liquidity and tick after the swap.
func (f *fakeChain) swapV4L(pm, id string, block uint64, tx string, sqrtP, liq *big.Int, tick int64) {
	f.logs = append(f.logs, fakeLog{addr: pm, topics: []string{topicSwapV4, id, addrTopic("0xaa")},
		data: ret(wInt(5), wInt(-7), w32(sqrtP), w32(liq), wInt(tick), wInt(3000)), block: block, tx: tx})
}

func testOnchain(t *testing.T, f *fakeChain, extraEnv map[string]string) *onchainSource {
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	for k, v := range extraEnv {
		t.Setenv(k, v)
	}
	oc, err := loadOnchainConfig()
	if err != nil {
		t.Fatal(err)
	}
	return newOnchainSource(oc)
}

const (
	tWETH = "0x0bd7d308f8e1639fab988df18a8011f41eacad73"
	tUSDG = "0x5fc5360d0400a0fd4f2af552add042d716f1d168"
	tPM   = "0x8366a39cc670b4001a1121b8f6a443a643e40951"
)

// ---------------------------------------------------------------------------

func TestABIConstantsAndMath(t *testing.T) {
	for got, want := range map[string]string{
		topicSwapV3:    "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67",
		topicSyncV2:    "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1",
		selToken0:      "0x0dfe1681",
		selToken1:      "0xd21220a7",
		selGetReserves: "0x0902f1ac",
		selSlot0:       "0x3850c7bd",
		selDecimals:    "0x313ce567",
		selBalanceOf:   "0x70a08231",
		selLatestRound: "0xfeaf968c",
	} {
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
	// 1 token0 (18 dec) = 2000 token1 (6 dec): raw ratio 2000e6/1e18
	p := priceFromSqrtX96(sqrtX96(2000e6/1e18), 18, 6)
	if math.Abs(p-2000) > 1e-6 {
		t.Errorf("sqrt price %v", p)
	}
	if p := priceFromReserves(big.NewInt(5e9), new(big.Int).Mul(big.NewInt(1e9), big.NewInt(1e10)), 6, 18); math.Abs(p-0.002) > 1e-12 {
		t.Errorf("reserve price %v", p) // 5000 USDG(6) vs 10 WETH(18) → 0.002 WETH per USDG
	}
	if v := signedWord(unhex(wInt(-42)), 0); v.Int64() != -42 {
		t.Errorf("signed %v", v)
	}
	if a := addrFromWord(unhex(wAddr(tWETH))); a != tWETH {
		t.Errorf("addr %s", a)
	}
	if s := decodeString(unhex(strings.TrimPrefix(abiString("NVDA"), "0x"))); s != "NVDA" {
		t.Errorf("string %q", s)
	}
}

func TestBlockAt(t *testing.T) {
	f := newFakeChain(t, 40*24*time.Hour)
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	for _, ago := range []time.Duration{35 * 24 * time.Hour, 8 * 24 * time.Hour, time.Hour} {
		ts := time.Now().Add(-ago).Unix()
		n, err := o.rpc.blockAt(ctx, ts)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.timeOf(n); got > ts || ts-got > 1 {
			t.Errorf("blockAt(-%s) = %d with time %d, want within 1s before %d", ago, n, got, ts)
		}
	}
	if f.count["eth_getBlockByNumber"] > 120 {
		t.Errorf("too many block lookups: %d", f.count["eth_getBlockByNumber"])
	}
	// irregular block production: blocks 5x slower in the second half of history
	g := newFakeChain(t, 40*24*time.Hour)
	half := g.latest / 2
	g.timeOf = func(n uint64) int64 {
		if n <= half {
			return g.t0 + int64(n)/20
		}
		return g.t0 + int64(half)/20 + int64(n-half)/4
	}
	o2 := testOnchain(t, g, nil)
	ts := g.timeOf(half + 123456)
	n, err := o2.rpc.blockAt(ctx, ts)
	if err != nil || g.timeOf(n) > ts || ts-g.timeOf(n) > 1 {
		t.Fatalf("irregular: block %d time %d want %d (%v)", n, g.timeOf(n), ts, err)
	}
	if last, _ := o2.rpc.blockAt(ctx, g.timeOf(g.latest)+999); last != g.latest {
		t.Fatalf("future timestamp should give latest")
	}
}

// rangeLog records the eth_getLogs ranges a fake chain was asked for.
type rangeLog struct {
	mu   sync.Mutex
	asks []rangeAsk
}

type rangeAsk struct {
	addr     string
	from, to uint64
	refused  bool
}

func (r *rangeLog) add(a rangeAsk) {
	r.mu.Lock()
	r.asks = append(r.asks, a)
	r.mu.Unlock()
}

func (r *rangeLog) list(addr string) []rangeAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rangeAsk
	for _, a := range r.asks {
		if addr == "" || a.addr == addr {
			out = append(out, a)
		}
	}
	return out
}

// captureRangeLog sends the standard logger to a buffer for the rest of the test and
// prints every range-size line (no rate limit).
func captureRangeLog(t *testing.T) *syncBuf {
	var buf syncBuf
	prevOut, prevEvery := log.Writer(), rangeNoteEvery
	log.SetOutput(&buf)
	rangeNoteEvery = 0
	t.Cleanup(func() { log.SetOutput(prevOut); rangeNoteEvery = prevEvery })
	return &buf
}

func fastRetries(t *testing.T) {
	old := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = old })
}

// A node that refuses big ranges over one busy stretch of blocks: the scan goes
// smaller there, and back up to SCOUT_RPC_LOG_CHUNK once past it.
func TestGetLogsRangeShrinksOnRefusalAndGrowsBack(t *testing.T) {
	buf := captureRangeLog(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(1000); b <= 3_000_000; b += 7001 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	var rl rangeLog
	f.setLogsHook(func(addr string, from, to uint64) string {
		refuse := from <= 100_000 && to-from+1 > 5000 // busy stretch: at most 5000 blocks per request
		rl.add(rangeAsk{addr, from, to, refuse})
		if refuse {
			return "query returned more than 10000 results"
		}
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000"})
	ctx := withScanProgress(context.Background(), "call 1 [1/1]", 0)
	var got []uint64
	if err := o.rpc.getLogsChunked(ctx, pool, []any{topicSwapV3}, 1, 3_000_000, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %d logs, want %d (in block order)", len(got), len(want))
	}
	asks := rl.list(pool)
	refusals := 0
	for _, a := range asks {
		if n := a.to - a.from + 1; n > 200_000 {
			t.Fatalf("asked for %d blocks, above SCOUT_RPC_LOG_CHUNK", n)
		}
		if a.refused {
			refusals++
		}
	}
	if refusals == 0 {
		t.Fatalf("%d refusals", refusals)
	}
	// Back to full-size ranges well before the end of the scan.
	for _, a := range asks {
		if a.from >= 2_000_000 && (a.refused || (a.to-a.from+1 != 200_000 && a.to != 3_000_000)) {
			t.Fatalf("range did not grow back: asked for %d-%d", a.from, a.to)
		}
	}
	t.Logf("%d requests for 3M blocks, %d of them refused", len(asks), refusals)
	if len(asks) > 200 { // 960 at the smallest size
		t.Fatalf("%d requests for 3M blocks: grew back too slowly", len(asks))
	}
	out := buf.String()
	for _, w := range []string{
		"call 1 [1/1]: eth_getLogs blocks 1-199999 (199999 blocks) refused as too large (rpc error -32000: query returned more than 10000 results); this scan continues with 100000-block ranges (max 200000)",
		"call 1 [1/1]: eth_getLogs ranges back up to 200000 blocks (max 200000)",
		"200000-block ranges)",
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("log is missing %q:\n%s", w, out)
		}
	}
	// A new scan (here of another pool, outside the busy stretch) starts at the configured size.
	other := "0x00000000000000000000000000000000000000c2"
	if err := o.rpc.getLogsChunked(context.Background(), other, []any{topicSwapV3}, 200_000, 599_999, func(rpcLog) {}); err != nil {
		t.Fatal(err)
	}
	if a := rl.list(other); len(a) != 2 || a[0].to-a[0].from+1 != 200_000 || a[1].to-a[1].from+1 != 200_000 {
		t.Fatalf("new scan asked for %+v", a)
	}
}

// Passing trouble is retried at the same size and never makes the ranges
// smaller: client time-outs that call()'s own retries get past, one "request
// timed out" from the node, rate limits and a busy node.
func TestGetLogsTimeoutsDoNotShrinkRange(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(5000); b <= 1_000_000; b += 90_001 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	var rl rangeLog
	var n atomic.Int64
	f.setLogsHook(func(addr string, from, to uint64) string {
		rl.add(rangeAsk{addr: addr, from: from, to: to})
		switch n.Add(1) {
		case 1, 2: // slower than the client waits: a transport timeout
			time.Sleep(300 * time.Millisecond)
		case 3:
			return "request timed out"
		case 4:
			return "rate limit exceeded, too many requests"
		case 5:
			return "server busy, try again later"
		}
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000", "SCOUT_RPC_PARALLEL": "1"})
	o.rpc.http.Timeout = 100 * time.Millisecond
	var got []uint64
	if err := o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 1, 1_000_000, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	asks := rl.list(pool)
	for _, a := range asks {
		if (a.to+1)%200_000 != 0 && a.to != 1_000_000 {
			t.Fatalf("range shrank after a timeout: asked for %d-%d", a.from, a.to)
		}
	}
	if len(asks) != 6+5 { // 6 ranges, the first one asked 6 times (2 timeouts, 3 busy answers)
		t.Fatalf("%d requests: %+v", len(asks), asks)
	}
	if strings.Contains(buf.String(), "refused") || o.rpc.splits.Load() != 0 {
		t.Fatalf("a timeout was treated as a refusal:\n%s", buf.String())
	}
	// A node that stays busy or rate limiting fails the scan (it is tried again
	// next cycle from its saved cursor) instead of shrinking the range: each
	// range is asked logsRetries times at full size, never in halves.
	for _, msg := range []string{"rate limit exceeded", "server busy, try again later", "too many requests",
		"rate limit exceeded: request timed out", "service temporarily unavailable"} {
		var busy rangeLog
		f.setLogsHook(func(addr string, from, to uint64) string {
			busy.add(rangeAsk{addr: addr, from: from, to: to})
			return msg
		})
		err := o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 1, 1_000_000, func(rpcLog) {})
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Fatalf("%q: want that error back, got %v", msg, err)
		}
		if o.rpc.splits.Load() != 0 {
			t.Fatalf("%q split a range", msg)
		}
		asks := busy.list(pool)
		if len(asks) != logsRetries {
			t.Fatalf("%q: %d requests, want %d", msg, len(asks), logsRetries)
		}
		for _, a := range asks {
			if a.from != 1 || a.to != 199_999 {
				t.Fatalf("%q: asked for %d-%d", msg, a.from, a.to)
			}
		}
	}
}

// A refusal in one call's scan does not shrink another call's ranges.
func TestGetLogsRefusalStaysWithItsScan(t *testing.T) {
	captureRangeLog(t)
	f := newFakeChain(t, 10*24*time.Hour)
	busy, quiet := "0x00000000000000000000000000000000000000b1", "0x00000000000000000000000000000000000000b2"
	var rl rangeLog
	f.setLogsHook(func(addr string, from, to uint64) string {
		refuse := addr == busy && to-from+1 > 5000
		rl.add(rangeAsk{addr, from, to, refuse})
		if refuse {
			return "Log response size exceeded. You can make eth_getLogs requests with up to a 5K block range"
		}
		time.Sleep(time.Millisecond) // keep both scans running side by side
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000", "SCOUT_RPC_PARALLEL": "2"})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, addr := range []string{busy, quiet} {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			ctx := withScanProgress(context.Background(), fmt.Sprintf("call %d [%d/2]", i+1, i+1), time.Hour)
			errs[i] = o.rpc.getLogsChunked(ctx, addr, []any{topicSwapV3}, 1, 1_999_999, func(rpcLog) {})
		}(i, addr)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if len(rl.list(busy)) < 20 {
		t.Fatalf("busy scan: %d requests", len(rl.list(busy)))
	}
	for _, a := range rl.list(quiet) {
		if a.to-a.from+1 < 199_999 {
			t.Fatalf("quiet scan shrank to %d blocks while the other scan was refused", a.to-a.from+1)
		}
	}
	if q := len(rl.list(quiet)); q != 10 {
		t.Fatalf("quiet scan made %d requests, want 10", q)
	}
}

// What counts as "too large", what as a passing error.
func TestClassifyLogsErr(t *testing.T) {
	rpc := func(msg string) error { return &rpcError{Code: -32000, Message: msg} }
	for msg, want := range map[string]logsErrKind{
		"block range too large":                               logsTooLarge,
		"query returned more than 10000 results":              logsTooLarge,
		"exceed maximum block range: 50000":                   logsTooLarge,
		"Log response size exceeded.":                         logsTooLarge,
		"eth_getLogs is limited to a 10,000 range":            logsTooLarge,
		"rate limit exceeded":                                 logsBusy,
		"project ID request rate exceeded":                    logsBusy,
		"too many requests":                                   logsBusy,
		"request timed out":                                   logsTimedOut,
		"Request Timed Out":                                   logsTimedOut,
		"Query timeout exceeded. Consider reducing the range": logsTimedOut,
		"context deadline exceeded":                           logsTimedOut,
		"rate limit exceeded: request timed out":              logsBusy,
		"server busy, request timed out":                      logsBusy,
		"server is busy":                                      logsBusy,
		"header not found":                                    logsBusy,
		"something odd happened":                              logsUnexplained,
	} {
		if got := classifyLogsErr(rpc(msg)); got != want {
			t.Errorf("%q: got %d, want %d", msg, got, want)
		}
	}
	if classifyLogsErr(fmt.Errorf("eth_getLogs: %w", errResponseTooLarge)) != logsTooLarge {
		t.Error("cut-off response")
	}
	if classifyLogsErr(fmt.Errorf("eth_getLogs: %w", context.DeadlineExceeded)) != logsClientTimeout {
		t.Error("client time-out (after call's retries)")
	}
	if classifyLogsErr(errors.New("connection reset")) != logsOther || classifyLogsErr(context.Canceled) != logsOther {
		t.Error("other transport errors are retried by call, not split")
	}
}

// An answer bigger than the client reads is a "result too large": the range is
// split instead of failing the scan; an error the node does not explain is
// asked again once, then treated the same.
func TestGetLogsSplitsOversizedAndUnexplained(t *testing.T) {
	captureRangeLog(t)
	fastRetries(t)
	old := maxResponseBytes
	maxResponseBytes = 3000
	t.Cleanup(func() { maxResponseBytes = old })
	f := newFakeChain(t, 2*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(10); b <= 3200; b += 70 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "3200"})
	var got []uint64
	if err := o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 1, 3200, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || o.rpc.splits.Load() == 0 {
		t.Fatalf("got %d logs (want %d), %d splits", len(got), len(want), o.rpc.splits.Load())
	}

	var rl rangeLog
	f.setLogsHook(func(addr string, from, to uint64) string {
		odd := to-from+1 > 50_000
		rl.add(rangeAsk{addr, from, to, odd})
		if odd {
			return "something odd happened"
		}
		return ""
	})
	o2 := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000", "SCOUT_RPC_PARALLEL": "1"})
	if err := o2.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 200_000, 399_999, func(rpcLog) {}); err != nil {
		t.Fatal(err)
	}
	asks := rl.list(pool)
	if len(asks) < 4 || asks[0] != asks[1] || asks[2].to-asks[2].from+1 != 100_000 {
		t.Fatalf("want the same range asked twice, then halves: %+v", asks[:min(len(asks), 4)])
	}
}

// v3 pool TOKEN/WETH where the token is token1, priced in USD via a Chainlink ETH feed.
func TestDiscoverAndPriceV3WithChainlink(t *testing.T) {
	f := newFakeChain(t, 40*24*time.Hour)
	token, pool, feed, wallet := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3",
		"0x00000000000000000000000000000000000000fe", "0x00000000000000000000000000000000000000d1"
	f.addToken(token, 18, "MALFOID")
	f.addToken(tWETH, 18, "WETH")
	f.constCall(pool, selToken0, ret(wAddr(tWETH))) // WETH < token → token is token1
	f.constCall(pool, selToken1, ret(wAddr(token)))
	f.constCall(pool, selSlot0, ret(wInt(1)))
	f.constCall(feed, selDecimals, ret(wInt(8)))
	entry := time.Now().Add(-8 * 24 * time.Hour)
	eb := f.blockAtTime(entry)
	// ETH = $3000 at entry, $3300 from day 2 on
	f.calls[feed+"|"+selLatestRound] = func(b uint64) (string, bool) {
		px := int64(3000e8)
		if b > eb+2*864000 {
			px = 3300e8
		}
		return ret(wInt(1), wInt(px), wInt(0), wInt(0), wInt(1)), true
	}
	// token price in WETH: p → pool raw price (WETH per... token0=WETH in token1=token) = 1/p
	at := func(block uint64, pWETH float64, tx string) {
		f.swapV3(pool, block, tx, sqrtX96(1/pWETH))
		f.transfer(token, pool, wallet, block, tx)
	}
	at(eb-500, 0.0000010, "0xa1") // last trade before the call → entry
	at(eb+300, 0.0000012, "0xa2")
	at(eb+20000, 0.0000050, "0xa3") // spike ~33 min in
	at(eb+30000, 0.0000020, "0xa4")
	at(eb+3*864000, 0.0000005, "0xa5")                                                     // day 3: dumps
	f.transfer(token, wallet, "0x00000000000000000000000000000000000000d2", eb+10, "0xb1") // wallet-to-wallet noise

	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + feed})
	ctx := context.Background()
	block, err := o.rpc.blockAt(ctx, entry.Unix())
	if err != nil {
		t.Fatal(err)
	}
	st, err := o.discover(ctx, token, block)
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind != "v3" || st.Pool != pool || st.TokenIs0 || st.Quote != tWETH || st.QuoteSym != "WETH" {
		t.Fatalf("discover: %+v", st)
	}
	if err := o.entryPrice(ctx, st, f.latest); err != nil || math.Abs(st.EntryPriceQ-0.000001) > 1e-12 {
		t.Fatalf("entry %v %v", st.EntryPriceQ, err)
	}
	q, ok, err := o.quoteUSD(ctx, st.Quote, st.EntryBlock)
	if err != nil || !ok || q != 3000 {
		t.Fatalf("quoteUSD %v %v %v", q, ok, err)
	}
	// scan to +1h: last 0.000002, max 0.000005, min 0.000001
	if err := o.scan(ctx, st, eb+36000, nil); err != nil {
		t.Fatal(err)
	}
	if math.Abs(st.LastPriceQ-0.000002) > 1e-12 || math.Abs(st.RunMaxQ-0.000005) > 1e-12 || math.Abs(st.RunMinQ-0.000001) > 1e-12 {
		t.Fatalf("after 1h: %+v", st)
	}
	// incremental: scanning to day 7 only reads the new range and picks up the dump
	if err := o.scan(ctx, st, eb+7*864000, nil); err != nil || math.Abs(st.LastPriceQ-0.0000005) > 1e-13 || math.Abs(st.RunMinQ-0.0000005) > 1e-13 {
		t.Fatalf("after 7d: %+v %v", st, err)
	}
	if q, _, _ := o.quoteUSD(ctx, st.Quote, eb+7*864000); q != 3300 {
		t.Fatalf("later ETH price %v", q)
	}
}

// v2 pair TOKEN/USDG (stablecoin → $1, no oracle needed).
func TestDiscoverAndPriceV2Stable(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	token, pair := "0x2222222222222222222222222222222222222222", "0x00000000000000000000000000000000000000c2"
	f.addToken(token, 18, "TKN")
	f.addToken(tUSDG, 6, "USDG")
	f.constCall(pair, selToken0, ret(wAddr(token)))
	f.constCall(pair, selToken1, ret(wAddr(tUSDG)))
	f.constCall(pair, selGetReserves, ret(wInt(1), wInt(1), wInt(0)))
	f.constCall(tUSDG, selBalanceOf, ret(w32(big.NewInt(12_000e6)))) // pool holds 12k USDG
	entry := time.Now().Add(-2 * 24 * time.Hour)
	eb := f.blockAtTime(entry)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	sync := func(block uint64, tokens, usdg int64, tx string) {
		f.syncV2(pair, block, tx, new(big.Int).Mul(big.NewInt(tokens), e18), big.NewInt(usdg*1e6))
		f.transfer(token, "0x00000000000000000000000000000000000000d1", pair, block, tx)
	}
	sync(eb-100, 1_000_000, 5_000, "0x1") // $0.005
	sync(eb+5000, 800_000, 6_250, "0x2")  // ≈ $0.0078125
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	st, err := o.discover(ctx, token, eb)
	if err != nil || st.Kind != "v2" || !st.TokenIs0 || st.Quote != tUSDG || st.QuoteDec != 6 {
		t.Fatalf("discover: %+v %v", st, err)
	}
	if err := o.entryPrice(ctx, st, f.latest); err != nil || math.Abs(st.EntryPriceQ-0.005) > 1e-12 {
		t.Fatalf("entry %v %v", st.EntryPriceQ, err)
	}
	o.scan(ctx, st, eb+10000, nil)
	if math.Abs(st.LastPriceQ-0.0078125) > 1e-12 {
		t.Fatalf("last %v", st.LastPriceQ)
	}
	if q, ok, _ := o.quoteUSD(ctx, st.Quote, eb); !ok || q != 1 {
		t.Fatalf("stable quote %v %v", q, ok)
	}
	// the quote side only: the 12k USDG the pool holds (the column stores 2×)
	if liq, ok := o.liquidityUSD(ctx, st, 1); !ok || liq != 12_000 || poolDepthUSD(liq) != 24_000 {
		t.Fatalf("liquidity: got %v %v, want quote side 12000 (depth 24000)", liq, ok)
	}
}

// v4 pool on the PoolManager, paired with a Stock Token (Long.xyz style):
// found via swaps in the same transactions as the token's transfers.
func TestDiscoverV4StockTokenPair(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	token, nvda, feed := "0x9999999999999999999999999999999999991e18", "0x1111111111111111111111111111111111111111", "0x00000000000000000000000000000000000000ff"
	id, otherID := "0x"+strings.Repeat("ab", 32), "0x"+strings.Repeat("cd", 32)
	f.addToken(token, 18, "LONGTKN")
	f.addToken(nvda, 18, "NVDA")
	f.constCall(feed, selDecimals, ret(wInt(8)))
	f.constCall(feed, selLatestRound, ret(wInt(1), wInt(150e8), wInt(0), wInt(0), wInt(1))) // NVDA = $150
	entry := time.Now().Add(-24 * time.Hour)
	eb := f.blockAtTime(entry)
	// pool created well before the call; nvda (0x11…) < token (0x99…) → token is currency1
	f.logs = append(f.logs, fakeLog{addr: tPM, topics: []string{topicInitV4, id, addrTopic(nvda), addrTopic(token)}, data: ret(wInt(3000)), block: eb - 400000, tx: "0xinit"})
	swap := func(block uint64, pInNVDA float64, tx string) {
		f.swapV4(tPM, id, block, tx, sqrtX96(1/pInNVDA))
		f.transfer(token, tPM, "0x00000000000000000000000000000000000000d1", block, tx)
	}
	swap(eb-50, 0.00002, "0xs1")
	swap(eb+900, 0.00006, "0xs2")
	f.swapV4(tPM, otherID, eb+5, "0xunrelated", sqrtX96(1)) // another pool's swap in the window

	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": nvda + "=" + feed})
	ctx := context.Background()
	st, err := o.discover(ctx, token, eb)
	if err != nil || st.Kind != "v4" || st.PoolID != id || st.TokenIs0 || st.Quote != nvda || st.QuoteSym != "NVDA" {
		t.Fatalf("discover: %+v %v", st, err)
	}
	if err := o.entryPrice(ctx, st, f.latest); err != nil || math.Abs(st.EntryPriceQ-0.00002) > 1e-12 {
		t.Fatalf("entry %v %v", st.EntryPriceQ, err)
	}
	o.scan(ctx, st, eb+2000, nil)
	if math.Abs(st.LastPriceQ-0.00006) > 1e-12 {
		t.Fatalf("last %v (other pool's swap must be ignored)", st.LastPriceQ)
	}
	if q, ok, err := o.quoteUSD(ctx, nvda, eb); err != nil || !ok || q != 150 {
		t.Fatalf("NVDA feed %v %v %v", q, ok, err)
	}
	// without a feed for the stock token there is no USD price
	o2 := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": ""})
	if _, ok, err := o2.quoteUSD(ctx, nvda, eb); ok || err != nil {
		t.Fatalf("expected no USD source, got ok=%v err=%v", ok, err)
	}
}

func TestDiscoverNoPool(t *testing.T) {
	f := newFakeChain(t, 5*24*time.Hour)
	token := "0x3333333333333333333333333333333333333333"
	f.addToken(token, 18, "X")
	eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
	f.transfer(token, "0x00000000000000000000000000000000000000d1", "0x00000000000000000000000000000000000000d2", eb, "0x1") // wallets only
	o := testOnchain(t, f, nil)
	if _, err := o.discover(context.Background(), token, eb); err != errNoPool {
		t.Fatalf("want errNoPool, got %v", err)
	}
}

// wei converts ether to wei.
func wei(eth float64) *big.Int {
	f := new(big.Float).Mul(big.NewFloat(eth), big.NewFloat(1e18))
	out, _ := f.Int(nil)
	return out
}

func (f *fakeChain) answerUpdated(agg string, block uint64, answer int64) {
	f.logs = append(f.logs, fakeLog{addr: agg, topics: []string{topicAnswerUpdated, "0x" + wInt(answer), "0x" + wInt(int64(block))},
		data: ret(wInt(0)), block: block, tx: hexU64(block)})
}

// A full (non-archive) node: historical eth_call fails, so Chainlink prices come
// from the aggregator's AnswerUpdated events and ETH/USD from the pool's swaps.
func TestFullNodePricesFromLogs(t *testing.T) {
	f := newFakeChain(t, 20*24*time.Hour)
	f.fullNode = true
	feed, agg, oldAgg := "0x00000000000000000000000000000000000000fe", "0x00000000000000000000000000000000000000a2", "0x00000000000000000000000000000000000000a1"
	f.constCall(feed, selDecimals, ret(wInt(8)))
	f.constCall(feed, selPhaseID, ret(wInt(2)))
	f.calls[feed+"|"+selPhaseAggregator] = nil
	f.calls[feed+"|"+selPhaseAggregator] = func(uint64) (string, bool) { return ret(wAddr(agg)), true } // overwritten per phase below
	f.constCall(feed, selAggregator, ret(wAddr(agg)))
	f.constCall(feed, selLatestRound, ret(wInt(9), wInt(170e8), wInt(0), wInt(0), wInt(9))) // "latest" still works
	now := f.latest
	day := uint64(864000)
	f.answerUpdated(oldAgg, now-15*day, 120e8) // phase 1 aggregator (not returned by the fake: ignored)
	f.answerUpdated(agg, now-10*day, 150e8)
	f.answerUpdated(agg, now-9*day, 155e8)
	// … weekend: no updates for 3 days …
	f.answerUpdated(agg, now-6*day, 160e8)

	o := testOnchain(t, f, nil)
	ctx := context.Background()
	for _, c := range []struct {
		block uint64
		want  float64
	}{
		{now - 10*day + 5, 150},   // right after an update
		{now - 9*day + 100, 155},  //
		{now - 6*day - 1000, 155}, // across the weekend gap: Friday's price
		{now - day, 160},
	} {
		got, err := o.chainlink(ctx, feed, c.block)
		if err != nil || got != c.want {
			t.Errorf("chainlink at %d = %v (%v), want %v", c.block, got, err, c.want)
		}
	}
	if !o.noState.Load() {
		t.Fatal("full node not detected")
	}
	if got, err := o.chainlink(ctx, feed, 0); err != nil || got != 170 {
		t.Fatalf("latest price should still come from the contract: %v %v", got, err)
	}
	if _, err := o.chainlink(ctx, feed, now-12*day); err == nil {
		t.Fatal("before the first update there is no price")
	}
	// once detected, no more historical eth_call attempts
	before := f.count["eth_call"]
	o.chainlink(ctx, feed, now-day)
	if d := f.count["eth_call"] - before; d != 0 {
		t.Fatalf("still tried eth_call on a full node (%d calls)", d)
	}

	// ETH/USD from the WETH/USDG pool's swap events
	pool := "0x69bfaf19c9f377bb306a89aed9f6b07e2c1a8d9a"
	f.addToken(tWETH, 18, "WETH")
	f.addToken(tUSDG, 6, "USDG")
	f.constCall(pool, selToken0, ret(wAddr(tWETH)))
	f.constCall(pool, selToken1, ret(wAddr(tUSDG)))
	f.swapV3(pool, now-5*day, "0xe1", sqrtX96(3000e6/1e18))
	f.swapV3(pool, now-2*day, "0xe2", sqrtX96(3300e6/1e18))
	if p, err := o.ethFromPool(ctx, now-3*day); err != nil || math.Abs(p-3000) > 1e-6 {
		t.Fatalf("ETH at -3d: %v %v", p, err)
	}
	if p, ok, err := o.quoteUSD(ctx, tWETH, now-day); err != nil || !ok || math.Abs(p-3300) > 1e-6 {
		t.Fatalf("WETH quote at -1d: %v %v %v", p, ok, err)
	}
	if p, ok, err := o.quoteUSD(ctx, zeroAddr, now-day); err != nil || !ok || math.Abs(p-3300) > 1e-6 {
		t.Fatalf("native ETH quote: %v %v %v", p, ok, err)
	}
}

func TestLastLogBeforeHandlesRangeCaps(t *testing.T) {
	f := newFakeChain(t, 20*24*time.Hour)
	f.maxRange = 3000
	agg := "0x00000000000000000000000000000000000000a2"
	f.answerUpdated(agg, f.latest-500_000, 42e8)
	o := testOnchain(t, f, map[string]string{"SCOUT_PRICE_LOOKBACK_BLOCKS": "1000000"})
	l, err := o.lastLogBefore(context.Background(), agg, []any{topicAnswerUpdated}, f.latest)
	if err != nil || l == nil || l.block() != f.latest-500_000 {
		t.Fatalf("got %+v %v", l, err)
	}
	if l, err := o.lastLogBefore(context.Background(), agg, []any{topicAnswerUpdated}, f.latest-600_000); err != nil || l != nil {
		t.Fatalf("nothing earlier: %+v %v", l, err)
	}
}

// newFakeMainnet: an Ethereum-like archive chain (12-second blocks) with the
// ETH/USD Chainlink feed; price(t) decides the answer at a block's time.
func newFakeMainnet(t *testing.T, price func(ts int64) float64) *fakeChain {
	m := newFakeChain(t, 400*24*time.Hour)
	m.timeOf = func(n uint64) int64 { return m.t0 + int64(n-1)*12 }
	m.latest = uint64(time.Now().Unix()-m.t0)/12 + 1
	m.constCall(mainnetEthUsdFeed, selDecimals, ret(wInt(8)))
	m.calls[mainnetEthUsdFeed+"|"+selLatestRound] = func(b uint64) (string, bool) {
		return ret(wInt(1), wInt(int64(price(m.timeOf(b))*1e8)), wInt(0), wInt(0), wInt(1)), true
	}
	return m
}

// ETH/USD from Chainlink on an Ethereum mainnet archive node: Robinhood block →
// timestamp → mainnet block at that time → latestRoundData().
func TestEthPriceFromMainnetArchive(t *testing.T) {
	f := newFakeChain(t, 30*24*time.Hour) // Robinhood Chain: full node, no ETH feed, no pool
	f.fullNode = true
	change := time.Now().Add(-5 * 24 * time.Hour).Unix()
	m := newFakeMainnet(t, func(ts int64) float64 {
		if ts < change {
			return 3000
		}
		return 3300
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_MAINNET_RPC_URL": m.srv.URL, "SCOUT_MAINNET_RPC_RPS": "100000", "SCOUT_ETH_USD_POOL": "0x00000000000000000000000000000000000000ee"})
	ctx := context.Background()
	day := uint64(864000)
	for _, c := range []struct {
		block uint64
		want  float64
	}{
		{f.latest - 10*day, 3000},
		{f.blockAtTime(time.Unix(change-30, 0)), 3000}, // 30s before the change
		{f.blockAtTime(time.Unix(change+30, 0)), 3300}, // 30s after
		{f.latest - day, 3300},
		{0, 3300}, // latest
	} {
		for _, quote := range []string{tWETH, zeroAddr} {
			got, ok, err := o.quoteUSD(ctx, quote, c.block)
			if err != nil || !ok || got != c.want {
				t.Errorf("quoteUSD(%s, %d) = %v %v %v, want %v", quote[:6], c.block, got, ok, err, c.want)
			}
		}
	}
	if o.quoteSource(tWETH) != "Chainlink on Ethereum mainnet" {
		t.Errorf("source = %s", o.quoteSource(tWETH))
	}
	// the Robinhood node was never asked for state at an old block
	if f.count["eth_call"] != 0 {
		t.Errorf("robinhood node got %d eth_call(s)", f.count["eth_call"])
	}
	// stablecoins stay $1; an unmapped stock token still has no USD source
	if q, ok, _ := o.quoteUSD(ctx, tUSDG, f.latest-day); !ok || q != 1 {
		t.Errorf("USDG %v %v", q, ok)
	}
	if _, ok, err := o.quoteUSD(ctx, "0x1111111111111111111111111111111111111111", f.latest-day); ok || err != nil {
		t.Errorf("unmapped token: ok=%v err=%v", ok, err)
	}
	// a mainnet feed can be mapped to any quote asset
	o2 := testOnchain(t, f, map[string]string{"SCOUT_MAINNET_RPC_URL": m.srv.URL,
		"SCOUT_MAINNET_CHAINLINK_FEEDS": "0x1111111111111111111111111111111111111111=" + mainnetEthUsdFeed})
	if q, ok, err := o2.quoteUSD(ctx, "0x1111111111111111111111111111111111111111", f.latest-day); err != nil || !ok || q != 3300 {
		t.Errorf("mapped mainnet feed: %v %v %v", q, ok, err)
	}
	if o2.cfg.MainnetFeeds["eth"] != mainnetEthUsdFeed {
		t.Error("default ETH/USD mainnet feed lost when adding another mapping")
	}
	// without SCOUT_MAINNET_RPC_URL nothing changes (falls back to the pool / Robinhood feeds)
	o3 := testOnchain(t, f, map[string]string{"SCOUT_MAINNET_RPC_URL": ""})
	if o3.mainnet != nil || o3.quoteSource(tWETH) != "WETH/USDG pool" {
		t.Errorf("mainnet should be off: %v %s", o3.mainnet != nil, o3.quoteSource(tWETH))
	}
}

// The Robinhood block number must be converted to the Ethereum block number at
// the same time; the two chains' block numbers are unrelated.
func TestRobinhoodBlockConvertedToMainnetBlock(t *testing.T) {
	f := newFakeChain(t, 30*24*time.Hour)
	f.fullNode = true
	m := newFakeMainnet(t, func(int64) float64 { return 3000 })
	o := testOnchain(t, f, map[string]string{"SCOUT_MAINNET_RPC_URL": m.srv.URL, "SCOUT_MAINNET_RPC_RPS": "100000"})
	ctx := context.Background()

	for _, ago := range []time.Duration{20 * 24 * time.Hour, 3 * 24 * time.Hour, 90 * time.Minute} {
		rh := f.blockAtTime(time.Now().Add(-ago))
		rhTime := f.timeOf(rh)
		mb, gotRT, gotMT, err := o.mainnetBlockFor(ctx, rh)
		if err != nil {
			t.Fatal(err)
		}
		// expected: the last mainnet block with timestamp <= the Robinhood block's time
		want := uint64((rhTime-m.t0)/12) + 1
		if mb != want || gotRT != rhTime || gotMT != m.timeOf(want) {
			t.Fatalf("Robinhood block %d (t=%d) → mainnet %d (t=%d), want %d (t=%d)", rh, rhTime, mb, gotMT, want, m.timeOf(want))
		}
		if gotMT > rhTime || rhTime-gotMT >= 12 {
			t.Fatalf("mainnet block time %d not within one block before %d", gotMT, rhTime)
		}
		if m.timeOf(mb+1) <= rhTime {
			t.Fatalf("a later mainnet block (%d) is still not after the Robinhood block's time", mb+1)
		}
		if mb == rh {
			t.Fatalf("mainnet block equals the Robinhood block number (%d): not converted", rh)
		}
		// the feed is read at exactly that mainnet block, never at the Robinhood number
		m.mu.Lock()
		m.callTags = nil
		m.mu.Unlock()
		if _, err := o.mainnetFeed(ctx, mainnetEthUsdFeed, rh); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		tags := append([]string(nil), m.callTags...)
		m.mu.Unlock()
		sawRound := false
		for _, tag := range tags {
			if tag == hexU64(rh) {
				t.Fatalf("mainnet node was asked for Robinhood block number %d", rh)
			}
			if tag == hexU64(want) {
				sawRound = true
			}
		}
		if !sawRound {
			t.Fatalf("feed not read at mainnet block %d; tags: %v", want, tags)
		}
	}
	// "latest" stays "latest"
	if mb, _, _, err := o.mainnetBlockFor(ctx, 0); err != nil || mb != 0 {
		t.Fatalf("latest: %d %v", mb, err)
	}
	// a mainnet node that is hours behind is reported, not silently used
	stale := newFakeMainnet(t, func(int64) float64 { return 3000 })
	stale.latest -= 3 * 3600 / 12 // 3 hours behind
	o2 := testOnchain(t, f, map[string]string{"SCOUT_MAINNET_RPC_URL": stale.srv.URL, "SCOUT_MAINNET_RPC_RPS": "100000"})
	if _, _, _, err := o2.mainnetBlockFor(ctx, f.latest-100); err == nil || !strings.Contains(err.Error(), "behind") {
		t.Fatalf("stale mainnet node not detected: %v", err)
	}
	if _, _, _, err := o2.mainnetBlockFor(ctx, f.blockAtTime(time.Now().Add(-5*time.Hour))); err != nil {
		t.Fatalf("a block older than the stale node's tip should still convert: %v", err)
	}
}

// slowLogsNode imitates a Nitro / geth node on blocks outside its log index:
// eth_getLogs below indexedFrom costs perBlock for every block in the range,
// and a range that would take longer than limit is answered with the node's
// own JSON-RPC error "request timed out" after limit. From indexedFrom on
// (indexed blocks) every range is answered at once. Times are scaled down:
// 1 µs per block and a 30 ms limit stand for ~1 ms and ~30 s on the real node.
func slowLogsNode(rl *rangeLog, indexedFrom uint64, perBlock, limit time.Duration) func(addr string, from, to uint64) string {
	return func(addr string, from, to uint64) string {
		if from >= indexedFrom {
			rl.add(rangeAsk{addr: addr, from: from, to: to})
			return ""
		}
		cost := time.Duration(to-from+1) * perBlock
		if cost > limit {
			rl.add(rangeAsk{addr, from, to, true})
			time.Sleep(limit)
			return "request timed out"
		}
		rl.add(rangeAsk{addr: addr, from: from, to: to})
		time.Sleep(cost)
		return ""
	}
}

// (a) A node that times out on ranges above 30000 blocks: each size is asked
// twice before it is split, the scan settles at 25000 blocks (the largest
// halving of SCOUT_RPC_LOG_CHUNK that works), and a grown size that times out
// is asked once, not twice, and tried ever more rarely.
func TestGetLogsNodeTimeoutSplitsAndSettles(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(5000); b < 1_500_000; b += 50_001 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	var rl rangeLog
	f.setLogsHook(slowLogsNode(&rl, math.MaxUint64, time.Microsecond, 30*time.Millisecond))
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000", "SCOUT_RPC_PARALLEL": "1"})
	ctx := withScanProgress(context.Background(), "call 2 [1/1]", 0)
	var got []uint64
	if err := o.rpc.getLogsChunked(ctx, pool, []any{topicSwapV3}, 0, 1_499_999, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %d logs, want %d (in block order)", len(got), len(want))
	}
	asks := rl.list(pool)
	// The way down: 200000, 100000 and 50000 blocks, each asked twice.
	if len(asks) < 6 {
		t.Fatalf("%d requests", len(asks))
	}
	for i, n := range []uint64{200_000, 200_000, 100_000, 100_000, 50_000, 50_000} {
		if a := asks[i]; !a.refused || a.from != 0 || a.to-a.from+1 != n {
			t.Fatalf("request %d: %+v, want blocks 0-%d timed out", i+1, a, n-1)
		}
	}
	answered, probes := 0, map[uint64]int{}
	for _, a := range asks[6:] {
		n := a.to - a.from + 1
		switch {
		case !a.refused && n == 25_000:
			answered++
		case a.refused && n == 50_000:
			probes[a.from]++
		default:
			t.Fatalf("after settling: asked for %d-%d (%d blocks, timed out %v)", a.from, a.to, n, a.refused)
		}
	}
	if answered != 60 {
		t.Fatalf("%d ranges of 25000 blocks answered, want 60", answered)
	}
	// Grow-back tries after 3, 6, 12 and 24 answered ranges: 4 in 60 ranges, each asked once.
	if len(probes) < 1 || len(probes) > 5 {
		t.Fatalf("%d grow-back tries: %v", len(probes), probes)
	}
	for from, n := range probes {
		if n != 1 {
			t.Fatalf("grow-back try at block %d asked %d times", from, n)
		}
	}
	out := buf.String()
	for _, w := range []string{
		"call 2 [1/1]: eth_getLogs blocks 0-199999 (200000 blocks) timed out on the node (2 tries) (rpc error -32000: request timed out); this scan continues with 100000-block ranges (max 200000)",
		"call 2 [1/1]: eth_getLogs blocks 0-49999 (50000 blocks) timed out on the node (2 tries) (rpc error -32000: request timed out); this scan continues with 25000-block ranges (max 200000)",
		"call 2 [1/1]: eth_getLogs ranges back up to 50000 blocks (max 200000) after 3 answered ranges",
		"(50000 blocks) timed out on the node (rpc error -32000: request timed out); this scan continues with 25000-block ranges",
		"call 2 [1/1]: eth_getLogs ranges back up to 50000 blocks (max 200000) after 6 answered ranges",
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("log is missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "refused as too large") {
		t.Fatalf("a time-out was reported as too large:\n%s", out)
	}
}

// (a) The same with a node that never answers big ranges at all (no JSON-RPC
// error, the client's own time limit runs out): once call()'s retries are used
// up, the range is split instead of failing the scan.
func TestGetLogsClientTimeoutSplits(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(500); b < 64_000; b += 3001 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	var rl rangeLog
	f.setLogsHook(func(addr string, from, to uint64) string {
		slow := to-from+1 > 4000
		rl.add(rangeAsk{addr, from, to, slow})
		if slow {
			time.Sleep(200 * time.Millisecond)
		}
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "16000", "SCOUT_RPC_PARALLEL": "1"})
	o.rpc.http.Timeout = 40 * time.Millisecond
	var got []uint64
	if err := o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 0, 63_999, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	asks := rl.list(pool)
	// 16000 and 8000 blocks: each asked 4 times by call(), then split.
	for i := 0; i < 8; i++ {
		n := uint64(16_000)
		if i >= 4 {
			n = 8000
		}
		if a := asks[i]; a.from != 0 || a.to-a.from+1 != n {
			t.Fatalf("request %d: %+v", i+1, a)
		}
	}
	for _, a := range asks[8:] {
		if n := a.to - a.from + 1; !a.refused && n != 4000 {
			t.Fatalf("answered a %d-block range", n)
		}
	}
	if !strings.Contains(buf.String(), "eth_getLogs blocks 0-15999 (16000 blocks) got no answer in time (client time-out)") {
		t.Fatalf("log:\n%s", buf.String())
	}
}

// (b) Old blocks the node reads slowly (outside its log index), then indexed
// blocks it answers at once: the scan goes small over the slow stretch and is
// back at SCOUT_RPC_LOG_CHUNK soon after it, without waiting out the long
// grow-back pause the slow stretch built up.
func TestGetLogsSlowStretchThenIndexed(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var want []uint64
	for b := uint64(5000); b < 6_200_000; b += 77_777 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	const indexedFrom = 3_000_000 // long enough for the grow-back pause to reach 96 ranges
	var rl rangeLog
	f.setLogsHook(slowLogsNode(&rl, indexedFrom, time.Microsecond, 30*time.Millisecond))
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000"}) // 8 ranges at a time
	var got []uint64
	if err := o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 0, 6_199_999, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %d logs, want %d (in block order)", len(got), len(want))
	}
	asks := rl.list(pool)
	for _, a := range asks {
		n := a.to - a.from + 1
		if a.from < indexedFrom && !a.refused && n > 30_000 {
			t.Fatalf("slow stretch answered %d blocks", n)
		}
		if a.from >= indexedFrom+1_800_000 && n != 200_000 {
			t.Fatalf("indexed blocks: asked for %d-%d (%d blocks), want %d", a.from, a.to, n, 200_000)
		}
	}
	if !strings.Contains(buf.String(), "ranges back up to 200000 blocks (max 200000)") {
		t.Fatalf("log:\n%s", buf.String())
	}
	t.Logf("%d requests for 6.2M blocks", len(asks))
}

// (d) Ranges are not split below the smallest size: a range of that size that
// still times out fails the scan, with a log line that says so.
func TestGetLogsTimeoutAtSmallestRange(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c1"
	var rl rangeLog
	f.setLogsHook(func(addr string, from, to uint64) string {
		rl.add(rangeAsk{addr, from, to, true})
		return "request timed out"
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "800", "SCOUT_RPC_PARALLEL": "1"})
	if o.rpc.minChunk != 200 {
		t.Fatalf("smallest range %d", o.rpc.minChunk)
	}
	ctx := withScanProgress(context.Background(), "call 3 [1/1]", time.Hour)
	err := o.rpc.getLogsChunked(ctx, pool, []any{topicSwapV3}, 0, 9999, func(rpcLog) {})
	if err == nil || !strings.Contains(err.Error(), "eth_getLogs 0-199: rpc error -32000: request timed out") {
		t.Fatalf("want the time-out at blocks 0-199, got %v", err)
	}
	var sizes []uint64
	for _, a := range rl.list(pool) {
		sizes = append(sizes, a.to-a.from+1)
	}
	if fmt.Sprint(sizes) != "[800 800 400 400 200 200]" {
		t.Fatalf("asked for %v", sizes)
	}
	if w := "call 3 [1/1]: eth_getLogs blocks 0-199 (200 blocks) timed out on the node (2 tries) at the smallest range size (200 blocks); this scan stops here (rpc error -32000: request timed out)"; !strings.Contains(buf.String(), w) {
		t.Fatalf("log is missing %q:\n%s", w, buf.String())
	}
}
