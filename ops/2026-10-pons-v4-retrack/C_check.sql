-- =============================================================================
-- C_check.sql  (step 7 of README.md)  READ-ONLY: changes nothing.
--
-- Paste B's reset_at into the line marked EDIT (keep the quotes) and run the
-- whole file (F5). The query fails until the placeholder is replaced.
-- Run it again after one or two tracker cycles (the tracker log says
-- "tracking: processed N call(s)").
--
-- "Pons V2 first calls" below = first calls (same rule as A and B) whose
-- launchpad or dex, normalised, is 'ponsv2'.
--
-- Result: ONE row.
--   not_reset         Must be 0. Calls that match A/B's selection now and were
--                     last written BEFORE reset_at, i.e. B missed them.
--                     (The latest-price pass never changes pool_dex, so this
--                     stays meaningful while the tracker works.)
--   race_victims      Must be 0, otherwise run D_catchup.sql. Pons V2 first
--                     calls written at/after reset_at whose on-chain state says
--                     the pre-call stats or a horizon are done while that row
--                     is missing: a tracker that was still running saved them
--                     back with their OLD state over the reset.
--   waiting           Pons V2 first calls created before reset_at, reset
--                     at/after it, still 'pending' (falls to 0 as the tracker
--                     works).
--   retracked_so_far  Pons V2 first calls that existed before reset_at and
--                     whose pre-call stats were computed after it (grows).
--   now_on_curve      Pons V2 first calls written at/after reset_at that are
--                     now tracked on the bonding curve (pool_dex 'pons-curve'):
--                     the calls that were wrong before.
--   now_on_v4         ... now on 'uniswap-v4' (graduated before the call: the
--                     old result was right and has been recomputed).
--   now_other         ... any other status/pool (no_pool, gave_up, error, ...).
--   v4_without_pons_state  Should be 0. Of now_on_v4, rows whose on-chain
--                     state has no 'pons_curve' key, i.e. not written by the
--                     Pons-aware code. If not 0, tell the PM.
-- =============================================================================

WITH params AS (SELECT 'PASTE reset_at FROM B HERE'::timestamptz AS reset_at),   -- <<< EDIT: B's reset_at
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
pons AS (   -- Pons V2 first calls, as they are now
  SELECT t.* FROM scout_call_tracking t
  JOIN fc ON fc.id = t.call_id
  JOIN scout_call_metrics m ON m.call_id = t.call_id
  WHERE regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
     OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'),
sel AS (    -- the selection of A and B, applied to the rows as they are now
  SELECT s.* FROM pons s
  WHERE s.status IN ('tracking','done','error') AND s.pool_dex = 'uniswap-v4'),
after AS (  -- Pons V2 first calls that existed before reset_at and were written at/after it
  SELECT s.* FROM pons s, params
  WHERE s.updated_at >= params.reset_at AND s.created_at < params.reset_at)
SELECT
  (SELECT count(*) FROM sel, params WHERE sel.updated_at < params.reset_at) AS not_reset,
  (SELECT count(*) FROM pons s, params
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
  (SELECT count(*) FROM pons s, params
    WHERE s.created_at < params.reset_at
      AND EXISTS (SELECT 1 FROM scout_call_precall q
                  WHERE q.call_id = s.call_id AND q.computed_at >= params.reset_at)) AS retracked_so_far,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done') AND pool_dex = 'pons-curve') AS now_on_curve,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done') AND pool_dex = 'uniswap-v4') AS now_on_v4,
  (SELECT count(*) FROM after WHERE status <> 'pending'
      AND NOT (status IN ('tracking','done') AND pool_dex IN ('pons-curve','uniswap-v4'))) AS now_other,
  (SELECT count(*) FROM after WHERE status IN ('tracking','done') AND pool_dex = 'uniswap-v4'
      AND NOT COALESCE(jsonb_typeof(onchain) = 'object' AND onchain ? 'pons_curve', false)) AS v4_without_pons_state;
