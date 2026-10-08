package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDefaultPoolMaxConns(t *testing.T) {
	tests := []struct {
		workers int
		want    int32
	}{
		{workers: 0, want: 4},   // floor
		{workers: 1, want: 5},   // one worker (the GeckoTerminal source, see TestPoolWorkersFollowTracker)
		{workers: 8, want: 12},  // SCOUT_TRACK_WORKERS default
		{workers: 12, want: 16}, // prod
		{workers: 28, want: 32}, // exactly the cap
		{workers: 64, want: 32}, // SCOUT_TRACK_WORKERS maximum: capped
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("workers=%d", tt.workers), func(t *testing.T) {
			if got := defaultPoolMaxConns(tt.workers); got != tt.want {
				t.Errorf("defaultPoolMaxConns(%d) = %d, want %d", tt.workers, got, tt.want)
			}
		})
	}
}

// TestPoolWorkersFollowTracker checks that the pool is sized from the workers the
// tracker really runs: the GeckoTerminal source runs one, whatever
// SCOUT_TRACK_WORKERS says.
func TestPoolWorkersFollowTracker(t *testing.T) {
	tests := []struct {
		name    string
		pc      priceConfig
		wantW   int
		wantMax int32
	}{
		{name: "gecko ignores SCOUT_TRACK_WORKERS", pc: priceConfig{Source: "gecko", Workers: 12}, wantW: 1, wantMax: 5},
		{name: "onchain uses SCOUT_TRACK_WORKERS", pc: priceConfig{Source: "onchain", Workers: 12}, wantW: 12, wantMax: 16},
		{name: "onchain with 0 workers runs one", pc: priceConfig{Source: "onchain", Workers: 0}, wantW: 1, wantMax: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.pc.trackWorkers()
			if w != tt.wantW {
				t.Errorf("priceConfig{Source: %q, Workers: %d}.trackWorkers() = %d, want %d", tt.pc.Source, tt.pc.Workers, w, tt.wantW)
			}
			if got := defaultPoolMaxConns(w); got != tt.wantMax {
				t.Errorf("defaultPoolMaxConns(%d) for Source %q, Workers %d = %d, want %d", w, tt.pc.Source, tt.pc.Workers, got, tt.wantMax)
			}
			if got, want := defaultPoolSource(w), fmt.Sprintf("default for %d tracker worker(s)", tt.wantW); got != want {
				t.Errorf("defaultPoolSource(%d) = %q, want %q", w, got, want)
			}
		})
	}
}

func TestApplyPoolMaxConns(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		workers  int
		want     int32
		wantFrom string // substring of the returned source
	}{
		{
			name: "url without pool_max_conns: default for workers", dsn: "postgres://u@127.0.0.1:5432/scout_test?sslmode=disable",
			workers: 12, want: 16, wantFrom: "default for 12",
		},
		{
			name: "keyword dsn without pool_max_conns: default for workers", dsn: "host=127.0.0.1 port=5432 user=u dbname=scout_test sslmode=disable",
			workers: 8, want: 12, wantFrom: "default for 8",
		},
		{
			name: "url with explicit value below the default is kept", dsn: "postgres://u@127.0.0.1:5432/scout_test?sslmode=disable&pool_max_conns=2",
			workers: 12, want: 2, wantFrom: "pool_max_conns",
		},
		{
			name: "keyword dsn with explicit value is kept", dsn: "host=127.0.0.1 user=u dbname=scout_test pool_max_conns=3",
			workers: 12, want: 3, wantFrom: "pool_max_conns",
		},
		{
			name: "explicit value above the cap is kept", dsn: "postgres://u@127.0.0.1/scout_test?pool_max_conns=100",
			workers: 12, want: 100, wantFrom: "pool_max_conns",
		},
		{
			name: "explicit value equal to the default is still explicit", dsn: "postgres://u@127.0.0.1/scout_test?pool_max_conns=16",
			workers: 12, want: 16, wantFrom: "pool_max_conns",
		},
		{
			name: "many workers without pool_max_conns: capped", dsn: "postgres://u@127.0.0.1/scout_test",
			workers: 64, want: 32, wantFrom: "default for 64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := pgxpool.ParseConfig(tt.dsn)
			if err != nil {
				t.Fatalf("pgxpool.ParseConfig(%q): %v", tt.dsn, err)
			}
			from, err := applyPoolMaxConns(tt.dsn, cfg, tt.workers)
			if err != nil {
				t.Fatalf("applyPoolMaxConns(%q, %d): %v", tt.dsn, tt.workers, err)
			}
			if cfg.MaxConns != tt.want {
				t.Errorf("applyPoolMaxConns(%q, %d): MaxConns = %d, want %d", tt.dsn, tt.workers, cfg.MaxConns, tt.want)
			}
			if !strings.Contains(from, tt.wantFrom) {
				t.Errorf("applyPoolMaxConns(%q, %d): source = %q, want it to contain %q", tt.dsn, tt.workers, from, tt.wantFrom)
			}
			if _, ok := cfg.ConnConfig.RuntimeParams["pool_max_conns"]; ok {
				t.Errorf("applyPoolMaxConns(%q, %d): pool_max_conns left in the runtime parameters (would be sent to the server)", tt.dsn, tt.workers)
			}
		})
	}
}
