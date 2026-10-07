# scout ML: runner / collapse scores for calls

Trains small models on the `scout_call_dataset_v` view (one row per call:
features known at the moment of the post, plus what the price did afterwards)
and serves a score for a new call over HTTP.

For each holding bucket there are two yes/no models:

| bucket | runner (net of tax) | collapse (net of tax) |
|---|---|---|
| `short`  | max gain within 1d >= +100% | return at 1d <= -50% |
| `3day`   | return at 3d >= +50% | return at 3d <= -60% |
| `medium` | return at 7d >= +50% | return at 7d <= -70% |
| `long`   | return at 30d > 0 | return at 30d <= -90% or rugged |

Outcomes are the `*_late_*` columns (entry 60 s after the post), reduced by
buy and sell tax. Thresholds, feature lists and pass gates are in
`scout_ml/config.py`. The view's `latest_*` columns (price and return as of the
tracker's most recent look) are outcomes that keep moving: they are never
features, and the leakage guard refuses any column whose name starts with `latest_`.
The same holds for what the tracker discovers after the call: `rugged`,
`current_liquidity_usd`, `current_price_usd`, `entry_price_source` and the
pool it found (`pool_dex`, `pool_address`, ...; any `current_*` or `pool_*`
column). The post's own `dex` and `launchpad` are known at the call and stay
features.

## Install

    pip install -r requirements.txt          # Python 3.11 or newer
    pip install 'psycopg[binary]'            # only for train.py --dsn

Step-by-step commands for a training run on the prod server (venv, export,
train, report, cleanup): [`RUNBOOK.md`](RUNBOOK.md).

## Export data

    <go binary> -export-dataset calls.csv

CSV with a header, one column per view column, empty string for NULL,
`true`/`false`, RFC3339 timestamps. No database at hand:
`python make_synthetic.py --out calls.csv` writes invented data in the same
layout, including repeat calls and update posts (for testing the pipeline only;
its metrics mean nothing; `--end` sets the time of the newest call).
`tests/test_view_columns.py` checks that the expected columns match the view in
`../scoutanalytics.sql`.

## Train

    python train.py --csv calls.csv --out models/
    python train.py --dsn postgres://user:pass@host/db --out models/

Writes `models/<version>/` (version = UTC time, e.g. `20261002-1530`) with
`<bucket>_<label>.joblib` (LightGBM model + calibrator), `meta.json`,
`report.md`, and points `models/LATEST` at it. Exit code is 0 even when the
quality gates fail; read the report.

How it validates (no random splits):

- rows ordered by `message_date`; train = earliest 70%, validation = next 15%
  (early stopping + calibration), test = latest 15%;
- embargo: train/validation rows posted less than the bucket horizon before
  the next boundary are dropped, so no outcome window reaches into a later part;
- all calls of a token go to the part where its first call falls (address
  compared without regard to letter case);
- walk-forward: train up to week N, test on week N+1, for every week.

Calls without a pool (`no_pool`, `gave_up`, or no late entry price) and rows
whose prices are not in USD are excluded; the report says how many. The tracker
follows only the first call of each token (lowest `message_date`, then
`call_id`, address case ignored, update posts never count as a first call).
Every later call is excluded and counted on its own line ("excluded, repeat
call"), whatever its row says: `tracking_status = repeat` rows that kept results
from before they were set aside, and later calls an older tracker left at
`done`, are excluded too.

A bucket uses a call only once all of that bucket's outcome columns are
present; the view has an outcome only after its horizon was computed. A call
still `tracking` (1d-7d done, 30d not due yet) is used by the short buckets and
left out of `long` as not labelled yet, never counted as a negative, even when
it is already flagged rugged. The report lists these per bucket ("not labelled
yet").
Update posts (`post_kind = update`, a "$TOKEN hit 3X ..." post about an earlier
call) are not calls: they are excluded from training and simulation whatever
else their row says, and counted on their own line ("excluded, update post (not
a call)"). `post_kind` itself can never become a model input.

Extreme outcomes: a drained or broken pool can produce absurd returns (e.g.
+3.9e47 %). A call whose label or simulation outcome (any bucket's runner,
collapse or return column, before tax) is above `MAX_OUTCOME_PCT` (100,000 %)
is excluded from every bucket, not clipped, and counted on its own line
("excluded, extreme outcome"). A total loss (-100 %) is a real outcome and is
kept. In the money simulation each call's net return counts at most
`SIM_MAX_RET_PCT` (+1,000 %), so one huge winner cannot decide "beats
buy-everything" alone; labels are not affected. Both caps are in
`scout_ml/config.py`.

## Read the report

`models/<version>/report.md`, top to bottom:

1. **Verdict** - PASS/FAIL per bucket. PASS needs all of: top-10% lift >= 2;
   skipping the 30% highest collapse scores removes >= 40% of collapses; the
   top-10% simulation beats buy-everything in every walk-forward week.
2. **Data and exclusions**, **Feature coverage** - how much data there was and
   which columns are mostly empty (coverage is measured on the calls left
   after the exclusions).
3. Per bucket: class balance (with warnings for labels under 10% / over 90%
   and for skipped models), test metrics for LightGBM and the logistic
   baseline (ROC AUC, PR AUC, Brier), the gate values, the money simulation,
   the walk-forward table, calibration by decile, top features.

If LightGBM is not clearly better than the logistic baseline, the data does
not yet support the more complex model. A bucket is skipped (with the reason
in the report) when it has fewer than 300 usable rows or a label has fewer
than 30 positives or negatives in training. The `long` bucket needs well over
30 days of history *after* the embargo: with about 70 days of data it is
skipped, and until roughly 230 days its validation part is empty, so it is
trained with a fixed number of rounds and left uncalibrated (the report says so).

## Run the scoring service

    uvicorn serve:app --host 127.0.0.1 --port 8601

`SCOUT_MODEL_DIR` selects the model directory (default `models/` next to
`serve.py`); the service follows `LATEST` and re-reads it on each request.

    GET  /health  -> {"ok": true, "model_version": "20261002-1530"}
    POST /score   <- {"row": { ...one scout_call_dataset_v row as JSON... }}
                  -> {"model_version": "...", "line": "Model ... · short: runner 31%, collapse 44% · ...",
                      "buckets": [{"bucket": "short", "runner_prob": 0.31,
                                   "collapse_prob": 0.44, "runner_rank_pct": 92.5}, ...]}

Nulls, missing columns and unknown keys are fine (treated as missing).
Outcome columns in the row are ignored. Only buckets with a trained runner
model appear; without a collapse model `collapse_prob` is null.
`runner_rank_pct` is where the runner score falls among the scores of the
test-period calls at training time (0-100). Both endpoints return 503 while
no model has been trained.

## Use from the Go listener

Start the service, then run the listener with

    SCOUT_MODEL_URL=http://127.0.0.1:8601

It posts the row of a new call to `/score` and can append `line` to its message.

## Retrain

Export again and run `train.py` again (weekly is reasonable, since new
outcomes arrive as horizons complete). A new version directory is written and
`LATEST` is updated; the running service picks it up on the next request. To
roll back, write an older version name into `models/LATEST`. Compare the new
`report.md` with the previous one before trusting it.

## Layout and tests

    scout_ml/config.py    buckets, thresholds, feature lists, forbidden columns, gates
    scout_ml/features.py  build_features(): the one function used by training AND serving
    scout_ml/labels.py    net-of-tax labels and usable-row rules
    scout_ml/validate.py  time split, embargo, token grouping, walk-forward, metrics
    scout_ml/model.py     LightGBM + calibration, logistic baseline, serving bundle
    scout_ml/report.py    report.md
    train.py  serve.py  make_synthetic.py  tests/

    python -m pytest tests -q

`build_features` raises if an outcome or bookkeeping column (anything starting
with `ret_`, `max_gain_`, `max_dd_`, plus `rugged`, `tracking_status`, ...)
would become a model input.
