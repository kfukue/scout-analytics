package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// trackOneOnchain updates one call from on-chain data. Work is incremental:
// swaps are scanned once, from the entry block up to each horizon's block, and
// the running last/max/min price is kept in scout_call_tracking.onchain.
func (s *scanner) trackOneOnchain(ctx context.Context, t *ScoutCallTracking) {
	now := time.Now().UTC()
	prev := *t // restored if the run is interrupted
	t.Attempts++
	t.LastCheckedAt = &now
	t.Error = nil
	deadline := t.EntryAt.Add(s.pc.maxHorizon() + 48*time.Hour)
	o := s.onchain
	tag := fmt.Sprintf("call %d%s", t.CallID, s.trackPos)
	reqStart := o.rpc.requests.Load()
	ctx = withScanProgress(ctx, tag, 5*time.Second)
	log.Printf("%s: %s, posted %s (%s ago), status %s",
		tag, t.ContractAddress, t.EntryAt.UTC().Format("2006-01-02 15:04"), time.Since(t.EntryAt).Round(time.Minute), t.Status)
	interrupted := false

	var st *onchainState
	if len(t.Onchain) > 0 && string(t.Onchain) != "null" {
		st = &onchainState{}
		if err := json.Unmarshal(t.Onchain, st); err != nil {
			st = nil
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
				o.rpc.requests.Load()-reqStart, next)
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
		st.Done = map[string]bool{}
		// Fresh on-chain state: drop any entry price left by another source
		// (e.g. GeckoTerminal) so every number for this call comes from one source.
		t.EntryPriceUSD, t.EntryPriceSource, t.PriceUnit = nil, nil, nil
		t.CurrentPriceUSD, t.CurrentLiquidityUSD, t.Rugged, t.PoolCreatedAt = nil, nil, nil, nil
		pool, name, dex := st.Pool, "token / "+st.QuoteSym, "uniswap-"+st.Kind
		if st.Kind == "v4" {
			pool = st.PoolID
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
	}
	if t.EntryPriceUSD == nil {
		q, ok, err := o.quoteUSD(ctx, st.Quote, st.EntryBlock)
		if err != nil {
			fail(TrackError, fmt.Errorf("USD price of %s: %w", st.QuoteSym, err), backoff(t.Attempts))
			return
		}
		unit, p := "usd", st.EntryPriceQ*q
		if !ok { // no USD source for this quote asset: keep everything in quote units
			unit, p = st.QuoteSym, st.EntryPriceQ
		}
		src := "onchain-" + st.Kind
		t.EntryPriceUSD, t.EntryPriceSource, t.PriceUnit = &p, &src, &unit
		if ok {
			log.Printf("%s: entry price $%.6g (%.6g %s × $%.6g, %s)", tag, p, st.EntryPriceQ, st.QuoteSym, q, o.quoteSource(st.Quote))
		} else {
			log.Printf("%s: entry price %.6g %s (no USD source for %s — tracked in %s)", tag, p, st.QuoteSym, st.QuoteSym, st.QuoteSym)
		}
	}
	inUSD := t.PriceUnit != nil && *t.PriceUnit == "usd"

	// 3. Horizons, in order, each scanned once.
	var next time.Time
	var lastQ float64 = 1
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
		hBlock, err := o.rpc.blockAt(ctx, due.Unix())
		if err != nil {
			fail(TrackError, fmt.Errorf("block at +%s: %w", h.Name, err), backoff(t.Attempts))
			return
		}
		if err := o.scan(ctx, st, hBlock); err != nil {
			fail(TrackError, fmt.Errorf("swaps to +%s: %w", h.Name, err), backoff(t.Attempts))
			return
		}
		q := 1.0
		if inUSD {
			var ok bool
			if q, ok, err = o.quoteUSD(ctx, st.Quote, hBlock); err != nil || !ok {
				if err == nil {
					err = errors.New("no USD source")
				}
				fail(TrackError, fmt.Errorf("USD price of %s at +%s: %w", st.QuoteSym, h.Name, err), backoff(t.Attempts))
				return
			}
		}
		lastQ = q
		entry := *t.EntryPriceUSD
		r := horizonResult{Horizon: h.Name, DueAt: due, Status: "done",
			PriceUSD: st.LastPriceQ * q, MaxPriceUSD: st.RunMaxQ * q, MinPriceUSD: st.RunMinQ * q}
		r.ReturnPct = (r.PriceUSD/entry - 1) * 100
		r.MaxGainPct = (r.MaxPriceUSD/entry - 1) * 100
		r.MaxDDPct = (r.MinPriceUSD/entry - 1) * 100
		if st.LastPriceBlock > st.EntryBlock {
			if ts, err := o.rpc.blockTime(ctx, st.LastPriceBlock); err == nil {
				lt := time.Unix(ts, 0).UTC()
				r.LastTradeAt = &lt
			}
		}
		if err := s.db.UpsertReturn(ctx, t.CallID, h, r); err != nil {
			fail(TrackError, fmt.Errorf("save %s: %w", h.Name, err), backoff(t.Attempts))
			return
		}
		st.Done[h.Name] = true
		cur := r.PriceUSD
		t.CurrentPriceUSD = &cur
		log.Printf("%s: +%s → %.6g (%+.1f%%), peak %+.1f%%, low %+.1f%%", tag, h.Name, r.PriceUSD, r.ReturnPct, r.MaxGainPct, r.MaxDDPct)
	}

	// 4. Schedule the next horizon, or finish.
	if !next.IsZero() {
		t.Status = TrackTracking
		t.NextCheckAt = next.Add(2 * time.Minute)
		return
	}
	rug := t.CurrentPriceUSD != nil && *t.CurrentPriceUSD < *t.EntryPriceUSD*0.05
	if inUSD {
		if liq, ok := o.liquidityUSD(ctx, st, lastQ); ok {
			t.CurrentLiquidityUSD = &liq
			if liq < s.pc.RugLiqUSD {
				rug = true
			}
		}
	}
	t.Rugged = &rug
	t.Status = TrackDone
	t.NextCheckAt = now
}
