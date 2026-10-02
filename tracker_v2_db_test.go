package main

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"testing"
	"time"
)

func (f *fakeChain) swapV3Amt(pool string, block uint64, sqrtP, a0, a1 *big.Int) {
	f.logs = append(f.logs, fakeLog{addr: pool, topics: []string{topicSwapV3, addrTopic("0xaa"), addrTopic("0xbb")},
		data: ret(w32(a0), w32(a1), w32(sqrtP), wInt(1), wInt(0)), block: block, tx: hexU64(block)})
}

// State v2 end to end: a token paired with an asset that has no price feed
// (priced through that asset's own WETH pool), the 60-second-later entry,
// candles, pre-call trading, and re-tracking of calls done by the old version.
func TestTrackerV2QuotePoolLateEntryCandlesPrecall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t, 60*24*time.Hour)
	feed := "0x00000000000000000000000000000000000000fe"
	virt := "0x2222222222222222222222222222222222222222" // quote asset without a feed
	token := "0x4444444444444444444444444444444444444444"
	pool := "0x00000000000000000000000000000000000000c3"  // VIRT / token
	vpool := "0x00000000000000000000000000000000000000c9" // WETH / VIRT
	f.addToken(tWETH, 18, "WETH")
	f.addToken(virt, 18, "VIRT")
	f.addToken(token, 18, "TKN")
	f.constCall(feed, selDecimals, ret(wInt(8)))
	f.constCall(feed, selLatestRound, ret(wInt(1), wInt(3000e8), wInt(0), wInt(0), wInt(1)))
	for p, pair := range map[string][2]string{pool: {virt, token}, vpool: {tWETH, virt}} {
		f.constCall(p, selToken0, ret(wAddr(pair[0])))
		f.constCall(p, selToken1, ret(wAddr(pair[1])))
		f.constCall(p, selSlot0, ret(wInt(1)))
	}
	entry := time.Now().Add(-2 * 24 * time.Hour)
	eb := f.blockAtTime(entry)

	// VIRT = 0.0005 WETH ($1.50), then 0.001 WETH ($3.00) from +12h.
	f.swapV3(vpool, eb-100000, "0xv1", sqrtX96(1/0.0005))
	f.swapV3(vpool, eb+432000, "0xv2", sqrtX96(1/0.001))
	// VIRT moves mostly through the token's pool; its WETH pool is the quieter counterparty.
	for i := uint64(0); i < 5; i++ {
		f.transfer(virt, pool, "0x00000000000000000000000000000000000000d1", eb-50-i, hexU64(eb-50-i))
	}
	f.transfer(virt, vpool, "0x00000000000000000000000000000000000000d2", eb-40, "0xvt")

	buy := func(off int64, p float64) { // 2 VIRT in, token out
		blk := uint64(int64(eb) + off)
		f.swapV3Amt(pool, blk, sqrtX96(1/p), wei(2), big.NewInt(-10))
		f.transfer(token, pool, "0x00000000000000000000000000000000000000d1", blk, hexU64(blk))
	}
	sell := func(off int64, p float64) { // token in, 1 VIRT out
		blk := uint64(int64(eb) + off)
		f.swapV3Amt(pool, blk, sqrtX96(1/p), new(big.Int).Neg(wei(1)), big.NewInt(10))
		f.transfer(token, pool, "0x00000000000000000000000000000000000000d1", blk, hexU64(blk))
	}
	buy(-30000, 1.0e-3) // -50 min
	sell(-6000, 1.5e-3) // -10 min
	buy(-1200, 2.0e-3)  // -2 min  → entry 2e-3 VIRT = $0.003
	buy(300, 3.0e-3)    // +30 s   → late entry 3e-3 VIRT = $0.0045
	buy(6000, 6.0e-3)   // +10 min → peak $0.009
	sell(30000, 2.4e-3) // +50 min → $0.0036 at the 1h mark
	sell(72000, 1.5e-3) // +2 h    → 1.5e-3 VIRT; at 1d VIRT is $3 → $0.0045

	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_CHAINLINK_FEEDS", "eth="+feed)
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID

	check := func(label string) {
		t.Helper()
		tr, _ := st.GetTracking(ctx, id)
		if tr.Status != TrackTracking || tr.PriceUnit == nil || *tr.PriceUnit != "usd" ||
			tr.EntryPriceUSD == nil || math.Abs(*tr.EntryPriceUSD-0.003) > 1e-9 ||
			tr.EntryLatePriceUSD == nil || math.Abs(*tr.EntryLatePriceUSD-0.0045) > 1e-9 {
			t.Fatalf("%s: tracking %+v late %v err %v", label, tr, tr.EntryLatePriceUSD, strOrNil(tr.Error))
		}
		var os onchainState
		json.Unmarshal(tr.Onchain, &os)
		if os.V != onchainStateVersion || !os.PreDone || os.LateBlock != os.EntryBlock+600 || math.Abs(os.EntryQuoteUSD-1.5) > 1e-9 {
			t.Fatalf("%s: state %+v", label, os)
		}
		rs, _ := st.ReturnsForCall(ctx, id)
		if len(rs) != 2 {
			t.Fatalf("%s: returns %v", label, sortedKeys(rs))
		}
		// 1h: $0.0036 → +20% from the call price, -20% from the late entry; peak $0.009.
		r := rs["1h"]
		if !near(r.ReturnPct, 20) || !near(r.MaxGainPct, 200) || r.ReturnLatePct == nil || !near(*r.ReturnLatePct, -20) ||
			!near(*r.MaxGainLatePct, 100) || !near(*r.MaxDDLatePct, -20) {
			t.Fatalf("%s: 1h %+v late %v %v %v", label, r, *r.ReturnLatePct, *r.MaxGainLatePct, *r.MaxDDLatePct)
		}
		// 1d: 1.5e-3 VIRT × $3 = $0.0045 → +50% from the call price, 0% from the late entry.
		// Peak and low are valued at the VIRT price of their own hour ($1.50), not the later $3:
		// peak $0.009 (+200% / +100% late), low 1.5e-3 VIRT × $1.50 = $0.00225 (-25% / -50% late).
		if r := rs["1d"]; !near(r.ReturnPct, 50) || !near(*r.ReturnLatePct, 0) || !near(r.MaxGainPct, 200) ||
			!near(r.MaxDDPct, -25) || !near(*r.MaxGainLatePct, 100) || !near(*r.MaxDDLatePct, -50) {
			t.Fatalf("%s: 1d %+v late %v", label, r, *r.ReturnLatePct)
		}
		for _, iv := range []int{candleFineS, candleCoarseS} {
			cs, err := st.CandlesForCall(ctx, id, iv)
			if err != nil {
				t.Fatal(err)
			}
			events, hi := 0, 0.0
			for _, c := range cs {
				events += c.Events
				hi = math.Max(hi, c.H)
				if c.L > c.O || c.L > c.C || c.H < c.O || c.H < c.C {
					t.Fatalf("%s: bad candle %+v", label, c)
				}
			}
			// 4 trades after the call, each counted once (also after a re-track)
			if events != 4 || len(cs) == 0 || math.Abs(hi-0.009) > 1e-9 || math.Abs(cs[0].O-0.0045) > 1e-9 {
				t.Fatalf("%s: %ds candles: %d events, high %v, first %+v", label, iv, events, hi, cs)
			}
			if d := cs[0].Start.Sub(entry); d > 31*time.Second || d < -time.Duration(iv)*time.Second {
				t.Fatalf("%s: first %ds candle starts %s from the call", label, iv, d)
			}
		}
		b, err := st.DatasetRowJSON(ctx, id)
		if err != nil || b == nil {
			t.Fatalf("%s: dataset row: %v", label, err)
		}
		var row map[string]any
		json.Unmarshal(b, &row)
		num := func(k string) float64 {
			v, ok := row[k].(float64)
			if !ok {
				t.Fatalf("%s: dataset column %s = %v", label, k, row[k])
			}
			return v
		}
		// before the call: -50 min buy, -10 min sell, -2 min buy
		if num("pre_swaps_5m") != 1 || num("pre_swaps_15m") != 2 || num("pre_swaps_60m") != 3 ||
			num("pre_buys_60m") != 2 || num("pre_sells_60m") != 1 ||
			!near(num("pre_buy_vol_60m"), 6) || !near(num("pre_sell_vol_60m"), 1.5) || // 2×2 VIRT and 1 VIRT at $1.50
			!near(num("pre_price_chg_5m_pct"), 100.0/3) || !near(num("pre_price_chg_15m_pct"), 100) || !near(num("pre_price_chg_60m_pct"), 100) ||
			math.Abs(num("pre_first_trade_age_s")-3000) > 5 || row["pre_vol_unit"] != "usd" {
			t.Fatalf("%s: pre-call features %v", label, row)
		}
		if num("prior_calls") != 0 || num("calls_prev_24h") != 0 || !near(num("ret_late_1h"), -20) ||
			!near(num("max_gain_late_1d"), 100) || math.Abs(num("entry_late_price_usd")-0.0045) > 1e-9 || row["ret_late_3d"] != nil {
			t.Fatalf("%s: dataset row %v", label, row)
		}
	}

	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	check("first run")
	if got := s.onchain.quoteSource(virt); got != "its WETH pool (uniswap-v3)" {
		t.Fatalf("quote source %q", got)
	}

	// A call tracked by the old version (state v1) is queued and tracked again,
	// without doubling its candles.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = jsonb_set(onchain, '{v}', '1') WHERE call_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if tr, _ := st.GetTracking(ctx, id); tr.Status != TrackPending {
		t.Fatalf("old-version call not queued again: %s", tr.Status)
	}
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("re-track processed %d", n)
	}
	check("re-track")
	// … and a current-version call is left alone by the migration.
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if tr, _ := st.GetTracking(ctx, id); tr.Status != TrackTracking {
		t.Fatalf("current call was reset: %s", tr.Status)
	}

	// Scores are stored once per call, model version and bucket.
	p1, p2 := 0.31, 0.44
	for i := 0; i < 2; i++ {
		if err := st.UpsertPrediction(ctx, ScoutCallPrediction{CallID: id, ModelVersion: "v1", Bucket: "short", RunnerProb: &p1, CollapseProb: &p2}, []byte(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := st.PredictionsForCall(ctx, id)
	if err != nil || len(ps) != 1 || *ps[0].RunnerProb != 0.31 || ps[0].RunnerRankPct != nil {
		t.Fatalf("predictions %+v %v", ps, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_call_predictions_v WHERE call_id = $1 AND max_gain_late_1d > 99`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("predictions view: %d %v", n, err)
	}
}

func TestTradeOfLogDirections(t *testing.T) {
	mk := func(words ...string) rpcLog { return rpcLog{Data: ret(words...)} }
	one := wei(1)
	// v4: amounts are the swapper's; receiving the token (token0 here) is a buy.
	v4 := &onchainState{Kind: "v4", TokenIs0: true, QuoteDec: 18}
	if buy, vol, ok := v4.tradeOfLog(mk(wInt(10), w32(new(big.Int).Neg(one))), nil); !ok || !buy || !near(vol, 1) {
		t.Fatalf("v4 buy: %v %v %v", buy, vol, ok)
	}
	if buy, _, ok := v4.tradeOfLog(mk(wInt(-10), w32(one)), nil); !ok || buy {
		t.Fatal("v4 sell")
	}
	// v3: amounts are the pool's; the pool paying out the token is a buy.
	v3 := &onchainState{Kind: "v3", TokenIs0: false, QuoteDec: 18}
	if buy, vol, ok := v3.tradeOfLog(mk(w32(one), wInt(-10)), nil); !ok || !buy || !near(vol, 1) {
		t.Fatalf("v3 buy: %v %v %v", buy, vol, ok)
	}
	// v2: reserves; token reserve falling and quote reserve rising is a buy.
	v2 := &onchainState{Kind: "v2", TokenIs0: true, QuoteDec: 18}
	var prev [2]*big.Int
	if _, _, ok := v2.tradeOfLog(mk(wInt(1000), w32(wei(5))), &prev); ok {
		t.Fatal("first Sync has no direction")
	}
	if buy, vol, ok := v2.tradeOfLog(mk(wInt(900), w32(wei(6))), &prev); !ok || !buy || !near(vol, 1) {
		t.Fatalf("v2 buy: %v %v %v", buy, vol, ok)
	}
	if _, _, ok := v2.tradeOfLog(mk(wInt(1800), w32(wei(12))), &prev); ok {
		t.Fatal("liquidity added is not a trade")
	}
}
