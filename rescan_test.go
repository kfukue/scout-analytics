package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestLoadRescanConfig(t *testing.T) {
	keys := []string{"SCOUT_RESCAN", "SCOUT_RESCAN_MAX_AGE", "SCOUT_RESCAN_MAX_PER_DAY", "SCOUT_RESCAN_GAP", "SCOUT_RESCAN_IDLE", "SCOUT_RESCAN_STATUSES"}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    rescanConfig
		wantErr bool
	}{
		{name: "defaults", want: rescanConfig{MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute, Idle: 5 * time.Minute,
			Statuses: []string{"backfill", "duplicate", "failed", "scanned"}}},
		{name: "on with overrides", env: map[string]string{"SCOUT_RESCAN": "ON", "SCOUT_RESCAN_MAX_AGE": "168h", "SCOUT_RESCAN_MAX_PER_DAY": "20",
			"SCOUT_RESCAN_GAP": "15m", "SCOUT_RESCAN_IDLE": "0s", "SCOUT_RESCAN_STATUSES": "failed, scanned,failed"},
			want: rescanConfig{Enabled: true, MaxAge: 168 * time.Hour, MaxPerDay: 20, Gap: 15 * time.Minute, Statuses: []string{"failed", "scanned"}}},
		{name: "bad switch", env: map[string]string{"SCOUT_RESCAN": "yes"}, wantErr: true},
		{name: "queued not allowed", env: map[string]string{"SCOUT_RESCAN_STATUSES": "failed,queued"}, wantErr: true},
		{name: "dropped not allowed", env: map[string]string{"SCOUT_RESCAN_STATUSES": "dropped"}, wantErr: true},
		{name: "update not allowed", env: map[string]string{"SCOUT_RESCAN_STATUSES": "update"}, wantErr: true},
		{name: "empty statuses", env: map[string]string{"SCOUT_RESCAN_STATUSES": " , "}, wantErr: true},
		{name: "cap zero", env: map[string]string{"SCOUT_RESCAN_MAX_PER_DAY": "0"}, wantErr: true},
		{name: "cap not a number", env: map[string]string{"SCOUT_RESCAN_MAX_PER_DAY": "lots"}, wantErr: true},
		{name: "bad gap", env: map[string]string{"SCOUT_RESCAN_GAP": "10"}, wantErr: true},
		{name: "negative idle", env: map[string]string{"SCOUT_RESCAN_IDLE": "-1m"}, wantErr: true},
		{name: "max age under an hour", env: map[string]string{"SCOUT_RESCAN_MAX_AGE": "30m"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, tc.env[k])
			}
			got, err := loadRescanConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadRescanConfig() with %v: got %+v, want an error", tc.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadRescanConfig() with %v: %v", tc.env, err)
			}
			if got.Enabled != tc.want.Enabled || got.MaxAge != tc.want.MaxAge || got.MaxPerDay != tc.want.MaxPerDay ||
				got.Gap != tc.want.Gap || got.Idle != tc.want.Idle || strings.Join(got.Statuses, ",") != strings.Join(tc.want.Statuses, ",") {
				t.Errorf("loadRescanConfig() with %v: got %+v, want %+v", tc.env, got, tc.want)
			}
		})
	}
}

func TestRescanDecide(t *testing.T) {
	cfg := rescanConfig{Enabled: true, MaxAge: 720 * time.Hour, MaxPerDay: 3, Gap: 10 * time.Minute, Idle: 5 * time.Minute}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	agoP := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	idle := ago(time.Hour)
	for _, tc := range []struct {
		name string
		in   rescanInputs
		want string
	}{
		{"live calls queued", rescanInputs{Now: now, QueueLen: 1, LastLive: idle}, rescanBusy},
		{"queued beats pause", rescanInputs{Now: now, QueueLen: 2, LastLive: idle, PausedUntil: now.Add(time.Hour)}, rescanBusy},
		{"paused", rescanInputs{Now: now, LastLive: idle, PausedUntil: now.Add(time.Second)}, rescanPaused},
		{"pause just over", rescanInputs{Now: now, LastLive: idle, PausedUntil: now}, rescanNeedStats},
		{"live call 4m59s ago", rescanInputs{Now: now, LastLive: ago(5*time.Minute - time.Second)}, rescanNotIdle},
		{"live call 5m ago", rescanInputs{Now: now, LastLive: ago(5 * time.Minute)}, rescanNeedStats},
		{"worker just started", rescanInputs{Now: now, LastLive: now}, rescanNotIdle},
		{"no candidates lately", rescanInputs{Now: now, LastLive: idle, NoCandidateUntil: now.Add(time.Minute)}, rescanNoCandidates},
		{"needs the counts", rescanInputs{Now: now, LastLive: idle}, rescanNeedStats},
		{"first rescan ever", rescanInputs{Now: now, LastLive: idle, StatsKnown: true}, rescanGo},
		{"cap reached", rescanInputs{Now: now, LastLive: idle, StatsKnown: true, Today: 3, LastRescan: agoP(time.Hour)}, rescanCapReached},
		{"under the cap", rescanInputs{Now: now, LastLive: idle, StatsKnown: true, Today: 2, LastRescan: agoP(time.Hour)}, rescanGo},
		{"gap not over", rescanInputs{Now: now, LastLive: idle, StatsKnown: true, Today: 1, LastRescan: agoP(9 * time.Minute)}, rescanGapWait},
		{"gap just over", rescanInputs{Now: now, LastLive: idle, StatsKnown: true, Today: 1, LastRescan: agoP(10 * time.Minute)}, rescanGo},
		{"cap before gap", rescanInputs{Now: now, LastLive: idle, StatsKnown: true, Today: 3, LastRescan: agoP(time.Minute)}, rescanCapReached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rescanDecide(cfg, tc.in); got != tc.want {
				t.Errorf("rescanDecide(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRescanSummaryAndPerDay(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cfg := rescanConfig{MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute, Idle: 5 * time.Minute,
		Statuses: []string{"backfill", "duplicate", "failed", "scanned"}}
	day := 24 * time.Hour
	pool := []RescanPoolRow{
		{CallID: 1, MessageDate: now.Add(-2 * time.Hour), Status: "failed"},
		{CallID: 2, MessageDate: now.Add(-3 * day), Status: "scanned"},
		{CallID: 3, MessageDate: now.Add(-10 * day), Status: "backfill"},
		{CallID: 4, MessageDate: now.Add(-29 * day), Status: "backfill"},
		{CallID: 5, MessageDate: now.Add(-31 * day), Status: "backfill", Rugged: true}, // too old wins
		{CallID: 6, MessageDate: now.Add(-1 * day), Status: "failed", Rugged: true, Wiped: true},
		{CallID: 7, MessageDate: now.Add(-1 * day), Status: "failed", Wiped: true, FailedRescans: 2},
		{CallID: 8, MessageDate: now.Add(-1 * day), Status: "duplicate", FailedRescans: 2},
		{CallID: 9, MessageDate: now.Add(-1 * day), Status: "duplicate", FailedRescans: 1},
	}
	s := summarizeRescan(cfg, now, pool)
	want := rescanSummary{Pool: 9, TooOld: 1, Rugged: 1, Wiped: 1, FailedTwice: 1, Candidates: 5,
		ByStatus: map[string]int{"failed": 1, "scanned": 1, "backfill": 2, "duplicate": 1}, ByAge: [4]int{1, 2, 2, 0}}
	if s.Pool != want.Pool || s.TooOld != want.TooOld || s.Rugged != want.Rugged || s.Wiped != want.Wiped ||
		s.FailedTwice != want.FailedTwice || s.Candidates != want.Candidates || s.ByAge != want.ByAge {
		t.Errorf("summarizeRescan: got %+v, want %+v", s, want)
	}
	for k, v := range want.ByStatus {
		if s.ByStatus[k] != v {
			t.Errorf("summarizeRescan: by status %s = %d, want %d", k, s.ByStatus[k], v)
		}
	}

	for _, tc := range []struct {
		cap  int
		gap  time.Duration
		want int
	}{{100, 10 * time.Minute, 100}, {500, 10 * time.Minute, 144}, {50, 0, 50}, {100, 30 * time.Minute, 48}} {
		if got := (rescanConfig{MaxPerDay: tc.cap, Gap: tc.gap}).perDay(); got != tc.want {
			t.Errorf("perDay(cap %d, gap %s) = %d, want %d", tc.cap, tc.gap, got, tc.want)
		}
	}

	var out bytes.Buffer
	writeRescanSummary(&out, cfg, s)
	for _, line := range []string{
		"SCOUT_RESCAN=off",
		"first calls without a Perceptor report, status backfill,duplicate,failed,scanned: 9",
		"excluded (each counted once, in this order): too old 1, rugged 1, latest return <= -99% 1, failed twice 1",
		"candidates: 5",
		"by status: backfill 2, duplicate 1, failed 1, scanned 1",
		"by age: <1d 1, 1-7d 2, 7-30d 2\n",
		"at most 100 a day (cap 100, gap 10m0s) -> about 1 day(s) for these 5",
		"runs inside the listener once SCOUT_RESCAN=on",
		"candidates in the order the lane takes them (newest first)",
		"  1\t2026-10-08T10:00:00Z\t2h\tfailed\t\n",
		"  9\t2026-10-07T12:00:00Z\t24h\tduplicate\t\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("summary lacks %q:\n%s", line, out.String())
		}
	}
	var listed []int
	for _, r := range s.List {
		listed = append(listed, r.CallID)
	}
	if want := []int{1, 2, 3, 4, 9}; !slices.Equal(listed, want) {
		t.Errorf("summarizeRescan list = %v, want %v (the pool's order, candidates only)", listed, want)
	}
}

func TestRescanDetailsAndAge(t *testing.T) {
	base := replyDetails(nil, 1, nil)
	got := rescanDetails(base, 12*24*time.Hour+time.Hour)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("rescanDetails(%s): %v", base, err)
	}
	if m["rescan"] != true || m["call_age_s"] != float64(12*86400+3600) || m["attempts"] != float64(1) {
		t.Errorf("rescanDetails(%s) = %s, want rescan true, call_age_s %d and attempts kept", base, got, 12*86400+3600)
	}
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", 7 * time.Hour: "7h", 47 * time.Hour: "47h", 12 * 24 * time.Hour: "12d"} {
		if got := rescanAgeText(d); got != want {
			t.Errorf("rescanAgeText(%s) = %q, want %q", d, got, want)
		}
	}
}

// fakeRescanStore records what the lane asks of the database.
type fakeRescanStore struct {
	mu        sync.Mutex
	today     int
	last      *time.Time
	cand      *RescanCandidate
	statsN    int
	candN     int
	inserted  []*ScoutInvestigation
	candSince time.Time
	insertErr error // when set, InsertScoutInvestigation fails (nothing stored)
}

func (f *fakeRescanStore) RescanStats(ctx context.Context, since time.Time) (int, *time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsN++
	return f.today, f.last, nil
}

func (f *fakeRescanStore) NextRescanCandidate(ctx context.Context, statuses []string, since time.Time, maxFailed int) (*RescanCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candN++
	f.candSince = since
	return f.cand, nil
}

func (f *fakeRescanStore) InsertScoutInvestigation(ctx context.Context, r *ScoutInvestigation) (*int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.inserted = append(f.inserted, r)
	id := len(f.inserted)
	return &id, nil
}

// testPerceptorSpec is Perceptor's spec with short waits and no pacing.
func testPerceptorSpec() ToolSpec {
	return ToolSpec{Code: "perceptor", Name: "Perceptor", Bot: "perceptor0xBot", Command: "/scan {ca}", Parser: parserPerceptor,
		Gate: true, Attach: true, Timeout: 2 * time.Second, Settle: 20 * time.Millisecond, MaxWait: 5 * time.Second,
		MaxRetries: 3, RateLimitRe: defaultRateLimitRe, ProgressRe: defaultProgressRe, MaxRateWait: time.Minute,
		DoneRe: regexp.MustCompile(`(?i)perceptor\.info/|red flags?|caution|no red flags`)}
}

// replyingSend is a fake send: the bot "answers" text at once (or the send
// fails with err).
func replyingSend(text string, err error, sent *[]string) func(ctx context.Context, r *toolRunner, cmd string) error {
	id := 0
	return func(ctx context.Context, r *toolRunner, cmd string) error {
		*sent = append(*sent, cmd)
		if err != nil {
			return err
		}
		id++
		r.onMessage(&tg.Message{ID: id, Date: int(time.Now().Unix()), Message: text})
		return nil
	}
}

func TestRescanLaneStep(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	callAt := now.Add(-12 * 24 * time.Hour)
	ca := "0x1111111111111111111111111111111111111111"
	cfg := rescanConfig{Enabled: true, MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute, Idle: 5 * time.Minute,
		Statuses: []string{"failed"}}
	newLane := func(reply string, sendErr error, sent *[]string) *rescanLane {
		l := &rescanLane{cfg: cfg, runner: newToolRunner(testPerceptorSpec()), toolID: 7, now: func() time.Time { return now },
			send: replyingSend(reply, sendErr, sent),
			judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
				return verdict{Level: levelClean, Label: "No red flags"}
			}, errLog: logLimiter{every: time.Minute}}
		l.noteLive(now.Add(-time.Hour))
		return l
	}
	empty := func() int { return 0 }

	t.Run("completed: one rescan row, nothing else", func(t *testing.T) {
		var sent []string
		l := newLane("✅ No red flags found", nil, &sent)
		st := &fakeRescanStore{today: 4, cand: &RescanCandidate{CallID: 42, ContractAddress: ca, MessageDate: callAt, Status: "failed"}}
		if !l.step(context.Background(), st, empty) {
			t.Fatal("step() = false, want a rescan")
		}
		if len(sent) != 1 || sent[0] != "/scan "+ca {
			t.Errorf("sent %q, want one /scan %s", sent, ca)
		}
		if want := now.Add(-cfg.MaxAge); !st.candSince.Equal(want) {
			t.Errorf("candidate query since %s, want %s", st.candSince, want)
		}
		if len(st.inserted) != 1 {
			t.Fatalf("inserted %d rows, want 1", len(st.inserted))
		}
		row := st.inserted[0]
		if row.ScanKind != ScanKindRescan || row.CallID == nil || *row.CallID != 42 || row.ToolID != 7 ||
			row.Status != investigationCompleted || row.VerdictLevel != levelClean || row.ContractAddress != ca {
			t.Errorf("row = %+v, want a completed clean rescan of call 42 by tool 7", row)
		}
		var d map[string]any
		if err := json.Unmarshal(row.Details, &d); err != nil || d["rescan"] != true || d["call_age_s"] != float64(12*86400) {
			t.Errorf("details %s (%v), want rescan true and call_age_s %d", row.Details, err, 12*86400)
		}
	})

	t.Run("busy queue: no database read, no scan", func(t *testing.T) {
		var sent []string
		l := newLane("x", nil, &sent)
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 1, ContractAddress: ca, MessageDate: callAt}}
		if l.step(context.Background(), st, func() int { return 1 }) || st.statsN+st.candN != 0 || len(sent) != 0 {
			t.Errorf("step with a queued live call: stats %d, candidate %d, sent %q; want nothing", st.statsN, st.candN, sent)
		}
	})

	t.Run("gap from the database", func(t *testing.T) {
		var sent []string
		l := newLane("x", nil, &sent)
		last := now.Add(-5 * time.Minute)
		st := &fakeRescanStore{today: 1, last: &last, cand: &RescanCandidate{CallID: 1, ContractAddress: ca, MessageDate: callAt}}
		if l.step(context.Background(), st, empty) || st.statsN != 1 || st.candN != 0 || len(sent) != 0 {
			t.Errorf("step 5m after a rescan: stats %d, candidate %d, sent %q; want only the stats read", st.statsN, st.candN, sent)
		}
	})

	t.Run("no candidate: not asked again for a while", func(t *testing.T) {
		var sent []string
		l := newLane("x", nil, &sent)
		st := &fakeRescanStore{}
		l.step(context.Background(), st, empty)
		l.step(context.Background(), st, empty)
		if st.candN != 1 || len(sent) != 0 {
			t.Errorf("two steps without candidates: candidate query run %d times, want 1", st.candN)
		}
	})

	t.Run("rate limited: row written, lane paused", func(t *testing.T) {
		var sent []string
		l := newLane("One scan every 2 minutes. You can scan again in 63 s", nil, &sent)
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		if !l.step(context.Background(), st, empty) {
			t.Fatal("step() = false, want a rescan")
		}
		if len(sent) != 1 {
			t.Errorf("sent %d requests, want 1 (no rate-limit retry)", len(sent))
		}
		if len(st.inserted) != 1 || st.inserted[0].Status != investigationRateLimited || st.inserted[0].ScanKind != ScanKindRescan {
			t.Fatalf("inserted %+v, want one rate_limited rescan row", st.inserted)
		}
		if got := l.pausedAt(); !got.Equal(now.Add(rescanPause)) {
			t.Errorf("paused until %s, want %s", got, now.Add(rescanPause))
		}
		if l.step(context.Background(), st, empty) || len(sent) != 1 {
			t.Errorf("step while paused scanned again")
		}
	})

	t.Run("FLOOD_WAIT: nothing written, lane paused", func(t *testing.T) {
		var sent []string
		l := newLane("x", tgerr.New(420, "FLOOD_WAIT_7200"), &sent)
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		l.step(context.Background(), st, empty)
		if len(st.inserted) != 0 {
			t.Errorf("inserted %+v after FLOOD_WAIT, want nothing", st.inserted)
		}
		if got, want := l.pausedAt(), now.Add(2*time.Hour); !got.Equal(want) {
			t.Errorf("paused until %s, want %s (Telegram's wait, longer than %s)", got, want, rescanPause)
		}
	})

	t.Run("stopped: nothing written", func(t *testing.T) {
		var sent []string
		l := newLane("x", nil, &sent)
		ctx, cancel := context.WithCancel(context.Background())
		l.send = func(ctx context.Context, r *toolRunner, cmd string) error {
			cancel() // the listener stops while the scan is in flight
			return nil
		}
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		l.step(ctx, st, empty)
		if len(st.inserted) != 0 {
			t.Errorf("inserted %+v after a stop, want nothing (a stop must not use up a try)", st.inserted)
		}
	})

	t.Run("stopped while judging: nothing written", func(t *testing.T) {
		var sent []string
		l := newLane("✅ No red flags found", nil, &sent)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		judged := 0
		l.judge = func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			judged++
			cancel() // the listener stops while the report page is read
			return verdict{Level: levelClean, Label: "No red flags"}
		}
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		l.step(ctx, st, empty)
		if judged != 1 {
			t.Fatalf("judge called %d times, want 1 (the report completed before the stop)", judged)
		}
		if len(st.inserted) != 0 {
			t.Errorf("inserted %+v after a stop during judge, want nothing", st.inserted)
		}
	})

	t.Run("insert fails: the gap still holds", func(t *testing.T) {
		var sent []string
		clock := now
		l := newLane("✅ No red flags found", nil, &sent)
		l.now = func() time.Time { return clock }
		// the database is down for writes: no row, so RescanStats sees no rescan
		st := &fakeRescanStore{insertErr: errors.New("connection refused"),
			cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		if !l.step(context.Background(), st, empty) || len(sent) != 1 {
			t.Fatalf("first step: sent %q, want one rescan", sent)
		}
		for _, tc := range []struct {
			after   time.Duration
			wantRun bool
		}{
			{cfg.Gap - time.Second, false},
			{cfg.Gap, true},
		} {
			clock = now.Add(tc.after)
			candBefore, sentBefore := st.candN, len(sent)
			ran := l.step(context.Background(), st, empty)
			if ran != tc.wantRun || (len(sent) > sentBefore) != tc.wantRun || (st.candN > candBefore) != tc.wantRun {
				t.Errorf("step %s after a rescan whose row was not stored (gap %s): ran %t, candidate query %t, sent %t; want %t",
					tc.after, cfg.Gap, ran, st.candN > candBefore, len(sent) > sentBefore, tc.wantRun)
			}
		}
	})

	t.Run("gap: the database's newer rescan wins over the lane's", func(t *testing.T) {
		var sent []string
		clock := now
		l := newLane("✅ No red flags found", nil, &sent)
		l.now = func() time.Time { return clock }
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		l.step(context.Background(), st, empty) // the lane's own rescan at now
		clock = now.Add(cfg.Gap + time.Minute)
		newer := now.Add(cfg.Gap) // another process's rescan, 1m ago
		st.last = &newer
		if l.step(context.Background(), st, empty) || len(sent) != 1 {
			t.Errorf("step 1m after a newer rescan in the database: sent %d requests, want 1 (gap %s)", len(sent), cfg.Gap)
		}
	})

	t.Run("send error: failed row", func(t *testing.T) {
		var sent []string
		l := newLane("x", errors.New("network down"), &sent)
		st := &fakeRescanStore{cand: &RescanCandidate{CallID: 9, ContractAddress: ca, MessageDate: callAt}}
		l.step(context.Background(), st, empty)
		if len(st.inserted) != 1 || st.inserted[0].Status != investigationFailed || st.inserted[0].Error == nil {
			t.Errorf("inserted %+v, want one failed rescan row with its error", st.inserted)
		}
		if !l.pausedAt().IsZero() {
			t.Errorf("paused after a plain send error, want no pause")
		}
	})
}

// The worker runs live calls first and, with the lane off, never looks at it.
func TestWorkerLaneOffRunsLiveOnly(t *testing.T) {
	s := &scanner{cfg: &config{ScanGap: time.Millisecond}, queue: make(chan job, 1), pending: map[int]bool{}}
	if s.newRescanLane(rescanConfig{}) != nil {
		t.Fatal("newRescanLane(off) != nil")
	}
	var nilLane *rescanLane
	nilLane.noteLive(time.Now()) // must not panic: the worker calls it with the lane off
}
