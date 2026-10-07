# Re-track the suspicious first calls (pgAdmin version)

> **One-off, October 2026.** These scripts re-track the calls that were tracked
> under the old rug rules. Selection: quote-side liquidity < $500 (stored as
> `current_liquidity_usd < 1000`), or impossible numbers. They are run **by
> hand in pgAdmin**; the program never executes them (nothing under `ops/` is
> compiled or embedded).

## What it does

Resets, so that the tracker tracks them again from scratch under the new rug
rule, every FIRST call of a token whose tracking status is tracking, done or
error AND that either

- has `current_liquidity_usd < 1000` (that column stores 2 x the pool's
  quote side, so this is the new $500 quote-side rug threshold), or
- has impossible numbers: a stored return/peak > 100,000 %, a latest
  return > 100,000 %, or the 2^128 price fingerprint.

Expected size on prod: about 149 calls.

The threshold is the `quote_side_usd` line near the top of A, B and C
(500 = $500 on the quote side). If you change it, change it in all three.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File            | Kind      | Purpose                                              |
|-----------------|-----------|------------------------------------------------------|
| `A_count.sql`   | read-only | what B would reset (per pool_dex + TOTAL)            |
| `B_reset.sql`   | WRITES    | the reset, one transaction, checks the count         |
| `C_check.sql`   | read-only | check after B (needs B's reset_at)                   |
| `D_catchup.sql` | WRITES    | only if C shows race_victims > 0 (needs reset_at)    |

## Steps

1. Deploy the new code and restart `-track` (and the listener).
2. Run `A_count.sql` and paste the whole result grid to the PM.
   Wait for the go-ahead.
3. Stop EVERY tracking process: `-track`, and the listener too unless it runs
   with `-listen-only` (it runs the tracker inside the same process).
   Ctrl+C is safe; wait until each process has exited.
4. In `B_reset.sql` set `expected_count` (line marked EDIT, near the top) to the
   "selected" value of A's TOTAL row, then run the whole file.
   If the count does not match, B stops with an error and changes nothing:
   the error says how many calls it found. Run `ROLLBACK;` in the same tab (or
   close the tab), compare with A, and ask the PM before changing the number.
5. Note `reset_at` from B's result grid. Copy the whole value exactly as
   shown, with the microseconds and the time zone offset (rounding it
   makes C and D miss rows).
6. Start the tracker again (`-track`, and the listener).
7. Paste `reset_at` into `C_check.sql` (line marked EDIT) and run it.
   `not_reset` must be 0. `race_victims` must be 0. Since every tracker was
   stopped during B, it should be 0; only if it is not, paste `reset_at` into
   `D_catchup.sql` and run it once, then run C again.
   Run C again later to watch `waiting` fall and `retracked_so_far` grow.
   `not_reset` is only meaningful on the first run(s): once the tracker has
   worked for a while, its latest-price pass can flag untouched calls under
   the new rule without changing `updated_at`, and they then show up there.
   (You may also run C once between steps 5 and 6, before the tracker
   starts; `race_victims` is then 0 by definition.)

## What you will see

- The website shows NO results for those calls (status pending, no returns,
  no latest price) until each one is tracked again.
- Time: about 150 calls x 1 to 2 minutes / 8 workers (`SCOUT_TRACK_WORKERS`)
  = roughly 20 to 40 minutes, well under an hour. Calls with a long, busy
  30-day window take longer. Live calls (priority 0) go first.
- Node load: each call costs a pool discovery plus a swap scan from the call
  to its last due horizon.

## pgAdmin notes

- Keep "Auto commit" ON (the default). B and D contain their own
  `BEGIN ... COMMIT`; A and C are a single read-only SELECT.
- pgAdmin shows only the LAST result set of a script. Each file is written
  so that the last statement is the result you need. Warnings and errors
  appear in the Messages tab.
- B and D start with `ROLLBACK;` the "WARNING: there is no transaction in
  progress" this gives is expected, and so is "NOTICE: schema "pg_temp"
  does not exist, skipping".
- If B or D stops with an ERROR, nothing was changed, but the failed
  transaction may stay open in that tab and keep its row locks: run
  `ROLLBACK;` in the same tab or close the tab before starting the tracker.
- Do not run B again "to be sure": right after success it finds 0 calls and
  stops; later, after re-tracking, it would reset calls again.
- B leaves two small temporary tables in the session (`retrack`,
  `retrack_params`); they disappear when the tab is closed.
