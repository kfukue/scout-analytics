package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestSAlphaDeclined: the phrases of a reply that declines to report are found
// in any letter case and with text around them; real reports are not.
func TestSAlphaDeclined(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Not enough public signals to generate a report for this token.", true},
		{"  NOT ENOUGH PUBLIC SIGNALS TO GENERATE A REPORT for this token\n", true},
		{"Too little liquidity or trading activity to research yet.", true},
		{"too little liquidity or trading activity to research yet", true},
		{"Strong community traction; dev wallet sold 2%.", false},
		{"Not enough liquidity yet, but 3 smart wallets bought.", false},
		{"", false},
	} {
		if got := salphaDeclined(tc.text); got != tc.want {
			t.Errorf("salphaDeclined(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	for _, p := range salphaDeclinePhrases {
		if p != strings.ToLower(p) || strings.TrimSpace(p) != p || p == "" {
			t.Errorf("phrase %q: must be lower case, trimmed and not empty (the query compares with lower())", p)
		}
	}
}

// declineWebServer is a website over synthetic rows whose sAlpha report of
// row pos says text instead of its own.
func declineWebServer(t *testing.T, pos int, text string) (*webServer, []ScoutWebRow) {
	t.Helper()
	ws := benchWebServer(t, 1)
	rows := syntheticWebRows(60, 9)
	reports := syntheticWebReports(rows)
	if text != "" {
		reports[*rows[pos].SAlphaID].Text = text
	}
	db := &fakeWebDB{rows: rows, reports: reports}
	ws.readRows, ws.readReports = db.read, db.readReports
	ws.snap.Store(nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ws, rows
}

// TestWebSAlphaDeclinedRow: a row whose sAlpha reply only declines to report
// has no "sA" flag, still names the reply (so the detail can say sAlpha did
// not report, with its reason), and the snapshot version changes with it.
func TestWebSAlphaDeclinedRow(t *testing.T) {
	rows := syntheticWebRows(60, 9)
	pos, other := -1, -1
	for i := range rows {
		if rows[i].SAlphaID != nil {
			if pos < 0 {
				pos = i
			} else if other < 0 {
				other = i
			}
		}
	}
	if pos < 0 || other < 0 {
		t.Fatal("the synthetic rows need two tokens with an sAlpha report")
	}
	const reason = "Not enough public signals to generate a report for this token."
	real, _ := declineWebServer(t, pos, "")
	ws, _ := declineWebServer(t, pos, reason)
	snap := ws.snap.Load()
	if snap.version == real.snap.Load().version {
		t.Fatal("a reply that declines must change the snapshot version (same ids, different flag)")
	}
	id := *rows[pos].SAlphaID
	c := sentCall(t, snap, pos, 1)
	if c.HasSAlpha || c.SAlphaReportID == nil || *c.SAlphaReportID != id {
		t.Errorf("declined row: has_salpha_report=%v salpha_report_id=%v, want false and %d", c.HasSAlpha, c.SAlphaReportID, id)
	}
	if c := sentCall(t, snap, other, 1); !c.HasSAlpha {
		t.Errorf("row %d with a real report: has_salpha_report = false, want true", other)
	}

	for _, tc := range []struct {
		pos      int
		declined bool
		text     string
	}{
		{pos, true, reason},
		{other, false, ""},
	} {
		path := "/api/call?id=" + strconv.Itoa(rows[tc.pos].CallID)
		rec := getWeb(ws, path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if tag := rec.Header().Get("ETag"); !strings.HasSuffix(tag, "-"+webCallDetailVersion+`"`) || webCallDetailVersion != "2" {
			t.Errorf("%s: ETag %s, want one ending in -2 (the detail body changed form)", path, tag)
		}
		d := decodeCallDetail(t, rec.Body.Bytes())
		if d.SAlpha == nil || d.SAlpha.Declined != tc.declined || (tc.text != "" && d.SAlpha.Text != tc.text) {
			t.Errorf("%s: salpha = %+v, want declined=%v text %q", path, d.SAlpha, tc.declined, tc.text)
		}
	}
}

// TestWebRuggedRow: a rugged call shows no peak in any window (and sorts with
// the calls without one), keeps its stored returns and worst drop, carries
// rugged = true, and its latest market cap is not shown at a price of 0.
func TestWebRuggedRow(t *testing.T) {
	rows := syntheticWebRows(80, 3)
	pos := -1
	for i := range rows {
		r := &rows[i]
		if r.PriceUnit != nil && *r.PriceUnit == "usd" && r.LatestAt != nil {
			pos = i
			break
		}
	}
	if pos < 0 {
		t.Fatal("the synthetic rows need a USD row with a latest price")
	}
	r := &rows[pos]
	yes := true
	r.Rugged = &yes
	r.HasPerf = 1<<15 - 1
	for h := range ScoutWebHorizons {
		r.Perf[h*webPerfPerHorizon+webPerfReturn] = -100
		r.Perf[h*webPerfPerHorizon+webPerfPeak] = 250 + float64(h) // the high before the rug
		r.Perf[h*webPerfPerHorizon+webPerfDrawdown] = -100
	}
	zero, minus100 := 0.0, -100.0
	r.LatestPrice, r.LatestReturn = &zero, &minus100
	call, mc, price := 50000.0, 52000.0, 0.001
	r.CalledAtMcap, r.PostMcap, r.PostPrice = &call, &mc, &price

	snap := mustWebSnapshot(t, cloneWebRows(rows), 0, nil)
	for h, name := range ScoutWebHorizons {
		c := sentCall(t, snap, pos, h)
		if c.PeakPct != nil {
			t.Errorf("window %s: peak_pct = %v, want null (rugged)", name, *c.PeakPct)
		}
		if c.ReturnPct == nil || *c.ReturnPct != -100 || c.DrawdownPct == nil || *c.DrawdownPct != -100 {
			t.Errorf("window %s: return_pct %s drawdown_pct %s, want -100 -100 (as stored)", name, fnum(c.ReturnPct), fnum(c.DrawdownPct))
		}
		if c.Rugged == nil || !*c.Rugged {
			t.Errorf("window %s: rugged = %v, want true", name, c.Rugged)
		}
	}
	c := sentCall(t, snap, pos, 1)
	if c.LatestPriceUSD == nil || *c.LatestPriceUSD != 0 || c.LatestMcapUSD != nil || c.CallMcapUSD == nil {
		t.Errorf("latest price %s latest_mcap_usd %s call_mcap_usd %s, want 0, null and 50000", fnum(c.LatestPriceUSD), fnum(c.LatestMcapUSD), fnum(c.CallMcapUSD))
	}
	// sorted by peak, the row is among the rows without a value, in both directions
	for _, dir := range []string{"desc", "asc"} {
		ids, total := pageIDs(t, snap, ScoutWebCallsFilter{Sort: "peak", Dir: dir, Horizon: "1d", Page: 1, Per: 200})
		at := -1
		for i, id := range ids {
			if id == r.CallID {
				at = i
			}
		}
		if total != len(rows) || at < snap.nonNull[webSortKeyPeak+1] {
			t.Errorf("sort peak %s: rugged row at %d of %d, want after the %d rows with a peak", dir, at, total, snap.nonNull[webSortKeyPeak+1])
		}
	}
	// the same row not rugged shows its peak, and the version differs
	no := false
	plain := cloneWebRows(rows)
	plain[pos].Rugged = &no
	other := mustWebSnapshot(t, plain, 0, nil)
	if c := sentCall(t, other, pos, 1); c.PeakPct == nil || *c.PeakPct != 251 {
		t.Errorf("not rugged: peak_pct %s, want 251", fnum(c.PeakPct))
	}
	if other.version == snap.version {
		t.Error("rugged must change the snapshot version")
	}
}
