package main

import (
	"errors"
	"fmt"
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
