package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWebModelScores: the list shows the score stored for a token's first
// call at the time of the call; first-experiment rows (no bucket), repeat
// calls' scores and scores made long after the call are not shown.
func TestWebModelScores(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate, ModelScores: true})
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := func(callID int, version string, bucket *string, runner float64, passed *bool, at time.Time) {
		t.Helper()
		if _, err := fx.st.Pool.Exec(ctx, `INSERT INTO scout_call_predictions (uuid, call_id, contract_address, model_version,
			bucket, runner_prob, collapse_prob, runner_rank_pct, passed, rug_prob, features, created_by, created_at)
			SELECT $1::uuid, c.id, c.contract_address, $3, $4, $5::numeric, $5::numeric / 2, 90, $6, 0.5, '{"secret":1}'::jsonb, 't', $7
			FROM scout_calls c WHERE c.id = $2`, uuid.NewString(), callID, version, bucket, runner, passed, at); err != nil {
			t.Fatal(err)
		}
	}
	alphaAt := base.Add(time.Hour) // Msg 1
	for i, b := range ScoutWebModelBuckets {
		var passed *bool
		switch b {
		case "short", "3day":
			passed = bp(false)
		case "medium":
			passed = bp(true)
		}
		store(fx.ids["alpha"], "20261013-1909", sp(b), []float64{0.1, 0.2, 0.3, 0.4}[i], passed, alphaAt.Add(2*time.Minute))
	}
	// a repeat call of alpha, scored by another version at its own time: not alpha's score
	repeat := seedWebCall(t, fx.st, base, webSeed{Msg: 20, CA: caAlpha, Status: TrackPending})
	store(repeat, "20261020-0800", sp("medium"), 0.9, nil, base.Add(20*time.Hour+time.Minute))
	// beta: only a row of the first experiment (no bucket)
	store(fx.ids["beta"], "exp-1", nil, 0.7, nil, base.Add(2*time.Hour+time.Minute))
	// virt (not in USD): scored at the call; gamma: back-scored 30 days later
	store(fx.ids["virt"], "20261013-1909", sp("medium"), 0.3, nil, base.Add(4*time.Hour+time.Minute))
	store(fx.ids["gamma"], "20261013-1909", sp("medium"), 0.95, bp(true), base.Add(3*time.Hour+30*24*time.Hour))

	// the scores as read are already as the website sends them (time in UTC,
	// finite numbers), so newWebSnapshot never copies one (webShownModel)
	rows, _, err := fx.st.SelectWebRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Model != nil && !webModelClean(r.Model) {
			t.Errorf("call %d: score as read at %s (%s) %+v is not clean", r.CallID, r.Model.ScoredAt, r.Model.ScoredAt.Location(), r.Model.Buckets)
		}
	}

	// the scored feature row (features) never reaches the API
	if _, _, body := fx.get(t, "/api/calls?per=200"); strings.Contains(string(body), "secret") {
		t.Fatalf("the stored features leaked into /api/calls: %s", body)
	}
	res := fx.calls(t, "sort=model")
	if !res.Models || res.USDOnly {
		t.Fatalf("model_scores %t usd_only %t, want true false", res.Models, res.USDOnly)
	}
	byKey := map[string]webCallJSON{}
	for _, c := range res.Calls {
		for k, id := range fx.ids {
			if id == c.CallID {
				byKey[k] = c
			}
		}
	}
	a := byKey["alpha"].Model
	if a == nil || a.Version != "20261013-1909" || a.AfterS != 120 || a.ScoredAt != "2026-09-01T01:02:00Z" || len(a.Buckets) != 4 {
		t.Fatalf("alpha's score: %+v", a)
	}
	got := ""
	for _, b := range a.Buckets {
		passed := "null"
		if b.Passed != nil {
			passed = map[bool]string{true: "true", false: "false"}[*b.Passed]
		}
		got += b.Bucket + "/" + b.Horizon + " " + fnum(b.RunnerProb) + " " + fnum(b.CollapseProb) + " " + fnum(b.RunnerRankPct) + " " + passed + "; "
	}
	if want := "short/1d 0.1 0.05 90 false; 3day/3d 0.2 0.1 90 false; medium/7d 0.3 0.15 90 true; long/30d 0.4 0.2 90 null; "; got != want {
		t.Fatalf("alpha's buckets: %s, want %s", got, want)
	}
	for _, k := range []string{"beta", "gamma", "pct", "xss"} {
		if m := byKey[k].Model; m != nil {
			t.Errorf("%s: model %+v, want none", k, m)
		}
	}
	if v := byKey["virt"].Model; v == nil || len(v.Buckets) != 1 || v.Buckets[0].Bucket != "medium" {
		t.Errorf("virt (not in USD): model %+v, want its medium score", v)
	}
	// sort=model: by the medium runner probability (alpha 0.3 ties virt 0.3:
	// the higher call id first), the rest after them
	if keys := fx.keys(res.Calls); len(keys) != 9 || keys[0] != "virt" || keys[1] != "alpha" {
		t.Fatalf("sort=model: %v", keys)
	}
	fx.wantOrder(t, "sort=model&dir=asc&per=2", "alpha", "virt")

	// a website that does not show scores: no row has one, and sort=model is refused
	off := mustWebServer(t, fx.st, webConfig{GMGNTemplate: defaultGMGNTemplate}, fx.web.static)
	if rec := getWeb(off, "/api/calls?sort=model"); rec.Code != 400 {
		t.Fatalf("ModelScores off, sort=model: %d %s", rec.Code, rec.Body)
	}
	for _, c := range decodeCalls(t, getWeb(off, "/api/calls")).Calls {
		if c.Model != nil {
			t.Fatalf("ModelScores off: call %d has a model", c.CallID)
		}
	}
}
