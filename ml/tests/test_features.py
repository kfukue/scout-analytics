import numpy as np
import pandas as pd
import pytest

from scout_ml import config as C
from scout_ml.features import assert_no_leakage, build_features, learn_cat_levels


def test_guard_raises_on_any_forbidden_column():
    for col in ["ret_late_1d", "max_gain_7d", "max_dd_late_30d", "rugged", "tracking_status",
                "entry_late_price_usd", "current_liquidity_usd", "price_unit", "pool_dex",
                "post_kind", "latest_price_usd", "latest_return_pct", "latest_checked_at",
                "latest_trade_at", "latest_anything_added_later"]:
        with pytest.raises(ValueError, match="forbidden"):
            assert_no_leakage(["mcap_usd", col])
    assert not [c for c in C.FEATURES if c.startswith("latest_")]
    assert set(C.LATEST) <= C.FORBIDDEN_COLUMNS
    assert_no_leakage(C.FEATURES)              # the configured list itself is clean
    assert not C.is_forbidden("quote_asset")   # explicitly allowed


def test_build_features_refuses_a_leaky_feature_list(synthetic_df):
    with pytest.raises(ValueError, match="forbidden"):
        build_features(synthetic_df.assign(ret_x=1.0), {}, columns=C.FEATURES[:3] + ["ret_late_1d"])


def test_outcomes_in_the_input_never_reach_the_matrix(synthetic_df):
    X = build_features(synthetic_df, learn_cat_levels(synthetic_df))
    assert list(X.columns) == C.FEATURES and len(X) == len(synthetic_df)
    assert not [c for c in X.columns if C.is_forbidden(c)]
    # Changing every forbidden column must leave the features untouched.
    shuffled = synthetic_df.copy()
    for c in shuffled.columns:
        if C.is_forbidden(c) and c not in ("message_date", "pre_vol_unit"):  # real inputs
            shuffled[c] = shuffled[c].sample(frac=1, random_state=1).to_numpy()
    pd.testing.assert_frame_equal(X, build_features(shuffled, learn_cat_levels(synthetic_df)))


def test_derived_features_and_usd_only_volumes():
    base = {"mcap_usd": 200.0, "liq_usd": 50.0, "called_at_mcap_usd": 100.0,
            "live_buys_elite_usd": 10.0, "live_buys_good_usd": 15.0,
            "pre_buys_5m": 3, "pre_sells_5m": 1, "pre_buy_vol_5m": 30.0, "pre_sell_vol_5m": 10.0,
            "message_date": "2026-10-02T15:30:00Z", "dex": "Uniswap V4"}
    X = build_features([{**base, "pre_vol_unit": "usd"}, {**base, "pre_vol_unit": "SOL"},
                        {"liq_usd": 5.0, "mcap_usd": 0, "dex": "never_seen"}],
                       {"dex_family": ["uniswap_v4"]})
    a, b, c = X.iloc[0], X.iloc[1], X.iloc[2]
    assert (a.liq_to_mcap, a.live_usd_to_liq, a.live_usd_to_mcap, a.mcap_vs_called) == (0.25, 0.5, 0.125, 2.0)
    assert a.pre_buy_sell_ratio_5m == 2.0 and a.pre_buy_vol_share_5m == 0.75
    assert (a.hour_utc, a.weekday_utc, a.dex_family) == (15.0, 4.0, 0.0)   # 2026-10-02 is a Friday
    assert np.isnan(b.pre_buy_vol_5m) and np.isnan(b.pre_buy_vol_share_5m)
    assert b.pre_buy_sell_ratio_5m == 2.0                            # counts are unit-free
    assert np.isnan(c.liq_to_mcap) and np.isnan(c.dex_family) and np.isnan(c.hour_utc)


def test_single_json_row_matches_batch_row(synthetic_df):
    """Serving path (dict with None / native types) == training path (DataFrame)."""
    levels = learn_cat_levels(synthetic_df)
    batch = build_features(synthetic_df, levels)
    for i in (0, 17, 4321):
        raw = synthetic_df.iloc[i]
        as_json = {k: (None if pd.isna(v) else v.item() if hasattr(v, "item") else v)
                   for k, v in raw.items()}
        np.testing.assert_allclose(build_features(as_json, levels).iloc[0].to_numpy(),
                                   batch.iloc[i].to_numpy(), equal_nan=True)


@pytest.mark.parametrize("col", ["rugged", "current_liquidity_usd", "current_price_usd",
                                 "current_anything_added_later", "entry_price_source",
                                 "pool_dex", "pool_address", "pool_name", "pool_created_at",
                                 "pool_anything_added_later", "latest_price_usd"])
def test_tracker_discoveries_are_forbidden(col):
    assert C.is_forbidden(col), col
    with pytest.raises(ValueError, match="forbidden"):
        assert_no_leakage(["liq_usd", col])


def test_post_fields_known_at_the_call_stay_allowed():
    for col in ("launchpad", "dex", "dex_family", "quote_asset", "liq_usd", "mcap_usd"):
        assert not C.is_forbidden(col), col
    assert "launchpad" in C.FEATURES and "dex_family" in C.FEATURES
    assert C.DEX_INPUTS == "family" and "dex" not in C.FEATURES


def test_feature_list_excludes_columns_constant_for_first_calls(synthetic_df):
    X = build_features(synthetic_df, {})
    for c in ("prior_calls", "secs_since_prev_call"):
        assert c not in C.FEATURES and c not in X.columns, c
    assert list(X.columns) == C.FEATURES
    # no derived feature reads them: changing them changes nothing
    changed = synthetic_df.assign(prior_calls=99, secs_since_prev_call=1.0)
    pd.testing.assert_frame_equal(X, build_features(changed, {}))


def test_categories_match_without_regard_to_case_and_whitespace():
    rows = [{"launchpad": "Raydium"}, {"launchpad": " raydium "}, {"launchpad": "RAYDIUM"},
            {"launchpad": "raydium"}, {"launchpad": "PumpSwap"}, {"launchpad": "never_seen"},
            {"launchpad": "  "}, {"launchpad": None}]
    levels = learn_cat_levels(pd.DataFrame([{"launchpad": "RayDium "}] * 20
                                           + [{"launchpad": "pumpswap"}] * 20))
    assert levels["launchpad"] == ["pumpswap", "raydium"]     # stored normalised
    X = build_features(rows, levels)
    assert X["launchpad"].tolist()[:5] == [1.0, 1.0, 1.0, 1.0, 0.0]
    assert X["launchpad"][5:].isna().all()                    # unseen / empty -> missing
    # an old flat meta.json (exact_levels) keeps its case-sensitive matching: no skew;
    # raw `dex` is still encoded for a model whose feature list has it
    old_rows = [{"dex": r["launchpad"]} for r in rows]
    old = build_features(old_rows, {"dex": ["PumpSwap", "Raydium", "raydium"]}, columns=["dex"],
                         exact_levels=True)
    assert old["dex"][[0, 1, 3, 4]].tolist() == [1.0, 2.0, 2.0, 0.0]
    assert old["dex"][[2, 5, 6, 7]].isna().all()                # 'RAYDIUM' is no old level


def test_levels_counted_case_insensitively_against_the_minimum():
    """10 'Meteora' + 10 'meteora' are one level with 20 rows (MIN_CATEGORY_COUNT)."""
    df = pd.DataFrame({"launchpad": ["Meteora"] * 10 + ["meteora "] * 10 + ["x"] * 19})
    assert C.MIN_CATEGORY_COUNT == 20
    assert learn_cat_levels(df)["launchpad"] == ["meteora"]


@pytest.mark.parametrize("name, family", [
    ("Pons", "pons"), ("Pons V2", "pons"), ("PONS v2", "pons"), (" pons-v2 ", "pons"),
    ("Uniswap V4", "uniswap_v4"), ("uniswap_v4", "uniswap_v4"), ("UniswapV4", "uniswap_v4"),
    ("uniswap-v3", "uniswap_v3"), ("Uniswap V2", "uniswap_v2"), ("UNISWAP", "uniswap"),
    ("Longxyz", "longxyz"), ("Pools Trade Instant", "pools"), ("O1 Rwa", "o1"),
    ("never_seen", "other"), ("", None), ("   ", None), (None, None)])
def test_dex_family_rules_ignore_case_and_separators(name, family):
    from scout_ml.features import dex_family_of
    assert dex_family_of(name) == family
    X = build_features([{"dex": name}], {"dex_family": ["other", "pons", "uniswap_v4"]})
    want = {"other": 0.0, "pons": 1.0, "uniswap_v4": 2.0}.get(family)
    assert (np.isnan(X["dex_family"][0]) if want is None else X["dex_family"][0] == want)


def test_dex_family_rules_come_from_config_first_match_wins(monkeypatch):
    from scout_ml.features import dex_family_of
    monkeypatch.setattr(C, "DEX_FAMILY_RULES", (("uniswap", "any_uniswap"), ("uniswapv4", "v4")))
    assert dex_family_of("Uniswap V4") == "any_uniswap"
    monkeypatch.setattr(C, "DEX_FAMILY_OTHER", "misc")
    assert dex_family_of("Pons") == "misc"


def test_dex_family_levels_are_learned_like_other_categories():
    df = pd.DataFrame({"dex": ["Pons"] * 12 + ["Pons V2"] * 12 + ["Uniswap V4"] * 19 + [None] * 30})
    assert learn_cat_levels(df)["dex_family"] == ["pons"]    # 24 Pons rows; 19 v4 < 20


@pytest.mark.parametrize("col", ["trades_24h", "trades_7d", "trades_anything_added_later"])
def test_trading_after_the_call_is_never_a_feature(col, synthetic_df):
    assert C.is_forbidden(col) and col not in C.FEATURES
    with pytest.raises(ValueError, match="forbidden"):
        assert_no_leakage(["liq_usd", col])
    with pytest.raises(ValueError, match="forbidden"):
        build_features(synthetic_df.head(5), {}, columns=C.FEATURES[:3] + [col])
    assert not C.is_forbidden("pre_swaps_60m")             # trading BEFORE the call stays allowed


def test_old_model_feature_list_with_removed_columns_still_builds():
    """A meta.json from before the change lists prior_calls: serving must not crash."""
    cols = C.FEATURES[:3] + ["prior_calls", "secs_since_prev_call"]
    X = build_features({"prior_calls": 0, "mcap_usd": 1.0}, {}, columns=cols)
    assert list(X.columns) == cols and X["prior_calls"][0] == 0.0
    assert np.isnan(X["secs_since_prev_call"][0])


# The owner's posted DEX names (RUNBOOK.md query (g), 28 Jul - 8 Oct 2026; lower-cased,
# as the query prints them) and the family each must get.
OWNER_DEX_NAMES = {
    "pons v2": "pons", "uniswap v4": "uniswap_v4", "longxyz": "longxyz",
    "uniswap v3": "uniswap_v3", "pons": "pons", "pools trade instant": "pools",
    "bankr": "bankr", "o1 rwa": "o1", "letscash": "letscash", "varo": "varo", "flap": "flap",
    "sushiswap": "sushi", "lunch pair v4": "lunch", "uniswap v2": "uniswap_v2",
    "sushi": "sushi", "virtuals v2": "other", "pair fund": "other", "flap stocks": "flap",
    "pools fun": "pools", "noxa": "other", "bags": "other", "lunch pair v3": "lunch",
    "stonkbroker": "other", "lemonswap": "other", "trench": "other", "coinbarrel": "other",
    "pools trade cca": "pools", "stonkbroker v2": "other", "o1": "o1", "orbofi": "other",
    "bowfun": "other", "clanker": "other", "flap pve": "flap", "lunch v3": "lunch",
    "nasdank v3": "other", "noxafi": "other", "arenatrade": "other", "pmav": "other",
    "klik": "other", "livo": "other", "realfun": "other", "robinfun": "other",
    "dyorfun v3": "other", "dontblink": "other", "up": "other"}


def test_owner_dex_list_maps_to_the_agreed_families():
    from scout_ml.features import dex_family_of
    assert len(OWNER_DEX_NAMES) == 45
    got = {name: dex_family_of(name) for name in OWNER_DEX_NAMES}
    assert got == OWNER_DEX_NAMES
    # the same names as posted (any case / separators) give the same family
    for name, family in OWNER_DEX_NAMES.items():
        assert dex_family_of(name.title().replace(" ", "_")) == family, name
    assert dex_family_of("orbofi") == "other" and dex_family_of("O1") == "o1"
    assert dex_family_of("noxa") == dex_family_of("noxafi") == "other"
    # every configured family is reached by some posted name, except plain "uniswap"
    families = {f for _, f in C.DEX_FAMILY_RULES}
    assert families - set(got.values()) == {"uniswap"}
