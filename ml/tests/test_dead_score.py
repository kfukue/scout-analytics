"""The separate "dead after the call" score: report only, never saved or served,
never fails the run. Synthetic data only."""
import json

import numpy as np
import pandas as pd
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml.labels import build_labels
from scout_ml.model import Bundle


@pytest.fixture(scope="module")
def dead_df():
    df = make_synthetic.make(n=4000, days=80, seed=21)
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    return df.sort_values("message_date").reset_index(drop=True)


@pytest.fixture(scope="module")
def scored(dead_df):
    return train_mod.dead_score(dead_df, build_labels(dead_df))


@pytest.fixture
def short_only(monkeypatch):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})


def test_dead_label_leaves_null_trades_out_and_uses_the_threshold(dead_df, scored):
    L = build_labels(dead_df)
    use = L["usable_short"]
    trades = pd.to_numeric(dead_df.loc[use, "trades_24h"], errors="coerce")
    assert trades.isna().sum() > 0                                # synthetic data has NULLs
    assert scored["known"] == int(trades.notna().sum())
    assert scored["null_excluded"] == int(trades.isna().sum())    # left out, not negatives
    assert scored["usable"] == int(use.sum())
    parts = scored["parts"]
    assert sum(p["n"] for p in parts.values()) <= scored["known"]  # minus the embargo
    # every dead row of a part has a known trades_24h < 50: no NULL counted as dead or alive
    assert sum(p["dead"] for p in parts.values()) <= int((trades < C.DEAD_TRADES_24H).sum())
    assert sum(w["calls"] for w in scored["waves"]) == scored["known"]
    assert sum(w["dead"] for w in scored["waves"]) == int((trades < C.DEAD_TRADES_24H).sum())
    for p in parts.values():
        assert p["base_rate"] == pytest.approx(p["dead"] / p["n"])


def test_dead_label_null_rows_never_reach_the_model(dead_df, monkeypatch):
    """The model gets 0/1 labels for the known rows only; filling the NULLs in
    adds exactly those rows (so they were left out, not counted as negatives)."""
    seen = []
    real = train_mod._train_label
    monkeypatch.setattr(train_mod, "_train_label",
                        lambda X, y, split, *a: seen.append((len(y), set(np.unique(y))))
                        or real(X, y, split, *a))
    df = dead_df.copy()
    df["trades_24h"] = pd.to_numeric(df["trades_24h"], errors="coerce")
    L = build_labels(df)
    a = train_mod.dead_score(df, L)
    assert seen[-1] == (a["known"], {0.0, 1.0})                  # labels are 0/1, no NaN
    filled = df.assign(trades_24h=df["trades_24h"].fillna(10_000))
    b = train_mod.dead_score(filled, build_labels(filled))
    assert b["known"] == a["known"] + a["null_excluded"] and b["null_excluded"] == 0


def test_dead_score_levels_and_fits_never_see_its_test_part(dead_df, scored, monkeypatch):
    """Same split as the short bucket: category levels from train rows only, the
    main LightGBM fit on train (+ validation for early stopping / calibration)."""
    from scout_ml import validate as V
    seen = {"levels": [], "fit": []}
    real_learn, real_fit, real_wf = (train_mod.learn_cat_levels, train_mod.fit_lgbm,
                                     train_mod._dead_walk_forward)
    monkeypatch.setattr(train_mod, "learn_cat_levels",
                        lambda rows: seen["levels"].append(rows["message_date"].max())
                        or real_learn(rows))
    monkeypatch.setattr(train_mod, "fit_lgbm", lambda Xt, yt, Xv=None, yv=None, n_estimators=None:
                        seen["fit"].append((len(Xt), None if Xv is None else len(Xv)))
                        or real_fit(Xt, yt, Xv, yv, n_estimators))
    monkeypatch.setattr(train_mod, "_dead_walk_forward", lambda *a: [])
    monkeypatch.setattr(train_mod, "dead_score_proposal", lambda res: {})
    train_mod.dead_score(dead_df, build_labels(dead_df))
    L = build_labels(dead_df)
    rows = dead_df[L["usable_short"].to_numpy()].reset_index(drop=True)
    s = V.time_split(rows["message_date"], rows["contract_address"], 1)
    assert scored["split"]["t1"] == s["t1"].strftime("%Y-%m-%d %H:%M")  # the short bucket's split
    assert scored["split"]["t2"] == s["t2"].strftime("%Y-%m-%d %H:%M")
    assert seen["levels"] and all(d < s["t1"] - pd.Timedelta(days=1) for d in seen["levels"])
    assert seen["fit"] == [(scored["parts"]["train"]["n"], scored["parts"]["val"]["n"])]
    assert callable(real_wf)


def test_logistic_coefficient_names_follow_the_pipeline_order(dead_df):
    from scout_ml.model import fit_baseline
    from scout_ml.features import build_features, learn_cat_levels
    X = build_features(dead_df, learn_cat_levels(dead_df))
    y = (np.arange(len(X)) % 7 == 0).astype(float)
    pipe = fit_baseline(X, y)
    # the fitted ColumnTransformer's own column order (FunctionTransformer has no
    # get_feature_names_out, so read the column lists of its transformers)
    names = [c for name, _, cols in pipe[0].transformers_ if name != "remainder" for c in cols]
    assert len(names) == len(pipe[-1].coef_.ravel())
    coef = dict(zip(names, pipe[-1].coef_.ravel()))
    got = train_mod._logistic_coefs(pipe)
    assert len(got) == C.DEAD_SCORE_N_COEF
    assert all(coef[n] == c for n, c, _ in got)
    assert all(s == ("log1p" if n in C.LOG1P_FEATURES else "as is") for n, _, s in got)


def test_dead_score_never_uses_trades_24h(dead_df, scored):
    assert scored["features"] == list(C.FEATURES) and "trades_24h" not in scored["features"]
    names = [f for f, _ in scored["label"]["importance"]] + [c[0] for c in scored["logistic_coefs"]]
    assert not any(n.startswith("trades_") for n in names)
    # rewrite every known trades_24h, keeping each row's dead / not dead label: identical score
    t = pd.to_numeric(dead_df["trades_24h"], errors="coerce")
    other = dead_df.assign(trades_24h=np.where(t.isna(), np.nan,
                                               np.where(t < C.DEAD_TRADES_24H, 0, 10_000)))
    again = train_mod.dead_score(other, build_labels(other))
    for k in ("parts", "label", "top", "logistic_coefs", "walk_forward", "proposal"):
        assert json.dumps(train_mod._json_safe(again[k])) == json.dumps(train_mod._json_safe(scored[k])), k


def test_dead_score_results_have_the_expected_fields(scored):
    assert set(scored["parts"]) == {"train", "val", "test"}
    for model in ("lightgbm", "logistic"):
        m = scored["label"][model]
        assert m["n"] == scored["parts"]["test"]["n"] and 0 <= m["roc_auc"] <= 1
        assert m["pr_auc"] >= 0 and m["brier"] >= 0
        tops = scored["top"][model]
        assert [t["frac"] for t in tops] == list(C.DEAD_SCORE_TOP_FRACS)
        for t in tops:
            assert t["precision_lo"] <= t["precision"] <= t["precision_hi"]
            assert t["lift"] == pytest.approx(t["precision"] / m["base_rate"])
            assert 0 <= t["caught"] <= 1
        assert tops[0]["caught"] <= tops[1]["caught"]
    assert len(scored["label"]["calibration"]) == 10 and scored["label"]["calib"]
    assert len(scored["logistic_coefs"]) == C.DEAD_SCORE_N_COEF
    coefs = [abs(c) for _, c, _ in scored["logistic_coefs"]]
    assert coefs == sorted(coefs, reverse=True)
    weeks = scored["walk_forward"]
    assert any(not w.get("skipped") for w in weeks)
    for w in weeks:
        assert w["few_dead"] == (w["n_dead"] < C.DEAD_SCORE_MIN_WEEK_DEAD)
    p = scored["proposal"]
    assert p["min_lift"] == C.DEAD_SCORE_PROPOSED_LIFT
    for model in ("lightgbm", "logistic"):
        assert p[model]["weeks_judged"] == sum(1 for w in weeks
                                               if not w.get("skipped") and not w["few_dead"])


def _models_and_meta(vdir):
    meta = json.loads((vdir / "meta.json").read_text())
    return meta, Bundle(vdir)


def test_report_section_and_saved_model_unchanged_by_the_dead_score(dead_df, short_only,
                                                                    tmp_path, monkeypatch):
    dumped = []
    real_dump = train_mod.joblib.dump
    monkeypatch.setattr(train_mod.joblib, "dump", lambda obj, path, *a, **k: (
        dumped.append(str(path)), real_dump(obj, path, *a, **k)))
    with_ds = train_mod.train(dead_df, tmp_path / "a", "v1")
    n_saved = len(dumped)
    monkeypatch.setattr(train_mod, "dead_score", lambda df, L: {"not_applicable": "switched off"})
    without = train_mod.train(dead_df, tmp_path / "b", "v1")
    assert len(dumped) == 2 * n_saved                         # the dead score saves nothing
    ma, ba = _models_and_meta(with_ds)
    mb, bb = _models_and_meta(without)
    assert sorted(p.name for p in with_ds.iterdir()) == sorted(p.name for p in without.iterdir())
    assert not any("dead" in p.name for p in with_ds.glob("*.joblib"))
    assert ma["models"] == mb["models"] == ["short_collapse", "short_runner"]
    for k in ma:
        if k not in ("trained_at", "dead_score"):
            assert ma[k] == mb[k], k
    rows = json.loads(dead_df.head(50).to_json(orient="records", date_format="iso"))
    for name in ma["models"]:
        np.testing.assert_array_equal(
            ba.models[name]["model"].predict(ba.features(rows, "short"), raw_score=True),
            bb.models[name]["model"].predict(bb.features(rows, "short"), raw_score=True))
    ds = ma["dead_score"]
    for k in ("bucket", "known", "null_excluded", "parts", "label", "top", "logistic_coefs",
              "walk_forward", "waves", "proposal"):
        assert k in ds, k
    report = (with_ds / "report.md").read_text(encoding="utf-8")
    for needle in ("## Dead after the call: separate score (report only; not saved, not served)",
                   "| part | calls | dead | base rate |",
                   "| model | n | base rate | ROC AUC | PR AUC | Brier |",
                   "precision [95% Wilson]", "share of all dead calls caught",
                   "### Calibration by decile (test, LightGBM)", "| feature (LightGBM) | gain share |",
                   "| feature (logistic) | coefficient | input |",
                   "dead caught, skipping top 10% / 30% (LightGBM)",
                   "Dead calls by week: the 2 weeks with the most hold",
                   "NOT applied", "with NULL trades_24h left out, not counted as negatives"):
        assert needle in report, needle
    if any(w["n_dead"] < C.DEAD_SCORE_MIN_WEEK_DEAD for w in ds["walk_forward"]):
        assert "dead: too few to judge" in report
    assert "n/a: switched off" in (without / "report.md").read_text(encoding="utf-8")


def test_missing_trades_24h_dead_score_says_na(dead_df, short_only, tmp_path, capsys):
    vdir = train_mod.train(dead_df.drop(columns="trades_24h"), tmp_path, "v")
    out = capsys.readouterr().out
    assert "the dead-after-the-call table and the separate dead score are n/a" in out
    assert "rule is SKIPPED" not in out                       # the rule is off: nothing skipped
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead"]["policy"] == "off" and meta["data"]["trades_24h_present"] is False
    assert meta["dead_score"] == {"not_applicable": "the data has no trades_24h column"}
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "- dead after the call: n/a, the data has no trades_24h column" in report
    assert "n/a: the data has no trades_24h column" in report
    assert "### Dead after the call" not in report and "incl. dead" not in report
    assert "rule SKIPPED" not in report
    assert "collapse_removed_with_dead" not in meta["metrics"]["short"]["trading"]


def test_rule_on_and_missing_trades_24h_warns_the_dead_score_is_na(dead_df, short_only, tmp_path,
                                                                  monkeypatch, capsys):
    monkeypatch.setattr(C, "DEAD_IS_COLLAPSE", True)
    vdir = train_mod.train(dead_df.drop(columns="trades_24h"), tmp_path, "v")
    out = capsys.readouterr().out
    assert "the dead-after-the-call rule is SKIPPED" in out
    assert "and the separate dead score is n/a" in out
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead_score"] == {"not_applicable": "the data has no trades_24h column"}


def test_a_crashing_dead_score_never_fails_the_run(dead_df, short_only, tmp_path, monkeypatch,
                                                   capsys):
    def boom(df, L):
        raise RuntimeError("boom | second part\nTraceback line that must not appear")

    monkeypatch.setattr(train_mod, "dead_score", boom)
    csv = tmp_path / "calls.csv"
    make_synthetic.write_csv(dead_df, csv)
    root = tmp_path / "models"
    assert train_mod.main(["--csv", str(csv), "--out", str(root)]) == 0
    assert "WARNING: the dead score failed (error: RuntimeError: boom / second part)" in \
        capsys.readouterr().out
    vdir = root / (root / "LATEST").read_text().strip()
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead_score"] == {"error": "error: RuntimeError: boom / second part"}
    assert "short_runner" in meta["models"] and (vdir / "short_runner.joblib").exists()
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "**Not computed: error: RuntimeError: boom / second part.**" in report
    assert "Traceback line" not in report and "**Bucket result:" in report


def test_a_crash_while_rendering_the_dead_score_never_fails_the_run(dead_df, short_only, tmp_path,
                                                                    monkeypatch, capsys):
    from scout_ml import report as R
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in
                                        ("baseline", "raw dex instead of dex_family")})

    def boom(p):
        raise KeyError("lightgbm")

    monkeypatch.setattr(R, "proposal_text", boom)            # reached by a full result only
    csv = tmp_path / "calls.csv"
    make_synthetic.write_csv(dead_df, csv)
    root = tmp_path / "models"
    assert train_mod.main(["--csv", str(csv), "--out", str(root), "--variants"]) == 0
    assert "WARNING: the dead score failed (error: KeyError: 'lightgbm')" in capsys.readouterr().out
    vdir = root / (root / "LATEST").read_text().strip()
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["dead_score"] == {"error": "error: KeyError: 'lightgbm'"}
    assert "short_runner" in meta["models"] and (vdir / "short_runner.joblib").exists()
    assert "top_lift" in meta["variants"]["raw dex instead of dex_family"]["buckets"]["short"]
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "**Not computed: error: KeyError: 'lightgbm'.**" in report
    assert "## Variant comparison" in report


def test_dead_waves_count_the_dex_family_of_dead_calls():
    dates = pd.Series(pd.to_datetime(["2026-09-22", "2026-09-23", "2026-09-24", "2026-09-30"],
                                     utc=True))
    y = np.array([1.0, 1.0, 0.0, 1.0])
    fam = pd.Series(["uniswap_v4", "uniswap_v4", "pancakeswap", None])
    waves = train_mod._dead_waves(dates, y, fam)
    assert waves[0] == {"week": "2026-09-21", "dead": 2, "calls": 3,
                        "dead_families": {"uniswap_v4": 2}}       # the alive call not counted
    assert waves[1] == {"week": "2026-09-28", "dead": 1, "calls": 1,
                        "dead_families": {"(none)": 1}}


def test_waves_line_says_few_waves_only_when_the_top_weeks_hold_most_dead():
    from scout_ml.report import waves_line
    conc = [{"week": "2026-09-22", "dead": 40, "calls": 100,
             "dead_families": {"uniswap_v4": 30, "other": 10}},
            {"week": "2026-09-29", "dead": 30, "calls": 90,
             "dead_families": {"uniswap_v4": 20, "raydium": 10}},
            {"week": "2026-09-15", "dead": 10, "calls": 80, "dead_families": {"other": 10}}]
    line = waves_line(conc)
    assert "hold 70 of 80 (88%;" in line
    assert "Most dead calls came in a few waves" in line
    assert 'most common dex_family among them: "uniswap_v4", 50 of 70' in line
    assert "9 Oct" not in line and "Uniswap V4" not in line   # nothing hard-coded
    spread = [{"week": f"2026-09-{d:02d}", "dead": 10, "calls": 50, "dead_families": {"x": 10}}
              for d in (1, 8, 15, 22, 29)]
    line = waves_line(spread)
    assert "hold 20 of 50 (40%;" in line
    assert "few waves" not in line and "Dead calls are spread over the weeks" in line
    # exactly the threshold is not "more than": neutral
    edge = [{"week": f"2026-09-{d:02d}", "dead": 10, "calls": 20} for d in (1, 8, 15, 22)]
    assert "hold 20 of 40 (50%;" in waves_line(edge) and "few waves" not in waves_line(edge)
    assert "few waves" not in waves_line([])                  # no dead calls at all


def test_the_top10_share_is_looked_up_by_value(monkeypatch):
    def top(lift10, lift30):
        return [{"frac": 0.30, "lift": lift30}, {"frac": 0.10, "lift": lift10}]

    monkeypatch.setattr(C, "DEAD_SCORE_TOP_FRACS", (0.30, 0.10))
    assert train_mod._gate_index() == 1
    res = {"top": {"lightgbm": top(5.0, 1.0), "logistic": top(1.0, 5.0)}, "walk_forward": []}
    p = train_mod.dead_score_proposal(res)
    assert p["lightgbm"]["test_lift"] == 5.0 and p["lightgbm"]["test_ok"]
    assert p["logistic"]["test_lift"] == 1.0 and not p["logistic"]["test_ok"]
    monkeypatch.setattr(C, "DEAD_SCORE_TOP_FRACS", (0.20, 0.30))
    with pytest.raises(ValueError, match="DEAD_SCORE_GATE_FRAC"):
        train_mod._gate_index()
