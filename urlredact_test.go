package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKey stands in for a node API key in test URLs.
const fakeKey = "FAKEKEY0123456789"

func TestRedactURLs(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    string
		known []string
		want  string
	}{
		{"net/http dial error, key in the path",
			`Post "https://rpc.example.com/v2/` + fakeKey + `": dial tcp 10.0.0.1:443: connect: connection refused`, nil,
			`Post "<url>": dial tcp 10.0.0.1:443: connect: connection refused`},
		{"client time-out, key in the query",
			`Post "https://rpc.example.com/?apikey=` + fakeKey + `&chain=1": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, nil,
			`Post "<url>": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`},
		{"wrapped by the caller",
			`eth_getLogs blocks 100-199: Post "https://rpc.example.com/v2/` + fakeKey + `": EOF`, nil,
			`eth_getLogs blocks 100-199: Post "<url>": EOF`},
		{"user info",
			`Post "https://user:` + fakeKey + `@rpc.example.com:8545": net/http: TLS handshake timeout`, nil,
			`Post "<url>": net/http: TLS handshake timeout`},
		{"go-ethereum websocket dial, punctuation after the URL kept",
			`websocket: dial wss://rpc.example.com/ws/` + fakeKey + `: bad handshake (HTTP 401)`, nil,
			`websocket: dial <url>: bad handshake (HTTP 401)`},
		{"unquoted, in parentheses",
			`node down (https://rpc.example.com/` + fakeKey + `), retrying`, nil,
			`node down (<url>), retrying`},
		{"two URLs",
			`mainnet https://eth.example/v3/` + fakeKey + ` and rh http://127.0.0.1:8540/` + fakeKey + ` failed`, nil,
			`mainnet <url> and rh <url> failed`},
		{"configured URL replaced verbatim, even with a space in it",
			`Post "https://rpc.example.com/v2/FAKE ` + fakeKey + `": EOF`, []string{"https://rpc.example.com/v2/FAKE " + fakeKey},
			`Post "<url>": EOF`},
		{"proxy page echoing only the path of a known URL",
			`<pre>Cannot POST /v2/` + fakeKey + `</pre>`, []string{"https://rpc.example.com/v2/" + fakeKey + "?apikey=" + fakeKey},
			`<pre>Cannot POST <url></pre>`},
		{"proxy page echoing the path and query of a known URL",
			`Cannot POST /v2/` + fakeKey + `?apikey=` + fakeKey + ` (404)`, []string{"https://rpc.example.com/v2/" + fakeKey + "?apikey=" + fakeKey},
			`Cannot POST <url> (404)`},
		{"query of a known URL echoed alone",
			`unknown parameters: apikey=` + fakeKey, []string{"https://rpc.example.com/?apikey=" + fakeKey},
			`unknown parameters: <url>`},
		{"short path of a known URL not blanked in other text",
			`POST /rpc failed: /rpc/health says /v2/abc is down`, []string{"https://rpc.example.com/rpc", "https://other.example.com/v2/abc"},
			`POST /rpc failed: /rpc/health says /v2/abc is down`},
		{"root path of a known URL not blanked",
			`path / not found`, []string{"https://rpc.example.com/"},
			`path / not found`},
		{"empty known URL ignored",
			`rpc error -32000: missing trie node 0xabc (path ) state 0x12 is not available`, []string{""},
			`rpc error -32000: missing trie node 0xabc (path ) state 0x12 is not available`},
		{"no URL: unchanged",
			`no trades for 0x4444 before block 123: context canceled`, nil,
			`no trades for 0x4444 before block 123: context canceled`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := redactURLs(c.in, c.known...)
			if got != c.want {
				t.Fatalf("redactURLs(%q, %q):\n got %q\nwant %q", c.in, c.known, got, c.want)
			}
			if strings.Contains(got, fakeKey) {
				t.Fatalf("redactURLs(%q) = %q still holds the key", c.in, got)
			}
		})
	}
}

func TestKnownURLParts(t *testing.T) {
	for _, c := range []struct {
		name  string
		known []string
		want  []string
	}{
		{"path and query, longest first",
			[]string{"https://rpc.example.com/v2/" + fakeKey + "?apikey=" + fakeKey},
			[]string{
				"https://rpc.example.com/v2/" + fakeKey + "?apikey=" + fakeKey,
				"/v2/" + fakeKey + "?apikey=" + fakeKey,
				"apikey=" + fakeKey,
				"/v2/" + fakeKey,
			}},
		{"short path left out, query kept",
			[]string{"https://rpc.example.com/rpc?k=" + fakeKey},
			[]string{"https://rpc.example.com/rpc?k=" + fakeKey, "k=" + fakeKey}},
		{"no path or query",
			[]string{"http://127.0.0.1:8545"},
			[]string{"http://127.0.0.1:8545"}},
		{"empty and duplicate URLs dropped",
			[]string{"", "https://a.example/v2/" + fakeKey, "https://b.example/v2/" + fakeKey},
			[]string{"https://a.example/v2/" + fakeKey, "https://b.example/v2/" + fakeKey, "/v2/" + fakeKey}},
		{"unparsable URL kept whole",
			[]string{"http://bad host/%zz"},
			[]string{"http://bad host/%zz"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := knownURLParts(c.known); !slices.Equal(got, c.want) {
				t.Errorf("knownURLParts(%q):\n got %q\nwant %q", c.known, got, c.want)
			}
		})
	}
}

func TestRedactErrKeepsClassification(t *testing.T) {
	if redactErr(nil) != nil {
		t.Fatal("redactErr(nil) is not nil")
	}
	re := &rpcError{Code: -32000, Message: "missing trie node 0xabc"}
	if got := redactErr(re, "https://rpc.example.com/v2/"+fakeKey); got != error(re) {
		t.Fatalf("an error without a URL: got %v (%T), want the same *rpcError back", got, got)
	}

	nodeURL := "https://rpc.example.com/v2/" + fakeKey
	orig := fmt.Errorf("eth_getLogs: %w", &url.Error{Op: "Post", URL: nodeURL, Err: context.DeadlineExceeded})
	got := redactErr(orig, nodeURL)
	if want := `eth_getLogs: Post "<url>": context deadline exceeded`; got.Error() != want {
		t.Fatalf("text: got %q, want %q", got.Error(), want)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Error("errors.Is(DeadlineExceeded): got false, want true")
	}
	var ue *url.Error
	if !errors.As(got, &ue) {
		t.Error("errors.As(*url.Error): got false, want true")
	}
	if isTimeout(got) != isTimeout(orig) || !isTimeout(got) {
		t.Errorf("isTimeout: got %v, original %v, want both true", isTimeout(got), isTimeout(orig))
	}
	if g, w := classifyLogsErr(got), classifyLogsErr(orig); g != w {
		t.Errorf("classifyLogsErr: got %v, want %v (as the original)", g, w)
	}
}

func TestURLOrigin(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://rpc.example.com/v2/" + fakeKey, "https://rpc.example.com"},
		{"https://user:" + fakeKey + "@rpc.example.com:8545/?apikey=" + fakeKey + "#x", "https://rpc.example.com:8545"},
		{"http://localhost:8540", "http://localhost:8540"},
		{"user:" + fakeKey + "@rpc.example.com", "<url>"},
		{"not a url " + fakeKey, "<url>"},
		{"", "<url>"},
	} {
		if got := urlOrigin(c.in); got != c.want {
			t.Errorf("urlOrigin(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

// droppingNode is a node whose every request has its connection closed
// before an answer: the client gets a *url.Error naming the URL ("Post
// \"<url>\": EOF"). onRequest, when set, runs first.
func droppingNode(t *testing.T, onRequest func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot be hijacked")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A transport error leaves the RPC client without the node URL in its text,
// both in the returned error and in the heartbeat's "last error".
func TestRPCClientTransportErrorRedacted(t *testing.T) {
	fastRetries(t)
	var client atomic.Pointer[rpcClient]
	var mu sync.Mutex
	var waiting []string
	srv := droppingNode(t, func() {
		if c := client.Load(); c != nil {
			mu.Lock()
			waiting = append(waiting, c.waitingOn())
			mu.Unlock()
		}
	})
	nodeURL := srv.URL + "/v2/" + fakeKey + "?apikey=" + fakeKey
	c := newRPCClient(onchainConfig{RPCURL: nodeURL})
	client.Store(c)

	err := c.call(context.Background(), nil, "eth_blockNumber")
	if err == nil {
		t.Fatal("got no error from a node that drops every connection")
	}
	if strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), `Post "<url>"`) {
		t.Fatalf("error text: got %q, want Post \"<url>\" and no key", err.Error())
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("errors.As(*url.Error) on %v: got false, want true", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waiting) != 4 {
		t.Fatalf("got %d request(s), want the 4 attempts", len(waiting))
	}
	for i, w := range waiting[1:] {
		if strings.Contains(w, fakeKey) || !strings.Contains(w, "last error: Post \"<url>\"") {
			t.Errorf("attempt %d waitingOn: got %q, want the last error without the key", i+2, w)
		}
	}
}

// A client time-out is still classified as one after redaction.
func TestRPCClientTimeoutRedactedStillTimeout(t *testing.T) {
	fastRetries(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c := newRPCClient(onchainConfig{RPCURL: srv.URL + "/v2/" + fakeKey})
	c.http.Timeout = 20 * time.Millisecond

	err := c.call(context.Background(), nil, "eth_getLogs")
	if err == nil {
		t.Fatal("got no error from a node that never answers")
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("error text %q still holds the key", err.Error())
	}
	if !isTimeout(err) {
		t.Errorf("isTimeout(%v): got false, want true", err)
	}
	if got := classifyLogsErr(err); got != logsClientTimeout {
		t.Errorf("classifyLogsErr(%v): got %v, want logsClientTimeout", err, got)
	}
}

// The tracker's start-up line names the nodes by scheme and host only.
func TestOnchainDescribeHidesNodePath(t *testing.T) {
	t.Setenv("SCOUT_RPC_URL", "http://127.0.0.1:1/v2/"+fakeKey)
	t.Setenv("SCOUT_MAINNET_RPC_URL", "https://eth.example.com/v3/"+fakeKey+"?k="+fakeKey)
	oc, err := loadOnchainConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := newOnchainSource(oc).describe()
	if strings.Contains(d, fakeKey) {
		t.Fatalf("describe() holds the key: %q", d)
	}
	for _, want := range []string{"on-chain pools via http://127.0.0.1:1 ", "from Ethereum mainnet via https://eth.example.com"} {
		if !strings.Contains(d, want) {
			t.Errorf("describe(): got %q, want it to contain %q", d, want)
		}
	}
}

// A node (or a proxy in front of it) that answers with a page instead of
// JSON: the error echoes the start of the page without the node URL in it.
func TestRPCClientBadResponseRedacted(t *testing.T) {
	var nodeURL string
	for _, c := range []struct {
		name string
		page func() string
	}{
		{"URL in the text", func() string {
			return "<html><body>Cannot POST " + nodeURL + " (no route)</body></html>"
		}},
		{"URL in an attribute", func() string {
			return `<html><a href="` + nodeURL + `">retry</a></html>`
		}},
		{"URL across the 120-byte cut", func() string {
			return strings.Repeat("x", 80) + " " + nodeURL + " tail"
		}},
		{"only the path echoed", func() string {
			return "<pre>Cannot POST " + nodeRequestURI(t, nodeURL, false) + "</pre>"
		}},
		{"path and query echoed", func() string {
			return "<pre>Cannot POST " + nodeRequestURI(t, nodeURL, true) + "</pre>"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, c.page())
			}))
			t.Cleanup(srv.Close)
			nodeURL = srv.URL + "/v2/" + fakeKey + "?apikey=" + fakeKey
			page := c.page()
			if !strings.Contains(fmt.Sprintf("%.120s", page), fakeKey[:4]) {
				t.Fatalf("test page %q: the key does not start within the first 120 bytes", page)
			}

			err := newRPCClient(onchainConfig{RPCURL: nodeURL}).call(context.Background(), nil, "eth_blockNumber")
			if err == nil {
				t.Fatalf("page %q: got no error, want a bad response", page)
			}
			got := err.Error()
			if strings.Contains(got, fakeKey[:4]) || strings.Contains(got, srv.URL) {
				t.Fatalf("page %q: error %q still holds the node URL or part of the key", page, got)
			}
			if !strings.HasPrefix(got, "eth_blockNumber: bad response: ") || !strings.Contains(got, urlMask) {
				t.Errorf("page %q: got %q, want \"eth_blockNumber: bad response: ...\" with %s", page, got, urlMask)
			}
		})
	}
}

// nodeRequestURI returns the path of raw, with its query when withQuery is
// set, as a proxy page in front of the node would echo it.
func nodeRequestURI(t *testing.T, raw string, withQuery bool) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	if withQuery {
		return u.RequestURI()
	}
	return u.EscapedPath()
}

// The GeckoTerminal start-up line names the price API by scheme and host only.
func TestGeckoTrackingLineHidesAPIPath(t *testing.T) {
	pc := &priceConfig{
		BaseURL:  "https://pro-api.example.com/api/v3/onchain/" + fakeKey + "?x_cg_pro_api_key=" + fakeKey,
		Network:  "robinhood",
		RPM:      30,
		Horizons: []horizon{{Name: "1h", Dur: time.Hour}, {Name: "1d", Dur: 24 * time.Hour}},
	}
	got := geckoTrackingLine(pc)
	if strings.Contains(got, fakeKey) || strings.Contains(got, "/api/v3") {
		t.Fatalf("geckoTrackingLine: got %q, want no path or key", got)
	}
	if want := `performance tracking on: ` + horizonNames(pc.Horizons) + ` via https://pro-api.example.com (network "robinhood", 30 req/min)`; got != want {
		t.Errorf("geckoTrackingLine:\n got %q\nwant %q", got, want)
	}
}
