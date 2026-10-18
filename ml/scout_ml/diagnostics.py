"""Pooled walk-forward with a week-block bootstrap, spreads over seeds, and
adversarial validation (train vs test).

Pure functions on arrays; train.py collects the arrays and renders nothing.
Since the owner decision of 13 Oct 2026, `week_sums` and `pooled` compute the
bucket gate inputs (train.gate_inputs, judged by validate.pooled_gates): the
pooled walk-forward lift (also per seed), its bootstrap interval and the pooled
simulation difference. The rest stays report only (seed spreads via `spread`,
`adversarial`, and the rolling and no-dead walk-forward tables train.py builds
with `pooled`): it only goes into report.md and the "diagnostics" part of
meta.json, and is never served or used by a gate."""
import lightgbm as lgb
import numpy as np
import pandas as pd
from sklearn.metrics import roc_auc_score
from sklearn.model_selection import StratifiedKFold

from . import config as C   # every setting read at call time
from . import validate as V

# Per-week sums the pooled numbers (and their bootstrap) are built from. The
# last three are the collapse filter's (report only): calls flagged (the
# C.SKIP_FRAC highest collapse scores, the calls whose collapses `coll_top`
# counts), runners among them and their capped net returns.
_SUMS = ("top_pos", "top_n", "all_pos", "all_n", "coll_top", "coll_all", "ret_top", "ret_all",
         "flag_n", "run_flag", "ret_flag")
_FILTER_SUMS = ("flag_n", "run_flag", "ret_flag")


def week_sums(y_runner, s_runner, y_collapse, s_collapse, net_ret, keep=None) -> dict:
    """Sums of one walk-forward week (rows of its test part), ranked by that
    week's own model: runners in the top C.TOP_FRAC by runner score and overall,
    collapses (plain label) among the C.SKIP_FRAC highest collapse scores and
    overall, and the capped net returns of the top and of all rows; for the
    collapse filter (report only) the flagged calls (those C.SKIP_FRAC
    highest collapse scores), the runners among them and their capped net
    returns.
    `keep` (bool mask) restricts the week to those rows first, and the top
    shares are taken among them (e.g. dead calls left out). `s_collapse` None:
    no collapse model (collapse and filter sums are NaN)."""
    y = np.asarray(y_runner, dtype=float)
    keep = np.ones(len(y), bool) if keep is None else np.asarray(keep, bool)
    y, s = y[keep], np.asarray(s_runner, dtype=float)[keep]
    ret = np.minimum(np.asarray(net_ret, dtype=float)[keep], C.SIM_MAX_RET_PCT)
    out = dict.fromkeys(_SUMS, 0.0)
    out["n"] = int(len(y))
    if len(y):
        top = V.top_mask(s, C.TOP_FRAC)
        out.update(top_pos=float(y[top].sum()), top_n=float(top.sum()), all_pos=float(y.sum()),
                   all_n=float(len(y)), ret_top=float(ret[top].sum()), ret_all=float(ret.sum()))
    if s_collapse is None:
        for k in ("coll_top", "coll_all") + _FILTER_SUMS:
            out[k] = np.nan
    elif len(y):
        yc = np.asarray(y_collapse, dtype=float)[keep]
        flag = V.top_mask(np.asarray(s_collapse, dtype=float)[keep], C.SKIP_FRAC)
        f = V.collapse_filter_sums(yc, y, ret, flag)
        out.update(coll_top=f["coll_flag"], coll_all=f["coll_all"], flag_n=f["flag_n"],
                   run_flag=f["run_flag"], ret_flag=f["ret_flag"])
    return out


_div = V._ratio   # a / b where b > 0, else NaN (one helper, shared with validate)


def _stats(S: np.ndarray) -> dict:
    """Pooled numbers from summed columns (last axis in _SUMS order); the
    collapse filter's rates (V.collapse_filter_rates) with the prefix "filter_"."""
    c = {k: S[..., i] for i, k in enumerate(_SUMS)}
    top_rate, base = _div(c["top_pos"], c["top_n"]), _div(c["all_pos"], c["all_n"])
    sim_top, sim_all = _div(c["ret_top"], c["top_n"]), _div(c["ret_all"], c["all_n"])
    flt = V.collapse_filter_rates({"n": c["all_n"], "flag_n": c["flag_n"], "coll_all": c["coll_all"],
                                   "coll_flag": c["coll_top"], "run_all": c["all_pos"],
                                   "run_flag": c["run_flag"], "ret_all": c["ret_all"],
                                   "ret_flag": c["ret_flag"]})
    return {"lift": _div(top_rate, base), "collapse_removed": _div(c["coll_top"], c["coll_all"]),
            "sim_top_mean": sim_top, "sim_all_mean": sim_all, "sim_gain": sim_top - sim_all,
            **{f"filter_{k}": v for k, v in flt.items()}}


def without_filter(p: dict) -> dict:
    """`p` (from `pooled`) without the collapse filter's "filter_*" keys."""
    return {k: v for k, v in p.items() if not k.startswith("filter_")}


def pooled(weeks: list, reps: int | None = None, seed: int | None = None,
           with_filter: bool = False) -> dict:
    """Pool the per-week sums of `weeks` (from `week_sums`) and add a 95%
    week-block bootstrap interval (C.POOLED_CI): the weeks are resampled with
    replacement `reps` times (default C.POOLED_BOOTSTRAP_REPS, generator seeded
    with C.POOLED_BOOTSTRAP_SEED) and the pooled numbers recomputed. Pooled
    lift = runner rate in the per-week tops / runner rate of all rows. The
    interval needs at least 2 weeks (with 1 it is n/a); resamples whose numbers
    are undefined (NaN, e.g. no runner drawn, so no lift) are dropped.
    `with_filter`: also the collapse filter's rates ("filter_*" keys, report
    only; V.collapse_filter_rates), from the same resamples."""
    reps = C.POOLED_BOOTSTRAP_REPS if reps is None else reps
    seed = C.POOLED_BOOTSTRAP_SEED if seed is None else seed
    out = {"weeks": len(weeks), "rows": int(sum(w["n"] for w in weeks)), "reps": reps,
           "seed": seed}
    if not weeks:
        return out
    A = np.array([[w[k] for k in _SUMS] for w in weeks], dtype=float)
    total = A.sum(axis=0)
    point = _stats(total)
    if not with_filter:
        point = without_filter(point)
    out.update({k: float(v) for k, v in point.items()})
    out.update(top_pos=int(total[0]), top_n=int(total[1]), all_pos=int(total[2]),
               all_n=int(total[3]))
    out["weeks_beating"] = int(sum(_div(w["ret_top"], w["top_n"]) > _div(w["ret_all"], w["all_n"])
                                   for w in weeks))
    lo_q, hi_q = (q * 100 for q in C.POOLED_CI)
    if len(weeks) < 2 or reps <= 0:
        for k in point:
            out[f"{k}_lo"] = out[f"{k}_hi"] = np.nan
        return out
    rng = np.random.default_rng(seed)
    idx = rng.integers(0, len(weeks), size=(reps, len(weeks)))
    boot = _stats(A[idx].sum(axis=1))
    for k in point:
        v = boot[k]
        ok = v[~np.isnan(v)]
        out[f"{k}_lo"] = float(np.percentile(ok, lo_q)) if len(ok) else np.nan
        out[f"{k}_hi"] = float(np.percentile(ok, hi_q)) if len(ok) else np.nan
    return out


def spread(values) -> dict:
    """min / median / max of the non-NaN values (NaN when there is none)."""
    v = np.asarray([np.nan if x is None else x for x in values], dtype=float)
    v = v[~np.isnan(v)]
    if not len(v):
        return {"min": np.nan, "median": np.nan, "max": np.nan, "n": 0}
    return {"min": float(v.min()), "median": float(np.median(v)), "max": float(v.max()),
            "n": int(len(v))}


def adversarial(X: pd.DataFrame, is_test, folds: int | None = None) -> dict:
    """Can a classifier tell the test rows (1) from the train rows (0) on the
    model inputs `X` (without the calendar inputs C.ADV_EXCLUDE)? ROC AUC of
    out-of-fold predictions (`folds`, default C.ADV_FOLDS; stratified, shuffled
    folds seeded with C.ADV_PARAMS["random_state"]: a drift probe, not a
    forecast; 0.5 = no detectable drift) and the features a model fitted on
    all rows uses most (gain share, top C.ADV_TOP_FEATURES)."""
    folds = C.ADV_FOLDS if folds is None else folds
    X = X[[c for c in X.columns if c not in C.ADV_EXCLUDE]]
    y = np.asarray(is_test, dtype=int)
    out = {"n_train": int((y == 0).sum()), "n_test": int(y.sum()), "auc": np.nan, "top": []}
    if min(out["n_train"], out["n_test"]) < folds:
        out["skipped"] = f"too few rows ({out['n_train']} train, {out['n_test']} test)"
        return out
    cats = [c for c in X.columns if c in C.ALL_CATEGORICAL]
    oof = np.zeros(len(y))
    seed = C.ADV_PARAMS.get("random_state", 0)
    for fit, held in StratifiedKFold(folds, shuffle=True, random_state=seed).split(X, y):
        m = lgb.LGBMClassifier(**C.ADV_PARAMS).fit(X.iloc[fit], y[fit], categorical_feature=cats)
        oof[held] = m.predict(X.iloc[held], raw_score=True)
    out["auc"] = float(roc_auc_score(y, oof))
    m = lgb.LGBMClassifier(**C.ADV_PARAMS).fit(X, y, categorical_feature=cats)
    gain = m.booster_.feature_importance("gain")
    order = np.argsort(-gain, kind="stable")[:C.ADV_TOP_FEATURES]
    total = max(float(gain.sum()), 1e-12)
    out["top"] = [[X.columns[i], float(gain[i] / total)] for i in order if gain[i] > 0]
    return out
