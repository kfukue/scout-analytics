package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass (on-chain source): keeps, for the first call of every
// tracked token, the return as of the most recent pool price — also after the
// last horizon, where the horizon numbers stop moving.
//
// A pool's price only changes with a trade, so the latest price is the price of
// the last price event at or before the newest block. Each refresh reads the
// events from where the previous one stopped (or from where the horizon scan
// stands, whichever is further) up to the newest block. A row whose horizon
// check is due is left to the horizon scan first (latestLeftToHorizon).
//
// The pass has its own cursor in the persisted on-chain state (latest_block,
// latest_price_q, latest_trade_block) and writes only that and the four
// latest_* columns. The horizon scan (scan_block, last_price_q, the running
// extremes, done), status, next_check_at and attempts are never touched.
// ---------------------------------------------------------------------------

const (
	// latestRecentAge splits the two refresh intervals: calls younger than this
	// are refreshed every LatestRecent, older ones every LatestOld.
	latestRecentAge      = 30 * 24 * time.Hour
	defaultLatestRecent  = 15 * time.Minute
	defaultLatestOld     = 24 * time.Hour
	minLatestRecent      = time.Minute
	minLatestOld         = 10 * time.Minute
	defaultLatestBatch   = 200
	maxLatestBatch       = 10000
	latestProgressEvery  = 30 * time.Second // a pass still running says so this often
	latestSaveTimeout    = 10 * time.Second
	latestMaxErrTextLen  = 300
	latestMinPassBudget  = time.Minute
	latestStateKeyBlock  = "latest_block"
	latestStateKeyPrice  = "latest_price_q"
	latestStateKeyTrades = "latest_trade_block"
	latestStateKeyRug    = "rug_block"   // onchainState.RugBlock, when the pass finds the rug
	latestStateKeyRugLiq = "rug_liq_usd" // onchainState.RugLiquidityUSD
)

// A Pons V2 graduation the pass finds is stored as well (onchainState fields),
// so the horizon scan and later passes follow the token to its v4 pool.
const (
	latestStateKeyPonsDone = "pons_curve_done"
	latestStateKeyPonsGrad = "pons_grad_block"
	latestStateKeyPonsSeen = "pons_grad_seen"
	latestStateKeyPoolID   = "pool_id"
	latestStateKeyTokenIs0 = "token_is_0"
	latestStateKeyPonsHook = "pons_hook"
)

// ponsPatch: the Pons graduation fields that changed from before to after,
// as state keys (nil = none).
func ponsPatch(before, after *onchainState) map[string]any {
	if after.Kind != "pons" {
		return nil
	}
	p := map[string]any{}
	if after.PonsDone != before.PonsDone {
		p[latestStateKeyPonsDone] = after.PonsDone
	}
	if after.PonsSeen != before.PonsSeen {
		p[latestStateKeyPonsSeen] = after.PonsSeen
	}
	if after.PoolID != before.PoolID {
		p[latestStateKeyPonsGrad] = after.PonsGrad
		p[latestStateKeyPoolID] = after.PoolID
		p[latestStateKeyTokenIs0] = after.TokenIs0
		p[latestStateKeyPonsHook] = after.Hook
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

// latestRetryAfter: a row whose refresh failed is left alone for this long
// (remembered in memory only), so rows that keep failing cannot use up every
// pass while others wait.
var latestRetryAfter = 30 * time.Minute

// loadLatestConfig reads the settings of the latest-price pass.
func (pc *priceConfig) loadLatestConfig() error {
	pc.LatestOn, pc.LatestRecent, pc.LatestOld, pc.LatestBatch = true, defaultLatestRecent, defaultLatestOld, defaultLatestBatch
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("SCOUT_LATEST_REFRESH"))); v {
	case "", "on", "1", "true", "yes", "y":
	case "off", "0", "false", "no", "n":
		pc.LatestOn = false
	default:
		return fmt.Errorf("SCOUT_LATEST_REFRESH=%q: use on or off", v)
	}
	dur := func(key string, dst *time.Duration, min time.Duration) error {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s=%q is not a duration (for example 15m or 24h)", key, v)
		}
		if d < min {
			return fmt.Errorf("%s=%s is below the minimum of %s", key, v, min)
		}
		*dst = d
		return nil
	}
	if err := dur("SCOUT_LATEST_REFRESH_RECENT", &pc.LatestRecent, minLatestRecent); err != nil {
		return err
	}
	if err := dur("SCOUT_LATEST_REFRESH_OLD", &pc.LatestOld, minLatestOld); err != nil {
		return err
	}
	if v := env("SCOUT_LATEST_BATCH", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLatestBatch {
			return fmt.Errorf("SCOUT_LATEST_BATCH=%q: want a number from 1 to %d", v, maxLatestBatch)
		}
		pc.LatestBatch = n
	}
	return nil
}

// latestStats is the outcome of one pass.
type latestStats struct {
	Due       int // rows read for this pass (without the deferred ones)
	Deferred  int // rows left to their due horizon check (see latestLeftToHorizon)
	Refreshed int // rows that got a latest price
	Changed   int // of those, rows with a trade since the price before
	Failed    int
	FirstErr  string
	Requests  int64 // node requests of the whole pass
	Waiting   int   // rows still due after the pass (−1 = not counted)
}

// latestQuotes gives the USD price of a quote asset for one pass: the cached
// price at the start of the current hour, looked up once however many rows and
// workers ask for it.
type latestQuotes struct {
	o    *onchainSource
	hour int64
	mu   sync.Mutex
	m    map[string]*latestQuote
}

type latestQuote struct {
	once sync.Once
	q    float64
	err  error
}

func (lq *latestQuotes) usd(ctx context.Context, quote string) (float64, error) {
	key := strings.ToLower(quote)
	lq.mu.Lock()
	e := lq.m[key]
	if e == nil {
		e = &latestQuote{}
		lq.m[key] = e
	}
	lq.mu.Unlock()
	e.once.Do(func() { e.q, e.err = lq.o.quoteUSDHour(ctx, quote, lq.hour) })
	return e.q, e.err
}

// latestSkip returns the call ids that failed less than latestRetryAfter ago
// (and forgets the older ones).
func (s *scanner) latestSkip(now time.Time) []int {
	s.latestMu.Lock()
	defer s.latestMu.Unlock()
	var ids []int
	for id, until := range s.latestRetry {
		if now.Before(until) {
			ids = append(ids, id)
		} else {
			delete(s.latestRetry, id)
		}
	}
	return ids
}

func (s *scanner) latestFailed(id int, now time.Time) {
	s.latestMu.Lock()
	defer s.latestMu.Unlock()
	if s.latestRetry == nil {
		s.latestRetry = map[int]time.Time{}
	}
	s.latestRetry[id] = now.Add(latestRetryAfter)
}

// refreshLatest runs one latest-price pass on its own (see trackRun.latest)
// and returns its outcome once every row of it is finished.
func (s *scanner) refreshLatest(ctx context.Context, recentOnly bool) latestStats {
	r := s.newTrackRun()
	res := r.latest(ctx, recentOnly)
	r.close()
	return <-res
}

// latestLeftToHorizon: the row's horizon check is due, so the horizon scan
// is about to read the blocks the pass would read now (from its scan block on).
// The pass leaves the row alone until that check is done and then reads only
// what lies beyond the new scan block. Rows on schedule are read as usual: the
// horizon scan reads their blocks again later, when the next horizon is due
// (it must see every event for its candles and running extremes).
func latestLeftToHorizon(t *ScoutCallTracking, now time.Time) bool {
	return t.Status == TrackTracking && !t.NextCheckAt.After(now)
}

// latest runs one latest-price pass: up to LatestBatch due rows, calls younger
// than 30 days first, then the stalest, handed to the run's workers as they
// become free. recentOnly leaves the older calls for a later cycle (used while
// horizon work is still waiting). Rows being worked on by the run, and rows
// whose horizon check is due (latestLeftToHorizon), are left out. It returns
// once the rows are handed out; the outcome arrives on the channel, and is
// logged in one summary line, when the last of them is finished.
func (r *trackRun) latest(ctx context.Context, recentOnly bool) <-chan latestStats {
	s := r.s
	out := make(chan latestStats, 1)
	st := latestStats{Waiting: -1}
	if s.db == nil || !s.pc.Enabled || !s.pc.LatestOn || s.pc.Source != "onchain" || s.onchain == nil || ctx.Err() != nil {
		out <- st
		return out
	}
	start := time.Now()
	limit := s.pc.LatestBatch
	if limit < 1 {
		limit = defaultLatestBatch
	}
	rows, _, err := r.fetch(false, func(claimed []int) ([]ScoutCallTracking, error) {
		skip := append(append(s.latestSkip(start), r.deferredIDs(start)...), claimed...)
		return s.db.DueLatest(ctx, start, s.pc.LatestRecent, s.pc.LatestOld, latestRecentAge, recentOnly, skip, limit)
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("latest prices: %v", err)
		}
		out <- st
		return out
	}
	var deferred []int
	var left []ScoutCallTracking
	keep := rows[:0]
	for _, t := range rows {
		if latestLeftToHorizon(&t, start) {
			deferred = append(deferred, t.CallID)
			left = append(left, t)
			continue
		}
		keep = append(keep, t)
	}
	rows = keep
	r.unclaim(left)
	r.deferLatest(deferred, start)
	st.Deferred = len(deferred)
	st.Due = len(rows)
	if len(rows) == 0 {
		out <- st
		return out
	}
	ctx, reqs := withReqCounter(ctx)
	o := s.onchain
	// The newest block is read once: every row of the pass is priced as of it.
	head, err := o.rpc.blockNumber(ctx)
	if err != nil {
		r.unclaim(rows)
		if ctx.Err() == nil {
			log.Printf("latest prices: rpc: %v — %s call(s) wait for the next pass", err, commas(len(rows)))
		}
		out <- st
		return out
	}
	readAt := time.Now().UTC()
	quotes := &latestQuotes{o: o, hour: readAt.Unix() - readAt.Unix()%3600, m: map[string]*latestQuote{}}

	// Horizon tracking comes first: a pass that runs long (the first one reads
	// weeks of blocks per token) hands out no new rows once a tracker interval
	// has gone by. The rest stays due and is picked up after the next horizon check.
	budget := s.pc.Interval
	if budget < latestMinPassBudget {
		budget = latestMinPassBudget
	}
	var refreshed, changed, failed, done atomic.Int64
	var errMu sync.Mutex
	var pass sync.WaitGroup // the rows of this pass
	stopBeat := make(chan struct{})
	r.aux.Add(1)
	go func() {
		defer r.aux.Done()
		t := time.NewTicker(latestProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("latest prices: %s of %s call(s) so far, %s elapsed, %s RPC requests",
					commas(int(done.Load())), commas(len(rows)), time.Since(start).Round(time.Second), commas(int(reqs.Load())))
			}
		}
	}()
	handed := 0
	for handed < len(rows) {
		if time.Since(start) > budget || !r.acquire(ctx) {
			break
		}
		if time.Since(start) > budget { // the wait for a worker used up the budget
			r.free <- struct{}{}
			break
		}
		t := &rows[handed]
		handed++
		pass.Add(1)
		r.jobs <- trackJob{id: t.CallID, run: func() {
			defer pass.Done()
			ch, err := s.latestOne(ctx, t, head, readAt, quotes)
			done.Add(1)
			switch {
			case err == nil:
				refreshed.Add(1)
				if ch {
					changed.Add(1)
				}
			case ctx.Err() != nil:
				// Ctrl+C / shutdown: the row is as it was; not a failure
			default:
				failed.Add(1)
				s.latestFailed(t.CallID, time.Now())
				errMu.Lock()
				if st.FirstErr == "" {
					st.FirstErr = fmt.Sprintf("call %d: %v", t.CallID, err)
				}
				errMu.Unlock()
			}
		}}
	}
	r.unclaim(rows[handed:]) // not handed out: still due, for a later pass

	// The summary, once the last row handed out is finished.
	r.aux.Add(1)
	go func() {
		defer r.aux.Done()
		pass.Wait()
		close(stopBeat)
		out <- s.latestSummary(ctx, st, start, int(refreshed.Load()), int(changed.Load()), int(failed.Load()), reqs.Load())
	}()
	return out
}

// latestSummary completes the outcome of a finished pass and logs it.
func (s *scanner) latestSummary(ctx context.Context, st latestStats, start time.Time, refreshed, changed, failed int, reqs int64) latestStats {
	st.Refreshed, st.Changed, st.Failed, st.Requests = refreshed, changed, failed, reqs
	if ctx.Err() != nil {
		if st.Refreshed > 0 {
			log.Printf("latest prices: interrupted after %s refreshed — the rest is picked up on the next run", commas(st.Refreshed))
		}
		return st
	}
	waiting := "?"
	if n, err := s.db.CountDueLatest(ctx, time.Now(), s.pc.LatestRecent, s.pc.LatestOld, latestRecentAge); err == nil {
		st.Waiting = n
		waiting = commas(n)
	}
	line := fmt.Sprintf("latest prices: %s refreshed (%s changed) in %s, %s RPC requests, %s waiting",
		commas(st.Refreshed), commas(st.Changed), time.Since(start).Round(100*time.Millisecond), commas(int(st.Requests)), waiting)
	if st.Deferred > 0 {
		line += fmt.Sprintf(" (%s left to their due horizon check)", commas(st.Deferred))
	}
	if st.Failed > 0 {
		text := st.FirstErr
		if len(text) > latestMaxErrTextLen {
			text = text[:latestMaxErrTextLen] + "…"
		}
		line += fmt.Sprintf("; %s failed, tried again after %s (first: %s)", commas(st.Failed), latestRetryAfter, text)
	}
	log.Print(line)
	return st
}

// latestOne refreshes the latest price of one row as of block head (read at
// readAt). changed reports a trade since the price stored before. On an error,
// and when ctx is cancelled, nothing is written: the row stays as it was.
func (s *scanner) latestOne(ctx context.Context, t *ScoutCallTracking, head uint64, readAt time.Time, quotes *latestQuotes) (changed bool, err error) {
	o := s.onchain
	var st onchainState
	if len(t.Onchain) == 0 || json.Unmarshal(t.Onchain, &st) != nil || st.V < onchainStateVersion ||
		st.Pool == "" || st.EntryPriceQ <= 0 || t.EntryPriceUSD == nil {
		return false, errors.New("no usable on-chain state (pool and entry price)")
	}
	if st.RugBlock > 0 {
		// Rugged: -100% for good, nothing to read from the node. The row is
		// still saved (latest_checked_at moves on), so it is due again only
		// after a day (see latestDueSQL), not on every pass.
		return false, s.saveLatestRugged(ctx, t, readAt, nil, nil)
	}
	if st.LastPriceQ <= 0 {
		return false, errors.New("no usable on-chain state (pool and entry price)")
	}
	usd := t.PriceUnit != nil && *t.PriceUnit == "usd"
	// The quote's USD price of the current hour (one lookup per hour for all
	// rows): converts the price, and values the pool's quote side for the rug
	// check. Without one, only the certain signals count.
	qUSD := 0.0
	if usd {
		q, err := quotes.usd(ctx, st.Quote)
		if err != nil {
			return false, fmt.Errorf("USD price of %s: %w", st.QuoteSym, err)
		}
		qUSD = q
	}
	// Where to start: after the last block either scan has covered. The price
	// known there is the pass's own, unless the horizon scan has moved past it
	// (or the pass never ran): then it is the horizon scan's last price.
	from, priceQ, tradeBlock := st.LatestBlock, st.LatestPriceQ, st.LatestTradeBlock
	if st.LatestBlock == 0 || st.ScanBlock > st.LatestBlock || priceQ <= 0 {
		from, priceQ, tradeBlock = st.ScanBlock, st.LastPriceQ, st.LastPriceBlock
	}
	to := from
	events := false
	var rugBlock uint64
	var rugLiq *float64
	loaded := st // to see what a Pons graduation found changed
	if head > from {
		if err := o.scanLogs(ctx, &st, from+1, head, func(l rpcLog) {
			if rugBlock > 0 {
				return // drained: nothing after the rug counts
			}
			ev := st.eventOfLog(l)
			if rug, liq := ev.rug(qUSD, o.cfg.RugLiqUSD); rug {
				rugBlock, rugLiq = l.block(), liq
				return
			}
			if p := ev.price; p > 0 && !st.implausible(p, l.block()) {
				priceQ, tradeBlock, events = p, l.block(), true
			}
		}); err != nil {
			return false, fmt.Errorf("swaps to block %d: %w", head, err)
		}
		to = head
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if rugBlock > 0 {
		log.Printf("call %d: pool quote side under $%.0f at block %d — rugged, latest return -100%%", t.CallID, o.cfg.RugLiqUSD, rugBlock)
		patch := map[string]any{latestStateKeyBlock: to, latestStateKeyPrice: priceQ, latestStateKeyTrades: tradeBlock,
			latestStateKeyRug: rugBlock}
		for k, v := range ponsPatch(&loaded, &st) {
			patch[k] = v
		}
		var depth *float64 // current_liquidity_usd: the pool's depth, 2 × the quote side
		if rugLiq != nil {
			patch[latestStateKeyRugLiq] = *rugLiq // the state keeps the quote side
			d := poolDepthUSD(*rugLiq)
			depth = &d
		}
		if err := s.saveLatestRugged(ctx, t, readAt, patch, depth); err != nil {
			return true, err
		}
		// timing.go: the rug's exact time (one node request), only once the
		// rug is saved, so a slow or failed lookup never holds it up; without
		// it -backfill-timing can look the time up later.
		rugTS, err := o.rpc.blockTime(ctx, rugBlock)
		if err != nil {
			log.Printf("call %d: rug time not stored: time of block %d: %v", t.CallID, rugBlock, err)
			return true, nil
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), latestSaveTimeout)
		defer cancel()
		if err := s.db.SetRugAt(sctx, t.CallID, time.Unix(rugTS, 0).UTC(), rugAtEvent); err != nil {
			log.Printf("call %d: rug time not stored: %v", t.CallID, err)
		}
		return true, nil
	}
	// Quote units → the call's price unit.
	price := priceQ
	if usd {
		price = priceQ * qUSD
	}
	entry := t.EntryLatePriceUSD
	if entry == nil {
		entry = t.EntryPriceUSD
	}
	var ret *float64
	if entry != nil && *entry > 0 {
		r := (price / *entry - 1) * 100
		ret = &r
	}
	// The time of the trade behind the price: read only when that trade changed.
	var tradeAt *time.Time
	if tradeBlock > 0 && (tradeBlock != st.LatestTradeBlock || st.LatestBlock == 0) {
		ts, err := o.rpc.blockTime(ctx, tradeBlock)
		if err != nil {
			return false, fmt.Errorf("time of block %d: %w", tradeBlock, err)
		}
		at := time.Unix(ts, 0).UTC()
		tradeAt = &at
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	changed = events
	if st.LatestBlock != 0 {
		changed = tradeBlock != st.LatestTradeBlock
	}
	fields := map[string]any{latestStateKeyBlock: to, latestStateKeyPrice: priceQ, latestStateKeyTrades: tradeBlock}
	for k, v := range ponsPatch(&loaded, &st) {
		fields[k] = v
	}
	patch, err := json.Marshal(fields)
	if err != nil {
		return false, err
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), latestSaveTimeout)
	defer cancel()
	if err := s.db.SaveLatestPrice(sctx, ScoutLatestPrice{CallID: t.CallID, Price: price, ReturnPct: ret,
		CheckedAt: readAt, TradeAt: tradeAt, State: patch}); err != nil {
		return false, fmt.Errorf("save: %w", err)
	}
	return changed, nil
}

// saveLatestRugged stores the latest price of a rugged call: price 0 (the
// token can no longer be sold), return -100%, and the rugged flag. patch
// (optional) is merged into the on-chain state; liq, when known, becomes
// current_liquidity_usd (the pool's depth, poolDepthUSD of the quote side).
// Like latestOne, nothing is written once ctx is cancelled.
func (s *scanner) saveLatestRugged(ctx context.Context, t *ScoutCallTracking, readAt time.Time, patch map[string]any, liq *float64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if patch == nil {
		patch = map[string]any{}
	}
	state, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	ret := -100.0
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), latestSaveTimeout)
	defer cancel()
	if err := s.db.SaveLatestPrice(sctx, ScoutLatestPrice{CallID: t.CallID, Price: 0, ReturnPct: &ret,
		CheckedAt: readAt, State: state, Rugged: true, LiquidityUSD: liq}); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}
