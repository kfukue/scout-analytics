import numpy as np
import pandas as pd
import pytest

from scout_ml import validate as V


@pytest.fixture(scope="module")
def calls():
    rng = np.random.default_rng(0)
    n = 3000
    dates = pd.Timestamp("2026-01-01", tz="UTC") + pd.to_timedelta(np.sort(rng.uniform(0, 100, n)), unit="D")
    tokens = pd.Series(rng.integers(0, 900, n)).map("T{}".format)   # tokens repeat across time
    return pd.Series(dates), tokens


@pytest.mark.parametrize("horizon", [1, 7, 30])
def test_time_split_embargo_and_token_grouping(calls, horizon):
    dates, tokens = calls
    s = V.time_split(dates, tokens, horizon)
    tr, va, te = s["train"], s["val"], s["test"]
    assert not (tr & va).any() and not (tr & te).any() and not (va & te).any()
    for a, b in ((tr, va), (tr, te), (va, te)):                      # no token on both sides
        assert not set(tokens[a]) & set(tokens[b])
    gap = pd.Timedelta(days=horizon)
    assert tr.sum() > 0 and te.sum() > 0
    assert dates[tr].max() < s["t1"] - gap                           # no train row in the embargo
    assert dates[te].min() >= s["t2"]
    if va.any():
        assert dates[va].min() >= s["t1"] and dates[va].max() < s["t2"] - gap
    # Outcome windows of earlier parts end before the next part starts.
    assert dates[tr].max() + gap < dates[va | te].min()


def test_split_is_chronological_70_15_15_without_repeats():
    dates = pd.Series(pd.Timestamp("2026-01-01", tz="UTC") + pd.to_timedelta(np.arange(1000), unit="h"))
    s = V.time_split(dates, pd.Series(np.arange(1000)).astype(str), 0)
    assert (int(s["train"].sum()), int(s["val"].sum()), int(s["test"].sum())) == (700, 150, 150)


def test_walk_forward_windows_expand_and_respect_embargo(calls):
    dates, tokens = calls
    windows = list(V.walk_forward_windows(dates, tokens, 3))
    assert len(windows) >= 12
    sizes = [w["train"].sum() for w in windows]
    assert sizes == sorted(sizes)
    for w in windows:
        assert not set(tokens[w["train"]]) & set(tokens[w["test"]])
        if w["train"].any():
            assert dates[w["train"]].max() < w["start"] - pd.Timedelta(days=3)
        if w["test"].any():
            assert w["start"] <= dates[w["test"]].min()
            assert dates[w["test"]].max() < w["start"] + pd.Timedelta(days=7)


def test_trading_metrics_and_gates():
    y_run = np.array([1, 1, 0, 0, 0, 0, 0, 0, 0, 0] * 2)
    score = np.where(y_run == 1, 0.9, 0.1)
    y_col = np.array([0, 0, 1, 1, 1, 0, 0, 0, 0, 0] * 2)
    ret = np.where(y_run == 1, 150.0, -40.0)
    t = V.trading_metrics(y_run, score, y_col, y_col * 0.8, ret)
    assert t["top_lift"] == pytest.approx(5.0)          # top 10% = 2 calls, both runners
    assert t["collapse_removed"] == pytest.approx(1.0)  # top 30% = exactly the 6 collapses
    assert t["sim_top_mean"] == 150.0 and t["sim_all_mean"] == pytest.approx(-2.0)
    assert V.gates(t, [{"sim_beats_all": True}])["passed"]
    assert not V.gates(t, [{"sim_beats_all": True}, {"sim_beats_all": False}])["passed"]
    assert not V.gates(t, [{"skipped": "x"}])["passed"]                 # nothing evaluated
    no_collapse = V.trading_metrics(y_run, score, y_col, None, ret)
    assert not V.gates(no_collapse, [{"sim_beats_all": True}])["collapse_ok"]


def test_metrics_do_not_crash_on_one_class():
    m = V.model_metrics(np.zeros(20), np.full(20, 0.1))
    assert np.isnan(m["roc_auc"]) and m["base_rate"] == 0.0
    assert len(V.calibration_table(np.arange(100) % 2, np.linspace(0, 1, 100))) == 10
