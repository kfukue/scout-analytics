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

// The database may hold older or foreign versions of the views with other
// columns (CREATE OR REPLACE VIEW would fail with "cannot drop columns from
// view"). Starting up must rebuild them.
func TestMigrateRebuildsViewsWithDifferentColumns(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DROP VIEW scout_call_predictions_v; DROP VIEW scout_call_dataset_v;
		DROP VIEW scout_calls_v; DROP VIEW scout_investigations_v;
		CREATE VIEW scout_calls_v AS SELECT c.id AS call_id, c.message_id, 1 AS extra_a, 2 AS extra_b, c.contract_address,
			c.message_date, c.status, c.channel_username, c.chain, 3 AS extra_c, 4 AS extra_d, 5 AS extra_e, 6 AS extra_f,
			7 AS g, 8 AS h, 9 AS i, 10 AS j, 11 AS k, 12 AS l, 13 AS m, 14 AS n, 15 AS o, 16 AS p, 17 AS q, 18 AS r,
			19 AS s, 20 AS u, 21 AS v, 22 AS w, 23 AS x, 24 AS y, 25 AS z FROM scout_calls c;
		CREATE VIEW scout_investigations_v AS SELECT i.id, i.call_id, 1 AS one, 2 AS two, 3 AS three, 4 AS four, 5 AS five,
			6 AS six, 7 AS seven, 8 AS eight, 9 AS nine, 10 AS ten, 11 AS eleven, 12 AS twelve, 13 AS thirteen, 14 AS fourteen,
			15 AS fifteen, 16 AS sixteen, 17 AS seventeen, 18 AS eighteen, 19 AS nineteen, 20 AS twenty, 21 AS a21, 22 AS a22,
			23 AS a23, 24 AS a24, 25 AS a25, 26 AS a26, 27 AS a27, 28 AS a28, 29 AS a29, 30 AS a30 FROM scout_investigations i;
		CREATE VIEW scout_call_dataset_v AS SELECT call_id, extra_a FROM scout_calls_v`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'scout_calls_v' AND column_name IN ('extra_a', 'post_kind')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("scout_calls_v not rebuilt: %d %v", n, err)
	}
	if _, err := st.Pool.Exec(ctx, `SELECT * FROM scout_call_dataset_v LIMIT 1; SELECT * FROM scout_call_predictions_v LIMIT 1;
		SELECT * FROM scout_investigations_v LIMIT 1`); err != nil {
		t.Fatal(err)
	}
}
