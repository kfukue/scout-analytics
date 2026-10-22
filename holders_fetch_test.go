package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Holder log reader test addresses: the token, its deployer, a buyer and a
// Pons curve. The v2/v3 pool and the v4 PoolManager are lqPool and lqPM
// (holders_lp_test.go).
var hfToken, hfDeployer, hfBuyer, hfCurve = hAddr(80), hAddr(81), hAddr(82), hAddr(83)

// hfTx is a 32-byte tx hash.
func hfTx(n int) string { return fmt.Sprintf("0x%064x", n) }

// hfLaunchTx is the launch (mint) transaction of every hfCall.
var hfLaunchTx = hfTx(1)

// hfCall is one call on a fake chain: entered entryAgo before the chain's
// head, its token minted to hfDeployer launchAge before the entry (in
// hfLaunchTx, known to the node).
type hfCall struct {
	f           *fakeChain
	o           *onchainSource
	entryBlock  uint64
	entryAt     time.Time
	launchBlock uint64
	postEnd     uint64 // the block holderPostWindow after the entry, by the fake's own rule
}

// newHFCall builds the chain and the source; chunk is SCOUT_RPC_LOG_CHUNK.
// The log cache is off, so every chunk is a request.
func newHFCall(t *testing.T, chainAge, entryAgo, launchAge time.Duration, chunk int) *hfCall {
	t.Helper()
	return newHFCallEnv(t, chainAge, entryAgo, launchAge, chunk, nil)
}

// newHFCallEnv is newHFCall with more SCOUT_* settings for the source.
func newHFCallEnv(t *testing.T, chainAge, entryAgo, launchAge time.Duration, chunk int, env map[string]string) *hfCall {
	t.Helper()
	f := newFakeChain(t, chainAge)
	c := &hfCall{f: f}
	c.entryBlock = f.latest - uint64(entryAgo/time.Second)*10
	c.entryAt = time.Unix(f.timeOf(c.entryBlock), 0)
	c.launchBlock = c.entryBlock - uint64(launchAge/time.Second)*10
	c.postEnd = f.blockAtTime(c.entryAt.Add(holderPostWindow))
	f.txs = map[string]fakeTx{hfLaunchTx: {from: hfDeployer, block: c.launchBlock}}
	f.transfer(hfToken, zeroAddr, hfDeployer, c.launchBlock, hfLaunchTx)
	settings := map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_LOG_CHUNK": fmt.Sprint(chunk)}
	for k, v := range env {
		settings[k] = v
	}
	c.o = testOnchain(t, f, settings)
	return c
}

// input is the call's fetch input with venues and v4 pool ids.
func (c *hfCall) input(venues []holderVenue, ids ...string) holderFetchInput {
	return holderFetchInput{Token: hfToken, EntryBlock: c.entryBlock, EntryAt: c.entryAt, Venues: venues, V4PoolIDs: ids}
}

func (c *hfCall) mustFetch(t *testing.T, in holderFetchInput) holderLogs {
	t.Helper()
	r, err := c.o.fetchHolderLogs(context.Background(), in)
	if err != nil {
		t.Fatalf("fetchHolderLogs: %v", err)
	}
	return r
}

// addLog adds a log built by the holders_lp_test.go helpers at block, in tx.
func (f *fakeChain) addLog(l rpcLog, block uint64, tx string) {
	f.logs = append(f.logs, fakeLog{addr: l.Address, topics: l.Topics, data: l.Data, block: block, tx: tx})
}

// counts returns a copy of the requests the chain answered, per method.
func (f *fakeChain) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for m, n := range f.count {
		out[m] = n
	}
	return out
}

// otherRequests: every request the chain answered except eth_getLogs.
func (f *fakeChain) otherRequests() int64 {
	n := int64(0)
	for m, c := range f.counts() {
		if m != "eth_getLogs" {
			n += int64(c)
		}
	}
	return n
}

// chunks: the eth_getLogs ranges of size s over [from, to] (they end on
// multiples of s).
func chunks(from, to, s uint64) int64 { return int64(to/s - from/s + 1) }

// launchSearchRequests: the eth_getLogs of a launch search that stops in the
// range holding block (the launch, or the entry when there is no mint): the
// ranges of the default SCOUT_RPC_HISTORY_CHUNK over [1, block].
func launchSearchRequests(block uint64) int64 { return chunks(1, block, defaultHistoryChunk) }

// checkRequestCounts: the result's counters equal what the chain answered.
func checkRequestCounts(t *testing.T, f *fakeChain, r holderLogs) {
	t.Helper()
	if got, want := r.LogRequests, int64(f.counts()["eth_getLogs"]); got != want {
		t.Errorf("LogRequests = %d, want %d (eth_getLogs the chain answered)", got, want)
	}
	if got, want := r.OtherRequests, f.otherRequests(); got != want {
		t.Errorf("OtherRequests = %d, want %d (other requests the chain answered: %v)", got, want, f.counts())
	}
}

func blocksOf[T any](items []T, block func(T) uint64) []uint64 {
	var out []uint64
	for _, it := range items {
		out = append(out, block(it))
	}
	return out
}

func transferBlocks(trs []tokenTransfer) []uint64 {
	return blocksOf(trs, func(t tokenTransfer) uint64 { return t.Block })
}

func liquidityBlocks(evs []liquidityEvent) []uint64 {
	return blocksOf(evs, func(e liquidityEvent) uint64 { return e.Block })
}

// The eth_getLogs count follows 1 + (1 + F) × chunks(launch, postEnd), and
// the launch, deployer and post-window end are found, for a few token ages
// and venue mixes.
func TestHoldersFetchRequestCounts(t *testing.T) {
	const chunk = 1_000_000
	var manyIDs []string
	for i := range 40 {
		manyIDs = append(manyIDs, fmt.Sprintf("0x%064x", 1000+i))
	}
	cases := []struct {
		name                string
		chainAge, launchAge time.Duration
		venues              []holderVenue
		ids                 []string
		filters             int64 // F: liquidity filters
	}{
		{"1h old, v2", 30 * 24 * time.Hour, time.Hour, []holderVenue{{lqPool, "v2"}}, nil, 1},
		{"5d old, v3 and a Pons curve", 30 * 24 * time.Hour, 5 * 24 * time.Hour,
			[]holderVenue{{lqPool, "v3"}, {hfCurve, "pons"}}, nil, 1},
		{"20d old, v2 and v4 with two ids", 40 * 24 * time.Hour, 20 * 24 * time.Hour,
			[]holderVenue{{lqPool, "v2"}, {lqPM, "v4"}}, []string{lqPoolID, lqOtherPoolID}, 2},
		{"60d old, v4 with 40 ids (two batches)", 80 * 24 * time.Hour, 60 * 24 * time.Hour,
			[]holderVenue{{lqPM, "v4"}}, manyIDs, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHFCall(t, tc.chainAge, 10*24*time.Hour, tc.launchAge, chunk)
			r := c.mustFetch(t, c.input(tc.venues, tc.ids...))
			if r.Status != holderFetchOK {
				t.Fatalf("status %v, want ok", r.Status)
			}
			if r.LaunchBlock != c.launchBlock || r.LaunchTx != hfLaunchTx {
				t.Errorf("launch block %d tx %s, want %d %s", r.LaunchBlock, r.LaunchTx, c.launchBlock, hfLaunchTx)
			}
			if r.Age != tc.launchAge {
				t.Errorf("age %v, want %v", r.Age, tc.launchAge)
			}
			if r.Deployer != hfDeployer || r.DeployerSource != deployerFromMintTx {
				t.Errorf("deployer %q from %q, want %q from %q", r.Deployer, r.DeployerSource, hfDeployer, deployerFromMintTx)
			}
			if r.PostEndBlock != c.postEnd {
				t.Errorf("post end block %d, want %d", r.PostEndBlock, c.postEnd)
			}
			if want := c.entryAt.Add(holderPostWindow).Unix(); r.PostEndAt.Unix() != want {
				t.Errorf("post end time %d, want %d", r.PostEndAt.Unix(), want)
			}
			launch := launchSearchRequests(c.launchBlock)
			want := launch + (1+tc.filters)*chunks(c.launchBlock, c.postEnd, chunk)
			if r.LogRequests != want {
				t.Errorf("LogRequests = %d, want %d + (1+%d) × chunks(%d, %d, %d) = %d",
					r.LogRequests, launch, tc.filters, c.launchBlock, c.postEnd, chunk, want)
			}
			checkRequestCounts(t, c.f, r)
			// The launch search: ranges of SCOUT_RPC_HISTORY_CHUNK from block 1 on
			// the token, mints only, up to the one holding the launch.
			q := c.f.logQueries()[0]
			wantTopics := fmt.Sprint([]any{topicTransfer, addrTopic(zeroAddr)})
			firstTo := min(defaultHistoryChunk-1, c.entryBlock)
			if q.addr != hfToken || fmt.Sprint(q.topics) != wantTopics || q.from != 1 || q.to != firstTo {
				t.Errorf("first query %s %v %d-%d, want %s %s 1-%d", q.addr, q.topics, q.from, q.to, hfToken, wantTopics, firstTo)
			}
			if got := transferBlocks(r.TransfersPre); !slices.Equal(got, []uint64{c.launchBlock}) {
				t.Errorf("pre transfers at %v, want the mint at %d", got, c.launchBlock)
			}
			if !r.LiquidityRead {
				t.Errorf("LiquidityRead false, want true")
			}
		})
	}
}

// serialRanges: one eth_getLogs range of a scan at a time. With parallel
// ranges, a sibling cancelled after another range was refused (or after the
// launch search stopped) is counted although it may never reach the node, so
// tests that compare the counters with what the chain answered use this.
var serialRanges = map[string]string{"SCOUT_RPC_PARALLEL": "1"}

// hfMintKey is the launch search's stream key (address + topics) in queries.
var hfMintKey = hfToken + fmt.Sprint([]any{topicTransfer, addrTopic(zeroAddr)})

// answeredRanges groups the queries a node with maxRange answered by stream
// (address + topics) and counts the refused ones.
func answeredRanges(qs []fakeLogQuery, maxRange uint64) (answered map[string][][2]uint64, refused int) {
	answered = map[string][][2]uint64{}
	for _, q := range qs {
		if q.to-q.from+1 > maxRange {
			refused++
			continue
		}
		k := q.addr + fmt.Sprint(q.topics)
		answered[k] = append(answered[k], [2]uint64{q.from, q.to})
	}
	return answered, refused
}

// rangeHolding returns the range in ranges that holds block b.
func rangeHolding(ranges [][2]uint64, b uint64) ([2]uint64, bool) {
	for _, r := range ranges {
		if r[0] <= b && b <= r[1] {
			return r, true
		}
	}
	return [2]uint64{}, false
}

// A node that refuses ranges over maxRange blocks: every range is split until
// it is answered, every refused attempt is counted, and nothing is lost. The
// launch search stops after the chunk holding the launch block.
func TestHoldersFetchSplitsRefusedRanges(t *testing.T) {
	const maxRange = 300_000
	c := newHFCallEnv(t, 20*24*time.Hour, 5*24*time.Hour, 2*24*time.Hour, 1_000_000, serialRanges)
	f := c.f
	f.maxRange = maxRange
	f.transfer(hfToken, hfDeployer, hfBuyer, c.launchBlock+1_234_567, hfTx(2))
	f.transfer(hfToken, hfBuyer, lqPool, c.postEnd-10, hfTx(3))
	f.addLog(v2MintLog(), c.launchBlock+5, hfTx(4))
	f.addLog(v2BurnLog(), c.entryBlock+700_001, hfTx(5))
	r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
	if r.Status != holderFetchOK || r.LaunchBlock != c.launchBlock {
		t.Fatalf("status %v launch %d, want ok %d", r.Status, r.LaunchBlock, c.launchBlock)
	}
	if got, want := transferBlocks(r.TransfersPre), []uint64{c.launchBlock, c.launchBlock + 1_234_567}; !slices.Equal(got, want) {
		t.Errorf("pre transfers at %v, want %v", got, want)
	}
	if got, want := transferBlocks(r.TransfersPost), []uint64{c.postEnd - 10}; !slices.Equal(got, want) {
		t.Errorf("post transfers at %v, want %v", got, want)
	}
	if got, want := liquidityBlocks(slices.Concat(r.LiquidityPre, r.LiquidityPost)), []uint64{c.launchBlock + 5, c.entryBlock + 700_001}; !slices.Equal(got, want) {
		t.Errorf("liquidity at %v, want %v", got, want)
	}
	checkRequestCounts(t, f, r)
	qs := f.logQueries()
	if r.LogRequests != int64(len(qs)) {
		t.Errorf("LogRequests = %d, want %d (every query, refused ones included)", r.LogRequests, len(qs))
	}
	// Each stream's answered ranges cover its span; some ranges were refused.
	answered, refused := answeredRanges(qs, maxRange)
	if refused == 0 {
		t.Errorf("no range was refused; the test does not test splitting")
	}
	// The launch search covers [1, the end of the chunk holding the launch].
	launchChunk, ok := rangeHolding(answered[hfMintKey], c.launchBlock)
	if !ok {
		t.Fatalf("no answered launch-search range holds the launch block %d: %v", c.launchBlock, answered[hfMintKey])
	}
	if launchChunk[1] >= c.entryBlock {
		t.Errorf("launch chunk %v reaches the entry block %d; the test does not test stopping early", launchChunk, c.entryBlock)
	}
	spans := map[string][2]uint64{
		hfMintKey: {1, launchChunk[1]},
		hfToken + fmt.Sprint([]any{topicTransfer}):                  {c.launchBlock, c.postEnd},
		lqPool + fmt.Sprint([]any{[]any{topicMintV2, topicBurnV2}}): {c.launchBlock, c.postEnd},
	}
	if len(answered) != len(spans) {
		t.Errorf("streams read: %d, want %d", len(answered), len(spans))
	}
	for k, span := range spans {
		if !covers(answered[k], span) {
			t.Errorf("stream %s: answered ranges do not cover %d-%d", k, span[0], span[1])
		}
	}
}

// covers reports whether the union of ranges is exactly [span[0], span[1]].
func covers(ranges [][2]uint64, span [2]uint64) bool {
	if len(ranges) == 0 {
		return false
	}
	rs := slices.Clone(ranges)
	slices.SortFunc(rs, func(a, b [2]uint64) int { return cmp.Compare(a[0], b[0]) })
	if rs[0][0] != span[0] {
		return false
	}
	end := rs[0][1]
	for _, r := range rs[1:] {
		if r[0] > end+1 {
			return false
		}
		end = max(end, r[1])
	}
	return end == span[1]
}

// Mints spread over several chunks on a node that refuses the single launch
// range: the search stops after the chunk holding the first kept mint (an
// earlier removed mint does not count), and the launch block and tx are the
// first mint's. The transfer stream still sees every mint.
func TestHoldersFetchLaunchSearchStops(t *testing.T) {
	const maxRange = 300_000
	setup := func(t *testing.T, env map[string]string) *hfCall {
		c := newHFCallEnv(t, 20*24*time.Hour, 5*24*time.Hour, 2*24*time.Hour, 1_000_000, env)
		c.f.maxRange = maxRange
		l := c.launchBlock
		c.f.logs = append(c.f.logs, fakeLog{addr: hfToken, topics: []string{topicTransfer, addrTopic(zeroAddr), addrTopic(hfBuyer)},
			data: ret(wInt(1)), block: l - 50, tx: hfTx(19), removed: true})
		for i, b := range []uint64{l + 3, l + 400_000, l + 900_000, c.entryBlock - 10} {
			c.f.transfer(hfToken, zeroAddr, hfBuyer, b, hfTx(20+i))
		}
		return c
	}
	check := func(t *testing.T, c *hfCall, r holderLogs) {
		t.Helper()
		if r.Status != holderFetchOK || r.LaunchBlock != c.launchBlock || r.LaunchTx != hfLaunchTx {
			t.Fatalf("status %v launch %d %s, want ok %d %s", r.Status, r.LaunchBlock, r.LaunchTx, c.launchBlock, hfLaunchTx)
		}
		l := c.launchBlock
		if got, want := transferBlocks(r.TransfersPre), []uint64{l, l + 3, l + 400_000, l + 900_000, c.entryBlock - 10}; !slices.Equal(got, want) {
			t.Errorf("pre transfers at %v, want %v", got, want)
		}
	}
	t.Run("one range at a time", func(t *testing.T) {
		c := setup(t, serialRanges)
		r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
		check(t, c, r)
		checkRequestCounts(t, c.f, r)
		qs := c.f.logQueries()
		answered, refused := answeredRanges(qs, maxRange)
		if refused == 0 {
			t.Fatalf("no range was refused; the test does not test the split search")
		}
		launchChunk, ok := rangeHolding(answered[hfMintKey], c.launchBlock)
		if !ok {
			t.Fatalf("no answered launch-search range holds the launch block %d: %v", c.launchBlock, answered[hfMintKey])
		}
		if launchChunk[1] >= c.launchBlock+400_000 {
			t.Fatalf("launch chunk %v holds the next mint too; the test does not test stopping", launchChunk)
		}
		for _, q := range qs {
			if q.addr+fmt.Sprint(q.topics) == hfMintKey && q.from > launchChunk[1] {
				t.Errorf("launch-search query %d-%d starts after the launch chunk %v", q.from, q.to, launchChunk)
			}
		}
		if !covers(answered[hfMintKey], [2]uint64{1, launchChunk[1]}) {
			t.Errorf("launch-search ranges %v do not cover 1-%d", answered[hfMintKey], launchChunk[1])
		}
	})
	t.Run("parallel ranges", func(t *testing.T) {
		// Siblings of the launch chunk may be answered and delivered too:
		// the earliest mint still wins and the search ends without error.
		// A history cap equal to SCOUT_RPC_LOG_CHUNK (1M here) keeps the
		// search parallel; a cap above it would make it one range at a time.
		c := setup(t, map[string]string{"SCOUT_RPC_PARALLEL": "8", "SCOUT_RPC_HISTORY_CHUNK": "1000000"})
		check(t, c, c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}})))
	})
}

// Cancelling the caller's context during the split launch search returns
// context.Canceled (the search's own stop is not taken for success), and no
// stream is read.
func TestHoldersFetchLaunchSearchCancel(t *testing.T) {
	const maxRange = 300_000
	cases := []struct {
		name        string
		cancel      func(c *hfCall, n int, from, to uint64) bool // n: launch-search queries so far
		holdsLaunch bool                                         // the cancelling range holds the launch block
	}{
		{"before the launch chunk", func(_ *hfCall, n int, _, _ uint64) bool { return n == 3 }, false},
		{"when the launch chunk is asked", func(c *hfCall, _ int, from, to uint64) bool {
			return from <= c.launchBlock && c.launchBlock <= to && to-from+1 <= maxRange
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHFCallEnv(t, 20*24*time.Hour, 5*24*time.Hour, 2*24*time.Hour, 1_000_000, serialRanges)
			c.f.maxRange = maxRange
			c.f.transfer(hfToken, zeroAddr, hfBuyer, c.launchBlock+900_000, hfTx(2))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			n := 0
			var at [2]uint64 // the range whose request cancelled
			c.f.setLogsHook(func(addr string, from, to uint64) string {
				mu.Lock()
				defer mu.Unlock()
				n++
				if addr == hfToken && tc.cancel(c, n, from, to) && at == ([2]uint64{}) {
					at = [2]uint64{from, to}
					cancel()
				}
				return ""
			})
			_, err := c.o.fetchHolderLogs(ctx, c.input([]holderVenue{{lqPool, "v2"}}))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err %v, want context.Canceled", err)
			}
			mu.Lock()
			holds := at[0] <= c.launchBlock && c.launchBlock <= at[1]
			mu.Unlock()
			if holds != tc.holdsLaunch {
				t.Errorf("cancelled at range %v: holds the launch block %d = %v, want %v", at, c.launchBlock, holds, tc.holdsLaunch)
			}
			for _, q := range c.f.logQueries() {
				if k := q.addr + fmt.Sprint(q.topics); k != hfMintKey {
					t.Errorf("query %s %d-%d after the cancel, want only the launch search", k, q.from, q.to)
				}
			}
		})
	}
}

// Logs removed by a reorg and repeated logs are dropped and counted, per
// stream; a liquidity log that does not decode is skipped and counted; a
// removed mint does not define the launch.
func TestHoldersFetchDropsRemovedAndDuplicateLogs(t *testing.T) {
	c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, 24*time.Hour, 1_000_000)
	f, l := c.f, c.launchBlock
	// An earlier mint that a reorg removed.
	f.logs = append(f.logs, fakeLog{addr: hfToken, topics: []string{topicTransfer, addrTopic(zeroAddr), addrTopic(hfBuyer)},
		data: ret(wInt(1)), block: l - 100, tx: hfTx(9), removed: true})
	xf := func(block uint64, tx string, removed bool, idx uint64) fakeLog {
		return fakeLog{addr: hfToken, topics: []string{topicTransfer, addrTopic(hfDeployer), addrTopic(hfBuyer)},
			data: ret(wInt(1)), block: block, tx: tx, removed: removed, index: idx, hasIndex: idx > 0}
	}
	f.logs = append(f.logs, xf(l+10, hfTx(2), false, 500), xf(l+10, hfTx(2), false, 500), xf(l+20, hfTx(3), true, 0))
	mint := v2MintLog()
	burn := v2BurnLog()
	badMint := lqLog(lqPool, topicMintV2, []string{addrTopic(lqRouter)}, wInt(1)) // one data word, want two
	f.logs = append(f.logs,
		fakeLog{addr: lqPool, topics: mint.Topics, data: mint.Data, block: l + 5, tx: hfTx(4), index: 600, hasIndex: true},
		fakeLog{addr: lqPool, topics: mint.Topics, data: mint.Data, block: l + 5, tx: hfTx(4), index: 600, hasIndex: true},
		fakeLog{addr: lqPool, topics: burn.Topics, data: burn.Data, block: l + 30, tx: hfTx(5), removed: true},
		fakeLog{addr: lqPool, topics: badMint.Topics, data: badMint.Data, block: l + 40, tx: hfTx(6)})
	r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
	if r.Status != holderFetchOK || r.LaunchBlock != l || r.LaunchTx != hfLaunchTx {
		t.Fatalf("status %v launch %d %s, want ok %d %s", r.Status, r.LaunchBlock, r.LaunchTx, l, hfLaunchTx)
	}
	if want := (logStreamCounts{Logs: 4, Removed: 1, Duplicates: 1}); r.TransferCounts != want {
		t.Errorf("transfer counts %+v, want %+v", r.TransferCounts, want)
	}
	if got, want := transferBlocks(r.TransfersPre), []uint64{l, l + 10}; !slices.Equal(got, want) {
		t.Errorf("pre transfers at %v, want %v", got, want)
	}
	if want := (logStreamCounts{Logs: 4, Removed: 1, Duplicates: 1, Bad: 1}); r.LiquidityCounts != want {
		t.Errorf("liquidity counts %+v, want %+v", r.LiquidityCounts, want)
	}
	if len(r.LiquidityPre) != 1 || r.LiquidityPre[0].Block != l+5 || r.LiquidityPre[0].Kind != lpAdd {
		t.Errorf("pre liquidity %+v, want one add at %d", r.LiquidityPre, l+5)
	}
}

// cleanLogs on its own: removed first, then repeats; a log whose block does
// not parse is kept for the decoder to reject.
func TestHoldersFetchCleanLogs(t *testing.T) {
	a := rpcLog{BlockNumber: "0x10", LogIndex: "0x1", TxHash: "0xa"}
	aRemoved := a
	aRemoved.Removed = true
	b := rpcLog{BlockNumber: "0x10", LogIndex: "0x2"}
	bad := rpcLog{BlockNumber: "zz", LogIndex: "0x1"}
	var c logStreamCounts
	// The removed copy comes first: the canonical one is still kept.
	got := cleanLogs([]rpcLog{aRemoved, a, b, a, bad, bad}, &c)
	if want := (logStreamCounts{Logs: 6, Removed: 1, Duplicates: 1}); c != want {
		t.Errorf("counts %+v, want %+v", c, want)
	}
	if want := []rpcLog{a, b, bad, bad}; !slices.EqualFunc(got, want, func(x, y rpcLog) bool {
		return x.BlockNumber == y.BlockNumber && x.LogIndex == y.LogIndex && x.Removed == y.Removed
	}) {
		t.Errorf("kept %+v, want %+v", got, want)
	}
}

// No mint at or before the entry block: a result, after one eth_getLogs and
// nothing else.
func TestHoldersFetchNoMint(t *testing.T) {
	f := newFakeChain(t, 20*24*time.Hour)
	entry := f.latest - 5*864_000
	f.transfer(hfToken, hfDeployer, hfBuyer, entry-50, hfTx(2))
	f.transfer(hfToken, zeroAddr, hfDeployer, entry+1, hfTx(3)) // a mint after the entry does not count
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0"})
	in := holderFetchInput{Token: hfToken, EntryBlock: entry, EntryAt: time.Unix(f.timeOf(entry), 0), Venues: []holderVenue{{lqPool, "v2"}}}
	r, err := o.fetchHolderLogs(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != holderFetchNoMint || r.LaunchBlock != 0 {
		t.Errorf("status %v launch %d, want no_mint 0", r.Status, r.LaunchBlock)
	}
	if want := launchSearchRequests(entry); r.LogRequests != want || r.OtherRequests != 0 {
		t.Errorf("requests: %d log, %d other; want %d (the launch search over 1-%d) and 0", r.LogRequests, r.OtherRequests, want, entry)
	}
	checkRequestCounts(t, f, r)
}

// A token older than the 90-day cap at the entry is capped after the launch
// block's time and reads no stream; at exactly 90 days it is read in full.
func TestHoldersFetchLookbackCap(t *testing.T) {
	cases := []struct {
		name      string
		launchAge time.Duration
		want      holderFetchStatus
	}{
		{"90 days: read", holderLookbackCap, holderFetchOK},
		{"90 days and 1 s: capped", holderLookbackCap + time.Second, holderFetchCapped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHFCall(t, 100*24*time.Hour, 5*24*time.Hour, tc.launchAge, 1_000_000)
			r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
			if r.Status != tc.want || r.Age != tc.launchAge {
				t.Fatalf("status %v age %v, want %v %v", r.Status, r.Age, tc.want, tc.launchAge)
			}
			checkRequestCounts(t, c.f, r)
			if tc.want != holderFetchCapped {
				return
			}
			if want := launchSearchRequests(c.launchBlock); r.LogRequests != want || r.OtherRequests != 1 {
				t.Errorf("requests: %d log, %d other; want %d (launch search) and 1 (launch block time)", r.LogRequests, r.OtherRequests, want)
			}
			if r.Deployer != "" || r.PostEndBlock != 0 || r.TransfersPre != nil || r.Venues != nil {
				t.Errorf("capped result read on: %+v", r)
			}
		})
	}
}

// The v4 PoolManager is filtered on the token's pool ids: another pool's
// ModifyLiquidity is not returned. Without pool ids nothing is read there.
func TestHoldersFetchV4PoolFilter(t *testing.T) {
	setup := func(t *testing.T) *hfCall {
		c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
		c.f.addLog(v4ModifyLog(lqPoolID, big.NewInt(1000)), c.launchBlock+5, hfTx(2))
		c.f.addLog(v4ModifyLog(lqOtherPoolID, big.NewInt(-1000)), c.launchBlock+6, hfTx(3))
		return c
	}
	t.Run("with the pool id", func(t *testing.T) {
		c := setup(t)
		r := c.mustFetch(t, c.input([]holderVenue{{lqPM, "v4"}}, "0x"+strings.ToUpper(lqPoolID[2:])))
		if len(r.LiquidityPre) != 1 || r.LiquidityPre[0].PoolID != lqPoolID || r.LiquidityCounts.Logs != 1 {
			t.Fatalf("liquidity %+v (counts %+v), want only pool %s", r.LiquidityPre, r.LiquidityCounts, lqPoolID)
		}
		for _, q := range c.f.logQueries() {
			if q.addr == lqPM && (len(q.topics) != 2 || q.topics[1] != lqPoolID) {
				t.Errorf("PoolManager query topics %v, want [ModifyLiquidity %s]", q.topics, lqPoolID)
			}
		}
		if !r.LiquidityRead || len(r.Venues) != 1 || !r.Venues[0].Read {
			t.Errorf("venues %+v read %v, want the v4 venue read", r.Venues, r.LiquidityRead)
		}
	})
	t.Run("without pool ids", func(t *testing.T) {
		c := setup(t)
		r := c.mustFetch(t, c.input([]holderVenue{{lqPM, "v4"}}))
		if n := c.f.queriesTo(lqPM); n != 0 {
			t.Errorf("%d queries to the PoolManager, want 0", n)
		}
		if r.Status != holderFetchOK || r.LiquidityRead || len(r.Venues) != 1 || r.Venues[0].Read || r.LiquidityPre != nil {
			t.Errorf("status %v venues %+v read %v liquidity %+v; want ok, the v4 venue not read", r.Status, r.Venues, r.LiquidityRead, r.LiquidityPre)
		}
	})
}

// A Pons curve has no liquidity stream: the curve is never asked; the token's
// transfers are still read.
func TestHoldersFetchPonsNoLiquidity(t *testing.T) {
	c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
	c.f.transfer(hfToken, hfCurve, hfBuyer, c.launchBlock+3, hfTx(2))
	c.f.addLog(lqLog(hfCurve, topicMintV2, []string{addrTopic(lqRouter)}, wInt(1), wInt(2)), c.launchBlock+4, hfTx(3))
	r := c.mustFetch(t, c.input([]holderVenue{{hfCurve, "pons"}}))
	if n := c.f.queriesTo(hfCurve); n != 0 {
		t.Errorf("%d queries to the curve, want 0", n)
	}
	if r.Status != holderFetchOK || r.LiquidityRead || r.Venues[0].Read || r.LiquidityCounts != (logStreamCounts{}) {
		t.Errorf("status %v read %v venues %+v counts %+v; want ok and no liquidity read", r.Status, r.LiquidityRead, r.Venues, r.LiquidityCounts)
	}
	if got, want := transferBlocks(r.TransfersPre), []uint64{c.launchBlock, c.launchBlock + 3}; !slices.Equal(got, want) {
		t.Errorf("pre transfers at %v, want %v", got, want)
	}
	want := launchSearchRequests(c.launchBlock) + chunks(c.launchBlock, c.postEnd, 1_000_000)
	if r.LogRequests != want {
		t.Errorf("LogRequests = %d, want %d (launch search + transfers only)", r.LogRequests, want)
	}
}

// The deployer: the launch tx's sender; when the node answers null or an
// error, the launch block's transactions; otherwise unknown, with source
// lookup_failed when the block read failed with a node error (whatever the tx
// lookup answered), else "" (not found). The call still succeeds.
func TestHoldersFetchDeployer(t *testing.T) {
	const nodeErr = "internal error"
	cases := []struct {
		name        string
		hashNull    bool
		hashErr     bool // eth_getTransactionByHash answers a node error
		blockErr    bool // the full block read answers a node error
		txElsewhere bool // the launch tx is not in the launch block either
		deployer    string
		source      string
		fullBlocks  int
	}{
		{"from the tx", false, false, false, false, hfDeployer, deployerFromMintTx, 0},
		{"tx null: from the block", true, false, false, false, hfDeployer, deployerFromMintBlock, 1},
		{"tx error: from the block", false, true, false, false, hfDeployer, deployerFromMintBlock, 1},
		{"tx null, not in the block: not found", true, false, false, true, "", "", 1},
		{"tx error, not in the block: not found", false, true, false, true, "", "", 1},
		{"tx null, block error: lookup failed", true, false, true, false, "", deployerLookupFailed, 1},
		{"both lookups error: lookup failed", false, true, true, false, "", deployerLookupFailed, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
			c.f.txByHashNull = tc.hashNull
			if tc.hashErr {
				c.f.txByHashErr = nodeErr
			}
			if tc.blockErr {
				c.f.fullBlockErr = nodeErr
			}
			c.f.txs[hfTx(7)] = fakeTx{from: hfBuyer, block: c.launchBlock} // another tx in the launch block
			if tc.txElsewhere {
				c.f.txs[hfLaunchTx] = fakeTx{from: hfDeployer, block: c.launchBlock + 1}
			}
			r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
			if r.Status != holderFetchOK {
				t.Fatalf("status %v, want ok", r.Status)
			}
			if r.Deployer != tc.deployer || r.DeployerSource != tc.source {
				t.Errorf("deployer %q from %q, want %q from %q", r.Deployer, r.DeployerSource, tc.deployer, tc.source)
			}
			if got := c.f.counts()["eth_getTransactionByHash"]; got != 1 {
				t.Errorf("%d eth_getTransactionByHash, want 1", got)
			}
			if got := c.f.fullBlockReads(); got != tc.fullBlocks {
				t.Errorf("%d full block reads, want %d", got, tc.fullBlocks)
			}
			checkRequestCounts(t, c.f, r)
		})
	}
}

// Pre is at or before the entry block, post after it and at or before the
// post-window end; nothing later is read.
func TestHoldersFetchPrePostSplit(t *testing.T) {
	c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
	e, p := c.entryBlock, c.postEnd
	for i, b := range []uint64{e, e + 1, p, p + 1} {
		c.f.transfer(hfToken, hfDeployer, hfBuyer, b, hfTx(10+i))
		c.f.addLog(v2MintLog(), b, hfTx(10+i))
	}
	r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
	if r.PostEndBlock != p {
		t.Fatalf("post end %d, want %d", r.PostEndBlock, p)
	}
	for _, s := range []struct {
		name      string
		got, want []uint64
	}{
		{"transfers pre", transferBlocks(r.TransfersPre), []uint64{c.launchBlock, e}},
		{"transfers post", transferBlocks(r.TransfersPost), []uint64{e + 1, p}},
		{"liquidity pre", liquidityBlocks(r.LiquidityPre), []uint64{e}},
		{"liquidity post", liquidityBlocks(r.LiquidityPost), []uint64{e + 1, p}},
	} {
		if !slices.Equal(s.got, s.want) {
			t.Errorf("%s at %v, want %v", s.name, s.got, s.want)
		}
	}
	for _, q := range c.f.logQueries()[1:] {
		if q.to > p {
			t.Errorf("query %s %d-%d reads past the post-window end %d", q.addr, q.from, q.to, p)
		}
	}
}

// splitAtEntry on its own, at the exact boundaries.
func TestHoldersFetchSplitAtEntry(t *testing.T) {
	pre, post := splitAtEntry([]uint64{1, 9, 10, 11, 19, 20, 21}, func(b uint64) uint64 { return b }, 10, 20)
	if !slices.Equal(pre, []uint64{1, 9, 10}) || !slices.Equal(post, []uint64{11, 19, 20}) {
		t.Errorf("pre %v post %v, want [1 9 10] [11 19 20]", pre, post)
	}
}

// A Transfer log that does not decode makes the call's result
// "bad_transfer_logs"; the liquidity stream is not read.
func TestHoldersFetchBadTransferLog(t *testing.T) {
	c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
	c.f.logs = append(c.f.logs, fakeLog{addr: hfToken, block: c.launchBlock + 5, tx: hfTx(2),
		topics: []string{topicTransfer, addrTopic(hfDeployer), addrTopic(hfBuyer), "0x" + wInt(7)}}) // an ERC-721 Transfer
	r := c.mustFetch(t, c.input([]holderVenue{{lqPool, "v2"}}))
	if r.Status != holderFetchBadTransfers || !errors.Is(r.BadTransfer, errBadTransferLog) {
		t.Fatalf("status %v bad %v, want bad_transfer_logs wrapping errBadTransferLog", r.Status, r.BadTransfer)
	}
	if r.TransferCounts.Bad != 1 || r.TransfersPre != nil || r.TransfersPost != nil {
		t.Errorf("counts %+v pre %d post %d, want 1 bad and no transfers", r.TransferCounts, len(r.TransfersPre), len(r.TransfersPost))
	}
	if n := c.f.queriesTo(lqPool); n != 0 || r.LiquidityRead {
		t.Errorf("%d pool queries (LiquidityRead %v), want none", n, r.LiquidityRead)
	}
	checkRequestCounts(t, c.f, r)
}

// Entry block 0 and bad input fail before any node request.
func TestHoldersFetchRejectsBeforeAnyRequest(t *testing.T) {
	f := newFakeChain(t, 20*24*time.Hour)
	o := testOnchain(t, f, nil)
	good := holderFetchInput{Token: hfToken, EntryBlock: 1000, EntryAt: time.Unix(f.timeOf(1000), 0), Venues: []holderVenue{{lqPool, "v2"}}}
	cases := []struct {
		name    string
		edit    func(*holderFetchInput)
		unknown bool // errUnknownEntryBlock
	}{
		{"entry block 0", func(in *holderFetchInput) { in.EntryBlock = 0 }, true},
		{"token not an address", func(in *holderFetchInput) { in.Token = "0x12" }, false},
		{"no entry time", func(in *holderFetchInput) { in.EntryAt = time.Time{} }, false},
		{"unknown venue kind", func(in *holderFetchInput) { in.Venues = []holderVenue{{lqPool, "v5"}} }, false},
		{"venue not an address", func(in *holderFetchInput) { in.Venues = []holderVenue{{"pool", "v2"}} }, false},
		{"v4 pool id not 32 bytes", func(in *holderFetchInput) { in.V4PoolIDs = []string{lqPoolID, "0xabcd"} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := good
			tc.edit(&in)
			r, err := o.fetchHolderLogs(context.Background(), in)
			if err == nil || errors.Is(err, errUnknownEntryBlock) != tc.unknown {
				t.Errorf("err %v, want an error (errUnknownEntryBlock: %v)", err, tc.unknown)
			}
			if n := f.counts(); len(n) != 0 || r.LogRequests != 0 || r.OtherRequests != 0 {
				t.Errorf("node requests %v (counted %d/%d), want none", n, r.LogRequests, r.OtherRequests)
			}
		})
	}
}

// Cancelling the context stops the read promptly with ctx.Err().
func TestHoldersFetchCancel(t *testing.T) {
	c := newHFCall(t, 20*24*time.Hour, 5*24*time.Hour, time.Hour, 1_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.f.setLogsHook(func(string, uint64, uint64) string {
		cancel()
		return ""
	})
	start := time.Now()
	_, err := c.o.fetchHolderLogs(ctx, c.input([]holderVenue{{lqPool, "v2"}}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v after the cancel, want prompt", took)
	}
	total := 0
	for _, n := range c.f.counts() {
		total += n
	}
	if total > 1 {
		t.Errorf("node requests after the cancel: %v, want at most the one being answered", c.f.counts())
	}
}

// holderV4PoolIDs on the fake chain: the token's pools from the PoolManager's
// Initialize events (token as currency0 or currency1, any case of letters),
// split by Initialize block: a pool initialised in the entry block is a pre
// id, one initialised after it is in all only. 2 searches in
// SCOUT_RPC_HISTORY_CHUNK ranges up to the head (2 × the ranges eth_getLogs,
// what holderV4SearchRequests counts) for a state whose PoolManager is a
// venue at some block, none for the others.
func TestHolderV4PoolIDs(t *testing.T) {
	low := "0x0000000000000000000000000000000000000001" // sorts below hfToken: the token is currency1
	idOld, idAtEntry, idLate := "0x01"+strings.Repeat("00", 31), "0x02"+strings.Repeat("00", 31), "0x03"+strings.Repeat("00", 31)
	idOther := "0x04" + strings.Repeat("00", 31)
	cases := []struct {
		name     string
		st       func(eb uint64) onchainState
		wantPre  []string
		wantAll  []string
		requests int // eth_getLogs
	}{
		{name: "plain v4", st: func(eb uint64) onchainState { return onchainState{Kind: "v4", Pool: tPM, EntryBlock: eb} },
			wantPre: []string{idOld, idAtEntry}, wantAll: []string{idOld, idAtEntry, idLate}, requests: 2},
		{name: "plain v4, state PoolID is the late pool", st: func(eb uint64) onchainState {
			return onchainState{Kind: "v4", Pool: tPM, PoolID: idLate, EntryBlock: eb}
		}, wantPre: []string{idOld, idAtEntry}, wantAll: []string{idOld, idAtEntry, idLate}, requests: 2},
		{name: "Pons graduated after the call", st: func(eb uint64) onchainState {
			return onchainState{Kind: "pons", Pool: hfCurve, Curve: hfCurve, PonsDone: eb + 50, PonsGrad: eb + 50, EntryBlock: eb}
		}, wantPre: []string{idOld, idAtEntry}, wantAll: []string{idOld, idAtEntry, idLate}, requests: 2},
		{name: "v2", st: func(eb uint64) onchainState { return onchainState{Kind: "v2", Pool: lqPool, EntryBlock: eb} }},
		{name: "v3", st: func(eb uint64) onchainState { return onchainState{Kind: "v3", Pool: lqPool, EntryBlock: eb} }},
		{name: "Pons on the curve", st: func(eb uint64) onchainState {
			return onchainState{Kind: "pons", Pool: hfCurve, Curve: hfCurve, EntryBlock: eb}
		}},
		{name: "Pons closed, pool not found yet", st: func(eb uint64) onchainState {
			return onchainState{Kind: "pons", Pool: hfCurve, Curve: hfCurve, PonsDone: eb - 10, EntryBlock: eb}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour)
			eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
			f.initV4(idOld, hfToken, tWETH, zeroAddr, 3000, eb-100_000) // token = currency0
			f.initV4(idAtEntry, low, hfToken, zeroAddr, 500, eb)        // token = currency1, in the entry block
			f.initV4(idLate, hfToken, tUSDG, zeroAddr, 100, eb+50)      // after the call
			f.initV4(idOther, tWETH, tUSDG, zeroAddr, 500, eb-10)       // another token's pool
			o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0"})
			upper := "0x" + strings.ToUpper(hfToken[2:])
			pre, all, err := o.holderV4PoolIDs(context.Background(), upper, tc.st(eb), f.latest)
			if err != nil {
				t.Fatalf("holderV4PoolIDs: %v", err)
			}
			if !slices.Equal(pre, tc.wantPre) || !slices.Equal(all, tc.wantAll) {
				t.Errorf("got pre %v all %v, want pre %v all %v", pre, all, tc.wantPre, tc.wantAll)
			}
			counts := f.counts()
			// requests: Initialize searches, each in ranges of SCOUT_RPC_HISTORY_CHUNK.
			if want := tc.requests * len(historySpans(1, f.latest, defaultHistoryChunk)); counts["eth_getLogs"] != want {
				t.Errorf("got %d eth_getLogs, want %d (requests %v)", counts["eth_getLogs"], want, counts)
			}
			// The dry run's count of the same search (holderV4SearchRequests) is
			// what the reader asked.
			if tc.requests > 0 {
				if est := holderV4SearchRequests(f.latest, defaultHistoryChunk); int64(counts["eth_getLogs"]) != est {
					t.Errorf("reader asked %d eth_getLogs up to head %d, the estimate counts %d", counts["eth_getLogs"], f.latest, est)
				}
			}
			if n := f.otherRequests(); n != 0 {
				t.Errorf("got %d other requests, want 0 (requests %v)", n, counts)
			}
			if tc.requests > 0 {
				checkInitLookups(t, f, hfToken, 1, f.latest)
			}
		})
	}
}

// splitV4PoolIDs: pre holds the pools initialised at or before the entry
// block, all every pool; both lowercase, sorted and deduplicated.
func TestSplitV4PoolIDs(t *testing.T) {
	a, b, c := "0x"+strings.Repeat("aa", 32), "0x"+strings.Repeat("bb", 32), "0x"+strings.Repeat("cc", 32)
	pools := []v4Pool{{id: c, block: 101}, {id: "0x" + strings.ToUpper(b[2:]), block: 100}, {id: a, block: 5}, {id: a, block: 5}}
	pre, all := splitV4PoolIDs(pools, 100)
	if want := []string{a, b}; !slices.Equal(pre, want) {
		t.Errorf("pre: got %v, want %v", pre, want)
	}
	if want := []string{a, b, c}; !slices.Equal(all, want) {
		t.Errorf("all: got %v, want %v", all, want)
	}
	if pre, all := splitV4PoolIDs(nil, 100); len(pre) != 0 || len(all) != 0 {
		t.Errorf("no pools: got pre %v all %v, want none", pre, all)
	}
}
