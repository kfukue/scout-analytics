package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestExportLiveBuysCSV: two calls, three live buys (one without an amount),
// stored out of order; the export is sorted by call and position, NULL is an
// empty field and the "…" of the wallet survives the CSV round trip.
func TestExportLiveBuysCSV(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	newCall := func(msgID int, ca string) int {
		t.Helper()
		id, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 111, ChannelUsername: "scoutrobinhood",
			MessageID: msgID, MessageDate: at, ContractAddress: ca, Chain: "evm", Status: CallStatusQueued})
		if err != nil {
			t.Fatal(err)
		}
		return *id
	}
	callA := newCall(501, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	callB := newCall(502, "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	amt := func(v float64) *float64 { return &v }

	// call B first, and its positions out of order
	if err := st.UpsertCallMetrics(ctx, callB, &CallMeta{LiveBuys: []LiveBuy{
		{Position: 2, Tier: "good", TierEmoji: "✅", AmountUSD: nil, WalletDisplay: "0x12ab…9f00", WalletPrefix: "0x12ab", WalletSuffix: "9f00"},
		{Position: 1, Tier: "elite", TierEmoji: "💎", AmountUSD: amt(1234.5), WalletDisplay: "0x79f6…5c5d", WalletPrefix: "0x79f6", WalletSuffix: "5c5d"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCallMetrics(ctx, callA, &CallMeta{LiveBuys: []LiveBuy{
		{Position: 1, Tier: "good", TierEmoji: "✅", AmountUSD: amt(350), WalletDisplay: "0xdead…beef", WalletPrefix: "0xdead", WalletSuffix: "beef"},
	}}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := st.ExportLiveBuysCSV(ctx, &buf)
	if err != nil || n != 3 {
		t.Fatalf("ExportLiveBuysCSV = %d rows, %v; want 3, nil", n, err)
	}
	got, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	a, b := strconv.Itoa(callA), strconv.Itoa(callB)
	want := [][]string{
		{"call_id", "position", "tier", "amount_usd", "wallet_display", "wallet_prefix", "wallet_suffix"},
		{a, "1", "good", "350", "0xdead…beef", "0xdead", "beef"},
		{b, "1", "elite", "1234.5", "0x79f6…5c5d", "0x79f6", "5c5d"},
		{b, "2", "good", "", "0x12ab…9f00", "0x12ab", "9f00"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("export (call A = %d, call B = %d):\ngot  %q\nwant %q", callA, callB, got, want)
	}
}

// TestExportLiveBuysCSVEmpty: no live buys → only the header, 0 rows.
func TestExportLiveBuysCSVEmpty(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := st.ExportLiveBuysCSV(ctx, &buf)
	if err != nil || n != 0 {
		t.Fatalf("ExportLiveBuysCSV = %d rows, %v; want 0, nil", n, err)
	}
	if got, want := buf.String(), "call_id,position,tier,amount_usd,wallet_display,wallet_prefix,wallet_suffix\n"; got != want {
		t.Fatalf("export of an empty table = %q, want %q", got, want)
	}
}
