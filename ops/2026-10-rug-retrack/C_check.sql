-- =============================================================================
-- C_check.sql  (step 7 of README.txt)  READ-ONLY: changes nothing.
--
-- Paste B's reset_at into the line marked EDIT (keep the quotes), keep
-- quote_side_usd the same as in A and B, and run the whole file (F5).
-- The query fails until the placeholder is replaced.
-- Run it again after one or two tracker cycles (the tracker log says
-- "tracking: processed N call(s)").
--
-- Result: ONE row.
--   not_reset         Must be 0. Calls that match the selection now and were
--                     last written BEFORE reset_at, i.e. B missed them.
--                     Only meaningful on a run before the tracker has worked
--                     for a while: the tracker's latest-price pass updates
--                     latest_return_pct, rugged and current_liquidity_usd
--                     WITHOUT changing updated_at, so later an untouched call
--                     that the new rug rule flags can show up here. That is
--                     the new code at work, not a call B missed.
--   race_victims      Must be 0, otherwise run D_catchup.sql. First calls
--                     written at/after reset_at whose on-chain state says the
--                     pre-call stats or a horizon are done while that row is
--                     missing: a tracker that was still running saved them back
--                     with their OLD state over the reset.
--   waiting           Calls created before reset_at, reset at/after it, that
--                     are still 'pending' (falls to 0 as the tracker works).
--   retracked_so_far  First calls that existed before reset_at and whose
--                     pre-call stats were computed after it (grows as the
--                     tracker works; it can also include calls that were
--                     already pending before the reset).
-- =============================================================================

WITH params AS (SELECT 'PASTE reset_at FROM B HERE'::timestamptz AS reset_at,   -- <<< EDIT: B's reset_at
                       500::numeric AS quote_side_usd),                          -- <<< rug threshold (same as A and B)
fc AS (SELECT DISTINCT ON (lower(contract_address)) id FROM scout_calls
       WHERE post_kind IS DISTINCT FROM 'update'
       ORDER BY lower(contract_address), message_date, id),
first_t AS (SELECT t.* FROM scout_call_tracking t JOIN fc ON fc.id = t.call_id),
sel AS (   -- the selection of A and B, applied to the rows as they are now
  SELECT t.* FROM first_t t CROSS JOIN params p
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
                    END))))
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
                  WHERE q.call_id = s.call_id AND q.computed_at >= params.reset_at)) AS retracked_so_far;
