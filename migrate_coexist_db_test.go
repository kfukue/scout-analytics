package main

import (
	"context"
	"testing"
	"time"
)

// The scores go into the scout_call_predictions table the first model
// experiment already created (uuid, contract_address, created_by required; no
// bucket columns). The migration must extend it in place, keep its rows, and be
// repeatable.
func TestPredictionsUseTheExistingTable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil { // creates scout_calls etc.
		t.Fatal(err)
	}
	// put the table back to the shape the first experiment left in the database
	if _, err := st.Pool.Exec(ctx, `DROP VIEW scout_call_predictions_v; DROP TABLE scout_call_predictions;
		CREATE TABLE scout_call_predictions (
		  id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, call_id INTEGER REFERENCES scout_calls (id) ON DELETE CASCADE,
		  contract_address TEXT NOT NULL, model_version TEXT NOT NULL, rug_prob DOUBLE PRECISION, ret_1d_pred DOUBLE PRECISION,
		  pos_7d_prob DOUBLE PRECISION, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO scout_call_predictions (uuid, contract_address, model_version, rug_prob, created_by)
		VALUES ('00000000-0000-0000-0000-000000000001', '0xabc', 'v1-old', 0.2, 'other'),
		       ('00000000-0000-0000-0000-000000000002', '0xabc', 'v1-old', 0.3, 'other')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: priceConfig{Enabled: false}}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	ca := "0x4444444444444444444444444444444444444444"
	s.onChannelPost(postAt(1, time.Now().Add(-time.Minute), ca))
	id := *(<-s.queue).CallID
	p := 0.31
	for i := 0; i < 2; i++ { // the second write updates, it does not add a row
		if err := st.UpsertPrediction(ctx, ScoutCallPrediction{CallID: id, ModelVersion: "20261002-1530", Bucket: "short", RunnerProb: &p}, []byte(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := st.PredictionsForCall(ctx, id)
	if err != nil || len(ps) != 1 || *ps[0].RunnerProb != 0.31 {
		t.Fatalf("%+v %v", ps, err)
	}
	var total, old, viewRows int
	var gotCA, by string
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM scout_call_predictions),
		(SELECT count(*) FROM scout_call_predictions WHERE bucket IS NULL AND rug_prob IS NOT NULL),
		(SELECT count(*) FROM scout_call_predictions_v),
		(SELECT contract_address FROM scout_call_predictions WHERE bucket = 'short'),
		(SELECT created_by FROM scout_call_predictions WHERE bucket = 'short')`).Scan(&total, &old, &viewRows, &gotCA, &by); err != nil {
		t.Fatal(err)
	}
	if total != 3 || old != 2 || viewRows != 1 || gotCA != ca || by != "scoutanalytics" {
		t.Fatalf("total %d, old rows %d, view %d, ca %s, by %s", total, old, viewRows, gotCA, by)
	}
}
