package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// badDatabaseURLs are unparseable SCOUT_DATABASE_URL values with fake,
// distinctive credentials (user scoutuser7, password s3cretpw).
var badDatabaseURLs = []string{
	"postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?sslmode=bogus",
	"postgres://scoutuser7:s3cretpw@db example:5432/scoutdb",
	"postgres://scoutuser7:s3cret%zzpw@db.example:5432/scoutdb",
	"postgres://scoutuser7:s3cret:p@w@db example/scoutdb",
	"host=db.example user=scoutuser7 password='s3cret pw dbname=scoutdb",
}

// checkDatabaseURLError fails unless err names SCOUT_DATABASE_URL and holds
// neither dsn, its user, nor any part of its password.
func checkDatabaseURLError(t *testing.T, call, dsn string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s(%q): nil error, want one", call, dsn)
	}
	got := err.Error()
	if !strings.HasPrefix(got, "SCOUT_DATABASE_URL could not be parsed") {
		t.Errorf("%s(%q) = %q, want it to start with %q", call, dsn, got, "SCOUT_DATABASE_URL could not be parsed")
	}
	for _, secret := range []string{dsn, "scoutuser7", "s3cret"} {
		if strings.Contains(got, secret) {
			t.Errorf("%s(%q) = %q, contains %q", call, dsn, got, secret)
		}
	}
}

// NewScoutStore returns the parse error before it connects, so no database
// is needed.
func TestNewScoutStoreParseErrorHidesURL(t *testing.T) {
	clearPGEnv(t)
	for _, dsn := range badDatabaseURLs {
		st, err := NewScoutStore(context.Background(), dsn, 4)
		if st != nil {
			st.Close()
			t.Fatalf("NewScoutStore(%q) opened a store, want a parse error", dsn)
		}
		checkDatabaseURLError(t, "NewScoutStore", dsn, err)
	}
}

func TestApplyPoolMaxConnsParseErrorHidesURL(t *testing.T) {
	clearPGEnv(t)
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:55555/scout_test?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	for _, dsn := range badDatabaseURLs {
		_, err := applyPoolMaxConns(dsn, cfg, 4)
		checkDatabaseURLError(t, "applyPoolMaxConns", dsn, err)
	}
}
