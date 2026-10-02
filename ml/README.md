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
`scout_ml/config.py`.

## Install

    pip install -r requirements.txt          # Python 3.11
    pip install 'psycopg[binary]'            # only for train.py --dsn

## Export data

    <go binary> -export-dataset calls.csv

CSV with a header, one column per view column, empty string for NULL,
`true`/`false`, RFC3339 timestamps. No database at hand:
`python make_synthetic.py --out calls.csv` writes invented data in the same
layout (for testing the pipeline only; its metrics mean nothing).

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
- all calls of a token go to the part where its first call falls;
- walk-forward: train up to week N, test on week N+1, for every week.

Calls without a pool (`no_pool`, `gave_up`, or no late entry price) and rows
whose prices are not in USD are excluded; the report says how many.

## Read the report

`models/<version>/report.md`, top to bottom:

1. **Verdict** - PASS/FAIL per bucket. PASS needs all of: top-10% lift >= 2;
   skipping the 30% highest collapse scores removes >= 40% of collapses; the
   top-10% simulation beats buy-everything in every walk-forward week.
2. **Data and exclusions**, **Feature coverage** - how much data there was and
   which columns are mostly empty.
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
