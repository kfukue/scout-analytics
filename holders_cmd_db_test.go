package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// cmdDBConfig: cmdConfig with the throwaway test database.
func cmdDBConfig(t *testing.T) *config {
	t.Helper()
	cfg := cmdConfig(t)
	cfg.DatabaseURL = testDatabaseURL(t)
	return cfg
}

// The command end to end on a throwaway database and the fake chain, through
// runHolderExportCommand (its own database connection): rows, resume, -max,
// CSV, columns.json and the run record.
func TestHolderExportCommandDB(t *testing.T) {
	st := hdbMigrated(t)
	c := newCmdChain(t)
	b, err := json.Marshal(cmdState(c))
	if err != nil {
		t.Fatal(err)
	}
	state := string(b)
	var readable []int64
	for msg := 1; msg <= 3; msg++ {
		readable = append(readable, int64(hdbSeedCall(t, st, hdbSeed{Msg: msg, CA: hfToken, Entry: c.entryAt, Tracking: true, State: &state, AgeS: ip(3600)})))
	}
	skipped := int64(hdbSeedCall(t, st, hdbSeed{Msg: 4, CA: hfToken, Entry: c.entryAt}))
	cfg := cmdDBConfig(t)
	checkExportResume(t, c, func(t *testing.T, opts holderExportOptions) string {
		var logw bytes.Buffer
		if err := runHolderExportCommand(context.Background(), cfg, opts, &logw); err != nil {
			t.Fatalf("runHolderExportCommand: %v; log %s", err, logw.String())
		}
		return logw.String()
	}, readable, skipped, skipped+1000)
}

// The command never applies the schema: on a database without the scout
// tables it fails at its first read and leaves the database as it was.
func TestHolderExportCommandNeverMigratesDB(t *testing.T) {
	st := testStore(t) // drops the scout_* tables
	c := newCmdChain(t)
	cfg := cmdDBConfig(t)
	opts := holderExportOptions{IDsFile: writeIDs(t, "1\n"), OutDir: filepath.Join(t.TempDir(), "out")}
	if err := runHolderExportCommand(context.Background(), cfg, opts, &bytes.Buffer{}); err == nil {
		t.Fatal("export on a database without the schema: no error")
	}
	var calls *string
	if err := st.Pool.QueryRow(context.Background(), `SELECT to_regclass('scout_calls')::text`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != nil {
		t.Errorf("scout_calls exists after the export (%s): the command applied the schema", *calls)
	}
	if n := c.f.counts(); len(n) != 0 {
		t.Errorf("node asked: %v", n)
	}
}
