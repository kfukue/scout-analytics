package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Quote asset → USD through the asset's own pool (no Chainlink feed needed)
// ---------------------------------------------------------------------------

// quoteViaPool prices a quote asset (e.g. VIRTUAL or a stock token) from the
// pool it trades in against WETH or a stablecoin: the pool's last trade at or
// before the block × the USD price of that pool's other side.
func (o *onchainSource) quoteViaPool(ctx context.Context, quote string, block uint64) (float64, bool, error) {
	quote = strings.ToLower(quote)
	if quote == zeroAddr {
		return 0, false, nil
	}
	o.mu.Lock()
	qp, known := o.quotePools[quote]
	o.mu.Unlock()
	if !known {
		at := block
		if at == 0 {
			var err error
			if at, err = o.rpc.blockNumber(ctx); err != nil {
				return 0, false, err
			}
		}
		direct := func(q string) bool {
			q = strings.ToLower(q)
			return q != quote && o.quoteSourceDirect(q) != "none"
		}
		found, err := o.discoverWith(ctx, quote, at, direct)
		if err != nil && !errors.Is(err, errNoPool) {
			if nonRPC(err) != nil || ctx.Err() != nil {
				return 0, false, err // transport problem: try again later
			}
			err = errNoPool // not a token contract we can read
		}
		if err == nil {
			qm, err := o.rpc.tokenInfo(ctx, found.Quote)
			if err != nil {
				return 0, false, err
			}
			found.QuoteDec, found.QuoteSym = qm.Decimals, qm.Symbol
			qp = found
		}
		o.mu.Lock()
		o.quotePools[quote] = qp // nil = this asset has no usable pool; don't look again
		o.mu.Unlock()
	}
	if qp == nil {
		return 0, false, nil
	}
	at := block
	if at == 0 {
		var err error
		if at, err = o.rpc.blockNumber(ctx); err != nil {
			return 0, false, err
		}
	}
	addr, topics := qp.logFilter()
	l, err := o.lastLogBefore(ctx, addr, topics, at)
	if err != nil {
		return 0, false, err
	}
	if l == nil {
		return 0, false, fmt.Errorf("no trades in the %s pool of quote asset %s before block %d", qp.QuoteSym, quote, at)
	}
	p := qp.priceOfLog(*l)
	if p <= 0 {
		return 0, false, fmt.Errorf("bad price in the pool of quote asset %s", quote)
	}
	q, ok, err := o.quoteUSDDirect(ctx, qp.Quote, block)
	if err != nil || !ok {
		return 0, false, err
	}
	return p * q, true, nil
}

// quoteSourceDirect is quoteSource without the pool fallback ("none" = no feed,
// not a stablecoin, not ETH).
func (o *onchainSource) quoteSourceDirect(quote string) string {
	quote = strings.ToLower(quote)
	isETH := quote == zeroAddr || quote == o.cfg.WETH
	switch {
	case o.cfg.Stables[quote]:
		return "stablecoin = $1"
	case o.mainnet != nil && (o.cfg.MainnetFeeds[quote] != "" || (isETH && o.cfg.MainnetFeeds["eth"] != "")):
		return "Chainlink on Ethereum mainnet"
	case o.cfg.Feeds[quote] != "" || (isETH && o.cfg.Feeds["eth"] != ""):
		return "Chainlink on Robinhood Chain"
	case isETH && o.cfg.EthUSDPool != "":
		return "WETH/USDG pool"
	}
	return "none"
}

// ---------------------------------------------------------------------------
// Block → time (events carry a block number, not a timestamp)
// ---------------------------------------------------------------------------

// hourBlock: the block at a UTC hour boundary. Cached, so all calls share the
// same ~24 lookups per day of history.
func (o *onchainSource) hourBlock(ctx context.Context, hour int64) (uint64, error) {
	o.mu.Lock()
	b, ok := o.hourBlocks[hour]
	o.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := o.rpc.blockAt(ctx, hour)
	if err != nil {
		return 0, err
	}
	o.mu.Lock()
	o.hourBlocks[hour] = b
	o.mu.Unlock()
	return b, nil
}

// timeOfBlock estimates a block's timestamp: it finds the UTC hour whose
// boundary blocks bracket it (starting from the guess est) and interpolates
// inside that hour. Accurate to seconds without one RPC call per event.
func (o *onchainSource) timeOfBlock(ctx context.Context, block uint64, est int64) (int64, error) {
	h := est - est%3600
	now := time.Now().Unix()
	for i := 0; i < 24*40; i++ {
		lo, err := o.hourBlock(ctx, h)
		if err != nil {
			return 0, err
		}
		if block < lo && h > 0 {
			h -= 3600
			continue
		}
		if h+3600 > now { // the current, unfinished hour
			latest, err := o.rpc.blockNumber(ctx)
			if err != nil {
				return 0, err
			}
			if latest <= lo || block >= latest {
				return now, nil
			}
			return h + int64(float64(block-lo)/float64(latest-lo)*float64(now-h)), nil
		}
		hi, err := o.hourBlock(ctx, h+3600)
		if err != nil {
			return 0, err
		}
		if block >= hi {
			h += 3600
			continue
		}
		if hi <= lo {
			return h, nil
		}
		return h + int64(float64(block-lo)/float64(hi-lo)*3600), nil
	}
	return 0, fmt.Errorf("could not place block %d in time (near %s)", block, time.Unix(est, 0).UTC().Format(time.RFC3339))
}

// quoteUSDHour: USD price of the quote asset at a UTC hour boundary (cached).
func (o *onchainSource) quoteUSDHour(ctx context.Context, quote string, hour int64) (float64, error) {
	key := strings.ToLower(quote) + "|" + fmt.Sprint(hour)
	o.mu.Lock()
	q, ok := o.hourQuotes[key]
	o.mu.Unlock()
	if ok {
		return q, nil
	}
	b, err := o.hourBlock(ctx, hour)
	if err != nil {
		return 0, err
	}
	q, found, err := o.quoteUSD(ctx, quote, b)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, errors.New("no USD source")
	}
	o.mu.Lock()
	o.hourQuotes[key] = q
	o.mu.Unlock()
	return q, nil
}

// ---------------------------------------------------------------------------
// Candles
// ---------------------------------------------------------------------------

// candleRow is one OHLC bucket of a call's price path (scout_call_candles).
type candleRow struct {
	IntervalS  int
	Start      time.Time
	O, H, L, C float64
	Events     int
}

const (
	candleFineS    = 300   // 5-minute candles …
	candleFineSpan = 86400 // … for the first 24 hours after the call
	candleCoarseS  = 3600  // hourly candles for the whole tracked window
)

// candleBuf collects candles while a block range is scanned.
type candleBuf struct {
	entry int64 // call time
	rows  map[[2]int64]*candleRow
	order [][2]int64
	ext   map[int64]*hourExtremes // per UTC hour, in quote units
}

// hourExtremes: highest and lowest price in one hour, over all events and over
// those after the realistic entry (0 = none).
type hourExtremes struct{ max, min, maxLate, minLate float64 }

func newCandleBuf(entry int64) *candleBuf {
	return &candleBuf{entry: entry, rows: map[[2]int64]*candleRow{}, ext: map[int64]*hourExtremes{}}
}

func (b *candleBuf) add(ts int64, p float64, late bool) {
	e := b.ext[ts-ts%3600]
	if e == nil {
		e = &hourExtremes{}
		b.ext[ts-ts%3600] = e
	}
	e.max = math.Max(e.max, p)
	if e.min == 0 || p < e.min {
		e.min = p
	}
	if late {
		e.maxLate = math.Max(e.maxLate, p)
		if e.minLate == 0 || p < e.minLate {
			e.minLate = p
		}
	}
	put := func(interval int64) {
		k := [2]int64{interval, ts - ts%interval}
		r := b.rows[k]
		if r == nil {
			r = &candleRow{IntervalS: int(interval), Start: time.Unix(k[1], 0).UTC(), O: p, H: p, L: p}
			b.rows[k] = r
			b.order = append(b.order, k)
		}
		if p > r.H {
			r.H = p
		}
		if p < r.L {
			r.L = p
		}
		r.C = p
		r.Events++
	}
	put(candleCoarseS)
	if ts < b.entry+candleFineSpan {
		put(candleFineS)
	}
}

// foldExtremes widens the state's running extremes (in the price unit) with
// this scan's events, each hour converted at that hour's rate.
func (b *candleBuf) foldExtremes(st *onchainState, scale func(hour int64) (float64, error)) error {
	lower := func(cur *float64, v float64) {
		if v > 0 && (*cur == 0 || v < *cur) {
			*cur = v
		}
	}
	for hour, e := range b.ext {
		q, err := scale(hour)
		if err != nil {
			return err
		}
		st.RunMaxU = math.Max(st.RunMaxU, e.max*q)
		st.RunMaxLateU = math.Max(st.RunMaxLateU, e.maxLate*q)
		lower(&st.RunMinU, e.min*q)
		lower(&st.RunMinLateU, e.minLate*q)
	}
	return nil
}

// list returns the candles in insertion order, prices multiplied by scale(hour).
func (b *candleBuf) list(scale func(hour int64) (float64, error)) ([]candleRow, error) {
	out := make([]candleRow, 0, len(b.order))
	for _, k := range b.order {
		r := *b.rows[k]
		ts := r.Start.Unix()
		q, err := scale(ts - ts%3600)
		if err != nil {
			return nil, err
		}
		r.O, r.H, r.L, r.C = r.O*q, r.H*q, r.L*q, r.C*q
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pre-call trading (features known at the moment of the post)
// ---------------------------------------------------------------------------

// precallWindows are the look-back windows, in seconds: 5, 15 and 60 minutes.
var precallWindows = [3]int64{300, 900, 3600}

// precallStats describes trading in the pool just before the call.
type precallStats struct {
	WindowS        int
	Swaps          [3]int
	Buys           [3]int
	Sells          [3]int
	BuyVol         [3]float64  // in quote units until scaled
	SellVol        [3]float64  //
	PriceChgPct    [3]*float64 // price at the call vs the price one window earlier
	FirstTradeAgeS *int        // seconds from the first trade in the 60-minute window to the call
	VolUnit        string      // usd, or the quote asset's symbol
}

// tradeOfLog: direction and size (quote units) of the trade behind a price
// event. prev carries the last v2 reserves between calls.
func (st *onchainState) tradeOfLog(l rpcLog, prev *[2]*big.Int) (buy bool, vol float64, ok bool) {
	data := unhex(l.Data)
	var tok, quo *big.Int
	switch st.logKind(l) {
	case "curve": // CurveBuy / CurveSell: direction from the event, volume before fee and tax
		return curveTrade(l, st.QuoteDec)
	case "v2": // Sync(reserve0, reserve1): compare with the previous reserves
		r0, r1 := word(data, 0), word(data, 1)
		p0, p1 := prev[0], prev[1]
		prev[0], prev[1] = r0, r1
		if p0 == nil {
			return false, 0, false
		}
		d0, d1 := new(big.Int).Sub(r0, p0), new(big.Int).Sub(r1, p1)
		tok, quo = d0, d1 // pool deltas: the pool loses the token on a buy
		if !st.TokenIs0 {
			tok, quo = d1, d0
		}
		if tok.Sign() == 0 || quo.Sign() == 0 || tok.Sign() == quo.Sign() {
			return false, 0, false // liquidity added or removed, not a swap
		}
		buy = tok.Sign() < 0
	case "v4": // amounts are the swapper's deltas: positive = received
		a0, a1 := signedWord(data, 0), signedWord(data, 1)
		tok, quo = a0, a1
		if !st.TokenIs0 {
			tok, quo = a1, a0
		}
		if tok.Sign() == 0 {
			return false, 0, false
		}
		buy = tok.Sign() > 0
	default: // v3: amounts are the pool's deltas: negative = paid out
		a0, a1 := signedWord(data, 0), signedWord(data, 1)
		tok, quo = a0, a1
		if !st.TokenIs0 {
			tok, quo = a1, a0
		}
		if tok.Sign() == 0 {
			return false, 0, false
		}
		buy = tok.Sign() < 0
	}
	return buy, math.Abs(bigToFloat(quo)) / pow10(st.QuoteDec), true
}

// precall reads the pool's trades in the hour before the call.
func (o *onchainSource) precall(ctx context.Context, st *onchainState, entryTS int64) (*precallStats, error) {
	span := precallWindows[2]
	from, err := o.rpc.blockAt(ctx, entryTS-span)
	if err != nil {
		return nil, err
	}
	if from >= st.EntryBlock {
		from = st.EntryBlock
	}
	type ev struct {
		ts   int64
		p    float64
		buy  bool
		vol  float64
		trde bool
	}
	var evs []ev
	var prev [2]*big.Int
	width := float64(st.EntryBlock - from)
	err = o.scanLogs(ctx, st, from, st.EntryBlock, func(l rpcLog) {
		p := st.priceOfLog(l)
		if p <= 0 {
			return
		}
		ts := entryTS
		if width > 0 {
			ts = entryTS - span + int64(float64(l.block()-from)/width*float64(span))
		}
		buy, vol, ok := st.tradeOfLog(l, &prev)
		evs = append(evs, ev{ts: ts, p: p, buy: buy, vol: vol, trde: ok})
	})
	if err != nil {
		return nil, err
	}
	ps := &precallStats{WindowS: int(span), VolUnit: st.QuoteSym}
	if len(evs) == 0 {
		return ps, nil
	}
	age := int(entryTS - evs[0].ts)
	ps.FirstTradeAgeS = &age
	last := evs[len(evs)-1].p
	for i, w := range precallWindows {
		cut := entryTS - w
		ref := 0.0
		for _, e := range evs {
			if e.ts <= cut {
				ref = e.p // last price at or before the window start
				continue
			}
			if ref == 0 {
				ref = e.p // the pool is younger than the window: its first price
			}
			ps.Swaps[i]++
			if !e.trde {
				continue
			}
			if e.buy {
				ps.Buys[i]++
				ps.BuyVol[i] += e.vol
			} else {
				ps.Sells[i]++
				ps.SellVol[i] += e.vol
			}
		}
		if ref > 0 {
			chg := (last/ref - 1) * 100
			ps.PriceChgPct[i] = &chg
		}
	}
	return ps, nil
}

// scale converts the volumes to USD.
func (ps *precallStats) scale(q float64) {
	for i := range ps.BuyVol {
		ps.BuyVol[i] *= q
		ps.SellVol[i] *= q
	}
	ps.VolUnit = "usd"
}
