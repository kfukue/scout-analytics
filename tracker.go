package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ---------------------------------------------------------------------------
// Performance tracker: for every call, find its pool, the entry price at the
// call time, and the return / peak gain / drawdown after each horizon
// (1h, 1d, 3d, 7d, 30d). Runs inside the listener (or alone with -track).
// Only the first call of each token is tracked; later calls get status "repeat".
// ---------------------------------------------------------------------------

// trackLoop processes due tracking rows every PriceCfg.Interval.
func (s *scanner) trackLoop(ctx context.Context) {
	if s.db == nil || !s.pc.Enabled {
		return
	}
	if s.pc.Source == "onchain" {
		log.Printf("performance tracking on: %s via %s; %d call(s) at a time", horizonNames(s.pc.Horizons), s.onchain.describe(), max(s.pc.Workers, 1))
	} else {
		log.Printf("performance tracking on: %s via %s (network %q, %d req/min)",
			horizonNames(s.pc.Horizons), s.pc.BaseURL, s.pc.Network, s.pc.RPM)
	}
	t := time.NewTicker(s.pc.Interval)
	defer t.Stop()
	for {
		n := s.trackDue(ctx, 50)
		if ctx.Err() != nil {
			return
		}
		s.logTrackingStatus(ctx, n)
		s.fillTokenNames(ctx)
		if n == 50 {
			continue // more are waiting: keep going without the pause
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

// commas writes n with thousands separators: 3120 → "3,120".
func commas(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// trackDue processes up to limit due calls; returns how many were processed.
// Only the first call of each token is tracked (see MarkRepeatTracking).
func (s *scanner) trackDue(ctx context.Context, limit int) int {
	// Only the first call of each token is tracked: later calls are set aside
	// before the due rows are read (one statement; writes nothing when all is in place).
	if n, err := s.db.MarkRepeatTracking(ctx); err != nil {
		log.Printf("tracking: repeat calls: %v", err)
		if ctx.Err() != nil {
			return 0
		}
	} else if n > 0 {
		log.Printf("tracking: %s repeat call(s) skipped — only the first call of each token is tracked", commas(n))
	}
	rows, err := s.db.DueTracking(ctx, time.Now(), limit)
	if err != nil {
		log.Printf("tracking: %v", err)
		return 0
	}
	if len(rows) > 0 {
		log.Printf("tracking: %d call(s) due now", len(rows))
	}
	// On-chain tracking runs several calls at once (SCOUT_TRACK_WORKERS): the
	// time goes into waiting for the node, and they share one rate limit. The
	// GeckoTerminal source stays one at a time (its API allowance is small).
	workers := 1
	if s.pc.Source == "onchain" {
		workers = s.pc.Workers
	}
	if workers < 1 {
		workers = 1
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				pos := fmt.Sprintf(" [%d/%d]", i+1, len(rows))
				if s.pc.Source == "onchain" {
					s.trackOneOnchain(ctx, &rows[i], pos)
				} else {
					s.trackOne(ctx, &rows[i])
				}
				n.Add(1)
			}
		}()
	}
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	return int(n.Load())
}

// logTrackingStatus prints a one-line summary so it's clear the tracker is alive
// and how much is left.
func (s *scanner) logTrackingStatus(ctx context.Context, processed int) {
	counts, nextDue, err := s.db.TrackingStats(ctx)
	if err != nil {
		log.Printf("tracking: status: %v", err)
		return
	}
	var parts []string
	for _, st := range []string{TrackPending, TrackTracking, TrackDone, TrackNoPool, TrackError, TrackGaveUp, TrackRepeat} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
		}
	}
	summary := "no calls yet"
	if len(parts) > 0 {
		summary = strings.Join(parts, ", ")
	}
	next := "nothing scheduled"
	if nextDue != nil {
		if d := time.Until(*nextDue); d > 0 {
			next = fmt.Sprintf("next check in %s", d.Round(time.Second))
		} else {
			next = "more due now"
		}
	}
	if processed > 0 {
		log.Printf("tracking: processed %d call(s) — %s; %s", processed, summary, next)
	} else {
		log.Printf("tracking: idle — %s; %s", summary, next)
	}
}

// tokenNameBatch is how many distinct tokens one fill pass looks up.
const tokenNameBatch = 200

// fillTokenNames stores the name and symbol that token contracts report
// (name() / symbol()) for calls that have none yet, for display on the website.
// It touches only those two columns: tracking status, schedule and the on-chain
// state are left alone, so no call is tracked again because of it. A token
// without a name gets "" and is not asked again. Returns the tokens filled.
func (s *scanner) fillTokenNames(ctx context.Context) int {
	if s.db == nil || !s.pc.Enabled || s.pc.Source != "onchain" || s.onchain == nil || ctx.Err() != nil {
		return 0
	}
	// Repeat calls of a token already looked up: copied in the database, no node request.
	copied, err := s.db.CopyKnownTokenNames(ctx)
	if err != nil {
		log.Printf("token names: %v", err)
		return 0
	}
	cas, err := s.db.ContractsMissingTokenName(ctx, tokenNameBatch)
	if err != nil {
		log.Printf("token names: %v", err)
		return 0
	}
	tokens, named, rows := 0, 0, copied
	for _, ca := range cas {
		if ctx.Err() != nil {
			break
		}
		var l tokenLabel
		if evmAddrRe.MatchString(ca) { // anything else cannot have an on-chain name here
			var ok bool
			if l, ok = s.onchain.rpc.tokenLabel(ctx, ca); !ok {
				if ctx.Err() == nil {
					log.Printf("token names: the node did not answer for %s — the rest is tried again on the next pass", ca)
				}
				break
			}
		}
		n, err := s.db.SetTokenName(ctx, ca, l.Name, l.Symbol)
		if err != nil {
			log.Printf("token names: save %s: %v", ca, err)
			break
		}
		tokens++
		rows += n
		if l.Name != "" {
			named++
		}
	}
	if tokens > 0 || copied > 0 {
		log.Printf("token names: looked up %d token(s) (%d with a name, %d without), %d call(s) updated", tokens, named, tokens-named, rows)
	}
	return tokens
}

// evmAddrRe matches exactly one EVM address.
var evmAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

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
	prev := *t
	interrupted := false
	fail := func(status string, err error, retryIn time.Duration) {
		if ctx.Err() != nil { // Ctrl+C / shutdown: not a failure of this call
			interrupted = true
			return
		}
		msg := err.Error()
		t.Error = &msg
		t.Status = status
		// Give up only when waiting can't help (no pool / no trades), never on transient errors.
		if now.After(deadline) && (status == TrackNoPool || strings.Contains(msg, "no trades")) {
			t.Status = TrackGaveUp
		}
		t.NextCheckAt = now.Add(retryIn)
		log.Printf("tracking call %d (%s): %s: %v", t.CallID, t.ContractAddress, t.Status, err)
	}
	defer func() {
		if interrupted {
			*t = prev
			return
		}
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
