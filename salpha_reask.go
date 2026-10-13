package main

// sAlpha re-ask lane: when a first call's live sAlpha reply was no report (it
// declined, came back empty, timed out or failed), the listener asks sAlpha
// once more, 6 to 24 hours after the call (SCOUT_SALPHA_REASK_MIN_AGE and
// _MAX_AGE), once per token. It runs inside the listener (one Telegram
// session; never a second process) when the live queue is idle. Off unless
// SCOUT_SALPHA_REASK=on.
//
// A re-ask writes one scout_investigations row with scan_kind 'reask' and
// nothing else of the call: no scout_calls status change, no seen_cas.json
// entry, no model score, no tracking change. It is never a call-time feature:
// the training dataset (scout_call_dataset_v) reads no sAlpha report, and
// every other reader of scout_investigations compares scan_kind with =
// ('live' or 'rescan'), so 'reask' rows are invisible to the Perceptor
// readers, the live re-check, the requeue and the rescan lane's counts. Only
// the website's sAlpha lookup (webSAlphaSQL) and this lane read them.
//
// When the re-ask brought a real report (completed, visible text, not a
// decline) and the call was delivered, the report goes to the delivery chat
// as a follow-up: a text reply to the delivery's header (scout_deliveries.
// notify_message_id; without a reply when that is unknown), then the report
// messages, forwarded (or a text copy). The follow-up is another
// scout_deliveries row linking the re-ask investigation. Calls that were not
// delivered (red flags, -dry-run) get no follow-up; the website shows the
// report either way ("sAlpha later").
//
// The scan worker handles the live queue first, then the live Perceptor
// re-check (recheck.go), then the rescan lane, then this lane, each at most
// one request per look (rescanDecide's rules: live queue empty, idle
// reaskIdle, the gap and the daily cap from the database; and not while
// sAlpha's pacing still runs, paceWait). A live call that arrives meanwhile
// waits for that one request: at most sAlpha's MaxWait (300 s) plus the
// follow-up.
//
// The follow-up replies to the call's first delivery with status sent: a
// live delivery, or a live Perceptor re-check delivery (recheck.go), both of
// which store their header's notify_message_id. A re-check delivery still at
// status sending (its outcome was never stored) does not count.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tgerr"
)

const (
	reaskMinAgeDefault    = 6 * time.Hour
	reaskMaxAgeDefault    = 24 * time.Hour
	reaskMaxPerDayDefault = 60
	reaskGapDefault       = 10 * time.Minute

	// reaskIdle: the lane runs only after the live queue has been idle this
	// long (the rescan lane's default idle time).
	reaskIdle = rescanIdleDefault
	// reaskTool: the only tool the lane asks.
	reaskTool = webToolSAlpha
)

// reaskConfig holds the SCOUT_SALPHA_REASK_* settings.
type reaskConfig struct {
	Enabled   bool          // SCOUT_SALPHA_REASK=on
	MinAge    time.Duration // SCOUT_SALPHA_REASK_MIN_AGE: ask again once the call is this old
	MaxAge    time.Duration // SCOUT_SALPHA_REASK_MAX_AGE: and not when it is older
	MaxPerDay int           // SCOUT_SALPHA_REASK_MAX_PER_DAY: re-asks in a rolling 24 hours
	Gap       time.Duration // SCOUT_SALPHA_REASK_GAP: between two re-asks
}

// loadReaskConfig reads the SCOUT_SALPHA_REASK_* settings; an invalid value is
// an error (the listener does not start).
func loadReaskConfig() (reaskConfig, error) {
	c := reaskConfig{MinAge: reaskMinAgeDefault, MaxAge: reaskMaxAgeDefault, MaxPerDay: reaskMaxPerDayDefault, Gap: reaskGapDefault}
	switch v := strings.ToLower(env("SCOUT_SALPHA_REASK", "off")); v {
	case "on":
		c.Enabled = true
	case "off":
	default:
		return c, fmt.Errorf("SCOUT_SALPHA_REASK=%q: want on or off", v)
	}
	dur := func(key string, def, min time.Duration) (time.Duration, error) {
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
	if c.MinAge, err = dur("SCOUT_SALPHA_REASK_MIN_AGE", c.MinAge, time.Hour); err != nil {
		return c, err
	}
	if c.MaxAge, err = dur("SCOUT_SALPHA_REASK_MAX_AGE", c.MaxAge, time.Hour); err != nil {
		return c, err
	}
	if c.MaxAge <= c.MinAge {
		return c, fmt.Errorf("SCOUT_SALPHA_REASK_MAX_AGE (%s) must be longer than SCOUT_SALPHA_REASK_MIN_AGE (%s)", c.MaxAge, c.MinAge)
	}
	if c.Gap, err = dur("SCOUT_SALPHA_REASK_GAP", c.Gap, 0); err != nil {
		return c, err
	}
	if v := env("SCOUT_SALPHA_REASK_MAX_PER_DAY", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return c, fmt.Errorf("SCOUT_SALPHA_REASK_MAX_PER_DAY=%q: want a number from 1 to 10000", v)
		}
		c.MaxPerDay = n
	}
	return c, nil
}

// decideConfig is the part of the settings rescanDecide reads.
func (c reaskConfig) decideConfig() rescanConfig {
	return rescanConfig{MaxPerDay: c.MaxPerDay, Gap: c.Gap, Idle: reaskIdle}
}

// perDay is how many re-asks a day the settings allow at most.
func (c reaskConfig) perDay() int {
	return c.decideConfig().perDay()
}

// describe is the settings in one line, for the logs.
func (c reaskConfig) describe() string {
	return fmt.Sprintf("sAlpha asked again about first calls whose live reply was no report (declined, empty, timeout, failed), %s to %s after the call, once per token: at most %d a day, %s apart, after the live queue has been idle %s",
		hoursText(c.MinAge), hoursText(c.MaxAge), c.MaxPerDay, c.Gap, reaskIdle)
}

// hoursText writes a duration in hours, "6 h" or "1.5 h".
func hoursText(d time.Duration) string {
	return strconv.FormatFloat(math.Round(d.Hours()*10)/10, 'f', -1, 64) + " h"
}

// ---------------------------------------------------------------------------
// Live delivery: declines and the header
// ---------------------------------------------------------------------------

// salphaDeclineLabels: how the delivery header names a decline, by the phrase
// of salphaDeclinePhrases it contains.
var salphaDeclineLabels = map[string]string{
	"not enough public":    "not enough public signals",
	"too little liquidity": "too little liquidity or trading",
}

// salphaDeclineReason names the decline in text for the delivery header.
func salphaDeclineReason(text string) string {
	t := strings.ToLower(text)
	for _, p := range salphaDeclinePhrases {
		if strings.Contains(t, p) {
			if l, ok := salphaDeclineLabels[p]; ok {
				return l
			}
			return p
		}
	}
	return "no reason given"
}

// reaskNote is what the delivery header adds to an sAlpha line without a
// report when the re-ask lane is on. It only says "may": whether the lane
// takes the call (the token's first call, not asked again yet, no real report
// from another call, not rugged or wiped by then) needs database reads the
// delivery does not make, and can change before the lane gets to it.
func (s *scanner) reaskNote(j job, r *toolResult) string {
	if s.reask == nil || j.CallID == nil || r.Spec.Code != reaskTool {
		return ""
	}
	return " — may be asked again later"
}

// salphaRealReport reports whether an sAlpha result is a report: it
// completed, has visible text and does not only decline.
func salphaRealReport(r *toolResult) bool {
	text := r.ReportText()
	return r.Status == investigationCompleted && hasVisibleText(text) && !salphaDeclined(text)
}

// ---------------------------------------------------------------------------
// The lane
// ---------------------------------------------------------------------------

// reaskFollowUp is what the follow-up of a re-ask sends: Header as a reply to
// message ReplyTo of the delivery chat (0 = no reply), then Result's report.
type reaskFollowUp struct {
	Header  string
	ReplyTo int64
	Result  *toolResult
}

// reaskLane is the lane's state. It lives on the scanner (not in the worker),
// so a reconnect, which starts a new worker, keeps a pause. The fields after
// mu are guarded by it.
type reaskLane struct {
	cfg    reaskConfig
	runner *toolRunner
	toolID int
	now    func() time.Time
	// target is deliveryLabel of the settings: a stored message id is
	// replied to only when its delivery went to the same target.
	target string
	// noFollowUp: the listener runs with -dry-run (nothing is delivered).
	noFollowUp bool
	// send sends text to the runner's bot; judge reads the verdict of a
	// completed report; followUp sends a follow-up to the delivery chat and
	// returns the id of its first message (the scanner's own by default;
	// tests use fakes).
	send     func(ctx context.Context, r *toolRunner, text string) error
	judge    func(ctx context.Context, spec ToolSpec, res *toolResult) verdict
	followUp func(ctx context.Context, f reaskFollowUp) (int64, error)

	mu               sync.Mutex
	lastLive         time.Time
	pausedUntil      time.Time
	noCandidateUntil time.Time
	// lastReask: when this lane last asked (zero = not since start), a floor
	// for the database's newest requested_at, so the gap holds even when
	// storing the row failed.
	lastReask  time.Time
	lastReason string // the last "not now" reason logged, to log each once
	errLog     logLimiter
}

// newReaskLane returns the lane, or nil when it is off or cannot run (logged
// once): SCOUT_SALPHA_REASK=off, no database, no sAlpha tool or its id unknown.
func (s *scanner) newReaskLane(c reaskConfig) *reaskLane {
	if !c.Enabled {
		return nil
	}
	r := s.runnerFor(reaskTool)
	switch {
	case s.db == nil:
		log.Printf("salpha reask: SCOUT_SALPHA_REASK=on but the database is off — the re-ask lane stays off")
		return nil
	case r == nil:
		log.Printf("salpha reask: SCOUT_SALPHA_REASK=on but %q is not in SCOUT_TOOLS — the re-ask lane stays off", reaskTool)
		return nil
	case r.toolID == nil:
		log.Printf("salpha reask: SCOUT_SALPHA_REASK=on but the %s tool is not registered in the database — the re-ask lane stays off", reaskTool)
		return nil
	}
	l := &reaskLane{cfg: c, runner: r, toolID: *r.toolID, now: time.Now, target: deliveryLabel(s.cfg), noFollowUp: s.cfg.DryRun,
		send: func(ctx context.Context, r *toolRunner, text string) error {
			_, err := s.sender.To(r.botPeer).Text(ctx, text)
			return err
		},
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			return s.judge(ctx, spec, res.Replies)
		},
		followUp: s.sendReaskFollowUp,
		errLog:   logLimiter{every: 10 * time.Minute},
	}
	follow := "real reports of delivered calls go to " + l.target + " as a reply to the delivery"
	if l.noFollowUp {
		follow = "no follow-up is sent (-dry-run)"
	}
	log.Printf("salpha reask: on — %s; %s", c.describe(), follow)
	return l
}

// noteLive records that a live call finished (or a worker started) at t.
func (l *reaskLane) noteLive(t time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastLive = t
}

// inputs returns the lane's own part of rescanInputs.
func (l *reaskLane) inputs(now time.Time, queueLen int) rescanInputs {
	l.mu.Lock()
	defer l.mu.Unlock()
	return rescanInputs{Now: now, QueueLen: queueLen, LastLive: l.lastLive,
		PausedUntil: l.pausedUntil, NoCandidateUntil: l.noCandidateUntil}
}

// logOnce logs line (when not empty) if reason differs from the last one
// noted this way, so a waiting state is logged once, not on every tick.
func (l *reaskLane) logOnce(reason, line string) {
	l.mu.Lock()
	same := l.lastReason == reason
	l.lastReason = reason
	l.mu.Unlock()
	if !same && line != "" {
		log.Print(line)
	}
}

// logErr logs a database error of the lane, at most once every 10 minutes.
func (l *reaskLane) logErr(err error) {
	l.mu.Lock()
	ok := l.errLog.allow(l.now())
	l.mu.Unlock()
	if ok {
		log.Printf("salpha reask: %v (retried later)", err)
	}
}

func (l *reaskLane) pause(now time.Time, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := now.Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

func (l *reaskLane) pausedAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pausedUntil
}

func (l *reaskLane) lastReaskAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastReask
}

// reaskStore is the part of ScoutStore the lane uses.
type reaskStore interface {
	ReaskStats(ctx context.Context, since time.Time) (int, *time.Time, error)
	NextReaskCandidate(ctx context.Context, from, to time.Time) (*ReaskCandidate, error)
	InsertScoutInvestigation(ctx context.Context, r *ScoutInvestigation) (*int, error)
	FirstSentDelivery(ctx context.Context, callID int) (*ScoutDelivery, error)
	InsertScoutDelivery(ctx context.Context, d *ScoutDelivery) (*int, error)
}

// step looks at the lane once (the worker calls it when the live queue is
// empty and the rescan lane did nothing) and asks sAlpha again about at most
// one call, synchronously. It reports whether it asked (then the worker looks
// at the live queue again at once).
func (l *reaskLane) step(ctx context.Context, st reaskStore, queueLen func() int) bool {
	if paceWait(l.runner) > 0 {
		// sAlpha's pacing (MinInterval after its last request) still runs:
		// never sleep in the worker, look again later (as rescan.go)
		return false
	}
	now := l.now()
	in := l.inputs(now, queueLen())
	dc := l.cfg.decideConfig()
	reason := rescanDecide(dc, in)
	if reason == rescanNeedStats {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		n, last, err := st.ReaskStats(qctx, now.Add(-rescanWindow))
		cancel()
		if err != nil {
			l.logErr(err)
			return false
		}
		in.StatsKnown, in.Today, in.LastRescan = true, n, last
		if mine := l.lastReaskAt(); !mine.IsZero() && (last == nil || mine.After(*last)) {
			in.LastRescan = &mine
		}
		reason = rescanDecide(dc, in)
	}
	if reason == rescanCapReached {
		l.logOnce(reason, fmt.Sprintf("salpha reask: %d of %d done in the last 24 hours — waiting", in.Today, l.cfg.MaxPerDay))
	}
	if reason != rescanGo {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := st.NextReaskCandidate(qctx, now.Add(-l.cfg.MaxAge), now.Add(-l.cfg.MinAge))
	cancel()
	if err != nil {
		l.logErr(err)
		return false
	}
	if c == nil {
		l.mu.Lock()
		l.noCandidateUntil = now.Add(rescanNoCandidateWait)
		l.mu.Unlock()
		l.logOnce(rescanNoCandidates, "salpha reask: no call to ask about again — looking again every "+rescanNoCandidateWait.String())
		return false
	}
	l.logOnce(rescanGo, "")
	l.reask(ctx, st, *c, in.Today+1)
	return true
}

// reask asks sAlpha once more about c (no rate-limit retry) and stores the
// result as a scan_kind 'reask' investigation of c's call; with a real report
// of a delivered call it sends the follow-up. Nothing else is written. A
// request cut off by a stop, and a send that Telegram refused with FLOOD_WAIT,
// store nothing; a FLOOD_WAIT or a rate-limit reply pauses the lane (so does a
// FLOOD_WAIT or a Bot API 429 on the follow-up). nth is
// this re-ask's number in the last 24 hours, for the log.
func (l *reaskLane) reask(ctx context.Context, st reaskStore, c ReaskCandidate, nth int) {
	r := l.runner
	l.mu.Lock()
	l.lastReask = l.now() // the gap counts from the request, like requested_at
	l.mu.Unlock()
	send := func(ctx context.Context, text string) error { return l.send(ctx, r, text) }
	asked := l.now()
	res := r.investigateWithRetryN(ctx, send, c.ContractAddress, 0)
	now := l.now()
	age := asked.Sub(c.MessageDate)
	what := fmt.Sprintf("salpha reask: call %d (%s, asked %s after the call; the call's reply: %s)",
		c.CallID, c.ContractAddress, rescanAgeText(age), c.Reason)
	if ctx.Err() != nil {
		log.Printf("%s: stopped before the reply came (not recorded)", what)
		return
	}
	if d, ok := tgerr.AsFloodWait(res.Err); ok {
		l.pause(now, max(rescanPause, d))
		log.Printf("%s: Telegram FLOOD_WAIT %s — not recorded; re-ask lane paused until %s", what, d, l.pausedAt().Format(time.RFC3339))
		return
	}
	if res.Status == investigationCompleted {
		res.Verdict = l.judge(ctx, r.spec, res)
	} else {
		res.Verdict = verdict{Level: levelUnknown}
	}
	if ctx.Err() != nil {
		log.Printf("%s: stopped while reading the reply (not recorded)", what)
		return
	}
	row := investigationRow(res, &c.CallID, c.ContractAddress, l.toolID)
	row.ScanKind = ScanKindReask
	row.Details = reaskDetails(row.Details, age, c.Reason)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	invID, err := st.InsertScoutInvestigation(wctx, row)
	cancel()
	outcome := "a report"
	switch {
	case res.Status != investigationCompleted:
		outcome = res.Status
		if res.Err != nil {
			outcome += " — " + res.Err.Error()
		}
	case reportDeclined(res):
		outcome = "declined again"
	case !hasVisibleText(res.ReportText()):
		outcome = "an empty reply again"
	}
	if err != nil {
		log.Printf("%s: %s, but storing it failed: %v", what, outcome, err)
	} else {
		log.Printf("%s: %s (%d of %d today)", what, outcome, nth, l.cfg.MaxPerDay)
	}
	if res.Status == investigationRateLimited {
		l.pause(now, rescanPause)
		log.Printf("salpha reask: @%s is rate-limiting — re-ask lane paused until %s", r.spec.Bot, l.pausedAt().Format(time.RFC3339))
	}
	if err != nil || invID == nil || !salphaRealReport(res) {
		return
	}
	l.deliverFollowUp(ctx, st, c, res, *invID, age)
}

// deliverFollowUp sends a re-ask's real report to the delivery chat when the
// call was delivered, as a reply to the delivery's header, and records it as
// a delivery linking the re-ask investigation invID.
func (l *reaskLane) deliverFollowUp(ctx context.Context, st reaskStore, c ReaskCandidate, res *toolResult, invID int, age time.Duration) {
	what := fmt.Sprintf("salpha reask: call %d", c.CallID)
	if l.noFollowUp {
		log.Printf("%s: -dry-run — follow-up not sent (the report shows on the website)", what)
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	d, err := st.FirstSentDelivery(qctx, c.CallID)
	cancel()
	if err != nil {
		log.Printf("%s: follow-up not sent: %v", what, err)
		return
	}
	if d == nil {
		log.Printf("%s: the call was not delivered — no follow-up (the report shows on the website)", what)
		return
	}
	f := reaskFollowUp{Header: reaskFollowUpHeader(c, age), Result: res}
	if d.NotifyMessageID != nil && d.Target == l.target {
		f.ReplyTo = *d.NotifyMessageID
	}
	msgID, sendErr := l.followUp(ctx, f)
	row := &ScoutDelivery{CallID: &c.CallID, ContractAddress: c.ContractAddress, Target: l.target,
		Status: DeliveryStatusSent, HeaderText: f.Header, InvestigationIDs: []int{invID}, NotifyMessageID: notifyMsgIDPtr(msgID)}
	paused := false
	if sendErr != nil {
		row.Status, row.Error = DeliveryStatusFailed, strPtr(sendErr.Error())
		if fw, ok := sendFloodWait(sendErr); ok {
			l.pause(l.now(), max(rescanPause, fw))
			paused = true
		}
	} else {
		at := l.now().UTC()
		row.DeliveredAt = &at
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	_, err = st.InsertScoutDelivery(wctx, row)
	cancel()
	switch {
	case paused:
		log.Printf("%s: follow-up failed: %v — re-ask lane paused until %s", what, sendErr, l.pausedAt().Format(time.RFC3339))
	case sendErr != nil:
		log.Printf("%s: follow-up failed: %v", what, sendErr)
	case f.ReplyTo != 0:
		log.Printf("%s: follow-up sent as a reply to message %d", what, f.ReplyTo)
	default:
		log.Printf("%s: follow-up sent (no stored delivery message to reply to)", what)
	}
	if err != nil {
		log.Printf("%s: storing the follow-up delivery failed: %v", what, err)
	}
}

// sendFloodWait reports whether a send error is Telegram asking to slow down —
// a FLOOD_WAIT (user account) or a Bot API 429 — and how long it asks to wait
// (0 when a 429 does not say).
func sendFloodWait(err error) (time.Duration, bool) {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return d, true
	}
	return botAPIRetryAfter(err)
}

// reaskFollowUpHeader is the first message of a follow-up:
//
//	🕐 sAlpha report (asked again 7h after the call) — $TICKER
//	CA: 0x…
//	Source: https://t.me/scoutrobinhood/123
func reaskFollowUpHeader(c ReaskCandidate, age time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🕐 sAlpha report (asked again %s after the call)", rescanAgeText(age))
	if c.Symbol != nil {
		if sym := strings.TrimSpace(*c.Symbol); sym != "" {
			if !strings.HasPrefix(sym, "$") {
				sym = "$" + sym
			}
			b.WriteString(" — " + sym)
		}
	}
	b.WriteString("\nCA: " + c.ContractAddress)
	if u := postURL(c.ChannelUsername, c.MessageID); u != nil {
		b.WriteString("\nSource: " + *u)
	}
	return b.String()
}

// sendReaskFollowUp sends a follow-up to the delivery chat: with the Bot API
// one text (header and report) as a reply; with the user account the header
// as a reply (sent again without it only when Telegram refused the reply
// itself, e.g. the message was deleted: reaskReplyRefused), then the report
// messages forwarded (or a text copy). It returns the id of the header
// message.
func (s *scanner) sendReaskFollowUp(ctx context.Context, f reaskFollowUp) (int64, error) {
	if s.cfg.NotifyBotToken != "" {
		text := f.Header + "\n\n━━ " + f.Result.Spec.Name + " ━━\n" + f.Result.ReportText()
		return botAPISend(ctx, s.cfg.NotifyBotToken, s.cfg.NotifyChatID, text, f.ReplyTo)
	}
	id, err := s.sendNotify(ctx, f.Header, int(f.ReplyTo))
	if f.ReplyTo != 0 && reaskReplyRefused(ctx, err) {
		log.Printf("salpha reask: the reply to message %d was refused (%v) — sending without it", f.ReplyTo, err)
		id, err = s.sendNotify(ctx, f.Header, 0)
	}
	if err != nil {
		return 0, fmt.Errorf("send follow-up header: %w", err)
	}
	return int64(id), s.forwardReport(ctx, f.Result)
}

// reaskReplyRefused reports whether a follow-up header sent as a reply may be
// sent again without the reply: only when Telegram refused the message
// replied to (REPLY_MESSAGE_ID_INVALID, MESSAGE_ID_INVALID), so the first
// send created nothing, and never after a stop (ctx done). Any other error
// (FLOOD_WAIT, a network error, a timeout) may come after the header reached
// Telegram, and a second send would post it twice.
func reaskReplyRefused(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && tgerr.Is(err, "REPLY_MESSAGE_ID_INVALID", "MESSAGE_ID_INVALID")
}

// reaskDetails adds {"reask": true, "call_age_s": …, "call_reply": …} to the
// details JSON of an investigation (replyDetails' object), keeping its keys.
// call_reply is why the call's own reply was no report (ReaskCandidate.Reason).
func reaskDetails(details []byte, age time.Duration, callReply string) []byte {
	m := map[string]any{}
	if len(details) > 0 {
		if err := json.Unmarshal(details, &m); err != nil {
			m = map[string]any{} // replyDetails always writes an object; start afresh otherwise
		}
	}
	m["reask"] = true
	m["call_age_s"] = int64(age / time.Second)
	m["call_reply"] = callReply
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{"reask":true}`) // cannot happen: the map holds JSON values only
	}
	return b
}

// ---------------------------------------------------------------------------
// -salpha-reask-missing -dry-run
// ---------------------------------------------------------------------------

// reaskSummary is what the dry run prints.
type reaskSummary struct {
	Pool      int // first calls whose latest live sAlpha reply was no report
	Reasked   int // of those: the token was asked again already
	HasReport int // … the token has a real report (another call's)
	Rugged    int // … rugged
	Wiped     int // … latest return <= rescanWipedPct
	Young     int // … younger than MinAge: asked later
	Old       [3]int
	// Old: older than MaxAge (the lane skips them), by age: up to 7 days, 7 to
	// 30 days, older; a larger SCOUT_SALPHA_REASK_MAX_AGE would take them.
	Candidates int            // the rest: in the window now
	ByReason   map[string]int // the call's reply, of every call left after the exclusions (any age)
	// List: the candidates in the order the lane takes them (oldest first);
	// Now: the time their ages are counted from.
	List []ReaskCandidate
	Now  time.Time
}

// reaskOldBuckets names reaskSummary.Old.
var reaskOldBuckets = [3]string{"up to 7d", "7-30d", ">30d"}

// reaskReasons: the order the dry run lists the reasons in.
var reaskReasons = []string{reaskReasonDeclined, reaskReasonEmpty, investigationTimeout, investigationFailed, investigationRateLimited}

// summarizeReask sorts the pool into the exclusions (each first call counted
// once, in the order asked again, has a report, rugged, wiped), the calls too
// young or too old, and the candidates. It applies the same rules as
// NextReaskCandidate. pool is in SelectReaskPool's order (the lane's).
func summarizeReask(c reaskConfig, now time.Time, pool []ReaskCandidate) reaskSummary {
	s := reaskSummary{Pool: len(pool), ByReason: map[string]int{}, Now: now}
	from, to := now.Add(-c.MaxAge), now.Add(-c.MinAge)
	for _, r := range pool {
		switch {
		case r.Reasked:
			s.Reasked++
			continue
		case r.HasReport:
			s.HasReport++
			continue
		case r.Rugged:
			s.Rugged++
			continue
		case r.Wiped:
			s.Wiped++
			continue
		}
		s.ByReason[r.Reason]++
		age := now.Sub(r.MessageDate)
		switch {
		case r.MessageDate.After(to):
			s.Young++
		case r.MessageDate.Before(from):
			switch {
			case age <= 7*24*time.Hour:
				s.Old[0]++
			case age <= 30*24*time.Hour:
				s.Old[1]++
			default:
				s.Old[2]++
			}
		default:
			s.Candidates++
			s.List = append(s.List, r)
		}
	}
	return s
}

// runReaskDryRun prints how many first calls the re-ask lane would ask sAlpha
// about again under the current settings. Database only.
func runReaskDryRun(ctx context.Context, st *ScoutStore, w io.Writer, c reaskConfig, now time.Time) error {
	pool, err := st.SelectReaskPool(ctx)
	if err != nil {
		return err
	}
	writeReaskSummary(w, c, summarizeReask(c, now, pool))
	return nil
}

// writeReaskSummary prints a summary (see runReaskDryRun).
func writeReaskSummary(w io.Writer, c reaskConfig, s reaskSummary) {
	state := "off"
	if c.Enabled {
		state = "on"
	}
	fmt.Fprintf(w, "sAlpha re-ask dry run (nothing is sent; no investigation or delivery is written); SCOUT_SALPHA_REASK=%s\n", state)
	fmt.Fprintf(w, "settings: %s\n", c.describe())
	fmt.Fprintf(w, "first calls whose latest live sAlpha reply was no report: %d\n", s.Pool)
	fmt.Fprintf(w, "excluded (each counted once, in this order): asked again already %d, token has a report %d, rugged %d, latest return <= %d%% %d\n",
		s.Reasked, s.HasReport, s.Rugged, rescanWipedPct, s.Wiped)
	var parts []string
	for _, r := range reaskReasons {
		parts = append(parts, fmt.Sprintf("%s %d", r, s.ByReason[r]))
	}
	fmt.Fprintf(w, "left, by the call's reply (any age): %s\n", strings.Join(parts, ", "))
	fmt.Fprintf(w, "younger than %s (asked later): %d\n", hoursText(c.MinAge), s.Young)
	parts = parts[:0]
	for i, name := range reaskOldBuckets {
		parts = append(parts, fmt.Sprintf("%s %d", name, s.Old[i]))
	}
	fmt.Fprintf(w, "older than %s (skipped; a larger SCOUT_SALPHA_REASK_MAX_AGE would take them): %s\n", hoursText(c.MaxAge), strings.Join(parts, ", "))
	fmt.Fprintf(w, "candidates now (%s to %s after the call): %d\n", hoursText(c.MinAge), hoursText(c.MaxAge), s.Candidates)
	perDay := c.perDay()
	fmt.Fprintf(w, "ETA: at most %d a day (cap %d, gap %s) -> about %.0f day(s) for these %d; longer in practice: the lane runs only while the live queue is idle\n",
		perDay, c.MaxPerDay, c.Gap, math.Ceil(float64(s.Candidates)/float64(perDay)), s.Candidates)
	if !c.Enabled {
		fmt.Fprintln(w, "the lane runs inside the listener once SCOUT_SALPHA_REASK=on is set")
	}
	if len(s.List) == 0 {
		return
	}
	fmt.Fprintln(w, "candidates in the order the lane takes them (oldest first): call id, posted (UTC), age, the call's reply, contract address")
	for _, r := range s.List {
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\n", r.CallID, r.MessageDate.UTC().Format(time.RFC3339),
			rescanAgeText(s.Now.Sub(r.MessageDate)), r.Reason, r.ContractAddress)
	}
}
