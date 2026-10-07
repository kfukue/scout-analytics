-- =============================================================================
-- B_reset.sql  (step 4 of README.txt)  WRITES: resets the selected calls.
--
-- BEFORE YOU RUN IT
--   * The new tracker code is deployed, and EVERY tracking process is stopped
--     (-track, and the listener unless it runs with -listen-only). Wait until
--     each process has exited.
--   * Set expected_count below (the line marked EDIT) to the "selected" value
--     of the TOTAL row of A_count.sql. Keep quote_side_usd the same as in A.
--
-- WHAT IT DOES (one transaction, all or nothing)
--   1. Picks the calls (same selection as A_count.sql) and locks them.
--   2. If their number is not exactly expected_count, it STOPS with an error
--      and changes nothing. Read the number in the error message, compare it
--      with A, and only then set expected_count again.
--   3. Deletes their rows in scout_call_returns, scout_call_candles and
--      scout_call_precall, and puts their scout_call_tracking row back to
--      'pending' (due now, attempts 0) with no on-chain state, pool, prices,
--      rug flag or latest price. Kept: token_name, token_symbol_onchain,
--      priority, entry_at, contract_address, created_at.
--      scout_call_predictions is not touched.
--   4. Commits and shows ONE row: reset_at and the counts. Copy reset_at:
--      C_check.sql and D_catchup.sql need it.
--
-- IF IT STOPS WITH AN ERROR
--   Nothing was changed. pgAdmin may keep the failed transaction open (it then
--   still holds row locks and the tracker would wait on them). Run
--       ROLLBACK;
--   in the same Query Tool tab, or close the tab, before starting the tracker.
--   (This file also starts with ROLLBACK, so simply running it again is safe.)
--
-- RUNNING IT A SECOND TIME
--   Right after a successful run it finds 0 calls (they are all 'pending'),
--   so with the same expected_count it stops and changes nothing. Do NOT
--   lower expected_count to rerun it later: once the tracker has re-tracked
--   the calls, a rerun resets the ones still under the threshold again.
--   For rows the tracker wrote back over the reset, use D_catchup.sql.
--
-- pgAdmin: run the whole file with F5. Auto-commit can stay ON (the file has
-- its own BEGIN/COMMIT). The "WARNING: there is no transaction in progress"
-- from the first line and "NOTICE: schema "pg_temp" does not exist, skipping"
-- are expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.retrack_params, pg_temp.retrack;
CREATE TEMP TABLE retrack_params AS
SELECT -1           AS expected_count,   -- <<< EDIT: the "selected" TOTAL from A_count.sql
       500::numeric AS quote_side_usd,   -- <<< rug threshold: $ on the pool's quote side (same as A)
       now()        AS reset_at;

-- The calls to reset, locked (FOR UPDATE) so that a tracker save of one of them
-- waits for this transaction instead of interleaving with it.
CREATE TEMP TABLE retrack AS
SELECT t.call_id, t.status AS old_status
FROM scout_call_tracking t
JOIN (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
      WHERE post_kind IS DISTINCT FROM 'update'
      ORDER BY lower(contract_address), message_date, id) fc ON fc.id = t.call_id
CROSS JOIN retrack_params p
WHERE t.status IN ('tracking','done','error')
  AND (   COALESCE(t.current_liquidity_usd < 2 * p.quote_side_usd, false)
       OR EXISTS (SELECT 1 FROM scout_call_returns r WHERE r.call_id = t.call_id
                  AND (r.return_pct > 1e5 OR r.return_late_pct > 1e5
                       OR r.max_gain_pct > 1e5 OR r.max_gain_late_pct > 1e5))
       OR COALESCE(t.latest_return_pct > 1e5, false)
       OR (jsonb_typeof(t.onchain) = 'object' AND EXISTS (
            SELECT 1 FROM unnest(ARRAY['entry_price_q','last_price_q','run_max_q','run_min_q',
                                       'late_price_q','run_max_late_q','run_min_late_q','latest_price_q']) AS k(key)
            WHERE CASE WHEN jsonb_typeof(t.onchain -> k.key) IS DISTINCT FROM 'number' THEN false
                       WHEN (t.onchain ->> k.key)::numeric <= 0 THEN false
                       ELSE log((t.onchain ->> k.key)::numeric)
                            - (CASE WHEN jsonb_typeof(t.onchain -> 'token_dec') = 'number'
                                    THEN (t.onchain ->> 'token_dec')::numeric ELSE 0 END
                               - CASE WHEN jsonb_typeof(t.onchain -> 'quote_dec') = 'number'
                                      THEN (t.onchain ->> 'quote_dec')::numeric ELSE 0 END) > 38
                  END)))
FOR UPDATE OF t;

-- Stop (and roll everything back) unless the count is exactly expected_count.
DO $$
DECLARE
  expected int;
  found    int;
BEGIN
  SELECT expected_count INTO expected FROM retrack_params;
  SELECT count(*) INTO found FROM retrack;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'B_reset: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'B_reset: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
END
$$;

DELETE FROM scout_call_returns WHERE call_id IN (SELECT call_id FROM retrack);
DELETE FROM scout_call_candles WHERE call_id IN (SELECT call_id FROM retrack);
DELETE FROM scout_call_precall WHERE call_id IN (SELECT call_id FROM retrack);

UPDATE scout_call_tracking t SET
    status = 'pending', next_check_at = now(), attempts = 0, error = NULL,
    last_checked_at = NULL, updated_at = now(), onchain = NULL,
    pool_address = NULL, pool_name = NULL, pool_dex = NULL, pool_created_at = NULL,
    entry_price_usd = NULL, entry_price_source = NULL, price_unit = NULL, entry_late_price_usd = NULL,
    current_price_usd = NULL, current_liquidity_usd = NULL, rugged = NULL,
    latest_price_usd = NULL, latest_return_pct = NULL, latest_checked_at = NULL, latest_trade_at = NULL
FROM retrack r
WHERE t.call_id = r.call_id;

COMMIT;

-- The result grid. Copy reset_at for C_check.sql and D_catchup.sql.
SELECT p.reset_at,
       (SELECT count(*) FROM retrack)                                 AS calls_reset,
       (SELECT count(*) FROM retrack WHERE old_status = 'tracking')  AS were_tracking,
       (SELECT count(*) FROM retrack WHERE old_status = 'done')      AS were_done,
       (SELECT count(*) FROM retrack WHERE old_status = 'error')     AS were_error
FROM retrack_params p;
