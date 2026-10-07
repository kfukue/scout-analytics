"""Write a synthetic scout_call_dataset_v export so the pipeline can be run
end to end without a database.  The numbers are invented; only the column
layout, missingness pattern and CSV conventions mimic the real export.

    python make_synthetic.py --out calls.csv [--calls 6000 --days 70 --seed 7 --end 2026-10-01T00:00:00Z]
"""
import argparse

import numpy as np
import pandas as pd

from scout_ml.config import HORIZONS, LATEST_VIEW, VIEW_COLUMNS

HORIZON_DAYS = {"1h": 1 / 24, "1d": 1, "3d": 3, "7d": 7, "30d": 30}
STEP_SIGMA = {"1h": 0.30, "1d": 0.75, "3d": 0.55, "7d": 0.55, "30d": 0.80}  # log-price steps
STEP_DRIFT = {"1h": -0.03, "1d": -0.30, "3d": -0.20, "7d": -0.20, "30d": -0.45}
INT_COLUMNS = (["call_id", "message_id", "age_seconds", "holders", "proof_elite", "proof_good",
                "live_buys_elite_count", "live_buys_good_count", "prior_calls",
                "secs_since_prev_call", "calls_prev_1h", "calls_prev_24h", "pre_first_trade_age_s"]
               + [f"pre_{k}_{w}" for k in ("swaps", "buys", "sells") for w in ("5m", "15m", "60m")])


def make(n: int = 6000, days: float = 70, seed: int = 7, end: str = "2026-10-01T00:00:00Z"):
    rng = np.random.default_rng(seed)
    end = pd.Timestamp(end)
    when = (end - pd.to_timedelta(np.sort(rng.uniform(0, days, n))[::-1], unit="D")).floor("s")
    d = pd.DataFrame({"call_id": np.arange(1, n + 1), "message_id": np.arange(1, n + 1) * 3 + 1000,
                      "message_date": when})

    # Tokens: ~30% of calls repeat one of the last 200 called tokens.
    tok = np.zeros(n, dtype=int)
    for i in range(1, n):
        tok[i] = tok[rng.integers(max(0, i - 200), i)] if rng.random() < 0.30 else tok[:i].max() + 1
    # Addresses in mixed case: the same token can be posted in different case.
    d["contract_address"] = [f"0xAb{t:06d}C0" if rng.random() < 0.5 else f"0xab{t:06d}c0" for t in tok]
    later = pd.Series(tok).duplicated().to_numpy()      # not the token's first post
    # Update posts ("$TOKEN hit 3X ..."): about an earlier call, never a first post.
    update = later & (rng.random(n) < 0.15)
    d["call_status"] = np.where(update, "update", np.where(later, "duplicate", "scanned"))
    d["post_kind"] = pd.Series(np.where(update, "update", "call"), dtype=object)
    d.loc[rng.random(n) < 0.01, "post_kind"] = None      # not classified yet: read as a call
    d["token_symbol"] = [f"SYM{t % 997}" for t in tok]
    d["token_name"] = pd.Series([f"Token {t}" for t in tok], dtype=object).mask(
        pd.Series(rng.random(n) < 0.2))
    d["dex"] = rng.choice(["raydium", "pumpswap", "meteora", "uniswap_v2", "rare_dex"], n,
                          p=[0.35, 0.35, 0.15, 0.149, 0.001])
    d["launchpad"] = rng.choice(["pumpfun", "bonk", None], n, p=[0.40, 0.10, 0.50])

    # Repeat/busyness features, computed from the generated timeline itself.
    secs = (d["message_date"] - d["message_date"].min()).dt.total_seconds().to_numpy()
    d["prior_calls"] = d.groupby("contract_address").cumcount()
    d["secs_since_prev_call"] = d.groupby("contract_address")["message_date"].diff().dt.total_seconds()
    for name, span in (("calls_prev_1h", 3600), ("calls_prev_24h", 86400)):
        d[name] = np.arange(n) - np.searchsorted(secs, secs - span, "left")

    mcap = rng.lognormal(np.log(150_000), 1.2, n)
    liq_share = np.clip(rng.lognormal(np.log(0.15), 0.6, n), 0.01, 0.9)
    d["called_at_mcap_usd"] = mcap * rng.lognormal(0, 0.15, n)
    d["mcap_usd"], d["liq_usd"], d["liq_pct"] = mcap, mcap * liq_share, liq_share * 100
    d["tax_buy_pct"] = rng.choice([0, 0, 0, 1, 2.5, 5, 10], n)
    d["tax_sell_pct"] = rng.choice([0, 0, 0, 1, 2.5, 5, 10], n)
    d["age_seconds"] = rng.lognormal(np.log(3600), 2.0, n).astype(int)
    d["holders"] = rng.lognormal(np.log(400), 1.0, n).astype(int)
    d["proof_elite"], d["proof_good"] = rng.poisson(0.5, n), rng.poisson(1.5, n)
    d["live_buys_elite_count"], d["live_buys_good_count"] = rng.poisson(0.8, n), rng.poisson(2.0, n)
    d["live_buys_elite_usd"] = d["live_buys_elite_count"] * rng.lognormal(np.log(800), 1.0, n)
    d["live_buys_good_usd"] = d["live_buys_good_count"] * rng.lognormal(np.log(300), 1.0, n)
    d["live_buy_max_usd"] = np.maximum(d["live_buys_elite_usd"], d["live_buys_good_usd"]) * rng.uniform(0.4, 1, n)

    buy_share = rng.beta(5, 5, n)
    swaps = {"60m": rng.poisson(rng.lognormal(np.log(60), 1.0, n))}
    swaps["15m"] = rng.binomial(swaps["60m"], 0.4)
    swaps["5m"] = rng.binomial(swaps["15m"], 0.4)
    size = rng.lognormal(np.log(150), 0.7, n)
    for w, scale in (("5m", 0.3), ("15m", 0.5), ("60m", 1.0)):
        d[f"pre_swaps_{w}"] = swaps[w]
        d[f"pre_buys_{w}"] = rng.binomial(swaps[w], buy_share)
        d[f"pre_sells_{w}"] = swaps[w] - d[f"pre_buys_{w}"]
        d[f"pre_buy_vol_{w}"] = d[f"pre_buys_{w}"] * size
        d[f"pre_sell_vol_{w}"] = d[f"pre_sells_{w}"] * size * rng.uniform(0.7, 1.3, n)
        d[f"pre_price_chg_{w}_pct"] = (buy_share - 0.5) * 120 * scale + rng.standard_t(3, n) * 8 * scale
    d["pre_first_trade_age_s"] = np.minimum(d["age_seconds"], rng.integers(60, 3600, n))
    d["pre_vol_unit"] = rng.choice(["usd", "SOL"], n, p=[0.85, 0.15])
    recent = (end - d["message_date"]) < pd.Timedelta(days=10)
    d["perceptor_verdict"] = np.where(
        recent, rng.choice(["clean", "caution", "red_flags", "unknown"], n), None)

    # Planted, modest signal: deeper liquidity, elite live buys, buy pressure
    # and low tax help; everything else is noise.
    z = lambda a: (a - a.mean()) / a.std()
    signal = (0.45 * z(np.log(liq_share)) + 0.45 * z(np.log1p(d["live_buys_elite_usd"]))
              + 0.45 * z(buy_share) - 0.30 * z(d["tax_buy_pct"] + d["tax_sell_pct"])).to_numpy()

    # Price path: heavy-tailed log steps between horizons; rugs go to ~-99%.
    late = np.exp(rng.normal(0.03, 0.06, n))        # price 60s after the post / price at post
    rug_at = np.where(rng.random(n) < 1 / (1 + np.exp(2.0 + signal)), rng.integers(1, 5, n), 99)
    age_days = (end - d["message_date"]).dt.total_seconds().to_numpy() / 86400
    log_p, peak, trough = np.zeros(n), np.zeros(n), np.zeros(n)
    for i, h in enumerate(HORIZONS):
        prev = log_p.copy()
        log_p = log_p + STEP_DRIFT[h] + 0.22 * signal + STEP_SIGMA[h] * rng.standard_t(3, n) * 0.6
        log_p = np.where(rug_at <= i, np.log(rng.uniform(0.001, 0.03, n)), log_p)
        wiggle = np.abs(rng.normal(0, STEP_SIGMA[h] * 0.6, n))
        peak = np.maximum(peak, np.maximum(prev, log_p) + wiggle)
        trough = np.minimum(trough, np.minimum(prev, log_p) - wiggle)
        known = (age_days >= HORIZON_DAYS[h]) & (rng.random(n) > 0.03)   # 3%: no trades
        for name, path in (("ret", log_p), ("max_gain", peak), ("max_dd", trough)):
            d[f"{name}_{h}"] = np.where(known, np.exp(path) * 100 - 100, np.nan)
            late_pct = np.exp(path) / late * 100 - 100
            late_pct = np.maximum(late_pct, 0) if name == "max_gain" else late_pct
            late_pct = np.minimum(late_pct, 0) if name == "max_dd" else late_pct
            d[f"{name}_late_{h}"] = np.where(known, late_pct, np.nan)

    done = age_days >= 30
    d["tracking_status"] = np.where(done, "done", "tracking")
    d["pool_address"] = [f"Pool{i:07d}" for i in range(n)]
    kind = rng.choice(["v2", "v3", "v4", "pons"], n, p=[0.45, 0.15, 0.25, 0.15])
    d["pool_dex"] = np.where(kind == "pons", "pons-curve", np.char.add("uniswap-", kind))
    d["entry_price_usd"] = rng.lognormal(np.log(1e-4), 2.0, n)
    d["entry_price_source"] = np.char.add("onchain-", kind)
    d["price_unit"] = rng.choice(["usd", "VIRTUAL"], n, p=[0.94, 0.06])
    d["quote_asset"] = rng.choice(["WETH", "USDG", "VIRTUAL"], n, p=[0.75, 0.15, 0.10])
    d["entry_late_price_usd"] = d["entry_price_usd"] * late
    d["current_liquidity_usd"] = d["liq_usd"] * np.where(rug_at < 99, 0.001, np.exp(log_p / 2))
    # The tracker flags a rug at once, before the last horizon is due.
    rug_seen = (rug_at < 99) & (age_days >= rug_at)
    d["rugged"] = pd.Series(np.where(done | rug_seen, rug_at < 99, None), dtype=object)
    latest_ret = np.where(rug_at < 99, -100.0, np.exp(log_p) / late * 100 - 100)
    d["latest_price_usd"] = d["entry_price_usd"] * late * (1 + latest_ret / 100)
    d["latest_return_pct"] = latest_ret
    d["latest_checked_at"] = end - pd.Timedelta(minutes=7)

    # Missingness. No pool => nothing about the pool is known, incl. pre-call trading.
    def blank(mask, cols):
        d[cols] = d[cols].astype(object).mask(pd.Series(mask, index=d.index))
    pre_cols = [c for c in VIEW_COLUMNS if c.startswith("pre_")]
    live_cols = [c for c in VIEW_COLUMNS if c.startswith("live_buy")]
    outcome_cols = [c for c in VIEW_COLUMNS if c.startswith(("ret_", "max_gain_", "max_dd_"))]
    no_pool = rng.random(n) < 0.08
    blank(no_pool, pre_cols + outcome_cols + ["pool_address", "pool_dex", "entry_price_usd",
          "entry_price_source", "price_unit", "quote_asset", "entry_late_price_usd",
          "current_liquidity_usd", "rugged"])
    d.loc[no_pool, "tracking_status"] = rng.choice(["no_pool", "gave_up"], int(no_pool.sum()))
    blank(rng.random(n) < 0.25, pre_cols)
    blank(rng.random(n) < 0.40, live_cols)
    # Later posts of a token are not tracked (status repeat, no pool or outcomes)
    # and update posts have no tracking row; a few older ones kept the results
    # of an earlier tracker version (they must still be left out of training).
    tracked = ["tracking_status", "pool_address", "pool_dex", "entry_price_usd",
               "entry_price_source", "price_unit", "quote_asset", "entry_late_price_usd",
               "current_liquidity_usd", "rugged"] + LATEST_VIEW
    stale = later & ~no_pool & (rng.random(n) < 0.15)
    blank(later & ~stale, pre_cols + outcome_cols + tracked)
    d.loc[later & ~update & ~stale, "tracking_status"] = "repeat"
    # set aside while still tracking: status repeat, partial results kept
    d.loc[later & ~update & stale & (d["tracking_status"] == "tracking").to_numpy(), "tracking_status"] = "repeat"
    blank(rng.random(n) < 0.20, ["tax_buy_pct", "tax_sell_pct"])
    blank(rng.random(n) < 0.30, ["holders"])
    blank(rng.random(n) < 0.10, ["liq_usd", "liq_pct"])
    return d[VIEW_COLUMNS]


def write_csv(d: pd.DataFrame, path) -> None:
    """Same conventions as the Go exporter: '' for NULL, true/false, RFC3339."""
    d = d.copy()
    for c in ("message_date", "latest_checked_at"):
        d[c] = pd.to_datetime(d[c], utc=True).dt.strftime("%Y-%m-%dT%H:%M:%SZ").where(d[c].notna(), None)
    for c in INT_COLUMNS:
        d[c] = pd.to_numeric(d[c]).astype("Int64")
    d["rugged"] = d["rugged"].map({True: "true", False: "false"})
    d.to_csv(path, index=False)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--out", default="calls.csv")
    ap.add_argument("--calls", type=int, default=6000)
    ap.add_argument("--days", type=float, default=70)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--end", default="2026-10-01T00:00:00Z", help="time of the newest call (UTC)")
    a = ap.parse_args()
    write_csv(make(a.calls, a.days, a.seed, a.end), a.out)
    print(f"wrote {a.out}: {a.calls} synthetic calls over {a.days:g} days")
