package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Whether an address is a v2 pair or a v3 pool (resolveV2V3): "not a pool"
// only on the contract's own answer; a node that does not answer is an error.

const (
	poolCheckToken = "0x2323232323232323232323232323232323232323"
	poolCheckV2    = "0x00000000000000000000000000000000000000b2" // getReserves() answers, no slot0()
	poolCheckV3    = "0x00000000000000000000000000000000000000b3" // slot0() answers, no getReserves()
	poolCheckOther = "0x00000000000000000000000000000000000000b4" // token0()/token1() set by each case
)

// poolCheckChain: a fake chain with poolCheckToken / USDG as a v2 pair and as
// a v3 pool (the token is token0 of both).
func poolCheckChain(t *testing.T) *fakeChain {
	t.Helper()
	f := newFakeChain(t, 24*time.Hour)
	f.addToken(poolCheckToken, 18, "PCT")
	f.addToken(tUSDG, 6, "USDG")
	for _, p := range []string{poolCheckV2, poolCheckV3} {
		f.constCall(p, selToken0, ret(wAddr(poolCheckToken)))
		f.constCall(p, selToken1, ret(wAddr(tUSDG)))
	}
	f.constCall(poolCheckV2, selGetReserves, ret(wInt(1), wInt(1), wInt(0)))
	f.constCall(poolCheckV3, selSlot0, ret(wInt(1)))
	return f
}

// nodeFailure is one way the node fails to answer an eth_call: a JSON-RPC
// error (msg, code), or an answer outside JSON-RPC (answer: an HTTP status, a
// body that is not JSON, a dropped connection).
type nodeFailure struct {
	name   string
	msg    string
	code   int
	answer http.HandlerFunc
}

// httpStatus: the node answers with an HTTP error status.
func httpStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, http.StatusText(code), code) }
}

// notJSON: the node answers 200 with a body that is not JSON.
func notJSON(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "bad gateway: not JSON")
}

// dropConn: the node closes the connection without answering (a transport
// error for the client).
func dropConn(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err) // httptest's HTTP/1.1 server supports Hijack
	}
	conn.Close()
}

// poolCheckNodeFailures: answers that say nothing about the contract.
var poolCheckNodeFailures = []nodeFailure{
	{name: "rate limited (-32005)", msg: "rate limit exceeded", code: -32005},
	{name: "request timed out (-32002)", msg: "request timed out", code: -32002},
	{name: "over capacity (-32000)", msg: "server over capacity, try again", code: -32000},
	{name: "internal error (-32603)", msg: "internal error", code: -32603},
	// No state at latest: a pruned or mid-sync node (e.g. one behind a load
	// balancer), not the contract's answer.
	{name: "no state at latest", msg: "missing trie node 0xab (path ) state 0xcd is not available", code: -32000},
	{name: "no historical state at latest", msg: "historical state 0xcd is not available", code: -32000},
	{name: "HTTP 429", answer: httpStatus(http.StatusTooManyRequests)},
	{name: "HTTP 502", answer: httpStatus(http.StatusBadGateway)},
	{name: "HTTP 503", answer: httpStatus(http.StatusServiceUnavailable)},
	{name: "body not JSON", answer: notJSON},
	{name: "connection dropped", answer: dropConn},
}

// failCall makes the node fail every eth_call of sel on addr as fl says; the
// returned func makes it answer again.
func failCall(f *fakeChain, addr, sel string, fl nodeFailure) (stop func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	match := func(to, s string) bool { return to == addr && s == sel }
	f.callErrCode = fl.code
	if fl.answer != nil {
		f.callHTTP = func(to, s string) http.HandlerFunc {
			if match(to, s) {
				return fl.answer
			}
			return nil
		}
	} else {
		f.callErr = func(to, s string, _ uint64) string {
			if match(to, s) {
				return fl.msg
			}
			return ""
		}
	}
	return func() {
		f.mu.Lock()
		f.callHTTP, f.callErr, f.callErrCode = nil, nil, 0
		f.mu.Unlock()
	}
}

// getterName: the getter's name as resolveV2V3's errors say it.
func getterName(sel string) string {
	if sel == selGetReserves {
		return "getReserves()"
	}
	return "slot0()"
}

// getReserves() or slot0() not answered by the node (rate limit, time-out,
// busy node, no state at latest, HTTP 429 / 5xx, a body that is not JSON, a
// dropped connection): an error naming the call, never "not a pool";
// once the node answers, the same address is found as before.
func TestResolveV2V3NodeErrors(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, pool, sel, wantKind string
	}{
		{"v2 pair, getReserves()", poolCheckV2, selGetReserves, "v2"},
		{"v3 pool, getReserves()", poolCheckV3, selGetReserves, "v3"},
		{"v3 pool, slot0()", poolCheckV3, selSlot0, "v3"},
	} {
		for _, fl := range poolCheckNodeFailures {
			t.Run(c.name+", "+fl.name, func(t *testing.T) {
				f := poolCheckChain(t)
				o := testOnchain(t, f, nil)
				stop := failCall(f, c.pool, c.sel, fl)
				st := &onchainState{}
				ok, err := o.resolveV2V3(ctx, st, poolCheckToken, c.pool)
				if ok || err == nil || isRevert(err) || errors.Is(err, errEmptyResult) {
					t.Fatalf("resolveV2V3(%s) with %s failing: got ok=%v err=%v, want the node's error", c.pool, getterName(c.sel), ok, err)
				}
				if !strings.Contains(err.Error(), getterName(c.sel)) {
					t.Fatalf("error %q does not name %s", err, getterName(c.sel))
				}
				stop()
				st = &onchainState{}
				ok, err = o.resolveV2V3(ctx, st, poolCheckToken, c.pool)
				if err != nil || !ok || st.Kind != c.wantKind || st.Pool != c.pool || !st.TokenIs0 || st.Quote != tUSDG {
					t.Fatalf("resolveV2V3(%s) once the node answers: got ok=%v err=%v %+v, want a %s pool against USDG", c.pool, ok, err, st, c.wantKind)
				}
			})
		}
	}
}

// token0() or token1() not answered by the node (the same failures, no state
// at latest included): an error, never "not a pool"; once the node answers,
// the pair is found.
func TestResolveV2V3TokenCallNodeErrors(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	for _, c := range []struct{ name, sel string }{{"token0()", selToken0}, {"token1()", selToken1}} {
		for _, fl := range poolCheckNodeFailures {
			t.Run(c.name+", "+fl.name, func(t *testing.T) {
				f := poolCheckChain(t)
				o := testOnchain(t, f, nil)
				stop := failCall(f, poolCheckV2, c.sel, fl)
				st := &onchainState{}
				ok, err := o.resolveV2V3(ctx, st, poolCheckToken, poolCheckV2)
				if ok || err == nil || isRevert(err) || errors.Is(err, errEmptyResult) {
					t.Fatalf("resolveV2V3 with %s failing: got ok=%v err=%v, want the node's error", c.name, ok, err)
				}
				stop()
				st = &onchainState{}
				ok, err = o.resolveV2V3(ctx, st, poolCheckToken, poolCheckV2)
				if err != nil || !ok || st.Kind != "v2" || st.Pool != poolCheckV2 {
					t.Fatalf("resolveV2V3 once the node answers: got ok=%v err=%v %+v, want the v2 pair", ok, err, st)
				}
			})
		}
	}
}

// The contract's own answers decide as before: a getter that answers is a
// pool; reverts (any reason, any code), empty results and answers too short
// to read are "not a pool" with no error.
func TestResolveV2V3DefinitiveAnswers(t *testing.T) {
	ctx := context.Background()
	pair := func(f *fakeChain) {
		f.constCall(poolCheckOther, selToken0, ret(wAddr(poolCheckToken)))
		f.constCall(poolCheckOther, selToken1, ret(wAddr(tUSDG)))
	}
	getterErr := func(f *fakeChain, msg string, code int) {
		f.callErrCode = code
		f.callErr = func(to, sel string, _ uint64) string {
			if to == poolCheckOther && (sel == selGetReserves || sel == selSlot0) {
				return msg
			}
			return ""
		}
	}
	for _, c := range []struct {
		name     string
		setup    func(f *fakeChain)
		wantKind string // "" = not a pool
	}{
		{"getReserves() answers: v2", func(f *fakeChain) {
			pair(f)
			f.constCall(poolCheckOther, selGetReserves, ret(wInt(1), wInt(1), wInt(0)))
		}, "v2"},
		{"getReserves() reverts, slot0() answers: v3", func(f *fakeChain) {
			pair(f)
			f.constCall(poolCheckOther, selSlot0, ret(wInt(1)))
		}, "v3"},
		{"both answer: v2 (getReserves() first)", func(f *fakeChain) {
			pair(f)
			f.constCall(poolCheckOther, selGetReserves, ret(wInt(1), wInt(1), wInt(0)))
			f.constCall(poolCheckOther, selSlot0, ret(wInt(1)))
		}, "v2"},
		{"both revert", pair, ""},
		{"both revert with code 3", func(f *fakeChain) { pair(f); getterErr(f, "execution reverted", 3) }, ""},
		{"revert reason reads like a busy node", func(f *fakeChain) { pair(f); getterErr(f, "execution reverted: rate limit", 3) }, ""},
		{"invalid opcode", func(f *fakeChain) { pair(f); getterErr(f, "invalid opcode: INVALID", -32000) }, ""},
		{"both empty (0x)", func(f *fakeChain) {
			pair(f)
			f.constCall(poolCheckOther, selGetReserves, "0x")
			f.constCall(poolCheckOther, selSlot0, "0x")
		}, ""},
		{"too short to read", func(f *fakeChain) {
			pair(f)
			f.constCall(poolCheckOther, selGetReserves, ret(wInt(1)))
			f.constCall(poolCheckOther, selSlot0, "0x01")
		}, ""},
		{"no token0() (a wallet: empty result)", func(f *fakeChain) { f.constCall(poolCheckOther, selToken0, "0x") }, ""},
		{"no token0() (reverts)", func(f *fakeChain) {}, ""},
		{"a pool of other tokens", func(f *fakeChain) {
			f.constCall(poolCheckOther, selToken0, ret(wAddr(tWETH)))
			f.constCall(poolCheckOther, selToken1, ret(wAddr(tUSDG)))
			f.constCall(poolCheckOther, selSlot0, ret(wInt(1)))
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := poolCheckChain(t)
			f.mu.Lock()
			c.setup(f)
			f.mu.Unlock()
			o := testOnchain(t, f, nil)
			st := &onchainState{}
			ok, err := o.resolveV2V3(ctx, st, poolCheckToken, poolCheckOther)
			if err != nil || ok != (c.wantKind != "") || (ok && st.Kind != c.wantKind) {
				t.Fatalf("resolveV2V3: got ok=%v kind %q err=%v, want ok=%v kind %q and no error", ok, st.Kind, err, c.wantKind != "", c.wantKind)
			}
		})
	}
}

// Pool discovery (the tracker's discover): the pair's getReserves() not
// answered is the node's error, not errNoPool (which the tracker keeps as
// no_pool for 6 hours); once the node answers, the pair is found.
func TestDiscoverPoolCheckNodeError(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	for _, fl := range poolCheckNodeFailures {
		t.Run(fl.name, func(t *testing.T) {
			f := poolCheckChain(t)
			eb := f.blockAtTime(time.Now().Add(-2 * time.Hour))
			f.transfer(poolCheckToken, poolCheckV2, "0x00000000000000000000000000000000000000d1", eb-10, "0x1")
			o := testOnchain(t, f, nil)
			stop := failCall(f, poolCheckV2, selGetReserves, fl)
			if st, err := o.discover(ctx, poolCheckToken, eb); err == nil || errors.Is(err, errNoPool) {
				t.Fatalf("discover with getReserves() failing: got %+v %v, want the node's error", st, err)
			}
			stop()
			st, err := o.discover(ctx, poolCheckToken, eb)
			if err != nil || st.Kind != "v2" || st.Pool != poolCheckV2 || st.QuoteSym != "USDG" {
				t.Fatalf("discover once the node answers: got %+v %v, want the v2 pair", st, err)
			}
		})
	}
}

// The near-block USD-pool search (discoverUSDPools): the pool's slot0() not
// answered is the node's error, and nothing is kept for the asset (no
// "searched here", no "no pool"), so the next call searches again and prices
// the block once the node answers.
func TestQuoteUSDNearPoolCheckNodeError(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	const asset, pool = "0x2424242424242424242424242424242424242424", "0x00000000000000000000000000000000000000b5"
	for _, fl := range poolCheckNodeFailures {
		t.Run(fl.name, func(t *testing.T) {
			f := usdChain(t)
			f.addToken(asset, 18, "NEAR")
			block := f.latest - 100_000
			newUSDTestPool(f, pool, asset, tUSDG, 18, 6).trade(block-100, 2)
			o := testOnchain(t, f, map[string]string{"SCOUT_DISCOVERY_BLOCKS": "1000"})
			stop := failCall(f, pool, selSlot0, fl)
			p, ok, err := o.quoteUSD(ctx, asset, block)
			if err == nil || ok || errors.Is(err, errUSDSearchPending) || errors.Is(err, errNoPriceYet) || errors.Is(err, errNoUSDSource) {
				t.Fatalf("quoteUSD with slot0() failing: got %v ok=%v err=%v, want the node's error", p, ok, err)
			}
			o.mu.Lock()
			set := o.usdPools[asset]
			kept := len(set.anchors) != 0 || len(set.pools) != 0 || !set.noPoolUntil.IsZero() || len(set.histSeen) != 0
			o.mu.Unlock()
			if kept {
				t.Fatalf("after the failed check: the asset's set keeps %+v, want nothing kept", set)
			}
			stop()
			p, ok, err = o.quoteUSD(ctx, asset, block)
			if err != nil || !ok || !near6(p, 2) {
				t.Fatalf("quoteUSD once the node answers: got %v ok=%v err=%v, want $2", p, ok, err)
			}
		})
	}
}
