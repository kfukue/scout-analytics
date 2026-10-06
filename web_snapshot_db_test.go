package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawClient neither asks for gzip nor unpacks it by itself.
var rawClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

// raw asks the website as it is now: no refresh first, only the given headers.
func (fx *webFixture) raw(t *testing.T, path string, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	return rawGet(t, fx.srv.URL+path, hdr...)
}

func rawGet(t *testing.T, url string, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (fx *webFixture) refresh(t *testing.T) {
	t.Helper()
	if err := fx.web.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var webBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

const caNew = "0xabababababababababababababababababababab"

// TestWebSnapshotRefresh: requests are answered from the snapshot; a call
// stored after it appears with the next refresh, not before.
func TestWebSnapshotRefresh(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	var before webCallsJSON
	code, hdr, body := fx.raw(t, "/api/calls")
	if err := json.Unmarshal(body, &before); err != nil || code != 200 || before.Total != 9 {
		t.Fatalf("before: %d %s", code, body)
	}
	at, err := time.Parse(time.RFC3339, before.At)
	if err != nil || time.Since(at) > time.Minute || time.Since(at) < -time.Second {
		t.Fatalf("snapshot_at %q (%v)", before.At, err)
	}
	if h, err := time.Parse(time.RFC3339, hdr.Get("X-Snapshot-At")); err != nil || h.Sub(at).Abs() > time.Second {
		t.Fatalf("X-Snapshot-At %q, snapshot_at %q", hdr.Get("X-Snapshot-At"), before.At)
	}
	var sum1 map[string]any
	_, _, body = fx.raw(t, "/api/summary")
	if err := json.Unmarshal(body, &sum1); err != nil || sum1["imported"] != 9.0 || sum1["updated_at"] != before.At {
		t.Fatalf("summary before: %s (snapshot_at %s)", body, before.At)
	}

	id := seedWebCall(t, fx.st, webBase, webSeed{Msg: 50, At: 100 * time.Hour, CA: caNew, Name: sp("Brand New"), PostSym: "NEW",
		Status: TrackDone, Unit: "usd", Entry: 1, Returns: map[string][3]float64{"1d": {77, 88, -9}}})

	// not refreshed yet: the answers are the same as before, to the byte
	var still webCallsJSON
	_, _, body2 := fx.raw(t, "/api/calls")
	if err := json.Unmarshal(body2, &still); err != nil || still.Total != 9 || still.At != before.At {
		t.Fatalf("before the refresh: %s", body2)
	}
	if r := fx.rawCalls(t, "q=Brand"); r.Total != 0 {
		t.Fatalf("the new call shows before the refresh: %+v", r)
	}

	time.Sleep(5 * time.Millisecond) // the snapshot time has millisecond steps
	fx.refresh(t)
	after := fx.rawCalls(t, "")
	if after.Total != 10 || len(after.Calls) != 10 || after.Calls[0].CallID != id || strOrNil(after.Calls[0].TokenName) != "Brand New" ||
		fnum(after.Calls[0].ReturnPct) != "77" || after.At == before.At {
		t.Fatalf("after the refresh: %+v", after)
	}
	if r := fx.rawCalls(t, "q=Brand&sort=peak"); r.Total != 1 || fnum(r.Calls[0].PeakPct) != "88" {
		t.Fatalf("search after the refresh: %+v", r)
	}
	var sum2 map[string]any
	_, _, body = fx.raw(t, "/api/summary")
	if err := json.Unmarshal(body, &sum2); err != nil || sum2["imported"] != 10.0 || sum2["total_calls"] != 10.0 || sum2["updated_at"] != after.At {
		t.Fatalf("summary after: %s", body)
	}

	// The background loop does the same on its own.
	loop, err := newWebServer(fx.st, webConfig{GMGNTemplate: defaultGMGNTemplate, Refresh: 20 * time.Millisecond}, fx.web.static)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.refreshLoop(ctx); close(done) }()
	if _, err := fx.st.Pool.Exec(context.Background(), `DELETE FROM scout_calls WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for loop.snap.Load().n != 9 {
		if time.Now().After(deadline) {
			t.Fatal("the background refresh did not pick up the change")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh loop did not stop")
	}
}

// TestWebRefreshButton: "Refresh now" (POST /api/refresh) shows a call stored
// after the last snapshot at once, without waiting for the background loop
// (which does not run here); on a database error the old snapshot stays.
func TestWebRefreshButton(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	post := func(hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest("POST", fx.srv.URL+"/api/refresh", nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := rawClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	before := fx.rawCalls(t, "")
	id := seedWebCall(t, fx.st, webBase, webSeed{Msg: 50, At: 100 * time.Hour, CA: caNew, Name: sp("Brand New"), PostSym: "NEW",
		Status: TrackDone, Unit: "usd", Entry: 1, Returns: map[string][3]float64{"1h": {3, 4, -1}, "7d": {77, 88, -9}}})
	fx.ids["new"] = id
	if r := fx.rawCalls(t, "q=Brand"); r.Total != 0 {
		t.Fatalf("the new call shows before the press: %+v", r)
	}
	time.Sleep(5 * time.Millisecond) // the snapshot time has millisecond steps
	code, hdr, body := post("Origin", fx.srv.URL)
	var res webRefreshResponse
	if err := json.Unmarshal(body, &res); err != nil || code != 200 || !res.Refreshed || res.RateLimited || hdr.Get("X-Snapshot-At") == "" {
		t.Fatalf("POST /api/refresh: %d %s", code, body)
	}
	after := fx.rawCalls(t, "")
	if after.Total != before.Total+1 || after.Calls[0].CallID != id || after.At == before.At || after.At != res.SnapshotAt.Format(time.RFC3339Nano) {
		t.Fatalf("after the press: total %d (before %d), first %d, at %s (press %s)", after.Total, before.Total, after.Calls[0].CallID, after.At, res.SnapshotAt)
	}
	if w := after.Calls[0].windows(); w != "3 null null 77 null" {
		t.Fatalf("the new call's windows: %s", w)
	}
	fx.wantOrder(t, "sort=return_7d", "new", "alpha", "gamma", "beta") // 77, 10, then none (higher call id first)

	// pressed again at once: no read, the same snapshot
	snap := fx.web.snap.Load()
	if code, _, body := post(); code != 200 || !strings.Contains(string(body), `"rate_limited":true`) || fx.web.snap.Load() != snap {
		t.Fatalf("second press: %d %s", code, body)
	}
	// from another site: refused
	if code, _, _ := post("Origin", "http://evil.example"); code != 403 {
		t.Fatalf("other origin: %d", code)
	}

	// the database fails: an error, and the old snapshot stays
	fx.web.flightMu.Lock()
	fx.web.lastManual = time.Time{}
	fx.web.flightMu.Unlock()
	_, hdr1, calls1 := fx.raw(t, "/api/calls")
	old := fx.web.snap.Load()
	if _, err := fx.st.Pool.Exec(ctx, `DROP TABLE IF EXISTS scout_call_returns_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.Pool.Exec(ctx, `ALTER TABLE scout_call_returns RENAME TO scout_call_returns_gone`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := fx.st.Pool.Exec(ctx, `ALTER TABLE scout_call_returns_gone RENAME TO scout_call_returns`); err != nil {
			t.Fatal(err)
		}
	}()
	code, _, body = post()
	if code != 503 || !strings.Contains(string(body), `"error"`) || !strings.Contains(string(body), `"snapshot_at"`) || strings.Contains(string(body), "scout_call_returns") {
		t.Fatalf("press with a missing table: %d %s", code, body)
	}
	if code, hdr2, calls2 := fx.raw(t, "/api/calls"); fx.web.snap.Load() != old || code != 200 || !bytes.Equal(calls1, calls2) || hdr2.Get("ETag") != hdr1.Get("ETag") {
		t.Fatalf("after the failed press: %d", code)
	}
}

func (fx *webFixture) rawCalls(t *testing.T, query string) webCallsJSON {
	t.Helper()
	code, _, body := fx.raw(t, "/api/calls?"+query)
	var out webCallsJSON
	if err := json.Unmarshal(body, &out); err != nil || code != 200 {
		t.Fatalf("GET /api/calls?%s: %d %s (%v)", query, code, body, err)
	}
	return out
}

// TestWebRefreshFailureKeepsSnapshot: when the database cannot be read, the
// website keeps answering from the snapshot it has.
func TestWebRefreshFailureKeepsSnapshot(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	_, hdr1, calls1 := fx.raw(t, "/api/calls")
	_, _, sum1 := fx.raw(t, "/api/summary")
	old := fx.web.snap.Load()

	seedWebCall(t, fx.st, webBase, webSeed{Msg: 50, At: 100 * time.Hour, CA: caNew, Name: sp("Brand New"), Status: TrackPending})
	if _, err := fx.st.Pool.Exec(ctx, `DROP TABLE IF EXISTS scout_call_returns_gone`); err != nil { // left by an interrupted run
		t.Fatal(err)
	}
	if _, err := fx.st.Pool.Exec(ctx, `ALTER TABLE scout_call_returns RENAME TO scout_call_returns_gone`); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if _, err := fx.st.Pool.Exec(ctx, `ALTER TABLE scout_call_returns_gone RENAME TO scout_call_returns`); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer restore()

	for i := 0; i < 3; i++ {
		if err := fx.web.refresh(ctx); err == nil {
			t.Fatal("refresh with a missing table: no error")
		}
	}
	if fx.web.snap.Load() != old {
		t.Fatal("a failed refresh replaced the snapshot")
	}
	code, hdr2, calls2 := fx.raw(t, "/api/calls")
	if code != 200 || !bytes.Equal(calls1, calls2) || hdr2.Get("ETag") != hdr1.Get("ETag") {
		t.Fatalf("calls after a failed refresh: %d\n%s\n%s", code, calls1, calls2)
	}
	var a, b map[string]any
	code, _, sum2 := fx.raw(t, "/api/summary")
	if json.Unmarshal(sum1, &a) != nil || json.Unmarshal(sum2, &b) != nil || code != 200 || a["updated_at"] != b["updated_at"] || b["imported"] != 9.0 {
		t.Fatalf("summary after a failed refresh: %d %s", code, sum2)
	}
	if code, _, _ := fx.raw(t, "/api/calls", "If-None-Match", hdr1.Get("ETag")); code != 304 {
		t.Fatalf("If-None-Match after a failed refresh: %d", code)
	}
	if code, _, body := fx.raw(t, "/api/calls?q=alpha&sort=return"); code != 200 || !strings.Contains(string(body), "Alpha Token") {
		t.Fatalf("a filtered request after a failed refresh: %d %s", code, body)
	}

	// the database is back: the next refresh shows the call stored meanwhile
	restore()
	fx.refresh(t)
	if r := fx.rawCalls(t, ""); r.Total != 10 || strOrNil(r.Calls[0].TokenName) != "Brand New" {
		t.Fatalf("after the database came back: %+v", r)
	}

	// A website without a first snapshot does not invent an empty list.
	cold, err := newWebServer(fx.st, webConfig{GMGNTemplate: defaultGMGNTemplate}, fx.web.static)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/calls", "/api/summary"} {
		rec := httptest.NewRecorder()
		cold.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"error"`) || rec.Header().Get("ETag") != "" {
			t.Errorf("%s before the first snapshot: %d %s", p, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	cold.ServeHTTP(rec, httptest.NewRequest("GET", "/style.css", nil))
	if rec.Code != 200 {
		t.Errorf("the page before the first snapshot: %d", rec.Code)
	}
	// … and starting the website fails with a clear error when the first read fails.
	if _, err := fx.st.Pool.Exec(ctx, `ALTER TABLE scout_call_returns RENAME TO scout_call_returns_gone`); err != nil {
		t.Fatal(err)
	}
	restored = false
	err = runWeb(ctx, fx.st, webConfig{Addr: "127.0.0.1:0", GMGNTemplate: defaultGMGNTemplate})
	if err == nil || !strings.Contains(err.Error(), "first snapshot") || !strings.Contains(err.Error(), "scout_call_returns") {
		t.Fatalf("runWeb with an unreadable database: %v", err)
	}
}

// TestWebETag: an answer carries an ETag made of the snapshot's content and the
// question; asking again with it costs a 304; unchanged data keeps its ETag
// over a refresh; changed data gets a new one.
func TestWebETag(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	etagOf := func(path string) string {
		t.Helper()
		code, hdr, _ := fx.raw(t, path)
		e := hdr.Get("ETag")
		if code != 200 || !strings.HasPrefix(e, `W/"`) || !strings.HasSuffix(e, `"`) || hdr.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d ETag %q Cache-Control %q", path, code, e, hdr.Get("Cache-Control"))
		}
		return e
	}
	notModified := func(path, etag string, sent ...string) {
		t.Helper()
		ifNoneMatch := etag
		if len(sent) > 0 {
			ifNoneMatch = sent[0] // another way of writing it
		}
		code, hdr, body := fx.raw(t, path, "If-None-Match", ifNoneMatch)
		if code != 304 || len(body) != 0 || hdr.Get("ETag") != etag || hdr.Get("X-Snapshot-At") == "" || hdr.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s with If-None-Match %s: %d, %d bytes, ETag %q", path, etag, code, len(body), hdr.Get("ETag"))
		}
		checkSecurityHeaders(t, path+" 304", hdr)
	}
	calls := etagOf("/api/calls")
	sum := etagOf("/api/summary")
	if calls == sum {
		t.Fatal("calls and summary share an ETag")
	}
	notModified("/api/calls", calls)
	notModified("/api/summary", sum)
	notModified("/api/calls", calls, `"other", `+calls)
	notModified("/api/calls", calls, strings.TrimPrefix(calls, "W/"))
	// the same question written another way has the same ETag …
	for _, same := range []string{"/api/calls?page=1", "/api/calls?sort=date&dir=desc&horizon=1d&usd_only=0&page=1&per=50", "/api/calls?per=50&dir=desc", "/api/calls?q="} {
		if e := etagOf(same); e != calls {
			t.Errorf("%s: ETag %s, want %s", same, e, calls)
		}
	}
	if a, b := etagOf("/api/calls?q=ALPHA&sort=return"), etagOf("/api/calls?sort=return&usd_only=1&q=%20alpha"); a != b {
		t.Errorf("same search, other spelling: %s %s", a, b)
	}
	// … and every other question another one
	seen := map[string]string{calls: "/api/calls"}
	for _, other := range []string{"q=a", "q=b", "sort=return", "sort=peak", "sort=return&usd_only=0", "dir=asc", "horizon=1h", "horizon=30d",
		"sort=call_mc", "sort=latest_mc", "sort=call_mc&dir=asc", "sort=latest_mc&usd_only=0",
		"sort=return_1h", "sort=return_30d&dir=asc", "sort=return_1h&horizon=7d", "sort=return_1d", "sort=return_1d&usd_only=0",
		"usd_only=1", "verdict=clean", "verdict=not_scanned", "page=2", "per=10", "per=10&page=2", "q=a&verdict=clean"} {
		e := etagOf("/api/calls?" + other)
		if prev, dup := seen[e]; dup {
			t.Errorf("?%s has the ETag of %s", other, prev)
		}
		seen[e] = other
		if code, _, _ := fx.raw(t, "/api/calls?"+other, "If-None-Match", calls); code != 200 {
			t.Errorf("?%s with the ETag of the default list: %d", other, code)
		}
	}
	// a bad request is never "not modified"
	if code, hdr, _ := fx.raw(t, "/api/calls?sort=x", "If-None-Match", calls); code != 400 || hdr.Get("ETag") != "" {
		t.Errorf("bad request with If-None-Match: %d", code)
	}

	// Unchanged data: the ETags survive any number of refreshes, although the snapshot time moves on.
	_, hdr, _ := fx.raw(t, "/api/calls")
	at1 := hdr.Get("X-Snapshot-At")
	v1 := fx.web.snap.Load()
	time.Sleep(1100 * time.Millisecond) // X-Snapshot-At has whole seconds
	for i := 0; i < 3; i++ {
		fx.refresh(t)
	}
	v2 := fx.web.snap.Load()
	if v2 == v1 || v2.version != v1.version || !v2.loadedAt.After(v1.loadedAt) || &v2.rowJSON[0] != &v1.rowJSON[0] {
		t.Fatalf("unchanged data: version %s → %s, loaded %s → %s", v1.version, v2.version, v1.loadedAt, v2.loadedAt)
	}
	if etagOf("/api/calls") != calls || etagOf("/api/summary") != sum {
		t.Fatal("the ETags changed although the data did not")
	}
	// … and the body says when the database was last read, not when the answer was first made
	if r := fx.rawCalls(t, ""); r.At != v2.loadedAt.Format(time.RFC3339Nano) || r.At == v1.loadedAt.Format(time.RFC3339Nano) {
		t.Fatalf("snapshot_at %s after the refreshes, want %s", r.At, v2.loadedAt.Format(time.RFC3339Nano))
	}
	notModified("/api/calls", calls)
	notModified("/api/summary", sum)
	if _, hdr, _ := fx.raw(t, "/api/calls", "If-None-Match", calls); hdr.Get("X-Snapshot-At") == at1 {
		t.Errorf("X-Snapshot-At of the 304 did not move: %s", at1)
	}

	// Changed data: new ETags, full answers again.
	if _, err := fx.st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking SET token_name = 'Alpha Renamed' WHERE call_id = $1`, fx.ids["alpha"]); err != nil {
		t.Fatal(err)
	}
	fx.refresh(t)
	calls2 := etagOf("/api/calls")
	if calls2 == calls {
		t.Fatal("the data changed, the ETag did not")
	}
	if code, _, body := fx.raw(t, "/api/calls", "If-None-Match", calls); code != 200 || !strings.Contains(string(body), "Alpha Renamed") {
		t.Fatalf("old ETag after a change: %d", code)
	}
	notModified("/api/calls", calls2)
	notModified("/api/summary", sum) // the counts did not change with the name
	// a change the list does not show (an update post) still changes the summary
	sumA := etagOf("/api/summary")
	if _, err := fx.st.InsertScoutCall(context.Background(), &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 70,
		MessageDate: webBase.Add(200 * time.Hour), MessageText: updateText, ContractAddress: caAlpha, Chain: "evm", Status: CallStatusUpdate}); err != nil {
		t.Fatal(err)
	}
	fx.refresh(t)
	if code, _, body := fx.raw(t, "/api/summary", "If-None-Match", sumA); code != 200 || !strings.Contains(string(body), `"update_posts":1`) {
		t.Fatalf("summary after an update post: %d %s", code, body)
	}
	// another GMGN address is another answer
	other := mustWebServer(t, fx.st, webConfig{GMGNTemplate: "https://example.test/{ca}"}, fx.web.static)
	rec := httptest.NewRecorder()
	other.ServeHTTP(rec, httptest.NewRequest("GET", "/api/calls", nil))
	if e := rec.Header().Get("ETag"); e == "" || e == etagOf("/api/calls") {
		t.Errorf("ETag with another SCOUT_GMGN_URL: %q", e)
	}

	// Static files of the built-in page: a strong ETag from the content.
	for _, p := range []string{"/", "/app.js", "/style.css", "/favicon.svg"} {
		code, hdr, body := fx.raw(t, p)
		e := hdr.Get("ETag")
		if code != 200 || !strings.HasPrefix(e, `"`) || len(e) < 10 || hdr.Get("Cache-Control") != "no-cache" || hdr.Get("Content-Length") != fmt.Sprint(len(body)) {
			t.Fatalf("%s: %d ETag %q", p, code, e)
		}
		code, hdr2, body2 := fx.raw(t, p, "If-None-Match", e)
		if code != 304 || len(body2) != 0 || hdr2.Get("ETag") != e || hdr2.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s with If-None-Match: %d, %d bytes", p, code, len(body2))
		}
		checkSecurityHeaders(t, p+" 304", hdr2)
		if code, _, body3 := fx.raw(t, p, "If-None-Match", `"somethingelse"`); code != 200 || !bytes.Equal(body3, body) {
			t.Fatalf("%s with another ETag: %d", p, code)
		}
		// the compressed form has an ETag of its own, and is "not modified" under that one
		code, hz, _ := fx.raw(t, p, "Accept-Encoding", "gzip")
		if hz.Get("Content-Encoding") == "gzip" {
			if ez := hz.Get("ETag"); code != 200 || ez == e || !strings.HasPrefix(ez, `"`) {
				t.Fatalf("%s gzip: ETag %q (plain %q)", p, ez, e)
			} else if code, _, _ := fx.raw(t, p, "Accept-Encoding", "gzip", "If-None-Match", ez); code != 304 {
				t.Fatalf("%s gzip with If-None-Match: %d", p, code)
			}
		}
	}
	if _, ha, _ := fx.raw(t, "/"); ha.Get("ETag") == "" {
		t.Fatal("no ETag on /")
	} else if _, hb, _ := fx.raw(t, "/index.html"); hb.Get("ETag") != ha.Get("ETag") {
		t.Fatal("/ and /index.html are the same file")
	} else if _, hc, _ := fx.raw(t, "/app.js"); hc.Get("ETag") == ha.Get("ETag") {
		t.Fatal("two files share an ETag")
	}
	if code, hdr, _ := fx.raw(t, "/missing.js", "If-None-Match", "*"); code != 404 || hdr.Get("ETag") != "" {
		t.Fatalf("missing file with If-None-Match: %d", code)
	}
}

// TestWebGzip: JSON and text files are compressed for clients that ask for it,
// and only for them.
func TestWebGzip(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	for _, p := range []string{"/api/calls", "/api/calls?sort=return&usd_only=0", "/", "/app.js", "/style.css", "/favicon.svg"} {
		code, plainHdr, plain := fx.raw(t, p)
		if code != 200 || plainHdr.Get("Content-Encoding") != "" || plainHdr.Get("Vary") != "Accept-Encoding" || len(plain) < webGzipMinBytes ||
			plainHdr.Get("Content-Length") != fmt.Sprint(len(plain)) {
			t.Fatalf("%s without Accept-Encoding: %d, encoding %q, vary %q, %d bytes", p, code, plainHdr.Get("Content-Encoding"), plainHdr.Get("Vary"), len(plain))
		}
		for _, ae := range []string{"gzip", "gzip, deflate, br", "br;q=1.0, gzip;q=0.5"} {
			code, hdr, packed := fx.raw(t, p, "Accept-Encoding", ae)
			if code != 200 || hdr.Get("Content-Encoding") != "gzip" || hdr.Get("Vary") != "Accept-Encoding" || hdr.Get("Content-Length") != fmt.Sprint(len(packed)) ||
				hdr.Get("Content-Type") != plainHdr.Get("Content-Type") {
				t.Fatalf("%s with %q: %d, encoding %q, vary %q", p, ae, code, hdr.Get("Content-Encoding"), hdr.Get("Vary"))
			}
			if len(packed) >= len(plain) || !bytes.Equal(gunzip(t, packed), plain) {
				t.Fatalf("%s with %q: %d bytes packed, %d plain, or other content", p, ae, len(packed), len(plain))
			}
			checkSecurityHeaders(t, p+" gzip", hdr)
		}
		for _, ae := range []string{"identity", "br", "gzip;q=0", "deflate"} {
			if code, hdr, body := fx.raw(t, p, "Accept-Encoding", ae); code != 200 || hdr.Get("Content-Encoding") != "" || !bytes.Equal(body, plain) {
				t.Fatalf("%s with %q: %d, encoding %q", p, ae, code, hdr.Get("Content-Encoding"))
			}
		}
	}
	// Go's own client asks for gzip and unpacks it: the helper used by the other tests reads the same JSON.
	_, _, viaDefault := fx.get(t, "/api/calls?per=3")
	_, _, viaRaw := fx.raw(t, "/api/calls?per=3")
	if !bytes.Equal(viaDefault, viaRaw) {
		t.Fatalf("gzip and plain differ:\n%s\n%s", viaDefault, viaRaw)
	}
	// Small answers, errors and missing files are sent as they are. (SVG is
	// text and is compressed above; real images are not, see pic.png below.)
	for _, p := range []string{"/api/summary", "/api/calls?sort=x", "/api/nope", "/missing.js"} {
		if _, hdr, body := fx.raw(t, p, "Accept-Encoding", "gzip"); hdr.Get("Content-Encoding") != "" || len(body) == 0 {
			t.Errorf("%s: Content-Encoding %q, %d bytes", p, hdr.Get("Content-Encoding"), len(body))
		}
	}
	if _, hdr, _ := fx.raw(t, "/api/summary", "Accept-Encoding", "gzip"); hdr.Get("Vary") != "Accept-Encoding" {
		t.Errorf("summary: Vary %q", hdr.Get("Vary"))
	}

	// SCOUT_WEB_DIR: compressed on the way out, read from the folder every time, checked by modification time.
	dir := t.TempDir()
	big := "/* edited */\n" + strings.Repeat("body { color: red; }\n", 200)
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>small</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 4000)...)
	if err := os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	static, _, err := webConfig{Dir: dir}.staticFS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mustWebServer(t, fx.st, webConfig{Dir: dir, GMGNTemplate: defaultGMGNTemplate}, static))
	defer srv.Close()
	code, hdr, body := rawGet(t, srv.URL+"/style.css", "Accept-Encoding", "gzip")
	if code != 200 || hdr.Get("Content-Encoding") != "gzip" || hdr.Get("Vary") != "Accept-Encoding" || string(gunzip(t, body)) != big ||
		hdr.Get("Cache-Control") != "no-cache" || hdr.Get("Last-Modified") == "" || hdr.Get("ETag") != "" || len(body) >= len(big) {
		t.Fatalf("SCOUT_WEB_DIR style.css gzip: %d %v", code, hdr)
	}
	checkSecurityHeaders(t, "dir gzip", hdr)
	if code, h2, b2 := rawGet(t, srv.URL+"/style.css"); code != 200 || h2.Get("Content-Encoding") != "" || string(b2) != big {
		t.Fatalf("SCOUT_WEB_DIR style.css plain: %d", code)
	}
	for _, ae := range []string{"gzip", ""} {
		if code, _, b := rawGet(t, srv.URL+"/style.css", "Accept-Encoding", ae, "If-Modified-Since", hdr.Get("Last-Modified")); code != 304 || len(b) != 0 {
			t.Fatalf("SCOUT_WEB_DIR If-Modified-Since (Accept-Encoding %q): %d", ae, code)
		}
	}
	if code, h, b := rawGet(t, srv.URL+"/", "Accept-Encoding", "gzip"); code != 200 || h.Get("Content-Encoding") != "" || string(b) != "<p>small</p>" {
		t.Fatalf("SCOUT_WEB_DIR small file: %d %q", code, h.Get("Content-Encoding"))
	}
	if code, h, b := rawGet(t, srv.URL+"/pic.png", "Accept-Encoding", "gzip"); code != 200 || h.Get("Content-Encoding") != "" || !bytes.Equal(b, png) {
		t.Fatalf("SCOUT_WEB_DIR image: %d %q", code, h.Get("Content-Encoding"))
	}
	// an edit shows at once
	edited := strings.Repeat("a { b: c }\n", 300)
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, b := rawGet(t, srv.URL+"/style.css", "Accept-Encoding", "gzip"); string(gunzip(t, b)) != edited {
		t.Fatal("SCOUT_WEB_DIR: the edit does not show")
	}
}

// TestWebConcurrentRequestsDuringSwap: requests keep being answered, each from
// one consistent snapshot, while the snapshot is replaced again and again.
// Run with -race.
func TestWebConcurrentRequestsDuringSwap(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	paths := []string{"/api/calls", "/api/calls?sort=return&usd_only=0", "/api/calls?q=a&dir=asc", "/api/calls?verdict=not_scanned&horizon=1h&sort=peak",
		"/api/calls?per=2&page=3", "/api/summary", "/app.js"}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	fail := func(format string, a ...any) {
		select {
		case errs <- fmt.Sprintf(format, a...):
		default:
		}
	}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			etags := map[string]string{}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := paths[(g+i)%len(paths)]
				req := httptest.NewRequest("GET", p, nil)
				if i%3 == 0 {
					req.Header.Set("Accept-Encoding", "gzip")
				}
				if e := etags[p]; e != "" && i%2 == 0 {
					req.Header.Set("If-None-Match", e)
				}
				rec := httptest.NewRecorder()
				fx.web.ServeHTTP(rec, req)
				switch rec.Code {
				case http.StatusNotModified:
				case http.StatusOK:
					etags[p] = rec.Header().Get("ETag")
					if !strings.HasPrefix(p, "/api/calls") || rec.Header().Get("Content-Encoding") == "gzip" {
						continue
					}
					var res webCallsJSON
					if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
						fail("%s: %v", p, err)
						continue
					}
					// the list has 9 tokens, or 10 while the extra call is there
					if p == "/api/calls" && !(res.Total == 9 && len(res.Calls) == 9) && !(res.Total == 10 && len(res.Calls) == 10) {
						fail("%s: total %d with %d rows", p, res.Total, len(res.Calls))
					}
				default:
					fail("%s: status %d %s", p, rec.Code, rec.Body)
				}
			}
		}(g)
	}
	for i := 0; i < 15; i++ {
		id := seedWebCall(t, fx.st, webBase, webSeed{Msg: 100 + i, At: time.Duration(100+i) * time.Hour, CA: caNew, Name: sp("Swap"), Status: TrackPending})
		fx.refresh(t)
		if n := fx.web.snap.Load().n; n != 10 {
			t.Fatalf("round %d: %d rows, want 10", i, n)
		}
		if _, err := fx.st.Pool.Exec(ctx, `DELETE FROM scout_calls WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		fx.refresh(t)
		fx.refresh(t) // unchanged: shares the rows of the one before
	}
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := fx.web.snap.Load().n; n != 9 {
		t.Fatalf("%d rows at the end, want 9", n)
	}
}
