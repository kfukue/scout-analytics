# Scout analytics: handoff to Claude Code

Up to date as of 8 October 2026 (evening: 4e7e049 deployed). Start Claude
Code in the repo root; `.claude/settings.json` makes the session the product
manager, which delegates to the agents in `.claude/agents/` (see "Agent setup").

First message to give it:

> Read HANDOFF.md and README.md, then check
> "Prod state and pending owner actions" with me before starting "Next work".

**Moved from `kfukue/geth-analytics` (history kept).** This repo,
`kfukue/scout-analytics`, is `telegrambot/scoutanalytics` of
`kfukue/geth-analytics` extracted with `git filter-repo --subdirectory-filter`
from that repo's `main` at 4369a1d (PR #20), plus one standalone commit (own
`go.mod`, `internal/database`, paths). Every commit hash in this file before
section 5 is an **old-repo** hash; the rewritten hashes for the ones that
matter are in section 5. Package `main` is at the repo root: `go run .`.

## What the system is

Repo `kfukue/scout-analytics`, branch `main` (the only branch carried over).
Production runs from a checkout of this repo since the cut-over (section 5,
owner confirmed 8 October); prod runs `main` at **4e7e049** (section 3).

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
  "Refresh now" button, 16 columns (17 before 2c), search, sorts, Perceptor
  filter, legend and a per-row report detail pane. Plain JavaScript in
  `frontend/` (no framework, no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. First
  trained on prod 8 October, all gates FAIL (section 4 item 1);
  `ml/RUNBOOK.md` has the steps.

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

Since the cut-over (8 October) prod runs from the new checkout of this repo
(section 5); the old checkout (old repo at 4369a1d) is kept for rollback.

## 2. Merged and deployed: the 8 October work and the re-scan backend

Both branches went out together. `work/2026-10-09-perceptor-rescan`
(stacked on `work/2026-10-08-ml-pool-web`) was merged as PR #1 into
`main` = **4e7e049**. Because it was stacked, the PR also brought in the
8 October commits:

- 26da9d8: ML script (a).
- 4707e77: DB pool size (b). **It also contains all of the README changes of
  8 October**, including the website docs (c) and the stale wording, because
  all README hunks were staged into it.
- 5ad017c: website code (c): `frontend/*` and the web Go files, including the
  2 h stale limit.
- e50695b: HANDOFF.
- 0a46f28: the Perceptor re-scan backend (section 4 item 2).
- c3a1cec: HANDOFF for the re-scan.

a. **One-command ML training**: `ml/run_training.sh` (new), `.gitattributes`
   (new: `*.sh` always LF), `ml/RUNBOOK.md` ("Quick way: one script" and the
   Cleanup note). Tested locally on Windows with stubs, not on Linux.
b. **DB pool size**: `clamp(effective tracker workers + 4, 4, 32)` (effective =
   `SCOUT_TRACK_WORKERS` with the on-chain source, 1 with GeckoTerminal); an
   explicit `pool_max_conns` in `SCOUT_DATABASE_URL` is respected; the startup
   line shows the pool size and where it came from. Files: `main.go`,
   `scout_models.data.go`, `tracker.go`, `internal/database/database.go`,
   `poolconns_test.go` (new), `scout_models_db_test.go`, README/DEPLOY.
   `internal/database` is no longer an unchanged copy of the API repo's (new
   `SetupDatabaseMaxConns`).
c. **Website readability + `?days=` age filter**: `frontend/*`, `web.go`,
   `websnapshot.go`, `web_db_test.go`, `websnapshot_test.go`, README website
   section. Reviewed.

Done: the website "Perceptor today" label (section 4 item 2), merged
(PR #2/#3) and deployed.

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

Done 8 October (owner confirmed): `kfukue/scout-analytics` created and `main`
pushed; prod cut-over to the new checkout. Prod `git rev-parse HEAD` =
**4e7e049**, run on the server (owner confirmed 8 October; for DEPLOY.md
3.0).

Deployed 8 October (owner confirmed):

- The processes were restarted on 4e7e049.
- The tracker startup lines show: the pool at most 16 connections for 12
  tracker workers; 33 Robinhood feeds from the asset database; "latest
  prices on: … every 1h0m0s for older ones; at most 400 call(s) per cycle".
- `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are now in the prod
  `.env`.
- `SCOUT_RESCAN=on` in prod since 8 October: the re-scan lane runs with
  the defaults (at most 100 a day, 10 m gap, 30-day max age).
- Lesson: the first restart picked up a different `.env` file and ran with
  the defaults (24h/200). The tracker reads `.env` from the folder it is
  started in, and a variable already set in the shell wins over the file.
  After every restart, check the `latest prices on:` line.

Still to confirm in prod:

- the `latest prices: … waiting` count falling over the first 1–2 hours;
- the listener catch-up/polling lines and the `website on …` line;
- the phone check of the new table and the "stale" label.

Pending owner actions:

1. Done 8 October: the first ML training (model 20261008-1852; results in
   section 4 item 1).
2. Done 8 October: failed-rescan limit decided = 2 (section 4 item 2).
3. Answer the five early-transaction questions (section 4 item 4).
4. `.claude/commands.txt`: add to `.gitignore` or delete (below).
5. Optional: the sizing queries (read-only, in pgAdmin).
6. No longer needed (resolved by query, 8 October): `-price-check` on
   calls 664, 578 and 504 (the "Uniswap V4" flat calls, section 4 item 1).
7. Answer the five Analytics page questions (section 4 item 5; the PM chose
   defaults meanwhile).

`.claude/commands.txt` is an untracked local prompt file (no secrets). It
should go in `.gitignore` or be deleted; the owner decides.

Decisions taken 8 October: the 1-hour latest-price refresh for old calls is
approved; the Perceptor re-scan is approved with the defaults in section 4;
`env.tmp` in the session scratchpad was deleted by the owner. Decided
8 October: long model waits until about 25 December (option a).

**Prod settings** (for reference):

- `-track` with `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12
  SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48`, latest-price pass on;
  `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are in the prod
  `.env` since 8 October (the website's 2 h stale limit assumes them).
- `SCOUT_RESCAN=on` since 8 October (defaults: `SCOUT_RESCAN_MAX_PER_DAY=100`,
  `SCOUT_RESCAN_GAP=10m`, `SCOUT_RESCAN_MAX_AGE=720h` = 30 days).
- The web port is `SCOUT_WEB_ADDR` in `.env`.
- Nitro runs with `--execution.rpc.log-history=0` (full log index; a full
  node, not an archive). Keep it, or old ranges slow down again.

**Firewall** (prod server, 8 October):

- ufw: 8090 is allowed on tailscale0. 5432 is restricted to 10.0.0.0/24;
  the LAN rule for 8090 may still need adding.
- The Nitro node runs in Docker (`offchainlabs/nitro-node:v3.11.2`,
  container cool_allen, bridge network) and publishes 8540/8541 on 0.0.0.0.
  Docker-published ports bypass ufw. Optional later: publish on 127.0.0.1
  only, which needs a node restart.

## 4. Next work, in order (owner's decision)

1. **ML model** (ml-coder agent). Run by the owner on the prod server via
   `ml/RUNBOOK.md` ("Quick way"). No GPU needed: CPU only. The long (30d)
   horizon is skipped until it has 2,000 matured rows over 120 days, about
   25 December 2026; the owner chose on 8 October to wait rather than
   loosen the threshold. The gates are strict; scores stay out of
   deliveries until a report passes.
   - **First training on prod, 8 October:** model 20261008-1852, all four
     buckets FAIL. 4,709 first calls after exclusions. Train time 6 s.
     Python 3.11 was installed alongside the system 3.10 (Ubuntu 22.04);
     the venv was created with `python3.11 -m venv ~/scout-ml/venv`. Never
     replace the system python3. Reports copied to the PC go in
     `ml-results/` (gitignored).
   - **Results:** the collapse models are strong (test ROC AUC 0.80–0.87;
     skipping the top 30% avoids about 50% of collapses). Runner lift
     fails: short 1.16, 3day 1.83 (CI about 0.9–3.2), medium 0.52, long
     not meaningful. Buying all calls loses −21% (1d), −27% (3d), −44%
     (7d).
   - **ML fixes done (ml-coder, 8 October; on the branch, not yet
     merged):**
     - drop `secs_since_prev_call`/`prior_calls` (always NULL/0 for first
       calls);
     - categorical levels from train rows only, case-insensitive (old
       saved models with flat meta keep exact matching, so the model on
       the server scores as trained until retrained);
     - a real `long` skip;
     - for medium/long, a 14-day date-based forward test plus purged
       k-fold (K=5, at least 3 usable folds, else a fixed 150 rounds)
       inside train for the rounds and Platt calibration;
     - Wilson lift intervals in the report;
     - the `run_training.sh` guard now checks `MIN_MATURED` in
       `config.py`.
     - The gates are unchanged.
     - Tested locally: 74 pytest passed.
     - Next: the owner merges, then retrains on prod with
       `REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh
       --skip-install` and copies `report.md` to `ml-results/`.
   - **Queued ML items:**
     - Group DEX names and quote assets into stable families. There are
       about 50 posted DEX names, many short-lived and time-specific: e.g.
       "Pons" until mid-Aug then "Pons V2", "Pools Trade Instant" in Aug
       only, "O1 Rwa" for one week.
     - Calibrate on recent weeks or weight recent calls (the base rate
       drifts week to week).
     - A "dead after the call" label/flag (e.g. fewer than about 50
       trades in the 24 h after the call), so flat dead tokens are not
       counted as safe non-collapses.
     - Optional: refit on all data before saving the model.
     - Optional: case-insensitive `pre_vol_unit` / `price_unit`.
   - **Query findings (the owner ran them 8 October):**
     - Pons V2 is a steady pump and dump: the 1d collapse rate is 0.58–0.83
       every week; the median peak is +50–90%, then −75 to −90%.
     - The Uniswap v4 collapse rate fell from 0.57–0.75 (late Aug to mid
       Sep) to 0.16–0.26 (from 21 Sep).
     - Backfilled and live calls agree in the same week (no provenance
       artefact).
     - Collapses are rarely total losses (0–9%).
   - **"Uniswap V4" flat calls, resolved 8 October: real dead tokens, not
     a tracker bug; no fix and no re-track.**
     - About 230 first calls posted "DEX: Uniswap V4", quoted in ETH, in
       the weeks of 21 and 28 Sep (to about 2 Oct); e.g. calls 664, 578,
       504.
     - The owner's read-only query showed the tracker follows the active
       pool: 100–250 buy-heavy swaps in the 60 minutes before the call,
       then only 0–30 trades in the next 24 h; flat price; latest return
       about −10% (ETH drift). No zero-amount (hook) swaps.
     - The implied mcap is about 10x below the posted mcap (likely a 10B
       supply).
     - A second pattern, 2–5 Oct: many tokens peak at about 9.0–9.3e-8 ETH
       with lows of about 4.5–5e-8 ETH; likely templated launches with a
       fixed liquidity range.
     - ML impact: the low v4 collapse rate from 21 Sep is partly dead
       tokens counted as non-collapses, hence the queued "dead after the
       call" item.
2. **Perceptor re-scan of first calls without a Perceptor report**
   (approved 8 October with the defaults below). **Backend merged
   (0a46f28, in PR #1 = 4e7e049) and deployed 8 October; the lane is on
   in prod since 8 October (`SCOUT_RESCAN=on`).** The website "Perceptor
   today" label is merged (PR #2/#3) and deployed.
   - What it does, backend only:
     - `scan_kind` column on `scout_investigations` (`'live'`/`'rescan'`,
       idempotent `ADD COLUMN IF NOT EXISTS`, default `'live'`) with a
       partial index;
     - every existing reader uses `scan_kind = 'live'`: the dataset view
       (no leak of today's verdict into ML features), the requeue checks,
       the website verdict/filter/counts and `/api/call`, and sAlpha;
     - a rescan lane inside the listener (`rescan.go`; one Telegram session,
       never a second process; Perceptor only, no delivery, no
       status/seen/score changes), off unless `SCOUT_RESCAN=on`;
     - `-rescan-missing -dry-run` lists and counts the candidates;
     - website snapshot fields `perceptor_today_verdict`,
       `perceptor_today_url` (https only) and `perceptor_today_at`;
     - NOTIFY payloads carry `scan_kind`;
     - a stop while reading the report stores nothing;
     - an in-memory floor keeps the 10m gap when inserts fail.
   - Files (15): `DEPLOY.md`, `README.md`, `events.go`, `events_db_test.go`,
     `events_test.go`, `main.go`, `scout_models.data.go`, `scout_models.go`,
     `scoutanalytics.sql`, `tools.go`, `web_db_test.go`, `websnapshot.go`,
     and new `rescan.go`, `rescan_db_test.go`, `rescan_test.go`. The README
     includes the "Reading the list" wording fix (STALE_OLD_MS).
   - Tests (local): `go vet` clean; `go test -race` ok; the
     throwaway-Postgres run gave 286 PASS / 0 SKIP, including the 8
     `rescan_db_test.go` tests. Web benchmark roughly unchanged (HTTP paths
     ≤ 0.21 ms/op).
   - Shipped: committed on `work/2026-10-09-perceptor-rescan` (stacked on
     `work/2026-10-08-ml-pool-web`), merged as PR #1 (4e7e049), deployed
     8 October with the 8 October work.
   - Prod notes:
     - Done: the "Perceptor today" label was deployed, and
       `SCOUT_RESCAN=on` is set in prod since 8 October.
     - The candidate count: a read-only pgAdmin query (the PM provides
       it).
     - Send the dry run's output to a file (it may be thousands of lines).
     - Rollback caveat: an old binary would read rescan rows as call-time
       verdicts (in DEPLOY.md).
   - Open:
     - **Decided 8 October:** drop a call after 2 failed rescans (as
       `rescanFailedLimit = 2`; no code change).
     - The daily cap can undercount while database inserts fail (the gap
       still holds).
     - A "Perceptor today" change sends no SSE event in 4e7e049; the label
       (merged in PR #2/#3) adds one (`report` with `tool: perceptor_today`).
   - Done on this line: the frontend "Perceptor today" label, merged
     (PR #2/#3) and deployed; the lane is on in prod since 8 October.
   - Off by default: `SCOUT_RESCAN=on`. Settings and defaults:
     `SCOUT_RESCAN_MAX_AGE=720h`, `SCOUT_RESCAN_MAX_PER_DAY=100`,
     `SCOUT_RESCAN_GAP=10m`, `SCOUT_RESCAN_IDLE=5m`,
     `SCOUT_RESCAN_STATUSES=backfill,duplicate,failed,scanned`.
   - Filters: first calls only; no completed Perceptor report of any kind;
     not rugged; `latest_return_pct` NULL or > −99; at most 2 failed
     rescans. A live call can wait up to about 5 min (shared account pacing
     2m5s).
   - The schema change runs at the next start of any mode; it does not make
     the tracker redo history.
   - The existing `-scan`/`-post` must not be used for this: they deliver,
     overwrite status, and would leak today's verdict into the ML features.
3. **Fresher latest price for old calls** (approved 8 October, config only,
   deployed 8 October in 4e7e049, now in the prod `.env`; see "Deploying"):
   `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` on the tracker. No
   history re-run; ≈ 133k `eth_getLogs`/day in total (≈ 1.5 req/s),
   +108k/day (≈ +1.25 req/s) over today's ≈ 25k; old calls' latest price at
   most about 1 h old instead of about 24 h. The website's stale threshold
   for old and rugged calls (`STALE_OLD_MS` in `frontend/app.js`) is now 2 h
   (was 48 h). Later option: batched multi-pool `eth_getLogs` for 15-minute
   freshness for all calls. Refresh on view is rejected for now (30–90 s
   latency, the web process would have to write a NOTIFY, no login).
4. **Early-transaction pattern analysis** (owner request 8 October;
   planning only, nothing to build yet). For each Scout first call, look at
   the token's early on-chain transactions, from pool creation (or Pons
   curve launch) up to the call and shortly after, and find:
   - (a) whether certain addresses (deployers, funders, early buyers/sellers,
     LP providers/removers, wallets that recur across tokens) usually
     precede a rug;
   - (b) whether there is a distribution pattern (holder concentration,
     early buy/sell mix, wallet overlap) for calls that did well: over
     +100% at 7d or later.

   Process: the PM plans it with the researcher first (data available:
   `scout_call_precall`, `scout_call_candles`, pool/rug data, live buys in
   `scout_call_live_buys`; what more would need reading from the node; node
   cost; leakage rules for any ML use: only pre-call data as features),
   agrees the plan with the owner, and only then hands anything to a coder.
   No coder work until the owner approves the plan.

   **Plan drafted by the researcher (8 October, later); waiting for the
   owner.**
   - Data: the existing tables give labels and sizing only. No table has
     per-trade wallets, holder balances, deployers or LP actions, and
     `scout_call_live_buys` stores wallets truncated (prefix/suffix).
   - Labels: the `rugged` flag is censored (it grows with call age; the
     price rule sets no `rug_block`; a drain found after 30d gets the check
     block). Proposed primary label: rugged by 7d (`rugged` and `ret_7d` ≤
     −99.99, not rugged at the call).
   - Phases:
     - P0: the owner runs read-only queries Q1–Q5. Q5 filters on
       `scan_kind`; the column is in prod since 4e7e049.
     - P1: offline pilot of about 450 calls (150 rug-by-7d / 150 +100% / 150
       neither); a one-off, rate-limited, read-only exporter; files on the
       server; no schema change; about 8k `eth_getLogs`, under 1 h.
     - P2: full run, about 33k–80k `eth_getLogs` (about 0.6 day of tracker
       log load), after the post-deploy latest-price catch-up.
     - P3: only if a pre-call signal passes the ML time-split gate: `pre_*`
       columns on `scout_call_precall` (not a new table).
   - Leakage: only chain data at or before `entry_block` as features.
     Wallet reputation is point-in-time with a horizon + embargo, never
     computed over the whole history.
   - Owner questions:
     - the window (call + 1 h or + 24 h);
     - the +100% definition (horizon return or peak; gross or net of tax);
     - Blockscout API for funders (an outside service, needs a key);
     - the pilot size and the server folder;
     - the look-back cap for old tokens (7 days?).
   - No coder work until the owner approves.
5. **Analytics page**: two sections on a separate page (`/analytics.html`,
   linked from the list's top bar and back).
   - **Section 1, "Call performance": merged (PR #5) and deployed.**
     - `GET /api/analytics`: one compact row per first call, built once per
       snapshot (pre-encoded, pre-gzipped, ETag = hash of the body, 304);
       the browser groups and filters it. No new table or schema change; the
       load query gained the posted DEX, `entry_price_source`, the quote
       asset, a per-window `no_data` bit, the Perceptor verdict at the time of
       the call (the dataset view's rule, live scans only) and a "first 24 h
       of candles stored" flag. Trades in the first 24 h come from
       `scout_call_candles` (5-minute candles), read once and cached; only
       newly complete calls are read per refresh, all of them hourly. A
       failed trade-count read no longer fails the refresh: the counts held
       stay, the others show as unknown, and the next refresh asks again.
     - Page: window 1h–30d (1d default); "Quiet after the call" (< 50 trades
       in 24 h, not rugged) include / exclude / only; tables overall, by
       verdict at the call, pool family (v2/v3/v4/pons/GeckoTerminal), posted
       DEX (top 12 + other), quote asset, week (Mon–Sun UTC) or month, and
       week × verdict medians. Calls, with data, mean, median, win rate,
       ≥ +100% peak, collapse, median peak / drop, rugged; n under every rate;
       groups under 20 calls greyed. First calls, late entry, USD, before
       tax. Rugged calls keep their pre-rug peak here (the list hides it).
     - Defaults the PM chose (owner may change): weeks Mon–Sun UTC, returns
       before tax, 1d default.
   - **Chart batch: built 8 October, uncommitted on
     `work/2026-10-09-analytics-charts`** (coder; reviewed; awaiting
     commit/PR and deploy). Three tabs over the same feed (`/api/analytics`
     format 2, which adds per first call the values known at the call:
     called-at market cap, holders, elite/good holders, elite/good live buys
     count and USD, and the hour before the call: buy/sell volume in USD,
     swaps, price change; same expressions as `scout_call_dataset_v`; no
     schema change).
     - Overview: weekly (or monthly) trend, outcome mix, "$100 on every
       call" (returns capped at +1,000%), hour × weekday heat map, and the
       section-1 tables, now with mean and median of return, peak and drop.
     - By factor: equal-count groups (4/5/10, zero group when ≥ 10% are 0),
       per-group histograms with fixed bands, table, scatter (log / symlog),
       typical path 1h → 30d, verdict × pool family grid.
     - Peak vs final: peak against return, share that gave back more than
       half of the peak.
     - Shared filters: window, metric, mean/median, quiet calls, week/month,
       pool family, verdict. Charts are hand-built SVG (the PM's task said
       no chart library), not ECharts as `coder.md` planned.
     - Review follow-ups (9 October): number factors with a zero group now
       cut the values below and above 0 separately (no group spans 0;
       matters for the pre-call price change); "$100 on every call":
       "All calls" and "No red flags" ignore the pool family and verdict
       choices (quiet choice only), plus one line for the chosen family /
       verdict; label and caption fixes.
     - Market cap factor: the owner chose "market cap at call from the
       price" (entry price × supply), but the token supply is not stored
       anywhere, so the page still sends the posted `called_at_mcap_usd`,
       labelled "Market cap at call (posted)". A price-based one needs a
       `totalSupply()` read per token (about 4.7k node calls once, then one
       per new call), stored with the call (optional `onchain` JSON field
       or a new `scout_call_tracking` column), a backfill pass, and the
       website load query; owner to decide (details in the coder's report
       of 9 October).
     - Needs its own PR from `work/2026-10-09-analytics-charts`: `main`
       already has PR #6 (merge 0122a98), which merged only the skill
       commit (c9e9fcc), not this batch.
   - **Section 2, "Model insights": next** (now only an empty "coming soon"
     heading). Plan: show the model report from the training run's
     `meta.json`, read from a folder set by a new `SCOUT_ML_DIR` setting;
     per-call scores later, and only out-of-sample ones.
6. **Paused: multi-source calls** (other Telegram call channels next to
   Scout). Nothing to build until the open questions are answered.
   - Decisions:
     - all channels in the website list and in the model, with scout-only
       comparison runs;
     - other channels' returns from our own node's stored candles;
     - past calls are stored; whether to track tokens without history is
       decided after a sizing query;
     - Perceptor + sAlpha for all channels; a report is reused unless it
       failed, is missing or unreadable; Scout calls first in the queue;
     - deliveries to the same group, labelled by channel, with
       clean,caution.
   - Open questions:
     - the age limit for reusing a report;
     - the channels: @CallAnalyserRobinhood recommended, @hoodchains as a
       possible baseline;
     - Call Analyser delivery: first post per token, or every caller.
   - Phase plan: 0 pin the readers to Scout; 1 Robinhood sources; 2 store
     Base; 3 track Base.

Later:

- ML label issues: the 30d collapse uses the `rugged` set after day 30;
  train/serve skew in `quote_asset` and `perceptor_verdict`.
- GCP Pub/Sub for live updates when scaling (owner's plan).
- Resolved by 2b: `MaxConns` for `SCOUT_DATABASE_URL` was hard-coded to 4.
- Website summary strip ("now" medians and shares); it changes the summary
  `ETag`.
- `-scan`/`-post` attach to the newest post of the token, not its first call.
- FLOOD_WAIT is not handled on bot sends.
- `internal/database/database.go` discards the `pgx5.Connect` /
  `NewWithConfig` errors, keeps 2 connections outside the pool per process,
  and fatally requires `.env` in the working directory.
- Scatter plot with ECharts 6.1 after the model report (rules in `coder.md`,
  "Charts").
- Other launchpads (about 46 unpriced calls); the launchpad query results are
  pending from the owner.
- Other call sources (Call Analyser channels): paused, section 4 item 6.
- In-flight scans cut off by a stop end as `failed` and are not requeued.
- Poison post: a stored-only post with a permanent DB error pauses polling.
- Resolved: `prior_calls`, `calls_prev_1h` and `calls_prev_24h` counting update
  posts no longer matters (training uses first calls only, so they are
  near-constant).

## 5. Repo migration (done; pushed and cut over on 8 October)

**Moved from `kfukue/geth-analytics` (history kept).**

- Done on 7 October: `git filter-repo --subdirectory-filter
  telegrambot/scoutanalytics --refs main` on a fresh clone of
  `https://github.com/kfukue/geth-analytics.git`, from `origin/main` at 4369a1d
  (same tree as `origin/scout-call-model` for this folder). 37 commits touch
  the folder (55 with merges). No secrets anywhere in the extracted history
  (no `.env`, `*session.json`, `calls.csv`, `scoutanalytics_data/` or `.exe`
  object). Only `main` was carried over; the old stash (`stash@{0}`, "On
  codex/scout-dashboard: scout-dashboard before main sync 2026-10-06") stayed
  behind in the old repo (owner's decision).
- Then one standalone commit, no functional changes: module
  `github.com/kfukue/scout-analytics` (`go.mod`/`go.sum` from the old repo,
  `go mod tidy`, same versions for every module still required);
  `internal/database/database.go` copied unchanged from the old
  `database/database.go` (same `.env` variables; since changed, see 2b); new `.gitignore`; `.claude/`
  copied with paths rewritten; docs and comments to `go run .` from the repo
  root.
- Old → new hashes (filter-repo rewrote every commit): 4369a1d → 7a4edaa
  (PR #20 merge), d49fe9c → 34d70c4, 21139e1 → 0795e9e (PR #19 merge),
  08b3a71 → b10251b. Older hashes in this file and in the ops READMEs are
  old-repo hashes; `git log --grep` or the subject line finds them here.

- Done on 8 October (owner confirmed): `main` pushed to
  `kfukue/scout-analytics`, and the prod cut-over below completed; prod runs
  from the new checkout. Prod `git rev-parse HEAD` = **4e7e049**, run on
  the server (owner confirmed 8 October).
- Rollback: the old checkout (old repo at 4369a1d) is kept; step 7 below.

**Prod cut-over checklist** (done 8 October; kept for reference):

1. On the server, clone the new repo next to the old checkout, e.g.
   `git clone https://github.com/kfukue/scout-analytics.git /srv/scout-analytics`
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
   `WorkingDirectory=/srv/scout-analytics` (EDIT).
9. Removing `telegrambot/scoutanalytics` from the old repo is a separate,
   later decision; nothing in the old repo was changed.

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
- Headless browser checks: use a throwaway `--user-data-dir` in the session
  scratchpad and start Edge/Chrome with `--disable-sync --disable-extensions
  --no-first-run --no-default-browser-check` (plus Edge `--inprivate` if it
  works headless), so the test profile is never signed into the owner's
  Microsoft account and never loads his extensions (on 9 October a test
  profile synced his extensions, a wallet among them). Close the browser by
  PID and delete the profile directory afterwards.
- Ops SQL: run A/B/C/D against the throwaway Postgres before handing them over.

## Deploying

See `DEPLOY.md` (committed in 08b3a71) for the full steps. In short:
the owner commits and pushes a work branch from the PC, merges it into
`main` with a PR, and deploys `main` on the server (in the new checkout): `git checkout main && git pull`, then restarts the processes
that changed, from the repo root:

- `go run . -listen-only`
- `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run . -track`
  (latest-price pass on; `SCOUT_RPC_LOG_CHUNK` unset, default 200000).
  `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are in the prod
  `.env` (since 8 October). Start it from the checkout folder, so it reads
  that `.env`; a variable set in the shell wins over the file. Check the
  `latest prices on:` line after every restart.
- `go run . -web`. A reverse proxy in front must
  pass the `Host` header unchanged, or the same-origin check makes
  `POST /api/refresh` return 403.

After a tracker change: `-price-check` on 2–3 affected tokens before the full
run.

Checks after deploying `work/2026-10-08-ml-pool-web` (deployed 8 October in
4e7e049; the pool, feeds and latest-prices startup lines were seen; the
`waiting` count, listener and phone checks are still to confirm):

- Tracker startup line: "pool of at most 16 connections (default for 12
  tracker worker(s))".
- Expect many "stale" labels for the first hour or two while about 4,000 old
  calls are refreshed at 400 per cycle; the tracker's `latest prices:` line
  "N waiting" should fall and then stay low.
- The owner checks the new scrolling table on a phone, and that a non-rugged
  call older than 30 min shows "stale" when its price is old.

## Agent setup

- `.claude/agents/` (committed in bde6d81 in the old repo; copied here in the
  standalone commit with paths for this layout): `coder` (Go + plain JS website,
  with the Go, JS, CSP and Charts rules), `ml-coder`, `react-coder`, `infra`
  (approval-gated), `reviewer`, `researcher`, `product-manager`. All 7 are
  active.
- The reviewer runs before every commit.
- At most 10 coding agents in parallel (owner's limit, raised 9 Oct 2026).
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
  - On this PC git needs `safe.directory` (the owner added it globally for
    this repo); agents use `git -c safe.directory=* …`.
  - `git add -p` instructions must say exactly which hunks to take (the owner
    answered y to all, so every README hunk landed in 4707e77); give
    file-level splits when possible.
  - In Git Bash `grep -c $'\r'` reports 0 even for CRLF files; count CRs with
    `tr -cd '\r' | wc -c`.
  - The owner merged the stacked branch directly (PR #1). A PR from a
    stacked branch includes the base branch's commits.

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
