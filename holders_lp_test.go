package main

import (
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
)

// Liquidity test addresses: a v2/v3 pool, the v4 PoolManager, a router, the
// v3 position manager, and the v4 pool ids.
var (
	lqPool, lqPM, lqRouter, lqNPM = hAddr(50), hAddr(51), hAddr(70), hAddr(71)
	lqPoolID                      = "0x" + strings.Repeat("ab", 32)
	lqOtherPoolID                 = "0x" + strings.Repeat("cd", 32)
)

func tickTopic(n int64) string { return "0x" + wInt(n) }

// lqLog builds a raw log as the node returns it: block 0x10, log index 3, an
// upper-case tx hash.
func lqLog(emitter, topic0 string, topics []string, words ...string) rpcLog {
	return rpcLog{Address: emitter, Topics: append([]string{topic0}, topics...), Data: ret(words...),
		BlockNumber: hexU64(0x10), TxHash: "0xABCD01", LogIndex: hexU64(3)}
}

func v2MintLog() rpcLog {
	return lqLog(lqPool, topicMintV2, []string{addrTopic(lqRouter)}, wInt(1), wInt(2))
}

func v2BurnLog() rpcLog {
	return lqLog(lqPool, topicBurnV2, []string{addrTopic(lqRouter), addrTopic(hAddr(1))}, wInt(1), wInt(2))
}

func v3MintLog() rpcLog {
	return lqLog(lqPool, topicMintV3, []string{addrTopic(lqNPM), tickTopic(-600), tickTopic(600)},
		wAddr(lqRouter), wInt(1000), wInt(5), wInt(6))
}

func v3BurnLog(liquidity int64) rpcLog {
	return lqLog(lqPool, topicBurnV3, []string{addrTopic(lqNPM), tickTopic(-600), tickTopic(600)},
		wInt(liquidity), wInt(5), wInt(6))
}

func v3CollectLog() rpcLog {
	return lqLog(lqPool, topicCollectV3, []string{addrTopic(lqNPM), tickTopic(-600), tickTopic(600)},
		wAddr(hAddr(1)), wInt(5), wInt(6))
}

func v4ModifyLog(id string, delta *big.Int) rpcLog {
	return lqLog(lqPM, topicModifyLiquidityV4, []string{id, addrTopic(lqRouter)},
		wInt(-600), wInt(600), w32(delta), strings.Repeat("11", 32))
}

// The topic constants come from topicOf; the literals are the published
// hashes, so a typo in a signature string fails here.
func TestLiquidityTopics(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"v2 Mint", topicMintV2, "0x4c209b5fc8ad50758f13e2e1088ba56a560dff690a1c6fef26394f4c03821c4f"},
		{"v2 Burn", topicBurnV2, "0xdccd412f0b1252819cb1fd330b93224ca42612892bb3f4f789976e6d81936496"},
		{"v3 Mint", topicMintV3, "0x7a53080ba414158be7ec69b987b5fb7d07dee101fe85488f0853ae16239d0bde"},
		{"v3 Burn", topicBurnV3, "0x0c396cd989a39f4459b5fa1aed6a9a8dcdbc45908acfd67e028cd568da98982c"},
		{"v3 Collect", topicCollectV3, "0x70935338e69775456a85ddef226c395fb668b63fa0115f5f20610b388e6ca9c0"},
		{"v4 ModifyLiquidity", topicModifyLiquidityV4, "0xf208f4912782fd25c7f114ca3723a2d5dd6f3bcc3ac8db5af63baa85f711d5ec"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s topic0: got %s, want %s", c.name, c.got, c.want)
		}
	}
}

func TestLiquidityDecode(t *testing.T) {
	maxInt256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1))
	minInt256 := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255))
	upperID := "0x" + strings.ToUpper(lqPoolID[2:])
	ev := func(proto string, kind liquidityKind, venue, poolID, owner string) liquidityEvent {
		return liquidityEvent{Protocol: proto, Kind: kind, Venue: venue, PoolID: poolID, Owner: owner,
			Block: 0x10, LogIndex: 3, TxHash: "0xabcd01"}
	}
	cases := []struct {
		name string
		log  rpcLog
		want liquidityEvent
	}{
		{"v2 Mint: add", v2MintLog(), ev("v2", lpAdd, lqPool, "", lqRouter)},
		{"v2 Burn: remove", v2BurnLog(), ev("v2", lpRemove, lqPool, "", lqRouter)},
		{"v3 Mint: add", v3MintLog(), ev("v3", lpAdd, lqPool, "", lqNPM)},
		{"v3 Burn: remove", v3BurnLog(1000), ev("v3", lpRemove, lqPool, "", lqNPM)},
		{"v3 Burn of 0 (fee poke): collect", v3BurnLog(0), ev("v3", lpCollect, lqPool, "", lqNPM)},
		{"v3 Collect: collect", v3CollectLog(), ev("v3", lpCollect, lqPool, "", lqNPM)},
		{"v4 positive delta: add", v4ModifyLog(lqPoolID, big.NewInt(1)), ev("v4", lpAdd, lqPM, lqPoolID, lqRouter)},
		{"v4 negative delta: remove", v4ModifyLog(lqPoolID, big.NewInt(-1)), ev("v4", lpRemove, lqPM, lqPoolID, lqRouter)},
		{"v4 zero delta: collect", v4ModifyLog(lqPoolID, new(big.Int)), ev("v4", lpCollect, lqPM, lqPoolID, lqRouter)},
		{"v4 largest delta", v4ModifyLog(lqPoolID, maxInt256), ev("v4", lpAdd, lqPM, lqPoolID, lqRouter)},
		{"v4 most negative delta", v4ModifyLog(lqPoolID, minInt256), ev("v4", lpRemove, lqPM, lqPoolID, lqRouter)},
		{"v4 upper-case pool id is lowered", v4ModifyLog(upperID, big.NewInt(-5)), ev("v4", lpRemove, lqPM, lqPoolID, lqRouter)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := c.log
			l.Topics[0] = "0x" + strings.ToUpper(l.Topics[0][2:]) // case must not matter
			l.Address = "0x" + strings.ToUpper(l.Address[2:])
			got, err := decodeLiquidityLog(l)
			if err != nil {
				t.Fatalf("decodeLiquidityLog(%+v): %v", l, err)
			}
			if got != c.want {
				t.Errorf("decodeLiquidityLog(%+v): got %+v, want %+v", l, got, c.want)
			}
		})
	}
}

// Every malformation is rejected for every event, with the sentinel and no
// panic.
func TestLiquidityDecodeMalformed(t *testing.T) {
	events := []struct {
		name       string
		log        func() rpcLog
		ownerTopic int
	}{
		{"v2 Mint", v2MintLog, 1},
		{"v2 Burn", v2BurnLog, 1},
		{"v3 Mint", v3MintLog, 1},
		{"v3 Burn", func() rpcLog { return v3BurnLog(1000) }, 1},
		{"v3 Collect", v3CollectLog, 1},
		{"v4 ModifyLiquidity", func() rpcLog { return v4ModifyLog(lqPoolID, big.NewInt(-1)) }, 2},
	}
	edits := []struct {
		name string
		edit func(l *rpcLog, owner int)
	}{
		{"no topics", func(l *rpcLog, _ int) { l.Topics = nil }},
		{"v3 Swap topic0", func(l *rpcLog, _ int) { l.Topics[0] = topicSwapV3 }},
		{"v4 Swap topic0", func(l *rpcLog, _ int) { l.Topics[0] = topicSwapV4 }},
		{"v2 Sync topic0", func(l *rpcLog, _ int) { l.Topics[0] = topicSyncV2 }},
		{"Transfer topic0", func(l *rpcLog, _ int) { l.Topics[0] = topicTransfer }},
		{"non-hex topic0", func(l *rpcLog, _ int) { l.Topics[0] = "0x" + strings.Repeat("zz", 32) }},
		{"one topic fewer", func(l *rpcLog, _ int) { l.Topics = l.Topics[:len(l.Topics)-1] }},
		{"one topic more", func(l *rpcLog, _ int) { l.Topics = append(l.Topics, l.Topics[1]) }},
		{"short topic 1", func(l *rpcLog, _ int) { l.Topics[1] = "0x" + strings.Repeat("11", 20) }},
		{"non-hex last topic", func(l *rpcLog, _ int) { l.Topics[len(l.Topics)-1] = "0x" + strings.Repeat("zz", 32) }},
		{"dirty owner padding", func(l *rpcLog, o int) { l.Topics[o] = "0x01" + l.Topics[o][4:] }},
		{"empty data", func(l *rpcLog, _ int) { l.Data = "0x" }},
		{"data one word short", func(l *rpcLog, _ int) { l.Data = l.Data[:len(l.Data)-64] }},
		{"data one word long", func(l *rpcLog, _ int) { l.Data += strings.Repeat("00", 32) }},
		{"data one byte long", func(l *rpcLog, _ int) { l.Data += "00" }},
		{"odd-length data", func(l *rpcLog, _ int) { l.Data += "0" }},
		{"non-hex data", func(l *rpcLog, _ int) { l.Data = "0xzz" + l.Data[4:] }},
		{"short emitter", func(l *rpcLog, _ int) { l.Address = "0x1234" }},
		{"non-hex emitter", func(l *rpcLog, _ int) { l.Address = "0x" + strings.Repeat("zz", 20) }},
		{"missing block number", func(l *rpcLog, _ int) { l.BlockNumber = "" }},
		{"bad block number", func(l *rpcLog, _ int) { l.BlockNumber = "0xzz" }},
		{"bad log index", func(l *rpcLog, _ int) { l.LogIndex = "12" }},
	}
	for _, e := range events {
		for _, ed := range edits {
			t.Run(e.name+"/"+ed.name, func(t *testing.T) {
				l := e.log()
				ed.edit(&l, e.ownerTopic)
				got, err := decodeLiquidityLog(l)
				if !errors.Is(err, errBadLiquidityLog) {
					t.Errorf("decodeLiquidityLog(%+v): got (%+v, %v), want an errBadLiquidityLog error", l, got, err)
				}
			})
		}
	}
}

// lqEv builds a decoded event directly.
func lqEv(proto string, kind liquidityKind, venue, poolID, tx string, block uint64) liquidityEvent {
	return liquidityEvent{Protocol: proto, Kind: kind, Venue: venue, PoolID: poolID, TxHash: tx, Block: block}
}

// Liquidity kinds of one transaction, for building sets by hand.
var (
	lqAddK       = liquidityTxKinds{Add: true}
	lqRemoveK    = liquidityTxKinds{Remove: true}
	lqCollectK   = liquidityTxKinds{Collect: true}
	lqAddRemoveK = liquidityTxKinds{Add: true, Remove: true}
)

// kindsString writes k as "add", "remove", "collect" joined by "+".
func kindsString(k liquidityTxKinds) string {
	var parts []string
	for _, p := range []struct {
		on   bool
		name string
	}{{k.Add, "add"}, {k.Remove, "remove"}, {k.Collect, "collect"}} {
		if p.on {
			parts = append(parts, p.name)
		}
	}
	return strings.Join(parts, "+")
}

// flatTxs lists a liquidityTxSet as sorted "venue tx kinds" strings.
func flatTxs(s liquidityTxSet) []string {
	var out []string
	for v, txs := range s {
		for tx, k := range txs {
			out = append(out, v+" "+tx+" "+kindsString(k))
		}
	}
	slices.Sort(out)
	return out
}

func TestLiquidityTxs(t *testing.T) {
	const entry = 50
	up := func(s string) string { return "0x" + strings.ToUpper(s[2:]) }
	events := []liquidityEvent{
		lqEv("v2", lpAdd, lqPool, "", "0xa1", 5),
		lqEv("v4", lpAdd, lqPM, lqPoolID, "0xb1", 6),
		lqEv("v4", lpRemove, lqPM, lqOtherPoolID, "0xc1", 7),       // another token's v4 pool
		lqEv("v3", lpRemove, hAddr(77), "", "0xd1", 8),             // not one of the token's venues
		lqEv("v2", lpRemove, lqPool, "", "0xe1", entry+1),          // after the entry block
		lqEv("v2", lpRemove, lqPool, "", "0xf1", entry),            // at the entry block
		lqEv("v4", lpCollect, up(lqPM), up(lqPoolID), "0xAB", 9),   // mixed case
		lqEv("v2", lpCollect, lqPool, "", "", 10),                  // no tx hash
		lqEv("v4", lpAdd, lqPM, "", "0x91", 11),                    // v4 without a pool id
		lqEv("v2", lpAdd, lqPool, "", "0xa1", 12),                  // same tx twice
		lqEv("v3", lpRemove, lqPool, lqOtherPoolID, "0x92", entry), // v3 never checks the pool id
		lqEv("v3", lpAdd, lqPool, "", "0xa2", 14),                  // an add and a collect in one tx: merged
		lqEv("v3", lpCollect, lqPool, "", "0xa2", 14),
		lqEv("v4", lpAdd, lqPM, lqPoolID, "0xb2", 15), // a v4 add and remove in one tx: merged
		lqEv("v4", lpRemove, lqPM, lqPoolID, "0xb2", 15),
	}
	cases := []struct {
		name            string
		venues, poolIDs map[string]bool
		want            []string
	}{
		{"pool and PoolManager, the token's pool id", set(lqPool, up(lqPM)), set(up(lqPoolID)),
			[]string{lqPM + " 0xab collect", lqPM + " 0xb1 add", lqPM + " 0xb2 add+remove", lqPool + " 0x92 remove",
				lqPool + " 0xa1 add", lqPool + " 0xa2 add+collect", lqPool + " 0xf1 remove"}},
		{"no pool ids: every v4 event dropped", set(lqPool, lqPM), nil,
			[]string{lqPool + " 0x92 remove", lqPool + " 0xa1 add", lqPool + " 0xa2 add+collect", lqPool + " 0xf1 remove"}},
		{"the other pool id only", set(lqPM), set(lqOtherPoolID), []string{lqPM + " 0xc1 remove"}},
		{"no venues", nil, set(lqPoolID), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := liquidityTxs(events, c.venues, c.poolIDs, entry)
			if err != nil {
				t.Fatalf("liquidityTxs: %v", err)
			}
			slices.Sort(c.want)
			if f := flatTxs(got); !slices.Equal(f, c.want) {
				t.Errorf("liquidityTxs(venues %v, pool ids %v, entry %d): got %v, want %v", c.venues, c.poolIDs, entry, f, c.want)
			}
		})
	}
}

func TestLiquidityRejectsUnknownEntryBlock(t *testing.T) {
	events := []liquidityEvent{lqEv("v2", lpAdd, lqPool, "", "0xa1", 0)}
	if got, err := liquidityTxs(events, set(lqPool), nil, 0); !errors.Is(err, errUnknownEntryBlock) {
		t.Errorf("liquidityTxs(entry 0): got (%v, %v), want an errUnknownEntryBlock error", got, err)
	}
	if got, err := liquidityStats(events, nil, set(lqPool), nil, hAddr(1), 0); !errors.Is(err, errUnknownEntryBlock) {
		t.Errorf("liquidityStats(entry 0): got (%+v, %v), want an errUnknownEntryBlock error", got, err)
	}
}

func TestLiquidityTxSetKinds(t *testing.T) {
	s := liquidityTxSet{lqPool: {"0xa1": lqAddK, "0xb1": {Remove: true, Collect: true}}}
	cases := []struct {
		name      string
		set       liquidityTxSet
		venue, tx string
		want      liquidityTxKinds
	}{
		{"listed add", s, lqPool, "0xa1", lqAddK},
		{"upper-case tx", s, lqPool, "0xA1", lqAddK},
		{"listed remove and collect", s, lqPool, "0xb1", liquidityTxKinds{Remove: true, Collect: true}},
		{"other venue", s, lqPM, "0xa1", liquidityTxKinds{}},
		{"other tx", s, lqPool, "0xa2", liquidityTxKinds{}},
		{"empty tx", liquidityTxSet{lqPool: {"": lqAddK}}, lqPool, "", liquidityTxKinds{}},
		{"nil set", nil, lqPool, "0xa1", liquidityTxKinds{}},
	}
	for _, c := range cases {
		if got := c.set.kinds(c.venue, c.tx); got != c.want {
			t.Errorf("%s: kinds(%s, %s) on %v: got %+v, want %+v", c.name, c.venue, c.tx, c.set, got, c.want)
		}
	}
}

// in: a wallet -> venue transfer is liquidity (an add); out: a venue ->
// wallet transfer is (a removal or a collect).
func TestLiquidityTxKindsDirection(t *testing.T) {
	cases := []struct {
		k       liquidityTxKinds
		in, out bool
	}{
		{liquidityTxKinds{}, false, false},
		{lqAddK, true, false},
		{lqRemoveK, false, true},
		{lqCollectK, false, true},
		{lqAddRemoveK, true, true},
		{liquidityTxKinds{Add: true, Collect: true}, true, true},
	}
	for _, c := range cases {
		if in, out := c.k.in(), c.k.out(); in != c.in || out != c.out {
			t.Errorf("%+v: got in %v, out %v; want %v, %v", c.k, in, out, c.in, c.out)
		}
	}
}

// removalTxs: removals on any venue, not adds or collects alone.
func TestLiquidityRemovalTxs(t *testing.T) {
	s := liquidityTxSet{
		lqPool: {"0xa1": lqAddK, "0xc1": lqCollectK, "0xd1": lqRemoveK, "0xd2": lqAddRemoveK,
			"0xd3": {Remove: true, Collect: true}},
		lqPM: {"0xd4": lqRemoveK},
	}
	var got []string
	for tx := range s.removalTxs() {
		got = append(got, tx)
	}
	slices.Sort(got)
	if want := []string{"0xd1", "0xd2", "0xd3", "0xd4"}; !slices.Equal(got, want) {
		t.Errorf("removalTxs: got %v, want %v", got, want)
	}
	if got := liquidityTxSet(nil).removalTxs(); len(got) != 0 {
		t.Errorf("removalTxs on nil: got %v, want none", got)
	}
}

func TestLiquidityStats(t *testing.T) {
	const entry = 50
	dep := hAddr(1)
	// add at 5; fee collection at 20; removal at 30 through the router; a
	// removal at the entry block; one after it; one on another venue.
	events := []liquidityEvent{
		lqEv("v2", lpAdd, lqPool, "", "0xa1", 5),
		lqEv("v3", lpCollect, lqPool, "", "0xc1", 20),
		lqEv("v2", lpRemove, lqPool, "", "0xd1", 30),
		lqEv("v2", lpRemove, lqPool, "", "0xd2", entry),
		lqEv("v2", lpRemove, lqPool, "", "0xd3", entry+1),
		lqEv("v2", lpRemove, hAddr(77), "", "0xd4", 10),
		lqEv("v4", lpRemove, lqPM, lqOtherPoolID, "0xd5", 10),
	}
	viaRouter := []tokenTransfer{
		inTx(xfer(dep, lqPool, tokens(600), 5, 0), "0xa1"),
		inTx(xfer(lqPool, lqRouter, tokens(100), 30, 0), "0xD1"), // v2 removeLiquidityETH: pool -> router -> deployer
		inTx(xfer(lqRouter, dep, tokens(100), 30, 1), "0xD1"),
	}
	notRemoval := []tokenTransfer{
		inTx(xfer(dep, lqPool, tokens(600), 5, 0), "0xa1"),
		inTx(xfer(lqPool, dep, tokens(1), 5, 1), "0xa1"),       // in the add tx
		inTx(xfer(lqPool, dep, tokens(1), 20, 0), "0xc1"),      // fee collection
		inTx(xfer(lqPool, dep, tokens(1), entry+1, 0), "0xd3"), // after the entry block
		inTx(xfer(hAddr(77), dep, tokens(1), 10, 0), "0xd4"),   // removal on another venue
		inTx(xfer(lqPM, dep, tokens(1), 10, 0), "0xd5"),        // another token's v4 pool
		inTx(xfer(lqPool, dep, new(big.Int), 30, 0), "0xd1"),   // zero value
		inTx(xfer(lqPool, hAddr(2), tokens(1), 30, 1), "0xd1"), // to someone else
		inTx(xfer(lqPool, dep, tokens(1), 31, 0), ""),          // no tx hash
	}
	cases := []struct {
		name          string
		transfers     []tokenTransfer
		deployer      string
		known, remove bool
	}{
		{"deployer removed through the router", viaRouter, dep, true, true},
		{"deployer in mixed case", viaRouter, "0x" + strings.ToUpper(dep[2:]), true, true},
		{"deployer got tokens, but not in a removal", notRemoval, dep, true, false},
		{"deployer unknown", viaRouter, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := liquidityStats(events, c.transfers, set(lqPool, lqPM), set(lqPoolID), c.deployer, entry)
			if err != nil {
				t.Fatalf("liquidityStats: %v", err)
			}
			want := liquiditySummary{Adds: 1, Removes: 2, Collects: 1, RemovedBeforeEntry: true, DeployerKnown: c.known, DeployerRemoved: c.remove}
			if got != want {
				t.Errorf("liquidityStats(deployer %q): got %+v, want %+v", c.deployer, got, want)
			}
		})
	}
	t.Run("only removal is in the entry block", func(t *testing.T) {
		got, err := liquidityStats(events[3:4], nil, set(lqPool), nil, "", entry)
		want := liquiditySummary{Removes: 1}
		if err != nil || got != want {
			t.Errorf("liquidityStats: got (%+v, %v), want %+v", got, err, want)
		}
	})
	t.Run("v3 Burn and Collect in one transaction", func(t *testing.T) {
		evs := []liquidityEvent{
			lqEv("v3", lpRemove, lqPool, "", "0xe1", 30),
			lqEv("v3", lpCollect, lqPool, "", "0xe1", 30),
		}
		trs := []tokenTransfer{inTx(xfer(lqPool, dep, tokens(5), 30, 2), "0xe1")}
		got, err := liquidityStats(evs, trs, set(lqPool), nil, dep, entry)
		want := liquiditySummary{Removes: 1, Collects: 1, RemovedBeforeEntry: true, DeployerKnown: true, DeployerRemoved: true}
		if err != nil || got != want {
			t.Errorf("liquidityStats: got (%+v, %v), want %+v", got, err, want)
		}
	})
	t.Run("no events", func(t *testing.T) {
		got, err := liquidityStats(nil, viaRouter, set(lqPool), nil, dep, entry)
		want := liquiditySummary{DeployerKnown: true}
		if err != nil || got != want {
			t.Errorf("liquidityStats: got (%+v, %v), want %+v", got, err, want)
		}
	})
}

func TestEarlyLiquidityClassify(t *testing.T) {
	pool, pm, a, b := hAddr(50), hAddr(51), hAddr(2), hAddr(3)
	venues := set(pool, pm)
	const addTx, rmTx, colTx, bothTx, noneTx = "0xaa", "0xbb", "0xcc", "0xdd", "0xee"
	lp := liquidityTxSet{pool: {addTx: lqAddK, rmTx: lqRemoveK, colTx: lqCollectK, bothTx: lqAddRemoveK, noneTx: {}}}
	cases := []struct {
		name     string
		from, to string
		v        *big.Int
		tx       string
		want     tradeKind
	}{
		{"add tx: wallet to pool is liquidity", a, pool, tokens(1), addTx, tradeLiquidity},
		{"add tx: pool to wallet is still a buy", pool, a, tokens(1), addTx, tradeBuy},
		{"remove tx: pool to wallet is liquidity", pool, a, tokens(1), rmTx, tradeLiquidity},
		{"remove tx: wallet to pool is still a sell", a, pool, tokens(1), rmTx, tradeSell},
		{"collect tx: pool to wallet is liquidity", pool, a, tokens(1), colTx, tradeLiquidity},
		{"collect tx: wallet to pool is still a sell", a, pool, tokens(1), colTx, tradeSell},
		{"add and remove tx: wallet to pool is liquidity", a, pool, tokens(1), bothTx, tradeLiquidity},
		{"add and remove tx: pool to wallet is liquidity", pool, a, tokens(1), bothTx, tradeLiquidity},
		{"listed tx with no kind: buy", pool, a, tokens(1), noneTx, tradeBuy},
		{"listed tx with no kind: sell", a, pool, tokens(1), noneTx, tradeSell},
		{"upper-case tx hash", pool, a, tokens(1), "0xBB", tradeLiquidity},
		{"swap in another tx", pool, a, tokens(1), "0xab", tradeBuy},
		{"sell in another tx", a, pool, tokens(1), "0xab", tradeSell},
		{"removal tx of another venue", pm, a, tokens(1), bothTx, tradeBuy},
		{"add tx of another venue", a, pm, tokens(1), bothTx, tradeSell},
		{"wallet to wallet in a liquidity tx", a, b, tokens(1), bothTx, tradeNone},
		{"mint to pool in a liquidity tx", zeroAddr, pool, tokens(1), bothTx, tradeNone},
		{"zero value in a liquidity tx", pool, a, new(big.Int), bothTx, tradeNone},
		{"no tx hash", pool, a, tokens(1), "", tradeBuy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := inTx(xfer(c.from, c.to, c.v, 1, 0), c.tx)
			if got := classifyTrade(tr, venues, venues, lp); got != c.want {
				t.Errorf("classifyTrade(%s -> %s, %v, tx %q): got %d, want %d", c.from, c.to, c.v, c.tx, got, c.want)
			}
		})
	}
}

// The #34 v2 launch with its liquidity add marked: the deployer's add is
// neither a sell nor a seller; every buy figure is unchanged.
func TestEarlyLiquidityAddMarked(t *testing.T) {
	// The same add in a transaction whose hash has letters, upper case in the
	// transfer and lower case in the set (as liquidityTxs builds it).
	upper := v2LaunchTransfers()
	upper[1].TxHash = "0xADD1"
	cases := []struct {
		name string
		trs  []tokenTransfer
		lp   liquidityTxSet
	}{
		{"marked add", v2LaunchTransfers(), liquidityTxSet{vPool: {vAddTx: lqAddK}}},
		{"upper-case tx hash in the transfer", upper, liquidityTxSet{vPool: {"0xadd1": lqAddK}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEarlyLP(t, c.trs, set(vPool), set(vPool), c.lp, vLaunchEntry, defaultSniperWindowBlocks)
			if !e.TradesOK || !e.BuysOK || e.FirstBuyBlock != 100 || e.Snipers != 3 || e.FirstBlockBuyers != 2 || e.MaxTxBuyers != 2 {
				t.Errorf("got tradesOK %v, buysOK %v, first buy block %d, snipers %d, first-block buyers %d, max tx buyers %d; want true, true, 100, 3, 2, 2",
					e.TradesOK, e.BuysOK, e.FirstBuyBlock, e.Snipers, e.FirstBlockBuyers, e.MaxTxBuyers)
			}
			f := e.Flow
			if f.Sells != 0 || f.Sellers != 0 || f.Buys != 4 || f.Buyers != 4 || !f.SellBuyRatioOK || f.SellBuyRatio != 0 {
				t.Errorf("flow: got sells %d, sellers %d, buys %d, buyers %d, ratioOK %v, ratio %v; want 0, 0, 4, 4, true, 0",
					f.Sells, f.Sellers, f.Buys, f.Buyers, f.SellBuyRatioOK, f.SellBuyRatio)
			}
			wantBig(t, "sold", f.Sold, new(big.Int))
			wantBig(t, "bought", f.Bought, tokens(140))
			if want := []string{vS1, vS2, vS3, vB4}; !slices.Equal(e.EarlyBuyers, want) {
				t.Errorf("early buyers: got %v, want %v", e.EarlyBuyers, want)
			}
			// Balances do not depend on classification: as without liquidity input.
			wantShare(t, "sniper held share (50 + 40 + 30)", e.SniperHeldShare, 120.0/540)
		})
	}
}

// A removal (pool -> deployer) is not a buy, before or after trading opens.
// Without liquidity input the early removal would be the first buy.
func TestEarlyLiquidityRemovalNotABuy(t *testing.T) {
	for _, block := range []uint64{50, 155} {
		trs := append(v2LaunchTransfers(), inTx(xfer(vPool, vDep, tokens(100), block, 5), "0xf1"))
		lp := liquidityTxSet{vPool: {vAddTx: lqAddK, "0xf1": lqRemoveK}}
		e := mustEarlyLP(t, trs, set(vPool), set(vPool), lp, vLaunchEntry, defaultSniperWindowBlocks)
		if e.FirstBuyBlock != 100 || e.Snipers != 3 || e.Flow.Buys != 4 || e.Flow.Buyers != 4 || e.Flow.Sells != 0 ||
			!slices.Equal(e.EarlyBuyers, []string{vS1, vS2, vS3, vB4}) {
			t.Errorf("removal at %d: got first buy block %d, snipers %d, buys %d, buyers %d, sells %d, early buyers %v; want 100, 3, 4, 4, 0, [s1 s2 s3 b4]",
				block, e.FirstBuyBlock, e.Snipers, e.Flow.Buys, e.Flow.Buyers, e.Flow.Sells, e.EarlyBuyers)
		}
		wantBig(t, "bought", e.Flow.Bought, tokens(140))
		// Pool: 600 - 140 - 100 = 360; circulating 640.
		wantShare(t, "sniper held share", e.SniperHeldShare, 120.0/640)
		if block == 50 {
			old := mustEarly(t, trs, set(vPool), set(vPool), vLaunchEntry, defaultSniperWindowBlocks)
			if old.FirstBuyBlock != 50 || len(old.EarlyBuyers) == 0 || old.EarlyBuyers[0] != vDep {
				t.Errorf("without liquidity input: got first buy block %d, first buyer %v; want 50, the deployer", old.FirstBuyBlock, old.EarlyBuyers)
			}
		}
	}
}

// Liquidity transactions that are not the launch's trades change nothing:
// checkLaunch's expectations hold unchanged.
func TestEarlyLiquidityOtherTxUnchanged(t *testing.T) {
	trs := launchTransfers()
	cases := []struct {
		name string
		lp   liquidityTxSet
	}{
		{"nil", nil},
		{"empty", liquidityTxSet{}},
		{"the mint's tx (not a trade)", liquidityTxSet{lPool: {trs[0].TxHash: lqAddRemoveK}}},
		{"a tx with no transfer", liquidityTxSet{lPool: {"0x" + strings.Repeat("ee", 32): lqAddRemoveK}}},
		{"the bundle tx, on another venue", liquidityTxSet{hAddr(77): {"0xb1": lqAddRemoveK}}},
		// Direction: an add does not hide buys, a removal does not hide sells.
		{"the bundle tx, with an add", liquidityTxSet{lPool: {"0xb1": lqAddK}}},
		{"s1's sell tx, with a removal", liquidityTxSet{lPool: {trs[6].TxHash: lqRemoveK}}},
		{"s1's sell tx, with a collect", liquidityTxSet{lPool: {trs[6].TxHash: lqCollectK}}},
	}
	if trs[6].From != lS1 || trs[6].To != lPool {
		t.Fatalf("launchTransfers()[6]: got %s -> %s, want s1's sell", trs[6].From, trs[6].To)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkLaunch(t, mustEarlyLP(t, trs, set(lPool), set(lPool), c.lp, launchEntry, defaultSniperWindowBlocks))
		})
	}
}

// A liquidity add with a buy in the same transaction on the same venue: the
// add (wallet -> pool) is liquidity, not a sell; the buy (pool -> wallet)
// stays a buy.
func TestEarlyLiquidityAddAndSwapSameTx(t *testing.T) {
	trs := []tokenTransfer{
		xfer(zeroAddr, vDep, tokens(1000), 1, 0),
		inTx(xfer(vDep, vPool, tokens(600), 5, 0), "0xab"),
		inTx(xfer(vPool, vS1, tokens(50), 5, 1), "0xab"),
		xfer(vPool, vS2, tokens(40), 7, 0),
	}
	e := mustEarlyLP(t, trs, set(vPool), set(vPool), liquidityTxSet{vPool: {"0xab": lqAddK}}, 10, defaultSniperWindowBlocks)
	f := e.Flow
	if e.FirstBuyBlock != 5 || f.Buys != 2 || f.Buyers != 2 || f.Sells != 0 || f.Sellers != 0 ||
		e.Snipers != 2 || e.FirstBlockBuyers != 1 || !slices.Equal(e.EarlyBuyers, []string{vS1, vS2}) {
		t.Errorf("got first buy block %d, buys %d, buyers %d, sells %d, sellers %d, snipers %d, first-block buyers %d, early buyers %v; want 5, 2, 2, 0, 0, 2, 1, [%s %s]",
			e.FirstBuyBlock, f.Buys, f.Buyers, f.Sells, f.Sellers, e.Snipers, e.FirstBlockBuyers, e.EarlyBuyers, vS1, vS2)
	}
	wantBig(t, "bought", f.Bought, tokens(90))
	wantBig(t, "sold", f.Sold, new(big.Int))
}

// A bundled launch: the deployer adds liquidity and three wallets buy in the
// same transaction, then one more buys. The bundle buyers stay snipers,
// first-block buyers and max-tx buyers, exactly as without liquidity input;
// only the add stops reading as a sell.
func TestEarlyLiquidityBundledLaunch(t *testing.T) {
	const bundle = "0xbd"
	b5 := hAddr(15)
	trs := []tokenTransfer{
		xfer(zeroAddr, vDep, tokens(1000), 1, 0),
		inTx(xfer(vDep, vPool, tokens(600), 100, 0), bundle), // liquidity add
		inTx(xfer(vPool, vS1, tokens(50), 100, 1), bundle),
		inTx(xfer(vPool, vS2, tokens(40), 100, 2), bundle),
		inTx(xfer(vPool, vS3, tokens(30), 100, 3), bundle),
		xfer(vPool, vB4, tokens(20), 120, 0),
		xfer(vPool, b5, tokens(10), 200, 0), // outside the sniper window
	}
	const entry = 210
	lp := liquidityTxSet{vPool: {bundle: lqAddK}}
	for _, c := range []struct {
		name           string
		lp             liquidityTxSet
		sells, sellers int
		sold           *big.Int
	}{
		{"add marked", lp, 0, 0, new(big.Int)},
		{"without liquidity input: the add reads as a sell", nil, 1, 1, tokens(600)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := mustEarlyLP(t, trs, set(vPool), set(vPool), c.lp, entry, defaultSniperWindowBlocks)
			if !e.BuysOK || e.FirstBuyBlock != 100 || e.Snipers != 4 || e.FirstBlockBuyers != 3 || e.MaxTxBuyers != 3 {
				t.Errorf("got buysOK %v, first buy block %d, snipers %d, first-block buyers %d, max tx buyers %d; want true, 100, 4, 3, 3",
					e.BuysOK, e.FirstBuyBlock, e.Snipers, e.FirstBlockBuyers, e.MaxTxBuyers)
			}
			f := e.Flow
			if f.Buys != 5 || f.Buyers != 5 || f.Sells != c.sells || f.Sellers != c.sellers {
				t.Errorf("flow: got buys %d, buyers %d, sells %d, sellers %d; want 5, 5, %d, %d",
					f.Buys, f.Buyers, f.Sells, f.Sellers, c.sells, c.sellers)
			}
			wantBig(t, "bought", f.Bought, tokens(150))
			wantBig(t, "sold", f.Sold, c.sold)
			if want := []string{vS1, vS2, vS3, vB4, b5}; !slices.Equal(e.EarlyBuyers, want) {
				t.Errorf("early buyers: got %v, want %v", e.EarlyBuyers, want)
			}
			// Pool: 600 - 150 = 450; circulating 550; snipers hold 50 + 40 + 30 + 20.
			wantShare(t, "sniper held share", e.SniperHeldShare, 140.0/550)
		})
	}
}

// A removal in which another wallet also sells to the pool: the removal
// (pool -> deployer) is liquidity, the sell (wallet -> pool) stays a sell.
func TestEarlyLiquidityRemovalTxKeepsSell(t *testing.T) {
	const rm = "0xf2"
	trs := append(v2LaunchTransfers(),
		inTx(xfer(vPool, vDep, tokens(100), 155, 0), rm),
		inTx(xfer(vS1, vPool, tokens(20), 155, 1), rm))
	lp := liquidityTxSet{vPool: {vAddTx: lqAddK, rm: lqRemoveK}}
	e := mustEarlyLP(t, trs, set(vPool), set(vPool), lp, vLaunchEntry, defaultSniperWindowBlocks)
	f := e.Flow
	if f.Buys != 4 || f.Buyers != 4 || f.Sells != 1 || f.Sellers != 1 || e.FirstBuyBlock != 100 ||
		!slices.Equal(e.EarlyBuyers, []string{vS1, vS2, vS3, vB4}) {
		t.Errorf("got buys %d, buyers %d, sells %d, sellers %d, first buy block %d, early buyers %v; want 4, 4, 1 (s1's), 1, 100, [s1 s2 s3 b4]",
			f.Buys, f.Buyers, f.Sells, f.Sellers, e.FirstBuyBlock, e.EarlyBuyers)
	}
	wantBig(t, "bought", f.Bought, tokens(140))
	wantBig(t, "sold", f.Sold, tokens(20))
}

// A transaction with an add and a removal on the same venue (a rebalance):
// both directions are liquidity, so the deployer is neither a buyer nor a
// seller.
func TestEarlyLiquidityAddAndRemoveSameTx(t *testing.T) {
	const rebalance = "0xf3"
	trs := append(v2LaunchTransfers(),
		inTx(xfer(vPool, vDep, tokens(100), 155, 0), rebalance),
		inTx(xfer(vDep, vPool, tokens(80), 155, 1), rebalance))
	lp := liquidityTxSet{vPool: {vAddTx: lqAddK, rebalance: lqAddRemoveK}}
	e := mustEarlyLP(t, trs, set(vPool), set(vPool), lp, vLaunchEntry, defaultSniperWindowBlocks)
	f := e.Flow
	if f.Buys != 4 || f.Buyers != 4 || f.Sells != 0 || f.Sellers != 0 || slices.Contains(e.EarlyBuyers, vDep) {
		t.Errorf("got buys %d, buyers %d, sells %d, sellers %d, early buyers %v; want 4, 4, 0, 0, without the deployer",
			f.Buys, f.Buyers, f.Sells, f.Sellers, e.EarlyBuyers)
	}
	// With only the add marked, the removal half reads as the deployer's buy.
	addOnly := liquidityTxSet{vPool: {vAddTx: lqAddK, rebalance: lqAddK}}
	if e := mustEarlyLP(t, trs, set(vPool), set(vPool), addOnly, vLaunchEntry, defaultSniperWindowBlocks); e.Flow.Buys != 5 || e.Flow.Sells != 0 {
		t.Errorf("add only: got buys %d, sells %d; want 5 (the deployer's), 0", e.Flow.Buys, e.Flow.Sells)
	}
}

// A v3 Collect-only transaction (fees paid out, no Burn), built from the raw
// log: the pool -> deployer payout is liquidity, not a buy, and a sell to the
// pool in the same transaction stays a sell. Without liquidity input the
// payout, before trading opens, would be the first buy.
func TestEarlyLiquidityCollectOnly(t *testing.T) {
	l := v3CollectLog() // emitted by lqPool (== vPool) at block 0x10 in tx 0xABCD01
	ev, err := decodeLiquidityLog(l)
	if err != nil {
		t.Fatalf("decodeLiquidityLog: %v", err)
	}
	add := lqEv("v2", lpAdd, vPool, "", vAddTx, 5) // the launch's add
	lp, err := liquidityTxs([]liquidityEvent{add, ev}, set(vPool), nil, vLaunchEntry)
	if err != nil {
		t.Fatalf("liquidityTxs: %v", err)
	}
	seller := hAddr(20)
	trs := append(v2LaunchTransfers(),
		xfer(vDep, seller, tokens(5), 2, 0), // gives the seller something to sell
		inTx(xfer(vPool, vDep, tokens(3), 0x10, 4), l.TxHash),
		inTx(xfer(seller, vPool, tokens(2), 0x10, 5), l.TxHash))
	e := mustEarlyLP(t, trs, set(vPool), set(vPool), lp, vLaunchEntry, defaultSniperWindowBlocks)
	f := e.Flow
	if e.FirstBuyBlock != 100 || f.Buys != 4 || f.Buyers != 4 || f.Sells != 1 || f.Sellers != 1 || slices.Contains(e.EarlyBuyers, vDep) {
		t.Errorf("got first buy block %d, buys %d, buyers %d, sells %d, sellers %d, early buyers %v; want 100, 4, 4, 1 (the seller's), 1, without the deployer",
			e.FirstBuyBlock, f.Buys, f.Buyers, f.Sells, f.Sellers, e.EarlyBuyers)
	}
	wantBig(t, "sold", f.Sold, tokens(2))
	if old := mustEarly(t, trs, set(vPool), set(vPool), vLaunchEntry, defaultSniperWindowBlocks); old.FirstBuyBlock != 0x10 {
		t.Errorf("without liquidity input: got first buy block %d, want %d (the fee payout)", old.FirstBuyBlock, 0x10)
	}
}

// Only a liquidity add before the entry block: no trades at all.
func TestEarlyLiquidityOnly(t *testing.T) {
	trs := v2LaunchTransfers()[:2]
	e := mustEarlyLP(t, trs, set(vPool), set(vPool), liquidityTxSet{vPool: {vAddTx: lqAddK}}, 10, defaultSniperWindowBlocks)
	if e.TradesOK || e.BuysOK || e.Flow.Sells != 0 || e.Flow.Sellers != 0 {
		t.Errorf("got tradesOK %v, buysOK %v, sells %d, sellers %d; want false, false, 0, 0", e.TradesOK, e.BuysOK, e.Flow.Sells, e.Flow.Sellers)
	}
}

// End to end from raw logs: decode, skip what is not a liquidity event, build
// the set and classify. On the v4 PoolManager only the token's own pool id
// counts: a sell in a transaction with another pool's ModifyLiquidity stays a
// sell.
func TestEarlyLiquidityFromLogs(t *testing.T) {
	a, b := hAddr(2), hAddr(3)
	const addTx, otherTx = "0x0a", "0x0b"
	withTx := func(l rpcLog, tx string, block uint64) rpcLog {
		l.TxHash, l.BlockNumber = tx, hexU64(block)
		return l
	}
	swap := lqLog(lqPM, topicSwapV4, []string{lqPoolID, addrTopic(lqRouter)}, wInt(1))
	logs := []rpcLog{
		withTx(v4ModifyLog(lqPoolID, big.NewInt(10)), addTx, 5),
		withTx(swap, otherTx, 8), // not a liquidity event: skipped
		withTx(v4ModifyLog(lqOtherPoolID, big.NewInt(-10)), otherTx, 8),
	}
	var events []liquidityEvent
	for _, l := range logs {
		ev, err := decodeLiquidityLog(l)
		if errors.Is(err, errBadLiquidityLog) {
			continue
		}
		if err != nil {
			t.Fatalf("decodeLiquidityLog: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("decoded %d events, want 2 (the swap skipped)", len(events))
	}
	lp, err := liquidityTxs(events, set(lqPM), set(lqPoolID), 10)
	if err != nil {
		t.Fatalf("liquidityTxs: %v", err)
	}
	trs := []tokenTransfer{
		xfer(zeroAddr, vDep, tokens(1000), 1, 0),
		inTx(xfer(vDep, lqPM, tokens(600), 5, 0), addTx), // liquidity add on the token's pool
		xfer(lqPM, a, tokens(50), 6, 0),
		xfer(lqPM, b, tokens(40), 7, 0),
		inTx(xfer(a, lqPM, tokens(10), 8, 0), otherTx), // a sell next to another pool's removal
	}
	e := mustEarlyLP(t, trs, set(lqPM), set(lqPM), lp, 10, defaultSniperWindowBlocks)
	f := e.Flow
	if f.Buys != 2 || f.Sells != 1 || f.Sellers != 1 || e.FirstBuyBlock != 6 {
		t.Errorf("got buys %d, sells %d, sellers %d, first buy block %d; want 2, 1 (a's sell, not the add), 1, 6",
			f.Buys, f.Sells, f.Sellers, e.FirstBuyBlock)
	}
	wantBig(t, "sold", f.Sold, tokens(10))
}
