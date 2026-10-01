package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// trackerFixture: a scanner with a real DB and a fake GeckoTerminal.
func trackerFixture(t *testing.T) (*scanner, *ScoutStore, *fakeGecko) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fakeGecko{pools: map[string][]map[string]any{}, candles: map[string][]candle{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	t.Setenv("SCOUT_PRICE_API_BASE", srv.URL)
	t.Setenv("SCOUT_PRICE_RPM", "6000")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	return s, st, f
}

// addToken sets up a pool whose price is 1.0 at entry, spikes to 5 at +2h,
// then trades at 2.0 hourly, and collapses to 0.01 after 10 days.
func addToken(f *fakeGecko, token, pool string, entry time.Time, liq float64) {
	f.pools[strings.ToLower(token)] = []map[string]any{gpool(pool, token, liq, 0.01, entry.Add(-2*time.Hour), "longxyz")}
	e := entry.Unix()
	m0 := e - e%60
	f.candles[pool+"|minute"] = []candle{{T: m0 - 60, O: 0.98, H: 1, L: 0.97, C: 0.99}, {T: m0, O: 0.99, H: 1.01, L: 0.99, C: 1.0}, {T: m0 + 60, O: 1, H: 1.1, L: 1, C: 1.05}}
	h0 := e - e%3600
	var hs []candle
	for k := int64(0); k < 24*40; k++ {
		c := candle{T: h0 + k*3600, O: 2, H: 2.1, L: 1.9, C: 2}
		if k == 2 {
			c.H = 5
		}
		if k >= 24*10 {
			c = candle{T: h0 + k*3600, O: 0.01, H: 0.012, L: 0.008, C: 0.01}
		}
		if c.T < time.Now().Unix() {
			hs = append(hs, c)
		}
	}
	f.candles[pool+"|hour"] = hs
}

func postAt(id int, at time.Time, ca string) *tg.Message {
	return &tg.Message{ID: id, Date: int(at.Unix()), Message: samplePost,
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonURL{Text: "Chart", URL: "https://dexscreener.com/robinhood/" + ca}}}}}}
}

func TestTrackerEndToEnd(t *testing.T) {
	s, st, f := trackerFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	young := "0x4444444444444444444444444444444444444444" // 8 days old: 30d pending
	old := "0x5555555555555555555555555555555555555555"   // 35 days old: all done, rugged
	ghost := "0x6666666666666666666666666666666666666666" // no pool on the price source
	fresh := "0x7777777777777777777777777777777777777777" // 10 min old: nothing due yet
	eYoung, eOld := now.Add(-8*24*time.Hour), now.Add(-35*24*time.Hour)
	addToken(f, young, "0xpoolyoung", eYoung, 25000)
	addToken(f, old, "0xpoolold", eOld, 120) // liquidity pulled → rug

	s.onChannelPost(postAt(1, eYoung, young))
	s.onChannelPost(postAt(2, eOld, old))
	s.onChannelPost(postAt(3, now.Add(-2*time.Hour), ghost))
	s.onChannelPost(postAt(4, now.Add(-10*time.Minute), fresh))
	var ids []int
	for i := 0; i < 4; i++ {
		ids = append(ids, *(<-s.queue).CallID)
	}

	if n := s.trackDue(ctx, 50); n != 3 {
		t.Fatalf("processed %d, want 3 (fresh call not due yet)", n)
	}

	// young: entry from the minute candle, 1h/1d/3d/7d done, 30d pending
	ty, _ := st.GetTracking(ctx, ids[0])
	if ty.Status != TrackTracking || ty.EntryPriceUSD == nil || *ty.EntryPriceUSD != 1.0 || *ty.EntryPriceSource != "minute" ||
		*ty.PoolAddress != "0xpoolyoung" || ty.Priority != 0 {
		t.Fatalf("young tracking: %+v", ty)
	}
	if want := eYoung.Add(30*24*time.Hour + 10*time.Minute); !ty.NextCheckAt.Equal(want.Truncate(time.Microsecond)) && ty.NextCheckAt.Sub(want).Abs() > time.Second {
		t.Fatalf("next check %s want %s", ty.NextCheckAt, want)
	}
	ry, _ := st.ReturnsForCall(ctx, ids[0])
	if len(ry) != 4 {
		t.Fatalf("young returns: %v", sortedKeys(ry))
	}
	if r := ry["1d"]; !near(r.ReturnPct, 100) || !near(r.MaxGainPct, 400) || r.Status != "done" {
		t.Fatalf("young 1d: %+v", r)
	}

	// old: everything done; collapse after day 10 + tiny liquidity → rugged
	to, _ := st.GetTracking(ctx, ids[1])
	if to.Status != TrackDone || to.Rugged == nil || !*to.Rugged {
		t.Fatalf("old tracking: %+v", to)
	}
	ro, _ := st.ReturnsForCall(ctx, ids[1])
	if r := ro["30d"]; len(ro) != 5 || !near(r.ReturnPct, -99) || !near(r.MaxGainPct, 400) || !near(r.MaxDDPct, -99.2) {
		t.Fatalf("old 30d: %+v (%v)", r, sortedKeys(ro))
	}

	// ghost: no pool → retried in 6h
	tg3, _ := st.GetTracking(ctx, ids[2])
	if tg3.Status != TrackNoPool || tg3.Error == nil || tg3.NextCheckAt.Before(now.Add(5*time.Hour)) {
		t.Fatalf("ghost tracking: %+v", tg3)
	}
	// fresh: untouched until entry + 1h
	tf, _ := st.GetTracking(ctx, ids[3])
	if tf.Status != TrackPending || tf.Attempts != 0 {
		t.Fatalf("fresh tracking: %+v", tf)
	}
	// nothing else due now
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("second pass processed %d", n)
	}

	// a re-call of the same CA reuses the known pool (no pools API call)
	before := len(f.requests)
	s.onChannelPost(&tg.Message{ID: 9, Date: int(eYoung.Add(time.Hour).Unix()), Message: "again " + young})
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = (SELECT max(call_id) FROM scout_call_tracking)`); err != nil {
		t.Fatal(err)
	}
	s.trackDue(ctx, 50)
	for _, r := range f.requests[before:] {
		if strings.Contains(r, "/tokens/") {
			t.Fatalf("pools API called for a known CA: %v", f.requests[before:])
		}
	}

	// dataset export: one row per call with outcomes pivoted
	var buf bytes.Buffer
	n, err := st.ExportDatasetCSV(ctx, &buf)
	if err != nil || n != 5 {
		t.Fatalf("export: %d rows, %v", n, err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, c := range []string{"mcap_usd", "liq_usd", "holders", "proof_elite", "live_buys_elite_usd", "perceptor_verdict", "entry_price_usd", "rugged", "ret_1d", "max_gain_7d", "max_dd_30d"} {
		if _, ok := col[c]; !ok {
			t.Fatalf("export missing column %s: %v", c, recs[0])
		}
	}
	var oldRow []string
	for _, r := range recs[1:] {
		if r[col["contract_address"]] == old {
			oldRow = r
		}
	}
	if oldRow == nil || oldRow[col["ret_30d"]] != "-99" || oldRow[col["rugged"]] != "true" || oldRow[col["mcap_usd"]] != "52000" || oldRow[col["holders"]] != "1150" {
		t.Fatalf("old row: %v", oldRow)
	}
}

func TestBackfillMessage(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	ca := "0x8888888888888888888888888888888888888888"
	var bs backfillStats
	s.backfillMessage(postAt(500, time.Now().Add(-20*24*time.Hour), ca), &bs)
	s.backfillMessage(&tg.Message{ID: 501, Message: "gm"}, &bs)
	if bs.Posts != 2 || bs.Calls != 1 || bs.CAs != 1 {
		t.Fatalf("stats %+v", bs)
	}
	calls, _ := st.SelectScoutCalls(ctx, ca, 10)
	if len(calls) != 1 || calls[0].Status != CallStatusBackfill {
		t.Fatalf("calls %+v", calls)
	}
	tr, _ := st.GetTracking(ctx, *calls[0].ID)
	if tr == nil || tr.Priority != 1 || tr.Status != TrackPending {
		t.Fatalf("tracking %+v", tr)
	}
	if m, _ := st.GetCallMetrics(ctx, *calls[0].ID); m == nil || i(m.Meta.Holders) != 1150 {
		t.Fatal("backfilled call has no metrics")
	}
	// backfilling the same post again is harmless and doesn't touch the seen-list
	s.backfillMessage(postAt(500, time.Now().Add(-20*24*time.Hour), ca), &bs)
	if calls, _ := st.SelectScoutCalls(ctx, ca, 10); len(calls) != 1 {
		t.Fatalf("duplicate rows: %d", len(calls))
	}
	if !s.seen.markNew(ca) {
		t.Fatal("backfill must not mark CAs as investigated")
	}
}

// Running -backfill again (any number of times, overlapping ranges, CA written in
// a different case) must not create duplicate rows or reset tracking progress.
func TestBackfillIsIdempotent(t *testing.T) {
	s, st, f := trackerFixture(t)
	ctx := context.Background()
	caA := "0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa"
	caB := "0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb"
	caC := "0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc"
	entry := time.Now().Add(-10 * 24 * time.Hour).UTC()
	addToken(f, caA, "0xpoola", entry, 30000)

	twoCAs := postAt(102, entry.Add(time.Hour), caB)
	twoCAs.Message += "\nalso " + caC
	posts := []*tg.Message{
		postAt(101, entry, caA),
		twoCAs,
		{ID: 103, Date: int(entry.Unix()), Message: "gm, no call here"},
	}
	run := func() backfillStats {
		var bs backfillStats
		for _, m := range posts {
			s.backfillMessage(m, &bs)
		}
		return bs
	}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tbl := range []string{"scout_calls", "scout_call_metrics", "scout_call_live_buys", "scout_call_tracking", "scout_call_returns"} {
			var n int
			if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[tbl] = n
		}
		return out
	}

	first := run()
	if first.Posts != 3 || first.Calls != 2 || first.CAs != 3 || first.New != 3 || first.Existing != 0 {
		t.Fatalf("first run stats %+v", first)
	}
	want := map[string]int{"scout_calls": 3, "scout_call_metrics": 3, "scout_call_live_buys": 12, "scout_call_tracking": 3, "scout_call_returns": 0}
	if got := counts(); !equalCounts(got, want) {
		t.Fatalf("after first run: %v want %v", got, want)
	}

	// price tracking makes progress on call A
	if n := s.trackDue(ctx, 50); n == 0 {
		t.Fatal("tracker processed nothing")
	}
	calls, _ := st.SelectScoutCalls(ctx, caA, 10)
	trBefore, _ := st.GetTracking(ctx, *calls[0].ID)
	retBefore := counts()["scout_call_returns"]
	if trBefore.EntryPriceUSD == nil || retBefore == 0 {
		t.Fatalf("tracking didn't progress: %+v, %d returns", trBefore, retBefore)
	}
	want["scout_call_returns"] = retBefore

	// re-run twice, plus once with the CA in lower case (same call)
	for i := 0; i < 2; i++ {
		again := run()
		if again.New != 0 || again.Existing != 3 {
			t.Fatalf("re-run %d stats %+v", i+1, again)
		}
	}
	var bs backfillStats
	s.backfillMessage(postAt(101, entry, strings.ToLower(caA)), &bs)
	if bs.New != 0 || bs.Existing != 1 {
		t.Fatalf("case variant created a new call: %+v", bs)
	}
	if got := counts(); !equalCounts(got, want) {
		t.Fatalf("after re-runs: %v want %v", got, want)
	}

	// tracking progress and call status were kept
	trAfter, _ := st.GetTracking(ctx, *calls[0].ID)
	if trAfter.Status != trBefore.Status || *trAfter.EntryPriceUSD != *trBefore.EntryPriceUSD ||
		trAfter.Attempts != trBefore.Attempts || !trAfter.NextCheckAt.Equal(trBefore.NextCheckAt) {
		t.Fatalf("tracking reset by re-run:\nbefore %+v\nafter  %+v", trBefore, trAfter)
	}

	// a call first seen live keeps its status when later backfilled
	live := "0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd"
	s.onChannelPost(postAt(104, entry, live))
	j := <-s.queue
	s.setCallStatus(j.CallID, CallStatusScanned)
	s.backfillMessage(postAt(104, entry, live), &bs)
	if c, _ := st.GetScoutCall(ctx, *j.CallID); c.Status != CallStatusScanned {
		t.Fatalf("backfill overwrote live status: %s", c.Status)
	}
	if got, _ := st.SelectScoutCalls(ctx, live, 10); len(got) != 1 {
		t.Fatalf("live+backfill gave %d rows", len(got))
	}

	// an edited post refreshes its text, still one row
	edited := postAt(101, entry, caA)
	edited.Message += "\nUPDATE: CEX listing"
	s.backfillMessage(edited, &bs)
	c, _ := st.GetScoutCall(ctx, *calls[0].ID)
	if !strings.Contains(c.MessageText, "CEX listing") {
		t.Fatal("edited text not refreshed")
	}
	if n := counts()["scout_calls"]; n != 4 {
		t.Fatalf("scout_calls = %d, want 4", n)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
