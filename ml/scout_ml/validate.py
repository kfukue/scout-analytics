"""Time-ordered validation: split + embargo + token grouping, and the metrics.

Random splits would leak twice here: the same token is called repeatedly, and
a call's outcome window (up to 30 days) can reach into the later period."""
import numpy as np
import pandas as pd
from sklearn.metrics import average_precision_score, brier_score_loss, roc_auc_score

from .config import GATES, MIN_WINDOW_TEST_ROWS, SKIP_FRAC, TOP_FRAC, TRAIN_FRAC, VAL_FRAC


def time_split(dates: pd.Series, tokens: pd.Series, horizon_days: float) -> dict:
    """Boolean masks `train`/`val`/`test` (aligned to `dates`) plus boundaries.

    * boundaries t1/t2 are the message_date at 70% and 85% of the rows;
    * a token belongs to the side where its first call falls (all its calls);
    * train rows posted after t1 - horizon and validation rows posted after
      t2 - horizon are dropped, so no outcome window overlaps a later part.
    """
    dates = pd.to_datetime(dates, utc=True)
    ordered = dates.sort_values()
    n = len(ordered)
    t1 = ordered.iloc[min(int(n * TRAIN_FRAC), n - 1)]
    t2 = ordered.iloc[min(int(n * (TRAIN_FRAC + VAL_FRAC)), n - 1)]
    first = dates.groupby(tokens.to_numpy()).transform("min")
    gap = pd.Timedelta(days=horizon_days)
    return {"train": ((first < t1) & (dates < t1 - gap)).to_numpy(),
            "val": ((first >= t1) & (first < t2) & (dates < t2 - gap)).to_numpy(),
            "test": (first >= t2).to_numpy(), "t1": t1, "t2": t2}


def walk_forward_windows(dates: pd.Series, tokens: pd.Series, horizon_days: float):
    """Expanding weekly windows: train on everything before week N+1 (minus the
    embargo), test on week N+1. Same token-grouping rule as `time_split`."""
    dates = pd.to_datetime(dates, utc=True)
    first = dates.groupby(tokens.to_numpy()).transform("min")
    gap, week = pd.Timedelta(days=horizon_days), pd.Timedelta(days=7)
    start, k = dates.min().floor("D"), 1
    while start + k * week <= dates.max():
        lo, hi = start + k * week, start + (k + 1) * week
        yield {"week": k + 1, "start": lo,
               "train": ((first < lo) & (dates < lo - gap)).to_numpy(),
               "test": ((first >= lo) & (dates >= lo) & (dates < hi)).to_numpy()}
        k += 1


def model_metrics(y, p) -> dict:
    """Base rate, ROC AUC, PR AUC and Brier score (NaN where undefined)."""
    y, p = np.asarray(y, dtype=float), np.asarray(p, dtype=float)
    both = len(y) > 0 and 0 < y.sum() < len(y)
    return {"n": int(len(y)), "base_rate": float(y.mean()) if len(y) else np.nan,
            "roc_auc": float(roc_auc_score(y, p)) if both else np.nan,
            "pr_auc": float(average_precision_score(y, p)) if both else np.nan,
            "brier": float(brier_score_loss(y, p)) if len(y) else np.nan}


def calibration_table(y, p, bins: int = 10) -> list:
    """Deciles of predicted probability: mean prediction vs observed rate."""
    d = pd.DataFrame({"y": np.asarray(y, dtype=float), "p": np.asarray(p, dtype=float)})
    if len(d) < bins:
        return []
    d["decile"] = pd.qcut(d["p"].rank(method="first"), bins, labels=False) + 1
    g = d.groupby("decile").agg(n=("y", "size"), mean_pred=("p", "mean"), observed=("y", "mean"))
    return [{"decile": int(i), "n": int(r.n), "mean_pred": float(r.mean_pred),
             "observed": float(r.observed)} for i, r in g.iterrows()]


def top_mask(score, frac: float) -> np.ndarray:
    """Mask of the ceil(frac * n) highest scores (at least one row)."""
    score = np.asarray(score, dtype=float)
    mask = np.zeros(len(score), dtype=bool)
    if len(score):
        mask[np.argsort(-score, kind="stable")[:max(1, int(np.ceil(len(score) * frac)))]] = True
    return mask


def trading_metrics(y_runner, runner_score, y_collapse, collapse_score, net_ret) -> dict:
    """The numbers a trader cares about. Any input may be None (model skipped).

    lift            = runner rate in the top 10% by runner score / base rate
    collapse_removed= share of all collapses that sit in the 30% of calls with
                      the highest collapse score (i.e. avoided by skipping them)
    sim_*           = mean net return of the top 10% vs buying every call
    """
    out = {"top_lift": np.nan, "collapse_removed": np.nan, "sim_top_mean": np.nan,
           "sim_all_mean": np.nan, "sim_n_top": 0, "sim_beats_all": False}
    if runner_score is not None and len(runner_score):
        y, top = np.asarray(y_runner, dtype=float), top_mask(runner_score, TOP_FRAC)
        ret = np.asarray(net_ret, dtype=float)
        if y.mean() > 0:
            out["top_lift"] = float(y[top].mean() / y.mean())
        out.update(sim_top_mean=float(ret[top].mean()), sim_all_mean=float(ret.mean()),
                   sim_n_top=int(top.sum()), sim_beats_all=bool(ret[top].mean() > ret.mean()))
    if collapse_score is not None and len(collapse_score):
        y = np.asarray(y_collapse, dtype=float)
        if y.sum() > 0:
            out["collapse_removed"] = float(y[top_mask(collapse_score, SKIP_FRAC)].sum() / y.sum())
    return out


def gates(test: dict, windows: list) -> dict:
    """PASS needs all three: lift, collapse removal, and every evaluated
    walk-forward window beating buy-everything (no evaluated window = fail)."""
    evaluated = [w for w in windows if not w.get("skipped")]
    g = {"lift_ok": bool(test["top_lift"] >= GATES["min_top_lift"]),
         "collapse_ok": bool(test["collapse_removed"] >= GATES["min_collapse_removed"]),
         "walk_forward_ok": bool(evaluated) and all(w["sim_beats_all"] for w in evaluated),
         "windows_evaluated": len(evaluated),
         "windows_beating": sum(bool(w["sim_beats_all"]) for w in evaluated)}
    g["passed"] = g["lift_ok"] and g["collapse_ok"] and g["walk_forward_ok"]
    return g


def window_ok(n_test: int) -> bool:
    return n_test >= MIN_WINDOW_TEST_ROWS
