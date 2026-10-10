package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// Timing of a horizon (scout_call_returns.peak_late_after_s, first_2x_after_s,
// above_2x_s, fall_below_2x_after_s, above_2x_censored, timing_at) and the
// time of a rug (scout_call_tracking.rug_at, rug_at_kind).
//
// Computed from the stored candles only (no node request): 5-minute candles
// before entry_at + 24 h, hourly candles from there on. The tracker computes
// them right after each on-chain horizon is stored; -backfill-timing fills in
// horizons stored before the columns existed. These are outcomes: never model
// inputs.
// ---------------------------------------------------------------------------

// Kinds of scout_call_tracking.rug_at.
const (
	rugAtEvent    = "event"    // the time of the price event that showed the pool drained
	rugAtAtCall   = "at_call"  // drained at or before the call (the time can be before entry_at)
	rugAtDetected = "detected" // found only by the end-of-tracking liquidity check: the time of that check
)

// timingTol: relative tolerance when a candle high is compared with the
// stored late peak or with 2× the late entry (both went through NUMERIC).
const timingTol = 1e-9

// errTimingInconsistent: the candles do not reach the stored late peak (or
// its 2×). The horizon keeps timing_at NULL, so -backfill-timing tries again
// (e.g. once the missing candles are stored).
var errTimingInconsistent = errors.New("candles inconsistent with the stored peak")

// timingInput is what one horizon's timing is computed from. Prices are in the
// call's price unit (the candles' and entry_late_price_usd's unit).
type timingInput struct {
	EntryAt     time.Time   // the post: the time origin
	LateAt      time.Time   // entry_at + SCOUT_ENTRY_DELAY: the realistic entry
	DueAt       time.Time   // the horizon's end
	Entry       float64     // E = entry_late_price_usd
	PeakGainPct float64     // the horizon's max_gain_late_pct
	Rugged      bool        // the horizon is rugged (price 0 at its end)
	RugAt       *time.Time  // scout_call_tracking.rug_at (needed when Rugged)
	Fine        []candleRow // 300 s candles, oldest first
	Coarse      []candleRow // 3600 s candles, oldest first
}

// horizonTiming is the result; seconds are counted from entry_at.
type horizonTiming struct {
	PeakLateAfterS    *int // first bucket reaching the stored late peak (0 when the peak is the entry)
	First2xAfterS     *int // first bucket whose high reached 2E (nil = never)
	Above2xS          int  // seconds a close was at or above 2E, carried forward (0 = never reached 2×, or never closed at 2×)
	FallBelow2xAfterS *int // first bucket from the first 2× on whose close is under 2E, or the rug (nil = never reached, or censored)
	Censored          bool // reached 2× and not seen under it by the end of the window (no rug)
}

// timingSeg is one candle on the series.
type timingSeg struct {
	s        int64 // start, clipped to entry_at (5-minute) or entry_at + 24 h (hourly), and to the late entry (straddle)
	e        int64 // when its close was reached: the bucket's end (a 5-minute bucket: at most entry_at + 24 h)
	h, c     float64
	straddle bool // the bucket holds the late entry: its high and close may come from trades before it
}

// candleTiming computes a horizon's timing from its candles.
//
// The series: 5-minute candles before entry_at + 24 h, hourly candles from
// then on; the first bucket is clipped to start at entry_at, the hourly bucket
// straddling entry_at + 24 h counts from entry_at + 24 h. The window is
// (entry_at, min(due_at, rug_at)]. Times are seconds after entry_at;
// resolution 5 minutes on day one, 1 hour after.
//
// The late entry: buckets that end at or before it (LateAt) hold only trades
// from before it and are left out. The bucket straddling it (the
// "straddle") is clipped to start at the late entry; its high may come from a
// trade before the late entry, so it is used for the peak and the first 2×
// only when no later bucket matches (then the stored late peak, which only
// counts trades after the late entry, proves the hit was after it).
//
//   - peak: the (clipped) start of the first bucket whose high equals the
//     stored late peak E × (1 + max_gain_late_pct/100), preferring a later
//     bucket over the straddle; else the first whose high is at or above it,
//     again preferring a later bucket; 0 when the peak is the entry itself.
//     No bucket reaches it: errTimingInconsistent.
//   - reached 2×: the stored late peak is at least 2E (candle highs also hold
//     trades between the post and the late entry, the stored peak does not);
//     first 2× = the straddle when the peak was found there (a trade after
//     the late entry reached the peak, so 2×), else the first later bucket
//     whose high is at least 2E, else the straddle.
//   - time at 2× is measured on closes: from the first 2× bucket on, each
//     bucket's close counts from the bucket's end and is carried forward
//     (across gaps) to the next bucket's end or the window's end. A bucket
//     whose high touched 2× but closed under it adds nothing. The straddle's
//     close only counts when it is the first 2× bucket, and then it is a
//     price after the late entry (the bucket has a trade after it). Closes
//     reached after the window's end are not used (a bucket straddling the
//     due time gives the same result with or without the trades after the
//     due time); when the first 2× bucket ends after the window's end,
//     nothing is held.
//   - fall below 2×: the end of the first bucket, from the first 2× bucket
//     on, whose close is under 2E; else the rug time when the window ends
//     with a rug; else nil and Censored.
func candleTiming(in timingInput) (horizonTiming, error) {
	var out horizonTiming
	if !(in.Entry > 0) {
		return out, errors.New("no late entry price")
	}
	entry := in.EntryAt.Unix()
	end := in.DueAt.Unix()
	if in.Rugged {
		if in.RugAt == nil {
			return out, errors.New("rugged without a rug time")
		}
		end = min(end, in.RugAt.Unix())
	}
	end = max(end, entry)
	day := entry + candleFineSpan
	late := max(in.LateAt.Unix(), entry)

	// The series, in time order.
	var segs []timingSeg
	// add: a bucket [st, st+span) counted from `from` on, its close at most at `upTo`.
	add := func(st, span, from, upTo int64, h, c float64) {
		if st+span <= late {
			return // only trades before the late entry
		}
		g := timingSeg{s: max(st, from), e: min(st+span, upTo), h: h, c: c, straddle: st < late}
		if g.straddle {
			g.s = max(g.s, late)
		}
		if g.s < end {
			segs = append(segs, g)
		}
	}
	for _, c := range in.Fine {
		if st := c.Start.Unix(); st < day {
			add(st, candleFineS, entry, day, c.H, c.C)
		}
	}
	if end > day {
		for _, c := range in.Coarse {
			if st := c.Start.Unix(); st+candleCoarseS > day {
				add(st, candleCoarseS, day, math.MaxInt64, c.H, c.C)
			}
		}
	}
	secs := func(t int64) *int { v := int(t - entry); return &v }
	// find: the first bucket matching, preferring buckets after the straddle.
	find := func(match func(timingSeg) bool) int {
		strad := -1
		for i, g := range segs {
			if !match(g) {
				continue
			}
			if !g.straddle {
				return i
			}
			if strad < 0 {
				strad = i
			}
		}
		return strad
	}

	// Peak.
	peak := in.Entry * (1 + in.PeakGainPct/100)
	peakAt := -1
	if peak <= in.Entry*(1+timingTol) {
		out.PeakLateAfterS = secs(entry)
	} else {
		if peakAt = find(func(g timingSeg) bool { return math.Abs(g.h-peak) <= timingTol*peak }); peakAt < 0 {
			peakAt = find(func(g timingSeg) bool { return g.h >= peak*(1-timingTol) })
		}
		if peakAt < 0 {
			return out, fmt.Errorf("%w: no candle reaches the late peak %.6g", errTimingInconsistent, peak)
		}
		out.PeakLateAfterS = secs(segs[peakAt].s)
	}

	// 2×.
	two := 2 * in.Entry * (1 - timingTol)
	if peak < two {
		return out, nil // never reached 2×: 0 seconds held
	}
	first := find(func(g timingSeg) bool { return g.h >= two })
	if peakAt >= 0 && segs[peakAt].straddle {
		first = peakAt
	}
	if first < 0 {
		return out, fmt.Errorf("%w: late peak %.6g is at least 2x the entry %.6g but no candle reaches it", errTimingInconsistent, peak, in.Entry)
	}
	out.First2xAfterS = secs(segs[first].s)
	// Held: closes only, from the end of the first 2× bucket on.
	var held int64
	at, above := int64(0), false
	for _, g := range segs[first:] {
		if g.e > end {
			break // its close may hold trades after the window's end
		}
		if above {
			held += g.e - at
		}
		at, above = g.e, g.c >= two
		if !above && out.FallBelow2xAfterS == nil {
			out.FallBelow2xAfterS = secs(g.e)
		}
	}
	if above {
		held += end - at
	}
	out.Above2xS = int(held)
	if out.FallBelow2xAfterS == nil {
		if in.Rugged {
			out.FallBelow2xAfterS = secs(end)
		} else {
			out.Censored = true
		}
	}
	return out, nil
}

// rugKindOf classifies the rug in a state: drained at or before the call;
// found by a scan (the horizon scan or the latest-price pass, whose cursors
// have passed it); or else set by the end-of-tracking liquidity check (to the
// node's head at that moment, beyond both cursors).
func rugKindOf(st *onchainState) string {
	switch {
	case st.RugBlock <= st.EntryBlock:
		return rugAtAtCall
	case st.RugBlock <= st.ScanBlock || st.RugBlock <= st.LatestBlock:
		return rugAtEvent
	}
	return rugAtDetected
}

// ensureRugAt returns the call's rug time: the stored rug_at, or else the
// exact time of the rug block (one node request), which is then stored. A
// failed lookup is remembered in the state for the rest of this run (not
// stored), so the call's other rugged horizons do not ask the node again.
func (s *scanner) ensureRugAt(ctx context.Context, callID int, st *onchainState) (*time.Time, error) {
	if st.rugAtErr != nil && st.rugAtErrBlock == st.RugBlock {
		return nil, st.rugAtErr
	}
	at, err := s.db.RugAt(ctx, callID)
	if err != nil {
		return nil, fmt.Errorf("read rug time: %w", err)
	}
	if at != nil {
		return at, nil
	}
	ts, err := s.onchain.rpc.blockTime(ctx, st.RugBlock)
	if err != nil {
		err = fmt.Errorf("time of rug block %d: %w", st.RugBlock, err)
		if ctx.Err() == nil {
			st.rugAtErr, st.rugAtErrBlock = err, st.RugBlock
		}
		return nil, err
	}
	t := time.Unix(ts, 0).UTC()
	if err := s.db.SetRugAt(ctx, callID, t, rugKindOf(st)); err != nil {
		return nil, fmt.Errorf("save rug time: %w", err)
	}
	return &t, nil
}

// fillHorizonTiming adds the timing to an on-chain horizon result before it
// is stored. On any failure the horizon is stored without it (timing_at NULL,
// so -backfill-timing can fill it in later); the failure is only logged.
func (s *scanner) fillHorizonTiming(ctx context.Context, t *ScoutCallTracking, st *onchainState, r *horizonResult, late *float64, rugged bool, tag string) {
	if late == nil || r.MaxGainLatePct == nil || r.Status != "done" {
		return
	}
	in := timingInput{EntryAt: t.EntryAt, LateAt: t.EntryAt.Add(s.onchain.cfg.EntryDelay), DueAt: r.DueAt,
		Entry: *late, PeakGainPct: *r.MaxGainLatePct, Rugged: rugged}
	if rugged {
		at, err := s.ensureRugAt(ctx, t.CallID, st)
		if err != nil {
			log.Printf("%s: +%s timing not computed: %v", tag, r.Horizon, err)
			return
		}
		in.RugAt = at
	}
	var err error
	if in.Fine, err = s.db.CandlesForCall(ctx, t.CallID, candleFineS); err == nil && r.DueAt.After(t.EntryAt.Add(candleFineSpan*time.Second)) {
		in.Coarse, err = s.db.CandlesForCall(ctx, t.CallID, candleCoarseS)
	}
	if err != nil {
		log.Printf("%s: +%s timing not computed: read candles: %v", tag, r.Horizon, err)
		return
	}
	tm, err := candleTiming(in)
	if err != nil {
		log.Printf("%s: +%s timing not computed: %v", tag, r.Horizon, err)
		return
	}
	r.setTiming(tm, time.Now().UTC())
}

// setTiming copies a computed timing into the result (stored by UpsertReturn).
func (r *horizonResult) setTiming(tm horizonTiming, at time.Time) {
	above, cens := tm.Above2xS, tm.Censored
	r.PeakLateAfterS, r.First2xAfterS, r.FallBelow2xAfterS = tm.PeakLateAfterS, tm.First2xAfterS, tm.FallBelow2xAfterS
	r.Above2xS, r.Above2xCensored, r.TimingAt = &above, &cens, &at
}
