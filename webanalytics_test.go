package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// webAnalyticsJSON is the body of GET /api/analytics as the page reads it.
type webAnalyticsJSON struct {
	Format         int          `json:"format"`
	Horizons       []string     `json:"horizons"`
	HorizonSeconds []int        `json:"horizon_seconds"`
	QuietBelow     int          `json:"quiet_below"`
	Verdicts       []string     `json:"verdicts"`
	Families       []string     `json:"families"`
	Dexes          []string     `json:"dexes"`
	Quotes         []string     `json:"quotes"`
	Columns        []string     `json:"columns"`
	Rows           [][]*float64 `json:"rows"`
}

func decodeAnalytics(tb testing.TB, body []byte) webAnalyticsJSON {
	tb.Helper()
	var a webAnalyticsJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		tb.Fatalf("decode analytics: %v", err)
	}
	return a
}

// withAnalyticsFields fills the Analytics page's fields of synthetic rows:
// posted DEX names (with spaces, empty, control characters), price sources of
// every kind, quote assets, no_data windows, verdicts at the call and trades.
func withAnalyticsFields(rows []ScoutWebRow, seed int64) []ScoutWebRow {
	rng := rand.New(rand.NewSource(seed))
	dexes := []string{"Uniswap V4", " Uniswap V2 ", "Pons", "Pons V2", "", "O1 Rwa", "Bad\x07Name", "Uniswap V3"}
	sources := []string{"onchain-v2", "onchain-v3", "onchain-v4", "onchain-pons", "minute", "hour"}
	quotes := []string{"WETH", "USDG", "VIRT", ""}
	verdicts := []string{levelClean, levelCaution, levelRedFlags, levelUnknown, "weird"}
	for i := range rows {
		r := &rows[i]
		if rng.Intn(5) != 0 {
			r.PostedDex = sp(dexes[rng.Intn(len(dexes))])
		}
		if r.Tracked {
			r.EntrySource = sp(sources[rng.Intn(len(sources))])
			if strings.HasPrefix(*r.EntrySource, "onchain-") {
				r.QuoteSym = sp(quotes[rng.Intn(len(quotes))])
				if rng.Intn(3) != 0 {
					r.TradesFinal = true
					n := rng.Intn(400)
					r.Trades24h = &n
				}
			}
			r.NoData = uint8(rng.Intn(4)) &^ uint8(r.HasPerf&1) // never on a window with a value
		}
		if rng.Intn(3) == 0 {
			r.VerdictAtCall = sp(verdicts[rng.Intn(len(verdicts))])
		}
		if rng.Intn(17) == 0 {
			n := 99
			r.Trades24h = &n // without TradesFinal: must not be sent
		}
	}
	return rows
}

// TestWebAnalyticsRows: every row of the body carries what the page needs,
// worked out from the row as read: flags, verdict at the call, pool family,
// posted DEX and quote asset (dictionaries), trades only when final, no_data
// bits, and the numbers of USD-priced calls only, including a rugged call's
// peak (which the list hides).
func TestWebAnalyticsRows(t *testing.T) {
	for _, n := range []int{0, 1, 600} {
		raw := withAnalyticsFields(syntheticWebRows(n, int64(n)+7), int64(n))
		snap := mustWebSnapshot(t, cloneWebRows(raw), 3, nil)
		if snap.analytics == nil || snap.analytics.rows != n {
			t.Fatalf("n=%d: no analytics, or rows %v", n, snap.analytics)
		}
		a := decodeAnalytics(t, snap.analytics.plain)
		if a.Format != webAnalyticsFormat || !slices.Equal(a.Horizons, ScoutWebHorizons[:]) || a.QuietBelow != webQuietTrades ||
			!slices.Equal(a.HorizonSeconds, []int{3600, 86400, 259200, 604800, 2592000}) || !slices.Equal(a.Columns, webAnalyticsColumns) {
			t.Fatalf("n=%d: header %+v", n, a)
		}
		if len(a.Rows) != n {
			t.Fatalf("n=%d: got %d rows, want %d", n, len(a.Rows), n)
		}
		col := map[string]int{}
		for i, c := range a.Columns {
			col[c] = i
		}
		name := func(dict []string, v *float64) string {
			if v == nil || *v < 0 {
				return ""
			}
			return dict[int(*v)]
		}
		sawRugPeak := false
		for i, row := range a.Rows {
			r := &raw[i]
			if len(row) != len(a.Columns) {
				t.Fatalf("row %d: %d values, want %d", i, len(row), len(a.Columns))
			}
			num := func(c string) float64 {
				t.Helper()
				if row[col[c]] == nil {
					t.Fatalf("row %d (call %d): %s is null", i, r.CallID, c)
				}
				return *row[col[c]]
			}
			usd := r.PriceUnit != nil && *r.PriceUnit == "usd"
			rugged := r.Rugged != nil && *r.Rugged
			wantFlags := 0
			if usd {
				wantFlags |= webAnaFlagUSD
			}
			if rugged {
				wantFlags |= webAnaFlagRugged
			}
			if r.Tracked {
				wantFlags |= webAnaFlagTracked
			}
			if got := num("call_id"); int(got) != r.CallID {
				t.Fatalf("row %d: call_id %v, want %d", i, got, r.CallID)
			}
			if got := num("t"); int64(got) != r.MessageDate.Unix() {
				t.Errorf("call %d: t %v, want %d", r.CallID, got, r.MessageDate.Unix())
			}
			if got := num("flags"); int(got) != wantFlags {
				t.Errorf("call %d: flags %v, want %d", r.CallID, got, wantFlags)
			}
			wantVerdict := "none"
			if r.VerdictAtCall != nil {
				wantVerdict = levelUnknown
				if slices.Contains([]string{levelClean, levelCaution, levelRedFlags}, *r.VerdictAtCall) {
					wantVerdict = *r.VerdictAtCall
				}
			}
			if got := a.Verdicts[int(num("verdict"))]; got != wantVerdict {
				t.Errorf("call %d: verdict %q, want %q (VerdictAtCall %v)", r.CallID, got, wantVerdict, r.VerdictAtCall)
			}
			if got, want := name(a.Families, row[col["family"]]), webPoolFamily(r.EntrySource); got != want {
				t.Errorf("call %d: family %q, want %q", r.CallID, got, want)
			}
			wantDex := ""
			if r.PostedDex != nil {
				wantDex = webAnalyticsName(*r.PostedDex)
			}
			if got := name(a.Dexes, row[col["dex"]]); got != wantDex {
				t.Errorf("call %d: dex %q, want %q", r.CallID, got, wantDex)
			}
			wantQuote := ""
			if r.QuoteSym != nil {
				wantQuote = *r.QuoteSym
			}
			if got := name(a.Quotes, row[col["quote"]]); got != wantQuote {
				t.Errorf("call %d: quote %q, want %q", r.CallID, got, wantQuote)
			}
			tr := row[col["trades_24h"]]
			switch {
			case r.TradesFinal && r.Trades24h != nil:
				if tr == nil || int(*tr) != *r.Trades24h {
					t.Errorf("call %d: trades %v, want %d", r.CallID, tr, *r.Trades24h)
				}
			case tr != nil:
				t.Errorf("call %d: trades %v, want null (final %v)", r.CallID, *tr, r.TradesFinal)
			}
			if got := num("no_data"); int(got) != int(r.NoData) {
				t.Errorf("call %d: no_data %v, want %d", r.CallID, got, r.NoData)
			}
			for j := range webPerfPerRow {
				c := a.Columns[col["ret_1h"]+j]
				got := row[col["ret_1h"]+j]
				v := r.Perf[j]
				if !usd || r.HasPerf&(1<<j) == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					if got != nil {
						t.Errorf("call %d: %s = %v, want null", r.CallID, c, *got)
					}
					continue
				}
				if got == nil || math.Abs(*got-v) > 0.05+1e-9 {
					t.Errorf("call %d: %s = %v, want %v (to 0.1)", r.CallID, c, got, v)
				}
				if rugged && j%webPerfPerHorizon == webPerfPeak {
					sawRugPeak = true
				}
			}
		}
		if n >= 600 && !sawRugPeak {
			t.Fatal("no rugged call with a peak in the test data")
		}
		// the dictionaries: most frequent first, no empty or unclean names
		for _, d := range [][]string{a.Dexes, a.Quotes, a.Families} {
			for _, s := range d {
				if s == "" || s != webAnalyticsName(s) {
					t.Errorf("dictionary entry %q", s)
				}
			}
		}
	}
}

func TestAppendAnaNum(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{0, "0"}, {-0.04, "0"}, {12.345, "12.3"}, {-99.99, "-100"}, {150.05, "150.1"},
		{123456789.5, "123456789.5"}, {3.9e47, "3.9e+47"}, {-1e12, "-1e+12"},
		{math.NaN(), "null"}, {math.Inf(-1), "null"},
	} {
		if got := string(appendAnaNum(nil, c.in)); got != c.want {
			t.Errorf("appendAnaNum(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWebPoolFamily(t *testing.T) {
	for _, c := range []struct {
		in   *string
		want string
	}{
		{nil, "untracked"}, {sp(""), "untracked"}, {sp("onchain-v2"), "v2"}, {sp("onchain-v4"), "v4"},
		{sp("onchain-pons"), "pons"}, {sp("minute"), "gecko"}, {sp("hour"), "gecko"}, {sp("onchain-"), "gecko"},
	} {
		if got := webPoolFamily(c.in); got != c.want {
			t.Errorf("webPoolFamily(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWebAnalyticsVersion: a change to any field of the Analytics page changes
// the snapshot version (and so the ETag of /api/calls) and the ETag of
// /api/analytics; the same rows again keep both, and the encoded body is
// taken over (not built and compressed again).
func TestWebAnalyticsVersion(t *testing.T) {
	raw := withAnalyticsFields(syntheticWebRows(300, 11), 3)
	base := mustWebSnapshot(t, cloneWebRows(raw), 0, nil)
	again := mustWebSnapshot(t, cloneWebRows(raw), 0, base)
	if again.version != base.version || again.analytics != base.analytics {
		t.Fatal("the same rows: version or analytics changed")
	}
	// a new snapshot whose list differs but whose analytics are the same keeps the body
	listOnly := cloneWebRows(raw)
	listOnly[0].TokenName = sp("Renamed Token")
	moved := mustWebSnapshot(t, listOnly, 0, base)
	if moved.version == base.version || moved.analytics != base.analytics {
		t.Fatalf("token name changed: version same %v, analytics body new %v", moved.version == base.version, moved.analytics != base.analytics)
	}

	find := func(ok func(r *ScoutWebRow) bool) int {
		for i := range raw {
			if ok(&raw[i]) {
				return i
			}
		}
		t.Fatal("no row for the case")
		return -1
	}
	usd := func(r *ScoutWebRow) bool { return r.PriceUnit != nil && *r.PriceUnit == "usd" }
	peakBit := uint16(1 << (1*webPerfPerHorizon + webPerfPeak)) // the 1d peak
	cases := []struct {
		name   string
		row    func(r *ScoutWebRow) bool
		change func(r *ScoutWebRow)
	}{
		{"posted dex", func(r *ScoutWebRow) bool { return true }, func(r *ScoutWebRow) { r.PostedDex = sp("Brand New DEX") }},
		{"entry source", func(r *ScoutWebRow) bool { return r.EntrySource != nil && *r.EntrySource == "onchain-v2" },
			func(r *ScoutWebRow) { r.EntrySource = sp("onchain-v4") }},
		{"quote", func(r *ScoutWebRow) bool { return r.QuoteSym != nil }, func(r *ScoutWebRow) { r.QuoteSym = sp("NEWQ") }},
		{"no data", func(r *ScoutWebRow) bool { return true }, func(r *ScoutWebRow) { r.NoData ^= 1 << 4 }},
		{"verdict at call", func(r *ScoutWebRow) bool { return r.VerdictAtCall == nil },
			func(r *ScoutWebRow) { r.VerdictAtCall = sp(levelRedFlags) }},
		{"trades final", func(r *ScoutWebRow) bool { return !r.TradesFinal && r.Tracked }, func(r *ScoutWebRow) {
			n := 7
			r.TradesFinal, r.Trades24h = true, &n
		}},
		{"trades count", func(r *ScoutWebRow) bool { return r.TradesFinal && r.Trades24h != nil }, func(r *ScoutWebRow) {
			n := *r.Trades24h + 1
			r.Trades24h = &n
		}},
		{"rugged call's peak", func(r *ScoutWebRow) bool {
			return usd(r) && r.Rugged != nil && *r.Rugged && r.HasPerf&peakBit != 0 && !math.IsNaN(r.Perf[1*webPerfPerHorizon+webPerfPeak])
		}, func(r *ScoutWebRow) { r.Perf[1*webPerfPerHorizon+webPerfPeak] += 1000 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := cloneWebRows(raw)
			i := find(c.row)
			c.change(&rows[i])
			s := mustWebSnapshot(t, rows, 0, base)
			if s.version == base.version {
				t.Errorf("call %d: version unchanged", raw[i].CallID)
			}
			if s.analytics.etag == base.analytics.etag || bytes.Equal(s.analytics.plain, base.analytics.plain) {
				t.Errorf("call %d: analytics ETag %s unchanged", raw[i].CallID, s.analytics.etag)
			}
		})
	}
}

// TestWebAnalyticsEndpoint: GET /api/analytics answers from the snapshot:
// 200 with the body built with it, gzip on request, 304 for its ETag, 400 for
// any parameter, 405 for other methods, 503 before the first snapshot.
func TestWebAnalyticsEndpoint(t *testing.T) {
	ws := benchWebServer(t, 300)
	snap := ws.snap.Load()
	rec := getWeb(ws, "/api/analytics")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	checkSecurityHeaders(t, "/api/analytics", h)
	if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Cache-Control") != "no-cache" ||
		h.Get("ETag") != snap.analytics.etag || !strings.HasPrefix(h.Get("ETag"), `W/"`) ||
		h.Get("X-Snapshot-At") != snap.loadedAt.Format(time.RFC3339) || h.Get("Content-Encoding") != "" ||
		!strings.Contains(h.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("headers %v", h)
	}
	plain := rec.Body.Bytes()
	if !bytes.Equal(plain, snap.analytics.plain) {
		t.Fatal("body is not the snapshot's")
	}
	if a := decodeAnalytics(t, plain); len(a.Rows) != 300 {
		t.Fatalf("got %d rows, want 300", len(a.Rows))
	}

	zrec := getWeb(ws, "/api/analytics", "Accept-Encoding", "gzip, deflate")
	if zrec.Code != 200 || zrec.Header().Get("Content-Encoding") != "gzip" || zrec.Header().Get("ETag") != snap.analytics.etag {
		t.Fatalf("gzip: %d %v", zrec.Code, zrec.Header())
	}
	zr, err := gzip.NewReader(zrec.Body)
	if err != nil {
		t.Fatal(err)
	}
	unz, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(unz, plain) {
		t.Fatalf("gzip body differs from the plain one (%v)", err)
	}
	if zrec.Body.Len() > 0 && len(snap.analytics.gz) >= len(plain) {
		t.Fatal("compressed body not smaller")
	}

	for _, enc := range []string{"", "gzip"} {
		nm := getWeb(ws, "/api/analytics", "If-None-Match", snap.analytics.etag, "Accept-Encoding", enc)
		if nm.Code != 304 || nm.Body.Len() != 0 || nm.Header().Get("ETag") != snap.analytics.etag {
			t.Fatalf("If-None-Match (%q): got %d, body %d bytes", enc, nm.Code, nm.Body.Len())
		}
		checkSecurityHeaders(t, "/api/analytics 304", nm.Header())
	}
	if other := getWeb(ws, "/api/analytics", "If-None-Match", `W/"other"`); other.Code != 200 {
		t.Fatalf("other ETag: got %d, want 200", other.Code)
	}
	if bad := getWeb(ws, "/api/analytics?x=1"); bad.Code != 400 {
		t.Fatalf("parameter: got %d, want 400", bad.Code)
	}
	post := httptest.NewRecorder()
	ws.ServeHTTP(post, httptest.NewRequest("POST", "/api/analytics", nil))
	if post.Code != 405 || post.Header().Get("Allow") != "GET" {
		t.Fatalf("POST: got %d, Allow %q", post.Code, post.Header().Get("Allow"))
	}
	ws.snap.Store(nil)
	if early := getWeb(ws, "/api/analytics"); early.Code != 503 {
		t.Fatalf("before the first snapshot: got %d, want 503", early.Code)
	}
}

// fakeTrades is a readTrades that counts what it is asked.
type fakeTrades struct {
	counts map[int]int // candles' events by call (absent = no candles)
	asked  [][]int
	err    error
}

func (f *fakeTrades) read(_ context.Context, ids []int) (map[int]int, error) {
	f.asked = append(f.asked, slices.Clone(ids))
	if f.err != nil {
		return nil, f.err
	}
	out := map[int]int{}
	for _, id := range ids {
		if n, ok := f.counts[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// TestWebFillTrades24h: the count is known (a number) only for calls whose
// first 24 hours are stored; such a call without candles counts 0, any other
// call is unknown (null), whatever the database holds for it. Counts once read
// are kept: a refresh reads only calls it has not counted yet, and all of them
// again after webTradesFullEvery. A failed read leaves the counts as they were
// (the rows get those) and is tried again by the next refresh.
func TestWebFillTrades24h(t *testing.T) {
	ws := &webServer{}
	f := &fakeTrades{counts: map[int]int{1: 120, 3: 99, 4: 10}}
	mk := func(final ...bool) []ScoutWebRow {
		rows := make([]ScoutWebRow, len(final))
		for i, fin := range final {
			rows[i] = ScoutWebRow{CallID: i + 1, TradesFinal: fin}
		}
		return rows
	}
	got := func(rows []ScoutWebRow) []any {
		out := make([]any, len(rows))
		for i := range rows {
			if rows[i].Trades24h == nil {
				out[i] = nil
			} else {
				out[i] = *rows[i].Trades24h
			}
		}
		return out
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// no database: all unknown
	rows := mk(true, true, false, true)
	if ws.fillTrades24h(context.Background(), rows, t0); !slices.Equal(got(rows), []any{nil, nil, nil, nil}) {
		t.Fatalf("without readTrades: got %v, want all null", got(rows))
	}

	// a failed first read (nothing held yet): all unknown, nothing kept
	ws.readTrades = (&fakeTrades{err: errors.New("db down")}).read
	rows = mk(true, true, false, true)
	if ws.fillTrades24h(context.Background(), rows, t0); !slices.Equal(got(rows), []any{nil, nil, nil, nil}) || ws.trades != nil || !ws.tradesAt.IsZero() {
		t.Fatalf("failed first read: got %v, trades %v, at %v; want all null, nothing kept", got(rows), ws.trades, ws.tradesAt)
	}

	ws.readTrades = f.read
	steps := []struct {
		name  string
		final []bool
		at    time.Duration
		asked []int
		want  []any
	}{
		{"first read: all final calls", []bool{true, true, false, true}, 0, []int{1, 2, 4}, []any{120, 0, nil, 10}},
		{"next refresh: nothing new", []bool{true, true, false, true}, time.Minute, nil, []any{120, 0, nil, 10}},
		{"call 3 final now: only it", []bool{true, true, true, true}, 2 * time.Minute, []int{3}, []any{120, 0, 99, 10}},
		{"call 5 appears, not final", []bool{true, true, true, true, false}, 3 * time.Minute, nil, []any{120, 0, 99, 10, nil}},
		{"after the full interval: all again", []bool{true, true, true, true, true}, webTradesFullEvery, []int{1, 2, 3, 4, 5}, []any{120, 0, 99, 10, 0}},
	}
	for _, s := range steps {
		f.asked = nil
		rows := mk(s.final...)
		ws.fillTrades24h(context.Background(), rows, t0.Add(s.at))
		var asked []int
		if len(f.asked) > 0 {
			asked = f.asked[0]
		}
		if len(f.asked) > 1 || !slices.Equal(asked, s.asked) {
			t.Errorf("%s: asked %v, want %v", s.name, f.asked, s.asked)
		}
		if g := got(rows); !slices.Equal(g, s.want) {
			t.Errorf("%s: got %v, want %v", s.name, g, s.want)
		}
	}

	// a failed full read (call 6 new): the counts held stay and fill the rows,
	// the new call stays unknown, tradesAt does not move
	heldAt := ws.tradesAt
	f.err = errors.New("db down")
	f.asked = nil
	later := t0.Add(2*webTradesFullEvery + time.Minute)
	rows = mk(true, true, true, true, true, true)
	ws.fillTrades24h(context.Background(), rows, later)
	if want := []any{120, 0, 99, 10, 0, nil}; !slices.Equal(got(rows), want) {
		t.Fatalf("after a failed read: got %v, want %v", got(rows), want)
	}
	if len(ws.trades) != 5 || ws.trades[1] != 120 || !ws.tradesAt.Equal(heldAt) {
		t.Fatalf("after a failed read: trades %v at %v, want the 5 held at %v", ws.trades, ws.tradesAt, heldAt)
	}

	// the next refresh tries again, and reads all of them (still due for a full read)
	f.err = nil
	f.asked = nil
	rows = mk(true, true, true, true, true, true)
	ws.fillTrades24h(context.Background(), rows, later.Add(time.Minute))
	if want := []any{120, 0, 99, 10, 0, 0}; !slices.Equal(got(rows), want) {
		t.Fatalf("retry: got %v, want %v", got(rows), want)
	}
	if want := [][]int{{1, 2, 3, 4, 5, 6}}; len(f.asked) != 1 || !slices.Equal(f.asked[0], want[0]) || !ws.tradesAt.Equal(later.Add(time.Minute)) {
		t.Fatalf("retry: asked %v at %v, want %v at %v", f.asked, ws.tradesAt, want, later.Add(time.Minute))
	}

	// a row that is not final carries no count into the snapshot, whatever it holds
	n := 5
	snap := mustWebSnapshot(t, []ScoutWebRow{{CallID: 1, Trades24h: &n}}, 0, nil)
	if a := decodeAnalytics(t, snap.analytics.plain); a.Rows[0][7] != nil {
		t.Fatalf("not final: trades %v, want null", *a.Rows[0][7])
	}
}

// TestWebRefreshFillsTrades: a refresh through the website reads the counts
// with the rows. A failed count read does not fail the refresh: the new
// snapshot keeps the counts held, and the next refresh reads them again. On a
// cold start a failed count read still gives a snapshot (counts unknown).
func TestWebRefreshFillsTrades(t *testing.T) {
	ws, db := fakeWebServer(t, 50)
	for i := range db.rows {
		db.rows[i].TradesFinal = i%2 == 0
	}
	f := &fakeTrades{counts: map[int]int{db.rows[0].CallID: 42}}
	ws.readTrades = f.read
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := decodeAnalytics(t, ws.snap.Load().analytics.plain)
	for i, row := range a.Rows {
		tr := row[7]
		switch {
		case i == 0:
			if tr == nil || *tr != 42 {
				t.Fatalf("row 0: trades %v, want 42", tr)
			}
		case i%2 == 0:
			if tr == nil || *tr != 0 {
				t.Fatalf("row %d: trades %v, want 0", i, tr)
			}
		case tr != nil:
			t.Fatalf("row %d: trades %v, want null", i, *tr)
		}
	}
	// a failed full read: the refresh succeeds with the counts held
	f.err = errors.New("candles unreadable")
	f.asked = nil
	heldAt := ws.tradesAt.Add(-webTradesFullEvery - time.Minute) // due for a full read
	ws.tradesAt = heldAt
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatalf("refresh with a failed count read: got %v, want nil", err)
	}
	if len(f.asked) != 1 {
		t.Fatalf("count reads: got %d, want 1", len(f.asked))
	}
	if a := decodeAnalytics(t, ws.snap.Load().analytics.plain); a.Rows[0][7] == nil || *a.Rows[0][7] != 42 {
		t.Fatalf("row 0 after a failed count read: trades %v, want 42 (held)", a.Rows[0][7])
	}
	if !ws.tradesAt.Equal(heldAt) || ws.trades[db.rows[0].CallID] != 42 {
		t.Fatalf("after a failed count read: trades %v at %v, want 42 held at %v", ws.trades, ws.tradesAt, heldAt)
	}

	// the next refresh reads again
	f.err = nil
	f.asked = nil
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.asked) != 1 || ws.tradesAt.Equal(heldAt) {
		t.Fatalf("retry: got %d count reads, tradesAt %v; want 1 read and a new time", len(f.asked), ws.tradesAt)
	}

	// cold start: no snapshot yet and the count read fails: a snapshot all the same
	cold, cdb := fakeWebServer(t, 10)
	for i := range cdb.rows {
		cdb.rows[i].TradesFinal = true
	}
	cold.snap.Store(nil)
	cold.trades, cold.tradesAt = nil, time.Time{}
	cf := &fakeTrades{err: errors.New("candles unreadable")}
	cold.readTrades = cf.read
	if err := cold.refresh(context.Background()); err != nil {
		t.Fatalf("cold start with a failed count read: got %v, want nil", err)
	}
	snap := cold.snap.Load()
	if snap == nil {
		t.Fatal("cold start with a failed count read: no snapshot")
	}
	for i, row := range decodeAnalytics(t, snap.analytics.plain).Rows {
		if row[7] != nil {
			t.Fatalf("cold start row %d: trades %v, want null", i, *row[7])
		}
	}
	// the retry succeeds: the counts appear, and so the version (ETag) changes
	cf.err = nil
	if err := cold.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := cold.snap.Load()
	if after.version == snap.version {
		t.Fatalf("cold start retry: version %s unchanged, want a new one once the counts are read", after.version)
	}
	for i, row := range decodeAnalytics(t, after.analytics.plain).Rows {
		if row[7] == nil || *row[7] != 0 {
			t.Fatalf("cold start retry row %d: trades %v, want 0", i, row[7])
		}
	}
}

// TestAnalyticsPageFiles: the Analytics page is built in, loads only its own
// script (deferred, no inline script or style), builds its DOM without HTML
// parsing, and the two pages link to each other.
func TestAnalyticsPageFiles(t *testing.T) {
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(static, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	html, js, index := read("analytics.html"), read("analytics.js"), read("index.html")
	for _, banned := range []string{"<script>", "<style", " style=", "onclick=", "onload=", "http://", "https://", "//cdn"} {
		if strings.Contains(html, banned) {
			t.Errorf("analytics.html contains %q", banned)
		}
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('style'", "http://"} {
		if strings.Contains(js, banned) {
			t.Errorf("analytics.js contains %q", banned)
		}
	}
	for _, want := range []string{`<script src="analytics.js" defer></script>`, `href="./"`, "Model insights — coming soon",
		"First calls only", "late entry", "in USD", "before tax"} {
		if !strings.Contains(html, want) {
			t.Errorf("analytics.html: missing %q", want)
		}
	}
	if !strings.Contains(index, `href="analytics.html"`) {
		t.Error("index.html does not link to the Analytics page")
	}
	if !strings.Contains(js, "'api/analytics'") || !strings.Contains(js, "If-None-Match") {
		t.Error("analytics.js does not read /api/analytics with its ETag")
	}
	ws := benchWebServer(t, 3)
	for _, p := range []string{"/analytics.html", "/analytics.js"} {
		if rec := getWeb(ws, p); rec.Code != 200 {
			t.Errorf("%s: got %d, want 200", p, rec.Code)
		}
	}
}
