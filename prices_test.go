package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseHorizonAndConfig(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "1d": 24 * time.Hour, "3d": 72 * time.Hour, "2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseHorizon(in); err != nil || got != want {
			t.Errorf("%s → %s %v", in, got, err)
		}
	}
	t.Setenv("SCOUT_PERF_HORIZONS", "7d, 1h ,1d")
	pc, err := loadPriceConfig()
	if err != nil || len(pc.Horizons) != 3 || pc.Horizons[0].Name != "1h" || pc.maxHorizon() != 7*24*time.Hour || pc.RPM != 10 || pc.Network != "robinhood" {
		t.Fatalf("%+v %v", pc, err)
	}
	t.Setenv("SCOUT_PERF_HORIZONS", "60d")
	if _, err := loadPriceConfig(); err == nil {
		t.Fatal("horizons over 40d must be rejected")
	}
}

func TestEntryFromCandles(t *testing.T) {
	entry := time.Unix(1_000_000_020, 0) // inside the 1_000_000_000 minute
	cs := []candle{{T: 999_999_940, C: 0.9}, {T: 1_000_000_000, O: 0.95, C: 1.0}, {T: 1_000_000_060, O: 1.1, C: 1.2}}
	if p, ok := entryFromCandles(cs, time.Minute, entry, 15*time.Minute); !ok || p != 1.0 {
		t.Fatalf("containing candle: %v %v", p, ok)
	}
	// token had not traded yet: first candle after → its open
	if p, ok := entryFromCandles(cs[2:], time.Minute, entry, 15*time.Minute); !ok || p != 1.1 {
		t.Fatalf("after: %v %v", p, ok)
	}
	// last trade long before the call: too stale
	if _, ok := entryFromCandles([]candle{{T: 999_000_000, C: 1}}, time.Minute, entry, 15*time.Minute); ok {
		t.Fatal("stale candle used")
	}
}

func TestHorizonStats(t *testing.T) {
	entry := time.Date(2026, 9, 1, 12, 20, 0, 0, time.UTC)
	h0 := entry.Truncate(time.Hour).Unix()
	hourly := []candle{
		{T: h0 - 3600, H: 9, L: 0.1, C: 0.5},            // before the entry hour: ignored for max/min
		{T: h0, O: 1, H: 2.2, L: 0.9, C: 2},             // entry hour
		{T: h0 + 2*3600, O: 2, H: 5, L: 1.5, C: 3},      // spike
		{T: h0 + 30*3600, O: 3, H: 3.2, L: 0.5, C: 0.8}, // day 2 dump
	}
	now := entry.Add(4 * 24 * time.Hour)
	r1h := horizonStats(hourly, entry, 1.0, horizon{"1h", time.Hour}, now)
	if r1h.Status != "done" || r1h.PriceUSD != 2 || !near(r1h.ReturnPct, 100) || !near(r1h.MaxGainPct, 120) || !near(r1h.MaxDDPct, -10) {
		t.Fatalf("1h: %+v", r1h)
	}
	r1d := horizonStats(hourly, entry, 1.0, horizon{"1d", 24 * time.Hour}, now)
	if r1d.PriceUSD != 3 || !near(r1d.ReturnPct, 200) || !near(r1d.MaxGainPct, 400) || !near(r1d.MaxDDPct, -10) {
		t.Fatalf("1d: %+v", r1d)
	}
	r3d := horizonStats(hourly, entry, 1.0, horizon{"3d", 72 * time.Hour}, now)
	if r3d.PriceUSD != 0.8 || !near(r3d.ReturnPct, -20) || !near(r3d.MaxGainPct, 400) || !near(r3d.MaxDDPct, -50) || r3d.LastTradeAt == nil {
		t.Fatalf("3d: %+v", r3d)
	}
	if r := horizonStats(hourly, entry, 1.0, horizon{"7d", 7 * 24 * time.Hour}, now); r.Status != "pending" {
		t.Fatalf("7d should be pending: %+v", r)
	}
	if r := horizonStats(nil, entry, 1.0, horizon{"1d", 24 * time.Hour}, now); r.Status != "no_data" {
		t.Fatalf("no candles: %+v", r)
	}
}

func TestPickPool(t *testing.T) {
	call := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	early, late := call.Add(-time.Hour), call.Add(48*time.Hour)
	pools := []poolInfo{
		{Address: "0xlaunch", ReserveUSD: 5000, CreatedAt: &early},
		{Address: "0xmigrated", ReserveUSD: 90000, CreatedAt: &late}, // created after the call
		{Address: "0xsmall", ReserveUSD: 100, CreatedAt: &early},
	}
	if p := pickPool(pools, call); p.Address != "0xlaunch" {
		t.Fatalf("picked %s", p.Address)
	}
	if p := pickPool(pools[1:2], call); p.Address != "0xmigrated" {
		t.Fatalf("fallback picked %s", p.Address)
	}
}

// ---------------------------------------------------------------------------
// Fake GeckoTerminal
// ---------------------------------------------------------------------------

type fakeGecko struct {
	mu       sync.Mutex
	pools    map[string][]map[string]any // token(lower) → pool objects
	candles  map[string][]candle         // "pool|minute" / "pool|hour" → ascending candles
	requests []string
	fail429  int // respond 429 this many times first
}

func (f *fakeGecko) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
		if f.fail429 > 0 {
			f.fail429--
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("missing Accept header")
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// networks/<net>/tokens/<token>/pools
		if len(parts) == 5 && parts[2] == "tokens" && parts[4] == "pools" {
			pools, ok := f.pools[parts[3]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": pools})
			return
		}
		// networks/<net>/pools/<pool>/ohlcv/<tf>
		if len(parts) == 6 && parts[2] == "pools" && parts[4] == "ohlcv" {
			q := r.URL.Query()
			before, _ := strconv.ParseInt(q.Get("before_timestamp"), 10, 64)
			limit, _ := strconv.Atoi(q.Get("limit"))
			cs := f.candles[parts[3]+"|"+parts[5]]
			var list [][]float64
			for i := len(cs) - 1; i >= 0 && len(list) < limit; i-- { // newest first
				if cs[i].T < before {
					c := cs[i]
					list = append(list, []float64{float64(c.T), c.O, c.H, c.L, c.C, c.Vol})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"attributes": map[string]any{"ohlcv_list": list}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

func gpool(addr, token string, reserve, price float64, created time.Time, dex string) map[string]any {
	return map[string]any{
		"id": "robinhood_" + addr, "type": "pool",
		"attributes": map[string]any{
			"address": addr, "name": "TKN / WETH", "pool_created_at": created.UTC().Format(time.RFC3339),
			"reserve_in_usd": fmt.Sprint(reserve), "base_token_price_usd": fmt.Sprint(price), "quote_token_price_usd": "3000",
		},
		"relationships": map[string]any{
			"base_token": map[string]any{"data": map[string]any{"id": "robinhood_" + strings.ToLower(token)}},
			"dex":        map[string]any{"data": map[string]any{"id": dex}},
		},
	}
}

func TestGeckoClient(t *testing.T) {
	geckoRetryBase = 10 * time.Millisecond
	f := &fakeGecko{pools: map[string][]map[string]any{}, candles: map[string][]candle{}, fail429: 1}
	token := "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	created := time.Now().Add(-10 * 24 * time.Hour)
	f.pools[strings.ToLower(token)] = []map[string]any{gpool("0xpool1", token, 12345.5, 0.002, created, "longxyz")}
	f.candles["0xpool1|hour"] = []candle{{T: 100, C: 1}, {T: 3700, C: 2}, {T: 7300, C: 3}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	g := newGeckoClient(priceConfig{BaseURL: srv.URL, Network: "robinhood", RPM: 6000})

	pools, err := g.tokenPools(context.Background(), token)
	if err != nil || len(pools) != 1 || pools[0].Address != "0xpool1" || pools[0].ReserveUSD != 12345.5 ||
		pools[0].PriceUSD != 0.002 || pools[0].Dex != "longxyz" || pools[0].CreatedAt == nil {
		t.Fatalf("pools: %+v %v", pools, err)
	}
	if _, err := g.tokenPools(context.Background(), "0xunknown"); err != errNotFound {
		t.Fatalf("unknown token: %v", err)
	}
	cs, err := g.ohlcv(context.Background(), "0xpool1", token, "hour", 1, time.Unix(7300, 0), 10)
	if err != nil || len(cs) != 2 || cs[0].T != 100 || cs[1].C != 2 {
		t.Fatalf("ohlcv: %+v %v", cs, err)
	}
	last := f.requests[len(f.requests)-1]
	for _, want := range []string{"/networks/robinhood/pools/0xpool1/ohlcv/hour", "currency=usd", "token=" + strings.ToLower(token), "before_timestamp=7300", "aggregate=1"} {
		if !strings.Contains(last, want) {
			t.Errorf("request %q missing %q", last, want)
		}
	}
	if !strings.Contains(f.requests[0], "/tokens/"+strings.ToLower(token)+"/pools") || len(f.requests) < 2 {
		t.Errorf("429 retry not exercised: %v", f.requests)
	}
}

func TestRateLimiterSpacing(t *testing.T) {
	g := newGeckoClient(priceConfig{RPM: 600}) // 100ms apart
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := g.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 280*time.Millisecond {
		t.Fatalf("4 requests at 600/min took only %s", el)
	}
}

func sortedKeys(m map[string]horizonResult) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
