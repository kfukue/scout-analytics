package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The give-up rule for calls that keep failing (SCOUT_TRACK_MAX_ATTEMPTS):
// the setting, the decision and the progress checks. No database.
// ---------------------------------------------------------------------------

func TestLoadTrackMaxAttempts(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int
	}{
		{"", 10}, // unset: the default
		{"0", 0}, // off
		{" 0 ", 0},
		{"1", 1},
		{"25", 25},
	} {
		t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", c.v)
		if got, err := loadTrackMaxAttempts(); err != nil || got != c.want {
			t.Errorf("SCOUT_TRACK_MAX_ATTEMPTS=%q: got %d, %v; want %d, nil", c.v, got, err, c.want)
		}
	}
	for _, bad := range []string{"-1", "ten", "1.5", "10x"} {
		t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", bad)
		if got, err := loadTrackMaxAttempts(); err == nil || !strings.Contains(err.Error(), "SCOUT_TRACK_MAX_ATTEMPTS") {
			t.Errorf("SCOUT_TRACK_MAX_ATTEMPTS=%q: got %d, %v; want an error naming the setting", bad, got, err)
		}
	}
}

// An invalid value stops the program at start: loadPriceConfig (read by
// loadConfig before anything runs) refuses it, naming the setting; a valid
// one reaches the tracker's config.
func TestPriceConfigTrackMaxAttempts(t *testing.T) {
	for v, want := range map[string]int{"": 10, "0": 0, "3": 3} {
		t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", v)
		if pc, err := loadPriceConfig(); err != nil || pc.MaxAttempts != want {
			t.Errorf("SCOUT_TRACK_MAX_ATTEMPTS=%q: got %d, %v; want %d, nil", v, pc.MaxAttempts, err, want)
		}
	}
	for _, bad := range []string{"-1", "ten"} {
		t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", bad)
		if _, err := loadPriceConfig(); err == nil || !strings.Contains(err.Error(), "SCOUT_TRACK_MAX_ATTEMPTS") {
			t.Errorf("SCOUT_TRACK_MAX_ATTEMPTS=%q: got %v, want an error naming the setting", bad, err)
		}
	}
}

func TestMaxAttemptsText(t *testing.T) {
	if got := maxAttemptsText(10); !strings.Contains(got, "10 attempts") || !strings.Contains(got, "(SCOUT_TRACK_MAX_ATTEMPTS)") {
		t.Errorf("maxAttemptsText(10) = %q, want the limit and the setting's name", got)
	}
	if got := maxAttemptsText(0); !strings.Contains(got, "never") || !strings.Contains(got, "SCOUT_TRACK_MAX_ATTEMPTS=0") {
		t.Errorf("maxAttemptsText(0) = %q, want \"never\" and SCOUT_TRACK_MAX_ATTEMPTS=0", got)
	}
}

// The rule fires only when all four conditions hold; each one false on its
// own keeps the call in error. 0 turns it off.
func TestGiveUpOnError(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	past, ahead := now.Add(-time.Hour), now.Add(time.Hour)
	node := errors.New("swaps to +1d: rpc error -32000: request timed out")
	pending := fmt.Errorf("USD price of QT: %w", fmt.Errorf("0x22 at block 9: %w (…)", errUSDSearchPending))
	noPrice := fmt.Errorf("candles to +1d: %w", errNoPriceYet)
	for _, c := range []struct {
		name       string
		max, att   int
		deadline   time.Time
		err        error
		progressed bool
		want       bool
	}{
		{"all four hold", 10, 10, past, node, false, true},
		{"all four hold, more attempts", 10, 717, past, node, false, true},
		{"no price yet counts as a failure", 10, 10, past, noPrice, false, true},
		{"attempts under the limit", 10, 9, past, node, false, false},
		{"not past the deadline", 10, 10, ahead, node, false, false},
		{"at the deadline itself", 10, 10, now, node, false, false},
		{"USD pool search not finished", 10, 10, past, pending, false, false},
		{"progress in this run", 10, 10, past, node, true, false},
		{"0 = off", 0, 717, past, node, false, false},
	} {
		if got := giveUpOnError(c.max, c.att, now, c.deadline, c.err, c.progressed); got != c.want {
			t.Errorf("%s: giveUpOnError(max %d, attempts %d, deadline %s, %v, progressed %v) = %v, want %v",
				c.name, c.max, c.att, c.deadline.Format(time.RFC3339), c.err, c.progressed, got, c.want)
		}
	}
}

func TestGaveUpErrorText(t *testing.T) {
	deadline := time.Date(2026, 9, 1, 7, 30, 0, 0, time.FixedZone("PDT", -7*3600))
	got := gaveUpErrorText(12, deadline, errors.New("swaps to +1d: timeout"))
	want := "gave up after 12 attempts, past the deadline 2026-09-01 14:30 UTC: swaps to +1d: timeout"
	if got != want {
		t.Errorf("gaveUpErrorText: got %q, want %q", got, want)
	}
}

func TestOnchainProgressed(t *testing.T) {
	base := func() *onchainState {
		return &onchainState{Pool: "0xp", EntryPriceQ: 1, PreDone: true, ScanBlock: 100, Done: map[string]bool{"1h": true}}
	}
	with := func(f func(*onchainState)) *onchainState {
		st := base()
		f(st)
		return st
	}
	for _, c := range []struct {
		name       string
		start, end *onchainState
		entrySet   bool
		want       bool
	}{
		{"nothing changed", base(), base(), false, false},
		{"no state before or after (the pool step failed)", nil, nil, false, false},
		{"pool found", nil, base(), false, true},
		{"entry price in quote units found", with(func(s *onchainState) { s.EntryPriceQ = 0 }), base(), false, true},
		{"entry price set", base(), base(), true, true},
		{"pre-call trades stored", with(func(s *onchainState) { s.PreDone = false }), base(), false, true},
		{"scan cursor moved on", base(), with(func(s *onchainState) { s.ScanBlock = 200 }), false, true},
		{"one more horizon", base(), with(func(s *onchainState) { s.Done["1d"] = true }), false, true},
		{"rug found", base(), with(func(s *onchainState) { s.RugBlock = 150 }), false, true},
		{"Done map created empty", with(func(s *onchainState) { s.Done = nil }), with(func(s *onchainState) { s.Done = map[string]bool{"1h": true} }), false, true},
		{"empty Done map vs none", with(func(s *onchainState) { s.Done = nil }), with(func(s *onchainState) { s.Done = map[string]bool{} }), false, false},
	} {
		if got := onchainProgressed(c.start, c.end, c.entrySet); got != c.want {
			t.Errorf("%s: onchainProgressed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGeckoProgressed(t *testing.T) {
	pool, price := "0xpool", 1.0
	known := ScoutCallTracking{PoolAddress: &pool, EntryPriceUSD: &price}
	for _, c := range []struct {
		name       string
		start, end ScoutCallTracking
		stored     int
		want       bool
	}{
		{"nothing new", known, known, 0, false},
		{"pool found", ScoutCallTracking{}, ScoutCallTracking{PoolAddress: &pool}, 0, true},
		{"entry price set", ScoutCallTracking{PoolAddress: &pool}, known, 0, true},
		{"a horizon stored", known, known, 1, true},
		{"no pool yet, still none", ScoutCallTracking{}, ScoutCallTracking{}, 0, false},
	} {
		if got := geckoProgressed(&c.start, &c.end, c.stored); got != c.want {
			t.Errorf("%s: geckoProgressed = %v, want %v", c.name, got, c.want)
		}
	}
}
