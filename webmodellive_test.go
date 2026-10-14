package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// GET /api/model-live: the model's scores at the time of each first call, as
// one compact row per scored call (webmodellive.go).

// webModelLiveJSON is the body of GET /api/model-live as a client reads it;
// every row kept as its raw JSON, so a test compares it byte for byte.
type webModelLiveJSON struct {
	Format     int               `json:"format"`
	Buckets    []string          `json:"buckets"`
	Horizons   []string          `json:"horizons"`
	FirstCalls int               `json:"first_calls"`
	Scored     int               `json:"scored"`
	Versions   []string          `json:"versions"`
	Columns    []string          `json:"columns"`
	Rows       []json.RawMessage `json:"rows"`
}

func decodeModelLive(tb testing.TB, body []byte) webModelLiveJSON {
	tb.Helper()
	var m webModelLiveJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		tb.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// modelLiveRows: five first calls at the same time, none with a score yet.
func modelLiveRows(at time.Time) []ScoutWebRow {
	var rows []ScoutWebRow
	for _, id := range []int{10, 20, 30, 40, 50} {
		r := modelRow(id, at, nil)
		r.Model = nil
		rows = append(rows, r)
	}
	return rows
}

// modelLivePreds: the stored scores of modelLiveRows' calls, in call id order
// (as webPredictionsSQL reads them): 10 scored two minutes after the call with
// every bucket and passed in all three states; 20 scored a day and a second
// after (back-scored: not shown); 30 with an unknown bucket beside its long
// one; 40 of another version with a NaN runner (and so collapse); 50 none.
func modelLivePreds(at time.Time) []ScoutWebPrediction {
	pred := func(call int, version, bucket string, after time.Duration, runner float64, passed *bool) ScoutWebPrediction {
		return ScoutWebPrediction{CallID: call, Version: version, Bucket: bucket, Runner: fp(runner), Collapse: fp(runner / 2),
			Rank: fp(90), Passed: passed, CreatedAt: at.Add(after)}
	}
	return []ScoutWebPrediction{
		pred(10, "v1", "short", 2*time.Minute, 0.31, bp(false)),
		pred(10, "v1", "3day", 2*time.Minute, 0.2, bp(false)),
		pred(10, "v1", "medium", 2*time.Minute, 0.12, bp(true)),
		pred(10, "v1", "long", 2*time.Minute, 0.06, nil),
		pred(20, "v1", "medium", 24*time.Hour+time.Second, 0.5, bp(true)),
		pred(30, "v1", "weekly", time.Minute, 0.7, bp(true)),
		pred(30, "v1", "long", time.Minute, 0.4, nil),
		pred(40, "v2", "medium", time.Minute, math.NaN(), bp(true)),
	}
}

// modelLiveSnapshot builds a snapshot of modelLiveRows with modelLivePreds
// attached, the way the database path does.
func modelLiveSnapshot(tb testing.TB, prev *webSnapshot) *webSnapshot {
	tb.Helper()
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	rows := modelLiveRows(at)
	attachWebModel(rows, modelLivePreds(at))
	return mustWebSnapshot(tb, rows, 0, prev)
}

// TestWebModelLiveRows: one row per first call with a score shown, values in
// the order of the columns: a scored call; a call scored more than 24 hours
// after it left out; passed true, false and null; an unknown bucket ignored;
// NaN sent as null; a bucket the score does not have all null.
func TestWebModelLiveRows(t *testing.T) {
	snap := modelLiveSnapshot(t, nil)
	if snap.modelLive == nil {
		t.Fatal("no model-live body with ModelScores on")
	}
	m := decodeModelLive(t, snap.modelLive.plain)
	wantCols := []string{"call_id", "version", "after_s",
		"runner_short", "collapse_short", "rank_short", "passed_short",
		"runner_3day", "collapse_3day", "rank_3day", "passed_3day",
		"runner_medium", "collapse_medium", "rank_medium", "passed_medium",
		"runner_long", "collapse_long", "rank_long", "passed_long"}
	if !slices.Equal(m.Columns, wantCols) {
		t.Fatalf("columns %v, want %v", m.Columns, wantCols)
	}
	if m.Format != webModelLiveFormat || !slices.Equal(m.Buckets, ScoutWebModelBuckets[:]) ||
		!slices.Equal(m.Horizons, ScoutWebModelHorizons[:]) {
		t.Fatalf("format %d buckets %v horizons %v", m.Format, m.Buckets, m.Horizons)
	}
	if m.FirstCalls != 5 || m.Scored != 3 {
		t.Fatalf("first_calls %d scored %d, want 5 and 3", m.FirstCalls, m.Scored)
	}
	// v1 scores two rows, v2 one: most frequent first
	if !slices.Equal(m.Versions, []string{"v1", "v2"}) {
		t.Fatalf("versions %v, want [v1 v2]", m.Versions)
	}
	want := []string{
		`[10,0,120,0.31,0.155,90,false,0.2,0.1,90,false,0.12,0.06,90,true,0.06,0.03,90,null]`,
		`[30,0,60,null,null,null,null,null,null,null,null,null,null,null,null,0.4,0.2,90,null]`,
		`[40,1,60,null,null,null,null,null,null,null,null,null,null,90,true,null,null,null,null]`,
	}
	if len(m.Rows) != len(want) {
		t.Fatalf("got %d rows %s, want %d", len(m.Rows), m.Rows, len(want))
	}
	for i, row := range m.Rows {
		if string(row) != want[i] {
			t.Errorf("row %d: got %s, want %s", i, row, want[i])
		}
		var vals []any
		if err := json.Unmarshal(row, &vals); err != nil || len(vals) != len(wantCols) {
			t.Errorf("row %d: %d values (%v), want %d", i, len(vals), err, len(wantCols))
		}
	}
	if !strings.HasSuffix(snap.modelLive.etag, `-m1"`) || !strings.HasPrefix(snap.modelLive.etag, `W/"`) {
		t.Errorf("ETag %s, want a weak one ending in -m1", snap.modelLive.etag)
	}
	if snap.modelLive.gz != nil && len(snap.modelLive.gz) >= len(snap.modelLive.plain) {
		t.Error("compressed body kept although not smaller")
	}
}

// TestWebModelLiveNoScores: a snapshot without any score still has the body
// (scored 0, an empty rows array), so the page can show "no scores yet".
func TestWebModelLiveNoScores(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	snap := mustWebSnapshot(t, modelLiveRows(at), 0, nil)
	m := decodeModelLive(t, snap.modelLive.plain)
	if m.FirstCalls != 5 || m.Scored != 0 || len(m.Rows) != 0 || m.Versions == nil || len(m.Versions) != 0 {
		t.Fatalf("first_calls %d scored %d rows %d versions %v, want 5 0 0 []", m.FirstCalls, m.Scored, len(m.Rows), m.Versions)
	}
	if !bytes.Contains(snap.modelLive.plain, []byte(`"rows":[]`)) {
		t.Fatalf("rows not an empty array: %s", snap.modelLive.plain)
	}
}

// TestAppendModelLiveBucket: the values of one bucket; numbers JSON cannot
// carry are null even when a score was not cleaned first.
func TestAppendModelLiveBucket(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name string
		v    ScoutWebModelBucket
		want string
	}{
		{"no bucket", ScoutWebModelBucket{Runner: fp(0.5), Passed: bp(true)}, ",null,null,null,null"},
		{"all there", ScoutWebModelBucket{Has: true, Runner: fp(0.25), Collapse: fp(0.125), Rank: fp(97.5), Passed: bp(true)}, ",0.25,0.125,97.5,true"},
		{"passed false", ScoutWebModelBucket{Has: true, Runner: fp(0), Passed: bp(false)}, ",0,null,null,false"},
		{"not finite", ScoutWebModelBucket{Has: true, Runner: &nan, Collapse: &inf, Rank: fp(1e-7)}, ",null,null,1e-07,null"},
	} {
		if got := string(appendModelLiveBucket(nil, tc.v)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestWebModelLiveVersion: the same scores give back the body of the snapshot
// before (no new compression); a change of the list alone keeps it too; a
// change of a score makes a new body with another ETag.
func TestWebModelLiveVersion(t *testing.T) {
	a := modelLiveSnapshot(t, nil)
	if b := modelLiveSnapshot(t, a); b.modelLive != a.modelLive {
		t.Fatal("same scores: the body was built again")
	}
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	rows := modelLiveRows(at)
	attachWebModel(rows, modelLivePreds(at))
	rows[4].TokenName = sp("Renamed") // the list changes, the scores do not
	b := mustWebSnapshot(t, rows, 0, a)
	if b.version == a.version || b.modelLive != a.modelLive {
		t.Fatalf("list change: version changed %t, body kept %t; want true true", b.version != a.version, b.modelLive == a.modelLive)
	}
	rows = modelLiveRows(at)
	preds := modelLivePreds(at)
	preds[2].Passed = bp(false) // call 10's medium bucket
	attachWebModel(rows, preds)
	c := mustWebSnapshot(t, rows, 0, a)
	if c.modelLive == a.modelLive || c.modelLive.etag == a.modelLive.etag {
		t.Fatalf("a changed score: ETag %s, before %s", c.modelLive.etag, a.modelLive.etag)
	}
}

// TestWebModelLiveOff: a website that does not show model scores builds and
// holds no body, also when the snapshot before had one.
func TestWebModelLiveOff(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	off := testWebConfig
	off.ModelScores = false
	rows := modelLiveRows(at)
	attachWebModel(rows, modelLivePreds(at))
	s, err := newWebSnapshot(rows, 0, nil, off)
	if err != nil {
		t.Fatal(err)
	}
	if s.modelLive != nil {
		t.Fatal("ModelScores off: a model-live body was built")
	}
	// no scores at all: the same version as a snapshot built with them on
	on := mustWebSnapshot(t, modelLiveRows(at), 0, nil)
	same, err := newWebSnapshot(modelLiveRows(at), 0, on, off)
	if err != nil {
		t.Fatal(err)
	}
	if same.version != on.version || same.modelLive != nil {
		t.Fatalf("ModelScores off after on: version kept %t, body %v; want true, nil", same.version == on.version, same.modelLive)
	}
}

// TestWebModelLiveEndpoint: GET /api/model-live answers from the snapshot: 200
// with the body built with it, gzip on request, 304 for its ETag, 400 for any
// parameter, 405 for other methods, 503 before the first snapshot, and 404
// on a website that does not show model scores.
func TestWebModelLiveEndpoint(t *testing.T) {
	ws := benchWebServer(t, 300)
	snap := ws.snap.Load()
	rec := getWeb(ws, "/api/model-live")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	checkSecurityHeaders(t, "/api/model-live", h)
	if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Cache-Control") != "no-cache" ||
		h.Get("ETag") != snap.modelLive.etag || !strings.HasSuffix(h.Get("ETag"), `-m1"`) ||
		h.Get("X-Snapshot-At") != snap.loadedAt.Format(time.RFC3339) || h.Get("Content-Encoding") != "" ||
		!strings.Contains(h.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("headers %v", h)
	}
	plain := rec.Body.Bytes()
	if !bytes.Equal(plain, snap.modelLive.plain) {
		t.Fatal("body is not the snapshot's")
	}
	m := decodeModelLive(t, plain)
	scored := 0
	for i := range snap.n {
		if snap.modelAt[i] != 0 {
			scored++
		}
	}
	if m.FirstCalls != 300 || m.Scored != scored || len(m.Rows) != scored || scored == 0 {
		t.Fatalf("first_calls %d scored %d rows %d, want 300 and %d (> 0)", m.FirstCalls, m.Scored, len(m.Rows), scored)
	}

	zrec := getWeb(ws, "/api/model-live", "Accept-Encoding", "gzip")
	if zrec.Code != 200 || zrec.Header().Get("Content-Encoding") != "gzip" || zrec.Header().Get("ETag") != snap.modelLive.etag {
		t.Fatalf("gzip: %d %v", zrec.Code, zrec.Header())
	}
	zr, err := gzip.NewReader(zrec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if unz, err := io.ReadAll(zr); err != nil || !bytes.Equal(unz, plain) {
		t.Fatalf("gzip body differs from the plain one (%v)", err)
	}
	for _, enc := range []string{"", "gzip"} {
		nm := getWeb(ws, "/api/model-live", "If-None-Match", snap.modelLive.etag, "Accept-Encoding", enc)
		if nm.Code != 304 || nm.Body.Len() != 0 || nm.Header().Get("ETag") != snap.modelLive.etag {
			t.Fatalf("If-None-Match (%q): got %d, body %d bytes", enc, nm.Code, nm.Body.Len())
		}
	}
	if other := getWeb(ws, "/api/model-live", "If-None-Match", snap.analytics.etag); other.Code != 200 {
		t.Fatalf("the analytics ETag: got %d, want 200", other.Code)
	}
	if bad := getWeb(ws, "/api/model-live?x=1"); bad.Code != 400 {
		t.Fatalf("parameter: got %d, want 400", bad.Code)
	}
	post := httptest.NewRecorder()
	ws.ServeHTTP(post, httptest.NewRequest("POST", "/api/model-live", nil))
	if post.Code != 405 {
		t.Fatalf("POST: got %d, want 405", post.Code)
	}
	ws.snap.Store(nil)
	if early := getWeb(ws, "/api/model-live"); early.Code != 503 {
		t.Fatalf("before the first snapshot: got %d, want 503", early.Code)
	}

	// a website that does not show scores: the path does not exist
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	off := testWebConfig
	off.ModelScores = false
	wsOff := mustWebServer(t, nil, off, static)
	s, err := newWebSnapshot(syntheticWebRows(300, 42), 600, nil, off)
	if err != nil {
		t.Fatal(err)
	}
	wsOff.snap.Store(s)
	for _, hdr := range [][]string{nil, {"If-None-Match", snap.modelLive.etag}} {
		rec := getWeb(wsOff, "/api/model-live", hdr...)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no such endpoint") || rec.Header().Get("ETag") != "" {
			t.Fatalf("ModelScores off (%v): %d %s, want 404", hdr, rec.Code, rec.Body)
		}
	}
}
