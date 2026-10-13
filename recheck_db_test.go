package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

// recheckFixture is a migrated test database with Perceptor (gate) and sAlpha,
// a scanner for them, and what the fake delivery received.
type recheckFixture struct {
	st        *ScoutStore
	perc, sal int
	s         *scanner

	mu      sync.Mutex
	headers []string
	tools   [][]string // per delivery: the tools delivered, in order
}

func newRecheckFixture(t *testing.T) *recheckFixture {
	t.Helper()
	st := testStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &recheckFixture{st: st}
	f.perc = mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	f.sal = mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	f.s = &scanner{cfg: &config{SourceChannel: "scoutrobinhood", NotifyPeer: "me",
		DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}},
		db: st, logf: filepath.Join(t.TempDir(), "scans.jsonl")}
	pr := newToolRunner(testPerceptorSpec())
	pr.toolID = &f.perc
	sr := newToolRunner(builtinTools["salpha"])
	sr.toolID = &f.sal
	f.s.runners = []*toolRunner{pr, sr}
	return f
}

// lane is a re-check lane as a fresh listener would make it (nothing in
// memory): Perceptor replies reply (no reply at all when reply is ""), and
// its report reads as judged.
func (f *recheckFixture) lane(reply string, judged verdict, sent *[]string) *recheckLane {
	send := replyingSend(reply, nil, sent)
	if reply == "" {
		send = func(ctx context.Context, r *toolRunner, cmd string) error {
			*sent = append(*sent, cmd)
			return nil
		}
	}
	l := &recheckLane{cfg: recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour}, s: f.s, db: f.st,
		runner: f.s.runners[0], toolID: f.perc, now: time.Now, send: send,
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict { return judged },
		deliver: func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.headers = append(f.headers, header)
			var tools []string
			for _, r := range results {
				tools = append(tools, r.Spec.Code)
			}
			f.tools = append(f.tools, tools)
			// the header's message id in the delivery chat: 7001, 7002, …
			return int64(recheckFakeMsgID + len(f.headers)), nil
		}}
	l.init()
	return l
}

// call stores a first call posted age ago (status scanned) with its live
// Perceptor result (level) and a live sAlpha report, the Perceptor result
// marked held for the re-check (as process() marks it) unless level is a real
// verdict; it returns the call id.
func (f *recheckFixture) call(t *testing.T, msg int, ca string, age time.Duration, level string) int {
	t.Helper()
	id, inv := f.unheldCall(t, msg, ca, age, level)
	if !verdictUsable(level) {
		if err := f.st.MarkHeldForRecheck(context.Background(), inv, ""); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// unheldCall is call without the held marker (as an older binary, another
// gate's "no", -scan / -post or a delivery at the call leave it); it returns
// the call id and its live Perceptor investigation's id.
func (f *recheckFixture) unheldCall(t *testing.T, msg int, ca string, age time.Duration, level string) (int, int) {
	t.Helper()
	at := time.Now().Add(-age)
	id := seedRescanCall(t, f.st, rescanSeed{msg: msg, ca: ca, at: at, status: CallStatusScanned})
	inv := seedInvestigation(t, f.st, f.perc, &id, ca, at.Add(time.Minute), investigationCompleted, ScanKindLive, level)
	done := at.Add(2 * time.Minute)
	if _, err := f.st.InsertScoutInvestigation(context.Background(), &ScoutInvestigation{CallID: &id, ToolID: f.sal,
		ContractAddress: ca, RequestText: ca, RequestedAt: at.Add(time.Minute), CompletedAt: &done,
		Status: investigationCompleted, BotMessageIDs: []int32{5, 6}, ReportText: "sAlpha research"}); err != nil {
		t.Fatal(err)
	}
	return id, inv
}

// held reports whether investigation id carries the held marker.
func (f *recheckFixture) held(t *testing.T, id int) bool {
	t.Helper()
	var held bool
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(details->>'held_for_recheck' = 'true', false) FROM scout_investigations WHERE id = $1`, id).Scan(&held); err != nil {
		t.Fatal(err)
	}
	return held
}

// recheckRow stores a re-check of a call (n = 1: live, 2: rescan).
func (f *recheckFixture) recheckRow(t *testing.T, callID int, ca string, at time.Time, n int, level string) int {
	t.Helper()
	kind := ScanKindLive
	if n == 2 {
		kind = ScanKindRescan
	}
	id, err := f.st.InsertScoutInvestigation(context.Background(), &ScoutInvestigation{CallID: &callID, ToolID: f.perc,
		ContractAddress: ca, RequestText: "/scan " + ca, RequestedAt: at, CompletedAt: &at, Status: investigationCompleted,
		VerdictLevel: level, ScanKind: kind, Details: recheckDetails(nil, n, 12*time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return *id
}

// percRows returns the call's Perceptor investigations, oldest first.
func (f *recheckFixture) percRows(t *testing.T, callID int) []ScoutInvestigation {
	t.Helper()
	rows, err := f.st.SelectScoutInvestigations(context.Background(), ScoutInvestigationFilter{CallID: &callID, ToolCodes: []string{"perceptor"}})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rows)
	return rows
}

func (f *recheckFixture) deliveries(t *testing.T, callID int) []ScoutDelivery {
	t.Helper()
	d, err := f.st.SelectScoutDeliveries(context.Background(), &callID, 10)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// recheckFakeMsgID + n is the message id the fixture's deliver returns for
// its n-th delivery.
const recheckFakeMsgID = 7000

const recheckReply = "$SNIFF on-chain check: report https://www.perceptor.info/r/0fc1598d6fc24311b4437066528e214b"

// What is due is read from the database: a fresh lane (a restarted listener)
// re-checks the call that is due, leaves the others, and never repeats a
// re-check that is stored.
func TestRecheckScheduleFromDatabase(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := func(c byte) string { return "0x" + strings.Repeat(string(c), 40) }
	due := f.call(t, 1, ca('a'), 13*time.Minute, levelInsufficient)
	f.call(t, 2, ca('b'), 5*time.Minute, levelUnknown)                    // not due yet
	f.call(t, 3, ca('c'), 70*time.Minute, levelInsufficient)              // window passed (and the second is not due)
	f.call(t, 4, ca('d'), 20*time.Minute, levelRedFlags)                  // a real verdict
	sent1, _ := f.unheldCall(t, 5, ca('e'), 20*time.Minute, levelUnknown) // delivered at the call (SCOUT_DELIVER_LEVELS had unknown)
	if _, err := f.st.InsertScoutDelivery(ctx, &ScoutDelivery{CallID: &sent1, ContractAddress: ca('e'), Target: "me", Status: DeliveryStatusSent}); err != nil {
		t.Fatal(err)
	}
	done := f.call(t, 6, ca('f'), 20*time.Minute, levelInsufficient) // re-checked and delivered already (before the restart)
	f.recheckRow(t, done, ca('f'), time.Now().Add(-8*time.Minute), 1, levelInsufficient)
	if _, err := f.st.InsertScoutDelivery(ctx, &ScoutDelivery{CallID: &done, ContractAddress: ca('f'), Target: "me", Status: DeliveryStatusSent}); err != nil {
		t.Fatal(err)
	}
	red := f.call(t, 9, ca('i'), 20*time.Minute, levelInsufficient) // re-checked already: red flags, not delivered
	f.recheckRow(t, red, ca('i'), time.Now().Add(-8*time.Minute), 1, levelRedFlags)

	var sent []string
	l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
	if !l.step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want the due re-check")
	}
	if want := []string{"/scan " + ca('a')}; !slices.Equal(sent, want) {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	if rows := f.percRows(t, due); len(rows) != 2 || rows[1].ScanKind != ScanKindLive || rows[1].VerdictLevel != levelClean {
		t.Fatalf("call %d Perceptor rows %+v, want the first answer and a live clean re-check", due, rows)
	}
	// the same process, and a restarted one: nothing else is due
	for i, l := range []*recheckLane{l, f.lane(recheckReply, verdict{Level: levelClean}, &sent)} {
		if l.step(ctx, func() int { return 0 }) || len(sent) != 1 {
			t.Errorf("lane %d: a second step re-checked again (sent %q)", i, sent)
		}
	}
	// a queued live call goes first
	if f.lane(recheckReply, verdict{Level: levelClean}, &sent).step(ctx, func() int { return 1 }) {
		t.Error("step() with a queued live call = true, want false")
	}
	// a call handled live long after it was posted (catch-up) is held back,
	// but marked as past its window at once
	f.s.recheck = f.lane(recheckReply, verdict{Level: levelClean}, &sent)
	late, lateInv := f.unheldCall(t, 7, ca('g'), 4*time.Hour, levelInsufficient)
	res := []*toolResult{{Spec: f.s.runners[0].spec, Status: investigationCompleted, Verdict: verdict{Level: levelInsufficient}}}
	if !f.s.deferToRecheck(ctx, job{CA: ca('g'), CallID: &late}, res, &lateInv, "Model v1 · 7d: runner 9%") || !f.s.recheck.missed[recheckKey(late, 1)] {
		t.Errorf("late call: deferToRecheck false or not marked missed (%v)", f.s.recheck.missed)
	}
	if got := f.s.recheck.scoreLine(late); got != "" {
		t.Errorf("late call: score line kept %q, want none (it is never delivered)", got)
	}
	fresh, freshInv := f.unheldCall(t, 8, ca('h'), 2*time.Minute, levelInsufficient)
	if !f.s.deferToRecheck(ctx, job{CA: ca('h'), CallID: &fresh}, res, &freshInv, "Model v1 · 7d: runner 12%") || f.s.recheck.missed[recheckKey(fresh, 1)] {
		t.Errorf("fresh call: deferToRecheck false or marked missed (%v)", f.s.recheck.missed)
	}
	if got, want := f.s.recheck.scoreLine(fresh), "Model v1 · 7d: runner 12%"; got != want {
		t.Errorf("fresh call: score line kept %q, want %q", got, want)
	}
	if !f.held(t, lateInv) || !f.held(t, freshInv) {
		t.Errorf("held markers: late %v, fresh %v; want both written", f.held(t, lateInv), f.held(t, freshInv))
	}
}

// The first re-check decides the delivery. One without a report (no reply,
// a failed send) stores nothing and is tried again while the window lasts;
// the last try that fits it is stored and delivered with the label.
func TestRecheckDeliveryOutcomes(t *testing.T) {
	ca := "0x2222222222222222222222222222222222222222"
	sendErr := errors.New("network down")
	cases := []struct {
		name      string
		reply     string
		sendErr   error
		age       time.Duration // of the call
		judged    verdict
		status    string // stored status of the re-check ("" = nothing stored, retried later)
		level     string // stored level of the re-check
		delivered bool
		first     string // the header's first line
	}{
		{"clean", recheckReply, nil, 13 * time.Minute, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"},
			investigationCompleted, levelClean, true, "✅ $SNIFF No red flags found"},
		{"caution", recheckReply, nil, 13 * time.Minute, verdict{Level: levelCaution, Label: "Caution", Ticker: "$SNIFF"},
			investigationCompleted, levelCaution, true, "🟡 $SNIFF Caution"},
		{"red flags", recheckReply, nil, 13 * time.Minute, verdict{Level: levelRedFlags, Label: "Red flags", Ticker: "$SNIFF"},
			investigationCompleted, levelRedFlags, false, ""},
		{"still insufficient", recheckReply, nil, 13 * time.Minute, verdict{Level: levelInsufficient, Label: labelInsufficient, Ticker: "$SNIFF"},
			investigationCompleted, levelInsufficient, true, "⏳ $SNIFF Perceptor: not enough data yet"},
		{"no reply, window open: retried later", "", nil, 13 * time.Minute, verdict{}, "", "", false, ""},
		{"send failed, window open: retried later", recheckReply, sendErr, 13 * time.Minute, verdict{}, "", "", false, ""},
		{"no reply, end of window", "", nil, 57 * time.Minute, verdict{}, investigationTimeout, levelUnknown, true, "❔ Perceptor: no report (timeout)"},
		{"send failed, end of window", recheckReply, sendErr, 57 * time.Minute, verdict{}, investigationFailed, levelUnknown, true, "❔ Perceptor: no report (failed)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			ctx := context.Background()
			call := f.call(t, 10, ca, c.age, levelInsufficient)
			var sent []string
			l := f.lane(c.reply, c.judged, &sent)
			if c.sendErr != nil {
				l.send = replyingSend(c.reply, c.sendErr, &sent)
			}
			start := time.Now()
			if !l.step(ctx, func() int { return 0 }) {
				t.Fatal("step() = false, want a re-check")
			}
			rows := f.percRows(t, call)
			if c.status == "" {
				key := recheckKey(call, 1)
				if len(rows) != 1 || len(f.headers) != 0 || len(f.deliveries(t, call)) != 0 {
					t.Fatalf("%d Perceptor rows, %d deliveries; want the first answer only and none", len(rows), len(f.headers))
				}
				if l.tried[key] || l.retryAt[key].Before(start.Add(recheckRetryPause)) {
					t.Errorf("tried %v, retry at %s; want released, retried after %s", l.tried[key], l.retryAt[key], recheckRetryPause)
				}
				if l.step(ctx, func() int { return 0 }) || len(sent) != 1 {
					t.Errorf("a step before the retry time re-checked again (sent %q)", sent)
				}
				return
			}
			if len(rows) != 2 {
				t.Fatalf("%d Perceptor rows, want 2", len(rows))
			}
			re := rows[1]
			var d map[string]any
			if err := json.Unmarshal(re.Details, &d); err != nil || d["recheck"] != float64(1) {
				t.Errorf("re-check details %s (%v), want recheck 1", re.Details, err)
			}
			if age, _ := d["call_age_s"].(float64); age < c.age.Seconds() || age > c.age.Seconds()+60 {
				t.Errorf("call_age_s %v, want about %v", d["call_age_s"], c.age.Seconds())
			}
			if re.ScanKind != ScanKindLive || re.Status != c.status || re.VerdictLevel != c.level {
				t.Errorf("re-check row: scan_kind %s, status %s, level %s; want live, %s, %s", re.ScanKind, re.Status, re.VerdictLevel, c.status, c.level)
			}
			dels := f.deliveries(t, call)
			if !c.delivered {
				if len(f.headers) != 0 || len(dels) != 0 {
					t.Fatalf("delivered %d time(s), %d delivery rows; want none", len(f.headers), len(dels))
				}
				return
			}
			if len(f.headers) != 1 || len(dels) != 1 || dels[0].Status != DeliveryStatusSent {
				t.Fatalf("deliveries: %d sent, rows %+v; want one sent", len(f.headers), dels)
			}
			h := f.headers[0]
			lines := strings.Split(h, "\n")
			if lines[0] != c.first || lines[1] != "🔁 Perceptor re-checked "+rescanAgeText(c.age)+" after the call" {
				t.Errorf("header starts %q / %q, want %q / the re-check line:\n%s", lines[0], lines[1], c.first, h)
			}
			if dels[0].HeaderText != h {
				t.Errorf("stored header %q, want the one sent", dels[0].HeaderText)
			}
			if want := []string{"perceptor", "salpha"}; !slices.Equal(f.tools[0], want) {
				t.Errorf("delivered tools %q, want %q", f.tools[0], want)
			}
			// the re-check's report (when it completed) and the call's sAlpha report are attached
			var n int
			if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_delivery_investigations WHERE delivery_id = $1`, *dels[0].ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := 2
			if c.status != investigationCompleted {
				want = 1 // no report to attach
			}
			if n != want {
				t.Errorf("%d attached reports, want %d", n, want)
			}
		})
	}
}

// A re-check delivery of a call whose stored live sAlpha result is a decline
// treats it as the live delivery does: the header says it declined, only the
// Perceptor re-check is attached, and no sAlpha message goes with it. This
// holds also when the stored row has no bot message ids (the decline is read
// from its stored report text).
func TestRecheckDeliveryStoredSAlphaDecline(t *testing.T) {
	ca := "0x3333333333333333333333333333333333333333"
	for _, tc := range []struct {
		name string
		ids  []int32
	}{
		{"with bot messages", []int32{5, 6}},
		{"without bot messages", []int32{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			ctx := context.Background()
			call := f.call(t, 10, ca, 13*time.Minute, levelInsufficient)
			tag, err := f.st.Pool.Exec(ctx, `UPDATE scout_investigations SET report_text = $1, bot_message_ids = $2
				WHERE call_id = $3 AND tool_id = $4 AND scan_kind = 'live'`, testDecline, tc.ids, call, f.sal)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("seeding the sAlpha decline: %v (%d rows), want 1 row", err, tag.RowsAffected())
			}
			var sent []string
			l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
			fake := l.deliver
			var got []*toolResult
			l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
				got = results
				return fake(ctx, j, header, results)
			}
			if !l.step(ctx, func() int { return 0 }) {
				t.Fatal("step() = false, want a re-check")
			}
			dels := f.deliveries(t, call)
			if len(f.headers) != 1 || len(dels) != 1 || dels[0].Status != DeliveryStatusSent {
				t.Fatalf("deliveries: %d sent, rows %+v; want one sent", len(f.headers), dels)
			}
			h := f.headers[0]
			if want := "• sAlpha: no report (declined: " + salphaDeclineReason(testDecline) + ")"; !strings.Contains(h, want) {
				t.Errorf("header does not say %q:\n%s", want, h)
			}
			// only the Perceptor re-check is attached, not the decline
			rows := f.percRows(t, call)
			if len(rows) != 2 {
				t.Fatalf("%d Perceptor rows, want 2", len(rows))
			}
			linked, err := f.st.Pool.Query(ctx, `SELECT investigation_id FROM scout_delivery_investigations WHERE delivery_id = $1 ORDER BY investigation_id`, *dels[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			defer linked.Close()
			var ids []int
			for linked.Next() {
				var id int
				if err := linked.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if err := linked.Err(); err != nil {
				t.Fatal(err)
			}
			if want := []int{*rows[1].ID}; !slices.Equal(ids, want) {
				t.Errorf("attached investigations %v, want only the re-check %v", ids, want)
			}
			if !slices.Equal(dels[0].InvestigationIDs, []int{*rows[1].ID}) {
				t.Errorf("delivery's investigation ids %v, want only the re-check %d", dels[0].InvestigationIDs, *rows[1].ID)
			}
			// no sAlpha message is forwarded (user account) or copied (Bot API)
			var sal *toolResult
			for _, r := range got {
				if r.Spec.Code == "salpha" {
					sal = r
				}
			}
			if sal == nil {
				t.Fatalf("delivered results %v: no sAlpha result", f.tools[0])
			}
			if len(sal.Replies) != len(tc.ids) {
				t.Errorf("sAlpha result has %d replies, want %d (the stored bot message ids)", len(sal.Replies), len(tc.ids))
			}
			if !reportDeclined(sal) || forwardable(sal) {
				t.Errorf("sAlpha result: declined %v, forwardable %v; want declined, not forwardable (text %q)", reportDeclined(sal), forwardable(sal), sal.ReportText())
			}
			if txt := f.s.botAPIText(h, job{CA: ca}, got); strings.Contains(txt, "━━ sAlpha ━━") {
				t.Errorf("Bot API text copies the sAlpha decline:\n%s", txt)
			}
		})
	}
}

// The second re-check (2 h) only writes a 'rescan' row: no delivery.
func TestRecheckSecondIsRescan(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := "0x3333333333333333333333333333333333333333"
	call := f.call(t, 20, ca, 2*time.Hour+5*time.Minute, levelInsufficient)
	f.recheckRow(t, call, ca, time.Now().Add(-113*time.Minute), 1, levelInsufficient)
	var sent []string
	if !f.lane(recheckReply, verdict{Level: levelCaution, Label: "Caution"}, &sent).step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want the second re-check")
	}
	rows := f.percRows(t, call)
	if len(rows) != 3 {
		t.Fatalf("%d Perceptor rows, want 3", len(rows))
	}
	var d map[string]any
	if err := json.Unmarshal(rows[2].Details, &d); err != nil || d["recheck"] != float64(2) || rows[2].ScanKind != ScanKindRescan || rows[2].VerdictLevel != levelCaution {
		t.Errorf("second re-check: kind %s, level %s, details %s; want a caution rescan with recheck 2", rows[2].ScanKind, rows[2].VerdictLevel, rows[2].Details)
	}
	if len(f.headers) != 0 || len(f.deliveries(t, call)) != 0 {
		t.Errorf("the second re-check delivered (%d)", len(f.headers))
	}
	// the call-time verdict stays the first answer; the website shows the 2-hour one as "Perceptor today"
	var v *string
	if err := f.st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, call).Scan(&v); err != nil || v == nil || *v != levelInsufficient {
		t.Errorf("dataset perceptor_verdict %v (%v), want insufficient", strOrNil(v), err)
	}
	if again := f.lane(recheckReply, verdict{Level: levelCaution}, &sent); again.step(ctx, func() int { return 0 }) || len(sent) != 1 {
		t.Errorf("a third re-check ran (sent %q)", sent)
	}
}

// The website takes a real verdict over "not enough data yet" for the same
// call (the 12-minute re-check), and never the 2-hour one. The dataset view
// (a model input) and the Analytics "verdict at call" never take a re-check:
// they keep the answer known at the call.
func TestRecheckVerdictChoice(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	t0 := time.Now().Add(-5 * time.Hour)
	type want struct{ dataset, web, today, atCall string }
	cases := map[string]struct {
		setup func(call int, ca string)
		want  want
	}{
		"12m re-check caution": {func(call int, ca string) {
			f.recheckRow(t, call, ca, t0.Add(12*time.Minute), 1, levelCaution)
		}, want{levelInsufficient, levelCaution, "", levelUnknown}},
		"12m insufficient, 2h clean": {func(call int, ca string) {
			f.recheckRow(t, call, ca, t0.Add(12*time.Minute), 1, levelInsufficient)
			f.recheckRow(t, call, ca, t0.Add(2*time.Hour), 2, levelClean)
		}, want{levelInsufficient, levelInsufficient, levelClean, levelUnknown}},
		"a later insufficient live scan": {func(call int, ca string) {
			// e.g. a re-check stored with a real verdict, then an insufficient live scan of the same call
			f.recheckRow(t, call, ca, t0.Add(12*time.Minute), 1, levelRedFlags)
			seedInvestigation(t, f.st, f.perc, &call, ca, t0.Add(30*time.Minute), investigationCompleted, ScanKindLive, levelInsufficient)
		}, want{levelInsufficient, levelRedFlags, "", levelUnknown}},
		"only insufficient": {func(call int, ca string) {}, want{levelInsufficient, levelInsufficient, "", levelUnknown}},
	}
	ids := map[string]int{}
	i := 0
	for name, c := range cases {
		i++
		ca := "0x" + strings.Repeat(string(rune('a'+i)), 40)
		at := t0.Add(-time.Minute)
		call := seedRescanCall(t, f.st, rescanSeed{msg: 100 + i, ca: ca, at: at, status: CallStatusScanned})
		seedInvestigation(t, f.st, f.perc, &call, ca, t0, investigationCompleted, ScanKindLive, levelInsufficient)
		c.setup(call, ca)
		ids[name] = call
	}
	rows, _, err := f.st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int]ScoutWebRow{}
	for _, r := range rows {
		byID[r.CallID] = r
	}
	for name, c := range cases {
		call := ids[name]
		var v *string
		if err := f.st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, call).Scan(&v); err != nil {
			t.Fatal(err)
		}
		r := byID[call]
		got := want{strOrNil(v), strOrNil(r.PerceptorVerd), strOrNil(r.PerceptorTodayVerd), strOrNil(r.VerdictAtCall)}
		if c.want.today == "" {
			c.want.today = "<nil>"
		}
		if got != c.want {
			t.Errorf("%s: dataset, web, today, at call = %+v, want %+v", name, got, c.want)
		}
	}
}

// The relabel in scoutanalytics.sql touches only completed Perceptor rows
// still unknown whose text says "not enough data yet", and nothing else of
// them; running it again changes nothing.
func TestRelabelInsufficient(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	text := insufficientReplies[0]
	src := "telegram_text"
	at := time.Now().Add(-48 * time.Hour)
	add := func(tool int, status, level, kind, report string) int {
		id, err := f.st.InsertScoutInvestigation(ctx, &ScoutInvestigation{ToolID: tool, ContractAddress: "0x4444444444444444444444444444444444444444",
			RequestText: "/scan x", RequestedAt: at, CompletedAt: &at, Status: status, VerdictLevel: level, ScanKind: kind,
			ReportText: report, VerdictSource: &src, Ticker: strPtr("$OLD")})
		if err != nil {
			t.Fatal(err)
		}
		return *id
	}
	type row struct{ level, label string }
	cases := []struct {
		id   int
		want row
	}{
		{add(f.perc, investigationCompleted, levelUnknown, ScanKindLive, text), row{levelInsufficient, labelInsufficient}},
		{add(f.perc, investigationCompleted, levelUnknown, ScanKindRescan, strings.ToUpper(insufficientReplies[2])), row{levelInsufficient, labelInsufficient}},
		{add(f.perc, investigationCompleted, levelUnknown, ScanKindLive, "Token not found"), row{levelUnknown, "<nil>"}},
		{add(f.perc, investigationTimeout, levelUnknown, ScanKindLive, text), row{levelUnknown, "<nil>"}},
		{add(f.sal, investigationCompleted, levelUnknown, ScanKindLive, text), row{levelUnknown, "<nil>"}},
		{add(f.perc, investigationCompleted, levelRedFlags, ScanKindLive, text+" red flags"), row{levelRedFlags, "<nil>"}},
	}
	snapshot := func() map[int]string {
		out := map[int]string{}
		r, err := f.st.Pool.Query(ctx, `SELECT id, concat_ws('|', status, scan_kind, report_text, ticker, verdict_source, updated_at::text, updated_by)
			FROM scout_investigations`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var id int
			var s string
			if err := r.Scan(&id, &s); err != nil {
				t.Fatal(err)
			}
			out[id] = s
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	for i := 0; i < 2; i++ {
		if err := f.st.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
		for _, c := range cases {
			var level string
			var label *string
			if err := f.st.Pool.QueryRow(ctx, `SELECT verdict_level, verdict_label FROM scout_investigations WHERE id = $1`, c.id).Scan(&level, &label); err != nil {
				t.Fatal(err)
			}
			if got := (row{level, strOrNil(label)}); got != c.want {
				t.Errorf("migrate %d, row %d: got %+v, want %+v", i+1, c.id, got, c.want)
			}
		}
	}
	if after := snapshot(); !maps.Equal(before, after) {
		t.Errorf("the relabel changed other columns:\nbefore %v\nafter  %v", before, after)
	}
}

// Rescan candidates: a first call whose Perceptor answers are all
// insufficient or unknown has no report; a completed rescan without a real
// verdict counts as failed; the lane leaves calls the re-check may still take.
func TestRescanCandidatesInsufficient(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	now := time.Now()
	ca := func(c byte) string { return "0x" + strings.Repeat(string(c), 40) }
	seed := func(msg int, c byte, age time.Duration, live string, rescans ...string) int {
		at := now.Add(-age)
		id := seedRescanCall(t, f.st, rescanSeed{msg: msg, ca: ca(c), at: at, status: CallStatusScanned})
		seedInvestigation(t, f.st, f.perc, &id, ca(c), at.Add(time.Minute), investigationCompleted, ScanKindLive, live)
		for i, lvl := range rescans {
			seedInvestigation(t, f.st, f.perc, &id, ca(c), at.Add(time.Duration(i+2)*time.Hour), investigationCompleted, ScanKindRescan, lvl)
		}
		return id
	}
	p := seed(1, 'p', time.Hour, levelInsufficient)
	q := seed(2, 'q', 2*time.Hour, levelUnknown)
	seed(3, 'r', 3*time.Hour, levelClean)
	s := seed(4, 's', 4*time.Hour, levelInsufficient, levelInsufficient)
	tt := seed(5, 't', 5*time.Hour, levelInsufficient, levelInsufficient, levelUnknown)
	seed(6, 'u', 6*time.Hour, levelInsufficient, levelCaution)

	pool, err := f.st.SelectRescanPool(ctx, []string{CallStatusScanned})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]int{}
	for _, r := range pool {
		got[r.CallID] = r.FailedRescans
	}
	if want := map[int]int{p: 0, q: 0, s: 1, tt: 2}; !maps.Equal(got, want) {
		t.Errorf("pool (call → failed rescans) = %v, want %v", got, want)
	}
	since := now.Add(-30 * 24 * time.Hour)
	c, err := f.st.NextRescanCandidate(ctx, []string{CallStatusScanned}, since, rescanFailedLimit)
	if err != nil || c == nil || c.CallID != p {
		t.Errorf("next candidate %+v (%v), want call %d", c, err, p)
	}
	c, err = f.st.NextRescanCandidateBefore(ctx, []string{CallStatusScanned}, since, now.Add(-90*time.Minute), rescanFailedLimit)
	if err != nil || c == nil || c.CallID != q {
		t.Errorf("next candidate before 90m ago %+v (%v), want call %d", c, err, q)
	}
	c, err = f.st.NextRescanCandidateBefore(ctx, []string{CallStatusScanned}, since, now.Add(-4*time.Hour-30*time.Minute), rescanFailedLimit)
	if err != nil || c != nil {
		t.Errorf("next candidate before 4h30m ago %+v (%v), want none (t failed twice)", c, err)
	}
}

type (
	recheckSendFunc  = func(context.Context, *toolRunner, string) error
	recheckJudgeFunc = func(context.Context, ToolSpec, *toolResult) verdict
)

// A stop (reconnect or shutdown) during a re-check stores nothing and
// releases it: the next step, in the same process, re-checks the call again.
func TestRecheckStopPaths(t *testing.T) {
	ca := "0x5555555555555555555555555555555555555555"
	cases := []struct {
		name string
		// stop returns the send and judge (nil: the lane's) of a first try
		// that cancel stops
		stop func(cancel context.CancelFunc, sent *[]string) (recheckSendFunc, recheckJudgeFunc)
	}{
		{"before the report came", func(cancel context.CancelFunc, sent *[]string) (recheckSendFunc, recheckJudgeFunc) {
			return func(ctx context.Context, r *toolRunner, cmd string) error {
				*sent = append(*sent, cmd)
				cancel() // no reply: the stop comes while waiting for it
				return nil
			}, nil
		}},
		{"while reading the report", func(cancel context.CancelFunc, sent *[]string) (recheckSendFunc, recheckJudgeFunc) {
			return replyingSend(recheckReply, nil, sent), func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
				cancel()
				return verdict{Level: levelClean}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			call := f.call(t, 30, ca, 13*time.Minute, levelInsufficient)
			var sent []string
			l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
			okSend, okJudge := l.send, l.judge
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			send, judge := c.stop(cancel, &sent)
			l.send = send
			if judge != nil {
				l.judge = judge
			}
			if !l.step(ctx, func() int { return 0 }) {
				t.Fatal("step() = false, want a re-check")
			}
			if l.tried[recheckKey(call, 1)] {
				t.Errorf("after a stop %s the re-check is still marked tried; want released", c.name)
			}
			if rows := f.percRows(t, call); len(rows) != 1 || len(f.headers) != 0 || len(f.deliveries(t, call)) != 0 {
				t.Fatalf("after a stop: %d Perceptor rows, %d deliveries; want the first answer only and none", len(rows), len(f.headers))
			}
			// the reconnected worker: a new ctx, the same lane
			l.send, l.judge = okSend, okJudge
			if !l.step(context.Background(), func() int { return 0 }) || len(sent) != 2 {
				t.Fatalf("step after the stop: sent %q, want a second /scan", sent)
			}
			if rows := f.percRows(t, call); len(rows) != 2 || rows[1].VerdictLevel != levelClean || len(f.headers) != 1 {
				t.Errorf("after the retry: %d Perceptor rows, %d deliveries; want a clean re-check delivered once", len(rows), len(f.headers))
			}
		})
	}
}

// A Telegram FLOOD_WAIT stores nothing and pauses the lane for Telegram's
// wait; the re-check runs after the pause.
func TestRecheckFloodWait(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := "0x6666666666666666666666666666666666666666"
	call := f.call(t, 40, ca, 13*time.Minute, levelInsufficient)
	var sent []string
	l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
	ok := l.send
	clock := time.Now()
	l.now = func() time.Time { return clock }
	l.send = replyingSend("", fmt.Errorf("send: %w", tgerr.New(420, "FLOOD_WAIT_600")), &sent)
	if !l.step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want a re-check")
	}
	key := recheckKey(call, 1)
	if want := clock.Add(600 * time.Second); !l.pausedUntil.Equal(want) || l.tried[key] {
		t.Errorf("paused until %s, tried %v; want %s, released", l.pausedUntil, l.tried[key], want)
	}
	if rows := f.percRows(t, call); len(rows) != 1 {
		t.Fatalf("%d Perceptor rows after FLOOD_WAIT, want 1 (nothing stored)", len(rows))
	}
	l.send = ok
	clock = clock.Add(5 * time.Minute)
	if l.step(ctx, func() int { return 0 }) || len(sent) != 1 {
		t.Errorf("step during the pause re-checked (sent %q)", sent)
	}
	clock = clock.Add(5*time.Minute + time.Second)
	if !l.step(ctx, func() int { return 0 }) || len(sent) != 2 {
		t.Fatalf("step after the pause: sent %q, want a second /scan", sent)
	}
	if rows := f.percRows(t, call); len(rows) != 2 || len(f.headers) != 1 {
		t.Errorf("after the pause: %d Perceptor rows, %d deliveries; want the re-check stored and delivered", len(rows), len(f.headers))
	}
}

// A stop before the delivery sent anything records no delivery: the stored
// re-check is delivered on a later step, without asking Perceptor again. A
// failure after something may have been sent is recorded and never sent again.
func TestRecheckDeliveryStopped(t *testing.T) {
	ca := "0x7777777777777777777777777777777777777777"
	t.Run("nothing sent: delivered later", func(t *testing.T) {
		f := newRecheckFixture(t)
		call := f.call(t, 50, ca, 13*time.Minute, levelInsufficient)
		var sent []string
		l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
		ok := l.deliver
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
			cancel()
			return 0, &deliveryNotSentError{fmt.Errorf("not sent: %w", ctx.Err())}
		}
		if !l.step(ctx, func() int { return 0 }) {
			t.Fatal("step() = false, want a re-check")
		}
		if d := f.deliveries(t, call); len(d) != 0 || l.tried[recheckKey(call, 1)] {
			t.Fatalf("after a stop before sending: %d delivery rows, tried %v; want none, released", len(d), l.tried[recheckKey(call, 1)])
		}
		if rows := f.percRows(t, call); len(rows) != 2 {
			t.Fatalf("%d Perceptor rows, want the re-check stored", len(rows))
		}
		l.deliver = ok
		if !l.step(context.Background(), func() int { return 0 }) {
			t.Fatal("step() after the stop = false, want the stored re-check delivered")
		}
		d := f.deliveries(t, call)
		if len(sent) != 1 || len(f.headers) != 1 || len(d) != 1 || d[0].Status != DeliveryStatusSent {
			t.Fatalf("sent %q, %d deliveries, rows %+v; want one /scan and one sent delivery", sent, len(f.headers), d)
		}
		// the header's message id is stored, so a sAlpha re-ask can reply to it
		if d[0].NotifyMessageID == nil || *d[0].NotifyMessageID != recheckFakeMsgID+1 {
			t.Errorf("notify_message_id of the re-check delivery = %v, want %d", d[0].NotifyMessageID, recheckFakeMsgID+1)
		}
		if lines := strings.Split(f.headers[0], "\n"); lines[0] != "✅ $SNIFF No red flags found" || !strings.HasPrefix(lines[1], "🔁 Perceptor re-checked 13m") {
			t.Errorf("header %q, want the stored clean verdict and the re-check line", f.headers[0])
		}
		if !slices.Equal(f.tools[0], []string{"perceptor", "salpha"}) {
			t.Errorf("delivered tools %q, want perceptor, salpha", f.tools[0])
		}
		// a restarted listener does nothing more
		if f.lane(recheckReply, verdict{Level: levelClean}, &sent).step(context.Background(), func() int { return 0 }) || len(f.headers) != 1 || len(sent) != 1 {
			t.Errorf("a fresh lane delivered or re-checked again (sent %q, %d deliveries)", sent, len(f.headers))
		}
	})
	t.Run("stopped while sending: recorded, not sent again", func(t *testing.T) {
		f := newRecheckFixture(t)
		call := f.call(t, 51, ca, 13*time.Minute, levelInsufficient)
		var sent []string
		l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		calls := 0
		l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
			calls++
			cancel()
			return 0, fmt.Errorf("send original call: %w", ctx.Err())
		}
		l.keepScore(call, recheckScoreLine)
		if !l.step(ctx, func() int { return 0 }) {
			t.Fatal("step() = false, want a re-check")
		}
		if d := f.deliveries(t, call); len(d) != 1 || d[0].Status != DeliveryStatusFailed || d[0].NotifyMessageID != nil {
			t.Fatalf("delivery rows %+v, want one failed without a message id", d)
		}
		if got := l.scoreLine(call); got != "" {
			t.Errorf("score line after a failed send %q, want it forgotten (the delivery is never sent again)", got)
		}
		if l.step(context.Background(), func() int { return 0 }) || calls != 1 || len(sent) != 1 {
			t.Errorf("after a stop in the middle of sending: deliver called %d times, sent %q; want once, one /scan", calls, sent)
		}
	})
}

// The dataset view (a model input) never returns a re-check's verdict: not for
// the call it was made for, nor for a later call of the same token through
// the "latest scan at or before the call" branch.
func TestDatasetViewIgnoresRecheck(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := "0x8888888888888888888888888888888888888888"
	t0 := time.Now().Add(-5 * time.Hour)
	first := seedRescanCall(t, f.st, rescanSeed{msg: 200, ca: ca, at: t0, status: CallStatusScanned})
	seedInvestigation(t, f.st, f.perc, &first, ca, t0.Add(time.Minute), investigationCompleted, ScanKindLive, levelInsufficient)
	f.recheckRow(t, first, ca, t0.Add(12*time.Minute), 1, levelClean)
	later := seedRescanCall(t, f.st, rescanSeed{msg: 201, ca: ca, at: t0.Add(time.Hour), status: CallStatusScanned})
	for _, c := range []struct {
		name string
		call int
	}{{"the re-checked call", first}, {"a later call of the token", later}} {
		var v *string
		if err := f.st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, c.call).Scan(&v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strOrNil(v) != levelInsufficient {
			t.Errorf("%s (call %d): dataset perceptor_verdict %s, want insufficient (the first answer, never the clean re-check)", c.name, c.call, strOrNil(v))
		}
	}
	var n int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_dataset_v WHERE contract_address = $1 AND perceptor_verdict = 'clean'`, ca).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d dataset rows with the re-check's clean verdict (%v), want 0", n, err)
	}
}

// A first re-check released in this process without storing anything (a
// retry, a stop, FLOOD_WAIT) still runs once when its window ended before the
// next try (busy queue, pause, reconnect), and is stored and delivered with
// the label; a call never tried here keeps the downtime rule (skipped).
func TestRecheckOwedAfterWindow(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	owedCA, otherCA := "0x9999999999999999999999999999999999999999", "0x1010101010101010101010101010101010101010"
	owed := f.call(t, 60, owedCA, 61*time.Minute, levelInsufficient)
	other := f.call(t, 61, otherCA, 62*time.Minute, levelInsufficient)
	var sent []string
	l := f.lane("", verdict{}, &sent) // Perceptor does not answer: timeout
	l.cfg.After2 = 0                  // the horizon is the first window only: the owed call is older
	c, err := f.st.GetScoutCall(ctx, owed)
	if err != nil {
		t.Fatal(err)
	}
	l.owed[owed] = c.MessageDate // as retryLater stores it
	if !l.step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want the owed call's last try")
	}
	if want := []string{"/scan " + owedCA}; !slices.Equal(sent, want) {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	rows := f.percRows(t, owed)
	if len(rows) != 2 || rows[1].Status != investigationTimeout {
		t.Fatalf("owed call: %d Perceptor rows (%+v), want the first answer and a stored timeout re-check", len(rows), rows)
	}
	if d := f.deliveries(t, owed); len(d) != 1 || !strings.HasPrefix(f.headers[0], "❔ Perceptor: no report (timeout)") {
		t.Errorf("owed call: deliveries %+v, headers %q; want one, headed no report (timeout)", d, f.headers)
	}
	if _, still := l.owed[owed]; still {
		t.Error("owed call still owed after it was delivered")
	}
	if l.step(ctx, func() int { return 0 }) || len(sent) != 1 || len(f.deliveries(t, other)) != 0 || len(f.percRows(t, other)) != 1 {
		t.Errorf("not owed call past its window: sent %q, %d deliveries; want it skipped (nothing sent, stored or delivered)", sent, len(f.deliveries(t, other)))
	}
}

// Perceptor rate-limited: the lane does not wait it out in the runner (one
// request, with the tool's production MaxRetries), and inside the window
// nothing is stored and asking Perceptor pauses for recheckRetryPause; at the
// end of the window the re-check is stored as rate_limited and delivered
// labelled. A rate-limited second re-check is also tried again later.
func TestRecheckRateLimited(t *testing.T) {
	const limited = "One scan every 2 minutes. You can scan again in 63 s"
	cases := []struct {
		name   string
		age    time.Duration
		second bool // the call has its first re-check: the second is due
		stored bool
	}{
		{"window open: paused, retried later", 13 * time.Minute, false, false},
		{"end of window: stored and delivered", 57 * time.Minute, false, true},
		{"second re-check: paused, retried later", 2*time.Hour + 5*time.Minute, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			spec := &f.s.runners[0].spec
			spec.MaxRetries = builtinTools["perceptor"].MaxRetries // production: 3
			if spec.MaxRetries == 0 {
				t.Fatal("the built-in Perceptor tool has no rate-limit retries; the test needs some")
			}
			ca := "0x1212121212121212121212121212121212121212"
			call := f.call(t, 70, ca, c.age, levelInsufficient)
			n := 1
			if c.second {
				f.recheckRow(t, call, ca, time.Now().Add(-c.age+12*time.Minute), 1, levelInsufficient)
				n = 2
			}
			before := len(f.percRows(t, call))
			var sent []string
			l := f.lane(limited, verdict{}, &sent)
			clock := time.Now()
			l.now = func() time.Time { return clock }
			start := time.Now()
			if !l.step(context.Background(), func() int { return 0 }) {
				t.Fatal("step() = false, want a re-check")
			}
			if len(sent) != 1 || time.Since(start) > 30*time.Second {
				t.Fatalf("sent %q in %s; want one /scan and no rate-limit wait", sent, time.Since(start))
			}
			rows := f.percRows(t, call)
			if !c.stored {
				key := recheckKey(call, n)
				if len(rows) != before || len(f.headers) != 0 {
					t.Fatalf("%d Perceptor rows (had %d), %d deliveries; want nothing stored or sent", len(rows), before, len(f.headers))
				}
				want := clock.Add(recheckRetryPause)
				if !l.percPausedUntil.Equal(want) || !l.retryAt[key].Equal(want) || l.tried[key] || !l.pausedUntil.IsZero() {
					t.Errorf("Perceptor paused until %s, lane paused until %s, retry at %s, tried %v; want %s, none, %s, released",
						l.percPausedUntil, l.pausedUntil, l.retryAt[key], l.tried[key], want, want)
				}
				return
			}
			if len(rows) != 2 || rows[1].Status != investigationRateLimited {
				t.Fatalf("%d Perceptor rows (%+v), want a stored rate_limited re-check", len(rows), rows)
			}
			if len(f.headers) != 1 || !strings.HasPrefix(f.headers[0], "❔ Perceptor: no report (rate_limited)") {
				t.Errorf("headers %q, want one headed no report (rate_limited)", f.headers)
			}
		})
	}
}

// Only calls the listener held back are re-checked: deferToRecheck marks the
// call's live Perceptor result (details.held_for_recheck) and the lane reads
// marked calls only. A call that another gate stopped, one the gate let
// through (SCOUT_DELIVER_LEVELS with unknown; its delivery row failed to
// store), one handled by -scan / -post or a dry run (no lane), and one an
// older binary dropped before the deploy get no marker and are never
// re-checked or delivered by the lane.
func TestRecheckHeldMarker(t *testing.T) {
	ca := "0x1313131313131313131313131313131313131313"
	cases := []struct {
		name  string
		level string
		// setup adjusts the scanner and the live results before deferToRecheck
		setup    func(f *recheckFixture, res []*toolResult)
		deferred bool // whether process() reaches deferToRecheck (false: a row as an older binary left it)
		held     bool
	}{
		{"held back: marked and re-checked", levelInsufficient, nil, true, true},
		{"unknown held back: marked and re-checked", levelUnknown, nil, true, true},
		{"another gate said no", levelInsufficient, func(f *recheckFixture, res []*toolResult) {
			res[1].Spec.Gate, res[1].Status = true, investigationTimeout
		}, true, false},
		{"delivered at the call (SCOUT_DELIVER_LEVELS has unknown), delivery row not stored", levelUnknown,
			func(f *recheckFixture, res []*toolResult) { f.s.cfg.DeliverLevels[levelUnknown] = true }, true, false},
		{"-scan / -post: no lane", levelInsufficient, func(f *recheckFixture, res []*toolResult) { f.s.recheck = nil }, true, false},
		{"dry run: the lane is off", levelInsufficient, func(f *recheckFixture, res []*toolResult) {
			f.s.cfg.DryRun = true
			f.s.recheck = f.s.newRecheckLane(recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour})
			if f.s.recheck != nil {
				t.Error("newRecheckLane in a dry run = a lane, want nil")
			}
		}, true, false},
		{"marker cannot be written", levelInsufficient, func(f *recheckFixture, res []*toolResult) {
			f.s.recheck.hold = func(ctx context.Context, invID int, scoreLine string) error { return errors.New("database gone") }
		}, true, false},
		{"dropped by an older binary before the deploy", levelInsufficient, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			ctx := context.Background()
			call, inv := f.unheldCall(t, 80, ca, 13*time.Minute, c.level)
			var sent []string
			f.s.recheck = f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
			res := []*toolResult{
				{Spec: f.s.runners[0].spec, Status: investigationCompleted, Verdict: verdict{Level: c.level}},
				{Spec: f.s.runners[1].spec, Status: investigationCompleted, Verdict: verdict{Level: levelUnknown}},
			}
			if c.setup != nil {
				c.setup(f, res)
			}
			if c.deferred {
				if got := f.s.deferToRecheck(ctx, job{CA: ca, CallID: &call}, res, &inv, ""); got != c.held {
					t.Errorf("deferToRecheck = %v, want %v", got, c.held)
				}
			}
			if got := f.held(t, inv); got != c.held {
				t.Errorf("held marker on investigation %d = %v, want %v", inv, got, c.held)
			}
			// a running (real, not dry-run) listener's lane
			f.s.cfg.DryRun = false
			if got := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent).
				step(ctx, func() int { return 0 }); got != c.held {
				t.Errorf("lane step = %v, want %v (sent %q)", got, c.held, sent)
			}
			wantSent := 0
			if c.held {
				wantSent = 1
			}
			if len(sent) != wantSent || len(f.headers) != wantSent || len(f.deliveries(t, call)) != wantSent {
				t.Errorf("sent %q, %d deliveries, %d delivery rows; want %d of each", sent, len(f.headers), len(f.deliveries(t, call)), wantSent)
			}
		})
	}
}

// MarkHeldForRecheck adds the key, and the score line (score_line) when there
// is one, and keeps the others; running it again changes nothing; an empty
// line stores no score_line (nor removes one); an unknown id is an error.
func TestMarkHeldForRecheck(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	read := func(id int) (map[string]any, string) {
		t.Helper()
		var b []byte
		var rest string
		if err := f.st.Pool.QueryRow(ctx, `SELECT details, concat_ws('|', status, verdict_level, updated_at::text, updated_by)
			FROM scout_investigations WHERE id = $1`, id).Scan(&b, &rest); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m, rest
	}
	cases := []struct {
		name string
		line string
		want map[string]any // details after each mark
	}{
		{"with a score line", recheckScoreLine,
			map[string]any{"attempts": float64(1), "held_for_recheck": true, "score_line": recheckScoreLine}},
		{"no score line: no key", "",
			map[string]any{"attempts": float64(1), "held_for_recheck": true}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := time.Now().Add(-time.Hour)
			ca := fmt.Sprintf("0x15%d", i)
			id, err := f.st.InsertScoutInvestigation(ctx, &ScoutInvestigation{ToolID: f.perc, ContractAddress: ca,
				RequestText: "/scan " + ca, RequestedAt: at, CompletedAt: &at, Status: investigationCompleted,
				VerdictLevel: levelInsufficient, Details: []byte(`{"attempts":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			_, before := read(*id)
			for n := 1; n <= 2; n++ {
				if err := f.st.MarkHeldForRecheck(ctx, *id, c.line); err != nil {
					t.Fatalf("mark %d: %v", n, err)
				}
				m, rest := read(*id)
				if !maps.Equal(m, c.want) || rest != before {
					t.Errorf("mark %d (line %q): details %v, other columns %q (before %q); want details %v, nothing else changed", n, c.line, m, rest, before, c.want)
				}
			}
		})
	}
	t.Run("an empty line keeps a stored one", func(t *testing.T) {
		at := time.Now().Add(-time.Hour)
		id, err := f.st.InsertScoutInvestigation(ctx, &ScoutInvestigation{ToolID: f.perc, ContractAddress: "0x15e",
			RequestText: "/scan 0x15e", RequestedAt: at, CompletedAt: &at, Status: investigationCompleted,
			VerdictLevel: levelInsufficient, Details: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{recheckScoreLine, ""} {
			if err := f.st.MarkHeldForRecheck(ctx, *id, line); err != nil {
				t.Fatalf("mark (line %q): %v", line, err)
			}
		}
		if m, _ := read(*id); m["score_line"] != recheckScoreLine || m["held_for_recheck"] != true {
			t.Errorf("details %v after marks with %q then \"\", want score_line %q kept and held_for_recheck true", m, recheckScoreLine, recheckScoreLine)
		}
	})
	if err := f.st.MarkHeldForRecheck(ctx, 1<<30, recheckScoreLine); err == nil {
		t.Error("MarkHeldForRecheck of an unknown id = nil, want an error")
	}
}

// The delivery is recorded (status sending) before it is sent. When the send
// went out but recording its outcome failed, the row stays at sending, and
// neither this process nor a restarted one delivers the call again; only
// sent counts as delivered (scout_investigations_v).
func TestRecheckPendingDelivery(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := "0x1414141414141414141414141414141414141414"
	call := f.call(t, 90, ca, 13*time.Minute, levelInsufficient)
	var sent []string
	l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
	inner := l.deliver
	var atSend []ScoutDelivery
	l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
		atSend = f.deliveries(t, call)
		return inner(ctx, j, header, results)
	}
	l.finish = func(ctx context.Context, id int, status string, at *time.Time, errText *string, notifyMsgID *int64) error {
		return errors.New("database gone")
	}
	if !l.step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want a re-check")
	}
	if len(atSend) != 1 || atSend[0].Status != DeliveryStatusSending {
		t.Fatalf("delivery rows while sending %+v, want one at sending", atSend)
	}
	d := f.deliveries(t, call)
	if len(f.headers) != 1 || len(d) != 1 || d[0].Status != DeliveryStatusSending || d[0].HeaderText != f.headers[0] {
		t.Fatalf("%d sent, rows %+v; want one sent and its row left at sending with the header", len(f.headers), d)
	}
	for i, l := range []*recheckLane{l, f.lane(recheckReply, verdict{Level: levelClean}, &sent)} {
		if l.step(ctx, func() int { return 0 }) || len(f.headers) != 1 || len(sent) != 1 {
			t.Errorf("lane %d (0: same process, 1: restarted): delivered or re-checked again (%d deliveries, sent %q)", i, len(f.headers), sent)
		}
	}
	re := f.percRows(t, call)[1]
	var delivered bool
	if err := f.st.Pool.QueryRow(ctx, `SELECT delivered FROM scout_investigations_v WHERE id = $1`, *re.ID).Scan(&delivered); err != nil || delivered {
		t.Errorf("scout_investigations_v.delivered of the re-check = %v (%v), want false (sending is not sent)", delivered, err)
	}
}

// The lane asks Perceptor only when the runner is free: while the runner's
// pacing (production MinInterval) still runs it does not sleep (a live call
// never waits behind it) and looks again at the earlier of the normal look
// (recheckTick) and the end of the pacing. During a rate-limit pause it does
// not ask either. Delivering a stored re-check, which does not ask Perceptor,
// goes ahead in all of them.
func TestRecheckPacing(t *testing.T) {
	cases := []struct {
		name  string
		block func(l *recheckLane) // makes Perceptor busy
		free  func(l *recheckLane) // frees it
		// look: the next look expected while blocked (nil: the normal look)
		look func(l *recheckLane) time.Time
	}{
		// the pacing ends after the normal look (about 65 s left, tick 30 s):
		// the normal look stays (before the fix it was pushed to the pacing end)
		{"pacing past the normal look", func(l *recheckLane) { l.runner.lastSent = time.Now().Add(-time.Minute) },
			func(l *recheckLane) { l.runner.lastSent = time.Now().Add(-3 * time.Minute) }, nil},
		// the pacing ends before the normal look (about 15 s left): look then
		{"pacing before the normal look", func(l *recheckLane) { l.runner.lastSent = time.Now().Add(-110 * time.Second) },
			func(l *recheckLane) { l.runner.lastSent = time.Now().Add(-3 * time.Minute) },
			func(l *recheckLane) time.Time { return l.runner.lastSent.Add(l.runner.spec.MinInterval) }},
		{"rate-limit pause", func(l *recheckLane) { l.percPausedUntil = time.Now().Add(recheckRetryPause) },
			func(l *recheckLane) { l.percPausedUntil = time.Time{} }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			ctx := context.Background()
			spec := &f.s.runners[0].spec
			spec.MinInterval, spec.MaxRetries = builtinTools["perceptor"].MinInterval, builtinTools["perceptor"].MaxRetries
			dueCA, storedCA := "0x1616161616161616161616161616161616161616", "0x1717171717171717171717171717171717171717"
			f.call(t, 110, dueCA, 13*time.Minute, levelInsufficient)
			stored := f.call(t, 111, storedCA, 20*time.Minute, levelInsufficient)
			f.recheckRow(t, stored, storedCA, time.Now().Add(-8*time.Minute), 1, levelClean) // its delivery was stopped
			var sent []string
			l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
			c.block(l)
			start := time.Now()
			if !l.step(ctx, func() int { return 0 }) || len(sent) != 0 || len(f.deliveries(t, stored)) != 1 {
				t.Fatalf("first step: sent %q, %d delivery rows of the stored re-check; want it delivered, Perceptor not asked", sent, len(f.deliveries(t, stored)))
			}
			if l.step(ctx, func() int { return 0 }) || len(sent) != 0 {
				t.Fatalf("second step while Perceptor is busy: sent %q, want nothing", sent)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("the steps took %s, want no pacing sleep", d)
			}
			if c.look != nil {
				if want := c.look(l); l.nextLook.Sub(want).Abs() > 2*time.Second {
					t.Errorf("next look %s, want about %s (the end of the pacing, before the normal look)", l.nextLook, want)
				}
			} else if want := time.Now().Add(recheckTick); l.nextLook.Sub(want).Abs() > 2*time.Second {
				t.Errorf("next look %s, want about %s (the normal look)", l.nextLook, want)
			}
			c.free(l)
			l.nextLook = time.Time{}
			if !l.step(ctx, func() int { return 0 }) || !slices.Equal(sent, []string{"/scan " + dueCA}) {
				t.Errorf("step once Perceptor is free: sent %q, want the due re-check", sent)
			}
		})
	}
}
