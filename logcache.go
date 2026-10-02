package main

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// In-memory cache of finished eth_getLogs chunks. Repeat calls of a token scan
// the same pool over (mostly) the same blocks; with chunk-aligned ranges the
// second scan costs no node requests. Bounded by the number of logs held.
// ---------------------------------------------------------------------------

type logCache struct {
	mu    sync.Mutex
	limit int // logs (an empty result counts as 1)
	size  int
	order *list.List // front = most recently used
	items map[string]*list.Element
	hits  atomic.Int64
}

type logCacheEntry struct {
	key  string
	logs []rpcLog
}

func newLogCache(limit int) *logCache {
	return &logCache{limit: limit, order: list.New(), items: map[string]*list.Element{}}
}

func logCacheKey(address string, topics []any, from, to uint64) string {
	t, _ := json.Marshal(topics)
	return fmt.Sprintf("%s|%s|%d|%d", address, t, from, to)
}

func cost(logs []rpcLog) int { return len(logs) + 1 }

func (c *logCache) get(key string) ([]rpcLog, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	c.hits.Add(1)
	return e.Value.(*logCacheEntry).logs, true
}

func (c *logCache) put(key string, logs []rpcLog) {
	if cost(logs) > c.limit/4 {
		return // one very busy chunk must not push everything else out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.order.PushFront(&logCacheEntry{key: key, logs: logs})
	c.size += cost(logs)
	for c.size > c.limit {
		last := c.order.Back()
		ent := last.Value.(*logCacheEntry)
		c.order.Remove(last)
		delete(c.items, ent.key)
		c.size -= cost(ent.logs)
	}
}

// Per-call request counter: with several calls tracked at once the client's
// total says nothing about one call.
type reqCounterKey struct{}

func withReqCounter(ctx context.Context) (context.Context, *atomic.Int64) {
	n := &atomic.Int64{}
	return context.WithValue(ctx, reqCounterKey{}, n), n
}

func reqCounterFrom(ctx context.Context) *atomic.Int64 {
	n, _ := ctx.Value(reqCounterKey{}).(*atomic.Int64)
	return n
}
