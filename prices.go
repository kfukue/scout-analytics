package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Price source: GeckoTerminal (indexes Robinhood Chain; DexScreener mostly doesn't).
//
//	GET /networks/{network}/tokens/{token}/pools                 → pools for a token
//	GET /networks/{network}/pools/{pool}/ohlcv/{minute|hour|day}  → candles
//	    ?aggregate=1&before_timestamp=<unix>&limit=<=1000&currency=usd&token=<token>
//	    ohlcv_list: [[unix_ts, open, high, low, close, volume], …] newest first
//
// The keyless public API allows ~10 calls/min; set SCOUT_PRICE_RPM (and
// SCOUT_PRICE_API_KEY / SCOUT_PRICE_API_BASE for a CoinGecko plan) to go faster.
// ---------------------------------------------------------------------------

type priceConfig struct {
	Source    string // "onchain" (Uniswap pools + Chainlink, default) | "gecko" (GeckoTerminal API)
	Onchain   onchainConfig
	BaseURL   string
	Network   string
	APIKey    string
	KeyHeader string
	RPM       int
	Horizons  []horizon
	RugLiqUSD float64 // = Onchain.RugLiqUSD (SCOUT_RUG_LIQ_USD); the gecko source compares half its pool reserve (the quote side) with it
	Enabled   bool
	Interval  time.Duration // tracker loop interval
	Workers   int           // calls tracked at the same time (on-chain source)

	// Latest-price pass (on-chain source; tracker_latest.go)
	LatestOn     bool          // SCOUT_LATEST_REFRESH (off = no pass)
	LatestRecent time.Duration // SCOUT_LATEST_REFRESH_RECENT: calls younger than 30 days
	LatestOld    time.Duration // SCOUT_LATEST_REFRESH_OLD: calls 30 days or older
	LatestBatch  int           // SCOUT_LATEST_BATCH: rows per cycle at most
}

type horizon struct {
	Name string // "1h", "1d", …
	Dur  time.Duration
}

// parseHorizon accepts Go durations plus "d" (days) and "w" (weeks).
func parseHorizon(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	for suffix, mult := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if strings.HasSuffix(s, suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, suffix), 64)
			if err != nil {
				return 0, err
			}
			return time.Duration(n * float64(mult)), nil
		}
	}
	return time.ParseDuration(s)
}

func loadPriceConfig() (priceConfig, error) {
	pc := priceConfig{
		Source:    strings.ToLower(env("SCOUT_PRICE_SOURCE", "onchain")),
		BaseURL:   strings.TrimRight(env("SCOUT_PRICE_API_BASE", "https://api.geckoterminal.com/api/v2"), "/"),
		Network:   env("SCOUT_PRICE_NETWORK", "robinhood"),
		APIKey:    env("SCOUT_PRICE_API_KEY", ""),
		KeyHeader: env("SCOUT_PRICE_API_KEY_HEADER", "x-cg-pro-api-key"),
		RPM:       10,
		RugLiqUSD: defaultRugLiqUSD,
		Enabled:   envBool("SCOUT_TRACK_PERFORMANCE", true),
		Interval:  envDur("SCOUT_TRACK_INTERVAL", time.Minute),
		Workers:   8,
	}
	if v := env("SCOUT_PRICE_RPM", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return pc, fmt.Errorf("SCOUT_PRICE_RPM=%q: want a number >= 1", v)
		}
		pc.RPM = n
	}
	if v := env("SCOUT_TRACK_WORKERS", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return pc, fmt.Errorf("SCOUT_TRACK_WORKERS=%q: want a number from 1 to 64", v)
		}
		pc.Workers = n
	}
	if err := pc.loadLatestConfig(); err != nil {
		return pc, err
	}
	for _, h := range strings.Split(env("SCOUT_PERF_HORIZONS", "1h,1d,3d,7d,30d"), ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		d, err := parseHorizon(h)
		if err != nil || d <= 0 {
			return pc, fmt.Errorf("SCOUT_PERF_HORIZONS: bad horizon %q", h)
		}
		if d > 40*24*time.Hour {
			return pc, fmt.Errorf("SCOUT_PERF_HORIZONS: %q is longer than 40d (one hourly candle request)", h)
		}
		pc.Horizons = append(pc.Horizons, horizon{Name: h, Dur: d})
	}
	sort.Slice(pc.Horizons, func(i, j int) bool { return pc.Horizons[i].Dur < pc.Horizons[j].Dur })
	if pc.Source != "onchain" && pc.Source != "gecko" {
		return pc, fmt.Errorf("SCOUT_PRICE_SOURCE=%q: use onchain or gecko", pc.Source)
	}
	var err error
	if pc.Onchain, err = loadOnchainConfig(); err != nil {
		return pc, err
	}
	// SCOUT_RUG_LIQ_USD is read and checked once, by loadOnchainConfig.
	pc.RugLiqUSD = pc.Onchain.RugLiqUSD
	if len(pc.Horizons) == 0 {
		return pc, errors.New("SCOUT_PERF_HORIZONS is empty")
	}
	return pc, nil
}

func (pc priceConfig) maxHorizon() time.Duration { return pc.Horizons[len(pc.Horizons)-1].Dur }

// ---------------------------------------------------------------------------
// HTTP client with a simple rate limiter and 429 back-off.
// ---------------------------------------------------------------------------

type geckoClient struct {
	cfg  priceConfig
	http *http.Client

	mu   sync.Mutex
	next time.Time // earliest time the next request may start
}

func newGeckoClient(cfg priceConfig) *geckoClient {
	return &geckoClient{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

var errNotFound = errors.New("not found")

// geckoRetryBase is the back-off unit after HTTP 429/5xx (shortened in tests).
var geckoRetryBase = 30 * time.Second

func (g *geckoClient) wait(ctx context.Context) error {
	g.mu.Lock()
	gap := time.Minute / time.Duration(g.cfg.RPM)
	now := time.Now()
	start := g.next
	if start.Before(now) {
		start = now
	}
	g.next = start.Add(gap)
	g.mu.Unlock()
	return sleepCtx(ctx, time.Until(start))
}

func (g *geckoClient) get(ctx context.Context, path string, q url.Values, out any) error {
	u := g.cfg.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		if err := g.wait(ctx); err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "scoutanalytics/1.0")
		if g.cfg.APIKey != "" {
			req.Header.Set(g.cfg.KeyHeader, g.cfg.APIKey)
		}
		resp, err := g.http.Do(req)
		if err != nil {
			if attempt < 2 {
				continue
			}
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return json.Unmarshal(body, out)
		case resp.StatusCode == http.StatusNotFound:
			return errNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if attempt >= 3 {
				return fmt.Errorf("GET %s: %s", path, resp.Status)
			}
			back := time.Duration(attempt+1) * geckoRetryBase
			log.Printf("prices: %s from GeckoTerminal — backing off %s", resp.Status, back)
			if err := sleepCtx(ctx, back); err != nil {
				return err
			}
		default:
			return fmt.Errorf("GET %s: %s: %.200s", path, resp.Status, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Pools
// ---------------------------------------------------------------------------

type poolInfo struct {
	Address    string
	Name       string
	Dex        string
	CreatedAt  *time.Time
	ReserveUSD float64
	PriceUSD   float64 // current price of OUR token in this pool
}

type geckoPoolsResp struct {
	Data []struct {
		Attributes struct {
			Address            string  `json:"address"`
			Name               string  `json:"name"`
			PoolCreatedAt      *string `json:"pool_created_at"`
			ReserveInUSD       *string `json:"reserve_in_usd"`
			BaseTokenPriceUSD  *string `json:"base_token_price_usd"`
			QuoteTokenPriceUSD *string `json:"quote_token_price_usd"`
		} `json:"attributes"`
		Relationships struct {
			BaseToken struct {
				Data struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"base_token"`
			Dex struct {
				Data struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"dex"`
		} `json:"relationships"`
	} `json:"data"`
}

func atof(p *string) float64 {
	if p == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(*p, 64)
	return f
}

// tokenPools lists the pools trading the token.
func (g *geckoClient) tokenPools(ctx context.Context, token string) ([]poolInfo, error) {
	var r geckoPoolsResp
	err := g.get(ctx, fmt.Sprintf("/networks/%s/tokens/%s/pools", g.cfg.Network, strings.ToLower(token)), url.Values{"page": {"1"}}, &r)
	if err != nil {
		return nil, err
	}
	var out []poolInfo
	for _, d := range r.Data {
		a := d.Attributes
		p := poolInfo{Address: a.Address, Name: a.Name, Dex: d.Relationships.Dex.Data.ID, ReserveUSD: atof(a.ReserveInUSD)}
		if a.PoolCreatedAt != nil {
			if t, err := time.Parse(time.RFC3339, *a.PoolCreatedAt); err == nil {
				p.CreatedAt = &t
			}
		}
		// base token id is "<network>_<address>"
		if strings.HasSuffix(strings.ToLower(d.Relationships.BaseToken.Data.ID), strings.ToLower(token)) {
			p.PriceUSD = atof(a.BaseTokenPriceUSD)
		} else {
			p.PriceUSD = atof(a.QuoteTokenPriceUSD)
		}
		out = append(out, p)
	}
	return out, nil
}

// pickPool chooses the pool that best represents the token at call time: the
// most liquid pool that already existed at the call, else the most liquid one.
func pickPool(pools []poolInfo, callAt time.Time) *poolInfo {
	var best, bestBefore *poolInfo
	for i := range pools {
		p := &pools[i]
		if best == nil || p.ReserveUSD > best.ReserveUSD {
			best = p
		}
		if p.CreatedAt != nil && !p.CreatedAt.After(callAt.Add(5*time.Minute)) {
			if bestBefore == nil || p.ReserveUSD > bestBefore.ReserveUSD {
				bestBefore = p
			}
		}
	}
	if bestBefore != nil {
		return bestBefore
	}
	return best
}

// ---------------------------------------------------------------------------
// Candles
// ---------------------------------------------------------------------------

type candle struct {
	T               int64 // period start, unix seconds
	O, H, L, C, Vol float64
}

type geckoOHLCVResp struct {
	Data struct {
		Attributes struct {
			OHLCVList [][]float64 `json:"ohlcv_list"`
		} `json:"attributes"`
	} `json:"data"`
}

// ohlcv returns candles (ascending) with period start < before.
func (g *geckoClient) ohlcv(ctx context.Context, pool, token, timeframe string, aggregate int, before time.Time, limit int) ([]candle, error) {
	if limit > 1000 {
		limit = 1000
	}
	if limit < 1 {
		limit = 1
	}
	var r geckoOHLCVResp
	q := url.Values{
		"aggregate":        {strconv.Itoa(aggregate)},
		"before_timestamp": {strconv.FormatInt(before.Unix(), 10)},
		"limit":            {strconv.Itoa(limit)},
		"currency":         {"usd"},
		"token":            {strings.ToLower(token)},
	}
	if err := g.get(ctx, fmt.Sprintf("/networks/%s/pools/%s/ohlcv/%s", g.cfg.Network, strings.ToLower(pool), timeframe), q, &r); err != nil {
		return nil, err
	}
	return toCandles(r.Data.Attributes.OHLCVList), nil
}

func toCandles(list [][]float64) []candle {
	out := make([]candle, 0, len(list))
	for _, x := range list {
		if len(x) < 5 {
			continue
		}
		c := candle{T: int64(x[0]), O: x[1], H: x[2], L: x[3], C: x[4]}
		if len(x) > 5 {
			c.Vol = x[5]
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// ---------------------------------------------------------------------------
// Performance maths (pure functions, unit-tested)
// ---------------------------------------------------------------------------

// entryFromCandles: the price when the call was posted. Uses the close of the
// candle the call falls in (or the latest before it, if fresh enough), else the
// open of the first candle after the call (token hadn't traded yet).
func entryFromCandles(cs []candle, period time.Duration, entry time.Time, maxStale time.Duration) (float64, bool) {
	e := entry.Unix()
	var before, after *candle
	for i := range cs {
		c := &cs[i]
		if c.T <= e {
			before = c
		} else if after == nil {
			after = c
		}
	}
	if before != nil && e-before.T <= int64((period+maxStale)/time.Second) && before.C > 0 {
		return before.C, true
	}
	if after != nil && after.T-e <= int64((period+maxStale)/time.Second) && after.O > 0 {
		return after.O, true
	}
	return 0, false
}

type horizonResult struct {
	Horizon     string
	DueAt       time.Time
	Status      string // done | pending | no_data
	PriceUSD    float64
	ReturnPct   float64
	MaxGainPct  float64
	MaxDDPct    float64
	MaxPriceUSD float64
	MinPriceUSD float64
	LastTradeAt *time.Time

	// Measured from the realistic entry (on-chain source only).
	ReturnLatePct  *float64
	MaxGainLatePct *float64
	MaxDDLatePct   *float64

	// Timing (on-chain source only; timing.go). TimingAt nil = not computed.
	PeakLateAfterS    *int
	First2xAfterS     *int
	Above2xS          *int
	FallBelow2xAfterS *int
	Above2xCensored   *bool
	TimingAt          *time.Time
}

// horizonStats computes return / peak gain / drawdown at entry+h from hourly
// candles. The first hour uses the whole entry-hour candle (slight bias).
func horizonStats(hourly []candle, entry time.Time, entryPrice float64, h horizon, now time.Time) horizonResult {
	end := entry.Add(h.Dur)
	r := horizonResult{Horizon: h.Name, DueAt: end, Status: "pending"}
	if now.Before(end) {
		return r
	}
	if entryPrice <= 0 {
		r.Status = "no_data"
		return r
	}
	startHour := entry.Unix() - entry.Unix()%3600
	var last *candle
	maxH, minL := 0.0, math.MaxFloat64
	for i := range hourly {
		c := &hourly[i]
		if c.T >= end.Unix() {
			break
		}
		last = c
		if c.T >= startHour {
			if c.H > maxH {
				maxH = c.H
			}
			if c.L > 0 && c.L < minL {
				minL = c.L
			}
		}
	}
	if last == nil {
		r.Status = "no_data"
		return r
	}
	r.Status = "done"
	r.PriceUSD = last.C
	if maxH == 0 { // no trades inside the window: flat at the last price
		maxH, minL = last.C, last.C
	}
	r.MaxPriceUSD, r.MinPriceUSD = maxH, minL
	r.ReturnPct = (last.C/entryPrice - 1) * 100
	r.MaxGainPct = (maxH/entryPrice - 1) * 100
	r.MaxDDPct = (minL/entryPrice - 1) * 100
	if last.T >= startHour {
		t := time.Unix(last.T, 0).UTC()
		r.LastTradeAt = &t
	}
	return r
}
