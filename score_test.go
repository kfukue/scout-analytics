package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The scored row gets the quote symbol live pre-call found only when the view
// has none yet; a symbol the view already has, or an unknown one, leaves the
// row as it is.
func TestFillQuoteAsset(t *testing.T) {
	cases := []struct {
		name    string
		row     string
		sym     string
		want    any  // quote_asset in the result (nil = JSON null or absent)
		same    bool // the row comes back byte for byte
		wantErr bool
	}{
		{name: "null filled", row: `{"call_id":7,"quote_asset":null,"holders":1150}`, sym: "WETH", want: "WETH"},
		{name: "empty filled", row: `{"call_id":7,"quote_asset":"","holders":1150}`, sym: "WETH", want: "WETH"},
		{name: "absent filled", row: `{"call_id":7,"holders":1150}`, sym: "USDG", want: "USDG"},
		{name: "symbol kept as read", row: `{"quote_asset":null}`, sym: "wETH<&>", want: "wETH<&>"},
		{name: "view value kept", row: `{"call_id":7,"quote_asset":"USDG"}`, sym: "WETH", want: "USDG", same: true},
		{name: "unknown symbol leaves null", row: `{"call_id":7,"quote_asset":null}`, sym: "", want: nil, same: true},
		{name: "unknown symbol leaves bad row", row: `not json`, sym: "", same: true},
		{name: "bad row", row: `not json`, sym: "WETH", wantErr: true},
		{name: "not an object", row: `null`, sym: "WETH", wantErr: true},
		{name: "quote_asset not a string", row: `{"quote_asset":5}`, sym: "WETH", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := fillQuoteAsset([]byte(c.row), c.sym)
			if c.wantErr {
				if err == nil {
					t.Fatalf("fillQuoteAsset(%s, %q) = %s, want an error", c.row, c.sym, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("fillQuoteAsset(%s, %q): %v", c.row, c.sym, err)
			}
			if c.same {
				if !bytes.Equal(out, []byte(c.row)) {
					t.Fatalf("fillQuoteAsset(%s, %q) = %s, want the row unchanged", c.row, c.sym, out)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("fillQuoteAsset(%s, %q) = %s: not JSON: %v", c.row, c.sym, out, err)
			}
			if got["quote_asset"] != c.want {
				t.Fatalf("fillQuoteAsset(%s, %q): quote_asset = %v, want %v", c.row, c.sym, got["quote_asset"], c.want)
			}
			var in map[string]any
			if err := json.Unmarshal([]byte(c.row), &in); err != nil {
				t.Fatal(err)
			}
			for k, v := range in { // every other column passes through
				if k != "quote_asset" && got[k] != v {
					t.Fatalf("fillQuoteAsset(%s, %q): %s = %v, want %v", c.row, c.sym, k, got[k], v)
				}
			}
		})
	}
}

// The start-up line says whether scoring is on and where it goes, and never
// prints user info, query or fragment from SCOUT_MODEL_URL or a URL that does
// not parse. With SCOUT_DB=off scoreCall never scores, so the line says off.
func TestModelScoringStartupLine(t *testing.T) {
	const onTail = " (shared with the live pre-call stats); scores are stored and shown in the delivery header, never filter deliveries"
	cases := []struct {
		name    string
		url     string
		timeout time.Duration
		dbOff   bool     // SCOUT_DB=off: the scanner has no database
		want    string   // exact line ("" = only the contains checks)
		has     []string // must appear
		hasNot  []string // must not appear
	}{
		{name: "off", url: "", timeout: 10 * time.Second,
			want: "model scoring: off (SCOUT_MODEL_URL not set)"},
		{name: "on", url: "http://127.0.0.1:8601", timeout: 10 * time.Second,
			want: "model scoring: on — live calls are scored by http://127.0.0.1:8601/score within 10s" + onTail},
		{name: "on with timeout", url: "http://scorer.local:8601", timeout: 2500 * time.Millisecond,
			want: "model scoring: on — live calls are scored by http://scorer.local:8601/score within 2.5s" + onTail},
		{name: "db off", url: "http://127.0.0.1:8601", timeout: 10 * time.Second, dbOff: true,
			want: "model scoring: off (SCOUT_MODEL_URL is set but SCOUT_DB=off; scoring needs the database)"},
		{name: "query and fragment left out", url: "http://127.0.0.1:8601/?token=x#frag", timeout: 10 * time.Second,
			want:   "model scoring: on — live calls are scored by http://127.0.0.1:8601//score within 10s" + onTail,
			hasNot: []string{"token", "=x", "?", "#", "frag"}},
		{name: "user info left out", url: "http://alice:s3cret@127.0.0.1:8601", timeout: 10 * time.Second,
			want:   "model scoring: on — live calls are scored by http://127.0.0.1:8601/score within 10s" + onTail,
			hasNot: []string{"alice", "s3cret", "@"}},
		{name: "user without password left out", url: "https://bob@scorer.local", timeout: 10 * time.Second,
			has: []string{"https://scorer.local/score"}, hasNot: []string{"bob", "@"}},
		{name: "invalid URL not echoed", url: "alice:s3cret@127.0.0.1:8601", timeout: 10 * time.Second,
			has: []string{"model scoring: on", "(SCOUT_MODEL_URL is not a valid URL)/score"}, hasNot: []string{"alice", "s3cret", "127.0.0.1"}},
		{name: "no scheme not echoed", url: "127.0.0.1:8601", timeout: 10 * time.Second,
			has: []string{"(SCOUT_MODEL_URL is not a valid URL)/score"}, hasNot: []string{"127.0.0.1"}},
		{name: "parse error not echoed", url: "http://alice:s3cret@127.0.0.1:86x1", timeout: 10 * time.Second,
			has: []string{"(SCOUT_MODEL_URL is not a valid URL)/score"}, hasNot: []string{"alice", "s3cret"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := modelScoringStartupLine(&config{ModelURL: c.url, ModelTimeout: c.timeout}, !c.dbOff)
			if c.want != "" && got != c.want {
				t.Errorf("modelScoringStartupLine(%q, %s, dbOn=%t) = %q, want %q", c.url, c.timeout, !c.dbOff, got, c.want)
			}
			for _, s := range c.has {
				if !strings.Contains(got, s) {
					t.Errorf("modelScoringStartupLine(%q, %s, dbOn=%t) = %q, want it to contain %q", c.url, c.timeout, !c.dbOff, got, s)
				}
			}
			for _, s := range c.hasNot {
				if strings.Contains(got, s) {
					t.Errorf("modelScoringStartupLine(%q, %s, dbOn=%t) = %q, want no %q in it", c.url, c.timeout, !c.dbOff, got, s)
				}
			}
		})
	}
}
