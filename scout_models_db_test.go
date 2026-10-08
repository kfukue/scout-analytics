package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// Integration tests. They run only when SCOUT_TEST_DATABASE_URL points at a
// THROWAWAY Postgres database (they drop and recreate the scout_* tables), e.g.
//
//	SCOUT_TEST_DATABASE_URL=postgres://postgres@localhost:5432/scout_test?sslmode=disable go test -p 1 .

func testStore(t *testing.T) *ScoutStore {
	t.Helper()
	dsn := os.Getenv("SCOUT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOUT_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := NewScoutStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Pool.Exec(ctx, `DROP VIEW IF EXISTS scout_call_predictions_v, scout_investigations_v, scout_call_dataset_v, scout_calls_v;
		DROP TABLE IF EXISTS scout_call_predictions, scout_call_precall, scout_call_candles, scout_delivery_investigations, scout_deliveries, scout_investigations,
			scout_investigation_tools, scout_scan_reports, scout_call_live_buys, scout_call_metrics,
			scout_call_returns, scout_call_tracking, scout_calls CASCADE`); err != nil {
		t.Fatal(err)
	}
	return st
}

func mustTool(t *testing.T, st *ScoutStore, code, bot, cmd, parser string, gate bool) int {
	t.Helper()
	id, err := st.UpsertInvestigationTool(context.Background(), &ScoutInvestigationTool{
		Code: code, Name: code, BotUsername: bot, CommandTemplate: cmd, Parser: parser, IsGate: gate, IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	return *id
}

func TestScoutStore(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // migration must be idempotent
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}

	// --- tools registry
	perc := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true)
	sal := mustTool(t, st, "salpha", "salpha_research_bot", "{ca}", parserText, false)
	old := mustTool(t, st, "oldtool", "old_bot", "{ca}", parserText, false)
	if again := mustTool(t, st, "perceptor", "perceptor0xBot", "/scan {ca}", parserPerceptor, true); again != perc {
		t.Fatalf("upsert changed id: %d -> %d", perc, again)
	}
	if err := st.SetActiveInvestigationTools(ctx, []string{"perceptor", "salpha"}); err != nil {
		t.Fatal(err)
	}
	active, err := st.SelectInvestigationTools(ctx, true)
	if err != nil || len(active) != 2 {
		t.Fatalf("active tools = %d, %v", len(active), err)
	}
	_ = old

	// --- calls
	ca := "0x1234567890abcdef1234567890ABCDEF12345678"
	call := &ScoutCall{ChannelID: 111, ChannelUsername: "scoutrobinhood", MessageID: 42,
		MessageDate: time.Now().UTC().Truncate(time.Second), MessageText: "CA " + ca,
		URLs: []string{"https://dexscreener.com/x"}, ContractAddress: ca, Chain: chainOf(ca), Status: CallStatusQueued}
	callID, err := st.InsertScoutCall(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.InsertScoutCall(ctx, &ScoutCall{ChannelID: 111, ChannelUsername: "scoutrobinhood",
		MessageID: 42, MessageDate: call.MessageDate, ContractAddress: ca, Chain: "evm", Status: CallStatusQueued})
	if err != nil || *id2 != *callID {
		t.Fatalf("call upsert: %v %v", id2, err)
	}
	if err := st.UpdateScoutCallStatus(ctx, *callID, CallStatusScanned); err != nil {
		t.Fatal(err)
	}
	if c, err := st.GetScoutCall(ctx, *callID); err != nil || c.Status != CallStatusScanned {
		t.Fatalf("GetScoutCall = %+v, %v", c, err)
	}

	// --- investigations: one per tool
	now := time.Now().UTC()
	pInv := &ScoutInvestigation{CallID: callID, ToolID: perc, ContractAddress: ca, RequestText: "/scan " + ca,
		RequestedAt: now, CompletedAt: &now, Status: investigationCompleted, BotMessageIDs: []int32{9001},
		ReportText: "report…", ReportURLs: []string{"https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0"},
		ReportURL: strPtr("https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0"), ExternalID: strPtr("deb9d3118ec1480e985032f9472c87c0"),
		VerdictLevel: levelClean, VerdictLabel: strPtr("No red flags found"), Ticker: strPtr("$ANYR"), VerdictSource: strPtr("perceptor.info")}
	pID, err := st.InsertScoutInvestigation(ctx, pInv)
	if err != nil {
		t.Fatal(err)
	}
	sInv := &ScoutInvestigation{CallID: callID, ToolID: sal, ContractAddress: ca, RequestText: ca,
		RequestedAt: now, CompletedAt: &now, Status: investigationCompleted, BotMessageIDs: []int32{7001, 7002},
		ReportText: "sAlpha research…", Details: json.RawMessage(`{"media":[{"message_id":7002,"type":"photo"}]}`)}
	sID, err := st.InsertScoutInvestigation(ctx, sInv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertScoutInvestigation(ctx, &ScoutInvestigation{ToolID: sal, ContractAddress: "SoLxyz",
		RequestText: "SoLxyz", RequestedAt: now, Status: investigationTimeout, Error: strPtr("no reply")}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetScoutInvestigation(ctx, *sID)
	if err != nil || got.ToolCode != "salpha" || len(got.BotMessageIDs) != 2 || got.VerdictLevel != levelUnknown {
		t.Fatalf("GetScoutInvestigation = %+v, %v", got, err)
	}
	var det map[string]any
	if err := json.Unmarshal(got.Details, &det); err != nil || det["media"] == nil {
		t.Fatalf("details = %s, %v", got.Details, err)
	}
	if list, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{CallID: callID}); err != nil || len(list) != 2 {
		t.Fatalf("by call: %d, %v", len(list), err)
	}
	if list, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{ToolCodes: []string{"perceptor"},
		VerdictLevels: []string{levelClean, levelCaution}}); err != nil || len(list) != 1 || *list[0].ExternalID == "" {
		t.Fatalf("perceptor clean: %d, %v", len(list), err)
	}
	if list, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{Statuses: []string{investigationTimeout}}); err != nil || len(list) != 1 {
		t.Fatalf("timeouts: %d, %v", len(list), err)
	}
	if list, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{ContractAddress: "0x1234567890ABCDEF1234567890abcdef12345678"}); err != nil || len(list) != 2 {
		t.Fatalf("by CA case-insensitive: %d, %v", len(list), err)
	}

	// --- delivery with both reports attached
	at := time.Now().UTC()
	dID, err := st.InsertScoutDelivery(ctx, &ScoutDelivery{CallID: callID, ContractAddress: ca, Target: "me",
		Status: DeliveryStatusSent, HeaderText: "✅ $ANYR", DeliveredAt: &at, InvestigationIDs: []int{*pID, *sID}})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := st.SelectScoutDeliveries(ctx, callID, 10)
	if err != nil || len(ds) != 1 || *ds[0].ID != *dID || len(ds[0].InvestigationIDs) != 2 {
		t.Fatalf("deliveries = %+v, %v", ds, err)
	}
	// bad investigation id → whole delivery rolled back
	if _, err := st.InsertScoutDelivery(ctx, &ScoutDelivery{ContractAddress: ca, Target: "me",
		Status: DeliveryStatusSent, InvestigationIDs: []int{999999}}); err == nil {
		t.Fatal("expected FK error")
	}
	if ds, _ := st.SelectScoutDeliveries(ctx, nil, 10); len(ds) != 1 {
		t.Fatalf("rollback failed: %d deliveries", len(ds))
	}

	// --- view
	var tool string
	var delivered bool
	if err := st.Pool.QueryRow(ctx, `SELECT tool, delivered FROM scout_investigations_v WHERE id = $1`, *sID).
		Scan(&tool, &delivered); err != nil || tool != "salpha" || !delivered {
		t.Fatalf("view: tool=%s delivered=%v err=%v", tool, delivered, err)
	}
}

// Upgrading from the earlier single-tool schema (scout_scan_reports).
func TestLegacyMigration(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	_, err := st.Pool.Exec(ctx, `
	CREATE TABLE scout_calls (
		id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, channel_id BIGINT NOT NULL, channel_username TEXT NOT NULL,
		message_id INTEGER NOT NULL, message_date TIMESTAMPTZ NOT NULL, message_text TEXT NOT NULL DEFAULT '',
		urls TEXT[] NOT NULL DEFAULT '{}', contract_address TEXT NOT NULL, chain TEXT NOT NULL, status TEXT NOT NULL,
		created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_by TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		CONSTRAINT scout_calls_msg_ca_uq UNIQUE (channel_id, message_id, contract_address));
	CREATE TABLE scout_scan_reports (
		id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, call_id INTEGER REFERENCES scout_calls (id) ON DELETE SET NULL,
		contract_address TEXT NOT NULL, scan_bot TEXT NOT NULL, scan_command TEXT NOT NULL,
		requested_at TIMESTAMPTZ NOT NULL, completed_at TIMESTAMPTZ, bot_message_id INTEGER,
		report_text TEXT NOT NULL DEFAULT '', report_urls TEXT[] NOT NULL DEFAULT '{}', perceptor_id TEXT, perceptor_url TEXT,
		verdict_level TEXT NOT NULL, verdict_label TEXT, ticker TEXT, verdict_summary TEXT, verdict_source TEXT,
		delivered BOOLEAN NOT NULL DEFAULT false, delivered_at TIMESTAMPTZ, deliver_target TEXT, error TEXT,
		created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_by TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
	INSERT INTO scout_calls (uuid, channel_id, channel_username, message_id, message_date, contract_address, chain, status, created_by, updated_by)
	VALUES ('11111111-1111-1111-1111-111111111111', 1, 'scoutrobinhood', 10, now(), '0xabc', 'evm', 'scanned', 'x', 'x');
	INSERT INTO scout_scan_reports (uuid, call_id, contract_address, scan_bot, scan_command, requested_at, bot_message_id,
		report_text, verdict_level, verdict_label, delivered, delivered_at, deliver_target, created_by, updated_by)
	VALUES ('22222222-2222-2222-2222-222222222222', 1, '0xabc', '@perceptor0xBot', '/scan 0xabc', now(), 55,
		'old report', 'clean', 'No red flags found', true, now(), 'me', 'x', 'x'),
	       ('33333333-3333-3333-3333-333333333333', NULL, '0xdef', '@perceptor0xBot', '/scan 0xdef', now(), NULL,
		'', 'unknown', NULL, false, NULL, NULL, 'x', 'x');`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // re-running must not duplicate
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
	inv, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{})
	if err != nil || len(inv) != 2 {
		t.Fatalf("migrated investigations = %d, %v", len(inv), err)
	}
	for _, i := range inv {
		if i.ToolCode != "perceptor" {
			t.Fatalf("tool = %s", i.ToolCode)
		}
		if i.ContractAddress == "0xabc" && (i.Status != investigationCompleted || len(i.BotMessageIDs) != 1 || i.ReportText != "old report") {
			t.Fatalf("migrated row = %+v", i)
		}
		if i.ContractAddress == "0xdef" && i.Status != investigationFailed {
			t.Fatalf("failed row status = %s", i.Status)
		}
	}
	ds, err := st.SelectScoutDeliveries(ctx, nil, 10)
	if err != nil || len(ds) != 1 || len(ds[0].InvestigationIDs) != 1 {
		t.Fatalf("migrated deliveries = %+v, %v", ds, err)
	}
}

func TestScannerRecordingHooks(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOUT_TOOLS", "perceptor,salpha")
	tools, err := loadTools()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Tools: tools, Chains: map[string]bool{"evm": true},
		StateDir: t.TempDir(), NotifyPeer: "me", DeliverLevels: map[string]bool{levelClean: true, levelCaution: true}}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	if err := s.registerTools(ctx); err != nil {
		t.Fatal(err)
	}

	msg := &tg.Message{ID: 5, Date: int(time.Now().Unix()), Message: "ape 0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	s.onChannelPost(msg)                                                      // new CA -> queued + job
	s.onChannelPost(&tg.Message{ID: 6, Date: msg.Date, Message: msg.Message}) // re-call -> duplicate
	j := <-s.queue
	if j.CallID == nil {
		t.Fatal("job has no CallID")
	}

	now := time.Now().UTC()
	results := []*toolResult{
		{Spec: tools[0], Command: "/scan " + j.CA, RequestedAt: now, CompletedAt: now, Status: investigationCompleted,
			Replies: []*tg.Message{{ID: 100, Message: "Done: https://www.perceptor.info/r/d3e4fa0b496b456fbdc9499ae5fcf5f2"}},
			Verdict: verdict{Level: levelCaution, Label: "Caution", Ticker: "$DARKCOMP", Source: "perceptor.info",
				URL: "https://www.perceptor.info/r/d3e4fa0b496b456fbdc9499ae5fcf5f2"}},
		{Spec: tools[1], Command: j.CA, RequestedAt: now, CompletedAt: now, Status: investigationCompleted,
			Replies: []*tg.Message{{ID: 200, Message: "sAlpha: holders 1.2k, dev sold 0%"},
				{ID: 201, Media: &tg.MessageMediaPhoto{}}},
			Verdict: verdict{Level: levelUnknown, Source: "telegram_text"}},
	}
	if !s.shouldDeliver(results) {
		t.Fatal("caution from gate tool should deliver")
	}
	var ids []int
	for _, r := range results {
		id := s.recordInvestigation(j, r)
		if id == nil {
			t.Fatalf("[%s] not recorded", r.Spec.Code)
		}
		ids = append(ids, *id)
	}
	s.setCallStatus(j.CallID, CallStatusScanned)
	s.recordDelivery(j, s.deliveryHeader(j, results), ids, nil)
	s.recordDelivery(j, "x", nil, errors.New("flood wait"))

	var queued, dup, scanned int
	_ = st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='queued'),
		count(*) FILTER (WHERE status='duplicate'), count(*) FILTER (WHERE status='scanned') FROM scout_calls`).
		Scan(&queued, &dup, &scanned)
	if queued != 0 || dup != 1 || scanned != 1 {
		t.Fatalf("calls: queued=%d dup=%d scanned=%d", queued, dup, scanned)
	}
	inv, err := st.SelectScoutInvestigations(ctx, ScoutInvestigationFilter{CallID: j.CallID})
	if err != nil || len(inv) != 2 {
		t.Fatalf("investigations = %d, %v", len(inv), err)
	}
	for _, i := range inv {
		switch i.ToolCode {
		case "perceptor":
			if i.ExternalID == nil || *i.ExternalID != "d3e4fa0b496b456fbdc9499ae5fcf5f2" || i.VerdictLevel != levelCaution {
				t.Fatalf("perceptor row = %+v", i)
			}
		case "salpha":
			if i.RequestText != j.CA || len(i.BotMessageIDs) != 2 || string(i.Details) == "{}" {
				t.Fatalf("salpha row = %+v details=%s", i, i.Details)
			}
		}
	}
	ds, _ := st.SelectScoutDeliveries(ctx, j.CallID, 10)
	if len(ds) != 2 {
		t.Fatalf("deliveries = %d", len(ds))
	}
	var sent, failed int
	for _, d := range ds {
		if d.Status == DeliveryStatusSent && len(d.InvestigationIDs) == 2 {
			sent++
		}
		if d.Status == DeliveryStatusFailed && d.Error != nil {
			failed++
		}
	}
	if sent != 1 || failed != 1 {
		t.Fatalf("sent=%d failed=%d", sent, failed)
	}
	if u := replyURLs([]*tg.Message{{Message: "see https://www.perceptor.info/r/abc and (https://x.io/y)"}}); len(u) != 2 || u[1] != "https://x.io/y" {
		t.Fatalf("replyURLs=%v", u)
	}
}

func TestOpenScoutStoreSelection(t *testing.T) {
	ctx := context.Background()
	// off → no store, no error
	if st, _, err := openScoutStore(ctx, &config{DBMode: "off"}); st != nil || err != nil {
		t.Fatalf("off: %v %v", st, err)
	}
	if _, _, err := openScoutStore(ctx, &config{DBMode: "bogus"}); err == nil {
		t.Fatal("unknown SCOUT_DB should error")
	}
	dsn := os.Getenv("SCOUT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOUT_TEST_DATABASE_URL not set")
	}
	// explicit DSN overrides the repo database
	st, src, err := openScoutStore(ctx, &config{DBMode: "repo", DatabaseURL: dsn})
	if err != nil || src != "SCOUT_DATABASE_URL" || !strings.Contains(st.Describe(ctx), `database "scout_test"`) {
		t.Fatalf("dsn: %v %s %v", st, src, err)
	}
	// a store wrapping a shared pool must not close it
	shared := NewScoutStoreFromPool(st.Pool)
	shared.Close()
	if err := st.Pool.Ping(ctx); err != nil {
		t.Fatalf("shared pool was closed: %v", err)
	}
	st.Close()
}

func TestCallMetricsStore(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir()}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777

	ca := "0x3333333333333333333333333333333333333333"
	msg := &tg.Message{ID: 10120, Date: int(time.Now().Unix()), Message: samplePost,
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonURL{Text: "Chart", URL: "https://dexscreener.com/robinhood/" + ca}}}}}}
	s.onChannelPost(msg)
	j := <-s.queue
	if j.CallID == nil || j.Meta == nil || str(j.Meta.TokenSymbol) != "MALFOID" {
		t.Fatalf("job = %+v", j)
	}
	got, err := st.GetCallMetrics(ctx, *j.CallID)
	if err != nil || got == nil {
		t.Fatalf("GetCallMetrics: %v %v", got, err)
	}
	m := got.Meta
	if f(m.McapUSD) != 52000 || f(m.LiqUSD) != 20000 || f(m.LiqPct) != 38 || f(m.TaxSellPct) != 2.5 ||
		i(m.AgeSeconds) != 720 || i(m.Holders) != 1150 || i(m.ProofElite) != 3 || i(m.ProofGood) != 5 ||
		str(m.Dex) != "Longxyz" || f(m.CalledAtMcapUSD) != 45200 {
		t.Fatalf("metrics = %+v", m)
	}
	if got.LiveBuysEliteCount != 2 || got.LiveBuysGoodCount != 2 || got.LiveBuysEliteUSD != 2000 || got.LiveBuysGoodUSD != 445.5 {
		t.Fatalf("totals = %+v", got)
	}
	if len(m.LiveBuys) != 4 || m.LiveBuys[0].Tier != "elite" || m.LiveBuys[0].WalletDisplay != "0x79f6…5c5d" || f(m.LiveBuys[3].AmountUSD) != 95.5 {
		t.Fatalf("buys = %+v", m.LiveBuys)
	}

	// re-parse (e.g. edited post) replaces the buys instead of appending
	edited := strings.Replace(samplePost, "✅ $95.5 · 0x9999…aaaa\n", "", 1)
	mm, _ := parseCallMeta(edited)
	if err := st.UpsertCallMetrics(ctx, *j.CallID, mm); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetCallMetrics(ctx, *j.CallID)
	if len(got.Meta.LiveBuys) != 3 || got.LiveBuysGoodCount != 1 {
		t.Fatalf("after re-upsert: %d buys, good=%d", len(got.Meta.LiveBuys), got.LiveBuysGoodCount)
	}

	// convenience view
	var sym string
	var holders, elite int
	if err := st.Pool.QueryRow(ctx, `SELECT token_symbol, holders, proof_elite FROM scout_calls_v WHERE call_id = $1`, *j.CallID).
		Scan(&sym, &holders, &elite); err != nil || sym != "MALFOID" || holders != 1150 || elite != 3 {
		t.Fatalf("view: %s %d %d %v", sym, holders, elite, err)
	}
	// a post that isn't a call has no metrics row
	if none, err := st.GetCallMetrics(ctx, 999999); none != nil || err != nil {
		t.Fatalf("missing metrics: %v %v", none, err)
	}
	// header shows the summary
	h := s.deliveryHeader(j, nil)
	if !strings.Contains(h, "MCap $52k") || !strings.Contains(h, "Proof 💎3 ✅5") {
		t.Fatalf("header:\n%s", h)
	}
}
