"""Train runner/collapse models for every bucket and write models/<version>/.

    python train.py --csv calls.csv --out models/
    python train.py --dsn postgres://user:pass@host/db --out models/
"""
import argparse
import json
import math
import sys
from datetime import datetime, timezone
from pathlib import Path

import joblib
import numpy as np
import pandas as pd

from scout_ml import config as C
from scout_ml.features import build_features, dex_family_rules, learn_cat_levels, num
from scout_ml.labels import build_labels, dead_policy
from scout_ml.model import (apply_calibrator, best_rounds, fit_baseline, fit_lgbm, fit_platt,
                            fit_platt_recent, predict, usable_holdout)
from scout_ml.report import render
from scout_ml import validate as V


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


def _calibrate_recent(fitted, info, raw, y, dates):
    """Replace the calibrator by one fitted on the most recent held-out rows
    (`raw`/`y`/`dates`: validation or out-of-fold rows only); the calibrator on
    all of them (the earlier behaviour) is kept for the report."""
    fitted["calibrator_all"] = fit_platt(raw, y)
    fitted["calibrator"], info["calib"] = fit_platt_recent(raw, y, dates)
    if info["calib"]["note"]:
        info["messages"].append("calibration on all held-out rows: " + info["calib"]["note"])


def _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label):
    """Test metrics, importance and the saved model for a fitted label."""
    if fitted["note"]:
        info["messages"].append(fitted["note"])
    p_test = predict(fitted, X[te])
    if fitted.get("calibrator_all") is not None:  # before: calibrated on all held-out rows
        p_all = apply_calibrator(fitted["calibrator_all"],
                                 fitted["model"].predict(X[te], raw_score=True))
        info["calibration_all_rows"] = V.calibration_table(y[te], p_all)
        info["brier_all_rows"] = V.model_metrics(y[te], p_all)["brier"]
    gain = fitted["model"].booster_.feature_importance("gain")
    order = np.argsort(-gain)[:10]
    info.update(
        lightgbm=V.model_metrics(y[te], p_test),
        logistic=V.model_metrics(y[te], fit_baseline(X[tr], y[tr]).predict_proba(
            X[te][C.NUMERIC_FEATURES])[:, 1]),
        calibration=V.calibration_table(y[te], p_test),
        rounds=best_rounds(fitted),
        importance=[[X.columns[i], float(gain[i] / max(gain.sum(), 1e-12))] for i in order])
    joblib.dump({"model": fitted["model"], "calibrator": fitted["calibrator"]},
                out_dir / f"{bucket}_{label}.joblib")
    return info, fitted, p_test


def _train_label(X, y, split, label, out_dir, bucket, dates):
    """short/3day: early stopping on the validation part, Platt calibration on
    its most recent rows (C.CALIB_RECENT_DAYS; fallback: all of it)."""
    tr, va, te = split["train"], split["val"], split["test"]
    info = _class_check(y, tr, "earliest 70% minus the embargo")
    if info.get("skipped"):
        return info, None, None
    fitted = fit_lgbm(X[tr], y[tr], X[va], y[va])
    if fitted["calibrator"] is not None:
        _calibrate_recent(fitted, info, fitted["model"].predict(X[va], raw_score=True), y[va],
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
    (C.CALIB_RECENT_DAYS; fallback: all of them) of fold models refitted with
    those rounds; final model on the whole train part."""
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
    _calibrate_recent(fitted, info, raw, obs, when)
    return _finish_label(info, fitted, X, y, tr, te, out_dir, bucket, label)


def collapse_check(y_runner, runner_score, y_plain, y_trained, collapse_score, net_ret,
                   dead_on: bool) -> dict:
    """Trading metrics for one test part. "collapses removed" (the gate) is
    always measured on the PLAIN collapse label `y_plain`, whatever label the
    collapse model was trained on. With the dead rule on, the share of the
    trained-on label (collapse OR dead) removed by the same scores is added as
    `collapse_removed_with_dead`: information only, never a gate."""
    out = V.trading_metrics(y_runner, runner_score, y_plain, collapse_score, net_ret)
    if dead_on:
        out["collapse_removed_with_dead"] = V.collapse_removed(y_trained, collapse_score)
    return out


def _walk_forward(rows, Y, y_plain, net_ret, dates, tokens, horizon_days, rounds, dead_on):
    """Refit per week with the main model's round count; ranking metrics only.
    Category levels are learned from each window's training rows. Collapses
    removed are measured on the plain label `y_plain` (see collapse_check)."""
    out = []
    for w in V.walk_forward_windows(dates, tokens, horizon_days):
        tr, te = w["train"], w["test"]
        row = {"week": w["week"], "start": w["start"].strftime("%Y-%m-%d"),
               "n_train": int(tr.sum()), "n_test": int(te.sum())}
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
                                      Y["collapse"][te], scores.get("collapse"), net_ret[te],
                                      dead_on))
            row["runner_auc"] = V.model_metrics(Y["runner"][te], scores["runner"])["roc_auc"]
        out.append(row)
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


def _train_bucket(b, df, L, out_dir, dead_on=False):
    """Returns (results, runner reference quantiles or None, category levels or None)."""
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
    res["labels"], scores, rounds = {}, {}, {}
    for lab in C.LABELS:
        if forward:
            out = _train_label_kfold(X, Y[lab], split, folds, lab, out_dir, b, dates)
        else:
            out = _train_label(X, Y[lab], split, lab, out_dir, b, dates)
        res["labels"][lab], fitted, scores[lab] = out
        if fitted is not None:
            rounds[lab] = res["labels"][lab]["rounds"]
    if "runner" not in rounds:
        res["skipped"] = res["labels"]["runner"]["skipped"]
        return res, None, None
    # gate on the plain collapse label; collapse OR dead is information only
    y_plain = L[f"collapse_plain_{b}"][use].to_numpy()
    res["trading"] = collapse_check(Y["runner"][te], scores["runner"], y_plain[te],
                                    Y["collapse"][te], scores["collapse"], net_ret[te], dead_on)
    res["dead"] = _dead_stats(L, b, use, te)
    res["walk_forward"] = _walk_forward(rows, Y, y_plain, net_ret, dates, tokens, h, rounds,
                                        dead_on)
    res["gates"] = V.gates(res["trading"], res["walk_forward"])
    reference = np.quantile(scores["runner"], np.linspace(0, 1, C.N_REF_QUANTILES))
    return res, reference.tolist(), levels


def train(df: pd.DataFrame, out_root, version: str | None = None) -> Path:
    """Run the whole pipeline on raw view rows; returns the version directory."""
    version = version or datetime.now(timezone.utc).strftime("%Y%m%d-%H%M")
    out_dir = Path(out_root) / version
    out_dir.mkdir(parents=True, exist_ok=True)
    df = df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    policy = dead_policy(df)
    if policy == "missing":
        print(f"WARNING: the data has no {C.DEAD_COLUMN} column (the view on the server predates "
              "it; apply scoutanalytics.sql, see RUNBOOK.md): the dead-after-the-call rule is "
              "SKIPPED, collapse labels are the plain ones", flush=True)
    L = build_labels(df, policy)
    call = ~L["update"]  # update posts are not calls: out of training, counted on their own
    first = call & ~L["repeat"]  # later calls of a token: out of training, counted on their own
    extreme = first & ~L["no_pool"] & ~L["not_usd"] & L["extreme"]
    eligible = first & ~L["no_pool"] & ~L["not_usd"] & ~L["extreme"]

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
        "trades_24h_known": int((eligible & num(df, C.DEAD_COLUMN).notna()).sum()),
        "dead": int((eligible & L["dead"]).sum()),
        # over the rows training can use (repeat calls and update posts have no pool data)
        "coverage": {c: float(blank[c][eligible].notna().mean()) if eligible.any() else 0.0
                     for c in raw_cols}}}
    reference, cat_levels = {}, {}  # per trained bucket: learned from its training rows
    for b in C.BUCKETS:
        res["buckets"][b], ref, levels = _train_bucket(b, df, L, out_dir, policy == "on")
        if ref is not None:
            reference[b], cat_levels[b] = ref, levels
        status = res["buckets"][b]
        print(f"[{b}] " + (f"skipped: {status['skipped']}" if status.get("skipped") else
                           "PASS" if status["gates"]["passed"] else "FAIL (see report.md)"))

    models = sorted(p.stem for p in out_dir.glob("*.joblib") if p.stem.split("_")[0] in reference)
    for p in out_dir.glob("*.joblib"):  # a collapse model without its runner model is unusable
        if p.stem not in models:
            p.unlink()
    meta = {"version": version, "trained_at": datetime.now(timezone.utc).isoformat(),
            "features": list(C.FEATURES), "categorical": C.CATEGORICAL,
            "category_levels": cat_levels, "thresholds": {"buckets": C.BUCKETS, "gates": C.GATES},
            "dead": {"policy": res["data"]["dead_policy"], "column": C.DEAD_COLUMN,
                     "max_trades_24h": C.DEAD_TRADES_24H},
            "dex_family_rules": dex_family_rules(),  # serving maps dex -> family with these
            "calib_recent_days": C.CALIB_RECENT_DAYS,
            "models": models, "runner_reference": reference,
            "data": res["data"], "metrics": res["buckets"]}
    (out_dir / "meta.json").write_text(json.dumps(_json_safe(meta), indent=1, allow_nan=False))
    (out_dir / "report.md").write_text(render(res), encoding="utf-8")
    (Path(out_root) / "LATEST").write_text(version + "\n")
    return out_dir


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--csv", help="file written by the Go program's -export-dataset")
    src.add_argument("--dsn", help="Postgres DSN; reads scout_call_dataset_v (needs psycopg)")
    ap.add_argument("--out", default="models", help="output root (default: models)")
    args = ap.parse_args(argv)
    df = pd.read_csv(args.csv, low_memory=False) if args.csv else read_dsn(args.dsn)
    out_dir = train(df, args.out)
    print(f"wrote {out_dir} (report: {out_dir / 'report.md'})")
    return 0  # gate failures are reported, not errors


if __name__ == "__main__":
    sys.exit(main())
