package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tmAt: a time on 1 Jan 2026 (UTC) plus whole days, "15:04" or "15:04:05".
func tmAt(t *testing.T, days int, clock string) time.Time {
	t.Helper()
	layout := "15:04"
	if len(clock) > 5 {
		layout = "15:04:05"
	}
	c, err := time.Parse(layout, clock)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, 1, 1+days, c.Hour(), c.Minute(), c.Second(), 0, time.UTC)
}

func tmFine(t *testing.T, days int, clock string, h, c float64) candleRow {
	t.Helper()
	return candleRow{IntervalS: candleFineS, Start: tmAt(t, days, clock), O: c, H: h, L: c, C: c, Events: 1}
}

func tmHour(t *testing.T, days int, clock string, h, c float64) candleRow {
	t.Helper()
	return candleRow{IntervalS: candleCoarseS, Start: tmAt(t, days, clock), O: c, H: h, L: c, C: c, Events: 1}
}

func tmStr(tm horizonTiming) string {
	p := func(v *int) string {
		if v == nil {
			return "nil"
		}
		return fmt.Sprint(*v)
	}
	return fmt.Sprintf("{peak %s, first2x %s, above %d, fall %s, censored %v}",
		p(tm.PeakLateAfterS), p(tm.First2xAfterS), tm.Above2xS, p(tm.FallBelow2xAfterS), tm.Censored)
}

func TestCandleTiming(t *testing.T) {
	entry := tmAt(t, 0, "12:03")
	in := func(gain float64, due time.Duration, fine ...candleRow) timingInput {
		return timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(due), Entry: 1, PeakGainPct: gain, Fine: fine}
	}
	rugAt := tmAt(t, 0, "12:33")
	tests := []struct {
		name string
		in   timingInput
		want horizonTiming
	}{
		{
			// 12:05 crosses 2× and closes above (held from its end, 12:10);
			// nothing until 12:30, which closes under (fall at its end, 12:35);
			// 12:45 closes above again and is carried to the due time 13:03.
			name: "gaps carried forward",
			in: in(150, time.Hour,
				tmFine(t, 0, "12:05", 2.5, 2.2), tmFine(t, 0, "12:30", 2.1, 1.5), tmFine(t, 0, "12:45", 2.3, 2.1)),
			want: horizonTiming{PeakLateAfterS: ip(120), First2xAfterS: ip(120),
				Above2xS: 1500 + 780, FallBelow2xAfterS: ip(32 * 60)},
		},
		{
			// The bucket 12:00–12:05 starts before the post (12:03) and holds
			// the late entry (12:04): clipped to start at the late entry. The
			// stored peak is only in it, so it came after the late entry; its
			// close counts from 12:05.
			name: "first bucket holds the post and the late entry",
			in:   in(200, time.Hour, tmFine(t, 0, "12:00", 3, 3)),
			want: horizonTiming{PeakLateAfterS: ip(60), First2xAfterS: ip(60), Above2xS: 3480, Censored: true},
		},
		{
			// Post 12:04:30, late entry 12:05:30: the bucket 12:00–12:05 holds
			// only trades before the late entry (a 3× spike) and is left out.
			name: "spike before the late entry",
			in: timingInput{EntryAt: tmAt(t, 0, "12:04:30"), LateAt: tmAt(t, 0, "12:05:30"), DueAt: tmAt(t, 0, "13:04:30"),
				Entry: 1, PeakGainPct: 20, Fine: []candleRow{tmFine(t, 0, "12:00", 3, 1), tmFine(t, 0, "12:10", 1.2, 1.1)}},
			want: horizonTiming{PeakLateAfterS: ip(330)},
		},
		{
			// The bucket holding the late entry reaches 2.5× before it; the
			// stored late peak (+20%) says the late entry never reached 2×.
			name: "candle at 2x but stored late peak under 2x",
			in:   in(20, time.Hour, tmFine(t, 0, "12:00", 2.5, 1), tmFine(t, 0, "12:10", 1.2, 1.1)),
			want: horizonTiming{PeakLateAfterS: ip(420)},
		},
		{
			// Day one ends at 2 Jan 12:03: the 5-minute bucket 12:00 counts up
			// to 12:03, the hourly 12:00 bucket from 12:03 (the hourly 11:00
			// bucket is day one: left out, so its 9× high is not used).
			name: "5-minute / hourly boundary",
			in: timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(72 * time.Hour), Entry: 1, PeakGainPct: 120,
				Fine: []candleRow{tmFine(t, 1, "12:00", 2.2, 2.2)},
				Coarse: []candleRow{tmHour(t, 1, "11:00", 9, 9), tmHour(t, 1, "12:00", 2.2, 2.1),
					tmHour(t, 1, "14:00", 1.5, 1.0)}},
			want: horizonTiming{PeakLateAfterS: ip(86400 - 180), First2xAfterS: ip(86400 - 180),
				Above2xS: 3420 + 7200, FallBelow2xAfterS: ip(86400 + 10620)},
		},
		{
			// Rug at 12:33 inside the 1d window: still above 2× then, so the
			// fall is the rug.
			name: "rug mid-window",
			in: timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(24 * time.Hour), Entry: 1, PeakGainPct: 160,
				Rugged: true, RugAt: &rugAt, Fine: []candleRow{tmFine(t, 0, "12:05", 2.5, 2.5), tmFine(t, 0, "12:20", 2.6, 2.4)}},
			want: horizonTiming{PeakLateAfterS: ip(1020), First2xAfterS: ip(120), Above2xS: 900 + 480, FallBelow2xAfterS: ip(1800)},
		},
		{
			name: "never reaches 2x: 0 held",
			in:   in(80, time.Hour, tmFine(t, 0, "12:20", 1.8, 1.5)),
			want: horizonTiming{PeakLateAfterS: ip(17 * 60)},
		},
		{
			name: "still at 2x at the end: censored",
			in:   in(110, time.Hour, tmFine(t, 0, "12:05", 2.1, 2.05)),
			want: horizonTiming{PeakLateAfterS: ip(120), First2xAfterS: ip(120), Above2xS: 3180, Censored: true},
		},
		{
			name: "peak is the entry",
			in:   in(0, time.Hour),
			want: horizonTiming{PeakLateAfterS: ip(0)},
		},
		{
			// The stored peak went through NUMERIC: a high a few ulps off still matches.
			name: "peak within float tolerance",
			in:   in(150, time.Hour, tmFine(t, 0, "12:05", 2.4, 2.4), tmFine(t, 0, "12:10", 2.5000000000000004, 1.5)),
			want: horizonTiming{PeakLateAfterS: ip(420), First2xAfterS: ip(120), Above2xS: 300, FallBelow2xAfterS: ip(720)},
		},
		{
			// Touched 2× and closed under it in the same 5-minute bucket (day
			// one): held on closes, so 0; fell at its end.
			name: "touch and drop, day one",
			in:   in(100, time.Hour, tmFine(t, 0, "12:05", 2, 1.4), tmFine(t, 0, "12:10", 1.5, 1.5)),
			want: horizonTiming{PeakLateAfterS: ip(120), First2xAfterS: ip(120), Above2xS: 0, FallBelow2xAfterS: ip(420)},
		},
		{
			// The same in an hourly bucket on day two (2 Jan 15:00–16:00).
			name: "touch and drop, day two",
			in: timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(72 * time.Hour), Entry: 1, PeakGainPct: 150,
				Fine:   []candleRow{tmFine(t, 0, "12:05", 1.2, 1.1)},
				Coarse: []candleRow{tmHour(t, 1, "15:00", 2.5, 1.5), tmHour(t, 1, "17:00", 1.4, 1.3)}},
			want: horizonTiming{PeakLateAfterS: ip(86400 + 10620), First2xAfterS: ip(86400 + 10620), Above2xS: 0,
				FallBelow2xAfterS: ip(86400 + 14220)},
		},
		{
			// The review probe: post 12:01, late entry 12:02. The bucket 12:00
			// holds the late entry and a 3x high, but the stored late peak is
			// +120%: the 3x came before the late entry. The first 2x after it
			// is 12:30 (closes above: held 12:35 to 12:45), under at 12:45.
			name: "2x before the late entry inside its bucket",
			in: timingInput{EntryAt: tmAt(t, 0, "12:01"), LateAt: tmAt(t, 0, "12:02"), DueAt: tmAt(t, 0, "13:01"), Entry: 1, PeakGainPct: 120,
				Fine: []candleRow{tmFine(t, 0, "12:00", 3, 1), tmFine(t, 0, "12:30", 2.2, 2.2), tmFine(t, 0, "12:40", 1.5, 1.5)}},
			want: horizonTiming{PeakLateAfterS: ip(1740), First2xAfterS: ip(1740), Above2xS: 600, FallBelow2xAfterS: ip(2640)},
		},
		{
			// The only 2x is in the bucket holding the late entry (12:00-12:05,
			// late entry 12:02), and the stored late peak (+150%) says it came
			// after the late entry: first 2x and peak at the late entry; its
			// close (2.5) counts from 12:05 to the end of 12:10 (under).
			name: "2x only in the late entry's bucket, after it",
			in: timingInput{EntryAt: tmAt(t, 0, "12:01"), LateAt: tmAt(t, 0, "12:02"), DueAt: tmAt(t, 0, "13:01"), Entry: 1, PeakGainPct: 150,
				Fine: []candleRow{tmFine(t, 0, "12:00", 2.5, 2.5), tmFine(t, 0, "12:10", 1.2, 1.1)}},
			want: horizonTiming{PeakLateAfterS: ip(60), First2xAfterS: ip(60), Above2xS: 600, FallBelow2xAfterS: ip(840)},
		},
		{
			// The peak (+200%) is only in the late entry's bucket, so its 2x
			// came after the late entry too: first 2x there, although 12:10
			// also reaches 2x.
			name: "peak in the late entry's bucket, later 2x too",
			in: timingInput{EntryAt: tmAt(t, 0, "12:01"), LateAt: tmAt(t, 0, "12:02"), DueAt: tmAt(t, 0, "13:01"), Entry: 1, PeakGainPct: 200,
				Fine: []candleRow{tmFine(t, 0, "12:00", 3, 2.5), tmFine(t, 0, "12:10", 2.2, 1.5)}},
			want: horizonTiming{PeakLateAfterS: ip(60), First2xAfterS: ip(60), Above2xS: 600, FallBelow2xAfterS: ip(840)},
		},
		{
			name: "rugged at the call",
			in: func() timingInput {
				x := in(0, time.Hour)
				at := entry.Add(-2 * time.Minute)
				x.Rugged, x.RugAt = true, &at
				return x
			}(),
			want: horizonTiming{PeakLateAfterS: ip(0)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := candleTiming(tc.in)
			if err != nil {
				t.Fatalf("candleTiming: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %s, want %s", tmStr(got), tmStr(tc.want))
			}
		})
	}
}

// The tracker computes a horizon when only the candles up to its due time
// exist; the backfill sees the later trades in the bucket straddling the due
// time too. Both must give the same timing.
func TestCandleTimingDueBucketSameWithLaterTrades(t *testing.T) {
	entry := tmAt(t, 0, "12:03")
	tests := []struct {
		name        string
		gain        float64
		trunc, full []candleRow
		want        horizonTiming
	}{
		{
			name:  "2x before the due time's bucket",
			gain:  150,
			trunc: []candleRow{tmFine(t, 0, "12:50", 2.5, 2.5), tmFine(t, 0, "13:00", 2.4, 2.4)},
			full:  []candleRow{tmFine(t, 0, "12:50", 2.5, 2.5), tmFine(t, 0, "13:00", 3.0, 1.0)},
			want:  horizonTiming{PeakLateAfterS: ip(2820), First2xAfterS: ip(2820), Above2xS: 480, Censored: true},
		},
		{
			// The first 2x bucket ends after the window: no close in the
			// window, nothing held, not seen under 2x: censored.
			name:  "first 2x in the due time's bucket",
			gain:  120,
			trunc: []candleRow{tmFine(t, 0, "12:50", 1.5, 1.5), tmFine(t, 0, "13:00", 2.2, 2.2)},
			full:  []candleRow{tmFine(t, 0, "12:50", 1.5, 1.5), tmFine(t, 0, "13:00", 3.0, 1.0)},
			want:  horizonTiming{PeakLateAfterS: ip(3420), First2xAfterS: ip(3420), Above2xS: 0, Censored: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, set := range []struct {
				name string
				cs   []candleRow
			}{{"up to the due time", tc.trunc}, {"with later trades", tc.full}} {
				got, err := candleTiming(timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(time.Hour),
					Entry: 1, PeakGainPct: tc.gain, Fine: set.cs})
				if err != nil {
					t.Fatalf("%s: %v", set.name, err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: got %s, want %s", set.name, tmStr(got), tmStr(tc.want))
				}
			}
		})
	}
}

func TestCandleTimingErrors(t *testing.T) {
	entry := tmAt(t, 0, "12:03")
	base := timingInput{EntryAt: entry, LateAt: entry.Add(time.Minute), DueAt: entry.Add(time.Hour), Entry: 1}
	tests := []struct {
		name         string
		edit         func(*timingInput)
		inconsistent bool
	}{
		{"no late entry", func(x *timingInput) { x.Entry = 0 }, false},
		{"rugged without a rug time", func(x *timingInput) { x.Rugged = true }, false},
		{"stored peak at 2x, no candle reaches it", func(x *timingInput) {
			x.PeakGainPct = 150
			x.Fine = []candleRow{tmFine(t, 0, "12:05", 1.5, 1.5)}
		}, true},
		{"no candle reaches the stored peak (under 2x)", func(x *timingInput) {
			x.PeakGainPct = 50
			x.Fine = []candleRow{tmFine(t, 0, "12:05", 1.2, 1.2)}
		}, true},
		{"the stored peak only before the late entry", func(x *timingInput) {
			x.PeakGainPct = 50
			x.Fine = []candleRow{tmFine(t, 0, "11:55", 1.5, 1.5), tmFine(t, 0, "12:05", 1.2, 1.2)}
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := base
			tc.edit(&x)
			got, err := candleTiming(x)
			if err == nil {
				t.Fatalf("got %s and no error, want an error", tmStr(got))
			}
			if errors.Is(err, errTimingInconsistent) != tc.inconsistent {
				t.Errorf("errors.Is(%v, errTimingInconsistent) = %v, want %v", err, !tc.inconsistent, tc.inconsistent)
			}
		})
	}
}

func TestRugKindOf(t *testing.T) {
	tests := []struct {
		name string
		st   onchainState
		want string
	}{
		{"drained before the call", onchainState{EntryBlock: 100, RugBlock: 90, ScanBlock: 100}, rugAtAtCall},
		{"thin at the call", onchainState{EntryBlock: 100, RugBlock: 100, ScanBlock: 100}, rugAtAtCall},
		{"found by the horizon scan", onchainState{EntryBlock: 100, RugBlock: 150, ScanBlock: 200}, rugAtEvent},
		{"found by the latest pass", onchainState{EntryBlock: 100, RugBlock: 250, ScanBlock: 200, LatestBlock: 260}, rugAtEvent},
		{"end-of-tracking check", onchainState{EntryBlock: 100, RugBlock: 300, ScanBlock: 200, LatestBlock: 260}, rugAtDetected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rugKindOf(&tc.st); got != tc.want {
				t.Errorf("rugKindOf(entry %d, rug %d, scan %d, latest %d) = %q, want %q",
					tc.st.EntryBlock, tc.st.RugBlock, tc.st.ScanBlock, tc.st.LatestBlock, got, tc.want)
			}
		})
	}
}

// -backfill-timing's rug-time step gives up after rugLookupGiveUp node
// failures in a row, at any point; a lookup that succeeds starts the count
// again, and a failure to store (database) does not count.
func TestBackfillRugTimesGivesUp(t *testing.T) {
	errStore := errors.New("store failed")
	tests := []struct {
		name string
		// one letter per rugged call: o = lookup and store succeed, f = the
		// node fails the lookup, w = lookup succeeds, storing fails
		calls                              string
		lookups                            int
		stored, failed, writeFail, skipped int
	}{
		{"every lookup failing", "fffff", 3, 0, 3, 0, 2},
		{"a success, then 3 failures", "offfoo", 4, 1, 3, 0, 2},
		{"failures between successes", "ffoffofo", 8, 3, 5, 0, 0},
		{"store failures do not count", "wwwwffo", 7, 1, 2, 4, 0},
		{"store failures between node failures", "ffwffo", 6, 1, 4, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := ltChain(t, ltDay)
			fail := map[uint64]bool{}
			rugs := make([]rugAtBackfillRow, len(tc.calls))
			writeFail := map[int]bool{}
			for i, c := range tc.calls {
				block := uint64(100 + i)
				rugs[i] = rugAtBackfillRow{CallID: i + 1, State: onchainState{EntryBlock: 10, RugBlock: block}}
				fail[block] = c == 'f'
				writeFail[i+1] = c == 'w'
			}
			f.mu.Lock()
			f.blockErr = func(n uint64) string {
				if fail[n] {
					return "header not found"
				}
				return ""
			}
			f.mu.Unlock()
			s := ltScanner(t, nil, f.srv.URL)
			f.tookAll()
			var set []int
			setter := func(_ context.Context, callID int, _ time.Time, _ string) (bool, error) {
				if writeFail[callID] {
					return false, errStore
				}
				set = append(set, callID)
				return true, nil
			}
			stats := timingBackfillStats{RugCalls: len(rugs)}
			var out bytes.Buffer
			if err := s.backfillRugTimes(context.Background(), &out, rugs, setter, &stats); err != nil {
				t.Fatal(err)
			}
			if stats.RugStored != tc.stored || stats.RugFailed != tc.failed || stats.RugWriteFail != tc.writeFail || stats.RugSkipped != tc.skipped {
				t.Errorf("calls %q: got %d stored, %d failed, %d store failures, %d skipped; want %d, %d, %d, %d",
					tc.calls, stats.RugStored, stats.RugFailed, stats.RugWriteFail, stats.RugSkipped, tc.stored, tc.failed, tc.writeFail, tc.skipped)
			}
			if len(set) != tc.stored {
				t.Errorf("calls %q: got %d rug time(s) stored (%v), want %d", tc.calls, len(set), set, tc.stored)
			}
			if n := f.tookAll(); n["eth_getBlockByNumber"] != tc.lookups {
				t.Errorf("calls %q: got %d block lookups, want %d", tc.calls, n["eth_getBlockByNumber"], tc.lookups)
			}
			if want := fmt.Sprintf("%d not looked up", tc.skipped); !strings.Contains(out.String(), want) {
				t.Errorf("calls %q: output %q does not contain %q", tc.calls, out.String(), want)
			}
		})
	}
}
