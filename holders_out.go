package main

// The holder export's files, in an output folder the caller passes in (see
// checkHolderOutDir in holders_run.go). File I/O only: no node, no database.
//
//   - holders.jsonl: one JSON object per finished call (holderRow), appended
//     and synced line by line, so a run that stops loses at most the line it
//     was writing. On open, the call ids already there are read (a resumed
//     run skips them) and a partial last line is cut off (that call is
//     redone).
//   - holders.csv: rebuilt from holders.jsonl (a temp file, then a rename), in
//     the fixed column order of holders_columns.go.
//   - runs.jsonl: one line per run (holderRunRecord).

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"time"
)

const (
	holderRowsFile = "holders.jsonl"
	holderCSVFile  = "holders.csv"
	holderRunsFile = "runs.jsonl"
)

// holderRowWriter appends rows to holders.jsonl. Not safe for concurrent use.
type holderRowWriter struct {
	f    *os.File
	done map[int64]bool
}

// openHolderRowWriter opens dir's holders.jsonl for appending (creating it),
// after reading the call ids already written and cutting off a partial last
// line. A complete line that is not a row is an error: the file is not
// touched further.
func openHolderRowWriter(dir string) (*holderRowWriter, error) {
	path := filepath.Join(dir, holderRowsFile)
	done, err := recoverHolderRows(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", holderRowsFile, err)
	}
	return &holderRowWriter{f: f, done: done}, nil
}

// recoverHolderRows returns the call ids in path (none when it does not
// exist), after truncating it to its last complete line.
func recoverHolderRows(path string) (map[int64]bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[int64]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", holderRowsFile, err)
	}
	complete := completeLines(data)
	ids, err := holderRowIDs(data[:complete])
	if err != nil {
		return nil, err
	}
	if complete < len(data) {
		if err := os.Truncate(path, int64(complete)); err != nil {
			return nil, fmt.Errorf("cut the partial last line of %s: %w", holderRowsFile, err)
		}
	}
	return ids, nil
}

// completeLines returns the length of data up to and including its last
// newline (0 when there is none).
func completeLines(data []byte) int {
	return bytes.LastIndexByte(data, '\n') + 1
}

// holderRowIDs reads the call_id of every non-empty line.
func holderRowIDs(data []byte) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r struct {
			CallID *int64 `json:"call_id"`
		}
		if err := json.Unmarshal(line, &r); err != nil || r.CallID == nil {
			return nil, fmt.Errorf("%s line %d is not a holder row (call_id missing or bad JSON: %v)", holderRowsFile, i+1, err)
		}
		ids[*r.CallID] = true
	}
	return ids, nil
}

// Done reports whether callID already has a row.
func (w *holderRowWriter) Done(callID int64) bool { return w.done[callID] }

// Append writes r as one line and syncs the file. A call that already has a
// row is refused, so a run never writes it twice. The caller must stop the run
// on any Append error, with no retry in the same process: a failed write can
// leave part of a line, and the next Append would continue it. A new run
// reopens the file (openHolderRowWriter), which cuts that partial line off.
func (w *holderRowWriter) Append(r holderRow) error {
	if w.done[r.CallID] {
		return fmt.Errorf("call %d already has a row in %s", r.CallID, holderRowsFile)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("call %d: encode row: %w", r.CallID, err)
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("call %d: write %s: %w", r.CallID, holderRowsFile, err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("call %d: sync %s: %w", r.CallID, holderRowsFile, err)
	}
	w.done[r.CallID] = true
	return nil
}

// Close closes the file.
func (w *holderRowWriter) Close() error { return w.f.Close() }

// readHolderRows reads every complete line of dir's holders.jsonl as a row,
// in file order (a partial last line is ignored, not cut: the writer does
// that). A missing file gives no rows.
func readHolderRows(dir string) ([]holderRow, error) {
	data, err := os.ReadFile(filepath.Join(dir, holderRowsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", holderRowsFile, err)
	}
	var rows []holderRow
	for i, line := range bytes.Split(data[:completeLines(data)], []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r holderRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", holderRowsFile, i+1, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// rebuildHolderCSV writes dir's holders.csv from its holders.jsonl: header
// and rows in the fixed column order, through a temp file renamed over the
// old CSV, so a reader never sees half a file. Without a holders.jsonl the
// CSV holds the header only.
func rebuildHolderCSV(dir string) error {
	rows, err := readHolderRows(dir)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, holderCSVFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp csv: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // fails harmlessly once renamed
	if err := writeHolderCSV(tmp, rows); err != nil {
		_ = tmp.Close() // the write error is the one reported
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp csv: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, holderCSVFile)); err != nil {
		return fmt.Errorf("replace %s: %w", holderCSVFile, err)
	}
	return nil
}

// writeHolderCSV writes the header and rows to f and syncs it.
func writeHolderCSV(f *os.File, rows []holderRow) error {
	w := csv.NewWriter(f)
	if err := w.Write(holderColumnNames()); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, r := range rows {
		if err := w.Write(holderCSVValues(r)); err != nil {
			return fmt.Errorf("write csv row of call %d: %w", r.CallID, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("write csv: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync csv: %w", err)
	}
	return nil
}

// holderRequestStats: JSON-RPC requests per call over a run.
type holderRequestStats struct {
	Total  int64 `json:"total"`
	Median int64 `json:"median"` // nearest rank
	P90    int64 `json:"p90"`    // nearest rank
	Max    int64 `json:"max"`
}

// holderRunRecord is one line of runs.jsonl.
type holderRunRecord struct {
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	CodeVersion string    `json:"code_version"`
	// Settings: as the caller passes them (names and values), written as is.
	// Never put a node URL, a database URL (DSN), a token or any other secret
	// in it: runs.jsonl is plain text next to the export and travels with it.
	Settings map[string]string `json:"settings"`
	Calls    int               `json:"calls"` // rows written in this run
	ByStatus map[string]int    `json:"by_status"`
	BySkip   map[string]int    `json:"by_skip_reason"`
	// Requests: per call, over the calls with request counts (every call
	// that reached the node).
	Requests         holderRequestStats `json:"requests_per_call"`
	LogRequestsTotal int64              `json:"eth_getlogs_total"`
	PausedS          float64            `json:"paused_s"`
}

// addRows fills the counts and request figures from the rows of this run.
func (rec *holderRunRecord) addRows(rows []holderRow) {
	rec.Calls = len(rows)
	rec.ByStatus, rec.BySkip = map[string]int{}, map[string]int{}
	var perCall []int64
	for _, r := range rows {
		rec.ByStatus[string(r.Status)]++
		if r.SkipReason != nil {
			rec.BySkip[string(*r.SkipReason)]++
		}
		if r.RPCRequestsTotal != nil {
			perCall = append(perCall, *r.RPCRequestsTotal)
		}
		if r.RPCLogsRequests != nil {
			rec.LogRequestsTotal += *r.RPCLogsRequests
		}
	}
	rec.Requests = requestStats(perCall)
}

// requestStats: total, median, p90 (nearest rank) and max of n; all 0 for
// none.
func requestStats(n []int64) holderRequestStats {
	if len(n) == 0 {
		return holderRequestStats{}
	}
	s := slices.Sorted(slices.Values(n))
	var st holderRequestStats
	for _, v := range s {
		st.Total += v
	}
	st.Median, st.P90, st.Max = nearestRank(s, 50), nearestRank(s, 90), s[len(s)-1]
	return st
}

// nearestRank returns the p-th percentile of sorted (non-empty): the value at
// rank ⌈p/100 × n⌉.
func nearestRank(sorted []int64, p int) int64 {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// appendHolderRun appends rec to dir's runs.jsonl and syncs it.
func appendHolderRun(dir string, rec holderRunRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, holderRunsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", holderRunsFile, err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close() // the write error is the one reported
		return fmt.Errorf("write %s: %w", holderRunsFile, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error is the one reported
		return fmt.Errorf("sync %s: %w", holderRunsFile, err)
	}
	return f.Close()
}

// holderCodeVersion returns the commit the binary was built from (with
// "-dirty" for uncommitted changes), or "" when the build has no VCS
// information (go run, go test).
func holderCodeVersion() string {
	info, ok := debug.ReadBuildInfo()
	return codeVersionOf(info, ok)
}

// codeVersionOf reads vcs.revision and vcs.modified from build info.
func codeVersionOf(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}
