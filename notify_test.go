package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestInviteLinkRe(t *testing.T) {
	for in, want := range map[string]string{
		"https://t.me/+EXAMPLEHASH12345":         "EXAMPLEHASH12345",
		"t.me/+EXAMPLEHASH12345":                 "EXAMPLEHASH12345",
		"https://t.me/joinchat/EXAMPLEHASH12345": "EXAMPLEHASH12345",
		"tg://join?invite=EXAMPLEHASH12345":      "EXAMPLEHASH12345",
		"+EXAMPLEHASH12345":                      "EXAMPLEHASH12345",
	} {
		m := inviteLinkRe.FindStringSubmatch(in)
		if m == nil || m[1] != want {
			t.Errorf("%q → %v", in, m)
		}
	}
	for _, in := range []string{"scout analytics", "@scoutanalytics", "-1001234567890", "me"} {
		if inviteLinkRe.MatchString(in) {
			t.Errorf("%q should not be treated as an invite link", in)
		}
	}
}

func TestIsPeerError(t *testing.T) {
	for _, typ := range []string{"PEER_ID_INVALID", "CHAT_ID_INVALID", "CHANNEL_INVALID", "CHANNEL_PRIVATE"} {
		err := fmt.Errorf("send text: %w", tgerr.New(400, typ))
		if !isPeerError(err) {
			t.Errorf("%s should be a peer error", typ)
		}
	}
	if isPeerError(tgerr.New(420, "FLOOD_WAIT_5")) || isPeerError(errors.New("network down")) {
		t.Error("non-peer errors must not trigger a re-resolve")
	}
}

func TestChatPeer(t *testing.T) {
	if p, label, ok := chatPeer(&tg.Chat{ID: 42, Title: "scout analytics"}); !ok || label != `group "scout analytics" (id 42)` {
		t.Fatalf("%v %q %v", p, label, ok)
	} else if c, _ := p.(*tg.InputPeerChat); c == nil || c.ChatID != 42 {
		t.Fatalf("peer %T", p)
	}
	if _, label, ok := chatPeer(&tg.Channel{ID: 7, AccessHash: 9, Title: "scout analytics", Megagroup: true}); !ok || label != `supergroup "scout analytics" (id 7)` {
		t.Fatalf("%q %v", label, ok)
	}
	migrated := &tg.Chat{ID: 1, Title: "old"}
	migrated.SetMigratedTo(&tg.InputChannel{ChannelID: 7, AccessHash: 9})
	if _, _, ok := chatPeer(migrated); ok {
		t.Fatal("a migrated basic group must not be used")
	}
}

// fakeInviteHash is a made-up invite hash; no test output may contain it.
const fakeInviteHash = "AbCdEfGh123"

func TestRedactInviteLink(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://t.me/+" + fakeInviteHash, "https://t.me/+<invite>"},
		{"t.me/+" + fakeInviteHash, "t.me/+<invite>"},
		{"https://t.me/joinchat/" + fakeInviteHash, "https://t.me/joinchat/<invite>"},
		{"t.me/joinchat/" + fakeInviteHash, "t.me/joinchat/<invite>"},
		{"tg://join?invite=" + fakeInviteHash, "tg://join?invite=<invite>"},
		{"+" + fakeInviteHash, "+<invite>"},
		// not resolved as invite links by inviteLinkRe, still hidden
		{"HTTPS://T.ME/+" + fakeInviteHash, "HTTPS://T.ME/+<invite>"},
		{"https://telegram.me/joinchat/" + fakeInviteHash, "https://telegram.me/joinchat/<invite>"},
		{"https://t.me/+AbC", "https://t.me/+<invite>"},
		// inside a longer text (an error message)
		{`no chat matching "https://t.me/+` + fakeInviteHash + `" in your dialogs`, `no chat matching "https://t.me/+<invite>" in your dialogs`},
		// not secret: unchanged
		{"@scoutanalytics", "@scoutanalytics"},
		{"https://t.me/scoutanalytics", "https://t.me/scoutanalytics"},
		{"me", "me"},
		{"-1001234567890", "-1001234567890"},
		{"bot API chat -1001234567890", "bot API chat -1001234567890"},
		{"scout analytics", "scout analytics"},
		{"+1234", "+1234"}, // too short for inviteLinkRe: a chat id, not a hash
		{"", ""},
	} {
		if got := redactInviteLink(tc.in); got != tc.want {
			t.Errorf("redactInviteLink(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every link resolveNotify treats as an invite link must be redacted.
func TestRedactInviteLinkCoversInviteLinkRe(t *testing.T) {
	for _, in := range []string{
		"https://t.me/+" + fakeInviteHash, "t.me/+" + fakeInviteHash, "https://t.me/joinchat/" + fakeInviteHash,
		"tg://join?invite=" + fakeInviteHash, "+" + fakeInviteHash,
	} {
		if !inviteLinkRe.MatchString(in) {
			t.Fatalf("inviteLinkRe does not match %q: update this test", in)
		}
		if got := redactInviteLink(in); strings.Contains(got, fakeInviteHash) {
			t.Errorf("redactInviteLink(%q) = %q, still has the hash", in, got)
		}
	}
}

func TestListeningLine(t *testing.T) {
	tools := []ToolSpec{{Bot: "perceptorbot", Command: "/scan {ca}", Gate: true}, {Bot: "salphabot", Command: "{ca}"}}
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"invite link", config{SourceChannel: "src", Tools: tools, NotifyPeer: "https://t.me/+" + fakeInviteHash},
			`listening to @src → @perceptorbot "/scan {ca}" [gate], @salphabot "{ca}" → deliver to https://t.me/+<invite> (dry-run=false)`},
		{"joinchat link", config{SourceChannel: "src", NotifyPeer: "https://t.me/joinchat/" + fakeInviteHash, DryRun: true},
			`listening to @src →  → deliver to https://t.me/joinchat/<invite> (dry-run=true)`},
		{"public username", config{SourceChannel: "src", NotifyPeer: "@scoutanalytics"},
			`listening to @src →  → deliver to @scoutanalytics (dry-run=false)`},
		{"bot API", config{SourceChannel: "src", NotifyPeer: "https://t.me/+" + fakeInviteHash, NotifyBotToken: "x", NotifyChatID: "-1001234567890"},
			`listening to @src →  → deliver to bot API chat -1001234567890 (dry-run=false)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listeningLine(&tc.cfg); got != tc.want {
				t.Errorf("listeningLine(NotifyPeer=%q):\n got %q\nwant %q", redactInviteLink(tc.cfg.NotifyPeer), got, tc.want)
			}
			// the stored target keeps the real value
			if tc.cfg.NotifyBotToken == "" && deliveryLabel(&tc.cfg) != tc.cfg.NotifyPeer {
				t.Errorf("deliveryLabel changed: got %q", redactInviteLink(deliveryLabel(&tc.cfg)))
			}
		})
	}
}

func TestReaskStartLine(t *testing.T) {
	for _, tc := range []struct {
		name       string
		target     string
		noFollowUp bool
		want       string
	}{
		{"invite link", "https://t.me/+" + fakeInviteHash, false,
			"salpha reask: on — every 5m; real reports of delivered calls go to https://t.me/+<invite> as a reply to the delivery"},
		{"joinchat link", "t.me/joinchat/" + fakeInviteHash, false,
			"salpha reask: on — every 5m; real reports of delivered calls go to t.me/joinchat/<invite> as a reply to the delivery"},
		{"public username", "@scoutanalytics", false,
			"salpha reask: on — every 5m; real reports of delivered calls go to @scoutanalytics as a reply to the delivery"},
		{"dry-run", "https://t.me/+" + fakeInviteHash, true,
			"salpha reask: on — every 5m; no follow-up is sent (-dry-run)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reaskStartLine("every 5m", tc.target, tc.noFollowUp); got != tc.want {
				t.Errorf("reaskStartLine(%q, %v):\n got %q\nwant %q", redactInviteLink(tc.target), tc.noFollowUp, got, tc.want)
			}
		})
	}
}
