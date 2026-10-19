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
That also holds when serving: a value the model was not trained with (for
example Perceptor's `insufficient` verdict, "not enough data yet", for a model
trained before it existed) is scored exactly as if `perceptor_verdict` were
null, without an error. At training, `insufficient` is its own level, separate
from `unknown` (the view passes `verdict_level` through unchanged), once it has
at least `MIN_CATEGORY_COUNT` (20) rows in the training part; rarer, it is
missing. After the relabel of the historical Perceptor reports from `unknown`
to `insufficient` ships, retrain before trusting the scores: older models
learned those reports as `unknown`.
The timing and rug outcomes the tracker stores (`peak_late_after_s`,
`first_2x_after_s`, `above_2x_s`, `fall_below_2x_after_s`, `above_2x_censored`,
`timing_at`, `rug_at`, `rug_at_kind`) are not in the view yet; when they are
added they are outcomes, never features: the guard refuses them and any
per-horizon variant (`TIMING_RUG_OUTCOMES` and `FORBIDDEN_PREFIXES` in
`scout_ml/config.py`).
An older model whose `meta.json` has one flat set of levels is still served
with the exact, case-sensitive matching it was trained with, and a model whose
feature list has raw `dex` (trained before `dex_family`) is still served with it.
`prior_calls` and `secs_since_prev_call` stay in the view but are not features:
training uses first calls only, where they are always 0 and NULL
(`UNUSED_VIEW_COLUMNS` in `scout_ml/config.py`).
`calls_prev_1h` and `calls_prev_24h` stay in the view too, but since 14 Oct
2026 they are not inputs of new models (`EXCLUDED_INPUTS` in
`scout_ml/config.py`): they act as a calendar proxy (adversarial AUC 0.99,
mostly from `calls_prev_24h`), and in report 20261014-1842 the model without
them had a higher across-seed median pooled lift (3day 2.14 vs 2.10, medium
2.10 vs 1.86), passed every gate in both buckets and cut the drift to AUC
0.90. The scoring service builds each model's inputs from that model's own
`meta.json` `features`, so a model trained with them (up to 20261014-1842)
is still scored with them until a retrain replaces it.

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
quality gates fail; read the report. A version directory that already exists
(a second run in the same UTC minute) is refused with an error before
anything is written.

    python train.py --csv calls.csv --out models/ --candidate

`--candidate` trains exactly the same way but never writes `LATEST`: the
serving model stays as it was. At the very end of the run, after `meta.json`
and `report.md` are final, it writes the new version name into
`models/CANDIDATE`. It goes live only with `promote.py` (see "Promote,
roll back, status").

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
  A week counts toward the pooled walk-forward gates (see "Gates" below; and
  the old "beats buy-all in every week" count, now information), and toward
  the walk-forward summaries (variant table, dead-score proposal), only when
  its training part has at least `WF_MIN_TRAIN_ROWS` (1000) rows (owner
  decision, 9 Oct 2026). Weeks below are still computed and shown, marked "not
  counted (train < 1000)": the early weeks of an expanding walk-forward from
  the data start (e.g. `medium`, week of 18 Aug 2026, 530 train rows) are such
  weeks.

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

### Gates (owner decision of 13 Oct 2026)

A bucket PASSes, i.e. `meta.json` has `metrics.<bucket>.gates.passed = true`
(what the scoring service reads), only when all of these hold
(`validate.pooled_gates`, settings in the "Bucket gates" block of
`scout_ml/config.py`). The owner chose these pooled walk-forward gates
("option (b)") on 13 Oct 2026, after report 20261013-0702 showed `3day` and
`medium` passing the old gates with seed 7 but failing with seeds 11 and 23.
"Pooled" = the test rows of all counted walk-forward weeks of the bucket
together, each week ranked by its own model (the expanding walk-forward;
counted = training part of at least 1000 rows).

1. **Runner lift**: the median over the LightGBM seeds `STABILITY_SEEDS`
   (7, 11, 23, 31, 47, 61, 89; the baseline retrained with each, nothing
   saved; with 7 seeds the median is the 4th value) of the pooled
   top-10% lift >= `GATES["min_top_lift"]` (2), and the lower end of the 95%
   week-block bootstrap interval of the saved seed's (`LGBM_SEED` = 7) pooled
   lift > `GATE_POOLED_LIFT_LOWER` (1.0). Pooled lift = runner rate in the
   per-week top 10% / runner rate of all pooled calls.
2. **Beats buy-all**: the lower end of the 95% bootstrap interval of the
   pooled simulation difference (mean net return of the per-week top 10% minus
   that of all calls, each call capped at `SIM_MAX_RET_PCT`; saved seed)
   > `GATE_SIM_DIFF_LOWER` (0).
3. **Collapses removed** >= 40% on the test part (saved model; unchanged).

Bootstrap: `POOLED_BOOTSTRAP_REPS` (2000) resamples of whole weeks with
replacement, seed `POOLED_BOOTSTRAP_SEED` (7), interval `POOLED_CI` (2.5% /
97.5%). A missing or undefined input never passes: fewer than
`GATE_MIN_POOLED_WEEKS` (2) counted weeks (the bootstrap needs two), a seed
retrain that failed or skipped the bucket, or any NaN fails the gate
concerned, and `gates.reasons` (and the report) says why. The **absolute
return** (pooled top-10% mean net return > 0) is shown as information only,
never a gate. The old single-run gates (top-10% lift >= 2 on the test part;
beats buy-all in every counted walk-forward week) are still computed and shown
as "old gate, information only" (`gates.old` in `meta.json`), so the change is
traceable. On the numbers of report 20261013-0702: `3day` FAIL (median pooled
lift 1.98 from 1.98 / 2.02 / 1.85; its simulation difference +28.6 [14.7, 51.0]
passes), `medium` PASS (2.18 from 2.18 / 2.26 / 1.94, lower end 1.74,
simulation difference +43.1 [36.7, 51.6]), `short` FAIL.

The seed retrains and the pooled walk-forward run in every training, before
`meta.json` is written, also with `SCOUT_ML_DIAGNOSTICS=0`: that switch now
only skips the report-only diagnostics (see "Stability diagnostics"). So
`STABILITY_SEEDS` and `POOLED_BOOTSTRAP_*` decide the verdict; change them
only with the owner. The saved model itself (training, features, seed 7,
calibration, scores) is the same as before the decision; only `passed` and
the report changed. `meta.json` keeps its structure; the new gate values are
in `metrics.<bucket>.gates` (`pooled_lift_seeds`, `pooled_lift_median`,
`pooled_lift`, `pooled_lift_lo` / `_hi`, `sim_diff`, `sim_diff_lo` / `_hi`,
`collapse_removed`, one `*_ok` per gate, `abs_return_*`, `reasons`, `old`)
and the settings in `thresholds.gate_rules`.

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
baseline (except "wallet reputation, quality only" and "wallet reputation,
last 14 days", which change two each):

| variant | change |
|---|---|
| baseline | none (the saved model's own numbers; not retrained) |
| dead rule on (collapse OR dead) | `DEAD_IS_COLLAPSE = True` |
| raw dex instead of dex_family | `DEX_INPUTS = "raw"` |
| raw dex + dex_family | `DEX_INPUTS = "both"` |
| runners must be tradeable | `RUNNER_NEEDS_TRADES = True`: runner also needs `trades_24h >= RUNNER_MIN_TRADES_24H` (100); NULL keeps the label |
| stronger regularisation | `LGBM_REGULARISATION = "strong"`: `num_leaves` 15 -> 7, `min_child_samples` (min_data_in_leaf) 20 -> 50, `reg_lambda` (lambda_l2) 1 -> 10, `colsample_bytree` (feature_fraction) 0.8 -> 0.6 (`LGBM_REG_SETS`); rounds still from early stopping / the purged k-fold |
| with calls_prev counts (default before 14 Oct) | `EXCLUDED_INPUTS = ()`: the model and the logistic baseline with `calls_prev_1h` and `calls_prev_24h` back in, i.e. the inputs of the saved models up to 14 Oct 2026, so every report compares against the old setting. The baseline leaves them out (see the start of this file). `EXCLUDED_INPUTS` can only remove numeric inputs; an outcome, a categorical or an unknown name raises |
| wallet reputation (wrep_*) | `WALLET_REP = True`: the eight `WREP_FEATURES` of `scout_ml/wallet_rep.py` appended to the numeric inputs (the three shares as is, the count and the lifts log1p-scaled in the logistic baseline). Needs `--live-buys PATH` (the `-export-live-buys` CSV); "n/a" without it. See the `wallet_rep.py` paragraph in "Layout and tests" |
| wallet reputation, quality only | `WALLET_REP = True` and `EXCLUDED_INPUTS` extended by `WREP_COVERAGE` (`wrep_n_known`, `wrep_known_share`): the six quality inputs only (runner lift mean / max, rug lift mean / max, good share, bad share), calls_prev still left out. The two coverage inputs grow with calendar time as wallet history builds up from August, so they act as a time proxy (owner decision 14 Oct 2026, "option b"). Needs `--live-buys PATH`; "n/a" without it |
| wallet reputation, last 14 days | `WALLET_REP = True` and `WREP_LOOKBACK_DAYS = 14` (`WREP_LOOKBACK_VARIANT_DAYS`): the same eight inputs as "wallet reputation (wrep_*)", but a wallet's history counts only outcomes known in the 14 days before the call, so it is a fixed-length window instead of growing with calendar time (owner decision 15 Oct 2026, after report 20261015-1944, where the full wallet variant held adoption rules 1 to 4 at 3day and failed only the drift rule; 14 days fixed before any result, the same as `CALIB_RECENT_DAYS`, one value, no sweep). Needs `--live-buys PATH`; "n/a" without it |

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
collapses removed (counted weeks only), simulation top 10% vs all calls, the
variant's own pooled walk-forward lift and simulation difference with their
95% bootstrap intervals (its seed-7 run only; information; the calls_prev
variant also over every seed, see below), and its
"single-run (old) gate, information" result. The variants are not judged by
the pooled gates of the verdict (that would need each variant retrained with
every seed, three times the cost); the baseline row shows the old single-run
gate too, so the column compares like with like. Lift and runner
AUC of the tradeable variant use its own (stricter) runner label; compare its
runner rate. The logistic baseline uses numeric inputs only, so it is the same
in the DEX variants. All variants are compared on the same test part: picking
the best by these numbers alone risks fitting the test period, so prefer a
variant that also wins in the walk-forward weeks. To adopt a variant, change
the setting in `config.py` and train again (it then becomes the baseline).

After the per-bucket tables, a table **"Variants across seeds"** ("calls_prev
variants across seeds" before 14 Oct 2026; `meta.json`: `"variants"` -> `<name>` -> `"across_seeds"`) shows the baseline
and the `SEED_VARIANTS`, per bucket: the pooled walk-forward lift of each seed
of `STABILITY_SEEDS` and their median (as the runner-lift gate takes it), the
seed-7 pooled lift and simulation difference with their 95% intervals, and
the adversarial AUC (train vs test) on each one's own inputs with its top
three drifting inputs. The variants are retrained with the other seeds of
`STABILITY_SEEDS` (six since 14 Oct 2026, 11 and 23 before); the
baseline row reuses the gates' and diagnostics' numbers. Report only, never a
gate; an error there keeps the variant's other numbers. Empty `SEED_VARIANTS`
to skip it. A `SEED_VARIANTS` entry that is n/a (the wallet variants without
`--live-buys`) gets an "n/a" row. Since 14 Oct 2026 `SEED_VARIANTS` is "with
calls_prev counts (default before 14 Oct)", "wallet reputation (wrep_*)" and
"wallet reputation, quality only";
the "no calls_prev inputs" variant became the
baseline, and the owner rejected "calls_prev as a burst ratio" (worse in
report 20261014-1842), so it was removed. Its setting
`CALLS_PREV_INPUTS = "burst"` is still in `config.py` (the two raw counts
replaced in place by `calls_burst_1h = calls_prev_1h * 24 /
max(calls_prev_24h, 1)`), but no variant uses it. Since 15 Oct 2026
`SEED_VARIANTS` also has "raw dex + dex_family" (owner decision: in report
20261015-0554 its seed-7 pooled walk-forward lift at 7d was 2.34 vs the
baseline's 1.95, sim difference +41.2 %, a gap of the size single seeds have
shown as noise; the median over the seeds tells which). It is not a wallet
variant: it runs without `--live-buys` and has no adoption rules. Since
15 Oct 2026 `SEED_VARIANTS` also has "wallet reputation, last 14 days" (its
adoption rules need its median over the seeds).

A variant that raises an error is recorded as `error: <message>` (in its
table row and in `meta.json` `"variants"`), a `WARNING` line is printed, the
other variants still run and `train.py` still exits 0: the baseline is saved
before any variant starts. If every variant fails, a `WARNING: all N variants
failed` line says so.

Runtime: each variant costs about one training of the normal model (on
synthetic data of the fixture's size, 6,000 calls over 170 days: about 25 s,
the dead score about 2 s of it; a whole run with `--variants` took about
2 min before the gates of 13 Oct 2026, whose seed retrains add about 47 s to
every run, see "Stability diagnostics"). The across-seeds table adds one
retrain per extra seed (two with 3 seeds, six with 7) plus one adversarial
probe per bucket for each `SEED_VARIANTS` entry
(the baseline row reuses the diagnostics' probe): about 93 s for two
entries on that synthetic data (13 Oct 2026, the sum of their `[variant]`
lines' "of which" times; three entries since 14 Oct 2026, five since 15 Oct, see below); on the prod data an
estimated 40 to 50 s for two, not measured. Runs without `--variants` are unchanged.
With 7 seeds (14 Oct 2026, same synthetic data, `make_synthetic.py --calls
6000 --days 170 --seed 11`, one desktop PC): a whole run with `--variants`
took 453 s instead of 280 s; the gates' seed retrains 134 s instead of 47 s,
and the calls_prev variant's other seeds and adversarial validation 139 s
instead of 48 s. The wallet reputation variant (with `--live-buys`) adds its
own run, six more seed retrains and an adversarial probe per bucket
(14 Oct 2026, same synthetic data plus `make_synthetic.py --live-buys`,
17,359 live buys): its `[variant]` line 170 s, of which other seeds and
adversarial validation 147 s; computing the wallet reputation itself took
under 1 s; a whole run with `--variants --live-buys` 614 s instead of 453 s.
The variant "wallet reputation, quality only" costs as much again (14 Oct
2026, same data and PC: its `[variant]` line 165 s, of which other seeds and
adversarial validation 140 s; the full wallet variant's line 164 s in the
same run); a whole run with `--variants --live-buys` took 759 s.
"raw dex + dex_family" in `SEED_VARIANTS` (15 Oct 2026, same synthetic data,
`make_synthetic.py --calls 6000 --days 170 --seed 11 --live-buys`, one
desktop PC, synthetic, not the prod data): its `[variant]` line 154 s instead of 22 s, of
which other seeds and adversarial validation 132 s; a whole run with
`--variants --live-buys` 891 s instead of 773 s (the run before the change,
same day). "wallet reputation, last 14 days" (15 Oct 2026, same synthetic
data and command, one desktop PC, synthetic, not the prod data): its
`[variant]` line 191 s, of which other seeds and adversarial validation
163 s; computing its 14-day wallet reputation took under 1 s; a whole run
with `--variants --live-buys` 1,142 s instead of 944 s (the same run with
that variant left out, same day).

### Stability diagnostics

Every `train.py` run also writes a section "Stability diagnostics" near the
end of `report.md` (before the variant comparison; `meta.json`:
`"diagnostics"`), to tell whether a result is real or noise. Nothing in it is
saved or served. Since 13 Oct 2026 two of its inputs are the gates' inputs
(see "Gates"): the seeds' pooled walk-forward lifts and the pooled expanding
walk-forward with its bootstrap. They are computed in every training, before
`meta.json`, whether the section is on or off, and the section reuses them
(nothing is retrained twice). The rest is report only and never a gate. The
saved model, its `meta.json` fields, the gates, the verdict and everything
above the section are the same with it on or off (`tests/test_diagnostics.py`
checks this). Settings: the "Stability diagnostics" block of
`scout_ml/config.py`.

- **Seed stability**: the baseline retrained with each LightGBM seed in
  `STABILITY_SEEDS` (7, 11, 23, 31, 47, 61, 89; 7 is the saved model's
  `LGBM_SEED`, its numbers are reused). Owner decision of 14 Oct 2026: 7
  seeds instead of 3 (7, 11, 23), because in reports 20261014-1842 and
  20261014-2211, on almost the same data, the median pooled lift over 3 seeds
  moved by up to ±0.4 and buckets near the threshold of 2 flipped PASS / FAIL
  from run to run; a median over 7 seeds is steadier. Per bucket and seed: test top-10% lift with its
  interval, collapses removed, runner ROC AUC, simulation top 10% vs all,
  counted walk-forward weeks beating buy-all, pooled walk-forward lift (its
  median over the seeds is the runner-lift gate) and the run's old single-run
  gate (information); then min / median / max over the seeds. A lift or a
  PASS/FAIL that moves between seeds is noise.
- **Pooled walk-forward (expanding and rolling)**: the test rows of all
  counted walk-forward weeks of a bucket pooled. Each week is ranked by its
  own model (top 10% / top 30% per week, as the gate does; the week models'
  scores are not on one scale), so pooled lift = runner rate in the per-week
  tops / runner rate of all pooled rows. 95% interval by a week-block bootstrap
  (`POOLED_BOOTSTRAP_REPS` = 2000 resamples of the weeks with replacement, seed
  `POOLED_BOOTSTRAP_SEED` = 7; n/a with one week, rough with few). Also pooled
  collapses removed, pooled simulation top 10% vs all (with an interval on the
  difference) and weeks beating buy-all. The expanding row is the gates'
  input (its lift and simulation-difference lower ends). Next to the expanding
  walk-forward, a rolling one trains each week's model on the last
  `WF_ROLLING_WEEKS` (4) weeks of calls before the week only (minus the
  embargo; same rounds and test weeks), with a per-week table of both. Both
  are pooled over the same weeks, the ones the expanding walk-forward counts,
  minus any week the rolling model could not train (marked in the per-week
  table) (a paired comparison; the rolling window's own training size, shown in the
  table, is not held to the `WF_MIN_TRAIN_ROWS` rule, it is small by design).
- **Evaluation without dead calls** (`EVAL_EXCLUDE_DEAD`, on): the same
  trained models (saved model on the test part, the gate's week models),
  metrics recomputed on the calls that are not dead (`trades_24h < 50`; NULL
  is kept), top shares re-taken among them. Training rows, labels and gates
  are unchanged. n/a without a `trades_24h` column.
- **Adversarial validation**: a LightGBM classifier (`ADV_PARAMS`) told to
  separate the train part from the test part on the model's own inputs
  (`build_features`, so the leakage guard holds; without the calendar inputs
  `ADV_EXCLUDE` = `hour_utc`, `weekday_utc`, which differ by construction when a
  part covers only part of a week). Out-of-fold ROC AUC (`ADV_FOLDS` = 5,
  stratified, shuffled with a fixed seed; about 0.5 = no drift, above 0.7 =
  clear drift) and the top drifting features by gain; also per counted
  walk-forward week (`ADV_PER_WEEK`; the newest `ADV_MAX_WEEKS` = 6 weeks,
  `ADV_WEEK_FOLDS` = 3).

Switch: on by default (`DIAGNOSTICS = True`); `SCOUT_ML_DIAGNOSTICS=0` (or
`off`, `false`, `no`) in the environment switches the section off for one run
(the seed retrains and the pooled walk-forward still run for the gates; only
the rolling walk-forward, the evaluation without dead calls and adversarial
validation are skipped), e.g.
`SCOUT_ML_DIAGNOSTICS=0 python train.py --csv calls.csv --out models/`
(`run_training.sh` passes the environment through; it needs no change). A part
that raises is shown as "Not computed: error: ..." with a `WARNING` and the
others still run; the saved model is written before the section starts.

Runtime: on synthetic data of the fixture's size (6,000 calls over 170 days,
every bucket trained, one desktop PC, 13 Oct 2026): 26 s before the gate
decision with the section off, 73 s now with it off (the seed retrains for the
gates: 47 s, two retrains of the baseline; pooled walk-forward under 1 s) and
102 s with it on, about as before (rolling walk-forward 11 s, adversarial
validation 18 s, evaluation without dead calls under 1 s). So a run with the
section off now costs about two normal runs more; on the prod data (about 6 s
per variant in report 20261012-2209, `long` still skipped) expect roughly 20
to 30 s more per run then. `--variants` adds two more variant rows ("stronger
regularisation", "with calls_prev counts (default before 14 Oct)"), about one
normal run each, and the across-seeds table (two more runs for the calls_prev
variant; see "Variant comparison"). Since 14 Oct 2026 (7 seeds) the seed
retrains are six runs of the baseline, not two: on the same synthetic data
134 s instead of 47 s (about 22 s per seed), so a run with the section on
takes about 187 s instead of 102 s; the across-seeds table then costs six
more runs for the calls_prev variant (139 s instead of 48 s). On the prod
data expect roughly 3 times the earlier extra time, not measured.

The `model.py`, `validate.py`, `features.py` (`MIN_CATEGORY_COUNT`) and
`labels.py` (`MAX_OUTCOME_PCT`) settings are read from `config` when a function
runs, not bound at import, so every knob (`C.overrides`, the variants, the
seeds) takes effect.

### Collapse filter

Asked by the owner on 15 Oct 2026: the evidence for presenting the model as a
risk filter ("avoid likely collapses") instead of "top picks". Each trained
bucket's section of `report.md` has a "Collapse filter" table after its
walk-forward table. It is never a bucket gate, is not served, and does
not change the saved model, the gates or `passed`.

**Verdict (owner decision of 18 Oct 2026).** The model is presented as a risk
filter. In `3day` and `medium` (`FILTER_BUCKETS`; not `short`) the filter is
"validated" when it beats random skipping on the pooled walk-forward: the
lower end of the 95% interval of the pooled "collapses caught" is above 0.30
(the share a random skip of 30% catches) AND the lower end of the pooled
"kept minus all (mean net %)" is above 0 (`FILTER_RULES`; literal values).
Fewer than `GATE_MIN_POOLED_WEEKS` (2) counted weeks, a failed pooled
walk-forward or any undefined value: not validated, with the reasons. The
rule was set after seeing report 20261018-2204 (`FILTER_RULES_NOTE`, printed
in the report); every training run since then computes the verdict, with
no human step (retraining is weekly and automatic). The report has a
"Collapse filter verdict (owner rule
of 18 Oct 2026)" table under the bucket verdict (bucket, validated, the two
lower ends, counted weeks, cut-off, reference rows, test-part flagged share
at the cut-off), the reasons of each filter bucket that is not validated, and
a verdict line in each bucket's "Collapse filter" section. Code:
`validate.filter_verdict` (pure), `train.apply_collapse_filter`.

**Fixed cut-off.** A live filter needs a score threshold set in advance. Each
training run takes it as the 70% quantile (`1 - SKIP_FRAC`) of the saved
collapse model's probabilities (its own `predict`, the scoring service's
`collapse_prob` unrounded) on the bucket's pre-test held-out rows: the usable
rows posted after the saved model's training rows end and before its test
part starts (`3day`, `short`: `[t1 - horizon, t2)`, the validation part with
both embargo gaps; `medium`, `long`: `[t_train_end, t_test)`, the embargo
gap). Only inputs are used, never outcomes, never test rows. Fewer than
`FILTER_CUTOFF_MIN_ROWS` (100) rows, or no collapse model: no cut-off. Code:
`train.cutoff_rows`, `train.collapse_reference_block`,
`validate.collapse_cutoff`. For `3day` (and `short`) the reference rows are
mostly the validation part, whose outcomes chose the early-stopping rounds and
fitted the Platt calibrator, so the cut-off's probability value is in-sample
for the calibrator there (only inputs are used; which rows form the top 30% of
the reference set does not depend on the calibrator); `medium` uses the
embargo gap, which no fit saw. The check that the cut-off carries over is the
test part's flagged share at the cut-off (`test_at_cutoff`; near 0.30 = it
carries over). A recent-window reference is a possible later refinement.
The section's column "test part at the fixed
cut-off (information)" flags the test calls whose score is at or above it:
the share a live filter would flag. Re-measuring the walk-forward at a fixed
cut-off is a later change.

Flagged = the `SKIP_FRAC` (30%) calls with the highest collapse score, the
same calls "collapses removed" counts (`top_mask`); kept = the others.
Collapse = the plain collapse label, as the gate. For (a) the saved model's
test part and (b) the pooled expanding walk-forward (the gate's counted weeks,
each week flagged by its own model):

- calls and flagged calls (share);
- collapses caught = collapses removed (for (a) the gate value, for (b) the
  pooled collapses removed of "Pooled walk-forward", same 95% week-block
  bootstrap);
- collapse rate among the flagged vs the kept calls;
- runners lost = the share of all runners that are flagged, and the runner
  rate flagged vs kept;
- mean net return (each call capped at `SIM_MAX_RET_PCT`, +1,000 %, as the
  money simulation) of the kept calls vs all calls and the difference; for
  (b) with its 95% week-block bootstrap interval (the same resamples as the
  gates);
- random skip for comparison: skipping 30% at random catches 30% of the
  collapses and loses 30% of the runners on average, and leaves the mean
  unchanged.

The 30% cut is taken within each part (the test part, each walk-forward week)
after its scores are known, as for "collapses removed"; a live filter would
need a fixed score threshold set in advance, so these rates are likely
optimistic for such a filter (the fixed cut-off column above is the
information on that).

Then a per-week table for (b) (week, flagged/calls, collapses caught, runners
lost, kept vs all mean) to show the trend over time. `short` (1d): the runner
label is the peak (`max_gain_late_1d >= 100%`) and the collapse label the
return (`ret_late_1d <= -50%`), so one call can be both; `3day` and `medium`
read the same return column for both, so no call is both (`long`: a rugged
call counts as a collapse, so it can be both too). The section says which
applies.

Computed in every training, also with `SCOUT_ML_DIAGNOSTICS=0`, from the
arrays the gates already use (nothing is retrained; well under a second).
An error in it shows "Not computed: error: ..." with a `WARNING`, leaves no
verdict (= not validated) and changes nothing else; an error in the cut-off
records `collapse_reference.<bucket>` as `{"error": ...}` with a `WARNING`
and never stops the saved model. Code: `validate.collapse_filter_sums` /
`collapse_filter_rates` / `collapse_filter_stats` (pure), the filter columns
of `diagnostics.week_sums` and `diagnostics.pooled(..., with_filter=True)`,
`train.collapse_filter`. Tests: `tests/test_collapse_filter.py`,
`test_collapse_filter_pooled_is_the_pooled_collapses_removed` in
`tests/test_diagnostics.py`, and the `collapse_reference` / cut-off tests in
`tests/test_pipeline.py`.

`meta.json`: `metrics[<bucket>]["collapse_filter"]` (plain numbers; null =
undefined, e.g. no collapses):

- `flag_frac`, `random_skip_expected` (both `SKIP_FRAC`), `sim_max_ret_pct`,
  `runner_label`, `collapse_label` (text), `can_be_both` (bool);
- `test`, `pooled` and each entry of `weeks` (which also has `week` and
  `start`) hold the counts `calls`, `flagged`, `collapses`,
  `collapses_flagged`, `runners`, `runners_flagged` and the rates
  `flagged_share`, `collapses_caught`, `collapse_rate_flagged`,
  `collapse_rate_kept`, `runners_lost`, `runner_rate_flagged`,
  `runner_rate_kept`, `all_mean`, `kept_mean`, `kept_minus_all` (mean net %,
  capped);
- `pooled` also has `weeks`, `reps`, `seed` and `<rate>_lo` / `<rate>_hi`
  (95% week-block bootstrap) for every rate;
- `test` is `{"skipped": "no collapse model"}` when the bucket has no
  collapse model (a week likewise), `pooled` is `{"error": ...}` when the
  pooled walk-forward failed, and the whole block is `{"error": ...}` when
  it failed (then there is no `verdict`: not validated);
- `verdict` (`validate.filter_verdict`): `applies` (a `FILTER_BUCKETS`
  bucket), `validated`, `weeks`, `min_weeks`, `caught_lo`,
  `min_caught_lower`, `caught_ok`, `kept_minus_all_lo`,
  `min_kept_minus_all_lower`, `kept_ok`, `reasons`, `set_on`, `seen_report`.
  Later changes read `verdict.validated`;
- `test_at_cutoff`: `cutoff` and the counts and rates above on the test part
  with flagged = score >= the fixed cut-off (information), or
  `{"skipped": reason}` without a cut-off.

`meta.json` also has (both read by later changes, keep the paths):

- `collapse_reference[<bucket>]` for every trained bucket: `cutoff` (the
  probability), `quantile` (0.7), `n`, `from`, `to` (posting dates of the
  reference rows), `rows` ("pre-test held-out"), `calibrated` (bool),
  `quantiles` (`N_REF_QUANTILES` = 101 quantiles of the same scores, like
  `runner_reference`); or `{"skipped": reason}` / `{"error": ...}`;
- `thresholds.filter_rules`: `set_on`, `seen_report`, `note`, `buckets`,
  `min_caught_lower`, `min_kept_minus_all_lower`, `flag_frac`,
  `min_pooled_weeks`, `cutoff_min_rows`.

A `meta.json` without these keys (models trained before 18 Oct 2026) still
loads and scores as before.

## Read the report

`models/<version>/report.md`, top to bottom:

1. **Verdict** - PASS/FAIL per bucket by the gates of 13 Oct 2026 (see
   "Gates"): the median pooled walk-forward lift over the seeds (with each
   seed's value), the lower end of the saved seed's pooled-lift interval, the
   pooled simulation difference with its interval, collapses removed on the
   test part; then, as information only, the absolute return (pooled top-10%
   mean net return) and the two old gates ("old gate, information only":
   test-part lift >= 2, beats buy-all in every counted week, with the weeks
   not counted). Each FAILing bucket gets a line with its reasons. Under it,
   the "Collapse filter verdict (owner rule of 18 Oct 2026)" table (see
   "Collapse filter" above; never a gate). Each
   top-10% lift of the test part and of every walk-forward week also comes
   with an approximate 95% interval (Wilson interval of the runner rate in the
   top 10%, divided by the base rate) and the runner counts in the top 10% and
   overall.
2. **Data and exclusions**, **Feature coverage** - how much data there was and
   which columns are mostly empty (coverage is measured on the calls left
   after the exclusions).
3. Per bucket: class balance (with warnings for labels under 10% / over 90%
   and for skipped models), test metrics for LightGBM and the logistic
   baseline (ROC AUC, PR AUC, Brier), the gate values, the money simulation,
   the walk-forward table, the "Collapse filter" table (see
   "Collapse filter" above), calibration by decile, top features. `medium` and
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
5. **Stability diagnostics** (not saved or served; the seeds' pooled lifts
   and the pooled expanding walk-forward are the gates' inputs, the rest is
   report only; see "Stability diagnostics" above): seed stability, pooled
   and rolling walk-forward, evaluation without dead calls, adversarial
   validation.
6. With `--variants` only: **Variant comparison**, one table per bucket, then
   "Variants across seeds" (see "Variant comparison" above), then "Wallet
   reputation data" and the wallet variant's pre-registered adoption rules
   (see the `wallet_rep.py` paragraph in "Layout and tests").

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
`SCOUT_MODEL_LINE` (`passed`, the default, or `all`) selects which buckets the
delivery line names (below).

    GET  /health  -> {"ok": true, "model_version": "20261002-1530"}
    POST /score   <- {"row": { ...one scout_call_dataset_v row as JSON... }}
                  -> {"model_version": "...", "line": "Model ... · 7d: runner 12%, collapse 64%",
                      "buckets": [{"bucket": "short", "passed": false, "runner_prob": 0.31,
                                   "collapse_prob": 0.44, "runner_rank_pct": 92.5}, ...]}

Nulls, missing columns and unknown keys are fine (treated as missing).
Outcome columns in the row are ignored. `buckets` has every bucket with a
trained runner model (a skipped bucket, e.g. `long` before its data has
matured, is left out), whether or not it passed its gates, so all scores can be
stored and checked live; `passed` is true only when `meta.json` records
`metrics.<bucket>.gates.passed = true` (the gates of 13 Oct 2026 for a model
trained since; a missing entry, e.g. an older `meta.json` without gate info,
counts as not passed). Without a collapse model
`collapse_prob` is null.

`SCOUT_MODEL_LINE` chooses which buckets `line` names, by horizon (`1d`, `3d`,
`7d`, `30d`), in that order, after the model version:

- `passed` (the default, also when unset): only the buckets that passed their
  gates, e.g. with only `medium` passed:

      Model 20261010-0621 · 7d: runner 12%, collapse 64%

  When no bucket passed, `line` is `""` and the listener appends nothing.
- `all`: every bucket in `buckets` (a skipped bucket, with no model, never
  appears). A bucket that did not pass is marked `(not validated)` right after
  its horizon; a passing bucket looks as above. With only `medium` passed:

      Model 20261010-0621 · 1d (not validated): runner 31%, collapse 44% · 3d (not validated): runner 9%, collapse 71% · 7d: runner 12%, collapse 64% · 30d (not validated): up 55%, collapse 20%

  When no bucket passed, every part is marked; `line` is `""` only when no
  bucket has a model.

Case and surrounding spaces are ignored; any other value counts as `passed`
and logs one warning (per distinct value). The service reads the variable on
each request, but a running process does not see a changed environment:
changing it takes a restart of the scoring service only (no retrain, no
`LATEST` change, no listener restart). The mode changes only `line`, never a
score or `passed`, and scores never filter deliveries in either mode. The
marker and the values are in `scout_ml/config.py` (`LINE_*`).

The owner decided on 12 Oct 2026 to run prod with `SCOUT_MODEL_LINE=all`: he
is the only reader of the private delivery group, so he sees every bucket's
numbers, with the unvalidated ones marked. The default stays `passed` for any
other setup.

Each bucket is scored with the category levels its models were trained with (stored per
bucket in `meta.json`).
`runner_rank_pct` is where the runner score falls among the scores of the
test-period calls at training time (0-100). Both endpoints return 503 while
no model has been trained.

## Use from the Go listener

Start the service, then run the listener with

    SCOUT_MODEL_URL=http://127.0.0.1:8601

It posts the row of a new call to `/score` and can append `line` to its message
(nothing when `line` is empty: with the default `SCOUT_MODEL_LINE=passed` when
no bucket passed its gates, with `all` only when no bucket has a model). Scores
never filter deliveries.

## Retrain

Export again and run `train.py` again (weekly is reasonable, since new
outcomes arrive as horizons complete). A new version directory is written and
`LATEST` is updated; the running service picks it up on the next request. A
`--candidate` run (see "Train") leaves `LATEST` alone; serve it with
`promote.py promote candidate` (below). If `train.py` stopped with an error,
`CANDIDATE` still names the previous candidate (or does not exist), so check
that `status` shows the version a successful run printed on its last line
(`wrote <dir> ...`) as the candidate. Compare the new `report.md` with the
previous one before trusting it. To roll back, use `promote.py rollback`, or
`promote.py promote <version>` for an older one.

### Promote, roll back, status

    python promote.py --models models/ status
    python promote.py --models models/ promote candidate     # or a version name
    python promote.py --models models/ rollback

`status` prints `LATEST` (live), `PREVIOUS` and `CANDIDATE` (or `none`), then
the newest version directories (10 by default, `--limit N`), each with
`data.last_date` and every bucket's gate verdict from `meta.json` (`PASS`,
`FAIL` or `skipped`, never the reasons; read `report.md` for those), marked
`[live]`, `[previous]` or `[candidate]`.

`promote` first checks the version: its directory exists, `meta.json` parses
and names that version, every model file `meta.json` lists is there,
`report.md` is there, and the serving code loads it and scores a fixed
made-up row (`PROMOTE_SAMPLE_ROW` in `config.py`) with finite numbers. If any
check fails it prints `refused: <why>` and changes nothing. Then it writes
`PREVIOUS` (the old `LATEST`), then `LATEST`, both atomically, and appends one
line to `models/promotions.log`, e.g.
`2026-10-19T05:12:03Z promote from=20261012-0702 to=20261019-0500 by=manual`
(UTC; `--by` names who promoted, default `manual`). The running service
serves the new version from its next request, with no restart.

`rollback` promotes the version in `PREVIOUS` with the same checks; `PREVIOUS`
then names the version it rolled away from, so a second `rollback` goes back
again. `CANDIDATE` is never changed by either command.

Exit codes: `0` done; `1` refused, nothing changed; `2` usage error (also a
bad `--by`); `3` the version is already `LATEST`, nothing changed; `4` a file
error while reading or writing the pointers: it prints
`error: <error type> while moving the pointers; LATEST is X (unchanged), PREVIOUS is Y (was Z)`,
so you see which pointer moved. Fix the cause, run `status`, then promote or
roll back again.

## Layout and tests

    scout_ml/config.py    buckets, thresholds, feature lists, forbidden columns, gates
    scout_ml/features.py  build_features(): the one function used by training AND serving
    scout_ml/labels.py    net-of-tax labels and usable-row rules
    scout_ml/validate.py  time splits, embargo, purged k-fold, token grouping, walk-forward, metrics
    scout_ml/model.py     LightGBM + calibration, logistic baseline, serving bundle
    scout_ml/report.py    report.md
    scout_ml/diagnostics.py  pooled walk-forward + bootstrap (gate inputs), seed spreads, adversarial
    scout_ml/wallet_rep.py   live-buy wallet reputation (wrep_* columns; a report-only variant)
    scout_ml/promote.py   promote / rollback / status checks (promote.py is its command line)
    train.py  serve.py  promote.py  make_synthetic.py  tests/

    python -m pytest tests -q        # 399 tests: 399 passed on Linux; on Windows 398 passed, 1 skipped (POSIX-only), about 7 minutes

`scout_ml/wallet_rep.py` (`wrep_frame(df, live_buys)`) scores the wallets in
each call's live buys (the `-export-live-buys` CSV) by how the earlier first
calls they bought into turned out at 7 d, counting only outcomes known before
the call and never the call's own token (settings `WREP_*` in `config.py`).
It is wired in as a report-only variant (14 Oct 2026): `train.py --variants
--live-buys PATH` computes it once per distinct `WREP_LOOKBACK_DAYS` the
wallet variants use (`wallet_rep_frames`; a `[wallet rep] ...` line each, `[wallet
rep, last 14 days]` for the window) and joins the `wrep_*` columns to the
rows of the wallet variants only, each with its own lookback
(`variant_data`; also in its seed retrains and adversarial validation), for
the `WALLET_REP_VARIANTS` "wallet reputation (wrep_*)" (`WALLET_REP = True`,
all history), "wallet reputation, quality only" (the same without the two
coverage inputs `WREP_COVERAGE`) and, since 15 Oct 2026, "wallet reputation,
last 14 days" (`WREP_LOOKBACK_DAYS = 14`: only outcomes known in the 14 days
before the call); all three are retrained with every seed, see "Variant
comparison". The saved model, its gates, the dead score and the
diagnostics use the rows without them, so they and the model files are the
same with or without `--live-buys` (a test compares them byte for byte);
`train()` raises if a `wrep_*` name is in the configured `FEATURES` (serving
cannot compute them). Without `--live-buys`, or when the CSV cannot be read
(`WARNING: the wallet reputation failed`), or when it has no live buys for
the calls (header only or zero rows: every `wrep_*` input empty), the wallet
variants are "n/a", with their reason, also in the adoption block (when only
the 14-day computation fails, only that variant is).
`run_training.sh --variants` exports the CSV (`-export-live-buys`, RUNBOOK
step 4b) when the Go program has the flag. `meta.json` `"wallet_rep"` and
the block "Wallet reputation data" give the counts, the observed 7 d runner
and rug rates next to the fixed priors and the coverage per week (all
history; one line and one more coverage column, under `"lookbacks"`, for
the 14-day window); the block
after it prints the pre-registered adoption rules (`WREP_ADOPTION`, fixed on
14 Oct 2026 before any result) and per bucket and wallet variant whether each
holds, in one table (report only, never a gate; `meta.json` `"variants"` ->
each wallet variant -> `"adoption"`). The same rules apply to every wallet
variant, each against the same baseline. A variant passes in a bucket only
if all hold: across-seeds median pooled
walk-forward lift >= 2.0 and >= the baseline's; the seed-7 pooled lift's 95%
lower end > 1.0; the pooled simulation difference's 95% lower end > 0;
collapses removed >= 0.40; and its adversarial AUC no more than 0.02 above
the baseline's with no `wrep_*` input among the top 3 drifting inputs at a
gain share >= 0.40. When only that drift rule fails, the next step is a
lookback variant (`WREP_LOOKBACK_DAYS`), not adoption; for a variant that
already uses a lookback, the result reads "not adopted; drift persists with a
14-day window" (the days from its own setting; no further step is
pre-registered).
`make_synthetic.make_live_buys` writes matching invented live buys
(`make_synthetic.py --live-buys PATH` writes them as a CSV).

`build_features` raises if an outcome or bookkeeping column (anything starting
with `ret_`, `max_gain_`, `max_dd_`, `trades_`, `peak_late_after_`, `peak_after_`,
`first_2x_`, `above_2x_`, `fall_below_2x_`, `timing_`, `rug_at`, plus `rugged`,
`tracking_status`, `trades_24h`, ...) would become a model input.
