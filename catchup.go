package main

// Catch-up after a restart. The listener resumes from a saved cursor and
// handles the posts that arrived while it was down: the newest ones as live
// posts (investigated and delivered as usual), older ones stored only, the way
// -backfill stores them, so a long outage does not flood the investigation
// bots (Perceptor paces one scan per 2 minutes) or the delivery chat.
//
// The cursor is the lower of two marks:
//   - the newest post recorded in scout_calls for the source channel (the
//     database's mark; posts without a CA are never recorded there);
//   - the poll cursor file in SCOUT_STATE_DIR, written whenever polling moves
//     on: every post up to it was handled. Live updates can record a newer post
//     before polling has seen the ones in between, so the database's mark alone
//     could skip posts.
//
// Starting a little too low is harmless: the (post, CA) pairs already recorded
// above the cursor are loaded first and are not handled again.
//
// The job queue is in memory, so the calls still waiting in it when the
// listener stopped are queued again from the database first (requeue), with
// the same limits. Only calls posted within requeueWindow are looked at.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	catchUpMaxDefault    = 100
	catchUpMaxLimit      = 250 // half the job queue: a post can carry more than one CA
	catchUpMaxAgeDefault = 24 * time.Hour
	pollCursorFileName   = "poll_cursor.json"
	catchUpPageSize      = 100
	// requeueMargin is added to SCOUT_CATCHUP_MAX_AGE (24h when that is 0) to
	// get how far back requeue looks: 72h with the defaults.
	requeueMargin = 48 * time.Hour
)

// requeueWindow is how far back requeue looks for calls left queued. Older
// ones are never rewritten automatically (on the first deploy of requeue that
// would turn every historical stuck row into backfill or duplicate in one
// pass); they are counted and logged once instead.
func requeueWindow(maxAge time.Duration) time.Duration {
	if maxAge <= 0 {
		maxAge = catchUpMaxAgeDefault
	}
	return maxAge + requeueMargin
}

// loadCatchUpConfig reads SCOUT_CATCHUP_MAX (posts handled live after a
// restart, 0 = none) and SCOUT_CATCHUP_MAX_AGE (older missed posts are stored
// only, 0 = no age limit).
func loadCatchUpConfig() (int, time.Duration, error) {
	n := catchUpMaxDefault
	if v := env("SCOUT_CATCHUP_MAX", ""); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil || x < 0 || x > catchUpMaxLimit {
			return 0, 0, fmt.Errorf("SCOUT_CATCHUP_MAX=%q: want a number from 0 to %d", v, catchUpMaxLimit)
		}
		n = x
	}
	age := catchUpMaxAgeDefault
	if v := env("SCOUT_CATCHUP_MAX_AGE", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return 0, 0, fmt.Errorf("SCOUT_CATCHUP_MAX_AGE=%q: want 0 (no limit) or a positive duration such as 24h", v)
		}
		age = d
	}
	return n, age, nil
}

// planCatchUp splits the posts missed while the listener was down (oldest
// first) into the ones stored only and the ones handled live: the newest
// limit posts that are at most maxAge old (maxAge 0 = any age) are live.
func planCatchUp(posts []*tg.Message, limit int, maxAge time.Duration, now time.Time) (stored, live []*tg.Message) {
	split := len(posts)
	for i := len(posts) - 1; i >= 0 && len(posts)-i <= limit; i-- {
		if maxAge > 0 && now.Sub(time.Unix(int64(posts[i].Date), 0)) > maxAge {
			break
		}
		split = i
	}
	return posts[:split], posts[split:]
}

// startCursor picks the post to resume after: the lower of the database's
// newest post and the cursor file; 0 when neither is known.
func startCursor(dbID int, dbOK bool, fileID int, fileOK bool) (int, string) {
	switch {
	case dbOK && fileOK && fileID < dbID:
		return fileID, "the saved poll cursor"
	case dbOK:
		return dbID, "the newest post in the database"
	case fileOK:
		return fileID, "the saved poll cursor"
	}
	return 0, ""
}

// pollCursor is the content of the poll cursor file.
type pollCursor struct {
	ChannelID int64     `json:"channel_id"`
	Channel   string    `json:"channel"`
	PostID    int       `json:"post_id"` // every post up to this one was handled
	SavedAt   time.Time `json:"saved_at"`
}

// readPollCursor returns the saved cursor for channelID; ok is false when the
// file is missing or belongs to another channel.
func readPollCursor(path string, channelID int64) (id int, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading the poll cursor: %w", err)
	}
	var c pollCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, false, fmt.Errorf("parsing the poll cursor %s: %w", path, err)
	}
	if c.ChannelID != channelID || c.PostID <= 0 {
		return 0, false, nil
	}
	return c.PostID, true, nil
}

// writePollCursor saves the cursor through a temporary file and a rename, so a
// crash never leaves half a file.
func writePollCursor(path string, c pollCursor) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the poll cursor: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("writing the poll cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing the poll cursor: %w", err)
	}
	return nil
}

func (s *scanner) cursorPath() string {
	return filepath.Join(s.cfg.StateDir, pollCursorFileName)
}

// saveCursor writes the poll cursor file; a failure is logged and polling goes
// on. Writes are serialised: two goroutines never share the temporary file.
func (s *scanner) saveCursor(postID int) {
	if postID <= 0 {
		return
	}
	c := pollCursor{ChannelID: s.sourceChannelID, Channel: s.cfg.SourceChannel, PostID: postID, SavedAt: time.Now().UTC()}
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	if err := writePollCursor(s.cursorPath(), c); err != nil {
		log.Printf("warning: %v", err)
	}
}

// resumeCursor returns the post to resume after (0 = none saved) and where it
// came from. A database error is returned, not taken for "nothing saved": the
// caller tries again later instead of skipping the outage. An unreadable
// cursor file is logged and ignored (it would fail the same way every time).
func (s *scanner) resumeCursor(ctx context.Context) (int, string, error) {
	var dbID int
	var dbOK bool
	if s.db != nil {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		id, ok, err := s.db.MaxScoutCallMessageID(qctx, s.sourceChannelID)
		cancel()
		if err != nil {
			return 0, "", fmt.Errorf("reading the resume point: %w", err)
		}
		dbID, dbOK = id, ok
	}
	fileID, fileOK, err := readPollCursor(s.cursorPath(), s.sourceChannelID)
	if err != nil {
		log.Printf("catch-up: %v (ignoring it)", err)
	}
	id, from := startCursor(dbID, dbOK, fileID, fileOK)
	return id, from, nil
}

// historyPageFunc returns up to catchUpPageSize source-channel posts older than
// offsetID (0 = from the newest), oldest first.
type historyPageFunc func(ctx context.Context, offsetID int) ([]*tg.Message, error)

// historyPage reads one page of the source channel's history, waiting out
// Telegram's flood limits; pages after the first are spaced by a second, like
// -backfill.
func (s *scanner) historyPage(ctx context.Context, offsetID int) ([]*tg.Message, error) {
	if offsetID != 0 {
		if err := sleepCtx(ctx, time.Second); err != nil {
			return nil, err
		}
	}
	for {
		res, err := s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: s.sourcePeer, OffsetID: offsetID, Limit: catchUpPageSize})
		if d, ok := tgerr.AsFloodWait(err); ok {
			log.Printf("catch-up: Telegram asks to wait %s", d)
			if err := sleepCtx(ctx, d+time.Second); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading @%s history before post %d: %w", s.cfg.SourceChannel, offsetID, err)
		}
		return messagesOf(res), nil
	}
}

// fetchGap returns the posts newer than cursor, oldest first, reading the
// history page by page from the newest post down to the cursor.
func fetchGap(ctx context.Context, cursor int, page historyPageFunc) ([]*tg.Message, error) {
	var out []*tg.Message // newest first while reading
	offset := 0
	for pages := 1; ; pages++ {
		msgs, err := page(ctx, offset)
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			break
		}
		reached := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].ID <= cursor {
				reached = true
				break
			}
			if offset == 0 || msgs[i].ID < offset {
				out = append(out, msgs[i])
			}
		}
		oldest := msgs[0].ID
		if reached || oldest <= 1 || (offset != 0 && oldest >= offset) {
			break
		}
		if pages%10 == 0 {
			log.Printf("catch-up: read %d missed post(s) so far, down to post %d", len(out), oldest)
		}
		offset = oldest
	}
	slices.Reverse(out)
	return out, nil
}

// preloadHandled marks the (post, CA) pairs already recorded above cursor as
// handled, so the catch-up does not handle them again.
func (s *scanner) preloadHandled(ctx context.Context, cursor int) error {
	if s.db == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := s.db.SelectScoutCallPostCAs(qctx, s.sourceChannelID, cursor)
	if err != nil {
		return err
	}
	s.postMu.Lock()
	defer s.postMu.Unlock()
	for _, r := range rows {
		s.handled[handledKey(r.MessageID, r.ContractAddress)] = true
	}
	return nil
}

// recorded reports whether every (post, CA) pair of a post with a CA was
// already handled (or is already in the database).
func (s *scanner) recorded(m *tg.Message) bool {
	cas := extractCAs(m.Message, postURLs(m), s.cfg.Chains)
	if len(cas) == 0 {
		return false
	}
	s.postMu.Lock()
	defer s.postMu.Unlock()
	for _, ca := range cas {
		if !s.handled[handledKey(m.ID, ca)] {
			return false
		}
	}
	return true
}

// storeOnlyPost records a missed post the way -backfill does (a call gets
// status backfill and a tracking row, an update post status update; no bot
// scans, no delivery, the token is not marked as investigated) and marks its
// (post, CA) pairs handled, so a later edit or poll of the post does nothing.
// It records nothing when the database is off. When a write fails (any
// error: database down, shutdown, reconnect) the pair is not marked handled
// and the error is returned: the caller stops there without moving the
// cursor, so the next catch-up stores the post.
func (s *scanner) storeOnlyPost(ctx context.Context, m *tg.Message) error {
	s.postMu.Lock()
	defer s.postMu.Unlock()
	urls := postURLs(m)
	cas := extractCAs(m.Message, urls, s.cfg.Chains)
	if len(cas) == 0 {
		s.handled[strconv.Itoa(m.ID)] = true
		return nil
	}
	for _, ca := range cas {
		k := handledKey(m.ID, ca)
		if s.handled[k] {
			continue
		}
		s.handled[k] = true
		// update posts get status update; "created" is not needed here
		if _, _, err := s.storeCall(ctx, m, ca, urls, CallStatusBackfill); err != nil {
			delete(s.handled, k)
			return fmt.Errorf("storing post %d: %w", m.ID, err)
		}
	}
	return nil
}

// catchUp handles the posts newer than cursor (see the top of this file) and
// returns the newest post id seen. Nothing is handled when reading fails, so
// the caller can simply try again. When ctx is cancelled half way, or a
// stored-only post cannot be written, it stops and returns the error without
// moving the cursor: the posts handled so far are in the database (or in
// s.handled) and are skipped by the next catch-up.
func (s *scanner) catchUp(ctx context.Context, cursor int, page historyPageFunc) (int, error) {
	if err := s.preloadHandled(ctx, cursor); err != nil {
		return cursor, err
	}
	gap, err := fetchGap(ctx, cursor, page)
	if err != nil {
		return cursor, err
	}
	var todo []*tg.Message
	for _, m := range gap {
		if !s.recorded(m) {
			todo = append(todo, m)
		}
	}
	stored, live := planCatchUp(todo, s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, time.Now())
	for _, m := range stored {
		if err := ctx.Err(); err != nil {
			return cursor, err
		}
		if err := s.storeOnlyPost(ctx, m); err != nil {
			return cursor, err
		}
	}
	for _, m := range live {
		if err := ctx.Err(); err != nil {
			return cursor, err
		}
		s.onChannelPost(m)
	}
	line := fmt.Sprintf("catch-up: %d post(s) since post %d (%d handled live, %d stored only", len(todo), cursor, len(live), len(stored))
	if n := len(gap) - len(todo); n > 0 {
		line += fmt.Sprintf("; %d already recorded, skipped", n)
	}
	line += ")"
	if s.db == nil && len(stored) > 0 {
		line += "; SCOUT_DB=off, so the stored-only posts were not recorded"
	}
	log.Print(line)
	newest := cursor
	if len(gap) > 0 {
		newest = max(newest, gap[len(gap)-1].ID)
	}
	s.saveCursor(newest)
	return newest, nil
}

// requeue puts back in the job queue the source channel's calls that were
// queued before a restart (or dropped because the queue was full) and never
// got a report: the in-memory queue does not survive a restart, and
// seen_cas.json already holds their tokens, so neither the catch-up nor
// polling would pick them up again. Only calls posted within requeueWindow
// are looked at; older ones are left as they are and counted in one log line
// per process. The catch-up's rules apply: the newest CatchUpMax calls no
// older than CatchUpMaxAge are queued again (oldest first), the others are
// stored only (status backfill; the token's investigation is not used up). A
// call whose token another call already investigated, or is queued for in
// this process, becomes a duplicate. Several calls of one token (posts
// recorded while seen_cas.json was lost) count as one, the newest (if any of
// them is new enough to be scanned, it is): the others follow it (duplicates
// of its scan, or stored only with it), so the token is scanned once and its
// seen_cas.json entry is never freed while its scan is queued. Calls still in this process's queue (or being processed) are left
// alone, so a reconnect never queues a call twice. It does nothing when the
// database is off.
func (s *scanner) requeue(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	window := requeueWindow(s.cfg.CatchUpMaxAge)
	since := time.Now().Add(-window)
	// postMu is held from the read to the last push: the worker takes a call
	// out of s.pending only after its final status is written, so a call read
	// as queued here is either still pending or really unprocessed.
	s.postMu.Lock()
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rows, older, err := s.db.SelectScoutCallsToRequeue(qctx, s.sourceChannelID, since)
	cancel()
	if err != nil {
		s.postMu.Unlock()
		return fmt.Errorf("requeue: %w", err)
	}
	noteOlder := older > 0 && !s.olderNoted
	s.olderNoted = s.olderNoted || older > 0

	busy := map[string]bool{} // tokens of the calls in this process's queue
	for _, c := range rows {
		if s.pending[c.ID] {
			busy[caKey(c.ContractAddress)] = true
		}
	}
	var todo, dups []ScoutCallToRequeue
	olderOf := map[string][]ScoutCallToRequeue{} // per token in todo: its older calls
	// newest first: a token's newest call goes in todo
	for i := len(rows) - 1; i >= 0; i-- {
		c := rows[i]
		k := caKey(c.ContractAddress)
		_, inTodo := olderOf[k]
		switch {
		case s.pending[c.ID]:
		case c.CAInvestigated, busy[k]:
			dups = append(dups, c)
		case inTodo:
			olderOf[k] = append(olderOf[k], c)
		default:
			olderOf[k] = nil
			todo = append(todo, c)
		}
	}
	slices.Reverse(todo) // oldest first again
	slices.Reverse(dups)
	posts := make([]*tg.Message, 0, len(todo))
	for _, c := range todo {
		posts = append(posts, &tg.Message{ID: c.MessageID, Date: int(c.MessageDate.Unix())})
	}
	storedPosts, _ := planCatchUp(posts, s.cfg.CatchUpMax, s.cfg.CatchUpMaxAge, time.Now())
	stored, live := slices.Clone(todo[:len(storedPosts)]), todo[len(storedPosts):]
	for _, c := range todo[:len(storedPosts)] {
		stored = append(stored, olderOf[caKey(c.ContractAddress)]...)
	}
	for _, c := range live {
		dups = append(dups, olderOf[caKey(c.ContractAddress)]...)
	}
	// like the catch-up's stored-only posts: a later edit or poll of these
	// posts does nothing (their tokens are freed below)
	for _, c := range slices.Concat(stored, dups) {
		s.handled[handledKey(c.MessageID, c.ContractAddress)] = true
	}
	pushed := 0
	var dropped []int
	for _, c := range live {
		s.handled[handledKey(c.MessageID, c.ContractAddress)] = true
		s.seen.markNew(c.ContractAddress) // normally already there (false then); it is this call's token either way
		// dropped -> queued is written before the push, under postMu: once
		// pushed, the worker may write the call's final status at any moment,
		// and a later write here would overwrite it
		if c.Status == CallStatusDropped {
			s.setStatus(ctx, c.ID, CallStatusQueued)
		}
		id := c.ID
		select {
		case s.queue <- job{CA: c.ContractAddress, SourceMsg: c.MessageID, SourceText: c.MessageText, CallID: &id, Meta: metaOf(c.MessageText)}:
			s.pending[id] = true
			pushed++
		default:
			dropped = append(dropped, id)
		}
	}
	s.postMu.Unlock()

	// Written after the unlock, to keep database writes out of postMu (live
	// posts wait on it). Nothing races with these writes: a call that was not
	// pushed is not in s.pending and no worker has it, and requeue runs from
	// the one poll goroutine only (see poll in main.go).
	for _, id := range dropped {
		s.setStatus(ctx, id, CallStatusDropped)
	}
	for _, c := range stored {
		s.setStatus(ctx, c.ID, CallStatusBackfill)
		s.seen.forget(c.ContractAddress)
	}
	for _, c := range dups {
		s.setStatus(ctx, c.ID, CallStatusDuplicate)
	}
	if noteOlder {
		log.Printf("requeue: %d queued or dropped call(s) posted before %s (older than %s) left as they are; DEPLOY.md 3.3 has the SQL to fix them by hand",
			older, since.UTC().Format(time.RFC3339), window)
	}
	n := len(stored) + len(live) + len(dups)
	if n == 0 {
		return nil
	}
	line := fmt.Sprintf("requeue: %d queued call(s) from before the restart (%d scanned now, %d stored only", n, pushed, len(stored))
	if len(dropped) > 0 {
		line += fmt.Sprintf("; %d dropped, the queue is full", len(dropped))
	}
	if len(dups) > 0 {
		line += fmt.Sprintf("; %d duplicate(s), the token was investigated by another call", len(dups))
	}
	log.Print(line + ")")
	return nil
}

// setStatus sets a call's status; a failure is logged (the call keeps its old
// status and a later requeue looks at it again).
func (s *scanner) setStatus(ctx context.Context, id int, status string) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.db.UpdateScoutCallStatus(qctx, id, status); err != nil {
		log.Printf("requeue: setting call %d to %s: %v", id, status, err)
	}
}

// pollBatch is how many new posts one regular poll asks for; a full batch
// means there may be more, and they are read page by page (fetchGap).
const pollBatch = 50

// pollSource is what polling reads from the source channel: tgPollSource in
// the listener, a fake in tests.
type pollSource interface {
	// newest returns the channel's newest post id; ok is false for an empty channel.
	newest(ctx context.Context) (id int, ok bool, err error)
	// since returns up to limit posts newer than minID, oldest first; full
	// reports that limit messages came back, so there may be more.
	since(ctx context.Context, minID, limit int) (msgs []*tg.Message, full bool, err error)
	// page is one page of history (historyPageFunc).
	page(ctx context.Context, offsetID int) ([]*tg.Message, error)
}

type tgPollSource struct{ s *scanner }

func (t tgPollSource) newest(ctx context.Context) (int, bool, error) {
	top, err := t.s.fetchNewPosts(ctx, 0, 1)
	if err != nil || len(top) == 0 {
		return 0, false, err
	}
	return top[len(top)-1].ID, true, nil
}

func (t tgPollSource) since(ctx context.Context, minID, limit int) ([]*tg.Message, bool, error) {
	res, err := t.s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: t.s.sourcePeer, MinID: minID, Limit: limit})
	if err != nil {
		return nil, false, err
	}
	// counted before messagesOf drops service messages: a full page of them
	// still means there may be more
	return messagesOf(res), rawMessageCount(res) >= limit, nil
}

func (t tgPollSource) page(ctx context.Context, offsetID int) ([]*tg.Message, error) {
	return t.s.historyPage(ctx, offsetID)
}

// rawMessageCount is the number of messages of any kind in a history result.
func rawMessageCount(res tg.MessagesMessagesClass) int {
	switch m := res.(type) {
	case *tg.MessagesChannelMessages:
		return len(m.Messages)
	case *tg.MessagesMessagesSlice:
		return len(m.Messages)
	case *tg.MessagesMessages:
		return len(m.Messages)
	}
	return 0
}

// poller is the state of one poll goroutine (see poll in main.go).
type poller struct {
	s          *scanner
	src        pollSource
	resolved   bool   // the resume point was read
	started    bool   // requeue and catch-up done; regular polling from here
	requeueDue bool   // the requeue after the catch-up failed: the next tick tries it again
	cursor     int    // every post up to this one was handled
	from       string // where the resume point came from (for the log)

	requeueFn func(context.Context) error // nil = s.requeue (tests replace it)
}

func (p *poller) requeue(ctx context.Context) error {
	if p.requeueFn != nil {
		return p.requeueFn(ctx)
	}
	return p.s.requeue(ctx)
}

// begin reads the resume point, queues again the calls left queued before the
// restart and catches up the missed posts. On an error nothing is skipped:
// the next call starts again from the step that failed. When only the second
// requeue (after the catch-up) fails, polling has started and every tick
// tries that requeue again until it works.
func (p *poller) begin(ctx context.Context) error {
	if !p.resolved {
		c, from, err := p.s.resumeCursor(ctx)
		if err != nil {
			return fmt.Errorf("catch-up: %w", err)
		}
		p.cursor, p.from, p.resolved = c, from, true
		if c == 0 {
			p.from = "the channel's newest post, no saved cursor"
		}
	}
	if p.cursor == 0 {
		// nothing saved (first start, empty database): nothing to catch up
		id, ok, err := p.src.newest(ctx)
		if err != nil {
			return fmt.Errorf("poll: initial read of @%s: %w", p.s.cfg.SourceChannel, err)
		}
		if ok {
			p.cursor = id
			p.s.saveCursor(id)
		}
		p.started = true
		return nil
	}
	// the calls queued before the restart are older than the missed posts:
	// they go first
	if err := p.requeue(ctx); err != nil {
		return err
	}
	c, err := p.s.catchUp(ctx, p.cursor, p.src.page)
	if err != nil {
		return fmt.Errorf("catch-up after post %d: %w", p.cursor, err)
	}
	p.cursor, p.started = c, true
	// again after the catch-up: calls it could not queue (queue full) get one
	// more try; the ones queued in this process are skipped
	p.requeueDue = true
	return p.requeueAgain(ctx)
}

// requeueAgain runs the requeue after the catch-up while it is due.
func (p *poller) requeueAgain(ctx context.Context) error {
	if !p.requeueDue {
		return nil
	}
	if err := p.requeue(ctx); err != nil {
		return fmt.Errorf("%w (after the catch-up)", err)
	}
	p.requeueDue = false
	return nil
}

// tick is one poll: the start again while it has not succeeded, otherwise
// the requeue after the catch-up if it failed before, and the posts newer than
// the cursor (also when that requeue fails again). A full batch is read page
// by page, so no post is skipped when more than pollBatch arrive in one
// interval.
func (p *poller) tick(ctx context.Context) error {
	if !p.started {
		return p.begin(ctx)
	}
	rqErr := p.requeueAgain(ctx)
	return errors.Join(rqErr, p.pollNew(ctx))
}

// pollNew handles the posts newer than the cursor.
func (p *poller) pollNew(ctx context.Context) error {
	if p.cursor == 0 {
		id, ok, err := p.src.newest(ctx)
		if err != nil {
			return fmt.Errorf("poll: reading the newest post of @%s: %w", p.s.cfg.SourceChannel, err)
		}
		if ok {
			p.cursor = id
			p.s.saveCursor(id)
		}
		return nil
	}
	msgs, full, err := p.src.since(ctx, p.cursor, pollBatch)
	if err != nil {
		return fmt.Errorf("poll: %w", err)
	}
	if full {
		if msgs, err = fetchGap(ctx, p.cursor, p.src.page); err != nil {
			return fmt.Errorf("poll: %d or more new posts since post %d, reading them page by page: %w", pollBatch, p.cursor, err)
		}
		log.Printf("poll: %d new post(s) since post %d (more than one batch of %d, read page by page)", len(msgs), p.cursor, pollBatch)
	}
	prev := p.cursor
	for _, m := range msgs {
		if ctx.Err() != nil {
			break
		}
		p.s.onChannelPost(m) // no-op if the live update already handled it
		p.cursor = max(p.cursor, m.ID)
	}
	if p.cursor > prev {
		p.s.saveCursor(p.cursor)
	}
	return nil
}
