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
        row(ret_late_7d=10, tracking_status="gave_up"),
        row(ret_late_7d=None, entry_late_price_usd=None, tracking_status="repeat"),  # untracked repeat call
        row(ret_late_7d=10, tracking_status="repeat")])  # set aside after it was tracked: results kept
    L = build_labels(df)
    assert L["usable_medium"].tolist() == [True, False, False, False, False, False, False]
    assert L["no_pool"].tolist() == [False, False, False, True, True, True, False]
    assert L["repeat"].tolist() == [False, False, False, False, False, True, True]
    assert np.isnan(L["runner_medium"][1]) and np.isnan(L["runner_medium"][6])


def test_repeat_calls_are_never_usable_whatever_their_status():
    """Only the first call of a token counts (lowest message_date, then call_id;
    address case ignored; update posts are never a first call). Later calls are
    excluded even when an older tracker left them `done` with results."""
    df = pd.DataFrame([
        row(call_id=1, contract_address="0xAAA", message_date="2026-08-01T10:00:00Z", ret_late_7d=10),
        row(call_id=2, contract_address="0xaaa", message_date="2026-08-02T10:00:00Z", ret_late_7d=10),
        row(call_id=3, contract_address="0xBBB", message_date="2026-08-01T09:00:00Z", ret_late_7d=10,
            post_kind="update"),                                        # update first: not a call
        row(call_id=4, contract_address="0xBBB", message_date="2026-08-01T11:00:00Z", ret_late_7d=10),
        row(call_id=6, contract_address="0xCCC", message_date="2026-08-03T10:00:00Z", ret_late_7d=10),
        row(call_id=5, contract_address="0xCCC", message_date="2026-08-03T10:00:00Z", ret_late_7d=10),
        row(call_id=7, contract_address="0xDDD", message_date="2026-08-04T10:00:00Z", ret_late_7d=10,
            tracking_status="repeat")])                                 # flagged by the tracker
    L = build_labels(df)
    assert L["repeat"].tolist() == [False, True, False, False, True, False, True]
    assert L["update"].tolist() == [False, False, True, False, False, False, False]
    assert L["usable_medium"].tolist() == [True, False, False, True, False, True, False]
    assert L["runner_medium"][[1, 4, 6]].isna().all()


def test_horizon_not_due_yet_is_unlabelled_per_bucket_not_negative():
    """A call still `tracking`: 1d/3d/7d computed, 30d not due (NULL in the
    view). Short buckets use it; `long` leaves it out (NaN), even when the
    tracker has already flagged it rugged (rugs are flagged at once)."""
    early = dict(max_gain_late_1d=150, ret_late_1d=-60, ret_late_3d=-70, ret_late_7d=-80)
    df = pd.DataFrame([
        row(tracking_status="tracking", ret_late_30d=None, **early),
        row(tracking_status="tracking", ret_late_30d=None, rugged="true", **early),
        row(tracking_status="tracking", ret_late_30d="", rugged="false", **early),  # CSV empty string
        row(tracking_status="done", ret_late_30d=-95.0, **early),
        row(tracking_status="tracking", max_gain_late_1d=None, ret_late_1d=10)])     # 1d half-known
    L = build_labels(df)
    assert L["usable_long"].tolist() == [False, False, False, True, False]
    assert L["runner_long"][:3].isna().all() and L["collapse_long"][:3].isna().all()
    assert L["net_ret_long"][:3].isna().all()
    assert L["collapse_long"][3] == 1 and L["runner_long"][3] == 0
    for b in ("short", "3day", "medium"):
        assert L[f"usable_{b}"][:4].all(), b
        assert (L[f"collapse_{b}"][:4] == 1).all(), b
    assert L["runner_short"][:4].tolist() == [1, 1, 1, 1]
    assert not L["usable_short"][4]                       # needs max_gain_late_1d too


def test_update_posts_are_never_usable():
    df = pd.DataFrame([
        row(ret_late_7d=10, post_kind="call"),
        row(ret_late_7d=10, post_kind="update"),   # tracked before it was known to be an update
        row(ret_late_7d=10, post_kind=None),       # not classified yet: read as a call
        row(ret_late_7d=None, entry_late_price_usd=None, tracking_status="repeat", post_kind="update")])
    L = build_labels(df)
    assert L["update"].tolist() == [False, True, False, True]
    assert L["usable_medium"].tolist() == [True, False, True, False]
    assert np.isnan(L["runner_medium"][1]) and np.isnan(L["net_ret_medium"][1])
    # a dataset exported before the column existed has no update rows
    assert not build_labels(df.drop(columns="post_kind"))["update"].any()


def test_thresholds_come_from_config(monkeypatch):
    monkeypatch.setitem(BUCKETS["3day"], "runner", ("ret_late_3d", ">=", 10.0))
    assert build_labels(pd.DataFrame([row(ret_late_3d=12)]))["runner_3day"][0] == 1


def test_extreme_outcomes_are_excluded_not_clipped():
    df = pd.DataFrame([
        row(ret_late_7d=10),                                       # normal
        row(ret_late_7d=2e5),                                      # medium ret above the cap
        row(ret_late_7d=10, max_gain_late_1d=float("inf")),        # short runner column: inf
        row(ret_late_7d=1e5),                                      # exactly the cap: kept
        row(ret_late_7d=-100.0),                                   # total loss: a real label
        row(ret_late_7d=10, max_gain_7d=1e9, ret_7d=1e9),          # huge but not label-relevant
        row(ret_late_7d=10, ret_late_30d=3.9e47),                  # long bucket column -> all buckets
        row(ret_late_7d=None)])                                    # missing: not extreme
    L = build_labels(df)
    assert L["extreme"].tolist() == [False, True, True, False, False, False, True, False]
    assert L["usable_medium"].tolist() == [True, False, False, True, True, True, False, False]
    for b in BUCKETS:
        assert not L[f"usable_{b}"][L["extreme"]].any(), b
        assert L[f"net_ret_{b}"][L["extreme"]].isna().all(), b
    assert L["collapse_medium"][4] == 1 and L["net_ret_medium"][4] == pytest.approx(-100.0)
    assert L["net_ret_medium"][3] == pytest.approx(1e5)          # kept as is, never clipped


def test_outcome_columns_cover_every_label_and_simulation_column():
    from scout_ml.labels import outcome_columns
    cols = set(outcome_columns())
    for cfg in BUCKETS.values():
        assert {cfg["runner"][0], cfg["collapse"][0], cfg["ret_col"]} <= cols


def test_cap_comes_from_config(monkeypatch):
    from scout_ml import labels
    monkeypatch.setattr(labels, "MAX_OUTCOME_PCT", 50.0)
    assert build_labels(pd.DataFrame([row(ret_late_7d=60)]))["extreme"][0]


def _dead_df():
    """1d/3d/7d outcomes for: a dead flat call, a dead collapse, a busy flat call,
    a flat call whose trades_24h is unknown, and one exactly at the threshold."""
    flat = dict(max_gain_late_1d=5, ret_late_1d=-5, ret_late_3d=-8, ret_late_7d=-10)
    return pd.DataFrame([
        row(**flat, trades_24h=12),
        row(max_gain_late_1d=5, ret_late_1d=-80, ret_late_3d=-80, ret_late_7d=-90, trades_24h=3),
        row(**flat, trades_24h=400),
        row(**flat, trades_24h=None),
        row(**flat, trades_24h=50)])


def test_dead_calls_become_collapses_in_every_bucket_and_stay_usable():
    from scout_ml import config as C
    from scout_ml.labels import dead_policy
    df = _dead_df()
    assert C.DEAD_IS_COLLAPSE and C.DEAD_TRADES_24H == 50 and dead_policy(df) == "on"
    L = build_labels(df)
    assert L["dead"].tolist() == [True, True, False, False, False]    # NULL / 50 are not dead
    for b in ("short", "3day", "medium"):
        assert L[f"usable_{b}"].all(), b                                 # dead calls stay in the data
        assert L[f"collapse_plain_{b}"].tolist() == [0, 1, 0, 0, 0], b
        assert L[f"collapse_{b}"].tolist() == [1, 1, 0, 0, 0], b         # NULL keeps the plain label
        assert L[f"runner_{b}"].tolist() == [0, 0, 0, 0, 0], b           # runners untouched


def test_dead_rule_can_be_switched_off_and_threshold_comes_from_config(monkeypatch):
    from scout_ml import config as C
    from scout_ml.labels import dead_policy
    df = _dead_df()
    monkeypatch.setattr(C, "DEAD_IS_COLLAPSE", False)
    assert dead_policy(df) == "off"
    L = build_labels(df)
    assert L["collapse_short"].tolist() == L["collapse_plain_short"].tolist() == [0, 1, 0, 0, 0]
    assert L["dead"].tolist() == [True, True, False, False, False]     # still counted for the report
    monkeypatch.setattr(C, "DEAD_IS_COLLAPSE", True)
    monkeypatch.setattr(C, "DEAD_TRADES_24H", 51)
    assert build_labels(df)["collapse_short"].tolist() == [1, 1, 0, 0, 1]


def test_dead_rule_is_skipped_when_the_column_is_missing():
    from scout_ml.labels import dead_policy
    df = _dead_df().drop(columns="trades_24h")
    assert dead_policy(df) == "missing"
    L = build_labels(df)
    assert not L["dead"].any()
    assert L["collapse_short"].tolist() == L["collapse_plain_short"].tolist() == [0, 1, 0, 0, 0]
