"""Train runner/collapse models for every bucket and write models/<version>/.

    python train.py --csv calls.csv --out models/
    python train.py --dsn postgres://user:pass@host/db --out models/
    python train.py --csv calls.csv --out models/ --variants   # plus the variant comparison
    python train.py --csv calls.csv --out models/ --variants --live-buys live_buys.csv
                                        # plus the wallet reputation variants (report only)
    python train.py --csv calls.csv --out models/ --candidate
                                        # writes CANDIDATE, never LATEST (not served)
"""
import argparse
import json
import math
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import joblib
import numpy as np
import pandas as pd

from scout_ml import config as C
from scout_ml import diagnostics as D
from scout_ml.features import build_features, category, dex_family_rules, learn_cat_levels, num
from scout_ml.labels import build_labels, dead_policy
from scout_ml.model import (apply_calibrator, best_rounds, choose_platt, fit_baseline, fit_lgbm,
                            fit_platt, fit_platt_recent, predict, usable_holdout, write_pointer)
from scout_ml.report import render, variant_error
from scout_ml import validate as V
from scout_ml import wallet_rep as W


def read_dsn(dsn: str) -> pd.DataFrame:
    try:
        import psycopg
    except ImportError as e:
        raise SystemExit("--dsn needs psycopg: pip install 'psycopg[binary]'") from e
    with psycopg.connect(dsn) as conn:
        cur = conn.execute("SELECT * FROM scout_call_dataset_v")
        return pd.DataFrame(cur.fetchall(), columns=[d.name for d in cur.description])


def _json_safe(o):
    """NaN -> null, numpy scalars / timestamps -> plain JSON types."""
    if isinstance(o, dict):
        return {str(k): _json_safe(v) for k, v in o.items()}
    if isinstance(o, (list, tuple)):
        return [_json_safe(v) for v in o]
    if isinstance(o, (np.bool_, bool)):
        return bool(o)
    if isinstance(o, np.integer):
        return int(o)
    if isinstance(o, (float, np.floating)):
        return None if math.isnan(o) else float(o)
    if isinstance(o, (pd.Timestamp, datetime)):
        return o.isoformat()
    return o


def _class_check(y, mask, part_desc):
    """Class balance info for one label; info["skipped"] is set when the
    training part has too few rows of a class."""
    pos, neg = int(y[mask].sum()), int((1 - y[mask]).sum())
    info = {"positive_rate": float(np.nanmean(y)), "train_pos": pos, "train_neg": neg,
            "messages": []}
    if not 0.10 <= info["positive_rate"] <= 0.90:
        info["messages"].append(f"WARNING: positive rate {info['positive_rate']:.1%} is outside 10-90%")
    if min(pos, neg) < C.MIN_CLASS_ROWS:
        info["skipped"] = (f"model skipped: training part ({part_desc}) has {pos} positives / "
                           f"{neg} negatives (need {C.MIN_CLASS_ROWS} of each)")
        info["messages"].append(info["skipped"])
    return info


def _calibrate_chosen(fitted, info, raw, y, dates):
    """Replace the calibrator by the one `choose_platt` picks ("recent" window or
    "all" held-out rows, compared on the held-out rows alone; `raw`/`y`/`dates`:
    validation or out-of-fold rows only, never test rows). Both candidates fitted
    on all held-out rows are kept for the report (test Brier: information only).
    When the recent window cannot be fitted, there is no recent candidate
    (`calibrator_recent` None; the report says "n/a"), never the silent
    all-rows fallback of `fit_platt_recent` under the recent-window name."""
    fitted["calibrator"], info["calib"] = choose_platt(raw, y, dates)
    if info["calib"]["used"] == "recent":     # the chosen one is that candidate: no refit
        fitted["calibrator_recent"] = fitted["calibrator"]
        fitted["calibrator_all"] = fit_platt(raw, y)
    else:
        fitted["calibrator_all"] = fitted["calibrator"]
        fitted["calibrator_recent"] = (fit_platt_recent(raw, y, dates)[0]
                                       if info["calib"]["recent_fit"] == "recent" else None)
    if info["calib"]["note"]:
        info["messages"].append("calibration on all held-out rows: " + info["calib"]["note"])


def _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label):
    """Test metrics, importance and the saved model for a fitted label
    (`out_dir` None: a --variants run, nothing is saved)."""
    if fitted["note"]:
        info["messages"].append(fitted["note"])
    p_test = predict(fitted, X[te])
    if fitted.get("calibrator_all") is not None:
        # both candidates on test: shown in the report, information only (the
        # choice was made on held-out rows before; test rows never choose)
        raw_te = fitted["model"].predict(X[te], raw_score=True)
        for key, cal in (("all_rows", fitted["calibrator_all"]),
                         ("recent", fitted["calibrator_recent"])):
            if cal is None:                   # recent window cannot be fitted: no candidate
                info[f"calibration_{key}"], info[f"brier_{key}"] = None, None
                continue
            p_c = apply_calibrator(cal, raw_te)
            info[f"calibration_{key}"] = V.calibration_table(y[te], p_c)
            info[f"brier_{key}"] = V.model_metrics(y[te], p_c)["brier"]
    gain = fitted["model"].booster_.feature_importance("gain")
    order = np.argsort(-gain)[:10]
    fitted["baseline"] = fit_baseline(X[tr], y[tr])  # kept in memory only (never saved)
    fitted["p_logistic"] = fitted["baseline"].predict_proba(X[te][C.NUMERIC_FEATURES])[:, 1]
    info.update(
        lightgbm=V.model_metrics(y[te], p_test),
        logistic=V.model_metrics(y[te], fitted["p_logistic"]),
        calibration=V.calibration_table(y[te], p_test),
        rounds=best_rounds(fitted),
        importance=[[X.columns[i], float(gain[i] / max(gain.sum(), 1e-12))] for i in order])
    if out_dir is not None:
        joblib.dump({"model": fitted["model"], "calibrator": fitted["calibrator"]},
                    out_dir / f"{bucket}_{label}.joblib")
    return info, fitted, p_test


def _train_label(X, y, split, label, out_dir, bucket, dates):
    """short/3day: early stopping on the validation part, Platt calibration on
    its most recent rows (C.CALIB_RECENT_DAYS) or on all of it, whichever
    `choose_platt` picks on the validation rows alone."""
    tr, va, te = split["train"], split["val"], split["test"]
    info = _class_check(y, tr, "earliest 70% minus the embargo")
    if info.get("skipped"):
        return info, None, None
    fitted = fit_lgbm(X[tr], y[tr], X[va], y[va])
    if fitted["calibrator"] is not None:
        _calibrate_chosen(fitted, info, fitted["model"].predict(X[va], raw_score=True), y[va],
                          dates[va])
    return _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label)


def _kfold_matrices(rows, dates, tokens, tr, horizon_days):
    """Purged k-fold inside the train part: per fold the row masks (over all
    rows of the bucket) and a feature matrix whose category levels come from
    that fold's fit rows only. Test rows are never in a fold."""
    idx = np.flatnonzero(tr)
    folds = []
    for f in V.purged_kfold(dates[tr], tokens[tr], horizon_days, C.KFOLD_K):
        fit, held = np.zeros(len(tr), bool), np.zeros(len(tr), bool)
        fit[idx[f["fit"]]], held[idx[f["held"]]] = True, True
        X = build_features(rows, learn_cat_levels(rows[fit]))
        folds.append({**f, "fit": fit, "held": held, "X": X})
    return folds


def _train_label_kfold(X, y, split, folds, label, out_dir, bucket, dates):
    """medium/long: rounds = median best iteration over the usable purged
    folds; Platt calibration fitted on the most recent out-of-fold margins
    (C.CALIB_RECENT_DAYS) or on all of them, whichever `choose_platt` picks on
    the out-of-fold rows alone, of fold models refitted with those rounds;
    final model on the whole train part."""
    tr, te = split["train"], split["test"]
    info = _class_check(y, tr, "calls before the test date minus the embargo")
    if info.get("skipped"):
        return info, None, None
    usable = [f for f in folds if min(y[f["fit"]].sum(), (1 - y[f["fit"]]).sum()) >= C.MIN_CLASS_ROWS
              and usable_holdout(y[f["held"]])]
    cv = {"folds": len(folds), "folds_used": len(usable), "fold_rounds": [], "oof_rows": 0}
    info["cv"] = cv
    if len(usable) < C.KFOLD_MIN_FOLDS:
        fitted = fit_lgbm(X[tr], y[tr], n_estimators=C.FALLBACK_ROUNDS)
        fitted["note"] = (f"only {len(usable)} of {len(folds)} purged folds usable (need "
                          f"{C.KFOLD_MIN_FOLDS}): fixed {C.FALLBACK_ROUNDS} rounds, "
                          "probabilities uncalibrated")
        return _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label)
    for f in usable:
        Xf = f["X"]
        cv["fold_rounds"].append(best_rounds(fit_lgbm(Xf[f["fit"]], y[f["fit"]],
                                                       Xf[f["held"]], y[f["held"]])))
    rounds = int(np.median(cv["fold_rounds"]))
    raw, obs, when = [], [], []
    for f in usable:
        Xf = f["X"]
        m = fit_lgbm(Xf[f["fit"]], y[f["fit"]], n_estimators=rounds)["model"]
        raw.append(m.predict(Xf[f["held"]], raw_score=True))
        obs.append(y[f["held"]])
        when.append(dates[f["held"]])
    raw, obs, when = np.concatenate(raw), np.concatenate(obs), pd.concat(when)
    cv["oof_rows"] = int(len(obs))
    fitted = fit_lgbm(X[tr], y[tr], n_estimators=rounds)
    _calibrate_chosen(fitted, info, raw, obs, when)
    return _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label)


def collapse_check(y_runner, runner_score, y_plain, y_with_dead, collapse_score, net_ret,
                   with_dead: bool) -> dict:
    """Trading metrics for one test part. "collapses removed" (the gate) is
    always measured on the PLAIN collapse label `y_plain`, whatever label the
    collapse model was trained on. With `with_dead` (the data has trades_24h),
    the share of collapse OR dead (`y_with_dead`) removed by the same scores is
    added as `collapse_removed_with_dead`: information only, never a gate."""
    out = V.trading_metrics(y_runner, runner_score, y_plain, collapse_score, net_ret)
    if with_dead:
        out["collapse_removed_with_dead"] = V.collapse_removed(y_with_dead, collapse_score)
    return out


def _walk_forward(rows, Y, y_plain, y_with_dead, net_ret, dates, tokens, horizon_days, rounds,
                  with_dead, train_weeks=None, collect=None):
    """Refit per week with the main model's round count; ranking metrics only.
    Category levels are learned from each window's training rows. Collapses
    removed are measured on the plain label `y_plain` (see collapse_check).
    `train_weeks`: rolling windows (see V.walk_forward_windows; report-only
    diagnostics), None = expanding (the gate). `collect` (a list, diagnostics
    only): each evaluated week's test rows (positions in `rows`) and scores are
    appended to it; the returned rows are the same with or without it."""
    out = []
    for w in V.walk_forward_windows(dates, tokens, horizon_days, train_weeks=train_weeks):
        tr, te = w["train"], w["test"]
        row = {"week": w["week"], "start": w["start"].strftime("%Y-%m-%d"),
               "n_train": int(tr.sum()), "n_test": int(te.sum())}
        row["enough_train"] = V.wf_enough_train(row)   # below C.WF_MIN_TRAIN_ROWS: shown, not counted
        scores, X = {}, None
        for label, n_rounds in rounds.items():
            y = Y[label]
            if min(y[tr].sum(), (1 - y[tr]).sum()) >= C.MIN_CLASS_ROWS and V.window_ok(te.sum()):
                if X is None:
                    X = build_features(rows, learn_cat_levels(rows[tr]))
                scores[label] = predict(fit_lgbm(X[tr], y[tr], n_estimators=n_rounds), X[te])
        if "runner" not in scores:
            row["skipped"] = "too few training rows per class or test rows"
        else:
            row.update(collapse_check(Y["runner"][te], scores["runner"], y_plain[te],
                                      y_with_dead[te], scores.get("collapse"), net_ret[te],
                                      with_dead))
            row["runner_auc"] = V.model_metrics(Y["runner"][te], scores["runner"])["roc_auc"]
        row["counted"] = V.wf_counted(row)
        out.append(row)
        if collect is not None and "runner" in scores:
            collect.append({"week": row["week"], "start": row["start"], "n_train": row["n_train"],
                            "counted": row["counted"], "train": tr, "idx": np.flatnonzero(te),
                            "s_runner": scores["runner"], "s_collapse": scores.get("collapse")})
    return out


def maturity_skip(b, dates):
    """Reason to skip bucket `b` while too few calls have a matured outcome
    (C.MIN_MATURED), else None."""
    need = C.MIN_MATURED.get(b)
    if not need:
        return None
    n = len(dates)
    span = (dates.max() - dates.min()).total_seconds() / 86400 if n else 0.0
    if n >= need["rows"] and span >= need["span_days"]:
        return None
    h = C.BUCKETS[b]["horizon_days"]
    return (f"not enough matured {h:g}d data ({n} rows over {span:.0f} days, need "
            f"{need['rows']} rows over at least {need['span_days']} days)")


def _dead_stats(L, b, use, te):
    """Dead calls among the bucket's usable rows and its test part, and how
    many of them are collapses by the plain label anyway."""
    dead = L["dead"][use].to_numpy()
    plain = L[f"collapse_plain_{b}"][use].to_numpy() == 1
    out = {}
    for part, m in (("usable", np.ones(len(dead), bool)), ("test", te)):
        out[part] = {"rows": int(m.sum()), "dead": int((dead & m).sum()),
                     "dead_collapse": int((dead & plain & m).sum()),
                     "collapse_rate_plain": float(plain[m].mean()) if m.any() else np.nan,
                     "collapse_rate_with_dead": float((plain | dead)[m].mean()) if m.any() else np.nan}
    return out


def cutoff_rows(split: dict, dates: pd.Series, horizon_days: float) -> np.ndarray:
    """The rows the collapse filter's fixed cut-off is taken on: the bucket's
    usable rows posted after the saved model's training rows end and before
    its test part starts. 70/15/15 split (`time_split`): [t1 - horizon, t2),
    i.e. the validation part with both embargo gaps; forward split
    (`forward_split`): [t_train_end, t_test), the embargo gap. Never a test
    row (a test row's token has its first call at or after the test start);
    the test mask is removed anyway."""
    d = pd.to_datetime(dates, utc=True)
    if "t_test" in split:
        lo, hi = split["t_train_end"], split["t_test"]
    else:
        lo, hi = split["t1"] - pd.Timedelta(days=horizon_days), split["t2"]
    return ((d >= lo) & (d < hi)).to_numpy() & ~np.asarray(split["test"], dtype=bool)


def collapse_reference_block(fitted, X: pd.DataFrame, mask, dates: pd.Series) -> dict:
    """The collapse filter's fixed cut-off (meta.json collapse_reference.<bucket>):
    the saved collapse model's probabilities (`predict`, as the scoring
    service's collapse_prob, unrounded) on the `cutoff_rows` rows `mask` of the
    bucket's feature matrix `X` (inputs only, never outcomes), their
    (1 - C.SKIP_FRAC) quantile (`V.collapse_cutoff`) and C.N_REF_QUANTILES
    quantiles. {"skipped": reason} without a collapse model or with fewer than
    C.FILTER_CUTOFF_MIN_ROWS rows."""
    mask = np.asarray(mask, dtype=bool)
    if fitted is None:
        return {"skipped": "no collapse model"}
    n = int(mask.sum())
    if n < C.FILTER_CUTOFF_MIN_ROWS:
        return {"skipped": f"only {n} pre-test held-out rows (need {C.FILTER_CUTOFF_MIN_ROWS})"}
    scores = predict(fitted, X[mask])
    d = pd.to_datetime(dates, utc=True)[mask]
    return {"cutoff": V.collapse_cutoff(scores), "quantile": 1 - C.SKIP_FRAC, "n": n,
            "from": d.min().strftime("%Y-%m-%d %H:%M"), "to": d.max().strftime("%Y-%m-%d %H:%M"),
            "rows": "pre-test held-out", "calibrated": fitted.get("calibrator") is not None,
            "quantiles": np.quantile(scores, np.linspace(0, 1, C.N_REF_QUANTILES)).tolist()}


def _collapse_reference(fitted, X, split, dates, h) -> dict:
    """`collapse_reference_block` on the `cutoff_rows`; an error is recorded as
    {"error": ...} with a WARNING and never stops the saved model."""
    try:
        return collapse_reference_block(fitted, X, cutoff_rows(split, dates, h), dates)
    except Exception as exc:  # not BaseException: Ctrl-C still stops the run
        out = {"error": variant_error(exc)}
        print(f"WARNING: the collapse cut-off failed ({out['error']}); no cut-off, the saved "
              "models are not affected", flush=True)
        return out


def _train_bucket(b, df, L, out_dir, with_dead=False, keep=None):
    """Returns (results, runner reference quantiles or None, category levels or None).
    `out_dir` None: nothing is saved (a --variants run). `with_dead` (the data
    has trades_24h): the "Dead after the call" table and the information
    figures on collapse OR dead, whether or not the dead rule is on.
    `keep` (a dict; every caller passes it, since the pooled gates need the
    walk-forward arrays): filled with the bucket's rows, labels, masks, feature
    matrix, test scores and walk-forward scores, the input of gate_inputs and of
    the stability diagnostics, and "collapse_reference" (`_collapse_reference`,
    the collapse filter's fixed cut-off). The results are the same with or
    without it; the arrays never reach meta.json (only the gate values
    apply_gates computes from them do)."""
    cfg = C.BUCKETS[b]
    use = L[f"usable_{b}"].to_numpy()
    res = {"usable": int(use.sum())}
    if res["usable"] < C.MIN_BUCKET_ROWS:
        res["skipped"] = f"only {res['usable']} usable rows (need {C.MIN_BUCKET_ROWS})"
        return res, None, None
    rows = df[use].reset_index(drop=True)
    dates = rows["message_date"]
    tokens = rows["contract_address"]
    reason = maturity_skip(b, dates)
    if reason:
        res["skipped"] = reason
        return res, None, None
    net_ret = L[f"net_ret_{b}"][use].to_numpy()
    Y = {lab: L[f"{lab}_{b}"][use].to_numpy() for lab in C.LABELS}
    h = cfg["horizon_days"]
    forward = b in C.FORWARD_SPLIT_BUCKETS
    split = V.forward_split(dates, tokens, h) if forward else V.time_split(dates, tokens, h)
    tr, te = split["train"], split["test"]
    res["split"] = {"kind": "forward" if forward else "70/15/15", "n_train": int(tr.sum()),
                    "n_test": int(te.sum()), "n_dropped_embargo": split["n_dropped_embargo"],
                    "n_dropped_token": split["n_dropped_token"]}
    if forward:
        res["split"].update(t_train_end=split["t_train_end"].strftime("%Y-%m-%d %H:%M"),
                            t_test=split["t_test"].strftime("%Y-%m-%d %H:%M"),
                            test_days=C.FORWARD_TEST_DAYS, k=C.KFOLD_K)
    else:
        res["split"].update(n_val=int(split["val"].sum()),
                            t1=split["t1"].strftime("%Y-%m-%d %H:%M"),
                            t2=split["t2"].strftime("%Y-%m-%d %H:%M"))
    if te.sum() < C.MIN_CLASS_ROWS:
        res["skipped"] = f"only {int(te.sum())} test rows after the time split"
        return res, None, None
    # Category levels: from the train part only (never validation or test rows).
    levels = learn_cat_levels(rows[tr])
    X = build_features(rows, levels)
    folds = _kfold_matrices(rows, dates, tokens, tr, h) if forward else None
    res["labels"], scores, rounds, logistic = {}, {}, {}, {}
    for lab in C.LABELS:
        if forward:
            out = _train_label_kfold(X, Y[lab], split, folds, lab, out_dir, b, dates)
        else:
            out = _train_label(X, Y[lab], split, lab, out_dir, b, dates)
        res["labels"][lab], fitted, scores[lab] = out
        if lab == "collapse" and keep is not None:   # the filter's fixed cut-off (no outcomes)
            keep["collapse_reference"] = _collapse_reference(fitted, X, split, dates, h)
        if fitted is not None:
            rounds[lab] = res["labels"][lab]["rounds"]
            logistic[lab] = fitted["p_logistic"]
    if "runner" not in rounds:
        res["skipped"] = res["labels"]["runner"]["skipped"]
        return res, None, None
    # gate on the plain collapse label; collapse OR dead is information only
    y_plain = L[f"collapse_plain_{b}"][use].to_numpy()
    y_with_dead = np.maximum(y_plain, L["dead"][use].to_numpy().astype(float))  # NULL: plain
    res["trading"] = collapse_check(Y["runner"][te], scores["runner"], y_plain[te],
                                    y_with_dead[te], scores["collapse"], net_ret[te], with_dead)
    if with_dead:
        res["dead"] = _dead_stats(L, b, use, te)
    # collapse ROC AUC on the PLAIN label, comparable whatever label the model was trained on
    if "collapse" in rounds:
        res["collapse_auc_plain"] = {
            "lightgbm": V.model_metrics(y_plain[te], scores["collapse"])["roc_auc"],
            "logistic": V.model_metrics(y_plain[te], logistic["collapse"])["roc_auc"]}
    wf_rows = None if keep is None else []
    res["walk_forward"] = _walk_forward(rows, Y, y_plain, y_with_dead, net_ret, dates, tokens, h,
                                        rounds, with_dead, collect=wf_rows)
    res["gates"] = V.gates(res["trading"], res["walk_forward"])
    if keep is not None:
        keep.update(rows=rows, dates=dates, tokens=tokens, h=h, Y=Y, y_plain=y_plain,
                    y_with_dead=y_with_dead, net_ret=net_ret, rounds=dict(rounds),
                    with_dead=with_dead, tr=tr, te=te, X=X, scores=dict(scores),
                    dead=L["dead"][use].to_numpy().astype(bool), wf=wf_rows)
    reference = np.quantile(scores["runner"], np.linspace(0, 1, C.N_REF_QUANTILES))
    return res, reference.tolist(), levels


def variant_summary(r: dict, pooled: dict | None = None) -> dict:
    """The comparison numbers of one bucket of one variant (see report.variant_table).
    The gate columns are the old single-run gates (information; computing the
    pooled gates of a variant would need its seed retrains too). `pooled`: the
    variant's own pooled walk-forward (its LGBM_SEED run only, with the bootstrap;
    information, not its verdict)."""
    if r.get("skipped"):
        return {"skipped": r["skipped"]}
    t, g, labs = r["trading"], V.single_run_gates(r["gates"]), r["labels"]
    p = pooled if isinstance(pooled, dict) else {}

    def auc(lab, model):
        m = labs.get(lab) or {}
        return np.nan if m.get("skipped") else m[model]["roc_auc"]

    weeks = [w for w in r["walk_forward"] if V.wf_counted(w)]   # as the gate: counted weeks only
    plain = r.get("collapse_auc_plain") or {}
    return {
        "runner_rate_test": t["all_pos"] / t["all_n"] if t["all_n"] else np.nan,
        **{k: t[k] for k in ("top_lift", "top_lift_lo", "top_lift_hi", "top_pos", "top_n",
                             "all_pos", "all_n", "collapse_removed", "sim_top_mean",
                             "sim_all_mean")},
        "runner_auc_lightgbm": auc("runner", "lightgbm"),
        "runner_auc_logistic": auc("runner", "logistic"),
        "collapse_auc_plain_lightgbm": plain.get("lightgbm", np.nan),
        "collapse_auc_plain_logistic": plain.get("logistic", np.nan),
        "collapse_auc_trained_lightgbm": auc("collapse", "lightgbm"),
        "collapse_auc_trained_logistic": auc("collapse", "logistic"),
        "wf_mean_lift": float(np.nanmean([w["top_lift"] for w in weeks]))
        if weeks and not all(np.isnan(w["top_lift"]) for w in weeks) else np.nan,
        "wf_mean_collapse_removed": float(np.nanmean([w["collapse_removed"] for w in weeks]))
        if weeks and not all(np.isnan(w["collapse_removed"]) for w in weeks) else np.nan,
        "windows_beating": g["windows_beating"], "windows_evaluated": g["windows_evaluated"],
        "passed": g["passed"],
        **{f"wf_pooled_{k}": p.get(k, np.nan)
           for k in ("lift", "lift_lo", "lift_hi", "sim_gain", "sim_gain_lo", "sim_gain_hi")}}


def _variant_bucket(b, df, L, has_trades, store: dict | None = None) -> dict:
    """One bucket of one variant: its summary with its own pooled walk-forward.
    `store` (a dict; the C.SEED_VARIANTS): gets {b: {"r", "keep", "pooled"}}, the
    input of `variant_across_seeds`; the summary is the same with or without it."""
    keep = {}
    r = _train_bucket(b, df, L, None, has_trades, keep=keep)[0]
    pooled = D.pooled(_wf_weeks(keep)) if keep.get("wf") is not None else None
    if store is not None:
        store[b] = {"r": r, "keep": keep, "pooled": pooled}
    return variant_summary(r, pooled)


# --- "Variants across seeds" (report only; config SEED_VARIANTS) ---

def seed_median(seed_lifts: dict) -> dict:
    """Per seed of C.STABILITY_SEEDS its pooled walk-forward lift (None when there
    is none; the reason under "why") and their median taken as the runner-lift
    gate takes it: NaN unless every seed's lift is defined (V.seed_lift_median,
    shared with V.pooled_gates)."""
    return V.seed_lift_median(seed_lifts, C.STABILITY_SEEDS)


def _top_inputs(adv: dict) -> dict:
    """`adv` with its top drifting inputs cut to as many as the report shows (3)
    or the wallet drift rule reads (C.WREP_ADOPTION["drift_top"]), the larger."""
    n = max(3, C.WREP_ADOPTION["drift_top"])
    return {**adv, "top": list(adv.get("top") or [])[:n]}


def adversarial_split(keep: dict) -> dict:
    """D.adversarial on a trained bucket's own feature matrix: its train part vs
    its test part (as the diagnostics' "split" row; top drifting inputs: `_top_inputs`)."""
    m = keep["tr"] | keep["te"]
    return _top_inputs(D.adversarial(keep["X"][m], keep["te"][m]))


def across_row(seed_lifts: dict, pooled, adversarial) -> dict:
    """One bucket of one row of the across-seeds table: `seed_median`, the
    LGBM_SEED run's pooled walk-forward lift and simulation difference with
    their 95% intervals (`pooled`, from D.pooled), and `adversarial`
    (`adversarial_split`, or {"error": ...})."""
    p = pooled if isinstance(pooled, dict) else {}
    return {**seed_median(seed_lifts),
            **{k: p.get(k, np.nan) for k in ("weeks", "lift", "lift_lo", "lift_hi", "sim_gain",
                                             "sim_gain_lo", "sim_gain_hi")},
            "adversarial": adversarial}


def _adv_or_error(keep: dict) -> dict:
    try:
        return adversarial_split(keep)
    except Exception as exc:  # one probe failing never loses the row
        return {"error": variant_error(exc)}


def across_seeds(buckets: dict, runs: dict, pooled: dict | None, keeps: dict,
                 adv_known: dict | None = None) -> dict:
    """One row of the across-seeds table, nothing retrained: per bucket `across_row`
    from the seed retrains `runs` and pooled walk-forward `pooled`; the
    adversarial AUC from `adv_known` (bucket -> the diagnostics' "split" result,
    the same computation) when given, else computed from `keeps`."""
    t0 = time.time()
    out = {"seeds": list(C.STABILITY_SEEDS), "buckets": {}}
    for b, r in buckets.items():
        if r.get("skipped") or b not in keeps:
            out["buckets"][b] = {"skipped": r.get("skipped") or "not trained"}
            continue
        known = (adv_known or {}).get(b)
        adv = _top_inputs(known) if isinstance(known, dict) else _adv_or_error(keeps[b])
        out["buckets"][b] = across_row(_seed_lifts(runs, b), (pooled or {}).get(b), adv)
    out["seconds"] = round(time.time() - t0, 1)
    return out


def variant_across_seeds(df, L, has_trades: bool, store: dict) -> dict:
    """For a C.SEED_VARIANTS entry, inside its C.overrides: the variant retrained
    with the other STABILITY_SEEDS (`seed_runs`; its LGBM_SEED run in `store`,
    from `_variant_bucket`, is reused), then `across_seeds`. Report only."""
    t0 = time.time()
    results = {b: v["r"] for b, v in store.items()}
    keeps = {b: v["keep"] for b, v in store.items() if v["keep"].get("wf") is not None}
    runs = seed_runs(df, L, results, keeps, has_trades,
                     on_fail="the variant's median over the seeds is n/a (report only)")
    pooled = {b: v["pooled"] for b, v in store.items()}
    return {**across_seeds(results, runs, pooled, keeps), "seconds": round(time.time() - t0, 1)}


def run_variants(df: pd.DataFrame, baseline: dict, has_trades: bool,
                 baseline_pooled: dict | None = None, baseline_runs: dict | None = None,
                 baseline_keeps: dict | None = None, baseline_adv: dict | None = None,
                 wrep_frames: dict | None = None) -> dict:
    """Train every C.VARIANTS entry (one knob changed from the configured
    values) on the same rows and splits; report only, nothing is saved.
    `baseline`: the bucket results of the saved model, reused for "baseline"
    (`baseline_pooled`: its pooled walk-forward per bucket, from the gate inputs).
    `df`: sorted rows as prepared by `train`. `wrep_frames` ({WREP_LOOKBACK_DAYS:
    wrep_* frame}, `wallet_rep_frames`; None without live buys): each
    C.WALLET_REP_VARIANTS entry trains on `df` joined with the frame of its own
    lookback (`variant_data`), also in its seed retrains and adversarial
    validation. `has_trades`: the data has the
    trades_24h column (else the C.TRADES_VARIANTS equal the baseline: n/a).
    The C.WALLET_REP_VARIANTS are n/a when their rows have no wrep_* column or only
    empty ones (`wrep_not_applicable`; they would otherwise train on all-NaN
    inputs); when they ran, their pre-registered
    adoption rules (`wallet_adoption`) are added under "adoption".
    The C.SEED_VARIANTS also get "across_seeds" (`variant_across_seeds`), and
    then the baseline row too (`across_seeds` from `baseline_runs`, the
    gates' seed retrains, `baseline_keeps` and `baseline_adv`; left out when
    `baseline_runs` is None).
    A variant that raises is recorded as {"error": "error: <short message>"} and the
    others still run: the saved baseline must never be lost to a variant. An
    error in an across-seeds part only replaces that part by {"error": ...}."""
    out = {}
    attempted = failed = 0
    seed_variants = [n for n in C.SEED_VARIANTS if n in C.VARIANTS]
    for name, knobs in C.VARIANTS.items():
        entry = {"changes": dict(knobs)}
        out[name] = entry
        if not knobs:
            entry["buckets"] = {b: variant_summary(r, (baseline_pooled or {}).get(b))
                                for b, r in baseline.items()}
            if seed_variants and baseline_runs is not None:
                try:
                    entry["across_seeds"] = across_seeds(
                        baseline, baseline_runs, baseline_pooled, baseline_keeps or {},
                        baseline_adv)
                except Exception as exc:  # report only: never fails the run
                    entry["across_seeds"] = {"error": variant_error(exc)}
            continue
        if not has_trades and name in C.TRADES_VARIANTS:
            entry["not_applicable"] = (f"the data has no {C.DEAD_COLUMN} column, so this "
                                       "variant equals the baseline")
            continue
        df_v = variant_data(df, name, wrep_frames)
        if name in C.WALLET_REP_VARIANTS and wrep_not_applicable(df_v):
            entry["not_applicable"] = wrep_not_applicable(df_v)
            continue
        t0 = time.time()
        attempted += 1
        store = {} if name in seed_variants else None
        try:  # around overrides too; its `finally` restores the config after an error
            with C.overrides(**knobs):
                policy = dead_policy(df_v)
                L = build_labels(df_v, policy)
                entry["buckets"] = {b: _variant_bucket(b, df_v, L, has_trades, store)
                                    for b in C.BUCKETS}
                if store is not None:
                    try:
                        entry["across_seeds"] = variant_across_seeds(df_v, L, has_trades, store)
                    except Exception as exc:  # the variant's own row is kept
                        entry["across_seeds"] = {"error": variant_error(exc)}
                        print(f"WARNING: the across-seeds part of variant {name!r} failed "
                              f"({entry['across_seeds']['error']}); its other numbers are "
                              "kept", flush=True)
        except Exception as exc:  # not BaseException: Ctrl-C still stops the run
            failed += 1
            entry.pop("buckets", None)
            entry.pop("across_seeds", None)
            entry["error"] = variant_error(exc)
            print(f"WARNING: variant {name!r} failed ({entry['error']}); the saved baseline "
                  "model is not affected, the other variants go on", flush=True)
        entry["seconds"] = round(time.time() - t0, 1)
        extra = (entry.get("across_seeds") or {}).get("seconds")
        print(f"[variant] {name}: {entry['seconds']:.0f} s"
              + (f" (of which other seeds and adversarial validation {extra:.0f} s)"
                 if extra is not None else ""), flush=True)
    if attempted and failed == attempted:
        print(f"WARNING: all {attempted} variants failed; report.md has no variant numbers "
              "(the saved baseline model and its gates are complete and valid)", flush=True)
    add_wallet_adoption(out)
    return out


# --- Wallet reputation (report-only variant; config WALLET_REP, WREP_*) ---------

WREP_NOT_APPLICABLE = ("no live-buy wallet data (train.py --live-buys PATH not given, or its "
                       "wallet reputation failed: see \"Wallet reputation data\")")


WREP_NO_LIVE_BUYS = ("no live buys in the export (the --live-buys CSV has no rows for these "
                     "calls, so every wrep_* input is empty)")


def has_wrep_columns(df: pd.DataFrame) -> bool:
    """Whether `df` has any wrep_* column (the wallet reputation joined)."""
    return any(str(c).startswith(C.WREP_PREFIX) for c in df.columns)


def wrep_not_applicable(df: pd.DataFrame) -> str | None:
    """Why the wallet reputation variants (C.WALLET_REP_VARIANTS) cannot run on
    `df`, or None when they can:
    no wrep_* column (WREP_NOT_APPLICABLE), or every wrep_* value missing, e.g. a
    header-only or zero-row live_buys.csv (WREP_NO_LIVE_BUYS)."""
    if not has_wrep_columns(df):
        return WREP_NOT_APPLICABLE
    cols = [c for c in df.columns if str(c).startswith(C.WREP_PREFIX)]
    return WREP_NO_LIVE_BUYS if df[cols].isna().all().all() else None


def read_live_buys(src) -> pd.DataFrame:
    """The -export-live-buys CSV (`src` a path; a DataFrame is returned as is).
    Every column is read as text, so a wallet suffix such as "0042" or "1e10"
    stays the string it was (the call id is parsed by wallet_rep); only an
    empty field is missing."""
    if isinstance(src, pd.DataFrame):
        return src
    return pd.read_csv(src, dtype=str, keep_default_na=False, na_values=[""])


def wrep_weeks(dates: pd.Series, wrep: pd.DataFrame, rows) -> list:
    """Per calendar week (Monday, UTC) of the calls `rows` (a mask over `wrep`):
    calls, calls with live buys, the share of those with at least one known
    wallet, and their mean wrep_known_share (the coverage ramp)."""
    rows = np.asarray(rows, dtype=bool)
    week = pd.to_datetime(dates, utc=True).dt.tz_localize(None).dt.to_period("W-SUN").dt.start_time
    f = pd.DataFrame({"week": week.to_numpy()[rows],
                      "live": wrep["wrep_known_share"].notna().to_numpy()[rows],
                      "known": (wrep["wrep_n_known"] >= 1).to_numpy()[rows],
                      "share": wrep["wrep_known_share"].to_numpy()[rows]})
    out = []
    for w, g in f.groupby("week", sort=True):
        live = g[g["live"]]
        out.append({"week": w.strftime("%Y-%m-%d"), "calls": len(g), "with_live_buys": len(live),
                    "share_known": float(live["known"].mean()) if len(live) else np.nan,
                    "known_share_mean": float(live["share"].mean()) if len(live) else np.nan})
    return out


def _wrep_window() -> str:
    """", last N days" for C.WREP_LOOKBACK_DAYS = N, "" for all history (None)."""
    return "" if C.WREP_LOOKBACK_DAYS is None else f", last {C.WREP_LOOKBACK_DAYS} days"


def wrep_failure_warning(error: str, others_ok: bool = False) -> None:
    """The WARNING after the wallet reputation of C.WREP_LOOKBACK_DAYS failed.
    `others_ok`: another lookback's frame exists, so with all history only the
    variants using all history are n/a (not every wallet variant)."""
    window = _wrep_window()
    scope = "using that window " if window else ("using all history " if others_ok else "")
    print(f"WARNING: the wallet reputation{window} failed ({error}); the wallet reputation "
          f"variants {scope}are n/a, the saved models are not affected", flush=True)


def wallet_rep_data(df: pd.DataFrame, live_buys, rows, warn: bool = True) -> tuple:
    """(the wrep_* frame indexed like `df`, or None after an error; its stats:
    W.wrep_frame's, the fixed priors, `wrep_weeks` over the calls `rows` (the
    rows training can use) and the seconds taken, or {"error": ...}). Never
    raises: the wallet reputation only feeds a report-only variant. `warn`:
    print `wrep_failure_warning` itself after an error."""
    t0 = time.time()
    window = _wrep_window()
    try:
        wrep, stats = W.wrep_frame(df, read_live_buys(live_buys))
        stats = {**stats, "prior_runner": C.WREP_PRIOR_RUNNER, "prior_rug": C.WREP_PRIOR_RUG,
                 "weeks": wrep_weeks(df["message_date"], wrep, rows)}
    except Exception as exc:  # not BaseException: Ctrl-C still stops the run
        wrep, stats = None, {"error": variant_error(exc)}
        if warn:
            wrep_failure_warning(stats["error"])
    stats["seconds"] = round(time.time() - t0, 1)
    if wrep is not None:
        print(f"[wallet rep{window}] {stats['live_buy_rows']} live buys, {stats['wallets']} wallets, "
              f"{stats['calls_with_live_buys']} calls with live buys; {stats['seconds']:.0f} s",
              flush=True)
    return wrep, stats


def wrep_lookbacks() -> list:
    """The distinct WREP_LOOKBACK_DAYS the wallet reputation is computed for:
    the configured one (C.WREP_LOOKBACK_DAYS) first, then those of the
    C.WALLET_REP_VARIANTS among C.VARIANTS (e.g. C.WREP_LOOKBACK_VARIANT_DAYS)."""
    days = [C.WREP_LOOKBACK_DAYS] + [C.variant_settings(n)["WREP_LOOKBACK_DAYS"]
                                     for n in C.WALLET_REP_VARIANTS if n in C.VARIANTS]
    return list(dict.fromkeys(days))


def _live_buys_once(src):
    """read_live_buys(src), read once for every lookback; `src` itself when
    that fails (each wallet_rep_data then records the error)."""
    try:
        return read_live_buys(src)
    except Exception:  # not BaseException: Ctrl-C still stops the run
        return src


def wallet_rep_frames(df: pd.DataFrame, live_buys, rows) -> tuple:
    """({lookback: the wrep_* frame, or None after an error} for each of
    `wrep_lookbacks`, each computed by `wallet_rep_data` inside
    C.overrides(WREP_LOOKBACK_DAYS=lookback); the configured lookback's stats,
    with "lookbacks": {str(days): {"days", "seconds", "weeks"} or {"days",
    "seconds", "error"}} for the others). Never raises. The configured
    lookback's WARNING comes after the others are computed, so it can say
    whether only the variants using that history are n/a."""
    src = _live_buys_once(live_buys)
    frames, stats = {}, None
    for days in wrep_lookbacks():
        with C.overrides(WREP_LOOKBACK_DAYS=days):
            frames[days], st = wallet_rep_data(df, src, rows, warn=stats is not None)
        if stats is None:
            stats = {**st, "lookbacks": {}}
        else:
            stats["lookbacks"][str(days)] = {"days": days, **{k: st[k] for k in
                                             ("seconds", "weeks", "error") if k in st}}
    if stats.get("error"):
        wrep_failure_warning(stats["error"], any(f is not None for f in frames.values()))
    return frames, stats


def variant_data(df: pd.DataFrame, name: str, frames: dict | None) -> pd.DataFrame:
    """The rows variant `name` trains on: `df` itself, or for a
    C.WALLET_REP_VARIANTS entry `df` joined with the wrep_* frame of its own
    WREP_LOOKBACK_DAYS (`frames`, from `wallet_rep_frames`); `df` alone when that
    frame is missing (the variant is then n/a, `wrep_not_applicable`)."""
    if name not in C.WALLET_REP_VARIANTS:
        return df
    frame = (frames or {}).get(C.variant_settings(name)["WREP_LOOKBACK_DAYS"])
    return df if frame is None else df.join(frame)


def _as_float(x) -> float:
    """x as a float, NaN when missing (None) or not a number."""
    try:
        return float(x)
    except (TypeError, ValueError):
        return np.nan


def _rule(ok, **values) -> dict:
    """One adoption rule: holds True / False, or None (n/a) when an input is undefined."""
    if any(np.isnan(v) for v in values.values()):
        return {"holds": None, **values}
    return {"holds": bool(ok()), **values}


def _wrep_drift(adv: dict, rules: dict) -> list:
    """The wrep_* inputs among the top `drift_top` drifting inputs with a gain
    share >= `drift_max_share` (both from `rules`, C.WREP_ADOPTION): [[name, share], ...]."""
    top = list((adv or {}).get("top") or [])[:rules["drift_top"]]
    return [[f, float(g)] for f, g in top
            if str(f).startswith(C.WREP_PREFIX) and float(g) >= rules["drift_max_share"]]


def adoption_bucket(base: dict, var: dict, summary: dict) -> dict:
    """The pre-registered adoption rules (C.WREP_ADOPTION) for one bucket:
    `base` / `var` the baseline's / the variant's across-seeds row, `summary` the
    variant's bucket row. Each rule: {"holds": True / False / None (n/a), values};
    "result": "pass" (all hold), "lookback" (only the drift rule does not hold:
    next step a WREP_LOOKBACK_DAYS variant, not adoption; for a variant that
    already has a lookback, the drift persists: report.adoption_result_label)
    or "fail"."""
    r = C.WREP_ADOPTION
    v_med, b_med = _as_float(var.get("median")), _as_float(base.get("median"))
    lift_lo, sim_lo = _as_float(var.get("lift_lo")), _as_float(var.get("sim_gain_lo"))
    removed = _as_float(summary.get("collapse_removed"))
    v_adv, b_adv = var.get("adversarial") or {}, base.get("adversarial") or {}
    v_auc, b_auc = _as_float(v_adv.get("auc")), _as_float(b_adv.get("auc"))
    drifting = _wrep_drift(v_adv, r)
    rules = {
        "median_lift": _rule(lambda: v_med >= r["min_median_lift"] and v_med >= b_med,
                             variant=v_med, baseline=b_med),
        "lift_lower": _rule(lambda: lift_lo > r["min_lift_lower"], value=lift_lo),
        "sim_lower": _rule(lambda: sim_lo > r["min_sim_diff_lower"], value=sim_lo),
        "collapse_removed": _rule(lambda: removed >= r["min_collapse_removed"], value=removed),
        # rounded: 0.52 - 0.50 is 0.020000000000000018 in floating point
        "drift": {**_rule(lambda: round(v_auc - b_auc, 9) <= r["max_adv_auc_rise"] and not drifting,
                          variant=v_auc, baseline=b_auc),
                  "wrep_top": drifting},
    }
    others = all(rules[k]["holds"] is True for k in rules if k != "drift")
    result = ("pass" if others and rules["drift"]["holds"] is True
              else "lookback" if others else "fail")
    return {"rules": rules, "result": result}


def wallet_adoption(baseline: dict, variant: dict) -> dict:
    """The pre-registered adoption rules of one wallet reputation variant
    (any of C.WALLET_REP_VARIANTS) per bucket (`adoption_bucket`), from the
    baseline's and that variant's across-seeds rows. Report only: never a
    gate of the saved model."""
    out = {"rules": dict(C.WREP_ADOPTION), "set_on": C.WREP_ADOPTION_SET_ON,
           "saved_seed": C.LGBM_SEED, "buckets": {}}
    b_acr, v_acr = baseline.get("across_seeds") or {}, variant.get("across_seeds") or {}
    for b, summary in (variant.get("buckets") or {}).items():
        v_row = (v_acr.get("buckets") or {}).get(b)
        b_row = (b_acr.get("buckets") or {}).get(b)
        why = (v_acr.get("error") or b_acr.get("error")
               or (summary or {}).get("skipped") or (v_row or {}).get("skipped")
               or (None if v_row is not None and b_row is not None else "no across-seeds row"))
        out["buckets"][b] = ({"not_applicable": why} if why
                             else adoption_bucket(b_row, v_row, summary))
    return out


def add_wallet_adoption(variants: dict) -> None:
    """Add "adoption" (`wallet_adoption`) to every C.WALLET_REP_VARIANTS entry of
    `variants` (run_variants' result) that ran without an error. Report only: an
    error is recorded as {"error": ...}, never raised."""
    for name in C.WALLET_REP_VARIANTS:
        entry = variants.get(name)
        if entry is None or entry.get("not_applicable") or entry.get("error"):
            continue
        try:  # report only: never fails the run
            entry["adoption"] = wallet_adoption(variants.get("baseline") or {}, entry)
        except Exception as exc:
            entry["adoption"] = {"error": variant_error(exc)}


def _top_stats(y, score) -> list:
    """Per C.DEAD_SCORE_TOP_FRACS: precision of the top share by score (95% Wilson
    interval), lift over the base rate (interval: Wilson / base rate, base fixed)
    and the share of all positives caught by skipping that top share."""
    y = np.asarray(y, dtype=float)
    base, total = (float(y.mean()) if len(y) else np.nan), float(y.sum())
    out = []
    for frac in C.DEAD_SCORE_TOP_FRACS:
        top = V.top_mask(score, frac)
        k, n = int(y[top].sum()), int(top.sum())
        lo, hi = V.wilson(k, n)
        prec = k / n if n else np.nan
        ok = base > 0
        out.append({"frac": frac, "n_top": n, "dead_top": k, "precision": prec,
                    "precision_lo": lo, "precision_hi": hi,
                    "lift": prec / base if ok else np.nan,
                    "lift_lo": lo / base if ok else np.nan, "lift_hi": hi / base if ok else np.nan,
                    "caught": k / total if total > 0 else np.nan})
    return out


def _logistic_coefs(baseline) -> list:
    """[feature, coefficient, input scale] of the logistic baseline, largest |coef|
    first (C.DEAD_SCORE_N_COEF). Coefficients are per standard deviation of the
    standardised input: log1p(x) for skewed columns, x as is for the others."""
    names = list(C.LOG1P_FEATURES) + [c for c in C.NUMERIC_FEATURES if c not in C.LOG1P_FEATURES]
    coef = baseline[-1].coef_.ravel()
    if len(coef) != len(names):   # never expected (keep_empty_features); do not mislabel
        raise ValueError(f"{len(coef)} logistic coefficients for {len(names)} inputs")
    order = np.argsort(-np.abs(coef), kind="stable")[:C.DEAD_SCORE_N_COEF]
    return [[names[i], float(coef[i]), "log1p" if names[i] in C.LOG1P_FEATURES else "as is"]
            for i in order]


def _gate_index() -> int:
    """Position of C.DEAD_SCORE_GATE_FRAC (the "top-10%" share) in
    C.DEAD_SCORE_TOP_FRACS, looked up by value; raises if it is not there."""
    for i, f in enumerate(C.DEAD_SCORE_TOP_FRACS):
        if math.isclose(f, C.DEAD_SCORE_GATE_FRAC):
            return i
    raise ValueError(f"DEAD_SCORE_GATE_FRAC {C.DEAD_SCORE_GATE_FRAC} is not in "
                     f"DEAD_SCORE_TOP_FRACS {C.DEAD_SCORE_TOP_FRACS}")


def _dead_waves(dates, y, families=None) -> list:
    """Dead calls per calendar week (Monday, UTC), most dead first; with
    `families` (dex_family per row) also the dex_family counts of the dead calls."""
    week = pd.to_datetime(dates, utc=True).dt.tz_localize(None).dt.to_period("W-SUN").dt.start_time
    frame = pd.DataFrame({"week": week.to_numpy(), "dead": np.asarray(y, dtype=float)})
    g = frame.groupby("week")["dead"].agg(["sum", "size"])
    g = g.sort_values(["sum", "size"], ascending=False, kind="stable")
    fam = None
    if families is not None:
        frame["family"] = pd.Series(families).fillna("(none)").to_numpy()
        fam = frame[frame["dead"] == 1].groupby("week")["family"].value_counts()
    out = []
    for i, r in g.iterrows():
        w = {"week": i.strftime("%Y-%m-%d"), "dead": int(r["sum"]), "calls": int(r["size"])}
        if fam is not None:
            w["dead_families"] = ({str(k): int(v) for k, v in fam.loc[i].items()}
                                  if i in fam.index.get_level_values(0) else {})
        out.append(w)
    return out


def _dead_walk_forward(rows, y, dates, tokens, horizon_days, rounds) -> list:
    """Weekly expanding windows (as for the buckets): LightGBM with the main
    round count and the logistic baseline refitted per week; ranking only."""
    gi = _gate_index()
    out = []
    for w in V.walk_forward_windows(dates, tokens, horizon_days):
        tr, te = w["train"], w["test"]
        n_dead = int(y[te].sum())
        row = {"week": w["week"], "start": w["start"].strftime("%Y-%m-%d"),
               "n_train": int(tr.sum()), "n_test": int(te.sum()), "n_dead": n_dead,
               "few_dead": n_dead < C.DEAD_SCORE_MIN_WEEK_DEAD}
        row["enough_train"] = V.wf_enough_train(row)   # below C.WF_MIN_TRAIN_ROWS: shown, not counted
        if min(y[tr].sum(), (1 - y[tr]).sum()) < C.MIN_CLASS_ROWS or not V.window_ok(te.sum()):
            row["skipped"] = "too few training rows per class or test rows"
            row["counted"] = False
            out.append(row)
            continue
        row["counted"] = V.wf_counted(row)
        X = build_features(rows, learn_cat_levels(rows[tr]))
        p = predict(fit_lgbm(X[tr], y[tr], n_estimators=rounds), X[te])
        p_log = fit_baseline(X[tr], y[tr]).predict_proba(X[te][C.NUMERIC_FEATURES])[:, 1]
        base = float(y[te].mean())
        for name, s in (("lightgbm", p), ("logistic", p_log)):
            top = _top_stats(y[te], s)
            row[name] = {"roc_auc": V.model_metrics(y[te], s)["roc_auc"],
                         "top10_precision": top[gi]["precision"], "top10_lift": top[gi]["lift"],
                         "caught": {f"{t['frac']:g}": t["caught"] for t in top}}
        row["base_rate"] = base
        out.append(row)
    return out


def dead_score(df: pd.DataFrame, L: pd.DataFrame) -> dict:
    """The separate "dead after the call" score (report only; nothing saved).

    Label: dead = trades_24h < C.DEAD_TRADES_24H; rows with NULL trades_24h are
    left out (not negatives). Rows and split: the usable rows of
    C.DEAD_SCORE_SPLIT_BUCKET with its own time split (same function, same rows,
    so the same boundaries and embargo), then restricted to known trades_24h.
    Inputs: build_features, i.e. C.FEATURES (trades_24h is forbidden there)."""
    if C.DEAD_COLUMN not in df.columns:
        return {"not_applicable": f"the data has no {C.DEAD_COLUMN} column"}
    b = C.DEAD_SCORE_SPLIT_BUCKET
    h = C.BUCKETS[b]["horizon_days"]
    use = L[f"usable_{b}"].to_numpy()
    rows_b = df[use].reset_index(drop=True)
    trades = num(rows_b, C.DEAD_COLUMN).to_numpy()
    known = ~np.isnan(trades)
    res = {"bucket": b, "threshold": C.DEAD_TRADES_24H, "horizon_days": h,
           "usable": int(use.sum()), "known": int(known.sum()),
           "null_excluded": int((~known).sum()), "features": list(C.FEATURES)}
    if known.sum() < C.MIN_BUCKET_ROWS:
        res["skipped"] = (f"only {int(known.sum())} {b}-bucket rows with a known "
                          f"{C.DEAD_COLUMN} (need {C.MIN_BUCKET_ROWS})")
        return res
    split_b = V.time_split(rows_b["message_date"], rows_b["contract_address"], h)
    rows = rows_b[known].reset_index(drop=True)
    y = (trades[known] < C.DEAD_TRADES_24H).astype(float)
    split = {k: split_b[k][known] for k in ("train", "val", "test")}
    res["split"] = {"t1": split_b["t1"].strftime("%Y-%m-%d %H:%M"),
                    "t2": split_b["t2"].strftime("%Y-%m-%d %H:%M")}
    res["parts"] = {k: {"n": int(m.sum()), "dead": int(y[m].sum()),
                        "base_rate": float(y[m].mean()) if m.any() else np.nan}
                    for k, m in split.items()}
    dates, tokens = rows["message_date"], rows["contract_address"]
    res["waves"] = _dead_waves(dates, y, category(rows, "dex_family"))
    tr, te = split["train"], split["test"]
    if te.sum() < C.MIN_CLASS_ROWS:
        res["skipped"] = f"only {int(te.sum())} test rows after the time split"
        return res
    X = build_features(rows, learn_cat_levels(rows[tr]))
    info, fitted, p_test = _train_label(X, y, split, "dead", None, "dead", dates)
    res["label"] = info
    if fitted is None:
        res["skipped"] = info["skipped"]
        return res
    res["top"] = {"lightgbm": _top_stats(y[te], p_test),
                  "logistic": _top_stats(y[te], fitted["p_logistic"])}
    res["logistic_coefs"] = _logistic_coefs(fitted["baseline"])
    res["walk_forward"] = _dead_walk_forward(rows, y, dates, tokens, h, info["rounds"])
    res["proposal"] = dead_score_proposal(res)
    return res


def dead_score_proposal(res: dict) -> dict:
    """How this run fares against the PROPOSED gates (never applied): top-10%
    lift >= C.DEAD_SCORE_PROPOSED_LIFT in the test part, and in more than half of
    the walk-forward weeks with at least C.DEAD_SCORE_MIN_WEEK_DEAD dead calls
    that count (trained on >= C.WF_MIN_TRAIN_ROWS rows, as for the bucket gates)."""
    out = {"min_lift": C.DEAD_SCORE_PROPOSED_LIFT, "wf_min_train_rows": C.WF_MIN_TRAIN_ROWS}
    gi = _gate_index()
    for model in ("lightgbm", "logistic"):
        top = res["top"][model][gi]
        if not math.isclose(top["frac"], C.DEAD_SCORE_GATE_FRAC):
            raise ValueError(f"top share {top['frac']} where {C.DEAD_SCORE_GATE_FRAC} was expected")
        lift = top["lift"]
        weeks = [w for w in res["walk_forward"] if V.wf_counted(w) and not w["few_dead"]]
        meet = sum(bool(w[model]["top10_lift"] >= C.DEAD_SCORE_PROPOSED_LIFT) for w in weeks)
        out[model] = {"test_lift": lift,
                      "test_ok": bool(lift >= C.DEAD_SCORE_PROPOSED_LIFT),
                      "weeks_meeting": meet, "weeks_judged": len(weeks),
                      "weeks_ok": bool(weeks) and meet * 2 > len(weeks)}
        out[model]["would_pass"] = out[model]["test_ok"] and out[model]["weeks_ok"]
    return out


def run_dead_score(df: pd.DataFrame, L: pd.DataFrame) -> dict:
    """dead_score, never failing the run: an error is recorded as
    {"error": "error: <short message>"} and a WARNING printed."""
    t0 = time.time()
    try:
        out = dead_score(df, L)
    except Exception as exc:  # not BaseException: Ctrl-C still stops the run
        out = {"error": variant_error(exc)}
        print(f"WARNING: the dead score failed ({out['error']}); the saved models are not "
              "affected", flush=True)
    print(f"[dead score] {time.time() - t0:.0f} s", flush=True)
    return out


def diagnostics_on(flag: bool | None = None) -> bool:
    """Whether to run the stability diagnostics: `flag` when given; else the
    environment variable C.DIAGNOSTICS_ENV (0/off/false/no: off, 1/on/true/yes:
    on), else C.DIAGNOSTICS. Any other value of the variable: the default and a
    WARNING."""
    if flag is not None:
        return bool(flag)
    raw = os.environ.get(C.DIAGNOSTICS_ENV, "")
    val = raw.strip().lower()
    if val in ("0", "off", "false", "no"):
        return False
    if val in ("1", "on", "true", "yes"):
        return True
    if val:
        print(f"WARNING: {C.DIAGNOSTICS_ENV}={raw!r} is not one of 0/off/false/no or "
              f"1/on/true/yes; using the default ({'on' if C.DIAGNOSTICS else 'off'})", flush=True)
    return bool(C.DIAGNOSTICS)


def train(df: pd.DataFrame, out_root, version: str | None = None, variants: bool = False,
          diagnostics: bool | None = None, live_buys=None, set_latest: bool = True) -> Path:
    """Run the whole pipeline on raw view rows; returns the version directory.
    `version` (default `version_name()`) must not exist yet under `out_root`
    (`new_version_dir`). `set_latest`: point LATEST at the new version as soon
    as the saved model is complete (the default); False (train.py --candidate):
    never touch LATEST, and write the CANDIDATE pointer only at the very end,
    after every meta.json / report.md write.
    `variants`: also train C.VARIANTS for the report (never saved or served).
    `diagnostics`: the report-only stability diagnostics (None: `diagnostics_on`,
    i.e. C.DIAGNOSTICS unless C.DIAGNOSTICS_ENV says otherwise).
    `live_buys` (the -export-live-buys rows, a DataFrame or a CSV path; used only
    with `variants`): the wallet reputation (wrep_*) is computed once per
    lookback the C.WALLET_REP_VARIANTS use (`wallet_rep_frames`) and joined to
    their rows only (`variant_data`). The saved model,
    its gates, the dead score and the diagnostics never see it, so they are the
    same with or without it."""
    leaked = [c for c in C.FEATURES if c.startswith(C.WREP_PREFIX)]
    if leaked:   # serving cannot compute them: never inputs of the saved model
        raise ValueError(f"the saved model's inputs (config FEATURES) must not include the "
                         f"wallet reputation columns {leaked}; set WALLET_REP = False (the "
                         "WALLET_REP_VARIANTS test them, report only)")
    held = [c for c in C.FEATURES if c.startswith(C.HOLD_PREFIX)]
    if held:   # holder export columns: serving has no holder data (dead7_trial.py tests one, report only)
        raise ValueError(f"the saved model's inputs (config FEATURES) must not include the "
                         f"holder export columns {held}")
    diag = diagnostics_on(diagnostics)
    version = version or version_name()
    out_dir = new_version_dir(out_root, version)
    df = df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    policy = dead_policy(df)
    has_trades = C.DEAD_COLUMN in df.columns
    if policy == "missing":
        print(f"WARNING: the data has no {C.DEAD_COLUMN} column (the view on the server predates "
              "it; apply scoutanalytics.sql, see RUNBOOK.md): the dead-after-the-call rule is "
              "SKIPPED, collapse labels are the plain ones, and the separate dead score is n/a",
              flush=True)
    elif not has_trades:
        print(f"WARNING: the data has no {C.DEAD_COLUMN} column (the view on the server predates "
              "it; apply scoutanalytics.sql, see RUNBOOK.md): the dead-after-the-call table and "
              "the separate dead score are n/a", flush=True)
    L = build_labels(df, policy)
    call = ~L["update"]  # update posts are not calls: out of training, counted on their own
    first = call & ~L["repeat"]  # later calls of a token: out of training, counted on their own
    extreme = first & ~L["no_pool"] & ~L["not_usd"] & L["extreme"]
    eligible = first & ~L["no_pool"] & ~L["not_usd"] & ~L["extreme"]
    wrep_frames = wrep_stats = None   # the wallet reputation, per lookback: for the variants only
    if live_buys is not None and not variants:
        print("NOTE: --live-buys is used only with --variants (the wallet reputation "
              "variant); ignored", flush=True)
    elif live_buys is not None:
        wrep_frames, wrep_stats = wallet_rep_frames(df, live_buys, eligible.to_numpy())

    raw_cols = C.NUMERIC_RAW + C.CATEGORICAL_VIEW + ["pre_vol_unit", "message_date"]
    blank = df.reindex(columns=raw_cols).replace("", np.nan)
    res = {"version": version, "buckets": {}, "data": {
        "rows": len(df), "first_date": df["message_date"].min().strftime("%Y-%m-%d"),
        "last_date": df["message_date"].max().strftime("%Y-%m-%d"),
        "update": int(L["update"].sum()),
        "repeat": int((call & L["repeat"]).sum()), "no_pool": int((first & L["no_pool"]).sum()),
        "not_usd": int((first & ~L["no_pool"] & L["not_usd"]).sum()),
        "extreme": int(extreme.sum()),
        "eligible": int(eligible.sum()),
        "dead_policy": policy,
        "trades_24h_present": has_trades,
        "trades_24h_known": int((eligible & num(df, C.DEAD_COLUMN).notna()).sum()),
        "dead": int((eligible & L["dead"]).sum()),
        # runner label also needs trades_24h >= this (None: rule off, the default)
        "runner_min_trades_24h": C.RUNNER_MIN_TRADES_24H if C.RUNNER_NEEDS_TRADES else None,
        # over the rows training can use (repeat calls and update posts have no pool data)
        "coverage": {c: float(blank[c][eligible].notna().mean()) if eligible.any() else 0.0
                     for c in raw_cols}}}
    reference, cat_levels = {}, {}  # per trained bucket: learned from its training rows
    collapse_ref = {}   # per trained bucket: the collapse filter's fixed cut-off
    keeps = {}   # arrays of each trained bucket: gate inputs and diagnostics (never in meta.json)
    for b in C.BUCKETS:
        keep = {}
        res["buckets"][b], ref, levels = _train_bucket(b, df, L, out_dir, has_trades, keep=keep)
        if keep.get("wf") is not None:
            keeps[b] = keep
        if ref is not None:
            reference[b], cat_levels[b] = ref, levels
            collapse_ref[b] = keep.get("collapse_reference") or {"skipped": "not computed"}
    res["collapse_reference"] = collapse_ref
    # The bucket gates (owner decision of 13 Oct 2026): seed retrains and the pooled
    # walk-forward, in every training (also with the diagnostics off), before meta.json.
    gate_in = gate_inputs(df, L, res["buckets"], keeps, has_trades)
    apply_gates(res["buckets"], gate_in)
    apply_collapse_filter(res["buckets"], keeps, gate_in["pooled"])   # never a gate
    for b, status in res["buckets"].items():
        print(f"[{b}] " + (f"skipped: {status['skipped']}" if status.get("skipped") else
                           "PASS" if status["gates"]["passed"] else
                           "FAIL: " + "; ".join(status["gates"]["reasons"]) + " (see report.md)"))

    models = sorted(p.stem for p in out_dir.glob("*.joblib") if p.stem.split("_")[0] in reference)
    for p in out_dir.glob("*.joblib"):  # a collapse model without its runner model is unusable
        if p.stem not in models:
            p.unlink()
    meta = {"version": version, "trained_at": datetime.now(timezone.utc).isoformat(),
            "features": list(C.FEATURES), "categorical": C.CATEGORICAL,
            "category_levels": cat_levels,
            "thresholds": {"buckets": C.BUCKETS, "gates": C.GATES, "gate_rules": gate_rules(),
                           "filter_rules": filter_rules()},
            "dead": {"policy": res["data"]["dead_policy"], "column": C.DEAD_COLUMN,
                     "max_trades_24h": C.DEAD_TRADES_24H},
            "dex_family_rules": dex_family_rules(),  # serving maps dex -> family with these
            "calib_recent_days": C.CALIB_RECENT_DAYS,
            "calib_choice_fit_frac": C.CALIB_CHOICE_FIT_FRAC,
            "wf_min_train_rows": C.WF_MIN_TRAIN_ROWS,
            "models": models, "runner_reference": reference, "collapse_reference": collapse_ref,
            "data": res["data"], "metrics": res["buckets"]}
    meta = json.loads(json.dumps(_json_safe(meta), allow_nan=False))  # fixed before any variant runs
    _write_version(out_dir, meta, res)
    if set_latest:
        write_pointer(Path(out_root), C.LATEST_POINTER, version)
    # The saved model is complete above; the dead score and the variants only add
    # to meta.json / report.md, and an error in either never fails the run.
    res["dead_score"] = run_dead_score(df, L)
    try:  # rendering the section must not fail the run either
        meta["dead_score"] = _json_safe(res["dead_score"])
        _write_version(out_dir, meta, res)
    except Exception as exc:
        res["dead_score"] = meta["dead_score"] = {"error": variant_error(exc)}
        print(f"WARNING: the dead score failed ({meta['dead_score']['error']}); the saved "
              "models are not affected", flush=True)
        _write_version(out_dir, meta, res)
    if diag:   # report only, after the saved model is complete; never fails the run
        res["diagnostics"] = run_diagnostics(df, L, res["buckets"], keeps, has_trades, gate_in)
        try:
            meta["diagnostics"] = _json_safe(res["diagnostics"])
            _write_version(out_dir, meta, res)
        except Exception as exc:
            res["diagnostics"] = meta["diagnostics"] = {"error": variant_error(exc)}
            print(f"WARNING: the stability diagnostics failed ({meta['diagnostics']['error']}); "
                  "the saved models are not affected", flush=True)
            _write_version(out_dir, meta, res)
    if variants:
        # the baseline row of "Variants across seeds" reuses the diagnostics'
        # adversarial validation when it ran (else it is computed from `keeps`)
        adv = (res.get("diagnostics") or {}).get("adversarial")
        adv_known = ({b: v["split"] for b, v in adv.items()
                      if isinstance(v, dict) and isinstance(v.get("split"), dict)}
                     if isinstance(adv, dict) else {})
        # the wrep_* columns reach the wallet variants only, each with its own
        # lookback (the baseline above used `df`)
        res["variants"] = run_variants(df, res["buckets"], has_trades, gate_in["pooled"],
                                       gate_in["runs"], keeps, adv_known, wrep_frames)
        meta["variants"] = _json_safe(res["variants"])
        if wrep_stats is not None:
            res["wallet_rep"] = wrep_stats
            meta["wallet_rep"] = _json_safe(wrep_stats)
        _write_version(out_dir, meta, res)
    if not set_latest:   # last: the directory is complete when the pointer appears
        write_pointer(Path(out_root), C.CANDIDATE_POINTER, version)
    keeps.clear()
    return out_dir


def version_name() -> str:
    """The default version name: the current UTC minute, e.g. 20261018-1530."""
    return datetime.now(timezone.utc).strftime("%Y%m%d-%H%M")


def new_version_dir(out_root, version: str) -> Path:
    """Create out_root/version (and out_root if needed). An existing version
    directory (e.g. a second run in the same UTC minute) is refused with
    FileExistsError and left untouched: nothing is written into it."""
    out_dir = Path(out_root) / version
    Path(out_root).mkdir(parents=True, exist_ok=True)
    try:
        out_dir.mkdir()
    except FileExistsError:
        raise FileExistsError(f"the version directory {out_dir} already exists (a run in the "
                              "same UTC minute?); nothing was written. Wait for the next minute, "
                              "or delete that directory if it is a stopped run's leftover, and "
                              "run again") from None
    return out_dir


# --- Stability diagnostics (report only; see config "Stability diagnostics") ---

def _wf_weeks(keep: dict, exclude_dead: bool = False) -> list:
    """Per counted walk-forward week of `keep["wf"]`: D.week_sums of its test
    rows (`exclude_dead`: without dead calls, then only weeks that still have
    C.MIN_WINDOW_TEST_ROWS rows), each week ranked by its own model."""
    out = []
    for w in keep["wf"]:
        if not w["counted"]:
            continue
        idx, mask = w["idx"], None
        if exclude_dead:
            mask = ~keep["dead"][idx]
            if not V.window_ok(int(mask.sum())):
                continue
        out.append(D.week_sums(keep["Y"]["runner"][idx], w["s_runner"], keep["y_plain"][idx],
                               w["s_collapse"], keep["net_ret"][idx], mask))
    return out


SEED_METRICS = ("top_lift", "collapse_removed", "runner_auc", "sim_top_mean", "sim_all_mean",
                "windows_beating", "wf_pooled_lift")


def _run_summary(r: dict, keep: dict | None) -> dict:
    """The seed-stability numbers of one bucket of one training run (its gate
    fields are the old single-run gates; `wf_pooled_lift` feeds the bucket gate)."""
    if r.get("skipped"):
        return {"skipped": r["skipped"]}
    t, g, lab = r["trading"], V.single_run_gates(r["gates"]), r["labels"]["runner"]
    pool = D.pooled(_wf_weeks(keep), reps=0) if keep and keep.get("wf") is not None else {}
    return {"top_lift": t["top_lift"], "top_lift_lo": t["top_lift_lo"],
            "top_lift_hi": t["top_lift_hi"], "collapse_removed": t["collapse_removed"],
            "runner_auc": lab["lightgbm"]["roc_auc"], "sim_top_mean": t["sim_top_mean"],
            "sim_all_mean": t["sim_all_mean"], "windows_beating": g["windows_beating"],
            "windows_evaluated": g["windows_evaluated"], "passed": g["passed"],
            "wf_pooled_lift": pool.get("lift", np.nan)}


def seed_runs(df, L, buckets: dict, keeps: dict, has_trades: bool,
              on_fail: str = "every trained bucket FAILS its runner-lift gate") -> dict:
    """The baseline retrained with each C.STABILITY_SEEDS LightGBM seed (a seed
    equal to C.LGBM_SEED reuses the saved model's results; nothing is saved):
    {seed: {"buckets": {bucket: _run_summary}, ...} or {"error": ...}}. A gate
    input (the median pooled lift) and the seed-stability table. `buckets`:
    the saved model's results with their single-run gates. Inside a variant's
    C.overrides it retrains that variant instead (`variant_across_seeds`;
    `on_fail`: what a failed seed's WARNING says follows from it)."""
    runs = {}
    for seed in dict.fromkeys(C.STABILITY_SEEDS):
        if seed == C.LGBM_SEED:
            runs[seed] = {"saved_model": True, "buckets": {
                b: _run_summary(r, keeps.get(b)) for b, r in buckets.items()}}
            continue
        t0 = time.time()
        try:
            out = {}
            with C.overrides(LGBM_SEED=seed):
                for b in C.BUCKETS:
                    keep = {}
                    r = _train_bucket(b, df, L, None, has_trades, keep=keep)[0]
                    out[b] = _run_summary(r, keep)
            runs[seed] = {"buckets": out}
        except Exception as exc:  # one seed failing never stops the others
            runs[seed] = {"error": variant_error(exc)}
            print(f"WARNING: seed {seed} of the seed retrains failed ({runs[seed]['error']}); "
                  f"{on_fail}", flush=True)
        runs[seed]["seconds"] = round(time.time() - t0, 1)
    return runs


def _seed_lifts(runs: dict, b: str) -> dict:
    """Per seed of C.STABILITY_SEEDS: the pooled walk-forward lift of bucket `b`,
    or a string saying why there is none (for V.pooled_gates)."""
    out = {}
    for seed in C.STABILITY_SEEDS:
        run = runs.get(seed)
        s = ((run or {}).get("buckets") or {}).get(b)
        if run is None:
            out[seed] = "not run"
        elif run.get("error"):
            out[seed] = f"retrain failed ({run['error']})"
        elif s is None:
            out[seed] = "bucket not trained"
        elif s.get("skipped"):
            out[seed] = f"bucket skipped ({s['skipped']})"
        else:
            out[seed] = s.get("wf_pooled_lift")
    return out


def gate_inputs(df, L, buckets: dict, keeps: dict, has_trades: bool) -> dict:
    """What the bucket gates need (owner decision of 13 Oct 2026), computed in
    every training, also with the stability diagnostics off: the seed retrains
    (`seed_runs`) and, per trained bucket, the pooled expanding walk-forward of
    the saved seed with the week-block bootstrap. A part that raises is
    recorded as an error (the buckets concerned then FAIL); never fails the run."""
    t0 = time.time()
    try:
        runs = seed_runs(df, L, buckets, keeps, has_trades)
    except Exception as exc:  # not BaseException: Ctrl-C still stops the run
        err = variant_error(exc)
        runs = {s: {"error": err} for s in C.STABILITY_SEEDS}
        print(f"WARNING: the seed retrains failed ({err}); every trained bucket FAILS", flush=True)
    t1 = time.time()
    pooled = {}
    for b, k in keeps.items():
        try:
            pooled[b] = D.pooled(_wf_weeks(k), with_filter=True)   # filter_*: report only
        except Exception as exc:
            pooled[b] = {"error": variant_error(exc)}
            print(f"WARNING: the pooled walk-forward of {b} failed ({pooled[b]['error']}); "
                  f"{b} FAILS", flush=True)
    seconds = {"seeds": round(t1 - t0, 1), "pooled": round(time.time() - t1, 1)}
    print(f"[gates] seed retrains {seconds['seeds']:.0f} s, pooled walk-forward "
          f"{seconds['pooled']:.0f} s", flush=True)
    return {"runs": runs, "pooled": pooled, "seconds": seconds}


def apply_gates(buckets: dict, gate_in: dict) -> None:
    """Replace each trained bucket's single-run gates by the verdict of
    V.pooled_gates (the single-run gates kept under "old")."""
    for b, r in buckets.items():
        if r.get("skipped"):
            continue
        r["gates"] = V.pooled_gates(r["gates"], r["trading"], gate_in["pooled"].get(b),
                                    _seed_lifts(gate_in["runs"], b))


# --- Collapse filter (never a gate): metrics[b]["collapse_filter"] ---

def filter_labels(b: str) -> dict:
    """The runner and collapse label rules of bucket `b` as text, and whether
    one call can be both (the two rules read different columns, e.g. 1d: the
    runner label is the peak max_gain_late_1d, the collapse label the return;
    or the collapse label also counts rugged calls)."""
    cfg = C.BUCKETS[b]

    def rule(lab):
        return f"{cfg[lab][0]} {cfg[lab][1]} {cfg[lab][2]:g}%"
    return {"runner_label": rule("runner"),
            "collapse_label": rule("collapse") + (" OR rugged" if cfg["collapse_or_rugged"] else ""),
            "can_be_both": bool(cfg["runner"][0] != cfg["collapse"][0] or cfg["collapse_or_rugged"])}


def filter_weeks(keep: dict) -> list:
    """Per counted walk-forward week (the pooled gate's weeks): the collapse
    filter of that week's test calls, flagged by the week's own collapse model."""
    out = []
    for w in keep["wf"]:
        if not w["counted"]:
            continue
        row = {"week": w["week"], "start": w["start"]}
        if w["s_collapse"] is None:
            out.append({**row, "skipped": "no collapse model"})
            continue
        idx = w["idx"]
        out.append({**row, **V.collapse_filter_stats(
            keep["y_plain"][idx], keep["Y"]["runner"][idx], keep["net_ret"][idx],
            V.top_mask(w["s_collapse"], C.SKIP_FRAC))})
    return out


def filter_pooled(p: dict | None, weeks: list) -> dict:
    """The pooled collapse filter: the "filter_*" rates of the pooled expanding
    walk-forward `p` (D.pooled of the gate weeks, so `collapses_caught` is its
    `collapse_removed`; 95% week-block bootstrap as `<key>_lo` / `<key>_hi`)
    and the counts summed over `weeks` (`filter_weeks`). Every rate key is
    there, NaN when undefined (e.g. no counted week)."""
    if p is None or p.get("error"):
        return {"error": (p or {}).get("error") or "pooled walk-forward not computed"}
    out = {"weeks": p["weeks"], "reps": p["reps"], "seed": p["seed"]}
    counted = [w for w in weeks if not w.get("skipped")]
    for k in V.FILTER_COUNTS:
        out[k] = sum(w[k] for w in counted) if len(counted) == len(weeks) else None
    for k in V.FILTER_RATES:
        for key in (k, f"{k}_lo", f"{k}_hi"):
            out[key] = p.get(f"filter_{key}", np.nan)
    return out


def collapse_filter(b: str, keep: dict, pooled: dict | None) -> dict:
    """The collapse filter of bucket `b` (report only; never a gate, never
    served): skip the C.SKIP_FRAC calls with the highest collapse score (the
    calls "collapses removed" counts) and see what that catches and costs, on
    the saved model's test part ("test", = trading.collapse_removed) and on the
    pooled expanding walk-forward ("pooled", "weeks"). Collapse = the plain
    collapse label, as the gate."""
    s_c = keep["scores"].get("collapse")
    te = keep["te"]
    test = ({"skipped": "no collapse model"} if s_c is None else V.collapse_filter_stats(
        keep["y_plain"][te], keep["Y"]["runner"][te], keep["net_ret"][te],
        V.top_mask(s_c, C.SKIP_FRAC)))
    weeks = filter_weeks(keep)
    return {"flag_frac": C.SKIP_FRAC, "random_skip_expected": C.SKIP_FRAC,
            "sim_max_ret_pct": C.SIM_MAX_RET_PCT, **filter_labels(b),
            "test": test, "pooled": filter_pooled(pooled, weeks), "weeks": weeks}


def filter_at_cutoff(keep: dict) -> dict:
    """The collapse filter on the saved model's test part with the FIXED cut-off
    (keep["collapse_reference"]["cutoff"]): flagged = test calls whose collapse
    score is >= it. Information only (the live flagged share; the verdict is
    taken on the pooled walk-forward). {"skipped": reason} without a cut-off."""
    ref = keep.get("collapse_reference") or {}
    s_c = keep["scores"].get("collapse")
    cutoff = ref.get("cutoff")
    if s_c is None or cutoff is None or not V._finite(cutoff):
        return {"skipped": ref.get("skipped") or ref.get("error") or "no cut-off"}
    te = keep["te"]
    return {"cutoff": cutoff, **V.collapse_filter_stats(
        keep["y_plain"][te], keep["Y"]["runner"][te], keep["net_ret"][te],
        np.asarray(s_c, dtype=float) >= cutoff)}


def apply_collapse_filter(buckets: dict, keeps: dict, pooled: dict) -> None:
    """metrics[b]["collapse_filter"] for every trained bucket, with its
    "verdict" (V.filter_verdict on the pooled block; owner decision of
    18 Oct 2026; never a gate) and "test_at_cutoff" (`filter_at_cutoff`,
    information). An error is recorded there with a WARNING and leaves no
    verdict (= not validated); it never fails the run or changes anything else."""
    for b, k in keeps.items():
        try:
            cf = collapse_filter(b, k, pooled.get(b))
            cf["verdict"] = V.filter_verdict(cf["pooled"], b in C.FILTER_BUCKETS)
            cf["test_at_cutoff"] = filter_at_cutoff(k)
            buckets[b]["collapse_filter"] = cf
        except Exception as exc:
            buckets[b]["collapse_filter"] = {"error": variant_error(exc)}
            print(f"WARNING: the collapse filter of {b} failed "
                  f"({buckets[b]['collapse_filter']['error']}); nothing else is affected",
                  flush=True)


def gate_rules() -> dict:
    """The gate settings written to meta.json (thresholds.gate_rules)."""
    return {"set_on": C.GATES_SET_ON, "seeds": list(C.STABILITY_SEEDS), "saved_seed": C.LGBM_SEED,
            "min_pooled_lift_median": C.GATES["min_top_lift"],
            "min_pooled_lift_lower": C.GATE_POOLED_LIFT_LOWER,
            "min_sim_diff_lower": C.GATE_SIM_DIFF_LOWER,
            "min_collapse_removed": C.GATES["min_collapse_removed"],
            "min_pooled_weeks": C.GATE_MIN_POOLED_WEEKS,
            "bootstrap_reps": C.POOLED_BOOTSTRAP_REPS, "bootstrap_seed": C.POOLED_BOOTSTRAP_SEED,
            "ci": list(C.POOLED_CI), "wf_min_train_rows": C.WF_MIN_TRAIN_ROWS,
            "sim_max_ret_pct": C.SIM_MAX_RET_PCT}


def filter_rules() -> dict:
    """The collapse filter verdict settings written to meta.json
    (thresholds.filter_rules; owner decision of 18 Oct 2026)."""
    return {"set_on": C.FILTER_RULES_SET_ON, "seen_report": C.FILTER_RULES_SEEN_REPORT,
            "note": C.FILTER_RULES_NOTE, "buckets": list(C.FILTER_BUCKETS),
            **C.FILTER_RULES, "flag_frac": C.SKIP_FRAC,
            "min_pooled_weeks": C.GATE_MIN_POOLED_WEEKS,
            "cutoff_min_rows": C.FILTER_CUTOFF_MIN_ROWS}


def seed_stability(runs: dict, buckets: dict) -> dict:
    """The seed-stability table from the seed retrains of the gate inputs
    (`seed_runs`): per bucket the numbers of every run and their spread
    (min / median / max)."""
    spread = {}
    for b in buckets:
        ok = [v["buckets"][b] for v in runs.values()
              if not v.get("error") and not v["buckets"].get(b, {"skipped": 1}).get("skipped")]
        if ok:
            spread[b] = {m: D.spread([s[m] for s in ok]) for m in SEED_METRICS}
    return {"seeds": list(C.STABILITY_SEEDS), "saved_seed": C.LGBM_SEED,
            "runs": {str(k): v for k, v in runs.items()}, "spread": spread}


def _wf_week_row(w: dict) -> dict:
    keys = ("n_train", "n_test", "top_lift", "collapse_removed", "sim_top_mean", "sim_all_mean",
            "sim_beats_all", "runner_auc", "counted", "skipped")
    return {k: w[k] for k in keys if k in w}


def walk_forward_diagnostics(buckets: dict, keeps: dict, pooled: dict) -> dict:
    """Per trained bucket: the pooled expanding walk-forward (the gate's weeks;
    `pooled`: the gate inputs' pooled numbers per bucket, reused) and a rolling
    walk-forward (C.WF_ROLLING_WEEKS weeks of training calls), both pooled with the week-block bootstrap over the same weeks: the weeks the
    expanding walk-forward counts, minus any week the rolling model could not
    train (marked in the per-week table) (a paired comparison; a rolling window is
    small by design, so the C.WF_MIN_TRAIN_ROWS rule is not applied to its own
    training size, which the table shows), and the two side by side per week."""
    out = {}
    for b, k in keeps.items():
        roll = []
        weeks_roll = _walk_forward(k["rows"], k["Y"], k["y_plain"], k["y_with_dead"], k["net_ret"],
                                   k["dates"], k["tokens"], k["h"], k["rounds"], k["with_dead"],
                                   train_weeks=C.WF_ROLLING_WEEKS, collect=roll)
        same = {w["week"] for w in buckets[b]["walk_forward"] if V.wf_counted(w)}
        for w in roll + weeks_roll:
            w["counted"] = w["week"] in same and not w.get("skipped")
        by_week = {w["week"]: w for w in weeks_roll}
        counted_roll = [w for w in weeks_roll if w["counted"]]
        out[b] = {"expanding": D.without_filter(pooled[b]),
                  "rolling": D.pooled(_wf_weeks({**k, "wf": roll})),
                  "rolling_weeks": C.WF_ROLLING_WEEKS,
                  "expanding_gate": {x: V.single_run_gates(buckets[b]["gates"])[x]
                                     for x in ("windows_evaluated", "windows_beating")},
                  "rolling_gate": {"windows_evaluated": len(counted_roll),
                                   "windows_beating": sum(bool(w["sim_beats_all"])
                                                          for w in counted_roll)},
                  "weeks": [{"week": w["week"], "start": w["start"],
                             "expanding": _wf_week_row(w),
                             "rolling": _wf_week_row(by_week.get(w["week"], {}))}
                            for w in buckets[b]["walk_forward"]]}
    return out


def no_dead_evaluation(buckets: dict, keeps: dict, has_trades: bool) -> dict:
    """The same trained models evaluated without dead calls (C.EVAL_EXCLUDE_DEAD):
    test part and counted walk-forward weeks, top shares re-taken among the
    calls that are not dead. Evaluation only (labels, training, gates unchanged)."""
    if not C.EVAL_EXCLUDE_DEAD:
        return {"off": "EVAL_EXCLUDE_DEAD = False"}
    if not has_trades:
        return {"not_applicable": f"the data has no {C.DEAD_COLUMN} column"}
    out = {}
    for b, k in keeps.items():
        te = k["te"]
        dead_te = k["dead"][te]
        y_r, s_r, s_c = k["Y"]["runner"][te], k["scores"]["runner"], k["scores"].get("collapse")
        rows = {}
        for name, m, exclude in (("all calls", np.ones(len(y_r), bool), False),
                                 ("dead calls excluded", ~dead_te, True)):
            t = V.trading_metrics(y_r[m], s_r[m], k["y_plain"][te][m],
                                  None if s_c is None else s_c[m], k["net_ret"][te][m])
            pool = D.pooled(_wf_weeks(k, exclude_dead=exclude))
            rows[name] = {**t, "n_test": int(m.sum()),
                          "runner_auc": V.model_metrics(y_r[m], s_r[m])["roc_auc"],
                          "wf_weeks": pool["weeks"],
                          "wf_weeks_beating": pool.get("weeks_beating", 0),
                          "wf_pooled": {x: pool.get(x, np.nan) for x in
                                        ("lift", "lift_lo", "lift_hi", "collapse_removed",
                                         "sim_top_mean", "sim_all_mean")}}
        out[b] = {"dead_test": int(dead_te.sum()), "rows": rows}
    return out


def adversarial_validation(keeps: dict) -> dict:
    """Per trained bucket: train part vs test part (the bucket's own feature
    matrix, levels from its train part), and with C.ADV_PER_WEEK per counted
    walk-forward week (that week's training rows vs its test rows; the newest
    C.ADV_MAX_WEEKS counted weeks, C.ADV_WEEK_FOLDS folds)."""
    out = {}
    for b, k in keeps.items():
        m = k["tr"] | k["te"]
        res = {"split": D.adversarial(k["X"][m], k["te"][m]), "weeks": []}
        if C.ADV_PER_WEEK:
            counted = [w for w in k["wf"] if w["counted"]]
            for w in counted[len(counted) - max(C.ADV_MAX_WEEKS, 0):]:
                te_w = np.zeros(len(k["rows"]), bool)
                te_w[w["idx"]] = True
                mm = w["train"] | te_w
                Xw = build_features(k["rows"], learn_cat_levels(k["rows"][w["train"]]))
                res["weeks"].append({"week": w["week"], "start": w["start"],
                                     **D.adversarial(Xw[mm], te_w[mm], C.ADV_WEEK_FOLDS)})
        out[b] = res
    return out


def run_diagnostics(df, L, buckets: dict, keeps: dict, has_trades: bool, gate_in: dict) -> dict:
    """The stability diagnostics section; each part that raises is recorded
    as {"error": ...} with a WARNING and the others still run. `gate_in` (the
    gate inputs of this training, `gate_inputs`): its seed retrains and pooled
    expanding walk-forward are reused, not computed again."""
    out = {"settings": {"seeds": list(C.STABILITY_SEEDS), "saved_seed": C.LGBM_SEED,
                        "rolling_weeks": C.WF_ROLLING_WEEKS, "reps": C.POOLED_BOOTSTRAP_REPS,
                        "bootstrap_seed": C.POOLED_BOOTSTRAP_SEED,
                        "eval_exclude_dead": C.EVAL_EXCLUDE_DEAD, "adv_folds": C.ADV_FOLDS,
                        "adv_per_week": C.ADV_PER_WEEK},
           "seconds": {}}
    parts = (("seeds", lambda: seed_stability(gate_in["runs"], buckets)),
             ("walk_forward", lambda: walk_forward_diagnostics(buckets, keeps, gate_in["pooled"])),
             ("no_dead", lambda: no_dead_evaluation(buckets, keeps, has_trades)),
             ("adversarial", lambda: adversarial_validation(keeps)))
    for name, fn in parts:
        t0 = time.time()
        try:
            out[name] = fn()
        except Exception as exc:  # not BaseException: Ctrl-C still stops the run
            out[name] = {"error": variant_error(exc)}
            print(f"WARNING: diagnostics part {name!r} failed ({out[name]['error']}); the saved "
                  "models are not affected", flush=True)
        out["seconds"][name] = round(time.time() - t0, 1)
    # the seed retrains ran with the gate inputs: their time is shown here
    out["seconds"]["seeds"] += gate_in["seconds"]["seeds"]
    total = sum(out["seconds"].values())
    print("[diagnostics] " + ", ".join(f"{k} {v:.0f} s" for k, v in out["seconds"].items())
          + f"; total {total:.0f} s", flush=True)
    return out


def _write_version(out_dir: Path, meta: dict, res: dict) -> None:
    (out_dir / "meta.json").write_text(json.dumps(meta, indent=1, allow_nan=False))
    (out_dir / "report.md").write_text(render(res), encoding="utf-8")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--csv", help="file written by the Go program's -export-dataset")
    src.add_argument("--dsn", help="Postgres DSN; reads scout_call_dataset_v (needs psycopg)")
    ap.add_argument("--out", default="models", help="output root (default: models)")
    ap.add_argument("--variants", action="store_true",
                    help="also train the one-change variants of config.VARIANTS and compare them "
                         "in report.md (report only: the saved model is always the baseline)")
    ap.add_argument("--live-buys", metavar="PATH",
                    help="with --variants: the Go program's -export-live-buys CSV; adds the "
                         "report-only wallet reputation variants (config WALLET_REP_VARIANTS, "
                         "each with its own WREP_LOOKBACK_DAYS; n/a without it); "
                         "never used by the saved model")
    ap.add_argument("--candidate", action="store_true",
                    help="never write LATEST: write the CANDIDATE pointer instead, at the very end "
                         "of the run (the model is not served until it is promoted)")
    args = ap.parse_args(argv)
    df = pd.read_csv(args.csv, low_memory=False) if args.csv else read_dsn(args.dsn)
    out_dir = train(df, args.out, variants=args.variants, live_buys=args.live_buys,
                    set_latest=not args.candidate)
    print(f"wrote {out_dir} (report: {out_dir / 'report.md'})")
    return 0  # gate failures are reported, not errors


if __name__ == "__main__":
    sys.exit(main())
