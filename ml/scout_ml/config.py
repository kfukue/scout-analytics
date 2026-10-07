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

# --- Columns of scout_call_dataset_v, in view order ------------------------
_WINDOWS = ("5m", "15m", "60m")
HORIZONS = ("1h", "1d", "3d", "7d", "30d")
IDENTITY = ["call_id", "message_id", "message_date", "contract_address",
            "call_status", "token_symbol"]
CATEGORICAL = ["dex", "launchpad", "quote_asset", "perceptor_verdict"]
PRE_VOL = [f"pre_{s}_vol_{w}" for s in ("buy", "sell") for w in _WINDOWS]
NUMERIC_RAW = (
    ["called_at_mcap_usd", "mcap_usd", "liq_usd", "liq_pct", "tax_buy_pct",
     "tax_sell_pct", "age_seconds", "holders", "proof_elite", "proof_good",
     "live_buys_elite_count", "live_buys_good_count", "live_buys_elite_usd",
     "live_buys_good_usd", "live_buy_max_usd", "prior_calls",
     "secs_since_prev_call", "calls_prev_1h", "calls_prev_24h"]
    + [f"pre_{k}_{w}" for k in ("swaps", "buys", "sells") for w in _WINDOWS]
    + PRE_VOL
    + [f"pre_price_chg_{w}_pct" for w in _WINDOWS] + ["pre_first_trade_age_s"])
OUTCOMES = [f"{m}{late}_{h}" for h in HORIZONS for late in ("", "_late")
            for m in ("ret", "max_gain", "max_dd")]
BOOKKEEPING = ["tracking_status", "pool_address", "pool_dex", "entry_price_usd",
               "entry_price_source", "price_unit", "quote_asset",
               "entry_late_price_usd", "current_liquidity_usd", "rugged"]
# latest_* columns the view exports (latest_trade_at is not exported).
LATEST_VIEW = ["latest_price_usd", "latest_return_pct", "latest_checked_at"]
# Exactly the columns of scout_call_dataset_v (scoutanalytics.sql), in order;
# tests/test_view_columns.py compares this list with the SQL.
VIEW_COLUMNS = (["call_id", "message_id", "message_date", "contract_address", "call_status",
                 "post_kind", "token_symbol", "token_name", "dex", "launchpad"] + NUMERIC_RAW
                + ["pre_vol_unit", "perceptor_verdict"] + BOOKKEEPING + OUTCOMES + LATEST_VIEW)

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
                     | set(TRACKER_DISCOVERY))
FORBIDDEN_PREFIXES = ("ret_", "max_gain_", "max_dd_", "latest_", "current_", "pool_")

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

# --- Derived features (built in features.build_features) -------------------
DERIVED = (["liq_to_mcap", "live_usd_to_liq", "live_usd_to_mcap", "mcap_vs_called"]
           + [f"pre_buy_sell_ratio_{w}" for w in _WINDOWS]
           + [f"pre_buy_vol_share_{w}" for w in _WINDOWS]
           + ["hour_utc", "weekday_utc"])
FEATURES = NUMERIC_RAW + DERIVED + CATEGORICAL
NUMERIC_FEATURES = NUMERIC_RAW + DERIVED
# Signed, bounded or already-small columns are standardised as is by the
# logistic baseline; every other numeric feature is non-negative and skewed.
NO_LOG = set(["liq_pct", "tax_buy_pct", "tax_sell_pct", "proof_elite", "proof_good",
              "hour_utc", "weekday_utc"]
             + [f"pre_price_chg_{w}_pct" for w in _WINDOWS]
             + [f"pre_buy_vol_share_{w}" for w in _WINDOWS])
LOG1P_FEATURES = [c for c in NUMERIC_FEATURES if c not in NO_LOG]
MIN_CATEGORY_COUNT = 20  # rarer category levels are treated as missing

# --- Validation ------------------------------------------------------------
TRAIN_FRAC, VAL_FRAC = 0.70, 0.15      # test = the latest remaining 15%
MIN_BUCKET_ROWS = 300                  # fewer usable rows -> bucket skipped
MIN_CLASS_ROWS = 30                    # per class, in the training part
MIN_WINDOW_TEST_ROWS = 50              # walk-forward weeks smaller than this are skipped
TOP_FRAC = 0.10                        # "buy the top 10% by runner score"
SKIP_FRAC = 0.30                       # "skip the 30% with highest collapse score"
GATES = {"min_top_lift": 2.0, "min_collapse_removed": 0.40}
N_REF_QUANTILES = 101

# --- LightGBM: deliberately small/regularised for ~10k rows ----------------
LGBM_PARAMS = dict(objective="binary", n_estimators=600, learning_rate=0.05,
                   num_leaves=15, min_child_samples=20, subsample=0.8,
                   subsample_freq=1, colsample_bytree=0.8, reg_lambda=1.0,
                   random_state=7, n_jobs=2, verbose=-1)
EARLY_STOPPING_ROUNDS = 50
FALLBACK_ROUNDS = 150  # used when the validation part cannot drive early stopping


def is_forbidden(col: str) -> bool:
    """True if `col` is an outcome/bookkeeping/identity column."""
    return col in FORBIDDEN_COLUMNS or col.startswith(FORBIDDEN_PREFIXES)
