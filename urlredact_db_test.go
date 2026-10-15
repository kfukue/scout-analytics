package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A node error stored on the call (scout_call_tracking.error) and logged by
// the on-chain tracker carries no node URL: a transport error (redacted by
// the RPC client) and a JSON-RPC error whose message names the node URL
// (redacted where the tracker stores it).
func TestTrackerOnchainStoresRedactedNodeError(t *testing.T) {
	rpcErrNode := func(t *testing.T) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			msg := "service unavailable, status at http://" + r.Host + "/v2/" + fakeKey + "/status"
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":%q}}`, msg)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	for i, c := range []struct {
		name string
		node func(t *testing.T) *httptest.Server
		want string // in the stored error and the log
	}{
		{"transport error", func(t *testing.T) *httptest.Server { return droppingNode(t, nil) }, `Post "<url>"`},
		{"node message names its URL", rpcErrNode, "status at <url>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			fastRetries(t)
			noHeadCache(t)
			srv := c.node(t)
			logs := captureLog(t)
			s := usdScanner(t, st, srv.URL+"/v2/"+fakeKey+"?apikey="+fakeKey, "eth="+ltFeed)
			token := fmt.Sprintf("0x%040x", 0x4545+i)
			s.onChannelPost(postAt(1, time.Now().Add(-2*time.Hour), token))
			id := *(<-s.queue).CallID

			s.trackDue(ctx, 50)
			tr, err := st.GetTracking(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Status != TrackError || tr.Error == nil {
				t.Fatalf("got status %s error %s, want an error stored; log:\n%s", tr.Status, strOrNil(tr.Error), logs.String())
			}
			if strings.Contains(*tr.Error, fakeKey) || !strings.Contains(*tr.Error, c.want) {
				t.Fatalf("stored error: got %q, want %q and no key", *tr.Error, c.want)
			}
			if strings.Contains(logs.String(), fakeKey) {
				t.Fatalf("the log holds the key:\n%s", logs.String())
			}
			if !strings.Contains(logs.String(), c.want) {
				t.Fatalf("the log does not show the redacted error %q:\n%s", c.want, logs.String())
			}
		})
	}
}

// The price-API tracker (non-on-chain) stores and logs its errors without
// URLs too.
func TestTrackerGeckoStoresRedactedError(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	srv := droppingNode(t, nil)
	t.Setenv("SCOUT_PRICE_SOURCE", "gecko")
	t.Setenv("SCOUT_PRICE_API_BASE", srv.URL+"/key/"+fakeKey)
	t.Setenv("SCOUT_PRICE_RPM", "6000")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	logs := captureLog(t)
	token := "0x4646464646464646464646464646464646464646"
	s.onChannelPost(postAt(1, time.Now().Add(-2*time.Hour), token))
	id := *(<-s.queue).CallID

	s.trackDue(ctx, 50)
	tr, err := st.GetTracking(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != TrackError || tr.Error == nil {
		t.Fatalf("got status %s error %s, want an error stored; log:\n%s", tr.Status, strOrNil(tr.Error), logs.String())
	}
	if strings.Contains(*tr.Error, fakeKey) || !strings.Contains(*tr.Error, `"<url>"`) {
		t.Fatalf("stored error: got %q, want \"<url>\" and no key", *tr.Error)
	}
	if line := trackingLogLine(logs.String(), id); line == "" || strings.Contains(line, fakeKey) {
		t.Fatalf("tracking log line for call %d: got %q, want one without the key; log:\n%s", id, line, logs.String())
	}
}

// trackingLogLine returns the tracker's "tracking call <id> (...)" failure
// line from log ("" when there is none).
func trackingLogLine(log string, id int) string {
	prefix := "tracking call " + strconv.Itoa(id) + " ("
	for _, l := range strings.Split(log, "\n") {
		if i := strings.Index(l, prefix); i >= 0 {
			return l[i:]
		}
	}
	return ""
}
