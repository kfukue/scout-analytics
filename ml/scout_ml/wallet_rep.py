"""Live-buy wallet reputation: how the earlier calls a call's live-buy wallets
bought into turned out, as known at the moment of the call.

Inputs: the dataset export (scout_call_dataset_v, one row per call) and the
live buys (-export-live-buys: call_id, position, tier, amount_usd,
wallet_display, wallet_prefix, wallet_suffix). A wallet is only shown
shortened in the post, so it is identified by lower(prefix) + lower(suffix).

History: the first calls P of each token (not update posts, not no_pool, USD
prices) whose 7 d outcome is in the export. P's outcome becomes known at
P.message_date + 7 d + C.WREP_KNOWN_LAG_HOURS; for a call C posted at T only
outcomes known strictly before T count, and never history on C's own token.
The labels are the medium bucket's runner rule (net of tax) and a rug = raw
ret_late_7d <= C.WREP_RUG_PCT. `rugged`, `rug_at` and every 30 d column are
never read: the tracker sets them later, so they would leak.

Report-only for now: train.py calls `wrep_frame` only for the
C.WALLET_REP_VARIANTS (--variants --live-buys PATH), once per distinct
C.WREP_LOOKBACK_DAYS they use; C.WREP_FEATURES
are never inputs of the saved model (train.py raises if they are in
C.FEATURES), and serve.py does not call this module."""
import numpy as np
import pandas as pd

from . import config as C
from .config import NO_POOL_STATUSES, UPDATE_KIND   # structural; C.WREP_* are read at call time
from .features import num, text
from .labels import _OPS, net_pct, repeat_call

COUNTS = ["n", "k_run", "k_rug"]


def wallet_key(prefix, suffix):
    """lower(prefix) + lower(suffix), for two Series (a Series, NaN where a
    part is missing or empty) or two scalars (a str, or None)."""
    if not isinstance(prefix, pd.Series):
        key = wallet_key(pd.Series([prefix], dtype=object), pd.Series([suffix], dtype=object)).iloc[0]
        return None if pd.isna(key) else key
    p = text(pd.DataFrame({"p": prefix}), "p").str.lower()
    s = text(pd.DataFrame({"s": pd.Series(suffix, index=prefix.index)}), "s").str.lower()
    return (p + s).where(p.notna() & s.notna())


def _when(df: pd.DataFrame) -> pd.Series:
    """message_date as UTC timestamps (NaT where missing or unparsable)."""
    return pd.to_datetime(df["message_date"], utc=True, errors="coerce", format="ISO8601")


def _token(df: pd.DataFrame) -> pd.Series:
    """lower(contract_address), None where missing."""
    return text(df, "contract_address").str.lower()


def _call_id(s: pd.Series) -> pd.Series:
    """call ids as nullable integers (one dtype on both sides of every merge)."""
    return pd.to_numeric(s, errors="coerce").round().astype("Int64")


def _uncapped_eligible(df: pd.DataFrame):
    """(ok, raw): ok = every history rule except the C.MAX_OUTCOME_PCT cap
    (first call of its token, not an update post, not no_pool / gave_up,
    price_unit usd, outcome, date, token and call id present); raw = the
    medium bucket's outcome column (ret_late_7d)."""
    update = text(df, "post_kind") == UPDATE_KIND
    first = ~update & ~repeat_call(df, update)
    no_pool = (num(df, "entry_late_price_usd").isna()
               | text(df, "tracking_status").isin(NO_POOL_STATUSES))
    usd = text(df, "price_unit") == "usd"
    raw = num(df, C.BUCKETS["medium"]["runner"][0])
    ok = (first & ~no_pool & usd & raw.notna() & _when(df).notna()
          & _token(df).notna() & _call_id(df["call_id"]).notna())
    return ok, raw


def extreme_dropped(df: pd.DataFrame) -> int:
    """How many calls pass every history rule but are dropped because their
    ret_late_7d is above C.MAX_OUTCOME_PCT."""
    ok, raw = _uncapped_eligible(df)
    return int((ok & (raw > C.MAX_OUTCOME_PCT)).sum())


def eligible_history(df: pd.DataFrame) -> pd.DataFrame:
    """The history calls P: first call of their token, not an update post, not
    no_pool / gave_up, price_unit usd, the medium bucket's outcome column
    (ret_late_7d) present and <= C.MAX_OUTCOME_PCT. Returns call_id, token,
    known_at (message_date + the medium horizon + C.WREP_KNOWN_LAG_HOURS),
    y_run (the medium runner rule, net of tax) and y_rug (raw outcome <=
    C.WREP_RUG_PCT). Only the medium bucket's own column is capped: an extreme
    value in another column (e.g. ret_late_30d) does not drop a row."""
    ok, raw = _uncapped_eligible(df)
    keep = ok & (raw <= C.MAX_OUTCOME_PCT)
    medium = C.BUCKETS["medium"]
    _, op, threshold = medium["runner"]
    when, token, call_id = _when(df), _token(df), _call_id(df["call_id"])
    lag = pd.Timedelta(days=medium["horizon_days"]) + pd.Timedelta(hours=C.WREP_KNOWN_LAG_HOURS)
    net = net_pct(raw, num(df, "tax_buy_pct"), num(df, "tax_sell_pct"))
    out = pd.DataFrame({"call_id": call_id, "token": token, "known_at": when + lag,
                        "y_run": _OPS[op](net, threshold), "y_rug": raw <= C.WREP_RUG_PCT})
    return out[keep].reset_index(drop=True)


def live_buy_wallets(live_buys: pd.DataFrame) -> pd.DataFrame:
    """The distinct (call_id, wallet) pairs of the live buys (rows without a
    call id or with an empty prefix / suffix are dropped)."""
    out = pd.DataFrame({"call_id": _call_id(live_buys["call_id"]),
                        "wallet": wallet_key(live_buys["wallet_prefix"], live_buys["wallet_suffix"])})
    return out.dropna().drop_duplicates().reset_index(drop=True)


def wallet_events(history: pd.DataFrame, live_buys: pd.DataFrame) -> pd.DataFrame:
    """Each history call P times each of its distinct live-buy wallets:
    call_id, wallet, token, known_at, y_run, y_rug."""
    ev = history.merge(live_buy_wallets(live_buys), on="call_id", how="inner")
    return ev[["call_id", "wallet", "token", "known_at", "y_run", "y_rug"]]


def call_queries(df: pd.DataFrame, live_buys: pd.DataFrame) -> pd.DataFrame:
    """Each call C of `df` times each of its distinct live-buy wallets: call_id,
    wallet, token (lower contract address, "" when missing) and T (the post time)."""
    calls = pd.DataFrame({"call_id": _call_id(df["call_id"]), "token": _token(df).fillna(""),
                          "T": _when(df)}).dropna(subset=["call_id", "T"])
    q = live_buy_wallets(live_buys).merge(calls, on="call_id", how="inner")
    return q[["call_id", "wallet", "token", "T"]].reset_index(drop=True)


def _cumulative(events: pd.DataFrame, by: list) -> pd.DataFrame:
    """Per `by` group, the running totals of n / k_run / k_rug up to and
    including each distinct known_at, sorted by known_at (for merge_asof)."""
    ev = events.assign(n=1, k_run=events["y_run"].astype(int), k_rug=events["y_rug"].astype(int))
    per = ev.groupby(by + ["known_at"], as_index=False)[COUNTS].sum()   # sorted by by + known_at
    per[COUNTS] = per.groupby(by)[COUNTS].cumsum()
    return per.sort_values("known_at", kind="stable")


def _counts_before(queries: pd.DataFrame, cum: pd.DataFrame, by: list, at: pd.Series) -> pd.DataFrame:
    """n / k_run / k_rug of the events of each query's `by` group whose
    known_at is strictly before `at` (aligned with queries.index; 0 if none)."""
    if queries.empty or cum.empty:
        return pd.DataFrame(0.0, index=queries.index, columns=COUNTS)
    left = queries[by].assign(_at=at, _row=np.arange(len(queries))).sort_values("_at", kind="stable")
    hit = pd.merge_asof(left, cum, left_on="_at", right_on="known_at", by=by,
                        direction="backward", allow_exact_matches=False)
    hit = hit.set_index("_row").sort_index()
    return pd.DataFrame(hit[COUNTS].fillna(0.0).to_numpy(), index=queries.index, columns=COUNTS)


def _counts_in_window(queries: pd.DataFrame, events: pd.DataFrame, by: list) -> pd.DataFrame:
    """_counts_before T, minus those before T - C.WREP_LOOKBACK_DAYS when set."""
    cum = _cumulative(events, by)
    out = _counts_before(queries, cum, by, queries["T"])
    if C.WREP_LOOKBACK_DAYS is not None:
        start = queries["T"] - pd.Timedelta(days=C.WREP_LOOKBACK_DAYS)
        out = out - _counts_before(queries, cum, by, start)
    return out


def wallet_counts(queries: pd.DataFrame, events: pd.DataFrame) -> pd.DataFrame:
    """Per query (call_id, wallet): n history calls of the wallet whose outcome
    was known strictly before T, k_run runners and k_rug rugs among them;
    history on the query's own token is left out (contract addresses compared
    lower-case). Vectorised with pd.merge_asof by wallet (backward, no exact
    matches), minus the same join by (wallet, token)."""
    total = _counts_in_window(queries, events, ["wallet"])
    own = _counts_in_window(queries, events, ["wallet", "token"])
    out = queries[["call_id", "wallet"]].copy()
    out[COUNTS] = (total - own).astype(int)
    return out


def posterior(k, n, m, a):
    """Smoothed rate: (k + a*m) / (n + a), a pseudo-calls at the prior rate m."""
    return (k + a * m) / (n + a)


def aggregate_wallets(counts: pd.DataFrame) -> pd.DataFrame:
    """Per call_id, the C.WREP_FEATURES of its wallets W (all rows of the call)
    and K (those with n >= 1). n_known / known_share are NaN only for a call
    without rows (no live buys); lifts and good / bad shares (shares of K) are
    NaN when K is empty."""
    n = counts["n"].astype(float)
    known = n >= 1
    a = C.WREP_PRIOR_STRENGTH
    run = (posterior(counts["k_run"], n, C.WREP_PRIOR_RUNNER, a) / C.WREP_PRIOR_RUNNER).where(known)
    rug = (posterior(counts["k_rug"], n, C.WREP_PRIOR_RUG, a) / C.WREP_PRIOR_RUG).where(known)
    enough = known & (n >= C.WREP_MIN_CALLS)
    w = pd.DataFrame({"call_id": counts["call_id"], "known": known.astype(float), "run": run,
                      "rug": rug, "good": (enough & (run >= C.WREP_GOOD_LIFT)).astype(float),
                      "bad": (enough & (rug >= C.WREP_BAD_LIFT)).astype(float)})
    g = w.groupby("call_id")
    n_known = g["known"].sum()
    k = n_known.where(n_known > 0)          # 0 known wallets -> the shares of K are NaN
    out = pd.DataFrame({"wrep_n_known": n_known, "wrep_known_share": n_known / g.size(),
                        "wrep_runner_lift_mean": g["run"].mean(), "wrep_runner_lift_max": g["run"].max(),
                        "wrep_rug_lift_mean": g["rug"].mean(), "wrep_rug_lift_max": g["rug"].max(),
                        "wrep_good_share": g["good"].sum() / k, "wrep_bad_share": g["bad"].sum() / k})
    return out[list(C.WREP_FEATURES)].astype(float)


def wrep_frame(df: pd.DataFrame, live_buys: pd.DataFrame):
    """(a frame indexed like `df` with the C.WREP_FEATURES columns, NaN for a
    call without live buys; a stats dict: rows, live_buy_rows, wallets,
    calls_with_live_buys, eligible_history, extreme_dropped (first calls left
    out of the history only for a ret_late_7d above C.MAX_OUTCOME_PCT) and the
    observed y_run / y_rug rates over the eligible history calls)."""
    history = eligible_history(df)
    queries = call_queries(df, live_buys)
    agg = aggregate_wallets(wallet_counts(queries, wallet_events(history, live_buys)))
    ids = _call_id(df["call_id"])
    frame = agg.reindex(pd.Index(ids, dtype="Int64")).set_axis(df.index)
    stats = {"rows": len(df), "live_buy_rows": len(live_buys),
             "wallets": int(queries["wallet"].nunique()),
             "calls_with_live_buys": int(queries["call_id"].nunique()),
             "eligible_history": len(history),
             "extreme_dropped": extreme_dropped(df),
             "y_run_rate": float(history["y_run"].mean()),     # NaN when there is no history
             "y_rug_rate": float(history["y_rug"].mean())}
    return frame, stats
