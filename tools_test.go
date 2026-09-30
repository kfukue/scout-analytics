package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestLoadToolsDefaultsAndOverrides(t *testing.T) {
	t.Setenv("SCOUT_TOOLS", "")
	tools, err := loadTools()
	if err != nil || len(tools) != 2 {
		t.Fatalf("defaults: %v %v", tools, err)
	}
	p, s := tools[0], tools[1]
	if p.Code != "perceptor" || p.Bot != "perceptor0xBot" || !p.Gate || p.CommandFor("0xA") != "/scan 0xA" {
		t.Fatalf("perceptor = %+v", p)
	}
	if s.Code != "salpha" || s.Bot != "salpha_research_bot" || s.Gate || !s.Attach || s.CommandFor("0xA") != "0xA" {
		t.Fatalf("salpha = %+v", s)
	}

	// custom third tool + overrides + legacy single-bot settings
	t.Setenv("SCOUT_TOOLS", "perceptor, salpha, rugcheck")
	t.Setenv("SCOUT_SCAN_COMMAND", "/audit") // legacy: no {ca} -> appended
	t.Setenv("SCOUT_TOOL_SALPHA_TIMEOUT", "3m")
	t.Setenv("SCOUT_TOOL_RUGCHECK_BOT", "@rug_bot")
	t.Setenv("SCOUT_TOOL_RUGCHECK_COMMAND", "/check {ca} full")
	t.Setenv("SCOUT_TOOL_RUGCHECK_GATE", "true")
	tools, err = loadTools()
	if err != nil || len(tools) != 3 {
		t.Fatalf("custom: %v %v", tools, err)
	}
	if tools[0].CommandFor("0xA") != "/audit 0xA" || tools[1].Timeout != 3*time.Minute {
		t.Fatalf("overrides: %+v %+v", tools[0], tools[1])
	}
	if r := tools[2]; r.Bot != "rug_bot" || !r.Gate || r.Parser != parserText || r.CommandFor("0xA") != "/check 0xA full" {
		t.Fatalf("rugcheck = %+v", r)
	}

	t.Setenv("SCOUT_TOOLS", "mystery")
	if _, err := loadTools(); err == nil {
		t.Fatal("unknown tool without _BOT should error")
	}
}

func TestShouldDeliverAndHeader(t *testing.T) {
	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, _ := loadTools()
	s := &scanner{cfg: &config{SourceChannel: "scoutrobinhood",
		DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}}}
	mk := func(level, sStatus string) []*toolResult {
		return []*toolResult{
			{Spec: tools[0], Status: investigationCompleted, Verdict: verdict{Level: level, Label: "Caution", Ticker: "$X",
				URL: "https://www.perceptor.info/r/abc"}},
			{Spec: tools[1], Status: sStatus, Verdict: verdict{Level: levelUnknown},
				Replies: []*tg.Message{{ID: 1, Message: "research"}}},
		}
	}
	if !s.shouldDeliver(mk(levelCaution, investigationCompleted)) {
		t.Fatal("caution should deliver")
	}
	if !s.shouldDeliver(mk(levelClean, investigationTimeout)) {
		t.Fatal("clean should deliver even if salpha timed out (not a gate)")
	}
	if s.shouldDeliver(mk(levelRedFlags, investigationCompleted)) {
		t.Fatal("red flags must not deliver")
	}
	gateFailed := mk(levelClean, investigationCompleted)
	gateFailed[0].Status = investigationTimeout
	if s.shouldDeliver(gateFailed) {
		t.Fatal("gate tool failure must not deliver")
	}

	h := s.deliveryHeader(job{CA: "0xA", SourceMsg: 7}, mk(levelCaution, investigationTimeout))
	for _, want := range []string{"🟡 $X Caution", "CA: 0xA", "• Perceptor: 🟡 Caution", "https://www.perceptor.info/r/abc",
		"• sAlpha: no report (timeout)", "https://t.me/scoutrobinhood/7"} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	h = s.deliveryHeader(job{CA: "0xA"}, mk(levelClean, investigationCompleted))
	if !strings.Contains(h, "• sAlpha: report attached") {
		t.Errorf("header:\n%s", h)
	}
}

func TestToolResultText(t *testing.T) {
	r := &toolResult{Replies: []*tg.Message{{ID: 1, Message: "part 1"}, {ID: 2, Message: " "}, {ID: 3, Message: "part 2"}}}
	if r.ReportText() != "part 1\n\npart 2" || len(r.MessageIDs()) != 3 {
		t.Fatalf("%q %v", r.ReportText(), r.MessageIDs())
	}
}

func TestNormalizeChatIDAndUsername(t *testing.T) {
	for in, want := range map[int64]int64{-1001234567890: 1234567890, -4012345678: 4012345678, 987: 987} {
		if got := normalizeChatID(in); got != want {
			t.Errorf("normalizeChatID(%d)=%d want %d", in, got, want)
		}
	}
	for s, want := range map[string]bool{"scout analytics": false, "scout_analytics": true, "Scout": true, "ab": false} {
		if usernameRe.MatchString(s) != want {
			t.Errorf("usernameRe(%q) != %v", s, want)
		}
	}
}

func TestPickCallMessageAndBotAPIText(t *testing.T) {
	chains := map[string]bool{"evm": true, "solana": true}
	ca := "0x0976f3067dd97321b7ab269c5a2c290264f7046d"
	res := &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 50, Message: "old call " + ca},
		&tg.Message{ID: 90, Message: "🚀 NEW CALL\nCA: 0x0976F3067DD97321B7AB269C5A2C290264F7046D"}, // case differs
		&tg.Message{ID: 95, Message: "unrelated 0x1111111111111111111111111111111111111111"},
		&tg.MessageService{ID: 99},
	}}
	m := pickCallMessage(res, ca, chains)
	if m == nil || m.ID != 90 {
		t.Fatalf("picked %+v", m)
	}
	if pickCallMessage(&tg.MessagesChannelMessages{}, ca, chains) != nil {
		t.Fatal("empty search should give nil")
	}

	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, _ := loadTools()
	s := &scanner{cfg: &config{SourceChannel: "scoutrobinhood"}}
	results := []*toolResult{
		{Spec: tools[0], Status: investigationCompleted, Replies: []*tg.Message{{ID: 1, Message: "perceptor report"}}},
		{Spec: tools[1], Status: investigationTimeout},
	}
	txt := s.botAPIText("HEADER", job{CA: ca, SourceMsg: 90, SourceText: "🚀 NEW CALL"}, results)
	iH, iC, iP := strings.Index(txt, "HEADER"), strings.Index(txt, "Original call (@scoutrobinhood)"), strings.Index(txt, "perceptor report")
	if iH != 0 || iC < 0 || iP < iC || !strings.Contains(txt, "🚀 NEW CALL") || strings.Contains(txt, "sAlpha") {
		t.Fatalf("bot API text order/content wrong:\n%s", txt)
	}
}
