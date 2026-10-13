"""Everything a human may want to tune lives here: buckets, label thresholds,
feature lists, forbidden (leaky) columns, model settings and pass gates."""
from contextlib import contextmanager

# --- Label definitions -----------------------------------------------------
# Each label is (outcome column, comparison, threshold in percent). The outcome
# is first netted of buy+sell tax (see labels.net_pct). `ret_col` is the return
# used by the money simulation; `horizon_days` drives the embargo and names the
# bucket in the delivery line ("7d: runner 12%, collapse 64%", with `runner_word`).
BUCKETS = {
    "short": {"title": "short", "runner_word": "runner", "horizon_days": 1,
              "runner": ("max_gain_late_1d", ">=", 100.0),
              "collapse": ("ret_late_1d", "<=", -50.0),
              "collapse_or_rugged": False, "ret_col": "ret_late_1d"},
    "3day": {"title": "3-day", "runner_word": "runner", "horizon_days": 3,
             "runner": ("ret_late_3d", ">=", 50.0),
             "collapse": ("ret_late_3d", "<=", -60.0),
             "collapse_or_rugged": False, "ret_col": "ret_late_3d"},
    "medium": {"title": "medium", "runner_word": "runner", "horizon_days": 7,
               "runner": ("ret_late_7d", ">=", 50.0),
               "collapse": ("ret_late_7d", "<=", -70.0),
               "collapse_or_rugged": False, "ret_col": "ret_late_7d"},
    "long": {"title": "long", "runner_word": "up", "horizon_days": 30,
             "runner": ("ret_late_30d", ">", 0.0),
             "collapse": ("ret_late_30d", "<=", -90.0),
             "collapse_or_rugged": True, "ret_col": "ret_late_30d"},
}
LABELS = ("runner", "collapse")

# --- Delivery line (serving) -------------------------------------------------
# Which buckets /score puts in `line`; chosen per request by the scoring
# service from the SCOUT_MODEL_LINE environment variable. "passed" (default):
# only buckets whose gates passed. "all": every bucket with a runner model,
# a bucket that did not pass marked with LINE_NOT_VALIDATED right after its
# horizon ("3d (not validated): runner 9%, collapse 71%"). Any other value
# counts as the default. Scores never filter deliveries in either mode.
LINE_MODE_ENV = "SCOUT_MODEL_LINE"
LINE_MODES = ("passed", "all")
LINE_MODE_DEFAULT = "passed"
LINE_NOT_VALIDATED = "(not validated)"
NO_POOL_STATUSES = ("no_pool", "gave_up")  # calls we could never have traded
REPEAT_STATUS = "repeat"  # a later call of a token: the tracker follows the first call only
UPDATE_KIND = "update"  # post_kind of a "$TOKEN hit 3X ..." post about an earlier call: not a call

# --- "Dead after the call" ---------------------------------------------------
# trades_24h (view column, an OUTCOME): price events in the pool during the 24 h
# after the call (5-minute candles; on a v2 pool every Sync counts, which includes
# liquidity added or removed). A call with fewer than DEAD_TRADES_24H is "dead":
# bought, then nobody traded it. Dead calls stay in the data (they were real
# buys). With DEAD_IS_COLLAPSE = True the collapse label of EVERY bucket becomes
# "collapse OR dead"; a row whose trades_24h is NULL (not priced on-chain, or
# its first 24 h not scanned yet) keeps the plain collapse label.
# Off by default since report 20261009-1904: with the rule off the collapse
# ROC AUC on the plain label was higher in every bucket and collapses removed
# about equal (the "dead rule on" row of --variants keeps comparing).
# The gates do not change: "collapses removed" (GATES) is always measured on
# the plain collapse label; the share of collapse OR dead removed is reported
# next to it as information only (whenever the data has trades_24h).
DEAD_COLUMN = "trades_24h"
DEAD_TRADES_24H = 50
DEAD_IS_COLLAPSE = False

# --- "Dead after the call": separate score (report only) -----------------------
# One extra model (LightGBM + logistic baseline) for the label dead =
# trades_24h < DEAD_TRADES_24H, trained once, not per bucket: the same inputs
# (FEATURES) and the same time split as the DEAD_SCORE_SPLIT_BUCKET bucket (its
# usable rows; train / validation / test; embargo = its horizon, 1 day), keeping
# only the rows whose trades_24h is known (NULL rows are left out, never
# negatives). Platt calibration on the validation part as for the other labels.
# Report section and meta.json "dead_score" only: never saved, never served,
# no gates. Walk-forward weeks with fewer than DEAD_SCORE_MIN_WEEK_DEAD dead
# calls are marked (too few to judge).
DEAD_SCORE_SPLIT_BUCKET = "short"
DEAD_SCORE_TOP_FRACS = (0.10, 0.30)
DEAD_SCORE_GATE_FRAC = 0.10            # the "top-10%" share; must be in DEAD_SCORE_TOP_FRACS
DEAD_SCORE_MIN_WEEK_DEAD = 5
# Report caveat: when the DEAD_WAVE_TOP_WEEKS weeks with the most dead calls
# hold more than DEAD_WAVE_SHARE of all dead calls, the report says they came
# in a few waves (and names the most common dex_family among those dead calls).
DEAD_WAVE_TOP_WEEKS = 2
DEAD_WAVE_SHARE = 0.5
DEAD_SCORE_N_COEF = 10                 # largest logistic coefficients shown
# A PROPOSAL for the product manager, not a gate (nothing passes or fails on
# it): top-10% precision >= this many times the base rate in the test part,
# and in more than half of the walk-forward weeks with at least
# DEAD_SCORE_MIN_WEEK_DEAD dead calls. The report shows how this run fares.
DEAD_SCORE_PROPOSED_LIFT = 3.0

# --- Columns of scout_call_dataset_v, in view order ------------------------
_WINDOWS = ("5m", "15m", "60m")
HORIZONS = ("1h", "1d", "3d", "7d", "30d")
IDENTITY = ["call_id", "message_id", "message_date", "contract_address",
            "call_status", "token_symbol"]
# View columns that are categorical model inputs (or, for `dex`, the source of one).
CATEGORICAL_VIEW = ["dex", "launchpad", "quote_asset", "perceptor_verdict"]
PRE_VOL = [f"pre_{s}_vol_{w}" for s in ("buy", "sell") for w in _WINDOWS]
# Numeric columns of the view, in view order (builds VIEW_COLUMNS below).
VIEW_NUMERIC = (
    ["called_at_mcap_usd", "mcap_usd", "liq_usd", "liq_pct", "tax_buy_pct",
     "tax_sell_pct", "age_seconds", "holders", "proof_elite", "proof_good",
     "live_buys_elite_count", "live_buys_good_count", "live_buys_elite_usd",
     "live_buys_good_usd", "live_buy_max_usd", "prior_calls",
     "secs_since_prev_call", "calls_prev_1h", "calls_prev_24h"]
    + [f"pre_{k}_{w}" for k in ("swaps", "buys", "sells") for w in _WINDOWS]
    + PRE_VOL
    + [f"pre_price_chg_{w}_pct" for w in _WINDOWS] + ["pre_first_trade_age_s"])
# View columns that are known at the call (not leaky) but carry no information
# for the rows the model trains on: training uses first calls only, and for a
# first call `prior_calls` is always 0 and `secs_since_prev_call` always NULL
# (the view counts / takes max() over earlier calls of the same address). They
# stay in the view and in VIEW_COLUMNS, but are never model inputs.
UNUSED_VIEW_COLUMNS = ["prior_calls", "secs_since_prev_call"]
# Raw numeric model inputs.
NUMERIC_RAW = [c for c in VIEW_NUMERIC if c not in UNUSED_VIEW_COLUMNS]
OUTCOMES = [f"{m}{late}_{h}" for h in HORIZONS for late in ("", "_late")
            for m in ("ret", "max_gain", "max_dd")]
BOOKKEEPING = ["tracking_status", "pool_address", "pool_dex", "entry_price_usd",
               "entry_price_source", "price_unit", "quote_asset",
               "entry_late_price_usd", "current_liquidity_usd", "rugged"]
# latest_* columns the view exports (latest_trade_at is not exported).
LATEST_VIEW = ["latest_price_usd", "latest_return_pct", "latest_checked_at"]
# Outcomes the view appends after latest_* (known only after the call): never
# features, never label thresholds in percent (trades_24h is a count).
AFTER_CALL_VIEW = ["trades_24h"]
# Exactly the columns of scout_call_dataset_v (scoutanalytics.sql), in order;
# tests/test_view_columns.py compares this list with the SQL.
VIEW_COLUMNS = (["call_id", "message_id", "message_date", "contract_address", "call_status",
                 "post_kind", "token_symbol", "token_name", "dex", "launchpad"] + VIEW_NUMERIC
                + ["pre_vol_unit", "perceptor_verdict"] + BOOKKEEPING + OUTCOMES + LATEST_VIEW
                + AFTER_CALL_VIEW)

# --- Leakage guard ---------------------------------------------------------
# Outcomes and bookkeeping are only known after the post; ids are not signal.
# quote_asset is deliberately NOT here (allowed as a categorical feature).
# token_name (the token contract's own name, for the website) is display text.
# post_kind (call | update) only decides which rows are used at all.
# latest_* (the price and return as of the tracker's most recent look) are
# outcomes that keep moving after the call: never features.
LATEST = ["latest_price_usd", "latest_return_pct", "latest_checked_at", "latest_trade_at"]
# What the tracker finds out AFTER the call (pool discovery, rug check, current
# price/liquidity): never inputs, whether or not the view exports them. The
# post's own `dex` and `launchpad` are known at the call and stay allowed.
TRACKER_DISCOVERY = ["rugged", "current_liquidity_usd", "current_price_usd",
                     "entry_price_source", "pool_dex", "pool_address", "pool_name",
                     "pool_created_at"]
# Timing and rug outcomes the tracker stores after the call (scout_call_returns:
# peak_late_after_s ... timing_at; scout_call_tracking: rug_at, rug_at_kind).
# Not in scout_call_dataset_v yet; when the view gets them they are OUTCOMES
# (report-only scores such as "2x within 1 h", "rug within 24 h"), never inputs.
# Per-horizon variants (e.g. above_2x_s_1h, first_2x_after_late_1d, rug_at_1d,
# following the ret_late_1d naming) are caught by the prefixes below.
TIMING_RUG_OUTCOMES = ["peak_late_after_s", "first_2x_after_s", "above_2x_s",
                       "fall_below_2x_after_s", "above_2x_censored", "timing_at",
                       "rug_at", "rug_at_kind"]
FORBIDDEN_COLUMNS = (set(BOOKKEEPING) - {"quote_asset"} | set(IDENTITY)
                     | {"pre_vol_unit", "token_name", "post_kind"} | set(LATEST)
                     | set(TRACKER_DISCOVERY) | set(AFTER_CALL_VIEW)
                     | set(TIMING_RUG_OUTCOMES))
# trades_*: trading counted AFTER the call (trades_24h, any later trades_7d ...).
# Trading before the call is pre_* and stays allowed (pre_first_trade_age_s too).
# peak_* / first_2x_* / above_2x_* / fall_below_2x_* / timing_* / rug_at*: the
# timing and rug outcomes above and any per-horizon or entry ("_late") variant.
FORBIDDEN_PREFIXES = ("ret_", "max_gain_", "max_dd_", "latest_", "current_", "pool_", "trades_",
                      "peak_late_after_", "peak_after_", "first_2x_", "above_2x_",
                      "fall_below_2x_", "timing_", "rug_at")

# --- Extreme outcomes ------------------------------------------------------
# A drained or broken pool can give absurd outcomes (e.g. +3.9e47 %). A row
# with ANY label-relevant outcome (the raw, before-tax value of a column used
# by some bucket's runner, collapse or ret_col) above this cap is EXCLUDED
# from every bucket (training, validation, simulation), not clipped, and
# counted in the report. A missing outcome is not extreme. -100 % (total loss)
# is a real outcome and is kept.
MAX_OUTCOME_PCT = 1e5
# The money simulation (mean net return of the top 10% vs all calls) credits
# each call with at most this net return, so that one legitimate but huge
# winner below MAX_OUTCOME_PCT cannot decide "beats buy-everything" on its
# own. Labels are not affected. +1000 % = 11x.
SIM_MAX_RET_PCT = 1000.0

# --- DEX families -------------------------------------------------------------
# The posted DEX name (`dex`, known at the call) has about 50 values, many
# short-lived ("Pons" until mid-August, then "Pons V2"). `dex_family` groups
# them: the name is lower-cased and every space, "_", "-", "." and "/" removed
# ("Uniswap V4", "uniswap_v4", "UniswapV4" -> "uniswapv4"); the FIRST rule whose
# prefix it starts with gives the family. A name no rule matches is "other";
# an empty / NULL name stays missing. Edit the table to add a family (put more
# specific prefixes before shorter ones). Like every categorical, the family
# levels a model uses are learned from its training rows only (a family seen
# fewer than MIN_CATEGORY_COUNT times there is treated as missing).
# Built from the posted names of 28 Jul - 8 Oct 2026 (RUNBOOK.md query (g)).
# Names with fewer than about 30 calls in all (virtuals, pair fund, noxa,
# noxafi, stonkbroker, bags, lemonswap, trench, orbofi, ...) get no family of
# their own: they are "other". A family is still subject to MIN_CATEGORY_COUNT
# in each training part (a family rarer than that there is treated as missing).
DEX_FAMILY_RULES = (
    ("pons", "pons"),              # "Pons", "Pons V2" (Pons launchpad; bonding curve, then v4)
    ("uniswapv2", "uniswap_v2"),
    ("uniswapv3", "uniswap_v3"),
    ("uniswapv4", "uniswap_v4"),
    ("uniswap", "uniswap"),        # Uniswap without a version (none posted so far)
    ("longxyz", "longxyz"),
    ("pools", "pools"),            # "Pools Trade Instant", "Pools Fun", "Pools Trade CCA"
    ("bankr", "bankr"),
    ("o1", "o1"),                  # "O1 Rwa", "O1" (not "Orbofi": "or..." does not start with "o1")
    ("letscash", "letscash"),
    ("varo", "varo"),
    ("flap", "flap"),              # "Flap", "Flap Stocks", "Flap Pve"
    ("sushi", "sushi"),            # "Sushiswap", "Sushi"
    ("lunch", "lunch"),            # "Lunch Pair V4", "Lunch Pair V3", "Lunch V3"
)
DEX_FAMILY_OTHER = "other"
# Which DEX inputs a new model gets (see ml/README.md, "DEX families"):
# "family" = dex_family only (default), "raw" = raw `dex` only (the inputs of
# models trained before dex_family), "both" = raw `dex` and dex_family.
DEX_INPUTS = "family"
DEX_INPUT_CHOICES = ("family", "raw", "both")

# --- "Runners must be tradeable" (off by default; a --variants row) ------------
# With RUNNER_NEEDS_TRADES = True the runner label of every bucket also needs
# trades_24h >= RUNNER_MIN_TRADES_24H, so a jump in a pool nobody traded (e.g.
# 52 price events and +219 % at 1 d) is not a runner. A NULL trades_24h (not
# priced on-chain, or not scanned yet) keeps the plain runner label. Only the
# label (and so the lift / runner counts) changes: the money simulation still
# uses every call. trades_24h stays an outcome, never a feature.
RUNNER_NEEDS_TRADES = False
RUNNER_MIN_TRADES_24H = 100

# --- Derived features (built in features.build_features) -------------------
DERIVED = (["liq_to_mcap", "live_usd_to_liq", "live_usd_to_mcap", "mcap_vs_called"]
           + [f"pre_buy_sell_ratio_{w}" for w in _WINDOWS]
           + [f"pre_buy_vol_share_{w}" for w in _WINDOWS]
           + ["hour_utc", "weekday_utc"])
DERIVED_CATEGORICAL = ["dex_family"]
# Every categorical build_features can encode (a model saved before a change
# may still list one that is no longer configured, e.g. raw `dex`).
ALL_CATEGORICAL = CATEGORICAL_VIEW + DERIVED_CATEGORICAL


def categorical_inputs(dex_inputs: str) -> list:
    """The categorical model inputs for a DEX_INPUTS setting."""
    if dex_inputs not in DEX_INPUT_CHOICES:
        raise ValueError(f"DEX_INPUTS must be one of {DEX_INPUT_CHOICES}, not {dex_inputs!r}")
    raw = [c for c in CATEGORICAL_VIEW if dex_inputs != "family" or c != "dex"]
    return raw + (DERIVED_CATEGORICAL if dex_inputs != "raw" else [])


# Inputs left out of new models (a VARIANT_KNOB; "no calls_prev inputs" sets
# it). Empty: every input above (the saved model). Only numeric inputs
# (NUMERIC_RAW + DERIVED) can be left out; it can only remove inputs, never add one.
EXCLUDED_INPUTS = ()
# Signed, bounded or already-small columns are standardised as is by the
# logistic baseline; every other numeric feature is non-negative and skewed.
NO_LOG = set(["liq_pct", "tax_buy_pct", "tax_sell_pct", "proof_elite", "proof_good",
              "hour_utc", "weekday_utc"]
             + [f"pre_price_chg_{w}_pct" for w in _WINDOWS]
             + [f"pre_buy_vol_share_{w}" for w in _WINDOWS])


def numeric_inputs(excluded=()) -> list:
    """The numeric model inputs without `excluded` (raises on a name that is not one)."""
    allowed, excluded = NUMERIC_RAW + DERIVED, set(excluded)
    bad = sorted(excluded - set(allowed))
    if bad:
        raise ValueError(f"EXCLUDED_INPUTS must name numeric inputs only, not {bad}")
    return [c for c in allowed if c not in excluded]


# The model inputs of new models (rebuilt by `overrides`).
CATEGORICAL = categorical_inputs(DEX_INPUTS)
NUMERIC_FEATURES = numeric_inputs(EXCLUDED_INPUTS)
FEATURES = NUMERIC_FEATURES + CATEGORICAL
LOG1P_FEATURES = [c for c in NUMERIC_FEATURES if c not in NO_LOG]
# Category levels are learned from the training rows of each split only (main
# split, every walk-forward window, every inner k-fold), after lower-casing
# and stripping; rarer levels, and levels never seen in training, are missing.
MIN_CATEGORY_COUNT = 20

# --- Validation ------------------------------------------------------------
# `short` and `3day`: row-quantile split train 70% / validation 15% / test 15%
# (validation drives early stopping and calibration), embargo = horizon.
TRAIN_FRAC, VAL_FRAC = 0.70, 0.15      # test = the latest remaining 15%
# `medium` and `long` (long horizons, where a horizon-length embargo between
# three parts left almost no validation rows): no validation part. The test
# part is forward only: the calls of the last FORWARD_TEST_DAYS days of the
# bucket's labelled data, starting at a midnight UTC date; train = calls
# posted before that date minus the horizon (embargo). Boosting rounds and
# Platt calibration come from a purged, time-ordered KFOLD_K-fold inside the
# train part only: folds are contiguous in time; training rows posted within
# one horizon before or after a fold are purged; calibration is fitted on the
# out-of-fold scores. A fold is used only when its fit part has MIN_CLASS_ROWS
# of each class and its held-out part MIN_CLASS_ROWS // 3 of each; with fewer
# than KFOLD_MIN_FOLDS usable folds the model falls back to FALLBACK_ROUNDS,
# uncalibrated (the report says so).
FORWARD_SPLIT_BUCKETS = ("medium", "long")
FORWARD_TEST_DAYS = 14
KFOLD_K = 5
KFOLD_MIN_FOLDS = 3
# A bucket is skipped until enough calls have a matured outcome: at least
# `rows` usable rows AND `span_days` between the first and the last of them.
# long (30d): the purged 5-fold needs about 75 days of train calls so that at
# least 4 folds keep training rows after purging 30 days on both sides; plus
# the 30-day embargo and the 14-day test part: 75 + 30 + 14 = 119, so 120.
# With calls from 28 Jul 2026 that is first met by calls up to about
# 25 Nov 2026, i.e. by an export from about 25 Dec 2026 (their 30d is due).
MIN_MATURED = {"long": {"rows": 2000, "span_days": 120}}
MIN_BUCKET_ROWS = 300                  # fewer usable rows -> bucket skipped
MIN_CLASS_ROWS = 30                    # per class, in the training part
MIN_WINDOW_TEST_ROWS = 50              # walk-forward weeks smaller than this are skipped
TOP_FRAC = 0.10                        # "buy the top 10% by runner score"
SKIP_FRAC = 0.30                       # "skip the 30% with highest collapse score"
GATES = {"min_top_lift": 2.0, "min_collapse_removed": 0.40}
# --- Bucket gates (owner decision of 13 Oct 2026, "option (b)") ----------------
# A bucket PASSes (meta.json metrics.<bucket>.gates.passed, which the scoring
# service reads) only when all four hold (validate.pooled_gates):
# 1. runner lift: the MEDIAN over STABILITY_SEEDS of the pooled expanding
#    walk-forward top-10% lift >= GATES["min_top_lift"] (2), every seed's pooled
#    lift defined; AND the lower end of the 95% week-block bootstrap interval
#    (POOLED_CI, POOLED_BOOTSTRAP_REPS, POOLED_BOOTSTRAP_SEED) of the saved
#    seed's (LGBM_SEED) pooled lift > GATE_POOLED_LIFT_LOWER;
# 2. beats buy-all: the lower end of the 95% bootstrap interval of the pooled
#    simulation difference (mean net return of the per-week top 10% minus that
#    of all calls, each capped at SIM_MAX_RET_PCT; saved seed) > GATE_SIM_DIFF_LOWER;
# 3. collapses removed >= GATES["min_collapse_removed"] on the test part (saved
#    model; unchanged).
# Pooled = the test rows of the counted walk-forward weeks (WF_MIN_TRAIN_ROWS),
# each week ranked by its own model. Fewer than GATE_MIN_POOLED_WEEKS counted
# weeks, a seed whose run failed / was skipped, or any undefined (NaN) input:
# the bucket FAILS, with the reason in gates["reasons"]; never a PASS on missing
# data. The pooled top-10% mean net return (> 0 or not) is shown as information
# only, never a gate. The old single-run gates (test-part lift >= 2, beats
# buy-all in every counted week) are still computed and shown as information
# (gates["old"]); the --variants rows and the seed table use them.
# The seed retrains and the pooled walk-forward run in every training, also
# with the stability diagnostics switched off (DIAGNOSTICS_ENV=0): only the
# report-only parts (rolling walk-forward, evaluation without dead calls,
# adversarial validation) are switchable. STABILITY_SEEDS and POOLED_* below
# therefore decide the verdict.
GATE_POOLED_LIFT_LOWER = 1.0
GATE_SIM_DIFF_LOWER = 0.0
GATE_MIN_POOLED_WEEKS = 2              # the week-block bootstrap needs at least 2 weeks
GATES_SET_ON = "13 Oct 2026"
# Owner decision of 9 Oct 2026: a walk-forward week counts (toward the pooled
# walk-forward gates since 13 Oct 2026, and the old "beats buy-all in every
# week" count, now information) only when its training part has at least this
# many rows. Weeks below are still computed and shown, marked "not counted
# (train < WF_MIN_TRAIN_ROWS)". The same weeks are left out of the walk-forward
# summaries (variant table, dead-score proposal). The early weeks of an
# expanding walk-forward from the data start (e.g. medium, week of 18 Aug 2026:
# 530 train rows) stay in every report and would otherwise fail it forever.
WF_MIN_TRAIN_ROWS = 1000
WF_MIN_TRAIN_ROWS_SET_ON = "9 Oct 2026"

# --- Calibration on recent weeks --------------------------------------------
# The base rates drift week to week, so the Platt calibration is fitted on the
# most recent held-out rows only: the validation rows (short/3day) or the
# out-of-fold rows (medium/long) posted in the last CALIB_RECENT_DAYS days
# before the newest of them. Never on rows the model was fitted on. Fallback
# to all held-out rows (the earlier behaviour) when the recent rows have fewer
# than CALIB_MIN_CLASS_ROWS of either class or the fit would invert the
# ranking (slope <= 0). Ranking metrics (lift, collapses removed) do not change.
# Whether this recent window or all held-out rows are used is chosen per label
# (see CALIB_CHOICE_FIT_FRAC below).
CALIB_RECENT_DAYS = 14
CALIB_MIN_CLASS_ROWS = 20
# Owner decision of 9 Oct 2026: the recent window is not always better (prod
# report 20261009-2250, medium collapse: Brier 0.191 recent vs 0.170 all rows),
# so the calibration is CHOSEN between "recent window" and "all held-out rows"
# on the held-out rows alone (never test rows): they are ordered by time, both
# methods are fitted on the earliest CALIB_CHOICE_FIT_FRAC of them and their
# Brier is compared on the rest; the method with the lower Brier is then fitted
# on all held-out rows. "all held-out rows" is used (fallback) when the
# held-out rows span less than CALIB_RECENT_DAYS, the recent window of all
# held-out rows cannot be fitted (then the report shows it as "n/a"), the later
# or the earlier part has fewer than CALIB_MIN_CLASS_ROWS of a class, the
# earlier part cannot fit the recent window (too few of a class in it, slope
# <= 0, or it covers all of the earlier part), or the two Briers are equal.
CALIB_CHOICE_FIT_FRAC = 0.5
N_REF_QUANTILES = 101

# --- LightGBM: deliberately small/regularised for ~10k rows ----------------
# model.lgbm_params() builds the settings of every fit at call time:
# LGBM_PARAMS, then LGBM_REG_SETS[LGBM_REGULARISATION], then
# random_state = LGBM_SEED (all three are VARIANT_KNOBS).
LGBM_PARAMS = dict(objective="binary", n_estimators=600, learning_rate=0.05,
                   num_leaves=15, min_child_samples=20, subsample=0.8,
                   subsample_freq=1, colsample_bytree=0.8, reg_lambda=1.0,
                   n_jobs=2, verbose=-1)
LGBM_SEED = 7                          # the saved model's seed (bagging and feature sampling)
# "default" changes nothing. "strong" (a --variants row, report only) is a
# clearly more regularised tree: fewer leaves (15 -> 7), at least 50 rows per
# leaf (min_child_samples = LightGBM's min_data_in_leaf, 20 -> 50), L2 penalty
# 1 -> 10 (reg_lambda = lambda_l2) and 60% of the features per tree
# (colsample_bytree = feature_fraction, 0.8 -> 0.6). Rounds still come from
# early stopping / the purged k-fold as for the baseline.
LGBM_REGULARISATION = "default"
LGBM_REG_SETS = {
    "default": {},
    "strong": {"num_leaves": 7, "min_child_samples": 50, "reg_lambda": 10.0,
               "colsample_bytree": 0.6},
}
EARLY_STOPPING_ROUNDS = 50
FALLBACK_ROUNDS = 150  # used when the validation part / k-fold cannot drive early stopping

# --- Report: interval around the top-10% lift -------------------------------
# Wilson score interval (95%) on the runner rate in the top 10%, divided by
# the runner rate of all calls in the same part (treated as fixed).
LIFT_CI_Z = 1.96

# --- Variant comparison (train.py --variants) --------------------------------
# Each variant changes exactly ONE knob from the configured (baseline) values
# above. They are trained and reported only: never saved, never served, never
# the gates of the saved model (that is always the baseline). "baseline" reuses
# the results of the saved model. Knobs: VARIANT_KNOBS.
VARIANT_KNOBS = ("DEAD_IS_COLLAPSE", "DEAD_TRADES_24H", "DEX_INPUTS", "RUNNER_NEEDS_TRADES",
                 "LGBM_REGULARISATION", "LGBM_SEED", "EXCLUDED_INPUTS")
# "dead threshold 100" was dropped: DEAD_TRADES_24H only changes the labels
# while the dead rule is on, so with the rule off it would equal the baseline.
VARIANTS = {
    "baseline": {},
    "dead rule on (collapse OR dead)": {"DEAD_IS_COLLAPSE": True},
    "raw dex instead of dex_family": {"DEX_INPUTS": "raw"},
    "raw dex + dex_family": {"DEX_INPUTS": "both"},
    "runners must be tradeable": {"RUNNER_NEEDS_TRADES": True},
    "stronger regularisation": {"LGBM_REGULARISATION": "strong"},
    # report 20261013-0702: adversarial validation found calls_prev_24h driving the
    # train/test drift (AUC 0.99; gain share 0.42-0.87)
    "no calls_prev inputs": {"EXCLUDED_INPUTS": ("calls_prev_1h", "calls_prev_24h")},
}
# Variants that only differ from the baseline through trades_24h: not applicable
# when the export has no trades_24h column.
TRADES_VARIANTS = ("dead rule on (collapse OR dead)", "runners must be tradeable")

# --- Stability diagnostics ---------------------------------------------------------
# Extra sections at the end of report.md (before the variant comparison) to
# tell whether the results are real or noise: seed stability, pooled and
# rolling walk-forward, the evaluation without dead calls, and adversarial
# validation. Since 13 Oct 2026 the seed retrains (STABILITY_SEEDS) and the
# pooled EXPANDING walk-forward with its bootstrap are gate inputs (see "Bucket
# gates" above): they run in every training, before meta.json is written,
# whatever the switch says. The rest (rolling walk-forward, evaluation without
# dead calls, adversarial validation) is report only: never saved, never
# served, never a gate; the saved model, its meta.json fields, the gates and
# the verdict are the same with the switch on or off (meta.json only gets an
# extra "diagnostics" entry).
# On by default for train.py; the environment variable DIAGNOSTICS_ENV set to
# 0 / off / false / no switches the section off for one run (1 / on / true / yes
# switches it on when DIAGNOSTICS is False).
DIAGNOSTICS = True
DIAGNOSTICS_ENV = "SCOUT_ML_DIAGNOSTICS"
# Seed stability (a gate input): the baseline retrained with each of these
# LightGBM seeds (LGBM_SEED); a seed equal to LGBM_SEED reuses the saved model's
# results. The runner-lift gate takes the median over exactly these seeds.
STABILITY_SEEDS = (7, 11, 23)
# Pooled walk-forward (the expanding one is a gate input): the test rows of all
# counted weeks of a bucket pooled, each week ranked by its own model (top 10% /
# top 30% per week); 95% interval by a week-block bootstrap (weeks resampled
# with replacement) with this many repetitions and this seed.
POOLED_BOOTSTRAP_REPS = 2000
POOLED_BOOTSTRAP_SEED = 7
POOLED_CI = (0.025, 0.975)
# Rolling walk-forward next to the expanding one: each week's model is trained
# on the calls of the last WF_ROLLING_WEEKS weeks before the week (minus the
# embargo) only. Same rounds, test weeks and metrics; pooled over the weeks the
# expanding walk-forward counts (paired; the rolling window's own size is not
# held to WF_MIN_TRAIN_ROWS, it is small by design).
WF_ROLLING_WEEKS = 4
# Evaluation without dead calls (trades_24h < DEAD_TRADES_24H): the SAME
# trained models (test part and walk-forward weeks), metrics recomputed on the
# rows that are not dead (NULL trades_24h is kept). Evaluation only: training
# rows, labels, gates and the verdict are unchanged.
EVAL_EXCLUDE_DEAD = True
# Adversarial validation: a classifier told to separate the train part (0)
# from the test part (1) on the model's own inputs (build_features, so the
# leakage guard holds). ROC AUC out of fold (ADV_FOLDS-fold stratified, shuffled
# with ADV_PARAMS' seed: a drift probe, not a forecast); 0.5 = no drift. Top
# drifting features by gain. ADV_PER_WEEK: also per counted walk-forward week
# (that week's training rows vs its test rows), for the newest ADV_MAX_WEEKS
# counted weeks only and with ADV_WEEK_FOLDS folds (to keep the run short).
ADV_PARAMS = dict(objective="binary", n_estimators=100, learning_rate=0.1, num_leaves=15,
                  min_child_samples=20, subsample=0.8, subsample_freq=1,
                  colsample_bytree=0.8, random_state=7, n_jobs=2, verbose=-1)
ADV_FOLDS = 5
# Calendar inputs left out of the probe: when in the day / week a call was
# posted does not drift, but a test part or week that covers only part of a
# week differs in weekday_utc by construction (the probe would just find that).
ADV_EXCLUDE = ("hour_utc", "weekday_utc")
ADV_TOP_FEATURES = 8
ADV_PER_WEEK = True
ADV_MAX_WEEKS = 6
ADV_WEEK_FOLDS = 3


def is_forbidden(col: str) -> bool:
    """True if `col` is an outcome/bookkeeping/identity column."""
    return col in FORBIDDEN_COLUMNS or col.startswith(FORBIDDEN_PREFIXES)


def variant_settings(name: str) -> dict:
    """The knob values of variant `name` (baseline values for the knobs it does not change)."""
    g = globals()
    return {**{k: g[k] for k in VARIANT_KNOBS}, **VARIANTS[name]}


@contextmanager
def overrides(**knobs):
    """Temporarily set VARIANT_KNOBS (and rebuild CATEGORICAL, NUMERIC_FEATURES,
    LOG1P_FEATURES and FEATURES from DEX_INPUTS / EXCLUDED_INPUTS); everything is
    restored on exit, also after an error."""
    g = globals()
    bad = sorted(set(knobs) - set(VARIANT_KNOBS))
    if bad:
        raise ValueError(f"not a variant knob: {bad}")
    if "LGBM_REGULARISATION" in knobs and knobs["LGBM_REGULARISATION"] not in LGBM_REG_SETS:
        raise ValueError(f"LGBM_REGULARISATION must be one of {sorted(LGBM_REG_SETS)}, "
                         f"not {knobs['LGBM_REGULARISATION']!r}")
    numeric_inputs(knobs.get("EXCLUDED_INPUTS", ()))       # raises before anything is changed
    saved = {k: g[k] for k in (*VARIANT_KNOBS, "CATEGORICAL", "FEATURES", "NUMERIC_FEATURES",
                               "LOG1P_FEATURES")}
    try:
        g.update(knobs)
        g["CATEGORICAL"] = categorical_inputs(g["DEX_INPUTS"])
        g["NUMERIC_FEATURES"] = numeric_inputs(g["EXCLUDED_INPUTS"])
        g["LOG1P_FEATURES"] = [c for c in g["NUMERIC_FEATURES"] if c not in NO_LOG]
        g["FEATURES"] = g["NUMERIC_FEATURES"] + g["CATEGORICAL"]
        yield
    finally:
        g.update(saved)
