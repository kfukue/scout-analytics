package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Timing columns and rug times end to end with the database (fake chain).
// ---------------------------------------------------------------------------

// tmRow is a horizon's stored timing.
type tmRow struct {
	Peak, First, Above, Fall *int
	Censored                 *bool
	At                       *time.Time
}

func tmRead(t *testing.T, st *ScoutStore, id int, horizon string) tmRow {
	t.Helper()
	var r tmRow
	if err := st.Pool.QueryRow(context.Background(), `SELECT peak_late_after_s, first_2x_after_s, above_2x_s,
		fall_below_2x_after_s, above_2x_censored, timing_at FROM scout_call_returns WHERE call_id = $1 AND horizon = $2`,
		id, horizon).Scan(&r.Peak, &r.First, &r.Above, &r.Fall, &r.Censored, &r.At); err != nil {
		t.Fatalf("timing of call %d +%s: %v", id, horizon, err)
	}
	return r
}

// same: the timing values (not when they were computed).
func (a tmRow) same(b tmRow) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.Peak, b.Peak) && eq(a.First, b.First) && eq(a.Above, b.Above) && eq(a.Fall, b.Fall) &&
		(a.Censored == nil) == (b.Censored == nil) && (a.Censored == nil || *a.Censored == *b.Censored)
}

func (a tmRow) String() string {
	return tmStr(horizonTiming{PeakLateAfterS: a.Peak, First2xAfterS: a.First, Above2xS: derefInt(a.Above),
		FallBelow2xAfterS: a.Fall, Censored: a.Censored != nil && *a.Censored}) + func() string {
		if a.At == nil {
			return " (timing_at NULL)"
		}
		return ""
	}()
}

func derefInt(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func tmRugAt(t *testing.T, st *ScoutStore, id int) (*time.Time, *string) {
	t.Helper()
	var at *time.Time
	var kind *string
	if err := st.Pool.QueryRow(context.Background(), `SELECT rug_at, rug_at_kind FROM scout_call_tracking WHERE call_id = $1`, id).
		Scan(&at, &kind); err != nil {
		t.Fatal(err)
	}
	return at, kind
}

// tmPumpCall: entry and late entry at 1e-6 WETH ($0.003), a peak of 3e-6
// (+200%) 30 minutes after the call, then 1.5e-6 (+50%, under 2×) at 50
// minutes. drain: the pool is drained 2 days after the call.
func tmPumpCall(f *fakeChain, token, pool string, age time.Duration, drain bool) (rugCall, *rugPool) {
	c := rugCall{entry: time.Now().Add(-age)}
	c.eb = f.blockAtTime(c.entry)
	rp := newRugPool(f, "v3", token, pool, c.eb)
	rp.trade(c.at(-2*time.Minute), 1e-6)
	rp.trade(c.at(30*time.Second), 1e-6)
	rp.trade(c.at(30*time.Minute), 3e-6)
	rp.trade(c.at(50*time.Minute), 1.5e-6)
	if drain {
		rp.drain(c.at(2 * ltDay))
	}
	return c, rp
}

// tmCheckPump: the timing of tmPumpCall's horizons: first 2× and peak in the
// 5-minute bucket holding +30m, under 2× at the end of the bucket holding
// +50m, held (on closes) from the end of the first 2× bucket to it, not
// censored; the same for every horizon (the fall comes before the first
// horizon's end and before the rug).
func tmCheckPump(t *testing.T, st *ScoutStore, c rugCall, horizons []string) tmRow {
	t.Helper()
	first := tmRead(t, st, c.id, horizons[0])
	if first.At == nil || first.First == nil || first.Peak == nil || first.Fall == nil || first.Above == nil || first.Censored == nil {
		t.Fatalf("call %d +%s: got %s, want every column set", c.id, horizons[0], first)
	}
	if *first.First < 30*60-300 || *first.First > 30*60 || *first.Peak != *first.First {
		t.Errorf("call %d: got first 2x %d s, peak %d s, want both in the 5-minute bucket holding 1800 s", c.id, *first.First, *first.Peak)
	}
	if *first.Fall <= 50*60 || *first.Fall > 50*60+300 || *first.Above != *first.Fall-*first.First-300 || *first.Censored {
		t.Errorf("call %d: got %s, want the fall at the end of the bucket holding 3000 s, held from the end of the first 2x bucket to it, not censored", c.id, first)
	}
	for _, h := range horizons[1:] {
		if got := tmRead(t, st, c.id, h); got.At == nil || !got.same(first) {
			t.Errorf("call %d +%s: got %s, want %s (as +%s)", c.id, h, got, first, horizons[0])
		}
	}
	return first
}

// The tracker stores the timing with each horizon and the exact rug time
// (kind event); a call tracked again from scratch drops both and stores them
// anew.
func TestTrackerStoresTimingAndRugTime(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	c, rp := tmPumpCall(f, "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", 8*ltDay, true)
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	horizons := []string{"1h", "1d", "3d", "7d"}
	want := tmCheckPump(t, st, c, horizons)
	rugWant := time.Unix(f.timeOf(c.at(2*ltDay)), 0).UTC()
	if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(rugWant) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("rug: got %v %v, want %v event", at, strOrNil(kind), rugWant)
	}

	// Tracked again from scratch (no on-chain state): the stale rug time and
	// timing are replaced.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = NULL, status = 'pending', next_check_at = now(),
		rug_at = '2000-01-01', rug_at_kind = 'detected' WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_returns SET above_2x_s = -1, first_2x_after_s = NULL WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	for _, h := range horizons {
		if got := tmRead(t, st, c.id, h); !got.same(want) {
			t.Errorf("re-tracked +%s: got %s, want %s", h, got, want)
		}
	}
	if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(rugWant) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("re-tracked rug: got %v %v, want %v event", at, strOrNil(kind), rugWant)
	}
	if onchainStateVersion != 2 {
		t.Fatalf("onchainStateVersion = %d, want 2 (timing must not make the tracker redo history)", onchainStateVersion)
	}
}

// The rug block's time cannot be read: the tracker asks the node once in the
// run (not once per rugged horizon), stores the rug and the horizons anyway
// (the rugged ones without timing), and -backfill-timing fills both in later.
func TestTrackerRugTimeLookupFailsOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	c, rp := tmPumpCall(f, "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", 8*ltDay, true)
	rugBlock := c.at(2 * ltDay)
	asked := 0
	f.mu.Lock()
	f.blockErr = func(n uint64) string {
		if n != rugBlock {
			return ""
		}
		asked++
		return "header not found"
	}
	f.mu.Unlock()
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	f.mu.Lock()
	got := asked
	f.blockErr = nil
	f.mu.Unlock()
	if got != 1 {
		t.Errorf("rug block %d: got %d time lookup(s) in the run, want 1", rugBlock, got)
	}
	if at, _ := tmRugAt(t, st, c.id); at != nil {
		t.Fatalf("got rug time %v, want none (lookup failed)", at)
	}
	if os := ltState(t, st, c.id); os.RugBlock != rugBlock {
		t.Fatalf("stored rug block %d, want %d", os.RugBlock, rugBlock)
	}
	for _, h := range []string{"1h", "1d"} {
		if got := tmRead(t, st, c.id, h); got.At == nil {
			t.Errorf("+%s (before the rug): got %s, want the timing", h, got)
		}
	}
	for _, h := range []string{"3d", "7d"} {
		if got := tmRead(t, st, c.id, h); got.At != nil {
			t.Errorf("+%s (rugged): got %s, want timing_at NULL", h, got)
		}
	}

	stats, err := runTimingBackfill(ctx, s, &bytes.Buffer{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RugStored != 1 || stats.Written != 2 {
		t.Fatalf("backfill: got %+v, want the rug time and the 2 rugged horizons", stats)
	}
	tmCheckPump(t, st, c, []string{"1h", "1d", "3d", "7d"})
}

// UpsertReturn writes the timing in the INSERT and in the ON CONFLICT update:
// a result without timing clears what an earlier one stored.
func TestUpsertReturnTiming(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id := mustCall(t, st)
	h := horizon{Name: "1h", Dur: time.Hour}
	g := 150.0
	r := horizonResult{Horizon: "1h", DueAt: time.Now().UTC(), Status: "done", PriceUSD: 1, MaxGainLatePct: &g}
	r.setTiming(horizonTiming{PeakLateAfterS: ip(120), First2xAfterS: ip(60), Above2xS: 300, FallBelow2xAfterS: ip(360)}, time.Now())
	if err := st.UpsertReturn(ctx, id, h, r); err != nil {
		t.Fatal(err)
	}
	if got := tmRead(t, st, id, "1h"); got.At == nil || derefInt(got.Above) != 300 || derefInt(got.First) != 60 || got.Censored == nil || *got.Censored {
		t.Fatalf("after insert: got %s, want above 300, first 60, censored false", got)
	}
	r.PeakLateAfterS, r.First2xAfterS, r.Above2xS, r.FallBelow2xAfterS, r.Above2xCensored, r.TimingAt = nil, nil, nil, nil, nil, nil
	if err := st.UpsertReturn(ctx, id, h, r); err != nil {
		t.Fatal(err)
	}
	if got := tmRead(t, st, id, "1h"); got.At != nil || got.Above != nil || got.First != nil || got.Peak != nil || got.Fall != nil || got.Censored != nil {
		t.Fatalf("after update without timing: got %s, want every timing column NULL", got)
	}
}

// mustCall stores one call with a tracking row and returns its id.
func mustCall(t *testing.T, st *ScoutStore) int {
	t.Helper()
	ctx := context.Background()
	var id int
	if err := st.Pool.QueryRow(ctx, `INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date,
		contract_address, chain, status, created_by, updated_by)
		VALUES (gen_random_uuid(), 1, 'c', 1, now(), '0xabc', 'evm', 'scanned', 't', 't') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTracking(ctx, id, "0xabc", time.Now(), 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

// Rug kinds: drained before the call (at_call, its time before the post), by
// the end-of-tracking check (detected, the check's time) and by the latest
// pass (event, the rug block's time).
func TestRugAtKinds(t *testing.T) {
	t.Run("at_call", func(t *testing.T) {
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
		s := ltScanner(t, st, f.srv.URL)
		s.onChannelPost(postAt(1, c.entry, rp.token))
		c.id = *(<-s.queue).CallID
		if n := s.trackDue(ctx, 50); n != 1 {
			t.Fatalf("processed %d", n)
		}
		want := time.Unix(f.timeOf(c.at(-2*time.Minute)), 0).UTC()
		if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(want) || kind == nil || *kind != rugAtAtCall {
			t.Fatalf("got %v %v, want %v at_call", at, strOrNil(kind), want)
		}
		for _, h := range []string{"1h", "1d", "3d", "7d"} {
			got := tmRead(t, st, c.id, h)
			if got.At == nil || derefInt(got.Peak) != 0 || derefInt(got.Above) != 0 || got.First != nil || got.Fall != nil {
				t.Errorf("+%s: got %s, want peak 0, held 0, never 2x", h, got)
			}
		}
	})
	t.Run("at_call before any horizon is due", func(t *testing.T) {
		st := testStore(t)
		ctx := context.Background()
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		f := ltChain(t, 20*ltDay)
		c := rugCall{entry: time.Now().Add(-30 * time.Minute)}
		c.eb = f.blockAtTime(c.entry)
		rp := newRugPool(f, "v3", "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", c.eb)
		rp.trade(c.at(-10*time.Minute), 1e-6)
		rp.drain(c.at(-2 * time.Minute))
		s := ltScanner(t, st, f.srv.URL)
		s.onChannelPost(postAt(1, c.entry, rp.token))
		c.id = *(<-s.queue).CallID
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() WHERE call_id = $1`, c.id); err != nil {
			t.Fatal(err)
		}
		if n := s.trackDue(ctx, 50); n != 1 {
			t.Fatalf("processed %d", n)
		}
		if rs := rugReturns(t, st, c.id); len(rs) != 0 {
			t.Fatalf("got horizons %v, want none due yet", sortedKeys(rs))
		}
		want := time.Unix(f.timeOf(c.at(-2*time.Minute)), 0).UTC()
		if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(want) || kind == nil || *kind != rugAtAtCall {
			t.Fatalf("got %v %v, want %v at_call", at, strOrNil(kind), want)
		}
	})
	t.Run("detected", func(t *testing.T) {
		t.Setenv("SCOUT_RUG_LIQ_USD", "0")
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
		f.constCall(tWETH, selBalanceOf, ret(w32(wei(0)))) // empty at the end of tracking
		s := ltScanner(t, st, f.srv.URL)
		s.onChannelPost(postAt(1, c.entry, rp.token))
		c.id = *(<-s.queue).CallID
		before := time.Now().Add(-time.Second)
		if n := s.trackDue(ctx, 50); n != 1 {
			t.Fatalf("processed %d", n)
		}
		after := time.Now().Add(time.Second)
		at, kind := tmRugAt(t, st, c.id)
		if at == nil || at.Before(before) || at.After(after) || kind == nil || *kind != rugAtDetected {
			t.Fatalf("got %v %v, want detected between %v and %v", at, strOrNil(kind), before, after)
		}
		if os := ltState(t, st, c.id); rugKindOf(&os) != rugAtDetected {
			t.Fatalf("rugKindOf the stored state = %q, want detected (rug %d scan %d latest %d)", rugKindOf(&os), os.RugBlock, os.ScanBlock, os.LatestBlock)
		}
		// The horizons ended before the check: none of them is rugged.
		if got := tmRead(t, st, c.id, "30d"); got.At == nil || got.Censored == nil || !*got.Censored {
			t.Fatalf("+30d: got %s, want censored (2x at the end, the rug after the window)", got)
		}
	})
	t.Run("latest pass", func(t *testing.T) {
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
		f.constCall(tWETH, selBalanceOf, ret(w32(wei(1)))) // alive at the end of tracking
		rp.drain(c.at(32 * ltDay))
		s := ltScanner(t, st, f.srv.URL)
		s.onChannelPost(postAt(1, c.entry, rp.token))
		c.id = *(<-s.queue).CallID
		if n := s.trackDue(ctx, 50); n != 1 {
			t.Fatalf("processed %d", n)
		}
		if at, _ := tmRugAt(t, st, c.id); at != nil {
			t.Fatalf("rug time %v before the pass found the rug", at)
		}
		if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
			t.Fatalf("pass %+v", res)
		}
		want := time.Unix(f.timeOf(c.at(32*ltDay)), 0).UTC()
		if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(want) || kind == nil || *kind != rugAtEvent {
			t.Fatalf("got %v %v, want %v event", at, strOrNil(kind), want)
		}
		if os := ltState(t, st, c.id); rugKindOf(&os) != rugAtEvent {
			t.Fatalf("rugKindOf the stored state = %q, want event", rugKindOf(&os))
		}
	})
}

// The latest-price pass finds the rug but cannot read the rug block's time:
// the rug is saved all the same (the lookup comes after it), the time is left
// to -backfill-timing.
func TestLatestPassRugSavedWithoutRugTime(t *testing.T) {
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
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(1)))) // alive at the end of tracking
	rugBlock := c.at(32 * ltDay)
	rp.drain(rugBlock)
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	f.mu.Lock()
	f.blockErr = func(n uint64) string {
		if n == rugBlock {
			return "header not found"
		}
		return ""
	}
	f.mu.Unlock()
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass %+v, want 1 refreshed (the rug saved)", res)
	}
	f.mu.Lock()
	f.blockErr = nil
	f.mu.Unlock()
	if os := ltState(t, st, c.id); os.RugBlock != rugBlock {
		t.Fatalf("stored rug block %d, want %d", os.RugBlock, rugBlock)
	}
	var rugged *bool
	if err := st.Pool.QueryRow(ctx, `SELECT rugged FROM scout_call_tracking WHERE call_id = $1`, c.id).Scan(&rugged); err != nil {
		t.Fatal(err)
	}
	if rugged == nil || !*rugged {
		t.Fatalf("rugged = %v, want true", rugged)
	}
	if at, _ := tmRugAt(t, st, c.id); at != nil {
		t.Fatalf("got rug time %v, want none (lookup failed)", at)
	}
	stats, err := runTimingBackfill(ctx, s, &bytes.Buffer{}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(f.timeOf(rugBlock), 0).UTC()
	if at, kind := tmRugAt(t, st, c.id); stats.RugStored != 1 || at == nil || !at.Equal(want) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("backfill %+v: got %v %v, want %v event", stats, at, strOrNil(kind), want)
	}
}

// tmOthers: everything of a call except the new columns: the backfill must
// leave it exactly as it was.
func tmOthers(t *testing.T, st *ScoutStore, id int) string {
	t.Helper()
	var row, rets, candles string
	ctx := context.Background()
	if err := st.Pool.QueryRow(ctx, `SELECT (to_jsonb(t) - 'rug_at' - 'rug_at_kind')::text FROM scout_call_tracking t WHERE call_id = $1`, id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(r) - 'peak_late_after_s' - 'first_2x_after_s' - 'above_2x_s'
		- 'fall_below_2x_after_s' - 'above_2x_censored' - 'timing_at' ORDER BY horizon_seconds)::text, '')
		FROM scout_call_returns r WHERE call_id = $1`, id).Scan(&rets); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY interval_seconds, bucket_start)::text, '')
		FROM scout_call_candles c WHERE call_id = $1`, id).Scan(&candles); err != nil {
		t.Fatal(err)
	}
	return row + "\n" + rets + "\n" + candles
}

// -backfill-timing on calls tracked before the columns existed: the dry run
// only counts; the run fills in the same timing the tracker computes and the
// exact rug time with one node lookup per rugged call, reads no logs, touches
// nothing else; a second run does nothing.
func TestBackfillTiming(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	rugged, rr := tmPumpCall(f, "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", 8*ltDay, true)
	alive, ra := tmPumpCall(f, "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c5", 9*ltDay, false)
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, rugged.entry, rr.token))
	s.onChannelPost(postAt(2, alive.entry, ra.token))
	rugged.id, alive.id = *(<-s.queue).CallID, *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("processed %d", n)
	}
	horizons := []string{"1h", "1d", "3d", "7d"}
	want := map[int]tmRow{rugged.id: tmCheckPump(t, st, rugged, horizons), alive.id: tmCheckPump(t, st, alive, horizons)}
	rugWant := time.Unix(f.timeOf(rugged.at(2*ltDay)), 0).UTC()

	// As if tracked before the columns existed.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_returns SET peak_late_after_s = NULL, first_2x_after_s = NULL,
		above_2x_s = NULL, fall_below_2x_after_s = NULL, above_2x_censored = NULL, timing_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET rug_at = NULL, rug_at_kind = NULL`); err != nil {
		t.Fatal(err)
	}
	others := map[int]string{rugged.id: tmOthers(t, st, rugged.id), alive.id: tmOthers(t, st, alive.id)}

	// Dry run: counts only, no node request, nothing written.
	s2 := ltScanner(t, st, f.srv.URL) // a fresh node client: no cached block times
	f.tookAll()
	var out bytes.Buffer
	stats, err := runTimingBackfill(ctx, s2, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Calls != 2 || stats.Horizons != 8 || stats.RugCalls != 1 || stats.Written != 0 {
		t.Fatalf("dry run: got %+v, want 2 calls, 8 horizons, 1 rug lookup, nothing written", stats)
	}
	if !strings.Contains(out.String(), "1 node lookup(s)") {
		t.Errorf("dry run output %q does not give the node lookups", out.String())
	}
	if n := f.tookAll(); len(n) != 0 {
		t.Fatalf("dry run asked the node: %v", n)
	}
	if got := tmRead(t, st, rugged.id, "1h"); got.At != nil {
		t.Fatalf("dry run wrote timing: %s", got)
	}

	// The node fails the rug-time lookup: the rugged horizons (+3d, +7d) are
	// skipped, the others written.
	f.mu.Lock()
	f.blockErr = func(uint64) string { return "header not found" }
	f.mu.Unlock()
	stats, err = runTimingBackfill(ctx, s2, &out, false)
	f.mu.Lock()
	f.blockErr = nil
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 6 || stats.NoRugTime != 2 || stats.RugFailed != 1 || stats.RugStored+stats.RugSkipped+stats.RugWriteFail+stats.Inconsistent+stats.Failed != 0 {
		t.Fatalf("run with the node failing: got %+v, want 6 written, 2 rugged horizons skipped, 1 rug lookup failed", stats)
	}
	if n := f.tookAll(); n["eth_getBlockByNumber"] != 1 || len(n) != 1 {
		t.Fatalf("run with the node failing: node requests %v, want exactly 1 eth_getBlockByNumber", n)
	}

	// The run.
	out.Reset()
	stats, err = runTimingBackfill(ctx, s2, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 2 || stats.RugStored != 1 || stats.RugFailed+stats.RugSkipped+stats.RugWriteFail+stats.NoRugTime+stats.Inconsistent+stats.Failed != 0 {
		t.Fatalf("run: got %+v, want the 2 rugged horizons written, 1 rug time, no failure (output %q)", stats, out.String())
	}
	if n := f.tookAll(); n["eth_getLogs"] != 0 || n["eth_getBlockByNumber"] != 1 || len(n) != 1 {
		t.Fatalf("run: node requests %v, want exactly 1 eth_getBlockByNumber (one per rugged call)", n)
	}
	for id, w := range want {
		for _, h := range horizons {
			if got := tmRead(t, st, id, h); got.At == nil || !got.same(w) {
				t.Errorf("call %d +%s: got %s, want %s (as the tracker)", id, h, got, w)
			}
		}
		if got := tmOthers(t, st, id); got != others[id] {
			t.Errorf("call %d: the backfill changed other columns:\nbefore %s\nafter  %s", id, others[id], got)
		}
	}
	if at, kind := tmRugAt(t, st, rugged.id); at == nil || !at.Equal(rugWant) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("rug: got %v %v, want %v event", at, strOrNil(kind), rugWant)
	}
	if at, _ := tmRugAt(t, st, alive.id); at != nil {
		t.Fatalf("alive call got a rug time %v", at)
	}

	// Again: nothing left to do.
	stats, err = runTimingBackfill(ctx, s2, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Horizons != 0 || stats.Written != 0 || stats.RugCalls != 0 {
		t.Fatalf("second run: got %+v, want nothing to do", stats)
	}
	if n := f.tookAll(); len(n) != 0 {
		t.Fatalf("second run asked the node: %v", n)
	}
	var due int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_tracking WHERE status IN ('pending', 'error')`).Scan(&due); err != nil || due != 0 {
		t.Fatalf("calls queued to be tracked again: %d (%v)", due, err)
	}
}

// ---------------------------------------------------------------------------
// Price-rule rug time (rug_at_kind 'price') and the guards that let a drained
// pool always win over it.
// ---------------------------------------------------------------------------

// prCall stores a call (message n) with a tracking row posted at entryAt.
func prCall(t *testing.T, st *ScoutStore, n int, entryAt time.Time) int {
	t.Helper()
	ctx := context.Background()
	var id int
	if err := st.Pool.QueryRow(ctx, `INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date,
		contract_address, chain, status, created_by, updated_by)
		VALUES (gen_random_uuid(), 1, 'c', $1, $2, '0xabc', 'evm', 'scanned', 't', 't') RETURNING id`, n, entryAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTracking(ctx, id, "0xabc", entryAt, 0, entryAt); err != nil {
		t.Fatal(err)
	}
	return id
}

// prReturn is a stored horizon: due `after` the post.
type prReturn struct {
	horizon string
	after   time.Duration
	status  string
	price   float64
}

// prCandle is a stored candle starting `after` the post (open = high = low = close).
type prCandle struct {
	intervalS int
	after     time.Duration
	close     float64
}

// prHourEnd is the end of the hourly candle holding the unix time ts.
func prHourEnd(ts int64) time.Time { return time.Unix(ts-ts%3600+3600, 0).UTC() }

func prSeed(t *testing.T, st *ScoutStore, id int, entryAt time.Time, rets []prReturn, candles []prCandle) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rets {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_returns (call_id, horizon, horizon_seconds, due_at, status, price_usd)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, r.horizon, int(r.after/time.Second), entryAt.Add(r.after), r.status, r.price); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range candles {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_candles (call_id, interval_seconds, bucket_start, open, high, low, close)
			VALUES ($1, $2, $3, $4, $4, $4, $4)`, id, c.intervalS, entryAt.Add(c.after), c.close); err != nil {
			t.Fatal(err)
		}
	}
}

// PriceRugAt: the end of the first hourly candle closing strictly under 5 % of
// the entry (capped at the last done horizon's due time), else the earliest
// done horizon under 5 %, else the last done horizon's due time; nothing when
// no horizon is done. storePriceRugAt stores it with kind 'price'.
func TestPriceRugAt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const entry = 100.0 // 5 % = 5
	e0 := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	hour := func(k int) time.Duration { return time.Duration(k)*time.Hour - 30*time.Minute } // the k-th UTC hour bucket after 00:00
	done := func(h string, after time.Duration, p float64) prReturn { return prReturn{h, after, "done", p} }
	std := []prReturn{done("1h", time.Hour, 80), done("1d", 24*time.Hour, 3), done("7d", 7*24*time.Hour, 2)}
	cases := []struct {
		name    string
		rets    []prReturn
		candles []prCandle
		want    time.Duration // after e0
		src     string
		noDone  bool
	}{
		{name: "the first of several closes under 5 %, even if the price recovers", rets: std,
			candles: []prCandle{{3600, hour(1), 50}, {3600, hour(5), 4}, {3600, hour(8), 3}, {3600, hour(10), 60}},
			want:    hour(6), src: "candle"},
		{name: "under 5 %, recovered, under 5 % again: the first close counts", rets: std,
			candles: []prCandle{{3600, hour(1), 50}, {3600, hour(3), 4}, {3600, hour(4), 60}, {3600, hour(9), 2}},
			want:    hour(4), src: "candle"},
		{name: "a close exactly at 5 % does not count", rets: std,
			candles: []prCandle{{3600, hour(5), 5}, {3600, hour(7), 4.99}},
			want:    hour(8), src: "candle"},
		{name: "the bucket straddling the last due time is capped at it",
			rets:    []prReturn{done("1h", time.Hour, 80), done("1d", 24*time.Hour, 50)},
			candles: []prCandle{{3600, hour(24), 1}},
			want:    24 * time.Hour, src: "candle"},
		{name: "300 s candles are ignored", rets: std,
			candles: []prCandle{{300, 10 * time.Minute, 1}, {3600, hour(3), 4}},
			want:    hour(4), src: "candle"},
		{name: "a candle from the last due time on is ignored",
			rets:    []prReturn{done("1h", time.Hour, 80), done("1d", 24*time.Hour, 50), {"7d", 7 * 24 * time.Hour, "no_data", 1}},
			candles: []prCandle{{3600, 24 * time.Hour, 1}},
			want:    24 * time.Hour, src: "last_due"},
		{name: "no candle under 5 %: the earliest done horizon under 5 %",
			rets: []prReturn{done("1h", time.Hour, 80), {"12h", 12 * time.Hour, "no_data", 1}, done("1d", 24*time.Hour, 4),
				done("7d", 7*24*time.Hour, 2)},
			candles: []prCandle{{300, 10 * time.Minute, 1}, {3600, hour(2), 50}},
			want:    24 * time.Hour, src: "horizon"},
		{name: "neither (a horizon exactly at 5 % does not count): the last done horizon's due time",
			rets: []prReturn{done("1h", time.Hour, 80), done("1d", 24*time.Hour, 5), done("7d", 7*24*time.Hour, 20),
				{"30d", 30 * 24 * time.Hour, "no_data", 1}},
			candles: []prCandle{{3600, hour(2), 50}},
			want:    7 * 24 * time.Hour, src: "last_due"},
		{name: "no done horizon: no time",
			rets:    []prReturn{{"1h", time.Hour, "no_data", 1}},
			candles: []prCandle{{3600, hour(1), 1}},
			noDone:  true},
	}
	s := &scanner{db: st}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := prCall(t, st, 1000+i, e0)
			prSeed(t, st, id, e0, c.rets, c.candles)
			at, src, err := st.PriceRugAt(ctx, id, entry)
			s.storePriceRugAt(ctx, id, entry, "test")
			stored, kind := tmRugAt(t, st, id)
			if c.noDone {
				if !errors.Is(err, errNoDoneHorizon) {
					t.Fatalf("PriceRugAt: got %v %q err %v, want errNoDoneHorizon", at, src, err)
				}
				if stored != nil || kind != nil {
					t.Fatalf("stored: got %v %v, want nothing", stored, strOrNil(kind))
				}
				return
			}
			want := e0.Add(c.want)
			if err != nil || !at.Equal(want) || src != c.src {
				t.Fatalf("PriceRugAt: got %v %q err %v, want %v %q", at, src, err, want, c.src)
			}
			if stored == nil || !stored.Equal(want) || kind == nil || *kind != rugAtPrice {
				t.Fatalf("stored: got %v %v, want %v price", stored, strOrNil(kind), want)
			}
		})
	}
}

// SetRugAtIfMissing replaces nothing or a 'price' time, never a drained kind
// (event, at_call, detected): a drained pool wins over the price rule, and the
// price rule never replaces a drained time.
func TestSetRugAtIfMissingPricePrecedence(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newAt := old.Add(time.Hour)
	n := 0
	for _, stored := range []string{"", rugAtPrice, rugAtEvent, rugAtAtCall, rugAtDetected} {
		for _, write := range []string{rugAtPrice, rugAtEvent, rugAtDetected} {
			wantWrote := stored == "" || stored == rugAtPrice
			label := stored
			if label == "" {
				label = "none"
			}
			t.Run("stored "+label+", write "+write, func(t *testing.T) {
				n++
				id := prCall(t, st, 2000+n, old)
				if stored != "" {
					if err := st.SetRugAt(ctx, id, old, stored); err != nil {
						t.Fatal(err)
					}
				}
				wrote, err := st.SetRugAtIfMissing(ctx, id, newAt, write)
				if err != nil {
					t.Fatal(err)
				}
				wantAt, wantKind := newAt, write
				if !wantWrote {
					wantAt, wantKind = old, stored
				}
				at, kind := tmRugAt(t, st, id)
				if wrote != wantWrote || at == nil || !at.Equal(wantAt) || kind == nil || *kind != wantKind {
					t.Fatalf("got wrote %v, %v %v; want wrote %v, %v %s", wrote, at, strOrNil(kind), wantWrote, wantAt, wantKind)
				}
			})
		}
	}
}

// A stored 'price' time does not stop ensureRugAt: with a rug block it asks
// the node for the block's time and stores it with the drained kind.
func TestEnsureRugAtReplacesPrice(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	s := ltScanner(t, st, f.srv.URL)
	id := prCall(t, st, 1, time.Now().Add(-10*ltDay))
	if err := st.SetRugAt(ctx, id, time.Now().Add(-ltDay), rugAtPrice); err != nil {
		t.Fatal(err)
	}
	if at, err := st.RugAt(ctx, id); err != nil || at != nil {
		t.Fatalf("RugAt with a 'price' time: got %v err %v, want nil (not a drained time)", at, err)
	}
	const rugBlock = 5000
	state := &onchainState{EntryBlock: 100, RugBlock: rugBlock, ScanBlock: 6000}
	f.tookAll()
	got, err := s.ensureRugAt(ctx, id, state)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(f.timeOf(rugBlock), 0).UTC()
	if n := f.tookAll(); n["eth_getBlockByNumber"] != 1 {
		t.Errorf("node requests %v, want 1 eth_getBlockByNumber (the rug block's time)", n)
	}
	at, kind := tmRugAt(t, st, id)
	if got == nil || !got.Equal(want) || at == nil || !at.Equal(want) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("got returned %v, stored %v %v; want %v event", got, at, strOrNil(kind), want)
	}
}

// RugAtBackfillRows (-backfill-timing) picks a 'price' time when the state
// has a rug block (the latest-price pass found the pool drained but could not
// read the block's time), and skips 'price' without a rug block and drained
// kinds; the backfill then stores the drained kind.
func TestRugAtBackfillRowsPrice(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const rugBlock = 5000
	withRug := `{"v":2,"entry_block":100,"scan_block":6000,"rug_block":5000}`
	noRug := `{"v":2,"entry_block":100,"scan_block":6000}`
	cases := []struct {
		name  string
		kind  string // "" = no rug time
		state string
		pick  bool
	}{
		{"price with a rug block", rugAtPrice, withRug, true},
		{"price without a rug block", rugAtPrice, noRug, false},
		{"no rug time, a rug block", "", withRug, true},
		{"event with a rug block", rugAtEvent, withRug, false},
		{"detected with a rug block", rugAtDetected, withRug, false},
	}
	ids := make([]int, len(cases))
	var want []int
	priceAt := time.Now().Add(-ltDay).UTC().Truncate(time.Second)
	for i, c := range cases {
		ids[i] = prCall(t, st, 3000+i, time.Now().Add(-10*ltDay))
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = $2::jsonb WHERE call_id = $1`, ids[i], c.state); err != nil {
			t.Fatal(err)
		}
		if c.kind != "" {
			if err := st.SetRugAt(ctx, ids[i], priceAt, c.kind); err != nil {
				t.Fatal(err)
			}
		}
		if c.pick {
			want = append(want, ids[i])
		}
	}
	rows, err := st.RugAtBackfillRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range rows {
		got = append(got, r.CallID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got calls %v, want %v (cases %v)", got, want, ids)
	}

	f := ltChain(t, 20*ltDay)
	s := ltScanner(t, st, f.srv.URL)
	var stats timingBackfillStats
	if err := s.backfillRugTimes(ctx, &bytes.Buffer{}, rows, st.SetRugAtIfMissing, &stats); err != nil {
		t.Fatal(err)
	}
	drained := time.Unix(f.timeOf(rugBlock), 0).UTC()
	for i, c := range cases {
		wantAt, wantKind := priceAt, c.kind
		if c.pick {
			wantAt, wantKind = drained, rugAtEvent
		}
		at, kind := tmRugAt(t, st, ids[i])
		if at == nil || !at.Equal(wantAt) || kind == nil || *kind != wantKind {
			t.Errorf("%s: after the backfill got %v %v, want %v %s", c.name, at, strOrNil(kind), wantAt, wantKind)
		}
	}
}

// End to end: a call collapses under 5 % of its entry while its pool stays
// alive. The tracker stores kind 'price' at the end of the hourly bucket of
// the collapse; later the latest-price pass finds the pool drained and the
// kind becomes event, at the rug block's time.
func TestPriceRugThenDrainedByLatestPass(t *testing.T) {
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
	dump := c.at(10 * ltDay)
	rp.trade(dump, 1e-8)                               // 1 % of the entry
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(1)))) // alive at the end of tracking ($3000 of quote side)
	rp.drain(c.at(32 * ltDay))
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	var rugged *bool
	if err := st.Pool.QueryRow(ctx, `SELECT rugged FROM scout_call_tracking WHERE call_id = $1`, c.id).Scan(&rugged); err != nil {
		t.Fatal(err)
	}
	if rugged == nil || !*rugged {
		t.Fatalf("rugged = %v, want true (price rule)", rugged)
	}
	wantPrice := prHourEnd(f.timeOf(dump))
	if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(wantPrice) || kind == nil || *kind != rugAtPrice {
		t.Fatalf("after tracking: got %v %v, want %v price", at, strOrNil(kind), wantPrice)
	}
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass %+v", res)
	}
	want := time.Unix(f.timeOf(c.at(32*ltDay)), 0).UTC()
	if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(want) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("after the latest pass: got %v %v, want %v event", at, strOrNil(kind), want)
	}
}

// storePriceRugAt never replaces a drained kind: a call with an event or
// detected time and a done horizon under 5 % of the entry keeps its time and
// kind (the price rule is written only through SetRugAtIfMissing).
func TestStorePriceRugAtKeepsDrained(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const entry = 100.0 // 5 % = 5
	e0 := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	drained := e0.Add(36 * time.Hour)
	s := &scanner{db: st}
	for i, kind := range []string{rugAtEvent, rugAtDetected} {
		t.Run(kind, func(t *testing.T) {
			id := prCall(t, st, 4000+i, e0)
			if err := st.SetRugAt(ctx, id, drained, kind); err != nil {
				t.Fatal(err)
			}
			prSeed(t, st, id, e0, []prReturn{{"1h", time.Hour, "done", 80}, {"1d", 24 * time.Hour, "done", 1}},
				[]prCandle{{3600, 2*time.Hour - 30*time.Minute, 2}})
			if _, _, err := st.PriceRugAt(ctx, id, entry); err != nil {
				t.Fatalf("PriceRugAt: %v, want a time (the case must reach the write)", err)
			}
			s.storePriceRugAt(ctx, id, entry, "test")
			if at, k := tmRugAt(t, st, id); at == nil || !at.Equal(drained) || k == nil || *k != kind {
				t.Fatalf("got %v %v, want %v %s unchanged", at, strOrNil(k), drained, kind)
			}
		})
	}
}

// TimingBackfillHorizons gives a drained time as RugAt, never a 'price' one
// (approximate: it must not cut a horizon's window).
func TestTimingBackfillHorizonsIgnoresPriceRugAt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e0 := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	rugAt := e0.Add(12 * time.Hour)
	for i, c := range []struct {
		kind string
		want *time.Time
	}{{rugAtPrice, nil}, {rugAtEvent, &rugAt}} {
		t.Run(c.kind, func(t *testing.T) {
			id := prCall(t, st, 5000+i, e0)
			if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET entry_late_price_usd = 1,
				onchain = '{"v":2,"done":{"1d":true}}'::jsonb WHERE call_id = $1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_returns (call_id, horizon, horizon_seconds, due_at, status, price_usd, max_gain_late_pct)
				VALUES ($1, '1d', 86400, $2, 'done', 0.5, 10)`, id, e0.Add(24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := st.SetRugAt(ctx, id, rugAt, c.kind); err != nil {
				t.Fatal(err)
			}
			rows, err := st.TimingBackfillHorizons(ctx, []int{id})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1 (the seeded 1d horizon)", len(rows))
			}
			if got := rows[0].RugAt; (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
				t.Fatalf("kind %s: got RugAt %v, want %v", c.kind, got, c.want)
			}
		})
	}
}

// A drained call finishes tracking while the rug block's time cannot be read
// (node down for that block): rug_at stays NULL, and no 'price' time is
// stored in its place (the pool was drained, so the price rule does not
// apply). -backfill-timing then stores the drained time.
func TestTrackerDrainedRugTimeFailsNoPrice(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 60*ltDay)
	c, rp := tmPumpCall(f, "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3", 35*ltDay, true)
	rugBlock := c.at(2 * ltDay)
	asked := 0
	f.mu.Lock()
	f.blockErr = func(n uint64) string {
		if n != rugBlock {
			return ""
		}
		asked++
		return "header not found"
	}
	f.mu.Unlock()
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, c.entry, rp.token))
	c.id = *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	f.mu.Lock()
	got := asked
	f.blockErr = nil
	f.mu.Unlock()
	if got != 1 {
		t.Errorf("rug block %d: got %d time lookup(s) in the run, want 1", rugBlock, got)
	}
	var status string
	var rugged *bool
	if err := st.Pool.QueryRow(ctx, `SELECT status, rugged FROM scout_call_tracking WHERE call_id = $1`, c.id).Scan(&status, &rugged); err != nil {
		t.Fatal(err)
	}
	if status != TrackDone || rugged == nil || !*rugged {
		t.Fatalf("got status %q rugged %v, want done and rugged (the test must reach the end of tracking)", status, rugged)
	}
	if os := ltState(t, st, c.id); os.RugBlock != rugBlock {
		t.Fatalf("stored rug block %d, want %d", os.RugBlock, rugBlock)
	}
	if at, kind := tmRugAt(t, st, c.id); at != nil || kind != nil {
		t.Fatalf("got rug time %v kind %v, want both NULL (lookup failed; no price time for a drained pool)", at, strOrNil(kind))
	}

	stats, err := runTimingBackfill(ctx, s, &bytes.Buffer{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RugStored != 1 {
		t.Fatalf("backfill: got %+v, want the rug time stored", stats)
	}
	want := time.Unix(f.timeOf(rugBlock), 0).UTC()
	if at, kind := tmRugAt(t, st, c.id); at == nil || !at.Equal(want) || kind == nil || *kind != rugAtEvent {
		t.Fatalf("after the backfill: got %v %v, want %v event", at, strOrNil(kind), want)
	}
}
