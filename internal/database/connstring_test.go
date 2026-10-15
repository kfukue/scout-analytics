package database

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Fake credentials: distinctive, so a leak of any part of them is caught.
const (
	testUser     = "scoutuser7"
	testPassword = "s3cretpw"
)

// clearPGEnv empties the libpq variables pgconn.ParseConfig reads, so a
// developer's shell cannot change how a test string parses.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGCONNECT_TIMEOUT", "PGTARGETSESSIONATTRS", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT"} {
		t.Setenv(k, "")
	}
}

// assertNoSecret fails when text contains the connection string, the user,
// the password or a part of the password.
func assertNoSecret(t *testing.T, text, connString string) {
	t.Helper()
	for _, secret := range []string{connString, testUser, testPassword, "s3cret"} {
		if strings.Contains(text, secret) {
			t.Errorf("error %q contains %q (connection string %q)", text, secret, connString)
		}
	}
}

func TestConnStringErrorHidesConnString(t *testing.T) {
	clearPGEnv(t)
	tests := []struct {
		name, connString, want string
	}{
		{
			name:       "URL with a bogus sslmode",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?sslmode=bogus",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to configure TLS: sslmode is invalid",
		},
		{
			name:       "URL that url.Parse refuses (space in the host)",
			connString: "postgres://scoutuser7:s3cretpw@db example:5432/scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to parse as URL",
		},
		{
			name:       "URL with a bad escape in the password",
			connString: "postgres://scoutuser7:s3cret%zzpw@db.example:5432/scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to parse as URL",
		},
		{
			name:       "broken URL whose password holds a colon and an at sign",
			connString: "postgres://scoutuser7:s3cret:p@w@db example/scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to parse as URL",
		},
		{
			name:       "URL with a port too large for 16 bits",
			connString: "postgres://scoutuser7:s3cretpw@db.example:99999/scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid port: number out of range",
		},
		{
			name:       "URL with port 0",
			connString: "postgres://scoutuser7:s3cretpw@db.example:0/scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid port: outside range",
		},
		{
			name:       "URL with a connect_timeout that is not a number",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?connect_timeout=soon",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid connect_timeout: not a number",
		},
		{
			name:       "URL with an unknown target_session_attrs value",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?target_session_attrs=s3cretpw",
			want:       "SCOUT_DATABASE_URL could not be parsed: unknown target_session_attrs value",
		},
		{
			name:       "URL with a pool_max_conns that is not a number",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?pool_max_conns=many",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid pool_max_conns",
		},
		{
			name:       "URL with pool_max_conns too small",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?pool_max_conns=0",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid pool_max_conns",
		},
		{
			name:       "URL with a bad pool_max_conn_lifetime_jitter",
			connString: "postgres://scoutuser7:s3cretpw@db.example:5432/scoutdb?pool_max_conn_lifetime_jitter=s3cretpw",
			want:       "SCOUT_DATABASE_URL could not be parsed: invalid pool_max_conn_lifetime_jitter",
		},
		{
			name:       "keyword/value with a bogus sslmode",
			connString: "host=db.example user=scoutuser7 password=s3cretpw dbname=scoutdb sslmode=bogus",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to configure TLS: sslmode is invalid",
		},
		{
			name:       "keyword/value with an unterminated quoted password",
			connString: "host=db.example user=scoutuser7 password='s3cret pw dbname=scoutdb",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to parse as keyword/value: unterminated quoted string in connection info string",
		},
		{
			name:       "keyword/value without an equals sign",
			connString: "host=db.example user=scoutuser7 s3cretpw",
			want:       "SCOUT_DATABASE_URL could not be parsed: failed to parse as keyword/value: invalid keyword/value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pgxpool.ParseConfig(tt.connString)
			if err == nil {
				t.Fatalf("pgxpool.ParseConfig(%q) = nil error, want one", tt.connString)
			}
			got := ConnStringError("SCOUT_DATABASE_URL", err).Error()
			assertNoSecret(t, got, tt.connString)
			if got != tt.want {
				t.Errorf("ConnStringError for %q = %q, want %q", tt.connString, got, tt.want)
			}
		})
	}
}

func TestConnStringErrorUnknownError(t *testing.T) {
	connString := "postgres://scoutuser7:s3cretpw@db.example/scoutdb"
	err := errors.New("something new went wrong with " + connString)
	got := ConnStringError("SCOUT_DATABASE_URL", err).Error()
	assertNoSecret(t, got, connString)
	if want := "SCOUT_DATABASE_URL could not be parsed"; got != want {
		t.Errorf("ConnStringError(%q) = %q, want %q", err, got, want)
	}
}

func TestConnStringErrorDoesNotWrap(t *testing.T) {
	clearPGEnv(t)
	_, err := pgxpool.ParseConfig("postgres://scoutuser7:s3cretpw@db example/scoutdb")
	if err == nil {
		t.Fatal("pgxpool.ParseConfig: nil error, want one")
	}
	if got := ConnStringError("SCOUT_DATABASE_URL", err); errors.Unwrap(got) != nil {
		t.Errorf("ConnStringError wraps %v; a wrapped pgx error keeps the connection string reachable", errors.Unwrap(got))
	}
}

func TestParseRepoConnStringHidesConnString(t *testing.T) {
	clearPGEnv(t)
	// The keyword/value form SetupDatabase builds, with a broken sslmode and
	// with an unterminated quote.
	for _, connString := range []string{
		"host=db.example port=5432 user=scoutuser7 password=s3cretpw dbname=scoutdb sslmode=bogus",
		"host=db.example port=5432 user=scoutuser7 password='s3cret pw dbname=scoutdb",
	} {
		cfg, err := parseRepoConnString(connString)
		if err == nil {
			t.Fatalf("parseRepoConnString(%q) = %v, nil error; want an error", connString, cfg)
		}
		got := err.Error()
		assertNoSecret(t, got, connString)
		if !strings.HasPrefix(got, repoConnStringSetting+" could not be parsed") {
			t.Errorf("parseRepoConnString(%q) error = %q, want it to start with %q", connString, got, repoConnStringSetting+" could not be parsed")
		}
	}
}

func TestParseRepoConnStringValid(t *testing.T) {
	clearPGEnv(t)
	connString := "host=db.example port=5432 user=scoutuser7 password=s3cretpw dbname=scoutdb sslmode=disable"
	cfg, err := parseRepoConnString(connString)
	if err != nil {
		t.Fatalf("parseRepoConnString(%q) = %v, want no error", connString, err)
	}
	if got := cfg.ConnConfig.Database; got != "scoutdb" {
		t.Errorf("parseRepoConnString(%q): database = %q, want %q", connString, got, "scoutdb")
	}
}
