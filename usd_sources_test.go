package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of quoteUSD's sources (onchain.go / onchain_extra.go): their order,
// the asset's own pools chosen per block, what is cached and for how long,
// and Chainlink aggregator switches.

const (
	uFeedETH = "0x00000000000000000000000000000000000000fe" // ETH/USD on the fake Robinhood chain: $3000
	uBuyer   = "0x00000000000000000000000000000000000000d1"
)

// usdChain: a fake archive chain with WETH, USDG and an ETH/USD feed at $3000.
func usdChain(t *testing.T) *fakeChain {
	t.Helper()
	f := newFakeChain(t, 30*24*time.Hour)
	f.addToken(tWETH, 18, "WETH")
	f.addToken(tUSDG, 6, "USDG")
	f.constCall(uFeedETH, selDecimals, ret(wInt(8)))
	f.constCall(uFeedETH, selLatestRound, ret(wInt(1), wInt(3000e8), wInt(0), wInt(0), wInt(1)))
	return f
}

// usdTestPool is a v3 pool asset / other on a fake chain (the asset is token0).
type usdTestPool struct {
	f                  *fakeChain
	addr, asset, other string
	assetDec, otherDec int
	n                  int
}

func newUSDTestPool(f *fakeChain, addr, asset, other string, assetDec, otherDec int) *usdTestPool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.constCall(addr, selToken0, ret(wAddr(asset)))
	f.constCall(addr, selToken1, ret(wAddr(other)))
	f.constCall(addr, selSlot0, ret(wInt(1)))
	return &usdTestPool{f: f, addr: addr, asset: asset, other: other, assetDec: assetDec, otherDec: otherDec}
}

// trade: a swap leaving the asset at price (in the other asset), and a
// transfer of the asset in the same transaction (what discovery sees).
func (p *usdTestPool) trade(block uint64, price float64) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.n++
	tx := fmt.Sprintf("0x%s%06d", p.addr[len(p.addr)-6:], p.n)
	raw := price * pow10(p.otherDec) / pow10(p.assetDec)
	p.f.swapV3(p.addr, block, tx, sqrtX96(raw))
	p.f.transfer(p.asset, p.addr, uBuyer, block, tx)
}

// fakeClock is a settable clock for the cache expiries.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func near6(got, want float64) bool { return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want)) }

func TestQuoteUSDSourceOrder(t *testing.T) {
	const (
		nvda   = "0x1111111111111111111111111111111111111111"
		asset  = "0x2222222222222222222222222222222222222222"
		rhNVDA = "0x00000000000000000000000000000000000000f1"
		mnNVDA = "0x00000000000000000000000000000000000000f2"
		wpool  = "0x00000000000000000000000000000000000000a1"
		upool  = "0x00000000000000000000000000000000000000a2"
	)
	type setup struct {
		rhFeed, mainnetFeed bool // NVDA feeds configured
		rhBusy              bool // the Robinhood NVDA feed answers "rate limit"
		wethPool, usdgPool  bool // the asset's own pools (by default the USDG pool traded last)
		wethLast, sameBlock bool // the WETH pool traded last / both in the same block
	}
	cases := []struct {
		name    string
		quote   string
		s       setup
		want    float64
		wantOK  bool
		wantErr bool
		wantSrc string
	}{
		{name: "stablecoin", quote: tUSDG, want: 1, wantOK: true, wantSrc: "stablecoin = $1"},
		{name: "Robinhood feed before the mainnet feed", quote: nvda, s: setup{rhFeed: true, mainnetFeed: true},
			want: 150, wantOK: true, wantSrc: "Chainlink on Robinhood Chain"},
		{name: "mainnet feed", quote: nvda, s: setup{mainnetFeed: true}, want: 777, wantOK: true, wantSrc: "Chainlink on Ethereum mainnet"},
		{name: "a busy Robinhood feed is an error, not the next source", quote: nvda, s: setup{rhFeed: true, mainnetFeed: true, rhBusy: true},
			wantErr: true, wantSrc: "Chainlink on Robinhood Chain"},
		{name: "own pools: the stablecoin pool traded last", quote: asset, s: setup{wethPool: true, usdgPool: true},
			want: 2.5, wantOK: true, wantSrc: "its USDG pool (uniswap-v3)"},
		{name: "own pools: the WETH pool traded last", quote: asset, s: setup{wethPool: true, usdgPool: true, wethLast: true},
			want: 3, wantOK: true, wantSrc: "its WETH pool (uniswap-v3)"},
		{name: "own pools: same block, the WETH pool first", quote: asset, s: setup{wethPool: true, usdgPool: true, sameBlock: true},
			want: 3, wantOK: true, wantSrc: "its WETH pool (uniswap-v3)"},
		{name: "own stablecoin pool", quote: asset, s: setup{usdgPool: true}, want: 2.5, wantOK: true, wantSrc: "its USDG pool (uniswap-v3)"},
		{name: "no source at all", quote: asset, wantSrc: "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := usdChain(t)
			f.addToken(nvda, 18, "NVDA")
			f.addToken(asset, 18, "ASSET")
			f.constCall(rhNVDA, selDecimals, ret(wInt(8)))
			f.constCall(rhNVDA, selLatestRound, ret(wInt(1), wInt(150e8), wInt(0), wInt(0), wInt(1)))
			if c.s.rhBusy {
				f.callErr = func(to, sel string, _ uint64) string {
					if to == rhNVDA && sel == selLatestRound {
						return "rate limit exceeded"
					}
					return ""
				}
			}
			m := newFakeMainnet(t, func(int64) float64 { return 3000 })
			m.constCall(mnNVDA, selDecimals, ret(wInt(8)))
			m.constCall(mnNVDA, selLatestRound, ret(wInt(1), wInt(777e8), wInt(0), wInt(0), wInt(1)))
			b := f.latest - 864000
			wAt, uAt := b-5000, b-100
			if c.s.wethLast {
				wAt, uAt = uAt, wAt
			}
			if c.s.sameBlock {
				wAt = uAt
			}
			if c.s.wethPool {
				newUSDTestPool(f, wpool, asset, tWETH, 18, 18).trade(wAt, 0.001) // $3
			}
			if c.s.usdgPool {
				newUSDTestPool(f, upool, asset, tUSDG, 18, 6).trade(uAt, 2.5)
			}
			env := map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH, "SCOUT_MAINNET_RPC_URL": m.srv.URL,
				"SCOUT_MAINNET_RPC_RPS": "100000", "SCOUT_MAINNET_CHAINLINK_FEEDS": ""}
			if c.s.rhFeed {
				env["SCOUT_CHAINLINK_FEEDS"] += "," + nvda + "=" + rhNVDA
			}
			if c.s.mainnetFeed {
				env["SCOUT_MAINNET_CHAINLINK_FEEDS"] = nvda + "=" + mnNVDA
			}
			o := testOnchain(t, f, env)
			got, ok, err := o.quoteUSD(context.Background(), c.quote, b)
			if (err != nil) != c.wantErr || ok != c.wantOK || (ok && !near6(got, c.want)) {
				t.Fatalf("quoteUSD(%s, %d): got %v %v %v, want %v ok=%v error=%v", c.quote, b, got, ok, err, c.want, c.wantOK, c.wantErr)
			}
			if src := o.quoteSource(c.quote); src != c.wantSrc {
				t.Fatalf("quoteSource(%s): got %q, want %q", c.quote, src, c.wantSrc)
			}
		})
	}
}

// An error of the node while looking for an asset's own pools is returned and
// never kept as "no pool": the next lookup asks again and finds the pool.
func TestQuoteViaPoolsNodeErrorNotCached(t *testing.T) {
	old := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = old })
	const asset, pool = "0x2222222222222222222222222222222222222222", "0x00000000000000000000000000000000000000a2"
	for _, c := range []struct {
		name string
		busy func(f *fakeChain, on *bool)
	}{
		{"decimals() rate limited", func(f *fakeChain, on *bool) {
			f.callErr = func(to, sel string, _ uint64) string {
				if *on && to == asset && sel == selDecimals {
					return "rate limit exceeded"
				}
				return ""
			}
		}},
		{"transfer logs rate limited", func(f *fakeChain, on *bool) {
			f.setLogsHook(func(addr string, _, _ uint64) string {
				if *on && addr == asset {
					return "rate limit exceeded"
				}
				return ""
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := usdChain(t)
			f.addToken(asset, 18, "ASSET")
			b := f.latest - 864000
			newUSDTestPool(f, pool, asset, tUSDG, 18, 6).trade(b-100, 2.5)
			on := true
			c.busy(f, &on)
			o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH})
			ctx := context.Background()
			if p, ok, err := o.quoteUSD(ctx, asset, b); err == nil || ok || errors.Is(err, errNoUSDSource) {
				t.Fatalf("busy node: got %v %v %v, want a (temporary) error", p, ok, err)
			}
			o.mu.Lock()
			set := o.usdPools[asset]
			cached := set != nil && (!set.noPoolUntil.IsZero() || len(set.pools) > 0 || len(set.noPrice) > 0 ||
				set.histThrough > 0 || len(set.histSeen) > 0 || len(set.histPending) > 0)
			o.mu.Unlock()
			if cached {
				t.Fatalf("busy node: the failure was cached: %+v", set)
			}
			on = false
			if p, ok, err := o.quoteUSD(ctx, asset, b); err != nil || !ok || !near6(p, 2.5) {
				t.Fatalf("node back: got %v %v %v, want $2.5", p, ok, err)
			}
		})
	}
}

// "No pool anywhere" is kept for an hour, then asked again of the new blocks
// only; "no price yet" (pools, but no trade before the block) is kept for six
// hours at that block; then they are asked again. Found per-hour prices are
// kept.
func TestQuoteViaPoolsNegativeCacheExpiry(t *testing.T) {
	const (
		noPools = "0x3333333333333333333333333333333333333333"
		late    = "0x5555555555555555555555555555555555555555"
		poolA   = "0x00000000000000000000000000000000000000a3"
		poolB   = "0x00000000000000000000000000000000000000a5"
	)
	f := usdChain(t)
	f.addToken(noPools, 18, "NOPOOL")
	f.addToken(late, 18, "LATE")
	clk := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH})
	o.now = clk.now
	ctx := context.Background()
	b := f.latest - 864000
	requests := func() int64 { return o.rpc.requests.Load() }

	// no pool anywhere: definitive, kept for an hour
	if p, ok, err := o.quoteUSD(ctx, noPools, b); ok || err != nil {
		t.Fatalf("no pool: got %v %v %v, want ok=false and no error", p, ok, err)
	}
	// a pool created afterwards (in blocks the search had not read)
	noHeadCache(t) // the head moves on below
	f.mu.Lock()
	oldHead := f.latest
	f.latest += 20_000
	f.mu.Unlock()
	pa := newUSDTestPool(f, poolA, noPools, tUSDG, 18, 6)
	pa.trade(oldHead+100, 4)
	before := requests()
	if _, ok, err := o.quoteUSD(ctx, noPools, b); ok || err != nil || requests() != before {
		t.Fatalf("no pool within the hour: got ok=%v err=%v and %d request(s), want the cached answer and none", ok, err, requests()-before)
	}
	clk.add(61 * time.Minute)
	f.mu.Lock()
	asked := len(f.queries)
	f.mu.Unlock()
	if _, ok, err := o.quoteUSD(ctx, noPools, b); ok || !errors.Is(err, errNoPriceYet) {
		t.Fatalf("no pool after an hour, at block %d (before the new pool's first trade): got ok=%v %v, want errNoPriceYet", b, ok, err)
	}
	f.mu.Lock()
	for _, q := range f.queries[asked:] {
		if q.addr == noPools && q.from < oldHead-1000 {
			t.Errorf("after the hour the asset's transfers were read from block %d, want only the blocks after %d", q.from, oldHead-1000)
		}
	}
	f.mu.Unlock()
	if p, ok, err := o.quoteUSD(ctx, noPools, oldHead+200); err != nil || !ok || !near6(p, 4) {
		t.Fatalf("after the new pool's trade: got %v %v %v, want the new pool's $4", p, ok, err)
	}

	// no price yet: the pool's first trade is after the block
	pb := newUSDTestPool(f, poolB, late, tUSDG, 18, 6)
	pb.trade(b+500, 7)
	if _, _, err := o.quoteUSD(ctx, late, b); !errors.Is(err, errNoPriceYet) {
		t.Fatalf("before the first trade: got %v, want errNoPriceYet", err)
	}
	before = requests()
	clk.add(5 * time.Hour)
	if _, _, err := o.quoteUSD(ctx, late, b); !errors.Is(err, errNoPriceYet) || requests() != before {
		t.Fatalf("no price yet within 6h: got %v and %d request(s), want the cached errNoPriceYet and none", err, requests()-before)
	}
	if p, ok, err := o.quoteUSD(ctx, late, b+600); err != nil || !ok || !near6(p, 7) {
		t.Fatalf("a later block: got %v %v %v, want $7 (only the block asked is cached)", p, ok, err)
	}
	pb.trade(b-20, 6) // a trade before the block turns up (e.g. the node caught up)
	clk.add(61 * time.Minute)
	if p, ok, err := o.quoteUSD(ctx, late, b); err != nil || !ok || !near6(p, 6) {
		t.Fatalf("no price yet after 6h: got %v %v %v, want $6", p, ok, err)
	}

	// a found price at an hour boundary is kept
	hour := (f.timeOf(b+600)/3600 + 1) * 3600 // after the $7 trade
	q1, err := o.quoteUSDHour(ctx, late, hour)
	if err != nil {
		t.Fatal(err)
	}
	before = requests()
	clk.add(48 * time.Hour)
	if q2, err := o.quoteUSDHour(ctx, late, hour); err != nil || q2 != q1 || requests() != before {
		t.Fatalf("hour price again: got %v %v and %d request(s), want %v from the cache", q2, err, requests()-before, q1)
	}
}

// The pool is chosen per block: the one whose last trade at or before the
// block is the most recent, whatever its other side. SNDK:
// a USDG pool that only starts trading at block N no longer fails the blocks
// before N ("no trades in the USDG pool … before block N"); a source change is
// logged once.
func TestQuoteViaPoolsPerBlock(t *testing.T) {
	const (
		sndk  = "0x6666666666666666666666666666666666666666"
		mix   = "0x7777777777777777777777777777777777777777"
		newer = "0x00000000000000000000000000000000000000b1" // SNDK / USDG, trades from N on
		older = "0x00000000000000000000000000000000000000b2" // SNDK / USDG, trades before N only
		mixU  = "0x00000000000000000000000000000000000000b3" // MIX / USDG, before N
		mixW  = "0x00000000000000000000000000000000000000b4" // MIX / WETH, from N on
	)
	f := usdChain(t)
	f.addToken(sndk, 18, "SNDK")
	f.addToken(mix, 18, "MIX")
	logs := captureLog(t)
	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH})
	ctx := context.Background()
	n := f.latest - 864000
	pn, po := newUSDTestPool(f, newer, sndk, tUSDG, 18, 6), newUSDTestPool(f, older, sndk, tUSDG, 18, 6)
	po.trade(n-3000, 9)
	po.trade(n-500, 9.5)
	pn.trade(n, 10)
	pn.trade(n+2000, 11)
	pu, pw := newUSDTestPool(f, mixU, mix, tUSDG, 18, 6), newUSDTestPool(f, mixW, mix, tWETH, 18, 18)
	pu.trade(n-400, 2)
	pw.trade(n+100, 0.001) // $3
	pu.trade(n+200, 2.2)   // the most recent: the USDG pool again
	for _, c := range []struct {
		asset string
		block uint64
		want  float64
		src   string
	}{
		{sndk, n - 100, 9.5, "its USDG pool (uniswap-v3) " + older},
		{sndk, n + 100, 10, "its USDG pool (uniswap-v3) " + newer},
		{sndk, n + 3000, 11, "its USDG pool (uniswap-v3) " + newer},
		{sndk, n - 1000, 9, "its USDG pool (uniswap-v3) " + older},
		{mix, n - 100, 2, "its USDG pool (uniswap-v3) " + mixU},
		{mix, n + 150, 3, "its WETH pool (uniswap-v3) " + mixW},
		{mix, n + 300, 2.2, "its USDG pool (uniswap-v3) " + mixU},
	} {
		p, ok, err := o.quoteUSD(ctx, c.asset, c.block)
		if err != nil || !ok || !near6(p, c.want) {
			t.Fatalf("quoteUSD(%s, N%+d): got %v %v %v, want %v", c.asset[:6], int64(c.block)-int64(n), p, ok, err, c.want)
		}
		o.mu.Lock()
		got := o.usdPools[c.asset].sourceRef
		o.mu.Unlock()
		if got != c.src {
			t.Fatalf("source of %s at N%+d: got %q, want %q", c.asset[:6], int64(c.block)-int64(n), got, c.src)
		}
	}
	out := logs.String()
	if k := strings.Count(out, "USD price of SNDK"); k != 3 { // first use, then 2 changes (the third lookup keeps the pool)
		t.Fatalf("got %d source lines for SNDK, want 3 (first + 2 changes):\n%s", k, out)
	}
	if !strings.Contains(out, "now from its USDG pool (uniswap-v3) "+newer+" (was its USDG pool (uniswap-v3) "+older+")") {
		t.Fatalf("no change line for SNDK:\n%s", out)
	}
}

// A Chainlink proxy that switched aggregators, on a node without historical
// state: each block is read from the aggregator in use then, even when the
// old aggregator keeps reporting after the switch. With AggregatorConfirmed
// events (v0.7+ proxies) the switch block decides; without them, the newest
// phase that has reported. A switch seen on a later read is logged.
func TestChainlinkAggregatorSwitch(t *testing.T) {
	const (
		proxy  = "0x00000000000000000000000000000000000000f9"
		aggOld = "0x00000000000000000000000000000000000000e1"
		aggNew = "0x00000000000000000000000000000000000000e2"
		aggX   = "0x00000000000000000000000000000000000000e3"
	)
	day := uint64(864000)
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("AggregatorConfirmed=%v", confirmed), func(t *testing.T) {
			f := newFakeChain(t, 20*24*time.Hour)
			f.fullNode = true
			now := f.latest
			current := aggNew
			f.constCall(proxy, selDecimals, ret(wInt(8)))
			f.constCall(proxy, selPhaseID, ret(wInt(2)))
			f.calls[proxy+"|"+selAggregator] = func(uint64) (string, bool) { return ret(wAddr(current)), true }
			f.dataCalls = map[string]func(string, uint64) (string, bool){
				proxy + "|" + selPhaseAggregator: func(data string, _ uint64) (string, bool) {
					switch data[len(data)-1] {
					case '1':
						return ret(wAddr(aggOld)), true
					case '2':
						return ret(wAddr(aggNew)), true
					}
					return "", false
				}}
			switchAt := now - 6*day
			f.answerUpdated(aggOld, now-10*day, 100e8)
			f.answerUpdated(aggNew, now-7*day, 300e8) // the new aggregator reports before the proxy uses it
			if confirmed {
				f.logs = append(f.logs, fakeLog{addr: proxy, topics: []string{topicAggregatorConfirmed, addrTopic(aggOld), addrTopic(aggNew)},
					data: "0x", block: switchAt, tx: "0xconfirm"})
			}
			f.answerUpdated(aggNew, now-5*day, 200e8)
			f.answerUpdated(aggOld, now-4*day, 105e8) // the old one keeps reporting after the switch
			logs := captureLog(t)
			clk := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
			o := testOnchain(t, f, nil)
			o.now = clk.now
			ctx := context.Background()
			want7d := 100.0 // between the new aggregator's first report and the switch
			if !confirmed {
				want7d = 300 // no switch events: the newest phase that has reported
			}
			for _, c := range []struct {
				block uint64
				want  float64
			}{
				{now - 9*day, 100},
				{now - 7*day + 10, want7d},
				{now - 5*day + 10, 200},
				{now - 3*day, 200}, // the old aggregator's later 105 is not read
			} {
				got, err := o.chainlink(ctx, proxy, c.block)
				if err != nil || got != c.want {
					t.Fatalf("chainlink at now-%.2fd: got %v %v, want %v", float64(now-c.block)/float64(day), got, err, c.want)
				}
			}
			if _, err := o.chainlink(ctx, proxy, now-15*day); !errors.Is(err, errFeedNoData) {
				t.Fatalf("before any report: got %v, want errFeedNoData", err)
			}
			// a third aggregator: seen once the cached list expires
			current = aggX
			if _, err := o.chainlink(ctx, proxy, now-day); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(logs.String(), "switched aggregator") {
				t.Fatalf("switch logged before the cache expired:\n%s", logs.String())
			}
			clk.add(feedInfoTTL + time.Minute)
			if _, err := o.chainlink(ctx, proxy, now-day); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "switched aggregator: "+aggNew+" → "+aggX) {
				t.Fatalf("no switch line:\n%s", logs.String())
			}
		})
	}
}

// GeckoTerminal's reserve_usd counts both sides: half of it is the quote side
// compared with SCOUT_RUG_LIQ_USD.
func TestGeckoRuggedHalvesReserve(t *testing.T) {
	for _, c := range []struct {
		reserve, threshold float64
		want               bool
	}{
		{120, 500, true},
		{800, 500, true}, // $400 a side: rugged (the old check said no)
		{999, 500, true},
		{1000, 500, false},
		{5000, 500, false},
		{0, 0, false}, // threshold off
	} {
		if got := geckoRugged(c.reserve, c.threshold); got != c.want {
			t.Errorf("geckoRugged(%v, %v): got %v, want %v", c.reserve, c.threshold, got, c.want)
		}
	}
}

// clone is a deep copy: changing the copy leaves the original as it was.
func TestOnchainStateClone(t *testing.T) {
	liq := 42.0
	st := &onchainState{ScanBlock: 10, RunMaxU: 2, Done: map[string]bool{"1h": true}, RugLiquidityUSD: &liq}
	c := st.clone()
	c.ScanBlock, c.RunMaxU = 20, 9
	c.Done["1d"] = true
	*c.RugLiquidityUSD = 1
	if st.ScanBlock != 10 || st.RunMaxU != 2 || len(st.Done) != 1 || *st.RugLiquidityUSD != 42 {
		t.Fatalf("original changed through the copy: %+v (rug liq %v)", st, *st.RugLiquidityUSD)
	}
}

// mergeFeeds: database rows add to the environment's; the environment wins.
func TestMergeFeeds(t *testing.T) {
	env := map[string]string{"eth": "0xe", "0xaa": "0x1"}
	db := map[string]string{"0xaa": "0x2", "0xbb": "0x3"}
	got, fromDB, conflicts := mergeFeeds(env, db)
	want := map[string]string{"eth": "0xe", "0xaa": "0x1", "0xbb": "0x3"}
	if fmt.Sprint(got) != fmt.Sprint(want) || fromDB != 1 || conflicts != 1 {
		t.Fatalf("mergeFeeds(%v, %v): got %v (%d from db, %d conflicts), want %v (1, 1)", env, db, got, fromDB, conflicts, want)
	}
	if env["0xbb"] != "" || len(env) != 2 {
		t.Fatalf("env map changed: %v", env)
	}
}

// A counterparty whose token0() reverts is not a pool, even when the revert
// reason reads like a busy node ("rate limit"); a busy node itself is an error.
func TestResolveV2V3RevertReason(t *testing.T) {
	const token, router, pool = "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000d9", "0x00000000000000000000000000000000000000c3"
	for _, c := range []struct {
		name, msg string
		wantErr   bool
	}{
		{"revert with a rate-limit reason", "execution reverted: rate limit", false},
		{"busy node", "rate limit exceeded", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := usdChain(t)
			f.addToken(token, 18, "TKN")
			p := newUSDTestPool(f, pool, token, tWETH, 18, 18)
			eb := f.latest - 864000
			p.trade(eb-10, 0.001)
			for i := uint64(0); i < 3; i++ { // the router is the busiest counterparty
				f.transfer(token, router, uBuyer, eb-20-i, fmt.Sprintf("0xr%d", i))
			}
			f.callErr = func(to, sel string, _ uint64) string {
				if to == router && sel == selToken0 {
					return c.msg
				}
				return ""
			}
			o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH})
			st, err := o.discover(context.Background(), token, eb)
			if c.wantErr {
				if err == nil {
					t.Fatalf("discover with token0() answering %q: got pool %+v, want an error", c.msg, st)
				}
				return
			}
			if err != nil || st.Pool != pool {
				t.Fatalf("discover with token0() answering %q: got %+v %v, want the pool %s", c.msg, st, err, pool)
			}
		})
	}
}

// farPoolChain: a fake chain with an asset FAR whose only USD pool (FAR / USDG)
// trades far from the blocks asked (outside the near-block search's window:
// SCOUT_DISCOVERY_BLOCKS=1000, so 64,000 blocks either side), and wallet
// transfers of FAR right around those blocks. b1 and b2 are two call blocks
// 2,000,000 blocks apart; the pool traded at b1-3,000,000 ($2) and at
// b1+1,000,000 ($3).
type farPoolChain struct {
	f      *fakeChain
	b1, b2 uint64
	env    map[string]string
}

const (
	farAsset  = "0x8888888888888888888888888888888888888888"
	farPool   = "0x00000000000000000000000000000000000000e1"
	farWallet = "0x00000000000000000000000000000000000000d2"
)

func newFarPoolChain(t *testing.T) *farPoolChain {
	t.Helper()
	f := usdChain(t)
	f.addToken(farAsset, 18, "FAR")
	c := &farPoolChain{f: f, b1: f.latest - 6_000_000}
	c.b2 = c.b1 + 2_000_000
	p := newUSDTestPool(f, farPool, farAsset, tUSDG, 18, 6)
	p.trade(c.b1-3_000_000, 2)
	p.trade(c.b1+1_000_000, 3)
	f.mu.Lock()
	for i, b := range []uint64{c.b1 - 10, c.b1 + 10, c.b2 - 10, c.b2 + 10} {
		f.transfer(farAsset, farWallet, uBuyer, b, fmt.Sprintf("0xw%d", i)) // a wallet near each block: no pool
	}
	f.mu.Unlock()
	c.env = map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH, "SCOUT_DISCOVERY_BLOCKS": "1000"}
	return c
}

// The bug the whole-history search fixes: two calls with the same quote asset
// whose only USD pool trades far from the first call's block. The near-block
// search around the first block finds only a wallet; that must not become
// "no USD source" for the asset (the old code kept it for an hour, for every
// block, and the call was stored in quote units for good). Both blocks are
// priced in USD, whichever is asked first.
func TestQuoteViaPoolsFarPool(t *testing.T) {
	for _, order := range []string{"b1 first", "b2 first"} {
		t.Run(order, func(t *testing.T) {
			c := newFarPoolChain(t)
			o := testOnchain(t, c.f, c.env)
			ctx := context.Background()
			asks := []struct {
				block uint64
				want  float64
			}{{c.b1, 2}, {c.b2, 3}}
			if order == "b2 first" {
				asks[0], asks[1] = asks[1], asks[0]
			}
			for _, a := range asks {
				p, ok, err := o.quoteUSD(ctx, farAsset, a.block)
				if err != nil || !ok || !near6(p, a.want) {
					t.Fatalf("quoteUSD(FAR, %d): got %v ok=%v err=%v, want $%v", a.block, p, ok, err, a.want)
				}
			}
			if src := o.quoteSource(farAsset); src != "its USDG pool (uniswap-v3)" {
				t.Fatalf("quoteSource(FAR): got %q, want its USDG pool", src)
			}
		})
	}
}

// A quote asset with no pool against WETH / ETH / a stablecoin anywhere in its
// history (only wallets, a v3 pool and a v4 pool against a token with no USD
// price) is definitively without a USD source: ok=false, no error, kept for an
// hour for every block the search covered (no more requests).
func TestQuoteViaPoolsNoPoolAnywhere(t *testing.T) {
	const (
		meme  = "0x9999999999999999999999999999999999999999" // no USD price
		mpool = "0x00000000000000000000000000000000000000e2" // NOP / MEME, v3
	)
	f := usdChain(t)
	f.addToken(farAsset, 18, "NOP")
	f.addToken(meme, 18, "MEME")
	b := f.latest - 6_000_000
	mp := newUSDTestPool(f, mpool, farAsset, meme, 18, 18)
	mp.trade(b-2_000_000, 1)
	mp.trade(b+500, 1.1)
	id := "0x" + strings.Repeat("56", 32)
	f.mu.Lock()
	f.initV4(id, farAsset, meme, zeroAddr, 500, b-1_500_000)
	f.transfer(farAsset, farWallet, uBuyer, b-10, "0xw1")
	f.mu.Unlock()
	clk := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH, "SCOUT_DISCOVERY_BLOCKS": "1000"})
	o.now = clk.now
	ctx := context.Background()
	if p, ok, err := o.quoteUSD(ctx, farAsset, b); ok || err != nil {
		t.Fatalf("quoteUSD(NOP, %d): got %v ok=%v err=%v, want ok=false and no error (definitive)", b, p, ok, err)
	}
	before := o.rpc.requests.Load()
	for _, blk := range []uint64{b, b - 3_000_000, b + 2_000_000} {
		if p, ok, err := o.quoteUSD(ctx, farAsset, blk); ok || err != nil {
			t.Fatalf("quoteUSD(NOP, %d) again: got %v ok=%v err=%v, want the definitive answer", blk, p, ok, err)
		}
	}
	if n := o.rpc.requests.Load() - before; n != 0 {
		t.Fatalf("got %d request(s) for the cached definitive answer, want 0", n)
	}
}

// A quote asset whose USD pool only trades after the block (far after: outside
// the near-block window) has no price yet there: a temporary error, never the
// definitive "no USD source".
func TestQuoteViaPoolsPoolOnlyAfterBlock(t *testing.T) {
	f := usdChain(t)
	f.addToken(farAsset, 18, "LATE")
	b := f.latest - 6_000_000
	newUSDTestPool(f, farPool, farAsset, tUSDG, 18, 6).trade(b+2_000_000, 5)
	o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH, "SCOUT_DISCOVERY_BLOCKS": "1000"})
	ctx := context.Background()
	if p, ok, err := o.quoteUSD(ctx, farAsset, b); ok || !errors.Is(err, errNoPriceYet) {
		t.Fatalf("quoteUSD(LATE, %d): got %v ok=%v err=%v, want errNoPriceYet", b, p, ok, err)
	}
	if p, ok, err := o.quoteUSD(ctx, farAsset, b+2_000_100); err != nil || !ok || !near6(p, 5) {
		t.Fatalf("quoteUSD(LATE) after the trade: got %v ok=%v err=%v, want $5", p, ok, err)
	}
}

// A node error during the whole-history search (the wide transfer read, the
// v4 Initialize read, a counterparty's token0()) is returned and nothing of
// it is kept; once the node answers, the far pool prices the block.
func TestQuoteViaPoolsHistoryNodeErrorNotCached(t *testing.T) {
	fastRetries(t)
	old := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = old })
	for _, c := range []struct {
		name string
		busy func(f *fakeChain, on *bool)
	}{
		{"wide transfer read", func(f *fakeChain, on *bool) {
			f.setLogsHook(func(addr string, from, to uint64) string {
				if *on && addr == farAsset && to-from > 200_000 {
					return "rate limit exceeded"
				}
				return ""
			})
		}},
		{"v4 Initialize read", func(f *fakeChain, on *bool) {
			f.setLogsHook(func(addr string, _, _ uint64) string {
				if *on && addr == tPM {
					return "rate limit exceeded"
				}
				return ""
			})
		}},
		{"token0() of a counterparty", func(f *fakeChain, on *bool) {
			f.callErr = func(to, sel string, _ uint64) string {
				if *on && to == farPool && sel == selToken0 {
					return "rate limit exceeded"
				}
				return ""
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fc := newFarPoolChain(t)
			on := true
			c.busy(fc.f, &on)
			o := testOnchain(t, fc.f, fc.env)
			ctx := context.Background()
			if p, ok, err := o.quoteUSD(ctx, farAsset, fc.b1); err == nil || ok || errors.Is(err, errNoPriceYet) || errors.Is(err, errNoUSDSource) {
				t.Fatalf("busy node: got %v ok=%v err=%v, want a node error", p, ok, err)
			}
			o.mu.Lock()
			set := o.usdPools[farAsset]
			kept := set != nil && (!set.noPoolUntil.IsZero() || len(set.pools) > 0 || len(set.noPrice) > 0 || set.histThrough > 0 || len(set.histSeen) > 0 || len(set.histPending) > 0)
			o.mu.Unlock()
			if kept {
				t.Fatalf("busy node: the failed search was kept: %+v", set)
			}
			on = false
			if p, ok, err := o.quoteUSD(ctx, farAsset, fc.b1); err != nil || !ok || !near6(p, 2) {
				t.Fatalf("node back: got %v ok=%v err=%v, want $2", p, ok, err)
			}
		})
	}
}

// setHistBounds lowers the whole-history sweep's bounds for one test.
func setHistBounds(t *testing.T, maxLogs, maxChecks int) {
	t.Helper()
	oldLogs, oldChecks := usdHistMaxLogs, usdHistMaxChecks
	usdHistMaxLogs, usdHistMaxChecks = maxLogs, maxChecks
	t.Cleanup(func() { usdHistMaxLogs, usdHistMaxChecks = oldLogs, oldChecks })
}

// A sweep stopped by its bounds is never a definitive answer, and never a
// "no price yet" that the deadline cap could make final: errUSDSearchPending
// until a sweep has covered the whole history, each sweep going on from where
// the last one stopped. FAR's counterparties, busiest first: the buyer (6
// transfers), a wallet (4), the pool (2). NOP's three transfers are in three
// blocks.
func TestQuoteViaPoolsHistoryBounded(t *testing.T) {
	ctx := context.Background()
	type ask struct {
		price    float64 // 0: no price
		definite bool    // ok=false, no error
	}
	for _, c := range []struct {
		name              string
		maxLogs, maxCheck int
		noPool            bool
		want              []ask
	}{
		// one counterparty per sweep: the pool is the third
		{"1 check per sweep, far pool", 100_000, 1, false, []ask{{}, {}, {price: 2}}},
		// one transfer per sweep: the pool's first trade is the first transfer
		{"1 log per sweep, far pool", 1, 256, false, []ask{{price: 2}}},
		// one transfer per sweep, no USD pool: definitive only after the third
		{"1 log per sweep, no pool", 1, 256, true, []ask{{}, {}, {definite: true}}},
		// one counterparty per sweep, no USD pool (pool, wallet, buyer)
		{"1 check per sweep, no pool", 100_000, 1, true, []ask{{}, {}, {definite: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setHistBounds(t, c.maxLogs, c.maxCheck)
			var o *onchainSource
			var asset string
			var block uint64
			if c.noPool {
				const meme = "0x9999999999999999999999999999999999999999"
				const mpool = "0x00000000000000000000000000000000000000e2"
				f := usdChain(t)
				f.addToken(farAsset, 18, "NOP")
				f.addToken(meme, 18, "MEME")
				block = f.latest - 6_000_000
				mp := newUSDTestPool(f, mpool, farAsset, meme, 18, 18)
				mp.trade(block-2_000_000, 1)
				mp.trade(block+500, 1.1)
				f.mu.Lock()
				f.transfer(farAsset, farWallet, uBuyer, block-10, "0xw1")
				f.mu.Unlock()
				o = testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + uFeedETH, "SCOUT_DISCOVERY_BLOCKS": "1000"})
				asset = farAsset
			} else {
				fc := newFarPoolChain(t)
				o = testOnchain(t, fc.f, fc.env)
				asset, block = farAsset, fc.b1
			}
			for i, w := range c.want {
				p, ok, err := o.quoteUSD(ctx, asset, block)
				switch {
				case w.price > 0:
					if err != nil || !ok || !near6(p, w.price) {
						t.Fatalf("ask %d (bounds %d logs, %d checks): got %v ok=%v err=%v, want $%v", i+1, c.maxLogs, c.maxCheck, p, ok, err, w.price)
					}
				case w.definite:
					if err != nil || ok {
						t.Fatalf("ask %d (bounds %d logs, %d checks): got %v ok=%v err=%v, want the definitive ok=false, no error", i+1, c.maxLogs, c.maxCheck, p, ok, err)
					}
				default:
					if ok || !errors.Is(err, errUSDSearchPending) || noPriceYetIsFinal(err, time.Now(), time.Now().Add(-time.Hour)) {
						t.Fatalf("ask %d (bounds %d logs, %d checks): got %v ok=%v err=%v, want errUSDSearchPending, never final (the sweep is not finished)", i+1, c.maxLogs, c.maxCheck, p, ok, err)
					}
				}
			}
		})
	}
}

// Concurrent callers on the same cold quote asset share one whole-history
// sweep: the far pool's token0() is asked once, not once per caller.
func TestQuoteViaPoolsHistoryOneSweepAtATime(t *testing.T) {
	fc := newFarPoolChain(t)
	var n int64
	var mu sync.Mutex
	fc.f.callErr = func(to, sel string, _ uint64) string {
		if to == farPool && sel == selToken0 {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return ""
	}
	o := testOnchain(t, fc.f, fc.env)
	ctx := context.Background()
	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	prices := make([]float64, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, ok, err := o.quoteUSD(ctx, farAsset, fc.b1)
			if err == nil && !ok {
				err = errors.New("ok=false")
			}
			prices[i], errs[i] = p, err
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || !near6(prices[i], 2) {
			t.Fatalf("caller %d: got %v %v, want $2", i, prices[i], errs[i])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("token0() of the far pool asked %d times by %d concurrent callers, want 1 (one sweep at a time)", n, callers)
	}
}

// A call waiting for its quote's USD pool search is tried again soon (each try
// moves the search on), and that wait is never final; every other USD failure
// keeps the back-off.
func TestUSDRetryIn(t *testing.T) {
	pending := fmt.Errorf("USD price of QT: %w", fmt.Errorf("0x22 at block 9: %w (…)", errUSDSearchPending))
	noPrice := fmt.Errorf("USD price of QT: %w", errNoPriceYet)
	node := errors.New("rpc error -32000: busy")
	for _, c := range []struct {
		name     string
		err      error
		attempts int
		want     time.Duration
	}{
		{"search pending, attempt 1", pending, 1, usdSearchRetry},
		{"search pending, attempt 20", pending, 20, usdSearchRetry},
		{"no price yet, attempt 3", noPrice, 3, backoff(3)},
		{"node error, attempt 20", node, 20, backoff(20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := usdRetryIn(c.err, c.attempts); got != c.want {
				t.Errorf("usdRetryIn(%v, %d) = %s, want %s", c.err, c.attempts, got, c.want)
			}
		})
	}
	if noPriceYetIsFinal(pending, time.Now(), time.Now().Add(-time.Hour)) {
		t.Errorf("noPriceYetIsFinal(%v) after the deadline = true, want false (the search goes on)", pending)
	}
}
