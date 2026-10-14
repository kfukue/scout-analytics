"""Live-buy wallet reputation (scout_ml/wallet_rep.py): only outcomes known
before the call count, never the call's own token, never `rugged` or 30 d
columns; smoothing and NaN rules; not a model input yet. Synthetic data only."""
import numpy as np
import pandas as pd
import pytest

import make_synthetic
from scout_ml import config as C
from scout_ml import wallet_rep as W

T0 = pd.Timestamp("2026-09-01T00:00:00Z")
DAY = pd.Timedelta(days=1)
MIN = pd.Timedelta(minutes=1)
HORIZON_DAYS = {"1h": 1 / 24, "1d": 1, "3d": 3, "7d": 7, "30d": 30}


def lag() -> pd.Timedelta:
    """Time from a call to the moment its 7 d outcome counts as known."""
    return pd.Timedelta(days=C.BUCKETS["medium"]["horizon_days"], hours=C.WREP_KNOWN_LAG_HOURS)


def call(cid, when, token, ret7=None, **extra):
    """One dataset row with just the columns wallet_rep reads (and a few it must not)."""
    row = {"call_id": cid, "message_date": when.strftime("%Y-%m-%dT%H:%M:%SZ"),
           "contract_address": token, "post_kind": "call", "tracking_status": "done",
           "entry_late_price_usd": 1.0, "price_unit": "usd", "tax_buy_pct": 0.0,
           "tax_sell_pct": 0.0, "ret_late_7d": ret7, "ret_late_30d": None, "rugged": "false"}
    row.update(extra)
    return row


def buys(cid, *wallets):
    """Live-buy rows of call `cid`, one per wallet ("0x" + 4 hex + 4 hex)."""
    return [{"call_id": cid, "position": i + 1, "tier": "good", "amount_usd": 100.0,
             "wallet_display": f"{w[:6]}…{w[6:]}", "wallet_prefix": w[:6], "wallet_suffix": w[6:]}
            for i, w in enumerate(wallets)]


def frame(rows, live):
    return W.wrep_frame(pd.DataFrame(rows), pd.DataFrame(live))[0]


@pytest.fixture(scope="module")
def small():
    """1500 synthetic calls over 60 days and their live buys (as read from CSV)."""
    d = make_synthetic.make(n=1500, days=60, seed=3)
    lb = make_synthetic.make_live_buys(d, n_wallets=300, skilled_share=0.05, seed=3)
    return d, lb


def _row(f, i):
    return f.loc[i, list(C.WREP_FEATURES)].astype(float)


def _sample_calls(d, lb, k, seed):
    """k random calls with live buys, posted after the first 10 days."""
    when = pd.to_datetime(d["message_date"], utc=True)
    ok = d["call_id"].isin(lb["call_id"]) & (when > when.min() + 10 * DAY)
    return np.random.default_rng(seed).choice(d.index[ok], k, replace=False)


# --- 1. an outcome known at or after T does not count -------------------------

@pytest.mark.parametrize("offset, counted", [(MIN, False), (pd.Timedelta(0), False), (-MIN, True)])
def test_outcome_known_after_call_excluded(offset, counted):
    """P posted at T - 7d - lag + offset: known_at = T + offset; only strictly
    before T counts (an exact tie does not)."""
    t = T0 + 30 * DAY
    rows = [call(1, t - lag() + offset, "0xAAA", ret7=200.0), call(2, t, "0xBBB")]
    f = frame(rows, buys(1, "0x1111aaaa") + buys(2, "0x1111aaaa"))
    assert f.loc[0, "wrep_n_known"] == 0          # P: no earlier history
    assert f.loc[1, "wrep_n_known"] == (1 if counted else 0)
    assert f.loc[1, "wrep_known_share"] == (1.0 if counted else 0.0)
    assert np.isnan(f.loc[1, "wrep_runner_lift_max"]) != counted


# --- 2. history on C's own token never counts ----------------------------------

def test_same_token_excluded():
    """C is a repeat call (other letter case) of P's token: P does not count for
    C, while P2 on another token, bought by the same wallet, does."""
    t = T0 + 30 * DAY
    rows = [call(1, t - 20 * DAY, "0xAbCdEf", ret7=200.0),
            call(2, t - 15 * DAY, "0x222222", ret7=-95.0),
            call(3, t, "0xABCDEF", tracking_status="repeat")]
    lb = pd.DataFrame(buys(1, "0x1111aaaa") + buys(2, "0x1111aaaa") + buys(3, "0x1111AAAA"))
    df = pd.DataFrame(rows)
    counts = W.wallet_counts(W.call_queries(df, lb), W.wallet_events(W.eligible_history(df), lb))
    c = counts[counts["call_id"] == 3].iloc[0]
    assert (c["n"], c["k_run"], c["k_rug"]) == (1, 0, 1)
    # only the same-token call in history: the wallet is not known at all
    only_same = df[df["call_id"] != 2]
    f = W.wrep_frame(only_same, lb)[0]
    assert f.loc[2, "wrep_n_known"] == 0
    assert np.isnan(f.loc[2, "wrep_runner_lift_max"])


# --- 3. as-of reconstruction -----------------------------------------------------

def test_as_of_reconstruction(small):
    """For random calls C: the features from the full export equal those from
    the rows posted up to T with every outcome not yet known at T blanked
    (and `rugged` unknown), as a live system at T would have seen them."""
    d, lb = small
    full = W.wrep_frame(d, lb)[0]
    when = pd.to_datetime(d["message_date"], utc=True)
    extra = pd.Timedelta(hours=C.WREP_KNOWN_LAG_HOURS)
    known = 0
    for i in _sample_calls(d, lb, 15, seed=1):
        t = when[i]
        sub = d[when <= t].copy()
        sw = when[sub.index]
        for h, days in HORIZON_DAYS.items():
            cols = [c for c in C.OUTCOMES if c.endswith(f"_{h}")]
            sub.loc[sw + pd.Timedelta(days=days) + extra >= t, cols] = np.nan
        sub["rugged"] = None
        part = W.wrep_frame(sub, lb[lb["call_id"].isin(sub["call_id"])])[0]
        pd.testing.assert_series_equal(_row(part, i), _row(full, i), check_names=False)
        known += full.loc[i, "wrep_n_known"] > 0
    assert known >= 10                       # mostly calls with known wallets, not NaN == NaN


# --- 4. fuzz: outcomes not yet known at T do not matter ----------------------

def test_fuzz_unknown_outcomes(small):
    """Randomising every outcome (and `rugged`) of the rows whose 7 d outcome is
    known at or after T, including filling in missing ones, leaves C unchanged."""
    d, lb = small
    full = W.wrep_frame(d, lb)[0]
    when = pd.to_datetime(d["message_date"], utc=True)
    rng = np.random.default_rng(5)
    picked = _sample_calls(d, lb, 10, seed=2)
    assert (full.loc[picked, "wrep_n_known"] > 0).sum() >= 7
    for i in picked:
        late = when + lag() >= when[i]
        fuzzed = d.copy()
        for c in C.OUTCOMES:
            fuzzed[c] = pd.to_numeric(fuzzed[c], errors="coerce").astype(float)
            fuzzed.loc[late, c] = rng.choice([-99.0, -50.0, 0.0, 80.0, 500.0, 1e47],
                                             int(late.sum()))
        fuzzed.loc[late, "rugged"] = rng.random(int(late.sum())) < 0.5
        got = W.wrep_frame(fuzzed, lb)[0]
        pd.testing.assert_series_equal(_row(got, i), _row(full, i), check_names=False)


# --- 5. planted signal --------------------------------------------------------

def test_planted_skilled_wallets(synthetic_df):
    """make_live_buys' skilled wallets buy runners: before any outcome is known
    the lift is NaN (or about 1); once history has matured, calls carrying a
    skilled wallet have a runner lift max above 2, the others do not."""
    n_wallets, share, seed = 2000, 0.02, 7
    lb = make_synthetic.make_live_buys(synthetic_df, n_wallets, share, seed)
    f, stats = W.wrep_frame(synthetic_df, lb)
    assert stats["calls_with_live_buys"] > 0 and stats["eligible_history"] > 0
    when = pd.to_datetime(synthetic_df["message_date"], utc=True)
    early = f.loc[when < when.min() + 7 * DAY, "wrep_runner_lift_max"]
    assert (early.isna() | (early - 1).abs().lt(0.5)).all()
    skilled = {"0x" + h for h in make_synthetic.wallet_digits(int(round(n_wallets * share)), seed)}
    keys = W.wallet_key(lb["wallet_prefix"], lb["wallet_suffix"])
    has = synthetic_df["call_id"].isin(lb.loc[keys.isin(skilled), "call_id"])
    mature = when > when.min() + 60 * DAY
    assert f.loc[mature & has, "wrep_runner_lift_max"].median() > 2
    assert f.loc[mature & ~has, "wrep_runner_lift_max"].median() < 2


# --- 6. / 7. rugged and 30 d columns are never read --------------------------

def test_rugged_flip_changes_nothing(small):
    d, lb = small
    flipped = d.copy()
    flipped["rugged"] = d["rugged"].map(lambda v: not v if isinstance(v, (bool, np.bool_)) else True)
    pd.testing.assert_frame_equal(W.wrep_frame(flipped, lb)[0], W.wrep_frame(d, lb)[0])


def test_extreme_30d_keeps_eligibility(small):
    d, lb = small
    wild = d.copy()
    wild["ret_late_30d"] = 1e47
    wild["max_gain_late_30d"] = 1e47
    pd.testing.assert_frame_equal(W.eligible_history(wild), W.eligible_history(d))
    pd.testing.assert_frame_equal(W.wrep_frame(wild, lb)[0], W.wrep_frame(d, lb)[0])
    t = T0 + 30 * DAY
    rows = [call(1, t - 20 * DAY, "0xAAA", ret7=200.0, ret_late_30d=1e47), call(2, t, "0xBBB")]
    assert W.eligible_history(pd.DataFrame(rows))["call_id"].tolist() == [1]


def test_eligibility_rules():
    """Update posts, repeat calls, no_pool, non-USD rows, missing or extreme
    ret_late_7d are not history; y_run is the medium runner rule net of tax."""
    t = T0
    _, _, thr = C.BUCKETS["medium"]["runner"]
    rows = [call(1, t, "0xA1", ret7=thr + 1.0),
            call(2, t + DAY, "0xa1", ret7=500.0),                         # repeat of 0xA1
            call(3, t, "0xA3", ret7=500.0, post_kind="update"),
            call(4, t, "0xA4", ret7=500.0, tracking_status="no_pool"),
            call(5, t, "0xA5", ret7=500.0, entry_late_price_usd=None),
            call(6, t, "0xA6", ret7=500.0, price_unit="VIRTUAL"),
            call(7, t, "0xA7", ret7=None),
            call(8, t, "0xA8", ret7=C.MAX_OUTCOME_PCT * 10),
            call(9, t, "0xA9", ret7=thr + 1.0, tax_buy_pct=10.0),         # net below threshold
            call(10, t, "0xA10", ret7=C.WREP_RUG_PCT)]
    h = W.eligible_history(pd.DataFrame(rows)).set_index("call_id")
    assert h.index.tolist() == [1, 9, 10]
    assert h["y_run"].tolist() == [True, False, False]
    assert h["y_rug"].tolist() == [False, False, True]
    assert (h["known_at"] == t + lag()).all()
    assert h.loc[1, "token"] == "0xa1"
    empty = pd.DataFrame(columns=make_synthetic.LIVE_BUY_COLUMNS)
    assert W.wrep_frame(pd.DataFrame(rows), empty)[1]["extreme_dropped"] == 1   # call 8


# --- 8. smoothing and NaN rules ------------------------------------------------

def test_posterior_formula():
    assert W.posterior(3, 10, 0.05, 20) == pytest.approx((3 + 1) / 30)
    assert W.posterior(0, 0, 0.15, 20) == pytest.approx(0.15)


def test_aggregate_values_and_nan_rules():
    a, mr, mg = C.WREP_PRIOR_STRENGTH, C.WREP_PRIOR_RUNNER, C.WREP_PRIOR_RUG
    counts = pd.DataFrame({"call_id": [1, 1, 1, 2, 2],
                           "wallet": ["w0", "w1", "w2", "w0", "w3"],
                           "n": [0, 10, 2, 0, 0], "k_run": [0, 5, 0, 0, 0],
                           "k_rug": [0, 0, 2, 0, 0]})
    out = W.aggregate_wallets(counts)
    assert list(out.columns) == list(C.WREP_FEATURES)
    run1, run2 = (5 + a * mr) / (10 + a) / mr, (0 + a * mr) / (2 + a) / mr
    rug1, rug2 = (0 + a * mg) / (10 + a) / mg, (2 + a * mg) / (2 + a) / mg
    r = out.loc[1]
    assert r["wrep_n_known"] == 2 and r["wrep_known_share"] == pytest.approx(2 / 3)
    assert r["wrep_runner_lift_mean"] == pytest.approx((run1 + run2) / 2)
    assert r["wrep_runner_lift_max"] == pytest.approx(max(run1, run2))
    assert r["wrep_rug_lift_mean"] == pytest.approx((rug1 + rug2) / 2)
    assert r["wrep_rug_lift_max"] == pytest.approx(max(rug1, rug2))
    # w1: n=10 >= MIN_CALLS, runner lift 3.67 >= GOOD_LIFT; w2: n=2 < MIN_CALLS
    assert run1 >= C.WREP_GOOD_LIFT and 2 < C.WREP_MIN_CALLS <= 10
    assert r["wrep_good_share"] == pytest.approx(0.5)
    assert r["wrep_bad_share"] == pytest.approx(0.0)
    # K empty: counts are 0, everything about K is NaN
    e = out.loc[2]
    assert e["wrep_n_known"] == 0 and e["wrep_known_share"] == 0
    for c in C.WREP_FEATURES[2:]:
        assert np.isnan(e[c]), c


def test_bad_share_needs_enough_calls():
    """b's rug lift is above WREP_BAD_LIFT, but it has fewer than WREP_MIN_CALLS calls."""
    counts = pd.DataFrame({"call_id": [1, 1], "wallet": ["a", "b"], "n": [10, 4],
                           "k_run": [0, 0], "k_rug": [8, 4]})
    out = W.aggregate_wallets(counts).loc[1]
    assert C.WREP_MIN_CALLS > 4
    assert W.posterior(4, 4, C.WREP_PRIOR_RUG, C.WREP_PRIOR_STRENGTH) / C.WREP_PRIOR_RUG >= C.WREP_BAD_LIFT
    assert out["wrep_bad_share"] == pytest.approx(0.5)


def test_no_live_buys_all_nan():
    t = T0 + 30 * DAY
    rows = [call(1, t - 20 * DAY, "0xAAA", ret7=200.0), call(2, t, "0xBBB"), call(3, t, "0xCCC")]
    f = frame(rows, buys(1, "0x1111aaaa") + buys(2, "0x1111aaaa"))
    assert f.loc[2].isna().all()                  # call 3: no live buys
    assert f.loc[1, "wrep_n_known"] == 1
    empty = pd.DataFrame(columns=make_synthetic.LIVE_BUY_COLUMNS)
    f, stats = W.wrep_frame(pd.DataFrame(rows), empty)
    assert f.isna().all().all() and list(f.columns) == list(C.WREP_FEATURES)
    assert stats["calls_with_live_buys"] == 0


# --- other behaviour -----------------------------------------------------------

def test_wallet_key():
    assert W.wallet_key("0xAbCd", "EF12") == "0xabcdef12"
    assert W.wallet_key("0xabcd", None) is None
    assert W.wallet_key("", "ef12") is None
    s = W.wallet_key(pd.Series(["0xAB12", None, " 0xcd34"]), pd.Series(["FfFf", "aaaa", "0000 "]))
    assert s.iloc[0] == "0xab12ffff" and pd.isna(s.iloc[1]) and s.iloc[2] == "0xcd340000"


def test_lookback_window(monkeypatch):
    t = T0 + 60 * DAY
    rows = [call(1, t - lag() - 20 * DAY, "0xA1", ret7=200.0),   # known 20 days before T
            call(2, t - lag() - 5 * DAY, "0xA2", ret7=-95.0),    # known 5 days before T
            call(3, t, "0xA3")]
    lb = buys(1, "0x1111aaaa") + buys(2, "0x1111aaaa") + buys(3, "0x1111aaaa")
    df, live = pd.DataFrame(rows), pd.DataFrame(lb)
    counts = lambda: W.wallet_counts(W.call_queries(df, live), W.wallet_events(
        W.eligible_history(df), live)).set_index("call_id").loc[3]
    assert counts()[["n", "k_run", "k_rug"]].tolist() == [2, 1, 1]
    monkeypatch.setattr(C, "WREP_LOOKBACK_DAYS", 10)
    assert counts()[["n", "k_run", "k_rug"]].tolist() == [1, 0, 1]


def test_duplicate_wallet_in_one_post_counts_once():
    t = T0 + 30 * DAY
    rows = [call(1, t - 20 * DAY, "0xAAA", ret7=200.0), call(2, t, "0xBBB")]
    f = frame(rows, buys(1, "0x1111aaaa", "0x1111AAAA") + buys(2, "0x1111aaaa", "0x1111aaaa"))
    assert f.loc[1, "wrep_n_known"] == 1 and f.loc[1, "wrep_known_share"] == 1


def test_make_live_buys_layout(small):
    d, lb = small
    assert list(lb.columns) == make_synthetic.LIVE_BUY_COLUMNS
    assert not lb["call_id"].isin(d.loc[d["post_kind"] == C.UPDATE_KIND, "call_id"]).any()
    again = make_synthetic.make_live_buys(d, n_wallets=300, skilled_share=0.05, seed=3)
    pd.testing.assert_frame_equal(lb, again)
    assert (lb.groupby("call_id")["position"].min() == 1).all()


# --- guard: not a model input yet ------------------------------------------------

def test_wrep_not_model_inputs():
    """The saved model must not use the wallet-reputation columns until PR C
    wires them in on purpose (in no variant either)."""
    assert len(C.WREP_FEATURES) == 8
    assert all(c.startswith(C.WREP_PREFIX) for c in C.WREP_FEATURES)
    assert not [c for c in C.FEATURES if c.startswith(C.WREP_PREFIX)]
    for name, knobs in C.VARIANTS.items():
        with C.overrides(**knobs):
            assert not [c for c in C.FEATURES if c.startswith(C.WREP_PREFIX)], name
