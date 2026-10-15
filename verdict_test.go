package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A Perceptor answer that counts zero red flags ("0 red flags", "Red flags: 0",
// "zero red flags", "Red flags: none") means the same as "no red flags".
// Before, the "red flag" in it made levelFromLabel say red_flags (so a report
// page titled "$X: 0 red flags" was a red-flags verdict) and the text fallback
// say unknown or red_flags. A real count ("10 red flags", "Red flags: 2"), a
// bare "Red flags" and a contract address after the colon are still red flags.
func TestZeroRedFlags(t *testing.T) {
	for _, c := range []struct {
		label string
		want  string
	}{
		{"0 red flags", levelClean},
		{"0 Red Flags found", levelClean},
		{"Red flags: 0", levelClean},
		{"RED FLAGS: 0.", levelClean},
		{"Red flags: none", levelClean},
		{"red flag: 0", levelClean},
		{"zero red flags", levelClean},
		{"No red flags found", levelClean},
		{"Red flags", levelRedFlags},
		{"10 red flags", levelRedFlags},
		{"100 red flags", levelRedFlags},
		{"Red flags: 2", levelRedFlags},
		{"Red flags: 10", levelRedFlags},
		{"Red flags: 0x4444444444444444444444444444444444444444", levelRedFlags},
		{"Caution", levelCaution},
		{"not enough data yet", levelInsufficient},
	} {
		if got := levelFromLabel(c.label); got != c.want {
			t.Errorf("levelFromLabel(%q) = %s, want %s", c.label, got, c.want)
		}
	}
	// a zero count reads exactly as "no red flags" does, whatever follows
	for zero, none := range map[string]string{
		"0 red flags, 2 warnings":         "no red flags, 2 warnings",
		"Red flags: 0 | Caution":          "no red flags | Caution",
		"0 red flags; honeypot: red flag": "no red flags; honeypot: red flag",
	} {
		if got, want := levelFromLabel(zero), levelFromLabel(none); got != want {
			t.Errorf("levelFromLabel(%q) = %s, want %s as for %q", zero, got, want, none)
		}
	}

	page := `<html><head><meta property="og:title" content="$ZERO: 0 red flags | 0xPerceptor">` +
		`<meta property="og:description" content="$ZERO on-chain check: 0 red flags. Top 10 hold 12%."></head></html>`
	if v := parsePerceptorPage(page); v.Level != levelClean || v.Ticker != "$ZERO" || v.Label != "0 red flags" || v.Summary != "Top 10 hold 12%" {
		t.Errorf("parsePerceptorPage(title %q) = %+v, want level %s, ticker $ZERO, label %q, summary %q",
			"$ZERO: 0 red flags", v, levelClean, "0 red flags", "Top 10 hold 12%")
	}

	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for text, want := range map[string]string{
		"$ZERO on-chain check: 0 red flags.":                   levelClean,
		"$ZERO on-chain check: Red flags: 0. Top 10 hold 9%":   levelClean,
		"$ZERO on-chain check: zero red flags.":                levelClean,
		"$ZERO on-chain check: 0 red flags, caution.":          levelCaution,
		"$ZERO on-chain check: 0 red flags. ⚠️ owner can mint": levelCaution,
		"$ZERO on-chain check: 0 red flags. honeypot":          levelRedFlags,
		"$TWO on-chain check: Red flags: 2":                    levelRedFlags,
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("textVerdict(%q) = %s (label %q), want %s", text, v.Level, v.Label, want)
		}
	}
}

// A safe phrase with a number at its edge ("0 red flags", "0 warnings", "red
// flags: 0", "warnings: 0") is stripped by classify only where that number
// stands alone. Before, "0 red flags" was cut out of "10 red flags" (and "0
// warnings" out of "10 warnings", "red flags: 0" out of "Red flags: 0x…"), so
// the red flag or warning marker was lost and the verdict was unknown.
func TestClassifySafeCountStandalone(t *testing.T) {
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for text, want := range map[string]string{
		"$TEN on-chain check: 10 red flags.":               levelRedFlags,
		"$HUN on-chain check: 100 red flags.":              levelRedFlags,
		"$TWE on-chain check: 20 red flags":                levelRedFlags,
		"$TEN on-chain check: Red flags: 10":               levelRedFlags,
		"$HEX on-chain check: Red flags: 0x4444444444":     levelRedFlags,
		"$TEN on-chain check: 10 warnings":                 levelCaution,
		"$TEN on-chain check: Warnings: 05":                levelCaution,
		"$ZERO on-chain check: 0 red flags, 0 warnings":    levelClean,
		"$ZERO on-chain check: 0 red flags (0 warnings)":   levelClean,
		"$ZERO on-chain check: Red flags: 0. Warnings: 0.": levelClean,
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("textVerdict(%q) = %s (label %q), want %s", text, v.Level, v.Label, want)
		}
	}
	for text, want := range map[string]bool{
		"10 red flags":                true,
		"100 red flags":               true,
		"Red flags: 10":               true,
		"10 warnings":                 true,
		"0 red flags":                 false,
		"0 warnings, 0 red flags":     false,
		"red flags: 0":                false,
		"warnings: 0":                 false,
		"Honeypot: No. Mintable: No":  false,
		"Honeypot: not detected":      false, // "honeypot: no" still strips as a prefix
		"Mintable: false, Proxy: no":  false,
		"Honeypot: ✅ no red flags":    false,
		"0 red flags but 2 red flags": true,
	} {
		if got, m := classify(text, mk, sf); got != want {
			t.Errorf("classify(%q) = %v (marker %q), want %v", text, got, m, want)
		}
	}
}

func TestStripSafePhrase(t *testing.T) {
	for _, c := range []struct{ text, phrase, want string }{
		{"0 red flags", "0 red flags", " "},
		{"10 red flags", "0 red flags", "10 red flags"},
		{"x0 red flags", "0 red flags", "x0 red flags"},
		{"(0 red flags)", "0 red flags", "( )"},
		{"10 red flags, 0 red flags", "0 red flags", "10 red flags,  "},
		{"0 red flags; 100 red flags; 0 red flags", "0 red flags", " ; 100 red flags;  "},
		{"red flags: 0", "red flags: 0", " "},
		{"red flags: 0.", "red flags: 0", " ."},
		{"red flags: 0x44, red flags: 0", "red flags: 0", "red flags: 0x44,  "},
		{"red flags: 05", "red flags: 0", "red flags: 05"},
		{"✅0 warnings", "0 warnings", "✅ "},
		// no digit at an edge: plain substring replacement, as before
		{"honeypot: not detected", "honeypot: no", " t detected"},
		{"no red flags", "no red flag", " s"},
		{"honeypot: ✅✅", "honeypot: ✅", " ✅"},
		{"anything", "", "anything"},
	} {
		if got := stripSafePhrase(c.text, c.phrase); got != c.want {
			t.Errorf("stripSafePhrase(%q, %q) = %q, want %q", c.text, c.phrase, got, c.want)
		}
	}
}

// A red flag marker must start a word, and "non-" before it cancels it.
// Before, markers matched as substrings: "non-mintable" was a red flag
// ("mintable") and "precaution" a caution ("caution"). The end of the word is
// not checked, so "blacklisted" or "scammer" still match, as before.
func TestClassifyMarkerStartsWord(t *testing.T) {
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for _, c := range []struct {
		text, marker string // marker "" = not flagged
	}{
		// the two examples from HANDOFF.md
		{"Token is non-mintable", ""},
		{"Take precaution: low LP", ""},
		// inside other words, or after "non-"
		{"unmintable supply", ""},
		{"NON-MINTABLE", ""},
		{"non-mintable, non-honeypot", ""},
		{"endangered", ""},
		{"prewarning", ""},
		{"2mintable", ""},
		{"émintable", ""},
		// a word that starts with a marker matches, whatever follows
		{"dangerous", "danger"},
		{"Blacklisted: yes", "blacklist"},
		{"rugpulled", "rugpull"},
		{"scammer", "scam"},
		{"scamper", "scam"}, // known, accepted false red flag (PM, 15 Oct 2026)
		// still matched
		{"non-mintable but honeypot", "honeypot"},
		{"precaution, caution", "caution"},
		{"not mintable", "mintable"},           // only "non-" negates
		{"non mintable", "mintable"},           // only the hyphenated "non-"
		{"canon-mintable", "mintable"},         // "non-" must be a word of its own
		{"honeypot-like contract", "honeypot"}, // "-" is a boundary
		{"MINTABLE", "mintable"},
		{"Mintable: yes.", "mintable"},
		{"(honeypot)", "honeypot"},
		{"scam!", "scam"},
		{"Red flags: liquidity pulled", "red flag"},
		{"2 WARNINGS", "warning"},
		{"honeypots", "honeypot"},
		{"Owner: CAN'T SELL", "can't sell"},
		{"proxy contract detected", "proxy contract"},
		{"low LP⚠️", "⚠️"},
		{"🚩🚩", "🚩"},
		{"liquidity🔴pulled", "🔴"},
	} {
		t.Run(c.text, func(t *testing.T) {
			got, m := classify(c.text, mk, sf)
			if got != (c.marker != "") || m != c.marker {
				t.Errorf("classify(%q) = %v, %q; want %v, %q", c.text, got, m, c.marker != "", c.marker)
			}
		})
	}
}

// Every default marker still matches on its own, at the start or end of the
// text, next to punctuation, in upper case and with a suffix glued after it.
// A marker that starts with a letter or digit does not match with a letter or
// digit glued before it, or after "non-".
func TestDefaultMarkersMatchAtWordStart(t *testing.T) {
	sf := splitList(defaultSafe)
	for _, m := range splitList(defaultMarkers) {
		t.Run(m, func(t *testing.T) {
			match := []string{m, m + " at start", "at end " + m, "(" + m + ")", "- " + m + "!",
				strings.ToUpper(m), m + "s", m + "zz"}
			var noMatch []string
			if first, _ := utf8.DecodeRuneInString(m); isWordRune(first) {
				noMatch = append(noMatch, "zz"+m, "9"+m, "non-"+m)
			}
			for _, text := range match {
				if got, gotM := classify(text, []string{m}, sf); !got || gotM != m {
					t.Errorf("classify(%q, [%q]) = %v, %q; want true, %q", text, m, got, gotM, m)
				}
			}
			for _, text := range noMatch {
				if got, _ := classify(text, []string{m}, sf); got {
					t.Errorf("classify(%q, [%q]) = true, want false", text, m)
				}
			}
		})
	}
}

func TestContainsMarker(t *testing.T) {
	for _, c := range []struct {
		text, marker string
		want         bool
	}{
		{"mintable", "mintable", true},
		{"mintable yes", "mintable", true},
		{"yes mintable", "mintable", true},
		{"premintable mintable", "mintable", true}, // second occurrence stands alone
		{"non-mintable mintable", "mintable", true},
		{"premintable", "mintable", false},
		{"mintablex", "mintable", true}, // only the start of the word is checked
		{"non-mintable", "mintable", false},
		{"x-non-mintable", "mintable", false}, // "-" before "non-" is a boundary
		{"non-", "mintable", false},
		{"⚠️low", "⚠️", true},
		{"anything", "", false},
		{"", "mintable", false},
	} {
		if got := containsMarker(c.text, c.marker); got != c.want {
			t.Errorf("containsMarker(%q, %q) = %v, want %v", c.text, c.marker, got, c.want)
		}
	}
}

// What the word-start rule for markers changes for a reply without a report
// link.
func TestTextVerdictMarkerWordStart(t *testing.T) {
	mk, sf := splitList(defaultMarkers), splitList(defaultSafe)
	for text, want := range map[string]string{
		"No red flags found. Token is non-mintable.":  levelClean,    // was red_flags
		"No red flags found. Take precaution: low LP": levelClean,    // was caution
		"$X check: non-mintable, LP locked":           levelUnknown,  // was red_flags
		"No red flags found. Mintable: yes":           levelRedFlags, // unchanged
		"$NH on-chain check: red flags.":              levelRedFlags, // unchanged
		"$X check: 2 warnings":                        levelCaution,  // unchanged
		"$X check: Blacklisted: yes":                  levelRedFlags, // unchanged
		"$X check: dev is a scammer":                  levelRedFlags, // unchanged
	} {
		if v := textVerdict(text, mk, sf); v.Level != want {
			t.Errorf("textVerdict(%q) = %s (label %q), want %s", text, v.Level, v.Label, want)
		}
	}
}
