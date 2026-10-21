# Rug times for past price-rule rugs (pgAdmin version)

**Status: pending. Run only after #49 is live** (step 1). B runs once; R
exists to undo it.

> **One-off, October 2026.** Owner decision A2 (20 Oct 2026): a call the
> tracker marked rugged only by the price rule (its last price strictly under
> 5 % of the entry price; the pool was never seen drained) gets
> `rug_at_kind = 'price'` and an approximate `rug_at`. Since #49 the tracker
> does this for every call it finishes. These scripts do the same for the
> calls it finished before. They are run **by hand in pgAdmin**; the program
> never executes them (nothing under `ops/` is compiled or embedded). Same
> conventions as `ops/2026-10-07-liquidity-retrack/`.

## What B fills

The calls the tracker would time today, with the stored entry price
(`entry_price_usd`, the price the rule compares with):

```sql
FROM scout_call_tracking t
WHERE t.status = 'done'
  AND t.rugged IS TRUE
  AND t.rug_at IS NULL AND t.rug_at_kind IS NULL
  AND t.entry_price_source LIKE 'onchain-%'
  AND t.onchain IS NOT NULL
  AND COALESCE((t.onchain->>'v')::int, 0) >= 2
  AND COALESCE((t.onchain->>'rug_block')::numeric, 0) = 0
  AND t.entry_price_usd > 0
  AND t.current_price_usd < t.entry_price_usd * 0.05
  -- and the call has a done horizon (else there is no time to give)
```

The time is the tracker's own query (`priceRugAtSQL` in
`scout_models.data.go`), copied into A, B and C as it is (only `$1` became
`t.call_id` and `$2` became `t.entry_price_usd`; a test checks the copy):

1. `candle`: the end of the first hourly candle whose close is under 5 % of
   the entry, capped at the last done horizon's due time;
2. `horizon`: else the due time of the earliest done horizon whose price is
   under 5 % of the entry;
3. `last_due`: else the last done horizon's due time.

A shows how many calls take each source. Calls whose horizons all ended
within the first day have only 5-minute candles there, so `horizon` and
`last_due` are expected to be common, not only `candle`.

Only `rug_at` and `rug_at_kind` change. Not touched: `updated_at`, status,
the on-chain state, prices, every other table.

Not filled (A counts them, section `left out`). A puts every rugged call
without a rug time (any status) in exactly one of its rows: `selected`
(the `TOTAL` row), `other` (the `total` row) or one of the `left out` rows
below; the `base` row is their sum.

- GeckoTerminal-priced calls (not on-chain) or calls without an on-chain
  state: no stored candles; the tracker gives them no time either (outside
  A2).
- Calls with a rug block in the state but no rug time: `-backfill-timing`
  gives them their drained time.
- Entry price NULL or 0, last price (`current_price_usd`) NULL, on-chain
  state version under 2, status other than `done`, no done horizon, or only
  one of `rug_at` / `rug_at_kind` set: expected 0 each.

### The "other" group: a STOP check

A's `other` rows count the rugged calls priced on-chain
(`entry_price_source` `onchain-...`), of **any status**, with no rug block
and no rug time, an entry price above 0, and a last price that is known
and **at or above** 5 % of the entry (`current_price_usd >=
entry_price_usd * 0.05`). A NULL last price is not counted here: it has
its own `left out` row (expected 0). They were marked rugged by an older
rule; B leaves them alone. The PM's investigation (query R1, no status
filter) found **29**; two of them (calls 9947 and 9948) are now `repeat`
rows. So A shows three rows that add up:

| A row                    | Expected |
|--------------------------|----------|
| `other`, status done     | 27       |
| `other`, status not done | 2 (the two `repeat` rows) |
| `other`, total           | 29       |

> **STOP: if A's `other, total` is not 29, stop. Do not run B; send A's
> whole grid to the PM.**

## Why B may run beside the tracker

No need to stop `-track` or the listener:

- The tracker's row save (`SaveTracking`) never writes `rug_at` or
  `rug_at_kind`, so it neither undoes B's write nor is undone by it.
- The only code that writes these two columns:
  - `SetRugAt` and `SetRugAtIfMissing` store a drained time (kind `event`,
    `at_call` or `detected`), which replaces a `'price'` time; that is the
    intended order (a drained pool always wins over the price rule);
  - `storePriceRugAt` (#49) writes `'price'` only on a row with no rug time
    or a `'price'` one (through `SetRugAtIfMissing`); on one of B's rows it
    would write the same time from the same query;
  - `ClearRugAt` empties both columns only when a call is re-tracked from
    scratch (its on-chain state starts again); the tracker then times it
    anew.

  None of them writes `'price'` over a drained time.
- B locks the rows it picks (`FOR UPDATE`) until it commits. A drained time
  the tracker finds for one of them meanwhile waits a moment, then replaces
  `'price'`. B's UPDATE also re-checks "no rug time yet", so it never
  overwrites a time stored after A ran; the count check then stops B and
  changes nothing (run A again).
- A call the tracker finishes now gets its own `'price'` time (#49), so it
  is not in B's selection.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File             | Kind      | Purpose                                                              |
|------------------|-----------|----------------------------------------------------------------------|
| `A_count.sql`    | read-only | what B would fill (by source, TOTAL, the "other" STOP check, left out, kinds) |
| `B_fill.sql`     | WRITES    | the fill, one transaction, saves a backup table, checks the count    |
| `C_check.sql`    | read-only | the state of the filled calls, after B and after R                   |
| `R_rollback.sql` | WRITES    | undoes B from the backup table (only rows still as B left them)      |

## The line you edit (search with Ctrl+F for `<<< EDIT`)

Only `B_fill.sql` has a line containing `<<< EDIT` (line 62); no other line
of the SQL files (not even the comments) contains it.

| File         | Line | What goes there                                        |
|--------------|------|--------------------------------------------------------|
| `B_fill.sql` | 62   | `-1` becomes the `calls` value of A's `TOTAL` row      |

## Steps

1. Check that #49 is live on the server. In the server checkout:

   ```sh
   cd "$REPO" && git merge-base --is-ancestor ab36869 HEAD && echo "#49 is in"
   ```

   It must print `#49 is in` (ab36869 is the merge of #49; the website PR
   #45 came before it). If it prints nothing, #49 is not on the server:
   stop, pull and restart `-track` first. Also, `-track` must have been restarted after that
   commit was pulled (`git pull` alone changes nothing in a running
   process): its start is later than the pull, and since then calls that
   finish rugged by the price rule log `rugged by the price rule, rug time
   ...`. `-web` must also have been started on that commit or later, so
   the website shows these calls as collapsed.
2. Run `A_count.sql` and paste the whole result grid to the PM. **If A's
   `other, total` is not 29, stop here.** (`other`, status not done is
   expected to be 2: the two `repeat` rows.) Check also that the `check`
   row and the `left out` rows marked `0` are 0. Wait for the go-ahead.
3. The tracker may keep running (see "Why B may run beside the tracker").
4. In `B_fill.sql`, Ctrl+F `<<< EDIT` (line 62): set `expected_count` to the
   `calls` value of A's `TOTAL` row, then run the whole file.
   If the count does not match, B stops with an error and changes nothing
   (no backup table either): the error says how many calls it found. Run
   `ROLLBACK;` in the same tab (or close the tab), run A again, and ask the
   PM before changing the number. B's grid shows the calls filled by source
   and `filled_at`.
5. Run `C_check.sql` and paste its grid to the PM (expectations below).
6. Later runs of C (any time while the backup table exists) show calls
   moving from "as B left it" to "drained" as the tracker finds pools
   drained. That is expected.
7. Only if the PM says B's result must be undone: run `R_rollback.sql`, then
   `C_check.sql` again, and paste both grids to the PM.
8. When the PM is satisfied (with B, or after R), drop the backup table
   (the last line of this file).

## What C shows

| Row                                    | After B                                | After R                          |
|----------------------------------------|----------------------------------------|----------------------------------|
| `backup`                               | B's TOTAL                              | B's TOTAL                        |
| `state` as B left it                   | B's TOTAL, less "drained"              | 0                                |
| `state` drained                        | 0 at first, may grow                   | as before R (R leaves them)      |
| `state` no rug time                    | 0                                      | the rows R restored              |
| `state` anything else                  | 0                                      | 0, plus calls R skipped because they were tracked again |
| `info` tracked again since B           | 0 (rarely more)                        | as after B                       |
| `sanity` (both rows)                   | 0                                      | 0                                |
| `selection` (B would select now)       | 0                                      | the rows R restored              |
| `other` status done                    | 27                                     | 27                               |
| `other` status not done                | 2                                      | 2                                |
| `other` total                          | 29                                     | 29                               |
| `left out` last price NULL             | 0                                      | 0                                |
| `kinds` `price`                        | up by B's TOTAL (less "drained")       | as before B, plus calls the tracker timed since |

The `src` columns (`candle`, `horizon`, `last_due`) split the same rows by
where B's time came from.

## R: when it is safe

At any time while the backup table exists, with the tracker running or not.
R works only from the backup table, never from B's selection again, and
restores a call only while it is exactly as B left it: `rug_at_kind =
'price'`, `rug_at` equal to B's time, and `updated_at` unchanged since B (not
tracked again). It leaves alone, and counts in its grid:

- calls whose `'price'` time a drained time replaced (the right time now);
- calls the tracker tracked again since B (their new state is the
  tracker's).

R is safe to run again: a second run restores nothing. Running R loses no
data that existed before B: B's only change is these two columns, and their
old values (both empty) are in the backup.

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
  transaction may stay open in that tab and keep its row locks (the tracker
  would wait on them): run `ROLLBACK;` in the same tab or close the tab.
- B leaves a small temporary table in the session
  (`price_rug_fill_params`), R too (`price_rug_fill_restored`); they
  disappear when the tab is closed. The backup table is a real table and
  stays until you drop it.

## Drop the backup table

Once the PM is satisfied (with B, or after R), run:

```sql
DROP TABLE scout_call_tracking_bak_price_rug_fill_20261020;
```
