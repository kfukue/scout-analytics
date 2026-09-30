package main

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestExtractCAs(t *testing.T) {
	chains := map[string]bool{"evm": true, "solana": true}
	text := "New gem! CA: 0x1234567890abcdef1234567890ABCDEF12345678 \nsol: 7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU and again 0x1234567890ABCDEF1234567890abcdef12345678"
	got := extractCAs(text, []string{"https://dexscreener.com/robinhood/0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, chains)
	if len(got) != 3 {
		t.Fatalf("want 3 CAs, got %v", got)
	}
	if n := len(extractCAs("hello world https://t.me/scoutrobinhood/12", nil, chains)); n != 0 {
		t.Fatalf("want 0, got %d", n)
	}
}

func TestClassify(t *testing.T) {
	m, s := splitList(defaultMarkers), splitList(defaultSafe)
	cases := map[string]bool{
		"Token looks fine. No red flags found. Honeypot: No": false,
		"⚠️ Owner can mint":                                  true,
		"🚩 Red flag: liquidity not locked":                   true,
		"Security: ✅ verified, ✅ renounced, LP burned 100%":  false,
		"WARNING: high tax":                                  true,
	}
	for in, want := range cases {
		if got, mk := classify(in, m, s); got != want {
			t.Errorf("%q: got %v (%q), want %v", in, got, mk, want)
		}
	}
}

// Page heads modeled on the three real examples:
// red flags  https://www.perceptor.info/r/6faada809b4c4c158d32ed11fe1604bf
// clean      https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0
// caution    https://www.perceptor.info/r/d3e4fa0b496b456fbdc9499ae5fcf5f2
func TestParsePerceptorPage(t *testing.T) {
	page := func(title, desc string) string {
		return `<html><head><title>` + title + `</title>
<meta name="description" content="` + desc + `"/>
<meta property="og:title" content="` + title + `"/>
<meta property="og:description" content="` + desc + `"/></head></html>`
	}
	cases := []struct {
		html, level, ticker, label, summary string
	}{
		{page("$NH: Red flags | 0xPerceptor", "$NH on-chain check: red flags. Top 10 hold 40%; Liquidity PULLED. Full breakdown via @0xPerceptor"),
			levelRedFlags, "$NH", "Red flags", "Top 10 hold 40%; Liquidity PULLED"},
		{page("$ANYR: No red flags found | 0xPerceptor", "$ANYR on-chain check: no red flags found. Full breakdown via @0xPerceptor"),
			levelClean, "$ANYR", "No red flags found", ""},
		{page("$DARKCOMP: Caution | 0xPerceptor", "$DARKCOMP on-chain check: caution. Full breakdown via @0xPerceptor"),
			levelCaution, "$DARKCOMP", "Caution", ""},
	}
	for _, c := range cases {
		v := parsePerceptorPage(c.html)
		if v.Level != c.level || v.Ticker != c.ticker || v.Label != c.label || v.Summary != c.summary {
			t.Errorf("got %+v, want level=%s ticker=%s label=%s summary=%q", v, c.level, c.ticker, c.label, c.summary)
		}
	}
}

func TestPerceptorIDAndTextFallback(t *testing.T) {
	m := &tg.Message{Message: "Done. https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0"}
	if id := perceptorID(m); id != "deb9d3118ec1480e985032f9472c87c0" {
		t.Fatalf("id=%q", id)
	}
	btn := &tg.Message{ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonURL{Text: "Full report", URL: "https://www.perceptor.info/?investigation=d3e4fa0b496b456fbdc9499ae5fcf5f2"}}}}}}
	if id := perceptorID(btn); id != "d3e4fa0b496b456fbdc9499ae5fcf5f2" {
		t.Fatalf("button id=%q", id)
	}
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for text, want := range map[string]string{
		"$ANYR on-chain check: no red flags found.": levelClean,
		"$DARKCOMP on-chain check: caution.":        levelCaution,
		"$NH on-chain check: red flags.":            levelRedFlags,
		"Token not found":                           levelUnknown,
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("%q: got %s want %s", text, v.Level, want)
		}
	}
}

func TestFallbackLevels(t *testing.T) {
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for text, want := range map[string]string{
		"⚠️ owner can change tax":       levelCaution,
		"Possible honeypot":             levelRedFlags,
		"Likely scam, rug pull":         levelRedFlags,
		"No red flags found. ⚠️ low LP": levelCaution,
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("%q: got %s want %s", text, v.Level, want)
		}
	}
}
