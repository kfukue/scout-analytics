"""The bucket gates (owner decision of 13 Oct 2026, "option (b)"): median over the
seeds of the pooled walk-forward lift, the bootstrap lower ends of the pooled lift
and of the pooled simulation difference, collapses removed; FAIL on missing
inputs; computed with the diagnostics off too; the scoring service reads the new
verdict. Synthetic data only."""
import json

import numpy as np
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml import validate as V
from scout_ml.model import Bundle

NAN = float("nan")


@pytest.fixture
def seeds(monkeypatch, shipped_seeds):
    """The shipped STABILITY_SEEDS (7, 11, 23) for the unit tests of the verdict."""
    monkeypatch.setattr(C, "STABILITY_SEEDS", shipped_seeds)
    return shipped_seeds


def _single(passed=False):
    """An old single-run gate dict (information only)."""
    return {"lift_ok": passed, "collapse_ok": passed, "walk_forward_ok": passed,
            "windows_evaluated": 8, "windows_beating": 8 if passed else 7,
            "windows_not_counted": 2, "wf_min_train_rows": C.WF_MIN_TRAIN_ROWS, "passed": passed}


def _pooled(lift, lo, hi, sim, sim_lo, sim_hi, weeks=8, top=5.0, all_=-30.0):
    return {"weeks": weeks, "rows": 4000, "lift": lift, "lift_lo": lo, "lift_hi": hi,
            "sim_gain": sim, "sim_gain_lo": sim_lo, "sim_gain_hi": sim_hi,
            "sim_top_mean": top, "sim_all_mean": all_}


def _verdict(lifts, pooled, collapse_removed=0.46, single=None):
    test = {"top_lift": 1.9, "collapse_removed": collapse_removed}
    return V.pooled_gates(single or _single(), test, pooled, dict(zip((7, 11, 23), lifts)))


def test_shipped_gate_settings(shipped_seeds):
    assert shipped_seeds == (7, 11, 23) and C.LGBM_SEED == 7
    assert C.GATES == {"min_top_lift": 2.0, "min_collapse_removed": 0.40}
    assert (C.GATE_POOLED_LIFT_LOWER, C.GATE_SIM_DIFF_LOWER, C.GATE_MIN_POOLED_WEEKS) == (1.0, 0.0, 2)
    assert C.GATES_SET_ON == "13 Oct 2026"
    assert (C.POOLED_BOOTSTRAP_REPS, C.POOLED_BOOTSTRAP_SEED, C.POOLED_CI) == (2000, 7, (0.025, 0.975))


# --- the verdict on the numbers of report 20261013-0702 ---------------------------

def test_3day_of_report_20261013_0702_fails_on_the_median_lift_only(seeds):
    g = _verdict((1.98, 2.02, 1.85), _pooled(1.98, 1.45, 2.6, 28.6, 14.7, 51.0))
    assert g["pooled_lift_median"] == pytest.approx(1.98)
    assert g["pooled_lift_seeds"] == {"7": 1.98, "11": 2.02, "23": 1.85}
    assert not g["pooled_lift_ok"] and g["pooled_lift_lower_ok"] and g["sim_diff_ok"]
    assert g["collapse_ok"] and g["passed"] is False
    assert g["reasons"] == ["median pooled lift 1.98 < 2"]


def test_medium_of_report_20261013_0702_passes(seeds):
    g = _verdict((2.18, 2.26, 1.94), _pooled(2.18, 1.74, 2.7, 43.1, 36.7, 51.6))
    assert g["pooled_lift_median"] == pytest.approx(2.18)
    assert g["pooled_lift_ok"] and g["pooled_lift_lower_ok"] and g["sim_diff_ok"] and g["collapse_ok"]
    assert g["passed"] is True and g["reasons"] == []
    # the old single-run gate is kept as information and never decides
    assert g["old"]["passed"] is False and g["old"]["top_lift"] == 1.9


def test_the_median_is_over_the_seeds_not_the_saved_seed(seeds):
    # the saved seed alone would pass (2.5), the median of the three does not
    g = _verdict((2.5, 1.9, 1.95), _pooled(2.5, 1.8, 3.1, 40.0, 20.0, 60.0))
    assert g["pooled_lift_median"] == pytest.approx(1.95) and not g["passed"]
    # and the other way round: a weak saved seed is outvoted by the other two
    g = _verdict((1.9, 2.1, 2.3), _pooled(1.9, 1.3, 2.5, 40.0, 20.0, 60.0))
    assert g["pooled_lift_median"] == pytest.approx(2.1) and g["passed"]
    assert _verdict((2.0, 2.0, 2.0), _pooled(2.0, 1.5, 2.5, 1.0, 0.5, 2.0))["pooled_lift_ok"]  # >= 2


def test_lower_bound_gate_is_strict(seeds):
    ok = _pooled(2.2, 1.01, 3.0, 40.0, 20.0, 60.0)
    assert _verdict((2.2, 2.2, 2.2), ok)["passed"]
    g = _verdict((2.2, 2.2, 2.2), {**ok, "lift_lo": 1.0})
    assert not g["pooled_lift_lower_ok"] and not g["passed"]
    assert g["reasons"] == ["pooled lift 95% lower end 1.00 <= 1"]


def test_sim_difference_gate_needs_its_lower_end_above_zero(seeds):
    ok = _pooled(2.2, 1.5, 3.0, 10.0, 0.5, 20.0)
    assert _verdict((2.2, 2.2, 2.2), ok)["sim_diff_ok"]
    for lo in (0.0, -3.2):
        g = _verdict((2.2, 2.2, 2.2), {**ok, "sim_gain_lo": lo})
        assert not g["sim_diff_ok"] and not g["passed"]
        assert g["reasons"] == [f"pooled simulation difference 95% lower end {lo:+.1f} <= 0"]


def test_absolute_return_is_information_only(seeds):
    # the top 10% still loses money on average, but less than buying all: PASS
    g = _verdict((2.2, 2.3, 2.1), _pooled(2.2, 1.5, 3.0, 35.0, 20.0, 50.0, top=-8.8, all_=-43.5))
    assert g["passed"] and g["abs_return_positive"] is False and g["abs_return_top"] == -8.8


def test_collapses_removed_unchanged(seeds):
    p = _pooled(2.2, 1.5, 3.0, 35.0, 20.0, 50.0)
    assert _verdict((2.2,) * 3, p, collapse_removed=0.40)["collapse_ok"]          # >= 40%
    g = _verdict((2.2,) * 3, p, collapse_removed=0.399)
    assert not g["collapse_ok"] and not g["passed"] and g["reasons"] == ["collapses removed 0.399 < 0.4"]


def test_thresholds_are_read_at_call_time(seeds, monkeypatch):
    p = _pooled(2.2, 1.5, 3.0, 35.0, 20.0, 50.0)
    assert _verdict((2.2,) * 3, p)["passed"]
    monkeypatch.setattr(C, "GATE_POOLED_LIFT_LOWER", 1.6)
    assert not _verdict((2.2,) * 3, p)["pooled_lift_lower_ok"]
    monkeypatch.setattr(C, "GATE_POOLED_LIFT_LOWER", 1.0)
    monkeypatch.setattr(C, "GATE_SIM_DIFF_LOWER", 25.0)
    assert not _verdict((2.2,) * 3, p)["sim_diff_ok"]
    monkeypatch.setattr(C, "GATE_SIM_DIFF_LOWER", 0.0)
    monkeypatch.setattr(C, "GATES", {"min_top_lift": 2.5, "min_collapse_removed": 0.40})
    assert not _verdict((2.2,) * 3, p)["pooled_lift_ok"]
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7, 11))           # the median follows the seeds
    g = V.pooled_gates(_single(), {"collapse_removed": 0.5}, p, {7: 3.0, 11: 2.4})
    assert g["pooled_lift_median"] == pytest.approx(2.7) and g["passed"]


def test_verdict_cells_and_reasons_in_the_report(seeds):
    from scout_ml import report as R
    med = _verdict((2.18, 2.26, 1.94), _pooled(2.18, 1.74, 2.7, 43.1, 36.7, 51.6))
    three = _verdict((1.98, 2.02, 1.85), _pooled(1.98, 1.45, 2.6, 28.6, 14.7, 51.0, top=-2.0))
    assert R.gate_cells(med)[:5] == ["yes: 2.18 (2.18 / 2.26 / 1.94)", "yes: 1.74",
                                     "yes: +43.1 [+36.7, +51.6]", "yes: 0.460", "+5.0 (> 0)"]
    assert R.gate_cells(three)[0] == "no: 1.98 (1.98 / 2.02 / 1.85)"
    assert R.gate_cells(three)[4] == "-2.0 (<= 0)"                  # information only
    assert R.gate_cells(three)[5:] == ["no: 1.90", "no (7/8 weeks; 2 not counted)"]
    heads = R.verdict_headers()
    assert heads[2] == "runner lift: median pooled WF lift >= 2 (seeds 7 / 11 / 23)"
    assert heads[7].startswith("old gate, information only")
    lines = R.reasons_lines({"medium": {"gates": med}, "3day": {"gates": three},
                             "long": {"skipped": "x"}})
    assert lines == "- `3day` FAIL: median pooled lift 1.98 < 2\n"
    assert "Gates (owner decision of 13 Oct 2026)" in R.gate_rule_text()


# --- missing or undefined inputs: always FAIL, with a reason -------------------------

@pytest.mark.parametrize("case", ["one week", "no week", "pooled missing", "pooled error",
                                  "seed failed", "seed skipped", "seed NaN", "seed absent",
                                  "lift_lo NaN", "sim_lo NaN", "collapse NaN"])
def test_missing_inputs_fail_with_a_reason(seeds, case):
    good = _pooled(2.5, 1.8, 3.2, 40.0, 20.0, 60.0)
    lifts = {7: 2.5, 11: 2.4, 23: 2.6}
    pooled, collapse = dict(good), 0.5
    if case == "one week":       # D.pooled leaves the interval NaN with one week
        pooled.update(weeks=1, lift_lo=NAN, lift_hi=NAN, sim_gain_lo=NAN, sim_gain_hi=NAN)
    elif case == "no week":
        pooled = {"weeks": 0, "rows": 0, "reps": 2000, "seed": 7}
    elif case == "pooled missing":
        pooled = None
    elif case == "pooled error":
        pooled = {"error": "error: RuntimeError: x"}
    elif case == "seed failed":
        lifts[11] = "retrain failed (error: RuntimeError: x)"
    elif case == "seed skipped":
        lifts[23] = "bucket skipped (only 10 usable rows (need 300))"
    elif case == "seed NaN":
        lifts[11] = NAN
    elif case == "seed absent":
        del lifts[23]
    elif case == "lift_lo NaN":
        pooled["lift_lo"] = NAN
    elif case == "sim_lo NaN":
        pooled["sim_gain_lo"] = NAN
    elif case == "collapse NaN":
        collapse = NAN
    g = V.pooled_gates(_single(passed=True), {"collapse_removed": collapse}, pooled, lifts)
    assert g["passed"] is False and g["reasons"], case
    text = "; ".join(g["reasons"])
    want = {"one week": "only 1 counted walk-forward week(s); the pooled gates need at least 2",
            "no week": "only 0 counted walk-forward week(s)",
            "pooled missing": "pooled walk-forward not computed",
            "pooled error": "pooled walk-forward not computed (error: RuntimeError: x)",
            "seed failed": "pooled lift of seed 11: retrain failed (error: RuntimeError: x)",
            "seed skipped": "pooled lift of seed 23: bucket skipped",
            "seed NaN": "pooled lift of seed 11: undefined",
            "seed absent": "pooled lift of seed 23: undefined",
            "lift_lo NaN": "pooled lift 95% interval undefined",
            "sim_lo NaN": "pooled simulation difference 95% interval undefined",
            "collapse NaN": "collapses removed undefined"}[case]
    assert want in text, (case, text)
    if case.startswith("seed"):                    # never the median of the seeds that survived
        assert np.isnan(g["pooled_lift_median"]) and not g["pooled_lift_ok"]
    json.dumps(train_mod._json_safe(g), allow_nan=False)        # meta.json-safe


def test_no_seeds_configured_fails(monkeypatch):
    monkeypatch.setattr(C, "STABILITY_SEEDS", ())
    g = V.pooled_gates(_single(), {"collapse_removed": 0.5}, _pooled(3, 2, 4, 50, 40, 60), {})
    assert not g["passed"] and "no STABILITY_SEEDS configured" in g["reasons"]


# --- in training ------------------------------------------------------------------

@pytest.fixture(scope="module")
def small_df():
    return make_synthetic.make(n=2500, days=60, seed=5)


def _short_only(monkeypatch, seeds=(7, 11)):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "STABILITY_SEEDS", seeds)


def _count_train_bucket(monkeypatch):
    calls = []
    real = train_mod._train_bucket
    monkeypatch.setattr(train_mod, "_train_bucket",
                        lambda *a, **k: calls.append((a[0], C.LGBM_SEED, a[3])) or real(*a, **k))
    return calls


def test_gate_inputs_are_computed_with_the_diagnostics_off(small_df, tmp_path, monkeypatch):
    """Choice: SCOUT_ML_DIAGNOSTICS=0 still runs the seed retrains and the pooled
    walk-forward (the verdict needs them); only the report-only parts are skipped."""
    _short_only(monkeypatch)
    monkeypatch.setenv(C.DIAGNOSTICS_ENV, "0")
    calls = _count_train_bucket(monkeypatch)
    out = train_mod.train(small_df, tmp_path, "v1")              # diagnostics: env says off
    assert calls == [("short", 7, out), ("short", 11, None)]     # the seed-11 retrain saves nothing
    meta = json.loads((out / "meta.json").read_text())
    assert "diagnostics" not in meta
    g = meta["metrics"]["short"]["gates"]
    assert set(g["pooled_lift_seeds"]) == {"7", "11"}
    assert all(isinstance(v, float) for v in g["pooled_lift_seeds"].values())
    assert g["pooled_weeks"] >= C.GATE_MIN_POOLED_WEEKS
    assert g["pooled_lift_lo"] <= g["pooled_lift"] <= g["pooled_lift_hi"]
    assert g["sim_diff_lo"] <= g["sim_diff"] <= g["sim_diff_hi"]
    assert g["pooled_lift_seeds"]["7"] == pytest.approx(g["pooled_lift"])    # the saved seed's
    assert meta["thresholds"]["gate_rules"]["seeds"] == [7, 11]
    assert meta["thresholds"]["gate_rules"]["set_on"] == "13 Oct 2026"
    report = (out / "report.md").read_text(encoding="utf-8")
    assert "Stability diagnostics" not in report
    assert "runner lift: median pooled WF lift >= 2 (seeds 7 / 11)" in report
    assert "Gates (owner decision of 13 Oct 2026)" in report
    assert "old gate, information only: test-part lift >= 2" in report
    assert "absolute return: pooled top-10% mean net % (information, not a gate)" in report


def test_a_failing_seed_retrain_fails_the_bucket_and_the_run_succeeds(small_df, tmp_path,
                                                                      monkeypatch, capsys):
    _short_only(monkeypatch)
    real = train_mod._train_bucket

    def flaky(*a, **k):
        if C.LGBM_SEED == 11:
            raise RuntimeError("seed broke | here")
        return real(*a, **k)
    monkeypatch.setattr(train_mod, "_train_bucket", flaky)
    out = train_mod.train(small_df, tmp_path, "v1", diagnostics=False)
    printed = capsys.readouterr().out
    assert "WARNING: seed 11 of the seed retrains failed" in printed
    assert "[short] FAIL: pooled lift of seed 11: retrain failed" in printed
    meta = json.loads((out / "meta.json").read_text())
    g = meta["metrics"]["short"]["gates"]
    assert g["passed"] is False and g["pooled_lift_seeds"]["11"] is None
    assert any("seed 11: retrain failed (error: RuntimeError: seed broke / here)" in r
               for r in g["reasons"])
    assert (out / "short_runner.joblib").exists()                  # the saved model is intact
    assert Bundle(out).passed("short") is False
    report = (out / "report.md").read_text(encoding="utf-8")
    assert "- `short` FAIL: " in report and "Why FAIL: " in report


def test_too_few_counted_weeks_fail_the_bucket(small_df, tmp_path, monkeypatch):
    _short_only(monkeypatch, seeds=(7,))
    monkeypatch.setattr(C, "WF_MIN_TRAIN_ROWS", 10 ** 9)         # no week counts
    out = train_mod.train(small_df, tmp_path, "v1", diagnostics=False)
    g = json.loads((out / "meta.json").read_text())["metrics"]["short"]["gates"]
    assert g["pooled_weeks"] == 0 and g["passed"] is False
    assert g["reasons"][0] == "only 0 counted walk-forward week(s); the pooled gates need at least 2"
    assert "pooled lift of seed 7: undefined" in g["reasons"]
    assert g["old"]["walk_forward_ok"] is False                    # no counted week: old gate too


def _train_forced(small_df, root, monkeypatch, old_passed: bool, new_passes: bool):
    """Train the short bucket with the OLD single-run gate forced to `old_passed`
    and the new gate thresholds set so that it trivially passes / cannot pass."""
    real = V.gates
    monkeypatch.setattr(train_mod.V, "gates", lambda t, w: {**real(t, w), "passed": old_passed})
    lo = -1e9 if new_passes else 1e9
    monkeypatch.setattr(C, "GATE_POOLED_LIFT_LOWER", lo)
    monkeypatch.setattr(C, "GATE_SIM_DIFF_LOWER", lo)
    monkeypatch.setattr(C, "GATES", {"min_top_lift": 0.0 if new_passes else 1e9,
                                     "min_collapse_removed": 0.0 if new_passes else 1e9})
    return train_mod.train(small_df, root, "v1", diagnostics=False)


@pytest.mark.parametrize("old_passed,new_passes", [(True, False), (False, True)])
def test_meta_passed_flag_from_the_new_gates_drives_the_scoring_service(
        small_df, tmp_path, monkeypatch, old_passed, new_passes):
    _short_only(monkeypatch, seeds=(7,))
    out = _train_forced(small_df, tmp_path, monkeypatch, old_passed, new_passes)
    meta = json.loads((out / "meta.json").read_text())
    g = meta["metrics"]["short"]["gates"]
    assert g["old"]["passed"] is old_passed and g["passed"] is new_passes
    bundle = Bundle(out)
    assert bundle.passed("short") is new_passes                    # serve reads the new verdict
    row = json.loads(small_df.tail(1).to_json(orient="records", date_format="iso"))[0]
    passed_line = bundle.score(row, "passed")
    all_line = bundle.score(row, "all")
    assert passed_line["buckets"][0]["passed"] is new_passes
    if new_passes:
        assert "1d: runner" in passed_line["line"] and C.LINE_NOT_VALIDATED not in all_line["line"]
    else:
        assert passed_line["line"] == ""                           # default mode: nothing shown
        assert f"1d {C.LINE_NOT_VALIDATED}: runner" in all_line["line"]
    # SCOUT_MODEL_LINE never changes a score or `passed`
    assert passed_line["buckets"] == all_line["buckets"]
    # the end to end through serve.py
    from fastapi.testclient import TestClient
    import serve
    monkeypatch.setenv("SCOUT_MODEL_DIR", str(out))
    monkeypatch.delenv(C.LINE_MODE_ENV, raising=False)
    serve._cache.update(dir=None, bundle=None)
    body = TestClient(serve.app).post("/score", json={"row": row}).json()
    assert body["buckets"][0]["passed"] is new_passes
    assert (body["line"] != "") is new_passes
    serve._cache.update(dir=None, bundle=None)


def test_the_saved_model_is_the_same_whatever_the_gate_inputs(small_df, tmp_path, monkeypatch):
    """The seed retrains run after the saved model is fitted and save nothing:
    the same model files and scores with one seed or three."""
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7,))
    one = train_mod.train(small_df, tmp_path / "one", "v1", diagnostics=False)
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7, 11, 23))
    three = train_mod.train(small_df, tmp_path / "three", "v1", diagnostics=False)
    m1, m3 = (json.loads((d / "meta.json").read_text()) for d in (one, three))
    for k in m1:
        if k == "metrics":
            a, b = dict(m1[k]["short"]), dict(m3[k]["short"])
            a.pop("gates"), b.pop("gates")
            assert a == b
        elif k == "thresholds":
            assert m1[k]["buckets"] == m3[k]["buckets"] and m1[k]["gates"] == m3[k]["gates"]
        elif k != "trained_at":
            assert m1[k] == m3[k], k
    rows = json.loads(small_df.tail(40).to_json(orient="records", date_format="iso"))
    b1, b3 = Bundle(one), Bundle(three)
    for name in b1.models:
        np.testing.assert_array_equal(
            b1.models[name]["model"].predict(b1.features(rows, "short"), raw_score=True),
            b3.models[name]["model"].predict(b3.features(rows, "short"), raw_score=True))
    assert C.LGBM_SEED == 7                                        # restored after the retrains
