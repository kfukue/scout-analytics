package main

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
)

// inTx puts a transfer in transaction h (xfer gives each transfer its own).
func inTx(tr tokenTransfer, h string) tokenTransfer {
	tr.TxHash = h
	return tr
}

// mustEarly runs earlyTradeStats without liquidity input (lp nil).
func mustEarly(t *testing.T, trs []tokenTransfer, venues, excluded map[string]bool, entry, window uint64) earlyTrading {
	t.Helper()
	return mustEarlyLP(t, trs, venues, excluded, nil, entry, window)
}

func mustEarlyLP(t *testing.T, trs []tokenTransfer, venues, excluded map[string]bool, lp liquidityTxSet, entry, window uint64) earlyTrading {
	t.Helper()
	e, err := earlyTradeStats(trs, venues, excluded, lp, entry, window)
	if err != nil {
		t.Fatalf("earlyTradeStats(entry %d, window %d, lp %v): %v", entry, window, lp, err)
	}
	return e
}

func set(addrs ...string) map[string]bool {
	m := map[string]bool{}
	for _, a := range addrs {
		m[a] = true
	}
	return m
}

// Launch addresses: the pool, the deployer, three snipers, three later buyers.
var (
	lPool, lDep   = hAddr(50), hAddr(1)
	lS1, lS2, lS3 = hAddr(11), hAddr(12), hAddr(13)
	lB4, lB5, lB6 = hAddr(14), hAddr(15), hAddr(16)
)

const launchEntry = 210

// launchTransfers: 600 minted into the pool and 400 to the deployer at block
// 100; first buy at 110 (s1 and s2 bundled in one transaction), s3 on the
// window's last block (160 = 110 + 50), b4 one block after it, s1 sells half,
// two more buyers, then a whale after the entry block.
func launchTransfers() []tokenTransfer {
	return []tokenTransfer{
		xfer(zeroAddr, lPool, tokens(600), 100, 0),
		xfer(zeroAddr, lDep, tokens(400), 100, 1),
		inTx(xfer(lPool, lS1, tokens(50), 110, 1), "0xb1"),
		inTx(xfer(lPool, lS2, tokens(40), 110, 2), "0xb1"),
		xfer(lPool, lS3, tokens(30), 160, 0),
		xfer(lPool, lB4, tokens(20), 161, 0),
		xfer(lS1, lPool, tokens(25), 170, 0),
		xfer(lPool, lB5, tokens(10), 200, 0),
		xfer(lDep, lB6, tokens(7), 205, 0), // wallet to wallet: not a buy
		xfer(lPool, lB6, tokens(5), launchEntry, 3),
		xfer(lPool, hAddr(99), tokens(300), launchEntry+1, 0), // after the entry block
	}
}

// checkLaunch checks every feature of launchTransfers at launchEntry.
func checkLaunch(t *testing.T, e earlyTrading) {
	t.Helper()
	if !e.TradesOK || !e.BuysOK || e.FirstBuyBlock != 110 || e.SniperWindowBlocks != defaultSniperWindowBlocks || e.Incomplete {
		t.Errorf("got tradesOK %v, buysOK %v, first buy block %d, window %d, incomplete %v; want true, true, 110, %d, false",
			e.TradesOK, e.BuysOK, e.FirstBuyBlock, e.SniperWindowBlocks, e.Incomplete, defaultSniperWindowBlocks)
	}
	if e.Snipers != 3 || e.FirstBlockBuyers != 2 || e.MaxTxBuyers != 2 {
		t.Errorf("got snipers %d, first-block buyers %d, max tx buyers %d; want 3, 2, 2",
			e.Snipers, e.FirstBlockBuyers, e.MaxTxBuyers)
	}
	f := e.Flow
	if f.Buyers != 6 || f.Sellers != 1 || f.Buys != 6 || f.Sells != 1 || !f.SellBuyRatioOK {
		t.Errorf("flow: got buyers %d, sellers %d, buys %d, sells %d, ratioOK %v; want 6, 1, 6, 1, true",
			f.Buyers, f.Sellers, f.Buys, f.Sells, f.SellBuyRatioOK)
	}
	wantBig(t, "bought", f.Bought, tokens(155))
	wantBig(t, "sold", f.Sold, tokens(25))
	wantShare(t, "sell/buy ratio", f.SellBuyRatio, 25.0/155)
	if want := []string{lS1, lS2, lS3, lB4, lB5, lB6}; !slices.Equal(e.EarlyBuyers, want) {
		t.Errorf("early buyers: got %v, want %v", e.EarlyBuyers, want)
	}
	// Pool: 600 - 155 + 25 = 470; circulating = 1000 - 470 = 530.
	if !e.SniperHeldShareOK || !e.EarlyBuyersHeldShareOK {
		t.Errorf("got sniperHeldShareOK %v, earlyBuyersHeldShareOK %v; want true, true", e.SniperHeldShareOK, e.EarlyBuyersHeldShareOK)
	}
	wantShare(t, "sniper held share (s1 25 + s2 40 + s3 30)", e.SniperHeldShare, 95.0/530)
	wantShare(t, "early buyers held share (+ b4 20 + b5 10 + b6 7 + 5)", e.EarlyBuyersHeldShare, 137.0/530)
}

func TestEarlyNormalLaunch(t *testing.T) {
	e := mustEarly(t, launchTransfers(), set(lPool), set(lPool), launchEntry, defaultSniperWindowBlocks)
	checkLaunch(t, e)
}

// A caller that leaves the pool out of excluded still gets the pool's balance
// out of circulating: earlyTradeStats excludes every venue itself.
func TestEarlyVenueMissingFromExcluded(t *testing.T) {
	cases := []struct {
		name     string
		excluded map[string]bool
	}{
		{"excluded nil", nil},
		{"excluded without the pool", set(hAddr(77))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkLaunch(t, mustEarly(t, launchTransfers(), set(lPool), c.excluded, launchEntry, defaultSniperWindowBlocks))
		})
	}
}

func TestEarlyUnsortedInput(t *testing.T) {
	in := launchTransfers()
	slices.Reverse(in)
	in[2], in[7] = in[7], in[2]
	orig := slices.Clone(in)
	checkLaunch(t, mustEarly(t, in, set(lPool), set(lPool), launchEntry, defaultSniperWindowBlocks))
	for i := range in {
		if in[i].Block != orig[i].Block || in[i].LogIndex != orig[i].LogIndex {
			t.Fatalf("input[%d] moved: got block %d log %d, want block %d log %d",
				i, in[i].Block, in[i].LogIndex, orig[i].Block, orig[i].LogIndex)
		}
	}
}

// The window counts from the first buy; a sell (a liquidity add, without
// liquidity input) one block before it does not move the anchor.
func TestEarlySniperWindowBoundary(t *testing.T) {
	pool, lp, a, b, c := hAddr(50), hAddr(1), hAddr(2), hAddr(3), hAddr(4)
	const first = math.MaxUint64 - 60 // first+window overflows for the largest window
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(100), first-2, 0),
		xfer(zeroAddr, lp, tokens(10), first-2, 1),
		xfer(lp, pool, tokens(10), first-1, 0), // reads as a sell
		xfer(pool, a, tokens(1), first, 0),
		xfer(pool, b, tokens(1), first+50, 0),
		xfer(pool, c, tokens(1), first+51, 0),
	}
	cases := []struct {
		name   string
		window uint64
		want   int
	}{
		{"default window: last block inside, next one out", defaultSniperWindowBlocks, 2},
		{"window 0: first buy block only", 0, 1},
		{"window 51", 51, 3},
		{"largest window, no overflow", math.MaxUint64, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEarly(t, trs, set(pool), set(pool), first+51, c.window)
			if e.Snipers != c.want || e.FirstBuyBlock != first || e.FirstBlockBuyers != 1 || e.Flow.Sells != 1 {
				t.Errorf("window %d: got snipers %d, first buy block %d, first-block buyers %d, sells %d; want %d, %d, 1, 1",
					c.window, e.Snipers, e.FirstBuyBlock, e.FirstBlockBuyers, e.Flow.Sells, c.want, uint64(first))
			}
		})
	}
}

// v2 launch addresses: the pool, the deployer, three snipers, a later buyer.
var (
	vPool, vDep        = hAddr(50), hAddr(1)
	vS1, vS2, vS3, vB4 = hAddr(11), hAddr(12), hAddr(13), hAddr(14)
	vAddTx             = xfer(vDep, vPool, tokens(600), 5, 0).TxHash // the liquidity add's transaction
)

const vLaunchEntry = 160

// v2LaunchTransfers: the deployer gets 1000 at block 1 and adds 600 as
// liquidity (wallet -> pool) at block 5; trading opens at block 100.
func v2LaunchTransfers() []tokenTransfer {
	return []tokenTransfer{
		xfer(zeroAddr, vDep, tokens(1000), 1, 0),
		xfer(vDep, vPool, tokens(600), 5, 0), // liquidity add
		inTx(xfer(vPool, vS1, tokens(50), 100, 0), "0xb1"),
		inTx(xfer(vPool, vS2, tokens(40), 100, 1), "0xb1"),
		xfer(vPool, vS3, tokens(30), 150, 0),
		xfer(vPool, vB4, tokens(20), 151, 0),
	}
}

// A v2 launch without liquidity input: the liquidity add reads as a sell.
// The sniper window and the first-block bundle count from the first buy, so
// the three buyers within 50 blocks of it are snipers.
func TestEarlyLiquidityAddBeforeFirstBuy(t *testing.T) {
	pool, s1, s2, s3, b4 := vPool, vS1, vS2, vS3, vB4
	trs := v2LaunchTransfers()
	e := mustEarly(t, trs, set(pool), set(pool), vLaunchEntry, defaultSniperWindowBlocks)
	if !e.TradesOK || !e.BuysOK || e.FirstBuyBlock != 100 || e.Snipers != 3 || e.FirstBlockBuyers != 2 || e.MaxTxBuyers != 2 {
		t.Errorf("got tradesOK %v, buysOK %v, first buy block %d, snipers %d, first-block buyers %d, max tx buyers %d; want true, true, 100, 3, 2, 2",
			e.TradesOK, e.BuysOK, e.FirstBuyBlock, e.Snipers, e.FirstBlockBuyers, e.MaxTxBuyers)
	}
	if e.Flow.Sells != 1 || e.Flow.Sellers != 1 || e.Flow.Buys != 4 {
		t.Errorf("flow: got sells %d, sellers %d, buys %d; want 1 (the liquidity add), 1, 4", e.Flow.Sells, e.Flow.Sellers, e.Flow.Buys)
	}
	if want := []string{s1, s2, s3, b4}; !slices.Equal(e.EarlyBuyers, want) {
		t.Errorf("early buyers: got %v, want %v", e.EarlyBuyers, want)
	}
	// Pool: 600 - 140 = 460; circulating = 1000 - 460 = 540 (dep 400 + 140).
	wantShare(t, "sniper held share (50 + 40 + 30)", e.SniperHeldShare, 120.0/540)
}

// Sells only (e.g. just a liquidity add): trades, but no buy-based figures.
func TestEarlySellsOnly(t *testing.T) {
	pool, dep := hAddr(50), hAddr(1)
	trs := []tokenTransfer{
		xfer(zeroAddr, dep, tokens(1000), 1, 0),
		xfer(dep, pool, tokens(600), 5, 0),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 10, defaultSniperWindowBlocks)
	if !e.TradesOK || e.BuysOK || e.FirstBuyBlock != 0 || e.Snipers != 0 || e.FirstBlockBuyers != 0 || e.MaxTxBuyers != 0 ||
		len(e.EarlyBuyers) != 0 || e.SniperHeldShareOK || e.EarlyBuyersHeldShareOK || e.SniperHeldShare != 0 || e.EarlyBuyersHeldShare != 0 {
		t.Errorf("got %+v; want tradesOK true, buysOK false and every buy figure zero", e)
	}
	if e.SniperWindowBlocks != defaultSniperWindowBlocks {
		t.Errorf("window: got %d, want %d", e.SniperWindowBlocks, defaultSniperWindowBlocks)
	}
	f := e.Flow
	if f.Sells != 1 || f.Sellers != 1 || f.Buys != 0 || f.SellBuyRatioOK {
		t.Errorf("flow: got sells %d, sellers %d, buys %d, ratioOK %v; want 1, 1, 0, false", f.Sells, f.Sellers, f.Buys, f.SellBuyRatioOK)
	}
	wantBig(t, "sold", f.Sold, tokens(600))
}

func TestEarlyIgnoresTradesAfterEntry(t *testing.T) {
	pool, a, b := hAddr(50), hAddr(2), hAddr(3)
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(100), 1, 0),
		xfer(pool, a, tokens(10), 5, 0), // at the entry block: counted
		xfer(pool, b, tokens(20), 6, 0),
		xfer(a, pool, tokens(10), 6, 1),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 5, defaultSniperWindowBlocks)
	if e.Flow.Buys != 1 || e.Flow.Sells != 0 || e.Snipers != 1 || !slices.Equal(e.EarlyBuyers, []string{a}) {
		t.Errorf("got buys %d, sells %d, snipers %d, early buyers %v; want 1, 0, 1, [%s]",
			e.Flow.Buys, e.Flow.Sells, e.Snipers, e.EarlyBuyers, a)
	}
	wantBig(t, "bought", e.Flow.Bought, tokens(10))
	wantShare(t, "early buyers held share (a holds all of circulating)", e.EarlyBuyersHeldShare, 1)
}

func TestEarlyNoTrades(t *testing.T) {
	pool, dep, a := hAddr(50), hAddr(1), hAddr(2)
	cases := []struct {
		name string
		trs  []tokenTransfer
	}{
		{"no transfers", nil},
		{"mints, wallet to wallet and burns only", []tokenTransfer{
			xfer(zeroAddr, dep, tokens(100), 1, 0),
			xfer(zeroAddr, pool, tokens(100), 1, 1),
			xfer(dep, a, tokens(10), 2, 0),
			xfer(a, deadAddr, tokens(5), 3, 0),
			xfer(pool, zeroAddr, tokens(5), 3, 1),
		}},
		{"first trade after the entry block", []tokenTransfer{
			xfer(zeroAddr, pool, tokens(100), 1, 0),
			xfer(pool, a, tokens(10), 11, 0),
		}},
		{"zero-value transfers only", []tokenTransfer{
			xfer(zeroAddr, pool, tokens(100), 1, 0),
			xfer(pool, a, new(big.Int), 2, 0),
			xfer(a, pool, new(big.Int), 3, 0),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEarly(t, c.trs, set(pool), set(pool), 10, defaultSniperWindowBlocks)
			if e.TradesOK || e.BuysOK || e.FirstBuyBlock != 0 || e.Snipers != 0 || e.FirstBlockBuyers != 0 || e.MaxTxBuyers != 0 ||
				len(e.EarlyBuyers) != 0 || e.SniperHeldShareOK || e.EarlyBuyersHeldShareOK {
				t.Errorf("got %+v, want no trades and every figure zero", e)
			}
			if e.SniperWindowBlocks != defaultSniperWindowBlocks {
				t.Errorf("window: got %d, want %d (set regardless)", e.SniperWindowBlocks, defaultSniperWindowBlocks)
			}
			f := e.Flow
			if f.Buyers != 0 || f.Sellers != 0 || f.Buys != 0 || f.Sells != 0 || f.SellBuyRatioOK {
				t.Errorf("flow: got %+v, want all zero and ratioOK false", f)
			}
			wantBig(t, "bought", f.Bought, new(big.Int))
			wantBig(t, "sold", f.Sold, new(big.Int))
			for _, v := range []float64{f.SellBuyRatio, e.SniperHeldShare, e.EarlyBuyersHeldShare} {
				if v != 0 || math.IsNaN(v) {
					t.Errorf("figure %v: want a finite 0", v)
				}
			}
		})
	}
}

func TestEarlyClassifyTrade(t *testing.T) {
	pool, pm, curve, token, a, b := hAddr(50), hAddr(51), hAddr(52), hAddr(53), hAddr(2), hAddr(3)
	venues := set(pool, pm, curve)
	ex := set(pool, pm, curve, token)
	up := func(s string) string { return "0x" + strings.ToUpper(s[2:]) }
	cases := []struct {
		name     string
		from, to string
		v        *big.Int
		want     tradeKind
	}{
		{"pool to wallet", pool, a, tokens(1), tradeBuy},
		{"wallet to pool", a, pool, tokens(1), tradeSell},
		{"PoolManager to wallet (v4)", pm, a, tokens(1), tradeBuy},
		{"wallet to PoolManager (v4)", a, pm, tokens(1), tradeSell},
		{"Pons curve to wallet", curve, a, tokens(1), tradeBuy},
		{"wallet to Pons curve", a, curve, tokens(1), tradeSell},
		{"mixed-case addresses", up(pool), up(a), tokens(1), tradeBuy},
		{"wallet to wallet", a, b, tokens(1), tradeNone},
		{"venue to venue", pm, pool, tokens(1), tradeNone},
		{"mint to wallet", zeroAddr, a, tokens(1), tradeNone},
		{"mint to pool", zeroAddr, pool, tokens(1), tradeNone},
		{"from dead to pool", deadAddr, pool, tokens(1), tradeNone},
		{"pool burns", pool, zeroAddr, tokens(1), tradeNone},
		{"pool to dead", pool, up(deadAddr), tokens(1), tradeNone},
		{"wallet burns", a, deadAddr, tokens(1), tradeNone},
		{"zero value", pool, a, new(big.Int), tradeNone},
		{"nil value", pool, a, nil, tradeNone},
		{"buy fee: pool to token contract", pool, token, tokens(1), tradeNone},
		{"buy fee: PoolManager to token contract", pm, up(token), tokens(1), tradeNone},
		{"fee swap-back: token contract to pool", token, pool, tokens(1), tradeNone},
		{"sell fee: wallet to token contract", a, token, tokens(1), tradeNone},
		{"token contract to wallet", token, a, tokens(1), tradeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTrade(xfer(c.from, c.to, c.v, 1, 0), venues, ex, nil); got != c.want {
				t.Errorf("classifyTrade(%s -> %s, %v): got %d, want %d", c.from, c.to, c.v, got, c.want)
			}
		})
	}
}

// A tax token: every buy transaction sends the buyer's share and a fee
// (pool -> token contract); a sell sends a fee to the token contract (wallet
// -> token) beside the sale; the contract later swaps fees back (token ->
// pool). Only the wallets' sides are trades.
func TestEarlyTaxToken(t *testing.T) {
	pool, token, a, b, c := hAddr(50), hAddr(53), hAddr(2), hAddr(3), hAddr(4)
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(1000), 1, 0),
		inTx(xfer(pool, a, tokens(95), 10, 0), "0xt1"),
		inTx(xfer(pool, token, tokens(5), 10, 1), "0xt1"),
		inTx(xfer(pool, b, tokens(95), 10, 2), "0xt2"),
		inTx(xfer(pool, token, tokens(5), 10, 3), "0xt2"),
		inTx(xfer(pool, c, tokens(95), 70, 0), "0xt3"), // outside the window
		inTx(xfer(pool, token, tokens(5), 70, 1), "0xt3"),
		inTx(xfer(a, token, tokens(5), 80, 0), "0xs1"), // sell fee
		inTx(xfer(a, pool, tokens(45), 80, 1), "0xs1"),
		xfer(token, pool, tokens(10), 90, 0), // swap-back
	}
	cases := []struct {
		name     string
		excluded map[string]bool
	}{
		{"pool and token excluded", set(pool, token)},
		{"pool missing from excluded", set(token)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mustEarly(t, trs, set(pool), tc.excluded, 90, defaultSniperWindowBlocks)
			if e.FirstBuyBlock != 10 || e.Snipers != 2 || e.FirstBlockBuyers != 2 || e.MaxTxBuyers != 1 {
				t.Errorf("got first buy block %d, snipers %d, first-block buyers %d, max tx buyers %d; want 10, 2, 2, 1",
					e.FirstBuyBlock, e.Snipers, e.FirstBlockBuyers, e.MaxTxBuyers)
			}
			if want := []string{a, b, c}; !slices.Equal(e.EarlyBuyers, want) {
				t.Errorf("early buyers: got %v, want %v", e.EarlyBuyers, want)
			}
			f := e.Flow
			if f.Buys != 3 || f.Buyers != 3 || f.Sells != 1 || f.Sellers != 1 {
				t.Errorf("flow: got buys %d, buyers %d, sells %d, sellers %d; want 3, 3, 1, 1", f.Buys, f.Buyers, f.Sells, f.Sellers)
			}
			wantBig(t, "bought (fees left out)", f.Bought, tokens(285))
			wantBig(t, "sold (fee left out)", f.Sold, tokens(45))
			// Pool: 1000 - 300 + 45 + 10 = 755; token: 15 + 5 - 10 = 10;
			// circulating = 1000 - 755 - 10 = 235 (a 45, b 95, c 95).
			wantShare(t, "sniper held share (a 45 + b 95)", e.SniperHeldShare, 140.0/235)
			wantShare(t, "early buyers held share", e.EarlyBuyersHeldShare, 1)
		})
	}
}

// A v4 token traded through the PoolManager and a Pons token on its curve,
// both listed as venues; a transfer between them is not a trade.
func TestEarlyV4AndPonsVenues(t *testing.T) {
	pm, curve, a, b, c := hAddr(51), hAddr(52), hAddr(2), hAddr(3), hAddr(4)
	trs := []tokenTransfer{
		xfer(zeroAddr, curve, tokens(1000), 1, 0),
		xfer(curve, a, tokens(100), 2, 0),
		xfer(curve, pm, tokens(500), 3, 0), // graduation: curve to PoolManager
		xfer(pm, b, tokens(50), 4, 0),
		xfer(a, pm, tokens(40), 5, 0),
		xfer(pm, c, tokens(10), 100, 0),
	}
	e := mustEarly(t, trs, set(pm, curve), set(pm, curve), 100, defaultSniperWindowBlocks)
	if e.FirstBuyBlock != 2 || e.Snipers != 2 || e.Flow.Buys != 3 || e.Flow.Sells != 1 ||
		e.Flow.Buyers != 3 || e.Flow.Sellers != 1 {
		t.Errorf("got first buy block %d, snipers %d, buys %d, sells %d, buyers %d, sellers %d; want 2, 2, 3, 1, 3, 1",
			e.FirstBuyBlock, e.Snipers, e.Flow.Buys, e.Flow.Sells, e.Flow.Buyers, e.Flow.Sellers)
	}
	wantBig(t, "bought", e.Flow.Bought, tokens(160))
	wantBig(t, "sold", e.Flow.Sold, tokens(40))
	// circulating: a 60 + b 50 + c 10 = 120; snipers a and b hold 110.
	wantShare(t, "sniper held share", e.SniperHeldShare, 110.0/120)
}

func TestEarlyBundles(t *testing.T) {
	pool := hAddr(50)
	w := func(n int) string { return hAddr(n) }
	mint := xfer(zeroAddr, pool, tokens(1000), 1, 0)
	cases := []struct {
		name                   string
		trs                    []tokenTransfer
		wantFirstBlock, wantTx int
	}{
		{"by block: separate transactions, one buyer twice", []tokenTransfer{mint,
			xfer(pool, w(1), tokens(1), 10, 0), xfer(pool, w(2), tokens(1), 10, 1),
			xfer(pool, w(3), tokens(1), 10, 2), xfer(pool, w(1), tokens(1), 10, 3),
			xfer(pool, w(4), tokens(1), 11, 0),
		}, 3, 1},
		{"by transaction: a later tx with four recipients", []tokenTransfer{mint,
			xfer(pool, w(1), tokens(1), 10, 0),
			inTx(xfer(pool, w(2), tokens(1), 20, 0), "0xAA"), inTx(xfer(pool, w(3), tokens(1), 20, 1), "0xaa"),
			inTx(xfer(pool, w(4), tokens(1), 20, 2), "0xaa"), inTx(xfer(pool, w(5), tokens(1), 20, 3), "0xaa"),
			inTx(xfer(pool, w(5), tokens(1), 20, 4), "0xaa"), // repeat recipient
			inTx(xfer(w(1), pool, tokens(1), 20, 5), "0xaa"), // a sell in the same tx
		}, 1, 4},
		{"buys without a tx hash are not grouped", []tokenTransfer{mint,
			inTx(xfer(pool, w(1), tokens(1), 10, 0), ""), inTx(xfer(pool, w(2), tokens(1), 12, 0), ""),
			inTx(xfer(pool, w(3), tokens(1), 13, 0), ""),
		}, 1, 0},
		{"first block opens with a sell, then a buy", []tokenTransfer{mint,
			xfer(zeroAddr, w(9), tokens(5), 2, 0), // minted to a wallet: not a trade
			xfer(w(9), pool, tokens(5), 5, 0), xfer(pool, w(1), tokens(1), 5, 1),
		}, 1, 1},
		{"a sell in an earlier block: counted from the first buy block", []tokenTransfer{mint,
			xfer(zeroAddr, w(9), tokens(5), 2, 0),
			xfer(w(9), pool, tokens(5), 3, 0), // liquidity add, read as a sell
			xfer(pool, w(1), tokens(1), 8, 0), xfer(pool, w(2), tokens(1), 8, 1),
		}, 2, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEarly(t, c.trs, set(pool), set(pool), 30, defaultSniperWindowBlocks)
			if e.FirstBlockBuyers != c.wantFirstBlock || e.MaxTxBuyers != c.wantTx {
				t.Errorf("got first-block buyers %d, max tx buyers %d; want %d, %d",
					e.FirstBlockBuyers, e.MaxTxBuyers, c.wantFirstBlock, c.wantTx)
			}
		})
	}
}

// A zero-value "buy" (address poisoning) before trading opens neither sets
// the first buy block nor counts as a buyer.
func TestEarlyZeroValueIgnored(t *testing.T) {
	pool, poisoner, a := hAddr(50), hAddr(66), hAddr(2)
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(100), 1, 0),
		xfer(pool, poisoner, new(big.Int), 2, 0),
		xfer(poisoner, pool, new(big.Int), 3, 0),
		xfer(pool, a, tokens(10), 100, 0),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 100, defaultSniperWindowBlocks)
	if e.FirstBuyBlock != 100 || e.Flow.Buys != 1 || e.Flow.Sells != 0 || !slices.Equal(e.EarlyBuyers, []string{a}) {
		t.Errorf("got first buy block %d, buys %d, sells %d, early buyers %v; want 100, 1, 0, [%s]",
			e.FirstBuyBlock, e.Flow.Buys, e.Flow.Sells, e.EarlyBuyers, a)
	}
}

// Venue keys and transfer addresses in mixed case: one buyer, lowercase out.
func TestEarlyMixedCaseAddresses(t *testing.T) {
	pool, a := hAddr(0xbeef), hAddr(0xabc) // both end in hex letters
	up := func(s string) string { return "0x" + strings.ToUpper(s[2:]) }
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(100), 1, 0),
		xfer(up(pool), a, tokens(10), 2, 0),
		xfer(pool, up(a), tokens(10), 3, 0),
		xfer(up(a), up(pool), tokens(5), 4, 0),
	}
	venues := set(up(pool))
	e := mustEarly(t, trs, venues, venues, 4, defaultSniperWindowBlocks)
	if e.Flow.Buyers != 1 || e.Flow.Buys != 2 || e.Flow.Sellers != 1 || e.Snipers != 1 || !slices.Equal(e.EarlyBuyers, []string{a}) {
		t.Errorf("got buyers %d, buys %d, sellers %d, snipers %d, early buyers %v; want 1, 2, 1, 1, [%s]",
			e.Flow.Buyers, e.Flow.Buys, e.Flow.Sellers, e.Snipers, e.EarlyBuyers, a)
	}
	wantShare(t, "early buyers held share (a holds all 15 circulating)", e.EarlyBuyersHeldShare, 1)
}

// A transfer to an excluded address (here the token contract itself, before
// any wallet buys) is not a buy, so it neither sets the first buy block nor
// takes an early-buyer slot; of eleven wallet buyers only the first ten are
// followed. The 10 is written out, not earlyBuyerCount, so the test fails if
// the cap changes.
func TestEarlyBuyersFirstTenAndExcluded(t *testing.T) {
	pool, token := hAddr(50), hAddr(52)
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(1000), 1, 0),
		xfer(pool, token, tokens(10), 10, 0),
	}
	var wallets []string
	for i := range 11 {
		w := hAddr(2 + i)
		wallets = append(wallets, w)
		trs = append(trs, xfer(pool, w, tokens(10), uint64(11+i), 0))
	}
	e := mustEarly(t, trs, set(pool), set(pool, token), 30, 0)
	if !slices.Equal(e.EarlyBuyers, wallets[:10]) || e.FirstBuyBlock != 11 || e.Snipers != 1 {
		t.Errorf("got early buyers %v, first buy block %d, snipers %d; want %v, 11, 1", e.EarlyBuyers, e.FirstBuyBlock, e.Snipers, wallets[:10])
	}
	if slices.Contains(e.EarlyBuyers, wallets[10]) {
		t.Errorf("early buyers %v include the eleventh buyer %s; want it left out", e.EarlyBuyers, wallets[10])
	}
	if e.Flow.Buys != 11 || e.Flow.Buyers != 11 {
		t.Errorf("flow: got buys %d, buyers %d; want 11, 11 (the token contract is not a buyer)", e.Flow.Buys, e.Flow.Buyers)
	}
	wantBig(t, "bought", e.Flow.Bought, tokens(110))
	// circulating: 110 (eleven wallets; the token's 10 is excluded); followed: 100.
	wantShare(t, "early buyers held share", e.EarlyBuyersHeldShare, 100.0/110)
	wantShare(t, "sniper held share (first wallet only)", e.SniperHeldShare, 10.0/110)
}

// Everything bought was sold back: circulating is 0, so no held shares.
func TestEarlyZeroCirculating(t *testing.T) {
	pool, a := hAddr(50), hAddr(2)
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, tokens(100), 1, 0),
		xfer(pool, a, tokens(10), 2, 0),
		xfer(a, pool, tokens(10), 3, 0),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 3, defaultSniperWindowBlocks)
	if !e.TradesOK || !e.BuysOK || e.SniperHeldShareOK || e.EarlyBuyersHeldShareOK || e.SniperHeldShare != 0 || e.EarlyBuyersHeldShare != 0 {
		t.Errorf("got %+v; want trades and buys, held shares not OK and 0", e)
	}
	wantShare(t, "sell/buy ratio", e.Flow.SellBuyRatio, 1)
}

// Values past uint64 and float64 exactness: totals stay exact.
func TestEarlyHugeValues(t *testing.T) {
	pool, a, b := hAddr(50), hAddr(2), hAddr(3)
	half := new(big.Int).Lsh(big.NewInt(1), 254)
	odd := new(big.Int).Add(half, big.NewInt(1))
	trs := []tokenTransfer{
		xfer(zeroAddr, pool, new(big.Int).Lsh(big.NewInt(1), 255), 1, 0),
		xfer(zeroAddr, pool, big.NewInt(1), 1, 1),
		xfer(pool, a, odd, 2, 0),
		xfer(pool, b, big.NewInt(3), 2, 1),
		xfer(a, pool, half, 3, 0),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 3, defaultSniperWindowBlocks)
	wantBig(t, "bought", e.Flow.Bought, new(big.Int).Add(odd, big.NewInt(3)))
	wantBig(t, "sold", e.Flow.Sold, half)
	wantShare(t, "sell/buy ratio", e.Flow.SellBuyRatio, 1)
	// a holds 1, b holds 3: circulating 4.
	wantShare(t, "sniper held share", e.SniperHeldShare, 1)
}

// The look-back cap cut off the mint: the pool spends what it never received.
func TestEarlyIncompleteCarriedThrough(t *testing.T) {
	pool, a := hAddr(50), hAddr(2)
	trs := []tokenTransfer{
		xfer(pool, a, tokens(10), 5, 0),
		xfer(a, pool, tokens(4), 6, 0),
	}
	e := mustEarly(t, trs, set(pool), set(pool), 6, defaultSniperWindowBlocks)
	if !e.Incomplete || !e.TradesOK || e.Flow.Buys != 1 || e.Flow.Sells != 1 {
		t.Errorf("got incomplete %v, tradesOK %v, buys %d, sells %d; want true, true, 1, 1",
			e.Incomplete, e.TradesOK, e.Flow.Buys, e.Flow.Sells)
	}
	noTrades := mustEarly(t, []tokenTransfer{xfer(a, hAddr(3), tokens(1), 2, 0)}, set(pool), set(pool), 6, defaultSniperWindowBlocks)
	if !noTrades.Incomplete || noTrades.TradesOK {
		t.Errorf("no trades: got incomplete %v, tradesOK %v; want true, false", noTrades.Incomplete, noTrades.TradesOK)
	}
}

func TestEarlyRejectsUnknownEntryBlock(t *testing.T) {
	trs := []tokenTransfer{xfer(hAddr(50), hAddr(1), tokens(1), 0, 0)}
	got, err := earlyTradeStats(trs, set(hAddr(50)), nil, nil, 0, defaultSniperWindowBlocks)
	if !errors.Is(err, errUnknownEntryBlock) {
		t.Errorf("earlyTradeStats(entry 0): got (%+v, %v), want an errUnknownEntryBlock error", got, err)
	}
}

func TestEarlyRejectsBadValue(t *testing.T) {
	pool := hAddr(50)
	for _, v := range []*big.Int{nil, big.NewInt(-1)} {
		trs := []tokenTransfer{xfer(pool, hAddr(1), v, 1, 0)}
		if _, err := earlyTradeStats(trs, set(pool), set(pool), nil, 1, defaultSniperWindowBlocks); err == nil {
			t.Errorf("earlyTradeStats(value %v): got no error, want one", v)
		}
	}
}
