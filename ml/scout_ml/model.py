"""Model fitting, calibrated prediction, and the loaded bundle used for serving."""
import json
import warnings
from pathlib import Path

import joblib
import lightgbm as lgb
import numpy as np
from sklearn.compose import ColumnTransformer
from sklearn.impute import SimpleImputer
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import FunctionTransformer, StandardScaler

from .config import (BUCKETS, CATEGORICAL, EARLY_STOPPING_ROUNDS, FALLBACK_ROUNDS,
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
    usable_val = (n_estimators is None and y_val is not None
                  and min(y_val.sum(), len(y_val) - y_val.sum()) >= MIN_CLASS_ROWS // 3)
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
        model.fit(X_train, np.asarray(y_train, dtype=int), categorical_feature=CATEGORICAL, **fit_kw)
    calibrator, note = None, None
    if usable_val:
        raw = model.predict(X_val, raw_score=True).reshape(-1, 1)
        calibrator = LogisticRegression(C=1e6, max_iter=1000).fit(raw, y_val)
    elif n_estimators is None:
        note = ("validation part too small for early stopping/calibration: "
                f"fixed {FALLBACK_ROUNDS} rounds, probabilities uncalibrated")
    return {"model": model, "calibrator": calibrator, "note": note}


def predict(fitted: dict, X) -> np.ndarray:
    """Calibrated probability of the positive class."""
    raw = fitted["model"].predict(X, raw_score=True)
    if fitted["calibrator"] is not None:
        return fitted["calibrator"].predict_proba(raw.reshape(-1, 1))[:, 1]
    return 1.0 / (1.0 + np.exp(-raw))


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

    def score(self, row: dict) -> dict:
        """Score one raw view row -> the /score response body."""
        X = build_features(row, self.meta["category_levels"], self.meta["features"])
        buckets, parts = [], [f"Model {self.version}"]
        for b, cfg in BUCKETS.items():
            if f"{b}_runner" not in self.models:
                continue
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
