package main

// The holder export's database reads. The export runs all day beside -track
// and never writes: every read here runs in a short read-only transaction
// (readOnlyTx), so a stray write fails in Postgres, and no snapshot is held
// while the export works on the nodes (a long snapshot would hold back VACUUM
// on scout_call_tracking).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// latestLagPercentile: the share of the latest-price rows LatestPriceLag looks
// past. A failed refresh does not stamp latest_checked_at, so a few stuck rows
// stay overdue for good; the maximum would hold the export's pause forever.
const latestLagPercentile = 0.95

// readOnlyTx runs fn in a read-only, repeatable-read transaction and rolls it
// back afterwards (there is nothing to commit). fn's error is returned as is.
func (st *ScoutStore) readOnlyTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := st.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read-only transaction: %w", err)
	}
	// The rollback error is dropped: the transaction is read-only, so there is
	// nothing to undo, and pgx closes the connection if the rollback fails.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return fn(tx)
}

// holderCallMetasSQL: one row per call found among $1, with its tracking row
// and its post metrics when it has them.
const holderCallMetasSQL = `SELECT c.id, c.contract_address, c.chain, (t.call_id IS NOT NULL), t.entry_at, t.onchain, m.age_seconds
	FROM scout_calls c
	LEFT JOIN scout_call_tracking t ON t.call_id = c.id
	LEFT JOIN scout_call_metrics m ON m.call_id = c.id
	WHERE c.id = ANY($1::bigint[])`

// HolderCallMetas reads the export's metadata of the calls ids, in one short
// read-only transaction. metas follows the order of ids (an id given twice
// appears twice); ids that are not in scout_calls are in missing, in the same
// order. An on-chain state that does not parse gives State nil, not an error.
func (st *ScoutStore) HolderCallMetas(ctx context.Context, ids []int64) (metas []holderCallMeta, missing []int64, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	var byID map[int64]holderCallMeta
	err = st.readOnlyTx(ctx, func(tx pgx.Tx) error {
		var qerr error
		byID, qerr = queryHolderCallMetas(ctx, tx, ids)
		return qerr
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading holder call metas: %w", err)
	}
	metas, missing = orderHolderCallMetas(ids, byID)
	return metas, missing, nil
}

// queryHolderCallMetas runs holderCallMetasSQL and returns the calls found, by id.
func queryHolderCallMetas(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]holderCallMeta, error) {
	rows, err := tx.Query(ctx, holderCallMetasSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[int64]holderCallMeta, len(ids))
	for rows.Next() {
		m, err := scanHolderCallMeta(rows)
		if err != nil {
			return nil, err
		}
		byID[m.CallID] = m
	}
	return byID, rows.Err()
}

// scanHolderCallMeta reads one row of holderCallMetasSQL.
func scanHolderCallMeta(row pgx.Row) (holderCallMeta, error) {
	var m holderCallMeta
	var entryAt *time.Time
	var onchain []byte
	if err := row.Scan(&m.CallID, &m.Token, &m.Chain, &m.TrackingRow, &entryAt, &onchain, &m.PostedAgeS); err != nil {
		return holderCallMeta{}, err
	}
	if entryAt != nil {
		m.EntryAt = *entryAt
	}
	m.State = parseHolderState(onchain)
	return m, nil
}

// parseHolderState decodes a stored on-chain state; nil when there is none or
// it does not parse. Older state versions are kept as they are.
func parseHolderState(raw []byte) *onchainState {
	if len(raw) == 0 {
		return nil
	}
	var s onchainState
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// orderHolderCallMetas puts the calls found in the order of ids and lists the
// ids not found.
func orderHolderCallMetas(ids []int64, byID map[int64]holderCallMeta) (metas []holderCallMeta, missing []int64) {
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		metas = append(metas, m)
	}
	return metas, missing
}

// latestPriceLagSQL: the latestLagPercentile ($4) percentile, in seconds, of how
// far the latest-price rows are past their due cut-off (0 when not due yet).
// Same rows and cut-offs as latestDueSQL / CountDueLatest; rows never refreshed
// (latest_checked_at NULL) are left out. No rows: 0.
var latestPriceLagSQL = `SELECT COALESCE(percentile_disc($4::float8) WITHIN GROUP (ORDER BY
	  GREATEST(EXTRACT(EPOCH FROM (` + latestCutoffSQL + ` - t.latest_checked_at))::float8, 0)), 0)` +
	latestRowsSQL + `
	  AND t.latest_checked_at IS NOT NULL`

// LatestPriceLag returns how far the latest-price pass is behind: the
// latestLagPercentile percentile of how overdue the refreshed rows are.
//
// Overdue, per row: its cut-off minus latest_checked_at, at least 0, i.e. how
// long ago the row became due (latestDueSQL: due once latest_checked_at is at
// or before the cut-off). Rows not due yet count as 0, so the lag rises only
// once more than 5% of the rows are overdue, and a few rows whose refresh
// keeps failing (a failure does not stamp latest_checked_at) do not move it.
// Rows never refreshed have no stamp to measure from and are left out.
//
// now, recent, old and recentAge mean what they mean for CountDueLatest; pass
// the tracker's own values (priceConfig.LatestRecent, LatestOld and
// latestRecentAge), or the lag no longer matches the pass. Read in readOnlyTx,
// like every read of the export.
func (st *ScoutStore) LatestPriceLag(ctx context.Context, now time.Time, recent, old, recentAge time.Duration) (time.Duration, error) {
	now = now.UTC()
	var secs float64
	err := st.readOnlyTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, latestPriceLagSQL, now.Add(-recentAge), now.Add(-recent), now.Add(-old), latestLagPercentile).Scan(&secs)
	})
	if err != nil {
		return 0, fmt.Errorf("reading latest price lag: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
