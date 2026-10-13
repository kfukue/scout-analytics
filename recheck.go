package main

// Live Perceptor re-check: Perceptor answers "not enough data yet"
// (levelInsufficient) for a token too new to judge, and sometimes gives no
// readable verdict (levelUnknown). Neither passes the gate, so such a live call
// used to be dropped. Now process() holds it back instead, and this lane asks
// Perceptor again:
//
//   - SCOUT_PERCEPTOR_RECHECK_AFTER (12m) after the call: the delivery
//     decision. clean or caution (SCOUT_DELIVER_LEVELS) -> delivered then, the
//     header saying "Perceptor re-checked 12m after the call"; red flags -> not
//     delivered; still no verdict -> delivered with "Perceptor: not enough data
//     yet" (or "no readable verdict" / "no report"). Stored as scan_kind 'live'
//     with details {"recheck":1,"call_age_s":…}. The website prefers it over
//     the first answer; the dataset view (model input) and the Analytics
//     "verdict at call" never use it: they keep the answer known at the call.
//     A re-check that gets no report (timeout, failed, rate_limited) stores
//     nothing and is tried again after recheckRetryPause while its window
//     lasts; the last try that fits the window is stored and delivered with
//     the label, so the call is not lost.
//   - SCOUT_PERCEPTOR_RECHECK2_AFTER (2h) after the call, only when there is
//     still no real verdict: website only, no delivery. Stored as scan_kind
//     'rescan' with details {"recheck":2,…}, so it shows as "Perceptor today"
//     and never feeds the model.
//
// After that the rescan lane (rescan.go) covers the call. process() marks the
// call's live Perceptor investigation held (details {"held_for_recheck":true},
// MarkHeldForRecheck), and what is due is read from the database
// (SelectPerceptorRechecks, marked calls only), so a restart keeps the
// schedule. A call this lane did not hold (another gate said no, delivered at
// the call, -scan / -post, a dry run, a call an older binary dropped) is never
// re-checked. A restart does lose the in-memory "owed" retries
// (recheckLane.owed): a re-check whose window passes while the listener is
// down, even one that was being retried, is skipped and logged, and the call
// stays undelivered. The lane runs in the scan worker, after the live queue,
// and asks Perceptor only when its runner's pacing (MinInterval) has passed
// and no rate-limit pause is on, with no rate-limit retries in the runner (a
// rate-limit notice is tried again recheckRetryPause later), so a live call
// never waits behind a pacing sleep or retry of the lane (one that comes right
// after a re-check request still waits Perceptor's normal pacing, as after any
// request); delivering a stored re-check (which does not ask Perceptor) is
// held back by neither. A Telegram
// FLOOD_WAIT stores nothing and pauses the whole lane for the time Telegram
// asks. A stop (reconnect or shutdown) during a re-check stores nothing, so
// the re-check is run again. A delivery is recorded (status sending) before it
// is sent and updated to sent or failed after; any delivery row of a call,
// sending included, means it is never delivered again, so a failed update
// cannot cause a second send after a restart. A stop before anything was sent
// removes that record; the stored first re-check is then delivered on a later
// step (recheckDeliver), without asking Perceptor again. In a dry run
// (-dry-run) the lane is off: nothing is held, re-checked or stored.
//
// Model score line: process() scores every call when it comes in (scoreCall,
// from call-time data) and a held call keeps that line in memory
// (recheckLane.scores) and stores it with the held marker (details
// {"score_line":…} of the same investigation, MarkHeldForRecheck; only when
// there is a line); the re-check delivery shows it last, exactly as a delivery
// at the call would have: the line in memory, else the stored one
// (PerceptorRecheckRow.ScoreLine), so a re-check delivered after a restart of
// the listener shows the same first-pass line. The score is never asked again
// for the re-check (re-check results are never model inputs), and the stored
// line is display text only: the dataset view and the export never read it.
// A missing line never holds back or delays a delivery.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	recheckAfterDefault  = 12 * time.Minute
	recheckAfterMin      = 5 * time.Minute
	recheckAfterMax      = 60 * time.Minute
	recheck2AfterDefault = 2 * time.Hour
	recheck2AfterMax     = 24 * time.Hour
	// recheckWindowMin and recheckWindowSlack: the first re-check is made for
	// calls up to max(recheckWindowMin, AFTER + recheckWindowSlack) old.
	recheckWindowMin   = 60 * time.Minute
	recheckWindowSlack = 30 * time.Minute
	// recheck2Window: the second re-check is made up to this long after it is due.
	recheck2Window = time.Hour
	// recheckTick: how often the lane reads the database while the worker is idle.
	recheckTick = 30 * time.Second
	// recheckTool: the tool re-checked (the gate).
	recheckTool = "perceptor"
	// recheckRetryPause: a first re-check that got no report (timeout, failed,
	// rate_limited) is tried again this long after, while its window lasts.
	recheckRetryPause = 5 * time.Minute
)

// recheckConfig holds the SCOUT_PERCEPTOR_RECHECK* settings.
type recheckConfig struct {
	After  time.Duration // SCOUT_PERCEPTOR_RECHECK_AFTER; 0 = the whole re-check is off
	After2 time.Duration // SCOUT_PERCEPTOR_RECHECK2_AFTER; 0 = no second re-check
}

func (c recheckConfig) enabled() bool { return c.After > 0 }

// maxAge1: the oldest a call may be for its first re-check.
func (c recheckConfig) maxAge1() time.Duration {
	return max(recheckWindowMin, c.After+recheckWindowSlack)
}

// maxAge2: the oldest a call may be for its second re-check.
func (c recheckConfig) maxAge2() time.Duration { return c.After2 + recheck2Window }

// horizon: calls younger than this may still get a re-check (0 = off). The
// rescan lane leaves them alone.
func (c recheckConfig) horizon() time.Duration {
	switch {
	case !c.enabled():
		return 0
	case c.After2 > 0:
		return c.maxAge2()
	}
	return c.maxAge1()
}

// describe is the settings in one line, for the logs.
func (c recheckConfig) describe() string {
	second := "no second re-check"
	if c.After2 > 0 {
		second = fmt.Sprintf("a second one (website only) %s after the call, up to %s", c.After2, c.maxAge2())
	}
	return fmt.Sprintf("live calls whose Perceptor answer is \"not enough data yet\" or unreadable wait for a re-check %s after the call (up to %s old), then are delivered unless it finds red flags; %s",
		c.After, c.maxAge1(), second)
}

// loadRecheckConfig reads the settings; an invalid value is an error (the
// listener does not start).
func loadRecheckConfig() (recheckConfig, error) {
	var c recheckConfig
	var err error
	if c.After, err = recheckDur("SCOUT_PERCEPTOR_RECHECK_AFTER", recheckAfterDefault, recheckAfterMin, recheckAfterMax); err != nil {
		return c, err
	}
	if !c.enabled() {
		return c, nil
	}
	c.After2, err = recheckDur("SCOUT_PERCEPTOR_RECHECK2_AFTER", recheck2AfterDefault, c.maxAge1(), recheck2AfterMax)
	return c, err
}

// recheckDur reads one duration setting: empty = def, "0" or "off" = 0,
// otherwise a duration from lo to hi.
func recheckDur(key string, def, lo, hi time.Duration) (time.Duration, error) {
	v := strings.ToLower(env(key, ""))
	switch v {
	case "":
		return def, nil
	case "0", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return 0, fmt.Errorf("%s=%q: want 0 (off) or a duration from %s to %s, such as %s", key, v, lo, hi, def)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Decision (pure)
// ---------------------------------------------------------------------------

// What a call needs now.
type recheckStep int

const (
	recheckNothing recheckStep = iota // a real verdict already, or nothing left to do
	recheckWait                       // a re-check comes later (recheckPlan.Due)
	recheckFirst                      // run the first re-check now (the delivery decision)
	recheckSecond                     // run the second re-check now (website only)
	recheckDeliver                    // deliver the stored first re-check now (its delivery was stopped before anything was sent)
)

// recheckPlan is recheckDecide's answer.
type recheckPlan struct {
	Step recheckStep
	Due  time.Time // recheckWait: when the next re-check is due
	// FirstMissed and SecondMissed: that re-check's window passed without it
	// (the listener was down); it is skipped, and the call is not delivered.
	// DeliverMissed: the same for the delivery of a stored first re-check.
	FirstMissed, SecondMissed, DeliverMissed bool
}

// recheckDecide says what call p needs at now. A stored first re-check that
// deliverable says is to be delivered (nil: none is) but has no delivery is
// delivered up to maxAge1 after the call. The first re-check is for a call
// not delivered and not re-checked yet, from After to maxAge1 after the call;
// the second for a call without a second one, from After2 to maxAge2. A call
// with a real verdict (any Perceptor scan of the call) needs nothing else.
func recheckDecide(c recheckConfig, now time.Time, p PerceptorRecheckRow, deliverable func(status, level string) bool) recheckPlan {
	var pl recheckPlan
	if !c.enabled() {
		return pl
	}
	age := now.Sub(p.MessageDate)
	if p.Recheck1ID != nil && !p.Delivered && deliverable != nil && deliverable(p.Recheck1Status, p.Recheck1Level) {
		if age <= c.maxAge1() {
			return recheckPlan{Step: recheckDeliver}
		}
		pl.DeliverMissed = true
	}
	if p.Usable {
		return pl
	}
	if !p.Recheck1 && !p.Delivered {
		switch {
		case age < c.After:
			return recheckPlan{Step: recheckWait, Due: p.MessageDate.Add(c.After)}
		case age <= c.maxAge1():
			return recheckPlan{Step: recheckFirst}
		}
		pl.FirstMissed = true
	}
	if c.After2 <= 0 || p.Recheck2 {
		return pl
	}
	switch {
	case age < c.After2:
		pl.Step, pl.Due = recheckWait, p.MessageDate.Add(c.After2)
	case age <= c.maxAge2():
		pl.Step = recheckSecond
	default:
		pl.SecondMissed = true
	}
	return pl
}

// ---------------------------------------------------------------------------
// Holding a live call back (called by process)
// ---------------------------------------------------------------------------

// deferToRecheck reports whether a live call that the gate did not let
// through waits for a Perceptor re-check instead (and logs it): the re-check
// is on, the call is in the database, every other gate passed and Perceptor
// completed with "not enough data yet" or an unreadable verdict that
// SCOUT_DELIVER_LEVELS does not deliver. A Perceptor scan that did not
// complete (timeout, failed) is not held back: it stays undelivered, as before.
// invID is that Perceptor result's investigation (nil: storing it failed).
// A held call's investigation is marked held_for_recheck: the lane finds the
// calls to re-check in the database by that marker, so a call whose result
// was not stored, or whose marker cannot be written, is not held back (it
// stays undelivered, as before, and that is logged).
// scoreLine is the call's model score line computed at the call (scoreCall;
// "" without one): a held call keeps it for its re-check delivery, in memory
// and stored with the marker.
func (s *scanner) deferToRecheck(ctx context.Context, j job, results []*toolResult, invID *int, scoreLine string) bool {
	if s.recheck == nil || j.CallID == nil {
		return false
	}
	var perc *toolResult
	for _, r := range results {
		if !r.Spec.Gate {
			continue
		}
		if r.Spec.Code == recheckTool {
			perc = r
			continue
		}
		if r.Status != investigationCompleted || !s.cfg.DeliverLevels[r.Verdict.Level] {
			return false // another gate said no
		}
	}
	if perc == nil || perc.Status != investigationCompleted || verdictUsable(perc.Verdict.Level) || s.cfg.DeliverLevels[perc.Verdict.Level] {
		return false
	}
	what := firstNonEmpty(perc.Verdict.Label, perc.Verdict.Level)
	if invID == nil {
		log.Printf("%s: Perceptor %s — storing it failed, so call %d cannot wait for a re-check — not delivered",
			j.CA, what, *j.CallID)
		return false
	}
	if err := s.recheck.markHeld(ctx, *invID, scoreLine); err != nil {
		log.Printf("%s: Perceptor %s — call %d cannot wait for a re-check (%v) — not delivered",
			j.CA, what, *j.CallID, err)
		return false
	}
	// A call handled late (catch-up or requeue after a long stop) may already
	// be past the first re-check's window: say so now, the lane would not.
	if age, ok := s.callAge(ctx, *j.CallID); ok && age > s.recheck.cfg.maxAge1() {
		s.recheck.mu.Lock()
		s.recheck.missed[recheckKey(*j.CallID, 1)] = true
		s.recheck.mu.Unlock()
		log.Printf("%s: Perceptor %s — call %d is %s old, past the re-check window — not delivered",
			j.CA, what, *j.CallID, rescanAgeText(age))
		return true
	}
	s.recheck.keepScore(*j.CallID, scoreLine)
	log.Printf("%s: Perceptor %s — not delivering yet; re-checking %s after the call (call %d)",
		j.CA, what, s.recheck.cfg.After, *j.CallID)
	return true
}

// callAge returns how long ago call id was posted (ok false when it cannot be
// read: no database, or an error, which is logged).
func (s *scanner) callAge(ctx context.Context, id int) (time.Duration, bool) {
	if s.db == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := s.db.GetScoutCall(ctx, id)
	if err != nil || c == nil {
		if err != nil {
			log.Printf("perceptor re-check: reading call %d: %v", id, err)
		}
		return 0, false
	}
	return time.Since(c.MessageDate), true
}

// ---------------------------------------------------------------------------
// The lane
// ---------------------------------------------------------------------------

// recheckLane is the re-check's state. It lives on the scanner, so a
// reconnect (a new worker) keeps a pause. The fields after mu are guarded by it.
type recheckLane struct {
	cfg    recheckConfig
	s      *scanner    // the header, the delivery record, the tools, the scan log
	db     *ScoutStore // never nil
	runner *toolRunner // Perceptor's
	toolID int
	now    func() time.Time
	// send sends text to the runner's bot, judge reads a completed report's
	// verdict and deliver sends a delivery and returns the id of its header
	// message in the delivery chat, 0 when unknown (the scanner's by default;
	// tests use fakes).
	send    func(ctx context.Context, r *toolRunner, text string) error
	judge   func(ctx context.Context, spec ToolSpec, res *toolResult) verdict
	deliver func(ctx context.Context, j job, header string, results []*toolResult) (int64, error)
	// hold marks an investigation held for the re-check (storing the score
	// line with it) and finish sets the outcome of a recorded delivery, with
	// its header's message id (nil: the database's MarkHeldForRecheck and
	// FinishScoutDelivery; tests use fakes).
	hold   func(ctx context.Context, invID int, scoreLine string) error
	finish func(ctx context.Context, id int, status string, at *time.Time, errText *string, notifyMsgID *int64) error

	mu          sync.Mutex
	nextLook    time.Time // the database is not read again before this
	pausedUntil time.Time // after a FLOOD_WAIT: nothing runs
	// percPausedUntil: after a rate_limited re-check Perceptor is not asked
	// before this; stored re-checks are still delivered
	percPausedUntil time.Time
	// tried: re-checks (and deliveries, n = recheckDeliverN) run in this
	// process. A key is released (retryLater) on every outcome that stored
	// nothing and sent nothing (a stop, FLOOD_WAIT, a retry, a delivery
	// stopped before anything was sent); it stays when something was stored
	// or sent, so a failed insert is never a reason to ask or deliver twice.
	tried   map[int]bool
	retryAt map[int]time.Time // recheckKey: not run again before this
	// owed: calls (id -> message date) whose first re-check or its delivery
	// was released in this process without storing or sending anything (a
	// retry, a stop, FLOOD_WAIT, a delivery stopped before sending). If the
	// window ends before the next try (a busy queue, a pause, a reconnect),
	// that try still runs once, so the call is not lost: the re-check is then
	// stored and delivered with the label. Kept up to recheckOwedMax.
	owed   map[int]time.Time
	missed map[int]bool // recheckKey: missed windows already logged
	// scores: the model score line of each call held in this process (call
	// id -> line, computed at the call), shown last in its re-check delivery.
	// Removed once that delivery has an outcome (sent or failed) or the
	// re-check finds red flags; given up after recheckOwedMax past the first
	// window. Also stored (details.score_line), the fallback after a restart
	// (see the file comment).
	scores map[int]heldScore
	errLog logLimiter
}

// heldScore is a held call's model score line and when it was kept.
type heldScore struct {
	line string
	at   time.Time
}

// recheckOwedMax: an owed call (recheckLane.owed) older than its first
// window plus this is given up (logged, not delivered).
const recheckOwedMax = 24 * time.Hour

// recheckDeliverN is the n of recheckKey for the delivery of a stored first
// re-check (recheckDeliver).
const recheckDeliverN = 3

// recheckKey is the key of a call's n-th re-check in recheckLane.tried,
// retryAt and missed.
func recheckKey(callID, n int) int { return callID*4 + n }

// retryLater releases p's n-th key so that step may run it again, not before
// at (zero: at once), and marks the call owed (see recheckLane.owed) when n is
// the first re-check or its delivery. l.mu must not be held.
func (l *recheckLane) retryLater(p PerceptorRecheckRow, n int, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := recheckKey(p.CallID, n)
	if n != 2 {
		l.owed[p.CallID] = p.MessageDate
	}
	delete(l.tried, k)
	if at.IsZero() {
		delete(l.retryAt, k)
		return
	}
	l.retryAt[k] = at
}

// deliverable says whether a stored first re-check (status, level) is one
// to deliver: anything but a real verdict that SCOUT_DELIVER_LEVELS leaves
// out.
func (l *recheckLane) deliverable(status, level string) bool {
	return status != investigationCompleted || !verdictUsable(level) || l.s.cfg.DeliverLevels[level]
}

// newRecheckLane returns the lane, or nil when the re-check is off or cannot
// run (logged once): a dry run, no database, no Perceptor tool, its id
// unknown, or it is not a gate. In a dry run nothing is held back, re-checked
// or stored, so a later real run never delivers anything from it.
func (s *scanner) newRecheckLane(c recheckConfig) *recheckLane {
	if !c.enabled() {
		log.Printf("perceptor re-check: off (SCOUT_PERCEPTOR_RECHECK_AFTER=0)")
		return nil
	}
	if s.cfg.DryRun {
		log.Printf("perceptor re-check: off in a dry run (nothing is held back, re-checked or stored)")
		return nil
	}
	r := s.runnerFor(recheckTool)
	switch {
	case s.db == nil:
		log.Printf("perceptor re-check: the database is off — the re-check stays off")
		return nil
	case r == nil || !r.spec.Gate:
		log.Printf("perceptor re-check: %q is not a gating tool in SCOUT_TOOLS — the re-check stays off", recheckTool)
		return nil
	case r.toolID == nil:
		log.Printf("perceptor re-check: the %s tool is not registered in the database — the re-check stays off", recheckTool)
		return nil
	}
	l := &recheckLane{cfg: c, s: s, db: s.db, runner: r, toolID: *r.toolID, now: time.Now,
		send: s.sendToBot,
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			return s.judge(ctx, spec, res.Replies)
		},
		deliver: s.deliver,
	}
	l.init()
	log.Printf("perceptor re-check: on — %s", c.describe())
	return l
}

// init sets the maps and the error log's pace.
func (l *recheckLane) init() {
	l.tried, l.missed, l.retryAt, l.owed = map[int]bool{}, map[int]bool{}, map[int]time.Time{}, map[int]time.Time{}
	l.scores = map[int]heldScore{}
	l.errLog = logLimiter{every: 10 * time.Minute}
}

// keepScore keeps call id's model score line (computed at the call) for its
// re-check delivery; an empty line keeps nothing.
func (l *recheckLane) keepScore(id int, line string) {
	if line == "" {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scores[id] = heldScore{line: line, at: now}
}

// scoreLine returns call id's kept model score line ("" without one).
func (l *recheckLane) scoreLine(id int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scores[id].line
}

// dropScore forgets call id's model score line.
func (l *recheckLane) dropScore(id int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.scores, id)
}

// markHeld marks investigation invID held for the re-check, storing the
// call's score line with it when there is one (a stop of ctx does not cut the
// write short).
func (l *recheckLane) markHeld(ctx context.Context, invID int, scoreLine string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if l.hold != nil {
		return l.hold(ctx, invID, scoreLine)
	}
	return l.db.MarkHeldForRecheck(ctx, invID, scoreLine)
}

// finishDelivery sets the outcome of delivery id, recorded as sending, and
// the id of its header message in the delivery chat (nil = unknown).
func (l *recheckLane) finishDelivery(ctx context.Context, id int, status string, at *time.Time, errText *string, notifyMsgID *int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if l.finish != nil {
		return l.finish(ctx, id, status, at, errText, notifyMsgID)
	}
	return l.db.FinishScoutDelivery(ctx, id, status, at, errText, notifyMsgID)
}

// paceWait returns how long r's pacing (MinInterval after its last request:
// live, re-check or rescan) still runs, by the wall clock as the runner counts
// it (0: none). The lanes (this one and rescan.go) do not start a request
// before it has passed, so the worker never sleeps on their behalf. Only the
// scan worker's goroutine calls it (and the runner's requests run there).
func paceWait(r *toolRunner) time.Duration {
	if r.spec.MinInterval <= 0 || r.lastSent.IsZero() {
		return 0
	}
	return max(0, time.Until(r.lastSent.Add(r.spec.MinInterval)))
}

// logErr logs a database error of the lane, at most once every 10 minutes.
func (l *recheckLane) logErr(err error) {
	l.mu.Lock()
	ok := l.errLog.allow(l.now())
	l.mu.Unlock()
	if ok {
		log.Printf("perceptor re-check: %v (retried later)", err)
	}
}

// step looks at the lane once (the worker calls it when the live queue is
// empty) and runs at most one re-check, synchronously: due first re-checks
// before second ones, oldest call first. It reports whether it ran one.
func (l *recheckLane) step(ctx context.Context, queueLen func() int) bool {
	now := l.now()
	l.mu.Lock()
	if queueLen() > 0 || now.Before(l.pausedUntil) || now.Before(l.nextLook) {
		l.mu.Unlock()
		return false
	}
	l.nextLook = now.Add(recheckTick)
	// an owed call may be older than the horizon: read back to the oldest
	horizonStart := now.Add(-l.cfg.horizon())
	since := horizonStart
	for id, at := range l.owed {
		if now.Sub(at) > l.cfg.maxAge1()+recheckOwedMax {
			delete(l.owed, id)
			log.Printf("perceptor re-check 1: call %d is %s old — given up, not delivered", id, rescanAgeText(now.Sub(at)))
			continue
		}
		if at.Before(since) {
			since = at
		}
	}
	for id, sc := range l.scores {
		if now.Sub(sc.at) > l.cfg.maxAge1()+recheckOwedMax {
			delete(l.scores, id) // its delivery can no longer come
		}
	}
	l.mu.Unlock()

	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rows, err := l.db.SelectPerceptorRechecks(qctx, since)
	cancel()
	if err != nil {
		l.logErr(err)
		return false
	}
	var pending, first, second *PerceptorRecheckRow
	l.mu.Lock()
	// free reports whether key k of p may run now; a later retry time lowers
	// nextLook instead
	free := func(p *PerceptorRecheckRow, n int) bool {
		k := recheckKey(p.CallID, n)
		if l.tried[k] {
			return false
		}
		if at := l.retryAt[k]; now.Before(at) {
			if at.Before(l.nextLook) {
				l.nextLook = at
			}
			return false
		}
		return true
	}
	for i := range rows {
		p := &rows[i]
		_, owed := l.owed[p.CallID]
		if !owed && p.MessageDate.Before(horizonStart) {
			continue // read only for an owed call
		}
		pl := recheckDecide(l.cfg, now, *p, l.deliverable)
		switch {
		case pl.DeliverMissed && owed:
			pl.Step = recheckDeliver // released in this process: deliver it late rather than lose it
		case pl.DeliverMissed:
			l.noteMissedLocked(p, recheckDeliverN, now, "its stored re-check is not delivered")
		case pl.FirstMissed && owed:
			pl.Step = recheckFirst // released in this process: the last try, stored and delivered
		case pl.FirstMissed:
			l.noteMissedLocked(p, 1, now, "skipped, not delivered")
		case owed && (p.Delivered || (p.Recheck1 && pl.Step != recheckDeliver)):
			delete(l.owed, p.CallID) // done
		}
		if pl.SecondMissed {
			l.noteMissedLocked(p, 2, now, "skipped")
		}
		switch pl.Step {
		case recheckDeliver:
			if pending == nil && free(p, recheckDeliverN) {
				pending = p
			}
		case recheckFirst:
			if first == nil && free(p, 1) {
				first = p
			}
		case recheckSecond:
			if second == nil && free(p, 2) {
				second = p
			}
		case recheckWait:
			if pl.Due.Before(l.nextLook) {
				l.nextLook = pl.Due
			}
		}
	}
	p, n := pending, recheckDeliverN
	if p == nil && (first != nil || second != nil) {
		// Perceptor is asked only when its runner is free: no pacing sleep and
		// no rate-limit pause inside the worker, so a live call never waits
		// behind the lane. A stored re-check (pending) is delivered anyway.
		switch wait := paceWait(l.runner); {
		case wait > 0:
			first, second = nil, nil
			// look again when the pacing has passed, unless an earlier look
			// is already set (the normal tick, a retry or a due re-check)
			if at := now.Add(wait); at.Before(l.nextLook) {
				l.nextLook = at
			}
		case now.Before(l.percPausedUntil):
			first, second = nil, nil // the normal look: a stored re-check may need delivering
		}
	}
	if p == nil {
		p, n = first, 1
	}
	if p == nil {
		p, n = second, 2
	}
	if p != nil {
		l.tried[recheckKey(p.CallID, n)] = true
		delete(l.retryAt, recheckKey(p.CallID, n))
		l.nextLook = time.Time{} // look again right after this one
	}
	l.mu.Unlock()
	if p == nil {
		return false
	}
	if n == recheckDeliverN {
		l.deliverStored(ctx, *p)
		return true
	}
	l.recheck(ctx, *p, n)
	return true
}

// noteMissedLocked logs, once per process, that p's n-th re-check window
// passed without it. l.mu must be held.
func (l *recheckLane) noteMissedLocked(p *PerceptorRecheckRow, n int, now time.Time, what string) {
	k := recheckKey(p.CallID, n)
	if l.missed[k] {
		return
	}
	l.missed[k] = true
	log.Printf("perceptor re-check %d: call %d (%s) is %s old, past its window — %s",
		n, p.CallID, p.ContractAddress, rescanAgeText(now.Sub(p.MessageDate)), what)
}

// recheck runs Perceptor once more for p (the n-th re-check), stores the
// result and, for the first, decides the delivery. A stop, or a send that
// Telegram refused with FLOOD_WAIT, stores nothing and releases the re-check
// (FLOOD_WAIT pauses the lane; the re-check is tried again while its window
// lasts). A first re-check without a report (timeout, failed, rate_limited),
// or a rate_limited second one, also stores nothing and is tried again after
// recheckRetryPause, unless that would be past its window: then it is stored
// (the first is delivered with the label). Perceptor's rate-limit notice is
// not waited out in the runner (no retries: the worker would sleep); it
// pauses asking Perceptor for recheckRetryPause instead.
func (l *recheckLane) recheck(ctx context.Context, p PerceptorRecheckRow, n int) {
	r := l.runner
	send := func(ctx context.Context, text string) error { return l.send(ctx, r, text) }
	res := r.investigateWithRetryN(ctx, send, p.ContractAddress, 0)
	now := l.now()
	asked := res.RequestedAt
	if asked.IsZero() {
		asked = now
	}
	age := asked.Sub(p.MessageDate)
	what := fmt.Sprintf("perceptor re-check %d: call %d (%s, %s after the call)", n, p.CallID, p.ContractAddress, rescanAgeText(age))
	if ctx.Err() != nil {
		l.retryLater(p, n, time.Time{})
		log.Printf("%s: stopped before the report came (not recorded; run again later)", what)
		return
	}
	if d, ok := tgerr.AsFloodWait(res.Err); ok {
		l.mu.Lock()
		if until := now.Add(d); until.After(l.pausedUntil) {
			l.pausedUntil = until
		}
		l.mu.Unlock()
		l.retryLater(p, n, time.Time{}) // after the pause
		log.Printf("%s: Telegram FLOOD_WAIT %s — not recorded; re-check paused for %s", what, d, d)
		return
	}
	if res.Status == investigationRateLimited {
		l.mu.Lock()
		if retry := now.Add(recheckRetryPause); retry.After(l.percPausedUntil) {
			l.percPausedUntil = retry // Perceptor only: stored re-checks are still delivered
		}
		l.mu.Unlock()
	}
	if res.Status != investigationCompleted && (n == 1 || res.Status == investigationRateLimited) {
		end := l.cfg.maxAge1()
		if n == 2 {
			end = l.cfg.maxAge2()
		}
		if retry := now.Add(recheckRetryPause); retry.Before(p.MessageDate.Add(end)) {
			l.retryLater(p, n, retry)
			log.Printf("%s: %s (%v) — not recorded; tried again in %s", what, res.Status, res.Err, recheckRetryPause)
			return
		}
	}
	if res.Status == investigationCompleted {
		res.Verdict = l.judge(ctx, r.spec, res)
	} else {
		res.Verdict = verdict{Level: levelUnknown}
	}
	if ctx.Err() != nil {
		l.retryLater(p, n, time.Time{})
		log.Printf("%s: stopped while reading the report (not recorded; run again later)", what)
		return
	}
	if n == 1 {
		l.mu.Lock()
		delete(l.owed, p.CallID) // stored from here on (a stopped delivery owes it again)
		l.mu.Unlock()
	}
	row := investigationRow(res, &p.CallID, p.ContractAddress, l.toolID)
	if n == 2 {
		row.ScanKind = ScanKindRescan // "Perceptor today": never the call-time verdict
	}
	row.Details = recheckDetails(row.Details, n, age)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	invID, err := l.db.InsertScoutInvestigation(wctx, row)
	cancel()
	outcome := res.Verdict.Level
	if res.Status != investigationCompleted {
		outcome = res.Status
		if res.Err != nil {
			outcome += " (" + res.Err.Error() + ")"
		}
	}
	if err != nil {
		outcome += fmt.Sprintf(" [storing it failed: %v]", err)
		invID = nil
	}
	if n == 2 {
		log.Printf("%s: %s — website only", what, outcome)
		return
	}
	cfg := l.s.cfg
	completed := res.Status == investigationCompleted
	if completed && verdictUsable(res.Verdict.Level) && !cfg.DeliverLevels[res.Verdict.Level] {
		log.Printf("%s: %s — not delivered (not in SCOUT_DELIVER_LEVELS)", what, outcome)
		l.dropScore(p.CallID)
		l.logScan(p, res, false, false, "")
		return
	}
	l.deliverCall(ctx, p, res, invID, age, n, what, outcome)
}

// deliverStored delivers the stored first re-check of p (p.Recheck1ID) whose
// delivery was stopped before anything was sent; Perceptor is not asked again.
func (l *recheckLane) deliverStored(ctx context.Context, p PerceptorRecheckRow) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	row, err := l.db.GetScoutInvestigation(qctx, *p.Recheck1ID)
	cancel()
	if err == nil && row == nil {
		err = fmt.Errorf("investigation %d not found", *p.Recheck1ID)
	}
	if err != nil {
		l.retryLater(p, recheckDeliverN, l.now().Add(recheckTick))
		l.logErr(fmt.Errorf("reading the stored re-check of call %d: %w", p.CallID, err))
		return
	}
	res := storedToolResult(l.runner.spec, *row)
	age := row.RequestedAt.Sub(p.MessageDate)
	what := fmt.Sprintf("perceptor re-check 1: call %d (%s, %s after the call, stored)", p.CallID, p.ContractAddress, rescanAgeText(age))
	outcome := res.Verdict.Level
	if res.Status != investigationCompleted {
		outcome = res.Status
	}
	l.deliverCall(ctx, p, res, row.ID, age, recheckDeliverN, what, outcome)
}

// deliverCall delivers p after its first re-check res (stored as invID; nil
// when storing failed): the header as for a live call plus a line with the
// re-check's age (second) and, last, the model score line computed at the
// call (recheckLane.scores, else the one stored when the call was held,
// p.ScoreLine; never asked again, and left out when there is none), the
// original call post, the re-check's report
// and the other tools' reports of the call's live scan, read from the
// database. n is the lane key's n that ran it (1 or recheckDeliverN).
//
// The delivery is recorded in scout_deliveries (status sending) BEFORE it is
// sent, and that row is updated to sent or failed after: any delivery row of
// the call (sending included) marks it delivered for good, so a failed update
// can never cause a second send, in this process or after a restart. When
// recording it fails nothing is sent and the delivery is tried again later.
// When the stop came before anything was sent (ctx is done and the error is a
// deliveryNotSentError: ctx was done before the first message, so nothing
// reached Telegram) the record is removed and the keys released, so the
// stored re-check is delivered on a later step. Any other failure, a stop in
// the middle of a send included, is recorded as failed, as for a live call:
// part of the delivery may have been sent, and sending it again would send
// that part twice.
func (l *recheckLane) deliverCall(ctx context.Context, p PerceptorRecheckRow, res *toolResult, invID *int, age time.Duration, n int, what, outcome string) {
	j := job{CA: p.ContractAddress, SourceMsg: p.MessageID, SourceText: p.MessageText, Meta: metaOf(p.MessageText), CallID: &p.CallID}
	results, ids := l.callResults(ctx, p.CallID, res, invID)
	header := withRecheckLine(l.s.deliveryHeader(j, results), fmt.Sprintf("🔁 %s re-checked %s after the call", res.Spec.Name, rescanAgeText(age)))
	// the line in memory, else the one stored when the call was held (after a restart)
	if line := firstNonEmpty(l.scoreLine(p.CallID), p.ScoreLine); line != "" {
		header += "\n" + line // as process() adds it at the call
	}
	// as for a live call: an sAlpha decline is not forwarded, so not attached
	var attached []int
	for i, r := range results {
		if ids[i] != nil && forwardable(r) {
			attached = append(attached, *ids[i])
		}
	}
	d := &ScoutDelivery{CallID: j.CallID, ContractAddress: j.CA, Target: deliveryLabel(l.s.cfg),
		Status: DeliveryStatusSending, HeaderText: header, InvestigationIDs: attached}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	delID, err := l.db.InsertScoutDelivery(wctx, d)
	cancel()
	if err != nil {
		l.retryLater(p, n, l.now().Add(recheckTick))
		log.Printf("%s: %s — recording the delivery failed, so nothing was sent (tried again later): %v", what, outcome, err)
		return
	}
	// recorded from here on: never delivered again, by this process or a later one
	l.mu.Lock()
	l.tried[recheckKey(p.CallID, recheckDeliverN)] = true
	delete(l.owed, p.CallID)
	l.mu.Unlock()
	msgID, err := l.deliver(ctx, j, header, results)
	var notSent *deliveryNotSentError
	if err != nil && ctx.Err() != nil && errors.As(err, &notSent) {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		derr := l.db.DeleteSendingDelivery(dctx, *delID)
		cancel()
		if derr != nil {
			log.Printf("%s: %s — stopped before the delivery was sent, but its record stays (%v): not delivered", what, outcome, derr)
			return
		}
		l.retryLater(p, recheckDeliverN, time.Time{})
		if n != recheckDeliverN {
			l.retryLater(p, n, time.Time{})
		}
		log.Printf("%s: %s — stopped before the delivery was sent (not recorded; delivered later): %v", what, outcome, err)
		return
	}
	l.dropScore(p.CallID) // the delivery has an outcome now: never sent again
	status, errText := DeliveryStatusSent, (*string)(nil)
	var at *time.Time
	if err != nil {
		status, errText = DeliveryStatusFailed, strPtr(err.Error())
	} else {
		t := time.Now().UTC()
		at = &t
	}
	// the header's message id, also after a later part failed: a sAlpha
	// re-ask follow-up replies to it (salpha_reask.go)
	if ferr := l.finishDelivery(ctx, *delID, status, at, errText, notifyMsgIDPtr(msgID)); ferr != nil {
		log.Printf("%s: recording the delivery's outcome (%s) failed: %v — it stays recorded as sending (never sent again)", what, status, ferr)
	}
	if err != nil {
		log.Printf("%s: %s — delivery failed: %v", what, outcome, err)
		l.logScan(p, res, true, false, "deliver: "+err.Error())
		return
	}
	log.Printf("%s: %s — delivered (%d report(s))", what, outcome, len(attached))
	l.logScan(p, res, true, true, "")
}

// callResults returns the results to deliver for a call, in SCOUT_TOOLS order,
// with their investigation ids: res (and invID) for Perceptor, and for every
// other tool the newest live investigation of the call stored by its live
// scan (a tool without one is left out).
func (l *recheckLane) callResults(ctx context.Context, callID int, res *toolResult, invID *int) ([]*toolResult, []*int) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	rows, err := l.db.SelectScoutInvestigations(qctx, ScoutInvestigationFilter{CallID: &callID, Limit: 200})
	cancel()
	if err != nil {
		log.Printf("perceptor re-check: reading the other reports of call %d: %v (delivering Perceptor's only)", callID, err)
	}
	var results []*toolResult
	var ids []*int
	for _, r := range l.s.runners {
		if r == l.runner {
			results, ids = append(results, res), append(ids, invID)
			continue
		}
		for _, row := range rows { // newest first
			if row.ToolCode == r.spec.Code && row.ScanKind == ScanKindLive {
				results, ids = append(results, storedToolResult(r.spec, row)), append(ids, row.ID)
				break
			}
		}
	}
	return results, ids
}

// storedToolResult rebuilds a tool's result from its stored investigation, as
// far as a delivery needs it: the bot's message ids (forwarded), the report
// text (on the last message: the text copy when forwarding fails; also kept
// as StoredText, so a row without message ids still reads as its text, e.g.
// an sAlpha decline is not forwarded or attached) and the verdict.
func storedToolResult(spec ToolSpec, row ScoutInvestigation) *toolResult {
	res := &toolResult{Spec: spec, Command: row.RequestText, RequestedAt: row.RequestedAt, Status: row.Status,
		StoredText: row.ReportText}
	if row.CompletedAt != nil {
		res.CompletedAt = *row.CompletedAt
	}
	for _, id := range row.BotMessageIDs {
		res.Replies = append(res.Replies, &tg.Message{ID: int(id)})
	}
	if n := len(res.Replies); n > 0 {
		res.Replies[n-1].Message = row.ReportText
		res.Best = res.Replies[n-1]
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	res.Verdict = verdict{Level: row.VerdictLevel, Label: str(row.VerdictLabel), Ticker: str(row.Ticker),
		Summary: str(row.VerdictSummary), URL: str(row.ReportURL), Source: str(row.VerdictSource)}
	if row.Error != nil {
		res.Err = errors.New(*row.Error)
	}
	return res
}

// withRecheckLine puts line under the first line of header.
func withRecheckLine(header, line string) string {
	first, rest, ok := strings.Cut(header, "\n")
	if !ok {
		return header + "\n" + line
	}
	return first + "\n" + line + "\n" + rest
}

// logScan appends the re-check to scans.jsonl, like a live scan.
func (l *recheckLane) logScan(p PerceptorRecheckRow, res *toolResult, deliver, delivered bool, errText string) {
	tr := toolRecord{Tool: res.Spec.Code, Status: res.Status, Verdict: res.Verdict, ReportText: res.ReportText()}
	if res.Err != nil {
		tr.Error = res.Err.Error()
	}
	appendLog(l.s.logf, scanRecord{Time: l.now(), CA: p.ContractAddress, SourceMsg: p.MessageID, CallMeta: metaOf(p.MessageText),
		Tools: []toolRecord{tr}, Deliver: deliver, Delivered: delivered, Error: errText})
}

// recheckDetails adds {"recheck": n, "call_age_s": …} to the details JSON of
// an investigation (replyDetails' object), keeping its keys.
func recheckDetails(details []byte, n int, age time.Duration) []byte {
	m := map[string]any{}
	if len(details) > 0 {
		if err := json.Unmarshal(details, &m); err != nil {
			m = map[string]any{} // replyDetails always writes an object; start afresh otherwise
		}
	}
	m["recheck"] = n
	m["call_age_s"] = int64(age / time.Second)
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(fmt.Sprintf(`{"recheck":%d}`, n)) // cannot happen: the map holds JSON values only
	}
	return b
}
