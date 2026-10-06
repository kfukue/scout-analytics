package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWebDetailSelection: which Perceptor and sAlpha report a token's row
// detail shows (the latest completed one of each tool, by contract address in
// any letter case, whichever post it was made for; for sAlpha the latest one
// with text), what the row says about it, and that a refresh reads the texts
// of new reports only.
func TestWebDetailSelection(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	st := fx.st
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	salpha := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	// count the reads of the texts and the ids they asked for
	var mu sync.Mutex
	var asked [][]int
	read := fx.web.readReports
	fx.web.readReports = func(ctx context.Context, ids []int) (map[int]*ScoutWebReport, error) {
		mu.Lock()
		asked = append(asked, slices.Clone(ids))
		mu.Unlock()
		return read(ctx, ids)
	}
	takeAsked := func() [][]int {
		mu.Lock()
		defer mu.Unlock()
		a := asked
		asked = nil
		return a
	}

	type invSeed struct {
		tool      int
		call      string // "" = a scan of no post
		ca        string
		at        time.Duration
		status    string
		text      string
		summary   *string
		url       *string
		completed time.Duration // 0 = completed_at NULL
	}
	inv := func(s invSeed) int {
		t.Helper()
		var callID *int
		if s.call != "" {
			id := fx.ids[s.call]
			callID = &id
		}
		r := &ScoutInvestigation{CallID: callID, ToolID: s.tool, ContractAddress: s.ca, RequestText: s.ca,
			RequestedAt: base.Add(s.at), Status: s.status, ReportText: s.text, VerdictLevel: levelClean,
			VerdictLabel: sp("No red flags found"), VerdictSummary: s.summary, ReportURL: s.url}
		if s.tool == salpha {
			r.VerdictLevel, r.VerdictLabel = levelUnknown, nil
		}
		if s.completed != 0 {
			c := base.Add(s.completed)
			r.CompletedAt = &c
		}
		id, err := st.InsertScoutInvestigation(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return *id
	}
	done := investigationCompleted
	fx.ids["beta_again"] = seedWebCall(t, st, base, webSeed{Msg: 21, At: 30 * time.Hour, CA: caBeta, Name: sp("Beta Coin"), Status: TrackPending})

	// beta: sAlpha with text on the first post, a later one with text on the
	// later post (stored first, so a lower id), then newer ones that do not count:
	// empty, white space only, failed, timed out
	betaNew := inv(invSeed{tool: salpha, call: "beta_again", ca: strings.ToLower(caBeta), at: 30 * time.Hour, status: done,
		text: "Dev wallet sold 40%\n<script>alert(1)</script>", url: sp("https://salpha.example/r/beta"), completed: 31 * time.Hour})
	inv(invSeed{tool: salpha, call: "beta", ca: caBeta, at: 2 * time.Hour, status: done, text: "old beta text"})
	inv(invSeed{tool: salpha, call: "beta_again", ca: caBeta, at: 40 * time.Hour, status: done, text: ""})
	inv(invSeed{tool: salpha, call: "beta_again", ca: caBeta, at: 41 * time.Hour, status: done, text: " \n\t \r\n"})
	inv(invSeed{tool: salpha, call: "beta_again", ca: caBeta, at: 50 * time.Hour, status: "failed", text: "failed text"})
	inv(invSeed{tool: salpha, call: "beta_again", ca: caBeta, at: 51 * time.Hour, status: "timeout", text: "timeout text"})
	// alpha: the latest sAlpha reply is empty, an older one has text; Perceptor clean
	alphaOld := inv(invSeed{tool: salpha, call: "alpha", ca: caAlpha, at: 5 * time.Hour, status: done, text: "alpha: 3 smart wallets"})
	inv(invSeed{tool: salpha, call: "alpha", ca: caAlpha, at: 10 * time.Hour, status: done, text: ""})
	alphaPerc := inv(invSeed{tool: perc, call: "alpha", ca: caAlpha, at: 1 * time.Hour, status: done,
		summary: sp("Top 10 hold 40%; LP locked"), url: sp("https://www.perceptor.info/r/alpha")})
	// gamma: Perceptor only (stored with the address in lower case), and a
	// failed Perceptor scan later
	gammaPerc := inv(invSeed{tool: perc, call: "gamma", ca: strings.ToLower(caGamma), at: 3 * time.Hour, status: done,
		summary: sp("Liquidity PULLED"), url: sp("https://www.perceptor.info/r/gamma"), completed: 3*time.Hour + 30*time.Second})
	inv(invSeed{tool: perc, call: "gamma", ca: caGamma, at: 9 * time.Hour, status: "failed", summary: sp("failed")})
	// sol: links that are not https are not shown
	solPerc := inv(invSeed{tool: perc, call: "sol", ca: caSol, at: 6 * time.Hour, status: done, url: sp("http://www.perceptor.info/r/plain")})
	solSA := inv(invSeed{tool: salpha, call: "sol", ca: caSol, at: 6 * time.Hour, status: done, text: "sol text", url: sp("javascript:alert(1)")})
	// virt: only an empty sAlpha reply, and a rate-limited one with text
	inv(invSeed{tool: salpha, call: "virt", ca: caVirt, at: 4 * time.Hour, status: done, text: "   "})
	inv(invSeed{tool: salpha, call: "virt", ca: caVirt, at: 5 * time.Hour, status: "rate_limited", text: "limited"})

	res := fx.calls(t, "per=200")
	byKey := map[string]webCallJSON{}
	for i, k := range fx.keys(res.Calls) {
		byKey[k] = res.Calls[i]
	}
	idOf := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	for k, want := range map[string][3]int{ // has sAlpha (0/1), Perceptor id, sAlpha id
		"alpha": {1, alphaPerc, alphaOld}, "beta": {1, 0, betaNew}, "gamma": {0, gammaPerc, 0},
		"sol": {1, solPerc, solSA}, "virt": {0, 0, 0}, "gave": {0, 0, 0},
	} {
		c, ok := byKey[k]
		if !ok {
			t.Fatalf("no row %s", k)
		}
		has := 0
		if c.HasSAlpha {
			has = 1
		}
		if got := [3]int{has, idOf(c.PerceptorReportID), idOf(c.SAlphaReportID)}; got != want {
			t.Errorf("%s: has_salpha_report, perceptor_report_id, salpha_report_id = %v, want %v", k, got, want)
		}
	}

	detail := func(key string) webCallDetailJSON {
		t.Helper()
		code, _, body := fx.raw(t, fmt.Sprintf("/api/call?id=%d", fx.ids[key]))
		if code != 200 {
			t.Fatalf("%s: %d %s", key, code, body)
		}
		return decodeCallDetail(t, body)
	}
	d := detail("beta")
	if d.Perceptor != nil || d.SAlpha == nil || d.SAlpha.ID != betaNew || d.SAlpha.Text != "Dev wallet sold 40%\n<script>alert(1)</script>" ||
		d.SAlpha.URL == nil || *d.SAlpha.URL != "https://salpha.example/r/beta" || d.SAlpha.At != "2026-09-02T07:00:00Z" || d.SAlpha.Truncated {
		t.Errorf("beta: %+v %+v", d.Perceptor, d.SAlpha)
	}
	d = detail("alpha")
	if d.SAlpha == nil || d.SAlpha.Text != "alpha: 3 smart wallets" || d.SAlpha.URL != nil || d.SAlpha.At != "2026-09-01T05:00:00Z" ||
		d.Perceptor == nil || d.Perceptor.Verdict != levelClean || d.Perceptor.Label == nil || *d.Perceptor.Label != "No red flags found" ||
		d.Perceptor.Summary == nil || *d.Perceptor.Summary != "Top 10 hold 40%; LP locked" ||
		d.Perceptor.URL == nil || *d.Perceptor.URL != "https://www.perceptor.info/r/alpha" {
		t.Errorf("alpha: %+v %+v", d.Perceptor, d.SAlpha)
	}
	d = detail("gamma")
	if d.SAlpha != nil || d.Perceptor == nil || d.Perceptor.ID != gammaPerc || *d.Perceptor.Summary != "Liquidity PULLED" || d.Perceptor.At != "2026-09-01T03:00:30Z" {
		t.Errorf("gamma: %+v %+v", d.Perceptor, d.SAlpha)
	}
	d = detail("sol")
	if d.Perceptor == nil || d.Perceptor.URL != nil || d.SAlpha == nil || d.SAlpha.URL != nil || d.SAlpha.Text != "sol text" {
		t.Errorf("sol: %+v %+v", d.Perceptor, d.SAlpha)
	}
	if d := detail("virt"); d.Perceptor != nil || d.SAlpha != nil {
		t.Errorf("virt: %+v %+v", d.Perceptor, d.SAlpha)
	}
	// a repeat call is not a row
	if code, _, _ := fx.raw(t, fmt.Sprintf("/api/call?id=%d", fx.ids["beta_again"])); code != 404 {
		t.Errorf("repeat call: %d", code)
	}
	// the reads of the texts so far: only ids the website did not hold
	seen := map[int]bool{}
	for _, ids := range takeAsked() {
		for _, id := range ids {
			if seen[id] {
				t.Errorf("text %d read twice", id)
			}
			seen[id] = true
		}
	}
	for _, id := range []int{betaNew, alphaOld, alphaPerc, gammaPerc, solPerc, solSA} {
		if !seen[id] {
			t.Errorf("text %d never read", id)
		}
	}

	// nothing new: no read of the texts, even when a stored text changes
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_investigations SET report_text = 'edited' WHERE id = $1`, alphaOld); err != nil {
		t.Fatal(err)
	}
	fx.refresh(t)
	fx.refresh(t)
	if a := takeAsked(); len(a) != 0 {
		t.Fatalf("texts read without a new report: %v", a)
	}
	if d := detail("alpha"); d.SAlpha == nil || d.SAlpha.Text != "alpha: 3 smart wallets" {
		t.Fatalf("a text was read again: %+v", d.SAlpha)
	}
	// a new report with text for virt: one read, of that id only
	_, hdr, _ := fx.raw(t, "/api/calls?per=200")
	tag := hdr.Get("ETag")
	// (a scan of no post)
	virtNew := inv(invSeed{tool: salpha, ca: caVirt, at: 7 * time.Hour, status: done, text: "virt now has text"})
	fx.refresh(t)
	if a := takeAsked(); len(a) != 1 || !slices.Equal(a[0], []int{virtNew}) {
		t.Fatalf("texts read: %v, want only [%d]", a, virtNew)
	}
	if d := detail("virt"); d.SAlpha == nil || d.SAlpha.Text != "virt now has text" {
		t.Fatalf("virt after its report: %+v", d.SAlpha)
	}
	if _, hdr, _ := fx.raw(t, "/api/calls?per=200"); hdr.Get("ETag") == tag {
		t.Fatal("a new sAlpha report must change the ETag of /api/calls")
	}
	// another empty reply for virt: nothing changes, nothing is read
	_, hdr, _ = fx.raw(t, "/api/calls?per=200")
	tag = hdr.Get("ETag")
	inv(invSeed{tool: salpha, call: "virt", ca: caVirt, at: 8 * time.Hour, status: done, text: "\n"})
	fx.refresh(t)
	if _, hdr, _ := fx.raw(t, "/api/calls?per=200"); hdr.Get("ETag") != tag {
		t.Fatal("an empty sAlpha reply changed the ETag of /api/calls")
	}
	if a := takeAsked(); len(a) != 0 {
		t.Fatalf("an empty reply caused a read: %v", a)
	}
}
