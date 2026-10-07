-- =============================================================================
-- D_catchup.sql  (step 7 of README.txt)  WRITES. Run it ONLY if C_check.sql
-- shows race_victims > 0.
--
-- Paste B's reset_at into the line marked EDIT (keep the quotes) and run the
-- whole file (F5). The script fails, and changes nothing, until the
-- placeholder is replaced.
--
-- It resets exactly the race victims of C_check.sql (first calls written
-- at/after reset_at whose on-chain state says the pre-call stats or a horizon
-- are done while that row is missing), the same way B does: their returns,
-- candles and pre-call rows are deleted and their tracking row goes back to
-- 'pending' with no on-chain state. Token names and priority are kept.
-- Rows last written before reset_at are never touched.
--
-- Safe to run again at any time: a row the tracker started fresh never
-- matches. If a tracker was running during B, run C again after its current
-- cycle; a victim can only come from a batch that was in progress during B.
--
-- Result: ONE row, calls_reset and their call_ids.
-- IF IT STOPS WITH AN ERROR: nothing was changed; run ROLLBACK; in the same
-- tab (or close it) before starting the tracker. Running the file again is
-- also safe (it starts with ROLLBACK). The "WARNING: there is no transaction
-- in progress" from the first line and "NOTICE: schema "pg_temp" does not
-- exist, skipping" are expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.catchup_params, pg_temp.catchup;
CREATE TEMP TABLE catchup_params AS
SELECT 'PASTE reset_at FROM B HERE'::timestamptz AS reset_at;   -- <<< EDIT: B's reset_at

CREATE TEMP TABLE catchup AS
SELECT t.call_id, t.status AS old_status
FROM scout_call_tracking t
JOIN (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
      WHERE post_kind IS DISTINCT FROM 'update'
      ORDER BY lower(contract_address), message_date, id) fc ON fc.id = t.call_id
CROSS JOIN catchup_params p
WHERE t.status IN ('tracking','done','error')
  AND t.updated_at >= p.reset_at
  AND jsonb_typeof(t.onchain) = 'object'
  AND (   (CASE WHEN jsonb_typeof(t.onchain -> 'pre_done') = 'boolean'
                THEN (t.onchain ->> 'pre_done')::boolean ELSE false END
           AND NOT EXISTS (SELECT 1 FROM scout_call_precall q WHERE q.call_id = t.call_id))
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(CASE WHEN jsonb_typeof(t.onchain -> 'done') = 'object'
                                                       THEN t.onchain -> 'done' ELSE '{}'::jsonb END) AS d(horizon)
                  WHERE NOT EXISTS (SELECT 1 FROM scout_call_returns r
                                    WHERE r.call_id = t.call_id AND r.horizon = d.horizon)))
FOR UPDATE OF t;

DELETE FROM scout_call_returns WHERE call_id IN (SELECT call_id FROM catchup);
DELETE FROM scout_call_candles WHERE call_id IN (SELECT call_id FROM catchup);
DELETE FROM scout_call_precall WHERE call_id IN (SELECT call_id FROM catchup);

UPDATE scout_call_tracking t SET
    status = 'pending', next_check_at = now(), attempts = 0, error = NULL,
    last_checked_at = NULL, updated_at = now(), onchain = NULL,
    pool_address = NULL, pool_name = NULL, pool_dex = NULL, pool_created_at = NULL,
    entry_price_usd = NULL, entry_price_source = NULL, price_unit = NULL, entry_late_price_usd = NULL,
    current_price_usd = NULL, current_liquidity_usd = NULL, rugged = NULL,
    latest_price_usd = NULL, latest_return_pct = NULL, latest_checked_at = NULL, latest_trade_at = NULL
FROM catchup c
WHERE t.call_id = c.call_id;

COMMIT;

-- The result grid.
SELECT count(*) AS calls_reset,
       array_agg(call_id ORDER BY call_id) AS call_ids
FROM catchup;
