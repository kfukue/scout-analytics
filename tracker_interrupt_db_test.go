package main

import (
	"context"
	"testing"
	"time"
)

// Ctrl+C in the middle of a call must not change it (and never mark it gave_up),
// and rows damaged by the old behaviour are repaired by the migration.
func TestTrackerInterruptedLeavesCallUntouched(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t, 60*24*time.Hour)
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777

	// 40 days old: past the give-up deadline, the case that used to be marked gave_up.
	s.onChannelPost(postAt(1, time.Now().Add(-40*24*time.Hour), "0x4444444444444444444444444444444444444444"))
	id := *(<-s.queue).CallID

	rows, err := st.DueTracking(ctx, time.Now(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due: %d %v", len(rows), err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	s.trackOneOnchain(cctx, &rows[0])

	got, _ := st.GetTracking(ctx, id)
	if got.Status != TrackPending || got.Attempts != 0 || got.Error != nil {
		t.Fatalf("interrupted call changed: status %s attempts %d err %v", got.Status, got.Attempts, strOrNil(got.Error))
	}
	if rows[0].Status != TrackPending || rows[0].Attempts != 0 {
		t.Fatalf("in-memory row changed: %+v", rows[0])
	}

	counts, next, err := st.TrackingStats(ctx)
	if err != nil || counts[TrackPending] != 1 || next == nil {
		t.Fatalf("stats: %v %v %v", counts, next, err)
	}

	// A row broken by the old behaviour is reset on the next start.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status='gave_up', attempts=3,
		error='swaps to +1d: eth_getLogs 1-2: context canceled', next_check_at = now() + interval '1 day' WHERE call_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetTracking(ctx, id)
	if got.Status != TrackPending || got.Error != nil || got.NextCheckAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("not repaired: %+v", got)
	}
}

func TestScanProgressCallback(t *testing.T) {
	var calls int
	ctx := context.WithValue(context.Background(), progressKey{}, progressFunc(func(from, done, to uint64, events int) { calls++ }))
	if p := progressFrom(ctx); p == nil {
		t.Fatal("no progress func")
	} else {
		p(1, 2, 3, 0)
	}
	if calls != 1 || progressFrom(context.Background()) != nil {
		t.Fatal("progress plumbing")
	}
}
