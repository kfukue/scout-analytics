package main

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The website's "Latest %": the fields of a row, sort=latest in both
// directions with the rows without a value last, usd_only on by default, and
// an ETag that follows the latest price.
func TestWebLatestPrice(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) // the fixture's calls are posted base + 1h, + 2h, …
	set := func(key string, ret, price any, checked, trade time.Time) {
		t.Helper()
		var tr any
		if !trade.IsZero() {
			tr = trade
		}
		if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET latest_return_pct = $2, latest_price_usd = $3,
			latest_checked_at = $4, latest_trade_at = $5 WHERE call_id = $1`, fx.ids[key], ret, price, checked, tr); err != nil {
			t.Fatal(err)
		}
	}
	// before any latest price: the fields are there and null, and sort=latest still lists the USD rows
	for _, c := range fx.wantOrder(t, "sort=latest", "gamma", "beta", "alpha").Calls {
		if c.LatestReturnPct != nil || c.LatestPriceUSD != nil || c.LatestAt != nil || c.LatestTradeAt != nil || c.LatestAgeSeconds != nil {
			t.Fatalf("latest fields before the first refresh: %+v", c)
		}
	}
	if code, _, body := fx.get(t, "/api/calls?sort=latest"); code != 200 || strings.Count(string(body), `"latest_return_pct":null,"latest_price_usd":null,"latest_at":null,"latest_trade_at":null,"latest_age_seconds":null`) != 3 {
		t.Fatalf("the five fields of a row without a latest price: %d %s", code, body)
	}
	etagOf := func(path string) string {
		t.Helper()
		code, hdr, _ := fx.get(t, path)
		if code != 200 || hdr.Get("ETag") == "" {
			t.Fatalf("%s: %d ETag %q", path, code, hdr.Get("ETag"))
		}
		return hdr.Get("ETag")
	}
	tagBefore, sumBefore := etagOf("/api/calls"), etagOf("/api/summary")

	alphaAt := base.Add(time.Hour + 60*24*time.Hour + 90*time.Second) // 60 days and 90 seconds after alpha's post
	set("alpha", 35.2, 0.004056, alphaAt, alphaAt.Add(-3*time.Hour))
	set("beta", -80.0, 0.3, base.Add(40*24*time.Hour), base.Add(10*24*time.Hour)) // a quiet token: last trade 30 days before
	set("gamma", 35.2, 0.8112, base.Add(3*time.Hour+45*time.Minute), time.Time{}) // the same return as alpha; 45 minutes old
	set("virt", 150.0, 30.0, base.Add(50*time.Hour), base.Add(49*time.Hour))      // not in USD: stored, never shown
	set("pct", 12.0, 2.24, base.Add(50*time.Hour), base.Add(49*time.Hour))        // no price unit at all: the same

	// ties by call id in the direction asked for; USD rows only by default
	res := fx.wantOrder(t, "sort=latest", "gamma", "alpha", "beta")
	if !res.USDOnly || res.Sort != "latest" || res.Dir != "desc" || res.Total != 3 {
		t.Fatalf("sort=latest: %+v", res)
	}
	fx.wantOrder(t, "sort=latest&dir=asc", "beta", "alpha", "gamma")
	// with every row: the ones without a value come last in both directions
	if res := fx.wantOrder(t, "sort=latest&usd_only=0", "gamma", "alpha", "beta", "gave", "err", "pct", "sol", "xss", "virt"); res.USDOnly || res.Total != 9 {
		t.Fatalf("usd_only=0: %+v", res)
	}
	fx.wantOrder(t, "sort=latest&usd_only=0&dir=asc", "beta", "alpha", "gamma", "virt", "xss", "sol", "pct", "err", "gave")
	// the window does not matter for this sort, the other filters do
	fx.wantOrder(t, "sort=latest&horizon=30d", "gamma", "alpha", "beta")
	fx.wantOrder(t, "sort=latest&q=a&per=2", "gamma", "alpha")
	fx.wantOrder(t, "sort=latest&per=1&page=2", "alpha")
	fx.wantOrder(t, "sort=latest&verdict=clean")

	by := map[string]webCallJSON{}
	all := fx.calls(t, "sort=latest&usd_only=0")
	for i, k := range fx.keys(all.Calls) {
		by[k] = all.Calls[i]
	}
	a := by["alpha"]
	if fnum(a.LatestReturnPct) != "35.2" || fnum(a.LatestPriceUSD) != "0.004056" || strOrNil(a.LatestAt) != "2026-10-31T01:01:30Z" ||
		strOrNil(a.LatestTradeAt) != "2026-10-30T22:01:30Z" || a.LatestAgeSeconds == nil || *a.LatestAgeSeconds != 60*86400+90 ||
		fnum(a.ReturnPct) != "50" || fnum(a.PeakPct) != "120" { // the window's numbers are as before
		t.Fatalf("alpha %+v", a)
	}
	if b := by["beta"]; fnum(b.LatestReturnPct) != "-80" || *b.LatestAgeSeconds != (40*24-2)*3600 ||
		strOrNil(b.LatestAt) != "2026-10-11T00:00:00Z" || strOrNil(b.LatestTradeAt) != "2026-09-11T00:00:00Z" {
		t.Fatalf("beta %+v", b)
	}
	if g := by["gamma"]; fnum(g.LatestReturnPct) != "35.2" || *g.LatestAgeSeconds != 45*60 || g.LatestAt == nil || g.LatestTradeAt != nil {
		t.Fatalf("gamma %+v", g)
	}
	for _, k := range []string{"virt", "pct", "xss", "sol", "err", "gave"} {
		if c := by[k]; c.LatestReturnPct != nil || c.LatestPriceUSD != nil || c.LatestAt != nil || c.LatestTradeAt != nil || c.LatestAgeSeconds != nil {
			t.Fatalf("%s shows a latest price: %+v", k, c)
		}
	}
	// the other sorts and the window's numbers are untouched by all this
	fx.wantOrder(t, "sort=return&horizon=1d", "alpha", "beta", "gamma")
	fx.wantOrder(t, "", "gave", "err", "pct", "sol", "xss", "virt", "gamma", "beta", "alpha")

	// The ETag of the list follows the latest price; the summary's does not.
	tag := etagOf("/api/calls")
	if tag == tagBefore {
		t.Fatal("the first latest prices did not change the ETag of the list")
	}
	if etagOf("/api/summary") != sumBefore {
		t.Fatal("a latest price changed the ETag of the summary")
	}
	if etagOf("/api/calls") != tag {
		t.Fatal("the ETag changed although nothing did")
	}
	set("alpha", 36.0, 0.00408, alphaAt, alphaAt.Add(-3*time.Hour))
	tag2 := etagOf("/api/calls")
	if tag2 == tag {
		t.Fatal("a new latest price did not change the ETag")
	}
	set("alpha", 36.0, 0.00408, alphaAt.Add(15*time.Minute), alphaAt.Add(-3*time.Hour)) // read again, same price
	tag3 := etagOf("/api/calls")
	if tag3 == tag2 {
		t.Fatal("a later reading (another age) did not change the ETag")
	}
	set("virt", 999.0, 99.0, base.Add(60*time.Hour), base.Add(59*time.Hour)) // nothing the page shows
	if etagOf("/api/calls") != tag3 {
		t.Fatal("a latest price outside USD changed the ETag")
	}
	fx.wantOrder(t, "sort=latest", "alpha", "gamma", "beta")

	// the query string: usd_only defaults on for sort=latest, like return and peak
	for q, want := range map[string]bool{"sort=latest": true, "sort=latest&usd_only=0": false, "sort=latest&usd_only=1": true, "sort=date": false} {
		v, _ := url.ParseQuery(q)
		if f, err := parseWebCallsQuery(v); err != nil || f.USDOnly != want {
			t.Errorf("?%s: usd_only %v (%v), want %v", q, f.USDOnly, err, want)
		}
	}
	for _, bad := range []string{"sort=latest_return_pct", "sort=LATEST", "sort=latest%20desc", "sort=late"} {
		if code, _, body := fx.get(t, "/api/calls?"+bad); code != 400 {
			t.Errorf("?%s: %d %s", bad, code, body)
		}
	}
}
