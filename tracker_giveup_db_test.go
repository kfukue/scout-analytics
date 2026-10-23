package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Database tests of the give-up rule (SCOUT_TRACK_MAX_ATTEMPTS, tracker_giveup.go)
// through the on-chain tracker on the fake chain, and the GeckoTerminal one.
// ---------------------------------------------------------------------------

// giveUpCall posts a call ago old on a fake chain, quoted in QT ($2 through
// its feed), whose 1d segment fails on every run: the QT feed answers "rate
// limit exceeded" for the blocks 3 to 6 hours after the call, where a trade
// needs its hour's QT price. A first run stores the 1h horizon and fails on
// the 1d candles. maxAttempts is SCOUT_TRACK_MAX_ATTEMPTS.
func giveUpCall(t *testing.T, st *ScoutStore, ago time.Duration, maxAttempts string) (*scanner, int) {
	t.Helper()
	const (
		quote = "0x2222222222222222222222222222222222222222"
		qFeed = "0x00000000000000000000000000000000000000f8"
		token = "0x4444444444444444444444444444444444444444"
		pool  = "0x00000000000000000000000000000000000000c3"
	)
	f := ltChain(t, 40*ltDay)
	f.addToken(quote, 18, "QT")
	f.constCall(qFeed, selDecimals, ret(wInt(8)))
	f.constCall(qFeed, selLatestRound, ret(wInt(1), wInt(2e8), wInt(0), wInt(0), wInt(1))) // QT = $2
	f.ltPool(token, pool, quote)
	c := rugCall{entry: time.Now().Add(-ago)}
	c.eb = f.blockAtTime(c.entry)
	f.ltTrade(token, pool, c.at(-time.Minute), 1e-3) // entry
	f.ltTrade(token, pool, c.at(30*time.Second), 1.2e-3)
	f.ltTrade(token, pool, c.at(33*time.Minute), 1.5e-3)
	f.ltTrade(token, pool, c.at(5*time.Hour), 5e-3) // in the hours the feed fails
	f.ltTrade(token, pool, c.at(20*time.Hour), 1.8e-3)
	busyFrom, busyTo := c.at(3*time.Hour), c.at(6*time.Hour)
	f.callErr = func(to, sel string, block uint64) string {
		if to == qFeed && sel == selLatestRound && block >= busyFrom && block <= busyTo {
			return "rate limit exceeded"
		}
		return ""
	}
	t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", maxAttempts)
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed+","+quote+"="+qFeed)
	s.onChannelPost(postAt(1, c.entry, token))
	return s, *(<-s.queue).CallID
}

// makeDue sets a tracking row's attempts (when attempts >= 0) and makes it due.
func makeDue(t *testing.T, st *ScoutStore, id, attempts int) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking
		SET attempts = CASE WHEN $2::int >= 0 THEN $2::int ELSE attempts END, next_check_at = now() - interval '1 minute'
		WHERE call_id = $1`, id, attempts); err != nil {
		t.Fatal(err)
	}
}

// giveUpSide is everything of a call the give-up rule must leave alone: the
// tracking row without the columns a failed run writes (status, error,
// attempts, last / next check, updated_at), its returns and its candles.
type giveUpSide struct {
	Row, Returns, Candles string
	UpdatedAt             time.Time
}

func readGiveUpSide(t *testing.T, st *ScoutStore, id int) giveUpSide {
	t.Helper()
	ctx := context.Background()
	var out giveUpSide
	if err := st.Pool.QueryRow(ctx, `SELECT (to_jsonb(t) - 'status' - 'error' - 'attempts' - 'last_checked_at' - 'next_check_at' - 'updated_at')::text, updated_at
		FROM scout_call_tracking t WHERE call_id = $1`, id).Scan(&out.Row, &out.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY horizon_seconds)::text, '') FROM scout_call_returns r WHERE call_id = $1`, id).Scan(&out.Returns); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY interval_seconds, bucket_start)::text, '') FROM scout_call_candles c WHERE call_id = $1`, id).Scan(&out.Candles); err != nil {
		t.Fatal(err)
	}
	return out
}

// dueIDs: the call ids DueTracking hands out a year from now (every row in a
// status it retries).
func dueIDs(t *testing.T, st *ScoutStore) map[int]bool {
	t.Helper()
	rows, err := st.DueTracking(context.Background(), time.Now().Add(365*ltDay), 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, r := range rows {
		ids[r.CallID] = true
	}
	return ids
}

// The rule fires only when all four conditions hold: one case where they do,
// and one per condition that does not (the fourth, the USD pool search, is
// TestTrackerGiveUpNotWhileUSDSearchPending); 0 turns the rule off.
func TestTrackerGiveUpRule(t *testing.T) {
	for _, c := range []struct {
		name         string
		ago          time.Duration
		setting      string // SCOUT_TRACK_MAX_ATTEMPTS
		before1      int    // attempts set before run 1 (-1: as created, 0)
		before2      int    // attempts set before run 2 (-1: no run 2)
		wantStatus   string
		wantAttempts int
	}{
		// run 2 makes no progress (the same piece fails), 10 attempts, 33 days old
		{"all four hold", 33 * ltDay, "", -1, 9, TrackGaveUp, 10},
		{"attempts under the limit", 33 * ltDay, "", -1, 8, TrackError, 9},
		// 2 days old: the deadline (30d + 48 h) is ahead
		{"not past the deadline", 2 * ltDay, "", -1, 9, TrackError, 10},
		// run 1 at 10 attempts: the pool, the entry and the 1h horizon are new
		{"progress in the run", 33 * ltDay, "", 9, -1, TrackError, 10},
		{"setting 0 = off", 33 * ltDay, "0", -1, 9, TrackError, 10},
		{"a lower setting", 33 * ltDay, "3", -1, 2, TrackGaveUp, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			s, id := giveUpCall(t, st, c.ago, c.setting)
			logs := captureLog(t)
			if c.before1 >= 0 {
				makeDue(t, st, id, c.before1)
			}
			s.trackDue(ctx, 50)
			tr, _ := st.GetTracking(ctx, id)
			if c.before2 < 0 { // the case is decided by run 1
				if tr.Status != c.wantStatus || tr.Attempts != c.wantAttempts {
					t.Fatalf("run 1: got status %s attempts %d error %s, want %s, %d attempts", tr.Status, tr.Attempts, strOrNil(tr.Error), c.wantStatus, c.wantAttempts)
				}
				if tr.Error == nil || !strings.HasPrefix(*tr.Error, "candles to +1d") {
					t.Fatalf("run 1: got error %s, want the 1d candles' error", strOrNil(tr.Error))
				}
				return
			}
			if tr.Status != TrackError || tr.Error == nil || !strings.Contains(*tr.Error, "candles to +1d") || !ltState(t, st, id).Done["1h"] {
				t.Fatalf("run 1: got status %s error %s, want error on the 1d candles with 1h stored", tr.Status, strOrNil(tr.Error))
			}
			makeDue(t, st, id, c.before2)
			before := readGiveUpSide(t, st, id)
			s.trackDue(ctx, 50)
			tr, _ = st.GetTracking(ctx, id)
			after := readGiveUpSide(t, st, id)
			if tr.Status != c.wantStatus || tr.Attempts != c.wantAttempts {
				t.Fatalf("run 2: got status %s attempts %d error %s, want %s, %d attempts", tr.Status, tr.Attempts, strOrNil(tr.Error), c.wantStatus, c.wantAttempts)
			}
			if before.Row != after.Row || before.Returns != after.Returns || before.Candles != after.Candles {
				t.Fatalf("run 2 (no progress) changed the call:\nrow     %s\n     ->  %s\nreturns %s\n     ->  %s\ncandles %s\n     ->  %s",
					before.Row, after.Row, before.Returns, after.Returns, before.Candles, after.Candles)
			}
			if !after.UpdatedAt.After(before.UpdatedAt) {
				t.Fatalf("updated_at: got %s, want after %s", after.UpdatedAt, before.UpdatedAt)
			}
			gaveUpLines := strings.Count(logs.String(), "gave up after")
			if c.wantStatus != TrackGaveUp {
				if tr.Error == nil || !strings.HasPrefix(*tr.Error, "candles to +1d") || gaveUpLines != 0 {
					t.Fatalf("run 2: got error %s and %d \"gave up\" log lines, want the 1d candles' error and none", strOrNil(tr.Error), gaveUpLines)
				}
				if !dueIDs(t, st)[id] {
					t.Fatalf("DueTracking: call %d (status %s) not handed out, want it retried", id, tr.Status)
				}
				return
			}
			deadline := tr.EntryAt.Add(30*ltDay + 48*time.Hour).UTC().Format("2006-01-02 15:04 UTC")
			wantPrefix := fmt.Sprintf("gave up after %d attempts, past the deadline %s: candles to +1d: ", c.wantAttempts, deadline)
			if tr.Error == nil || !strings.HasPrefix(*tr.Error, wantPrefix) {
				t.Fatalf("error: got %s, want it to start with %q", strOrNil(tr.Error), wantPrefix)
			}
			n := 0 // lines like "call 1 [#1]: gave up after 10 attempts, …"
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, fmt.Sprintf(" call %d ", id)) && strings.Contains(line, fmt.Sprintf("]: gave up after %d attempts, ", c.wantAttempts)) {
					n++
				}
			}
			if n != 1 || gaveUpLines != 1 {
				t.Fatalf("got %d log lines naming call %d (%d \"gave up\" lines), want 1:\n%s", n, id, gaveUpLines, logs.String())
			}
			if dueIDs(t, st)[id] {
				t.Fatalf("DueTracking: call %d (gave_up) still handed out", id)
			}
			// A later cycle does not touch it.
			s.trackDue(ctx, 50)
			again := readGiveUpSide(t, st, id)
			if again.Row != after.Row || again.Returns != after.Returns || again.Candles != after.Candles || !again.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatalf("a cycle after the give-up changed the call: %+v -> %+v", after, again)
			}
		})
	}
}

// A call whose quote asset's whole-history USD pool search is still running
// (errUSDSearchPending) is never given up, however many attempts it has, and
// those retries leave attempts as they were.
func TestTrackerGiveUpNotWhileUSDSearchPending(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		quote = "0x2222222222222222222222222222222222222222" // QT: no feed, no USD pool
		token = "0x4444444444444444444444444444444444444444"
		pool  = "0x00000000000000000000000000000000000000c3"
	)
	setHistBounds(t, 1, 256) // one transfer per sweep: QT's history takes several sweeps
	f := ltChain(t, 40*ltDay)
	f.addToken(quote, 18, "QT")
	f.ltPool(token, pool, quote)
	c := rugCall{entry: time.Now().Add(-33 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	f.ltTrade(token, pool, c.at(-time.Minute), 1e-3)
	f.ltTrade(token, pool, c.at(20*time.Hour), 2e-3)
	f.mu.Lock()
	for i := range 8 { // QT moving between wallets, one transfer per block
		f.transfer(quote, ltBuyer, fmt.Sprintf("0x%040x", 0xd2+i), c.at(-time.Duration(10+i)*time.Hour), fmt.Sprintf("0xq%d", i))
	}
	f.mu.Unlock()
	t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", "")
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed)
	s.onChannelPost(postAt(1, c.entry, token))
	id := *(<-s.queue).CallID

	for run, preset := range []int{-1, 20} {
		makeDue(t, st, id, preset)
		want, _ := st.GetTracking(ctx, id)
		s.trackDue(ctx, 50)
		tr, _ := st.GetTracking(ctx, id)
		if tr.Status != TrackError || tr.Error == nil || !strings.Contains(*tr.Error, "USD pool search not finished") {
			t.Fatalf("run %d: got status %s error %s, want error: USD pool search not finished", run+1, tr.Status, strOrNil(tr.Error))
		}
		if tr.Attempts != want.Attempts {
			t.Fatalf("run %d: got attempts %d, want %d (unchanged by a pending USD pool search)", run+1, tr.Attempts, want.Attempts)
		}
		if retry := time.Until(tr.NextCheckAt); retry > usdSearchRetry || retry < usdSearchRetry-time.Minute {
			t.Fatalf("run %d: next check in %s, want about %s", run+1, retry, usdSearchRetry)
		}
	}
}

// The GeckoTerminal tracker follows the same rule: a run that finds the pool
// (progress) stays in error at 10 attempts; the next one, failing on the
// hourly candles again with nothing new, gives up.
func TestTrackerGiveUpGecko(t *testing.T) {
	t.Setenv("SCOUT_TRACK_MAX_ATTEMPTS", "") // the default, 10
	s, st, f := trackerFixture(t)
	ctx := context.Background()
	token := "0x5555555555555555555555555555555555555555"
	entry := time.Now().UTC().Add(-35 * ltDay)
	addToken(f, token, "0xpoolold", entry, 25000)
	f.mu.Lock()
	f.ohlcvStatus = http.StatusBadRequest
	f.mu.Unlock()
	logs := captureLog(t)
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID

	makeDue(t, st, id, 9)
	s.trackDue(ctx, 50)
	tr, _ := st.GetTracking(ctx, id)
	if tr.Status != TrackError || tr.Attempts != 10 || tr.PoolAddress == nil || tr.Error == nil || !strings.HasPrefix(*tr.Error, "hourly candles") {
		t.Fatalf("run 1: got status %s attempts %d pool %s error %s, want error on the hourly candles, 10 attempts, the pool found",
			tr.Status, tr.Attempts, strOrNil(tr.PoolAddress), strOrNil(tr.Error))
	}
	makeDue(t, st, id, -1)
	s.trackDue(ctx, 50)
	tr, _ = st.GetTracking(ctx, id)
	if tr.Status != TrackGaveUp || tr.Attempts != 11 || tr.Error == nil || !strings.HasPrefix(*tr.Error, "gave up after 11 attempts, past the deadline ") ||
		!strings.Contains(*tr.Error, ": hourly candles") {
		t.Fatalf("run 2: got status %s attempts %d error %s, want gave_up after 11 attempts on the hourly candles", tr.Status, tr.Attempts, strOrNil(tr.Error))
	}
	if n := strings.Count(logs.String(), fmt.Sprintf("tracking call %d (%s): gave up after 11 attempts", id, token)); n != 1 {
		t.Fatalf("got %d give-up log lines for call %d, want 1:\n%s", n, id, logs.String())
	}
	if dueIDs(t, st)[id] {
		t.Fatalf("DueTracking: call %d (gave_up) still handed out", id)
	}
}
