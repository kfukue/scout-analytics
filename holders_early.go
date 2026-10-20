package main

// Early-trading features at a call's entry block, from the same token
// Transfer list that replayTransfers uses: snipers, bundled buys, buy/sell
// flow and how much the first buyers still hold. Pure, like holders.go: the
// caller reads the logs and passes them in.
//
// The wallet is always the token Transfer's other side: a buy is a transfer
// out of a trading venue, a sell a transfer into one. A router or a swap's
// sender is never used, and the wallet is never a venue, a burn address or an
// excluded address (such as the token contract taking a fee). A transfer in
// the direction of a liquidity event on the same venue in the same
// transaction (holders_lp.go) is liquidity, not a trade: into the venue in a
// transaction with an add, out of it in one with a removal or collect.

import (
	"cmp"
	"math/big"
	"slices"
	"strings"
)

// defaultSniperWindowBlocks is the default sniper window: a buyer whose first
// buy lands within this many blocks of the first buy (inclusive) is a sniper.
// 50 blocks ≈ 5 s at about 10 blocks/s (see onchain.go).
const defaultSniperWindowBlocks = 50

// earlyBuyerCount is how many of the first distinct buyers earlyTradeStats
// follows to the entry block.
const earlyBuyerCount = 10

// tradeKind says whether a transfer is a buy, a sell, liquidity or neither.
type tradeKind int

const (
	tradeNone tradeKind = iota
	tradeBuy
	tradeSell
	// tradeLiquidity: a wallet-venue transfer that would be a buy or a sell,
	// in the direction of a liquidity event on that venue in the same
	// transaction (see classifyTrade). Never in tradesUpTo's output.
	tradeLiquidity
)

// earlyTrade is one buy or sell. Wallet is the lowercase buyer (the
// transfer's To) or seller (its From); never a venue, burn or excluded
// address.
type earlyTrade struct {
	Kind     tradeKind
	Wallet   string
	Value    *big.Int
	Block    uint64
	LogIndex uint64
	TxHash   string // lowercase; "" when unknown
}

// earlyFlow is the buy/sell flow up to the entry block.
//
// Liquidity adds and removals are left out when earlyTradeStats is given the
// token's liquidity transactions (liquidityTxs). Without them (nil), a
// liquidity add (wallet -> pool) reads as a sell and a removal (pool ->
// wallet) as a buy; on v2/v3/v4 launches Sellers, Sells, Sold and
// SellBuyRatio are then dominated by liquidity adds and must not be used.
type earlyFlow struct {
	Buyers, Sellers int      // distinct wallets
	Buys, Sells     int      // number of transfers
	Bought, Sold    *big.Int // token base units; never nil
	// SellBuyRatioOK is false when nothing was bought: SellBuyRatio is then
	// 0 and must not be used.
	SellBuyRatioOK bool
	SellBuyRatio   float64 // Sold / Bought; can exceed 1
}

// earlyTrading is what earlyTradeStats reports for one call.
type earlyTrading struct {
	// TradesOK is false when there is no buy or sell at or before the entry
	// block: then BuysOK is false too and Flow is all zero (its Bought and
	// Sold are still non-nil zeros). SniperWindowBlocks and Incomplete are
	// set regardless.
	TradesOK bool
	// BuysOK is false when there is no buy at or before the entry block (no
	// trades, or sells only): then FirstBuyBlock, Snipers, FirstBlockBuyers,
	// MaxTxBuyers and both held shares are zero, EarlyBuyers is empty, both
	// held-share OK flags are false, and none of them must be used.
	BuysOK             bool
	FirstBuyBlock      uint64 // block of the earliest buy
	SniperWindowBlocks uint64 // the window used
	// Snipers: distinct buyers with a buy within SniperWindowBlocks of
	// FirstBuyBlock (inclusive). Anchored on the first buy, not the first
	// trade, so a sell long before trading opens (or a liquidity add, when no
	// liquidity transactions are given) does not push every buyer out of the
	// window.
	Snipers int
	// SniperHeldShare is what the snipers still hold at the entry block, as a
	// fraction of circulating (holderStats' definition). OK is false when
	// circulating is not > 0 or BuysOK is false.
	SniperHeldShareOK bool
	SniperHeldShare   float64
	// FirstBlockBuyers: distinct buy recipients in FirstBuyBlock (bundle by
	// block); at least 1 when BuysOK.
	FirstBlockBuyers int
	// MaxTxBuyers: the most distinct buy recipients in one transaction
	// (bundle by transaction). Buys without a TxHash are not grouped.
	MaxTxBuyers int
	Flow        earlyFlow
	// EarlyBuyers are the first earlyBuyerCount distinct buyers, by first buy;
	// EarlyBuyersHeldShare is what they still hold, like SniperHeldShare.
	EarlyBuyers            []string
	EarlyBuyersHeldShareOK bool
	EarlyBuyersHeldShare   float64
	// Incomplete is copied from the replay: the history does not reach back
	// far enough and the figures are unreliable.
	Incomplete bool
}

// earlyTradeStats computes the early-trading features at entryBlock, using
// only transfers with Block <= entryBlock. venues are the token's trading
// venues (its pools, the v4 PoolManager, a Pons curve); excluded are the
// addresses that are not holders (the token contract, burn addresses, and
// normally the venues too). Keys of both may be in any case.
//
// The union of excluded and venues is used as the excluded set, both for the
// held shares' circulating supply (holderStats) and for the wallets
// (classifyTrade), so a caller that leaves a venue out of excluded still gets
// correct shares. The exporter (a later PR) should build one excluded set
// (venues + token + burn addresses) and pass it to both holderStats and
// earlyTradeStats, so the two agree on circulating, and the same lp to both,
// so a liquidity add is neither a sell here nor a deployer sale there.
//
// lp are the token's liquidity transactions per venue (liquidityTxs, built
// from the same venues; keys lowercase): a wallet-venue transfer in one of
// them, in the direction of its liquidity event, is liquidity, neither a buy
// nor a sell (see classifyTrade). Buys and sells in the other direction stay,
// so a bundled launch keeps its snipers. nil gives exactly the results
// without liquidity awareness (adds read as sells, removals as buys).
//
// The input may be in any order and is not modified. entryBlock 0 returns
// errUnknownEntryBlock; a bad Value returns replayTransfers' error.
func earlyTradeStats(transfers []tokenTransfer, venues, excluded map[string]bool, lp liquidityTxSet, entryBlock, sniperWindowBlocks uint64) (earlyTrading, error) {
	if entryBlock == 0 {
		return earlyTrading{}, errUnknownEntryBlock
	}
	r, err := replayTransfers(transfers, entryBlock)
	if err != nil {
		return earlyTrading{}, err
	}
	v := lowerSet(venues)
	ex := unionSet(lowerSet(excluded), v)
	trades := tradesUpTo(transfers, v, ex, lp, entryBlock)
	out := earlyTrading{SniperWindowBlocks: sniperWindowBlocks, Flow: tradeFlow(trades), Incomplete: r.Incomplete}
	out.TradesOK = len(trades) > 0
	first, ok := firstBuyBlock(trades)
	if !ok {
		return out, nil
	}
	out.BuysOK = true
	out.FirstBuyBlock = first
	snipers := sniperBuyers(trades, first, sniperWindowBlocks)
	out.Snipers = len(snipers)
	out.FirstBlockBuyers = len(sniperBuyers(trades, first, 0))
	out.MaxTxBuyers = maxBuyersInOneTx(trades)
	out.EarlyBuyers = firstBuyers(trades, earlyBuyerCount)

	s := holderStats(r, transfers, ex, "", lp) // no deployer: lp is not used
	out.SniperHeldShareOK, out.EarlyBuyersHeldShareOK = s.SharesOK, s.SharesOK
	if s.SharesOK {
		out.SniperHeldShare = shareOf(heldBy(r, ex, snipers), s.Circulating)
		out.EarlyBuyersHeldShare = shareOf(heldBy(r, ex, out.EarlyBuyers), s.Circulating)
	}
	return out, nil
}

// unionSet adds every key of b to a and returns a. Both must have lowercase
// keys.
func unionSet(a, b map[string]bool) map[string]bool {
	for k := range b {
		a[k] = true
	}
	return a
}

// classifyTrade says whether tr is a buy (value > 0 from a venue to a
// wallet), a sell (value > 0 from a wallet to a venue) or neither (wallet to
// wallet, venue to venue, mints, burns, zero-value transfers, transfers to or
// from an excluded address). A wallet is any address that is not a venue, a
// burn address or in ex, so on a tax token the fee (pool -> token contract)
// is not a buy and the contract swapping fees back (token contract -> pool)
// is not a sell. The venue side is venues only: a sell's fee (wallet ->
// token contract) is neither.
//
// Liquidity is direction-aware, per venue and transaction (lp):
//   - in a transaction with an add on venue V, a wallet -> V transfer is
//     tradeLiquidity, while a V -> wallet transfer is still a buy, so a
//     bundled launch (add liquidity and snipe in one transaction) keeps its
//     snipers and bundle buyers;
//   - in a transaction with a removal or collect on V (a v3 Burn of 0 or
//     Collect, a v4 zero delta), a V -> wallet transfer is tradeLiquidity,
//     while a wallet -> V transfer is still a sell;
//   - with both an add and a removal or collect on V, both directions are
//     tradeLiquidity.
//
// Known edge cases:
//   - a wallet that adds liquidity and sells to the same venue in one
//     transaction has its sell treated as liquidity; likewise a buy from a
//     venue in a transaction that removes or collects on it;
//   - a v3 Collect without a Burn is fee collection, still liquidity;
//   - a transfer without a TxHash never matches and stays a buy or sell;
//   - with lp nil (or without the venue), adds read as sells and removals as
//     buys, as before liquidity events were decoded.
//
// venues, ex and lp must have lowercase keys.
func classifyTrade(tr tokenTransfer, venues, ex map[string]bool, lp liquidityTxSet) tradeKind {
	if tr.Value == nil || tr.Value.Sign() <= 0 {
		return tradeNone
	}
	from, to := strings.ToLower(tr.From), strings.ToLower(tr.To)
	wallet := func(a string) bool { return !venues[a] && !isBurnAddr(a) && !ex[a] }
	switch {
	case venues[from] && wallet(to):
		if lp.kinds(from, tr.TxHash).out() {
			return tradeLiquidity
		}
		return tradeBuy
	case wallet(from) && venues[to]:
		if lp.kinds(to, tr.TxHash).in() {
			return tradeLiquidity
		}
		return tradeSell
	}
	return tradeNone
}

// tradesUpTo returns the buys and sells with Block <= entryBlock, sorted by
// (Block, LogIndex); liquidity transfers are left out. venues, ex and lp must
// have lowercase keys (see classifyTrade). The input is not modified.
func tradesUpTo(transfers []tokenTransfer, venues, ex map[string]bool, lp liquidityTxSet, entryBlock uint64) []earlyTrade {
	sorted := slices.Clone(transfers)
	slices.SortStableFunc(sorted, func(a, b tokenTransfer) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	var trades []earlyTrade
	for _, tr := range sorted {
		if tr.Block > entryBlock {
			break // sorted: everything after is later too
		}
		t := earlyTrade{Kind: classifyTrade(tr, venues, ex, lp), Value: tr.Value, Block: tr.Block,
			LogIndex: tr.LogIndex, TxHash: strings.ToLower(tr.TxHash)}
		switch t.Kind {
		case tradeBuy:
			t.Wallet = strings.ToLower(tr.To)
		case tradeSell:
			t.Wallet = strings.ToLower(tr.From)
		default:
			continue
		}
		trades = append(trades, t)
	}
	return trades
}

// tradeFlow counts buys and sells, their distinct wallets and token totals.
// trades holds buys and sells only (tradesUpTo's output).
func tradeFlow(trades []earlyTrade) earlyFlow {
	f := earlyFlow{Bought: new(big.Int), Sold: new(big.Int)}
	buyers, sellers := map[string]bool{}, map[string]bool{}
	for _, t := range trades {
		switch t.Kind {
		case tradeBuy:
			f.Buys++
			f.Bought.Add(f.Bought, t.Value)
			buyers[t.Wallet] = true
		case tradeSell:
			f.Sells++
			f.Sold.Add(f.Sold, t.Value)
			sellers[t.Wallet] = true
		}
	}
	f.Buyers, f.Sellers = len(buyers), len(sellers)
	f.SellBuyRatioOK = f.Bought.Sign() > 0
	if f.SellBuyRatioOK {
		f.SellBuyRatio = shareOf(f.Sold, f.Bought)
	}
	return f
}

// firstBuyBlock returns the block of the first buy; ok is false when there is
// none. trades must be sorted.
func firstBuyBlock(trades []earlyTrade) (block uint64, ok bool) {
	for _, t := range trades {
		if t.Kind == tradeBuy {
			return t.Block, true
		}
	}
	return 0, false
}

// sniperBuyers returns the distinct buyers with a buy within window blocks of
// firstBlock (inclusive; window 0 means firstBlock only), in order of first
// buy. trades must be sorted and firstBlock must be the first buy's block, so
// a buyer with any buy in the window has its first buy there too. Sells are
// skipped, including any before firstBlock (e.g. a liquidity add).
func sniperBuyers(trades []earlyTrade, firstBlock, window uint64) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range trades {
		if t.Kind != tradeBuy || t.Block < firstBlock {
			continue
		}
		if t.Block-firstBlock > window { // subtraction: firstBlock+window could overflow
			break
		}
		if !seen[t.Wallet] {
			seen[t.Wallet] = true
			out = append(out, t.Wallet)
		}
	}
	return out
}

// maxBuyersInOneTx returns the largest number of distinct buy recipients in a
// single transaction. Buys with an empty TxHash are skipped: grouping them
// would merge unrelated transactions.
func maxBuyersInOneTx(trades []earlyTrade) int {
	byTx := map[string]map[string]bool{}
	best := 0
	for _, t := range trades {
		if t.Kind != tradeBuy || t.TxHash == "" {
			continue
		}
		if byTx[t.TxHash] == nil {
			byTx[t.TxHash] = map[string]bool{}
		}
		byTx[t.TxHash][t.Wallet] = true
		best = max(best, len(byTx[t.TxHash]))
	}
	return best
}

// firstBuyers returns the first n distinct buyers, in order of first buy
// (fewer when there are fewer buyers). trades must be sorted.
func firstBuyers(trades []earlyTrade, n int) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range trades {
		if len(out) == n {
			break
		}
		if t.Kind == tradeBuy && !seen[t.Wallet] {
			seen[t.Wallet] = true
			out = append(out, t.Wallet)
		}
	}
	return out
}

// heldBy sums the positive balances of addrs at the replay's entry block,
// leaving out excluded addresses (their balance is not part of circulating).
// The exclusion is defensive: classifyTrade already keeps excluded addresses
// out of the buyers. ex must have lowercase keys.
func heldBy(r holderReplay, ex map[string]bool, addrs []string) *big.Int {
	sum := new(big.Int)
	for _, a := range addrs {
		if b := r.Balances[a]; b != nil && b.Sign() > 0 && !ex[a] {
			sum.Add(sum, b)
		}
	}
	return sum
}
