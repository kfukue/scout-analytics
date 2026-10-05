package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCleanTokenText(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"  Alpha Token \x00\x00", 100, "Alpha Token"},
		{"Bad\x00\nName\t\x1b[31m!", 100, "BadName[31m!"},
		{"evil‮gnp.exe", 100, "evilgnp.exe"},
		{"bad\xff\xfeutf8", 100, "badutf8"},
		{strings.Repeat("é", 150), 100, strings.Repeat("é", 100)},
		{"TOOLONGSYMBOL" + strings.Repeat("X", 40), 32, ("TOOLONGSYMBOL" + strings.Repeat("X", 40))[:32]},
		{"<img src=x onerror=alert(1)>", 100, "<img src=x onerror=alert(1)>"}, // kept as text; the page never parses it as HTML
		{"\x00\x00", 100, ""},
	} {
		if got := cleanTokenText(c.in, c.max); got != c.want {
			t.Errorf("cleanTokenText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// name() is read next to symbol()/decimals(); a token without it still works.
func TestTokenInfoReadsName(t *testing.T) {
	f := newFakeChain(t, time.Hour)
	o := testOnchain(t, f, nil)
	ctx := context.Background()
	named, bare, b32 := "0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000a2", "0x00000000000000000000000000000000000000a3"
	f.addToken(named, 18, "ALPHA")
	f.constCall(named, selName, abiString("Alpha Token"))
	f.addToken(bare, 6, "BARE") // no name()
	f.addToken(b32, 18, "MKR")
	padded := make([]byte, 32)
	copy(padded, "Maker")
	f.constCall(b32, selName, "0x"+hex.EncodeToString(padded)) // bytes32-style name

	for addr, want := range map[string]tokenMeta{
		named: {Decimals: 18, Symbol: "ALPHA", Name: "Alpha Token"},
		bare:  {Decimals: 6, Symbol: "BARE", Name: ""},
		b32:   {Decimals: 18, Symbol: "MKR", Name: "Maker"},
	} {
		got, err := o.rpc.tokenInfo(ctx, addr)
		if err != nil || got != want {
			t.Fatalf("tokenInfo(%s) = %+v, %v; want %+v", addr, got, err, want)
		}
	}
	// The label of a token already read by tokenInfo costs no further request.
	before := f.count["eth_call"]
	if l, ok := o.rpc.tokenLabel(ctx, named); !ok || l != (tokenLabel{Name: "Alpha Token", Symbol: "ALPHA"}) {
		t.Fatalf("label %+v %v", l, ok)
	}
	if f.count["eth_call"] != before {
		t.Fatalf("cached label asked the node again (%d → %d)", before, f.count["eth_call"])
	}
}

func TestFillTokenNames(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t, 24*time.Hour)
	named := "0x44444444444444444444444444444444444444ab"
	namedMixed := "0x44444444444444444444444444444444444444AB" // a repeat call of the same token
	noName := "0x5555555555555555555555555555555555555555"     // symbol() but no name()
	nothing := "0x6666666666666666666666666666666666666666"    // not a token at all
	html := "0x7777777777777777777777777777777777777777"
	sol := "So11111111111111111111111111111111111111112x" // not an EVM address
	f.addToken(named, 18, "ALPHA")
	f.constCall(named, selName, abiString("Alpha Token"))
	f.addToken(noName, 18, "NONAME")
	f.addToken(html, 18, "<b>X</b>")
	f.constCall(html, selName, abiString("<img src=x onerror=alert(1)>\x00\n"+strings.Repeat("y", 200)))

	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	newScan := func() *scanner {
		s := newScanner(&config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true, "solana": true}, StateDir: t.TempDir(), Price: pc})
		s.db = st
		s.sourceChannelID = 777
		return s
	}
	s := newScan()
	now := time.Now()
	ids := map[string]int{}
	post := func(key string, msgID int, ca string) {
		t.Helper()
		id, _ := s.recordCallInfo(postAt(msgID, now.Add(-time.Duration(msgID)*time.Hour), ca), ca, nil, CallStatusBackfill)
		if id == nil {
			t.Fatalf("call %s not recorded", key)
		}
		ids[key] = *id
	}
	post("named", 1, named)
	post("named2", 2, namedMixed)
	post("noName", 3, noName)
	post("nothing", 4, nothing)
	post("html", 5, html)
	post("sol", 6, sol)
	// Give the rows a state the pass must not disturb.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET status = 'done', attempts = 3,
		next_check_at = '2031-01-02T03:04:05Z', onchain = '{"v": 2, "kind": "v3", "scan_block": 123}'::jsonb,
		updated_at = '2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	// The posts carry "$MALFOID"; one call has no symbol in its post.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_metrics SET token_symbol = NULL WHERE call_id = ANY($1)`,
		[]int{ids["named2"], ids["noName"], ids["nothing"]}); err != nil {
		t.Fatal(err)
	}
	type snap struct {
		Status, Next, Onchain, Updated string
		Attempts                       int
	}
	snapshot := func() map[int]snap {
		t.Helper()
		rows, err := st.Pool.Query(ctx, `SELECT call_id, status, next_check_at::text, COALESCE(onchain::text, ''), updated_at::text, attempts FROM scout_call_tracking`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int]snap{}
		for rows.Next() {
			var id int
			var x snap
			if err := rows.Scan(&id, &x.Status, &x.Next, &x.Onchain, &x.Updated, &x.Attempts); err != nil {
				t.Fatal(err)
			}
			out[id] = x
		}
		return out
	}
	names := func(key string) (name, sym *string) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx, `SELECT token_name, token_symbol_onchain FROM scout_call_tracking WHERE call_id = $1`, ids[key]).Scan(&name, &sym); err != nil {
			t.Fatal(err)
		}
		return
	}
	before := snapshot()
	if len(before) != 6 {
		t.Fatalf("%d tracking rows", len(before))
	}

	// The node is unreachable: nothing is stored (so it is tried again later).
	oldBase := rpcRetryBase
	rpcRetryBase = time.Millisecond
	t.Cleanup(func() { rpcRetryBase = oldBase })
	down := newScan()
	down.onchain.rpc.url = "http://127.0.0.1:1"
	if n := down.fillTokenNames(ctx); n > 1 { // at most the non-EVM address, which needs no node
		t.Fatalf("filled %d token(s) with the node down", n)
	}
	if n, _ := names("named"); n != nil {
		t.Fatalf("name stored with the node down: %q", *n)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET token_name = NULL, token_symbol_onchain = NULL`); err != nil {
		t.Fatal(err)
	}

	// First pass: five distinct tokens, two eth_calls for each of the four EVM ones.
	if n := s.fillTokenNames(ctx); n != 5 {
		t.Fatalf("filled %d tokens, want 5", n)
	}
	if got := f.count["eth_call"]; got != 8 {
		t.Fatalf("%d eth_calls, want 8 (name + symbol for 4 tokens)", got)
	}
	want := map[string][2]string{
		"named":   {"Alpha Token", "ALPHA"},
		"named2":  {"Alpha Token", "ALPHA"}, // every call of the contract
		"noName":  {"", "NONAME"},
		"nothing": {"", ""},
		"html":    {"<img src=x onerror=alert(1)>" + strings.Repeat("y", 72), "<b>X</b>"},
		"sol":     {"", ""},
	}
	for key, w := range want {
		n, sy := names(key)
		if n == nil || sy == nil || *n != w[0] || *sy != w[1] {
			t.Fatalf("%s: name %v symbol %v, want %q %q", key, strOrNil(n), strOrNil(sy), w[0], w[1])
		}
	}
	if n, _ := names("html"); len([]rune(*n)) != maxTokenNameLen {
		t.Fatalf("name length %d", len([]rune(*n)))
	}
	// Status, schedule, attempts, on-chain state and updated_at are untouched.
	after := snapshot()
	for id, b := range before {
		if after[id] != b {
			t.Fatalf("call %d changed: %+v → %+v", id, b, after[id])
		}
	}

	// Second pass, and a pass by a new process (empty cache): nothing is asked again,
	// not even for the tokens without a name.
	for i, sc := range []*scanner{s, newScan()} {
		if n := sc.fillTokenNames(ctx); n != 0 {
			t.Fatalf("pass %d filled %d tokens again", i+2, n)
		}
	}
	if got := f.count["eth_call"]; got != 8 {
		t.Fatalf("%d eth_calls after the repeat passes, want still 8", got)
	}
	// Nothing became due, and the migration does not queue anything again.
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.trackDue(ctx, 50); n != 0 {
		t.Fatalf("%d call(s) tracked again", n)
	}
	now2 := snapshot()
	for id, b := range before {
		if now2[id] != b {
			t.Fatalf("call %d changed after migrate: %+v → %+v", id, b, now2[id])
		}
	}

	// A later call of a known token is filled from the database, without the node.
	post("named3", 7, namedMixed)
	if n, _ := names("named3"); n != nil {
		t.Fatal("new call already has a name")
	}
	newScan().fillTokenNames(ctx)
	if n, sy := names("named3"); n == nil || *n != "Alpha Token" || *sy != "ALPHA" || f.count["eth_call"] != 8 {
		t.Fatalf("repeat call: %v %v, %d eth_calls", strOrNil(n), strOrNil(sy), f.count["eth_call"])
	}

	// The dataset view: token_name, and token_symbol = post symbol, else the on-chain one.
	view := func(key string) (name, sym any) {
		t.Helper()
		b, err := st.DatasetRowJSON(ctx, ids[key])
		if err != nil || b == nil {
			t.Fatalf("dataset row %s: %v", key, err)
		}
		var row map[string]any
		json.Unmarshal(b, &row)
		if _, ok := row["token_name"]; !ok {
			t.Fatal("view has no token_name column")
		}
		return row["token_name"], row["token_symbol"]
	}
	for key, w := range map[string][2]any{
		"named":   {"Alpha Token", "MALFOID"}, // the post's symbol wins
		"named2":  {"Alpha Token", "ALPHA"},   // no symbol in the post → on-chain
		"noName":  {nil, "NONAME"},
		"nothing": {nil, nil},
	} {
		if n, sy := view(key); n != w[0] || sy != w[1] {
			t.Fatalf("view %s: name %v symbol %v, want %v %v", key, n, sy, w[0], w[1])
		}
	}

	// Off for the GeckoTerminal source.
	if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET token_name = NULL`); err != nil {
		t.Fatal(err)
	}
	g := newScan()
	g.pc.Source = "gecko"
	if n := g.fillTokenNames(ctx); n != 0 || f.count["eth_call"] != 8 {
		t.Fatalf("gecko source filled %d", n)
	}
}
