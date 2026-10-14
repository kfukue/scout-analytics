package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestExportCSVFile(t *testing.T) {
	errExport := errors.New("export failed")
	tests := []struct {
		name     string
		existing string // file content before the export ("" = no file)
		write    string
		rows     int
		err      error
	}{
		{name: "new file", write: "h\n1\n2\n", rows: 2},
		{name: "replaces an existing file", existing: "old content that is longer\n", write: "h\n", rows: 0},
		{name: "export error is returned with the partial count", write: "h\n1\n", rows: 1, err: errExport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.csv")
			if tc.existing != "" {
				if err := os.WriteFile(path, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			n, err := exportCSVFile(path, func(w io.Writer) (int, error) {
				if _, err := io.WriteString(w, tc.write); err != nil {
					return 0, err
				}
				return tc.rows, tc.err
			})
			if n != tc.rows || !errors.Is(err, tc.err) {
				t.Fatalf("exportCSVFile = %d, %v; want %d, %v", n, err, tc.rows, tc.err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.write {
				t.Fatalf("file content = %q, want %q", b, tc.write)
			}
		})
	}
}

func TestExportCSVFileCreateError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "out.csv")
	called := false
	n, err := exportCSVFile(path, func(io.Writer) (int, error) { called = true; return 5, nil })
	if err == nil || n != 0 || called {
		t.Fatalf("exportCSVFile(%q) = %d, %v, export called %v; want 0, an error, not called", path, n, err, called)
	}
}
