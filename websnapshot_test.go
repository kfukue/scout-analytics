package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	mrng := rand.New(rand.NewSource(seed + 2000)) // so do the market caps
	srng := rand.New(rand.NewSource(seed + 3000)) // and the sAlpha reports
	// a market cap from the post: mostly one of few values (many ties), now and
	// then missing, zero, negative or a number JSON cannot carry
	mcap := func() *float64 {
		var v float64
		switch mrng.Intn(14) {
		case 0, 1:
			return nil
		case 2:
			v = 0
		case 3:
			v = -45000
		case 4:
			v = math.NaN()
		case 5:
			v = math.Inf(1)
		default:
			v = 5000 * float64(1+mrng.Intn(40))
		}
		return &v
	}
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
		// market caps on every row (also the ones not tracked or not in USD,
		// which must not show them) and the price at the post on tracked ones
		r.CalledAtMcap, r.PostMcap = mcap(), mcap()
		if r.Tracked {
			p := 0.0001 * float64(1+mrng.Intn(20))
			switch mrng.Intn(12) {
			case 0:
				r.PostPrice = nil
			case 1:
				r.PostPrice = new(float64) // zero
			case 2:
				nan := math.NaN()
				r.PostPrice = &nan
			default:
				r.PostPrice = &p
			}
		}
		if rng.Intn(4) == 0 {
			r.PerceptorVerd = sp(verdicts[rng.Intn(len(verdicts))])
			r.PerceptorURL = sp(fmt.Sprintf("https://www.perceptor.info/r/%032x", i))
			if i%13 == 0 {
				r.PerceptorURL = sp("http://www.perceptor.info/r/plain")
			}
			id := 1000000 + i
			r.PerceptorID = &id
		}
		if srng.Intn(6) == 0 {
			id := 2000000 + i
			r.SAlphaID = &id
		}
	}
	return rows
}

// syntheticWebReports makes the report of every id rows refer to, the way
// ScoutStore.SelectWebReports returns them.
func syntheticWebReports(rows []ScoutWebRow) map[int]*ScoutWebReport {
	out := map[int]*ScoutWebReport{}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := range rows {
		r := &rows[i]
		if p := r.PerceptorID; p != nil {
			verdict := levelUnknown
			if r.PerceptorVerd != nil {
				verdict = *r.PerceptorVerd
			}
			out[*p] = &ScoutWebReport{ID: *p, Tool: webToolPerceptor, At: at.Add(time.Duration(i) * time.Minute), Verdict: verdict,
				Label: sp("No red flags found"), Summary: sp(fmt.Sprintf("Top 10 hold %d%%; LP locked", i%90)), URL: r.PerceptorURL}
		}
		if p := r.SAlphaID; p != nil {
			out[*p] = &ScoutWebReport{ID: *p, Tool: webToolSAlpha, At: at.Add(time.Duration(i) * time.Minute),
				Text: fmt.Sprintf("Smart money: %d wallets bought\nDev holds %d%%", i%17, i%9), URL: sp(fmt.Sprintf("https://salpha.example/r/%d", i))}
		}
	}
	return out
}

// refMcaps works out the two market caps of a raw row (as ScoutStore.SelectWebRows
// returns it) the obvious way: call = the first valid of called-at and Mcap;
// latest = (the first valid of Mcap and called-at) × latest price ÷ price at the
// post; valid = a finite number above zero. nil unless the row is priced in USD
// (and, for latest, has a latest price that is shown).
func refMcaps(r *ScoutWebRow) (call, latest *float64) {
	if r.PriceUnit == nil || *r.PriceUnit != "usd" {
		return nil, nil
	}
	ok := func(p *float64) bool { return p != nil && *p > 0 && !math.IsInf(*p, 0) } // NaN > 0 is false
	c := r.CalledAtMcap
	if !ok(c) {
		c = r.PostMcap
	}
	if ok(c) {
		v := *c
		call = &v
	}
	b := r.PostMcap
	if !ok(b) {
		b = r.CalledAtMcap
	}
	if r.LatestAt != nil && finite(r.LatestReturn) != nil && ok(r.LatestPrice) && ok(b) && ok(r.PostPrice) {
		if v := *b * *r.LatestPrice / *r.PostPrice; ok(&v) {
			latest = &v
		}
	}
	return call, latest
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
		if f.Verdict != "" && !slices.Contains(strings.Split(f.Verdict, ","), verdict) {
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
		if f.Sort == "call_mc" || f.Sort == "latest_mc" {
			call, latest := refMcaps(r)
			if f.Sort == "latest_mc" {
				call = latest
			}
			if call == nil {
				return 0, false
			}
			return *call, true
		}
		i := h * webPerfPerHorizon
		if f.Sort == "peak" {
			i++
		}
		if w, ok := map[string]int{"return_1h": 0, "return_1d": 1, "return_3d": 2, "return_7d": 3, "return_30d": 4}[f.Sort]; ok {
			i = w * webPerfPerHorizon // that window's return, whatever the horizon
		}
		if r.PriceUnit == nil || *r.PriceUnit != "usd" || r.HasPerf&(1<<i) == 0 {
			return 0, false
		}
		if f.Sort == "peak" && r.Rugged != nil && *r.Rugged {
			return 0, false // a rugged call shows no peak
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
	pos, total, err := s.page(f, webAllAges, nil)
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
		for _, sortBy := range []string{"date", "return", "peak", "latest", "call_mc", "latest_mc", "return_1h", "return_1d", "return_3d", "return_7d", "return_30d"} {
			for _, dir := range []string{"desc", "asc"} {
				for _, hz := range ScoutWebHorizons {
					for _, usdOnly := range []bool{false, true} {
						for _, verdict := range []string{"", "clean", "caution", "red_flags", "not_scanned",
							"clean,caution", "not_scanned,red_flags,caution", "red_flags,not_scanned"} {
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
		if checked != 11*2*5*2*8*7*6 {
			t.Fatalf("checked %d combinations", checked)
		}
		// the market caps sent are the ones worked out the obvious way
		withCall, withLatest := 0, 0
		for i := range raw {
			c := sentCall(t, snap, i, 1)
			wantCall, wantLatest := refMcaps(&raw[i])
			if fnum(c.CallMcapUSD) != fnum(wantCall) || fnum(c.LatestMcapUSD) != fnum(wantLatest) {
				t.Fatalf("n=%d row %d: call_mcap_usd %s latest_mcap_usd %s, want %s %s", n, i,
					fnum(c.CallMcapUSD), fnum(c.LatestMcapUSD), fnum(wantCall), fnum(wantLatest))
			}
			// … and so are the five window returns: each window's return, USD only
			for h, got := range []*float64{c.Return1hPct, c.Return1dPct, c.Return3dPct, c.Return7dPct, c.Return30dPct} {
				var want *float64
				j := h * webPerfPerHorizon
				if r := &raw[i]; r.PriceUnit != nil && *r.PriceUnit == "usd" && r.HasPerf&(1<<j) != 0 {
					want = finite(&r.Perf[j])
				}
				if fnum(got) != fnum(want) {
					t.Fatalf("n=%d row %d window %s: %s, want %s", n, i, ScoutWebHorizons[h], fnum(got), fnum(want))
				}
				// the same number return_pct has for that window
				if r := sentCall(t, snap, i, h); fnum(r.ReturnPct) != fnum(got) {
					t.Fatalf("n=%d row %d window %s: return_pct %s, window field %s", n, i, ScoutWebHorizons[h], fnum(r.ReturnPct), fnum(got))
				}
			}
			if wantCall != nil {
				withCall++
			}
			if wantLatest != nil {
				withLatest++
			}
		}
		if n == 600 && (withCall < 100 || withLatest < 50) {
			t.Fatalf("too few market caps to test: %d call, %d latest", withCall, withLatest)
		}
	}
	snap := mustWebSnapshot(t, syntheticWebRows(20, 1), 0, nil)
	for _, bad := range []ScoutWebCallsFilter{
		{Sort: "price", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "call_mcap", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "CALL_MC", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "latest_mcap_usd", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "latest_mc", Dir: "up", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "return_2d", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "return_", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "Return_1h", Dir: "desc", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "return_1h", Dir: "desc", Horizon: "2d", Page: 1, Per: 10},
		{Sort: "date", Dir: "up", Horizon: "1d", Page: 1, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "2d", Page: 1, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 0, Per: 10},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 0},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 10, Verdict: "unknown"},
		{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: 10, Q: "a\x00b"},
	} {
		if _, _, err := snap.page(bad, webAllAges, nil); err == nil {
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
	// the window returns: 1h has none, 1d is NaN (none), 3d none; the same in every window asked for
	for h := range ScoutWebHorizons {
		if w := sentCall(t, snap, 0, h).windows(); w != "null null null null null" {
			t.Fatalf("windows of the usd row at %s: %s", ScoutWebHorizons[h], w)
		}
	}
	if w := sentCall(t, snap, 1, 1).windows(); w != "null null null null null" { // 999 and 1999 are not in USD
		t.Fatalf("windows of the non-usd row: %s", w)
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

	// The market caps: call = called-at, else Mcap; latest = (Mcap, else
	// called-at) × latest price ÷ price at the post (not the late entry).
	f := func(v float64) *float64 { return &v }
	at := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	usd := sp("usd")
	cases := []struct {
		name                       string
		unit                       *string
		calledAt, mcap, post, late *float64
		ret, price                 *float64
		readAt                     *time.Time
		wantCall, wantLatest       string
	}{
		{"both (Mcap line first; at-post price, not the late entry)", usd, f(40000), f(50000), f(0.002), f(0.003), f(10), f(0.004), &at, "40000", "100000"},
		{"no called-at", usd, nil, f(50000), f(0.002), nil, f(10), f(0.001), &at, "50000", "25000"},
		{"no Mcap line", usd, f(40000), nil, f(0.002), nil, f(10), f(0.001), &at, "40000", "20000"},
		{"called-at 0: the Mcap line", usd, f(0), f(50000), f(0.002), nil, f(10), f(0.004), &at, "50000", "100000"},
		{"called-at negative: the Mcap line", usd, f(-1), f(50000), f(0.002), nil, f(10), f(0.004), &at, "50000", "100000"},
		{"called-at NaN, no Mcap line", usd, f(nan), nil, f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
		{"called-at +Inf, no Mcap line", usd, f(inf), nil, f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
		{"called-at NaN: the Mcap line", usd, f(nan), f(60000), f(0.002), nil, f(10), f(0.004), &at, "60000", "120000"},
		{"Mcap line 0: called-at for the estimate", usd, f(40000), f(0), f(0.002), nil, f(10), f(0.004), &at, "40000", "80000"},
		{"Mcap line -Inf: called-at for the estimate", usd, f(40000), f(math.Inf(-1)), f(0.002), nil, f(10), f(0.004), &at, "40000", "80000"},
		{"both invalid", usd, f(-5), f(0), f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
		{"no price at the post (late entry only)", usd, f(40000), f(50000), nil, f(0.003), f(10), f(0.004), &at, "40000", "null"},
		{"price at the post 0", usd, f(40000), f(50000), f(0), nil, f(10), f(0.004), &at, "40000", "null"},
		{"price at the post NaN", usd, f(40000), f(50000), f(nan), nil, f(10), f(0.004), &at, "40000", "null"},
		{"latest price 0", usd, f(40000), f(50000), f(0.002), nil, f(10), f(0), &at, "40000", "null"},
		{"latest price negative", usd, f(40000), f(50000), f(0.002), nil, f(10), f(-0.004), &at, "40000", "null"},
		{"latest price +Inf", usd, f(40000), f(50000), f(0.002), nil, f(10), f(inf), &at, "40000", "null"},
		{"no latest price", usd, f(40000), f(50000), f(0.002), nil, nil, nil, nil, "40000", "null"},
		{"latest price without the time it was read", usd, f(40000), f(50000), f(0.002), nil, f(10), f(0.004), nil, "40000", "null"},
		{"latest return NaN", usd, f(40000), f(50000), f(0.002), nil, f(nan), f(0.004), &at, "40000", "null"},
		{"too large", usd, f(1e300), nil, f(1e-300), nil, f(10), f(1e10), &at, "1e+300", "null"},
		{"not in USD", sp("VIRT"), f(40000), f(50000), f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
		{"no price unit", nil, f(40000), f(50000), f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
		{"nothing from the post", usd, nil, nil, f(0.002), nil, f(10), f(0.004), &at, "null", "null"},
	}
	var mrows []ScoutWebRow
	for i, c := range cases {
		entry := c.late // the late entry, else the price at the post
		if entry == nil {
			entry = c.post
		}
		mrows = append(mrows, ScoutWebRow{CallID: 100 + i, MessageID: 1, MessageDate: at.Add(-time.Hour), ChannelUsername: "scoutrobinhood",
			ContractAddress: caAlpha, PriceUnit: c.unit, EntryPrice: entry, Tracked: true, CallCount: 1,
			CalledAtMcap: c.calledAt, PostMcap: c.mcap, PostPrice: c.post, LatestReturn: c.ret, LatestPrice: c.price, LatestAt: c.readAt})
	}
	msnap := mustWebSnapshot(t, mrows, 0, nil)
	for i, c := range cases {
		got := sentCall(t, msnap, i, 1)
		if fnum(got.CallMcapUSD) != c.wantCall || fnum(got.LatestMcapUSD) != c.wantLatest {
			t.Errorf("%s: call_mcap_usd %s latest_mcap_usd %s, want %s %s", c.name, fnum(got.CallMcapUSD), fnum(got.LatestMcapUSD), c.wantCall, c.wantLatest)
		}
		// the reference of TestWebSnapshotPageMatchesReference agrees
		if a, b := refMcaps(&ScoutWebRow{PriceUnit: c.unit, CalledAtMcap: c.calledAt, PostMcap: c.mcap, PostPrice: c.post,
			LatestReturn: c.ret, LatestPrice: c.price, LatestAt: c.readAt}); fnum(a) != c.wantCall || fnum(b) != c.wantLatest {
			t.Errorf("%s: the reference gives %s %s", c.name, fnum(a), fnum(b))
		}
	}
	// by value (ties: the higher call id first), the rows without one last in both directions
	for q, want := range map[[2]string]string{
		{"call_mc", "desc"}:   "[120 107 104 103 101 119 118 117 116 115 114 113 112 111 109 108 102 100 123 122 121 110 106 105]",
		{"call_mc", "asc"}:    "[100 102 108 109 111 112 113 114 115 116 117 118 119 101 103 104 107 120 105 106 110 121 122 123]",
		{"latest_mc", "desc"}: "[107 104 103 100 109 108 101 102 123 122 121 120 119 118 117 116 115 114 113 112 111 110 106 105]",
		{"latest_mc", "asc"}:  "[102 101 108 109 100 103 104 107 105 106 110 111 112 113 114 115 116 117 118 119 120 121 122 123]",
	} {
		ids, total := pageIDs(t, msnap, ScoutWebCallsFilter{Sort: q[0], Dir: q[1], Horizon: "1d", Page: 1, Per: 50})
		if fmt.Sprint(ids) != want || total != len(cases) {
			t.Errorf("sort=%s dir=%s: %v (total %d), want %s", q[0], q[1], ids, total, want)
		}
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
	change("a 1h return", 5, func(rows []ScoutWebRow) { rows[4].Perf[0], rows[4].HasPerf = rows[4].Perf[0]+0.25, rows[4].HasPerf|1 })
	seven := -1 // a USD row with a 7d return
	for i := range raw {
		if r := &raw[i]; r.PriceUnit != nil && *r.PriceUnit == "usd" && r.HasPerf&(1<<(3*webPerfPerHorizon)) != 0 && !math.IsNaN(r.Perf[3*webPerfPerHorizon]) {
			seven = i
			break
		}
	}
	if seven < 0 {
		t.Fatal("no row with a 7d return")
	}
	change("a 7d return removed", 5, func(rows []ScoutWebRow) { rows[seven].HasPerf &^= 1 << (3 * webPerfPerHorizon) })
	change("unit", 5, func(rows []ScoutWebRow) { rows[4].PriceUnit = sp("eth") })
	change("verdict", 5, func(rows []ScoutWebRow) { rows[5].PerceptorVerd = sp("clean!") })
	change("report", 5, func(rows []ScoutWebRow) { rows[5].PerceptorURL = sp("https://example.org/r") })
	change("sAlpha report", 5, func(rows []ScoutWebRow) { id := 7777777; rows[6].SAlphaID = &id })
	change("Perceptor report id", 5, func(rows []ScoutWebRow) { id := 7777778; rows[6].PerceptorID = &id })
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
	// the market caps: a USD row with both, from both figures of the post
	m := -1
	for i := range raw {
		r := &raw[i]
		if call, latest := refMcaps(r); call != nil && latest != nil && positive(r.CalledAtMcap) != nil && positive(r.PostMcap) != nil {
			m = i
			break
		}
	}
	if m < 0 {
		t.Fatal("no row with both market caps to change")
	}
	scaled := func(p *float64, k float64) *float64 { v := *p * k; return &v }
	change("called-at market cap", 5, func(rows []ScoutWebRow) { rows[m].CalledAtMcap = scaled(rows[m].CalledAtMcap, 2) })
	change("Mcap line", 5, func(rows []ScoutWebRow) { rows[m].PostMcap = scaled(rows[m].PostMcap, 3) })
	change("price at the post", 5, func(rows []ScoutWebRow) { rows[m].PostPrice = scaled(rows[m].PostPrice, 4) })
	change("no market caps", 5, func(rows []ScoutWebRow) { rows[m].CalledAtMcap, rows[m].PostMcap = nil, nil })
	change("called-at only", 5, func(rows []ScoutWebRow) { rows[m].PostMcap = nil })
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
	// … nor do market caps of calls not priced in USD, nor a price at the post
	// that no latest market cap uses
	other = cloneWebRows(raw)
	mcChanged, postChanged := 0, 0
	for i := range other {
		r := &other[i]
		if r.PriceUnit == nil || *r.PriceUnit != "usd" {
			v, w := 123456.0, 654321.0
			r.CalledAtMcap, r.PostMcap = &v, &w
			mcChanged++
			continue
		}
		if _, latest := refMcaps(r); latest == nil && r.PostPrice != nil {
			v := 0.5
			r.PostPrice = &v
			if _, latest := refMcaps(r); latest == nil {
				postChanged++
			} else {
				r.PostPrice = raw[i].PostPrice
			}
		}
	}
	if c := mustWebSnapshot(t, other, 5, a); mcChanged == 0 || postChanged == 0 || c.version != a.version {
		t.Fatalf("market caps outside USD (%d rows), unused prices at the post (%d rows): version %q, want %q", mcChanged, postChanged, c.version, a.version)
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
	rows := syntheticWebRows(n, 42)
	snap := mustWebSnapshot(tb, rows, 600, nil)
	snap.loadedAt = time.Now().UTC()
	snap.reports = webReportsFor(rows, nil, syntheticWebReports(rows))
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
	{"sort_call_mc", "sort=call_mc"},
	{"sort_latest_mc", "sort=latest_mc"},
	{"sort_return_1h", "sort=return_1h"},
	{"sort_return_7d", "sort=return_7d"},
	{"sort_return_30d_asc", "sort=return_30d&dir=asc&horizon=7d"},
	{"q", "q=pe"},
	{"verdict", "verdict=clean"},
	{"q_verdict_peak", "q=pe&verdict=clean&sort=peak"},
	{"verdicts", "verdict=clean,caution"},
	{"q_verdicts_peak", "q=pe&verdict=caution,clean&sort=peak"},
	{"asc_30d_usd", "sort=return&dir=asc&horizon=30d"},
	{"q_no_match", "q=zzzzzz"},
	{"last_page", "page=240"},
}

// benchWebDaysQueries: questions asked with the age filter (days=…).
var benchWebDaysQueries = []struct{ name, query string }{
	{"default", ""},
	{"date_asc", "dir=asc"},
	{"sort_latest", "sort=latest"},
	{"verdicts", "verdict=clean,caution"},
	{"q_verdict_peak", "q=pe&verdict=clean&sort=peak"},
	{"last_page", "page=120"},
}

// BenchmarkWebSnapshot: filter + sort + page on a snapshot of 12,000 rows
// ("page/…"), the whole request with its JSON ("http/…", "gzip/…"), a request
// answered 304, and building the snapshot. No database needed:
//
//	go test -run '^$' -bench WebSnapshot -benchmem .
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
				if _, _, err := snap.page(f.ScoutWebCallsFilter, snap.ageCutoff(f.Days), room[:0]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	// the age filter, with a cutoff in the middle of the made-up dates (they
	// are months old, so a real days=7 would match nothing and measure nothing)
	mid := snap.dates[snap.order[webSortKeyDate][n/2]]
	for _, c := range benchWebDaysQueries {
		f, err := parseWebCallsQuery(httptest.NewRequest("GET", "/api/calls?"+c.query, nil).URL.Query())
		if err != nil {
			b.Fatal(err)
		}
		b.Run("page_days/"+c.name, func(b *testing.B) {
			b.ReportAllocs()
			var room [webMaxPer]int32
			for i := 0; i < b.N; i++ {
				if _, _, err := snap.page(f.ScoutWebCallsFilter, mid, room[:0]); err != nil {
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
	// the whole request with the age filter: the snapshot read one day after
	// the middle of the made-up dates, so days=1 keeps about half the rows
	aged := *snap
	aged.loadedAt = time.Unix(0, mid).Add(24 * time.Hour).UTC()
	ws.snap.Store(&aged)
	for _, c := range benchWebDaysQueries {
		b.Run("http_days/"+c.name, func(b *testing.B) {
			serve(b, httptest.NewRequest("GET", "/api/calls?days=1&"+c.query, nil), 200)
		})
	}
	ws.snap.Store(snap)
	b.Run("http/summary", func(b *testing.B) { serve(b, httptest.NewRequest("GET", "/api/summary", nil), 200) })
	b.Run("http/not_modified", func(b *testing.B) {
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, httptest.NewRequest("GET", "/api/calls?q=pe&verdict=clean&sort=peak", nil))
		req := httptest.NewRequest("GET", "/api/calls?q=pe&verdict=clean&sort=peak", nil)
		req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
		serve(b, req, 304)
	})
	// the row detail (GET /api/call) of a token with both reports
	detail := 0
	for i := 0; i < snap.n && detail == 0; i++ {
		if snap.percID[i] != 0 && snap.salphaID[i] != 0 {
			detail = int(snap.ids[i])
		}
	}
	if detail == 0 {
		b.Fatal("no row with both reports")
	}
	detailPath := fmt.Sprintf("/api/call?id=%d", detail)
	b.Run("http/call", func(b *testing.B) { serve(b, httptest.NewRequest("GET", detailPath, nil), 200) })
	b.Run("http/call_not_modified", func(b *testing.B) {
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, httptest.NewRequest("GET", detailPath, nil))
		req := httptest.NewRequest("GET", detailPath, nil)
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

// getWeb asks ws for path with header pairs.
func getWeb(ws *webServer, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, req)
	return rec
}

// webCallDetailJSON is the body of GET /api/call as the page reads it.
type webCallDetailJSON struct {
	CallID    int `json:"call_id"`
	Perceptor *struct {
		ID        int     `json:"id"`
		Verdict   string  `json:"verdict"`
		Label     *string `json:"label"`
		Summary   *string `json:"summary"`
		URL       *string `json:"url"`
		At        string  `json:"at"`
		Truncated bool    `json:"truncated"`
	} `json:"perceptor"`
	SAlpha *struct {
		ID        int     `json:"id"`
		Text      string  `json:"text"`
		Declined  bool    `json:"declined"`
		URL       *string `json:"url"`
		At        string  `json:"at"`
		Truncated bool    `json:"truncated"`
	} `json:"salpha"`
}

func decodeCallDetail(tb testing.TB, body []byte) webCallDetailJSON {
	tb.Helper()
	var d webCallDetailJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		tb.Fatalf("decode %s: %v", body, err)
	}
	return d
}

// TestWebReportFlagAndVersion: the row carries whether the token has an sAlpha
// report with text, and the ids of the two reports; a new sAlpha report
// changes the version (and so the ETag of /api/calls), while a reply without
// text, which the read never picks (see TestWebDetailSelection), changes
// nothing and costs no read of the texts.
func TestWebReportFlagAndVersion(t *testing.T) {
	ws, db := fakeWebServer(t, 200)
	snap := ws.snap.Load()
	// a row without an sAlpha report, and one with
	without, with := -1, -1
	for i := range db.rows {
		if db.rows[i].SAlphaID == nil && without < 0 {
			without = i
		}
		if db.rows[i].SAlphaID != nil && with < 0 {
			with = i
		}
	}
	if without < 0 || with < 0 {
		t.Fatal("the synthetic rows need tokens with and without an sAlpha report")
	}
	if c := sentCall(t, snap, with, 1); !c.HasSAlpha || c.SAlphaReportID == nil || *c.SAlphaReportID != *db.rows[with].SAlphaID {
		t.Fatalf("row with a report: %+v", c)
	}
	if c := sentCall(t, snap, without, 1); c.HasSAlpha || c.SAlphaReportID != nil {
		t.Fatalf("row without a report: %+v", c)
	}
	tag := getWeb(ws, "/api/calls?per=200").Header().Get("ETag")

	// an empty reply arrives: the read picks the same report as before (none)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s2 := ws.snap.Load(); s2.version != snap.version || getWeb(ws, "/api/calls?per=200").Header().Get("ETag") != tag {
		t.Fatal("no new report with text: the version must stay")
	}
	if a := db.takeAsked(); len(a) != 0 {
		t.Fatalf("nothing new, yet the texts were read: %v", a)
	}

	// a report with text arrives for the token that had none
	rows := cloneWebRows(db.rows)
	id := 3000000
	rows[without].SAlphaID = &id
	db.reports[id] = &ScoutWebReport{ID: id, Tool: webToolSAlpha, At: time.Now(), Text: "fresh"}
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s3 := ws.snap.Load()
	if s3.version == snap.version || getWeb(ws, "/api/calls?per=200").Header().Get("ETag") == tag {
		t.Fatal("a new sAlpha report must change the version and the ETag")
	}
	if c := sentCall(t, s3, without, 1); !c.HasSAlpha || c.SAlphaReportID == nil || *c.SAlphaReportID != id {
		t.Fatalf("row after the new report: %+v", c)
	}
	if a := db.takeAsked(); len(a) != 1 || !slices.Equal(a[0], []int{id}) {
		t.Fatalf("texts read: %v, want only [%d]", a, id)
	}
	// a newer Perceptor report too
	rows = cloneWebRows(rows)
	pid := 3000001
	rows[without].PerceptorID = &pid
	db.reports[pid] = &ScoutWebReport{ID: pid, Tool: webToolPerceptor, At: time.Now(), Verdict: levelCaution}
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ws.snap.Load().version == s3.version {
		t.Fatal("a new Perceptor report must change the version")
	}
}

// TestWebReportTexts: the text map holds exactly the reports the rows refer
// to; a refresh reads only the ids it does not hold yet (none when nothing is
// new), drops the ones no row refers to any more, and leaves the map of the
// snapshot before as it was.
func TestWebReportTexts(t *testing.T) {
	ws, db := fakeWebServer(t, 300)
	referenced := func(rows []ScoutWebRow) map[int]bool {
		m := map[int]bool{}
		for i := range rows {
			for _, p := range [2]*int{rows[i].PerceptorID, rows[i].SAlphaID} {
				if p != nil {
					m[*p] = true
				}
			}
		}
		return m
	}
	first := ws.snap.Load()
	want := referenced(db.rows)
	if len(want) < 50 || len(first.reports) != len(want) {
		t.Fatalf("%d texts kept, %d referenced", len(first.reports), len(want))
	}
	for id := range want {
		if first.reports[id] == nil {
			t.Fatalf("report %d missing", id)
		}
	}

	// the first read asks for every id, once
	ws2, db2 := benchWebServer(t, 1), &fakeWebDB{rows: syntheticWebRows(300, 5)}
	db2.reports = syntheticWebReports(db2.rows)
	ws2.readRows, ws2.readReports = db2.read, db2.readReports
	ws2.snap.Store(nil)
	if err := ws2.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := db2.takeAsked(); len(a) != 1 || len(a[0]) != len(want) {
		t.Fatalf("first read asked %d times, want once for %d ids", len(a), len(want))
	}

	// unchanged: no read of the texts, the same entries
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := db.takeAsked(); len(a) != 0 {
		t.Fatalf("unchanged rows, yet texts read: %v", a)
	}
	second := ws.snap.Load()
	for id, e := range first.reports {
		if second.reports[id] != e {
			t.Fatalf("report %d was made again", id)
		}
	}
	// a text changed in the database is not read again (an investigation is
	// written once, so this cannot happen; it shows that nothing is re-read)
	for id := range want {
		db.reports[id] = &ScoutWebReport{ID: id, Tool: db.reports[id].Tool, Text: "changed", Label: sp("changed")}
	}
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := db.takeAsked(); len(a) != 0 {
		t.Fatalf("texts read again: %v", a)
	}

	// one report gone, one new: only the new id is read, the old one is dropped
	rows := cloneWebRows(db.rows)
	gone, added := -1, -1
	for i := range rows {
		if rows[i].SAlphaID != nil && gone < 0 {
			gone = i
		} else if rows[i].SAlphaID == nil && rows[i].PerceptorID == nil && added < 0 {
			added = i
		}
	}
	goneID := *rows[gone].SAlphaID
	rows[gone].SAlphaID = nil
	newID := 4000000
	rows[added].SAlphaID = &newID
	db.reports[newID] = &ScoutWebReport{ID: newID, Tool: webToolSAlpha, Text: "new one"}
	// and an id the database does not return (deleted in between): the row
	// loses it for now, and it is asked for again next time
	lostID := 4000001
	rows[added].PerceptorID = &lostID
	db.set(rows, nil)
	before := ws.snap.Load()
	beforeLen := len(before.reports)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := db.takeAsked()
	if len(a) == 1 {
		slices.Sort(a[0])
	}
	if len(a) != 1 || !slices.Equal(a[0], []int{newID, lostID}) {
		t.Fatalf("texts read: %v, want [%d %d]", a, newID, lostID)
	}
	now := ws.snap.Load()
	if now.reports[goneID] != nil || now.reports[newID] == nil || now.reports[newID].text != "new one" || now.reports[lostID] != nil {
		t.Fatal("text map after one report gone and one new")
	}
	want = referenced(rows)
	delete(want, lostID)
	if len(now.reports) != len(want) {
		t.Fatalf("%d texts kept, want %d", len(now.reports), len(want))
	}
	if len(before.reports) != beforeLen || before.reports[goneID] == nil || before.reports[newID] != nil {
		t.Fatal("the map of the snapshot before was changed")
	}
	if c := sentCall(t, now, added, 1); c.PerceptorReportID != nil || !c.HasSAlpha {
		t.Fatalf("row with a report that could not be read: %+v", c)
	}
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := db.takeAsked(); len(a) != 1 || !slices.Equal(a[0], []int{lostID}) {
		t.Fatalf("the missing report must be asked for again: %v", a)
	}

	// the texts cannot be read: the refresh fails and the snapshot stays
	rows = cloneWebRows(rows)
	newer := 4000002
	rows[gone].SAlphaID = &newer
	db.set(rows, nil)
	db.mu.Lock()
	db.reportsErr = errors.New("down")
	db.mu.Unlock()
	kept := ws.snap.Load()
	if err := ws.refresh(context.Background()); err == nil || ws.snap.Load() != kept {
		t.Fatalf("a failed read of the texts: %v", err)
	}
}

// TestWebReportCut: texts are kept up to 32 KB, cut at a character boundary
// and marked; links only when they are https and not absurdly long.
func TestWebReportCut(t *testing.T) {
	exact := strings.Repeat("a", webReportMaxBytes)
	if r := newWebReport(&ScoutWebReport{Tool: webToolSAlpha, Text: exact}); r.truncated || r.text != exact {
		t.Fatal("a text of exactly the limit must be kept whole")
	}
	// a 3-byte character across the limit
	long := strings.Repeat("a", webReportMaxBytes-1) + "€" + strings.Repeat("b", 100)
	r := newWebReport(&ScoutWebReport{Tool: webToolSAlpha, Text: long, URL: sp("http://salpha.example/x")})
	if !r.truncated || len(r.text) != webReportMaxBytes-1 || !utf8.ValidString(r.text) || r.url != nil {
		t.Fatalf("cut: truncated %v, %d bytes, valid %v, url %v", r.truncated, len(r.text), utf8.ValidString(r.text), r.url)
	}
	p := newWebReport(&ScoutWebReport{Tool: webToolPerceptor, Verdict: levelRedFlags, Summary: sp(strings.Repeat("é", webReportMaxBytes)),
		Label: sp("Red flags"), Text: "not shown", URL: sp("https://www.perceptor.info/r/" + strings.Repeat("x", webReportMaxURL))})
	if !p.truncated || len(*p.summary) != webReportMaxBytes || p.text != "" || p.url != nil || p.verdict != levelRedFlags {
		t.Fatalf("perceptor cut: %v %d %q %v %q", p.truncated, len(*p.summary), p.text, p.url, p.verdict)
	}
	if p := newWebReport(&ScoutWebReport{Tool: webToolPerceptor, Verdict: "not_scanned", URL: sp("https://www.perceptor.info/r/1")}); p.verdict != levelUnknown ||
		p.url == nil || p.truncated {
		t.Fatalf("perceptor: %+v", p)
	}
}

// TestWebCallEndpoint: GET /api/call answers from memory with the reports of
// a listed call's token: 400 for a bad id, 404 for one that is not a row, 503
// before the first snapshot; its own ETag (which follows the reports only),
// 304, Cache-Control: no-cache and gzip for large answers.
func TestWebCallEndpoint(t *testing.T) {
	cold := benchWebServer(t, 1)
	cold.snap.Store(nil)
	if rec := getWeb(cold, "/api/call?id=10"); rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("before the first snapshot: %d", rec.Code)
	}
	if rec := getWeb(cold, "/api/call?id=x"); rec.Code != 400 {
		t.Fatalf("bad id before the first snapshot: %d", rec.Code)
	}

	ws, db := fakeWebServer(t, 120)
	for _, q := range []string{"", "?", "?id=", "?id=abc", "?id=0", "?id=-4", "?id=010", "?id=+10", "?id=10.0", "?id=1e3", "?id=%2010",
		"?id=2147483648", "?id=99999999999999999999", "?id=10&id=10", "?id=10&x=1", "?ID=10", "?id=10;", "?%zz"} {
		rec := getWeb(ws, "/api/call"+q)
		var e map[string]string
		if rec.Code != 400 || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e["error"] == "" || rec.Header().Get("ETag") != "" {
			t.Errorf("%q: %d %s", q, rec.Code, rec.Body)
		}
	}
	// ids of the synthetic rows are 10, 12, 14, …: 11 is no row
	for _, q := range []string{"?id=11", "?id=1", "?id=2147483647"} {
		if rec := getWeb(ws, "/api/call"+q); rec.Code != 404 || !strings.Contains(rec.Body.String(), "refresh") || rec.Header().Get("ETag") != "" {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, httptest.NewRequest("POST", "/api/call?id=10", nil))
	if rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}

	// a row with both reports and one with none
	both, none := -1, -1
	for i := range db.rows {
		r := &db.rows[i]
		if r.PerceptorID != nil && r.SAlphaID != nil && r.PerceptorURL != nil && both < 0 {
			both = i
		}
		if r.PerceptorID == nil && r.SAlphaID == nil && none < 0 {
			none = i
		}
	}
	if both < 0 || none < 0 {
		t.Fatal("the synthetic rows need tokens with both reports and with none")
	}
	r := db.rows[both]
	rec = getWeb(ws, fmt.Sprintf("/api/call?id=%d", r.CallID))
	h := rec.Header()
	if rec.Code != 200 || h.Get("Cache-Control") != "no-cache" || !strings.HasPrefix(h.Get("ETag"), `W/"`) || h.Get("X-Snapshot-At") == "" ||
		!strings.HasPrefix(h.Get("Content-Type"), "application/json") || h.Get("Content-Security-Policy") == "" || h.Get("Content-Encoding") != "" {
		t.Fatalf("detail: %d %v", rec.Code, h)
	}
	d := decodeCallDetail(t, rec.Body.Bytes())
	src := db.reports[*r.SAlphaID]
	if d.CallID != r.CallID || d.Perceptor == nil || d.Perceptor.ID != *r.PerceptorID || d.Perceptor.Verdict != *r.PerceptorVerd ||
		d.Perceptor.Summary == nil || *d.Perceptor.Summary != *db.reports[*r.PerceptorID].Summary ||
		d.SAlpha == nil || d.SAlpha.ID != *r.SAlphaID || d.SAlpha.Text != src.Text || d.SAlpha.URL == nil || *d.SAlpha.URL != *src.URL ||
		d.SAlpha.At != src.At.UTC().Format(time.RFC3339) || d.SAlpha.Truncated {
		t.Fatalf("detail of %d: %s", r.CallID, rec.Body)
	}
	if (d.Perceptor.URL == nil) != !strings.HasPrefix(*r.PerceptorURL, "https://") {
		t.Fatalf("perceptor link %v for %q", d.Perceptor.URL, *r.PerceptorURL)
	}
	etag := h.Get("ETag")
	if rec := getWeb(ws, fmt.Sprintf("/api/call?id=%d", r.CallID), "If-None-Match", etag); rec.Code != 304 || rec.Body.Len() != 0 ||
		rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	rec = getWeb(ws, fmt.Sprintf("/api/call?id=%d", db.rows[none].CallID))
	if d := decodeCallDetail(t, rec.Body.Bytes()); rec.Code != 200 || d.Perceptor != nil || d.SAlpha != nil ||
		!strings.Contains(rec.Body.String(), `"perceptor":null`) || !strings.Contains(rec.Body.String(), `"salpha":null`) {
		t.Fatalf("token without reports: %d %s", rec.Code, rec.Body)
	}

	// another row changes: the detail stays "not modified"
	rows := cloneWebRows(db.rows)
	rows[none].TokenName = sp("renamed")
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := getWeb(ws, fmt.Sprintf("/api/call?id=%d", r.CallID), "If-None-Match", etag); rec.Code != 304 {
		t.Fatalf("an unrelated change: %d", rec.Code)
	}
	// a new, long sAlpha report: another ETag, and gzip for clients that take it
	rows = cloneWebRows(rows)
	big := 5000000
	rows[both].SAlphaID = &big
	text := strings.Repeat("<b>wallet</b> 0xabc bought \"a lot\"\n", 200)
	db.reports[big] = &ScoutWebReport{ID: big, Tool: webToolSAlpha, At: time.Now(), Text: text}
	db.set(rows, nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec = getWeb(ws, fmt.Sprintf("/api/call?id=%d", r.CallID), "If-None-Match", etag)
	if rec.Code != 200 || rec.Header().Get("ETag") == etag {
		t.Fatalf("new report: %d, ETag %s", rec.Code, rec.Header().Get("ETag"))
	}
	if d := decodeCallDetail(t, rec.Body.Bytes()); d.SAlpha == nil || d.SAlpha.Text != text || d.SAlpha.URL != nil {
		t.Fatalf("long report: %+v", d.SAlpha)
	}
	plain := rec.Body.Bytes()
	zipped := getWeb(ws, fmt.Sprintf("/api/call?id=%d", r.CallID), "Accept-Encoding", "gzip")
	if zipped.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(zipped.Header().Get("Vary"), "Accept-Encoding") || zipped.Body.Len() >= len(plain) {
		t.Fatalf("gzip: %v", zipped.Header())
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := io.ReadAll(zr); !bytes.Equal(out, plain) {
		t.Fatal("gzip body differs")
	}
	if small := getWeb(ws, fmt.Sprintf("/api/call?id=%d", db.rows[none].CallID), "Accept-Encoding", "gzip"); small.Code != 200 || small.Header().Get("Content-Encoding") != "" {
		t.Fatal("a small answer was compressed")
	}
	// a call that is no row any more (asked by a page of an older snapshot): 404
	db.set(rows[1:], nil)
	if err := ws.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := getWeb(ws, fmt.Sprintf("/api/call?id=%d", rows[0].CallID)); rec.Code != 404 {
		t.Fatalf("a row that has gone: %d", rec.Code)
	}
}

// TestParseWebVerdicts: the verdict parameter of /api/calls as a list
// separated by commas, repeated, or both; written back in one order.
func TestParseWebVerdicts(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string // the filter's Verdict; "!" = an error
	}{
		{"", ""},
		{"verdict=", ""},
		{"verdict=,", ""},
		{"verdict=clean", "clean"},
		{"verdict=not_scanned", "not_scanned"},
		{"verdict=clean,caution", "clean,caution"},
		{"verdict=caution,clean", "clean,caution"},
		{"verdict=caution&verdict=clean", "clean,caution"},
		{"verdict=clean,clean&verdict=clean", "clean"},
		{"verdict=,clean,,red_flags,", "clean,red_flags"},
		{"verdict=not_scanned,red_flags,caution", "caution,red_flags,not_scanned"},
		{"verdict=not_scanned,red_flags,caution,clean", ""}, // every bucket = all
		{"verdict=clean&verdict=caution&verdict=red_flags&verdict=not_scanned", ""},
		{"verdict=bad", "!"},
		{"verdict=clean,bad", "!"},
		{"verdict=clean&verdict=bad", "!"},
		{"verdict=CLEAN", "!"},
		{"verdict=clean,%20caution", "!"},
		{"verdict=all", "!"},
		{"verdict=unknown", "!"},
		{"sort=date&sort=date", "!"}, // only verdict may be repeated
		{"q=a&q=b", "!"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			v, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parseWebCallsQuery(v)
			if tc.want == "!" {
				if err == nil {
					t.Fatalf("parseWebCallsQuery(%q) = %q, want an error", tc.query, f.Verdict)
				}
				return
			}
			if err != nil || f.Verdict != tc.want {
				t.Fatalf("parseWebCallsQuery(%q) = %q, %v; want %q", tc.query, f.Verdict, err, tc.want)
			}
		})
	}
}

// TestWebVerdictSetETag: the same set of verdicts, however written, gets the
// same ETag and the same answer; another set gets another ETag. The answer
// echoes the set.
func TestWebVerdictSetETag(t *testing.T) {
	ws := benchWebServer(t, 300)
	get := func(q string) (string, webCallsJSON) {
		t.Helper()
		rec := getWeb(ws, "/api/calls?"+q)
		if rec.Code != 200 {
			t.Fatalf("?%s: %d %s", q, rec.Code, rec.Body)
		}
		var res webCallsJSON
		dec := json.NewDecoder(rec.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&res); err != nil {
			t.Fatalf("?%s: %v", q, err)
		}
		return rec.Header().Get("ETag"), res
	}
	same := []string{"verdict=clean,caution", "verdict=caution,clean", "verdict=caution&verdict=clean", "verdict=clean,caution,clean,"}
	tag0, res0 := get(same[0])
	if res0.Verdict != "clean,caution" || !reflect.DeepEqual(res0.Verdicts, []string{"clean", "caution"}) || res0.Total == 0 {
		t.Fatalf("?%s: verdict %q %q, total %d; want \"clean,caution\" [clean caution], some rows", same[0], res0.Verdict, res0.Verdicts, res0.Total)
	}
	for _, q := range same[1:] {
		if tag, res := get(q); tag != tag0 || !reflect.DeepEqual(res, res0) {
			t.Errorf("?%s: ETag %s, want %s (as ?%s), same answer %v", q, tag, tag0, same[0], reflect.DeepEqual(res, res0))
		}
	}
	tagClean, resClean := get("verdict=clean")
	tagCaution, resCaution := get("verdict=caution")
	if tagClean == tag0 || tagCaution == tag0 {
		t.Errorf("one verdict has the ETag of two: %s %s %s", tagClean, tagCaution, tag0)
	}
	if resClean.Total+resCaution.Total != res0.Total {
		t.Errorf("total clean %d + caution %d, want clean,caution %d", resClean.Total, resCaution.Total, res0.Total)
	}
	if resClean.Verdict != "clean" || !reflect.DeepEqual(resClean.Verdicts, []string{"clean"}) {
		t.Errorf("verdict=clean echoed as %q %q", resClean.Verdict, resClean.Verdicts)
	}
	tagAll, resAll := get("")
	tagEvery, resEvery := get("verdict=red_flags,not_scanned,caution,clean")
	if tagAll != tagEvery || resEvery.Verdict != "" || !reflect.DeepEqual(resEvery.Verdicts, []string{}) || !reflect.DeepEqual(resAll, resEvery) {
		t.Errorf("every verdict: ETag %s (all: %s), verdict %q %q", tagEvery, tagAll, resEvery.Verdict, resEvery.Verdicts)
	}
}

// recentRows returns the rows posted at since or later (all for webAllAges).
func recentRows(rows []ScoutWebRow, since int64) []ScoutWebRow {
	var out []ScoutWebRow
	for _, r := range rows {
		if since == webAllAges || r.MessageDate.UnixNano() >= since {
			out = append(out, r)
		}
	}
	return out
}

// TestWebSnapshotAgeFilterMatchesReference: the age filter ("calls from the
// last N days") lets through exactly the rows posted at the cutoff or later,
// combined with every sort, direction, search, Perceptor filter and page, with
// the right total.
func TestWebSnapshotAgeFilterMatchesReference(t *testing.T) {
	for _, n := range []int{0, 1, 7, 600} {
		raw := syntheticWebRows(n, int64(n)+7)
		snap := mustWebSnapshot(t, cloneWebRows(raw), 0, nil)
		// cutoffs: none, before every row, after every row, exactly on the
		// date of a row (several rows share it), and just after it
		cutoffs := []int64{webAllAges, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()}
		if n > 0 {
			mid := raw[n/2].MessageDate.UnixNano()
			cutoffs = append(cutoffs, mid, mid+1, raw[0].MessageDate.UnixNano())
		}
		for _, since := range cutoffs {
			sub := recentRows(raw, since)
			if got := snap.countSince(since); got != len(sub) {
				t.Fatalf("n=%d countSince(%d) = %d, want %d", n, since, got, len(sub))
			}
			for _, sortBy := range []string{"date", "return", "latest", "call_mc", "return_7d"} {
				for _, dir := range []string{"desc", "asc"} {
					for _, usdOnly := range []bool{false, true} {
						for _, verdict := range []string{"", "clean", "caution,not_scanned"} {
							for _, q := range []string{"", "pe", "nothing matches"} {
								for _, pg := range [][2]int{{1, 50}, {2, 50}, {3, 7}, {1, 200}, {1000000, 200}, {1, 1}} {
									f := ScoutWebCallsFilter{Q: q, Sort: sortBy, Dir: dir, Horizon: "7d", USDOnly: usdOnly, Verdict: verdict, Page: pg[0], Per: pg[1]}
									pos, total, err := snap.page(f, since, nil)
									if err != nil {
										t.Fatalf("n=%d since=%d %+v: %v", n, since, f, err)
									}
									got := []int{}
									for _, p := range pos {
										got = append(got, int(snap.ids[p]))
									}
									want, wantTotal := referencePage(sub, f)
									if total != wantTotal || fmt.Sprint(got) != fmt.Sprint(want) {
										t.Fatalf("n=%d since=%d %+v:\n got %d %v\nwant %d %v", n, since, f, total, got, wantTotal, want)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// ageFilterServer is a website over 300 made-up rows whose snapshot was read
// at loadedAt.
func ageFilterServer(t *testing.T, loadedAt time.Time) (*webServer, []ScoutWebRow) {
	t.Helper()
	ws := benchWebServer(t, 300)
	raw := syntheticWebRows(300, 42) // the rows of benchWebServer
	ws.snap.Load().loadedAt = loadedAt
	return ws, raw
}

func decodeCalls(t *testing.T, rec *httptest.ResponseRecorder) webCallsJSON {
	t.Helper()
	var res webCallsJSON
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}

// TestWebCallsDaysParam: days=N of /api/calls keeps the calls posted within N
// days of the snapshot's time, in whole days from 1 to webMaxDays; anything
// else is refused. It works with the search, the Perceptor filter and every
// sort, the total counts only what it lets through, and the answer says which
// filter it used.
func TestWebCallsDaysParam(t *testing.T) {
	// the made-up rows are posted over about 2 days from 2026-01-01: the
	// snapshot is read 3 days after the first post
	at := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	ws, raw := ageFilterServer(t, at)
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"days=1", 200}, {"days=7", 200}, {"days=30", 200}, {"days=3650", 200},
		{"days=0", 400}, {"days=-1", 400}, {"days=3651", 400}, {"days=7d", 400}, {"days=07", 400},
		{"days=", 400}, {"days=1.5", 400}, {"days=all", 400}, {"days=7&days=7", 400}, {"days=%207", 400},
	} {
		if rec := getWeb(ws, "/api/calls?"+tc.query); rec.Code != tc.code {
			t.Errorf("GET /api/calls?%s: %d, want %d (%s)", tc.query, rec.Code, tc.code, rec.Body)
		}
	}
	for _, tc := range []string{
		"days=1",
		"days=1&dir=asc",
		"days=2&sort=return&horizon=7d",
		"days=2&sort=latest&dir=asc",
		"days=2&verdict=clean,caution",
		"days=2&verdict=clean&q=pe&sort=peak",
		"days=1&q=PE&sort=return_1h&per=200",
		"days=2&page=2&per=20",
		"days=30",
	} {
		t.Run(tc, func(t *testing.T) {
			v, err := url.ParseQuery(tc)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parseWebCallsQuery(v)
			if err != nil {
				t.Fatalf("parseWebCallsQuery(%q): %v", tc, err)
			}
			rec := getWeb(ws, "/api/calls?"+tc)
			if rec.Code != 200 {
				t.Fatalf("GET /api/calls?%s: %d %s", tc, rec.Code, rec.Body)
			}
			res := decodeCalls(t, rec)
			if res.Days != f.Days || f.Days == 0 {
				t.Errorf("?%s: days %d in the answer, want %d", tc, res.Days, f.Days)
			}
			since := at.Add(-time.Duration(f.Days) * 24 * time.Hour)
			sub := recentRows(raw, since.UnixNano())
			want, wantTotal := referencePage(sub, f.ScoutWebCallsFilter)
			got := []int{}
			for _, c := range res.Calls {
				got = append(got, c.CallID)
				d, err := time.Parse(time.RFC3339Nano, c.MessageDate)
				if err != nil || d.Before(since) {
					t.Errorf("?%s: call %d posted %s, before the cutoff %s", tc, c.CallID, c.MessageDate, since)
				}
			}
			if res.Total != wantTotal || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("?%s:\n got %d %v\nwant %d %v", tc, res.Total, got, wantTotal, want)
			}
			// the filter must matter for this test to mean anything (days=30 keeps every row)
			if _, allTotal := referencePage(raw, f.ScoutWebCallsFilter); f.Days < 30 && wantTotal == allTotal {
				t.Errorf("?%s: total %d with and without the age filter; pick another snapshot time", tc, wantTotal)
			}
		})
	}
	// without days the answer says 0 and has every row
	if res := decodeCalls(t, getWeb(ws, "/api/calls")); res.Days != 0 || res.Total != 300 {
		t.Errorf("no days: days %d, total %d; want 0, 300", res.Days, res.Total)
	}
}

// TestWebCallsDaysETag: with the age filter, the ETag changes when a later
// snapshot time pushes a row out of the window (even though the rows, and so
// the snapshot's version, are the same: the tracker may be stopped), and stays
// when the rows let through stay the same. Without the filter the time does
// not count.
func TestWebCallsDaysETag(t *testing.T) {
	at := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	ws, raw := ageFilterServer(t, at)
	snap := ws.snap.Load()
	tag := func(path string) string {
		t.Helper()
		rec := getWeb(ws, path)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		return rec.Header().Get("ETag")
	}
	tag1, tagAll := tag("/api/calls?days=2"), tag("/api/calls")
	// the oldest row the window lets through
	since := at.Add(-48 * time.Hour)
	var oldest time.Time
	for _, r := range recentRows(raw, since.UnixNano()) {
		if oldest.IsZero() || r.MessageDate.Before(oldest) {
			oldest = r.MessageDate
		}
	}
	if oldest.IsZero() || !oldest.After(since) {
		t.Fatalf("no row strictly inside the window (oldest %s, cutoff %s)", oldest, since)
	}
	// a little later, still before the oldest row drops out
	snap.loadedAt = at.Add(oldest.Sub(since) / 2)
	if got := tag("/api/calls?days=2"); got != tag1 {
		t.Errorf("same rows let through: ETag %s, want %s", got, tag1)
	}
	// later: the oldest row is out
	snap.loadedAt = at.Add(oldest.Sub(since) + time.Nanosecond)
	if got := tag("/api/calls?days=2"); got == tag1 {
		t.Errorf("a row left the window but the ETag stayed %s", got)
	}
	if got := tag("/api/calls"); got != tagAll {
		t.Errorf("no age filter: ETag %s changed with the snapshot time (was %s)", got, tagAll)
	}
	// a client holding the old ETag gets the new list, not 304
	if rec := getWeb(ws, "/api/calls?days=2", "If-None-Match", tag1); rec.Code != 200 {
		t.Errorf("old ETag after a row left the window: %d, want 200", rec.Code)
	}
}
