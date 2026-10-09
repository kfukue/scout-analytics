import json
import shutil

import numpy as np
import pytest
from fastapi.testclient import TestClient

import make_synthetic
import train as train_mod
from scout_ml import config as C

BUCKET_KEYS = ["short", "3day", "medium", "long"]
# The feature list of the models trained before dex_family (e.g. 20261008-2228), pinned.
OLD_CATEGORICAL = ["dex", "launchpad", "quote_asset", "perceptor_verdict"]
OLD_FEATURES = C.NUMERIC_RAW + C.DERIVED + OLD_CATEGORICAL


def test_synthetic_csv_has_exact_view_columns(synthetic_df):
    assert list(synthetic_df.columns) == C.VIEW_COLUMNS
    assert synthetic_df["contract_address"].str.lower().duplicated().mean() > 0.1  # repeat calls
    assert (synthetic_df["tracking_status"] == "repeat").sum() > 100
    assert (synthetic_df["post_kind"] == "update").sum() > 50
    assert {"onchain-pons", "onchain-v2"} <= set(synthetic_df["entry_price_source"].dropna())
    assert "pons-curve" in set(synthetic_df["pool_dex"].dropna())
    assert synthetic_df["holders"].isna().mean() > 0.1                     # missingness
    assert set(synthetic_df["rugged"].dropna().astype(str).str.lower()) == {"true", "false"}


def test_end_to_end_training_artifacts(trained):
    root, vdir = trained
    assert (root / "LATEST").read_text().strip() == vdir.name
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["version"] == vdir.name and meta["features"] == C.FEATURES
    assert "prior_calls" not in meta["features"] and "secs_since_prev_call" not in meta["features"]
    # one set of category levels per trained bucket, learned from its own training rows
    assert set(meta["category_levels"]) == set(BUCKET_KEYS)
    for levels in meta["category_levels"].values():
        assert set(levels) == set(C.CATEGORICAL)
        assert all(lv == lv.strip().lower() for vals in levels.values() for lv in vals)
    assert not [f for f in meta["features"] if C.is_forbidden(f)]
    assert meta["thresholds"]["buckets"]["short"]["runner"] == ["max_gain_late_1d", ">=", 100.0]
    assert meta["data"]["no_pool"] > 0
    for b in BUCKET_KEYS:
        assert (vdir / f"{b}_runner.joblib").exists() and f"{b}_runner" in meta["models"]
        assert len(meta["runner_reference"][b]) == 101
        m = meta["metrics"][b]
        assert {"lift_ok", "collapse_ok", "walk_forward_ok", "passed"} <= set(m["gates"])
        assert len(m["walk_forward"]) >= 5
        for model in ("lightgbm", "logistic"):
            assert {"base_rate", "roc_auc", "pr_auc", "brier"} <= set(m["labels"]["runner"][model])
        assert m["labels"]["runner"]["lightgbm"]["roc_auc"] > 0.55   # planted signal is found
        assert len(m["labels"]["runner"]["calibration"]) == 10
        assert m["split"]["n_dropped_token"] == 0                     # first calls only
        t = m["trading"]
        assert t["top_lift_lo"] <= t["top_lift"] <= t["top_lift_hi"] and t["top_n"] > 0
        if b in C.FORWARD_SPLIT_BUCKETS:
            assert m["split"]["kind"] == "forward" and "n_val" not in m["split"]
            cv = m["labels"]["runner"]["cv"]
            assert cv["folds_used"] >= C.KFOLD_MIN_FOLDS and cv["oof_rows"] > 0
            assert not m["labels"]["runner"]["messages"]               # calibrated, no fallback
        else:
            assert m["split"]["kind"] == "70/15/15" and "cv" not in m["labels"]["runner"]
    assert meta["data"]["repeat"] > 0 and meta["data"]["update"] > 0
    # coverage is measured on the usable calls: repeats and updates have no pool data
    assert meta["data"]["coverage"]["pre_swaps_60m"] > 0.65
    report = (vdir / "report.md").read_text(encoding="utf-8")
    for needle in ("Feature coverage", "Class balance", "no pool", "Walk-forward",
                   "calibration by decile", "Bucket result:", "gain share", "Money simulation",
                   "Lift interval: approximate 95%", "lift 95% interval",
                   "rows dropped by the 1-day embargo", "rows dropped by the 30-day embargo",
                   "Split (forward, no validation part)", "Rounds and calibration (purged k-fold",
                   "applied to the model refitted on the whole train part"):
        assert needle in report, needle
    assert "embargo/token grouping" not in report and "by token grouping" not in report


def test_update_posts_are_excluded_and_counted_on_their_own_line(synthetic_df, tmp_path, monkeypatch):
    n_upd = int((synthetic_df["post_kind"] == "update").sum())
    assert n_upd > 0
    assert (f"- excluded, update post (not a call): {n_upd}"
            in render_report_for(synthetic_df, tmp_path / "a", monkeypatch))
    df = synthetic_df.copy()
    calls = df.index[(df["post_kind"] != "update").to_numpy()]
    df.loc[calls[:50], "post_kind"] = "update"
    df.loc[calls[50:60], "post_kind"] = None  # not classified yet: a call
    vdir = tmp_path / "b" / "v"
    report = render_report_for(df, tmp_path / "b", monkeypatch)
    assert f"- excluded, update post (not a call): {n_upd + 50}" in report
    plain = json.loads((tmp_path / "a" / "v" / "meta.json").read_text())["data"]
    data = json.loads((vdir / "meta.json").read_text())["data"]
    assert data["update"] == n_upd + 50 and data["rows"] == plain["rows"]
    # every row is in exactly one line of the report
    for d in (plain, data):
        assert (d["update"] + d["no_pool"] + d["repeat"] + d["not_usd"] + d["extreme"]
                + d["eligible"] == d["rows"])
    assert data["eligible"] < plain["eligible"]


def test_extreme_outcomes_are_excluded_and_counted(synthetic_df, tmp_path, monkeypatch):
    from scout_ml.labels import build_labels
    df = synthetic_df.copy()
    L = build_labels(df)
    first = ~L["update"] & ~L["repeat"]
    base = int((first & ~L["no_pool"] & ~L["not_usd"] & L["extreme"]).sum())  # the generator makes a few
    idx = df.index[(first & ~L["no_pool"] & ~L["not_usd"] & ~L["extreme"]).to_numpy()]
    # tokens posted once, so that turning a call into an update promotes no later call
    once = df["contract_address"].str.lower().map(df["contract_address"].str.lower().value_counts()) == 1
    single = [i for i in idx if once[i]]
    df.loc[idx[:5], "ret_late_7d"] = 2e5                 # medium label column
    df.loc[idx[5:7], "max_gain_late_1d"] = float("inf")  # short runner column
    df.loc[idx[7:9], "max_gain_7d"] = 1e9                # not label-relevant: kept
    df.loc[idx[9], "ret_late_3d"] = 1e5                  # exactly the cap: kept
    df.loc[single[-3:], ["post_kind", "ret_late_7d"]] = ["update", 2e5]  # counted as update
    report = render_report_for(df, tmp_path, monkeypatch)
    want = base + 7
    line = f"- excluded, extreme outcome (a label/simulation outcome above 100,000 %; bogus pool data): {want}"
    assert line in report, [r for r in report.splitlines() if "extreme" in r]
    data = json.loads((tmp_path / "v" / "meta.json").read_text())["data"]
    n_upd = int((synthetic_df["post_kind"] == "update").sum())
    assert data["extreme"] == want and data["update"] == n_upd + 3, data
    assert data["eligible"] == len(idx) - 10, (data["eligible"], len(idx))
    assert (data["update"] + data["no_pool"] + data["repeat"] + data["not_usd"] + data["extreme"]
            + data["eligible"] == data["rows"])


def render_report_for(df, root, monkeypatch):
    """Run train() with no bucket to train (fast): only the data section matters."""
    monkeypatch.setattr(C, "BUCKETS", {})
    out = train_mod.train(df, root, version="v")
    return (out / "report.md").read_text(encoding="utf-8")


def _client(monkeypatch, model_dir):
    monkeypatch.setenv("SCOUT_MODEL_DIR", str(model_dir))
    import serve
    return TestClient(serve.app)


def _check_shape(body, version, buckets=BUCKET_KEYS):
    assert set(body) == {"model_version", "line", "buckets"}
    assert body["model_version"] == version
    assert [b["bucket"] for b in body["buckets"]] == buckets
    for b in body["buckets"]:
        assert set(b) == {"bucket", "runner_prob", "collapse_prob", "runner_rank_pct"}
        assert 0.0 <= b["runner_prob"] <= 1.0 and 0.0 <= b["runner_rank_pct"] <= 100.0
        assert b["collapse_prob"] is None or 0.0 <= b["collapse_prob"] <= 1.0
    assert body["line"].startswith(f"Model {version} · ")


def test_serve_health_and_score_shape(trained, synthetic_df, monkeypatch):
    root, vdir = trained
    client = _client(monkeypatch, root)
    assert client.get("/health").json() == {"ok": True, "model_version": vdir.name}

    full = json.loads(synthetic_df.iloc[[5]].to_json(orient="records"))[0]   # NaN -> null
    r = client.post("/score", json={"row": full})
    assert r.status_code == 200
    body = r.json()
    _check_shape(body, vdir.name)
    parts = body["line"].split(" · ")
    assert [p.split(":")[0] for p in parts[1:]] == ["short", "3-day", "medium", "long"]
    short = body["buckets"][0]
    assert parts[1] == (f"short: runner {short['runner_prob'] * 100:.0f}%, "
                        f"collapse {short['collapse_prob'] * 100:.0f}%")
    assert parts[4].startswith("long: up at 30d ")

    # Outcomes in the request must not change the score (they are not inputs).
    leaky = {**full, "ret_late_1d": 9999, "max_gain_late_1d": 9999, "rugged": True}
    assert client.post("/score", json={"row": leaky}).json() == body


def test_score_survives_nulls_unknown_keys_and_junk(trained, monkeypatch):
    root, vdir = trained
    client = _client(monkeypatch, root)
    sparse = {"call_id": 1, "message_date": "2026-10-02T15:30:00Z", "mcap_usd": 120000,
              "liq_usd": None, "holders": None, "dex": "a_dex_never_seen", "launchpad": None,
              "tax_buy_pct": "2.5", "perceptor_verdict": None, "pre_vol_unit": None,
              "some_new_column": {"nested": [1, 2]}, "another": "x", "age_seconds": "oops"}
    for row in (sparse, {}, {"message_date": "not a date", "mcap_usd": [1]}):
        r = client.post("/score", json={"row": row})
        assert r.status_code == 200, r.text
        _check_shape(r.json(), vdir.name)
    assert client.post("/score", json={"nope": 1}).status_code == 422


def test_missing_models_are_left_out(trained, tmp_path, monkeypatch):
    """No collapse model -> null + omitted from the line; no runner model -> no bucket."""
    _, vdir = trained
    copy = tmp_path / vdir.name
    shutil.copytree(vdir, copy)
    meta = json.loads((copy / "meta.json").read_text())
    meta["models"] = [m for m in meta["models"] if m not in ("3day_collapse", "long_runner")]
    (copy / "meta.json").write_text(json.dumps(meta))
    (tmp_path / "LATEST").write_text(vdir.name)
    body = _client(monkeypatch, tmp_path).post("/score", json={"row": {"mcap_usd": 5e4}}).json()
    _check_shape(body, vdir.name, ["short", "3day", "medium"])
    assert body["buckets"][1]["collapse_prob"] is None
    part = body["line"].split(" · ")[2]
    assert part.startswith("3-day: runner ") and "collapse" not in part and "long" not in body["line"]


def test_no_model_dir_gives_503_not_a_crash(tmp_path, monkeypatch):
    client = _client(monkeypatch, tmp_path / "missing")
    assert client.get("/health").status_code == 503
    assert client.post("/score", json={"row": {}}).status_code == 503


def test_too_little_data_is_reported_not_raised(tmp_path):
    """200 calls: every bucket is under the 300-row guard; train still exits 0."""
    csv = tmp_path / "tiny.csv"
    make_synthetic.write_csv(make_synthetic.make(n=200, days=70, seed=3), csv)
    assert train_mod.main(["--csv", str(csv), "--out", str(tmp_path / "m")]) == 0
    vdir = tmp_path / "m" / (tmp_path / "m" / "LATEST").read_text().strip()
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["models"] == [] and not list(vdir.glob("*.joblib"))
    assert all("usable rows (need 300)" in m["skipped"] for m in meta["metrics"].values())
    assert meta["category_levels"] == {} and meta["runner_reference"] == {}
    assert (vdir / "report.md").read_text(encoding="utf-8").count("**SKIPPED:**") == 4


def test_collapse_gate_uses_plain_label_when_dead_rule_is_on():
    """The model is trained on collapse OR dead, but collapse_ok is decided on
    the plain collapse label; the with-dead share is information only."""
    from scout_ml import validate as V
    n = 20
    score = np.arange(n, dtype=float)               # the top 30% = rows 14..19
    plain = np.zeros(n)
    plain[[0, 1, 2, 3, 19]] = 1                     # 1 of 5 plain collapses skipped: 0.2
    dead = np.zeros(n)
    dead[14:19] = 1                                 # dead calls are easy: all in the top 30%
    trained_on = np.maximum(plain, dead)            # collapse OR dead: 6 of 10 skipped: 0.6
    y_run = (score >= 18).astype(float)
    ret = np.where(y_run == 1, 100.0, -10.0)
    t = train_mod.collapse_check(y_run, score, plain, trained_on, score, ret, dead_on=True)
    assert t["collapse_removed"] == pytest.approx(0.2)
    assert t["collapse_removed_with_dead"] == pytest.approx(0.6)
    assert t["collapse_removed_with_dead"] >= C.GATES["min_collapse_removed"] > t["collapse_removed"]
    g = V.gates(t, [{"sim_beats_all": True}])
    assert g["collapse_ok"] is False and g["lift_ok"] and not g["passed"]
    # rule off (or trades_24h missing): plain == trained-on label, no extra figure
    off = train_mod.collapse_check(y_run, score, plain, plain, score, ret, dead_on=False)
    assert "collapse_removed_with_dead" not in off
    assert off == V.trading_metrics(y_run, score, plain, score, ret)


def test_rare_label_is_skipped_with_message(synthetic_df, tmp_path, monkeypatch):
    """Fewer than 30 positives in training -> that model is skipped, others still train."""
    monkeypatch.setitem(C.BUCKETS["short"], "runner", ("max_gain_late_1d", ">=", 1e9))
    monkeypatch.setitem(C.BUCKETS["3day"], "collapse", ("ret_late_3d", "<=", -100.0))
    monkeypatch.setattr(C, "DEAD_IS_COLLAPSE", False)   # dead calls would be collapses
    monkeypatch.setattr(C, "BUCKETS", {k: C.BUCKETS[k] for k in ("short", "3day")})
    vdir = train_mod.train(synthetic_df, tmp_path, version="t")
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["models"] == ["3day_runner"]
    assert "0 positives" in meta["metrics"]["short"]["skipped"]
    col = meta["metrics"]["3day"]["labels"]["collapse"]
    assert "skipped" in col and any("WARNING: positive rate" in m for m in col["messages"])
    assert meta["metrics"]["3day"]["gates"]["collapse_ok"] is False
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "model skipped" in report
    # dead rule off: no with-dead figure anywhere, the gate is the plain label as before
    for needle in ("collapse OR dead;", "incl. dead", "model trained on collapse OR dead"):
        assert needle not in report, needle
    assert "the dead rule is off, the model was trained on the plain label" in report
    for b in ("short", "3day"):
        t = meta["metrics"][b].get("trading", {})
        assert "collapse_removed_with_dead" not in t and "collapse_removed_plain" not in t


def test_long_is_skipped_until_its_30d_data_has_matured(synthetic_df, tmp_path, monkeypatch):
    """Too short a span of matured 30d calls: no long model, a clear reason in
    the report, and serving answers without a long line."""
    import pandas as pd
    df = synthetic_df.copy()
    dates = pd.to_datetime(df["message_date"], utc=True)
    df = df[dates >= dates.max() - pd.Timedelta(days=100)]       # ~70 days of matured 30d calls
    monkeypatch.setattr(C, "BUCKETS", {k: C.BUCKETS[k] for k in ("short", "long")})
    vdir = train_mod.train(df, tmp_path, version="v")
    meta = json.loads((vdir / "meta.json").read_text())
    skipped = meta["metrics"]["long"]["skipped"]
    assert skipped.startswith("not enough matured 30d data (") and "need 2000 rows over at least 120 days" in skipped
    assert not list(vdir.glob("long_*.joblib")) and not [m for m in meta["models"] if m.startswith("long")]
    assert "long" not in meta["runner_reference"] and "long" not in meta["category_levels"]
    assert meta["models"] == ["short_collapse", "short_runner"]
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert f"**SKIPPED:** {skipped}" in report and "| long | skipped |" in report
    monkeypatch.undo()                                       # serving sees every configured bucket
    (tmp_path / "LATEST").write_text("v")
    client = _client(monkeypatch, tmp_path)
    body = client.post("/score", json={"row": json.loads(df.iloc[[3]].to_json(orient="records"))[0]})
    assert body.status_code == 200, body.text
    _check_shape(body.json(), "v", ["short"])
    assert "long" not in body.json()["line"] and "up at 30d" not in body.json()["line"]


def test_maturity_rule_needs_rows_and_span(monkeypatch):
    import pandas as pd
    d = lambda n, days: pd.Series(pd.Timestamp("2026-08-01", tz="UTC")
                                  + pd.to_timedelta(np.linspace(0, days, n), unit="D"))
    assert train_mod.maturity_skip("long", d(2000, 120)) is None
    assert "1999 rows over 120 days" in train_mod.maturity_skip("long", d(1999, 120))
    assert "5000 rows over 119 days" in train_mod.maturity_skip("long", d(5000, 119))
    assert train_mod.maturity_skip("medium", d(10, 1)) is None          # only long has a rule
    monkeypatch.setitem(C.MIN_MATURED, "long", {"rows": 10, "span_days": 1})
    assert train_mod.maturity_skip("long", d(10, 1)) is None            # thresholds from config


def test_levels_rounds_and_calibration_never_see_the_test_part(synthetic_df, tmp_path, monkeypatch):
    """Every category-level fit and every LightGBM fit of the saved models
    (main split, inner k-folds) uses rows posted before the test part; a dex
    seen only in the test period is not a level; case variants are one level.
    Walk-forward windows learn levels from exactly their own training rows."""
    import pandas as pd
    from scout_ml import validate as V
    from scout_ml.features import dex_family_of
    df = synthetic_df.copy()
    when = pd.to_datetime(df["message_date"], utc=True)
    late = (when >= when.max() - pd.Timedelta(days=12)).to_numpy()
    assert not (df["dex"].map(dex_family_of) == "uniswap_v4").any()
    df.loc[late, "dex"] = "Uniswap V4"                               # family in the test period only
    df.loc[df.index[:400], "dex"] = " UNISWAP-v2 "                    # case/space variant
    every_bucket = dict(C.BUCKETS)
    seen = {"levels": [], "fit": [], "wf_levels": [], "wf_windows": []}
    real_learn, real_fit, real_wf, real_windows = (train_mod.learn_cat_levels, train_mod.fit_lgbm,
                                                   train_mod._walk_forward, V.walk_forward_windows)
    in_wf = {"on": False}

    def learn(rows):
        (seen["wf_levels"] if in_wf["on"] else seen["levels"]).append(set(rows.index))
        return real_learn(rows)

    def fit(X_train, y_train, X_val=None, y_val=None, n_estimators=None):
        if not in_wf["on"]:
            seen["fit"].append(set(X_train.index) | (set(X_val.index) if X_val is not None else set()))
        return real_fit(X_train, y_train, X_val, y_val, n_estimators)

    def windows(*a, **k):
        for w in real_windows(*a, **k):
            seen["wf_windows"].append(set(np.flatnonzero(w["train"])))
            yield w

    def wf(*a, **k):
        in_wf["on"] = True
        try:
            return real_wf(*a, **k)
        finally:
            in_wf["on"] = False

    monkeypatch.setattr(train_mod, "learn_cat_levels", learn)
    monkeypatch.setattr(train_mod, "fit_lgbm", fit)
    monkeypatch.setattr(train_mod, "_walk_forward", wf)
    monkeypatch.setattr(V, "walk_forward_windows", windows)
    from scout_ml.labels import build_labels
    sorted_df = df.assign(message_date=when).sort_values("message_date").reset_index(drop=True)
    L = build_labels(sorted_df)
    for b in ("short", "medium"):
        for k in seen:
            seen[k].clear()
        monkeypatch.setattr(C, "BUCKETS", {b: every_bucket[b]})
        vdir = train_mod.train(df, tmp_path / b, version="v")
        meta = json.loads((vdir / "meta.json").read_text())
        rows = sorted_df[L[f"usable_{b}"].to_numpy()].reset_index(drop=True)
        h = C.BUCKETS[b]["horizon_days"]
        if b == "medium":
            split = V.forward_split(rows["message_date"], rows["contract_address"], h)
            before = split["t_test"]
        else:
            split = V.time_split(rows["message_date"], rows["contract_address"], h)
            before = split["t2"]
        test_rows = set(np.flatnonzero(split["test"]))
        assert seen["levels"] and seen["fit"]
        for idx in seen["levels"] + seen["fit"]:
            assert not idx & test_rows, b
            assert rows["message_date"][sorted(idx)].max() < before, b
        assert seen["levels"][0] == set(np.flatnonzero(split["train"]))  # main levels = train part
        if b == "medium":                                   # + one per inner fold: its fit rows only
            assert len(seen["levels"]) == 1 + C.KFOLD_K
        for idx in seen["wf_levels"]:
            assert idx in seen["wf_windows"], b
        assert seen["wf_levels"], b
        levels = meta["category_levels"][b]["dex_family"]
        assert "uniswap_v4" not in levels and "uniswap_v2" in levels
        assert len(levels) == len(set(levels))


@pytest.mark.parametrize("bucket", ["short", "medium"])
def test_serving_scores_match_training_scores(trained, synthetic_df, bucket):
    """No train/serve skew: the service bundle (its per-bucket category levels
    and models) reproduces the test-score quantiles stored at training, and
    /score's one-JSON-row path gives the same probabilities."""
    import pandas as pd
    from scout_ml import validate as V
    from scout_ml.labels import build_labels
    from scout_ml.features import build_features
    from scout_ml.model import Bundle, predict
    _, vdir = trained
    meta = json.loads((vdir / "meta.json").read_text())
    df = synthetic_df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    rows = df[build_labels(df)[f"usable_{bucket}"].to_numpy()].reset_index(drop=True)
    h = C.BUCKETS[bucket]["horizon_days"]
    split = (V.forward_split if bucket in C.FORWARD_SPLIT_BUCKETS else V.time_split)(
        rows["message_date"], rows["contract_address"], h)
    test = rows[split["test"]].assign(message_date=lambda d: d["message_date"].dt.strftime(
        "%Y-%m-%dT%H:%M:%SZ"))
    bundle = Bundle(vdir)
    X = build_features(test, bundle.levels(bucket), meta["features"])
    probs = predict(bundle.models[f"{bucket}_runner"], X)
    ref = np.quantile(probs, np.linspace(0, 1, C.N_REF_QUANTILES))
    np.testing.assert_allclose(ref, meta["runner_reference"][bucket], rtol=1e-9, atol=1e-12)
    for i, rec in enumerate(json.loads(test.to_json(orient="records"))[:25]):
        body = bundle.score(rec)
        got = next(b["runner_prob"] for b in body["buckets"] if b["bucket"] == bucket)
        assert got == round(float(probs[i]), 4), i


def test_kfold_fallback_when_too_few_folds_are_usable(synthetic_df, tmp_path, monkeypatch):
    """Fewer usable purged folds than KFOLD_MIN_FOLDS: fixed rounds, no calibrator,
    a note, and the report says the model is uncalibrated."""
    import joblib
    monkeypatch.setattr(C, "KFOLD_MIN_FOLDS", 6)                # more than KFOLD_K = 5 folds
    monkeypatch.setattr(C, "BUCKETS", {"medium": C.BUCKETS["medium"]})
    vdir = train_mod.train(synthetic_df, tmp_path, version="v")
    meta = json.loads((vdir / "meta.json").read_text())
    m = meta["metrics"]["medium"]["labels"]["runner"]
    cv = m["cv"]
    assert cv["folds"] == C.KFOLD_K and 0 < cv["folds_used"] < 6
    assert cv["fold_rounds"] == [] and cv["oof_rows"] == 0 and m["rounds"] == C.FALLBACK_ROUNDS
    note = (f"only {cv['folds_used']} of {cv['folds']} purged folds usable (need 6): "
            f"fixed {C.FALLBACK_ROUNDS} rounds, probabilities uncalibrated")
    assert note in m["messages"]
    saved = joblib.load(vdir / "medium_runner.joblib")
    assert saved["calibrator"] is None and saved["model"].n_estimators == C.FALLBACK_ROUNDS
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert note in report
    assert (f"| runner | {cv['folds_used']}/{cv['folds']} | - | {C.FALLBACK_ROUNDS} | "
            "none (uncalibrated) |") in report, [r for r in report.splitlines() if "| runner |" in r]
    assert "applied to the model refitted on the whole train part" not in report  # no fold model


def test_old_flat_meta_still_serves_with_exact_levels(trained, synthetic_df, tmp_path, monkeypatch):
    """A meta.json from before per-bucket levels: one flat set of case-sensitive
    levels, a long_runner model and prior_calls/secs_since_prev_call as features.
    /score answers 200 with the long line, and the models get exactly the codes
    they were trained with ('Raydium' and 'raydium' stay two levels)."""
    import joblib
    import pandas as pd
    from scout_ml.features import build_features
    from scout_ml.labels import build_labels
    from scout_ml.model import Bundle, fit_lgbm, predict
    _, vdir = trained
    old = tmp_path / "old"
    shutil.copytree(vdir, old)
    for p in old.glob("*.joblib"):
        p.unlink()
    meta = json.loads((old / "meta.json").read_text())
    old_cols = OLD_FEATURES + ["prior_calls", "secs_since_prev_call"]
    flat = {c: list(meta["category_levels"]["short"][c]) for c in OLD_CATEGORICAL if c != "dex"}
    flat["dex"] = sorted(set(synthetic_df["dex"].dropna()) | {"Raydium"})  # two levels in another case
    assert "raydium" in flat["dex"] and flat["dex"].index("Raydium") != flat["dex"].index("raydium")
    df = synthetic_df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    ray = df.index[(df["dex"] == "raydium").to_numpy()]
    df.loc[ray[::2], "dex"] = "Raydium"
    L = build_labels(df)
    models = {}
    for b in ("short", "long"):
        use = L[f"usable_{b}"].to_numpy()
        X = build_features(df[use], flat, old_cols, exact_levels=True)
        models[f"{b}_runner"] = fit_lgbm(X, L[f"runner_{b}"][use].to_numpy(), n_estimators=20)
        joblib.dump({"model": models[f"{b}_runner"]["model"], "calibrator": None},
                    old / f"{b}_runner.joblib")
    meta.update(category_levels=flat, features=old_cols, models=sorted(models))
    (old / "meta.json").write_text(json.dumps(meta))
    (tmp_path / "LATEST").write_text("old")

    # the bundle gives each categorical the exact code of the old level (by hand)
    bundle = Bundle(old)
    assert bundle.flat_levels and bundle.levels("long") == flat
    codes = bundle.features([{"dex": v} for v in ("Raydium", "raydium", " Raydium ", "RAYDIUM")],
                            "short")["dex"]
    ix = flat["dex"].index
    assert codes[:3].tolist() == [ix("Raydium"), ix("raydium"), ix("Raydium")]
    assert np.isnan(codes[3])                                     # not an old level

    client = _client(monkeypatch, tmp_path)
    base = json.loads(df.iloc[[7]].assign(message_date="2026-09-01T12:00:00Z")
                      .to_json(orient="records"))[0]
    for dex in ("Raydium", "raydium", "RAYDIUM"):
        rec = {**base, "dex": dex}
        r = client.post("/score", json={"row": rec})
        assert r.status_code == 200, r.text
        body = r.json()
        _check_shape(body, meta["version"], ["short", "long"])
        assert " · long: up at 30d " in body["line"]
        X_ref = build_features(rec, {}, old_cols)                  # numerics; categoricals by hand
        for c in OLD_CATEGORICAL:
            v = rec.get(c)
            v = v.strip() if isinstance(v, str) else v
            X_ref[c] = float(flat[c].index(v)) if v in flat[c] else np.nan
        for b in body["buckets"]:
            want = round(float(predict(models[f"{b['bucket']}_runner"], X_ref)[0]), 4)
            assert b["runner_prob"] == want, (dex, b)


def test_report_shows_dead_calls_and_recent_calibration(trained):
    _, vdir = trained
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead"] == {"policy": "on", "column": "trades_24h", "max_trades_24h": 50}
    assert meta["data"]["dead_policy"] == "on" and meta["data"]["dead"] > 0
    assert 0 < meta["data"]["trades_24h_known"] <= meta["data"]["eligible"]
    assert meta["calib_recent_days"] == C.CALIB_RECENT_DAYS
    for b in BUCKET_KEYS:
        m = meta["metrics"][b]
        dead = m["dead"]
        for part in ("usable", "test"):
            assert 0 <= dead[part]["dead_collapse"] <= dead[part]["dead"] <= dead[part]["rows"]
            assert dead[part]["collapse_rate_with_dead"] >= dead[part]["collapse_rate_plain"]
        assert dead["usable"]["dead"] > dead["usable"]["dead_collapse"]   # some flat dead calls
        assert "collapse_removed_plain" not in m["trading"]
        assert 0 <= m["trading"]["collapse_removed_with_dead"] <= 1        # information only
        assert m["gates"]["collapse_ok"] == (m["trading"]["collapse_removed"]
                                             >= C.GATES["min_collapse_removed"])
        assert any(not w.get("skipped") for w in m["walk_forward"])
        for w in m["walk_forward"]:
            if not w.get("skipped"):
                assert "collapse_removed_with_dead" in w
        for lab in ("runner", "collapse"):
            info = m["labels"][lab]
            assert info["calib"]["rows_recent"] <= info["calib"]["rows_all"]
            before, after = info["calibration_all_rows"], info["calibration"]
            # deciles are by rank: the same rows and outcomes, only the predictions move
            assert [c["observed"] for c in before] == [c["observed"] for c in after]
            assert info["brier_all_rows"] >= 0
    report = (vdir / "report.md").read_text(encoding="utf-8")
    for needle in ("- dead after the call (trades_24h < 50; kept in the data): ",
                   "collapse label = collapse OR dead", "OR dead (trades_24h < 50)",
                   "### Dead after the call (trades_24h < 50)", "dead and already a collapse",
                   "of plain collapses (the gate); bad outcomes removed incl. dead: ",
                   "(information only, not a gate)",
                   "bad outcomes removed incl. dead (collapse OR dead; information, not a gate)",
                   "| incl. dead (info, not a gate) |",
                   "Platt calibration fitted on", "mean predicted (all held-out rows)",
                   "(calibrated on all held-out rows, as before)"):
        assert needle in report, needle


def test_missing_trades_24h_skips_the_dead_rule_loudly(synthetic_df, tmp_path, monkeypatch, capsys):
    """An export from a view without trades_24h (server not migrated yet):
    training still runs, with plain collapse labels, and says so."""
    from scout_ml.labels import build_labels
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    df = synthetic_df.drop(columns="trades_24h")
    vdir = train_mod.train(df, tmp_path, version="v")
    assert "WARNING: the data has no trades_24h column" in capsys.readouterr().out
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead"]["policy"] == "missing" and meta["data"]["dead"] == 0
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "**WARNING: dead-after-the-call rule SKIPPED" in report
    assert "OR dead" not in report and "### Dead after the call" not in report
    assert "incl. dead" not in report                     # no information figure either
    t = meta["metrics"]["short"]["trading"]
    assert "collapse_removed_with_dead" not in t and "collapse_removed_plain" not in t
    assert all("collapse_removed_with_dead" not in w for w in meta["metrics"]["short"]["walk_forward"])
    L = build_labels(df)
    use = L["usable_short"]
    assert (L["collapse_short"][use] == L["collapse_plain_short"][use]).all()
    assert m_rate(meta) == pytest.approx(float(L["collapse_plain_short"][use].mean()))


def m_rate(meta):
    return meta["metrics"]["short"]["labels"]["collapse"]["positive_rate"]


def test_current_prod_meta_with_raw_dex_still_scores(trained, synthetic_df, tmp_path, monkeypatch):
    """A model trained before dex_family (per-bucket, case-insensitive levels with
    raw `dex` in its feature list, no dex_family) is served as trained: `dex` is
    encoded as a category (not read as a number), case-insensitively."""
    import joblib
    import pandas as pd
    from scout_ml.features import build_features, learn_cat_levels
    from scout_ml.labels import build_labels
    from scout_ml.model import Bundle, fit_lgbm, predict
    _, vdir = trained
    old = tmp_path / "prod"
    shutil.copytree(vdir, old)
    for p in old.glob("*.joblib"):
        p.unlink()
    meta = json.loads((old / "meta.json").read_text())
    df = synthetic_df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    df = df.sort_values("message_date").reset_index(drop=True)
    L = build_labels(df)
    use = L["usable_short"].to_numpy()
    learned = learn_cat_levels(df[use])
    levels = {c: learned[c] for c in OLD_CATEGORICAL if c != "dex"}
    levels["dex"] = sorted(df["dex"][use].str.lower().value_counts().loc[lambda s: s >= 20].index)
    assert "raydium" in levels["dex"]
    X = build_features(df[use], levels, OLD_FEATURES)
    assert X["dex"].notna().mean() > 0.9                         # codes, not NaN from num()
    fitted = fit_lgbm(X, L["runner_short"][use].to_numpy(), n_estimators=20)
    joblib.dump({"model": fitted["model"], "calibrator": None}, old / "short_runner.joblib")
    meta.update(features=OLD_FEATURES, categorical=OLD_CATEGORICAL, models=["short_runner"],
                category_levels={"short": levels})
    for k in ("dead", "dex_family_rules", "calib_recent_days"):   # keys the old meta did not have
        meta.pop(k, None)
    (old / "meta.json").write_text(json.dumps(meta))

    bundle = Bundle(old)
    assert not bundle.flat_levels
    codes = bundle.features([{"dex": v} for v in ("Raydium", " RAYDIUM ", "raydium", "Uniswap V4")],
                            "short")
    assert list(codes.columns) == OLD_FEATURES and "dex_family" not in codes.columns
    ray = float(levels["dex"].index("raydium"))
    assert codes["dex"][:3].tolist() == [ray] * 3 and np.isnan(codes["dex"][3])
    (tmp_path / "LATEST").write_text("prod")
    rec = json.loads(df.iloc[[7]].assign(message_date="2026-09-01T12:00:00Z", dex="Raydium")
                     .to_json(orient="records"))[0]
    r = _client(monkeypatch, tmp_path).post("/score", json={"row": rec})
    assert r.status_code == 200, r.text
    body = r.json()
    _check_shape(body, meta["version"], ["short"])
    want = round(float(predict(fitted, build_features(rec, levels, OLD_FEATURES))[0]), 4)
    assert body["buckets"][0]["runner_prob"] == want


def test_serving_uses_the_dex_family_rules_stored_at_training(trained, synthetic_df, monkeypatch):
    """Editing DEX_FAMILY_RULES after training must not change what a served
    model sees: the bundle maps dex -> dex_family with the rules in its meta.json."""
    from scout_ml.features import build_features
    from scout_ml.model import Bundle
    _, vdir = trained
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dex_family_rules"] == {"rules": [list(r) for r in C.DEX_FAMILY_RULES],
                                        "other": C.DEX_FAMILY_OTHER}
    bundle = Bundle(vdir)
    rec = json.loads(synthetic_df.iloc[[5]].assign(dex="Uniswap_V2").to_json(orient="records"))[0]
    before = bundle.score(rec)
    code = bundle.features(rec, "short")["dex_family"][0]
    assert not np.isnan(code)                                  # uniswap_v2: a trained level
    monkeypatch.setattr(C, "DEX_FAMILY_RULES", (("uniswap", "uniswap_any"),))
    assert bundle.features(rec, "short")["dex_family"][0] == code
    assert bundle.score(rec) == before
    # without the stored rules the edited table would have changed the input
    assert np.isnan(build_features(rec, bundle.levels("short"))["dex_family"][0])
