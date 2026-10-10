# Testing scout analytics

The single guide to testing this repo, for people and for the agents
(`coder`, `ml-coder`, `tester`, `reviewer`). The agent files keep only the
essentials and point here.

## Layers

- **Unit (Go):** `*_test.go` files that need no database. They always run with
  `go test`.
- **Integration / DB (Go):** `*_db_test.go` files. They drop and recreate the
  `scout_*` tables and run the real schema from `scoutanalytics.sql`
  (`Migrate`), so they need `SCOUT_TEST_DATABASE_URL` pointing at a
  **throwaway** Postgres whose database is named `scout_test`
  (`TestOpenScoutStoreSelection` needs that name); never at a database the
  owner uses. Run them with `-p 1`. Without the variable they are
  **skipped**, not failed, so always say which DB tests actually ran.
- **On-chain:** on-chain code is tested only against the fake chain in
  `onchain_test.go`. Tests never call the real nodes.
- **Telegram, Perceptor, sAlpha, Bot API:** never contacted from tests. Test
  what gets recorded and what would be sent, through DB state and fakes.
- **Website:**
  - `node --check <file>` on every changed file in `frontend/`;
  - `node testdata/analytics-stats.test.js` tests `frontend/analytics-stats.js`
    (`TestAnalyticsStatsNode` also runs it from `go test` when `node` is on the
    PATH);
  - when web snapshot or API code changes, run the benchmark before and after
    the change and report both:
    `go test -run xxx -bench BenchmarkWebSnapshot -benchmem .`
    (p95 must stay under 10 ms). The coder runs the "before" number on the
    untouched base, before editing, and puts it in the report; the tester runs
    the changed code and compares it with the coder's "before" number. If the
    coder did not report one, the tester asks the PM and never stashes, checks
    out or otherwise swaps in other code in the coder's worktree to measure
    it.
- **ML (`ml/`):** `python -m pytest tests -q` inside `ml/`, with the `ml/.venv`
  interpreter (`.venv/Scripts/python.exe -m pytest tests -q` from inside `ml/`
  on Windows). Synthetic data only (`make_synthetic.py`), never real exports.
  `ml/tests/test_pipeline.py` is the end-to-end test: synthetic data, then
  `train`, then the scoring service.

## Throwaway Postgres

The owner does not want a permanent local test database. Each agent starts its
own throwaway Postgres and deletes it afterwards.

### Preferred: the scripts

(Scripts added in a separate branch; until merged use the manual steps.)

- PowerShell: `scripts/testdb.ps1 start <dir> [port]`,
  `scripts/testdb.ps1 stop <dir>`,
  `scripts/testdb.ps1 run <dir> -- <command>`.
- bash: `scripts/testdb.sh` with the same subcommands.
- `start` picks a free port in 55000-55999 (or the one given), refuses 5432,
  creates the database `scout_test` and prints the DSN on its last line.
- `run` runs a command with `SCOUT_TEST_DATABASE_URL` set to that database
  (check the exact behaviour against the scripts once that branch merges).
- `<dir>` is a new subdirectory of your session scratchpad, never inside the
  repo.
- After `stop`, check that `<dir>` is gone (delete it if it is not) and say so
  in your report.

### Manual steps

- Run `initdb` in your own subdirectory of the session scratchpad. Listen on
  127.0.0.1 only, on a port no other agent uses (pick one in 55000-55999).
  Name the database `scout_test`.
- Set `SCOUT_TEST_DATABASE_URL` to it, and run the tests with `-p 1`.
- At the end, stop it (`pg_ctl stop`) and delete its directory. Confirm both in
  your report.
- Never touch the Windows Postgres service on port 5432.

Example (Git Bash or Linux; `D` and `PORT` are yours):

```bash
D="<scratchpad>/pg-<task>"; PORT=55123
initdb -D "$D/data" -U postgres -A trust -E UTF8
pg_ctl -D "$D/data" -l "$D/pg.log" -w \
  -o "-p $PORT -c listen_addresses=127.0.0.1" start
# on Linux, add -k "$D" inside -o if the default socket directory is not writable
createdb -h 127.0.0.1 -p "$PORT" -U postgres scout_test
export SCOUT_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:$PORT/scout_test?sslmode=disable"
# ... run the tests ...
pg_ctl -D "$D/data" -m fast -w stop
rm -rf "$D"
```

## CI

`.github/workflows/test.yml` (a draft on a separate branch, pending the owner's
approval; not on `main` yet) runs the Go unit and DB tests against a Postgres
service container, `node --check` on the frontend, and the ML tests when `ml/`
changes. It fails if any database test was skipped for a missing
`SCOUT_TEST_DATABASE_URL`, or if `TestOpenScoutStoreSelection` /
`TestScoutStore` did not pass.

## A complete local run

From the repo root. Put coverage files and logs in your scratchpad, not in the
repo.

PowerShell:

```powershell
$db = "<scratchpad>\testdb-<task>"
$env:SCOUT_TEST_DATABASE_URL = (& .\scripts\testdb.ps1 start $db | Select-Object -Last 1)
go build ./...
go vet ./...
go test -race -p 1 ./...
go test -p 1 -coverprofile "<scratchpad>\cover.out" .
go tool cover -func "<scratchpad>\cover.out"
node --check frontend\app.js        # each changed JS file
node testdata\analytics-stats.test.js
go test -run xxx -bench BenchmarkWebSnapshot -benchmem .
Push-Location ml; .\.venv\Scripts\python.exe -m pytest tests -q; Pop-Location
& .\scripts\testdb.ps1 stop $db
Remove-Item Env:SCOUT_TEST_DATABASE_URL
Test-Path $db                       # must print False
```

bash:

```bash
db="<scratchpad>/testdb-<task>"
export SCOUT_TEST_DATABASE_URL="$(scripts/testdb.sh start "$db" | tail -n 1)"
go build ./...
go vet ./...
go test -race -p 1 ./...
go test -p 1 -coverprofile "<scratchpad>/cover.out" .
go tool cover -func "<scratchpad>/cover.out"
node --check frontend/app.js        # each changed JS file
node testdata/analytics-stats.test.js
go test -run xxx -bench BenchmarkWebSnapshot -benchmem .
(cd ml && .venv/bin/python -m pytest tests -q)   # .venv/Scripts/python.exe on Windows
scripts/testdb.sh stop "$db"
unset SCOUT_TEST_DATABASE_URL
test ! -e "$db" && echo deleted
```

Until the scripts are merged, replace the `start`/`stop` lines with the manual
steps above. To list skipped tests, add `-v` and look for `--- SKIP`.

## Known flaky tests

- `TestLatestPriceInterruptedLeavesRowUntouched` was flaky (about 1 run in 10)
  before aa60a3b (7 October); it has not been seen failing since. If it fails,
  rerun it once
  (`go test -race -p 1 -run '^TestLatestPriceInterruptedLeavesRowUntouched$' -count=10 .`)
  and report the result.

## Line endings and gofmt

- The Go files are CRLF, so plain `gofmt -l` lists almost every file. Check
  gofmt on LF copies (e.g. `tr -d '\r' < x.go > <scratchpad>/lf/x.go`, then
  `gofmt -l <scratchpad>/lf`), and keep CRLF line endings in the files you
  edit.
- In Git Bash, `grep -c $'\r'` reports 0 even for CRLF files; count CRs with
  `tr -cd '\r' < file | wc -c` and compare with `wc -l < file`.

## Rules

- Every behaviour change gets a test.
- Never call the real nodes, Telegram (or the bots) or the production database
  from tests.
- Secrets never go in fixtures or test data: nothing from `.env`,
  `scout.session.json` or `scoutanalytics_data/`, no real tokens, invite links
  or DSNs, no real exported datasets.
- No sleeps for synchronisation where a channel or clock can be injected; fixed
  random seeds; no real network.
- Headless browser checks (only when the task asks for them): a throwaway
  `--user-data-dir` in the session scratchpad, and Edge/Chrome started with
  `--disable-sync --disable-extensions --no-first-run
  --no-default-browser-check` (plus Edge `--inprivate` if it works headless),
  so the test profile is never signed into the owner's Microsoft account and
  never loads his extensions (on 9 October a test profile synced his
  extensions, a wallet among them). Close the browser by PID and delete the
  profile directory afterwards.

## Reporting test results

- The exact commands you ran and their results as counts (passed, failed,
  skipped) per layer.
- Every skipped test with its reason. "DB tests skipped" means the DB layer was
  not tested; say so plainly.
- Coverage of the changed code when you measured it, and changed functions
  with no test.
- Flaky reruns and their fail rate.
- What was tested locally versus what needs the prod server or the owner's
  browser. Anything not run on the prod server is "tested locally", never
  "verified in prod".
- That the throwaway Postgres was stopped and its directory deleted.
