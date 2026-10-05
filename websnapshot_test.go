package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// syntheticWebRows makes n rows the way ScoutStore.SelectWebRows returns them
// (call id order), with every kind of gap the real data has: no name, no
// symbol, no price unit, another price unit, windows not reached yet, equal
// values, equal dates, NaN, and tokens with and without a Perceptor report.
func syntheticWebRows(n int, seed int64) []ScoutWebRow {
	rng := rand.New(rand.NewSource(seed))
	first := []string{"Moon", "Pepe", "Robin", "Doge", "Alpha", "Turbo", "Sigma", "Hood", "Wagmi", "Based", "Chad", "Frog", "Giga", "Laser", "Rocket", "Shiba", "Wojak"}
	second := []string{"Coin", "Inu", "Token", "AI", "Cat", "Finance", "Protocol", "Dog", "Labs"}
	statuses := []string{TrackPending, TrackTracking, TrackDone, TrackDone, TrackDone, TrackNoPool, TrackError, TrackGaveUp}
	verdicts := []string{levelClean, levelClean, levelCaution, levelRedFlags, levelUnknown}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]ScoutWebRow, n)
	lrng := rand.New(rand.NewSource(seed + 1000)) // the latest price has its own source, so the rest stays as it was
	for i := range rows {
		r := &rows[i]
		r.CallID = 10 + i*2
		r.MessageID = 10000 + i
		r.MessageDate = base.Add(time.Duration(rng.Intn(n/2+1)) * 20 * time.Minute) // some posts share a time
		r.ChannelUsername = "scoutrobinhood"
		r.ContractAddress = fmt.Sprintf("0x%040x", rng.Uint64())
		if i%97 == 0 {
			r.ContractAddress = "So1111111111111111111111111111111111111111" + fmt.Sprint(i)
		}
		if i%15 != 7 {
			r.TokenName = sp(first[rng.Intn(len(first))] + " " + second[rng.Intn(len(second))] + " " + fmt.Sprint(i))
		}
		if i%11 != 3 {
			r.TokenSymbol = sp("T" + fmt.Sprint(i))
		}
		r.TrackingStatus = sp(statuses[rng.Intn(len(statuses))])
		r.CallCount = 1 + rng.Intn(3)*rng.Intn(2)
		r.LastCallDate = r.MessageDate.Add(time.Duration(r.CallCount-1) * time.Hour)
		switch k := i % 10; {
		case k == 0 || k == 1:
			// not tracked: no unit, no price
		case k == 2:
			r.PriceUnit, r.Tracked, r.EntryPrice = sp("VIRT"), true, new(float64)
			*r.EntryPrice = 12
		case k == 3:
			r.Tracked, r.EntryPrice = true, new(float64) // a price but no unit
			*r.EntryPrice = 2
		default:
			r.PriceUnit, r.Tracked, r.EntryPrice = sp("usd"), true, new(float64)
			*r.EntryPrice = 0.0001 * float64(1+rng.Intn(5000))
		}
		if r.Tracked {
			windows := rng.Intn(len(ScoutWebHorizons) + 1) // the first few windows are done
			for h := 0; h < windows; h++ {
				ret := float64(rng.Intn(400)-95) + float64(rng.Intn(3))/2 // few different values: many ties
				vals := [3]float64{ret, math.Max(ret, 0) + float64(rng.Intn(70)), math.Min(ret, 0) - float64(rng.Intn(60))}
				for k, v := range vals {
					if rng.Intn(50) == 0 {
						continue // this one number is missing
					}
					if rng.Intn(400) == 0 {
						v = math.NaN()
					}
					r.Perf[h*webPerfPerHorizon+k] = v
					r.HasPerf |= 1 << (h*webPerfPerHorizon + k)
				}
			}
			if rng.Intn(9) == 0 {
				yes := rng.Intn(2) == 0
				r.Rugged = &yes
			}
		}
		// a latest price: on most tracked rows (also the ones not in USD, which
		// must not show it), few different values, now and then one that JSON
		// cannot carry or one without the time it was read
		if r.Tracked && lrng.Intn(4) != 0 {
			ret := float64(lrng.Intn(60)-30) * 12.5
			switch lrng.Intn(60) {
			case 0:
				ret = math.NaN()
			case 1:
				ret = math.Inf(1)
			}
			price := 0.001 * float64(1+lrng.Intn(900))
			at := r.MessageDate.Add(time.Duration(1+lrng.Intn(90*24*60)) * time.Minute)
			trade := at.Add(-time.Duration(lrng.Intn(20*24*60)) * time.Minute)
			r.LatestReturn, r.LatestPrice, r.LatestAt, r.LatestTradeAt = &ret, &price, &at, &trade
			if lrng.Intn(40) == 0 {
				r.LatestAt = nil
			}
		}
		if rng.Intn(4) == 0 {
			r.PerceptorVerd = sp(verdicts[rng.Intn(len(verdicts))])
			r.PerceptorURL = sp(fmt.Sprintf("https://www.perceptor.info/r/%032x", i))
			if i%13 == 0 {
				r.PerceptorURL = sp("http://www.perceptor.info/r/plain")
			}
		}
	}
	return rows
}

// referencePage answers a request the slow, obvious way: filter every row, sort
// the matches with the full rule (value in the asked direction, rows without a
// value last, ties by call id in the same direction), cut out the page.
func referencePage(rows []ScoutWebRow, f ScoutWebCallsFilter) (ids []int, total int) {
	h, _ := webHorizonIndex(f.Horizon)
	q := strings.ToLower(f.Q)
	has := func(p *string) bool { return p != nil && strings.Contains(strings.ToLower(*p), q) }
	var match []*ScoutWebRow
	for i := range rows {
		r := &rows[i]
		usd := r.PriceUnit != nil && *r.PriceUnit == "usd"
		if f.USDOnly && !usd {
			continue
		}
		if q != "" && !has(r.TokenName) && !has(r.TokenSymbol) && !strings.Contains(strings.ToLower(r.ContractAddress), q) {
			continue
		}
		verdict := "not_scanned"
		if r.PerceptorVerd != nil && *r.PerceptorVerd != levelUnknown {
			verdict = *r.PerceptorVerd
		}
		if f.Verdict != "" && f.Verdict != verdict {
			continue
		}
		match = append(match, r)
	}
	// value of the sort key: ok = false when the row has none
	key := func(r *ScoutWebRow) (float64, bool) {
		if f.Sort == "date" {
			return float64(r.MessageDate.Unix()), true
		}
		if f.Sort == "latest" {
			// shown (and so sorted) only with a USD price, the time it was read and a number JSON can carry
			if r.PriceUnit == nil || *r.PriceUnit != "usd" || r.LatestAt == nil || finite(r.LatestReturn) == nil {
				return 0, false
			}
			return *r.LatestReturn, true
		}
		i := h * webPerfPerHorizon
		if f.Sort == "peak" {
			i++
		}
		if r.PriceUnit == nil || *r.PriceUnit != "usd" || r.HasPerf&(1<<i) == 0 {
			return 0, false
		}
		return r.Perf[i], true
	}
	// PostgreSQL's order of float8: NaN is above every number
	less := func(a, b float64) bool {
		if math.IsNaN(a) || math.IsNaN(b) {
			return !math.IsNaN(a) && math.IsNaN(b)
		}
		return a < b
	}
	sort.SliceStable(match, func(i, j int) bool {
		a, aok := key(match[i])
		b, bok := key(match[j])
		if aok != bok {
			return aok // rows without a value come last, in both directions
		}
		if aok && (less(a, b) || less(b, a)) {
			if f.Dir == "asc" {
				return less(a, b)
			}
			return less(b, a)
		}
		if f.Dir == "asc" {
			return match[i].CallID < match[j].CallID
		}
		return match[i].CallID > match[j].CallID
	})
	ids = []int{}
	for i := (f.Page - 1) * f.Per; i < len(match) && len(ids) < f.Per; i++ {
		ids = append(ids, match[i].CallID)
	}
	return ids, len(match)
}

func cloneWebRows(rows []ScoutWebRow) []ScoutWebRow { return append([]ScoutWebRow(nil), rows...) }

var testWebConfig = webConfig{GMGNTemplate: defaultGMGNTemplate}

// mustWebSnapshot builds a snapshot from rows (which it changes in place).
func mustWebSnapshot(tb testing.TB, rows []ScoutWebRow, updatePosts int, prev *webSnapshot) *webSnapshot {
	tb.Helper()
	s, err := newWebSnapshot(rows, updatePosts, prev, testWebConfig)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// pageIDs returns the call ids of one page and the total.
func pageIDs(tb testing.TB, s *webSnapshot, f ScoutWebCallsFilter) ([]int, int) {
	tb.Helper()
	pos, total, err := s.page(f, nil)
	if err != nil {
		tb.Fatalf("%+v: %v", f, err)
	}
	ids := []int{}
	for _, p := range pos {
		ids = append(ids, int(s.ids[p]))
	}
	return ids, total
}

// sentCall decodes row pos as the page gets it for window h.
func sentCall(tb testing.TB, s *webSnapshot, pos, h int) webCallJSON {
	tb.Helper()
	var c webCallJSON
	dec := json.NewDecoder(bytes.NewReader(s.appendRow(nil, int32(pos), h)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		tb.Fatalf("row %d: %v", pos, err)
	}
	return c
}

// TestWebSnapshotPageMatchesReference: the pre-sorted lists and the shortcuts
// of webSnapshot.page give the same rows, in the same order, with the same
// total as the obvious way, for every sort, direction, window and filter.
func TestWebSnapshotPageMatchesReference(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 600} {
		raw := syntheticWebRows(n, int64(n)+1)
		snap := mustWebSnapshot(t, cloneWebRows(raw), 3, nil)
		checked := 0
		for _, sortBy := range []string{"date", "return", "peak", "latest"} {
			for _, dir := range []string{"desc", "asc"} {
				for _, hz := range ScoutWebHorizons {
					for _, usdOnly := range []bool{false, true} {
						for _, verdict := range []string{"", "clean", "caution", "red_flags", "not_scanned"} {
							for _, q := range []string{"", "pe", "PEPE c", "0x", "t1", "so1111", "nothing matches"} {
								for _, pg := range [][2]int{{1, 50}, {2, 50}, {3, 7}, {1, 200}, {1000000, 200}, {1, 1}} {
									f := ScoutWebCallsFilter{Q: q, Sort: sortBy, Dir: dir, Horizon: hz, USDOnly: usdOnly, Verdict: verdict, Page: pg[0], Per: pg[1]}
									got, total := pageIDs(t, snap, f)
									want, wantTotal := referencePage(raw, f)
									if total != wantTotal || fmt.Sprint(got) != fmt.Sprint(want) {
										t.Fatalf("n=%d %+v:\n got %d %v\nwant %d %v", n, f, total, got, wantTotal, want)
									}
									checked++
								}
							}
						}
					}
				}
			}
		}
		if checked != 4*2*5*2*5*7*6 {
			t.Fatalf("checked %d combinations", checked)
		}
	}
	snap := mustWebSnapshot(t, syntheticWebRows(20, 1), 0, nil)
	for _, bad := range []ScoutWebCallsFilter{
		{Sort: "price", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "date", Dir: "up", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "2d", Page: 1, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 0, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 0},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 10, Verdict: "unknown"},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 10, Q: "a\x00b"},
	} {
		if _, _, err := snap.page(bad, nil); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

// TestWebSnapshotRowRules: what a snapshot does to the rows once, when it is built.
func TestWebSnapshotRowRules(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	rows := []ScoutWebRow{
		{CallID: 1, MessageID: 5, ChannelUsername: "scoutrobinhood", ContractAddress: caAlpha, TokenName: sp("Ünï Çoin"), TokenSymbol: sp("UNI"),
			PriceUnit: sp("usd"), EntryPrice: &inf, Tracked: true, TrackingStatus: sp(TrackDone), CallCount: 3,
			Perf: [15]float64{1: 7, 3: nan, 4: 40, 5: -3}, HasPerf: 1<<1 | 1<<3 | 1<<4 | 1<<5,
			PerceptorVerd: sp(levelClean), PerceptorURL: sp("javascript:alert(1)")},
		{CallID: 2, MessageID: 0, ChannelUsername: "bad name", ContractAddress: caSol, PriceUnit: sp("VIRT"), EntryPrice: new(float64), Tracked: true,
			TrackingStatus: sp(TrackTracking), CallCount: 1, Perf: [15]float64{3: 999, 4: 1999}, HasPerf: 1<<3 | 1<<4,
			PerceptorVerd: sp(levelUnknown), PerceptorURL: sp("https://www.perceptor.info/r/x")},
		{CallID: 3, MessageID: 9, ChannelUsername: "@scoutrobinhood", ContractAddress: caBeta, CallCount: 1},
	}
	snap := mustWebSnapshot(t, rows, 4, nil)
	want := ScoutWebSummary{Imported: 3, Tracked: 2, Tracking: 1, Done: 1, NoUSDPrice: 1, TotalCalls: 5, RepeatCalls: 2, UpdatePosts: 4}
	if snap.summary != want {
		t.Fatalf("summary %+v\nwant    %+v", snap.summary, want)
	}
	if snap.counts != [2][webBuckets + 1]int{{1, 0, 0, 2, 3}, {1, 0, 0, 0, 1}} {
		t.Fatalf("counts %v", snap.counts)
	}
	a := sentCall(t, snap, 0, 1) // the 1d window; +Inf and NaN are left out
	if a.EntryPriceUSD != nil || a.ReturnPct != nil || fnum(a.PeakPct) != "40" || fnum(a.DrawdownPct) != "-3" ||
		a.PerceptorURL != nil || strOrNil(a.Perceptor) != levelClean || strOrNil(a.PostURL) != "https://t.me/scoutrobinhood/5" ||
		strOrNil(a.GMGNURL) != "https://gmgn.ai/robinhood/token/"+caAlpha || a.CallID != 1 || a.CallCount != 3 {
		t.Fatalf("usd row %+v", a)
	}
	if h := sentCall(t, snap, 0, 0); h.ReturnPct != nil || fnum(h.PeakPct) != "7" || h.DrawdownPct != nil {
		t.Fatalf("1h of the usd row %+v", h)
	}
	b := sentCall(t, snap, 1, 1)
	if b.EntryPriceUSD != nil || b.ReturnPct != nil || b.PeakPct != nil || b.DrawdownPct != nil || strOrNil(b.PriceUnit) != "VIRT" ||
		b.PostURL != nil || b.GMGNURL != nil || strOrNil(b.Perceptor) != levelUnknown || strOrNil(b.PerceptorURL) != "https://www.perceptor.info/r/x" {
		t.Fatalf("non-usd row %+v", b)
	}
	if c := sentCall(t, snap, 2, 4); c.PriceUnit != nil || c.TrackingStatus != nil || c.Perceptor != nil || strOrNil(c.PostURL) != "https://t.me/scoutrobinhood/9" {
		t.Fatalf("bare row %+v", c)
	}
	// the search text is lower-cased once, also beyond ASCII
	for q, wantIDs := range map[string]string{"ünï ç": "[1]", "ÜNÏ": "[1]", "uni": "[1]", "0x2222": "[3]", "so1111": "[2]", "": "[3 2 1]",
		"coin0x": "[]", "uni0x1111": "[]"} { // a text never matches across two fields
		ids, total := pageIDs(t, snap, ScoutWebCallsFilter{Q: q, Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 10})
		if fmt.Sprint(ids) != wantIDs || total != len(ids) {
			t.Errorf("q=%q: %v (total %d), want %s", q, ids, total, wantIDs)
		}
	}
	// NaN sorts as the highest value (as in the database) and is shown as no value
	if ids, _ := pageIDs(t, snap, ScoutWebCallsFilter{Sort: "return", Dir: "desc", Horizon: "1d", Page: 1, Per: 10}); fmt.Sprint(ids) != "[1 3 2]" {
		t.Fatalf("sort=return: %v", ids)
	}
}

// TestWebSnapshotRowJSON: the rows a snapshot keeps already encoded, with the
// numbers of the window put in per request, are byte for byte what
// encoding/json makes of the same ScoutWebCall.
func TestWebSnapshotRowJSON(t *testing.T) {
	raw := syntheticWebRows(400, 3)
	odd := []float64{0, -0.0, 1e-7, 9.99e-7, 1e-6, 1e20, 1e21, -1e21, 1.5e-9, 123456789.125, -99.9, 0.1 + 0.2, math.MaxFloat64, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), math.Inf(-1)}
	for i := range raw {
		if raw[i].PriceUnit != nil && *raw[i].PriceUnit == "usd" && i%3 == 0 {
			raw[i].Perf[i%15], raw[i].HasPerf = odd[i%len(odd)], raw[i].HasPerf|1<<(i%15)
		}
	}
	raw[1].TokenName = sp(xssName + ` "quoted" \ back\u2028 line` + "\u2028\x01é,\"return_pct\":null,\"peak_pct\":null,\"drawdown_pct\":null,")
	raw[2].TokenSymbol = sp(`,"return_pct":null,"peak_pct":null,"drawdown_pct":null,`)
	prepared := cloneWebRows(raw)
	snap := mustWebSnapshot(t, prepared, 0, nil) // prepares the rows in place: USD only, https only
	for i := range prepared {
		r := &prepared[i]
		for h := range ScoutWebHorizons {
			want := testWebConfig.webCall(r)
			num := func(k int) *float64 {
				j := h*webPerfPerHorizon + k
				if r.HasPerf&(1<<j) == 0 {
					return nil
				}
				return finite(&r.Perf[j])
			}
			want.ReturnPct, want.PeakPct, want.DrawdownPct = num(webPerfReturn), num(webPerfPeak), num(webPerfDrawdown)
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if got := snap.appendRow(nil, int32(i), h); !bytes.Equal(got, wantJSON) {
				t.Fatalf("row %d window %s:\n got %s\nwant %s", i, ScoutWebHorizons[h], got, wantJSON)
			}
		}
	}
	for _, v := range append(odd[:len(odd)-3], 1, -1, 100, 1e6, 12345.678, 2.5e-7, 1e-10, 3e22) {
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := appendJSONFloat(nil, v); !bytes.Equal(got, want) {
			t.Errorf("appendJSONFloat(%v) = %s, encoding/json writes %s", v, got, want)
		}
	}
	// the envelope of /api/calls ends the way handleCalls expects
	env, err := json.Marshal(webCallsResponse{Calls: []ScoutWebCall{}})
	if err != nil || !bytes.HasSuffix(env, []byte(webNoCallsJSON)) {
		t.Fatalf("envelope %s (%v)", env, err)
	}
}

// TestWebSnapshotVersion: the version is a hash of the content. The same data
// gives the same version (and the lists built before are used again); any
// change that the page could show gives another one.
func TestWebSnapshotVersion(t *testing.T) {
	raw := syntheticWebRows(300, 7)
	a := mustWebSnapshot(t, cloneWebRows(raw), 5, nil)
	b := mustWebSnapshot(t, cloneWebRows(raw), 5, a)
	if a.version == "" || a.version != b.version {
		t.Fatalf("versions %q %q", a.version, b.version)
	}
	if a == b || &a.rowJSON[0] != &b.rowJSON[0] || &a.order[0][0] != &b.order[0][0] || &a.search[0] != &b.search[0] || a.n != b.n ||
		a.counts != b.counts || a.summary != b.summary {
		t.Fatal("an unchanged snapshot must share the blocks of the one before")
	}
	if c := mustWebSnapshot(t, cloneWebRows(raw), 5, nil); c.version != a.version || &c.rowJSON[0] == &a.rowJSON[0] {
		t.Fatalf("built alone: %q, want %q", c.version, a.version)
	}
	// the link template is part of every row
	if c, err := newWebSnapshot(cloneWebRows(raw), 5, a, webConfig{GMGNTemplate: "https://example.test/{ca}"}); err != nil || c.version == a.version {
		t.Fatalf("another SCOUT_GMGN_URL: version %q (%v)", c.version, err)
	}
	// rows must come in call id order
	swapped := cloneWebRows(raw)
	swapped[3], swapped[4] = swapped[4], swapped[3]
	if _, err := newWebSnapshot(swapped, 5, nil, testWebConfig); err == nil {
		t.Fatal("rows out of order: no error")
	}
	if a.summaryTag == "" || b.summaryTag != a.summaryTag {
		t.Fatalf("summary tags %q %q", a.summaryTag, b.summaryTag)
	}
	// the tag of the counts follows the counts, not the rows
	if c := mustWebSnapshot(t, cloneWebRows(raw), 6, a); c.summaryTag == a.summaryTag {
		t.Fatal("one more update post: same summary tag")
	}
	renamed := cloneWebRows(raw)
	renamed[0].TokenSymbol = sp("OTHER")
	if c := mustWebSnapshot(t, renamed, 5, a); c.summaryTag != a.summaryTag || c.version == a.version {
		t.Fatal("a renamed token must change the version and keep the summary tag")
	}
	seen := map[string]string{a.version: "unchanged"}
	change := func(what string, updatePosts int, edit func(rows []ScoutWebRow)) {
		t.Helper()
		rows := cloneWebRows(raw)
		// deep-copy what the edits below touch
		for i := range rows {
			if rows[i].TokenName != nil {
				rows[i].TokenName = sp(*rows[i].TokenName)
			}
		}
		edit(rows)
		c := mustWebSnapshot(t, rows, updatePosts, a)
		if prev, dup := seen[c.version]; dup {
			t.Errorf("%s: same version as %s", what, prev)
		}
		seen[c.version] = what
		if &c.rowJSON[0] == &a.rowJSON[0] {
			t.Errorf("%s: the old blocks were kept", what)
		}
	}
	change("update posts", 6, func([]ScoutWebRow) {})
	change("last row replaced", 5, func(rows []ScoutWebRow) { rows[len(rows)-1] = ScoutWebRow{CallID: 1 << 30} })
	change("name", 5, func(rows []ScoutWebRow) { rows[0].TokenName = sp("renamed") })
	change("name removed", 5, func(rows []ScoutWebRow) { rows[0].TokenName = nil })
	change("status", 5, func(rows []ScoutWebRow) { rows[4].TrackingStatus = sp("other") })
	change("call count", 5, func(rows []ScoutWebRow) { rows[4].CallCount += 1 })
	change("last call", 5, func(rows []ScoutWebRow) { rows[4].LastCallDate = rows[4].LastCallDate.Add(time.Second) })
	change("date", 5, func(rows []ScoutWebRow) { rows[4].MessageDate = rows[4].MessageDate.Add(time.Microsecond) })
	change("a 30d number", 5, func(rows []ScoutWebRow) { rows[4].Perf[14], rows[4].HasPerf = 1.25, rows[4].HasPerf|1<<14 })
	change("unit", 5, func(rows []ScoutWebRow) { rows[4].PriceUnit = sp("eth") })
	change("verdict", 5, func(rows []ScoutWebRow) { rows[5].PerceptorVerd = sp("clean!") })
	change("report", 5, func(rows []ScoutWebRow) { rows[5].PerceptorURL = sp("https://example.org/r") })
	change("rugged", 5, func(rows []ScoutWebRow) { no := false; rows[0].Rugged = &no })
	change("tracked", 5, func(rows []ScoutWebRow) { rows[0].Tracked = !rows[0].Tracked })
	// the latest price: row 4 is priced in USD and has one
	if r := raw[4]; r.PriceUnit == nil || *r.PriceUnit != "usd" || r.LatestAt == nil || finite(r.LatestReturn) == nil || r.LatestTradeAt == nil {
		t.Fatalf("row 4 has no latest price to change: %+v", r)
	}
	change("latest return", 5, func(rows []ScoutWebRow) { v := *rows[4].LatestReturn + 0.5; rows[4].LatestReturn = &v })
	change("latest price", 5, func(rows []ScoutWebRow) { v := *rows[4].LatestPrice * 2; rows[4].LatestPrice = &v })
	change("latest read at", 5, func(rows []ScoutWebRow) { v := rows[4].LatestAt.Add(15 * time.Minute); rows[4].LatestAt = &v })
	change("latest trade at", 5, func(rows []ScoutWebRow) { v := rows[4].LatestTradeAt.Add(-time.Hour); rows[4].LatestTradeAt = &v })
	change("latest trade unknown", 5, func(rows []ScoutWebRow) { rows[4].LatestTradeAt = nil })
	change("no latest price", 5, func(rows []ScoutWebRow) { rows[4].LatestAt = nil })
	// … and one that the page does not show changes nothing: a latest price of a call not priced in USD
	other := cloneWebRows(raw)
	changed := false
	for i := range other {
		if r := &other[i]; r.PriceUnit != nil && *r.PriceUnit != "usd" && r.LatestReturn != nil {
			v := *r.LatestReturn + 1
			r.LatestReturn, changed = &v, true
		}
	}
	if c := mustWebSnapshot(t, other, 5, a); !changed || c.version != a.version {
		t.Fatalf("a latest price outside USD (changed %v): version %q, want %q", changed, c.version, a.version)
	}
}

func TestParseWebRefresh(t *testing.T) {
	for in, want := range map[string]struct {
		d    time.Duration
		warn bool
	}{
		"":       {15 * time.Second, false},
		"  ":     {15 * time.Second, false},
		"15s":    {15 * time.Second, false},
		"2s":     {2 * time.Second, false},
		"2500ms": {2500 * time.Millisecond, false},
		" 1m ":   {time.Minute, false},
		"1h":     {time.Hour, false},
		"1999ms": {2 * time.Second, true}, // below the minimum
		"1s":     {2 * time.Second, true},
		"0":      {2 * time.Second, true},
		"0s":     {2 * time.Second, true},
		"-5s":    {2 * time.Second, true},
		"30":     {15 * time.Second, true}, // no unit: not a duration
		"abc":    {15 * time.Second, true},
		"15 s":   {15 * time.Second, true},
	} {
		d, warn := parseWebRefresh(in)
		if d != want.d || (warn != "") != want.warn {
			t.Errorf("parseWebRefresh(%q) = %s, %q; want %s, warning %v", in, d, warn, want.d, want.warn)
		}
	}
	t.Setenv("SCOUT_WEB_REFRESH", "45s")
	if cfg := loadWebConfig(); cfg.Refresh != 45*time.Second {
		t.Errorf("SCOUT_WEB_REFRESH=45s: %s", cfg.Refresh)
	}
	t.Setenv("SCOUT_WEB_REFRESH", "100ms")
	if cfg := loadWebConfig(); cfg.Refresh != 2*time.Second {
		t.Errorf("SCOUT_WEB_REFRESH=100ms: %s", cfg.Refresh)
	}
	t.Setenv("SCOUT_WEB_REFRESH", "")
	if cfg := loadWebConfig(); cfg.Refresh != 15*time.Second {
		t.Errorf("default: %s", cfg.Refresh)
	}
}

func TestWebLogLimiter(t *testing.T) {
	l := logLimiter{every: time.Minute}
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var got []bool
	for _, after := range []time.Duration{0, time.Second, 59 * time.Second, time.Minute, 61 * time.Second, 2 * time.Minute} {
		got = append(got, l.allow(t0.Add(after)))
	}
	if fmt.Sprint(got) != "[true false false true false true]" {
		t.Fatalf("allowed %v", got)
	}
}

func TestWebAcceptsGzipAndETagMatch(t *testing.T) {
	for in, want := range map[string]bool{
		"gzip": true, "gzip, deflate, br": true, "deflate, gzip;q=0.5": true, "GZIP": true, "x-gzip": true, "br;q=1.0, gzip;q=0.8": true,
		"": false, "identity": false, "br": false, "gzip;q=0": false, "gzip; q=0.0": false, "deflate, gzip;q=0": false, "gzipped": false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if in != "" {
			r.Header.Set("Accept-Encoding", in)
		}
		if got := acceptsGzip(r); got != want {
			t.Errorf("Accept-Encoding %q: %v, want %v", in, got, want)
		}
	}
	for _, c := range []struct {
		header, etag string
		want         bool
	}{
		{`"abc"`, `"abc"`, true}, {`W/"abc"`, `"abc"`, true}, {`"abc"`, `W/"abc"`, true}, {`W/"abc"`, `W/"abc"`, true},
		{`"x", W/"abc" , "y"`, `W/"abc"`, true}, {`*`, `"abc"`, true},
		{``, `"abc"`, false}, {`"abd"`, `"abc"`, false}, {`abc`, `"abc"`, false}, {`"abc-gz"`, `"abc"`, false}, {`"abc"`, ``, false},
	} {
		if got := etagMatches(c.header, c.etag); got != c.want {
			t.Errorf("etagMatches(%q, %q) = %v", c.header, c.etag, got)
		}
	}
}

// benchWebServer is a website over a made-up snapshot of n rows (no database).
func benchWebServer(tb testing.TB, n int) *webServer {
	tb.Helper()
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		tb.Fatal(err)
	}
	ws, err := newWebServer(nil, testWebConfig, static)
	if err != nil {
		tb.Fatal(err)
	}
	snap := mustWebSnapshot(tb, syntheticWebRows(n, 42), 600, nil)
	snap.loadedAt = time.Now().UTC()
	ws.snap.Store(snap)
	return ws
}

// TestWebKeptAnswers: an answer of /api/calls is kept for its snapshot and
// sent again, byte for byte, when the same question comes back; the store has
// a fixed size and a full one changes nothing but the work done.
func TestWebKeptAnswers(t *testing.T) {
	ws := benchWebServer(t, 300)
	plainSrv := benchWebServer(t, 300) // the same data, nothing kept
	plainSrv.snap.Load().loadedAt = ws.snap.Load().loadedAt
	snap := ws.snap.Load()
	snap.answers = &webAnswers{}
	get := func(srv *webServer, path string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	unzip := func(b []byte) []byte {
		t.Helper()
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, q := range []string{"", "q=pe&verdict=clean&sort=peak", "sort=return&dir=asc&horizon=7d", "q=nothing+matches", "per=200&page=2"} {
		want := get(plainSrv, "/api/calls?"+q)
		first := get(ws, "/api/calls?"+q)
		again := get(ws, "/api/calls?"+q)
		if want.Code != 200 || first.Code != 200 || again.Code != 200 || !bytes.Equal(first.Body.Bytes(), want.Body.Bytes()) || !bytes.Equal(again.Body.Bytes(), want.Body.Bytes()) {
			t.Fatalf("?%s: kept answer differs\n%s\n%s", q, again.Body, want.Body)
		}
		for _, k := range []string{"Content-Type", "ETag", "Cache-Control", "Vary", "X-Snapshot-At", "Content-Length", "Content-Security-Policy"} {
			if again.Header().Get(k) != want.Header().Get(k) || want.Header().Get(k) == "" {
				t.Errorf("?%s: header %s %q, want %q", q, k, again.Header().Get(k), want.Header().Get(k))
			}
		}
		zipped := get(ws, "/api/calls?"+q, "Accept-Encoding", "gzip")
		if len(want.Body.Bytes()) >= webGzipMinBytes {
			if zipped.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(unzip(zipped.Body.Bytes()), want.Body.Bytes()) {
				t.Fatalf("?%s: kept gzip answer differs", q)
			}
			if z2 := get(ws, "/api/calls?"+q, "Accept-Encoding", "gzip"); !bytes.Equal(z2.Body.Bytes(), zipped.Body.Bytes()) {
				t.Fatalf("?%s: second gzip answer differs", q)
			}
		} else if zipped.Header().Get("Content-Encoding") != "" || !bytes.Equal(zipped.Body.Bytes(), want.Body.Bytes()) {
			t.Fatalf("?%s: a small answer was compressed", q)
		}
		if rec := get(ws, "/api/calls?"+q, "If-None-Match", again.Header().Get("ETag")); rec.Code != 304 || rec.Body.Len() != 0 {
			t.Fatalf("?%s: If-None-Match: %d", q, rec.Code)
		}
	}
	if n := len(snap.answers.m); n != 5 {
		t.Fatalf("%d answers kept, want 5", n)
	}
	// the same question written another way is the same kept answer
	get(ws, "/api/calls?sort=peak&q=PE&verdict=clean&page=1")
	if n := len(snap.answers.m); n != 5 {
		t.Fatalf("%d answers kept after a respelled question, want 5", n)
	}
	// bad requests are not kept
	if rec := get(ws, "/api/calls?sort=x"); rec.Code != 400 || len(snap.answers.m) != 5 {
		t.Fatalf("bad request: %d, %d kept", rec.Code, len(snap.answers.m))
	}
	// full: later questions are still answered, and correctly
	for i := 1; i <= webAnswersMax+50; i++ {
		q := fmt.Sprintf("/api/calls?per=1&page=%d", i)
		if got, want := get(ws, q), get(plainSrv, q); got.Code != 200 || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
			t.Fatalf("%s: %d %s", q, got.Code, got.Body)
		}
	}
	if n := len(snap.answers.m); n != webAnswersMax || snap.answers.bytes > webAnswersMaxBytes || snap.answers.bytes <= 0 {
		t.Fatalf("%d answers (%d bytes) kept, want %d", n, snap.answers.bytes, webAnswersMax)
	}
	// by size
	small := &webAnswers{}
	if small.add("a", make([]byte, webAnswersMaxBytes-10)) == nil || small.add("b", make([]byte, 11)) != nil || small.add("c", make([]byte, 10)) == nil ||
		small.get("a") == nil || small.get("b") != nil || small.add("a", []byte("x")) != small.get("a") {
		t.Fatal("size limit of the kept answers")
	}
	var none *webAnswers
	if none.get("a") != nil || none.add("a", []byte("x")) != nil {
		t.Fatal("a nil store keeps nothing")
	}
}

var benchWebQueries = []struct{ name, query string }{
	{"default", ""},
	{"sort_return", "sort=return"},
	{"sort_latest", "sort=latest"},
	{"q", "q=pe"},
	{"verdict", "verdict=clean"},
	{"q_verdict_peak", "q=pe&verdict=clean&sort=peak"},
	{"asc_30d_usd", "sort=return&dir=asc&horizon=30d"},
	{"q_no_match", "q=zzzzzz"},
	{"last_page", "page=240"},
}

// BenchmarkWebSnapshot: filter + sort + page on a snapshot of 12,000 rows
// ("page/…"), the whole request with its JSON ("http/…", "gzip/…"), a request
// answered 304, and building the snapshot. No database needed:
//
//	go test -run '^$' -bench WebSnapshot -benchmem ./telegrambot/scoutanalytics
func BenchmarkWebSnapshot(b *testing.B) {
	const n = 12000
	ws := benchWebServer(b, n)
	snap := ws.snap.Load()
	for _, c := range benchWebQueries {
		req := httptest.NewRequest("GET", "/api/calls?"+c.query, nil)
		f, err := parseWebCallsQuery(req.URL.Query())
		if err != nil {
			b.Fatal(err)
		}
		b.Run("page/"+c.name, func(b *testing.B) {
			b.ReportAllocs()
			var room [webMaxPer]int32
			for i := 0; i < b.N; i++ {
				if _, _, err := snap.page(f, room[:0]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	serve := func(b *testing.B, req *http.Request, want int) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			ws.ServeHTTP(rec, req)
			if rec.Code != want {
				b.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
		}
	}
	for _, c := range benchWebQueries {
		b.Run("http/"+c.name, func(b *testing.B) {
			serve(b, httptest.NewRequest("GET", "/api/calls?"+c.query, nil), 200)
		})
	}
	b.Run("gzip/default", func(b *testing.B) {
		req := httptest.NewRequest("GET", "/api/calls", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		serve(b, req, 200)
	})
	// the same question asked again of the same snapshot: the kept answer
	// (the cases above run without keeping answers, so they show the full work)
	kept := *snap
	kept.answers = &webAnswers{}
	ws.snap.Store(&kept)
	b.Run("kept/q_verdict_peak", func(b *testing.B) {
		serve(b, httptest.NewRequest("GET", "/api/calls?q=pe&verdict=clean&sort=peak", nil), 200)
	})
	b.Run("kept_gzip/default", func(b *testing.B) {
		req := httptest.NewRequest("GET", "/api/calls", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		serve(b, req, 200)
	})
	ws.snap.Store(snap)
	b.Run("http/summary", func(b *testing.B) { serve(b, httptest.NewRequest("GET", "/api/summary", nil), 200) })
	b.Run("http/not_modified", func(b *testing.B) {
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, httptest.NewRequest("GET", "/api/calls?q=pe&verdict=clean&sort=peak", nil))
		req := httptest.NewRequest("GET", "/api/calls?q=pe&verdict=clean&sort=peak", nil)
		req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
		serve(b, req, 304)
	})
	raw := syntheticWebRows(n, 42)
	b.Run("build", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			rows := cloneWebRows(raw)
			b.StartTimer()
			if s := mustWebSnapshot(b, rows, 600, nil); s.n != n {
				b.Fatal("rows")
			}
		}
	})
	b.Run("build_unchanged", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			rows := cloneWebRows(raw)
			b.StartTimer()
			if s := mustWebSnapshot(b, rows, 600, snap); s.version != snap.version {
				b.Fatal("version")
			}
		}
	})
}
