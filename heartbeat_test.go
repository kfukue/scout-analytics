package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A slow node must show up in the log as "still working … waiting on …", not as silence.
func TestHeartbeatNamesTheSlowRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
	}))
	defer srv.Close()

	var buf syncBuf
	log.SetOutput(&buf)
	defer log.SetOutput(log.Default().Writer())
	old := heartbeatEvery
	heartbeatEvery = 20 * time.Millisecond
	defer func() { heartbeatEvery = old }()

	rpc := newRPCClient(onchainConfig{RPCURL: srv.URL, RPS: 100000})
	s := &scanner{onchain: &onchainSource{rpc: rpc}}
	hctx, reqs := withReqCounter(context.Background())
	stop := s.heartbeat(hctx, "call 7 [1/1]", time.Now(), reqs)
	if _, err := rpc.getLogs(hctx, "0x00000000000000000000000000000000000000aa", nil, 100, 299); err != nil {
		t.Fatal(err)
	}
	stop()
	out := buf.String()
	for _, want := range []string{"call 7 [1/1]: still working", "1 RPC requests so far", "Robinhood node busy with: eth_getLogs blocks 100-299 (200 blocks)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if rpc.waitingOn() != "" {
		t.Fatalf("in-flight not cleared: %q", rpc.waitingOn())
	}
	var none *rpcClient
	if none.waitingOn() != "" {
		t.Fatal("nil client")
	}
}
