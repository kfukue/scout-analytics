package main

import (
	"context"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The website's "Call MC" and "Latest MC": the values (the called-at figure
// first for the call; the Mcap line first, times the latest price, divided by
// the price at the post, for the estimate), sort=call_mc and sort=latest_mc in
// both directions with the rows without a value last, usd_only on by default,
// and an ETag that follows the market caps shown and nothing else.
func TestWebMarketCaps(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := fx.st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// setMC stores the two market caps of a post (nil = NULL; a string is put in as a numeric literal, e.g. "NaN")
	setMC := func(key string, calledAt, mcap any) {
		t.Helper()
		exec(`INSERT INTO scout_call_metrics (call_id, called_at_mcap_usd, mcap_usd, created_by, updated_by) VALUES ($1, $2::numeric, $3::numeric, 't', 't')
			ON CONFLICT (call_id) DO UPDATE SET called_at_mcap_usd = EXCLUDED.called_at_mcap_usd, mcap_usd = EXCLUDED.mcap_usd`, fx.ids[key], calledAt, mcap)
	}
	setLatest := func(key string, price float64) {
		t.Helper()
		at := base.Add(30 * 24 * time.Hour)
		exec(`UPDATE scout_call_tracking SET latest_return_pct = 1, latest_price_usd = $2, latest_checked_at = $3, latest_trade_at = $3
			WHERE call_id = $1`, fx.ids[key], price, at)
	}
	etagOf := func(path string) string {
		t.Helper()
		code, hdr, _ := fx.get(t, path)
		if code != 200 || hdr.Get("ETag") == "" {
			t.Fatalf("%s: %d ETag %q", path, code, hdr.Get("ETag"))
		}
		return hdr.Get("ETag")
	}

	// before any market cap: the two fields are there, after the latest price and before the
	// report fields, and null
	if code, _, body := fx.get(t, "/api/calls?usd_only=0"); code != 200 ||
		strings.Count(string(body), `"latest_age_seconds":null,"call_mcap_usd":null,"latest_mcap_usd":null,"has_salpha_report":`) != 9 {
		t.Fatalf("the market cap fields of rows without one: %d %s", code, body)
	}
	tagBefore, sumBefore := etagOf("/api/calls?sort=call_mc"), etagOf("/api/summary")

	// a USD-priced token whose post had a called-at of 0 and an Mcap line (stored through the seed):
	// the called-at figure is not usable, so both columns use the Mcap line
	zero, three := 0.0, 3000.0
	fx.ids["delta"] = seedWebCall(t, fx.st, base, webSeed{Msg: 20, At: 20 * time.Hour, CA: caDelta, Name: sp("Delta"), Status: TrackDone,
		Unit: "usd", Entry: 2, CalledMC: &zero, MC: &three})

	// alpha: at the post 0.002, late entry 0.003 → the estimate divides by 0.002, and starts from the Mcap line
	setMC("alpha", 40000.0, 50000.0)
	setLatest("alpha", 0.004) // 50,000 × 0.004 ÷ 0.002 = 100,000 (not 80,000 from called-at, not 66,667 from the late entry)
	// beta: only an Mcap line
	setMC("beta", nil, 1200000.0)
	setLatest("beta", 0.75) // 1,200,000 × 0.75 ÷ 1.5 = 600,000
	// gamma: only a called-at figure, and no latest price
	setMC("gamma", 40000.0, nil)
	// delta: called-at 0 (not usable: the call column falls back to the Mcap line, 3,000)
	setLatest("delta", 4) // 3,000 × 4 ÷ 2 = 6,000
	// not priced in USD: never shown
	setMC("virt", 9000000.0, 9000000.0)
	setLatest("virt", 30)
	setMC("pct", 5000000.0, 5000000.0) // no price unit at all
	setLatest("pct", 3)
	// a number that is not finite
	setMC("xss", "NaN", "NaN")

	usdKeys := "alpha beta gamma delta"
	res := fx.wantOrder(t, "sort=call_mc", "beta", "gamma", "alpha", "delta") // 1.2M; 40k, 40k (higher call id first); 3k
	if !res.USDOnly || res.Sort != "call_mc" || res.Dir != "desc" || res.Total != 4 {
		t.Fatalf("sort=call_mc (%s): %+v", usdKeys, res)
	}
	fx.wantOrder(t, "sort=call_mc&dir=asc", "delta", "alpha", "gamma", "beta")
	res = fx.wantOrder(t, "sort=latest_mc", "beta", "alpha", "delta", "gamma") // 600k, 100k, 6k; none
	if !res.USDOnly || res.Sort != "latest_mc" || res.Total != 4 {
		t.Fatalf("sort=latest_mc: %+v", res)
	}
	fx.wantOrder(t, "sort=latest_mc&dir=asc", "delta", "alpha", "beta", "gamma")
	// every row: the ones without a value last in both directions
	if res := fx.wantOrder(t, "sort=latest_mc&usd_only=0", "beta", "alpha", "delta", "gave", "err", "pct", "sol", "xss", "virt", "gamma"); res.USDOnly || res.Total != 10 {
		t.Fatalf("sort=latest_mc&usd_only=0: %+v", res)
	}
	fx.wantOrder(t, "sort=latest_mc&usd_only=0&dir=asc", "delta", "alpha", "beta", "gamma", "virt", "xss", "sol", "pct", "err", "gave")
	fx.wantOrder(t, "sort=call_mc&usd_only=0", "beta", "gamma", "alpha", "delta", "gave", "err", "pct", "sol", "xss", "virt")
	fx.wantOrder(t, "sort=call_mc&usd_only=0&dir=asc", "delta", "alpha", "gamma", "beta", "virt", "xss", "sol", "pct", "err", "gave")
	// the window does not matter for these sorts, the other filters do
	fx.wantOrder(t, "sort=call_mc&horizon=30d", "beta", "gamma", "alpha", "delta")
	fx.wantOrder(t, "sort=latest_mc&q=a&per=2", "beta", "alpha")
	fx.wantOrder(t, "sort=latest_mc&per=1&page=3", "delta")
	fx.wantOrder(t, "sort=call_mc&verdict=clean")

	by := map[string]webCallJSON{}
	all := fx.calls(t, "per=200")
	for i, k := range fx.keys(all.Calls) {
		by[k] = all.Calls[i]
	}
	near := func(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) <= 1e-9*want }
	for k, w := range map[string][2]float64{ // call, latest (-1 = none)
		"alpha": {40000, 100000}, "beta": {1200000, 600000}, "gamma": {40000, -1}, "delta": {3000, 6000},
		"virt": {-1, -1}, "pct": {-1, -1}, "xss": {-1, -1}, "sol": {-1, -1}, "err": {-1, -1}, "gave": {-1, -1},
	} {
		c := by[k]
		okCall := (w[0] < 0 && c.CallMcapUSD == nil) || (w[0] >= 0 && near(c.CallMcapUSD, w[0]))
		okLatest := (w[1] < 0 && c.LatestMcapUSD == nil) || (w[1] >= 0 && near(c.LatestMcapUSD, w[1]))
		if !okCall || !okLatest {
			t.Errorf("%s: call_mcap_usd %s latest_mcap_usd %s, want %v", k, fnum(c.CallMcapUSD), fnum(c.LatestMcapUSD), w)
		}
	}
	// the rest of a row is as before
	if a := by["alpha"]; fnum(a.EntryPriceUSD) != "0.003" || fnum(a.LatestPriceUSD) != "0.004" || fnum(a.ReturnPct) != "50" {
		t.Fatalf("alpha %+v", a)
	}
	fx.wantOrder(t, "sort=return", "alpha", "beta", "delta", "gamma") // delta has no window numbers

	// The ETag of the list follows the market caps shown; the summary's does not.
	tag := etagOf("/api/calls?sort=call_mc")
	if tag == tagBefore {
		t.Fatal("the market caps did not change the ETag of the list")
	}
	if etagOf("/api/summary") == sumBefore {
		t.Fatal("the summary's ETag did not follow the new call") // delta is one more call
	}
	sum := etagOf("/api/summary")
	steps := []struct {
		what    string
		sql     string
		changes bool
	}{
		{"the Mcap line of alpha (its estimate)", `UPDATE scout_call_metrics SET mcap_usd = 60000 WHERE call_id = $1`, true},
		{"the called-at figure of alpha (its call column)", `UPDATE scout_call_metrics SET called_at_mcap_usd = 45000 WHERE call_id = $1`, true},
		{"the price at alpha's post (the divisor)", `UPDATE scout_call_tracking SET entry_price_usd = 0.001 WHERE call_id = $1`, true},
	}
	for _, s := range steps {
		exec(s.sql, fx.ids["alpha"])
		next := etagOf("/api/calls?sort=call_mc")
		if (next != tag) != s.changes {
			t.Fatalf("%s: ETag changed %v, want %v", s.what, next != tag, s.changes)
		}
		tag = next
	}
	if a := fx.calls(t, "q=alpha").Calls[0]; !near(a.CallMcapUSD, 45000) || !near(a.LatestMcapUSD, 240000) { // 60,000 × 0.004 ÷ 0.001
		t.Fatalf("alpha after the changes: %s %s", fnum(a.CallMcapUSD), fnum(a.LatestMcapUSD))
	}
	// nothing the page shows: market caps outside USD, and the price at the post
	// of a call with a late entry and no latest price (gamma)
	exec(`UPDATE scout_call_metrics SET called_at_mcap_usd = 1, mcap_usd = 2 WHERE call_id = ANY($1)`, []int{fx.ids["virt"], fx.ids["pct"]})
	exec(`UPDATE scout_call_tracking SET entry_price_usd = 0.4 WHERE call_id = $1`, fx.ids["gamma"])
	if etagOf("/api/calls?sort=call_mc") != tag {
		t.Fatal("a change the page does not show changed the ETag")
	}
	if etagOf("/api/summary") != sum {
		t.Fatal("a market cap changed the ETag of the summary")
	}
	fx.wantOrder(t, "sort=latest_mc", "beta", "alpha", "delta", "gamma")

	// a called-at figure that is not a number: the call column falls back to the Mcap line, the estimate starts from it anyway
	setMC("beta", "NaN", 1200000.0)
	if b := fx.calls(t, "q=Beta").Calls[0]; !near(b.CallMcapUSD, 1200000) || !near(b.LatestMcapUSD, 600000) {
		t.Fatalf("beta with a NaN called-at: %s %s", fnum(b.CallMcapUSD), fnum(b.LatestMcapUSD))
	}
	fx.wantOrder(t, "sort=call_mc", "beta", "alpha", "gamma", "delta") // 1.2M, 45k, 40k, 3k
	// a Mcap line of 0: the estimate falls back to the called-at figure
	setMC("beta", 900000.0, 0.0)
	if b := fx.calls(t, "q=Beta").Calls[0]; !near(b.CallMcapUSD, 900000) || !near(b.LatestMcapUSD, 450000) { // 900,000 × 0.75 ÷ 1.5
		t.Fatalf("beta with a Mcap line of 0: %s %s", fnum(b.CallMcapUSD), fnum(b.LatestMcapUSD))
	}
	// neither figure usable: no value
	setMC("beta", -1.0, "NaN")
	if b := fx.calls(t, "q=Beta").Calls[0]; b.CallMcapUSD != nil || b.LatestMcapUSD != nil {
		t.Fatalf("beta without a usable market cap: %s %s", fnum(b.CallMcapUSD), fnum(b.LatestMcapUSD))
	}

	// the query string: usd_only defaults on for both sorts, like return, peak and latest
	for q, want := range map[string]bool{"sort=call_mc": true, "sort=latest_mc": true, "sort=call_mc&usd_only=0": false,
		"sort=latest_mc&usd_only=0": false, "sort=latest_mc&usd_only=1": true, "sort=date": false} {
		v, _ := url.ParseQuery(q)
		if f, err := parseWebCallsQuery(v); err != nil || f.USDOnly != want {
			t.Errorf("?%s: usd_only %v (%v), want %v", q, f.USDOnly, err, want)
		}
	}
	for _, bad := range []string{"sort=call_mcap_usd", "sort=LATEST_MC", "sort=latest_mc%20desc", "sort=mc", "sort=call"} {
		if code, _, body := fx.get(t, "/api/calls?"+bad); code != 400 {
			t.Errorf("?%s: %d %s", bad, code, body)
		}
	}
}
