# Re-track the old Uniswap v4 calls (pgAdmin version)

**Status: DONE, 7 Oct 2026** (649 calls, cutoff 2026-10-06 12:53:00-07, `reset_at` 2026-10-07 12:18:38.004137-07; 52 found rugged). Never run B again.

> **One-off, October 2026.** These scripts re-track the Uniswap v4 calls that
> older tracker code tracked, so that every v4 call follows the current rules
> before the first model training. They are run **by hand in pgAdmin**; the
> program never executes them (nothing under `ops/` is compiled or embedded).
> Same conventions as `ops/2026-10-usd-repair/`.

## Why

About 1,166 v4 calls were tracked by code older than today's:

- before the rug guard (8846dc6), the tracker never measured v4 liquidity, so
  a v4 pool that was drained can still show its old, pre-rug numbers;
- before the USD-source fix (40e1888), USD prices came from the old lookup;
- before 14052f8, the call-time block was not chosen deterministically.

The current code handles all three, so these calls are tracked again from
scratch.

## How B tells old rows from current ones

The on-chain state (`scout_call_tracking.onchain`) has no field that only the
USD fix or the `blockAt` fix writes. Two signals together mark a row tracked
by the current code:

1. **`entry_liq_q`** in the state (a JSON number). Only the entry step writes
   it, it runs only when the call has no entry yet, and for a v4 entry it is
   always set (and above 0) since the rug guard. A row whose entry was found
   by older code never gets it later. No `entry_liq_q` = entry found before
   the rug guard, or no entry found yet.
2. **`scout_call_precall.computed_at` at or after `current_code_since`**. The
   pre-call stats are computed once per fresh state, right after the entry, so
   their time is when this row was (re)tracked from scratch.

When the cutoff `current_code_since` (below) is set, a row with both is left
alone. Every other first-call v4 row with status `tracking`, `done` or
`error` is reset. Also reset, whatever the markers:
rows whose post says Pons V2 but whose state has no `pons_curve` key (known:
call 497 `0x2a13...` and call 10275 `0xd79a...`; harmless, re-tracking makes them
consistent).

`current_code_since` is set on the line marked `OPTION` (A, B and C, the same
value in all three). **Its default is `NULL`: every first-call v4 row with
status tracking/done/error is reset** (the safe choice: nothing tracked by
older code can slip through). To skip the rows the current code has already
tracked, replace `NULL` with the time `-track` was restarted with the current
code, in quotes with its offset, e.g. `'2026-10-07 18:05:00-07'` (the first
line of the tracker's log after the restart). Never put an earlier time
there: rows tracked by the old code after it would be skipped. A later time
is always safe (it only re-tracks more). The value in use is shown in A's
grid (info row `current_code_since in use`), in B's result and in C's.

A's info row `the most a restart-time cutoff could skip` shows, before you
decide, how many v4 rows carry `entry_liq_q` and pre-call stats from after
the 14052f8 commit time: the most the cutoff can save. If it is small, keep
`NULL`.

### Residual risk

(Only when `current_code_since` is set; with `NULL` every v4 row is reset.)

- **Wrong cutoff.** A row whose entry an older `-track` found after
  `current_code_since` is taken as current and skipped. That is why the
  OPTION must be the real restart time with the current code, never earlier.
- **An entry found by older code, pre-call stats by the current code.** If an
  old run found the entry and then failed before the pre-call stats (e.g.
  `USD price of ...` error), and the current code computed the stats later,
  the row is skipped although its entry block came from the old code.
- Rows whose state has `pons_curve` were written by the Pons-aware code
  (since d39f399); they are still reset when their pre-call stats are older
  than `current_code_since` (they predate the USD and `blockAt` fixes).
- `scout_call_precall.computed_at` has two writers: the tracker (above) and
  the listener's live scoring (`score.go`, `livePrecall`), which stores the
  pre-call stats of a new live call right away. The tracker's first pass
  over a fresh state always overwrites them, so the listener's time can only
  matter for a call whose entry the old tracker found inside the window
  between `current_code_since` and the listener's write; with a correct
  cutoff that window is empty in practice.

## What B resets

The WHERE clause (identical in B and C; A computes the same with joins):

```sql
WHERE t.status IN ('tracking','done','error')
  AND t.pool_dex = 'uniswap-v4'
  AND (   NOT COALESCE(jsonb_typeof(t.onchain) = 'object'
                       AND jsonb_typeof(t.onchain -> 'entry_liq_q') = 'number'
                       AND EXISTS (SELECT 1 FROM scout_call_precall q
                                   WHERE q.call_id = t.call_id AND q.computed_at >= p.current_code_since), false)
       OR (    NOT COALESCE(jsonb_typeof(t.onchain) = 'object' AND t.onchain ? 'pons_curve', false)
           AND EXISTS (SELECT 1 FROM scout_call_metrics m
                       WHERE m.call_id = t.call_id
                         AND (   regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = 'ponsv2'
                              OR regexp_replace(lower(COALESCE(m.dex, '')),       '[^a-z0-9]+', '', 'g') = 'ponsv2'))))
```

applied to the FIRST call of each token only (same first-call rule as the
other re-tracks: address without regard to case, earliest post, lowest id on
ties, update posts never count). Rows in `pending`, `no_pool`, `gave_up` or
`repeat` are never touched.

Deleted: their rows in `scout_call_returns`, `scout_call_candles`,
`scout_call_precall`. The tracking row goes back to `pending` (due now,
attempts 0) without pool, prices, `price_unit`, on-chain state, rug flag or
latest price. Kept: token name and on-chain symbol, priority, entry time,
contract address, created_at. Not touched: `scout_call_metrics`,
`scout_call_predictions`, the calls themselves.

Expected size on prod: with `NULL`, the 1,166 old v4 calls PLUS every v4
row tracked since the rug guard by code older than 14052f8 (live calls, and
the rug, Pons-v4 and USD-repair re-tracks that landed on v4). So 1,166 is a
floor; A's TOTAL tells.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File            | Kind      | Purpose                                                          |
|-----------------|-----------|------------------------------------------------------------------|
| `A_count.sql`   | read-only | what B would reset (by reason, status, TOTAL, excluded, info)    |
| `B_reset.sql`   | WRITES    | the reset, one transaction, checks the count                     |
| `C_check.sql`   | read-only | check after B (needs B's reset_at)                               |
| `D_catchup.sql` | WRITES    | only if C shows race_victims > 0 (needs reset_at)                |

## The lines you edit (search with Ctrl+F for `<<< EDIT`)

Each file you edit has exactly ONE line containing `<<< EDIT`; no other line
(not even the comments) contains it. Press Ctrl+F in the Query Tool, type
`<<< EDIT`, and change only that line. A has no such line.

| File            | Line | What goes there                                         |
|-----------------|------|---------------------------------------------------------|
| `B_reset.sql`   | 62   | `-1` becomes the `selected` value of A's `TOTAL` row    |
| `C_check.sql`   | 45   | the text between the quotes becomes B's `reset_at`      |
| `D_catchup.sql` | 35   | the text between the quotes becomes B's `reset_at`      |

The `OPTION` line (`current_code_since`) is line 47 in A, line 63 in B (right
below the EDIT line) and line 46 in C (right below the EDIT line). It stays
`NULL` unless the PM says otherwise (see "How B tells old rows from current
ones"); then change it the same way in all three files.

## Steps

0. Order: deploy the current code FIRST (step 1), let the other re-tracks
   (rug, Pons-v4, USD repair) finish under it (`waiting = 0` in their C),
   and only then run A and B. Their v4 rows are then tracked by the current
   code, and with the OPTION set to the restart time B skips them. If you run
   this earlier, it still works, but their C's `waiting` (and
   `retracked_so_far`) then also count the calls reset here, and this C
   counts theirs.
1. Deploy the current code (commit 14052f8 or later: deterministic call-time
   block, USD own-pool fix) and restart `-track` (and the listener). Note the
   restart time. Only if the PM says to use the cutoff: put it on the
   `OPTION` line of A, B and C instead of `NULL` (same value in all three,
   in quotes with the offset).
2. Run `A_count.sql` and paste the whole result grid to the PM (check that
   the info row `current_code_since in use` shows what you meant). Wait for
   the go-ahead.
3. Stop EVERY tracking process: `-track`, and the listener too unless it runs
   with `-listen-only` (it runs the tracker inside the same process).
   Ctrl+C is safe; wait until each process has exited.
4. In `B_reset.sql`, Ctrl+F `<<< EDIT` (line 62): set `expected_count` to the
   `selected` value of A's `TOTAL` row, then run the whole file.
   If the count does not match, B stops with an error and changes nothing:
   the error says how many calls it found. Run `ROLLBACK;` in the same tab (or
   close the tab), compare with A, and ask the PM before changing the number.
5. Note `reset_at` from B's result grid. Copy the whole value exactly as
   shown, with the microseconds and the time zone offset (rounding it
   makes C and D miss rows).
6. Start the tracker again (`-track`, and the listener).
7. In `C_check.sql`, Ctrl+F `<<< EDIT` (line 45): paste `reset_at` between
   the quotes, and run it. `not_reset` must be 0. `race_victims` must be 0.
   Since every tracker was stopped during B, it should be 0; only if it is
   not, paste `reset_at` into `D_catchup.sql` (Ctrl+F `<<< EDIT`, line 35)
   and run it once, then run C again.
   Run C again later to watch `waiting` fall and `retracked_so_far` and
   `now_on_v4` grow. Paste `now_rugged`, `v4_without_entry_liq_after` and
   `pons_label_v4_without_curve_after` to the PM when `waiting` is 0.
   (You may also run C once between steps 5 and 6, before the tracker
   starts; `race_victims` is then 0 by definition.)

## Do not run B a second time

Right after a successful B, a second run finds 0 calls and stops with an
error, changing nothing. **After the tracker has started, never run B again,
and never change `expected_count` to make it run.** Two kinds of re-tracked
rows match the selection again and would be reset for nothing:

- a v4 call whose entry still cannot be found (no `entry_liq_q` yet, status
  `error`);
- a call labelled Pons V2 that is not a Pons token: it comes back on v4
  without `pons_curve`.

C counts both (`v4_without_entry_liq_after`,
`pons_label_v4_without_curve_after`). Only D is safe to rerun.

## What you will see

- The website shows NO results for the calls B reset (status pending, no
  returns, no latest price) until each one is tracked again.
- Some calls come back as rugged (`now_rugged` in C): their v4 pool was
  drained, which the old code could not see.
- Time: roughly A's TOTAL x 1 to 2 minutes / the tracker workers
  (`SCOUT_TRACK_WORKERS`). For 1,100 calls with 12 workers that is 1.5 to
  3 hours; most of these calls are older than 30 days, so each needs its full
  30-day scan: expect the upper end (2 to 4 hours), more if TOTAL is well
  above 1,166. Live calls (priority 0) go first.
- Node load: each call costs a pool discovery plus a swap scan from the call
  to its last due horizon, plus the USD lookups.

## pgAdmin notes

- Keep "Auto commit" ON (the default). B and D contain their own
  `BEGIN ... COMMIT`; A and C are a single read-only SELECT.
- pgAdmin shows only the LAST result set of a script. Each file is written
  so that the last statement is the result you need. Warnings and errors
  appear in the Messages tab.
- B and D start with `ROLLBACK;` the "WARNING: there is no transaction in
  progress" this gives is expected, and so is "NOTICE: schema "pg_temp" does
  not exist, skipping".
- If B or D stops with an ERROR, nothing was changed, but the failed
  transaction may stay open in that tab and keep its row locks: run
  `ROLLBACK;` in the same tab or close the tab before starting the tracker.
- B leaves two small temporary tables in the session (`v4retrack`,
  `v4retrack_params`), D too (`v4retrack_catchup`,
  `v4retrack_catchup_params`); they disappear when the tab is closed.
