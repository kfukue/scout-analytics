package main

// The holder export command (-export-holders): call ids in, files out. It
// wires the pieces of the holders_*.go files and adds little logic of its own:
//
//   - parse: the flags (holderExportOptions) and the call-id file
//     (parseHolderIDFile: the first column of each line);
//   - plan: the call details from the database (HolderCallMetas, one read-only
//     transaction), minus the calls already in holders.jsonl, cut to
//     -export-holders-max (planHolderExport);
//   - -dry-run: the eth_getLogs estimate (holderCostEstimate), no node request,
//     nothing written;
//   - run loop: per call, the pauser (waitClear), then the node reads and the
//     row (exportCall), appended to holders.jsonl;
//   - finish: holders.csv rebuilt, one line in runs.jsonl, the end summary.
//
// The database is only read (holders_db.go). The node is read through the
// export's own slow client (holderOnchainConfig). Files are written only to
// the folder checkHolderOutDir judged, never to the flag's path joined again.
// main dispatches the command before Migrate and registerTools: it never
// applies the schema and starts nothing else. One goroutine.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// holderColumnsFile: the column list for the Python side.
	holderColumnsFile = "columns.json"
	// holderMaxFailsInARow: the run stops after this many calls in a row whose
	// node reads failed (the node is likely down).
	holderMaxFailsInARow = 5
	// holderV4SearchStreams: the Initialize searches of a v4 pool-id search
	// (holderV4PoolIDs) on a token not searched yet, token as currency0 and as
	// currency1. Each reads blocks 1 to the head in SCOUT_RPC_HISTORY_CHUNK
	// ranges, one eth_getLogs per range (holderV4SearchRequests).
	holderV4SearchStreams = 2
)

// holderExportOptions: the command's flags.
type holderExportOptions struct {
	IDsFile string // -export-holders: the call-id file
	OutDir  string // -export-holders-out
	Max     int    // -export-holders-max: calls taken up this run; 0 = no limit
	DryRun  bool   // -dry-run
}

// holderExportFlags registers -export-holders, -export-holders-out and
// -export-holders-max on fs. The caller sets DryRun from the shared -dry-run.
func holderExportFlags(fs *flag.FlagSet) *holderExportOptions {
	o := &holderExportOptions{}
	fs.StringVar(&o.IDsFile, "export-holders", "", "export the holder features of the call ids in this file (one per line, or a CSV whose first column is call_id) to -export-holders-out, then exit (reads the database, writes nothing to it; reads the node at its own slow rate; with -dry-run: estimate only)")
	fs.StringVar(&o.OutDir, "export-holders-out", filepath.FromSlash(holderDefaultOutDir), "with -export-holders: the output folder (outside every checkout, or in its data folder)")
	fs.IntVar(&o.Max, "export-holders-max", 0, "with -export-holders: take up at most this many calls this run (0 = no limit)")
	return o
}

// check refuses -export-holders-out and -export-holders-max without
// -export-holders, and a negative maximum.
func (o holderExportOptions) check() error {
	if o.IDsFile == "" {
		if o.Max != 0 || o.OutDir != filepath.FromSlash(holderDefaultOutDir) {
			return errors.New("-export-holders-out and -export-holders-max are used with -export-holders")
		}
		return nil
	}
	if o.Max < 0 {
		return fmt.Errorf("-export-holders-max %d: want 0 (no limit) or more", o.Max)
	}
	return nil
}

// readHolderIDs reads the call-id file (parseHolderIDFile).
func readHolderIDs(path string) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("call id file: %w", err)
	}
	defer f.Close() // read only: a close error changes nothing
	return parseHolderIDFile(f)
}

// holderExportDB: the database reads of the export, all read-only. It has no
// Migrate: the command cannot apply the schema.
type holderExportDB interface {
	HolderCallMetas(ctx context.Context, ids []int64) ([]holderCallMeta, []int64, error)
	LatestPriceLag(ctx context.Context, now time.Time, recent, old, recentAge time.Duration) (time.Duration, error)
}

// holderExport is one run of the command. Not safe for concurrent use.
type holderExport struct {
	opts     holderExportOptions
	db       holderExportDB
	src      *onchainSource // the export's own node client (holderOnchainConfig)
	pauser   *holderPauser
	hc       holderExportConfig
	oc       onchainConfig // src's settings
	pc       priceConfig   // the tracker's settings: its latest-price cut-offs
	venues   holderVenueConfig
	stateDir string
	log      io.Writer
	now      func() time.Time
	// judgeOut is checkHolderOutDir; tests replace it.
	judgeOut func(out string, repoRoots []string, stateDir string) (string, error)
}

// newHolderExport reads the SCOUT_HOLDERS_* settings and builds the export's
// node client and pauser. No request is made.
func newHolderExport(cfg *config, db holderExportDB, opts holderExportOptions, logw io.Writer) (*holderExport, error) {
	hc, err := loadHolderExportConfig()
	if err != nil {
		return nil, err
	}
	oc := holderOnchainConfig(cfg.Price.Onchain, hc)
	x := &holderExport{opts: opts, db: db, src: newOnchainSource(oc), hc: hc, oc: oc, pc: cfg.Price,
		venues:   holderVenueConfig{PoolManagerV4: oc.PoolManagerV4, PonsFactory: oc.PonsFactory},
		stateDir: cfg.StateDir, log: logw, now: time.Now, judgeOut: checkHolderOutDir}
	// Both checks are always passed (a nil one would panic); waitClear skips
	// a check whose limit is 0.
	x.pauser = newHolderPauser(hc, x.nodeLag, x.priceLag, nil, nil, logw)
	return x, nil
}

// nodeLag: how far the node is behind, asked through the export's client.
func (x *holderExport) nodeLag(ctx context.Context) (time.Duration, error) {
	return nodeLag(ctx, x.src.rpc, x.now())
}

// priceLag: how far the tracker's latest-price pass is behind, with the
// tracker's own cut-offs (the values CountDueLatest gets).
func (x *holderExport) priceLag(ctx context.Context) (time.Duration, error) {
	return x.db.LatestPriceLag(ctx, x.now(), x.pc.LatestRecent, x.pc.LatestOld, latestRecentAge)
}

// holderPlan: what one run does.
type holderPlan struct {
	IDs     int              // distinct ids in the file
	Missing []int64          // ids not in scout_calls
	Done    int              // calls already in holders.jsonl
	ToDo    int              // calls without a row
	ThisRun []holderCallMeta // the calls without a row, in file order, cut to max
}

// planHolderExport drops the calls in done and keeps at most limit (0 = all) of
// the rest. Pure.
func planHolderExport(ids int, metas []holderCallMeta, missing []int64, done map[int64]bool, limit int) holderPlan {
	p := holderPlan{IDs: ids, Missing: missing}
	for _, m := range metas {
		if done[m.CallID] {
			p.Done++
			continue
		}
		p.ToDo++
		if limit == 0 || len(p.ThisRun) < limit {
			p.ThisRun = append(p.ThisRun, m)
		}
	}
	return p
}

// holderDoneIDs returns the call ids already in dir's holders.jsonl, without
// changing the file (a partial last line is not counted).
func holderDoneIDs(dir string) (map[int64]bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, holderRowsFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[int64]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", holderRowsFile, err)
	}
	return holderRowIDs(data[:completeLines(data)])
}

// run is the command: plan, then the dry-run estimate or the export.
func (x *holderExport) run(ctx context.Context) error {
	ids, err := readHolderIDs(x.opts.IDsFile)
	if err != nil {
		return err
	}
	dir, err := x.judgeOut(x.opts.OutDir, nil, x.stateDir)
	if err != nil {
		return err
	}
	done, err := holderDoneIDs(dir)
	if err != nil {
		return err
	}
	metas, missing, err := x.db.HolderCallMetas(ctx, ids)
	if err != nil {
		return err
	}
	plan := planHolderExport(len(ids), metas, missing, done, x.opts.Max)
	x.logStart(plan, dir)
	if x.opts.DryRun {
		return x.dryRun(plan)
	}
	return x.export(ctx, dir, plan)
}

// logStart: what the run found and will do.
func (x *holderExport) logStart(p holderPlan, dir string) {
	fmt.Fprintf(x.log, "holder export: %d call ids read (%d not in scout_calls), %d already in %s, %d to do; this run: %d (-export-holders-max %d)\n",
		p.IDs, len(p.Missing), p.Done, filepath.Join(dir, holderRowsFile), p.ToDo, len(p.ThisRun), x.opts.Max)
	if len(p.Missing) > 0 {
		fmt.Fprintf(x.log, "holder export: ids not in scout_calls (no row is written for them): %v\n", p.Missing)
	}
}

// holderCostCalls is what the estimate knows of each call before any node
// read: its entry block, the age its post showed (none: estimated as a new
// token, counted in unknownAge) and the liquidity filters of its stored
// venues (a v4 venue with the state's pool id only). Calls skipped before any
// read cost nothing and are left out; v4 counts the calls with a v4 pool-id
// search.
func holderCostCalls(metas []holderCallMeta, cfg holderVenueConfig) (calls []holderCostCall, skipped, unknownAge, v4 int) {
	for _, m := range metas {
		if holderSkipBeforeFetch(m) != "" {
			skipped++
			continue
		}
		st := *m.State
		c := holderCostCall{EntryBlock: st.EntryBlock,
			Filters: holderLiquidityFilterCount(holderFetchVenues(st, cfg), holderAllPoolIDs(st, nil, nil))}
		if m.PostedAgeS != nil {
			c.Age = time.Duration(*m.PostedAgeS) * time.Second
		} else {
			unknownAge++
		}
		if holderV4VenueAt(st, ^uint64(0)) {
			v4++
		}
		calls = append(calls, c)
	}
	return calls, skipped, unknownAge, v4
}

// dryRun logs the eth_getLogs estimate of this run's calls and the time it
// takes at the export's rate. No node request, nothing written. The v4
// pool-id searches read up to the head, which the dry run does not ask: it
// counts them up to the largest entry block of the calls instead, a lower
// bound (the log line says "at least").
func (x *holderExport) dryRun(p holderPlan) error {
	calls, skipped, unknownAge, v4 := holderCostCalls(p.ThisRun, x.venues)
	_, total, err := holderCostEstimate(calls, x.oc.LogChunk, x.oc.HistoryChunk)
	if err != nil {
		return err
	}
	headAtLeast := holderMaxEntryBlock(calls)
	v4Logs := int64(v4) * holderV4SearchRequests(headAtLeast, x.oc.HistoryChunk)
	total += v4Logs
	at := time.Duration(float64(total) / float64(x.hc.RPS) * float64(time.Second))
	fmt.Fprintf(x.log, "holder export (dry run): about %d eth_getLogs for %d calls (%d skipped before any read; %d without a posted age, estimated as new tokens; the v4 pool searches of %d calls: at least %d, counted up to block %d, the largest entry block, as the head is not asked), at least %s at %d requests/s (the other requests come on top); nothing read from the node, nothing written\n",
		total, len(p.ThisRun), skipped, unknownAge, v4, v4Logs, headAtLeast, at.Round(time.Second), x.hc.RPS)
	return nil
}

// holderLoopResult: what the run loop did.
type holderLoopResult struct {
	Rows   []holderRow // written this run
	Failed []int64     // calls whose node reads failed (no row written)
	Paused time.Duration
}

// export writes columns.json, runs the loop on dir's holders.jsonl and then
// finishes (always, also when the loop stopped on an error).
func (x *holderExport) export(ctx context.Context, dir string, p holderPlan) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	if err := writeHolderColumns(dir); err != nil {
		return err
	}
	w, err := openHolderRowWriter(dir)
	if err != nil {
		return err
	}
	rec := holderRunRecord{StartedAt: x.now().UTC(), CodeVersion: holderCodeVersion(),
		Settings: holderRunSettings(x.hc, x.oc, x.pc, x.opts)}
	res, loopErr := x.loop(ctx, w, p.ThisRun, rec.CodeVersion)
	if ctx.Err() != nil && errors.Is(loopErr, ctx.Err()) {
		fmt.Fprintln(x.log, "holder export: stopped; run the same command again to go on where it stopped")
		loopErr = nil // an interrupt is a normal stop; the files are finished below
	}
	closeErr := w.Close()
	return errors.Join(loopErr, closeErr, x.finish(dir, rec, res, p))
}

// writeHolderColumns writes dir's columns.json: every column in order, and
// the pre_hold_* feature columns (the only ones a model may use).
func writeHolderColumns(dir string) error {
	b, err := json.MarshalIndent(struct {
		Columns        []string `json:"columns"`
		PreHoldColumns []string `json:"pre_hold_columns"`
	}{holderColumnNames(), holderPreHoldColumns()}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", holderColumnsFile, err)
	}
	if err := os.WriteFile(filepath.Join(dir, holderColumnsFile), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", holderColumnsFile, err)
	}
	return nil
}

// holderRunSettings: the settings put in the run record. An allowlist: these
// names only, each value formatted from a parsed number or duration, so no
// URL, password or token can reach runs.jsonl.
func holderRunSettings(hc holderExportConfig, oc onchainConfig, pc priceConfig, o holderExportOptions) map[string]string {
	return map[string]string{
		holdersRPSSetting:          strconv.Itoa(hc.RPS),
		holdersMaxNodeLagSetting:   hc.MaxNodeLag.String(),
		holdersMaxPriceLagSetting:  hc.MaxPriceLag.String(),
		holdersMaxPauseSetting:     hc.MaxPause.String(),
		rpcLogChunkSetting:         strconv.FormatUint(oc.LogChunk, 10),
		rpcHistoryChunkSetting:     strconv.FormatUint(oc.HistoryChunk, 10),
		latestRefreshRecentSetting: pc.LatestRecent.String(),
		latestRefreshOldSetting:    pc.LatestOld.String(),
		"export_holders_max":       strconv.Itoa(o.Max),
		"sniper_window_blocks":     strconv.Itoa(defaultSniperWindowBlocks),
	}
}

// loop exports calls one by one. It stops on ctx's end, a pause past
// SCOUT_HOLDERS_MAX_PAUSE, a write error, or holderMaxFailsInARow failed
// calls in a row; a call whose node reads fail gets no row (a later run
// tries it again).
func (x *holderExport) loop(ctx context.Context, w *holderRowWriter, calls []holderCallMeta, version string) (holderLoopResult, error) {
	var res holderLoopResult
	inARow := 0
	for i, m := range calls {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		row, err := x.exportCall(ctx, m, version, &res.Paused)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errHolderPausedTooLong) {
				return res, err
			}
			res.Failed = append(res.Failed, m.CallID)
			fmt.Fprintf(x.log, "holder export: call %d (%d/%d) failed, no row written (a later run tries it again): %v\n", m.CallID, i+1, len(calls), err)
			if inARow++; inARow >= holderMaxFailsInARow {
				return res, fmt.Errorf("holder export: %d calls in a row failed, the last: %w", inARow, err)
			}
			continue
		}
		inARow = 0
		if err := w.Append(row); err != nil {
			return res, err
		}
		res.Rows = append(res.Rows, row)
		x.logRow(i+1, len(calls), row)
	}
	return res, nil
}

// logRow: one progress line per call written.
func (x *holderExport) logRow(n, of int, r holderRow) {
	what := string(r.Status)
	if r.SkipReason != nil {
		what += " (" + string(*r.SkipReason) + ")"
	}
	var logs int64
	if r.RPCLogsRequests != nil {
		logs = *r.RPCLogsRequests
	}
	fmt.Fprintf(x.log, "holder export: call %d (%d/%d): %s, %d eth_getLogs\n", r.CallID, n, of, what, logs)
}

// exportCall builds one call's row. A call that cannot be read gets its
// skipped row with no node request; otherwise the pauser waits until the node
// and the tracker are within their limits (the wait is added to paused), then
// the call is read and its row built.
func (x *holderExport) exportCall(ctx context.Context, m holderCallMeta, version string, paused *time.Duration) (holderRow, error) {
	stamp := holderRowStamp{ExportedAt: x.now().UTC(), CodeVersion: version}
	if reason := holderSkipBeforeFetch(m); reason != "" {
		return skippedHolderRow(m, reason, stamp), nil
	}
	p, err := x.pauser.waitClear(ctx)
	*paused += p
	if err != nil {
		return holderRow{}, err
	}
	start := x.now()
	in, err := x.readCall(ctx, m)
	if err != nil {
		return holderRow{}, fmt.Errorf("call %d: %w", m.CallID, err)
	}
	stamp.ExportedAt, stamp.Elapsed = x.now().UTC(), x.now().Sub(start)
	return buildHolderRow(in, stamp)
}

// readCall does one call's node reads: the head block, the token's v4 pool
// ids, then its logs (fetchHolderLogs). The head and pool-id requests are
// added to the logs' request counts.
func (x *holderExport) readCall(ctx context.Context, m holderCallMeta) (holderRowInput, error) {
	logCtx, logN := withReqCounter(ctx)
	otherCtx, otherN := withReqCounter(ctx)
	head, err := x.src.rpc.blockNumber(otherCtx)
	if err != nil {
		return holderRowInput{}, fmt.Errorf("head block: %w", err)
	}
	pre, all, err := x.src.holderV4PoolIDs(logCtx, m.Token, *m.State, head)
	if err != nil {
		return holderRowInput{}, err
	}
	logs, err := x.src.fetchHolderLogs(ctx, holderFetchInputFor(m, pre, all, x.venues))
	if err != nil {
		return holderRowInput{}, err
	}
	logs.LogRequests += logN.Load()
	logs.OtherRequests += otherN.Load()
	return holderRowInput{Meta: m, Logs: logs, V4PoolIDsPre: pre, V4PoolIDsAll: all, Config: x.venues,
		SniperWindowBlocks: defaultSniperWindowBlocks}, nil
}

// finish rebuilds holders.csv, appends the run record and logs the summary.
// Each step runs even when an earlier one failed.
func (x *holderExport) finish(dir string, rec holderRunRecord, res holderLoopResult, p holderPlan) error {
	csvErr := rebuildHolderCSV(dir)
	rec.EndedAt = x.now().UTC()
	rec.addRows(res.Rows)
	rec.Failed = len(res.Failed)
	rec.PausedS = res.Paused.Seconds()
	runErr := appendHolderRun(dir, rec)
	left := p.ToDo - len(res.Rows)
	fmt.Fprintf(x.log, "holder export: end: %d rows written this run %v, %d failed, %d eth_getLogs, paused %s; %d calls still to do; files in %s\n",
		rec.Calls, rec.ByStatus, rec.Failed, rec.LogRequestsTotal, res.Paused.Round(time.Second), left, dir)
	return errors.Join(csvErr, runErr)
}

// runHolderExportCommand is -export-holders: it opens the database itself
// (no Migrate, no registerTools) and runs the export.
func runHolderExportCommand(ctx context.Context, cfg *config, opts holderExportOptions, logw io.Writer) error {
	db, _, err := openScoutStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if db == nil {
		return errors.New("-export-holders needs the database, which is turned off")
	}
	defer db.Close()
	x, err := newHolderExport(cfg, db, opts, logw)
	if err != nil {
		return err
	}
	return x.run(ctx)
}

// logLineWriter writes each Write as one line of l (which adds the time).
type logLineWriter struct{ l *log.Logger }

func (w logLineWriter) Write(p []byte) (int, error) {
	w.l.Print(string(p))
	return len(p), nil
}
