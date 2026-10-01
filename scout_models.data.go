package main

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed scoutanalytics.sql
var scoutSchemaSQL string

// ScoutStore is the data-access layer for the scout_* tables
// (tools, calls, investigations, deliveries).
type ScoutStore struct {
	Pool  *pgxpool.Pool
	owned bool // true when this store opened the pool (and must close it)
}

// NewScoutStoreFromPool wraps an existing pool, e.g. database.DbConnPgx from
// the repo's database package. Close() leaves a shared pool open.
func NewScoutStoreFromPool(pool *pgxpool.Pool) *ScoutStore {
	return &ScoutStore{Pool: pool}
}

// NewScoutStore connects to Postgres using a pgx DSN / URL, e.g.
// postgres://user:pass@host:5432/dbname?sslmode=disable
func NewScoutStore(ctx context.Context, dsn string) (*ScoutStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse SCOUT_DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &ScoutStore{Pool: pool, owned: true}, nil
}

func (st *ScoutStore) Close() {
	if st.owned {
		st.Pool.Close()
	}
}

// Describe returns "database / user" for startup logs.
func (st *ScoutStore) Describe(ctx context.Context) string {
	var db, user, schema string
	if err := st.Pool.QueryRow(ctx, `SELECT current_database(), current_user, current_schema()`).Scan(&db, &user, &schema); err != nil {
		return "?"
	}
	return fmt.Sprintf("database %q, schema %q, user %q", db, schema, user)
}

// Migrate creates the tables/indexes if they don't exist (idempotent).
func (st *ScoutStore) Migrate(ctx context.Context) error {
	_, err := st.Pool.Exec(ctx, scoutSchemaSQL)
	return err
}

// ---------------------------------------------------------------------------
// scout_calls
// ---------------------------------------------------------------------------

const scoutCallColumns = `
	id,               -- 1
	uuid,             -- 2
	channel_id,       -- 3
	channel_username, -- 4
	message_id,       -- 5
	message_date,     -- 6
	message_text,     -- 7
	urls,             -- 8
	contract_address, -- 9
	chain,            -- 10
	status,           -- 11
	created_by,       -- 12
	created_at,       -- 13
	updated_by,       -- 14
	updated_at        -- 15
`

func scanScoutCall(row pgx.Row) (*ScoutCall, error) {
	var c ScoutCall
	var id int
	var u uuid.UUID
	err := row.Scan(&id, &u, &c.ChannelID, &c.ChannelUsername, &c.MessageID, &c.MessageDate,
		&c.MessageText, &c.URLs, &c.ContractAddress, &c.Chain, &c.Status,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedBy, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.ID, c.UUID = &id, u.String()
	return &c, nil
}

// InsertScoutCall inserts a call and returns its id. If the same
// (channel, message, CA) already exists (e.g. after a restart or a re-run of
// -backfill), the existing id is returned instead.
func (st *ScoutStore) InsertScoutCall(ctx context.Context, c *ScoutCall) (*int, error) {
	id, _, err := st.UpsertScoutCall(ctx, c)
	return id, err
}

// UpsertScoutCall is InsertScoutCall that also reports whether a new row was
// created. A call is the same call when channel + post + CA match, with the CA
// compared case-insensitively (0xAbC… == 0xabc…). On a match, only the post
// text/links and updated_at are refreshed; status and everything else is kept.
func (st *ScoutStore) UpsertScoutCall(ctx context.Context, c *ScoutCall) (*int, bool, error) {
	now := time.Now().UTC()
	if c.UUID == "" {
		c.UUID = uuid.NewString()
	}
	if c.CreatedBy == "" {
		c.CreatedBy = scoutDBUser
	}
	if c.UpdatedBy == "" {
		c.UpdatedBy = c.CreatedBy
	}
	if c.URLs == nil {
		c.URLs = []string{}
	}
	var id int
	err := st.Pool.QueryRow(ctx, `UPDATE scout_calls SET message_text = $4, urls = $5, updated_at = $6
		WHERE id = (SELECT id FROM scout_calls
		            WHERE channel_id = $1 AND message_id = $2 AND lower(contract_address) = lower($3)
		            ORDER BY id LIMIT 1)
		RETURNING id`,
		c.ChannelID, c.MessageID, c.ContractAddress, c.MessageText, c.URLs, now).Scan(&id)
	if err == nil {
		c.ID = &id
		return &id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var created bool
	err = st.Pool.QueryRow(ctx, `INSERT INTO scout_calls (
		uuid, channel_id, channel_username, message_id, message_date, message_text,
		urls, contract_address, chain, status, created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT (channel_id, message_id, contract_address) DO UPDATE
		SET message_text = EXCLUDED.message_text, urls = EXCLUDED.urls, updated_at = EXCLUDED.updated_at
	RETURNING id, (xmax = 0)`,
		c.UUID, c.ChannelID, c.ChannelUsername, c.MessageID, c.MessageDate, c.MessageText,
		c.URLs, c.ContractAddress, c.Chain, c.Status, c.CreatedBy, now, c.UpdatedBy, now,
	).Scan(&id, &created)
	if err != nil {
		return nil, false, err
	}
	c.ID = &id
	return &id, created, nil
}

// UpdateScoutCallStatus sets the status of a call.
func (st *ScoutStore) UpdateScoutCallStatus(ctx context.Context, id int, status string) error {
	tag, err := st.Pool.Exec(ctx,
		`UPDATE scout_calls SET status = $2, updated_by = $3, updated_at = $4 WHERE id = $1`,
		id, status, scoutDBUser, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("scout_calls id %d not found", id)
	}
	return nil
}

// GetScoutCall returns one call by id (nil if not found).
func (st *ScoutStore) GetScoutCall(ctx context.Context, id int) (*ScoutCall, error) {
	c, err := scanScoutCall(st.Pool.QueryRow(ctx,
		`SELECT `+scoutCallColumns+` FROM scout_calls WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// SelectScoutCalls returns the latest calls, optionally for one CA.
func (st *ScoutStore) SelectScoutCalls(ctx context.Context, contractAddress string, limit int) ([]ScoutCall, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + scoutCallColumns + ` FROM scout_calls`
	args := []any{}
	if contractAddress != "" {
		q += ` WHERE lower(contract_address) = lower($1)`
		args = append(args, contractAddress)
	}
	q += fmt.Sprintf(` ORDER BY message_date DESC, id DESC LIMIT %d`, limit)
	rows, err := st.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutCall
	for rows.Next() {
		c, err := scanScoutCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scout_investigation_tools
// ---------------------------------------------------------------------------

const scoutToolColumns = `
	id,               -- 1
	code,             -- 2
	name,             -- 3
	bot_username,     -- 4
	command_template, -- 5
	parser,           -- 6
	is_gate,          -- 7
	is_active,        -- 8
	created_by,       -- 9
	created_at,       -- 10
	updated_by,       -- 11
	updated_at        -- 12
`

// UpsertInvestigationTool inserts or updates a tool by code and returns its id.
func (st *ScoutStore) UpsertInvestigationTool(ctx context.Context, t *ScoutInvestigationTool) (*int, error) {
	now := time.Now().UTC()
	if t.CreatedBy == "" {
		t.CreatedBy = scoutDBUser
	}
	if t.UpdatedBy == "" {
		t.UpdatedBy = t.CreatedBy
	}
	var id int
	err := st.Pool.QueryRow(ctx, `INSERT INTO scout_investigation_tools (
		code, name, bot_username, command_template, parser, is_gate, is_active,
		created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	ON CONFLICT (code) DO UPDATE SET
		name = EXCLUDED.name, bot_username = EXCLUDED.bot_username,
		command_template = EXCLUDED.command_template, parser = EXCLUDED.parser,
		is_gate = EXCLUDED.is_gate, is_active = EXCLUDED.is_active,
		updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at
	RETURNING id`,
		t.Code, t.Name, t.BotUsername, t.CommandTemplate, t.Parser, t.IsGate, t.IsActive,
		t.CreatedBy, now, t.UpdatedBy, now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	t.ID = &id
	return &id, nil
}

// SetActiveInvestigationTools marks the given codes active and all others inactive.
func (st *ScoutStore) SetActiveInvestigationTools(ctx context.Context, codes []string) error {
	_, err := st.Pool.Exec(ctx, `UPDATE scout_investigation_tools
		SET is_active = (code = ANY($1)), updated_by = $2, updated_at = $3
		WHERE is_active <> (code = ANY($1))`, codes, scoutDBUser, time.Now().UTC())
	return err
}

// SelectInvestigationTools lists tools (optionally only active ones).
func (st *ScoutStore) SelectInvestigationTools(ctx context.Context, activeOnly bool) ([]ScoutInvestigationTool, error) {
	q := `SELECT ` + scoutToolColumns + ` FROM scout_investigation_tools`
	if activeOnly {
		q += ` WHERE is_active`
	}
	rows, err := st.Pool.Query(ctx, q+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutInvestigationTool
	for rows.Next() {
		var t ScoutInvestigationTool
		var id int
		if err := rows.Scan(&id, &t.Code, &t.Name, &t.BotUsername, &t.CommandTemplate, &t.Parser,
			&t.IsGate, &t.IsActive, &t.CreatedBy, &t.CreatedAt, &t.UpdatedBy, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.ID = &id
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scout_investigations
// ---------------------------------------------------------------------------

const scoutInvestigationColumns = `
	i.id,               -- 1
	i.uuid,             -- 2
	i.call_id,          -- 3
	i.tool_id,          -- 4
	i.contract_address, -- 5
	i.request_text,     -- 6
	i.requested_at,     -- 7
	i.completed_at,     -- 8
	i.status,           -- 9
	i.bot_message_ids,  -- 10
	i.report_text,      -- 11
	i.report_urls,      -- 12
	i.report_url,       -- 13
	i.external_id,      -- 14
	i.verdict_level,    -- 15
	i.verdict_label,    -- 16
	i.ticker,           -- 17
	i.verdict_summary,  -- 18
	i.verdict_source,   -- 19
	i.details,          -- 20
	i.error,            -- 21
	i.created_by,       -- 22
	i.created_at,       -- 23
	i.updated_by,       -- 24
	i.updated_at,       -- 25
	t.code              -- tool code (join)
`

func scanScoutInvestigation(row pgx.Row) (*ScoutInvestigation, error) {
	var r ScoutInvestigation
	var id int
	var u uuid.UUID
	var details []byte
	err := row.Scan(&id, &u, &r.CallID, &r.ToolID, &r.ContractAddress, &r.RequestText,
		&r.RequestedAt, &r.CompletedAt, &r.Status, &r.BotMessageIDs, &r.ReportText, &r.ReportURLs,
		&r.ReportURL, &r.ExternalID, &r.VerdictLevel, &r.VerdictLabel, &r.Ticker,
		&r.VerdictSummary, &r.VerdictSource, &details, &r.Error,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedBy, &r.UpdatedAt, &r.ToolCode)
	if err != nil {
		return nil, err
	}
	r.ID, r.UUID, r.Details = &id, u.String(), details
	return &r, nil
}

// InsertScoutInvestigation inserts one tool's result and returns its id.
func (st *ScoutStore) InsertScoutInvestigation(ctx context.Context, r *ScoutInvestigation) (*int, error) {
	now := time.Now().UTC()
	if r.UUID == "" {
		r.UUID = uuid.NewString()
	}
	if r.CreatedBy == "" {
		r.CreatedBy = scoutDBUser
	}
	if r.UpdatedBy == "" {
		r.UpdatedBy = r.CreatedBy
	}
	if r.BotMessageIDs == nil {
		r.BotMessageIDs = []int32{}
	}
	if r.ReportURLs == nil {
		r.ReportURLs = []string{}
	}
	if r.VerdictLevel == "" {
		r.VerdictLevel = levelUnknown
	}
	if len(r.Details) == 0 {
		r.Details = []byte("{}")
	}
	var id int
	err := st.Pool.QueryRow(ctx, `INSERT INTO scout_investigations (
		uuid, call_id, tool_id, contract_address, request_text, requested_at, completed_at, status,
		bot_message_ids, report_text, report_urls, report_url, external_id, verdict_level,
		verdict_label, ticker, verdict_summary, verdict_source, details, error,
		created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb,$20,$21,$22,$23,$24)
	RETURNING id`,
		r.UUID, r.CallID, r.ToolID, r.ContractAddress, r.RequestText, r.RequestedAt, r.CompletedAt, r.Status,
		r.BotMessageIDs, r.ReportText, r.ReportURLs, r.ReportURL, r.ExternalID, r.VerdictLevel,
		r.VerdictLabel, r.Ticker, r.VerdictSummary, r.VerdictSource, string(r.Details), r.Error,
		r.CreatedBy, now, r.UpdatedBy, now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	r.ID = &id
	return &id, nil
}

// GetScoutInvestigation returns one investigation by id (nil if not found).
func (st *ScoutStore) GetScoutInvestigation(ctx context.Context, id int) (*ScoutInvestigation, error) {
	r, err := scanScoutInvestigation(st.Pool.QueryRow(ctx, `SELECT `+scoutInvestigationColumns+`
		FROM scout_investigations i JOIN scout_investigation_tools t ON t.id = i.tool_id
		WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ScoutInvestigationFilter narrows SelectScoutInvestigations. Zero values mean "any".
type ScoutInvestigationFilter struct {
	ContractAddress string
	CallID          *int
	ToolCodes       []string // e.g. {"perceptor"}
	VerdictLevels   []string // e.g. {"clean","caution"}
	Statuses        []string // e.g. {"completed"}
	Since           *time.Time
	Limit           int // default 100
}

// SelectScoutInvestigations returns investigations newest first.
func (st *ScoutStore) SelectScoutInvestigations(ctx context.Context, f ScoutInvestigationFilter) ([]ScoutInvestigation, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.ContractAddress != "" {
		add("lower(i.contract_address) = lower($%d)", f.ContractAddress)
	}
	if f.CallID != nil {
		add("i.call_id = $%d", *f.CallID)
	}
	if len(f.ToolCodes) > 0 {
		add("t.code = ANY($%d)", f.ToolCodes)
	}
	if len(f.VerdictLevels) > 0 {
		add("i.verdict_level = ANY($%d)", f.VerdictLevels)
	}
	if len(f.Statuses) > 0 {
		add("i.status = ANY($%d)", f.Statuses)
	}
	if f.Since != nil {
		add("i.requested_at >= $%d", *f.Since)
	}
	q := `SELECT ` + scoutInvestigationColumns + `
		FROM scout_investigations i JOIN scout_investigation_tools t ON t.id = i.tool_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += fmt.Sprintf(" ORDER BY i.requested_at DESC, i.id DESC LIMIT %d", limit)

	rows, err := st.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutInvestigation
	for rows.Next() {
		r, err := scanScoutInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scout_deliveries (+ scout_delivery_investigations)
// ---------------------------------------------------------------------------

// InsertScoutDelivery records a delivery and links the attached investigations,
// in one transaction.
func (st *ScoutStore) InsertScoutDelivery(ctx context.Context, d *ScoutDelivery) (*int, error) {
	now := time.Now().UTC()
	if d.UUID == "" {
		d.UUID = uuid.NewString()
	}
	if d.CreatedBy == "" {
		d.CreatedBy = scoutDBUser
	}
	if d.UpdatedBy == "" {
		d.UpdatedBy = d.CreatedBy
	}
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO scout_deliveries (
		uuid, call_id, contract_address, target, status, header_text, delivered_at, error,
		created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		d.UUID, d.CallID, d.ContractAddress, d.Target, d.Status, d.HeaderText, d.DeliveredAt, d.Error,
		d.CreatedBy, now, d.UpdatedBy, now,
	).Scan(&id); err != nil {
		return nil, err
	}
	for _, iid := range d.InvestigationIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO scout_delivery_investigations (delivery_id, investigation_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, iid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	d.ID = &id
	return &id, nil
}

// SelectScoutDeliveries returns deliveries newest first, optionally for one call.
func (st *ScoutStore) SelectScoutDeliveries(ctx context.Context, callID *int, limit int) ([]ScoutDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT d.id, d.uuid, d.call_id, d.contract_address, d.target, d.status, d.header_text,
		d.delivered_at, d.error, d.created_by, d.created_at, d.updated_by, d.updated_at,
		COALESCE(array_agg(di.investigation_id ORDER BY di.investigation_id)
			FILTER (WHERE di.investigation_id IS NOT NULL), '{}')
	FROM scout_deliveries d
	LEFT JOIN scout_delivery_investigations di ON di.delivery_id = d.id`
	args := []any{}
	if callID != nil {
		q += ` WHERE d.call_id = $1`
		args = append(args, *callID)
	}
	q += fmt.Sprintf(` GROUP BY d.id ORDER BY d.created_at DESC, d.id DESC LIMIT %d`, limit)
	rows, err := st.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutDelivery
	for rows.Next() {
		var d ScoutDelivery
		var id int
		var u uuid.UUID
		var ids []int32
		if err := rows.Scan(&id, &u, &d.CallID, &d.ContractAddress, &d.Target, &d.Status, &d.HeaderText,
			&d.DeliveredAt, &d.Error, &d.CreatedBy, &d.CreatedAt, &d.UpdatedBy, &d.UpdatedAt, &ids); err != nil {
			return nil, err
		}
		d.ID, d.UUID = &id, u.String()
		for _, x := range ids {
			d.InvestigationIDs = append(d.InvestigationIDs, int(x))
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scout_call_metrics + scout_call_live_buys
// ---------------------------------------------------------------------------

// UpsertCallMetrics stores the parsed post data for a call and replaces its
// live-buy rows, in one transaction.
func (st *ScoutStore) UpsertCallMetrics(ctx context.Context, callID int, m *CallMeta) error {
	if m == nil {
		return nil
	}
	parsed, err := json.Marshal(m)
	if err != nil {
		return err
	}
	en, gn, eu, gu := m.LiveBuyTotals()
	now := time.Now().UTC()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO scout_call_metrics (
		call_id, token_symbol, chain_name, called_at_mcap_usd, dex, mcap_usd, liq_usd, liq_pct,
		tax_buy_pct, tax_sell_pct, age_text, age_seconds, launchpad, holders, proof_elite, proof_good,
		live_buys_elite_count, live_buys_good_count, live_buys_elite_usd, live_buys_good_usd, parsed,
		created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21::jsonb,$22,$23,$22,$23)
	ON CONFLICT (call_id) DO UPDATE SET
		token_symbol = EXCLUDED.token_symbol, chain_name = EXCLUDED.chain_name,
		called_at_mcap_usd = EXCLUDED.called_at_mcap_usd, dex = EXCLUDED.dex, mcap_usd = EXCLUDED.mcap_usd,
		liq_usd = EXCLUDED.liq_usd, liq_pct = EXCLUDED.liq_pct, tax_buy_pct = EXCLUDED.tax_buy_pct,
		tax_sell_pct = EXCLUDED.tax_sell_pct, age_text = EXCLUDED.age_text, age_seconds = EXCLUDED.age_seconds,
		launchpad = EXCLUDED.launchpad, holders = EXCLUDED.holders, proof_elite = EXCLUDED.proof_elite,
		proof_good = EXCLUDED.proof_good, live_buys_elite_count = EXCLUDED.live_buys_elite_count,
		live_buys_good_count = EXCLUDED.live_buys_good_count, live_buys_elite_usd = EXCLUDED.live_buys_elite_usd,
		live_buys_good_usd = EXCLUDED.live_buys_good_usd, parsed = EXCLUDED.parsed,
		updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		callID, m.TokenSymbol, m.ChainName, m.CalledAtMcapUSD, m.Dex, m.McapUSD, m.LiqUSD, m.LiqPct,
		m.TaxBuyPct, m.TaxSellPct, m.AgeText, m.AgeSeconds, m.Launchpad, m.Holders, m.ProofElite, m.ProofGood,
		en, gn, eu, gu, string(parsed), scoutDBUser, now,
	); err != nil {
		return fmt.Errorf("scout_call_metrics: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM scout_call_live_buys WHERE call_id = $1`, callID); err != nil {
		return err
	}
	for _, b := range m.LiveBuys {
		if _, err := tx.Exec(ctx, `INSERT INTO scout_call_live_buys
			(call_id, position, tier, tier_emoji, amount_usd, wallet_display, wallet_prefix, wallet_suffix)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			callID, b.Position, b.Tier, b.TierEmoji, b.AmountUSD, b.WalletDisplay, b.WalletPrefix, b.WalletSuffix); err != nil {
			return fmt.Errorf("scout_call_live_buys: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// GetCallMetrics returns the parsed post data for a call, with its live buys
// (nil if the call has none).
func (st *ScoutStore) GetCallMetrics(ctx context.Context, callID int) (*ScoutCallMetrics, error) {
	var r ScoutCallMetrics
	m := &r.Meta
	var parsed []byte
	err := st.Pool.QueryRow(ctx, `SELECT call_id, token_symbol, chain_name, called_at_mcap_usd::float8, dex,
		mcap_usd::float8, liq_usd::float8, liq_pct::float8, tax_buy_pct::float8, tax_sell_pct::float8,
		age_text, age_seconds, launchpad, holders, proof_elite, proof_good,
		live_buys_elite_count, live_buys_good_count, live_buys_elite_usd::float8, live_buys_good_usd::float8,
		parsed, created_at, updated_at
		FROM scout_call_metrics WHERE call_id = $1`, callID).Scan(
		&r.CallID, &m.TokenSymbol, &m.ChainName, &m.CalledAtMcapUSD, &m.Dex,
		&m.McapUSD, &m.LiqUSD, &m.LiqPct, &m.TaxBuyPct, &m.TaxSellPct,
		&m.AgeText, &m.AgeSeconds, &m.Launchpad, &m.Holders, &m.ProofElite, &m.ProofGood,
		&r.LiveBuysEliteCount, &r.LiveBuysGoodCount, &r.LiveBuysEliteUSD, &r.LiveBuysGoodUSD,
		&parsed, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Parsed = parsed
	rows, err := st.Pool.Query(ctx, `SELECT position, tier, tier_emoji, amount_usd::float8, wallet_display,
		wallet_prefix, wallet_suffix FROM scout_call_live_buys WHERE call_id = $1 ORDER BY position`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b LiveBuy
		if err := rows.Scan(&b.Position, &b.Tier, &b.TierEmoji, &b.AmountUSD, &b.WalletDisplay,
			&b.WalletPrefix, &b.WalletSuffix); err != nil {
			return nil, err
		}
		m.LiveBuys = append(m.LiveBuys, b)
	}
	return &r, rows.Err()
}

// ---------------------------------------------------------------------------
// scout_call_tracking + scout_call_returns
// ---------------------------------------------------------------------------

// EnsureTracking creates the tracking row for a call (no-op if it exists).
func (st *ScoutStore) EnsureTracking(ctx context.Context, callID int, ca string, entryAt time.Time, priority int, firstCheck time.Time) error {
	_, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_tracking
		(call_id, contract_address, entry_at, priority, status, next_check_at)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (call_id) DO NOTHING`,
		callID, ca, entryAt.UTC(), priority, TrackPending, firstCheck.UTC())
	return err
}

const trackingColumns = `call_id, contract_address, entry_at, priority, status, pool_address, pool_name, pool_dex,
	pool_created_at, entry_price_usd::float8, entry_price_source, current_price_usd::float8,
	current_liquidity_usd::float8, rugged, next_check_at, last_checked_at, attempts, error`

func scanTracking(row pgx.Row) (*ScoutCallTracking, error) {
	var t ScoutCallTracking
	err := row.Scan(&t.CallID, &t.ContractAddress, &t.EntryAt, &t.Priority, &t.Status, &t.PoolAddress, &t.PoolName,
		&t.PoolDex, &t.PoolCreatedAt, &t.EntryPriceUSD, &t.EntryPriceSource, &t.CurrentPriceUSD,
		&t.CurrentLiquidityUSD, &t.Rugged, &t.NextCheckAt, &t.LastCheckedAt, &t.Attempts, &t.Error)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DueTracking returns calls whose next price check is due: live calls first.
func (st *ScoutStore) DueTracking(ctx context.Context, now time.Time, limit int) ([]ScoutCallTracking, error) {
	rows, err := st.Pool.Query(ctx, `SELECT `+trackingColumns+` FROM scout_call_tracking
		WHERE status IN ('pending','tracking','no_pool','error') AND next_check_at <= $1
		ORDER BY priority, next_check_at LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutCallTracking
	for rows.Next() {
		t, err := scanTracking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetTracking returns the tracking row of a call (nil if none).
func (st *ScoutStore) GetTracking(ctx context.Context, callID int) (*ScoutCallTracking, error) {
	t, err := scanTracking(st.Pool.QueryRow(ctx, `SELECT `+trackingColumns+` FROM scout_call_tracking WHERE call_id = $1`, callID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// KnownPool returns a pool already chosen for this CA by another call (saves an API call).
func (st *ScoutStore) KnownPool(ctx context.Context, ca string) (*ScoutCallTracking, error) {
	t, err := scanTracking(st.Pool.QueryRow(ctx, `SELECT `+trackingColumns+` FROM scout_call_tracking
		WHERE lower(contract_address) = lower($1) AND pool_address IS NOT NULL
		ORDER BY updated_at DESC LIMIT 1`, ca))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// SaveTracking writes the mutable fields of a tracking row.
func (st *ScoutStore) SaveTracking(ctx context.Context, t *ScoutCallTracking) error {
	_, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET
		status = $2, pool_address = $3, pool_name = $4, pool_dex = $5, pool_created_at = $6,
		entry_price_usd = $7, entry_price_source = $8, current_price_usd = $9, current_liquidity_usd = $10,
		rugged = $11, next_check_at = $12, last_checked_at = $13, attempts = $14, error = $15, updated_at = now()
		WHERE call_id = $1`,
		t.CallID, t.Status, t.PoolAddress, t.PoolName, t.PoolDex, t.PoolCreatedAt,
		t.EntryPriceUSD, t.EntryPriceSource, t.CurrentPriceUSD, t.CurrentLiquidityUSD,
		t.Rugged, t.NextCheckAt.UTC(), t.LastCheckedAt, t.Attempts, t.Error)
	return err
}

// UpsertReturn stores the result for one horizon.
func (st *ScoutStore) UpsertReturn(ctx context.Context, callID int, h horizon, r horizonResult) error {
	nz := func(v float64) *float64 {
		if r.Status != "done" {
			return nil
		}
		return &v
	}
	_, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_returns
		(call_id, horizon, horizon_seconds, due_at, status, price_usd, return_pct, max_gain_pct, max_drawdown_pct,
		 max_price_usd, min_price_usd, last_trade_at, computed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now())
		ON CONFLICT (call_id, horizon) DO UPDATE SET
		 horizon_seconds = EXCLUDED.horizon_seconds, due_at = EXCLUDED.due_at, status = EXCLUDED.status,
		 price_usd = EXCLUDED.price_usd, return_pct = EXCLUDED.return_pct, max_gain_pct = EXCLUDED.max_gain_pct,
		 max_drawdown_pct = EXCLUDED.max_drawdown_pct, max_price_usd = EXCLUDED.max_price_usd,
		 min_price_usd = EXCLUDED.min_price_usd, last_trade_at = EXCLUDED.last_trade_at, computed_at = now()`,
		callID, h.Name, int(h.Dur/time.Second), r.DueAt.UTC(), r.Status, nz(r.PriceUSD), nz(r.ReturnPct),
		nz(r.MaxGainPct), nz(r.MaxDDPct), nz(r.MaxPriceUSD), nz(r.MinPriceUSD), r.LastTradeAt)
	return err
}

// ReturnsForCall returns the stored horizon results of a call, by horizon name.
func (st *ScoutStore) ReturnsForCall(ctx context.Context, callID int) (map[string]horizonResult, error) {
	rows, err := st.Pool.Query(ctx, `SELECT horizon, due_at, status, COALESCE(price_usd,0)::float8,
		COALESCE(return_pct,0)::float8, COALESCE(max_gain_pct,0)::float8, COALESCE(max_drawdown_pct,0)::float8,
		COALESCE(max_price_usd,0)::float8, COALESCE(min_price_usd,0)::float8, last_trade_at
		FROM scout_call_returns WHERE call_id = $1`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]horizonResult{}
	for rows.Next() {
		var r horizonResult
		if err := rows.Scan(&r.Horizon, &r.DueAt, &r.Status, &r.PriceUSD, &r.ReturnPct, &r.MaxGainPct,
			&r.MaxDDPct, &r.MaxPriceUSD, &r.MinPriceUSD, &r.LastTradeAt); err != nil {
			return nil, err
		}
		out[r.Horizon] = r
	}
	return out, rows.Err()
}

// ExportDatasetCSV writes scout_call_dataset_v (one row per call) as CSV.
func (st *ScoutStore) ExportDatasetCSV(ctx context.Context, w io.Writer) (int, error) {
	rows, err := st.Pool.Query(ctx, `SELECT * FROM scout_call_dataset_v ORDER BY message_date, call_id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cw := csv.NewWriter(w)
	var header []string
	for _, f := range rows.FieldDescriptions() {
		header = append(header, f.Name)
	}
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return n, err
		}
		rec := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				rec[i] = ""
			case time.Time:
				rec[i] = x.UTC().Format(time.RFC3339)
			case float64:
				rec[i] = strconv.FormatFloat(x, 'f', -1, 64)
			default:
				rec[i] = fmt.Sprint(x)
			}
		}
		if err := cw.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return n, err
	}
	return n, rows.Err()
}
