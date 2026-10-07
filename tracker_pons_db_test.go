package main

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pons V2 tokens end to end with the database: tracked on the bonding curve,
// then on the v4 pool after the graduation (horizons, latest pass, rugs).
// ETH = $3000 (ltChain); USDG = $1.
// ---------------------------------------------------------------------------

const (
	tPonsCurve2 = "0x00000000000000000000000000000000000c0e02"
	tPonsCurve3 = "0x00000000000000000000000000000000000c0e03"
)

func relNear(got, want float64) bool { return math.Abs(got-want) <= 1e-6*math.Abs(want) }

// Called on the curve; the curve closes after the 1d horizon and the v4 pool
// opens 2 h later: 1h and 1d come from the curve, 3d and 7d from the v4 pool,
// the peak and the candles run on across the switch, nothing is a rug, and the
// latest pass reads the v4 pool.
func TestTrackerPonsCurveThenV4(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	c := rugCall{entry: time.Now().Add(-8 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, zeroAddr, 18, 1.5, 1e9, false)
	fp.buy(c.at(-5*time.Minute), e18(0.2))
	entry := fp.buy(c.at(-1*time.Minute), e18(0.2))
	late := fp.buy(c.at(30*time.Second), e18(0.3))
	peak1d := fp.buy(c.at(30*time.Minute), e18(2))
	at1h := fp.sell(c.at(50*time.Minute), e18(5e6))
	at1d := fp.buy(c.at(20*time.Hour), e18(1))
	peak1d = math.Max(peak1d, at1d)
	closeB := c.at(30 * time.Hour)
	curvePeak := math.Max(peak1d, fp.buy(closeB, e18(1))) // the crossing buy
	fp.close(closeB)
	grad := c.at(32 * time.Hour)
	fp.graduate(grad, defaultPonsHook)
	fp.decoySwap(grad + 5)
	fp.swap(c.at(40*time.Hour), 2e-8, 18)
	fp.swap(c.at(60*time.Hour), 9e-8, 18) // the peak, on the v4 pool
	fp.swap(c.at(70*time.Hour), 4e-8, 18) // the 3d price
	fp.swap(c.at(5*ltDay), 3e-8, 18)      // the 7d price

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, fp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Status != TrackTracking || tr.Error != nil || tr.Rugged != nil || *tr.PoolDex != "pons-curve" || *tr.PoolAddress != tPonsCurve ||
		*tr.EntryPriceSource != "onchain-pons" || *tr.PriceUnit != "usd" || !relNear(*tr.EntryPriceUSD, entry*ltETH) {
		t.Fatalf("tracking row: got %+v (err %s), want tracking on pons-curve %s, entry $%v", tr, strOrNil(tr.Error), tPonsCurve, entry*ltETH)
	}
	rs := rugReturns(t, st, c.id)
	if len(rs) != 4 {
		t.Fatalf("returns %v, want 1h 1d 3d 7d", sortedKeys(rs))
	}
	peak := math.Max(curvePeak, 9e-8)
	for _, w := range []struct {
		h     string
		price float64
	}{{"1h", at1h}, {"1d", at1d}, {"3d", 4e-8}, {"7d", 3e-8}} {
		r := rs[w.h]
		if !relNear(r.PriceUSD, w.price*ltETH) || r.ReturnLatePct == nil || !relNear(*r.ReturnLatePct, (w.price/late-1)*100) {
			t.Fatalf("%s: got price %v, want %v (%+v)", w.h, r.PriceUSD, w.price*ltETH, r)
		}
	}
	if r := rs["1d"]; !relNear(r.MaxPriceUSD, peak1d*ltETH) {
		t.Fatalf("1d peak: got %v, want the curve peak %v", r.MaxPriceUSD, peak1d*ltETH)
	}
	if r := rs["7d"]; !relNear(r.MaxPriceUSD, peak*ltETH) || !relNear(r.MinPriceUSD, math.Min(entry, 2e-8)*ltETH) {
		t.Fatalf("7d: got peak %v low %v, want %v %v", r.MaxPriceUSD, r.MinPriceUSD, peak*ltETH, math.Min(entry, 2e-8)*ltETH)
	}
	os := ltState(t, st, c.id)
	if os.Kind != "pons" || os.PonsDone != closeB || os.PonsGrad != grad || os.PoolID != fp.poolID || os.RugBlock != 0 || os.V != onchainStateVersion {
		t.Fatalf("state: got %+v, want closed %d, pool %s from %d", os, closeB, fp.poolID, grad)
	}
	if hi := rugHigh(t, st, c.id); !relNear(hi, peak*ltETH) {
		t.Fatalf("candle high: got %v, want %v (decoy pool or double count?)", hi, peak*ltETH)
	}

	// latest pass: a v4 swap after the 7d horizon
	fp.swap(c.at(7*ltDay+time.Hour), 6e-8, 18)
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("latest pass: %+v", res)
	}
	if lc := ltRead(t, st, c.id); !ltNear(lc.Price, 6e-8*ltETH) || !ltNear(lc.Ret, (6e-8/late-1)*100) {
		t.Fatalf("latest: got %v %v, want $%v", rugF(lc.Price), rugF(lc.Ret), 6e-8*ltETH)
	}
}

// A curve quoted in USDG with a tiny real reserve is not rugged (also not by
// the end-of-tracking liquidity check); a token whose curve price falls under
// 5% of the entry is rugged by the 5% rule; a launch swept without a pool
// keeps its last curve price.
func TestTrackerPonsCurveRugRules(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	f.addToken(tUSDG, 6, "USDG")
	f.constCall(tUSDG, selBalanceOf, ret(w32(big.NewInt(30e6)))) // every curve holds $30 of USDG
	usd := func(x float64) *big.Int { return scaleUnits(x, 6) }

	tiny := rugCall{entry: time.Now().Add(-35 * ltDay)}
	tiny.eb = f.blockAtTime(tiny.entry)
	ft := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, tUSDG, 6, 5000, 1e9, false)
	ft.buy(tiny.at(-time.Minute), usd(20))
	ft.buy(tiny.at(time.Hour/2), usd(10))
	tinyLast := ft.sell(tiny.at(10*ltDay), e18(1e5)) * 1e12 // raw → USDG per token

	dump := rugCall{entry: time.Now().Add(-36 * ltDay)}
	dump.eb = f.blockAtTime(dump.entry)
	fd := newFakePons(f, "0x5555555555555555555555555555555555555555", tPonsCurve2, tUSDG, 6, 5000, 1e9, false)
	bought := new(big.Int).Set(fd.t)
	fd.buy(dump.at(-2*time.Minute), usd(45000))
	bought.Sub(bought, fd.t)
	fd.buy(dump.at(-time.Minute), usd(1)) // the entry: near the top of the curve
	// Dumped back to near the start. A trade's price is its average, so the
	// dump itself is priced high; the small buy after it shows the bottom.
	fd.sell(dump.at(2*ltDay), new(big.Int).Div(new(big.Int).Mul(bought, big.NewInt(99)), big.NewInt(100)))
	fd.buy(dump.at(3*ltDay), usd(1))

	swept := rugCall{entry: time.Now().Add(-37 * ltDay)}
	swept.eb = f.blockAtTime(swept.entry)
	fs := newFakePons(f, "0x6666666666666666666666666666666666666666", tPonsCurve3, tUSDG, 6, 5000, 1e9, false)
	fs.buy(swept.at(-time.Minute), usd(100))
	sweptLast := fs.buy(swept.at(3*ltDay), usd(500)) * 1e12
	fs.close(swept.at(3 * ltDay))

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, tiny.entry, ft.token))
	s.onChannelPost(postAt(2, dump.entry, fd.token))
	s.onChannelPost(postAt(3, swept.entry, fs.token))
	tiny.id, dump.id, swept.id = *(<-s.queue).CallID, *(<-s.queue).CallID, *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 3 {
		t.Fatalf("processed %d", n)
	}

	for _, x := range []struct {
		name string
		c    rugCall
		last float64
	}{{"tiny reserve", tiny, tinyLast}, {"swept, no pool", swept, sweptLast}} {
		tr, _ := st.GetTracking(ctx, x.c.id)
		if tr.Status != TrackDone || tr.Error != nil || tr.Rugged == nil || *tr.Rugged || tr.CurrentLiquidityUSD != nil {
			t.Fatalf("%s: got %+v (err %s), want done, not rugged, no liquidity measured", x.name, tr, strOrNil(tr.Error))
		}
		if r := rugReturns(t, st, x.c.id)["30d"]; !relNear(r.PriceUSD, x.last) {
			t.Fatalf("%s 30d: got %v, want the last curve price %v", x.name, r.PriceUSD, x.last)
		}
		if os := ltState(t, st, x.c.id); os.RugBlock != 0 || os.Kind != "pons" {
			t.Fatalf("%s: state %+v", x.name, os)
		}
	}
	td, _ := st.GetTracking(ctx, dump.id)
	if td.Status != TrackDone || td.Rugged == nil || !*td.Rugged || *td.CurrentPriceUSD >= *td.EntryPriceUSD*0.05 || ltState(t, st, dump.id).RugBlock != 0 {
		t.Fatalf("dump: got %+v (err %s), want done and rugged by the 5%% rule (not by liquidity)", td, strOrNil(td.Error))
	}
	if os := ltState(t, st, swept.id); os.PonsDone != swept.at(3*ltDay) || os.PoolID != "" || os.PonsSeen == 0 {
		t.Fatalf("swept: state %+v", os)
	}
}

// The curve closes after the last horizon: the latest pass follows the token
// to its v4 pool, stores what it found, and still follows it when its cursor
// is past the graduation and the state no longer knows it: on an archive node,
// and on a full node (no historical state) where the graduation is beyond the
// price look-back and is found in the event logs, without the PoolManager-wide
// swap scan.
func TestTrackerPonsGraduationAfterLastHorizon(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "full node"}[full], func(t *testing.T) {
			testTrackerPonsGraduationAfterLastHorizon(t, full)
		})
	}
}

func testTrackerPonsGraduationAfterLastHorizon(t *testing.T, fullNode bool) {
	noHeadCache(t) // the test moves the chain's head (advance)
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	c := rugCall{entry: time.Now().Add(-40 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, zeroAddr, 18, 1.5, 1e9, false)
	fp.buy(c.at(-time.Minute), e18(0.2))
	late := fp.buy(c.at(30*time.Second), e18(0.2))
	at30 := fp.buy(c.at(20*ltDay), e18(0.5))
	closeB := c.at(32 * ltDay)
	fp.buy(closeB, e18(3))
	fp.close(closeB)
	grad := closeB + 50_000
	fp.graduate(grad, defaultPonsHook)
	fp.swap(c.at(35*ltDay), 5e-8, 18)

	if fullNode {
		t.Setenv("SCOUT_PRICE_LOOKBACK_BLOCKS", ponsFarLookback) // the graduation is far beyond it
	}
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, fp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Status != TrackDone || tr.Rugged == nil || *tr.Rugged {
		t.Fatalf("tracking row: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	if r := rugReturns(t, st, c.id)["30d"]; !relNear(r.PriceUSD, at30*ltETH) {
		t.Fatalf("30d: got %v, want the curve price %v", r.PriceUSD, at30*ltETH)
	}
	before := ltState(t, st, c.id)
	if before.PonsDone != 0 && before.PonsDone != closeB {
		t.Fatalf("state after tracking: closing block %d", before.PonsDone)
	}

	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("latest pass: %+v", res)
	}
	if lc := ltRead(t, st, c.id); !ltNear(lc.Price, 5e-8*ltETH) || !ltNear(lc.Ret, (5e-8/late-1)*100) {
		t.Fatalf("latest: got %v %v, want the v4 price $%v", rugF(lc.Price), rugF(lc.Ret), 5e-8*ltETH)
	}
	os := ltState(t, st, c.id)
	if os.PonsDone != closeB || os.PonsGrad != grad || os.PoolID != fp.poolID || os.TokenIs0 != fp.tokenIs0 || os.Hook != defaultPonsHook ||
		os.ScanBlock != before.ScanBlock || os.LastPriceQ != before.LastPriceQ {
		t.Fatalf("state after the pass: got %+v, want the graduation stored and the horizon side kept", os)
	}

	// The state loses the graduation (e.g. overwritten), the cursor is past it.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = onchain - 'pons_curve_done' - 'pons_grad_block'
		- 'pons_grad_seen' - 'pool_id' - 'token_is_0' - 'pons_hook' WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	fp.swap(f.head()+100, 8e-8, 18)
	f.advance(9000)
	ltMakeDue(t, st, "25 hours")
	if fullNode {
		f.mu.Lock()
		f.fullNode = true // from here on, no historical state
		f.mu.Unlock()
	}
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("second pass: %+v", res)
	}
	if lc := ltRead(t, st, c.id); !ltNear(lc.Price, 8e-8*ltETH) {
		t.Fatalf("latest after losing the graduation: got %v, want $%v; state %+v head %d", rugF(lc.Price), 8e-8*ltETH, ltState(t, st, c.id), f.head())
	}
	if os := ltState(t, st, c.id); os.PonsDone != closeB || os.PoolID != fp.poolID || os.PonsGrad != grad {
		t.Fatalf("state after the second pass: got closed %d pool %s from %d, want %d %s %d", os.PonsDone, os.PoolID, os.PonsGrad, closeB, fp.poolID, grad)
	}
	if n := f.pmSwapScans(); n != 0 {
		t.Fatalf("got %d PoolManager-wide swap scans, want 0", n)
	}
}

// After the graduation the v4 pool is checked like any other: drained → rugged
// from that block, later horizons -100% with the peak from before.
func TestTrackerPonsV4DrainRugged(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	c := rugCall{entry: time.Now().Add(-8 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, zeroAddr, 18, 1.5, 1e9, false)
	entry := fp.buy(c.at(-time.Minute), e18(0.2))
	fp.buy(c.at(30*time.Minute), e18(1))
	closeB := c.at(10 * time.Hour)
	fp.buy(closeB, e18(4))
	fp.close(closeB)
	fp.graduate(closeB+100, defaultPonsHook)
	fp.swap(c.at(12*time.Hour), 9e-8, 18)
	fp.swap(c.at(20*time.Hour), 6e-8, 18)
	fp.swapL(c.at(2*ltDay), 6e-8, 18, big.NewInt(0), -maxTick) // liquidity gone
	fp.swap(c.at(4*ltDay), 9e-7, 18)                           // after the rug: ignored

	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, fp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Rugged == nil || !*tr.Rugged || tr.Status != TrackTracking {
		t.Fatalf("tracking row: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	rs := rugReturns(t, st, c.id)
	if r := rs["1d"]; !relNear(r.PriceUSD, 6e-8*ltETH) {
		t.Fatalf("1d: %+v", r)
	}
	peak := (9e-8/entry - 1) * 100
	for _, h := range []string{"3d", "7d"} {
		if r := rs[h]; r.PriceUSD != 0 || r.ReturnPct != -100 || !relNear(r.MaxGainPct, peak) {
			t.Fatalf("%s: got %+v, want -100%% with the peak %v%% from before the rug", h, r, peak)
		}
	}
	if os := ltState(t, st, c.id); os.RugBlock != c.at(2*ltDay) {
		t.Fatalf("rug block: got %d, want %d", os.RugBlock, c.at(2*ltDay))
	}
}
