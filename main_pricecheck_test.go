package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The "node type" line of -price-check comes from a state probe, so it is
// right for a pair that never needs old state (a stablecoin quote) too.
func TestPriceCheckNodeTypeLine(t *testing.T) {
	for _, c := range []struct {
		name       string
		fullNode   bool
		stateFrom  func(f *fakeChain) uint64 // with fullNode: oldest block with state
		balanceErr string
		noState    bool // the check itself fell back to event logs
		oldCall    bool // the call block is older than mid-history
		want       []string
		notWant    string
	}{
		{name: "archive", want: []string{"archive node", "at mid-history block", "and at the call block"}, notWant: "full node"},
		{name: "archive, old call", oldCall: true, want: []string{"archive node", "older than mid-history block"}, notWant: "full node"},
		{name: "full, old call", fullNode: true, oldCall: true, want: []string{"full node (no historical state at the call block"}, notWant: "archive"},
		{name: "full", fullNode: true, want: []string{"full node (no historical state at the call block", "event logs"}, notWant: "archive"},
		{name: "full, logs used", fullNode: true, noState: true, want: []string{"full node (no historical", "this check read USD prices from event logs"}},
		{name: "full, recent state kept", fullNode: true, stateFrom: func(f *fakeChain) uint64 { return f.latest - 100000 },
			want: []string{"full node that still keeps recent state", "none at mid-history block"}, notWant: "archive"},
		{name: "busy node", balanceErr: "too many requests, try again later", want: []string{"unknown (state probe at the call block failed", "too many requests"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour)
			f.fullNode, f.balanceErr = c.fullNode, c.balanceErr
			if c.stateFrom != nil {
				f.stateFrom = c.stateFrom(f)
			}
			o := testOnchain(t, f, nil)
			o.noState.Store(c.noState)
			callBlock := f.latest - 1000 // recent: inside stateFrom
			if c.oldCall {
				callBlock = f.latest / 3
			}
			got := nodeTypeLine(context.Background(), o, callBlock)
			if !strings.HasPrefix(got, "node type:    ") {
				t.Errorf("got %q, want the \"node type:\" prefix", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("fullNode=%v stateFrom=%d err=%q: got %q, want it to contain %q", c.fullNode, f.stateFrom, c.balanceErr, got, w)
				}
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Errorf("fullNode=%v: got %q, want no %q", c.fullNode, got, c.notWant)
			}
		})
	}
}

// Without a database (SCOUT_DB=off, or it could not be opened) -price-check
// uses the environment's feeds only and says so.
func TestPriceCheckFeedsWithoutDB(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	const tok, feed = "0x1111111111111111111111111111111111111111", "0x00000000000000000000000000000000000000f1"
	for _, c := range []struct {
		name  string
		dbErr error
		want  string
	}{
		{"SCOUT_DB=off", nil, "(no database (SCOUT_DB=off))"},
		{"open failed", errors.New("ping: connection refused"), "(database not opened: ping: connection refused)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := testOnchain(t, f, map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + feed + "," + tok + "=" + feed})
			got := priceCheckFeeds(context.Background(), o, nil, c.dbErr)
			for _, w := range []string{"feeds:        2 Robinhood Chain", "from SCOUT_*CHAINLINK_FEEDS only; 0 from the asset database", c.want} {
				if !strings.Contains(got, w) {
					t.Errorf("dbErr=%v: got %q, want it to contain %q", c.dbErr, got, w)
				}
			}
		})
	}
}

// noPriceYetIsFinal: only "no price yet", and only after the deadline (the
// call's last horizon + 48 h).
func TestNoPriceYetIsFinal(t *testing.T) {
	deadline := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	noPrice := fmt.Errorf("USD of QT: %w", fmt.Errorf("0x22 at block 9: %w (no trade)", errNoPriceYet))
	for _, c := range []struct {
		name string
		err  error
		now  time.Time
		want bool
	}{
		{"no price yet, before the deadline", noPrice, deadline.Add(-time.Minute), false},
		{"no price yet, at the deadline", noPrice, deadline, false},
		{"no price yet, after the deadline", noPrice, deadline.Add(time.Minute), true},
		{"node error, after the deadline", errors.New("rpc error -32000: busy"), deadline.Add(time.Hour), false},
		{"no USD source, after the deadline", errNoUSDSource, deadline.Add(time.Hour), false},
		{"no error", nil, deadline.Add(time.Hour), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := noPriceYetIsFinal(c.err, c.now, deadline); got != c.want {
				t.Errorf("noPriceYetIsFinal(%v, %s, %s) = %v, want %v", c.err, c.now, deadline, got, c.want)
			}
		})
	}
}
