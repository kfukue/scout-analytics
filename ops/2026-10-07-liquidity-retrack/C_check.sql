-- =============================================================================
-- C_check.sql  (step 7 of README.md)  READ-ONLY: changes nothing.
--
-- THE ONLY LINE TO EDIT is reset_at, on LINE 45: it is the only line of
-- this file with the EDIT marker. Find it with Ctrl+F: type
-- three less-than signs, a space and EDIT. Replace the text between the quotes
-- with B's reset_at (keep the quotes). The query fails until you do.
-- (The OPTION line below it, current_code_since, must stay the same as in
-- A_count.sql and B_reset.sql.) Then run the whole file (F5).
-- Run it again after one or two tracker cycles (the tracker log says
-- "tracking: processed N call(s)").
--
-- Result: ONE row.
--   not_reset           Must be 0. Calls that match the selection now and were
--                       last written BEFORE reset_at, i.e. B missed them.
--   race_victims        Must be 0, otherwise run D_catchup.sql. First calls
--                       written at/after reset_at whose on-chain state says the
--                       pre-call stats or a horizon are done while that row is
--                       missing: a tracker that was still running saved them
--                       back with their OLD state over the reset.
--   waiting             Calls created before reset_at, reset at/after it, that
--                       are still 'pending' (falls to 0 as the tracker works;
--                       it also counts calls still waiting from the other
--                       re-tracks).
--   retracked_so_far    First calls that existed before reset_at and whose
--                       pre-call stats were computed after it (grows as the
--                       tracker works; it also counts calls of the other
--                       re-tracks that finish after reset_at).
--   now_on_v4           First calls created before and written at/after
--                       reset_at, status tracking/done, on 'uniswap-v4'.
--   now_rugged          ... of those, rugged = true (the v4 liquidity check
--                       found the pool drained).
--   v4_without_entry_liq_after
--                       First calls created before and written at/after
--                       reset_at, status tracking/done/error, on 'uniswap-v4',
--                       whose state has no entry_liq_q (no entry found yet).
--                       Should stay small; paste it to the PM if it stays
--                       above 0 across several runs.
--   pons_label_v4_without_curve_after
--                       Same window, on 'uniswap-v4', post labelled Pons V2,
--                       state without pons_curve: not a Pons token although
--                       labelled so. Paste it to the PM if not 0.
-- =============================================================================

WITH params AS (SELECT 'PASTE reset_at FROM B HERE'::timestamptz AS reset_at,                -- <<< EDIT: B's reset_at, inside the quotes
                       NULL::timestamptz AS current_code_since),          -- OPTION: NULL = every v4 row, or the -track restart time in quotes; same in A, B, C
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
first_t AS (SELECT t.* FROM scout_call_tracking t JOIN fc ON fc.id = t.call_id),
sel AS (   -- the selection of A and B, applied to the rows as they are now
  SELECT t.* FROM first_t t CROSS JOIN params p
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
                                OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'))))),
after AS (  -- first calls that existed before reset_at and were written at/after it
  SELECT s.* FROM first_t s, params
  WHERE s.updated_at >= params.reset_at AND s.created_at < params.reset_at)
SELECT
  (SELECT current_code_since FROM params) AS current_code_since,
  (SELECT count(*) FROM sel, params WHERE sel.updated_at < params.reset_at) AS not_reset,
  (SELECT count(*) FROM first_t s, params
    WHERE s.status IN ('tracking','done','error')
      AND s.updated_at >= params.reset_at
      AND jsonb_typeof(s.onchain) = 'object'
      AND (   (CASE WHEN jsonb_typeof(s.onchain -> 'pre_done') = 'boolean'
                    THEN (s.onchain ->> 'pre_done')::boolean ELSE false END
               AND NOT EXISTS (SELECT 1 FROM scout_call_precall q WHERE q.call_id = s.call_id))
           OR EXISTS (SELECT 1 FROM jsonb_object_keys(CASE WHEN jsonb_typeof(s.onchain -> 'done') = 'object'
                                                           THEN s.onchain -> 'done' ELSE '{}'::jsonb END) AS d(horizon)
                      WHERE NOT EXISTS (SELECT 1 FROM scout_call_returns r
                                        WHERE r.call_id = s.call_id AND r.horizon = d.horizon)))) AS race_victims,
  (SELECT count(*) FROM after WHERE status = 'pending') AS waiting,
  (SELECT count(*) FROM first_t s, params
    WHERE s.created_at < params.reset_at
      AND EXISTS (SELECT 1 FROM scout_call_precall q
                  WHERE q.call_id = s.call_id AND q.computed_at >= params.reset_at)) AS retracked_so_far,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done') AND pool_dex = 'uniswap-v4') AS now_on_v4,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done') AND pool_dex = 'uniswap-v4'
      AND rugged) AS now_rugged,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done','error') AND pool_dex = 'uniswap-v4'
      AND NOT COALESCE(jsonb_typeof(onchain) = 'object'
                       AND jsonb_typeof(onchain -> 'entry_liq_q') = 'number', false)) AS v4_without_entry_liq_after,
  (SELECT count(*) FROM after a WHERE a.status IN ('tracking','done','error') AND a.pool_dex = 'uniswap-v4'
      AND NOT COALESCE(jsonb_typeof(a.onchain) = 'object' AND a.onchain ? 'pons_curve', false)
      AND EXISTS (SELECT 1 FROM scout_call_metrics m
                  WHERE m.call_id = a.call_id
                    AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
                         OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'))) AS pons_label_v4_without_curve_after;
