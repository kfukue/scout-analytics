package main

import (
	"testing"

	"github.com/gotd/td/tg"
)

// A call like https://t.me/scoutrobinhood/10002: the token CA is only behind a
// button / text link, the visible text has shortened wallets, and the wallets
// link to explorer address pages that must NOT be taken as CAs.
func TestCallWithCAOnlyInButtonsAndWalletLinks(t *testing.T) {
	token := "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	wallet1 := "0x79f6000000000000000000000000000000005c5d"
	wallet2 := "0x1111222233334444555566667777888899990000"
	msg := &tg.Message{
		ID:      10002,
		Message: "🚨 EARLY CALL $MALFOID\nMC 45k | Liq 20k | DEX Longxyz\nLive buys:\n0x79f6…5c5d 0.5 ETH\n0x1111…0000 0.3 ETH\ndyor, nfa",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{URL: "https://explorer.robinhood.com/address/" + wallet1},
			&tg.MessageEntityTextURL{URL: "https://explorer.robinhood.com/tx/0x" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"},
			&tg.MessageEntityTextURL{URL: "https://debank.com/profile/" + wallet2},
		},
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonURL{Text: "📈 Chart", URL: "https://dexscreener.com/robinhood/" + token},
			&tg.KeyboardButtonURL{Text: "Buy", URL: "https://t.me/somebuybot?start=" + token},
		}}}},
	}
	cas := extractCAs(msg.Message, postURLs(msg), map[string]bool{"evm": true, "solana": true})
	if len(cas) != 1 || caKey(cas[0]) != caKey(token) {
		t.Fatalf("want only the token CA, got %v", cas)
	}
	// "copy CA" and other button kinds are read too
	msg2 := &tg.Message{ID: 1, ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCopy{Text: "Copy CA", CopyText: token},
	}}}}}
	if cas := extractCAs(msg2.Message, postURLs(msg2), map[string]bool{"evm": true}); len(cas) != 1 {
		t.Fatalf("copy button: %v", cas)
	}
}

// Live update, edit and polling can all deliver the same post: each (post, CA)
// is queued once; an edit that adds a CA later is picked up.
func TestOnChannelPostDedupAndEdit(t *testing.T) {
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir()}
	s := newScanner(cfg)
	ca := "0x2222222222222222222222222222222222222222"

	s.onChannelPost(&tg.Message{ID: 7, Message: "🚨 call coming, CA soon"}) // no CA yet
	s.onChannelPost(&tg.Message{ID: 7, Message: "🚨 call CA: " + ca})       // edit adds it
	s.onChannelPost(&tg.Message{ID: 7, Message: "🚨 call CA: " + ca})       // poll sees it again
	if n := len(s.queue); n != 1 {
		t.Fatalf("queued %d jobs, want 1", n)
	}
	j := <-s.queue
	if j.CA != ca || j.SourceMsg != 7 || j.SourceText == "" {
		t.Fatalf("job = %+v", j)
	}
	// the same CA re-posted later is a duplicate call, not re-investigated
	s.onChannelPost(&tg.Message{ID: 9, Message: "again " + ca})
	if len(s.queue) != 0 {
		t.Fatal("re-post of an investigated CA should not be queued")
	}
}

func TestMessagesOfSorted(t *testing.T) {
	res := &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 30}, &tg.MessageService{ID: 25}, &tg.Message{ID: 10}, &tg.Message{ID: 20},
	}}
	got := messagesOf(res)
	if len(got) != 3 || got[0].ID != 10 || got[2].ID != 30 {
		t.Fatalf("%v", got)
	}
}
