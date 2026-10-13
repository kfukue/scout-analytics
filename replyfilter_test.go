package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

// botMsg is a bot message with id, text and, when replyTo is not 0, a reply
// header naming message replyTo.
func botMsg(id, replyTo int, text string) *tg.Message {
	m := &tg.Message{ID: id, Date: int(time.Now().Unix()), Message: text}
	if replyTo != 0 {
		h := &tg.MessageReplyHeader{}
		h.SetReplyToMsgID(replyTo)
		m.SetReplyTo(h)
	}
	return m
}

func msgIDs(msgs []*tg.Message) []int {
	ids := make([]int, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestStaleReply(t *testing.T) {
	noID := &tg.Message{ID: 205}
	noID.SetReplyTo(&tg.MessageReplyHeader{}) // a reply header without a message id
	otherChat := botMsg(206, 100, "")
	h := otherChat.ReplyTo.(*tg.MessageReplyHeader)
	h.SetReplyToPeerID(&tg.PeerChannel{ChannelID: 9})
	story := &tg.Message{ID: 207}
	story.SetReplyTo(&tg.MessageReplyStoryHeader{})
	cases := []struct {
		name      string
		m         *tg.Message
		floor     int
		wantStale bool
	}{
		{"request id unknown: nothing is filtered", botMsg(150, 100, "x"), 0, false},
		{"no reply_to, sent after the request", botMsg(201, 0, "x"), 200, false},
		{"replies to our request", botMsg(201, 200, "x"), 200, false},
		{"replies to the bot's own later message", botMsg(202, 201, "x"), 200, false},
		{"replies to an earlier request", botMsg(201, 100, "x"), 200, true},
		{"sent before our request", botMsg(199, 0, "x"), 200, true},
		{"reply header without a message id", noID, 200, false},
		{"reply to a message in another chat", otherChat, 200, false},
		{"reply to a story", story, 200, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := staleReply(c.m, c.floor)
			if (why != "") != c.wantStale {
				t.Errorf("staleReply(message %d, reply_to %d, floor %d) = %q, want stale %v", c.m.ID, replyToMsgID(c.m), c.floor, why, c.wantStale)
			}
		})
	}
}

// reqBot is a fake bot for toolRunner tests: the n-th send's request gets
// message id reqs[n] (0 = unknown: noteRequest is not called) and the bot
// sends msgs[n] at once, as Telegram updates would deliver them.
type reqBot struct {
	r    *toolRunner
	reqs []int
	msgs [][]*tg.Message
	sent []string
}

func (b *reqBot) send(ctx context.Context, text string) error {
	i := len(b.sent)
	b.sent = append(b.sent, text)
	if i < len(b.reqs) {
		b.r.noteRequest(b.reqs[i])
	}
	if i < len(b.msgs) {
		for _, m := range b.msgs[i] {
			m.Date = int(time.Now().Unix())
			b.r.onMessage(m)
		}
	}
	return nil
}

const lateReport = "$OLD on-chain check: red flags found. https://www.perceptor.info/r/00000000000000000000000000000001"
const newReport = "$NEW on-chain check: no red flags found. https://www.perceptor.info/r/00000000000000000000000000000002"

// A bot message is kept unless it cannot answer our request: it replies to a
// message older than the request, or it was sent before it. Without the
// request's id, or without reply_to, today's behaviour stays.
func TestInvestigateIgnoresRepliesToEarlierRequests(t *testing.T) {
	cases := []struct {
		name        string
		req         int
		msgs        []*tg.Message
		wantIDs     []int
		wantIgnored []ignoredReply
	}{
		{"late reply to an earlier request is ignored", 200,
			[]*tg.Message{botMsg(201, 100, lateReport), botMsg(202, 200, newReport)},
			[]int{202}, []ignoredReply{{MessageID: 201, ReplyToMsgID: 100}}},
		{"request id unknown: both kept (today's behaviour)", 0,
			[]*tg.Message{botMsg(201, 100, lateReport), botMsg(202, 200, newReport)},
			[]int{201, 202}, nil},
		{"no reply_to: kept", 200,
			[]*tg.Message{botMsg(201, 0, newReport)},
			[]int{201}, nil},
		{"a second part replying to the first part: kept", 200,
			[]*tg.Message{botMsg(201, 200, newReport), botMsg(202, 201, "Top 10 holders 35%")},
			[]int{201, 202}, nil},
		{"message older than the request (a late edit): ignored", 200,
			[]*tg.Message{botMsg(150, 0, lateReport), botMsg(201, 200, newReport)},
			[]int{201}, []ignoredReply{{MessageID: 150}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newToolRunner(fastSpec(t))
			bot := &reqBot{r: r, reqs: []int{c.req}, msgs: [][]*tg.Message{c.msgs}}
			res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
			if res.Status != investigationCompleted {
				t.Fatalf("status %s (%v), want completed", res.Status, res.Err)
			}
			if got := msgIDs(res.Replies); !slices.Equal(got, c.wantIDs) {
				t.Errorf("replies %v, want %v", got, c.wantIDs)
			}
			if !slices.Equal(res.Ignored, c.wantIgnored) {
				t.Errorf("ignored %+v, want %+v", res.Ignored, c.wantIgnored)
			}
			if res.RequestMsgID != c.req {
				t.Errorf("request message id %d, want %d", res.RequestMsgID, c.req)
			}
		})
	}
}

// The same runner serves one token after another (and the re-check, re-scan
// and re-ask lanes): a late reply to the previous token's request is not
// taken for the next token's.
func TestInvestigateNextTokenIgnoresPreviousTokensReply(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &reqBot{r: r, reqs: []int{100, 200}, msgs: [][]*tg.Message{
		{botMsg(101, 100, lateReport)},
		{botMsg(201, 100, lateReport), botMsg(202, 200, newReport)}, // 201: the previous token's report again
	}}
	if res := r.investigateWithRetry(context.Background(), bot.send, "0xold"); res.ReportText() != lateReport {
		t.Fatalf("first token: report %q, want %q", res.ReportText(), lateReport)
	}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xnew")
	if res.Status != investigationCompleted || res.ReportText() != newReport {
		t.Fatalf("second token: status %s, report %q; want completed %q", res.Status, res.ReportText(), newReport)
	}
	if want := []ignoredReply{{MessageID: 201, ReplyToMsgID: 100}}; !slices.Equal(res.Ignored, want) {
		t.Errorf("ignored %+v, want %+v", res.Ignored, want)
	}
}

// Only a late reply to an earlier request: the first-reply timeout runs on (it
// does not count as a reply), so the result is "no reply" after Timeout, not
// a wait until MaxWait.
func TestInvestigateOnlyStaleRepliesTimesOut(t *testing.T) {
	spec := fastSpec(t)
	spec.Timeout = 300 * time.Millisecond
	r := newToolRunner(spec)
	bot := &reqBot{r: r, reqs: []int{200}, msgs: [][]*tg.Message{{botMsg(201, 100, lateReport)}}}
	start := time.Now()
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationTimeout || res.Err == nil || !strings.Contains(res.Err.Error(), "no reply") {
		t.Fatalf("status %s (%v), want timeout with no reply", res.Status, res.Err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s, want about the 300ms first-reply timeout (MaxWait is %s)", d, spec.MaxWait)
	}
	if len(res.Replies) != 0 || len(res.Ignored) != 1 {
		t.Errorf("replies %v, ignored %+v; want none and message 201", msgIDs(res.Replies), res.Ignored)
	}
}

// A rate-limit retry asks again for the same token: a reply to the first
// attempt's request still counts (it answers the same token), while a reply
// to a request before it does not.
func TestInvestigateRetryAcceptsEarlierAttemptOfSameToken(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &reqBot{r: r, reqs: []int{300, 310}, msgs: [][]*tg.Message{
		{botMsg(301, 300, "One scan every 2 minutes. You can scan again in 1 s")},
		{botMsg(311, 250, lateReport), botMsg(312, 300, newReport)},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.Attempts != 2 || res.ReportText() != newReport {
		t.Fatalf("status %s, attempts %d, report %q (%v); want completed after 2 with %q", res.Status, res.Attempts, res.ReportText(), res.Err, newReport)
	}
	if want := []ignoredReply{{MessageID: 311, ReplyToMsgID: 250}}; !slices.Equal(res.Ignored, want) || res.RequestMsgID != 310 {
		t.Errorf("ignored %+v, request %d; want %+v and 310", res.Ignored, res.RequestMsgID, want)
	}
}

// A rate-limit retry whose send names no message id (Telegram's answer named
// none) while the first attempt's id (300) is known: the retry still filters
// by the first attempt's id, and the stored result has no request id (0).
func TestInvestigateRetryUnknownRetryIDKeepsFirstFloor(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &reqBot{r: r, reqs: []int{300, 0}, msgs: [][]*tg.Message{
		{botMsg(301, 300, "One scan every 2 minutes. You can scan again in 1 s")},
		{botMsg(311, 250, lateReport), botMsg(312, 300, newReport)},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.Attempts != 2 || res.ReportText() != newReport {
		t.Fatalf("status %s, attempts %d, report %q (%v); want completed after 2 with %q", res.Status, res.Attempts, res.ReportText(), res.Err, newReport)
	}
	want := []ignoredReply{{MessageID: 311, ReplyToMsgID: 250}}
	if got := msgIDs(res.Replies); !slices.Equal(got, []int{312}) || !slices.Equal(res.Ignored, want) || res.RequestMsgID != 0 {
		t.Errorf("replies %v, ignored %+v, request %d; want [312], %+v and 0", got, res.Ignored, res.RequestMsgID, want)
	}
}

// The first attempt's request id is unknown (Telegram's answer named none;
// the request was really message 305): nothing is filtered for that token,
// not even after a retry whose id (310) is known, so a late reply to the
// first attempt still counts.
func TestInvestigateRetryUnknownFirstRequestNotFiltered(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &reqBot{r: r, reqs: []int{0, 310}, msgs: [][]*tg.Message{
		{botMsg(306, 305, "One scan every 2 minutes. You can scan again in 1 s")},
		{botMsg(311, 305, newReport)},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.Attempts != 2 || res.ReportText() != newReport {
		t.Fatalf("status %s, attempts %d, report %q (%v); want completed after 2 with %q", res.Status, res.Attempts, res.ReportText(), res.Err, newReport)
	}
	if got := msgIDs(res.Replies); !slices.Equal(got, []int{311}) || len(res.Ignored) != 0 || res.RequestMsgID != 310 {
		t.Errorf("replies %v, ignored %+v, request %d; want [311], none and 310", got, res.Ignored, res.RequestMsgID)
	}
	// The next token starts afresh: its known request id filters again.
	bot.reqs, bot.msgs, bot.sent = []int{400}, [][]*tg.Message{{botMsg(401, 305, lateReport), botMsg(402, 400, newReport)}}, nil
	res = r.investigateWithRetry(context.Background(), bot.send, "0xdef")
	if want := []ignoredReply{{MessageID: 401, ReplyToMsgID: 305}}; res.ReportText() != newReport || !slices.Equal(res.Ignored, want) {
		t.Errorf("next token: report %q, ignored %+v; want %q and %+v", res.ReportText(), res.Ignored, newReport, want)
	}
}

// A message left out in one attempt and seen again in a retry (an edit) is
// listed once in Ignored.
func TestInvestigateRetryIgnoredListedOnce(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &reqBot{r: r, reqs: []int{300, 310}, msgs: [][]*tg.Message{
		{botMsg(301, 250, lateReport), botMsg(302, 300, "One scan every 2 minutes. You can scan again in 1 s")},
		{botMsg(301, 250, lateReport+" (edited)"), botMsg(312, 310, newReport)},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.Attempts != 2 || res.ReportText() != newReport {
		t.Fatalf("status %s, attempts %d, report %q (%v); want completed after 2 with %q", res.Status, res.Attempts, res.ReportText(), res.Err, newReport)
	}
	if want := []ignoredReply{{MessageID: 301, ReplyToMsgID: 250}}; !slices.Equal(res.Ignored, want) {
		t.Errorf("ignored %+v, want %+v", res.Ignored, want)
	}
}

// details stores the request's message id and the ignored messages; both are
// left out when there are none.
func TestReplyDetailsRequestAndIgnored(t *testing.T) {
	for _, c := range []struct {
		name string
		res  *toolResult
		want string
	}{
		{"filtered", &toolResult{Replies: []*tg.Message{botMsg(202, 200, "x")}, Attempts: 1, RequestMsgID: 200,
			Ignored: []ignoredReply{{MessageID: 201, ReplyToMsgID: 100}, {MessageID: 150}}},
			`{"attempts":1,"request_msg_id":200,"ignored_replies":[{"message_id":201,"reply_to_msg_id":100},{"message_id":150}]}`},
		{"nothing known", &toolResult{Attempts: 1}, `{"attempts":1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal(replyDetails(c.res), &got); err != nil {
				t.Fatal(err)
			}
			delete(got, "replies") // covered by TestReplyDetailsMedia
			var want map[string]any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("replyDetails = %s, want %s", gb, wb)
			}
		})
	}
}

func TestRequestMessageID(t *testing.T) {
	if got := requestMessageID("perceptor", &tg.UpdateShortSentMessage{ID: 77}); got != 77 {
		t.Errorf("requestMessageID(short sent message 77) = %d, want 77", got)
	}
	if got := requestMessageID("perceptor", &tg.Updates{}); got != 0 {
		t.Errorf("requestMessageID(no message) = %d, want 0", got)
	}
}

// laneScanner is a scanner whose Telegram sends go to fakeSendInvoker (every
// sent message gets id 77), with Perceptor (a gate) and sAlpha registered.
func laneScanner(inv *fakeSendInvoker) (*scanner, *toolRunner, *toolRunner) {
	perc, sal := 1, 2
	pr := newToolRunner(testPerceptorSpec())
	pr.toolID, pr.botPeer = &perc, &tg.InputPeerSelf{}
	sr := newToolRunner(testSAlphaSpec())
	sr.toolID, sr.botPeer = &sal, &tg.InputPeerSelf{}
	s := &scanner{cfg: &config{NotifyPeer: "me"}, db: &ScoutStore{}, sender: message.NewSender(tg.NewClient(inv)),
		runners: []*toolRunner{pr, sr}}
	return s, pr, sr
}

// Every lane that asks a bot records the request's message id: the re-check,
// the rescan lane and the sAlpha re-ask send through sendToBot.
func TestLaneSendsRecordRequestID(t *testing.T) {
	s, pr, sr := laneScanner(&fakeSendInvoker{})
	rc := s.newRecheckLane(recheckConfig{After: 12 * time.Minute, After2: 2 * time.Hour})
	rs := s.newRescanLane(rescanConfig{Enabled: true})
	ra := s.newReaskLane(reaskConfig{Enabled: true})
	if rc == nil || rs == nil || ra == nil {
		t.Fatalf("lanes: recheck %v, rescan %v, reask %v; want all three", rc != nil, rs != nil, ra != nil)
	}
	for _, c := range []struct {
		name string
		send func(context.Context, *toolRunner, string) error
		r    *toolRunner
	}{
		{"recheck", rc.send, pr},
		{"rescan", rs.send, pr},
		{"reask", ra.send, sr},
	} {
		c.r.request, c.r.floor = 0, 0
		if err := c.send(context.Background(), c.r, "x"); err != nil {
			t.Fatalf("%s: send: %v", c.name, err)
		}
		if c.r.request != 77 || c.r.floor != 77 {
			t.Errorf("%s: request %d, floor %d after the send; want 77 (the sent message's id)", c.name, c.r.request, c.r.floor)
		}
	}
}

// The live lane (investigateAll): a late reply to an earlier request that
// arrives while the send is still in flight (before its id is known) is left
// out once the id is known; the reply to our request (id 77) is kept.
func TestInvestigateAllIgnoresLateReply(t *testing.T) {
	inv := &fakeSendInvoker{}
	s, _, sr := laneScanner(inv)
	s.runners = []*toolRunner{sr}
	inv.stop = func() { // during the send: Telegram delivers the bot's messages first
		sr.onMessage(botMsg(78, 60, "sAlpha research on $OLD: dev sold."))
		sr.onMessage(botMsg(79, 77, "sAlpha research on $NEW: 3 smart wallets bought."))
	}
	res := s.investigateAll(context.Background(), "0xnew")
	if len(res) != 1 || res[0].Status != investigationCompleted {
		t.Fatalf("results %+v, want one completed", res)
	}
	if got := msgIDs(res[0].Replies); !slices.Equal(got, []int{79}) || res[0].RequestMsgID != 77 {
		t.Errorf("replies %v, request %d; want [79] and 77", got, res[0].RequestMsgID)
	}
	if want := []ignoredReply{{MessageID: 78, ReplyToMsgID: 60}}; !slices.Equal(res[0].Ignored, want) {
		t.Errorf("ignored %+v, want %+v", res[0].Ignored, want)
	}
}

// laneBot is a lane send (re-check, re-scan, re-ask): each request gets the
// next message id (next, then next+10, …); the bot then sends late, a reply
// to lateTo (an earlier request of ours), and report, a reply to our request.
type laneBot struct {
	next   int
	lateTo int
	late   string
	report string
	sent   []string
	reqs   []int
}

func (b *laneBot) send(ctx context.Context, r *toolRunner, cmd string) error {
	b.sent = append(b.sent, cmd)
	req := b.next
	b.next += 10
	b.reqs = append(b.reqs, req)
	r.noteRequest(req)
	r.onMessage(botMsg(req+1, b.lateTo, b.late))
	r.onMessage(botMsg(req+2, req, b.report))
	return nil
}

// checkLaneRow checks a lane's stored row: only the reply to our request,
// with the request's id and the ignored late reply in details.
func checkLaneRow(t *testing.T, row *ScoutInvestigation, req, lateTo int, report string) {
	t.Helper()
	if !slices.Equal(row.BotMessageIDs, []int32{int32(req + 2)}) || row.ReportText != report {
		t.Errorf("row: bot messages %v, report %q; want [%d] and %q", row.BotMessageIDs, row.ReportText, req+2, report)
	}
	var d struct {
		RequestMsgID int            `json:"request_msg_id"`
		Ignored      []ignoredReply `json:"ignored_replies"`
	}
	if err := json.Unmarshal(row.Details, &d); err != nil {
		t.Fatalf("details %s: %v", row.Details, err)
	}
	if want := []ignoredReply{{MessageID: req + 1, ReplyToMsgID: lateTo}}; d.RequestMsgID != req || !slices.Equal(d.Ignored, want) {
		t.Errorf("details %s: request %d, ignored %+v; want %d and %+v", row.Details, d.RequestMsgID, d.Ignored, req, want)
	}
}

func TestRescanLaneIgnoresLateReply(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ca := "0x1111111111111111111111111111111111111111"
	bot := &laneBot{next: 500, lateTo: 400, late: lateReport, report: newReport}
	l := &rescanLane{cfg: rescanConfig{Enabled: true, MaxAge: 720 * time.Hour, MaxPerDay: 100, Gap: 10 * time.Minute, Idle: 5 * time.Minute,
		Statuses: []string{"failed"}}, runner: newToolRunner(testPerceptorSpec()), toolID: 7, now: func() time.Time { return now },
		send: bot.send,
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			return textVerdict(res.ReportText(), splitList(defaultMarkers), splitList(defaultSafe))
		}, errLog: logLimiter{every: time.Minute}}
	l.noteLive(now.Add(-time.Hour))
	st := &fakeRescanStore{cand: &RescanCandidate{CallID: 42, ContractAddress: ca, MessageDate: now.Add(-24 * time.Hour), Status: "failed"}}
	if !l.step(context.Background(), st, func() int { return 0 }) || len(st.inserted) != 1 {
		t.Fatalf("step: %d rows stored, want one rescan", len(st.inserted))
	}
	row := st.inserted[0]
	checkLaneRow(t, row, 500, 400, newReport)
	if row.VerdictLevel == levelRedFlags {
		t.Errorf("verdict %s: the late red-flags report of an earlier request was judged", row.VerdictLevel)
	}
}

func TestReaskLaneIgnoresLateReply(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ca := "0x1111111111111111111111111111111111111111"
	const real = "sAlpha research: 3 smart wallets bought; dev holds 2%."
	bot := &laneBot{next: 900, lateTo: 850, late: "sAlpha research on another token: dev sold everything.", report: real}
	var follow []reaskFollowUp
	l := &reaskLane{cfg: reaskConfig{Enabled: true, MinAge: 6 * time.Hour, MaxAge: 24 * time.Hour, MaxPerDay: 60, Gap: 10 * time.Minute},
		runner: newToolRunner(testSAlphaSpec()), toolID: 3, now: func() time.Time { return now }, target: "scout analytics",
		send: bot.send,
		judge: func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
			return verdict{Level: levelUnknown, Source: "telegram_text"}
		},
		followUp: func(ctx context.Context, f reaskFollowUp) (int64, error) {
			follow = append(follow, f)
			return 9000, nil
		},
		errLog: logLimiter{every: time.Minute}}
	l.noteLive(now.Add(-time.Hour))
	stored := int64(4242)
	st := &fakeReaskStore{cand: &ReaskCandidate{CallID: 42, ContractAddress: ca, MessageDate: now.Add(-7 * time.Hour), MessageID: 777,
		ChannelUsername: "scoutrobinhood", Reason: reaskReasonDeclined},
		delivery: &ScoutDelivery{ID: ip(1), Target: "scout analytics", NotifyMessageID: &stored, Status: DeliveryStatusSent}}
	if !l.step(context.Background(), st, func() int { return 0 }) || len(st.inserted) != 1 {
		t.Fatalf("step: %d rows stored, want one re-ask", len(st.inserted))
	}
	checkLaneRow(t, st.inserted[0], 900, 850, real)
	if len(follow) != 1 || follow[0].Result.ReportText() != real {
		t.Errorf("follow-ups %+v, want one with only %q", follow, real)
	}
}
