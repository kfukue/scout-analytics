"""Everything a human may want to tune lives here: buckets, label thresholds,
feature lists, forbidden (leaky) columns, model settings and pass gates."""

# --- Label definitions -----------------------------------------------------
# Each label is (outcome column, comparison, threshold in percent). The outcome
# is first netted of buy+sell tax (see labels.net_pct). `ret_col` is the return
# used by the money simulation; `horizon_days` drives the embargo.
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
    "long": {"title": "long", "runner_word": "up at 30d", "horizon_days": 30,
             "runner": ("ret_late_30d", ">", 0.0),
             "collapse": ("ret_late_30d", "<=", -90.0),
             "collapse_or_rugged": True, "ret_col": "ret_late_30d"},
}
LABELS = ("runner", "collapse")
NO_POOL_STATUSES = ("no_pool", "gave_up")  # calls we could never have traded
REPEAT_STATUS = "repeat"  # a later call of a token: the tracker follows the first call only
UPDATE_KIND = "update"  # post_kind of a "$TOKEN hit 3X ..." post about an earlier call: not a call

# --- "Dead after the call" ---------------------------------------------------
# trades_24h (view column, an OUTCOME): price events in the pool during the 24 h
# after the call (5-minute candles; on a v2 pool every Sync counts, which includes
# liquidity added or removed). A call with fewer than DEAD_TRADES_24H is "dead":
# bought, then nobody traded it. Dead calls stay in the data (they were real
# buys). With DEAD_IS_COLLAPSE the collapse label of EVERY bucket becomes
# "collapse OR dead"; a row whose trades_24h is NULL (not priced on-chain, or
# its first 24 h not scanned yet) keeps the plain collapse label. Set
# DEAD_IS_COLLAPSE = False to train on the plain collapse label again.
# The gates do not change: "collapses removed" (GATES) is always measured on
# the plain collapse label; the share of collapse OR dead removed is reported
# next to it as information only.
DEAD_COLUMN = "trades_24h"
DEAD_TRADES_24H = 50
DEAD_IS_COLLAPSE = True

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
FORBIDDEN_COLUMNS = (set(BOOKKEEPING) - {"quote_asset"} | set(IDENTITY)
                     | {"pre_vol_unit", "token_name", "post_kind"} | set(LATEST)
                     | set(TRACKER_DISCOVERY) | set(AFTER_CALL_VIEW))
# trades_*: trading counted AFTER the call (trades_24h, any later trades_7d ...).
# Trading before the call is pre_* and stays allowed.
FORBIDDEN_PREFIXES = ("ret_", "max_gain_", "max_dd_", "latest_", "current_", "pool_", "trades_")

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
DEX_FAMILY_RULES = (
    ("pons", "pons"),              # "Pons", "Pons V2" (Pons launchpad; bonding curve, then v4)
    ("uniswapv2", "uniswap_v2"),
    ("uniswapv3", "uniswap_v3"),
    ("uniswapv4", "uniswap_v4"),
    ("uniswap", "uniswap"),        # Uniswap without a version
    ("longxyz", "longxyz"),
)
DEX_FAMILY_OTHER = "other"
# Raw `dex` as a model input next to `dex_family` (see ml/README.md, "DEX families").
USE_RAW_DEX = False

# --- Derived features (built in features.build_features) -------------------
DERIVED = (["liq_to_mcap", "live_usd_to_liq", "live_usd_to_mcap", "mcap_vs_called"]
           + [f"pre_buy_sell_ratio_{w}" for w in _WINDOWS]
           + [f"pre_buy_vol_share_{w}" for w in _WINDOWS]
           + ["hour_utc", "weekday_utc"])
DERIVED_CATEGORICAL = ["dex_family"]
# Every categorical build_features can encode (a model saved before a change
# may still list one that is no longer configured, e.g. raw `dex`).
ALL_CATEGORICAL = CATEGORICAL_VIEW + DERIVED_CATEGORICAL
# The categorical model inputs of new models.
CATEGORICAL = [c for c in CATEGORICAL_VIEW if USE_RAW_DEX or c != "dex"] + DERIVED_CATEGORICAL
FEATURES = NUMERIC_RAW + DERIVED + CATEGORICAL
NUMERIC_FEATURES = NUMERIC_RAW + DERIVED
# Signed, bounded or already-small columns are standardised as is by the
# logistic baseline; every other numeric feature is non-negative and skewed.
NO_LOG = set(["liq_pct", "tax_buy_pct", "tax_sell_pct", "proof_elite", "proof_good",
              "hour_utc", "weekday_utc"]
             + [f"pre_price_chg_{w}_pct" for w in _WINDOWS]
             + [f"pre_buy_vol_share_{w}" for w in _WINDOWS])
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

# --- Calibration on recent weeks --------------------------------------------
# The base rates drift week to week, so the Platt calibration is fitted on the
# most recent held-out rows only: the validation rows (short/3day) or the
# out-of-fold rows (medium/long) posted in the last CALIB_RECENT_DAYS days
# before the newest of them. Never on rows the model was fitted on. Fallback
# to all held-out rows (the earlier behaviour) when the recent rows have fewer
# than CALIB_MIN_CLASS_ROWS of either class or the fit would invert the
# ranking (slope <= 0). Ranking metrics (lift, collapses removed) do not change.
CALIB_RECENT_DAYS = 14
CALIB_MIN_CLASS_ROWS = 20
N_REF_QUANTILES = 101

# --- LightGBM: deliberately small/regularised for ~10k rows ----------------
LGBM_PARAMS = dict(objective="binary", n_estimators=600, learning_rate=0.05,
                   num_leaves=15, min_child_samples=20, subsample=0.8,
                   subsample_freq=1, colsample_bytree=0.8, reg_lambda=1.0,
                   random_state=7, n_jobs=2, verbose=-1)
EARLY_STOPPING_ROUNDS = 50
FALLBACK_ROUNDS = 150  # used when the validation part / k-fold cannot drive early stopping

# --- Report: interval around the top-10% lift -------------------------------
# Wilson score interval (95%) on the runner rate in the top 10%, divided by
# the runner rate of all calls in the same part (treated as fixed).
LIFT_CI_Z = 1.96


def is_forbidden(col: str) -> bool:
    """True if `col` is an outcome/bookkeeping/identity column."""
    return col in FORBIDDEN_COLUMNS or col.startswith(FORBIDDEN_PREFIXES)
