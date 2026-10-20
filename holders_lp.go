package main

// Liquidity (LP) events on a token's pools, decoded from raw logs, so that
// earlyTradeStats can tell a liquidity add or removal apart from a sell or a
// buy, and holderStats' deployer-sold check (deployerSoldBefore) a liquidity
// add apart from a sale. Pure, like holders.go: the caller reads the logs and
// passes them in.
//
// Events decoded:
//   - Uniswap v2 pair: Mint (add), Burn (remove);
//   - Uniswap v3 pool: Mint (add), Burn (remove; a Burn of 0 liquidity is the
//     fee "poke" the position manager sends before a Collect, so it is a
//     collect), Collect (fees, or the tokens of an earlier Burn);
//   - Uniswap v4 PoolManager: ModifyLiquidity, by the sign of liquidityDelta
//     (> 0 add, < 0 remove, 0 fee collection only).
//
// A Pons bonding curve has no LP events; its graduation (curve -> pool) is a
// venue-to-venue transfer and was never a trade.

import (
	"errors"
	"fmt"
	"strings"
)

var errBadLiquidityLog = errors.New("malformed liquidity log")

// liquidityKind says what a liquidity event did to the pool's liquidity.
type liquidityKind int

const (
	lpAdd     liquidityKind = iota + 1
	lpRemove                // tokens leave the pool (v3: on the Collect that follows)
	lpCollect               // fees only, or (v3) the tokens of an earlier Burn
)

// liquidityEvent is one decoded liquidity event. Addresses, the pool id and
// the tx hash are lowercase 0x hex.
type liquidityEvent struct {
	Protocol string // "v2", "v3" or "v4"
	Kind     liquidityKind
	// Venue is the address that emitted the event: the v2 pair, the v3 pool
	// or the v4 PoolManager (the same addresses earlyTradeStats calls venues).
	Venue string
	// PoolID is the v4 pool id (topic 1); "" for v2 and v3.
	PoolID string
	// Owner is the event's own actor: v2 sender, v3 owner, v4 sender. On real
	// chains it is almost never the wallet: the v2 router, the v3
	// NonfungiblePositionManager, the v4 PositionManager. Nothing here uses it
	// to identify the deployer.
	Owner    string
	Block    uint64
	LogIndex uint64
	TxHash   string
}

// liquidityLogSpec is the shape of one liquidity event's log.
type liquidityLogSpec struct {
	protocol   string
	topics     int // topic0 included
	words      int // 32-byte data words
	ownerTopic int // the topic holding Owner
	kind       func(data []byte) liquidityKind
}

// liquidityLogSpecs maps topic0 to its event. v3 Burn and Collect have the
// same shape, so only topic0 tells them apart.
var liquidityLogSpecs = map[string]liquidityLogSpec{
	// Mint(address indexed sender, uint256 amount0, uint256 amount1)
	topicMintV2: {"v2", 2, 2, 1, func([]byte) liquidityKind { return lpAdd }},
	// Burn(address indexed sender, uint256 amount0, uint256 amount1, address indexed to)
	topicBurnV2: {"v2", 3, 2, 1, func([]byte) liquidityKind { return lpRemove }},
	// Mint(address sender, address indexed owner, int24 indexed tickLower,
	// int24 indexed tickUpper, uint128 amount, uint256 amount0, uint256 amount1)
	topicMintV3: {"v3", 4, 4, 1, func([]byte) liquidityKind { return lpAdd }},
	// Burn(address indexed owner, int24 indexed tickLower, int24 indexed
	// tickUpper, uint128 amount, uint256 amount0, uint256 amount1)
	topicBurnV3: {"v3", 4, 3, 1, v3BurnKind},
	// Collect(address indexed owner, address recipient, int24 indexed
	// tickLower, int24 indexed tickUpper, uint128 amount0, uint128 amount1)
	topicCollectV3: {"v3", 4, 3, 1, func([]byte) liquidityKind { return lpCollect }},
	// ModifyLiquidity(bytes32 indexed id, address indexed sender, int24
	// tickLower, int24 tickUpper, int256 liquidityDelta, bytes32 salt)
	topicModifyLiquidityV4: {"v4", 3, 4, 2, v4ModifyKind},
}

// v3BurnKind: a v3 Burn of 0 liquidity only updates the fees owed (the
// position manager does it before every Collect), so it is a collect.
func v3BurnKind(data []byte) liquidityKind {
	if word(data, 0).Sign() == 0 {
		return lpCollect
	}
	return lpRemove
}

// v4ModifyKind reads the signed liquidityDelta (data word 2).
func v4ModifyKind(data []byte) liquidityKind {
	switch signedWord(data, 2).Sign() {
	case 1:
		return lpAdd
	case -1:
		return lpRemove
	}
	return lpCollect
}

// decodeLiquidityLog decodes a v2 Mint/Burn, v3 Mint/Burn/Collect or v4
// ModifyLiquidity log. Any other topic0 (a Swap, a Sync, a Transfer) or a log
// whose topic count, topic width, data length, address, block number or log
// index is wrong returns an error wrapping errBadLiquidityLog, so a caller fed
// a pool's whole log stream can skip what is not a liquidity event.
func decodeLiquidityLog(l rpcLog) (liquidityEvent, error) {
	if len(l.Topics) == 0 {
		return liquidityEvent{}, fmt.Errorf("%w: no topics", errBadLiquidityLog)
	}
	spec, ok := liquidityLogSpecs[strings.ToLower(l.Topics[0])]
	if !ok {
		return liquidityEvent{}, fmt.Errorf("%w: topic0 %s is not a liquidity event", errBadLiquidityLog, l.Topics[0])
	}
	if len(l.Topics) != spec.topics {
		return liquidityEvent{}, fmt.Errorf("%w: %d topics, want %d", errBadLiquidityLog, len(l.Topics), spec.topics)
	}
	for i, t := range l.Topics[1:] {
		// Ticks are sign-extended, not zero-padded: only the width is checked
		// here; the owner topic is checked as an address below.
		if b, err := hexBytes(t); err != nil || len(b) != 32 {
			return liquidityEvent{}, fmt.Errorf("%w: topic %d is not one 32-byte word", errBadLiquidityLog, i+1)
		}
	}
	owner, err := topicAddress(l.Topics[spec.ownerTopic])
	if err != nil {
		return liquidityEvent{}, fmt.Errorf("%w: owner: %w", errBadLiquidityLog, err)
	}
	data, err := hexBytes(l.Data)
	if err != nil || len(data) != 32*spec.words {
		return liquidityEvent{}, fmt.Errorf("%w: data is not %d 32-byte words", errBadLiquidityLog, spec.words)
	}
	if a, err := hexBytes(l.Address); err != nil || len(a) != 20 {
		return liquidityEvent{}, fmt.Errorf("%w: emitter %q is not an address", errBadLiquidityLog, l.Address)
	}
	block, err := hexUint(l.BlockNumber)
	if err != nil {
		return liquidityEvent{}, fmt.Errorf("%w: block number: %w", errBadLiquidityLog, err)
	}
	idx, err := hexUint(l.LogIndex)
	if err != nil {
		return liquidityEvent{}, fmt.Errorf("%w: log index: %w", errBadLiquidityLog, err)
	}
	ev := liquidityEvent{Protocol: spec.protocol, Kind: spec.kind(data), Venue: strings.ToLower(l.Address),
		Owner: owner, Block: block, LogIndex: idx, TxHash: strings.ToLower(l.TxHash)}
	if spec.protocol == "v4" {
		ev.PoolID = strings.ToLower(l.Topics[1])
	}
	return ev, nil
}

// liquidityEventsFor keeps the events that belong to the token at entryBlock:
// Block <= entryBlock, Venue in venues and, for v4 (the PoolManager is shared
// by every v4 pool), PoolID in poolIDs. With no pool ids every v4 event is
// dropped: it cannot be told apart from another token's. Keys of venues and
// poolIDs may be in any case.
func liquidityEventsFor(events []liquidityEvent, venues, poolIDs map[string]bool, entryBlock uint64) []liquidityEvent {
	v, ids := lowerSet(venues), lowerSet(poolIDs)
	var out []liquidityEvent
	for _, e := range events {
		if e.Block > entryBlock || !v[strings.ToLower(e.Venue)] {
			continue
		}
		if e.Protocol == "v4" && !ids[strings.ToLower(e.PoolID)] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// liquidityTxKinds is what one transaction did to one venue's liquidity. The
// direction decides which transfers are liquidity (see classifyTrade): an
// add moves tokens into the venue, a removal or a collect moves them out.
type liquidityTxKinds struct {
	Add     bool // lpAdd
	Remove  bool // lpRemove
	Collect bool // lpCollect (fees, v3 Burn of 0, v4 zero delta)
}

// in: a wallet -> venue transfer in this transaction is liquidity.
func (k liquidityTxKinds) in() bool { return k.Add }

// out: a venue -> wallet transfer in this transaction is liquidity.
func (k liquidityTxKinds) out() bool { return k.Remove || k.Collect }

// with returns k plus the event kind lk.
func (k liquidityTxKinds) with(lk liquidityKind) liquidityTxKinds {
	switch lk {
	case lpAdd:
		k.Add = true
	case lpRemove:
		k.Remove = true
	case lpCollect:
		k.Collect = true
	}
	return k
}

// liquidityTxSet maps a venue to the transactions with a liquidity event on
// it, and what each did. Keys are lowercase (venue address, then tx hash).
type liquidityTxSet map[string]map[string]liquidityTxKinds

// kinds returns what tx did to venue's liquidity. venue must be lowercase; tx
// may be in any case. An empty tx hash never matches, and a nil set has
// nothing: both give the zero liquidityTxKinds.
func (s liquidityTxSet) kinds(venue, tx string) liquidityTxKinds {
	tx = strings.ToLower(tx)
	if tx == "" {
		return liquidityTxKinds{}
	}
	return s[venue][tx]
}

// venues returns the set's venues (its keys, lowercase); empty for a nil set.
func (s liquidityTxSet) venues() map[string]bool {
	out := make(map[string]bool, len(s))
	for v := range s {
		out[v] = true
	}
	return out
}

// removalTxs returns the transactions with a removal (lpRemove, not a
// collect) on any venue of the set.
func (s liquidityTxSet) removalTxs() map[string]bool {
	out := map[string]bool{}
	for _, txs := range s {
		for tx, k := range txs {
			if k.Remove {
				out[tx] = true
			}
		}
	}
	return out
}

// liquidityTxs returns, per venue, the transactions with a liquidity event
// (add, remove or collect) at or before entryBlock that belong to the token
// (see liquidityEventsFor), with the kinds of the events in each. Events
// without a tx hash are left out: they cannot be matched to a transfer.
// entryBlock 0 returns errUnknownEntryBlock.
func liquidityTxs(events []liquidityEvent, venues, poolIDs map[string]bool, entryBlock uint64) (liquidityTxSet, error) {
	if entryBlock == 0 {
		return nil, errUnknownEntryBlock
	}
	return txSetOf(liquidityEventsFor(events, venues, poolIDs, entryBlock)), nil
}

// txSetOf builds the set from already-filtered events, merging the kinds of
// every event of one transaction on one venue.
func txSetOf(events []liquidityEvent) liquidityTxSet {
	out := liquidityTxSet{}
	for _, e := range events {
		tx := strings.ToLower(e.TxHash)
		if tx == "" {
			continue
		}
		venue := strings.ToLower(e.Venue)
		if out[venue] == nil {
			out[venue] = map[string]liquidityTxKinds{}
		}
		out[venue][tx] = out[venue][tx].with(e.Kind)
	}
	return out
}

// liquiditySummary is the token's liquidity activity up to and including the
// entry block, on its own venues.
type liquiditySummary struct {
	// Adds, Removes and Collects count events, not transactions: one
	// transaction can hold several (two v3 positions, a v4 rebalance).
	Adds, Removes, Collects int
	// RemovedBeforeEntry: a removal strictly before the entry block. A
	// removal in the entry block itself is in Removes but not here, since it
	// may come after the entry trade in that block.
	RemovedBeforeEntry bool
	// DeployerKnown is false when no deployer was given; DeployerRemoved is
	// then false and means nothing.
	DeployerKnown bool
	// DeployerRemoved: the deployer received the token (Transfer to the
	// deployer, value > 0, at or before the entry block) in a transaction
	// with a removal on one of the venues (liquidityTxSet.removalTxs: a
	// collect-only transaction is not one). Any sender counts, since a router
	// or position manager often forwards the tokens (v2 removeLiquidityETH,
	// v3 sweepToken). Event owners are not used (see liquidityEvent.Owner).
	// Missed: a v3 removal whose Collect is in a later transaction than its
	// Burn (that Collect alone reads as fee collection).
	DeployerRemoved bool
}

// liquidityStats summarises the token's liquidity events at entryBlock (see
// liquidityEventsFor for which events count). transfers are the token's
// Transfers, for DeployerRemoved; deployer is "" when unknown. entryBlock 0
// returns errUnknownEntryBlock.
//
// Not here: the share of LP tokens burned or locked (needs the LP token's own
// Transfers, a later PR), and amounts (not needed by any feature yet).
func liquidityStats(events []liquidityEvent, transfers []tokenTransfer, venues, poolIDs map[string]bool, deployer string, entryBlock uint64) (liquiditySummary, error) {
	if entryBlock == 0 {
		return liquiditySummary{}, errUnknownEntryBlock
	}
	var s liquiditySummary
	evs := liquidityEventsFor(events, venues, poolIDs, entryBlock)
	for _, e := range evs {
		switch e.Kind {
		case lpAdd:
			s.Adds++
		case lpRemove:
			s.Removes++
			s.RemovedBeforeEntry = s.RemovedBeforeEntry || e.Block < entryBlock
		case lpCollect:
			s.Collects++
		}
	}
	deployer = strings.ToLower(deployer)
	s.DeployerKnown = deployer != ""
	if s.DeployerKnown {
		// The same set earlyTradeStats is given: its removal transactions.
		s.DeployerRemoved = receivedInTxs(transfers, deployer, txSetOf(evs).removalTxs(), entryBlock)
	}
	return s, nil
}

// receivedInTxs reports whether a (lowercase) received a positive transfer at
// or before entryBlock in one of txs (lowercase hashes).
func receivedInTxs(transfers []tokenTransfer, a string, txs map[string]bool, entryBlock uint64) bool {
	for _, tr := range transfers {
		if tr.Block <= entryBlock && tr.Value != nil && tr.Value.Sign() > 0 &&
			strings.ToLower(tr.To) == a && txs[strings.ToLower(tr.TxHash)] {
			return true
		}
	}
	return false
}
