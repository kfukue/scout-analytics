package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass: fake chain end to end, scheduling, interruption.
// ---------------------------------------------------------------------------

const (
	ltFeed  = "0x00000000000000000000000000000000000000fe"
	ltDay   = 24 * time.Hour
	ltETH   = 3000.0 // the fake ETH/USD feed
	ltBuyer = "0x00000000000000000000000000000000000000d1"
)

// ltChain is a fake chain with WETH and an ETH/USD feed at $3000.
func ltChain(t *testing.T, age time.Duration) *fakeChain {
	t.Helper()
	f := newFakeChain(t, age)
	f.addToken(tWETH, 18, "WETH")
	f.constCall(ltFeed, selDecimals, ret(wInt(8)))
	f.constCall(ltFeed, selLatestRound, ret(wInt(1), wInt(3000e8), wInt(0), wInt(0), wInt(1)))
	return f
}

// ltScanner is a tracker over st that reads the chain at rpcURL.
func ltScanner(t *testing.T, st *ScoutStore, rpcURL string) *scanner {
	t.Helper()
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", rpcURL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_CHAINLINK_FEEDS", "eth="+ltFeed)
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

// ltPool sets up a v3 pool quote / token (the token is token1).
func (f *fakeChain) ltPool(token, pool, quote string) {
	f.addToken(token, 18, "TKN")
	f.constCall(pool, selToken0, ret(wAddr(quote)))
	f.constCall(pool, selToken1, ret(wAddr(token)))
	f.constCall(pool, selSlot0, ret(wInt(1)))
}

// ltTrade adds a buy at block that leaves the pool price at p quote units.
func (f *fakeChain) ltTrade(token, pool string, block uint64, p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swapV3Amt(pool, block, sqrtX96(1/p), wei(2), big.NewInt(-10))
	f.transfer(token, pool, ltBuyer, block, hexU64(block))
}

// advance moves the chain's newest block forward.
func (f *fakeChain) advance(blocks uint64) {
	f.mu.Lock()
	f.latest += blocks
	f.mu.Unlock()
}

func (f *fakeChain) head() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest
}

// took returns how many requests of a method arrived since the last call, and
// starts counting again.
func (f *fakeChain) took(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.count[method]
	f.count = map[string]int{}
	return n
}

type ltCols struct {
	Price, Ret     *float64
	Checked, Trade *time.Time
}

func ltRead(t *testing.T, st *ScoutStore, id int) ltCols {
	t.Helper()
	var c ltCols
	if err := st.Pool.QueryRow(context.Background(), `SELECT latest_price_usd::float8, latest_return_pct::float8, latest_checked_at, latest_trade_at
		FROM scout_call_tracking WHERE call_id = $1`, id).Scan(&c.Price, &c.Ret, &c.Checked, &c.Trade); err != nil {
		t.Fatal(err)
	}
	return c
}

func ltNear(a *float64, want float64) bool {
	return a != nil && math.Abs(*a-want) <= 1e-9*math.Max(1, math.Abs(want))
}

// ltMakeDue moves the last refresh of every row back, so all are due again.
func ltMakeDue(t *testing.T, st *ScoutStore, by string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking SET latest_checked_at = latest_checked_at - $1::text::interval
		WHERE latest_checked_at IS NOT NULL`, by); err != nil {
		t.Fatal(err)
	}
}

// ltHorizonSide is everything of a call that belongs to horizon tracking: the
// latest-price pass must leave all of it exactly as it was.
type ltHorizonSide struct {
	Row     string // the tracking row without the latest columns and the state
	State   map[string]any
	Returns string
	Candles string
}

func ltSide(t *testing.T, st *ScoutStore, id int) ltHorizonSide {
	t.Helper()
	ctx := context.Background()
	var out ltHorizonSide
	var state []byte
	if err := st.Pool.QueryRow(ctx, `SELECT (to_jsonb(t) - 'onchain' - 'latest_price_usd' - 'latest_return_pct' - 'latest_checked_at' - 'latest_trade_at')::text, onchain
		FROM scout_call_tracking t WHERE call_id = $1`, id).Scan(&out.Row, &state); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state, &out.State); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{latestStateKeyBlock, latestStateKeyPrice, latestStateKeyTrades} {
		delete(out.State, k)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY horizon_seconds)::text, '') FROM scout_call_returns r WHERE call_id = $1`, id).Scan(&out.Returns); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY interval_seconds, bucket_start)::text, '') FROM scout_call_candles c WHERE call_id = $1`, id).Scan(&out.Candles); err != nil {
		t.Fatal(err)
	}
	return out
}

func ltState(t *testing.T, st *ScoutStore, id int) onchainState {
	t.Helper()
	tr, err := st.GetTracking(context.Background(), id)
	if err != nil || tr == nil {
		t.Fatalf("tracking %d: %v", id, err)
	}
	var os onchainState
	if err := json.Unmarshal(tr.Onchain, &os); err != nil {
		t.Fatal(err)
	}
	return os
}

// ltAPI asks the website (a fresh snapshot of st) for /api/calls?query.
func ltAPI(t *testing.T, st *ScoutStore, query string) map[int]webCallJSON {
	t.Helper()
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	ws := mustWebServer(t, st, testWebConfig, static)
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, httptest.NewRequest("GET", "/api/calls?"+query, nil))
	if rec.Code != 200 {
		t.Fatalf("/api/calls?%s: %d %s", query, rec.Code, rec.Body)
	}
	var res webCallsJSON
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		t.Fatal(err)
	}
	out := map[int]webCallJSON{}
	for _, c := range res.Calls {
		out[c.CallID] = c
	}
	return out
}

func ltTime(t *testing.T, s *string) time.Time {
	t.Helper()
	if s == nil {
		t.Fatal("time is null")
	}
	at, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// A call older than 30 days with every horizon done, and trades after the
// 30-day mark: the pass stores the latest price, return and times, leaves the
// horizon side alone, costs little on the next refresh and follows a new trade.
func TestLatestPriceOldCall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 70*ltDay)
	token, pool := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	f.ltPool(token, pool, tWETH)
	entry := time.Now().Add(-60 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	f.ltTrade(token, pool, at(-2*time.Minute), 1e-6) // entry $0.003
	f.ltTrade(token, pool, at(30*time.Second), 2e-6) // late entry $0.006
	f.ltTrade(token, pool, at(10*time.Minute), 3e-6) //
	f.ltTrade(token, pool, at(2*ltDay), 4e-6)        //
	f.ltTrade(token, pool, at(20*ltDay), 2e-6)       // the 30d mark: $0.006
	f.ltTrade(token, pool, at(40*ltDay), 5e-6)       // after the last horizon
	f.ltTrade(token, pool, at(50*ltDay), 9e-6)       // latest: $0.027 = +350% from the late entry
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID

	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, id)
	rs, _ := st.ReturnsForCall(ctx, id)
	if tr.Status != TrackDone || len(rs) != 5 || rs["30d"].ReturnLatePct == nil || !near(*rs["30d"].ReturnLatePct, 0) {
		t.Fatalf("horizons: status %s, %v, err %v", tr.Status, sortedKeys(rs), strOrNil(tr.Error))
	}
	if c := ltRead(t, st, id); c.Price != nil || c.Ret != nil || c.Checked != nil || c.Trade != nil {
		t.Fatalf("latest columns before the first pass: %+v", c)
	}
	before := ltSide(t, st, id)
	scanBlock := ltState(t, st, id).ScanBlock

	// First refresh: everything from the 30-day mark to now.
	f.took("")
	res := s.refreshLatest(ctx, false)
	first := f.took("eth_getLogs")
	if res.Due != 1 || res.Refreshed != 1 || res.Changed != 1 || res.Failed != 0 || res.Waiting != 0 {
		t.Fatalf("first pass %+v", res)
	}
	blocks := f.head() - scanBlock
	t.Logf("60-day-old call, first refresh: %d eth_getLogs for %d blocks (%d requests in all)", first, blocks, res.Requests)
	if want := int(blocks/200_000) + 1; first < want-1 || first > want+1 {
		t.Fatalf("first refresh made %d eth_getLogs requests, want about %d", first, want)
	}
	c := ltRead(t, st, id)
	if !ltNear(c.Price, 9e-6*ltETH) || !ltNear(c.Ret, 350) || c.Checked == nil || time.Since(*c.Checked) > time.Minute || time.Since(*c.Checked) < 0 ||
		c.Trade == nil || c.Trade.Sub(entry.Add(50*ltDay)).Abs() > 5*time.Second {
		t.Fatalf("latest after the first pass: %v %v %v %v", fnum(c.Price), fnum(c.Ret), c.Checked, c.Trade)
	}
	os := ltState(t, st, id)
	if os.LatestBlock != f.head() || !ltNear(&os.LatestPriceQ, 9e-6) || os.LatestTradeBlock != at(50*ltDay) ||
		os.V != onchainStateVersion || os.ScanBlock != scanBlock || !ltNear(&os.LastPriceQ, 2e-6) {
		t.Fatalf("state after the first pass: %+v", os)
	}
	if after := ltSide(t, st, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("the pass changed the horizon side:\n got %+v\nwant %+v", after, before)
	}
	// On the website: the return, and how old the call was when the price was read.
	row := ltAPI(t, st, "sort=latest")[id]
	if !ltNear(row.LatestReturnPct, 350) || !ltNear(row.LatestPriceUSD, 9e-6*ltETH) || row.LatestAgeSeconds == nil ||
		math.Abs(float64(*row.LatestAgeSeconds)-60*86400) > 120 || ltTime(t, row.LatestAt).Sub(*c.Checked).Abs() > time.Second ||
		ltTime(t, row.LatestTradeAt).Sub(*c.Trade).Abs() > time.Second {
		t.Fatalf("website row %+v", row)
	}
	// In the dataset view: the three columns, at the end.
	var cols []string
	if err := st.Pool.QueryRow(ctx, `SELECT array_agg(column_name::text ORDER BY ordinal_position) FROM information_schema.columns
		WHERE table_name = 'scout_call_dataset_v'`).Scan(&cols); err != nil || len(cols) < 3 ||
		strings.Join(cols[len(cols)-3:], ",") != "latest_price_usd,latest_return_pct,latest_checked_at" {
		t.Fatalf("dataset view columns end with %v (%v)", cols, err)
	}
	var vp, vr float64
	if err := st.Pool.QueryRow(ctx, `SELECT latest_price_usd, latest_return_pct FROM scout_call_dataset_v WHERE call_id = $1 AND latest_checked_at IS NOT NULL`, id).Scan(&vp, &vr); err != nil ||
		!ltNear(&vp, 9e-6*ltETH) || !ltNear(&vr, 350) {
		t.Fatalf("dataset view: %v %v %v", vp, vr, err)
	}

	// Not due again until a day has gone by.
	if res := s.refreshLatest(ctx, false); res.Due != 0 || f.took("eth_getLogs") != 0 {
		t.Fatalf("second pass right away: %+v", res)
	}
	// A day later, no new trades: the small follow-up scan, the same price.
	ltMakeDue(t, st, "25 hours")
	f.advance(864_000)
	f.took("")
	res = s.refreshLatest(ctx, false)
	follow := f.took("eth_getLogs")
	t.Logf("60-day-old call, daily follow-up: %d eth_getLogs for 864000 blocks (%d requests in all)", follow, res.Requests)
	if res.Refreshed != 1 || res.Changed != 0 || res.Failed != 0 || follow < 4 || follow > 6 {
		t.Fatalf("follow-up pass %+v with %d eth_getLogs", res, follow)
	}
	c2 := ltRead(t, st, id)
	if !ltNear(c2.Price, 9e-6*ltETH) || !ltNear(c2.Ret, 350) || !c2.Trade.Equal(*c.Trade) || time.Since(*c2.Checked) > time.Minute {
		t.Fatalf("latest after the follow-up: %v %v %v %v", fnum(c2.Price), fnum(c2.Ret), c2.Checked, c2.Trade)
	}
	if os := ltState(t, st, id); os.LatestBlock != f.head() || os.LatestTradeBlock != at(50*ltDay) {
		t.Fatalf("state after the follow-up: %+v", os)
	}

	// A new trade moves it.
	f.ltTrade(token, pool, f.head()+100, 1.2e-5)
	f.advance(9000)
	ltMakeDue(t, st, "25 hours")
	f.took("")
	res = s.refreshLatest(ctx, false)
	if n := f.took("eth_getLogs"); res.Refreshed != 1 || res.Changed != 1 || n < 1 || n > 2 {
		t.Fatalf("pass after a new trade %+v with %d eth_getLogs", res, n)
	}
	c3 := ltRead(t, st, id)
	if !ltNear(c3.Price, 1.2e-5*ltETH) || !ltNear(c3.Ret, 500) || !c3.Trade.After(*c2.Trade) {
		t.Fatalf("latest after a new trade: %v %v %v", fnum(c3.Price), fnum(c3.Ret), c3.Trade)
	}
	if after := ltSide(t, st, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("three passes changed the horizon side:\n got %+v\nwant %+v", after, before)
	}
	// … and the horizon tracker still has nothing to do with this call.
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("the call became due for horizon tracking: %d", n)
	}
}

// A call that is still being tracked: the pass runs, and the horizons that
// arrive later come out exactly as for a twin call the pass never touched.
func TestLatestPriceTrackingCallKeepsLaterHorizons(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	tokenA, poolA := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	tokenB, poolB := "0x5555555555555555555555555555555555555555", "0x00000000000000000000000000000000000000c4"
	entry := time.Now().Add(-2 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	for _, p := range [][2]string{{tokenA, poolA}, {tokenB, poolB}} {
		f.ltPool(p[0], p[1], tWETH)
		f.ltTrade(p[0], p[1], at(-2*time.Minute), 1e-6) // entry $0.003
		f.ltTrade(p[0], p[1], at(30*time.Second), 2e-6) // late entry $0.006
		f.ltTrade(p[0], p[1], at(2*time.Hour), 6e-6)    // peak $0.018
		f.ltTrade(p[0], p[1], at(20*time.Hour), 3e-6)   // the 1d mark
		f.ltTrade(p[0], p[1], at(30*time.Hour), 4e-6)   // not scanned by the horizons yet
		f.ltTrade(p[0], p[1], at(40*time.Hour), 8e-6)   // latest: $0.024 = +300%
	}
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, entry, tokenA))
	a := *(<-s.queue).CallID
	s.onChannelPost(postAt(2, entry, tokenB))
	b := *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("processed %d", n)
	}
	for _, id := range []int{a, b} {
		tr, _ := st.GetTracking(ctx, id)
		if rs, _ := st.ReturnsForCall(ctx, id); tr.Status != TrackTracking || len(rs) != 2 {
			t.Fatalf("call %d: status %s, horizons %v, err %v", id, tr.Status, sortedKeys(rs), strOrNil(tr.Error))
		}
	}
	before := ltSide(t, st, a)
	scanBlock := ltState(t, st, a).ScanBlock

	// The pass refreshes A only: B is left out, as a row that just failed would be.
	s.latestFailed(b, time.Now())
	f.took("")
	res := s.refreshLatest(ctx, false)
	first := f.took("eth_getLogs")
	t.Logf("2-day-old call, first refresh: %d eth_getLogs for %d blocks (%d requests in all)", first, f.head()-scanBlock, res.Requests)
	if res.Due != 1 || res.Refreshed != 1 || res.Changed != 1 || res.Failed != 0 || first < 4 || first > 6 {
		t.Fatalf("first pass %+v with %d eth_getLogs", res, first)
	}
	if c := ltRead(t, st, a); !ltNear(c.Price, 8e-6*ltETH) || !ltNear(c.Ret, 300) || c.Trade.Sub(entry.Add(40*time.Hour)).Abs() > 5*time.Second {
		t.Fatalf("latest of A: %v %v %v", fnum(c.Price), fnum(c.Ret), c.Trade)
	}
	if c := ltRead(t, st, b); c.Price != nil || c.Checked != nil {
		t.Fatalf("B was refreshed: %+v", c)
	}
	if after := ltSide(t, st, a); !reflect.DeepEqual(after, before) {
		t.Fatalf("the pass changed the horizon side of A:\n got %+v\nwant %+v", after, before)
	}
	if os := ltState(t, st, a); os.ScanBlock != scanBlock || !ltNear(&os.LastPriceQ, 3e-6) || os.LatestBlock != f.head() || len(os.Done) != 2 {
		t.Fatalf("state of A: %+v", os)
	}
	// 15 minutes later: one small request.
	ltMakeDue(t, st, "16 minutes")
	f.advance(9000)
	f.took("")
	res = s.refreshLatest(ctx, false)
	follow := f.took("eth_getLogs")
	t.Logf("2-day-old call, 15-minute follow-up: %d eth_getLogs for 9000 blocks (%d requests in all)", follow, res.Requests)
	if res.Refreshed != 1 || res.Changed != 0 || follow < 1 || follow > 2 {
		t.Fatalf("follow-up pass %+v with %d eth_getLogs", res, follow)
	}

	// The 3-day horizon arrives (the calls are made 36 hours older): its block is
	// 36 hours after the post, between the +30h and the +40h trade.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET message_date = message_date - interval '36 hours';
		UPDATE scout_call_tracking SET entry_at = entry_at - interval '36 hours', next_check_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 2 {
		t.Fatalf("3d: processed %d", n)
	}
	ra, _ := st.ReturnsForCall(ctx, a)
	rb, _ := st.ReturnsForCall(ctx, b)
	// $0.012 at the mark: +300% from the call, +100% from the late entry; peak $0.018.
	if r := ra["3d"]; len(ra) != 3 || !near(r.ReturnPct, 300) || r.ReturnLatePct == nil || !near(*r.ReturnLatePct, 100) ||
		!near(*r.MaxGainLatePct, 200) || !near(r.MaxGainPct, 500) || !near(r.PriceUSD, 4e-6*ltETH) {
		t.Fatalf("3d of A: %+v (%v)", r, sortedKeys(ra))
	}
	for _, h := range []string{"1h", "1d", "3d"} {
		x, y := ra[h], rb[h]
		if x.PriceUSD != y.PriceUSD || x.ReturnPct != y.ReturnPct || x.MaxGainPct != y.MaxGainPct || x.MaxDDPct != y.MaxDDPct ||
			*x.ReturnLatePct != *y.ReturnLatePct || *x.MaxGainLatePct != *y.MaxGainLatePct || *x.MaxDDLatePct != *y.MaxDDLatePct ||
			!x.LastTradeAt.Equal(*y.LastTradeAt) || !x.DueAt.Equal(y.DueAt) {
			t.Fatalf("%s differs between the refreshed call and its twin:\n%+v\n%+v", h, x, y)
		}
	}
	sa, sb := ltState(t, st, a), ltState(t, st, b)
	if sa.LatestBlock == 0 || sb.LatestBlock != 0 {
		t.Fatalf("latest cursors: A %d, B %d", sa.LatestBlock, sb.LatestBlock)
	}
	sa.LatestBlock, sa.LatestPriceQ, sa.LatestTradeBlock = 0, 0, 0
	sa.Pool, sb.Pool = "", ""
	if !reflect.DeepEqual(sa, sb) {
		t.Fatalf("horizon state differs:\n A %+v\n B %+v", sa, sb)
	}
	var ca, cb int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE call_id = $1), count(*) FILTER (WHERE call_id = $2) FROM scout_call_candles`, a, b).Scan(&ca, &cb); err != nil || ca != cb || ca == 0 {
		t.Fatalf("candles: %d and %d (%v)", ca, cb, err)
	}

	// The horizon scan now stands before the pass's cursor: the pass keeps its own price.
	ltMakeDue(t, st, "16 minutes")
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Changed != 0 {
		t.Fatalf("pass after the 3d horizon: %+v", res)
	}
	if c := ltRead(t, st, a); !ltNear(c.Price, 8e-6*ltETH) || !ltNear(c.Ret, 300) {
		t.Fatalf("latest of A after the 3d horizon: %v %v", fnum(c.Price), fnum(c.Ret))
	}
	// And when the horizon scan has moved past the pass's cursor, the pass goes
	// on from the horizon scan's block and price, not from its own older ones.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET latest_checked_at = now() - interval '1 hour',
		onchain = onchain || '{"latest_block": 5, "latest_price_q": 123, "latest_trade_block": 5}'::jsonb WHERE call_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	f.took("")
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Changed != 1 || f.took("eth_getLogs") > 4 {
		t.Fatalf("pass behind the horizon scan: %+v", res)
	}
	if c := ltRead(t, st, a); !ltNear(c.Price, 8e-6*ltETH) || c.Trade.Sub(entry.Add(40*time.Hour)).Abs() > 5*time.Second {
		t.Fatalf("latest of A behind the horizon scan: %v %v", fnum(c.Price), c.Trade)
	}
}

// A dead token: no trade after the fifth day. The latest price is the last
// known one, and the trade behind it is weeks older than the check ("quiet").
func TestLatestPriceDeadToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 70*ltDay)
	token, pool := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	f.ltPool(token, pool, tWETH)
	entry := time.Now().Add(-60 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	f.ltTrade(token, pool, at(-2*time.Minute), 1e-6)
	f.ltTrade(token, pool, at(30*time.Second), 2e-6) // late entry $0.006
	f.ltTrade(token, pool, at(ltDay), 1e-6)
	f.ltTrade(token, pool, at(5*ltDay), 5e-7) // the last trade: $0.0015 = −75%
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	if tr, _ := st.GetTracking(ctx, id); tr.Status != TrackDone {
		t.Fatalf("status %s err %v", tr.Status, strOrNil(tr.Error))
	}
	before := ltSide(t, st, id)
	res := s.refreshLatest(ctx, false)
	if res.Refreshed != 1 || res.Changed != 0 || res.Failed != 0 {
		t.Fatalf("pass %+v", res)
	}
	c := ltRead(t, st, id)
	os := ltState(t, st, id)
	if !ltNear(c.Price, os.LastPriceQ*ltETH) || !ltNear(c.Price, 5e-7*ltETH) || !ltNear(c.Ret, -75) ||
		c.Trade.Sub(entry.Add(5*ltDay)).Abs() > 5*time.Second || c.Checked.Sub(*c.Trade) < 54*ltDay {
		t.Fatalf("latest: %v %v checked %v trade %v", fnum(c.Price), fnum(c.Ret), c.Checked, c.Trade)
	}
	if after := ltSide(t, st, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("the pass changed the horizon side")
	}
	// what the page calls quiet: the last trade is more than 7 days older than the check
	row := ltAPI(t, st, "sort=latest")[id]
	if quiet := ltTime(t, row.LatestAt).Sub(ltTime(t, row.LatestTradeAt)); quiet <= 7*ltDay || !ltNear(row.LatestReturnPct, -75) {
		t.Fatalf("website row %+v (last trade %s before the check)", row, quiet)
	}
}

// A pair without a USD price: the latest price and return are stored in quote
// units, and the website shows none of it.
func TestLatestPriceNonUSDPair(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	qqq := "0x2222222222222222222222222222222222222222" // no feed, no pool of its own
	token, pool := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	f.addToken(qqq, 18, "QQQ")
	f.ltPool(token, pool, qqq)
	entry := time.Now().Add(-2 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	f.ltTrade(token, pool, at(-2*time.Minute), 2)
	f.ltTrade(token, pool, at(30*time.Second), 4) // late entry: 4 QQQ
	f.ltTrade(token, pool, at(20*time.Hour), 5)
	f.ltTrade(token, pool, at(30*time.Hour), 10) // latest: 10 QQQ = +150%
	s := ltScanner(t, st, f.srv.URL)
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	tr, _ := st.GetTracking(ctx, id)
	if tr.Status != TrackTracking || tr.PriceUnit == nil || *tr.PriceUnit != "QQQ" || !ltNear(tr.EntryLatePriceUSD, 4) {
		t.Fatalf("tracking %+v err %v", tr, strOrNil(tr.Error))
	}
	f.took("")
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass %+v", res)
	}
	if n := f.took("eth_call"); n != 0 {
		t.Fatalf("%d eth_call for a pair that has no USD price", n)
	}
	if c := ltRead(t, st, id); !ltNear(c.Price, 10) || !ltNear(c.Ret, 150) || c.Checked == nil || c.Trade == nil {
		t.Fatalf("latest: %v %v %v %v", fnum(c.Price), fnum(c.Ret), c.Checked, c.Trade)
	}
	row, ok := ltAPI(t, st, "sort=latest&usd_only=0")[id]
	if !ok || strOrNil(row.PriceUnit) != "QQQ" || row.LatestReturnPct != nil || row.LatestPriceUSD != nil || row.LatestAt != nil ||
		row.LatestTradeAt != nil || row.LatestAgeSeconds != nil {
		t.Fatalf("website row %+v (%v)", row, ok)
	}
	if _, listed := ltAPI(t, st, "sort=latest")[id]; listed {
		t.Fatal("sort=latest lists a call without a USD price by default")
	}
}

// ltSeed is one tracking row stored directly, for the scheduling tests.
type ltSeed struct {
	Msg     int
	CA      string
	Age     time.Duration
	Status  string
	Checked time.Duration // latest price read this long ago (0 = never)
	State   string        // on-chain state ("" = NULL)
	NoEntry bool
}

func ltCA(n int) string { return fmt.Sprintf("0x%040x", 0xabc000+n) }

func ltSeedRow(t *testing.T, st *ScoutStore, w ltSeed) int {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-w.Age)
	id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 777, ChannelUsername: "scoutrobinhood", MessageID: w.Msg,
		MessageDate: at, ContractAddress: w.CA, Chain: "evm", Status: CallStatusBackfill})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTracking(ctx, *id, w.CA, at, 1, time.Now().Add(100*ltDay)); err != nil {
		t.Fatal(err)
	}
	var entry, checked any
	if !w.NoEntry {
		entry = 2.0
	}
	if w.Checked != 0 {
		checked = time.Now().Add(-w.Checked)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = $2, price_unit = 'usd', entry_price_usd = $3,
		entry_late_price_usd = $3, onchain = $4::jsonb, latest_checked_at = $5 WHERE call_id = $1`,
		*id, w.Status, entry, strPtr(w.State), checked); err != nil {
		t.Fatal(err)
	}
	return *id
}

// ltStateJSON is a usable on-chain state: a USDG pool scanned up to scanBlock,
// last price 3 USDG (entry 2).
func ltStateJSON(pool string, scanBlock uint64) string {
	return fmt.Sprintf(`{"v":%d,"kind":"v3","pool":%q,"token_is_0":false,"token_dec":18,"quote":%q,"quote_sym":"USDG","quote_dec":6,
		"entry_block":100,"entry_price_q":2,"scan_block":%d,"last_price_q":3,"last_price_block":90,"late_price_q":2,"done":{"1h":true}}`,
		onchainStateVersion, pool, tUSDG, scanBlock)
}

// Which rows are due, in which order, and what one pass and one tracker cycle take.
func TestLatestPriceScheduling(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 100*ltDay)
	s := ltScanner(t, st, f.srv.URL)
	state := ltStateJSON("0x00000000000000000000000000000000000000c7", f.head()-1000)
	ids := map[string]int{}
	n := 0
	add := func(key string, w ltSeed) {
		n++
		w.Msg = n
		if w.CA == "" {
			w.CA = ltCA(n)
		}
		ids[key] = ltSeedRow(t, st, w)
	}
	add("r1", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: state})                                 // recent, never read
	add("r2", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: state, Checked: 10 * time.Minute})      // read a moment ago
	add("r3", ltSeed{Age: 29 * ltDay, Status: TrackTracking, State: state, Checked: 20 * time.Minute}) // due again
	add("o1", ltSeed{Age: 40 * ltDay, Status: TrackDone, State: state})                                // old, never read
	add("o2", ltSeed{Age: 40 * ltDay, Status: TrackDone, State: state, Checked: 20 * time.Minute})     // read today
	add("o3", ltSeed{Age: 31 * ltDay, Status: TrackDone, State: state, Checked: 25 * time.Hour})       // due again
	add("o4", ltSeed{Age: 90 * ltDay, Status: TrackDone, State: state, Checked: 3 * ltDay})            // the stalest
	// never refreshed, whatever else is true of them:
	for _, status := range []string{TrackPending, TrackNoPool, TrackError, TrackGaveUp} {
		add("x_"+status, ltSeed{Age: 5 * ltDay, Status: status, State: state})
	}
	add("x_repeat", ltSeed{Age: 2 * ltDay, Status: TrackRepeat, State: state, CA: ltCA(3)})           // a later call of r3's token
	add("x_second", ltSeed{Age: 2 * ltDay, Status: TrackDone, State: state, CA: ltCA(1)})             // a later call of r1's token, with results
	add("x_upper", ltSeed{Age: ltDay, Status: TrackDone, State: state, CA: strings.ToUpper(ltCA(4))}) // … the address in other letter case
	add("x_nostate", ltSeed{Age: 5 * ltDay, Status: TrackDone})
	add("x_nopoolstate", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: fmt.Sprintf(`{"v":%d}`, onchainStateVersion)})
	add("x_v1", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: strings.Replace(state, fmt.Sprintf(`"v":%d`, onchainStateVersion), `"v":1`, 1)})
	add("x_noentry", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: state, NoEntry: true})
	add("x_update", ltSeed{Age: 5 * ltDay, Status: TrackDone, State: state})
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls SET post_kind = 'update' WHERE id = $1`, ids["x_update"]); err != nil {
		t.Fatal(err)
	}
	names := func(rows []ScoutCallTracking) string {
		byID := map[int]string{}
		for k, id := range ids {
			byID[id] = k
		}
		var out []string
		for _, r := range rows {
			out = append(out, byID[r.CallID])
		}
		return strings.Join(out, " ")
	}
	due := func(recent, old time.Duration, recentOnly bool, skip []int, limit int) string {
		t.Helper()
		rows, err := st.DueLatest(ctx, time.Now(), recent, old, latestRecentAge, recentOnly, skip, limit)
		if err != nil {
			t.Fatal(err)
		}
		return names(rows)
	}
	const m15, h24 = 15 * time.Minute, 24 * time.Hour
	for _, c := range []struct {
		what, got, want string
	}{
		{"recent first, then the stalest", due(m15, h24, false, nil, 100), "r1 r3 o1 o4 o3"},
		{"the batch cap", due(m15, h24, false, nil, 3), "r1 r3 o1"},
		{"recent only", due(m15, h24, true, nil, 100), "r1 r3"},
		{"rows that just failed", due(m15, h24, false, []int{ids["r1"], ids["o4"]}, 100), "r3 o1 o3"},
		{"a shorter interval for recent calls", due(5*time.Minute, h24, false, nil, 100), "r1 r3 r2 o1 o4 o3"},
		{"a shorter interval for old calls", due(m15, 10*time.Minute, false, nil, 100), "r1 r3 o1 o4 o3 o2"},
		{"longer intervals", due(time.Hour, 4*ltDay, false, nil, 100), "r1 o1"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %q, want %q", c.what, c.got, c.want)
		}
	}
	if n, err := st.CountDueLatest(ctx, time.Now(), m15, h24, latestRecentAge); err != nil || n != 5 {
		t.Fatalf("waiting: %d %v", n, err)
	}
	if t.Failed() {
		t.FailNow()
	}
	read := func(key string) ltCols { return ltRead(t, st, ids[key]) }

	// SCOUT_LATEST_REFRESH=off: no pass at all.
	f.took("")
	s.pc.LatestOn = false
	if res := s.refreshLatest(ctx, false); res.Due != 0 || res.Refreshed != 0 || f.took("eth_blockNumber") != 0 || read("r1").Checked != nil {
		t.Fatalf("pass while switched off: %+v", res)
	}
	s.pc.LatestOn = true
	// … nor with the GeckoTerminal source, or with tracking switched off.
	s.pc.Source = "gecko"
	if res := s.refreshLatest(ctx, false); res.Due != 0 {
		t.Fatalf("pass with the gecko source: %+v", res)
	}
	s.pc.Source, s.pc.Enabled = "onchain", false
	if res := s.refreshLatest(ctx, false); res.Due != 0 {
		t.Fatalf("pass with tracking off: %+v", res)
	}
	s.pc.Enabled = true

	// One pass takes SCOUT_LATEST_BATCH rows, and says so in one line.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	s.pc.LatestBatch = 2
	res := s.refreshLatest(ctx, false)
	log.SetOutput(os.Stderr)
	if res.Due != 2 || res.Refreshed != 2 || res.Changed != 0 || res.Failed != 0 || res.Waiting != 3 {
		t.Fatalf("capped pass: %+v", res)
	}
	if line := logged.String(); strings.Count(line, "\n") != 1 ||
		!regexp.MustCompile(`latest prices: 2 refreshed \(0 changed\) in [0-9.]+m?s, \d+ RPC requests, 3 waiting\n$`).MatchString(line) {
		t.Fatalf("log of the pass: %q", line)
	}
	for _, k := range []string{"r1", "r3"} {
		if c := read(k); !ltNear(c.Price, 3) || !ltNear(c.Ret, 50) || c.Checked == nil || time.Since(*c.Checked) > time.Minute || c.Trade == nil {
			t.Fatalf("%s after the capped pass: %v %v %v %v", k, fnum(c.Price), fnum(c.Ret), c.Checked, c.Trade)
		}
	}
	for _, k := range []string{"o1", "x_pending", "x_second", "x_v1"} {
		if c := read(k); c.Price != nil {
			t.Fatalf("%s was refreshed", k)
		}
	}
	if c := read("o4"); c.Price != nil || time.Since(*c.Checked) < 2*ltDay {
		t.Fatalf("o4 was refreshed: %+v", c)
	}

	// A tracker cycle whose horizon batch is full refreshes calls younger than 30
	// days only; the next, quieter cycle takes the older ones.
	s.pc.LatestBatch = 200
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET latest_checked_at = now() - interval '1 hour' WHERE call_id = $1`, ids["r2"]); err != nil {
		t.Fatal(err)
	}
	// one horizon check is due: a new call of a token the chain does not know
	add("x_horizon", ltSeed{Age: 5 * ltDay, Status: TrackPending, NoEntry: true})
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET next_check_at = now() - interval '1 minute' WHERE call_id = $1`, ids["x_horizon"]); err != nil {
		t.Fatal(err)
	}
	if more := s.trackCycle(ctx, 1); !more {
		t.Fatal("a full horizon batch must report more work")
	}
	if tr, _ := st.GetTracking(ctx, ids["x_horizon"]); tr.Attempts != 1 || tr.Status == TrackPending {
		t.Fatalf("the due horizon row was not processed first: %+v", tr)
	}
	if c := read("r2"); c.Price == nil || time.Since(*c.Checked) > time.Minute {
		t.Fatalf("r2 (recent) was not refreshed in the busy cycle: %+v", c)
	}
	for _, k := range []string{"o1", "o3", "o4"} {
		if c := read(k); c.Price != nil {
			t.Fatalf("%s (old) was refreshed in the busy cycle", k)
		}
	}
	if more := s.trackCycle(ctx, 1); more {
		t.Fatal("nothing was due for the horizons in the second cycle")
	}
	for _, k := range []string{"o1", "o3", "o4"} {
		if c := read(k); !ltNear(c.Price, 3) || !ltNear(c.Ret, 50) || time.Since(*c.Checked) > time.Minute {
			t.Fatalf("%s after the quiet cycle: %v %v %v", k, fnum(c.Price), fnum(c.Ret), c.Checked)
		}
	}
	for k, id := range ids {
		if strings.HasPrefix(k, "x_") {
			if c := ltRead(t, st, id); c.Price != nil || c.Checked != nil {
				t.Fatalf("%s was refreshed: %+v", k, c)
			}
		}
	}
	if n, err := st.CountDueLatest(ctx, time.Now(), m15, h24, latestRecentAge); err != nil || n != 0 {
		t.Fatalf("waiting after both cycles: %d %v", n, err)
	}

	// A row that fails is counted, reported once with its error, left as it was,
	// and left out of the next passes for a while.
	bad := strings.Replace(state, tUSDG, "0x00000000000000000000000000000000000000bb", 1) // a quote asset without a USD price
	add("bad", ltSeed{Age: ltDay, Status: TrackDone, State: bad})
	add("good", ltSeed{Age: ltDay, Status: TrackDone, State: state})
	logged.Reset()
	log.SetOutput(&logged)
	res = s.refreshLatest(ctx, false)
	log.SetOutput(os.Stderr)
	if res.Due != 2 || res.Refreshed != 1 || res.Failed != 1 || res.Waiting != 1 ||
		!strings.Contains(res.FirstErr, fmt.Sprintf("call %d: USD price of", ids["bad"])) {
		t.Fatalf("pass with a failing row: %+v", res)
	}
	if line := logged.String(); strings.Count(line, "latest prices:") != 1 || !strings.Contains(line, "1 refreshed") ||
		!strings.Contains(line, "1 failed") || !strings.Contains(line, res.FirstErr) {
		t.Fatalf("log of the pass with a failing row: %q", line)
	}
	if c := read("bad"); c.Price != nil || c.Checked != nil {
		t.Fatalf("the failing row was written: %+v", c)
	}
	if os := ltState(t, st, ids["bad"]); os.LatestBlock != 0 {
		t.Fatalf("the failing row's state was written: %+v", os)
	}
	if res := s.refreshLatest(ctx, false); res.Due != 0 {
		t.Fatalf("the failing row was tried again at once: %+v", res)
	}
	old := latestRetryAfter
	latestRetryAfter = -time.Second // the wait is over
	defer func() { latestRetryAfter = old }()
	s.latestFailed(ids["bad"], time.Now())
	if res := s.refreshLatest(ctx, false); res.Due != 1 || res.Failed != 1 {
		t.Fatalf("the failing row after the wait: %+v", res)
	}
}

func TestLatestPriceConfig(t *testing.T) {
	keys := []string{"SCOUT_LATEST_REFRESH", "SCOUT_LATEST_REFRESH_RECENT", "SCOUT_LATEST_REFRESH_OLD", "SCOUT_LATEST_BATCH"}
	load := func(env map[string]string) (priceConfig, error) {
		for _, k := range keys {
			t.Setenv(k, env[k])
		}
		return loadPriceConfig()
	}
	pc, err := load(nil)
	if err != nil || !pc.LatestOn || pc.LatestRecent != 15*time.Minute || pc.LatestOld != 24*time.Hour || pc.LatestBatch != 200 {
		t.Fatalf("defaults: %+v %v", pc, err)
	}
	pc, err = load(map[string]string{"SCOUT_LATEST_REFRESH": "on", "SCOUT_LATEST_REFRESH_RECENT": "1m", "SCOUT_LATEST_REFRESH_OLD": "10m", "SCOUT_LATEST_BATCH": "50"})
	if err != nil || !pc.LatestOn || pc.LatestRecent != time.Minute || pc.LatestOld != 10*time.Minute || pc.LatestBatch != 50 {
		t.Fatalf("the minimums: %+v %v", pc, err)
	}
	for _, v := range []string{"off", "OFF", "0", "false", " no "} {
		if pc, err := load(map[string]string{"SCOUT_LATEST_REFRESH": v}); err != nil || pc.LatestOn {
			t.Errorf("SCOUT_LATEST_REFRESH=%q: on=%v %v", v, pc.LatestOn, err)
		}
	}
	for _, bad := range []map[string]string{
		{"SCOUT_LATEST_REFRESH": "maybe"},
		{"SCOUT_LATEST_REFRESH_RECENT": "59s"},
		{"SCOUT_LATEST_REFRESH_RECENT": "soon"},
		{"SCOUT_LATEST_REFRESH_RECENT": "15"},
		{"SCOUT_LATEST_REFRESH_OLD": "9m59s"},
		{"SCOUT_LATEST_REFRESH_OLD": "daily"},
		{"SCOUT_LATEST_REFRESH_OLD": "-24h"},
		{"SCOUT_LATEST_BATCH": "0"},
		{"SCOUT_LATEST_BATCH": "-5"},
		{"SCOUT_LATEST_BATCH": "many"},
		{"SCOUT_LATEST_BATCH": "10001"},
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("%v: no error", bad)
		} else {
			for k := range bad {
				if !strings.Contains(err.Error(), k) {
					t.Errorf("%v: the error does not name the setting: %v", bad, err)
				}
			}
		}
	}
}

// Ctrl+C in the middle of a pass: the row it was working on stays as it was,
// and it is not counted as a failure.
func TestLatestPriceInterruptedLeavesRowUntouched(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 20*ltDay)
	// The node, with a switch: when armed, the first eth_getLogs cancels the run.
	var armed atomic.Bool
	var cancel context.CancelFunc
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if armed.Load() && bytes.Contains(body, []byte("eth_getLogs")) {
			cancel()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.serve(w, r)
	}))
	t.Cleanup(node.Close)
	token, pool := "0x4444444444444444444444444444444444444444", "0x00000000000000000000000000000000000000c3"
	f.ltPool(token, pool, tWETH)
	entry := time.Now().Add(-2 * ltDay)
	eb := f.blockAtTime(entry)
	at := func(d time.Duration) uint64 { return uint64(int64(eb) + int64(d/time.Second)*10) }
	f.ltTrade(token, pool, at(-2*time.Minute), 1e-6)
	f.ltTrade(token, pool, at(30*time.Second), 2e-6)
	f.ltTrade(token, pool, at(30*time.Hour), 4e-6)
	s := ltScanner(t, st, node.URL)
	s.onChannelPost(postAt(1, entry, token))
	id := *(<-s.queue).CallID
	if n := s.trackDue(ctx, 50); n != 1 {
		t.Fatalf("processed %d", n)
	}
	whole := func() string {
		var row string
		if err := st.Pool.QueryRow(ctx, `SELECT to_jsonb(t)::text FROM scout_call_tracking t WHERE call_id = $1`, id).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := whole()

	var cctx context.Context
	cctx, cancel = context.WithCancel(ctx)
	defer cancel()
	armed.Store(true)
	f.took("")
	res := s.refreshLatest(cctx, false)
	armed.Store(false)
	if n := f.took("eth_getLogs"); n == 0 || cctx.Err() == nil {
		t.Fatalf("the pass was not interrupted in its scan (%d eth_getLogs)", n)
	}
	if res.Due != 1 || res.Refreshed != 0 || res.Failed != 0 || res.FirstErr != "" {
		t.Fatalf("interrupted pass: %+v", res)
	}
	if after := whole(); after != before {
		t.Fatalf("the interrupted row changed:\n got %s\nwant %s", after, before)
	}
	if skip := s.latestSkip(time.Now()); len(skip) != 0 {
		t.Fatalf("the interrupted row is set aside as failed: %v", skip)
	}
	// a pass started after the stop does nothing at all
	if res := s.refreshLatest(cctx, false); res.Due != 0 || f.took("eth_blockNumber") != 0 {
		t.Fatalf("pass on a stopped run: %+v", res)
	}
	// one row, called directly on a stopped run: an error, nothing written
	rows, err := st.DueLatest(ctx, time.Now(), time.Minute, time.Hour, latestRecentAge, false, nil, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due: %d %v", len(rows), err)
	}
	quotes := &latestQuotes{o: s.onchain, hour: time.Now().Unix() / 3600 * 3600, m: map[string]*latestQuote{}}
	if _, err := s.latestOne(cctx, &rows[0], f.head(), time.Now(), quotes); err == nil || whole() != before {
		t.Fatalf("one row on a stopped run: err %v", err)
	}
	// the next run picks it up
	if res := s.refreshLatest(ctx, false); res.Refreshed != 1 || res.Changed != 1 {
		t.Fatalf("pass after the restart: %+v", res)
	}
	if c := ltRead(t, st, id); !ltNear(c.Price, 4e-6*ltETH) || !ltNear(c.Ret, 100) {
		t.Fatalf("latest after the restart: %v %v", fnum(c.Price), fnum(c.Ret))
	}
}
