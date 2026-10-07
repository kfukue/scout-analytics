package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"sync/atomic"
	"time"
)

// trackOneOnchain updates one call from on-chain data. Work is incremental:
// swaps are scanned once, from the entry block up to each horizon's block, and
// the running last/max/min price is kept in scout_call_tracking.onchain.
func (s *scanner) trackOneOnchain(ctx context.Context, t *ScoutCallTracking, pos string) {
	now := time.Now().UTC()
	prev := *t // restored if the run is interrupted
	t.Attempts++
	t.LastCheckedAt = &now
	t.Error = nil
	deadline := t.EntryAt.Add(s.pc.maxHorizon() + 48*time.Hour)
	o := s.onchain
	tag := fmt.Sprintf("call %d%s", t.CallID, pos)
	ctx, reqs := withReqCounter(ctx) // this call's own requests (other workers share the client)
	ctx, _ = withInflightSlot(ctx)
	ctx = withScanProgress(ctx, tag, 5*time.Second)
	log.Printf("%s: %s, posted %s (%s ago), status %s",
		tag, t.ContractAddress, t.EntryAt.UTC().Format("2006-01-02 15:04"), time.Since(t.EntryAt).Round(time.Minute), t.Status)
	interrupted := false
	stopBeat := s.heartbeat(ctx, tag, now, reqs)
	defer stopBeat()

	var st *onchainState
	if len(t.Onchain) > 0 && string(t.Onchain) != "null" {
		st = &onchainState{}
		if err := json.Unmarshal(t.Onchain, st); err != nil || st.V < onchainStateVersion {
			st = nil // unreadable, or tracked by an older version: start this call again
		}
	}
	fail := func(status string, err error, retryIn time.Duration) {
		if ctx.Err() != nil { // Ctrl+C / shutdown: not a failure of this call
			interrupted = true
			log.Printf("%s: interrupted — progress kept, it resumes on the next run", tag)
			return
		}
		msg := err.Error()
		t.Error = &msg
		t.Status = status
		// Give up only when waiting can't help: no pool / no trades, well after the last horizon.
		// RPC or database errors are always retried.
		if now.After(deadline) && (status == TrackNoPool || errors.Is(err, errNoTrades)) {
			t.Status = TrackGaveUp
		}
		t.NextCheckAt = now.Add(retryIn)
		log.Printf("%s: %s: %v (retry in %s)", tag, t.Status, err, retryIn.Round(time.Minute))
	}
	defer func() {
		if interrupted {
			// keep what was already scanned, but leave status / attempts / schedule as they were
			t.Status, t.Attempts, t.NextCheckAt, t.LastCheckedAt, t.Error = prev.Status, prev.Attempts, prev.NextCheckAt, prev.LastCheckedAt, prev.Error
		} else if t.Error == nil {
			next := "all horizons done"
			if t.Status == TrackTracking {
				next = "next check " + t.NextCheckAt.Local().Format("2006-01-02 15:04")
			}
			log.Printf("%s: %s in %s, %d RPC requests — %s", tag, t.Status, time.Since(now).Round(100*time.Millisecond),
				reqs.Load(), next)
		}
		if st != nil {
			t.Onchain, _ = json.Marshal(st)
		}
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.db.SaveTracking(sctx, t); err != nil {
			log.Printf("tracking call %d: save: %v", t.CallID, err)
		}
	}()

	latest, err := o.rpc.blockNumber(ctx)
	if err != nil {
		fail(TrackError, fmt.Errorf("rpc: %w", err), backoff(t.Attempts))
		return
	}

	// 1. Pool.
	if st == nil {
		entryBlock, err := o.rpc.blockAt(ctx, t.EntryAt.Unix())
		if err != nil {
			fail(TrackError, fmt.Errorf("block at call time: %w", err), backoff(t.Attempts))
			return
		}
		found, err := o.discover(ctx, t.ContractAddress, entryBlock)
		if errors.Is(err, errNoPool) {
			fail(TrackNoPool, fmt.Errorf("no Uniswap v2/v3/v4 pool found for %s near block %d", t.ContractAddress, entryBlock), 6*time.Hour)
			return
		}
		if err != nil {
			fail(TrackError, fmt.Errorf("pool discovery: %w", err), backoff(t.Attempts))
			return
		}
		st = found
		st.V = onchainStateVersion
		st.Done = map[string]bool{}
		if st.LateBlock, err = o.rpc.blockAt(ctx, t.EntryAt.Add(o.cfg.EntryDelay).Unix()); err != nil {
			st = nil
			fail(TrackError, fmt.Errorf("block at call time + %s: %w", o.cfg.EntryDelay, err), backoff(t.Attempts))
			return
		}
		if err := s.db.DeleteCandles(ctx, t.CallID); err != nil {
			st = nil
			fail(TrackError, fmt.Errorf("clear candles: %w", err), backoff(t.Attempts))
			return
		}
		t.EntryLatePriceUSD = nil
		// Fresh on-chain state: drop any entry price left by another source
		// (e.g. GeckoTerminal) so every number for this call comes from one source.
		t.EntryPriceUSD, t.EntryPriceSource, t.PriceUnit = nil, nil, nil
		t.CurrentPriceUSD, t.CurrentLiquidityUSD, t.Rugged, t.PoolCreatedAt = nil, nil, nil, nil
		pool, name, dex := st.Pool, "token / "+st.QuoteSym, "uniswap-"+st.Kind
		switch st.Kind {
		case "v4":
			pool = st.PoolID
		case "pons":
			dex = "pons-curve" // the bonding curve; the state follows the token to its v4 pool after the graduation
		}
		t.PoolAddress, t.PoolName, t.PoolDex = &pool, &name, &dex
		log.Printf("%s: pool found: %s %s, paired with %s (entry block %d)", tag, dex, pool, st.QuoteSym, st.EntryBlock)
	}
	if st.Done == nil {
		st.Done = map[string]bool{}
	}

	// 2. Entry price (in quote units, then USD).
	if st.EntryPriceQ == 0 {
		if err := o.entryPrice(ctx, st, latest); err != nil {
			fail(TrackError, err, time.Hour)
			return
		}
		if st.RugBlock > 0 {
			log.Printf("%s: pool drained before the call (block %d) — rugged from the start (-100%%)", tag, st.RugBlock)
		}
	}
	if t.EntryPriceUSD == nil {
		q, ok, err := o.quoteUSD(ctx, st.Quote, st.EntryBlock)
		noUSD := "no USD source for " + st.QuoteSym // why the call is in quote units (ok=false)
		if noPriceYetIsFinal(err, now, deadline) {
			// The quote's own pools still had no trade before the call block,
			// and the call is past its last horizon + 48 h (the gave_up
			// deadline): waiting will not bring a price at the call any more.
			// Treated like a definitive absence: tracked in quote units (one
			// log line: the entry is then set, so this is not asked again).
			noUSD = fmt.Sprintf("no USD price of %s at the call block %d after the last horizon + 48 h: %v — giving up on USD",
				st.QuoteSym, st.EntryBlock, err)
			q, ok, err = 0, false, nil
		}
		if err != nil {
			// Temporary (the node, or no trade yet in the quote's own pools):
			// tried again later, never switched to quote units for it.
			fail(TrackError, fmt.Errorf("USD price of %s: %w", st.QuoteSym, err), usdRetryIn(err, t.Attempts))
			return
		}
		// ok=false: every USD source is definitively absent; the call is
		// tracked in quote units.
		unit, p := "usd", st.EntryPriceQ*q
		st.EntryQuoteUSD = q
		if !ok { // no USD source for this quote asset: keep everything in quote units
			unit, p = st.QuoteSym, st.EntryPriceQ
			st.EntryQuoteUSD = 0
		}
		src := "onchain-" + st.Kind
		t.EntryPriceUSD, t.EntryPriceSource, t.PriceUnit = &p, &src, &unit
		if ok {
			log.Printf("%s: entry price $%.6g (%.6g %s × $%.6g, %s)", tag, p, st.EntryPriceQ, st.QuoteSym, q, o.quoteSource(st.Quote))
		} else {
			log.Printf("%s: entry price %.6g %s (%s, tracked in %s)", tag, p, st.QuoteSym, noUSD, st.QuoteSym)
		}
		// The pool's quote side already under the rug threshold at entry:
		// rugged from the start.
		if st.RugBlock == 0 && st.EntryLiqQ > 0 && st.EntryQuoteUSD > 0 && st.EntryLiqQ*st.EntryQuoteUSD < o.cfg.RugLiqUSD {
			liq := st.EntryLiqQ * st.EntryQuoteUSD
			st.RugBlock, st.RugLiquidityUSD = st.EntryBlock, &liq
			log.Printf("%s: pool quote side ~$%.4g at the call, under $%.0f — rugged from the start (-100%%)", tag, liq, o.cfg.RugLiqUSD)
		}
	}
	inUSD := t.PriceUnit != nil && *t.PriceUnit == "usd"
	entryQ := 1.0 // quote → price unit at the call
	if inUSD && st.EntryQuoteUSD > 0 {
		entryQ = st.EntryQuoteUSD
	}

	// 2b. Trading in the hour before the call (features known at call time).
	if !st.PreDone {
		ps, err := o.precall(ctx, st, t.EntryAt.Unix())
		if err != nil {
			fail(TrackError, fmt.Errorf("pre-call trades: %w", err), backoff(t.Attempts))
			return
		}
		if inUSD {
			ps.scale(entryQ)
		}
		if err := s.db.UpsertPrecall(ctx, t.CallID, ps); err != nil {
			fail(TrackError, fmt.Errorf("save pre-call trades: %w", err), backoff(t.Attempts))
			return
		}
		st.PreDone = true
		log.Printf("%s: before the call: %d swaps in 5m, %d in 15m, %d in 60m (%d buys / %d sells in 60m)",
			tag, ps.Swaps[0], ps.Swaps[1], ps.Swaps[2], ps.Buys[2], ps.Sells[2])
	}

	// 3. Horizons, in order, each scanned once.
	var next time.Time
	var lastQ float64 = 1 // the quote's USD price at the last horizon computed in this run
	var lastQKnown bool   // lastQ was read in this run
	for _, h := range s.pc.Horizons {
		due := t.EntryAt.Add(h.Dur)
		if st.Done[h.Name] {
			continue
		}
		if now.Before(due) {
			if next.IsZero() || due.Before(next) {
				next = due
			}
			continue
		}
		if st.RugBlock > 0 && st.RugBlock <= st.ScanBlock {
			// The pool was drained before this horizon's end (horizons are in
			// order): -100%, nothing to read from the node.
			if !s.saveHorizonOnchain(ctx, t, st, h, due, entryQ, 0, true, fail, tag) {
				return
			}
			continue
		}
		hBlock, err := o.rpc.blockAt(ctx, due.Unix())
		if err != nil {
			fail(TrackError, fmt.Errorf("block at +%s: %w", h.Name, err), backoff(t.Attempts))
			return
		}
		// The segment up to this horizon is scanned in pieces (segmentPieceEnd:
		// about segmentPieceChunks log ranges each, cut at a UTC hour), each
		// worked out on a copy of the state (scan cursor, extremes, rug) that
		// replaces the state only once the piece's candles are stored, and
		// then saved as a checkpoint. A failure in a piece (node, USD price,
		// candle times, database) leaves the state at the last stored piece,
		// so the next run goes on from there: nothing is lost or counted twice
		// (candle upserts add up event counts; pieces end on hour boundaries,
		// so no candle is split between two of them).
		var tFrom int64 // time of the piece's first block (after the first piece: its hour, a guess for timeOfBlock)
		if st.ScanBlock < hBlock {
			if tFrom, err = o.rpc.blockTime(ctx, st.ScanBlock); err != nil {
				fail(TrackError, fmt.Errorf("time of block %d: %w", st.ScanBlock, err), backoff(t.Attempts))
				return
			}
		}
		for st.ScanBlock < hBlock {
			to, toHour, err := s.segmentPieceEnd(ctx, st.ScanBlock, hBlock, tFrom, due.Unix())
			if err != nil {
				fail(TrackError, fmt.Errorf("swaps to +%s: piece end: %w", h.Name, err), backoff(t.Attempts))
				return
			}
			work := st.clone()
			// Scan the swaps of the piece, building candles on the way. Event
			// times come from the block number (see timeOfBlock).
			segFrom := work.ScanBlock
			buf := newCandleBuf(t.EntryAt.Unix())
			var obsErr error
			obs := func(block uint64, p float64) {
				est := tFrom // a guess for timeOfBlock: linear up to the horizon
				if hBlock > segFrom {
					est = tFrom + int64(float64(block-segFrom)/float64(hBlock-segFrom)*float64(due.Unix()-tFrom))
				}
				ts, err := o.timeOfBlock(ctx, block, est)
				if err != nil {
					if obsErr == nil {
						obsErr = err
					}
					return
				}
				buf.add(ts, p, block > work.LateBlock)
			}
			if err := o.scan(ctx, work, to, obs); err != nil {
				fail(TrackError, fmt.Errorf("swaps to +%s: %w", h.Name, err), backoff(t.Attempts))
				return
			}
			if obsErr != nil {
				fail(TrackError, fmt.Errorf("candle times to +%s: %w", h.Name, obsErr), backoff(t.Attempts))
				return
			}
			scale := func(hour int64) (float64, error) {
				if !inUSD {
					return 1, nil
				}
				return o.quoteUSDHour(ctx, work.Quote, hour)
			}
			candles, err := buf.list(scale)
			if err == nil {
				err = buf.foldExtremes(work, scale)
			}
			if err != nil {
				fail(TrackError, fmt.Errorf("candles to +%s: %w", h.Name, err), usdRetryIn(err, t.Attempts))
				return
			}
			if err := s.db.UpsertCandles(ctx, t.CallID, candles); err != nil {
				fail(TrackError, fmt.Errorf("candles to +%s: %w", h.Name, err), backoff(t.Attempts))
				return
			}
			st = work // the piece is stored: it is the state from now on
			if to < hBlock {
				s.saveCheckpoint(t, &prev, st, tag)
				tFrom = toHour
			}
		}
		// Drained at or before this horizon's end (found by this scan, or ahead
		// of it by the latest pass): -100%, no USD rate needed.
		rugged := st.RugBlock > 0 && st.RugBlock <= hBlock
		q := 1.0
		if inUSD && !rugged {
			var ok bool
			if q, ok, err = o.quoteUSD(ctx, st.Quote, hBlock); err != nil || !ok {
				if err == nil {
					err = errNoUSDSource
				}
				fail(TrackError, fmt.Errorf("USD price of %s at +%s: %w", st.QuoteSym, h.Name, err), usdRetryIn(err, t.Attempts))
				return
			}
		}
		if inUSD && !rugged {
			lastQ, lastQKnown = q, true
		}
		if !s.saveHorizonOnchain(ctx, t, st, h, due, entryQ, q, rugged, fail, tag) {
			return
		}
	}

	// A rugged call is flagged at once, without waiting for the last horizon.
	if st.RugBlock > 0 {
		s.flagRugged(t, st)
	}

	// 4. Schedule the next horizon, or finish.
	if !next.IsZero() {
		t.Status = TrackTracking
		t.NextCheckAt = next.Add(2 * time.Minute)
		return
	}
	rug := st.RugBlock > 0 || (t.CurrentPriceUSD != nil && *t.CurrentPriceUSD < *t.EntryPriceUSD*0.05)
	if inUSD && st.RugBlock == 0 && !lastQKnown {
		// Every horizon was computed by an earlier run (interrupted before
		// the row was finished): lastQ is not the quote's USD price yet.
		if q, ok, err := o.quoteUSD(ctx, st.Quote, latest); err == nil && ok {
			lastQ, lastQKnown = q, true
		}
	}
	if inUSD && st.RugBlock == 0 && lastQKnown {
		// The quote side is compared with the threshold, like the price
		// events; the column keeps the pool's depth (2 × the quote side).
		if quoteSide, ok := o.liquidityUSD(ctx, st, lastQ); ok {
			depth := poolDepthUSD(quoteSide)
			t.CurrentLiquidityUSD = &depth
			// An empty quote side always counts, even with the USD check
			// off (SCOUT_RUG_LIQ_USD=0), like a drained price event.
			if quoteSide == 0 || quoteSide < o.cfg.RugLiqUSD {
				// Drained after the last horizon: the horizons stay, the latest
				// return becomes -100% from now on.
				rug = true
				st.RugBlock, st.RugLiquidityUSD = latest, &quoteSide
			}
		}
	}
	t.Rugged = &rug
	t.Status = TrackDone
	t.NextCheckAt = now
}

// segmentPieceChunks: a horizon segment is scanned and stored in pieces of
// about this many log ranges (SCOUT_RPC_LOG_CHUNK blocks each: 32 × 200,000
// blocks ≈ 7.4 days at 10 blocks per second), so a long segment interrupted
// by a failure or a restart resumes from its last stored piece instead of
// from its start. A var so tests can lower it.
var segmentPieceChunks uint64 = 32

// segmentPieceEnd: the last block of the piece of a horizon segment that
// starts after block from (the state's scan cursor, at about time tFrom) and
// ends at hBlock (time tEnd). A piece ends on a UTC hour boundary as
// timeOfBlock places blocks (the block before hourBlock(H): hour H starts at
// hourBlock(H)), so no candle (hourly or 5-minute) is split between two
// pieces; toHour is that H. The last piece ends at hBlock (toHour 0). The
// hour is estimated from the times given (no request); its first block is a
// cached hour-block lookup, shared with the candle times.
func (s *scanner) segmentPieceEnd(ctx context.Context, from, hBlock uint64, tFrom, tEnd int64) (to uint64, toHour int64, err error) {
	o := s.onchain
	span := segmentPieceChunks * o.rpc.maxChunk
	if span == 0 || hBlock <= from || hBlock-from <= span {
		return hBlock, 0, nil
	}
	ts := tFrom + int64(float64(span)/float64(hBlock-from)*float64(tEnd-tFrom))
	// The hour boundary at or before the estimated end; a later one when
	// that would leave the piece empty (an estimate that fell short).
	for hour, i := ts-ts%3600, 0; i < 24*40; hour, i = hour+3600, i+1 {
		b, err := o.hourBlock(ctx, hour)
		if err != nil {
			return 0, 0, err
		}
		if b >= hBlock {
			return hBlock, 0, nil
		}
		if b > from+1 {
			return b - 1, hour, nil
		}
	}
	return hBlock, 0, nil
}

// saveCheckpoint stores the state after a piece of a horizon segment (see
// segmentPieceEnd), so a run killed before it ends keeps the pieces already
// stored. The row's status, attempts, schedule and error are written as they
// were before this run (prev), like an interrupted run leaves them. A failed
// save is only logged: the state is saved again at the end of the run.
func (s *scanner) saveCheckpoint(t *ScoutCallTracking, prev *ScoutCallTracking, st *onchainState, tag string) {
	b, err := json.Marshal(st)
	if err != nil {
		log.Printf("%s: checkpoint: %v", tag, err)
		return
	}
	c := *t
	c.Status, c.Attempts, c.NextCheckAt, c.LastCheckedAt, c.Error = prev.Status, prev.Attempts, prev.NextCheckAt, prev.LastCheckedAt, prev.Error
	c.Onchain = b
	// Its own deadline: a checkpoint is written even while the run is
	// being stopped (the candles of the piece are already stored).
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.db.SaveTracking(sctx, &c); err != nil {
		log.Printf("%s: checkpoint at block %d: save: %v", tag, st.ScanBlock, err)
	}
}

// clone is a deep copy of the state (the horizon step works on one).
func (st *onchainState) clone() *onchainState {
	c := *st
	c.Done = maps.Clone(st.Done)
	if st.RugLiquidityUSD != nil {
		v := *st.RugLiquidityUSD
		c.RugLiquidityUSD = &v
	}
	return &c
}

// flagRugged marks a call whose pool was drained: rugged, and, when the quote
// side at the rug is known, current_liquidity_usd = the pool's depth then
// (poolDepthUSD: 2 × the quote side, the column's meaning).
func (s *scanner) flagRugged(t *ScoutCallTracking, st *onchainState) {
	yes := true
	t.Rugged = &yes
	if st.RugLiquidityUSD != nil {
		depth := poolDepthUSD(*st.RugLiquidityUSD)
		t.CurrentLiquidityUSD = &depth
	}
}

// onchainHorizon computes one horizon's result from the running state. entry is
// the entry price in the price unit, entryQ the quote → price-unit rate at the
// call and q the rate at the horizon's end. rugged: the pool was drained at or
// before the horizon's end; the price is then 0 (the token can no longer be
// sold), the return and the low are -100% and the peak is the one reached
// before the rug (the running extremes never include the rug or anything after
// it). late is the realistic entry price (nil when there is none).
func onchainHorizon(st *onchainState, name string, due time.Time, entry, entryQ, q float64, rugged bool) (r horizonResult, late *float64) {
	price := st.LastPriceQ * q
	// Peak and low since the call, each trade valued at its own hour's rate.
	hiU, loU := math.Max(st.RunMaxU, entry), entry
	if st.RunMinU > 0 && st.RunMinU < loU {
		loU = st.RunMinU
	}
	if rugged {
		price, loU = 0, 0
	}
	r = horizonResult{Horizon: name, DueAt: due, Status: "done", PriceUSD: price, MaxPriceUSD: hiU, MinPriceUSD: loU}
	r.ReturnPct = (r.PriceUSD/entry - 1) * 100
	r.MaxGainPct = (r.MaxPriceUSD/entry - 1) * 100
	r.MaxDDPct = (r.MinPriceUSD/entry - 1) * 100
	// The same, measured from the realistic entry: the pool price EntryDelay
	// after the post, and only what happened after it.
	if st.LatePriceQ > 0 {
		lp := st.LatePriceQ * entryQ
		late = &lp
		hi, lo := math.Max(st.RunMaxLateU, lp), lp
		if st.RunMinLateU > 0 && st.RunMinLateU < lo {
			lo = st.RunMinLateU
		}
		if rugged {
			lo = 0
		}
		rl, gl, dl := (r.PriceUSD/lp-1)*100, (hi/lp-1)*100, (lo/lp-1)*100
		r.ReturnLatePct, r.MaxGainLatePct, r.MaxDDLatePct = &rl, &gl, &dl
	}
	return r, late
}

// saveHorizonOnchain stores one horizon's result (see onchainHorizon) and marks
// it done. On a database error it calls fail and returns false.
func (s *scanner) saveHorizonOnchain(ctx context.Context, t *ScoutCallTracking, st *onchainState, h horizon, due time.Time,
	entryQ, q float64, rugged bool, fail func(string, error, time.Duration), tag string) bool {
	r, late := onchainHorizon(st, h.Name, due, *t.EntryPriceUSD, entryQ, q, rugged)
	if late != nil {
		t.EntryLatePriceUSD = late
	}
	if st.LastPriceBlock > st.EntryBlock {
		if ts, err := s.onchain.rpc.blockTime(ctx, st.LastPriceBlock); err == nil {
			lt := time.Unix(ts, 0).UTC()
			r.LastTradeAt = &lt
		}
	}
	if err := s.db.UpsertReturn(ctx, t.CallID, h, r); err != nil {
		fail(TrackError, fmt.Errorf("save %s: %w", h.Name, err), backoff(t.Attempts))
		return false
	}
	st.Done[h.Name] = true
	cur := r.PriceUSD
	t.CurrentPriceUSD = &cur
	if rugged {
		log.Printf("%s: +%s → rugged at block %d (pool quote side under $%.0f): -100%%, peak before the rug %+.1f%%",
			tag, h.Name, st.RugBlock, s.onchain.cfg.RugLiqUSD, r.MaxGainPct)
	} else {
		log.Printf("%s: +%s → %.6g (%+.1f%%), peak %+.1f%%, low %+.1f%%", tag, h.Name, r.PriceUSD, r.ReturnPct, r.MaxGainPct, r.MaxDDPct)
	}
	return true
}

// heartbeatEvery is how often a call that is still being worked on says so.
var heartbeatEvery = 10 * time.Second

// heartbeat logs, every heartbeatEvery, that the call is still being worked on
// and which node request it is waiting for — so a slow node is visible instead
// of looking like a hang. The returned func stops it.
func (s *scanner) heartbeat(ctx context.Context, tag string, start time.Time, reqs *atomic.Int64) func() {
	o := s.onchain
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				waiting := "between requests"
				if slot := inflightSlotFrom(ctx); slot != nil { // this call's own request
					if f := slot.Load(); f != nil {
						node := "Robinhood node"
						if o.mainnet != nil && f.url == o.mainnet.url {
							node = "Ethereum node"
						}
						waiting = "waiting on " + node + ": " + f.String()
					}
				} else if w := o.rpc.waitingOn(); w != "" {
					waiting = "waiting on Robinhood node: " + w
				}
				log.Printf("%s: still working — %s elapsed, %d RPC requests so far; %s",
					tag, time.Since(start).Round(time.Second), reqs.Load(), waiting)
			}
		}
	}()
	return func() { close(done) }
}

// noPriceYetIsFinal: a "no price yet" for the entry's quote asset (its own
// pools had no trade before the call block) is final once the call is past
// deadline (its last horizon + 48 h, the same deadline as gave_up); before
// that it is retried. Any other error (the node, the database) is never final.
func noPriceYetIsFinal(err error, now, deadline time.Time) bool {
	return errors.Is(err, errNoPriceYet) && now.After(deadline)
}

// usdSearchRetry: when a call waits for the whole-history search of its
// quote asset's USD pools (errUSDSearchPending), it is tried again this soon:
// every try moves that search on (bounded per try), so it ends in a price or
// a definitive answer after a few tries instead of waiting out the back-off.
var usdSearchRetry = 5 * time.Minute

// usdRetryIn: when to try a call again after a USD-price failure (err).
func usdRetryIn(err error, attempts int) time.Duration {
	if errors.Is(err, errUSDSearchPending) {
		return usdSearchRetry
	}
	return backoff(attempts)
}
