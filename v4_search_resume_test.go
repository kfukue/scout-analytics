package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A v4 pool search (v4PoolsOf) that stops on an error keeps how far each of
// its two Initialize searches (token as currency0, as currency1) got and the
// pools found there: the next try asks for no range of either search before
// the one that failed, reads the last 1,000 blocks again, and ends with the
// same pools, in the same order, and the same progress as one uninterrupted
// search.

// v4ResumeChunk: the Initialize ranges of the tests (SCOUT_RPC_HISTORY_CHUNK).
const v4ResumeChunk = 1_000_000

// v4ResumeHead: the test chain's head (9 ranges of v4ResumeChunk per side).
const v4ResumeHead = 8_640_000

// v4ResumeToken: the token searched (lowercase, as v4PoolsOf compares).
const v4ResumeToken = "0x5555555555555555555555555555555555555555"

// v4TestPool is one of the token's pools on the test chain: side 0 = the
// token is currency0, 1 = currency1.
type v4TestPool struct {
	id    string
	side  int
	block uint64
}

// v4ResumePools: the token's pools on the test chain (8.64M blocks), in the
// order one uninterrupted search finds them (side 0 by block, then side 1):
// some on the boundaries of the 1M ranges, one of each side in the last
// 1,000 blocks.
func v4ResumePools(head uint64) []v4TestPool {
	id := func(n int) string { return fmt.Sprintf("0x%064x", n) }
	return []v4TestPool{
		{id(0xa1), 0, 500},
		{id(0xa2), 0, 999_999}, // the last block of range 1
		{id(0xa3), 0, 3_000_000},
		{id(0xa4), 0, 6_500_000},
		{id(0xa5), 0, head - 800},
		{id(0xb1), 1, 1_000_000}, // the first block of range 2
		{id(0xb2), 1, 4_200_000},
		{id(0xb3), 1, 7_999_999},
		{id(0xb4), 1, head - 700},
	}
}

// v4ResumeTest is the test chain with the token's pools (v4ResumePools) and
// another token's pool, its Initialize asks recorded by side, and a failure
// set by failSide / failFrom.
type v4ResumeTest struct {
	f        *fakeChain
	head     uint64
	o        *onchainSource
	failSide atomic.Int64  // the side whose range starting at failFrom fails (-1: none)
	failFrom atomic.Uint64 // …
	mu       sync.Mutex
	asks     [2][][2]uint64 // Initialize ranges asked, by side, retries included
	onFail   func()         // (optional) called before the failing range is answered
}

func newV4ResumeTest(t *testing.T, env map[string]string) *v4ResumeTest {
	t.Helper()
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	f.latest = v4ResumeHead // fixed, not the clock's (the ranges and pools near the head depend on it)
	s := &v4ResumeTest{f: f, head: f.latest}
	s.failSide.Store(-1)
	f.mu.Lock()
	for _, p := range v4ResumePools(s.head) {
		if p.side == 0 {
			f.initV4(p.id, v4ResumeToken, tWETH, zeroAddr, 500, p.block)
		} else {
			f.initV4(p.id, tUSDG, v4ResumeToken, zeroAddr, 500, p.block)
		}
	}
	f.initV4(fmt.Sprintf("0x%064x", 0xc1), tWETH, tUSDG, zeroAddr, 500, 2_000_000) // another token's pool
	f.mu.Unlock()
	f.setLogsQueryHook(func(q fakeLogQuery) string {
		if q.addr != tPM || len(q.topics) < 3 {
			return ""
		}
		if t0, _ := q.topics[0].(string); !strings.EqualFold(t0, topicInitV4) {
			return ""
		}
		side := len(q.topics) - 3
		s.mu.Lock()
		s.asks[side] = append(s.asks[side], [2]uint64{q.from, q.to})
		onFail := s.onFail
		s.mu.Unlock()
		if int(s.failSide.Load()) == side && q.from == s.failFrom.Load() {
			if onFail != nil {
				onFail()
			}
			return "rate limit exceeded"
		}
		return ""
	})
	all := map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4",
		"SCOUT_RPC_HISTORY_CHUNK": strconv.Itoa(v4ResumeChunk)}
	maps.Copy(all, env)
	s.o = testOnchain(t, f, all)
	if _, err := s.o.rpc.blockNumber(context.Background()); err != nil { // the client learns the head
		t.Fatal(err)
	}
	return s
}

// search runs v4PoolsOf up to the head: the pools as id@block.
func (s *v4ResumeTest) search(ctx context.Context) ([]string, error) {
	pools, err := s.o.v4PoolsOf(ctx, v4ResumeToken, s.head)
	if err != nil {
		return nil, err
	}
	return v4PoolNames(pools), nil
}

// asked: the Initialize ranges asked of one side so far.
func (s *v4ResumeTest) asked(side int) [][2]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asks[side])
}

// reset forgets the ranges asked so far and the failure.
func (s *v4ResumeTest) reset() {
	s.failSide.Store(-1)
	s.mu.Lock()
	s.asks = [2][][2]uint64{}
	s.onFail = nil
	s.mu.Unlock()
}

func v4PoolNames(pools []v4Pool) []string {
	out := []string{}
	for _, p := range pools {
		out = append(out, fmt.Sprintf("%s@%d", p.id, p.block))
	}
	return out
}

// v4State is what the token's record (o.v4Pools) says.
type v4State struct {
	known   bool
	through [2]uint64
	pools   []string
}

func v4StateOf(o *onchainSource, token string) v4State {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := o.v4Pools[token]
	if k == nil {
		return v4State{}
	}
	return v4State{known: true, through: k.through, pools: v4PoolNames(k.pools)}
}

func (a v4State) equal(b v4State) bool {
	return a.known == b.known && a.through == b.through && slices.Equal(a.pools, b.pools)
}

// v4Spans: the ranges a search with SCOUT_RPC_HISTORY_CHUNK = chunk asks over
// [from, to] when nothing fails (0: one range).
func v4Spans(from, to, chunk uint64) [][2]uint64 {
	if from > to {
		return nil
	}
	if chunk == 0 {
		return [][2]uint64{{from, to}}
	}
	return historySpans(from, to, chunk)
}

// v4RangeStart: the first block of the k-th 1M range (1-based).
func v4RangeStart(k int) uint64 {
	if k == 1 {
		return 1
	}
	return uint64(k-1) * v4ResumeChunk
}

// uninterruptedV4 searches a fresh chain with env and nothing failing: the
// pools and record to compare a resumed search with. Its asks are the plain
// ranges over [1, head] on each side, as before progress was kept.
func uninterruptedV4(t *testing.T, env map[string]string, chunk uint64) ([]string, v4State) {
	t.Helper()
	s := newV4ResumeTest(t, env)
	got, err := s.search(context.Background())
	if err != nil {
		t.Fatalf("uninterrupted search: %v", err)
	}
	if want := v4PoolNamesOf(v4ResumePools(s.head), [2]uint64{s.head, s.head}); !slices.Equal(got, want) {
		t.Fatalf("uninterrupted search: got %v, want %v; the test's chain changed", got, want)
	}
	for side := range 2 {
		if a, want := s.asked(side), v4Spans(1, s.head, chunk); !slices.Equal(a, want) {
			t.Fatalf("uninterrupted search, side %d: asked %v, want %v", side, a, want)
		}
	}
	return got, v4StateOf(s.o, v4ResumeToken)
}

// v4PoolNamesOf: the pools of each side at or before upTo[side], as id@block
// in the order of pools.
func v4PoolNamesOf(pools []v4TestPool, upTo [2]uint64) []string {
	out := []string{}
	for _, p := range pools {
		if p.block <= upTo[p.side] {
			out = append(out, fmt.Sprintf("%s@%d", p.id, p.block))
		}
	}
	return out
}

// v4FailCase: a search that fails on side, in the range starting at
// failFrom (the node, or the end of the caller's context).
type v4FailCase struct {
	name      string
	side      int
	failFrom  uint64
	cancel    bool      // the caller's context ends while that range is asked
	delivered [2]uint64 // the end of the last range of each side answered in full (0: none)
}

// runV4FailResume: the failed try keeps each side read through the end of its
// last range answered in full (but not into the last 1,000 blocks) and the
// pools found there, and asked no range twice before the failing one; the
// next try asks each side only from the block after it on, each range once,
// and ends like one uninterrupted search.
func runV4FailResume(t *testing.T, env map[string]string, chunk uint64, c v4FailCase) {
	t.Helper()
	wantPools, wantState := uninterruptedV4(t, env, chunk)
	s := newV4ResumeTest(t, env)
	head := s.head
	ctx := context.Background()
	if c.cancel {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		s.onFail = func() {
			cancel()
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // the client sees its context end first
		}
	}
	s.failFrom.Store(c.failFrom)
	s.failSide.Store(int64(c.side))
	if got, err := s.search(ctx); err == nil {
		t.Fatalf("first try: got %v, want the node's error", got)
	}
	for side := range 2 {
		var want [][2]uint64
		switch {
		case side == c.side:
			want = v4Spans(1, c.failFrom-1, chunk)
		case side < c.side:
			want = v4Spans(1, head, chunk) // the search before the failing one ended
		}
		var before [][2]uint64
		for _, a := range s.asked(side) {
			if a[0] < c.failFrom || side != c.side {
				before = append(before, a)
			} else if a[0] > c.failFrom {
				t.Fatalf("first try, side %d: asked %v after the failing range at %d", side, a, c.failFrom)
			}
		}
		if !slices.Equal(before, want) {
			t.Fatalf("first try, side %d: asked %v (the failing range left out), want %v", side, before, want)
		}
	}
	var wantThrough [2]uint64
	for side := range 2 {
		wantThrough[side] = min(c.delivered[side], head-1000)
	}
	got := v4StateOf(s.o, v4ResumeToken)
	want := v4State{known: wantThrough != [2]uint64{}, through: wantThrough, pools: v4PoolNamesOf(v4ResumePools(head), c.delivered)}
	if !got.equal(want) {
		t.Fatalf("after the failure: got %+v, want %+v", got, want)
	}

	s.reset()
	pools, err := s.search(context.Background())
	if err != nil {
		t.Fatalf("resumed try: %v", err)
	}
	for side := range 2 {
		if a, want := s.asked(side), v4Spans(wantThrough[side]+1, head, chunk); !slices.Equal(a, want) {
			t.Fatalf("resumed try, side %d: asked %v, want %v (from block %d on, each once)", side, a, want, wantThrough[side]+1)
		}
	}
	if !slices.Equal(pools, wantPools) {
		t.Fatalf("resumed try: got %v, want %v (as one uninterrupted search)", pools, wantPools)
	}
	if got := v4StateOf(s.o, v4ResumeToken); !got.equal(wantState) {
		t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted search)", got, wantState)
	}
}

// A search fails in range k of side 0 or side 1 (the node, or the end of the
// caller's context). A failure in side 0's first range keeps nothing; one in
// side 1's first range keeps side 0's search.
func TestV4PoolsOfResumesAfterFailure(t *testing.T) {
	head := uint64(v4ResumeHead)
	end := func(k int) uint64 { return v4RangeStart(k+1) - 1 } // the last block of range k
	for _, c := range []v4FailCase{
		{name: "side 0 fails range 1 (no progress)", side: 0, failFrom: v4RangeStart(1)},
		{name: "side 0 fails range 2", side: 0, failFrom: v4RangeStart(2), delivered: [2]uint64{end(1), 0}},
		{name: "side 0 fails range 4", side: 0, failFrom: v4RangeStart(4), delivered: [2]uint64{end(3), 0}},
		{name: "side 0 fails the last range", side: 0, failFrom: v4RangeStart(9), delivered: [2]uint64{end(8), 0}},
		{name: "side 1 fails range 1", side: 1, failFrom: v4RangeStart(1), delivered: [2]uint64{head, 0}},
		{name: "side 1 fails range 2", side: 1, failFrom: v4RangeStart(2), delivered: [2]uint64{head, end(1)}},
		{name: "side 1 fails range 5", side: 1, failFrom: v4RangeStart(5), delivered: [2]uint64{head, end(4)}},
		{name: "side 1 fails the last range", side: 1, failFrom: v4RangeStart(9), delivered: [2]uint64{head, end(8)}},
		{name: "context ends in side 0 range 3", side: 0, failFrom: v4RangeStart(3), cancel: true, delivered: [2]uint64{end(2), 0}},
		{name: "context ends in side 1 range 4", side: 1, failFrom: v4RangeStart(4), cancel: true, delivered: [2]uint64{head, end(3)}},
	} {
		t.Run(c.name, func(t *testing.T) { runV4FailResume(t, nil, v4ResumeChunk, c) })
	}
}

// The search fails after a range that ends inside the last 1,000 blocks was
// answered (each side in two ranges, [1, head-501] and [head-500, head]): that
// side is kept as read only through head-1000, so the next try reads the
// blocks after it again. The pools found there by the failed try (a5 and b4,
// at head-800 and head-700) are found again and kept once.
func TestV4PoolsOfFailureNearHead(t *testing.T) {
	head := uint64(v4ResumeHead)
	chunk := head - 500
	env := map[string]string{"SCOUT_RPC_HISTORY_CHUNK": strconv.FormatUint(chunk, 10)}
	for _, c := range []v4FailCase{
		{name: "side 0 fails its range at the head", side: 0, failFrom: head - 500, delivered: [2]uint64{head - 501, 0}},
		{name: "side 1 fails its range at the head", side: 1, failFrom: head - 500, delivered: [2]uint64{head, head - 501}},
	} {
		t.Run(c.name, func(t *testing.T) { runV4FailResume(t, env, chunk, c) })
	}
}

// A search that reached the head keeps each side read through head-1000; the
// next search at the same head reads only the last 1,000 blocks. When that
// read fails (side 0 or side 1), the record stays as after the first search
// (nothing near the head kept as read, no pool lost), and the try after it
// reads the last 1,000 blocks of both sides again and ends the same.
func TestV4PoolsOfFailureAfterHeadReached(t *testing.T) {
	_, wantState := uninterruptedV4(t, nil, v4ResumeChunk)
	for side := range 2 {
		t.Run(fmt.Sprintf("side %d fails", side), func(t *testing.T) {
			s := newV4ResumeTest(t, nil)
			if _, err := s.search(context.Background()); err != nil {
				t.Fatalf("first search: %v", err)
			}
			s.reset()
			near := v4Spans(s.head-999, s.head, v4ResumeChunk)
			s.failFrom.Store(near[0][0])
			s.failSide.Store(int64(side))
			if got, err := s.search(context.Background()); err == nil {
				t.Fatalf("failed try: got %v, want the node's error", got)
			}
			if a := s.asked(1 - side); side == 0 && len(a) != 0 || side == 1 && !slices.Equal(a, near) {
				t.Fatalf("failed try, side %d: asked %v", 1-side, a)
			}
			if got := v4StateOf(s.o, v4ResumeToken); !got.equal(wantState) {
				t.Fatalf("after the failed try: got %+v, want %+v (as after the first search)", got, wantState)
			}
			s.reset()
			if _, err := s.search(context.Background()); err != nil {
				t.Fatalf("resumed try: %v", err)
			}
			for sd := range 2 {
				if a := s.asked(sd); !slices.Equal(a, near) {
					t.Fatalf("resumed try, side %d: asked %v, want %v", sd, a, near)
				}
			}
			if got := v4StateOf(s.o, v4ResumeToken); !got.equal(wantState) {
				t.Fatalf("after the resumed try: got %+v, want %+v", got, wantState)
			}
		})
	}
}

// With SCOUT_RPC_HISTORY_CHUNK=0 (no cap, the documented rollback) each side
// is one range over the whole history: a failure of side 0 keeps nothing, one
// of side 1 keeps side 0's search.
func TestV4PoolsOfFailureNoCap(t *testing.T) {
	head := uint64(v4ResumeHead)
	env := map[string]string{"SCOUT_RPC_HISTORY_CHUNK": "0"}
	for _, c := range []v4FailCase{
		{name: "side 0 fails", side: 0, failFrom: 1},
		{name: "side 1 fails", side: 1, failFrom: 1, delivered: [2]uint64{head, 0}},
	} {
		t.Run(c.name, func(t *testing.T) { runV4FailResume(t, env, 0, c) })
	}
}

// Concurrent callers of a token whose side-1 search keeps failing at range 5
// (no lock of its own: each searches from what was kept when it started):
// every caller fails, the record is what one failed try keeps, and the next
// try asks side 1 only from range 5 on and ends like one uninterrupted
// search. Concurrent callers with nothing failing all end like one
// uninterrupted search.
func TestV4PoolsOfConcurrentCallers(t *testing.T) {
	wantPools, wantState := uninterruptedV4(t, nil, v4ResumeChunk)
	run := func(t *testing.T, s *v4ResumeTest) []error {
		t.Helper()
		const callers = 8
		errs := make([]error, callers)
		results := make([][]string, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = s.search(context.Background())
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err == nil && !slices.Equal(results[i], wantPools) {
				t.Fatalf("caller %d: got %v, want %v", i, results[i], wantPools)
			}
		}
		return errs
	}
	t.Run("side 1 fails at range 5", func(t *testing.T) {
		s := newV4ResumeTest(t, nil)
		failFrom := v4RangeStart(5)
		s.failFrom.Store(failFrom)
		s.failSide.Store(1)
		for i, err := range run(t, s) {
			if err == nil {
				t.Fatalf("caller %d: no error, want the node's", i)
			}
		}
		delivered := [2]uint64{s.head, failFrom - 1}
		want := v4State{known: true, through: [2]uint64{s.head - 1000, failFrom - 1}, pools: v4PoolNamesOf(v4ResumePools(s.head), delivered)}
		if got := v4StateOf(s.o, v4ResumeToken); !got.equal(want) {
			t.Fatalf("after the failed callers: got %+v, want %+v", got, want)
		}
		s.reset()
		pools, err := s.search(context.Background())
		if err != nil || !slices.Equal(pools, wantPools) {
			t.Fatalf("after the node answers: got %v %v, want %v", pools, err, wantPools)
		}
		if a, want := s.asked(1), v4Spans(failFrom, s.head, v4ResumeChunk); !slices.Equal(a, want) {
			t.Fatalf("after the node answers, side 1: asked %v, want %v", a, want)
		}
		if got := v4StateOf(s.o, v4ResumeToken); !got.equal(wantState) {
			t.Fatalf("after the node answers: got %+v, want %+v (as one uninterrupted search)", got, wantState)
		}
	})
	t.Run("nothing fails", func(t *testing.T) {
		s := newV4ResumeTest(t, nil)
		for i, err := range run(t, s) {
			if err != nil {
				t.Fatalf("caller %d: %v", i, err)
			}
		}
		if got := v4StateOf(s.o, v4ResumeToken); !got.equal(wantState) {
			t.Fatalf("after the callers: got %+v, want %+v (as one uninterrupted search)", got, wantState)
		}
	})
}

// The holder export's v4 pool-id search (holderV4PoolIDs, on the export's
// own onchainSource) goes on after a failure the same way: the next call of
// the token asks side 1 only from the failed range on and gets the same ids
// as without the failure.
func TestHolderV4PoolIDsResumeAfterFailure(t *testing.T) {
	st := onchainState{Kind: "v4", Pool: tPM, EntryBlock: 5_000_000}
	base := newV4ResumeTest(t, nil)
	wantPre, wantAll, err := base.o.holderV4PoolIDs(context.Background(), v4ResumeToken, st, base.head)
	if err != nil || len(wantPre) == 0 || len(wantAll) <= len(wantPre) {
		t.Fatalf("uninterrupted holderV4PoolIDs: got pre %v all %v %v, want pools before and after the entry block", wantPre, wantAll, err)
	}
	s := newV4ResumeTest(t, nil)
	failFrom := v4RangeStart(3)
	s.failFrom.Store(failFrom)
	s.failSide.Store(1)
	upper := "0x" + strings.ToUpper(v4ResumeToken[2:])
	if pre, all, err := s.o.holderV4PoolIDs(context.Background(), upper, st, s.head); err == nil {
		t.Fatalf("first try: got pre %v all %v, want the node's error", pre, all)
	}
	s.reset()
	pre, all, err := s.o.holderV4PoolIDs(context.Background(), upper, st, s.head)
	if err != nil || !slices.Equal(pre, wantPre) || !slices.Equal(all, wantAll) {
		t.Fatalf("resumed try: got pre %v all %v %v, want pre %v all %v", pre, all, err, wantPre, wantAll)
	}
	if a, want := s.asked(1), v4Spans(failFrom, s.head, v4ResumeChunk); !slices.Equal(a, want) {
		t.Fatalf("resumed try, side 1: asked %v, want %v", a, want)
	}
	if a, want := s.asked(0), v4Spans(s.head-999, s.head, v4ResumeChunk); !slices.Equal(a, want) {
		t.Fatalf("resumed try, side 0: asked %v, want %v (only the last 1,000 blocks again)", a, want)
	}
}
