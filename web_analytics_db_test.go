package main

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// TestWebAnalyticsDB: the load query's fields for the Analytics page, on a
// real database (SCOUT_TEST_DATABASE_URL):
//   - the verdict at the call: the first call's own live scan, else the latest
//     live scan of the token made before it; a repeat call's later scan, a
//     re-scan, or a scan of no post made after the call never count (while
//     the list's verdict is the token's latest live scan); the address is
//     compared exactly (the list ignores letter case);
//   - the no_data bit of a window stored as no_data;
//   - trades in the first 24 hours: final only for calls priced from an
//     on-chain pool (state version 2+) whose 1d window is stored; then the
//     5-minute candles' events before entry + 24 h (0 without candles); null
//     otherwise; and the posted DEX, price source and quote asset.
func TestWebAnalyticsDB(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const (
		caA = "0xa000000000000000000000000000000000000001" // own scan clean (an earlier manual scan caution), repeat call red flags
		caB = "0xb000000000000000000000000000000000000002" // earlier manual scan caution, later repeat clean
		caC = "0xc000000000000000000000000000000000000003" // only a re-scan
		caD = "0xd000000000000000000000000000000000000004" // only the repeat call's scan; manual scan after the call
		caE = "0xe000000000000000000000000000000000000005" // 1d stored as no_data
		caF = "0xf000000000000000000000000000000000000006" // v2, 1d done, candles
		caG = "0x6000000000000000000000000000000000000007" // v3, 1d done, no candles
		caH = "0x7000000000000000000000000000000000000008" // v4, 1d not stored yet
		caI = "0x8000000000000000000000000000000000000009" // GeckoTerminal, 1d done
		caJ = "0x900000000000000000000000000000000000000a" // v2 but state version 1
		caK = "0xAB0000000000000000000000000000000000000b" // posted in mixed case; an earlier manual scan in lower case
	)
	ret := map[string][3]float64{"1h": {5, 10, -3}, "1d": {-20, 40, -60}}
	only1h := map[string][3]float64{"1h": {5, 10, -3}}
	seed := func(msg int, at time.Duration, ca string, returns map[string][3]float64) int {
		return seedWebCall(t, st, base, webSeed{Msg: msg, At: at, CA: ca, PostSym: "S", Status: TrackDone, Unit: "usd", Entry: 1, Late: 1, Returns: returns})
	}
	a1 := seed(1, time.Minute, caA, ret)
	a2 := seed(2, 3*time.Hour, caA, nil)
	b1 := seed(3, 2*time.Minute, caB, ret)
	b2 := seed(4, 5*time.Hour, caB, nil)
	c1 := seed(5, 3*time.Minute, caC, ret)
	d1 := seed(6, 4*time.Minute, caD, ret)
	d2 := seed(7, 6*time.Hour, caD, nil)
	e1 := seed(8, 5*time.Minute, caE, only1h)
	f1 := seed(9, 6*time.Minute, caF, ret)
	g1 := seed(10, 7*time.Minute, caG, ret)
	h1 := seed(11, 8*time.Minute, caH, only1h)
	i1 := seed(12, 9*time.Minute, caI, ret)
	j1 := seed(13, 10*time.Minute, caJ, ret)
	k1 := seed(14, 11*time.Minute, caK, ret)

	inv := func(call *int, ca string, at time.Duration, kind, level string) {
		t.Helper()
		seedInvestigation(t, st, perc, call, ca, base.Add(at), investigationCompleted, kind, level)
	}
	inv(&a1, caA, 2*time.Minute, ScanKindLive, levelClean)
	inv(nil, caA, -2*time.Hour, ScanKindLive, levelCaution) // an earlier manual scan: the call's own one wins
	inv(&a2, caA, 3*time.Hour+time.Minute, ScanKindLive, levelRedFlags)
	inv(nil, caB, -time.Hour, ScanKindLive, levelCaution)
	inv(&b2, caB, 5*time.Hour+time.Minute, ScanKindLive, levelClean)
	inv(&c1, caC, 30*24*time.Hour, ScanKindRescan, levelRedFlags)
	inv(&d2, caD, 6*time.Hour+time.Minute, ScanKindLive, levelCaution)
	inv(nil, caD, time.Hour, ScanKindLive, levelRedFlags) // a manual scan after the first call
	// the verdict at the call compares the address exactly (as the view does),
	// the list without regard to letter case
	inv(nil, strings.ToLower(caK), -time.Hour, ScanKindLive, levelCaution)
	// a failed live scan of A's first call never counts
	seedInvestigation(t, st, perc, &a1, caA, base.Add(3*time.Minute), "failed", ScanKindLive, levelRedFlags)

	// E: 1d stored as no_data
	if err := st.UpsertReturn(ctx, e1, horizon{Name: "1d", Dur: 24 * time.Hour}, horizonResult{Horizon: "1d",
		DueAt: base.Add(24 * time.Hour), Status: "no_data"}); err != nil {
		t.Fatal(err)
	}
	// price sources, quote assets, state versions, posted DEX names
	src := func(id int, source, onchain string) {
		t.Helper()
		var oc any
		if onchain != "" {
			oc = onchain
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET entry_price_source = $2, onchain = $3::jsonb WHERE call_id = $1`,
			id, source, oc); err != nil {
			t.Fatal(err)
		}
	}
	src(f1, "onchain-v2", `{"v":2,"quote_sym":"WETH"}`)
	src(g1, "onchain-v3", `{"v":2,"quote_sym":"USDG"}`)
	src(h1, "onchain-v4", `{"v":2,"quote_sym":"WETH"}`)
	src(i1, "minute", "")
	src(j1, "onchain-v2", `{"v":1,"quote_sym":"WETH"}`)
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_metrics SET dex = $2 WHERE call_id = $1`, f1, "  Uniswap V2 "); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_metrics SET dex = '   ' WHERE call_id = $1`, g1); err != nil {
		t.Fatal(err)
	}
	// F's candles: two 5-minute buckets in the first day (30 + 25 swaps), one
	// that starts at entry + 24 h (left out), and hourly ones (other interval)
	entryF := base.Add(6 * time.Minute)
	if err := st.UpsertCandles(ctx, f1, []candleRow{
		{IntervalS: candleFineS, Start: entryF.Truncate(5 * time.Minute), O: 1, H: 1, L: 1, C: 1, Events: 30},
		{IntervalS: candleFineS, Start: entryF.Add(23 * time.Hour).Truncate(5 * time.Minute), O: 1, H: 1, L: 1, C: 1, Events: 25},
		{IntervalS: candleFineS, Start: entryF.Add(24 * time.Hour), O: 1, H: 1, L: 1, C: 1, Events: 1000},
		{IntervalS: candleCoarseS, Start: entryF.Truncate(time.Hour), O: 1, H: 1, L: 1, C: 1, Events: 500},
	}); err != nil {
		t.Fatal(err)
	}
	// candles of calls whose count is not final: never asked for
	for _, id := range []int{h1, i1, j1} {
		if err := st.UpsertCandles(ctx, id, []candleRow{{IntervalS: candleFineS, Start: base, O: 1, H: 1, L: 1, C: 1, Events: 7}}); err != nil {
			t.Fatal(err)
		}
	}

	rows, _, err := st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int]*ScoutWebRow{}
	for i := range rows {
		byID[rows[i].CallID] = &rows[i]
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	for _, c := range []struct {
		name          string
		id            int
		verdictAtCall string
		listVerdict   string
	}{
		{"own scan, later repeat red flags", a1, levelClean, levelRedFlags},
		{"earlier scan of no post, later repeat clean", b1, levelCaution, levelClean},
		{"only a re-scan", c1, "<nil>", "<nil>"},
		{"only the repeat's scan, and a manual scan after the call", d1, "<nil>", levelCaution},
		{"earlier scan of the address in another letter case", k1, "<nil>", levelCaution},
	} {
		r := byID[c.id]
		if r == nil {
			t.Fatalf("%s: call %d not listed", c.name, c.id)
		}
		if got := str(r.VerdictAtCall); got != c.verdictAtCall {
			t.Errorf("%s: verdict at call %s, want %s", c.name, got, c.verdictAtCall)
		}
		if got := str(r.PerceptorVerd); got != c.listVerdict {
			t.Errorf("%s: list verdict %s, want %s (unchanged rule)", c.name, got, c.listVerdict)
		}
	}
	if _, listed := byID[a2]; listed {
		t.Fatal("a repeat call is listed")
	}
	if r := byID[e1]; r.NoData != 2 || r.HasPerf&(1<<(1*webPerfPerHorizon)) != 0 || r.HasPerf&1 == 0 {
		t.Errorf("no_data: bits %b, has %b; want no_data 10 (1d) and the 1h return", r.NoData, r.HasPerf)
	}
	if r := byID[f1]; r.NoData != 0 {
		t.Errorf("done windows: no_data %b, want 0", r.NoData)
	}
	for _, c := range []struct {
		name   string
		id     int
		final  bool
		source string
		quote  string
	}{
		{"v2, 1d done", f1, true, "onchain-v2", "WETH"},
		{"v3, 1d done, no candles", g1, true, "onchain-v3", "USDG"},
		{"v4, 1d not stored", h1, false, "onchain-v4", "WETH"},
		{"GeckoTerminal", i1, false, "minute", "<nil>"},
		{"state version 1", j1, false, "onchain-v2", "WETH"},
		{"untracked source", a1, false, "<nil>", "<nil>"},
	} {
		r := byID[c.id]
		if r.TradesFinal != c.final || str(r.EntrySource) != c.source || str(r.QuoteSym) != c.quote {
			t.Errorf("%s: final %v source %s quote %s; want %v %s %s", c.name, r.TradesFinal, str(r.EntrySource), str(r.QuoteSym),
				c.final, c.source, c.quote)
		}
	}
	if got := str(byID[f1].PostedDex); got != "Uniswap V2" {
		t.Errorf("posted dex %q, want trimmed", got)
	}
	if got := byID[g1].PostedDex; got != nil {
		t.Errorf("blank posted dex %q, want nil", *got)
	}

	counts, err := st.SelectWebTrades24h(ctx, []int{f1, g1})
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[f1] != 55 {
		t.Fatalf("trades: got %v, want {%d: 55}", counts, f1)
	}

	// through the website: the body says 55 for F, 0 for G, null for the others
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, st, webConfig{GMGNTemplate: defaultGMGNTemplate}, static)
	rec := getWeb(ws, "/api/analytics")
	if rec.Code != 200 {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	body := decodeAnalytics(t, rec.Body.Bytes())
	got := map[int][]*float64{}
	for _, row := range body.Rows {
		got[int(*row[0])] = row
	}
	want := map[int]any{f1: 55, g1: 0, h1: nil, i1: nil, j1: nil, a1: nil}
	for id, w := range want {
		row := got[id]
		if row == nil {
			t.Fatalf("call %d not in the body", id)
		}
		tr := row[7]
		switch w := w.(type) {
		case nil:
			if tr != nil {
				t.Errorf("call %d: trades %v, want null", id, *tr)
			}
		case int:
			if tr == nil || int(*tr) != w {
				t.Errorf("call %d: trades %v, want %d", id, tr, w)
			}
		}
	}
	if v := body.Verdicts[int(*got[a1][3])]; v != levelClean {
		t.Errorf("body: verdict of A %s, want clean", v)
	}
	if v := body.Verdicts[int(*got[d1][3])]; v != "none" {
		t.Errorf("body: verdict of D %s, want none", v)
	}
	if f := body.Families[int(*got[i1][4])]; f != "gecko" {
		t.Errorf("body: family of I %s, want gecko", f)
	}
	if nd := *got[e1][8]; nd != 2 {
		t.Errorf("body: no_data of E %v, want 2", nd)
	}
}
