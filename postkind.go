package main

import (
	"regexp"
	"strings"
)

// Post kinds (scout_calls.post_kind). The channel posts two kinds of messages
// that carry a contract address:
//
//	call:   🚨 EARLY CALL — $TOKEN · robinhood, followed by pool info, token info, …
//	update: ⚡ $TOKEN hit 3X called $21k → $63k peak since the call · dyor
//
// An update is about an EARLIER call. It is recorded, but it is not a call: it
// is not investigated, not delivered and not tracked.
const (
	PostKindCall   = "call"
	PostKindUpdate = "update"
)

var (
	// "hit 3X", "hit 2.5x", "hit 10 X": a multiplier update.
	reHitMultiple = regexp.MustCompile(`(?i)\bhit\s+[0-9]+(?:\.[0-9]+)?\s?x\b`)
	// Marks of a full call post that are not fields of CallMeta.
	reCallMark = regexp.MustCompile(`(?i)EARLY\s+CALL|Pool\s+Info|Token\s+Info`)
)

// isFullCall reports whether the text has the shape of a call post: the call
// header, a Pool Info / Token Info section, or any of the call's data lines
// that parseCallMeta reads (DEX, Mcap, Liq, Tax, Age, Holders, Proof, live
// buys). "called at $…" alone does not count: an update post may say it too.
func isFullCall(text string) bool {
	if reCallMark.MatchString(text) {
		return true
	}
	m, _ := parseCallMeta(text)
	return m.TokenSymbol != nil || m.Dex != nil || m.McapUSD != nil || m.LiqUSD != nil || m.TaxBuyPct != nil ||
		m.AgeText != nil || m.Holders != nil || m.ProofElite != nil || len(m.LiveBuys) > 0
}

// postKind classifies a channel post by its text. It is an update only when
// the text says "hit <number>x" and does not look like a full call; anything
// else, an empty text included, is a call.
func postKind(text string) string {
	if strings.TrimSpace(text) == "" || !reHitMultiple.MatchString(text) || isFullCall(text) {
		return PostKindCall
	}
	return PostKindUpdate
}
