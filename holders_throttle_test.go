package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoadHolderExportConfig(t *testing.T) {
	keys := []string{"SCOUT_HOLDERS_RPS", "SCOUT_HOLDERS_MAX_NODE_LAG", "SCOUT_HOLDERS_MAX_PRICE_LAG", "SCOUT_HOLDERS_MAX_PAUSE"}
	defaults := holderExportConfig{RPS: 4, MaxNodeLag: 2 * time.Minute, MaxPriceLag: 2 * time.Hour, MaxPause: 30 * time.Minute}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    holderExportConfig
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{name: "all set", env: map[string]string{"SCOUT_HOLDERS_RPS": "20", "SCOUT_HOLDERS_MAX_NODE_LAG": "5m",
			"SCOUT_HOLDERS_MAX_PRICE_LAG": "90m", "SCOUT_HOLDERS_MAX_PAUSE": "1h"},
			want: holderExportConfig{RPS: 20, MaxNodeLag: 5 * time.Minute, MaxPriceLag: 90 * time.Minute, MaxPause: time.Hour}},
		{name: "lowest rate", env: map[string]string{"SCOUT_HOLDERS_RPS": "1"},
			want: holderExportConfig{RPS: 1, MaxNodeLag: 2 * time.Minute, MaxPriceLag: 2 * time.Hour, MaxPause: 30 * time.Minute}},
		{name: "0 turns both lag checks off", env: map[string]string{"SCOUT_HOLDERS_MAX_NODE_LAG": "0", "SCOUT_HOLDERS_MAX_PRICE_LAG": "0s"},
			want: holderExportConfig{RPS: 4, MaxPause: 30 * time.Minute}},
		{name: "rate 0 refused", env: map[string]string{"SCOUT_HOLDERS_RPS": "0"}, wantErr: "SCOUT_HOLDERS_RPS="},
		{name: "rate above 20", env: map[string]string{"SCOUT_HOLDERS_RPS": "21"}, wantErr: "SCOUT_HOLDERS_RPS="},
		{name: "rate negative", env: map[string]string{"SCOUT_HOLDERS_RPS": "-4"}, wantErr: "SCOUT_HOLDERS_RPS="},
		{name: "rate not a number", env: map[string]string{"SCOUT_HOLDERS_RPS": "4.5"}, wantErr: "SCOUT_HOLDERS_RPS="},
		{name: "node lag negative", env: map[string]string{"SCOUT_HOLDERS_MAX_NODE_LAG": "-1m"}, wantErr: "SCOUT_HOLDERS_MAX_NODE_LAG="},
		{name: "node lag not a duration", env: map[string]string{"SCOUT_HOLDERS_MAX_NODE_LAG": "2"}, wantErr: "SCOUT_HOLDERS_MAX_NODE_LAG="},
		{name: "price lag not a duration", env: map[string]string{"SCOUT_HOLDERS_MAX_PRICE_LAG": "1d"}, wantErr: "SCOUT_HOLDERS_MAX_PRICE_LAG="},
		{name: "pause 0 refused", env: map[string]string{"SCOUT_HOLDERS_MAX_PAUSE": "0"}, wantErr: "SCOUT_HOLDERS_MAX_PAUSE="},
		{name: "pause negative", env: map[string]string{"SCOUT_HOLDERS_MAX_PAUSE": "-30m"}, wantErr: "SCOUT_HOLDERS_MAX_PAUSE="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, tc.env[k])
			}
			got, err := loadHolderExportConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadHolderExportConfig(%v) error = %v, want one about %s", tc.env, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("loadHolderExportConfig(%v) = %+v, %v; want %+v", tc.env, got, err, tc.want)
			}
		})
	}
}

// The export's client: its own rate, one request in flight, one range at a
// time, no log cache, no mainnet node; everything else as the tracker's.
func TestHolderOnchainConfig(t *testing.T) {
	base := onchainConfig{RPCURL: "http://node.invalid:8540", RPS: 300, MaxInflight: 48, Parallel: 4, LogCache: 300_000,
		LogChunk: 100_000, MainnetRPCURL: "http://mainnet.invalid", MainnetRPS: 10,
		Feeds: map[string]string{"eth": "0xfeed"}, WETH: "0xweth"}
	got := holderOnchainConfig(base, holderExportConfig{RPS: 4})

	if got.RPS != 4 || got.MaxInflight != 1 || got.Parallel != 1 || got.LogCache != 0 || got.MainnetRPCURL != "" || got.MainnetRPS != 0 {
		t.Errorf("holderOnchainConfig(RPS 4) = RPS %d, MaxInflight %d, Parallel %d, LogCache %d, MainnetRPCURL %q, MainnetRPS %d; want 4, 1, 1, 0, \"\", 0",
			got.RPS, got.MaxInflight, got.Parallel, got.LogCache, got.MainnetRPCURL, got.MainnetRPS)
	}
	if got.RPCURL != base.RPCURL || got.LogChunk != base.LogChunk || got.WETH != base.WETH || got.Feeds["eth"] != "0xfeed" {
		t.Errorf("holderOnchainConfig changed other fields: got %+v, base %+v", got, base)
	}
	if base.RPS != 300 || base.MaxInflight != 48 || base.LogCache != 300_000 || base.MainnetRPCURL == "" {
		t.Errorf("holderOnchainConfig changed its base: %+v", base)
	}

	c := newRPCClient(got)
	if c.gap != 250*time.Millisecond || cap(c.slots) != 1 || c.parallel != 1 || c.logs != nil {
		t.Errorf("newRPCClient(holder config) = gap %s, slots %d, parallel %d, log cache %v; want 250ms, 1, 1, none",
			c.gap, cap(c.slots), c.parallel, c.logs != nil)
	}
	if o := newOnchainSource(got); o.mainnet != nil {
		t.Error("newOnchainSource(holder config) made a mainnet client; want none")
	}
}

func TestHolderNodeLag(t *testing.T) {
	noHeadCache(t)
	f := newFakeChain(t, time.Hour)
	f.mu.Lock()
	f.latest -= 3000 // the head is about 5 minutes old
	head := f.latest
	f.mu.Unlock()
	now := time.Unix(f.timeOf(head), 0).Add(5 * time.Minute)

	got, err := nodeLag(context.Background(), newRPCClient(onchainConfig{RPCURL: f.srv.URL}), now)
	if err != nil || got != 5*time.Minute {
		t.Fatalf("nodeLag(head %d, now = head time + 5m) = %s, %v; want 5m0s, nil", head, got, err)
	}
}

// A node that cannot be reached: the error comes back.
func TestHolderNodeLagUnreachable(t *testing.T) {
	noHeadCache(t)
	fastRetries(t)
	f := newFakeChain(t, time.Hour)
	f.srv.Close()

	_, err := nodeLag(context.Background(), newRPCClient(onchainConfig{RPCURL: f.srv.URL}), time.Now())
	if err == nil {
		t.Fatal("nodeLag on a closed node: got no error")
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("nodeLag on a closed node: error %v does not wrap a *url.Error; want it to", err)
	}
	if !strings.HasPrefix(err.Error(), "head block: ") {
		t.Errorf("nodeLag on a closed node: error %q; want it to start with \"head block: \"", err)
	}
}

// holderFakeTime is a clock the fake sleep moves on.
type holderFakeTime struct {
	now    time.Time
	sleeps []time.Duration
}

func (f *holderFakeTime) clock() time.Time { return f.now }

func (f *holderFakeTime) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.sleeps = append(f.sleeps, d)
	f.now = f.now.Add(d)
	return nil
}

// holderLagScript answers a lag check from the time since start.
type holderLagScript func(elapsed time.Duration) (time.Duration, error)

// lagCheck turns script into a holderLagFunc that counts its calls; a nil
// script is a check that must never be asked.
func (f *holderFakeTime) lagCheck(t *testing.T, name string, start time.Time, script holderLagScript, calls *int) holderLagFunc {
	return func(context.Context) (time.Duration, error) {
		*calls++
		if script == nil {
			t.Errorf("%s check was asked; want it off", name)
			return 0, nil
		}
		return script(f.now.Sub(start))
	}
}

var errHolderTestCheck = errors.New("check failed")

// overUntil is behind (over) until clear, then within the limit.
func overUntil(clear, over, within time.Duration) holderLagScript {
	return func(elapsed time.Duration) (time.Duration, error) {
		if elapsed < clear {
			return over, nil
		}
		return within, nil
	}
}

// failUntil fails until clear, then is within the limit.
func failUntil(clear, within time.Duration) holderLagScript {
	return func(elapsed time.Duration) (time.Duration, error) {
		if elapsed < clear {
			return 0, errHolderTestCheck
		}
		return within, nil
	}
}

func TestHolderPauserWaitClear(t *testing.T) {
	cfg := holderExportConfig{RPS: 4, MaxNodeLag: 2 * time.Minute, MaxPriceLag: 2 * time.Hour, MaxPause: 30 * time.Minute}
	nodeOK := overUntil(0, 0, 10*time.Second)
	priceOK := overUntil(0, 0, 10*time.Minute)
	for _, tc := range []struct {
		name           string
		cfg            holderExportConfig
		node, price    holderLagScript
		wantPaused     time.Duration
		wantErr        error
		wantNodeCalls  int
		wantPriceCalls int
	}{
		{name: "clear: no pause", cfg: cfg, node: nodeOK, price: priceOK, wantNodeCalls: 1, wantPriceCalls: 1},
		{name: "node behind until 90s", cfg: cfg, node: overUntil(90*time.Second, 3*time.Minute, 5*time.Second), price: priceOK,
			wantPaused: 90 * time.Second, wantNodeCalls: 4, wantPriceCalls: 1},
		// Asked at 0, 60s, 120s; the answers at 30s and 90s are the cached ones.
		{name: "price behind until 2m", cfg: cfg, node: nodeOK, price: overUntil(2*time.Minute, 3*time.Hour, time.Hour),
			wantPaused: 2 * time.Minute, wantNodeCalls: 5, wantPriceCalls: 3},
		{name: "node check error pauses", cfg: cfg, node: failUntil(time.Minute, time.Second), price: priceOK,
			wantPaused: time.Minute, wantNodeCalls: 3, wantPriceCalls: 1},
		// The error at 0 is reused at 30s; asked again at 60s.
		{name: "price check error pauses, cached a minute", cfg: cfg, node: nodeOK, price: failUntil(30*time.Second, time.Minute),
			wantPaused: time.Minute, wantNodeCalls: 3, wantPriceCalls: 2},
		// Sleeps at 0, 30s, 60s, 90s; at 2m one more step would pass MaxPause.
		{name: "pause longer than MaxPause", cfg: holderExportConfig{MaxNodeLag: time.Minute, MaxPriceLag: time.Hour, MaxPause: 2 * time.Minute},
			node: overUntil(time.Hour, 5*time.Minute, 0), price: priceOK, wantPaused: 2 * time.Minute, wantErr: errHolderPausedTooLong, wantNodeCalls: 5},
		{name: "MaxPause below one step", cfg: holderExportConfig{MaxNodeLag: time.Minute, MaxPriceLag: time.Hour, MaxPause: 10 * time.Second},
			node: overUntil(time.Hour, 5*time.Minute, 0), price: priceOK, wantErr: errHolderPausedTooLong, wantNodeCalls: 1},
		{name: "node check off", cfg: holderExportConfig{MaxPriceLag: time.Hour, MaxPause: time.Minute}, price: priceOK, wantPriceCalls: 1},
		{name: "price check off", cfg: holderExportConfig{MaxNodeLag: time.Minute, MaxPause: time.Minute}, node: nodeOK, wantNodeCalls: 1},
		{name: "both checks off", cfg: holderExportConfig{MaxPause: time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			ft := &holderFakeTime{now: start}
			var nodeCalls, priceCalls int
			var logs bytes.Buffer
			p := newHolderPauser(tc.cfg, ft.lagCheck(t, "node", start, tc.node, &nodeCalls),
				ft.lagCheck(t, "price", start, tc.price, &priceCalls), ft.clock, ft.sleep, &logs)

			paused, err := p.waitClear(context.Background())
			if paused != tc.wantPaused || !errors.Is(err, tc.wantErr) {
				t.Errorf("waitClear() = %s, %v; want %s, %v", paused, err, tc.wantPaused, tc.wantErr)
			}
			if nodeCalls != tc.wantNodeCalls || priceCalls != tc.wantPriceCalls {
				t.Errorf("checks asked: node %d, price %d; want %d, %d", nodeCalls, priceCalls, tc.wantNodeCalls, tc.wantPriceCalls)
			}
			wantSleeps := int(tc.wantPaused / holderPauseStep)
			if len(ft.sleeps) != wantSleeps {
				t.Errorf("slept %d times (%v); want %d", len(ft.sleeps), ft.sleeps, wantSleeps)
			}
			for _, d := range ft.sleeps {
				if d != holderPauseStep {
					t.Errorf("slept %s; want %s each time", d, holderPauseStep)
				}
			}
			if n := strings.Count(logs.String(), "\n"); n != wantSleeps {
				t.Errorf("logged %d lines; want %d (one per pause step; giving up is returned, not logged):\n%s", n, wantSleeps, logs.String())
			}
		})
	}
}

// Over many clear waitClear calls the node lag (a node request per new head)
// is asked at most once per 30 s and the price lag (a database query) at
// most once a minute.
func TestHolderPauserLagsCachedOverClearWaits(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ft := &holderFakeTime{now: start}
	var nodeCalls, priceCalls int
	p := newHolderPauser(holderExportConfig{MaxNodeLag: time.Minute, MaxPriceLag: time.Hour, MaxPause: time.Minute},
		ft.lagCheck(t, "node", start, overUntil(0, 0, time.Second), &nodeCalls),
		ft.lagCheck(t, "price", start, overUntil(0, 0, time.Minute), &priceCalls), ft.clock, ft.sleep, nil)
	for i := 0; i < 30; i++ { // 30 calls, 10 s apart: 5 minutes
		if _, err := p.waitClear(context.Background()); err != nil {
			t.Fatalf("waitClear() at %s: %v; want nil", ft.now.Sub(start), err)
		}
		ft.now = ft.now.Add(10 * time.Second)
	}
	if nodeCalls != 10 || priceCalls != 5 {
		t.Errorf("over 5 minutes, 30 waits: node asked %d, price asked %d; want 10, 5", nodeCalls, priceCalls)
	}
}

// Clear waits reuse the node answer; once the node is behind, a blocked wait
// asks it again after each pause step, and the clear answer at the end is
// reused again.
func TestHolderPauserNodeLagRecheckedWhileBlocked(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ft := &holderFakeTime{now: start}
	var nodeCalls int
	// Clear until 40s, behind from 40s to 2m, clear again afterwards.
	node := func(elapsed time.Duration) (time.Duration, error) {
		if elapsed >= 40*time.Second && elapsed < 2*time.Minute {
			return 5 * time.Minute, nil
		}
		return time.Second, nil
	}
	p := newHolderPauser(holderExportConfig{MaxNodeLag: 2 * time.Minute, MaxPause: time.Hour},
		ft.lagCheck(t, "node", start, node, &nodeCalls), nil, ft.clock, ft.sleep, nil)
	wait := func(wantPaused time.Duration, wantCalls int) {
		t.Helper()
		at := ft.now.Sub(start)
		paused, err := p.waitClear(context.Background())
		if err != nil || paused != wantPaused || nodeCalls != wantCalls {
			t.Fatalf("waitClear() at %s = %s, %v, node asked %d in all; want %s, nil, %d", at, paused, err, nodeCalls, wantPaused, wantCalls)
		}
	}

	wait(0, 1) // 0s: asked, clear
	ft.now = ft.now.Add(20 * time.Second)
	wait(0, 1) // 20s: the answer from 0s
	ft.now = ft.now.Add(25 * time.Second)
	// 45s: asked, behind; then asked at 75s and 105s (behind) and 135s (clear).
	wait(90*time.Second, 5)
	if len(ft.sleeps) != 3 {
		t.Errorf("slept %d times (%v) while the node was behind; want 3", len(ft.sleeps), ft.sleeps)
	}
	ft.now = ft.now.Add(29 * time.Second)
	wait(0, 5) // 164s: the answer from 135s
	ft.now = ft.now.Add(time.Second)
	wait(0, 6) // 165s: 30s old, asked again
}

func TestHolderPauserLagCacheGet(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	errNode := errors.New("connection refused")

	t.Run("an answer, an error included, is reused for less than ttl", func(t *testing.T) {
		var c holderLagCache
		calls := 0
		answers := []error{errNode, nil}
		check := func(context.Context) (time.Duration, error) {
			calls++
			if err := answers[calls-1]; err != nil {
				return 0, err
			}
			return 7 * time.Second, nil
		}
		for _, step := range []struct {
			after     time.Duration
			wantLag   time.Duration
			wantErr   error
			wantCalls int
		}{
			{after: 0, wantErr: errNode, wantCalls: 1},
			{after: 29 * time.Second, wantErr: errNode, wantCalls: 1},
			{after: 30 * time.Second, wantLag: 7 * time.Second, wantCalls: 2},
			{after: 59 * time.Second, wantLag: 7 * time.Second, wantCalls: 2},
		} {
			lag, err := c.get(context.Background(), start.Add(step.after), 30*time.Second, check)
			if lag != step.wantLag || !errors.Is(err, step.wantErr) || calls != step.wantCalls {
				t.Errorf("get(ttl 30s) at %s = %s, %v, asked %d in all; want %s, %v, %d",
					step.after, lag, err, calls, step.wantLag, step.wantErr, step.wantCalls)
			}
		}
	})

	t.Run("an answer given after ctx ended is not kept", func(t *testing.T) {
		var c holderLagCache
		calls := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		check := func(context.Context) (time.Duration, error) {
			calls++
			if calls == 1 {
				cancel() // the export is stopped during the check
				return 0, context.Canceled
			}
			return time.Second, nil
		}
		if _, err := c.get(ctx, start, 30*time.Second, check); !errors.Is(err, context.Canceled) {
			t.Fatalf("get(ctx cancelled during check) error = %v; want context.Canceled", err)
		}
		lag, err := c.get(context.Background(), start.Add(time.Second), 30*time.Second, check)
		if lag != time.Second || err != nil || calls != 2 {
			t.Errorf("get after a cancelled check = %s, %v, asked %d in all; want 1s, nil, 2 (nothing kept)", lag, err, calls)
		}
	})
}

// A failing node check is reused for 30 s across waitClear calls too: no new
// node request until the answer is 30 s old.
func TestHolderPauserNodeErrorReused(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ft := &holderFakeTime{now: start}
	var nodeCalls int
	p := newHolderPauser(holderExportConfig{MaxNodeLag: time.Minute, MaxPause: 10 * time.Second},
		ft.lagCheck(t, "node", start, failUntil(time.Hour, 0), &nodeCalls), nil, ft.clock, ft.sleep, nil)
	for _, step := range []struct {
		at        time.Duration
		wantCalls int
	}{{0, 1}, {10 * time.Second, 1}, {29 * time.Second, 1}, {30 * time.Second, 2}} {
		ft.now = start.Add(step.at)
		_, err := p.waitClear(context.Background())
		if !errors.Is(err, errHolderPausedTooLong) || !strings.Contains(err.Error(), errHolderTestCheck.Error()) || nodeCalls != step.wantCalls {
			t.Errorf("waitClear() at %s = %v, node asked %d in all; want errHolderPausedTooLong naming %q, %d",
				step.at, err, nodeCalls, errHolderTestCheck, step.wantCalls)
		}
	}
}

func TestHolderPauserContextCancel(t *testing.T) {
	cfg := holderExportConfig{MaxNodeLag: time.Minute, MaxPriceLag: time.Hour, MaxPause: time.Hour}
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	t.Run("already cancelled", func(t *testing.T) {
		ft := &holderFakeTime{now: start}
		var nodeCalls, priceCalls int
		p := newHolderPauser(cfg, ft.lagCheck(t, "node", start, nil, &nodeCalls), ft.lagCheck(t, "price", start, nil, &priceCalls),
			ft.clock, ft.sleep, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.waitClear(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("waitClear(cancelled ctx) error = %v; want context.Canceled", err)
		}
	})

	t.Run("cancelled while paused", func(t *testing.T) {
		ft := &holderFakeTime{now: start}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var nodeCalls int
		behind := func(elapsed time.Duration) (time.Duration, error) {
			if elapsed >= time.Minute {
				cancel() // the export is stopped during the third check
			}
			return 5 * time.Minute, nil
		}
		p := newHolderPauser(cfg, ft.lagCheck(t, "node", start, behind, &nodeCalls), nil, ft.clock, ft.sleep, nil)
		paused, err := p.waitClear(ctx)
		if !errors.Is(err, context.Canceled) || paused != time.Minute || nodeCalls != 3 {
			t.Errorf("waitClear(cancelled at 1m) = %s, %v after %d checks; want 1m0s, context.Canceled after 3", paused, err, nodeCalls)
		}
	})
}

// Each pause step logs its reason and the setting; giving up returns an
// error naming the last reason, the limit and how long it paused.
func TestHolderPauserLogAndGiveUpError(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ft := &holderFakeTime{now: start}
	first := true
	node := func(context.Context) (time.Duration, error) {
		if first {
			first = false
			return 0, errors.New("connection refused")
		}
		return 3 * time.Minute, nil
	}
	price := func(context.Context) (time.Duration, error) { return 3 * time.Hour, nil }
	var logs bytes.Buffer
	cfg := holderExportConfig{MaxNodeLag: 2 * time.Minute, MaxPriceLag: 2 * time.Hour, MaxPause: time.Minute}
	p := newHolderPauser(cfg, node, price, ft.clock, ft.sleep, &logs)

	_, err := p.waitClear(context.Background())
	if !errors.Is(err, errHolderPausedTooLong) {
		t.Fatalf("waitClear() error = %v; want errHolderPausedTooLong", err)
	}
	wantErr := "holder export: paused longer than SCOUT_HOLDERS_MAX_PAUSE (1m0s): " +
		"node head block 3m0s behind (SCOUT_HOLDERS_MAX_NODE_LAG is 2m0s) after 1m0s"
	if err.Error() != wantErr {
		t.Errorf("waitClear() error = %q; want %q", err, wantErr)
	}
	wantLogs := "holder export: node head block check failed: connection refused; pausing 30s (paused 0s so far)\n" +
		"holder export: node head block 3m0s behind (SCOUT_HOLDERS_MAX_NODE_LAG is 2m0s); pausing 30s (paused 30s so far)\n"
	if got := logs.String(); got != wantLogs {
		t.Errorf("log =\n%s\nwant\n%s", got, wantLogs)
	}
}

// The price check holds the export back when the node is fine, and its
// reason names its own setting.
func TestHolderPauserPriceReason(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ft := &holderFakeTime{now: start}
	node := func(context.Context) (time.Duration, error) { return time.Second, nil }
	price := func(context.Context) (time.Duration, error) { return 150 * time.Minute, nil }
	cfg := holderExportConfig{MaxNodeLag: 2 * time.Minute, MaxPriceLag: 2 * time.Hour, MaxPause: 10 * time.Second}
	_, err := newHolderPauser(cfg, node, price, ft.clock, ft.sleep, nil).waitClear(context.Background())
	want := "latest-price pass 2h30m0s behind (SCOUT_HOLDERS_MAX_PRICE_LAG is 2h0m0s) after 0s"
	if !errors.Is(err, errHolderPausedTooLong) || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("waitClear(price 2h30m behind) error = %v; want errHolderPausedTooLong ending %q", err, want)
	}
}
