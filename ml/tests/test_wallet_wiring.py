"""The live-buy wallet reputation wired in as a report-only variant (train.py
--variants --live-buys): config knob, the saved model unchanged, the n/a path,
the report blocks and the pre-registered adoption rules. Synthetic data only."""
import hashlib
import json

import numpy as np
import pandas as pd
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml import report as R
from scout_ml.features import assert_no_leakage, build_features
from scout_ml.model import fit_baseline

WALLET = "wallet reputation (wrep_*)"
SHARES = {"wrep_known_share", "wrep_good_share", "wrep_bad_share"}


# --- config ---------------------------------------------------------------------------

def test_default_is_off_and_the_saved_inputs_have_no_wrep():
    assert C.WALLET_REP is False and "WALLET_REP" in C.VARIANT_KNOBS
    assert not [c for c in C.FEATURES if c.startswith(C.WREP_PREFIX)]
    assert C.VARIANTS[WALLET] == {"WALLET_REP": True}
    assert C.WALLET_REP_VARIANTS == (WALLET,) and WALLET in C.SEED_VARIANTS
    assert set(C.WREP_SHARES) == SHARES and SHARES <= C.NO_LOG
    assert not (set(C.WREP_FEATURES) - SHARES) & C.NO_LOG          # count and lifts: log1p


def test_the_variant_adds_wrep_also_in_a_nested_seed_override(synthetic_df):
    before = (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES))
    with C.overrides(**C.VARIANTS[WALLET]):
        assert C.FEATURES == before[1] + list(C.WREP_FEATURES) + C.CATEGORICAL
        assert set(C.LOG1P_FEATURES) - set(before[2]) == set(C.WREP_FEATURES) - SHARES
        assert_no_leakage(C.FEATURES)
        with C.overrides(LGBM_SEED=11):                       # the variant's seed retrains
            assert C.WALLET_REP is True and C.LGBM_SEED == 11
            assert set(C.WREP_FEATURES) <= set(C.FEATURES)
            assert set(C.WREP_FEATURES) - SHARES <= set(C.LOG1P_FEATURES)
        assert set(C.WREP_FEATURES) <= set(C.FEATURES) and C.LGBM_SEED == 7
        # build_features reads them from the joined rows (NaN where absent)
        rows = synthetic_df.head(200).assign(wrep_n_known=2.0, wrep_known_share=0.5)
        X = build_features(rows, {})
        assert list(X.columns) == C.FEATURES
        assert (X["wrep_n_known"] == 2.0).all() and X["wrep_runner_lift_max"].isna().all()
        fit_baseline(X, (np.arange(len(X)) % 3 == 0).astype(int))   # the logistic follows
    assert (list(C.FEATURES), list(C.NUMERIC_FEATURES), list(C.LOG1P_FEATURES)) == before


def test_excluded_inputs_may_name_wrep_columns():
    with C.overrides(WALLET_REP=True, EXCLUDED_INPUTS=C.EXCLUDED_INPUTS + ("wrep_n_known",)):
        assert "wrep_n_known" not in C.FEATURES and "wrep_known_share" in C.FEATURES
    assert C.numeric_inputs(("wrep_n_known",)) == C.numeric_inputs()   # off: nothing to remove
    with pytest.raises(ValueError, match="WALLET_REP"):
        with C.overrides(WALLET_REP="yes"):
            pass
    assert not [c for c in C.FEATURES if c.startswith(C.WREP_PREFIX)]


def test_train_refuses_wrep_in_the_saved_inputs(monkeypatch, tmp_path, synthetic_df):
    monkeypatch.setattr(C, "FEATURES", list(C.FEATURES) + ["wrep_n_known"])
    with pytest.raises(ValueError, match="must not include the wallet reputation"):
        train_mod.train(synthetic_df, tmp_path / "models", "v1")
    assert not (tmp_path / "models").exists()                 # nothing written


# --- live buys as text, weeks ----------------------------------------------------------

def test_read_live_buys_keeps_the_wallet_text(tmp_path):
    path = tmp_path / "lb.csv"
    path.write_text("call_id,position,tier,amount_usd,wallet_display,wallet_prefix,wallet_suffix\n"
                    "5,1,elite,,0x12…0042,0x12,0042\n6,1,good,10.5,0x1e…1e10,0x1e,1e10\n"
                    "7,1,good,1,0x00…,0x00,\n", encoding="utf-8")
    lb = train_mod.read_live_buys(path)
    assert lb["wallet_suffix"].tolist()[:2] == ["0042", "1e10"]
    assert pd.isna(lb["wallet_suffix"].iloc[2]) and pd.isna(lb["amount_usd"].iloc[0])
    assert train_mod.read_live_buys(lb) is lb


def test_wrep_weeks_counts_the_coverage_ramp():
    dates = pd.Series(pd.to_datetime(["2026-09-01", "2026-09-02", "2026-09-03", "2026-09-08",
                                      "2026-09-09"], utc=True))
    wrep = pd.DataFrame({"wrep_n_known": [np.nan, 0.0, 2.0, 1.0, 3.0],
                         "wrep_known_share": [np.nan, 0.0, 0.5, 1.0, 0.25]})
    weeks = train_mod.wrep_weeks(dates, wrep, [True, True, True, True, False])
    assert weeks == [{"week": "2026-08-31", "calls": 3, "with_live_buys": 2, "share_known": 0.5,
                      "known_share_mean": 0.25},
                     {"week": "2026-09-07", "calls": 1, "with_live_buys": 1, "share_known": 1.0,
                      "known_share_mean": 1.0}]


# --- pre-registered adoption rules -----------------------------------------------------

def _rows(v_median=2.3, b_median=2.1, lift_lo=1.2, sim_lo=0.5, removed=0.45, v_auc=0.61,
          b_auc=0.60, top=(("liq_usd", 0.3), ("wrep_known_share", 0.2), ("holders", 0.1))):
    var = {"median": v_median, "lift_lo": lift_lo, "sim_gain_lo": sim_lo,
           "adversarial": {"auc": v_auc, "top": [list(t) for t in top]}}
    base = {"median": b_median, "adversarial": {"auc": b_auc, "top": []}}
    return base, var, {"collapse_removed": removed}


@pytest.mark.parametrize("kw, failing, result", [
    ({}, set(), "pass"),
    ({"v_median": 2.0, "b_median": 2.0, "removed": 0.40, "v_auc": 0.52, "b_auc": 0.50},
     set(), "pass"),                                                   # the bounds hold
    ({"v_median": 1.99, "b_median": 1.5}, {"median_lift"}, "fail"),    # below 2
    ({"v_median": 2.2, "b_median": 2.3}, {"median_lift"}, "fail"),     # below the baseline
    ({"lift_lo": 1.0}, {"lift_lower"}, "fail"),                        # must be above 1
    ({"sim_lo": 0.0}, {"sim_lower"}, "fail"),
    ({"removed": 0.39}, {"collapse_removed"}, "fail"),
    ({"v_auc": 0.63}, {"drift"}, "lookback"),                          # AUC 0.03 above
    ({"top": (("wrep_known_share", 0.45), ("liq_usd", 0.2))}, {"drift"}, "lookback"),
    ({"top": (("a", 0.1), ("b", 0.1), ("c", 0.1), ("wrep_n_known", 0.5))}, set(), "pass"),  # 4th
    ({"v_auc": 0.7, "removed": 0.3}, {"drift", "collapse_removed"}, "fail"),
])
def test_adoption_rules(kw, failing, result):
    a = train_mod.adoption_bucket(*_rows(**kw))
    assert {k for k, r in a["rules"].items() if not r["holds"]} == failing
    assert a["result"] == result


def test_adoption_rules_undefined_values_do_not_hold():
    a = train_mod.adoption_bucket(*_rows(v_median=float("nan"), v_auc=None))
    assert a["rules"]["median_lift"]["holds"] is None and a["rules"]["drift"]["holds"] is None
    assert a["result"] == "fail"
    a = train_mod.adoption_bucket(*_rows(v_auc=None))                 # only the drift is n/a
    assert a["result"] == "lookback"
    cells = R.adoption_cells(a)
    assert cells[4].startswith("n/a: AUC n/a") and "lookback variant" in cells[-1]


def test_wallet_adoption_per_bucket_and_not_applicable():
    base, var, summary = _rows()
    variants = {"baseline": {"across_seeds": {"buckets": {"short": base, "medium": base}}},
                WALLET: {"buckets": {"short": summary, "medium": {"skipped": "too few"},
                                     "3day": summary},
                         "across_seeds": {"buckets": {"short": var, "medium": {"skipped": "x"}}}}}
    out = train_mod.wallet_adoption(variants["baseline"], variants[WALLET])
    assert out["rules"] == C.WREP_ADOPTION and out["saved_seed"] == C.LGBM_SEED
    assert out["buckets"]["short"]["result"] == "pass"
    assert out["buckets"]["medium"] == {"not_applicable": "too few"}
    assert out["buckets"]["3day"] == {"not_applicable": "no across-seeds row"}
    text = R.adoption_section(WALLET, {**variants[WALLET], "adoption": out})
    assert "| short | holds: 2.30 vs baseline 2.10 | holds: 1.20 |" in text
    assert "| PASS (all rules hold) |" in text and "| medium | n/a: too few |" in text
    assert "| holds: 45.0% |" in text                         # rule 4 as a percentage, as "40%"


def test_drift_rule_reads_drift_top_inputs(monkeypatch):
    monkeypatch.setattr(C, "WREP_ADOPTION", {**C.WREP_ADOPTION, "drift_top": 4})
    top = [[f"f{i}", 0.1] for i in range(8)]
    kept = train_mod._top_inputs({"auc": 0.6, "top": top})
    assert kept["top"] == top[:4] and kept["auc"] == 0.6
    out = train_mod.across_seeds({"short": {}}, {}, None, {"short": {}},
                                 {"short": {"auc": 0.6, "top": top}})
    assert out["buckets"]["short"]["adversarial"]["top"] == top[:4]
    # a wrep_* input in 4th place now counts (with drift_top 3 it passes, see above)
    a = train_mod.adoption_bucket(*_rows(top=(("a", 0.1), ("b", 0.1), ("c", 0.1),
                                              ("wrep_n_known", 0.5))))
    assert a["rules"]["drift"]["wrep_top"] == [["wrep_n_known", 0.5]] and a["result"] == "lookback"
    # the report still shows 3 inputs
    assert R._adv_cells({"auc": 0.6, "top": top[:4]})[1] == "f0 0.10, f1 0.10, f2 0.10"


def test_wrep_not_applicable_reasons():
    df = pd.DataFrame({"x": [1.0, 2.0]})
    assert train_mod.wrep_not_applicable(df) == train_mod.WREP_NOT_APPLICABLE
    empty = df.assign(wrep_n_known=np.nan, wrep_known_share=np.nan)
    assert train_mod.wrep_not_applicable(empty) == train_mod.WREP_NO_LIVE_BUYS
    assert train_mod.wrep_not_applicable(empty.assign(wrep_n_known=[np.nan, 0.0])) is None


def test_report_blocks_without_live_buys_or_after_an_error():
    na = {"changes": {"WALLET_REP": True}, "not_applicable": train_mod.WREP_NOT_APPLICABLE}
    text = R.variants_section({"baseline": {"changes": {}}, WALLET: na}, {})
    assert R.WALLET_DATA_TITLE in text and "No live buys given" in text
    assert R.ADOPTION_TITLE in text and "Fixed on 14 Oct 2026, before any result" in text
    assert f"Not evaluated for `{WALLET}`: n/a: no live-buy wallet data" in text
    text = R.wallet_data_section({"error": "error: FileNotFoundError: x", "seconds": 0.0})
    assert "**Not computed: error: FileNotFoundError: x.**" in text
    # without the wallet variant (e.g. VARIANTS trimmed) there is no wallet block
    assert R.WALLET_DATA_TITLE not in R.variants_section({"baseline": {"changes": {}}}, {})


# --- end to end: the saved model is the same with and without live buys -----------------

@pytest.fixture(scope="module")
def wallet_runs(tmp_path_factory):
    """make_synthetic's CLI writes calls + live buys; train.py's CLI then runs
    --variants without and with --live-buys, and once with --live-buys but no
    --variants (short and medium buckets, baseline + the wallet variant)."""
    mp = pytest.MonkeyPatch()
    mp.setattr(C, "BUCKETS", {b: C.BUCKETS[b] for b in ("short", "medium")})
    mp.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline", WALLET)})
    root = tmp_path_factory.mktemp("wallet")
    csv, lb = root / "calls.csv", root / "live_buys.csv"
    try:
        make_synthetic.main(["--out", str(csv), "--calls", "2500", "--days", "60", "--seed", "5",
                             "--live-buys", str(lb)])
        out = {}
        for name, extra in (("without", ["--variants"]),
                            ("with", ["--variants", "--live-buys", str(lb)]),
                            ("plain", ["--live-buys", str(lb)])):
            models = root / name
            assert train_mod.main(["--csv", str(csv), "--out", str(models), *extra]) == 0
            out[name] = models / (models / "LATEST").read_text().strip()
    finally:
        mp.undo()
    return out, lb


def _digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def test_saved_model_and_gates_identical_with_and_without_live_buys(wallet_runs):
    runs, _ = wallet_runs
    metas = {k: json.loads((d / "meta.json").read_text()) for k, d in runs.items()}
    files = {k: sorted(p.name for p in d.glob("*.joblib")) for k, d in runs.items()}
    assert files["with"] == files["without"] == files["plain"] and len(files["with"]) >= 2
    for f in files["with"]:                                     # byte-identical model files
        assert _digest(runs["with"] / f) == _digest(runs["without"] / f) == _digest(runs["plain"] / f)
    for k in metas["without"]:
        if k not in ("version", "trained_at", "variants"):
            assert metas["with"][k] == metas["without"][k], k
            assert metas["plain"][k] == metas["without"][k], k
    assert set(metas["with"]) - set(metas["without"]) == {"wallet_rep"}
    assert "wallet_rep" not in metas["plain"] and "variants" not in metas["plain"]
    assert not [c for c in metas["with"]["features"] if c.startswith(C.WREP_PREFIX)]
    # the baseline row of the comparison is the same too (apart from its timing)
    rows = [{**m["variants"]["baseline"],
             "across_seeds": {k: v for k, v in m["variants"]["baseline"]["across_seeds"].items()
                              if k != "seconds"}} for m in (metas["with"], metas["without"])]
    assert rows[0] == rows[1]


def test_wallet_variant_runs_with_live_buys_and_is_na_without(wallet_runs):
    runs, _ = wallet_runs
    without = json.loads((runs["without"] / "meta.json").read_text())["variants"][WALLET]
    assert without["not_applicable"] == train_mod.WREP_NOT_APPLICABLE and "buckets" not in without
    meta = json.loads((runs["with"] / "meta.json").read_text())
    v = meta["variants"][WALLET]
    assert "error" not in v and set(v["buckets"]) == {"short", "medium"}
    base = meta["variants"]["baseline"]["buckets"]["short"]
    # more numeric inputs: the logistic baseline differs, the rows and simulation do not
    assert v["buckets"]["short"]["runner_auc_logistic"] != base["runner_auc_logistic"]
    assert v["buckets"]["short"]["sim_all_mean"] == base["sim_all_mean"]
    assert set(v["across_seeds"]["buckets"]) == {"short", "medium"}
    assert set(v["adoption"]["buckets"]) == {"short", "medium"}
    assert v["adoption"]["buckets"]["short"]["result"] in ("pass", "lookback", "fail")
    st = meta["wallet_rep"]
    for k in ("rows", "live_buy_rows", "wallets", "calls_with_live_buys", "eligible_history",
              "extreme_dropped", "y_run_rate", "y_rug_rate", "seconds", "weeks"):
        assert k in st, k
    assert st["rows"] == 2500 and st["calls_with_live_buys"] > 0 and st["wallets"] > 0
    assert st["prior_runner"] == C.WREP_PRIOR_RUNNER and st["prior_rug"] == C.WREP_PRIOR_RUG
    shares = [w["share_known"] for w in st["weeks"] if w["with_live_buys"]]
    assert shares[0] < shares[-1]                         # history builds up: the coverage ramp


def test_report_has_the_wallet_blocks(wallet_runs):
    runs, _ = wallet_runs
    meta = json.loads((runs["with"] / "meta.json").read_text())
    st = meta["wallet_rep"]
    report = (runs["with"] / "report.md").read_text(encoding="utf-8")
    data = report[report.index(R.WALLET_DATA_TITLE):report.index(R.ADOPTION_TITLE)]
    assert (f"{st['live_buy_rows']} live buys of {st['wallets']} distinct wallets on "
            f"{st['calls_with_live_buys']} calls") in data
    assert f"| runner (medium runner rule, net of tax) | {st['y_run_rate']:.1%} | 5.0% |" in data
    assert "| rug (raw ret_late_7d <= -90 %) |" in data and "| 15.0% |" in data
    assert "| week | calls | calls with live buys | share with >= 1 known wallet |" in data
    assert f"| {st['weeks'][-1]['week']} |" in data
    rules = report[report.index(R.ADOPTION_TITLE):]
    assert "1. median lift" in rules and "next step is a lookback variant" in rules
    assert "| bucket | 1. median lift | 2. lift lower end |" in rules
    for b in ("short", "medium"):
        assert f"| {b} | " in rules
    across = report[report.index("### Variants across seeds (report only)"):]
    assert f"| short | {WALLET} | " in across and f"| short | {WALLET} | n/a" not in across
    plain = (runs["plain"] / "report.md").read_text(encoding="utf-8")
    assert R.WALLET_DATA_TITLE not in plain and "Variant comparison" not in plain
    without = (runs["without"] / "report.md").read_text(encoding="utf-8")
    assert "No live buys given" in without and "Not evaluated for" in without


def test_a_failing_wallet_reputation_is_recorded_and_the_run_succeeds(small_live_df, tmp_path,
                                                                      monkeypatch, capsys):
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline", WALLET)})
    out = train_mod.train(small_live_df, tmp_path, "v1", variants=True,
                          live_buys=tmp_path / "missing.csv")
    assert "WARNING: the wallet reputation failed (error: FileNotFoundError" in capsys.readouterr().out
    meta = json.loads((out / "meta.json").read_text())
    assert meta["wallet_rep"]["error"].startswith("error: FileNotFoundError")
    assert meta["variants"][WALLET]["not_applicable"] == train_mod.WREP_NOT_APPLICABLE
    assert "short_runner" in meta["models"]
    assert "**Not computed: error: FileNotFoundError" in (out / "report.md").read_text(encoding="utf-8")


def test_a_header_only_live_buys_csv_makes_the_variant_na(small_live_df, tmp_path, monkeypatch):
    """A header-only live_buys.csv passes run_training.sh's `[ -s ]` check and gives
    an all-NaN wrep_* frame: the variant is n/a with its own reason, not trained."""
    # the medium bucket stays: the wallet history uses its runner rule
    monkeypatch.setattr(C, "BUCKETS", {b: C.BUCKETS[b] for b in ("short", "medium")})
    monkeypatch.setattr(C, "VARIANTS", {k: C.VARIANTS[k] for k in ("baseline", WALLET)})
    lb = tmp_path / "live_buys.csv"
    lb.write_text("call_id,position,tier,amount_usd,wallet_display,wallet_prefix,wallet_suffix\n",
                  encoding="utf-8")
    out = train_mod.train(small_live_df, tmp_path / "models", "v1", variants=True, live_buys=lb)
    meta = json.loads((out / "meta.json").read_text())
    assert meta["wallet_rep"]["calls_with_live_buys"] == 0 and "error" not in meta["wallet_rep"]
    v = meta["variants"][WALLET]
    assert v["not_applicable"] == train_mod.WREP_NO_LIVE_BUYS
    assert not {"buckets", "across_seeds", "adoption"} & set(v)
    assert "short_runner" in meta["models"]
    report = (out / "report.md").read_text(encoding="utf-8")
    assert f"Not evaluated for `{WALLET}`: n/a: no live buys in the export" in report
    assert "0 live buys of 0 distinct wallets on 0 calls" in report
    across = report[report.index("### Variants across seeds (report only)"):]
    assert f"| short | {WALLET} | n/a: no live buys in the export" in across


@pytest.fixture(scope="module")
def small_live_df():
    return make_synthetic.make(n=1500, days=40, seed=5)
