package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The tracker's latest-price cut-offs in cmdConfig: not the defaults, so a
// test sees that the pauser passes these.
const cmdLatestRecent, cmdLatestOld = 17 * time.Minute, 25 * time.Hour

// newCmdChain: a fake chain with one token on a v2 pool (lqPool), entered 10
// days ago, launched an hour before that, with one buy before the call. The
// export's client runs at its highest rate and retries fast.
func newCmdChain(t *testing.T) *hfCall {
	t.Helper()
	fastRetries(t)
	t.Setenv("SCOUT_HOLDERS_RPS", "20")
	c := newHFCall(t, 40*24*time.Hour, 10*24*time.Hour, time.Hour, 1_000_000)
	c.f.transfer(hfToken, lqPool, hfBuyer, c.entryBlock-5, hfTx(2))
	return c
}

// cmdState: the tracker's stored state of the token of c.
func cmdState(c *hfCall) onchainState {
	return onchainState{V: onchainStateVersion, Kind: "v2", Pool: lqPool, EntryBlock: c.entryBlock}
}

// cmdMeta: a readable call of the token of c.
func cmdMeta(c *hfCall, id int64) holderCallMeta {
	st := cmdState(c)
	return holderCallMeta{CallID: id, Token: hfToken, Chain: "evm", TrackingRow: true, State: &st, EntryAt: c.entryAt, PostedAgeS: ip(3600)}
}

// cmdConfig: the program's settings for the fake chain (its URL is in the
// environment since newCmdChain).
func cmdConfig(t *testing.T) *config {
	t.Helper()
	oc, err := loadOnchainConfig()
	if err != nil {
		t.Fatal(err)
	}
	return &config{StateDir: filepath.Join(t.TempDir(), "state"),
		Price: priceConfig{Onchain: oc, LatestRecent: cmdLatestRecent, LatestOld: cmdLatestOld}}
}

// cmdFakeDB is holderExportDB without Postgres.
type cmdFakeDB struct {
	metas    map[int64]holderCallMeta
	priceLag func(ask int) time.Duration // nil = 0
	lagAsks  int
	lagArgs  [3]time.Duration // recent, old, recentAge of the last ask
}

func newCmdFakeDB(metas ...holderCallMeta) *cmdFakeDB {
	d := &cmdFakeDB{metas: map[int64]holderCallMeta{}}
	for _, m := range metas {
		d.metas[m.CallID] = m
	}
	return d
}

func (d *cmdFakeDB) HolderCallMetas(_ context.Context, ids []int64) ([]holderCallMeta, []int64, error) {
	metas, missing := orderHolderCallMetas(ids, d.metas)
	return metas, missing, nil
}

func (d *cmdFakeDB) LatestPriceLag(_ context.Context, _ time.Time, recent, old, recentAge time.Duration) (time.Duration, error) {
	d.lagAsks++
	d.lagArgs = [3]time.Duration{recent, old, recentAge}
	if d.priceLag == nil {
		return 0, nil
	}
	return d.priceLag(d.lagAsks), nil
}

// newCmdExport builds the export with a fake pauser clock: a pause step
// moves the clock instead of sleeping.
func newCmdExport(t *testing.T, db holderExportDB, opts holderExportOptions, logw *bytes.Buffer) *holderExport {
	t.Helper()
	x, err := newHolderExport(cmdConfig(t), db, opts, logw)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Now()
	x.pauser.clock = func() time.Time { return clk }
	x.pauser.sleep = func(_ context.Context, d time.Duration) error { clk = clk.Add(d); return nil }
	return x
}

// writeIDs writes an ids file and returns its path.
func writeIDs(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ids.csv")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHolderExportFlags(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string // "" = valid
	}{
		{nil, ""},
		{[]string{"-export-holders", "ids.csv"}, ""},
		{[]string{"-export-holders", "ids.csv", "-export-holders-out", "x", "-export-holders-max", "10"}, ""},
		{[]string{"-export-holders-max", "10"}, "used with -export-holders"},
		{[]string{"-export-holders-out", "x"}, "used with -export-holders"},
		{[]string{"-export-holders", "ids.csv", "-export-holders-max", "-1"}, "want 0"},
	}
	for _, tc := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		o := holderExportFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		err := o.check()
		if (tc.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("check(%v) = %v, want error %q", tc.args, err, tc.wantErr)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	if o := holderExportFlags(fs); o.OutDir != filepath.FromSlash("data/holders") {
		t.Errorf("default out %q, want data/holders", o.OutDir)
	}
}

// The ids file of the pilot query, exported as CSV: call_id first, a header,
// blank lines and a repeat.
func TestHolderExportReadIDs(t *testing.T) {
	p := writeIDs(t, "call_id,grp\r\n5,dead7\r\n\r\n\"6\",held2x7\r\n5,dead7\r\n")
	got, err := readHolderIDs(p)
	if err != nil || !slices.Equal(got, []int64{5, 6}) {
		t.Errorf("readHolderIDs = %v, %v; want [5 6]", got, err)
	}
	if _, err := readHolderIDs(filepath.Join(t.TempDir(), "none.csv")); err == nil {
		t.Error("a missing ids file: no error")
	}
}

// The plan drops the calls already written and cuts the rest to the maximum.
func TestHolderExportPlan(t *testing.T) {
	var metas []holderCallMeta
	for _, id := range []int64{1, 2, 3, 4, 5} {
		metas = append(metas, holderCallMeta{CallID: id})
	}
	done := map[int64]bool{2: true, 4: true}
	for _, tc := range []struct {
		limit int
		want  []int64
	}{{0, []int64{1, 3, 5}}, {2, []int64{1, 3}}, {9, []int64{1, 3, 5}}} {
		p := planHolderExport(6, metas, []int64{9}, done, tc.limit)
		if got := hdbIDs(p.ThisRun); !slices.Equal(got, tc.want) || p.Done != 2 || p.ToDo != 3 || p.IDs != 6 {
			t.Errorf("limit %d: this run %v, done %d, to do %d; want %v, 2, 3", tc.limit, got, p.Done, p.ToDo, tc.want)
		}
	}
}

// The command's database has no Migrate (it cannot apply the schema), and
// main dispatches the command before newScanner, openScoutStore, Migrate and
// registerTools, and returns right after it.
func TestHolderExportDispatchedBeforeMigrate(t *testing.T) {
	db := reflect.TypeOf((*holderExportDB)(nil)).Elem()
	if _, ok := db.MethodByName("Migrate"); ok || db.NumMethod() != 2 {
		t.Errorf("holderExportDB has %d methods (Migrate: %v), want HolderCallMetas and LatestPriceLag only", db.NumMethod(), ok)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("no func main in main.go")
	}
	first := firstCalls(body)
	export, ok := first["runHolderExportCommand"]
	if !ok {
		t.Fatal("main does not call runHolderExportCommand")
	}
	for _, later := range []string{"newScanner", "openScoutStore", "Migrate", "registerTools"} {
		if p, ok := first[later]; !ok || p < export {
			t.Errorf("%s is called before runHolderExportCommand (or not at all: %v)", later, !ok)
		}
	}
	if !returnsAfter(body, export) {
		t.Error("the if block that calls runHolderExportCommand does not end with return")
	}
}

// firstCalls maps each called function or method name in n to its first call.
func firstCalls(n ast.Node) map[string]token.Pos {
	first := map[string]token.Pos{}
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if _, seen := first[name]; !seen && name != "" {
			first[name] = call.Pos()
		}
		return true
	})
	return first
}

// returnsAfter: the top-level if statement of body holding pos ends with a
// return.
func returnsAfter(body *ast.BlockStmt, pos token.Pos) bool {
	for _, s := range body.List {
		ifs, ok := s.(*ast.IfStmt)
		if !ok || pos < ifs.Pos() || pos > ifs.End() {
			continue
		}
		_, ok = ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt)
		return ok
	}
	return false
}

// -dry-run estimates from the stored state alone: no node request, no folder,
// no file.
func TestHolderExportDryRunNoNode(t *testing.T) {
	c := newCmdChain(t)
	db := newCmdFakeDB(cmdMeta(c, 1), cmdMeta(c, 2), holderCallMeta{CallID: 3, Chain: "evm"})
	out := filepath.Join(t.TempDir(), "out")
	var logw bytes.Buffer
	x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n2\n3\n"), OutDir: out, DryRun: true}, &logw)
	if err := x.run(context.Background()); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := c.f.counts(); len(n) != 0 {
		t.Errorf("dry run asked the node: %v", n)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry run made the output folder (stat: %v)", err)
	}
	cost := holderCostCall{EntryBlock: c.entryBlock, Age: time.Hour, Filters: 1}
	if x.oc.HistoryChunk != defaultHistoryChunk {
		t.Fatalf("export history cap %d, want the default %d", x.oc.HistoryChunk, defaultHistoryChunk)
	}
	// The dry run counts the launch search in SCOUT_RPC_HISTORY_CHUNK ranges
	// (the entry is near block 26M: 13 ranges, not 1).
	_, want, err := holderCostEstimate([]holderCostCall{cost, cost}, x.oc.LogChunk, x.oc.HistoryChunk)
	if err != nil {
		t.Fatal(err)
	}
	if _, uncapped, _ := holderCostEstimate([]holderCostCall{cost, cost}, x.oc.LogChunk, 0); want <= uncapped {
		t.Fatalf("capped estimate %d, uncapped %d: the test does not test the cap", want, uncapped)
	}
	if line := fmt.Sprintf("about %d eth_getLogs for 3 calls (1 skipped", want); !strings.Contains(logw.String(), line) {
		t.Errorf("log %q, want %q", logw.String(), line)
	}
	if db.lagAsks != 0 {
		t.Errorf("dry run asked the price lag %d times", db.lagAsks)
	}
}

// The dry run counts each v4 call's pool-id search in SCOUT_RPC_HISTORY_CHUNK
// ranges up to the largest entry block of the run (the head is not asked),
// says that part is a lower bound, and counts 2 per call with no cap.
func TestHolderExportDryRunV4Search(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string // SCOUT_RPC_HISTORY_CHUNK ("" = the default)
		perV4 func(head uint64) int64
	}{
		{"default history cap", "", func(head uint64) int64 { return 2 * int64(head/2_000_000+1) }},
		{"no history cap", "0", func(uint64) int64 { return 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCmdChain(t)
			t.Setenv("SCOUT_RPC_HISTORY_CHUNK", tc.value)
			v4Meta := func(id int64, entry uint64) holderCallMeta {
				st := onchainState{V: onchainStateVersion, Kind: "v4", Pool: lqPM, PoolID: lqPoolID, EntryBlock: entry}
				return holderCallMeta{CallID: id, Token: hfToken, Chain: "evm", TrackingRow: true, State: &st, EntryAt: c.entryAt, PostedAgeS: ip(3600)}
			}
			metas := []holderCallMeta{cmdMeta(c, 1), v4Meta(2, c.entryBlock-4_000_000), v4Meta(3, c.entryBlock)}
			db := newCmdFakeDB(metas...)
			var logw bytes.Buffer
			x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n2\n3\n"), OutDir: filepath.Join(t.TempDir(), "out"), DryRun: true}, &logw)
			if err := x.run(context.Background()); err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if n := c.f.counts(); len(n) != 0 {
				t.Errorf("dry run asked the node: %v", n)
			}
			calls, _, _, v4 := holderCostCalls(metas, x.venues)
			if v4 != 2 {
				t.Fatalf("%d v4 calls, want 2", v4)
			}
			_, base, err := holderCostEstimate(calls, x.oc.LogChunk, x.oc.HistoryChunk)
			if err != nil {
				t.Fatal(err)
			}
			v4Logs := 2 * tc.perV4(c.entryBlock) // the largest entry block, not the older call's
			for _, line := range []string{
				fmt.Sprintf("about %d eth_getLogs for 3 calls (0 skipped", base+v4Logs),
				fmt.Sprintf("the v4 pool searches of 2 calls: at least %d, counted up to block %d, the largest entry block", v4Logs, c.entryBlock),
			} {
				if !strings.Contains(logw.String(), line) {
					t.Errorf("SCOUT_RPC_HISTORY_CHUNK=%q: log %q, want %q", tc.value, logw.String(), line)
				}
			}
		})
	}
}

// The v4 pool-id search is in the estimate.
func TestHolderExportCostCallsV4(t *testing.T) {
	st := onchainState{V: onchainStateVersion, Kind: "v4", Pool: lqPM, PoolID: lqPoolID, EntryBlock: 5000}
	m := holderCallMeta{CallID: 1, Token: hfToken, Chain: "evm", TrackingRow: true, State: &st, EntryAt: time.Now()}
	calls, skipped, unknownAge, v4 := holderCostCalls([]holderCallMeta{m}, holderVenueConfig{PoolManagerV4: lqPM})
	if len(calls) != 1 || calls[0].Filters != 1 || skipped != 0 || unknownAge != 1 || v4 != 1 {
		t.Errorf("calls %+v, skipped %d, unknown age %d, v4 %d; want 1 call with 1 filter, 0, 1, 1", calls, skipped, unknownAge, v4)
	}
}

// exportRun runs the export once with opts and returns its log.
type exportRun func(t *testing.T, opts holderExportOptions) string

// checkExportResume runs the export twice over an ids file holding the
// readable calls, a call without a tracking row (skipped), an id not in
// scout_calls, a blank line and a repeat: first with -export-holders-max 2,
// then with no maximum. The second run does not redo the first two calls.
func checkExportResume(t *testing.T, c *hfCall, run exportRun, readable []int64, skipped, missing int64) {
	t.Helper()
	ids := fmt.Sprintf("call_id,grp\r\n%d,a\r\n%d,b\r\n%d,c\r\n%d,d\r\n%d,e\r\n\r\n%d,a\r\n", readable[0], readable[1], readable[2], skipped, missing, readable[0])
	opts := holderExportOptions{IDsFile: writeIDs(t, ids), OutDir: filepath.Join(t.TempDir(), "out"), Max: 2}
	log1 := run(t, opts)
	dir := opts.OutDir
	if rows := mustRows(t, dir); !slices.Equal(rowIDs(rows), readable[:2]) {
		t.Fatalf("after -export-holders-max 2: rows %v, want %v; log %s", rowIDs(rows), readable[:2], log1)
	}
	logsBefore := c.f.counts()["eth_getLogs"]
	opts.Max = 0
	log2 := run(t, opts)
	rows := mustRows(t, dir)
	if want := append(slices.Clone(readable), skipped); !slices.Equal(rowIDs(rows), want) {
		t.Fatalf("after the second run: rows %v, want %v; log %s", rowIDs(rows), want, log2)
	}
	for _, s := range []string{"5 call ids read (1 not in scout_calls), 2 already in", "2 to do; this run: 2"} {
		if !strings.Contains(log2, s) {
			t.Errorf("second run's log %q lacks %q", log2, s)
		}
	}
	var asked int64
	for _, r := range rows[2:] {
		if r.RPCLogsRequests != nil {
			asked += *r.RPCLogsRequests
		}
	}
	if got := int64(c.f.counts()["eth_getLogs"] - logsBefore); got != asked || asked == 0 {
		t.Errorf("second run: the node answered %d eth_getLogs, its rows count %d (a call redone?)", got, asked)
	}
	for _, r := range rows {
		want := holderRowOK
		if r.CallID == skipped {
			want = holderRowSkipped
		}
		if r.Status != want {
			t.Errorf("call %d: status %s, want %s", r.CallID, r.Status, want)
		}
	}
	checkExportFiles(t, c, dir, len(rows))
}

func mustRows(t *testing.T, dir string) []holderRow {
	t.Helper()
	rows, err := readHolderRows(dir)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// mustRunRecord reads dir's runs.jsonl as a single run record.
func mustRunRecord(t *testing.T, dir string) holderRunRecord {
	t.Helper()
	var rec holderRunRecord
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, holderRunsFile))), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func rowIDs(rows []holderRow) []int64 {
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r.CallID)
	}
	return ids
}

// checkExportFiles: holders.csv has the header and n rows, columns.json the
// columns, runs.jsonl two runs of 2 calls with the allowlisted settings and
// no URL.
func checkExportFiles(t *testing.T, c *hfCall, dir string, n int) {
	t.Helper()
	csvLines := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(dir, holderCSVFile)), "\n"), "\n")
	if len(csvLines) != n+1 || csvLines[0] != strings.Join(holderColumnNames(), ",") {
		t.Errorf("holders.csv: %d lines (want %d), header %.80q", len(csvLines), n+1, csvLines[0])
	}
	var cols struct {
		Columns        []string `json:"columns"`
		PreHoldColumns []string `json:"pre_hold_columns"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, holderColumnsFile))), &cols); err != nil {
		t.Fatalf("columns.json: %v", err)
	}
	if !slices.Equal(cols.PreHoldColumns, holderPreHoldColumns()) || len(cols.PreHoldColumns) == 0 || !slices.Equal(cols.Columns, holderColumnNames()) {
		t.Errorf("columns.json: %d pre_hold columns, %d columns; want %d, %d", len(cols.PreHoldColumns), len(cols.Columns), len(holderPreHoldColumns()), len(holderColumnNames()))
	}
	runs := readFile(t, filepath.Join(dir, holderRunsFile))
	if strings.Contains(runs, "://") || strings.Contains(runs, c.f.srv.URL) || strings.Contains(runs, strings.TrimPrefix(c.f.srv.URL, "http://")) {
		t.Errorf("runs.jsonl holds a URL: %s", runs)
	}
	lines := strings.Split(strings.TrimSuffix(runs, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("runs.jsonl: %d lines, want 2", len(lines))
	}
	for _, l := range lines {
		var rec holderRunRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Calls != 2 || !slices.Equal(slices.Sorted(maps.Keys(rec.Settings)), holderRunSettingNames()) {
			t.Errorf("run record: %d calls (want 2), settings %v", rec.Calls, rec.Settings)
		}
	}
}

// holderRunSettingNames: the allowlist, sorted, written out so a new name is
// a visible change here.
func holderRunSettingNames() []string {
	return slices.Sorted(slices.Values([]string{"SCOUT_HOLDERS_RPS", "SCOUT_HOLDERS_MAX_NODE_LAG", "SCOUT_HOLDERS_MAX_PRICE_LAG",
		"SCOUT_HOLDERS_MAX_PAUSE", "SCOUT_RPC_LOG_CHUNK", "SCOUT_RPC_HISTORY_CHUNK", "SCOUT_LATEST_REFRESH_RECENT", "SCOUT_LATEST_REFRESH_OLD",
		"export_holders_max", "sniper_window_blocks"}))
}

// The run loop on the fake chain with a fake database: rows, resume, -max,
// CSV, columns.json and the run record.
func TestHolderExportRunResume(t *testing.T) {
	c := newCmdChain(t)
	db := newCmdFakeDB(cmdMeta(c, 11), cmdMeta(c, 12), cmdMeta(c, 13), holderCallMeta{CallID: 14, Token: hfToken, Chain: "evm"})
	checkExportResume(t, c, func(t *testing.T, opts holderExportOptions) string {
		var logw bytes.Buffer
		if err := newCmdExport(t, db, opts, &logw).run(context.Background()); err != nil {
			t.Fatalf("run: %v; log %s", err, logw.String())
		}
		return logw.String()
	}, []int64{11, 12, 13}, 14, 99)
}

// The run-record settings are the allowlist, with values from the parsed
// settings: a node URL with a password never reaches them.
func TestHolderRunSettingsAllowlist(t *testing.T) {
	oc := onchainConfig{RPCURL: "http://user:secret@node:8545", MainnetRPCURL: "https://key@mainnet", LogChunk: 5000, HistoryChunk: 1_500_000}
	s := holderRunSettings(holderExportConfig{RPS: 4, MaxNodeLag: time.Minute}, oc, priceConfig{LatestRecent: time.Minute}, holderExportOptions{Max: 3, IDsFile: "secret.csv"})
	if got := slices.Sorted(maps.Keys(s)); !slices.Equal(got, holderRunSettingNames()) {
		t.Errorf("settings %v, want the names %v", got, holderRunSettingNames())
	}
	for k, v := range s {
		if strings.Contains(v, "secret") || strings.Contains(v, "://") || strings.Contains(v, "@") {
			t.Errorf("setting %s = %q", k, v)
		}
	}
	if s["SCOUT_RPC_LOG_CHUNK"] != "5000" || s["SCOUT_RPC_HISTORY_CHUNK"] != "1500000" || s["export_holders_max"] != "3" {
		t.Errorf("values %v", s)
	}
}

// Files go only to the folder the guard judged, never to the flag's path.
func TestHolderExportWritesOnlyJudgedPath(t *testing.T) {
	c := newCmdChain(t)
	flagDir, judged := filepath.Join(t.TempDir(), "flag"), filepath.Join(t.TempDir(), "judged")
	var logw bytes.Buffer
	x := newCmdExport(t, newCmdFakeDB(cmdMeta(c, 1)), holderExportOptions{IDsFile: writeIDs(t, "1\n"), OutDir: flagDir}, &logw)
	var asked string
	x.judgeOut = func(out string, _ []string, _ string) (string, error) { asked = out; return judged, nil }
	if err := x.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if asked != flagDir {
		t.Errorf("guard asked about %q, want %q", asked, flagDir)
	}
	if _, err := os.Stat(flagDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the flag's folder exists (stat: %v)", err)
	}
	for _, f := range []string{holderRowsFile, holderCSVFile, holderRunsFile, holderColumnsFile} {
		if _, err := os.Stat(filepath.Join(judged, f)); err != nil {
			t.Errorf("%s not in the judged folder: %v", f, err)
		}
	}
}

// A refused folder stops the run before anything is read or written.
func TestHolderExportRefusedOutDir(t *testing.T) {
	c := newCmdChain(t)
	db := newCmdFakeDB(cmdMeta(c, 1))
	x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n"), OutDir: stateSubDir(t)}, &bytes.Buffer{})
	x.stateDir = filepath.Dir(x.opts.OutDir)
	if err := x.run(context.Background()); !errors.Is(err, errHolderOutDir) {
		t.Errorf("run into the state folder: %v, want errHolderOutDir", err)
	}
	if n := c.f.counts(); len(n) != 0 {
		t.Errorf("node asked: %v", n)
	}
}

// stateSubDir: a folder inside a fresh state folder.
func stateSubDir(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(state, "holders")
}

// The price lag over its limit pauses the export (with the tracker's
// cut-offs), and the run goes on once it is back under.
func TestHolderExportPausesOnPriceLag(t *testing.T) {
	c := newCmdChain(t)
	db := newCmdFakeDB(cmdMeta(c, 1))
	db.priceLag = func(ask int) time.Duration {
		if ask == 1 {
			return 3 * time.Hour
		}
		return 0
	}
	out := filepath.Join(t.TempDir(), "out")
	var logw bytes.Buffer
	x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n"), OutDir: out}, &logw)
	if err := x.run(context.Background()); err != nil {
		t.Fatalf("run: %v; log %s", err, logw.String())
	}
	if !strings.Contains(logw.String(), "latest-price pass 3h0m0s behind") || !strings.Contains(logw.String(), "pausing") {
		t.Errorf("log %q, want a price-lag pause", logw.String())
	}
	if want := [3]time.Duration{cmdLatestRecent, cmdLatestOld, latestRecentAge}; db.lagArgs != want {
		t.Errorf("price lag asked with %v, want %v", db.lagArgs, want)
	}
	if rows := mustRows(t, out); len(rows) != 1 {
		t.Errorf("%d rows, want 1", len(rows))
	}
	if rec := mustRunRecord(t, out); rec.PausedS != (2 * holderPauseStep).Seconds() {
		t.Errorf("paused_s %v, want %v", rec.PausedS, (2 * holderPauseStep).Seconds())
	}
}

// The node lag, asked through the export's own client, pauses too; past
// SCOUT_HOLDERS_MAX_PAUSE the run stops before reading the call. A check
// whose limit is 0 is never asked.
func TestHolderExportNodeLagStops(t *testing.T) {
	c := newCmdChain(t)
	t.Setenv("SCOUT_HOLDERS_MAX_NODE_LAG", "1ns")
	t.Setenv("SCOUT_HOLDERS_MAX_PRICE_LAG", "0")
	t.Setenv("SCOUT_HOLDERS_MAX_PAUSE", "1m")
	db := newCmdFakeDB(cmdMeta(c, 1))
	out := filepath.Join(t.TempDir(), "out")
	var logw bytes.Buffer
	x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n"), OutDir: out}, &logw)
	err := x.run(context.Background())
	if !errors.Is(err, errHolderPausedTooLong) {
		t.Fatalf("run: %v, want errHolderPausedTooLong; log %s", err, logw.String())
	}
	if !strings.Contains(logw.String(), "node head block") {
		t.Errorf("log %q, want a node-lag pause", logw.String())
	}
	n := c.f.counts()
	if n["eth_blockNumber"] == 0 || n["eth_getLogs"] != 0 {
		t.Errorf("node requests %v, want head reads only", n)
	}
	if db.lagAsks != 0 {
		t.Errorf("price lag asked %d times with its check off", db.lagAsks)
	}
	if rows := mustRows(t, out); len(rows) != 0 {
		t.Errorf("%d rows, want none", len(rows))
	}
	if _, err := os.Stat(filepath.Join(out, holderRunsFile)); err != nil {
		t.Errorf("no run record after the stop: %v", err)
	}
}

// A call whose node reads fail gets no row and the run goes on; after
// holderMaxFailsInARow failures in a row it stops.
func TestHolderExportFailedCalls(t *testing.T) {
	c := newCmdChain(t)
	t.Setenv("SCOUT_HOLDERS_MAX_NODE_LAG", "0")
	c.f.mu.Lock()
	c.f.blockErr = func(n uint64) string {
		if n == c.launchBlock {
			return "block not available"
		}
		return ""
	}
	c.f.mu.Unlock()
	var metas []holderCallMeta
	var ids strings.Builder
	for id := int64(1); id <= holderMaxFailsInARow+1; id++ {
		metas = append(metas, cmdMeta(c, id))
		fmt.Fprintln(&ids, id)
	}
	out := filepath.Join(t.TempDir(), "out")
	var logw bytes.Buffer
	x := newCmdExport(t, newCmdFakeDB(metas...), holderExportOptions{IDsFile: writeIDs(t, ids.String()), OutDir: out}, &logw)
	err := x.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d calls in a row failed", holderMaxFailsInARow)) {
		t.Fatalf("run: %v, want a stop after %d failures", err, holderMaxFailsInARow)
	}
	if got := strings.Count(logw.String(), "failed, no row written"); got != holderMaxFailsInARow {
		t.Errorf("%d failure lines, want %d", got, holderMaxFailsInARow)
	}
	if rec := mustRunRecord(t, out); rec.Failed != holderMaxFailsInARow || rec.Calls != 0 {
		t.Errorf("run record: %d failed, %d calls; want %d, 0", rec.Failed, rec.Calls, holderMaxFailsInARow)
	}
}

// An interrupt (ctx ends, here during the first pause check) is a normal
// stop: no error, the CSV and the run record are still written.
func TestHolderExportInterruptIsNormalStop(t *testing.T) {
	c := newCmdChain(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := newCmdFakeDB(cmdMeta(c, 1), cmdMeta(c, 2))
	db.priceLag = func(int) time.Duration { cancel(); return 0 }
	out := filepath.Join(t.TempDir(), "out")
	var logw bytes.Buffer
	x := newCmdExport(t, db, holderExportOptions{IDsFile: writeIDs(t, "1\n2\n"), OutDir: out}, &logw)
	if err := x.run(ctx); err != nil {
		t.Fatalf("run after an interrupt: %v, want nil", err)
	}
	if !strings.Contains(logw.String(), "stopped; run the same command again") {
		t.Errorf("log %q, want the stop line", logw.String())
	}
	if rows := mustRows(t, out); len(rows) != 0 {
		t.Errorf("%d rows, want none", len(rows))
	}
	for _, f := range []string{holderCSVFile, holderRunsFile} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s after the stop: %v", f, err)
		}
	}
}
