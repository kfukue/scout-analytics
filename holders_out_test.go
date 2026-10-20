package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustOpenWriter(t *testing.T, dir string) *holderRowWriter {
	t.Helper()
	w, err := openHolderRowWriter(dir)
	if err != nil {
		t.Fatalf("openHolderRowWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() }) // a second Close after the test's own only returns an error
	return w
}

// outRow is a small finished row for call id.
func outRow(id int64) holderRow {
	r := holderRow{}
	r.CallID, r.Token, r.Chain, r.Status = id, hrToken, "evm", holderRowOK
	r.ExportedAt = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	r.VenuesPre = []string{hrPool}
	r.holderPreCols.Holders = valPtr(int(id))
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Rows are appended one line each; a reopened writer knows them and refuses
// to write them again.
func TestHoldersWriterResume(t *testing.T) {
	dir := t.TempDir()
	w := mustOpenWriter(t, dir)
	for _, id := range []int64{3, 1} {
		if err := w.Append(outRow(id)); err != nil {
			t.Fatalf("append %d: %v", id, err)
		}
	}
	if err := w.Append(outRow(3)); err == nil {
		t.Errorf("appending call 3 twice: no error")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := strings.Count(readFile(t, filepath.Join(dir, holderRowsFile)), "\n"); n != 2 {
		t.Errorf("%d lines, want 2", n)
	}
	w2 := mustOpenWriter(t, dir)
	for id, want := range map[int64]bool{1: true, 3: true, 2: false} {
		if got := w2.Done(id); got != want {
			t.Errorf("Done(%d) = %v, want %v", id, got, want)
		}
	}
	if err := w2.Append(outRow(2)); err != nil {
		t.Fatalf("append 2 after resume: %v", err)
	}
	rows, err := readHolderRows(dir)
	if err != nil {
		t.Fatalf("readHolderRows: %v", err)
	}
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r.CallID)
	}
	if !slices.Equal(ids, []int64{3, 1, 2}) {
		t.Errorf("rows %v, want 3 1 2 (file order)", ids)
	}
}

// A run that stopped mid-line: the partial line is cut off on open (that call
// is redone) and the next row starts on a line of its own.
func TestHoldersWriterTruncatedLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, holderRowsFile)
	good, err := json.Marshal(outRow(5))
	if err != nil {
		t.Fatal(err)
	}
	partial, err := json.Marshal(outRow(6))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(good, '\n'), partial[:len(partial)/2]...), 0o644); err != nil {
		t.Fatal(err)
	}
	w := mustOpenWriter(t, dir)
	if !w.Done(5) || w.Done(6) {
		t.Errorf("Done(5) %v Done(6) %v, want true false", w.Done(5), w.Done(6))
	}
	if got := readFile(t, path); got != string(good)+"\n" {
		t.Errorf("file after open = %q, want only the complete line", got)
	}
	if err := w.Append(outRow(6)); err != nil {
		t.Fatalf("append 6: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"call_id":6`) {
		t.Errorf("lines %q, want two, the second call 6", lines)
	}
}

// A complete line that is not a row stops the writer instead of being
// skipped or overwritten.
func TestHoldersWriterCorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, holderRowsFile)
	content := "{\"call_id\":1}\nnot json\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openHolderRowWriter(dir); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("open: error %v, want one naming line 2", err)
	}
	if got := readFile(t, path); got != content {
		t.Errorf("file changed to %q", got)
	}
}

// holders.csv: the header in column order, one record per JSONL row, NULL
// empty; a rebuild replaces the old file and leaves no temp file.
func TestHoldersCSVRebuild(t *testing.T) {
	dir := t.TempDir()
	w := mustOpenWriter(t, dir)
	full := mustRow(t, hrInput())
	skipped := skippedHolderRow(holderCallMeta{CallID: 9, Token: hrToken, Chain: "evm"}, holderSkipNoTrackingRow, hrStamp)
	for _, r := range []holderRow{full, skipped} {
		if err := w.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, holderCSVFile), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rebuildHolderCSV(dir); err != nil {
		t.Fatalf("rebuildHolderCSV: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, holderCSVFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d records, want header + 2", len(recs))
	}
	if !slices.Equal(recs[0], holderColumnsWant) {
		t.Errorf("header %v, want %v", recs[0], holderColumnsWant)
	}
	// The JSONL round trip gives back exactly what the row formats to.
	if !slices.Equal(recs[1], holderCSVValues(full)) {
		t.Errorf("row 1 %v, want %v", recs[1], holderCSVValues(full))
	}
	col := func(rec []string, name string) string { return rec[slices.Index(holderColumnsWant, name)] }
	if got := col(recs[1], "pre_hold_early_buyers"); got != strings.Join([]string{hrS1, hrS2, hrS3, hrB4, hrB5}, ";") {
		t.Errorf("early buyers %q", got)
	}
	if got := col(recs[1], "pre_hold_supply_raw"); got != tokens(1000).String() {
		t.Errorf("supply %q, want %s", got, tokens(1000))
	}
	if got, want := col(recs[2], "skip_reason"), "no_tracking_row"; got != want {
		t.Errorf("skip reason %q, want %q", got, want)
	}
	for _, c := range holderPreHoldColumns() {
		if v := col(recs[2], c); v != "" {
			t.Errorf("skipped row: %s = %q, want empty (NULL)", c, v)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{holderCSVFile, holderRowsFile}; !slices.Equal(names, want) {
		t.Errorf("folder holds %v, want %v (no temp file left)", names, want)
	}
}

// A folder without holders.jsonl (a run that wrote no row yet) gets a
// header-only holders.csv, not an error.
func TestHoldersCSVRebuildNoRows(t *testing.T) {
	dir := t.TempDir()
	if err := rebuildHolderCSV(dir); err != nil {
		t.Fatalf("rebuildHolderCSV on an empty folder: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, holderCSVFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(recs) != 1 || !slices.Equal(recs[0], holderColumnsWant) {
		t.Errorf("records %v, want the header only", recs)
	}
	if _, err := os.Stat(filepath.Join(dir, holderRowsFile)); err == nil {
		t.Errorf("rebuild created %s; it must only read it", holderRowsFile)
	}
}

// runs.jsonl: one line per run, counts by status and reason, request figures.
func TestHoldersRunRecord(t *testing.T) {
	var rows []holderRow
	for i, n := range []int64{10, 2, 7, 30, 4} {
		r := outRow(int64(i + 1))
		r.RPCRequestsTotal, r.RPCLogsRequests = valPtr(n), valPtr(n-1)
		rows = append(rows, r)
	}
	rows = append(rows, skippedHolderRow(holderCallMeta{CallID: 50}, holderSkipNotEVM, hrStamp))
	rows[0].Status = holderRowCapped
	rec := holderRunRecord{StartedAt: hrEntryAt, EndedAt: hrEntryAt.Add(time.Hour), CodeVersion: "abc",
		Settings: map[string]string{"SCOUT_RPC_LOG_CHUNK": "200000"}, PausedS: 12.5}
	rec.addRows(rows)
	if rec.Calls != 6 || rec.ByStatus["ok"] != 4 || rec.ByStatus["capped"] != 1 || rec.ByStatus["skipped"] != 1 ||
		rec.BySkip["not_evm"] != 1 || len(rec.BySkip) != 1 {
		t.Errorf("counts: calls %d, status %v, skips %v", rec.Calls, rec.ByStatus, rec.BySkip)
	}
	if want := (holderRequestStats{Total: 53, Median: 7, P90: 30, Max: 30}); rec.Requests != want {
		t.Errorf("requests %+v, want %+v", rec.Requests, want)
	}
	if rec.LogRequestsTotal != 48 {
		t.Errorf("eth_getLogs total %d, want 48", rec.LogRequestsTotal)
	}
	dir := t.TempDir()
	for range 2 {
		if err := appendHolderRun(dir, rec); err != nil {
			t.Fatalf("appendHolderRun: %v", err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(dir, holderRunsFile)), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
	var back holderRunRecord
	if err := json.Unmarshal([]byte(lines[1]), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Requests != rec.Requests || back.Settings["SCOUT_RPC_LOG_CHUNK"] != "200000" || back.PausedS != 12.5 ||
		!back.EndedAt.Equal(rec.EndedAt) {
		t.Errorf("decoded %+v, want %+v", back, rec)
	}
}

func TestHoldersRequestStats(t *testing.T) {
	cases := []struct {
		in   []int64
		want holderRequestStats
	}{
		{nil, holderRequestStats{}},
		{[]int64{5}, holderRequestStats{Total: 5, Median: 5, P90: 5, Max: 5}},
		{[]int64{4, 1}, holderRequestStats{Total: 5, Median: 1, P90: 4, Max: 4}},
		{[]int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, holderRequestStats{Total: 55, Median: 5, P90: 9, Max: 10}},
	}
	for _, tc := range cases {
		if got := requestStats(tc.in); got != tc.want {
			t.Errorf("requestStats(%v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestHoldersCodeVersion(t *testing.T) {
	info := func(kv ...string) *debug.BuildInfo {
		bi := &debug.BuildInfo{}
		for i := 0; i < len(kv); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return bi
	}
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build info", nil, false, ""},
		{"no vcs", info("GOOS", "windows"), true, ""},
		{"clean", info("vcs.revision", "abc123", "vcs.modified", "false"), true, "abc123"},
		{"dirty", info("vcs.revision", "abc123", "vcs.modified", "true"), true, "abc123-dirty"},
	}
	for _, tc := range cases {
		if got := codeVersionOf(tc.bi, tc.ok); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	_ = holderCodeVersion() // must not panic under go test (no vcs stamp)
}
