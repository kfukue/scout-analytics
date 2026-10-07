package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Asset → USD through the asset's own pools (no Chainlink feed needed)
// ---------------------------------------------------------------------------

var (
	// errNoUSDSource: every USD source of an asset is definitively absent (no
	// stablecoin, no feed, no pool against WETH / ETH / a stablecoin).
	errNoUSDSource = errors.New("no USD source")
	// errNoPriceYet: the asset has pools against WETH / a stablecoin, but none
	// of them traded within the look-back before the block. Temporary: tried
	// again later (other pools may turn up).
	errNoPriceYet = errors.New("no price yet")
)

const (
	usdNoPoolTTL    = time.Hour     // "no pool" is asked again after this
	usdNoPriceTTL   = 6 * time.Hour // "no price yet" at a block is asked again after this
	usdPoolsPerTier = 3             // own pools kept per kind (WETH / ETH, stablecoin)
)

// usdPoolSet: what is known about one asset's own pools. Changed under o.mu only.
type usdPoolSet struct {
	sym         string
	pools       []*onchainState      // every acceptable pool found (de-duplicated)
	anchors     []uint64             // the blocks the pool search ran around
	noPoolUntil time.Time            // the search found no pool: none until then
	noPrice     map[uint64]time.Time // block → "no price yet" until then
	source      string               // the pool used last (quoteSource, and the change log)
	sourceRef   string               // … with its address / id
}

// usdPoolTier: 1 = the pool's other side is WETH / native ETH (× ETH/USD),
// 2 = a stablecoin ($1), 0 = not a USD pool.
func (o *onchainSource) usdPoolTier(other string) int {
	other = strings.ToLower(other)
	switch {
	case o.isETH(other):
		return 1
	case o.cfg.Stables[other]:
		return 2
	}
	return 0
}

// quoteViaPools prices an asset (e.g. VIRTUAL or a stock token) from its own
// pools against WETH / native ETH (source 4) or a stablecoin (source 5): the
// last trade at or before the block × the USD price of the pool's other side.
// The pool whose last trade at or before the block is the most recent wins
// (within SCOUT_PRICE_LOOKBACK_BLOCKS; ties: a WETH / ETH pool first), so the
// pool can differ from block to block.
//
// Caching: the pools found are kept (a block none of them covers starts one
// more search around that block); "no pool" is kept for usdNoPoolTTL and "no
// price yet" at a block for usdNoPriceTTL; an error of the node is never kept.
func (o *onchainSource) quoteViaPools(ctx context.Context, asset string, block uint64) (float64, bool, error) {
	asset = strings.ToLower(asset)
	if block == 0 {
		b, err := o.rpc.blockNumber(ctx)
		if err != nil {
			return 0, false, err
		}
		block = b
	}
	now := o.clock()
	reach := o.cfg.DiscoveryBlocks * 64 // the widest window of a search
	o.mu.Lock()
	set := o.usdPools[asset]
	if set == nil {
		set = &usdPoolSet{}
		o.usdPools[asset] = set
	}
	noPool := now.Before(set.noPoolUntil)
	retryAt, noPrice := set.noPrice[block]
	pools := slices.Clone(set.pools)
	prefer := ""
	if set.sourceRef != "" {
		prefer = set.sourceRef[strings.LastIndex(set.sourceRef, " ")+1:]
	}
	anchored := false
	for _, a := range set.anchors {
		if a <= block+reach && block <= a+reach {
			anchored = true
		}
	}
	o.mu.Unlock()
	if noPool {
		return 0, false, nil
	}
	if noPrice && now.Before(retryAt) {
		return 0, false, fmt.Errorf("%s at block %d: %w (its pools have no trade within %d blocks before it; asked again after %s)",
			asset, block, errNoPriceYet, o.cfg.PriceLookback, retryAt.Local().Format("15:04"))
	}
	search := func() error {
		sym, found, err := o.discoverUSDPools(ctx, asset, block)
		if err != nil {
			return fmt.Errorf("own pools of %s: %w", asset, err)
		}
		pools = o.addUSDPools(asset, sym, block, found)
		anchored = true
		return nil
	}
	if len(pools) == 0 {
		if err := search(); err != nil {
			return 0, false, err
		}
		if len(pools) == 0 {
			o.mu.Lock()
			set.noPoolUntil = now.Add(usdNoPoolTTL)
			o.mu.Unlock()
			return 0, false, nil
		}
	}
	p, src, ok, err := o.priceViaPools(ctx, pools, block, prefer)
	if err == nil && !ok && !anchored {
		// None of the known pools traded before this block: look for others
		// around it (once; the result is kept like the first search's).
		if err = search(); err == nil {
			p, src, ok, err = o.priceViaPools(ctx, pools, block, prefer)
		}
	}
	if err != nil {
		return 0, false, err
	}
	if !ok {
		until := now.Add(usdNoPriceTTL)
		o.mu.Lock()
		if set.noPrice == nil {
			set.noPrice = map[uint64]time.Time{}
		}
		for b, t := range set.noPrice {
			if !now.Before(t) {
				delete(set.noPrice, b)
			}
		}
		set.noPrice[block] = until
		o.mu.Unlock()
		return 0, false, fmt.Errorf("%s at block %d: %w (its pools have no trade within %d blocks before it)", asset, block, errNoPriceYet, o.cfg.PriceLookback)
	}
	o.noteUSDSource(asset, src)
	return p, true, nil
}

// addUSDPools merges pools found by a search around block into the asset's
// set and returns them all.
func (o *onchainSource) addUSDPools(asset, sym string, block uint64, found []*onchainState) []*onchainState {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.usdPools[asset]
	if sym != "" {
		set.sym = sym
	}
	set.anchors = append(set.anchors, block)
	have := map[string]bool{}
	for _, p := range set.pools {
		have[p.poolRef()] = true
	}
	for _, p := range found {
		if !have[p.poolRef()] {
			have[p.poolRef()] = true
			set.pools = append(set.pools, p)
		}
	}
	if len(set.pools) > 0 {
		set.noPoolUntil = time.Time{}
	}
	return slices.Clone(set.pools)
}

// noteUSDSource records the pool an asset was priced from, and logs one line
// when it is not the one used before.
func (o *onchainSource) noteUSDSource(asset string, src *onchainState) {
	short := "its " + src.QuoteSym + " pool (uniswap-" + src.Kind + ")"
	ref := short + " " + src.poolRef()
	o.mu.Lock()
	set := o.usdPools[asset]
	prev, sym := set.sourceRef, set.sym
	set.source, set.sourceRef = short, ref
	o.mu.Unlock()
	if prev == ref {
		return
	}
	if prev == "" {
		log.Printf("prices: USD price of %s (%s) from %s", sym, asset, ref)
		return
	}
	log.Printf("prices: USD price of %s (%s) now from %s (was %s)", sym, asset, ref, prev)
}

// priceViaPools: the asset's price in USD at block from the pool whose last
// trade at or before it is the most recent (ties: a WETH / ETH pool first),
// × the USD price of that pool's other side. ok=false: none of the pools has
// a usable trade within the look-back. The pool used last (prefer) is read
// first; once a pool has a trade at block X the others are only searched in
// [X, block], so a quiet pool costs little.
func (o *onchainSource) priceViaPools(ctx context.Context, pools []*onchainState, block uint64, prefer string) (float64, *onchainState, bool, error) {
	type hit struct {
		st   *onchainState
		l    rpcLog
		p    float64
		tier int
	}
	ordered := make([]*onchainState, 0, len(pools))
	for _, st := range pools {
		if o.usdPoolTier(st.Quote) == 0 {
			continue
		}
		if st.poolRef() == prefer {
			ordered = append([]*onchainState{st}, ordered...)
			continue
		}
		ordered = append(ordered, st)
	}
	var best *hit
	for _, st := range ordered {
		floor := uint64(1)
		if best != nil {
			floor = best.l.block()
		}
		addr, topics := st.logFilter()
		l, err := o.lastLogBetween(ctx, addr, topics, floor, block)
		if err != nil {
			return 0, nil, false, fmt.Errorf("last trade in the %s pool %s: %w", st.QuoteSym, st.poolRef(), err)
		}
		if l == nil {
			continue
		}
		p := st.priceOfLog(*l)
		if p <= 0 {
			continue // a drained pool (or a bound price): not a price
		}
		h := &hit{st: st, l: *l, p: p, tier: o.usdPoolTier(st.Quote)}
		switch {
		case best == nil, l.block() > best.l.block():
			best = h
		case l.block() == best.l.block() && (h.tier < best.tier || (h.tier == best.tier && l.index() > best.l.index())):
			best = h
		}
	}
	if best == nil {
		return 0, nil, false, nil
	}
	q, ok, err := o.quoteUSDDirect(ctx, best.st.Quote, block)
	if err != nil {
		return 0, nil, false, fmt.Errorf("USD price of %s: %w", best.st.QuoteSym, err)
	}
	if !ok {
		// ETH has no USD source (none configured, or no update within the
		// look-back): only the stablecoin pools can price the asset.
		var stable []*onchainState
		for _, st := range pools {
			if o.usdPoolTier(st.Quote) == 2 {
				stable = append(stable, st)
			}
		}
		if best.tier == 2 || len(stable) == 0 {
			return 0, nil, false, nil
		}
		return o.priceViaPools(ctx, stable, block, prefer)
	}
	return best.p * q, best.st, true, nil
}

// discoverUSDPools finds the asset's pools against WETH / native ETH or a
// stablecoin: the v2/v3 pools among who the asset was transferred to or from
// around block at, and its v4 pools (Initialize events) that traded in the
// same transactions. At most usdPoolsPerTier of each kind, the busiest
// first. No pool and no error: the asset has none (or is not a token we can
// read); an error is the node's and is never taken for "no pool".
func (o *onchainSource) discoverUSDPools(ctx context.Context, asset string, at uint64) (string, []*onchainState, error) {
	tm, err := o.rpc.tokenInfo(ctx, asset)
	if err != nil {
		if ctx.Err() == nil && (isRevert(err) || isNoSuchValue(err)) {
			return "", nil, nil // not a token contract we can read
		}
		return "", nil, err
	}
	accept := func(q string) bool {
		q = strings.ToLower(q)
		return q != asset && o.usdPoolTier(q) > 0
	}
	cands, from, to, err := o.transferCounterparties(ctx, asset, at)
	if err != nil {
		return "", nil, err
	}
	if len(cands) > 32 {
		cands = cands[:32] // the wanted pool is rarely the busiest counterparty
	}
	var found []*onchainState
	n := [3]int{}
	keep := func(st *onchainState) {
		if t := o.usdPoolTier(st.Quote); t > 0 && n[t] < usdPoolsPerTier {
			n[t]++
			found = append(found, st)
		}
	}
	for _, c := range cands {
		if c.addr == o.cfg.PoolManagerV4 {
			ranked, err := o.v4Ranked(ctx, asset, c.txs, from, to, accept)
			if err != nil {
				return "", nil, err
			}
			for _, p := range ranked {
				st := &onchainState{Kind: "v4", Pool: o.cfg.PoolManagerV4, PoolID: p.id, TokenIs0: p.c0 == asset,
					TokenDec: tm.Decimals, EntryBlock: at, Quote: p.c1}
				if !st.TokenIs0 {
					st.Quote = p.c0
				}
				keep(st)
			}
			continue
		}
		st := &onchainState{TokenDec: tm.Decimals, EntryBlock: at}
		ok, err := o.resolveV2V3(ctx, st, asset, c.addr)
		if err != nil {
			return "", nil, err
		}
		if ok && accept(st.Quote) {
			keep(st)
		}
	}
	for _, st := range found {
		qm, err := o.rpc.tokenInfo(ctx, st.Quote)
		if err != nil {
			return "", nil, err
		}
		st.QuoteDec, st.QuoteSym = qm.Decimals, qm.Symbol
	}
	return tm.Symbol, found, nil
}

// quoteSourceDirect is quoteSource without the pools ("none" = not a
// stablecoin, no feed, and not ETH with the WETH/USDG pool). In quoteUSD's order.
func (o *onchainSource) quoteSourceDirect(quote string) string {
	quote = strings.ToLower(quote)
	fm := o.feedMaps()
	switch {
	case o.cfg.Stables[quote]:
		return "stablecoin = $1"
	case o.feedFor(fm.rh, quote) != "":
		return "Chainlink on Robinhood Chain"
	case o.mainnet != nil && o.feedFor(fm.mainnet, quote) != "":
		return "Chainlink on Ethereum mainnet"
	case o.isETH(quote) && o.cfg.EthUSDPool != "":
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

// quoteUSDHour: USD price of the quote asset at a UTC hour boundary. Found
// prices are cached; an error or no source is asked again next time.
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
		return 0, fmt.Errorf("%s: %w", quote, errNoUSDSource)
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
