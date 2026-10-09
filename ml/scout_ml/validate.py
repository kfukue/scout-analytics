"""Time-ordered validation: splits + embargo + token grouping, purged k-fold
inside the train part (long horizons), and the metrics.

Random splits would leak twice here: the same token is called repeatedly, and
a call's outcome window (up to 30 days) can reach into the later period."""
import numpy as np
import pandas as pd
from sklearn.metrics import average_precision_score, brier_score_loss, roc_auc_score

from .config import (FORWARD_TEST_DAYS, GATES, KFOLD_K, LIFT_CI_Z, MIN_WINDOW_TEST_ROWS,
                     SIM_MAX_RET_PCT, SKIP_FRAC, TOP_FRAC, TRAIN_FRAC, VAL_FRAC)


def _token_key(tokens: pd.Series) -> pd.Series:
    """Contract address without regard to letter case (the website's rule)."""
    return pd.Series(tokens).astype(str).str.lower()


def time_split(dates: pd.Series, tokens: pd.Series, horizon_days: float) -> dict:
    """Boolean masks `train`/`val`/`test` (aligned to `dates`) plus boundaries.

    * boundaries t1/t2 are the message_date at 70% and 85% of the rows;
    * a token belongs to the side where its first call falls (all its calls);
    * train rows posted after t1 - horizon and validation rows posted after
      t2 - horizon are dropped, so no outcome window overlaps a later part.
    """
    dates = pd.to_datetime(dates, utc=True)
    tokens = _token_key(tokens)
    ordered = dates.sort_values()
    n = len(ordered)
    t1 = ordered.iloc[min(int(n * TRAIN_FRAC), n - 1)]
    t2 = ordered.iloc[min(int(n * (TRAIN_FRAC + VAL_FRAC)), n - 1)]
    first = dates.groupby(tokens.to_numpy()).transform("min")
    gap = pd.Timedelta(days=horizon_days)
    parts = {"train": ((first < t1) & (dates < t1 - gap)).to_numpy(),
             "val": ((first >= t1) & (first < t2) & (dates < t2 - gap)).to_numpy(),
             "test": (first >= t2).to_numpy()}
    zones = {"train": (dates < t1 - gap).to_numpy(),
             "val": ((dates >= t1) & (dates < t2 - gap)).to_numpy(),
             "test": (dates >= t2).to_numpy()}
    return {**parts, "t1": t1, "t2": t2, **dropped_counts(dates, tokens, parts, zones)}


def dropped_counts(dates: pd.Series, tokens: pd.Series, parts: dict, zones: dict) -> dict:
    """Rows in no part, split by cause. `zones` are the parts by posting date
    alone; a row outside every zone was dropped by the embargo, a row inside a
    zone but in no part by token grouping (its token's first call is on
    another side; with first calls only this is always 0)."""
    in_zone = np.logical_or.reduce([np.asarray(z) for z in zones.values()])
    kept = np.logical_or.reduce([np.asarray(p) for p in parts.values()])
    return {"n_dropped_embargo": int((~in_zone).sum()),
            "n_dropped_token": int((in_zone & ~kept).sum())}


def forward_split(dates: pd.Series, tokens: pd.Series, horizon_days: float,
                  test_days: float = FORWARD_TEST_DAYS) -> dict:
    """Masks `train`/`test` for long horizons; no validation part.

    * the test part starts at a date: midnight UTC `test_days` days before the
      newest call (time-based, not a row quantile), and runs to the end;
    * train = calls posted before that date minus the horizon (embargo), so
      no train outcome window reaches into the test period;
    * token grouping as in `time_split`.
    Rounds and calibration come from `purged_kfold` on the train rows only."""
    dates = pd.to_datetime(dates, utc=True)
    first = dates.groupby(_token_key(tokens).to_numpy()).transform("min")
    t_test = (dates.max() - pd.Timedelta(days=test_days)).floor("D")
    cut = t_test - pd.Timedelta(days=horizon_days)
    parts = {"train": ((first < cut) & (dates < cut)).to_numpy(),
             "test": (first >= t_test).to_numpy()}
    zones = {"train": (dates < cut).to_numpy(), "test": (dates >= t_test).to_numpy()}
    return {**parts, "t_train_end": cut, "t_test": t_test,
            **dropped_counts(dates, tokens, parts, zones)}


def purged_kfold(dates: pd.Series, tokens: pd.Series, horizon_days: float, k: int = KFOLD_K):
    """Time-ordered k-fold for rounds and calibration INSIDE a train part.

    Yields {"fold", "fit", "held", "lo", "hi"} (masks aligned to `dates`).
    Folds are contiguous in time with about equal row counts (calls with the
    same timestamp stay together). Purge: a fit row's outcome window
    [t, t + horizon] must not overlap any held-out row's window, so rows posted
    within one horizon before the fold's first or after its last call are
    dropped from the fit part (both sides); calls of a
    held-out token are dropped from the fit part too. Never used for the final
    test, which is forward only (`forward_split`)."""
    dates = pd.to_datetime(dates, utc=True).reset_index(drop=True)
    tok = _token_key(tokens).reset_index(drop=True)
    gap = pd.Timedelta(days=horizon_days)
    ordered = dates.sort_values().reset_index(drop=True)
    n = len(ordered)
    if n == 0:
        return
    bounds = [ordered.iloc[min(i * n // k, n - 1)] for i in range(k)]
    for i in range(k):
        lo, hi = bounds[i], bounds[i + 1] if i + 1 < k else None
        held = (dates >= lo) if hi is None else ((dates >= lo) & (dates < hi))
        if not held.any():
            continue
        start, end = dates[held].min(), dates[held].max()
        fit = ((dates < start - gap) | (dates > end + gap)) & ~tok.isin(set(tok[held]))
        yield {"fold": i + 1, "fit": fit.to_numpy(), "held": held.to_numpy(),
               "start": start, "end": end}


def walk_forward_windows(dates: pd.Series, tokens: pd.Series, horizon_days: float):
    """Expanding weekly windows: train on everything before week N+1 (minus the
    embargo), test on week N+1. Same token-grouping rule as `time_split`."""
    dates = pd.to_datetime(dates, utc=True)
    first = dates.groupby(_token_key(tokens).to_numpy()).transform("min")
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


def wilson(k: int, n: int, z: float = LIFT_CI_Z) -> tuple:
    """Wilson score interval for a proportion k/n ((nan, nan) when n == 0)."""
    if n <= 0:
        return np.nan, np.nan
    p = k / n
    centre = (p + z * z / (2 * n)) / (1 + z * z / n)
    half = z * np.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / (1 + z * z / n)
    return float(max(0.0, centre - half)), float(min(1.0, centre + half))


def lift_interval(top_pos: int, top_n: int, all_pos: int, all_n: int) -> tuple:
    """Approximate 95% interval of the top-10% lift: the Wilson interval of
    the runner rate in the top 10%, divided by the base rate of the same part
    (treated as fixed)."""
    if all_n <= 0 or all_pos <= 0:
        return np.nan, np.nan
    lo, hi = wilson(top_pos, top_n)
    base = all_pos / all_n
    return lo / base, hi / base


def trading_metrics(y_runner, runner_score, y_collapse, collapse_score, net_ret) -> dict:
    """The numbers a trader cares about. Any input may be None (model skipped).

    lift            = runner rate in the top 10% by runner score / base rate,
                      with an approximate 95% interval (`lift_interval`) and the
                      runner counts in the top 10% and overall
    collapse_removed= share of all collapses that sit in the 30% of calls with
                      the highest collapse score (i.e. avoided by skipping them)
    sim_*           = mean net return of the top 10% vs buying every call, each
                      call's net return capped at SIM_MAX_RET_PCT (so that one
                      huge winner cannot decide the comparison on its own)
    """
    out = {"top_lift": np.nan, "top_lift_lo": np.nan, "top_lift_hi": np.nan, "top_pos": 0,
           "top_n": 0, "all_pos": 0, "all_n": 0, "collapse_removed": np.nan,
           "sim_top_mean": np.nan, "sim_all_mean": np.nan, "sim_n_top": 0, "sim_beats_all": False}
    if runner_score is not None and len(runner_score):
        y, top = np.asarray(y_runner, dtype=float), top_mask(runner_score, TOP_FRAC)
        ret = np.minimum(np.asarray(net_ret, dtype=float), SIM_MAX_RET_PCT)
        counts = dict(top_pos=int(y[top].sum()), top_n=int(top.sum()),
                      all_pos=int(y.sum()), all_n=int(len(y)))
        out.update(counts)
        if y.mean() > 0:
            out["top_lift"] = float(y[top].mean() / y.mean())
            out["top_lift_lo"], out["top_lift_hi"] = lift_interval(**counts)
        out.update(sim_top_mean=float(ret[top].mean()), sim_all_mean=float(ret.mean()),
                   sim_n_top=int(top.sum()), sim_beats_all=bool(ret[top].mean() > ret.mean()))
    if collapse_score is not None and len(collapse_score):
        out["collapse_removed"] = collapse_removed(y_collapse, collapse_score)
    return out


def collapse_removed(y_collapse, collapse_score) -> float:
    """Share of all collapses among the SKIP_FRAC highest collapse scores (NaN
    without a score or without collapses)."""
    if collapse_score is None or not len(collapse_score):
        return np.nan
    y = np.asarray(y_collapse, dtype=float)
    if y.sum() <= 0:
        return np.nan
    return float(y[top_mask(collapse_score, SKIP_FRAC)].sum() / y.sum())


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
