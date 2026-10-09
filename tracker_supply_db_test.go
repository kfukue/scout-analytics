package main

import (
	"context"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWholeTokens(t *testing.T) {
	big10 := func(s string) *big.Int {
		t.Helper()
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			t.Fatalf("bad number %q", s)
		}
		return v
	}
	for _, c := range []struct {
		raw  string
		dec  int
		want string
	}{
		{"1000000000000000000000000000", 18, "1000000000"},
		{"123456789", 6, "123.456789"},
		{"1230000", 6, "1.23"},
		{"5", 18, "0.000000000000000005"},
		{"0", 18, "0"},
		{"42", 0, "42"},
		{"115792089237316195423570985008687907853269984665640564039457584007913129639935", 36,
			"115792089237316195423570985008687907853269.984665640564039457584007913129639935"},
	} {
		if got := wholeTokens(big10(c.raw), c.dec); got != c.want {
			t.Errorf("wholeTokens(%s, %d) = %s, want %s", c.raw, c.dec, got, c.want)
		}
	}
}

// supplyFixture is a database with calls of several tokens, and a scanner
// factory on a fake chain, for the supply fill pass.
type supplyFixture struct {
	st      *ScoutStore
	f       *fakeChain
	ids     map[string]int
	newScan func() *scanner
	post    func(key string, msgID int, ca string)
}

func newSupplyFixture(t *testing.T, f *fakeChain) *supplyFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	fx := &supplyFixture{st: st, f: f, ids: map[string]int{}}
	fx.newScan = func() *scanner {
		s := newScanner(&config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true, "solana": true}, StateDir: t.TempDir(), Price: pc})
		s.db = st
		s.sourceChannelID = 777
		return s
	}
	s := fx.newScan()
	now := time.Now()
	fx.post = func(key string, msgID int, ca string) {
		t.Helper()
		id, _ := s.recordCallInfo(context.Background(), postAt(msgID, now.Add(-time.Duration(100-msgID)*time.Hour), ca), ca, nil, CallStatusBackfill)
		if id == nil {
			t.Fatalf("call %s not recorded", key)
		}
		fx.ids[key] = *id
	}
	return fx
}

// track gives a call an entry price (USD) and an on-chain state with its entry
// block, plus a status, schedule and updated_at the fill pass must not touch.
func (fx *supplyFixture) track(t *testing.T, key string, entryBlock uint64) {
	t.Helper()
	if _, err := fx.st.Pool.Exec(context.Background(), `UPDATE scout_call_tracking SET status = 'done', attempts = 3,
		price_unit = 'usd', entry_price_usd = 0.001, next_check_at = '2031-01-02T03:04:05Z',
		onchain = jsonb_build_object('v', 2, 'kind', 'v3', 'scan_block', 123, 'entry_block', $2::bigint),
		updated_at = '2026-01-01T00:00:00Z' WHERE call_id = $1`, fx.ids[key], int64(entryBlock)); err != nil {
		t.Fatal(err)
	}
}

type supplyRow struct {
	Supply *string
	Block  *int64
}

func (fx *supplyFixture) supply(t *testing.T, key string) supplyRow {
	t.Helper()
	var r supplyRow
	if err := fx.st.Pool.QueryRow(context.Background(), `SELECT token_supply::text, token_supply_block FROM scout_call_tracking WHERE call_id = $1`,
		fx.ids[key]).Scan(&r.Supply, &r.Block); err != nil {
		t.Fatal(err)
	}
	return r
}

func (fx *supplyFixture) wantSupply(t *testing.T, key string, supply *string, block *int64) {
	t.Helper()
	got := fx.supply(t, key)
	if strOrNil(got.Supply) != strOrNil(supply) || (got.Block == nil) != (block == nil) || (block != nil && *got.Block != *block) {
		t.Errorf("%s: supply %v block %v, want %v %v", key, strOrNil(got.Supply), i64OrNil(got.Block), strOrNil(supply), i64OrNil(block))
	}
}

func i64OrNil(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func i64p(v int64) *int64 { return &v }

type trackSnap struct {
	Status, Next, Onchain, Updated string
	Attempts                       int
}

// trackSnapshot: what the fill pass must leave alone, per call.
func (fx *supplyFixture) trackSnapshot(t *testing.T) map[int]trackSnap {
	t.Helper()
	rows, err := fx.st.Pool.Query(context.Background(), `SELECT call_id, status, COALESCE(next_check_at::text, ''), COALESCE(onchain::text, ''),
		updated_at::text, attempts FROM scout_call_tracking`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]trackSnap{}
	for rows.Next() {
		var id int
		var x trackSnap
		if err := rows.Scan(&id, &x.Status, &x.Next, &x.Onchain, &x.Updated, &x.Attempts); err != nil {
			t.Fatal(err)
		}
		out[id] = x
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// callTagsSince returns the block tags of the eth_calls the chain received
// after the first before ones.
func callTagsSince(f *fakeChain, before int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.callTags[before:]...)
}

// TestFillTokenSupplyArchive: on a node with historical state, the supply is
// read at the entry block of the token's earliest call, divided exactly by
// its decimals; a contract without totalSupply() (a revert, though the node
// has the state) or with absurd decimals gets NULL with the block set and is
// not asked again, without marking the node as having no state; a non-EVM
// address gets NULL at block 0 without asking the node; a call without an
// entry price is left for later. Repeat calls are filled from the database.
// Nothing else of the rows changes, and nothing is tracked again.
func TestFillTokenSupplyArchive(t *testing.T) {
	f := newFakeChain(t, 24*time.Hour)
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokA := "0x44444444444444444444444444444444444444ab"
	tokAMixed := "0x44444444444444444444444444444444444444AB"
	tokB := "0x5555555555555555555555555555555555555555" // 6 decimals, a fractional supply
	tokC := "0x6666666666666666666666666666666666666666" // no totalSupply()
	tokD := "0x7777777777777777777777777777777777777777" // 40 decimals
	tokE := "0x8888888888888888888888888888888888888888" // no entry price yet
	sol := "So11111111111111111111111111111111111111112x"

	f.addToken(tokA, 18, "ALPHA")
	f.calls[strings.ToLower(tokA)+"|"+selTotalSupply] = func(block uint64) (string, bool) {
		if block < 500 { // minted more later: the entry block's supply is the smaller one
			return ret(w32(new(big.Int).Mul(big.NewInt(1e9), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))), true
		}
		return ret(w32(new(big.Int).Mul(big.NewInt(3e9), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))), true
	}
	f.addToken(tokB, 6, "BETA")
	f.constCall(tokB, selTotalSupply, ret(wInt(123456789)))
	f.addToken(tokC, 18, "NOSUP")
	f.addToken(tokD, 40, "HUGE")
	f.constCall(tokD, selTotalSupply, ret(wInt(1)))
	f.addToken(tokE, 18, "LATER")
	f.constCall(tokE, selTotalSupply, ret(wInt(1)))

	fx.post("a1", 1, tokA) // earliest post of A: its entry block is the one read
	fx.post("a2", 2, tokAMixed)
	fx.post("b", 3, tokB)
	fx.post("c", 4, tokC)
	fx.post("d", 5, tokD)
	fx.post("e", 6, tokE)
	fx.post("sol", 7, sol)
	fx.track(t, "a1", 300)
	fx.track(t, "a2", 900)
	fx.track(t, "b", 400)
	fx.track(t, "c", 410)
	fx.track(t, "d", 420)
	fx.track(t, "sol", 0)
	// e: tracked, but no entry price yet
	if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'done', next_check_at = '2031-01-02T03:04:05Z',
		onchain = '{"v": 2, "entry_block": 430}'::jsonb WHERE call_id = $1`, fx.ids["e"]); err != nil {
		t.Fatal(err)
	}
	before := fx.trackSnapshot(t)

	s := fx.newScan()
	tags0 := len(callTagsSince(f, 0))
	if n := s.fillTokenSupply(ctx); n != 5 {
		t.Fatalf("looked up %d tokens, want 5 (A, B, C, D, sol)", n)
	}
	if s.onchain.noState.Load() {
		t.Fatal("a reverting totalSupply() on an archive node marked the node as having no state")
	}
	fx.wantSupply(t, "a1", sp("1000000000"), i64p(300))
	fx.wantSupply(t, "a2", sp("1000000000"), i64p(300)) // every call of the contract, from the first call's block
	fx.wantSupply(t, "b", sp("123.456789"), i64p(400))
	fx.wantSupply(t, "c", nil, i64p(410))
	fx.wantSupply(t, "d", nil, i64p(int64(f.latest))) // decimals unusable: no supply, block = head
	fx.wantSupply(t, "sol", nil, i64p(0))
	fx.wantSupply(t, "e", nil, nil)
	// totalSupply() was read at the entry blocks of A, B and C (D's decimals
	// are unusable, sol is not asked); decimals() at latest
	var historical []string
	for _, tag := range callTagsSince(f, tags0) {
		if tag != "latest" {
			historical = append(historical, tag)
		}
	}
	if want := []string{hexU64(410), hexU64(400), hexU64(300)}; strings.Join(historical, ",") != strings.Join(want, ",") {
		t.Errorf("eth_calls at past blocks %v, want %v (C, B, A: newest call first)", historical, want)
	}
	after := fx.trackSnapshot(t)
	for id, b := range before {
		if after[id] != b {
			t.Fatalf("call %d changed: %+v → %+v", id, b, after[id])
		}
	}

	// A second pass and a new process ask nothing again.
	calls := f.count["eth_call"]
	for i, sc := range []*scanner{s, fx.newScan()} {
		if n := sc.fillTokenSupply(ctx); n != 0 {
			t.Fatalf("pass %d looked up %d tokens again", i+2, n)
		}
	}
	if f.count["eth_call"] != calls {
		t.Fatalf("eth_calls %d → %d on the repeat passes", calls, f.count["eth_call"])
	}
	// Nothing became due, and the migration does not queue anything again.
	if err := fx.st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("%d call(s) tracked again", n)
	}
	if now := fx.trackSnapshot(t); len(now) != len(before) {
		t.Fatalf("%d tracking rows, want %d", len(now), len(before))
	} else {
		for id, b := range before {
			if now[id] != b {
				t.Fatalf("call %d changed after migrate: %+v → %+v", id, b, now[id])
			}
		}
	}

	// A later call of a known token is copied in the database, without the node.
	fx.post("a3", 8, tokAMixed)
	fx.track(t, "a3", 2000)
	fx.wantSupply(t, "a3", nil, nil)
	fx.newScan().fillTokenSupply(ctx)
	fx.wantSupply(t, "a3", sp("1000000000"), i64p(300))
	if f.count["eth_call"] != calls {
		t.Fatalf("repeat call asked the node: eth_calls %d → %d", calls, f.count["eth_call"])
	}

	// The entry price arrives for e: it is looked up on the next pass.
	if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET price_unit = 'usd', entry_price_usd = 2 WHERE call_id = $1`, fx.ids["e"]); err != nil {
		t.Fatal(err)
	}
	if n := fx.newScan().fillTokenSupply(ctx); n != 1 {
		t.Fatalf("looked up %d tokens, want 1 (e)", n)
	}
	fx.wantSupply(t, "e", sp("0.000000000000000001"), i64p(430))

	// Off for the GeckoTerminal source.
	if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET token_supply = NULL, token_supply_block = NULL`); err != nil {
		t.Fatal(err)
	}
	g := fx.newScan()
	g.pc.Source = "gecko"
	calls = f.count["eth_call"]
	if n := g.fillTokenSupply(ctx); n != 0 || f.count["eth_call"] != calls {
		t.Fatalf("gecko source looked up %d tokens", n)
	}
	fx.wantSupply(t, "a1", nil, nil)
}

// TestFillTokenSupplyFullNode: a node without historical state ("missing trie
// node" at the entry block, confirmed by the state probe) gets the latest
// supply read, with the head block stored; the next (older) tokens are read at
// latest straight away. The price reads' noState flag is left alone.
func TestFillTokenSupplyFullNode(t *testing.T) {
	f := newFakeChain(t, 24*time.Hour)
	f.fullNode = true
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokA := "0x44444444444444444444444444444444444444ab"
	tokB := "0x5555555555555555555555555555555555555555"
	tokC := "0x6666666666666666666666666666666666666666" // no totalSupply()
	f.addToken(tokA, 18, "ALPHA")
	f.constCall(tokA, selTotalSupply, ret(w32(new(big.Int).Mul(big.NewInt(7), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))))
	f.addToken(tokB, 9, "BETA")
	f.constCall(tokB, selTotalSupply, ret(wInt(2500000000)))
	f.addToken(tokC, 18, "NOSUP")
	fx.post("a", 1, tokA)
	fx.post("b", 2, tokB)
	fx.post("c", 3, tokC)
	fx.track(t, "a", 300)
	fx.track(t, "b", 310)
	fx.track(t, "c", 320)
	before := fx.trackSnapshot(t)

	s := fx.newScan()
	tags0 := len(callTagsSince(f, 0))
	if n := s.fillTokenSupply(ctx); n != 3 {
		t.Fatalf("looked up %d tokens, want 3", n)
	}
	if s.onchain.noState.Load() {
		t.Fatal("the supply pass set the price reads' noState flag")
	}
	head := int64(f.latest)
	fx.wantSupply(t, "a", sp("7"), &head)
	fx.wantSupply(t, "b", sp("2.5"), &head)
	fx.wantSupply(t, "c", nil, &head)
	// Only the first token (the newest call: c) tried its entry block.
	historical := 0
	for _, tag := range callTagsSince(f, tags0) {
		if tag != "latest" {
			historical++
		}
	}
	if historical != 1 {
		t.Errorf("%d eth_call(s) at a past block, want 1 (the first token's entry block only)", historical)
	}
	after := fx.trackSnapshot(t)
	for id, b := range before {
		if after[id] != b {
			t.Fatalf("call %d changed: %+v → %+v", id, b, after[id])
		}
	}
}

// TestFillTokenSupplyTransient: a node that does not answer (busy, rate
// limited) ends the pass: nothing is written for that token or the ones
// after it, and the node is not marked as having no state.
func TestFillTokenSupplyTransient(t *testing.T) {
	oldBase := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = oldBase })
	f := newFakeChain(t, 24*time.Hour)
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokA := "0x44444444444444444444444444444444444444ab"
	tokB := "0x5555555555555555555555555555555555555555"
	f.addToken(tokA, 18, "ALPHA")
	f.constCall(tokA, selTotalSupply, ret(wInt(1)))
	f.addToken(tokB, 18, "BETA")
	f.constCall(tokB, selTotalSupply, ret(wInt(1)))
	busy := true
	f.balanceErr = "rate limit exceeded, try again later" // the liveness request fails too
	f.callErr = func(to, sel string, block uint64) string {
		if busy && sel == selTotalSupply {
			return "rate limit exceeded, try again later"
		}
		return ""
	}
	fx.post("a", 1, tokA)
	fx.post("b", 2, tokB)
	fx.track(t, "a", 300)
	fx.track(t, "b", 310)

	s := fx.newScan()
	if n := s.fillTokenSupply(ctx); n != 0 {
		t.Fatalf("looked up %d tokens with the node busy, want 0", n)
	}
	if s.onchain.noState.Load() {
		t.Fatal("a busy node marked as having no state")
	}
	fx.wantSupply(t, "a", nil, nil)
	fx.wantSupply(t, "b", nil, nil)
	if got := len(s.supplyStrikes); got != 0 {
		t.Errorf("got %d tokens with strikes after a pass with the node busy, want 0 (%v)", got, s.supplyStrikes)
	}

	// The node answers again: the next pass fills both.
	f.mu.Lock()
	busy = false
	f.balanceErr = ""
	f.mu.Unlock()
	if n := s.fillTokenSupply(ctx); n != 2 {
		t.Fatalf("looked up %d tokens, want 2", n)
	}
	fx.wantSupply(t, "a", sp("0.000000000000000001"), i64p(300))
	fx.wantSupply(t, "b", sp("0.000000000000000001"), i64p(310))
}

// fakeCount returns how many requests of a method the chain received.
func fakeCount(f *fakeChain, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count[method]
}

// TestFillTokenSupplyRecentState: a full node that keeps only recent state.
// An old token falls back to the latest supply (head block stored), a recent
// token is still read at its entry block, a second old token does not try its
// entry block again, the head is read once per pass, and the price reads'
// noState flag stays false (no switch to event logs).
func TestFillTokenSupplyRecentState(t *testing.T) {
	oldTTL := headCacheTTL
	headCacheTTL = 0 // every blockNumber asks the node: the pass itself must read it once
	t.Cleanup(func() { headCacheTTL = oldTTL })
	f := newFakeChain(t, 24*time.Hour)
	f.fullNode = true
	f.stateFrom = f.latest - 1000
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokOld1 := "0x44444444444444444444444444444444444444ab"
	tokRecent := "0x5555555555555555555555555555555555555555"
	tokOld2 := "0x6666666666666666666666666666666666666666"
	recentBlock := f.latest - 10
	for _, ca := range []string{tokOld1, tokRecent, tokOld2} {
		f.addToken(ca, 0, "T")
		f.calls[strings.ToLower(ca)+"|"+selTotalSupply] = func(block uint64) (string, bool) {
			if block == f.latest {
				return ret(wInt(9)), true // the latest supply
			}
			return ret(wInt(5)), true // the supply at a past block
		}
	}
	// processed newest call first: old1, recent, old2
	fx.post("old2", 1, tokOld2)
	fx.post("recent", 2, tokRecent)
	fx.post("old1", 3, tokOld1)
	fx.track(t, "old2", 200)
	fx.track(t, "recent", recentBlock)
	fx.track(t, "old1", 300)

	s := fx.newScan()
	tags0 := len(callTagsSince(f, 0))
	heads0 := fakeCount(f, "eth_blockNumber")
	if n := s.fillTokenSupply(ctx); n != 3 {
		t.Fatalf("looked up %d tokens, want 3", n)
	}
	if s.onchain.noState.Load() {
		t.Fatal("the supply pass set the price reads' noState flag")
	}
	head := int64(f.latest)
	fx.wantSupply(t, "old1", sp("9"), &head)
	fx.wantSupply(t, "recent", sp("5"), i64p(int64(recentBlock)))
	fx.wantSupply(t, "old2", sp("9"), &head)
	var historical []string
	for _, tag := range callTagsSince(f, tags0) {
		if tag != "latest" {
			historical = append(historical, tag)
		}
	}
	if want := []string{hexU64(300), hexU64(recentBlock)}; strings.Join(historical, ",") != strings.Join(want, ",") {
		t.Errorf("eth_calls at past blocks %v, want %v (old1's entry, recent's entry; old2 skips its entry block)", historical, want)
	}
	if got := fakeCount(f, "eth_blockNumber") - heads0; got != 1 {
		t.Errorf("got %d eth_blockNumber in one pass with two tokens read at latest, want 1", got)
	}
}

// TestContractsMissingTokenSupplyEntryBlock: the entry block used is the one
// of the token's earliest call that has a usable entry block, not the
// earliest priced call when that one has none (e.g. GeckoTerminal); a token
// with no usable entry block at all ("0", text, missing) is read at latest.
func TestContractsMissingTokenSupplyEntryBlock(t *testing.T) {
	f := newFakeChain(t, 24*time.Hour)
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokA := "0x44444444444444444444444444444444444444ab"
	tokB := "0x5555555555555555555555555555555555555555"
	for _, ca := range []string{tokA, tokB} {
		f.addToken(ca, 0, "T")
		f.calls[strings.ToLower(ca)+"|"+selTotalSupply] = func(block uint64) (string, bool) { return ret(wInt(int64(block))), true }
	}
	fx.post("a-gecko", 1, tokA) // earliest call of A, priced without an entry block
	fx.post("a-zero", 2, tokA)  // entry block 0: not usable
	fx.post("a-chain", 3, tokA) // the first usable entry block
	fx.post("a-late", 4, tokA)
	fx.post("b-text", 5, tokB)
	fx.post("b-zero", 6, tokB)
	fx.track(t, "a-chain", 500)
	fx.track(t, "a-late", 900)
	fx.track(t, "a-zero", 0)
	fx.track(t, "b-zero", 0)
	for key, onchain := range map[string]string{"a-gecko": `{"v": 2}`, "b-text": `{"v": 2, "entry_block": "x12"}`} {
		if _, err := fx.st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET price_unit = 'usd', entry_price_usd = 0.5,
			onchain = $2::jsonb WHERE call_id = $1`, fx.ids[key], onchain); err != nil {
			t.Fatal(err)
		}
	}
	got, err := fx.st.ContractsMissingTokenSupply(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{tokA: 500, tokB: 0}
	if len(got) != len(want) {
		t.Fatalf("got %d targets %+v, want %d", len(got), got, len(want))
	}
	for _, g := range got {
		if w, ok := want[strings.ToLower(g.CA)]; !ok || g.EntryBlock != w {
			t.Errorf("target %s: got entry block %d, want %d", g.CA, g.EntryBlock, w)
		}
	}
	if n := fx.newScan().fillTokenSupply(ctx); n != 2 {
		t.Fatalf("looked up %d tokens, want 2", n)
	}
	fx.wantSupply(t, "a-gecko", sp("500"), i64p(500))
	fx.wantSupply(t, "b-text", sp(strconv.FormatUint(f.latest, 10)), i64p(int64(f.latest)))
}

// TestFillTokenSupplyGivesUp: a token whose totalSupply() keeps failing with a
// text that looks like a busy node ("limit") is skipped, not a stall: the
// tokens after it are filled, and after tokenSupplyMaxStrikes passes in which
// the node otherwise answered (a later token, or the liveness request at the
// end of the pass when it is the last or only token) it gets NULL at block 0
// and is not asked again. A pass in which the node answers nothing (really
// busy) is not counted.
func TestFillTokenSupplyGivesUp(t *testing.T) {
	oldBase := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = oldBase })
	f := newFakeChain(t, 24*time.Hour)
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	tokX := "0x44444444444444444444444444444444444444ab" // reverts with "mint limit exceeded"
	tokY := "0x5555555555555555555555555555555555555555"
	f.addToken(tokX, 0, "X")
	f.constCall(tokX, selTotalSupply, ret(wInt(1)))
	f.addToken(tokY, 0, "Y")
	f.constCall(tokY, selTotalSupply, ret(wInt(7)))
	allBusy := false // read by callErr with f.mu held
	f.callErr = func(to, sel string, block uint64) string {
		if sel != selTotalSupply {
			return ""
		}
		if allBusy || to == strings.ToLower(tokX) {
			return "execution reverted: mint limit exceeded"
		}
		return ""
	}
	setBusy := func(v bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		allBusy = v
		f.balanceErr = ""
		if v {
			f.balanceErr = "too many requests, try again later"
		}
	}
	fx.post("x", 1, tokX) // the oldest call: processed last
	fx.post("y", 2, tokY)
	fx.track(t, "x", 300)
	fx.track(t, "y", 310)

	s := fx.newScan()
	for _, p := range []struct {
		busy  bool
		wantN int
		wantY *int64
		wantX *int64
	}{
		{busy: true, wantN: 0},                       // Y and X fail: the pass ends, no strike
		{wantN: 1, wantY: i64p(310)},                 // Y filled, X skipped: strike 1 (Y answered)
		{busy: true, wantN: 0, wantY: i64p(310)},     // X alone, liveness request fails: no strike
		{wantN: 0, wantY: i64p(310)},                 // X alone, liveness request answered: strike 2
		{wantN: 0, wantY: i64p(310), wantX: i64p(0)}, // strike 3: given up
	} {
		setBusy(p.busy)
		if n := s.fillTokenSupply(ctx); n != p.wantN {
			t.Fatalf("busy=%v: looked up %d tokens, want %d", p.busy, n, p.wantN)
		}
		var ySup *string
		if p.wantY != nil {
			ySup = sp("7")
		}
		fx.wantSupply(t, "y", ySup, p.wantY)
		fx.wantSupply(t, "x", nil, p.wantX)
	}
	calls := fakeCount(f, "eth_call")
	if n := s.fillTokenSupply(ctx); n != 0 {
		t.Fatalf("looked up %d tokens after giving up on X, want 0", n)
	}
	if got := fakeCount(f, "eth_call") - calls; got != 0 {
		t.Errorf("got %d eth_calls after giving up on X, want 0", got)
	}
}

// TestFillTokenSupplyAdjacentFailures: two tokens next to each other whose
// totalSupply() keeps failing with a revert text that looks like a busy node
// (the words "timeout" and "generate") end the pass, but they do not stall
// it: the liveness request at the end strikes both, the next pass tries them
// last so the good tokens fill, and after tokenSupplyMaxStrikes passes they
// are given up. While the liveness request fails too (a really busy node),
// nothing is struck.
func TestFillTokenSupplyAdjacentFailures(t *testing.T) {
	oldBase := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = oldBase })
	f := newFakeChain(t, 24*time.Hour)
	fx := newSupplyFixture(t, f)
	ctx := context.Background()
	toks := map[string]string{
		"g1": "0x1111111111111111111111111111111111111111",
		"g2": "0x2222222222222222222222222222222222222222",
		"b1": "0x3333333333333333333333333333333333333333",
		"b2": "0x4444444444444444444444444444444444444444",
	}
	for k, ca := range toks {
		f.addToken(ca, 0, strings.ToUpper(k))
		f.constCall(ca, selTotalSupply, ret(wInt(9)))
	}
	bad := map[string]string{
		toks["b1"]: "execution aborted (timeout = 5s)",
		toks["b2"]: "execution reverted: could not generate supply",
	}
	f.callErr = func(to, sel string, block uint64) string {
		if sel != selTotalSupply {
			return ""
		}
		return bad[to]
	}
	setBusy := func(v bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.balanceErr = ""
		if v {
			f.balanceErr = "too many requests, try again later"
		}
	}
	// Newest calls first: the two failing tokens come before the good ones.
	fx.post("g1", 1, toks["g1"])
	fx.post("g2", 2, toks["g2"])
	fx.post("b1", 3, toks["b1"])
	fx.post("b2", 4, toks["b2"])
	for i, k := range []string{"g1", "g2", "b1", "b2"} {
		fx.track(t, k, uint64(300+i))
	}
	strikes := func(s *scanner) map[string]int {
		s.supplyMu.Lock()
		defer s.supplyMu.Unlock()
		out := map[string]int{}
		for k, n := range s.supplyStrikes {
			out[k] = n
		}
		return out
	}

	s := fx.newScan()
	for i, p := range []struct {
		busy        bool
		wantN       int
		wantGood    bool   // g1, g2 filled
		wantBad     *int64 // b1, b2 given up at this block
		wantStrikes int    // per failing token, after the pass
	}{
		{busy: true, wantN: 0, wantStrikes: 0},                       // b2, b1 fail, liveness fails: no strike
		{wantN: 0, wantStrikes: 1},                                   // b2, b1 fail, liveness answered: strike 1
		{wantN: 2, wantGood: true, wantStrikes: 2},                   // g2, g1 first and filled; b2, b1 strike 2
		{wantN: 0, wantGood: true, wantBad: i64p(0), wantStrikes: 0}, // strike 3: given up
	} {
		setBusy(p.busy)
		if n := s.fillTokenSupply(ctx); n != p.wantN {
			t.Fatalf("pass %d (busy=%v): looked up %d tokens, want %d", i+1, p.busy, n, p.wantN)
		}
		for _, k := range []string{"g1", "g2"} {
			if p.wantGood {
				fx.wantSupply(t, k, sp("9"), i64p(int64(300+map[string]int{"g1": 0, "g2": 1}[k])))
				continue
			}
			fx.wantSupply(t, k, nil, nil)
		}
		fx.wantSupply(t, "b1", nil, p.wantBad)
		fx.wantSupply(t, "b2", nil, p.wantBad)
		got := strikes(s)
		for _, k := range []string{"b1", "b2"} {
			if n := got[toks[k]]; n != p.wantStrikes {
				t.Errorf("pass %d (busy=%v): %s has %d strikes, want %d", i+1, p.busy, k, n, p.wantStrikes)
			}
		}
	}
	calls := fakeCount(f, "eth_call")
	if n := s.fillTokenSupply(ctx); n != 0 {
		t.Fatalf("looked up %d tokens after giving up, want 0", n)
	}
	if got := fakeCount(f, "eth_call") - calls; got != 0 {
		t.Errorf("got %d eth_calls after giving up, want 0", got)
	}
}
