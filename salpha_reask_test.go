package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestLoadReaskConfig(t *testing.T) {
	keys := []string{"SCOUT_SALPHA_REASK", "SCOUT_SALPHA_REASK_MIN_AGE", "SCOUT_SALPHA_REASK_MAX_AGE",
		"SCOUT_SALPHA_REASK_MAX_PER_DAY", "SCOUT_SALPHA_REASK_GAP"}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    reaskConfig
		wantErr string
	}{
		{name: "defaults: off", want: reaskConfig{MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute}},
		{name: "on with settings", env: map[string]string{"SCOUT_SALPHA_REASK": "ON", "SCOUT_SALPHA_REASK_MIN_AGE": "2h",
			"SCOUT_SALPHA_REASK_MAX_AGE": "72h", "SCOUT_SALPHA_REASK_MAX_PER_DAY": "5", "SCOUT_SALPHA_REASK_GAP": "0s"},
			want: reaskConfig{Enabled: true, MinAge: 2 * time.Hour, MaxAge: 72 * time.Hour, MaxPerDay: 5, Gap: 0}},
		{name: "bad switch", env: map[string]string{"SCOUT_SALPHA_REASK": "yes"}, wantErr: "SCOUT_SALPHA_REASK="},
		{name: "min age too short", env: map[string]string{"SCOUT_SALPHA_REASK_MIN_AGE": "30m"}, wantErr: "SCOUT_SALPHA_REASK_MIN_AGE"},
		{name: "max age not a duration", env: map[string]string{"SCOUT_SALPHA_REASK_MAX_AGE": "1d"}, wantErr: "SCOUT_SALPHA_REASK_MAX_AGE"},
		{name: "max not after min", env: map[string]string{"SCOUT_SALPHA_REASK_MIN_AGE": "24h"}, wantErr: "must be longer"},
		{name: "per day zero", env: map[string]string{"SCOUT_SALPHA_REASK_MAX_PER_DAY": "0"}, wantErr: "SCOUT_SALPHA_REASK_MAX_PER_DAY"},
		{name: "negative gap", env: map[string]string{"SCOUT_SALPHA_REASK_GAP": "-1m"}, wantErr: "SCOUT_SALPHA_REASK_GAP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, tc.env[k])
			}
			got, err := loadReaskConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadReaskConfig(%v) error = %v, want one about %s", tc.env, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("loadReaskConfig(%v) = %+v, %v; want %+v", tc.env, got, err, tc.want)
			}
		})
	}
}

// salphaResults is a Perceptor result (clean, the gate) and an sAlpha result
// with the given status and text.
func salphaResults(t *testing.T, status, text string) []*toolResult {
	t.Helper()
	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	sa := &toolResult{Spec: tools[1], Status: status, Verdict: verdict{Level: levelUnknown}}
	if text != "" || status == investigationCompleted {
		sa.Replies = []*tg.Message{{ID: 2, Message: text}}
	}
	return []*toolResult{
		{Spec: tools[0], Status: investigationCompleted, Verdict: verdict{Level: levelClean, Label: "No red flags", Ticker: "$X"},
			Replies: []*tg.Message{{ID: 1, Message: "perceptor report"}}},
		sa,
	}
}

const testDecline = "Not enough public signals to generate a report for this token."

// A decline is no report: the header says so (with when the lane asks again
// when it is on), it is not in the Bot API text nor in the attached
// investigations, and the Perceptor gate decides as before.
func TestDeliveryDeclines(t *testing.T) {
	cfg := &config{SourceChannel: "scoutrobinhood", DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}}
	off := &scanner{cfg: cfg}
	on := &scanner{cfg: cfg, reask: &reaskLane{cfg: reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour}}}
	call := job{CA: "0xA", SourceMsg: 7, CallID: ip(5)}
	for _, tc := range []struct {
		name, status, text string
		s                  *scanner
		j                  job
		want, notWant      string
	}{
		{"decline, lane off", investigationCompleted, testDecline, off, call,
			"• sAlpha: no report (declined: not enough public signals)\n", "asked again"},
		{"decline, lane on", investigationCompleted, testDecline, on, call,
			"• sAlpha: no report (declined: not enough public signals) — may be asked again later", ""},
		{"liquidity decline", investigationCompleted, "Too little liquidity or trading activity to research yet.", off, call,
			"• sAlpha: no report (declined: too little liquidity or trading)", ""},
		{"timeout, lane on", investigationTimeout, "", on, call, "• sAlpha: no report (timeout) — may be asked again later", ""},
		{"empty, lane on", investigationCompleted, " \n ", on, call, "• sAlpha: no report (empty reply) — may be asked again later", ""},
		{"timeout, -scan without a call", investigationTimeout, "", on, job{CA: "0xA"}, "• sAlpha: no report (timeout)", "asked again"},
		{"real report", investigationCompleted, "Smart money: 3 wallets bought", on, call, "• sAlpha: report attached", "asked again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.s.deliveryHeader(tc.j, salphaResults(t, tc.status, tc.text))
			if !strings.Contains(h+"\n", tc.want) || (tc.notWant != "" && strings.Contains(h, tc.notWant)) {
				t.Errorf("header for sAlpha %s %q:\n%s\nwant %q, without %q", tc.status, tc.text, h, tc.want, tc.notWant)
			}
		})
	}

	declined := salphaResults(t, investigationCompleted, testDecline)
	if !reportDeclined(declined[1]) || reportDeclined(declined[0]) {
		t.Fatalf("reportDeclined: sAlpha %v, Perceptor %v; want true, false", reportDeclined(declined[1]), reportDeclined(declined[0]))
	}
	if txt := off.botAPIText("HEADER", call, declined); strings.Contains(txt, "━━ sAlpha ━━") || strings.Contains(txt, testDecline) {
		t.Errorf("bot API text forwards the decline:\n%s", txt)
	}
	if txt := off.botAPIText("HEADER", call, salphaResults(t, investigationCompleted, "Smart money: 3 wallets")); !strings.Contains(txt, "━━ sAlpha ━━\nSmart money") {
		t.Errorf("bot API text lacks a real sAlpha report:\n%s", txt)
	}
	ids := map[string]*int{"perceptor": ip(11), "salpha": ip(12)}
	if got := deliveryAttachments(declined, ids); len(got) != 1 || got[0] != 11 {
		t.Errorf("attachments with a decline = %v, want [11]", got)
	}
	if got := deliveryAttachments(salphaResults(t, investigationCompleted, "Smart money: 3 wallets"), ids); len(got) != 2 {
		t.Errorf("attachments with a real report = %v, want [11 12]", got)
	}

	// the gate decides as before: a decline neither blocks nor allows
	if !off.shouldDeliver(declined) {
		t.Error("clean Perceptor with an sAlpha decline: not delivered, want delivered")
	}
	red := salphaResults(t, investigationCompleted, testDecline)
	red[0].Verdict.Level = levelRedFlags
	if off.shouldDeliver(red) {
		t.Error("red flags with an sAlpha decline: delivered, want not")
	}
	// without a gate tool, a decline alone is no report
	noGate := salphaResults(t, investigationCompleted, testDecline)
	noGate[0].Spec.Gate, noGate[0].Status = false, investigationTimeout
	if off.shouldDeliver(noGate) {
		t.Error("no gate tool, only an sAlpha decline: delivered, want not (a decline is no report)")
	}
	noGate[1] = salphaResults(t, investigationCompleted, "Smart money: 3 wallets")[1]
	if !off.shouldDeliver(noGate) {
		t.Error("no gate tool, a real sAlpha report: not delivered, want delivered")
	}
}

// botAPISend returns the first chunk's message id and replies with it only.
func TestBotAPISend(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // a read error leaves the body short; the decode below fails then
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("request body %q: %v", b, err)
		}
		mu.Lock()
		bodies = append(bodies, m)
		n := len(bodies)
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/botTOKEN/sendMessage") {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.Itoa(500+n) + `,"chat":{"id":-100}}}`)) // a failed write shows as a failed decode in the client
	}))
	t.Cleanup(srv.Close)
	old := botAPIBase
	botAPIBase = srv.URL
	t.Cleanup(func() { botAPIBase = old })

	text := strings.Repeat("a", 4000) + "tail"
	id, err := botAPISend(context.Background(), "TOKEN", "-100", text, 77)
	if err != nil || id != 501 {
		t.Fatalf("botAPISend = %d, %v; want 501 (the first chunk's id)", id, err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2 chunks", len(bodies))
	}
	rp, ok := bodies[0]["reply_parameters"].(map[string]any)
	if !ok || rp["message_id"] != float64(77) || rp["allow_sending_without_reply"] != true {
		t.Errorf("first chunk reply_parameters = %v, want message_id 77 and allow_sending_without_reply", bodies[0]["reply_parameters"])
	}
	if _, ok := bodies[1]["reply_parameters"]; ok {
		t.Errorf("second chunk replies too: %v", bodies[1])
	}
	bodies = nil
	if id, err := botAPISend(context.Background(), "TOKEN", "-100", "short", 0); err != nil || id != 501 {
		t.Fatalf("botAPISend without a reply = %d, %v; want 501", id, err)
	}
	if _, ok := bodies[0]["reply_parameters"]; ok {
		t.Errorf("reply_parameters sent without a message to reply to: %v", bodies[0])
	}

	// an answer without a message id: sent, id 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) })) // write errors: the client sees an unreadable answer either way
	t.Cleanup(srv2.Close)
	botAPIBase = srv2.URL
	if id, err := botAPISend(context.Background(), "TOKEN", "-100", "x", 0); err != nil || id != 0 {
		t.Errorf("botAPISend with an unreadable answer = %d, %v; want 0 and no error", id, err)
	}
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusForbidden) }))
	t.Cleanup(srv3.Close)
	botAPIBase = srv3.URL
	if _, err := botAPISend(context.Background(), "TOKEN", "-100", "x", 0); err == nil {
		t.Error("botAPISend with 403: no error")
	}
}

// sentMessageID reads the id of a sent message; an answer without one gives 0.
func TestSentMessageID(t *testing.T) {
	for _, tc := range []struct {
		name string
		upd  tg.UpdatesClass
		want int
	}{
		{"channel message", &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 9, RandomID: 1},
			&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 9, Message: "h"}}}}, 9},
		{"short sent", &tg.UpdateShortSentMessage{ID: 12}, 12},
		{"nothing", &tg.Updates{}, 0},
	} {
		if got := sentMessageID(tc.upd); got != tc.want {
			t.Errorf("%s: sentMessageID = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// fakeReaskStore records what the lane asks of the database.
type fakeReaskStore struct {
	mu         sync.Mutex
	today      int
	last       *time.Time
	cand       *ReaskCandidate
	statsN     int
	candN      int
	from, to   time.Time
	inserted   []*ScoutInvestigation
	delivery   *ScoutDelivery // FirstSentDelivery's answer
	deliveries []*ScoutDelivery
}

func (f *fakeReaskStore) ReaskStats(ctx context.Context, since time.Time) (int, *time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsN++
	return f.today, f.last, nil
}

func (f *fakeReaskStore) NextReaskCandidate(ctx context.Context, from, to time.Time) (*ReaskCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candN++
	f.from, f.to = from, to
	return f.cand, nil
}

func (f *fakeReaskStore) InsertScoutInvestigation(ctx context.Context, r *ScoutInvestigation) (*int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, r)
	id := 100 + len(f.inserted)
	return &id, nil
}

func (f *fakeReaskStore) FirstSentDelivery(ctx context.Context, callID int) (*ScoutDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivery, nil
}

func (f *fakeReaskStore) InsertScoutDelivery(ctx context.Context, d *ScoutDelivery) (*int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, d)
	id := len(f.deliveries)
	return &id, nil
}

// testSAlphaSpec is sAlpha's spec with short waits.
func testSAlphaSpec() ToolSpec {
	return ToolSpec{Code: "salpha", Name: "sAlpha", Bot: "salpha_research_bot", Command: "{ca}", Parser: parserText,
		Attach: true, Timeout: 2 * time.Second, Settle: 20 * time.Millisecond, MaxWait: 5 * time.Second,
		MaxRetries: 3, RateLimitRe: defaultRateLimitRe, ProgressRe: defaultProgressRe, MaxRateWait: time.Minute, NeedsText: true}
}

func TestReaskLaneStep(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	callAt := now.Add(-7 * time.Hour)
	ca := "0x1111111111111111111111111111111111111111"
	cfg := reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute}
	const real = "sAlpha research: 3 smart wallets bought; dev holds 2%."
	type sent struct {
		f  reaskFollowUp
		id int64
	}
	newLane := func(reply string, sendErr error, cmds *[]string, follow *[]sent) *reaskLane {
		l := &reaskLane{cfg: cfg, runner: newToolRunner(testSAlphaSpec()), toolID: 3, now: func() time.Time { return now },
			target: "scout analytics", send: replyingSend(reply, sendErr, cmds),
			judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
				return verdict{Level: levelUnknown, Source: "telegram_text"}
			},
			followUp: func(ctx context.Context, f reaskFollowUp) (int64, error) {
				*follow = append(*follow, sent{f: f, id: 9000})
				return 9000, nil
			},
			errLog: logLimiter{every: time.Minute}}
		l.noteLive(now.Add(-time.Hour))
		return l
	}
	cand := func() *ReaskCandidate {
		return &ReaskCandidate{CallID: 42, ContractAddress: ca, MessageDate: callAt, MessageID: 777, ChannelUsername: "scoutrobinhood",
			Symbol: sp("PEPE"), Reason: reaskReasonDeclined}
	}
	stored := int64(4242)
	delivered := func() *ScoutDelivery {
		return &ScoutDelivery{ID: ip(1), Target: "scout analytics", NotifyMessageID: &stored, Status: DeliveryStatusSent}
	}
	empty := func() int { return 0 }

	t.Run("real report of a delivered call: one reask row, a reply to the delivery", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		st := &fakeReaskStore{today: 2, cand: cand(), delivery: delivered()}
		if !l.step(context.Background(), st, empty) {
			t.Fatal("step() = false, want a re-ask")
		}
		if len(cmds) != 1 || cmds[0] != ca {
			t.Errorf("sent %q, want one %s", cmds, ca)
		}
		if !st.from.Equal(now.Add(-24*time.Hour)) || !st.to.Equal(now.Add(-6*time.Hour)) {
			t.Errorf("candidate window %s .. %s, want 24h .. 6h before %s", st.from, st.to, now)
		}
		if len(st.inserted) != 1 {
			t.Fatalf("inserted %d rows, want 1", len(st.inserted))
		}
		row := st.inserted[0]
		var d map[string]any
		if err := json.Unmarshal(row.Details, &d); err != nil {
			t.Fatal(err)
		}
		if row.ScanKind != ScanKindReask || row.CallID == nil || *row.CallID != 42 || row.ToolID != 3 || row.Status != investigationCompleted ||
			d["reask"] != true || d["call_age_s"] != float64(7*3600) || d["call_reply"] != reaskReasonDeclined {
			t.Errorf("row = %+v, details %s; want a completed reask of call 42 by tool 3 with reask, call_age_s 25200, call_reply", row, row.Details)
		}
		if len(follow) != 1 || follow[0].f.ReplyTo != stored || follow[0].f.Result.ReportText() != real {
			t.Fatalf("follow-ups %+v, want one reply to %d with the report", follow, stored)
		}
		wantHeader := "🕐 sAlpha report (asked again 7h after the call) — $PEPE\nCA: " + ca + "\nSource: https://t.me/scoutrobinhood/777"
		if follow[0].f.Header != wantHeader {
			t.Errorf("follow-up header:\n%s\nwant\n%s", follow[0].f.Header, wantHeader)
		}
		if len(st.deliveries) != 1 {
			t.Fatalf("recorded %d deliveries, want 1", len(st.deliveries))
		}
		dl := st.deliveries[0]
		if dl.Status != DeliveryStatusSent || dl.CallID == nil || *dl.CallID != 42 || len(dl.InvestigationIDs) != 1 || dl.InvestigationIDs[0] != 101 ||
			dl.NotifyMessageID == nil || *dl.NotifyMessageID != 9000 || dl.HeaderText != wantHeader || dl.DeliveredAt == nil {
			t.Errorf("follow-up delivery = %+v, want sent, call 42, investigation 101, message 9000", dl)
		}
	})

	for _, tc := range []struct {
		name      string
		reply     string
		delivery  *ScoutDelivery
		dryRun    bool
		wantRow   string // status of the stored row
		wantSends int    // follow-ups
		replyTo   int64
	}{
		{"declined again: no follow-up", testDecline, delivered(), false, investigationCompleted, 0, 0},
		{"never delivered (red flags): no follow-up", real, nil, false, investigationCompleted, 0, 0},
		{"-dry-run: no follow-up", real, delivered(), true, investigationCompleted, 0, 0},
		{"delivery without a stored id: sent without a reply", real,
			&ScoutDelivery{ID: ip(1), Target: "scout analytics", Status: DeliveryStatusSent}, false, investigationCompleted, 1, 0},
		{"delivery to another target: sent without a reply", real,
			&ScoutDelivery{ID: ip(1), Target: "me", NotifyMessageID: &stored, Status: DeliveryStatusSent}, false, investigationCompleted, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cmds []string
			var follow []sent
			l := newLane(tc.reply, nil, &cmds, &follow)
			l.noFollowUp = tc.dryRun
			st := &fakeReaskStore{cand: cand(), delivery: tc.delivery}
			if !l.step(context.Background(), st, empty) {
				t.Fatal("step() = false, want a re-ask")
			}
			if len(st.inserted) != 1 || st.inserted[0].Status != tc.wantRow || st.inserted[0].ScanKind != ScanKindReask {
				t.Fatalf("inserted %+v, want one %s reask row", st.inserted, tc.wantRow)
			}
			if len(follow) != tc.wantSends || len(st.deliveries) != tc.wantSends {
				t.Fatalf("follow-ups %d, deliveries %d; want %d", len(follow), len(st.deliveries), tc.wantSends)
			}
			if tc.wantSends > 0 && follow[0].f.ReplyTo != tc.replyTo {
				t.Errorf("reply to %d, want %d", follow[0].f.ReplyTo, tc.replyTo)
			}
		})
	}

	t.Run("busy queue: no database read, no request", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		st := &fakeReaskStore{cand: cand()}
		if l.step(context.Background(), st, func() int { return 1 }) || st.statsN+st.candN != 0 || len(cmds) != 0 {
			t.Errorf("step with a queued live call: stats %d, candidate %d, sent %q; want nothing", st.statsN, st.candN, cmds)
		}
	})

	t.Run("a live call waits for at most one re-ask", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		st := &fakeReaskStore{cand: cand(), delivery: delivered()}
		if !l.step(context.Background(), st, empty) {
			t.Fatal("first step: no re-ask")
		}
		// right after it: the gap (from the lane itself) holds even with candidates left
		if l.step(context.Background(), st, empty) || len(cmds) != 1 {
			t.Errorf("a second step right after a re-ask asked again (sent %q)", cmds)
		}
	})

	t.Run("not idle: nothing", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		l.noteLive(now.Add(-time.Minute))
		st := &fakeReaskStore{cand: cand()}
		if l.step(context.Background(), st, empty) || st.statsN+st.candN != 0 {
			t.Errorf("step 1m after a live call: stats %d, candidate %d; want nothing (idle %s)", st.statsN, st.candN, reaskIdle)
		}
	})

	t.Run("cap from the database", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		st := &fakeReaskStore{today: 60, cand: cand()}
		if l.step(context.Background(), st, empty) || st.candN != 0 {
			t.Errorf("step with %d re-asks today: candidate query %d, want none", st.today, st.candN)
		}
	})

	t.Run("FLOOD_WAIT: nothing written, lane paused", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane("x", tgerr.New(420, "FLOOD_WAIT_7200"), &cmds, &follow)
		st := &fakeReaskStore{cand: cand(), delivery: delivered()}
		l.step(context.Background(), st, empty)
		if len(st.inserted) != 0 || len(follow) != 0 {
			t.Errorf("inserted %+v, follow-ups %d after FLOOD_WAIT, want nothing", st.inserted, len(follow))
		}
		if got, want := l.pausedAt(), now.Add(2*time.Hour); !got.Equal(want) {
			t.Errorf("paused until %s, want %s", got, want)
		}
		if l.step(context.Background(), st, empty) || len(cmds) != 1 {
			t.Errorf("step while paused asked again")
		}
	})

	t.Run("rate limited: row written, lane paused an hour", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane("Too many requests. Try again in 60 seconds", nil, &cmds, &follow)
		st := &fakeReaskStore{cand: cand(), delivery: delivered()}
		l.step(context.Background(), st, empty)
		if len(cmds) != 1 || len(st.inserted) != 1 || st.inserted[0].Status != investigationRateLimited || len(follow) != 0 {
			t.Fatalf("sent %d, inserted %+v, follow-ups %d; want one rate_limited row, no retry, no follow-up", len(cmds), st.inserted, len(follow))
		}
		if got := l.pausedAt(); !got.Equal(now.Add(rescanPause)) {
			t.Errorf("paused until %s, want %s", got, now.Add(rescanPause))
		}
	})

	t.Run("follow-up FLOOD_WAIT: failed delivery recorded, lane paused", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane(real, nil, &cmds, &follow)
		l.followUp = func(ctx context.Context, f reaskFollowUp) (int64, error) {
			return 0, fmt.Errorf("send follow-up header: %w", tgerr.New(420, "FLOOD_WAIT_100"))
		}
		st := &fakeReaskStore{cand: cand(), delivery: delivered()}
		l.step(context.Background(), st, empty)
		if len(st.deliveries) != 1 || st.deliveries[0].Status != DeliveryStatusFailed || st.deliveries[0].Error == nil || st.deliveries[0].NotifyMessageID != nil {
			t.Fatalf("deliveries %+v, want one failed with its error", st.deliveries)
		}
		if got := l.pausedAt(); !got.Equal(now.Add(rescanPause)) {
			t.Errorf("paused until %s, want %s", got, now.Add(rescanPause))
		}
	})

	t.Run("stopped: nothing written", func(t *testing.T) {
		var cmds []string
		var follow []sent
		l := newLane("x", nil, &cmds, &follow)
		ctx, cancel := context.WithCancel(context.Background())
		l.send = func(ctx context.Context, r *toolRunner, cmd string) error {
			cancel()
			return nil
		}
		st := &fakeReaskStore{cand: cand()}
		l.step(ctx, st, empty)
		if len(st.inserted) != 0 {
			t.Errorf("inserted %+v after a stop, want nothing", st.inserted)
		}
	})
}

func TestReaskDetailsAndHeader(t *testing.T) {
	got := reaskDetails([]byte(`{"attempts":1}`), 90*time.Minute, investigationTimeout)
	var d map[string]any
	if err := json.Unmarshal(got, &d); err != nil || d["attempts"] != float64(1) || d["reask"] != true ||
		d["call_age_s"] != float64(5400) || d["call_reply"] != investigationTimeout {
		t.Errorf("reaskDetails = %s (%v), want attempts kept, reask, call_age_s 5400, call_reply timeout", got, err)
	}
	c := ReaskCandidate{ContractAddress: "0xA", ChannelUsername: "bad name", MessageID: 1}
	if h := reaskFollowUpHeader(c, 20*time.Hour); h != "🕐 sAlpha report (asked again 20h after the call)\nCA: 0xA" {
		t.Errorf("header without a symbol or link: %q", h)
	}
	c.Symbol = sp("$DOG")
	if h := reaskFollowUpHeader(c, 6*time.Hour); !strings.HasPrefix(h, "🕐 sAlpha report (asked again 6h after the call) — $DOG\n") {
		t.Errorf("header with a $ symbol: %q", h)
	}
	for d, want := range map[time.Duration]string{6 * time.Hour: "6 h", 90 * time.Minute: "1.5 h", 24 * time.Hour: "24 h"} {
		if got := hoursText(d); got != want {
			t.Errorf("hoursText(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestSummarizeReask(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cfg := reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute}
	h := func(n float64) time.Time { return now.Add(-time.Duration(n * float64(time.Hour))) }
	pool := []ReaskCandidate{
		{CallID: 1, MessageDate: h(24 * 40), Reason: investigationTimeout},                         // >30d
		{CallID: 2, MessageDate: h(24 * 10), Reason: reaskReasonDeclined},                          // 7-30d
		{CallID: 3, MessageDate: h(48), Reason: reaskReasonEmpty},                                  // up to 7d
		{CallID: 4, MessageDate: h(24), Reason: reaskReasonDeclined},                               // at MaxAge: in
		{CallID: 5, MessageDate: h(10), Reason: investigationFailed},                               // in
		{CallID: 6, MessageDate: h(10), Reason: reaskReasonDeclined, Reasked: true},                // excluded
		{CallID: 7, MessageDate: h(10), Reason: reaskReasonDeclined, HasReport: true},              // excluded
		{CallID: 8, MessageDate: h(10), Reason: reaskReasonDeclined, Rugged: true},                 // excluded
		{CallID: 9, MessageDate: h(10), Reason: reaskReasonDeclined, Wiped: true},                  // excluded
		{CallID: 10, MessageDate: h(6), Reason: investigationTimeout},                              // at MinAge: in
		{CallID: 11, MessageDate: h(2), Reason: investigationTimeout},                              // young
		{CallID: 12, MessageDate: h(10), Reason: reaskReasonDeclined, Reasked: true, Rugged: true}, // counted once
	}
	s := summarizeReask(cfg, now, pool)
	if s.Pool != 12 || s.Reasked != 2 || s.HasReport != 1 || s.Rugged != 1 || s.Wiped != 1 || s.Young != 1 ||
		s.Old != [3]int{1, 1, 1} || s.Candidates != 3 {
		t.Errorf("summary = %+v; want pool 12, reasked 2, has report 1, rugged 1, wiped 1, young 1, old [1 1 1], candidates 3", s)
	}
	var ids []int
	for _, r := range s.List {
		ids = append(ids, r.CallID)
	}
	if len(ids) != 3 || ids[0] != 4 || ids[1] != 5 || ids[2] != 10 {
		t.Errorf("candidates %v, want [4 5 10] (oldest first)", ids)
	}
	if s.ByReason[reaskReasonDeclined] != 2 || s.ByReason[investigationTimeout] != 3 || s.ByReason[investigationFailed] != 1 || s.ByReason[reaskReasonEmpty] != 1 {
		t.Errorf("by reason %v", s.ByReason)
	}
	var out bytes.Buffer
	writeReaskSummary(&out, cfg, s)
	for _, line := range []string{
		"first calls whose latest live sAlpha reply was no report: 12\n",
		"asked again already 2, token has a report 1, rugged 1, latest return <= -99% 1\n",
		"left, by the call's reply (any age): declined 2, empty 1, timeout 3, failed 1, rate_limited 0\n",
		"younger than 6 h (asked later): 1\n",
		"older than 24 h (skipped; a larger SCOUT_SALPHA_REASK_MAX_AGE would take them): up to 7d 1, 7-30d 1, >30d 1\n",
		"candidates now (6 h to 24 h after the call): 3\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("dry run lacks %q:\n%s", line, out.String())
		}
	}
}

// The website shows a re-ask's report like a call's, with how long after the
// call it was asked (salpha_reask_after_s, part of the snapshot version), and
// the detail says it is a later report.
func TestWebSAlphaReaskRow(t *testing.T) {
	build := func(reask bool) (*webServer, []ScoutWebRow) {
		t.Helper()
		ws := benchWebServer(t, 1)
		rows := syntheticWebRows(60, 9)
		reports := syntheticWebReports(rows)
		for i := range rows {
			r := &rows[i]
			kind := ScanKindLive
			if reask && r.SAlphaID != nil {
				kind = ScanKindReask
				at := r.MessageDate.Add(7 * time.Hour)
				r.SAlphaAt = &at
				reports[*r.SAlphaID].Kind = ScanKindReask
			}
			r.SAlphaKind = &kind
		}
		db := &fakeWebDB{rows: rows, reports: reports}
		ws.readRows, ws.readReports = db.read, db.readReports
		ws.snap.Store(nil)
		if err := ws.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		return ws, rows
	}
	live, _ := build(false)
	ws, rows := build(true)
	if ws.snap.Load().version == live.snap.Load().version {
		t.Fatal("a re-ask report must change the snapshot version")
	}
	pos := -1
	for i := range rows {
		if rows[i].SAlphaID != nil {
			pos = i
			break
		}
	}
	if pos < 0 {
		t.Fatal("the synthetic rows need an sAlpha report")
	}
	if c := sentCall(t, ws.snap.Load(), pos, 1); !c.HasSAlpha || c.SAlphaReaskAfterS == nil || *c.SAlphaReaskAfterS != 7*3600 {
		t.Errorf("re-ask row: has_salpha_report %v, salpha_reask_after_s %v; want true and %d", c.HasSAlpha, c.SAlphaReaskAfterS, 7*3600)
	}
	if c := sentCall(t, live.snap.Load(), pos, 1); c.SAlphaReaskAfterS != nil {
		t.Errorf("live row: salpha_reask_after_s %d, want null", *c.SAlphaReaskAfterS)
	}
	for _, tc := range []struct {
		ws   *webServer
		want bool
	}{{ws, true}, {live, false}} {
		rec := getWeb(tc.ws, "/api/call?id="+strconv.Itoa(rows[pos].CallID))
		if rec.Code != 200 {
			t.Fatalf("/api/call: %d %s", rec.Code, rec.Body)
		}
		if d := decodeCallDetail(t, rec.Body.Bytes()); d.SAlpha == nil || d.SAlpha.Reask != tc.want {
			t.Errorf("/api/call salpha = %+v, want reask %v", d.SAlpha, tc.want)
		}
	}
}

// testBotToken looks like a real bot token; no error or log line may show it.
const testBotToken = "123456789:AAH-secretTOKENvalue_xyz"

// setBotAPIBase points botAPIBase at base for the test.
func setBotAPIBase(t *testing.T, base string) {
	t.Helper()
	old := botAPIBase
	botAPIBase = base
	t.Cleanup(func() { botAPIBase = old })
}

// closedServerURL is the address of a server that is no longer listening (a
// request to it fails in http.Client.Do, whose error names the URL).
func closedServerURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := srv.URL
	srv.Close()
	return u
}

// botAPIAnswer is a fake Bot API that answers every request with code, the
// header Retry-After (when not "") and body.
func botAPIAnswer(t *testing.T, code int, retryAfter, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body)) // a failed write shows as an unreadable answer in the client
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A Bot API send error never contains the bot token (it reaches the logs and
// scout_deliveries.error); a 429 says how long to wait.
func TestBotAPISendErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base      func(t *testing.T) string
		token     string
		want429   bool
		wantRetry time.Duration
	}{
		{"connection refused", closedServerURL, testBotToken, false, 0},
		{"invalid URL (control character in the token)", closedServerURL, testBotToken + "\x7f", false, 0},
		{"403", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusForbidden, "", `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`)
		}, testBotToken, false, 0},
		{"429 with retry_after", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "", `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7200","parameters":{"retry_after":7200}}`)
		}, testBotToken, true, 2 * time.Hour},
		{"429 with a Retry-After header only", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "90", `not json`)
		}, testBotToken, true, 90 * time.Second},
		{"429 without a wait", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "", `{"ok":false,"error_code":429}`)
		}, testBotToken, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBotAPIBase(t, tc.base(t))
			waits := stubBotAPISleep(t, nil)
			_, err := botAPISend(context.Background(), tc.token, "-100", "x", 0)
			if err == nil {
				t.Fatal("botAPISend: no error")
			}
			if len(*waits) != 0 {
				t.Errorf("waited %v; want no wait (no 429 to wait out within the budget)", *waits)
			}
			if strings.Contains(err.Error(), "secretTOKEN") {
				t.Errorf("error %q contains the bot token", err)
			}
			d, ok := sendFloodWait(err)
			if ok != tc.want429 || d != tc.wantRetry {
				t.Errorf("sendFloodWait(%q) = %s, %v; want %s, %v", err, d, ok, tc.wantRetry, tc.want429)
			}
		})
	}

	t.Run("a stop is still a context error", func(t *testing.T) {
		setBotAPIBase(t, closedServerURL(t))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := botAPISend(ctx, testBotToken, "-100", "x", 0)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secretTOKEN") {
			t.Errorf("botAPISend with a cancelled context = %q; want context.Canceled without the token", err)
		}
	})

	t.Run("live delivery", func(t *testing.T) {
		setBotAPIBase(t, closedServerURL(t))
		s := &scanner{cfg: &config{NotifyBotToken: testBotToken, NotifyChatID: "-100", SourceChannel: "scoutrobinhood"}}
		_, err := s.deliver(context.Background(), job{CA: "0xA"}, "HEADER", nil)
		if err == nil || strings.Contains(err.Error(), "secretTOKEN") {
			t.Errorf("deliver error = %v; want an error without the bot token", err)
		}
	})
}

// A follow-up sent with the Bot API: its error is stored and logged without
// the bot token, and a 429 pauses the lane like a FLOOD_WAIT (for retry_after
// or an hour, whichever is longer).
func TestReaskFollowUpBotAPIErrors(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	stored := int64(4242)
	for _, tc := range []struct {
		name      string
		base      func(t *testing.T) string
		wantPause time.Time       // zero: not paused
		wantWaits []time.Duration // botAPISend's 429 waits before it gave up
	}{
		{"connection refused: not paused", closedServerURL, time.Time{}, nil},
		{"403: not paused", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusForbidden, "", `{"ok":false,"error_code":403,"description":"Forbidden"}`)
		}, time.Time{}, nil},
		{"429 with retry_after 2h: paused 2h", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "", `{"ok":false,"error_code":429,"parameters":{"retry_after":7200}}`)
		}, now.Add(2 * time.Hour), nil},
		{"429 with retry_after 30s every time: retried within the budget, then paused an hour", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "", `{"ok":false,"error_code":429,"parameters":{"retry_after":30}}`)
		}, now.Add(rescanPause), []time.Duration{30 * time.Second, 30 * time.Second}},
		{"429 without a wait: paused an hour", func(t *testing.T) string {
			return botAPIAnswer(t, http.StatusTooManyRequests, "", `{"ok":false,"error_code":429}`)
		}, now.Add(rescanPause), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBotAPIBase(t, tc.base(t))
			waits := stubBotAPISleep(t, nil)
			logs := captureLog(t)
			s := &scanner{cfg: &config{NotifyBotToken: testBotToken, NotifyChatID: "-100"}}
			l := &reaskLane{cfg: reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute},
				runner: newToolRunner(testSAlphaSpec()), now: func() time.Time { return now }, target: "bot",
				followUp: s.sendReaskFollowUp, errLog: logLimiter{every: time.Minute}}
			st := &fakeReaskStore{delivery: &ScoutDelivery{ID: ip(1), Target: "bot", NotifyMessageID: &stored, Status: DeliveryStatusSent}}
			res := &toolResult{Spec: testSAlphaSpec(), Status: investigationCompleted,
				Replies: []*tg.Message{{ID: 2, Message: "sAlpha research: 3 smart wallets bought."}}}
			c := ReaskCandidate{CallID: 42, ContractAddress: "0xA", MessageDate: now.Add(-7 * time.Hour)}
			l.deliverFollowUp(context.Background(), st, c, res, 101, 7*time.Hour)

			if len(st.deliveries) != 1 || st.deliveries[0].Status != DeliveryStatusFailed || st.deliveries[0].Error == nil {
				t.Fatalf("deliveries %+v, want one failed with its error", st.deliveries)
			}
			if e := *st.deliveries[0].Error; strings.Contains(e, "secretTOKEN") {
				t.Errorf("stored error %q contains the bot token", e)
			}
			if out := logs.String(); strings.Contains(out, "secretTOKEN") || !strings.Contains(out, "follow-up failed") {
				t.Errorf("log %q: want a follow-up failure without the bot token", out)
			}
			if got := l.pausedAt(); !got.Equal(tc.wantPause) {
				t.Errorf("paused until %s, want %s", got, tc.wantPause)
			}
			if !slices.Equal(*waits, tc.wantWaits) {
				t.Errorf("429 waits %v, want %v", *waits, tc.wantWaits)
			}
		})
	}
}

// stubBotAPISleep replaces botAPISleep for the test: it records each wait and
// returns err (nil: the wait "passed" at once). Tests never sleep.
func stubBotAPISleep(t *testing.T, err error) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := botAPISleep
	botAPISleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return err
	}
	t.Cleanup(func() { botAPISleep = old })
	return &waits
}

// botAPISequence is a fake Bot API that gives the n-th request (from 0) the
// answer answers[n] (the last one again after the end): its code and body.
// A code of 0 drops the connection without an answer. It returns the base URL
// and a function counting the requests so far.
func botAPISequence(t *testing.T, answers ...botAnswer) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the body does not matter here
		mu.Lock()
		a := answers[min(n, len(answers)-1)]
		n++
		mu.Unlock()
		if a.code == 0 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server cannot drop a connection")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close() // dropping the connection is the point
			return
		}
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body)) // a failed write shows as an unreadable answer in the client
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

type botAnswer = struct {
	code int
	body string
}

func botOK(id int) botAnswer {
	return botAnswer{http.StatusOK, `{"ok":true,"result":{"message_id":` + strconv.Itoa(id) + `}}`}
}

func bot429(retryAfter int) botAnswer {
	return botAnswer{http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after ` +
		strconv.Itoa(retryAfter) + `","parameters":{"retry_after":` + strconv.Itoa(retryAfter) + `}}`}
}

// A Bot API 429 on the delivery path is waited out (Telegram's retry_after)
// and the same chunk sent again, within botAPIRetryBudget and botAPIRetryMax;
// beyond that the 429 is returned (the delivery is then recorded as failed
// and logged), never dropped. A stop during the wait is a context error.
func TestBotAPISend429(t *testing.T) {
	long := strings.Repeat("a", botAPILimit) + "tail" // two chunks
	for _, tc := range []struct {
		name      string
		text      string
		answers   []botAnswer
		sleepErr  error
		wantID    int64
		wantErr   bool
		want429   bool
		wantWaits []time.Duration
		wantReqs  int
	}{
		{"one 429, then sent", "x", []botAnswer{bot429(3), botOK(500)}, nil, 500, false, false, []time.Duration{3 * time.Second}, 2},
		{"429 on the second chunk: only that one again", long, []botAnswer{botOK(600), bot429(5), botOK(601)}, nil,
			600, false, false, []time.Duration{5 * time.Second}, 3},
		{"retry_after longer than the budget: not waited, returned", "x", []botAnswer{bot429(int(botAPIRetryBudget/time.Second) + 1)}, nil,
			0, true, true, nil, 1},
		{"429 again and again: given up within the budget", "x", []botAnswer{bot429(25)}, nil,
			0, true, true, []time.Duration{25 * time.Second, 25 * time.Second}, 3},
		{"429 again and again with short waits: at most botAPIRetryMax retries", "x", []botAnswer{bot429(1)}, nil,
			0, true, true, []time.Duration{time.Second, time.Second, time.Second}, botAPIRetryMax + 1},
		{"429 without retry_after: returned", "x", []botAnswer{{http.StatusTooManyRequests, `{"ok":false,"error_code":429}`}}, nil,
			0, true, true, nil, 1},
		{"stopped during the wait: a context error", "x", []botAnswer{bot429(3)}, context.Canceled,
			0, true, false, []time.Duration{3 * time.Second}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, reqs := botAPISequence(t, tc.answers...)
			setBotAPIBase(t, base)
			waits := stubBotAPISleep(t, tc.sleepErr)
			id, err := botAPISend(context.Background(), testBotToken, "-100", tc.text, 0)
			if id != tc.wantID || (err != nil) != tc.wantErr {
				t.Fatalf("botAPISend = %d, %v; want %d, error %v", id, err, tc.wantID, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secretTOKEN") {
				t.Errorf("error %q contains the bot token", err)
			}
			if _, is429 := botAPIRetryAfter(err); is429 != tc.want429 {
				t.Errorf("error %v: a 429 = %v, want %v", err, is429, tc.want429)
			}
			if tc.sleepErr != nil && !errors.Is(err, tc.sleepErr) {
				t.Errorf("error %v, want it to wrap %v", err, tc.sleepErr)
			}
			if !slices.Equal(*waits, tc.wantWaits) || reqs() != tc.wantReqs {
				t.Errorf("waits %v, requests %d; want %v, %d", *waits, reqs(), tc.wantWaits, tc.wantReqs)
			}
		})
	}

	t.Run("live delivery: a 429 beyond the budget is the delivery's error", func(t *testing.T) {
		base, _ := botAPISequence(t, bot429(7200))
		setBotAPIBase(t, base)
		stubBotAPISleep(t, nil)
		s := &scanner{cfg: &config{NotifyBotToken: testBotToken, NotifyChatID: "-100", SourceChannel: "scoutrobinhood"}}
		_, err := s.deliver(context.Background(), job{CA: "0xA"}, "HEADER", nil)
		if d, ok := botAPIRetryAfter(err); !ok || d != 2*time.Hour || !strings.Contains(err.Error(), "retry after 2h0m0s") {
			t.Errorf("deliver error = %v; want the 429 with its retry_after (2h) in the text", err)
		}
	})
}

// A network error on a later chunk (splitMessage) is redacted like one on the
// first: the *url.Error names the URL with the bot token. The first chunk's id
// is still returned.
func TestBotAPISendRedactsLaterChunk(t *testing.T) {
	base, reqs := botAPISequence(t, botOK(700), botAnswer{0, ""})
	setBotAPIBase(t, base)
	stubBotAPISleep(t, nil)
	text := strings.Repeat("line\n", botAPILimit/5) + "tail" // two chunks, cut at a newline
	id, err := botAPISend(context.Background(), testBotToken, "-100", text, 0)
	if err == nil || id != 700 || reqs() != 2 {
		t.Fatalf("botAPISend = %d, %v after %d requests; want 700, an error, 2 requests", id, err, reqs())
	}
	if strings.Contains(err.Error(), "secretTOKEN") || strings.Contains(err.Error(), "/bot") {
		t.Errorf("error %q names the URL or the bot token", err)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Errorf("error %q still wraps the *url.Error (its URL has the token)", err)
	}
}

// The re-ask lane does not start while sAlpha's pacing still runs (after a
// live request): it reads nothing and sends nothing, so the worker never
// sleeps on its behalf.
func TestReaskLaneWaitsForPacing(t *testing.T) {
	now := time.Now()
	spec := testSAlphaSpec()
	spec.MinInterval = time.Minute
	r := newToolRunner(spec)
	r.lastSent = now.Add(-10 * time.Second)
	var cmds []string
	l := &reaskLane{cfg: reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute},
		runner: r, toolID: 3, now: func() time.Time { return now }, send: replyingSend("x", nil, &cmds), errLog: logLimiter{every: time.Minute}}
	l.noteLive(now.Add(-time.Hour))
	st := &fakeReaskStore{cand: &ReaskCandidate{CallID: 1, ContractAddress: "0xA", MessageDate: now.Add(-7 * time.Hour)}}
	if l.step(context.Background(), st, func() int { return 0 }) || st.statsN+st.candN != 0 || len(cmds) != 0 {
		t.Errorf("step during sAlpha's pacing: stats %d, candidate %d, sent %q; want nothing", st.statsN, st.candN, cmds)
	}
	r.lastSent = now.Add(-2 * time.Minute)
	st.delivery = nil
	l.judge = func(ctx context.Context, spec ToolSpec, res *toolResult) verdict { return verdict{Level: levelUnknown} }
	if !l.step(context.Background(), st, func() int { return 0 }) || len(cmds) != 1 {
		t.Errorf("step after the pacing: sent %q, want one re-ask", cmds)
	}
}

// The row detail's sAlpha section (frontend/app.js salphaSection): a re-ask
// that declined says it was asked again and declined; the "Later report" note
// only goes with a later report (after the declined branch has returned).
func TestReaskDetailCopy(t *testing.T) {
	b, err := frontendFS.ReadFile("frontend/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	start := strings.Index(js, "function salphaSection(")
	end := strings.Index(js, "function drawDetail(")
	if start < 0 || end < start {
		t.Fatalf("app.js: salphaSection not found (start %d, end %d)", start, end)
	}
	fn := js[start:end]
	declined := strings.Index(fn, "'sAlpha did not generate a report'")
	reaskDeclined := strings.Index(fn, "'sAlpha gave none at the time of the call; asked again later, it declined.'")
	ret := strings.Index(fn[max(declined, 0):], "return sec;")
	later := strings.Index(fn, "'Later report: sAlpha gave none at the time of the call and was asked again.'")
	if declined < 0 || reaskDeclined < declined || ret < 0 || reaskDeclined > declined+ret {
		t.Errorf("salphaSection: the re-ask decline note (at %d) must sit in the declined branch (from %d, returns at +%d)", reaskDeclined, declined, ret)
	}
	if later < 0 || later < declined+ret {
		t.Errorf("salphaSection: the \"Later report\" note (at %d) must come after the declined branch returns (at %d)", later, declined+ret)
	}
	if strings.Count(js, "Later report:") != 1 {
		t.Errorf("app.js has %d \"Later report:\" notes, want 1", strings.Count(js, "Later report:"))
	}
}

// A follow-up header is sent again without the reply only when Telegram
// refused the message replied to, and never after a stop.
func TestReaskReplyRefused(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"no error", live, nil, false},
		{"REPLY_MESSAGE_ID_INVALID", live, tgerr.New(400, "REPLY_MESSAGE_ID_INVALID"), true},
		{"MESSAGE_ID_INVALID", live, tgerr.New(400, "MESSAGE_ID_INVALID"), true},
		{"wrapped REPLY_MESSAGE_ID_INVALID", live, fmt.Errorf("send text: %w", tgerr.New(400, "REPLY_MESSAGE_ID_INVALID")), true},
		{"FLOOD_WAIT", live, tgerr.New(420, "FLOOD_WAIT_30"), false},
		{"other Telegram error", live, tgerr.New(500, "INTERNAL"), false},
		{"network error", live, errors.New("connection reset"), false},
		{"deadline", live, context.DeadlineExceeded, false},
		{"REPLY_MESSAGE_ID_INVALID after a stop", stopped, tgerr.New(400, "REPLY_MESSAGE_ID_INVALID"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reaskReplyRefused(tc.ctx, tc.err); got != tc.want {
				t.Errorf("reaskReplyRefused(ctx err %v, %v) = %v, want %v", tc.ctx.Err(), tc.err, got, tc.want)
			}
		})
	}
}

// fakeSendInvoker answers messages.sendMessage with errs[n] for the n-th
// send (nil: sent, as message id 77) and records whether each was a reply.
// stop, when set, is called during the first send (a stop while it is in
// flight).
type fakeSendInvoker struct {
	errs    []error
	stop    func()
	replies []bool // per send: sent as a reply
}

func (f *fakeSendInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	req, ok := input.(*tg.MessagesSendMessageRequest)
	if !ok {
		return fmt.Errorf("unexpected request %T", input)
	}
	n := len(f.replies)
	f.replies = append(f.replies, req.ReplyTo != nil)
	if n == 0 && f.stop != nil {
		f.stop()
	}
	if n < len(f.errs) && f.errs[n] != nil {
		return f.errs[n]
	}
	var b bin.Buffer
	if err := (&tg.UpdateShortSentMessage{ID: 77}).Encode(&b); err != nil {
		return err
	}
	return output.Decode(&b)
}

// With the user account, a follow-up header refused as a reply is sent once
// more without it only for a reply-specific error; any other error, or a stop
// during the send, leaves the single send (it may have reached Telegram).
func TestReaskFollowUpUserAccountReply(t *testing.T) {
	replyErr := tgerr.New(400, "REPLY_MESSAGE_ID_INVALID")
	for _, tc := range []struct {
		name        string
		errs        []error
		stop        bool
		wantReplies []bool // per send: as a reply
		wantID      int64
		wantErr     bool
	}{
		{"sent as a reply", nil, false, []bool{true}, 77, false},
		{"REPLY_MESSAGE_ID_INVALID: sent again without the reply", []error{replyErr}, false, []bool{true, false}, 77, false},
		{"MESSAGE_ID_INVALID: sent again without the reply", []error{tgerr.New(400, "MESSAGE_ID_INVALID")}, false, []bool{true, false}, 77, false},
		{"network error: not sent again", []error{errors.New("connection reset")}, false, []bool{true}, 0, true},
		{"other Telegram error: not sent again", []error{tgerr.New(500, "INTERNAL")}, false, []bool{true}, 0, true},
		{"FLOOD_WAIT: not sent again", []error{tgerr.New(420, "FLOOD_WAIT_30")}, false, []bool{true}, 0, true},
		{"stopped during the send: not sent again", []error{replyErr}, true, []bool{true}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inv := &fakeSendInvoker{errs: tc.errs}
			if tc.stop {
				inv.stop = cancel
			}
			s := &scanner{cfg: &config{NotifyPeer: "me"}, sender: message.NewSender(tg.NewClient(inv)), notifyPeer: &tg.InputPeerSelf{}}
			res := &toolResult{Spec: testSAlphaSpec(), Status: investigationCompleted,
				Replies: []*tg.Message{{ID: 2, Message: "sAlpha research: 3 smart wallets bought."}}}
			id, err := s.sendReaskFollowUp(ctx, reaskFollowUp{Header: "🕐 sAlpha report", ReplyTo: 4242, Result: res})
			if !slices.Equal(inv.replies, tc.wantReplies) {
				t.Errorf("sends (as a reply each) %v, want %v", inv.replies, tc.wantReplies)
			}
			if id != tc.wantID || (err != nil) != tc.wantErr {
				t.Errorf("sendReaskFollowUp = %d, %v; want id %d, error %v", id, err, tc.wantID, tc.wantErr)
			}
		})
	}
}
