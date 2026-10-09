package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// step is one bot action after a request: a new message or an edit of an earlier one.
type step struct {
	after  time.Duration
	editOf int // 0 = new message; otherwise index (1-based) of an earlier step's message to edit
	msg    tg.Message
}

// scriptedBot plays a fixed script of replies for every request.
type scriptedBot struct {
	r      *toolRunner
	script []step
}

func (b *scriptedBot) send(ctx context.Context, text string) error {
	go func() {
		ids := []int{}
		next := 1000
		for _, st := range b.script {
			time.Sleep(st.after)
			m := st.msg
			if st.editOf > 0 {
				m.ID = ids[st.editOf-1]
			} else {
				next++
				m.ID = next
			}
			ids = append(ids, m.ID)
			m.Date = int(time.Now().Unix())
			b.r.onMessage(&m)
		}
	}()
	return nil
}

func progressSpec(t *testing.T, code string) ToolSpec {
	t.Setenv("SCOUT_TOOLS", code)
	up := strings.ToUpper(code)
	t.Setenv("SCOUT_TOOL_"+up+"_TIMEOUT", "2s")
	t.Setenv("SCOUT_TOOL_"+up+"_SETTLE", "100ms")
	t.Setenv("SCOUT_TOOL_"+up+"_MAX_WAIT", "1500ms")
	t.Setenv("SCOUT_TOOL_"+up+"_MIN_INTERVAL", "0s")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	return tools[0]
}

const perceptorFinal = "$ABC on-chain check: caution. Top 10 hold 35%. https://www.perceptor.info/r/d3e4fa0b496b456fbdc9499ae5fcf5f2"

// The real failure: "Scanning 0x0ad7…a5b0 on Robinhood Chain…" then, well after the
// settle time, the report arrives as an EDIT of that message.
func TestPerceptorScanningThenEdit(t *testing.T) {
	r := newToolRunner(progressSpec(t, "perceptor"))
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Message: "Scanning 0x0ad7…a5b0 on Robinhood Chain…"}},
		{after: 400 * time.Millisecond, editOf: 1, msg: tg.Message{Message: perceptorFinal}},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0x0ad79edf66e797a3bcae56360134876facc0a5b0")
	if res.Status != investigationCompleted || res.ReportText() != perceptorFinal {
		t.Fatalf("status=%s text=%q err=%v", res.Status, res.ReportText(), res.Err)
	}
	s := &scanner{cfg: &config{RedFlagMarkers: splitList(defaultMarkers), SafePhrases: splitList(defaultSafe)}}
	// (page fetch is blocked in tests → text fallback) caution is detected from the final report
	if v := textVerdict(res.ReportText(), s.cfg.RedFlagMarkers, s.cfg.SafePhrases); v.Level != levelCaution {
		t.Fatalf("verdict = %+v", v)
	}
}

// Same, but the report is a NEW message; the placeholder must not be forwarded.
func TestPerceptorScanningThenNewMessage(t *testing.T) {
	r := newToolRunner(progressSpec(t, "perceptor"))
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Message: "🔍 Scanning 0x0ad7…a5b0 on Robinhood Chain…"}},
		{after: 400 * time.Millisecond, msg: tg.Message{Message: perceptorFinal}},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || len(res.Replies) != 1 || res.Replies[0].Message != perceptorFinal {
		t.Fatalf("status=%s replies=%d text=%q", res.Status, len(res.Replies), res.ReportText())
	}
}

// Report never finishes: time out with a clear error (not "completed" with the placeholder).
func TestPerceptorStuckScanning(t *testing.T) {
	r := newToolRunner(progressSpec(t, "perceptor"))
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Message: "Scanning 0x0ad7…a5b0 on Robinhood Chain…"}},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationTimeout || res.Err == nil || !strings.Contains(res.Err.Error(), "Scanning") {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
}

// Rate-limit notice still ends the wait immediately and is retried.
func TestPerceptorRateLimitStillDetected(t *testing.T) {
	spec := progressSpec(t, "perceptor")
	spec.RateLimitBuffer = 0
	r := newToolRunner(spec)
	res := r.investigate(context.Background(), (&scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Message: "One scan every 2 minutes. You can scan again in 63 s"}},
	}}).send, "0xabc")
	if res.Status != investigationCompleted {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if d, ok := r.rateLimitWait(res); !ok || d != 63*time.Second {
		t.Fatalf("rate limit: %s %v", d, ok)
	}
}

// sAlpha: a loading sticker/GIF with no text first, then the research text.
func TestSalphaLoadingAnimationThenReport(t *testing.T) {
	r := newToolRunner(progressSpec(t, "salpha"))
	anim := &tg.MessageMediaDocument{Document: &tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}}}}
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Media: anim}},
		{after: 400 * time.Millisecond, msg: tg.Message{Message: "sAlpha research: holders 1.2k, dev sold 0%, LP locked"}},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || !strings.HasPrefix(res.ReportText(), "sAlpha research") || len(res.Replies) != 1 {
		t.Fatalf("status=%s replies=%d text=%q err=%v", res.Status, len(res.Replies), res.ReportText(), res.Err)
	}
}

// For a tool without NEEDS_TEXT (a custom tool), a report that is only an
// image (e.g. a chart card) counts as a real report.
func TestPhotoOnlyReportIsComplete(t *testing.T) {
	t.Setenv("SCOUT_TOOL_FOO_BOT", "foo_bot")
	spec := progressSpec(t, "foo")
	if spec.NeedsText {
		t.Fatalf("custom tool foo: NeedsText = true, want false")
	}
	r := newToolRunner(spec)
	bot := &scriptedBot{r: r, script: []step{{after: 20 * time.Millisecond, msg: tg.Message{Media: &tg.MessageMediaPhoto{}}}}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || len(res.Replies) != 1 {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
}

const salphaReport = "$ABC research. Generated 1d ago. Full Report (#report)\nNarrative: … Catalysts: …"

// sAlpha (NEEDS_TEXT): a reply without text (a photo, or media of a type we do
// not know, as in prod call 10424) does not complete the report; the text that
// arrives after the settle time does, and the media reply is kept so the
// forward still includes it. Times are scaled down: settle 100 ms, text 500 ms
// after the media (in prod: settle 15 s, text about 20 s later).
func TestSalphaMediaThenTextAfterSettle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		media tg.MessageMediaClass
	}{
		{"photo", &tg.MessageMediaPhoto{}},
		{"unsupported media", &tg.MessageMediaUnsupported{}},
		{"photo document", &tg.MessageMediaDocument{Document: &tg.Document{MimeType: "image/png"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRunner(progressSpec(t, "salpha"))
			bot := &scriptedBot{r: r, script: []step{
				{after: 20 * time.Millisecond, msg: tg.Message{Media: tc.media}},
				{after: 500 * time.Millisecond, msg: tg.Message{Message: salphaReport}},
			}}
			res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
			if res.Status != investigationCompleted || res.Err != nil {
				t.Fatalf("status=%s err=%v, want completed", res.Status, res.Err)
			}
			if got := res.MessageIDs(); len(got) != 2 || got[0] != 1001 || got[1] != 1002 {
				t.Errorf("MessageIDs() = %v, want [1001 1002]", got)
			}
			if got := res.ReportText(); got != salphaReport {
				t.Errorf("ReportText() = %q, want %q", got, salphaReport)
			}
			if res.Best == nil || res.Best.ID != 1002 {
				best := 0
				if res.Best != nil {
					best = res.Best.ID
				}
				t.Errorf("Best = message %d, want message 1002 (the text)", best)
			}
		})
	}
}

// sAlpha: only media and never any text until MaxWait: timeout (not a
// completed report with empty text), with an error that names the media, and
// the media reply's id kept for the record.
func TestSalphaMediaOnlyTimesOut(t *testing.T) {
	r := newToolRunner(progressSpec(t, "salpha"))
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Media: &tg.MessageMediaPhoto{}}},
		{after: 20 * time.Millisecond, msg: tg.Message{Message: "​ ⁣"}}, // invisible characters only
	}}
	start := time.Now()
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationTimeout {
		t.Fatalf("status=%s err=%v, want timeout", res.Status, res.Err)
	}
	if el := time.Since(start); el < 1400*time.Millisecond {
		t.Errorf("returned after %s, want about MaxWait (1.5s)", el)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no report text") || !strings.Contains(res.Err.Error(), "messageMediaPhoto") {
		t.Errorf("err = %v, want it to say no report text and name messageMediaPhoto", res.Err)
	}
	if got := res.MessageIDs(); len(got) != 2 {
		t.Errorf("MessageIDs() = %v, want both replies", got)
	}
}

// sAlpha's decline is text: it completes the report at once (after the
// settle time) and the website classifies it as a decline.
func TestSalphaDeclineCompletes(t *testing.T) {
	const decline = "There is not enough public information to write a report on this token."
	r := newToolRunner(progressSpec(t, "salpha"))
	bot := &scriptedBot{r: r, script: []step{{after: 20 * time.Millisecond, msg: tg.Message{Message: decline}}}}
	start := time.Now()
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.ReportText() != decline {
		t.Fatalf("status=%s text=%q err=%v, want completed with the decline", res.Status, res.ReportText(), res.Err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("returned after %s, want soon after the settle time (100ms)", el)
	}
	if !salphaDeclined(res.ReportText()) {
		t.Errorf("salphaDeclined(%q) = false, want true", res.ReportText())
	}
}

// Perceptor is unchanged: NEEDS_TEXT is off and its DoneRe decides, as before
// (a photo has nothing DoneRe matches, so it waits for the verdict text), and
// the photo is kept with the report.
func TestPerceptorPhotoThenVerdictUnchanged(t *testing.T) {
	spec := progressSpec(t, "perceptor")
	if spec.NeedsText {
		t.Fatalf("perceptor: NeedsText = true, want false")
	}
	r := newToolRunner(spec)
	bot := &scriptedBot{r: r, script: []step{
		{after: 20 * time.Millisecond, msg: tg.Message{Media: &tg.MessageMediaPhoto{}}},
		{after: 400 * time.Millisecond, msg: tg.Message{Message: perceptorFinal}},
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.ReportText() != perceptorFinal || len(res.Replies) != 2 {
		t.Fatalf("status=%s replies=%d text=%q err=%v", res.Status, len(res.Replies), res.ReportText(), res.Err)
	}
}
