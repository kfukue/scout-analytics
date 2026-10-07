package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// Checkpoints within a horizon segment (segmentPieceEnd, saveCheckpoint).

// A horizon segment scanned in pieces, interrupted in the middle and resumed,
// ends exactly like an uninterrupted run: the same candles (prices and event
// counts: no trade counted twice, none lost), the same extremes and horizon
// results. Two calls on two pools with the same trades at the same times:
// A is tracked in one run, B is stopped after a few of its 1d-segment log
// ranges (the checkpoint is in the database before the run ends) and then
// tracked again.
func TestTrackerSegmentCheckpointResume(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	old := segmentPieceChunks
	segmentPieceChunks = 4
	t.Cleanup(func() { segmentPieceChunks = old })
	t.Setenv("SCOUT_RPC_LOG_CHUNK", "20000") // ≈ 33 minutes: pieces of ≈ 2 hours
	t.Setenv("SCOUT_RPC_PARALLEL", "1")
	const (
		tokA, poolA = "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
		tokB, poolB = "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c4"
	)
	f := ltChain(t, 20*ltDay)
	f.ltPool(tokA, poolA, tWETH)
	f.ltPool(tokB, poolB, tWETH)
	c := rugCall{entry: time.Now().Add(-2 * ltDay).Truncate(time.Second)}
	c.eb = f.blockAtTime(c.entry)
	trades := 0
	for _, pool := range []struct{ tok, addr string }{{tokA, poolA}, {tokB, poolB}} {
		f.ltTrade(pool.tok, pool.addr, c.at(-time.Minute), 1e-3) // entry
		trades = 0
		for i := 1; i <= 60; i++ { // every 23 minutes for 23 hours, a zigzag
			p := 1e-3 * (1 + float64(i%7)/3 + float64(i)/40)
			f.ltTrade(pool.tok, pool.addr, c.at(time.Duration(i)*23*time.Minute), p)
			trades++
		}
	}
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, tokA))
	idA := *(<-s.queue).CallID
	s.onChannelPost(postAt(2, c.entry, tokB))
	idB := *(<-s.queue).CallID
	row := func(id int) ScoutCallTracking {
		t.Helper()
		tr, err := st.GetTracking(ctx, id)
		if err != nil || tr == nil {
			t.Fatalf("tracking %d: %v", id, err)
		}
		return *tr
	}

	// A: one uninterrupted run.
	ra := row(idA)
	s.trackOneOnchain(ctx, &ra, "")

	// B: stopped after its 6th log range inside the 1d segment.
	oneH := c.at(time.Hour)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	n, atStop := 0, onchainState{}
	f.setLogsHook(func(addr string, from, _ uint64) string {
		if addr != poolB || from <= oneH+1 {
			return ""
		}
		mu.Lock()
		defer mu.Unlock()
		if n++; n == 6 {
			atStop = ltState(t, st, idB) // what a kill at this moment would leave
			cancel()
		}
		return ""
	})
	rb := row(idB)
	s.trackOneOnchain(cctx, &rb, "")
	f.setLogsHook(nil)
	mu.Lock()
	stopped := atStop
	mu.Unlock()
	dayBlock := c.at(24 * time.Hour)
	if stopped.ScanBlock <= oneH+1 || stopped.ScanBlock >= dayBlock || stopped.Done["1d"] {
		t.Fatalf("B at the stop: scan block %d (1h at %d, 1d at %d), done %v; want a checkpoint inside the 1d segment",
			stopped.ScanBlock, oneH, dayBlock, stopped.Done)
	}
	if b := row(idB); b.Status != TrackPending || b.Attempts != 0 {
		t.Fatalf("B after the stop: status %s, attempts %d; want pending, 0 (an interrupted run changes neither)", b.Status, b.Attempts)
	}
	// resumed
	rb = row(idB)
	s.trackOneOnchain(ctx, &rb, "")

	// The same candles, events included.
	candles := func(id int) map[string]candleRow {
		t.Helper()
		out := map[string]candleRow{}
		for _, iv := range []int{candleFineS, candleCoarseS} {
			cs, err := st.CandlesForCall(ctx, id, iv)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range cs {
				out[fmt.Sprintf("%d@%s", iv, k.Start.UTC().Format(time.RFC3339))] = k
			}
		}
		return out
	}
	ca, cb := candles(idA), candles(idB)
	if len(ca) == 0 || len(ca) != len(cb) {
		t.Fatalf("candles: A has %d, B (interrupted, resumed) %d; want the same, > 0", len(ca), len(cb))
	}
	events := 0
	for k, a := range ca {
		b, ok := cb[k]
		if !ok || !relNear(a.O, b.O) || !relNear(a.H, b.H) || !relNear(a.L, b.L) || !relNear(a.C, b.C) || a.Events != b.Events {
			t.Errorf("candle %s: A %+v, B %+v; want the same", k, a, b)
		}
		if a.IntervalS == candleCoarseS {
			events += a.Events
		}
	}
	if want := trades; events != want { // the entry trade is before the call: no candle
		t.Errorf("hourly candle events of A: got %d, want %d (each trade after the call once)", events, want)
	}
	// The same state and horizon results.
	sa, sb := ltState(t, st, idA), ltState(t, st, idB)
	for _, v := range []struct {
		name string
		a, b float64
	}{
		{"run max", sa.RunMaxU, sb.RunMaxU}, {"run min", sa.RunMinU, sb.RunMinU},
		{"run max late", sa.RunMaxLateU, sb.RunMaxLateU}, {"run min late", sa.RunMinLateU, sb.RunMinLateU},
		{"run max q", sa.RunMaxQ, sb.RunMaxQ}, {"run min q", sa.RunMinQ, sb.RunMinQ},
		{"last price q", sa.LastPriceQ, sb.LastPriceQ}, {"scan block", float64(sa.ScanBlock), float64(sb.ScanBlock)},
	} {
		if !relNear(v.a, v.b) || v.a == 0 {
			t.Errorf("%s: A %v, B %v; want the same, non-zero", v.name, v.a, v.b)
		}
	}
	retA, retB := rugReturns(t, st, idA), rugReturns(t, st, idB)
	for _, h := range []string{"1h", "1d"} {
		a, b := retA[h], retB[h]
		if a.Status != "done" || b.Status != "done" || !relNear(a.PriceUSD, b.PriceUSD) || !relNear(a.MaxPriceUSD, b.MaxPriceUSD) ||
			!relNear(a.MinPriceUSD, b.MinPriceUSD) || math.Abs(a.ReturnPct-b.ReturnPct) > 1e-9 {
			t.Errorf("%s: A %+v, B %+v; want the same", h, a, b)
		}
	}
}
