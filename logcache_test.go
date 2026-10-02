package main

import (
	"context"
	"testing"
	"time"
)

// Chunks end on multiples of the chunk size and finished ones are cached, so a
// second scan of the same pool only asks the node for the partial ends.
func TestGetLogsChunkedAlignsAndCaches(t *testing.T) {
	f := newFakeChain(t, 24*time.Hour)
	pool := "0x00000000000000000000000000000000000000c3"
	for _, b := range []uint64{1600, 2500, 2501, 4999, 5400} {
		f.swapV3(pool, b, hexU64(b), sqrtX96(1))
	}
	t.Setenv("SCOUT_RPC_LOG_CHUNK", "1000")
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	if _, err := o.rpc.blockNumber(ctx); err != nil { // the cache needs to know where the tip is
		t.Fatal(err)
	}
	scan := func(from, to uint64) (blocks []uint64, requests int) {
		before := f.count["eth_getLogs"]
		if err := o.rpc.getLogsChunked(ctx, pool, []any{topicSwapV3}, from, to, func(l rpcLog) { blocks = append(blocks, l.block()) }); err != nil {
			t.Fatal(err)
		}
		return blocks, f.count["eth_getLogs"] - before
	}
	want := []uint64{1600, 2500, 2501, 4999, 5400}
	same := func(got []uint64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	// 1500-1999, 2000-2999, 3000-3999, 4000-4999, 5000-5500
	if got, n := scan(1500, 5500); !same(got) || n != 5 {
		t.Fatalf("first scan: %v in %d requests", got, n)
	}
	// the three full chunks come from memory
	if got, n := scan(1500, 5500); !same(got) || n != 2 {
		t.Fatalf("second scan: %v in %d requests", got, n)
	}
	// another call of the same token, posted a little later: only its own ends are fetched
	if got, n := scan(1700, 5450); len(got) != 4 || n != 2 {
		t.Fatalf("overlapping scan: %v in %d requests", got, n)
	}
	// chunks near the chain's tip are never cached
	tip := f.latest
	if _, n := scan(tip-2000, tip); n == 0 {
		t.Fatal("tip scan made no requests")
	}
	if _, n := scan(tip-2000, tip); n < 2 {
		t.Fatalf("tip chunks were cached (%d requests)", n)
	}

	c := newLogCache(8)
	c.put("a", make([]rpcLog, 1)) // cost 2
	c.put("b", make([]rpcLog, 1))
	c.put("c", make([]rpcLog, 1))
	c.get("a")
	c.put("d", make([]rpcLog, 1)) // total 8
	c.put("e", make([]rpcLog, 1)) // evicts b, the least recently used
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a was used recently and should stay")
	}
	c.put("big", make([]rpcLog, 5)) // over a quarter of the cache: not kept
	if _, ok := c.get("big"); ok || c.size > c.limit {
		t.Fatalf("size %d", c.size)
	}
}
