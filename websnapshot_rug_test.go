package main

import (
	"context"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestSAlphaDeclined: the phrases of a short reply that declines to report
// are found in any letter case and with text around them; real reports are
// not, also a long one that mentions a phrase in a risk line.
func TestSAlphaDeclined(t *testing.T) {
	// a real report of more than salphaDeclineMaxLen characters with a risk line
	longReport := "Summary: meme token on Base, launched 3 h ago. " + strings.Repeat("Holders grow steadily; dev wallet holds 2%. ", 6) +
		"Risks: too little liquidity to exit large positions; not enough public information on the team."
	if n := len([]rune(longReport)); n <= salphaDeclineMaxLen {
		t.Fatalf("longReport has %d characters, want more than %d", n, salphaDeclineMaxLen)
	}
	atLimit := "Too little liquidity to research." + strings.Repeat(".", salphaDeclineMaxLen-len("Too little liquidity to research."))
	padded := strings.Repeat(" \n", 400) + "Not enough public signals to generate a report for this token." + strings.Repeat("\r\n\t", 100)
	for _, tc := range []struct {
		text string
		want bool
	}{
		{longReport, false},
		{atLimit, true},
		{atLimit + ".", false},
		{padded, true}, // white space around a decline does not count
		{"Not enough public signals to generate a report for this token.", true},
		{"  NOT ENOUGH PUBLIC SIGNALS TO GENERATE A REPORT for this token\n", true},
		{"Too little liquidity or trading activity to research yet.", true},
		{"too little liquidity or trading activity to research yet", true},
		{"There is not enough public information to write a report on this token.", true},
		{"Not enough public data yet.", true},
		{"Too little liquidity to research.", true},
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
		if tag := rec.Header().Get("ETag"); !strings.HasSuffix(tag, "-"+webCallDetailVersion+`"`) || webCallDetailVersion != "3" {
			t.Errorf("%s: ETag %s, want one ending in -3 (the detail body changed form: reask)", path, tag)
		}
		d := decodeCallDetail(t, rec.Body.Bytes())
		if d.SAlpha == nil || d.SAlpha.Declined != tc.declined || (tc.text != "" && d.SAlpha.Text != tc.text) {
			t.Errorf("%s: salpha = %+v, want declined=%v text %q", path, d.SAlpha, tc.declined, tc.text)
		}
	}
}

// TestWebRuggedRow: a rugged call whose pool was drained shows no peak in any
// window (and sorts with the calls without one), keeps its stored returns and
// worst drop, carries rugged = true, and its latest market cap is not shown at
// a price of 0. A collapsed one (rug kind price) shows its peak.
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
	r.Rugged, r.RugAtKind = &yes, sp(rugAtEvent) // drained (a collapsed call keeps its peak: below)
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
	// the same row collapsed (rugged by the price rule) shows its peak too
	collapsed := cloneWebRows(rows)
	collapsed[pos].RugAtKind = sp(rugAtPrice)
	coll := mustWebSnapshot(t, collapsed, 0, nil)
	if c := sentCall(t, coll, pos, 1); c.PeakPct == nil || *c.PeakPct != 251 || c.Rugged == nil || !*c.Rugged || strOrNil(c.RugKind) != rugAtPrice {
		t.Errorf("collapsed: peak_pct %s rugged %v rug_kind %s, want 251 true price", fnum(c.PeakPct), c.Rugged, strOrNil(c.RugKind))
	}
	if coll.version == snap.version {
		t.Error("a collapsed call must change the snapshot version")
	}
}

// runNodeTest runs a testdata/*.test.js file with node (the test is skipped
// when node is not on the PATH); it must print "ok: …".
func runNodeTest(t *testing.T, file string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("node %s: %v: %s", file, err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node %s: got %q, want \"ok: …\"", file, out)
	}
}

// TestAppRugNode runs testdata/app-rug.test.js: the list page's "rugged"
// (pool drained) and "collapsed" (rug kind price) badges in Status, and the
// peak cell (a dash for a drained call only).
func TestAppRugNode(t *testing.T) { runNodeTest(t, "testdata/app-rug.test.js") }

// TestAnalyticsRugsNode runs testdata/analytics-rugs.test.js: the Timing
// tab's rug kinds (rugOf, addTm) and its "Rugs by kind of rug time" table
// with the collapsed row (fillTmRugs).
func TestAnalyticsRugsNode(t *testing.T) { runNodeTest(t, "testdata/analytics-rugs.test.js") }

// TestRugCollapsedTexts: the pages explain the collapsed badge apart from
// the rugged one, with the threshold written out (the model's and the
// Analytics page's "collapse" is a smaller drop), and the badge has its
// colours in both colour schemes.
func TestRugCollapsedTexts(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(frontendFS, "frontend/"+name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	index, ana, css := read("index.html"), read("analytics.html"), read("style.css")
	for _, c := range []struct{ file, text, want string }{
		{"index.html", index, `<span class="badge badge-rug">rugged</span>`},
		{"index.html", index, `<span class="badge badge-collapse">collapsed</span>`},
		{"index.html", index, "Collapsed (price down more than 95%)"},
		{"index.html", index, "A rugged call (pool drained) shows no peak (–); a collapsed one shows its peak."},
		{"analytics.html", ana, "The flag covers both calls whose pool was drained and collapsed calls (price down more than 95%"},
		{"analytics.html", ana, `"Collapsed (price down more than 95%)"`},
		{"style.css", css, ".badge-collapse { color: var(--b-collapse); background: var(--b-collapse-bg);"},
	} {
		if !strings.Contains(c.text, c.want) {
			t.Errorf("%s: missing %q", c.file, c.want)
		}
	}
	if n := strings.Count(css, "--b-collapse-bg: #"); n != 2 {
		t.Errorf("style.css: --b-collapse-bg set %d times, want 2 (light and dark)", n)
	}
	if strings.Contains(index, "after the last window also when the price fell below 5% of entry") {
		t.Error("index.html: the rugged legend still describes the price rule (now the collapsed badge)")
	}
}
