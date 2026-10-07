-- =============================================================================
-- B_reset.sql  (step 4 of README.md)  WRITES: resets the selected calls.
--
-- THE ONLY LINE TO EDIT is expected_count, on LINE 60: it is the
-- only line of this file with the EDIT marker. Find it with Ctrl+F: type
-- three less-than signs, a space and EDIT. Put there the "selected" value of the
-- TOTAL row of A_count.sql. Do not edit any other line. (The OPTION line
-- below it, include_gap_heuristic, must stay the same as in A_count.sql.)
--
-- BEFORE YOU RUN IT
--   * The USD-source fix is deployed, and EVERY tracking process is stopped
--     (-track, and the listener unless it runs with -listen-only). Wait until
--     each process has exited.
--   * expected_count is set as above.
--
-- WHAT IT DOES (one transaction, all or nothing)
--   1. Picks the calls (same selection as A_count.sql: first calls, status
--      not 'repeat', and (a) price_unit not 'usd' with status
--      tracking/done/error, or (b) a 'candles to +...' error with 'no USD
--      source' / 'no trades in the', or (c) the candle-gap heuristic when
--      include_gap_heuristic is true) and locks them.
--   2. If their number is not exactly expected_count, it STOPS with an error
--      and changes nothing. Read the number in the error message, compare it
--      with A, and only then set expected_count again.
--   3. Deletes their rows in scout_call_returns, scout_call_candles and
--      scout_call_precall, and puts their scout_call_tracking row back to
--      'pending' (due now, attempts 0) with no on-chain state, pool, prices,
--      price_unit, rug flag or latest price. Kept: token_name,
--      token_symbol_onchain, priority, entry_at, contract_address,
--      created_at. scout_call_predictions and scout_call_metrics are not
--      touched.
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
-- NEVER RUN IT AGAIN AFTER THE TRACKER HAS STARTED
--   Right after a successful run it finds 0 calls (they are all 'pending' with
--   no price_unit, error or returns), so with the same expected_count it stops
--   and changes nothing. Once the tracker has re-tracked them, a call with
--   truly no USD source is back in quote units and would be selected again.
--   Do NOT lower or raise expected_count to "make it run".
--
-- pgAdmin: run the whole file with F5. Auto-commit can stay ON (the file has
-- its own BEGIN/COMMIT). The "WARNING: there is no transaction in progress"
-- from the first line and "NOTICE: schema "pg_temp" does not exist, skipping"
-- are expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.usdrepair_params, pg_temp.usdrepair;
CREATE TEMP TABLE usdrepair_params AS
SELECT -1    AS expected_count,          -- <<< EDIT: the "selected" value of A_count.sql's TOTAL row
       false AS include_gap_heuristic,   -- OPTION: true adds rule (c); same value in A, B and C
       now() AS reset_at;

-- The calls to reset, locked (FOR UPDATE) so that a tracker save of one of them
-- waits for this transaction instead of interleaving with it.
CREATE TEMP TABLE usdrepair AS
SELECT t.call_id, t.status AS old_status, t.price_unit AS old_price_unit,
       (t.status IN ('tracking','done','error')
        AND t.price_unit IS NOT NULL AND t.price_unit <> 'usd') AS a_unit,
       COALESCE(t.error LIKE 'candles to +%'
                AND (t.error LIKE '%no USD source%' OR t.error LIKE '%no trades in the%'), false) AS b_err
FROM scout_call_tracking t
JOIN (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
      WHERE post_kind IS DISTINCT FROM 'update'
      ORDER BY lower(contract_address), message_date, id) fc ON fc.id = t.call_id
CROSS JOIN usdrepair_params p
WHERE t.status <> 'repeat'
  AND (   (t.status IN ('tracking','done','error')
           AND t.price_unit IS NOT NULL AND t.price_unit <> 'usd')
       OR COALESCE(t.error LIKE 'candles to +%'
                   AND (t.error LIKE '%no USD source%' OR t.error LIKE '%no trades in the%'), false)
       OR (p.include_gap_heuristic
           AND t.status IN ('tracking','done','error') AND t.onchain IS NOT NULL AND EXISTS (
              SELECT 1 FROM scout_call_returns r
              WHERE r.call_id = t.call_id AND r.last_trade_at IS NOT NULL
                AND NOT EXISTS (SELECT 1 FROM scout_call_candles c
                                WHERE c.call_id = r.call_id AND c.interval_seconds = 3600
                                  AND c.bucket_start = date_trunc('hour', r.last_trade_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'))))
FOR UPDATE OF t;

-- Stop (and roll everything back) unless the count is exactly expected_count.
DO $$
DECLARE
  expected int;
  found    int;
BEGIN
  SELECT expected_count INTO expected FROM usdrepair_params;
  SELECT count(*) INTO found FROM usdrepair;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'B_reset: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'B_reset: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
END
$$;

DELETE FROM scout_call_returns WHERE call_id IN (SELECT call_id FROM usdrepair);
DELETE FROM scout_call_candles WHERE call_id IN (SELECT call_id FROM usdrepair);
DELETE FROM scout_call_precall WHERE call_id IN (SELECT call_id FROM usdrepair);

UPDATE scout_call_tracking t SET
    status = 'pending', next_check_at = now(), attempts = 0, error = NULL,
    last_checked_at = NULL, updated_at = now(), onchain = NULL,
    pool_address = NULL, pool_name = NULL, pool_dex = NULL, pool_created_at = NULL,
    entry_price_usd = NULL, entry_price_source = NULL, price_unit = NULL, entry_late_price_usd = NULL,
    current_price_usd = NULL, current_liquidity_usd = NULL, rugged = NULL,
    latest_price_usd = NULL, latest_return_pct = NULL, latest_checked_at = NULL, latest_trade_at = NULL
FROM usdrepair r
WHERE t.call_id = r.call_id;

COMMIT;

-- The result grid. Copy reset_at for C_check.sql and D_catchup.sql.
SELECT p.reset_at,
       (SELECT count(*) FROM usdrepair)                                   AS calls_reset,
       (SELECT count(*) FROM usdrepair WHERE a_unit)                      AS quote_units,
       (SELECT count(*) FROM usdrepair WHERE b_err)                       AS candle_error,
       (SELECT count(*) FROM usdrepair WHERE NOT a_unit AND NOT b_err)    AS candle_gap_only,
       (SELECT count(*) FROM usdrepair WHERE old_status = 'tracking')     AS were_tracking,
       (SELECT count(*) FROM usdrepair WHERE old_status = 'done')         AS were_done,
       (SELECT count(*) FROM usdrepair WHERE old_status = 'error')        AS were_error,
       (SELECT count(*) FROM usdrepair WHERE old_status = 'gave_up')      AS were_gave_up,
       (SELECT count(*) FROM usdrepair
         WHERE old_status NOT IN ('tracking','done','error','gave_up'))  AS were_other
FROM usdrepair_params p;
