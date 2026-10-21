package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// hdbSeed: one call for the holder DB tests. Tracking false = no tracking row.
type hdbSeed struct {
	Msg      int
	CA       string // "" = a CA of its own
	Entry    time.Time
	Tracking bool
	Status   string  // tracking status; "" = done
	State    *string // onchain JSON; nil = NULL
	NoEntry  bool    // entry_price_usd NULL
	Checked  *time.Time
	AgeS     *int // scout_call_metrics.age_seconds; nil = no metrics row
	Update   bool // an update post (post_kind 'update')
}

func hdbCA(n int) string { return fmt.Sprintf("0x%040x", 0xa000+n) }

// hdbSeedCall inserts w and returns its call id.
func hdbSeedCall(t *testing.T, st *ScoutStore, w hdbSeed) int {
	t.Helper()
	ctx := context.Background()
	if w.CA == "" {
		w.CA = hdbCA(w.Msg)
	}
	id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 888, ChannelUsername: "scoutrobinhood", MessageID: w.Msg,
		MessageDate: w.Entry, ContractAddress: w.CA, Chain: "evm", Status: CallStatusBackfill})
	if err != nil {
		t.Fatal(err)
	}
	if w.Update {
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = 'update' WHERE id = $1`, *id); err != nil {
			t.Fatal(err)
		}
	}
	if w.AgeS != nil {
		if err := st.UpsertCallMetrics(ctx, *id, &CallMeta{AgeSeconds: w.AgeS}); err != nil {
			t.Fatal(err)
		}
	}
	if !w.Tracking {
		return *id
	}
	if err := st.EnsureTracking(ctx, *id, w.CA, w.Entry, 1, w.Entry.Add(100*ltDay)); err != nil {
		t.Fatal(err)
	}
	status := w.Status
	if status == "" {
		status = TrackDone
	}
	var entry any
	if !w.NoEntry {
		entry = 2.0
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = $2, price_unit = 'usd', entry_price_usd = $3,
		entry_late_price_usd = $3, onchain = $4::jsonb, latest_checked_at = $5 WHERE call_id = $1`,
		*id, status, entry, w.State, w.Checked); err != nil {
		t.Fatal(err)
	}
	return *id
}

func hdbMigrated(t *testing.T) *ScoutStore {
	t.Helper()
	st := testStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// hdbPool: the pool of hdbState.
const hdbPool = "0x00000000000000000000000000000000000000c7"

// hdbRecent, hdbOld: the latest-price cut-offs the lag tests pass.
const hdbRecent, hdbOld = 15 * time.Minute, 24 * time.Hour

// hdbState is a usable on-chain state at onchainStateVersion.
func hdbState() string { return ltStateJSON(hdbPool, 5000) }

// hdbV1State is hdbState at state version 1.
func hdbV1State() string {
	return strings.Replace(hdbState(), fmt.Sprintf(`"v":%d`, onchainStateVersion), `"v":1`, 1)
}

// hdbIDs lists the call ids of metas, in order.
func hdbIDs(metas []holderCallMeta) []int64 {
	var ids []int64
	for _, m := range metas {
		ids = append(ids, m.CallID)
	}
	return ids
}

// TestHolderCallMetas: every kind of call, read in the order asked, unknown
// ids in missing.
func TestHolderCallMetas(t *testing.T) {
	st := hdbMigrated(t)
	ctx := context.Background()
	entry := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	state, v1State := hdbState(), hdbV1State()
	wrongShape := `[1,2]` // valid JSON (jsonb takes it) that is not an onchainState
	full := hdbSeedCall(t, st, hdbSeed{Msg: 1, Entry: entry, Tracking: true, State: &state, AgeS: ip(300)})
	noTracking := hdbSeedCall(t, st, hdbSeed{Msg: 2, Entry: entry, AgeS: ip(60)})
	nullState := hdbSeedCall(t, st, hdbSeed{Msg: 3, Entry: entry, Tracking: true, AgeS: ip(90)})
	noMetrics := hdbSeedCall(t, st, hdbSeed{Msg: 4, Entry: entry, Tracking: true, State: &state})
	badState := hdbSeedCall(t, st, hdbSeed{Msg: 5, Entry: entry, Tracking: true, State: &wrongShape, AgeS: ip(1)})
	oldState := hdbSeedCall(t, st, hdbSeed{Msg: 6, Entry: entry, Tracking: true, State: &v1State, AgeS: ip(2)})
	unknown := int64(oldState + 1000)

	ids := []int64{int64(badState), unknown, int64(full), int64(nullState), int64(noTracking), int64(oldState), int64(noMetrics)}
	metas, missing, err := st.HolderCallMetas(ctx, ids)
	if err != nil {
		t.Fatalf("HolderCallMetas(%v): %v", ids, err)
	}
	if want := []int64{unknown}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	if order, want := hdbIDs(metas), []int64{int64(badState), int64(full), int64(nullState), int64(noTracking), int64(oldState), int64(noMetrics)}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v (input %v)", order, want, ids)
	}
	byID := map[int64]holderCallMeta{}
	for _, m := range metas {
		byID[m.CallID] = m
	}
	for _, c := range []struct {
		what    string
		id, msg int
		track   bool
		entryAt time.Time
		stateV  int // -1 = State nil
		age     *int
	}{
		{"tracking, state and metrics", full, 1, true, entry, onchainStateVersion, ip(300)},
		{"no tracking row", noTracking, 2, false, time.Time{}, -1, ip(60)},
		{"onchain NULL", nullState, 3, true, entry, -1, ip(90)},
		{"no metrics row", noMetrics, 4, true, entry, onchainStateVersion, nil},
		{"onchain that does not parse", badState, 5, true, entry, -1, ip(1)},
		{"an older state version is kept", oldState, 6, true, entry, 1, ip(2)},
	} {
		m := byID[int64(c.id)]
		if m.TrackingRow != c.track || !m.EntryAt.Equal(c.entryAt) {
			t.Errorf("%s: TrackingRow %v, EntryAt %v; want %v, %v", c.what, m.TrackingRow, m.EntryAt, c.track, c.entryAt)
		}
		if c.stateV < 0 && m.State != nil {
			t.Errorf("%s: State = %+v, want nil", c.what, m.State)
		}
		if c.stateV >= 0 && (m.State == nil || m.State.V != c.stateV) {
			t.Errorf("%s: State = %+v, want version %d", c.what, m.State, c.stateV)
		}
		if (m.PostedAgeS == nil) != (c.age == nil) || (c.age != nil && *m.PostedAgeS != *c.age) {
			t.Errorf("%s: PostedAgeS = %v, want %v", c.what, m.PostedAgeS, c.age)
		}
		if m.Token != hdbCA(c.msg) || m.Chain != "evm" {
			t.Errorf("%s: Token %q, Chain %q; want %q, evm", c.what, m.Token, m.Chain, hdbCA(c.msg))
		}
	}
	if m := byID[int64(full)]; m.State != nil && m.State.Pool != hdbPool {
		t.Errorf("full state pool = %q", m.State.Pool)
	}
}

// TestHolderCallMetasEmpty: no ids, no read.
func TestHolderCallMetasEmpty(t *testing.T) {
	st := hdbMigrated(t)
	metas, missing, err := st.HolderCallMetas(context.Background(), nil)
	if err != nil || metas != nil || missing != nil {
		t.Fatalf("HolderCallMetas(nil) = %v, %v, %v; want nil, nil, nil", metas, missing, err)
	}
}

// TestReadOnlyTxRejectsWrite: a write inside readOnlyTx fails with
// read_only_sql_transaction, and nothing is written.
func TestReadOnlyTxRejectsWrite(t *testing.T) {
	st := hdbMigrated(t)
	ctx := context.Background()
	err := st.readOnlyTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date,
			contract_address, chain, status, created_by, updated_by)
			VALUES (gen_random_uuid(), 1, 'x', 1, now(), '0x1', 'evm', 'queued', 't', 't')`)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("INSERT in readOnlyTx: err = %v, want SQLSTATE 25006 (read_only_sql_transaction)", err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("scout_calls rows after the failed INSERT = %d, %v; want 0", n, err)
	}
}

// TestHolderStateAndOrder: the pure helpers of HolderCallMetas (no database).
func TestHolderStateAndOrder(t *testing.T) {
	for _, c := range []struct {
		raw   string
		wantV int // -1 = nil
	}{{"", -1}, {"[1,2]", -1}, {`"x"`, -1}, {`{"v":"two"}`, -1}, {`{"v":1}`, 1}, {`{"v":2,"pool":"0xp"}`, 2}} {
		got := parseHolderState([]byte(c.raw))
		if (got == nil) != (c.wantV < 0) || (got != nil && got.V != c.wantV) {
			t.Errorf("parseHolderState(%q) = %+v, want version %d (-1 = nil)", c.raw, got, c.wantV)
		}
	}
	byID := map[int64]holderCallMeta{3: {CallID: 3}, 1: {CallID: 1}}
	metas, missing := orderHolderCallMetas([]int64{3, 9, 1, 3}, byID)
	if order := hdbIDs(metas); !reflect.DeepEqual(order, []int64{3, 1, 3}) || !reflect.DeepEqual(missing, []int64{9}) {
		t.Fatalf("orderHolderCallMetas([3 9 1 3]) = %v, missing %v; want [3 1 3], [9]", order, missing)
	}
}

// lagRow: a latest-price row for TestLatestPriceLag, relative to the test's now.
type lagRow struct {
	Age     time.Duration // call age
	Checked time.Duration // how long ago latest_checked_at; 0 = NULL
	Rugged  bool          // rug_block in the state
	N       int           // copies (0 = 1)
}

// TestLatestPriceLag: each row is measured against its own cut-off (young:
// recent; old or rugged: old), never-refreshed rows are left out, and the
// 95th percentile ignores a few stuck rows but not many overdue ones.
func TestLatestPriceLag(t *testing.T) {
	state := hdbState()
	rugged := strings.Replace(state, "{", `{"rug_block":4000,`, 1)
	young, aged := 5*ltDay, 40*ltDay
	for _, c := range []struct {
		what string
		rows []lagRow
		want time.Duration
	}{
		{"no rows", nil, 0},
		{"young: the recent cut-off", []lagRow{{Age: young, Checked: time.Hour}}, 45 * time.Minute},
		{"old: the old cut-off", []lagRow{{Age: aged, Checked: 30 * time.Hour}}, 6 * time.Hour},
		{"young but rugged: the old cut-off", []lagRow{{Age: young, Checked: 30 * time.Hour, Rugged: true}}, 6 * time.Hour},
		{"old and rugged: the old cut-off", []lagRow{{Age: aged, Checked: 30 * time.Hour, Rugged: true}}, 6 * time.Hour},
		{"not due yet: 0", []lagRow{{Age: young, Checked: 5 * time.Minute}}, 0},
		{"never refreshed rows are left out", []lagRow{{Age: young, N: 30}, {Age: young, Checked: time.Hour}}, 45 * time.Minute},
		{"a few stuck rows do not move it", []lagRow{{Age: young, Checked: 20 * time.Minute, N: 40}, {Age: young, Checked: 10 * ltDay, N: 2}}, 5 * time.Minute},
		{"many overdue rows do", []lagRow{{Age: young, Checked: 20 * time.Minute, N: 40}, {Age: young, Checked: 3 * time.Hour, N: 10}}, 2*time.Hour + 45*time.Minute},
	} {
		t.Run(c.what, func(t *testing.T) {
			st := hdbMigrated(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			msg := 0
			for _, r := range c.rows {
				for range max(r.N, 1) {
					msg++
					w := hdbSeed{Msg: msg, Entry: now.Add(-r.Age), Tracking: true, State: &state}
					if r.Rugged {
						w.State = &rugged
					}
					if r.Checked != 0 {
						at := now.Add(-r.Checked)
						w.Checked = &at
					}
					hdbSeedCall(t, st, w)
				}
			}
			got, err := st.LatestPriceLag(ctx, now, hdbRecent, hdbOld, latestRecentAge)
			if err != nil || got != c.want {
				t.Fatalf("LatestPriceLag = %v, %v; want %v (rows %+v)", got, err, c.want, c.rows)
			}
		})
	}
}

// TestLatestPriceLagSameRowsAsDue: rows the latest-price pass never refreshes
// (latestDueSQL leaves them out) do not count, however overdue; the one
// qualifying row does, and CountDueLatest agrees on both.
func TestLatestPriceLagSameRowsAsDue(t *testing.T) {
	st := hdbMigrated(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	state, v1State := hdbState(), hdbV1State()
	noPool := fmt.Sprintf(`{"v":%d,"entry_price_q":2}`, onchainStateVersion)
	stuck := now.Add(-10 * ltDay)
	young := now.Add(-5 * ltDay)
	for _, w := range []hdbSeed{
		{Msg: 1, Entry: young, Tracking: true, State: &state, Checked: &stuck, Status: TrackPending},
		{Msg: 2, Entry: young, Tracking: true, State: &state, Checked: &stuck, Status: TrackError},
		{Msg: 3, Entry: young, Tracking: true, State: &state, Checked: &stuck, NoEntry: true},
		{Msg: 4, Entry: young, Tracking: true, Checked: &stuck}, // onchain NULL
		{Msg: 5, Entry: young, Tracking: true, State: &v1State, Checked: &stuck},
		{Msg: 6, Entry: young, Tracking: true, State: &noPool, Checked: &stuck},
		{Msg: 7, Entry: young, Tracking: true, State: &state, Checked: &stuck, Update: true},
		// a token's first call without a tracking row, then its later call
		{Msg: 8, Entry: young, CA: hdbCA(100), Tracking: false},
		{Msg: 9, Entry: young.Add(time.Hour), CA: hdbCA(100), Tracking: true, State: &state, Checked: &stuck},
	} {
		hdbSeedCall(t, st, w)
	}
	check := func(what string, wantLag time.Duration, wantDue int) {
		t.Helper()
		lag, err := st.LatestPriceLag(ctx, now, hdbRecent, hdbOld, latestRecentAge)
		if err != nil || lag != wantLag {
			t.Errorf("%s: LatestPriceLag = %v, %v; want %v", what, lag, err, wantLag)
		}
		n, err := st.CountDueLatest(ctx, now, hdbRecent, hdbOld, latestRecentAge)
		if err != nil || n != wantDue {
			t.Errorf("%s: CountDueLatest = %d, %v; want %d", what, n, err, wantDue)
		}
	}
	check("only rows the pass never refreshes", 0, 0)
	checked := now.Add(-time.Hour)
	hdbSeedCall(t, st, hdbSeed{Msg: 10, Entry: young, Tracking: true, State: &state, Checked: &checked})
	check("plus one qualifying row", 45*time.Minute, 1)
}

// TestReadOnlyTxMode: fn runs in a read-only, repeatable-read transaction, and
// its error comes back as is (not wrapped).
func TestReadOnlyTxMode(t *testing.T) {
	st := hdbMigrated(t)
	ctx := context.Background()
	var readOnly, isolation string
	err := st.readOnlyTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation)
	})
	if err != nil || readOnly != "on" || isolation != "repeatable read" {
		t.Fatalf("inside readOnlyTx: transaction_read_only %q, transaction_isolation %q, err %v; want on, repeatable read, nil",
			readOnly, isolation, err)
	}
	sentinel := errors.New("fn failed")
	if err := st.readOnlyTx(ctx, func(pgx.Tx) error { return sentinel }); err != sentinel {
		t.Fatalf("readOnlyTx(fn returning %v) = %v, want the same error unwrapped", sentinel, err)
	}
}

// TestHolderReadsCancelled: a cancelled context fails each read with a wrapped
// context.Canceled, and the read-only helper is not entered.
func TestHolderReadsCancelled(t *testing.T) {
	st := hdbMigrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := st.readOnlyTx(ctx, func(pgx.Tx) error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || ran {
		t.Errorf("readOnlyTx(cancelled) = %v, fn ran %v; want context.Canceled, false", err, ran)
	}
	if _, _, err := st.HolderCallMetas(ctx, []int64{1}); !errors.Is(err, context.Canceled) {
		t.Errorf("HolderCallMetas(cancelled) err = %v, want context.Canceled", err)
	}
	if _, err := st.LatestPriceLag(ctx, time.Now(), hdbRecent, hdbOld, latestRecentAge); !errors.Is(err, context.Canceled) {
		t.Errorf("LatestPriceLag(cancelled) err = %v, want context.Canceled", err)
	}
}
