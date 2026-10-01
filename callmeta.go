package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CallMeta is the structured data in a @scoutrobinhood call post:
//
//	🚨 EARLY CALL — $TOKEN · robinhood
//	💰 called at $45k
//	📊 Pool Info
//	🏛 DEX: Longxyz
//	📈 Mcap: $52k
//	💧 Liq: $20k | 38%
//	🧾 Tax: B 0% | S 0%
//	🪙 Token Info
//	⏱ Age: 12m · 🚀 Longxyz
//	👥 Holders: 150
//	🔎 Proof: 3 elite + 5 good holding — validated on-chain
//	📹 Live buys (💎 elite · ✅ good)
//	💎 $1.2k · 0x79f6…5c5d
//	✅ $350 · 0x12ab…9f00
//	⚠️ dyor, nfa
//
// Every field is optional (nil when the post doesn't contain it).
type CallMeta struct {
	TokenSymbol     *string   `json:"token_symbol,omitempty"`
	ChainName       *string   `json:"chain_name,omitempty"`
	CalledAtMcapUSD *float64  `json:"called_at_mcap_usd,omitempty"`
	Dex             *string   `json:"dex,omitempty"`
	McapUSD         *float64  `json:"mcap_usd,omitempty"`
	LiqUSD          *float64  `json:"liq_usd,omitempty"`
	LiqPct          *float64  `json:"liq_pct,omitempty"`
	TaxBuyPct       *float64  `json:"tax_buy_pct,omitempty"`
	TaxSellPct      *float64  `json:"tax_sell_pct,omitempty"`
	AgeText         *string   `json:"age_text,omitempty"`
	AgeSeconds      *int      `json:"age_seconds,omitempty"`
	Launchpad       *string   `json:"launchpad,omitempty"`
	Holders         *int      `json:"holders,omitempty"`
	ProofElite      *int      `json:"proof_elite,omitempty"`
	ProofGood       *int      `json:"proof_good,omitempty"`
	LiveBuys        []LiveBuy `json:"live_buys,omitempty"`
}

// LiveBuy is one line of the "Live buys" section.
type LiveBuy struct {
	Position      int      `json:"position"` // 1-based order in the post
	Tier          string   `json:"tier"`     // elite | good | other
	TierEmoji     string   `json:"tier_emoji"`
	AmountUSD     *float64 `json:"amount_usd,omitempty"`
	WalletDisplay string   `json:"wallet_display"` // as shown, e.g. 0x79f6…5c5d
	WalletPrefix  string   `json:"wallet_prefix"`  // 0x79f6
	WalletSuffix  string   `json:"wallet_suffix"`  // 5c5d
}

// Summary counts for the live buys.
func (m *CallMeta) LiveBuyTotals() (eliteN, goodN int, eliteUSD, goodUSD float64) {
	for _, b := range m.LiveBuys {
		amt := 0.0
		if b.AmountUSD != nil {
			amt = *b.AmountUSD
		}
		switch b.Tier {
		case "elite":
			eliteN++
			eliteUSD += amt
		case "good":
			goodN++
			goodUSD += amt
		}
	}
	return
}

// Empty reports whether nothing was recognised.
func (m *CallMeta) Empty() bool {
	return m == nil || (m.TokenSymbol == nil && m.McapUSD == nil && m.LiqUSD == nil && m.Holders == nil &&
		m.ProofElite == nil && m.TaxBuyPct == nil && m.AgeSeconds == nil && len(m.LiveBuys) == 0)
}

const num = `\$?\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*([kKmMbB])?`

var (
	reHeader   = regexp.MustCompile(`(?i)EARLY\s+CALL\s*[—–\-:]*\s*\$([A-Za-z0-9_.\-]+)(?:\s*[·•|]\s*([^\n]+))?`)
	reCalledAt = regexp.MustCompile(`(?i)called\s+at\s*` + num)
	reDex      = regexp.MustCompile(`(?i)\bDEX\s*:\s*([^\n|·]+)`)
	reMcap     = regexp.MustCompile(`(?i)\bM\.?\s?cap\s*:\s*` + num)
	reLiq      = regexp.MustCompile(`(?i)\bLiq(?:uidity)?\s*:\s*` + num + `(?:\s*[|·/]\s*([0-9]+(?:\.[0-9]+)?)\s*%)?`)
	reTax      = regexp.MustCompile(`(?i)\bTax(?:es)?\s*:\s*B(?:uy)?\s*:?\s*([0-9]+(?:\.[0-9]+)?)\s*%\s*[|/·,]\s*S(?:ell)?\s*:?\s*([0-9]+(?:\.[0-9]+)?)\s*%`)
	reAge      = regexp.MustCompile(`(?im)\bAge\s*:\s*([^·•|\n]+?)\s*(?:[·•|]\s*(?:🚀\s*)?([^\n]+?))?\s*$`)
	reHolders  = regexp.MustCompile(`(?i)\bHolders\s*:\s*([0-9][0-9,]*)`)
	reProof    = regexp.MustCompile(`(?i)\bProof\s*:\s*([0-9]+)\s*elite\s*\+\s*([0-9]+)\s*good`)
	reLiveHdr  = regexp.MustCompile(`(?i)live\s+buys`)
	reBuyLine  = regexp.MustCompile(`^\s*(\S+?)\s*` + num + `\s*[·•|\-]\s*(0x[0-9a-fA-F]+)\s*(?:…|\.\.\.|\.\.)\s*([0-9a-fA-F]+)`)
	reAgePart  = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(s|sec|secs|seconds?|m|min|mins|minutes?|h|hr|hrs|hours?|d|days?|w|weeks?)\b`)
)

// parseUSD turns "45", "45k", "1.2M", "1,234" (+ suffix) into dollars.
func parseUSD(numStr, suffix string) *float64 {
	v, err := strconv.ParseFloat(strings.ReplaceAll(numStr, ",", ""), 64)
	if err != nil {
		return nil
	}
	switch strings.ToLower(suffix) {
	case "k":
		v *= 1e3
	case "m":
		v *= 1e6
	case "b":
		v *= 1e9
	}
	return &v
}

func parseFloat(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func parseInt(s string) *int {
	v, err := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	if err != nil {
		return nil
	}
	return &v
}

// parseAgeSeconds handles "12m", "2h 5m", "1d 3h", "45s", "3 days".
func parseAgeSeconds(s string) *int {
	total := 0.0
	found := false
	for _, m := range reAgePart.FindAllStringSubmatch(s, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		found = true
		switch u := strings.ToLower(m[2]); {
		case strings.HasPrefix(u, "s"):
			total += v
		case strings.HasPrefix(u, "m"):
			total += v * 60
		case strings.HasPrefix(u, "h"):
			total += v * 3600
		case strings.HasPrefix(u, "d"):
			total += v * 86400
		case strings.HasPrefix(u, "w"):
			total += v * 7 * 86400
		}
	}
	if !found {
		return nil
	}
	n := int(total)
	return &n
}

func trimPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func tierOf(emoji string) string {
	switch {
	case strings.Contains(emoji, "💎"):
		return "elite"
	case strings.Contains(emoji, "✅"), strings.Contains(emoji, "✔"):
		return "good"
	}
	return "other"
}

// parseCallMeta extracts CallMeta from a call post. ok=false if nothing matched.
func parseCallMeta(text string) (*CallMeta, bool) {
	m := &CallMeta{}
	if x := reHeader.FindStringSubmatch(text); x != nil {
		m.TokenSymbol = trimPtr(x[1])
		m.ChainName = trimPtr(x[2])
	}
	if x := reCalledAt.FindStringSubmatch(text); x != nil {
		m.CalledAtMcapUSD = parseUSD(x[1], x[2])
	}
	if x := reDex.FindStringSubmatch(text); x != nil {
		m.Dex = trimPtr(x[1])
	}
	if x := reMcap.FindStringSubmatch(text); x != nil {
		m.McapUSD = parseUSD(x[1], x[2])
	}
	if x := reLiq.FindStringSubmatch(text); x != nil {
		m.LiqUSD = parseUSD(x[1], x[2])
		if x[3] != "" {
			m.LiqPct = parseFloat(x[3])
		}
	}
	if x := reTax.FindStringSubmatch(text); x != nil {
		m.TaxBuyPct, m.TaxSellPct = parseFloat(x[1]), parseFloat(x[2])
	}
	if x := reAge.FindStringSubmatch(text); x != nil {
		m.AgeText = trimPtr(x[1])
		m.AgeSeconds = parseAgeSeconds(x[1])
		m.Launchpad = trimPtr(x[2])
	}
	if x := reHolders.FindStringSubmatch(text); x != nil {
		m.Holders = parseInt(x[1])
	}
	if x := reProof.FindStringSubmatch(text); x != nil {
		m.ProofElite, m.ProofGood = parseInt(x[1]), parseInt(x[2])
	}

	// Live buys: lines after the "Live buys" header that look like "<emoji> $<amt> · 0x…".
	inBuys := false
	for _, line := range strings.Split(text, "\n") {
		if !inBuys {
			if reLiveHdr.MatchString(line) {
				inBuys = true
			}
			continue
		}
		x := reBuyLine.FindStringSubmatch(line)
		if x == nil {
			continue
		}
		m.LiveBuys = append(m.LiveBuys, LiveBuy{
			Position:      len(m.LiveBuys) + 1,
			Tier:          tierOf(x[1]),
			TierEmoji:     x[1],
			AmountUSD:     parseUSD(x[2], x[3]),
			WalletDisplay: x[4] + "…" + x[5],
			WalletPrefix:  x[4],
			WalletSuffix:  x[5],
		})
	}
	return m, !m.Empty()
}

// usd formats dollars compactly: $950, $45k, $1.2M.
func usd(v float64) string {
	switch {
	case v >= 1e6:
		return "$" + strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", v/1e6), "0"), ".0") + "M"
	case v >= 1e3:
		return "$" + strings.TrimSuffix(fmt.Sprintf("%.1f", v/1e3), ".0") + "k"
	}
	return fmt.Sprintf("$%.0f", v)
}

// SummaryLine is a compact line for the delivery header.
func (m *CallMeta) SummaryLine() string {
	if m.Empty() {
		return ""
	}
	var parts []string
	if m.McapUSD != nil {
		parts = append(parts, "MCap "+usd(*m.McapUSD))
	}
	if m.LiqUSD != nil {
		p := "Liq " + usd(*m.LiqUSD)
		if m.LiqPct != nil {
			p += fmt.Sprintf(" (%g%%)", *m.LiqPct)
		}
		parts = append(parts, p)
	}
	if m.TaxBuyPct != nil && m.TaxSellPct != nil {
		parts = append(parts, fmt.Sprintf("Tax %g/%g%%", *m.TaxBuyPct, *m.TaxSellPct))
	}
	if m.AgeText != nil {
		parts = append(parts, "Age "+*m.AgeText)
	}
	if m.Holders != nil {
		parts = append(parts, fmt.Sprintf("Holders %d", *m.Holders))
	}
	if m.ProofElite != nil || m.ProofGood != nil {
		e, g := 0, 0
		if m.ProofElite != nil {
			e = *m.ProofElite
		}
		if m.ProofGood != nil {
			g = *m.ProofGood
		}
		parts = append(parts, fmt.Sprintf("Proof 💎%d ✅%d", e, g))
	}
	line := "📊 " + strings.Join(parts, " · ")
	if len(m.LiveBuys) > 0 {
		en, gn, eu, gu := m.LiveBuyTotals()
		line += fmt.Sprintf("\nLive buys: 💎 %d (%s) · ✅ %d (%s)", en, usd(eu), gn, usd(gu))
	}
	return line
}
