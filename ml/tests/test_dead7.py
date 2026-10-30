"""The dead7 trial (dead7_trial.py, scout_ml/dead7.py): synthetic data only, no database."""
import copy
import hashlib
import json
import re
from datetime import datetime, timezone

import numpy as np
import pandas as pd
import pytest

import dead7_trial
import make_synthetic as M
import train as train_mod
from scout_ml import config as C
from scout_ml import dead7 as T
from scout_ml import diagnostics as D
from scout_ml import holders as H

FAST = {"DEAD7_SEEDS": (7, 11, 23), "DEAD7_BOOT_REPS": 200}   # the suite's speed-up; values pinned below
NOW = datetime(2026, 10, 30, 6, 15, tzinfo=timezone.utc)
ADDRESS = re.compile(r"0x[0-9a-fA-F]{40}")


def fast(mp):
    for k, v in FAST.items():
        mp.setattr(C, k, v)


def write_inputs(folder, d, rows, ids, pilot) -> dict:
    p = {"csv": folder / "calls.csv", "holders": folder / "holders", "ids": folder / "ids.csv",
         "pilot": folder / "pilot.csv"}
    M.write_csv(d, p["csv"])
    M.write_dead7_trial(p["holders"], p["ids"], p["pilot"], rows, ids, pilot)
    return p


def load(p) -> tuple:
    ex = H.load_export(p["holders"])
    H.check_export(ex, C.HOLD_FEATURES)
    return (pd.read_csv(p["csv"], low_memory=False), ex, H.load_ids(p["ids"]), T.load_pilot_ids(p["pilot"]))


@pytest.fixture(scope="module")
def inputs(tmp_path_factory):
    """Synthetic calls with a planted sellers signal, written as the trial reads them."""
    d, rows, ids, pilot = M.make_dead7_trial(M.make(n=6000, days=70, seed=11))
    return write_inputs(tmp_path_factory.mktemp("dead7"), d, rows, ids, pilot)


@pytest.fixture(scope="module")
def loaded(inputs):
    return load(inputs)


@pytest.fixture(scope="module")
def assembled(loaded):
    view_df, ex, ids, pilot = loaded
    view, L = T.prepare_view(view_df)
    return view, T.assemble(view, L, ids, ex, pilot)


@pytest.fixture(scope="module")
def trial(loaded):
    mp = pytest.MonkeyPatch()
    fast(mp)
    try:
        yield T.analyse(*loaded, T.TRIAL)
    finally:
        mp.undo()


# --- settings ------------------------------------------------------------------------
def test_rules_are_the_approved_literal_values():
    assert C.DEAD7_RULES_SET_ON == "29 Oct 2026"
    assert (C.DEAD7_FEATURE, C.DEAD7_GROUP, C.DEAD7_HORIZON_DAYS) == ("pre_hold_sellers", "dead7", 7)
    assert C.DEAD7_SEEDS == (7, 11, 23, 31, 47, 61, 89) and C.DEAD7_SEED == 7
    assert (C.DEAD7_MIN_TRAIN_ROWS, C.DEAD7_MIN_TEST_ROWS, C.DEAD7_MIN_TEST_DEAD7, C.DEAD7_MIN_WEEKS) == (1000, 50, 5, 2)
    assert (C.DEAD7_MIN_MEDIAN_DELTA, C.DEAD7_MIN_DELTA_LOWER) == (0.0, 0.0)
    assert (C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED, C.DEAD7_CI) == (2000, 7, (2.5, 97.5))
    assert (C.DEAD7_SKIP_FRAC, C.DEAD7_MIN_CAUGHT_LOWER, C.DEAD7_MIN_KEPT_MINUS_ALL_LOWER) == (0.30, 0.30, 0.0)
    assert (C.DEAD7_MIN_RAW_AUC, C.DEAD7_PD_QUANTILES, C.DEAD7_MIN_PD_DELTA) == (0.5, (25, 75), 0.0)
    assert (C.DEAD7_MAX_ADV_AUC_RISE, C.DEAD7_DRIFT_TOP, C.DEAD7_DRIFT_MAX_SHARE) == (0.02, 3, 0.40)
    assert (C.DEAD7_CONFIRM_TRAIN_END, C.DEAD7_CONFIRM_TEST_START) == ("2026-10-09T00:00:00Z", "2026-10-16T00:00:00Z")
    assert (C.DEAD7_CONFIRM_MIN_DEAD7, C.DEAD7_CONFIRM_DEADLINE, C.DEAD7_ROUNDS) == (100, "31 Jan 2027", 150)


def test_check_settings_refuses_a_seed_outside_the_seeds(monkeypatch):
    T.check_settings()                                          # the shipped settings pass
    monkeypatch.setattr(C, "DEAD7_SEED", 8)
    with pytest.raises(H.Refused, match="DEAD7_SEED 8 must be one of DEAD7_SEEDS"):
        T.check_settings()


# --- result logic -----------------------------------------------------------------------
def rules(*statuses):
    return {f"R{i}": {"status": s} for i, s in enumerate(statuses)}


def test_result_fail_beats_na_and_na_is_not_judged():
    assert T.result_of(rules(T.PASS, T.NA, T.FAIL)) == T.FAIL         # FAIL even with an n/a
    assert T.result_of(rules(T.PASS, T.NA, T.PASS)) == T.NOT_JUDGED
    assert T.result_of(rules(T.PASS, T.PASS, T.PASS)) == T.PASS
    assert T.combine([T.PASS, T.NA]) == T.NA and T.combine([T.NA, T.FAIL]) == T.FAIL and T.combine([]) == T.NA
    assert T.result_line(T.PASS, T.TRIAL) == "provisional PASS: must hold again in the confirmation run"
    assert T.result_line(T.PASS, T.CONFIRM) == T.PASS and T.result_line(T.FAIL, T.TRIAL) == T.FAIL


def test_check_is_na_on_undefined_values():
    assert T.check(np.nan, lambda v: v > 0) == T.NA and T.check(None, lambda v: v > 0) == T.NA
    assert T.check(0.0, lambda v: v > 0) == T.FAIL and T.check(0.01, lambda v: v > 0) == T.PASS


# --- R0: which weeks count -----------------------------------------------------------------
@pytest.mark.parametrize("args, counted", [
    ((1000, 50, 5, 10, 10), True),       # every floor met exactly
    ((999, 50, 5, 10, 10), False),       # training rows
    ((1000, 49, 5, 10, 10), False),      # test rows
    ((1000, 50, 4, 10, 10), False),      # dead7 test rows
    ((1000, 50, 5, 0, 1000), False),     # one class in training: no model
])
def test_week_status_floors(args, counted):
    ok, why = T.week_status(*args)
    assert ok is counted and (why == "") is counted


def test_fewer_than_two_counted_weeks_is_not_judged(loaded, monkeypatch):
    monkeypatch.setattr(C, "DEAD7_MIN_TRAIN_ROWS", 10 ** 6)
    s = T.analyse(*loaded, T.TRIAL)
    assert s["result"] == T.NOT_JUDGED and s["rules"]["R0"]["status"] == T.NA
    assert all(s["rules"][k]["status"] == T.NA for k in ("R1", "R2", "R3", "R4", "R5"))
    assert not any(w["counted"] for w in s["weeks"]) and "pooled" not in s


def test_trial_weeks_counted_by_the_floors(trial):
    weeks = trial["weeks"]
    assert any(not w["counted"] and "train" in w["reason"] for w in weeks)   # early weeks: too little training
    for w in weeks:
        assert w["counted"] == T.week_status(w["n_train"], w["n_test"], w["n_dead7"], 1, 1)[0]
    assert trial["rules"]["R0"]["counted_weeks"] == sum(w["counted"] for w in weeks) >= 2


# --- rows: pilot, allowlist, drops ------------------------------------------------------------
def test_pilot_ids_are_never_test_rows(assembled):
    _, (data, _) = assembled
    rows, pilot = data["rows"], data["pilot"]
    assert pilot.sum() > 50
    windows = T.trial_windows(rows["message_date"], rows["contract_address"], data["y"], pilot)
    assert not any((w["test"] & pilot).any() for w in windows)
    assert any((w["train"] & pilot).any() for w in windows if w["counted"])    # they do train
    assert sum(w["pilot_removed"] for w in windows) > 0
    counted = [w for w in windows if w["counted"]][:2]
    pooled = T.score_blocks(rows, data["y"], counted, (C.DEAD7_SEED,))
    assert not pilot[pooled["idx"]].any()


def test_only_pre_hold_sellers_is_taken_from_the_export(assembled, loaded):
    view, (data, _) = assembled
    rows = data["rows"]
    assert set(rows.columns) - set(view.columns) == {C.DEAD7_FEATURE}
    assert not [c for c in rows.columns if c.startswith((C.HOLD_PREFIX, "post24h_")) and c != C.DEAD7_FEATURE]
    ex = loaded[1]
    by_id = {r["call_id"]: r for r in ex["rows"]}
    for cid, v in rows[["call_id", C.DEAD7_FEATURE]].head(200).itertuples(index=False):
        raw = by_id[cid][C.DEAD7_FEATURE]
        assert (np.isnan(v) and raw is None) or v == raw                # raw count, NULL stays NaN
    X = T.arm_inputs(rows, np.ones(len(rows), bool))
    assert list(X[T.WITHOUT].columns) == list(C.FEATURES)
    assert list(X[T.WITH].columns) == list(C.FEATURES) + [C.DEAD7_FEATURE]


def test_with_inputs_pass_the_leakage_guard(assembled, monkeypatch):
    _, (data, _) = assembled
    X = T.arm_inputs(data["rows"], np.ones(len(data["rows"]), bool))[T.WITHOUT]
    monkeypatch.setattr(C, "DEAD7_FEATURE", "post24h_sellers")      # a forbidden name: refused
    with pytest.raises(ValueError, match="forbidden"):
        T.with_sellers(X, data["rows"])


def test_train_still_refuses_pre_hold_inputs(monkeypatch, tmp_path, synthetic_df):
    monkeypatch.setattr(C, "FEATURES", list(C.FEATURES) + ["pre_hold_sellers"])
    with pytest.raises(ValueError, match="must not include the holder export columns"):
        train_mod.train(synthetic_df, tmp_path / "models", "v1")
    assert not (tmp_path / "models").exists()                 # nothing written
    with pytest.raises(H.Refused, match="must not include"):
        T.check_settings()


def test_every_drop_is_counted(assembled):
    _, (data, drops) = assembled
    lost = (drops["no_view_row"] + drops["not_usable_7d"] + drops["no_holder_row"]
            + sum(drops["not_analysable"].values()) + drops["no_label"])
    assert drops["ids_rows"] == lost + drops["analysed_rows"] == lost + len(data["rows"])
    assert (drops["no_view_row"], drops["not_usable_7d"]) == (4, 6)   # make_dead7_trial's defaults
    by_reason = drops["not_usable_7d_by_reason"]
    assert list(by_reason) == ["update post", "repeat", "no pool", "not USD", "extreme outcome", "missing 7d return"]
    assert sum(by_reason.values()) == drops["not_usable_7d"]
    # make(seed=11)'s first six unusable rows: one update post (also no pool), two repeats, two no pool, one not USD
    assert by_reason == {"update post": 1, "repeat": 2, "no pool": 2, "not USD": 1, "extreme outcome": 0,
                         "missing 7d return": 0}
    assert drops["export_rows_not_in_ids_ignored"] == 5 and drops["pilot_ids_not_in_ids"] == 2
    assert drops["no_holder_row"] > 0 and {"incomplete", "entry-block mismatch"} <= set(drops["not_analysable"])
    assert drops["sellers_null_analysed"] > 0 and drops["analysed_dead7"] == int(data["y"].sum())


def test_not_usable_reason_is_the_first_flag_in_order():
    f, t = False, True
    L = pd.DataFrame({"usable_medium": [t, f, f, f, f, f, f, f],
                      "update":        [f, t, f, f, f, f, f, f],
                      "repeat":        [f, t, t, f, f, f, f, f],
                      "no_pool":       [f, t, t, t, f, f, f, f],
                      "not_usd":       [f, f, f, t, t, f, f, f],
                      "extreme":       [f, f, f, f, t, t, f, f]})
    assert T.not_usable_reason(L).tolist() == [None, "update post", "repeat", "no pool", "not USD",
                                               "extreme outcome", "missing 7d return", "missing 7d return"]
    view = pd.DataFrame({"call_id": range(8)})
    assert T.not_usable_counts(view, L, [0, 1, 2, 6, 7]) == {   # a usable row is never counted
        "update post": 1, "repeat": 1, "no pool": 0, "not USD": 0, "extreme outcome": 0, "missing 7d return": 2}


def test_report_shows_the_drops_by_reason(trial):
    report = T.render_report({**H.clean(trial), "run": "x", "settings": T.settings()})
    by_reason = trial["drops"]["not_usable_7d_by_reason"]
    line = next(x for x in report.splitlines() if x.startswith("- dropped, not usable at 7d"))
    assert line.startswith(f"- dropped, not usable at 7d in the view: {trial['drops']['not_usable_7d']} (")
    assert all(f"{k} {v}" in line for k, v in by_reason.items()) and "update post 1" in line


@pytest.mark.parametrize("undefined", [None, np.nan])
def test_report_renders_undefined_metrics_as_na(trial, undefined):
    """After H.clean a NaN metric is None: the report prints n/a instead of raising."""
    s = copy.deepcopy({**H.clean(trial), "run": "x", "settings": T.settings()})
    s["rules"]["R3"]["with"]["kept_minus_all"] = undefined
    s["rules"]["R4"]["pd_mean"] = undefined
    s["rules"]["R5"]["with"]["top"] = [["holders", undefined]]
    lines = T.render_report(s).splitlines()
    assert next(x for x in lines if "kept minus all (mean net 7d %" in x).startswith(
        f"  - kept minus all (mean net 7d %, capped at {C.SIM_MAX_RET_PCT:g}) n/a, 95 % interval")
    assert f"of {C.DEAD7_FEATURE}: n/a (> 0)" in next(x for x in lines if "mean score change" in x)
    assert next(x for x in lines if "top drifting inputs (with)" in x).endswith("holders n/a")


# --- rules on the planted signal ------------------------------------------------------------------
def test_planted_sellers_signal_passes_the_trial(trial):
    r = trial["rules"]
    assert r["R1"]["median_delta"] > 0.05 and r["R1"]["status"] == T.PASS
    assert len(r["R1"]["deltas"]) == len(FAST["DEAD7_SEEDS"])
    assert r["R2"]["ci"][0] > 0 and r["R2"]["status"] == T.PASS and r["R2"]["week_block_ci"] is not None
    assert r["R4"]["raw_auc"] > 0.5 and r["R4"]["pd_mean"] > 0 and r["R4"]["status"] == T.PASS
    assert r["R3"]["parts"]["caught_vs_without"] == T.PASS
    assert trial["result"] in (T.PASS, T.FAIL)            # judged; R3 / R5 may go either way on noise


def test_r4_partial_dependence_follows_the_planted_sign(tmp_path, monkeypatch):
    d, rows, ids, pilot = M.make_dead7_trial(M.make(n=6000, days=70, seed=11), signal=-1.5)
    monkeypatch.setattr(C, "DEAD7_SEEDS", (C.DEAD7_SEED,))
    monkeypatch.setattr(C, "DEAD7_BOOT_REPS", 50)
    s = T.analyse(*load(write_inputs(tmp_path, d, rows, ids, pilot)), T.TRIAL)
    r4 = s["rules"]["R4"]
    assert r4["raw_auc"] < 0.5 and r4["pd_mean"] < 0 and r4["status"] == T.FAIL
    assert s["result"] == T.FAIL


def test_pd_scores_move_only_the_feature():
    rng = np.random.default_rng(1)
    X = pd.DataFrame({"a": rng.normal(size=400), C.DEAD7_FEATURE: rng.poisson(10, 400).astype(float)})
    y = (X[C.DEAD7_FEATURE] + rng.normal(0, 2, 400) > 10).astype(float).to_numpy()
    fitted = T.fit_arm(X, y, np.ones(400, bool), 7)
    s25, s75, q = T.pd_scores(fitted, X, np.arange(400) >= 300, X[C.DEAD7_FEATURE][:300])
    assert q == list(np.percentile(X[C.DEAD7_FEATURE][:300], (25, 75))) and q[0] < q[1]
    assert len(s25) == 100 and (s75 - s25).mean() > 0
    nan25, nan75, qn = T.pd_scores(fitted, X, np.arange(400) >= 300, np.full(10, np.nan))
    assert np.isnan(qn).all() and np.isnan(nan25).all() and np.isnan(nan75).all()


def test_drift_rule_reads_the_top_three_at_the_share():
    F = C.DEAD7_FEATURE
    assert T.drifting({"top": [["holders", 0.3], [F, 0.40]]}) == [[F, 0.40]]
    assert T.drifting({"top": [["a", 0.5], ["b", 0.2], ["c", 0.1], [F, 0.9]]}) == []     # 4th: not in the top 3
    assert T.drifting({"top": [[F, 0.39]]}) == [] and T.drifting({}) == []


# --- bootstraps ------------------------------------------------------------------------------
def test_group_weights_resample_within_each_group():
    g = np.array([0] * 5 + [1] * 7 + [2] * 3)
    w = T.group_weights(g, 100, 7)
    for name, size in ((0, 5), (1, 7), (2, 3)):
        assert (w[:, g == name].sum(1) == size).all()
    assert (T.group_weights(g, 100, 7) == w).all()          # seeded


def test_same_resample_for_both_arms():
    rng = np.random.default_rng(3)
    y, s = (rng.random(300) < 0.3).astype(float), rng.random(300)
    r2 = T.rule_r2(y, s, s, y.astype(int))
    assert r2["delta"] == 0 and r2["ci"] == [0.0, 0.0] and r2["status"] == T.FAIL


def test_r3_bootstrap_mirrors_the_collapse_filter_pooled_bootstrap(monkeypatch):
    """Week blocks drawn as diagnostics.pooled draws them: the same intervals as the collapse filter."""
    monkeypatch.setattr(C, "DEAD7_BOOT_REPS", 300)
    rng = np.random.default_rng(5)
    n, block = 600, np.repeat(np.arange(6), 100)
    y = (rng.random(n) < 0.35).astype(float)
    score = y * 0.5 + rng.random(n)
    ret = np.where(y == 1, -95.0, rng.normal(20, 300, n))
    runner = (ret >= 50).astype(float)
    mine = T.filter_block(y, runner, ret, T.filter_flags(score, block),
                          T.block_weights(block, C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED))
    weeks = [D.week_sums(runner[block == b], score[block == b], y[block == b], score[block == b],
                         ret[block == b]) for b in range(6)]
    ref = D.pooled(weeks, reps=C.DEAD7_BOOT_REPS, seed=C.DEAD7_BOOT_SEED, with_filter=True)
    for k in ("collapses_caught", "kept_minus_all"):
        assert mine[k] == pytest.approx(ref[f"filter_{k}"])
        assert mine[f"{k}_ci"] == pytest.approx([ref[f"filter_{k}_lo"], ref[f"filter_{k}_hi"]])


# --- confirm mode ---------------------------------------------------------------------------
def test_confirm_split_dates():
    dates = pd.Series(pd.to_datetime(["2026-10-08T23:59:59Z", "2026-10-09T00:00:00Z", "2026-10-15T23:59:59Z",
                                      "2026-10-16T00:00:00Z", "2026-10-20T00:00:00Z", "2026-08-01T00:00:00Z"]))
    pilot = np.array([False, False, False, False, True, True])
    sp = T.confirm_split(dates, pilot)
    assert sp["train"].tolist() == [True, False, False, False, False, True]     # pilot trains
    assert sp["test"].tolist() == [False, False, False, True, False, False]     # never a pilot id
    assert (sp["train_end"], sp["test_start"]) == ("2026-10-09 00:00", "2026-10-16 00:00")


def test_confirm_below_100_dead7_is_not_judged(tmp_path, monkeypatch):
    fast(monkeypatch)
    d, rows, ids, pilot = M.make_dead7_trial(M.make(n=3000, days=70, seed=11, end="2026-10-20T00:00:00Z"))
    s = T.analyse(*load(write_inputs(tmp_path, d, rows, ids, pilot)), T.CONFIRM)
    assert s["split"]["n_dead7"] < 100 and s["result"] == T.NOT_JUDGED == s["result_line"]
    na = s["rules"]["R1"]["na"]
    assert f"hold {s['split']['n_dead7']} dead7" in na and "needs 100 dead7; deadline 31 Jan 2027" in na
    assert "pooled" not in s and "R0" not in s["rules"]
    assert "needs 100 dead7; deadline 31 Jan 2027, then the owner decides" in T.render_report(
        {**H.clean(s), "run": "x", "settings": T.settings()})


def test_confirm_judges_one_split(tmp_path, monkeypatch):
    fast(monkeypatch)
    d, rows, ids, pilot = M.make_dead7_trial(M.make(n=6000, days=110, seed=11, end="2026-11-20T00:00:00Z"))
    s = T.analyse(*load(write_inputs(tmp_path, d, rows, ids, pilot)), T.CONFIRM)
    sp = s["split"]
    assert sp["n_dead7"] >= 100 and sp["pilot_in_train"] > 0
    r = s["rules"]
    assert "R0" not in r and "week_block_ci" not in r["R2"] and r["R1"]["status"] == T.PASS
    assert s["result"] in (T.PASS, T.FAIL) and s["result_line"] != T.PROVISIONAL
    assert s["adversarial_split"]["train_rows"] == sp["n_train"]
    assert s["limits"] == [T.CONFIRM_LIMIT, *T.LIMITS[1:]]           # limit (1) names the new calls


# --- end to end ---------------------------------------------------------------------------------
def cli_args(inputs, out) -> list:
    return ["--csv", str(inputs["csv"]), "--holders", str(inputs["holders"]), "--ids", str(inputs["ids"]),
            "--pilot-ids", str(inputs["pilot"]), "--out", str(out)]


def tree_hashes(root) -> dict:
    return {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in sorted(root.rglob("*")) if p.is_file()}


def test_cli_writes_the_report_and_no_model_files(inputs, trained, tmp_path, monkeypatch):
    fast(monkeypatch)
    models_root = trained[0]
    before, inputs_before = tree_hashes(models_root), tree_hashes(inputs["csv"].parent)
    out = tmp_path / "out"
    argv = cli_args(inputs, out)
    assert dead7_trial.main(argv, now=NOW) == 0
    dest = out / "20261030-0615"
    assert sorted(p.name for p in out.rglob("*")) == ["20261030-0615", "report.md", "summary.json"]
    assert tree_hashes(models_root) == before                       # the saved model is byte-identical
    assert tree_hashes(inputs["csv"].parent) == inputs_before       # inputs untouched, nothing written there
    assert not list(tmp_path.rglob("*.joblib")) and not list(tmp_path.rglob("LATEST"))
    assert not list(tmp_path.rglob("CANDIDATE"))
    report = (dest / "report.md").read_text(encoding="utf-8")
    summary = json.loads((dest / "summary.json").read_text(encoding="utf-8"))
    assert summary["mode"] == "trial" and summary["result"] in (T.PASS, T.FAIL, T.NOT_JUDGED)
    for text in ("Known limits", "confirmation run", "looks past the entry block", "nothing is saved or served",
                 "**Result: ", "R3 test the filter itself", "R1 and R2 test sellers", "Spearman of pre_hold_sellers",
                 "pilot in train", "week-block interval", "set on 29 Oct 2026"):
        assert text in report
    assert not ADDRESS.search(report) and not ADDRESS.search(json.dumps(summary))
    assert dead7_trial.main(argv, now=NOW) == H.EXIT_REFUSED       # an existing run folder is never overwritten


def test_cli_refuses_out_inside_the_repo_outside_data(inputs, capsys):
    out = dead7_trial.REPO_ROOT / "ml" / "dead7_out_never"
    assert dead7_trial.main(cli_args(inputs, out), now=NOW) == H.EXIT_REFUSED
    assert "outside its data/ folder" in capsys.readouterr().err and not out.exists()


def test_cli_usage_error():
    with pytest.raises(SystemExit) as e:
        dead7_trial.main(["--mode", "confirm"])                     # --csv is required
    assert e.value.code == 2
