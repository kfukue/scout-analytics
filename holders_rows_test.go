package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Row-builder test addresses: the token, its v2 pool, a Pons curve, the
// deployer, three snipers, two later pre-call buyers, a post-call buyer and a
// wallet that never received anything.
var (
	hrToken, hrPool, hrCurve, hrDep    = hAddr(320), hAddr(321), hAddr(323), hAddr(322)
	hrS1, hrS2, hrS3, hrB4, hrB5, hrB6 = hAddr(331), hAddr(332), hAddr(333), hAddr(334), hAddr(335), hAddr(336)
	hrNobody                           = hAddr(339)
	hrEntryAt                          = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hrCfg                              = holderVenueConfig{PoolManagerV4: hvPM, PonsFactory: hvFactory}
)

const hrEntry, hrPostEnd = 210, 1000

// hrPre: 1000 minted to the deployer, 600 of it added as liquidity (with a v2
// Mint in the same tx), s1 and s2 bundled at 110, s3 on the sniper window's
// last block, b4 just after it, s1 sells half, b5 buys.
func hrPre() []tokenTransfer {
	return []tokenTransfer{
		xfer(zeroAddr, hrDep, tokens(1000), 100, 0),
		inTx(xfer(hrDep, hrPool, tokens(600), 101, 0), "0xa1"),
		inTx(xfer(hrPool, hrS1, tokens(50), 110, 1), "0xb1"),
		inTx(xfer(hrPool, hrS2, tokens(40), 110, 2), "0xb1"),
		xfer(hrPool, hrS3, tokens(30), 160, 0),
		xfer(hrPool, hrB4, tokens(20), 161, 0),
		xfer(hrS1, hrPool, tokens(25), 170, 0),
		xfer(hrPool, hrB5, tokens(10), 200, 0),
	}
}

// hrPost: b6 buys, s2 sells everything, the deployer removes liquidity (a v2
// Burn in the same tx).
func hrPost() []tokenTransfer {
	return []tokenTransfer{
		xfer(hrPool, hrB6, tokens(100), 300, 0),
		xfer(hrS2, hrPool, tokens(40), 400, 0),
		inTx(xfer(hrPool, hrDep, tokens(200), 500, 0), "0xr1"),
	}
}

func hrState() *onchainState {
	return &onchainState{Kind: "v2", Pool: hrPool, EntryBlock: hrEntry, V: 2}
}

// hrInput: a finished v2 call (see hrPre and hrPost).
func hrInput() holderRowInput {
	posted := 600
	return holderRowInput{
		Meta: holderCallMeta{CallID: 7, Token: "0x" + strings.ToUpper(hrToken[2:]), Chain: "evm", TrackingRow: true,
			State: hrState(), EntryAt: hrEntryAt, PostedAgeS: &posted},
		Logs: holderLogs{Status: holderFetchOK, LaunchBlock: 100, LaunchTx: "0xa0", LaunchAt: hrEntryAt.Add(-3 * time.Hour),
			Age: 3 * time.Hour, Deployer: hrDep, DeployerSource: deployerFromMintTx, PostEndBlock: hrPostEnd,
			PostEndAt: hrEntryAt.Add(holderPostWindow), TransfersPre: hrPre(), TransfersPost: hrPost(),
			LiquidityPre:    []liquidityEvent{lqEv("v2", lpAdd, hrPool, "", "0xa1", 101)},
			LiquidityPost:   []liquidityEvent{lqEv("v2", lpRemove, hrPool, "", "0xr1", 500)},
			TransferCounts:  logStreamCounts{Logs: 13, Removed: 1, Duplicates: 1},
			LiquidityCounts: logStreamCounts{Logs: 4, Duplicates: 1, Bad: 1},
			LogRequests:     5, OtherRequests: 4},
		Config:             hrCfg,
		SniperWindowBlocks: defaultSniperWindowBlocks,
	}
}

var hrStamp = holderRowStamp{ExportedAt: hrEntryAt.Add(48 * time.Hour), CodeVersion: "abc123", Elapsed: 1500 * time.Millisecond}

func mustRow(t *testing.T, in holderRowInput) holderRow {
	t.Helper()
	r, err := buildHolderRow(in, hrStamp)
	if err != nil {
		t.Fatalf("buildHolderRow(call %d): %v", in.Meta.CallID, err)
	}
	return r
}

// rowMap is the row as the JSONL line decodes (numbers kept as text).
func rowMap(t *testing.T, r holderRow) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	m := map[string]any{}
	if err := d.Decode(&m); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	return m
}

func wantNull(t *testing.T, m map[string]any, cols ...string) {
	t.Helper()
	for _, c := range cols {
		if v, ok := m[c]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want NULL", c, v, ok)
		}
	}
}

func wantNotNull(t *testing.T, m map[string]any, cols ...string) {
	t.Helper()
	for _, c := range cols {
		if v, ok := m[c]; !ok || v == nil {
			t.Errorf("%s is NULL (present %v), want a value", c, ok)
		}
	}
}

// wantValue compares a column with want's JSON text.
func wantValue(t *testing.T, m map[string]any, col string, want any) {
	t.Helper()
	got, _ := json.Marshal(m[col]) // a decoded JSON value always re-encodes
	w, _ := json.Marshal(want)     // test literals always encode
	if !bytes.Equal(got, w) {
		t.Errorf("%s = %s, want %s", col, got, w)
	}
}

func wantShareCol(t *testing.T, m map[string]any, col string, want float64) {
	t.Helper()
	n, ok := m[col].(json.Number)
	if !ok {
		t.Errorf("%s = %v, want %.6f", col, m[col], want)
		return
	}
	f, err := n.Float64()
	if err != nil {
		t.Fatalf("%s: %v", col, err)
	}
	wantShare(t, col, f, want)
}

// groupCols returns the column names with prefix.
func groupCols(prefix string) []string {
	var out []string
	for _, c := range holderColumnNames() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// colsExcept returns cols without the ones in skip.
func colsExcept(cols []string, skip ...string) []string {
	return slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return slices.Contains(skip, c) })
}

// A finished v2 call: provenance, pre-call features and the post window.
func TestHoldersRowValues(t *testing.T) {
	m := rowMap(t, mustRow(t, hrInput()))
	for col, want := range map[string]any{
		"call_id": 7, "token": hrToken, "chain": "evm", "status": "ok", "skip_reason": nil, "state_v": 2,
		"venue_kind": "v2", "venue_pool": hrPool, "venue_pool_id": nil, "venue_curve": nil, "pons_grad_block": nil,
		"venues_pre": []string{hrPool}, "venues_post": []string{hrPool}, "v4_pool_ids_pre": []string{},
		"excluded_pre": sortedStrings(hrPool, hrToken, zeroAddr, deadAddr, hvPM), "v4_pool_ids": []string{},
		"entry_at": hrEntryAt, "entry_block": hrEntry, "launch_block": 100, "launch_time": hrEntryAt.Add(-3 * time.Hour),
		"launch_tx": "0xa0", "capped": false, "deployer": hrDep, "deployer_source": "mint_tx_from",
		"post_end_block": hrPostEnd, "post_end_time": hrEntryAt.Add(holderPostWindow),
		"transfers_pre": 8, "transfers_post": 3, "lp_events_pre": 1, "lp_events_post": 1,
		"dup_logs_dropped": 2, "removed_logs_dropped": 1, "bad_lp_logs_skipped": 1,
		"bad_transfer_logs": 0, "bad_transfer_error": nil,
		"rpc_logs_requests": 5, "rpc_other_requests": 4, "rpc_requests_total": 9, "posted_age_s": 600,
		"exported_at": hrStamp.ExportedAt, "code_version": "abc123", "elapsed_s": 1.5,

		"pre_hold_incomplete": false, "pre_hold_launch_age_s": 3 * 3600, "pre_hold_transfers": 8,
		"pre_hold_supply_raw": tokens(1000).String(), "pre_hold_circulating_raw": tokens(525).String(),
		"pre_hold_holders": 6, "pre_hold_deployer_sold": true,
		"pre_hold_buyers": 5, "pre_hold_sellers": 1, "pre_hold_buys": 5, "pre_hold_sells": 1,
		"pre_hold_first_buy_block": 110, "pre_hold_first_buy_to_entry_blocks": 100, "pre_hold_sniper_window_blocks": 50,
		"pre_hold_snipers": 3, "pre_hold_first_block_buyers": 2, "pre_hold_max_tx_buyers": 2,
		"pre_hold_early_buyers":  []string{hrS1, hrS2, hrS3, hrB4, hrB5},
		"pre_hold_lp_applicable": true, "pre_hold_lp_adds": 1, "pre_hold_lp_removes": 0, "pre_hold_lp_collects": 0,
		"pre_hold_lp_removed_before_entry": false, "pre_hold_deployer_removed_lp": false,

		"post24h_incomplete": false, "post24h_transfers": 3, "post24h_holders": 6, "post24h_deployer_sold": false,
		"post24h_buyers": 1, "post24h_sellers": 1, "post24h_buys": 1, "post24h_sells": 1,
		"post24h_lp_adds": 0, "post24h_lp_removes": 1, "post24h_lp_collects": 0, "post24h_deployer_removed_lp": true,
	} {
		wantValue(t, m, col, want)
	}
	for col, want := range map[string]float64{
		"pre_hold_top1_share": 400.0 / 525, "pre_hold_top5_share": 515.0 / 525, "pre_hold_top10_share": 1,
		"pre_hold_deployer_share": 400.0 / 525, "pre_hold_sell_buy_ratio": 25.0 / 150,
		"pre_hold_sniper_held_share": 95.0 / 525, "pre_hold_early_buyers_held_share": 125.0 / 525,
		"post24h_top1_share": 600.0 / 785, "post24h_deployer_share": 600.0 / 785, "post24h_sell_buy_ratio": 0.4,
		"post24h_presnipers_held_share": 55.0 / 785, "post24h_early_buyers_held_share": 85.0 / 785,
	} {
		wantShareCol(t, m, col, want)
	}
}

// Each OK flag of the pure helpers turns its columns NULL, and only those.
func TestHoldersRowNullRules(t *testing.T) {
	sellsOnly := []tokenTransfer{
		xfer(zeroAddr, hrDep, tokens(1000), 100, 0),
		inTx(xfer(hrDep, hrPool, tokens(600), 101, 0), "0xa1"),
		xfer(hrDep, hrS1, tokens(100), 105, 0),
		xfer(hrS1, hrPool, tokens(50), 120, 0),
	}
	postNoS2 := []tokenTransfer{hrPost()[0], hrPost()[2]} // without s2's sell (s2 never bought here)
	allInPool := []tokenTransfer{
		xfer(zeroAddr, hrPool, tokens(1000), 100, 0),
		xfer(hrPool, hrS1, tokens(50), 110, 0),
		xfer(hrS1, hrPool, tokens(50), 120, 0),
	}
	buyNulls := []string{"pre_hold_first_buy_block", "pre_hold_first_buy_to_entry_blocks", "pre_hold_snipers",
		"pre_hold_sniper_held_share", "pre_hold_first_block_buyers", "pre_hold_max_tx_buyers", "pre_hold_early_buyers",
		"pre_hold_early_buyers_held_share", "post24h_presnipers_held_share", "post24h_early_buyers_held_share"}
	preLP := []string{"pre_hold_lp_adds", "pre_hold_lp_removes", "pre_hold_lp_collects",
		"pre_hold_lp_removed_before_entry", "pre_hold_deployer_removed_lp"}
	postLP := []string{"post24h_lp_adds", "post24h_lp_removes", "post24h_lp_collects", "post24h_deployer_removed_lp"}
	cases := []struct {
		name    string
		mutate  func(in *holderRowInput)
		null    []string
		notNull []string
		equal   map[string]any
	}{
		{name: "circulating not > 0 (!SharesOK)",
			mutate: func(in *holderRowInput) {
				in.Logs.TransfersPre, in.Logs.TransfersPost = allInPool, nil
				in.Logs.LiquidityPre, in.Logs.LiquidityPost = nil, nil
			},
			null: []string{"pre_hold_top1_share", "pre_hold_top5_share", "pre_hold_top10_share", "pre_hold_deployer_share",
				"post24h_sell_buy_ratio", // nothing bought in the window
				"pre_hold_sniper_held_share", "pre_hold_early_buyers_held_share", "post24h_top1_share", "post24h_top5_share",
				"post24h_top10_share", "post24h_deployer_share", "post24h_presnipers_held_share", "post24h_early_buyers_held_share"},
			notNull: []string{"pre_hold_first_buy_block", "pre_hold_snipers", "pre_hold_early_buyers", "pre_hold_holders",
				"pre_hold_deployer_sold", "pre_hold_sell_buy_ratio", "post24h_holders"},
			equal: map[string]any{"pre_hold_holders": 0, "pre_hold_circulating_raw": "0"}},
		{name: "deployer is an excluded address (!DeployerShareOK)",
			mutate:  func(in *holderRowInput) { in.Logs.Deployer = hrPool },
			null:    []string{"pre_hold_deployer_share", "post24h_deployer_share"},
			notNull: []string{"pre_hold_deployer_sold", "pre_hold_deployer_removed_lp", "pre_hold_top1_share", "post24h_deployer_sold"}},
		{name: "deployer unknown (!DeployerKnown)",
			mutate: func(in *holderRowInput) { in.Logs.Deployer, in.Logs.DeployerSource = "", "" },
			null: []string{"deployer", "deployer_source", "pre_hold_deployer_share", "pre_hold_deployer_sold",
				"pre_hold_deployer_removed_lp", "post24h_deployer_share", "post24h_deployer_sold", "post24h_deployer_removed_lp"},
			notNull: []string{"pre_hold_lp_adds", "pre_hold_lp_removed_before_entry", "post24h_lp_removes", "pre_hold_top1_share"}},
		{name: "sells only before the call (!SellBuyRatioOK, !BuysOK)",
			mutate: func(in *holderRowInput) { in.Logs.TransfersPre, in.Logs.TransfersPost = sellsOnly, postNoS2 },
			null:   append([]string{"pre_hold_sell_buy_ratio"}, buyNulls...),
			notNull: []string{"pre_hold_buys", "pre_hold_sells", "pre_hold_buyers", "pre_hold_top1_share",
				"post24h_buys", "post24h_sell_buy_ratio"},
			equal: map[string]any{"pre_hold_buys": 0, "pre_hold_buyers": 0, "pre_hold_sells": 1}},
		{name: "no trades at all before the call",
			mutate: func(in *holderRowInput) { in.Logs.TransfersPre, in.Logs.TransfersPost = sellsOnly[:3], postNoS2 },
			null:   append([]string{"pre_hold_sell_buy_ratio"}, buyNulls...),
			equal:  map[string]any{"pre_hold_buys": 0, "pre_hold_sells": 0, "pre_hold_buyers": 0, "pre_hold_sellers": 0}},
		{name: "Pons token on its curve (LP not applicable)",
			mutate: func(in *holderRowInput) {
				in.Meta.State = &onchainState{Kind: "pons", Pool: hrPool, Curve: hrPool, EntryBlock: hrEntry}
			},
			null:    append(slices.Clone(preLP), postLP...),
			notNull: []string{"pre_hold_buys", "post24h_buys"},
			equal:   map[string]any{"pre_hold_lp_applicable": false}},
		{name: "Pons token graduated inside the window (LP applies after the call only)",
			mutate: func(in *holderRowInput) {
				// The fixture's pool plays the curve; the window's v2 events
				// count once the PoolManager is a venue too.
				in.Meta.State = &onchainState{Kind: "pons", Pool: hrPool, Curve: hrPool, PoolID: hvPoolID,
					PonsDone: 450, PonsGrad: 450, EntryBlock: hrEntry}
			},
			null:    preLP,
			notNull: postLP,
			equal:   map[string]any{"pre_hold_lp_applicable": false, "venues_post": []string{hrPool, hvPM}}},
		{name: "v4 without pool ids (LP not applicable)",
			mutate: func(in *holderRowInput) {
				in.Meta.State = &onchainState{Kind: "v4", Pool: hrPool, EntryBlock: hrEntry}
			},
			null:  append(slices.Clone(preLP), postLP...),
			equal: map[string]any{"pre_hold_lp_applicable": false, "v4_pool_ids": []string{}, "v4_pool_ids_pre": []string{}}},
		{name: "v4 with the state's PoolID only (not a pre id: LP applies after the call only)",
			mutate: func(in *holderRowInput) {
				in.Meta.State = &onchainState{Kind: "v4", Pool: hrPool, PoolID: hvPoolID, EntryBlock: hrEntry}
			},
			null:    preLP,
			notNull: postLP,
			equal: map[string]any{"pre_hold_lp_applicable": false, "v4_pool_ids": []string{hvPoolID},
				"v4_pool_ids_pre": []string{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := hrInput()
			tc.mutate(&in)
			m := rowMap(t, mustRow(t, in))
			wantValue(t, m, "status", "ok")
			wantNull(t, m, tc.null...)
			wantNotNull(t, m, tc.notNull...)
			for c, v := range tc.equal {
				wantValue(t, m, c, v)
			}
			// Nothing else in the groups is NULL that the happy path fills,
			// apart from what the case names.
			base := rowMap(t, mustRow(t, hrInput()))
			for _, c := range append(groupCols("pre_hold_"), groupCols("post24h_")...) {
				if m[c] == nil && base[c] != nil && !slices.Contains(tc.null, c) {
					t.Errorf("%s is NULL, not expected in this case", c)
				}
			}
		})
	}
}

// An incomplete replay leaves only *_incomplete and the transfer counts in
// its group; an incomplete post window leaves the pre-call features alone.
func TestHoldersRowIncomplete(t *testing.T) {
	unfunded := xfer(hrNobody, hrS1, tokens(5), 180, 0)
	keep := []string{"incomplete", "transfers"}
	t.Run("before the call", func(t *testing.T) {
		in := hrInput()
		in.Logs.TransfersPre = append(hrPre(), unfunded)
		m := rowMap(t, mustRow(t, in))
		for _, g := range []string{"pre_hold_", "post24h_"} {
			wantValue(t, m, g+"incomplete", true)
			wantNull(t, m, colsExcept(groupCols(g), g+keep[0], g+keep[1])...)
		}
		wantValue(t, m, "pre_hold_transfers", 9)
		wantValue(t, m, "post24h_transfers", 3)
		wantValue(t, m, "status", "ok")
	})
	t.Run("after the call only", func(t *testing.T) {
		in := hrInput()
		in.Logs.TransfersPost = append(hrPost(), xfer(hrNobody, hrS1, tokens(5), 600, 0))
		m := rowMap(t, mustRow(t, in))
		wantValue(t, m, "post24h_incomplete", true)
		wantNull(t, m, colsExcept(groupCols("post24h_"), "post24h_incomplete", "post24h_transfers")...)
		wantValue(t, m, "pre_hold_incomplete", false)
		wantNotNull(t, m, colsExcept(groupCols("pre_hold_"))...)
	})
}

// A capped call and every skip reason: status and reason, provenance kept,
// every pre_hold_* and post24h_* column NULL.
func TestHoldersRowCappedAndSkipped(t *testing.T) {
	features := append(groupCols("pre_hold_"), groupCols("post24h_")...)
	cases := []struct {
		name    string
		mutate  func(in *holderRowInput)
		status  string
		reason  any
		notNull []string
		null    []string
	}{
		{name: "capped", status: "capped", reason: nil,
			mutate: func(in *holderRowInput) {
				in.Logs = holderLogs{Status: holderFetchCapped, LaunchBlock: 5, LaunchTx: "0xa0",
					LaunchAt: hrEntryAt.Add(-100 * 24 * time.Hour), Age: 100 * 24 * time.Hour, LogRequests: 1, OtherRequests: 1}
			},
			notNull: []string{"launch_block", "launch_time", "rpc_requests_total", "venues_pre", "entry_block"},
			null: []string{"deployer", "post_end_block", "venues_post", "transfers_pre", "dup_logs_dropped",
				"bad_transfer_logs", "bad_transfer_error"}},
		{name: "no mint found", status: "skipped", reason: "no_mint_found",
			mutate:  func(in *holderRowInput) { in.Logs = holderLogs{Status: holderFetchNoMint, LogRequests: 1} },
			notNull: []string{"rpc_logs_requests", "venues_pre", "entry_block"},
			null:    []string{"launch_block", "deployer", "transfers_pre", "bad_transfer_logs", "bad_transfer_error"}},
		{name: "bad transfer logs", status: "skipped", reason: "bad_transfer_logs",
			mutate: func(in *holderRowInput) {
				l := in.Logs
				in.Logs = holderLogs{Status: holderFetchBadTransfers, LaunchBlock: l.LaunchBlock, LaunchAt: l.LaunchAt,
					Deployer: l.Deployer, DeployerSource: l.DeployerSource, PostEndBlock: l.PostEndBlock, PostEndAt: l.PostEndAt,
					TransferCounts: logStreamCounts{Logs: 3, Bad: 2}, BadTransfer: hrBadTransferErr,
					LogRequests: 3, OtherRequests: 3}
			},
			notNull: []string{"deployer", "post_end_block", "venues_post", "dup_logs_dropped",
				"bad_transfer_logs", "bad_transfer_error"},
			null: []string{"transfers_pre", "lp_events_pre", "bad_lp_logs_skipped"}},
		{name: "no tracking row", status: "skipped", reason: "no_tracking_row",
			mutate: func(in *holderRowInput) { in.Meta.TrackingRow = false },
			null:   []string{"rpc_requests_total", "launch_block", "venues_pre"}},
		{name: "not evm", status: "skipped", reason: "not_evm",
			mutate: func(in *holderRowInput) {
				in.Meta.Chain, in.Meta.Token = "solana", "So11111111111111111111111111111111111111112"
			},
			null: []string{"rpc_requests_total"}},
		{name: "evm chain but not an address", status: "skipped", reason: "not_evm",
			mutate: func(in *holderRowInput) { in.Meta.Token = "0x1234" }},
		{name: "no on-chain state", status: "skipped", reason: "no_onchain_state",
			mutate: func(in *holderRowInput) { in.Meta.State = nil },
			null:   []string{"venue_kind", "state_v", "entry_block"}},
		{name: "unknown state kind", status: "skipped", reason: "no_onchain_state",
			mutate:  func(in *holderRowInput) { in.Meta.State.Kind = "" },
			notNull: []string{"state_v", "entry_block"}},
		{name: "unknown entry block", status: "skipped", reason: "unknown_entry_block",
			mutate:  func(in *holderRowInput) { in.Meta.State.EntryBlock = 0 },
			notNull: []string{"venue_kind", "entry_at"},
			null:    []string{"entry_block"}},
		{name: "no entry time", status: "skipped", reason: "unknown_entry_block",
			mutate: func(in *holderRowInput) { in.Meta.EntryAt = time.Time{} },
			null:   []string{"entry_at"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := hrInput()
			tc.mutate(&in)
			m := rowMap(t, mustRow(t, in))
			wantValue(t, m, "status", tc.status)
			wantValue(t, m, "skip_reason", tc.reason)
			wantValue(t, m, "capped", tc.status == "capped")
			wantValue(t, m, "call_id", 7)
			wantValue(t, m, "code_version", "abc123")
			wantNull(t, m, features...)
			wantNotNull(t, m, tc.notNull...)
			wantNull(t, m, tc.null...)
		})
	}
}

// The token column is lowercased for an EVM address only: a Solana address
// is case-sensitive.
func TestHoldersRowTokenCase(t *testing.T) {
	const sol = "So11111111111111111111111111111111111111112"
	in := hrInput()
	in.Meta.Chain, in.Meta.Token = "solana", sol
	wantValue(t, rowMap(t, mustRow(t, in)), "token", sol)
	wantValue(t, rowMap(t, mustRow(t, hrInput())), "token", hrToken) // given upper-case
}

// hrBadTransferErr is shaped like readTransferStream's first decode error.
var hrBadTransferErr = fmt.Errorf("block %s log %s: %w", "0x65", "0x2", fmt.Errorf("%w: 2 topics, want 3", errBadTransferLog))

// A bad_transfer_logs skip records how many Transfer logs did not decode and
// the first decode error's text, exactly.
func TestHoldersRowBadTransferProvenance(t *testing.T) {
	in := hrInput()
	in.Logs = holderLogs{Status: holderFetchBadTransfers, LaunchBlock: 100, Deployer: hrDep, PostEndBlock: hrPostEnd,
		TransferCounts: logStreamCounts{Logs: 9, Bad: 2}, BadTransfer: hrBadTransferErr, LogRequests: 2}
	m := rowMap(t, mustRow(t, in))
	wantValue(t, m, "skip_reason", "bad_transfer_logs")
	wantValue(t, m, "bad_transfer_logs", 2)
	wantValue(t, m, "bad_transfer_error", "block 0x65 log 0x2: malformed Transfer log: 2 topics, want 3")
}

// An unknown fetch status is an error, never a row.
func TestHoldersRowUnknownStatus(t *testing.T) {
	in := hrInput()
	in.Logs.Status = 0
	if r, err := buildHolderRow(in, hrStamp); err == nil {
		t.Errorf("status 0: row %+v, want an error", r.holderProvenance)
	}
}

// preJSON is the pre_hold_* group as written.
func preJSON(t *testing.T, r holderRow) string {
	t.Helper()
	b, err := json.Marshal(r.holderPreCols)
	if err != nil {
		t.Fatalf("marshal pre columns: %v", err)
	}
	return string(b)
}

// THE LEAKAGE TEST: whatever happens after the entry block (transfers,
// liquidity events, the window's end, a graduation), every pre_hold_* value
// stays exactly the same.
func TestHoldersRowLeakage(t *testing.T) {
	lateGrad := func(in *holderRowInput) {
		in.Meta.State = &onchainState{Kind: "pons", Pool: hrPool, Curve: hrPool, Hook: hvHook, EntryBlock: hrEntry}
	}
	bases := map[string]func(in *holderRowInput){
		"v2":                  func(*holderRowInput) {},
		"Pons, no graduation": lateGrad,
		// The two v4 bases a leaked pool id would show on: no pre id (the
		// state's PoolID is not one unless the caller lists it), so LP is not
		// applicable before the call.
		"v4 with its pool id": func(in *holderRowInput) {
			in.Meta.State = &onchainState{Kind: "v4", Pool: hrPool, PoolID: hvPoolID, EntryBlock: hrEntry}
		},
		"v4 state without PoolID": func(in *holderRowInput) {
			in.Meta.State = &onchainState{Kind: "v4", Pool: hrPool, EntryBlock: hrEntry}
		},
		"deployer not known":   func(in *holderRowInput) { in.Logs.Deployer = "" },
		"no buys before entry": func(in *holderRowInput) { in.Logs.TransfersPre = hrPre()[:2] },
	}
	mutations := map[string]func(in *holderRowInput){
		"a whale buys after the call": func(in *holderRowInput) {
			in.Logs.TransfersPost = append(in.Logs.TransfersPost, xfer(hrPool, hrB6, tokens(300), 250, 0))
		},
		"the deployer dumps after the call": func(in *holderRowInput) {
			in.Logs.TransfersPost = append(in.Logs.TransfersPost, xfer(hrDep, hrPool, tokens(400), 260, 0))
		},
		"an unfunded spend after the call": func(in *holderRowInput) {
			in.Logs.TransfersPost = append(in.Logs.TransfersPost, xfer(hrNobody, hrS1, tokens(1), 270, 0))
		},
		"a snipers' sell-off after the call": func(in *holderRowInput) {
			in.Logs.TransfersPost = append(in.Logs.TransfersPost, xfer(hrS3, hrPool, tokens(30), 211, 0),
				xfer(hrS1, hrPool, tokens(25), 211, 1))
		},
		"post transfer values changed": func(in *holderRowInput) {
			for i := range in.Logs.TransfersPost {
				in.Logs.TransfersPost[i].Value = tokens(1)
			}
		},
		"more liquidity events after the call": func(in *holderRowInput) {
			in.Logs.LiquidityPost = append(in.Logs.LiquidityPost, lqEv("v2", lpAdd, hrPool, "", "0xc1", 300),
				lqEv("v2", lpRemove, hrPool, "", "0xc2", 301), lqEv("v4", lpRemove, hvPM, hvPoolID, "0xc3", 302))
			in.Logs.TransfersPost = append(in.Logs.TransfersPost, inTx(xfer(hrPool, hrDep, tokens(100), 301, 0), "0xc2"))
		},
		"no post data at all": func(in *holderRowInput) {
			in.Logs.TransfersPost, in.Logs.LiquidityPost = nil, nil
		},
		"window end moved earlier": func(in *holderRowInput) {
			in.Logs.PostEndBlock, in.Logs.PostEndAt = 350, hrEntryAt.Add(time.Hour)
		},
		"window end moved later": func(in *holderRowInput) {
			in.Logs.PostEndBlock, in.Logs.PostEndAt = 1_000_000, hrEntryAt.Add(100*holderPostWindow)
		},
		"window end at the entry block": func(in *holderRowInput) {
			in.Logs.PostEndBlock, in.Logs.TransfersPost, in.Logs.LiquidityPost = hrEntry, nil, nil
		},
		"graduation found after the call": func(in *holderRowInput) {
			if in.Meta.State.Kind == "pons" {
				in.Meta.State.PoolID, in.Meta.State.PonsDone, in.Meta.State.PonsGrad = hvPoolID, 300, 300
			}
		},
		// holderRowInput's contract: a pool initialised after the entry block
		// goes in V4PoolIDsAll only, and nothing pre may see it.
		"extra pool id learned later": func(in *holderRowInput) {
			in.V4PoolIDsAll = append(in.V4PoolIDsAll, lqPoolID)
			in.Logs.LiquidityPost = append(in.Logs.LiquidityPost, lqEv("v4", lpAdd, hvPM, lqPoolID, "0xd1", 320))
		},
		// The tracker's discovery may store a v4 pool initialised after the
		// entry block as the state's PoolID; the caller does not list it as a
		// pre id, so nothing pre may see it.
		"state PoolID set for a pool the caller does not list as pre": func(in *holderRowInput) {
			in.Meta.State.PoolID = hvPoolID
			in.Logs.LiquidityPost = append(in.Logs.LiquidityPost, lqEv("v4", lpAdd, hvPM, hvPoolID, "0xd2", 330))
		},
	}
	for bn, base := range bases {
		in := hrInput()
		base(&in)
		want := preJSON(t, mustRow(t, in))
		changedPost := false
		for mn, mutate := range mutations {
			t.Run(bn+"/"+mn, func(t *testing.T) {
				in := hrInput()
				base(&in)
				before := mustRow(t, in)
				mutate(&in)
				r := mustRow(t, in)
				if got := preJSON(t, r); got != want {
					t.Errorf("pre_hold_* changed:\n got  %s\n want %s", got, want)
				}
				if b1, _ := json.Marshal(before.holderPostCols); string(b1) != mustJSON(t, r.holderPostCols) {
					changedPost = true
				}
			})
		}
		if !changedPost {
			t.Errorf("%s: no mutation changed a post24h_* column: the leakage test is not exercising anything", bn)
		}
	}
}

// The pool-id mutation above can only pass because pre uses V4PoolIDsPre
// alone: the same id given as a pre id does change pre_hold_lp_applicable,
// and as a later id it changes the post window only.
func TestHoldersRowPreIDsOnly(t *testing.T) {
	in := hrInput()
	in.Meta.State = &onchainState{Kind: "v4", Pool: hrPool, EntryBlock: hrEntry}
	base := rowMap(t, mustRow(t, in))
	wantValue(t, base, "pre_hold_lp_applicable", false)
	wantNull(t, base, "post24h_lp_adds")

	later := in
	later.V4PoolIDsAll = []string{lqPoolID}
	m := rowMap(t, mustRow(t, later))
	wantValue(t, m, "pre_hold_lp_applicable", false)
	wantNotNull(t, m, "post24h_lp_adds")
	wantValue(t, m, "v4_pool_ids", []string{lqPoolID})
	wantValue(t, m, "v4_pool_ids_pre", []string{})

	early := in
	early.V4PoolIDsPre = []string{lqPoolID}
	m = rowMap(t, mustRow(t, early))
	wantValue(t, m, "pre_hold_lp_applicable", true)
	wantValue(t, m, "v4_pool_ids_pre", []string{lqPoolID})
}

// A state's PoolID is a pre id only when the caller lists it (plain v4: the
// tracker does not check the pool existed at the entry block) or when it is a
// Pons pool created at or before the entry block.
func TestHoldersRowStatePoolIDPre(t *testing.T) {
	preLP := []string{"pre_hold_lp_adds", "pre_hold_lp_removes", "pre_hold_lp_collects",
		"pre_hold_lp_removed_before_entry", "pre_hold_deployer_removed_lp"}
	cases := []struct {
		name   string
		st     onchainState
		pre    []string // V4PoolIDsPre as given
		lpPre  bool
		preIDs []string // v4_pool_ids_pre
	}{
		{name: "v4, PoolID not listed", st: onchainState{Kind: "v4", Pool: hrPool, PoolID: hvPoolID, EntryBlock: hrEntry},
			preIDs: []string{}},
		{name: "v4, PoolID listed as a pre id", st: onchainState{Kind: "v4", Pool: hrPool, PoolID: hvPoolID, EntryBlock: hrEntry},
			pre: []string{hvPoolID}, lpPre: true, preIDs: []string{hvPoolID}},
		{name: "Pons graduated before the call", st: onchainState{Kind: "v4", Pool: hrPool, PoolID: hvPoolID, Curve: hrCurve,
			PonsDone: 150, PonsGrad: 150, EntryBlock: hrEntry}, lpPre: true, preIDs: []string{hvPoolID}},
		{name: "Pons graduated after the call", st: onchainState{Kind: "pons", Pool: hrCurve, Curve: hrCurve, PoolID: hvPoolID,
			PonsDone: 300, PonsGrad: 300, EntryBlock: hrEntry}, preIDs: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := hrInput()
			st := tc.st
			in.Meta.State, in.V4PoolIDsPre = &st, tc.pre
			m := rowMap(t, mustRow(t, in))
			wantValue(t, m, "status", "ok")
			wantValue(t, m, "pre_hold_lp_applicable", tc.lpPre)
			wantValue(t, m, "v4_pool_ids_pre", tc.preIDs)
			wantValue(t, m, "v4_pool_ids", []string{hvPoolID})
			if tc.lpPre {
				wantNotNull(t, m, preLP...)
			} else {
				wantNull(t, m, preLP...)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// holderFetchInputFor gives the reader the state's venues and every pool id
// (pre, later and the state's).
func TestHoldersFetchInputFor(t *testing.T) {
	m := hrInput().Meta
	m.State = &onchainState{Kind: "pons", Pool: hrCurve, Curve: hrCurve, PoolID: hvPoolID, PonsGrad: 5000, EntryBlock: hrEntry}
	in := holderFetchInputFor(m, []string{lqPoolID}, []string{lqOtherPoolID}, hrCfg)
	if in.Token != hrToken || in.EntryBlock != hrEntry || !in.EntryAt.Equal(hrEntryAt) {
		t.Errorf("input %s %d %v, want %s %d %v", in.Token, in.EntryBlock, in.EntryAt, hrToken, hrEntry, hrEntryAt)
	}
	if got, want := venueStrings(in.Venues), []string{"pons:" + hrCurve, "v4:" + hvPM}; !slices.Equal(got, want) {
		t.Errorf("venues %v, want %v", got, want)
	}
	if want := sortedStrings(hvPoolID, lqPoolID, lqOtherPoolID); !slices.Equal(in.V4PoolIDs, want) {
		t.Errorf("pool ids %v, want %v", in.V4PoolIDs, want)
	}
	if _, err := normalizeHolderInput(in); err != nil {
		t.Errorf("the reader refuses the input: %v", err)
	}
}
