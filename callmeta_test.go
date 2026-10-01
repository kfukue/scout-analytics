package main

import (
	"strings"
	"testing"
)

// Modeled on https://t.me/scoutrobinhood/10120 (values are made up).
const samplePost = `🚨 EARLY CALL — $MALFOID · robinhood
💰 called at $45.2k

📊 Pool Info
🏛 DEX: Longxyz
📈 Mcap: $52k
💧 Liq: $20k | 38%
🧾 Tax: B 0% | S 2.5%

🪙 Token Info
⏱ Age: 12m · 🚀 Longxyz
👥 Holders: 1,150

🔎 Proof: 3 elite + 5 good holding — validated on-chain

📹 Live buys (💎 elite · ✅ good)
💎 $1.2k · 0x79f6…5c5d
✅ $350 · 0x12ab…9f00
💎 $800 · 0xabcd…0001
✅ $95.5 · 0x9999…aaaa

⚠️ dyor, nfa`

func f(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}
func i(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestParseCallMeta(t *testing.T) {
	m, ok := parseCallMeta(samplePost)
	if !ok {
		t.Fatal("nothing parsed")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"symbol", str(m.TokenSymbol), "MALFOID"},
		{"chain", str(m.ChainName), "robinhood"},
		{"called at", f(m.CalledAtMcapUSD), 45200.0},
		{"dex", str(m.Dex), "Longxyz"},
		{"mcap", f(m.McapUSD), 52000.0},
		{"liq", f(m.LiqUSD), 20000.0},
		{"liq %", f(m.LiqPct), 38.0},
		{"tax buy", f(m.TaxBuyPct), 0.0},
		{"tax sell", f(m.TaxSellPct), 2.5},
		{"age text", str(m.AgeText), "12m"},
		{"age s", i(m.AgeSeconds), 720},
		{"launchpad", str(m.Launchpad), "Longxyz"},
		{"holders", i(m.Holders), 1150},
		{"proof elite", i(m.ProofElite), 3},
		{"proof good", i(m.ProofGood), 5},
		{"live buys", len(m.LiveBuys), 4},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
	b := m.LiveBuys
	if b[0].Tier != "elite" || f(b[0].AmountUSD) != 1200 || b[0].WalletPrefix != "0x79f6" || b[0].WalletSuffix != "5c5d" || b[0].Position != 1 {
		t.Errorf("buy 1 = %+v", b[0])
	}
	if b[1].Tier != "good" || f(b[1].AmountUSD) != 350 || b[3].Tier != "good" || f(b[3].AmountUSD) != 95.5 {
		t.Errorf("buys = %+v", b)
	}
	en, gn, eu, gu := m.LiveBuyTotals()
	if en != 2 || gn != 2 || eu != 2000 || gu != 445.5 {
		t.Errorf("totals %d %d %v %v", en, gn, eu, gu)
	}
	line := m.SummaryLine()
	for _, want := range []string{"MCap $52k", "Liq $20k (38%)", "Tax 0/2.5%", "Age 12m", "Holders 1150", "Proof 💎3 ✅5", "Live buys: 💎 2 ($2k) · ✅ 2 ($446)"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary missing %q:\n%s", want, line)
		}
	}
}

func TestParseCallMetaVariants(t *testing.T) {
	m, ok := parseCallMeta("📈 MCap: $1.25M\n💧 Liq: $310K\n⏱️ Age: 2h 5m\n🧾 Tax: Buy 5% / Sell 5%\n👥 Holders: 87")
	if !ok || f(m.McapUSD) != 1250000 || f(m.LiqUSD) != 310000 || m.LiqPct != nil ||
		i(m.AgeSeconds) != 7500 || m.Launchpad != nil || f(m.TaxBuyPct) != 5 || f(m.TaxSellPct) != 5 || i(m.Holders) != 87 {
		t.Fatalf("%+v", m)
	}
	if _, ok := parseCallMeta("gm, new call soon"); ok {
		t.Fatal("plain text should not parse")
	}
	// wallet-looking lines BEFORE the live-buys header are not buys
	m, _ = parseCallMeta("💎 $500 · 0x1111…2222\n📹 Live buys\n✅ $10 · 0x3333...4444")
	if len(m.LiveBuys) != 1 || m.LiveBuys[0].WalletSuffix != "4444" || m.LiveBuys[0].Tier != "good" {
		t.Fatalf("%+v", m.LiveBuys)
	}
	for in, want := range map[float64]string{950: "$950", 45000: "$45k", 45200: "$45.2k", 1250000: "$1.25M", 2000000: "$2M"} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v)=%s want %s", in, got, want)
		}
	}
}
