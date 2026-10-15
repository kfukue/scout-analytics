"""train.py --variants: one-change variants, report only; and the runner
"tradeable" rule they use. Synthetic data only."""
import json

import numpy as np
import pandas as pd
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml import validate as V
from scout_ml.features import assert_no_leakage
from scout_ml.labels import build_labels
from scout_ml.model import Bundle

DEX_VARIANTS = ("raw dex instead of dex_family", "raw dex + dex_family")
WITH_CALLS_PREV = "with calls_prev counts (default before 14 Oct)"
WALLET = "wallet reputation (wrep_*)"
QUALITY = "wallet reputation, quality only"
CALLS_PREV = {"calls_prev_1h", "calls_prev_24h"}


def _row(**kw):
    base = {"price_unit": "usd", "entry_late_price_usd": 1.0, "tracking_status": "done",
            "tax_buy_pct": None, "tax_sell_pct": None, "rugged": "false",
            "ret_late_3d": 60, "ret_late_7d": 60, "ret_late_30d": 10}
    return {**base, **kw}


def _tradeable_df():
    # all four are runners by the plain label in short/3day/medium/long
    return pd.DataFrame([_row(max_gain_late_1d=219, ret_late_1d=219, trades_24h=52),
                         _row(max_gain_late_1d=219, ret_late_1d=219, trades_24h=100),
                         _row(max_gain_late_1d=219, ret_late_1d=219, trades_24h=None),
                         _row(max_gain_late_1d=219, ret_late_1d=219, trades_24h=5000)])


def test_runner_tradeable_rule_is_off_by_default():
    assert C.RUNNER_NEEDS_TRADES is False and C.RUNNER_MIN_TRADES_24H == 100
    L = build_labels(_tradeable_df())
    for b in C.BUCKETS:
        assert L[f"runner_{b}"].tolist() == [1, 1, 1, 1], b


def test_runner_tradeable_rule_needs_trades_and_null_keeps_the_label():
    df = _tradeable_df()
    plain = build_labels(df)
    with C.overrides(RUNNER_NEEDS_TRADES=True):
        L = build_labels(df)
    for b in C.BUCKETS:
        assert L[f"runner_{b}"].tolist() == [0, 1, 1, 1], b     # 52 < 100; NULL keeps it
        # only the runner label changes: usable rows, collapses, returns (simulation) do not
        for col in (f"usable_{b}", f"collapse_{b}", f"collapse_plain_{b}", f"net_ret_{b}"):
            pd.testing.assert_series_equal(L[col], plain[col])
    with C.overrides(RUNNER_NEEDS_TRADES=True):          # no trades_24h column: label as before
        L = build_labels(df.drop(columns="trades_24h"))
    assert L["runner_short"].tolist() == [1, 1, 1, 1]


def test_each_variant_changes_exactly_one_knob():
    base = {k: getattr(C, k) for k in C.VARIANT_KNOBS}
    assert C.VARIANTS["baseline"] == {}
    assert list(C.VARIANTS) == ["baseline", "dead rule on (collapse OR dead)",
                                "raw dex instead of dex_family", "raw dex + dex_family",
                                "runners must be tradeable", "stronger regularisation",
                                "with calls_prev counts (default before 14 Oct)", WALLET, QUALITY]
    # the one two-knob variant: the coverage names are inputs only with WALLET_REP on
    assert C.VARIANTS[QUALITY] == {"WALLET_REP": True,
                                   "EXCLUDED_INPUTS": C.EXCLUDED_INPUTS + C.WREP_COVERAGE}
    for name, knobs in C.VARIANTS.items():
        if name in ("baseline", QUALITY):
            continue
        assert len(knobs) == 1, name
        (k, v), = knobs.items()
        assert k in C.VARIANT_KNOBS and v != base[k], name
        settings = C.variant_settings(name)
        assert [x for x in C.VARIANT_KNOBS if settings[x] != base[x]] == [k], name
    assert C.DEAD_IS_COLLAPSE is False                         # the baseline: plain collapse
    assert C.VARIANTS["dead rule on (collapse OR dead)"] == {"DEAD_IS_COLLAPSE": True}
    assert not any("DEAD_TRADES_24H" in k for k in C.VARIANTS.values())   # needs the rule on
    assert set(C.TRADES_VARIANTS) == {"dead rule on (collapse OR dead)", "runners must be tradeable"}
    # the regularisation variant: one knob, a named set of stronger LightGBM settings
    assert C.VARIANTS["stronger regularisation"] == {"LGBM_REGULARISATION": "strong"}
    assert C.LGBM_REGULARISATION == "default" and C.LGBM_REG_SETS["default"] == {}
    strong, base_p = C.LGBM_REG_SETS["strong"], C.LGBM_PARAMS
    assert strong["num_leaves"] < base_p["num_leaves"]
    assert strong["min_child_samples"] > base_p["min_child_samples"]
    assert strong["reg_lambda"] > base_p["reg_lambda"]
    assert strong["colsample_bytree"] < base_p["colsample_bytree"]


def test_overrides_rebuild_features_never_leak_and_restore():
    before = (list(C.FEATURES), list(C.CATEGORICAL), C.DEX_INPUTS, C.DEAD_IS_COLLAPSE)
    want = {"baseline": ({"dex_family"}, {"dex"}),
            "raw dex instead of dex_family": ({"dex"}, {"dex_family"}),
            "raw dex + dex_family": ({"dex", "dex_family"}, set())}
    for name, knobs in C.VARIANTS.items():
        with C.overrides(**knobs):
            assert_no_leakage(C.FEATURES)                       # trades_24h etc. never inputs
            assert "trades_24h" not in C.FEATURES
            assert C.FEATURES == C.NUMERIC_FEATURES + C.CATEGORICAL
            assert C.LOG1P_FEATURES == [c for c in C.NUMERIC_FEATURES if c not in C.NO_LOG]
            dropped = set(C.EXCLUDED_INPUTS)                    # the effective value here
            numeric = C.NUMERIC_RAW + C.DERIVED
            if knobs.get("CALLS_PREV_INPUTS") == "burst":   # the raw counts replaced in place
                numeric = [C.CALLS_PREV_BURST if c == "calls_prev_1h" else c
                           for c in numeric if c != "calls_prev_24h"]
            if knobs.get("WALLET_REP"):                     # the wrep_* columns appended
                numeric = numeric + list(C.WREP_FEATURES)
            assert C.NUMERIC_FEATURES == [c for c in numeric if c not in dropped]
            has, lacks = want.get(name, want["baseline"])
            assert has <= set(C.CATEGORICAL) and not lacks & set(C.CATEGORICAL), name
    with pytest.raises(RuntimeError):
        with C.overrides(DEX_INPUTS="both", DEAD_IS_COLLAPSE=False):
            raise RuntimeError("boom")
    assert (list(C.FEATURES), list(C.CATEGORICAL), C.DEX_INPUTS, C.DEAD_IS_COLLAPSE) == before
    with pytest.raises(ValueError):
        with C.overrides(GATES={}):                             # gates are never a variant knob
            pass
    with pytest.raises(ValueError):
        with C.overrides(DEX_INPUTS="nonsense"):
            pass
    assert list(C.FEATURES) == before[0]


@pytest.fixture(scope="module")
def small_df():
    return make_synthetic.make(n=2500, days=60, seed=5)


@pytest.fixture(scope="module")
def plain_and_variants(small_df, tmp_path_factory):
    """The same data trained twice: plain, and with --variants (counting saved files)."""
    plain = train_mod.train(small_df, tmp_path_factory.mktemp("plain"), "v1")
    dumped = []
    real_dump = train_mod.joblib.dump
    train_mod.joblib.dump = lambda obj, path, *a, **k: (dumped.append(str(path)),
                                                        real_dump(obj, path, *a, **k))
    try:
        var = train_mod.train(small_df, tmp_path_factory.mktemp("var"), "v1", variants=True)
    finally:
        train_mod.joblib.dump = real_dump
    return plain, var, dumped


def test_variants_are_not_saved_and_the_saved_model_is_the_baseline(plain_and_variants, small_df):
    plain, var, dumped = plain_and_variants
    files = sorted(p.name for p in plain.iterdir())
    assert sorted(p.name for p in var.iterdir()) == files
    n_models = len([f for f in files if f.endswith(".joblib")])
    assert n_models >= 4 and len(dumped) == n_models          # only the baseline run saved models
    mp, mv = (json.loads((d / "meta.json").read_text()) for d in (plain, var))
    for k in mp:
        if k not in ("trained_at",):
            assert mv[k] == mp[k], k
    assert set(mv) - set(mp) == {"variants"}
    # same scores from both saved models
    df = small_df.sort_values("message_date").head(50)
    rows = json.loads(df.to_json(orient="records", date_format="iso"))
    bp, bv = Bundle(plain), Bundle(var)
    for b in mp["runner_reference"]:
        np.testing.assert_array_equal(
            bp.models[f"{b}_runner"]["model"].predict(bp.features(rows, b), raw_score=True),
            bv.models[f"{b}_runner"]["model"].predict(bv.features(rows, b), raw_score=True))
    assert list(C.FEATURES) == mv["features"]                 # config restored after the variants


def test_variant_table_contents(plain_and_variants):
    _, var, _ = plain_and_variants
    meta = json.loads((var / "meta.json").read_text())
    v = meta["variants"]
    assert list(v) == list(C.VARIANTS)
    base = v["baseline"]["buckets"]
    for b, m in meta["metrics"].items():
        if m.get("skipped"):
            assert base[b] == {"skipped": m["skipped"]}
            continue
        # the baseline row is the saved model's own numbers
        assert base[b]["top_lift"] == m["trading"]["top_lift"]
        assert base[b]["collapse_removed"] == m["trading"]["collapse_removed"]
        # the variant gate columns are the old single-run gates, also for the baseline row
        assert base[b]["windows_beating"] == m["gates"]["old"]["windows_beating"]
        assert base[b]["passed"] == m["gates"]["old"]["passed"]
        # the baseline's pooled walk-forward column is the gate's (saved seed)
        assert base[b]["wf_pooled_lift"] == m["gates"]["pooled_lift"]
        assert base[b]["wf_pooled_lift_lo"] == m["gates"]["pooled_lift_lo"]
        assert base[b]["wf_pooled_sim_gain_lo"] == m["gates"]["sim_diff_lo"]
        assert base[b]["runner_auc_lightgbm"] == m["labels"]["runner"]["lightgbm"]["roc_auc"]
        # logistic uses numeric inputs only and no LightGBM setting: unchanged by these variants
        for name in DEX_VARIANTS + ("stronger regularisation",):   # numeric inputs unchanged
            s = v[name]["buckets"][b]
            assert s["runner_auc_logistic"] == base[b]["runner_auc_logistic"], (name, b)
            assert s["collapse_auc_plain_logistic"] == base[b]["collapse_auc_plain_logistic"]
        # the baseline (dead rule off) trains on the plain label; the "on" variant on collapse
        # OR dead, while its plain-label numbers stay comparable
        assert base[b]["collapse_auc_trained_lightgbm"] == base[b]["collapse_auc_plain_lightgbm"]
        on = v["dead rule on (collapse OR dead)"]["buckets"][b]
        assert on["collapse_auc_trained_lightgbm"] != on["collapse_auc_plain_lightgbm"]
        assert on["runner_auc_lightgbm"] == base[b]["runner_auc_lightgbm"]   # runners untouched
        trd = v["runners must be tradeable"]["buckets"][b]
        assert trd["runner_rate_test"] <= base[b]["runner_rate_test"]
        assert trd["sim_all_mean"] == base[b]["sim_all_mean"]   # simulation still on all calls
        assert trd["collapse_auc_trained_lightgbm"] == base[b]["collapse_auc_trained_lightgbm"]
        for name in C.VARIANTS:
            if v[name].get("not_applicable"):                       # the wallet variant here
                continue
            s = v[name]["buckets"][b]
            for k in ("top_lift", "top_lift_lo", "top_lift_hi", "runner_auc_lightgbm",
                      "runner_auc_logistic", "collapse_auc_plain_lightgbm",
                      "collapse_auc_plain_logistic", "collapse_removed", "windows_beating",
                      "windows_evaluated", "sim_top_mean", "sim_all_mean", "passed",
                      "wf_pooled_lift", "wf_pooled_lift_lo", "wf_pooled_sim_gain",
                      "wf_pooled_sim_gain_lo"):
                assert k in s, (name, b, k)
            if s["wf_pooled_lift_lo"] is not None:                 # null with < 2 weeks
                assert s["wf_pooled_lift_lo"] <= s["wf_pooled_lift"] <= s["wf_pooled_lift_hi"]
        # with the calls_prev inputs back the model differs, the rows and simulation do not
        wcp = v[WITH_CALLS_PREV]["buckets"][b]
        assert wcp["sim_all_mean"] == base[b]["sim_all_mean"]
        assert wcp["runner_auc_logistic"] != base[b]["runner_auc_logistic"]   # more inputs
        # "Variants across seeds": the baseline and the SEED_VARIANTS that ran only
        seeds = [str(s) for s in C.STABILITY_SEEDS]
        for name in C.VARIANTS:
            has = name == "baseline" or (name in C.SEED_VARIANTS
                                         and not v[name].get("not_applicable"))
            assert ("across_seeds" in v[name]) == has, name
            if not has:
                continue
            a = v[name]["across_seeds"]["buckets"][b]
            assert list(a["lifts"]) == seeds
            # the saved seed's lift is the variant's own pooled walk-forward lift
            own = v[name]["buckets"][b]["wf_pooled_lift"]
            assert a["lifts"][str(C.LGBM_SEED)] == (None if own is None else pytest.approx(own))
            for k in ("lift", "lift_lo", "lift_hi", "sim_gain", "sim_gain_lo", "sim_gain_hi"):
                assert a[k] == v[name]["buckets"][b][f"wf_pooled_{k}"], (name, b, k)
            adv = a["adversarial"]
            assert "auc" in adv and "error" not in adv
            assert len(adv["top"]) <= max(3, C.WREP_ADOPTION["drift_top"])
            assert not {f for f, _ in adv["top"]} & set(C.ADV_EXCLUDE)
        # baseline row = the gates' own numbers (nothing retrained)
        ab = v["baseline"]["across_seeds"]["buckets"][b]
        assert ab["lifts"] == m["gates"]["pooled_lift_seeds"]
        assert ab["median"] == m["gates"]["pooled_lift_median"]
        assert ab["sim_gain_lo"] == m["gates"]["sim_diff_lo"]
        # each variant's adversarial probe sees only its own inputs
        for name, gone in (("baseline", {"calls_prev_1h", "calls_prev_24h", C.CALLS_PREV_BURST}),
                           (WITH_CALLS_PREV, {C.CALLS_PREV_BURST})):
            top = {f for f, _ in v[name]["across_seeds"]["buckets"][b]["adversarial"]["top"]}
            assert not top & gone, (name, b, top)
    ran = [n for n in C.VARIANTS if n != "baseline" and not v[n].get("not_applicable")]
    assert WALLET not in ran and QUALITY not in ran            # n/a: no --live-buys here
    assert all(v[n].get("seconds") is not None for n in ran)
    for n in C.SEED_VARIANTS:
        if n in ran:
            assert v[n]["across_seeds"]["seconds"] <= v[n]["seconds"]
    report = (var / "report.md").read_text(encoding="utf-8")
    assert "## Variant comparison (report only; not saved, not served)" in report
    assert "risks fitting the test period" in report and "walk-forward weeks" in report
    for name in C.VARIANTS:
        assert f"| {name} |" in report
    assert "| single-run (old) gate, information |" in report
    assert "pooled WF lift [95%]; sim difference [95%] (seed 7 only; information)" in report
    assert f"| {WITH_CALLS_PREV} | EXCLUDED_INPUTS = () |" in report
    assert "no calls_prev inputs" not in report and "burst ratio" not in report
    # no --live-buys: the wallet variant is n/a, also in the across-seeds table
    assert v[WALLET] == {"changes": {"WALLET_REP": True},
                         "not_applicable": train_mod.WREP_NOT_APPLICABLE}
    assert v[QUALITY] == {"changes": {"WALLET_REP": True,     # meta.json: the tuple as a list
                                      "EXCLUDED_INPUTS": list(C.EXCLUDED_INPUTS + C.WREP_COVERAGE)},
                          "not_applicable": train_mod.WREP_NOT_APPLICABLE}
    assert "wallet_rep" not in meta
    across = report[report.index("### Variants across seeds (report only)"):]
    assert "calls_prev variants across seeds" not in report
    assert f"| short | {WALLET} | n/a: no live-buy wallet data" in across
    assert f"| short | {QUALITY} | n/a: no live-buy wallet data" in across
    assert "| median (as the gate) |" in across and "| adversarial AUC (train vs test) |" in across
    for b in meta["metrics"]:
        for name in ("baseline",) + C.SEED_VARIANTS:
            assert f"| {b} | {name} |" in across
    assert "| stronger regularisation |" not in across
    plain_report = (plain_and_variants[0] / "report.md").read_text(encoding="utf-8")
    assert "Variant comparison" not in plain_report


def test_variant_walk_forward_summary_uses_counted_weeks_only():
    """The walk-forward means of the variant table leave out the weeks the gate does
    not count (training part below WF_MIN_TRAIN_ROWS), as the gate does."""
    from scout_ml import validate as V
    big, small = C.WF_MIN_TRAIN_ROWS, C.WF_MIN_TRAIN_ROWS - 1
    weeks = [{"n_train": small, "top_lift": 0.0, "collapse_removed": 0.0, "sim_beats_all": False},
             {"n_train": big, "top_lift": 3.0, "collapse_removed": 0.6, "sim_beats_all": True},
             {"n_train": big + 1, "top_lift": 2.0, "collapse_removed": 0.4, "sim_beats_all": True},
             {"n_train": big, "skipped": "too few"}]
    trading = {"top_lift": 2.5, "top_lift_lo": 1.5, "top_lift_hi": 3.5, "top_pos": 5, "top_n": 10,
               "all_pos": 20, "all_n": 100, "collapse_removed": 0.5, "sim_top_mean": 5.0,
               "sim_all_mean": 1.0}
    nan = {"roc_auc": np.nan}
    r = {"trading": trading, "gates": V.gates(trading, weeks), "walk_forward": weeks,
         "labels": {"runner": {"lightgbm": nan, "logistic": nan},
                    "collapse": {"lightgbm": nan, "logistic": nan}}}
    s = train_mod.variant_summary(r)
    assert s["wf_mean_lift"] == pytest.approx(2.5) and s["wf_mean_collapse_removed"] == pytest.approx(0.5)
    assert (s["windows_beating"], s["windows_evaluated"]) == (2, 2) and s["passed"]


def test_trades_variants_not_applicable_without_trades_24h(small_df, tmp_path, monkeypatch):
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline",) + C.TRADES_VARIANTS})
    calls = []
    real = train_mod._train_bucket
    monkeypatch.setattr(train_mod, "_train_bucket", lambda *a, **k: calls.append(a[0]) or real(*a, **k))
    out = train_mod.train(small_df.drop(columns="trades_24h"), tmp_path, "v1", variants=True)
    assert len(calls) == len(C.BUCKETS)                       # the baseline run only
    meta = json.loads((out / "meta.json").read_text())
    for name in C.TRADES_VARIANTS:
        assert "no trades_24h column" in meta["variants"][name]["not_applicable"]
    assert "n/a: the data has no trades_24h column" in (out / "report.md").read_text(encoding="utf-8")


def test_cli_passes_variants(monkeypatch, tmp_path):
    seen = {}
    monkeypatch.setattr(train_mod, "train", lambda df, out, variants=False, live_buys=None:
                        seen.update(variants=variants, live_buys=live_buys) or tmp_path)
    monkeypatch.setattr(train_mod.pd, "read_csv", lambda *a, **k: pd.DataFrame())
    assert train_mod.main(["--csv", "x.csv", "--out", str(tmp_path), "--variants"]) == 0
    assert seen == {"variants": True, "live_buys": None}
    assert train_mod.main(["--csv", "x.csv", "--out", str(tmp_path)]) == 0
    assert seen == {"variants": False, "live_buys": None}
    assert train_mod.main(["--csv", "x.csv", "--out", str(tmp_path), "--variants",
                           "--live-buys", "lb.csv"]) == 0
    assert seen == {"variants": True, "live_buys": "lb.csv"}   # read (as text) inside train


def _main_with_failing_variants(small_df, tmp_path, monkeypatch, fail_when):
    """Run the real CLI (short bucket only) with the baseline plus two DEX variants;
    _train_bucket raises while `fail_when()` is true (never during the baseline)."""
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline",) + DEX_VARIANTS})
    real = train_mod._train_bucket

    def flaky(*a, **k):
        if fail_when():
            raise RuntimeError("boom | second part\nTraceback line that must not appear")
        return real(*a, **k)

    monkeypatch.setattr(train_mod, "_train_bucket", flaky)
    csv = tmp_path / "calls.csv"
    make_synthetic.write_csv(small_df, csv)
    before = (list(C.FEATURES), list(C.CATEGORICAL), C.DEX_INPUTS)
    code = train_mod.main(["--csv", str(csv), "--out", str(tmp_path / "models"), "--variants"])
    assert (list(C.FEATURES), list(C.CATEGORICAL), C.DEX_INPUTS) == before   # config restored
    root = tmp_path / "models"
    vdir = root / (root / "LATEST").read_text().strip()
    return code, vdir


def test_a_crashing_variant_is_recorded_and_the_run_still_succeeds(small_df, tmp_path,
                                                                   monkeypatch, capsys):
    code, vdir = _main_with_failing_variants(
        small_df, tmp_path, monkeypatch, lambda: C.DEX_INPUTS == "both")
    assert code == 0                                         # gates/variants never fail the run
    out = capsys.readouterr().out
    assert "WARNING: variant 'raw dex + dex_family' failed (error: RuntimeError: boom / second part)" in out
    assert "all 2 variants failed" not in out
    meta = json.loads((vdir / "meta.json").read_text())
    assert (vdir / "short_runner.joblib").exists() and "short_runner" in meta["models"]
    v = meta["variants"]
    assert v["raw dex + dex_family"]["error"] == "error: RuntimeError: boom / second part"
    assert "buckets" not in v["raw dex + dex_family"]
    good = v["raw dex instead of dex_family"]["buckets"]["short"]
    assert "error" not in v["raw dex instead of dex_family"] and "top_lift" in good
    report = (vdir / "report.md").read_text(encoding="utf-8")
    assert "| raw dex + dex_family | DEX_INPUTS = both | error: RuntimeError: boom / second part |" in report
    assert "Traceback line" not in report
    assert "**Bucket result:" in report                          # the baseline report is intact


def test_all_variants_failing_still_exits_0_with_a_clear_warning(small_df, tmp_path,
                                                                monkeypatch, capsys):
    code, vdir = _main_with_failing_variants(
        small_df, tmp_path, monkeypatch, lambda: C.DEX_INPUTS != "family")
    assert code == 0                                         # gates/variants never fail the run
    out = capsys.readouterr().out
    assert "WARNING: all 2 variants failed" in out
    meta = json.loads((vdir / "meta.json").read_text())
    assert "short_runner" in meta["models"]
    assert all(meta["variants"][n]["error"].startswith("error: RuntimeError") for n in DEX_VARIANTS)
    assert (meta["variants"]["baseline"]["buckets"]["short"]["passed"]
            == meta["metrics"]["short"]["gates"]["old"]["passed"])     # the single-run gate


def test_variant_error_text_is_short_and_table_safe():
    from scout_ml.report import variant_error
    assert variant_error(ValueError("a | b\nmore")) == "error: ValueError: a / b"
    assert variant_error(KeyError()) == "error: KeyError"
    long = variant_error(RuntimeError("x" * 500))
    assert long.startswith("error: RuntimeError: xxx") and len(long) <= len("error: ") + 150


# --- calls_prev inputs: left out by default since 14 Oct 2026 -----------------------

def test_default_leaves_out_calls_prev_and_the_variant_puts_them_back(synthetic_df):
    from scout_ml.features import build_features
    from scout_ml.model import fit_baseline
    assert C.EXCLUDED_INPUTS == ("calls_prev_1h", "calls_prev_24h")   # the saved model
    assert not CALLS_PREV & set(C.FEATURES) and not CALLS_PREV & set(C.LOG1P_FEATURES)
    assert CALLS_PREV <= set(C.NUMERIC_RAW)                     # still read from the view
    knobs = C.VARIANTS[WITH_CALLS_PREV]
    assert knobs == {"EXCLUDED_INPUTS": ()}
    assert WITH_CALLS_PREV in C.SEED_VARIANTS
    before = (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES))
    rows = synthetic_df.head(400)
    with C.overrides(**knobs):
        assert CALLS_PREV <= set(C.NUMERIC_FEATURES) and CALLS_PREV <= set(C.LOG1P_FEATURES)
        assert set(C.FEATURES) - set(before[0]) == CALLS_PREV      # nothing else added
        assert not set(before[0]) - set(C.FEATURES)               # and nothing removed
        assert C.FEATURES == C.NUMERIC_RAW + C.DERIVED + C.CATEGORICAL   # the old input list
        assert_no_leakage(C.FEATURES)
        X = build_features(rows, {})
        assert list(X.columns) == C.FEATURES
        y = (np.arange(len(X)) % 3 == 0).astype(int)
        fit_baseline(X, y)                                       # the logistic baseline follows
    assert (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES)) == before
    X = build_features(rows, {})
    assert list(X.columns) == C.FEATURES and not CALLS_PREV & set(X.columns)
    # it can only remove numeric inputs: an outcome, a categorical or an unknown name raises
    for bad in (("ret_late_1d",), ("dex_family",), ("nonsense",)):
        with pytest.raises(ValueError, match="EXCLUDED_INPUTS"):
            with C.overrides(EXCLUDED_INPUTS=bad):
                pass
        assert list(C.FEATURES) == before[0]


# --- the CALLS_PREV_INPUTS = "burst" code path (kept; no variant uses it since 14 Oct) ---

def test_burst_setting_replaces_exactly_the_raw_counts(synthetic_df):
    from scout_ml.features import build_features
    from scout_ml.model import fit_baseline
    assert C.CALLS_PREV_INPUTS == "counts"                      # the saved model: no burst ratio
    assert not any(k.get("CALLS_PREV_INPUTS") == "burst" for k in C.VARIANTS.values())
    before = (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES))
    with C.overrides(EXCLUDED_INPUTS=()):
        counts = list(C.FEATURES)
    want = [C.CALLS_PREV_BURST if c == "calls_prev_1h" else c
            for c in counts if c != "calls_prev_24h"]
    rows = synthetic_df.head(400)
    with C.overrides(CALLS_PREV_INPUTS="burst", EXCLUDED_INPUTS=()):
        assert C.FEATURES == want                                 # in place, nothing else changed
        assert C.CALLS_PREV_BURST in C.LOG1P_FEATURES             # non-negative, skewed
        assert_no_leakage(C.FEATURES)
        X = build_features(rows, {})
        assert list(X.columns) == want
        np.testing.assert_allclose(
            X[C.CALLS_PREV_BURST].to_numpy(),
            (rows["calls_prev_1h"] * 24 / np.maximum(rows["calls_prev_24h"], 1)).to_numpy(float))
        fit_baseline(X, (np.arange(len(X)) % 3 == 0).astype(int))   # the logistic follows
        with C.overrides(LGBM_SEED=11):                            # nested (the seed retrains)
            assert C.FEATURES == want
        assert C.FEATURES == want
    assert (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES)) == before
    # with the default exclusion of the raw counts: no error, the ratio in their place
    with C.overrides(CALLS_PREV_INPUTS="burst"):
        assert C.FEATURES == want and not CALLS_PREV & set(C.FEATURES)
    with C.overrides(CALLS_PREV_INPUTS="burst", EXCLUDED_INPUTS=(C.CALLS_PREV_BURST,)):
        assert C.FEATURES == before[0]                             # neither ratio nor counts
    for bad in ("nonsense", "raw", None):
        with pytest.raises(ValueError, match="CALLS_PREV_INPUTS"):
            with C.overrides(CALLS_PREV_INPUTS=bad):
                pass
        assert list(C.FEATURES) == before[0]


# --- "Variants across seeds" (report only) -------------------------------------------

def test_every_variant_applies_without_error_and_seed_variants_are_variants():
    assert isinstance(C.SEED_VARIANTS, tuple) and C.SEED_VARIANTS == (WITH_CALLS_PREV, WALLET, QUALITY)
    assert all(n in C.VARIANTS for n in C.SEED_VARIANTS)
    for name in C.VARIANTS:
        with C.overrides(**C.VARIANTS[name]):
            assert_no_leakage(C.FEATURES)


def test_seed_median_is_taken_as_the_gate_takes_it(monkeypatch):
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7, 11, 23))
    s = train_mod.seed_median({7: 2.0, 11: 1.0, 23: 3.5})
    assert s["lifts"] == {"7": 2.0, "11": 1.0, "23": 3.5} and s["median"] == 2.0 and not s["why"]
    s = train_mod.seed_median({7: 2.0, 11: "retrain failed (error: X)", 23: float("nan")})
    assert s["lifts"] == {"7": 2.0, "11": None, "23": None} and np.isnan(s["median"])
    assert s["why"] == {"11": "retrain failed (error: X)", "23": "undefined"}
    assert np.isnan(train_mod.seed_median({7: 2.0})["median"])        # a seed not run: n/a


@pytest.mark.parametrize("seed_lifts", [
    {7: 2.0, 11: 1.0, 23: 3.5},
    {7: 2.0, 11: "retrain failed (error: X)", 23: 3.5},
    {7: 2.0, 11: 1.0, 23: float("nan")},
    {7: 2.0, 11: 1.0},
])
def test_seed_median_and_the_gate_share_one_median(monkeypatch, seed_lifts):
    """The across-seeds table and the runner-lift gate take the same lifts and
    median from the same input (one helper, V.seed_lift_median)."""
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7, 11, 23))
    s = train_mod.seed_median(seed_lifts)
    g = V.pooled_gates({}, {"collapse_removed": 0.5}, None, seed_lifts)
    assert s["lifts"] == g["pooled_lift_seeds"]
    assert (np.isnan(s["median"]) and np.isnan(g["pooled_lift_median"])
            or s["median"] == g["pooled_lift_median"])
    for k, why in s["why"].items():
        assert f"pooled lift of seed {k}: {why}" in g["reasons"]
    assert not any(r.startswith("pooled lift of seed") and r.split()[4].rstrip(":") not in s["why"]
                   for r in g["reasons"])


def test_seed_median_and_the_across_seeds_table_follow_the_shipped_seeds(monkeypatch,
                                                                       shipped_seeds):
    """With the shipped STABILITY_SEEDS: the table's median and the gate's are the
    middle value of every seed's lift, and the table shows every seed."""
    from scout_ml import report as R
    monkeypatch.setattr(C, "STABILITY_SEEDS", shipped_seeds)
    n = len(shipped_seeds)
    seed_lifts = {s: 1.0 + 0.25 * ((3 * i) % n) for i, s in enumerate(shipped_seeds)}
    s = train_mod.seed_median(seed_lifts)
    g = V.pooled_gates({}, {"collapse_removed": 0.5}, None, seed_lifts)
    assert list(s["lifts"]) == [str(x) for x in shipped_seeds]
    assert s["median"] == pytest.approx(sorted(seed_lifts.values())[n // 2])
    assert s["median"] == g["pooled_lift_median"] and s["lifts"] == g["pooled_lift_seeds"]
    row = {**s, "weeks": 5, "lift": 1.5, "lift_lo": 1.1, "lift_hi": 2.0, "sim_gain": 3.0,
           "sim_gain_lo": 1.0, "sim_gain_hi": 5.0, "adversarial": {"auc": 0.6, "top": []}}
    across = {"seeds": list(shipped_seeds), "buckets": {"short": row}}
    text = R.across_seeds_section({"baseline": {"across_seeds": across},
                                   WITH_CALLS_PREV: {"across_seeds": across}}, {"short": {}})
    seeds_text = " / ".join(str(x) for x in shipped_seeds)
    lifts_text = " / ".join(f"{seed_lifts[x]:.2f}" for x in shipped_seeds)
    assert f"| pooled WF lift, seed {seeds_text} |" in text
    assert f"| short | {WITH_CALLS_PREV} | 5 | {lifts_text} | {s['median']:.2f} |" in text


def test_calls_prev_variants_are_retrained_with_every_seed(small_df, tmp_path, monkeypatch):
    """With two seeds: the variant is retrained once more (seed 11), the baseline not
    at all (its row is the gates' seed retrains), and the baseline's adversarial
    AUC is the diagnostics' own when they ran."""
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "STABILITY_SEEDS", (7, 11))
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline", WITH_CALLS_PREV)})
    calls = []
    real = train_mod._train_bucket
    monkeypatch.setattr(train_mod, "_train_bucket", lambda *a, **k: calls.append(
        (C.LGBM_SEED, C.EXCLUDED_INPUTS)) or real(*a, **k))
    out = train_mod.train(small_df, tmp_path, "v1", variants=True, diagnostics=True)
    # saved model + gate seed 11, then the variant's own run + its seed 11: nothing else
    default = ("calls_prev_1h", "calls_prev_24h")
    assert calls == [(7, default), (11, default), (7, ()), (11, ())]
    meta = json.loads((out / "meta.json").read_text())
    assert meta["features"] == list(C.FEATURES) and C.CALLS_PREV_BURST not in meta["features"]
    assert not CALLS_PREV & set(meta["features"])             # the saved model: the default
    g = meta["metrics"]["short"]["gates"]
    base = meta["variants"]["baseline"]["across_seeds"]["buckets"]["short"]
    assert base["lifts"] == g["pooled_lift_seeds"] and set(base["lifts"]) == {"7", "11"}
    assert base["adversarial"]["auc"] == meta["diagnostics"]["adversarial"]["short"]["split"]["auc"]
    var = meta["variants"][WITH_CALLS_PREV]["across_seeds"]
    a = var["buckets"]["short"]
    assert var["seeds"] == [7, 11] and set(a["lifts"]) == {"7", "11"}
    assert all(isinstance(x, float) for x in a["lifts"].values())
    assert a["median"] == pytest.approx(float(np.median(list(a["lifts"].values()))))
    assert 0.0 <= a["adversarial"]["auc"] <= 1.0
    report = (out / "report.md").read_text(encoding="utf-8")
    assert "| pooled WF lift, seed 7 / 11 |" in report
    assert (f"| short | {WITH_CALLS_PREV} | {a['weeks']} | "
            f"{a['lifts']['7']:.2f} / {a['lifts']['11']:.2f} | {a['median']:.2f} |") in report
    assert "other seeds and adversarial validation" in report


def test_a_failing_across_seeds_part_keeps_the_variant_rows(small_df, tmp_path, monkeypatch,
                                                            capsys):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    other = "stronger regularisation"                          # a second variant across seeds
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in
                                        ("baseline", other, WITH_CALLS_PREV)})
    monkeypatch.setattr(C, "SEED_VARIANTS", (other, WITH_CALLS_PREV))
    real = train_mod.variant_across_seeds

    def across(*a, **k):
        if C.EXCLUDED_INPUTS == ():
            raise RuntimeError("boom | x")
        return real(*a, **k)

    def no_probe(keep):
        raise ValueError("probe down")

    monkeypatch.setattr(train_mod, "variant_across_seeds", across)
    monkeypatch.setattr(train_mod, "adversarial_split", no_probe)
    out = train_mod.train(small_df, tmp_path, "v1", variants=True)
    assert f"across-seeds part of variant '{WITH_CALLS_PREV}' failed" in capsys.readouterr().out
    v = json.loads((out / "meta.json").read_text())["variants"]
    assert "top_lift" in v[WITH_CALLS_PREV]["buckets"]["short"] and "error" not in v[WITH_CALLS_PREV]
    assert v[WITH_CALLS_PREV]["across_seeds"] == {"error": "error: RuntimeError: boom / x"}
    for name in ("baseline", other):                           # the probe failing loses no row
        a = v[name]["across_seeds"]["buckets"]["short"]
        assert a["adversarial"] == {"error": "error: ValueError: probe down"}
        assert "lifts" in a and "sim_gain_lo" in a
    report = (out / "report.md").read_text(encoding="utf-8")
    assert f"| short | {WITH_CALLS_PREV} | error: RuntimeError: boom / x |" in report
    assert "error: ValueError: probe down" in report
