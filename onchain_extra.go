package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"math/big"
	"slices"
	"sort"
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
	// errUSDSearchPending: the whole-history search for the asset's pools
	// stopped at its per-run bounds (usdPoolsInHistory) and has not found a
	// pool that prices the block yet. Temporary and never final (unlike
	// errNoPriceYet after the deadline): the next search goes on from there,
	// so it ends in a price, errNoPriceYet or the definitive "no USD source".
	errUSDSearchPending = errors.New("USD pool search not finished")
)

const (
	usdNoPoolTTL    = time.Hour     // "no pool anywhere" is asked again after this
	usdNoPriceTTL   = 6 * time.Hour // "no price yet" at a block is asked again after this
	usdPoolsPerTier = 3             // own pools kept per kind (WETH / ETH, stablecoin)
)

// usdPoolSet: what is known about one asset's own pools. Changed under o.mu only.
type usdPoolSet struct {
	sym     string
	pools   []*onchainState      // every acceptable pool found (de-duplicated)
	anchors []uint64             // the blocks the near-block pool search ran around
	noPrice map[uint64]time.Time // block → "no price yet" until then
	// The whole-history search (usdPoolsInHistory): no acceptable pool in
	// blocks [1, noPoolThrough], kept until noPoolUntil. Definitive only for a
	// block at or before noPoolThrough.
	noPoolUntil   time.Time
	noPoolThrough uint64
	// histThrough: the asset's transfers were read through this block by the
	// whole-history search; histSeen: the counterparties of those transfers
	// already checked (pool or not: a contract's token0/token1 never change);
	// histPending: counterparties read but not checked yet (the per-sweep cap),
	// with their transfer counts; histBusy: a sweep is running (closed when it
	// ends), so concurrent callers wait for it instead of repeating it.
	histThrough uint64
	histSeen    map[string]bool
	histPending map[string]int
	histBusy    chan struct{}
	source      string // the pool used last (quoteSource, and the change log)
	sourceRef   string // … with its address / id
}

// Bounds of one whole-history sweep (usdPoolsInHistory). A sweep that hits
// one stops there; the next sweep goes on from where it stopped, and the
// answer stays "no price yet" (never the definitive "no pool") until a sweep
// has read every transfer and checked every counterparty. Variables so tests
// can lower them.
var (
	usdHistMaxLogs   = 100_000 // transfer logs read per sweep (then cut at a block boundary)
	usdHistMaxChecks = 256     // counterparties checked (token0/token1) per sweep, the busiest first
)

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
// Two searches find the pools. The near-block search (discoverUSDPools) reads
// the asset's transfers around the block. When it finds none that prices the
// block, the whole-history search (usdPoolsInHistory) looks at every v4 pool
// of the asset and every counterparty of its transfers from its first one to
// the chain's head. The answer is then:
//   - a price: a pool, from either search, traded within the look-back;
//   - ok=false, no error (definitive, the call is tracked in quote units): the
//     whole-history search found no pool against WETH / ETH / a stablecoin at
//     all (or the asset is not a token we can read);
//   - errNoPriceYet (temporary; final for the entry after the gave_up deadline,
//     see noPriceYetIsFinal): such pools exist somewhere in the asset's
//     history, but none traded within the look-back before the block.
//
// A search around one block never decides for another block: the near-block
// search runs again for a block outside its window, and "no pool anywhere"
// holds only for blocks the whole-history search covered.
//
// The whole-history search is bounded per run (usdHistMaxLogs,
// usdHistMaxChecks); while it has not covered the whole history the answer is
// errUSDSearchPending (temporary, never final), never the definitive one.
//
// Caching: the pools found are kept; "no pool anywhere" is kept for
// usdNoPoolTTL (then the whole-history search reads only the new blocks),
// "no price yet" at a block for usdNoPriceTTL (only once the history was
// searched in full); an error of the node is never kept.
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
	noPool := now.Before(set.noPoolUntil) && block <= set.noPoolThrough
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
	var (
		p   float64
		src *onchainState
		ok  bool
		err error
		// complete: no whole-history sweep ran, or the one that ran read the
		// asset's whole history (see usdPoolsInHistory)
		complete = true
	)
	if len(pools) > 0 {
		p, src, ok, err = o.priceViaPools(ctx, pools, block, prefer)
	}
	if err == nil && !ok && !anchored {
		// No known pool traded before this block (or none is known): look
		// for others around it (once; the result is kept like the first
		// search's).
		if err = search(); err == nil && len(pools) > 0 {
			p, src, ok, err = o.priceViaPools(ctx, pools, block, prefer)
		}
	}
	if err == nil && !ok {
		// Nothing near the block prices it: every pool in the asset's
		// history (the near-block search sees only its window).
		var h usdHistory
		if h, err = o.usdPoolsInHistory(ctx, asset); err == nil {
			known := len(pools)
			pools = h.pools
			switch {
			case len(pools) == 0 && !h.complete:
				// The sweep stopped at its bounds: not an answer yet (the
				// next one goes on from there).
				return 0, false, fmt.Errorf("%s at block %d: %w (no pool against WETH / ETH / a stablecoin found yet: %s)",
					asset, block, errUSDSearchPending, h.progress)
			case len(pools) == 0:
				o.mu.Lock()
				set.noPoolUntil, set.noPoolThrough = now.Add(usdNoPoolTTL), h.through
				o.mu.Unlock()
				if block <= h.through {
					return 0, false, nil // no acceptable pool anywhere: definitive
				}
				// A block after the searched history (a call at the chain's
				// head): its pool may not be there yet.
				return 0, false, fmt.Errorf("%s at block %d: %w (no pool against WETH / ETH / a stablecoin through block %d)",
					asset, block, errNoPriceYet, h.through)
			case len(pools) > known: // pools the searches before had not seen
				p, src, ok, err = o.priceViaPools(ctx, pools, block, prefer)
			}
			complete = h.complete
		}
	}
	if err != nil {
		return 0, false, err
	}
	if !ok && !complete {
		// Pools, none with a trade before the block, and the sweep has not
		// read the whole history yet: not kept (the next sweep may find more).
		return 0, false, fmt.Errorf("%s at block %d: %w (the pools found so far have no trade within %d blocks before it)",
			asset, block, errUSDSearchPending, o.cfg.PriceLookback)
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
	o.usdPools[asset].anchors = append(o.usdPools[asset].anchors, block)
	o.mu.Unlock()
	return o.mergeUSDPools(asset, sym, found)
}

// mergeUSDPools merges pools found by any search into the asset's set and
// returns them all.
func (o *onchainSource) mergeUSDPools(asset, sym string, found []*onchainState) []*onchainState {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.usdPools[asset]
	if sym != "" {
		set.sym = sym
	}
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
		set.noPoolUntil, set.noPoolThrough = time.Time{}, 0
	}
	return slices.Clone(set.pools)
}

// usdHistory is the answer of one whole-history sweep (usdPoolsInHistory).
type usdHistory struct {
	pools    []*onchainState // every acceptable pool of the asset known now (all searches)
	through  uint64          // the last block the answer covers (settled: 1000 under the head)
	complete bool            // every transfer through the head read, every counterparty checked
	progress string          // what is left, for the error message when not complete
}

// usdPoolsInHistory looks for the asset's pools against WETH / native ETH or
// a stablecoin anywhere in its history, up to the chain's head:
//   - v4: its pools from the PoolManager's Initialize events (v4PoolsOf: two
//     indexed queries over all blocks, then only the new blocks);
//   - v2/v3: the addresses its transfers went to or came from (address-
//     filtered eth_getLogs from its first transfer on, split by the node if
//     needed), each checked with token0()/token1() once per process, the
//     busiest first.
//
// Cost and bounds: one sweep reads at most usdHistMaxLogs transfer logs (cut
// at a block boundary) and checks at most usdHistMaxChecks counterparties (a
// wallet costs one eth_call, token0() answering empty; a contract two to four).
// The next sweep goes on from there: the blocks after the last one read, and
// the counterparties left over. complete=false until a sweep has done both
// to the end; only then can "no pool" be definitive. Everything is kept per
// process, so once an asset's history is covered a later sweep costs a few
// requests (the new blocks only). One sweep per asset at a time: a caller
// that finds one running waits for it, then sweeps what is still left
// (usually nothing). The pools found are merged into the asset's set before
// the sweep ends. An error is the node's; nothing of a failed sweep is kept.
func (o *onchainSource) usdPoolsInHistory(ctx context.Context, asset string) (usdHistory, error) {
	var set *usdPoolSet
	for {
		o.mu.Lock()
		set = o.usdPools[asset]
		busy := set.histBusy
		if busy == nil {
			set.histBusy = make(chan struct{})
			o.mu.Unlock()
			break
		}
		o.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return usdHistory{}, ctx.Err()
		}
	}
	defer func() {
		o.mu.Lock()
		close(set.histBusy)
		set.histBusy = nil
		o.mu.Unlock()
	}()

	head, err := o.rpc.blockNumber(ctx)
	if err != nil {
		return usdHistory{}, err
	}
	through := head
	if head > 1000 {
		through = head - 1000
	}
	tm, err := o.rpc.tokenInfo(ctx, asset)
	if err != nil {
		if ctx.Err() == nil && (isRevert(err) || isNoSuchValue(err)) {
			// not a token contract we can read
			return usdHistory{pools: o.mergeUSDPools(asset, "", nil), through: through, complete: true}, nil
		}
		return usdHistory{}, err
	}
	var found []*onchainState
	n := [3]int{}
	keep := func(st *onchainState) { // up to usdPoolsPerTier of each kind
		if t := o.usdPoolTier(st.Quote); t > 0 && n[t] < usdPoolsPerTier {
			n[t]++
			found = append(found, st)
		}
	}
	// v4: every pool initialised with the asset as one of its currencies.
	v4, err := o.v4PoolsOf(ctx, asset, head)
	if err != nil {
		return usdHistory{}, err
	}
	for _, p := range v4 {
		st := &onchainState{Kind: "v4", Pool: o.cfg.PoolManagerV4, PoolID: p.id, TokenIs0: p.c0 == asset,
			TokenDec: tm.Decimals, EntryBlock: p.block, Quote: p.c1}
		if !st.TokenIs0 {
			st.Quote = p.c0
		}
		if strings.ToLower(st.Quote) != asset {
			keep(st)
		}
	}
	// v2/v3: the counterparties of the transfers not read yet, plus those
	// read by an earlier sweep but not checked.
	o.mu.Lock()
	from := set.histThrough + 1
	seen := maps.Clone(set.histSeen)
	counts := maps.Clone(set.histPending)
	o.mu.Unlock()
	if counts == nil {
		counts = map[string]int{}
	}
	readTo := head // transfers read through this block
	if from <= head {
		lctx, stop := context.WithCancel(ctx)
		var nLogs int
		var cut, last uint64 // cut: the first block not read (0 = read to the head)
		err := o.rpc.getLogsChunkedUpTo(lctx, asset, []any{topicTransfer}, from, head, head+1, func(l rpcLog) {
			if cut > 0 {
				return
			}
			if b := l.block(); nLogs >= usdHistMaxLogs && b != last {
				cut = b // logs come in block order: every block before b is complete
				stop()
				return
			}
			nLogs++
			last = l.block()
			if len(l.Topics) < 3 {
				return
			}
			for _, t := range l.Topics[1:3] {
				if a := addrFromWord(unhex(t)); a != zeroAddr && a != asset && a != o.cfg.PoolManagerV4 {
					counts[a]++
				}
			}
		})
		stop()
		if err != nil && (cut == 0 || ctx.Err() != nil) {
			return usdHistory{}, fmt.Errorf("transfers of %s in blocks %d-%d: %w", asset, from, head, err)
		}
		if cut > 0 {
			readTo = cut - 1
		}
	}
	var cands []string
	for a := range counts {
		if !seen[a] {
			cands = append(cands, a)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if counts[cands[i]] != counts[cands[j]] {
			return counts[cands[i]] > counts[cands[j]]
		}
		return cands[i] < cands[j]
	})
	var checked []string
	pending := map[string]int{}
	for i, a := range cands {
		if i >= usdHistMaxChecks {
			pending[a] = counts[a] // left for the next sweep
			continue
		}
		st := &onchainState{TokenDec: tm.Decimals, EntryBlock: head}
		ok, err := o.resolveV2V3(ctx, st, asset, a)
		if err != nil {
			return usdHistory{}, fmt.Errorf("is %s a pool of %s: %w", a, asset, err)
		}
		if ok && o.usdPoolTier(st.Quote) > 0 && strings.ToLower(st.Quote) != asset {
			// A pool beyond usdPoolsPerTier of its kind is left out (the
			// busier ones of that kind came first) and not asked again.
			keep(st)
		}
		checked = append(checked, a)
	}
	// Pools seen in earlier sweeps are already in the asset's set.
	for _, st := range found {
		qm, err := o.rpc.tokenInfo(ctx, st.Quote)
		if err != nil {
			return usdHistory{}, err
		}
		st.QuoteDec, st.QuoteSym = qm.Decimals, qm.Symbol
	}
	o.mu.Lock()
	if set.histSeen == nil {
		set.histSeen = map[string]bool{}
	}
	for _, a := range checked {
		set.histSeen[a] = true
	}
	set.histPending = pending
	if readTo < head {
		set.histThrough = readTo // stopped at the log cap: go on from the cut
	} else {
		set.histThrough = max(set.histThrough, through) // blocks near the head are read again next time
	}
	if tm.Symbol != "" {
		set.sym = tm.Symbol
	}
	o.mu.Unlock()
	h := usdHistory{pools: o.mergeUSDPools(asset, "", found), through: through, complete: readTo == head && len(pending) == 0}
	if !h.complete {
		h.progress = fmt.Sprintf("transfers read through block %d of %d, %d counterparties left to check", readTo, head, len(pending))
	}
	return h, nil
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
