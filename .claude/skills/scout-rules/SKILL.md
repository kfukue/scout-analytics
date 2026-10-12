---
name: scout-rules
description: Shared rules for every scout analytics agent (secrets, git, databases, history, parallel work, reporting basics) and where things are in the code.
---

# Scout analytics: shared rules

You work on the scout analytics project (`kfukue/scout-analytics`). You report
to the product manager, not to the user. Testing guide: `TESTING.md` (repo
root).

## Hard rules

- Never read out, print, commit or copy `.env`, `scout.session.json` or anything
  under `scoutanalytics_data/`.
- Do not commit, push, or switch branches unless the task explicitly says to.
- Do not run the listener, `-backfill` or `-track` against a real database.
- Never reset, re-track or bulk-update calls in any database the owner uses.
  When existing rows need repair, give the exact read-only SQL to find them and
  the exact UPDATE in your report; the owner decides and runs it.
- Never change `onchainStateVersion` or anything else that makes the tracker
  redo history without the task explicitly saying so. Flag it instead.
- Don't edit `HANDOFF.md` unless the task is about it. Edit `README.md` only
  with targeted edits to the sections your change affects; never rewrite the
  whole file.
- If the task says another agent is working in parallel, stay within the files
  the task names. If `git status` shows unexpected changes in files you need,
  report it instead of editing over them.
- Processes and test DBs: stop only your own processes, by PID. Throwaway
  Postgres only, in your own scratchpad subdirectory on your own port (see
  `TESTING.md`); never the Windows Postgres service on port 5432.
- `.env`: the tracker reads `.env` from the folder it is started in, and a
  variable already set in the shell wins over the file.

## How you report (basics)

Your agent file adds its own report contract on top of these.

- When you changed files: list the files changed and why, the exact test
  commands you ran with their results, anything you could not test, and any
  follow-up the product manager should know about (for example a change that
  makes the tracker redo history).
- Say what was tested locally versus what needs the prod server or the owner's
  browser. The production database and the nodes are only reachable from the
  prod server: anything not run there is "tested locally", never "verified in
  prod".
- Include any README text you were told not to write yourself.
- Never include secrets or the contents of `.env` or `scout.session.json`.

## Where things are (on `main`)

Go, package `main` at the repo root (`*_test.go` next to each file):

- `main.go`: config, command-line flags (`-listen-only`, `-track`, `-web`,
  `-backfill`, `-export-dataset`, `-rescan-missing`, `-price-check`, …), the
  listener, the scan queue and delivery to the private group (Telegram and Bot
  API sends, `scout_deliveries`).
- `callmeta.go`: parses a call post; `postkind.go`: call vs "hit 3X" update
  post; `catchup.go`: catch-up and requeue after a restart.
- `tools.go`: the investigation bots (Perceptor, sAlpha; `SCOUT_TOOLS`) and
  reading their replies; `verdict.go`: Perceptor verdicts and report pages.
- `rescan.go`: the Perceptor re-scan lane (`scan_kind = 'rescan'`,
  `SCOUT_RESCAN`).
- `tracker.go`, `tracker_onchain.go`, `tracker_latest.go`, `tracker_retry.go`:
  the price tracker (horizons, latest-price pass, `-retry-no-pool`);
  `onchain.go`, `onchain_extra.go`, `onchain_pons.go`: on-chain prices (Uniswap
  v2/v3/v4, Pons V2 curves, USD sources, token supply); `logcache.go`:
  `eth_getLogs` cache; `feeds_db.go`: Chainlink feeds from the asset database;
  `prices.go`: GeckoTerminal price source (the non-on-chain option).
- `scout_models.go`, `scout_models.data.go`: models and storage (pgx), including
  the dataset export (`ExportDatasetCSV`); `scoutanalytics.sql`: schema and
  views (incl. `scout_call_dataset_v`, the ML contract), run at every start;
  `internal/database/`: connection setup.
- `score.go`: client for the scoring service (`ml/serve.py`).
- `web.go` (HTTP API, CSP, static files), `websnapshot.go` (in-memory snapshot),
  `webanalytics.go` (`GET /api/analytics`), `events.go` (SSE live updates via
  LISTEN/NOTIFY): the read-only website.

Other folders and files:

- `frontend/`: the plain-JavaScript site (`index.html`, `app.js`,
  `analytics.html`, `analytics.js`, `analytics-stats.js`, `style.css`).
- `ml/`: the Python model package (`scout_ml/`, `train.py`, `serve.py`,
  `run_training.sh`, `README.md`, `RUNBOOK.md`, `tests/`).
- `ops/`: one-off ops SQL folders and their status (`ops/README.md`).
- `DEPLOY.md`, `HANDOFF.md`, `README.md`, `TESTING.md`.

Work branches may add more (e.g. a Perceptor re-check file, sAlpha re-ask,
`deploy/gcp/`); confirm a file exists with Glob before citing it.
