package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One scan fetches several block ranges from the node at the same time and
// still delivers the logs in block order.
func TestGetLogsChunkedFetchesInParallel(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	var mu sync.Mutex
	var ranges []uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var q struct{ FromBlock, ToBlock string }
		json.Unmarshal(req.Params[0], &q)
		from, _ := strconv.ParseUint(strings.TrimPrefix(q.FromBlock, "0x"), 16, 64)
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		// later ranges answer sooner: order must come from the client, not from arrival
		time.Sleep(time.Duration(60-from/100) * time.Millisecond)
		inFlight.Add(-1)
		mu.Lock()
		ranges = append(ranges, from)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": []map[string]any{
			{"address": "0xaa", "topics": []string{"0x01"}, "data": "0x", "blockNumber": hexU64(from + 7), "transactionHash": "0x1", "logIndex": "0x0"},
			{"address": "0xaa", "topics": []string{"0x01"}, "data": "0x", "blockNumber": hexU64(from + 3), "transactionHash": "0x2", "logIndex": "0x0"},
		}})
	}))
	defer srv.Close()

	c := newRPCClient(onchainConfig{RPCURL: srv.URL, RPS: 100000, LogChunk: 100, Parallel: 4})
	var got []uint64
	start := time.Now()
	if err := c.getLogsChunked(context.Background(), "0xaa", []any{"0x01"}, 1000, 1799, func(l rpcLog) { got = append(got, l.block()) }); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if len(got) != 16 {
		t.Fatalf("%d logs", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("out of order: %v", got)
		}
	}
	if m := maxInFlight.Load(); m < 2 || m > 4 {
		t.Fatalf("max requests in flight %d, want 2..4", m)
	}
	// 8 requests of ~50 ms: ~400 ms one at a time, ~100 ms four at a time
	if took > 300*time.Millisecond {
		t.Fatalf("took %s: not parallel", took)
	}

	// Parallel = 1 keeps the old one-at-a-time behaviour.
	maxInFlight.Store(0)
	c1 := newRPCClient(onchainConfig{RPCURL: srv.URL, RPS: 100000, LogChunk: 100, Parallel: 1})
	if err := c1.getLogsChunked(context.Background(), "0xaa", []any{"0x01"}, 1000, 1299, func(rpcLog) {}); err != nil {
		t.Fatal(err)
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Fatalf("sequential client had %d requests in flight", m)
	}
}
