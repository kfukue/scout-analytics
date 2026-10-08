# One-off ops scripts (pgAdmin)

> **Each folder's B may only be run once. No folder is pending now (7 Oct
> 2026): every B below has been run. Never run any of them again.**

Each folder holds hand-run SQL scripts for pgAdmin (A = read-only count, B =
the write, C = check, D/E = optional follow-ups); its own `README.md` has the
steps. The program never executes anything under `ops/`. Every new folder gets
a distinct name and a row with its status in this table.

| Folder                          | Status                    | Date run | What it does                                                                                   | Run next?          |
|---------------------------------|---------------------------|----------|------------------------------------------------------------------------------------------------|--------------------|
| `2026-10-rug-retrack`           | DONE (151 calls)          | 6 Oct    | Re-tracks first calls tracked under the old rug rules (low liquidity or impossible numbers)    | No, never run B again |
| `2026-10-pons-v4-retrack`       | DONE (539 calls)          | 6 Oct    | Re-tracks Pons V2 calls that the old tracker priced on their Uniswap v4 pool                   | No, never run B again |
| `2026-10-asset-chains-feeds`    | DONE (33 feeds)           | 7 Oct    | Seeds the Robinhood Chain stock tokens and Chainlink feeds into the main API database          | No (B again inserts nothing) |
| `2026-10-usd-repair`            | DONE (50 calls + 1 nudge) | 7 Oct    | Re-tracks calls hurt by the old USD lookup (stuck in quote units or lost candle segments)      | No, never run B again |
| `2026-10-07-liquidity-retrack`  | DONE (649 calls; 52 found rugged) | 7 Oct | Re-tracks the old Uniswap v4 calls (no v4 liquidity check, old USD lookup, old call-time block); cutoff 2026-10-06 12:53:00-07, `reset_at` 2026-10-07 12:18:38.004137-07 | No, never run B again |

Not to be confused: `2026-10-pons-v4-retrack` (DONE, Pons V2 calls only) and
`2026-10-07-liquidity-retrack` (DONE, the old Uniswap v4 calls).
