package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// ---------------------------------------------------------------------------
// Investigation tools
//
// A "tool" is any Telegram bot you DM a CA to and that replies with a report.
// Tools are configured with SCOUT_TOOLS (comma list of codes, run in parallel
// for every CA) and optional per-tool overrides SCOUT_TOOL_<CODE>_*.
// Adding a new bot needs no code: SCOUT_TOOLS=perceptor,salpha,foo +
// SCOUT_TOOL_FOO_BOT=foo_bot (+ SCOUT_TOOL_FOO_COMMAND="/check {ca}").
// ---------------------------------------------------------------------------

const (
	parserPerceptor = "perceptor" // verdict from the linked perceptor.info page, text fallback
	parserText      = "text"      // verdict from keywords in the reply text
)

// ToolSpec describes one investigation tool.
type ToolSpec struct {
	Code     string // stable key, e.g. "perceptor" (stored in scout_investigation_tools.code)
	Name     string // display name, e.g. "Perceptor"
	Bot      string // bot username without @
	Command  string // message template; {ca} is replaced by the contract address
	Parser   string // parserPerceptor | parserText
	Gate     bool   // this tool's verdict decides whether the CA is delivered
	Attach   bool   // attach this tool's report to the delivery
	Timeout  time.Duration
	Settle   time.Duration
	MaxWait  time.Duration
	Position int // order in SCOUT_TOOLS (delivery/attachment order)

	// Rate limiting.
	MinInterval     time.Duration  // min time between two requests to this bot (pacing)
	MaxRetries      int            // retries after a "try again in N s" reply
	RateLimitRe     *regexp.Regexp // detects a rate-limit reply; group 1 = amount, group 2 = unit
	RateLimitBuffer time.Duration  // extra wait on top of the bot's countdown
	MaxRateWait     time.Duration  // never wait longer than this for one retry

	// Completion detection. Bots often answer "Scanning…" first and send or
	// edit in the real report later; the report counts as finished only once a
	// non-progress reply arrives (and, if DoneRe is set, one that matches it).
	ProgressRe *regexp.Regexp // "Scanning…", "Analyzing…" placeholders
	DoneRe     *regexp.Regexp // optional: the final report must match (text or links)
}

const defaultProgressPattern = `(?i)^[\W_]*(scanning|analy[sz]ing|researching|checking|processing|loading|fetching|working on|investigating|please wait|one moment|generating|looking up|searching|running)\b`

var defaultProgressRe = regexp.MustCompile(defaultProgressPattern)

// Default rate-limit detector, e.g. perceptor's
// "One scan every 2 minutes. You can scan again in 63 s".
const defaultRateLimitPattern = `(?i)(?:again|retry|wait)\D{0,20}?(\d+)\s*(s|sec|secs|seconds?|m|mins?|minutes?)\b`

var defaultRateLimitRe = regexp.MustCompile(defaultRateLimitPattern)

func (t ToolSpec) CommandFor(ca string) string {
	if strings.Contains(t.Command, "{ca}") {
		return strings.ReplaceAll(t.Command, "{ca}", ca)
	}
	return strings.TrimSpace(t.Command + " " + ca)
}

var builtinTools = map[string]ToolSpec{
	"perceptor": {Code: "perceptor", Name: "Perceptor", Bot: "perceptor0xBot", Command: "/scan {ca}",
		Parser: parserPerceptor, Gate: true, Attach: true,
		Timeout: 60 * time.Second, Settle: 6 * time.Second, MaxWait: 180 * time.Second,
		// "One scan every 2 minutes": pace requests so we rarely hit the limit.
		MinInterval: 2*time.Minute + 5*time.Second, MaxRetries: 3,
		// First reply is "Scanning 0x… on Robinhood Chain…"; the report has the verdict / perceptor.info link.
		DoneRe: regexp.MustCompile(`(?i)perceptor\.info/|red flags?|caution|no red flags`)},
	// "DM CA for research" — the bot takes the bare contract address.
	"salpha": {Code: "salpha", Name: "sAlpha", Bot: "salpha_research_bot", Command: "{ca}",
		Parser: parserText, Gate: false, Attach: true,
		Timeout: 90 * time.Second, Settle: 15 * time.Second, MaxWait: 300 * time.Second,
		MaxRetries: 3},
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	}
	return def
}

// loadTools builds the tool list from SCOUT_TOOLS and SCOUT_TOOL_<CODE>_* overrides.
func loadTools() ([]ToolSpec, error) {
	codes := splitList(env("SCOUT_TOOLS", "perceptor,salpha"))
	if len(codes) == 0 {
		return nil, errors.New("SCOUT_TOOLS is empty")
	}
	var out []ToolSpec
	seen := map[string]bool{}
	for i, code := range codes {
		if seen[code] {
			continue
		}
		seen[code] = true
		t, ok := builtinTools[code]
		if !ok {
			t = ToolSpec{Code: code, Name: code, Command: "{ca}", Parser: parserText, Attach: true,
				Timeout: 90 * time.Second, Settle: 10 * time.Second, MaxWait: 240 * time.Second, MaxRetries: 3}
		}
		// Backward compatibility with the single-bot settings.
		if code == "perceptor" {
			t.Bot = env("SCOUT_SCAN_BOT", t.Bot)
			if c := os.Getenv("SCOUT_SCAN_COMMAND"); c != "" {
				t.Command = c
			}
			t.Timeout = envDur("SCOUT_SCAN_TIMEOUT", t.Timeout)
			t.Settle = envDur("SCOUT_SETTLE", t.Settle)
			t.MaxWait = envDur("SCOUT_MAX_WAIT", t.MaxWait)
		}
		p := "SCOUT_TOOL_" + strings.ToUpper(strings.ReplaceAll(code, "-", "_")) + "_"
		t.Name = env(p+"NAME", t.Name)
		t.Bot = strings.TrimPrefix(env(p+"BOT", t.Bot), "@")
		t.Command = env(p+"COMMAND", t.Command)
		t.Parser = strings.ToLower(env(p+"PARSER", t.Parser))
		t.Gate = envBool(p+"GATE", t.Gate)
		t.Attach = envBool(p+"ATTACH", t.Attach)
		t.Timeout = envDur(p+"TIMEOUT", t.Timeout)
		t.Settle = envDur(p+"SETTLE", t.Settle)
		t.MaxWait = envDur(p+"MAX_WAIT", t.MaxWait)
		t.MinInterval = envDur(p+"MIN_INTERVAL", t.MinInterval)
		if v := os.Getenv(p + "MAX_RETRIES"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%sMAX_RETRIES=%q: want a number >= 0", p, v)
			}
			t.MaxRetries = n
		}
		t.RateLimitRe = defaultRateLimitRe
		if v := os.Getenv(p + "RATE_LIMIT_REGEX"); v != "" {
			re, err := regexp.Compile(v)
			if err != nil || re.NumSubexp() < 1 {
				return nil, fmt.Errorf("%sRATE_LIMIT_REGEX: need a valid regex with a (number) group: %v", p, err)
			}
			t.RateLimitRe = re
		}
		t.ProgressRe = defaultProgressRe
		if v := os.Getenv(p + "PROGRESS_REGEX"); v != "" {
			re, err := regexp.Compile(v)
			if err != nil {
				return nil, fmt.Errorf("%sPROGRESS_REGEX: %v", p, err)
			}
			t.ProgressRe = re
		}
		if v := os.Getenv(p + "DONE_REGEX"); v != "" {
			if strings.EqualFold(v, "none") {
				t.DoneRe = nil
			} else {
				re, err := regexp.Compile(v)
				if err != nil {
					return nil, fmt.Errorf("%sDONE_REGEX: %v", p, err)
				}
				t.DoneRe = re
			}
		}
		t.RateLimitBuffer = envDur(p+"RATE_LIMIT_BUFFER", 3*time.Second)
		t.MaxRateWait = envDur(p+"MAX_RATE_WAIT", 10*time.Minute)
		t.Position = i
		if t.Bot == "" {
			return nil, fmt.Errorf("tool %q: set %sBOT to the bot's username", code, p)
		}
		if t.Parser != parserPerceptor && t.Parser != parserText {
			return nil, fmt.Errorf("tool %q: unknown parser %q (use perceptor or text)", code, t.Parser)
		}
		out = append(out, t)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Runner: sends the command to one bot and collects its replies/edits.
// Different bots run concurrently; each runner handles one CA at a time.
// ---------------------------------------------------------------------------

type toolRunner struct {
	spec    ToolSpec
	toolID  *int // scout_investigation_tools.id (nil without DB)
	botID   int64
	botPeer tg.InputPeerClass

	lastSent time.Time // for MinInterval pacing (one request in flight per runner)

	mu      sync.Mutex
	active  bool
	since   int
	replies map[int]*tg.Message
	bump    chan struct{}
}

func newToolRunner(spec ToolSpec) *toolRunner {
	return &toolRunner{spec: spec, replies: map[int]*tg.Message{}, bump: make(chan struct{}, 1)}
}

// toolResult is what one tool produced for one CA.
type toolResult struct {
	Spec        ToolSpec
	Command     string
	RequestedAt time.Time
	CompletedAt time.Time
	Replies     []*tg.Message // sorted by message id
	Best        *tg.Message   // longest reply (the report rather than a "working…" stub)
	Verdict     verdict
	Status      string // completed | failed | timeout | rate_limited
	Err         error

	Attempts       int   // requests sent (1 + rate-limit retries)
	RateLimitWaits []int // seconds waited after each rate-limit reply
}

func (r *toolResult) ReportText() string {
	var parts []string
	for _, m := range r.Replies {
		if t := strings.TrimSpace(m.Message); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (r *toolResult) MessageIDs() []int32 {
	ids := make([]int32, 0, len(r.Replies))
	for _, m := range r.Replies {
		ids = append(ids, int32(m.ID))
	}
	return ids
}

func (r *toolRunner) onMessage(msg *tg.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && msg.Date >= r.since {
		r.replies[msg.ID] = msg
		select {
		case r.bump <- struct{}{}:
		default:
		}
	}
}

func (r *toolRunner) snapshot() []*tg.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := make([]*tg.Message, 0, len(r.replies))
	for _, m := range r.replies {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all
}

// sendFunc sends text to the tool's bot.
type sendFunc func(ctx context.Context, text string) error

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rateLimitWait reports whether a completed result is really a rate-limit reply
// ("You can scan again in 63 s") and how long to wait before retrying.
func (r *toolRunner) rateLimitWait(res *toolResult) (time.Duration, bool) {
	if res.Status != investigationCompleted || r.spec.RateLimitRe == nil {
		return 0, false
	}
	text := res.ReportText()
	if len(text) > 600 { // a real report, not a short "try again" notice
		return 0, false
	}
	m := r.spec.RateLimitRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if len(m) > 2 && strings.HasPrefix(strings.ToLower(m[2]), "m") {
		d = time.Duration(n) * time.Minute
	}
	d += r.spec.RateLimitBuffer
	if d > r.spec.MaxRateWait {
		d = r.spec.MaxRateWait
	}
	return d, true
}

// investigateWithRetry paces requests (MinInterval) and, when the bot answers
// with a rate-limit notice, waits the requested time and asks again
// (up to MaxRetries times).
func (r *toolRunner) investigateWithRetry(ctx context.Context, send sendFunc, ca string) *toolResult {
	return r.investigateWithRetryN(ctx, send, ca, r.spec.MaxRetries)
}

// investigateWithRetryN is investigateWithRetry with maxRetries in place of
// the tool's MaxRetries (the rescan lane uses 0: a rate-limit notice ends the
// attempt as rate_limited at once).
func (r *toolRunner) investigateWithRetryN(ctx context.Context, send sendFunc, ca string, maxRetries int) *toolResult {
	var waits []int
	first := time.Time{}
	for attempt := 1; ; attempt++ {
		if r.spec.MinInterval > 0 && !r.lastSent.IsZero() {
			if d := r.spec.MinInterval - time.Since(r.lastSent); d > 0 {
				log.Printf("[%s] pacing: waiting %s before next request to @%s", r.spec.Code, d.Round(time.Second), r.spec.Bot)
				if err := sleepCtx(ctx, d); err != nil {
					return &toolResult{Spec: r.spec, Command: r.spec.CommandFor(ca), RequestedAt: time.Now().UTC(),
						CompletedAt: time.Now().UTC(), Status: investigationFailed, Err: err, Attempts: attempt - 1, RateLimitWaits: waits}
				}
			}
		}
		res := r.investigate(ctx, send, ca)
		if first.IsZero() {
			first = res.RequestedAt
		}
		res.RequestedAt, res.Attempts, res.RateLimitWaits = first, attempt, waits
		wait, limited := r.rateLimitWait(res)
		if !limited {
			return res
		}
		if attempt > maxRetries {
			res.Status = investigationRateLimited
			res.Err = fmt.Errorf("@%s still rate-limited after %d attempt(s): %s", r.spec.Bot, attempt, strings.TrimSpace(res.ReportText()))
			return res
		}
		log.Printf("[%s] rate-limited by @%s (%q) — retrying in %s (retry %d/%d)",
			r.spec.Code, r.spec.Bot, strings.TrimSpace(res.ReportText()), wait.Round(time.Second), attempt, maxRetries)
		waits = append(waits, int(wait.Round(time.Second)/time.Second))
		if err := sleepCtx(ctx, wait); err != nil {
			res.Status, res.Err = investigationFailed, err
			return res
		}
	}
}

// investigate sends the command once and waits until the bot has gone quiet.
func (r *toolRunner) investigate(ctx context.Context, send sendFunc, ca string) *toolResult {
	res := &toolResult{Spec: r.spec, Command: r.spec.CommandFor(ca), RequestedAt: time.Now().UTC()}
	defer func() { res.CompletedAt = time.Now().UTC() }()

	r.mu.Lock()
	r.active = true
	r.since = int(time.Now().Unix()) - 2
	r.replies = map[int]*tg.Message{}
	select {
	case <-r.bump:
	default:
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active = false
		r.mu.Unlock()
	}()

	r.lastSent = time.Now()
	if err := send(ctx, res.Command); err != nil {
		res.Status, res.Err = investigationFailed, fmt.Errorf("send %q to @%s: %w", res.Command, r.spec.Bot, err)
		return res
	}
	log.Printf("[%s] sent to @%s: %s", r.spec.Code, r.spec.Bot, res.Command)

	hard := time.NewTimer(r.spec.MaxWait)
	defer hard.Stop()
	wait := time.NewTimer(r.spec.Timeout) // first-reply timeout, later the settle timer
	defer wait.Stop()
	stopTimer := func(t *time.Timer) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
	got := false
	finish := func(status string, err error) *toolResult {
		all := r.snapshot()
		res.Replies = r.reportMessages(all)
		for _, m := range res.Replies {
			if res.Best == nil || len(m.Message) > len(res.Best.Message) {
				res.Best = m
			}
		}
		res.Status, res.Err = status, err
		return res
	}
	for {
		select {
		case <-ctx.Done():
			return finish(investigationFailed, ctx.Err())
		case <-r.bump:
			got = true
			stopTimer(wait)
			if r.isComplete(r.snapshot()) {
				wait.Reset(r.spec.Settle) // report is here; wait for trailing edits/parts
			}
			// still only "Scanning…": keep waiting (bounded by MaxWait)
		case <-wait.C:
			if !got {
				return finish(investigationTimeout, fmt.Errorf("no reply from @%s within %s", r.spec.Bot, r.spec.Timeout))
			}
			return finish(investigationCompleted, nil)
		case <-hard.C:
			all := r.snapshot()
			if r.isComplete(all) {
				return finish(investigationCompleted, nil)
			}
			if got {
				last := ""
				if n := len(all); n > 0 {
					last = strings.TrimSpace(all[n-1].Message)
				}
				return finish(investigationTimeout, fmt.Errorf("@%s's report did not finish within %s (last reply: %q)", r.spec.Bot, r.spec.MaxWait, last))
			}
			return finish(investigationTimeout, fmt.Errorf("timed out waiting for @%s", r.spec.Bot))
		}
	}
}

// isProgress reports whether a reply is only a placeholder: "Scanning…" text,
// or no text with nothing but a sticker/animation.
func (r *toolRunner) isProgress(m *tg.Message) bool {
	text := strings.TrimSpace(m.Message)
	if text == "" {
		return !hasReportMedia(m)
	}
	return len(text) < 200 && r.spec.ProgressRe != nil && r.spec.ProgressRe.MatchString(text)
}

func hasReportMedia(m *tg.Message) bool {
	switch md := m.Media.(type) {
	case nil:
		return false
	case *tg.MessageMediaPhoto:
		return true
	case *tg.MessageMediaDocument:
		if doc, ok := md.Document.(*tg.Document); ok {
			for _, a := range doc.Attributes {
				switch a.(type) {
				case *tg.DocumentAttributeSticker, *tg.DocumentAttributeAnimated:
					return false // loading sticker / GIF
				}
			}
		}
		return true
	case *tg.MessageMediaWebPage:
		return false
	default:
		return true
	}
}

// isComplete: at least one non-placeholder reply, matching DoneRe when set.
// A rate-limit notice also counts as complete (it's handled by the retry logic).
func (r *toolRunner) isComplete(msgs []*tg.Message) bool {
	for _, m := range msgs {
		if r.isProgress(m) {
			continue
		}
		if r.spec.RateLimitRe != nil && len(m.Message) <= 600 && r.spec.RateLimitRe.MatchString(m.Message) {
			return true
		}
		if r.spec.DoneRe == nil || r.spec.DoneRe.MatchString(strings.Join(messageURLs(m), "\n")) {
			return true
		}
	}
	return false
}

// reportMessages drops placeholder replies ("Scanning…", loading stickers) so
// only the real report is judged, stored and forwarded.
func (r *toolRunner) reportMessages(all []*tg.Message) []*tg.Message {
	var out []*tg.Message
	for _, m := range all {
		if !r.isProgress(m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}
