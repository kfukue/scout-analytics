package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Pons V2 (ponsfamily.com) on Robinhood Chain: tokens that trade on their own
// bonding curve first and graduate into a Uniswap v4 pool later.
//
// Source: github.com/ponsdotdev/pons-labs, contractsV2/src/v2
// (PonsV2LaunchFactory.sol, PonsV2BondingCurve.sol, PonsV2LauncherToken.sol).
//
//   - Every launch mints its whole supply to its curve. The token exposes
//     curve() and launchFactory(); the curve exposes token(), factory(),
//     pairToken() (the quote asset; zero = native ETH) and graduated().
//   - The curve emits CurveBuy / CurveSell for every trade. Fee and creator tax
//     are always taken on the quote leg:
//       CurveBuy(buyer, recipient, quoteIn, tokensOut, fee, tax): quoteIn is
//         what the buyer paid (gross); quoteIn − fee − tax moved along the curve.
//       CurveSell(seller, recipient, tokensIn, quoteOut, fee, tax): quoteOut is
//         what the seller received (net); quoteOut + fee + tax left the curve.
//     The price of a curve trade is the quote that moved along the curve ÷ the
//     tokens (the trade's average execution price).
//   - Graduation has two steps. graduate() (usually inside the buy that
//     crosses the threshold) closes the curve: CurveCompleted on the curve,
//     LaunchSwept on the factory. createGraduatedPool() (any later
//     transaction, by anyone) initialises the v4 pool behind the Pons hook and
//     emits PoolGraduated(token, …) on the factory in the same transaction.
//     Nothing trades in between.
//
// A call made while the token is on its curve is tracked on the curve
// (Kind "pons") until the graduation, then on the v4 pool: curve trades count
// up to and including the CurveCompleted block, v4 swaps from the
// PoolGraduated block on (the pool does not exist before it). The running
// extremes, candles and cursors simply continue across the switch.
//
// A curve has no LP that can be pulled, and its real quote reserve starts at
// zero: no liquidity-based rug check applies while the token is on the curve
// (curve events report the liquidity as unknown). The 5%-of-entry rule and the
// 1e6× backstop still apply; after the graduation the v4 pool is checked like
// any other.
// ---------------------------------------------------------------------------

const (
	defaultPonsFactory = "0x7ed598bcef8bd9edd8c97a195c6d13f40801ec7e" // PonsV2LaunchFactory
	defaultPonsHook    = "0xe5e702641ea86f4ae6cc3cdaed2b886f976be044" // PonsV2MemeHook (v4)
)

// ponsAddrEnv reads an address setting: lower case; "off" or "none" = "".
func ponsAddrEnv(key, def string) string {
	v := strings.ToLower(strings.TrimSpace(env(key, def)))
	if v == "off" || v == "none" {
		return ""
	}
	return v
}

var (
	topicCurveBuy       = topicOf("CurveBuy(address,address,uint256,uint256,uint256,uint256)")
	topicCurveSell      = topicOf("CurveSell(address,address,uint256,uint256,uint256,uint256)")
	topicCurveCompleted = topicOf("CurveCompleted(address,uint256,uint256)")
	topicTokenLaunched  = topicOf("TokenLaunched(address,address,address,address,uint256,uint256)")
	topicPoolGraduated  = topicOf("PoolGraduated(address,uint256,uint256,uint256)")
	topicLaunchSwept    = topicOf("LaunchSwept(address,uint256,uint256)")

	selPonsCurve     = selectorOf("curve()")
	selPonsToken     = selectorOf("token()")
	selPonsFactory   = selectorOf("factory()")
	selPonsPairToken = selectorOf("pairToken()")
	selPonsGraduated = selectorOf("graduated()")
)

// isCurveLog: the log is one of the curve's own events (buy, sell, completed).
func isCurveLog(l rpcLog) bool {
	if len(l.Topics) == 0 {
		return false
	}
	t := strings.ToLower(l.Topics[0])
	return t == topicCurveBuy || t == topicCurveSell || t == topicCurveCompleted
}

// logKind is how a log of this state's pool is decoded: v2, v3, v4, or curve.
func (st *onchainState) logKind(l rpcLog) string {
	if st.Kind != "pons" {
		return st.Kind
	}
	if isCurveLog(l) {
		return "curve"
	}
	return "v4"
}

// curveAmounts decodes a CurveBuy / CurveSell: the direction, the quote that
// moved along the curve (raw units) and the tokens (raw units). ok is false for
// CurveCompleted and for a trade with no usable amounts.
func curveAmounts(l rpcLog) (buy bool, quote, tokens *big.Int, ok bool) {
	if len(l.Topics) == 0 {
		return false, nil, nil, false
	}
	data := unhex(l.Data)
	if len(data) < 4*32 {
		return false, nil, nil, false
	}
	fee, tax := word(data, 2), word(data, 3)
	switch strings.ToLower(l.Topics[0]) {
	case topicCurveBuy: // quoteIn (gross), tokensOut, fee, tax
		quote = new(big.Int).Sub(word(data, 0), fee)
		quote.Sub(quote, tax)
		tokens, buy = word(data, 1), true
	case topicCurveSell: // tokensIn, quoteOut (net), fee, tax
		quote = new(big.Int).Add(word(data, 1), fee)
		quote.Add(quote, tax)
		tokens = word(data, 0)
	default:
		return false, nil, nil, false
	}
	if quote.Sign() <= 0 || tokens.Sign() <= 0 {
		return false, nil, nil, false
	}
	return buy, quote, tokens, true
}

// curvePrice: the token's price in quote units of a curve trade (0 = none).
func (st *onchainState) curvePrice(l rpcLog) float64 {
	_, q, tok, ok := curveAmounts(l)
	if !ok {
		return 0
	}
	p := bigToFloat(q) / pow10(st.QuoteDec) / (bigToFloat(tok) / pow10(st.TokenDec))
	if p <= 0 || math.IsInf(p, 0) || math.IsNaN(p) {
		return 0
	}
	return p
}

// curveTrade: direction and size of a curve trade, in quote units: what the
// buyer paid (fee and tax included) or what left the curve on a sell (before
// fee and tax), i.e. the quote value of the trade before costs.
func curveTrade(l rpcLog, quoteDec int) (buy bool, vol float64, ok bool) {
	if len(l.Topics) == 0 {
		return false, 0, false
	}
	data := unhex(l.Data)
	if len(data) < 4*32 {
		return false, 0, false
	}
	switch strings.ToLower(l.Topics[0]) {
	case topicCurveBuy:
		return true, bigToFloat(word(data, 0)) / pow10(quoteDec), true
	case topicCurveSell:
		gross := new(big.Int).Add(word(data, 1), word(data, 2))
		gross.Add(gross, word(data, 3))
		return false, bigToFloat(gross) / pow10(quoteDec), true
	}
	return false, 0, false
}

// isRevertOrNoCode: an eth_call that reverted or hit an address without code
// (as opposed to a node that could not answer, e.g. no historical state).
func isRevertOrNoCode(err error) bool {
	if err == nil {
		return false
	}
	if err.Error() == "empty result" {
		return true
	}
	var re *rpcError
	return errors.As(err, &re) && strings.Contains(strings.ToLower(re.Message), "revert")
}

// ponsCurveFor checks whether curve is the Pons V2 curve of token (factory()
// is the configured Pons factory and token() is the token) and returns its
// quote asset. ok is false when it is not; err only for node trouble.
func (o *onchainSource) ponsCurveFor(ctx context.Context, token, curve string) (quote string, ok bool, err error) {
	if o.cfg.PonsFactory == "" || curve == zeroAddr {
		return "", false, nil
	}
	fb, err := o.rpc.ethCall(ctx, curve, selPonsFactory, 0)
	if err != nil {
		return "", false, nonRPC(err)
	}
	if addrFromWord(fb) != o.cfg.PonsFactory {
		return "", false, nil
	}
	tb, err := o.rpc.ethCall(ctx, curve, selPonsToken, 0)
	if err != nil {
		return "", false, nonRPC(err)
	}
	if addrFromWord(tb) != token {
		return "", false, nil
	}
	pb, err := o.rpc.ethCall(ctx, curve, selPonsPairToken, 0)
	if err != nil {
		return "", false, nonRPC(err)
	}
	return addrFromWord(pb), true, nil
}

// ponsCurveOfToken asks the token for its curve (PonsV2LauncherToken.curve())
// and checks it. "" = not a Pons V2 token (or the token has no such getter).
func (o *onchainSource) ponsCurveOfToken(ctx context.Context, token string) (curve, quote string, err error) {
	if o.cfg.PonsFactory == "" {
		return "", "", nil
	}
	b, err := o.rpc.ethCall(ctx, token, selPonsCurve, 0)
	if err != nil {
		return "", "", nonRPC(err)
	}
	curve = addrFromWord(b)
	q, ok, err := o.ponsCurveFor(ctx, token, curve)
	if err != nil || !ok {
		return "", "", err
	}
	return curve, q, nil
}

// ponsGraduatedAt reads curve.graduated() at a block (0 = latest). A revert
// or no code there (the curve did not exist yet) is false; any other error
// (e.g. no historical state on a full node) is returned.
func (o *onchainSource) ponsGraduatedAt(ctx context.Context, curve string, block uint64) (bool, error) {
	b, err := o.rpc.ethCall(ctx, curve, selPonsGraduated, block)
	if err != nil {
		if isRevertOrNoCode(err) {
			return false, nil
		}
		return false, err
	}
	return word(b, 0).Sign() != 0, nil
}

// ponsSearchChunk: the largest eth_getLogs range the graduation searches ask
// for (split automatically, like any scan, when the node refuses a range or it
// times out). They look for one rare event filtered by address and topics,
// which a node with a full log index answers quickly over any range.
const ponsSearchChunk = 4_000_000

// ponsSearchProgressEvery: a graduation search under a progress context (the
// tracker, -price-check) says how far it got this often.
var ponsSearchProgressEvery = 5 * time.Second

// errPonsCloseNotFound: graduated() says the curve has closed, but the event
// logs show neither its CurveCompleted nor the token's PoolGraduated.
var errPonsCloseNotFound = errors.New("the curve has closed, but neither CurveCompleted nor PoolGraduated was found in the event logs")

// ponsFindCurveDone finds the block where the curve closed (graduated() turned
// true, the block of CurveCompleted), knowing it is closed at block hi:
// bisection over graduated() at past blocks (about 25 eth_calls) on an archive
// node, else the event logs (ponsSearchClose, no look-back limit). 0 = not
// found.
func (o *onchainSource) ponsFindCurveDone(ctx context.Context, st *onchainState, hi uint64) (uint64, error) {
	if !o.noState.Load() {
		lo, top := uint64(0), hi // graduated(lo) is false (block 0: before any curve), graduated(top) true
		bisected := true
		for top-lo > 1 {
			mid := lo + (top-lo)/2
			g, err := o.ponsGraduatedAt(ctx, st.Curve, mid)
			if err != nil {
				if ctx.Err() != nil || nonRPC(err) != nil {
					return 0, err // transport trouble: try again later
				}
				// No historical state (full node): read the events instead, and
				// do not ask the next token's curve about the past either.
				if o.noState.CompareAndSwap(false, true) {
					log.Printf("pons: node has no historical state (%v) — finding graduations from event logs", err)
				}
				bisected = false
				break
			}
			if g {
				top = mid
			} else {
				lo = mid
			}
		}
		if bisected {
			return top, nil
		}
	}
	return o.ponsSearchClose(ctx, st, hi)
}

// ponsSearchClose finds the close in the event logs, with no look-back limit:
// the curve's CurveCompleted and the factory's PoolGraduated with the token as
// topic 1, in windows around the call block (st.EntryBlock; hi if unknown)
// that double in size on both sides, starting at SCOUT_DISCOVERY_BLOCKS, until
// one of the two shows up or all of [1, hi] has been searched. The graduation
// can be before or after the call; it is usually close to it. Each new window
// costs up to four requests (two filters, both sides; more only when the node
// splits a range), so a graduation d blocks from the call takes about
// 4 × log2(d / SCOUT_DISCOVERY_BLOCKS) requests. Returns CurveCompleted's
// block, else PoolGraduated's (the pool is created after the close and nothing
// trades in between, so every curve trade comes before it); 0 = neither. A
// PoolGraduated found on the way is kept in st.ponsGradAt for ponsFindPool.
func (o *onchainSource) ponsSearchClose(ctx context.Context, st *onchainState, hi uint64) (uint64, error) {
	if hi < 1 {
		return 0, nil
	}
	center := st.EntryBlock
	if center < 1 || center > hi {
		center = hi
	}
	var done, grad uint64
	search := func(a, b uint64) error {
		err := o.rpc.getLogsChunkedUpTo(ctx, st.Curve, []any{topicCurveCompleted}, a, b, ponsSearchChunk, func(l rpcLog) {
			if done == 0 || l.block() < done {
				done = l.block()
			}
		})
		if err != nil {
			return fmt.Errorf("CurveCompleted of curve %s in blocks %d-%d: %w", st.Curve, a, b, err)
		}
		if st.Token == "" {
			return nil
		}
		err = o.rpc.getLogsChunkedUpTo(ctx, o.cfg.PonsFactory, []any{topicPoolGraduated, addrTopic(st.Token)}, a, b, ponsSearchChunk, func(l rpcLog) {
			if grad == 0 || l.block() < grad {
				grad = l.block()
			}
		})
		if err != nil {
			return fmt.Errorf("PoolGraduated of %s in blocks %d-%d: %w", st.Token, a, b, err)
		}
		return nil
	}
	span := max(o.cfg.DiscoveryBlocks, 1)
	lo, top := center+1, center // searched so far: [lo, top] (empty at first)
	progress, said := progressFrom(ctx), time.Now()
	for {
		a := uint64(1)
		if center > span {
			a = center - span
		}
		b := min(hi, center+span)
		if a < lo {
			if err := search(a, lo-1); err != nil {
				return 0, err
			}
		}
		if b > top {
			if err := search(top+1, b); err != nil {
				return 0, err
			}
		}
		lo, top = a, b
		if done > 0 || grad > 0 || (a == 1 && b == hi) {
			break
		}
		if progress != nil && time.Since(said) >= ponsSearchProgressEvery {
			// Each window is quick, but there can be many: say how far it got.
			said = time.Now()
			log.Printf("%spons graduation of %s: searched blocks %d → %d around the call block %d (%.0f%% of blocks 1 → %d), not found yet",
				labelPrefix(scanLabelFrom(ctx)), st.Token, lo, top, center, float64(top-lo+1)/float64(hi)*100, hi)
		}
		span *= 2
	}
	if grad > 0 {
		st.ponsGradAt = grad
	}
	if done > 0 {
		return done, nil
	}
	return grad, nil
}

// ponsCheckClosed: once per loaded state, ask the curve whether it has closed
// (one eth_call). When it has and the block is not known yet, find it. This
// keeps a scan whose cursor is already past the closing block (e.g. the latest
// pass after its state was overwritten) from staying on the curve for good.
// closed reports what graduated() said (true as well when the block is known).
// A closed curve whose close cannot be found is an error (errPonsCloseNotFound;
// the call is tried again later), never a guess.
func (o *onchainSource) ponsCheckClosed(ctx context.Context, st *onchainState) (closed bool, err error) {
	if st.PonsDone > 0 {
		return true, nil
	}
	if st.ponsChecked {
		return false, nil
	}
	g, err := o.ponsGraduatedAt(ctx, st.Curve, 0)
	if err != nil {
		return false, fmt.Errorf("pons curve %s graduated(): %w", st.Curve, err)
	}
	if g {
		head, err := o.rpc.blockNumber(ctx)
		if err != nil {
			return false, err
		}
		c, err := o.ponsFindCurveDone(ctx, st, head)
		if err != nil {
			return false, fmt.Errorf("pons curve %s: closing block: %w", st.Curve, err)
		}
		if c == 0 {
			return false, fmt.Errorf("pons token %s, curve %s (blocks 1-%d): %w", st.Token, st.Curve, head, errPonsCloseNotFound)
		}
		if st.PonsDone == 0 {
			st.PonsDone = c
		}
	}
	st.ponsChecked = true
	return g, nil
}

// ponsCurrencies: the v4 currencies of the token's graduated pool (native ETH,
// the zero address, and smaller addresses come first).
func (st *onchainState) ponsCurrencies() (c0, c1 string) {
	c0, c1 = st.Quote, st.Token
	if strings.Compare(c1, c0) < 0 {
		c0, c1 = c1, c0
	}
	return c0, c1
}

// ponsWantHook: the hook the graduated pool must have ("" = any).
func (o *onchainSource) ponsWantHook(st *onchainState) string {
	if st.Hook != "" {
		return st.Hook
	}
	return o.cfg.PonsHook
}

// ponsFindPool looks for the graduation (PoolGraduated of the token on the
// factory) after the curve closed, up to block to, and then for the v4 pool
// it created: the PoolManager's Initialize for the token's two currencies in
// that very block, with the Pons hook. First a narrow window after the close,
// then the rest (in large ranges); where PoolGraduated is missing, the pool's
// Initialize (the token's currencies with the Pons hook) is looked for in the
// same ranges. PonsSeen remembers how far it looked, so a launch whose pool is
// never created is not searched again from the start every time.
func (o *onchainSource) ponsFindPool(ctx context.Context, st *onchainState, to uint64) error {
	if st.PonsDone == 0 || st.PoolID != "" {
		return nil
	}
	if g := st.ponsGradAt; g >= st.PonsDone && g <= to {
		return o.ponsPoolAt(ctx, st, g) // found while locating the close
	}
	from := st.PonsDone
	if st.PonsSeen >= from {
		from = st.PonsSeen + 1
	}
	if from > to {
		return nil
	}
	narrow := min(to, from+o.cfg.DiscoveryBlocks)
	for _, r := range [][2]uint64{{from, narrow}, {narrow + 1, to}} {
		if r[0] > r[1] {
			continue
		}
		var grad uint64
		err := o.rpc.getLogsChunkedUpTo(ctx, o.cfg.PonsFactory, []any{topicPoolGraduated, addrTopic(st.Token)}, r[0], r[1], ponsSearchChunk, func(l rpcLog) {
			if grad == 0 {
				grad = l.block()
			}
		})
		if err != nil {
			return fmt.Errorf("pons graduation of %s: %w", st.Token, err)
		}
		if grad > 0 {
			return o.ponsPoolAt(ctx, st, grad)
		}
		found, err := o.ponsInitIn(ctx, st, r[0], r[1])
		if err != nil || found {
			return err
		}
	}
	st.PonsSeen = to // swept, no pool yet: the price stays the last curve price
	return nil
}

// ponsInitIn looks for the graduated pool's Initialize itself in [from, to]
// (for a graduation whose PoolGraduated is missing): the token's two
// currencies with the Pons hook. Only with a hook to check: any other pool of
// the same currencies would otherwise pass for it.
func (o *onchainSource) ponsInitIn(ctx context.Context, st *onchainState, from, to uint64) (bool, error) {
	want := o.ponsWantHook(st)
	if want == "" {
		return false, nil
	}
	c0, c1 := st.ponsCurrencies()
	found := false
	err := o.rpc.getLogsChunkedUpTo(ctx, o.cfg.PoolManagerV4, []any{topicInitV4, nil, addrTopic(c0), addrTopic(c1)}, from, to, ponsSearchChunk, func(l rpcLog) {
		d := unhex(l.Data) // Initialize data: fee, tickSpacing, hooks, sqrtPriceX96, tick
		if found || len(l.Topics) < 4 || len(d) < 3*32 || addrFromWord(d[64:96]) != want {
			return
		}
		st.PoolID, st.TokenIs0, st.PonsGrad, st.Hook = strings.ToLower(l.Topics[1]), c0 == st.Token, l.block(), want
		found = true
	})
	if err != nil {
		return false, fmt.Errorf("pons v4 pool of %s (Initialize in blocks %d-%d): %w", st.Token, from, to, err)
	}
	return found, nil
}

// ponsPoolAt finds the v4 pool PoolGraduated announced at block grad: the
// PoolManager's Initialize for the token's two currencies in that very block,
// with the Pons hook.
func (o *onchainSource) ponsPoolAt(ctx context.Context, st *onchainState, grad uint64) error {
	c0, c1 := st.ponsCurrencies()
	logs, err := o.rpc.retryLogs(ctx, true, func() ([]rpcLog, error) {
		return o.rpc.getLogs(ctx, o.cfg.PoolManagerV4, []any{topicInitV4, nil, addrTopic(c0), addrTopic(c1)}, grad, grad)
	})
	if err != nil {
		return fmt.Errorf("pons v4 pool of %s (block %d): %w", st.Token, grad, err)
	}
	want := o.ponsWantHook(st)
	var seen []string // hooks of the pools of these currencies initialised in that block
	for _, l := range logs {
		d := unhex(l.Data) // Initialize data: fee, tickSpacing, hooks, sqrtPriceX96, tick
		if len(l.Topics) < 4 || len(d) < 3*32 {
			continue
		}
		hook := addrFromWord(d[64:96])
		if want != "" && hook != want {
			seen = append(seen, hook)
			continue
		}
		st.PoolID, st.TokenIs0, st.PonsGrad, st.Hook = strings.ToLower(l.Topics[1]), c0 == st.Token, grad, hook
		return nil
	}
	// Say which assumption broke, so the owner knows what to set.
	if len(seen) > 0 {
		return fmt.Errorf("pons: PoolGraduated for %s at block %d; its v4 pool has hook %s, not the Pons hook %s (set SCOUT_PONS_HOOK, or off for any hook)",
			st.Token, grad, strings.Join(seen, ", "), want)
	}
	return fmt.Errorf("pons: PoolGraduated for %s at block %d, but no v4 Initialize of the currencies %s / %s in that block (the pool may use other currencies, e.g. WETH instead of native ETH)",
		st.Token, grad, c0, c1)
}

// scanPons calls fn for every price event of a Pons token in [from, to], in
// block order: curve trades up to the block the curve closed, then the v4
// pool's swaps from the graduation block on. Finding the close (CurveCompleted
// among the curve's events, or ponsCheckClosed) and the pool updates st.
func (o *onchainSource) scanPons(ctx context.Context, st *onchainState, from, to uint64, fn func(rpcLog)) error {
	if _, err := o.ponsCheckClosed(ctx, st); err != nil {
		return err
	}
	curveTo := to
	if st.PonsDone > 0 && st.PonsDone < curveTo {
		curveTo = st.PonsDone
	}
	if from <= curveTo {
		err := o.rpc.getLogsChunked(ctx, st.Curve, []any{[]any{topicCurveBuy, topicCurveSell, topicCurveCompleted}}, from, curveTo, func(l rpcLog) {
			if len(l.Topics) > 0 && strings.EqualFold(l.Topics[0], topicCurveCompleted) {
				if st.PonsDone == 0 || l.block() < st.PonsDone {
					st.PonsDone = l.block()
				}
				return
			}
			if st.PonsDone > 0 && l.block() > st.PonsDone {
				return // (a curve cannot trade after it closed)
			}
			fn(l)
		})
		if err != nil {
			return err
		}
	}
	if st.PonsDone == 0 || st.PonsDone > to {
		return nil
	}
	if err := o.ponsFindPool(ctx, st, to); err != nil {
		return err
	}
	if st.PoolID == "" {
		return nil
	}
	v4From := max(from, st.PonsGrad)
	if v4From > to {
		return nil
	}
	return o.rpc.getLogsChunked(ctx, o.cfg.PoolManagerV4, []any{topicSwapV4, st.PoolID}, v4From, to, fn)
}

// scanLogs calls fn for every price event of st's pool in [from, to], in block
// order (for a Pons token: its curve, then its v4 pool).
func (o *onchainSource) scanLogs(ctx context.Context, st *onchainState, from, to uint64, fn func(rpcLog)) error {
	if from > to {
		return nil
	}
	if st.Kind == "pons" {
		return o.scanPons(ctx, st, from, to, fn)
	}
	addr, topics := st.logFilter()
	return o.rpc.getLogsChunked(ctx, addr, topics, from, to, fn)
}

// discoverPons tracks a confirmed Pons V2 token (the token's curve() getter,
// or a counterparty of its transfers that answers like a Pons curve):
//   - On the curve at the call (or the curve closed after it): Kind "pons".
//   - Graduated before the call (PoolGraduated at or before entryBlock): a plain
//     v4 state on the Pons pool, so the normal v4 rules apply from the start.
//   - Closed, but the close cannot be found in the event logs: an error (the
//     call is tried again later). A confirmed Pons token never goes to the
//     generic counterparty search (which knows nothing of the curve).
func (o *onchainSource) discoverPons(ctx context.Context, token string, tm tokenMeta, entryBlock uint64, curve, quote string) (*onchainState, error) {
	qm, err := o.rpc.tokenInfo(ctx, quote)
	if err != nil {
		return nil, err
	}
	st := &onchainState{Kind: "pons", Pool: curve, Curve: curve, Token: token, TokenDec: tm.Decimals,
		Quote: quote, QuoteSym: qm.Symbol, QuoteDec: qm.Decimals, EntryBlock: entryBlock, Hook: o.cfg.PonsHook}
	if _, err := o.ponsCheckClosed(ctx, st); err != nil {
		return nil, err
	}
	if st.PonsDone == 0 || st.PonsDone > entryBlock {
		return st, nil // on the curve at the call
	}
	// The curve closed at or before the call: was the v4 pool there already?
	latest, err := o.rpc.blockNumber(ctx)
	if err != nil {
		return nil, err
	}
	if err := o.ponsFindPool(ctx, st, latest); err != nil {
		return nil, err
	}
	if st.PoolID != "" && st.PonsGrad <= entryBlock {
		st.Kind, st.Pool = "v4", o.cfg.PoolManagerV4 // graduated before the call: v4 from the start
	}
	return st, nil
}

// ponsSummary describes a Pons state for -price-check.
func (st *onchainState) ponsSummary() []string {
	if st.Curve == "" {
		return nil
	}
	out := []string{fmt.Sprintf("launchpad:    Pons V2 (curve %s, quote %s %s)", st.Curve, st.QuoteSym, st.Quote)}
	if st.PonsDone == 0 {
		return append(out, "graduation:   not graduated (still on the bonding curve)")
	}
	when := ""
	if st.EntryBlock > 0 && st.PonsDone > st.EntryBlock {
		when = "graduated after the call: "
	}
	switch {
	case st.PoolID != "":
		out = append(out, fmt.Sprintf("graduation:   %scurve closed at block %d, v4 pool from block %d: PoolManager id %s (hook %s, token is currency%d)",
			when, st.PonsDone, st.PonsGrad, st.PoolID, st.Hook, map[bool]int{true: 0, false: 1}[st.TokenIs0]))
	case st.PonsSeen >= st.PonsDone:
		out = append(out, fmt.Sprintf("graduation:   %scurve closed at block %d; no v4 pool yet (searched through block %d)", when, st.PonsDone, st.PonsSeen))
	default: // the pool was not looked for (yet)
		out = append(out, fmt.Sprintf("graduation:   %scurve closed at block %d; v4 pool not looked up", when, st.PonsDone))
	}
	return out
}
