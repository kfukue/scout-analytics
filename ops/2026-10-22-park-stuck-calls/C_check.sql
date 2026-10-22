-- =============================================================================
-- C_check.sql  (steps 4 and 7 of README.md)  READ-ONLY: changes nothing.
--
-- Shows the state of the calls B_park.sql parked, after B and after
-- R_rollback.sql. Needs the backup table
-- scout_call_tracking_bak_park_stuck_20261022 (run it before the DROP TABLE
-- at the end of README.md). Nothing to edit. Run the whole file (F5).
--
-- ONE grid, one row per (section, item). Columns: calls, then what to expect
-- after B and after R (README.md "What C shows").
--
--   section 'backup'  the rows B saved (the calls it parked)
--   section 'state'   what those calls have now, one class each:
--                       as B left it   'gave_up' with B's error text, not
--                                      written since B (updated_at = B's time)
--                       as before B    status, error, attempts, next_check_at
--                                      and updated_at all as before B (what R
--                                      restores)
--                       gave_up, written since B   still 'gave_up', but another
--                                      error text or updated_at moved
--                       anything else  another status (a running tracker wrote
--                                      over B, a start queued it again,
--                                      -retry-no-pool -retry-gave-up,
--                                      'repeat', or the tracker since R), or
--                                      the row is gone
--   section 'due'     backed-up calls the tracker would retry: in a status its
--                     horizon scan reads (DueTracking: pending, tracking,
--                     no_pool, error), and of those the ones due now
--                     (next_check_at at or before now)
--   section 'check'   backed-up calls the start-up SQL (scoutanalytics.sql)
--                     would queue again at the next start: an error with
--                     "context canceled", or an on-chain state version
--                     under 2
--   section 'listed'  listed calls B did not park
--   section 'info'    every tracking row in status error / gave_up
--
-- The list of 20 calls is the same as in A_count.sql and B_park.sql.
-- =============================================================================

WITH bak AS (
  SELECT b.*, t.status, t.error, t.next_check_at, t.onchain,
         CASE WHEN t.call_id IS NULL THEN 'else'
              WHEN t.status = 'gave_up' AND t.error IS NOT DISTINCT FROM b.new_error
               AND t.updated_at = b.parked_at THEN 'as_b'
              WHEN t.status IS NOT DISTINCT FROM b.old_status AND t.error IS NOT DISTINCT FROM b.old_error
               AND t.attempts IS NOT DISTINCT FROM b.old_attempts
               AND t.next_check_at IS NOT DISTINCT FROM b.old_next_check_at
               AND t.updated_at IS NOT DISTINCT FROM b.old_updated_at THEN 'before_b'
              WHEN t.status = 'gave_up' THEN 'gave_up_since'
              ELSE 'else' END AS state
  FROM scout_call_tracking_bak_park_stuck_20261022 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id),
grid AS (
  SELECT 1 AS ord, 'backup' AS section, 'calls B parked (rows in the backup table)' AS item, count(*) AS calls,
         'B''s TOTAL, always' AS after_b, 'B''s TOTAL, always' AS after_r
  FROM bak
  UNION ALL
  SELECT 2, 'state', 'as B left it: gave_up with B''s error text, not written since', count(*),
         'B''s TOTAL', '0'
  FROM bak WHERE state = 'as_b'
  UNION ALL
  SELECT 3, 'state', 'as before B: old status, error, attempts, next_check_at and updated_at (restored by R)', count(*),
         '0', 'the rows R restored (fewer once the tracker runs again)'
  FROM bak WHERE state = 'before_b'
  UNION ALL
  SELECT 4, 'state', 'gave_up, but written since B (another error text or updated_at moved)', count(*),
         '0', '0'
  FROM bak WHERE state = 'gave_up_since'
  UNION ALL
  SELECT 5, 'state', 'anything else (another status, or the row is gone)', count(*),
         '0', '0 at first; grows once the tracker runs again'
  FROM bak WHERE state = 'else'
  UNION ALL
  SELECT 6, 'due', 'backed-up calls in a status the tracker retries (pending, tracking, no_pool, error)', count(*),
         '0', 'the rows R restored'
  FROM bak WHERE status IN ('pending','tracking','no_pool','error')
  UNION ALL
  SELECT 7, 'due', 'of those, due now (next_check_at at or before now)', count(*),
         '0', 'the rows R restored whose old next_check_at has passed (normally all)'
  FROM bak WHERE status IN ('pending','tracking','no_pool','error') AND next_check_at <= now()
  UNION ALL
  SELECT 8, 'check', 'backed-up calls the next start would queue again (context canceled, or state version under 2)', count(*),
         '0', '0'
  FROM bak
  WHERE (status IN ('gave_up', 'error') AND error LIKE '%context canceled%')
     OR (onchain IS NOT NULL AND COALESCE((onchain->>'v')::int, 0) < 2
         AND status IN ('done', 'tracking', 'error', 'gave_up'))
  UNION ALL
  SELECT 9, 'listed', 'listed calls B did not park (not in the backup table)', count(*),
         '20 less B''s TOTAL (0 on 22 Oct)', 'as after B'
  FROM unnest(ARRAY[2344, 2920, 2935, 2940, 2946, 3103, 3127, 3195, 3212, 3214, 3220, 3246, 3815, 5033, 5360, 5400, 6201, 6391, 8142, 10061]::int[]) AS l(call_id)
  WHERE NOT EXISTS (SELECT 1 FROM scout_call_tracking_bak_park_stuck_20261022 b WHERE b.call_id = l.call_id)
  UNION ALL
  SELECT 10, 'info', 'every tracking row in status error', count(*),
         'A''s two error rows (priority 1 + 0) less B''s TOTAL', 'up by the rows R restored'
  FROM scout_call_tracking WHERE status = 'error'
  UNION ALL
  SELECT 11, 'info', 'every tracking row in status gave_up', count(*),
         'up by B''s TOTAL', 'as before B'
  FROM scout_call_tracking WHERE status = 'gave_up')
SELECT section, item, calls, after_b, after_r
FROM grid
ORDER BY ord;
