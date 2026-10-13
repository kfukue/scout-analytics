package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// testDatabaseEnv names the variable the DB tests read; testDatabaseName is the
// only database they may run against (they drop and recreate the scout_*
// tables).
const (
	testDatabaseEnv  = "SCOUT_TEST_DATABASE_URL"
	testDatabaseName = "scout_test"
)

// testDatabaseURL returns SCOUT_TEST_DATABASE_URL for a DB test. Unset: the
// test is skipped. Set to anything but a local scout_test database (see
// checkTestDatabaseURL): the test fails before any connection is made.
func testDatabaseURL(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(testDatabaseEnv)
	if dsn == "" {
		t.Skip(testDatabaseEnv + " not set")
	}
	if err := checkTestDatabaseURL(dsn); err != nil {
		t.Fatalf("%s refused: %v; DB tests drop and recreate the scout_* tables, so they only run against a throwaway local %s database (see TESTING.md)", testDatabaseEnv, err, testDatabaseName)
	}
	return dsn
}

// checkTestDatabaseURL accepts dsn only if pgx would connect to the database
// scout_test on a local host, and not over TCP on port 5432. It parses dsn the way pgxpool.ParseConfig does
// (URL or key=value form, PG* environment defaults, multiple hosts), so it
// checks the effective target, not the text. It never connects. The returned
// error never contains the password.
func checkTestDatabaseURL(dsn string) error {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// Not wrapped: pgconn redacts the password in its own text but the
		// url.Error it wraps quotes the whole URL, password included.
		return errors.New("cannot parse it as a pgx connection string (URL or key=value)")
	}
	if cfg.Database != testDatabaseName {
		return fmt.Errorf("database is %q, want %q", cfg.Database, testDatabaseName)
	}
	// In URL form a dbname= query parameter overrides the path; pgx would
	// connect to scout_test, but a URL naming another database in its path is
	// refused as ambiguous (e.g. .../prod?dbname=scout_test).
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// Not wrapped: url.Error quotes the whole URL, password included.
			return errors.New("cannot parse it as a URL")
		}
		if p := strings.TrimLeft(u.Path, "/"); p != "" && p != testDatabaseName {
			return fmt.Errorf("URL path names database %q but dbname is %q; use one name", p, cfg.Database)
		}
	}
	// pgx tries the main host, then each fallback (multiple hosts, TLS
	// fallbacks): every one must be local and off TCP port 5432.
	if err := checkLocalHost(cfg.Host, cfg.Port); err != nil {
		return err
	}
	for _, fb := range cfg.Fallbacks {
		if err := checkLocalHost(fb.Host, fb.Port); err != nil {
			return err
		}
	}
	return nil
}

// sharedPostgresPort is the port of the local Postgres service the owner uses.
// DB tests never connect to it over TCP. pgx also uses it when no port is
// given, so a DSN without a port is refused too.
const sharedPostgresPort = 5432

// checkLocalHost accepts unix socket paths (as pgx decides them: an absolute
// path is a socket directory) on any port, and localhost, 127.0.0.1 and ::1 on
// any TCP port but 5432.
func checkLocalHost(host string, port uint16) error {
	if network, _ := pgconn.NetworkAddress(host, port); network == "unix" {
		return nil
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return fmt.Errorf("host is %q, want localhost, 127.0.0.1, ::1 or a local unix socket", host)
	}
	if port == sharedPostgresPort {
		return fmt.Errorf("port is %d (the local Postgres service); use a throwaway server on its own port", sharedPostgresPort)
	}
	return nil
}

// clearPGEnv empties the libpq variables pgconn.ParseConfig reads, so a
// developer's shell cannot change what a test DSN resolves to.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGTARGETSESSIONATTRS"} {
		t.Setenv(k, "")
	}
}

func TestCheckTestDatabaseURL(t *testing.T) {
	clearPGEnv(t)
	accepted := []string{
		"postgres://postgres@127.0.0.1:55434/scout_test?sslmode=disable",
		"postgresql://postgres@127.0.0.1:55434/scout_test",
		"postgres://postgres@localhost:55434/scout_test?sslmode=disable",
		"postgres://postgres@LocalHost:55434/scout_test",
		"postgres://postgres@[::1]:55434/scout_test?sslmode=disable",
		"postgres://postgres@127.0.0.1:55434/scout_test?sslmode=prefer", // TLS fallback, same host
		"postgres://postgres@127.0.0.1:55434/?dbname=scout_test",
		"postgres://postgres@127.0.0.1:55434/scout_test?dbname=scout_test",
		"postgres://postgres@127.0.0.1:55434,localhost:55435/scout_test",
		"postgres:///scout_test?host=/var/run/postgresql", // unix socket, URL form, default port
		"host=127.0.0.1 port=55434 user=postgres dbname=scout_test sslmode=disable",
		"host=localhost port=55434 dbname=scout_test",
		"host=::1 port=55434 dbname=scout_test",
		"host=/tmp dbname=scout_test",                     // unix socket directory, default port
		"host=/tmp port=5432 dbname=scout_test",           // the port rule is for TCP only
		`host=C:\\pgsock dbname=scout_test`,               // pgx treats a drive path as a socket directory
		"port=55434 dbname=scout_test",                    // pgx default host: localhost or a local socket
		"host=127.0.0.1,::1 port=55434 dbname=scout_test", // every host local
		"host=127.0.0.1,::1 port=55434,55435 dbname=scout_test",
	}
	for _, dsn := range accepted {
		if err := checkTestDatabaseURL(dsn); err != nil {
			t.Errorf("checkTestDatabaseURL(%q) = %v, want nil", dsn, err)
		}
	}

	refused := []struct {
		dsn, want string // want: a substring of the error
	}{
		{"postgres://postgres@127.0.0.1:55434/scout_test_prod", `database is "scout_test_prod"`},
		{"postgres://postgres@127.0.0.1:55434/prod_scout_test", `database is "prod_scout_test"`},
		{"postgres://postgres@127.0.0.1:55434/prod", `database is "prod"`},
		{"postgres://postgres@127.0.0.1:55434/Scout_Test", `database is "Scout_Test"`},
		{"postgres://postgres@127.0.0.1:55434/prod?dbname=scout_test", `URL path names database "prod"`},
		{"postgres://postgres@127.0.0.1:55434/scout_test?dbname=prod", `database is "prod"`},
		{"postgres://postgres@127.0.0.1:55434?sslmode=disable", `database is ""`},
		{"postgres://postgres@db.example.com:5432/scout_test", `host is "db.example.com"`},
		{"postgres://postgres@10.0.0.5/scout_test", `host is "10.0.0.5"`},
		{"postgres://postgres@127.0.0.2/scout_test", `host is "127.0.0.2"`},
		{"postgres://postgres@localhost.example.com/scout_test", `host is "localhost.example.com"`},
		{"postgres://postgres@127.0.0.1.nip.io/scout_test", `host is "127.0.0.1.nip.io"`},
		{"postgres://postgres@[2001:db8::1]:5432/scout_test", `host is "2001:db8::1"`},
		{"postgres://postgres@127.0.0.1:55434/scout_test?host=db.example.com", `host is "db.example.com"`},
		{"postgres://postgres@127.0.0.1:55434,db.example.com:5432/scout_test", `host is "db.example.com"`},
		{"host=db.example.com dbname=scout_test", `host is "db.example.com"`},
		{"host=127.0.0.1,db.example.com port=55434 dbname=scout_test", `host is "db.example.com"`},
		{"postgres://postgres@127.0.0.1:55434,localhost/scout_test", "cannot parse"}, // pgx: 2 hosts, 1 port
		{"host=127.0.0.1 dbname=scout_test_prod", `database is "scout_test_prod"`},
		{"host=127.0.0.1 port=55434", `database is ""`},
		{"postgres://postgres:hunter2@db.example.com/scout_test", `host is "db.example.com"`},
		{"postgres://postgres:hunter2@[::1/scout_test", "cannot parse"},
		// TCP port 5432 (the local Postgres service), explicit or pgx's default.
		{"postgres://postgres@127.0.0.1:5432/scout_test", "port is 5432"},
		{"postgres://postgres@LocalHost/scout_test", "port is 5432"},
		{"postgres://postgres@[::1]/scout_test", "port is 5432"},
		{"postgres://postgres:hunter2@127.0.0.1/scout_test", "port is 5432"},
		{"postgres://postgres@127.0.0.1:55434,localhost:5432/scout_test", "port is 5432"},
		{"host=localhost dbname=scout_test", "port is 5432"},
		{"host=127.0.0.1 port=5432 dbname=scout_test", "port is 5432"},
		{"host=127.0.0.1,::1 dbname=scout_test", "port is 5432"},
		{"host=127.0.0.1,::1 port=55434,5432 dbname=scout_test", "port is 5432"}, // fallback host only
		{"host=127.0.0.1 dbname='scout_test", "cannot parse"},
	}
	for _, tc := range refused {
		err := checkTestDatabaseURL(tc.dsn)
		if err == nil {
			t.Errorf("checkTestDatabaseURL(%q) = nil, want an error containing %q", tc.dsn, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkTestDatabaseURL(%q) = %q, want it to contain %q", tc.dsn, err, tc.want)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("checkTestDatabaseURL(%q) = %q, leaks the password", tc.dsn, err)
		}
	}

	// No host and no port: pgx's default host is localhost on Windows (TCP
	// port 5432: refused) but elsewhere a socket directory such as /tmp when
	// one exists (accepted: the port rule is for TCP only).
	const noHost = "dbname=scout_test"
	cfg, err := pgconn.ParseConfig(noHost)
	if err != nil {
		t.Fatalf("pgconn.ParseConfig(%q): %v", noHost, err)
	}
	network, _ := pgconn.NetworkAddress(cfg.Host, cfg.Port)
	if runtime.GOOS == "windows" && network != "tcp" {
		t.Errorf("pgx default for %q on Windows: got network %q (host %q), want tcp", noHost, network, cfg.Host)
	}
	err = checkTestDatabaseURL(noHost)
	switch {
	case network == "tcp" && (err == nil || !strings.Contains(err.Error(), "port is 5432")):
		t.Errorf("checkTestDatabaseURL(%q) with default host %q (tcp) = %v, want port 5432 refused", noHost, cfg.Host, err)
	case network == "unix" && err != nil:
		t.Errorf("checkTestDatabaseURL(%q) with default socket %q = %v, want nil", noHost, cfg.Host, err)
	}
}

// The guard checks what pgx will connect to, including PG* defaults.
func TestCheckTestDatabaseURLEnvDefaults(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("PGHOST", "db.example.com")
	if err := checkTestDatabaseURL("dbname=scout_test"); err == nil || !strings.Contains(err.Error(), `host is "db.example.com"`) {
		t.Errorf("PGHOST=db.example.com, dsn dbname=scout_test: got %v, want host refused", err)
	}
	t.Setenv("PGHOST", "")
	t.Setenv("PGDATABASE", "prod")
	if err := checkTestDatabaseURL("host=127.0.0.1"); err == nil || !strings.Contains(err.Error(), `database is "prod"`) {
		t.Errorf("PGDATABASE=prod, dsn host=127.0.0.1: got %v, want database refused", err)
	}
	t.Setenv("PGDATABASE", "scout_test")
	if err := checkTestDatabaseURL("host=127.0.0.1 port=55434"); err != nil {
		t.Errorf("PGDATABASE=scout_test, dsn host=127.0.0.1 port=55434: got %v, want nil", err)
	}
	t.Setenv("PGDATABASE", "")

	t.Setenv("PGPORT", "5432")
	if err := checkTestDatabaseURL("host=127.0.0.1 dbname=scout_test"); err == nil || !strings.Contains(err.Error(), "port is 5432") {
		t.Errorf("PGPORT=5432, dsn host=127.0.0.1 dbname=scout_test: got %v, want port refused", err)
	}
	t.Setenv("PGPORT", "55434")
	if err := checkTestDatabaseURL("host=127.0.0.1 dbname=scout_test"); err != nil {
		t.Errorf("PGPORT=55434, dsn host=127.0.0.1 dbname=scout_test: got %v, want nil", err)
	}
	t.Setenv("PGPORT", "")

	// A service file can supply the host the DSN leaves out.
	serviceFile := filepath.Join(t.TempDir(), "pg_service.conf")
	if err := os.WriteFile(serviceFile, []byte("[x]\nhost=db.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", serviceFile)
	t.Setenv("PGSERVICE", "x")
	if err := checkTestDatabaseURL("dbname=scout_test port=55434"); err == nil || !strings.Contains(err.Error(), `host is "db.example.com"`) {
		t.Errorf("PGSERVICE=x (host=db.example.com), dsn dbname=scout_test port=55434: got %v, want host refused", err)
	}
}

// recordTB records skips and fatal failures instead of ending the real test;
// each still stops the calling goroutine, as the testing package does. Every
// Skip* and Fatal*/FailNow method is overridden so that no change to
// testDatabaseURL can reach the real test's Skip or FailNow through the
// embedded TB.
type recordTB struct {
	testing.TB
	skipped, failed string
}

func (r *recordTB) Helper() {}

func (r *recordTB) Skip(args ...any) {
	r.skipped = fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recordTB) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recordTB) SkipNow() {
	r.skipped = "SkipNow"
	runtime.Goexit()
}

func (r *recordTB) Fatal(args ...any) {
	r.failed = fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recordTB) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recordTB) FailNow() {
	r.failed = "FailNow"
	runtime.Goexit()
}

func TestTestDatabaseURL(t *testing.T) {
	clearPGEnv(t)
	run := func(env string) (string, *recordTB) {
		t.Helper()
		t.Setenv(testDatabaseEnv, env)
		rec := &recordTB{TB: t}
		var got string
		done := make(chan struct{})
		go func() {
			defer close(done)
			got = testDatabaseURL(rec)
		}()
		<-done
		return got, rec
	}

	if got, rec := run(""); rec.skipped != testDatabaseEnv+" not set" || rec.failed != "" || got != "" {
		t.Errorf("unset: got %q skipped=%q failed=%q, want a skip", got, rec.skipped, rec.failed)
	}

	ok := "postgres://postgres@127.0.0.1:55434/scout_test?sslmode=disable"
	if got, rec := run(ok); got != ok || rec.skipped != "" || rec.failed != "" {
		t.Errorf("%q: got %q skipped=%q failed=%q, want the DSN back", ok, got, rec.skipped, rec.failed)
	}

	bad := "postgres://postgres:hunter2@db.example.com:5432/scout_test"
	got, rec := run(bad)
	if got != "" || rec.skipped != "" || !strings.Contains(rec.failed, testDatabaseEnv+` refused: host is "db.example.com"`) {
		t.Errorf("%q: got %q skipped=%q failed=%q, want a failure naming the host", bad, got, rec.skipped, rec.failed)
	}
	if strings.Contains(rec.failed, "hunter2") {
		t.Errorf("failure message leaks the password: %q", rec.failed)
	}

	noPort := "postgres://postgres:hunter2@127.0.0.1/scout_test?sslmode=disable"
	got, rec = run(noPort)
	if got != "" || rec.skipped != "" || !strings.Contains(rec.failed, testDatabaseEnv+" refused: port is 5432") {
		t.Errorf("%q: got %q skipped=%q failed=%q, want a failure naming port 5432", noPort, got, rec.skipped, rec.failed)
	}
	if strings.Contains(rec.failed, "hunter2") || strings.Contains(rec.failed, "127.0.0.1/scout_test") {
		t.Errorf("failure message quotes the DSN: %q", rec.failed)
	}
}
