package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// ops/2026-10-20-price-rug-fill: the one-off fill that gives past price-rule
// rugged calls rug_at_kind 'price' and the tracker's approximate rug_at
// (priceRugAtSQL). The owner runs the files by hand in pgAdmin; these tests
// run them as they are against the throwaway database.
// ---------------------------------------------------------------------------

const (
	prfDir = "ops/2026-10-20-price-rug-fill"
	prfBak = "scout_call_tracking_bak_price_rug_fill_20261020"

	// The grid keys of A's (and C's) STOP row and A's base row.
	prfOtherTotal = "other|total (on-chain, no rug block, last price at or above 5 % of the entry, any status)"
	prfBaseTotal  = "base|every rugged call with no rug time (= TOTAL + other total + the left out rows above)"
)

// prfRead returns an ops file of the folder.
func prfRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(prfDir), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var prfSpace = regexp.MustCompile(`\s+`)

// prfNorm collapses every run of white space (CRLF and tabs included) into
// one space.
func prfNorm(s string) string { return prfSpace.ReplaceAllString(s, " ") }

// The files that compute the time (A, B, C) hold priceRugAtSQL as it is, with
// only $1 -> t.call_id and $2 -> t.entry_price_usd: the fill gives past calls
// the same time as the tracker gives new ones.
func TestPriceRugFillCopiesPriceRugAtSQL(t *testing.T) {
	want := prfNorm(strings.NewReplacer("$1", "t.call_id", "$2", "t.entry_price_usd").Replace(priceRugAtSQL))
	for _, name := range []string{"A_count.sql", "B_fill.sql", "C_check.sql"} {
		if got := prfNorm(prfRead(t, name)); !strings.Contains(got, want) {
			t.Errorf("%s does not contain priceRugAtSQL with $1 -> t.call_id, $2 -> t.entry_price_usd:\n%s", name, want)
		}
	}
	b := prfRead(t, "B_fill.sql")
	if n := strings.Count(b, "<<< EDIT"); n != 1 {
		t.Errorf("B_fill.sql has %d EDIT lines, want 1", n)
	}
	for _, name := range []string{"A_count.sql", "C_check.sql", "R_rollback.sql"} {
		if strings.Contains(prfRead(t, name), "<<< EDIT") {
			t.Errorf("%s has an EDIT line, want none", name)
		}
	}
}

// prfFixture is one tracking row of the fill test.
type prfFixture struct {
	name     string
	status   string
	rugged   bool
	entry    *float64 // nil = NULL
	source   string   // "" = NULL
	current  *float64 // nil = NULL
	onchain  string   // "" = NULL
	kind     string   // a rug time stored before B ("" = none)
	rets     []prReturn
	candles  []prCandle
	fill     bool          // B fills it
	wantSrc  string        // its src in the backup
	wantTime time.Duration // its time after the post
}

// prfHour is 00:00 + k hours (the k-th UTC hour bucket after 00:00) as an
// offset from the fixtures' entry at 00:30.
func prfHour(k int) time.Duration { return time.Duration(k)*time.Hour - 30*time.Minute }

// prfShape is the price-rule shape: done, rugged, on-chain, entry 100, last
// price 2 (under 5 % of the entry), a candle closing under 5 % in the 5th
// hour after 00:00.
func prfShape(name string) prfFixture {
	entry := 100.0
	return prfFixture{name: name, status: "done", rugged: true, entry: &entry, source: "onchain-v3", current: prfPrice(2),
		onchain: `{"v":2,"entry_block":100,"scan_block":6000}`,
		rets:    []prReturn{{"1h", time.Hour, "done", 80}, {"1d", 24 * time.Hour, "done", 3}, {"7d", 7 * 24 * time.Hour, "done", 2}},
		candles: []prCandle{{3600, prfHour(5), 4}}}
}

// prfPrice is a fixture price (a pointer: nil stands for NULL).
func prfPrice(v float64) *float64 { return &v }

// prfRugAtIs reports whether a row's rug time is wantAt with kind wantKind.
func prfRugAtIs(at *time.Time, kind *string, wantAt time.Time, wantKind string) bool {
	return at != nil && at.Equal(wantAt) && kind != nil && *kind == wantKind
}

// prfSnapshot returns every tracking row as JSON text, by call.
func prfSnapshot(t *testing.T, st *ScoutStore) map[int]string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `SELECT call_id, to_jsonb(t)::text FROM scout_call_tracking t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var js string
		if err := rows.Scan(&id, &js); err != nil {
			t.Fatal(err)
		}
		out[id] = js
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// prfWithoutRug returns a row's JSON without rug_at and rug_at_kind.
func prfWithoutRug(t *testing.T, st *ScoutStore, js string) string {
	t.Helper()
	var out string
	if err := st.Pool.QueryRow(context.Background(), `SELECT ($1::jsonb - 'rug_at' - 'rug_at_kind')::text`, js).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// prfGrid runs a read-only script (or a script's final SELECT) on conn and
// returns its "calls" column by "section|item" (or by "item" without a
// section column).
func prfGrid(t *testing.T, conn *pgxpool.Conn, sql string) map[string]int64 {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	col := map[string]int{}
	for i, f := range rows.FieldDescriptions() {
		col[f.Name] = i
	}
	out := map[string]int64{}
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprint(v[col["item"]])
		if i, ok := col["section"]; ok {
			key = fmt.Sprint(v[i]) + "|" + key
		}
		n, ok := v[col["calls"]].(int64) // count(*) is bigint
		if !ok {
			t.Fatalf("row %q: calls is %T (%v), want int64", key, v[col["calls"]], v[col["calls"]])
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// prfSection sums a grid's rows of one section.
func prfSection(g map[string]int64, section string) int64 {
	var n int64
	for k, v := range g {
		if strings.HasPrefix(k, section+"|") {
			n += v
		}
	}
	return n
}

// prfExec runs a writing script as the owner does in pgAdmin (all of it, in
// one go: simple protocol, no arguments). On an error it runs ROLLBACK, as the
// README tells, and returns the error.
func prfExec(t *testing.T, conn *pgxpool.Conn, sql string) error {
	t.Helper()
	ctx := context.Background()
	_, err := conn.Exec(ctx, sql)
	if err != nil {
		if _, rbErr := conn.Exec(ctx, "ROLLBACK"); rbErr != nil {
			t.Fatalf("ROLLBACK after %v: %v", err, rbErr)
		}
	}
	return err
}

// prfTail is a script's final result SELECT (after its last COMMIT).
func prfTail(t *testing.T, sql string) string {
	t.Helper()
	i := strings.LastIndex(sql, "COMMIT;")
	if i < 0 {
		t.Fatal("no COMMIT in the script")
	}
	return sql[i+len("COMMIT;"):]
}

// prfB is B_fill.sql with expected_count set to n.
func prfB(t *testing.T, n int) string {
	t.Helper()
	b := prfRead(t, "B_fill.sql")
	const edit = "-1 AS expected_count"
	if n := strings.Count(b, edit); n != 1 {
		t.Fatalf("B_fill.sql: %q found %d times, want once (the EDIT line)", edit, n)
	}
	return strings.Replace(b, edit, fmt.Sprintf("%d AS expected_count", n), 1)
}

func prfBakExists(t *testing.T, st *ScoutStore) bool {
	t.Helper()
	var ok bool
	if err := st.Pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, prfBak).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func prfDropBak(t *testing.T, st *ScoutStore) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+prfBak); err != nil {
		t.Fatal(err)
	}
}

// prfWant checks grid rows by key.
func prfWant(t *testing.T, step string, g map[string]int64, want map[string]int64) {
	t.Helper()
	for k, w := range want {
		if got, ok := g[k]; !ok || got != w {
			t.Errorf("%s: row %q = %d (present %v), want %d; grid %v", step, k, got, ok, w, g)
		}
	}
}

// The ops scripts end to end: A counts, B fills exactly the price-rule rows
// with the tracker's time (and refuses a wrong count or a second run), C
// shows the state, R restores only rows still as B left them, and B fills
// again after the backup is dropped.
func TestPriceRugFillOps(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	prfDropBak(t, st)
	t.Cleanup(func() {
		// st is closed after this (cleanups run last-registered first).
		if _, err := st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+prfBak); err != nil {
			t.Error(err)
		}
	})
	e0 := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	drainedAt := e0.Add(36 * time.Hour)

	// Each fixture is prfShape(name) with edit applied.
	var fx []prfFixture
	add := func(name string, edit func(*prfFixture)) {
		f := prfShape(name)
		edit(&f)
		fx = append(fx, f)
	}
	add("P1 candle", func(f *prfFixture) { f.fill, f.wantSrc, f.wantTime = true, "candle", prfHour(6) })
	add("P2 horizon", func(f *prfFixture) {
		f.rets = []prReturn{{"1h", time.Hour, "done", 80}, {"1d", 24 * time.Hour, "done", 4}, {"7d", 7 * 24 * time.Hour, "done", 2}}
		f.candles = []prCandle{{3600, prfHour(2), 50}}
		f.fill, f.wantSrc, f.wantTime = true, "horizon", 24*time.Hour
	})
	add("P3 last_due", func(f *prfFixture) {
		f.rets = []prReturn{{"1h", time.Hour, "done", 80}, {"1d", 24 * time.Hour, "done", 50}, {"7d", 7 * 24 * time.Hour, "done", 20}}
		f.candles = []prCandle{{3600, prfHour(2), 50}}
		f.fill, f.wantSrc, f.wantTime = true, "last_due", 7*24*time.Hour
	})
	add("P4 a close exactly at 5 % is ignored, 4.99 counts", func(f *prfFixture) {
		f.candles = []prCandle{{3600, prfHour(5), 5}, {3600, prfHour(7), 4.99}}
		f.fill, f.wantSrc, f.wantTime = true, "candle", prfHour(8)
	})
	add("no done horizon", func(f *prfFixture) { f.rets = []prReturn{{"1h", time.Hour, "no_data", 1}} })
	add("other: last price 7x the entry", func(f *prfFixture) { f.current = prfPrice(700) })
	// Like calls 9947 and 9948 on prod: the "other" shape, now a repeat row.
	add("other shape, status repeat", func(f *prfFixture) { f.status, f.current = "repeat", prfPrice(700) })
	// A NULL last price is not "other": its own left-out row.
	add("other shape, current price NULL", func(f *prfFixture) { f.current = nil })
	add("GeckoTerminal", func(f *prfFixture) { f.source, f.onchain = "hour", "" })
	add("already event", func(f *prfFixture) { f.kind = rugAtEvent })
	add("already price, another time", func(f *prfFixture) { f.kind = rugAtPrice })
	add("rug block, no rug time", func(f *prfFixture) {
		f.onchain = `{"v":2,"entry_block":100,"scan_block":6000,"rug_block":5000}`
	})
	add("NULL entry price", func(f *prfFixture) { f.entry = nil })
	add("status repeat", func(f *prfFixture) { f.status = "repeat" })
	add("not rugged", func(f *prfFixture) { f.rugged = false })

	ids := make([]int, len(fx))
	byName := map[string]int{}
	updatedAt := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for i, f := range fx {
		ids[i] = prCall(t, st, 6000+i, e0)
		byName[f.name] = ids[i]
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = $2, rugged = $3, entry_price_usd = $4,
			entry_price_source = NULLIF($5, ''), current_price_usd = $6, onchain = NULLIF($7, '')::jsonb, updated_at = $8
			WHERE call_id = $1`, ids[i], f.status, f.rugged, f.entry, f.source, f.current, f.onchain, updatedAt); err != nil {
			t.Fatal(err)
		}
		if f.kind != "" {
			if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET rug_at = $2, rug_at_kind = $3 WHERE call_id = $1`,
				ids[i], drainedAt, f.kind); err != nil {
				t.Fatal(err)
			}
		}
		prSeed(t, st, ids[i], e0, f.rets, f.candles)
	}

	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	// A: what B would fill.
	a := prfGrid(t, conn, prfRead(t, "A_count.sql"))
	if got := prfSection(a, "TOTAL"); got != 4 {
		t.Fatalf("A: TOTAL = %d, want 4; grid %v", got, a)
	}
	prfWant(t, "A", a, map[string]int64{
		"selected|candle":   2,
		"selected|horizon":  1,
		"selected|last_due": 1,
		"check|selected with a time before the post (rug_at < entry_at)": 0,
		"other|status done":                                          1, // last price 7x the entry
		"other|status not done":                                      1, // the repeat row
		prfOtherTotal:                                                2,
		"left out|last price NULL":                                   1,
		"left out|entry price NULL or 0":                             1,
		"left out|on-chain state version under 2":                    0,
		"left out|price rule, but status is not done":                1,
		"left out|price rule, but no done horizon (no time to give)": 1,
		"left out|GeckoTerminal-priced (not on-chain) or no on-chain state, any status": 1,
		"left out|a rug block in the state, any status":                                 1,
		prfBaseTotal: 12,
		"left out|rugged with only one of rug_at / rug_at_kind set (not in base)": 0,
		"kinds|event":         1,
		"kinds|price":         1,
		"kinds|(no rug time)": 12,
	})
	// Every rugged call without a rug time is in exactly one visible row: the
	// base row is TOTAL + other total + the left-out rows inside the base.
	if sum := prfSection(a, "TOTAL") + a[prfOtherTotal] + prfSection(a, "left out") -
		a["left out|rugged with only one of rug_at / rug_at_kind set (not in base)"]; sum != a[prfBaseTotal] {
		t.Errorf("A: TOTAL + other total + left out = %d, want the base row %d; grid %v", sum, a[prfBaseTotal], a)
	}
	if done, notDone := a["other|status done"], a["other|status not done"]; done+notDone != a[prfOtherTotal] {
		t.Errorf("A: other done %d + not done %d, want other total %d", done, notDone, a[prfOtherTotal])
	}

	before := prfSnapshot(t, st)

	// B with the EDIT line untouched: an error, nothing changed, no backup.
	if err := prfExec(t, conn, prfRead(t, "B_fill.sql")); err == nil || !strings.Contains(err.Error(), "set expected_count") {
		t.Fatalf("B with -1: got %v, want the expected_count error", err)
	}
	if prfBakExists(t, st) {
		t.Fatal("B with -1 left a backup table")
	}
	// A wrong count: the same.
	if err := prfExec(t, conn, prfB(t, 5)); err == nil || !strings.Contains(err.Error(), "the selection has 4 call(s) but expected_count is 5") {
		t.Fatalf("B with 5: got %v, want the count error", err)
	}
	if prfBakExists(t, st) {
		t.Fatal("B with a wrong count left a backup table")
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, before) {
		t.Fatal("a failed B changed tracking rows")
	}

	// B with 4.
	if err := prfExec(t, conn, prfB(t, 4)); err != nil {
		t.Fatalf("B with 4: %v", err)
	}
	g := prfGrid(t, conn, prfTail(t, prfRead(t, "B_fill.sql")))
	prfWant(t, "B grid", g, map[string]int64{"filled|candle": 2, "filled|horizon": 1, "filled|last_due": 1,
		"TOTAL|calls filled (= expected_count)": 4})
	afterB := prfSnapshot(t, st)
	for i, f := range fx {
		id := ids[i]
		at, kind := tmRugAt(t, st, id)
		// Every row B must not fill (the "other" rows of any status, the NULL
		// last price and every left-out row) is byte for byte as before B.
		if !f.fill {
			if afterB[id] != before[id] {
				t.Errorf("B changed %s:\n got %s\nwant %s", f.name, afterB[id], before[id])
			}
			continue
		}
		want, src, err := st.PriceRugAt(ctx, id, *f.entry)
		if err != nil {
			t.Fatal(err)
		}
		if src != f.wantSrc || !want.Equal(e0.Add(f.wantTime)) {
			t.Fatalf("%s: fixture gives %v %q, want %v %q", f.name, want, src, e0.Add(f.wantTime), f.wantSrc)
		}
		if !prfRugAtIs(at, kind, want, rugAtPrice) {
			t.Errorf("%s: after B got %v %v, want %v price (PriceRugAt)", f.name, at, strOrNil(kind), want)
		}
		if got, w := prfWithoutRug(t, st, afterB[id]), prfWithoutRug(t, st, before[id]); got != w {
			t.Errorf("%s: B changed more than rug_at and rug_at_kind:\n got %s\nwant %s", f.name, got, w)
		}
		var bakSrc string
		var bakAt time.Time
		var oldAt *time.Time
		var oldKind *string
		var oldUpd time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT src, new_rug_at, old_rug_at, old_rug_at_kind, old_updated_at FROM `+prfBak+
			` WHERE call_id = $1`, id).Scan(&bakSrc, &bakAt, &oldAt, &oldKind, &oldUpd); err != nil {
			t.Fatalf("%s: backup row: %v", f.name, err)
		}
		if bakSrc != f.wantSrc || !bakAt.Equal(want) || oldAt != nil || oldKind != nil || !oldUpd.Equal(updatedAt) {
			t.Errorf("%s: backup row %q %v %v %v %v, want %q %v <nil> <nil> %v", f.name, bakSrc, bakAt, oldAt, strOrNil(oldKind), oldUpd,
				f.wantSrc, want, updatedAt)
		}
	}
	var pk int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.table_constraints
		WHERE table_name = $1 AND constraint_type = 'PRIMARY KEY'`, prfBak).Scan(&pk); err != nil || pk != 1 {
		t.Errorf("backup primary key: %d (%v), want 1", pk, err)
	}

	// B again: stops at CREATE TABLE, nothing changed.
	err = prfExec(t, conn, prfB(t, 4))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
		t.Fatalf("B again: got %v, want duplicate table (42P07)", err)
	}
	if got := prfSnapshot(t, st); !maps.Equal(got, afterB) {
		t.Fatal("a second B changed tracking rows")
	}

	// C after B.
	cWant := func(step string, asB, drained, cleared, selection, priceKinds, eventKinds, noTime int64) {
		t.Helper()
		c := prfGrid(t, conn, prfRead(t, "C_check.sql"))
		prfWant(t, step, c, map[string]int64{
			"backup|calls B filled (rows in the backup table)":                            4,
			"state|as B left it: 'price' with B's time":                                   asB,
			"state|drained: event, at_call or detected (found by the tracker later)":      drained,
			"state|no rug time (restored by R, or its state started again)":               cleared,
			"state|anything else (another time or kind, or the row is gone)":              0,
			"info|backed-up calls tracked again since B (updated_at moved)":               0,
			"sanity|as B left it, but the time is after the last done horizon's due time": 0,
			"sanity|as B left it, but the time is before the post (rug_at < entry_at)":    0,
			"selection|calls B would select now (A_count.sql \"selected\")":               selection,
			"other|status done":     1,
			"other|status not done": 1,
			prfOtherTotal:           2,
			"left out|last price NULL (A_count.sql \"left out\")": 1,
			"kinds|price":         priceKinds,
			"kinds|event":         eventKinds,
			"kinds|(no rug time)": noTime,
		})
	}
	cWant("C after B", 4, 0, 0, 0, 5, 1, 8)

	// The tracker finds P1's pool drained later: its time replaces 'price'.
	p1 := byName["P1 candle"]
	if err := st.SetRugAt(ctx, p1, drainedAt, rugAtEvent); err != nil {
		t.Fatal(err)
	}
	// R: P2-P4 back to no rug time, P1 keeps its drained time.
	r := prfRead(t, "R_rollback.sql")
	if err := prfExec(t, conn, r); err != nil {
		t.Fatalf("R: %v", err)
	}
	rGrid := func(step string, restored, already, drained, other int64) {
		t.Helper()
		prfWant(t, step, prfGrid(t, conn, prfTail(t, r)), map[string]int64{
			"backed up by B": 4,
			"restored now":   restored,
			"skipped: already without a rug time (restored by an earlier run of R, or its state started again)": already,
			"skipped: a drained kind replaced B's time (event, at_call, detected)":                              drained,
			"skipped: anything else (tracked again since B, another time or kind, or the row is gone)":          other,
		})
	}
	rGrid("R", 3, 0, 1, 0)
	afterR := prfSnapshot(t, st)
	for i, f := range fx {
		id := ids[i]
		if id == p1 {
			if at, kind := tmRugAt(t, st, id); !prfRugAtIs(at, kind, drainedAt, rugAtEvent) {
				t.Errorf("%s: after R got %v %v, want %v event (kept)", f.name, at, strOrNil(kind), drainedAt)
			}
			continue
		}
		if afterR[id] != before[id] {
			t.Errorf("%s: after R\n got %s\nwant %s (as before B)", f.name, afterR[id], before[id])
		}
	}
	// R again changes nothing.
	if err := prfExec(t, conn, r); err != nil {
		t.Fatalf("R again: %v", err)
	}
	rGrid("R again", 0, 3, 1, 0)
	if got := prfSnapshot(t, st); !maps.Equal(got, afterR) {
		t.Fatal("R again changed tracking rows")
	}
	cWant("C after R", 0, 1, 3, 3, 1, 2, 11)

	// Backup dropped (README's last step): B fills P2-P4 again.
	prfDropBak(t, st)
	if got := prfSection(prfGrid(t, conn, prfRead(t, "A_count.sql")), "TOTAL"); got != 3 {
		t.Fatalf("A after R: TOTAL = %d, want 3", got)
	}
	if err := prfExec(t, conn, prfB(t, 3)); err != nil {
		t.Fatalf("B with 3: %v", err)
	}
	for i, f := range fx {
		if !f.fill || ids[i] == p1 {
			continue
		}
		want := e0.Add(f.wantTime)
		if at, kind := tmRugAt(t, st, ids[i]); !prfRugAtIs(at, kind, want, rugAtPrice) {
			t.Errorf("%s: after the second fill got %v %v, want %v price", f.name, at, strOrNil(kind), want)
		}
	}

	// R skips a row the tracker rewrote since B (updated_at moved), even with
	// B's time still on it.
	p3 := byName["P3 last_due"]
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET updated_at = now() WHERE call_id = $1`, p3); err != nil {
		t.Fatal(err)
	}
	if err := prfExec(t, conn, r); err != nil {
		t.Fatalf("R after the second fill: %v", err)
	}
	prfWant(t, "R after the second fill", prfGrid(t, conn, prfTail(t, r)), map[string]int64{
		"backed up by B": 3, "restored now": 2,
		"skipped: anything else (tracked again since B, another time or kind, or the row is gone)": 1,
	})
	if _, kind := tmRugAt(t, st, p3); kind == nil || *kind != rugAtPrice {
		t.Errorf("P3 after R: kind %v, want price (rewritten since B: left as is)", strOrNil(kind))
	}
	for _, name := range []string{"P2 horizon", "P4 a close exactly at 5 % is ignored, 4.99 counts"} {
		if at, kind := tmRugAt(t, st, byName[name]); at != nil || kind != nil {
			t.Errorf("%s after R: got %v %v, want no rug time", name, at, strOrNil(kind))
		}
	}
}
