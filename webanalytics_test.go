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
	"net/http/httptest"
	"os/exec"
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
	RugKinds       []string     `json:"rug_kinds"`
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
		withFactors(r, rng)
	}
	return rows
}

func f64p(v float64) *float64 { return &v }

// withFactors fills the values known at the call of a synthetic row: nil
// (no post metrics, no hour before the call), zeros, ordinary and huge
// values, and volumes in a quote asset rather than USD.
func withFactors(r *ScoutWebRow, rng *rand.Rand) {
	ip := func(max int) *int {
		n := 0
		if rng.Intn(4) != 0 {
			n = rng.Intn(max)
		}
		return &n
	}
	fp := func(max float64) *float64 {
		v := 0.0
		if rng.Intn(4) != 0 {
			v = math.Exp(rng.Float64()*math.Log(max)) - 1
		}
		return &v
	}
	if rng.Intn(6) != 0 { // post metrics parsed
		if rng.Intn(5) != 0 {
			r.CalledAtMcap = fp(5e7)
		}
		if rng.Intn(9) == 0 {
			v := []float64{0, -5, math.NaN(), math.Inf(1)}[rng.Intn(4)]
			r.CalledAtMcap = &v // not a market cap: sent as null
		}
		r.Holders, r.ProofElite, r.ProofGood = ip(5000), ip(40), ip(80)
		if rng.Intn(10) == 0 {
			r.Holders = nil
		}
		r.LiveBuysEliteCount, r.LiveBuysGoodCount = ip(12), ip(20)
		r.LiveBuysEliteUSD, r.LiveBuysGoodUSD = fp(3e4), fp(1e5)
	}
	if rng.Intn(5) != 0 { // the token supply read from the chain
		r.TokenSupply = f64p(math.Exp(rng.Float64() * math.Log(1e12)))
		if rng.Intn(9) == 0 {
			v := []float64{0, -5, math.NaN(), math.Inf(1), 1e30}[rng.Intn(5)]
			r.TokenSupply = &v // no market cap: sent as null (1e30: above webAnaMaxMcap)
		}
	}
	if rng.Intn(3) != 0 { // the hour before the call measured
		r.PreVolUnit = sp("usd")
		if rng.Intn(5) == 0 {
			r.PreVolUnit = sp("VIRT")
		}
		r.PreBuyVol60, r.PreSellVol60, r.PreSwaps60 = fp(2e6), fp(2e6), ip(900)
		if rng.Intn(5) != 0 {
			v := rng.NormFloat64() * 80
			r.PreChg60 = &v
		}
	}
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
		checkAnaFactors(t, a, raw)
		checkAnaTiming(t, a, raw)
		if !slices.Equal(a.RugKinds, []string{"event", "at_call", "detected", "other"}) {
			t.Errorf("n=%d: rug kinds %v", n, a.RugKinds)
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

// checkAnaFactors: the values known at the call, as each row was read:
// counts as they are, dollars rounded (3 significant digits from $1,000), the
// market cap only when positive, volumes only when measured in USD, the
// price change to 0.1; null for what is missing.
func checkAnaFactors(t *testing.T, a webAnalyticsJSON, raw []ScoutWebRow) {
	t.Helper()
	col := map[string]int{}
	for i, c := range a.Columns {
		col[c] = i
	}
	usdNum := func(p *float64, positive bool) *float64 {
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || (positive && *p == 0) {
			return nil
		}
		return p
	}
	intNum := func(p *int) *float64 {
		if p == nil {
			return nil
		}
		v := float64(*p)
		return &v
	}
	for i, row := range a.Rows {
		r := &raw[i]
		usdVol := r.PreVolUnit != nil && *r.PreVolUnit == "usd"
		// the price-based market cap (price at the post × token supply), of
		// calls priced in USD only; never the posted figure
		var mcap *float64
		if p, s := usdNum(r.PostPrice, true), usdNum(r.TokenSupply, true); r.PriceUnit != nil && *r.PriceUnit == "usd" && p != nil && s != nil {
			if v := *p * *s; v <= 1e13 && !math.IsInf(v, 0) {
				mcap = &v
			}
		}
		vol := func(p *float64) *float64 {
			if !usdVol {
				return nil
			}
			return usdNum(p, false)
		}
		for _, c := range []struct {
			name string
			want *float64
			tol  float64 // relative
		}{
			{"mcap", mcap, 0.005},
			{"holders", intNum(r.Holders), 0},
			{"proof_elite", intNum(r.ProofElite), 0},
			{"proof_good", intNum(r.ProofGood), 0},
			{"buys_elite_n", intNum(r.LiveBuysEliteCount), 0},
			{"buys_good_n", intNum(r.LiveBuysGoodCount), 0},
			{"buys_elite_usd", usdNum(r.LiveBuysEliteUSD, false), 0.005},
			{"buys_good_usd", usdNum(r.LiveBuysGoodUSD, false), 0.005},
			{"pre_buy_usd", vol(r.PreBuyVol60), 0.005},
			{"pre_sell_usd", vol(r.PreSellVol60), 0.005},
			{"pre_swaps", intNum(r.PreSwaps60), 0},
			{"pre_chg", r.PreChg60, 0},
		} {
			got := row[col[c.name]]
			if c.want == nil {
				if got != nil {
					t.Errorf("call %d: %s = %v, want null", r.CallID, c.name, *got)
				}
				continue
			}
			w := *c.want
			if got == nil || math.Abs(*got-w) > math.Max(0.5+1e-9, w*c.tol) {
				t.Errorf("call %d: %s = %v, want %v", r.CallID, c.name, got, w)
			}
		}
	}
}

// checkAnaTiming: the timing columns of every row as it was read: per window
// the four values in seconds only when the window has its timing (else
// null), the timed and censored bits, the rug time and the index of its kind.
func checkAnaTiming(t *testing.T, a webAnalyticsJSON, raw []ScoutWebRow) {
	t.Helper()
	col := map[string]int{}
	for i, c := range a.Columns {
		col[c] = i
	}
	sawTimed, sawNull, sawCens, sawRug := false, false, false, false
	for i, row := range a.Rows {
		r := &raw[i]
		for h, hz := range ScoutWebHorizons {
			for k, name := range []string{"tpeak_", "t2x_", "held_", "tfall_"} {
				j := h*webTimingPerHorizon + k
				got := row[col[name+hz]]
				if r.Timed&(1<<h) == 0 || r.HasTiming&(1<<j) == 0 {
					if got != nil {
						t.Errorf("call %d: %s%s = %v, want null (timed %b, has %b)", r.CallID, name, hz, *got, r.Timed, r.HasTiming)
					}
					sawNull = sawNull || r.Timed&(1<<h) != 0
					continue
				}
				sawTimed = true
				if got == nil || *got != float64(r.Timing[j]) {
					t.Errorf("call %d: %s%s = %v, want %d", r.CallID, name, hz, dumpFactors(got), r.Timing[j])
				}
			}
		}
		num := func(c string) *float64 { return row[col[c]] }
		if got := num("timed"); got == nil || int(*got) != int(r.Timed) {
			t.Errorf("call %d: timed %v, want %d", r.CallID, dumpFactors(got), r.Timed)
		}
		if got := num("cens_2x"); got == nil || int(*got) != int(r.Censored2x&r.Timed) {
			t.Errorf("call %d: cens_2x %v, want %d", r.CallID, dumpFactors(got), r.Censored2x&r.Timed)
		}
		sawCens = sawCens || r.Censored2x != 0
		got := num("rug_s")
		switch {
		case r.RugAfterS == nil && got != nil:
			t.Errorf("call %d: rug_s %v, want null", r.CallID, *got)
		case r.RugAfterS != nil && (got == nil || *got != float64(*r.RugAfterS)):
			t.Errorf("call %d: rug_s %v, want %d", r.CallID, dumpFactors(got), *r.RugAfterS)
		}
		wantKind := "-"
		if r.RugAtKind != nil || r.RugAfterS != nil {
			wantKind = "other"
			if r.RugAtKind != nil && slices.Contains([]string{"event", "at_call", "detected"}, *r.RugAtKind) {
				wantKind = *r.RugAtKind
			}
			sawRug = true
		}
		gotKind := "-"
		if k := num("rug_kind"); k == nil {
			t.Errorf("call %d: rug_kind null", r.CallID)
		} else if *k >= 0 {
			gotKind = a.RugKinds[int(*k)]
		}
		if gotKind != wantKind {
			t.Errorf("call %d: rug kind %q, want %q (rug_at_kind %v)", r.CallID, gotKind, wantKind, dumpFactors(r.RugAtKind))
		}
	}
	if len(a.Rows) >= 600 && !(sawTimed && sawNull && sawCens && sawRug) {
		t.Errorf("test data lacks a case: timed %v, null in a timed window %v, censored %v, rug %v", sawTimed, sawNull, sawCens, sawRug)
	}
}

// TestAppendAnaTiming: the timing columns of single rows, as encoded.
func TestAppendAnaTiming(t *testing.T) {
	ip := func(v int) *int { return &v }
	nulls := strings.Repeat(",null", len(ScoutWebHorizons)*webTimingPerHorizon)
	for _, c := range []struct {
		name string
		row  ScoutWebRow
		want string
	}{
		{"nothing", ScoutWebRow{}, nulls + ",0,0,null,-1"},
		{"values without timing_at are not sent", ScoutWebRow{Timing: [20]int32{5, 6, 7, 8}, HasTiming: 0xf, Censored2x: 1}, nulls + ",0,0,null,-1"},
		{"1d timed, never reached 2x", func() ScoutWebRow {
			r := ScoutWebRow{Timed: 2, HasTiming: 0x50}
			r.Timing[4], r.Timing[6] = 3600, 0
			return r
		}(), ",null,null,null,null,3600,null,0,null" + strings.Repeat(",null", 12) + ",2,0,null,-1"},
		{"1h censored, event rug", func() ScoutWebRow {
			r := ScoutWebRow{Timed: 1, HasTiming: 0x7, Censored2x: 1 | 4, RugAfterS: ip(90000), RugAtKind: sp(rugAtEvent)}
			r.Timing[0], r.Timing[1], r.Timing[2] = 900, 600, 2700
			return r
		}(), ",900,600,2700,null" + strings.Repeat(",null", 16) + ",1,1,90000,0"},
		{"rug at the call", ScoutWebRow{RugAfterS: ip(-120), RugAtKind: sp(rugAtAtCall)}, nulls + ",0,0,-120,1"},
		{"detected", ScoutWebRow{RugAfterS: ip(2600000), RugAtKind: sp(rugAtDetected)}, nulls + ",0,0,2600000,2"},
		{"unknown kind", ScoutWebRow{RugAfterS: ip(5), RugAtKind: sp("new")}, nulls + ",0,0,5,3"},
		{"time without kind", ScoutWebRow{RugAfterS: ip(5)}, nulls + ",0,0,5,3"},
		{"kind without time", ScoutWebRow{RugAtKind: sp(rugAtEvent)}, nulls + ",0,0,null,0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(appendAnaTiming(nil, &c.row)); got != c.want {
				t.Errorf("appendAnaTiming(%+v)\n got %s\nwant %s", c.row, got, c.want)
			}
		})
	}
}

func TestAppendAnaUSD(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		in       *float64
		positive bool
		want     string
	}{
		{nil, false, "null"}, {f(0), false, "0"}, {f(0), true, "null"}, {f(-1), false, "null"},
		{f(math.NaN()), false, "null"}, {f(math.Inf(1)), false, "null"},
		{f(0.4), false, "0"}, {f(12.6), false, "13"}, {f(999.4), false, "999"}, {f(999.6), false, "1000"},
		{f(12345), false, "12300"}, {f(45678.9), true, "45700"}, {f(1234567), false, "1230000"},
		{f(2.5e18), false, "2.5e+18"},
	} {
		if got := string(appendAnaUSD(nil, c.in, c.positive)); got != c.want {
			t.Errorf("appendAnaUSD(%v, aboveZero %v) = %q, want %q", c.in, c.positive, got, c.want)
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
	// mcapIn: the row (as read, not prepared) has a price-based market cap
	// well inside the range sent
	mcapIn := func(r *ScoutWebRow) bool {
		c := *r
		c.usd = usd(r)
		m := webAnaMcap(&c)
		return m != nil && *m > 1000 && *m < 1e12
	}
	// noSupply: a USD-priced row with a price at the post but no supply yet
	noSupply := func(r *ScoutWebRow) bool {
		return usd(r) && positive(r.PostPrice) != nil && *r.PostPrice < 1e3 && r.TokenSupply == nil
	}
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
		{"token supply", mcapIn, func(r *ScoutWebRow) { r.TokenSupply = f64p(*r.TokenSupply * 3) }},
		{"price at the post (market cap at call)", mcapIn, func(r *ScoutWebRow) { r.PostPrice = f64p(*r.PostPrice * 3) }},
		{"supply read", noSupply, func(r *ScoutWebRow) { r.TokenSupply = f64p(1e6) }},
		{"holders", func(r *ScoutWebRow) bool { return r.Holders != nil }, func(r *ScoutWebRow) { r.Holders = ip(*r.Holders + 1) }},
		{"elite holders", func(r *ScoutWebRow) bool { return r.ProofElite != nil }, func(r *ScoutWebRow) { r.ProofElite = ip(*r.ProofElite + 1) }},
		{"good holders", func(r *ScoutWebRow) bool { return r.ProofGood != nil }, func(r *ScoutWebRow) { r.ProofGood = ip(*r.ProofGood + 1) }},
		{"elite buyers", func(r *ScoutWebRow) bool { return r.LiveBuysEliteCount != nil },
			func(r *ScoutWebRow) { r.LiveBuysEliteCount = ip(*r.LiveBuysEliteCount + 1) }},
		{"good buyers", func(r *ScoutWebRow) bool { return r.LiveBuysGoodCount != nil },
			func(r *ScoutWebRow) { r.LiveBuysGoodCount = ip(*r.LiveBuysGoodCount + 1) }},
		{"elite buy usd", func(r *ScoutWebRow) bool { return r.LiveBuysEliteUSD != nil },
			func(r *ScoutWebRow) { r.LiveBuysEliteUSD = f64p(*r.LiveBuysEliteUSD*2 + 5000) }},
		{"good buy usd", func(r *ScoutWebRow) bool { return r.LiveBuysGoodUSD != nil },
			func(r *ScoutWebRow) { r.LiveBuysGoodUSD = f64p(*r.LiveBuysGoodUSD*2 + 5000) }},
		{"pre buy volume", func(r *ScoutWebRow) bool { return r.PreVolUnit != nil && *r.PreVolUnit == "usd" },
			func(r *ScoutWebRow) { r.PreBuyVol60 = f64p(*r.PreBuyVol60*2 + 5000) }},
		{"pre sell volume", func(r *ScoutWebRow) bool { return r.PreVolUnit != nil && *r.PreVolUnit == "usd" },
			func(r *ScoutWebRow) { r.PreSellVol60 = f64p(*r.PreSellVol60*2 + 5000) }},
		{"volume unit", func(r *ScoutWebRow) bool { return r.PreVolUnit != nil && *r.PreVolUnit == "usd" && *r.PreBuyVol60 > 0 },
			func(r *ScoutWebRow) { r.PreVolUnit = sp("WETH") }},
		{"pre swaps", func(r *ScoutWebRow) bool { return r.PreSwaps60 != nil }, func(r *ScoutWebRow) { r.PreSwaps60 = ip(*r.PreSwaps60 + 1) }},
		{"pre price change", func(r *ScoutWebRow) bool { return r.PreChg60 != nil }, func(r *ScoutWebRow) { r.PreChg60 = f64p(*r.PreChg60 + 3) }},
		{"rugged call's peak", func(r *ScoutWebRow) bool {
			return usd(r) && r.Rugged != nil && *r.Rugged && r.HasPerf&peakBit != 0 && !math.IsNaN(r.Perf[1*webPerfPerHorizon+webPerfPeak])
		}, func(r *ScoutWebRow) { r.Perf[1*webPerfPerHorizon+webPerfPeak] += 1000 }},
		// the timing after the call (the Timing tab)
		{"timing computed", func(r *ScoutWebRow) bool { return r.Timed&1 == 0 && r.HasPerf&1 != 0 }, func(r *ScoutWebRow) {
			r.Timed |= 1
			r.Timing[0], r.HasTiming = 300, r.HasTiming|1
		}},
		{"time to peak", func(r *ScoutWebRow) bool { return r.Timed&1 != 0 && r.HasTiming&1 != 0 },
			func(r *ScoutWebRow) { r.Timing[0] += 300 }},
		{"first 2x", func(r *ScoutWebRow) bool { return r.Timed&2 != 0 && r.HasTiming&(1<<5) == 0 }, func(r *ScoutWebRow) {
			r.Timing[5], r.HasTiming = 600, r.HasTiming|1<<5
		}},
		{"held at 2x", func(r *ScoutWebRow) bool { return r.Timed&2 != 0 && r.HasTiming&(1<<6) != 0 },
			func(r *ScoutWebRow) { r.Timing[6] += 3600 }},
		{"fall below 2x", func(r *ScoutWebRow) bool { return r.Timed&2 != 0 && r.HasTiming&(1<<7) != 0 },
			func(r *ScoutWebRow) { r.Timing[7] += 300 }},
		{"censored", func(r *ScoutWebRow) bool { return r.Timed&2 != 0 && r.Censored2x&2 == 0 },
			func(r *ScoutWebRow) { r.Censored2x |= 2 }},
		{"rug time", func(r *ScoutWebRow) bool { return r.RugAfterS != nil }, func(r *ScoutWebRow) { r.RugAfterS = ip(*r.RugAfterS + 1) }},
		{"rug kind", func(r *ScoutWebRow) bool { return r.RugAtKind != nil && *r.RugAtKind == rugAtEvent },
			func(r *ScoutWebRow) { r.RugAtKind = sp(rugAtDetected) }},
		{"rug stored", func(r *ScoutWebRow) bool { return r.RugAfterS == nil }, func(r *ScoutWebRow) {
			r.RugAfterS, r.RugAtKind = ip(7200), sp(rugAtEvent)
		}},
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
	if etag := h.Get("ETag"); !strings.HasSuffix(etag, `-a4"`) {
		t.Errorf("analytics ETag %s, want the format suffix -a4\"", etag)
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
	html, index := read("analytics.html"), read("index.html")
	for _, banned := range []string{"<script>", "<style", " style=", "onclick=", "onload=", "http://", "https://", "//cdn"} {
		if strings.Contains(html, banned) {
			t.Errorf("analytics.html contains %q", banned)
		}
	}
	js := read("analytics.js")
	for _, name := range []string{"analytics.js", "analytics-stats.js"} {
		// the SVG namespace is a name, not a link
		src := strings.ReplaceAll(read(name), "'http://www.w3.org/2000/svg'", "")
		for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
			"setAttribute('style'", "'style':", ".style.", "DOMParser", "http://", "https://"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q", name, banned)
			}
		}
	}
	// the calculations load first (deferred scripts run in order)
	if i, j := strings.Index(html, `<script src="analytics-stats.js" defer></script>`), strings.Index(html, `<script src="analytics.js" defer></script>`); i < 0 || j < 0 || i > j {
		t.Error("analytics.html must load analytics-stats.js (deferred) before analytics.js")
	}
	for _, want := range []string{`<script src="analytics.js" defer></script>`, `href="./"`,
		"First calls only", "late entry", "in USD", "before tax", "rounded to 0.1", `role="tablist"`,
		`id="panel-overview"`, `id="panel-factor"`, `id="panel-peak"`, "capped at +1,000%",
		// the exit-rule table on Peak vs final: fillExitTable writes into
		// #h-exit and #tbl-exit (a missing one would throw and blank the tab)
		`id="h-exit"`, `id="tbl-exit"`, `id="cap-exit"`, `id="cap-exit-out"`,
		"Sold at the first touch of 2×/3×/5× (a candle or trade high, before tax), else held to the end of the window.",
		"An upper bound: in thin pools a brief wick can count as a touch.",
		"Rugged calls count with their peak before the rug: a touch before the rug counts as a sale.",
		"Not the Timing tab's 'reached 2×' (held 1 h).",
		// the data health tables on Overview: renderHealth writes into
		// these ids (a missing one would throw and blank the rest of the
		// tab), right after the By week table; the threshold in the caption
		// is filled from the body's quiet_below
		`id="t-health"`, `id="tbl-health"`, `id="t-health-fam"`, `id="tbl-health-fam"`, `id="cap-health"`, `id="lg-health-quiet"`,
		"All calls, page filters not applied.",
		"Verdict = a Perceptor verdict at the time of the call, readable or not (history imported from before live handling has none).",
		`Quiet = fewer than <span id="lg-health-quiet">50</span> trades in the first 24 h after the call and not flagged rugged, among calls whose trade count is known (on-chain pools only); Uniswap v2 counts Sync events, so its quiet share reads low.`,
		"Calls under a day old have no trade count yet, so the newest period's 'known' share reads low.",
		"Rugged = flagged rugged by the tracker (as of now).", "Grey: a share of fewer than 20 calls."} {
		if !strings.Contains(html, want) {
			t.Errorf("analytics.html: missing %q", want)
		}
	}
	if i, j, k := strings.Index(html, `id="tbl-period"`), strings.Index(html, `id="t-health"`), strings.Index(html, `id="t-matrix"`); i < 0 || j < i || k < j {
		t.Error("analytics.html: the data health tables must come right after the By week table")
	}
	// the data health section uses every row (no page filter), and the
	// caption's "20" is MIN_N; the quiet, rugged and verdict rules and the
	// caption's threshold fill are covered by TestAnalyticsHealthNode
	for _, want := range []string{"renderHealth(data.rows);", "var MIN_N = 20;"} {
		if !strings.Contains(js, want) {
			t.Errorf("analytics.js: missing %q (data health by week)", want)
		}
	}
	if webQuietTrades != 50 {
		t.Errorf("webQuietTrades = %d, want 50 (the data health caption's placeholder)", webQuietTrades)
	}
	// Model insights (GET /api/model-live): the section is hidden until the
	// endpoint answers (a website without scores answers 404 and it stays
	// hidden); renderLive and applyLive write into these ids (a missing one
	// would throw inside render and blank the page). What it draws is covered
	// by TestAnalyticsLiveNode.
	if strings.Contains(html, "coming soon") {
		t.Error("analytics.html: the Model insights heading still says coming soon")
	}
	for _, want := range []string{
		`<section class="panel" id="model-insights" aria-labelledby="model-title" hidden>`,
		`<h2 id="model-title">Model insights</h2>`, `<p id="ml-error" class="muted small note" hidden></p>`,
		`<div id="ml-body" hidden>`, `id="ml-cover"`, `id="t-ml"`, `id="tbl-ml"`, `id="cap-ml"`,
		"Scores the model gave at the time of each call (only scores made within 24 h of the call).",
		"Top 10% = scored above 90% of the calls the model was tested on when it was trained (so the share can drift from 10%).",
		"Runner rates and returns are before tax (training labels are after tax).",
		"Page filters apply.", "Few calls are scored so far; treat small numbers as noise.",
		"Grey: fewer than 20 calls."} {
		if !strings.Contains(html, want) {
			t.Errorf("analytics.html: missing %q (model insights)", want)
		}
	}
	// The fetch (ETag, 304, 404, errors), the runner rules, the page
	// filters and the hooks into load() and render() are checked by
	// behaviour in testdata/analytics-live.test.js (TestAnalyticsLiveNode),
	// not by the source text here.
	if !strings.Contains(index, `href="analytics.html"`) {
		t.Error("index.html does not link to the Analytics page")
	}
	if !strings.Contains(js, "'api/analytics'") || !strings.Contains(js, "If-None-Match") {
		t.Error("analytics.js does not read /api/analytics with its ETag")
	}
	// The page understands the body's format: 4 since the timing columns
	// (3: "mcap" became the price-based market cap).
	if webAnalyticsFormat != 4 {
		t.Errorf("webAnalyticsFormat = %d, want 4", webAnalyticsFormat)
	}
	if want := fmt.Sprintf("var FORMAT = %d;", webAnalyticsFormat); !strings.Contains(js, want) {
		t.Errorf("analytics.js: missing %q (the page must understand the server's format)", want)
	}
	if want := "label: 'Market cap at call (price × supply, fully diluted)'"; !strings.Contains(js, want) {
		t.Errorf("analytics.js: missing %q", want)
	}
	if !strings.Contains(html, "price at the post × the token's total supply read from the chain") {
		t.Error("analytics.html: the By factor help does not explain the market cap at the call")
	}
	// the Timing tab: its panel, the "reached 2×" rule (owner decision, 12
	// October 2026: at or above 2× for at least 1 hour in total, in every
	// window) and the real time held of touched calls (owner decision, 13
	// October 2026) said on the page, and the vendored chart library loaded
	// (deferred) before the page's scripts, with its licence and notice, and
	// nothing in it that the CSP would need 'unsafe-eval' or 'unsafe-inline'
	// for
	for _, want := range []string{`id="panel-timing"`, `id="tab-timing"`,
		"<strong>Every call that touched 2× counts the time it really held</strong>",
		"a call that never touched 2× counts as 0 held",
		`Reached 2× = at or above 2× for at least <span id="lg-2x-hold">1 hour</span> in total within the window</strong> (the time held at 2×, below), in every window: a call still at 2× when the window ended needs it too.`} {
		if !strings.Contains(html, want) {
			t.Errorf("analytics.html: missing %q", want)
		}
	}
	// the rule's threshold lives in one constant and the rule in one
	// function, and the tab decides "reached 2×" through the function that
	// applies it
	stats := read("analytics-stats.js")
	for _, want := range []string{"var MIN_HELD_2X_S = 3600;", "function reached2x(t2x, held)", "held >= MIN_HELD_2X_S;"} {
		if !strings.Contains(stats, want) {
			t.Errorf("analytics-stats.js: missing %q (reached 2× = at least 1 hour at 2× in total)", want)
		}
	}
	for _, want := range []string{"A.timing2x(", "'Reached 2× = at or above 2× on closes for at least '", "' in total within the window '",
		"'(in every window; a call still at 2× at the window\\'s end needs it too).'"} {
		if !strings.Contains(js, want) {
			t.Errorf("analytics.js: missing %q", want)
		}
	}
	// a window too short for any call to reach the rule (1h; owner decision,
	// 12 October 2026) shows "n/a — needs 1 hour held" instead
	// of the reached-2× figures: the guard is defined once, from the first
	// candle's earliest close, and the tab checks it once per render and
	// passes it to every part that hangs on "reached 2×"
	for _, want := range []string{"var FIRST_2X_CLOSE_MIN_S = 1;", "function can2xReach(windowSeconds)",
		"return windowSeconds - FIRST_2X_CLOSE_MIN_S >= MIN_HELD_2X_S;", "function na2x()", "short: 'n/a', needs: needs, text: 'n/a — ' + needs,",
		"var needs = 'needs ' + hold + ' held';", "counted from the end of the first 2× candle"} {
		if !strings.Contains(stats, want) {
			t.Errorf("analytics-stats.js: missing %q (n/a where no call can reach 2× by the rule)", want)
		}
	}
	for _, want := range []string{"var na = !A.can2xReach(H);", "tmCounts(all, rows.length, withData, notTimed, hz, na);",
		"drawTmDist(all, hz, na);", "fillTmGroups(all, gr, hz, na);", "drawTmKM(all, gr, H, hz, na);",
		"stat(dl, 'Reached 2×', x.text,", "stat(dl, 'Still at 2× at the end', x.text,",
		"if (na && (d.key === 't2x' || d.key === 'fall'))", "naChart($('ch-tm-km'));",
		"tr.appendChild(na ? naCell() : rateCell(a.reached, a.n, 'timed calls reached 2×'));"} {
		if !strings.Contains(js, want) {
			t.Errorf("analytics.js: missing %q (n/a where no call can reach 2× by the rule)", want)
		}
	}
	// the earlier rule scaled to the window (a call still at 2× at the end
	// counted with less than 1 hour) is gone from the page
	for name, src := range map[string]string{"analytics.html": html, "analytics.js": js, "analytics-stats.js": stats} {
		if strings.Contains(src, "the window was left after the first 2×") {
			t.Errorf("%s: still describes the rule scaled to the window", name)
		}
	}
	// the time held figures use the real held of every call that touched 2×
	// (owner decision, 13 October 2026): timing2x passes the first 2× touch,
	// not only a reached one, and the histogram has its three kinds of bars
	// from heldHistogram; the earlier "did not reach 2× = 0 held" wording is
	// gone
	if want := "held: heldValue(t2x, held),"; !strings.Contains(stats, want) {
		t.Errorf("analytics-stats.js: missing %q (real time held for touched calls)", want)
	}
	for _, want := range []string{"function heldHistogram(helds, short, reached)", "heldHistogram: heldHistogram,"} {
		if !strings.Contains(stats, want) {
			t.Errorf("analytics-stats.js: missing %q", want)
		}
	}
	for _, want := range []string{"A.heldHistogram(all.helds, all.heldsShort, all.heldsReached)",
		"{ name: '0 held', counts: hh.zero, sw: 'sw-never', color: cssVar('--muted') }",
		"{ name: 'Touched 2×, held over 0 but under ' + hold, counts: hh.short, sw: 'c1', color: cssVar('--c1') }",
		"{ name: 'Reached 2× (' + hold + ' or more)', counts: hh.reached, sw: 'sw-acc', color: cssVar('--accent') }",
		"key.id = 'key-tm-held';"} {
		if !strings.Contains(js, want) {
			t.Errorf("analytics.js: missing %q (the time held histogram's three kinds of bars and key)", want)
		}
	}
	// the counts stat (all.short, includes held 0) and the histogram's amber
	// series (held over 0 only) must not share a name
	if got := strings.Count(js, "'Touched 2×, under ' + hold"); got != 1 {
		t.Errorf("analytics.js: %q appears %d times, want 1 (the counts stat only; the held histogram has its own name)", "'Touched 2×, under ' + hold", got)
	}
	// the key under the time held chart greys out with its bars when n is
	// under MIN_N
	if css, want := read("style.css"), ".ana-multiple.is-few .ana-key .sw { --c: var(--muted); }"; !strings.Contains(css, want) {
		t.Errorf("style.css: missing %q (the held chart's key in the few state)", want)
	}
	for name, src := range map[string]string{"analytics.html": html, "analytics.js": js, "analytics-stats.js": stats} {
		for _, gone := range []string{"Calls that did not reach 2× count as 0 held", "did not reach 2× = 0", "counted as 0)",
			"count as 0 held (grey part of the 0 bar)", "heldValue(rt, held)"} {
			if strings.Contains(src, gone) {
				t.Errorf("%s: still has %q (touched calls now count their real time held)", name, gone)
			}
		}
	}
	const echartsTag = `<script src="vendor/echarts-6.1.0.custom.min.js" defer></script>`
	if i, j := strings.Index(html, echartsTag), strings.Index(html, `<script src="analytics.js" defer></script>`); i < 0 || i > j {
		t.Errorf("analytics.html must load %s before analytics.js", echartsTag)
	}
	vendor := read("vendor/echarts-6.1.0.custom.min.js")
	for _, banned := range []string{"new Function", "eval(", "<style"} {
		if strings.Contains(vendor, banned) {
			t.Errorf("the vendored ECharts build contains %q", banned)
		}
	}
	if lic, notice := read("vendor/ECHARTS-LICENSE.txt"), read("vendor/ECHARTS-NOTICE.txt"); !strings.Contains(lic, "Apache License") ||
		!strings.Contains(lic, "zrender") || !strings.Contains(notice, "Apache ECharts") {
		t.Error("vendor: the ECharts licence or notice is incomplete")
	}
	// the Timing tab's tooltips are elements made by the page, never the
	// library's markup (whose style attributes the CSP blocks)
	if !strings.Contains(js, "confine: true") || !strings.Contains(js, "formatter: formatter") {
		t.Error("analytics.js: the chart tooltips must be confined and built by a formatter")
	}
	ws := benchWebServer(t, 3)
	for _, p := range []string{"/analytics.html", "/analytics.js", "/analytics-stats.js", "/vendor/echarts-6.1.0.custom.min.js", "/vendor/ECHARTS-LICENSE.txt"} {
		if rec := getWeb(ws, p); rec.Code != 200 {
			t.Errorf("%s: got %d, want 200", p, rec.Code)
		}
	}
}

// TestAnalyticsStatsNode runs the tests of the Analytics page's calculations
// (frontend/analytics-stats.js, testdata/analytics-stats.test.js) with node,
// when node is on the PATH (skipped otherwise), and checks every page script
// for syntax errors (node --check).
func TestAnalyticsStatsNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	for _, f := range []string{"frontend/analytics-stats.js", "frontend/analytics.js", "frontend/app.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("node --check %s: %v: %s", f, err, out)
		}
	}
	out, err := exec.Command(node, "testdata/analytics-stats.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/analytics-stats.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/analytics-stats.test.js: got %q, want \"ok: …\"", out)
	}
}

// TestAnalyticsExitNode runs testdata/analytics-exit.test.js with node, when
// node is on the PATH (skipped otherwise): the Peak vs final tab's exit-rule
// table (fillExitTable, exitRow, ppCell in frontend/analytics.js) drawn for a
// normal window, fewer than 20 calls (greyed), no calls and points missing a
// peak or a return. renderPeak draws it before its early return, so a throw
// there would blank the whole tab.
func TestAnalyticsExitNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, "testdata/analytics-exit.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/analytics-exit.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/analytics-exit.test.js: got %q, want \"ok: …\"", out)
	}
}

// TestAnalyticsHealthNode runs testdata/analytics-health.test.js with node,
// when node is on the PATH (skipped otherwise): the Overview tab's data
// health tables (renderHealth, healthRecords, fillHealthTable,
// fillHealthFamily, fewRateCell in frontend/analytics.js) drawn for a normal
// week, a week with no known trade count, the MIN_N greying, no calls, the
// Month period and the page filters set (ignored). renderOverview draws
// them, so a throw there would blank the rest of the tab.
func TestAnalyticsHealthNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, "testdata/analytics-health.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/analytics-health.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/analytics-health.test.js: got %q, want \"ok: …\"", out)
	}
}

// TestAnalyticsLiveNode runs testdata/analytics-live.test.js with node, when
// node is on the PATH (skipped otherwise): the Model insights section's
// calculations (LIVE_RULES, joinModelLive, liveBucketStats in
// frontend/analytics-stats.js) and drawing (applyLive, loadLive, renderLive
// in frontend/analytics.js) for a normal case, no outcomes yet, the 404
// (hidden) and error states, MIN_N greying, a true, false and null "passed",
// the page filters, extreme outcomes (above +100,000% in any bucket's column:
// an outcome in no bucket) and the +1,000% cap on the mean returns. render
// calls renderLive, so a throw there would
// blank the page.
func TestAnalyticsLiveNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, "testdata/analytics-live.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/analytics-live.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/analytics-live.test.js: got %q, want \"ok: …\"", out)
	}
}

// TestWebAnaMcap: the Analytics page's market cap at the call is the price at
// the post × the token supply, for USD-priced calls with both inputs positive
// and finite and a product of at most webAnaMaxMcap; never the posted figure.
func TestWebAnaMcap(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	posted := 123456.0
	for _, c := range []struct {
		name          string
		usd           bool
		price, supply *float64
		want          *float64
	}{
		{"positive", true, f64p(0.0005), f64p(1e9), f64p(5e5)},
		{"exact cap", true, f64p(10), f64p(1e12), f64p(1e13)},
		{"not priced in USD", false, f64p(0.0005), f64p(1e9), nil},
		{"no supply", true, f64p(0.0005), nil, nil},
		{"no price", true, nil, f64p(1e9), nil},
		{"zero supply", true, f64p(0.0005), f64p(0), nil},
		{"zero price", true, f64p(0), f64p(1e9), nil},
		{"negative supply", true, f64p(0.0005), f64p(-1), nil},
		{"NaN supply", true, f64p(0.0005), &nan, nil},
		{"Inf supply", true, f64p(0.0005), &inf, nil},
		{"NaN price", true, &nan, f64p(1e9), nil},
		{"Inf price", true, &inf, f64p(1e9), nil},
		{"above the cap", true, f64p(10.01), f64p(1e12), nil},
		{"product overflows", true, f64p(1e200), f64p(1e200), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &ScoutWebRow{usd: c.usd, PostPrice: c.price, TokenSupply: c.supply, CalledAtMcap: &posted, PostMcap: &posted}
			got := webAnaMcap(r)
			switch {
			case c.want == nil && got != nil:
				t.Errorf("webAnaMcap(usd %v, price %v, supply %v) = %v, want nil", c.usd, dumpFactors(c.price), dumpFactors(c.supply), *got)
			case c.want != nil && (got == nil || math.Abs(*got-*c.want) > 1e-9**c.want):
				t.Errorf("webAnaMcap(usd %v, price %v, supply %v) = %v, want %v", c.usd, dumpFactors(c.price), dumpFactors(c.supply), dumpFactors(got), *c.want)
			}
		})
	}
}
