-- =============================================================================
-- B_reset.sql  (step 4 of README.md)  WRITES: resets the selected calls.
--
-- THE ONLY LINE TO EDIT is expected_count, on LINE 62: it is the only line
-- of this file with the EDIT marker. Find it with Ctrl+F:
-- type three less-than signs, a space and EDIT. Put there the "selected" value
-- of the TOTAL row of A_count.sql. Do not edit any other line. (The OPTION
-- line below it, current_code_since, must stay the same as in A_count.sql.)
--
-- BEFORE YOU RUN IT
--   * The current tracker code is deployed, and EVERY tracking process is
--     stopped (-track, and the listener unless it runs with -listen-only).
--     Wait until each process has exited.
--   * expected_count is set as above.
--
-- WHAT IT DOES (one transaction, all or nothing)
--   1. Picks the calls (same selection as A_count.sql: first calls, status
--      tracking/done/error, pool_dex 'uniswap-v4', and either not tracked by
--      the current code (no entry_liq_q in the on-chain state, or no
--      pre-call row computed at/after current_code_since), or labelled Pons
--      V2 without a pons_curve key in the state) and locks them.
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
--   Right after a successful run it finds 0 calls (they are all 'pending'
--   with no pool), so with the same expected_count it stops and changes
--   nothing. Once the tracker has re-tracked them, a few can match again (a
--   v4 call whose entry still cannot be found has no entry_liq_q; a call
--   labelled Pons V2 that is not a Pons token is on v4 without pons_curve)
--   and would be reset again. Do NOT lower or raise expected_count to "make
--   it run".
--
-- pgAdmin: run the whole file with F5. Auto-commit can stay ON (the file has
-- its own BEGIN/COMMIT). The "WARNING: there is no transaction in progress"
-- from the first line and "NOTICE: schema "pg_temp" does not exist, skipping"
-- are expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.v4retrack_params, pg_temp.v4retrack;
CREATE TEMP TABLE v4retrack_params AS
SELECT -1    AS expected_count,                                    -- <<< EDIT: the "selected" value of A_count.sql's TOTAL row
       NULL::timestamptz AS current_code_since,  -- OPTION: NULL = every v4 row, or the -track restart time in quotes; same in A, B, C
       now() AS reset_at;

-- The calls to reset, locked (FOR UPDATE) so that a tracker save of one of them
-- waits for this transaction instead of interleaving with it.
CREATE TEMP TABLE v4retrack AS
SELECT t.call_id, t.status AS old_status,
       COALESCE(jsonb_typeof(t.onchain) = 'object'
                AND jsonb_typeof(t.onchain -> 'entry_liq_q') = 'number', false) AS had_liq,
       COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false) AS had_curve
FROM scout_call_tracking t
JOIN (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
      WHERE post_kind IS DISTINCT FROM 'update'
      ORDER BY lower(contract_address), message_date, id) fc ON fc.id = t.call_id
CROSS JOIN v4retrack_params p
WHERE t.status IN ('tracking','done','error')
  AND t.pool_dex = 'uniswap-v4'
  AND (   NOT COALESCE(jsonb_typeof(t.onchain) = 'object'
                       AND jsonb_typeof(t.onchain -> 'entry_liq_q') = 'number'
                       AND EXISTS (SELECT 1 FROM scout_call_precall q
                                   WHERE q.call_id = t.call_id AND q.computed_at >= p.current_code_since), false)
       OR (    NOT COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false)
           AND EXISTS (SELECT 1 FROM scout_call_metrics m
                       WHERE m.call_id = t.call_id
                         AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
                              OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'))))
FOR UPDATE OF t;

-- Stop (and roll everything back) unless the count is exactly expected_count.
DO $$
DECLARE
  expected int;
  found    int;
BEGIN
  SELECT expected_count INTO expected FROM v4retrack_params;
  SELECT count(*) INTO found FROM v4retrack;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'B_reset: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'B_reset: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
END
$$;

DELETE FROM scout_call_returns WHERE call_id IN (SELECT call_id FROM v4retrack);
DELETE FROM scout_call_candles WHERE call_id IN (SELECT call_id FROM v4retrack);
DELETE FROM scout_call_precall WHERE call_id IN (SELECT call_id FROM v4retrack);

UPDATE scout_call_tracking t SET
    status = 'pending', next_check_at = now(), attempts = 0, error = NULL,
    last_checked_at = NULL, updated_at = now(), onchain = NULL,
    pool_address = NULL, pool_name = NULL, pool_dex = NULL, pool_created_at = NULL,
    entry_price_usd = NULL, entry_price_source = NULL, price_unit = NULL, entry_late_price_usd = NULL,
    current_price_usd = NULL, current_liquidity_usd = NULL, rugged = NULL,
    latest_price_usd = NULL, latest_return_pct = NULL, latest_checked_at = NULL, latest_trade_at = NULL
FROM v4retrack r
WHERE t.call_id = r.call_id;

COMMIT;

-- The result grid. Copy reset_at for C_check.sql and D_catchup.sql.
SELECT p.reset_at,
       p.current_code_since,
       (SELECT count(*) FROM v4retrack)                                   AS calls_reset,
       (SELECT count(*) FROM v4retrack WHERE NOT had_liq)                 AS without_entry_liq_q,
       (SELECT count(*) FROM v4retrack WHERE had_liq)                     AS with_entry_liq_q,
       (SELECT count(*) FROM v4retrack WHERE had_curve)                   AS with_pons_curve,
       (SELECT count(*) FROM v4retrack WHERE old_status = 'tracking')     AS were_tracking,
       (SELECT count(*) FROM v4retrack WHERE old_status = 'done')         AS were_done,
       (SELECT count(*) FROM v4retrack WHERE old_status = 'error')        AS were_error
FROM v4retrack_params p;
