package database

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// fn wrote there, and the value fn panicked with (nil when it returned).
func captureStdout(t *testing.T, fn func()) (out string, panicked any) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	read := make(chan string)
	go func() {
		b, _ := io.ReadAll(r) // a read error only shortens the captured text
		read <- string(b)
	}()
	orig := os.Stdout
	os.Stdout = w
	func() {
		defer func() {
			os.Stdout = orig
			panicked = recover()
		}()
		fn()
	}()
	w.Close()
	out = <-read
	r.Close()
	return out, panicked
}

// TestSetupDatabaseLocalGethPrintsNothing checks that a non-production start
// no longer prints the "in base : <SSL_CERT_FILE_PATH>" line. The password
// holds an unterminated quote, so parseRepoConnString fails and
// SetupDatabaseMaxConns panics before sql.Open: the test never connects.
func TestSetupDatabaseLocalGethPrintsNothing(t *testing.T) {
	clearPGEnv(t)
	// utils.GoDotEnvVariable calls log.Fatal when there is no .env in the
	// working directory; give it an empty one in a temporary folder.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o600); err != nil {
		t.Fatalf("writing empty .env: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("APP_ENV", "LOCAL_GETH")
	t.Setenv("SSL_CERT_FILE_PATH", "cert-dir-marker")
	t.Setenv("GETH_HOST_PATH", "db.example")
	t.Setenv("DB_USER", testUser)
	t.Setenv("DB_PASS", "'s3cret pw")
	t.Setenv("DB_NAME_DEV", "scoutdb")
	t.Setenv("HOST_SECRET_PATH", "")

	out, panicked := captureStdout(t, func() { SetupDatabaseMaxConns(0) })

	err, ok := panicked.(error)
	if !ok {
		t.Fatalf("SetupDatabaseMaxConns: panicked with %v, want the connection string parse error", panicked)
	}
	if want := repoConnStringSetting + " could not be parsed"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("SetupDatabaseMaxConns: panicked with %q, want it to start with %q", err, want)
	}
	if out != "" {
		t.Errorf("SetupDatabaseMaxConns with APP_ENV=LOCAL_GETH printed %q to stdout, want nothing", out)
	}
}
