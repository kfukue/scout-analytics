-- =============================================================================
-- A_count.sql  (step 2 of README.md)  READ-ONLY: changes nothing.
--
-- Shows which of the 20 stuck calls B_park.sql would park as 'gave_up'.
-- Run the whole file (F5). Nothing to edit in this file. The result is ONE
-- grid; the "expected" column says what each row should show.
--
--   section 'TOTAL'  the number to type into B_park.sql: its "calls" value.
--                    B parks a call only if it is in the list below AND its
--                    tracking row has status 'error' AND priority 1.
--   section 'check'  rows that must be 0 (else STOP: do not run B, send this
--                    whole grid to the PM):
--                      * listed calls B would not park (a call that finished
--                        or changed since 22 Oct 09:30 PDT (16:30 UTC));
--                        their 'call' rows below say why
--                      * calls to park whose error contains "context
--                        canceled" (B refuses them: the start-up SQL would
--                        queue them again at the next start)
--                      * calls to park whose on-chain state version is under
--                        2 (the start-up SQL would queue them again too)
--   section 'info'   for the PM, no expected value
--   section 'call'   one row per listed call (20 rows): its tracking row now,
--                    and "parked by B" or why not
--
-- The 20 calls (the owner's read-only query, 22 Oct 2026, 09:30 PDT
-- (16:30 UTC)): the same list is in B_park.sql and C_check.sql.
-- =============================================================================

WITH listed AS (
  SELECT unnest(ARRAY[2344, 2920, 2935, 2940, 2946, 3103, 3127, 3195, 3212, 3214, 3220, 3246, 3815, 5033, 5360, 5400, 6201, 6391, 8142, 10061]::int[]) AS call_id),
cur AS (
  SELECT l.call_id, t.status, t.priority, t.attempts, t.entry_at, t.next_check_at,
         t.onchain->>'quote_sym'            AS quote_sym,
         (t.onchain->>'v')::int             AS state_v,
         t.onchain IS NOT NULL              AS has_state,
         t.entry_price_usd IS NOT NULL      AS has_entry_price,
         t.entry_late_price_usd IS NOT NULL AS has_late_entry,
         (SELECT count(*) FROM scout_call_returns r WHERE r.call_id = l.call_id AND r.status = 'done') AS done_horizons,
         t.error,
         CASE WHEN t.call_id IS NULL THEN 'NOT parked: no tracking row'
              WHEN t.status IS DISTINCT FROM 'error' THEN 'NOT parked: status is ' || t.status
              WHEN t.priority IS DISTINCT FROM 1 THEN 'NOT parked: priority is ' || t.priority
              ELSE 'parked by B' END AS fate
  FROM listed l
  LEFT JOIN scout_call_tracking t ON t.call_id = l.call_id),
grid AS (
  SELECT 1 AS ord, 'TOTAL' AS section, 'calls B parks (listed, status error, priority 1) = expected_count for B_park.sql' AS item,
         count(*) AS calls, NULL::int AS call_id, NULL::text AS status, NULL::smallint AS priority, NULL::int AS attempts,
         NULL::timestamptz AS entry_at, NULL::timestamptz AS next_check_at, NULL::text AS quote_sym, NULL::int AS state_v,
         NULL::boolean AS has_entry_price, NULL::boolean AS has_late_entry, NULL::bigint AS done_horizons,
         NULL::text AS error_excerpt,
         '20: type this into B_park.sql' AS expected
  FROM cur WHERE fate = 'parked by B'
  UNION ALL
  SELECT 2, 'check', 'listed calls B would NOT park (see their call rows)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
         '0: else STOP and send this grid to the PM'
  FROM cur WHERE fate <> 'parked by B'
  UNION ALL
  SELECT 3, 'check', 'calls to park whose error contains "context canceled" (B refuses them)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
         '0: else STOP and send this grid to the PM'
  FROM cur WHERE fate = 'parked by B' AND error LIKE '%context canceled%'
  UNION ALL
  SELECT 4, 'check', 'calls to park with an on-chain state version under 2 (a start would queue them again)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
         '0: else STOP and send this grid to the PM'
  FROM cur WHERE fate = 'parked by B' AND has_state AND COALESCE(state_v, 0) < 2
  UNION ALL
  SELECT 5, 'info', 'calls to park without an on-chain state yet', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM cur WHERE fate = 'parked by B' AND NOT has_state
  UNION ALL
  SELECT 6, 'info', 'calls to park without an entry price', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM cur WHERE fate = 'parked by B' AND NOT has_entry_price
  UNION ALL
  SELECT 7, 'info', 'calls to park with a late entry price (the ML data leaves gave_up calls out)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM cur WHERE fate = 'parked by B' AND has_late_entry
  UNION ALL
  SELECT 8, 'info', 'every tracking row in status error, priority 1 (any call)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM scout_call_tracking WHERE status = 'error' AND priority = 1
  UNION ALL
  SELECT 9, 'info', 'every tracking row in status error, priority 0 (any call)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM scout_call_tracking WHERE status = 'error' AND priority = 0
  UNION ALL
  SELECT 10, 'info', 'every tracking row in status gave_up (any call)', count(*),
         NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'for the PM'
  FROM scout_call_tracking WHERE status = 'gave_up'
  UNION ALL
  SELECT 11, 'call', fate, CASE WHEN fate = 'parked by B' THEN 1 ELSE 0 END::bigint,
         call_id, status, priority, attempts, entry_at, next_check_at, quote_sym, state_v,
         has_entry_price, has_late_entry, done_horizons, left(error, 200),
         CASE WHEN fate = 'parked by B' THEN 'parked by B' ELSE 'for the PM: B leaves it' END
  FROM cur)
SELECT section, item, calls, call_id, status, priority, attempts, entry_at, next_check_at, quote_sym, state_v,
       has_entry_price, has_late_entry, done_horizons, error_excerpt, expected
FROM grid
ORDER BY ord, call_id;
