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

// A report that is only an image (e.g. a chart card) counts as a real report.
func TestPhotoOnlyReportIsComplete(t *testing.T) {
	r := newToolRunner(progressSpec(t, "salpha"))
	bot := &scriptedBot{r: r, script: []step{{after: 20 * time.Millisecond, msg: tg.Message{Media: &tg.MessageMediaPhoto{}}}}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || len(res.Replies) != 1 {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
}
