-- =============================================================================
-- A_count.sql  (step 2 of README.md)  READ-ONLY: changes nothing.
--
-- Shows which calls B_reset.sql would reset. Run the whole file (F5).
-- The result is ONE grid: one row per tracking status, then a TOTAL row.
-- Paste the whole grid to the PM. The number to type into B_reset.sql is
-- the "selected" column of the TOTAL row.
--
-- Selection (the same text is in B_reset.sql and C_check.sql):
--   the FIRST call of each token (address compared without regard to case,
--   earliest message_date, lowest id on ties, update posts never count),
--   tracking status 'tracking', 'done' or 'error',
--   pool_dex = 'uniswap-v4', AND
--   the post's launchpad OR dex, normalised (lower case, everything but a-z
--   and 0-9 removed), is 'ponsv2' ("Pons V2", "pons_v2", "PONS-V2", ...).
--   A call without a scout_call_metrics row is not selected.
--
-- Why: before Pons V2 support, Pons V2 tokens called BEFORE their graduation
-- were tracked on the v4 pool, with the entry from the first v4 trade after the
-- graduation. The database does not tell those apart from tokens that had
-- already graduated at the call, so all of them are tracked again.
--
-- Columns:
--   status            current tracking status (TOTAL = all)
--   selected          calls B would reset (TOTAL row: type this into B)
--   by_launchpad      selected because of the launchpad (the rest: dex only)
--   dex_only          launchpad not Pons V2, but the posted dex is
--   live / backfilled priority 0 / other
--   rugged_now        rugged = true today
--   new_code_state    on-chain state already written by the Pons-aware code
--                     (has a 'pons_curve' key). Expected 0 before the reset;
--                     see README "If new_code_state is not 0".
--   returns_rows / candle_rows / precall_rows   rows B would delete
-- =============================================================================

WITH fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
            WHERE post_kind IS DISTINCT FROM 'update'
            ORDER BY lower(contract_address), message_date, id),
sel AS (
  SELECT t.call_id, t.status, t.priority, t.rugged,
         regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2' AS by_lp,
         COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false) AS new_state,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = t.call_id) AS n_ret,
         (SELECT count(*) FROM scout_call_candles c WHERE c.call_id = t.call_id) AS n_candle,
         (SELECT count(*) FROM scout_call_precall q WHERE q.call_id = t.call_id) AS n_pre
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  LEFT JOIN scout_call_metrics m ON m.call_id = t.call_id
  WHERE t.status IN ('tracking','done','error')
    AND t.pool_dex = 'uniswap-v4'
    AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
         OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'))
SELECT CASE WHEN GROUPING(status) = 1 THEN 'TOTAL' ELSE status END AS status,
       count(*)                                          AS selected,
       count(*) FILTER (WHERE by_lp)                     AS by_launchpad,
       count(*) FILTER (WHERE NOT by_lp)                 AS dex_only,
       count(*) FILTER (WHERE priority = 0)              AS live,
       count(*) FILTER (WHERE priority <> 0)             AS backfilled,
       count(*) FILTER (WHERE rugged)                    AS rugged_now,
       count(*) FILTER (WHERE new_state)                 AS new_code_state,
       COALESCE(sum(n_ret), 0)                           AS returns_rows,
       COALESCE(sum(n_candle), 0)                        AS candle_rows,
       COALESCE(sum(n_pre), 0)                           AS precall_rows
FROM sel
GROUP BY ROLLUP (status)
ORDER BY GROUPING(status), status;
