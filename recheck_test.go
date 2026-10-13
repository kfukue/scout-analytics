package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// The real "not enough data yet" answers of Perceptor (8–9 Oct 2026), with and
// without the smart-money line, "ATH now" and a first scan at another price.
var insufficientReplies = []string{
	"$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor\n" +
		"📊 MC $21.5K · ATH $22.8K (-6%) · first scanned here at $21.5K\n" +
		"🧠 Smart money · KOLs: 0 still in of 1 (@hotcatbase 89) · Smart wallets: 1 still in of 1\n" +
		"https://www.perceptor.info/r/0fc1598d6fc24311b4437066528e214b",
	"$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor\n" +
		"📊 MC $21.5K · ATH $22.8K (-6%) · first scanned here at $21.5K\n" +
		"https://www.perceptor.info/r/0fc1598d6fc24311b4437066528e214b",
	"$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor\n" +
		"📊 MC $23.0K · ATH now · first scanned here at $16.8K (+37%)\n" +
		"https://www.perceptor.info/r/0fc1598d6fc24311b4437066528e214b",
	"$SNIFF on-chain check: Not Enough Data Yet. Full breakdown via @0xPerceptor",
	"$SNIFF ON-CHAIN CHECK: NOT ENOUGH DATA YET",
}

func TestTextVerdictInsufficient(t *testing.T) {
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for i, text := range insufficientReplies {
		v := textVerdict(text, mk, sf)
		if v.Level != levelInsufficient || v.Label != labelInsufficient || v.Ticker != "$SNIFF" || v.Source != "telegram_text" {
			t.Errorf("variant %d: got %+v, want level %s, label %q, ticker $SNIFF\ntext: %q", i, v, levelInsufficient, labelInsufficient, text)
		}
	}
	// the other rules are unchanged; an explicit verdict phrase still wins
	for text, want := range map[string]string{
		"$ANYR on-chain check: no red flags found.":                    levelClean,
		"$DARKCOMP on-chain check: caution.":                           levelCaution,
		"$NH on-chain check: red flags.":                               levelRedFlags,
		"Token not found":                                              levelUnknown,
		"not enough data yet for the history, but red flags: honeypot": levelRedFlags,
		"not enough data yet. Caution: owner can mint":                 levelCaution,
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("textVerdict(%q) = %s, want %s", text, v.Level, want)
		}
	}
}

func TestParsePerceptorPageInsufficient(t *testing.T) {
	page := func(title, desc string) string {
		return `<html><head><title>` + title + `</title>
<meta property="og:title" content="` + title + `"/>
<meta property="og:description" content="` + desc + `"/></head></html>`
	}
	cases := []struct {
		name, html, level, ticker, label string
	}{
		{"description only", page("$SNIFF | 0xPerceptor", "$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor"),
			levelInsufficient, "$SNIFF", labelInsufficient},
		{"title", page("$SNIFF: Not enough data yet | 0xPerceptor", "$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor"),
			levelInsufficient, "$SNIFF", labelInsufficient},
		{"clean unchanged", page("$ANYR: No red flags found | 0xPerceptor", "$ANYR on-chain check: no red flags found. Full breakdown via @0xPerceptor"),
			levelClean, "$ANYR", "No red flags found"},
		{"red flags unchanged", page("$NH: Red flags | 0xPerceptor", "$NH on-chain check: red flags. Top 10 hold 40%. Full breakdown via @0xPerceptor"),
			levelRedFlags, "$NH", "Red flags"},
		{"nothing", page("0xPerceptor", ""), levelUnknown, "", "0xPerceptor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := parsePerceptorPage(c.html)
			if v.Level != c.level || v.Ticker != c.ticker || v.Label != c.label {
				t.Errorf("got level %s, ticker %q, label %q; want %s, %q, %q", v.Level, v.Ticker, v.Label, c.level, c.ticker, c.label)
			}
		})
	}
}

func TestVerdictUsable(t *testing.T) {
	for level, want := range map[string]bool{levelClean: true, levelCaution: true, levelRedFlags: true,
		levelInsufficient: false, levelUnknown: false, "": false} {
		if got := verdictUsable(level); got != want {
			t.Errorf("verdictUsable(%q) = %v, want %v", level, got, want)
		}
	}
}

// testRecheckScanner is a scanner with Perceptor (gate) and sAlpha and the
// default deliver levels; with a re-check lane when lane is set.
func testRecheckScanner(t *testing.T, lane bool) *scanner {
	t.Helper()
	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	s := &scanner{cfg: &config{SourceChannel: "scoutrobinhood",
		DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}}}
	for _, sp := range tools {
		s.runners = append(s.runners, newToolRunner(sp))
	}
	if lane {
		s.recheck = &recheckLane{cfg: recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour}, now: time.Now,
			hold: func(ctx context.Context, invID int, scoreLine string) error { return nil }} // no database: the marker "is written"
		s.recheck.init()
	}
	return s
}

func gateResults(s *scanner, status, level string) []*toolResult {
	return []*toolResult{
		{Spec: s.runners[0].spec, Status: status, Verdict: verdict{Level: level, Label: map[string]string{
			levelInsufficient: labelInsufficient, levelCaution: "Caution"}[level], Ticker: "$SNIFF"}},
		{Spec: s.runners[1].spec, Status: investigationCompleted, Verdict: verdict{Level: levelUnknown},
			Replies: []*tg.Message{{ID: 1, Message: "research"}}},
	}
}

// The gate is unchanged for real verdicts; only insufficient and unknown
// answers of a completed Perceptor scan of a stored call wait for the re-check.
func TestRecheckGating(t *testing.T) {
	call := 42
	cases := []struct {
		name   string
		lane   bool
		callID *int
		status string
		level  string
		levels string // extra SCOUT_DELIVER_LEVELS
		// unstored: the Perceptor result was not stored (recordInvestigation failed)
		unstored bool
		// holdErr: writing the held marker fails
		holdErr bool
		// want: shouldDeliver, deferToRecheck
		deliver, deferred bool
	}{
		{"clean", true, &call, investigationCompleted, levelClean, "", false, false, true, false},
		{"caution", true, &call, investigationCompleted, levelCaution, "", false, false, true, false},
		{"red flags", true, &call, investigationCompleted, levelRedFlags, "", false, false, false, false},
		{"insufficient", true, &call, investigationCompleted, levelInsufficient, "", false, false, false, true},
		{"unknown", true, &call, investigationCompleted, levelUnknown, "", false, false, false, true},
		{"timeout", true, &call, investigationTimeout, levelUnknown, "", false, false, false, false},
		{"insufficient, re-check off", false, &call, investigationCompleted, levelInsufficient, "", false, false, false, false},
		{"insufficient, no call id", true, nil, investigationCompleted, levelInsufficient, "", false, false, false, false},
		{"insufficient, not stored", true, &call, investigationCompleted, levelInsufficient, "", true, false, false, false},
		{"insufficient, marker not written", true, &call, investigationCompleted, levelInsufficient, "", false, true, false, false},
		{"insufficient delivered by SCOUT_DELIVER_LEVELS", true, &call, investigationCompleted, levelInsufficient, levelInsufficient, false, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testRecheckScanner(t, c.lane)
			if c.levels != "" {
				s.cfg.DeliverLevels[c.levels] = true
			}
			res := gateResults(s, c.status, c.level)
			if got := s.shouldDeliver(res); got != c.deliver {
				t.Errorf("shouldDeliver(%s %s) = %v, want %v", c.status, c.level, got, c.deliver)
			}
			if c.holdErr {
				s.recheck.hold = func(ctx context.Context, invID int, scoreLine string) error { return errors.New("database gone") }
			}
			inv := &call // the Perceptor result's investigation id (any id: no database here)
			if c.unstored {
				inv = nil
			}
			const line = "Model v1 · 7d: runner 12%, collapse 64%"
			if got := s.deferToRecheck(context.Background(), job{CA: "0xA", CallID: c.callID}, res, inv, line); got != c.deferred {
				t.Errorf("deferToRecheck(%s %s, stored %v, marker fails %v) = %v, want %v", c.status, c.level, !c.unstored, c.holdErr, got, c.deferred)
			}
			// the score line computed at the call is kept only for a held call
			if s.recheck != nil {
				want := ""
				if c.deferred {
					want = line
				}
				if got := s.recheck.scoreLine(call); got != want {
					t.Errorf("deferToRecheck(%s %s): kept score line %q, want %q", c.status, c.level, got, want)
				}
			}
		})
	}
}

func TestRecheckHeader(t *testing.T) {
	s := testRecheckScanner(t, true)
	cases := []struct {
		status, level string
		want          []string
	}{
		{investigationCompleted, levelInsufficient, []string{"⏳ $SNIFF Perceptor: not enough data yet", "• Perceptor: ⏳ not enough data yet"}},
		{investigationCompleted, levelUnknown, []string{"❔ $SNIFF Perceptor: no readable verdict"}},
		{investigationTimeout, levelUnknown, []string{"❔ $SNIFF Perceptor: no report (timeout)"}},
		{investigationCompleted, levelCaution, []string{"🟡 $SNIFF Caution"}},
	}
	for _, c := range cases {
		h := s.deliveryHeader(job{CA: "0xA", SourceMsg: 7}, gateResults(s, c.status, c.level))
		h = withRecheckLine(h, "🔁 Perceptor re-checked 12m after the call")
		lines := strings.Split(h, "\n")
		if len(lines) < 3 || lines[1] != "🔁 Perceptor re-checked 12m after the call" || lines[2] != "CA: 0xA" {
			t.Errorf("%s %s: the re-check line is not the second line:\n%s", c.status, c.level, h)
		}
		for _, w := range c.want {
			if !strings.Contains(h, w) {
				t.Errorf("%s %s: header misses %q:\n%s", c.status, c.level, w, h)
			}
		}
	}
	if got := withRecheckLine("one line", "x"); got != "one line\nx" {
		t.Errorf("withRecheckLine(one line) = %q, want %q", got, "one line\nx")
	}
}

func TestRecheckDecide(t *testing.T) {
	cfg := recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) time.Time { return now.Add(-age) }
	id := 77
	// a stored first re-check whose delivery was stopped before anything was sent
	stored := func(age time.Duration, status, level string) PerceptorRecheckRow {
		return PerceptorRecheckRow{MessageDate: at(age), Recheck1: true, Recheck1ID: &id, Recheck1Status: status, Recheck1Level: level,
			Usable: status == investigationCompleted && verdictUsable(level)}
	}
	cases := []struct {
		name string
		cfg  recheckConfig
		row  PerceptorRecheckRow
		want recheckPlan
	}{
		{"too early", cfg, PerceptorRecheckRow{MessageDate: at(5 * time.Minute)},
			recheckPlan{Step: recheckWait, Due: at(5 * time.Minute).Add(12 * time.Minute)}},
		{"first due", cfg, PerceptorRecheckRow{MessageDate: at(12 * time.Minute)}, recheckPlan{Step: recheckFirst}},
		{"first, end of window", cfg, PerceptorRecheckRow{MessageDate: at(60 * time.Minute)}, recheckPlan{Step: recheckFirst}},
		{"first missed, second waits", cfg, PerceptorRecheckRow{MessageDate: at(61 * time.Minute)},
			recheckPlan{Step: recheckWait, Due: at(61 * time.Minute).Add(2 * time.Hour), FirstMissed: true}},
		{"first done, second waits", cfg, PerceptorRecheckRow{MessageDate: at(30 * time.Minute), Recheck1: true},
			recheckPlan{Step: recheckWait, Due: at(30 * time.Minute).Add(2 * time.Hour)}},
		{"delivered at the call: no first", cfg, PerceptorRecheckRow{MessageDate: at(30 * time.Minute), Delivered: true},
			recheckPlan{Step: recheckWait, Due: at(30 * time.Minute).Add(2 * time.Hour)}},
		{"second due", cfg, PerceptorRecheckRow{MessageDate: at(2 * time.Hour), Recheck1: true}, recheckPlan{Step: recheckSecond}},
		{"second missed", cfg, PerceptorRecheckRow{MessageDate: at(3*time.Hour + time.Second), Recheck1: true},
			recheckPlan{SecondMissed: true}},
		{"both done", cfg, PerceptorRecheckRow{MessageDate: at(2 * time.Hour), Recheck1: true, Recheck2: true}, recheckPlan{}},
		{"a real verdict", cfg, PerceptorRecheckRow{MessageDate: at(20 * time.Minute), Usable: true}, recheckPlan{}},
		{"second off", recheckConfig{After: 12 * time.Minute}, PerceptorRecheckRow{MessageDate: at(2 * time.Hour), Recheck1: true}, recheckPlan{}},
		{"off", recheckConfig{}, PerceptorRecheckRow{MessageDate: at(20 * time.Minute)}, recheckPlan{}},
		{"stored clean re-check, not delivered", cfg, stored(20*time.Minute, investigationCompleted, levelClean), recheckPlan{Step: recheckDeliver}},
		{"stored insufficient re-check, not delivered", cfg, stored(20*time.Minute, investigationCompleted, levelInsufficient), recheckPlan{Step: recheckDeliver}},
		{"stored timeout re-check, not delivered", cfg, stored(59*time.Minute, investigationTimeout, levelUnknown), recheckPlan{Step: recheckDeliver}},
		{"stored red-flags re-check: nothing", cfg, stored(20*time.Minute, investigationCompleted, levelRedFlags), recheckPlan{}},
		{"stored clean re-check, delivered", cfg, func() PerceptorRecheckRow {
			p := stored(20*time.Minute, investigationCompleted, levelClean)
			p.Delivered = true
			return p
		}(), recheckPlan{}},
		{"stored clean re-check, window passed", cfg, stored(61*time.Minute, investigationCompleted, levelClean), recheckPlan{DeliverMissed: true}},
	}
	// as the lane's deliverable with SCOUT_DELIVER_LEVELS clean, caution
	deliverable := func(status, level string) bool {
		return status != investigationCompleted || !verdictUsable(level) || level == levelClean || level == levelCaution
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := recheckDecide(c.cfg, now, c.row, deliverable); got != c.want {
				t.Errorf("recheckDecide(age %s, %+v) = %+v, want %+v", now.Sub(c.row.MessageDate), c.row, got, c.want)
			}
		})
	}
}

func TestLoadRecheckConfig(t *testing.T) {
	cases := []struct {
		after, after2 string
		want          recheckConfig
		wantErr       bool
	}{
		{"", "", recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour}, false},
		{"0", "", recheckConfig{}, false},
		{"off", "3h", recheckConfig{}, false},
		{"5m", "0", recheckConfig{After: 5 * time.Minute}, false},
		{"60m", "90m", recheckConfig{After: time.Hour, After2: 90 * time.Minute}, false},
		{"4m", "", recheckConfig{}, true},
		{"61m", "", recheckConfig{}, true},
		{"12x", "", recheckConfig{}, true},
		{"12m", "59m", recheckConfig{}, true}, // inside the first re-check's window
		{"12m", "25h", recheckConfig{}, true},
	}
	for _, c := range cases {
		t.Setenv("SCOUT_PERCEPTOR_RECHECK_AFTER", c.after)
		t.Setenv("SCOUT_PERCEPTOR_RECHECK2_AFTER", c.after2)
		got, err := loadRecheckConfig()
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("AFTER=%q AFTER2=%q: got %+v, %v; want %+v (error %v)", c.after, c.after2, got, err, c.want, c.wantErr)
		}
	}
	c := recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour}
	if c.maxAge1() != time.Hour || c.maxAge2() != 3*time.Hour || c.horizon() != 3*time.Hour {
		t.Errorf("windows of %+v: %s, %s, horizon %s; want 1h, 3h, 3h", c, c.maxAge1(), c.maxAge2(), c.horizon())
	}
	if c := (recheckConfig{After: 45 * time.Minute}); c.maxAge1() != 75*time.Minute || c.horizon() != 75*time.Minute {
		t.Errorf("windows of %+v: %s, horizon %s; want 1h15m, 1h15m", c, c.maxAge1(), c.horizon())
	}
}

// On the website "not enough data yet" keeps its own value (row, detail,
// "Perceptor today") but the Perceptor filter counts it as "not scanned", like
// unknown; a change from unknown to insufficient changes the version.
func TestWebInsufficientVerdict(t *testing.T) {
	sp := func(s string) *string { return &s }
	at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	rows := func(v1 string) []ScoutWebRow {
		return []ScoutWebRow{
			{CallID: 1, MessageDate: at, ContractAddress: "0x01", PerceptorVerd: sp(v1)},
			{CallID: 2, MessageDate: at, ContractAddress: "0x02", PerceptorVerd: sp(levelUnknown)},
			{CallID: 3, MessageDate: at, ContractAddress: "0x03"},
			{CallID: 4, MessageDate: at, ContractAddress: "0x04", PerceptorVerd: sp(levelClean), PerceptorTodayVerd: sp(levelInsufficient), PerceptorTodayAt: &at},
		}
	}
	s, err := newWebSnapshot(rows(levelInsufficient), 0, nil, testWebConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.counts[0][webBucketNotScanned]; got != 3 {
		t.Errorf("not scanned count = %d, want 3 (insufficient, unknown, none)", got)
	}
	if s.flags[0]&^webFlagUSD != webBucketNotScanned {
		t.Errorf("insufficient row's bucket = %d, want not scanned (%d)", s.flags[0]&^webFlagUSD, webBucketNotScanned)
	}
	if !strings.Contains(string(s.rowJSON), `"perceptor_verdict":"insufficient"`) || !strings.Contains(string(s.rowJSON), `"perceptor_today_verdict":"insufficient"`) {
		t.Errorf("row JSON does not carry insufficient: %s", s.rowJSON)
	}
	if s.todayVerd[3] != webTodayVerdict(sp(levelInsufficient)) || webTodayVerdict(sp(levelInsufficient)) == webTodayVerdict(sp(levelUnknown)) {
		t.Errorf("today code of insufficient = %d, unknown %d; want distinct", webTodayVerdict(sp(levelInsufficient)), webTodayVerdict(sp(levelUnknown)))
	}
	u, err := newWebSnapshot(rows(levelUnknown), 0, nil, testWebConfig)
	if err != nil {
		t.Fatal(err)
	}
	if u.version == s.version {
		t.Error("unknown → insufficient left the snapshot version unchanged")
	}
	if p := newWebReport(&ScoutWebReport{Tool: webToolPerceptor, Verdict: levelInsufficient, Label: sp(labelInsufficient)}); p.verdict != levelInsufficient {
		t.Errorf("detail verdict = %q, want insufficient", p.verdict)
	}
}

func TestRecheckDetails(t *testing.T) {
	var d map[string]any
	b := recheckDetails([]byte(`{"attempts":1}`), 1, 12*time.Minute+30*time.Second)
	if err := json.Unmarshal(b, &d); err != nil || d["recheck"] != float64(1) || d["call_age_s"] != float64(750) || d["attempts"] != float64(1) {
		t.Errorf("recheckDetails = %s (%v), want recheck 1, call_age_s 750, attempts kept", b, err)
	}
}

// A delivery started after a stop sends nothing and says so with
// deliveryNotSentError (the re-check lane delivers again later).
func TestDeliverNotSentAfterStop(t *testing.T) {
	s := testRecheckScanner(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := s.deliver(ctx, job{CA: "0xA"}, "header", nil)
	var notSent *deliveryNotSentError
	if id != 0 || !errors.As(err, &notSent) || !errors.Is(err, context.Canceled) {
		t.Errorf("deliver after a stop = %d, %v; want 0 and a deliveryNotSentError wrapping context.Canceled", id, err)
	}
}
