# One-off ops scripts (pgAdmin)

> **Each folder's B may only be run once. Every folder marked DONE below has
> had its B run: never run that B again. One folder is pending (22 Oct
> 2026): `2026-10-22-park-stuck-calls`, to run only with the tracker
> stopped.**

Each folder holds hand-run SQL scripts for pgAdmin (A = read-only count, B =
the write, C = check, D/E = optional follow-ups, R = rollback: undoes a B
that finished, from the backup table B saved); its own `README.md` has the
steps. The program never executes anything under `ops/`. Every new folder gets
a distinct name and a row with its status in this table.

Not a folder and not in the table: `scout-model.service` and `scout-model.md`
are a systemd unit draft and its runbook for the scoring service
(`ml/serve.py`, DEPLOY.md 2.4). They are not pgAdmin scripts and never touch
the database.

| Folder                          | Status                    | Date run | What it does                                                                                   | Run next?          |
|---------------------------------|---------------------------|----------|------------------------------------------------------------------------------------------------|--------------------|
| `2026-10-rug-retrack`           | DONE (151 calls)          | 6 Oct    | Re-tracks first calls tracked under the old rug rules (low liquidity or impossible numbers)    | No, never run B again |
| `2026-10-pons-v4-retrack`       | DONE (539 calls)          | 6 Oct    | Re-tracks Pons V2 calls that the old tracker priced on their Uniswap v4 pool                   | No, never run B again |
| `2026-10-asset-chains-feeds`    | DONE (33 feeds)           | 7 Oct    | Seeds the Robinhood Chain stock tokens and Chainlink feeds into the main API database          | No (B again inserts nothing) |
| `2026-10-usd-repair`            | DONE (50 calls + 1 nudge) | 7 Oct    | Re-tracks calls hurt by the old USD lookup (stuck in quote units or lost candle segments)      | No, never run B again |
| `2026-10-07-liquidity-retrack`  | DONE (649 calls; 52 found rugged) | 7 Oct | Re-tracks the old Uniswap v4 calls (no v4 liquidity check, old USD lookup, old call-time block); cutoff 2026-10-06 12:53:00-07, `reset_at` 2026-10-07 12:18:38.004137-07 | No, never run B again |
| `2026-10-20-price-rug-fill`     | DONE (719 calls filled; backup table kept until the owner drops it) | 21 Oct | Gives past price-rule rugged calls `rug_at_kind = 'price'` and the tracker's approximate `rug_at` (owner decision A2); B saves a backup table, R restores from it | No, never run B again |
| `2026-10-22-park-stuck-calls`   | pending: run only with the tracker stopped | -- | Parks 20 stuck backfilled calls (status `error`, priority 1, retried forever) as `gave_up` so the tracker stops retrying them; B saves a backup table, R restores from it | Yes, once (A first; STOP if A's TOTAL is not 20 or a `check` row is not 0) |

Not to be confused: `2026-10-pons-v4-retrack` (DONE, Pons V2 calls only) and
`2026-10-07-liquidity-retrack` (DONE, the old Uniswap v4 calls).
