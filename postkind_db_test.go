package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

const updateText = "⚡ $MURKLE hit 3X called $21k → $63k peak since the call · dyor"

// updatePostAt builds an update post about an earlier call: one line of text,
// the contract address only behind a button (the way postAt carries it).
func updatePostAt(id int, at time.Time, ca string) *tg.Message {
	return &tg.Message{ID: id, Date: int(at.Unix()), Message: updateText,
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonURL{Text: "Chart", URL: "https://dexscreener.com/robinhood/" + ca}}}}}}
}

// captureLog sends the log to a buffer until the test ends.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// Without a database: an update post is not queued and does not use up the
// token's one investigation.
func TestOnChannelPostUpdateIsNotQueued(t *testing.T) {
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir()}
	s := newScanner(cfg)
	buf := captureLog(t)
	ca := "0x2222222222222222222222222222222222222222"
	now := time.Now()

	s.onChannelPost(updatePostAt(10408, now, ca))
	// the address as a text link instead of a button
	s.onChannelPost(&tg.Message{ID: 10409, Message: "⚡ $MURKLE hit 2.5x called $21k → $52k",
		Entities: []tg.MessageEntityClass{&tg.MessageEntityTextURL{URL: "https://dexscreener.com/robinhood/" + ca}}})
	if n := len(s.queue); n != 0 {
		t.Fatalf("update posts queued %d job(s)", n)
	}
	for _, want := range []string{"post 10408: update for " + ca + " (not a call), skipping", "post 10409: update for " + ca + " (not a call), skipping"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, buf.String())
		}
	}
	s.onChannelPost(postAt(10410, now, ca)) // the real call, after the updates
	if n := len(s.queue); n != 1 {
		t.Fatalf("real call after update posts: %d job(s) queued, want 1\n%s", n, buf.String())
	}
	if j := <-s.queue; j.CA != ca || j.SourceMsg != 10410 {
		t.Fatalf("job %+v", j)
	}
	s.onChannelPost(updatePostAt(10411, now, ca))
	if n := len(s.queue); n != 0 {
		t.Fatalf("update after the call queued %d job(s)", n)
	}
}

func callByMsg(t *testing.T, st *ScoutStore, ca string, msg int) *ScoutCall {
	t.Helper()
	calls, err := st.SelectScoutCalls(context.Background(), ca, 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		if calls[i].MessageID == msg {
			return &calls[i]
		}
	}
	t.Fatalf("post %d of %s is not recorded (%d rows)", msg, ca, len(calls))
	return nil
}

// Listener with the database: update posts are recorded as updates and nothing
// else; the token's real call is investigated and tracked, whichever came first.
func TestListenerUpdatePosts(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	buf := captureLog(t)
	unknown := "0xaAaAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // its update post arrives before any call
	known := "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"   // called first, update later
	at := time.Now().Add(-3 * time.Hour)

	// 1. update post for a token that was never called
	s.onChannelPost(updatePostAt(100, at, unknown))
	if n := len(s.queue); n != 0 {
		t.Fatalf("update post queued %d job(s)", n)
	}
	u := callByMsg(t, st, unknown, 100)
	if u.PostKind != PostKindUpdate || u.Status != CallStatusUpdate {
		t.Fatalf("update post: kind %q status %q", u.PostKind, u.Status)
	}
	if tr, err := st.GetTracking(ctx, *u.ID); err != nil || tr != nil {
		t.Fatalf("update post has a tracking row: %+v %v", tr, err)
	}
	if m, _ := st.GetCallMetrics(ctx, *u.ID); m != nil {
		t.Fatalf("update post has call metrics: %+v", m)
	}
	if !strings.Contains(buf.String(), "post 100: update for "+unknown+" (not a call), skipping") {
		t.Fatalf("log:\n%s", buf.String())
	}

	// 2. the real call of that token comes later: queued and tracked
	s.onChannelPost(postAt(101, at.Add(time.Hour), unknown))
	if n := len(s.queue); n != 1 {
		t.Fatalf("real call after the update: %d job(s) queued, want 1\n%s", n, buf.String())
	}
	j := <-s.queue
	c := callByMsg(t, st, unknown, 101)
	if j.CallID == nil || *j.CallID != *c.ID || c.PostKind != PostKindCall || c.Status != CallStatusQueued {
		t.Fatalf("real call: job %+v, row kind %q status %q", j, c.PostKind, c.Status)
	}
	if tr, _ := st.GetTracking(ctx, *c.ID); tr == nil || tr.Status != TrackPending {
		t.Fatalf("real call tracking: %+v", tr)
	}

	// 3. a call, then an update post about it
	s.onChannelPost(postAt(102, at, known))
	if n := len(s.queue); n != 1 {
		t.Fatalf("call queued %d job(s)", n)
	}
	<-s.queue
	s.onChannelPost(updatePostAt(103, at.Add(2*time.Hour), known))
	s.onChannelPost(updatePostAt(103, at.Add(2*time.Hour), known)) // seen again by polling
	if n := len(s.queue); n != 0 {
		t.Fatalf("update after a call queued %d job(s)", n)
	}
	u2 := callByMsg(t, st, known, 103)
	if u2.PostKind != PostKindUpdate || u2.Status != CallStatusUpdate {
		t.Fatalf("update after a call: kind %q status %q", u2.PostKind, u2.Status)
	}
	if tr, _ := st.GetTracking(ctx, *u2.ID); tr != nil {
		t.Fatalf("update after a call has a tracking row: %+v", tr)
	}
	if strings.Contains(buf.String(), "already scanned") {
		t.Fatalf("an update post was logged as a duplicate call:\n%s", buf.String())
	}

	// The first-call rule leaves both real calls in the tracker's queue.
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 0 {
		t.Fatalf("MarkRepeatTracking = %d, %v; want 0", n, err)
	}
	var rows, tracked, updates int
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM scout_calls), (SELECT count(*) FROM scout_call_tracking),
		(SELECT count(*) FROM scout_calls WHERE post_kind = 'update' AND status = 'update')`).Scan(&rows, &tracked, &updates); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || tracked != 2 || updates != 2 {
		t.Fatalf("%d rows, %d tracking rows, %d updates; want 4, 2, 2", rows, tracked, updates)
	}
	sum, err := webSummaryOf(ctx, st)
	if err != nil || sum.Imported != 2 || sum.TotalCalls != 2 || sum.RepeatCalls != 0 || sum.UpdatePosts != 2 {
		t.Fatalf("web summary %+v %v", sum, err)
	}
}

// Backfill: update posts are recorded and counted on their own; no tracking row.
func TestBackfillUpdatePosts(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	ca, other := "0x8888888888888888888888888888888888888888", "0x9999999999999999999999999999999999999999"
	at := time.Now().Add(-20 * 24 * time.Hour)
	run := func() backfillStats {
		var bs backfillStats
		// newest first, as the backfill walks the channel
		s.backfillMessage(updatePostAt(503, at.Add(3*time.Hour), other), &bs) // token whose call is not in the range
		s.backfillMessage(updatePostAt(502, at.Add(2*time.Hour), ca), &bs)
		s.backfillMessage(&tg.Message{ID: 501, Message: "gm"}, &bs)
		s.backfillMessage(postAt(500, at, ca), &bs)
		return bs
	}
	bs := run()
	if bs.Posts != 4 || bs.Calls != 1 || bs.CAs != 1 || bs.New != 1 || bs.Existing != 0 || bs.Updates != 2 {
		t.Fatalf("stats %+v", bs)
	}
	for msg, tok := range map[int]string{502: ca, 503: other} {
		u := callByMsg(t, st, tok, msg)
		if u.PostKind != PostKindUpdate || u.Status != CallStatusUpdate {
			t.Fatalf("post %d: kind %q status %q", msg, u.PostKind, u.Status)
		}
		if tr, _ := st.GetTracking(ctx, *u.ID); tr != nil {
			t.Fatalf("post %d has a tracking row: %+v", msg, tr)
		}
	}
	c := callByMsg(t, st, ca, 500)
	if c.PostKind != PostKindCall || c.Status != CallStatusBackfill {
		t.Fatalf("call: kind %q status %q", c.PostKind, c.Status)
	}
	if tr, _ := st.GetTracking(ctx, *c.ID); tr == nil || tr.Status != TrackPending {
		t.Fatalf("call tracking %+v", tr)
	}
	// a second run records nothing new
	if bs := run(); bs.Calls != 1 || bs.New != 0 || bs.Existing != 1 || bs.Updates != 2 {
		t.Fatalf("second run stats %+v", bs)
	}
	var rows, tracked int
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM scout_calls), (SELECT count(*) FROM scout_call_tracking)`).Scan(&rows, &tracked); err != nil || rows != 3 || tracked != 1 {
		t.Fatalf("%d rows, %d tracking rows (%v); want 3 and 1", rows, tracked, err)
	}
	// the backfill does not use up the token's investigation
	if !s.seen.markNew(ca) || !s.seen.markNew(other) {
		t.Fatal("backfill must not mark CAs as investigated")
	}
}

// seedOldPost stores a row the way the program did before post_kind existed:
// any text, post_kind NULL, a tracking row.
func seedOldPost(t *testing.T, st *ScoutStore, msg int, at time.Time, ca, text string) int {
	t.Helper()
	ctx := context.Background()
	id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: msg,
		MessageDate: at, MessageText: text, ContractAddress: ca, Chain: "evm", Status: CallStatusBackfill})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = NULL WHERE id = $1`, *id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTracking(ctx, *id, ca, at, 1, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return *id
}

// Rows from before post_kind existed are classified from their stored text; the
// update posts among them leave the tracker's queue, and where an update post
// was taken for a token's first call, the earliest real call takes its place.
func TestClassifyPostKindsRepairsOldRows(t *testing.T) {
	s, st, _ := trackerFixture(t)
	ctx := context.Background()
	buf := captureLog(t)
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	caA, caB, caC, caD := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"0xcccccccccccccccccccccccccccccccccccccccc", "0xdddddddddddddddddddddddddddddddddddddddd"
	ids := map[string]int{}
	// A: the update post is (wrongly) the earliest row of the token
	ids["a_upd"] = seedOldPost(t, st, 1, base, caA, updateText)
	ids["a_call"] = seedOldPost(t, st, 2, base.Add(time.Hour), caA, samplePost)
	ids["a_call2"] = seedOldPost(t, st, 3, base.Add(2*time.Hour), "0x"+strings.ToUpper(caA[2:]), samplePost)
	// B: call first, update posts later
	ids["b_call"] = seedOldPost(t, st, 4, base, caB, samplePost)
	ids["b_upd"] = seedOldPost(t, st, 5, base.Add(time.Hour), caB, "⚡ $B hit 2.5x called $10k → $25k")
	ids["b_upd_done"] = seedOldPost(t, st, 6, base.Add(2*time.Hour), caB, "⚡ $B hit 10 X called $10k → $100k")
	// C: only an update post is stored
	ids["c_upd"] = seedOldPost(t, st, 7, base, caC, updateText)
	// D: a post without text (the address was behind a button): a call
	ids["d_call"] = seedOldPost(t, st, 8, base, caD, "")

	// The state the old program left behind: first row per token tracked, the rest repeat.
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 4 {
		t.Fatalf("MarkRepeatTracking before = %d, %v; want 4", n, err)
	}
	wantStatuses(t, "before", trackingStatuses(t, st, ids), map[string]string{"a_upd": TrackPending, "a_call": TrackRepeat, "a_call2": TrackRepeat,
		"b_call": TrackPending, "b_upd": TrackRepeat, "b_upd_done": TrackRepeat, "c_upd": TrackPending, "d_call": TrackPending})
	for k, status := range map[string]string{"a_upd": TrackTracking, "b_upd_done": TrackDone, "c_upd": TrackError} {
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = $2, entry_price_usd = 2, attempts = 3,
			error = CASE WHEN $2 = 'error' THEN 'rpc: boom' END WHERE call_id = $1`, ids[k], status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() + interval '5 days' WHERE call_id = $1`, ids["a_call"]); err != nil {
		t.Fatal(err)
	}
	var viewRows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_dataset_v`).Scan(&viewRows); err != nil || viewRows != 8 {
		t.Fatalf("dataset view rows before: %d %v", viewRows, err)
	}
	if sum, err := webSummaryOf(ctx, st); err != nil || sum.Imported != 4 || sum.TotalCalls != 8 || sum.RepeatCalls != 4 || sum.UpdatePosts != 0 {
		t.Fatalf("web summary before: %+v %v", sum, err)
	}

	calls, updates, err := st.ClassifyPostKinds(ctx)
	if err != nil || calls != 4 || updates != 4 {
		t.Fatalf("ClassifyPostKinds = %d calls, %d updates, %v; want 4, 4", calls, updates, err)
	}
	for k, want := range map[string]string{"a_upd": PostKindUpdate, "a_call": PostKindCall, "a_call2": PostKindCall, "b_call": PostKindCall,
		"b_upd": PostKindUpdate, "b_upd_done": PostKindUpdate, "c_upd": PostKindUpdate, "d_call": PostKindCall} {
		c, err := st.GetScoutCall(ctx, ids[k])
		if err != nil || c == nil || c.PostKind != want || c.Status != CallStatusBackfill {
			t.Fatalf("%s: %+v %v; want kind %q, status unchanged", k, c, err, want)
		}
	}
	// nothing left: nothing counted, nothing written
	if calls, updates, err := st.ClassifyPostKinds(ctx); err != nil || calls != 0 || updates != 0 {
		t.Fatalf("second ClassifyPostKinds = %d, %d, %v", calls, updates, err)
	}

	// a_upd (tracking), c_upd (error) → repeat; a_call (repeat) → pending. b_upd is a repeat already.
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 3 {
		t.Fatalf("MarkRepeatTracking after = %d, %v; want 3", n, err)
	}
	wantStatuses(t, "after", trackingStatuses(t, st, ids), map[string]string{"a_upd": TrackRepeat, "a_call": TrackPending, "a_call2": TrackRepeat,
		"b_call": TrackPending, "b_upd": TrackRepeat, "b_upd_done": TrackDone, "c_upd": TrackRepeat, "d_call": TrackPending})
	a, _ := st.GetTracking(ctx, ids["a_call"])
	if a.NextCheckAt.After(time.Now().Add(time.Second)) || a.Attempts != 0 {
		t.Fatalf("the real first call is not due now: %+v", a)
	}
	if c, _ := st.GetTracking(ctx, ids["c_upd"]); c.Error != nil {
		t.Fatalf("error not cleared on the update post's row: %q", *c.Error)
	}
	if d, _ := st.GetTracking(ctx, ids["b_upd_done"]); d.EntryPriceUSD == nil || *d.EntryPriceUSD != 2 {
		t.Fatalf("done row lost its result: %+v", d)
	}
	if n, err := st.MarkRepeatTracking(ctx); err != nil || n != 0 {
		t.Fatalf("MarkRepeatTracking again = %d, %v; want 0", n, err)
	}
	due, err := st.DueTracking(ctx, time.Now(), 50)
	if err != nil {
		t.Fatal(err)
	}
	gotDue := map[int]bool{}
	for _, d := range due {
		gotDue[d.CallID] = true
	}
	if len(due) != 3 || !gotDue[ids["a_call"]] || !gotDue[ids["b_call"]] || !gotDue[ids["d_call"]] {
		t.Fatalf("due = %v, want the real first calls %d %d %d", gotDue, ids["a_call"], ids["b_call"], ids["d_call"])
	}

	// Website numbers: real calls only. Dataset view: same rows, with the kind.
	sum, err := webSummaryOf(ctx, st)
	if err != nil || sum.Imported != 3 || sum.TotalCalls != 4 || sum.RepeatCalls != 1 || sum.UpdatePosts != 4 {
		t.Fatalf("web summary after: %+v %v", sum, err)
	}
	var updRows, callRows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE post_kind = 'update'), count(*) FILTER (WHERE post_kind = 'call')
		FROM scout_call_dataset_v`).Scan(&viewRows, &updRows, &callRows); err != nil || viewRows != 8 || updRows != 4 || callRows != 4 {
		t.Fatalf("dataset view after: %d rows, %d update, %d call (%v)", viewRows, updRows, callRows, err)
	}
	row, err := st.DatasetRowJSON(ctx, ids["a_upd"])
	if err != nil || !strings.Contains(string(row), `"post_kind":"update"`) {
		t.Fatalf("dataset row of an update post: %s %v", row, err)
	}
	if strings.Contains(buf.String(), "posts:") {
		t.Fatalf("the store logged:\n%s", buf.String())
	}

	// The tracker does the same on its own at the start of a cycle, in batches,
	// and says so once.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = NULL;
		INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date, message_text, contract_address, chain, status, created_by, updated_by)
		SELECT gen_random_uuid(), 777, 'scoutrobinhood', 1000 + g, now() - interval '1 day' + g * interval '1 second',
		       CASE WHEN g % 10 = 0 THEN '⚡ $T' || g || ' hit ' || g || 'X called $1k' ELSE 'call ' || g END,
		       '0x' || lpad(to_hex(g), 40, '0'), 'evm', 'backfill', 't', 't'
		FROM generate_series(1, 2500) g`); err != nil {
		t.Fatal(err)
	}
	s.trackDue(ctx, 1)
	s.trackDue(ctx, 1)
	line := "posts: 2,254 call(s), 254 update(s) classified"
	if strings.Count(buf.String(), line) != 1 || strings.Count(buf.String(), "posts:") != 1 {
		t.Fatalf("want %q exactly once in:\n%s", line, buf.String())
	}
	var null, upd int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE post_kind IS NULL), count(*) FILTER (WHERE post_kind = 'update') FROM scout_calls`).Scan(&null, &upd); err != nil || null != 0 || upd != 254 {
		t.Fatalf("%d rows without a kind, %d updates (%v)", null, upd, err)
	}
}

// Website: update posts are never listed and never counted as calls.
func TestWebLeavesOutUpdatePosts(t *testing.T) {
	fx := newWebFixture(t, webConfig{GMGNTemplate: defaultGMGNTemplate})
	seedWebRepeats(t, fx)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	caOnly := "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	upd := func(key string, msg int, at time.Duration, ca, status string) {
		id, err := fx.st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: msg,
			MessageDate: base.Add(at), MessageText: updateText, ContractAddress: ca, Chain: "evm", Status: status})
		if err != nil {
			t.Fatal(err)
		}
		fx.ids[key] = *id
	}
	upd("alpha_upd", 40, 60*time.Hour, caAlpha, CallStatusUpdate) // after alpha's calls: would move last_call_date
	upd("only_upd", 42, 61*time.Hour, caOnly, CallStatusUpdate)   // a token with no call at all
	// an old row: update text stored before the column existed, earlier than beta's call, with a tracking row
	upd("beta_upd_early", 41, -5*time.Hour, caBeta, CallStatusBackfill)
	if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = NULL WHERE id = $1`, fx.ids["beta_upd_early"]); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.EnsureTracking(ctx, fx.ids["beta_upd_early"], caBeta, base.Add(-5*time.Hour), 1, base); err != nil {
		t.Fatal(err)
	}
	// Not classified yet: the old row still reads as beta's first call.
	if res := fx.calls(t, "q=Beta"); res.Total != 0 {
		t.Fatalf("before the classification, search Beta: %+v", res)
	}
	if calls, updates, err := fx.st.ClassifyPostKinds(ctx); err != nil || calls != 0 || updates != 1 {
		t.Fatalf("ClassifyPostKinds = %d, %d, %v; want 0, 1", calls, updates, err)
	}

	// The same 10 tokens, the same rows and order as without the update posts.
	all := fx.wantOrder(t, "", "gave", "err", "pct", "sol", "xss", "rep", "virt", "gamma", "beta", "alpha")
	if all.Total != 10 {
		t.Fatalf("total %d, want 10", all.Total)
	}
	by := map[string]webCallJSON{}
	for i, k := range fx.keys(all.Calls) {
		by[k] = all.Calls[i]
	}
	if a := by["alpha"]; a.CallCount != 2 || a.LastCallDate != "2026-09-02T16:00:00Z" {
		t.Fatalf("alpha: call_count %d last_call_date %s; the update post was counted", a.CallCount, a.LastCallDate)
	}
	if b := by["beta"]; b.CallID != fx.ids["beta"] || b.CallCount != 1 || b.MessageDate != "2026-09-01T02:00:00Z" || b.LastCallDate != b.MessageDate {
		t.Fatalf("beta: %+v", b)
	}
	if r := by["rep"]; r.CallCount != 4 {
		t.Fatalf("rep: call_count %d", r.CallCount)
	}
	if res := fx.calls(t, "usd_only=0&q="+caOnly); res.Total != 0 || len(res.Calls) != 0 {
		t.Fatalf("a token with only an update post is listed: %+v", res)
	}
	if res := fx.calls(t, "usd_only=0&per=200&dir=asc"); res.Total != 10 || len(res.Calls) != 10 {
		t.Fatalf("whole list: total %d, %d rows", res.Total, len(res.Calls))
	}

	_, _, body := fx.get(t, "/api/summary")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{"imported": 10, "tracked": 6, "done": 5, "pending": 1, "total_calls": 14, "repeat_calls": 4, "update_posts": 3} {
		if got[k] != want {
			t.Fatalf("summary %s = %v, want %v (%s)", k, got[k], want, body)
		}
	}
	var rows int
	if err := fx.st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls`).Scan(&rows); err != nil || rows != 17 {
		t.Fatalf("scout_calls rows %d (%v), want 17", rows, err)
	}

	// The page's quiet line is built from repeat_calls and update_posts.
	_, _, js := fx.get(t, "/app.js")
	for _, want := range []string{"s.update_posts", "' update post'", "' update posts'", "' repeat call'", "' repeat calls'", "hidden.join(' and ')", "' not shown.'"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js: missing %s", want)
		}
	}
}
