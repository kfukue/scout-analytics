package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ---------------------------------------------------------------------------
// Performance tracker: for every call, find its pool, the entry price at the
// call time, and the return / peak gain / drawdown after each horizon
// (1h, 1d, 3d, 7d, 30d). Runs inside the listener (or alone with -track).
// ---------------------------------------------------------------------------

// trackLoop processes due tracking rows every PriceCfg.Interval.
func (s *scanner) trackLoop(ctx context.Context) {
	if s.db == nil || !s.pc.Enabled {
		return
	}
	if s.pc.Source == "onchain" {
		log.Printf("performance tracking on: %s via %s", horizonNames(s.pc.Horizons), s.onchain.describe())
	} else {
		log.Printf("performance tracking on: %s via %s (network %q, %d req/min)",
			horizonNames(s.pc.Horizons), s.pc.BaseURL, s.pc.Network, s.pc.RPM)
	}
	t := time.NewTicker(s.pc.Interval)
	defer t.Stop()
	for {
		if n := s.trackDue(ctx, 50); n > 0 {
			log.Printf("tracking: processed %d call(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func horizonNames(hs []horizon) string {
	s := ""
	for i, h := range hs {
		if i > 0 {
			s += ", "
		}
		s += h.Name
	}
	return s
}

// trackDue processes up to limit due calls; returns how many were processed.
func (s *scanner) trackDue(ctx context.Context, limit int) int {
	rows, err := s.db.DueTracking(ctx, time.Now(), limit)
	if err != nil {
		log.Printf("tracking: %v", err)
		return 0
	}
	n := 0
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		if s.pc.Source == "onchain" {
			s.trackOneOnchain(ctx, &rows[i])
		} else {
			s.trackOne(ctx, &rows[i])
		}
		n++
	}
	return n
}

func backoff(attempts int) time.Duration {
	d := 15 * time.Minute
	for i := 1; i < attempts && d < 6*time.Hour; i++ {
		d *= 2
	}
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}

// trackOne updates one call. API calls: ≤1 pools + 1 hourly + ≤1 minute (+1 pools at the end).
func (s *scanner) trackOne(ctx context.Context, t *ScoutCallTracking) {
	now := time.Now().UTC()
	t.Attempts++
	t.LastCheckedAt = &now
	t.Error = nil
	deadline := t.EntryAt.Add(s.pc.maxHorizon() + 48*time.Hour)
	fail := func(status string, err error, retryIn time.Duration) {
		msg := err.Error()
		t.Error = &msg
		t.Status = status
		if now.After(deadline) {
			t.Status = TrackGaveUp
		}
		t.NextCheckAt = now.Add(retryIn)
		log.Printf("tracking call %d (%s): %s: %v", t.CallID, t.ContractAddress, t.Status, err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.db.SaveTracking(sctx, t); err != nil {
			log.Printf("tracking call %d: save: %v", t.CallID, err)
		}
	}()

	// 1. Pool.
	if t.PoolAddress == nil {
		if known, err := s.db.KnownPool(ctx, t.ContractAddress); err == nil && known != nil {
			t.PoolAddress, t.PoolName, t.PoolDex, t.PoolCreatedAt = known.PoolAddress, known.PoolName, known.PoolDex, known.PoolCreatedAt
		} else {
			pools, err := s.gecko.tokenPools(ctx, t.ContractAddress)
			if errors.Is(err, errNotFound) || (err == nil && len(pools) == 0) {
				fail(TrackNoPool, fmt.Errorf("no pool for %s on %s yet", t.ContractAddress, s.pc.Network), 6*time.Hour)
				return
			}
			if err != nil {
				fail(TrackError, err, backoff(t.Attempts))
				return
			}
			p := pickPool(pools, t.EntryAt)
			t.PoolAddress, t.PoolName, t.PoolDex, t.PoolCreatedAt = &p.Address, &p.Name, &p.Dex, p.CreatedAt
			t.CurrentLiquidityUSD, t.CurrentPriceUSD = &p.ReserveUSD, &p.PriceUSD
		}
	}
	pool := *t.PoolAddress

	// 2. Hourly candles from the entry hour to the last horizon (or now).
	before := t.EntryAt.Add(s.pc.maxHorizon() + time.Hour)
	if before.After(now) {
		before = now
	}
	startHour := t.EntryAt.Truncate(time.Hour)
	limit := int(before.Sub(startHour)/time.Hour) + 2
	hourly, err := s.gecko.ohlcv(ctx, pool, t.ContractAddress, "hour", 1, before, limit)
	if err != nil {
		fail(TrackError, fmt.Errorf("hourly candles: %w", err), backoff(t.Attempts))
		return
	}

	// 3. Entry price: minute candles around the call, else the hourly candle.
	if t.EntryPriceUSD == nil {
		if minute, err := s.gecko.ohlcv(ctx, pool, t.ContractAddress, "minute", 1, t.EntryAt.Add(30*time.Minute), 60); err == nil {
			if p, ok := entryFromCandles(minute, time.Minute, t.EntryAt, 15*time.Minute); ok {
				src := "minute"
				t.EntryPriceUSD, t.EntryPriceSource = &p, &src
			}
		}
		if t.EntryPriceUSD == nil {
			if p, ok := entryFromCandles(hourly, time.Hour, t.EntryAt, time.Hour); ok {
				src := "hour"
				t.EntryPriceUSD, t.EntryPriceSource = &p, &src
			}
		}
		if t.EntryPriceUSD == nil {
			fail(TrackError, errors.New("no trades around the call time yet"), time.Hour)
			return
		}
	}

	// 4. Horizons.
	var next time.Time
	for _, h := range s.pc.Horizons {
		r := horizonStats(hourly, t.EntryAt, *t.EntryPriceUSD, h, now)
		if r.Status == "pending" {
			if next.IsZero() || r.DueAt.Before(next) {
				next = r.DueAt
			}
			continue
		}
		if err := s.db.UpsertReturn(ctx, t.CallID, h, r); err != nil {
			fail(TrackError, fmt.Errorf("save %s: %w", h.Name, err), backoff(t.Attempts))
			return
		}
	}
	if n := len(hourly); n > 0 {
		c := hourly[n-1].C
		t.CurrentPriceUSD = &c
	}

	// 5. Schedule, or finish (refresh liquidity once to flag rugs).
	if !next.IsZero() {
		t.Status = TrackTracking
		t.NextCheckAt = next.Add(10 * time.Minute)
		return
	}
	if pools, err := s.gecko.tokenPools(ctx, t.ContractAddress); err == nil {
		for _, p := range pools {
			if p.Address == pool {
				liq := p.ReserveUSD
				t.CurrentLiquidityUSD = &liq
			}
		}
	}
	rug := false
	if t.CurrentLiquidityUSD != nil && *t.CurrentLiquidityUSD < s.pc.RugLiqUSD {
		rug = true
	}
	if t.CurrentPriceUSD != nil && *t.CurrentPriceUSD < *t.EntryPriceUSD*0.05 {
		rug = true
	}
	t.Rugged = &rug
	t.Status = TrackDone
	t.NextCheckAt = now
}

// firstCheckAt: the first price check is right after the shortest horizon.
func (s *scanner) firstCheckAt(entry time.Time) time.Time {
	return entry.Add(s.pc.Horizons[0].Dur + 5*time.Minute)
}

// ---------------------------------------------------------------------------
// Backfill: import past calls from the channel history (no bot scans), so the
// tracker can compute their performance from historical candles.
// ---------------------------------------------------------------------------

type backfillStats struct {
	Posts, Calls, CAs int
	New, Existing     int // CAs newly recorded vs. already in the DB (re-runs are safe)
}

// backfillMessage records the calls in one historical post.
func (s *scanner) backfillMessage(m *tg.Message, st *backfillStats) {
	st.Posts++
	urls := postURLs(m)
	cas := extractCAs(m.Message, urls, s.cfg.Chains)
	if len(cas) == 0 {
		return
	}
	st.Calls++
	for _, ca := range cas {
		id, created := s.recordCallInfo(m, ca, urls, CallStatusBackfill)
		if id == nil {
			continue
		}
		st.CAs++
		if created {
			st.New++
		} else {
			st.Existing++
		}
	}
}

// backfill walks the source channel from post toID (0 = newest) down to fromID.
func (s *scanner) backfill(ctx context.Context, fromID, toID, maxPosts int) (backfillStats, error) {
	var st backfillStats
	offset := 0
	if toID > 0 {
		offset = toID + 1
	}
	for {
		res, err := s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: s.sourcePeer, OffsetID: offset, Limit: 100})
		if d, ok := tgerr.AsFloodWait(err); ok {
			log.Printf("backfill: Telegram asks to wait %s", d)
			if err := sleepCtx(ctx, d+time.Second); err != nil {
				return st, err
			}
			continue
		}
		if err != nil {
			return st, err
		}
		msgs := messagesOf(res) // ascending
		if len(msgs) == 0 {
			return st, nil
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.ID < fromID || (maxPosts > 0 && st.Posts >= maxPosts) {
				return st, nil
			}
			s.backfillMessage(m, &st)
		}
		log.Printf("backfill: down to post %d — %d posts, %d calls, %d CAs (%d new, %d already recorded)",
			msgs[0].ID, st.Posts, st.Calls, st.CAs, st.New, st.Existing)
		if msgs[0].ID <= 1 {
			return st, nil
		}
		offset = msgs[0].ID
		if err := sleepCtx(ctx, time.Second); err != nil {
			return st, err
		}
	}
}
