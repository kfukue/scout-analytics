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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed scoutanalytics.sql
var scoutSchemaSQL string

// ScoutStore is the data-access layer for the scout_* tables
// (tools, calls, investigations, deliveries).
type ScoutStore struct {
	Pool  *pgxpool.Pool
	owned bool // true when this store opened the pool (and must close it)

	// MaxConnsFrom says where the pool's MaxConns came from, for the startup
	// log ("pool_max_conns in the DSN" or "default for N tracker workers").
	MaxConnsFrom string
}

// NewScoutStoreFromPool wraps an existing pool, e.g. database.DbConnPgx from
// the repo's database package. Close() leaves a shared pool open.
func NewScoutStoreFromPool(pool *pgxpool.Pool) *ScoutStore {
	return &ScoutStore{Pool: pool}
}

// Pool size. pgxpool opens connections only when they are needed (MinConns
// is 0), so MaxConns is a ceiling: a process holds about as many connections
// as it uses at the same time, and a listener or website process with a high
// ceiling still opens only a few.
//
// A tracker worker holds a connection for one query at a time, never across
// node requests (no transaction in the tracker or latest-price path); the
// latest-price pass runs on the same workers. On top of the workers come the
// dispatcher's reads and the cycle's own queries (status, token names,
// Chainlink feeds), which run one at a time, and in the default mode (no
// -listen-only) the listener's scans and tool results in the same process.
// The website's LISTEN connection is dialled separately, outside the pool.
const (
	poolConnsHeadroom = 4  // dispatcher and cycle queries, listener in the same process, slack
	poolConnsMin      = 4  // the old fixed size
	poolConnsCap      = 32 // keeps one process well below Postgres' default max_connections (100)
)

// defaultPoolMaxConns is the pool size used when the DSN does not set
// pool_max_conns: the tracker workers plus headroom, at least poolConnsMin
// and at most poolConnsCap.
func defaultPoolMaxConns(workers int) int32 {
	return int32(min(max(workers+poolConnsHeadroom, poolConnsMin), poolConnsCap))
}

// applyPoolMaxConns sets cfg.MaxConns to defaultPoolMaxConns(workers) unless
// dsn sets pool_max_conns itself (pgxpool has already applied it to cfg), and
// returns where the value came from.
func applyPoolMaxConns(dsn string, cfg *pgxpool.Config, workers int) (string, error) {
	// pgxpool.ParseConfig removes pool_max_conns from the runtime parameters,
	// so parse once more to see whether the DSN sets it. Comparing cfg.MaxConns
	// with pgxpool's default would miss an explicit value equal to it.
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse SCOUT_DATABASE_URL: %w", err)
	}
	if _, ok := cc.RuntimeParams["pool_max_conns"]; ok {
		return "pool_max_conns in SCOUT_DATABASE_URL", nil
	}
	cfg.MaxConns = defaultPoolMaxConns(workers)
	return defaultPoolSource(workers), nil
}

// defaultPoolSource is the startup log's note for a pool sized by
// defaultPoolMaxConns(workers).
func defaultPoolSource(workers int) string {
	return fmt.Sprintf("default for %d tracker worker(s)", workers)
}

// NewScoutStore connects to Postgres using a pgx DSN / URL, e.g.
// postgres://user:pass@host:5432/dbname?sslmode=disable. workers is the
// tracker's effective worker count (priceConfig.trackWorkers); it sizes the
// pool unless the DSN sets pool_max_conns.
func NewScoutStore(ctx context.Context, dsn string, workers int) (*ScoutStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse SCOUT_DATABASE_URL: %w", err)
	}
	from, err := applyPoolMaxConns(dsn, cfg, workers)
	if err != nil {
		return nil, err
	}
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
	return &ScoutStore{Pool: pool, owned: true, MaxConnsFrom: from}, nil
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
	updated_at,       -- 15
	COALESCE(post_kind, '') -- 16
`

func scanScoutCall(row pgx.Row) (*ScoutCall, error) {
	var c ScoutCall
	var id int
	var u uuid.UUID
	err := row.Scan(&id, &u, &c.ChannelID, &c.ChannelUsername, &c.MessageID, &c.MessageDate,
		&c.MessageText, &c.URLs, &c.ContractAddress, &c.Chain, &c.Status,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedBy, &c.UpdatedAt, &c.PostKind)
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
// text/links, post_kind and updated_at are refreshed; status and everything else
// is kept. post_kind is always worked out from the post text (postKind).
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
	c.PostKind = postKind(c.MessageText)
	var id int
	err := st.Pool.QueryRow(ctx, `UPDATE scout_calls SET message_text = $4, urls = $5, updated_at = $6, post_kind = $7
		WHERE id = (SELECT id FROM scout_calls
		            WHERE channel_id = $1 AND message_id = $2 AND lower(contract_address) = lower($3)
		            ORDER BY id LIMIT 1)
		RETURNING id`,
		c.ChannelID, c.MessageID, c.ContractAddress, c.MessageText, c.URLs, now, c.PostKind).Scan(&id)
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
		urls, contract_address, chain, status, created_by, created_at, updated_by, updated_at, post_kind
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	ON CONFLICT (channel_id, message_id, contract_address) DO UPDATE
		SET message_text = EXCLUDED.message_text, urls = EXCLUDED.urls, updated_at = EXCLUDED.updated_at,
		    post_kind = EXCLUDED.post_kind
	RETURNING id, (xmax = 0)`,
		c.UUID, c.ChannelID, c.ChannelUsername, c.MessageID, c.MessageDate, c.MessageText,
		c.URLs, c.ContractAddress, c.Chain, c.Status, c.CreatedBy, now, c.UpdatedBy, now, c.PostKind,
	).Scan(&id, &created)
	if err != nil {
		return nil, false, err
	}
	c.ID = &id
	if created && c.PostKind == PostKindCall && c.Status != CallStatusBackfill {
		// tells an open website (-web) at once; never fails the insert. Posts
		// imported from channel history (-backfill) are old news: they reach
		// the page with its next regular refresh, without a refresh per row.
		st.notifyScoutEvent(ctx, scoutEvent{Kind: scoutEventCall, CallID: id})
	}
	return &id, created, nil
}

// ClassifyPostKinds fills post_kind for the rows that do not have one yet (rows
// stored before the column existed), from their stored message_text and with
// the same rule as new posts (postKind). It works in batches and returns how
// many rows became 'call' and 'update'. When no row is left it is one small
// index lookup and writes nothing. Only post_kind is written: the status of
// those rows stays as it was.
func (st *ScoutStore) ClassifyPostKinds(ctx context.Context) (calls, updates int, err error) {
	const batch = 1000
	for {
		rows, err := st.Pool.Query(ctx, `SELECT id, message_text FROM scout_calls WHERE post_kind IS NULL ORDER BY id LIMIT $1`, batch)
		if err != nil {
			return calls, updates, err
		}
		var ids []int32
		var kinds []string
		for rows.Next() {
			var id int32
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return calls, updates, err
			}
			ids, kinds = append(ids, id), append(kinds, postKind(text))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return calls, updates, err
		}
		if len(ids) == 0 {
			return calls, updates, nil
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE scout_calls c SET post_kind = v.kind
			FROM unnest($1::int[], $2::text[]) AS v(id, kind) WHERE c.id = v.id AND c.post_kind IS NULL`, ids, kinds); err != nil {
			return calls, updates, err
		}
		for _, k := range kinds {
			if k == PostKindUpdate {
				updates++
			} else {
				calls++
			}
		}
		if len(ids) < batch {
			return calls, updates, nil
		}
	}
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

// MaxScoutCallMessageID returns the newest post id recorded for a channel;
// ok is false when the channel has no rows. The listener resumes from it.
func (st *ScoutStore) MaxScoutCallMessageID(ctx context.Context, channelID int64) (id int, ok bool, err error) {
	var newest *int
	if err := st.Pool.QueryRow(ctx, `SELECT MAX(message_id) FROM scout_calls WHERE channel_id = $1`, channelID).Scan(&newest); err != nil {
		return 0, false, fmt.Errorf("reading the newest post id of channel %d: %w", channelID, err)
	}
	if newest == nil {
		return 0, false, nil
	}
	return *newest, true, nil
}

// SelectScoutCallPostCAs returns the (post, CA) pairs recorded for a channel
// with a post id above afterID, so a restarted listener does not handle them again.
func (st *ScoutStore) SelectScoutCallPostCAs(ctx context.Context, channelID int64, afterID int) ([]ScoutCallPostCA, error) {
	rows, err := st.Pool.Query(ctx, `SELECT message_id, contract_address FROM scout_calls
		WHERE channel_id = $1 AND message_id > $2 ORDER BY message_id, id`, channelID, afterID)
	if err != nil {
		return nil, fmt.Errorf("reading posts of channel %d after %d: %w", channelID, afterID, err)
	}
	defer rows.Close()
	var out []ScoutCallPostCA
	for rows.Next() {
		var p ScoutCallPostCA
		if err := rows.Scan(&p.MessageID, &p.ContractAddress); err != nil {
			return nil, fmt.Errorf("scanning a post of channel %d: %w", channelID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading posts of channel %d after %d: %w", channelID, afterID, err)
	}
	return out, nil
}

// SelectScoutCallsToRequeue returns a channel's calls with status queued or
// dropped (the job queue was full) that have no completed investigation and
// were posted at or after since, oldest first. A restarted listener queues
// them again (see requeue in catchup.go). older is the number of such calls
// posted before since: they are left as they are (fixed by hand if needed).
func (st *ScoutStore) SelectScoutCallsToRequeue(ctx context.Context, channelID int64, since time.Time) (calls []ScoutCallToRequeue, older int, err error) {
	// the same predicates in both queries; only the date test differs
	const stuck = `c.channel_id = $1 AND c.status IN ($2, $3)
		  AND NOT EXISTS (SELECT 1 FROM scout_investigations i WHERE i.call_id = c.id AND i.status = $4)`
	rows, err := st.Pool.Query(ctx, `SELECT c.id, c.message_id, c.message_date, c.message_text, c.contract_address, c.status,
			EXISTS (SELECT 1 FROM scout_investigations o
				WHERE o.status = $4 AND (o.contract_address = c.contract_address
					OR (c.contract_address LIKE '0x%' AND lower(o.contract_address) = lower(c.contract_address))))
		FROM scout_calls c
		WHERE `+stuck+` AND c.message_date >= $5
		ORDER BY c.message_id, c.id`, channelID, CallStatusQueued, CallStatusDropped, investigationCompleted, since)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the calls of channel %d to queue again: %w", channelID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c ScoutCallToRequeue
		if err := rows.Scan(&c.ID, &c.MessageID, &c.MessageDate, &c.MessageText, &c.ContractAddress, &c.Status, &c.CAInvestigated); err != nil {
			return nil, 0, fmt.Errorf("scanning a call of channel %d to queue again: %w", channelID, err)
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading the calls of channel %d to queue again: %w", channelID, err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM scout_calls c WHERE `+stuck+` AND c.message_date < $5`,
		channelID, CallStatusQueued, CallStatusDropped, investigationCompleted, since).Scan(&older); err != nil {
		return nil, 0, fmt.Errorf("counting the older stuck calls of channel %d: %w", channelID, err)
	}
	return calls, older, nil
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
	var toolCode *string // the code of the tool, for the website's notice
	err := st.Pool.QueryRow(ctx, `WITH ins AS (
	INSERT INTO scout_investigations (
		uuid, call_id, tool_id, contract_address, request_text, requested_at, completed_at, status,
		bot_message_ids, report_text, report_urls, report_url, external_id, verdict_level,
		verdict_label, ticker, verdict_summary, verdict_source, details, error,
		created_by, created_at, updated_by, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb,$20,$21,$22,$23,$24)
	RETURNING id, tool_id)
	SELECT ins.id, t.code FROM ins LEFT JOIN scout_investigation_tools t ON t.id = ins.tool_id`,
		r.UUID, r.CallID, r.ToolID, r.ContractAddress, r.RequestText, r.RequestedAt, r.CompletedAt, r.Status,
		r.BotMessageIDs, r.ReportText, r.ReportURLs, r.ReportURL, r.ExternalID, r.VerdictLevel,
		r.VerdictLabel, r.Ticker, r.VerdictSummary, r.VerdictSource, string(r.Details), r.Error,
		r.CreatedBy, now, r.UpdatedBy, now,
	).Scan(&id, &toolCode)
	if err != nil {
		return nil, err
	}
	r.ID = &id
	if ev, ok := scoutReportEvent(r, toolCode); ok {
		// tells an open website (-web) at once; never fails the insert
		st.notifyScoutEvent(ctx, ev)
	}
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
	current_liquidity_usd::float8, rugged, next_check_at, last_checked_at, attempts, error, price_unit, onchain,
	entry_late_price_usd::float8`

// trackingColumnsT is trackingColumns for a query that calls the table t.
var trackingColumnsT = func() string {
	cols := strings.Split(trackingColumns, ",")
	for i, c := range cols {
		cols[i] = "t." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}()

func scanTracking(row pgx.Row) (*ScoutCallTracking, error) {
	var t ScoutCallTracking
	var onchain []byte
	err := row.Scan(&t.CallID, &t.ContractAddress, &t.EntryAt, &t.Priority, &t.Status, &t.PoolAddress, &t.PoolName,
		&t.PoolDex, &t.PoolCreatedAt, &t.EntryPriceUSD, &t.EntryPriceSource, &t.CurrentPriceUSD,
		&t.CurrentLiquidityUSD, &t.Rugged, &t.NextCheckAt, &t.LastCheckedAt, &t.Attempts, &t.Error,
		&t.PriceUnit, &onchain, &t.EntryLatePriceUSD)
	if err != nil {
		return nil, err
	}
	t.Onchain = onchain
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

// MarkRepeatTracking makes the tracker follow only the first call of each token
// (the website's rule: lowest message_date, then lowest scout_calls.id, contract
// address compared without regard to letter case; update posts are not calls
// and are never a first call). In one statement it
//   - sets status 'repeat' (and clears error) on every tracking row whose call is
//     not the first call of its token and is still pending, tracking, no_pool,
//     error or gave_up; 'done' rows keep their status and results;
//   - sets status 'repeat' on every tracking row of an update post, whatever its
//     status, except 'done';
//   - puts a 'repeat' row whose call is the first call of its token (an older
//     call was deleted, or the older row turned out to be an update post) back
//     to 'pending', due now.
//
// Nothing is deleted: results already stored for a row stay. Returns the rows
// changed; when everything is already in place it writes nothing.
func (st *ScoutStore) MarkRepeatTracking(ctx context.Context) (int, error) {
	tag, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking t
		SET status = x.new_status, error = NULL, updated_at = now(),
		    next_check_at = CASE WHEN x.new_status = 'pending' THEN now() ELSE t.next_check_at END,
		    attempts = CASE WHEN x.new_status = 'pending' THEN 0 ELSE t.attempts END
		FROM (SELECT t2.call_id, CASE WHEN fc.id IS NULL THEN 'repeat' ELSE 'pending' END AS new_status
		      FROM scout_call_tracking t2 JOIN scout_calls c2 ON c2.id = t2.call_id
		      LEFT JOIN `+webFirstCallsSQL+` fc ON fc.id = t2.call_id
		      WHERE (fc.id IS NULL AND t2.status IN ('pending','tracking','no_pool','error','gave_up'))
		         OR (c2.post_kind = 'update' AND t2.status NOT IN ('done','repeat'))
		         OR (fc.id IS NOT NULL AND t2.status = 'repeat')) x
		WHERE t.call_id = x.call_id`)
	return int(tag.RowsAffected()), err
}

// TrackingStats returns how many calls are in each tracking status and when the
// next check is due (nil if nothing is scheduled).
func (st *ScoutStore) TrackingStats(ctx context.Context) (map[string]int, *time.Time, error) {
	rows, err := st.Pool.Query(ctx, `SELECT status, count(*) FROM scout_call_tracking GROUP BY status`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, nil, err
		}
		counts[s] = n
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *time.Time
	err = st.Pool.QueryRow(ctx, `SELECT min(next_check_at) FROM scout_call_tracking
		WHERE status IN ('pending','tracking','no_pool','error')`).Scan(&next)
	return counts, next, err
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
	var onchainJSON *string
	if len(t.Onchain) > 0 {
		s := string(t.Onchain)
		onchainJSON = &s
	}
	_, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET
		status = $2, pool_address = $3, pool_name = $4, pool_dex = $5, pool_created_at = $6,
		entry_price_usd = $7, entry_price_source = $8, current_price_usd = $9, current_liquidity_usd = $10,
		rugged = $11, next_check_at = $12, last_checked_at = $13, attempts = $14, error = $15,
		price_unit = $16, onchain = $17::jsonb, entry_late_price_usd = $18, updated_at = now()
		WHERE call_id = $1`,
		t.CallID, t.Status, t.PoolAddress, t.PoolName, t.PoolDex, t.PoolCreatedAt,
		t.EntryPriceUSD, t.EntryPriceSource, t.CurrentPriceUSD, t.CurrentLiquidityUSD,
		t.Rugged, t.NextCheckAt.UTC(), t.LastCheckedAt, t.Attempts, t.Error, t.PriceUnit, onchainJSON, t.EntryLatePriceUSD)
	return err
}

// latestDueSQL: the tracking rows the latest-price pass may refresh, and which
// of them are due. A row qualifies when it is a token's first real call with
// status tracking or done, an entry price and a current on-chain state with a
// pool. It is due when it has no latest price yet, or the last one was read at
// or before its cut-off: $2 for calls posted after $1 (younger than 30 days),
// $3 for older ones and for rugged calls (rug_block in the state: their latest
// return is -100% for good, so they are re-stamped at most at the old pace).
var latestDueSQL = ` FROM scout_call_tracking t JOIN ` + webFirstCallsSQL + ` fc ON fc.id = t.call_id
	WHERE t.status IN ('tracking','done') AND t.entry_price_usd IS NOT NULL AND t.onchain IS NOT NULL
	  AND COALESCE(t.onchain->>'pool', '') <> '' AND COALESCE((t.onchain->>'entry_price_q')::float8, 0) > 0
	  AND COALESCE((t.onchain->>'v')::int, 0) >= ` + strconv.Itoa(onchainStateVersion) + `
	  AND (t.latest_checked_at IS NULL OR t.latest_checked_at <=
	       CASE WHEN t.entry_at > $1::timestamptz AND COALESCE((t.onchain->>'rug_block')::numeric, 0) = 0
	            THEN $2::timestamptz ELSE $3::timestamptz END)`

// DueLatest returns up to limit rows whose latest price is due: calls younger
// than recentAge first, then the ones not refreshed for the longest (never
// refreshed first). A young call is due when its price is older than recent,
// an older one when it is older than old. recentOnly leaves the older calls
// out; skip lists call ids to leave out (rows that failed a moment ago).
func (st *ScoutStore) DueLatest(ctx context.Context, now time.Time, recent, old, recentAge time.Duration, recentOnly bool, skip []int, limit int) ([]ScoutCallTracking, error) {
	if skip == nil {
		skip = []int{}
	}
	now = now.UTC()
	rows, err := st.Pool.Query(ctx, `SELECT `+trackingColumnsT+latestDueSQL+`
		AND (NOT $4::boolean OR t.entry_at > $1::timestamptz) AND t.call_id <> ALL($5::int[])
		ORDER BY (t.entry_at > $1::timestamptz) DESC, t.latest_checked_at NULLS FIRST, t.call_id
		LIMIT $6`, now.Add(-recentAge), now.Add(-recent), now.Add(-old), recentOnly, skip, limit)
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

// CountDueLatest returns how many rows are waiting for a latest price (due now).
func (st *ScoutStore) CountDueLatest(ctx context.Context, now time.Time, recent, old, recentAge time.Duration) (int, error) {
	var n int
	now = now.UTC()
	err := st.Pool.QueryRow(ctx, `SELECT count(*)`+latestDueSQL, now.Add(-recentAge), now.Add(-recent), now.Add(-old)).Scan(&n)
	return n, err
}

// ScoutLatestPrice is what one refresh of the latest-price pass stores.
type ScoutLatestPrice struct {
	CallID    int
	Price     float64    // in the call's price_unit
	ReturnPct *float64   // vs the late entry; nil when there is no usable entry price
	CheckedAt time.Time  // when the price was read
	TradeAt   *time.Time // time of the trade behind the price; nil = as stored before
	// State holds the pass's own keys of the on-chain state (latest_block,
	// latest_price_q, latest_trade_block) as a JSON object. They are merged into
	// the stored state: every other key stays as it is.
	State []byte
	// Rugged: the pool was drained (pass-found rugs add rug_block to State):
	// rugged is set to true, and current_liquidity_usd to LiquidityUSD when
	// that is known. false = both columns stay as they are.
	Rugged       bool
	LiquidityUSD *float64
}

// SaveLatestPrice writes the four latest_* columns and merges the pass's cursor
// into the on-chain state. Nothing else of the row changes (status, schedule,
// attempts, horizon scan progress and updated_at stay as they are), except
// rugged / current_liquidity_usd for a rugged call (p.Rugged).
func (st *ScoutStore) SaveLatestPrice(ctx context.Context, p ScoutLatestPrice) error {
	_, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET
		latest_price_usd = $2, latest_return_pct = $3, latest_checked_at = $4,
		latest_trade_at = COALESCE($5, latest_trade_at), onchain = onchain || $6::jsonb,
		rugged = CASE WHEN $7::boolean THEN true ELSE rugged END,
		current_liquidity_usd = CASE WHEN $7::boolean AND $8::float8 IS NOT NULL THEN $8::float8 ELSE current_liquidity_usd END
		WHERE call_id = $1 AND onchain IS NOT NULL`,
		p.CallID, p.Price, p.ReturnPct, p.CheckedAt.UTC(), p.TradeAt, string(p.State), p.Rugged, p.LiquidityUSD)
	return err
}

// ContractsMissingTokenName returns up to limit distinct contract addresses
// that have a tracking row without a token name yet, newest calls first.
func (st *ScoutStore) ContractsMissingTokenName(ctx context.Context, limit int) ([]string, error) {
	rows, err := st.Pool.Query(ctx, `SELECT min(contract_address) FROM scout_call_tracking
		WHERE token_name IS NULL GROUP BY lower(contract_address)
		ORDER BY max(call_id) DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ca string
		if err := rows.Scan(&ca); err != nil {
			return nil, err
		}
		out = append(out, ca)
	}
	return out, rows.Err()
}

// CopyKnownTokenNames fills the token name of calls whose token was already
// looked up for another call. Returns the rows filled.
func (st *ScoutStore) CopyKnownTokenNames(ctx context.Context) (int, error) {
	tag, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking t
		SET token_name = k.token_name, token_symbol_onchain = k.token_symbol_onchain
		FROM (SELECT DISTINCT ON (lower(contract_address)) lower(contract_address) AS ca, token_name, token_symbol_onchain
		      FROM scout_call_tracking WHERE token_name IS NOT NULL
		      ORDER BY lower(contract_address), call_id DESC) k
		WHERE t.token_name IS NULL AND lower(t.contract_address) = k.ca`)
	return int(tag.RowsAffected()), err
}

// SetTokenName stores a token's on-chain name and symbol on every call of that
// contract ("" = the contract has none). Only these two columns change: status,
// schedule, on-chain state and updated_at stay as they are. Returns the rows updated.
func (st *ScoutStore) SetTokenName(ctx context.Context, ca, name, symbol string) (int, error) {
	tag, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET token_name = $2, token_symbol_onchain = $3
		WHERE lower(contract_address) = lower($1)`, ca, name, symbol)
	return int(tag.RowsAffected()), err
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
		 max_price_usd, min_price_usd, last_trade_at, return_late_pct, max_gain_late_pct, max_drawdown_late_pct, computed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,now())
		ON CONFLICT (call_id, horizon) DO UPDATE SET
		 horizon_seconds = EXCLUDED.horizon_seconds, due_at = EXCLUDED.due_at, status = EXCLUDED.status,
		 price_usd = EXCLUDED.price_usd, return_pct = EXCLUDED.return_pct, max_gain_pct = EXCLUDED.max_gain_pct,
		 max_drawdown_pct = EXCLUDED.max_drawdown_pct, max_price_usd = EXCLUDED.max_price_usd,
		 min_price_usd = EXCLUDED.min_price_usd, last_trade_at = EXCLUDED.last_trade_at,
		 return_late_pct = EXCLUDED.return_late_pct, max_gain_late_pct = EXCLUDED.max_gain_late_pct,
		 max_drawdown_late_pct = EXCLUDED.max_drawdown_late_pct, computed_at = now()`,
		callID, h.Name, int(h.Dur/time.Second), r.DueAt.UTC(), r.Status, nz(r.PriceUSD), nz(r.ReturnPct),
		nz(r.MaxGainPct), nz(r.MaxDDPct), nz(r.MaxPriceUSD), nz(r.MinPriceUSD), r.LastTradeAt,
		r.ReturnLatePct, r.MaxGainLatePct, r.MaxDDLatePct)
	return err
}

// ReturnsForCall returns the stored horizon results of a call, by horizon name.
func (st *ScoutStore) ReturnsForCall(ctx context.Context, callID int) (map[string]horizonResult, error) {
	rows, err := st.Pool.Query(ctx, `SELECT horizon, due_at, status, COALESCE(price_usd,0)::float8,
		COALESCE(return_pct,0)::float8, COALESCE(max_gain_pct,0)::float8, COALESCE(max_drawdown_pct,0)::float8,
		COALESCE(max_price_usd,0)::float8, COALESCE(min_price_usd,0)::float8, last_trade_at,
		return_late_pct::float8, max_gain_late_pct::float8, max_drawdown_late_pct::float8
		FROM scout_call_returns WHERE call_id = $1`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]horizonResult{}
	for rows.Next() {
		var r horizonResult
		if err := rows.Scan(&r.Horizon, &r.DueAt, &r.Status, &r.PriceUSD, &r.ReturnPct, &r.MaxGainPct,
			&r.MaxDDPct, &r.MaxPriceUSD, &r.MinPriceUSD, &r.LastTradeAt,
			&r.ReturnLatePct, &r.MaxGainLatePct, &r.MaxDDLatePct); err != nil {
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

// ---------------------------------------------------------------------------
// Candles, pre-call trading, predictions
// ---------------------------------------------------------------------------

// DeleteCandles removes a call's candles (before it is tracked again from scratch).
func (st *ScoutStore) DeleteCandles(ctx context.Context, callID int) error {
	_, err := st.Pool.Exec(ctx, `DELETE FROM scout_call_candles WHERE call_id = $1`, callID)
	return err
}

// UpsertCandles stores candles. A bucket seen again (it straddled two scans) is
// merged: open is kept, high/low widen, close is replaced.
func (st *ScoutStore) UpsertCandles(ctx context.Context, callID int, cs []candleRow) error {
	if len(cs) == 0 {
		return nil
	}
	iv := make([]int32, len(cs))
	ts := make([]time.Time, len(cs))
	o, h, l, c := make([]float64, len(cs)), make([]float64, len(cs)), make([]float64, len(cs)), make([]float64, len(cs))
	n := make([]int32, len(cs))
	for i, r := range cs {
		iv[i], ts[i], o[i], h[i], l[i], c[i], n[i] = int32(r.IntervalS), r.Start.UTC(), r.O, r.H, r.L, r.C, int32(r.Events)
	}
	_, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_candles AS k
		(call_id, interval_seconds, bucket_start, open, high, low, close, events)
		SELECT $1, * FROM unnest($2::int[], $3::timestamptz[], $4::float8[], $5::float8[], $6::float8[], $7::float8[], $8::int[])
		ON CONFLICT (call_id, interval_seconds, bucket_start) DO UPDATE SET
		 high = GREATEST(k.high, EXCLUDED.high), low = LEAST(k.low, EXCLUDED.low),
		 close = EXCLUDED.close, events = k.events + EXCLUDED.events`,
		callID, iv, ts, o, h, l, c, n)
	return err
}

// CandlesForCall returns a call's candles of one interval, oldest first.
func (st *ScoutStore) CandlesForCall(ctx context.Context, callID, intervalS int) ([]candleRow, error) {
	rows, err := st.Pool.Query(ctx, `SELECT interval_seconds, bucket_start, open::float8, high::float8, low::float8,
		close::float8, events FROM scout_call_candles WHERE call_id = $1 AND interval_seconds = $2 ORDER BY bucket_start`, callID, intervalS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candleRow
	for rows.Next() {
		var r candleRow
		if err := rows.Scan(&r.IntervalS, &r.Start, &r.O, &r.H, &r.L, &r.C, &r.Events); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertPrecall stores the trading stats of the hour before a call.
func (st *ScoutStore) UpsertPrecall(ctx context.Context, callID int, p *precallStats) error {
	_, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_precall
		(call_id, window_seconds, vol_unit, first_trade_age_s,
		 swaps_5m, swaps_15m, swaps_60m, buys_5m, buys_15m, buys_60m, sells_5m, sells_15m, sells_60m,
		 buy_vol_5m, buy_vol_15m, buy_vol_60m, sell_vol_5m, sell_vol_15m, sell_vol_60m,
		 price_chg_5m_pct, price_chg_15m_pct, price_chg_60m_pct, computed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,now())
		ON CONFLICT (call_id) DO UPDATE SET
		 window_seconds = EXCLUDED.window_seconds, vol_unit = EXCLUDED.vol_unit, first_trade_age_s = EXCLUDED.first_trade_age_s,
		 swaps_5m = EXCLUDED.swaps_5m, swaps_15m = EXCLUDED.swaps_15m, swaps_60m = EXCLUDED.swaps_60m,
		 buys_5m = EXCLUDED.buys_5m, buys_15m = EXCLUDED.buys_15m, buys_60m = EXCLUDED.buys_60m,
		 sells_5m = EXCLUDED.sells_5m, sells_15m = EXCLUDED.sells_15m, sells_60m = EXCLUDED.sells_60m,
		 buy_vol_5m = EXCLUDED.buy_vol_5m, buy_vol_15m = EXCLUDED.buy_vol_15m, buy_vol_60m = EXCLUDED.buy_vol_60m,
		 sell_vol_5m = EXCLUDED.sell_vol_5m, sell_vol_15m = EXCLUDED.sell_vol_15m, sell_vol_60m = EXCLUDED.sell_vol_60m,
		 price_chg_5m_pct = EXCLUDED.price_chg_5m_pct, price_chg_15m_pct = EXCLUDED.price_chg_15m_pct,
		 price_chg_60m_pct = EXCLUDED.price_chg_60m_pct, computed_at = now()`,
		callID, p.WindowS, p.VolUnit, p.FirstTradeAgeS,
		p.Swaps[0], p.Swaps[1], p.Swaps[2], p.Buys[0], p.Buys[1], p.Buys[2], p.Sells[0], p.Sells[1], p.Sells[2],
		p.BuyVol[0], p.BuyVol[1], p.BuyVol[2], p.SellVol[0], p.SellVol[1], p.SellVol[2],
		p.PriceChgPct[0], p.PriceChgPct[1], p.PriceChgPct[2])
	return err
}

// HasPrecall reports whether pre-call stats are stored for a call.
func (st *ScoutStore) HasPrecall(ctx context.Context, callID int) (bool, error) {
	var ok bool
	err := st.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scout_call_precall WHERE call_id = $1)`, callID).Scan(&ok)
	return ok, err
}

// DatasetRowJSON returns the call's row of scout_call_dataset_v as JSON: the
// exact features the model was trained on (nil if the call is unknown).
func (st *ScoutStore) DatasetRowJSON(ctx context.Context, callID int) ([]byte, error) {
	var b []byte
	err := st.Pool.QueryRow(ctx, `SELECT row_to_json(v) FROM scout_call_dataset_v v WHERE call_id = $1`, callID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ScoutCallPrediction is one model score for one call and holding-period bucket.
type ScoutCallPrediction struct {
	CallID        int      `json:"call_id"`
	ModelVersion  string   `json:"model_version"`
	Bucket        string   `json:"bucket"` // short | 3day | medium | long
	RunnerProb    *float64 `json:"runner_prob"`
	CollapseProb  *float64 `json:"collapse_prob"`
	RunnerRankPct *float64 `json:"runner_rank_pct"` // share of recent calls with a lower runner score
}

// TableName returns the table name for this model.
func (ScoutCallPrediction) TableName() string { return "scout_call_predictions" }

// UpsertPrediction stores a score (one row per call, model version and bucket).
func (st *ScoutStore) UpsertPrediction(ctx context.Context, p ScoutCallPrediction, features []byte) error {
	var f *string
	if len(features) > 0 {
		s := string(features)
		f = &s
	}
	// uuid, contract_address and created_by are the shared table's required columns.
	_, err := st.Pool.Exec(ctx, `INSERT INTO scout_call_predictions
		(uuid, call_id, contract_address, model_version, bucket, runner_prob, collapse_prob, runner_rank_pct, features, created_by)
		SELECT $8::uuid, c.id, c.contract_address, $2, $3, $4, $5, $6, $7::jsonb, $9 FROM scout_calls c WHERE c.id = $1
		ON CONFLICT (call_id, model_version, bucket) DO UPDATE SET
		 runner_prob = EXCLUDED.runner_prob, collapse_prob = EXCLUDED.collapse_prob,
		 runner_rank_pct = EXCLUDED.runner_rank_pct, features = EXCLUDED.features, created_at = now()`,
		p.CallID, p.ModelVersion, p.Bucket, p.RunnerProb, p.CollapseProb, p.RunnerRankPct, f, uuid.NewString(), "scoutanalytics")
	return err
}

// PredictionsForCall returns the stored scores of a call.
func (st *ScoutStore) PredictionsForCall(ctx context.Context, callID int) ([]ScoutCallPrediction, error) {
	rows, err := st.Pool.Query(ctx, `SELECT call_id, model_version, bucket, runner_prob::float8, collapse_prob::float8,
		runner_rank_pct::float8 FROM scout_call_predictions WHERE call_id = $1 AND bucket IS NOT NULL ORDER BY model_version, id`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutCallPrediction
	for rows.Next() {
		var p ScoutCallPrediction
		if err := rows.Scan(&p.CallID, &p.ModelVersion, &p.Bucket, &p.RunnerProb, &p.CollapseProb, &p.RunnerRankPct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Website (read-only): import progress and the call list
// ---------------------------------------------------------------------------

// The website shows one row per token: its first call. A token is a contract
// address compared without regard to letter case; the first call is the one
// with the lowest message_date, ties broken by the lowest id. Update posts
// (post_kind = 'update') are not calls: they are never a first call and are not
// counted; a row not classified yet (post_kind NULL) is read as a call. This is
// a display rule only: nothing is deleted, and the dataset view keeps one row
// per stored post and contract address.
const (
	// webRealCallSQL: the row is a real call, not an update post.
	webRealCallSQL = `post_kind IS DISTINCT FROM 'update'`
	// webFirstCallsSQL: the id of each token's first call.
	webFirstCallsSQL = `(SELECT DISTINCT ON (lower(contract_address)) id, lower(contract_address) AS ca
		FROM scout_calls WHERE ` + webRealCallSQL + ` ORDER BY lower(contract_address), message_date, id)`
	// webCallCountsSQL: how often each token was called, and when last.
	webCallCountsSQL = `(SELECT lower(contract_address) AS ca, count(*) AS call_count, max(message_date) AS last_call_date
		FROM scout_calls WHERE ` + webRealCallSQL + ` GROUP BY lower(contract_address))`
	// webPerceptorSQL: each token's latest completed Perceptor investigation
	// (by contract address without regard to letter case, whichever post of the
	// token it was run for). verdict is one of clean | caution | red_flags | unknown.
	webPerceptorSQL = `(SELECT DISTINCT ON (lower(i.contract_address)) lower(i.contract_address) AS ca,
		CASE WHEN i.verdict_level IN ('clean', 'caution', 'red_flags') THEN i.verdict_level ELSE 'unknown' END AS verdict,
		i.report_url, i.id
		FROM scout_investigations i JOIN scout_investigation_tools pt ON pt.id = i.tool_id AND pt.code = 'perceptor'
		WHERE i.status = 'completed'
		ORDER BY lower(i.contract_address), i.requested_at DESC, i.id DESC)`
	webPerceptorJoinSQL = ` LEFT JOIN ` + webPerceptorSQL + ` p ON p.ca = fc.ca`
	// webSAlphaSQL: each token's latest completed sAlpha investigation by the
	// same rule, among those whose report_text has something other than white
	// space (about half of sAlpha's replies are empty: they count as no report,
	// so an older one with text is taken instead). A reply that only declines
	// ("Not enough public signals …": its text contains one of the phrases of
	// $1, lower case, see salphaDeclinePhrases) is taken only when the token
	// has no real report: any real report, however old, comes first.
	webSAlphaSQL = `(SELECT DISTINCT ON (lower(i.contract_address)) lower(i.contract_address) AS ca, i.id
		FROM scout_investigations i JOIN scout_investigation_tools sat ON sat.id = i.tool_id AND sat.code = 'salpha'
		WHERE i.status = 'completed' AND i.report_text ~ '[^[:space:]]'
		ORDER BY lower(i.contract_address),
			EXISTS (SELECT 1 FROM unnest($1::text[]) AS d(phrase) WHERE strpos(lower(i.report_text), d.phrase) > 0),
			i.requested_at DESC, i.id DESC)`
	webSAlphaJoinSQL = ` LEFT JOIN ` + webSAlphaSQL + ` sa ON sa.ca = fc.ca`
)

// webRowsSQL loads the website's whole list in one statement: each token's
// first call with its tracking row (and its latest price), the late-entry
// results of the five windows, how often the token was called, its latest
// Perceptor report, the id of its latest sAlpha report with text (a real
// report before a decline; $1 = salphaDeclinePhrases) and the
// market caps of the post. It reads the
// tables directly (not scout_call_dataset_v, whose per-row lookups the website
// does not need). Ordered by call id.
var webRowsSQL = `SELECT c.id, c.message_id, c.message_date, c.channel_username, c.contract_address,
	NULLIF(t.token_name, ''), COALESCE(NULLIF(m.token_symbol, ''), NULLIF(t.token_symbol_onchain, '')),
	t.price_unit, COALESCE(t.entry_late_price_usd, t.entry_price_usd)::float8, t.entry_price_usd IS NOT NULL,
	t.rugged, t.status,
	` + webReturnsPivotSQL("r.") + `,
	p.verdict, p.report_url, p.id, sa.id, n.call_count, n.last_call_date,
	t.latest_return_pct::float8, t.latest_price_usd::float8, t.latest_checked_at, t.latest_trade_at,
	m.called_at_mcap_usd::float8, m.mcap_usd::float8, t.entry_price_usd::float8
	FROM ` + webFirstCallsSQL + ` fc
	JOIN scout_calls c ON c.id = fc.id
	JOIN ` + webCallCountsSQL + ` n ON n.ca = fc.ca
	LEFT JOIN scout_call_metrics m ON m.call_id = fc.id
	LEFT JOIN scout_call_tracking t ON t.call_id = fc.id
	LEFT JOIN (SELECT call_id, ` + webReturnsPivotSQL("") + `
		FROM scout_call_returns WHERE status = 'done' GROUP BY call_id) r ON r.call_id = fc.id` + webPerceptorJoinSQL + webSAlphaJoinSQL + `
	ORDER BY c.id`

// webReturnsPivotSQL lists the 15 result columns (window × return, peak,
// drawdown; late entry) in the order of ScoutWebRow.Perf: with prefix "" as the
// aggregates over scout_call_returns, otherwise as the columns of that subquery.
func webReturnsPivotSQL(prefix string) string {
	var cols []string
	for _, h := range ScoutWebHorizons {
		for _, c := range [3][2]string{{"ret", "return_late_pct"}, {"gain", "max_gain_late_pct"}, {"dd", "max_drawdown_late_pct"}} {
			name := c[0] + "_" + h
			if prefix != "" {
				cols = append(cols, prefix+name)
				continue
			}
			cols = append(cols, "max("+c[1]+") FILTER (WHERE horizon = '"+h+"')::float8 AS "+name)
		}
	}
	return strings.Join(cols, ", ")
}

// SelectWebRows returns every row of the website's list (one per token: its
// first call, in call id order) and the number of update posts. Both are read
// in one read-only transaction, so they describe the same moment.
func (st *ScoutStore) SelectWebRows(ctx context.Context) ([]ScoutWebRow, int, error) {
	tx, err := st.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var updatePosts int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM scout_calls WHERE post_kind = 'update'`).Scan(&updatePosts); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, webRowsSQL, salphaDeclinePhrases)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ScoutWebRow{}
	var perf [len(ScoutWebHorizons) * 3]pgtype.Float8
	for rows.Next() {
		var r ScoutWebRow
		dest := []any{&r.CallID, &r.MessageID, &r.MessageDate, &r.ChannelUsername, &r.ContractAddress,
			&r.TokenName, &r.TokenSymbol, &r.PriceUnit, &r.EntryPrice, &r.Tracked, &r.Rugged, &r.TrackingStatus}
		for i := range perf {
			dest = append(dest, &perf[i])
		}
		dest = append(dest, &r.PerceptorVerd, &r.PerceptorURL, &r.PerceptorID, &r.SAlphaID, &r.CallCount, &r.LastCallDate,
			&r.LatestReturn, &r.LatestPrice, &r.LatestAt, &r.LatestTradeAt,
			&r.CalledAtMcap, &r.PostMcap, &r.PostPrice)
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		for i, v := range perf {
			if v.Valid {
				r.Perf[i] = v.Float64
				r.HasPerf |= 1 << i
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, updatePosts, nil
}

// webReportReadChars: how much of a text SelectWebReports reads, in characters:
// one more than the website keeps in bytes, so it can tell that a text was cut
// (a character is at least one byte).
const webReportReadChars = webReportMaxBytes + 1

// SelectWebReports returns the investigations with the given ids, for the
// website's row detail: the verdict, label, summary and link of each, and the
// report text of sAlpha investigations (Perceptor's own text is not shown).
// Every text is read up to webReportReadChars characters. Ids that do not
// exist are left out.
func (st *ScoutStore) SelectWebReports(ctx context.Context, ids []int) (map[int]*ScoutWebReport, error) {
	out := make(map[int]*ScoutWebReport, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := st.Pool.Query(ctx, `SELECT i.id, t.code, COALESCE(i.completed_at, i.requested_at),
		CASE WHEN i.verdict_level IN ('clean', 'caution', 'red_flags') THEN i.verdict_level ELSE 'unknown' END,
		left(i.verdict_label, $2), left(i.verdict_summary, $2),
		CASE WHEN t.code = 'salpha' THEN left(i.report_text, $2) ELSE '' END,
		i.report_url
		FROM scout_investigations i JOIN scout_investigation_tools t ON t.id = i.tool_id
		WHERE i.id = ANY($1)`, ids, webReportReadChars)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ScoutWebReport
		if err := rows.Scan(&r.ID, &r.Tool, &r.At, &r.Verdict, &r.Label, &r.Summary, &r.Text, &r.URL); err != nil {
			return nil, err
		}
		out[r.ID] = &r
	}
	return out, rows.Err()
}
