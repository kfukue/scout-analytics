package main

import (
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The model's scores on the website: which stored score a row shows
// (attachWebModel), how it is sent, sorted, hashed and announced.

func fp(v float64) *float64 { return &v }
func bp(v bool) *bool       { return &v }

// TestAttachWebModel: a row shows the score made for its own call at the time
// of the call (within webModelMaxDelay), of the earliest such score's model
// version.
func TestAttachWebModel(t *testing.T) {
	if ScoutWebModelBuckets[webModelColumn] != "medium" || ScoutWebModelHorizons[webModelColumn] != "7d" {
		t.Fatalf("the model column is %s (%s), want medium (7d)", ScoutWebModelBuckets[webModelColumn], ScoutWebModelHorizons[webModelColumn])
	}
	at := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
	rows := func() []ScoutWebRow {
		return []ScoutWebRow{{CallID: 10, MessageDate: at}, {CallID: 20, MessageDate: at}, {CallID: 30, MessageDate: at}}
	}
	pred := func(call int, version, bucket string, after time.Duration, runner float64, passed *bool) ScoutWebPrediction {
		return ScoutWebPrediction{CallID: call, Version: version, Bucket: bucket, Runner: fp(runner), Collapse: fp(runner / 2),
			Rank: fp(90), Passed: passed, CreatedAt: at.Add(after)}
	}
	type want struct {
		version string
		after   time.Duration
		// runner of each bucket in ScoutWebModelBuckets order ("-" = no bucket)
		runners string
	}
	runners := func(m *ScoutWebModel) string {
		var parts []string
		for _, b := range m.Buckets {
			if !b.Has {
				parts = append(parts, "-")
				continue
			}
			parts = append(parts, fnum(b.Runner))
		}
		return strings.Join(parts, " ")
	}
	nan := math.NaN()
	for _, tc := range []struct {
		name  string
		preds []ScoutWebPrediction
		want  map[int]*want // call id → score (missing = none)
	}{
		{"no scores", nil, nil},
		{"a scored first call", []ScoutWebPrediction{
			pred(20, "v1", "short", 2*time.Minute, 0.31, bp(false)),
			pred(20, "v1", "3day", 2*time.Minute, 0.2, bp(false)),
			pred(20, "v1", "medium", 2*time.Minute, 0.12, bp(true)),
			pred(20, "v1", "long", 2*time.Minute, 0.06, nil)},
			map[int]*want{20: {"v1", 2 * time.Minute, "0.31 0.2 0.12 0.06"}}},
		{"a repeat call's score (another call id) is ignored", []ScoutWebPrediction{
			pred(25, "v1", "medium", time.Minute, 0.5, nil)}, nil},
		{"two versions: the earliest within the limit, with its own buckets", []ScoutWebPrediction{
			pred(10, "v1", "short", time.Minute, 0.1, nil),
			pred(10, "v1", "medium", time.Minute, 0.2, nil),
			pred(10, "v2", "short", time.Hour, 0.7, nil),
			pred(10, "v2", "medium", time.Hour, 0.8, nil),
			pred(10, "v2", "long", time.Hour, 0.9, nil)},
			map[int]*want{10: {"v1", time.Minute, "0.1 - 0.2 -"}}},
		{"an earlier version back-scored, a later one in time", []ScoutWebPrediction{
			pred(10, "v2", "medium", 5*time.Minute, 0.8, nil),
			pred(10, "v1", "medium", 30*24*time.Hour, 0.2, nil)},
			map[int]*want{10: {"v2", 5 * time.Minute, "- - 0.8 -"}}},
		{"scored more than 24 hours after the call: none", []ScoutWebPrediction{
			pred(30, "v1", "medium", 24*time.Hour+time.Second, 0.4, nil)}, nil},
		{"exactly 24 hours after the call: shown", []ScoutWebPrediction{
			pred(30, "v1", "medium", 24*time.Hour, 0.4, nil)},
			map[int]*want{30: {"v1", 24 * time.Hour, "- - 0.4 -"}}},
		{"one bucket re-scored a month later drops out", []ScoutWebPrediction{
			pred(10, "v1", "short", time.Minute, 0.1, nil),
			pred(10, "v1", "medium", 30*24*time.Hour, 0.2, nil)},
			map[int]*want{10: {"v1", time.Minute, "0.1 - - -"}}},
		{"an unknown bucket is ignored", []ScoutWebPrediction{
			pred(10, "v1", "weekly", time.Minute, 0.3, nil),
			pred(10, "v1", "long", time.Minute, 0.4, nil)},
			map[int]*want{10: {"v1", time.Minute, "- - - 0.4"}}},
		{"only unknown buckets: none", []ScoutWebPrediction{
			pred(10, "v1", "weekly", time.Minute, 0.3, nil)}, nil},
		{"NaN dropped, the bucket kept", []ScoutWebPrediction{
			pred(10, "v1", "medium", time.Minute, nan, nil)},
			map[int]*want{10: {"v1", time.Minute, "- - null -"}}},
		{"several calls", []ScoutWebPrediction{
			pred(5, "v1", "medium", time.Minute, 0.9, nil),
			pred(10, "v1", "medium", time.Minute, 0.1, nil),
			pred(30, "v1", "medium", time.Minute, 0.3, nil),
			pred(40, "v1", "medium", time.Minute, 0.4, nil)},
			map[int]*want{10: {"v1", time.Minute, "- - 0.1 -"}, 30: {"v1", time.Minute, "- - 0.3 -"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := rows()
			attachWebModel(rs, tc.preds)
			for _, r := range rs {
				if r.Model != nil && !webModelClean(r.Model) {
					t.Fatalf("call %d: score %+v is not clean", r.CallID, r.Model)
				}
				w := tc.want[r.CallID]
				if w == nil {
					if r.Model != nil {
						t.Fatalf("call %d: got %s %s, want no score", r.CallID, r.Model.Version, runners(r.Model))
					}
					continue
				}
				if r.Model == nil {
					t.Fatalf("call %d: no score, want %s %s", r.CallID, w.version, w.runners)
				}
				if r.Model.Version != w.version || !r.Model.ScoredAt.Equal(at.Add(w.after)) || runners(r.Model) != w.runners {
					t.Fatalf("call %d: got %s at %s [%s], want %s at %s [%s]", r.CallID, r.Model.Version, r.Model.ScoredAt,
						runners(r.Model), w.version, at.Add(w.after), w.runners)
				}
			}
		})
	}
	t.Run("passed: not recorded, false, true", func(t *testing.T) {
		rs := rows()
		attachWebModel(rs, []ScoutWebPrediction{
			pred(10, "v1", "short", time.Minute, 0.1, nil),
			pred(10, "v1", "3day", time.Minute, 0.1, bp(false)),
			pred(10, "v1", "medium", time.Minute, 0.1, bp(true))})
		b := rs[0].Model.Buckets
		if b[0].Passed != nil || b[1].Passed == nil || *b[1].Passed || b[2].Passed == nil || !*b[2].Passed || b[3].Has {
			t.Fatalf("passed %v %v %v, long %v; want nil false true, no long", b[0].Passed, b[1].Passed, b[2].Passed, b[3].Has)
		}
	})
	t.Run("a row without a score loses an old one", func(t *testing.T) {
		rs := rows()
		rs[1].Model = &ScoutWebModel{Version: "old"}
		attachWebModel(rs, nil)
		if rs[1].Model != nil {
			t.Fatal("a stale score was kept")
		}
	})
}

// TestAttachWebModelClean: every score attachWebModel makes is already as the
// website sends it (webModelClean), also from stored times in another zone and
// numbers JSON cannot carry, so newWebSnapshot never copies one on the
// database path (webShownModel).
func TestAttachWebModelClean(t *testing.T) {
	at := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
	zone := time.FixedZone("UTC+9", 9*3600)
	nan, inf, ninf := math.NaN(), math.Inf(1), math.Inf(-1)
	rows := []ScoutWebRow{{CallID: 10, MessageDate: at}, {CallID: 20, MessageDate: at.In(zone)}, {CallID: 30, MessageDate: at}}
	// in call id order (as webPredictionsSQL reads them); each bucket of a
	// call gets another mix of missing, NaN, ±Inf and finite numbers
	var preds []ScoutWebPrediction
	nums := []*float64{nil, &nan, &inf, &ninf, fp(0.25)}
	for c, call := range []int{10, 20, 30} {
		for i, b := range ScoutWebModelBuckets {
			k := c + i
			preds = append(preds, ScoutWebPrediction{CallID: call, Version: "v1", Bucket: b,
				Runner: nums[k%len(nums)], Collapse: nums[(k+1)%len(nums)], Rank: nums[(k+2)%len(nums)],
				CreatedAt: at.Add(time.Duration(call) * time.Second).In(zone)})
		}
	}
	attachWebModel(rows, preds)
	for _, r := range rows {
		if r.Model == nil {
			t.Fatalf("call %d: no score, want one", r.CallID)
		}
		for b, v := range r.Model.Buckets {
			if !v.Has {
				t.Fatalf("call %d: no %s bucket, want all four", r.CallID, ScoutWebModelBuckets[b])
			}
		}
		if !webModelClean(r.Model) {
			t.Errorf("call %d: score at %s %+v is not clean", r.CallID, r.Model.ScoredAt, r.Model.Buckets)
		}
		if got := webShownModel(r.Model, true); got != r.Model {
			t.Errorf("call %d: webShownModel copied a clean score", r.CallID)
		}
	}
}

// TestWebShownModel: the score a prepared row shows: none when the website
// does not show scores; a clean score as it is; any other one as a copy in UTC
// without the numbers JSON cannot carry, the original left as it was.
func TestWebShownModel(t *testing.T) {
	at := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	clean := func() *ScoutWebModel {
		m := &ScoutWebModel{Version: "v1", ScoredAt: at}
		m.Buckets[webModelColumn] = ScoutWebModelBucket{Has: true, Runner: fp(0.3), Collapse: fp(0.4), Rank: fp(90), Passed: bp(true)}
		return m
	}
	nan, inf := math.NaN(), math.Inf(-1)
	for _, tc := range []struct {
		name string
		edit func(m *ScoutWebModel)
	}{
		{"another zone", func(m *ScoutWebModel) { m.ScoredAt = m.ScoredAt.In(time.FixedZone("x", -3600)) }},
		{"runner NaN", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Runner = &nan }},
		{"collapse -Inf", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Collapse = &inf }},
		{"rank NaN in another bucket", func(m *ScoutWebModel) { m.Buckets[0] = ScoutWebModelBucket{Has: true, Rank: &nan} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := clean()
			tc.edit(m)
			before := fmt.Sprintf("%s %+v", m.ScoredAt, m.Buckets)
			got := webShownModel(m, true)
			if got == m || got == nil || !webModelClean(got) {
				t.Fatalf("got %p (clean %t), want a clean copy of %p", got, got != nil && webModelClean(got), m)
			}
			if after := fmt.Sprintf("%s %+v", m.ScoredAt, m.Buckets); after != before {
				t.Fatalf("the original changed: %s, was %s", after, before)
			}
			if !got.ScoredAt.Equal(at) || got.Version != "v1" || got.Buckets[webModelColumn].Passed == nil {
				t.Fatalf("copy %s %s %+v, want v1 at %s with the rest kept", got.Version, got.ScoredAt, got.Buckets, at)
			}
		})
	}
	m := clean()
	if got := webShownModel(m, true); got != m {
		t.Errorf("a clean score: got %p, want the same %p", got, m)
	}
	if got := webShownModel(m, false); got != nil {
		t.Errorf("scores not shown: got %+v, want nil", got)
	}
	if got := webShownModel(nil, true); got != nil {
		t.Errorf("no score: got %+v, want nil", got)
	}
}

// modelRow is a row with a score of the medium bucket only (runner nil = the
// bucket without a runner probability).
func modelRow(id int, at time.Time, runner *float64) ScoutWebRow {
	r := ScoutWebRow{CallID: id, MessageID: id, MessageDate: at, ChannelUsername: "scoutrobinhood",
		ContractAddress: fmt.Sprintf("0x%040x", id), CallCount: 1, LastCallDate: at}
	r.Model = &ScoutWebModel{Version: "v1", ScoredAt: at.Add(90 * time.Second)}
	r.Model.Buckets[webModelColumn] = ScoutWebModelBucket{Has: true, Runner: runner, Collapse: fp(0.4), Rank: fp(92.5), Passed: bp(true)}
	return r
}

// TestWebModelJSON: the "model" object of a row: only the buckets the score
// has, in order, with their horizon, after_s and passed in three states; null
// without a score and when the website does not show scores.
func TestWebModelJSON(t *testing.T) {
	at := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	r := modelRow(10, at, fp(0.31))
	r.PriceUnit = sp("VIRT") // a score does not depend on the USD price
	r.Model.ScoredAt = at.Add(150 * time.Second).In(time.FixedZone("x", 3600))
	r.Model.Buckets[0] = ScoutWebModelBucket{Has: true, Runner: fp(0.5), Collapse: fp(math.Inf(1)), Rank: nil, Passed: bp(false)}
	r.Model.Buckets[3] = ScoutWebModelBucket{Has: true, Runner: fp(0.06), Passed: nil}
	plain := modelRow(12, at, nil)
	plain.Model = nil
	snap := mustWebSnapshot(t, []ScoutWebRow{r, plain}, 0, nil)
	c := sentCall(t, snap, 0, 1)
	m := c.Model
	if m == nil || m.Version != "v1" || m.ScoredAt != "2026-10-13T19:02:30Z" || m.AfterS != 150 || len(m.Buckets) != 3 {
		t.Fatalf("model %+v", m)
	}
	got := ""
	for _, b := range m.Buckets {
		passed := "null"
		if b.Passed != nil {
			passed = fmt.Sprint(*b.Passed)
		}
		got += fmt.Sprintf("%s/%s %s %s %s %s; ", b.Bucket, b.Horizon, fnum(b.RunnerProb), fnum(b.CollapseProb), fnum(b.RunnerRankPct), passed)
	}
	if want := "short/1d 0.5 null null false; medium/7d 0.31 0.4 92.5 true; long/30d 0.06 null null null; "; got != want {
		t.Fatalf("buckets %s, want %s", got, want)
	}
	if c := sentCall(t, snap, 1, 1); c.Model != nil {
		t.Fatalf("a row without a score: %+v", c.Model)
	}
	// a website that does not show scores sends none
	off := testWebConfig
	off.ModelScores = false
	s, err := newWebSnapshot([]ScoutWebRow{modelRow(10, at, fp(0.31))}, 0, nil, off)
	if err != nil {
		t.Fatal(err)
	}
	if c := sentCall(t, s, 0, 1); c.Model != nil || s.modelAt[0] != 0 || s.nonNull[webSortKeyModel] != 0 {
		t.Fatalf("ModelScores off: model %+v, modelAt %d, sorted %d", c.Model, s.modelAt[0], s.nonNull[webSortKeyModel])
	}
}

// TestWebSnapshotModelVersion: every field of a score the page shows changes
// the snapshot's version (and so the ETag of every answer).
func TestWebSnapshotModelVersion(t *testing.T) {
	at := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	base := func() []ScoutWebRow { return []ScoutWebRow{modelRow(10, at, fp(0.31)), modelRow(12, at, fp(0.2))} }
	a := mustWebSnapshot(t, base(), 0, nil)
	if b := mustWebSnapshot(t, base(), 0, a); b.version != a.version {
		t.Fatal("the same scores: another version")
	}
	seen := map[string]string{a.version: "unchanged"}
	for _, tc := range []struct {
		name string
		edit func(m *ScoutWebModel)
	}{
		{"version", func(m *ScoutWebModel) { m.Version = "v2" }},
		{"scored at (a re-score)", func(m *ScoutWebModel) { m.ScoredAt = m.ScoredAt.Add(time.Second) }},
		{"runner", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Runner = fp(0.32) }},
		{"runner missing", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Runner = nil }},
		{"collapse", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Collapse = fp(0.41) }},
		{"collapse missing", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Collapse = nil }},
		{"rank", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Rank = fp(92.6) }},
		{"rank missing", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Rank = nil }},
		{"passed false", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Passed = bp(false) }},
		{"passed not recorded", func(m *ScoutWebModel) { m.Buckets[webModelColumn].Passed = nil }},
		{"another bucket", func(m *ScoutWebModel) { m.Buckets[0] = ScoutWebModelBucket{Has: true} }},
		{"bucket gone", func(m *ScoutWebModel) {
			m.Buckets[webModelColumn] = ScoutWebModelBucket{}
			m.Buckets[1] = ScoutWebModelBucket{Has: true}
		}},
	} {
		rows := base()
		tc.edit(rows[0].Model)
		c := mustWebSnapshot(t, rows, 0, a)
		if prev, dup := seen[c.version]; dup {
			t.Errorf("%s: same version as %s", tc.name, prev)
		}
		seen[c.version] = tc.name
	}
	rows := base()
	rows[1].Model = nil
	if c := mustWebSnapshot(t, rows, 0, a); seen[c.version] != "" {
		t.Errorf("score removed: same version as %s", seen[c.version])
	}
	// … and the ETag of an answer follows
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, nil, testWebConfig, static)
	ws.snap.Store(a)
	tag := getWeb(ws, "/api/calls").Header().Get("ETag")
	rows = base()
	rows[0].Model.Buckets[webModelColumn].Passed = bp(false)
	ws.snap.Store(mustWebSnapshot(t, rows, 0, a))
	if got := getWeb(ws, "/api/calls").Header().Get("ETag"); got == tag || got == "" {
		t.Fatalf("ETag %q after a change of passed, before %q", got, tag)
	}
}

// TestWebCallsSortModel: sort=model orders by the medium bucket's runner
// probability (rows without one last, in both directions), takes every price
// unit by default and is refused by a website that does not show scores.
func TestWebCallsSortModel(t *testing.T) {
	for q, want := range map[string]bool{"sort=model": false, "sort=model&usd_only=1": true, "sort=model&usd_only=0": false, "sort=latest": true} {
		v, _ := url.ParseQuery(q)
		if f, err := parseWebCallsQuery(v); err != nil || f.USDOnly != want {
			t.Errorf("?%s: usd_only %v (%v), want %v", q, f.USDOnly, err, want)
		}
	}
	at := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	nan := math.NaN()
	rows := []ScoutWebRow{
		modelRow(10, at, fp(0.2)),
		modelRow(12, at, nil), // the bucket without a runner probability
		modelRow(14, at, fp(0.9)),
		modelRow(16, at, fp(0.2)), // a tie with 10
		modelRow(18, at, &nan),    // not finite: dropped
		modelRow(20, at, fp(0.05)),
		modelRow(22, at, fp(0.5)),
	}
	rows[3].PriceUnit = sp("VIRT")
	rows[5].Model = nil // unscored
	rows[6].Model.Buckets[webModelColumn] = ScoutWebModelBucket{}
	rows[6].Model.Buckets[0] = ScoutWebModelBucket{Has: true, Runner: fp(0.99)} // scored, but no medium bucket
	snap := mustWebSnapshot(t, rows, 0, nil)
	for dir, want := range map[string]string{
		"desc": "[14 16 10 22 20 18 12]",
		"asc":  "[10 16 14 12 18 20 22]",
	} {
		ids, total := pageIDs(t, snap, ScoutWebCallsFilter{Sort: "model", Dir: dir, Horizon: "1d", Page: 1, Per: 50})
		if fmt.Sprint(ids) != want || total != len(rows) {
			t.Errorf("sort=model dir=%s: %v (total %d), want %s", dir, ids, total, want)
		}
	}
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, nil, testWebConfig, static)
	ws.snap.Store(snap)
	res := decodeCalls(t, getWeb(ws, "/api/calls?sort=model"))
	if !res.Models || res.USDOnly || res.Total != len(rows) || res.Calls[0].CallID != 14 || res.Calls[0].Model == nil {
		t.Fatalf("sort=model: model_scores %t usd_only %t total %d first %+v", res.Models, res.USDOnly, res.Total, res.Calls[0])
	}
	off := testWebConfig
	off.ModelScores = false
	wsOff := mustWebServer(t, nil, off, static)
	wsOff.snap.Store(mustWebSnapshot(t, cloneWebRows(rows), 0, nil))
	if rec := getWeb(wsOff, "/api/calls?sort=model"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "model") {
		t.Fatalf("sort=model with ModelScores off: %d %s, want 400", rec.Code, rec.Body)
	}
	if res := decodeCalls(t, getWeb(wsOff, "/api/calls")); res.Models {
		t.Fatal("ModelScores off: model_scores true")
	}
}

// TestWebSnapshotEventsScore: a score that arrives, or a re-score (same row,
// another time), is a "score" event carrying the row; many at once are one
// "reload" that counts them.
func TestWebSnapshotEventsScore(t *testing.T) {
	const n = 60
	base := eventRows(n)
	for i := range base {
		base[i].Model = nil
	}
	reports := syntheticWebReports(base)
	at := func(r *ScoutWebRow, after time.Duration) *ScoutWebModel {
		m := &ScoutWebModel{Version: "v1", ScoredAt: r.MessageDate.Add(after)}
		m.Buckets[webModelColumn] = ScoutWebModelBucket{Has: true, Runner: fp(0.3), Passed: bp(true)}
		return m
	}
	scored := cloneWebRows(base)
	scored[3].Model = at(&scored[3], time.Minute)
	for _, tc := range []struct {
		name       string
		prev, next func() []ScoutWebRow
		want       []gotEvent
	}{
		{"a score arrives", func() []ScoutWebRow { return cloneWebRows(base) }, func() []ScoutWebRow { return cloneWebRows(scored) },
			[]gotEvent{{"score", base[3].CallID, ""}}},
		{"the same score", func() []ScoutWebRow { return cloneWebRows(scored) }, func() []ScoutWebRow { return cloneWebRows(scored) }, nil},
		{"a re-score (same row, later)", func() []ScoutWebRow { return cloneWebRows(scored) }, func() []ScoutWebRow {
			rows := cloneWebRows(scored)
			rows[3].Model = at(&rows[3], 2*time.Minute)
			return rows
		}, []gotEvent{{"score", base[3].CallID, ""}}},
		{"a new row with its score: one call", func() []ScoutWebRow { return cloneWebRows(base[:n-1]) }, func() []ScoutWebRow {
			rows := cloneWebRows(base)
			rows[n-1].Model = at(&rows[n-1], time.Minute)
			return rows
		}, []gotEvent{{"call", base[n-1].CallID, ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := snapWithReports(t, tc.prev(), reports)
			next := snapWithReports(t, tc.next(), reports)
			got := decodeEvents(t, webSnapshotEvents(prev, next))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("events %v, want %v", got, tc.want)
			}
			for _, ev := range webSnapshotEvents(prev, next) {
				if ev.kind == "score" && !strings.Contains(string(ev.data), `"model":{"version":"v1"`) {
					t.Fatalf("score event without the score: %s", ev.data)
				}
			}
		})
	}
	t.Run("more than the limit: one reload with the scores", func(t *testing.T) {
		rows := cloneWebRows(base)
		for k := 0; k <= webEventsMaxPerRefresh; k++ {
			rows[k].Model = at(&rows[k], time.Minute)
		}
		evs := webSnapshotEvents(snapWithReports(t, cloneWebRows(base), reports), snapWithReports(t, rows, reports))
		want := fmt.Sprintf(`{"calls":0,"reports":0,"scores":%d}`, webEventsMaxPerRefresh+1)
		if len(evs) != 1 || evs[0].kind != "reload" || string(evs[0].data) != want {
			t.Fatalf("got %v, want one reload with %s", evs, want)
		}
	})
}
