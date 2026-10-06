package main

import (
	"context"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Rugs end to end with the database: what is stored in scout_call_returns and
// scout_call_tracking when a pool's quote side drops under SCOUT_RUG_LIQ_USD.
// ETH = $3000 (ltChain); the threshold is the default $500 of quote side;
// current_liquidity_usd stores the pool's depth, 2 × the quote side.
// ---------------------------------------------------------------------------

// rugPool is a TOKEN/WETH pool (the token is token1) of one kind on a fake chain.
type rugPool struct {
	f     *fakeChain
	token string
	swap  func(block uint64, sqrtP, liq *big.Int, tick int64) // v3/v4
	sync  func(block uint64, weth, tokens float64)            // v2
}

func newRugPool(f *fakeChain, kind, token, pool string, eb uint64) *rugPool {
	rp := &rugPool{f: f, token: token}
	switch kind {
	case "v2":
		f.addToken(token, 18, "TKN")
		f.constCall(pool, selToken0, ret(wAddr(tWETH)))
		f.constCall(pool, selToken1, ret(wAddr(token)))
		f.constCall(pool, selGetReserves, ret(wInt(1), wInt(1), wInt(0)))
		rp.sync = func(block uint64, weth, tokens float64) {
			f.syncV2(pool, block, hexU64(block), wei(weth), wei(tokens))
			f.transfer(token, pool, ltBuyer, block, hexU64(block))
		}
	case "v3":
		f.ltPool(token, pool, tWETH)
		rp.swap = func(block uint64, sqrtP, liq *big.Int, tick int64) {
			f.swapV3L(pool, block, hexU64(block), sqrtP, liq, tick)
			f.transfer(token, pool, ltBuyer, block, hexU64(block))
		}
	case "v4":
		f.addToken(token, 18, "TKN")
		id := "0x" + token[2:] + "000000000000000000000000"
		f.logs = append(f.logs, fakeLog{addr: tPM, topics: []string{topicInitV4, id, addrTopic(tWETH), addrTopic(token)},
			data: ret(wInt(3000)), block: eb - 400000, tx: "0xinit" + token[2:8]})
		rp.swap = func(block uint64, sqrtP, liq *big.Int, tick int64) {
			f.swapV4L(tPM, id, block, hexU64(block), sqrtP, liq, tick)
			f.transfer(token, tPM, ltBuyer, block, hexU64(block))
		}
	}
	return rp
}

// trade: a normal trade in a deep pool leaving the price at p WETH.
func (rp *rugPool) trade(block uint64, p float64) {
	if rp.sync != nil {
		rp.sync(block, 10, 10/p)
		return
	}
	rp.swap(block, sqrtX96(1/p), fakeLiq, 0)
}

// drain: the liquidity is removed and a swap runs to the tick bound (v3/v4),
// or the WETH reserve drops to 0.4/3 WETH = $400 of quote side (v2), under the
// $500 default.
func (rp *rugPool) drain(block uint64) {
	if rp.sync != nil {
		rp.sync(block, 0.4/3, 1e10)
		return
	}
	rp.swap(block, minSqrtX96, big.NewInt(0), -maxTick)
}

type rugCall struct {
	id    int
	entry time.Time
	eb    uint64
}

func (c rugCall) at(d time.Duration) uint64 { return uint64(int64(c.eb) + int64(d/time.Second)*10) }

func rugReturns(t *testing.T, st *ScoutStore, id int) map[string]horizonResult {
	t.Helper()
	rs, err := st.ReturnsForCall(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// isRugged: a horizon stored as rugged: price 0, return and low -100% (also
// from the late entry), and the given peak (from before the rug).
func isRugged(r horizonResult, peak, peakLate float64) bool {
	return r.Status == "done" && r.PriceUSD == 0 && r.MinPriceUSD == 0 && r.ReturnPct == -100 && r.MaxDDPct == -100 &&
		near(r.MaxGainPct, peak) && r.ReturnLatePct != nil && *r.ReturnLatePct == -100 &&
		r.MaxDDLatePct != nil && *r.MaxDDLatePct == -100 && r.MaxGainLatePct != nil && near(*r.MaxGainLatePct, peakLate)
}

func rugHigh(t *testing.T, st *ScoutStore, id int) float64 {
	t.Helper()
	var hi float64
	if err := st.Pool.QueryRow(context.Background(), `SELECT COALESCE(max(high), 0)::float8 FROM scout_call_candles WHERE call_id = $1`, id).Scan(&hi); err != nil {
		t.Fatal(err)
	}
	return hi
}

// (a) v3 and v4: normal trades, then the pool is drained and a swap runs to the
// tick bound. The bound price is ignored, the call is rugged at that block (and
// flagged at once, while horizons are still pending), later horizons are -100%
// with the peak from before the rug, and the latest pass says -100% without
// reading the chain, at most once a day.
func TestTrackerRugDrainedPool(t *testing.T) {
	for _, kind := range []string{"v3", "v4"} {
		t.Run(kind, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			f := ltChain(t, 20*ltDay)
			c := rugCall{entry: time.Now().Add(-8 * ltDay)}
			c.eb = f.blockAtTime(c.entry)
			rp := newRugPool(f, kind, "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", c.eb)
			rp.trade(c.at(-2*time.Minute), 1e-6) // entry $0.003
			rp.trade(c.at(30*time.Second), 1e-6) // late entry $0.003
			rp.trade(c.at(30*time.Minute), 3e-6) // peak +200%
			rp.trade(c.at(50*time.Minute), 2e-6) // +100% at 1h and 1d
			rp.drain(c.at(2 * ltDay))            // the bound price: ~2^128 WETH per token
			rp.trade(c.at(4*ltDay), 5e-6)        // a trade after the rug: ignored

			s := ltScanner(t, st, f.srv.URL)
			s.onChannelPost(postAt(1, c.entry, rp.token))
			c.id = *(<-s.queue).CallID
			if n := s.trackDue(ctx, 50); n != 1 {
				t.Fatalf("processed %d", n)
			}
			tr, _ := st.GetTracking(ctx, c.id)
			if tr.Status != TrackTracking || tr.Rugged == nil || !*tr.Rugged || tr.CurrentLiquidityUSD == nil || *tr.CurrentLiquidityUSD != 0 ||
				tr.CurrentPriceUSD == nil || *tr.CurrentPriceUSD != 0 || *tr.PoolDex != "uniswap-"+kind {
				t.Fatalf("tracking row: %+v (err %s)", tr, strOrNil(tr.Error))
			}
			rs := rugReturns(t, st, c.id)
			if len(rs) != 4 {
				t.Fatalf("returns %v", sortedKeys(rs))
			}
			for _, h := range []string{"1h", "1d"} {
				if r := rs[h]; !near(r.ReturnPct, 100) || !near(r.MaxGainPct, 200) || !near(*r.ReturnLatePct, 100) {
					t.Fatalf("%s (before the rug): %+v", h, r)
				}
			}
			for _, h := range []string{"3d", "7d"} {
				if r := rs[h]; !isRugged(r, 200, 200) {
					t.Fatalf("%s (after the rug): %+v late %v/%v/%v", h, r, rugF(r.ReturnLatePct), rugF(r.MaxGainLatePct), rugF(r.MaxDDLatePct))
				}
			}
			os := ltState(t, st, c.id)
			if os.RugBlock != c.at(2*ltDay) || math.Abs(os.RunMaxQ-3e-6) > 1e-15 || math.Abs(os.RunMaxU-0.009) > 1e-12 ||
				math.Abs(os.LastPriceQ-2e-6) > 1e-15 || os.V != onchainStateVersion {
				t.Fatalf("state: %+v", os)
			}
			if hi := rugHigh(t, st, c.id); math.Abs(hi-0.009) > 1e-12 {
				t.Fatalf("candle high %v: the bound price or a post-rug trade got in", hi)
			}

			// latest pass: -100%, price 0, no log reads; due again only after a day
			f.took("eth_getLogs")
			if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
				t.Fatalf("pass %+v", res)
			}
			if lc := ltRead(t, st, c.id); !ltNear(lc.Ret, -100) || !ltNear(lc.Price, 0) || lc.Checked == nil {
				t.Fatalf("latest: %v %v", rugF(lc.Price), rugF(lc.Ret))
			}
			if n := f.took("eth_getLogs"); n != 0 {
				t.Fatalf("the pass read logs for a rugged call: %d", n)
			}
			ltMakeDue(t, st, "20 minutes") // a young call would be due again now …
			if res := s.refreshLatest(ctx, false); res.Due != 0 {
				t.Fatalf("rugged call due again after 20 minutes: %+v", res)
			}
			ltMakeDue(t, st, "25 hours") // … a rugged one only after a day
			if res := s.refreshLatest(ctx, false); res.Due != 1 || res.Refreshed != 1 || f.took("eth_getLogs") != 0 {
				t.Fatalf("daily refresh: %+v", res)
			}
			if lc := ltRead(t, st, c.id); !ltNear(lc.Ret, -100) {
				t.Fatalf("latest after the daily refresh: %v", rugF(lc.Ret))
			}
		})
	}
}

// (b) v2: a Sync leaves 0.4/3 WETH ($400 of quote side) → rugged, -100% after
// it; current_liquidity_usd = $800 (2 × the quote side).
// (c) a v2 pool with $600 of quote side left (thin, above the $500 default) is
// not rugged, also by the end-of-tracking check (balanceOf: 0.2 WETH = $600 of
// quote side, stored as $1200).
func TestTrackerRugV2Liquidity(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	thin := rugCall{entry: time.Now().Add(-8 * ltDay)}
	thin.eb = f.blockAtTime(thin.entry)
	alive := rugCall{entry: time.Now().Add(-35 * ltDay)}
	alive.eb = f.blockAtTime(alive.entry)
	rt := newRugPool(f, "v2", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c2", thin.eb)
	ra := newRugPool(f, "v2", "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c5", alive.eb)
	for _, x := range []struct {
		rp *rugPool
		c  rugCall
	}{{rt, thin}, {ra, alive}} {
		x.rp.trade(x.c.at(-2*time.Minute), 1e-6)
		x.rp.trade(x.c.at(30*time.Minute), 3e-6)
		x.rp.trade(x.c.at(50*time.Minute), 2e-6)
	}
	rt.drain(thin.at(2 * ltDay))
	ra.sync(alive.at(2*ltDay), 0.2, 2e5)                 // 1e-6 WETH, $600 of quote side: thin but alive
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(0.2)))) // what liquidityUSD reads at the end: $600 of quote side

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, thin.entry, rt.token))
	s.onChannelPost(postAt(2, alive.entry, ra.token))
	thin.id, alive.id = *(<-s.queue).CallID, *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("processed %d", n)
	}

	tt, _ := st.GetTracking(ctx, thin.id)
	if tt.Status != TrackTracking || tt.Rugged == nil || !*tt.Rugged || tt.CurrentLiquidityUSD == nil || math.Abs(*tt.CurrentLiquidityUSD-800) > 1e-6 {
		t.Fatalf("thin: got %+v (err %s), want tracking, rugged, liquidity 800", tt, strOrNil(tt.Error))
	}
	rs := rugReturns(t, st, thin.id)
	if r := rs["1d"]; !near(r.ReturnPct, 100) || !near(r.MaxGainPct, 200) {
		t.Fatalf("thin 1d: %+v", r)
	}
	if r := rs["3d"]; !isRugged(r, 200, 200) {
		t.Fatalf("thin 3d: %+v", r)
	}
	if os := ltState(t, st, thin.id); os.RugBlock != thin.at(2*ltDay) || os.RugLiquidityUSD == nil || math.Abs(*os.RugLiquidityUSD-400) > 1e-6 {
		t.Fatalf("thin: got rug block %d quote side %v, want %d and 400", os.RugBlock, rugF(os.RugLiquidityUSD), thin.at(2*ltDay))
	}

	ta, _ := st.GetTracking(ctx, alive.id)
	if ta.Status != TrackDone || ta.Rugged == nil || *ta.Rugged || ta.CurrentLiquidityUSD == nil || math.Abs(*ta.CurrentLiquidityUSD-1200) > 1e-6 {
		t.Fatalf("alive: got %+v (err %s), want done, not rugged, liquidity 1200", ta, strOrNil(ta.Error))
	}
	if r := rugReturns(t, st, alive.id)["30d"]; !near(r.ReturnPct, 0) || !near(r.MaxGainPct, 200) || r.PriceUSD == 0 {
		t.Fatalf("alive 30d: %+v", r)
	}
	if os := ltState(t, st, alive.id); os.RugBlock != 0 {
		t.Fatalf("alive rug block %d", os.RugBlock)
	}
}

// (e) Drained before the call: rugged from the start. Every horizon is -100%
// with no peak, and the later horizons need no log reads.
func TestTrackerRuggedAtEntry(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	c := rugCall{entry: time.Now().Add(-8 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	rp := newRugPool(f, "v3", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", c.eb)
	rp.trade(c.at(-10*time.Minute), 1e-6)
	rp.drain(c.at(-2 * time.Minute))
	rp.trade(c.at(time.Hour/2), 4e-6) // after the call: ignored

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Status != TrackTracking || tr.Rugged == nil || !*tr.Rugged || tr.EntryPriceUSD == nil || math.Abs(*tr.EntryPriceUSD-0.003) > 1e-12 {
		t.Fatalf("tracking row: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	rs := rugReturns(t, st, c.id)
	if len(rs) != 4 {
		t.Fatalf("returns %v", sortedKeys(rs))
	}
	for h, r := range rs {
		if !isRugged(r, 0, 0) {
			t.Fatalf("%s: %+v", h, r)
		}
	}
	if os := ltState(t, st, c.id); os.RugBlock != c.at(-2*time.Minute) || os.RunMaxQ != 1e-6 || os.ScanBlock != os.EntryBlock {
		t.Fatalf("state: rug %d max %v scan %d (no scan past the entry)", os.RugBlock, os.RunMaxQ, os.ScanBlock)
	}
	if hi := rugHigh(t, st, c.id); hi != 0 {
		t.Fatalf("candles stored after a rug at entry: high %v", hi)
	}
}

// (f) A call already done whose pool is drained after the last horizon: the
// latest pass finds the rug itself (rugged, liquidity 0, rug_block in the state,
// -100%) and leaves the horizons alone. (d) The pass's 1e6× backstop: a price
// far above the entry in a deep pool is skipped.
func TestLatestPassFindsRugAndSkipsImplausible(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	rugged := rugCall{entry: time.Now().Add(-35 * ltDay)}
	rugged.eb = f.blockAtTime(rugged.entry)
	jump := rugCall{entry: time.Now().Add(-36 * ltDay)}
	jump.eb = f.blockAtTime(jump.entry)
	rr := newRugPool(f, "v3", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", rugged.eb)
	rj := newRugPool(f, "v3", "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c5", jump.eb)
	for _, x := range []struct {
		rp *rugPool
		c  rugCall
	}{{rr, rugged}, {rj, jump}} {
		x.rp.trade(x.c.at(-2*time.Minute), 1e-6)
		x.rp.trade(x.c.at(30*time.Minute), 3e-6)
		x.rp.trade(x.c.at(10*ltDay), 2e-6)
	}
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(1)))) // $3000 of quote side at the end of tracking (stored as $6000): not rugged then
	rr.drain(rugged.at(32 * ltDay))
	rj.trade(jump.at(32*ltDay), 2.5) // 2.5e6 × the entry
	rj.trade(jump.at(33*ltDay), 2.2e-6)
	rj.trade(jump.at(34*ltDay), 3.0) // again, and the last trade: skipped

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, rugged.entry, rr.token))
	s.onChannelPost(postAt(2, jump.entry, rj.token))
	rugged.id, jump.id = *(<-s.queue).CallID, *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("processed %d", n)
	}
	for _, id := range []int{rugged.id, jump.id} {
		if tr, _ := st.GetTracking(ctx, id); tr.Status != TrackDone || tr.Rugged == nil || *tr.Rugged {
			t.Fatalf("call %d after tracking: %+v (err %s)", id, tr, strOrNil(tr.Error))
		}
	}
	before := ltSide(t, st, rugged.id)

	if res := s.refreshLatest(ctx, false); res.Refreshed != 2 || res.Failed != 0 {
		t.Fatalf("pass %+v", res)
	}
	lc := ltRead(t, st, rugged.id)
	tr, _ := st.GetTracking(ctx, rugged.id)
	if !ltNear(lc.Ret, -100) || !ltNear(lc.Price, 0) || tr.Rugged == nil || !*tr.Rugged || tr.CurrentLiquidityUSD == nil || *tr.CurrentLiquidityUSD != 0 {
		t.Fatalf("rugged: latest %v %v, row %+v", rugF(lc.Price), rugF(lc.Ret), tr)
	}
	if os := ltState(t, st, rugged.id); os.RugBlock != rugged.at(32*ltDay) || math.Abs(os.LatestPriceQ-2e-6) > 1e-15 {
		t.Fatalf("rugged state: rug %d latest price %v", os.RugBlock, os.LatestPriceQ)
	}
	after := ltSide(t, st, rugged.id)
	if after.Returns != before.Returns || after.Candles != before.Candles || !reflect.DeepEqual(rugWithout(after.State), rugWithout(before.State)) {
		t.Fatalf("the pass changed the horizon side")
	}

	// the jump: the latest price is the last plausible one (2.2e-6 WETH = $0.0066)
	lj := ltRead(t, st, jump.id)
	if !ltNear(lj.Price, 2.2e-6*ltETH) || !ltNear(lj.Ret, 120) {
		t.Fatalf("jump: latest %v %v", rugF(lj.Price), rugF(lj.Ret))
	}
	if tj, _ := st.GetTracking(ctx, jump.id); *tj.Rugged {
		t.Fatal("jump flagged rugged")
	}
}

// rugWithout drops the keys the latest pass may add for a rug.
func rugWithout(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != latestStateKeyRug && k != latestStateKeyRugLiq {
			out[k] = v
		}
	}
	return out
}

// (g) The pool's quote side already under the threshold at the call (not
// empty, just thin): rugged from the start by the USD check on the entry
// event. Every horizon is -100% with no peak, even though the pool trades
// higher afterwards. A pool with $600 of quote side at the call is not.
func TestTrackerRugThinAtEntry(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	thin := rugCall{entry: time.Now().Add(-8 * ltDay)}
	thin.eb = f.blockAtTime(thin.entry)
	alive := rugCall{entry: time.Now().Add(-8*ltDay - time.Hour)}
	alive.eb = f.blockAtTime(alive.entry)
	rt := newRugPool(f, "v2", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c2", thin.eb)
	ra := newRugPool(f, "v2", "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c5", alive.eb)
	rt.sync(thin.at(-2*time.Minute), 0.4/3, 0.4/3/1e-6) // price 1e-6 WETH, $400 of quote side
	ra.sync(alive.at(-2*time.Minute), 0.2, 0.2/1e-6)    // price 1e-6 WETH, $600 of quote side
	for _, x := range []struct {
		rp *rugPool
		c  rugCall
	}{{rt, thin}, {ra, alive}} {
		x.rp.trade(x.c.at(30*time.Minute), 3e-6) // deep again: ignored for the thin call
		x.rp.trade(x.c.at(50*time.Minute), 2e-6)
	}

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, thin.entry, rt.token))
	s.onChannelPost(postAt(2, alive.entry, ra.token))
	thin.id, alive.id = *(<-s.queue).CallID, *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("processed %d", n)
	}

	tt, _ := st.GetTracking(ctx, thin.id)
	if tt.Status != TrackTracking || tt.Rugged == nil || !*tt.Rugged || tt.CurrentLiquidityUSD == nil || math.Abs(*tt.CurrentLiquidityUSD-800) > 1e-6 ||
		tt.EntryPriceUSD == nil || math.Abs(*tt.EntryPriceUSD-0.003) > 1e-12 {
		t.Fatalf("thin: got %+v (err %s), want tracking, rugged, liquidity 800, entry 0.003", tt, strOrNil(tt.Error))
	}
	rs := rugReturns(t, st, thin.id)
	if len(rs) != 4 {
		t.Fatalf("thin returns %v", sortedKeys(rs))
	}
	for h, r := range rs {
		if !isRugged(r, 0, 0) {
			t.Fatalf("thin %s: got %+v, want -100%% with no peak", h, r)
		}
	}
	if os := ltState(t, st, thin.id); os.RugBlock == 0 || os.RugBlock != os.EntryBlock || os.RugLiquidityUSD == nil ||
		math.Abs(*os.RugLiquidityUSD-400) > 1e-6 || math.Abs(os.EntryLiqQ-0.4/3) > 1e-12 {
		t.Fatalf("thin state: got rug %d (entry %d) quote side %v entry liq %v, want the entry block, 400, %v",
			os.RugBlock, os.EntryBlock, rugF(os.RugLiquidityUSD), os.EntryLiqQ, 0.4/3)
	}

	ta, _ := st.GetTracking(ctx, alive.id)
	if ta.Status != TrackTracking || (ta.Rugged != nil && *ta.Rugged) {
		t.Fatalf("alive: got %+v (err %s), want tracking, not rugged", ta, strOrNil(ta.Error))
	}
	if r := rugReturns(t, st, alive.id)["1d"]; !near(r.ReturnPct, 100) || !near(r.MaxGainPct, 200) {
		t.Fatalf("alive 1d: got %+v, want +100%% with a +200%% peak", r)
	}
	if os := ltState(t, st, alive.id); os.RugBlock != 0 {
		t.Fatalf("alive rug block %d, want 0", os.RugBlock)
	}
}

// (h) A call already done whose v2 pool is thinned (not emptied) after the
// last horizon: the latest pass compares the Sync's quote side with the
// threshold at the quote's current USD price. $400 → rugged, -100%,
// current_liquidity_usd $800, rug_liq_usd $400 in the state.
func TestLatestPassRugThinV2(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	c := rugCall{entry: time.Now().Add(-35 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	rp := newRugPool(f, "v2", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c2", c.eb)
	rp.trade(c.at(-2*time.Minute), 1e-6)
	rp.trade(c.at(30*time.Minute), 3e-6)
	rp.trade(c.at(10*ltDay), 2e-6)
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(1)))) // $3000 of quote side at the end of tracking: not rugged then
	rp.sync(c.at(32*ltDay), 0.4/3, 0.4/3/2e-6)         // still 2e-6 WETH, but only $400 of quote side

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	if tr, _ := st.GetTracking(ctx, c.id); tr.Status != TrackDone || tr.Rugged == nil || *tr.Rugged ||
		tr.CurrentLiquidityUSD == nil || math.Abs(*tr.CurrentLiquidityUSD-6000) > 1e-6 {
		t.Fatalf("after tracking: got %+v (err %s), want done, not rugged, liquidity 6000", tr, strOrNil(tr.Error))
	}
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass %+v", res)
	}
	lc := ltRead(t, st, c.id)
	tr, _ := st.GetTracking(ctx, c.id)
	if !ltNear(lc.Ret, -100) || !ltNear(lc.Price, 0) || tr.Rugged == nil || !*tr.Rugged || tr.CurrentLiquidityUSD == nil ||
		math.Abs(*tr.CurrentLiquidityUSD-800) > 1e-6 {
		t.Fatalf("got latest %v %v, row %+v; want -100%%, price 0, rugged, liquidity 800", rugF(lc.Price), rugF(lc.Ret), tr)
	}
	if os := ltState(t, st, c.id); os.RugBlock != c.at(32*ltDay) || os.RugLiquidityUSD == nil || math.Abs(*os.RugLiquidityUSD-400) > 1e-6 {
		t.Fatalf("state: got rug %d quote side %v, want %d and 400", os.RugBlock, rugF(os.RugLiquidityUSD), c.at(32*ltDay))
	}
}

// (i) The end-of-tracking check (balanceOf) with the USD check off
// (SCOUT_RUG_LIQ_USD=0): an empty quote side still counts, a thin one does not.
func TestTrackerEndCheckEmptyPoolThresholdOff(t *testing.T) {
	for _, c := range []struct {
		name    string
		weth    float64
		wantRug bool
	}{
		{"empty", 0, true},
		{"thin $150", 0.05, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SCOUT_RUG_LIQ_USD", "0")
			st := testStore(t)
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			f := ltChain(t, 60*ltDay)
			rc := rugCall{entry: time.Now().Add(-35 * ltDay)}
			rc.eb = f.blockAtTime(rc.entry)
			rp := newRugPool(f, "v3", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", rc.eb)
			rp.trade(rc.at(-2*time.Minute), 1e-6)
			rp.trade(rc.at(30*time.Minute), 3e-6)
			rp.trade(rc.at(10*ltDay), 2e-6) // +100%: the 5%-of-entry rule does not apply
			f.constCall(tWETH, selBalanceOf, ret(w32(wei(c.weth))))

			s := ltScanner(t, st, f.srv.URL)
			s.onChannelPost(postAt(1, rc.entry, rp.token))
			rc.id = *(<-s.queue).CallID
			if n := s.trackDue(ctx, 50); n != 1 {
				t.Fatalf("processed %d", n)
			}
			tr, _ := st.GetTracking(ctx, rc.id)
			wantLiq := 2 * c.weth * ltETH
			if tr.Status != TrackDone || tr.Rugged == nil || *tr.Rugged != c.wantRug || tr.CurrentLiquidityUSD == nil ||
				math.Abs(*tr.CurrentLiquidityUSD-wantLiq) > 1e-6 {
				t.Fatalf("balanceOf %v WETH, threshold 0: got %+v (err %s), want done, rugged %v, liquidity %v",
					c.weth, tr, strOrNil(tr.Error), c.wantRug, wantLiq)
			}
			if os := ltState(t, st, rc.id); (os.RugBlock > 0) != c.wantRug {
				t.Fatalf("balanceOf %v WETH, threshold 0: got rug block %d, want rugged %v", c.weth, os.RugBlock, c.wantRug)
			}
		})
	}
}

// (j) A run that finds every horizon already computed (an earlier run was
// interrupted before the row was finished) values the end-of-tracking
// balanceOf at the quote's current USD price, not at 1: 0.2 WETH is $600 of
// quote side, not $0.20, so the call is not rugged.
func TestTrackerEndCheckAfterInterruptedRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	c := rugCall{entry: time.Now().Add(-35 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	rp := newRugPool(f, "v3", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", c.eb)
	rp.trade(c.at(-2*time.Minute), 1e-6)
	rp.trade(c.at(30*time.Minute), 3e-6)
	rp.trade(c.at(10*ltDay), 2e-6)
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(0.2)))) // $600 of quote side

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	// As if the run had stopped after the last horizon: due again, every
	// horizon in the state already done.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'tracking', rugged = NULL,
		current_liquidity_usd = NULL, next_check_at = now() - interval '1 minute' WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("second run processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Status != TrackDone || tr.Rugged == nil || *tr.Rugged || tr.CurrentLiquidityUSD == nil || math.Abs(*tr.CurrentLiquidityUSD-1200) > 1e-6 {
		t.Fatalf("after the second run: got %+v (err %s), want done, not rugged, liquidity 1200", tr, strOrNil(tr.Error))
	}
	if os := ltState(t, st, c.id); os.RugBlock != 0 {
		t.Fatalf("rug block %d, want 0", os.RugBlock)
	}
}
