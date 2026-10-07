-- =============================================================================
-- A_count.sql  (step 2 of README.md)  READ-ONLY: changes nothing.
--
-- Shows which calls B_reset.sql would reset. Run the whole file (F5).
-- Nothing to edit in this file (it has no EDIT line). The only optional
-- setting is the OPTION line near the top (current_code_since; NULL means
-- every v4 row is selected, see README.md); if you change it, change it the
-- same way in B_reset.sql and C_check.sql. Its value is shown in the grid.
--
-- The result is ONE grid, one row per (section, item):
--   section 'reason'   why a call is selected, as exclusive classes (each
--                      selected call is counted once), so these rows add up
--                      to TOTAL:
--                      * old_code: no entry_liq_q in the on-chain state, i.e.
--                        its entry was found before the rug guard (or no entry
--                        found yet)
--                      * rug_guard_before_usd_fix: entry_liq_q, pre-call stats
--                        computed before 2026-10-06 23:06 (USD-fix commit)
--                      * before_current_code: entry_liq_q, pre-call stats
--                        computed after that but before current_code_since
--                        (all of them while current_code_since is NULL)
--                      * no_precall_row: entry_liq_q but no pre-call row
--                      * current_code: tracked by the current code; selected
--                        ONLY because of the Pons rule below
--                      "+ pons_label" is added when the post says Pons V2 and
--                      the state has no pons_curve key (selected regardless)
--   section 'status'   the selected calls by their current tracking status
--   section 'TOTAL'    the number to type into B_reset.sql: its "selected"
--                      value
--   section 'excluded' first-call v4 rows (status tracking/done/error) that
--                      B leaves alone, per marker, for the PM
--   section 'info'     NOT part of the selection, for the PM
-- Columns returns_rows / candle_rows / precall_rows: rows B would delete
-- (NULL on the excluded and info rows).
--
-- Selection (the same text is in B_reset.sql and C_check.sql):
--   the FIRST call of each token (address compared without regard to case,
--   earliest message_date, lowest id on ties, update posts never count),
--   status 'tracking', 'done' or 'error', pool_dex = 'uniswap-v4', AND
--   (  NOT tracked by the current code: the on-chain state has an
--      entry_liq_q number AND its scout_call_precall row was computed at or
--      after current_code_since; anything else is selected
--   OR the post's launchpad or dex is Pons V2 (lower case, only a-z and 0-9
--      kept: 'ponsv2') and the on-chain state has no 'pons_curve' key )
-- =============================================================================

WITH params AS (SELECT NULL::timestamptz AS current_code_since),   -- OPTION: NULL = every v4 row, or the -track restart time in quotes; same in A, B, C
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
v4 AS (   -- every first-call v4 row B may look at, with its markers
  SELECT t.call_id, t.status,
         COALESCE(jsonb_typeof(t.onchain) = 'object'
                  AND jsonb_typeof(t.onchain -> 'entry_liq_q') = 'number', false) AS has_liq,
         COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false) AS has_curve,
         q.computed_at AS pre_at,
         COALESCE(jsonb_typeof(t.onchain) = 'object'
                  AND jsonb_typeof(t.onchain -> 'entry_liq_q') = 'number'
                  AND q.computed_at >= p.current_code_since, false) AS is_current,
         (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
          OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2') AS pons_label
  FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  CROSS JOIN params p
  LEFT JOIN scout_call_metrics m ON m.call_id = t.call_id
  LEFT JOIN scout_call_precall q ON q.call_id = t.call_id
  WHERE t.status IN ('tracking','done','error')
    AND t.pool_dex = 'uniswap-v4'),
flags AS (
  SELECT v.*,
         (v.pons_label AND NOT v.has_curve) AS pons_fix,
         CASE WHEN NOT v.has_liq THEN 'old_code (no entry_liq_q)'
              WHEN v.is_current THEN 'current_code (selected only by the Pons rule)'
              WHEN v.pre_at IS NULL THEN 'no_precall_row (entry_liq_q)'
              WHEN v.pre_at < '2026-10-06 23:06:25-07'::timestamptz
                THEN 'rug_guard_before_usd_fix (entry_liq_q, precall before 2026-10-06 23:06)'
              ELSE 'before_current_code (entry_liq_q, precall later, but before current_code_since or it is NULL)'
         END AS cls
  FROM v4 v),
sel AS (
  SELECT f.*,
         f.cls || CASE WHEN f.pons_fix THEN ' + pons_label' ELSE '' END AS reason,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = f.call_id) AS n_ret,
         (SELECT count(*) FROM scout_call_candles c WHERE c.call_id = f.call_id) AS n_candle,
         (SELECT count(*) FROM scout_call_precall q WHERE q.call_id = f.call_id) AS n_pre
  FROM flags f
  WHERE NOT f.is_current OR f.pons_fix),
grid AS (
  SELECT 1 AS ord, 'reason' AS section, reason AS item, count(*) AS selected,
         sum(n_ret) AS returns_rows, sum(n_candle) AS candle_rows, sum(n_pre) AS precall_rows
  FROM sel GROUP BY reason
  UNION ALL
  SELECT 2, 'status', status, count(*), sum(n_ret), sum(n_candle), sum(n_pre)
  FROM sel GROUP BY status
  UNION ALL
  SELECT 3, 'TOTAL', 'selected = expected_count for B_reset.sql', count(*),
         COALESCE(sum(n_ret), 0), COALESCE(sum(n_candle), 0), COALESCE(sum(n_pre), 0)
  FROM sel
  UNION ALL
  SELECT 4, 'excluded', 'current_code: entry_liq_q AND precall at/after current_code_since (B leaves them)',
         count(*), NULL, NULL, NULL
  FROM flags WHERE is_current AND NOT pons_fix
  UNION ALL
  SELECT 5, 'excluded', '  of which with a pons_curve key', count(*), NULL, NULL, NULL
  FROM flags WHERE is_current AND NOT pons_fix AND has_curve
  UNION ALL
  SELECT 6, 'info', 'v4 rows with entry_liq_q (any precall time; excluded only with a recent precall)',
         count(*), NULL, NULL, NULL
  FROM flags WHERE has_liq
  UNION ALL
  SELECT 7, 'info', 'v4 rows with a pons_curve key (any precall time)', count(*), NULL, NULL, NULL
  FROM flags WHERE has_curve
  UNION ALL
  SELECT 8, 'info', 'if re-track-all: every first-call v4 row, status tracking/done/error', count(*), NULL, NULL, NULL
  FROM flags
  UNION ALL
  SELECT 10, 'info', 'current_code_since in use: ' || COALESCE(p.current_code_since::text, 'NULL (every v4 row is selected)'),
         NULL, NULL, NULL, NULL
  FROM params p
  UNION ALL
  SELECT 11, 'info', 'v4 rows with entry_liq_q and precall at/after 2026-10-07 11:33:54-07 (14052f8 commit): the most a restart-time cutoff could skip',
         count(*), NULL, NULL, NULL
  FROM flags WHERE has_liq AND pre_at >= '2026-10-07 11:33:54-07'::timestamptz
  UNION ALL
  SELECT 9, 'info', 'first-call v4 rows in another status (pending, no_pool, gave_up: never selected)',
         count(*), NULL, NULL, NULL
  FROM scout_call_tracking t JOIN fc ON fc.id = t.call_id
  WHERE t.pool_dex = 'uniswap-v4' AND t.status NOT IN ('tracking','done','error'))
SELECT section, item, selected, returns_rows, candle_rows, precall_rows
FROM grid
ORDER BY ord, selected DESC, item;
