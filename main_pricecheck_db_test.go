package main

import (
	"context"
	"strings"
	"testing"
)

// -price-check loads the asset database's Chainlink feeds as the tracker does
// (env wins on a conflict), says how many came from the database, and prices a
// quote whose feed is only in the database in USD; a database without the
// asset tables leaves the environment's feeds only.
func TestPriceCheckFeedsFromDB(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		nvda     = "0x1111111111111111111111111111111111111111"
		tsla     = "0x1212121212121212121212121212121212121212"
		aapl     = "0x1313131313131313131313131313131313131313"
		fNVDA    = "0x00000000000000000000000000000000000000f1"
		fTSLA    = "0x00000000000000000000000000000000000000f3"
		fAAPLdb  = "0x00000000000000000000000000000000000000f4"
		fAAPLenv = "0x00000000000000000000000000000000000000f5"
	)
	f := ltChain(t, 20*ltDay)
	f.addToken(nvda, 18, "NVDA")
	f.constCall(fNVDA, selDecimals, ret(wInt(8)))
	f.constCall(fNVDA, selLatestRound, ret(wInt(1), wInt(150e8), wInt(0), wInt(0), wInt(1)))
	env := map[string]string{"SCOUT_CHAINLINK_FEEDS": "eth=" + ltFeed + "," + aapl + "=" + fAAPLenv,
		"SCOUT_MAINNET_RPC_URL": "", "SCOUT_MAINNET_CHAINLINK_FEEDS": ""}

	t.Run("no asset tables", func(t *testing.T) {
		if _, err := st.Pool.Exec(ctx, `DROP TABLE IF EXISTS asset_chains, assets, chains`); err != nil {
			t.Fatal(err)
		}
		o := testOnchain(t, f, env)
		got := priceCheckFeeds(ctx, o, st, nil)
		for _, w := range []string{"feeds:        2 Robinhood Chain", "SCOUT_*CHAINLINK_FEEDS only; 0 from the asset database (no asset tables)"} {
			if !strings.Contains(got, w) {
				t.Errorf("got %q, want it to contain %q", got, w)
			}
		}
		if _, ok, err := o.quoteUSD(ctx, nvda, f.latest-1000); ok || err != nil {
			t.Errorf("NVDA without the db feed: got ok=%v err=%v, want no USD source", ok, err)
		}
	})

	t.Run("asset tables", func(t *testing.T) {
		createAssetTables(t, st)
		addAssetFeed(t, st, 1, "NVDA", 1, nvda, 1, fNVDA)
		addAssetFeed(t, st, 2, "TSLA", 1, tsla, 2, fTSLA)   // feed on Ethereum mainnet
		addAssetFeed(t, st, 3, "AAPL", 1, aapl, 1, fAAPLdb) // env wins
		o := testOnchain(t, f, env)
		got := priceCheckFeeds(ctx, o, st, nil)
		// Robinhood: eth + AAPL (env) + NVDA (db); mainnet: TSLA (db) + the default ETH/USD feed.
		want := "feeds:        3 Robinhood Chain + 2 Ethereum mainnet Chainlink feed(s) in use; 1 + 1 of them from the asset database"
		if !strings.Contains(got, want) {
			t.Fatalf("got %q, want it to contain %q", got, want)
		}
		if fm := o.feedMaps(); fm.rh[aapl] != fAAPLenv || fm.rh[nvda] != fNVDA {
			t.Fatalf("feeds in use: got AAPL %q NVDA %q, want %q (env) and %q (db)", fm.rh[aapl], fm.rh[nvda], fAAPLenv, fNVDA)
		}
		if q, ok, err := o.quoteUSD(ctx, nvda, f.latest-1000); err != nil || !ok || q != 150 {
			t.Fatalf("NVDA through the db feed: got %v %v %v, want $150", q, ok, err)
		}
		if !strings.Contains(o.describe(), "3 Chainlink feed(s) on Robinhood Chain, 1 of them from the asset database") {
			t.Fatalf("source line: got %q, want the db count", o.describe())
		}
	})
}
