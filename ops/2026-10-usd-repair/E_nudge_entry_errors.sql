-- =============================================================================
-- E_nudge_entry_errors.sql  (step 8 of README.md)  WRITES: one column only.
--
-- THE ONLY LINE TO EDIT is expected_count, on LINE 36: it is the
-- only line of this file with the EDIT marker. Find it with Ctrl+F: type
-- three less-than signs, a space and EDIT. Put there the "selected" value of
-- A_count.sql's 'info' row E_entry_usd_errors, or the number in the error
-- message of a first run with -1.
--
-- Calls with status 'error' whose error is a USD price at the entry or at a
-- horizon close ('USD price of ...' with 'no USD source' or 'no trades in
-- the') lost nothing: they only waited for the USD-source fix. This sets
-- their next_check_at to now() so the tracker retries them in its next cycle
-- instead of after their back-off. Nothing else changes (status, attempts,
-- error and updated_at stay as they are; the tracker rewrites them).
-- 'gave_up' rows with the same error are NOT touched (the tracker does not
-- retry gave_up rows); the result grid counts them for the PM.
--
-- The tracker may keep running while you run this. If the count has changed
-- in between (the tracker retried some of them), E stops with an error that
-- says the new count and changes nothing; set that number and run it again.
-- Running it again after a success is harmless: it only sets next_check_at
-- to now() again for the calls still in that error.
--
-- IF IT STOPS WITH AN ERROR: nothing was changed; run ROLLBACK; in the same
-- tab (or close it). The file starts with ROLLBACK, so running it again is
-- safe; the "WARNING: there is no transaction in progress" and "NOTICE:
-- schema "pg_temp" does not exist, skipping" are expected and harmless.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;

DROP TABLE IF EXISTS pg_temp.usdnudge_params, pg_temp.usdnudge;
CREATE TEMP TABLE usdnudge_params AS
SELECT -1 AS expected_count;   -- <<< EDIT: A's info row E_entry_usd_errors (or the number in E's error)

CREATE TEMP TABLE usdnudge AS
SELECT t.call_id
FROM scout_call_tracking t
WHERE t.status = 'error'
  AND t.error LIKE 'USD price of %'
  AND (t.error LIKE '%no USD source%' OR t.error LIKE '%no trades in the%')
FOR UPDATE OF t;

DO $$
DECLARE
  expected int;
  found    int;
BEGIN
  SELECT expected_count INTO expected FROM usdnudge_params;
  SELECT count(*) INTO found FROM usdnudge;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'E_nudge: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'E_nudge: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
END
$$;

UPDATE scout_call_tracking t SET next_check_at = now()
FROM usdnudge n
WHERE t.call_id = n.call_id;

COMMIT;

-- The result grid.
SELECT (SELECT count(*) FROM usdnudge) AS calls_nudged,
       (SELECT count(*) FROM scout_call_tracking
         WHERE status = 'error' AND error LIKE 'USD price of %'
           AND (error LIKE '%no USD source%' OR error LIKE '%no trades in the%')
           AND next_check_at <= now()) AS now_due,
       (SELECT count(*) FROM scout_call_tracking
         WHERE status = 'gave_up' AND error LIKE 'USD price of %'
           AND (error LIKE '%no USD source%' OR error LIKE '%no trades in the%')) AS gave_up_not_nudged;
