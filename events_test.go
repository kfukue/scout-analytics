package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestScoutEventPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   scoutEvent
		want string
	}{
		{"call", scoutEvent{Kind: scoutEventCall, CallID: 42}, `{"kind":"call","call_id":42}`},
		{"perceptor report", scoutEvent{Kind: scoutEventReport, CallID: 42, Tool: "perceptor", ID: 7, ScanKind: ScanKindLive}, `{"kind":"report","call_id":42,"tool":"perceptor","id":7,"scan_kind":"live"}`},
		{"perceptor rescan", scoutEvent{Kind: scoutEventReport, CallID: 42, Tool: "perceptor", ID: 8, ScanKind: ScanKindRescan}, `{"kind":"report","call_id":42,"tool":"perceptor","id":8,"scan_kind":"rescan"}`},
		{"salpha report", scoutEvent{Kind: scoutEventReport, CallID: 1, Tool: "salpha", ID: 2147483647, ScanKind: ScanKindLive}, `{"kind":"report","call_id":1,"tool":"salpha","id":2147483647,"scan_kind":"live"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.ev.payload()
			if err != nil {
				t.Fatalf("payload(%+v): %v", tc.ev, err)
			}
			if got != tc.want {
				t.Errorf("payload(%+v) = %s, want %s", tc.ev, got, tc.want)
			}
			if len(got) >= 8000 {
				t.Errorf("payload(%+v) is %d bytes, want under 8000", tc.ev, len(got))
			}
		})
	}
	if _, err := (scoutEvent{Kind: strings.Repeat("x", 8000)}).payload(); err == nil {
		t.Error("an 8000-byte payload must be refused")
	}
}

func TestScoutReportEvent(t *testing.T) {
	id, call := 7, 42
	code := func(s string) *string { return &s }
	inv := func(status string, callID *int) *ScoutInvestigation {
		return &ScoutInvestigation{ID: &id, CallID: callID, Status: status}
	}
	rescan := func(status string) *ScoutInvestigation {
		r := inv(status, &call)
		r.ScanKind = ScanKindRescan
		return r
	}
	for _, tc := range []struct {
		name string
		inv  *ScoutInvestigation
		code *string
		want *scoutEvent
	}{
		{"completed perceptor", inv(investigationCompleted, &call), code("perceptor"), &scoutEvent{Kind: "report", CallID: 42, Tool: "perceptor", ID: 7, ScanKind: "live"}},
		{"completed perceptor rescan", rescan(investigationCompleted), code("perceptor"), &scoutEvent{Kind: "report", CallID: 42, Tool: "perceptor", ID: 7, ScanKind: "rescan"}},
		{"failed perceptor rescan", rescan("failed"), code("perceptor"), nil},
		{"completed salpha", inv(investigationCompleted, &call), code("salpha"), &scoutEvent{Kind: "report", CallID: 42, Tool: "salpha", ID: 7, ScanKind: "live"}},
		{"failed perceptor", inv("failed", &call), code("perceptor"), nil},
		{"timeout salpha", inv("timeout", &call), code("salpha"), nil},
		{"another tool", inv(investigationCompleted, &call), code("other"), nil},
		{"no tool code", inv(investigationCompleted, &call), nil, nil},
		{"no call", inv(investigationCompleted, nil), code("perceptor"), nil},
		{"nil", nil, code("perceptor"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scoutReportEvent(tc.inv, tc.code)
			if tc.want == nil {
				if ok {
					t.Fatalf("got event %+v, want none", got)
				}
				return
			}
			if !ok || got != *tc.want {
				t.Fatalf("got %+v (%t), want %+v", got, ok, *tc.want)
			}
		})
	}
}

// eventRows is a small list for the diff tests: n rows with reports, built
// fresh each time (snapshots change their rows).
func eventRows(n int) []ScoutWebRow { return syntheticWebRows(n, 5) }

// snapWithReports builds a snapshot the way readSnapshot does, with the texts
// of the reports (which decide whether an sAlpha reply declines).
func snapWithReports(t *testing.T, rows []ScoutWebRow, reports map[int]*ScoutWebReport) *webSnapshot {
	t.Helper()
	m := webReportsFor(rows, nil, reports)
	s := mustWebSnapshot(t, rows, 3, nil)
	s.reports = m
	return s
}

type gotEvent struct {
	kind   string
	callID int
	tool   string
}

func decodeEvents(t *testing.T, evs []webEventOut) []gotEvent {
	t.Helper()
	var out []gotEvent
	for _, e := range evs {
		if e.kind == "reload" {
			out = append(out, gotEvent{kind: "reload"})
			continue
		}
		var d struct {
			CallID  int             `json:"call_id"`
			Tool    string          `json:"tool"`
			Horizon string          `json:"horizon"`
			Row     json.RawMessage `json:"row"`
		}
		dec := json.NewDecoder(bytes.NewReader(e.data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("event %s %s: %v", e.kind, e.data, err)
		}
		if d.Horizon != webEventHorizon {
			t.Errorf("event %s: horizon %q, want %q", e.data, d.Horizon, webEventHorizon)
		}
		var row webCallJSON
		rdec := json.NewDecoder(bytes.NewReader(d.Row))
		rdec.DisallowUnknownFields()
		if err := rdec.Decode(&row); err != nil {
			t.Fatalf("event row %s: %v", d.Row, err)
		}
		if row.CallID != d.CallID {
			t.Errorf("event %s: row call_id %d, want %d", e.kind, row.CallID, d.CallID)
		}
		out = append(out, gotEvent{kind: e.kind, callID: d.CallID, tool: d.Tool})
	}
	return out
}

func TestWebSnapshotEvents(t *testing.T) {
	const n = 200
	base := eventRows(n)
	baseReports := syntheticWebReports(base)
	// rows of the tests, by position: one without any report and one with both
	plain, both := -1, -1
	for i := range base {
		if base[i].PerceptorID == nil && base[i].SAlphaID == nil && plain < 0 {
			plain = i
		}
		if base[i].PerceptorID != nil && base[i].SAlphaID != nil && both < 0 {
			both = i
		}
	}
	if plain < 0 || both < 0 {
		t.Fatalf("the synthetic list needs a row without reports (%d) and one with both (%d)", plain, both)
	}
	newID := base[n-1].CallID + 2
	type edit func(rows []ScoutWebRow, reports map[int]*ScoutWebReport) []ScoutWebRow
	addRow := func(id int) edit {
		return func(rows []ScoutWebRow, _ map[int]*ScoutWebReport) []ScoutWebRow {
			r := rows[0]
			r.CallID, r.MessageID, r.ContractAddress = id, 900000+id, fmt.Sprintf("0x%040x", id)
			r.MessageDate = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			r.PerceptorVerd, r.PerceptorURL, r.PerceptorID, r.SAlphaID = nil, nil, nil, nil
			return append(rows, r)
		}
	}
	setPerceptor := func(pos int, verdict string, id int) edit {
		return func(rows []ScoutWebRow, reports map[int]*ScoutWebReport) []ScoutWebRow {
			rows[pos].PerceptorVerd, rows[pos].PerceptorID = sp(verdict), &id
			reports[id] = &ScoutWebReport{ID: id, Tool: webToolPerceptor, Verdict: verdict}
			return rows
		}
	}
	setSAlpha := func(pos int, id int, text string) edit {
		return func(rows []ScoutWebRow, reports map[int]*ScoutWebReport) []ScoutWebRow {
			rows[pos].SAlphaID = &id
			reports[id] = &ScoutWebReport{ID: id, Tool: webToolSAlpha, Text: text}
			return rows
		}
	}
	setToday := func(pos int, verdict string, at time.Time) edit {
		return func(rows []ScoutWebRow, _ map[int]*ScoutWebReport) []ScoutWebRow {
			rows[pos].PerceptorTodayVerd, rows[pos].PerceptorTodayURL, rows[pos].PerceptorTodayAt = sp(verdict), sp("https://example.com/today"), &at
			return rows
		}
	}
	todayAt := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		edits []edit
		want  []gotEvent
	}{
		{"nothing changed", nil, nil},
		{"a first Perceptor today", []edit{setToday(plain, "caution", todayAt)},
			[]gotEvent{{"report", base[plain].CallID, "perceptor_today"}}},
		{"a Perceptor today next to a call-time report", []edit{setToday(both, "unknown", todayAt)},
			[]gotEvent{{"report", base[both].CallID, "perceptor_today"}}},
		{"a call-time verdict and a Perceptor today at once", []edit{setPerceptor(plain, "red_flags", 5000001), setToday(plain, "caution", todayAt)},
			[]gotEvent{{"report", base[plain].CallID, "perceptor"}, {"report", base[plain].CallID, "perceptor_today"}}},
		{"a new row", []edit{addRow(newID)}, []gotEvent{{"call", newID, ""}}},
		{"a first Perceptor report", []edit{setPerceptor(plain, "red_flags", 5000001)},
			[]gotEvent{{"report", base[plain].CallID, "perceptor"}}},
		{"a newer Perceptor report", []edit{setPerceptor(both, "caution", 5000002)},
			[]gotEvent{{"report", base[both].CallID, "perceptor"}}},
		{"an sAlpha report", []edit{setSAlpha(plain, 6000001, "Smart money bought")},
			[]gotEvent{{"report", base[plain].CallID, "salpha"}}},
		{"an sAlpha decline", []edit{setSAlpha(plain, 6000002, "Not enough public signals to generate a report for this token.")},
			[]gotEvent{{"report", base[plain].CallID, "salpha"}}},
		{"a row went away", []edit{func(rows []ScoutWebRow, _ map[int]*ScoutWebReport) []ScoutWebRow { return rows[1:] }}, nil},
		{"a new row and reports, in row order", []edit{addRow(newID), setPerceptor(plain, "clean", 5000003), setSAlpha(both, 6000003, "text")},
			func() []gotEvent {
				p, b := gotEvent{"report", base[plain].CallID, "perceptor"}, gotEvent{"report", base[both].CallID, "salpha"}
				if both < plain {
					p, b = b, p
				}
				return []gotEvent{p, b, {"call", newID, ""}}
			}()},
		{"too many: one reload", func() []edit {
			var es []edit
			for k := 0; k <= webEventsMaxPerRefresh; k++ {
				es = append(es, addRow(newID+2*k))
			}
			return es
		}(), []gotEvent{{kind: "reload"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevRows := cloneWebRows(base)
			prev := snapWithReports(t, prevRows, baseReports)
			rows := cloneWebRows(base)
			reports := map[int]*ScoutWebReport{}
			for k, v := range baseReports {
				reports[k] = v
			}
			for _, e := range tc.edits {
				rows = e(rows, reports)
			}
			next := snapWithReports(t, rows, reports)
			got := decodeEvents(t, webSnapshotEvents(prev, next))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("events %v, want %v", got, tc.want)
			}
		})
	}
	// A row that already has a "Perceptor today": a newer re-scan sends an
	// event (also with the same verdict), the same one or none does not.
	for _, tc := range []struct {
		name string
		edit edit
		want []gotEvent
	}{
		{"the same re-scan", setToday(plain, "clean", todayAt), nil},
		{"the same re-scan, another row changed", func(rows []ScoutWebRow, reports map[int]*ScoutWebReport) []ScoutWebRow {
			return setSAlpha(both, 6000004, "text")(rows, reports)
		}, []gotEvent{{"report", base[both].CallID, "salpha"}}},
		{"a newer re-scan, same verdict", setToday(plain, "clean", todayAt.Add(time.Hour)),
			[]gotEvent{{"report", base[plain].CallID, "perceptor_today"}}},
		{"another verdict", setToday(plain, "red_flags", todayAt),
			[]gotEvent{{"report", base[plain].CallID, "perceptor_today"}}},
		{"the re-scan went away", func(rows []ScoutWebRow, _ map[int]*ScoutWebReport) []ScoutWebRow {
			rows[plain].PerceptorTodayVerd, rows[plain].PerceptorTodayURL, rows[plain].PerceptorTodayAt = nil, nil, nil
			return rows
		}, nil},
	} {
		t.Run("today: "+tc.name, func(t *testing.T) {
			prevRows := setToday(plain, "clean", todayAt)(cloneWebRows(base), nil)
			prev := snapWithReports(t, prevRows, baseReports)
			reports := maps.Clone(baseReports)
			rows := tc.edit(setToday(plain, "clean", todayAt)(cloneWebRows(base), nil), reports)
			next := snapWithReports(t, rows, reports)
			got := decodeEvents(t, webSnapshotEvents(prev, next))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("events %v, want %v (previous: clean at %s)", got, tc.want, todayAt)
			}
		})
	}
	t.Run("the first snapshot", func(t *testing.T) {
		if evs := webSnapshotEvents(nil, snapWithReports(t, cloneWebRows(base), baseReports)); evs != nil {
			t.Fatalf("got %d events for the first snapshot, want none", len(evs))
		}
	})
	t.Run("reload counts", func(t *testing.T) {
		rows := cloneWebRows(base)
		for k := 0; k <= webEventsMaxPerRefresh; k++ {
			rows = addRow(newID+2*k)(rows, nil)
		}
		evs := webSnapshotEvents(snapWithReports(t, cloneWebRows(base), baseReports), snapWithReports(t, rows, baseReports))
		want := fmt.Sprintf(`{"calls":%d,"reports":0}`, webEventsMaxPerRefresh+1)
		if len(evs) != 1 || string(evs[0].data) != want {
			t.Fatalf("got %v, want one reload with %s", evs, want)
		}
	})
	t.Run("reload counts, Perceptor today only", func(t *testing.T) {
		rows := cloneWebRows(base)
		for k := 0; k <= webEventsMaxPerRefresh; k++ {
			rows = setToday(k, "caution", todayAt)(rows, nil)
		}
		evs := webSnapshotEvents(snapWithReports(t, cloneWebRows(base), baseReports), snapWithReports(t, rows, baseReports))
		want := fmt.Sprintf(`{"calls":0,"reports":%d}`, webEventsMaxPerRefresh+1)
		if len(evs) != 1 || evs[0].kind != "reload" || string(evs[0].data) != want {
			t.Fatalf("got %v after %d re-scans, want one reload with %s", evs, webEventsMaxPerRefresh+1, want)
		}
	})
}

// sseFrame is one event of a stream, or a comment (kind "comment").
type sseFrame struct {
	id, kind, data string
}

// sseStream reads frames of an open stream on a goroutine of its own, which
// ends when the body is closed.
type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	done   chan struct{}
}

func openSSE(t *testing.T, client *http.Client, url string, hdr ...string) *sseStream {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &sseStream{resp: resp, frames: make(chan sseFrame, 256), done: make(chan struct{})}
	t.Cleanup(s.close)
	if resp.StatusCode != http.StatusOK {
		return s
	}
	go func() {
		defer close(s.done)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f != (sseFrame{}) {
					s.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				s.frames <- sseFrame{kind: "comment", data: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.kind = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data = line[6:]
			}
		}
	}()
	return s
}

func (s *sseStream) close() {
	s.resp.Body.Close()
}

// next returns the next frame that is not a comment (skip = also skip those of
// this kind).
func (s *sseStream) next(t *testing.T, skipKinds ...string) sseFrame {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-s.frames:
			if f.kind == "comment" {
				continue
			}
			skip := false
			for _, k := range skipKinds {
				skip = skip || f.kind == k
			}
			if !skip {
				return f
			}
		case <-s.done:
			t.Fatal("the stream ended")
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

// liveWebServer is fakeWebServer behind a real HTTP server.
func liveWebServer(t *testing.T, n int) (*webServer, *fakeWebDB, *httptest.Server) {
	t.Helper()
	ws, db := fakeWebServer(t, n)
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)
	return ws, db, srv
}

func TestWebEventsStream(t *testing.T) {
	ws, db, srv := liveWebServer(t, 30)
	s := openSSE(t, srv.Client(), srv.URL+"/api/events", "Accept-Encoding", "gzip")
	if s.resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", s.resp.StatusCode)
	}
	h := s.resp.Header
	for k, want := range map[string]string{"Content-Type": "text/event-stream; charset=utf-8", "Cache-Control": "no-store",
		"Content-Encoding": "", "X-Content-Type-Options": "nosniff"} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
	checkSecurityHeaders(t, "/api/events", h)
	waitFor(t, "the stream to be counted", func() bool { return ws.events.count() == 1 })

	// a new row and a new Perceptor report, read by the next refresh
	rows := cloneWebRows(db.rows)
	add := rows[0]
	add.CallID, add.MessageID, add.ContractAddress = rows[len(rows)-1].CallID+2, 777777, "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	add.MessageDate = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	add.PerceptorVerd, add.PerceptorID, add.PerceptorURL, add.SAlphaID = nil, nil, nil, nil
	rows = append(rows, add)
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := s.next(t)
	if f.kind != "call" || !strings.Contains(f.data, fmt.Sprintf(`"call_id":%d,`, add.CallID)) {
		t.Fatalf("got %+v, want a call event for %d", f, add.CallID)
	}
	if !strings.HasPrefix(f.id, ws.events.epoch+"-") {
		t.Errorf("event id %q, want one of this process (%s-…)", f.id, ws.events.epoch)
	}
	decodeEvents(t, []webEventOut{{kind: f.kind, data: []byte(f.data)}})

	// an unchanged refresh sends nothing; a Perceptor verdict does
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows = cloneWebRows(rows)
	pid := 7000001
	rows[len(rows)-1].PerceptorVerd, rows[len(rows)-1].PerceptorID = sp("red_flags"), &pid
	db.mu.Lock()
	db.reports[pid] = &ScoutWebReport{ID: pid, Tool: webToolPerceptor, Verdict: "red_flags"}
	db.mu.Unlock()
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	f = s.next(t)
	if f.kind != "report" || !strings.Contains(f.data, `"tool":"perceptor"`) || !strings.Contains(f.data, `"perceptor_verdict":"red_flags"`) {
		t.Fatalf("got %+v, want the Perceptor report of %d", f, add.CallID)
	}
}

func TestWebEventsHeartbeat(t *testing.T) {
	ws, _, srv := liveWebServer(t, 5)
	ws.sseHeartbeat = 20 * time.Millisecond
	s := openSSE(t, srv.Client(), srv.URL+"/api/events")
	pings := 0
	timeout := time.After(10 * time.Second)
	for pings < 3 {
		select {
		case f := <-s.frames:
			if f.kind == "comment" && f.data == "ping" {
				pings++
			}
		case <-timeout:
			t.Fatalf("got %d heartbeats, want 3", pings)
		}
	}
}

func TestWebEventsRequests(t *testing.T) {
	_, _, srv := liveWebServer(t, 5)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		hdr    []string
		want   int
	}{
		{"same origin", "GET", "/api/events", []string{"Origin", srv.URL}, 200},
		{"no origin", "GET", "/api/events", nil, 200},
		{"another site", "GET", "/api/events", []string{"Origin", "https://evil.example"}, 403},
		{"null origin", "GET", "/api/events", []string{"Origin", "null"}, 403},
		{"a parameter", "GET", "/api/events?x=1", nil, 400},
		{"post", "POST", "/api/events", nil, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, tc.method, srv.URL+tc.path, nil) // a fixed, valid URL
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				req.Header.Set(tc.hdr[i], tc.hdr[i+1])
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s %v: status %d, want %d", tc.method, tc.path, tc.hdr, resp.StatusCode, tc.want)
			}
			checkSecurityHeaders(t, tc.path, resp.Header)
		})
	}
}

func TestWebEventsClientCap(t *testing.T) {
	ws, _, srv := liveWebServer(t, 5)
	ws.events = newWebEventHub(2, webEventsClientBuf)
	a := openSSE(t, srv.Client(), srv.URL+"/api/events")
	b := openSSE(t, srv.Client(), srv.URL+"/api/events")
	if a.resp.StatusCode != 200 || b.resp.StatusCode != 200 {
		t.Fatalf("statuses %d, %d, want 200", a.resp.StatusCode, b.resp.StatusCode)
	}
	c := openSSE(t, srv.Client(), srv.URL+"/api/events")
	if c.resp.StatusCode != http.StatusServiceUnavailable || c.resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third stream: status %d, Retry-After %q; want 503 with Retry-After", c.resp.StatusCode, c.resp.Header.Get("Retry-After"))
	}
	if ct := c.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("503 content type %q, want JSON", ct)
	}
	a.close()
	waitFor(t, "the closed stream to leave", func() bool { return ws.events.count() == 1 })
	d := openSSE(t, srv.Client(), srv.URL+"/api/events")
	if d.resp.StatusCode != 200 {
		t.Fatalf("after one closed: status %d, want 200", d.resp.StatusCode)
	}
}

func TestWebEventHubSlowClient(t *testing.T) {
	h := newWebEventHub(10, 4)
	slow, _, _, err := h.subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	fast, _, _, err := h.subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		h.publish("call", []byte(`{"call_id":1}`))
		<-fast.ch // the fast client keeps up
	}
	select {
	case <-slow.gone:
	default:
		t.Fatal("a client 10 events behind with room for 4 must be dropped")
	}
	select {
	case <-fast.gone:
		t.Fatal("a client that keeps up must stay")
	default:
	}
	if got := h.count(); got != 1 {
		t.Errorf("count %d, want 1", got)
	}
	if h.dropped != 1 {
		t.Errorf("dropped %d, want 1", h.dropped)
	}
	h.unsubscribe(slow) // the handler's own unsubscribe after a drop: no panic
	h.close()
	select {
	case <-fast.gone:
	default:
		t.Fatal("close must end every stream")
	}
	if _, _, _, err := h.subscribe(""); !errors.Is(err, errWebEventsClosed) {
		t.Errorf("subscribe after close: %v, want errWebEventsClosed", err)
	}
}

// A stream the hub drops (too slow) ends; so does every stream when the website stops.
func TestWebEventsDroppedStreamEnds(t *testing.T) {
	ws, _, srv := liveWebServer(t, 5)
	s := openSSE(t, srv.Client(), srv.URL+"/api/events")
	waitFor(t, "the stream", func() bool { return ws.events.count() == 1 })
	ws.events.mu.Lock()
	for c := range ws.events.clients {
		ws.events.dropLocked(c)
	}
	ws.events.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a dropped stream must end")
	}
}

func TestWebEventsReplay(t *testing.T) {
	ws, _, srv := liveWebServer(t, 5)
	for i := 1; i <= 3; i++ {
		ws.events.publish("call", fmt.Appendf(nil, `{"call_id":%d}`, i))
	}
	first := ws.events.epoch + "-1"
	s := openSSE(t, srv.Client(), srv.URL+"/api/events", "Last-Event-ID", first)
	for _, want := range []string{`{"call_id":2}`, `{"call_id":3}`} {
		if f := s.next(t); f.data != want {
			t.Fatalf("replay: got %+v, want data %s", f, want)
		}
	}
	// an id of another process (the website restarted): the page reloads
	o := openSSE(t, srv.Client(), srv.URL+"/api/events", "Last-Event-ID", "zzz-5")
	if f := o.next(t); f.kind != "reload" || f.data != `{"missed":true}` {
		t.Fatalf("other process: got %+v, want a reload", f)
	}
	// the current id: nothing to replay
	c := openSSE(t, srv.Client(), srv.URL+"/api/events", "Last-Event-ID", ws.events.epoch+"-3")
	ws.events.publish("call", []byte(`{"call_id":4}`))
	if f := c.next(t); f.data != `{"call_id":4}` || f.id != ws.events.epoch+"-4" {
		t.Fatalf("got %+v, want only the new event 4", f)
	}
	// too old: the kept events no longer reach back
	for i := 0; i < webEventsReplay+5; i++ {
		ws.events.publish("call", []byte(`{"call_id":5}`))
	}
	old := openSSE(t, srv.Client(), srv.URL+"/api/events", "Last-Event-ID", first)
	if f := old.next(t); f.kind != "reload" {
		t.Fatalf("too old: got %+v, want a reload first", f)
	}
}

// Closed streams leave no goroutine behind.
func TestWebEventsNoGoroutineLeak(t *testing.T) {
	ws, _, srv := liveWebServer(t, 5)
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	before := runtime.NumGoroutine()
	var streams []*sseStream
	for i := 0; i < 8; i++ {
		streams = append(streams, openSSE(t, client, srv.URL+"/api/events"))
	}
	waitFor(t, "8 streams", func() bool { return ws.events.count() == 8 })
	for _, s := range streams {
		s.close()
		<-s.done
	}
	tr.CloseIdleConnections()
	waitFor(t, "the streams to leave the hub", func() bool { return ws.events.count() == 0 })
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines, want at most %d as before\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The server's read and write timeouts do not cut a stream; stopping the
// website ends it at once.
func TestWebEventsOutlivesServerTimeouts(t *testing.T) {
	ws, _ := fakeWebServer(t, 5)
	ws.sseHeartbeat = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.life = ctx
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := newWebHTTPServer(ws)
	hs.ReadTimeout, hs.WriteTimeout = 200*time.Millisecond, 300*time.Millisecond
	done := make(chan error, 1)
	go func() { done <- serveWebWith(ctx, ln, hs) }()

	s := openSSE(t, &http.Client{}, "http://"+ln.Addr().String()+"/api/events")
	if s.resp.StatusCode != 200 {
		t.Fatalf("status %d", s.resp.StatusCode)
	}
	start := time.Now()
	late := 0
	timeout := time.After(10 * time.Second)
	for late < 3 {
		select {
		case f := <-s.frames:
			if f.kind == "comment" && f.data == "ping" && time.Since(start) > time.Second {
				late++
			}
		case <-s.done:
			t.Fatalf("the stream ended after %s (the server's timeouts are 200/300 ms)", time.Since(start).Round(time.Millisecond))
		case <-timeout:
			t.Fatal("no heartbeats after the server's timeouts")
		}
	}
	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveWebWith: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop with a stream open")
	}
	if took := time.Since(stopped); took > 3*time.Second {
		t.Errorf("stopping took %s with a stream open, want well under the 10 s shutdown limit", took)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when the website stopped")
	}
}

// refreshAfter reads again when the read it would join began before the change.
func TestWebRefreshAfter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		since time.Duration // since = the start of the read under way + this
		want  int64         // reads in all
	}{
		{"read under way began before", time.Millisecond, 2},
		{"read under way began at the same time", 0, 2}, // a coarse clock: may be before the change
		{"read under way began after", -time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, db := fakeWebServer(t, 5)
			open := db.hold(t)
			ws.flightMu.Lock()
			first := ws.startRefreshLocked(false)
			ws.flightMu.Unlock()
			waitFor(t, "the first read", func() bool { return db.reads.Load() == 1 })
			errc := make(chan error, 1)
			go func() { errc <- ws.refreshAfter(context.Background(), first.started.Add(tc.since)) }()
			waitFor(t, "refreshAfter to join", func() bool { return ws.joins.Load() == 1 })
			open()
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
			if got := db.reads.Load(); got != tc.want {
				t.Fatalf("since = start %+v: %d reads, want %d", tc.since, got, tc.want)
			}
			// nothing under way: one read, even at the same clock reading
			if err := ws.refreshAfter(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if got := db.reads.Load(); got != tc.want+1 {
				t.Fatalf("nothing under way: %d reads, want %d", got, tc.want+1)
			}
		})
	}
}

func TestWebNotifyLoopDebounce(t *testing.T) {
	ws, db := fakeWebServer(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	kick := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.notifyLoop(ctx, kick, 100*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	for i := 0; i < 20; i++ {
		kickNow(kick)
		time.Sleep(time.Millisecond)
	}
	waitFor(t, "a read", func() bool { return db.reads.Load() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if got := db.reads.Load(); got != 1 {
		t.Fatalf("a burst of 20 notifications made %d reads, want 1", got)
	}
	kickNow(kick)
	waitFor(t, "a second read", func() bool { return db.reads.Load() == 2 })
}

// fakeNotifyConn: notes are delivered in order; after them it waits for ctx,
// or fails with fail when that is set.
type fakeNotifyConn struct {
	notes   chan *pgconn.Notification
	fail    error
	pingErr error
	pings   atomic.Int64
	closed  atomic.Bool
}

func (c *fakeNotifyConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	select {
	case n, ok := <-c.notes:
		if !ok {
			if c.fail != nil {
				return nil, c.fail
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return n, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeNotifyConn) Ping(ctx context.Context) error {
	c.pings.Add(1)
	return c.pingErr
}

func (c *fakeNotifyConn) Close(ctx context.Context) error {
	c.closed.Store(true)
	return nil
}

func TestListenScoutEvents(t *testing.T) {
	first := &fakeNotifyConn{notes: make(chan *pgconn.Notification, 4), fail: errors.New("connection reset")}
	first.notes <- &pgconn.Notification{Channel: scoutEventsChannel, Payload: `{"kind":"call","call_id":1}`}
	first.notes <- &pgconn.Notification{Channel: "other", Payload: "x"}
	close(first.notes)
	second := &fakeNotifyConn{notes: make(chan *pgconn.Notification), pingErr: nil}
	var dials atomic.Int64
	dial := func(ctx context.Context) (webNotifyConn, error) {
		switch dials.Add(1) {
		case 1, 2:
			return nil, errors.New("connection refused")
		case 3:
			return first, nil
		default:
			return second, nil
		}
	}
	kick := make(chan struct{}, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenScoutEvents(ctx, dial, kick, webListenTimings{backoffMin: time.Millisecond, backoffMax: 4 * time.Millisecond, idle: 20 * time.Millisecond})
	}()
	waitFor(t, "the second connection to be pinged", func() bool { return second.pings.Load() >= 2 })
	if !first.closed.Load() {
		t.Error("the failed connection must be closed")
	}
	// kicks: one per connection (2) and one per notification on scout_events (1)
	if got := len(kick); got != 3 {
		t.Errorf("%d kicks, want 3 (connect, notification, reconnect)", got)
	}
	if got := dials.Load(); got != 4 {
		t.Errorf("%d dials, want 4", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the listener did not stop")
	}
	if !second.closed.Load() {
		t.Error("the connection must be closed when the website stops")
	}
}

// A connection whose ping fails is replaced.
func TestListenScoutEventsDeadConnection(t *testing.T) {
	dead := &fakeNotifyConn{notes: make(chan *pgconn.Notification), pingErr: errors.New("broken pipe")}
	live := &fakeNotifyConn{notes: make(chan *pgconn.Notification)}
	var dials atomic.Int64
	dial := func(ctx context.Context) (webNotifyConn, error) {
		if dials.Add(1) == 1 {
			return dead, nil
		}
		return live, nil
	}
	kick := make(chan struct{}, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenScoutEvents(ctx, dial, kick, webListenTimings{backoffMin: time.Millisecond, backoffMax: time.Millisecond, idle: 10 * time.Millisecond})
	}()
	waitFor(t, "a new connection", func() bool { return dials.Load() >= 2 && live.pings.Load() >= 1 })
	if !dead.closed.Load() {
		t.Error("the dead connection must be closed")
	}
	cancel()
	<-done
}

// The page, the static check and the stream: the stream endpoint is not served
// as a file, and the CSP of the page lets it connect (default-src 'self').
func TestWebEventsCSP(t *testing.T) {
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, nil, webConfig{}, static)
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "connect-src") {
		t.Fatalf("CSP %q: want default-src 'self' (which covers EventSource) and no connect-src of its own", csp)
	}
	body, _ := io.ReadAll(rec.Body)
	for _, id := range []string{`id="live-state"`, `id="sound"`, `id="desktop"`, `id="new-calls"`, `id="live-notices"`} {
		if !bytes.Contains(body, []byte(id)) {
			t.Errorf("the page has no %s", id)
		}
	}
}
