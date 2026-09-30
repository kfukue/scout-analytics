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

// levelFromLabel maps a verdict phrase to a level. Order matters:
// "no red flags" must be checked before "red flag".
func levelFromLabel(s string) string {
	t := strings.ToLower(s)
	switch {
	case strings.Contains(t, "no red flag"):
		return levelClean
	case strings.Contains(t, "red flag"):
		return levelRedFlags
	case strings.Contains(t, "caution"), strings.Contains(t, "warning"):
		return levelCaution
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
	if kb, ok := msg.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
		for _, row := range kb.Rows {
			for _, b := range row.Buttons {
				if u, ok := b.(*tg.KeyboardButtonURL); ok {
					urls = append(urls, u.URL)
				}
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
	if lvl := levelFromLabel(text); lvl == levelClean {
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
