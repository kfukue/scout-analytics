package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// seedSAlpha stores one sAlpha investigation with a reply text.
func seedSAlpha(t *testing.T, st *ScoutStore, tool int, callID *int, ca string, at time.Time, status, kind, text string) int {
	t.Helper()
	r := &ScoutInvestigation{CallID: callID, ToolID: tool, ContractAddress: ca, RequestText: ca,
		RequestedAt: at, CompletedAt: &at, Status: status, VerdictLevel: levelUnknown, ScanKind: kind, ReportText: text}
	id, err := st.InsertScoutInvestigation(context.Background(), r)
	if err != nil {
		t.Fatalf("seed sAlpha investigation of %s: %v", ca, err)
	}
	return *id
}

// The SQL decline rule (salphaDeclinedSQL) and salphaDeclined agree on every
// text, including lengths around salphaDeclineMaxLen counted in characters of
// several bytes.
func TestSAlphaDeclineSQLParity(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const decline = "Not enough public signals."
	pad := func(total int, r string) string {
		return decline + strings.Repeat(r, total-len([]rune(decline)))
	}
	texts := map[string]string{
		"empty":           "",
		"white space":     " \t\n\v\f\r ",
		"real":            "Smart money: 3 wallets bought; dev holds 2%.",
		"upper case":      "  NOT ENOUGH PUBLIC SIGNALS to generate a report\n",
		"trim set around": "\r\n\t\v\f " + decline + " \f\v\t\n\r",
		"nbsp is no trim": "\u00a0" + decline,
		"long risk line":  strings.Repeat("Holders grow; ", 30) + "too little liquidity to exit.",
	}
	for _, p := range salphaDeclinePhrases {
		texts["phrase "+p] = "x " + p + " y"
	}
	for _, n := range []int{299, 300, 301} {
		texts[strconv.Itoa(n)+" é"] = pad(n, "é")
		texts[strconv.Itoa(n)+" emoji"] = pad(n, "🚀")
		texts[strconv.Itoa(n)+" ascii"] = pad(n, ".")
		texts[strconv.Itoa(n)+" é + trailing space"] = pad(n, "é") + "   \n"
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			var got bool
			if err := st.Pool.QueryRow(ctx, `SELECT `+salphaDeclinedSQL+` FROM (SELECT $4::text AS report_text) i`,
				salphaDeclinePhrases, salphaDeclineMaxLen, salphaDeclineTrim, text).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := salphaDeclined(text); got != want {
				t.Errorf("SQL decline rule = %v, salphaDeclined = %v for %q (%d characters)", got, want, text, len([]rune(text)))
			}
		})
	}
}

// reaskFixture seeds first calls covering every rule of the selection.
type reaskFixture struct {
	st          *ScoutStore
	perc, sa    int
	now         time.Time
	ids         map[string]int
	cfg         reaskConfig
	order       []string // expected candidates, oldest first
	poolExclude []string // in the pool, never candidates (any age)
}

func newReaskFixture(t *testing.T) *reaskFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fx := &reaskFixture{st: st, now: time.Now().UTC().Truncate(time.Second), ids: map[string]int{},
		cfg: reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute}}
	fx.perc = mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	fx.sa = mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	h := func(n float64) time.Time { return fx.now.Add(-time.Duration(n * float64(time.Hour))) }
	yes, no := true, false
	f := func(v float64) *float64 { return &v }
	ca := func(n int) string { return "0x" + strings.Repeat("0", 38) + strconv.Itoa(10+n) }
	msg := 0
	add := func(key string, s rescanSeed) int {
		msg++
		s.msg = msg
		if s.status == "" {
			s.status = CallStatusScanned
		}
		id := seedRescanCall(t, st, s)
		if key != "" {
			fx.ids[key] = id
		}
		return id
	}
	live := func(key, ca string, at time.Time, status, text string) {
		seedSAlpha(t, st, fx.sa, ip(fx.ids[key]), ca, at, status, ScanKindLive, text)
	}
	long := strings.Repeat("Holders grow steadily; dev wallet holds 2%. ", 8) + "Risks: too little liquidity to exit."

	// candidates
	add("timeout", rescanSeed{ca: ca(1), at: h(20)})
	live("timeout", ca(1), h(20), investigationTimeout, "")
	add("rate_limited", rescanSeed{ca: ca(2), at: h(18), track: true, rugged: &no, latest: f(-98)})
	live("rate_limited", ca(2), h(18), investigationRateLimited, "Try again in 60 s")
	add("first_of_repeat", rescanSeed{ca: ca(3), at: h(15)})
	live("first_of_repeat", ca(3), h(15), investigationFailed, "")
	add("", rescanSeed{ca: ca(3), at: h(9), status: CallStatusDuplicate})
	add("empty", rescanSeed{ca: ca(4), at: h(12)})
	live("empty", ca(4), h(12), investigationCompleted, " \n ")
	add("declined", rescanSeed{ca: ca(5), at: h(10)})
	live("declined", ca(5), h(10), investigationCompleted, testDecline)
	// an earlier real report of the call, then a timeout (latest): the token has a report
	add("real_then_timeout", rescanSeed{ca: ca(6), at: h(10)})
	live("real_then_timeout", ca(6), h(10), investigationCompleted, "Smart money: 3 wallets")
	live("real_then_timeout", ca(6), h(9.9), investigationTimeout, "")
	fx.order = []string{"timeout", "rate_limited", "first_of_repeat", "empty", "declined"}

	// in the pool, excluded
	add("young", rescanSeed{ca: ca(20), at: h(2)})
	live("young", ca(20), h(2), investigationCompleted, testDecline)
	add("old", rescanSeed{ca: ca(21), at: h(30)})
	live("old", ca(21), h(30), investigationTimeout, "")
	add("reasked", rescanSeed{ca: ca(22), at: h(8)})
	live("reasked", ca(22), h(8), investigationCompleted, testDecline)
	seedSAlpha(t, st, fx.sa, ip(fx.ids["reasked"]), ca(22), h(1), investigationTimeout, ScanKindReask, "")
	add("rugged", rescanSeed{ca: ca(23), at: h(8), track: true, rugged: &yes})
	live("rugged", ca(23), h(8), investigationTimeout, "")
	add("wiped", rescanSeed{ca: ca(24), at: h(8), track: true, rugged: &no, latest: f(-99.5)})
	live("wiped", ca(24), h(8), investigationTimeout, "")
	add("real_elsewhere", rescanSeed{ca: ca(25), at: h(11)})
	live("real_elsewhere", ca(25), h(11), investigationTimeout, "")
	seedSAlpha(t, st, fx.sa, nil, strings.ToUpper("0x"+ca(25)[2:]), h(11), investigationCompleted, ScanKindLive, "Real report, other case")
	fx.poolExclude = []string{"real_then_timeout", "young", "old", "reasked", "rugged", "wiped", "real_elsewhere"}

	// not in the pool at all
	add("latest_real", rescanSeed{ca: ca(30), at: h(10)})
	live("latest_real", ca(30), h(10), investigationTimeout, "")
	live("latest_real", ca(30), h(9.9), investigationCompleted, "Smart money: 3 wallets")
	add("long_report", rescanSeed{ca: ca(31), at: h(10)})
	live("long_report", ca(31), h(10), investigationCompleted, long)
	add("no_salpha", rescanSeed{ca: ca(32), at: h(10)})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["no_salpha"]), ca(32), h(10), investigationCompleted, ScanKindLive, "")
	add("only_reask", rescanSeed{ca: ca(33), at: h(10), status: CallStatusBackfill})
	seedSAlpha(t, st, fx.sa, ip(fx.ids["only_reask"]), ca(33), h(1), investigationTimeout, ScanKindReask, "")
	add("update_post", rescanSeed{ca: ca(34), at: h(10), status: CallStatusUpdate, postKind: PostKindUpdate})
	live("update_post", ca(34), h(10), investigationTimeout, "")
	return fx
}

// NextReaskCandidate walks the candidates oldest first; each re-ask (any
// status) takes its token out, once per token.
func TestNextReaskCandidate(t *testing.T) {
	fx := newReaskFixture(t)
	ctx := context.Background()
	byID := map[int]string{}
	for k, id := range fx.ids {
		byID[id] = k
	}
	pool, err := fx.st.SelectReaskPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inPool := map[string]bool{}
	for _, c := range pool {
		inPool[byID[c.CallID]] = true
	}
	for _, k := range append(append([]string{}, fx.order...), fx.poolExclude...) {
		if !inPool[k] {
			t.Errorf("%s not in the pool", k)
		}
	}
	for _, k := range []string{"latest_real", "long_report", "no_salpha", "only_reask", "update_post"} {
		if inPool[k] {
			t.Errorf("%s in the pool, want it left out", k)
		}
	}
	reasons := map[string]string{}
	for _, c := range pool {
		reasons[byID[c.CallID]] = c.Reason
	}
	for k, want := range map[string]string{"timeout": investigationTimeout, "rate_limited": investigationRateLimited,
		"first_of_repeat": investigationFailed, "empty": reaskReasonEmpty, "declined": reaskReasonDeclined} {
		if reasons[k] != want {
			t.Errorf("%s: reason %q, want %q", k, reasons[k], want)
		}
	}

	var got []string
	for i := 0; i < 20; i++ {
		c, err := fx.st.NextReaskCandidate(ctx, fx.now.Add(-fx.cfg.MaxAge), fx.now.Add(-fx.cfg.MinAge))
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			break
		}
		got = append(got, byID[c.CallID])
		// a re-ask that failed still counts: once per token
		seedSAlpha(t, fx.st, fx.sa, ip(c.CallID), c.ContractAddress, fx.now, investigationFailed, ScanKindReask, "")
	}
	if strings.Join(got, ",") != strings.Join(fx.order, ",") {
		t.Errorf("candidates in order:\n got %v\nwant %v", got, fx.order)
	}
	n, last, err := fx.st.ReaskStats(ctx, fx.now.Add(-24*time.Hour))
	if err != nil || n != len(fx.order)+2 || last == nil || !last.Equal(fx.now) {
		t.Errorf("ReaskStats = %d, %v, %v; want %d (the candidates and the fixture's two) and %s", n, last, err, len(fx.order)+2, fx.now)
	}
}

// The dry run's numbers follow the same rules as the selection.
func TestReaskDryRun(t *testing.T) {
	fx := newReaskFixture(t)
	var out strings.Builder
	if err := runReaskDryRun(context.Background(), fx.st, &out, fx.cfg, fx.now); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"first calls whose latest live sAlpha reply was no report: 12\n",
		"asked again already 1, token has a report 2, rugged 1, latest return <= -99% 1\n",
		"younger than 6 h (asked later): 1\n",
		"up to 7d 1, 7-30d 0, >30d 0\n",
		"candidates now (6 h to 24 h after the call): 5\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("dry run lacks %q:\n%s", line, out.String())
		}
	}
	_, list, ok := strings.Cut(out.String(), "contract address\n")
	if !ok {
		t.Fatalf("dry run has no candidate list:\n%s", out.String())
	}
	var got, want []string
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		got = append(got, strings.Fields(line)[0])
	}
	for _, k := range fx.order {
		want = append(want, strconv.Itoa(fx.ids[k]))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dry run lists call ids %v, want %v (%v)", got, want, fx.order)
	}
}

// 'reask' rows are invisible to the readers of the other kinds: the rescan
// lane's counts and pool, the requeue and the dataset view.
func TestReaskRowsInvisibleToOtherReaders(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := datasetColumns(t, st)
	mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	sa := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	now := time.Now().UTC()
	ca := "0x1111111111111111111111111111111111111111"
	first := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: now.Add(-10 * time.Hour), status: CallStatusFailed})
	stuck := seedRescanCall(t, st, rescanSeed{msg: 2, ca: ca, at: now.Add(-time.Hour), status: CallStatusQueued})
	seedSAlpha(t, st, sa, &first, ca, now.Add(-2*time.Hour), investigationCompleted, ScanKindReask, "Real report")
	seedSAlpha(t, st, sa, &stuck, ca, now.Add(-30*time.Minute), investigationTimeout, ScanKindReask, "")

	if n, last, err := st.RescanStats(ctx, now.Add(-24*time.Hour)); err != nil || n != 0 || last != nil {
		t.Errorf("RescanStats with only re-asks = %d, %v, %v; want 0, nil", n, last, err)
	}
	pool, err := st.SelectRescanPool(ctx, []string{CallStatusFailed})
	if err != nil || len(pool) != 1 || pool[0].CallID != first || pool[0].FailedRescans != 0 {
		t.Errorf("rescan pool = %+v, %v; want call %d with no failed rescans", pool, err, first)
	}
	calls, _, err := st.SelectScoutCallsToRequeue(ctx, 777, now.Add(-72*time.Hour))
	if err != nil || len(calls) != 1 || calls[0].ID != stuck || calls[0].CAInvestigated {
		t.Errorf("requeue with re-asks only = %+v, %v; want call %d, not investigated", calls, err, stuck)
	}
	var v *string
	if err := st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, first).Scan(&v); err != nil || v != nil {
		t.Errorf("dataset view perceptor_verdict = %v, %v; want NULL", v, err)
	}
	if after := datasetColumns(t, st); after != before {
		t.Errorf("scout_call_dataset_v columns changed:\n got %s\nwant %s", after, before)
	}
}

// The website's sAlpha report per token: a real report before a decline, a
// call's own before a re-ask; a re-ask's kind and time come with it.
func TestReaskWebRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sa := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ca := func(n int) string { return "0x" + strings.Repeat("0", 38) + strconv.Itoa(10+n) }
	type want struct {
		id   int
		kind string
	}
	wants := map[int]want{}
	for i, tc := range []struct {
		name                  string
		liveText, reaskText   string // "" = no such row
		wantLive, wantReaskAt bool
	}{
		{"live real, re-ask real", "Live report", "Later report", true, false},
		{"live decline, re-ask real", testDecline, "Later report", false, true},
		{"live decline, re-ask decline", testDecline, testDecline, true, false},
		{"only a re-ask", "", "Later report", false, true},
		{"live empty, re-ask decline", " ", testDecline, false, true},
	} {
		at := t0.Add(time.Duration(i) * time.Hour)
		call := seedRescanCall(t, st, rescanSeed{msg: i + 1, ca: ca(i), at: at, status: CallStatusScanned})
		var liveID, reaskID int
		if tc.liveText != "" {
			liveID = seedSAlpha(t, st, sa, &call, ca(i), at.Add(time.Minute), investigationCompleted, ScanKindLive, tc.liveText)
		}
		if tc.reaskText != "" {
			reaskID = seedSAlpha(t, st, sa, &call, ca(i), at.Add(7*time.Hour), investigationCompleted, ScanKindReask, tc.reaskText)
		}
		if tc.wantLive {
			wants[call] = want{liveID, ScanKindLive}
		} else {
			wants[call] = want{reaskID, ScanKindReask}
		}
	}
	rows, _, err := st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		w, ok := wants[r.CallID]
		if !ok {
			continue
		}
		if r.SAlphaID == nil || *r.SAlphaID != w.id || r.SAlphaKind == nil || *r.SAlphaKind != w.kind || r.SAlphaAt == nil {
			t.Errorf("call %d: sAlpha id %v kind %v at %v; want %d %s", r.CallID, r.SAlphaID, r.SAlphaKind, r.SAlphaAt, w.id, w.kind)
		}
		if w.kind == ScanKindReask && !r.SAlphaAt.Equal(r.MessageDate.Add(7*time.Hour)) {
			t.Errorf("call %d: sAlpha at %s, want 7h after the call %s", r.CallID, r.SAlphaAt, r.MessageDate)
		}
		reps, err := st.SelectWebReports(ctx, []int{w.id})
		if err != nil || reps[w.id] == nil || reps[w.id].Kind != w.kind {
			t.Errorf("SelectWebReports(%d) = %+v, %v; want kind %s", w.id, reps[w.id], err, w.kind)
		}
	}
}

// A delivery with an sAlpha decline records only the forwarded reports and
// the header's message id; the schema upgrade adds the column and index twice
// without error.
func TestDeliveryRecordedWithoutDecline(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DROP VIEW scout_investigations_v; DROP INDEX scout_investigations_reask_idx;
		ALTER TABLE scout_deliveries DROP COLUMN notify_message_id`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("upgrade migrate %d: %v", i+1, err)
		}
	}
	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Tools: tools, StateDir: t.TempDir(), NotifyPeer: "me",
		DeliverLevels: map[string]bool{levelClean: true}}
	s := newScanner(cfg)
	s.db = st
	if err := s.registerTools(ctx); err != nil {
		t.Fatal(err)
	}
	ca := "0x2222222222222222222222222222222222222222"
	call := seedRescanCall(t, st, rescanSeed{msg: 9, ca: ca, at: time.Now().Add(-time.Minute), status: CallStatusScanned})
	j := job{CA: ca, CallID: &call}
	now := time.Now().UTC()
	results := []*toolResult{
		{Spec: tools[0], Command: "/scan " + ca, RequestedAt: now, CompletedAt: now, Status: investigationCompleted,
			Replies: []*tg.Message{{ID: 1, Message: "No red flags"}}, Verdict: verdict{Level: levelClean}},
		{Spec: tools[1], Command: ca, RequestedAt: now, CompletedAt: now, Status: investigationCompleted,
			Replies: []*tg.Message{{ID: 2, Message: testDecline}}, Verdict: verdict{Level: levelUnknown}},
	}
	if !s.shouldDeliver(results) {
		t.Fatal("clean Perceptor with an sAlpha decline: not delivered")
	}
	invIDs := map[string]*int{}
	for _, r := range results {
		invIDs[r.Spec.Code] = s.recordInvestigation(j, r)
	}
	s.recordDelivery(j, s.deliveryHeader(j, results), deliveryAttachments(results, invIDs), 777, nil)
	ds, err := st.SelectScoutDeliveries(ctx, &call, 10)
	if err != nil || len(ds) != 1 {
		t.Fatalf("deliveries = %+v, %v; want 1", ds, err)
	}
	d := ds[0]
	if len(d.InvestigationIDs) != 1 || d.InvestigationIDs[0] != *invIDs["perceptor"] || d.NotifyMessageID == nil || *d.NotifyMessageID != 777 ||
		!strings.Contains(d.HeaderText, "• sAlpha: no report (declined: not enough public signals)") {
		t.Errorf("delivery = %+v; want only investigation %d, message 777 and the decline in the header", d, *invIDs["perceptor"])
	}
	var delivered bool
	if err := st.Pool.QueryRow(ctx, `SELECT delivered FROM scout_investigations_v WHERE id = $1`, *invIDs["salpha"]).Scan(&delivered); err != nil || delivered {
		t.Errorf("the decline reads as delivered = %v (%v), want false", delivered, err)
	}
	fd, err := st.FirstSentDelivery(ctx, call)
	if err != nil || fd == nil || fd.NotifyMessageID == nil || *fd.NotifyMessageID != 777 || fd.Target != "me" {
		t.Errorf("FirstSentDelivery = %+v, %v; want message 777 to me", fd, err)
	}
	if fd, err := st.FirstSentDelivery(ctx, call+1000); err != nil || fd != nil {
		t.Errorf("FirstSentDelivery of a call never delivered = %+v, %v; want nil", fd, err)
	}
}

// FirstSentDelivery takes only status sent: a failed delivery and a live
// Perceptor re-check delivery still at sending (recorded before its send, its
// outcome never stored) are not sent. Once the re-check's outcome is stored
// (FinishScoutDelivery) with the header's message id, the follow-up replies
// to it.
func TestFirstSentDeliverySkipsSendingAndFailed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ca := "0x3333333333333333333333333333333333333333"
	call := seedRescanCall(t, st, rescanSeed{msg: 3, ca: ca, at: time.Now().Add(-7 * time.Hour), status: CallStatusScanned})
	insert := func(status string, msgID *int64) int {
		t.Helper()
		id, err := st.InsertScoutDelivery(ctx, &ScoutDelivery{CallID: &call, ContractAddress: ca, Target: "me", Status: status,
			HeaderText: "h", NotifyMessageID: msgID})
		if err != nil {
			t.Fatal(err)
		}
		return *id
	}
	failedID := int64(11)
	insert(DeliveryStatusFailed, &failedID) // the header went out, a later part failed
	sending := insert(DeliveryStatusSending, nil)
	if d, err := st.FirstSentDelivery(ctx, call); err != nil || d != nil {
		t.Fatalf("FirstSentDelivery with a failed and a sending delivery = %+v, %v; want nil", d, err)
	}
	at := time.Now().UTC()
	msgID := int64(5555)
	if err := st.FinishScoutDelivery(ctx, sending, DeliveryStatusSent, &at, nil, &msgID); err != nil {
		t.Fatal(err)
	}
	d, err := st.FirstSentDelivery(ctx, call)
	if err != nil || d == nil || d.ID == nil || *d.ID != sending || d.NotifyMessageID == nil || *d.NotifyMessageID != msgID {
		t.Fatalf("FirstSentDelivery after the re-check delivery was sent = %+v, %v; want delivery %d with message %d", d, err, sending, msgID)
	}
	// a failed re-check delivery keeps NULL when no message id is known
	other := insert(DeliveryStatusSending, nil)
	if err := st.FinishScoutDelivery(ctx, other, DeliveryStatusFailed, nil, strPtr("boom"), nil); err != nil {
		t.Fatal(err)
	}
	ds, err := st.SelectScoutDeliveries(ctx, &call, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if *d.ID == other && (d.Status != DeliveryStatusFailed || d.NotifyMessageID != nil) {
			t.Errorf("failed re-check delivery = %+v, want failed with notify_message_id NULL", d)
		}
	}
}

// A re-ask through the real scanner writes one scan_kind 'reask' row and the
// follow-up delivery, and nothing else: no status or tracking change, no
// seen_cas.json entry, no score, no queued job.
func TestReaskLaneSideEffects(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: dir,
		Tools: []ToolSpec{testPerceptorSpec(), testSAlphaSpec()}, NotifyPeer: "me", DeliverLevels: map[string]bool{levelClean: true}}
	s := newScanner(cfg)
	s.db = st
	if err := s.registerTools(ctx); err != nil {
		t.Fatal(err)
	}
	seenFile := filepath.Join(dir, "seen_cas.json")
	if err := os.WriteFile(seenFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	saID := *s.runnerFor("salpha").toolID
	ca := "0x1111111111111111111111111111111111111111"
	no := false
	callAt := time.Now().UTC().Add(-7 * time.Hour).Truncate(time.Second)
	call := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: callAt, status: CallStatusScanned, track: true, rugged: &no})
	liveID := seedSAlpha(t, st, saID, &call, ca, callAt.Add(time.Minute), investigationCompleted, ScanKindLive, testDecline)
	s.recordDelivery(job{CA: ca, CallID: &call}, "header", nil, 4242, nil)
	state := func() string {
		t.Helper()
		var s string
		if err := st.Pool.QueryRow(ctx, `SELECT row_to_json(c)::text || (SELECT row_to_json(t)::text FROM scout_call_tracking t WHERE t.call_id = c.id)
			|| (SELECT count(*) FROM scout_call_predictions)::text
			FROM scout_calls c WHERE c.id = $1`, call).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()

	lane := s.newReaskLane(reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute})
	if lane == nil {
		t.Fatal("newReaskLane(on) = nil with a database and the salpha tool")
	}
	const report = "sAlpha research: 3 smart wallets bought."
	var sent []string
	lane.send = replyingSend(report, nil, &sent)
	var follow []reaskFollowUp
	lane.followUp = func(ctx context.Context, f reaskFollowUp) (int64, error) {
		follow = append(follow, f)
		return 9000, nil
	}
	lane.noteLive(time.Now().Add(-time.Hour))
	if !lane.step(ctx, st, func() int { return len(s.queue) }) {
		t.Fatal("step() = false, want a re-ask")
	}
	if after := state(); after != before {
		t.Errorf("call, tracking or predictions changed:\nbefore %s\n after %s", before, after)
	}
	if b, err := os.ReadFile(seenFile); err != nil || string(b) != `{}` {
		t.Errorf("seen_cas.json = %q (%v), want it untouched", b, err)
	}
	if len(s.queue) != 0 || len(s.pending) != 0 {
		t.Errorf("queue %d, pending %d; want both empty", len(s.queue), len(s.pending))
	}
	invs, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{})
	if err != nil || len(invs) != 2 {
		t.Fatalf("%d investigations (%v), want the live one and the re-ask", len(invs), err)
	}
	r := invs[0]
	var d map[string]any
	if err := json.Unmarshal(r.Details, &d); err != nil {
		t.Fatal(err)
	}
	if r.ScanKind != ScanKindReask || r.CallID == nil || *r.CallID != call || r.Status != investigationCompleted || r.ReportText != report ||
		d["reask"] != true || d["call_reply"] != reaskReasonDeclined || d["call_age_s"] == nil {
		t.Errorf("re-ask = %+v, details %s; want a completed reask of call %d", r, r.Details, call)
	}
	if len(follow) != 1 || follow[0].ReplyTo != 4242 {
		t.Fatalf("follow-ups %+v, want one reply to 4242", follow)
	}
	ds, err := st.SelectScoutDeliveries(ctx, &call, 10)
	if err != nil || len(ds) != 2 {
		t.Fatalf("deliveries %+v (%v), want the call's and the follow-up", ds, err)
	}
	fu := ds[0]
	if fu.Status != DeliveryStatusSent || len(fu.InvestigationIDs) != 1 || fu.InvestigationIDs[0] != *r.ID ||
		fu.NotifyMessageID == nil || *fu.NotifyMessageID != 9000 || !strings.HasPrefix(fu.HeaderText, "🕐 sAlpha report (asked again 7h after the call)") {
		t.Errorf("follow-up delivery = %+v; want sent, linking re-ask %d, message 9000", fu, *r.ID)
	}
	// the website now shows the re-ask's report (a real one beats the live decline)
	rows, _, err := st.SelectWebRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].SAlphaID == nil || *rows[0].SAlphaID != *r.ID {
		t.Errorf("web rows %+v (%v); want the re-ask %d, not the live decline %d", rows, err, *r.ID, liveID)
	}
	// once per token: nothing left, even after the gap
	if c, err := st.NextReaskCandidate(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-6*time.Hour)); err != nil || c != nil {
		t.Errorf("NextReaskCandidate after the re-ask = %+v, %v; want none", c, err)
	}
}
