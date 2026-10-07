package main

import (
	"context"

	"strings"
	"testing"
	"time"
)

// Database tests of the USD sources: Chainlink feeds read from the asset
// tables (feeds_db.go), the horizon step that keeps a segment when its USD
// conversion fails, a Pons curve quoted in a token priced through its own
// WETH pool, and an entry whose quote has no price yet.

// usdScanner is ltScanner with SCOUT_CHAINLINK_FEEDS set to feeds.
func usdScanner(t *testing.T, st *ScoutStore, rpcURL, feeds string) *scanner {
	t.Helper()
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", rpcURL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_CHAINLINK_FEEDS", feeds)
	t.Setenv("SCOUT_MAINNET_RPC_URL", "")
	t.Setenv("SCOUT_MAINNET_CHAINLINK_FEEDS", "")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(), Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	return s
}

// createAssetTables makes the asset tracker's chains / assets / asset_chains
// (the columns feeds_db.go reads, as in lyle-labs-libraries' SQL) and drops
// them when the test ends.
func createAssetTables(t *testing.T, st *ScoutStore) {
	t.Helper()
	ctx := context.Background()
	drop := `DROP TABLE IF EXISTS asset_chains, assets, chains`
	if _, err := st.Pool.Exec(ctx, drop); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := st.Pool.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop asset tables: %v", err)
		}
	})
	if _, err := st.Pool.Exec(ctx, `
		CREATE TABLE chains (id SERIAL PRIMARY KEY, name VARCHAR(255) NOT NULL, chain_id INT NULL);
		CREATE TABLE assets (id SERIAL PRIMARY KEY, name TEXT NOT NULL, ticker VARCHAR(255) NULL,
			chain_id INT NULL REFERENCES chains(id), contract_address VARCHAR(255) NULL);
		CREATE TABLE asset_chains (asset_id INT NOT NULL REFERENCES assets(id), chain_id INT NOT NULL REFERENCES chains(id),
			chainlink_data_feed_contract_address TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT 'test', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_by TEXT NOT NULL DEFAULT 'test', updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (asset_id, chain_id));
		INSERT INTO chains (id, name, chain_id) VALUES (1, 'Robinhood Chain', 4663), (2, 'Ethereum', 1), (3, 'Base', 8453);`); err != nil {
		t.Fatal(err)
	}
}

func addAssetFeed(t *testing.T, st *ScoutStore, id int, name string, assetChain int, addr string, feedChain int, feed string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, `INSERT INTO assets (id, name, chain_id, contract_address) VALUES ($1, $2, $3, $4)`, id, name, assetChain, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO asset_chains (asset_id, chain_id, chainlink_data_feed_contract_address) VALUES ($1, $2, $3)`, id, feedChain, feed); err != nil {
		t.Fatal(err)
	}
}

// Feeds from the asset tables: Robinhood Chain tokens with a feed on
// Robinhood Chain or on Ethereum; lower-cased; the lowest asset id wins among
// duplicates; the environment wins on a conflict; reloads replace the maps
// (never change them); missing tables keep what is in use and are logged
// once. A stock-token pair whose feed is only in the database is tracked in USD.
func TestChainlinkFeedsFromDB(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	createAssetTables(t, st)
	const (
		nvda     = "0x1111111111111111111111111111111111111111"
		tsla     = "0x1212121212121212121212121212121212121212"
		aapl     = "0x1313131313131313131313131313131313131313"
		amd      = "0x1414141414141414141414141414141414141414"
		baseTok  = "0x1515151515151515151515151515151515151515"
		fNVDA    = "0x00000000000000000000000000000000000000f1"
		fDup     = "0x00000000000000000000000000000000000000f2"
		fTSLA    = "0x00000000000000000000000000000000000000f3"
		fAAPLdb  = "0x00000000000000000000000000000000000000f4"
		fAAPLenv = "0x00000000000000000000000000000000000000f5"
		fAMD     = "0x00000000000000000000000000000000000000f6"
		fBase    = "0x00000000000000000000000000000000000000f7"
		token    = "0x4444444444444444444444444444444444444444"
		pool     = "0x00000000000000000000000000000000000000c3"
	)
	addAssetFeed(t, st, 1, "NVDA", 1, strings.ToUpper(nvda[:2])+strings.ToUpper(nvda[2:]), 1, " "+strings.ToUpper(fNVDA)+" ")
	addAssetFeed(t, st, 2, "TSLA", 1, tsla, 2, fTSLA) // feed on Ethereum mainnet
	addAssetFeed(t, st, 3, "NVDA (dup)", 1, nvda, 1, fDup)
	addAssetFeed(t, st, 4, "AAPL", 1, aapl, 1, fAAPLdb)
	addAssetFeed(t, st, 5, "on Base", 3, baseTok, 1, fBase)      // not a Robinhood Chain token
	addAssetFeed(t, st, 6, "junk", 1, "not-an-address", 1, fAMD) // skipped

	f := ltChain(t, 20*ltDay)
	f.addToken(nvda, 18, "NVDA")
	f.constCall(fNVDA, selDecimals, ret(wInt(8)))
	f.constCall(fNVDA, selLatestRound, ret(wInt(1), wInt(150e8), wInt(0), wInt(0), wInt(1)))
	logs := captureLog(t)
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed+","+aapl+"="+fAAPLenv)
	o := s.onchain
	s.reloadFeeds(ctx)
	fm := o.feedMaps()
	for _, c := range []struct {
		m         map[string]string
		key, want string
	}{
		{fm.rh, nvda, fNVDA}, // lower-cased, trimmed, lowest asset id
		{fm.rh, aapl, fAAPLenv},
		{fm.rh, "eth", ltFeed},
		{fm.rh, baseTok, ""},
		{fm.rh, tsla, ""},
		{fm.mainnet, tsla, fTSLA},
		{fm.mainnet, "eth", mainnetEthUsdFeed},
	} {
		if got := c.m[c.key]; got != c.want {
			t.Errorf("feed of %s: got %q, want %q", c.key, got, c.want)
		}
	}
	if len(fm.rh) != 3 || fm.dbRH != 1 || fm.dbMainnet != 1 {
		t.Fatalf("Robinhood feeds %v (%d from the db), mainnet %v (%d from the db): want 3 (1) and (1)", fm.rh, fm.dbRH, fm.mainnet, fm.dbMainnet)
	}
	if out := logs.String(); !strings.Contains(out, "1 overridden by SCOUT_*CHAINLINK_FEEDS") || !strings.Contains(out, "1 row(s) with a malformed address skipped") {
		t.Fatalf("load line:\n%s", out)
	}
	if q, ok, err := o.quoteUSD(ctx, nvda, f.latest-864000); err != nil || !ok || q != 150 {
		t.Fatalf("NVDA through the db feed: got %v %v %v, want $150", q, ok, err)
	}

	// The tracker: a token paired with NVDA (feed only in the db) is in USD.
	f.ltPool(token, pool, nvda)
	c := rugCall{entry: time.Now().Add(-2 * time.Hour)}
	c.eb = f.blockAtTime(c.entry)
	f.ltTrade(token, pool, c.at(-time.Minute), 0.001)
	f.ltTrade(token, pool, c.at(30*time.Minute), 0.002)
	s.onChannelPost(postAt(1, c.entry, token))
	c.id = *(<-s.queue).CallID
	s.trackCycle(ctx, 50)
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.PriceUnit == nil || *tr.PriceUnit != "usd" || tr.EntryPriceUSD == nil || !relNear(*tr.EntryPriceUSD, 0.15) {
		t.Fatalf("stock-token pair: got unit %s entry %v (err %s), want usd $0.15", strOrNil(tr.PriceUnit), rugF(tr.EntryPriceUSD), strOrNil(tr.Error))
	}

	// Reload: a new row is picked up in a new map; the old map is unchanged.
	addAssetFeed(t, st, 7, "AMD", 1, amd, 1, fAMD)
	old := o.feedMaps()
	s.reloadFeeds(ctx)
	if got := o.feedMaps().rh[amd]; got != fAMD {
		t.Fatalf("after adding AMD: got %q, want %q", got, fAMD)
	}
	if _, ok := old.rh[amd]; ok {
		t.Fatal("the map in use before the reload was changed in place")
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM asset_chains WHERE asset_id IN (1, 3)`); err != nil {
		t.Fatal(err)
	}
	s.reloadFeeds(ctx)
	if got := o.feedMaps().rh[nvda]; got != "" {
		t.Fatalf("after deleting NVDA's rows: got %q, want none", got)
	}

	// The tables go away: the last good read stays, logged once.
	if _, err := st.Pool.Exec(ctx, `DROP TABLE asset_chains, assets, chains`); err != nil {
		t.Fatal(err)
	}
	s.reloadFeeds(ctx)
	s.reloadFeeds(ctx)
	if got := o.feedMaps().rh[amd]; got != fAMD {
		t.Fatalf("tables gone: got %q for AMD, want the last read's %q", got, fAMD)
	}
	if k := strings.Count(logs.String(), "no asset tables"); k != 1 {
		t.Fatalf("got %d 'no asset tables' lines, want 1:\n%s", k, logs.String())
	}

	// A database that never had them: env only.
	s2 := usdScanner(t, st, f.srv.URL, "eth="+ltFeed+","+aapl+"="+fAAPLenv)
	s2.reloadFeeds(ctx)
	if fm := s2.onchain.feedMaps(); len(fm.rh) != 2 || fm.rh[aapl] != fAAPLenv || fm.dbRH != 0 {
		t.Fatalf("no tables: got %v (%d from the db), want the 2 env feeds", fm.rh, fm.dbRH)
	}
	if !strings.Contains(logs.String(), "using the environment's feeds only") {
		t.Fatalf("no env-only line:\n%s", logs.String())
	}
}

// The data-loss regression: the USD conversion of a horizon's segment fails
// after the swaps were scanned. Nothing of the segment is kept (cursor,
// extremes, candles); the next run reads it again, so the peak and every
// candle are there, each trade counted once.
func TestTrackerSegmentKeptWhenUSDFails(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		quote = "0x2222222222222222222222222222222222222222"
		qFeed = "0x00000000000000000000000000000000000000f8"
		token = "0x4444444444444444444444444444444444444444"
		pool  = "0x00000000000000000000000000000000000000c3"
	)
	f := ltChain(t, 20*ltDay)
	f.addToken(quote, 18, "QT")
	f.constCall(qFeed, selDecimals, ret(wInt(8)))
	f.constCall(qFeed, selLatestRound, ret(wInt(1), wInt(2e8), wInt(0), wInt(0), wInt(1))) // QT = $2
	f.ltPool(token, pool, quote)
	c := rugCall{entry: time.Now().Add(-2 * ltDay)}
	c.eb = f.blockAtTime(c.entry)
	f.ltTrade(token, pool, c.at(-time.Minute), 1e-3) // entry
	f.ltTrade(token, pool, c.at(30*time.Second), 1.2e-3)
	f.ltTrade(token, pool, c.at(33*time.Minute), 1.5e-3)
	f.ltTrade(token, pool, c.at(5*time.Hour), 5e-3) // the peak, in the segment that fails
	f.ltTrade(token, pool, c.at(8*time.Hour), 2e-3)
	f.ltTrade(token, pool, c.at(20*time.Hour), 1.8e-3) // the 1d price
	busyFrom, busyTo := c.at(3*time.Hour), c.at(6*time.Hour)
	busy := true
	f.callErr = func(to, sel string, block uint64) string {
		if busy && to == qFeed && sel == selLatestRound && block >= busyFrom && block <= busyTo {
			return "rate limit exceeded"
		}
		return ""
	}
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed+","+quote+"="+qFeed)
	s.onChannelPost(postAt(1, c.entry, token))
	c.id = *(<-s.queue).CallID
	events := func() (fine, hourly int) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(sum(events) FILTER (WHERE interval_seconds = 300), 0)::int,
			COALESCE(sum(events) FILTER (WHERE interval_seconds = 3600), 0)::int FROM scout_call_candles WHERE call_id = $1`, c.id).Scan(&fine, &hourly); err != nil {
			t.Fatal(err)
		}
		return fine, hourly
	}

	// run 1: 1h is stored, the 1d segment fails on the QT price of hour 5
	s.trackDue(ctx, 50)
	tr, _ := st.GetTracking(ctx, c.id)
	os := ltState(t, st, c.id)
	if tr.Status != TrackError || tr.Error == nil || !strings.Contains(*tr.Error, "candles to +1d") {
		t.Fatalf("run 1: got status %s error %s, want error on the 1d candles", tr.Status, strOrNil(tr.Error))
	}
	if !os.Done["1h"] || os.Done["1d"] || os.ScanBlock >= c.at(5*time.Hour) || os.RunMaxQ >= 5e-3 {
		t.Fatalf("run 1: state %+v, want 1h done and the 1d segment (peak at block %d) not kept", os, c.at(5*time.Hour))
	}
	if fine, hourly := events(); fine != 2 || hourly != 2 {
		t.Fatalf("run 1: got %d / %d candle events, want 2 / 2 (the 1h segment only)", fine, hourly)
	}

	// run 2: the node answers again
	busy = false
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	s.trackDue(ctx, 50)
	tr, _ = st.GetTracking(ctx, c.id)
	if tr.Status != TrackTracking || tr.Error != nil {
		t.Fatalf("run 2: got status %s error %s, want tracking", tr.Status, strOrNil(tr.Error))
	}
	r := rugReturns(t, st, c.id)["1d"]
	if !relNear(r.MaxPriceUSD, 5e-3*2) || !relNear(r.PriceUSD, 1.8e-3*2) {
		t.Fatalf("1d: got peak %v price %v, want %v %v", r.MaxPriceUSD, r.PriceUSD, 5e-3*2, 1.8e-3*2)
	}
	if fine, hourly := events(); fine != 5 || hourly != 5 {
		t.Fatalf("run 2: got %d / %d candle events, want 5 / 5 (each trade once)", fine, hourly)
	}
	if hi := rugHigh(t, st, c.id); !relNear(hi, 5e-3*2) {
		t.Fatalf("candle high: got %v, want %v", hi, 5e-3*2)
	}
}

// ORBIO-like: a Pons curve quoted in a token with no feed, priced through the
// token's own WETH pool: tracked in USD.
func TestTrackerPonsCurveQuotedInPricedToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const orb, vpool = "0x3333333333333333333333333333333333333333", "0x00000000000000000000000000000000000000c9"
	f := ltChain(t, 20*ltDay)
	f.addToken(orb, 18, "ORBIO")
	c := rugCall{entry: time.Now().Add(-2 * time.Hour)}
	c.eb = f.blockAtTime(c.entry)
	vp := newUSDTestPool(f, vpool, orb, tWETH, 18, 18)
	vp.trade(c.at(-3*time.Hour), 0.0005) // ORBIO = 0.0005 WETH = $1.50
	vp.trade(c.at(-10*time.Minute), 0.0005)
	fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, orb, 18, 1.5, 1e9, false)
	entry := fp.buy(c.at(-time.Minute), e18(5))
	fp.buy(c.at(30*time.Minute), e18(5))
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed)
	s.onChannelPost(postAt(1, c.entry, fp.token))
	c.id = *(<-s.queue).CallID
	s.trackDue(ctx, 50)
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.PriceUnit == nil || *tr.PriceUnit != "usd" || tr.EntryPriceUSD == nil || !relNear(*tr.EntryPriceUSD, entry*1.5) || tr.Error != nil {
		t.Fatalf("got unit %s entry %v error %s, want usd $%v", strOrNil(tr.PriceUnit), rugF(tr.EntryPriceUSD), strOrNil(tr.Error), entry*1.5)
	}
	if got := s.onchain.quoteSource(orb); got != "its WETH pool (uniswap-v3)" {
		t.Fatalf("ORBIO's USD source: got %q, want its WETH pool", got)
	}
}

// An entry whose quote asset has pools but no trade yet before the call is a
// temporary error (retried), not a switch to quote units; once a price exists
// (after the "no price yet" cache expires) the call is tracked in USD.
func TestTrackerEntryNoPriceYetRetries(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		quote = "0x2222222222222222222222222222222222222222"
		qpool = "0x00000000000000000000000000000000000000c8" // QT / USDG
		token = "0x4444444444444444444444444444444444444444"
		pool  = "0x00000000000000000000000000000000000000c3"
	)
	f := ltChain(t, 20*ltDay)
	f.addToken(tUSDG, 6, "USDG")
	f.addToken(quote, 18, "QT")
	f.ltPool(token, pool, quote)
	c := rugCall{entry: time.Now().Add(-2 * time.Hour)}
	c.eb = f.blockAtTime(c.entry)
	qp := newUSDTestPool(f, qpool, quote, tUSDG, 18, 6)
	qp.trade(c.at(10*time.Minute), 3) // QT's first trade: after the call
	f.ltTrade(token, pool, c.at(-time.Minute), 0.01)
	f.ltTrade(token, pool, c.at(20*time.Minute), 0.02)
	clk := &fakeClock{t: time.Now()}
	s := usdScanner(t, st, f.srv.URL, "eth="+ltFeed)
	s.onchain.now = clk.now
	s.onChannelPost(postAt(1, c.entry, token))
	c.id = *(<-s.queue).CallID
	s.trackDue(ctx, 50)
	tr, _ := st.GetTracking(ctx, c.id)
	if tr.Status != TrackError || tr.PriceUnit != nil || tr.Error == nil || !strings.Contains(*tr.Error, "no price yet") {
		t.Fatalf("run 1: got status %s unit %s error %s, want an error (no price yet) and no unit", tr.Status, strOrNil(tr.PriceUnit), strOrNil(tr.Error))
	}
	qp.trade(c.at(-30*time.Minute), 2.5) // an earlier trade turns up
	clk.add(usdNoPriceTTL + time.Minute)
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	s.trackDue(ctx, 50)
	tr, _ = st.GetTracking(ctx, c.id)
	if tr.PriceUnit == nil || *tr.PriceUnit != "usd" || tr.EntryPriceUSD == nil || !relNear(*tr.EntryPriceUSD, 0.01*2.5) {
		t.Fatalf("run 2: got unit %s entry %v error %s, want usd $%v", strOrNil(tr.PriceUnit), rugF(tr.EntryPriceUSD), strOrNil(tr.Error), 0.01*2.5)
	}
}
