package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestRateLimitDetection(t *testing.T) {
	t.Setenv("SCOUT_TOOLS", "perceptor")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	r := newToolRunner(tools[0])
	cases := map[string]time.Duration{
		"One scan every 2 minutes. You can scan again in 63 s": 66 * time.Second, // 63s + 3s buffer
		"Rate limited, try again in 2 minutes":                 2*time.Minute + 3*time.Second,
		"Please wait 10 sec":                                   13 * time.Second,
	}
	for text, want := range cases {
		res := &toolResult{Status: investigationCompleted, Replies: []*tg.Message{{ID: 1, Message: text}}}
		got, ok := r.rateLimitWait(res)
		if !ok || got != want {
			t.Errorf("%q: got %s %v, want %s", text, got, ok, want)
		}
	}
	for _, text := range []string{
		"$ANYR on-chain check: no red flags found. https://www.perceptor.info/r/deb9",
		strings.Repeat("long research report ", 40) + " you can scan again in 5 s",
	} {
		res := &toolResult{Status: investigationCompleted, Replies: []*tg.Message{{ID: 1, Message: text}}}
		if _, ok := r.rateLimitWait(res); ok {
			t.Errorf("false positive: %.60q", text)
		}
	}
	if _, ok := r.rateLimitWait(&toolResult{Status: investigationTimeout}); ok {
		t.Error("timeout is not a rate limit")
	}
}

// fakeBot answers each request through runner.onMessage, like Telegram updates would.
type fakeBot struct {
	mu      sync.Mutex
	r       *toolRunner
	replies []string // one per request; last one repeats
	sent    []string
	sentAt  []time.Time
	// lastSent records r.lastSent as send sees it: the runner's own pacing
	// stamp. send runs on the runner's goroutine, right after investigate sets
	// r.lastSent, so reading it here is race-free.
	lastSent []time.Time
	nextID   int
}

func (b *fakeBot) send(ctx context.Context, text string) error {
	b.mu.Lock()
	i := len(b.sent)
	b.sent = append(b.sent, text)
	b.sentAt = append(b.sentAt, time.Now())
	b.lastSent = append(b.lastSent, b.r.lastSent)
	if i >= len(b.replies) {
		i = len(b.replies) - 1
	}
	reply := b.replies[i]
	b.nextID++
	id := b.nextID
	b.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.r.onMessage(&tg.Message{ID: id, Date: int(time.Now().Unix()), Message: reply})
	}()
	return nil
}

func fastSpec(t *testing.T) ToolSpec {
	t.Setenv("SCOUT_TOOLS", "perceptor")
	t.Setenv("SCOUT_TOOL_PERCEPTOR_TIMEOUT", "2s")
	t.Setenv("SCOUT_TOOL_PERCEPTOR_SETTLE", "100ms")
	t.Setenv("SCOUT_TOOL_PERCEPTOR_MAX_WAIT", "5s")
	t.Setenv("SCOUT_TOOL_PERCEPTOR_MIN_INTERVAL", "0s")
	t.Setenv("SCOUT_TOOL_PERCEPTOR_RATE_LIMIT_BUFFER", "0s")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	return tools[0]
}

func TestInvestigateWithRetry_WaitsAndRetries(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	bot := &fakeBot{r: r, replies: []string{
		"One scan every 2 minutes. You can scan again in 1 s",
		"$ANYR on-chain check: no red flags found. https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0",
	}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationCompleted || res.Attempts != 2 || len(res.RateLimitWaits) != 1 || res.RateLimitWaits[0] != 1 {
		t.Fatalf("status=%s attempts=%d waits=%v err=%v", res.Status, res.Attempts, res.RateLimitWaits, res.Err)
	}
	if !strings.Contains(res.ReportText(), "no red flags found") || strings.Contains(res.ReportText(), "scan again") {
		t.Fatalf("report should only contain the final reply: %q", res.ReportText())
	}
	if len(bot.sent) != 2 || bot.sent[0] != "/scan 0xabc" || bot.sent[1] != "/scan 0xabc" {
		t.Fatalf("sent = %v", bot.sent)
	}
	if gap := bot.sentAt[1].Sub(bot.sentAt[0]); gap < time.Second {
		t.Fatalf("retried too early: %s", gap)
	}
}

func TestInvestigateWithRetry_GivesUp(t *testing.T) {
	t.Setenv("SCOUT_TOOL_PERCEPTOR_MAX_RETRIES", "1")
	r := newToolRunner(fastSpec(t))
	bot := &fakeBot{r: r, replies: []string{"You can scan again in 1 s"}}
	res := r.investigateWithRetry(context.Background(), bot.send, "0xabc")
	if res.Status != investigationRateLimited || res.Attempts != 2 || res.Err == nil {
		t.Fatalf("status=%s attempts=%d err=%v", res.Status, res.Attempts, res.Err)
	}
	s := &scanner{cfg: &config{DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}}}
	if s.shouldDeliver([]*toolResult{res}) {
		t.Fatal("rate-limited gate tool must not deliver")
	}
}

func TestInvestigateWithRetry_Pacing(t *testing.T) {
	t.Setenv("SCOUT_TOOL_PERCEPTOR_MIN_INTERVAL", "700ms")
	spec := fastSpec(t)
	spec.MinInterval = 700 * time.Millisecond
	r := newToolRunner(spec)
	bot := &fakeBot{r: r, replies: []string{"$A on-chain check: caution.", "$B on-chain check: no red flags found."}}
	if res := r.investigateWithRetry(context.Background(), bot.send, "0x1"); res.Status != investigationCompleted {
		t.Fatal(res.Err)
	}
	if res := r.investigateWithRetry(context.Background(), bot.send, "0x2"); res.Status != investigationCompleted || res.ReportText() != "$B on-chain check: no red flags found." {
		t.Fatalf("%s %q", res.Status, res.ReportText())
	}
	// Compare the runner's own lastSent stamps, not sentAt: pacing waits until
	// MinInterval has passed since lastSent, while sentAt is a second clock
	// read taken later inside send (after a mutex lock). Under -race and with
	// Windows timer granularity that offset differs between the two sends, so
	// the sentAt gap could come out a millisecond short although pacing worked
	// (seen once: 699ms against 700ms).
	if len(bot.lastSent) < 2 || bot.lastSent[0].IsZero() || bot.lastSent[1].IsZero() {
		t.Fatalf("pacing check needs two non-zero lastSent stamps: got %v", bot.lastSent)
	}
	if gap := bot.lastSent[1].Sub(bot.lastSent[0]); gap < spec.MinInterval {
		t.Fatalf("pacing not applied: got gap %s between lastSent stamps %v, want >= %s", gap, bot.lastSent, spec.MinInterval)
	}
}

func TestInvestigateWithRetry_Cancel(t *testing.T) {
	r := newToolRunner(fastSpec(t))
	r.spec.RateLimitBuffer = time.Minute
	bot := &fakeBot{r: r, replies: []string{"You can scan again in 1 s"}}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := r.investigateWithRetry(ctx, bot.send, "0xabc")
	if res.Status != investigationFailed || time.Since(start) > 3*time.Second {
		t.Fatalf("status=%s after %s", res.Status, time.Since(start))
	}
}
