"""Time-ordered validation: splits + embargo + token grouping, purged k-fold
inside the train part (long horizons), and the metrics.

Random splits would leak twice here: the same token is called repeatedly, and
a call's outcome window (up to 30 days) can reach into the later period."""
import numpy as np
import pandas as pd
from sklearn.metrics import average_precision_score, brier_score_loss, roc_auc_score

# Every setting is read from C at call time (also the defaults of keyword
# arguments), never bound at import, so a knob set after import takes effect.
from . import config as C


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
    t1 = ordered.iloc[min(int(n * C.TRAIN_FRAC), n - 1)]
    t2 = ordered.iloc[min(int(n * (C.TRAIN_FRAC + C.VAL_FRAC)), n - 1)]
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
                  test_days: float | None = None) -> dict:
    """Masks `train`/`test` for long horizons; no validation part.

    * the test part starts at a date: midnight UTC `test_days` days before the
      newest call (time-based, not a row quantile), and runs to the end
      (`test_days` None: C.FORWARD_TEST_DAYS);
    * train = calls posted before that date minus the horizon (embargo), so
      no train outcome window reaches into the test period;
    * token grouping as in `time_split`.
    Rounds and calibration come from `purged_kfold` on the train rows only."""
    test_days = C.FORWARD_TEST_DAYS if test_days is None else test_days
    dates = pd.to_datetime(dates, utc=True)
    first = dates.groupby(_token_key(tokens).to_numpy()).transform("min")
    t_test = (dates.max() - pd.Timedelta(days=test_days)).floor("D")
    cut = t_test - pd.Timedelta(days=horizon_days)
    parts = {"train": ((first < cut) & (dates < cut)).to_numpy(),
             "test": (first >= t_test).to_numpy()}
    zones = {"train": (dates < cut).to_numpy(), "test": (dates >= t_test).to_numpy()}
    return {**parts, "t_train_end": cut, "t_test": t_test,
            **dropped_counts(dates, tokens, parts, zones)}


def purged_kfold(dates: pd.Series, tokens: pd.Series, horizon_days: float, k: int | None = None):
    """Time-ordered k-fold for rounds and calibration INSIDE a train part.

    Yields {"fold", "fit", "held", "lo", "hi"} (masks aligned to `dates`).
    Folds are contiguous in time with about equal row counts (calls with the
    same timestamp stay together). Purge: a fit row's outcome window
    [t, t + horizon] must not overlap any held-out row's window, so rows posted
    within one horizon before the fold's first or after its last call are
    dropped from the fit part (both sides); calls of a
    held-out token are dropped from the fit part too. Never used for the final
    test, which is forward only (`forward_split`). `k` None: C.KFOLD_K."""
    k = C.KFOLD_K if k is None else k
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


def walk_forward_windows(dates: pd.Series, tokens: pd.Series, horizon_days: float,
                         train_weeks: float | None = None):
    """Expanding weekly windows: train on everything before week N+1 (minus the
    embargo), test on week N+1. Same token-grouping rule as `time_split`.
    `train_weeks` (rolling walk-forward, report only): train only on calls
    posted in the `train_weeks` weeks before the embargo cut, i.e. in
    [week start - horizon - train_weeks * 7 days, week start - horizon); the
    test weeks are the same as for the expanding windows."""
    dates = pd.to_datetime(dates, utc=True)
    first = dates.groupby(_token_key(tokens).to_numpy()).transform("min")
    gap, week = pd.Timedelta(days=horizon_days), pd.Timedelta(days=7)
    start, k = dates.min().floor("D"), 1
    while start + k * week <= dates.max():
        lo, hi = start + k * week, start + (k + 1) * week
        train = (first < lo) & (dates < lo - gap)
        if train_weeks is not None:
            train &= dates >= lo - gap - train_weeks * week
        yield {"week": k + 1, "start": lo, "train": train.to_numpy(),
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


def wilson(k: int, n: int, z: float | None = None) -> tuple:
    """Wilson score interval for a proportion k/n ((nan, nan) when n == 0;
    `z` None: C.LIFT_CI_Z)."""
    z = C.LIFT_CI_Z if z is None else z
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
        y, top = np.asarray(y_runner, dtype=float), top_mask(runner_score, C.TOP_FRAC)
        ret = np.minimum(np.asarray(net_ret, dtype=float), C.SIM_MAX_RET_PCT)
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
    return float(y[top_mask(collapse_score, C.SKIP_FRAC)].sum() / y.sum())


# --- Collapse filter (never a gate): what skipping the flagged calls catches and costs ---

def _ratio(a, b):
    """a / b where b > 0, else NaN (scalars or arrays; NaN inputs stay NaN)."""
    a, b = np.asarray(a, dtype=float), np.asarray(b, dtype=float)
    with np.errstate(divide="ignore", invalid="ignore"):
        return np.where(b > 0, a / np.where(b > 0, b, 1.0), np.nan)


def collapse_filter_sums(y_collapse, y_runner, ret, flag) -> dict:
    """The sums the collapse filter numbers are built from: calls `n`, flagged
    calls `flag_n`, collapses and runners overall and among the flagged calls,
    and the net returns of all / the flagged calls, each capped at
    C.SIM_MAX_RET_PCT (as the money simulation)."""
    flag = np.asarray(flag, dtype=bool)
    yc, yr = np.asarray(y_collapse, dtype=float), np.asarray(y_runner, dtype=float)
    ret = np.minimum(np.asarray(ret, dtype=float), C.SIM_MAX_RET_PCT)
    return {"n": float(len(flag)), "flag_n": float(flag.sum()),
            "coll_all": float(yc.sum()), "coll_flag": float(yc[flag].sum()),
            "run_all": float(yr.sum()), "run_flag": float(yr[flag].sum()),
            "ret_all": float(ret.sum()), "ret_flag": float(ret[flag].sum())}


def collapse_filter_rates(s: dict) -> dict:
    """The filter's rates from `collapse_filter_sums` (values may be arrays,
    e.g. bootstrap resamples; NaN where a denominator is 0). Kept = not flagged.

    flagged_share          flagged calls / calls
    collapses_caught       collapses among the flagged / all collapses
                           (= "collapses removed" by skipping them)
    collapse_rate_flagged  collapse rate among the flagged calls
    collapse_rate_kept     collapse rate among the kept calls
    runners_lost           runners among the flagged / all runners
    runner_rate_flagged    runner rate among the flagged calls
    runner_rate_kept       runner rate among the kept calls
    all_mean / kept_mean   mean capped net return of all / the kept calls (%)
    kept_minus_all         kept_mean - all_mean"""
    kept_n = np.asarray(s["n"], dtype=float) - np.asarray(s["flag_n"], dtype=float)
    coll_kept = np.asarray(s["coll_all"], dtype=float) - np.asarray(s["coll_flag"], dtype=float)
    run_kept = np.asarray(s["run_all"], dtype=float) - np.asarray(s["run_flag"], dtype=float)
    ret_kept = np.asarray(s["ret_all"], dtype=float) - np.asarray(s["ret_flag"], dtype=float)
    all_mean, kept_mean = _ratio(s["ret_all"], s["n"]), _ratio(ret_kept, kept_n)
    return {"flagged_share": _ratio(s["flag_n"], s["n"]),
            "collapses_caught": _ratio(s["coll_flag"], s["coll_all"]),
            "collapse_rate_flagged": _ratio(s["coll_flag"], s["flag_n"]),
            "collapse_rate_kept": _ratio(coll_kept, kept_n),
            "runners_lost": _ratio(s["run_flag"], s["run_all"]),
            "runner_rate_flagged": _ratio(s["run_flag"], s["flag_n"]),
            "runner_rate_kept": _ratio(run_kept, kept_n),
            "all_mean": all_mean, "kept_mean": kept_mean, "kept_minus_all": kept_mean - all_mean}


FILTER_RATES = ("flagged_share", "collapses_caught", "collapse_rate_flagged", "collapse_rate_kept",
                "runners_lost", "runner_rate_flagged", "runner_rate_kept", "all_mean", "kept_mean",
                "kept_minus_all")   # the keys of collapse_filter_rates
FILTER_COUNTS = {"calls": "n", "flagged": "flag_n", "collapses": "coll_all",
                 "collapses_flagged": "coll_flag", "runners": "run_all", "runners_flagged": "run_flag"}


def collapse_cutoff(scores, frac: float | None = None) -> float:
    """The fixed collapse-score cut-off of the filter: the (1 - frac) quantile
    of `scores` (`frac` None: C.SKIP_FRAC), so about `frac` of them are >= it.
    NaN when there is no score."""
    s = np.asarray(scores, dtype=float)
    if not len(s):
        return np.nan
    frac = C.SKIP_FRAC if frac is None else frac
    return float(np.quantile(s, 1 - frac))


def collapse_filter_stats(y_collapse, y_runner, ret, flag) -> dict:
    """The collapse filter on one set of calls: `flag` marks the calls it would
    skip (the C.SKIP_FRAC highest collapse scores, `top_mask`). Counts
    (FILTER_COUNTS keys, ints) plus the `collapse_filter_rates` (floats, NaN
    where undefined). `ret` is capped at C.SIM_MAX_RET_PCT."""
    s = collapse_filter_sums(y_collapse, y_runner, ret, flag)
    return {**{k: int(s[v]) for k, v in FILTER_COUNTS.items()},
            **{k: float(v) for k, v in collapse_filter_rates(s).items()}}


def wf_enough_train(w: dict) -> bool:
    """The week's training part has at least C.WF_MIN_TRAIN_ROWS rows (`n_train`
    is required: a week without it is an error, never silently counted)."""
    return w["n_train"] >= C.WF_MIN_TRAIN_ROWS


def wf_counted(w: dict) -> bool:
    """A walk-forward week counts toward the gate and the walk-forward
    summaries: evaluated (not skipped) and trained on >= C.WF_MIN_TRAIN_ROWS rows."""
    return not w.get("skipped") and wf_enough_train(w)


def gates(test: dict, windows: list) -> dict:
    """The OLD single-run gates (the verdict until 13 Oct 2026; now information,
    and the gate column of the --variants rows and the seed table): PASS needs
    all three: test-part lift, collapse removal, and every COUNTED walk-forward
    window beating buy-everything (no counted window = fail). Counted =
    evaluated and trained on >= C.WF_MIN_TRAIN_ROWS rows (owner, 9 Oct 2026);
    evaluated weeks below that are reported as `windows_not_counted`. The
    verdict of the saved model is `pooled_gates`."""
    evaluated = [w for w in windows if not w.get("skipped")]
    counted = [w for w in evaluated if wf_counted(w)]
    g = {"lift_ok": bool(test["top_lift"] >= C.GATES["min_top_lift"]),
         "collapse_ok": bool(test["collapse_removed"] >= C.GATES["min_collapse_removed"]),
         "walk_forward_ok": bool(counted) and all(w["sim_beats_all"] for w in counted),
         "windows_evaluated": len(counted),
         "windows_beating": sum(bool(w["sim_beats_all"]) for w in counted),
         "windows_not_counted": len(evaluated) - len(counted),
         "wf_min_train_rows": C.WF_MIN_TRAIN_ROWS}
    g["passed"] = g["lift_ok"] and g["collapse_ok"] and g["walk_forward_ok"]
    return g


def _finite(v) -> bool:
    """A real number that is not NaN / inf (None, strings and bools are not)."""
    return (isinstance(v, (int, float, np.integer, np.floating)) and not isinstance(v, bool)
            and bool(np.isfinite(v)))


def seed_lift_median(seed_lifts: dict, seeds) -> dict:
    """Per seed of `seeds` its pooled walk-forward lift under "lifts" (keyed by
    str(seed); None when it is missing or not finite, the reason under "why": the
    string given for it, else "undefined") and their "median": NaN unless every
    seed's lift is defined. The runner-lift gate (`pooled_gates`) and the
    across-seeds table of train.py both take the median this way."""
    lifts, why = {}, {}
    for s in seeds:
        v = seed_lifts.get(s)
        lifts[str(s)] = float(v) if _finite(v) else None
        if lifts[str(s)] is None:
            why[str(s)] = v if isinstance(v, str) else "undefined"
    complete = bool(lifts) and all(v is not None for v in lifts.values())
    return {"lifts": lifts, "why": why,
            "median": float(np.median(list(lifts.values()))) if complete else np.nan}


def pooled_gates(single: dict, test: dict, pooled: dict | None, seed_lifts: dict) -> dict:
    """The bucket verdict since the owner decision of 13 Oct 2026 (config "Bucket
    gates"; `passed` is what the scoring service reads). All four must hold:

    - pooled_lift_ok: median over C.STABILITY_SEEDS of the pooled expanding
      walk-forward top-10% lift >= C.GATES["min_top_lift"], every seed defined;
    - pooled_lift_lower_ok: lower end of the 95% week-block bootstrap interval
      of the saved seed's pooled lift > C.GATE_POOLED_LIFT_LOWER;
    - sim_diff_ok: lower end of the 95% bootstrap interval of the pooled
      simulation difference (top 10% minus all, mean capped net return)
      > C.GATE_SIM_DIFF_LOWER;
    - collapse_ok: collapses removed on the test part >= C.GATES["min_collapse_removed"].

    `single`: the old single-run gates (`gates`), kept under "old" (information).
    `test`: the saved model's test-part trading metrics. `pooled`: `D.pooled` of
    the saved seed's counted walk-forward weeks with the bootstrap (None: not
    computed). `seed_lifts`: per seed its pooled walk-forward lift, or a string
    saying why there is none. Fewer than C.GATE_MIN_POOLED_WEEKS counted weeks,
    a missing seed or any NaN fails the gate concerned, with a reason in
    `reasons`: never a PASS on missing data. The pooled top-10% mean net return
    (`abs_return_*`) is information only."""
    p = pooled if isinstance(pooled, dict) else {}
    reasons = []
    if pooled is None or p.get("error"):
        reasons.append("pooled walk-forward not computed"
                       + (f" ({p['error']})" if p.get("error") else ""))
    weeks = int(p.get("weeks") or 0)
    enough = weeks >= C.GATE_MIN_POOLED_WEEKS
    if not enough:
        reasons.append(f"only {weeks} counted walk-forward week(s); the pooled gates need at "
                       f"least {C.GATE_MIN_POOLED_WEEKS}")
    seeds = list(C.STABILITY_SEEDS)
    if not seeds:
        reasons.append("no STABILITY_SEEDS configured")
    sl = seed_lift_median(seed_lifts, seeds)
    lifts, median = sl["lifts"], sl["median"]
    for s in seeds:
        if lifts[str(s)] is None:
            reasons.append(f"pooled lift of seed {s}: {sl['why'][str(s)]}")
    complete = bool(seeds) and all(v is not None for v in lifts.values())
    min_lift = C.GATES["min_top_lift"]
    lift_ok = enough and complete and median >= min_lift
    if enough and complete and not lift_ok:
        reasons.append(f"median pooled lift {median:.2f} < {min_lift:g}")
    lo = p.get("lift_lo", np.nan)
    lower_ok = enough and _finite(lo) and lo > C.GATE_POOLED_LIFT_LOWER
    if enough and not _finite(lo):
        reasons.append("pooled lift 95% interval undefined")
    elif enough and not lower_ok:
        reasons.append(f"pooled lift 95% lower end {lo:.2f} <= {C.GATE_POOLED_LIFT_LOWER:g}")
    sim_lo = p.get("sim_gain_lo", np.nan)
    sim_ok = enough and _finite(sim_lo) and sim_lo > C.GATE_SIM_DIFF_LOWER
    if enough and not _finite(sim_lo):
        reasons.append("pooled simulation difference 95% interval undefined")
    elif enough and not sim_ok:
        reasons.append(f"pooled simulation difference 95% lower end {sim_lo:+.1f} <= "
                       f"{C.GATE_SIM_DIFF_LOWER:g}")
    cr = test.get("collapse_removed", np.nan)
    min_cr = C.GATES["min_collapse_removed"]
    collapse_ok = _finite(cr) and cr >= min_cr
    if not _finite(cr):
        reasons.append("collapses removed undefined (no collapse model or no collapse on test)")
    elif not collapse_ok:
        reasons.append(f"collapses removed {cr:.3f} < {min_cr:g}")
    top = p.get("sim_top_mean", np.nan)
    g = {"rule": "pooled walk-forward (owner decision)", "set_on": C.GATES_SET_ON,
         "pooled_weeks": weeks, "min_pooled_weeks": C.GATE_MIN_POOLED_WEEKS,
         "pooled_lift_seeds": lifts, "pooled_lift_median": median, "min_pooled_lift": min_lift,
         "pooled_lift_ok": bool(lift_ok),
         "pooled_lift": p.get("lift", np.nan), "pooled_lift_lo": lo,
         "pooled_lift_hi": p.get("lift_hi", np.nan),
         "min_pooled_lift_lower": C.GATE_POOLED_LIFT_LOWER, "pooled_lift_lower_ok": bool(lower_ok),
         "sim_diff": p.get("sim_gain", np.nan), "sim_diff_lo": sim_lo,
         "sim_diff_hi": p.get("sim_gain_hi", np.nan),
         "min_sim_diff_lower": C.GATE_SIM_DIFF_LOWER, "sim_diff_ok": bool(sim_ok),
         "collapse_removed": cr, "min_collapse_removed": min_cr, "collapse_ok": bool(collapse_ok),
         # information only, never a gate
         "abs_return_top": top, "abs_return_all": p.get("sim_all_mean", np.nan),
         "abs_return_positive": bool(top > 0) if _finite(top) else None,
         "old": {**single, "top_lift": test.get("top_lift", np.nan)},
         "reasons": reasons}
    g["passed"] = bool(lift_ok and lower_ok and sim_ok and collapse_ok)
    return g


def _filter_rule(value, minimum: float, enough: bool, name: str, reasons: list) -> bool:
    """One rule of `filter_verdict`: `value` (a 95% lower end) > `minimum`;
    False with a reason when it does not hold or is undefined (only judged
    with `enough` counted weeks)."""
    ok = enough and _finite(value) and value > minimum
    if enough and not _finite(value):
        reasons.append(f"pooled {name} 95% interval undefined")
    elif enough and not ok:
        reasons.append(f"pooled {name} 95% lower end {value:.3f} <= {minimum:g}")
    return bool(ok)


def filter_verdict(pooled_filter: dict | None, applies: bool) -> dict:
    """Whether the collapse filter is validated in a bucket (owner decision of
    18 Oct 2026, config "Collapse filter verdict"): it beats random skipping on
    the pooled walk-forward. `pooled_filter`: the bucket's
    metrics[b]["collapse_filter"]["pooled"] (train.filter_pooled: the rates with
    their `<rate>_lo` / `<rate>_hi` and `weeks`, or {"error": ...}). `applies`:
    the bucket is one of C.FILTER_BUCKETS. Validated only when it applies, there
    are at least C.GATE_MIN_POOLED_WEEKS counted weeks, and both lower ends are
    defined and above C.FILTER_RULES; else not, with the reasons. Never a gate."""
    p = pooled_filter if isinstance(pooled_filter, dict) else {}
    rules, reasons = C.FILTER_RULES, []
    if not applies:
        reasons.append("not a filter bucket (FILTER_BUCKETS: "
                       + ", ".join(C.FILTER_BUCKETS) + ")")
    if pooled_filter is None or p.get("error"):
        reasons.append("pooled walk-forward not computed"
                       + (f" ({p['error']})" if p.get("error") else ""))
    weeks = int(p.get("weeks") or 0)
    enough = weeks >= C.GATE_MIN_POOLED_WEEKS
    if not enough:
        reasons.append(f"only {weeks} counted walk-forward week(s); the verdict needs at "
                       f"least {C.GATE_MIN_POOLED_WEEKS}")
    caught_lo = p.get("collapses_caught_lo", np.nan)
    kept_lo = p.get("kept_minus_all_lo", np.nan)
    caught_ok = _filter_rule(caught_lo, rules["min_caught_lower"], enough, "collapses caught",
                             reasons)
    kept_ok = _filter_rule(kept_lo, rules["min_kept_minus_all_lower"], enough,
                           "kept minus all (mean net %)", reasons)
    return {"applies": bool(applies), "validated": bool(applies and caught_ok and kept_ok),
            "weeks": weeks, "min_weeks": C.GATE_MIN_POOLED_WEEKS,
            "caught_lo": caught_lo, "min_caught_lower": rules["min_caught_lower"],
            "caught_ok": caught_ok, "kept_minus_all_lo": kept_lo,
            "min_kept_minus_all_lower": rules["min_kept_minus_all_lower"], "kept_ok": kept_ok,
            "reasons": reasons, "set_on": C.FILTER_RULES_SET_ON,
            "seen_report": C.FILTER_RULES_SEEN_REPORT}


def single_run_gates(g: dict) -> dict:
    """The old single-run gates of a bucket result: `g["old"]` for the saved
    model (whose `gates` are `pooled_gates`), `g` itself for a variant or seed run."""
    return g.get("old", g)


def window_ok(n_test: int) -> bool:
    return n_test >= C.MIN_WINDOW_TEST_ROWS
