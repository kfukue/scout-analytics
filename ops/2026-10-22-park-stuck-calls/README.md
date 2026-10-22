# Park 20 stuck backfilled calls as `gave_up` (pgAdmin version)

**Status: pending.** B runs once; R exists to undo it.

> **One-off, October 2026.** Owner decision ("go", 22 Oct 2026): 20 old
> backfilled calls (priority 1, posted 28 Jul-14 Sep 2026) are in status
> `error` and the tracker retries them forever (43 to 717 attempts). Every
> retry starts whole-chain `eth_getLogs` searches again (a quote token's
> USD-pool history from block 1) or times out on one small block range, and
> that clogs the node for every other call. B sets them to `gave_up`, which
> the tracker never retries. A code fix (give up after 10 failed tries; cap
> whole-history reads) follows in separate PRs. These scripts are run **by
> hand in pgAdmin**; the program never executes them (nothing under `ops/` is
> compiled or embedded). Same conventions as `ops/2026-10-20-price-rug-fill/`.

## What B parks

The calls in this list (the owner's read-only query, 22 Oct 2026, 09:30
PDT (16:30 UTC)): 2344, 2920, 2935, 2940, 2946, 3103, 3127, 3195, 3212,
3214, 3220, 3246, 3815, 5033, 5360, 5400, 6201, 6391, 8142, 10061; and of
those only the ones whose tracking row is still

```sql
WHERE t.status = 'error' AND t.priority = 1
```

A call that finished or changed since the query (another status or
priority) is left alone; A shows it in its `check` row and in its own `call`
row.

Only three columns change:

- `status` = `'gave_up'`;
- `error` = `parked 2026-10-22 (ops/2026-10-22-park-stuck-calls): ` followed
  by the old error (its first 400 characters, then ` [...]` if it was
  longer; the whole old error is in the backup table);
- `updated_at` = the time of B. The tracker sets `gave_up` itself only
  through its row save (`SaveTracking` in `scout_models.data.go`), which
  always sets `updated_at = now()`; the other Go code paths that change a
  tracking status (`MarkRepeatTracking`, `-retry-no-pool`) do the same. B
  matches that. It also gives R and C a mark: they treat a row as not
  written since B only while its status is still `gave_up`, its error is
  still B's text and its `updated_at` is still B's time. The two start-up
  re-queues in `scoutanalytics.sql` (see "What brings a parked call back")
  do not set `updated_at`, but they change the status (to `pending`) and
  clear the error, so R and C still recognise such a row as changed (R
  skips it; C counts it under `anything else`).

Not touched: `attempts`, `next_check_at`, `last_checked_at`, the on-chain
state, prices, returns, candles and every other table.

## Before B: stop the tracker

The tracker keeps its own copy of each call it is working on (some of these
calls run for hours) and saves it when that call ends, after every piece of
a long block scan (a checkpoint, with the status the call had when the run
began), and when the tracker is stopped (it then puts the old status back).
Each of these writes `error` over B's `gave_up`. So B runs **only while no tracker is running**:

- `-track` is stopped (Ctrl+C in its terminal; wait until the process has
  exited: "interrupted — progress kept" lines are expected);
- the listener runs with `-listen-only` (its start line says "listen-only:
  the performance tracker is not running in this process"); a listener
  started without `-listen-only` runs the tracker too and must be stopped as
  well;
- no `-track-once` or `-retry-no-pool` is running.

If B ran while a tracker was running, C shows those calls under `anything
else` (status `error` again) or `gave_up, but written since B`; tell the PM.

## What brings a parked call back (and what does not)

Nothing in the program re-queues these rows by itself:

- The tracker's horizon scan (`DueTracking`) reads only `pending`,
  `tracking`, `no_pool` and `error` rows; the latest-price pass only
  `tracking` and `done` rows with an on-chain pool and entry price.
- The start-up SQL in `scoutanalytics.sql` (run at every start of every
  mode) has two re-queues that include `gave_up`:
  - rows whose error contains `context canceled`. B's prefix does not
    contain it, and B refuses (stops, changes nothing) if an old error does;
    A's `check` row shows such calls (expected 0).
  - rows with an on-chain state whose version (`v`) is under 2. Such a row
    would be `pending` again (attempts 0, error cleared) at the next start,
    and the tracker would then start it from scratch: parking it would not
    hold. A's `check` row shows them (expected 0; a call without any
    on-chain state yet is not affected). C's `check` row shows the same
    after B.
- `MarkRepeatTracking` (every tracker cycle) treats `gave_up` like `error`:
  it makes a row `repeat` only if its call is no longer its token's first
  call (an older call of the token is imported, or the post turns out to be
  an update post). These 20 are first calls today (else they would already
  be `repeat`). If it happens anyway, the call is not tracked either way; R
  then skips it and C counts it under `anything else`.

What does bring them back:

- **`-retry-no-pool -retry-gave-up`** takes every `gave_up` first call,
  these 20 included, and sets it to `pending`, due now, attempts 0. For a
  `gave_up` row without an entry price it also clears the on-chain state and
  the pool (A's `info` row "without an entry price" counts these). These
  calls would then start their whole-history searches again. **Do not run
  `-retry-no-pool -retry-gave-up` until the code fix is live** (ask the PM).
- **R** (`R_rollback.sql`) is the deliberate way back: it restores all of
  the parked calls still as B left them, exactly as they were before B. For
  one call only, ask the PM.

## What changes on the website and in the analytics

- Calls page: the Status column of these calls shows "gave up" instead of
  "error" (for a rugged or collapsed call, only the badge's tooltip text
  changes). The summary's "Errors" number counts `error` and `gave_up`
  together, so it does not change. In `/api/summary`, 20 move from
  `error` to `gave_up`.
- Analytics page: no change. It uses the returns already stored for
  USD-priced calls, whatever their status, and B keeps them. These calls get
  no new horizons, as before (they never got past their error).
- Latest prices: no change (neither `error` nor `gave_up` rows are
  refreshed).
- ML data (`scout_call_dataset_v`, `ml/`): `gave_up` counts as "no pool" and
  is left out of every bucket. An `error` row without a late entry price was
  already left out; one with a late entry price could be used before and is
  left out now. A's `info` row "with a late entry price" says how many of
  the 20 that is.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File             | Kind      | Purpose                                                                  |
|------------------|-----------|--------------------------------------------------------------------------|
| `A_count.sql`    | read-only | what B would park (TOTAL, STOP checks, info, one row per listed call)   |
| `B_park.sql`     | WRITES    | the parking, one transaction, saves a backup table, checks the count    |
| `C_check.sql`    | read-only | the state of the parked calls, after B and after R                      |
| `R_rollback.sql` | WRITES    | undoes B from the backup table (only rows still as B left them)         |

## The line you edit (search with Ctrl+F for `<<< EDIT`)

Only `B_park.sql` has a line containing `<<< EDIT` (line 70); no other line
of the SQL files (not even the comments) contains it.

| File         | Line | What goes there                                        |
|--------------|------|--------------------------------------------------------|
| `B_park.sql` | 70   | `-1` becomes the `calls` value of A's `TOTAL` row      |

## Steps

1. Stop the tracker (see "Before B: stop the tracker").
2. Run `A_count.sql` and paste the whole result grid to the PM. Expected:
   `TOTAL` 20, and every `check` row 0. **If a `check` row is not 0, or
   `TOTAL` is not 20, stop here** and wait for the PM. The `call` rows show
   each listed call (status, priority, attempts, post time, next check,
   quote token, state version, error excerpt).
3. In `B_park.sql`, Ctrl+F `<<< EDIT` (line 70): set `expected_count` to the
   `calls` value of A's `TOTAL` row, then run the whole file. If the count
   does not match, or an error contains "context canceled", B stops with an
   error and changes nothing (no backup table either). Run `ROLLBACK;` in
   the same tab (or close the tab), run A again, and ask the PM. B's grid
   shows the TOTAL and one row per parked call (`status_now` `gave_up`).
4. Run `C_check.sql` and paste its grid to the PM (expectations below).
5. Then tell the PM. Starting the tracker again is the PM's separate
   instruction.
6. Later runs of C (any time while the backup table exists) should still
   show every parked call "as B left it".
7. Only if the PM says B's result must be undone: run `R_rollback.sql`, then
   `C_check.sql` again, and paste both grids to the PM.
8. When the PM is satisfied (with B, or after R), drop the backup table
   (the last section of this file).

## What C shows

"R" below is the number of rows R restored (its grid's `restored now`).

| Row                                                  | After B                              | After R                                         |
|------------------------------------------------------|--------------------------------------|-------------------------------------------------|
| `backup`                                             | B's TOTAL (20)                       | B's TOTAL                                       |
| `state` as B left it                                 | B's TOTAL                            | 0                                               |
| `state` as before B                                  | 0                                    | R (fewer once the tracker runs again)           |
| `state` gave_up, but written since B                 | 0                                    | 0                                               |
| `state` anything else                                | 0                                    | 0 at first, grows once the tracker runs again   |
| `due` in a status the tracker retries                | 0                                    | R                                               |
| `due` of those, due now                              | 0                                    | R whose old `next_check_at` has passed (normally all) |
| `check` the next start would queue again             | 0                                    | 0                                               |
| `listed` calls B did not park                        | 20 less B's TOTAL (0)                | as after B                                      |
| `info` status error                                  | A's two error rows less B's TOTAL    | up by R                                         |
| `info` status gave_up                                | up by B's TOTAL                      | as before B                                     |

## R: when it is safe

At any time while the backup table exists, with the tracker running or not.
R works only from the backup table, never from B's selection again, and
restores a call only while it is exactly as B left it: status `gave_up`, B's
error text, and `updated_at` still B's time (not written since B). It sets
back `status`, `error`, `attempts`, `next_check_at` and `updated_at` to their
values before B, so the row is as it was. Calls written since B are left
alone and counted in its grid (their new state is the program's).

R is safe to run again: a second run restores nothing. Running R loses no
data that existed before B: B changed only `status`, `error` and
`updated_at`, and their old values are in the backup. After R the tracker
(when it runs) retries these calls again, whole-history searches included.

### R: next_check_at

R puts back the old `next_check_at`, not `now()`. It is the exact undo: the
tracker set it after the call's last failure, at most 6 hours ahead (its
back-off stops growing at 6 hours). If R runs more than 6 hours after the
last failure, the calls are due at once and take their old place in the
tracker's queue (it reads due rows by priority, then `next_check_at`); if R
runs sooner, they wait until that time, as if B had never run.

## pgAdmin notes

- Keep "Auto commit" ON (the default). B and R contain their own
  `BEGIN ... COMMIT`; A and C are a single read-only SELECT.
- pgAdmin shows only the LAST result set of a script. Each file is written
  so that the last statement is the result you need. Warnings and errors
  appear in the Messages tab.
- B and R start with `ROLLBACK;` the "WARNING: there is no transaction in
  progress" this gives is expected, and so is "NOTICE: table ... does not
  exist, skipping".
- If B or R stops with an ERROR, nothing was changed, but the failed
  transaction may stay open in that tab and keep its row locks: run
  `ROLLBACK;` in the same tab or close the tab.
- B leaves a small temporary table in the session (`park_stuck_params`), R
  too (`park_stuck_restored`); they disappear when the tab is closed. The
  backup table is a real table and stays until you drop it.

## Drop the backup table

Once the PM is satisfied (with B, or after R), run:

```sql
DROP TABLE scout_call_tracking_bak_park_stuck_20261022;
```
