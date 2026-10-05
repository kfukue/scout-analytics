package main

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

func TestCommas(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 3120: "3,120", 1234567: "1,234,567", -1000: "-1,000"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

// seedTrackedCall stores a call and its tracking row (due since firstCheck).
func seedTrackedCall(t *testing.T, st *ScoutStore, msg int, at time.Time, ca string) int {
	t.Helper()
	ctx := context.Background()
	id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: msg,
		MessageDate: at, ContractAddress: ca, Chain: "evm", Status: CallStatusBackfill})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTracking(ctx, *id, ca, at, 1, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return *id
}

func trackingStatuses(t *testing.T, st *ScoutStore, ids map[string]int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, id := range ids {
		tr, err := st.GetTracking(context.Background(), id)
		if err != nil || tr == nil {
			t.Fatalf("tracking row of %s: %v %v", k, tr, err)
		}
		out[k] = tr.Status
	}
	return out
}

func wantStatuses(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: %s is %q, want %q (all: %v)", what, k, got[k], w, got)
		}
	}
}

// Only the first call of each token stays in the tracker's queue; the rule is
// the website's (earliest message_date, then lowest id, address case-insensitive).
func TestMarkRepeatTracking(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	caA, caB, caC := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "0xcccccccccccccccccccccccccccccccccccccccc"
	ids := map[string]int{}
	ids["a1"] = seedTrackedCall(t, st, 1, base, caA)                                          // first call of A
	ids["a2"] = seedTrackedCall(t, st, 2, base, caA)                                          // same message_date, higher id
	ids["a3"] = seedTrackedCall(t, st, 3, base.Add(time.Hour), "0x"+strings.ToUpper(caA[2:])) // same token, other letter case
	ids["b1"] = seedTrackedCall(t, st, 4, base.Add(2*time.Hour), caB)                         // called once
	ids["c1"] = seedTrackedCall(t, st, 5, base.Add(3*time.Hour), caC)                         // first call of C
	ids["c2"] = seedTrackedCall(t, st, 6, base.Add(4*time.Hour), caC)                         // repeat of C, already done
	if ids["a1"] > ids["a2"] {
		t.Fatalf("seed ids out of order: %v", ids)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'done', entry_price_usd = 2 WHERE call_id = $1`, ids["c2"]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'error', error = 'rpc: boom', attempts = 2 WHERE call_id = $1`, ids["a3"]); err != nil {
		t.Fatal(err)
	}
	var viewRows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_dataset_v`).Scan(&viewRows); err != nil || viewRows != 6 {
		t.Fatalf("dataset view rows before: %d %v", viewRows, err)
	}
	b1Before, _ := st.GetTracking(ctx, ids["b1"])

	n, err := st.MarkRepeatTracking(ctx)
	if err != nil || n != 2 {
		t.Fatalf("MarkRepeatTracking = %d, %v; want 2", n, err)
	}
	wantStatuses(t, "first run", trackingStatuses(t, st, ids), map[string]string{
		"a1": TrackPending, "a2": TrackRepeat, "a3": TrackRepeat, "b1": TrackPending, "c1": TrackPending, "c2": TrackDone})
	if a3, _ := st.GetTracking(ctx, ids["a3"]); a3.Error != nil {
		t.Fatalf("error not cleared on a repeat row: %q", *a3.Error)
	}
	if c2, _ := st.GetTracking(ctx, ids["c2"]); c2.EntryPriceUSD == nil || *c2.EntryPriceUSD != 2 {
		t.Fatalf("done repeat lost its result: %+v", c2)
	}
	if b1, _ := st.GetTracking(ctx, ids["b1"]); !b1.NextCheckAt.Equal(b1Before.NextCheckAt) || b1.Attempts != b1Before.Attempts {
		t.Fatalf("B changed: %+v → %+v", b1Before, b1)
	}

	due, err := st.DueTracking(ctx, time.Now(), 50)
	if err != nil {
		t.Fatal(err)
	}
	gotDue := map[int]bool{}
	for _, d := range due {
		gotDue[d.CallID] = true
	}
	if len(due) != 3 || !gotDue[ids["a1"]] || !gotDue[ids["b1"]] || !gotDue[ids["c1"]] {
		t.Fatalf("due = %v, want the first calls %d %d %d", gotDue, ids["a1"], ids["b1"], ids["c1"])
	}
	counts, _, err := st.TrackingStats(ctx)
	if err != nil || counts[TrackRepeat] != 2 || counts[TrackPending] != 3 || counts[TrackDone] != 1 {
		t.Fatalf("stats: %v %v", counts, err)
	}

	// Nothing left to do: no row is written.
	var stamp time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT max(updated_at) FROM scout_call_tracking`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v; want 0", n, err)
	}
	var stamp2 time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT max(updated_at) FROM scout_call_tracking`).Scan(&stamp2); err != nil || !stamp2.Equal(stamp) {
		t.Fatalf("second run wrote rows: %v → %v (%v)", stamp, stamp2, err)
	}
	// The dataset view keeps one row per call; repeats are marked, not removed.
	var repeats int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tracking_status = 'repeat') FROM scout_call_dataset_v`).Scan(&viewRows, &repeats); err != nil || viewRows != 6 || repeats != 2 {
		t.Fatalf("dataset view: %d rows, %d repeat (%v)", viewRows, repeats, err)
	}
	// The website's counts are over first calls: none of them is a repeat.
	sum, err := webSummaryOf(ctx, st)
	if err != nil || sum.Imported != 3 || sum.Pending != 3 || sum.TotalCalls != 6 || sum.RepeatCalls != 3 {
		t.Fatalf("web summary: %+v %v", sum, err)
	}

	// A newest-first backfill imports an OLDER call of B: that one is tracked,
	// the call that looked first until now becomes a repeat.
	ids["b0"] = seedTrackedCall(t, st, 7, base.Add(-10*time.Hour), caB)
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 1 {
		t.Fatalf("after the older call = %d, %v; want 1", n, err)
	}
	wantStatuses(t, "older call imported", trackingStatuses(t, st, ids), map[string]string{
		"b0": TrackPending, "b1": TrackRepeat, "a1": TrackPending, "a2": TrackRepeat, "a3": TrackRepeat, "c1": TrackPending, "c2": TrackDone})

	// The older call is deleted: the remaining call of B is the first again and is due now.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() + interval '5 days', attempts = 4 WHERE call_id = $1`, ids["b1"]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM scout_calls WHERE id = $1`, ids["b0"]); err != nil { // tracking row goes with it
		t.Fatal(err)
	}
	delete(ids, "b0")
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 1 {
		t.Fatalf("after the delete = %d, %v; want 1", n, err)
	}
	b1, _ := st.GetTracking(ctx, ids["b1"])
	if b1.Status != TrackPending || b1.NextCheckAt.After(time.Now().Add(time.Second)) || b1.Attempts != 0 || b1.Error != nil {
		t.Fatalf("B after the delete: %+v", b1)
	}
	var dueNow bool
	if err := st.Pool.QueryRow(ctx, `SELECT next_check_at <= now() FROM scout_call_tracking WHERE call_id = $1`, ids["b1"]).Scan(&dueNow); err != nil || !dueNow {
		t.Fatalf("B not due now: %v %v", dueNow, err)
	}
	wantStatuses(t, "older call deleted", trackingStatuses(t, st, ids), map[string]string{
		"a1": TrackPending, "a2": TrackRepeat, "a3": TrackRepeat, "c1": TrackPending, "c2": TrackDone})
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 0 {
		t.Fatalf("last run = %d, %v; want 0", n, err)
	}
}

// untouchedRepeat fails unless the call is a repeat nobody worked on.
func untouchedRepeat(t *testing.T, st *ScoutStore, what string, id int) {
	t.Helper()
	ctx := context.Background()
	tr, err := st.GetTracking(ctx, id)
	if err != nil || tr == nil {
		t.Fatalf("%s: %v %v", what, tr, err)
	}
	if tr.Status != TrackRepeat || tr.Attempts != 0 || tr.LastCheckedAt != nil || tr.PoolAddress != nil ||
		tr.EntryPriceUSD != nil || tr.EntryLatePriceUSD != nil || tr.Error != nil || len(tr.Onchain) != 0 {
		t.Fatalf("%s was worked on: %+v", what, tr)
	}
	if rs, err := st.ReturnsForCall(ctx, id); err != nil || len(rs) != 0 {
		t.Fatalf("%s has returns: %v %v", what, sortedKeys(rs), err)
	}
	var candles int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_candles WHERE call_id = $1`, id).Scan(&candles); err != nil || candles != 0 {
		t.Fatalf("%s has %d candles (%v)", what, candles, err)
	}
}

// On-chain source: a cycle tracks the first call of a token and does no node
// work for its repeat calls; the log says what was skipped.
func TestTrackDueSkipsRepeatCallsOnchain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t, 60*24*time.Hour)
	feed := "0x00000000000000000000000000000000000000fe"
	f.addToken(tWETH, 18, "WETH")
	f.constCall(feed, selDecimals, ret(wInt(8)))
	f.constCall(feed, selLatestRound, ret(wInt(1), wInt(3000e8), wInt(0), wInt(0), wInt(1)))
	token, pool := "0x44444444444444444444444444444444444444ab", "0x00000000000000000000000000000000000000c3"
	entry := time.Now().Add(-8 * 24 * time.Hour)
	f.addToken(token, 18, "TKN")
	f.constCall(pool, selToken0, ret(wAddr(tWETH)))
	f.constCall(pool, selToken1, ret(wAddr(token)))
	f.constCall(pool, selSlot0, ret(wInt(1)))
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(5))))
	eb := f.blockAtTime(entry)
	for off, p := range map[uint64]float64{500: 1e-6, 1000 + 20000: 5e-6, 1000 + 30000: 2e-6, 1000 + 2*864000: 3e-6} {
		blk := eb + off - 1000
		f.swapV3(pool, blk, hexU64(blk), sqrtX96(1/p))
		f.transfer(token, pool, "0x00000000000000000000000000000000000000d1", blk, hexU64(blk))
	}
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_CHAINLINK_FEEDS", "eth="+feed)
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	s := newScanner(&config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc})
	s.db = st

	// Stored newest first, as a backfill does: the first call has the highest id.
	later := seedTrackedCall(t, st, 3, entry.Add(48*time.Hour), token)
	otherCase := seedTrackedCall(t, st, 2, entry.Add(24*time.Hour), "0x"+strings.ToUpper(token[2:]))
	first := seedTrackedCall(t, st, 1, entry, token)

	var buf syncBuf
	log.SetOutput(&buf)
	defer log.SetOutput(log.Default().Writer())

	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d call(s), want 1 (the first call)\n%s", n, buf.String())
	}
	s.logTrackingStatus(ctx, 1)
	tf, _ := st.GetTracking(ctx, first)
	rf, _ := st.ReturnsForCall(ctx, first)
	if tf.Status != TrackTracking || tf.EntryPriceUSD == nil || len(rf) != 4 {
		t.Fatalf("first call: %+v (err %v) returns %v\n%s", tf, strOrNil(tf.Error), sortedKeys(rf), buf.String())
	}
	untouchedRepeat(t, st, "later call", later)
	untouchedRepeat(t, st, "call with the address in other letters", otherCase)

	// The next cycle has nothing to do and asks the node nothing.
	f.mu.Lock()
	logsBefore, callsBefore := f.count["eth_getLogs"], f.count["eth_call"]
	f.mu.Unlock()
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("second cycle processed %d", n)
	}
	s.logTrackingStatus(ctx, 0)
	f.mu.Lock()
	dLogs, dCalls := f.count["eth_getLogs"]-logsBefore, f.count["eth_call"]-callsBefore
	f.mu.Unlock()
	if dLogs != 0 || dCalls != 0 {
		t.Fatalf("second cycle asked the node: %d eth_getLogs, %d eth_call", dLogs, dCalls)
	}

	out := buf.String()
	skipped := "tracking: 2 repeat call(s) skipped — only the first call of each token is tracked"
	if strings.Count(out, skipped) != 1 { // logged when it changed something, not on the idle cycle
		t.Fatalf("want %q exactly once in:\n%s", skipped, out)
	}
	for _, want := range []string{"tracking: 1 call(s) due now", "tracking: processed 1 call(s) — tracking 1, repeat 2;", "tracking: idle — tracking 1, repeat 2;"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, id := range []int{later, otherCase} {
		if strings.Contains(out, "call "+itoa(id)+" ") || strings.Contains(out, "call "+itoa(id)+":") {
			t.Fatalf("repeat call %d was worked on:\n%s", id, out)
		}
	}
}

func itoa(n int) string { return commas(n) } // ids in these tests are below 1000

// GeckoTerminal source: the same rule, no API request for a repeat call.
func TestTrackDueSkipsRepeatCallsGecko(t *testing.T) {
	s, st, f := trackerFixture(t)
	ctx := context.Background()
	token, pool := "0x4444444444444444444444444444444444444444", "0xpool4"
	entry := time.Now().Add(-8 * 24 * time.Hour)
	addToken(f, token, pool, entry, 50000)
	first := seedTrackedCall(t, st, 1, entry, token)
	repeat := seedTrackedCall(t, st, 2, entry.Add(24*time.Hour), token)

	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d call(s), want 1", n)
	}
	tf, _ := st.GetTracking(ctx, first)
	if tf.EntryPriceUSD == nil || (tf.Status != TrackTracking && tf.Status != TrackDone) {
		t.Fatalf("first call: %+v (err %v)", tf, strOrNil(tf.Error))
	}
	untouchedRepeat(t, st, "repeat call", repeat)
	counts, _, err := st.TrackingStats(ctx)
	if err != nil || counts[TrackRepeat] != 1 {
		t.Fatalf("stats: %v %v", counts, err)
	}
}
