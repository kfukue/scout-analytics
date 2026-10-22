-- =============================================================================
-- B_park.sql  (step 3 of README.md)  WRITES: parks the stuck calls as
-- 'gave_up', so the tracker stops retrying them.
--
-- THE ONLY LINE TO EDIT is expected_count: it is the only line of this file
-- with the EDIT marker. Find it with Ctrl+F: type three less-than signs, a
-- space and EDIT. Put there the "calls" value of the TOTAL row of
-- A_count.sql. Do not edit any other line.
--
-- BEFORE YOU RUN IT
--   * The tracker is STOPPED: -track, and the listener if it runs without
--     -listen-only (README.md step 1). A running tracker holds its own copy
--     of the calls it is working on and writes 'error' back over B's
--     'gave_up' when that call ends or when it is stopped.
--   * A_count.sql's 'check' rows are all 0 (README.md step 2). If not: STOP.
--   * expected_count is set as above.
--
-- WHAT IT DOES (one transaction, all or nothing)
--   1. Picks the calls: in the list below AND status 'error' AND priority 1
--      (the same as A_count.sql's TOTAL), locks their tracking rows and saves
--      them in the backup table scout_call_tracking_bak_park_stuck_20261022
--      (call_id, old_status, old_error, old_attempts, old_next_check_at,
--      old_last_checked_at, old_updated_at, new_error, parked_at). The
--      backup is a real table: R_rollback.sql needs it.
--   2. If their number is not exactly expected_count, it STOPS with an error
--      and changes nothing (no backup table either). Read the number in the
--      error message, compare it with A, and only then set expected_count
--      again.
--   3. If any of them has "context canceled" in its error, it STOPS the same
--      way (the start-up SQL would queue such a row again at the next
--      start). Send A's grid to the PM.
--   4. Sets on exactly the backed-up rows:
--        status     = 'gave_up'
--        error      = 'parked 2026-10-22 (ops/2026-10-22-park-stuck-calls): '
--                     followed by the old error (its first 400 characters,
--                     then ' [...]' if it was longer; the whole old error is
--                     in the backup table)
--        updated_at = the time of B (as the tracker does when it sets
--                     gave_up itself)
--      Nothing else of the row changes: attempts, next_check_at,
--      last_checked_at, the on-chain state, prices and every other table
--      stay as they are.
--   5. Checks that every backed-up row is now 'gave_up' with its new error
--      (else it stops with an error and changes nothing).
--   6. Commits and shows ONE grid: the TOTAL, then one row per parked call.
--
-- IF IT STOPS WITH AN ERROR
--   Nothing was changed. pgAdmin may keep the failed transaction open (it then
--   still holds row locks). Run
--       ROLLBACK;
--   in the same Query Tool tab, or close the tab.
--   (This file also starts with ROLLBACK, so simply running it again is safe.)
--
-- RUN IT ONCE
--   A second run stops at CREATE TABLE ("relation ... already exists") and
--   changes nothing. Do not drop the backup table to make it run again
--   unless the PM says so.
--
-- pgAdmin: run the whole file with F5. Auto-commit can stay ON (the file has
-- its own BEGIN/COMMIT). The "WARNING: there is no transaction in progress"
-- from the first line and "NOTICE: table ... does not exist, skipping" are
-- expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.park_stuck_params;
CREATE TEMP TABLE park_stuck_params AS
SELECT -1 AS expected_count;   -- <<< EDIT: the "calls" value of A_count.sql's TOTAL row

-- The calls to park, locked (FOR UPDATE) until COMMIT. Without IF NOT
-- EXISTS: a second run stops here. now() is the time the transaction began,
-- the same value everywhere in this file.
CREATE TABLE scout_call_tracking_bak_park_stuck_20261022 AS
SELECT t.call_id,
       t.status          AS old_status,
       t.error           AS old_error,
       t.attempts        AS old_attempts,
       t.next_check_at   AS old_next_check_at,
       t.last_checked_at AS old_last_checked_at,
       t.updated_at      AS old_updated_at,
       'parked 2026-10-22 (ops/2026-10-22-park-stuck-calls): ' ||
         CASE WHEN length(COALESCE(t.error, '')) > 400 THEN left(t.error, 400) || ' [...]'
              ELSE COALESCE(t.error, '(no error text)') END AS new_error,
       now()             AS parked_at
FROM scout_call_tracking t
WHERE t.call_id = ANY (ARRAY[2344, 2920, 2935, 2940, 2946, 3103, 3127, 3195, 3212, 3214, 3220, 3246, 3815, 5033, 5360, 5400, 6201, 6391, 8142, 10061]::int[])
  AND t.status = 'error'
  AND t.priority = 1
FOR UPDATE OF t;

ALTER TABLE scout_call_tracking_bak_park_stuck_20261022 ADD PRIMARY KEY (call_id);

-- Stop (and roll everything back) unless the count is exactly expected_count,
-- and if any error text would make the next start queue the call again.
DO $$
DECLARE
  expected int;
  found    int;
  canceled int;
BEGIN
  SELECT expected_count INTO expected FROM park_stuck_params;
  SELECT count(*) INTO found FROM scout_call_tracking_bak_park_stuck_20261022;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'B_park: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'B_park: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
  SELECT count(*) INTO canceled FROM scout_call_tracking_bak_park_stuck_20261022
   WHERE old_error LIKE '%context canceled%' OR new_error LIKE '%context canceled%';
  IF canceled > 0 THEN
    RAISE EXCEPTION 'B_park: % call(s) have "context canceled" in their error (the next start would queue them again); nothing was changed, send A''s grid to the PM', canceled;
  END IF;
END
$$;

-- Only status, error and updated_at change.
UPDATE scout_call_tracking t
   SET status = 'gave_up', error = b.new_error, updated_at = b.parked_at
  FROM scout_call_tracking_bak_park_stuck_20261022 b
 WHERE t.call_id = b.call_id
   AND t.status = 'error'
   AND t.priority = 1;

-- Every backed-up row must now be gave_up with its new error.
DO $$
DECLARE
  missing int;
BEGIN
  SELECT count(*) INTO missing
  FROM scout_call_tracking_bak_park_stuck_20261022 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id
  WHERE t.call_id IS NULL OR t.status IS DISTINCT FROM 'gave_up' OR t.error IS DISTINCT FROM b.new_error
     OR t.updated_at IS DISTINCT FROM b.parked_at;
  IF missing > 0 THEN
    RAISE EXCEPTION 'B_park: % backed-up call(s) were not parked; nothing was changed', missing;
  END IF;
END
$$;

COMMIT;

-- The result grid: the TOTAL, then one row per parked call.
SELECT section, item, calls, call_id, old_attempts, old_next_check_at, status_now, parked_at
FROM (
  SELECT 1 AS ord, 'TOTAL' AS section, 'calls parked (= expected_count)' AS item, count(*) AS calls,
         NULL::int AS call_id, NULL::int AS old_attempts, NULL::timestamptz AS old_next_check_at,
         NULL::text AS status_now, min(b.parked_at) AS parked_at
  FROM scout_call_tracking_bak_park_stuck_20261022 b
  UNION ALL
  SELECT 2, 'call', 'parked', 1, b.call_id, b.old_attempts, b.old_next_check_at, t.status, b.parked_at
  FROM scout_call_tracking_bak_park_stuck_20261022 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id) g
ORDER BY ord, call_id;
