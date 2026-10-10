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

Calls that were dead after the call (fewer than 50 trades in the 24 h after
it) are reported per bucket and get their own report-only score; they are not
collapses unless `DEAD_IS_COLLAPSE` is switched on (off by default; see "Dead
after the call" below).

Outcomes are the `*_late_*` columns (entry 60 s after the post), reduced by
buy and sell tax. Thresholds, feature lists and pass gates are in
`scout_ml/config.py`. The view's `latest_*` columns (price and return as of the
tracker's most recent look) are outcomes that keep moving: they are never
features, and the leakage guard refuses any column whose name starts with `latest_`.
The same holds for what the tracker discovers after the call: `rugged`,
`current_liquidity_usd`, `current_price_usd`, `entry_price_source` and the
pool it found (`pool_dex`, `pool_address`, ...; any `current_*` or `pool_*`
column). So is `trades_24h` (trading in the 24 h after the call; any
`trades_*` column). The post's own `dex` and `launchpad` are known at the call:
`launchpad` is a feature, `dex` is grouped into `dex_family` (see "DEX
families" below). Their levels (and `quote_asset`, `perceptor_verdict`) are matched
without regard to case and surrounding spaces, and are learned from the
training rows of each split only; a level not seen there is treated as missing.
An older model whose `meta.json` has one flat set of levels is still served
with the exact, case-sensitive matching it was trained with, and a model whose
feature list has raw `dex` (trained before `dex_family`) is still served with it.
`prior_calls` and `secs_since_prev_call` stay in the view but are not features:
training uses first calls only, where they are always 0 and NULL
(`UNUSED_VIEW_COLUMNS` in `scout_ml/config.py`).

## Install

    pip install -r requirements.txt          # Python 3.11 or newer
    pip install 'psycopg[binary]'            # only for train.py --dsn

Step-by-step commands for a training run on the prod server (venv, export,
train, report, cleanup): [`RUNBOOK.md`](RUNBOOK.md).

## Export data

    go run . -export-dataset calls.csv       # from the repo root (or: <go binary> -export-dataset calls.csv)

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
    python train.py --csv calls.csv --out models/ --variants   # plus the variant comparison (below)

Writes `models/<version>/` (version = UTC time, e.g. `20261002-1530`) with
`<bucket>_<label>.joblib` (LightGBM model + calibrator), `meta.json`,
`report.md`, and points `models/LATEST` at it. Exit code is 0 even when the
quality gates fail; read the report.

How it validates (no random splits):

- `short` and `3day`: rows ordered by `message_date`; train = earliest 70%,
  validation = next 15% (early stopping + calibration), test = latest 15%;
  train/validation rows posted less than the bucket horizon before the next
  boundary are dropped (embargo), so no outcome window reaches into a later part;
- `medium` and `long`: no validation part. Test = the calls of the last 14
  days (`FORWARD_TEST_DAYS`), from a midnight UTC date (a date, not a row
  quantile); train = calls posted before that date minus the horizon. The
  number of boosting rounds and the Platt calibration come from a purged,
  time-ordered 5-fold inside the train part only: training rows posted
  within one horizon before or after a held-out fold are purged, rounds =
  median best iteration over the usable folds, calibration is fitted on the
  out-of-fold scores, and the saved model is refitted on the whole train part.
  The rounds and the Platt calibration thus come from the smaller fold models
  and are applied to the model refitted on the whole train part; this is
  standard practice and can slightly underestimate the rounds.
  Nothing from the test period reaches training, rounds, calibration or
  category levels;
- all calls of a token go to the part where its first call falls (address
  compared without regard to letter case). Training uses first calls only,
  so this drops nothing; the report counts embargo and token drops apart;
- walk-forward: train up to week N, test on week N+1, for every week (the
  main model's round count; category levels from each window's training rows).
  A week counts toward the "beats buy-all in every week" gate, and toward the
  walk-forward summaries (variant table, dead-score proposal), only when its
  training part has at least `WF_MIN_TRAIN_ROWS` (1000) rows (owner decision,
  9 Oct 2026). Weeks below are still computed and shown, marked "not counted
  (train < 1000)": the early weeks of an expanding walk-forward from the data
  start (e.g. `medium`, week of 18 Aug 2026, 530 train rows) would otherwise
  fail the gate in every report.

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

### Dead after the call

`trades_24h` (view column, an outcome) is the number of price events in the
pool during the 24 h after the call: the sum of `events` over the call's
5-minute candles (`scout_call_candles`, `interval_seconds = 300`; the trade at
the call itself has no candle). It is NULL when the call is not priced on-chain
(`entry_price_source` not `onchain-*`) or the tracker has not yet scanned
through its 1d horizon (then the candles are incomplete), and 0 when it was
scanned and no candle exists. Caveat: on a Uniswap v2 pool every `Sync` event
counts, which includes liquidity added or removed, not only swaps; on a v3/v4
pool it counts swaps, on a Pons curve `CurveBuy`/`CurveSell`.

A call with `trades_24h < DEAD_TRADES_24H` (50) is "dead": bought, then nobody
traded it (e.g. about 230 "Uniswap V4" calls from 21 Sep to about 2 Oct 2026,
flat price, which counted as safe non-collapses). Dead calls stay in the data.

The dead rule (`DEAD_IS_COLLAPSE`, `scout_ml/config.py`) is **off by default**
(since report 20261009-1904: with it off the collapse ROC AUC on the plain
label was higher in every bucket, e.g. short 0.792 vs 0.771, and collapses
removed about equal). With `DEAD_IS_COLLAPSE = True` the collapse label of
every bucket is "collapse OR dead"; a call whose `trades_24h` is NULL keeps
the plain collapse label. `DEAD_TRADES_24H` (50) changes the collapse labels
only while the rule is on; it still sets the dead label of the report
tables and of the separate dead score. Each bucket's "Dead after the call"
table says how many calls are dead, how many of them were collapses anyway,
and the collapse rate with and without them.

The gates are unchanged by the dead rule: "collapses removed >= 40%"
(`collapse_ok`, the walk-forward "collapses removed" column) is always
measured on the PLAIN collapse label (`collapse_plain_<b>`), whatever label
the collapse model was trained on. Next to it the report shows "bad
outcomes removed incl. dead" (the share of collapse OR dead calls among the
same skipped 30%; `meta.json`: `trading.collapse_removed_with_dead`, and an
extra walk-forward column), with the rule on or off. That figure is
information only, never a gate: dead calls are probably easy to spot and
would inflate it. Without a `trades_24h` column the figure, the column and
the "Dead after the call" table are absent.

If the export has no `trades_24h` column (the view on the server predates it),
training still runs with the plain collapse labels, prints a WARNING and says
so in the data section of the report ("dead after the call: n/a"; with the
rule switched on: "dead-after-the-call rule SKIPPED", `meta.json`:
`"dead": {"policy": "missing"}`); the separate dead score is "n/a".

### Dead after the call: separate score (report only)

Every run also trains one extra model for the label dead = `trades_24h < 50`,
to see whether dead calls can be told apart at the moment of the post. It is
**report only: not saved, not served, no gates** (`meta.json`:
`"dead_score"`; not in `"models"`). Settings: `DEAD_SCORE_*` in
`scout_ml/config.py`.

- Rows: the usable rows of the `short` bucket whose `trades_24h` is known;
  rows with NULL `trades_24h` are left out (never negatives).
- Split: the short bucket's own time split (train 70% / validation 15% /
  test 15%, 1-day embargo), restricted to those rows. Trained once, not per
  bucket.
- Inputs: the same features as the saved models (`build_features`);
  `trades_24h` is the label only and stays a forbidden input.
- LightGBM (early stopping and Platt calibration on the validation part, as
  for the other labels) and the logistic baseline.

The section "Dead after the call: separate score" near the end of
`report.md` shows: the base rate per part; test ROC AUC, PR AUC and Brier
for both models; for the 10% and 30% of calls with the highest dead score
the precision (share dead, 95% Wilson interval), the lift over the test base
rate and the share of all dead calls caught by skipping them; calibration by
decile; what drives the score (LightGBM gain, and the largest logistic
coefficients per standard deviation of the standardised input, log1p for
skewed columns, positive = more likely dead); a weekly walk-forward table
(AUC for both models, top-10% precision, dead calls caught, dead calls per
week; weeks with fewer than 5 dead calls are marked "too few to judge"); and
the weeks with the most dead calls. When the two weeks with the most dead
calls hold more than half of them (`DEAD_WAVE_SHARE`), the report says they
came in a few waves and names the most common `dex_family` among those dead
calls. As of the 9 Oct 2026 data most dead calls came in two waves (late Sep
2026, mostly "Uniswap V4"), so the test part and the walk-forward numbers
depend on those weeks.

The section ends with **proposed** gates (not applied; nothing passes or
fails on them): top-10% precision >= 3x the base rate in the test part, and
in more than half of the walk-forward weeks with at least 5 dead calls (and at
least 1000 training rows, as for the bucket gates); it
says how each model fares. Whether a dead score may ever be used is for the
product manager to decide. An error in this section prints a `WARNING`,
records `"dead_score": {"error": ...}` and does not fail the run (the saved
models are written before it starts); without a `trades_24h` column the
section says "n/a".

### DEX families

There are about 50 posted DEX names, many short-lived ("Pons" until mid-August,
then "Pons V2"; "Pools Trade Instant" in August only). `dex_family` groups them
by the ordered prefix rules in `DEX_FAMILY_RULES` (`scout_ml/config.py`): the
name is lower-cased and spaces, `_`, `-`, `.` and `/` are removed ("Uniswap
V4", "uniswap_v4" and "UniswapV4" are all `uniswapv4`), and the first rule
whose prefix matches gives the family; any other name is `other`, an empty name
is missing. Edit the table to add a family; more specific prefixes go first.

The rules were built from the owner's list of posted names (28 July to
8 October 2026, RUNBOOK.md query (g)); `tests/test_features.py` pins that list:

| family | posted names |
|---|---|
| `pons` | pons, pons v2 |
| `uniswap_v4` / `uniswap_v3` / `uniswap_v2` | uniswap v4 / v3 / v2 (`uniswap`: a name without a version; none so far) |
| `longxyz` | longxyz |
| `pools` | pools trade instant, pools fun, pools trade cca |
| `bankr`, `letscash`, `varo` | the name itself |
| `o1` | o1 rwa, o1 (not orbofi) |
| `flap` | flap, flap stocks, flap pve |
| `sushi` | sushiswap, sushi |
| `lunch` | lunch pair v4, lunch pair v3, lunch v3 |
| `other` | everything else, e.g. virtuals v2, pair fund, noxa, noxafi, stonkbroker (v2), bags, orbofi, up |

Names with fewer than about 30 calls get no family of their own (they are
`other`). The existing per-split minimum (`MIN_CATEGORY_COUNT` = 20, applied
by `learn_cat_levels` to every categorical) still applies to the families: a
family with fewer than 20 calls in a training part is treated as missing
there (likely for `flap`, `sushi`, `lunch` in some walk-forward weeks).

Which DEX inputs a model gets is `DEX_INPUTS`: `"family"` (default,
`dex_family` only), `"raw"` (raw `dex` only, as before `dex_family`) or
`"both"`. Raw `dex` is no longer a model input by default: its short-lived
names are levels that vanish (or never reach `MIN_CATEGORY_COUNT`) from one
period to the next, while a family stays. Experiment on synthetic data with
drifting names (`python make_synthetic.py --dex-names drifting`: "Pons", then
"Pons V2", then "Pons V3" only in the newest ~12% of calls; Pons planted as
collapse-prone; 5 seeds, LightGBM seed 7), mean test collapse ROC AUC:

| bucket | dex only (before) | dex + dex_family | dex_family only |
|---|---|---|---|
| short  | 0.688 | 0.718 | 0.731 |
| 3day   | 0.693 | 0.696 | 0.712 |
| medium | 0.719 | 0.722 | 0.725 |

With a name that appears only in the test period, raw `dex` loses the signal
(the new name is no level) and keeping it next to the family is worse than the
family alone (the trees split on raw `dex` in training). Without such a rename
the three variants were equal (within 0.006). Synthetic data only shows the
mechanism; the real effect shows in the next prod report (compare the collapse
ROC AUC and top features with the previous version; `DEX_INPUTS = "raw"` or
`"both"` brings raw `dex` back, and `train.py --variants` compares all three
in one run). (That experiment used the earlier rule table, under which the
synthetic "Pools Trade Instant" and "O1 Rwa" names were `other`; they are
now `pools` and `o1`.)

The pool the tracker chose (`entry_price_source` = `onchain-v2/v3/v4/pons`) is
NOT a feature. It describes the pool at the call block, but it is written by
the tracker after the call; when a call is scored at delivery, its row does not
have it yet (the listener's live pre-call pass discovers the pool but does not
store its kind), so the model would see it in training and never when serving.
It needs a Go change (store the pool kind with the pre-call stats) first.

### Calibration on recent weeks

The base rates drift week to week (the short collapse rate fell from about
0.65 to 0.45 in late September), so the Platt calibration is fitted on the
most recent held-out rows only: the validation rows (`short`, `3day`) or the
out-of-fold rows (`medium`, `long`) posted within `CALIB_RECENT_DAYS` (14) days
of the newest of them, never on rows the model was fitted on. With fewer than
`CALIB_MIN_CLASS_ROWS` (20) of a class there, or a fit that would invert the
ranking, it falls back to all held-out rows (as before).

The recent window is not always better (prod report 20261009-2250, `medium`
collapse: test Brier 0.191 recent vs 0.170 all rows), so since 9 Oct 2026
(owner decision) each label (runner, collapse, and the separate dead score)
**chooses** between "recent window" and "all held-out rows" on its held-out
rows alone, never on test rows: the held-out rows are ordered by time, both
methods are fitted on the earliest `CALIB_CHOICE_FIT_FRAC` (50%) of them, and
their Brier score is compared on the rest; the lower wins (equal: all rows)
and is then fitted on all held-out rows. It uses all held-out rows without a
comparison when the held-out rows (or their earlier half) span less than 14
days, when the later half or the earlier half has fewer than 20 of a class,
when the recent window of the earlier half cannot be fitted, or when the recent
window of all held-out rows cannot be fitted (only in that last case the report
shows the recent window as "n/a (recent window cannot be fitted)" instead of
the all-rows fallback). The validation part of `short`/`3day` usually spans
less than 28 days, so for them it is all held-out rows (the report says why).
Calibration does not change the ranking, so lift, collapses removed and the
gates are unaffected. The report names the chosen calibration and the
comparison Brier values per label, and shows the test deciles and test Brier
of both calibrations (information only; test rows never choose).

### Variant comparison (`--variants`)

    python train.py --csv calls.csv --out models/ --variants

trains, in addition to the normal model, one variant per entry of `VARIANTS`
(`scout_ml/config.py`), each changing exactly ONE setting of the configured
baseline:

| variant | change |
|---|---|
| baseline | none (the saved model's own numbers; not retrained) |
| dead rule on (collapse OR dead) | `DEAD_IS_COLLAPSE = True` |
| raw dex instead of dex_family | `DEX_INPUTS = "raw"` |
| raw dex + dex_family | `DEX_INPUTS = "both"` |
| runners must be tradeable | `RUNNER_NEEDS_TRADES = True`: runner also needs `trades_24h >= RUNNER_MIN_TRADES_24H` (100); NULL keeps the label |

Variants are report only: they are never saved or served, and the saved model,
its `meta.json` (features, levels, rules) and its gates are always the
baseline's. Each variant uses the same rows, splits, walk-forward weeks and
seed. `trades_24h` stays a forbidden input in every variant (it is only used
for labels). The tradeable rule changes only the runner label (so the lift and
runner counts); the money simulation still uses every call. Without a
`trades_24h` column the two trades-based variants are shown as "n/a".
The earlier "dead threshold 100" variant (`DEAD_TRADES_24H = 100`) was
dropped: the threshold only changes the labels while the dead rule is on, so
with the rule off (the baseline) it would equal the baseline; in report
20261009-1904 (rule on) it was worse than 50 in every bucket.

The end of `report.md` has one table per bucket (`meta.json`: `"variants"`):
runner rate on test, runner lift top 10% with its 95% interval, runner ROC AUC
(LightGBM / logistic), collapse ROC AUC on the plain label (comparable across
variants) and on the label each variant trained on, collapses removed (plain,
the gate), counted weeks beating buy-all, walk-forward mean lift and mean
collapses removed (counted weeks only, as the gate), simulation top 10% vs all
calls, and the gate result. Lift and runner
AUC of the tradeable variant use its own (stricter) runner label; compare its
runner rate. The logistic baseline uses numeric inputs only, so it is the same
in the DEX variants. All variants are compared on the same test part: picking
the best by these numbers alone risks fitting the test period, so prefer a
variant that also wins in the walk-forward weeks. To adopt a variant, change
the setting in `config.py` and train again (it then becomes the baseline).

A variant that raises an error is recorded as `error: <message>` (in its
table row and in `meta.json` `"variants"`), a `WARNING` line is printed, the
other variants still run and `train.py` still exits 0: the baseline is saved
before any variant starts. If every variant fails, a `WARNING: all N variants
failed` line says so.

Runtime: about 4 to 5 times a normal run (on synthetic data of the fixture's
size, 6,000 calls over 170 days: about 30 s without, including about 2 s for
the dead score, and about 2 min with `--variants`).

## Read the report

`models/<version>/report.md`, top to bottom:

1. **Verdict** - PASS/FAIL per bucket. PASS needs all of: top-10% lift >= 2;
   skipping the 30% highest collapse scores removes >= 40% of collapses; the
   top-10% simulation beats buy-everything in every counted walk-forward week
   (a week counts when its training part has at least 1000 rows; see
   "Validation"; the verdict cell says how many weeks were not counted). Each
   top-10% lift (test and every walk-forward week) comes with an approximate
   95% interval (Wilson interval of the runner rate in the top 10%, divided by
   the base rate) and the runner counts in the top 10% and overall; the gate
   uses the point value.
2. **Data and exclusions**, **Feature coverage** - how much data there was and
   which columns are mostly empty (coverage is measured on the calls left
   after the exclusions).
3. Per bucket: class balance (with warnings for labels under 10% / over 90%
   and for skipped models), test metrics for LightGBM and the logistic
   baseline (ROC AUC, PR AUC, Brier), the gate values, the money simulation,
   the walk-forward table, calibration by decile, top features. `medium` and
   `long` also show the purged k-fold (folds used, rounds per fold,
   out-of-fold rows used for calibration). Each bucket also has a "Dead after
   the call" table with the collapse ROC AUC on the plain collapse label
   (comparable with reports from before the dead rule), and each label's
   calibration line names the chosen calibration (recent window or all
   held-out rows) with the held-out comparison Brier values; its table shows
   the predictions of the chosen, the recent-window and the all-rows
   calibration side by side.
4. **Dead after the call: separate score** (report only; see "Dead after the
   call: separate score" above).
5. With `--variants` only: **Variant comparison**, one table per bucket (see
   "Variant comparison" above).

If LightGBM is not clearly better than the logistic baseline, the data does
not yet support the more complex model. A bucket is skipped (with the reason
in the report) when it has fewer than 300 usable rows or a label has fewer
than 30 positives or negatives in training. The `long` bucket is skipped
("not enough matured 30d data (N rows over D days, need ...)") until at least
2,000 calls with a 30-day outcome span at least 120 days (`MIN_MATURED`):
about 75 days of train calls so that the purged k-fold keeps training rows,
plus the 30-day embargo and the 14-day test. With calls from 28 July 2026 that
is an export from about 25 December 2026. A skipped bucket writes no model
file, and the scoring service leaves it out. If fewer than 3 folds of the
k-fold are usable, a model is trained with a fixed number of rounds and left
uncalibrated (the report says so).

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
model appear (a skipped bucket, e.g. `long` before its data has matured, has
no line); without a collapse model `collapse_prob` is null. Each bucket is
scored with the category levels its models were trained with (stored per
bucket in `meta.json`).
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
    scout_ml/validate.py  time splits, embargo, purged k-fold, token grouping, walk-forward, metrics
    scout_ml/model.py     LightGBM + calibration, logistic baseline, serving bundle
    scout_ml/report.py    report.md
    train.py  serve.py  make_synthetic.py  tests/

    python -m pytest tests -q        # 148 passed, about 2.5 minutes

`build_features` raises if an outcome or bookkeeping column (anything starting
with `ret_`, `max_gain_`, `max_dd_`, `trades_`, plus `rugged`, `tracking_status`,
`trades_24h`, ...) would become a model input.
