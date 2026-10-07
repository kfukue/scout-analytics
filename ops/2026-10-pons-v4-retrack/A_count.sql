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
--   pool_dex = 'uniswap-v4',
--   the post's launchpad OR dex, normalised (lower case, everything but a-z
--   and 0-9 removed), is 'ponsv2' ("Pons V2", "pons_v2", "PONS-V2", ...), AND
--   the on-chain state was NOT written by the Pons-aware code (it is not a
--   JSON object with a 'pons_curve' key). Those rows are tracked right
--   already and are excluded.
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
--   excluded_pons_state  NOT selected: rows that match every other rule but
--                     whose on-chain state has a 'pons_curve' key, i.e. the
--                     Pons-aware code wrote them; B leaves them alone.
--                     (Not part of "selected"; a status row may show
--                     selected = 0 if it only has excluded rows.)
--   returns_rows / candle_rows / precall_rows   rows B would delete
-- =============================================================================

WITH fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
            WHERE post_kind IS DISTINCT FROM 'update'
            ORDER BY lower(contract_address), message_date, id),
sel AS (    -- what B resets
  SELECT t.call_id, t.status, t.priority, t.rugged,
         regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2' AS by_lp,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = t.call_id) AS n_ret,
         (SELECT count(*) FROM scout_call_candles c WHERE c.call_id = t.call_id) AS n_candle,
         (SELECT count(*) FROM scout_call_precall q WHERE q.call_id = t.call_id) AS n_pre
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  LEFT JOIN scout_call_metrics m ON m.call_id = t.call_id
  WHERE t.status IN ('tracking','done','error')
    AND t.pool_dex = 'uniswap-v4'
    AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
         OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2')
    AND NOT COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false)),
excl AS (   -- same rules, but written by the Pons-aware code: NOT reset
  SELECT t.call_id, t.status
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  LEFT JOIN scout_call_metrics m ON m.call_id = t.call_id
  WHERE t.status IN ('tracking','done','error')
    AND t.pool_dex = 'uniswap-v4'
    AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
         OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2')
    AND COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false)),
allrows AS (
  SELECT status, true AS is_sel, priority, rugged, by_lp, n_ret, n_candle, n_pre FROM sel
  UNION ALL
  SELECT status, false, NULL, NULL, NULL, 0, 0, 0 FROM excl)
SELECT CASE WHEN GROUPING(status) = 1 THEN 'TOTAL' ELSE status END AS status,
       count(*) FILTER (WHERE is_sel)                    AS selected,
       count(*) FILTER (WHERE is_sel AND by_lp)          AS by_launchpad,
       count(*) FILTER (WHERE is_sel AND NOT by_lp)      AS dex_only,
       count(*) FILTER (WHERE is_sel AND priority = 0)   AS live,
       count(*) FILTER (WHERE is_sel AND priority <> 0)  AS backfilled,
       count(*) FILTER (WHERE is_sel AND rugged)         AS rugged_now,
       count(*) FILTER (WHERE NOT is_sel)                AS excluded_pons_state,
       COALESCE(sum(n_ret)    FILTER (WHERE is_sel), 0)  AS returns_rows,
       COALESCE(sum(n_candle) FILTER (WHERE is_sel), 0)  AS candle_rows,
       COALESCE(sum(n_pre)    FILTER (WHERE is_sel), 0)  AS precall_rows
FROM allrows
GROUP BY ROLLUP (status)
ORDER BY GROUPING(status), status;
