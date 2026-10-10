package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"
)

// timingBackfillChunk: calls whose horizons are read, computed and written
// together (one batch of UPDATEs).
const timingBackfillChunk = 200

// timingBackfillStats is what -backfill-timing did (or, with -dry-run, would do).
type timingBackfillStats struct {
	RugCalls     int // rugged calls without a rug time = node lookups (one eth_getBlockByNumber each)
	RugStored    int
	RugFailed    int // node lookups that failed
	RugWriteFail int // looked up, but storing the rug time failed (database)
	RugSkipped   int // not looked up: the node failed rugLookupGiveUp lookups in a row
	Calls        int // calls with horizons to compute
	Horizons     int // horizons to compute
	Written      int
	NoRugTime    int // rugged horizons skipped: no rug time (lookup failed, or rugged without a rug block)
	Inconsistent int // skipped: the candles do not reach the stored late peak (or its 2×)
	Failed       int // other failures (computing or reading candles)
}

// runTimingBackfill fills in the timing of done on-chain horizons stored
// without it (timing_at NULL) from the stored candles, and the rug time of
// rugged calls without one (one exact block-time lookup on the node per call,
// paced by the RPC client's limits). Only the new columns are written: status,
// schedules, on-chain state, returns and candles stay as they are, and no
// call is tracked again. Rows that got their timing or rug time meanwhile are
// left alone, so running it again is harmless. dryRun only counts.
func runTimingBackfill(ctx context.Context, s *scanner, w io.Writer, dryRun bool) (timingBackfillStats, error) {
	var stats timingBackfillStats
	rugs, err := s.db.RugAtBackfillRows(ctx)
	if err != nil {
		return stats, err
	}
	stats.RugCalls = len(rugs)
	ids, err := s.db.TimingBackfillCallIDs(ctx)
	if err != nil {
		return stats, err
	}
	stats.Calls = len(ids)

	if dryRun {
		lookup := map[int]bool{}
		for _, r := range rugs {
			lookup[r.CallID] = true
		}
		waiting := 0
		for start := 0; start < len(ids); start += timingBackfillChunk {
			rows, err := s.db.TimingBackfillHorizons(ctx, ids[start:min(start+timingBackfillChunk, len(ids))])
			if err != nil {
				return stats, err
			}
			stats.Horizons += len(rows)
			for _, r := range rows {
				if timingRowRugged(r) && r.RugAt == nil {
					if lookup[r.CallID] {
						waiting++
					} else {
						stats.NoRugTime++
					}
				}
			}
		}
		fmt.Fprintf(w, "backfill-timing (dry run): %d horizon(s) of %d call(s) to compute from the stored candles\n", stats.Horizons, stats.Calls)
		fmt.Fprintf(w, "backfill-timing (dry run): %d rugged call(s) without a rug time: %d node lookup(s) (eth_getBlockByNumber, one per call)\n",
			stats.RugCalls, stats.RugCalls)
		fmt.Fprintf(w, "backfill-timing (dry run): %d rugged horizon(s) wait for those lookups; %d rugged horizon(s) have no rug block and would be skipped\n",
			waiting, stats.NoRugTime)
		return stats, nil
	}

	// 1. Rug times (before the timing: a rugged horizon's window ends at the rug).
	// When the node does not answer, the step gives up (backfillRugTimes): the
	// rugged horizons without a rug time are left (counted in NoRugTime) for a
	// later run; the others are still filled in.
	if len(rugs) > 0 {
		if err := s.backfillRugTimes(ctx, w, rugs, s.db.SetRugAtIfMissing, &stats); err != nil {
			return stats, err
		}
	}

	// 2. Timing, by chunks of calls.
	delay := s.onchain.cfg.EntryDelay
	for start := 0; start < len(ids); start += timingBackfillChunk {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		rows, err := s.db.TimingBackfillHorizons(ctx, ids[start:min(start+timingBackfillChunk, len(ids))])
		if err != nil {
			return stats, err
		}
		stats.Horizons += len(rows)
		var writes []timingWrite
		for i := 0; i < len(rows); {
			j := i
			for j < len(rows) && rows[j].CallID == rows[i].CallID {
				j++
			}
			ws, err := s.timingOfCall(ctx, rows[i:j], delay, &stats)
			if err != nil {
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				stats.Failed += j - i
				log.Printf("backfill-timing: call %d: %v", rows[i].CallID, err)
			}
			writes = append(writes, ws...)
			i = j
		}
		n, err := s.db.SetHorizonTimings(ctx, writes)
		stats.Written += n
		if err != nil {
			return stats, err
		}
		log.Printf("backfill-timing: %d / %d calls, %d horizon(s) written", min(start+timingBackfillChunk, len(ids)), len(ids), stats.Written)
	}
	fmt.Fprintf(w, "backfill-timing: %d horizon(s) of %d call(s): %d written, %d skipped without a rug time, %d skipped (candles inconsistent with the stored peak), %d failed\n",
		stats.Horizons, stats.Calls, stats.Written, stats.NoRugTime, stats.Inconsistent, stats.Failed)
	return stats, nil
}

// timingRowRugged: the horizon was stored as rugged (price 0: a horizon not
// rugged always has a positive price).
func timingRowRugged(r timingBackfillRow) bool {
	return r.PriceUSD != nil && *r.PriceUSD == 0
}

// timingOfCall computes the timing of one call's horizons (rows) from its
// stored candles. Horizons that cannot be computed are counted in stats and
// left out.
func (s *scanner) timingOfCall(ctx context.Context, rows []timingBackfillRow, delay time.Duration, stats *timingBackfillStats) ([]timingWrite, error) {
	id, entryAt := rows[0].CallID, rows[0].EntryAt
	fine, err := s.db.CandlesForCall(ctx, id, candleFineS)
	if err != nil {
		return nil, err
	}
	var coarse []candleRow
	for _, r := range rows {
		if r.DueAt.After(entryAt.Add(candleFineSpan * time.Second)) {
			if coarse, err = s.db.CandlesForCall(ctx, id, candleCoarseS); err != nil {
				return nil, err
			}
			break
		}
	}
	var out []timingWrite
	for _, r := range rows {
		rugged := timingRowRugged(r)
		if rugged && r.RugAt == nil {
			stats.NoRugTime++
			continue
		}
		tm, err := candleTiming(timingInput{EntryAt: r.EntryAt, LateAt: r.EntryAt.Add(delay), DueAt: r.DueAt,
			Entry: r.EntryLate, PeakGainPct: r.MaxGainLatePct, Rugged: rugged, RugAt: r.RugAt, Fine: fine, Coarse: coarse})
		if errors.Is(err, errTimingInconsistent) {
			stats.Inconsistent++
			log.Printf("backfill-timing: call %d +%s: %v", id, r.Horizon, err)
			continue
		}
		if err != nil {
			stats.Failed++
			log.Printf("backfill-timing: call %d +%s: %v", id, r.Horizon, err)
			continue
		}
		out = append(out, timingWrite{CallID: id, Horizon: r.Horizon, T: tm})
	}
	return out, nil
}

// rugLookupGiveUp: rug-time lookups failed in a row after which the node is
// taken as unreachable and the rest are skipped (each failed lookup can take
// several retries). A lookup that succeeds starts the count again.
const rugLookupGiveUp = 3

// rugAtSetter stores a rug time unless the call has one
// ((*ScoutStore).SetRugAtIfMissing); it reports whether it wrote.
type rugAtSetter func(ctx context.Context, callID int, at time.Time, kind string) (bool, error)

// backfillRugTimes looks up the rug time of each rugged call without one and
// stores it with set (step 1 of runTimingBackfill). Failures are counted, not
// returned; only a cancelled context is. After rugLookupGiveUp node failures
// in a row the remaining calls are skipped; failures to store do not count
// towards that (the node answered).
func (s *scanner) backfillRugTimes(ctx context.Context, w io.Writer, rugs []rugAtBackfillRow, set rugAtSetter, stats *timingBackfillStats) error {
	var firstErr error
	last := time.Now()
	streak := 0 // node lookups failed in a row
	for i, r := range rugs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if streak >= rugLookupGiveUp {
			stats.RugSkipped = len(rugs) - i
			log.Printf("backfill-timing: %d rug-time lookups in a row failed (node unreachable?): %d rugged call(s) not looked up; their rugged horizons are skipped", streak, stats.RugSkipped)
			break
		}
		st := r.State
		ts, err := s.onchain.rpc.blockTime(ctx, st.RugBlock)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			streak++
			stats.RugFailed++
			if firstErr == nil {
				firstErr = fmt.Errorf("call %d, rug block %d: %w", r.CallID, st.RugBlock, err)
			}
		} else {
			streak = 0
			ok, err := set(ctx, r.CallID, time.Unix(ts, 0).UTC(), rugKindOf(&st))
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				stats.RugWriteFail++
				if firstErr == nil {
					firstErr = fmt.Errorf("call %d: %w", r.CallID, err)
				}
			}
			if ok {
				stats.RugStored++
			}
		}
		if time.Since(last) >= 30*time.Second {
			log.Printf("backfill-timing: rug times %d / %d", i+1, len(rugs))
			last = time.Now()
		}
	}
	fmt.Fprintf(w, "backfill-timing: rug times: %d stored, %d lookup(s) failed, %d not stored (database error), %d not looked up (of %d rugged calls without one)\n",
		stats.RugStored, stats.RugFailed, stats.RugWriteFail, stats.RugSkipped, stats.RugCalls)
	if firstErr != nil {
		fmt.Fprintf(w, "backfill-timing: first rug-time failure: %v\n", firstErr)
	}
	return nil
}
