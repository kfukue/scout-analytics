package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SCOUT_RPC_HISTORY_CHUNK: the whole-history searches (v4 pools, Chainlink
// proxy switches, the USD-pool transfer sweep, the holder launch search) ask
// for at most that many blocks per eth_getLogs, one range at a time, and find
// the same things in the same order as one range over the whole span.

// historySpans: the ranges a scan with ceiling size asks over [from, to]
// when nothing is refused (they end on multiples of size).
func historySpans(from, to, size uint64) [][2]uint64 {
	var out [][2]uint64
	for b := from; b <= to; {
		end := min(b-b%size+size-1, to)
		out = append(out, [2]uint64{b, end})
		b = end + 1
	}
	return out
}

// inflightProbe records the eth_getLogs a fake chain is asked for and the
// most of them in flight at once (each held for hold, so overlaps show).
type inflightProbe struct {
	rl       rangeLog
	now, top atomic.Int64
	hold     time.Duration
}

func probeLogs(f *fakeChain, hold time.Duration) *inflightProbe {
	p := &inflightProbe{hold: hold}
	f.setLogsHook(func(addr string, from, to uint64) string {
		n := p.now.Add(1)
		for {
			top := p.top.Load()
			if n <= top || p.top.CompareAndSwap(top, n) {
				break
			}
		}
		p.rl.add(rangeAsk{addr: addr, from: from, to: to})
		time.Sleep(p.hold)
		p.now.Add(-1)
		return ""
	})
	return p
}

// reset forgets the asks and the in-flight record.
func (p *inflightProbe) reset() {
	p.rl.mu.Lock()
	p.rl.asks = nil
	p.rl.mu.Unlock()
	p.top.Store(0)
}

// checkCapped: every ask is at most limit blocks, and there was one.
func checkCapped(t *testing.T, asks []rangeAsk, limit uint64) {
	t.Helper()
	if len(asks) == 0 {
		t.Fatalf("no eth_getLogs asked; the test does not test the cap")
	}
	for _, a := range asks {
		if n := a.to - a.from + 1; n > limit {
			t.Fatalf("asked for blocks %d-%d (%d blocks), above the cap of %d", a.from, a.to, n, limit)
		}
	}
}

// historyCapCases: the settings the search tests run with: the default cap,
// a smaller one, and none (one range for the whole span, as before).
var historyCapCases = []struct {
	name  string
	value string // SCOUT_RPC_HISTORY_CHUNK
	limit uint64 // 0 = no cap
}{
	{"default cap", "", defaultHistoryChunk},
	{"1M cap", "1000000", 1_000_000},
	{"no cap", "0", 0},
}

func TestLoadHistoryChunkSetting(t *testing.T) {
	for _, c := range []struct {
		value   string
		unset   bool
		want    uint64
		wantErr bool
	}{
		{unset: true, want: 2_000_000},
		{value: "", want: 2_000_000},
		{value: "0", want: 0},
		{value: "500000", want: 500_000},
		{value: "200", want: 200}, // the smallest range
		{value: "1", wantErr: true},
		{value: "199", wantErr: true},
		{value: "-5", wantErr: true},
		{value: "x", wantErr: true},
		{value: "1.5", wantErr: true},
	} {
		t.Run(fmt.Sprintf("%q unset=%v", c.value, c.unset), func(t *testing.T) {
			t.Setenv("SCOUT_RPC_HISTORY_CHUNK", c.value)
			if c.unset {
				os.Unsetenv("SCOUT_RPC_HISTORY_CHUNK") // t.Setenv restores it afterwards
			}
			oc, err := loadOnchainConfig()
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "SCOUT_RPC_HISTORY_CHUNK") {
					t.Fatalf("SCOUT_RPC_HISTORY_CHUNK=%q: got err %v, want an error naming the setting", c.value, err)
				}
				return
			}
			if err != nil || oc.HistoryChunk != c.want {
				t.Fatalf("SCOUT_RPC_HISTORY_CHUNK=%q: got %d %v, want %d", c.value, oc.HistoryChunk, err, c.want)
			}
			if got := newRPCClient(oc).histChunk; got != c.want {
				t.Fatalf("SCOUT_RPC_HISTORY_CHUNK=%q: client cap %d, want %d", c.value, got, c.want)
			}
		})
	}
}

func TestScanLimits(t *testing.T) {
	for _, c := range []struct {
		name                string
		maxChunk, histChunk uint64
		parallel            int
		ceiling             uint64
		wantCeiling         uint64
		wantPar             int
	}{
		{"normal scan", 200_000, 2_000_000, 8, 200_000, 200_000, 8},
		{"normal scan, no ceiling given", 200_000, 2_000_000, 8, 0, 200_000, 8},
		{"whole history, capped, one at a time", 200_000, 2_000_000, 8, 94_867_688, 2_000_000, 1},
		{"Pons search, capped", 200_000, 2_000_000, 8, ponsSearchChunk, 2_000_000, 1},
		{"Pons search, a cap above it", 200_000, 8_000_000, 8, ponsSearchChunk, ponsSearchChunk, 1},
		// No cap (0): exactly as before the cap, ceiling kept and
		// SCOUT_RPC_PARALLEL ranges at once.
		{"whole history, no cap: parallel as before", 200_000, 0, 8, 94_867_688, 94_867_688, 8},
		{"Pons search, no cap: parallel as before", 200_000, 0, 8, ponsSearchChunk, ponsSearchChunk, 8},
		{"normal scan, no cap", 200_000, 0, 8, 200_000, 200_000, 8},
		{"normal scan, no cap, no ceiling given", 5_000_000, 0, 8, 0, 5_000_000, 8},
		{"log chunk above the cap: bounded by it", 5_000_000, 2_000_000, 8, 0, 2_000_000, 8},
		{"log chunk above the cap, whole history", 5_000_000, 2_000_000, 8, 94_867_688, 2_000_000, 8},
		{"log chunk equal to the cap, whole history", 2_000_000, 2_000_000, 8, 94_867_688, 2_000_000, 8},
		{"parallel unset", 200_000, 2_000_000, 0, 200_000, 200_000, 1},
		{"parallel unset, no cap", 200_000, 0, 0, 94_867_688, 94_867_688, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &rpcClient{maxChunk: c.maxChunk, histChunk: c.histChunk, parallel: c.parallel}
			ceiling, par := r.scanLimits(c.ceiling)
			if ceiling != c.wantCeiling || par != c.wantPar {
				t.Fatalf("scanLimits(%d) with maxChunk %d, histChunk %d, parallel %d: got %d, %d; want %d, %d",
					c.ceiling, c.maxChunk, c.histChunk, c.parallel, ceiling, par, c.wantCeiling, c.wantPar)
			}
		})
	}
}

// v4PoolsOf: capped ranges, one at a time, covering the whole span; the same
// pools in the same order with and without the cap; without it, one range
// over the whole span (as before).
func TestV4PoolsOfHistoryCap(t *testing.T) {
	token := "0x5555555555555555555555555555555555555555"
	other := "0x9999999999999999999999999999999999999999"
	var want []string
	for _, c := range historyCapCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour) // 8.64M blocks
			head := f.latest
			for i, b := range []uint64{500, 1_999_999, 2_000_000, 5_500_000, 8_000_000} {
				id := fmt.Sprintf("0x%064x", 0x100+i)
				if i%2 == 0 {
					f.initV4(id, tWETH, token, zeroAddr, 500, b) // token = currency1
				} else {
					f.initV4(id, token, other, zeroAddr, 500, b) // token = currency0
				}
			}
			p := probeLogs(f, 2*time.Millisecond)
			o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4", "SCOUT_RPC_HISTORY_CHUNK": c.value})
			pools, err := o.v4PoolsOf(context.Background(), token, head)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, pl := range pools {
				got = append(got, fmt.Sprintf("%s@%d", pl.id, pl.block))
			}
			if want == nil {
				want = got
			}
			if len(got) != 5 || !slices.Equal(got, want) {
				t.Fatalf("pools %v, want %v (5, as with the first setting)", got, want)
			}
			asks := p.rl.list(tPM)
			if c.limit == 0 {
				if len(asks) != 2 || asks[0].from != 1 || asks[0].to != head {
					t.Fatalf("asks %+v, want two (one per currency side) over the whole span 1-%d", asks, head)
				}
				return
			}
			checkCapped(t, asks, c.limit)
			spans := historySpans(1, head, c.limit)
			if len(asks) != 2*len(spans) {
				t.Fatalf("%d asks, want 2 × %d ranges", len(asks), len(spans))
			}
			if top := p.top.Load(); top != 1 {
				t.Fatalf("%d ranges in flight at once, want 1", top)
			}
		})
	}
}

// findLaunch: the same launch block and tx with and without the cap; with it
// no range is above the cap and the search stops in the range holding the
// launch; without it one range over [1, entry].
func TestFindLaunchHistoryCap(t *testing.T) {
	for _, c := range historyCapCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 20*24*time.Hour) // 17.28M blocks
			token := hAddr(70)
			const launch = 5_000_123
			entry := f.latest - 100_000
			f.transfer(token, zeroAddr, hAddr(71), launch, "0xlaunch")
			f.transfer(token, zeroAddr, hAddr(72), 9_000_000, "0xlater")
			f.transfer(token, zeroAddr, hAddr(73), entry-10, "0xlatest")
			f.transfer(token, hAddr(71), hAddr(74), 3_000_000, "0xnotamint") // before the launch, not a mint
			p := probeLogs(f, time.Millisecond)
			o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "8", "SCOUT_RPC_HISTORY_CHUNK": c.value})
			block, tx, found, err := o.findLaunch(context.Background(), token, entry)
			if err != nil || !found || block != launch || tx != "0xlaunch" {
				t.Fatalf("findLaunch: got %d %s %v %v, want %d 0xlaunch", block, tx, found, err, launch)
			}
			asks := p.rl.list(token)
			if c.limit == 0 {
				if len(asks) != 1 || asks[0].from != 1 || asks[0].to != entry {
					t.Fatalf("asks %+v, want one over 1-%d", asks, entry)
				}
				return
			}
			checkCapped(t, asks, c.limit)
			// Up to the range holding the launch (which ends on a multiple of the cap).
			n, lastTo := len(historySpans(1, launch, c.limit)), launch-launch%c.limit+c.limit-1
			if len(asks) != n || asks[n-1].to != lastTo {
				t.Fatalf("asks %+v, want %d ranges, the last ending at %d (the one holding the launch)", asks, n, lastTo)
			}
			if top := p.top.Load(); top != 1 {
				t.Fatalf("%d ranges in flight at once, want 1", top)
			}
		})
	}
}

// The USD-pool search over an asset's history (usdPoolsInHistory, reached
// through quoteUSD): the same USD price and source with and without the cap;
// with it no transfer or Initialize range is above the cap and none overlap;
// without it one transfer range from block 1 to the head.
func TestUSDPoolsInHistoryCap(t *testing.T) {
	for _, c := range historyCapCases {
		t.Run(c.name, func(t *testing.T) {
			fc := newFarPoolChain(t) // 25.92M blocks
			p := probeLogs(fc.f, 0)
			env := map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4", "SCOUT_RPC_HISTORY_CHUNK": c.value}
			maps.Copy(env, fc.env)
			o := testOnchain(t, fc.f, env)
			got, ok, err := o.quoteUSD(context.Background(), farAsset, fc.b1)
			if err != nil || !ok || !near6(got, 2) {
				t.Fatalf("quoteUSD(FAR, %d): got %v ok=%v err=%v, want $2", fc.b1, got, ok, err)
			}
			if src := o.quoteSource(farAsset); src != "its USDG pool (uniswap-v3)" {
				t.Fatalf("quoteSource(FAR): got %q, want its USDG pool", src)
			}
			head := fc.f.latest
			var sweep []rangeAsk // the transfer sweep: the asset's ranges from block 1
			for _, a := range p.rl.list(farAsset) {
				if a.from == 1 || len(sweep) > 0 && a.from == sweep[len(sweep)-1].to+1 {
					sweep = append(sweep, a)
				}
			}
			if c.limit == 0 {
				if len(sweep) != 1 || sweep[0].to != head {
					t.Fatalf("transfer sweep %+v, want one range 1-%d", sweep, head)
				}
				return
			}
			checkCapped(t, p.rl.list(farAsset), c.limit)
			checkCapped(t, p.rl.list(tPM), c.limit)
			if len(sweep) == 0 || sweep[len(sweep)-1].to != head {
				t.Fatalf("transfer sweep %+v, want ranges over 1-%d", sweep, head)
			}
		})
	}
}

// The Chainlink proxy's AggregatorConfirmed history (feedAggregators): the
// same switches in the same order with and without the cap; with it no range
// above the cap, one at a time.
func TestFeedAggregatorsHistoryCap(t *testing.T) {
	const proxy = "0x00000000000000000000000000000000000000f8"
	aggs := []string{"0x00000000000000000000000000000000000000e1", "0x00000000000000000000000000000000000000e2",
		"0x00000000000000000000000000000000000000e3", "0x00000000000000000000000000000000000000e4"}
	var want string
	for _, c := range historyCapCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour)
			for i, b := range []uint64{1_000, 1_999_999, 4_500_000} {
				f.logs = append(f.logs, fakeLog{addr: proxy, topics: []string{topicAggregatorConfirmed, addrTopic(aggs[i]), addrTopic(aggs[i+1])},
					data: "0x", block: b, tx: fmt.Sprintf("0xc%d", i)})
			}
			p := probeLogs(f, time.Millisecond)
			o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4", "SCOUT_RPC_HISTORY_CHUNK": c.value})
			fi, err := o.feedAggregators(context.Background(), proxy)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprint(fi.confirms)
			if want == "" {
				want = got
			}
			if len(fi.confirms) != 3 || got != want {
				t.Fatalf("confirms %s, want %s (3, as with the first setting)", got, want)
			}
			asks := p.rl.list(proxy)
			head := f.latest
			if c.limit == 0 {
				if len(asks) != 1 || asks[0].from != 1 || asks[0].to < head {
					t.Fatalf("asks %+v, want one from block 1 to the head", asks)
				}
				return
			}
			checkCapped(t, asks, c.limit)
			if top := p.top.Load(); top != 1 {
				t.Fatalf("%d ranges in flight at once, want 1", top)
			}
		})
	}
}

// One client: a whole-history search never has two ranges in flight, while a
// normal scan keeps SCOUT_RPC_PARALLEL ranges in flight.
func TestHistorySearchOneRangeInFlight(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	addr := hAddr(60)
	for b := uint64(1000); b < f.latest; b += 300_000 {
		f.transfer(addr, hAddr(61), hAddr(62), b, fmt.Sprintf("0x%x", b))
	}
	p := probeLogs(f, 20*time.Millisecond)
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4", "SCOUT_RPC_LOG_CHUNK": "200000"})
	ctx := context.Background()
	collect := func(scan func(fn func(rpcLog)) error) []uint64 {
		t.Helper()
		var got []uint64
		if err := scan(func(l rpcLog) { got = append(got, l.block()) }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	hist := collect(func(fn func(rpcLog)) error {
		return o.rpc.getLogsChunkedUpTo(ctx, addr, []any{topicTransfer}, 1, f.latest, f.latest+1, fn)
	})
	if top := p.top.Load(); top != 1 {
		t.Fatalf("whole-history search: %d ranges in flight at once, want 1", top)
	}
	checkCapped(t, p.rl.list(addr), defaultHistoryChunk)
	p.reset()
	normal := collect(func(fn func(rpcLog)) error {
		return o.rpc.getLogsChunked(ctx, addr, []any{topicTransfer}, 1, f.latest, fn)
	})
	if top := p.top.Load(); top < 2 || top > 4 {
		t.Fatalf("normal scan: %d ranges in flight at once, want 2 to 4 (SCOUT_RPC_PARALLEL=4)", top)
	}
	if !slices.Equal(hist, normal) || !slices.IsSorted(hist) || len(hist) == 0 {
		t.Fatalf("history search delivered %v, normal scan %v; want the same logs in block order", hist, normal)
	}
}

// SCOUT_RPC_HISTORY_CHUNK=0 is the rollback to the behaviour before the cap:
// a Pons-sized search keeps its 4M ranges and SCOUT_RPC_PARALLEL of them in
// flight, and a whole-history search asks one range for the whole span.
func TestHistorySearchNoCapAsBefore(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour) // 8.64M blocks: three 4M ranges
	addr := hAddr(66)
	for b := uint64(1000); b < f.latest; b += 1_000_000 {
		f.transfer(addr, hAddr(67), hAddr(68), b, fmt.Sprintf("0x%x", b))
	}
	p := probeLogs(f, 20*time.Millisecond)
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0", "SCOUT_RPC_PARALLEL": "4", "SCOUT_RPC_HISTORY_CHUNK": "0"})
	ctx := context.Background()
	noop := func(rpcLog) {}
	if err := o.rpc.getLogsChunkedUpTo(ctx, addr, []any{topicTransfer}, 1, f.latest, ponsSearchChunk, noop); err != nil {
		t.Fatal(err)
	}
	asks := p.rl.list(addr)
	spans := historySpans(1, f.latest, ponsSearchChunk)
	if len(asks) != len(spans) {
		t.Fatalf("Pons-sized search asked %+v, want the %d ranges %v", asks, len(spans), spans)
	}
	if top := p.top.Load(); top < 2 {
		t.Fatalf("Pons-sized search: %d ranges in flight at once, want 2 or more (SCOUT_RPC_PARALLEL=4, as before the cap)", top)
	}
	p.reset()
	if err := o.rpc.getLogsChunkedUpTo(ctx, addr, []any{topicTransfer}, 1, f.latest, f.latest+1, noop); err != nil {
		t.Fatal(err)
	}
	if asks := p.rl.list(addr); len(asks) != 1 || asks[0].from != 1 || asks[0].to != f.latest {
		t.Fatalf("whole-history search asked %+v, want one range 1-%d", asks, f.latest)
	}
}

// A capped whole-history search whose ranges the node refuses as too large
// over one stretch still delivers every log in block order, and never grows
// back above the cap.
func TestHistorySearchCapAfterRefusal(t *testing.T) {
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	addr := hAddr(63)
	var want []uint64
	for b := uint64(500); b < f.latest; b += 250_001 {
		f.transfer(addr, hAddr(64), hAddr(65), b, fmt.Sprintf("0x%x", b))
		want = append(want, b)
	}
	var rl rangeLog
	f.setLogsHook(func(a string, from, to uint64) string {
		refuse := from < 3_000_000 && to-from+1 > 500_000
		rl.add(rangeAsk{a, from, to, refuse})
		if refuse {
			return "query returned more than 10000 results"
		}
		return ""
	})
	captureRangeLog(t)
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CACHE": "0"})
	var got []uint64
	if err := o.rpc.getLogsChunkedUpTo(context.Background(), addr, []any{topicTransfer}, 1, f.latest, f.latest+1, func(l rpcLog) {
		got = append(got, l.block())
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %d logs, want %d in block order", len(got), len(want))
	}
	refused := 0
	for _, a := range rl.list(addr) {
		if a.refused {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("no range refused; the test does not test the split")
	}
	checkCapped(t, rl.list(addr), defaultHistoryChunk)
}

// The start-up line names the cap (or that there is none).
func TestDescribeHistoryChunk(t *testing.T) {
	f := newFakeChain(t, time.Hour)
	for _, c := range []struct{ value, logChunk, want string }{
		{"", "", "whole-history searches one range at a time, of up to 2000000 blocks (SCOUT_RPC_HISTORY_CHUNK)"},
		{"0", "", "whole-history searches not capped (SCOUT_RPC_HISTORY_CHUNK=0: one range for the whole span, 8 ranges per scan as before)"},
		{"1000000", "1000000", "whole-history searches in ranges of up to 1000000 blocks, 8 at a time (SCOUT_RPC_HISTORY_CHUNK; SCOUT_RPC_LOG_CHUNK is not below it)"},
	} {
		o := testOnchain(t, f, map[string]string{"SCOUT_RPC_HISTORY_CHUNK": c.value, "SCOUT_RPC_LOG_CHUNK": c.logChunk, "SCOUT_RPC_PARALLEL": "8"})
		if got := o.describe(); !strings.Contains(got, c.want) {
			t.Errorf("SCOUT_RPC_HISTORY_CHUNK=%q: describe() = %q, want it to contain %q", c.value, got, c.want)
		}
	}
}
