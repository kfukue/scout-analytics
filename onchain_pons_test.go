package main

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pons V2 bonding curves (fake chain, no database).
// ---------------------------------------------------------------------------

const tPonsCurve = "0x00000000000000000000000000000000000c0e01"

// fakePons is a Pons V2 launch on a fake chain: a constant-product curve with a
// phantom quote reserve, priced exactly like PonsV2BondingCurve (fees and tax
// on the quote leg, PonsV2BondingCurveMath.getAmountOut with fee 0), then
// optionally its graduated v4 pool.
type fakePons struct {
	f              *fakeChain
	token, curve   string
	quote          string // zero address = native ETH
	q, t           *big.Int
	feeBps, taxBps int64
	closed         uint64 // block of CurveCompleted (0 = open)
	poolID         string
	tokenIs0       bool // in the v4 pool
	n              int
}

func e18(x float64) *big.Int { return wei(x) }

// newFakePons: token (18 decimals, supply tokens) on curve, quoted in quote
// (decimals qDec) with a phantom reserve of phantom whole quote units.
// noCurveGetter: the token does not name its curve (found as a counterparty).
func newFakePons(f *fakeChain, token, curve, quote string, qDec int, phantom, supply float64, noCurveGetter bool) *fakePons {
	fp := &fakePons{f: f, token: token, curve: curve, quote: quote, feeBps: 100, taxBps: 200}
	fp.q = scaleUnits(phantom, qDec)
	fp.t = e18(supply)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addToken(token, 18, "PONS")
	if !noCurveGetter {
		f.constCall(token, selPonsCurve, ret(wAddr(curve)))
	}
	f.constCall(curve, selPonsFactory, ret(wAddr(defaultPonsFactory)))
	f.constCall(curve, selPonsToken, ret(wAddr(token)))
	f.constCall(curve, selPonsPairToken, ret(wAddr(quote)))
	f.calls[curve+"|"+selPonsGraduated] = func(b uint64) (string, bool) {
		if fp.closed > 0 && b >= fp.closed {
			return ret(wInt(1)), true
		}
		return ret(wInt(0)), true
	}
	return fp
}

func scaleUnits(x float64, dec int) *big.Int {
	v := new(big.Float).Mul(big.NewFloat(x), new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)))
	out, _ := v.Int(nil)
	return out
}

func (fp *fakePons) tx() string {
	fp.n++
	return fmt.Sprintf("0x%s%04x", fp.curve[len(fp.curve)-6:], fp.n)
}

// buy spends spend raw quote units at block; returns the curve price of the
// trade (raw quote per raw token).
func (fp *fakePons) buy(block uint64, spend *big.Int) (priceRaw float64) {
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	fee := new(big.Int).Div(new(big.Int).Mul(spend, big.NewInt(fp.feeBps)), big.NewInt(10_000))
	tax := new(big.Int).Div(new(big.Int).Mul(spend, big.NewInt(fp.taxBps)), big.NewInt(10_000))
	net := new(big.Int).Sub(new(big.Int).Sub(spend, fee), tax)
	out := new(big.Int).Div(new(big.Int).Mul(net, fp.t), new(big.Int).Add(fp.q, net)) // getAmountOut, fee 0
	fp.q.Add(fp.q, net)
	fp.t.Sub(fp.t, out)
	tx := fp.tx()
	fp.f.logs = append(fp.f.logs, fakeLog{addr: fp.curve, topics: []string{topicCurveBuy, addrTopic(ltBuyer), addrTopic(ltBuyer)},
		data: ret(w32(spend), w32(out), w32(fee), w32(tax)), block: block, tx: tx})
	fp.f.transfer(fp.token, fp.curve, ltBuyer, block, tx)
	return bigToFloat(net) / bigToFloat(out)
}

// sell sells tokensIn raw tokens; returns the curve price of the trade (raw units).
func (fp *fakePons) sell(block uint64, tokensIn *big.Int) (priceRaw float64) {
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	gross := new(big.Int).Div(new(big.Int).Mul(tokensIn, fp.q), new(big.Int).Add(fp.t, tokensIn))
	fee := new(big.Int).Div(new(big.Int).Mul(gross, big.NewInt(fp.feeBps)), big.NewInt(10_000))
	tax := new(big.Int).Div(new(big.Int).Mul(gross, big.NewInt(fp.taxBps)), big.NewInt(10_000))
	quoteOut := new(big.Int).Sub(new(big.Int).Sub(gross, fee), tax)
	fp.q.Sub(fp.q, gross)
	fp.t.Add(fp.t, tokensIn)
	tx := fp.tx()
	fp.f.logs = append(fp.f.logs, fakeLog{addr: fp.curve, topics: []string{topicCurveSell, addrTopic(ltBuyer), addrTopic(ltBuyer)},
		data: ret(w32(tokensIn), w32(quoteOut), w32(fee), w32(tax)), block: block, tx: tx})
	fp.f.transfer(fp.token, ltBuyer, fp.curve, block, tx)
	return bigToFloat(gross) / bigToFloat(tokensIn)
}

// spot: the curve's price now, raw quote per raw token.
func (fp *fakePons) spot() float64 { return bigToFloat(fp.q) / bigToFloat(fp.t) }

// close: graduate() — CurveCompleted on the curve, LaunchSwept on the factory.
func (fp *fakePons) close(block uint64) {
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	fp.closed = block
	tx := fp.tx()
	fp.f.logs = append(fp.f.logs,
		fakeLog{addr: fp.curve, topics: []string{topicCurveCompleted}, data: ret(wAddr(defaultPonsFactory), wInt(1), wInt(1)), block: block, tx: tx},
		fakeLog{addr: defaultPonsFactory, topics: []string{topicLaunchSwept, addrTopic(fp.token)}, data: ret(wInt(1), wInt(1)), block: block, tx: tx})
}

// v4ID is the v4 pool id the fake uses for a token and hook.
func v4ID(token, hook string) string { return "0x" + token[2:] + hook[len(hook)-24:] }

// graduate: createGraduatedPool() — Initialize of the v4 pool with hook, then
// PoolGraduated, in one transaction. A decoy pool of the same currencies with
// another hook is initialised in the same block.
func (fp *fakePons) graduate(block uint64, hook string) {
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	c0, c1 := fp.quote, fp.token
	if c1 < c0 {
		c0, c1 = c1, c0
	}
	fp.tokenIs0 = c0 == fp.token
	fp.poolID = v4ID(fp.token, hook)
	decoy := "0x00000000000000000000000000000000000000ee"
	tx := fp.tx()
	fp.f.logs = append(fp.f.logs,
		fakeLog{addr: tPM, topics: []string{topicInitV4, v4ID(fp.token, decoy), addrTopic(c0), addrTopic(c1)},
			data: ret(wInt(3000), wInt(60), wAddr(decoy), wInt(1), wInt(0)), block: block, tx: "0xdecoy" + tx[2:]},
		fakeLog{addr: tPM, topics: []string{topicInitV4, fp.poolID, addrTopic(c0), addrTopic(c1)},
			data: ret(wInt(0), wInt(200), wAddr(hook), wInt(1), wInt(0)), block: block, tx: tx},
		fakeLog{addr: defaultPonsFactory, topics: []string{topicPoolGraduated, addrTopic(fp.token)}, data: ret(wInt(7), wInt(1), wInt(1)), block: block, tx: tx})
}

// v4sqrt: sqrtPriceX96 for a price of p quote per token (decimal-adjusted).
func (fp *fakePons) v4sqrt(p float64, qDec int) *big.Int {
	raw := p * pow10(qDec) / 1e18 // quote per token, raw
	if !fp.tokenIs0 {
		raw = 1 / raw
	}
	return sqrtX96(raw)
}

// swap: a v4 swap that leaves the pool at p quote per token (deep pool).
func (fp *fakePons) swap(block uint64, p float64, qDec int) {
	fp.swapL(block, p, qDec, fakeLiq, 0)
}

func (fp *fakePons) swapL(block uint64, p float64, qDec int, liq *big.Int, tick int64) {
	sq := fp.v4sqrt(p, qDec)
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	tx := fp.tx()
	fp.f.swapV4L(tPM, fp.poolID, block, tx, sq, liq, tick)
	fp.f.transfer(fp.token, tPM, ltBuyer, block, tx)
}

// decoySwap: a swap in the other-hook pool (must never count).
func (fp *fakePons) decoySwap(block uint64) {
	fp.f.mu.Lock()
	defer fp.f.mu.Unlock()
	fp.f.swapV4L(tPM, v4ID(fp.token, "0x00000000000000000000000000000000000000ee"), block, fp.tx(), sqrtX96(1e-30), fakeLiq, 0)
}

// ---------------------------------------------------------------------------

// The event signatures must hash to the topics Bitquery shows for the deployed
// contracts. A mismatch would leave every Pons call silently without trades.
func TestPonsTopicPrefixes(t *testing.T) {
	for _, c := range []struct {
		name, topic, prefix string
	}{
		{"CurveBuy", topicCurveBuy, "0xec36bf57"},
		{"CurveSell", topicCurveSell, "0x8113d738"},
		{"LaunchSwept", topicLaunchSwept, "0xcdb72f15"},
		{"PoolGraduated", topicPoolGraduated, "0x0a44ef75"},
	} {
		if !strings.HasPrefix(c.topic, c.prefix) {
			t.Errorf("%s: got topic %s, want it to start with %s (Bitquery)", c.name, c.topic, c.prefix)
		}
	}
	// Derived from the published source (no outside reference): kept as they are.
	for got, want := range map[string]string{
		topicCurveCompleted: "0xf8d37a90738ae063b8b8058b66f5880cf3cf7ab0c5d4fa78219696591dfbfb67",
		topicTokenLaunched:  "0x8d4aad4953d0ca700d468f3753aa14432d1b35b43ec6409f051fb6aa43a89607",
		selPonsCurve:        "0x7165485d",
		selPonsToken:        "0xfc0c546a",
		selPonsFactory:      "0xc45a0155",
		selPonsPairToken:    "0x3de35b79",
		selPonsGraduated:    "0xe7c2b772",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// Curve trades are priced with the quote that moved along the curve: a buy's
// quoteIn minus fee and tax, a sell's quoteOut plus fee and tax. The liquidity
// is unknown, so no rug check can fire on a curve event.
func TestPonsCurvePrices(t *testing.T) {
	for _, c := range []struct {
		name    string
		qDec    int
		phantom float64
		spend   float64
	}{
		{"ETH", 18, 1.5, 0.3},
		{"USDG (6 decimals)", 6, 5000, 900},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 24*time.Hour)
			fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, zeroAddr, c.qDec, c.phantom, 1e9, false)
			st := &onchainState{Kind: "pons", Pool: tPonsCurve, Curve: tPonsCurve, TokenDec: 18, QuoteDec: c.qDec}
			scale := pow10(18 - c.qDec) // raw quote per raw token → quote per token
			before := fp.spot() * scale
			buyP := fp.buy(10, scaleUnits(c.spend, c.qDec)) * scale
			after := fp.spot() * scale
			sellP := fp.sell(11, e18(1e7)) * scale
			lb, ls := f.logs[0], f.logs[2]

			gotBuy := st.priceOfLog(rpcLog{Topics: lb.topics, Data: lb.data})
			if math.Abs(gotBuy-buyP)/buyP > 1e-12 || gotBuy <= before || gotBuy >= after {
				t.Fatalf("buy: got %.12g, want %.12g (between the spot before %.12g and after %.12g)", gotBuy, buyP, before, after)
			}
			gross := c.spend / bigToFloat(word(unhex(lb.data), 1)) * 1e18
			if math.Abs(gotBuy-gross)/gross < 0.02 {
				t.Fatalf("buy priced with the gross quoteIn (%.12g) instead of the net amount", gross)
			}
			gotSell := st.priceOfLog(rpcLog{Topics: ls.topics, Data: ls.data})
			if math.Abs(gotSell-sellP)/sellP > 1e-12 {
				t.Fatalf("sell: got %.12g, want %.12g", gotSell, sellP)
			}
			net := bigToFloat(word(unhex(ls.data), 1)) / pow10(c.qDec) / 1e7
			if math.Abs(gotSell-net)/net < 0.02 {
				t.Fatalf("sell priced with the net quoteOut (%.12g) instead of the gross amount", net)
			}
			for _, l := range []fakeLog{lb, ls} {
				ev := st.eventOfLog(rpcLog{Topics: l.topics, Data: l.data})
				if ev.liqKnown || ev.drained || ev.liqQ != 0 {
					t.Fatalf("curve event liquidity: got %+v, want unknown", ev)
				}
				if rug, liq := ev.rug(3000, 500); rug || liq != nil {
					t.Fatalf("curve event counted as a rug (%v, %v)", rug, liq)
				}
			}
			var prev [2]*big.Int
			if buy, vol, ok := st.tradeOfLog(rpcLog{Topics: lb.topics, Data: lb.data}, &prev); !ok || !buy || math.Abs(vol-c.spend)/c.spend > 1e-9 {
				t.Fatalf("buy trade: got %v %v %v, want buy of %v", buy, vol, ok, c.spend)
			}
			if buy, vol, ok := st.tradeOfLog(rpcLog{Topics: ls.topics, Data: ls.data}, &prev); !ok || buy || math.Abs(vol-sellP*1e7)/(sellP*1e7) > 1e-9 {
				t.Fatalf("sell trade: got %v %v %v, want sell of %v", buy, vol, ok, sellP*1e7)
			}
			fp.close(12)
			lc := f.logs[len(f.logs)-2]
			if p := st.priceOfLog(rpcLog{Topics: lc.topics, Data: lc.data}); p != 0 {
				t.Fatalf("CurveCompleted priced at %v", p)
			}
		})
	}
}

// A Pons token is found through its curve, quoted in ETH or an ERC-20, also
// when the token does not name its curve; a curve of another factory is not Pons.
func TestPonsDiscovery(t *testing.T) {
	for _, c := range []struct {
		name          string
		quote         string
		qDec          int
		noCurveGetter bool
		wantSym       string
	}{
		{"native ETH", zeroAddr, 18, false, "ETH"},
		{"USDG pair token", tUSDG, 6, false, "USDG"},
		{"curve found as a counterparty", zeroAddr, 18, true, "ETH"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 5*24*time.Hour)
			f.addToken(tUSDG, 6, "USDG")
			token := "0x4444444444444444444444444444444444444444"
			eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
			fp := newFakePons(f, token, tPonsCurve, c.quote, c.qDec, 1000, 1e9, c.noCurveGetter)
			fp.buy(eb-100, scaleUnits(1, c.qDec))
			fp.buy(eb+100, scaleUnits(1, c.qDec))
			o := testOnchain(t, f, nil)
			st, err := o.discover(context.Background(), token, eb)
			if err != nil {
				t.Fatal(err)
			}
			if st.Kind != "pons" || st.Pool != tPonsCurve || st.Curve != tPonsCurve || st.Token != token || st.Quote != c.quote ||
				st.QuoteSym != c.wantSym || st.QuoteDec != c.qDec || st.PonsDone != 0 || st.Hook != defaultPonsHook {
				t.Fatalf("got %+v, want a pons state on curve %s quoted in %s", st, tPonsCurve, c.wantSym)
			}
			if err := o.entryPrice(context.Background(), st, f.latest); err != nil || st.EntryPriceQ <= 0 || st.LastPriceBlock != eb-100 {
				t.Fatalf("entry: got %v at block %d (%v), want the buy at %d", st.EntryPriceQ, st.LastPriceBlock, err, eb-100)
			}
		})
	}
	t.Run("other factory", func(t *testing.T) {
		f := newFakeChain(t, 5*24*time.Hour)
		token := "0x4444444444444444444444444444444444444444"
		eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
		newFakePons(f, token, tPonsCurve, zeroAddr, 18, 1, 1e9, false).buy(eb-100, e18(0.1))
		f.constCall(tPonsCurve, selPonsFactory, ret(wAddr("0x00000000000000000000000000000000000000ab")))
		o := testOnchain(t, f, nil)
		if _, err := o.discover(context.Background(), token, eb); err != errNoPool {
			t.Fatalf("got %v, want errNoPool", err)
		}
	})
	t.Run("off", func(t *testing.T) {
		f := newFakeChain(t, 5*24*time.Hour)
		token := "0x4444444444444444444444444444444444444444"
		eb := f.blockAtTime(time.Now().Add(-24 * time.Hour))
		newFakePons(f, token, tPonsCurve, zeroAddr, 18, 1, 1e9, false).buy(eb-100, e18(0.1))
		o := testOnchain(t, f, map[string]string{"SCOUT_PONS_FACTORY": "off"})
		if _, err := o.discover(context.Background(), token, eb); err != errNoPool {
			t.Fatalf("SCOUT_PONS_FACTORY=off: got %v, want errNoPool", err)
		}
	})
}

// ponsScanFixture: a call on the curve, the curve closing 2 h after the call,
// the v4 pool created later (gap blocks after the close), and v4 swaps.
type ponsScanFixture struct {
	quote            string
	qDec             int
	f                *fakeChain
	fp               *fakePons
	eb, closeB, grad uint64
	entryP           float64 // last curve price before the call
	curvePeak        float64
	v4Peak, v4Last   float64
	curveAfter       int // curve trades after the call
	v4Count          int
}

// newPonsScanFixture: quoted in native ETH (the token is currency1 of the v4
// pool) or, with usdg, in USDG (6 decimals; the token 0x44… is currency0).
func newPonsScanFixture(t *testing.T, gap uint64, usdg bool) *ponsScanFixture {
	t.Helper()
	x := &ponsScanFixture{f: newFakeChain(t, 5*24*time.Hour), quote: zeroAddr, qDec: 18}
	f := x.f
	if usdg {
		f.addToken(tUSDG, 6, "USDG")
		x.quote, x.qDec = tUSDG, 6
	}
	q := func(v float64) *big.Int { return scaleUnits(v, x.qDec) }
	x.eb = f.blockAtTime(time.Now().Add(-3 * 24 * time.Hour))
	x.fp = newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, x.quote, x.qDec, 1.5, 1e9, false)
	fp := x.fp
	scale := pow10(18 - x.qDec) // raw prices → quote per token
	fp.buy(x.eb-500, q(0.2))
	x.entryP = fp.buy(x.eb-50, q(0.2)) * scale
	for i, b := range []uint64{x.eb + 300, x.eb + 9000, x.eb + 30000, x.eb + 60000} {
		p := fp.buy(b, q(0.5+float64(i))) * scale
		x.curvePeak = math.Max(x.curvePeak, p)
		x.curveAfter++
	}
	x.curveAfter++
	fp.sell(x.eb+65000, e18(1e6))
	x.closeB = x.eb + 72000                                           // 2 h after the call
	x.curvePeak = math.Max(x.curvePeak, fp.buy(x.closeB, q(1))*scale) // the crossing buy, same block as the close
	x.curveAfter++
	fp.close(x.closeB)
	x.grad = x.closeB + gap
	fp.graduate(x.grad, defaultPonsHook)
	fp.decoySwap(x.grad + 1)
	for i, p := range []float64{2e-8, 9e-8, 4e-8} {
		fp.swap(x.grad+uint64(i+1)*1000, p, x.qDec)
		x.v4Peak = math.Max(x.v4Peak, p)
		x.v4Last = p
		x.v4Count++
	}
	return x
}

// Called on the curve; the curve closes and the v4 pool is created later in
// the scan: one running state across the switch, every trade counted once, in
// block order, nothing from the decoy pool of the same currencies.
func TestPonsGraduationMidScan(t *testing.T) {
	for _, c := range []struct {
		gap  uint64 // pool in the closing block, soon after, much later
		usdg bool
	}{{0, false}, {5000, false}, {400_000, false}, {5000, true}} {
		gap := c.gap
		t.Run(fmt.Sprintf("pool %d blocks after the close, USDG %v", gap, c.usdg), func(t *testing.T) {
			x := newPonsScanFixture(t, gap, c.usdg)
			o := testOnchain(t, x.f, nil)
			ctx := context.Background()
			st, err := o.discover(ctx, x.fp.token, x.eb)
			if err != nil || st.Kind != "pons" {
				t.Fatalf("discover: %+v %v", st, err)
			}
			if err := o.entryPrice(ctx, st, x.f.latest); err != nil || math.Abs(st.EntryPriceQ-x.entryP)/x.entryP > 1e-12 {
				t.Fatalf("entry: got %v (%v), want %v", st.EntryPriceQ, err, x.entryP)
			}
			var blocks []uint64
			obs := func(b uint64, _ float64) { blocks = append(blocks, b) }
			// first to a block between the close and the pool (or right after it), then to the end
			mid := x.closeB + gap/2
			if err := o.scan(ctx, st, mid, obs); err != nil {
				t.Fatal(err)
			}
			if st.PonsDone != x.closeB {
				t.Fatalf("after the first scan: closing block %d, want %d", st.PonsDone, x.closeB)
			}
			if err := o.scan(ctx, st, x.grad+10_000, obs); err != nil {
				t.Fatal(err)
			}
			if st.PonsGrad != x.grad || st.PoolID != x.fp.poolID || st.TokenIs0 != x.fp.tokenIs0 || st.TokenIs0 != c.usdg || st.Hook != defaultPonsHook || st.Kind != "pons" {
				t.Fatalf("graduation: got %+v, want pool %s from block %d", st, x.fp.poolID, x.grad)
			}
			if want := x.curveAfter + x.v4Count; len(blocks) != want {
				t.Fatalf("got %d price events %v, want %d (%d curve + %d v4)", len(blocks), blocks, want, x.curveAfter, x.v4Count)
			}
			for i := 1; i < len(blocks); i++ {
				if blocks[i] < blocks[i-1] {
					t.Fatalf("events out of block order: %v", blocks)
				}
			}
			if math.Abs(st.LastPriceQ-x.v4Last)/x.v4Last > 1e-9 || math.Abs(st.RunMaxQ-math.Max(x.v4Peak, x.curvePeak))/st.RunMaxQ > 1e-9 {
				t.Fatalf("got last %v max %v, want last %v max %v", st.LastPriceQ, st.RunMaxQ, x.v4Last, math.Max(x.v4Peak, x.curvePeak))
			}
		})
	}
}

// Graduated before the call: a plain v4 state on the Pons pool, entry from the
// last v4 swap before the call (normal v4 rules from the start).
func TestPonsGraduatedBeforeCall(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "full node"}[full], func(t *testing.T) {
			x := newPonsScanFixture(t, 3000, false)
			x.f.fullNode = full
			call := x.grad + 1500 // after the first v4 swap
			o := testOnchain(t, x.f, nil)
			ctx := context.Background()
			st, err := o.discover(ctx, x.fp.token, call)
			if err != nil {
				t.Fatal(err)
			}
			if st.Kind != "v4" || st.Pool != tPM || st.PoolID != x.fp.poolID || st.PonsDone != x.closeB || st.PonsGrad != x.grad || st.Curve != tPonsCurve {
				t.Fatalf("got %+v, want v4 pool %s (closed %d, graduated %d)", st, x.fp.poolID, x.closeB, x.grad)
			}
			if err := o.entryPrice(ctx, st, x.f.latest); err != nil || math.Abs(st.EntryPriceQ-2e-8)/2e-8 > 1e-9 {
				t.Fatalf("entry: got %v (%v), want the v4 swap's 2e-8", st.EntryPriceQ, err)
			}
		})
	}
}

// A scan whose cursor is already past the graduation while the state does not
// know it (e.g. overwritten): graduated() says the curve closed, the closing
// block is found (bisection on an archive node, the event on a full node) and
// the v4 swaps after the cursor are read.
func TestPonsCursorPastGraduation(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "full node"}[full], func(t *testing.T) {
			x := newPonsScanFixture(t, 5000, false)
			x.f.fullNode = full
			late := x.grad + 50_000
			x.fp.swap(late, 7e-8, 18)
			o := testOnchain(t, x.f, nil)
			st := &onchainState{Kind: "pons", Pool: tPonsCurve, Curve: tPonsCurve, Token: x.fp.token, TokenDec: 18, Quote: zeroAddr,
				QuoteDec: 18, QuoteSym: "ETH", EntryBlock: x.eb, EntryPriceQ: x.entryP, LastPriceQ: x.entryP, ScanBlock: x.grad + 20_000, Hook: defaultPonsHook}
			if err := o.scan(context.Background(), st, x.f.latest, nil); err != nil {
				t.Fatal(err)
			}
			if st.PonsDone != x.closeB || st.PoolID != x.fp.poolID || math.Abs(st.LastPriceQ-7e-8)/7e-8 > 1e-9 || st.LastPriceBlock != late {
				t.Fatalf("got closed %d pool %s last %v at %d, want %d %s 7e-8 at %d", st.PonsDone, st.PoolID, st.LastPriceQ, st.LastPriceBlock,
					x.closeB, x.fp.poolID, late)
			}
		})
	}
}

// The curve closed but the pool was never created (swept, rescued): the price
// stays the last curve price, no error, and the search is not repeated from
// the start.
func TestPonsSweptWithoutPool(t *testing.T) {
	f := newFakeChain(t, 5*24*time.Hour)
	eb := f.blockAtTime(time.Now().Add(-3 * 24 * time.Hour))
	fp := newFakePons(f, "0x4444444444444444444444444444444444444444", tPonsCurve, zeroAddr, 18, 1.5, 1e9, false)
	fp.buy(eb-10, e18(0.2))
	last := fp.buy(eb+100, e18(3))
	fp.close(eb + 100)
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	st, err := o.discover(ctx, fp.token, eb)
	if err != nil || st.Kind != "pons" {
		t.Fatalf("discover: %+v %v", st, err)
	}
	if err := o.entryPrice(ctx, st, f.latest); err != nil {
		t.Fatal(err)
	}
	if err := o.scan(ctx, st, f.latest, nil); err != nil {
		t.Fatal(err)
	}
	if st.PonsDone != eb+100 || st.PoolID != "" || st.PonsSeen != f.latest || math.Abs(st.LastPriceQ-last)/last > 1e-12 || st.RugBlock != 0 {
		t.Fatalf("got %+v, want closed at %d, no pool, last price %v", st, eb+100, last)
	}
}
