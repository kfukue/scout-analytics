"""train.py --variants: one-change variants, report only; and the runner
"tradeable" rule they use. Synthetic data only."""
import json

import numpy as np
import pandas as pd
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml.features import assert_no_leakage
from scout_ml.labels import build_labels
from scout_ml.model import Bundle

DEX_VARIANTS = ("raw dex instead of dex_family", "raw dex + dex_family")


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
                                "runners must be tradeable"]
    for name, knobs in C.VARIANTS.items():
        if name == "baseline":
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


def test_overrides_rebuild_features_never_leak_and_restore():
    before = (list(C.FEATURES), list(C.CATEGORICAL), C.DEX_INPUTS, C.DEAD_IS_COLLAPSE)
    want = {"baseline": ({"dex_family"}, {"dex"}),
            "raw dex instead of dex_family": ({"dex"}, {"dex_family"}),
            "raw dex + dex_family": ({"dex", "dex_family"}, set())}
    for name, knobs in C.VARIANTS.items():
        with C.overrides(**knobs):
            assert_no_leakage(C.FEATURES)                       # trades_24h etc. never inputs
            assert "trades_24h" not in C.FEATURES
            assert C.FEATURES == C.NUMERIC_RAW + C.DERIVED + C.CATEGORICAL
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
        assert base[b]["windows_beating"] == m["gates"]["windows_beating"]
        assert base[b]["passed"] == m["gates"]["passed"]
        assert base[b]["runner_auc_lightgbm"] == m["labels"]["runner"]["lightgbm"]["roc_auc"]
        for name in DEX_VARIANTS:   # logistic uses numeric inputs only: unchanged by DEX inputs
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
            s = v[name]["buckets"][b]
            for k in ("top_lift", "top_lift_lo", "top_lift_hi", "runner_auc_lightgbm",
                      "runner_auc_logistic", "collapse_auc_plain_lightgbm",
                      "collapse_auc_plain_logistic", "collapse_removed", "windows_beating",
                      "windows_evaluated", "sim_top_mean", "sim_all_mean", "passed"):
                assert k in s, (name, b, k)
    assert all(v[n].get("seconds") is not None for n in C.VARIANTS if n != "baseline")
    report = (var / "report.md").read_text(encoding="utf-8")
    assert "## Variant comparison (report only; not saved, not served)" in report
    assert "risks fitting the test period" in report and "walk-forward weeks" in report
    for name in C.VARIANTS:
        assert f"| {name} |" in report
    plain_report = (plain_and_variants[0] / "report.md").read_text(encoding="utf-8")
    assert "Variant comparison" not in plain_report


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
    monkeypatch.setattr(train_mod, "train", lambda df, out, variants=False: seen.update(
        variants=variants) or tmp_path)
    monkeypatch.setattr(train_mod.pd, "read_csv", lambda *a, **k: pd.DataFrame())
    assert train_mod.main(["--csv", "x.csv", "--out", str(tmp_path), "--variants"]) == 0
    assert seen["variants"] is True
    assert train_mod.main(["--csv", "x.csv", "--out", str(tmp_path)]) == 0
    assert seen["variants"] is False


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
    assert meta["variants"]["baseline"]["buckets"]["short"]["passed"] == meta["metrics"]["short"]["gates"]["passed"]


def test_variant_error_text_is_short_and_table_safe():
    from scout_ml.report import variant_error
    assert variant_error(ValueError("a | b\nmore")) == "error: ValueError: a / b"
    assert variant_error(KeyError()) == "error: KeyError"
    long = variant_error(RuntimeError("x" * 500))
    assert long.startswith("error: RuntimeError: xxx") and len(long) <= len("error: ") + 150
