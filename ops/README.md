# One-off ops scripts (pgAdmin)

> **Each folder's B may only be run once. Always open files from the folder
> marked PENDING.**

Each folder holds hand-run SQL scripts for pgAdmin (A = read-only count, B =
the write, C = check, D/E = optional follow-ups); its own `README.md` has the
steps. The program never executes anything under `ops/`.

| Folder                          | Status                    | Date run | What it does                                                                                   | Run next?          |
|---------------------------------|---------------------------|----------|------------------------------------------------------------------------------------------------|--------------------|
| `2026-10-rug-retrack`           | DONE (151 calls)          | 6 Oct    | Re-tracks first calls tracked under the old rug rules (low liquidity or impossible numbers)    | No, never run B again |
| `2026-10-pons-v4-retrack`       | DONE (539 calls)          | 6 Oct    | Re-tracks Pons V2 calls that the old tracker priced on their Uniswap v4 pool                   | No, never run B again |
| `2026-10-asset-chains-feeds`    | DONE (33 feeds)           | 7 Oct    | Seeds the Robinhood Chain stock tokens and Chainlink feeds into the main API database          | No (B again inserts nothing) |
| `2026-10-usd-repair`            | DONE (50 calls + 1 nudge) | 7 Oct    | Re-tracks calls hurt by the old USD lookup (stuck in quote units or lost candle segments)      | No, never run B again |
| `2026-10-07-liquidity-retrack`  | **PENDING: next to run**  | not yet  | Re-tracks the old Uniswap v4 calls (no v4 liquidity check, old USD lookup, old call-time block) | **Yes, next**      |

Not to be confused: `2026-10-pons-v4-retrack` (DONE, Pons V2 calls only) and
`2026-10-07-liquidity-retrack` (PENDING, all old Uniswap v4 calls).
