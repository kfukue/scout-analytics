package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// ops/2026-10-22-park-stuck-calls: the one-off write that parks 20 stuck
// backfilled calls (status error, priority 1) as gave_up so the tracker stops
// retrying them. The owner runs the files by hand in pgAdmin; these tests run
// them as they are against the throwaway database. Helpers shared with the
// price-rug fill test: prfGrid, prfExec, prfTail, prfSnapshot, prfWant.
// ---------------------------------------------------------------------------

const (
	pscDir    = "ops/2026-10-22-park-stuck-calls"
	pscBak    = "scout_call_tracking_bak_park_stuck_20261022"
	pscPrefix = "parked 2026-10-22 (ops/2026-10-22-park-stuck-calls): "
)

// pscListed is the list of calls in the scripts (the owner's query, 22 Oct).
var pscListed = []int{2344, 2920, 2935, 2940, 2946, 3103, 3127, 3195, 3212, 3214, 3220, 3246, 3815, 5033, 5360, 5400, 6201, 6391, 8142, 10061}

// pscRead returns an ops file of the folder.
func pscRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(pscDir), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var pscArrayRe = regexp.MustCompile(`ARRAY\[([0-9, ]+)\]::int\[\]`)

// A, B and C hold the same list of 20 calls; only B has the EDIT line; B's
// new error text can never match the start-up re-queue of "context canceled".
func TestParkStuckCallsFiles(t *testing.T) {
	want := strings.ReplaceAll(strings.Trim(fmt.Sprint(pscListed), "[]"), " ", ", ")
	for _, name := range []string{"A_count.sql", "B_park.sql", "C_check.sql"} {
		m := pscArrayRe.FindAllStringSubmatch(pscRead(t, name), -1)
		if len(m) != 1 {
			t.Errorf("%s: %d id lists, want 1", name, len(m))
			continue
		}
		if m[0][1] != want {
			t.Errorf("%s: ids %s, want %s", name, m[0][1], want)
		}
	}
	b := pscRead(t, "B_park.sql")
	if n := strings.Count(b, "<<< EDIT"); n != 1 {
		t.Errorf("B_park.sql has %d EDIT lines, want 1", n)
	}
	for _, name := range []string{"A_count.sql", "C_check.sql", "R_rollback.sql"} {
		if strings.Contains(pscRead(t, name), "<<< EDIT") {
			t.Errorf("%s has an EDIT line, want none", name)
		}
	}
	if !strings.Contains(b, "'"+pscPrefix+"'") {
		t.Errorf("B_park.sql does not use the prefix %q", pscPrefix)
	}
	if strings.Contains(pscPrefix, "context canceled") {
		t.Error("the prefix contains context canceled")
	}
}

// pscFixture is one call (and its tracking row) of the test.
type pscFixture struct {
	id       int
	noRow    bool // a call without a tracking row
	status   string
	priority int
	errText  string // "" = NULL
	onchain  string // "" = NULL
	entry    *float64
	park     bool // B parks it
}

// pscB is B_park.sql with expected_count set to n.
func pscB(t *testing.T, n int) string {
	t.Helper()
	b := pscRead(t, "B_park.sql")
	const edit = "-1 AS expected_count"
	if c := strings.Count(b, edit); c != 1 {
		t.Fatalf("B_park.sql: %q found %d times, want once (the EDIT line)", edit, c)
	}
	return strings.Replace(b, edit, fmt.Sprintf("%d AS expected_count", n), 1)
}

func pscBakExists(t *testing.T, st *ScoutStore) bool {
	t.Helper()
	var ok bool
	if err := st.Pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, pscBak).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// pscWithout returns a row's JSON without the given keys.
func pscWithout(t *testing.T, st *ScoutStore, js string, keys ...string) string {
	t.Helper()
	var out string
	if err := st.Pool.QueryRow(context.Background(), `SELECT ($1::jsonb - $2::text[])::text`, js, keys).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// pscDue returns the call ids DueTracking hands out now.
func pscDue(t *testing.T, st *ScoutStore) map[int]bool {
	t.Helper()
	rows, err := st.DueTracking(context.Background(), time.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]bool{}
	for _, r := range rows {
		out[r.CallID] = true
	}
	return out
}

// pscRow is the tracking status, error and updated_at of a call.
func pscRow(t *testing.T, st *ScoutStore, id int) (status string, errText *string, updated time.Time) {
	t.Helper()
	if err := st.Pool.QueryRow(context.Background(), `SELECT status, error, updated_at FROM scout_call_tracking WHERE call_id = $1`,
		id).Scan(&status, &errText, &updated); err != nil {
		t.Fatalf("call %d: %v", id, err)
	}
	return status, errText, updated
}

// The ops scripts end to end: A counts, B parks exactly the listed error rows
// of priority 1 (and refuses an unset or wrong count, a "context canceled"
// error and a second run), the parked rows leave the tracker's queue and stay
// parked across a start (except a state version under 2), C shows the state,
// R restores only rows still as B left them and puts them back in the queue.
func TestParkStuckCallsOps(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DROP TABLE IF EXISTS `+pscBak); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// st is closed after this (cleanups run last-registered first).
		if _, err := st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+pscBak); err != nil {
			t.Error(err)
		}
	})

	const v2 = `{"v":2,"kind":"v3","pool":"0xp","quote":"0xq","quote_sym":"WETH","entry_block":100,"scan_block":200}`
	longErr := "swaps to +1d: " + strings.Repeat("x", 500)
	entry := 1.5
	special := map[int]pscFixture{
		2920:  {status: "done", priority: 1, errText: "", onchain: v2},                                   // finished meanwhile
		2935:  {status: "error", priority: 0, errText: "rpc: request timed out", onchain: v2},            // a live call
		10061: {noRow: true},                                                                             // no tracking row
		3103:  {status: "error", priority: 1, errText: "pool discovery: rpc error -32002", park: true},   // no state yet
		3127:  {status: "error", priority: 1, errText: longErr, onchain: v2, entry: &entry, park: true},  // long error
		8142:  {status: "error", priority: 1, errText: "rpc: timed out", onchain: `{"v":1}`, park: true}, // v < 2
	}
	var fx []pscFixture
	for _, id := range pscListed {
		f, ok := special[id]
		if !ok {
			f = pscFixture{status: "error", priority: 1, onchain: v2, park: true,
				errText: fmt.Sprintf("USD price of WETH at +1d: eth_getLogs blocks 1-%d: request timed out", id)}
		}
		f.id = id
		fx = append(fx, f)
	}
	// Unlisted decoys.
	fx = append(fx,
		pscFixture{id: 9001, status: "error", priority: 1, errText: "rpc: timed out", onchain: v2},
		pscFixture{id: 9002, status: "gave_up", priority: 1, errText: "no trades around the call time yet", onchain: v2},
		pscFixture{id: 9003, status: "error", priority: 0, errText: "rpc: timed out", onchain: v2},
	)
	e0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC)
	var parked []int
	for i, f := range fx {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_calls (id, uuid, channel_id, channel_username, message_id, message_date,
			contract_address, chain, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 1, 'c', $1, $2, $3, 'evm', 'scanned', 't', 't')`,
			f.id, e0.Add(time.Duration(i)*time.Hour), fmt.Sprintf("0x%040x", f.id)); err != nil {
			t.Fatal(err)
		}
		if f.noRow {
			continue
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_tracking (call_id, contract_address, entry_at, priority, status,
			next_check_at, last_checked_at, attempts, error, onchain, entry_price_usd, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, '')::jsonb, $11, $12)`,
			f.id, fmt.Sprintf("0x%040x", f.id), e0.Add(time.Duration(i)*time.Hour), f.priority, f.status,
			time.Now().Add(-time.Duration(i+1)*time.Minute), updatedAt, 40+i, f.errText, f.onchain, f.entry, updatedAt); err != nil {
			t.Fatal(err)
		}
		if f.park {
			parked = append(parked, f.id)
		}
	}
	nPark := int64(len(parked)) // 17
	if nPark != 17 {
		t.Fatalf("fixtures park %d calls, want 17", nPark)
	}

	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	// A: what B would park. aSQL is A as a subquery (its final ";" dropped).
	aSQL := strings.TrimSuffix(strings.TrimSpace(pscRead(t, "A_count.sql")), ";")
	aWant := func(step string, canceled int64) {
		t.Helper()
		prfWant(t, step, prfGrid(t, conn, aSQL), map[string]int64{
			"TOTAL|calls B parks (listed, status error, priority 1) = expected_count for B_park.sql":      nPark,
			"check|listed calls B would NOT park (see their call rows)":                                   3,
			"check|calls to park whose error contains \"context canceled\" (B refuses them)":              canceled,
			"check|calls to park with an on-chain state version under 2 (a start would queue them again)": 1,
			"info|calls to park without an on-chain state yet":                                            1,
			"info|calls to park without an entry price":                                                   nPark - 1,
			"info|calls to park with a late entry price (the ML data leaves gave_up calls out)":           0,
			"info|every tracking row in status error, priority 1 (any call)":                              nPark + 1,
			"info|every tracking row in status error, priority 0 (any call)":                              2,
			"info|every tracking row in status gave_up (any call)":                                        1,
		})
		// The call rows (prfGrid keeps only one row per section|item):
		// one per listed call, a 1 in calls for each call B parks.
		var rows, parks int64
		if err := conn.QueryRow(ctx, `SELECT count(*), sum(calls) FROM (`+aSQL+`) a WHERE section = 'call'`).Scan(&rows, &parks); err != nil ||
			rows != int64(len(pscListed)) || parks != nPark {
			t.Errorf("%s: %d call rows, %d parked (%v), want %d, %d", step, rows, parks, err, len(pscListed), nPark)
		}
	}
	// The fate of the decoys and of one call to park.
	fate := func(id int) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, `SELECT item FROM (`+aSQL+`) a WHERE section = 'call' AND call_id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("A: call %d: %v", id, err)
		}
		return s
	}
	for id, want := range map[int]string{2920: "NOT parked: status is done", 2935: "NOT parked: priority is 0",
		10061: "NOT parked: no tracking row", 2344: "parked by B"} {
		if got := fate(id); got != want {
			t.Errorf("A: call %d: %q, want %q", id, got, want)
		}
	}

	before := prfSnapshot(t, st)

	// A "context canceled" error among the calls to park: A shows it, B
	// refuses, nothing changed.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET error = 'rpc: context canceled' WHERE call_id = 2344`); err != nil {
		t.Fatal(err)
	}
	aWant("A with context canceled", 1)
	withCanceled := prfSnapshot(t, st)
	if err := prfExec(t, conn, pscB(t, int(nPark))); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("B with context canceled: got %v, want the context canceled error", err)
	}
	if pscBakExists(t, st) {
		t.Fatal("B refused for context canceled but left a backup table")
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, withCanceled) {
		t.Fatal("a refused B changed tracking rows")
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET error = $2, updated_at = $3 WHERE call_id = $1`,
		2344, "USD price of WETH at +1d: eth_getLogs blocks 1-2344: request timed out", updatedAt); err != nil {
		t.Fatal(err)
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, before) {
		t.Fatal("fixture 2344 not put back")
	}
	aWant("A", 0)

	// The parked calls are in the tracker's queue now (next_check_at passed).
	due := pscDue(t, st)
	for _, id := range parked {
		if !due[id] {
			t.Errorf("before B: call %d not due, want due", id)
		}
	}

	// B with the EDIT line untouched, and with a wrong count: an error,
	// nothing changed, no backup.
	if err := prfExec(t, conn, pscRead(t, "B_park.sql")); err == nil || !strings.Contains(err.Error(), "set expected_count") {
		t.Fatalf("B with -1: got %v, want the expected_count error", err)
	}
	if err := prfExec(t, conn, pscB(t, 20)); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("the selection has %d call(s) but expected_count is 20", nPark)) {
		t.Fatalf("B with 20: got %v, want the count error", err)
	}
	if pscBakExists(t, st) {
		t.Fatal("a failed B left a backup table")
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, before) {
		t.Fatal("a failed B changed tracking rows")
	}

	// B with the right count.
	if err := prfExec(t, conn, pscB(t, int(nPark))); err != nil {
		t.Fatalf("B: %v", err)
	}
	g := prfGrid(t, conn, prfTail(t, pscRead(t, "B_park.sql")))
	prfWant(t, "B grid", g, map[string]int64{"TOTAL|calls parked (= expected_count)": nPark})
	var parkedAt time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT min(parked_at) FROM `+pscBak).Scan(&parkedAt); err != nil {
		t.Fatal(err)
	}
	afterB := prfSnapshot(t, st)
	for _, f := range fx {
		if f.noRow {
			continue
		}
		if !f.park {
			if afterB[f.id] != before[f.id] {
				t.Errorf("B changed call %d:\n got %s\nwant %s", f.id, afterB[f.id], before[f.id])
			}
			continue
		}
		status, errText, upd := pscRow(t, st, f.id)
		wantErr := pscPrefix + f.errText
		if f.id == 3127 {
			wantErr = pscPrefix + f.errText[:400] + " [...]"
		}
		if status != TrackGaveUp || errText == nil || *errText != wantErr || !upd.Equal(parkedAt) {
			t.Errorf("call %d after B: %s %q %v, want gave_up %q %v", f.id, status, strOrNil(errText), upd, wantErr, parkedAt)
		}
		if strings.Contains(*errText, "context canceled") {
			t.Errorf("call %d: parked error contains context canceled", f.id)
		}
		if got, w := pscWithout(t, st, afterB[f.id], "status", "error", "updated_at"),
			pscWithout(t, st, before[f.id], "status", "error", "updated_at"); got != w {
			t.Errorf("call %d: B changed more than status, error and updated_at:\n got %s\nwant %s", f.id, got, w)
		}
		var oldStatus string
		var oldErr *string
		var oldAttempts int
		var oldNext, oldUpd time.Time
		var oldLast *time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT old_status, old_error, old_attempts, old_next_check_at, old_last_checked_at,
			old_updated_at FROM `+pscBak+` WHERE call_id = $1`, f.id).Scan(&oldStatus, &oldErr, &oldAttempts, &oldNext, &oldLast,
			&oldUpd); err != nil {
			t.Fatalf("call %d: backup row: %v", f.id, err)
		}
		if oldStatus != TrackError || oldErr == nil || *oldErr != f.errText || !oldUpd.Equal(updatedAt) ||
			oldLast == nil || !oldLast.Equal(updatedAt) {
			t.Errorf("call %d: backup %s %q %v %v, want error %q %v %v", f.id, oldStatus, strOrNil(oldErr), oldLast, oldUpd,
				f.errText, updatedAt, updatedAt)
		}
	}
	var pk int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.table_constraints
		WHERE table_name = $1 AND constraint_type = 'PRIMARY KEY'`, pscBak).Scan(&pk); err != nil || pk != 1 {
		t.Errorf("backup primary key: %d (%v), want 1", pk, err)
	}

	// The tracker's horizon scan no longer hands them out; the decoys still.
	due = pscDue(t, st)
	for _, id := range parked {
		if due[id] {
			t.Errorf("after B: call %d is due, want not", id)
		}
	}
	for _, id := range []int{2935, 9001, 9003} {
		if !due[id] {
			t.Errorf("after B: decoy %d not due, want due", id)
		}
	}

	// B again: stops at CREATE TABLE, nothing changed.
	err = prfExec(t, conn, pscB(t, int(nPark)))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
		t.Fatalf("B again: got %v, want duplicate table (42P07)", err)
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, afterB) {
		t.Fatal("a second B changed tracking rows")
	}

	// C.
	cWant := func(step string, asB, beforeB, gaveUpSince, other, retry, dueNow, requeue, errorRows, gaveUpRows int64) {
		t.Helper()
		prfWant(t, step, prfGrid(t, conn, pscRead(t, "C_check.sql")), map[string]int64{
			"backup|calls B parked (rows in the backup table)":                                                    nPark,
			"state|as B left it: gave_up with B's error text, not written since":                                  asB,
			"state|as before B: old status, error, attempts, next_check_at and updated_at (restored by R)":        beforeB,
			"state|gave_up, but written since B (another error text or updated_at moved)":                         gaveUpSince,
			"state|anything else (another status, or the row is gone)":                                            other,
			"due|backed-up calls in a status the tracker retries (pending, tracking, no_pool, error)":             retry,
			"due|of those, due now (next_check_at at or before now)":                                              dueNow,
			"check|backed-up calls the next start would queue again (context canceled, or state version under 2)": requeue,
			"listed|listed calls B did not park (not in the backup table)":                                        3,
			"info|every tracking row in status error":                                                             errorRows,
			"info|every tracking row in status gave_up":                                                           gaveUpRows,
		})
	}
	cWant("C after B", nPark, 0, 0, 0, 0, 0, 1, 3, nPark+1)

	// The tracker's own passes over gave_up rows. MarkRepeatTracking leaves
	// first calls alone. -retry-no-pool -retry-gave-up WOULD take them back
	// (dry run here: nothing changes).
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 0 {
		t.Errorf("MarkRepeatTracking: %d (%v), want 0", n, err)
	}
	var sum RetrySummary
	if _, err := st.RetryNoPool(ctx, RetryFilter{GaveUp: true}, true, func(s RetrySummary) { sum = s }); err != nil {
		t.Fatal(err)
	}
	if got := int64(sum.ByStatus[TrackGaveUp]); got != nPark+1 {
		t.Errorf("-retry-gave-up dry run: %d gave_up rows, want %d (the parked ones and 9002)", got, nPark+1)
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, afterB) {
		t.Fatal("MarkRepeatTracking or a dry run changed tracking rows")
	}

	// A restart runs scoutanalytics.sql: the parked rows stay parked, except
	// 8142 (state version 1), which the v<2 re-queue makes pending.
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	afterStart := prfSnapshot(t, st)
	for id, js := range afterB {
		if id == 8142 {
			continue
		}
		if afterStart[id] != js {
			t.Errorf("a start changed call %d:\n got %s\nwant %s", id, afterStart[id], js)
		}
	}
	if s, _, _ := pscRow(t, st, 8142); s != TrackPending {
		t.Errorf("8142 after a start: %s, want pending (state version under 2)", s)
	}
	cWant("C after a start", nPark-1, 0, 0, 1, 1, 1, 0, 3, nPark)

	// R: the 16 rows still as B left them go back; 8142 is left as it is.
	r := pscRead(t, "R_rollback.sql")
	rGrid := func(step string, restored, already, other int64) {
		t.Helper()
		prfWant(t, step, prfGrid(t, conn, prfTail(t, r)), map[string]int64{
			"backed up by B": nPark,
			"restored now":   restored,
			"skipped: already as before B (restored by an earlier run of R)":                                already,
			"skipped: written since B (another status or error text, updated_at moved, or the row is gone)": other,
		})
	}
	if err := prfExec(t, conn, r); err != nil {
		t.Fatalf("R: %v", err)
	}
	rGrid("R", nPark-1, 0, 1)
	afterR := prfSnapshot(t, st)
	for id, js := range before {
		want := js
		if id == 8142 {
			want = afterStart[id]
		}
		if afterR[id] != want {
			t.Errorf("call %d after R:\n got %s\nwant %s", id, afterR[id], want)
		}
	}
	due = pscDue(t, st)
	for _, id := range parked {
		if !due[id] {
			t.Errorf("after R: call %d not due, want due", id)
		}
	}

	// R again changes nothing.
	if err := prfExec(t, conn, r); err != nil {
		t.Fatalf("R again: %v", err)
	}
	rGrid("R again", 0, nPark-1, 1)
	if got := prfSnapshot(t, st); !maps.Equal(got, afterR) {
		t.Fatal("R again changed tracking rows")
	}
	cWant("C after R", 0, nPark-1, 0, 1, nPark, nPark, 0, nPark+2, 1)

	// Nothing outside the listed calls was ever in the backup.
	var ids []int
	if err := st.Pool.QueryRow(ctx, `SELECT array_agg(call_id ORDER BY call_id) FROM `+pscBak).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	slices.Sort(parked)
	if !slices.Equal(ids, parked) {
		t.Errorf("backup ids %v, want %v", ids, parked)
	}

	// Backup dropped (README's last step): B parks the restored calls again
	// (8142 is pending now, not error: left out).
	if _, err := st.Pool.Exec(ctx, `DROP TABLE `+pscBak); err != nil {
		t.Fatal(err)
	}
	if got := prfGrid(t, conn, aSQL)["TOTAL|calls B parks (listed, status error, priority 1) = expected_count for B_park.sql"]; got != nPark-1 {
		t.Fatalf("A after R: TOTAL = %d, want %d", got, nPark-1)
	}
	if err := prfExec(t, conn, pscB(t, int(nPark-1))); err != nil {
		t.Fatalf("B after R: %v", err)
	}
	for _, id := range parked {
		want := TrackGaveUp
		if id == 8142 {
			want = TrackPending
		}
		if s, _, _ := pscRow(t, st, id); s != want {
			t.Errorf("call %d after the second B: %s, want %s", id, s, want)
		}
	}
}
