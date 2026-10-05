package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const xssName = `<img src=x onerror=alert(1)>`

// webSeed is one call in the website tests.
type webSeed struct {
	Msg     int
	At      time.Duration // post time after the base (0 = Msg hours)
	CA      string
	Name    *string // token_name in tracking (nil = not looked up)
	PostSym string  // symbol parsed from the post ("" = none)
	ChainSy *string // token_symbol_onchain
	Status  string
	Unit    string  // price_unit ("" = NULL)
	Entry   float64 // entry_price_usd (0 = NULL)
	Late    float64 // entry_late_price_usd (0 = NULL)
	Rugged  *bool
	// horizon → late return, late peak, late drawdown
	Returns map[string][3]float64
}

func sp(s string) *string { return &s }

// seedWebCall stores a call with its tracking row and returns; returns scout_calls.id.
func seedWebCall(t *testing.T, st *ScoutStore, base time.Time, w webSeed) int {
	t.Helper()
	ctx := context.Background()
	chain := "evm"
	if !strings.HasPrefix(w.CA, "0x") {
		chain = "solana"
	}
	at := base.Add(time.Duration(w.Msg) * time.Hour)
	if w.At != 0 {
		at = base.Add(w.At)
	}
	id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: w.Msg,
		MessageDate: at, ContractAddress: w.CA, Chain: chain, Status: CallStatusBackfill})
	if err != nil {
		t.Fatal(err)
	}
	if w.PostSym != "" {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_metrics (call_id, token_symbol, created_by, updated_by) VALUES ($1,$2,'t','t')`, *id, w.PostSym); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnsureTracking(ctx, *id, w.CA, at, 1, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	nz := func(v float64) *float64 {
		if v == 0 {
			return nil
		}
		return &v
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = $2, price_unit = $3, entry_price_usd = $4,
		entry_late_price_usd = $5, rugged = $6, token_name = $7, token_symbol_onchain = $8 WHERE call_id = $1`,
		*id, w.Status, strPtr(w.Unit), nz(w.Entry), nz(w.Late), w.Rugged, w.Name, w.ChainSy); err != nil {
		t.Fatal(err)
	}
	for h, v := range w.Returns {
		d, err := parseHorizon(h)
		if err != nil {
			t.Fatal(err)
		}
		r, g, dd := v[0], v[1], v[2]
		// the from-the-call numbers are deliberately different: the site must not use them
		if err := st.UpsertReturn(ctx, *id, horizon{Name: h, Dur: d}, horizonResult{Horizon: h, DueAt: at.Add(d), Status: "done",
			PriceUSD: 1, ReturnPct: r + 1000, MaxGainPct: g + 1000, MaxDDPct: dd - 50,
			ReturnLatePct: &r, MaxGainLatePct: &g, MaxDDLatePct: &dd}); err != nil {
			t.Fatal(err)
		}
	}
	return *id
}

type webFixture struct {
	st  *ScoutStore
	srv *httptest.Server
	ids map[string]int // name → call id
}

const (
	caAlpha = "0x1111111111111111111111111111111111111111"
	caBeta  = "0x2222222222222222222222222222222222222222"
	caGamma = "0x33333333333333333333333333333333333333aB"
	caVirt  = "0x4444444444444444444444444444444444444444"
	caXSS   = "0x5555555555555555555555555555555555555555"
	caSol   = "So11111111111111111111111111111111111111112x"
	caPct   = "0x7777777777777777777777777777777777777777"
	caErr   = "0x8888888888888888888888888888888888888888"
	caGave  = "0x9999999999999999999999999999999999999999"
)

func newWebFixture(t *testing.T, cfg webConfig) *webFixture {
	t.Helper()
	st := testStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	fx := &webFixture{st: st, ids: map[string]int{}}
	for _, w := range []struct {
		key string
		webSeed
	}{
		{"alpha", webSeed{Msg: 1, CA: caAlpha, Name: sp("Alpha Token"), PostSym: "ALPHA", Status: TrackDone, Unit: "usd", Entry: 0.002, Late: 0.003,
			Returns: map[string][3]float64{"1h": {5, 8, -2}, "1d": {50, 120, -10}, "3d": {-30, 130, -40}, "7d": {10, 140, -45}, "30d": {-90, 150, -95}}}},
		{"beta", webSeed{Msg: 2, CA: caBeta, Name: sp("Beta Coin"), ChainSy: sp("BETA"), Status: TrackDone, Unit: "usd", Entry: 1.5, Rugged: &yes,
			Returns: map[string][3]float64{"1h": {9, 11, -1}, "1d": {-20, 15, -35}}}},
		{"gamma", webSeed{Msg: 3, CA: caGamma, Name: sp(""), PostSym: "GAM", Status: TrackTracking, Unit: "usd", Entry: 0.5, Late: 0.6,
			Returns: map[string][3]float64{"1h": {1, 2, -3}}}},
		{"virt", webSeed{Msg: 4, CA: caVirt, Name: sp("Virtual Paired"), PostSym: "VP", Status: TrackDone, Unit: "VIRT", Entry: 12, Late: 13,
			Returns: map[string][3]float64{"1h": {700, 800, -5}, "1d": {999, 1999, -7}}}},
		{"xss", webSeed{Msg: 5, CA: caXSS, Name: sp(xssName), ChainSy: sp("<b>X</b>"), Status: TrackPending}},
		{"sol", webSeed{Msg: 6, CA: caSol, Status: TrackNoPool}},
		{"pct", webSeed{Msg: 7, CA: caPct, Name: sp("100%_real"), PostSym: "PCT", Status: TrackDone, Entry: 2}}, // GeckoTerminal-style row: no price_unit
		{"err", webSeed{Msg: 8, CA: caErr, Name: sp("Erratic"), Status: TrackError}},
		{"gave", webSeed{Msg: 9, CA: caGave, Name: sp("Gone"), Status: TrackGaveUp}},
	} {
		fx.ids[w.key] = seedWebCall(t, st, base, w.webSeed)
	}
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	fx.srv = httptest.NewServer(newWebHandler(st, cfg, static))
	t.Cleanup(fx.srv.Close)
	return fx
}

type webCallJSON struct {
	CallID          int      `json:"call_id"`
	MessageID       int      `json:"message_id"`
	MessageDate     string   `json:"message_date"`
	PostURL         *string  `json:"post_url"`
	ContractAddress string   `json:"contract_address"`
	TokenName       *string  `json:"token_name"`
	TokenSymbol     *string  `json:"token_symbol"`
	GMGNURL         *string  `json:"gmgn_url"`
	PriceUnit       *string  `json:"price_unit"`
	EntryPriceUSD   *float64 `json:"entry_price_usd"`
	ReturnPct       *float64 `json:"return_pct"`
	PeakPct         *float64 `json:"peak_pct"`
	DrawdownPct     *float64 `json:"drawdown_pct"`
	Rugged          *bool    `json:"rugged"`
	TrackingStatus  *string  `json:"tracking_status"`
	Perceptor       *string  `json:"perceptor_verdict"`
	CallCount       int      `json:"call_count"`
	LastCallDate    string   `json:"last_call_date"`
}

type webCallsJSON struct {
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	Per     int           `json:"per"`
	Horizon string        `json:"horizon"`
	Sort    string        `json:"sort"`
	Dir     string        `json:"dir"`
	USDOnly bool          `json:"usd_only"`
	Calls   []webCallJSON `json:"calls"`
}

func (fx *webFixture) get(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(fx.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (fx *webFixture) calls(t *testing.T, query string) webCallsJSON {
	t.Helper()
	code, hdr, body := fx.get(t, "/api/calls?"+query)
	if code != 200 {
		t.Fatalf("GET /api/calls?%s: %d %s", query, code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	var out webCallsJSON
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

// keys returns the fixture names of the calls, in order.
func (fx *webFixture) keys(cs []webCallJSON) []string {
	byID := map[int]string{}
	for k, id := range fx.ids {
		byID[id] = k
	}
	out := []string{}
	for _, c := range cs {
		out = append(out, byID[c.CallID])
	}
	return out
}

func (fx *webFixture) wantOrder(t *testing.T, query string, want ...string) webCallsJSON {
	t.Helper()
	res := fx.calls(t, query)
	if got := fx.keys(res.Calls); !(len(got) == 0 && len(want) == 0) && !reflect.DeepEqual(got, want) {
		t.Fatalf("?%s: order %v, want %v", query, got, want)
	}
	return res
}

func fnum(p *float64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

func TestWebSummary(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	code, hdr, body := fx.get(t, "/api/summary")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s %s", code, hdr.Get("Content-Type"), body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(got["updated_at"]))
	if err != nil || time.Since(at) > time.Minute || time.Since(at) < -time.Minute {
		t.Fatalf("updated_at %v (%v)", got["updated_at"], err)
	}
	delete(got, "updated_at")
	want := map[string]any{"imported": 9.0, "tracked": 5.0, "pending": 1.0, "tracking": 1.0, "done": 4.0,
		"no_pool": 1.0, "error": 1.0, "gave_up": 1.0,
		"no_usd_price": 2.0, // the VIRT-priced call, and the tracked call without a price unit
		"total_calls":  9.0, "repeat_calls": 0.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary %v\nwant    %v", got, want)
	}
	if code, _, body := fx.get(t, "/api/summary?x=1"); code != 400 || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("summary with a parameter: %d %s", code, body)
	}

	// An empty database: all zero, no error.
	if _, err := fx.st.Pool.Exec(context.Background(), `DELETE FROM scout_calls`); err != nil {
		t.Fatal(err)
	}
	_, _, body = fx.get(t, "/api/summary")
	var empty ScoutWebSummary
	if err := json.Unmarshal(body, &empty); err != nil || empty.Imported != 0 || empty.Tracked != 0 || empty.NoUSDPrice != 0 ||
		empty.TotalCalls != 0 || empty.RepeatCalls != 0 {
		t.Fatalf("empty summary %s (%v)", body, err)
	}
	if res := fx.calls(t, ""); res.Total != 0 || res.Calls == nil || len(res.Calls) != 0 {
		t.Fatalf("empty calls %+v", res)
	}
	if _, _, body := fx.get(t, "/api/calls"); !strings.Contains(string(body), `"calls":[]`) {
		t.Fatalf("an empty list must be [], got %s", body)
	}
}

func TestWebCallsOrderHorizonsAndFields(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})

	// Default: newest first, everything, 1d, page 1 of 50.
	res := fx.wantOrder(t, "", "gave", "err", "pct", "sol", "xss", "virt", "gamma", "beta", "alpha")
	if res.Total != 9 || res.Page != 1 || res.Per != 50 || res.Horizon != "1d" || res.Sort != "date" || res.Dir != "desc" || res.USDOnly {
		t.Fatalf("defaults %+v", res)
	}
	fx.wantOrder(t, "dir=asc", "alpha", "beta", "gamma", "virt", "xss", "sol", "pct", "err", "gave")
	fx.wantOrder(t, "sort=date&dir=desc&horizon=1d&usd_only=0&page=1&per=50", "gave", "err", "pct", "sol", "xss", "virt", "gamma", "beta", "alpha")

	// Return / peak: USD-priced calls only by default, NULLs last in both directions.
	if r := fx.wantOrder(t, "sort=return", "alpha", "beta", "gamma"); !r.USDOnly || r.Total != 3 {
		t.Fatalf("sort=return %+v", r)
	}
	fx.wantOrder(t, "sort=return&dir=asc", "beta", "alpha", "gamma")
	fx.wantOrder(t, "sort=peak", "alpha", "beta", "gamma")
	fx.wantOrder(t, "sort=peak&dir=asc", "beta", "alpha", "gamma")
	// 1h: beta +9, alpha +5, gamma +1; peak 11, 8, 2.
	fx.wantOrder(t, "sort=return&horizon=1h", "beta", "alpha", "gamma")
	fx.wantOrder(t, "sort=return&horizon=1h&dir=asc", "gamma", "alpha", "beta")
	fx.wantOrder(t, "sort=peak&horizon=1h&dir=asc", "gamma", "alpha", "beta")
	// 3d: only alpha has it; the others are NULL and tie → by call id, in the sort direction.
	fx.wantOrder(t, "sort=return&horizon=3d", "alpha", "gamma", "beta")
	fx.wantOrder(t, "sort=return&horizon=3d&dir=asc", "alpha", "beta", "gamma")
	// usd_only explicit: all calls; the non-USD call's 999% does not count, it sorts with the NULLs.
	if r := fx.wantOrder(t, "sort=return&usd_only=0", "alpha", "beta", "gave", "err", "pct", "sol", "xss", "virt", "gamma"); r.USDOnly || r.Total != 9 {
		t.Fatalf("usd_only=0 %+v", r)
	}
	fx.wantOrder(t, "sort=return&usd_only=0&dir=asc", "beta", "alpha", "gamma", "virt", "xss", "sol", "pct", "err", "gave")
	if r := fx.wantOrder(t, "usd_only=1", "gamma", "beta", "alpha"); !r.USDOnly || r.Total != 3 {
		t.Fatalf("usd_only=1 %+v", r)
	}

	// Each horizon reads its own late-entry columns.
	wantAlpha := map[string][3]float64{"1h": {5, 8, -2}, "1d": {50, 120, -10}, "3d": {-30, 130, -40}, "7d": {10, 140, -45}, "30d": {-90, 150, -95}}
	for h, w := range wantAlpha {
		r := fx.calls(t, "q=alpha&horizon="+h)
		if r.Horizon != h || len(r.Calls) != 1 {
			t.Fatalf("horizon %s: %+v", h, r)
		}
		c := r.Calls[0]
		if c.ReturnPct == nil || c.PeakPct == nil || c.DrawdownPct == nil || *c.ReturnPct != w[0] || *c.PeakPct != w[1] || *c.DrawdownPct != w[2] {
			t.Fatalf("horizon %s: return %s peak %s drawdown %s, want %v", h, fnum(c.ReturnPct), fnum(c.PeakPct), fnum(c.DrawdownPct), w)
		}
	}

	// Fields.
	by := map[string]webCallJSON{}
	all := fx.calls(t, "")
	for i, k := range fx.keys(all.Calls) {
		by[k] = all.Calls[i]
	}
	a := by["alpha"]
	if a.MessageID != 1 || a.MessageDate != "2026-09-01T01:00:00Z" || strOrNil(a.PostURL) != "https://t.me/scoutrobinhood/1" ||
		a.ContractAddress != caAlpha || strOrNil(a.TokenName) != "Alpha Token" || strOrNil(a.TokenSymbol) != "ALPHA" ||
		strOrNil(a.GMGNURL) != "https://gmgn.ai/robinhood/token/"+caAlpha || strOrNil(a.PriceUnit) != "usd" ||
		fnum(a.EntryPriceUSD) != "0.003" || // the late entry
		a.Rugged != nil || strOrNil(a.TrackingStatus) != "done" || a.Perceptor != nil ||
		a.CallCount != 1 || a.LastCallDate != a.MessageDate { // called once: the last call is the call
		t.Fatalf("alpha %+v entry %s", a, fnum(a.EntryPriceUSD))
	}
	b := by["beta"]
	if fnum(b.EntryPriceUSD) != "1.5" || b.Rugged == nil || !*b.Rugged || strOrNil(b.TokenSymbol) != "BETA" { // no late entry → the call price; on-chain symbol
		t.Fatalf("beta %+v", b)
	}
	if g := by["gamma"]; g.TokenName != nil || fnum(g.ReturnPct) != "null" || fnum(g.EntryPriceUSD) != "0.6" || strOrNil(g.TrackingStatus) != "tracking" ||
		strOrNil(g.GMGNURL) != "https://gmgn.ai/robinhood/token/"+caGamma {
		t.Fatalf("gamma (empty name → null, no 1d yet) %+v", g)
	}
	// Not priced in USD: no performance number at all, in any horizon.
	for _, h := range []string{"1h", "1d"} {
		v := fx.calls(t, "q=Virtual&horizon="+h).Calls[0]
		if v.EntryPriceUSD != nil || v.ReturnPct != nil || v.PeakPct != nil || v.DrawdownPct != nil || strOrNil(v.PriceUnit) != "VIRT" {
			t.Fatalf("non-USD call at %s: entry %s return %s peak %s drawdown %s", h, fnum(v.EntryPriceUSD), fnum(v.ReturnPct), fnum(v.PeakPct), fnum(v.DrawdownPct))
		}
	}
	if p := by["pct"]; p.PriceUnit != nil || p.EntryPriceUSD != nil || p.ReturnPct != nil {
		t.Fatalf("call without a price unit %+v", p)
	}
	if s := by["sol"]; s.GMGNURL != nil || s.TokenName != nil || s.TokenSymbol != nil || strOrNil(s.TrackingStatus) != "no_pool" ||
		strOrNil(s.PostURL) != "https://t.me/scoutrobinhood/6" {
		t.Fatalf("non-EVM call %+v", s)
	}

	// A hostile token name is data: it comes back as the same text, JSON-escaped.
	_, _, raw := fx.get(t, "/api/calls?q="+url.QueryEscape("<img"))
	if strings.Contains(string(raw), "<img") || strings.Contains(string(raw), "<b>") {
		t.Fatalf("raw markup in the response: %s", raw)
	}
	x := by["xss"]
	if strOrNil(x.TokenName) != xssName || strOrNil(x.TokenSymbol) != "<b>X</b>" || fnum(x.ReturnPct) != "null" {
		t.Fatalf("xss call %+v", x)
	}
}

func TestWebCallsSearchAndPaging(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	q := func(s string) string { return "q=" + url.QueryEscape(s) }
	fx.wantOrder(t, q("alpha"), "alpha")              // name, case-insensitive (also the symbol)
	fx.wantOrder(t, q("TA C"), "beta")                // "Beta Coin"
	fx.wantOrder(t, q("gam"), "gamma")                // symbol from the post
	fx.wantOrder(t, q("bet"), "beta")                 // on-chain symbol / name
	fx.wantOrder(t, q("0x3333"), "gamma")             // address
	fx.wantOrder(t, q("33333AB"), "gamma")            // address, other case
	fx.wantOrder(t, q(strings.ToUpper(caXSS)), "xss") // whole address
	fx.wantOrder(t, q("  alpha  "), "alpha")          // trimmed
	fx.wantOrder(t, q("so1111"), "sol")
	// % and _ are ordinary characters, not wildcards.
	fx.wantOrder(t, q("%"), "pct")
	fx.wantOrder(t, q("_"), "pct")
	fx.wantOrder(t, q("0%_r"), "pct")
	fx.wantOrder(t, q("A%a"))   // would match "Alpha" as a pattern
	fx.wantOrder(t, q("Alph_")) // would match "Alpha" as a pattern
	fx.wantOrder(t, q(`\`))
	fx.wantOrder(t, q("!"))
	fx.wantOrder(t, q("'; DROP TABLE scout_calls; --"))
	if r := fx.calls(t, q("nothing matches this")); r.Total != 0 || len(r.Calls) != 0 {
		t.Fatalf("no match: %+v", r)
	}
	// the search combines with the USD default of sort=return
	fx.wantOrder(t, "sort=return&"+q("a"), "alpha", "beta", "gamma")
	if r := fx.calls(t, q(strings.Repeat("x", 100))); r.Total != 0 {
		t.Fatalf("100-character search: %+v", r)
	}

	// Paging.
	if r := fx.wantOrder(t, "per=2", "gave", "err"); r.Total != 9 || r.Page != 1 || r.Per != 2 {
		t.Fatalf("page 1: %+v", r)
	}
	if r := fx.wantOrder(t, "per=2&page=2", "pct", "sol"); r.Total != 9 || r.Page != 2 {
		t.Fatalf("page 2: %+v", r)
	}
	fx.wantOrder(t, "per=2&page=5", "alpha")
	if r := fx.wantOrder(t, "per=2&page=6"); r.Total != 9 || r.Page != 6 {
		t.Fatalf("past the end: %+v", r)
	}
	fx.wantOrder(t, "per=4&page=2&dir=asc", "xss", "sol", "pct", "err")
	if r := fx.wantOrder(t, "per=1&page=2&sort=return", "beta"); r.Total != 3 {
		t.Fatalf("paged sort: %+v", r)
	}
	if r := fx.calls(t, "per=200"); len(r.Calls) != 9 || r.Per != 200 {
		t.Fatalf("per=200: %d", len(r.Calls))
	}
	if r := fx.calls(t, "page=1000000"); len(r.Calls) != 0 || r.Total != 9 {
		t.Fatalf("huge page: %+v", r)
	}
}

// caRep is called four times in the de-duplication tests: twice with the same
// message_date, once with the address in other letters, and once much later.
const caRep = "0xAbCdEf00000000000000000000000000000000aB"

// seedWebRepeats adds repeat calls to the fixture: caRep ×4 and a second call of
// alpha. The website must list only "rep" (the first call of caRep) for them.
func seedWebRepeats(t *testing.T, fx *webFixture) {
	t.Helper()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first := 4*time.Hour + 30*time.Minute // between virt (4h) and xss (5h)
	for _, w := range []struct {
		key string
		webSeed
	}{
		// stored first, so it has the lowest id of the token, but it is the latest call
		{"rep_last", webSeed{Msg: 30, At: 50 * time.Hour, CA: caRep, Name: sp("Renamed Later"), PostSym: "LATE", Status: TrackNoPool}},
		// the first call: same message_date as the next row, lower id
		{"rep", webSeed{Msg: 31, At: first, CA: caRep, Name: sp("Repeat Token"), PostSym: "REP", Status: TrackDone, Unit: "usd", Entry: 2,
			Returns: map[string][3]float64{"1h": {3, 4, -1}, "1d": {30, 60, -5}}}},
		{"rep_same_minute", webSeed{Msg: 32, At: first, CA: caRep, Name: sp("Repeat Token"), PostSym: "REP", Status: TrackDone, Unit: "usd", Entry: 2,
			Returns: map[string][3]float64{"1h": {9999, 9999, -1}, "1d": {9999, 9999, -1}}}},
		// same token, the address written in other letters
		{"rep_other_case", webSeed{Msg: 33, At: 6*time.Hour + 30*time.Minute, CA: "0x" + strings.ToUpper(caRep[2:]), Name: sp("Repeat Token"), Status: TrackError}},
		// a later call of alpha, with numbers that would win every sort
		{"alpha_again", webSeed{Msg: 34, At: 40 * time.Hour, CA: caAlpha, Name: sp("Alpha Token"), PostSym: "ALPHA", Status: TrackGaveUp, Unit: "usd", Entry: 9,
			Returns: map[string][3]float64{"1h": {5000, 6000, -1}, "1d": {5000, 6000, -1}}}},
	} {
		fx.ids[w.key] = seedWebCall(t, fx.st, base, w.webSeed)
	}
	if fx.ids["rep_last"] > fx.ids["rep"] || fx.ids["rep"] > fx.ids["rep_same_minute"] {
		t.Fatalf("seed ids out of order: %v", fx.ids)
	}
}

func TestWebOneRowPerToken(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	seedWebRepeats(t, fx)
	ctx := context.Background()
	var rows, sameDate int
	if err := fx.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls`).Scan(&rows); err != nil || rows != 14 {
		t.Fatalf("scout_calls rows %d (%v), want 14", rows, err)
	}
	if err := fx.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls a JOIN scout_calls b ON b.message_date = a.message_date AND b.id <> a.id
		WHERE a.id = $1`, fx.ids["rep"]).Scan(&sameDate); err != nil || sameDate != 1 {
		t.Fatalf("calls with the first call's message_date: %d (%v), want 1", sameDate, err)
	}

	// 10 tokens, newest first call first; the repeats are not listed.
	all := fx.wantOrder(t, "", "gave", "err", "pct", "sol", "xss", "rep", "virt", "gamma", "beta", "alpha")
	if all.Total != 10 {
		t.Fatalf("total %d, want 10", all.Total)
	}
	fx.wantOrder(t, "dir=asc", "alpha", "beta", "gamma", "virt", "rep", "xss", "sol", "pct", "err", "gave")
	seen := map[string]int{}
	by := map[string]webCallJSON{}
	for i, k := range fx.keys(all.Calls) {
		by[k] = all.Calls[i]
		seen[strings.ToLower(all.Calls[i].ContractAddress)]++
	}
	if len(seen) != 10 || seen[strings.ToLower(caRep)] != 1 || seen[caAlpha] != 1 {
		t.Fatalf("tokens listed: %v", seen)
	}

	// The row is the earliest call (the lower id of the two posted at the same time), with its own data.
	r := by["rep"]
	if r.CallID != fx.ids["rep"] || r.MessageID != 31 || r.MessageDate != "2026-09-01T04:30:00Z" || r.ContractAddress != caRep ||
		strOrNil(r.PostURL) != "https://t.me/scoutrobinhood/31" || strOrNil(r.TokenName) != "Repeat Token" || strOrNil(r.TokenSymbol) != "REP" ||
		strOrNil(r.TrackingStatus) != "done" || fnum(r.ReturnPct) != "30" || fnum(r.PeakPct) != "60" || fnum(r.DrawdownPct) != "-5" {
		t.Fatalf("rep %+v (return %s peak %s)", r, fnum(r.ReturnPct), fnum(r.PeakPct))
	}
	if r.CallCount != 4 || r.LastCallDate != "2026-09-03T02:00:00Z" {
		t.Fatalf("rep: call_count %d last_call_date %s, want 4 and 2026-09-03T02:00:00Z", r.CallCount, r.LastCallDate)
	}
	if a := by["alpha"]; a.CallID != fx.ids["alpha"] || a.CallCount != 2 || a.MessageDate != "2026-09-01T01:00:00Z" ||
		a.LastCallDate != "2026-09-02T16:00:00Z" || strOrNil(a.TrackingStatus) != "done" || fnum(a.ReturnPct) != "50" {
		t.Fatalf("alpha %+v", a)
	}
	for k, c := range by {
		if k != "rep" && k != "alpha" && (c.CallCount != 1 || c.LastCallDate != c.MessageDate) {
			t.Fatalf("%s: call_count %d, last_call_date %s, message_date %s", k, c.CallCount, c.LastCallDate, c.MessageDate)
		}
	}

	// Sort and horizon use the first call's numbers: the repeats' 5000 % / 9999 % never show.
	if res := fx.wantOrder(t, "sort=return", "alpha", "rep", "beta", "gamma"); res.Total != 4 || !res.USDOnly {
		t.Fatalf("sort=return %+v", res)
	}
	fx.wantOrder(t, "sort=return&dir=asc", "beta", "rep", "alpha", "gamma")
	fx.wantOrder(t, "sort=peak", "alpha", "rep", "beta", "gamma")
	fx.wantOrder(t, "sort=return&horizon=1h", "beta", "alpha", "rep", "gamma")
	fx.wantOrder(t, "sort=peak&horizon=1h&dir=asc", "gamma", "rep", "alpha", "beta")
	if res := fx.wantOrder(t, "sort=return&usd_only=0", "alpha", "rep", "beta", "gave", "err", "pct", "sol", "xss", "virt", "gamma"); res.Total != 10 {
		t.Fatalf("sort=return&usd_only=0 %+v", res)
	}
	if res := fx.wantOrder(t, "usd_only=1", "rep", "gamma", "beta", "alpha"); res.Total != 4 {
		t.Fatalf("usd_only=1 %+v", res)
	}

	// Search looks at the first calls only.
	q := func(s string) string { return "q=" + url.QueryEscape(s) }
	for _, s := range []string{"Repeat", "rep", "0xabcdef", strings.ToLower(caRep), strings.ToUpper(caRep), caRep[10:]} {
		if res := fx.wantOrder(t, q(s), "rep"); res.Total != 1 || res.Calls[0].CallCount != 4 {
			t.Fatalf("search %q: %+v", s, res)
		}
	}
	if res := fx.wantOrder(t, q("Renamed")); res.Total != 0 { // the name of a later call
		t.Fatalf("search by a repeat call's name: %+v", res)
	}
	if res := fx.wantOrder(t, q("LATE")); res.Total != 0 { // the symbol of a later call
		t.Fatalf("search by a repeat call's symbol: %+v", res)
	}
	if res := fx.wantOrder(t, q("alpha"), "alpha"); res.Total != 1 || res.Calls[0].CallCount != 2 {
		t.Fatalf("search alpha: %+v", res)
	}
	fx.wantOrder(t, "sort=return&"+q("a"), "alpha", "rep", "beta", "gamma")

	// Paging walks the de-duplicated list: no token twice, nothing missing.
	var paged []string
	for page, want := range [][]string{{"gave", "err", "pct"}, {"sol", "xss", "rep"}, {"virt", "gamma", "beta"}, {"alpha"}, {}} {
		res := fx.wantOrder(t, fmt.Sprintf("per=3&page=%d", page+1), want...)
		if res.Total != 10 || res.Page != page+1 || res.Per != 3 {
			t.Fatalf("page %d: %+v", page+1, res)
		}
		paged = append(paged, fx.keys(res.Calls)...)
	}
	if !reflect.DeepEqual(paged, fx.keys(all.Calls)) {
		t.Fatalf("paged %v, whole list %v", paged, fx.keys(all.Calls))
	}
	if res := fx.wantOrder(t, "per=1&page=2&sort=return", "rep"); res.Total != 4 {
		t.Fatalf("paged sort: %+v", res)
	}

	// Summary: first calls only; the state is the first call's (rep: done, not the
	// repeats' no_pool / error; alpha: done, not gave_up).
	_, _, body := fx.get(t, "/api/summary")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	delete(got, "updated_at")
	want := map[string]any{"imported": 10.0, "tracked": 6.0, "pending": 1.0, "tracking": 1.0, "done": 5.0,
		"no_pool": 1.0, "error": 1.0, "gave_up": 1.0, "no_usd_price": 2.0, "total_calls": 14.0, "repeat_calls": 4.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary %v\nwant    %v", got, want)
	}

	// Display rule only: every call is still in the table and in the dataset view.
	var viewRows int
	if err := fx.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_dataset_v`).Scan(&viewRows); err != nil || viewRows != 14 {
		t.Fatalf("dataset view rows %d (%v), want 14", viewRows, err)
	}
	if err := fx.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls`).Scan(&rows); err != nil || rows != 14 {
		t.Fatalf("scout_calls rows %d (%v), want 14", rows, err)
	}
	// Running the schema again (the new index) is harmless.
	if err := fx.st.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestWebBadRequestsAndMethods(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	for _, bad := range []string{
		"sort=price", "sort=", "sort=DATE", "sort=return%20desc", "sort=date&sort=return",
		"dir=up", "dir=", "dir=DESC", "dir=asc;--",
		"horizon=2d", "horizon=", "horizon=1D", "horizon=1d%27", "horizon=30d%20OR%201=1",
		"usd_only=yes", "usd_only=true", "usd_only=", "usd_only=2",
		"page=0", "page=-1", "page=abc", "page=1.5", "page=", "page=%2B1", "page=01", "page=1000001", "page=99999999999999999999",
		"per=0", "per=201", "per=x", "per=", "per=-5",
		"q=" + strings.Repeat("x", 101), "q=a&q=b", "q=%00", "q=%ff",
		"foo=1", "limit=10", "q=a;sort=date", "%zz",
	} {
		code, hdr, body := fx.get(t, "/api/calls?"+bad)
		var e map[string]string
		if code != 400 || json.Unmarshal(body, &e) != nil || e["error"] == "" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
			t.Errorf("?%s: %d %s", bad, code, body)
		}
	}
	// 100 characters of a multi-byte script is still allowed.
	if code, _, body := fx.get(t, "/api/calls?q="+url.QueryEscape(strings.Repeat("é", 100))); code != 200 {
		t.Errorf("100 non-ASCII characters: %d %s", code, body)
	}
	if code, _, body := fx.get(t, "/api/nope"); code != 404 || !strings.Contains(string(body), `"error"`) {
		t.Errorf("unknown endpoint: %d %s", code, body)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		for _, p := range []string{"/api/calls", "/api/summary", "/", "/app.js"} {
			req, _ := http.NewRequest(m, fx.srv.URL+p, strings.NewReader("x=1"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
				t.Errorf("%s %s: %d (Allow %q)", m, p, resp.StatusCode, resp.Header.Get("Allow"))
			}
		}
	}
	// Nothing was written or dropped.
	if r := fx.calls(t, ""); r.Total != 9 {
		t.Fatalf("calls left: %d", r.Total)
	}
}

func TestWebGMGNTemplate(t *testing.T) {
	t.Setenv("SCOUT_GMGN_URL", "https://example.test/t/{ca}?chain=rh")
	t.Setenv("SCOUT_WEB_ADDR", "127.0.0.1:9999")
	t.Setenv("SCOUT_WEB_DIR", "")
	cfg := loadWebConfig()
	if cfg.Addr != "127.0.0.1:9999" || cfg.Dir != "" {
		t.Fatalf("config %+v", cfg)
	}
	fx := newWebFixture(t, cfg)
	for _, c := range fx.calls(t, "").Calls {
		want := "https://example.test/t/" + c.ContractAddress + "?chain=rh"
		if c.ContractAddress == caSol {
			want = "<nil>"
		}
		if strOrNil(c.GMGNURL) != want {
			t.Fatalf("%s: gmgn_url %q, want %q", c.ContractAddress, strOrNil(c.GMGNURL), want)
		}
	}
	// Only a plain 0x + 40 hex address is ever put into the link.
	for _, ca := range []string{"", "0x123", caAlpha + "0", caAlpha + "/../x", "0x" + strings.Repeat("g", 40), " " + caAlpha,
		caAlpha + "\n", "javascript:alert(1)", caSol} {
		if u := cfg.gmgnURL(ca); u != nil {
			t.Errorf("gmgnURL(%q) = %q", ca, *u)
		}
	}
	t.Setenv("SCOUT_GMGN_URL", "")
	t.Setenv("SCOUT_WEB_ADDR", "")
	if d := loadWebConfig(); d.Addr != ":8090" || *d.gmgnURL(caGamma) != "https://gmgn.ai/robinhood/token/"+caGamma {
		t.Fatalf("defaults %+v", d)
	}
	if postURL("bad name", 5) != nil || postURL("scoutrobinhood", 0) != nil || *postURL("@scoutrobinhood", 7) != "https://t.me/scoutrobinhood/7" {
		t.Fatal("postURL")
	}
}

func checkSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	csp := h.Get("Content-Security-Policy")
	if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(csp, "default-src 'self'") ||
		strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("%s: security headers %v", what, h)
	}
}

func TestWebStaticFiles(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	for p, want := range map[string][2]string{
		"/":            {"text/html; charset=utf-8", "<title>Scout calls</title>"},
		"/index.html":  {"text/html; charset=utf-8", "Import progress"},
		"/app.js":      {"text/javascript; charset=utf-8", "textContent"},
		"/style.css":   {"text/css; charset=utf-8", "prefers-color-scheme"},
		"/favicon.svg": {"image/svg+xml", "<svg"},
	} {
		code, hdr, body := fx.get(t, p)
		if code != 200 || hdr.Get("Content-Type") != want[0] || !strings.Contains(string(body), want[1]) {
			t.Fatalf("%s: %d %q", p, code, hdr.Get("Content-Type"))
		}
		if hdr.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: Cache-Control %q", p, hdr.Get("Cache-Control"))
		}
		checkSecurityHeaders(t, p, hdr)
	}
	for _, p := range []string{"/api/summary", "/api/calls", "/api/calls?sort=x", "/missing.js"} {
		_, hdr, _ := fx.get(t, p)
		checkSecurityHeaders(t, p, hdr)
	}
	for _, p := range []string{"/missing.js", "/web.go", "/frontend/app.js", "/.env", "/app.js/", "/sub/", "/index.html.bak"} {
		if code, _, _ := fx.get(t, p); code != 404 {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}

	// The page itself: no inline script or style, nothing from another site, no innerHTML.
	_, _, html := fx.get(t, "/")
	_, _, js := fx.get(t, "/app.js")
	_, _, css := fx.get(t, "/style.css")
	page := strings.ToLower(string(html))
	for _, banned := range []string{"<script>", "<style", " style=", "onclick=", "onload=", "http://", "https://", "//cdn"} {
		if strings.Contains(page, banned) {
			t.Errorf("index.html contains %q", banned)
		}
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), banned) {
			t.Errorf("app.js contains %q", banned)
		}
	}
	for _, banned := range []string{"@import", "url(", "http://", "https://"} {
		if strings.Contains(string(css), banned) {
			t.Errorf("style.css contains %q", banned)
		}
	}
	if !strings.Contains(string(js), "noopener noreferrer") || !strings.Contains(string(js), "'https://'") {
		t.Error("app.js: links must be https-only and noopener noreferrer")
	}

	// SCOUT_WEB_DIR: files come from that folder; hidden and unknown file types do not.
	dir := t.TempDir()
	for name, content := range map[string]string{"index.html": "<p>edited copy</p>", "app.js": "// edited", ".secret.txt": "no", "notes.md": "no", ".env": "no"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "sub", "x.css"), []byte("b{}"), 0o600)
	static, _, err := webConfig{Dir: dir}.staticFS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newWebHandler(fx.st, webConfig{Dir: dir, GMGNTemplate: defaultGMGNTemplate}, static))
	defer srv.Close()
	fetch := func(p string) (int, http.Header, string) {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	if code, hdr, body := fetch("/"); code != 200 || body != "<p>edited copy</p>" || hdr.Get("Cache-Control") != "no-cache" {
		t.Fatalf("SCOUT_WEB_DIR index: %d %q", code, body)
	} else {
		checkSecurityHeaders(t, "dir /", hdr)
	}
	if code, _, body := fetch("/sub/x.css"); code != 200 || body != "b{}" {
		t.Fatalf("sub/x.css: %d", code)
	}
	for _, p := range []string{"/.secret.txt", "/.env", "/notes.md", "/sub/", "/sub", "/style.css", "/..%2f..%2fetc%2fpasswd", "/%2e%2e/x.css"} {
		if code, _, _ := fetch(p); code != 404 {
			t.Errorf("SCOUT_WEB_DIR %s: %d, want 404", p, code)
		}
	}
	if _, _, err := (webConfig{Dir: filepath.Join(dir, "nope")}).staticFS(); err == nil {
		t.Error("a missing SCOUT_WEB_DIR must be an error")
	}
}

func TestWebServeShutsDownOnCancel(t *testing.T) {
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveWeb(ctx, ln, newWebHandler(nil, webConfig{}, static)) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/style.css")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveWeb: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not stop")
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("still listening after shutdown")
	}
}
