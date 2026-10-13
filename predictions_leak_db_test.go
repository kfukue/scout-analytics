package main

import (
	"context"
	"strings"
	"testing"
)

// TestDatasetViewNeverReadsPredictions: the ML dataset (scout_call_dataset_v,
// -export-dataset, the scoring input) never reads the model's own scores.
func TestDatasetViewNeverReadsPredictions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var def string
	if err := st.Pool.QueryRow(ctx, `SELECT pg_get_viewdef('scout_call_dataset_v'::regclass, true)`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if def == "" || strings.Contains(strings.ToLower(def), "scout_call_predictions") {
		t.Fatalf("scout_call_dataset_v reads scout_call_predictions (or is empty): %.300s", def)
	}
	// nor does any view it is built on
	rows, err := st.Pool.Query(ctx, `WITH RECURSIVE deps(oid) AS (
			SELECT 'scout_call_dataset_v'::regclass::oid
			UNION
			SELECT DISTINCT d.refobjid FROM deps
			JOIN pg_rewrite r ON r.ev_class = deps.oid
			JOIN pg_depend d ON d.objid = r.oid AND d.classid = 'pg_rewrite'::regclass AND d.refobjid <> deps.oid
		)
		SELECT c.relname FROM deps JOIN pg_class c ON c.oid = deps.oid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		n++
		if name == "scout_call_predictions" || name == "scout_call_predictions_v" {
			t.Fatalf("scout_call_dataset_v depends on %s", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("found %d relations under scout_call_dataset_v; the dependency query is not working", n)
	}
}
