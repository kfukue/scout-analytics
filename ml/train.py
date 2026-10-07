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
from scout_ml.features import build_features, learn_cat_levels
from scout_ml.labels import build_labels
from scout_ml.model import fit_baseline, fit_lgbm, predict
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


def _train_label(X, y, split, label, out_dir, bucket):
    """Fit baseline + LightGBM for one label; returns (info, fitted, test scores)."""
    tr, va, te = split["train"], split["val"], split["test"]
    pos, neg = int(y[tr].sum()), int((1 - y[tr]).sum())
    info = {"positive_rate": float(y.mean()), "train_pos": pos, "train_neg": neg, "messages": []}
    if not 0.10 <= info["positive_rate"] <= 0.90:
        info["messages"].append(f"WARNING: positive rate {info['positive_rate']:.1%} is outside 10-90%")
    if min(pos, neg) < C.MIN_CLASS_ROWS:
        info["skipped"] = (f"model skipped: training part (earliest 70% minus the embargo) has "
                           f"{pos} positives / {neg} negatives (need {C.MIN_CLASS_ROWS} of each)")
        info["messages"].append(info["skipped"])
        return info, None, None
    fitted = fit_lgbm(X[tr], y[tr], X[va], y[va])
    if fitted["note"]:
        info["messages"].append(fitted["note"])
    p_test = predict(fitted, X[te])
    gain = fitted["model"].booster_.feature_importance("gain")
    order = np.argsort(-gain)[:10]
    info.update(
        lightgbm=V.model_metrics(y[te], p_test),
        logistic=V.model_metrics(y[te], fit_baseline(X[tr], y[tr]).predict_proba(
            X[te][C.NUMERIC_FEATURES])[:, 1]),
        calibration=V.calibration_table(y[te], p_test),
        rounds=int(fitted["model"].best_iteration_ or fitted["model"].n_estimators),
        importance=[[X.columns[i], float(gain[i] / max(gain.sum(), 1e-12))] for i in order])
    joblib.dump({"model": fitted["model"], "calibrator": fitted["calibrator"]},
                out_dir / f"{bucket}_{label}.joblib")
    return info, fitted, p_test


def _walk_forward(X, Y, net_ret, dates, tokens, horizon_days, rounds):
    """Refit per week with the main model's round count; ranking metrics only."""
    rows = []
    for w in V.walk_forward_windows(dates, tokens, horizon_days):
        tr, te = w["train"], w["test"]
        row = {"week": w["week"], "start": w["start"].strftime("%Y-%m-%d"),
               "n_train": int(tr.sum()), "n_test": int(te.sum())}
        scores = {}
        for label, n_rounds in rounds.items():
            y = Y[label]
            if min(y[tr].sum(), (1 - y[tr]).sum()) >= C.MIN_CLASS_ROWS and V.window_ok(te.sum()):
                scores[label] = predict(fit_lgbm(X[tr], y[tr], n_estimators=n_rounds), X[te])
        if "runner" not in scores:
            row["skipped"] = "too few training rows per class or test rows"
        else:
            row.update(V.trading_metrics(Y["runner"][te], scores["runner"], Y["collapse"][te],
                                         scores.get("collapse"), net_ret[te]))
            row["runner_auc"] = V.model_metrics(Y["runner"][te], scores["runner"])["roc_auc"]
        rows.append(row)
    return rows


def _train_bucket(b, df, X_all, L, out_dir):
    cfg = C.BUCKETS[b]
    use = L[f"usable_{b}"].to_numpy()
    res = {"usable": int(use.sum())}
    if res["usable"] < C.MIN_BUCKET_ROWS:
        res["skipped"] = f"only {res['usable']} usable rows (need {C.MIN_BUCKET_ROWS})"
        return res, None
    X = X_all[use].reset_index(drop=True)
    dates = df["message_date"][use].reset_index(drop=True)
    tokens = df["contract_address"][use].reset_index(drop=True)
    net_ret = L[f"net_ret_{b}"][use].to_numpy()
    Y = {lab: L[f"{lab}_{b}"][use].to_numpy() for lab in C.LABELS}
    split = V.time_split(dates, tokens, cfg["horizon_days"])
    kept = sum(int(split[k].sum()) for k in ("train", "val", "test"))
    res["split"] = {"n_train": int(split["train"].sum()), "n_val": int(split["val"].sum()),
                    "n_test": int(split["test"].sum()), "n_dropped": res["usable"] - kept,
                    "t1": split["t1"].strftime("%Y-%m-%d %H:%M"),
                    "t2": split["t2"].strftime("%Y-%m-%d %H:%M")}
    if split["test"].sum() < C.MIN_CLASS_ROWS:
        res["skipped"] = f"only {int(split['test'].sum())} test rows after the time split"
        return res, None
    res["labels"], scores, rounds = {}, {}, {}
    for lab in C.LABELS:
        res["labels"][lab], fitted, scores[lab] = _train_label(X, Y[lab], split, lab, out_dir, b)
        if fitted is not None:
            rounds[lab] = res["labels"][lab]["rounds"]
    if "runner" not in rounds:
        res["skipped"] = res["labels"]["runner"]["skipped"]
        return res, None
    te = split["test"]
    res["trading"] = V.trading_metrics(Y["runner"][te], scores["runner"], Y["collapse"][te],
                                       scores["collapse"], net_ret[te])
    res["walk_forward"] = _walk_forward(X, Y, net_ret, dates, tokens, cfg["horizon_days"], rounds)
    res["gates"] = V.gates(res["trading"], res["walk_forward"])
    reference = np.quantile(scores["runner"], np.linspace(0, 1, C.N_REF_QUANTILES))
    return res, reference.tolist()


def train(df: pd.DataFrame, out_root, version: str | None = None) -> Path:
    """Run the whole pipeline on raw view rows; returns the version directory."""
    version = version or datetime.now(timezone.utc).strftime("%Y%m%d-%H%M")
    out_dir = Path(out_root) / version
    out_dir.mkdir(parents=True, exist_ok=True)
    df = df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    L = build_labels(df)
    call = ~L["update"]  # update posts are not calls: out of training, counted on their own
    first = call & ~L["repeat"]  # later calls of a token: out of training, counted on their own
    extreme = first & ~L["no_pool"] & ~L["not_usd"] & L["extreme"]
    eligible = first & ~L["no_pool"] & ~L["not_usd"] & ~L["extreme"]
    cat_levels = learn_cat_levels(df[eligible])
    X_all = build_features(df, cat_levels)

    raw_cols = C.NUMERIC_RAW + C.CATEGORICAL + ["pre_vol_unit", "message_date"]
    blank = df.reindex(columns=raw_cols).replace("", np.nan)
    res = {"version": version, "buckets": {}, "data": {
        "rows": len(df), "first_date": df["message_date"].min().strftime("%Y-%m-%d"),
        "last_date": df["message_date"].max().strftime("%Y-%m-%d"),
        "update": int(L["update"].sum()),
        "repeat": int((call & L["repeat"]).sum()), "no_pool": int((first & L["no_pool"]).sum()),
        "not_usd": int((first & ~L["no_pool"] & L["not_usd"]).sum()),
        "extreme": int(extreme.sum()),
        "eligible": int(eligible.sum()),
        # over the rows training can use (repeat calls and update posts have no pool data)
        "coverage": {c: float(blank[c][eligible].notna().mean()) if eligible.any() else 0.0
                     for c in raw_cols}}}
    reference = {}
    for b in C.BUCKETS:
        res["buckets"][b], ref = _train_bucket(b, df, X_all, L, out_dir)
        if ref is not None:
            reference[b] = ref
        status = res["buckets"][b]
        print(f"[{b}] " + (f"skipped: {status['skipped']}" if status.get("skipped") else
                           "PASS" if status["gates"]["passed"] else "FAIL (see report.md)"))

    models = sorted(p.stem for p in out_dir.glob("*.joblib") if p.stem.split("_")[0] in reference)
    for p in out_dir.glob("*.joblib"):  # a collapse model without its runner model is unusable
        if p.stem not in models:
            p.unlink()
    meta = {"version": version, "trained_at": datetime.now(timezone.utc).isoformat(),
            "features": list(X_all.columns), "categorical": C.CATEGORICAL,
            "category_levels": cat_levels, "thresholds": {"buckets": C.BUCKETS, "gates": C.GATES},
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
