import json
import shutil

import pytest
from fastapi.testclient import TestClient

import make_synthetic
import train as train_mod
from scout_ml import config as C

BUCKET_KEYS = ["short", "3day", "medium", "long"]


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
    assert set(meta["category_levels"]) == set(C.CATEGORICAL)
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
    assert meta["data"]["repeat"] > 0 and meta["data"]["update"] > 0
    # coverage is measured on the usable calls: repeats and updates have no pool data
    assert meta["data"]["coverage"]["pre_swaps_60m"] > 0.65
    report = (vdir / "report.md").read_text(encoding="utf-8")
    for needle in ("Feature coverage", "Class balance", "no pool", "Walk-forward",
                   "calibration by decile", "Bucket result:", "gain share", "Money simulation"):
        assert needle in report, needle


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
    assert (vdir / "report.md").read_text(encoding="utf-8").count("**SKIPPED:**") == 4


def test_rare_label_is_skipped_with_message(synthetic_df, tmp_path, monkeypatch):
    """Fewer than 30 positives in training -> that model is skipped, others still train."""
    monkeypatch.setitem(C.BUCKETS["short"], "runner", ("max_gain_late_1d", ">=", 1e9))
    monkeypatch.setitem(C.BUCKETS["3day"], "collapse", ("ret_late_3d", "<=", -100.0))
    monkeypatch.setattr(C, "BUCKETS", {k: C.BUCKETS[k] for k in ("short", "3day")})
    vdir = train_mod.train(synthetic_df, tmp_path, version="t")
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["models"] == ["3day_runner"]
    assert "0 positives" in meta["metrics"]["short"]["skipped"]
    col = meta["metrics"]["3day"]["labels"]["collapse"]
    assert "skipped" in col and any("WARNING: positive rate" in m for m in col["messages"])
    assert meta["metrics"]["3day"]["gates"]["collapse_ok"] is False
    assert "model skipped" in (vdir / "report.md").read_text(encoding="utf-8")
