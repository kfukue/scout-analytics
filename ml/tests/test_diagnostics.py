"""Stability diagnostics (report only) and the late-bound config reads they
need. Synthetic data only."""
import json

import numpy as np
import pandas as pd
import pytest

import make_synthetic
import train as train_mod
from conftest import with_shipped_seeds
from scout_ml import config as C
from scout_ml import diagnostics as D
from scout_ml import validate as V
from scout_ml.features import learn_cat_levels
from scout_ml.model import Bundle, fit_lgbm, lgbm_params, usable_holdout

# The LightGBM settings every fit used before LGBM_SEED / LGBM_REGULARISATION
# existed (config.LGBM_PARAMS at 57fdced): the saved model must keep them.
PARAMS_BEFORE = dict(objective="binary", n_estimators=600, learning_rate=0.05, num_leaves=15,
                     min_child_samples=20, subsample=0.8, subsample_freq=1, colsample_bytree=0.8,
                     reg_lambda=1.0, random_state=7, n_jobs=2, verbose=-1)


def _xy(n=400, seed=0):
    rng = np.random.default_rng(seed)
    X = pd.DataFrame({c: rng.normal(size=n) for c in ("a", "b", "c")})
    y = (X["a"] + rng.normal(scale=0.5, size=n) > 0).astype(int).to_numpy()
    return X, y


# --- late-bound config reads ---------------------------------------------------

def test_saved_model_settings_are_unchanged_by_default():
    assert lgbm_params() == PARAMS_BEFORE
    assert C.LGBM_SEED == 7 and C.LGBM_REGULARISATION == "default"


def test_lightgbm_knobs_set_after_import_take_effect():
    X, y = _xy()
    assert fit_lgbm(X, y, n_estimators=5)["model"].get_params()["num_leaves"] == 15
    with C.overrides(LGBM_REGULARISATION="strong"):
        p = fit_lgbm(X, y, n_estimators=5)["model"].get_params()
        assert (p["num_leaves"], p["min_child_samples"], p["reg_lambda"],
                p["colsample_bytree"]) == (7, 50, 10.0, 0.6)
        assert p["random_state"] == 7 and p["learning_rate"] == 0.05   # the rest unchanged
    with C.overrides(LGBM_SEED=11):
        assert fit_lgbm(X, y, n_estimators=5)["model"].get_params()["random_state"] == 11
    assert lgbm_params() == PARAMS_BEFORE                        # restored on exit
    with pytest.raises(ValueError):
        with C.overrides(LGBM_REGULARISATION="nonsense"):
            pass


def test_model_and_validate_settings_are_read_at_call_time(monkeypatch):
    X, y = _xy()
    monkeypatch.setattr(C, "FALLBACK_ROUNDS", 3)
    assert fit_lgbm(X, y)["model"].get_params()["n_estimators"] == 3
    monkeypatch.setattr(C, "LGBM_PARAMS", {**C.LGBM_PARAMS, "learning_rate": 0.2})
    assert fit_lgbm(X, y, n_estimators=4)["model"].get_params()["learning_rate"] == 0.2
    assert usable_holdout(np.r_[np.ones(10), np.zeros(10)])       # 10 >= 30 // 3
    monkeypatch.setattr(C, "MIN_CLASS_ROWS", 60)
    assert not usable_holdout(np.r_[np.ones(10), np.zeros(10)])
    score, yy = np.arange(10, 0, -1, dtype=float), np.ones(10)
    assert V.trading_metrics(yy, score, None, None, np.zeros(10))["top_n"] == 1
    monkeypatch.setattr(C, "TOP_FRAC", 0.5)
    assert V.trading_metrics(yy, score, None, None, np.zeros(10))["top_n"] == 5
    monkeypatch.setattr(C, "SKIP_FRAC", 0.1)
    assert V.collapse_removed(yy, score) == pytest.approx(0.1)
    monkeypatch.setattr(C, "GATES", {"min_top_lift": 0.5, "min_collapse_removed": 0.05})
    g = V.gates({"top_lift": 1.0, "collapse_removed": 0.1}, [])
    assert g["lift_ok"] and g["collapse_ok"]
    monkeypatch.setattr(C, "MIN_WINDOW_TEST_ROWS", 5)
    assert V.window_ok(5)
    lo95 = V.wilson(5, 10)[0]
    monkeypatch.setattr(C, "LIFT_CI_Z", 3.0)
    assert V.wilson(5, 10)[0] < lo95
    dates = pd.Series(pd.date_range("2026-08-01", periods=60, freq="D", tz="UTC"))
    toks = pd.Series([f"t{i}" for i in range(60)])
    monkeypatch.setattr(C, "FORWARD_TEST_DAYS", 7)
    assert V.forward_split(dates, toks, 1)["t_test"] == pd.Timestamp("2026-09-22", tz="UTC")
    monkeypatch.setattr(C, "KFOLD_K", 3)
    assert len(list(V.purged_kfold(dates, toks, 1))) == 3
    monkeypatch.setattr(C, "TRAIN_FRAC", 0.5)
    assert V.time_split(dates, toks, 1)["t1"] == dates.iloc[30]
    rows = pd.DataFrame({"launchpad": ["x"] * 5 + ["y"] * 30})
    assert learn_cat_levels(rows)["launchpad"] == ["y"]
    monkeypatch.setattr(C, "MIN_CATEGORY_COUNT", 5)
    assert learn_cat_levels(rows)["launchpad"] == ["x", "y"]


# --- rolling walk-forward ------------------------------------------------------

def test_rolling_windows_keep_the_test_weeks_and_bound_the_training_span():
    dates = pd.Series(pd.date_range("2026-07-01", periods=120 * 4, freq="6h", tz="UTC"))
    toks = pd.Series([f"t{i}" for i in range(len(dates))])
    exp = list(V.walk_forward_windows(dates, toks, 1))
    roll = list(V.walk_forward_windows(dates, toks, 1, train_weeks=4))
    assert [w["week"] for w in exp] == [w["week"] for w in roll]
    for e, r in zip(exp, roll):
        np.testing.assert_array_equal(e["test"], r["test"])
        assert not (r["train"] & ~e["train"]).any()               # a subset of the expanding one
        cut = e["start"] - pd.Timedelta(days=1)
        d = dates[r["train"]]
        if len(d):
            assert d.min() >= cut - pd.Timedelta(days=28) and d.max() < cut
    assert sum(r["train"].sum() < e["train"].sum() for e, r in zip(exp, roll)) > 5


# --- pooled walk-forward -------------------------------------------------------

def _week(top_pos, all_pos, n=100, ret_top=10.0, ret_rest=-10.0, coll_top=0, coll_all=0):
    """A week whose top 10% (by score) holds top_pos runners out of all_pos."""
    score = np.arange(n, 0, -1, dtype=float)
    y = np.zeros(n)
    k = n // 10
    y[:top_pos] = 1
    y[k:k + all_pos - top_pos] = 1
    yc = np.zeros(n)
    yc[:coll_top] = 1
    yc[n - (coll_all - coll_top):] = 1
    ret = np.where(np.arange(n) < k, ret_top, ret_rest)
    return y, score, yc, score, ret


def test_pooled_lift_is_the_ratio_of_pooled_rates_with_week_ranking():
    w1 = D.week_sums(*_week(top_pos=4, all_pos=10, coll_top=20, coll_all=40))
    w2 = D.week_sums(*_week(top_pos=1, all_pos=10, ret_top=-50.0, coll_top=10, coll_all=40))
    assert (w1["top_pos"], w1["top_n"], w1["all_pos"], w1["all_n"]) == (4, 10, 10, 100)
    p = D.pooled([w1, w2])
    assert p["lift"] == pytest.approx((5 / 20) / (20 / 200))
    assert p["collapse_removed"] == pytest.approx(30 / 80)
    assert p["sim_top_mean"] == pytest.approx((10 * 10 - 50 * 10) / 20)
    assert p["weeks"] == 2 and p["rows"] == 200 and p["weeks_beating"] == 1
    assert p["lift_lo"] <= p["lift"] <= p["lift_hi"]
    assert D.pooled([w1, w2]) == p                                # fixed seed: reproducible
    one = D.pooled([w1])
    assert one["lift"] == pytest.approx(4.0) and np.isnan(one["lift_lo"])
    assert D.pooled([])["weeks"] == 0
    # the top share is re-taken among the kept rows
    y, s, yc, sc, ret = _week(top_pos=4, all_pos=10)
    keep = np.ones(100, bool)
    keep[:5] = False
    w = D.week_sums(y, s, yc, sc, ret, keep)
    assert (w["n"], w["top_n"], w["all_pos"]) == (95, 10, 10 - min(4, 5))
    assert np.isnan(D.week_sums(y, s, yc, None, ret)["coll_top"])


def test_pooled_bootstrap_interval_reflects_week_to_week_noise():
    steady = [D.week_sums(*_week(top_pos=3, all_pos=10)) for _ in range(6)]
    noisy = [D.week_sums(*_week(top_pos=t, all_pos=10)) for t in (0, 6, 1, 5, 0, 6)]
    ps, pn = D.pooled(steady), D.pooled(noisy)
    assert ps["lift"] == pytest.approx(pn["lift"]) == pytest.approx(3.0)
    assert ps["lift_hi"] - ps["lift_lo"] == pytest.approx(0.0)
    assert pn["lift_hi"] - pn["lift_lo"] > 1.0


def test_spread():
    assert D.spread([1.0, np.nan, 3.0, 2.0]) == {"min": 1.0, "median": 2.0, "max": 3.0, "n": 3}
    assert D.spread([None, np.nan])["n"] == 0


# --- adversarial validation ----------------------------------------------------

def test_adversarial_finds_planted_drift_and_not_noise():
    rng = np.random.default_rng(3)
    n = 1200
    X = pd.DataFrame({"liq_usd": rng.normal(size=n), "holders": rng.normal(size=n),
                      "weekday_utc": rng.integers(0, 7, n).astype(float)})
    is_test = np.r_[np.zeros(900), np.ones(300)]
    flat = D.adversarial(X, is_test)
    assert 0.35 < flat["auc"] < 0.65
    X.loc[is_test == 1, "holders"] += 1.5                         # the test calls drifted
    X.loc[is_test == 1, "weekday_utc"] = 0.0                      # calendar: never a probe input
    drift = D.adversarial(X, is_test)
    assert drift["auc"] > 0.8 and drift["top"][0][0] == "holders"
    assert "weekday_utc" not in [f for f, _ in drift["top"]]
    assert (drift["n_train"], drift["n_test"]) == (900, 300)
    assert D.adversarial(X.head(903), is_test[:903])["skipped"]


# --- end to end ----------------------------------------------------------------

@pytest.fixture(scope="module")
def small_df():
    return make_synthetic.make(n=2500, days=60, seed=5)


@pytest.fixture(scope="module")
def off_and_on(small_df, tmp_path_factory):
    """The same data trained without and with the diagnostics (same version name),
    with the shipped STABILITY_SEEDS."""
    with with_shipped_seeds():
        off = train_mod.train(small_df, tmp_path_factory.mktemp("off"), "v1", diagnostics=False)
        on = train_mod.train(small_df, tmp_path_factory.mktemp("on"), "v1", diagnostics=True)
    return off, on


def _rows(df, n=60):
    return json.loads(df.sort_values("message_date").tail(n).to_json(orient="records",
                                                                     date_format="iso"))


def test_diagnostics_never_change_the_saved_model_meta_gates_or_scores(off_and_on, small_df):
    off, on = off_and_on
    assert sorted(p.name for p in off.iterdir()) == sorted(p.name for p in on.iterdir())
    m_off, m_on = (json.loads((d / "meta.json").read_text()) for d in (off, on))
    assert set(m_on) - set(m_off) == {"diagnostics"} and not set(m_off) - set(m_on)
    for k in m_off:
        if k != "trained_at":
            assert m_on[k] == m_off[k], k                         # metrics, gates, levels, ...
    b_off, b_on = Bundle(off), Bundle(on)
    rows = _rows(small_df)
    for b in m_off["runner_reference"]:
        for lab in ("runner", "collapse"):
            name = f"{b}_{lab}"
            if name in b_off.models:
                np.testing.assert_array_equal(
                    b_off.models[name]["model"].predict(b_off.features(rows, b), raw_score=True),
                    b_on.models[name]["model"].predict(b_on.features(rows, b), raw_score=True))
    for row in rows[:10]:
        for mode in C.LINE_MODES:
            assert b_on.score(row, mode) == b_off.score(row, mode)
    r_off = (off / "report.md").read_text(encoding="utf-8")
    r_on = (on / "report.md").read_text(encoding="utf-8")
    assert "Stability diagnostics" not in r_off
    head = r_on.split("## Stability diagnostics")[0]
    assert head.rstrip("\n") == r_off.rstrip("\n")                # everything above unchanged
    assert C.LGBM_SEED == 7 and list(C.FEATURES) == m_on["features"]   # config restored


def test_seed_stability_contents(off_and_on, shipped_seeds):
    _, on = off_and_on
    meta = json.loads((on / "meta.json").read_text())
    sd = meta["diagnostics"]["seeds"]
    assert sd["seeds"] == list(shipped_seeds) and sd["saved_seed"] == C.LGBM_SEED
    assert set(sd["runs"]) == {str(s) for s in shipped_seeds}
    saved = sd["runs"][str(C.LGBM_SEED)]
    assert saved["saved_model"] is True
    trained = [b for b, m in meta["metrics"].items() if not m.get("skipped")]
    assert trained
    differs = False
    for b in trained:
        m, s = meta["metrics"][b], saved["buckets"][b]
        assert s["top_lift"] == m["trading"]["top_lift"]          # the saved model's own numbers
        assert s["collapse_removed"] == m["trading"]["collapse_removed"]
        assert s["runner_auc"] == m["labels"]["runner"]["lightgbm"]["roc_auc"]
        assert s["windows_beating"] == m["gates"]["old"]["windows_beating"]
        assert s["passed"] == m["gates"]["old"]["passed"]           # single-run gate column
        # the gate's median is over exactly these runs' pooled walk-forward lifts
        lifts = {str(seed): sd["runs"][str(seed)]["buckets"][b]["wf_pooled_lift"]
                 for seed in shipped_seeds}
        assert m["gates"]["pooled_lift_seeds"] == pytest.approx(lifts)
        if None in lifts.values():                                # no counted week: undefined
            assert m["gates"]["pooled_lift_median"] is None and m["gates"]["passed"] is False
        else:
            assert m["gates"]["pooled_lift_median"] == pytest.approx(
                float(np.median(list(lifts.values()))))
        for seed in shipped_seeds:
            r = sd["runs"][str(seed)]["buckets"][b]
            assert r["sim_all_mean"] == pytest.approx(s["sim_all_mean"])
            differs |= r["runner_auc"] != s["runner_auc"]
        sp = sd["spread"][b]
        assert sp["top_lift"]["n"] == len(shipped_seeds)
        assert sp["top_lift"]["min"] <= sp["top_lift"]["median"] <= sp["top_lift"]["max"]
    assert differs                                                # the seed really changed something
    for b, m in meta["metrics"].items():
        if m.get("skipped"):
            assert b not in sd["spread"]


def test_pooled_walk_forward_matches_the_gate_weeks(off_and_on):
    _, on = off_and_on
    meta = json.loads((on / "meta.json").read_text())
    wd = meta["diagnostics"]["walk_forward"]
    for b, m in meta["metrics"].items():
        if m.get("skipped"):
            assert b not in wd
            continue
        e = wd[b]["expanding"]
        counted = [w for w in m["walk_forward"] if w["counted"]]
        assert e["weeks"] == len(counted) == m["gates"]["old"]["windows_evaluated"]
        assert e.get("weeks_beating", 0) == m["gates"]["old"]["windows_beating"]
        assert e["weeks"] == m["gates"]["pooled_weeks"]
        # the gate's interval lower ends are this expanding row's
        assert m["gates"]["pooled_lift_lo"] == e.get("lift_lo")      # null with < 2 weeks
        assert m["gates"]["sim_diff"] == e.get("sim_gain")
        assert m["gates"]["sim_diff_lo"] == e.get("sim_gain_lo")
        if e["weeks"] < C.GATE_MIN_POOLED_WEEKS:
            assert m["gates"]["passed"] is False and m["gates"]["reasons"]
        assert e["rows"] == sum(w["n_test"] for w in counted)
        if len(counted) == 1:
            assert e["lift"] == pytest.approx(counted[0]["top_lift"])
        assert [w["week"] for w in wd[b]["weeks"]] == [w["week"] for w in m["walk_forward"]]
        assert wd[b]["rolling_weeks"] == C.WF_ROLLING_WEEKS
        smaller = 0
        for w in wd[b]["weeks"]:
            if "n_train" in w["rolling"]:
                assert w["rolling"]["n_train"] <= w["expanding"]["n_train"]
                smaller += w["rolling"]["n_train"] < w["expanding"]["n_train"]
            if not w["rolling"].get("skipped") and w["rolling"]:   # paired: the gate's weeks
                assert w["rolling"]["counted"] == w["expanding"]["counted"]
        assert smaller > 0                                        # the window really rolls
        r = wd[b]["rolling"]
        assert r["weeks"] == wd[b]["rolling_gate"]["windows_evaluated"] <= e["weeks"]
        assert r.get("weeks_beating", 0) == wd[b]["rolling_gate"]["windows_beating"]


def test_evaluation_without_dead_calls(off_and_on):
    _, on = off_and_on
    meta = json.loads((on / "meta.json").read_text())
    nd = meta["diagnostics"]["no_dead"]
    for b, m in meta["metrics"].items():
        if m.get("skipped"):
            continue
        allc, nod = nd[b]["rows"]["all calls"], nd[b]["rows"]["dead calls excluded"]
        t = m["trading"]
        for k in ("top_lift", "collapse_removed", "sim_top_mean", "sim_all_mean", "all_n"):
            assert allc[k] == pytest.approx(t[k]), (b, k)          # same models, same rows
        assert allc["runner_auc"] == pytest.approx(m["labels"]["runner"]["lightgbm"]["roc_auc"])
        assert nd[b]["dead_test"] == m["dead"]["test"]["dead"] > 0
        assert nod["n_test"] == allc["n_test"] - nd[b]["dead_test"]
        assert nod["all_n"] == nod["n_test"]


def test_adversarial_section_uses_model_inputs_only(off_and_on):
    _, on = off_and_on
    meta = json.loads((on / "meta.json").read_text())
    ad = meta["diagnostics"]["adversarial"]
    for b, m in meta["metrics"].items():
        if m.get("skipped"):
            continue
        s = ad[b]["split"]
        assert 0.0 <= s["auc"] <= 1.0
        assert s["n_test"] == m["split"]["n_test"] and s["n_train"] == m["split"]["n_train"]
        for f, _ in s["top"]:
            assert f in meta["features"] and f not in C.ADV_EXCLUDE and not C.is_forbidden(f)
        assert len(ad[b]["weeks"]) == min(m["gates"]["old"]["windows_evaluated"], C.ADV_MAX_WEEKS)


def test_report_has_the_diagnostics_sections(off_and_on):
    _, on = off_and_on
    report = (on / "report.md").read_text(encoding="utf-8")
    for title in ("## Stability diagnostics (not saved, not served; only the seeds' pooled lifts "
                  "and the pooled expanding walk-forward feed the gates)",
                  "| single-run (old) gate, information |", "pooled WF lift (gate input)",
                  "### Seed stability", "### Pooled walk-forward (expanding and rolling)",
                  "### Evaluation without dead calls", "### Adversarial validation",
                  "| seed 7 (saved model) |", "| expanding (the gate's) |", "| rolling (4 weeks) |",
                  "| dead calls excluded |", "week-block bootstrap (2000 resamples"):
        assert title in report, title
    meta = json.loads((on / "meta.json").read_text())
    assert set(meta["diagnostics"]["seconds"]) == {"seeds", "walk_forward", "no_dead", "adversarial"}


def test_diagnostics_come_before_the_variant_comparison():
    res = {"diagnostics": {"error": "error: X"}, "variants": {"baseline": {"changes": {}}}}
    from scout_ml import report as R
    text = "\n".join([R.diagnostics_section(res["diagnostics"], {}),
                      R.variants_section(res["variants"], {})])
    assert text.index("Stability diagnostics") < text.index("Variant comparison")
    assert "**Not computed: error: X.**" in text


# --- switches and failures -----------------------------------------------------

def test_diagnostics_are_on_by_default_as_shipped(shipped_diagnostics):
    # conftest switches C.DIAGNOSTICS off for the suite; this is the value
    # recorded before that, i.e. what train.py runs with.
    assert shipped_diagnostics is True
    assert C.DIAGNOSTICS is False                                 # the suite's override is active


def test_switch_env_and_config(monkeypatch, capsys):
    monkeypatch.setattr(C, "DIAGNOSTICS", True)
    monkeypatch.delenv(C.DIAGNOSTICS_ENV, raising=False)
    assert train_mod.diagnostics_on() is True
    for val, want in (("0", False), (" OFF ", False), ("no", False), ("false", False),
                      ("1", True), ("on", True)):
        monkeypatch.setenv(C.DIAGNOSTICS_ENV, val)
        assert train_mod.diagnostics_on() is want, val
    monkeypatch.setenv(C.DIAGNOSTICS_ENV, "maybe")
    assert train_mod.diagnostics_on() is True and "WARNING" in capsys.readouterr().out
    monkeypatch.setattr(C, "DIAGNOSTICS", False)
    monkeypatch.delenv(C.DIAGNOSTICS_ENV)
    assert train_mod.diagnostics_on() is False
    monkeypatch.setenv(C.DIAGNOSTICS_ENV, "on")
    assert train_mod.diagnostics_on() is True
    assert train_mod.diagnostics_on(False) is False               # an explicit flag wins


def test_cli_runs_diagnostics_by_default_and_env_switches_them_off(small_df, tmp_path, monkeypatch):
    monkeypatch.setattr(C, "BUCKETS", {})                        # fast: no bucket to train
    monkeypatch.setattr(C, "DIAGNOSTICS", True)                  # the shipped default
    monkeypatch.delenv(C.DIAGNOSTICS_ENV, raising=False)
    csv = tmp_path / "calls.csv"
    make_synthetic.write_csv(small_df, csv)

    def run(name):
        root = tmp_path / name
        assert train_mod.main(["--csv", str(csv), "--out", str(root)]) == 0
        return json.loads((root / (root / "LATEST").read_text().strip() / "meta.json").read_text())
    assert "diagnostics" in run("default")
    monkeypatch.setenv(C.DIAGNOSTICS_ENV, "0")
    assert "diagnostics" not in run("off")


def test_a_failing_diagnostics_part_is_recorded_and_the_run_succeeds(small_df, tmp_path,
                                                                     monkeypatch, capsys):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7,))              # no retrain: fast

    def boom(keeps):
        raise RuntimeError("adversarial | broke\nsecond line")
    monkeypatch.setattr(train_mod, "adversarial_validation", boom)
    out = train_mod.train(small_df, tmp_path, "v1", diagnostics=True)
    assert "WARNING: diagnostics part 'adversarial' failed" in capsys.readouterr().out
    meta = json.loads((out / "meta.json").read_text())
    d = meta["diagnostics"]
    assert d["adversarial"] == {"error": "error: RuntimeError: adversarial / broke"}
    assert "short" in d["walk_forward"] and "short" in d["no_dead"]
    assert (out / "short_runner.joblib").exists() and "short_runner" in meta["models"]
    report = (out / "report.md").read_text(encoding="utf-8")
    assert "**Not computed: error: RuntimeError: adversarial / broke.**" in report
    assert "second line" not in report


def test_without_trades_24h_or_switched_off_the_dead_evaluation_is_na(small_df, tmp_path,
                                                                      monkeypatch):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7,))
    monkeypatch.setattr(C, "ADV_PER_WEEK", False)
    out = train_mod.train(small_df.drop(columns="trades_24h"), tmp_path / "a", "v1",
                          diagnostics=True)
    d = json.loads((out / "meta.json").read_text())["diagnostics"]
    assert "no trades_24h column" in d["no_dead"]["not_applicable"]
    assert d["adversarial"]["short"]["weeks"] == []
    monkeypatch.setattr(C, "EVAL_EXCLUDE_DEAD", False)
    out = train_mod.train(small_df, tmp_path / "b", "v1", diagnostics=True)
    d = json.loads((out / "meta.json").read_text())["diagnostics"]
    assert d["no_dead"] == {"off": "EVAL_EXCLUDE_DEAD = False"}
    assert "Switched off (EVAL_EXCLUDE_DEAD = False)" in (out / "report.md").read_text(
        encoding="utf-8")
