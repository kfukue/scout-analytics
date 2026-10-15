package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// Perceptor reports have exactly three verdicts (as shown in the page title):
//
//	"$ANYR: No red flags found | 0xPerceptor"  -> clean
//	"$DARKCOMP: Caution | 0xPerceptor"          -> caution
//	"$NH: Red flags | 0xPerceptor"              -> red_flags
const (
	levelClean    = "clean"
	levelCaution  = "caution"
	levelRedFlags = "red_flags"
	levelUnknown  = "unknown"
)

// Perceptor's answer for a token too new to judge is a fourth one:
//
//	"$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor"
//
// It is no verdict: it never passes the gate unless SCOUT_DELIVER_LEVELS names
// it, and on the website it counts as "not scanned". A live call that gets it
// is re-checked later (recheck.go).
const (
	levelInsufficient = "insufficient"
	// labelInsufficient: the verdict_label of every insufficient row, the one
	// scoutanalytics.sql also writes when it relabels old rows.
	labelInsufficient = "not enough data yet"
)

// verdictUsable reports whether level is a real Perceptor verdict (clean,
// caution or red flags), as opposed to insufficient or unknown.
func verdictUsable(level string) bool {
	switch level {
	case levelClean, levelCaution, levelRedFlags:
		return true
	}
	return false
}

// insufficientTickerRe finds the ticker in "$SNIFF on-chain check: not enough data yet".
var insufficientTickerRe = regexp.MustCompile(`(?i)(\$[^\s$]+)\s+on-chain check`)

type verdict struct {
	Level   string `json:"level"`
	Label   string `json:"label,omitempty"`   // e.g. "No red flags found"
	Ticker  string `json:"ticker,omitempty"`  // e.g. "$ANYR"
	Summary string `json:"summary,omitempty"` // og:description, e.g. "Top 10 hold 40%; Liquidity PULLED"
	URL     string `json:"url,omitempty"`
	Source  string `json:"source"` // "perceptor.info" or "telegram_text"
}

var (
	perceptorIDRe = regexp.MustCompile(`perceptor\.info/(?:r/|\?investigation=)([a-fA-F0-9]{32})`)
	ogTitleRe     = regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)=["']og:title["'][^>]*content=["']([^"']*)["']`)
	ogTitleRe2    = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]*(?:property|name)=["']og:title["']`)
	ogDescRe      = regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)=["']og:description["'][^>]*content=["']([^"']*)["']`)
	ogDescRe2     = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]*(?:property|name)=["']og:description["']`)
	titleRe       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// zeroRedFlagsRe finds a count of zero red flags ("0 red flags", "zero red
// flags", "Red flags: 0", "Red flags: none"), which means the same as "no red
// flags". The boundaries keep "10 red flags" and "Red flags: 0x…" out.
var zeroRedFlagsRe = regexp.MustCompile(`(?i)\b(?:0|zero)\s+red\s+flags?\b|\bred\s+flags?\s*:\s*(?:0|none|zero)\b`)

// zeroRedFlagsAsNone rewrites every zero count of red flags as "no red flags",
// so that the "red flag" in it is not read as a red flag (by levelFromLabel or
// by the red flag markers; "no red flags" is a default safe phrase).
func zeroRedFlagsAsNone(s string) string {
	return zeroRedFlagsRe.ReplaceAllString(s, "no red flags")
}

// stripSafePhrase replaces each occurrence of the safe phrase p in t with a
// space, as strings.ReplaceAll would, except where a digit at an edge of p
// touches a letter or digit next to it in t: that number is then part of a
// longer one, so "0 red flags" stays in "10 red flags", "0 warnings" in "20
// warnings" and "red flags: 0" in "red flags: 0x…". Phrases with no digit at
// an edge ("honeypot: no", "no red flag") still match anywhere, as before.
func stripSafePhrase(t, p string) string {
	if p == "" {
		return t
	}
	first, _ := utf8.DecodeRuneInString(p)
	last, _ := utf8.DecodeLastRuneInString(p)
	checkStart, checkEnd := unicode.IsDigit(first), unicode.IsDigit(last)
	if !checkStart && !checkEnd {
		return strings.ReplaceAll(t, p, " ")
	}
	var b strings.Builder
	done, from := 0, 0 // t[:done] is written to b; search again from t[from:]
	for {
		k := strings.Index(t[from:], p)
		if k < 0 {
			break
		}
		i, j := from+k, from+k+len(p)
		before, _ := utf8.DecodeLastRuneInString(t[:i]) // RuneError at the start
		after, _ := utf8.DecodeRuneInString(t[j:])      // RuneError at the end
		if (checkStart && isWordRune(before)) || (checkEnd && isWordRune(after)) {
			from = i + len(string(first)) // not standalone: look further on
			continue
		}
		b.WriteString(t[done:i])
		b.WriteByte(' ')
		done, from = j, j
	}
	if done == 0 {
		return t
	}
	b.WriteString(t[done:])
	return b.String()
}

// isWordRune reports whether r is part of a word: a letter or a digit. Any
// other rune ("-", "_", "'", spaces, punctuation, emoji) is a word boundary.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// markerNegation right before a marker cancels it: "non-mintable" is not
// "mintable". Only this one hyphenated prefix, as a word of its own ("canon-"
// does not count); "not mintable" and "non mintable" still match.
const markerNegation = "non-"

// containsMarker reports whether the red flag marker m occurs in t at the
// start of a word (markerStartsWord). So "caution" is not found in
// "precaution", nor "mintable" in "unmintable" or "non-mintable", while "🚩"
// is found anywhere. Only the start is checked: a word that begins with a
// marker counts ("red flags", "blacklisted", "scammer", and also "scamper"),
// because missing a real red flag is worse than a false one. t and m are
// lowercase (classify lowercases t, splitList m).
func containsMarker(t, m string) bool {
	if m == "" {
		return false
	}
	_, size := utf8.DecodeRuneInString(m)
	for from := 0; ; {
		k := strings.Index(t[from:], m)
		if k < 0 {
			return false
		}
		i := from + k
		if markerStartsWord(t, m, i) {
			return true
		}
		from = i + size // not at the start of a word: look further on
	}
}

// markerStartsWord reports whether the marker m found at t[i:] starts a word.
// A marker that starts with a non-word rune ("🚩", "⚠️") always does. One
// that starts with a letter or digit must not follow a letter or digit, nor
// the negation "non-".
func markerStartsWord(t, m string, i int) bool {
	first, _ := utf8.DecodeRuneInString(m)
	if !isWordRune(first) {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(t[:i]) // RuneError at the start
	return !isWordRune(before) && !negatedAt(t, i)
}

// negatedAt reports whether t[:i] ends in the word markerNegation ("non-").
func negatedAt(t string, i int) bool {
	head, ok := strings.CutSuffix(t[:i], markerNegation)
	if !ok {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(head) // RuneError at the start
	return !isWordRune(before)
}

// levelFromLabel maps a verdict phrase to a level. Order matters:
// "no red flags" (or a zero count of them) must be checked before "red flag".
func levelFromLabel(s string) string {
	t := strings.ToLower(zeroRedFlagsAsNone(s))
	switch {
	case strings.Contains(t, "no red flag"):
		return levelClean
	case strings.Contains(t, "red flag"):
		return levelRedFlags
	case strings.Contains(t, "caution"), strings.Contains(t, "warning"):
		return levelCaution
	case strings.Contains(t, labelInsufficient):
		return levelInsufficient
	}
	return levelUnknown
}

// messageURLs collects every URL in a message: plain text, text links,
// link previews and inline-keyboard buttons.
func messageURLs(msg *tg.Message) []string {
	urls := []string{msg.Message}
	for _, e := range msg.Entities {
		if u, ok := e.(*tg.MessageEntityTextURL); ok {
			urls = append(urls, u.URL)
		}
	}
	if wp, ok := msg.Media.(*tg.MessageMediaWebPage); ok {
		if page, ok := wp.Webpage.(*tg.WebPage); ok {
			urls = append(urls, page.URL)
		}
	}
	return append(urls, buttonURLs(msg)...)
}

// buttonURLs returns the links behind a message's inline buttons
// ("Buy", "Chart", "DexScreener", mini-app buttons, …).
func buttonURLs(msg *tg.Message) []string {
	var urls []string
	kb, ok := msg.ReplyMarkup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil
	}
	for _, row := range kb.Rows {
		for _, b := range row.Buttons {
			switch x := b.(type) {
			case *tg.KeyboardButtonURL:
				urls = append(urls, x.URL)
			case *tg.KeyboardButtonURLAuth:
				urls = append(urls, x.URL)
			case *tg.KeyboardButtonWebView:
				urls = append(urls, x.URL)
			case *tg.KeyboardButtonSimpleWebView:
				urls = append(urls, x.URL)
			case *tg.KeyboardButtonSwitchInline:
				urls = append(urls, x.Query) // e.g. "0x… " share buttons
			case *tg.KeyboardButtonCopy:
				urls = append(urls, x.CopyText) // "copy CA" buttons
			}
		}
	}
	return urls
}

func perceptorID(msgs ...*tg.Message) string {
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, u := range messageURLs(m) {
			if sm := perceptorIDRe.FindStringSubmatch(u); sm != nil {
				return strings.ToLower(sm[1])
			}
		}
	}
	return ""
}

func firstMatch(body string, res ...*regexp.Regexp) string {
	for _, re := range res {
		if m := re.FindStringSubmatch(body); m != nil {
			return strings.TrimSpace(html.UnescapeString(m[1]))
		}
	}
	return ""
}

// parsePerceptorPage extracts the verdict from a /r/<id> page's HTML.
func parsePerceptorPage(body string) verdict {
	title := firstMatch(body, ogTitleRe, ogTitleRe2, titleRe)
	title = strings.TrimSpace(strings.TrimSuffix(title, "| 0xPerceptor"))
	v := verdict{Source: "perceptor.info", Level: levelUnknown}
	if i := strings.Index(title, ": "); i > 0 {
		v.Ticker, v.Label = title[:i], strings.TrimSpace(title[i+2:])
	} else {
		v.Label = title
	}
	v.Level = levelFromLabel(v.Label)

	desc := firstMatch(body, ogDescRe, ogDescRe2)
	if v.Level == levelUnknown && levelFromLabel(desc) == levelInsufficient {
		// "$SNIFF on-chain check: not enough data yet. Full breakdown via @0xPerceptor"
		v.Level = levelInsufficient
	}
	if v.Level == levelInsufficient {
		v.Label = labelInsufficient
		if v.Ticker == "" {
			if m := insufficientTickerRe.FindStringSubmatch(desc); m != nil {
				v.Ticker = m[1]
			}
		}
	}
	desc = strings.TrimSpace(strings.TrimSuffix(desc, "Full breakdown via @0xPerceptor"))
	// "$NH on-chain check: red flags. Top 10 hold 40%; Liquidity PULLED."
	if i := strings.Index(desc, ". "); i > 0 && strings.Contains(strings.ToLower(desc[:i]), "on-chain check") {
		desc = strings.TrimSpace(desc[i+2:])
	} else if strings.Contains(strings.ToLower(desc), "on-chain check") {
		desc = ""
	}
	v.Summary = strings.TrimSuffix(desc, ".")
	return v
}

// fetchPerceptorVerdict reads the public report page. Retries a few times in
// case the page isn't published the instant the bot replies.
func fetchPerceptorVerdict(ctx context.Context, id string) (verdict, error) {
	url := "https://www.perceptor.info/r/" + id
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return verdict{}, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (scoutanalytics)")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
			continue
		}
		v := parsePerceptorPage(string(body))
		v.URL = url
		if v.Level != levelUnknown {
			return v, nil
		}
		lastErr = fmt.Errorf("no verdict in page title %q", v.Label)
	}
	return verdict{}, lastErr
}

// textVerdict is the fallback when there is no report link: read the verdict
// from the Telegram message itself.
func textVerdict(text string, markers, safe []string) verdict {
	v := verdict{Source: "telegram_text"}
	text = zeroRedFlagsAsNone(text) // "0 red flags" is "no red flags" for the markers too
	lvl := levelFromLabel(text)
	if lvl == levelClean {
		// Explicit "no red flags" — still make sure there is no caution/warning marker elsewhere.
		if flagged, m := classify(text, markers, safe); flagged {
			v.Level, v.Label = levelFromLabel(m), m
			if v.Level == levelUnknown {
				v.Level = markerLevel(m)
			}
			return v
		}
		v.Level, v.Label = levelClean, "No red flags found"
		return v
	}
	if lvl == levelInsufficient {
		// Perceptor's own "not enough data yet" (no red flag, caution or
		// warning phrase in the text): it wins over the generic markers.
		v.Level, v.Label = levelInsufficient, labelInsufficient
		if m := insufficientTickerRe.FindStringSubmatch(text); m != nil {
			v.Ticker = m[1]
		}
		return v
	}
	if flagged, m := classify(text, markers, safe); flagged {
		v.Level, v.Label = levelFromLabel(m), m
		if v.Level == levelUnknown {
			v.Level = markerLevel(m)
		}
		return v
	}
	// No link and no verdict phrase: probably an error / "not found" reply. Don't deliver.
	v.Level = levelUnknown
	return v
}

// markerLevel classifies a fallback text marker: soft warnings are Caution,
// everything else (honeypot, scam, rug pull, 🚩, …) is treated as Red flags.
func markerLevel(m string) string {
	switch m {
	case "caution", "warning", "⚠️", "⚠":
		return levelCaution
	}
	return levelRedFlags
}
