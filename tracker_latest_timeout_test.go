package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass: the row time limit (SCOUT_LATEST_ROW_TIMEOUT) and the
// quote lookup shared by the rows of a pass. No database.
// ---------------------------------------------------------------------------

// fakeQuotes is a latestQuotes whose lookup is answered by answer (and
// counted); pass stands for the pass's context.
func fakeQuotes(t *testing.T, pass context.Context, answer func(n int64) (float64, error)) (*latestQuotes, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	lq := &latestQuotes{
		lookup:    func(string) (float64, error) { return answer(calls.Add(1)) },
		passEnded: func() bool { return pass.Err() != nil },
		wg:        &wg,
		m:         map[string]*latestQuote{},
	}
	return lq, &calls
}

// usdWithin is lq.usd with a context that ends after limit; it fails the test
// when the call has not returned well after that.
func usdWithin(t *testing.T, lq *latestQuotes, limit time.Duration) (float64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	type answer struct {
		q   float64
		err error
	}
	got := make(chan answer, 1)
	go func() {
		q, err := lq.usd(ctx, "0xQuote")
		got <- answer{q, err}
	}()
	select {
	case a := <-got:
		return a.q, a.err
	case <-time.After(limit + 5*time.Second):
		t.Fatalf("usd with a %s limit has not returned after %s", limit, limit+5*time.Second)
		return 0, nil
	}
}

func TestLatestQuotesRowLimitDoesNotStopTheLookup(t *testing.T) {
	release := make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	lq, calls := fakeQuotes(t, context.Background(), func(int64) (float64, error) {
		<-release
		return 2.5, nil
	})
	t.Cleanup(free) // runs before fakeQuotes' wait for the lookup
	// The first row starts the lookup; its own limit ends its wait, not the lookup.
	if q, err := usdWithin(t, lq, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || q != 0 {
		t.Fatalf("first row: got %v, %v; want 0, context.DeadlineExceeded", q, err)
	}
	// A row waiting for the same lookup is bounded by its own limit too.
	if q, err := usdWithin(t, lq, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || q != 0 {
		t.Fatalf("waiting row: got %v, %v; want 0, context.DeadlineExceeded", q, err)
	}
	// A row with time to spare gets the price once the lookup ends.
	got := make(chan error, 1)
	go func() {
		q, err := lq.usd(context.Background(), "0xQUOTE") // other letter case: the same quote
		if err == nil && q != 2.5 {
			err = fmt.Errorf("price %v, want 2.5", q)
		}
		got <- err
	}()
	free()
	if err := <-got; err != nil {
		t.Fatalf("row after the lookup: %v", err)
	}
	if q, err := usdWithin(t, lq, time.Second); err != nil || q != 2.5 {
		t.Fatalf("later row: got %v, %v; want 2.5, nil", q, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("got %d lookups, want 1", n)
	}
}

// A lookup that returns while the pass's context is done is not kept: the next
// row asks again. A context error while the pass is alive, such as the HTTP
// client's own time limit (it wraps context.DeadlineExceeded), is the node's
// answer: kept for the pass, so the next row fails fast without a new lookup.
func TestLatestQuotesContextErrorNotKept(t *testing.T) {
	for _, c := range []struct {
		what        string
		stop        error
		endPass     bool  // the pass's context is done when the lookup returns
		wantLookups int64 // after two rows
	}{
		{"pass cancelled", context.Canceled, true, 2},
		{"pass ended, deadline error", context.DeadlineExceeded, true, 2},
		{"client time-out, pass alive", context.DeadlineExceeded, false, 1},
		{"canceled error, pass alive", context.Canceled, false, 1},
	} {
		t.Run(c.what, func(t *testing.T) {
			pass, endPass := context.WithCancel(context.Background())
			defer endPass()
			lq, calls := fakeQuotes(t, pass, func(n int64) (float64, error) {
				if n == 1 {
					if c.endPass {
						endPass()
					}
					return 0, fmt.Errorf("eth_call: %w", c.stop)
				}
				return 3, nil
			})
			if _, err := usdWithin(t, lq, time.Second); !errors.Is(err, c.stop) {
				t.Fatalf("first lookup: got %v, want %v", err, c.stop)
			}
			q, err := usdWithin(t, lq, time.Second)
			if c.endPass && (err != nil || q != 3) {
				t.Fatalf("next row: got %v, %v; want 3, nil (a new lookup)", q, err)
			}
			if !c.endPass && (!errors.Is(err, c.stop) || q != 0) {
				t.Fatalf("next row: got %v, %v; want 0, %v (the kept error)", q, err, c.stop)
			}
			if n := calls.Load(); n != c.wantLookups {
				t.Fatalf("got %d lookups, want %d", n, c.wantLookups)
			}
		})
	}
}

func TestLatestQuotesOtherErrorShared(t *testing.T) {
	lq, calls := fakeQuotes(t, context.Background(), func(int64) (float64, error) {
		return 0, fmt.Errorf("0xquote: %w", errNoUSDSource)
	})
	for i := range 3 {
		if _, err := usdWithin(t, lq, time.Second); !errors.Is(err, errNoUSDSource) {
			t.Fatalf("row %d: got %v, want errNoUSDSource", i+1, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("got %d lookups, want 1 (the error is kept for the pass)", n)
	}
}

func TestLatestRowContext(t *testing.T) {
	pass := context.Background()
	// 0 = off: the pass's own context, no deadline.
	ctx, cancel := latestRowContext(pass, 0)
	cancel()
	if _, ok := ctx.Deadline(); ok || ctx.Err() != nil {
		t.Fatalf("limit 0: got a deadline (%v) or an ended context (%v), want neither", ok, ctx.Err())
	}
	ctx, cancel = latestRowContext(pass, time.Hour)
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > time.Hour || time.Until(d) < 59*time.Minute {
		t.Fatalf("limit 1h: got deadline %v (%v), want in about an hour", d, ok)
	}
}

func TestLatestRowError(t *testing.T) {
	const limit = 10 * time.Millisecond
	ended, cancel := latestRowContext(context.Background(), limit)
	defer cancel()
	<-ended.Done()
	scan := fmt.Errorf("swaps to block 9: %w", context.DeadlineExceeded)
	err := latestRowError(ended, limit, scan)
	want := "latest price: stopped after the 10ms row time limit (SCOUT_LATEST_ROW_TIMEOUT): swaps to block 9: context deadline exceeded"
	if err == nil || err.Error() != want || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("row stopped by its limit: got %v, want %q", err, want)
	}
	if err := latestRowError(ended, limit, nil); err != nil {
		t.Fatalf("no error: got %v, want nil", err)
	}
	// The pass cancelled (shutdown), or the limit not reached: the error as it was.
	passCtx, stop := context.WithCancel(context.Background())
	row, rowCancel := latestRowContext(passCtx, time.Hour)
	defer rowCancel()
	stop()
	other := errors.New("no usable on-chain state")
	for _, c := range []struct {
		what  string
		ctx   context.Context
		limit time.Duration
		err   error
	}{
		{"pass cancelled", row, time.Hour, context.Canceled},
		{"within the limit", context.Background(), time.Hour, other},
		{"limit off", ended, 0, scan},
	} {
		if got := latestRowError(c.ctx, c.limit, c.err); got != c.err {
			t.Errorf("%s: got %v, want %v unchanged", c.what, got, c.err)
		}
	}
}

func TestLatestRowLimitText(t *testing.T) {
	for _, c := range []struct {
		limit time.Duration
		want  string
	}{
		{5 * time.Minute, "each call stopped after 5m0s (SCOUT_LATEST_ROW_TIMEOUT)"},
		{0, "no time limit per call (SCOUT_LATEST_ROW_TIMEOUT=0)"},
	} {
		if got := latestRowLimitText(c.limit); got != c.want {
			t.Errorf("latestRowLimitText(%s) = %q, want %q", c.limit, got, c.want)
		}
	}
}
