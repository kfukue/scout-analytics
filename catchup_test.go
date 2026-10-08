package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeHistory pages through channel posts the way MessagesGetHistory does with
// OffsetID: up to catchUpPageSize posts older than offsetID (0 = the newest),
// returned oldest first. failAt makes the request with that offset fail.
type fakeHistory struct {
	posts  []*tg.Message // ascending ids
	calls  []int         // offsets asked for
	failAt int
}

func (f *fakeHistory) page(_ context.Context, offsetID int) ([]*tg.Message, error) {
	f.calls = append(f.calls, offsetID)
	if f.failAt != 0 && offsetID == f.failAt {
		return nil, errors.New("flaky network")
	}
	var older []*tg.Message
	for _, m := range f.posts {
		if offsetID == 0 || m.ID < offsetID {
			older = append(older, m)
		}
	}
	if len(older) > catchUpPageSize {
		older = older[len(older)-catchUpPageSize:]
	}
	return older, nil
}

// textPosts returns posts from..to without a CA, one minute apart, ending now.
func textPosts(from, to int, now time.Time) []*tg.Message {
	var out []*tg.Message
	for id := from; id <= to; id++ {
		out = append(out, &tg.Message{ID: id, Date: int(now.Add(time.Duration(id-to) * time.Minute).Unix()), Message: fmt.Sprintf("gm %d", id)})
	}
	return out
}

func ids(msgs []*tg.Message) []int {
	out := make([]int, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

func TestLoadCatchUpConfig(t *testing.T) {
	tests := []struct {
		name, max, age string
		wantMax        int
		wantAge        time.Duration
		wantErr        bool
	}{
		{name: "defaults", wantMax: catchUpMaxDefault, wantAge: catchUpMaxAgeDefault},
		{name: "set", max: "30", age: "6h", wantMax: 30, wantAge: 6 * time.Hour},
		{name: "zero: everything stored only", max: "0", age: "0", wantMax: 0, wantAge: 0},
		{name: "at the limit", max: "250", wantMax: 250, wantAge: catchUpMaxAgeDefault},
		{name: "over the limit", max: "251", wantErr: true},
		{name: "negative", max: "-1", wantErr: true},
		{name: "not a number", max: "lots", wantErr: true},
		{name: "negative age", age: "-1h", wantErr: true},
		{name: "age not a duration", age: "a day", wantErr: true},
		{name: "age without a unit", age: "24", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCOUT_CATCHUP_MAX", tc.max)
			t.Setenv("SCOUT_CATCHUP_MAX_AGE", tc.age)
			n, age, err := loadCatchUpConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SCOUT_CATCHUP_MAX=%q SCOUT_CATCHUP_MAX_AGE=%q: got no error, want one", tc.max, tc.age)
				}
				return
			}
			if err != nil || n != tc.wantMax || age != tc.wantAge {
				t.Fatalf("SCOUT_CATCHUP_MAX=%q SCOUT_CATCHUP_MAX_AGE=%q: got %d, %s, %v; want %d, %s, nil",
					tc.max, tc.age, n, age, err, tc.wantMax, tc.wantAge)
			}
		})
	}
}

func TestPlanCatchUp(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	hoursAgo := func(id int, h float64) *tg.Message {
		return &tg.Message{ID: id, Date: int(now.Add(-time.Duration(h * float64(time.Hour))).Unix())}
	}
	gap := []*tg.Message{hoursAgo(1, 30), hoursAgo(2, 25), hoursAgo(3, 23), hoursAgo(4, 2), hoursAgo(5, 1), hoursAgo(6, 0)}
	tests := []struct {
		name       string
		posts      []*tg.Message
		limit      int
		maxAge     time.Duration
		wantStored []int
		wantLive   []int
	}{
		{name: "empty gap", posts: nil, limit: 10, maxAge: 24 * time.Hour, wantStored: []int{}, wantLive: []int{}},
		{name: "count limit", posts: gap, limit: 2, maxAge: 0, wantStored: []int{1, 2, 3, 4}, wantLive: []int{5, 6}},
		{name: "age limit", posts: gap, limit: 100, maxAge: 24 * time.Hour, wantStored: []int{1, 2}, wantLive: []int{3, 4, 5, 6}},
		{name: "both: count is tighter", posts: gap, limit: 3, maxAge: 24 * time.Hour, wantStored: []int{1, 2, 3}, wantLive: []int{4, 5, 6}},
		{name: "both: age is tighter", posts: gap, limit: 5, maxAge: 90 * time.Minute, wantStored: []int{1, 2, 3, 4}, wantLive: []int{5, 6}},
		{name: "zero: all stored only", posts: gap, limit: 0, maxAge: 0, wantStored: []int{1, 2, 3, 4, 5, 6}, wantLive: []int{}},
		{name: "no limits hit", posts: gap, limit: 100, maxAge: 0, wantStored: []int{}, wantLive: []int{1, 2, 3, 4, 5, 6}},
		{name: "everything too old", posts: gap[:2], limit: 100, maxAge: time.Hour, wantStored: []int{1, 2}, wantLive: []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stored, live := planCatchUp(tc.posts, tc.limit, tc.maxAge, now)
			if got := ids(stored); fmt.Sprint(got) != fmt.Sprint(tc.wantStored) {
				t.Errorf("planCatchUp(%v, %d, %s): stored %v, want %v", ids(tc.posts), tc.limit, tc.maxAge, got, tc.wantStored)
			}
			if got := ids(live); fmt.Sprint(got) != fmt.Sprint(tc.wantLive) {
				t.Errorf("planCatchUp(%v, %d, %s): live %v, want %v", ids(tc.posts), tc.limit, tc.maxAge, got, tc.wantLive)
			}
		})
	}
}

func TestStartCursor(t *testing.T) {
	tests := []struct {
		name           string
		dbID           int
		dbOK           bool
		fileID         int
		fileOK         bool
		want           int
		wantFromSubstr string
	}{
		{name: "neither", want: 0},
		{name: "database only", dbID: 500, dbOK: true, want: 500, wantFromSubstr: "database"},
		{name: "file only (database off)", fileID: 480, fileOK: true, want: 480, wantFromSubstr: "poll cursor"},
		{name: "file lower: live updates recorded ahead of polling", dbID: 500, dbOK: true, fileID: 495, fileOK: true, want: 495, wantFromSubstr: "poll cursor"},
		{name: "database lower: posts without a CA after the last call", dbID: 500, dbOK: true, fileID: 510, fileOK: true, want: 500, wantFromSubstr: "database"},
		{name: "equal", dbID: 500, dbOK: true, fileID: 500, fileOK: true, want: 500, wantFromSubstr: "database"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, from := startCursor(tc.dbID, tc.dbOK, tc.fileID, tc.fileOK)
			if got != tc.want || !strings.Contains(from, tc.wantFromSubstr) {
				t.Fatalf("startCursor(%d, %v, %d, %v) = %d, %q; want %d, containing %q",
					tc.dbID, tc.dbOK, tc.fileID, tc.fileOK, got, from, tc.want, tc.wantFromSubstr)
			}
		})
	}
}

func TestPollCursorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), pollCursorFileName)
	if id, ok, err := readPollCursor(path, 777); id != 0 || ok || err != nil {
		t.Fatalf("missing file: got %d, %v, %v; want 0, false, nil", id, ok, err)
	}
	if err := writePollCursor(path, pollCursor{ChannelID: 777, Channel: "scoutrobinhood", PostID: 10420, SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := writePollCursor(path, pollCursor{ChannelID: 777, Channel: "scoutrobinhood", PostID: 10425, SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := readPollCursor(path, 777); id != 10425 || !ok || err != nil {
		t.Fatalf("after two writes: got %d, %v, %v; want 10425, true, nil", id, ok, err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left behind: %v", err)
	}
	if id, ok, err := readPollCursor(path, 999); id != 0 || ok || err != nil {
		t.Fatalf("another channel: got %d, %v, %v; want 0, false, nil", id, ok, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readPollCursor(path, 777); ok || err == nil {
		t.Fatalf("corrupt file: got ok=%v err=%v; want false and an error", ok, err)
	}
}

func TestFetchGap(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		channel   []*tg.Message
		cursor    int
		wantFirst int
		wantLast  int
		wantN     int
		wantPages int
	}{
		{name: "gap over three pages", channel: textPosts(1, 400, now), cursor: 150, wantFirst: 151, wantLast: 400, wantN: 250, wantPages: 3},
		{name: "gap ends exactly on a page", channel: textPosts(1, 400, now), cursor: 300, wantFirst: 301, wantLast: 400, wantN: 100, wantPages: 2},
		{name: "no gap", channel: textPosts(1, 400, now), cursor: 400, wantN: 0, wantPages: 1},
		{name: "cursor below the whole channel", channel: textPosts(1, 120, now), cursor: 0, wantFirst: 1, wantLast: 120, wantN: 120, wantPages: 2},
		{name: "empty channel", channel: nil, cursor: 10, wantN: 0, wantPages: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHistory{posts: tc.channel}
			got, err := fetchGap(context.Background(), tc.cursor, h.page)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantN || len(h.calls) != tc.wantPages {
				t.Fatalf("cursor %d: got %d posts in %d page(s) (offsets %v), want %d in %d", tc.cursor, len(got), len(h.calls), h.calls, tc.wantN, tc.wantPages)
			}
			for i := 1; i < len(got); i++ {
				if got[i].ID != got[i-1].ID+1 {
					t.Fatalf("cursor %d: posts not contiguous and ascending at %d: %d then %d", tc.cursor, i, got[i-1].ID, got[i].ID)
				}
			}
			if tc.wantN > 0 && (got[0].ID != tc.wantFirst || got[len(got)-1].ID != tc.wantLast) {
				t.Fatalf("cursor %d: got posts %d..%d, want %d..%d", tc.cursor, got[0].ID, got[len(got)-1].ID, tc.wantFirst, tc.wantLast)
			}
		})
	}
	t.Run("error", func(t *testing.T) {
		h := &fakeHistory{posts: textPosts(1, 400, now), failAt: 301}
		if got, err := fetchGap(context.Background(), 150, h.page); err == nil || got != nil {
			t.Fatalf("second page fails: got %d posts, err %v; want nil and an error", len(got), err)
		}
	})
}

// catchUpScanner is a scanner without a database (SCOUT_DB=off) whose state
// directory is the test's.
func catchUpScanner(t *testing.T, limit int, maxAge time.Duration) *scanner {
	t.Helper()
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(),
		CatchUpMax: limit, CatchUpMaxAge: maxAge, PollInterval: 20 * time.Second}
	s := newScanner(cfg)
	s.sourceChannelID = 777
	return s
}

func caN(n int) string { return fmt.Sprintf("0x%040x", n) }

// Without a database: the newest missed posts are handled live (queued once),
// older ones are not; the cursor file moves to the newest post, and the token
// of a stored-only post is not marked as investigated.
func TestCatchUpDBOff(t *testing.T) {
	s := catchUpScanner(t, 3, 24*time.Hour)
	buf := captureLog(t)
	now := time.Now()
	scanned := caN(9) // investigated before the restart
	if !s.seen.markNew(scanned) {
		t.Fatal("seed seen CA")
	}
	channel := []*tg.Message{
		postAt(100, now.Add(-3*time.Hour), caN(100)), // the cursor: handled before the restart
		postAt(101, now.Add(-2*time.Hour), caN(101)), // stored only (count cap)
		{ID: 102, Date: int(now.Add(-100 * time.Minute).Unix()), Message: "gm"},
		postAt(103, now.Add(-90*time.Minute), caN(103)),  // live
		updatePostAt(104, now.Add(-time.Hour), caN(100)), // live, but an update: recorded only
		postAt(105, now.Add(-30*time.Minute), scanned),   // live, but already investigated
	}
	h := &fakeHistory{posts: channel}
	if err := writePollCursor(s.cursorPath(), pollCursor{ChannelID: 777, PostID: 100}); err != nil {
		t.Fatal(err)
	}
	cursor, from, err := s.resumeCursor(context.Background())
	if cursor != 100 || !strings.Contains(from, "poll cursor") || err != nil {
		t.Fatalf("resumeCursor = %d, %q, %v; want 100 from the poll cursor file", cursor, from, err)
	}
	newest, err := s.catchUp(context.Background(), cursor, h.page)
	if err != nil || newest != 105 {
		t.Fatalf("catchUp(100) = %d, %v; want 105, nil", newest, err)
	}
	want := "catch-up: 5 post(s) since post 100 (3 handled live, 2 stored only); SCOUT_DB=off, so the stored-only posts were not recorded"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("log is missing %q:\n%s", want, buf.String())
	}
	if n := len(s.queue); n != 1 {
		t.Fatalf("queued %d job(s), want 1 (post 103)\n%s", n, buf.String())
	}
	if j := <-s.queue; j.SourceMsg != 103 || caKey(j.CA) != caKey(caN(103)) {
		t.Fatalf("queued post %d %s, want post 103 %s", j.SourceMsg, j.CA, caN(103))
	}
	// polling sees the stored-only post again: nothing happens
	s.onChannelPost(channel[1])
	if n := len(s.queue); n != 0 {
		t.Fatalf("stored-only post 101 seen again by polling queued %d job(s), want 0", n)
	}
	// its token is not used up: a later real call of it is investigated
	if !s.seen.markNew(caN(101)) {
		t.Fatal("a stored-only post marked its token as investigated")
	}
	if id, ok, err := readPollCursor(s.cursorPath(), 777); id != 105 || !ok || err != nil {
		t.Fatalf("cursor file after catch-up: %d, %v, %v; want 105, true, nil", id, ok, err)
	}
	// a second catch-up (reconnect) finds nothing new
	if newest, err := s.catchUp(context.Background(), 105, h.page); err != nil || newest != 105 {
		t.Fatalf("second catchUp(105) = %d, %v; want 105, nil", newest, err)
	}
	if !strings.Contains(buf.String(), "catch-up: 0 post(s) since post 105 (0 handled live, 0 stored only)") {
		t.Fatalf("second catch-up log:\n%s", buf.String())
	}
}

// A reconnect inside the same process catches up from an older cursor: posts
// it already handled are counted as recorded and not handled again.
func TestCatchUpSkipsPostsHandledInThisProcess(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	buf := captureLog(t)
	now := time.Now()
	channel := []*tg.Message{postAt(200, now, caN(200)), postAt(201, now, caN(201)), postAt(202, now, caN(202))}
	s.onChannelPost(channel[1]) // a live update before the disconnect
	<-s.queue
	newest, err := s.catchUp(context.Background(), 200, (&fakeHistory{posts: channel}).page)
	if err != nil || newest != 202 {
		t.Fatalf("catchUp(200) = %d, %v; want 202, nil", newest, err)
	}
	if !strings.Contains(buf.String(), "catch-up: 1 post(s) since post 200 (1 handled live, 0 stored only; 1 already recorded, skipped)") {
		t.Fatalf("log:\n%s", buf.String())
	}
	if n := len(s.queue); n != 1 {
		t.Fatalf("queued %d job(s), want 1 (post 202)", n)
	}
	if j := <-s.queue; j.SourceMsg != 202 {
		t.Fatalf("queued post %d, want 202", j.SourceMsg)
	}
}

// A failed read handles nothing and leaves the cursor where it was, so the next
// poll tick can try again.
func TestCatchUpReadErrorHandlesNothing(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	now := time.Now()
	var channel []*tg.Message
	for id := 1; id <= 150; id++ {
		channel = append(channel, postAt(id, now, caN(id)))
	}
	h := &fakeHistory{posts: channel, failAt: 51}
	newest, err := s.catchUp(context.Background(), 10, h.page)
	if err == nil || newest != 10 {
		t.Fatalf("catchUp(10) with a failing second page = %d, %v; want 10 and an error", newest, err)
	}
	if n := len(s.queue); n != 0 {
		t.Fatalf("a failed catch-up queued %d job(s), want 0", n)
	}
	if _, ok, _ := readPollCursor(s.cursorPath(), 777); ok {
		t.Fatal("a failed catch-up wrote the cursor file")
	}
	h.failAt = 0
	if newest, err := s.catchUp(context.Background(), 10, h.page); err != nil || newest != 150 {
		t.Fatalf("retry: catchUp(10) = %d, %v; want 150, nil", newest, err)
	}
	if n := len(s.queue); n != 10 {
		t.Fatalf("retry queued %d job(s), want 10 (SCOUT_CATCHUP_MAX)", n)
	}
}

// No saved cursor at all (first start, no database rows): nothing to catch up.
func TestResumeCursorNothingSaved(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	if cursor, from, err := s.resumeCursor(context.Background()); cursor != 0 || from != "" || err != nil {
		t.Fatalf("resumeCursor with no file and no database = %d, %q, %v; want 0, \"\", nil", cursor, from, err)
	}
}

// fakeSource is a pollSource over a fake channel: newest and since behave like
// MessagesGetHistory without and with MinID (since returns the newest limit
// posts above minID), page like fakeHistory. Each read can be made to fail.
type fakeSource struct {
	mu          sync.Mutex
	h           fakeHistory
	newestErr   error
	sinceErr    error
	newestCalls int
	sinceCalls  int
}

func (f *fakeSource) setPosts(posts []*tg.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.h.posts = posts
}

func (f *fakeSource) newest(context.Context) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newestCalls++
	if f.newestErr != nil {
		return 0, false, f.newestErr
	}
	if len(f.h.posts) == 0 {
		return 0, false, nil
	}
	return f.h.posts[len(f.h.posts)-1].ID, true, nil
}

func (f *fakeSource) since(_ context.Context, minID, limit int) ([]*tg.Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinceCalls++
	if f.sinceErr != nil {
		return nil, false, f.sinceErr
	}
	var out []*tg.Message
	for _, m := range f.h.posts {
		if m.ID > minID {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, len(out) >= limit, nil
}

func (f *fakeSource) page(ctx context.Context, offsetID int) ([]*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h.page(ctx, offsetID)
}

func (f *fakeSource) pageCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.h.calls)
}

// cursorFile returns the saved cursor, failing the test when there is none.
func cursorFile(t *testing.T, s *scanner) int {
	t.Helper()
	id, ok, err := readPollCursor(s.cursorPath(), s.sourceChannelID)
	if err != nil || !ok {
		t.Fatalf("cursor file: %d, %v, %v; want a saved cursor", id, ok, err)
	}
	return id
}

func drainQueue(s *scanner) []int {
	var out []int
	for len(s.queue) > 0 {
		out = append(out, (<-s.queue).SourceMsg)
	}
	return out
}

// No saved cursor: the first read starts at the channel's newest post and
// saves it; a failed first read is retried by the next tick.
func TestPollerFirstReadSavesCursor(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	ctx := context.Background()
	src := &fakeSource{h: fakeHistory{posts: textPosts(1, 40, time.Now())}, newestErr: errors.New("flaky network")}
	p := &poller{s: s, src: src}
	if err := p.begin(ctx); err == nil || !strings.Contains(err.Error(), "initial read") {
		t.Fatalf("begin with a failing first read: got %v, want an \"initial read\" error", err)
	}
	if p.started {
		t.Fatal("begin with a failing first read: started, want not started")
	}
	if _, ok, _ := readPollCursor(s.cursorPath(), 777); ok {
		t.Fatal("a failed first read saved a cursor")
	}
	src.newestErr = nil
	if err := p.tick(ctx); err != nil || !p.started || p.cursor != 40 {
		t.Fatalf("tick after the failure: err %v, started %v, cursor %d; want nil, true, 40", err, p.started, p.cursor)
	}
	if got := cursorFile(t, s); got != 40 {
		t.Fatalf("cursor file after the first read: %d, want 40", got)
	}
	if src.pageCalls() != 0 || len(s.queue) != 0 {
		t.Fatalf("first start read %d history page(s) and queued %d job(s), want 0 and 0 (no catch-up)", src.pageCalls(), len(s.queue))
	}
	if !strings.Contains(p.from, "newest post") {
		t.Fatalf("from = %q, want the channel's newest post", p.from)
	}
}

// A catch-up that fails is retried by every tick until it succeeds; regular
// polling waits for it, and the cursor is saved only once it worked.
func TestPollerRetriesCatchUpUntilDone(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	ctx := context.Background()
	now := time.Now()
	var channel []*tg.Message
	for id := 1; id <= 150; id++ {
		channel = append(channel, postAt(id, now, caN(id)))
	}
	s.saveCursor(140)
	src := &fakeSource{h: fakeHistory{posts: channel}}
	// the first history page fails twice
	bad := errors.New("telegram down")
	pages := 0
	p := &poller{s: s, src: pageFailer{fakeSource: src, fail: func() error {
		pages++
		if pages <= 2 {
			return bad
		}
		return nil
	}}}
	for i := 0; i < 2; i++ {
		var err error
		if i == 0 {
			err = p.begin(ctx)
		} else {
			err = p.tick(ctx)
		}
		if !errors.Is(err, bad) || p.started {
			t.Fatalf("attempt %d: got err %v, started %v; want %v, false", i+1, err, p.started, bad)
		}
		if got := cursorFile(t, s); got != 140 {
			t.Fatalf("attempt %d: cursor file %d, want 140 (unchanged)", i+1, got)
		}
		if n := len(s.queue); n != 0 {
			t.Fatalf("attempt %d: a failed catch-up queued %d job(s), want 0", i+1, n)
		}
	}
	if err := p.tick(ctx); err != nil || !p.started || p.cursor != 150 {
		t.Fatalf("third attempt: err %v, started %v, cursor %d; want nil, true, 150", err, p.started, p.cursor)
	}
	if got := cursorFile(t, s); got != 150 {
		t.Fatalf("cursor file after the catch-up: %d, want 150", got)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[141 142 143 144 145 146 147 148 149 150]" {
		t.Fatalf("catch-up queued posts %v, want 141..150", got)
	}
	if src.sinceCalls != 0 {
		t.Fatalf("regular polling ran %d time(s) before the catch-up was done, want 0", src.sinceCalls)
	}
}

// pageFailer makes history pages fail while fail returns an error.
type pageFailer struct {
	*fakeSource
	fail func() error
}

func (f pageFailer) page(ctx context.Context, offsetID int) ([]*tg.Message, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.fakeSource.page(ctx, offsetID)
}

// Regular polling saves the cursor when it moves, and only then.
func TestPollerSavesCursorWhenItMoves(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	ctx := context.Background()
	now := time.Now()
	channel := []*tg.Message{postAt(10, now, caN(10))}
	src := &fakeSource{h: fakeHistory{posts: channel}}
	p := &poller{s: s, src: src}
	if err := p.begin(ctx); err != nil || p.cursor != 10 {
		t.Fatalf("begin: err %v, cursor %d; want nil, 10", err, p.cursor)
	}
	saved := func() time.Time {
		t.Helper()
		b, err := os.ReadFile(s.cursorPath())
		if err != nil {
			t.Fatal(err)
		}
		var c pollCursor
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return c.SavedAt
	}

	tests := []struct {
		name       string
		add        []*tg.Message
		wantCursor int
		wantSaved  bool
		wantQueued string
	}{
		{name: "nothing new: not saved", wantCursor: 10, wantQueued: "[]"},
		{name: "two new posts: saved", add: []*tg.Message{postAt(11, now, caN(11)), {ID: 12, Date: int(now.Unix()), Message: "gm"}}, wantCursor: 12, wantSaved: true, wantQueued: "[11]"},
		{name: "nothing new again: not saved", wantCursor: 12, wantQueued: "[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			channel = append(channel, tc.add...)
			src.setPosts(channel)
			// same post, no SavedAt: a write by the tick sets it
			if err := writePollCursor(s.cursorPath(), pollCursor{ChannelID: 777, PostID: p.cursor}); err != nil {
				t.Fatal(err)
			}
			if err := p.tick(ctx); err != nil || p.cursor != tc.wantCursor {
				t.Fatalf("tick: err %v, cursor %d; want nil, %d", err, p.cursor, tc.wantCursor)
			}
			if got := fmt.Sprint(drainQueue(s)); got != tc.wantQueued {
				t.Fatalf("tick queued %s, want %s", got, tc.wantQueued)
			}
			if got := cursorFile(t, s); got != tc.wantCursor {
				t.Fatalf("cursor file %d, want %d", got, tc.wantCursor)
			}
			if rewritten := !saved().IsZero(); rewritten != tc.wantSaved {
				t.Fatalf("cursor file rewritten: %v, want %v", rewritten, tc.wantSaved)
			}
		})
	}
}

// More than one batch of new posts in one interval: they are read page by
// page, none is skipped, and the cursor ends on the newest.
func TestPollerFullBatchReadsPageByPage(t *testing.T) {
	tests := []struct {
		name      string
		newPosts  int
		wantPages int
	}{
		{name: "less than a batch", newPosts: pollBatch - 1, wantPages: 0},
		{name: "exactly a batch", newPosts: pollBatch, wantPages: 1},
		{name: "two and a half pages", newPosts: 250, wantPages: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := catchUpScanner(t, 10, 0)
			captureLog(t)
			ctx := context.Background()
			now := time.Now()
			channel := []*tg.Message{postAt(1000, now, caN(1000))}
			src := &fakeSource{h: fakeHistory{posts: channel}}
			p := &poller{s: s, src: src}
			if err := p.begin(ctx); err != nil {
				t.Fatal(err)
			}
			for id := 1001; id <= 1000+tc.newPosts; id++ {
				channel = append(channel, postAt(id, now, caN(id)))
			}
			src.setPosts(channel)
			if err := p.tick(ctx); err != nil {
				t.Fatalf("%d new posts: tick: %v", tc.newPosts, err)
			}
			got := drainQueue(s)
			if len(got) != tc.newPosts || (len(got) > 0 && (got[0] != 1001 || got[len(got)-1] != 1000+tc.newPosts)) {
				t.Fatalf("%d new posts: queued %d (first/last %v), want all of 1001..%d", tc.newPosts, len(got), firstLast(got), 1000+tc.newPosts)
			}
			if src.pageCalls() != tc.wantPages {
				t.Fatalf("%d new posts: read %d history page(s), want %d", tc.newPosts, src.pageCalls(), tc.wantPages)
			}
			if want := 1000 + tc.newPosts; p.cursor != want || cursorFile(t, s) != want {
				t.Fatalf("%d new posts: cursor %d, file %d; want %d", tc.newPosts, p.cursor, cursorFile(t, s), want)
			}
		})
	}
}

// Service messages (joins, pins) count towards a full batch although
// messagesOf drops them.
func TestRawMessageCount(t *testing.T) {
	tests := []struct {
		name          string
		res           tg.MessagesMessagesClass
		wantRaw, want int
	}{
		{name: "channel messages", res: &tg.MessagesChannelMessages{Messages: []tg.MessageClass{&tg.MessageService{ID: 2}, &tg.Message{ID: 1}}}, wantRaw: 2, want: 1},
		{name: "slice", res: &tg.MessagesMessagesSlice{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 3}, &tg.Message{ID: 1}, &tg.Message{ID: 2}}}, wantRaw: 3, want: 2},
		{name: "messages", res: &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageService{ID: 1}}}, wantRaw: 1, want: 0},
		{name: "not modified", res: &tg.MessagesMessagesNotModified{}, wantRaw: 0, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, gotMsgs := rawMessageCount(tc.res), len(messagesOf(tc.res)); got != tc.wantRaw || gotMsgs != tc.want {
				t.Fatalf("%T: rawMessageCount %d, messagesOf %d; want %d, %d", tc.res, got, gotMsgs, tc.wantRaw, tc.want)
			}
		})
	}
}

func firstLast(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	return []int{ids[0], ids[len(ids)-1]}
}

// closedStore is a database whose every query fails at once.
func closedStore(t *testing.T) *ScoutStore {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return NewScoutStoreFromPool(pool)
}

// A database error while reading the resume point is not "nothing saved":
// nothing is read or skipped, and the next tick tries again.
func TestPollerResumeDBErrorRetries(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	buf := captureLog(t)
	ctx := context.Background()
	s.db = closedStore(t)
	s.saveCursor(5)
	now := time.Now()
	src := &fakeSource{h: fakeHistory{posts: []*tg.Message{postAt(5, now, caN(5)), postAt(6, now, caN(6))}}}
	p := &poller{s: s, src: src}
	for i := 0; i < 2; i++ {
		var err error
		if i == 0 {
			err = p.begin(ctx)
		} else {
			err = p.tick(ctx)
		}
		if err == nil || !strings.Contains(err.Error(), "catch-up: reading the resume point") {
			t.Fatalf("attempt %d with the database down: got %v, want a \"catch-up: reading the resume point\" error", i+1, err)
		}
		if p.resolved || p.started || src.newestCalls != 0 || src.pageCalls() != 0 || src.sinceCalls != 0 {
			t.Fatalf("attempt %d: resolved %v, started %v, Telegram reads %d/%d/%d; want nothing done",
				i+1, p.resolved, p.started, src.newestCalls, src.pageCalls(), src.sinceCalls)
		}
	}
	if got := cursorFile(t, s); got != 5 {
		t.Fatalf("cursor file %d, want 5 (unchanged)", got)
	}
	if strings.Contains(buf.String(), "newest post") {
		t.Fatalf("the outage was skipped to the channel's newest post:\n%s", buf.String())
	}
	s.db = nil // the database is back (here: switched off, so the file is the cursor)
	if err := p.tick(ctx); err != nil || !p.started || p.cursor != 6 {
		t.Fatalf("tick with the database back: err %v, started %v, cursor %d; want nil, true, 6", err, p.started, p.cursor)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[6]" {
		t.Fatalf("catch-up queued %v, want [6]", got)
	}
}

func TestResumeCursorDBError(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	s.db = closedStore(t)
	s.saveCursor(5)
	if id, from, err := s.resumeCursor(context.Background()); err == nil || id != 0 || from != "" {
		t.Fatalf("resumeCursor with the database down = %d, %q, %v; want 0, \"\", an error", id, from, err)
	}
}

// SCOUT_POLL_INTERVAL=0: no polling, but the start (catch-up) runs once.
func TestPollIntervalZeroRunsCatchUpOnce(t *testing.T) {
	tests := []struct {
		name     string
		failPage bool
		wantLog  string
		wantFile int
		wantQ    string
	}{
		{name: "catch-up done", wantLog: "polling @scoutrobinhood is off (SCOUT_POLL_INTERVAL=0); the catch-up ran once, up to post 22", wantFile: 22, wantQ: "[21 22]"},
		{name: "catch-up fails: not retried", failPage: true, wantLog: "not retried: SCOUT_POLL_INTERVAL=0", wantFile: 20, wantQ: "[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := catchUpScanner(t, 10, 0)
			s.cfg.PollInterval = 0
			buf := captureLog(t)
			now := time.Now()
			s.saveCursor(20)
			src := &fakeSource{h: fakeHistory{posts: []*tg.Message{postAt(20, now, caN(20)), postAt(21, now, caN(21)), postAt(22, now, caN(22))}}}
			var ps pollSource = src
			if tc.failPage {
				ps = pageFailer{fakeSource: src, fail: func() error { return errors.New("telegram down") }}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.pollWith(context.Background(), ps)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("pollWith with SCOUT_POLL_INTERVAL=0 did not return")
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log is missing %q:\n%s", tc.wantLog, buf.String())
			}
			if got := cursorFile(t, s); got != tc.wantFile {
				t.Fatalf("cursor file %d, want %d", got, tc.wantFile)
			}
			if got := fmt.Sprint(drainQueue(s)); got != tc.wantQ {
				t.Fatalf("queued %s, want %s", got, tc.wantQ)
			}
		})
	}
}

// pollWith returns when its context ends, also while the catch-up is still
// being retried.
func TestPollWithStopsOnCancel(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	s.cfg.PollInterval = 5 * time.Millisecond
	captureLog(t)
	s.saveCursor(20)
	src := &fakeSource{h: fakeHistory{posts: textPosts(1, 30, time.Now())}}
	ps := pageFailer{fakeSource: src, fail: func() error { return errors.New("telegram down") }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pollWith(ctx, ps)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollWith did not return after cancel")
	}
}

// A cancelled context stops the catch-up between posts: the cursor stays,
// and the posts not handled yet are handled by the next catch-up.
func TestCatchUpStopsOnCancel(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	now := time.Now()
	channel := []*tg.Message{postAt(1, now, caN(1)), postAt(2, now, caN(2)), postAt(3, now, caN(3))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if newest, err := s.catchUp(ctx, 1, (&fakeHistory{posts: channel}).page); !errors.Is(err, context.Canceled) || newest != 1 {
		t.Fatalf("catchUp with a cancelled context = %d, %v; want 1, %v", newest, err, context.Canceled)
	}
	if n := len(s.queue); n != 0 {
		t.Fatalf("a cancelled catch-up queued %d job(s), want 0", n)
	}
	if _, ok, _ := readPollCursor(s.cursorPath(), 777); ok {
		t.Fatal("a cancelled catch-up saved the cursor")
	}
	if newest, err := s.catchUp(context.Background(), 1, (&fakeHistory{posts: channel}).page); err != nil || newest != 3 {
		t.Fatalf("next catchUp = %d, %v; want 3, nil", newest, err)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("next catch-up queued %v, want [2 3]", got)
	}
}

// Cursor writes from several goroutines never collide on the temporary file.
func TestSaveCursorConcurrent(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	buf := captureLog(t)
	var wg sync.WaitGroup
	for g := 1; g <= 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				s.saveCursor(g*100 + i)
			}
		}(g)
	}
	wg.Wait()
	if strings.Contains(buf.String(), "warning") {
		t.Fatalf("concurrent cursor writes logged a failure:\n%s", buf.String())
	}
	if got := cursorFile(t, s); got%100 != 19 {
		t.Fatalf("cursor file %d, want the last write of one goroutine (x19)", got)
	}
}

// Without a database there is nothing to queue again.
func TestRequeueDBOff(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	if err := s.requeue(context.Background()); err != nil || len(s.queue) != 0 {
		t.Fatalf("requeue with SCOUT_DB=off: err %v, %d job(s); want nil, 0", err, len(s.queue))
	}
}

func TestSeenForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen_cas.json")
	s := loadSeen(path)
	ca := "0xAbCdEf0000000000000000000000000000000001"
	if !s.markNew(ca) {
		t.Fatal("markNew on an empty set: got false, want true")
	}
	s.forget(strings.ToLower(ca))
	if s2 := loadSeen(path); !s2.markNew(ca) {
		t.Fatalf("after forget(%s), reloaded from disk: markNew got false, want true", ca)
	}
	s.forget(ca) // not there: nothing happens
}

func TestRequeueWindow(t *testing.T) {
	tests := []struct {
		maxAge, want time.Duration
	}{
		{maxAge: 24 * time.Hour, want: 72 * time.Hour},
		{maxAge: 6 * time.Hour, want: 54 * time.Hour},
		{maxAge: 0, want: 72 * time.Hour}, // no age limit: the default age is the base
		{maxAge: 7 * 24 * time.Hour, want: 9 * 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.maxAge.String(), func(t *testing.T) {
			if got := requeueWindow(tc.maxAge); got != tc.want {
				t.Fatalf("requeueWindow(%s) = %s, want %s", tc.maxAge, got, tc.want)
			}
		})
	}
}

// A stored-only post whose write fails (here: the database is down, not a
// cancelled context) is not marked handled, so the next catch-up stores it.
func TestStoreOnlyPostDBErrorLeavesPostUnhandled(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	s.db = closedStore(t)
	m := postAt(11, time.Now(), caN(11))
	err := s.storeOnlyPost(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "storing post 11") {
		t.Fatalf("storeOnlyPost(post 11) with the database down: got %v, want a \"storing post 11\" error", err)
	}
	if s.handled[handledKey(11, caN(11))] {
		t.Fatal("post 11 is marked handled after its write failed, want it unmarked")
	}
	s.db = nil // SCOUT_DB=off: nothing to write, the post is handled
	if err := s.storeOnlyPost(context.Background(), m); err != nil || !s.handled[handledKey(11, caN(11))] {
		t.Fatalf("storeOnlyPost(post 11) with SCOUT_DB=off: err %v, handled %v; want nil, true", err, s.handled[handledKey(11, caN(11))])
	}
}

// The requeue after the catch-up fails: polling starts anyway, and every tick
// tries that requeue again (while still polling new posts) until it works;
// the error does not claim it waits for a restart.
func TestPollerRetriesRequeueAfterCatchUp(t *testing.T) {
	s := catchUpScanner(t, 10, 0)
	captureLog(t)
	ctx := context.Background()
	now := time.Now()
	s.saveCursor(30)
	src := &fakeSource{h: fakeHistory{posts: []*tg.Message{postAt(30, now, caN(30)), postAt(31, now, caN(31))}}}
	bad := errors.New("database down")
	calls := 0
	p := &poller{s: s, src: src, requeueFn: func(context.Context) error {
		calls++
		if calls == 2 || calls == 3 { // the one after the catch-up, then its first retry
			return bad
		}
		return nil
	}}
	err := p.begin(ctx)
	if !errors.Is(err, bad) || !p.started || !p.requeueDue || p.cursor != 31 {
		t.Fatalf("begin: err %v, started %v, requeueDue %v, cursor %d; want %v, true, true, 31", err, p.started, p.requeueDue, p.cursor, bad)
	}
	if strings.Contains(err.Error(), "restart") {
		t.Fatalf("begin error %q talks about a restart, want only the retry by polling", err)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[31]" {
		t.Fatalf("catch-up queued %v, want [31]", got)
	}
	// the retry fails again, but the new post is polled all the same
	src.setPosts(append(src.h.posts, postAt(32, now, caN(32))))
	if err := p.tick(ctx); !errors.Is(err, bad) || !p.requeueDue || p.cursor != 32 || calls != 3 {
		t.Fatalf("second tick: err %v, requeueDue %v, cursor %d, requeues %d; want %v, true, 32, 3", err, p.requeueDue, p.cursor, calls, bad)
	}
	if got := drainQueue(s); fmt.Sprint(got) != "[32]" {
		t.Fatalf("tick queued %v, want [32]", got)
	}
	if err := p.tick(ctx); err != nil || p.requeueDue || calls != 4 {
		t.Fatalf("third tick: err %v, requeueDue %v, requeues %d; want nil, false, 4", err, p.requeueDue, calls)
	}
	if err := p.tick(ctx); err != nil || calls != 4 {
		t.Fatalf("tick after the requeue worked: err %v, requeues %d; want nil, 4 (not run again)", err, calls)
	}
}
