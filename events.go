package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Live updates for the open page.
//
//   - The listener sends NOTIFY scout_events with a small JSON payload when it
//     records a new call or a completed Perceptor / sAlpha report
//     (ScoutStore.notifyScoutEvent, called from the insert functions).
//   - The website (-web) LISTENs on its own connection; a notification makes it
//     refresh its snapshot at once, through the one refresh mechanism (so it is
//     coalesced with the background loop and "Refresh now").
//   - Every refresh (whatever started it) compares the new snapshot with the one
//     before and publishes the differences as Server-Sent Events on
//     GET /api/events: "call" (a new token row), "report" (a token's Perceptor
//     verdict or sAlpha report changed) and "reload" (too many changes at once,
//     or events were missed). Without LISTEN the page still gets them, only as
//     late as the next regular refresh.
//
// Requests are still answered from the snapshot only: the events carry rows
// of a snapshot already in place.
// ---------------------------------------------------------------------------

// scoutEventsChannel is the Postgres NOTIFY channel.
const scoutEventsChannel = "scout_events"

// scoutEventMaxPayload: Postgres refuses payloads of 8000 bytes or more.
const scoutEventMaxPayload = 7999

// Kinds of notification.
const (
	scoutEventCall   = "call"
	scoutEventReport = "report"
)

// scoutEvent is the payload of a notification. The website does not need it to
// work out what changed (it compares snapshots); it is there for anyone
// listening with psql, and for the logs.
type scoutEvent struct {
	Kind   string `json:"kind"`
	CallID int    `json:"call_id"`
	Tool   string `json:"tool,omitempty"` // report: perceptor | salpha
	ID     int    `json:"id,omitempty"`   // report: the investigation id
}

// payload returns the notification text: JSON, under scoutEventMaxPayload bytes.
func (e scoutEvent) payload() (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encode event: %w", err)
	}
	if len(b) > scoutEventMaxPayload {
		return "", fmt.Errorf("event payload of %d bytes is too large", len(b))
	}
	return string(b), nil
}

// scoutReportEvent returns the notification for an investigation just stored:
// only a completed Perceptor or sAlpha report of a call has one.
func scoutReportEvent(r *ScoutInvestigation, toolCode *string) (scoutEvent, bool) {
	if r == nil || r.CallID == nil || r.ID == nil || toolCode == nil || r.Status != investigationCompleted {
		return scoutEvent{}, false
	}
	if *toolCode != webToolPerceptor && *toolCode != webToolSAlpha {
		return scoutEvent{}, false
	}
	return scoutEvent{Kind: scoutEventReport, CallID: *r.CallID, Tool: *toolCode, ID: *r.ID}, true
}

// scoutNotifyLog: at most one "could not notify" line a minute, for all stores.
var scoutNotifyLog = struct {
	mu sync.Mutex
	l  logLimiter
}{l: logLimiter{every: time.Minute}}

// notifyScoutEvent sends NOTIFY scout_events right after an insert. It never
// fails the insert: an error is only logged (at most once a minute), and an
// open website then shows the row with its next regular refresh.
func (st *ScoutStore) notifyScoutEvent(ctx context.Context, ev scoutEvent) {
	payload, err := ev.payload()
	if err == nil {
		_, err = st.Pool.Exec(ctx, `SELECT pg_notify($1, $2)`, scoutEventsChannel, payload)
	}
	if err == nil {
		return
	}
	scoutNotifyLog.mu.Lock()
	ok := scoutNotifyLog.l.allow(time.Now())
	scoutNotifyLog.mu.Unlock()
	if ok {
		log.Printf("db: notify %s (%s, call %d): %v — an open website shows it with its next refresh", scoutEventsChannel, ev.Kind, ev.CallID, err)
	}
}

// ---------------------------------------------------------------------------
// Snapshot differences → events
// ---------------------------------------------------------------------------

// webEventsMaxPerRefresh: more changes than this in one refresh (a backfill, a
// restart of the listener) are sent as one "reload" event instead.
const webEventsMaxPerRefresh = 20

// webEventHorizon: the window whose three numbers (return, peak, worst drop)
// the row of an event carries; the page asks for the list again when it shows
// another window.
const webEventHorizon = "1d"

// webEventOut is one event to publish: its SSE event name and its data (JSON
// on one line).
type webEventOut struct {
	kind string
	data []byte
}

// webSnapshotEvents compares two snapshots and returns the events for what
// changed: a "call" for each new row, a "report" for each row whose Perceptor
// verdict or report, or sAlpha report (or decline), changed. Rows that went
// away send nothing. More than webEventsMaxPerRefresh events become one
// "reload". Both snapshots are in place already and are only read.
func webSnapshotEvents(prev, next *webSnapshot) []webEventOut {
	if prev == nil || next == nil || prev.version == next.version {
		return nil
	}
	type change struct {
		pos  int32
		tool string // "" = a new row
	}
	var changes []change
	i, j := 0, 0
	for j < next.n {
		switch {
		case i < prev.n && prev.ids[i] < next.ids[j]:
			i++ // a row that went away
		case i >= prev.n || prev.ids[i] > next.ids[j]:
			changes = append(changes, change{pos: int32(j)})
			j++
		default: // the same row
			pb, nb := prev.flags[i]&^webFlagUSD, next.flags[j]&^webFlagUSD
			if pb != nb || (next.percID[j] != 0 && next.percID[j] != prev.percID[i]) {
				changes = append(changes, change{pos: int32(j), tool: webToolPerceptor})
			}
			if next.salphaID[j] != 0 && next.salphaID[j] != prev.salphaID[i] {
				changes = append(changes, change{pos: int32(j), tool: webToolSAlpha})
			}
			i++
			j++
		}
	}
	if len(changes) == 0 {
		return nil
	}
	if len(changes) > webEventsMaxPerRefresh {
		calls := 0
		for _, c := range changes {
			if c.tool == "" {
				calls++
			}
		}
		data := fmt.Appendf(nil, `{"calls":%d,"reports":%d}`, calls, len(changes)-calls)
		return []webEventOut{{kind: "reload", data: data}}
	}
	h, _ := webHorizonIndex(webEventHorizon) // a constant of ScoutWebHorizons (TestWebSnapshotEvents checks the rows)
	out := make([]webEventOut, 0, len(changes))
	for _, c := range changes {
		id := int(next.ids[c.pos])
		b := make([]byte, 0, 1200)
		b = append(b, `{"call_id":`...)
		b = strconv.AppendInt(b, int64(id), 10)
		kind := scoutEventCall
		if c.tool != "" {
			kind = scoutEventReport
			b = append(b, `,"tool":"`...)
			b = append(b, c.tool...)
			b = append(b, '"')
		}
		b = append(b, `,"horizon":"`+webEventHorizon+`","row":`...)
		b = next.appendRow(b, c.pos, h)
		b = append(b, '}')
		out = append(out, webEventOut{kind: kind, data: b})
	}
	return out
}

// ---------------------------------------------------------------------------
// The hub: the open event streams
// ---------------------------------------------------------------------------

const (
	webEventsMaxClients = 50               // more get 503 (the page then polls)
	webEventsClientBuf  = 64               // events waiting per client; a client that falls this far behind is dropped
	webEventsReplay     = 128              // recent events kept for Last-Event-ID
	webSSEHeartbeat     = 25 * time.Second // a comment line keeps proxies from closing a quiet stream
	webSSEWriteTimeout  = 10 * time.Second // one write to a client
	webSSERetryMS       = 5000             // the browser reconnects after this
	webNotifyDebounce   = time.Second      // notifications this close together make one refresh
)

// errWebEventsFull and errWebEventsClosed: why a stream was refused.
var (
	errWebEventsFull   = errors.New("too many live connections")
	errWebEventsClosed = errors.New("the website is stopping")
)

// webEvent is one published event with its id ("<epoch>-<seq>") and the SSE
// frame as it is sent.
type webEvent struct {
	seq   uint64
	kind  string
	frame []byte
}

// webEventClient is one open stream. The hub owns it: it is in clients until
// it is dropped, and gone is closed (once, under the hub's lock) when it is.
type webEventClient struct {
	ch      chan *webEvent
	gone    chan struct{}
	dropped bool // guarded by the hub's mu
}

// webEventHub fans published events out to the open streams. Publishing never
// blocks: a client whose buffer is full is dropped (its stream ends; the
// browser reconnects and replays from Last-Event-ID).
type webEventHub struct {
	mu      sync.Mutex
	epoch   string // differs per process, so ids of an older process are recognised
	seq     uint64
	recent  []*webEvent // the last webEventsReplay events, oldest first
	clients map[*webEventClient]struct{}
	max     int
	buf     int
	closed  bool
	dropped int // clients dropped for being slow (for tests and logs)
}

func newWebEventHub(maxClients, buf int) *webEventHub {
	return &webEventHub{
		epoch:   strconv.FormatInt(time.Now().UnixMilli(), 36),
		clients: map[*webEventClient]struct{}{},
		max:     maxClients,
		buf:     buf,
	}
}

// parseEventID splits an id of this hub into its sequence number; ok is false
// for an id of another process or one not written by a hub.
func (h *webEventHub) parseEventID(id string) (uint64, bool) {
	epoch, seq, found := strings.Cut(strings.TrimSpace(id), "-")
	if !found || epoch != h.epoch {
		return 0, false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// subscribe opens a stream. lastID is the Last-Event-ID the browser sent ("" on
// a first connection). replay holds the kept events after lastID; missed is
// true when events after lastID can no longer be replayed (another process, or
// too long ago), and the page should reload its list.
func (h *webEventHub) subscribe(lastID string) (c *webEventClient, replay []*webEvent, missed bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, false, errWebEventsClosed
	}
	if len(h.clients) >= h.max {
		return nil, nil, false, errWebEventsFull
	}
	c = &webEventClient{ch: make(chan *webEvent, h.buf), gone: make(chan struct{})}
	h.clients[c] = struct{}{}
	if strings.TrimSpace(lastID) == "" {
		return c, nil, false, nil
	}
	last, ok := h.parseEventID(lastID)
	if !ok || last > h.seq {
		return c, nil, true, nil
	}
	if last < h.seq && (len(h.recent) == 0 || h.recent[0].seq > last+1) {
		missed = true // some of the events after last are no longer kept
	}
	for _, ev := range h.recent {
		if ev.seq > last {
			replay = append(replay, ev)
		}
	}
	return c, replay, missed, nil
}

// dropLocked ends a client's stream. h.mu is held.
func (h *webEventHub) dropLocked(c *webEventClient) {
	if c.dropped {
		return
	}
	c.dropped = true
	delete(h.clients, c)
	close(c.gone)
}

// unsubscribe removes a client (its stream has ended); safe to call twice.
func (h *webEventHub) unsubscribe(c *webEventClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(c)
}

// publish sends one event to every open stream, without waiting for any.
func (h *webEventHub) publish(kind string, data []byte) {
	if bytes.ContainsAny(data, "\r\n") {
		// a data line must be one line; the encoders here never write a raw line break
		log.Printf("web: live updates: an event of kind %s was not sent (line break in its data)", kind)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.seq++
	frame := make([]byte, 0, len(data)+64)
	frame = append(frame, "id: "...)
	frame = append(frame, h.epoch...)
	frame = append(frame, '-')
	frame = strconv.AppendUint(frame, h.seq, 10)
	frame = append(frame, "\nevent: "...)
	frame = append(frame, kind...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	ev := &webEvent{seq: h.seq, kind: kind, frame: frame}
	if len(h.recent) >= webEventsReplay {
		copy(h.recent, h.recent[1:])
		h.recent = h.recent[:len(h.recent)-1]
	}
	h.recent = append(h.recent, ev)
	for c := range h.clients {
		select {
		case c.ch <- ev:
		default:
			h.dropLocked(c) // too slow: its stream ends rather than hold up the others
			h.dropped++
		}
	}
}

// close ends every stream and refuses new ones (the website is stopping).
func (h *webEventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		h.dropLocked(c)
	}
}

// count returns the number of open streams.
func (h *webEventHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// ---------------------------------------------------------------------------
// GET /api/events
// ---------------------------------------------------------------------------

// handleEvents is GET /api/events: a Server-Sent Events stream of the changes
// of the list (see webSnapshotEvents). The stream is served by the request's
// own goroutine; it ends when the client goes away, when the hub drops it
// (too slow) and when the website stops. Not compressed: every event must
// reach the browser at once.
func (s *webServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "requests from other sites are not accepted")
		return
	}
	if r.URL.RawQuery != "" {
		writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
		return
	}
	c, replay, missed, err := s.events.subscribe(r.Header.Get("Last-Event-ID"))
	if err != nil {
		// the page falls back to asking every 30 seconds
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusServiceUnavailable, err.Error()+"; the page refreshes every 30 seconds instead")
		return
	}
	defer s.events.unsubscribe(c)

	rc := http.NewResponseController(w)
	// The server's read and write timeouts are made for ordinary requests; a
	// stream stays open. Each write gets its own deadline below instead.
	// (ErrNotSupported with a test recorder, which has no connection.)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: do not hold the stream back
	w.WriteHeader(http.StatusOK)

	send := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(webSSEWriteTimeout)) // see above
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	first := fmt.Appendf(nil, "retry: %d\n: connected\n\n", webSSERetryMS)
	if missed {
		first = append(first, "event: reload\ndata: {\"missed\":true}\n\n"...)
	}
	for _, ev := range replay {
		first = append(first, ev.frame...)
	}
	if !send(first) {
		return
	}
	every := s.sseHeartbeat
	if every <= 0 {
		every = webSSEHeartbeat
	}
	beat := time.NewTicker(every)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.life.Done():
			return
		case <-c.gone:
			return
		case ev := <-c.ch:
			if !send(ev.frame) {
				return
			}
		case <-beat.C:
			if !send([]byte(": ping\n\n")) {
				return
			}
		}
	}
}

// publishSnapshotEvents publishes what changed between two snapshots. Called by
// readSnapshot right after the new one is in place; it never blocks.
func (s *webServer) publishSnapshotEvents(prev, next *webSnapshot) {
	if s.events == nil {
		return
	}
	for _, ev := range webSnapshotEvents(prev, next) {
		s.events.publish(ev.kind, ev.data)
	}
}

// ---------------------------------------------------------------------------
// LISTEN scout_events → refresh
// ---------------------------------------------------------------------------

// refreshAfter brings the snapshot up to date with everything committed before
// since: it joins the read under way only when that read started after since,
// else it waits for it and starts another. Like refresh, it goes through
// startRefreshLocked, so there is still never more than one read at a time.
func (s *webServer) refreshAfter(ctx context.Context, since time.Time) error {
	s.flightMu.Lock()
	joined := s.inflight != nil
	c := s.startRefreshLocked(false)
	s.flightMu.Unlock()
	err := c.wait(ctx)
	// A read started here began after since. A read joined counts only when it
	// began strictly after since: the clock can be coarse (Windows), and an
	// equal time may be a read that began before the change.
	if !joined || c.started.After(since) || ctx.Err() != nil {
		return err
	}
	// The read joined began before the change: read once more. Any read under
	// way now began after that one ended, so after since.
	s.flightMu.Lock()
	c = s.startRefreshLocked(false)
	s.flightMu.Unlock()
	return c.wait(ctx)
}

// notifyLoop refreshes the snapshot after a kick, at most once per debounce:
// the first kick starts the wait, and the kicks during it are taken in by the
// same refresh. It returns when ctx is cancelled.
func (s *webServer) notifyLoop(ctx context.Context, kick <-chan struct{}, debounce time.Duration) {
	t := time.NewTimer(debounce)
	if !t.Stop() {
		<-t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		t.Reset(debounce)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		// kicks that came during the wait are covered by this refresh
		select {
		case <-kick:
		default:
		}
		_ = s.refreshAfter(ctx, time.Now()) // a failure is logged by readSnapshot; the old snapshot stays
	}
}

// kickNow asks notifyLoop for a refresh, without waiting (one pending kick is enough).
func kickNow(kick chan<- struct{}) {
	select {
	case kick <- struct{}{}:
	default:
	}
}

// webNotifyConn is the part of *pgx.Conn the listener uses (tests use a fake).
type webNotifyConn interface {
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}

// dialScoutEvents returns a dial function that opens a connection of its own
// (not one of the pool's, which has few) and LISTENs on scout_events.
func dialScoutEvents(cfg *pgx.ConnConfig) func(context.Context) (webNotifyConn, error) {
	return func(ctx context.Context) (webNotifyConn, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		conn, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		if _, err := conn.Exec(ctx, "LISTEN "+scoutEventsChannel); err != nil {
			// the LISTEN error is the one worth reporting; the connection is useless either way
			_ = conn.Close(context.Background())
			return nil, fmt.Errorf("listen: %w", err)
		}
		return conn, nil
	}
}

// webListenTimings: how the listener waits; tests make them short.
type webListenTimings struct {
	backoffMin, backoffMax time.Duration // between failed connections
	idle                   time.Duration // a connection quiet this long is pinged
}

var defaultWebListenTimings = webListenTimings{backoffMin: time.Second, backoffMax: time.Minute, idle: 4 * time.Minute}

// listenScoutEvents keeps a LISTEN connection open until ctx is cancelled and
// kicks a refresh for every notification. After every (re)connection it kicks
// once as well: notifications sent while it was away are lost. Failures are
// retried with a growing wait and logged at most once a minute; meanwhile the
// page still gets its events from the regular refreshes.
func listenScoutEvents(ctx context.Context, dial func(context.Context) (webNotifyConn, error), kick chan<- struct{}, tm webListenTimings) {
	backoff := tm.backoffMin
	errLog := logLimiter{every: time.Minute}
	failing := false
	for ctx.Err() == nil {
		conn, err := dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errLog.allow(time.Now()) {
				log.Printf("web: live updates: cannot listen for database events: %v — retrying (the page still updates with every refresh)", err)
			}
			failing = true
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, tm.backoffMax)
			continue
		}
		if failing {
			log.Printf("web: live updates: listening for database events again")
			failing, errLog.last = false, time.Time{}
		}
		backoff = tm.backoffMin
		kickNow(kick)
		err = waitScoutEvents(ctx, conn, kick, tm.idle)
		// the connection is done with either way
		_ = conn.Close(context.Background())
		if ctx.Err() != nil {
			return
		}
		if errLog.allow(time.Now()) {
			log.Printf("web: live updates: lost the database connection for events: %v — reconnecting", err)
		}
		failing = true
	}
}

// waitScoutEvents kicks for every notification on conn until an error (or ctx
// ends). A connection quiet for idle is pinged, so a dead one is noticed.
func waitScoutEvents(ctx context.Context, conn webNotifyConn, kick chan<- struct{}, idle time.Duration) error {
	for {
		wctx, cancel := context.WithTimeout(ctx, idle)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
			err = conn.Ping(pctx)
			pcancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if n != nil && n.Channel == scoutEventsChannel {
			kickNow(kick)
		}
	}
}
