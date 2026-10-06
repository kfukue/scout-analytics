package main

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Rugs: the pool's quote side under SCOUT_RUG_LIQ_USD (fake chain, no database).
// ---------------------------------------------------------------------------

// minSqrtX96 is just above Uniswap's MIN_SQRT_RATIO: where a swap ends when it
// runs out of in-range liquidity selling token1 for token0. A token that is
// token1 is then priced at about 2^128 quote per token.
var minSqrtX96 = big.NewInt(4295128740)

// v3/v4 swap data with the given sqrtPriceX96, liquidity and tick.
func swapData(sqrtP, liq *big.Int, tick int64) string {
	return ret(wInt(-5), wInt(7), w32(sqrtP), w32(liq), wInt(tick))
}

// liqFor returns the in-range L that holds q whole units of the quote asset
// (decimals qDec) on the quote side at the raw price sqrtP (token1 per token0).
func liqFor(q float64, qDec int, sqrtP *big.Int, quoteIs1 bool) *big.Int {
	sp := new(big.Float).SetPrec(256).SetInt(sqrtP)
	sp.Quo(sp, new(big.Float).SetPrec(256).SetInt(new(big.Int).Lsh(big.NewInt(1), 96)))
	amt := new(big.Float).SetPrec(256).SetFloat64(q * pow10(qDec))
	if quoteIs1 {
		amt.Quo(amt, sp) // quote = L·sqrtP
	} else {
		amt.Mul(amt, sp) // quote = L / sqrtP
	}
	out, _ := amt.Int(nil)
	return out
}

func TestEventOfLogLiquidity(t *testing.T) {
	// 18-decimal token against 6-decimal USDG at $0.002, both orders: the
	// formula's orientation and decimal scaling must give the same dollars.
	// The measure is the quote side only (no 2×), against the $500 default.
	for _, tokenIs0 := range []bool{true, false} {
		st := &onchainState{Kind: "v3", TokenIs0: tokenIs0, TokenDec: 18, QuoteDec: 6}
		raw := 0.002 * 1e6 / 1e18 // token1 (USDG) per token0 (token), raw units
		if !tokenIs0 {
			raw = 1 / raw
		}
		sqrtP := sqrtX96(raw)
		for _, c := range []struct {
			quoteSide float64 // USDG on the quote side
			rug       bool
		}{{600, false}, {400, true}, {5000, false}} {
			l := rpcLog{Data: swapData(sqrtP, liqFor(c.quoteSide, 6, sqrtP, tokenIs0), 0)}
			ev := st.eventOfLog(l)
			if math.Abs(ev.price-0.002)/0.002 > 1e-9 || ev.drained || !ev.liqKnown || math.Abs(ev.liqQ-c.quoteSide)/c.quoteSide > 1e-6 {
				t.Fatalf("token0=%v quote side %v: got %+v, want liqQ %v", tokenIs0, c.quoteSide, ev, c.quoteSide)
			}
			rug, liq := ev.rug(1, defaultRugLiqUSD)
			if rug != c.rug || liq == nil || math.Abs(*liq-c.quoteSide) > 1e-3 {
				t.Fatalf("token0=%v quote side %v: got rug %v liq %v, want rug %v liq %v", tokenIs0, c.quoteSide, rug, rugF(liq), c.rug, c.quoteSide)
			}
			// no USD price for the quote: never a guess, only the certain signals
			if rug, liq := ev.rug(0, defaultRugLiqUSD); rug || liq != nil {
				t.Fatalf("unknown USD price: rug %v liq %v", rug, rugF(liq))
			}
			// threshold 0: the USD check is off
			if rug, _ := ev.rug(1, 0); rug {
				t.Fatalf("token0=%v quote side %v: rugged with the USD check off", tokenIs0, c.quoteSide)
			}
		}
	}

	st := &onchainState{Kind: "v4", TokenIs0: false, TokenDec: 18, QuoteDec: 18}
	healthy := sqrtX96(1 / 1e-6)
	for name, c := range map[string]struct {
		data    string
		drained bool
	}{
		"no liquidity":         {swapData(healthy, big.NewInt(0), 0), true},
		"at the lower bound":   {swapData(minSqrtX96, fakeLiq, -maxTick), true},
		"near the upper bound": {swapData(healthy, fakeLiq, maxTick-50), true},
		"far from the bounds":  {swapData(healthy, fakeLiq, maxTick-150), false},
		"no liquidity words":   {ret(wInt(-5), wInt(7), w32(healthy)), false},
	} {
		ev := st.eventOfLog(rpcLog{Data: c.data})
		if ev.drained != c.drained || (c.drained && ev.price != 0) || (!c.drained && ev.price <= 0) {
			t.Errorf("%s: %+v", name, ev)
		}
		if rug, _ := ev.rug(0, defaultRugLiqUSD); rug != c.drained {
			t.Errorf("%s: rug %v without a USD price", name, rug)
		}
		// an empty pool counts even with the USD check off
		if rug, _ := ev.rug(1, 0); rug != c.drained {
			t.Errorf("%s: got rug %v with threshold 0, want %v", name, rug, c.drained)
		}
	}
	// the bound price the old code took: about 2^128 quote per token
	if p := priceFromSqrtX96(minSqrtX96, 18, 18); p <= 0 || 1/p < 1e38 {
		t.Fatalf("bound price %v", 1/p)
	}

	// v2: the quote reserve; an empty side is drained
	v2 := &onchainState{Kind: "v2", TokenIs0: true, TokenDec: 18, QuoteDec: 6}
	ev := v2.eventOfLog(rpcLog{Data: ret(w32(wei(1000)), w32(big.NewInt(450e6)))}) // 1000 tokens, 450 USDG
	if ev.drained || math.Abs(ev.liqQ-450) > 1e-9 || math.Abs(ev.price-0.45) > 1e-12 {
		t.Fatalf("v2: got %+v, want liqQ 450 price 0.45", ev)
	}
	if rug, liq := ev.rug(1, defaultRugLiqUSD); !rug || *liq != 450 {
		t.Fatalf("v2 $450 quote side: got rug %v liq %v, want rugged at 450", rug, rugF(liq))
	}
	if rug, _ := ev.rug(1, 250); rug {
		t.Fatal("v2 $450 quote side: rugged under a $250 threshold")
	}
	if ev := v2.eventOfLog(rpcLog{Data: ret(wInt(0), wInt(0))}); !ev.drained || ev.price != 0 {
		t.Fatalf("v2 empty: %+v", ev)
	}
}

// rugChain: a v3 TOKEN/WETH pool (the token is token1) on a fake chain, with
// helpers for a normal trade and the swap that drains it.
type rugChain struct {
	f           *fakeChain
	token, pool string
	eb          uint64
}

func newRugChain(t *testing.T, age, entryAgo time.Duration) *rugChain {
	f := newFakeChain(t, age)
	rc := &rugChain{f: f, token: "0x4444444444444444444444444444444444444444", pool: "0x00000000000000000000000000000000000000c3"}
	f.addToken(rc.token, 18, "COOK")
	f.addToken(tWETH, 18, "WETH")
	f.constCall(rc.pool, selToken0, ret(wAddr(tWETH)))
	f.constCall(rc.pool, selToken1, ret(wAddr(rc.token)))
	f.constCall(rc.pool, selSlot0, ret(wInt(1)))
	rc.eb = f.blockAtTime(time.Now().Add(-entryAgo))
	return rc
}

func (rc *rugChain) trade(block uint64, pWETH float64) {
	rc.f.swapV3(rc.pool, block, hexU64(block), sqrtX96(1/pWETH))
	rc.f.transfer(rc.token, rc.pool, "0x00000000000000000000000000000000000000d1", block, hexU64(block))
}

// drain: liquidity removed, then a swap that ends at the tick bound with L = 0.
func (rc *rugChain) drain(block uint64) {
	rc.f.swapV3L(rc.pool, block, hexU64(block), minSqrtX96, big.NewInt(0), -maxTick)
	rc.f.transfer(rc.token, rc.pool, "0x00000000000000000000000000000000000000d1", block, hexU64(block))
}

func (rc *rugChain) state(t *testing.T, o *onchainSource) *onchainState {
	t.Helper()
	st, err := o.discover(context.Background(), rc.token, rc.eb)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.entryPrice(context.Background(), st, rc.f.latest); err != nil {
		t.Fatal(err)
	}
	return st
}

// The bound price after a drain is ignored, the rug block is kept, nothing after
// it counts (not even trades in a re-funded pool), and the peak is the one before.
func TestScanStopsAtRug(t *testing.T) {
	rc := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	rc.trade(rc.eb-500, 1e-6)
	rc.trade(rc.eb+20000, 3e-6) // the peak
	rc.trade(rc.eb+30000, 2e-6)
	rc.drain(rc.eb + 50000)
	rc.trade(rc.eb+60000, 9e-6) // liquidity added back: still rugged
	o := testOnchain(t, rc.f, nil)
	st := rc.state(t, o)
	if st.RugBlock != 0 || st.EntryPriceQ != 1e-6 {
		t.Fatalf("entry: %+v", st)
	}
	var seen []float64
	if err := o.scan(context.Background(), st, rc.eb+40000, func(_ uint64, p float64) { seen = append(seen, p) }); err != nil {
		t.Fatal(err)
	}
	if st.RugBlock != 0 || len(seen) != 2 {
		t.Fatalf("before the drain: rug %d, events %v", st.RugBlock, seen)
	}
	if err := o.scan(context.Background(), st, rc.eb+100000, func(_ uint64, p float64) { seen = append(seen, p) }); err != nil {
		t.Fatal(err)
	}
	if st.RugBlock != rc.eb+50000 || st.RugLiquidityUSD == nil || *st.RugLiquidityUSD != 0 {
		t.Fatalf("rug block %d (want %d), liquidity %v", st.RugBlock, rc.eb+50000, rugF(st.RugLiquidityUSD))
	}
	if len(seen) != 2 || math.Abs(st.RunMaxQ-3e-6) > 1e-15 || math.Abs(st.LastPriceQ-2e-6) > 1e-15 || st.LastPriceBlock != rc.eb+30000 {
		t.Fatalf("after the drain: events %v, max %v, last %v at %d", seen, st.RunMaxQ, st.LastPriceQ, st.LastPriceBlock)
	}
}

// v4: the same drain on the PoolManager, found by its tick and zero liquidity.
func TestScanStopsAtRugV4(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	token := "0x9999999999999999999999999999999999991e18"
	id := "0x" + "ab" + "000000000000000000000000000000000000000000000000000000000000ab"
	f.addToken(token, 18, "COOK")
	f.addToken(tWETH, 18, "WETH")
	eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
	f.logs = append(f.logs, fakeLog{addr: tPM, topics: []string{topicInitV4, id, addrTopic(tWETH), addrTopic(token)}, data: ret(wInt(3000)), block: eb - 400000, tx: "0xinit"})
	swap := func(block uint64, sqrtP, liq *big.Int, tick int64) {
		tx := hexU64(block)
		f.swapV4L(tPM, id, block, tx, sqrtP, liq, tick)
		f.transfer(token, tPM, "0x00000000000000000000000000000000000000d1", block, tx)
	}
	swap(eb-50, sqrtX96(1/1e-6), fakeLiq, 0)
	swap(eb+900, sqrtX96(1/4e-6), fakeLiq, 0)
	swap(eb+2000, minSqrtX96, big.NewInt(0), -maxTick)
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	st, err := o.discover(ctx, token, eb)
	if err != nil || st.Kind != "v4" || st.TokenIs0 {
		t.Fatalf("discover %+v %v", st, err)
	}
	if err := o.entryPrice(ctx, st, f.latest); err != nil {
		t.Fatal(err)
	}
	if err := o.scan(ctx, st, eb+5000, nil); err != nil {
		t.Fatal(err)
	}
	if st.RugBlock != eb+2000 || math.Abs(st.RunMaxQ-4e-6) > 1e-15 || math.Abs(st.LastPriceQ-4e-6) > 1e-15 {
		t.Fatalf("v4: rug %d, max %v, last %v", st.RugBlock, st.RunMaxQ, st.LastPriceQ)
	}
}

// With a USD price for the quote, a pool whose quote side is under the $500
// default is a rug; one above it is not.
func TestScanRugThresholdUSD(t *testing.T) {
	rc := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	thin := func(block uint64, pWETH, quoteWETH float64) {
		sp := sqrtX96(1 / pWETH)
		rc.f.swapV3L(rc.pool, block, hexU64(block), sp, liqFor(quoteWETH, 18, sp, false), 0)
		rc.f.transfer(rc.token, rc.pool, "0x00000000000000000000000000000000000000d1", block, hexU64(block))
	}
	rc.trade(rc.eb-500, 1e-6)
	thin(rc.eb+1000, 1.5e-6, 0.2) // 0.2 WETH × $3000 = $600 quote side: thin but alive
	thin(rc.eb+2000, 1e-6, 0.4/3) // $400 quote side: rugged
	o := testOnchain(t, rc.f, map[string]string{"SCOUT_RUG_LIQ_USD": ""})
	st := rc.state(t, o)
	st.EntryQuoteUSD = 3000
	if err := o.scan(context.Background(), st, rc.eb+1500, nil); err != nil || st.RugBlock != 0 || math.Abs(st.LastPriceQ-1.5e-6) > 1e-15 {
		t.Fatalf("$600 quote side: got rug %d last %v (%v), want no rug, last 1.5e-6", st.RugBlock, st.LastPriceQ, err)
	}
	if err := o.scan(context.Background(), st, rc.eb+3000, nil); err != nil || st.RugBlock != rc.eb+2000 ||
		st.RugLiquidityUSD == nil || math.Abs(*st.RugLiquidityUSD-400) > 1e-6 || math.Abs(st.LastPriceQ-1.5e-6) > 1e-15 {
		t.Fatalf("$400 quote side: got rug %d liq %v last %v (%v), want rug %d liq 400 last 1.5e-6",
			st.RugBlock, rugF(st.RugLiquidityUSD), st.LastPriceQ, err, rc.eb+2000)
	}
	// a quote without a USD price: the same $400 pool is not a rug (no guess)
	st2 := rc.state(t, o)
	if err := o.scan(context.Background(), st2, rc.eb+3000, nil); err != nil || st2.RugBlock != 0 || math.Abs(st2.LastPriceQ-1e-6) > 1e-15 {
		t.Fatalf("no USD price: rug %d last %v", st2.RugBlock, st2.LastPriceQ)
	}
}

// Backstop: a price more than 1e6 × the entry is skipped, even in a deep pool.
func TestScanSkipsImplausiblePrice(t *testing.T) {
	rc := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	rc.trade(rc.eb-500, 1e-6)
	rc.trade(rc.eb+1000, 2e-6)
	rc.trade(rc.eb+2000, 2.5) // 2.5e6 × entry
	rc.trade(rc.eb+2500, 3.0) // again: skipped, logged once
	rc.trade(rc.eb+3000, 1.5e-6)
	o := testOnchain(t, rc.f, nil)
	st := rc.state(t, o)
	logs := captureLog(t)
	if err := o.scan(context.Background(), st, rc.eb+5000, nil); err != nil {
		t.Fatal(err)
	}
	if st.RugBlock != 0 || math.Abs(st.RunMaxQ-2e-6) > 1e-15 || math.Abs(st.LastPriceQ-1.5e-6) > 1e-15 {
		t.Fatalf("backstop: rug %d max %v last %v", st.RugBlock, st.RunMaxQ, st.LastPriceQ)
	}
	if n := countLines(logs.String(), "skipped as invalid"); n != 1 {
		t.Fatalf("backstop logged %d times, want once:\n%s", n, logs.String())
	}
	// a price just under the limit still counts
	if st.implausible(0.9, 1) {
		t.Fatal("0.9 = 900000 × entry is plausible")
	}
}

// Drained at the call: rugged from the start, the entry is the last real price.
func TestEntryPriceRuggedAtEntry(t *testing.T) {
	rc := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	rc.trade(rc.eb-3000, 1e-6)
	rc.drain(rc.eb - 1000)
	rc.trade(rc.eb+1000, 5e-6) // after the call: ignored
	o := testOnchain(t, rc.f, nil)
	st := rc.state(t, o)
	if st.EntryPriceQ != 1e-6 || st.RugBlock != rc.eb-1000 || st.LastPriceQ != 1e-6 {
		t.Fatalf("entry %v rug %d last %v", st.EntryPriceQ, st.RugBlock, st.LastPriceQ)
	}
	if err := o.scan(context.Background(), st, rc.eb+5000, nil); err != nil || st.RunMaxQ != 1e-6 || st.LastPriceQ != 1e-6 {
		t.Fatalf("after: max %v last %v (%v)", st.RunMaxQ, st.LastPriceQ, err)
	}

	// drained, then funded again before the call: not a rug; the entry is the
	// later price, never the bound price
	rc2 := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	rc2.trade(rc2.eb-3000, 1e-6)
	rc2.drain(rc2.eb - 2000)
	rc2.trade(rc2.eb-1000, 2e-6)
	st2 := rc2.state(t, testOnchain(t, rc2.f, nil))
	if st2.RugBlock != 0 || st2.EntryPriceQ != 2e-6 || st2.EntryLiqQ <= 0 {
		t.Fatalf("re-funded: rug %d entry %v liq %v", st2.RugBlock, st2.EntryPriceQ, st2.EntryLiqQ)
	}

	// the token never traded before the call and its pool was swapped into empty
	// first: not funded yet, not a rug; the entry is the first real trade
	rc3 := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	rc3.drain(rc3.eb + 100)
	rc3.trade(rc3.eb+200, 3e-6)
	st3 := rc3.state(t, testOnchain(t, rc3.f, nil))
	if st3.RugBlock != 0 || st3.EntryPriceQ != 3e-6 {
		t.Fatalf("unfunded: rug %d entry %v", st3.RugBlock, st3.EntryPriceQ)
	}
}

// onchainHorizon: a rugged horizon is -100% with price 0, keeps the peak from
// before the rug, and its low is -100%.
func TestOnchainHorizonRugged(t *testing.T) {
	st := &onchainState{LastPriceQ: 2e-6, LatePriceQ: 1.2e-6, RunMaxU: 0.009, RunMinU: 0.0024, RunMaxLateU: 0.009, RunMinLateU: 0.0045}
	due := time.Now()
	ok, late := onchainHorizon(st, "1d", due, 0.003, 3000, 3000, false)
	if !near(ok.ReturnPct, 100) || !near(ok.MaxGainPct, 200) || !near(ok.MaxDDPct, -20) || late == nil || !near(*late, 0.0036) {
		t.Fatalf("not rugged: %+v", ok)
	}
	r, _ := onchainHorizon(st, "3d", due, 0.003, 3000, 0, true)
	if r.PriceUSD != 0 || r.MinPriceUSD != 0 || r.ReturnPct != -100 || r.MaxDDPct != -100 || !near(r.MaxGainPct, 200) ||
		*r.ReturnLatePct != -100 || *r.MaxDDLatePct != -100 || !near(*r.MaxGainLatePct, 150) {
		t.Fatalf("rugged: %+v late %v %v %v", r, *r.ReturnLatePct, *r.MaxGainLatePct, *r.MaxDDLatePct)
	}
	// rugged at entry: no peak at all
	r0, _ := onchainHorizon(&onchainState{LastPriceQ: 1e-6, LatePriceQ: 1e-6}, "1h", due, 0.003, 3000, 0, true)
	if r0.ReturnPct != -100 || r0.MaxGainPct != 0 || *r0.MaxGainLatePct != 0 || *r0.ReturnLatePct != -100 {
		t.Fatalf("rugged at entry: %+v", r0)
	}
}

// A threshold from the environment (as set in .env) is the one the price
// events are checked against: a $400 quote side is a rug under the $500
// default but not under SCOUT_RUG_LIQ_USD=250; $200 is a rug under both.
func TestScanRugCustomThreshold(t *testing.T) {
	rc := newRugChain(t, 20*24*time.Hour, 8*24*time.Hour)
	thin := func(block uint64, pWETH, quoteWETH float64) {
		sp := sqrtX96(1 / pWETH)
		rc.f.swapV3L(rc.pool, block, hexU64(block), sp, liqFor(quoteWETH, 18, sp, false), 0)
		rc.f.transfer(rc.token, rc.pool, "0x00000000000000000000000000000000000000d1", block, hexU64(block))
	}
	rc.trade(rc.eb-500, 1e-6)
	thin(rc.eb+1000, 1.5e-6, 0.4/3) // $400 quote side
	thin(rc.eb+2000, 1.2e-6, 0.2/3) // $200 quote side
	for _, c := range []struct {
		env      string
		wantRug  uint64
		wantLiq  float64
		wantLast float64
	}{
		{"", rc.eb + 1000, 400, 1e-6},      // the $500 default
		{"250", rc.eb + 2000, 200, 1.5e-6}, // custom: $400 is alive, $200 is not
	} {
		name := "SCOUT_RUG_LIQ_USD=" + c.env
		if c.env == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			o := testOnchain(t, rc.f, map[string]string{"SCOUT_RUG_LIQ_USD": c.env})
			st := rc.state(t, o)
			st.EntryQuoteUSD = 3000
			if err := o.scan(context.Background(), st, rc.eb+3000, nil); err != nil {
				t.Fatal(err)
			}
			if st.RugBlock != c.wantRug || st.RugLiquidityUSD == nil || math.Abs(*st.RugLiquidityUSD-c.wantLiq) > 1e-6 ||
				math.Abs(st.LastPriceQ-c.wantLast) > 1e-15 {
				t.Fatalf("got rug %d liq %v last %v, want rug %d liq %v last %v",
					st.RugBlock, rugF(st.RugLiquidityUSD), st.LastPriceQ, c.wantRug, c.wantLiq, c.wantLast)
			}
		})
	}
}

func TestRugLiqConfig(t *testing.T) {
	t.Setenv("SCOUT_RUG_LIQ_USD", "")
	pc, err := loadPriceConfig()
	if err != nil || pc.RugLiqUSD != 500 || pc.Onchain.RugLiqUSD != 500 {
		t.Fatalf("default: got %v %v %v, want 500", pc.RugLiqUSD, pc.Onchain.RugLiqUSD, err)
	}
	t.Setenv("SCOUT_RUG_LIQ_USD", "0") // the USD check off
	if pc, err := loadPriceConfig(); err != nil || pc.RugLiqUSD != 0 || pc.Onchain.RugLiqUSD != 0 {
		t.Fatalf("0: got %v %v %v, want 0", pc.RugLiqUSD, pc.Onchain.RugLiqUSD, err)
	}
	t.Setenv("SCOUT_RUG_LIQ_USD", "250")
	if pc, err := loadPriceConfig(); err != nil || pc.RugLiqUSD != 250 || pc.Onchain.RugLiqUSD != 250 {
		t.Fatalf("250: %v %v %v", pc.RugLiqUSD, pc.Onchain.RugLiqUSD, err)
	}
	for _, bad := range []string{"-1", "abc", "NaN"} {
		t.Setenv("SCOUT_RUG_LIQ_USD", bad)
		if _, err := loadOnchainConfig(); err == nil {
			t.Fatalf("SCOUT_RUG_LIQ_USD=%q accepted by loadOnchainConfig", bad)
		}
		if _, err := loadPriceConfig(); err == nil {
			t.Fatalf("SCOUT_RUG_LIQ_USD=%q accepted by loadPriceConfig", bad)
		}
	}
}

// rugF prints an optional number for test messages.
func rugF(p *float64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

func countLines(text, sub string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
