package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// rescanSeed is a scout_calls row (with a tracking row when track is set).
type rescanSeed struct {
	msg      int
	ca       string
	at       time.Time
	status   string
	postKind string // "" = call
	track    bool
	rugged   *bool
	latest   *float64
}

func seedRescanCall(t *testing.T, st *ScoutStore, s rescanSeed) int {
	t.Helper()
	ctx := context.Background()
	kind := s.postKind
	if kind == "" {
		kind = PostKindCall
	}
	var id int
	if err := st.Pool.QueryRow(ctx, `INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date,
		contract_address, chain, status, post_kind, created_by, updated_by)
		VALUES ($1, 777, 'scoutrobinhood', $2, $3, $4, 'evm', $5, $6, 'test', 'test') RETURNING id`,
		uuid.New(), s.msg, s.at, s.ca, s.status, kind).Scan(&id); err != nil {
		t.Fatalf("seed call %d: %v", s.msg, err)
	}
	if s.track {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_tracking (call_id, contract_address, entry_at, status, next_check_at,
			rugged, latest_return_pct, entry_price_usd, price_unit)
			VALUES ($1, $2, $3, 'done', $3, $4, $5, 0.001, 'usd')`, id, s.ca, s.at, s.rugged, s.latest); err != nil {
			t.Fatalf("seed tracking %d: %v", s.msg, err)
		}
	}
	return id
}

// seedInvestigation stores one investigation (completed clean unless status says otherwise).
func seedInvestigation(t *testing.T, st *ScoutStore, tool int, callID *int, ca string, at time.Time, status, kind, level string) int {
	t.Helper()
	if level == "" {
		level = levelClean
	}
	r := &ScoutInvestigation{CallID: callID, ToolID: tool, ContractAddress: ca, RequestText: "/scan " + ca,
		RequestedAt: at, CompletedAt: &at, Status: status, VerdictLevel: level, ScanKind: kind}
	id, err := st.InsertScoutInvestigation(context.Background(), r)
	if err != nil {
		t.Fatalf("seed investigation of %s: %v", ca, err)
	}
	return *id
}

func ip(v int) *int { return &v }

// datasetColumns lists the columns of scout_call_dataset_v in order.
func datasetColumns(t *testing.T, st *ScoutStore) string {
	t.Helper()
	var cols string
	if err := st.Pool.QueryRow(context.Background(), `SELECT string_agg(column_name || ':' || data_type, ',' ORDER BY ordinal_position)
		FROM information_schema.columns WHERE table_name = 'scout_call_dataset_v'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	return cols
}

// A database made by the schema before scan_kind (the column, its index and
// the views as they were) is upgraded in place, twice without error; old rows
// read as live; the dataset view's columns do not change.
func TestRescanSchemaUpgrade(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("fresh migrate %d: %v", i+1, err)
		}
	}
	before := datasetColumns(t, st)
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	ca := "0x1111111111111111111111111111111111111111"
	call := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: time.Now().Add(-time.Hour), status: CallStatusScanned})
	seedInvestigation(t, st, perc, &call, ca, time.Now().Add(-time.Hour), investigationCompleted, "", "")
	// back to the old shape: no column, no index (the views go with the column)
	if _, err := st.Pool.Exec(ctx, `DROP VIEW scout_call_predictions_v, scout_call_dataset_v, scout_investigations_v;
		DROP INDEX scout_investigations_rescan_idx; ALTER TABLE scout_investigations DROP COLUMN scan_kind`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("upgrade migrate %d: %v", i+1, err)
		}
	}
	var kind, viewKind string
	if err := st.Pool.QueryRow(ctx, `SELECT i.scan_kind, v.scan_kind FROM scout_investigations i
		JOIN scout_investigations_v v ON v.id = i.id`).Scan(&kind, &viewKind); err != nil || kind != ScanKindLive || viewKind != ScanKindLive {
		t.Fatalf("old row after the upgrade: scan_kind %q, view %q (%v), want live", kind, viewKind, err)
	}
	if after := datasetColumns(t, st); after != before {
		t.Errorf("scout_call_dataset_v columns changed:\n got %s\nwant %s", after, before)
	}
}

// The dataset view's Perceptor verdict comes from live scans only: a rescan
// stored with the first call's id leaks neither into that call nor, through
// the "earlier scan of the same token" rule, into a later repeat call.
func TestRescanDatasetViewIgnoresRescans(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	ca := "0x1111111111111111111111111111111111111111"
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: t0, status: CallStatusFailed})
	seedInvestigation(t, st, perc, &first, ca, t0.Add(10*24*time.Hour), investigationCompleted, ScanKindRescan, levelClean)
	repeat := seedRescanCall(t, st, rescanSeed{msg: 2, ca: ca, at: t0.Add(11 * 24 * time.Hour), status: CallStatusDuplicate})
	verdicts := func() map[int]*string {
		t.Helper()
		rows, err := st.Pool.Query(ctx, `SELECT call_id, perceptor_verdict FROM scout_call_dataset_v`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int]*string{}
		for rows.Next() {
			var id int
			var v *string
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatal(err)
			}
			out[id] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for id, v := range verdicts() {
		if v != nil {
			t.Errorf("call %d: perceptor_verdict %q with only a rescan, want NULL", id, *v)
		}
	}
	// a live scan of the first call is used, by both calls
	seedInvestigation(t, st, perc, &first, ca, t0.Add(time.Minute), investigationCompleted, ScanKindLive, levelRedFlags)
	got := verdicts()
	for _, id := range []int{first, repeat} {
		if got[id] == nil || *got[id] != levelRedFlags {
			t.Errorf("call %d: perceptor_verdict %v, want %s (the live scan)", id, got[id], levelRedFlags)
		}
	}
}

// A call's own live scan wins over a manual scan (call_id NULL, e.g. -scan)
// of the same token requested before the call. In Postgres, (NULL = id) is
// NULL, which DESC puts first: the view must not let the manual scan win. A
// later repeat call has no scan of its own and takes the latest live scan
// requested at or before it.
func TestDatasetViewOwnScanBeatsEarlierManualScan(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	ca := "0x2222222222222222222222222222222222222222"
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedInvestigation(t, st, perc, nil, ca, t0.Add(-time.Hour), investigationCompleted, ScanKindLive, levelCaution)
	first := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: t0, status: CallStatusFailed})
	seedInvestigation(t, st, perc, &first, ca, t0.Add(time.Minute), investigationCompleted, ScanKindLive, levelRedFlags)
	repeat := seedRescanCall(t, st, rescanSeed{msg: 2, ca: ca, at: t0.Add(24 * time.Hour), status: CallStatusDuplicate})
	tests := []struct {
		name string
		call int
		want string
	}{
		{"first call, own scan", first, levelRedFlags},
		{"repeat call, latest earlier scan", repeat, levelRedFlags},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *string
			if err := st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, tt.call).Scan(&got); err != nil {
				t.Fatalf("call %d: %v", tt.call, err)
			}
			gotS := "NULL"
			if got != nil {
				gotS = *got
			}
			if gotS != tt.want {
				t.Errorf("call %d: perceptor_verdict = %s, want %s (manual scan %s requested an hour before the call)", tt.call, gotS, tt.want, levelCaution)
			}
		})
	}
}

// The requeue counts live scans only: a rescan neither makes a stuck call
// look scanned nor its token look investigated.
func TestRescanRequeueIgnoresRescans(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	ca := "0x1111111111111111111111111111111111111111"
	now := time.Now().UTC()
	old := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: now.Add(-10 * 24 * time.Hour), status: CallStatusFailed})
	stuck := seedRescanCall(t, st, rescanSeed{msg: 2, ca: ca, at: now.Add(-time.Hour), status: CallStatusQueued})
	seedInvestigation(t, st, perc, &old, ca, now.Add(-30*time.Minute), investigationCompleted, ScanKindRescan, "")
	seedInvestigation(t, st, perc, &stuck, ca, now.Add(-20*time.Minute), investigationCompleted, ScanKindRescan, "")
	calls, _, err := st.SelectScoutCallsToRequeue(ctx, 777, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].ID != stuck || calls[0].CAInvestigated {
		t.Fatalf("requeue with rescans only: got %+v, want call %d, not investigated", calls, stuck)
	}
	seedInvestigation(t, st, perc, &old, ca, now.Add(-10*time.Minute), investigationCompleted, ScanKindLive, "")
	calls, _, err = st.SelectScoutCallsToRequeue(ctx, 777, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0].CAInvestigated {
		t.Fatalf("requeue with a live scan of the token: got %+v, want call %d investigated", calls, stuck)
	}
}

// rescanFixture seeds first calls covering every rule of the selection.
type rescanFixture struct {
	st    *ScoutStore
	perc  int
	now   time.Time
	ids   map[string]int
	cfg   rescanConfig
	order []string // expected candidates, newest first
}

func newRescanFixture(t *testing.T) *rescanFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fx := &rescanFixture{st: st, now: time.Now().UTC().Truncate(time.Second), ids: map[string]int{},
		cfg: rescanConfig{Enabled: true, MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute, Idle: 5 * time.Minute,
			Statuses: []string{"backfill", "duplicate", "failed", "scanned"}}}
	fx.perc = mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	salpha := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	h := func(n float64) time.Time { return fx.now.Add(-time.Duration(n * float64(time.Hour))) }
	yes, no := true, false
	f := func(v float64) *float64 { return &v }
	ca := func(n int) string { return "0x" + strings.Repeat("0", 38) + strconv.Itoa(10+n) }
	msg := 0
	add := func(key string, s rescanSeed) int {
		msg++
		s.msg = msg
		id := seedRescanCall(t, st, s)
		if key != "" {
			fx.ids[key] = id
		}
		return id
	}
	// candidates
	add("fresh", rescanSeed{ca: ca(1), at: h(2), status: CallStatusFailed})                                             // no tracking row yet
	add("priced", rescanSeed{ca: ca(2), at: h(5), status: CallStatusScanned, track: true, rugged: &no, latest: f(-98)}) // > -99
	add("salpha_only", rescanSeed{ca: ca(3), at: h(30), status: CallStatusScanned})
	seedInvestigation(t, st, salpha, ip(fx.ids["salpha_only"]), ca(3), h(30), investigationCompleted, ScanKindLive, levelUnknown)
	add("perc_failed", rescanSeed{ca: ca(4), at: h(48), status: CallStatusFailed})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["perc_failed"]), ca(4), h(48), investigationTimeout, ScanKindLive, levelUnknown)
	add("one_failed_rescan", rescanSeed{ca: ca(5), at: h(72), status: CallStatusBackfill})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["one_failed_rescan"]), ca(5), h(1), investigationTimeout, ScanKindRescan, levelUnknown)
	seedInvestigation(t, st, fx.perc, ip(fx.ids["one_failed_rescan"]), ca(5), h(2), investigationRateLimited, ScanKindRescan, levelUnknown)
	// a first call after an update post of the token (the update is no call)
	add("", rescanSeed{ca: ca(6), at: h(200), status: CallStatusUpdate, postKind: PostKindUpdate})
	add("after_update", rescanSeed{ca: ca(6), at: h(100), status: CallStatusBackfill})
	// the first call, not its later repeat, is the candidate (the repeat is newer)
	add("first_of_repeat", rescanSeed{ca: ca(7), at: h(150), status: CallStatusDuplicate})
	add("", rescanSeed{ca: ca(7), at: h(3), status: CallStatusDuplicate})
	// same time as "tie_low": the higher id comes first
	add("tie_low", rescanSeed{ca: ca(8), at: h(400), status: CallStatusBackfill})
	add("tie_high", rescanSeed{ca: ca(9), at: h(400), status: CallStatusBackfill})
	fx.order = []string{"fresh", "priced", "salpha_only", "perc_failed", "one_failed_rescan", "after_update", "first_of_repeat", "tie_high", "tie_low"}

	// excluded
	add("rugged", rescanSeed{ca: ca(20), at: h(1), status: CallStatusFailed, track: true, rugged: &yes})
	add("wiped", rescanSeed{ca: ca(21), at: h(1), status: CallStatusFailed, track: true, rugged: &no, latest: f(-99.5)})
	add("at_minus_99", rescanSeed{ca: ca(22), at: h(1), status: CallStatusFailed, track: true, latest: f(-99)})
	add("too_old", rescanSeed{ca: ca(23), at: h(721), status: CallStatusFailed})
	add("queued", rescanSeed{ca: ca(24), at: h(1), status: CallStatusQueued})
	add("dropped", rescanSeed{ca: ca(25), at: h(1), status: CallStatusDropped})
	add("has_live", rescanSeed{ca: ca(26), at: h(1), status: CallStatusScanned})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["has_live"]), ca(26), h(1), investigationCompleted, ScanKindLive, "")
	add("has_rescan", rescanSeed{ca: ca(27), at: h(1), status: CallStatusFailed})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["has_rescan"]), ca(27), h(0.5), investigationCompleted, ScanKindRescan, "")
	// a report of the token in other letter case, attached to no call
	mixed := "0x" + strings.Repeat("abcdef", 6) + "0028"
	add("case_report", rescanSeed{ca: mixed, at: h(1), status: CallStatusFailed})
	seedInvestigation(t, st, fx.perc, nil, "0x"+strings.ToUpper(mixed[2:]), h(0.5), investigationCompleted, ScanKindLive, "")
	add("failed_twice", rescanSeed{ca: ca(29), at: h(1), status: CallStatusFailed})
	seedInvestigation(t, st, fx.perc, ip(fx.ids["failed_twice"]), ca(29), h(0.9), investigationFailed, ScanKindRescan, levelUnknown)
	seedInvestigation(t, st, fx.perc, ip(fx.ids["failed_twice"]), ca(29), h(0.8), investigationTimeout, ScanKindRescan, levelUnknown)
	// a later call is not the first call: the first one's status counts (queued here)
	add("", rescanSeed{ca: ca(30), at: h(10), status: CallStatusQueued})
	add("", rescanSeed{ca: ca(30), at: h(2), status: CallStatusFailed})
	return fx
}

// NextRescanCandidate walks the candidates newest first; each rescan that
// completes takes its token out.
func TestNextRescanCandidate(t *testing.T) {
	fx := newRescanFixture(t)
	ctx := context.Background()
	byID := map[int]string{}
	for k, id := range fx.ids {
		byID[id] = k
	}
	var got []string
	for i := 0; i < 20; i++ {
		c, err := fx.st.NextRescanCandidate(ctx, fx.cfg.Statuses, fx.now.Add(-fx.cfg.MaxAge), rescanFailedLimit)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			break
		}
		got = append(got, byID[c.CallID])
		seedInvestigation(t, fx.st, fx.perc, ip(c.CallID), c.ContractAddress, fx.now, investigationCompleted, ScanKindRescan, "")
	}
	if strings.Join(got, ",") != strings.Join(fx.order, ",") {
		t.Errorf("candidates in order:\n got %v\nwant %v", got, fx.order)
	}
	// with the completed rescans removed again: the status list and the age limit apply
	if _, err := fx.st.Pool.Exec(ctx, `DELETE FROM scout_investigations WHERE scan_kind = 'rescan' AND requested_at = $1`, fx.now); err != nil {
		t.Fatal(err)
	}
	c, err := fx.st.NextRescanCandidate(ctx, []string{CallStatusDuplicate}, fx.now.Add(-fx.cfg.MaxAge), rescanFailedLimit)
	if err != nil || c == nil || c.CallID != fx.ids["first_of_repeat"] {
		t.Errorf("duplicate only: got %+v (%v), want call %d", c, err, fx.ids["first_of_repeat"])
	}
	c, err = fx.st.NextRescanCandidate(ctx, fx.cfg.Statuses, fx.now.Add(-3*time.Hour), rescanFailedLimit)
	if err != nil || c == nil || c.CallID != fx.ids["fresh"] {
		t.Errorf("3h max age: got %+v (%v), want call %d", c, err, fx.ids["fresh"])
	}
}

// The daily cap and gap come from the database (rows of any status).
func TestRescanStats(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	now := time.Now().UTC().Truncate(time.Microsecond)
	n, last, err := st.RescanStats(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 0 || last != nil {
		t.Fatalf("RescanStats with no rescans = %d, %v, %v; want 0, nil", n, last, err)
	}
	ca := "0x1111111111111111111111111111111111111111"
	seedInvestigation(t, st, perc, nil, ca, now.Add(-25*time.Hour), investigationCompleted, ScanKindRescan, "")
	seedInvestigation(t, st, perc, nil, ca, now.Add(-2*time.Hour), investigationTimeout, ScanKindRescan, levelUnknown)
	seedInvestigation(t, st, perc, nil, ca, now.Add(-1*time.Hour), investigationRateLimited, ScanKindRescan, levelUnknown)
	seedInvestigation(t, st, perc, nil, ca, now.Add(-time.Minute), investigationCompleted, ScanKindLive, "") // live: not counted
	n, last, err = st.RescanStats(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 2 || last == nil || !last.Equal(now.Add(-time.Hour)) {
		t.Fatalf("RescanStats = %d, %v, %v; want 2 and %s", n, last, err, now.Add(-time.Hour))
	}
}

// The dry run's numbers follow the same rules as the selection.
func TestRescanDryRun(t *testing.T) {
	fx := newRescanFixture(t)
	var out bytes.Buffer
	if err := runRescanDryRun(context.Background(), fx.st, &out, fx.cfg, fx.now); err != nil {
		t.Fatal(err)
	}
	// pool: the 9 candidates and rugged, wiped, at_minus_99, too_old, failed_twice
	for _, line := range []string{
		"first calls without a Perceptor report, status backfill,duplicate,failed,scanned: 14\n",
		"too old 1, rugged 1, latest return <= -99% 2, failed twice 1\n",
		"candidates: 9\n",
		"by status: backfill 4, duplicate 1, failed 2, scanned 2\n",
		"by age: <1d 2, 1-7d 5, 7-30d 2\n",
		"at most 100 a day (cap 100, gap 10m0s) -> about 1 day(s) for these 9",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("dry run lacks %q:\n%s", line, out.String())
		}
	}
	// the list: the candidates, in the order NextRescanCandidate takes them
	_, list, ok := strings.Cut(out.String(), "contract address\n")
	if !ok {
		t.Fatalf("dry run has no candidate list:\n%s", out.String())
	}
	var got, want []string
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		got = append(got, strings.Fields(line)[0])
	}
	for _, k := range fx.order {
		want = append(want, strconv.Itoa(fx.ids[k]))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dry run lists call ids %v, want %v (%v)", got, want, fx.order)
	}
}

// A rescan through the real scanner writes one scan_kind 'rescan' row and
// nothing else: no delivery, no status or tracking change, no seen_cas.json
// entry, no score, no queued job.
func TestRescanLaneSideEffects(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the rescan asked the model service for %s", r.URL)
	}))
	t.Cleanup(model.Close)
	dir := t.TempDir()
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: dir,
		Tools: []ToolSpec{testPerceptorSpec()}, ModelURL: model.URL, ModelTimeout: time.Second,
		DeliverLevels: map[string]bool{levelClean: true}}
	s := newScanner(cfg)
	s.db = st
	if err := s.registerTools(ctx); err != nil {
		t.Fatal(err)
	}
	seenFile := filepath.Join(dir, "seen_cas.json")
	if err := os.WriteFile(seenFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := "0x1111111111111111111111111111111111111111"
	no := false
	callAt := time.Now().UTC().Add(-12 * 24 * time.Hour).Truncate(time.Second)
	call := seedRescanCall(t, st, rescanSeed{msg: 1, ca: ca, at: callAt, status: CallStatusFailed, track: true, rugged: &no})
	state := func() string {
		t.Helper()
		var s string
		if err := st.Pool.QueryRow(ctx, `SELECT row_to_json(c)::text || (SELECT row_to_json(t)::text FROM scout_call_tracking t WHERE t.call_id = c.id)
			|| (SELECT count(*) FROM scout_deliveries)::text || (SELECT count(*) FROM scout_call_predictions)::text
			FROM scout_calls c WHERE c.id = $1`, call).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()

	lane := s.newRescanLane(rescanConfig{Enabled: true, MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute,
		Idle: 5 * time.Minute, Statuses: []string{CallStatusFailed}})
	if lane == nil {
		t.Fatal("newRescanLane(on) = nil with a database and the perceptor tool")
	}
	var sent []string
	lane.send = replyingSend("✅ No red flags found", nil, &sent)
	lane.noteLive(time.Now().Add(-time.Hour))
	if !lane.step(ctx, st, func() int { return len(s.queue) }) {
		t.Fatal("step() = false, want a rescan")
	}
	if after := state(); after != before {
		t.Errorf("call, tracking, deliveries or predictions changed:\nbefore %s\n after %s", before, after)
	}
	if b, err := os.ReadFile(seenFile); err != nil || string(b) != `{}` {
		t.Errorf("seen_cas.json = %q (%v), want it untouched", b, err)
	}
	if len(s.queue) != 0 || len(s.pending) != 0 {
		t.Errorf("queue %d, pending %d; want both empty", len(s.queue), len(s.pending))
	}
	invs, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("%d investigations, want 1", len(invs))
	}
	r := invs[0]
	var d map[string]any
	if err := json.Unmarshal(r.Details, &d); err != nil {
		t.Fatal(err)
	}
	if r.ScanKind != ScanKindRescan || r.CallID == nil || *r.CallID != call || r.Status != investigationCompleted ||
		r.VerdictLevel != levelClean || d["rescan"] != true || d["call_age_s"] == nil {
		t.Errorf("investigation = %+v, details %s; want a completed clean rescan of call %d with rescan/call_age_s", r, r.Details, call)
	}
	if time.Since(r.RequestedAt) > time.Minute {
		t.Errorf("requested_at %s, want the real time of the scan", r.RequestedAt)
	}
	// the next step waits for the gap (from the database)
	lane.noteLive(time.Now().Add(-time.Hour))
	if lane.step(ctx, st, func() int { return 0 }) || len(sent) != 1 {
		t.Errorf("a second step right after a rescan scanned again (sent %q)", sent)
	}
}

// The website keeps rescans out of everything it showed before (the verdict,
// its link and report id, the Perceptor filter and its counts, the detail of
// GET /api/call) and shows them only as "Perceptor today"
// (perceptor_today_verdict, _url, _at), which is part of the snapshot version.
func TestRescanWebFields(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	perc := mustTool(t, fx.st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	t0 := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	later := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	url := func(s string) *string { return &s }
	inv := func(key, ca string, at time.Time, kind, level string, link *string) int {
		t.Helper()
		r := &ScoutInvestigation{CallID: ip(fx.ids[key]), ToolID: perc, ContractAddress: ca, RequestText: "/scan " + ca,
			RequestedAt: at, CompletedAt: &at, Status: investigationCompleted, VerdictLevel: level, ScanKind: kind, ReportURL: link}
		id, err := fx.st.InsertScoutInvestigation(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return *id
	}
	before := map[string]int{}
	for _, v := range []string{"not_scanned", "clean", "caution", "red_flags"} {
		before[v] = fx.calls(t, "per=100&verdict="+v).Total
	}

	// alpha: a live scan at the call, a rescan later (newer): the live one stays
	live := inv("alpha", caAlpha, t0, ScanKindLive, levelClean, url("https://www.perceptor.info/r/live"))
	inv("alpha", caAlpha, later, ScanKindRescan, levelCaution, url("https://www.perceptor.info/r/today"))
	// beta: a rescan only; virt: a rescan only, with a link that is not https
	inv("beta", caBeta, later, ScanKindRescan, levelRedFlags, url("https://www.perceptor.info/r/beta"))
	inv("virt", caVirt, later, ScanKindRescan, levelClean, url("http://www.perceptor.info/r/plain"))
	// gamma: a rescan that did not complete is not "Perceptor today"
	failedAt := later
	if _, err := fx.st.InsertScoutInvestigation(ctx, &ScoutInvestigation{CallID: ip(fx.ids["gamma"]), ToolID: perc, ContractAddress: caGamma,
		RequestText: "/scan " + caGamma, RequestedAt: failedAt, CompletedAt: &failedAt, Status: investigationTimeout,
		VerdictLevel: levelUnknown, ScanKind: ScanKindRescan}); err != nil {
		t.Fatal(err)
	}

	type want struct {
		verdict, url, reportID   string
		today, todayURL, todayAt string
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	num := func(p *int) string {
		if p == nil {
			return "<nil>"
		}
		return strconv.Itoa(*p)
	}
	wants := map[string]want{
		"alpha": {levelClean, "https://www.perceptor.info/r/live", strconv.Itoa(live), levelCaution, "https://www.perceptor.info/r/today", "2026-10-01T00:00:00Z"},
		"beta":  {"<nil>", "<nil>", "<nil>", levelRedFlags, "https://www.perceptor.info/r/beta", "2026-10-01T00:00:00Z"},
		"virt":  {"<nil>", "<nil>", "<nil>", levelClean, "<nil>", "2026-10-01T00:00:00Z"},
		"gamma": {"<nil>", "<nil>", "<nil>", "<nil>", "<nil>", "<nil>"},
		"xss":   {"<nil>", "<nil>", "<nil>", "<nil>", "<nil>", "<nil>"},
	}
	res := fx.calls(t, "per=100")
	byKey := map[string]webCallJSON{}
	for i, k := range fx.keys(res.Calls) {
		byKey[k] = res.Calls[i]
	}
	for k, w := range wants {
		c, ok := byKey[k]
		if !ok {
			t.Fatalf("%s not listed", k)
		}
		got := want{str(c.Perceptor), str(c.PerceptorURL), num(c.PerceptorReportID), str(c.PerceptorTodayVerd), str(c.PerceptorTodayURL), str(c.PerceptorTodayAt)}
		if got != w {
			t.Errorf("%s: perceptor (verdict, url, report id, today, today url, today at) = %v, want %v", k, got, w)
		}
	}
	// the Perceptor filter and its counts: only alpha's live scan changed them
	after := map[string]int{}
	for _, v := range []string{"not_scanned", "clean", "caution", "red_flags"} {
		after[v] = fx.calls(t, "per=100&verdict="+v).Total
	}
	wantAfter := map[string]int{"not_scanned": before["not_scanned"] - 1, "clean": before["clean"] + 1,
		"caution": before["caution"], "red_flags": before["red_flags"]}
	for v, n := range wantAfter {
		if after[v] != n {
			t.Errorf("verdict=%s: %d calls, want %d (before the scans %d; rescans must not count)", v, after[v], n, before[v])
		}
	}
	// the detail: the live report, or none
	for key, wantID := range map[string]int{"alpha": live, "beta": 0} {
		code, _, body := fx.get(t, "/api/call?id="+strconv.Itoa(fx.ids[key]))
		if code != 200 {
			t.Fatalf("%s: %d %s", key, code, body)
		}
		d := decodeCallDetail(t, body)
		switch {
		case wantID == 0 && d.Perceptor != nil:
			t.Errorf("%s: perceptor detail %+v, want none (a rescan only)", key, d.Perceptor)
		case wantID != 0 && (d.Perceptor == nil || d.Perceptor.ID != wantID || d.Perceptor.Verdict != levelClean):
			t.Errorf("%s: perceptor detail %+v, want the live report %d (clean)", key, d.Perceptor, wantID)
		}
	}
	// "Perceptor today" is part of the snapshot version
	rows, _, err := fx.st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := hashWebRows(rows, 0, "")
	for i := range rows {
		if rows[i].CallID == fx.ids["beta"] {
			v := levelCaution
			rows[i].PerceptorTodayVerd = &v
		}
	}
	if b := hashWebRows(rows, 0, ""); a == b {
		t.Error("hashWebRows ignores PerceptorTodayVerd")
	}
	for i := range rows {
		if rows[i].CallID == fx.ids["beta"] {
			at := rows[i].PerceptorTodayAt.Add(time.Second)
			rows[i].PerceptorTodayAt = &at
		}
	}
	if c := hashWebRows(rows, 0, ""); c == a {
		t.Error("hashWebRows ignores PerceptorTodayAt")
	}
}
