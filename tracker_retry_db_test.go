package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// retryRow is the part of a tracking row -retry-no-pool may change, plus priority.
type retryRow struct {
	Status      string
	NextCheckAt time.Time
	Attempts    int
	Error       *string
	Priority    int
	Onchain     *string
	PoolAddress *string
}

func retrySnapshot(t *testing.T, st *ScoutStore) map[int]retryRow {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `SELECT call_id, status, next_check_at, attempts, error, priority,
		onchain::text, pool_address FROM scout_call_tracking`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]retryRow{}
	for rows.Next() {
		var id int
		var r retryRow
		if err := rows.Scan(&id, &r.Status, &r.NextCheckAt, &r.Attempts, &r.Error, &r.Priority, &r.Onchain, &r.PoolAddress); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// setRetryRow puts a tracking row in a status, scheduled 6 h ahead with an error and a few attempts.
func setRetryRow(t *testing.T, st *ScoutStore, id int, status string, priority int) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking SET status = $2, priority = $3,
		next_check_at = now() + interval '6 hours', attempts = 4, error = 'no pool yet' WHERE call_id = $1`,
		id, status, priority); err != nil {
		t.Fatal(err)
	}
}

func setMeta(t *testing.T, st *ScoutStore, id int, launchpad, dex string) {
	t.Helper()
	m := &CallMeta{}
	if launchpad != "" {
		m.Launchpad = &launchpad
	}
	if dex != "" {
		m.Dex = &dex
	}
	if err := st.UpsertCallMetrics(context.Background(), id, m); err != nil {
		t.Fatal(err)
	}
}

func TestParseLaunchpads(t *testing.T) {
	got := parseLaunchpads(" pons_v2, Pons V2 ,,LONG-xyz, ")
	want := []string{"ponsv2", "ponsv2", "longxyz"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parseLaunchpads = %q, want %q", got, want)
	}
	if parseLaunchpads(" , _ ") != nil {
		t.Fatal("only separators must give no value")
	}
	if normLaunchpad("Pons V2") != normLaunchpad("pons_v2") || normLaunchpad("Pons") == normLaunchpad("pons_v2") {
		t.Fatal("normLaunchpad")
	}
}

// Which rows -retry-no-pool resets: first calls only, no_pool (gave_up only
// with the flag), the launchpad / dex filter, dry run, and that reset rows are
// due right away with their priority kept.
func TestRetryNoPoolFilters(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	ca := func(n int) string {
		return "0x" + strings.Repeat("0", 38) + string(rune('a'+n/10)) + string(rune('0'+n%10))
	}
	ids := map[string]int{}
	seed := func(name string, msg, n int, status string, priority int, launchpad, dex string) {
		id := seedTrackedCall(t, st, msg, at.Add(time.Duration(msg)*time.Minute), ca(n))
		setRetryRow(t, st, id, status, priority)
		if launchpad != "" || dex != "" {
			setMeta(t, st, id, launchpad, dex)
		}
		ids[name] = id
	}
	seed("pons", 1, 1, TrackNoPool, 0, "Pons V2", "")         // live call: priority 0
	seed("ponsDex", 2, 2, TrackNoPool, 1, "Other", "PONS_v2") // matched through dex
	seed("long", 3, 3, TrackNoPool, 1, "Longxyz", "Longxyz")
	seed("nometa", 4, 4, TrackNoPool, 1, "", "")
	seed("gaveUp", 5, 5, TrackGaveUp, 1, "pons v2", "")
	seed("later", 6, 1, TrackNoPool, 1, "Pons V2", "") // second call of "pons": not a first call
	seed("update", 7, 7, TrackNoPool, 1, "Pons V2", "")
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = 'update', status = 'update' WHERE id = $1`, ids["update"]); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{TrackRepeat, TrackDone, TrackTracking, TrackError, TrackPending} {
		seed(s, 10+i, 10+i, s, 1, "Pons V2", "")
	}
	// the gave_up row has the state of a pool that had no trades
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = '{"v":2,"kind":"v3","pool":"0xstale"}',
		pool_address = '0xstale', pool_dex = 'uniswap-v3', error = 'no trades around the call time' WHERE call_id = $1`, ids["gaveUp"]); err != nil {
		t.Fatal(err)
	}
	before := retrySnapshot(t, st)
	pons := RetryFilter{GaveUp: true, Launchpads: parseLaunchpads("pons_v2")}

	// Dry run: summary only, nothing changes.
	var out bytes.Buffer
	if err := runRetryNoPool(ctx, st, &out, pons, true); err != nil {
		t.Fatal(err)
	}
	t.Logf("dry run output:\n%s", out.String())
	for _, want := range []string{"3 call(s) to reset to pending — no_pool 2, gave_up 1", "Pons V2", "PONS_v2", "dry run — nothing changed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}
	if after := retrySnapshot(t, st); len(after) != len(before) {
		t.Fatal("dry run changed the row count")
	} else {
		for id, r := range before {
			a := after[id]
			if a.Status != r.Status || !a.NextCheckAt.Equal(r.NextCheckAt) || a.Attempts != r.Attempts ||
				strOrNil(a.Error) != strOrNil(r.Error) || strOrNil(a.Onchain) != strOrNil(r.Onchain) || strOrNil(a.PoolAddress) != strOrNil(r.PoolAddress) {
				t.Fatalf("dry run changed call %d: %+v → %+v", id, r, a)
			}
		}
	}

	// Pons only, without -retry-gave-up: the two no_pool first calls.
	n, err := st.RetryNoPool(ctx, RetryFilter{Launchpads: parseLaunchpads("Pons V2")}, false, nil)
	if err != nil || n != 2 {
		t.Fatalf("pons no_pool: %d %v", n, err)
	}
	now := time.Now()
	after := retrySnapshot(t, st)
	reset := map[int]bool{ids["pons"]: true, ids["ponsDex"]: true}
	for id, r := range after {
		b := before[id]
		if !reset[id] {
			if r != b && !(r.Status == b.Status && r.NextCheckAt.Equal(b.NextCheckAt) && r.Attempts == b.Attempts &&
				strOrNil(r.Error) == strOrNil(b.Error) && strOrNil(r.Onchain) == strOrNil(b.Onchain) && r.Priority == b.Priority) {
				t.Fatalf("call %d was changed: %+v → %+v", id, b, r)
			}
			continue
		}
		if r.Status != TrackPending || r.Attempts != 0 || r.Error != nil || r.NextCheckAt.After(now) || r.Priority != b.Priority {
			t.Fatalf("call %d not reset properly: %+v (was %+v)", id, r, b)
		}
	}
	due, err := st.DueTracking(ctx, time.Now(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].CallID != ids["pons"] || due[1].CallID != ids["ponsDex"] {
		t.Fatalf("due after reset: %+v", due)
	}

	// With -retry-gave-up: the gave_up row too, and it loses its stale pool.
	if n, err := st.RetryNoPool(ctx, pons, false, nil); err != nil || n != 1 {
		t.Fatalf("pons gave_up: %d %v", n, err)
	}
	g := retrySnapshot(t, st)[ids["gaveUp"]]
	if g.Status != TrackPending || g.Onchain != nil || g.PoolAddress != nil || g.Attempts != 0 {
		t.Fatalf("gave_up row after reset: %+v", g)
	}

	// No filter, no -retry-gave-up: the remaining no_pool first calls; never
	// the later call or the update post, and nothing else.
	out.Reset()
	if err := runRetryNoPool(ctx, st, &out, RetryFilter{}, false); err != nil {
		t.Fatal(err)
	}
	t.Logf("reset output:\n%s", out.String())
	if !strings.Contains(out.String(), "2 row(s) updated") {
		t.Fatalf("output:\n%s", out.String())
	}
	got := trackingStatuses(t, st, ids)
	wantStatuses(t, "after all resets", got, map[string]string{
		"pons": TrackPending, "ponsDex": TrackPending, "long": TrackPending, "nometa": TrackPending, "gaveUp": TrackPending,
		"later": TrackNoPool, "update": TrackNoPool,
		TrackRepeat: TrackRepeat, TrackDone: TrackDone, TrackTracking: TrackTracking, TrackError: TrackError, TrackPending: TrackPending,
	})
	if p := retrySnapshot(t, st)[ids[TrackPending]]; !p.NextCheckAt.Equal(before[ids[TrackPending]].NextCheckAt) || p.Attempts != 4 {
		t.Fatalf("a pending row was touched: %+v", p)
	}

	// Nothing left: says so and changes nothing.
	out.Reset()
	if err := runRetryNoPool(ctx, st, &out, RetryFilter{GaveUp: true}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reset") || !strings.Contains(out.String(), "0 row(s) updated") {
		t.Fatalf("empty output:\n%s", out.String())
	}
}

// gave_up calls well past their deadline get one real attempt after the reset:
// one whose pool can now be found is tracked to the end, one that still has no
// pool is gave_up again after that attempt, and one that gave up on the stale
// state of a pool without trades is discovered afresh (with the stale state
// kept, it would give up again at once).
func TestRetryNoPoolGaveUpDeadline(t *testing.T) {
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
	entry := time.Now().UTC().Add(-40 * 24 * time.Hour) // past 30d + 48 h
	eb := f.blockAtTime(entry)
	day := uint64(864000)
	withPool := func(token, pool string) {
		f.addToken(token, 18, "TKN")
		f.constCall(pool, selToken0, ret(wAddr(tWETH)))
		f.constCall(pool, selToken1, ret(wAddr(token)))
		f.constCall(pool, selSlot0, ret(wInt(1)))
		for off, p := range map[uint64]float64{500: 1e-6, 1000 + 20000: 2e-6, 1000 + 5*day: 3e-6} {
			blk := eb + off - 1000
			f.swapV3(pool, blk, hexU64(blk), sqrtX96(1/p))
			f.transfer(token, pool, "0x00000000000000000000000000000000000000d1", blk, hexU64(blk))
		}
	}
	found := "0x1000000000000000000000000000000000000001" // pool exists now (e.g. a newly supported DEX)
	nopool := "0x2000000000000000000000000000000000000002"
	stale := "0x3000000000000000000000000000000000000003"   // gave up on a pool without trades
	control := "0x4000000000000000000000000000000000000004" // the same, reset without clearing the state
	poolF, poolS, poolC := "0x00000000000000000000000000000000000000c1", "0x00000000000000000000000000000000000000c3", "0x00000000000000000000000000000000000000c4"
	withPool(found, poolF)
	f.addToken(nopool, 18, "NOP")
	withPool(stale, poolS)
	withPool(control, poolC)

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

	staleState, _ := json.Marshal(onchainState{V: onchainStateVersion, Kind: "v3", Pool: "0x00000000000000000000000000000000000000dd",
		Quote: tWETH, QuoteSym: "WETH", QuoteDec: 18, TokenDec: 18, EntryBlock: eb, LateBlock: eb + 600, Done: map[string]bool{}})
	ids := map[string]int{}
	for i, ca := range []string{found, nopool, stale, control} {
		id := seedTrackedCall(t, st, 100+i, entry, ca)
		ids[ca] = id
		errMsg := "no Uniswap v2/v3/v4 pool found"
		var onchain, poolAddr *string
		if ca == stale || ca == control {
			errMsg = "no trades around the call time"
			js, p := string(staleState), "0x00000000000000000000000000000000000000dd"
			onchain, poolAddr = &js, &p
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'gave_up', attempts = 9, error = $2,
			next_check_at = now() - interval '1 day', last_checked_at = now() - interval '1 day',
			onchain = $3::jsonb, pool_address = $4, pool_dex = CASE WHEN $4::text IS NULL THEN NULL ELSE 'uniswap-v3' END
			WHERE call_id = $1`, id, errMsg, onchain, poolAddr); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("gave_up rows were tracked before the reset: %d", n)
	}

	// The control row: back to pending the plain way, keeping its stale state.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'pending', next_check_at = now(), attempts = 0, error = NULL
		WHERE call_id = $1`, ids[control]); err != nil {
		t.Fatal(err)
	}
	// The other three through -retry-no-pool -retry-gave-up.
	n, err := st.RetryNoPool(ctx, RetryFilter{GaveUp: true}, false, nil)
	if err != nil || n != 3 {
		t.Fatalf("reset %d %v", n, err)
	}
	logs := f.count["eth_getLogs"]
	start := time.Now()
	if n := s.trackDue(ctx, 50); n != 4 {
		t.Fatalf("processed %d, want 4", n)
	}
	if f.count["eth_getLogs"] == logs {
		t.Fatal("no node request: the retry was not a real attempt")
	}
	get := func(ca string) *ScoutCallTracking {
		tr, err := st.GetTracking(ctx, ids[ca])
		if err != nil || tr == nil {
			t.Fatalf("%s: %v %v", ca, tr, err)
		}
		return tr
	}
	if tr := get(found); tr.Status != TrackDone || tr.PoolAddress == nil || *tr.PoolAddress != poolF || tr.EntryPriceUSD == nil {
		t.Fatalf("found: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	if tr := get(stale); tr.Status != TrackDone || tr.PoolAddress == nil || *tr.PoolAddress != poolS || tr.EntryPriceUSD == nil {
		t.Fatalf("stale: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	// still no pool: exactly one attempt, then gave_up again (past the deadline)
	if tr := get(nopool); tr.Status != TrackGaveUp || tr.Attempts != 1 || tr.LastCheckedAt == nil || tr.LastCheckedAt.Before(start.Add(-time.Second)) ||
		!strings.Contains(strOrNil(tr.Error), "no Uniswap") {
		t.Fatalf("nopool: %+v (err %s)", tr, strOrNil(tr.Error))
	}
	// why the reset clears the state: kept, the stale pool is read again and the row gives up at once
	if tr := get(control); tr.Status != TrackGaveUp || tr.Attempts != 1 || !strings.Contains(strOrNil(tr.Error), "no trades") {
		t.Fatalf("control: %+v (err %s)", tr, strOrNil(tr.Error))
	}
}
