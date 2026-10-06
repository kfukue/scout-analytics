package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWebDB stands in for the database behind the website: it counts the
// reads and, while gate is set, holds every read until the gate is opened.
type fakeWebDB struct {
	reads atomic.Int64
	mu    sync.Mutex
	rows  []ScoutWebRow
	err   error
	gate  chan struct{} // nil = answer at once
	// the report texts by investigation id, the ids each read of them asked
	// for, and the error it gives
	reports    map[int]*ScoutWebReport
	asked      [][]int
	reportsErr error
}

func (f *fakeWebDB) readReports(ctx context.Context, ids []int) (map[int]*ScoutWebReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(ids))
	if f.reportsErr != nil {
		return nil, f.reportsErr
	}
	out := map[int]*ScoutWebReport{}
	for _, id := range ids {
		if r := f.reports[id]; r != nil {
			c := *r
			out[id] = &c
		}
	}
	return out, nil
}

// takeAsked returns the ids each read of the texts asked for since the last call.
func (f *fakeWebDB) takeAsked() [][]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.asked
	f.asked = nil
	return a
}

func (f *fakeWebDB) read(ctx context.Context) ([]ScoutWebRow, int, error) {
	f.reads.Add(1)
	f.mu.Lock()
	gate, rows, err := f.gate, cloneWebRows(f.rows), f.err
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	return rows, 3, err
}

// hold makes the next reads wait; the returned func lets them go (once).
func (f *fakeWebDB) hold(t *testing.T) func() {
	t.Helper()
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var once sync.Once
	open := func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(open)
	return open
}

func (f *fakeWebDB) set(rows []ScoutWebRow, err error) {
	f.mu.Lock()
	f.rows, f.err = rows, err
	f.mu.Unlock()
}

// fakeWebServer is a website over a fake database of n rows, with its first
// snapshot read.
func fakeWebServer(t *testing.T, n int) (*webServer, *fakeWebDB) {
	t.Helper()
	ws := benchWebServer(t, 1)
	db := &fakeWebDB{rows: syntheticWebRows(n, 5)}
	db.reports = syntheticWebReports(db.rows)
	ws.readRows = db.read
	ws.readReports = db.readReports
	ws.snap.Store(nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.reads.Store(0)
	db.takeAsked()
	return ws, db
}

type refreshAnswer struct {
	code int
	hdr  http.Header
	body []byte
	res  webRefreshResponse
	err  string
}

// press sends POST /api/refresh, with header pairs.
func press(t *testing.T, ws *webServer, hdr ...string) refreshAnswer {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/refresh", nil) // Host: example.com
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, req)
	a := refreshAnswer{code: rec.Code, hdr: rec.Header(), body: rec.Body.Bytes()}
	if rec.Code == 200 {
		dec := json.NewDecoder(bytes.NewReader(a.body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a.res); err != nil {
			t.Fatalf("decode %s: %v", a.body, err)
		}
	} else {
		var e map[string]any
		if err := json.Unmarshal(a.body, &e); err != nil {
			t.Fatalf("decode %s: %v", a.body, err)
		}
		a.err, _ = e["error"].(string)
	}
	return a
}

// allowManual lets the next press read the database, as if 5 s had passed.
func allowManual(ws *webServer) {
	ws.flightMu.Lock()
	ws.lastManual = time.Now().Add(-webManualRefreshEvery - time.Millisecond)
	ws.flightMu.Unlock()
}

// waitFor polls cond for up to 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWebRefreshNow: a press reads the database and swaps the snapshot in
// before it answers; the answer says so.
func TestWebRefreshNow(t *testing.T) {
	ws, db := fakeWebServer(t, 40)
	old := ws.snap.Load()
	db.set(syntheticWebRows(41, 5), nil) // one more token
	time.Sleep(2 * time.Millisecond)     // the snapshot time has millisecond steps
	a := press(t, ws)
	snap := ws.snap.Load()
	if a.code != 200 || !a.res.Refreshed || a.res.RateLimited || db.reads.Load() != 1 || snap == old || snap.n != 41 ||
		!a.res.SnapshotAt.Equal(snap.loadedAt) || a.res.SnapshotAgeSeconds < 0 || a.res.SnapshotAgeSeconds > 5 {
		t.Fatalf("press: %d %s, %d reads, %d rows", a.code, a.body, db.reads.Load(), snap.n)
	}
	if a.hdr.Get("Cache-Control") != "no-store" || a.hdr.Get("X-Snapshot-At") != snap.loadedAt.Format(time.RFC3339) ||
		a.hdr.Get("ETag") != "" {
		t.Fatalf("headers %v", a.hdr)
	}
	checkSecurityHeaders(t, "refresh", a.hdr)
	// the list shows the new token at once
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, httptest.NewRequest("GET", "/api/summary", nil))
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"imported":41`)) {
		t.Fatalf("summary after the press: %s", rec.Body)
	}
}

// TestWebRefreshCoalesces: presses while a read is under way wait for that
// read rather than starting another; so does the background refresh, and a
// press joins a read the background refresh started.
func TestWebRefreshCoalesces(t *testing.T) {
	ws, db := fakeWebServer(t, 30)
	const n = 12
	db.set(syntheticWebRows(31, 5), nil) // what the one read will find
	open := db.hold(t)
	answers := make(chan refreshAnswer, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers <- press(t, ws)
		}()
	}
	waitFor(t, "the presses to join the read", func() bool { return db.reads.Load() == 1 && ws.joins.Load() == n-1 })
	// the background refresh joins it too
	loopDone := make(chan error, 1)
	go func() { loopDone <- ws.refresh(context.Background()) }()
	waitFor(t, "the background refresh to join", func() bool { return ws.joins.Load() == n })
	open()
	wg.Wait()
	close(answers)
	for a := range answers {
		if a.code != 200 || !a.res.Refreshed || a.res.RateLimited {
			t.Errorf("a press: %d %s", a.code, a.body)
		}
	}
	if err := <-loopDone; err != nil {
		t.Fatal(err)
	}
	if r := db.reads.Load(); r != 1 || ws.snap.Load().n != 31 {
		t.Fatalf("%d reads (want 1), %d rows", r, ws.snap.Load().n)
	}

	// The other way round: a press during a read the background refresh started.
	allowManual(ws)
	db.reads.Store(0)
	ws.joins.Store(0)
	open = db.hold(t)
	go func() { loopDone <- ws.refresh(context.Background()) }()
	waitFor(t, "the background read", func() bool { return db.reads.Load() == 1 })
	pressed := make(chan refreshAnswer, 1)
	go func() { pressed <- press(t, ws) }()
	waitFor(t, "the press to join", func() bool { return ws.joins.Load() == 1 })
	open()
	if a := <-pressed; a.code != 200 || !a.res.Refreshed {
		t.Fatalf("press during the background read: %d %s", a.code, a.body)
	}
	if err := <-loopDone; err != nil || db.reads.Load() != 1 {
		t.Fatalf("background read: %v, %d reads", err, db.reads.Load())
	}
	// joining a background read does not start the 5 s interval
	if a := press(t, ws); a.code != 200 || !a.res.Refreshed || a.res.RateLimited || db.reads.Load() != 2 {
		t.Fatalf("press after joining a background read: %d %s, %d reads", a.code, a.body, db.reads.Load())
	}
}

// TestWebRefreshRateLimit: a press within 5 s of the end of the last read a
// press started gets the snapshot as it is, at once, without a read; the
// background refresh is not limited by it.
func TestWebRefreshRateLimit(t *testing.T) {
	ws, db := fakeWebServer(t, 20)
	first := press(t, ws)
	if first.code != 200 || !first.res.Refreshed || db.reads.Load() != 1 {
		t.Fatalf("first press: %d %s", first.code, first.body)
	}
	for i := 0; i < 5; i++ {
		start := time.Now()
		a := press(t, ws)
		if a.code != 200 || a.res.Refreshed || !a.res.RateLimited || !a.res.SnapshotAt.Equal(first.res.SnapshotAt) || db.reads.Load() != 1 {
			t.Fatalf("press %d within 5 s: %d %s, %d reads", i, a.code, a.body, db.reads.Load())
		}
		if time.Since(start) > time.Second {
			t.Fatalf("a limited press took %s", time.Since(start))
		}
	}
	// the background refresh still reads
	if err := ws.refresh(context.Background()); err != nil || db.reads.Load() != 2 {
		t.Fatalf("background refresh: %v, %d reads", err, db.reads.Load())
	}
	if a := press(t, ws); !a.res.RateLimited || db.reads.Load() != 2 {
		t.Fatalf("the background refresh reset the limit: %s", a.body)
	}
	// 5 s later: a read again
	allowManual(ws)
	if a := press(t, ws); a.code != 200 || !a.res.Refreshed || a.res.RateLimited || db.reads.Load() != 3 {
		t.Fatalf("press after 5 s: %d %s, %d reads", a.code, a.body, db.reads.Load())
	}
	// the interval counts from the end of the read: a slow read is not
	// followed by another one right away
	allowManual(ws)
	open := db.hold(t)
	done := make(chan refreshAnswer, 1)
	go func() { done <- press(t, ws) }()
	waitFor(t, "the slow read", func() bool { return db.reads.Load() == 4 })
	ws.flightMu.Lock()
	ws.lastManual = time.Now().Add(-time.Hour) // the start of the read is long ago …
	ws.flightMu.Unlock()
	open()
	<-done
	if a := press(t, ws); !a.res.RateLimited || db.reads.Load() != 4 { // … but its end is not
		t.Fatalf("press right after a slow read: %s, %d reads", a.body, db.reads.Load())
	}
	// a failed read starts the interval as well
	allowManual(ws)
	db.set(syntheticWebRows(20, 5), errors.New("connection refused"))
	if a := press(t, ws); a.code != 503 || db.reads.Load() != 5 {
		t.Fatalf("failed press: %d %s", a.code, a.body)
	}
	if a := press(t, ws); a.code != 200 || !a.res.RateLimited || db.reads.Load() != 5 {
		t.Fatalf("press right after a failed read: %d %s, %d reads", a.code, a.body, db.reads.Load())
	}
}

// TestWebRefreshOrigin: only pages of this very site may press; anything else
// is refused before the database is touched.
func TestWebRefreshOrigin(t *testing.T) {
	ws, db := fakeWebServer(t, 10)
	for _, origin := range [][]string{
		{"Origin", "https://evil.example"},
		{"Origin", "http://evil.example.com"},
		{"Origin", "http://example.com.evil.test"},
		{"Origin", "http://example.com:8090"}, // another port is another site
		{"Origin", "null"},
		{"Origin", ""},
		{"Origin", "example.com"},
		{"Origin", "ftp://example.com"},
		{"Origin", "http://user@example.com"},
		{"Origin", "http://example.com/path"},
		{"Origin", "http://example.com", "Origin", "http://example.com"}, // two of them
	} {
		a := press(t, ws, origin...)
		if a.code != http.StatusForbidden || a.err == "" || db.reads.Load() != 0 {
			t.Errorf("Origin %q: %d %s, %d reads", origin, a.code, a.body, db.reads.Load())
		}
		checkSecurityHeaders(t, "refused", a.hdr)
	}
	for _, origin := range [][]string{{"Origin", "http://example.com"}, {"Origin", "https://EXAMPLE.com"}, {}} {
		allowManual(ws)
		before := db.reads.Load()
		if a := press(t, ws, origin...); a.code != 200 || !a.res.Refreshed || db.reads.Load() != before+1 {
			t.Errorf("Origin %q: %d %s", origin, a.code, a.body)
		}
	}
	// the host with its port, as a browser sends it
	allowManual(ws)
	req := httptest.NewRequest("POST", "http://192.168.1.5:8090/api/refresh", nil)
	req.Header.Set("Origin", "http://192.168.1.5:8090")
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("same host and port: %d %s", rec.Code, rec.Body)
	}
}

// TestWebRefreshFailure: when the database cannot be read (or is too slow),
// the press gets an error and the website keeps serving the old snapshot.
func TestWebRefreshFailure(t *testing.T) {
	ws, db := fakeWebServer(t, 25)
	old := ws.snap.Load()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	calls1 := get("/api/calls").Body.Bytes()

	db.set(syntheticWebRows(26, 5), errors.New(`relation "scout_call_returns" does not exist`))
	a := press(t, ws)
	if a.code != http.StatusServiceUnavailable || a.err == "" || bytes.Contains(a.body, []byte("scout_call_returns")) ||
		a.hdr.Get("X-Snapshot-At") != old.loadedAt.Format(time.RFC3339) || a.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("failed read: %d %s %v", a.code, a.body, a.hdr)
	}
	var body struct {
		Error      string    `json:"error"`
		SnapshotAt time.Time `json:"snapshot_at"`
	}
	if err := json.Unmarshal(a.body, &body); err != nil || !body.SnapshotAt.Equal(old.loadedAt) {
		t.Fatalf("error body %s (%v)", a.body, err)
	}
	if ws.snap.Load() != old || !bytes.Equal(get("/api/calls").Body.Bytes(), calls1) {
		t.Fatal("a failed read changed what is served")
	}

	// too slow: the press gives up after its wait (the read goes on)
	allowManual(ws)
	db.set(syntheticWebRows(26, 5), nil)
	ws.manualWait = 50 * time.Millisecond
	open := db.hold(t)
	start := time.Now()
	if a := press(t, ws); a.code != http.StatusGatewayTimeout || a.err == "" || time.Since(start) > 5*time.Second {
		t.Fatalf("slow read: %d %s after %s", a.code, a.body, time.Since(start))
	}
	if ws.snap.Load() != old || !bytes.Equal(get("/api/calls").Body.Bytes(), calls1) {
		t.Fatal("a slow read changed what is served before it ended")
	}
	// a press now joins the read still under way (no second read) …
	joined := make(chan refreshAnswer, 1)
	ws.manualWait = 10 * time.Second
	go func() { joined <- press(t, ws) }()
	waitFor(t, "the press to join the slow read", func() bool { return ws.joins.Load() == 1 })
	open()
	if a := <-joined; a.code != 200 || !a.res.Refreshed || db.reads.Load() != 2 || ws.snap.Load().n != 26 {
		t.Fatalf("press joining the slow read: %d %s, %d reads", a.code, a.body, db.reads.Load())
	}

	// Before the first snapshot: an error, not an empty list.
	cold := benchWebServer(t, 1)
	cold.snap.Store(nil)
	cold.readRows = func(context.Context) ([]ScoutWebRow, int, error) { return nil, 0, errors.New("down") }
	if a := press(t, cold); a.code != http.StatusServiceUnavailable || bytes.Contains(a.body, []byte("snapshot_at")) {
		t.Fatalf("cold press: %d %s", a.code, a.body)
	}
	// and without a database at all
	none := benchWebServer(t, 1)
	if a := press(t, none); a.code != http.StatusServiceUnavailable {
		t.Fatalf("no database: %d %s", a.code, a.body)
	}
}

// TestWebRefreshMethods: /api/refresh takes POST only, everything else GET
// only; a refused request reads nothing.
func TestWebRefreshMethods(t *testing.T) {
	ws, db := fakeWebServer(t, 5)
	for _, c := range []struct{ method, path, allow string }{
		{"GET", "/api/refresh", "POST"}, {"HEAD", "/api/refresh", "POST"}, {"PUT", "/api/refresh", "POST"},
		{"DELETE", "/api/refresh", "POST"}, {"OPTIONS", "/api/refresh", "POST"}, {"PATCH", "/api/refresh", "POST"},
		{"GET", "/api/refresh?x=1", "POST"},
		{"POST", "/api/calls", "GET"}, {"POST", "/api/summary", "GET"}, {"POST", "/", "GET"}, {"POST", "/app.js", "GET"},
		{"POST", "/api/refresh/", "GET"}, {"POST", "/API/refresh", "GET"}, {"PUT", "/api/calls", "GET"},
	} {
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d (Allow %q)", c.method, c.path, rec.Code, rec.Header().Get("Allow"))
		}
	}
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, httptest.NewRequest("POST", "/api/refresh?x=1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST with a parameter: %d", rec.Code)
	}
	if db.reads.Load() != 0 {
		t.Fatalf("%d reads from refused requests", db.reads.Load())
	}
	if a := press(t, ws); a.code != 200 || db.reads.Load() != 1 {
		t.Fatalf("POST: %d %s", a.code, a.body)
	}
}
