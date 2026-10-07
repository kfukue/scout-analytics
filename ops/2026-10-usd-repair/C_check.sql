-- =============================================================================
-- C_check.sql  (step 7 of README.md)  READ-ONLY: changes nothing.
--
-- THE ONLY LINE TO EDIT is reset_at, on LINE 40: it is the only line
-- of this file with the EDIT marker. Find it with Ctrl+F: type three
-- less-than signs, a space and EDIT. Replace the text between the quotes with
-- B's reset_at (keep the quotes). The query fails until you do.
-- (The OPTION line below it, include_gap_heuristic, must stay the same as in
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
--                       it also counts calls still waiting from the rug and
--                       Pons-v4 re-tracks).
--   retracked_so_far    First calls that existed before reset_at and whose
--                       pre-call stats were computed after it (grows as the
--                       tracker works; it also counts calls of the other
--                       re-tracks that finish after reset_at).
--   quote_units_after   First calls created before and written at/after
--                       reset_at that are in quote units again (price_unit not
--                       'usd'): the new code does that only when every USD
--                       source is absent. Paste it to the PM; should be small.
--   candle_usd_errors_after
--                       First calls created before and written at/after
--                       reset_at with a 'candles to +...' USD error again (the
--                       new code keeps nothing then and retries); paste it to
--                       the PM if it stays above 0 across several runs.
-- =============================================================================

WITH params AS (SELECT 'PASTE reset_at FROM B HERE'::timestamptz AS reset_at,   -- <<< EDIT: B's reset_at, inside the quotes
                       false AS include_gap_heuristic),                          -- OPTION: same value as in A and B
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
first_t AS (SELECT t.* FROM scout_call_tracking t JOIN fc ON fc.id = t.call_id),
sel AS (   -- the selection of A and B, applied to the rows as they are now
  SELECT t.* FROM first_t t CROSS JOIN params p
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
                                    AND c.bucket_start = date_trunc('hour', r.last_trade_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')))))
SELECT
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
  (SELECT count(*) FROM first_t s, params
    WHERE s.status = 'pending' AND s.updated_at >= params.reset_at
      AND s.created_at < params.reset_at) AS waiting,
  (SELECT count(*) FROM first_t s, params
    WHERE s.created_at < params.reset_at
      AND EXISTS (SELECT 1 FROM scout_call_precall q
                  WHERE q.call_id = s.call_id AND q.computed_at >= params.reset_at)) AS retracked_so_far,
  (SELECT count(*) FROM first_t s, params
    WHERE s.updated_at >= params.reset_at AND s.created_at < params.reset_at
      AND s.status IN ('tracking','done','error')
      AND s.price_unit IS NOT NULL AND s.price_unit <> 'usd') AS quote_units_after,
  (SELECT count(*) FROM first_t s, params
    WHERE s.updated_at >= params.reset_at AND s.created_at < params.reset_at
      AND s.error LIKE 'candles to +%'
      AND (s.error LIKE '%no USD source%' OR s.error LIKE '%no trades in the%')) AS candle_usd_errors_after;
