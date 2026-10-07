package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Full on-chain flow with the DB: discovery → entry → horizons (USD via
// Chainlink) → incremental re-check → done + rug flag → dataset export.
func TestTrackerOnchainEndToEnd(t *testing.T)         { runTrackerOnchain(t, "archive") }
func TestTrackerOnchainEndToEndFullNode(t *testing.T) { runTrackerOnchain(t, "full") }
func TestTrackerOnchainEndToEndMainnet(t *testing.T)  { runTrackerOnchain(t, "mainnet") }

// runTrackerOnchain runs the whole flow with ETH/USD coming from:
//   - "archive": the Robinhood feed read with historical eth_call
//   - "full":    the Robinhood feed's AnswerUpdated events (full node)
//   - "mainnet": Chainlink on an Ethereum mainnet archive node (Robinhood node is a full node)
func runTrackerOnchain(t *testing.T, mode string) {
	fullNode := mode != "archive"
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t, 60*24*time.Hour)
	feed := "0x00000000000000000000000000000000000000fe"
	f.addToken(tWETH, 18, "WETH")
	f.constCall(feed, selDecimals, ret(wInt(8)))
	f.constCall(feed, selLatestRound, ret(wInt(1), wInt(3000e8), wInt(0), wInt(0), wInt(1)))
	f.fullNode = fullNode
	if mode == "full" {
		agg := "0x00000000000000000000000000000000000000a9"
		f.constCall(feed, selAggregator, ret(wAddr(agg)))
		for b := uint64(1000); b < f.latest; b += 864000 { // a feed update every day: ETH = $3000
			f.answerUpdated(agg, b, 3000e8)
		}
	}

	// young call: 8 days old (30d pending). old call: 35 days old, collapses after day 10.
	young, old := "0x4444444444444444444444444444444444444444", "0x5555555555555555555555555555555555555555"
	nostock := "0x7777777777777777777777777777777777777777" // paired with a stock token that has no feed
	poolY, poolO, poolN := "0x00000000000000000000000000000000000000c3", "0x00000000000000000000000000000000000000c4", "0x00000000000000000000000000000000000000c5"
	stock := "0x1111111111111111111111111111111111111111"
	f.addToken(stock, 18, "TSLA")
	eYoung, eOld, eNo := time.Now().Add(-8*24*time.Hour), time.Now().Add(-35*24*time.Hour), time.Now().Add(-2*24*time.Hour)
	setup := func(token, pool, quote string, entry time.Time, prices map[uint64]float64) {
		f.addToken(token, 18, "TKN")
		f.constCall(pool, selToken0, ret(wAddr(quote))) // quote < token → token is token1
		f.constCall(pool, selToken1, ret(wAddr(token)))
		f.constCall(pool, selSlot0, ret(wInt(1)))
		eb := f.blockAtTime(entry)
		for off, p := range prices {
			blk := eb + off - 1000 // offsets are shifted so 500 means "before the call"
			f.swapV3(pool, blk, hexU64(blk), sqrtX96(1/p))
			f.transfer(token, pool, "0x00000000000000000000000000000000000000d1", blk, hexU64(blk))
		}
	}
	day := uint64(864000)
	setup(young, poolY, tWETH, eYoung, map[uint64]float64{500: 1e-6, 1000 + 20000: 5e-6, 1000 + 30000: 2e-6, 1000 + 2*day: 3e-6})
	setup(old, poolO, tWETH, eOld, map[uint64]float64{500: 1e-6, 1000 + 20000: 4e-6, 1000 + 5*day: 2e-6, 1000 + 11*day: 1e-8})
	setup(nostock, poolN, stock, eNo, map[uint64]float64{500: 2e-5, 1000 + 20000: 3e-5, 1000 + day/2: 6e-5})
	// 0.05 WETH left in the pool: $150 of quote side (stored as $300, 2 × it).
	// "archive" keeps the $500 default (under it: rugged by the end check too);
	// "full" and "mainnet" set SCOUT_RUG_LIQ_USD=100 (above it: price rule only).
	f.constCall(tWETH, selBalanceOf, ret(w32(wei(0.05))))
	rugLiqEnv := "100"
	if mode == "archive" {
		rugLiqEnv = ""
	}
	t.Setenv("SCOUT_RUG_LIQ_USD", rugLiqEnv)

	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_CHAINLINK_FEEDS", "eth="+feed)
	if mode == "mainnet" {
		m := newFakeMainnet(t, func(int64) float64 { return 3000 })
		t.Setenv("SCOUT_CHAINLINK_FEEDS", "") // no feed on Robinhood Chain at all
		t.Setenv("SCOUT_MAINNET_RPC_URL", m.srv.URL)
		t.Setenv("SCOUT_MAINNET_RPC_RPS", "100000")
	}
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777

	s.onChannelPost(postAt(1, eYoung, young))
	s.onChannelPost(postAt(2, eOld, old))
	s.onChannelPost(postAt(3, eNo, nostock))
	ids := []int{*(<-s.queue).CallID, *(<-s.queue).CallID, *(<-s.queue).CallID}

	// the young call was half-priced by GeckoTerminal earlier: on-chain must start it fresh
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET entry_price_usd = 123, entry_price_source = 'minute',
		pool_address = '0xgeckopool', status = 'tracking' WHERE call_id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 3 {
		t.Fatalf("processed %d", n)
	}
	f.mu.Lock()
	total := 0
	for _, n := range f.count {
		total += n
	}
	t.Logf("%s: first pass over 3 calls: %d eth_blockNumber, %d eth_getBlockByNumber, %d requests in all", mode, f.count["eth_blockNumber"], f.count["eth_getBlockByNumber"], total)
	f.mu.Unlock()

	// young: entry 1e-6 WETH × $3000 = $0.003
	ty, _ := st.GetTracking(ctx, ids[0])
	if ty.Status != TrackTracking || ty.EntryPriceUSD == nil || math.Abs(*ty.EntryPriceUSD-0.003) > 1e-9 ||
		*ty.EntryPriceSource != "onchain-v3" || *ty.PriceUnit != "usd" || *ty.PoolDex != "uniswap-v3" || *ty.PoolAddress != poolY {
		t.Fatalf("young: %+v (err %v)", ty, strOrNil(ty.Error))
	}
	ry, _ := st.ReturnsForCall(ctx, ids[0])
	if len(ry) != 4 {
		t.Fatalf("young returns: %v", sortedKeys(ry))
	}
	// 1h: last 2e-6 (+100%), peak 5e-6 (+400%), low = entry (0%)
	if r := ry["1h"]; !near(r.ReturnPct, 100) || !near(r.MaxGainPct, 400) || !near(r.MaxDDPct, 0) || r.LastTradeAt == nil {
		t.Fatalf("young 1h: %+v", r)
	}
	// 3d/7d: last 3e-6 (+200%), peak still +400%
	if r := ry["7d"]; !near(r.ReturnPct, 200) || !near(r.MaxGainPct, 400) || math.Abs(r.PriceUSD-0.009) > 1e-9 {
		t.Fatalf("young 7d: %+v", r)
	}
	var os onchainState
	json.Unmarshal(ty.Onchain, &os)
	if os.Kind != "v3" || os.ScanBlock == 0 || !os.Done["7d"] || os.Done["30d"] {
		t.Fatalf("young state: %+v", os)
	}

	// old: all horizons done; -99% at 30d → rugged by the price rule (< 5% of
	// entry). The end check reads $150 of quote side: under the $500 default
	// it is a rug too (rug block set, latest -100% from now on); above a $100
	// threshold it is not. Either way the column stores the depth, $300.
	to, _ := st.GetTracking(ctx, ids[1])
	ro, _ := st.ReturnsForCall(ctx, ids[1])
	if to.Status != TrackDone || to.Rugged == nil || !*to.Rugged || len(ro) != 5 || to.CurrentLiquidityUSD == nil || math.Abs(*to.CurrentLiquidityUSD-300) > 1e-6 {
		t.Fatalf("old: %+v returns %v", to, sortedKeys(ro))
	}
	var oos onchainState
	if err := json.Unmarshal(to.Onchain, &oos); err != nil {
		t.Fatal(err)
	}
	if endRug := oos.RugBlock > 0; endRug != (rugLiqEnv == "") ||
		(endRug && (oos.RugLiquidityUSD == nil || math.Abs(*oos.RugLiquidityUSD-150) > 1e-6)) {
		t.Fatalf("old, SCOUT_RUG_LIQ_USD=%q: got rug block %d quote side %v, want a rug at $150 only under the $500 default",
			rugLiqEnv, oos.RugBlock, rugF(oos.RugLiquidityUSD))
	}
	if r := ro["30d"]; !near(r.ReturnPct, -99) || !near(r.MaxGainPct, 300) || !near(r.MaxDDPct, -99) {
		t.Fatalf("old 30d: %+v", r)
	}
	if r := ro["7d"]; !near(r.ReturnPct, 100) || !near(r.MaxDDPct, 0) {
		t.Fatalf("old 7d (before the dump): %+v", r)
	}

	// stock-token pair without a feed: tracked in TSLA units, still gives returns
	tn, _ := st.GetTracking(ctx, ids[2])
	rn, _ := st.ReturnsForCall(ctx, ids[2])
	if tn.PriceUnit == nil || *tn.PriceUnit != "TSLA" || math.Abs(*tn.EntryPriceUSD-2e-5) > 1e-12 {
		t.Fatalf("nostock: %+v", tn)
	}
	if r := rn["1d"]; !near(r.ReturnPct, 200) || !near(r.MaxGainPct, 200) {
		t.Fatalf("nostock 1d: %+v", r)
	}

	// nothing due now; a later check must not rescan what was already scanned
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("second pass processed %d", n)
	}
	logsBefore := f.count["eth_getLogs"]
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	s.trackDue(ctx, 50) // 30d still not due → no log scans at all
	if d := f.count["eth_getLogs"] - logsBefore; d != 0 {
		t.Fatalf("re-check rescanned logs: %d eth_getLogs calls", d)
	}

	// export has the on-chain columns
	var buf bytes.Buffer
	if n, err := st.ExportDatasetCSV(ctx, &buf); err != nil || n != 3 {
		t.Fatalf("export %d %v", n, err)
	}
	recs, _ := csv.NewReader(&buf).ReadAll()
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, c := range []string{"price_unit", "quote_asset", "entry_price_source", "ret_7d", "rugged"} {
		if _, ok := col[c]; !ok {
			t.Fatalf("export missing %s", c)
		}
	}
	for _, r := range recs[1:] {
		if r[col["contract_address"]] == nostock && (r[col["price_unit"]] != "TSLA" || r[col["quote_asset"]] != "TSLA") {
			t.Fatalf("nostock export row: %v", r)
		}
	}
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// An entry whose quote asset first traded after the call ("no price yet" at
// the call block: its pool exists around the call, but its first trade is 10
// minutes later) stays a retried error until the call is past its last
// horizon + 48 h (the gave_up deadline); after that it is tracked in quote
// units, logged once, instead of staying in error for ever.
func TestTrackerEntryNoPriceYetCappedAfterDeadline(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		quote = "0x2222222222222222222222222222222222222222"
		qpool = "0x00000000000000000000000000000000000000c8" // QT / USDG
	)
	for i, c := range []struct {
		name     string
		ago      time.Duration
		pastLast bool // past the last horizon (30d) + 48 h
	}{
		{"2 days old", 2 * ltDay, false},
		{"31 days old (inside the 48 h)", 31 * ltDay, false},
		{"33 days old", 33 * ltDay, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			token := fmt.Sprintf("0x%040x", 0x4444+i)
			pool := fmt.Sprintf("0x%040x", 0xc3+i)
			f := ltChain(t, 40*ltDay) // a new chain and tracker: no USD caches shared with the other cases
			f.addToken(tUSDG, 6, "USDG")
			f.addToken(quote, 18, "QT")
			f.ltPool(token, pool, quote)
			call := rugCall{entry: time.Now().Add(-c.ago)}
			call.eb = f.blockAtTime(call.entry)
			qp := newUSDTestPool(f, qpool, quote, tUSDG, 18, 6)
			qp.trade(call.at(10*time.Minute), 3) // QT's first trade: after the call
			f.ltTrade(token, pool, call.at(-time.Minute), 0.01)
			f.ltTrade(token, pool, call.at(ltDay/2), 0.03)
			logs := captureLog(t)
			s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed)
			s.onChannelPost(postAt(100+i, call.entry, token))
			call.id = *(<-s.queue).CallID
			t.Cleanup(func() {
				// Not due for the next case's tracker (another chain).
				if _, err := st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking SET next_check_at = now() + interval '1 year' WHERE call_id = $1`, call.id); err != nil {
					t.Errorf("park call %d: %v", call.id, err)
				}
			})
			for run := 1; run <= 2; run++ {
				s.trackDue(ctx, 50)
				tr, _ := st.GetTracking(ctx, call.id)
				if !c.pastLast {
					if tr.Status != TrackError || tr.PriceUnit != nil || tr.Error == nil || !strings.Contains(*tr.Error, "no price yet") {
						t.Fatalf("run %d: got status %s unit %s error %s, want an error (no price yet) and no unit; log:\n%s",
							run, tr.Status, strOrNil(tr.PriceUnit), strOrNil(tr.Error), logs.String())
					}
				} else if tr.PriceUnit == nil || *tr.PriceUnit != "QT" || tr.EntryPriceUSD == nil || !relNear(*tr.EntryPriceUSD, 0.01) ||
					tr.Error != nil || tr.Status == TrackError || tr.Status == TrackGaveUp {
					t.Fatalf("run %d: got status %s unit %s entry %v error %s, want tracked in QT, entry 0.01 QT, no error",
						run, tr.Status, strOrNil(tr.PriceUnit), rugF(tr.EntryPriceUSD), strOrNil(tr.Error))
				}
				if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = $1`, call.id); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if c.pastLast {
				want = 1
			}
			if n := strings.Count(logs.String(), "giving up on USD, tracked in QT"); n != want {
				t.Fatalf("got %d \"giving up on USD\" lines, want %d:\n%s", n, want, logs.String())
			}
			if !c.pastLast {
				return
			}
			rets, err := st.ReturnsForCall(ctx, call.id)
			if err != nil {
				t.Fatal(err)
			}
			if r, ok := rets["1d"]; !ok || !relNear(r.ReturnPct, 200) {
				t.Fatalf("1d return (in QT): got %+v (present %v), want +200%%", r, ok)
			}
		})
	}
}
