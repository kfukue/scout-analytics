package main

import (
	"fmt"
	"math"
	"math/rand"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The Perceptor filter of the list (verdict= of /api/calls) and its counts use
// the token's latest Perceptor report with a verdict (webLatestPerceptor): the
// call-time report (live scan or 12-minute re-check) or "Perceptor today" (a
// re-scan or the 2-hour re-check), whichever is newer, a real verdict always
// before "not enough data yet" or an unreadable one.

// refFilterVerdict is the Perceptor filter bucket of a row as read from the
// database, worked out the obvious way: rank the reports the row has (a real
// verdict first, then the newest completion time, a missing time oldest, then
// the re-scan) and take the first one's verdict if it is a real one.
func refFilterVerdict(r *ScoutWebRow) string {
	type report struct {
		verdict string
		real    bool
		at      int64
		rescan  bool
	}
	var reps []report
	add := func(v *string, at *time.Time, rescan bool) {
		if v == nil {
			return
		}
		rp := report{verdict: *v, rescan: rescan, at: math.MinInt64}
		rp.real = slices.Contains([]string{"clean", "caution", "red_flags"}, *v)
		if at != nil {
			rp.at = at.UnixNano()
		}
		reps = append(reps, rp)
	}
	add(r.PerceptorVerd, r.PerceptorAt, false)
	add(r.PerceptorTodayVerd, r.PerceptorTodayAt, true)
	sort.SliceStable(reps, func(i, j int) bool {
		a, b := reps[i], reps[j]
		if a.real != b.real {
			return a.real
		}
		if a.at != b.at {
			return a.at > b.at
		}
		return a.rescan && !b.rescan
	})
	if len(reps) == 0 || !reps[0].real {
		return "not_scanned"
	}
	return reps[0].verdict
}

// withSyntheticPerceptorToday gives synthetic rows (syntheticWebRows) the
// time of their call-time report and, on about a third of them, a "Perceptor
// today" of any verdict, often at the same time as the call-time one (ties),
// now and then without a time; a few call-time verdicts become insufficient.
// It has its own random source, so the other fields stay as they were.
func withSyntheticPerceptorToday(rows []ScoutWebRow, seed int64) []ScoutWebRow {
	rng := rand.New(rand.NewSource(seed + 5000))
	verdicts := []string{levelClean, levelCaution, levelRedFlags, levelInsufficient, levelUnknown}
	for i := range rows {
		r := &rows[i]
		if r.PerceptorVerd != nil {
			if rng.Intn(6) == 0 {
				r.PerceptorVerd = sp(levelInsufficient)
			}
			if rng.Intn(5) != 0 {
				at := r.MessageDate.Add(time.Duration(rng.Intn(3)) * time.Hour)
				r.PerceptorAt = &at
			}
		}
		if rng.Intn(3) == 0 {
			at := r.MessageDate.Add(time.Duration(rng.Intn(3)) * time.Hour)
			r.PerceptorTodayVerd, r.PerceptorTodayURL, r.PerceptorTodayAt = sp(verdicts[rng.Intn(len(verdicts))]), sp("https://www.perceptor.info/r/today"), &at
			if rng.Intn(10) == 0 {
				r.PerceptorTodayAt = nil
			}
		}
	}
	return rows
}

func TestWebLatestPerceptor(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := t0.Add(time.Duration(h) * time.Hour); return &v }
	for _, tc := range []struct {
		name          string
		live          *string
		liveAt        *time.Time
		today         *string
		todayAt       *time.Time
		want, wantBkt string
	}{
		{"no report", nil, nil, nil, nil, "<nil>", "not_scanned"},
		{"call-time clean only", sp("clean"), at(0), nil, nil, "clean", "clean"},
		{"re-scan caution only (no report at the call)", nil, nil, sp("caution"), at(48), "caution", "caution"},
		{"clean, then a re-scan red_flags", sp("clean"), at(0), sp("red_flags"), at(48), "red_flags", "red_flags"},
		{"re-scan red_flags, then a newer call-time clean", sp("clean"), at(72), sp("red_flags"), at(48), "clean", "clean"},
		{"insufficient, then the 2-hour re-check clean", sp("insufficient"), at(0), sp("clean"), at(2), "clean", "clean"},
		{"clean, then a re-scan insufficient", sp("clean"), at(0), sp("insufficient"), at(48), "clean", "clean"},
		{"red_flags, then a re-scan unknown", sp("red_flags"), at(0), sp("unknown"), at(48), "red_flags", "red_flags"},
		{"a re-scan caution, then a call-time insufficient", sp("insufficient"), at(72), sp("caution"), at(48), "caution", "caution"},
		{"insufficient, then a re-scan unknown", sp("insufficient"), at(0), sp("unknown"), at(48), "unknown", "not_scanned"},
		{"unknown, then a newer call-time insufficient", sp("insufficient"), at(72), sp("unknown"), at(48), "insufficient", "not_scanned"},
		{"same time: the re-scan", sp("clean"), at(5), sp("caution"), at(5), "caution", "caution"},
		{"call-time time missing: the re-scan", sp("clean"), nil, sp("red_flags"), at(5), "red_flags", "red_flags"},
		{"re-scan time missing: the call-time one", sp("clean"), at(5), sp("red_flags"), nil, "clean", "clean"},
		{"both times missing: the re-scan", sp("clean"), nil, sp("caution"), nil, "caution", "caution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := webLatestPerceptor(tc.live, tc.liveAt, tc.today, tc.todayAt)
			if strOrNil(got) != tc.want {
				t.Errorf("webLatestPerceptor(%s at %v, %s at %v) = %s, want %s", strOrNil(tc.live), tc.liveAt, strOrNil(tc.today), tc.todayAt, strOrNil(got), tc.want)
			}
			if b := webVerdictNames[webBucketOf(got)]; b != tc.wantBkt {
				t.Errorf("bucket of %s = %s, want %s", strOrNil(got), b, tc.wantBkt)
			}
			r := ScoutWebRow{PerceptorVerd: tc.live, PerceptorAt: tc.liveAt, PerceptorTodayVerd: tc.today, PerceptorTodayAt: tc.todayAt}
			if ref := refFilterVerdict(&r); ref != tc.wantBkt {
				t.Errorf("the reference says %s, want %s (the test's reference and rule disagree)", ref, tc.wantBkt)
			}
		})
	}
}

// latestFilterRows: the owner's cases, each a row of a small list (built
// fresh each time: snapshots change their rows), by name. A failed re-scan is
// never read from the database (completed reports only), so "clean, then a
// failed re-scan" is a row with its call-time clean alone.
func latestFilterRows() ([]ScoutWebRow, map[string]int) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := t0.Add(time.Duration(h) * time.Hour); return &v }
	rows := syntheticWebRows(12, 3) // row 0 has a Solana-style address: the cases start at row 1, so "0x" finds them all
	for i := range rows {
		rows[i].PerceptorVerd, rows[i].PerceptorURL, rows[i].PerceptorID, rows[i].PerceptorAt = nil, nil, nil, nil
		rows[i].PerceptorTodayVerd, rows[i].PerceptorTodayURL, rows[i].PerceptorTodayAt = nil, nil, nil
	}
	ids := map[string]int{}
	set := func(i int, name string, live *string, liveAt *time.Time, today *string, todayAt *time.Time) {
		r := &rows[i]
		ids[name] = r.CallID
		if live != nil {
			id := 7000000 + i
			r.PerceptorVerd, r.PerceptorURL, r.PerceptorID, r.PerceptorAt = live, sp("https://www.perceptor.info/r/live"), &id, liveAt
		}
		if today != nil {
			r.PerceptorTodayVerd, r.PerceptorTodayURL, r.PerceptorTodayAt = today, sp("https://www.perceptor.info/r/today"), todayAt
		}
	}
	set(1, "clean, then a re-scan red_flags", sp("clean"), at(0), sp("red_flags"), at(48))
	set(2, "no report, then a re-scan caution", nil, nil, sp("caution"), at(48))
	set(3, "insufficient, then the 2-hour re-check clean", sp("insufficient"), at(0), sp("clean"), at(2))
	set(4, "clean, then a failed re-scan", sp("clean"), at(0), nil, nil)
	set(5, "clean, then a re-scan insufficient", sp("clean"), at(0), sp("insufficient"), at(48))
	set(6, "no report", nil, nil, nil, nil)
	return rows, ids
}

func TestWebPerceptorFilterLatest(t *testing.T) {
	rows, ids := latestFilterRows()
	raw := cloneWebRows(rows)
	snap := mustWebSnapshot(t, rows, 0, nil)
	byID := map[int]int{} // call id → position
	for i := 0; i < snap.n; i++ {
		byID[int(snap.ids[i])] = i
	}
	type want struct{ shown, today, latest string }
	wants := map[string]want{
		"clean, then a re-scan red_flags":              {"clean", "red_flags", "red_flags"},
		"no report, then a re-scan caution":            {"<nil>", "caution", "caution"},
		"insufficient, then the 2-hour re-check clean": {"insufficient", "clean", "clean"},
		"clean, then a failed re-scan":                 {"clean", "<nil>", "clean"},
		"clean, then a re-scan insufficient":           {"clean", "insufficient", "clean"},
		"no report":                                    {"<nil>", "<nil>", "<nil>"},
	}
	// the row as sent: the call-time verdict and "Perceptor today" as before,
	// and the filter's verdict next to them
	for name, w := range wants {
		c := sentCall(t, snap, byID[ids[name]], 1)
		if got := (want{strOrNil(c.Perceptor), strOrNil(c.PerceptorTodayVerd), strOrNil(c.PerceptorLatest)}); got != w {
			t.Errorf("%s: perceptor_verdict, perceptor_today_verdict, perceptor_latest_verdict = %v, want %v", name, got, w)
		}
	}
	// which rows each filter lets through; with a search text too, which
	// counts the rows one by one instead of using the snapshot's counts
	in := func(verdict, q string) map[string]bool {
		got, total := pageIDs(t, snap, ScoutWebCallsFilter{Q: q, Sort: "date", Dir: "desc", Horizon: "1d", Verdict: verdict, Page: 1, Per: 100})
		if total != len(got) {
			t.Fatalf("verdict=%s q=%q: total %d, %d rows", verdict, q, total, len(got))
		}
		out := map[string]bool{}
		for name, id := range ids {
			out[name] = slices.Contains(got, id)
		}
		return out
	}
	for _, tc := range []struct {
		verdict string
		name    string
		want    bool
	}{
		{"clean", "clean, then a re-scan red_flags", false},
		{"red_flags", "clean, then a re-scan red_flags", true},
		{"caution", "no report, then a re-scan caution", true},
		{"not_scanned", "no report, then a re-scan caution", false},
		{"clean", "insufficient, then the 2-hour re-check clean", true},
		{"not_scanned", "insufficient, then the 2-hour re-check clean", false},
		{"clean", "clean, then a failed re-scan", true},
		{"clean", "clean, then a re-scan insufficient", true},
		{"not_scanned", "clean, then a re-scan insufficient", false},
		{"not_scanned", "no report", true},
	} {
		for _, q := range []string{"", "0x"} {
			if got := in(tc.verdict, q)[tc.name]; got != tc.want {
				t.Errorf("verdict=%s q=%q: %q listed %t, want %t", tc.verdict, q, tc.name, got, tc.want)
			}
		}
	}
	// the counts (the total of a filter without a search) follow the same verdict
	for b, name := range webVerdictNames {
		n := 0
		for i := range raw {
			if refFilterVerdict(&raw[i]) == name {
				n++
			}
		}
		if snap.counts[0][b] != n {
			t.Errorf("count of %s: %d, want %d", name, snap.counts[0][b], n)
		}
	}
}

// TestWebSnapshotPageMatchesReferenceToday: the Perceptor filter on rows with
// "Perceptor today" (of every verdict, at every order of the two times) lets
// through the rows the reference rule says, with the right total, combined
// with the other filters.
func TestWebSnapshotPageMatchesReferenceToday(t *testing.T) {
	for _, n := range []int{1, 7, 600} {
		raw := withSyntheticPerceptorToday(syntheticWebRows(n, int64(n)+11), int64(n))
		snap := mustWebSnapshot(t, cloneWebRows(raw), 0, nil)
		moved := 0
		for i := range raw {
			call := "not_scanned"
			if v := raw[i].PerceptorVerd; webRealVerdict(v) {
				call = *v
			}
			if refFilterVerdict(&raw[i]) != call {
				moved++
			}
		}
		if n == 600 && moved < 30 {
			t.Fatalf("only %d rows where the latest verdict differs from the call-time one: too few to test", moved)
		}
		for _, verdict := range []string{"", "clean", "caution", "red_flags", "not_scanned", "clean,caution", "red_flags,not_scanned"} {
			for _, q := range []string{"", "pe", "0x"} {
				for _, usdOnly := range []bool{false, true} {
					for _, pg := range [][2]int{{1, 50}, {2, 50}, {1, 1000}} {
						f := ScoutWebCallsFilter{Q: q, Sort: "return", Dir: "desc", Horizon: "1d", USDOnly: usdOnly, Verdict: verdict, Page: pg[0], Per: pg[1]}
						got, total := pageIDs(t, snap, f)
						want, wantTotal := referencePage(raw, f)
						if total != wantTotal || fmt.Sprint(got) != fmt.Sprint(want) {
							t.Fatalf("n=%d %+v:\n got %d %v\nwant %d %v", n, f, total, got, wantTotal, want)
						}
					}
				}
			}
		}
		// every row says which verdict the filter used
		for i := range raw {
			c := sentCall(t, snap, i, 1)
			got := "not_scanned"
			if webRealVerdict(c.PerceptorLatest) {
				got = *c.PerceptorLatest
			}
			if want := refFilterVerdict(&raw[i]); got != want {
				t.Fatalf("n=%d row %d: perceptor_latest_verdict %v (bucket %s), want bucket %s", n, i, c.PerceptorLatest, got, want)
			}
		}
	}
}

// TestWebPerceptorFilterRescanArrives: a re-scan arriving moves its row from
// one filtered list to another: the snapshot version (and so the ETag of
// /api/calls) changes, the old ETag gets the new answer, and the live update
// is one quiet perceptor_today event whose row carries the new filter
// verdict. A failed re-scan changes nothing (it is never read).
func TestWebPerceptorFilterRescanArrives(t *testing.T) {
	ws := benchWebServer(t, 10)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	build := func(rescan bool) (*webSnapshot, int) {
		rows := syntheticWebRows(30, 5)
		for i := range rows {
			rows[i].PerceptorTodayVerd, rows[i].PerceptorTodayURL, rows[i].PerceptorTodayAt = nil, nil, nil
		}
		at := t0
		id := 7100000
		r := &rows[4]
		r.PerceptorVerd, r.PerceptorURL, r.PerceptorID, r.PerceptorAt = sp(levelClean), sp("https://www.perceptor.info/r/live"), &id, &at
		if rescan {
			later := t0.Add(72 * time.Hour)
			r.PerceptorTodayVerd, r.PerceptorTodayURL, r.PerceptorTodayAt = sp(levelRedFlags), sp("https://www.perceptor.info/r/today"), &later
		}
		s := mustWebSnapshot(t, rows, 0, nil)
		s.loadedAt = t0.Add(100 * time.Hour)
		return s, r.CallID
	}
	before, call := build(false)
	failed, _ := build(false) // a failed re-scan: the same rows
	after, _ := build(true)

	type answer struct {
		code int
		etag string
		ids  []int
	}
	ask := func(verdict, ifNoneMatch string) answer {
		t.Helper()
		rec := getWeb(ws, "/api/calls?per=200&verdict="+verdict, "If-None-Match", ifNoneMatch)
		a := answer{code: rec.Code, etag: rec.Header().Get("ETag")}
		if rec.Code == 200 {
			for _, c := range decodeCalls(t, rec).Calls {
				a.ids = append(a.ids, c.CallID)
			}
		}
		return a
	}
	ws.snap.Store(before)
	clean0, red0 := ask("clean", ""), ask("red_flags", "")
	if !slices.Contains(clean0.ids, call) || slices.Contains(red0.ids, call) {
		t.Fatalf("before the re-scan: call %d in clean %t, in red_flags %t; want clean only", call, slices.Contains(clean0.ids, call), slices.Contains(red0.ids, call))
	}

	if evs := webSnapshotEvents(before, failed); failed.version != before.version || evs != nil {
		t.Errorf("a failed re-scan: version %s → %s, %d events; want the same version and none", before.version, failed.version, len(evs))
	}

	ws.snap.Store(after)
	if after.version == before.version {
		t.Fatal("a re-scan did not change the snapshot version")
	}
	clean1 := ask("clean", clean0.etag)
	red1 := ask("red_flags", red0.etag)
	if clean1.code != 200 || clean1.etag == clean0.etag || slices.Contains(clean1.ids, call) {
		t.Errorf("verdict=clean after the re-scan with the old ETag: %d, ETag %s (was %s), call listed %t; want 200, a new ETag, not listed",
			clean1.code, clean1.etag, clean0.etag, slices.Contains(clean1.ids, call))
	}
	if red1.code != 200 || red1.etag == red0.etag || !slices.Contains(red1.ids, call) {
		t.Errorf("verdict=red_flags after the re-scan with the old ETag: %d, ETag %s (was %s), call listed %t; want 200, a new ETag, listed",
			red1.code, red1.etag, red0.etag, slices.Contains(red1.ids, call))
	}
	if a := ask("red_flags", red1.etag); a.code != 304 {
		t.Errorf("verdict=red_flags again with its new ETag: %d, want 304", a.code)
	}

	evs := webSnapshotEvents(before, after)
	got := decodeEvents(t, evs)
	if want := []gotEvent{{"report", call, webEventPerceptorToday}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v (one quiet re-scan event, no call-time notice)", got, want)
	}
	row := string(evs[0].data)
	for _, part := range []string{`"perceptor_verdict":"clean"`, `"perceptor_today_verdict":"red_flags"`, `"perceptor_latest_verdict":"red_flags"`} {
		if !strings.Contains(row, part) {
			t.Errorf("event %s: no %s", row, part)
		}
	}
}

// TestWebPerceptorFilterEventWithoutRescan: the filter's bucket can move
// without a new re-scan or call-time report (here a call-time report of the
// same id and verdict that completed later than the re-scan): the row is
// still sent, quietly, so a filtered page can drop or take it.
func TestWebPerceptorFilterEventWithoutRescan(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	build := func(liveAt time.Time) *webSnapshot {
		rows := syntheticWebRows(30, 5)
		id := 7100000
		r := &rows[4]
		later := t0.Add(48 * time.Hour)
		r.PerceptorVerd, r.PerceptorURL, r.PerceptorID, r.PerceptorAt = sp(levelClean), sp("https://www.perceptor.info/r/live"), &id, &liveAt
		r.PerceptorTodayVerd, r.PerceptorTodayURL, r.PerceptorTodayAt = sp(levelRedFlags), sp("https://www.perceptor.info/r/today"), &later
		return mustWebSnapshot(t, rows, 0, nil)
	}
	prev, next := build(t0), build(t0.Add(72*time.Hour))
	if prev.flags[4]&^webFlagUSD != webBucketRedFlags || next.flags[4]&^webFlagUSD != webBucketClean {
		t.Fatalf("buckets %d → %d, want red_flags (%d) → clean (%d)", prev.flags[4]&^webFlagUSD, next.flags[4]&^webFlagUSD, webBucketRedFlags, webBucketClean)
	}
	got := decodeEvents(t, webSnapshotEvents(prev, next))
	if want := []gotEvent{{"report", int(next.ids[4]), webEventPerceptorToday}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// TestAppFilterNode runs testdata/app-filter.test.js: the page applies the
// Perceptor filter to the rows of live events with the same verdict as the
// server (perceptor_latest_verdict).
func TestAppFilterNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, "testdata/app-filter.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/app-filter.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/app-filter.test.js: got %q, want \"ok: …\"", out)
	}
}
