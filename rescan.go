package main

// Rescan lane: Perceptor re-scans of first calls that have no Perceptor
// report, run inside the listener (one Telegram session; never a second
// process) when the live queue is idle. Off unless SCOUT_RESCAN=on.
//
// A rescan writes one scout_investigations row with scan_kind 'rescan' and
// nothing else: no delivery, no scout_calls status change, no seen_cas.json
// entry, no model score, no tracking change. Its verdict is today's, so the
// training dataset (scout_call_dataset_v) and the requeue ignore it.
//
// The scan worker handles the live queue first. Only when the queue is empty,
// the last live call finished at least SCOUT_RESCAN_IDLE ago, the newest
// rescan was requested at least SCOUT_RESCAN_GAP ago and fewer than
// SCOUT_RESCAN_MAX_PER_DAY rescans were requested in the last 24 hours does it
// take one candidate (NextRescanCandidate) and scan it, synchronously, with
// no rate-limit retry. A live call that arrives meanwhile waits for it: at
// most Perceptor's MaxWait (180 s) plus the report page fetch, after which the
// runner's 2m5s pacing has already passed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tgerr"
)

const (
	rescanMaxAgeDefault    = 30 * 24 * time.Hour
	rescanMaxPerDayDefault = 100
	rescanGapDefault       = 10 * time.Minute
	rescanIdleDefault      = 5 * time.Minute
	rescanStatusesDefault  = "backfill,duplicate,failed,scanned"

	// rescanFailedLimit: a token with this many failed rescans (failed or
	// timeout) is not tried again.
	rescanFailedLimit = 2
	// rescanPause: how long the lane stops after a rate-limit reply or a
	// Telegram FLOOD_WAIT (longer when Telegram asks for longer).
	rescanPause = time.Hour
	// rescanTick: how often an idle worker looks at the lane.
	rescanTick = 30 * time.Second
	// rescanNoCandidateWait: after finding no candidate, the candidate query
	// is not run again for this long.
	rescanNoCandidateWait = 10 * time.Minute
	// rescanWindow: the daily cap counts the rescans of this rolling window.
	rescanWindow = 24 * time.Hour
	// rescanTool: the only tool the lane runs.
	rescanTool = "perceptor"
)

// rescanAllowedStatuses: the scout_calls statuses SCOUT_RESCAN_STATUSES may
// name. queued and dropped belong to the requeue; update posts are no calls.
var rescanAllowedStatuses = []string{CallStatusBackfill, CallStatusDuplicate, CallStatusFailed, CallStatusScanned}

// rescanConfig holds the SCOUT_RESCAN_* settings.
type rescanConfig struct {
	Enabled   bool          // SCOUT_RESCAN=on
	MaxAge    time.Duration // SCOUT_RESCAN_MAX_AGE: only first calls posted within this
	MaxPerDay int           // SCOUT_RESCAN_MAX_PER_DAY: rescans in a rolling 24 hours
	Gap       time.Duration // SCOUT_RESCAN_GAP: between two rescan requests
	Idle      time.Duration // SCOUT_RESCAN_IDLE: since the last live call finished
	Statuses  []string      // SCOUT_RESCAN_STATUSES: scout_calls.status of the first call
}

// loadRescanConfig reads the SCOUT_RESCAN_* settings; an invalid value is an
// error (the listener does not start).
func loadRescanConfig() (rescanConfig, error) {
	c := rescanConfig{MaxAge: rescanMaxAgeDefault, MaxPerDay: rescanMaxPerDayDefault,
		Gap: rescanGapDefault, Idle: rescanIdleDefault}
	switch v := strings.ToLower(env("SCOUT_RESCAN", "off")); v {
	case "on":
		c.Enabled = true
	case "off":
	default:
		return c, fmt.Errorf("SCOUT_RESCAN=%q: want on or off", v)
	}
	dur := func(key string, def time.Duration, min time.Duration) (time.Duration, error) {
		v := env(key, "")
		if v == "" {
			return def, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < min {
			return 0, fmt.Errorf("%s=%q: want a duration of at least %s, such as %s", key, v, min, def)
		}
		return d, nil
	}
	var err error
	if c.MaxAge, err = dur("SCOUT_RESCAN_MAX_AGE", c.MaxAge, time.Hour); err != nil {
		return c, err
	}
	if c.Gap, err = dur("SCOUT_RESCAN_GAP", c.Gap, 0); err != nil {
		return c, err
	}
	if c.Idle, err = dur("SCOUT_RESCAN_IDLE", c.Idle, 0); err != nil {
		return c, err
	}
	if v := env("SCOUT_RESCAN_MAX_PER_DAY", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return c, fmt.Errorf("SCOUT_RESCAN_MAX_PER_DAY=%q: want a number from 1 to 10000", v)
		}
		c.MaxPerDay = n
	}
	c.Statuses = nil
	for _, s := range splitList(env("SCOUT_RESCAN_STATUSES", rescanStatusesDefault)) {
		if !slices.Contains(rescanAllowedStatuses, s) {
			return c, fmt.Errorf("SCOUT_RESCAN_STATUSES: %q is not allowed (use some of %s)", s, strings.Join(rescanAllowedStatuses, ","))
		}
		if !slices.Contains(c.Statuses, s) {
			c.Statuses = append(c.Statuses, s)
		}
	}
	if len(c.Statuses) == 0 {
		return c, errors.New("SCOUT_RESCAN_STATUSES is empty")
	}
	return c, nil
}

// perDay is how many rescans a day the settings allow at most: the cap, or
// fewer when the gap does not leave room for it.
func (c rescanConfig) perDay() int {
	if c.Gap <= 0 {
		return c.MaxPerDay
	}
	return min(c.MaxPerDay, int(rescanWindow/c.Gap))
}

// describe is the settings in one line, for the logs.
func (c rescanConfig) describe() string {
	return fmt.Sprintf("Perceptor re-scans of first calls without a report: at most %d a day, %s apart, after the live queue has been idle %s; calls up to %s old with status %s",
		c.MaxPerDay, c.Gap, c.Idle, c.MaxAge, strings.Join(c.Statuses, ","))
}

// ---------------------------------------------------------------------------
// Decision (pure)
// ---------------------------------------------------------------------------

// Reasons rescanDecide gives.
const (
	rescanGo           = "go"            // scan a candidate now
	rescanBusy         = "busy"          // live calls are queued
	rescanNotIdle      = "not_idle"      // a live call finished less than Idle ago
	rescanPaused       = "paused"        // after a rate limit or FLOOD_WAIT
	rescanNoCandidates = "no_candidates" // the last look found none
	rescanNeedStats    = "need_stats"    // read RescanStats, then decide again
	rescanCapReached   = "cap"           // MaxPerDay rescans in the last 24 hours
	rescanGapWait      = "gap"           // the newest rescan is less than Gap old
)

// rescanInputs is what the lane knows when it decides.
type rescanInputs struct {
	Now              time.Time
	QueueLen         int       // live jobs waiting
	LastLive         time.Time // when the last live call finished (or the worker started)
	PausedUntil      time.Time
	NoCandidateUntil time.Time
	// From the database (RescanStats); only read when StatsKnown.
	StatsKnown bool
	Today      int        // rescans requested in the last 24 hours
	LastRescan *time.Time // the newest rescan's requested_at (nil = none)
}

// rescanDecide says whether the lane may scan a candidate now, and if not,
// why. The checks that need no database come first: with rescanNeedStats the
// caller reads the counts and asks again.
func rescanDecide(c rescanConfig, in rescanInputs) string {
	switch {
	case in.QueueLen > 0:
		return rescanBusy
	case in.Now.Before(in.PausedUntil):
		return rescanPaused
	case in.Now.Sub(in.LastLive) < c.Idle:
		return rescanNotIdle
	case in.Now.Before(in.NoCandidateUntil):
		return rescanNoCandidates
	case !in.StatsKnown:
		return rescanNeedStats
	case in.Today >= c.MaxPerDay:
		return rescanCapReached
	case in.LastRescan != nil && in.Now.Sub(*in.LastRescan) < c.Gap:
		return rescanGapWait
	}
	return rescanGo
}

// ---------------------------------------------------------------------------
// The lane
// ---------------------------------------------------------------------------

// rescanLane is the lane's state. It lives on the scanner (not in the worker),
// so a reconnect, which starts a new worker, keeps a pause. The fields after
// mu are guarded by it.
type rescanLane struct {
	cfg    rescanConfig
	runner *toolRunner
	toolID int
	now    func() time.Time
	// send sends text to the runner's bot; judge reads the verdict of a
	// completed report (the scanner's own by default; tests use fakes).
	send  func(ctx context.Context, r *toolRunner, text string) error
	judge func(ctx context.Context, spec ToolSpec, res *toolResult) verdict

	mu               sync.Mutex
	lastLive         time.Time
	pausedUntil      time.Time
	noCandidateUntil time.Time
	// lastRescan: when this lane last requested a rescan (zero = none since
	// start). A floor for the database's newest requested_at, so the gap holds
	// even when storing the row failed.
	lastRescan time.Time
	lastReason string // the last "not now" reason logged (cap / no candidates), to log each once
	errLog     logLimiter
}

// newRescanLane returns the lane, or nil when it is off or cannot run (logged
// once): SCOUT_RESCAN=off, no database, no Perceptor tool or its id unknown.
func (s *scanner) newRescanLane(c rescanConfig) *rescanLane {
	if !c.Enabled {
		return nil
	}
	r := s.runnerFor(rescanTool)
	switch {
	case s.db == nil:
		log.Printf("rescan: SCOUT_RESCAN=on but the database is off — the rescan lane stays off")
		return nil
	case r == nil:
		log.Printf("rescan: SCOUT_RESCAN=on but %q is not in SCOUT_TOOLS — the rescan lane stays off", rescanTool)
		return nil
	case r.toolID == nil:
		log.Printf("rescan: SCOUT_RESCAN=on but the %s tool is not registered in the database — the rescan lane stays off", rescanTool)
		return nil
	}
	l := &rescanLane{cfg: c, runner: r, toolID: *r.toolID, now: time.Now,
		send: func(ctx context.Context, r *toolRunner, text string) error {
			_, err := s.sender.To(r.botPeer).Text(ctx, text)
			return err
		},
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			return s.judge(ctx, spec, res.Replies)
		},
		errLog: logLimiter{every: 10 * time.Minute},
	}
	log.Printf("rescan: on — %s", c.describe())
	return l
}

// noteLive records that a live call finished (or a worker started) at t.
func (l *rescanLane) noteLive(t time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastLive = t
}

// inputs returns the lane's own part of rescanInputs.
func (l *rescanLane) inputs(now time.Time, queueLen int) rescanInputs {
	l.mu.Lock()
	defer l.mu.Unlock()
	return rescanInputs{Now: now, QueueLen: queueLen, LastLive: l.lastLive,
		PausedUntil: l.pausedUntil, NoCandidateUntil: l.noCandidateUntil}
}

// logOnce logs line (when not empty) if reason differs from the last one
// noted this way, so a waiting state is logged once, not on every tick.
func (l *rescanLane) logOnce(reason, line string) {
	l.mu.Lock()
	same := l.lastReason == reason
	l.lastReason = reason
	l.mu.Unlock()
	if !same && line != "" {
		log.Print(line)
	}
}

// logErr logs a database error of the lane, at most once every 10 minutes.
func (l *rescanLane) logErr(err error) {
	l.mu.Lock()
	ok := l.errLog.allow(l.now())
	l.mu.Unlock()
	if ok {
		log.Printf("rescan: %v (retried later)", err)
	}
}

// rescanStore is the part of ScoutStore the lane uses.
type rescanStore interface {
	RescanStats(ctx context.Context, since time.Time) (int, *time.Time, error)
	NextRescanCandidate(ctx context.Context, statuses []string, since time.Time, maxFailed int) (*RescanCandidate, error)
	InsertScoutInvestigation(ctx context.Context, r *ScoutInvestigation) (*int, error)
}

// step looks at the lane once (the worker calls it when the live queue is
// empty) and runs at most one rescan, synchronously. It reports whether it
// ran one (then the worker looks at the live queue again at once).
func (l *rescanLane) step(ctx context.Context, st rescanStore, queueLen func() int) bool {
	now := l.now()
	in := l.inputs(now, queueLen())
	reason := rescanDecide(l.cfg, in)
	if reason == rescanNeedStats {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		n, last, err := st.RescanStats(qctx, now.Add(-rescanWindow))
		cancel()
		if err != nil {
			l.logErr(err)
			return false
		}
		in.StatsKnown, in.Today, in.LastRescan = true, n, last
		if mine := l.lastRescanAt(); !mine.IsZero() && (last == nil || mine.After(*last)) {
			in.LastRescan = &mine
		}
		reason = rescanDecide(l.cfg, in)
	}
	if reason == rescanCapReached {
		l.logOnce(reason, fmt.Sprintf("rescan: %d of %d done in the last 24 hours — waiting", in.Today, l.cfg.MaxPerDay))
	}
	if reason != rescanGo {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := st.NextRescanCandidate(qctx, l.cfg.Statuses, now.Add(-l.cfg.MaxAge), rescanFailedLimit)
	cancel()
	if err != nil {
		l.logErr(err)
		return false
	}
	if c == nil {
		l.mu.Lock()
		l.noCandidateUntil = now.Add(rescanNoCandidateWait)
		l.mu.Unlock()
		l.logOnce(rescanNoCandidates, "rescan: no first call left to re-scan — looking again every "+rescanNoCandidateWait.String())
		return false
	}
	l.logOnce(rescanGo, "")
	l.rescan(ctx, st, *c, in.Today+1)
	return true
}

// rescan runs Perceptor once for c (no rate-limit retry) and stores the
// result as a scan_kind 'rescan' investigation of c's call. Nothing else is
// written. A scan cut off by a stop, and a send that Telegram refused with
// FLOOD_WAIT, store nothing; a FLOOD_WAIT or a rate-limit reply pauses the
// lane. nth is this rescan's number in the last 24 hours, for the log.
func (l *rescanLane) rescan(ctx context.Context, st rescanStore, c RescanCandidate, nth int) {
	r := l.runner
	l.mu.Lock()
	l.lastRescan = l.now() // the gap counts from the request, like requested_at
	l.mu.Unlock()
	send := func(ctx context.Context, text string) error { return l.send(ctx, r, text) }
	res := r.investigateWithRetryN(ctx, send, c.ContractAddress, 0)
	now := l.now()
	age := now.Sub(c.MessageDate)
	what := fmt.Sprintf("rescan: call %d (%s, %s old)", c.CallID, c.ContractAddress, rescanAgeText(age))
	if ctx.Err() != nil {
		log.Printf("%s: stopped before the report came (not recorded)", what)
		return
	}
	if d, ok := tgerr.AsFloodWait(res.Err); ok {
		l.pause(now, max(rescanPause, d))
		log.Printf("%s: Telegram FLOOD_WAIT %s — not recorded; rescan lane paused until %s", what, d, l.pausedAt().Format(time.RFC3339))
		return
	}
	if res.Status == investigationCompleted {
		res.Verdict = l.judge(ctx, r.spec, res)
	} else {
		res.Verdict = verdict{Level: levelUnknown}
	}
	if ctx.Err() != nil {
		// a stop while the report page was read: the verdict may be cut short
		log.Printf("%s: stopped while reading the report (not recorded)", what)
		return
	}
	row := investigationRow(res, &c.CallID, c.ContractAddress, l.toolID)
	row.ScanKind = ScanKindRescan
	row.Details = rescanDetails(row.Details, age)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	_, err := st.InsertScoutInvestigation(wctx, row)
	cancel()
	outcome := res.Verdict.Level
	if res.Status != investigationCompleted {
		outcome = res.Status
		if res.Err != nil {
			outcome += " — " + res.Err.Error()
		}
	}
	if err != nil {
		log.Printf("%s: %s, but storing it failed: %v", what, outcome, err)
	} else {
		log.Printf("%s: %s (%d of %d today)", what, outcome, nth, l.cfg.MaxPerDay)
	}
	if res.Status == investigationRateLimited {
		l.pause(now, rescanPause)
		log.Printf("rescan: @%s is rate-limiting — rescan lane paused until %s", r.spec.Bot, l.pausedAt().Format(time.RFC3339))
	}
}

func (l *rescanLane) pause(now time.Time, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := now.Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

func (l *rescanLane) pausedAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pausedUntil
}

func (l *rescanLane) lastRescanAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastRescan
}

// rescanDetails adds {"rescan": true, "call_age_s": …} to the details JSON of
// an investigation (replyDetails' object), keeping its keys.
func rescanDetails(details []byte, age time.Duration) []byte {
	m := map[string]any{}
	if len(details) > 0 {
		if err := json.Unmarshal(details, &m); err != nil {
			m = map[string]any{} // replyDetails always writes an object; start afresh otherwise
		}
	}
	m["rescan"] = true
	m["call_age_s"] = int64(age / time.Second)
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{"rescan":true}`) // cannot happen: the map holds JSON values only
	}
	return b
}

// rescanAgeText writes an age the way the log shows it: 45m, 7h, 12d.
func rescanAgeText(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// ---------------------------------------------------------------------------
// -rescan-missing -dry-run
// ---------------------------------------------------------------------------

// rescanSummary is what the dry run prints.
type rescanSummary struct {
	Pool        int            // first calls without a Perceptor report, with an allowed status
	TooOld      int            // of those: posted before MaxAge
	Rugged      int            // … rugged
	Wiped       int            // … latest return ≤ −99%
	FailedTwice int            // … rescanFailedLimit failed rescans
	Candidates  int            // the rest
	ByStatus    map[string]int // candidates per status
	ByAge       [4]int         // candidates per age: <1d, 1–7d, 7–30d, >30d
	// List: the candidates in the order the lane takes them (newest first,
	// ties to the higher call id); Now: the time their ages are counted from.
	List []RescanPoolRow
	Now  time.Time
}

// rescanAgeBuckets names summary.ByAge.
var rescanAgeBuckets = [4]string{"<1d", "1-7d", "7-30d", ">30d"}

// summarizeRescan sorts the pool into the exclusions (each first call counted
// once, in the order too old, rugged, ≤ −99%, failed twice) and the
// candidates. It applies the same rules as NextRescanCandidate. pool is in
// SelectRescanPool's order (the lane's), which List keeps.
func summarizeRescan(c rescanConfig, now time.Time, pool []RescanPoolRow) rescanSummary {
	s := rescanSummary{Pool: len(pool), ByStatus: map[string]int{}, Now: now}
	since := now.Add(-c.MaxAge)
	for _, r := range pool {
		switch {
		case r.MessageDate.Before(since):
			s.TooOld++
		case r.Rugged:
			s.Rugged++
		case r.Wiped:
			s.Wiped++
		case r.FailedRescans >= rescanFailedLimit:
			s.FailedTwice++
		default:
			s.Candidates++
			s.List = append(s.List, r)
			s.ByStatus[r.Status]++
			age := now.Sub(r.MessageDate)
			switch {
			case age < 24*time.Hour:
				s.ByAge[0]++
			case age < 7*24*time.Hour:
				s.ByAge[1]++
			case age < 30*24*time.Hour:
				s.ByAge[2]++
			default:
				s.ByAge[3]++
			}
		}
	}
	return s
}

// runRescanDryRun prints how many first calls the rescan lane would re-scan
// under the current settings, and how long that would take. Database only.
func runRescanDryRun(ctx context.Context, st *ScoutStore, w io.Writer, c rescanConfig, now time.Time) error {
	pool, err := st.SelectRescanPool(ctx, c.Statuses)
	if err != nil {
		return err
	}
	writeRescanSummary(w, c, summarizeRescan(c, now, pool))
	return nil
}

// writeRescanSummary prints a summary (see runRescanDryRun).
func writeRescanSummary(w io.Writer, c rescanConfig, s rescanSummary) {
	state := "off"
	if c.Enabled {
		state = "on"
	}
	fmt.Fprintf(w, "rescan dry run (nothing is scanned; no call, investigation or delivery is written); SCOUT_RESCAN=%s\n", state)
	fmt.Fprintf(w, "settings: %s\n", c.describe())
	fmt.Fprintf(w, "first calls without a Perceptor report, status %s: %d\n", strings.Join(c.Statuses, ","), s.Pool)
	fmt.Fprintf(w, "excluded (each counted once, in this order): too old %d, rugged %d, latest return <= %d%% %d, failed twice %d\n",
		s.TooOld, s.Rugged, rescanWipedPct, s.Wiped, s.FailedTwice)
	fmt.Fprintf(w, "candidates: %d\n", s.Candidates)
	var parts []string
	for _, st := range c.Statuses {
		parts = append(parts, fmt.Sprintf("%s %d", st, s.ByStatus[st]))
	}
	fmt.Fprintf(w, "  by status: %s\n", strings.Join(parts, ", "))
	parts = parts[:0]
	for i, name := range rescanAgeBuckets {
		if i == 3 && c.MaxAge <= 30*24*time.Hour {
			break // no candidate can be older than 30 days
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, s.ByAge[i]))
	}
	fmt.Fprintf(w, "  by age: %s\n", strings.Join(parts, ", "))
	perDay := c.perDay()
	days := math.Ceil(float64(s.Candidates) / float64(perDay))
	fmt.Fprintf(w, "ETA: at most %d a day (cap %d, gap %s) -> about %.0f day(s) for these %d; longer in practice: the lane runs only while the live queue is idle\n",
		perDay, c.MaxPerDay, c.Gap, days, s.Candidates)
	if !c.Enabled {
		fmt.Fprintln(w, "the lane runs inside the listener once SCOUT_RESCAN=on is set")
	}
	if len(s.List) == 0 {
		return
	}
	fmt.Fprintln(w, "candidates in the order the lane takes them (newest first): call id, posted (UTC), age, status, contract address")
	for _, r := range s.List {
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\n", r.CallID, r.MessageDate.UTC().Format(time.RFC3339),
			rescanAgeText(s.Now.Sub(r.MessageDate)), r.Status, r.ContractAddress)
	}
}
