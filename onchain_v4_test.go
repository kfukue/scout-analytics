package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// v4 pool discovery through the PoolManager's Initialize events: the token's
// pools are listed by Initialize (token as currency0 or currency1), then only
// those pools' swaps are read (pool id as topic 1). The PoolManager's swaps are
// never read unfiltered.

// initV4 adds a PoolManager Initialize with full-length data (fee,
// tickSpacing, hooks, sqrtPriceX96, tick). c0 must sort below c1.
func (f *fakeChain) initV4(id, c0, c1, hooks string, fee int64, block uint64) {
	f.logs = append(f.logs, fakeLog{addr: tPM, topics: []string{topicInitV4, id, addrTopic(c0), addrTopic(c1)},
		data: ret(wInt(fee), wInt(60), wAddr(hooks), w32(sqrtX96(1)), wInt(0)), block: block, tx: "0xinit" + id[2:10]})
}

// pmQueries returns the eth_getLogs asked of the PoolManager whose first topic is sig.
func (f *fakeChain) pmQueries(sig string) []fakeLogQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeLogQuery
	for _, q := range f.queries {
		if q.addr != tPM || len(q.topics) == 0 {
			continue
		}
		if s, ok := q.topics[0].(string); ok && strings.EqualFold(s, sig) {
			out = append(out, q)
		}
	}
	return out
}

// swapIDsAsked: the pool ids the PoolManager Swap filters named (topic 1).
func (f *fakeChain) swapIDsAsked() []string {
	var ids []string
	for _, q := range f.pmQueries(topicSwapV4) {
		if len(q.topics) < 2 {
			continue
		}
		switch v := q.topics[1].(type) {
		case string:
			ids = append(ids, strings.ToLower(v))
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					ids = append(ids, strings.ToLower(s))
				}
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// checkInitLookups: exactly the two whole-history Initialize queries (token as
// currency0, as currency1) and no Initialize lookup by pool id (the old
// backward walk).
func checkInitLookups(t *testing.T, f *fakeChain, token string, from, to uint64) {
	t.Helper()
	qs := f.pmQueries(topicInitV4)
	if len(qs) != 2 {
		t.Fatalf("got %d Initialize queries, want 2: %+v", len(qs), qs)
	}
	for i, q := range qs {
		if q.from != from || q.to != to {
			t.Errorf("Initialize query %d: got blocks %d-%d, want %d-%d", i, q.from, q.to, from, to)
		}
		if len(q.topics) != 3+i || q.topics[1] != nil || !strings.EqualFold(q.topics[2+i].(string), addrTopic(token)) {
			t.Errorf("Initialize query %d: got topics %v, want the token as topic %d", i, q.topics, 2+i)
		}
	}
}

func TestV4PoolOfInit(t *testing.T) {
	id, c0, c1, hook := "0x"+strings.Repeat("ab", 32), "0x1111111111111111111111111111111111111111", "0x2222222222222222222222222222222222222222", "0x00000000000000000000000000000000000000e5"
	full := rpcLog{Topics: []string{topicInitV4, strings.ToUpper(id[:4]) + id[4:], addrTopic(c0), addrTopic(c1)},
		Data: ret(wInt(3000), wInt(-60), wAddr(hook), w32(sqrtX96(1)), wInt(0)), BlockNumber: hexU64(77)}
	short := full
	short.Data = ret(wInt(500)) // older fixtures: the fee only
	other := full
	other.Topics = []string{topicSwapV4, id, addrTopic(c0), addrTopic(c1)}
	for _, c := range []struct {
		name string
		l    rpcLog
		ok   bool
		want v4Pool
	}{
		{"full data", full, true, v4Pool{id: id, c0: c0, c1: c1, fee: 3000, tickSpacing: -60, hooks: hook, block: 77}},
		{"fee only", short, true, v4Pool{id: id, c0: c0, c1: c1, fee: 500, block: 77}},
		{"not an Initialize", other, false, v4Pool{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := v4PoolOfInit(c.l)
			if ok != c.ok || got != c.want {
				t.Fatalf("v4PoolOfInit(%+v): got %+v %v, want %+v %v", c.l, got, ok, c.want, c.ok)
			}
		})
	}
}

// A v4 token is found through its Initialize and its own pool's swaps; the
// unfiltered PoolManager swap scan and the backward Initialize walk are gone.
// A second discovery of the same token only reads the new blocks.
func TestDiscoverV4ViaInitialize(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	token, id, otherID := "0x9999999999999999999999999999999999991e18", "0x"+strings.Repeat("ab", 32), "0x"+strings.Repeat("cd", 32)
	f.addToken(token, 18, "TKN")
	f.addToken(tWETH, 18, "WETH")
	eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
	f.initV4(id, tWETH, token, "0x00000000000000000000000000000000000000e5", 3000, eb-4_000_000) // long before the call
	f.initV4(otherID, tWETH, tUSDG, zeroAddr, 500, eb-10)                                        // not the token's
	swap := func(block uint64, tx string) {
		f.swapV4(tPM, id, block, tx, sqrtX96(1e6))
		f.transfer(token, tPM, "0x00000000000000000000000000000000000000d1", block, tx)
	}
	swap(eb-50, "0xs1")
	swap(eb+900, "0xs2")
	f.swapV4(tPM, otherID, eb+5, "0xunrelated", sqrtX96(1))

	o := testOnchain(t, f, nil)
	ctx := context.Background()
	st, err := o.discover(ctx, token, eb)
	if err != nil || st.Kind != "v4" || st.PoolID != id || st.TokenIs0 || st.Quote != tWETH || st.Pool != tPM {
		t.Fatalf("discover(%s, %d): got %+v %v, want v4 pool %s with quote WETH, token currency1", token, eb, st, err, id)
	}
	to := eb + o.cfg.DiscoveryBlocks
	checkInitLookups(t, f, token, 1, to)
	if n := f.pmSwapScans(); n != 0 {
		t.Fatalf("got %d PoolManager-wide swap scans, want 0", n)
	}
	if got := f.swapIDsAsked(); !slices.Equal(got, []string{id}) {
		t.Fatalf("swap filters named pools %v, want only %s", got, id)
	}
	t.Logf("v4 discovery: %d eth_getLogs, %d eth_call", f.count["eth_getLogs"], f.count["eth_call"])

	// Again, later: the Initialize search covers only the blocks after the first one.
	eb2 := eb + 100_000
	swap(eb2+10, "0xs3")
	if st, err := o.discover(ctx, token, eb2); err != nil || st.PoolID != id {
		t.Fatalf("second discover(%s, %d): got %+v %v, want pool %s", token, eb2, st, err, id)
	}
	qs := f.pmQueries(topicInitV4)
	if len(qs) != 4 {
		t.Fatalf("got %d Initialize queries after two discoveries, want 4", len(qs))
	}
	for _, q := range qs[2:] {
		if q.from != to+1 || q.to != eb2+o.cfg.DiscoveryBlocks {
			t.Errorf("second Initialize search: got blocks %d-%d, want %d-%d", q.from, q.to, to+1, eb2+o.cfg.DiscoveryBlocks)
		}
	}
}

// A token with several v4 pools: the pool with the most swaps in the token's
// own transactions wins (ties: the lowest pool id), as before; accept filters
// the quote before any swap is read; swaps of other transactions don't count.
func TestDiscoverV4SeveralPools(t *testing.T) {
	token, other := "0xcccccccccccccccccccccccccccccccccccccccc", "0xdddddddddddddddddddddddddddddddddddddddd"
	idW, idU, idS, idLate := "0x01"+strings.Repeat("00", 31), "0x02"+strings.Repeat("00", 31), "0x03"+strings.Repeat("00", 31), "0x04"+strings.Repeat("00", 31)
	hookA, hookB := "0x00000000000000000000000000000000000000a0", "0x00000000000000000000000000000000000000b0"
	type swaps struct{ w, u, spam int } // swaps in the token's transactions (spam: in other transactions)
	for _, c := range []struct {
		name     string
		swaps    swaps
		accept   func(string) bool
		wantID   string // "" = no pool
		wantQ    string
		wantAsks []string // pool ids the swap filters may name
	}{
		{"most swaps in the token's transactions", swaps{2, 3, 10}, nil, idU, tUSDG, []string{idW, idU, idS}},
		{"accept: WETH only", swaps{2, 3, 10}, func(q string) bool { return q == tWETH }, idW, tWETH, []string{idW}},
		{"equal counts: lowest id", swaps{2, 2, 0}, nil, idW, tWETH, []string{idW, idU, idS}},
		{"other transactions' swaps don't count", swaps{0, 0, 10}, nil, "", "", []string{idW, idU, idS}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour)
			f.addToken(token, 18, "TKN")
			f.addToken(tWETH, 18, "WETH")
			f.addToken(tUSDG, 6, "USDG")
			f.addToken(other, 18, "OTH")
			eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
			f.initV4(idW, tWETH, token, hookA, 3000, eb-500_000)
			f.initV4(idU, tUSDG, token, hookB, 500, eb-300_000)
			f.initV4(idS, token, other, zeroAddr, 10000, eb-200_000)
			f.initV4(idLate, tWETH, token, zeroAddr, 100, eb+5*defaultDiscoveryBlocks) // after the window: not a candidate
			n := 0
			add := func(id string, k int, inToken bool) {
				for range k {
					n++
					tx := fmt.Sprintf("0x%064x", n)
					f.swapV4(tPM, id, eb+uint64(n), tx, sqrtX96(1e6))
					if inToken {
						f.transfer(token, tPM, "0x00000000000000000000000000000000000000d1", eb+uint64(n), tx)
					}
				}
			}
			add(idW, c.swaps.w, true)
			add(idU, c.swaps.u, true)
			add(idS, c.swaps.spam, false)
			add(idLate, 3, false)
			f.transfer(token, "0x00000000000000000000000000000000000000d2", tPM, eb, "0xbuy") // PoolManager is a counterparty

			o := testOnchain(t, f, nil)
			st, err := o.discoverWith(context.Background(), token, eb, c.accept)
			if c.wantID == "" {
				if !errors.Is(err, errNoPool) {
					t.Fatalf("discoverWith(%s, %d): got %+v %v, want errNoPool", token, eb, st, err)
				}
			} else if err != nil || st.PoolID != c.wantID || st.Quote != c.wantQ || st.TokenIs0 {
				t.Fatalf("discoverWith(%s, %d) with swaps %+v: got %+v %v, want pool %s quote %s", token, eb, c.swaps, st, err, c.wantID, c.wantQ)
			}
			if n := f.pmSwapScans(); n != 0 {
				t.Fatalf("got %d PoolManager-wide swap scans, want 0", n)
			}
			if got := f.swapIDsAsked(); !slices.Equal(got, c.wantAsks) {
				t.Fatalf("swap filters named pools %v, want %v", got, c.wantAsks)
			}
			o.mu.Lock()
			hooks := map[string]string{}
			for _, p := range o.v4Pools[token].pools {
				hooks[p.id] = p.hooks
			}
			o.mu.Unlock()
			if hooks[idW] != hookA || hooks[idU] != hookB || len(hooks) != 3 {
				t.Fatalf("cached pools of %s: got hooks %v, want %s→%s, %s→%s and the spam pool (not the late one)", token, hooks, idW, hookA, idU, hookB)
			}
		})
	}
}

// defaultDiscoveryBlocks: the default SCOUT_DISCOVERY_BLOCKS (the windows below assume it).
const defaultDiscoveryBlocks = 18_000

// A token with no v4 pool (no Initialize) and no v2/v3 pool, whose transfers
// touch the PoolManager: no_pool without reading the PoolManager's swaps, also
// when the transfers are only found in a widened window.
func TestDiscoverNoV4PoolNoScan(t *testing.T) {
	token := "0x3333333333333333333333333333333333333333"
	for _, c := range []struct {
		name     string
		at       uint64 // blocks after the call of the token's only transfers
		initPool bool   // the token has a v4 pool, with no swaps near the call
		windows  int    // transfer windows read (×1, ×4, ×16, ×64)
	}{
		{"transfers near the call", 100, false, 1},
		{"transfers only in the ×16 window", 10 * defaultDiscoveryBlocks, false, 3},
		{"a v4 pool without swaps near the call", 100, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 10*24*time.Hour)
			f.addToken(token, 18, "O1")
			eb := f.blockAtTime(time.Now().Add(-5 * 24 * time.Hour))
			f.transfer(token, "0x00000000000000000000000000000000000000d1", tPM, eb+c.at, "0x1")
			f.transfer(token, tPM, "0x00000000000000000000000000000000000000d2", eb+c.at, "0x1")
			f.initV4("0x"+strings.Repeat("cd", 32), tWETH, tUSDG, zeroAddr, 500, eb-10) // another token's pool
			f.swapV4(tPM, "0x"+strings.Repeat("cd", 32), eb+c.at, "0x1", sqrtX96(1))    // a swap in the same tx
			id := "0x" + strings.Repeat("ef", 32)
			if c.initPool {
				f.initV4(id, token, "0x4444444444444444444444444444444444444444", zeroAddr, 500, eb-1_000_000)
				f.swapV4(tPM, id, eb-500_000, "0xold", sqrtX96(1))
			}
			o := testOnchain(t, f, nil)
			if _, err := o.discover(context.Background(), token, eb); !errors.Is(err, errNoPool) {
				t.Fatalf("discover(%s, %d): got %v, want errNoPool", token, eb, err)
			}
			if n := f.pmSwapScans(); n != 0 {
				t.Fatalf("got %d PoolManager-wide swap scans, want 0", n)
			}
			span := o.cfg.DiscoveryBlocks * []uint64{1, 4, 16, 64}[c.windows-1]
			checkInitLookups(t, f, token, 1, eb+span)
			var wantIDs []string
			if c.initPool {
				wantIDs = []string{id}
			}
			if got := f.swapIDsAsked(); !slices.Equal(got, wantIDs) {
				t.Fatalf("swap filters named pools %v, want %v", got, wantIDs)
			}
			f.mu.Lock()
			var maxTo uint64
			for _, q := range f.queries {
				if q.addr == token {
					maxTo = max(maxTo, q.to)
				}
			}
			f.mu.Unlock()
			if maxTo != eb+span {
				t.Fatalf("transfer queries reach block %d, want %d (the ×%d window)", maxTo, eb+span, span/o.cfg.DiscoveryBlocks)
			}
			t.Logf("no-pool discovery: %d eth_getLogs, %d eth_call", f.count["eth_getLogs"], f.count["eth_call"])
		})
	}
}

// A quote asset with no feed (VIRT) priced through its own v4 pool against a
// stablecoin, found through Initialize: no PoolManager-wide swap scan, and the
// pool against an asset with no USD price is never read.
func TestQuoteViaV4PoolNoScan(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	virt, oth := "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "0xffffffffffffffffffffffffffffffffffffffff"
	idU, idO := "0x"+strings.Repeat("12", 32), "0x"+strings.Repeat("34", 32)
	f.addToken(virt, 18, "VIRT")
	f.addToken(tUSDG, 6, "USDG")
	f.addToken(oth, 18, "OTH")
	eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
	f.initV4(idU, tUSDG, virt, zeroAddr, 500, eb-2_000_000)
	f.initV4(idO, virt, oth, zeroAddr, 500, eb-1_000_000) // OTH has no USD price: not accepted
	trade := func(id string, block uint64, tx string, raw float64) {
		f.swapV4(tPM, id, block, tx, sqrtX96(raw))
		f.transfer(virt, tPM, "0x00000000000000000000000000000000000000d1", block, tx)
	}
	trade(idU, eb-50, "0xu1", 0.5e12) // USDG per VIRT 0.5 → VIRT = $2
	for i := range 5 {
		trade(idO, eb-40+uint64(i), "0xo"+string(rune('a'+i)), 1)
	}
	o := testOnchain(t, f, nil)
	q, ok, err := o.quoteUSD(context.Background(), virt, eb)
	if err != nil || !ok || math.Abs(q-2) > 1e-9 {
		t.Fatalf("quoteUSD(VIRT, %d): got %v %v %v, want $2", eb, q, ok, err)
	}
	if n := f.pmSwapScans(); n != 0 {
		t.Fatalf("got %d PoolManager-wide swap scans, want 0", n)
	}
	checkInitLookups(t, f, virt, 1, eb+o.cfg.DiscoveryBlocks)
	if got := f.swapIDsAsked(); !slices.Equal(got, []string{idU}) {
		t.Fatalf("swap filters named pools %v, want only the USDG pool %s", got, idU)
	}
}

// -price-check's graduation line never says "searched through block 0", and
// says when the graduation came after the call.
func TestPonsSummaryGraduation(t *testing.T) {
	base := onchainState{Curve: "0xc", QuoteSym: "ETH", Quote: zeroAddr, Token: "0xt", EntryBlock: 1000}
	with := func(f func(st *onchainState)) onchainState { st := base; f(&st); return st }
	for _, c := range []struct {
		name string
		st   onchainState
		want string
	}{
		{"on the curve", base, "graduation:   not graduated (still on the bonding curve)"},
		{"closed before the call, no pool", with(func(st *onchainState) { st.PonsDone, st.PonsSeen = 900, 5000 }),
			"graduation:   curve closed at block 900; no v4 pool yet (searched through block 5000)"},
		{"closed after the call, not looked up", with(func(st *onchainState) { st.PonsDone = 1200 }),
			"graduation:   graduated after the call: curve closed at block 1200; v4 pool not looked up"},
		{"closed after the call, pool found", with(func(st *onchainState) {
			st.PonsDone, st.PonsGrad, st.PoolID, st.Hook = 1200, 1202, "0xid", "0xh"
		}), "graduation:   graduated after the call: curve closed at block 1200, v4 pool from block 1202: PoolManager id 0xid (hook 0xh, token is currency1)"},
		{"closed before the call, pool found", with(func(st *onchainState) {
			st.PonsDone, st.PonsGrad, st.PoolID, st.Hook, st.TokenIs0 = 900, 902, "0xid", "0xh", true
		}), "graduation:   curve closed at block 900, v4 pool from block 902: PoolManager id 0xid (hook 0xh, token is currency0)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.st.ponsSummary()
			if len(got) != 2 || got[1] != c.want {
				t.Fatalf("ponsSummary(%+v): got %q, want %q", c.st, got, c.want)
			}
		})
	}
}

// The blocks near the chain's tip are not taken as searched: a pool whose
// Initialize shows up there after a first lookup is found by the next one.
func TestV4PoolsOfNearTip(t *testing.T) {
	f := newFakeChain(t, 10*24*time.Hour)
	token, id := "0x9999999999999999999999999999999999991e18", "0x"+strings.Repeat("ab", 32)
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	head, err := o.rpc.blockNumber(ctx) // the client learns the head
	if err != nil {
		t.Fatal(err)
	}
	if pools, err := o.v4PoolsOf(ctx, token, head); err != nil || len(pools) != 0 {
		t.Fatalf("v4PoolsOf(%s, %d): got %v %v, want no pools", token, head, pools, err)
	}
	f.mu.Lock()
	f.initV4(id, tWETH, token, zeroAddr, 3000, head-5) // indexed late
	f.mu.Unlock()
	pools, err := o.v4PoolsOf(ctx, token, head)
	if err != nil || len(pools) != 1 || pools[0].id != id {
		t.Fatalf("second v4PoolsOf(%s, %d): got %v %v, want pool %s", token, head, pools, err, id)
	}
	qs := f.pmQueries(topicInitV4)
	if len(qs) != 4 || qs[2].from != head-999 || qs[2].to != head {
		t.Fatalf("got Initialize queries %+v, want the second pair over blocks %d-%d", qs, head-999, head)
	}
}
