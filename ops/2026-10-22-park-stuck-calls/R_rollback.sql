-- =============================================================================
-- R_rollback.sql  (step 7 of README.md, only if B's result must be undone)
-- WRITES: puts the parked calls back as they were before B_park.sql, from the
-- backup table, so the tracker retries them again.
--
-- Nothing to edit. Run the whole file (F5).
--
-- WHAT IT DOES (one transaction, all or nothing)
--   Works ONLY from the backup table
--   scout_call_tracking_bak_park_stuck_20261022 (never from B's selection
--   again). For each backed-up call that is still exactly as B left it
--   (status 'gave_up', B's error text, and updated_at = B's time, i.e. not
--   written since B), it sets back the values from before B:
--     status, error, attempts, next_check_at (the old value, see README.md
--     "R: next_check_at") and updated_at.
--   Nothing else of the row changes. Calls written since B are left as they
--   are (a start queued them again, -retry-no-pool -retry-gave-up reset them,
--   they became 'repeat', ...): their new state is the program's.
--   Then it commits and shows ONE grid: backed up, restored now, and the rows
--   not restored, with why.
--
-- SAFE TO RUN AGAIN: a second run restores nothing (those rows are no longer
-- 'gave_up') and shows them as "already as before B". It is safe at any time
-- while the backup table exists, the tracker running or not.
--
-- IF IT STOPS WITH AN ERROR
--   Nothing was changed. Run
--       ROLLBACK;
--   in the same Query Tool tab, or close the tab.
--
-- pgAdmin: run the whole file with F5. Auto-commit can stay ON (the file has
-- its own BEGIN/COMMIT). The "WARNING: there is no transaction in progress"
-- from the first line and "NOTICE: table ... does not exist, skipping" are
-- expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.park_stuck_restored;
CREATE TEMP TABLE park_stuck_restored (call_id int PRIMARY KEY);

WITH upd AS (
  UPDATE scout_call_tracking t
     SET status = b.old_status, error = b.old_error, attempts = b.old_attempts,
         next_check_at = b.old_next_check_at, updated_at = b.old_updated_at
    FROM scout_call_tracking_bak_park_stuck_20261022 b
   WHERE t.call_id = b.call_id
     AND t.status = 'gave_up'
     AND t.error IS NOT DISTINCT FROM b.new_error
     AND t.updated_at = b.parked_at
  RETURNING t.call_id)
INSERT INTO park_stuck_restored SELECT call_id FROM upd;

COMMIT;

-- The result grid. Each backed-up call is counted once.
WITH s AS (
  SELECT b.call_id,
         CASE WHEN r.call_id IS NOT NULL THEN 'restored'
              WHEN t.call_id IS NULL THEN 'else'
              WHEN t.status IS NOT DISTINCT FROM b.old_status AND t.error IS NOT DISTINCT FROM b.old_error
               AND t.attempts IS NOT DISTINCT FROM b.old_attempts
               AND t.next_check_at IS NOT DISTINCT FROM b.old_next_check_at
               AND t.updated_at IS NOT DISTINCT FROM b.old_updated_at THEN 'already'
              ELSE 'else' END AS state
  FROM scout_call_tracking_bak_park_stuck_20261022 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id
  LEFT JOIN park_stuck_restored r ON r.call_id = b.call_id)
SELECT item, calls
FROM (
  SELECT 1 AS ord, 'backed up by B' AS item, count(*) AS calls FROM s
  UNION ALL
  SELECT 2, 'restored now', count(*) FROM s WHERE state = 'restored'
  UNION ALL
  SELECT 3, 'skipped: already as before B (restored by an earlier run of R)', count(*)
  FROM s WHERE state = 'already'
  UNION ALL
  SELECT 4, 'skipped: written since B (another status or error text, updated_at moved, or the row is gone)', count(*)
  FROM s WHERE state = 'else') g
ORDER BY ord;
