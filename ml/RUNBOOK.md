# Runbook: first model training on the prod server

For the owner, on the prod server (Linux, bash). It exports the dataset,
trains the four bucket models and produces `report.md`. The commands are
exact; replace only the two paths in step 0.

What it does NOT do: it does not start the scoring service and does not wire
scores into deliveries. `SCOUT_MODEL_URL` stays unset until the product
manager has read the report and said the gates passed. Scores never filter
deliveries in any case.

## Quick way: one script

`ml/run_training.sh` runs steps 1, 2, 4, 5 and 6 below in one go, with the
same commands and flags, plus the step 9 check (`SCOUT_MODEL_URL` in `.env`:
it warns, without showing the value, and goes on). It stops at the first
failure and says which step failed; when a command inside a step failed, it
also shows the last 20 lines of that command's output to paste. It always
deletes `$ML/data/calls.csv` when it exits, also on an error or Ctrl+C. It
does not run the step 3 queries and does not start `serve.py`. After the
export it reads the CSV's header line (column names only) and warns when
`trades_24h` is missing (step 3b).

Do not pull `main` into the deployed checkout for this: that changes the code
the listener runs (and its next restart applies the new schema). Instead take
a copy of the script from `origin/main` into `~/scout-ml` and run that copy:

```bash
cd /path/to/scout-analytics                # the deployed checkout (REPO), with .env
git fetch origin main && mkdir -p ~/scout-ml \
  && git show origin/main:ml/run_training.sh > ~/scout-ml/run_training.sh \
  && REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh
# weekly re-runs, venv already installed:          ... bash ~/scout-ml/run_training.sh --skip-install
# export with a built binary instead of go run:    ... bash ~/scout-ml/run_training.sh --binary ./scoutanalytics
```

Write `ML=~/scout-ml` without quotes (or `ML="$HOME/scout-ml"`): the script
refuses a quoted `~`, a relative path, a path inside `$REPO`, and a `$REPO`
inside `$ML` (or reached through `$ML/src`, e.g. a symlink). Do not run
the copy of the script inside `$ML/src/ml/`: step 1 rewrites that file while
it runs, so the script refuses to start from there.

At the end it prints the path of `report.md` and the train time; paste the
report as in step 8. Step output is kept in `$ML/logs/`. The step-by-step
below remains the reference; use it when the script stops or to run a single
step.

Time: about 10 minutes the first time (mostly `pip install`), then about
2 minutes per run. Memory: the export and training each need under 0.5 GB
(training took about 20 s on a desktop PC and peaked at about 200 MB on synthetic data of the
same size, and about 300 MB at 30,000 rows). `go run` compiles first, which
takes about 1 GB and 1 to 2 minutes. Disk: about 400 MB for the venv, about
10 MB for the CSV, about 1 MB per model version.

## 0. Paths

```bash
REPO=/path/to/scout-analytics      # the deployed checkout of kfukue/scout-analytics (runs main); .env is where you start the listener
ML=~/scout-ml                      # everything this runbook creates; outside the repo
mkdir -p "$ML"
```

This runbook assumes prod already runs from the new `kfukue/scout-analytics`
checkout (HANDOFF.md, section 5, cut-over checklist).

The CSV contains every call (addresses, symbols, outcomes). It never goes
into the repo: keep it under `$ML/data` (the repo's `.gitignore` also
ignores `*.csv`, but do not rely on that).

## 1. Get the ml/ code with the fixes (once per change to ml/)

The fixes from 7 October 2026 (repeat calls excluded, columns checked against
the view) and from 9 October 2026 (`long` skipped until its 30d data has
matured, forward test split for `medium`/`long`, category levels from the
train part only) must be on `main`. Train from a separate worktree of `origin/main`, so
the deployed checkout is not touched:

```bash
git -C "$REPO" fetch origin main
git -C "$REPO" worktree add --detach "$ML/src" origin/main
# later, to update it:  git -C "$ML/src" checkout --detach origin/main  (after the fetch above)

# refuse an old ml/ (it would train `long` and use the old medium split): MIN_MATURED comes with the fixes
grep -q '^MIN_MATURED' "$ML/src/ml/scout_ml/config.py" && echo "ml/ is up to date" || echo "OLD ml/: stop; the fixes are not pushed yet"
```

If `$REPO` is up to date with `origin/main`, `$REPO/ml` works as well; then
use that path instead of `$ML/src/ml` below.

## 2. Python venv (once)

Python 3.11 or newer (tested with 3.13; `requirements.txt` is not pinned).

```bash
python3 --version                      # must say 3.11 or newer
sudo apt-get install -y python3-venv libgomp1   # Debian/Ubuntu: venv module, OpenMP runtime for LightGBM
python3 -m venv "$ML/venv"
"$ML/venv/bin/python" -m pip install -U pip
"$ML/venv/bin/python" -m pip install -r "$ML/src/ml/requirements.txt"
```

If the server only has Python 3.10, stop here and tell the product manager
(not tested on 3.10).

Check the install with the test suite (synthetic data only, about 45 s;
`-p no:cacheprovider` keeps it from writing a cache into the worktree):

```bash
cd "$ML/src/ml"
"$ML/venv/bin/python" -m pytest tests -q -p no:cacheprovider
```

Expected: `109 passed` (one deprecation warning from fastapi is fine).

## 3. Read-only checks before the export (optional, psql or pgAdmin)

These only read. Paste the results to the product manager together with the
report.

```sql
-- a) calls per week and how many have each outcome yet
SELECT date_trunc('week', message_date)::date AS week, count(*) AS rows,
       count(*) FILTER (WHERE post_kind IS DISTINCT FROM 'update' AND tracking_status IS DISTINCT FROM 'repeat') AS calls,
       count(ret_late_1d) AS has_1d, count(ret_late_7d) AS has_7d, count(ret_late_30d) AS has_30d
FROM scout_call_dataset_v GROUP BY 1 ORDER BY 1;

-- b) horizons the tracker could not compute (left out of training, not counted as losses)
SELECT horizon, status, count(*) FROM scout_call_returns GROUP BY 1, 2 ORDER BY 1, 2;

-- c) later calls of a token (the training now leaves all of them out)
WITH v AS (
  SELECT d.call_id, d.tracking_status, d.ret_late_1d, d.ret_late_7d, d.ret_late_30d,
         row_number() OVER (PARTITION BY lower(d.contract_address) ORDER BY d.message_date, d.call_id) AS rn
  FROM scout_call_dataset_v d WHERE d.post_kind IS DISTINCT FROM 'update')
SELECT count(*) FILTER (WHERE rn > 1) AS later_calls,
       count(*) FILTER (WHERE rn > 1 AND tracking_status = 'done') AS later_calls_left_done,
       count(*) FILTER (WHERE rn > 1 AND COALESCE(ret_late_1d, ret_late_7d, ret_late_30d) IS NOT NULL) AS later_calls_with_outcomes,
       count(*) FILTER (WHERE rn = 1 AND tracking_status = 'repeat') AS first_calls_marked_repeat  -- expect 0
FROM v;
-- first_calls_marked_repeat > 0 means the tracker and the training disagree on
-- which call is a token's first: the training would lose that token. The
-- tracker puts such rows back to pending on its next cycle, so rerun the query
-- a few minutes later; if it stays above 0, stop and report the number.

-- d) long bucket: calls counted as "collapse" only because of the rugged flag
SELECT count(*) AS rugged_but_30d_above_minus_90
FROM scout_call_dataset_v WHERE rugged AND ret_late_30d > -90;

-- e) label rates per bucket (first calls in USD with that outcome, net of tax;
--    roughly what the report's "positive rate" will show; see the gate ceilings in step 7)
WITH v AS (
  SELECT d.*, (1 - COALESCE(d.tax_buy_pct, 0) / 100) * (1 - COALESCE(d.tax_sell_pct, 0) / 100) AS keep,
         row_number() OVER (PARTITION BY lower(d.contract_address) ORDER BY d.message_date, d.call_id) AS rn
  FROM scout_call_dataset_v d WHERE d.post_kind IS DISTINCT FROM 'update'),
f AS (
  SELECT * FROM v
  WHERE rn = 1 AND price_unit = 'usd' AND entry_late_price_usd IS NOT NULL
    AND COALESCE(tracking_status, '') NOT IN ('repeat', 'no_pool', 'gave_up'))
SELECT 'short' AS bucket, count(*) AS n,
       round(avg(((1 + max_gain_late_1d / 100) * keep >= 2.0)::int), 3) AS runner_rate,
       round(avg(((1 + ret_late_1d / 100) * keep <= 0.5)::int), 3) AS collapse_rate
FROM f WHERE max_gain_late_1d IS NOT NULL AND ret_late_1d IS NOT NULL
UNION ALL
SELECT '3day', count(*),
       round(avg(((1 + ret_late_3d / 100) * keep >= 1.5)::int), 3),
       round(avg(((1 + ret_late_3d / 100) * keep <= 0.4)::int), 3)
FROM f WHERE ret_late_3d IS NOT NULL
UNION ALL
SELECT 'medium', count(*),
       round(avg(((1 + ret_late_7d / 100) * keep >= 1.5)::int), 3),
       round(avg(((1 + ret_late_7d / 100) * keep <= 0.3)::int), 3)
FROM f WHERE ret_late_7d IS NOT NULL
UNION ALL
SELECT 'long', count(*),
       round(avg(((1 + ret_late_30d / 100) * keep > 1.0)::int), 3),
       round(avg((((1 + ret_late_30d / 100) * keep <= 0.1) OR COALESCE(rugged, false))::int), 3)
FROM f WHERE ret_late_30d IS NOT NULL;
```

Query (e) keeps the few extreme outcomes (above 100,000 %) that the training
drops, so its rates can differ slightly from the report's. None of these
queries has been run against a database yet (checked by reading only).

### 3b. `trades_24h` in the view (once, before the first training that uses it)

The "dead after the call" rule needs the `trades_24h` column of
`scout_call_dataset_v` (added in `scoutanalytics.sql` on 9 October 2026).
The export below runs with `SCOUT_DB_AUTO_MIGRATE=false`, so it does not add
the column. Two ways, the first preferred:

1. Deploy: getting the new view onto the server is a deploy decision, made
   by the owner. It normally happens when the branch is merged and deployed:
   pull `main` in the server checkout and restart the Go processes; every
   start (any mode) applies the whole `scoutanalytics.sql` unless
   `SCOUT_DB_AUTO_MIGRATE=false` (default `true`), and the new view comes
   with it. Training itself never pulls `main` into the deployed checkout
   (see the top of this runbook).
2. By hand, the views only (nothing else from the file; no table or tracking
   row is touched): take the block from `-- Training dataset: one row per call`
   to the end of `CREATE VIEW scout_call_predictions_v` from the `origin/main`
   copy and run it in one transaction:

   ```bash
   sed -n '/^-- Training dataset: one row per call/,/^WHERE pr.bucket IS NOT NULL;/p' \
       "$ML/src/scoutanalytics.sql" > "$ML/views.sql"
   grep -c '^DROP VIEW\|^CREATE VIEW' "$ML/views.sql"   # must print 4
   # then, connected to the listener's database as its user (do not paste the password anywhere):
   #   psql <connection> -1 -v ON_ERROR_STOP=1 -f "$ML/views.sql"
   # or paste the file into pgAdmin's query tool between BEGIN; and COMMIT;
   ```

   A hand-applied view lasts only until the next start of an older deployed
   program: its own `scoutanalytics.sql` recreates the views without the column.

Until then training runs with the plain collapse labels, prints a WARNING and
says so in the report ("dead-after-the-call rule SKIPPED").

`trades_24h` relies on the 1d horizon being named `1d` (`SCOUT_PERF_HORIZONS`,
default `1h,1d,3d,7d,30d`): if the report shows the dead rule on but 0 dead calls
("0 of 0 calls with a known `trades_24h`"), check that setting.

Read-only check after it is applied (paste the result with the report):

```sql
-- f) trades_24h: how many first calls have it, and how many are dead (< 50)
SELECT date_trunc('week', message_date)::date AS week,
       count(*) FILTER (WHERE entry_price_source LIKE 'onchain-%') AS onchain,
       count(trades_24h) AS has_trades_24h,
       count(*) FILTER (WHERE trades_24h < 50) AS dead,
       count(*) FILTER (WHERE trades_24h < 50 AND dex ILIKE '%uniswap%v4%') AS dead_uniswap_v4
FROM scout_call_dataset_v
WHERE post_kind IS DISTINCT FROM 'update' AND tracking_status IS DISTINCT FROM 'repeat'
GROUP BY 1 ORDER BY 1;
-- expected: many dead "Uniswap V4" calls in the weeks of 21 and 28 Sep; e.g.
SELECT call_id, dex, trades_24h, ret_late_1d FROM scout_call_dataset_v WHERE call_id IN (504, 578, 664);

-- g) posted DEX names, to extend DEX_FAMILY_RULES in ml/scout_ml/config.py
SELECT lower(trim(dex)) AS dex, count(*) AS calls, min(message_date)::date AS first,
       max(message_date)::date AS last
FROM scout_call_dataset_v
WHERE post_kind IS DISTINCT FROM 'update' AND dex IS NOT NULL
GROUP BY 1 ORDER BY 2 DESC;
```

## 4. Export the dataset

Run with the deployed code, from a directory that contains the listener's
`.env` (the Go program and its database package both read `./.env`).
`SCOUT_DB_AUTO_MIGRATE=false` keeps the export from applying
`scoutanalytics.sql` (which every start does otherwise). One write remains, as
on every start: the investigation-tool registry (`scout_investigation_tools`)
is upserted from the same `.env`, which leaves it as it is. The export does
not use Telegram and does not touch calls or tracking rows, so a running
listener and tracker are not affected.

```bash
umask 077
mkdir -p "$ML/data"
cd "$REPO"                         # must contain .env, as when you start the listener with go run
SCOUT_DB_AUTO_MIGRATE=false go run . -export-dataset "$ML/data/calls.csv"
# or, with a built binary:  SCOUT_DB_AUTO_MIGRATE=false ./scoutanalytics -export-dataset "$ML/data/calls.csv"
```

Expected: `wrote N calls to .../calls.csv`. N counts every row of the view:
first calls, later calls of the same token and update posts, so it is larger
than the ~4,733 first calls. Do not open, copy or paste the file.

## 5. Train

```bash
cd "$ML/src/ml"
time "$ML/venv/bin/python" train.py --csv "$ML/data/calls.csv" --out "$ML/models"
```

It prints one line per bucket (`PASS`, `FAIL (see report.md)` or
`skipped: ...`) and `wrote .../models/<version>`. A line `WARNING: the data has
no trades_24h column` means step 3b has not been done yet (the training is
still valid, with the plain collapse labels). Exit code 0 even when gates
fail. Random seed: fixed (`random_state=7` in `scout_ml/config.py`), so a
rerun on the same CSV gives the same numbers.

If `train.py` stops with a Python error instead, paste the last 20 lines of
the error to the product manager (not the CSV), and still do step 6.

Files written, in `$ML/models/<version>/` (version = UTC time, e.g.
`20261008-0930`):

| file | what |
|---|---|
| `report.md` | the report to read and paste |
| `meta.json` | features, category levels, thresholds, all metrics (summaries only) |
| `<bucket>_runner.joblib`, `<bucket>_collapse.joblib` | trained models, only for buckets that trained |

`$ML/models/LATEST` holds the newest version name.

## 6. Delete the CSV

```bash
rm -f "$ML/data/calls.csv"
```

The models and the report hold no rows of the dataset; keep them.

## 7. Read the report

```bash
cat "$ML/models/$(cat "$ML/models/LATEST")/report.md"
```

From the top:

1. **Verdict**: one line per bucket. A bucket PASSes only if all three hold
   on the test part (`short`, `3day`: the latest ~15% of calls; `medium`,
   `long`: the calls of the last 14 days with an outcome):
   - top-10% lift >= 2: calls in the top 10% by runner score are at least
     twice as often runners as calls overall (the report adds an approximate
     95% interval and the runner counts; with ~70 calls in the top 10% the
     interval is wide, so read a lift near 2 with care);
   - collapses removed >= 40%: skipping the 30% with the highest collapse
     score avoids at least 40% of the collapses. Always counted on the plain
     collapse label, also when the model was trained on collapse OR dead; the
     "bad outcomes removed incl. dead" row under it is information only, not
     a gate;
   - beats buy-all in every week: in every evaluated walk-forward week, the
     top 10% earned more on average than buying every call.
2. **Data and exclusions**: one line per reason a row was left out (update
   post, repeat call, no pool, not in USD, extreme outcome), then per bucket
   how many calls are not labelled yet. Not labelled yet means the horizon is
   not due or the tracker could not compute it; these calls are left out of
   that bucket, never counted as losses.
3. Per bucket: the split (rows dropped by the embargo), class balance, for
   `medium` and `long` the purged k-fold inside train (rounds, calibration),
   test metrics (LightGBM vs the logistic baseline), the gate table, the
   money simulation, the walk-forward table, calibration and top features.

What to expect from the October 2026 data (calls from late July 2026):

| bucket | horizon | walk-forward weeks | weeks with training data | expected |
|---|---|---|---|---|
| short | 1d | ~10 | ~10 | evaluated; 70/15/15 split |
| 3day | 3d | ~10 | ~10 | evaluated; 70/15/15 split |
| medium | 7d | ~9 | ~8 | evaluated; no validation part: test = last 14 days (~900 calls), train = everything before that date minus 7 days, rounds and calibration from a purged 5-fold inside train |
| long | 30d | - | - | **skipped**: "not enough matured 30d data (...)" until at least 2,000 calls with a 30d outcome span 120 days, i.e. an export from about 25 December 2026 |

`prior_calls` and `secs_since_prev_call` are no longer model inputs (always
0 / empty for a first call); the coverage table no longer lists them.

Ceilings: two gates cannot be met at all when a label is too common. Compare
with the "positive rate (usable)" column of each bucket's Class balance table
(or query (e)):

- collapses removed >= 40% by skipping 30% of calls is impossible when the
  collapse rate is above 75% (at most 30% / rate of the collapses can be removed);
  the gate counts plain collapses, so compare with the plain collapse rate: with
  the dead rule on, the "positive rate (usable)" of the collapse label includes
  dead calls, so use the "collapse rate (plain)" column of the bucket's "Dead
  after the call" table instead (it also shows the rate with dead calls, and
  the "bad outcomes removed incl. dead" figure, which is not a gate);
- top-10% lift >= 2 is impossible when the runner rate is above 50% (the lift
  is at most 1 / rate).

A bucket above either ceiling FAILs whatever the model does; say so when you
paste the report. The `long` collapse label (<= -90% at 30 days, or rugged)
is the most likely to be above 75%.

A FAIL on the first run is a normal outcome, not an error. "Beats buy-all in
every week" is strict with only 8 to 10 weeks, the earliest of which train
on one or two weeks of calls.

## 8. Paste back to the product manager

- The whole `report.md` (it has only counts and metrics, no calls or
  addresses), or at least: Verdict, Data and exclusions, and per bucket the
  Split line, the gate table, the money simulation line and the walk-forward
  table.
- The output of the step 3 queries, if you ran them.
- The train time (`real` from `time`).

Never paste the CSV, rows of it, or anything from `.env`.

## 9. Scores stay off

Until the product manager says the gates passed:

- `SCOUT_MODEL_URL` stays unset for the listener. To check without showing
  the value: `grep -c '^SCOUT_MODEL_URL=' .env` must print `0` (run in the
  listener's directory);
- do not start `serve.py`.

Even after a PASS, scores are only appended to the delivered message; they
never decide whether a call is delivered.

## Cleanup (when done with the worktree)

```bash
rm -f "$ML/data/calls.csv"
git -C "$REPO" worktree remove --force "$ML/src"   # --force: ignores __pycache__ left by the tests
```

If `$ML/src` was deleted by hand, git still lists it; clear only that entry with
`git -C "$REPO" worktree remove --force "$ML/src"` (do not use
`git worktree prune` in the deployed checkout; it clears every stale entry).

The venv and `$ML/models` can stay for the next weekly retrain (repeat steps
1, 4, 5 and 6).
