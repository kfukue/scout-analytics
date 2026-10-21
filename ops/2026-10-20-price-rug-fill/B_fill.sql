-- =============================================================================
-- B_fill.sql  (step 4 of README.md)  WRITES: gives past price-rule rugged
-- calls rug_at_kind 'price' and an approximate rug_at.
--
-- THE ONLY LINE TO EDIT is expected_count: it is the only line of this file
-- with the EDIT marker. Find it with Ctrl+F: type three less-than signs, a
-- space and EDIT. Put there the "calls" value of the TOTAL row of
-- A_count.sql. Do not edit any other line.
--
-- BEFORE YOU RUN IT
--   * #49 (the tracker that times new price-rule rugs) is live on the server:
--     see README.md step 1.
--   * A_count.sql's "other, total" row shows 29 (README.md step 2). If not: STOP.
--   * expected_count is set as above.
--   The tracker may keep running (README.md "Why B may run beside the
--   tracker").
--
-- WHAT IT DOES (one transaction, all or nothing)
--   1. Picks the calls (the same selection as A_count.sql's "selected" rows:
--      status done, rugged, no rug time, on-chain, state version 2 or later,
--      no rug block, entry price above 0, last price strictly under 5 % of
--      the entry, and a done horizon), with their time from the tracker's
--      own query (priceRugAtSQL, copied as it is), locks their tracking rows
--      and saves them in the backup table
--      scout_call_tracking_bak_price_rug_fill_20261020 (call_id,
--      old_rug_at, old_rug_at_kind, old_updated_at, new_rug_at, src,
--      filled_at). The backup is a real table: R_rollback.sql needs it.
--   2. If their number is not exactly expected_count, it STOPS with an error
--      and changes nothing (no backup table either). Read the number in the
--      error message, compare it with A, and only then set expected_count
--      again.
--   3. Sets rug_at = new_rug_at and rug_at_kind = 'price' on exactly the
--      backed-up rows that still have no rug time. Nothing else of the row
--      changes (updated_at neither).
--   4. Checks that every backed-up row now has its new time and 'price'
--      (else it stops with an error and changes nothing).
--   5. Commits and shows ONE grid: the calls filled, by src.
--
-- IF IT STOPS WITH AN ERROR
--   Nothing was changed. pgAdmin may keep the failed transaction open (it then
--   still holds row locks and the tracker would wait on them). Run
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

DROP TABLE IF EXISTS pg_temp.price_rug_fill_params;
CREATE TEMP TABLE price_rug_fill_params AS
SELECT -1 AS expected_count;   -- <<< EDIT: the "calls" value of A_count.sql's TOTAL row

-- The calls to fill, with their time, locked (FOR UPDATE) so that a drained
-- rug time the tracker finds for one of them meanwhile waits for this
-- transaction instead of interleaving with it. Without IF NOT EXISTS: a
-- second run stops here.
CREATE TABLE scout_call_tracking_bak_price_rug_fill_20261020 AS
SELECT t.call_id,
       t.rug_at      AS old_rug_at,
       t.rug_at_kind AS old_rug_at_kind,
       t.updated_at  AS old_updated_at,
       p.at          AS new_rug_at,
       p.src         AS src,
       now()         AS filled_at
FROM scout_call_tracking t
-- BEGIN copy of priceRugAtSQL (scout_models.data.go): $1 = t.call_id, $2 = t.entry_price_usd
CROSS JOIN LATERAL (
WITH last AS (
	SELECT max(due_at) AS due FROM scout_call_returns WHERE call_id = t.call_id AND status = 'done'
), candle AS (
	SELECT LEAST(c.bucket_start + c.interval_seconds * interval '1 second', last.due) AS at
	  FROM scout_call_candles c, last
	 WHERE c.call_id = t.call_id AND c.interval_seconds = 3600 AND c.bucket_start < last.due
	   AND c.close < t.entry_price_usd::numeric * 0.05
	 ORDER BY c.bucket_start LIMIT 1
), horizon AS (
	SELECT min(r.due_at) AS at FROM scout_call_returns r
	 WHERE r.call_id = t.call_id AND r.status = 'done' AND r.price_usd < t.entry_price_usd::numeric * 0.05
)
SELECT COALESCE((SELECT at FROM candle), horizon.at, last.due),
       CASE WHEN EXISTS (SELECT 1 FROM candle) THEN 'candle'
            WHEN horizon.at IS NOT NULL THEN 'horizon'
            ELSE 'last_due' END,
       last.due IS NULL
  FROM last, horizon
) AS p(at, src, no_done)
-- END copy of priceRugAtSQL
WHERE t.status = 'done'
  AND t.rugged IS TRUE
  AND t.rug_at IS NULL AND t.rug_at_kind IS NULL
  AND t.entry_price_source LIKE 'onchain-%'
  AND t.onchain IS NOT NULL
  AND COALESCE((t.onchain->>'v')::int, 0) >= 2
  AND COALESCE((t.onchain->>'rug_block')::numeric, 0) = 0
  AND t.entry_price_usd > 0
  AND t.current_price_usd < t.entry_price_usd * 0.05
  AND NOT p.no_done AND p.at IS NOT NULL
FOR UPDATE OF t;

ALTER TABLE scout_call_tracking_bak_price_rug_fill_20261020 ADD PRIMARY KEY (call_id);

-- Stop (and roll everything back) unless the count is exactly expected_count.
DO $$
DECLARE
  expected int;
  found    int;
BEGIN
  SELECT expected_count INTO expected FROM price_rug_fill_params;
  SELECT count(*) INTO found FROM scout_call_tracking_bak_price_rug_fill_20261020;
  IF expected IS NULL OR expected < 0 THEN
    RAISE EXCEPTION 'B_fill: set expected_count at the top of the file first (the selection has % call(s)); nothing was changed', found;
  END IF;
  IF found <> expected THEN
    RAISE EXCEPTION 'B_fill: the selection has % call(s) but expected_count is %; nothing was changed', found, expected;
  END IF;
END
$$;

-- Only the two rug-time columns change (not updated_at).
UPDATE scout_call_tracking t
   SET rug_at = b.new_rug_at, rug_at_kind = 'price'
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
 WHERE t.call_id = b.call_id
   AND t.rug_at IS NULL AND t.rug_at_kind IS NULL;

-- Every backed-up row must now have its new time and 'price'.
DO $$
DECLARE
  missing int;
BEGIN
  SELECT count(*) INTO missing
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
  JOIN scout_call_tracking t ON t.call_id = b.call_id
  WHERE t.rug_at IS DISTINCT FROM b.new_rug_at OR t.rug_at_kind IS DISTINCT FROM 'price';
  IF missing > 0 THEN
    RAISE EXCEPTION 'B_fill: % backed-up call(s) did not get their price rug time; nothing was changed', missing;
  END IF;
END
$$;

COMMIT;

-- The result grid: the calls filled, by where their time comes from.
SELECT section, item, calls, min_after_entry, max_after_entry, filled_at
FROM (
  SELECT 1 AS ord, 'filled' AS section, b.src AS item, count(*) AS calls,
         min(b.new_rug_at - t.entry_at) AS min_after_entry, max(b.new_rug_at - t.entry_at) AS max_after_entry,
         min(b.filled_at) AS filled_at
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
  JOIN scout_call_tracking t ON t.call_id = b.call_id
  GROUP BY b.src
  UNION ALL
  SELECT 2, 'TOTAL', 'calls filled (= expected_count)', count(*),
         min(b.new_rug_at - t.entry_at), max(b.new_rug_at - t.entry_at), min(b.filled_at)
  FROM scout_call_tracking_bak_price_rug_fill_20261020 b
  JOIN scout_call_tracking t ON t.call_id = b.call_id) g
ORDER BY ord, calls DESC, item;
