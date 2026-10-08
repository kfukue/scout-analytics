# Scout analytics: handoff to Claude Code

Up to date as of 7 October 2026. Start Claude Code in the repo root;
`.claude/settings.json` makes the session the product manager, which delegates
to the agents in `.claude/agents/` (see "Agent setup").

First message to give it:

> Read HANDOFF.md and README.md, then check
> "Prod state and pending owner actions" with me before starting "Next work".

**Moved from `kfukue/geth-analytics` (history kept).** This repo,
`kfukue/scoutanalytics`, is `telegrambot/scoutanalytics` of
`kfukue/geth-analytics` extracted with `git filter-repo --subdirectory-filter`
from that repo's `main` at 4369a1d (PR #20), plus one standalone commit (own
`go.mod`, `internal/database`, paths). Every commit hash in this file before
section 5 is an **old-repo** hash; the rewritten hashes for the ones that
matter are in section 5. Package `main` is at the repo root: `go run .`.

## What the system is

Repo `kfukue/scoutanalytics`, branch `main` (the only branch carried over).
Production runs `main` of the old repo at 4369a1d (the owner confirmed the
server has the latest); it moves to this repo with the cut-over in section 5.

- Listener: reads @scoutrobinhood, tells real calls from "hit 3X" update posts
  (`postkind.go`), sends new tokens to @perceptor0xBot and @salpha_research_bot,
  delivers to the private group, records to Postgres. After a restart it
  catches up on missed posts (section 1, 08b3a71).
- Tracker (`-track`): on-chain prices from Uniswap v2/v3/v4 pools and Pons V2
  bonding curves, returns at 1h/1d/3d/7d/30d, 5-minute and hourly candles,
  pre-call trading stats, token names from `name()`. Only the first real call of
  each token is tracked; later calls get status `repeat`. A latest-price pass
  keeps a current return per token. Rolling worker queue.
- Website (`-web`, no login, read-only): one row per token from an in-memory
  snapshot refreshed every 15 seconds, live updates over Server-Sent Events, a
  "Refresh now" button, 17 columns, search, sorts, Perceptor filter, legend and
  a per-row report detail pane. Plain JavaScript in `frontend/` (no framework,
  no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. Not
  trained on real data yet; `ml/RUNBOOK.md` has the steps.

## 1. Shipped

Merged to `main` through PRs #10–#14 (the last is a527d2c, up to 40e1888):

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
  - v4 pool discovery via `Initialize` (no PoolManager-wide scan).
  - USD-source fix: one lookup chain everywhere (stablecoin → Robinhood feed →
    mainnet feed → own pool against ETH or a stablecoin, chosen per block); no
    cached errors; state saved only when every step of a horizon segment
    succeeds; feeds loaded from `asset_chains`, with the env var winning on
    conflict; Chainlink aggregator switches handled; gecko uses reserve/2.
  - Ops scripts: `ops/2026-10-rug-retrack`, `ops/2026-10-pons-v4-retrack`,
    `ops/2026-10-asset-chains-feeds` (the asset_chains seed: 33 feeds, now live
    from the DB; `chains.id` 21 = Robinhood Chain).
  - ML: outcomes over `MAX_OUTCOME_PCT = 1e5` excluded, simulation return capped
    at `SIM_MAX_RET_PCT = +1000%` (owner approved), tracker columns forbidden as
    model inputs.

Pushed to `origin/scout-call-model` (local branch even with it, at 08b3a71)
and merged to `main` through PRs #15–#19 (the last is 21139e1, 7 Oct 23:06
-07:00, up to 08b3a71; `main` and the branch have the same tree). Then #20
(4369a1d, merging d49fe9c: handoff and ops index after the liquidity
re-track). The old repo's `origin/main` at 4369a1d and `origin/scout-call-model`
at d49fe9c have the same tree for `telegrambot/scoutanalytics`; this repo was
extracted from 4369a1d.

- #15: 4cd2bd5: Pons-v4 re-track skips rows already tracked by the Pons-aware
  code.
- #16: 39b1bac: website restyle (after oca.lylelabs.io), sAlpha declines,
  rugged rows without a peak, 10ⁿ numbers, one Perceptor line. 4eb6fbe:
  previous handoff.
- #17: 7a54f78: USD-repair ops scripts (`ops/2026-10-usd-repair`). 7cf8dd3: SSE
  live updates (`GET /api/events`; the listener `NOTIFY`s, the web process
  `LISTEN`s and refreshes its snapshot; notices only for calls < 1h old;
  `-backfill` sends no NOTIFY). 14052f8: rolling worker queue, deterministic
  `blockAt` + head cache, segment checkpoints, USD own-pool whole-history fix,
  the no-price-yet cap, `-price-check` loads the DB feeds. bde6d81: `.claude/`
  agent setup and permissions, including the `PowerShell(git push:*)` deny.
- #18: 75f666c: liquidity re-track scripts; ML first-call-only training rows,
  view columns test, `ml/RUNBOOK.md`. 2da28ab: ops rename
  (`2026-10-v4-retrack` → `2026-10-07-liquidity-retrack`) and the ops index
  `ops/README.md`.
- #19: 08b3a71 (one commit; the subject line only mentions the multi-select):
  - **Listener catch-up** after a restart: the resume cursor is min(DB,
    `poll_cursor.json`); the newest `SCOUT_CATCHUP_MAX` (default 100) missed
    posts no older than `SCOUT_CATCHUP_MAX_AGE` (default 24h) are handled live,
    older ones stored only; calls still `queued`/`dropped` within 72h are
    requeued; polling reads full batches (`catchup.go` and tests).
  - **Perceptor multi-select** filter on the website (`verdict=clean,caution`).
  - **`DEPLOY.md`** (new).

Prod runs the old repo's `main` at 4369a1d (the owner confirmed the server has
the latest), from the old checkout, until the cut-over in section 5.

## 2. Not yet committed

Nothing. The 7 October doc edits (`ops/README.md`,
`ops/2026-10-07-liquidity-retrack/README.md`) are committed in d49fe9c
(PR #20, 4369a1d). The corrected text of this `HANDOFF.md`, saved after
d49fe9c, is carried into this repo's standalone commit; the old repo's
uncommitted copy can be discarded.

## 3. Prod state and pending owner actions

State on 7 October:

- All re-tracks are done (see `ops/README.md`): rug 151, Pons-v4 539, USD
  repair 50 + 1 nudge, liquidity 649 (cutoff 2026-10-06 12:53:00-07,
  `reset_at` 2026-10-07 12:18:38.004137-07; 52 found rugged). The owner chose
  to re-track only the pre-rug-guard v4 calls (647 at selection time) rather
  than all 1,166 old v4 calls. No ops folder is pending.
- About 4,733 first calls, about 99% priced.
- Rugged: about 12% overall; 395 of 2,867 v4 calls.
- No stuck `queued`/`dropped` calls (the requeue preview was empty).
- The 33 asset_chains feeds are live from the DB; `.env` still has
  `SCOUT_CHAINLINK_FEEDS` as a backup (the env var wins on conflict).

Pending owner actions:

1. Create the empty private repo `kfukue/scoutanalytics` and push this repo's
   `main` (section 5).
2. The prod cut-over to the new checkout (section 5, checklist).
3. **Prod settings** (for reference):
   - `-track` with `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12
     SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48`, latest-price pass on.
   - The web port is `SCOUT_WEB_ADDR` in `.env`.
   - Nitro runs with `--execution.rpc.log-history=0` (full log index; a full
     node, not an archive). Keep it, or old ranges slow down again.

## 4. Next work, in order (owner's decision)

1. Done: **the stash decision.** `stash@{0}` ("On codex/scout-dashboard:
   scout-dashboard before main sync 2026-10-06") stays behind in the old repo;
   it did not move here. (`stash@{1..3}` are old GitHub Desktop stashes from
   other branches.)
2. Done, pending the owner's push and the prod cut-over: **the repo migration
   to `kfukue/scoutanalytics`** (section 5), BEFORE ML training.
3. **The first ML training** via `ml/RUNBOOK.md`, run from this repo after the
   cut-over (the runbook paths are for this layout). No GPU needed: CPU
   only, it takes seconds. The long (30d) horizon is skipped until about
   mid-November. The gates are strict; scores stay out of deliveries until a
   report passes. ml-coder agent.

Later:

- ML label issues: the 30d collapse uses the `rugged` set after day 30;
  train/serve skew in `quote_asset` and `perceptor_verdict`.
- GCP Pub/Sub for live updates when scaling (owner's plan).
- `MaxConns` for `SCOUT_DATABASE_URL` is hard-coded to 4 (too low for 12
  workers).
- Scatter plot with ECharts 6.1 after the model report (rules in `coder.md`,
  "Charts").
- Other launchpads (about 46 unpriced calls); the launchpad query results are
  pending from the owner.
- Other call sources (Call Analyser channels).
- In-flight scans cut off by a stop end as `failed` and are not requeued.
- Poison post: a stored-only post with a permanent DB error pauses polling.
- Resolved: `prior_calls`, `calls_prev_1h` and `calls_prev_24h` counting update
  posts no longer matters (training uses first calls only, so they are
  near-constant).

## 5. Repo migration (done; pending push and prod cut-over)

**Moved from `kfukue/geth-analytics` (history kept).**

- Done on 7 October: `git filter-repo --subdirectory-filter
  telegrambot/scoutanalytics --refs main` on a fresh clone of
  `https://github.com/kfukue/geth-analytics.git`, from `origin/main` at 4369a1d
  (same tree as `origin/scout-call-model` for this folder). 37 commits touch
  the folder (55 with merges). No secrets anywhere in the extracted history
  (no `.env`, `*session.json`, `calls.csv`, `scoutanalytics_data/` or `.exe`
  object). Only `main` was carried over; the old stash stayed behind.
- Then one standalone commit, no functional changes: module
  `github.com/kfukue/scoutanalytics` (`go.mod`/`go.sum` from the old repo,
  `go mod tidy`, same versions for every module still required);
  `internal/database/database.go` copied unchanged from the old
  `database/database.go` (same `.env` variables); new `.gitignore`; `.claude/`
  copied with paths rewritten; docs and comments to `go run .` from the repo
  root.
- Old → new hashes (filter-repo rewrote every commit): 4369a1d → 7a4edaa
  (PR #20 merge), d49fe9c → 34d70c4, 21139e1 → 0795e9e (PR #19 merge),
  08b3a71 → b10251b. Older hashes in this file and in the ops READMEs are
  old-repo hashes; `git log --grep` or the subject line finds them here.

Owner's push (from the extracted repo on the PC): create the empty private
repo `kfukue/scoutanalytics` on GitHub (no README, licence or .gitignore), then
`git remote add origin https://github.com/kfukue/scoutanalytics.git` and
`git push -u origin main`.

**Prod cut-over checklist** (the old checkout stays untouched for rollback):

1. On the server, clone the new repo next to the old checkout, e.g.
   `git clone https://github.com/kfukue/scoutanalytics.git /srv/scoutanalytics`
   (EDIT the path; a private repo needs a deploy key or token on the server).
   `go build -o /tmp/scout-new .` in it compiles everything before any
   downtime.
2. Copy `.env` from the old checkout into the new one with `cp -p` (never
   print or paste it). Check yourself, without pasting
   values, that the path-valued settings in `.env` still resolve from the new
   folder: `SSL_CERT_FILE_PATH` (the three `.pem` files), any credentials file
   for Secret Manager (`HOST_SECRET_PATH` access), and `SCOUT_SESSION_FILE`,
   `SCOUT_STATE_DIR`, `SCOUT_WEB_DIR`, `SCOUT_RPC_LOG_CACHE` if set. Relative
   paths need the files copied too, or absolute paths.
3. Optional read-only check from the new folder (needs `.env`):
   `go run . -price-check 0xTokenCA -price-at 6h`. It confirms the database and
   node settings and exits.
4. Stop the old processes, **listener first** (never two listeners on one
   Telegram session; `pgrep -af listen-only` must print nothing), then `-track`
   and `-web` (never two trackers at once).
5. Only now, with the old listener stopped (the session file and the state are
   final), copy `scout.session.json` with `cp -p` and `scoutanalytics_data/`
   (with `poll_cursor.json`) with `cp -a`. Never print or paste them.
6. Start from the new folder: `go run . -listen-only`, then
   `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4
   SCOUT_RPC_MAX_INFLIGHT=48 go run . -track`, then `go run . -web`. Check the
   `recording to SQL via …` line, the catch-up lines, `website on …`, and
   `curl /api/summary` (DEPLOY.md section 6).
7. Rollback: stop the new listener first, then the others; restart the old
   ones from the old checkout (still at 4369a1d). Its listener resumes from the
   lower of the database and its own `poll_cursor.json`.
8. After the cut-over: record `git rev-parse HEAD` in the new checkout for
   DEPLOY.md 3.0 (the old `/tmp/scout-prev-commit` hash is not in this
   history). The systemd drafts in DEPLOY.md use
   `WorkingDirectory=/srv/scoutanalytics` (EDIT).
9. Removing `telegrambot/scoutanalytics` from the old repo is a separate,
   later decision; nothing in the old repo was changed.

- Later: `database.go` fatally requires `.env` in the working directory.

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
- Wait for all checks (tests, reviewer) before committing; separate clean
  commits per change.
- Say "tested locally" unless it ran on the prod server. Flag anything that
  makes the tracker redo history or adds ongoing node load before shipping it.
- Ask the owner before resetting calls; give him pgAdmin scripts.
- Prod uses **pgAdmin** for SQL, and the owner edits in pgAdmin's editor. Ops
  scripts must have no psql meta-commands; point him to the exact lines to
  change, each marked `<<< EDIT` on that line (he has twice edited the wrong
  line).
- Ops folders get distinct names and a status row in `ops/README.md`.
- One line of work at a time on this database: two branches migrating the same
  schema caused both production startup failures so far.
- At most **3** coding agents in parallel, each on disjoint files.
- The owner prefers not to set up a local test database. For prod data he runs
  read-only queries and pastes the results.

## How to test

- From the repo root: `go vet .` and `go test -race .`. Database tests need
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

See `DEPLOY.md` (committed in 08b3a71) for the full steps. In short:
the owner commits and pushes a work branch from the PC, merges it into
`main` with a PR, and deploys `main` on the server (after the cut-over, in the
new checkout): `git checkout main && git pull`, then restarts the processes
that changed, from the repo root:

- `go run . -listen-only`
- `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run . -track`
  (latest-price pass on; `SCOUT_RPC_LOG_CHUNK` unset, default 200000).
- `go run . -web`. A reverse proxy in front must
  pass the `Host` header unchanged, or the same-origin check makes
  `POST /api/refresh` return 403.

After a tracker change: `-price-check` on 2–3 affected tokens before the full
run.

## Agent setup

- `.claude/agents/` (committed in bde6d81 in the old repo; copied here in the
  standalone commit with paths for this layout): `coder` (Go + plain JS website,
  with the Go, JS, CSP and Charts rules), `ml-coder`, `react-coder`, `infra`
  (approval-gated), `reviewer`, `researcher`, `product-manager`. All 7 are
  active.
- The reviewer runs before every commit.
- At most 3 coding agents in parallel (owner's limit).
- `settings.json`: read-only git, `node --check` and the ml venv pytest
  (`ml/.venv`, or `.venv` from inside `ml/`) are allowed; secrets are denied in
  any folder; `git push` is denied for both Bash and PowerShell. Binaries,
  secrets, state, CSVs and the venv are in `.gitignore`.
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
  `https://api.robinhood.com/rhj/prices/<SYMBOL>` (chain 4663; `chains.id` 21
  in the API database); feeds are Chainlink "Standard Proxy" addresses; the 33
  pairs are in `ops/2026-10-asset-chains-feeds/B_seed.sql`. HOODon
  (0xfb5b5778d45ae47f15323fb59b666c655174a79c) is not a Robinhood token; RDDT
  is official but has no feed.
- Token names are read with ERC-20 `name()` into
  `scout_call_tracking.token_name` (and `token_symbol_onchain`).
- One-off ops scripts and their status: `ops/README.md`.
