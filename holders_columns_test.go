package main

import (
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// holderColumnsWant is the column order, written out: a change to it must be
// deliberate (the ml script and the CSV readers depend on it).
var holderColumnsWant = []string{
	"call_id", "token", "chain", "status", "skip_reason",
	"state_v", "venue_kind", "venue_pool", "venue_pool_id", "venue_curve", "pons_grad_block",
	"venues_pre", "venues_post", "excluded_pre", "v4_pool_ids", "v4_pool_ids_pre",
	"entry_at", "entry_block", "launch_block", "launch_time", "launch_tx", "capped", "deployer", "deployer_source",
	"post_end_block", "post_end_time",
	"transfers_pre", "transfers_post", "lp_events_pre", "lp_events_post",
	"dup_logs_dropped", "removed_logs_dropped", "bad_lp_logs_skipped", "bad_transfer_logs", "bad_transfer_error",
	"rpc_logs_requests", "rpc_other_requests", "rpc_requests_total",
	"posted_age_s", "exported_at", "code_version", "elapsed_s",

	"pre_hold_incomplete", "pre_hold_launch_age_s", "pre_hold_transfers", "pre_hold_supply_raw",
	"pre_hold_circulating_raw", "pre_hold_holders", "pre_hold_top1_share", "pre_hold_top5_share",
	"pre_hold_top10_share", "pre_hold_deployer_share", "pre_hold_deployer_sold",
	"pre_hold_buyers", "pre_hold_sellers", "pre_hold_buys", "pre_hold_sells", "pre_hold_sell_buy_ratio",
	"pre_hold_first_buy_block", "pre_hold_first_buy_to_entry_blocks", "pre_hold_sniper_window_blocks",
	"pre_hold_snipers", "pre_hold_sniper_held_share", "pre_hold_first_block_buyers", "pre_hold_max_tx_buyers",
	"pre_hold_early_buyers", "pre_hold_early_buyers_held_share",
	"pre_hold_lp_applicable", "pre_hold_lp_adds", "pre_hold_lp_removes", "pre_hold_lp_collects",
	"pre_hold_lp_removed_before_entry", "pre_hold_deployer_removed_lp",

	"post24h_incomplete", "post24h_transfers", "post24h_holders", "post24h_top1_share", "post24h_top5_share",
	"post24h_top10_share", "post24h_deployer_share", "post24h_deployer_sold",
	"post24h_buyers", "post24h_sellers", "post24h_buys", "post24h_sells", "post24h_sell_buy_ratio",
	"post24h_presnipers_held_share", "post24h_early_buyers_held_share",
	"post24h_lp_adds", "post24h_lp_removes", "post24h_lp_collects", "post24h_deployer_removed_lp",
}

func TestHoldersColumnOrder(t *testing.T) {
	if got := holderColumnNames(); !slices.Equal(got, holderColumnsWant) {
		t.Errorf("columns:\n got  %v\n want %v", got, holderColumnsWant)
	}
}

// The feature whitelist is exactly the pre_hold_* columns, in order; no
// column anywhere is named pool_*, and only features start with pre_.
func TestHoldersPreHoldWhitelist(t *testing.T) {
	var want []string
	for _, c := range holderColumnsWant {
		if strings.HasPrefix(c, "pre_hold_") {
			want = append(want, c)
		}
	}
	if got := holderPreHoldColumns(); !slices.Equal(got, want) {
		t.Errorf("whitelist %v, want %v", got, want)
	}
	if n := reflect.TypeFor[holderPreCols]().NumField(); len(want) != n {
		t.Errorf("%d pre_hold_ columns, but holderPreCols has %d fields", len(want), n)
	}
	for _, c := range holderColumnNames() {
		if strings.HasPrefix(c, "pool_") || (strings.HasPrefix(c, "pre_") && !strings.HasPrefix(c, "pre_hold_")) {
			t.Errorf("column %s: features start pre_hold_, never pool_", c)
		}
	}
}

// fillAll sets every field of v (a struct) to a non-zero value, so a test
// sees every column with a value.
func fillAll(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		fillValue(t, v.Field(i))
	}
}

func fillValue(t *testing.T, f reflect.Value) {
	t.Helper()
	switch {
	case f.Type() == reflect.TypeFor[time.Time]():
		f.Set(reflect.ValueOf(time.Date(2026, 10, 1, 2, 3, 4, 5, time.UTC)))
		return
	case f.Kind() == reflect.Struct:
		fillAll(t, f)
		return
	case f.Kind() == reflect.Pointer:
		f.Set(reflect.New(f.Type().Elem()))
		fillValue(t, f.Elem())
		return
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString("x")
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int64:
		f.SetInt(-3)
	case reflect.Uint64:
		f.SetUint(3)
	case reflect.Float64:
		f.SetFloat(0.25)
	case reflect.Slice:
		f.Set(reflect.ValueOf([]string{"a", "b"}))
	default:
		t.Fatalf("fillValue: kind %s not handled", f.Kind())
	}
}

// JSON carries every column, as null when NULL, so JSONL and CSV never drift.
func TestHoldersColumnsMatchJSON(t *testing.T) {
	var full holderRow
	fillAll(t, reflect.ValueOf(&full).Elem())
	for name, r := range map[string]holderRow{"empty": {}, "full": full} {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		if got, want := sortedStrings(keys...), sortedStrings(holderColumnsWant...); !slices.Equal(got, want) {
			t.Errorf("%s: JSON keys %v, want the columns %v", name, got, want)
		}
	}
	// A full row has a non-empty CSV field for every column (the formatter
	// knows every field type).
	for i, v := range holderCSVValues(full) {
		if v == "" {
			t.Errorf("full row: column %s is empty in the CSV", holderColumnsWant[i])
		}
	}
}

// CSV fields: NULL empty, lists joined with ";", big integers as decimal
// text, booleans, floats and times readable back exactly.
func TestHoldersCSVValues(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 200) // far past float64 and int64
	r := holderRow{}
	r.CallID, r.Token, r.Status = 42, hrToken, holderRowOK
	r.VenuesPre = []string{hrPool, hvPM}
	r.V4PoolIDs = []string{}
	r.V4PoolIDsPre = []string{hvPoolID, lqPoolID}
	r.ExportedAt = time.Date(2026, 10, 20, 1, 2, 3, 0, time.FixedZone("PDT", -7*3600))
	r.holderPreCols.SupplyRaw = valPtr(huge.String())
	r.holderPreCols.Top1Share = valPtr(0.1)
	r.holderPreCols.DeployerSold = valPtr(false)
	r.holderPreCols.Buys = valPtr(0)
	r.holderPreCols.FirstBuyBlock = valPtr(uint64(18_446_744_073_709_551_615))
	r.holderPreCols.EarlyBuyers = []string{hrS1}
	got := map[string]string{}
	for i, v := range holderCSVValues(r) {
		got[holderColumnsWant[i]] = v
	}
	for col, want := range map[string]string{
		"call_id": "42", "token": hrToken, "status": "ok", "skip_reason": "",
		"venues_pre": hrPool + ";" + hvPM, "venues_post": "", "v4_pool_ids": "", "v4_pool_ids_pre": hvPoolID + ";" + lqPoolID,
		"exported_at": "2026-10-20T08:02:03Z", "entry_at": "", "capped": "false",
		"pre_hold_supply_raw": huge.String(), "pre_hold_top1_share": "0.1", "pre_hold_deployer_sold": "false",
		"pre_hold_buys": "0", "pre_hold_sells": "", "pre_hold_first_buy_block": "18446744073709551615",
		"pre_hold_early_buyers": hrS1, "pre_hold_top5_share": "", "post24h_holders": "",
	} {
		if got[col] != want {
			t.Errorf("csv %s = %q, want %q", col, got[col], want)
		}
	}
}
