# Repair the calls hurt by the old USD lookup (pgAdmin version)

> **One-off, October 2026.** These scripts re-track the calls that the tracker,
> before the USD-source fix, switched to quote units for good or let lose a
> segment of candles and peak/low. They are run **by hand in pgAdmin**; the
> program never executes them (nothing under `ops/` is compiled or embedded).
> Same conventions as `ops/2026-10-rug-retrack/`.

## Why

Before the USD-source fix (one lookup chain: stablecoin, Robinhood feed,
mainnet feed, own pool against ETH or a stablecoin, chosen per block):

- some calls were switched **for good** to quote units
  (`scout_call_tracking.price_unit` not `usd`; e.g. ORBIO-quoted Pons curves,
  HOODon, RDDT, PIPEDOG, PONS, SHIB, VIRTUAL);
- some calls **lost a segment** of candles and peak/low: the old code saved
  the scan progress even when converting that segment's candles to USD failed
  (error text starting `candles to +` with `no USD source` or
  `no trades in the`).

The new code prices both correctly, so they are tracked again from scratch.

Errors at the entry or at a horizon close (`USD price of ...`) lost nothing;
those calls only need to be retried now: `E_nudge_entry_errors.sql`.

## What B resets

Every FIRST call of a token (same first-call rule as the other re-tracks:
address without regard to case, earliest post, lowest id on ties, update
posts never count), status not `repeat`, that matches any of:

- **(a) quote_units**: `price_unit` set and not `usd`, status `tracking`,
  `done` or `error`;
- **(b) candle_error**: `error` starts with `candles to +` and contains
  `no USD source` or `no trades in the` (any status but `repeat`; these are
  often `gave_up`, because the tracker gives up on "no trades" errors after
  its deadline);
- **(c) candle_gap**: OFF by default, see below.

The WHERE clause (identical in A, B and C):

```sql
WHERE t.status <> 'repeat'
  AND (   (t.status IN ('tracking','done','error')
           AND t.price_unit IS NOT NULL AND t.price_unit <> 'usd')
       OR COALESCE(t.error LIKE 'candles to +%'
                   AND (t.error LIKE '%no USD source%' OR t.error LIKE '%no trades in the%'), false)
       OR (p.include_gap_heuristic
           AND t.status IN ('tracking','done','error') AND t.onchain IS NOT NULL AND EXISTS (
              SELECT 1 FROM scout_call_returns r
              WHERE r.call_id = t.call_id AND r.last_trade_at IS NOT NULL
                AND NOT EXISTS (SELECT 1 FROM scout_call_candles c
                                WHERE c.call_id = r.call_id AND c.interval_seconds = 3600
                                  AND c.bucket_start = date_trunc('hour', r.last_trade_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'))))
```

Deleted: their rows in `scout_call_returns`, `scout_call_candles`,
`scout_call_precall`. The tracking row goes back to `pending` (due now,
attempts 0) without pool, prices, `price_unit`, on-chain state, rug flag or
latest price. Kept: token name and on-chain symbol, priority, entry time,
contract address, created_at. Not touched: `scout_call_metrics`,
`scout_call_predictions`, the calls themselves.

Expected size on prod: unknown; A tells.

## (b) shrinks with every tracker cycle: run A soon

The tracker clears `error` at the start of every attempt, and (b) rows in
status `error` are retried with back-off. When the new code retries such a
row, its scan starts from the scan block the old code had already advanced
past the lost segment: it finds nothing to redo, succeeds, and the error text
is gone. The row then drops out of (b), and the lost segment stays lost.
So (b) only counts the rows not yet retried: it is a lower bound, and it
falls over time. Rows that dropped out this way are what rule (c) finds.

## (c) The candle-gap heuristic (optional, off by default)

Rule (c) looks for the trace a lost segment leaves: a horizon's
`last_trade_at` (the time of the last trade the tracker folded in) whose UTC
hour has no hourly candle (`interval_seconds = 3600`), although every trade
the tracker scans lands in an hourly candle. A row of (b) that was retried
(see above) has exactly that trace: the old code kept the last trade of the
lost segment but not its candles. That makes (c) the place where most of the
decayed (b) rows now are; A's info row `gap_heuristic_candidates` shows how
many. It only looks at calls with on-chain state (`onchain IS NOT NULL`) and
status tracking/done/error (`pending` rows can still carry returns of an
older run that are about to be replaced).

**False-positive risk** (a call it selects may be fine):

- calls whose candles were written by older code paths (for example before
  candles existed, or under rules since changed) can lack a candle that the
  current code would have written, without any data loss in the returns;
- the candle hour is placed from the hour-boundary blocks, the trade time from
  the block's own timestamp; if a node returned a slightly different boundary
  block, a trade right at an hour boundary can land in the neighbouring hour
  (minor);
- it cannot tell why the candles are missing: a call whose candle save failed
  for another reason under the old code (database, node) is selected too.
  That one also lost its segment, so re-tracking it is right.

A false positive costs one full re-track of that call (node time), nothing
else. **Only turn it on if the PM says so**, after looking at that number. To
turn it on, change the line marked `OPTION` (`false AS include_gap_heuristic`
to `true`) in A, B **and** C, the same way in all three.

## Files

Open each in pgAdmin's Query Tool, connected to the scout database, and run
the WHOLE file with F5; each shows ONE result grid at the end.

| File                        | Kind      | Purpose                                                         |
|-----------------------------|-----------|-----------------------------------------------------------------|
| `A_count.sql`               | read-only | what B would reset (by reason, price_unit, status, TOTAL, info) |
| `B_reset.sql`               | WRITES    | the reset, one transaction, checks the count                    |
| `C_check.sql`               | read-only | check after B (needs B's reset_at)                              |
| `D_catchup.sql`             | WRITES    | only if C shows race_victims > 0 (needs reset_at)               |
| `E_nudge_entry_errors.sql`  | WRITES    | sets next_check_at = now() on the entry/horizon USD errors      |

## The lines you edit (search with Ctrl+F for `<<< EDIT`)

Each file you edit has exactly ONE line containing `<<< EDIT`; no other line
(not even the comments) contains it. Press Ctrl+F in the Query Tool, type
`<<< EDIT`, and change only that line. A has no such line.

| File                        | Line | What goes there                                                    |
|-----------------------------|------|--------------------------------------------------------------------|
| `B_reset.sql`               | 60   | `-1` becomes the `selected` value of A's `TOTAL` row                  |
| `C_check.sql`               | 40   | the text between the quotes becomes B's `reset_at`                 |
| `D_catchup.sql`             | 35   | the text between the quotes becomes B's `reset_at`                 |
| `E_nudge_entry_errors.sql`  | 36   | `-1` becomes the `selected` value of A's info row `E_entry_usd_errors` |

The `OPTION` line (`include_gap_heuristic`, right below the EDIT line in B
and C, near the top of A) stays `false` unless the PM says otherwise.

## Steps

0. Order with the other re-tracks: preferably run this after the rug and the
   Pons-v4 re-tracks show `waiting = 0` in their C. If you run it earlier,
   it still works, but their C's `waiting` (and `retracked_so_far`) then also
   count the calls reset here, and this C counts theirs.
1. The USD-source fix is deployed and `-track` (and the listener) restarted.
2. Run `A_count.sql` and paste the whole result grid to the PM, soon (rule
   (b) shrinks with every tracker cycle, see above). Wait for the go-ahead.
3. Stop EVERY tracking process: `-track`, and the listener too unless it runs
   with `-listen-only` (it runs the tracker inside the same process).
   Ctrl+C is safe; wait until each process has exited.
4. In `B_reset.sql`, Ctrl+F `<<< EDIT` (line 60): set `expected_count` to the
   `selected` value of A's `TOTAL` row, then run the whole file.
   If the count does not match, B stops with an error and changes nothing:
   the error says how many calls it found. Run `ROLLBACK;` in the same tab (or
   close the tab), compare with A, and ask the PM before changing the number.
5. Note `reset_at` from B's result grid. Copy the whole value exactly as
   shown, with the microseconds and the time zone offset (rounding it
   makes C and D miss rows).
6. Start the tracker again (`-track`, and the listener).
7. In `C_check.sql`, Ctrl+F `<<< EDIT` (line 40): paste `reset_at` between
   the quotes, and run it. `not_reset` must be 0. `race_victims` must be 0.
   Since every tracker was stopped during B, it should be 0; only if it is
   not, paste `reset_at` into `D_catchup.sql` (Ctrl+F `<<< EDIT`, line 35)
   and run it once, then run C again.
   Run C again later to watch `waiting` fall and `retracked_so_far` grow.
   Paste `quote_units_after` and `candle_usd_errors_after` to the PM:
   the first counts re-tracked calls that are in quote units again (the new
   code does that only when no USD source exists at all), the second calls
   that hit a candle USD error again (the new code keeps nothing then and
   retries).
   (You may also run C once between steps 5 and 6, before the tracker
   starts; `race_victims` is then 0 by definition.)
8. With the tracker running, in `E_nudge_entry_errors.sql` Ctrl+F
   `<<< EDIT` (line 36): set `expected_count` to the `selected` value of A's
   info row `E_entry_usd_errors`, and run it. If the number changed since A
   (the tracker retried some of them meanwhile), E stops with an error that
   gives the new number and changes nothing; put that number in and run it
   again. E can also run before B; the two do not depend on each other.

## Do not run B a second time

Right after a successful B, a second run finds 0 calls and stops with an
error, changing nothing. **After the tracker has started, never run B again,
and never change `expected_count` to make it run**: a call with truly no USD
source is back in quote units after its re-track and would be reset again.
Only D is safe to rerun. E is safe to rerun too: it only sets
`next_check_at = now()` again on the calls still in that error.

## E: what it does and does not do

E changes ONE column, `next_check_at`, on calls with status `error` whose
error starts with `USD price of` and contains `no USD source` or
`no trades in the`. Status, attempts, error and `updated_at` stay; the tracker
rewrites them on its next attempt. Calls with the same error but status
`gave_up` are not touched (the tracker never retries `gave_up`); E's result
column `gave_up_not_nudged` and A's info row `E_entry_usd_errors_gave_up`
count them. Paste that number to the PM.

## What you will see

- The website shows NO results for the calls B reset (status pending, no
  returns, no latest price) until each one is tracked again.
- Time: roughly the number of calls x 1 to 2 minutes / the tracker workers
  (`SCOUT_TRACK_WORKERS`). Calls with a long, busy 30-day window take longer.
  Live calls (priority 0) go first.
- Node load: each call costs a pool discovery plus a swap scan from the call
  to its last due horizon, plus the USD lookups.

## pgAdmin notes

- Keep "Auto commit" ON (the default). B, D and E contain their own
  `BEGIN ... COMMIT`; A and C are a single read-only SELECT.
- pgAdmin shows only the LAST result set of a script. Each file is written
  so that the last statement is the result you need. Warnings and errors
  appear in the Messages tab.
- B, D and E start with `ROLLBACK;` the "WARNING: there is no transaction in
  progress" this gives is expected, and so is "NOTICE: schema "pg_temp" does
  not exist, skipping".
- If B, D or E stops with an ERROR, nothing was changed, but the failed
  transaction may stay open in that tab and keep its row locks: run
  `ROLLBACK;` in the same tab or close the tab before starting the tracker.
- B leaves two small temporary tables in the session (`usdrepair`,
  `usdrepair_params`), D (`usdrepair_catchup`, `usdrepair_catchup_params`) and
  E (`usdnudge`, `usdnudge_params`) too; they disappear when the tab is
  closed.
