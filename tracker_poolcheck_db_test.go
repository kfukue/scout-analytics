package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The tracker's pool discovery with the pool's slot0() not answered by the
// node (rate limited, no state at latest, HTTP 503): the call is in error and
// retried after the usual back-off, not no_pool (6 hours, and given up past
// the deadline); the next attempt, with the node answering, finds the pool and
// prices the call.
func TestTrackerPoolCheckNodeErrorIsRetried(t *testing.T) {
	for _, fl := range []nodeFailure{
		{name: "rate limited (-32005)", msg: "rate limit exceeded", code: -32005},
		{name: "no state at latest", msg: "missing trie node 0xab (path ) state 0xcd is not available", code: -32000},
		{name: "HTTP 503", answer: httpStatus(503)},
	} {
		t.Run(fl.name, func(t *testing.T) {
			fastRetries(t)
			st := testStore(t)
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			f := ltChain(t, 10*ltDay)
			token := "0x4444444444444444444444444444444444444444"
			pool := "0x00000000000000000000000000000000000000c3"
			f.ltPool(token, pool, tWETH)
			entry := time.Now().Add(-2 * ltDay)
			eb := f.blockAtTime(entry)
			for i, blk := range []uint64{eb - 500, eb + 20000} {
				tx := hexU64(blk)
				f.swapV3(pool, blk, tx, sqrtX96(1/(1e-6*float64(i+1))))
				f.transfer(token, pool, ltBuyer, blk, tx)
			}
			s := ltScanner(t, st, f.srv.URL)
			s.onChannelPost(postAt(1, entry, token))
			id := *(<-s.queue).CallID

			stop := failCall(f, pool, selSlot0, fl)
			before := time.Now()
			if n := s.trackDue(ctx, 10); n != 1 {
				t.Fatalf("first pass processed %d, want 1", n)
			}
			tr, err := st.GetTracking(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Status != TrackError || tr.Error == nil || !strings.Contains(*tr.Error, "pool discovery") ||
				!strings.Contains(*tr.Error, "slot0()") || tr.PoolAddress != nil {
				t.Fatalf("after slot0() was not answered: status %s error %q pool %s, want error (pool discovery, slot0()) and no pool",
					tr.Status, strOrNil(tr.Error), strOrNil(tr.PoolAddress))
			}
			if wait := tr.NextCheckAt.Sub(before); wait < backoff(1)-time.Minute || wait > backoff(1)+time.Minute {
				t.Fatalf("next check in %s, want the back-off of a first error (%s), not no_pool's 6h", wait, backoff(1))
			}

			stop()
			makeDue(t, st, id, -1)
			if n := s.trackDue(ctx, 10); n != 1 {
				t.Fatalf("second pass processed %d, want 1", n)
			}
			tr, err = st.GetTracking(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Status != TrackTracking || tr.PoolAddress == nil || *tr.PoolAddress != pool || tr.PoolDex == nil || *tr.PoolDex != "uniswap-v3" ||
				tr.EntryPriceUSD == nil {
				t.Fatalf("once the node answers: status %s error %q pool %s, want tracking in the v3 pool %s with an entry price",
					tr.Status, strOrNil(tr.Error), strOrNil(tr.PoolAddress), pool)
			}
		})
	}
}
