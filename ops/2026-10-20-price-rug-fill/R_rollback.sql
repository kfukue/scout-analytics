-- =============================================================================
-- R_rollback.sql  (step 7 of README.md, only if B's result must be undone)
-- WRITES: puts back the rug time B_fill.sql replaced, from the backup table.
--
-- Nothing to edit. Run the whole file (F5).
--
-- WHAT IT DOES (one transaction, all or nothing)
--   Works ONLY from the backup table
--   scout_call_tracking_bak_price_rug_fill_20261020 (never from B's
--   selection again). For each backed-up call that is still exactly as B left
--   it (rug_at_kind 'price', rug_at = B's time, and updated_at unchanged
--   since B, i.e. not tracked again), it sets rug_at and rug_at_kind back to
--   their values before B (both empty). Nothing else of the row changes.
--   Calls the tracker changed since B are left as they are:
--     * a drained kind (event, at_call, detected) replaced B's 'price' time:
--       the pool was found drained, that time is the right one;
--     * the call was tracked again (updated_at moved): its new state is the
--       tracker's.
--   Then it commits and shows ONE grid: backed up, restored now, and the
--   rows not restored, with why.
--
-- SAFE TO RUN AGAIN: a second run restores nothing (those rows no longer
-- have B's 'price' time) and shows them as "already without a rug time".
-- It is safe at any time while the backup table exists, the tracker
-- running or not.
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

DROP TABLE IF EXISTS pg_temp.price_rug_fill_restored;
CREATE TEMP TABLE price_rug_fill_restored (call_id int PRIMARY KEY);

WITH upd AS (
  UPDATE scout_call_tracking t
     SET rug_at = b.old_rug_at, rug_at_kind = b.old_rug_at_kind
    FROM scout_call_tracking_bak_price_rug_fill_20261020 b
   WHERE t.call_id = b.call_id
     AND t.rug_at_kind = 'price'
     AND t.rug_at = b.new_rug_at
     AND t.updated_at = b.old_updated_at
  RETURNING t.call_id)
INSERT INTO price_rug_fill_restored SELECT call_id FROM upd;

COMMIT;

-- The result grid. Each backed-up call is counted once.
WITH s AS (
  SELECT b.call_id,
         CASE WHEN r.call_id IS NOT NULL THEN 'restored'
              WHEN t.call_id IS NULL THEN 'else'
              WHEN t.rug_at IS NOT DISTINCT FROM b.old_rug_at
               AND t.rug_at_kind IS NOT DISTINCT FROM b.old_rug_at_kind THEN 'already'
              WHEN t.rug_at_kind IN ('event', 'at_call', 'detected') THEN 'drained'
              ELSE 'else' END AS state
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
  LEFT JOIN scout_call_tracking t ON t.call_id = b.call_id
  LEFT JOIN price_rug_fill_restored r ON r.call_id = b.call_id)
SELECT item, calls
FROM (
  SELECT 1 AS ord, 'backed up by B' AS item, count(*) AS calls FROM s
  UNION ALL
  SELECT 2, 'restored now', count(*) FROM s WHERE state = 'restored'
  UNION ALL
  SELECT 3, 'skipped: already without a rug time (restored by an earlier run of R, or its state started again)', count(*)
  FROM s WHERE state = 'already'
  UNION ALL
  SELECT 4, 'skipped: a drained kind replaced B''s time (event, at_call, detected)', count(*)
  FROM s WHERE state = 'drained'
  UNION ALL
  SELECT 5, 'skipped: anything else (tracked again since B, another time or kind, or the row is gone)', count(*)
  FROM s WHERE state = 'else') g
ORDER BY ord;
