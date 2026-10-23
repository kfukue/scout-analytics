package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A whole-history USD-pool sweep (usdPoolsInHistory) that stops on an error
// keeps its progress: the next try asks for no transfer range before the one
// that failed, does not check again the counterparties found not to be pools,
// and ends with the same pools, progress and price as one uninterrupted sweep.

// farWallet2: a wallet with one transfer of FAR (fewer than the far pool's
// two), far from the blocks asked, so it is checked after the pool.
const farWallet2 = "0x00000000000000000000000000000000000000d3"

// resumeHistChunk: the transfer ranges of the sweep (SCOUT_RPC_HISTORY_CHUNK).
const resumeHistChunk = 2_000_000

// sweepTest is the far-pool chain (newFarPoolChain) plus farWallet2, with the
// log cache off (every range asked reaches the chain) and the history cap at
// resumeHistChunk. FAR's counterparties, busiest first: the buyer (7
// transfers), farWallet (4), the far pool (2), farWallet2 (1). Its transfers,
// by block: 14.92M (farWallet2), 16.92M (pool), b1-10, b1+10 (b1 = 19.92M),
// 20.92M (pool), b2-10, b2+10 (b2 = 21.92M).
type sweepTest struct {
	*farPoolChain
	o        *onchainSource
	asks     rangeLog                    // the transfer sweep's eth_getLogs (FAR, ranges above 200,000 blocks)
	nearAsks rangeLog                    // every other eth_getLogs of FAR (the near-block search, the blocks near the head)
	failFrom atomic.Int64                // a sweep range starting here fails (0 = none)
	callFail atomic.Value                // string: token0() of this address fails ("" = none)
	callHook func(to, sel string) string // (optional) sees every eth_call; set with f.mu held
	mu       sync.Mutex
	token0   map[string]int // token0() asked, by address
}

func newSweepTest(t *testing.T) *sweepTest {
	t.Helper()
	fastRetries(t)
	s := &sweepTest{farPoolChain: newFarPoolChain(t), token0: map[string]int{}}
	s.callFail.Store("")
	f := s.f
	f.mu.Lock()
	f.transfer(farAsset, farWallet2, uBuyer, s.b1-5_000_000, "0xw9")
	f.callErr = func(to, sel string, _ uint64) string { // called with f.mu held
		if s.callHook != nil {
			if msg := s.callHook(to, sel); msg != "" {
				return msg
			}
		}
		if sel != selToken0 {
			return ""
		}
		s.mu.Lock()
		s.token0[to]++
		s.mu.Unlock()
		if to == s.callFail.Load().(string) {
			return "rate limit exceeded"
		}
		return ""
	}
	f.mu.Unlock()
	f.setLogsHook(func(addr string, from, to uint64) string {
		if addr != farAsset {
			return ""
		}
		if to-from+1 <= 200_000 {
			s.nearAsks.add(rangeAsk{addr: addr, from: from, to: to})
			return ""
		}
		s.asks.add(rangeAsk{addr: addr, from: from, to: to})
		if ff := s.failFrom.Load(); ff > 0 && from == uint64(ff) {
			return "rate limit exceeded"
		}
		return ""
	})
	s.o = s.open(t, nil)
	return s
}

// open returns an onchainSource on the test's chain with its settings, extra
// over them (e.g. another SCOUT_RPC_HISTORY_CHUNK).
func (s *sweepTest) open(t *testing.T, extra map[string]string) *onchainSource {
	t.Helper()
	env := map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_HISTORY_CHUNK": "2000000"}
	maps.Copy(env, s.env)
	maps.Copy(env, extra)
	return testOnchain(t, s.f, env)
}

// reset forgets the ranges and token0() calls asked so far.
func (s *sweepTest) reset() {
	for _, r := range []*rangeLog{&s.asks, &s.nearAsks} {
		r.mu.Lock()
		r.asks = nil
		r.mu.Unlock()
	}
	s.mu.Lock()
	clear(s.token0)
	s.mu.Unlock()
}

func (s *sweepTest) token0Asked(addr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token0[addr]
}

// sweepState is what the asset's set says about its whole-history search.
type sweepState struct {
	through        uint64
	seen           []string
	pending        map[string]int
	pools          []string
	logs, checks   int
	noPool, priced bool
}

func sweepStateOf(o *onchainSource, asset string) sweepState {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.usdPools[asset]
	if set == nil {
		return sweepState{}
	}
	st := sweepState{through: set.histThrough, pending: maps.Clone(set.histPending), logs: set.histLogs, checks: set.histChecks,
		noPool: !set.noPoolUntil.IsZero(), priced: len(set.noPrice) > 0}
	for a, ok := range set.histSeen {
		if ok {
			st.seen = append(st.seen, a)
		}
	}
	slices.Sort(st.seen)
	for _, p := range set.pools {
		st.pools = append(st.pools, p.poolRef())
	}
	slices.Sort(st.pools)
	if st.pending == nil {
		st.pending = map[string]int{}
	}
	return st
}

func (a sweepState) equal(b sweepState) bool {
	return a.through == b.through && slices.Equal(a.seen, b.seen) && maps.Equal(a.pending, b.pending) &&
		slices.Equal(a.pools, b.pools) && a.logs == b.logs && a.checks == b.checks && a.noPool == b.noPool && a.priced == b.priced
}

// spanFrom: the first block of the k-th transfer range of the sweep (1-based).
func spanFrom(k int) uint64 {
	if k == 1 {
		return 1
	}
	return uint64(k-1) * resumeHistChunk
}

// wantFailure: a try that stopped on the node (or ctx): an error that is not
// one of the USD answers.
func wantFailure(t *testing.T, what string, p float64, ok bool, err error) {
	t.Helper()
	if err == nil || ok || errors.Is(err, errUSDSearchPending) || errors.Is(err, errNoPriceYet) || errors.Is(err, errNoUSDSource) {
		t.Fatalf("%s: got %v ok=%v err=%v, want the node's error", what, p, ok, err)
	}
}

func wantPrice(t *testing.T, what string, p float64, ok bool, err error, want float64) {
	t.Helper()
	if err != nil || !ok || !near6(p, want) {
		t.Fatalf("%s: got %v ok=%v err=%v, want $%v", what, p, ok, err, want)
	}
}

// uninterruptedSweep prices FAR at b1 on a fresh sweepTest with nothing
// failing: the state and source to compare a resumed sweep with.
func uninterruptedSweep(t *testing.T) (sweepState, string) {
	t.Helper()
	return uninterruptedSweepOn(t, newSweepTest(t))
}

// uninterruptedSweepOn is uninterruptedSweep on a sweepTest the caller set up
// (its chain and settings), with nothing failing.
func uninterruptedSweepOn(t *testing.T, s *sweepTest) (sweepState, string) {
	t.Helper()
	p, ok, err := s.o.quoteUSD(context.Background(), farAsset, s.b1)
	wantPrice(t, "uninterrupted sweep", p, ok, err, 2)
	return sweepStateOf(s.o, farAsset), s.o.quoteSource(farAsset)
}

// The transfer read fails at its k-th range (the node, or the end of the
// caller's context): the ranges before k are kept and not asked again; the
// next try starts at range k and ends like one uninterrupted sweep. A failure
// in the first range keeps nothing (as before).
func TestUSDSweepResumesAfterTransferFailure(t *testing.T) {
	want, wantSrc := uninterruptedSweep(t)
	for _, c := range []struct {
		name   string
		k      int
		cancel bool // the caller's context ends while range k is asked
	}{
		{"node fails range 1 (no progress)", 1, false},
		{"node fails range 2", 2, false},
		{"node fails range 10", 10, false},
		{"node fails range 11", 11, false},
		{"node fails the last range", 13, false},
		{"context ends in range 10", 10, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSweepTest(t)
			head := s.f.latest
			spans := historySpans(1, head, resumeHistChunk)
			if len(spans) != 13 {
				t.Fatalf("%d transfer ranges over 1-%d, want 13 (the test's ranges)", len(spans), head)
			}
			failFrom := spanFrom(c.k)
			ctx := context.Background()
			if c.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				s.f.setLogsHook(func(addr string, from, to uint64) string {
					if addr != farAsset || to-from+1 <= 200_000 {
						return ""
					}
					s.asks.add(rangeAsk{addr: addr, from: from, to: to})
					if from == failFrom {
						cancel()
						<-ctx.Done()
						time.Sleep(20 * time.Millisecond) // the client sees its context end first
						return "rate limit exceeded"
					}
					return ""
				})
			} else {
				s.failFrom.Store(int64(failFrom))
			}
			p, ok, err := s.o.quoteUSD(ctx, farAsset, s.b1)
			wantFailure(t, "first try", p, ok, err)
			got := sweepStateOf(s.o, farAsset)
			wantThrough := failFrom - 1
			if c.k == 1 {
				wantThrough = 0
			}
			if got.through != wantThrough || got.noPool || got.priced || len(got.pools) != 0 || len(got.seen) != 0 {
				t.Fatalf("after the failure at range %d: got %+v, want transfers read through %d, no answer kept", c.k, got, wantThrough)
			}
			if c.k == 1 && (len(got.pending) != 0 || got.logs != 0) {
				t.Fatalf("failure in the first range: got %+v, want nothing kept", got)
			}
			if first := s.asks.list(farAsset); !slices.Equal(rangesBefore(first, failFrom), historySpans(1, failFrom-1, resumeHistChunk)) {
				t.Fatalf("first try asked %v, want each range before %d once", first, failFrom)
			}

			s.f.setLogsHook(func(addr string, from, to uint64) string {
				if addr == farAsset && to-from+1 > 200_000 {
					s.asks.add(rangeAsk{addr: addr, from: from, to: to})
				}
				return ""
			})
			s.reset()
			p, ok, err = s.o.quoteUSD(context.Background(), farAsset, s.b1)
			wantPrice(t, "resumed try", p, ok, err, 2)
			asked := rangesBefore(s.asks.list(farAsset), head+1)
			if wantAsks := historySpans(failFrom, head, resumeHistChunk); !slices.Equal(asked, wantAsks) {
				t.Fatalf("resumed try asked %v, want %v (from range %d on, each once)", asked, wantAsks, c.k)
			}
			if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
				t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
			}
			if src := s.o.quoteSource(farAsset); src != wantSrc {
				t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
			}
		})
	}
}

// rangesBefore: the asks starting before block b, as [from, to] pairs.
func rangesBefore(asks []rangeAsk, b uint64) [][2]uint64 {
	var out [][2]uint64
	for _, a := range asks {
		if a.from < b {
			out = append(out, [2]uint64{a.from, a.to})
		}
	}
	return out
}

// A counterparty's token0() fails after the transfers were read: the
// transfers and the counterparties found not to be pools are kept; a pool
// found before the failure is not kept as an answer but checked again by the
// next try. The next try reads only the blocks near the head and ends like
// one uninterrupted sweep.
func TestUSDSweepResumesAfterCheckFailure(t *testing.T) {
	want, wantSrc := uninterruptedSweep(t)
	for _, c := range []struct {
		name    string
		fail    string   // token0() of this counterparty fails
		seen    []string // counterparties kept as checked after the failure
		pending []string // … and left to check (the far pool among them: checked again)
	}{
		// (the buyer and farWallet are also checked by the near-block search,
		// so the failures are on the two counterparties only the sweep checks)
		{"the pool's token0() fails", farPool, []string{uBuyer, farWallet}, []string{farPool, farWallet2}},
		{"fails after the pool was found", farWallet2, []string{uBuyer, farWallet}, []string{farPool, farWallet2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSweepTest(t)
			head := s.f.latest
			s.callFail.Store(c.fail)
			p, ok, err := s.o.quoteUSD(context.Background(), farAsset, s.b1)
			wantFailure(t, "first try", p, ok, err)
			got := sweepStateOf(s.o, farAsset)
			slices.Sort(c.seen)
			pend := slices.Sorted(maps.Keys(got.pending))
			slices.Sort(c.pending)
			if got.through != head-1000 || !slices.Equal(got.seen, c.seen) || !slices.Equal(pend, c.pending) ||
				len(got.pools) != 0 || got.noPool || got.priced || got.checks != len(c.seen) {
				t.Fatalf("after the failed check of %s: got %+v, want transfers read through %d, checked %v, left %v, no pool kept",
					c.fail, got, head-1000, c.seen, c.pending)
			}

			s.callFail.Store("")
			s.reset()
			p, ok, err = s.o.quoteUSD(context.Background(), farAsset, s.b1)
			wantPrice(t, "resumed try", p, ok, err, 2)
			if a := s.asks.list(farAsset); len(a) != 0 {
				t.Fatalf("resumed try asked the transfer ranges %v again, want none", a)
			}
			for _, a := range s.nearAsks.list(farAsset) {
				if a.from <= head-1000 {
					t.Fatalf("resumed try asked FAR's transfers in %d-%d, want only blocks after %d", a.from, a.to, head-1000)
				}
			}
			for _, a := range c.seen {
				if n := s.token0Asked(a); n != 0 {
					t.Fatalf("resumed try asked token0() of %s %d time(s), want 0 (checked before the failure)", a, n)
				}
			}
			if n := s.token0Asked(farPool); n != 1 {
				t.Fatalf("resumed try asked token0() of the far pool %d time(s), want 1", n)
			}
			if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
				t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
			}
			if src := s.o.quoteSource(farAsset); src != wantSrc {
				t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
			}
		})
	}
}

// The caller's context ends while the far pool is checked (after its
// token0() and token1(), during getReserves()): the check reads as "not a
// pool" (getReserves and slot0 both fail), but it is not kept, so the next
// try checks the pool again and prices the block; kept, it would end in the
// definitive "no USD source".
func TestUSDSweepContextEndsDuringCheck(t *testing.T) {
	want, _ := uninterruptedSweep(t)
	s := newSweepTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.f.mu.Lock()
	s.callHook = func(to, sel string) string {
		if to == farPool && sel == selGetReserves {
			cancel()
			time.Sleep(20 * time.Millisecond) // the client sees its context end first
			return "rate limit exceeded"
		}
		return ""
	}
	s.f.mu.Unlock()
	p, ok, err := s.o.quoteUSD(ctx, farAsset, s.b1)
	wantFailure(t, "first try (context ends)", p, ok, err)
	got := sweepStateOf(s.o, farAsset)
	if slices.Contains(got.seen, farPool) || got.pending[farPool] == 0 || got.noPool || len(got.pools) != 0 {
		t.Fatalf("after the context ended in the pool's check: got %+v, want the pool left to check, no answer kept", got)
	}
	s.f.mu.Lock()
	s.callHook = nil
	s.f.mu.Unlock()
	p, ok, err = s.o.quoteUSD(context.Background(), farAsset, s.b1)
	wantPrice(t, "next try", p, ok, err, 2)
	if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
		t.Fatalf("after the next try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
	}
}

// A resumed sweep stops where one uninterrupted sweep would: the logs read and
// the counterparties checked by the failed try count against the sweep's
// bounds, so each try's answer (and the state after it) is the same as the
// uninterrupted sweep's.
func TestUSDSweepResumeKeepsBounds(t *testing.T) {
	ctx := context.Background()
	t.Run("checks", func(t *testing.T) {
		setHistBounds(t, 100_000, 3)
		// Uninterrupted: the buyer, farWallet and the pool checked (priced),
		// farWallet2 left for the next sweep.
		base := newSweepTest(t)
		p, ok, err := base.o.quoteUSD(ctx, farAsset, base.b1)
		wantPrice(t, "uninterrupted", p, ok, err, 2)
		want := sweepStateOf(base.o, farAsset)
		if !maps.Equal(want.pending, map[string]int{farWallet2: 1}) {
			t.Fatalf("uninterrupted: left to check %v, want farWallet2 only; the test's chain changed", want.pending)
		}

		// The pool's token0() fails: the buyer and farWallet are checked, so
		// one check of the sweep's three is left for the next try.
		s := newSweepTest(t)
		s.callFail.Store(farPool)
		p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
		wantFailure(t, "try 1 (the pool fails)", p, ok, err)
		if got := sweepStateOf(s.o, farAsset); got.checks != 2 {
			t.Fatalf("after try 1: got %+v, want 2 checks counted", got)
		}
		s.callFail.Store("")
		p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
		wantPrice(t, "try 2", p, ok, err, 2)
		if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
			t.Fatalf("after try 2: got %+v, want %+v (as the uninterrupted sweep: farWallet2 left)", got, want)
		}
	})
	t.Run("logs", func(t *testing.T) {
		setHistBounds(t, 2, 256)
		// Uninterrupted: 14.92M and 16.92M read, cut at b1-10.
		base := newSweepTest(t)
		p, ok, err := base.o.quoteUSD(ctx, farAsset, base.b1)
		wantPrice(t, "uninterrupted", p, ok, err, 2)
		want := sweepStateOf(base.o, farAsset)
		if want.through != base.b1-11 {
			t.Fatalf("uninterrupted: transfers read through %d, want %d (cut at the third log); the test's chain changed", want.through, base.b1-11)
		}

		// The range holding 16.92M fails: 14.92M read (one log of the two).
		s := newSweepTest(t)
		s.failFrom.Store(int64(spanFrom(9)))
		p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
		wantFailure(t, "try 1 (range 9 fails)", p, ok, err)
		if got := sweepStateOf(s.o, farAsset); got.logs != 1 || got.through != spanFrom(9)-1 {
			t.Fatalf("after try 1: got %+v, want 1 log read, through %d", got, spanFrom(9)-1)
		}
		s.failFrom.Store(0)
		p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
		wantPrice(t, "try 2", p, ok, err, 2)
		if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
			t.Fatalf("after try 2: got %+v, want %+v (as the uninterrupted sweep: cut after the second log)", got, want)
		}
	})
}

// Concurrent callers of a cold asset whose sweep keeps failing at range k:
// they sweep one at a time, each going on from the progress kept, so every
// range before k is asked once in all; once the node answers, the next caller
// ends like one uninterrupted sweep.
func TestUSDSweepResumeConcurrentCallers(t *testing.T) {
	want, _ := uninterruptedSweep(t)
	s := newSweepTest(t)
	const k = 11
	failFrom := spanFrom(k)
	s.failFrom.Store(int64(failFrom))
	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.o.quoteUSD(context.Background(), farAsset, s.b1)
			if err == nil && ok {
				err = errors.New("priced")
			}
			if errors.Is(err, errUSDSearchPending) || errors.Is(err, errNoPriceYet) || errors.Is(err, errNoUSDSource) {
				err = errors.New("a USD answer: " + err.Error())
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: no error, want the node's", i)
		}
	}
	if got, wantAsks := rangesBefore(s.asks.list(farAsset), failFrom), historySpans(1, failFrom-1, resumeHistChunk); !slices.Equal(got, wantAsks) {
		t.Fatalf("%d concurrent callers asked %v before block %d, want each range once: %v", callers, got, failFrom, wantAsks)
	}
	s.failFrom.Store(0)
	p, ok, err := s.o.quoteUSD(context.Background(), farAsset, s.b1)
	wantPrice(t, "after the node answers", p, ok, err, 2)
	if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
		t.Fatalf("after the node answers: got %+v, want %+v (as one uninterrupted sweep)", got, want)
	}
}

// farWallet3, farWallet4, farWallet5: wallets with one transfer of FAR each,
// added by the tests below.
const (
	farWallet3 = "0x00000000000000000000000000000000000000d4"
	farWallet4 = "0x00000000000000000000000000000000000000d5"
	farWallet5 = "0x00000000000000000000000000000000000000d6"
)

// The transfer read fails after a range that ends inside the last 1,000
// blocks was answered (a node splitting its ranges small near the head): the
// transfers are kept as read only through head-1000, as after a read to the
// head, so the next try reads the blocks after head-1000 again (they may
// still change) and ends like one uninterrupted sweep. The chain has a FAR
// transfer in those blocks (farWallet3, at head-700): the first try counts it
// and the next try counts it again, so it is left to check with 2 transfers
// instead of 1. That count only orders the checks; every counterparty is
// checked by the end (nothing left in either sweep), so the final states are
// compared in full.
func TestUSDSweepFailureNearHeadReadsLastBlocksAgain(t *testing.T) {
	ctx := context.Background()
	// nearHead: the sweep reads FAR's transfers in two ranges, [1, head-501]
	// and [head-500, head].
	nearHead := func(t *testing.T) (*sweepTest, uint64) {
		t.Helper()
		s := newSweepTest(t)
		head := s.f.latest
		s.f.mu.Lock()
		s.f.transfer(farAsset, farWallet3, uBuyer, head-700, "0xwa")
		s.f.mu.Unlock()
		s.o = s.open(t, map[string]string{"SCOUT_RPC_HISTORY_CHUNK": strconv.FormatUint(head-500, 10)})
		return s, head
	}
	base, _ := nearHead(t)
	want, wantSrc := uninterruptedSweepOn(t, base)
	if len(want.pending) != 0 {
		t.Fatalf("uninterrupted: left to check %v, want none; the test's chain changed", want.pending)
	}

	s, head := nearHead(t)
	var asks rangeLog // FAR's transfer ranges near the head
	var failing atomic.Bool
	failing.Store(true)
	s.f.setLogsHook(func(addr string, from, to uint64) string {
		if addr != farAsset || to+2000 < head {
			return ""
		}
		asks.add(rangeAsk{addr: addr, from: from, to: to})
		if failing.Load() && from == head-500 {
			return "rate limit exceeded"
		}
		return ""
	})
	p, ok, err := s.o.quoteUSD(ctx, farAsset, s.b1)
	wantFailure(t, "first try (the range at the head fails)", p, ok, err)
	if first := rangesBefore(asks.list(farAsset), head+1); len(first) == 0 || first[0] != [2]uint64{1, head - 501} {
		t.Fatalf("first try asked %v near the head, want [1 %d] answered first; the test's ranges changed", first, head-501)
	}
	got := sweepStateOf(s.o, farAsset)
	if got.through != head-1000 || got.pending[farWallet3] != 1 || len(got.seen) != 0 || len(got.pools) != 0 || got.noPool || got.priced {
		t.Fatalf("after the failure at block %d: got %+v, want transfers read through %d (not %d, the end of the range answered), farWallet3 counted, no answer kept",
			head-500, got, head-1000, head-501)
	}

	failing.Store(false)
	asks.mu.Lock()
	asks.asks = nil
	asks.mu.Unlock()
	p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
	wantPrice(t, "resumed try", p, ok, err, 2)
	if got, wantAsks := rangesBefore(asks.list(farAsset), head+1), [][2]uint64{{head - 999, head - 501}, {head - 500, head}}; !slices.Equal(got, wantAsks) {
		t.Fatalf("resumed try asked %v near the head, want %v (every block after %d read again)", got, wantAsks, head-1000)
	}
	if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
		t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
	}
	if src := s.o.quoteSource(farAsset); src != wantSrc {
		t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
	}
}

// A sweep that reached the head keeps its transfers as read through
// head-1000. The next sweep at the same head (a caller that waited for the
// first one, or a node whose head has not moved) starts after its own
// settled block (from = head-999 > through = head-1000): every block it reads
// is near the head. (Once the head moves by d blocks, through = head-1000+d
// is at or past from, so this case is only d = 0, or a head that went back.)
// That read fails, either in its first range (nothing answered) or after its
// first range was answered (the end of that range, head-501, is past
// through): the transfers stay read through head-1000 (readTo stays from-1;
// histThrough does not move into the last 1,000 blocks), no answer is kept,
// and the next try reads every block after head-1000 again and ends like two
// uninterrupted sweeps. The chain has a FAR transfer in the first near-head
// range (farWallet3, at head-700), so a failure after that range was answered
// counts its log against the sweep's bound.
func TestUSDSweepFailureAfterHeadReachedKeepsThrough(t *testing.T) {
	ctx := context.Background()
	// atHead: the sweep reads FAR's transfers near the head in two ranges,
	// [head-999, head-501] and [head-500, head]; the first sweep (pricing FAR
	// at b1) has run to the head.
	atHead := func(t *testing.T) (*sweepTest, uint64) {
		t.Helper()
		s := newSweepTest(t)
		head := s.f.latest
		s.f.mu.Lock()
		s.f.transfer(farAsset, farWallet3, uBuyer, head-700, "0xwa")
		s.f.mu.Unlock()
		s.o = s.open(t, map[string]string{"SCOUT_RPC_HISTORY_CHUNK": strconv.FormatUint(head-500, 10)})
		uninterruptedSweepOn(t, s)
		if got := sweepStateOf(s.o, farAsset); got.through != head-1000 || len(got.pending) != 0 || len(got.pools) == 0 {
			t.Fatalf("first sweep: got %+v, want transfers read through %d, nothing left to check, the pool kept; the test's chain changed", got, head-1000)
		}
		return s, head
	}
	base, _ := atHead(t)
	if h, err := base.o.usdPoolsInHistory(ctx, farAsset); err != nil || !h.complete {
		t.Fatalf("uninterrupted second sweep: got complete=%v err=%v, want a complete sweep", h.complete, err)
	}
	want, wantSrc := sweepStateOf(base.o, farAsset), base.o.quoteSource(farAsset)

	for _, c := range []struct {
		name     string
		failFrom uint64 // the near-head range starting here fails, as an offset under the head
		ranges   int    // near-head ranges the failed try asked (the failed one included)
		logs     int    // transfer logs counted by the failed try
	}{
		{"first range fails (nothing answered)", 999, 1, 0},
		{"second range fails (first answered, past through)", 500, 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, head := atHead(t)
			before := sweepStateOf(s.o, farAsset)
			var asks rangeLog // FAR's transfer ranges near the head
			var failing atomic.Bool
			failing.Store(true)
			s.f.setLogsHook(func(addr string, from, to uint64) string {
				if addr != farAsset || to+2000 < head {
					return ""
				}
				asks.add(rangeAsk{addr: addr, from: from, to: to})
				if failing.Load() && from == head-c.failFrom {
					return "rate limit exceeded"
				}
				return ""
			})
			h, err := s.o.usdPoolsInHistory(ctx, farAsset)
			if err == nil || errors.Is(err, errUSDSearchPending) || errors.Is(err, errNoPriceYet) || errors.Is(err, errNoUSDSource) {
				t.Fatalf("failed try: got %+v err=%v, want the node's error", h, err)
			}
			wantAsks := [][2]uint64{{head - 999, head - 501}, {head - 500, head}}[:c.ranges]
			// (the failed range is asked again by the client's retries)
			if got := slices.Compact(rangesBefore(asks.list(farAsset), head+1)); !slices.Equal(got, wantAsks) {
				t.Fatalf("failed try asked %v near the head (retries merged), want %v; the test's ranges changed", got, wantAsks)
			}
			got := sweepStateOf(s.o, farAsset)
			if got.through != head-1000 {
				t.Fatalf("after the failure at block %d: transfers read through %d, want %d (unchanged: no block after it kept as read)",
					head-c.failFrom, got.through, head-1000)
			}
			if got.logs != c.logs {
				t.Fatalf("after the failure at block %d: %d log(s) counted, want %d", head-c.failFrom, got.logs, c.logs)
			}
			got.logs = before.logs
			if !got.equal(before) {
				t.Fatalf("after the failure at block %d: got %+v, want %+v (as after the first sweep: no answer kept)", head-c.failFrom, got, before)
			}

			failing.Store(false)
			asks.mu.Lock()
			asks.asks = nil
			asks.mu.Unlock()
			h, err = s.o.usdPoolsInHistory(ctx, farAsset)
			if err != nil || !h.complete {
				t.Fatalf("resumed try: got complete=%v err=%v, want a complete sweep", h.complete, err)
			}
			if got, wantAsks := rangesBefore(asks.list(farAsset), head+1), [][2]uint64{{head - 999, head - 501}, {head - 500, head}}; !slices.Equal(got, wantAsks) {
				t.Fatalf("resumed try asked %v near the head, want %v (every block after %d read again)", got, wantAsks, head-1000)
			}
			if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
				t.Fatalf("after the resumed try: got %+v, want %+v (as two uninterrupted sweeps)", got, want)
			}
			if src := s.o.quoteSource(farAsset); src != wantSrc {
				t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
			}
		})
	}
}

// With SCOUT_RPC_HISTORY_CHUNK=0 (no cap, the documented rollback) the sweep's
// scan fetches SCOUT_RPC_PARALLEL ranges at once (withScanReached on that
// path). The node refuses ranges of more than 2,000,000 blocks, so the scan
// halves its first range down to 1,620,000 blocks and then fetches
// [3.24M, 4.86M), [4.86M, 6.48M), [6.48M, 8.1M) and [8.1M, 9.72M) as one
// batch; the second of them fails for good (after the later two were asked).
// The transfers kept as read are those through 4.86M-1, the end of the last
// range handed over in full (farWallet4's, at 4M), and none of a range
// fetched but never handed over (farWallet5's, at 7M); the next try asks no
// range before 4.86M and ends like one uninterrupted sweep.
func TestUSDSweepParallelScanKeepsLastFullRange(t *testing.T) {
	const (
		maxRange = 2_000_000 // the node refuses larger ranges
		size     = 1_620_000 // the scan's range size after halving
		failFrom = 3 * size  // the second range of the first batch of four
	)
	ctx := context.Background()
	refuse := func(from, to uint64) bool { return to-from+1 > maxRange }
	setup := func(t *testing.T) *sweepTest {
		t.Helper()
		s := newSweepTest(t)
		s.f.mu.Lock()
		s.f.transfer(farAsset, farWallet4, uBuyer, 4_000_000, "0xwb")
		s.f.transfer(farAsset, farWallet5, uBuyer, 7_000_000, "0xwc")
		s.f.mu.Unlock()
		s.o = s.open(t, map[string]string{"SCOUT_RPC_HISTORY_CHUNK": "0", "SCOUT_RPC_PARALLEL": "4"})
		return s
	}
	base := setup(t)
	base.f.setLogsHook(func(addr string, from, to uint64) string {
		if addr == farAsset && refuse(from, to) {
			return "query returned more than 10000 results"
		}
		return ""
	})
	want, wantSrc := uninterruptedSweepOn(t, base)

	s := setup(t)
	var asks rangeLog // every eth_getLogs of FAR
	asked := func(from, to uint64) bool {
		return slices.ContainsFunc(asks.list(farAsset), func(a rangeAsk) bool { return a.from == from && a.to == to })
	}
	var failing atomic.Bool
	failing.Store(true)
	s.f.setLogsHook(func(addr string, from, to uint64) string {
		if addr != farAsset {
			return ""
		}
		asks.add(rangeAsk{addr: addr, from: from, to: to, refused: refuse(from, to)})
		switch {
		case refuse(from, to):
			return "query returned more than 10000 results"
		case failing.Load() && from == failFrom:
			// Fail once the later ranges of the batch were asked, so they are
			// fetched while this one fails.
			for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
				if asked(failFrom+size, failFrom+2*size-1) && asked(failFrom+2*size, failFrom+3*size-1) {
					break
				}
			}
			return "rate limit exceeded"
		}
		return ""
	})
	p, ok, err := s.o.quoteUSD(ctx, farAsset, s.b1)
	wantFailure(t, "first try (a range in a batch fails)", p, ok, err)
	for k := uint64(1); k <= 4; k++ {
		if from := (k + 1) * size; !asked(from, from+size-1) {
			t.Fatalf("first try asked %v, want the range %d-%d of the batch; the test's ranges changed", asks.list(farAsset), from, from+size-1)
		}
	}
	got := sweepStateOf(s.o, farAsset)
	if wantPend := map[string]int{farWallet4: 1, uBuyer: 1}; got.through != failFrom-1 || got.logs != 1 || !maps.Equal(got.pending, wantPend) ||
		len(got.seen) != 0 || len(got.pools) != 0 || got.noPool || got.priced {
		t.Fatalf("after the failure at block %d: got %+v, want transfers read through %d (1 log: farWallet4's), left to check %v, no answer kept",
			failFrom, got, failFrom-1, wantPend)
	}

	failing.Store(false)
	asks.mu.Lock()
	asks.asks = nil
	asks.mu.Unlock()
	p, ok, err = s.o.quoteUSD(ctx, farAsset, s.b1)
	wantPrice(t, "resumed try", p, ok, err, 2)
	for _, a := range asks.list(farAsset) {
		if a.from < failFrom {
			t.Fatalf("resumed try asked FAR's transfers in %d-%d, want nothing before %d (read by the first try)", a.from, a.to, failFrom)
		}
	}
	if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
		t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
	}
	if src := s.o.quoteSource(farAsset); src != wantSrc {
		t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
	}
}

// The decimals() of a kept pool's other side (USDG, read by fillQuotes after
// the checks) fails: the pool goes back to those left to check (not kept as
// checked, nor counted in the checks used), the wallets checked are kept, and
// no answer is kept. The next try checks the pool again (and only the pool),
// prices the block and ends like one uninterrupted sweep.
func TestUSDSweepResumesAfterQuoteTokenFailure(t *testing.T) {
	want, wantSrc := uninterruptedSweep(t)
	s := newSweepTest(t)
	head := s.f.latest
	var failing atomic.Bool
	failing.Store(true)
	var quoteAsks atomic.Int64
	s.f.mu.Lock()
	s.callHook = func(to, sel string) string {
		if to == tUSDG && sel == selDecimals {
			quoteAsks.Add(1)
			if failing.Load() {
				return "rate limit exceeded"
			}
		}
		return ""
	}
	s.f.mu.Unlock()
	p, ok, err := s.o.quoteUSD(context.Background(), farAsset, s.b1)
	wantFailure(t, "first try (decimals() of USDG fails)", p, ok, err)
	if quoteAsks.Load() == 0 || s.token0Asked(farPool) != 1 {
		t.Fatalf("first try: decimals() of USDG asked %d time(s), token0() of the pool %d, want both; the test's chain changed",
			quoteAsks.Load(), s.token0Asked(farPool))
	}
	got := sweepStateOf(s.o, farAsset)
	wantSeen := []string{uBuyer, farWallet, farWallet2}
	slices.Sort(wantSeen)
	if wantPend := map[string]int{farPool: 2}; got.through != head-1000 || !slices.Equal(got.seen, wantSeen) || !maps.Equal(got.pending, wantPend) ||
		got.checks != len(wantSeen) || len(got.pools) != 0 || got.noPool || got.priced {
		t.Fatalf("after the failed decimals() of USDG: got %+v, want transfers read through %d, checked %v (%d checks used), left %v, no pool kept",
			got, head-1000, wantSeen, len(wantSeen), wantPend)
	}

	failing.Store(false)
	s.reset()
	p, ok, err = s.o.quoteUSD(context.Background(), farAsset, s.b1)
	wantPrice(t, "resumed try", p, ok, err, 2)
	if a := s.asks.list(farAsset); len(a) != 0 {
		t.Fatalf("resumed try asked the transfer ranges %v again, want none", a)
	}
	for _, a := range wantSeen {
		if n := s.token0Asked(a); n != 0 {
			t.Fatalf("resumed try asked token0() of %s %d time(s), want 0 (checked before the failure)", a, n)
		}
	}
	if n := s.token0Asked(farPool); n != 1 {
		t.Fatalf("resumed try asked token0() of the far pool %d time(s), want 1", n)
	}
	if got := sweepStateOf(s.o, farAsset); !got.equal(want) {
		t.Fatalf("after the resumed try: got %+v, want %+v (as one uninterrupted sweep)", got, want)
	}
	if src := s.o.quoteSource(farAsset); src != wantSrc {
		t.Fatalf("quoteSource after the resumed try: got %q, want %q", src, wantSrc)
	}
}

// withScanReached: after a scan that fails at its k-th range, the block
// reached is the end of range k-1 (0 when the first range fails), every log
// handed over is at or before it, and the context's own progress function is
// still called.
func TestWithScanReachedAfterFailedScan(t *testing.T) {
	fastRetries(t)
	for _, k := range []int{1, 2, 4} {
		f := newFakeChain(t, 2*time.Hour) // 72,000 blocks
		token := hAddr(95)
		for _, b := range []uint64{100, 15_000, 25_000, 39_999, 40_000, 55_000} {
			f.transfer(token, zeroAddr, hAddr(96), b, "0x1")
		}
		const size = 10_000
		failFrom := uint64(1)
		if k > 1 {
			failFrom = uint64(k-1) * size
		}
		f.setLogsHook(func(addr string, from, _ uint64) string {
			if addr == token && from == failFrom {
				return "rate limit exceeded"
			}
			return ""
		})
		o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_LOG_CHUNK": "10000", "SCOUT_RPC_PARALLEL": "1"})
		var outer atomic.Int64
		ctx := context.WithValue(context.Background(), progressKey{}, progressFunc(func(_, _, _ uint64, _ int, _ uint64) { outer.Add(1) }))
		ctx, reached := withScanReached(ctx)
		var got []uint64
		err := o.rpc.getLogsChunked(ctx, token, []any{topicTransfer}, 1, f.latest, func(l rpcLog) { got = append(got, l.block()) })
		if err == nil {
			t.Fatalf("k=%d: the scan did not fail", k)
		}
		wantReached := failFrom - 1
		if k == 1 {
			wantReached = 0
		}
		if *reached != wantReached {
			t.Fatalf("k=%d: reached %d, want %d (the end of the range before the failed one)", k, *reached, wantReached)
		}
		for _, b := range got {
			if b > *reached {
				t.Fatalf("k=%d: log at block %d handed over, after the block reached %d", k, b, *reached)
			}
		}
		if n := outer.Load(); n != int64(k-1) {
			t.Fatalf("k=%d: the context's progress function was called %d time(s), want %d (once per range answered)", k, n, k-1)
		}
	}
}
