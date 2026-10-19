"""Collapse filter (report only): what skipping the SKIP_FRAC calls with the
highest collapse score catches (collapses) and costs (runners, mean return),
on the test part and on the pooled walk-forward. Never a gate, never served.
Synthetic data only."""
import hashlib
import json

import numpy as np
import pytest

import make_synthetic
import train as train_mod
from scout_ml import config as C
from scout_ml import diagnostics as D
from scout_ml import report as R
from scout_ml import validate as V

NAN = float("nan")

# 10 calls, the first 3 flagged. Collapses: 0, 1, 5. Runners: 2, 6, 7, 8.
FLAG = np.array([1, 1, 1, 0, 0, 0, 0, 0, 0, 0], bool)
Y_COLL = np.array([1, 1, 0, 0, 0, 1, 0, 0, 0, 0], float)
Y_RUN = np.array([0, 0, 1, 0, 0, 0, 1, 1, 1, 0], float)
RET = np.array([-80, -90, 5000, 0, 10, -75, 60, 80, 2000, -20], float)   # two above the cap


def test_hand_made_rates_counts_and_capped_means():
    s = V.collapse_filter_stats(Y_COLL, Y_RUN, RET, FLAG)
    assert (s["calls"], s["flagged"], s["collapses"], s["collapses_flagged"],
            s["runners"], s["runners_flagged"]) == (10, 3, 3, 2, 4, 1)
    assert all(isinstance(s[k], int) for k in V.FILTER_COUNTS)
    assert s["flagged_share"] == pytest.approx(0.3)
    assert s["collapses_caught"] == pytest.approx(2 / 3)
    assert s["collapse_rate_flagged"] == pytest.approx(2 / 3)
    assert s["collapse_rate_kept"] == pytest.approx(1 / 7)
    assert s["runners_lost"] == pytest.approx(1 / 4)
    assert s["runner_rate_flagged"] == pytest.approx(1 / 3)
    assert s["runner_rate_kept"] == pytest.approx(3 / 7)
    capped = np.minimum(RET, C.SIM_MAX_RET_PCT)                 # 5000 and 2000 -> 1000
    assert capped.sum() == pytest.approx(1885.0)
    assert s["all_mean"] == pytest.approx(188.5)                # uncapped it would be 488.5
    assert s["kept_mean"] == pytest.approx(1055.0 / 7)          # 1885 minus the flagged 830
    assert s["kept_minus_all"] == pytest.approx(1055.0 / 7 - 188.5)


def test_rate_keys_are_the_documented_ones():
    assert (tuple(V.collapse_filter_rates(V.collapse_filter_sums([0], [0], [0], [True])))
            == V.FILTER_RATES)


def test_one_ratio_helper_shared_with_diagnostics():
    assert D._div is V._ratio
    out = V._ratio([1.0, 2.0, NAN, 1.0], [2.0, 0.0, 4.0, -1.0])
    assert out[0] == 0.5 and np.isnan(out[1]) and np.isnan(out[2]) and np.isnan(out[3])
    assert float(V._ratio(3, 4)) == 0.75 and np.isnan(V._ratio(3, 0))


def test_the_cap_is_read_at_call_time(monkeypatch):
    monkeypatch.setattr(C, "SIM_MAX_RET_PCT", 100.0)
    s = V.collapse_filter_stats(Y_COLL, Y_RUN, RET, FLAG)
    assert s["all_mean"] == pytest.approx(np.minimum(RET, 100.0).mean())


def test_collapses_caught_is_collapses_removed_with_the_same_flag():
    rng = np.random.default_rng(3)
    score, y = rng.random(200), (rng.random(200) < 0.4).astype(float)
    s = V.collapse_filter_stats(y, rng.random(200) < 0.1, rng.normal(size=200),
                                V.top_mask(score, C.SKIP_FRAC))
    assert s["collapses_caught"] == V.collapse_removed(y, score)       # exactly the gate's number
    assert s["flagged"] == int(np.ceil(200 * C.SKIP_FRAC))


def test_undefined_rates_are_nan_never_an_error():
    none = V.collapse_filter_stats(np.zeros(4), np.zeros(4), np.zeros(4), [1, 0, 0, 0])
    assert np.isnan(none["collapses_caught"]) and np.isnan(none["runners_lost"])
    every = V.collapse_filter_stats(np.ones(3), np.ones(3), np.ones(3), np.ones(3, bool))
    assert every["flagged_share"] == 1.0 and np.isnan(every["kept_mean"])
    assert np.isnan(every["collapse_rate_kept"]) and np.isnan(every["kept_minus_all"])


def test_rates_work_on_arrays_like_the_bootstrap():
    a = V.collapse_filter_sums(Y_COLL, Y_RUN, RET, FLAG)
    b = V.collapse_filter_sums(Y_COLL[::-1], Y_RUN, RET, FLAG)
    both = V.collapse_filter_rates({k: np.array([a[k], b[k]]) for k in a})
    for k, v in both.items():
        np.testing.assert_allclose(v, [V.collapse_filter_rates(a)[k], V.collapse_filter_rates(b)[k]])


# --- pooled over walk-forward weeks ------------------------------------------------

def _weeks(n_weeks=6, n=80, seed=1):
    rng = np.random.default_rng(seed)
    out = []
    for _ in range(n_weeks):
        out.append({"yr": (rng.random(n) < 0.15).astype(float), "sr": rng.random(n),
                    "yc": (rng.random(n) < 0.4).astype(float), "sc": rng.random(n),
                    "ret": rng.normal(0, 400, n) + (rng.random(n) < 0.05) * 5000})
    return out


def _sums(weeks):
    return [D.week_sums(w["yr"], w["sr"], w["yc"], w["sc"], w["ret"]) for w in weeks]


def test_pooled_filter_equals_the_pure_function_on_the_pooled_rows():
    weeks = _weeks()
    p = D.pooled(_sums(weeks), with_filter=True)
    flag = np.concatenate([V.top_mask(w["sc"], C.SKIP_FRAC) for w in weeks])   # per-week flags
    s = V.collapse_filter_stats(*(np.concatenate([w[k] for w in weeks]) for k in ("yc", "yr", "ret")),
                                flag)
    for k in V.FILTER_RATES:
        assert p[f"filter_{k}"] == pytest.approx(s[k]), k
        assert f"filter_{k}_lo" in p and f"filter_{k}_hi" in p


def test_pooled_collapses_caught_is_the_existing_pooled_figure_with_its_interval():
    sums = _sums(_weeks())
    plain, with_f = D.pooled(sums), D.pooled(sums, with_filter=True)
    for e in ("", "_lo", "_hi"):
        assert with_f[f"filter_collapses_caught{e}"] == plain[f"collapse_removed{e}"]
    assert not any(k.startswith("filter_") for k in plain)                # default: as before
    assert D.without_filter(with_f) == plain                             # gate numbers unchanged
    assert with_f["filter_kept_minus_all_lo"] < with_f["filter_kept_minus_all_hi"]


def test_a_week_without_a_collapse_model_has_no_filter_numbers():
    w = _weeks(1)[0]
    s = D.week_sums(w["yr"], w["sr"], w["yc"], None, w["ret"])
    assert all(np.isnan(s[k]) for k in ("coll_top", "coll_all", "flag_n", "run_flag", "ret_flag"))
    assert s["top_n"] > 0                                                # the runner sums stay


def test_label_note_per_bucket():
    assert train_mod.filter_labels("short") == {
        "runner_label": "max_gain_late_1d >= 100%", "collapse_label": "ret_late_1d <= -50%",
        "can_be_both": True}
    assert train_mod.filter_labels("3day")["can_be_both"] is False
    assert train_mod.filter_labels("medium")["can_be_both"] is False
    assert train_mod.filter_labels("long")["collapse_label"].endswith("OR rugged")


# --- report rendering ------------------------------------------------------------

def _block(**over):
    s = V.collapse_filter_stats(Y_COLL, Y_RUN, RET, FLAG)
    pooled = {"weeks": 2, "reps": 2000, "seed": 7, **s,
              **{f"{k}_{e}": 0.1 for k in V.FILTER_RATES for e in ("lo", "hi")}}
    return {"flag_frac": 0.3, "random_skip_expected": 0.3, "sim_max_ret_pct": 1000.0,
            **train_mod.filter_labels("short"), "test": s, "pooled": pooled,
            "weeks": [{"week": 3, "start": "2026-09-01", **s},
                      {"week": 4, "start": "2026-09-08", "skipped": "no collapse model"}], **over}


def test_section_text_tables_and_the_1d_note():
    text = R.collapse_filter_section(_block())
    assert text.startswith("### Collapse filter\n")
    assert "(report only)" not in text.splitlines()[0]
    assert ("The 30% cut is taken within each part (the test part, each walk-forward week) "
            "after its scores are known") in text
    assert ("a fixed score threshold set in advance, so these rates are likely optimistic "
            "for such a filter.") in text
    assert "upper bound" not in text
    assert "max_gain_late_1d >= 100%" in text and "one call can be both" in text
    assert "| collapses caught = collapses removed (flagged/all collapses) | 0.667 (2/3) | " in text
    assert "0.667 [0.100, 0.100] (2/3) | 0.300 |" in text
    assert "| runners lost (flagged/all runners) | 0.250 (1/4) |" in text
    assert "pooled walk-forward (2 counted weeks) [95%]" in text
    assert "| 3 | 2026-09-01 | 3/10 | 0.667 | 0.250 |" in text
    assert "| 4 | 2026-09-08 | n/a: no collapse model |" in text
    assert ("The runner label reads max_gain_late_1d (the peak, not the return) and the "
            "collapse label ret_late_1d, so one call can be both") in text
    same = R.collapse_filter_section({**_block(), **train_mod.filter_labels("medium")})
    assert "Both labels read the same return column (ret_late_7d), so no call is both." in same
    rug = R.collapse_filter_section({**_block(), **train_mod.filter_labels("long")})
    assert "A rugged call counts as a collapse whatever its return" in rug


def test_section_with_missing_parts_or_an_error():
    text = R.collapse_filter_section(_block(test={"skipped": "no collapse model"},
                                            pooled={"error": "boom"}, weeks=[]))
    assert "| calls | n/a: no collapse model | n/a: boom |" in text
    assert "Per counted walk-forward week" not in text
    err = R.collapse_filter_section({"error": "error: ValueError: x"})
    assert "**Not computed: error: ValueError: x.**" in err


def test_pooled_block_has_every_key_also_without_a_counted_week():
    out = train_mod.filter_pooled(D.pooled([], with_filter=True), [])
    assert out["weeks"] == 0 and all(out[k] == 0 for k in V.FILTER_COUNTS)
    assert all(np.isnan(out[f"{k}{e}"]) for k in V.FILTER_RATES for e in ("", "_lo", "_hi"))
    assert train_mod.filter_pooled({"error": "boom"}, []) == {"error": "boom"}
    assert train_mod.filter_pooled(None, []) == {"error": "pooled walk-forward not computed"}


# --- in a training run -------------------------------------------------------------

@pytest.fixture(scope="module")
def small_df():
    return make_synthetic.make(n=2500, days=60, seed=5)


def _hashes(d):
    return {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(d.glob("*.joblib"))}


def test_training_writes_the_block_and_a_failure_changes_nothing_else(small_df, tmp_path,
                                                                     monkeypatch, capsys):
    """Computed in every training (diagnostics off here); an error in it is
    recorded with a WARNING; the saved model files are byte-identical and the
    gates the same either way. The second run also loses the fixed cut-off
    (collapse_reference_block raises), so it is the run "without this change":
    gates and `passed` are identical with and without the verdict and the
    cut-off. `short` is made a filter bucket with rules any number meets, so
    the first run is validated and the failure visibly turns that off."""
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    monkeypatch.setattr(C, "FILTER_BUCKETS", ("short",))
    monkeypatch.setattr(C, "FILTER_RULES", {"min_caught_lower": -1e9,
                                            "min_kept_minus_all_lower": -1e9})
    ok = train_mod.train(small_df, tmp_path / "ok", "v1", diagnostics=False)
    meta = json.loads((ok / "meta.json").read_text())
    m = meta["metrics"]["short"]
    cf = m["collapse_filter"]
    assert set(cf) == {"flag_frac", "random_skip_expected", "sim_max_ret_pct", "runner_label",
                       "collapse_label", "can_be_both", "test", "pooled", "weeks", "verdict",
                       "test_at_cutoff"}
    v = cf["verdict"]
    assert v["applies"] is True and v["validated"] is True and v["reasons"] == []
    assert v["weeks"] == cf["pooled"]["weeks"] >= C.GATE_MIN_POOLED_WEEKS
    assert v["caught_lo"] == cf["pooled"]["collapses_caught_lo"]
    assert v["kept_minus_all_lo"] == cf["pooled"]["kept_minus_all_lo"]
    assert (v["set_on"], v["seen_report"]) == ("18 Oct 2026", "20261018-2204")
    ref = meta["collapse_reference"]["short"]
    assert ref["n"] >= C.FILTER_CUTOFF_MIN_ROWS and ref["rows"] == "pre-test held-out"
    at = cf["test_at_cutoff"]
    assert at["cutoff"] == ref["cutoff"] and at["calls"] == m["split"]["n_test"]
    assert 0 < at["flagged"] < at["calls"]
    assert meta["thresholds"]["filter_rules"]["buckets"] == ["short"]
    rates = set(V.FILTER_RATES)
    assert set(cf["test"]) == set(V.FILTER_COUNTS) | rates
    assert cf["test"]["collapses_caught"] == m["trading"]["collapse_removed"]
    assert cf["test"]["calls"] == m["split"]["n_test"]
    p = cf["pooled"]
    assert p["weeks"] == m["gates"]["pooled_weeks"] == len(cf["weeks"])
    assert set(p) >= rates | {f"{k}_{e}" for k in rates for e in ("lo", "hi")}
    assert p["calls"] == sum(w["n_test"] for w in m["walk_forward"] if w["counted"])
    assert p["collapses_flagged"] == sum(w["collapses_flagged"] for w in cf["weeks"])
    assert p["collapses_caught_lo"] <= p["collapses_caught"] <= p["collapses_caught_hi"]
    assert p["kept_minus_all_lo"] <= p["kept_minus_all"] <= p["kept_minus_all_hi"]

    assert "collapse_filter" not in m["gates"] and "collapse_filter" not in m["trading"]
    report = (ok / "report.md").read_text(encoding="utf-8")
    assert report.count("### Collapse filter\n") == 1
    assert report.count("### Collapse filter verdict (owner rule of 18 Oct 2026)") == 1
    assert "**Collapse filter verdict (owner rule of 18 Oct 2026): validated.**" in report
    assert "test part at the fixed cut-off" in report
    capsys.readouterr()

    def boom(*a, **k):
        raise RuntimeError("boom")
    monkeypatch.setattr(train_mod, "collapse_filter", boom)
    monkeypatch.setattr(train_mod, "collapse_reference_block", boom)
    bad = train_mod.train(small_df, tmp_path / "bad", "v1", diagnostics=False)
    printed = capsys.readouterr().out
    assert "WARNING: the collapse filter of short failed" in printed
    assert "WARNING: the collapse cut-off failed (error: RuntimeError: boom)" in printed
    meta_bad = json.loads((bad / "meta.json").read_text())
    m_bad = meta_bad["metrics"]["short"]
    assert m_bad["collapse_filter"] == {"error": "error: RuntimeError: boom"}   # no verdict
    assert meta_bad["collapse_reference"]["short"] == {"error": "error: RuntimeError: boom"}
    assert m_bad["gates"] == m["gates"] and m_bad["trading"] == m["trading"]
    assert m_bad["gates"]["passed"] == m["gates"]["passed"]
    assert m_bad["walk_forward"] == m["walk_forward"] and m_bad["labels"] == m["labels"]
    assert meta_bad["runner_reference"] == meta["runner_reference"]
    assert _hashes(bad) == _hashes(ok) and len(_hashes(ok)) == 2      # byte-identical models
    bad_report = (bad / "report.md").read_text(encoding="utf-8")
    assert "**Not computed: error: RuntimeError: boom.**" in bad_report
    assert ("- `short` not validated: the collapse filter was not computed "
            "(error: RuntimeError: boom)") in bad_report


# --- the fixed cut-off and the verdict (owner decision of 18 Oct 2026) ----------------

def test_rule_constants():
    assert C.FILTER_BUCKETS == ("3day", "medium")
    assert C.FILTER_RULES == {"min_caught_lower": 0.30, "min_kept_minus_all_lower": 0.0}
    assert (C.FILTER_RULES_SET_ON, C.FILTER_RULES_SEEN_REPORT) == ("18 Oct 2026", "20261018-2204")
    assert ("set on 18 Oct 2026 after seeing report 20261018-2204; every training run "
            "since then computes this verdict") in C.FILTER_RULES_NOTE
    assert "counts for the site" not in C.FILTER_RULES_NOTE
    assert C.FILTER_CUTOFF_MIN_ROWS == 100
    rules = train_mod.filter_rules()
    assert rules["min_caught_lower"] == 0.30 and rules["buckets"] == ["3day", "medium"]
    assert rules["flag_frac"] == C.SKIP_FRAC and rules["seen_report"] == "20261018-2204"


def test_cutoff_is_the_quantile_of_the_scores(monkeypatch):
    s = np.random.default_rng(4).random(1000)
    assert V.collapse_cutoff(s) == np.quantile(s, 1 - C.SKIP_FRAC)
    assert V.collapse_cutoff(s, 0.1) == np.quantile(s, 0.9)
    assert (s >= V.collapse_cutoff(s)).mean() == pytest.approx(C.SKIP_FRAC, abs=0.002)
    assert np.isnan(V.collapse_cutoff([]))
    monkeypatch.setattr(C, "SKIP_FRAC", 0.5)                      # read at call time
    assert V.collapse_cutoff(s) == np.quantile(s, 0.5)


class _RawModel:
    """A stand-in LightGBM model: its raw score is the column "x"."""

    def predict(self, X, raw_score=False):
        assert raw_score
        return X["x"].to_numpy(dtype=float)


def _sigmoid(x):
    return 1 / (1 + np.exp(-np.asarray(x, dtype=float)))


def _hand_rows(n=400):
    """`n` calls, one every 2 hours from 1 Sep 2026, one token each."""
    import pandas as pd
    dates = pd.Series(pd.date_range("2026-09-01", periods=n, freq="2h", tz="UTC"))
    tokens = pd.Series([f"0x{i:04x}" for i in range(n)])
    return dates, tokens


@pytest.mark.parametrize("forward", [False, True])
def test_cutoff_comes_from_the_pre_test_rows_not_the_test_rows(forward, monkeypatch):
    import pandas as pd
    monkeypatch.setattr(C, "FILTER_CUTOFF_MIN_ROWS", 10)
    h = 3.0
    dates, tokens = _hand_rows()
    split = (V.forward_split(dates, tokens, h, test_days=5) if forward
             else V.time_split(dates, tokens, h))
    lo, hi = ((split["t_train_end"], split["t_test"]) if forward
              else (split["t1"] - pd.Timedelta(days=h), split["t2"]))
    pre = ((dates >= lo) & (dates < hi)).to_numpy()
    te = split["test"]
    assert not (pre & te).any() and (dates[~pre & ~te] < lo).all()
    # raw collapse score x: -5 on the training side, 5 on the test rows, spread
    # between on the pre-test held-out rows: a leaked train or test row moves the cut-off
    x = np.where(te, 5.0, -5.0)
    x[pre] = np.linspace(-1, 1, pre.sum())
    X = pd.DataFrame({"x": x})
    mask = train_mod.cutoff_rows(split, dates, h)
    np.testing.assert_array_equal(mask, pre)
    out = train_mod.collapse_reference_block({"model": _RawModel(), "calibrator": None}, X,
                                             mask, dates)
    want = _sigmoid(np.linspace(-1, 1, pre.sum()))
    assert out["cutoff"] == pytest.approx(np.quantile(want, 1 - C.SKIP_FRAC), rel=1e-12)
    np.testing.assert_allclose(out["quantiles"],
                               np.quantile(want, np.linspace(0, 1, C.N_REF_QUANTILES)))
    assert len(out["quantiles"]) == C.N_REF_QUANTILES
    assert out["n"] == int(pre.sum()) and out["quantile"] == 1 - C.SKIP_FRAC
    assert out["from"] == dates[pre].min().strftime("%Y-%m-%d %H:%M")
    assert out["to"] == dates[pre].max().strftime("%Y-%m-%d %H:%M")
    assert out["rows"] == "pre-test held-out" and out["calibrated"] is False
    # taken on the test rows instead, the cut-off would be sigmoid(5)
    assert out["cutoff"] < _sigmoid(0.5) < _sigmoid(5.0)


def test_no_cutoff_with_too_few_rows_or_without_a_collapse_model():
    import pandas as pd
    dates, _ = _hand_rows(50)
    X = pd.DataFrame({"x": np.zeros(50)})
    fitted = {"model": _RawModel(), "calibrator": None}
    few = train_mod.collapse_reference_block(fitted, X, np.ones(50, bool), dates)
    assert few == {"skipped": "only 50 pre-test held-out rows (need 100)"}
    none = train_mod.collapse_reference_block(None, X, np.ones(50, bool), dates)
    assert none == {"skipped": "no collapse model"}


def _keep(scores, ref):
    n = len(scores)
    return {"scores": {"collapse": np.asarray(scores, dtype=float)}, "te": np.ones(n, bool),
            "y_plain": Y_COLL[:n], "Y": {"runner": Y_RUN[:n]}, "net_ret": RET[:n],
            "collapse_reference": ref}


def test_test_part_at_the_fixed_cutoff_flags_scores_at_or_above_it():
    scores = [0.9, 0.8, 0.7, 0.1, 0.2, 0.3, 0.1, 0.2, 0.3, 0.4]     # >= 0.7: the first 3 (FLAG)
    out = train_mod.filter_at_cutoff(_keep(scores, {"cutoff": 0.7}))
    assert out == {"cutoff": 0.7, **V.collapse_filter_stats(Y_COLL, Y_RUN, RET, FLAG)}
    assert train_mod.filter_at_cutoff(_keep(scores, {"cutoff": 0.95}))["flagged"] == 0
    assert (train_mod.filter_at_cutoff(_keep(scores, {"skipped": "no collapse model"}))
            == {"skipped": "no collapse model"})
    assert train_mod.filter_at_cutoff(_keep(scores, {"error": "x"})) == {"skipped": "x"}
    k = _keep(scores, {"cutoff": 0.7})
    k["scores"] = {}
    assert train_mod.filter_at_cutoff(k) == {"skipped": "no cut-off"}


def _pooled_block(**over):
    return {"weeks": 4, "collapses_caught": 0.5, "collapses_caught_lo": 0.35,
            "collapses_caught_hi": 0.6, "kept_minus_all": 3.0, "kept_minus_all_lo": 0.5,
            "kept_minus_all_hi": 6.0, **over}


def test_filter_verdict_passes_when_both_lower_ends_beat_random_skipping():
    v = V.filter_verdict(_pooled_block(), True)
    assert v == {"applies": True, "validated": True, "weeks": 4, "min_weeks": 2,
                 "caught_lo": 0.35, "min_caught_lower": 0.30, "caught_ok": True,
                 "kept_minus_all_lo": 0.5, "min_kept_minus_all_lower": 0.0, "kept_ok": True,
                 "reasons": [], "set_on": "18 Oct 2026", "seen_report": "20261018-2204"}


@pytest.mark.parametrize("over,reason", [
    ({"collapses_caught_lo": 0.30}, "pooled collapses caught 95% lower end 0.300 <= 0.3"),
    ({"collapses_caught_lo": 0.2}, "pooled collapses caught 95% lower end 0.200 <= 0.3"),
    ({"kept_minus_all_lo": 0.0}, "pooled kept minus all (mean net %) 95% lower end 0.000 <= 0"),
    ({"kept_minus_all_lo": -2.5}, "pooled kept minus all (mean net %) 95% lower end -2.500 <= 0"),
    ({"collapses_caught_lo": NAN}, "pooled collapses caught 95% interval undefined"),
    ({"kept_minus_all_lo": None}, "pooled kept minus all (mean net %) 95% interval undefined"),
    ({"weeks": 1}, "only 1 counted walk-forward week(s); the verdict needs at least 2"),
])
def test_filter_verdict_fails_with_a_reason(over, reason):
    v = V.filter_verdict(_pooled_block(**over), True)
    assert v["validated"] is False and reason in v["reasons"]
    if "weeks" in over:                       # too few weeks: neither rule is judged
        assert v["caught_ok"] is False and v["kept_ok"] is False and len(v["reasons"]) == 1


def test_filter_verdict_on_a_failed_or_missing_pooled_block():
    err = V.filter_verdict({"error": "boom"}, True)
    assert err["validated"] is False and err["weeks"] == 0
    assert err["reasons"][0] == "pooled walk-forward not computed (boom)"
    none = V.filter_verdict(None, True)
    assert none["validated"] is False and none["reasons"][0] == "pooled walk-forward not computed"


def test_filter_verdict_never_validates_a_bucket_outside_the_filter_buckets():
    v = V.filter_verdict(_pooled_block(), False)
    assert v["applies"] is False and v["validated"] is False
    assert v["caught_ok"] is True and v["kept_ok"] is True        # the numbers are still shown
    assert v["reasons"] == ["not a filter bucket (FILTER_BUCKETS: 3day, medium)"]


def test_filter_verdict_reads_the_rules_at_call_time(monkeypatch):
    monkeypatch.setattr(C, "FILTER_RULES", {"min_caught_lower": 0.4, "min_kept_minus_all_lower": 0})
    assert V.filter_verdict(_pooled_block(), True)["validated"] is False


def _res_for_report():
    passed = {**_block(), "verdict": V.filter_verdict(_pooled_block(), True),
              "test_at_cutoff": {"cutoff": 0.7, **V.collapse_filter_stats(Y_COLL, Y_RUN, RET,
                                                                          FLAG)}}
    failed = {**_block(), "verdict": V.filter_verdict(_pooled_block(kept_minus_all_lo=-1.0),
                                                      True)}
    short = {**_block(), "verdict": V.filter_verdict(_pooled_block(), False)}
    return {"buckets": {"short": {"collapse_filter": short}, "3day": {"collapse_filter": passed},
                        "medium": {"collapse_filter": failed}, "long": {"skipped": "too few"}},
            "collapse_reference": {"short": {"skipped": "no collapse model"},
                                   "3day": {"cutoff": 0.7, "n": 560},
                                   "medium": {"cutoff": 0.41234, "n": 133}}}


def test_verdict_table_reasons_and_note():
    text = R.filter_verdict_section(_res_for_report())
    assert text.startswith("### Collapse filter verdict (owner rule of 18 Oct 2026)\n")
    assert ("| bucket | collapse filter validated | pooled collapses caught 95% lower end > 0.3 "
            "| pooled kept minus all (mean net %) 95% lower end > 0 | counted weeks "
            "| cut-off (probability) | reference rows n "
            "| test-part flagged share at the cut-off (information) |") in text
    assert "| 3day | yes | yes: 0.350 | yes: +0.5 | 4 | 0.700 | 560 | 0.300 |" in text
    assert "| medium | no | yes: 0.350 | no: -1.0 | 4 | 0.412 | 133 | n/a |" in text
    assert ("| short | n/a (not a filter bucket) | yes: 0.350 | yes: +0.5 | 4 "
            "| n/a: no collapse model | n/a | n/a |") in text
    assert "| long | skipped |" in text
    assert ("- `medium` not validated: pooled kept minus all (mean net %) 95% lower end "
            "-1.000 <= 0") in text
    assert "`3day` not validated" not in text and "`short` not validated" not in text
    assert "Never a bucket gate: the PASS/FAIL above is unchanged." in text
    assert C.FILTER_RULES_NOTE in text


def test_section_verdict_line_and_the_fixed_cutoff_column():
    res = _res_for_report()
    ok = R.collapse_filter_section(res["buckets"]["3day"]["collapse_filter"])
    assert "**Collapse filter verdict (owner rule of 18 Oct 2026): validated.**" in ok
    assert ("|  | test part (saved model) | test part at the fixed cut-off 0.700 (information) "
            "| pooled walk-forward (2 counted weeks) [95%] | random skip (expected) |") in ok
    assert "| calls | 10 | 10 | 10 |" in ok
    assert "likely optimistic for such a filter." in ok
    bad = R.collapse_filter_section(res["buckets"]["medium"]["collapse_filter"])
    assert ("**Collapse filter verdict (owner rule of 18 Oct 2026): not validated (pooled kept "
            "minus all (mean net %) 95% lower end -1.000 <= 0).**") in bad
    # a block without "test_at_cutoff" (e.g. written before 18 Oct 2026): no such column
    assert "|  | test part (saved model) | pooled walk-forward (2 counted weeks) [95%] |" in bad
    short = R.collapse_filter_section(res["buckets"]["short"]["collapse_filter"])
    assert "verdict (owner rule of 18 Oct 2026): n/a (not a filter bucket" in short
