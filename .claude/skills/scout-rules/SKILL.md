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
  When existing rows need repair, write the read-only SQL that finds them and
  the change itself as an `ops/` folder with its rollback (see "Database
  changes: run script and rollback" below); the owner decides and runs it.
- Every database change ships with a script to run AND a rollback script
  (owner, 13 Oct 2026). See "Database changes: run script and rollback" below.
- Never change `onchainStateVersion` or anything else that makes the tracker
  redo history without the task explicitly saying so. Flag it instead.
- Don't edit `HANDOFF.md` unless the task is about it. Edit `README.md` only
  with targeted edits to the sections your change affects; never rewrite the
  whole file.
- If the task says another agent is working in parallel, stay within the files
  the task names. If `git status` shows unexpected changes in files you need,
  report it instead of editing over them.
- File access (owner, 14 Oct 2026): read and write files only inside the repo
  folder (`<repo root>`, the main checkout, its worktrees included) and the
  owner's personal notes folder. Its path comes in your
  brief when you need it; it is not in this repo and you do not look for it
  elsewhere. Never run git in the notes folder; the owner commits it.
  - Never use the session scratchpad under `AppData\Local\Temp` or any other
    folder.
  - Scratch files (throwaway Postgres data, coverage files, logs, LF copies
    for gofmt, browser profiles) go in your own subdirectory of the MAIN
    checkout's `_scratch` folder, always as an absolute path:
    `<repo root>/.claude/worktrees/_scratch/<task>`, where `<repo root>` is
    the parent folder of `git rev-parse --path-format=absolute
    --git-common-dir` (the same from any worktree). Never use the relative
    `.claude/worktrees/_scratch` from inside a worktree. It is git-ignored.
    Delete your subdirectory when you are done.
  - Public repo (owner, 14 Oct 2026): never write absolute local paths
    (drive letters, user folders) into repo files; write `<repo root>`
    instead. The same for the prod server: never write where its checkout
    lives; write `$REPO` (the server checkout) and `$ML` (its ML folder,
    the documented default `~/scout-ml`). PR texts are public too.
  - Installed tools (Go, Postgres binaries, git, `gh`, the repo's
    `ml/.venv`) may still be run. Files a tool makes for itself (the Go build
    and module caches, `t.TempDir()` and pytest `tmp_path` folders) are the
    tool's; you never read or write them directly.
  - Do not inspect system settings, event logs or other programs' data. If a
    write is blocked (e.g. Windows Defender's Controlled Folder Access, which
    protects `Documents`), stop and report it.
- Processes and test DBs: stop only your own processes, by PID. Throwaway
  Postgres only, in your own `_scratch` subdirectory on your own port (see
  `TESTING.md`); never the Windows Postgres service on port 5432.
- `.env`: the tracker reads `.env` from the folder it is started in, and a
  variable already set in the shell wins over the file.
- Small changes, small functions (owner, 13 Oct 2026): keep each change to
  one function of the system so its PR is easy to review; if a task turns
  out to span several (e.g. storage, API, page), say so and propose the
  split instead of growing one large diff. Write new code as small,
  single-purpose, testable functions; split a function that grows long
  rather than adding to it.

## Database changes: run script and rollback

Owner rule (13 Oct 2026): every change to the database, its structure or its
data, comes with a script to run and a rollback script, in case the script
fails or its result is wrong. Agents write the scripts as files in the repo,
not only as text in a report or PR body. Only the product manager may give
the owner a small one-off write in a message, and only with its backup step
and its rollback in the same message. This rule is for new work. Do not add
rollbacks to the old `ops/` folders that are already DONE, or to the writes
already in `scoutanalytics.sql` (the UPDATEs run at start and the seed
INSERTs). `DEPLOY.md` 3.4 ("rollback is forward-only") and 3.5 ("a
committed B is not undone") were written before this rule. For new changes,
R supersedes them.

Data changes (UPDATE, INSERT or DELETE the owner runs in pgAdmin; `ops/`
folder, A = read-only count, B = the write, C = check, D/E = follow-ups,
R = rollback):

- B runs as one transaction (`BEGIN; ... COMMIT;`). If B stops with an
  error, the owner runs `ROLLBACK;` and nothing has changed. R is for undoing
  a B that finished.
- Before changing anything, B saves what it is about to change, inside the
  same transaction, in a backup table for each table it touches:
  `CREATE TABLE <table>_bak_<topic>_<YYYYMMDD> AS SELECT ... FROM <table> WHERE ...`
  (the folder's topic in the name, so two folders on one day never clash).
  Keep the table's primary key in it (all its columns, e.g. `call_id`, or
  `call_id, horizon`). B then changes exactly the rows in the backup table,
  joining on that key.
  - UPDATE: the backup holds the key and the old values of the changed
    columns; R sets them back.
  - DELETE: the backup holds the whole rows (`SELECT *`); R inserts them
    again. Do not delete from a table that other tables point to (such as
    `scout_calls`) without asking the product manager first. If such a
    delete is needed, the backup covers every table that points to it:
    - `ON DELETE CASCADE` children, whose rows are deleted too (for
      `scout_calls`: tracking, returns, candles, live buys, predictions and
      more). Back up their whole rows.
    - `ON DELETE SET NULL` children, whose link is cleared (for
      `scout_calls`: `scout_investigations`, `scout_deliveries`). Back up
      their key and the old link.

    R puts the parent rows back first, then the child rows, then the links.
  - INSERT: the keys exist only after the insert. B creates the backup
    table empty first, then runs
    `WITH ins AS (INSERT ... RETURNING <key>) INSERT INTO <backup> SELECT * FROM ins`.
    R deletes those rows.
- The backup table is a real table, not `CREATE TEMP TABLE`: a TEMP table
  is lost when the pgAdmin tab closes. It is not a schema table: it is never
  in `scoutanalytics.sql`, the program never reads it, and the owner drops it
  afterwards. So the "no new tables" rule does not apply to it.
- R is `R_rollback.sql` in the same folder: one transaction that restores
  every table B touched from its backup table. It never works out the rows
  again from B's WHERE clause, because that would also hit rows the program
  wrote after B ran. The folder `README.md` says how long R is safe to run
  (for example "before the tracker is restarted", when the tracker rewrites
  those rows).
- The folder `README.md` ends with the `DROP TABLE` for the backup tables.
  The owner runs it in either case: once he is happy with B, or after R.
- Same pgAdmin rules as every other script: no psql meta-commands, and
  `<<< EDIT` on each line with a value the owner must change. C shows the
  state after B and after R; the README says what to expect in each case.
  The folder's row in `ops/README.md` says, in its "What it does" column,
  that R exists.

Structure changes (`scoutanalytics.sql`, which the program runs at every
start):

- The run script is the new statements in `scoutanalytics.sql`. Write them so
  they are safe to repeat (`IF NOT EXISTS`) and quote them in your report.
  New data writes (UPDATE, INSERT or DELETE) go in an `ops/` folder, not in
  `scoutanalytics.sql`. If one really must run at every start, ask the
  product manager first.
- Also write a folder `ops/<YYYY-MM-DD>-<topic>/` with `R_rollback.sql`, a
  `C_check.sql` and a `README.md`. C is read-only and shows whether the
  change is there (it queries `information_schema.tables`,
  `information_schema.columns`, `pg_indexes` and `pg_views` for what the
  change added). The folder's row in `ops/README.md` says, in its "Status"
  column, "rollback only, for branch `work/...`; not run". The README gives
  the rollback order:
  0. Only if the change adds a view built on a view that already exists:
     `DROP VIEW IF EXISTS <new view>;` after the services are stopped and
     before the previous commit starts in step 1. Otherwise the previous
     commit's `DROP VIEW` of the existing view fails at start.
  1. Restart the services on the previous commit, as in the rollback part
     of `DEPLOY.md` section 3.0: `go run` on the previous commit, or the
     previous binary. `scoutanalytics.sql` is built into the binary, so
     the binary that starts decides which schema runs, and `git pull`
     alone changes nothing. The old `scoutanalytics.sql` then runs at
     start, with `SCOUT_DB_AUTO_MIGRATE` on (the default). When it is
     off, the owner runs the old file by hand first.
  2. Then run R.

  If R ran first, the code still running would fail on the missing column
  or table, and a restart on the new commit would put the change back.
- R is safe to repeat too, and removes with `IF EXISTS` everything the
  change added: columns, indexes, tables and views (`DROP COLUMN IF EXISTS`,
  `DROP INDEX IF EXISTS`, `DROP TABLE IF EXISTS`, `DROP VIEW IF EXISTS`). New
  views go first, and a view built on another new view goes before that
  view. It never drops or recreates a view that exists on the previous
  commit, and never uses `CASCADE`. After step 1 the old views no
  longer use the new column. If `DROP COLUMN` fails with "view ... depends
  on column", either R missed a view the change added, or step 1 has not
  happened yet. R says plainly what data is lost, for example "the values in
  column X are deleted".

Test both directions on your throwaway Postgres and report the commands and
results:

- Data change: B, then C, then R, then C, then B again.
- Structure change: apply the new `scoutanalytics.sql`, then C, then apply
  the previous commit's file (`git show <base>:scoutanalytics.sql`, as step 1
  does), then R, then C, then apply the new file again.

The first new folder that has an R also updates the `ops/README.md` header
and legend in a targeted edit: add R to the legend, and make "never run
again" cover B only.

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
  Never write the owner's personal notes path into any repo file or into a
  report the PM might paste into one.
- Dates and times (owner, 14 Oct 2026). In reports, docs, PR text and commit
  messages, use the owner's local time (US Pacific: PDT in summer, PST in
  winter) first, with UTC in parentheses:
  - a time: "13 Oct 2026, 23:15 PDT (14 Oct 06:15 UTC)";
  - a date alone: "13 Oct (14 Oct UTC)" when the UTC date differs, otherwise
    just "13 Oct".

  Some formats keep their own convention:
  - Branch names use the owner's local date.
  - Model versions stay UTC stamps (e.g. `20261014-0559` = 13 Oct 22:59 PDT).
  - Log lines are quoted as printed.

  Older text is not rewritten.

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
