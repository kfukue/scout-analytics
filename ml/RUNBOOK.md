# Runbook: first model training on the prod server

For the owner, on the prod server (Linux, bash). It exports the dataset,
trains the four bucket models and produces `report.md`. The commands are
exact; replace only the two paths in step 0.

What it does NOT do: it does not start the scoring service and does not wire
scores into deliveries. The scoring service may run and the listener may use
it (`SCOUT_MODEL_URL`); which buckets the delivery line shows is set by
`SCOUT_MODEL_LINE` on the scoring service (prod runs `all` since 13 Oct 2026,
the owner's decision of 12 Oct 2026; step 9). Scores never filter deliveries in any case.

## Quick way: one script

`ml/run_training.sh` runs steps 1, 2, 4, 5 (with `--variants`: 5b instead of 5) and 6 below in one go, with the
same commands and flags, plus the step 9 check (`SCOUT_MODEL_URL` in `.env`:
it says whether it is set, without showing the value, and goes on). It stops at the first
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
# with the variant comparison (5b instead of 5):    ... bash ~/scout-ml/run_training.sh --skip-install --variants
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
2 minutes per run, plus the stability diagnostics, which are on by default
(step 5: on synthetic data they made the training about 3 to 4 times as long;
not measured on prod yet). Memory: the export and training each need under 0.5 GB
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

Check the install with the test suite (synthetic data only, about 3 minutes;
`-p no:cacheprovider` keeps it from writing a cache into the worktree):

```bash
cd "$ML/src/ml"
"$ML/venv/bin/python" -m pytest tests -q -p no:cacheprovider
```

Expected: `206 passed` (one deprecation warning from fastapi is fine).

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

The "dead after the call" tables, the separate dead score and the dead rule
(off by default) need the `trades_24h` column of
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
says so in the report ("dead after the call: n/a", or "dead-after-the-call
rule SKIPPED" if the rule was switched on); the dead score section says "n/a".

`trades_24h` relies on the 1d horizon being named `1d` (`SCOUT_PERF_HORIZONS`,
default `1h,1d,3d,7d,30d`): if the report shows 0 dead calls ("0 of 0 calls
with a known `trades_24h`"), check that setting.

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

It first trains the normal model, then retrains it with the other LightGBM
seeds of `STABILITY_SEEDS` (11, 23, 31, 47, 61 and 89 since 14 Oct 2026) and pools the walk-forward weeks: the gates need both (owner
decision of 13 Oct 2026, see step 7), and it prints
`[gates] seed retrains N s, pooled walk-forward N s`. Then one line per bucket
(`PASS`, `FAIL: <reasons> (see report.md)` or `skipped: ...`) and
`wrote .../models/<version>`. A line `WARNING: seed <n> of the seed retrains
failed (...)` means every trained bucket FAILS this run (a gate input is
missing); paste it. The saved model is not affected. A line `WARNING: the data has
no trades_24h column` means step 3b has not been done yet (the training is
still valid, with the plain collapse labels). After the buckets it trains
the report-only dead score and prints `[dead score] N s` (a few seconds); a
line `WARNING: the dead score failed (...)` means only that report section is
missing: the saved models are not affected.

Then it runs the stability diagnostics (see step 7, item 5). They are on by
default and print one line at the end, e.g.
`[diagnostics] seeds N s, walk_forward N s, no_dead N s, adversarial N s; total N s`
("seeds" is the time of the seed retrains above, done for the gates; they are
not run twice). The seed retrains cost about two normal runs, with or without
the diagnostics: on synthetic data a whole run took 26 s before 13 Oct 2026,
73 s now with the diagnostics off and 102 s with them on (desktop PC). On the
prod data the extra time is an estimate, not measured yet. Since 14 Oct 2026
there are six seed retrains instead of two (7 seeds, step 7): on the same
synthetic data `[gates] seed retrains` took 134 s instead of 47 s and a run
with the diagnostics on about 187 s instead of 102 s. The `[gates]` and
`[diagnostics]` lines give the real figures (paste them, step 8). The
diagnostics start only after the model, `meta.json`, `report.md` and `LATEST`
are written, and the saved model, the gates and the verdict are the same with
or without them, so stopping during them still leaves the normal result.
Stopping earlier, during the seed retrains (before the bucket `PASS` / `FAIL`
lines), leaves a version directory with model files but no `meta.json`, and
`LATEST` still names the previous version (the scoring service keeps using
it); delete that directory and run again. A `WARNING` line about the
diagnostics (`WARNING: diagnostics part '<name>' failed (...)` or
`WARNING: the stability diagnostics failed (...)`) means only that part of
the section is missing (it says `Not computed: error: ...`); the saved models
and the verdict are not affected. Paste the warning.

To skip them for one run (for example when time is short), put
`SCOUT_ML_DIAGNOSTICS=0` in front of the command. That skips only the
report-only parts (rolling walk-forward, evaluation without dead calls,
adversarial validation, about 30 s on synthetic data): the seed retrains and
the pooled walk-forward still run, because the verdict needs them.

```bash
time SCOUT_ML_DIAGNOSTICS=0 "$ML/venv/bin/python" train.py --csv "$ML/data/calls.csv" --out "$ML/models"
# or with the script (run from the deployed checkout, as in "Quick way"):
SCOUT_ML_DIAGNOSTICS=0 REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh --skip-install
```

`0`, `off`, `false` and `no` switch them off; any other value prints a
`WARNING` and keeps the default (on). The script passes the variable through
to `train.py` as it is; nothing else changes. The report then has no
"Stability diagnostics" section; the verdict table still shows every seed's
value and the pooled numbers.

Exit code 0 even when gates fail. Random seed: fixed (`LGBM_SEED = 7` in
`scout_ml/config.py`; the diagnostics use fixed seeds too), so a rerun on the
same CSV gives the same numbers.

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

### 5b. Variant comparison (optional, when the product manager asks for it)

Run 5b **instead of** step 5, not after it: it already trains and saves the
normal model and writes `LATEST`. Running both trains the baseline twice and
moves `LATEST` to the second version. The normal way is the script, as in
"Quick way" above with `--variants` added (it runs steps 1, 2, 4, 5b and 6;
the fresh copy of the script matters: an older copy rejects `--variants`):

```bash
cd /path/to/scout-analytics                # the deployed checkout (REPO), with .env
git fetch origin main && mkdir -p ~/scout-ml \
  && git show origin/main:ml/run_training.sh > ~/scout-ml/run_training.sh \
  && REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh --skip-install --variants
```

By hand (in place of the step 5 command):

```bash
cd "$ML/src/ml"
time "$ML/venv/bin/python" train.py --csv "$ML/data/calls.csv" --out "$ML/models" --variants
```

It trains the normal model
exactly as in step 5 (that is the one saved, served and gated), then six
variants that each change one setting (dead rule on, i.e. collapse OR dead;
raw `dex` instead of `dex_family`; raw `dex` + `dex_family`; runners must have
`trades_24h >= 100`; stronger regularisation, i.e. smaller trees with more
calls per leaf, a stronger L2 penalty and fewer inputs per tree,
`LGBM_REGULARISATION = "strong"` in `scout_ml/config.py`; with calls_prev
counts (default before 14 Oct), i.e. with `calls_prev_1h` and
`calls_prev_24h` back in, `EXCLUDED_INPUTS = ()`). Since 14 Oct 2026 the
baseline leaves those two out (owner decision after report 20261014-1842:
they act as a calendar proxy), so this row keeps the old setting in every
report. The "dead rule off" and "dead threshold 100" rows of
earlier reports are gone: the rule is now off in the baseline, and the
threshold only matters with the rule on; so are "no calls_prev inputs" (now
the baseline) and "calls_prev as a burst ratio" (rejected, 14 Oct 2026). The variants are not saved; they only add a section
"Variant comparison" at the end of `report.md`, one table per bucket, one row
per variant (the `baseline` row is the saved model). It prints one
`[variant] <name>: N s` line per variant. Time: more than the 4 to 5 times
the step 5 train time measured with four variants (on a desktop PC,
synthetic data of 6,000 calls, before the gates of 13 Oct 2026: about 30 s
without, about 2 min with `--variants`; the gates' seed retrains now add
about 47 s to every run): each variant costs about one normal training run, so the fifth
and sixth add about one more each, and the calls_prev variant two more
for its seeds 11 and 23 (its `[variant]` line says "of which other
seeds and adversarial validation N s"; about 93 s for two such variants on
that synthetic data, 13 Oct 2026; on the prod data an estimated 40 to 50 s
for two, not measured). Since 14 Oct 2026 (7 seeds) the calls_prev variant
is retrained with six other seeds: on the same synthetic data its "of which"
time was 139 s instead of 48 s and a whole run with `--variants` 453 s
instead of 280 s. The stability
diagnostics of step 5 also run (before the variants) unless switched off with
`SCOUT_ML_DIAGNOSTICS=0`, and add their own time on top. Memory as in step 5.
The model, `meta.json`, `report.md` and `LATEST` are written before the
diagnostics and the variants start, so stopping
during either still leaves the normal result. A variant that crashes
does not fail the run: it prints `WARNING: variant '<name>' failed (error:
...)`, its row in the table and its `meta.json` entry say `error: <message>`,
the other variants go on, and the run still ends with exit code 0 (if every
variant fails it also prints `WARNING: all N variants failed`). Paste such a
warning to the product manager; the saved model is not affected.

How to read the table: compare each row with `baseline`. "collapses removed
(plain; gate)" and "collapse AUC, plain label" are comparable across all rows.
The last column is the old single-run gate of each row ("single-run (old)
gate, information"), also for `baseline`, so it can differ from the verdict at
the top (the pooled gates would need every variant retrained with every seed).
The column before it is each variant's own pooled walk-forward lift and
simulation difference with their intervals (seed 7 only): compare those with
the baseline's for a fairer picture than the test part alone.
The `runners must be tradeable` row measures lift on its own stricter runner
label (look at its "runner rate (test)"); its money simulation still uses every
call. Every row is judged on the same test part, so a variant that is better
there only may just fit that period: prefer one that is also better in the
walk-forward columns (weeks beating buy-all, walk-forward mean lift and mean
collapses removed). Paste the whole report as in step 8; the product manager
decides whether a setting changes.

The last table, "calls_prev variants across seeds" (see `ml/README.md`),
puts the baseline (without the calls_prev inputs) next to the variant
with them over every seed. If the variant's median lift is clearly better
than the baseline's in several reports, tell the product manager; the gap
between the two adversarial AUCs shows the drift those two inputs carry.

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

1. **Verdict**: one line per bucket, by the gates the owner decided on
   13 Oct 2026 (pooled walk-forward, "option (b)"). "Pooled" means the test
   calls of all counted walk-forward weeks put together, each week still
   ranked by its own model. A week counts only when its training part has at
   least 1000 rows (`WF_MIN_TRAIN_ROWS`, owner decision of 9 Oct 2026); weeks
   below are still in the walk-forward table, marked "not counted (train <
   1000)" (e.g. the early `medium` weeks from 18 Aug 2026, 530 train rows).
   A bucket PASSes only if all of these hold:
   - runner lift: the median of the pooled top-10% lift over the seven
     LightGBM seeds 7, 11, 23, 31, 47, 61 and 89 (`STABILITY_SEEDS`; three,
     7, 11 and 23, before 14 Oct 2026) is at least 2, so at least 4 of the 7
     seeds must reach 2 (the cell shows the median and every seed's value in
     the order of the header, e.g. with made-up numbers
     "yes: 2.18 (2.18 / 2.26 / 1.94 / 2.31 / 2.05 / 2.40 / 1.97)"), and the lower
     end of the 95% interval of the seed-7 pooled lift (whole weeks drawn
     again 2000 times) is above 1.0 (the next column);
   - beats buy-all: the pooled money simulation, top 10% minus all calls
     (mean net return, each call capped at +1,000 %), has the lower end of
     its 95% interval above 0 (e.g. "yes: +43.1 [+36.7, +51.6]");
   - collapses removed >= 40% on the test part (`short`, `3day`: the latest
     ~15% of calls; `medium`, `long`: the calls of the last 14 days with an
     outcome): skipping the 30% with the highest collapse score avoids at
     least 40% of the collapses. Always counted on the plain collapse label,
     also when the model was trained on collapse OR dead; the "bad outcomes
     removed incl. dead" row under it is information only, not a gate.
   Information only, never a gate: "absolute return" (the pooled top-10%
   mean net return; "(<= 0)" means even the top 10% lost money on average)
   and the two old gates, marked "old gate, information only": the top-10%
   lift on the test part (>= 2) and "beats buy-all in every counted week"
   (with how many weeks were not counted). They show how the old verdict
   would have been. Under the table, each FAILing bucket has a line with its
   reasons. A missing value never passes: fewer than 2 counted weeks, a seed
   retrain that failed, or an "n/a" value makes the bucket FAIL with that
   reason. On report 20261013-0702 this gives: `3day` FAIL (median 1.98 from
   1.98 / 2.02 / 1.85), `medium` PASS, `short` FAIL. The variant table's
   walk-forward columns and the dead-score proposal use the counted weeks
   only, too.
2. **Data and exclusions**: one line per reason a row was left out (update
   post, repeat call, no pool, not in USD, extreme outcome), then per bucket
   how many calls are not labelled yet. Not labelled yet means the horizon is
   not due or the tracker could not compute it; these calls are left out of
   that bucket, never counted as losses.
3. Per bucket: the split (rows dropped by the embargo), class balance, for
   `medium` and `long` the purged k-fold inside train (rounds, calibration),
   test metrics (LightGBM vs the logistic baseline), the gate table (the
   gates, then the information rows and a "Why FAIL:" line), the money
   simulation on the test part, the walk-forward table, calibration and top
   features.
   Each label's "Platt calibration:" line names the calibration it uses
   (**recent window** or **all held-out rows**), chosen on held-out rows only
   (comparison Brier values given, or the reason no comparison was made);
   paste those lines with the report.
4. **Dead after the call: separate score (report only; not saved, not
   served)**: one extra model (LightGBM and logistic) for "dead" =
   `trades_24h < 50`, on the short bucket's rows and split, NULL
   `trades_24h` left out. Read, in order: the base rate per part (train /
   val / test); test ROC AUC and PR AUC of both models (0.5 = no better than
   chance); the "Skipping the calls with the highest dead score" table:
   precision = share of dead calls among the top 10% / 30%, with its 95%
   interval, and lift = precision / test base rate (lift 3 = three times as
   many dead calls as overall), and the share of all dead calls caught; then
   what drives the score; then the weekly walk-forward table, where weeks
   marked "< 5 dead: too few to judge" should be ignored. As of the 9 Oct
   2026 data most dead calls came in two waves (late Sep, "Uniswap V4"); the
   report says so when it is still true ("Most dead calls came in a few
   waves"): good numbers driven by one wave may not hold for the next. The closing "Proposed gates" lines are a proposal only
   (nothing passes or fails on them). If the section says `Not computed:
   error: ...` (and the run printed `WARNING: the dead score failed`), paste
   that line; the saved models are not affected. Never act on this score:
   it is not served.
5. **Stability diagnostics (not saved, not served)**: helps tell whether the
   numbers above are real or noise. Since 13 Oct 2026 two of its numbers are
   the inputs of the gates (computed in every run, also without this
   section): each seed's pooled walk-forward lift, and the pooled expanding
   walk-forward with its intervals. Everything else in it passes or fails
   nothing, and the section never changes the saved model, the gates or the
   verdict. It is missing when the run had `SCOUT_ML_DIAGNOSTICS=0`
   (step 5). Its parts:
   - **Seed stability**: the normal model trained again with the LightGBM
     seeds 11, 23, 31, 47, 61 and 89 next to the saved model's seed 7
     (`STABILITY_SEEDS`; seeds 11 and 23 only before 14 Oct 2026; same rows, splits and
     weeks; only the seed changes). Per bucket and seed: test top-10% lift
     with its interval, collapses removed, runner ROC AUC, the money
     simulation top 10% vs all, the counted walk-forward weeks beating
     buy-all, the pooled walk-forward lift (marked "gate input": its median
     is the runner-lift gate) and the old single-run gate of each run
     (information); then min / median / max over the seeds. A lift or a
     PASS/FAIL that moves between seeds is noise, not a real result.
   - **Pooled walk-forward (expanding and rolling)**: the test rows of all
     counted walk-forward weeks of a bucket put together, each week still
     ranked by its own model, so one lift over many more calls than a single
     week. Its 95% interval comes from a week-block bootstrap (whole weeks
     drawn again 2000 times), so it includes the swings from week to week;
     with few weeks it is rough, with one week n/a. Also pooled collapses
     removed, the pooled money simulation (with an interval on the
     difference to buying all) and the weeks beating buy-all. "Expanding" is
     the gates' walk-forward (every call before the week; its lift and
     simulation-difference intervals are the ones in the verdict); "rolling" trains
     each week's model on the last 4 weeks of calls only. Both are pooled
     over the same weeks (the ones the gate counts), minus any week the
     rolling model could not train (marked in the per-week table), and the
     per-week table shows them side by side; the rolling window is small by design, so the
     1000-row rule does not apply to its training size.
   - **Evaluation without dead calls**: the same trained models measured
     again on the calls that are not dead (`trades_24h < 50`; calls with an
     unknown `trades_24h` stay in), with the top 10% / 30% taken again among
     them. Nothing is retrained and the gates do not change; it shows how
     much of the result comes from the dead calls. "n/a" when the data has no
     `trades_24h` column (step 3b).
   - **Adversarial validation**: a separate model tries to tell the train
     part from the test part on the model's own inputs (without hour and
     weekday). Its ROC AUC: about 0.5 = the two parts look alike, above 0.7 =
     clear drift (the test calls differ from the calls the model learned
     on). The "top drifting features" are the inputs that changed most. The
     same per counted walk-forward week, for the newest 6 weeks.

   A part that failed says `Not computed: error: ...` (and the run printed a
   `WARNING`); paste that line. Apart from the two gate inputs, the product
   manager reads these numbers next to the gates; they never decide anything
   on their own.
6. With `--variants` only: **Variant comparison** (step 5b).

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
  the gate counts plain collapses, so compare with the plain collapse rate (the
  dead rule is off by default, then the two are the same): with
  the dead rule on, the "positive rate (usable)" of the collapse label includes
  dead calls, so use the "collapse rate (plain)" column of the bucket's "Dead
  after the call" table instead (it also shows the rate with dead calls, and
  the "bad outcomes removed incl. dead" figure, which is not a gate);
- a pooled top-10% lift >= 2 is impossible when the runner rate is above 50%
  (the lift is at most 1 / rate).

A bucket above either ceiling FAILs whatever the model does; say so when you
paste the report. The `long` collapse label (<= -90% at 30 days, or rugged)
is the most likely to be above 75%.

A FAIL is a normal outcome, not an error. The pooled gates replaced the old
"beats buy-all in every week" and test-part lift because those swung with
the seed and with single weeks (report 20261013-0702: `3day` and `medium`
passed with seed 7 and failed with seeds 11 and 23).

## 8. Paste back to the product manager

- The whole `report.md` (it has only counts and metrics, no calls or
  addresses), or at least: Verdict, Data and exclusions, and per bucket the
  Split line, the gate table (with its "Why FAIL:" line), the money
  simulation line and the walk-forward table.
- The output of the step 3 queries, if you ran them.
- The train time (`real` from `time`), the `[gates] ...` line and the
  `[diagnostics] ...` line (step 5), if the diagnostics ran.

Never paste the CSV, rows of it, or anything from `.env`.

## 9. Scores on deliveries

- The scoring service (`serve.py`) may run, and the listener may use it
  (`SCOUT_MODEL_URL` in its `.env`). To see whether it is set without showing
  the value: `grep -c '^SCOUT_MODEL_URL=' .env` (run in the listener's
  directory; `0` means no score line).
- Which buckets the delivery line shows is set by `SCOUT_MODEL_LINE` on the
  scoring service: `passed` (the default) or `all` (failing buckets marked
  "(not validated)"). The owner decided on 12 Oct 2026 to run prod with
  `all` (he is the only reader of the delivery group). Changing it needs a
  restart of the scoring service only.
- Scores are only appended to the delivered message; they never decide
  whether a call is delivered.
- After each retrain, send the report's verdict to the product manager
  (step 8).

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
