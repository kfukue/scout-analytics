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


def test_token_grouping_ignores_letter_case():
    """0xAbC and 0xabc are one token: both calls land on the same side."""
    dates = pd.Series(pd.Timestamp("2026-08-01", tz="UTC") + pd.to_timedelta(np.arange(100), unit="D"))
    tokens = pd.Series([f"0xT{i}" for i in range(100)])
    tokens[95] = "0xt5"                                  # a late call of token 5, other case
    s = V.time_split(dates, tokens, 1)
    assert s["train"][5] and not s["test"][95] and not s["val"][95]
    for w in V.walk_forward_windows(dates, tokens, 1):
        assert not w["test"][95]


@pytest.mark.parametrize("bucket,windows,with_train", [
    ("short", 10, 10), ("3day", 10, 10), ("medium", 9, 8), ("long", 6, 2)])
def test_fold_counts_for_the_first_real_dataset(bucket, windows, with_train):
    """First real export: calls from 25 Jul 2026, exported 7 Oct 2026; a bucket
    has labels up to the export minus its horizon. Documents how many weekly
    walk-forward windows each bucket gets (see RUNBOOK.md)."""
    from scout_ml.config import BUCKETS
    h = BUCKETS[bucket]["horizon_days"]
    start, export = pd.Timestamp("2026-07-25", tz="UTC"), pd.Timestamp("2026-10-07T12:00", tz="UTC")
    end = export - pd.Timedelta(days=h)
    n = int(4000 * (end - start) / (export - start))
    dates = pd.Series(start + (end - start) * np.linspace(0, 1, n))
    tokens = pd.Series(np.arange(n)).astype(str)
    ws = list(V.walk_forward_windows(dates, tokens, h))
    assert len(ws) == windows
    assert sum(bool(w["train"].any()) for w in ws) == with_train
    if bucket == "long":                 # 44 days of matured 30d calls: skipped until 120
        import train
        assert train.maturity_skip(bucket, dates).startswith("not enough matured 30d data (")
    if bucket == "medium":               # forward split: test = last 14 days, 7-day embargo
        s = V.forward_split(dates, tokens, h)
        assert 0.15 * n < s["test"].sum() < 0.30 * n and s["train"].sum() > 0.6 * n


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


def test_one_huge_winner_cannot_decide_the_simulation(monkeypatch):
    """Top 10% = 2 calls: -90% and one +50,000% winner (below MAX_OUTCOME_PCT,
    so not excluded). The other 18 calls make +600%. Uncapped, the one winner
    makes the top beat buy-everything; capped at SIM_MAX_RET_PCT it does not."""
    n = 20
    score = np.arange(n, 0, -1, dtype=float)          # rows 0 and 1 are the top 10%
    ret = np.full(n, 600.0)
    ret[0], ret[1] = -90.0, 50_000.0
    y = np.zeros(n)
    y[[0, 2, 3]] = 1
    capped = V.trading_metrics(y, score, y, None, ret)
    assert V.SIM_MAX_RET_PCT == 1000.0
    assert capped["sim_n_top"] == 2
    assert capped["sim_top_mean"] == pytest.approx((-90 + 1000) / 2), capped
    assert capped["sim_all_mean"] == pytest.approx((-90 + 1000 + 18 * 600) / n), capped
    assert capped["sim_beats_all"] is False, capped
    monkeypatch.setattr(V, "SIM_MAX_RET_PCT", np.inf)
    uncapped = V.trading_metrics(y, score, y, None, ret)
    assert uncapped["sim_beats_all"] is True, uncapped    # what the cap prevents
    # the cap never touches losses: -100% stays -100%
    all_lost = V.trading_metrics(y, score, y, None, np.full(n, -100.0))
    assert all_lost["sim_top_mean"] == -100.0 and all_lost["sim_all_mean"] == -100.0


@pytest.mark.parametrize("horizon", [7, 30])
def test_forward_split_test_strictly_after_train_plus_embargo(calls, horizon):
    dates, tokens = calls
    s = V.forward_split(dates, tokens, horizon)
    tr, te = s["train"], s["test"]
    assert "val" not in s and tr.sum() > 0 and te.sum() > 0 and not (tr & te).any()
    assert not set(tokens[tr]) & set(tokens[te])
    gap = pd.Timedelta(days=horizon)
    # time-based boundary: a midnight UTC date, test_days before the newest call
    assert s["t_test"] == s["t_test"].floor("D")
    assert s["t_test"] == (dates.max() - pd.Timedelta(days=V.FORWARD_TEST_DAYS)).floor("D")
    assert s["t_train_end"] == s["t_test"] - gap
    assert dates[tr].max() < s["t_test"] - gap                   # every train outcome window ends
    assert dates[tr].max() + gap < dates[te].min()               # before the first test call
    assert dates[te].min() >= s["t_test"]
    # everything after the test date that is not test was dropped by token grouping,
    # everything between train and test by the embargo
    in_gap = ((dates >= s["t_test"] - gap) & (dates < s["t_test"])).to_numpy()
    assert s["n_dropped_embargo"] == int(in_gap.sum())
    assert s["n_dropped_embargo"] + s["n_dropped_token"] + tr.sum() + te.sum() == len(dates)


def test_forward_split_boundary_is_a_date_not_a_row_quantile():
    """Adding many calls early in the period moves a row quantile, not the test date."""
    base = pd.Timestamp("2026-08-01", tz="UTC")
    dates = pd.Series(base + pd.to_timedelta(np.arange(0, 60, 0.25), unit="D"))
    crowded = pd.concat([pd.Series(base + pd.to_timedelta(np.linspace(0, 5, 2000), unit="D")),
                         dates], ignore_index=True)
    a = V.forward_split(dates, pd.Series(np.arange(len(dates))).astype(str), 7)
    b = V.forward_split(crowded, pd.Series(np.arange(len(crowded))).astype(str), 7)
    assert a["t_test"] == b["t_test"] == pd.Timestamp("2026-09-15", tz="UTC")
    assert a["test"].sum() == b["test"].sum()


@pytest.mark.parametrize("horizon", [7, 30])
def test_purged_kfold_purges_both_sides_and_stays_inside_train(calls, horizon):
    dates, tokens = calls
    s = V.forward_split(dates, tokens, horizon)
    d_tr, t_tr = dates[s["train"]].reset_index(drop=True), tokens[s["train"]].reset_index(drop=True)
    folds = list(V.purged_kfold(d_tr, t_tr, horizon, 5))
    assert len(folds) == 5
    gap = pd.Timedelta(days=horizon)
    held_all = np.zeros(len(d_tr), dtype=bool)
    for f in folds:
        fit, held = f["fit"], f["held"]
        assert len(fit) == len(held) == len(d_tr)                 # train rows only: no test row
        assert held.any() and not (fit & held).any() and not (held_all & held).any()
        held_all |= held
        lo, hi = d_tr[held].min(), d_tr[held].max()
        assert (d_tr[held] >= lo).all() and (d_tr[held] <= hi).all()
        # contiguous in time: no other row lies inside the fold's span
        assert not ((d_tr >= lo) & (d_tr <= hi) & ~pd.Series(held)).any()
        # purge: no fit row's outcome window [t, t + h] overlaps a held-out window
        f_d = d_tr[fit]
        assert not ((f_d + gap >= lo) & (f_d <= hi + gap)).any()
        assert not set(t_tr[fit]) & set(t_tr[held])
    assert held_all.all()                                        # every train row held out once
    assert d_tr.max() < s["t_test"]                              # folds never reach the test


def test_skip_threshold_leaves_enough_purged_folds():
    """A long bucket exactly at its MIN_MATURED threshold (rows spread evenly
    over span_days, a 15% minority class) gets at least KFOLD_MIN_FOLDS usable
    folds: the skip rule and the k-fold agree."""
    from scout_ml import config as C
    need, h = C.MIN_MATURED["long"], C.BUCKETS["long"]["horizon_days"]
    n = need["rows"]
    start = pd.Timestamp("2026-07-28", tz="UTC")
    dates = pd.Series(start + pd.to_timedelta(np.linspace(0, need["span_days"], n), unit="D"))
    tokens = pd.Series(np.arange(n)).astype(str)
    y = (np.arange(n) % 20 < 3).astype(float)                    # 15% positives, spread evenly
    s = V.forward_split(dates, tokens, h)
    d_tr, y_tr = dates[s["train"]].reset_index(drop=True), y[s["train"]]
    usable = 0
    for f in V.purged_kfold(d_tr, tokens[s["train"]], h, C.KFOLD_K):
        fit, held = y_tr[f["fit"]], y_tr[f["held"]]
        usable += (min(fit.sum(), len(fit) - fit.sum()) >= C.MIN_CLASS_ROWS
                   and min(held.sum(), len(held) - held.sum()) >= C.MIN_CLASS_ROWS // 3)
    assert usable >= C.KFOLD_MIN_FOLDS, usable


def test_lift_interval_wilson():
    lo, hi = V.wilson(8, 31)
    assert lo == pytest.approx(0.1365, abs=1e-3) and hi == pytest.approx(0.4316, abs=1e-3)
    assert V.wilson(0, 10)[0] == 0.0 and V.wilson(10, 10)[1] == 1.0
    assert np.isnan(V.wilson(0, 0)[0])
    lo_l, hi_l = V.lift_interval(8, 31, 43, 307)
    base = 43 / 307
    assert (lo_l, hi_l) == pytest.approx((lo / base, hi / base))
    assert lo_l < (8 / 31) / base < hi_l
    assert all(np.isnan(V.lift_interval(0, 10, 0, 100)))           # no runners: undefined


def test_trading_metrics_report_lift_counts_and_interval():
    y = np.array([1, 1, 0, 0, 0, 0, 0, 0, 0, 0] * 2)
    t = V.trading_metrics(y, np.where(y == 1, 0.9, 0.1), y, None, np.zeros(20))
    assert (t["top_pos"], t["top_n"], t["all_pos"], t["all_n"]) == (2, 2, 4, 20)
    assert t["top_lift_lo"] < t["top_lift"] == pytest.approx(5.0) and t["top_lift_hi"] <= 5.0
    assert (t["top_lift_lo"], t["top_lift_hi"]) == pytest.approx(V.lift_interval(2, 2, 4, 20))


def test_time_split_counts_embargo_and_token_drops_apart():
    dates = pd.Series(pd.Timestamp("2026-08-01", tz="UTC") + pd.to_timedelta(np.arange(100), unit="D"))
    tokens = pd.Series([f"0xT{i}" for i in range(100)])
    s = V.time_split(dates, tokens, 3)
    assert s["n_dropped_token"] == 0
    assert s["n_dropped_embargo"] == 100 - s["train"].sum() - s["val"].sum() - s["test"].sum() == 6
    tokens[95] = "0xt5"                                            # a later call of a train token
    s = V.time_split(dates, tokens, 3)
    assert s["n_dropped_token"] == 1 and s["n_dropped_embargo"] == 6


def _platt_data(n=600, seed=3, recent_pos_rate=0.3):
    """Held-out margins over 40 days; the outcome base rate drops in the last 14."""
    rng = np.random.default_rng(seed)
    dates = pd.Series(pd.Timestamp("2026-09-01", tz="UTC")
                      + pd.to_timedelta(np.sort(rng.uniform(0, 40, n)), unit="D"))
    raw = rng.normal(0, 1, n)
    recent = (dates >= dates.max() - pd.Timedelta(days=14)).to_numpy()
    base = np.where(recent, recent_pos_rate, 0.6)
    y = (rng.random(n) < base * 1.6 / (1 + np.exp(-raw))).astype(int)
    return raw, y, dates, recent


def test_recent_calibration_uses_the_last_days_of_held_out_rows():
    from scout_ml.model import apply_calibrator, fit_platt, fit_platt_recent
    raw, y, dates, recent = _platt_data()
    cal, info = fit_platt_recent(raw, y, dates)
    assert info["used"] == "recent" and info["note"] is None
    assert info["rows_recent"] == int(recent.sum()) and info["rows_all"] == len(y)
    assert info["pos_recent"] + info["neg_recent"] == info["rows_recent"]
    # the recent fit follows the lower recent base rate; the all-rows fit does not
    p_recent = apply_calibrator(cal, raw[recent]).mean()
    p_all = apply_calibrator(fit_platt(raw, y), raw[recent]).mean()
    assert abs(p_recent - y[recent].mean()) < 0.02 < abs(p_all - y[recent].mean())
    # monotone: the ranking of the scores is unchanged
    assert (np.argsort(apply_calibrator(cal, raw), kind="stable")
            == np.argsort(raw, kind="stable")).all()


def test_recent_calibration_falls_back_with_too_few_of_a_class(monkeypatch):
    from scout_ml import config as C
    from scout_ml.model import fit_platt, fit_platt_recent
    raw, y, dates, recent = _platt_data(recent_pos_rate=0.02)
    pos = int(y[recent].sum())
    assert pos < C.CALIB_MIN_CLASS_ROWS
    cal, info = fit_platt_recent(raw, y, dates)
    assert info["used"] == "all (fallback)" and f"{pos} positives" in info["note"]
    np.testing.assert_allclose(cal.coef_, fit_platt(raw, y).coef_)
    monkeypatch.setattr(C, "CALIB_RECENT_DAYS", 1000)      # window from config: everything is recent
    assert fit_platt_recent(raw, y, dates)[1]["used"] == "recent"


def test_recent_calibration_never_inverts_the_ranking(monkeypatch):
    from scout_ml import config as C
    from scout_ml.model import fit_platt_recent, platt_slope
    monkeypatch.setattr(C, "CALIB_RECENT_DAYS", 5)
    raw, y, dates, _ = _platt_data()
    recent = (dates >= dates.max() - pd.Timedelta(days=5)).to_numpy()
    y = y.copy()
    y[recent] = (raw[recent] < 0).astype(int)                # recent rows: anti-correlated
    cal, info = fit_platt_recent(raw, y, dates)
    assert info["used"] == "all (fallback)" and "slope" in info["note"] and platt_slope(cal) > 0


def test_collapse_removed_helper_matches_trading_metrics():
    y = np.array([1, 1, 0, 0, 1, 0, 0, 0, 0, 1])
    score = np.arange(10, dtype=float)
    assert V.collapse_removed(y, score) == pytest.approx(
        V.trading_metrics(None, None, y, score, None)["collapse_removed"])
    assert np.isnan(V.collapse_removed(np.zeros(10), score)) and np.isnan(V.collapse_removed(y, None))
