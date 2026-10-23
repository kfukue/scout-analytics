package main

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass: scan labels and the progress line (no database).
// ---------------------------------------------------------------------------

func TestLatestScanLabels(t *testing.T) {
	if got, want := latestRowLabel(1234), "latest call 1234"; got != want {
		t.Errorf("latestRowLabel(1234) = %q, want %q", got, want)
	}
	hour := time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC).Unix()
	got := latestQuoteLabel("0xAbC0000000000000000000000000000000000001", hour)
	want := "latest quote 0xabc0000000000000000000000000000000000001 hour 2026-10-22 06:00 UTC"
	if got != want {
		t.Errorf("latestQuoteLabel(..., %d) = %q, want %q", hour, got, want)
	}
	// withScanLabel sets the label scans print, and no progress lines.
	ctx := withScanLabel(context.Background(), "latest call 7")
	if got := scanLabelFrom(ctx); got != "latest call 7" {
		t.Errorf("scanLabelFrom(withScanLabel(ctx, %q)) = %q", "latest call 7", got)
	}
	if progressFrom(ctx) != nil {
		t.Error("withScanLabel installed a progress function: latest rows would print new progress lines")
	}
	// withScanProgress still sets the label too.
	if got := scanLabelFrom(withScanProgress(context.Background(), "call 9 [#1]", time.Second)); got != "call 9 [#1]" {
		t.Errorf("scanLabelFrom(withScanProgress(ctx, %q)) = %q", "call 9 [#1]", got)
	}
}

func TestShortAge(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{400 * time.Millisecond, "0s"},
		{45 * time.Second, "45s"},
		{9 * time.Minute, "9m"},
		{9*time.Minute + 200*time.Millisecond, "9m"},
		{21*time.Minute + 30*time.Second, "21m30s"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Minute, "1h5m"},
		{time.Hour + 5*time.Second, "1h0m5s"},
	} {
		if got := shortAge(c.d); got != c.want {
			t.Errorf("shortAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// The running rows are counted from hand-out to finish, oldest first; the
// progress line names the oldest ones that have run for latestSlowRowAfter.
func TestLatestProgressLine(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC)
	type start struct {
		id int
		at time.Duration // after t0
	}
	for _, c := range []struct {
		name     string
		starts   []start
		finished []int
		now      time.Duration // after t0
		due      int
		reqs     int64
		want     string
	}{
		{
			name: "nothing handed out yet",
			now:  30 * time.Second, due: 200,
			want: "latest prices: 0 finished, 0 running, 0 handed out of 200 due, 30s elapsed, 0 RPC requests",
		},
		{
			name:   "running, none for a minute yet",
			starts: []start{{1, 0}, {2, 0}, {3, 10 * time.Second}}, finished: []int{1},
			now: 50 * time.Second, due: 200, reqs: 42,
			want: "latest prices: 1 finished, 2 running, 3 handed out of 200 due, 50s elapsed, 42 RPC requests",
		},
		{
			name:   "two slow rows, oldest first",
			starts: []start{{5678, 13*time.Minute + 30*time.Second}, {1234, time.Second}, {99, 22 * time.Minute}, {7, 0}}, finished: []int{7},
			now: 22*time.Minute + 30*time.Second, due: 200, reqs: 606,
			want: "latest prices: 1 finished, 3 running (call 1234 for 22m29s, call 5678 for 9m), 4 handed out of 200 due, 22m30s elapsed, 606 RPC requests",
		},
		{
			name:   "more than three slow rows: the three oldest",
			starts: []start{{10, 4 * time.Minute}, {11, 0}, {12, time.Minute}, {13, 2 * time.Minute}, {14, 3 * time.Minute}},
			now:    10 * time.Minute, due: 1500, reqs: 12345,
			want: "latest prices: 0 finished, 5 running (call 11 for 10m, call 12 for 9m, call 13 for 8m), 5 handed out of 1,500 due, 10m0s elapsed, 12,345 RPC requests",
		},
		{
			name:   "same start: by call id",
			starts: []start{{30, 0}, {20, 0}},
			now:    2 * time.Minute, due: 2,
			want: "latest prices: 0 finished, 2 running (call 20 for 2m, call 30 for 2m), 2 handed out of 2 due, 2m0s elapsed, 0 RPC requests",
		},
		{
			name:   "all finished",
			starts: []start{{1, 0}, {2, 0}}, finished: []int{2, 1},
			now: 3 * time.Minute, due: 2, reqs: 9,
			want: "latest prices: 2 finished, 0 running, 2 handed out of 2 due, 3m0s elapsed, 9 RPC requests",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			lr := newLatestRunning()
			for _, s := range c.starts {
				lr.start(s.id, t0.Add(s.at))
			}
			for _, id := range c.finished {
				lr.finish(id)
			}
			finished, running := lr.snapshot(t0.Add(c.now))
			got := latestProgressLine(finished, running, c.due, c.now, c.reqs)
			if got != c.want {
				t.Errorf("starts %v, finished %v, at +%s:\n got %q\nwant %q", c.starts, c.finished, c.now, got, c.want)
			}
		})
	}
}
