"""Model fitting, calibrated prediction, and the loaded bundle used for serving."""
import json
import warnings
from pathlib import Path

import joblib
import lightgbm as lgb
import numpy as np
import pandas as pd
from sklearn.compose import ColumnTransformer
from sklearn.impute import SimpleImputer
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import FunctionTransformer, StandardScaler

from . import config as C
from .config import (ALL_CATEGORICAL, BUCKETS, EARLY_STOPPING_ROUNDS, FALLBACK_ROUNDS,
                     LGBM_PARAMS, LOG1P_FEATURES, MIN_CLASS_ROWS, NUMERIC_FEATURES)
from .features import build_features


def _log1p_clipped(a):
    return np.log1p(np.clip(a, 0, None))


def fit_baseline(X, y):
    """Logistic regression on numeric features: the 'is the GBM worth it' check."""
    def impute():
        return SimpleImputer(strategy="median", keep_empty_features=True)
    plain = [c for c in NUMERIC_FEATURES if c not in LOG1P_FEATURES]
    prep = ColumnTransformer([
        ("log", make_pipeline(impute(), FunctionTransformer(_log1p_clipped), StandardScaler()),
         LOG1P_FEATURES),
        ("plain", make_pipeline(impute(), StandardScaler()), plain)])
    return make_pipeline(prep, LogisticRegression(C=0.5, max_iter=2000)).fit(X[NUMERIC_FEATURES], y)


def fit_lgbm(X_train, y_train, X_val=None, y_val=None, n_estimators=None):
    """Fit LightGBM and return {"model", "calibrator", "note"}.

    With a usable validation part: early stopping on it, then Platt scaling
    (a 1-d logistic fit on the raw margin) on the same part. Platt rather than
    isotonic because the validation part is small and isotonic creates ties,
    which would blur the top-10% ranking. With `n_estimators` given
    (walk-forward) or an unusable validation part: fixed rounds, no calibrator.
    """
    y_val = None if y_val is None else np.asarray(y_val, dtype=int)
    usable_val = n_estimators is None and y_val is not None and usable_holdout(y_val)
    params = dict(LGBM_PARAMS)
    fit_kw = {}
    if usable_val:
        fit_kw = dict(eval_set=[(X_val, y_val)], eval_metric="binary_logloss",
                      callbacks=[lgb.early_stopping(EARLY_STOPPING_ROUNDS, verbose=False)])
    else:
        params["n_estimators"] = n_estimators or FALLBACK_ROUNDS
    model = lgb.LGBMClassifier(**params)
    with warnings.catch_warnings():  # lightgbm >= 4.7 deprecates eval_set; keep 4.0 compatibility
        warnings.filterwarnings("ignore", message=".*eval_set.*")
        model.fit(X_train, np.asarray(y_train, dtype=int),
                  categorical_feature=[c for c in X_train.columns if c in ALL_CATEGORICAL], **fit_kw)
    calibrator, note = None, None
    if usable_val:
        calibrator = fit_platt(model.predict(X_val, raw_score=True), y_val)
    elif n_estimators is None:
        note = ("validation part too small for early stopping/calibration: "
                f"fixed {FALLBACK_ROUNDS} rounds, probabilities uncalibrated")
    return {"model": model, "calibrator": calibrator, "note": note}


def usable_holdout(y) -> bool:
    """Enough of each class in a held-out part to drive early stopping/calibration."""
    y = np.asarray(y, dtype=int)
    return min(y.sum(), len(y) - y.sum()) >= MIN_CLASS_ROWS // 3


def fit_platt(raw, y):
    """Platt scaling: a 1-d logistic fit of the outcome on the raw margin."""
    return LogisticRegression(C=1e6, max_iter=1000).fit(
        np.asarray(raw, dtype=float).reshape(-1, 1), np.asarray(y, dtype=int))


def platt_slope(calibrator) -> float:
    return float(calibrator.coef_.ravel()[0])


def fit_platt_recent(raw, y, dates) -> tuple:
    """Platt calibration on the most recent held-out rows.

    `raw`, `y`, `dates`: margins, outcomes and posting dates of held-out rows
    (validation part, or out-of-fold rows), never rows the model was fitted on.
    Recent = posted in the C.CALIB_RECENT_DAYS days before the newest of them.
    Returns (calibrator, info); falls back to all rows when the recent rows have
    fewer than C.CALIB_MIN_CLASS_ROWS of a class or a non-increasing fit."""
    raw, y = np.asarray(raw, dtype=float), np.asarray(y, dtype=int)
    dates = pd.to_datetime(pd.Series(dates).reset_index(drop=True), utc=True)
    start = dates.max() - pd.Timedelta(days=C.CALIB_RECENT_DAYS)
    recent = (dates >= start).to_numpy()
    pos, neg = int(y[recent].sum()), int((1 - y[recent]).sum())
    info = {"rows_all": int(len(y)), "rows_recent": int(recent.sum()), "pos_recent": pos,
            "neg_recent": neg, "from": start, "span_days_all": float(
                (dates.max() - dates.min()).total_seconds() / 86400) if len(dates) else 0.0,
            "used": "recent", "note": None}
    if min(pos, neg) >= C.CALIB_MIN_CLASS_ROWS:
        cal = fit_platt(raw[recent], y[recent])
        if platt_slope(cal) > 0:
            return cal, info
        info["note"] = "recent-rows calibration would invert the ranking (slope <= 0)"
    else:
        info["note"] = (f"recent rows have {pos} positives / {neg} negatives "
                        f"(need {C.CALIB_MIN_CLASS_ROWS} of each)")
    info["used"] = "all (fallback)"
    return fit_platt(raw, y), info


def _brier(y, p) -> float:
    return float(np.mean((np.asarray(p, dtype=float) - np.asarray(y, dtype=float)) ** 2))


def choose_platt(raw, y, dates) -> tuple:
    """Choose the Platt calibration ("recent" window or "all" held-out rows) by
    an honest comparison on the held-out rows alone, then fit it on all of them.

    `raw`, `y`, `dates`: margins, outcomes and posting dates of HELD-OUT rows
    (validation part, or out-of-fold rows): never training rows, never test
    rows. The rows are ordered by time and cut at C.CALIB_CHOICE_FIT_FRAC (rows
    with the same timestamp stay on one side); both methods are fitted on the
    earlier part and their Brier score compared on the later part. The lower
    Brier wins; equal Brier, or any fallback below, gives "all".
    Returns (calibrator, info); info["used"] is "recent" or "all",
    info["choice"] has the comparison (or None) and info["note"] the reason
    when "all" was used without a comparison."""
    raw, y = np.asarray(raw, dtype=float), np.asarray(y, dtype=int)
    dates = pd.to_datetime(pd.Series(dates).reset_index(drop=True), utc=True)
    cal_recent, info = fit_platt_recent(raw, y, dates)
    info = {**info, "recent_fit": info["used"], "recent_note": info["note"],
            "used": "all", "note": None, "choice": None}

    def fallback(note):
        info["note"] = note
        return fit_platt(raw, y), info

    if info["recent_fit"] != "recent":
        return fallback(f"the recent window cannot be fitted ({info['recent_note']})")
    if info["rows_recent"] == info["rows_all"]:
        return fallback(f"the held-out rows span less than CALIB_RECENT_DAYS = "
                        f"{C.CALIB_RECENT_DAYS} days, so the recent window is all of them")
    ordered = dates.sort_values(kind="stable")
    cut = ordered.iloc[min(int(len(ordered) * C.CALIB_CHOICE_FIT_FRAC), len(ordered) - 1)]
    early, late = (dates < cut).to_numpy(), (dates >= cut).to_numpy()
    lpos, lneg = int(y[late].sum()), int((1 - y[late]).sum())
    choice = {"rows_fit": int(early.sum()), "rows_eval": int(late.sum()), "eval_from": cut,
              "pos_eval": lpos, "neg_eval": lneg}
    if min(lpos, lneg) < C.CALIB_MIN_CLASS_ROWS:
        return fallback(f"too few rows to compare: the later held-out part has {lpos} positives / "
                        f"{lneg} negatives (need {C.CALIB_MIN_CLASS_ROWS} of each)")
    e_raw, e_y, e_dates = raw[early], y[early], dates[early]
    if min(e_y.sum(), len(e_y) - e_y.sum()) < C.CALIB_MIN_CLASS_ROWS:
        return fallback(f"too few rows to compare: the earlier held-out part has {int(e_y.sum())} "
                        f"positives / {int(len(e_y) - e_y.sum())} negatives "
                        f"(need {C.CALIB_MIN_CLASS_ROWS} of each)")
    e_recent, e_info = fit_platt_recent(e_raw, e_y, e_dates)
    if e_info["used"] != "recent":
        return fallback(f"the recent window of the earlier held-out part cannot "
                        f"be fitted ({e_info['note']})")
    if e_info["rows_recent"] == e_info["rows_all"]:
        return fallback(f"the earlier held-out part (the first {C.CALIB_CHOICE_FIT_FRAC:.0%} of "
                        f"them) spans less than CALIB_RECENT_DAYS = {C.CALIB_RECENT_DAYS} days, so "
                        "its recent window is all of it")
    e_all = fit_platt(e_raw, e_y)
    choice.update(rows_fit_recent=e_info["rows_recent"],
                  brier_recent=_brier(y[late], apply_calibrator(e_recent, raw[late])),
                  brier_all=_brier(y[late], apply_calibrator(e_all, raw[late])))
    info["choice"] = choice
    if choice["brier_recent"] < choice["brier_all"]:
        info["used"] = "recent"
        return cal_recent, info
    return fit_platt(raw, y), info


def apply_calibrator(calibrator, raw) -> np.ndarray:
    raw = np.asarray(raw, dtype=float)
    if calibrator is None:
        return 1.0 / (1.0 + np.exp(-raw))
    return calibrator.predict_proba(raw.reshape(-1, 1))[:, 1]


def best_rounds(fitted: dict) -> int:
    m = fitted["model"]
    return int(m.best_iteration_ or m.n_estimators)


def predict(fitted: dict, X) -> np.ndarray:
    """Calibrated probability of the positive class."""
    return apply_calibrator(fitted.get("calibrator"), fitted["model"].predict(X, raw_score=True))


def rank_pct(score: float, ref_quantiles) -> float:
    """Percentile (0-100) of `score` within the stored reference quantiles."""
    q = np.asarray(ref_quantiles, dtype=float)
    below, not_above = np.searchsorted(q, score, "left"), np.searchsorted(q, score, "right")
    return float(100.0 * (below + not_above) / (2 * len(q)))


class Bundle:
    """One trained version loaded from disk: meta.json + the model files."""

    def __init__(self, version_dir):
        self.dir = Path(version_dir)
        self.meta = json.loads((self.dir / "meta.json").read_text())
        self.version = self.meta["version"]
        self.models = {name: joblib.load(self.dir / f"{name}.joblib")
                       for name in self.meta["models"]}

    def levels(self, bucket: str) -> dict:
        """Category levels the bucket's models were trained with.

        meta.json keeps one set per trained bucket (learned from that bucket's
        training rows); an older meta.json has one flat set for all buckets."""
        levels = self.meta.get("category_levels") or {}
        if self.flat_levels:
            return levels
        return levels.get(bucket, {})

    @property
    def flat_levels(self) -> bool:
        """Old meta.json layout: one flat set of levels, matched case-sensitively."""
        return bool(set(self.meta.get("category_levels") or {}) & set(ALL_CATEGORICAL))

    def features(self, row, bucket: str):
        """The feature matrix the bucket's models get for `row` (or rows)."""
        return build_features(row, self.levels(bucket), self.meta["features"],
                              exact_levels=self.flat_levels,
                              dex_rules=self.meta.get("dex_family_rules"))

    def score(self, row: dict) -> dict:
        """Score one raw view row -> the /score response body. A bucket
        without a runner model (skipped at training) is left out."""
        buckets, parts = [], [f"Model {self.version}"]
        for b, cfg in BUCKETS.items():
            if f"{b}_runner" not in self.models:
                continue
            X = self.features(row, b)
            runner = float(predict(self.models[f"{b}_runner"], X)[0])
            collapse = None
            text = f"{cfg['title']}: {cfg['runner_word']} {runner * 100:.0f}%"
            if f"{b}_collapse" in self.models:
                collapse = float(predict(self.models[f"{b}_collapse"], X)[0])
                text += f", collapse {collapse * 100:.0f}%"
            parts.append(text)
            buckets.append({
                "bucket": b, "runner_prob": round(runner, 4),
                "collapse_prob": None if collapse is None else round(collapse, 4),
                "runner_rank_pct": round(rank_pct(runner, self.meta["runner_reference"][b]), 1)})
        return {"model_version": self.version, "line": " · ".join(parts), "buckets": buckets}


def resolve_version_dir(model_dir) -> Path:
    """`model_dir/LATEST` names the version to serve; a version dir is used as is."""
    model_dir = Path(model_dir)
    if (model_dir / "meta.json").exists():
        return model_dir
    return model_dir / (model_dir / "LATEST").read_text().strip()
