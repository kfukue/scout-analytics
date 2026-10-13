package main

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"
)

// TestWebPerceptorFilterLatestDB: the Perceptor filter of /api/calls (and its
// counts) uses each token's latest completed Perceptor report with a verdict,
// read from the database: live scans, the 12-minute re-check (live) and the
// re-scans with the 2-hour re-check (rescan), ordered by completion time. A
// failed or timed-out scan never counts, "not enough data yet" never replaces
// a real verdict. The call-time fields, the dataset view and the Analytics
// "verdict at the call" keep the call-time answer.
func TestWebPerceptorFilterLatestDB(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	ctx := context.Background()
	perc := mustTool(t, fx.st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	t0 := time.Date(2026, 9, 1, 0, 5, 0, 0, time.UTC)
	inv := func(key string, ownCall bool, ca string, requested, completed time.Time, kind, status, level string, details json.RawMessage) {
		t.Helper()
		var call *int
		if ownCall {
			call = ip(fx.ids[key])
		}
		if _, err := fx.st.InsertScoutInvestigation(ctx, &ScoutInvestigation{CallID: call, ToolID: perc, ContractAddress: ca, RequestText: "/scan " + ca,
			RequestedAt: requested, CompletedAt: &completed, Status: status, VerdictLevel: level, ScanKind: kind, Details: details}); err != nil {
			t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	// alpha: clean at the call; a re-scan red_flags 30 days later
	inv("alpha", true, caAlpha, t0, t0, ScanKindLive, investigationCompleted, levelClean, nil)
	// beta: no report at the call; a re-scan caution
	inv("beta", true, caBeta, t0.Add(10*day), t0.Add(10*day), ScanKindRescan, investigationCompleted, levelCaution, nil)
	// gamma: "not enough data yet" at the call, the 12-minute re-check clean (live)
	inv("gamma", true, caGamma, t0, t0, ScanKindLive, investigationCompleted, levelInsufficient, nil)
	inv("gamma", true, caGamma, t0.Add(12*time.Minute), t0.Add(12*time.Minute), ScanKindLive, investigationCompleted, levelClean, recheckDetails(nil, 1, 12*time.Minute))
	// virt: "not enough data yet" at the call and at 12 minutes, the 2-hour re-check clean (rescan)
	inv("virt", true, caVirt, t0, t0, ScanKindLive, investigationCompleted, levelInsufficient, nil)
	inv("virt", true, caVirt, t0.Add(12*time.Minute), t0.Add(12*time.Minute), ScanKindLive, investigationCompleted, levelInsufficient, recheckDetails(nil, 1, 12*time.Minute))
	inv("virt", true, caVirt, t0.Add(2*time.Hour), t0.Add(2*time.Hour), ScanKindRescan, investigationCompleted, levelClean, recheckDetails(nil, 2, 2*time.Hour))
	// xss: clean at the call; later re-scans that failed, timed out, or gave no verdict
	inv("xss", true, caXSS, t0, t0, ScanKindLive, investigationCompleted, levelClean, nil)
	inv("xss", true, caXSS, t0.Add(20*day), t0.Add(20*day), ScanKindRescan, "failed", levelRedFlags, nil)
	inv("xss", true, caXSS, t0.Add(21*day), t0.Add(21*day), ScanKindRescan, investigationTimeout, levelUnknown, nil)
	inv("xss", true, caXSS, t0.Add(22*day), t0.Add(22*day), ScanKindRescan, investigationCompleted, levelInsufficient, nil)
	// err: a re-scan red_flags requested after a manual scan (no call) that
	// completed later, clean: completion time decides, so clean
	t1 := t0.Add(5 * day)
	inv("err", true, caErr, t1, t1.Add(time.Minute), ScanKindRescan, investigationCompleted, levelRedFlags, nil)
	inv("err", false, caErr, t1.Add(-time.Hour), t1.Add(2*time.Hour), ScanKindLive, investigationCompleted, levelClean, nil)
	// gave: the other way round: a manual scan clean requested after the
	// re-scan red_flags, completed before it: red_flags
	inv("gave", true, caGave, t1, t1.Add(3*time.Hour), ScanKindRescan, investigationCompleted, levelRedFlags, nil)
	inv("gave", false, caGave, t1.Add(time.Hour), t1.Add(time.Hour), ScanKindLive, investigationCompleted, levelClean, nil)
	// sol: a re-scan with "not enough data yet" only
	inv("sol", true, caSol, t0.Add(day), t0.Add(day), ScanKindRescan, investigationCompleted, levelInsufficient, nil)

	// before alpha's re-scan
	_, hdrClean0, _ := fx.get(t, "/api/calls?per=100&verdict=clean")
	_, hdrRed0, _ := fx.get(t, "/api/calls?per=100&verdict=red_flags")
	if keys := fx.keys(fx.calls(t, "per=100&verdict=clean").Calls); !slices.Contains(keys, "alpha") {
		t.Fatalf("before the re-scan, verdict=clean lists %v, want alpha among them", keys)
	}
	inv("alpha", true, caAlpha, t0.Add(30*day), t0.Add(30*day), ScanKindRescan, investigationCompleted, levelRedFlags, nil)

	type want struct{ shown, today, latest string }
	wants := map[string]want{
		"alpha": {levelClean, levelRedFlags, levelRedFlags},
		"beta":  {"<nil>", levelCaution, levelCaution},
		"gamma": {levelClean, "<nil>", levelClean},
		"virt":  {levelInsufficient, levelClean, levelClean},
		"xss":   {levelClean, levelInsufficient, levelClean},
		"err":   {levelClean, levelRedFlags, levelClean},
		"gave":  {levelClean, levelRedFlags, levelRedFlags},
		"sol":   {"<nil>", levelInsufficient, levelInsufficient},
		"pct":   {"<nil>", "<nil>", "<nil>"},
	}
	res := fx.calls(t, "per=100")
	byKey := map[string]webCallJSON{}
	for i, k := range fx.keys(res.Calls) {
		byKey[k] = res.Calls[i]
	}
	for k, w := range wants {
		c := byKey[k]
		if got := (want{strOrNil(c.Perceptor), strOrNil(c.PerceptorTodayVerd), strOrNil(c.PerceptorLatest)}); got != w {
			t.Errorf("%s: perceptor_verdict, perceptor_today_verdict, perceptor_latest_verdict = %v, want %v", k, got, w)
		}
	}
	for v, wantKeys := range map[string][]string{
		"clean":       {"gamma", "virt", "xss", "err"},
		"caution":     {"beta"},
		"red_flags":   {"alpha", "gave"},
		"not_scanned": {"sol", "pct"},
	} {
		got := fx.keys(fx.calls(t, "per=100&verdict="+v).Calls)
		for _, k := range wantKeys {
			if !slices.Contains(got, k) {
				t.Errorf("verdict=%s: %v, want %s among them", v, got, k)
			}
		}
		for k, w := range wants {
			b := w.latest
			if !verdictUsable(b) {
				b = "not_scanned"
			}
			if b != v && slices.Contains(got, k) {
				t.Errorf("verdict=%s lists %s, whose filter verdict is %s", v, k, w.latest)
			}
		}
	}
	// the counts (the total without a search) and the total with one agree
	for _, v := range []string{"clean", "caution", "red_flags", "not_scanned"} {
		all, searched := fx.calls(t, "per=100&verdict="+v), fx.calls(t, "per=100&q=0x&verdict="+v)
		var withAddr []string
		for _, k := range fx.keys(all.Calls) {
			if k != "sol" { // the one address without 0x
				withAddr = append(withAddr, k)
			}
		}
		if all.Total != len(all.Calls) || !reflect.DeepEqual(fx.keys(searched.Calls), withAddr) || searched.Total != len(withAddr) {
			t.Errorf("verdict=%s: total %d of %d rows; with q=0x %v (total %d), want %v", v, all.Total, len(all.Calls), fx.keys(searched.Calls), searched.Total, withAddr)
		}
	}
	// alpha's re-scan changed the ETag of both lists: the old one gets the new answer
	for _, tc := range []struct {
		query, oldTag string
		listed        bool
	}{{"verdict=clean", hdrClean0.Get("ETag"), false}, {"verdict=red_flags", hdrRed0.Get("ETag"), true}} {
		_, hdr, _ := fx.get(t, "/api/calls?per=100&"+tc.query)
		if hdr.Get("ETag") == tc.oldTag || tc.oldTag == "" {
			t.Errorf("?%s: ETag %s after alpha's re-scan, the same as before", tc.query, hdr.Get("ETag"))
		}
		if got := slices.Contains(fx.keys(fx.calls(t, "per=100&"+tc.query).Calls), "alpha"); got != tc.listed {
			t.Errorf("?%s after alpha's re-scan: alpha listed %t, want %t", tc.query, got, tc.listed)
		}
	}
	// the dataset view (model input) and the Analytics verdict at the call keep the call-time answer
	rows, _, err := fx.st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keyOf := map[int]string{}
	for k, id := range fx.ids {
		keyOf[id] = k
	}
	// key → dataset perceptor_verdict, verdict at the call
	keep := map[string][2]string{
		"alpha": {levelClean, levelClean}, "gamma": {levelInsufficient, levelUnknown}, "virt": {levelInsufficient, levelUnknown},
		"xss": {levelClean, levelClean}, "beta": {"<nil>", "<nil>"}, "sol": {"<nil>", "<nil>"}, "pct": {"<nil>", "<nil>"},
	}
	for _, r := range rows {
		key := keyOf[r.CallID]
		w, ok := keep[key]
		if !ok {
			continue
		}
		var ds *string
		if err := fx.st.Pool.QueryRow(ctx, `SELECT perceptor_verdict FROM scout_call_dataset_v WHERE call_id = $1`, r.CallID).Scan(&ds); err != nil {
			t.Fatal(err)
		}
		if strOrNil(ds) != w[0] || strOrNil(r.VerdictAtCall) != w[1] {
			t.Errorf("%s: dataset perceptor_verdict %s, verdict at call %s; want %s, %s (re-scans are never model inputs)", key, strOrNil(ds), strOrNil(r.VerdictAtCall), w[0], w[1])
		}
	}
}
