package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// listenTest opens a LISTEN scout_events connection of the test's own.
func listenTest(t *testing.T, st *ScoutStore) webNotifyConn {
	t.Helper()
	conn, err := dialScoutEvents(st.Pool.Config().ConnConfig)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) // the test is over either way
	return conn
}

// nextNotification returns the next payload, or "" when none comes within wait.
func nextNotification(t *testing.T, conn webNotifyConn, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	n, err := conn.WaitForNotification(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	if err != nil {
		t.Fatalf("wait for a notification: %v", err)
	}
	if n.Channel != scoutEventsChannel {
		t.Fatalf("notification on %q, want %q", n.Channel, scoutEventsChannel)
	}
	return n.Payload
}

// TestScoutNotifyOnInsert: a new real call and a completed Perceptor or
// sAlpha report send NOTIFY scout_events; an update post, a call stored again,
// a call imported by -backfill, a failed report and another tool's report do
// not.
func TestScoutNotifyOnInsert(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", "perceptor", true)
	salpha := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", "salpha", false)
	other := mustTool(t, st, "other", "other_bot", "{ca}", "generic", false)
	conn := listenTest(t, st)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const ca = "0x1234567890123456789012345678901234567890"

	call := &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 1, MessageDate: at,
		MessageText: "new call " + ca, ContractAddress: ca, Chain: "evm", Status: "new"}
	id, created, err := st.UpsertScoutCall(ctx, call)
	if err != nil || !created {
		t.Fatalf("insert: %v, created %t", err, created)
	}
	if got, want := nextNotification(t, conn, 5*time.Second), fmt.Sprintf(`{"kind":"call","call_id":%d}`, *id); got != want {
		t.Fatalf("after a new call: got %q, want %q", got, want)
	}

	// the same post again, and an update post: no notification
	again := *call
	if _, created, err := st.UpsertScoutCall(ctx, &again); err != nil || created {
		t.Fatalf("again: %v, created %t", err, created)
	}
	upd := &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 2, MessageDate: at.Add(time.Hour),
		MessageText: "⚡ $MURKLE hit 3X called $21k → $63k peak since the call · dyor", ContractAddress: ca, Chain: "evm", Status: "new"}
	if _, created, err := st.UpsertScoutCall(ctx, upd); err != nil || !created || upd.PostKind != PostKindUpdate {
		t.Fatalf("update post: %v, created %t, kind %q", err, created, upd.PostKind)
	}
	if got := nextNotification(t, conn, 300*time.Millisecond); got != "" {
		t.Fatalf("a call stored again or an update post sent %q, want nothing", got)
	}

	// a call imported from channel history (-backfill): no notification
	const caOld = "0x2234567890123456789012345678901234567890"
	old := &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 3, MessageDate: at.Add(-30 * 24 * time.Hour),
		MessageText: "old call " + caOld, ContractAddress: caOld, Chain: "evm", Status: CallStatusBackfill}
	if _, created, err := st.UpsertScoutCall(ctx, old); err != nil || !created || old.PostKind != PostKindCall {
		t.Fatalf("backfill call: %v, created %t, kind %q", err, created, old.PostKind)
	}
	if got := nextNotification(t, conn, 300*time.Millisecond); got != "" {
		t.Fatalf("a backfill call (status %q) sent %q, want nothing", CallStatusBackfill, got)
	}

	done := at.Add(time.Minute)
	for _, tc := range []struct {
		name   string
		tool   int
		status string
		want   string // "" = no notification; %d = the investigation id
	}{
		{"perceptor completed", perc, investigationCompleted, `{"kind":"report","call_id":%d,"tool":"perceptor","id":%d}`},
		{"perceptor failed", perc, "failed", ""},
		{"salpha completed", salpha, investigationCompleted, `{"kind":"report","call_id":%d,"tool":"salpha","id":%d}`},
		{"salpha timeout", salpha, "timeout", ""},
		{"another tool", other, investigationCompleted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &ScoutInvestigation{CallID: id, ToolID: tc.tool, ContractAddress: ca, RequestText: "x",
				RequestedAt: at, CompletedAt: &done, Status: tc.status}
			iid, err := st.InsertScoutInvestigation(ctx, inv)
			if err != nil {
				t.Fatal(err)
			}
			wait := 5 * time.Second
			want := ""
			if tc.want != "" {
				want = fmt.Sprintf(tc.want, *id, *iid)
			} else {
				wait = 300 * time.Millisecond
			}
			if got := nextNotification(t, conn, wait); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestWebLiveEndToEnd: a call stored by the listener reaches an open page as
// an SSE "call" event, and its Perceptor report as a "report" event, through a
// real LISTEN, the website's refresh and its stream, well before the regular
// refresh (an hour here).
func TestWebLiveEndToEnd(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", "perceptor", true)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedWebCall(t, st, base, webSeed{Msg: 1, CA: caAlpha, Name: sp("Alpha Token"), Status: TrackDone, Unit: "usd", Entry: 0.002})

	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, st, webConfig{GMGNTemplate: defaultGMGNTemplate, Refresh: time.Hour}, static)
	lctx, cancel := context.WithCancel(ctx)
	ws.life = lctx
	srv := httptest.NewServer(ws)
	kick := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ws.notifyLoop(lctx, kick, 50*time.Millisecond)
	}()
	go func() {
		defer wg.Done()
		// a short idle time: the quiet connection is pinged several times before the insert
		listenScoutEvents(lctx, dialScoutEvents(st.Pool.Config().ConnConfig), kick,
			webListenTimings{backoffMin: 10 * time.Millisecond, backoffMax: 100 * time.Millisecond, idle: 50 * time.Millisecond})
	}()
	t.Cleanup(func() {
		cancel()
		ws.events.close()
		srv.Close()
		wg.Wait()
	})

	s := openSSE(t, srv.Client(), srv.URL+"/api/events")
	if s.resp.StatusCode != 200 {
		t.Fatalf("status %d", s.resp.StatusCode)
	}
	// the listener's first connection kicks one refresh (catching up); let it pass
	time.Sleep(300 * time.Millisecond)

	const ca = "0x00000000000000000000000000000000000abcde"
	start := time.Now()
	id, created, err := st.UpsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 50,
		MessageDate: base.Add(48 * time.Hour), MessageText: "fresh " + ca, ContractAddress: ca, Chain: "evm", Status: "new"})
	if err != nil || !created {
		t.Fatalf("insert: %v, created %t", err, created)
	}
	f := s.next(t)
	var d struct {
		CallID int         `json:"call_id"`
		Row    webCallJSON `json:"row"`
	}
	if err := json.Unmarshal([]byte(f.data), &d); err != nil {
		t.Fatalf("event %+v: %v", f, err)
	}
	if f.kind != "call" || d.CallID != *id || d.Row.ContractAddress != ca {
		t.Fatalf("got %+v, want a call event for call %d (%s)", f, *id, ca)
	}
	t.Logf("call event %s after the insert", time.Since(start).Round(time.Millisecond))

	done := base.Add(48*time.Hour + time.Minute)
	if _, err := st.InsertScoutInvestigation(ctx, &ScoutInvestigation{CallID: id, ToolID: perc, ContractAddress: ca,
		RequestText: "/scan " + ca, RequestedAt: done, CompletedAt: &done, Status: investigationCompleted, VerdictLevel: "red_flags"}); err != nil {
		t.Fatal(err)
	}
	f = s.next(t)
	if f.kind != "report" || !strings.Contains(f.data, fmt.Sprintf(`"call_id":%d,"tool":"perceptor"`, *id)) ||
		!strings.Contains(f.data, `"perceptor_verdict":"red_flags"`) {
		t.Fatalf("got %+v, want the Perceptor report of call %d", f, *id)
	}
}
