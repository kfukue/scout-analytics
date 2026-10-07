package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The tracker loop keeps one pool of workers: a call that is still running
// when the next cycle starts stays with its worker. The next cycle (feeds
// reload, repeat marking, horizon hand-out, latest-price pass) neither hands it
// out again nor refreshes its latest price, and the other workers carry on.
func TestTrackLoopCycleWhileACallRuns(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	tokenA, poolA := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	tokenB, poolB := "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c4"
	entry := time.Now().Add(-2 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	for _, p := range [][2]string{{tokenA, poolA}, {tokenB, poolB}} {
		f.ltPool(p[0], p[1], tWETH)
		f.ltTrade(p[0], p[1], at(-2*time.Minute), 1e-6)
		f.ltTrade(p[0], p[1], at(30*time.Second), 2e-6)
		f.ltTrade(p[0], p[1], at(20*time.Hour), 3e-6)
		f.ltTrade(p[0], p[1], at(40*time.Hour), 8e-6)
	}
	s := ltScanner(t, st, f.srv.URL)
	s.pc.Workers = 4
	s.onChannelPost(postAt(1, entry, tokenA))
	a := *(<-s.queue).CallID
	s.onChannelPost(postAt(2, entry, tokenB))
	b := *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("got %d processed, want 2", n)
	}
	// The 3d horizon becomes due for both.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET message_date = message_date - interval '36 hours';
		UPDATE scout_call_tracking SET entry_at = entry_at - interval '36 hours', next_check_at = now()`); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetTracking(ctx, a)
	if err != nil {
		t.Fatal(err)
	}

	// A's swap scan hangs until released.
	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	f.setLogsHook(func(addr string, from, to uint64) string {
		if addr == poolA {
			once.Do(func() { close(entered) })
			select {
			case <-gate:
			case <-time.After(30 * time.Second):
			}
		}
		return ""
	})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)

	r := s.newTrackRun()
	if more := r.cycle(ctx, trackBatch); more {
		t.Fatal("first cycle: got more, want false (2 calls due)")
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("call A never reached its swap scan")
	}
	// B finishes on another worker while A hangs.
	deadline := time.Now().Add(30 * time.Second)
	for {
		tb, err := st.GetTracking(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if rs, _ := st.ReturnsForCall(ctx, b); len(rs) == 3 && tb.NextCheckAt.After(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("call B did not finish its 3d check while A was running")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The next cycle, with A still running and still due in the database.
	ltMakeDue(t, st, "1 hour")
	if more := r.cycle(ctx, trackBatch); more {
		t.Fatal("second cycle: got more, want false (only A is due, and it is running)")
	}
	// B's worker gives it back only after its run returned: wait for A alone.
	waitHorizonBusy(t, r, 1)
	release()
	r.close()

	ta, err := st.GetTracking(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if ta.Attempts != before.Attempts+1 {
		t.Fatalf("call A: got %d attempts, want %d (handed out once)", ta.Attempts, before.Attempts+1)
	}
	if rs, _ := st.ReturnsForCall(ctx, a); len(rs) != 3 {
		t.Fatalf("call A: got horizons %v, want 1h, 1d and 3d", sortedKeys(rs))
	}
	if c := ltRead(t, st, b); !ltNear(c.Price, 8e-6*ltETH) || c.Checked == nil || time.Since(*c.Checked) > time.Minute {
		t.Fatalf("call B: got latest %v checked %v, want %v just now", fnum(c.Price), c.Checked, 8e-6*ltETH)
	}
	if c := ltRead(t, st, a); c.Checked != nil && time.Since(*c.Checked) < 30*time.Minute {
		t.Fatalf("call A: latest price refreshed at %v while its horizon check ran", c.Checked)
	}
}
