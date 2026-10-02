import numpy as np
import pandas as pd
import pytest

from scout_ml.config import BUCKETS
from scout_ml.labels import build_labels, net_pct


def row(**kw):
    base = {"price_unit": "usd", "entry_late_price_usd": 1.0, "tracking_status": "done",
            "tax_buy_pct": None, "tax_sell_pct": None, "rugged": "false"}
    return {**base, **kw}


def test_net_pct_applies_both_taxes_and_treats_missing_as_zero():
    # +100% with 5% buy and 10% sell tax: 2 * 0.95 * 0.90 - 1 = +71%
    assert net_pct(100.0, 5.0, 10.0) == pytest.approx(71.0)
    assert net_pct(100.0, np.nan, None) == pytest.approx(100.0)
    assert net_pct(-50.0, 0.0, 10.0) == pytest.approx(-55.0)


def test_short_labels_use_late_columns_net_of_tax():
    df = pd.DataFrame([
        row(max_gain_late_1d=120, ret_late_1d=10, max_gain_1d=500),                   # runner
        row(max_gain_late_1d=120, ret_late_1d=10, tax_buy_pct=5, tax_sell_pct=10),    # tax kills it
        row(max_gain_late_1d=5, ret_late_1d=-45, tax_sell_pct=10),                    # -50.5 net
        row(max_gain_late_1d=5, ret_late_1d=-45)])                                    # -45: no
    L = build_labels(df)
    assert L["runner_short"].tolist() == [1, 0, 0, 0]
    assert L["collapse_short"].tolist() == [0, 0, 1, 0]
    assert L["net_ret_short"][2] == pytest.approx(-50.5)


def test_thresholds_per_bucket_and_long_rugged_rule():
    df = pd.DataFrame([
        row(ret_late_3d=50, ret_late_7d=49.9, ret_late_30d=0.0),
        row(ret_late_3d=-60, ret_late_7d=-69, ret_late_30d=-20, rugged="true"),
        row(ret_late_3d=-10, ret_late_7d=-70, ret_late_30d=-90, rugged=None)])
    L = build_labels(df)
    assert L["runner_3day"].tolist() == [1, 0, 0] and L["collapse_3day"].tolist() == [0, 1, 0]
    assert L["runner_medium"].tolist() == [0, 0, 0] and L["collapse_medium"].tolist() == [0, 0, 1]
    assert L["runner_long"].tolist() == [0, 0, 0]          # "> 0" is strict
    assert L["collapse_long"].tolist() == [0, 1, 1]        # rugged OR <= -90
    assert not L["usable_short"].any()                     # 1d outcome missing


def test_usable_needs_usd_outcome_and_a_pool():
    df = pd.DataFrame([
        row(ret_late_7d=10),
        row(ret_late_7d=10, price_unit="native"),
        row(ret_late_7d=None),
        row(ret_late_7d=10, entry_late_price_usd=None),
        row(ret_late_7d=10, tracking_status="gave_up")])
    L = build_labels(df)
    assert L["usable_medium"].tolist() == [True, False, False, False, False]
    assert L["no_pool"].tolist() == [False, False, False, True, True]
    assert np.isnan(L["runner_medium"][1])


def test_thresholds_come_from_config(monkeypatch):
    monkeypatch.setitem(BUCKETS["3day"], "runner", ("ret_late_3d", ">=", 10.0))
    assert build_labels(pd.DataFrame([row(ret_late_3d=12)]))["runner_3day"][0] == 1
