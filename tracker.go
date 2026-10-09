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

// trackBatch is how many due calls one cycle of horizon tracking hands out. A
// cycle that hands out a full batch reports that more are waiting.
const trackBatch = 50

// trackChunkMin is the smallest number of due rows read from the database at a
// time; with more workers, one read takes one row per worker (see horizon).
const trackChunkMin = 4

// latestDeferFor: how long the latest-price pass leaves a row alone whose
// horizon check is due (or until that check is done, whichever comes first).
var latestDeferFor = 10 * time.Minute

// trackLoop processes due tracking rows every PriceCfg.Interval.
func (s *scanner) trackLoop(ctx context.Context) {
	if s.db == nil || !s.pc.Enabled {
		return
	}
	if s.pc.Source == "onchain" {
		s.reloadFeeds(ctx) // the asset database's Chainlink feeds, before the first line names them
		log.Printf("performance tracking on: %s via %s; %d call(s) at a time", horizonNames(s.pc.Horizons), s.onchain.describe(), max(s.pc.Workers, 1))
		if s.pc.LatestOn {
			log.Printf("latest prices on: refreshed every %s for calls under 30 days old, every %s for older ones; at most %s call(s) per cycle (SCOUT_LATEST_REFRESH=off turns this off)",
				s.pc.LatestRecent, s.pc.LatestOld, commas(s.pc.LatestBatch))
		} else {
			log.Printf("latest prices off (SCOUT_LATEST_REFRESH=off)")
		}
	} else {
		log.Printf("performance tracking on: %s via %s (network %q, %d req/min)",
			horizonNames(s.pc.Horizons), s.pc.BaseURL, s.pc.Network, s.pc.RPM)
	}
	// One pool of workers for the whole run: a call still running when a cycle
	// ends keeps its worker, and the other workers go on with the next cycle.
	r := s.newTrackRun()
	defer r.close() // after Ctrl+C: waits for the calls in progress to put their rows back
	t := time.NewTicker(s.pc.Interval)
	defer t.Stop()
	for {
		more := r.cycle(ctx, trackBatch)
		if ctx.Err() != nil {
			return
		}
		if more {
			continue // more are waiting: keep going without the pause
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// trackCycle is one cycle of the tracker on its own (see trackRun.cycle), with
// every call it started finished when it returns.
func (s *scanner) trackCycle(ctx context.Context, batch int) (more bool) {
	r := s.newTrackRun()
	defer r.close()
	return r.cycle(ctx, batch)
}

// cycle is one round of the tracker: up to batch horizon checks that are due,
// the status line, token names and supplies, then a latest-price pass. more says the
// horizon batch was full, so more of that work is waiting. It returns once
// everything is handed out: calls still running finish on their workers.
func (r *trackRun) cycle(ctx context.Context, batch int) (more bool) {
	s := r.s
	if s.pc.Source == "onchain" {
		s.reloadFeeds(ctx) // feeds added to (or removed from) the asset database since the last cycle
	}
	if !s.prepareDue(ctx) {
		return false
	}
	_, more = r.horizon(ctx, batch)
	if ctx.Err() != nil {
		return false
	}
	s.logTrackingStatusRunning(ctx, int(r.horizonDone.Swap(0)), r.horizonBusy())
	s.fillTokenNames(ctx)
	s.fillTokenSupply(ctx)
	// Latest prices come after the horizon work of the cycle. While a full
	// horizon batch says more of that is waiting, only calls younger than 30
	// days are refreshed; the older ones wait for a quieter cycle.
	r.latest(ctx, more)
	return more
}

// trackWorkers is how many calls are tracked at the same time. On-chain
// tracking runs several (SCOUT_TRACK_WORKERS): the time goes into waiting for
// the node, and they share one rate limit. The GeckoTerminal source stays one
// at a time (its API allowance is small).
func (s *scanner) trackWorkers() int {
	return s.pc.trackWorkers()
}

// trackWorkers is the tracker's worker count for this price config; see
// scanner.trackWorkers. The database pool is sized from it too.
func (pc priceConfig) trackWorkers() int {
	if pc.Source != "onchain" {
		return 1
	}
	return max(pc.Workers, 1)
}

// trackRun is the tracker's pool of workers, and the bookkeeping that gives
// each call to one worker at a time. One goroutine (the dispatcher: trackLoop,
// or trackDue / refreshLatest when called on their own) reads due rows from the
// database a few at a time and hands each one out as soon as a worker is free,
// so one slow call never holds up the others. Horizon checks and latest-price
// refreshes share the workers; a call is never with two of them at once.
type trackRun struct {
	s       *scanner
	workers int
	jobs    chan trackJob  // buffered: the dispatcher sends only after taking a free token
	free    chan struct{}  // one token per idle worker
	wg      sync.WaitGroup // the workers
	aux     sync.WaitGroup // latest-pass progress lines and summaries

	seq         atomic.Int64 // horizon checks handed out so far (the "[#n]" in the log)
	horizonDone atomic.Int64 // horizon checks finished since the last status line

	// dueHorizon and trackRow are the database read and the work of one horizon
	// check (replaced in tests).
	dueHorizon func(ctx context.Context, n int) ([]ScoutCallTracking, error)
	trackRow   func(ctx context.Context, t *ScoutCallTracking, pos string)

	mu sync.Mutex
	// claimed: call ids handed out, or read and waiting in the dispatcher.
	// The value says which: true = a horizon check.
	claimed  map[int]bool
	fetching bool
	// released: ids let go while a read was running. That read may have seen
	// the row before its worker saved it, so its copy is dropped.
	released map[int]bool
	// deferred: rows the latest-price pass leaves to the horizon scan (their
	// horizon check is due), until the time given or that check is done.
	deferred map[int]time.Time
}

type trackJob struct {
	id      int
	horizon bool
	run     func()
}

// newTrackRun starts the workers. close stops them.
func (s *scanner) newTrackRun() *trackRun {
	n := s.trackWorkers()
	r := &trackRun{s: s, workers: n, jobs: make(chan trackJob, n), free: make(chan struct{}, n),
		claimed: map[int]bool{}, released: map[int]bool{}, deferred: map[int]time.Time{}}
	r.dueHorizon = func(ctx context.Context, n int) ([]ScoutCallTracking, error) {
		return s.db.DueTracking(ctx, time.Now(), n)
	}
	r.trackRow = func(ctx context.Context, t *ScoutCallTracking, pos string) {
		if s.pc.Source == "onchain" {
			s.trackOneOnchain(ctx, t, pos)
		} else {
			s.trackOne(ctx, t)
		}
	}
	for range n {
		r.free <- struct{}{}
		r.wg.Add(1)
		go r.work()
	}
	return r
}

func (r *trackRun) work() {
	defer r.wg.Done()
	for j := range r.jobs {
		j.run()
		r.release(j.id, j.horizon)
		if j.horizon {
			r.horizonDone.Add(1)
		}
		r.free <- struct{}{}
	}
}

// close waits for the calls handed out to finish, then stops the workers. Only
// the dispatcher calls it, once, after its last hand-out.
func (r *trackRun) close() {
	close(r.jobs)
	r.wg.Wait()
	r.aux.Wait()
}

// acquire waits for an idle worker; false once ctx is done.
func (r *trackRun) acquire(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-r.free:
	case <-ctx.Done():
		return false
	}
	if ctx.Err() != nil {
		r.free <- struct{}{}
		return false
	}
	return true
}

// fetch reads due rows with get, which is given the call ids already claimed
// (to leave out, or to read that many more), and claims the rows it returns.
// Rows already claimed are dropped, and so are rows let go while get ran: get
// may have read them before their save. read is how many rows get returned.
func (r *trackRun) fetch(horizon bool, get func(claimed []int) ([]ScoutCallTracking, error)) (rows []ScoutCallTracking, read int, err error) {
	r.mu.Lock()
	claimed := make([]int, 0, len(r.claimed))
	for id := range r.claimed {
		claimed = append(claimed, id)
	}
	r.fetching = true
	r.mu.Unlock()
	all, err := get(claimed)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetching = false
	stale := r.released
	r.released = map[int]bool{}
	if err != nil {
		return nil, 0, err
	}
	for _, t := range all {
		if _, busy := r.claimed[t.CallID]; busy || stale[t.CallID] {
			continue
		}
		r.claimed[t.CallID] = horizon
		rows = append(rows, t)
	}
	return rows, len(all), nil
}

// unclaim lets go of rows that were read but not handed out (nothing was
// written for them).
func (r *trackRun) unclaim(rows []ScoutCallTracking) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range rows {
		delete(r.claimed, t.CallID)
	}
}

// release lets go of a row whose worker is done with it (and has saved it).
func (r *trackRun) release(id int, horizon bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.claimed, id)
	if r.fetching {
		r.released[id] = true
	}
	if horizon {
		delete(r.deferred, id) // its horizon scan has moved on: the latest pass reads from there
	}
}

// horizonBusy is how many horizon checks are with a worker or waiting in the
// dispatcher.
func (r *trackRun) horizonBusy() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, h := range r.claimed {
		if h {
			n++
		}
	}
	return n
}

// deferLatest leaves rows to the horizon scan for a while (see latestDeferFor).
func (r *trackRun) deferLatest(ids []int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.deferred[id] = now.Add(latestDeferFor)
	}
}

// deferredIDs returns the rows still left to the horizon scan (and forgets
// the others).
func (r *trackRun) deferredIDs(now time.Time) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []int
	for id, until := range r.deferred {
		if now.Before(until) {
			ids = append(ids, id)
		} else {
			delete(r.deferred, id)
		}
	}
	return ids
}

// horizon hands out up to limit due horizon checks, in the order of
// DueTracking (live calls first, then the longest due), each as soon as a
// worker is free. It reads the due rows a few at a time (one per worker, at
// least trackChunkMin), so a live call that becomes due waits for at most that
// many hand-outs. handed is how many it handed out; more says due rows are
// left. It returns without waiting for the calls handed out.
func (r *trackRun) horizon(ctx context.Context, limit int) (handed int, more bool) {
	chunk := min(max(r.workers, trackChunkMin), trackBatch)
	var buf []ScoutCallTracking
	defer func() { r.unclaim(buf) }()
	all := false // the last read returned every due row
	first := true
	for handed < limit {
		if !r.acquire(ctx) {
			return handed, false
		}
		if len(buf) == 0 {
			if all {
				r.free <- struct{}{}
				return handed, false
			}
			want := min(chunk, limit-handed)
			asked := 0
			rows, read, err := r.fetch(true, func([]int) ([]ScoutCallTracking, error) {
				// The horizon checks being worked on are still due in the
				// database until they are saved: read that many more. (Rows with
				// the latest-price pass are not due for a horizon check: it
				// leaves those alone, see latestLeftToHorizon.)
				asked = want + r.horizonBusy()
				return r.dueHorizon(ctx, asked)
			})
			if err != nil {
				r.free <- struct{}{}
				if ctx.Err() == nil {
					log.Printf("tracking: %v", err)
				}
				return handed, false
			}
			all = read < asked
			if first && len(rows) > 0 {
				first = false
				plus := "+"
				if all {
					plus = ""
				}
				log.Printf("tracking: %d%s call(s) due now", len(rows), plus)
			}
			buf = rows
			if len(buf) == 0 {
				// Everything read is with a worker already (or was just saved).
				r.free <- struct{}{}
				return handed, !all
			}
		}
		if ctx.Err() != nil {
			r.free <- struct{}{}
			return handed, false
		}
		t := buf[0]
		buf = buf[1:]
		handed++
		n := r.seq.Add(1)
		r.jobs <- trackJob{id: t.CallID, horizon: true, run: func() {
			r.trackRow(ctx, &t, fmt.Sprintf(" [#%d]", n))
		}}
	}
	return handed, len(buf) > 0 || !all
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

// classifyPosts fills post_kind for rows that do not have it yet and logs one
// line when it classified any.
func classifyPosts(ctx context.Context, st *ScoutStore) {
	calls, updates, err := st.ClassifyPostKinds(ctx)
	if calls+updates > 0 {
		log.Printf("posts: %s call(s), %s update(s) classified", commas(calls), commas(updates))
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("posts: classifying old posts: %v", err)
	}
}

// prepareDue gets the rows in shape before due rows are read: posts stored
// before post_kind existed are classified (one small lookup when none is
// left), so update posts are set aside, and only the first call of each token
// stays tracked (MarkRepeatTracking: one statement; writes nothing when all is
// in place). false: ctx is done.
func (s *scanner) prepareDue(ctx context.Context) bool {
	classifyPosts(ctx, s.db)
	if n, err := s.db.MarkRepeatTracking(ctx); err != nil {
		if ctx.Err() != nil {
			return false
		}
		log.Printf("tracking: repeat calls: %v", err)
	} else if n > 0 {
		log.Printf("tracking: %s repeat call(s) skipped — only the first call of each token is tracked", commas(n))
	}
	return ctx.Err() == nil
}

// trackDue processes up to limit due calls and returns how many it processed,
// once all of them are finished. Only the first call of each token is tracked
// (see MarkRepeatTracking).
func (s *scanner) trackDue(ctx context.Context, limit int) int {
	if !s.prepareDue(ctx) {
		return 0
	}
	r := s.newTrackRun()
	n, _ := r.horizon(ctx, limit)
	r.close()
	return n
}

// logTrackingStatus prints a one-line summary so it's clear the tracker is alive
// and how much is left.
func (s *scanner) logTrackingStatus(ctx context.Context, processed int) {
	s.logTrackingStatusRunning(ctx, processed, 0)
}

// logTrackingStatusRunning is logTrackingStatus with the number of calls still
// being worked on (processed counts the finished ones).
func (s *scanner) logTrackingStatusRunning(ctx context.Context, processed, running int) {
	counts, nextDue, err := s.db.TrackingStats(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("tracking: status: %v", err)
		}
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
	switch {
	case running > 0:
		log.Printf("tracking: processed %d call(s), %d in progress — %s; %s", processed, running, summary, next)
	case processed > 0:
		log.Printf("tracking: processed %d call(s) — %s; %s", processed, summary, next)
	default:
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

// tokenSupplyBatch is how many distinct tokens one supply fill pass looks up.
const tokenSupplyBatch = 200

// tokenSupplyMaxStrikes: a token whose supply lookup gets a JSON-RPC error
// that looks like a busy node (e.g. a revert text with "limit" or "rate" in
// it) in this many passes in which the node otherwise answered (a later token,
// or a liveness request at the end of the pass, also when two failures in a
// row ended it), with no success in between, is given up: NULL supply at
// block 0, not asked again. Counted in memory; tokens with strikes are tried
// last in the next pass, so they cannot hold up the others.
const tokenSupplyMaxStrikes = 3

// fillTokenSupply stores the token supply (totalSupply() / 10^decimals(), in
// whole tokens) of tokens whose calls have an entry price but no supply lookup
// yet, for the Analytics page's market cap at the call (price at the post ×
// supply). Read at the first call's entry block while the node has that
// state, else at the latest block (tokenSupplyAt); once a token's entry block
// has no state, older entry blocks are not tried again in the same pass, and
// the head block is read at most once per pass. Repeat calls of a token
// already looked up are copied in the database first. It touches only
// token_supply and token_supply_block: tracking status, schedule and the
// on-chain state are left alone, so no call is tracked again because of it,
// and it never changes how prices are read (o.noState). A token without a
// usable supply gets NULL with the block set and is not asked again. A token
// the node does not answer for is skipped; two in a row end the pass (the
// node is busy) and the rest waits for the next pass, where tokens with
// strikes (tokenSupplyMaxStrikes) come last. Returns the tokens looked up.
func (s *scanner) fillTokenSupply(ctx context.Context) int {
	if s.db == nil || !s.pc.Enabled || s.pc.Source != "onchain" || s.onchain == nil || ctx.Err() != nil {
		return 0
	}
	copied, err := s.db.CopyKnownTokenSupply(ctx)
	if err != nil {
		log.Printf("token supply: %v", err)
		return 0
	}
	targets, err := s.db.ContractsMissingTokenSupply(ctx, tokenSupplyBatch)
	if err != nil {
		log.Printf("token supply: %v", err)
		return 0
	}
	targets = s.struckLast(targets)
	var head uint64 // read once per pass, when a token is first read at latest
	headOnce := func(ctx context.Context) (uint64, error) {
		if head != 0 {
			return head, nil
		}
		h, err := s.onchain.rpc.blockNumber(ctx)
		if err != nil {
			return 0, err
		}
		head = h
		return h, nil
	}
	var noStateUpTo uint64       // the newest entry block found without state: older ones have none either
	var pending, struck []string // tokens the node answered with an error; struck once a later token is answered
	tokens, atEntry, atLatest, rows, fails := 0, 0, 0, copied, 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		var r tokenSupplyRead // not an EVM address: no supply, block 0 (not asked again)
		if evmAddrRe.MatchString(t.CA) {
			r, err = s.onchain.tokenSupplyAt(ctx, t.CA, t.EntryBlock, t.EntryBlock > noStateUpTo, headOnce)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				var re *rpcError
				if errors.As(err, &re) { // an answer, not a lost connection: may be the token itself
					pending = append(pending, t.CA)
				}
				if fails++; fails >= 2 {
					log.Printf("token supply: the node did not answer for %s (%v) — the rest is tried again on the next pass", t.CA, err)
					break
				}
				log.Printf("token supply: the node did not answer for %s (%v) — skipped this pass", t.CA, err)
				continue
			}
			fails = 0
			struck, pending = append(struck, pending...), nil
			if r.NoStateAtEntry {
				noStateUpTo = max(noStateUpTo, t.EntryBlock)
			}
		}
		n, err := s.db.SetTokenSupply(ctx, t.CA, r.Supply, r.Block)
		if err != nil {
			log.Printf("token supply: %v", err)
			break
		}
		s.clearSupplyStrike(t.CA)
		tokens++
		rows += n
		switch {
		case r.Supply == nil:
		case r.AtEntry:
			atEntry++
		default:
			atLatest++
		}
	}
	// Tokens that failed with no token answered after them (the last ones of
	// the pass, the only ones, or the two that ended it): struck when a fresh
	// request shows the node answering (eth_getBalance at latest: not cached,
	// unlike the head). After a database error too: the probe still decides
	// for the tokens that failed before it.
	if len(pending) > 0 && ctx.Err() == nil {
		var bal string
		if err := s.onchain.rpc.call(ctx, &bal, "eth_getBalance", zeroAddr, "latest"); err == nil {
			struck = append(struck, pending...)
		}
	}
	gaveUp := 0
	for _, ca := range struck {
		if ctx.Err() != nil || !s.supplyStrike(ca) {
			continue
		}
		n, err := s.db.SetTokenSupply(ctx, ca, nil, 0)
		if err != nil {
			log.Printf("token supply: %v", err)
			continue
		}
		s.clearSupplyStrike(ca)
		log.Printf("token supply: %s gave an error in %d passes in which the node otherwise answered — given up (no supply, block 0)", ca, tokenSupplyMaxStrikes)
		gaveUp++
		rows += n
	}
	if tokens > 0 || copied > 0 || gaveUp > 0 {
		log.Printf("token supply: looked up %d token(s) (at entry block: %d, at latest: %d, none: %d), %d given up, %d call(s) updated",
			tokens, atEntry, atLatest, tokens-atEntry-atLatest, gaveUp, rows)
	}
	return tokens
}

// supplyStrike counts one more pass in which ca's supply lookup got an error
// while the node otherwise answered; true once it reaches tokenSupplyMaxStrikes.
func (s *scanner) supplyStrike(ca string) bool {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()
	if s.supplyStrikes == nil {
		s.supplyStrikes = map[string]int{}
	}
	k := strings.ToLower(ca)
	s.supplyStrikes[k]++
	return s.supplyStrikes[k] >= tokenSupplyMaxStrikes
}

// struckLast returns targets with the tokens that have strikes moved to the
// end (order otherwise kept), so tokens that keep failing cannot hold up the
// others: the pass ends after two failures in a row.
func (s *scanner) struckLast(targets []supplyTarget) []supplyTarget {
	s.supplyMu.Lock()
	struck := make(map[string]bool, len(s.supplyStrikes))
	for k, n := range s.supplyStrikes {
		struck[k] = n > 0
	}
	s.supplyMu.Unlock()
	if len(struck) == 0 {
		return targets
	}
	out := make([]supplyTarget, 0, len(targets))
	var tail []supplyTarget
	for _, t := range targets {
		if struck[strings.ToLower(t.CA)] {
			tail = append(tail, t)
			continue
		}
		out = append(out, t)
	}
	return append(out, tail...)
}

// clearSupplyStrike forgets ca's strikes (its supply was stored).
func (s *scanner) clearSupplyStrike(ca string) {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()
	delete(s.supplyStrikes, strings.ToLower(ca))
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
	rug := t.CurrentLiquidityUSD != nil && geckoRugged(*t.CurrentLiquidityUSD, s.pc.RugLiqUSD)
	if t.CurrentPriceUSD != nil && *t.CurrentPriceUSD < *t.EntryPriceUSD*0.05 {
		rug = true
	}
	t.Rugged = &rug
	t.Status = TrackDone
	t.NextCheckAt = now
}

// geckoRugged: GeckoTerminal's reserve_usd counts both sides of the pool;
// SCOUT_RUG_LIQ_USD is meant for the quote side, taken as half the reserve
// (current_liquidity_usd keeps the reserve itself: the pool's depth, like the
// on-chain source's 2 × the quote side).
func geckoRugged(reserveUSD, rugLiqUSD float64) bool {
	return reserveUSD/2 < rugLiqUSD
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
	Updates           int // update posts with a CA: recorded, but not calls (in none of the numbers above except Posts)
}

// backfillMessage records the calls in one historical post. An update post is
// recorded with status "update" and no tracking row, and counted on its own.
func (s *scanner) backfillMessage(m *tg.Message, st *backfillStats) {
	st.Posts++
	urls := postURLs(m)
	cas := extractCAs(m.Message, urls, s.cfg.Chains)
	if len(cas) == 0 {
		return
	}
	if postKind(m.Message) == PostKindUpdate {
		st.Updates++
		for _, ca := range cas {
			s.recordCallInfo(context.Background(), m, ca, urls, CallStatusUpdate)
		}
		return
	}
	st.Calls++
	for _, ca := range cas {
		id, created := s.recordCallInfo(context.Background(), m, ca, urls, CallStatusBackfill)
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
		log.Printf("backfill: down to post %d — %d posts, %d calls, %d CAs (%d new, %d already recorded), %d update post(s)",
			msgs[0].ID, st.Posts, st.Calls, st.CAs, st.New, st.Existing, st.Updates)
		if msgs[0].ID <= 1 {
			return st, nil
		}
		offset = msgs[0].ID
		if err := sleepCtx(ctx, time.Second); err != nil {
			return st, err
		}
	}
}
