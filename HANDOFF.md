# Scout analytics: handoff to Claude Code

Written 6–7 October 2026. Start Claude Code in the repo root; `.claude/settings.json`
makes the session the product manager, which delegates to the agents in
`.claude/agents/` (see "Agent setup").

First message to give it:

> Read telegrambot/scoutanalytics/HANDOFF.md and README.md, then check
> "Prod state and pending owner actions" with me before starting "Next work".

## What the system is

Branch `scout-call-model`. Production runs `main`.

- Listener: reads @scoutrobinhood, tells real calls from "hit 3X" update posts
  (`postkind.go`), sends new tokens to @perceptor0xBot and @salpha_research_bot,
  delivers to the private group, records to Postgres.
- Tracker (`-track`): on-chain prices from Uniswap v2/v3/v4 pools and Pons V2
  bonding curves, returns at 1h/1d/3d/7d/30d, 5-minute and hourly candles,
  pre-call trading stats, token names from `name()`. Only the first real call of
  each token is tracked; later calls get status `repeat`. A latest-price pass
  keeps a current return per token.
- Website (`-web`, no login, read-only): one row per token from an in-memory
  snapshot refreshed every 15 seconds, a "Refresh now" button, 17 columns,
  search, sorts, Perceptor filter, legend and a per-row report detail pane.
  Plain JavaScript in `frontend/` (no framework, no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. Not
  trained on real data yet.

## 1. Shipped

Committed and pushed on `scout-call-model`, merged to `main` through PRs
(#10–#14); prod runs `main`.

- Latest % (514b073); eth_getLogs range-size fix (af8e8fe); Call MC and Latest
  MC columns (9894e0f); Refresh now and the 17-column layout (1f4e206);
  `-retry-no-pool` (8ed80f9).
- Report detail pane (`GET /api/call`) and the rug guard (8846dc6): quote side
  under `SCOUT_RUG_LIQ_USD` (default $500, set in `.env`) means rugged, −100%
  from the rug point; bound prices (2^128) ignored; 1e6× backstop.
- Pons V2 support (d39f399, 730b1c2): curve pricing from `CurveBuy`/`CurveSell`,
  graduation to the v4 pool through the Pons hook, no USD rug check on the curve,
  graduation found from indexed logs (no look-back limit), no PoolManager
  fallback for Pons.
- In 40e1888 (the subject line only mentions ops and ML):
  - v4 pool discovery via `Initialize` (no PoolManager-wide scan; the old
    400-step backward walk is gone).
  - USD-source fix: one lookup chain everywhere (stablecoin → Robinhood feed →
    mainnet feed → own pool against ETH or a stablecoin, chosen per block); no
    cached errors; the data-loss fix (state is saved only when every step of a
    horizon segment succeeds); feeds loaded from `asset_chains`, with the env
    var winning on conflict; Chainlink aggregator switches handled; the gecko
    source uses reserve/2.
  - Ops scripts (pgAdmin, A/B/C/D): `ops/2026-10-rug-retrack`,
    `ops/2026-10-pons-v4-retrack`, `ops/2026-10-asset-chains-feeds`.
  - ML: outcomes over `MAX_OUTCOME_PCT = 1e5` excluded, simulation return capped
    at `SIM_MAX_RET_PCT = +1000%` (owner approved), tracker columns forbidden as
    model inputs.
- 4cd2bd5 (Pons-v4 re-track skips rows already tracked by the Pons-aware code)
  is pushed on `scout-call-model` but not yet merged to `main`. It changes only
  the ops scripts, which the owner runs from the branch checkout.

## 2. Not yet committed (owner must commit and push)

The website follow-ups and restyle (Mantine-style, after oca.lylelabs.io):

- sAlpha declines ("Not enough public signals…", "Too little liquidity…") show
  "sAlpha did not generate a report", with no badge.
- One Perceptor line (no more "no red flags · No red flags found").
- Rugged rows: the API sends no peak; Status shows only the "rugged" badge
  (owner approved).
- Huge numbers in 10ⁿ notation, exact value in the tooltip.

`git status` on 7 October shows exactly these uncommitted files, all under
`telegrambot/scoutanalytics/`:

- modified: `frontend/app.js`, `frontend/index.html`, `frontend/style.css`,
  `scout_models.data.go`, `scout_models.go`, `web.go`, `web_db_test.go`,
  `web_detail_db_test.go`, `websnapshot.go`, `websnapshot_test.go`
- untracked: `websnapshot_rug_test.go`
- also untracked: `.claude/` at the repo root (see "Agent setup").

Run the reviewer agent on this diff before giving the owner commit commands.

## 3. Prod state and pending owner actions

1. **Pons-v4 re-track** (`ops/2026-10-pons-v4-retrack`): B ran on 539 calls,
   `reset_at` 2026-10-06 23:16:27.758076-07. At the last C: 35 re-tracked (6
   went back to the curve, so they had wrong entries; 20 on v4), race 0. Re-run
   C until `waiting` = 0.
2. **Rug re-track** (`ops/2026-10-rug-retrack`): 151 calls, `reset_at`
   2026-10-06 15:13:37.355323-07. At the last C: 36 re-tracked, race 0. Confirm
   it finished.
3. **asset_chains seed** (`ops/2026-10-asset-chains-feeds`): run
   `A_inspect.sql`, fill the EDIT values in B (`asset_type_id`, the chain row
   values, `expected_count` 33), run B, then C. Token addresses are verified;
   feed addresses come from the owner's paste.
4. **USD repair:** calls stuck in a non-USD `price_unit` (ORBIO-type Pons
   quotes, HOODon, RDDT, meme quotes) and calls that lost candle segments
   ("candles to +…" errors). The selection SQL exists (in the USD coder's
   report); it still has to become a tested pgAdmin script set in `ops/` like
   the others (Next work 1).
5. **REMINDER (the owner asked to be reminded):** decide whether to re-track
   the 1,166 old Uniswap v4 calls. They have no stored liquidity (the old code
   never measured v4), so the liquidity filter cannot select them. About 2–4
   hours of tracker time.
6. Commit and push the website work (section 2), merge, and deploy (restart
   `-web`).
7. Decide about `stash@{0}` ("On codex/scout-dashboard: scout-dashboard before
   main sync 2026-10-06"; touches the scout README, `main.go`,
   `scout_models.data.go`, `scoutanalytics.sql`, `tracker.go` and a test). It
   is probably superseded and will not move to the new repo. (`stash@{1..3}`
   are old GitHub Desktop stashes from other branches.)
8. After the backlog clears, run the launchpad queries: status by launchpad;
   error kinds; 3 sample CAs per unpriced launchpad. O1 Rwa: 59 of 60 were
   just pending. Possible gaps: Lunch Pair V4 (0 done), Pools Trade.
9. **Prod settings** (for reference):
   - `-track` with `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12
     SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48`, latest-price pass on.
   - `SCOUT_CHAINLINK_FEEDS` in `.env` (10–33 stock feeds); the web port is
     `SCOUT_WEB_ADDR` in `.env`.
   - Nitro runs with `--execution.rpc.log-history=0` (full log index; a full
     node, not an archive). Keep it, or old ranges slow down again.
10. Prod uses **pgAdmin** for SQL. Ops scripts must have no psql
    meta-commands, and the `expected_count`/`reset_at` EDIT lines must be
    marked with `<<< EDIT` on the exact line (the owner has twice edited the
    wrong line).

## 4. Next work (code), in order

1. USD-repair pgAdmin scripts (item 3.4), in `ops/` with the same A/B/C/D
   layout, tested against a throwaway Postgres.
2. `settings.json`: add `PowerShell(git push:*)` to deny (Bash rules do not
   cover the PowerShell tool) and the matching PowerShell allows.
3. Small fixes:
   - `-price-check` should load the DB feeds (`main.go`, two lines);
   - cap retries for an entry whose quote first traded after the call (then
     fall back to quote units);
   - deterministic choice of the call-time block;
   - the "node type" wording.
4. Tracker efficiency: a rolling worker queue (today each cycle waits for its
   slowest call); the latest pass re-reads blocks the horizon scan reads later;
   checkpoints within a horizon segment; head-block caching; the flaky test
   `TestLatestPriceInterruptedLeavesRowUntouched`.
5. Open question: should `prior_calls`, `calls_prev_1h` and `calls_prev_24h`
   (model inputs) count update posts? They still do.
6. Support for other launchpads, after item 3.8.
7. **Train the baseline model** (the goal of the prediction plan) once the
   backlog and re-tracks are done; ml-coder agent.
8. Scatter plot with ECharts 6.1 (decided; the demo was in a former scratchpad
   and is gone after the restart; rules in `coder.md`, "Charts").
9. Push updates via Server-Sent Events (`GET /api/events`; the listener
   `NOTIFY`s, the web process `LISTEN`s and refreshes its snapshot).
10. Other call sources (Call Analyser channels; parked).
11. DEPLOY.md / infra.

## 5. Repo migration (on hold until the pending tasks are done)

- Plan: `git filter-repo --subdirectory-filter telegrambot/scoutanalytics` on a
  fresh clone of **`https://github.com/kfukue/geth-analytics.git`** (the real
  remote, not geth-analytics-api), from `origin/scout-call-model`.
- New module `github.com/kfukue/scoutanalytics`, `package main` at the root.
- Copy `database/database.go` to `internal/database` unchanged (same `.env`);
  new `.gitignore`; copy `.claude/` with paths rewritten; docs to `go run .`.
- Pre-checks done: no secrets ever in history (all refs); `origin/main` has
  nothing the branch lacks; `codex/scout-dashboard` is local-only with no
  unique commits; 28 commits.
- Blockers: the uncommitted website work (section 2) and the stash decision
  (item 3.7).
- Prod cut-over: copy `.env`; stop the old listener before starting the new one
  (never two on one Telegram session); copy `scout.session.json` and
  `scoutanalytics_data/`; start with `go run .`; rollback = restart the old one.
- Later: `MaxConns` for `SCOUT_DATABASE_URL` is hard-coded to 4 and needs
  raising for 12 workers; `database.go` fatally requires `.env` in the working
  directory.

## Rules the owner has set

- Never print, commit or copy `.env`, `scout.session.json`,
  `scoutanalytics_data/`.
- No new database tables without asking; extend existing ones. Every statement
  in `scoutanalytics.sql` must be safe to repeat (it runs at every start).
  Views are dropped and recreated at startup, dependents first.
- Shared tables (`assets`, `chains`, `asset_chains`) are never altered; the
  tracker only SELECTs from them. Change rows with SQL, not the API's
  `/assetChains` routes.
- Website speed is the priority: requests are answered from the snapshot and
  must not query the database (the only exception is the rate-limited
  `POST /api/refresh`). Keep p95 under 10 ms; rerun
  `go test -run xxx -bench BenchmarkWebSnapshot -benchmem` after touching it.
- Only real calls are scanned, delivered and tracked; one row per token.
- The owner commits and pushes; agents' git commit and push are blocked by
  permissions. Never commit to `main`.
- Say "tested locally" unless it ran on the prod server. Flag anything that
  makes the tracker redo history or adds ongoing node load before shipping it.
- Ask the owner before resetting calls; give him pgAdmin scripts (see 3.10).
- One line of work at a time on this database: two branches migrating the same
  schema caused both production startup failures so far.
- At most **3** coding agents in parallel, each on disjoint files.
- The owner prefers not to set up a local test database. For prod data he runs
  read-only queries and pastes the results.

## How to test

- `go vet ./telegrambot/scoutanalytics` and
  `go test -race ./telegrambot/scoutanalytics`. Database tests need
  `SCOUT_TEST_DATABASE_URL` pointing at a throwaway Postgres named
  `scout_test` (`TestOpenScoutStoreSelection` needs that name), run with
  `-p 1`; without it they are skipped.
- Throwaway Postgres: `initdb` in the agent's own scratchpad subdirectory, its
  own port on 127.0.0.1, `pg_ctl stop` and delete the directory at the end.
  Never touch the Windows Postgres service on port 5432.
- `TestLatestPriceInterruptedLeavesRowUntouched` is flaky (about 1 run in 10,
  old code too); rerun before treating it as real.
- On-chain code uses the fake chain in `onchain_test.go`; never call real nodes.
- `ml/`: `python -m pytest tests -q` with the ml venv.
- Go files are CRLF: check gofmt on LF copies.
- Page changes: check at 1280px and 390px, light and dark. DOM with
  `textContent` only, and keep the Content-Security-Policy (no inline script or
  style).
- Ops SQL: run A/B/C/D against the throwaway Postgres before handing them over.

## Deploying

The owner commits and pushes `scout-call-model` from the PC, merges it into
`main` with a PR, and deploys `main` on the server: `git checkout main && git
pull`, then restarts the processes that changed:

- `go run ./telegrambot/scoutanalytics -listen-only`
- `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run ./telegrambot/scoutanalytics -track`
  (latest-price pass on; `SCOUT_RPC_LOG_CHUNK` unset, default 200000).
- `go run ./telegrambot/scoutanalytics -web`. A reverse proxy in front must
  pass the `Host` header unchanged, or the same-origin check makes
  `POST /api/refresh` return 403.

After a tracker change: `-price-check` on 2–3 affected tokens before the full
run.

## Agent setup

- `.claude/agents/`: `coder` (Go + plain JS website, with the Go, JS, CSP and
  Charts rules), `ml-coder`, `react-coder`, `infra` (approval-gated),
  `reviewer` (approved; run it before giving commit commands), `researcher`,
  `product-manager`.
- At most 3 coding agents in parallel (owner's limit).
- `.claude/` is untracked; the owner decides whether to commit it.
- `settings.json` was tightened: read-only git, `node --check` and the ml venv
  pytest are allowed; secrets are denied in any folder; clutter is listed in
  `.git/info/exclude`. Still missing: the PowerShell `git push` deny (Next
  work 2).
- New or changed agents load only after a Claude Code restart.
- Lessons:
  - Parallel coders must have disjoint files (parallel README edits got mixed
    before).
  - Usage limits cut agents off mid-task, so a restarted agent must first check
    `git status` and the files for partial work.
  - Each throwaway Postgres gets its own scratchpad directory and port and is
    deleted afterwards.

## Reference

- Pons V2: factory 0x7eD598BcEf8bd9Edd8C97A195C6d13f40801EC7e
  (`TokenLaunched`, `PoolGraduated`); one bonding curve per token; graduates
  at 4.2 ETH to a Uniswap v4 pool behind hook
  0xE5e702641Ea86F4ae6cC3cDaeD2B886f976Be044. Pons V1 (factory
  0xA5aAb3F0c6EeadF30Ef1D3Eb997108E976351feB) uses Uniswap v3. Source:
  github.com/ponsdotdev/pons-labs.
- Robinhood stock tokens: addresses from
  `https://api.robinhood.com/rhj/prices/<SYMBOL>` (chain 4663); feeds are
  Chainlink "Standard Proxy" addresses; the 33 pairs are in
  `ops/2026-10-asset-chains-feeds/B_seed.sql`. HOODon
  (0xfb5b5778d45ae47f15323fb59b666c655174a79c) is not a Robinhood token; RDDT
  is official but has no feed.
- Token names are read with ERC-20 `name()` into
  `scout_call_tracking.token_name` (and `token_symbol_onchain`).
