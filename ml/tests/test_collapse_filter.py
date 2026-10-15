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
    assert text.startswith("### Collapse filter (report only)")
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
    gates the same either way."""
    monkeypatch.setattr(C, "BUCKETS", {"short": C.BUCKETS["short"]})
    ok = train_mod.train(small_df, tmp_path / "ok", "v1", diagnostics=False)
    meta = json.loads((ok / "meta.json").read_text())
    m = meta["metrics"]["short"]
    cf = m["collapse_filter"]
    assert set(cf) == {"flag_frac", "random_skip_expected", "sim_max_ret_pct", "runner_label",
                       "collapse_label", "can_be_both", "test", "pooled", "weeks"}
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
    assert report.count("### Collapse filter (report only)") == 1

    def boom(*a, **k):
        raise RuntimeError("boom")
    monkeypatch.setattr(train_mod, "collapse_filter", boom)
    bad = train_mod.train(small_df, tmp_path / "bad", "v1", diagnostics=False)
    assert "WARNING: the collapse filter of short failed" in capsys.readouterr().out
    m_bad = json.loads((bad / "meta.json").read_text())["metrics"]["short"]
    assert m_bad["collapse_filter"] == {"error": "error: RuntimeError: boom"}
    assert m_bad["gates"] == m["gates"] and m_bad["trading"] == m["trading"]
    assert _hashes(bad) == _hashes(ok) and len(_hashes(ok)) == 2      # byte-identical models
    assert "**Not computed: error: RuntimeError: boom.**" in (bad / "report.md").read_text(
        encoding="utf-8")
