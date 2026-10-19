package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
)

// hAddr returns a distinct lowercase test address ending in n.
func hAddr(n int) string { return fmt.Sprintf("0x%040x", 0xa0000+n) }

// tokens returns n whole tokens with 18 decimals.
func tokens(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

func xfer(from, to string, v *big.Int, block, idx uint64) tokenTransfer {
	return tokenTransfer{From: from, To: to, Value: v, Block: block, LogIndex: idx, TxHash: fmt.Sprintf("0x%064x", block*1000+idx)}
}

func mustReplay(t *testing.T, trs []tokenTransfer, entry uint64) holderReplay {
	t.Helper()
	r, err := replayTransfers(trs, entry)
	if err != nil {
		t.Fatalf("replayTransfers(entry %d): %v", entry, err)
	}
	return r
}

func wantBig(t *testing.T, what string, got, want *big.Int) {
	t.Helper()
	if got == nil || got.Cmp(want) != 0 {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func wantShare(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s: got %.15f, want %.15f", what, got, want)
	}
}

// transferLog builds a raw Transfer log as the node returns it.
func transferLog(from, to string, v *big.Int, block, idx uint64) rpcLog {
	return rpcLog{
		Address:     hAddr(999),
		Topics:      []string{topicTransfer, addrTopic(from), addrTopic(to)},
		Data:        ret(w32(v)),
		BlockNumber: hexU64(block),
		TxHash:      "0xABCDEF",
		LogIndex:    hexU64(idx),
	}
}

func TestDecodeTransferLog(t *testing.T) {
	from, to := hAddr(1), hAddr(2)
	v := new(big.Int).Lsh(big.NewInt(1), 255) // top bit set: still unsigned
	l := transferLog(from, to, v, 0x1234, 7)
	l.Topics[0] = "0x" + strings.ToUpper(l.Topics[0][2:]) // case must not matter
	got, err := decodeTransferLog(l)
	if err != nil {
		t.Fatalf("decodeTransferLog: %v", err)
	}
	want := tokenTransfer{From: from, To: to, Value: v, Block: 0x1234, LogIndex: 7, TxHash: "0xabcdef"}
	if got.From != want.From || got.To != want.To || got.Value.Cmp(want.Value) != 0 ||
		got.Block != want.Block || got.LogIndex != want.LogIndex || got.TxHash != want.TxHash {
		t.Errorf("decodeTransferLog: got %+v, want %+v", got, want)
	}
}

func TestDecodeTransferLogMalformed(t *testing.T) {
	good := func() rpcLog { return transferLog(hAddr(1), hAddr(2), tokens(5), 100, 1) }
	cases := []struct {
		name string
		edit func(l *rpcLog)
	}{
		{"no topics", func(l *rpcLog) { l.Topics = nil }},
		{"two topics", func(l *rpcLog) { l.Topics = l.Topics[:2] }},
		{"four topics (ERC-721)", func(l *rpcLog) { l.Topics = append(l.Topics, l.Topics[2]); l.Data = "0x" }},
		{"other event", func(l *rpcLog) { l.Topics[0] = topicSyncV2 }},
		{"short from topic", func(l *rpcLog) { l.Topics[1] = "0x" + strings.Repeat("11", 20) }},
		{"non-hex to topic", func(l *rpcLog) { l.Topics[2] = "0x" + strings.Repeat("zz", 32) }},
		{"dirty address padding", func(l *rpcLog) { l.Topics[1] = "0x01" + l.Topics[1][4:] }},
		{"empty data", func(l *rpcLog) { l.Data = "0x" }},
		{"data 31 bytes", func(l *rpcLog) { l.Data = "0x" + strings.Repeat("00", 31) }},
		{"data 64 bytes", func(l *rpcLog) { l.Data = l.Data + strings.Repeat("00", 32) }},
		{"odd-length data", func(l *rpcLog) { l.Data = l.Data + "0" }},
		{"missing block number", func(l *rpcLog) { l.BlockNumber = "" }},
		{"bad block number", func(l *rpcLog) { l.BlockNumber = "0xzz" }},
		{"bad log index", func(l *rpcLog) { l.LogIndex = "12" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := good()
			c.edit(&l)
			got, err := decodeTransferLog(l)
			if !errors.Is(err, errBadTransferLog) {
				t.Errorf("decodeTransferLog(%+v): got (%+v, %v), want an errBadTransferLog error", l, got, err)
			}
		})
	}
}

// Mint to the deployer, who hands out tokens; supply is minted minus burned.
func TestReplayMintAndDistribution(t *testing.T) {
	dep, a, b := hAddr(1), hAddr(2), hAddr(3)
	r := mustReplay(t, []tokenTransfer{
		xfer(zeroAddr, dep, tokens(1000), 10, 0),
		xfer(dep, a, tokens(300), 11, 0),
		xfer(dep, b, tokens(200), 11, 1),
		xfer(a, b, tokens(300), 12, 0), // a ends at zero: left out of the map
	}, 12)
	wantBig(t, "minted", r.Minted, tokens(1000))
	wantBig(t, "burned", r.Burned, new(big.Int))
	wantBig(t, "supply", r.Supply, tokens(1000))
	wantBig(t, "deployer", r.Balances[dep], tokens(500))
	wantBig(t, "b", r.Balances[b], tokens(500))
	if _, ok := r.Balances[a]; ok || len(r.Balances) != 2 {
		t.Errorf("balances: got %v, want only deployer and b (a is at zero)", r.Balances)
	}
	if r.Incomplete || r.Applied != 4 {
		t.Errorf("got incomplete %v, applied %d; want false, 4", r.Incomplete, r.Applied)
	}
}

func TestReplayIgnoresTransfersAfterEntry(t *testing.T) {
	dep, a := hAddr(1), hAddr(2)
	trs := []tokenTransfer{
		xfer(zeroAddr, dep, tokens(100), 10, 0),
		xfer(dep, a, tokens(40), 20, 3), // at the entry block: applied
		xfer(dep, a, tokens(60), 21, 0), // after: ignored
		xfer(zeroAddr, a, tokens(5), 22, 0),
	}
	r := mustReplay(t, trs, 20)
	wantBig(t, "supply", r.Supply, tokens(100))
	wantBig(t, "deployer", r.Balances[dep], tokens(60))
	wantBig(t, "a", r.Balances[a], tokens(40))
	if r.Applied != 2 {
		t.Errorf("applied: got %d, want 2", r.Applied)
	}
}

func TestReplayBurns(t *testing.T) {
	dep, a := hAddr(1), hAddr(2)
	deadMixed := "0x" + strings.ToUpper(deadAddr[2:]) // case must not matter
	r := mustReplay(t, []tokenTransfer{
		xfer(zeroAddr, dep, tokens(1000), 1, 0),
		xfer(dep, zeroAddr, tokens(100), 2, 0),
		xfer(dep, deadMixed, tokens(50), 2, 1),
		xfer(dep, a, tokens(10), 3, 0),
		xfer(zeroAddr, deadAddr, tokens(7), 4, 0), // minted straight to dead: net zero
	}, 4)
	wantBig(t, "minted", r.Minted, tokens(1007))
	wantBig(t, "burned", r.Burned, tokens(157))
	wantBig(t, "supply", r.Supply, tokens(850))
	for _, burn := range []string{zeroAddr, deadAddr} {
		if b, ok := r.Balances[burn]; ok {
			t.Errorf("balance of burn address %s: got %v, want none", burn, b)
		}
	}
	wantBig(t, "deployer", r.Balances[dep], tokens(840))
	wantBig(t, "a", r.Balances[a], tokens(10))
}

// The look-back cap cut off the mint: the first holder only ever spends.
func TestReplayIncompleteHistoryFlagged(t *testing.T) {
	dep, a, b := hAddr(1), hAddr(2), hAddr(3)
	r := mustReplay(t, []tokenTransfer{
		xfer(dep, a, tokens(100), 5, 0),
		xfer(a, b, tokens(30), 6, 0),
		xfer(b, dep, tokens(10), 7, 0), // dep is back up to -90: still flagged
	}, 10)
	if !r.Incomplete {
		t.Fatal("incomplete: got false, want true (the deployer spent tokens it never received)")
	}
	if !slices.Equal(r.Negative, []string{dep}) {
		t.Errorf("negative: got %v, want [%s]", r.Negative, dep)
	}
	wantBig(t, "deployer (not clamped)", r.Balances[dep], tokens(-90))
	wantBig(t, "supply", r.Supply, new(big.Int))
	s := holderStats(r, nil, nil, dep)
	if !s.Incomplete || s.SharesOK {
		t.Errorf("stats: got incomplete %v, sharesOK %v; want true, false (circulating is 0)", s.Incomplete, s.SharesOK)
	}
}

// Contract: any input order; a sorted copy is replayed and the input is left
// as it was. In input order the spend would come before its funding.
func TestReplaySortsUnorderedInput(t *testing.T) {
	dep, a, b := hAddr(1), hAddr(2), hAddr(3)
	in := []tokenTransfer{
		xfer(a, b, tokens(5), 12, 0),
		xfer(dep, a, tokens(10), 11, 4),
		xfer(dep, a, tokens(1), 11, 2),
		xfer(zeroAddr, dep, tokens(100), 11, 1),
		xfer(a, b, tokens(1), 30, 0), // after the entry block
	}
	orig := slices.Clone(in)
	r := mustReplay(t, in, 12)
	if r.Incomplete {
		t.Errorf("incomplete: got true (negative %v), want false once sorted", r.Negative)
	}
	wantBig(t, "a", r.Balances[a], tokens(6))
	wantBig(t, "b", r.Balances[b], tokens(5))
	if r.Applied != 4 {
		t.Errorf("applied: got %d, want 4", r.Applied)
	}
	for i := range in {
		if in[i].Block != orig[i].Block || in[i].LogIndex != orig[i].LogIndex {
			t.Fatalf("input[%d] moved: got block %d log %d, want block %d log %d",
				i, in[i].Block, in[i].LogIndex, orig[i].Block, orig[i].LogIndex)
		}
	}
}

// A self-transfer nets to zero, but it is debited before it is credited, so a
// sender without enough balance is still flagged.
func TestReplaySelfTransferInsufficientBalanceFlagged(t *testing.T) {
	a := hAddr(2)
	r := mustReplay(t, []tokenTransfer{
		xfer(zeroAddr, a, tokens(5), 1, 0),
		xfer(a, a, tokens(10), 2, 0),
	}, 2)
	if !r.Incomplete || !slices.Equal(r.Negative, []string{a}) {
		t.Errorf("got incomplete %v, negative %v; want true, [%s] (a sent 10 holding 5)", r.Incomplete, r.Negative, a)
	}
	wantBig(t, "a", r.Balances[a], tokens(5))
}

func TestReplayRejectsUnknownEntryBlock(t *testing.T) {
	trs := []tokenTransfer{xfer(zeroAddr, hAddr(1), tokens(1), 0, 0)}
	got, err := replayTransfers(trs, 0)
	if !errors.Is(err, errUnknownEntryBlock) {
		t.Errorf("replayTransfers(entry 0): got (%+v, %v), want an errUnknownEntryBlock error", got, err)
	}
}

func TestHexUint(t *testing.T) {
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"0x0", 0, false},
		{"0x1a", 26, false},
		{"0xffffffffffffffff", math.MaxUint64, false},
		{"0x", 0, true},
		{"", 0, true},
		{"1a", 0, true},
		{"0xzz", 0, true},
		{"0x10000000000000000", 0, true}, // past uint64
	}
	for _, c := range cases {
		got, err := hexUint(c.in)
		// On error the value is not used (ParseUint returns its max on overflow).
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("hexUint(%q): got (%d, %v), want (%d, error %v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestReplayRejectsBadValue(t *testing.T) {
	for _, v := range []*big.Int{nil, big.NewInt(-1)} {
		if _, err := replayTransfers([]tokenTransfer{xfer(zeroAddr, hAddr(1), v, 1, 0)}, 1); err == nil {
			t.Errorf("replayTransfers(value %v): got no error, want one", v)
		}
	}
}

// Five holders, a pool, the PoolManager and the token itself; pool and
// PoolManager balances leave circulating and the holder count.
func TestHolderStatsExclusionsAndTopShares(t *testing.T) {
	dep, pool, pm, token := hAddr(1), hAddr(50), hAddr(51), hAddr(52)
	h := []string{hAddr(2), hAddr(3), hAddr(4), hAddr(5), hAddr(6)}
	trs := []tokenTransfer{
		xfer(zeroAddr, dep, tokens(1000), 1, 0),
		xfer(dep, pool, tokens(500), 2, 0),
		xfer(dep, pm, tokens(100), 2, 1),
		xfer(dep, token, tokens(50), 2, 2),
		xfer(dep, deadAddr, tokens(50), 2, 3),
		xfer(pool, h[0], tokens(100), 3, 0),
		xfer(pool, h[1], tokens(80), 3, 1),
		xfer(pool, h[2], tokens(60), 3, 2),
		xfer(pool, h[3], tokens(40), 3, 3),
		xfer(pool, h[4], tokens(20), 3, 4),
	}
	r := mustReplay(t, trs, 3)
	excluded := map[string]bool{pm: true, token: true, zeroAddr: true, deadAddr: true}
	excluded["0x"+strings.ToUpper(pool[2:])] = true // case must not matter
	excluded[h[4]] = false                          // a false entry is not an exclusion
	s := holderStats(r, trs, excluded, dep)
	// supply 950; excluded: pool 200, pm 100, token 50 -> circulating 600
	wantBig(t, "circulating", s.Circulating, tokens(600))
	if s.Holders != 6 || !s.SharesOK {
		t.Fatalf("got holders %d, sharesOK %v; want 6 (deployer + 5), true", s.Holders, s.SharesOK)
	}
	wantShare(t, "top1 (deployer 300)", s.Top1, 300.0/600)
	wantShare(t, "top5", s.Top5, (300.0+100+80+60+40)/600)
	wantShare(t, "top10 (fewer than 10 holders: all of them)", s.Top10, 1)
	wantShare(t, "deployer share", s.DeployerShare, 0.5)
	if !s.DeployerKnown || !s.DeployerShareOK || !s.DeployerSold {
		t.Errorf("got deployerKnown %v, deployerShareOK %v, deployerSold %v; want true, true, true (sent to the pool)",
			s.DeployerKnown, s.DeployerShareOK, s.DeployerSold)
	}
}

func TestHolderStatsFewerThanNAndTies(t *testing.T) {
	a, b, c := hAddr(2), hAddr(3), hAddr(4)
	trs := []tokenTransfer{
		xfer(zeroAddr, a, tokens(10), 1, 0),
		xfer(zeroAddr, b, tokens(10), 1, 1),
		xfer(zeroAddr, c, tokens(10), 1, 2),
	}
	s := holderStats(mustReplay(t, trs, 1), trs, nil, "")
	if s.Holders != 3 {
		t.Errorf("holders: got %d, want 3", s.Holders)
	}
	wantShare(t, "top1 (three-way tie)", s.Top1, 1.0/3)
	wantShare(t, "top5 (3 holders)", s.Top5, 1)
	wantShare(t, "top10 (3 holders)", s.Top10, 1)
}

func TestHolderStatsDeployer(t *testing.T) {
	dep, pool, a := hAddr(1), hAddr(50), hAddr(2)
	mint := xfer(zeroAddr, dep, tokens(100), 1, 0)
	toA := xfer(dep, a, tokens(10), 5, 0)
	depUpper := "0x" + strings.ToUpper(dep[2:])
	cases := []struct {
		name        string
		trs         []tokenTransfer
		deployer    string
		excludeDep  bool // also put the deployer (checksummed) in the excluded set
		wantKnown   bool
		wantShareOK bool
		wantSold    bool
		wantShare   float64
	}{
		{"never sent", []tokenTransfer{mint}, dep, false, true, true, false, 1},
		{"sent to a wallet", []tokenTransfer{mint, toA}, dep, false, true, true, true, 0.9},
		{"sent to the excluded pool", []tokenTransfer{mint, xfer(dep, pool, tokens(10), 5, 0)}, dep, false, true, true, true, 1},
		{"sent at the entry block", []tokenTransfer{mint, xfer(dep, a, tokens(10), 9, 0)}, dep, false, true, true, true, 0.9},
		{"sent after the entry block", []tokenTransfer{mint, xfer(dep, a, tokens(10), 10, 0)}, dep, false, true, true, false, 1},
		{"burned only", []tokenTransfer{mint, xfer(dep, deadAddr, tokens(10), 5, 0), xfer(dep, zeroAddr, tokens(10), 5, 1)}, dep, false, true, true, false, 1},
		{"zero-value transfer", []tokenTransfer{mint, xfer(dep, a, new(big.Int), 5, 0)}, dep, false, true, true, false, 1},
		{"to itself", []tokenTransfer{mint, xfer(dep, dep, tokens(10), 5, 0)}, dep, false, true, true, false, 1},
		{"checksummed deployer", []tokenTransfer{mint, toA}, depUpper, false, true, true, true, 0.9},
		{"no deployer", []tokenTransfer{mint, toA}, "", false, false, false, false, 0},
		// Excluded: circulating is a's 10 only; the deployer's 90 over it
		// would be 9, so the share is withheld. Sold still works.
		{"deployer excluded", []tokenTransfer{mint, toA}, dep, true, true, false, true, 0},
		{"deployer known, zero circulating", []tokenTransfer{xfer(zeroAddr, pool, tokens(100), 1, 0)}, dep, false, true, false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustReplay(t, c.trs, 9)
			excluded := map[string]bool{pool: true}
			if c.excludeDep {
				excluded[depUpper] = true // case must not matter
			}
			s := holderStats(r, c.trs, excluded, c.deployer)
			if s.DeployerKnown != c.wantKnown || s.DeployerShareOK != c.wantShareOK || s.DeployerSold != c.wantSold {
				t.Errorf("deployer %q (excluded %v): got known %v, shareOK %v, sold %v; want %v, %v, %v",
					c.deployer, c.excludeDep, s.DeployerKnown, s.DeployerShareOK, s.DeployerSold, c.wantKnown, c.wantShareOK, c.wantSold)
			}
			wantShare(t, "deployer share", s.DeployerShare, c.wantShare)
		})
	}
}

func TestHolderStatsZeroCirculating(t *testing.T) {
	pool, a := hAddr(50), hAddr(2)
	cases := []struct {
		name string
		trs  []tokenTransfer
	}{
		{"no transfers", nil},
		{"everything in the pool", []tokenTransfer{xfer(zeroAddr, pool, tokens(100), 1, 0)}},
		{"everything burned", []tokenTransfer{xfer(zeroAddr, a, tokens(5), 1, 0), xfer(a, deadAddr, tokens(5), 2, 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := holderStats(mustReplay(t, c.trs, 5), c.trs, map[string]bool{pool: true}, a)
			if s.SharesOK || s.DeployerShareOK || s.Holders != 0 || s.Top1 != 0 || s.Top5 != 0 || s.Top10 != 0 || s.DeployerShare != 0 {
				t.Errorf("got %+v, want no holders, sharesOK and deployerShareOK false and all shares 0", s)
			}
			for _, f := range []float64{s.Top1, s.Top5, s.Top10, s.DeployerShare} {
				if math.IsNaN(f) || math.IsInf(f, 0) {
					t.Errorf("share %v: want a finite 0", f)
				}
			}
		})
	}
}

// 18-decimal tokens with a 3e27 base-unit balance per holder: the totals are
// far past uint64 and float64 exactness; supply must stay exact.
func TestHolderStatsHugeValues(t *testing.T) {
	big3e27, _ := new(big.Int).SetString("3000000000000000000000000001", 10) // odd: not a float64
	var trs []tokenTransfer
	for i := range 5 {
		v := new(big.Int).Mul(big3e27, big.NewInt(int64(i+1)))
		trs = append(trs, xfer(zeroAddr, hAddr(i+1), v, 1, uint64(i)))
	}
	maxWord := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	trs = append(trs, xfer(zeroAddr, hAddr(50), maxWord, 1, 9)) // a pool holding 2^256-1
	r := mustReplay(t, trs, 1)
	wantSupply := new(big.Int).Add(new(big.Int).Mul(big3e27, big.NewInt(15)), maxWord)
	wantBig(t, "supply", r.Supply, wantSupply)
	s := holderStats(r, trs, map[string]bool{hAddr(50): true}, hAddr(5))
	wantBig(t, "circulating", s.Circulating, new(big.Int).Mul(big3e27, big.NewInt(15)))
	wantShare(t, "top1", s.Top1, 5.0/15)
	wantShare(t, "top5", s.Top5, 1)
	wantShare(t, "deployer share", s.DeployerShare, 5.0/15)
}
