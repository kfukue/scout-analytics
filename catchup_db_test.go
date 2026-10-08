package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// restartScanner is a new listener process on the same database and state
// directory as s (seen_cas.json and the cursor file carry over).
func restartScanner(t *testing.T, s *scanner) *scanner {
	t.Helper()
	s2 := newScanner(s.cfg)
	s2.db = s.db
	s2.sourceChannelID = s.sourceChannelID
	return s2
}

func TestMaxScoutCallMessageIDAndPostCAs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := st.MaxScoutCallMessageID(ctx, 777); id != 0 || ok || err != nil {
		t.Fatalf("empty table: got %d, %v, %v; want 0, false, nil", id, ok, err)
	}
	add := func(channel int64, msg int, ca string) {
		t.Helper()
		if _, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: channel, ChannelUsername: "c", MessageID: msg,
			MessageDate: time.Now(), ContractAddress: ca, Chain: "evm", Status: CallStatusQueued}); err != nil {
			t.Fatal(err)
		}
	}
	add(777, 10, caN(1))
	add(777, 12, caN(2))
	add(777, 12, caN(3))
	add(888, 99, caN(4)) // another channel
	if id, ok, err := st.MaxScoutCallMessageID(ctx, 777); id != 12 || !ok || err != nil {
		t.Fatalf("channel 777: got %d, %v, %v; want 12, true, nil", id, ok, err)
	}
	got, err := st.SelectScoutCallPostCAs(ctx, 777, 10)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]ScoutCallPostCA{{12, caN(2)}, {12, caN(3)}}) {
		t.Fatalf("SelectScoutCallPostCAs(777, after 10) = %v, want post 12 with %s and %s", got, caN(2), caN(3))
	}
}

// After a restart without a cursor file (the first deploy of this feature) the
// listener resumes after the newest post in the database: the newest missed
// posts are handled live, older ones are stored like -backfill, update posts
// stay recorded only, and a token already investigated is a duplicate.
func TestCatchUpResumesFromDatabase(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC().Truncate(time.Second)
	a, b, c, d := caN(0xa), caN(0xb), caN(0xc), caN(0xd)

	// before the restart: a live call of a, and b imported by -backfill
	s.onChannelPost(postAt(300, now.Add(-40*time.Hour), a))
	<-s.queue
	var bs backfillStats
	s.backfillMessage(postAt(301, now.Add(-35*time.Hour), b), &bs)
	if _, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 888, ChannelUsername: "other", MessageID: 5000,
		MessageDate: now, ContractAddress: caN(0xe), Chain: "evm", Status: CallStatusQueued}); err != nil {
		t.Fatal(err)
	}

	s2 := restartScanner(t, s)
	cursor, from, err := s2.resumeCursor(ctx)
	if cursor != 301 || !strings.Contains(from, "database") || err != nil {
		t.Fatalf("resumeCursor = %d, %q, %v; want 301 from the database", cursor, from, err)
	}
	channel := []*tg.Message{
		postAt(299, now.Add(-41*time.Hour), caN(0x299)),
		postAt(300, now.Add(-40*time.Hour), a),
		postAt(301, now.Add(-35*time.Hour), b),
		postAt(302, now.Add(-30*time.Hour), c),       // older than 24h: stored only
		updatePostAt(303, now.Add(-20*time.Hour), a), // update: recorded only
		postAt(304, now.Add(-10*time.Hour), d),       // live: investigated
		postAt(305, now.Add(-5*time.Hour), a),        // live, already investigated: duplicate
		postAt(306, now.Add(-time.Hour), b),          // live: b was only backfilled, so investigated
	}
	newest, err := s2.catchUp(ctx, cursor, (&fakeHistory{posts: channel}).page)
	if err != nil || newest != 306 {
		t.Fatalf("catchUp(301) = %d, %v; want 306, nil", newest, err)
	}
	if want := "catch-up: 5 post(s) since post 301 (4 handled live, 1 stored only)"; !strings.Contains(buf.String(), want) {
		t.Fatalf("log is missing %q:\n%s", want, buf.String())
	}
	var queued []int
	for len(s2.queue) > 0 {
		queued = append(queued, (<-s2.queue).SourceMsg)
	}
	if fmt.Sprint(queued) != "[304 306]" {
		t.Fatalf("queued posts %v, want [304 306]\n%s", queued, buf.String())
	}
	for _, tc := range []struct {
		msg        int
		ca, status string
		tracked    bool
	}{
		{302, c, CallStatusBackfill, true},
		{303, a, CallStatusUpdate, false},
		{304, d, CallStatusQueued, true},
		{305, a, CallStatusDuplicate, true},
		{306, b, CallStatusQueued, true},
	} {
		row := callByMsg(t, st, tc.ca, tc.msg)
		tr, err := st.GetTracking(ctx, *row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != tc.status || (tr != nil) != tc.tracked {
			t.Errorf("post %d: status %q, tracking row %v; want %q, %v", tc.msg, row.Status, tr != nil, tc.status, tc.tracked)
		}
		if tc.msg == 302 && (tr == nil || tr.Priority != 1) {
			t.Errorf("stored-only post 302: tracking %+v, want priority 1 like -backfill", tr)
		}
	}
	if !s2.seen.markNew(c) {
		t.Error("the stored-only post marked its token as investigated")
	}
	if id, ok, err := readPollCursor(s2.cursorPath(), 777); id != 306 || !ok || err != nil {
		t.Fatalf("cursor file: %d, %v, %v; want 306, true, nil", id, ok, err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls WHERE channel_id = 777 AND message_id <= 301`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows up to the cursor: %d (%v), want 2: posts at or below the cursor are not read again", rows, err)
	}
}

// The cursor file is behind the database (live updates recorded posts that
// polling had not reached when the listener stopped): the catch-up starts at
// the file's mark, and the posts already in the database are skipped, not
// handled again, whatever their status.
func TestCatchUpSkipsPostsAlreadyInDatabase(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC().Truncate(time.Second)
	e, f, g, h := caN(0x1e), caN(0x1f), caN(0x20), caN(0x21)

	s.saveCursor(400)
	var bs backfillStats
	s.backfillMessage(postAt(401, now.Add(-3*time.Hour), e), &bs) // stored, not investigated
	s.onChannelPost(postAt(402, now.Add(-2*time.Hour), f))        // live update; its job died with the process
	<-s.queue

	s2 := restartScanner(t, s)
	cursor, from, err := s2.resumeCursor(ctx)
	if cursor != 400 || !strings.Contains(from, "poll cursor") || err != nil {
		t.Fatalf("resumeCursor = %d, %q, %v; want 400 from the poll cursor", cursor, from, err)
	}
	channel := []*tg.Message{
		postAt(400, now.Add(-4*time.Hour), h),
		postAt(401, now.Add(-3*time.Hour), e),
		postAt(402, now.Add(-2*time.Hour), f),
		postAt(403, now.Add(-time.Hour), g),
	}
	if newest, err := s2.catchUp(ctx, cursor, (&fakeHistory{posts: channel}).page); err != nil || newest != 403 {
		t.Fatalf("catchUp(400) = %d, %v; want 403, nil", newest, err)
	}
	if want := "catch-up: 1 post(s) since post 400 (1 handled live, 0 stored only; 2 already recorded, skipped)"; !strings.Contains(buf.String(), want) {
		t.Fatalf("log is missing %q:\n%s", want, buf.String())
	}
	if n := len(s2.queue); n != 1 {
		t.Fatalf("queued %d job(s), want 1 (post 403)", n)
	}
	if j := <-s2.queue; j.SourceMsg != 403 {
		t.Fatalf("queued post %d, want 403", j.SourceMsg)
	}
	if row := callByMsg(t, st, e, 401); row.Status != CallStatusBackfill {
		t.Fatalf("post 401: status %q, want %q (not investigated again)", row.Status, CallStatusBackfill)
	}
	// polling sees them again: still nothing
	s2.onChannelPost(channel[1])
	s2.onChannelPost(channel[2])
	if n := len(s2.queue); n != 0 {
		t.Fatalf("posts already in the database queued %d job(s) when polled again, want 0", n)
	}
}

// setStatusOf sets the status of the call of ca in post msg.
func setStatusOf(t *testing.T, st *ScoutStore, ca string, msg int, status string) int {
	t.Helper()
	row := callByMsg(t, st, ca, msg)
	if err := st.UpdateScoutCallStatus(context.Background(), *row.ID, status); err != nil {
		t.Fatal(err)
	}
	return *row.ID
}

func addCompletedInvestigation(t *testing.T, st *ScoutStore, callID int, ca string) {
	t.Helper()
	tool := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", "perceptor", true)
	done := time.Now().UTC()
	if _, err := st.InsertScoutInvestigation(context.Background(), &ScoutInvestigation{CallID: &callID, ToolID: tool, ContractAddress: ca,
		RequestText: "/scan " + ca, RequestedAt: done, CompletedAt: &done, Status: investigationCompleted, VerdictLevel: "clean"}); err != nil {
		t.Fatal(err)
	}
}

// After a restart, the calls left queued (or dropped, queue full) without a
// report are queued again, oldest first, although seen_cas.json holds their
// tokens; too old ones are stored only and their token freed; a call whose
// token another call investigated is a duplicate; calls with a report, other
// statuses and other channels are left alone. A second requeue (reconnect)
// and a poll of the same post queue nothing more.
func TestRequeueAfterRestart(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC().Truncate(time.Second)
	a, b, c, d, e, f := caN(0x31), caN(0x32), caN(0x33), caN(0x34), caN(0x35), caN(0x36)

	// before the restart
	s.onChannelPost(postAt(490, now.Add(-50*time.Hour), f)) // f investigated
	s.onChannelPost(postAt(500, now.Add(-30*time.Hour), a)) // queued, older than 24h
	s.onChannelPost(postAt(501, now.Add(-2*time.Hour), b))  // queued
	s.onChannelPost(postAt(502, now.Add(-time.Hour), c))    // dropped: the queue was full
	s.onChannelPost(postAt(503, now.Add(-50*time.Minute), d))
	s.onChannelPost(postAt(504, now.Add(-40*time.Minute), e))
	drainQueue(s) // the process stops with all of them still in its queue
	addCompletedInvestigation(t, st, setStatusOf(t, st, f, 490, CallStatusScanned), f)
	setStatusOf(t, st, c, 502, CallStatusDropped)
	addCompletedInvestigation(t, st, *callByMsg(t, st, d, 503).ID, d) // report stored, status not updated yet
	setStatusOf(t, st, e, 504, CallStatusScanned)
	// f queued again by a post recorded while seen_cas.json was lost
	if _, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: 505,
		MessageDate: now.Add(-30 * time.Minute), MessageText: samplePost, ContractAddress: f, Chain: "evm", Status: CallStatusQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 888, ChannelUsername: "other", MessageID: 9000,
		MessageDate: now, ContractAddress: caN(0x37), Chain: "evm", Status: CallStatusQueued}); err != nil {
		t.Fatal(err)
	}

	s2 := restartScanner(t, s)
	if err := s2.requeue(ctx); err != nil {
		t.Fatal(err)
	}
	want := "requeue: 4 queued call(s) from before the restart (2 scanned now, 1 stored only; 1 duplicate(s), the token was investigated by another call)"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("log is missing %q:\n%s", want, buf.String())
	}
	// a reconnect requeues again: nothing more, no second log line
	if err := s2.requeue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "requeue:"); n != 1 {
		t.Fatalf("%d requeue line(s) after two requeues, want 1:\n%s", n, buf.String())
	}
	// polling or an edit of a requeued, stored-only or duplicate post:
	// nothing more either
	s2.onChannelPost(postAt(501, now.Add(-2*time.Hour), b))
	s2.onChannelPost(postAt(500, now.Add(-30*time.Hour), a))
	s2.onChannelPost(postAt(505, now.Add(-30*time.Minute), f))
	if n := len(s2.queue); n != 2 {
		t.Fatalf("queue holds %d job(s), want 2 (posts 501 and 502)", n)
	}
	j1, j2 := <-s2.queue, <-s2.queue
	if j1.SourceMsg != 501 || caKey(j1.CA) != caKey(b) || j2.SourceMsg != 502 || j1.Meta == nil || j1.SourceText != samplePost || j1.CallID == nil {
		t.Fatalf("queued post %d %s (meta %v, text %d chars, call %v) then post %d; want 501 %s with its parsed post and call id, then 502",
			j1.SourceMsg, j1.CA, j1.Meta != nil, len(j1.SourceText), j1.CallID, j2.SourceMsg, b)
	}
	for _, tc := range []struct {
		msg        int
		ca, status string
	}{
		{490, f, CallStatusScanned},
		{500, a, CallStatusBackfill},
		{501, b, CallStatusQueued},
		{502, c, CallStatusQueued},
		{503, d, CallStatusQueued},
		{504, e, CallStatusScanned},
		{505, f, CallStatusDuplicate},
	} {
		if row := callByMsg(t, st, tc.ca, tc.msg); row.Status != tc.status {
			t.Errorf("post %d: status %q, want %q", tc.msg, row.Status, tc.status)
		}
	}
	var other string
	if err := st.Pool.QueryRow(ctx, `SELECT status FROM scout_calls WHERE channel_id = 888`).Scan(&other); err != nil || other != CallStatusQueued {
		t.Errorf("other channel's call: status %q (%v), want %q", other, err, CallStatusQueued)
	}
	if !s2.seen.markNew(a) {
		t.Error("the stored-only call kept its token marked as investigated")
	}
	if s2.seen.markNew(b) {
		t.Error("the requeued call's token was not marked as investigated")
	}

	// the worker finishes post 501; post 502 is still being processed
	s2.setCallStatus(j1.CallID, CallStatusScanned)
	s2.jobDone(j1)
	if err := s2.requeue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s2.queue); n != 0 {
		t.Fatalf("requeue while post 502 is being processed queued %d job(s), want 0", n)
	}
}

// The catch-up's cap and age rules, and a full queue.
func TestRequeueCapAndQueueFull(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 2, 0, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC()
	for id := 600; id <= 603; id++ {
		s.onChannelPost(postAt(id, now.Add(time.Duration(id-604)*time.Minute), caN(id)))
	}
	drainQueue(s)

	s2 := restartScanner(t, s)
	s2.queue = make(chan job, 1)
	if err := s2.requeue(ctx); err != nil {
		t.Fatal(err)
	}
	want := "requeue: 4 queued call(s) from before the restart (1 scanned now, 2 stored only; 1 dropped, the queue is full)"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("log is missing %q:\n%s", want, buf.String())
	}
	for id, status := range map[int]string{600: CallStatusBackfill, 601: CallStatusBackfill, 602: CallStatusQueued, 603: CallStatusDropped} {
		if row := callByMsg(t, st, caN(id), id); row.Status != status {
			t.Errorf("post %d: status %q, want %q", id, row.Status, status)
		}
	}
	j := <-s2.queue
	if j.SourceMsg != 602 {
		t.Fatalf("queued post %d, want 602", j.SourceMsg)
	}
	s2.setCallStatus(j.CallID, CallStatusScanned)
	s2.jobDone(j)
	// room again: the dropped call is queued
	if err := s2.requeue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := drainQueue(s2); fmt.Sprint(got) != "[603]" {
		t.Fatalf("second requeue queued %v, want [603]", got)
	}
	if row := callByMsg(t, st, caN(603), 603); row.Status != CallStatusQueued {
		t.Fatalf("post 603 after the second requeue: status %q, want %q", row.Status, CallStatusQueued)
	}
}

// The start of polling after a restart: the calls queued before it go first,
// then the catch-up's live posts; the requeue after the catch-up adds nothing.
func TestPollerBeginRequeuesBeforeCatchUp(t *testing.T) {
	s, _, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC()
	channel := []*tg.Message{postAt(700, now.Add(-time.Hour), caN(0x700)), postAt(701, now.Add(-30*time.Minute), caN(0x701)), postAt(702, now, caN(0x702))}
	s.onChannelPost(channel[0])
	s.saveCursor(700)
	drainQueue(s)

	s2 := restartScanner(t, s)
	p := &poller{s: s2, src: &fakeSource{h: fakeHistory{posts: channel}}}
	if err := p.begin(ctx); err != nil || !p.started || p.cursor != 702 {
		t.Fatalf("begin: err %v, started %v, cursor %d; want nil, true, 702", err, p.started, p.cursor)
	}
	if got := drainQueue(s2); fmt.Sprint(got) != "[700 701 702]" {
		t.Fatalf("queued posts %v, want [700 701 702]\n%s", got, buf.String())
	}
	log := buf.String()
	rq, cu := strings.Index(log, "requeue: 1 queued call(s) from before the restart (1 scanned now, 0 stored only)"), strings.Index(log, "catch-up: 2 post(s) since post 700")
	if rq < 0 || cu < 0 || rq > cu || strings.Count(log, "requeue:") != 1 {
		t.Fatalf("want one requeue line before the catch-up line:\n%s", log)
	}
}

// insertQueuedCall records a call of ca in post msg with the given status
// straight into the database (seen_cas.json is not touched).
func insertQueuedCall(t *testing.T, st *ScoutStore, msg int, at time.Time, ca, status string) {
	t.Helper()
	if _, err := st.InsertScoutCall(context.Background(), &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: msg,
		MessageDate: at, MessageText: samplePost, ContractAddress: ca, Chain: "evm", Status: status}); err != nil {
		t.Fatal(err)
	}
}

// Calls left queued or dropped longer ago than the requeue window (72h with
// the defaults) are not touched: not queued, not rewritten, their tokens kept
// in seen_cas.json; they are counted in one log line per process.
func TestRequeueLeavesCallsOlderThanWindowAlone(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	buf := captureLog(t)
	now := time.Now().UTC().Truncate(time.Second)
	old1, old2, recent := caN(0x41), caN(0x42), caN(0x43)
	insertQueuedCall(t, st, 1000, now.Add(-200*time.Hour), old1, CallStatusDropped)
	insertQueuedCall(t, st, 1001, now.Add(-100*time.Hour), old2, CallStatusQueued)
	insertQueuedCall(t, st, 1002, now.Add(-2*time.Hour), recent, CallStatusQueued)
	s.seen.markNew(old1)
	s.seen.markNew(old2)

	s2 := restartScanner(t, s)
	for i := 0; i < 2; i++ { // the requeues before and after the catch-up
		if err := s2.requeue(ctx); err != nil {
			t.Fatalf("requeue %d: %v", i+1, err)
		}
	}
	if got := drainQueue(s2); fmt.Sprint(got) != "[1002]" {
		t.Fatalf("queued posts %v, want [1002] (the older ones are outside the 72h window)", got)
	}
	want := "requeue: 2 queued or dropped call(s) posted before "
	if n := strings.Count(buf.String(), want); n != 1 {
		t.Fatalf("%d line(s) with %q after two requeues, want 1:\n%s", n, want, buf.String())
	}
	if !strings.Contains(buf.String(), "(older than 72h0m0s) left as they are; DEPLOY.md 3.3") {
		t.Fatalf("the older-calls line does not give the window and the pointer:\n%s", buf.String())
	}
	for _, tc := range []struct {
		msg        int
		ca, status string
	}{
		{1000, old1, CallStatusDropped},
		{1001, old2, CallStatusQueued},
		{1002, recent, CallStatusQueued},
	} {
		if row := callByMsg(t, st, tc.ca, tc.msg); row.Status != tc.status {
			t.Errorf("post %d: status %q, want %q", tc.msg, row.Status, tc.status)
		}
	}
	if s2.seen.markNew(old1) || s2.seen.markNew(old2) {
		t.Error("a call outside the window had its token freed in seen_cas.json, want it kept")
	}
}

// Several calls of one token waiting at a restart (recorded while
// seen_cas.json was lost; the CAs differ only in case): the token counts
// once, as its newest call, and the others follow it. A token already queued
// in this process makes its waiting calls duplicates.
func TestRequeueSameTokenOnce(t *testing.T) {
	upper, lower := "0xAbCd"+strings.Repeat("0", 34)+"51", "0xabcd"+strings.Repeat("0", 34)+"51"
	tests := []struct {
		name         string
		maxAge       time.Duration
		upperAge     time.Duration // age of the token's first call (post 1100); 3h when 0
		queuedFirst  bool          // a new post of the token is queued in this process before the requeue
		wantQueued   string
		wantStatuses map[int]string
		wantSeen     bool // the token is still marked as investigated
		wantLog      string
	}{
		{name: "both live: one scan, the older call a duplicate", maxAge: 24 * time.Hour, wantQueued: "[1101 1102]",
			wantStatuses: map[int]string{1100: CallStatusDuplicate, 1101: CallStatusQueued, 1102: CallStatusQueued}, wantSeen: true,
			wantLog: "requeue: 3 queued call(s) from before the restart (2 scanned now, 0 stored only; 1 duplicate(s)"},
		{name: "first call too old, later one live: the later one is scanned", maxAge: 24 * time.Hour, upperAge: 30 * time.Hour, wantQueued: "[1101 1102]",
			wantStatuses: map[int]string{1100: CallStatusDuplicate, 1101: CallStatusQueued, 1102: CallStatusQueued}, wantSeen: true,
			wantLog: "requeue: 3 queued call(s) from before the restart (2 scanned now, 0 stored only; 1 duplicate(s)"},
		{name: "both too old: both stored only, the token freed", maxAge: time.Minute, wantQueued: "[]",
			wantStatuses: map[int]string{1100: CallStatusBackfill, 1101: CallStatusBackfill, 1102: CallStatusBackfill}, wantSeen: false,
			wantLog: "requeue: 3 queued call(s) from before the restart (0 scanned now, 3 stored only)"},
		{name: "token already queued in this process", maxAge: 24 * time.Hour, queuedFirst: true, wantQueued: "[1103 1101]",
			wantStatuses: map[int]string{1100: CallStatusDuplicate, 1101: CallStatusQueued, 1102: CallStatusDuplicate, 1103: CallStatusQueued}, wantSeen: true,
			wantLog: "requeue: 3 queued call(s) from before the restart (1 scanned now, 0 stored only; 2 duplicate(s)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, st, _ := trackerFixture(t)
			ctx := context.Background()
			s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, tc.maxAge, 20*time.Second
			buf := captureLog(t)
			now := time.Now().UTC().Truncate(time.Second)
			other := caN(0x52)
			upperAge := tc.upperAge
			if upperAge == 0 {
				upperAge = 3 * time.Hour
			}
			insertQueuedCall(t, st, 1100, now.Add(-upperAge), upper, CallStatusQueued)
			insertQueuedCall(t, st, 1101, now.Add(-2*time.Hour), other, CallStatusQueued)
			insertQueuedCall(t, st, 1102, now.Add(-time.Hour), lower, CallStatusDropped)
			s.seen.markNew(upper)
			s.seen.markNew(other)

			s2 := restartScanner(t, s)
			if tc.queuedFirst {
				s2.seen.forget(upper) // the new post is a real call of the token
				s2.onChannelPost(postAt(1103, now, lower))
			}
			if err := s2.requeue(ctx); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(drainQueue(s2)); got != tc.wantQueued {
				t.Fatalf("queued posts %s, want %s\n%s", got, tc.wantQueued, buf.String())
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log is missing %q:\n%s", tc.wantLog, buf.String())
			}
			for msg, want := range tc.wantStatuses {
				ca := lower
				if msg == 1101 {
					ca = other
				}
				if row := callByMsg(t, st, ca, msg); row.Status != want {
					t.Errorf("post %d: status %q, want %q", msg, row.Status, want)
				}
			}
			if seen := !s2.seen.markNew(upper); seen != tc.wantSeen {
				t.Errorf("token %s marked in seen_cas.json: got %v, want %v", upper, seen, tc.wantSeen)
			}
		})
	}
}

// A stored-only post whose write fails with a database error (not a cancelled
// context) stops the catch-up: the cursor does not move past it, it is not
// marked handled, and the next catch-up stores it.
func TestCatchUpStoreOnlyDBErrorRetries(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, s.cfg.PollInterval = 10, 24*time.Hour, 20*time.Second
	captureLog(t)
	now := time.Now().UTC().Truncate(time.Second)
	bad, next, live := caN(0x61), caN(0x62), caN(0x63)
	if _, err := st.Pool.Exec(ctx, `ALTER TABLE scout_calls ADD CONSTRAINT test_reject_ca CHECK (contract_address <> '`+bad+`')`); err != nil {
		t.Fatal(err)
	}
	s.saveCursor(1200)
	channel := []*tg.Message{
		postAt(1200, now.Add(-40*time.Hour), caN(0x60)),
		postAt(1201, now.Add(-30*time.Hour), bad),  // stored only: its write fails
		postAt(1202, now.Add(-29*time.Hour), next), // stored only
		postAt(1203, now.Add(-time.Hour), live),    // live
	}
	newest, err := s.catchUp(ctx, 1200, (&fakeHistory{posts: channel}).page)
	if err == nil || newest != 1200 || !strings.Contains(err.Error(), "storing post 1201") {
		t.Fatalf("catchUp(1200) with post 1201's write failing = %d, %v; want 1200 and a \"storing post 1201\" error", newest, err)
	}
	if got := cursorFile(t, s); got != 1200 {
		t.Fatalf("cursor file %d, want 1200 (unchanged)", got)
	}
	if s.handled[handledKey(1201, bad)] || s.handled[handledKey(1202, next)] {
		t.Fatalf("posts 1201/1202 marked handled (%v/%v), want neither", s.handled[handledKey(1201, bad)], s.handled[handledKey(1202, next)])
	}
	if n := len(s.queue); n != 0 {
		t.Fatalf("a stopped catch-up queued %d job(s), want 0", n)
	}

	if _, err := st.Pool.Exec(ctx, `ALTER TABLE scout_calls DROP CONSTRAINT test_reject_ca`); err != nil {
		t.Fatal(err)
	}
	if newest, err := s.catchUp(ctx, 1200, (&fakeHistory{posts: channel}).page); err != nil || newest != 1203 {
		t.Fatalf("next catchUp(1200) = %d, %v; want 1203, nil", newest, err)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[1203]" {
		t.Fatalf("next catch-up queued %v, want [1203]", got)
	}
	for _, tc := range []struct {
		msg int
		ca  string
	}{{1201, bad}, {1202, next}} {
		if row := callByMsg(t, st, tc.ca, tc.msg); row.Status != CallStatusBackfill {
			t.Errorf("post %d: status %q, want %q", tc.msg, row.Status, CallStatusBackfill)
		}
	}
	if got := cursorFile(t, s); got != 1203 {
		t.Fatalf("cursor file %d, want 1203", got)
	}
}
