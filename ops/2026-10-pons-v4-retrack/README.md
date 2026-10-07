# Re-track Pons V2 calls tracked on their v4 pool (pgAdmin version)

> **One-off, October 2026.** These scripts re-track the Pons V2 calls that the
> tracker, before Pons V2 support, priced on their Uniswap v4 pool. They are
> run **by hand in pgAdmin**; the program never executes them (nothing under
> `ops/` is compiled or embedded). Same conventions as
> `ops/2026-10-rug-retrack/`.

## Why

Before Pons V2 support, a Pons V2 token called while it was still on its
bonding curve was tracked on the v4 pool it got at its graduation, so its entry
price came from the first v4 trade after the graduation, not from the curve at
the call. Tokens that had already graduated at the call were tracked right.
The database does not store the graduation for those old rows, so all of them
are tracked again; the Pons-aware code finds out which case each one is (curve
-> `pool_dex = 'pons-curve'`, already graduated -> `pool_dex = 'uniswap-v4'`).

## What it does

Resets, so that the tracker tracks them again from scratch, every FIRST call
of a token (same first-call rule as the rug scripts: address without regard to
case, earliest post, update posts never count) that

- has tracking status `tracking`, `done` or `error`,
- has `pool_dex = 'uniswap-v4'`, and
- whose post says launchpad **or** dex Pons V2 (compared lower-case with
  everything but letters and digits removed, so "Pons V2", "pons_v2" and
  "PONS-V2" all match; "Pons" alone does not).

Deleted: their rows in `scout_call_returns`, `scout_call_candles`,
`scout_call_precall`. The tracking row goes back to `pending` without pool,
prices, on-chain state, rug flag or latest price. Not touched:
`scout_call_metrics`, `scout_call_predictions`, the calls themselves.

Expected size on prod: unknown; A tells.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File            | Kind      | Purpose                                              |
|-----------------|-----------|------------------------------------------------------|
| `A_count.sql`   | read-only | what B would reset (per status + TOTAL)              |
| `B_reset.sql`   | WRITES    | the reset, one transaction, checks the count         |
| `C_check.sql`   | read-only | check after B (needs B's reset_at)                   |
| `D_catchup.sql` | WRITES    | only if C shows race_victims > 0 (needs reset_at)    |

## Steps

1. Deploy the Pons-aware code (Pons V2 support, graduation from indexed logs)
   and restart `-track` (and the listener).
2. Run `A_count.sql` and paste the whole result grid to the PM.
   Wait for the go-ahead. `new_code_state` should be 0 (see below).
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
   Run C again later to watch `waiting` fall and `retracked_so_far`,
   `now_on_curve` and `now_on_v4` grow. `v4_without_pons_state` should stay 0.
   (You may also run C once between steps 5 and 6, before the tracker
   starts; `race_victims` is then 0 by definition.)

## Do not run B a second time

Right after a successful B, a second run finds 0 calls and stops with an
error, changing nothing. **After the tracker has started, never run B again,
and never change `expected_count` to make it run:** every token that had
already graduated at its call is correctly back on `uniswap-v4` and matches
the selection again, so a rerun would throw away correct results and track
them all again. Only D is safe to rerun.

## If new_code_state is not 0

`new_code_state` counts selected rows whose on-chain state already has a
`pons_curve` key, i.e. rows the Pons-aware code wrote itself (tokens that had
graduated before the call). They are right already; B would track them again
anyway (harmless, only time and node load). Tell the PM the number; the
selection could exclude them if wanted.

## What you will see

- The website shows NO results for those calls (status pending, no returns,
  no latest price) until each one is tracked again.
- Time: roughly the number of calls x 1 to 2 minutes / 8 workers
  (`SCOUT_TRACK_WORKERS`). Calls with a long, busy 30-day window take longer.
  Live calls (priority 0) go first.
- Node load: each call costs a pool discovery (bonding curve and graduation
  lookup) plus a swap scan from the call to its last due horizon.

## pgAdmin notes

- Keep "Auto commit" ON (the default). B and D contain their own
  `BEGIN ... COMMIT`; A and C are a single read-only SELECT.
- pgAdmin shows only the LAST result set of a script. Each file is written
  so that the last statement is the result you need. Warnings and errors
  appear in the Messages tab.
- B and D start with `ROLLBACK;` the "WARNING: there is no transaction in
  progress" this gives is expected, and so is "NOTICE: schema "pg_temp" does not
  exist, skipping".
- If B or D stops with an ERROR, nothing was changed, but the failed
  transaction may stay open in that tab and keep its row locks: run
  `ROLLBACK;` in the same tab or close the tab before starting the tracker.
- B leaves two small temporary tables in the session (`ponsretrack`,
  `ponsretrack_params`); they disappear when the tab is closed.
