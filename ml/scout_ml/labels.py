"""Turn outcome columns into yes/no labels, net of tax, per holding bucket."""
import operator

import numpy as np
import pandas as pd

from .config import BUCKETS, LABELS, MAX_OUTCOME_PCT, NO_POOL_STATUSES, REPEAT_STATUS, UPDATE_KIND
from .features import num, text

_OPS = {">=": operator.ge, ">": operator.gt, "<=": operator.le, "<": operator.lt}


def net_pct(x, tax_buy_pct, tax_sell_pct):
    """Percent move `x` after paying buy tax on entry and sell tax on exit.

    net = (1 + x/100) * (1 - buy/100) * (1 - sell/100) - 1, in percent.
    A missing tax counts as 0.
    """
    buy = np.nan_to_num(np.asarray(tax_buy_pct, dtype=float)) / 100.0
    sell = np.nan_to_num(np.asarray(tax_sell_pct, dtype=float)) / 100.0
    return ((1.0 + x / 100.0) * (1.0 - buy) * (1.0 - sell) - 1.0) * 100.0


def to_bool(s: pd.Series) -> pd.Series:
    """CSV 'true'/'false'/'' or real booleans -> bool (NULL counts as False)."""
    return s.astype(str).str.strip().str.lower().isin(["true", "t", "1", "1.0"])


def outcome_columns() -> list:
    """Every outcome column some bucket's label or simulation reads."""
    cols = {cfg[k][0] for cfg in BUCKETS.values() for k in LABELS}
    cols |= {cfg["ret_col"] for cfg in BUCKETS.values()}
    return sorted(cols)


def extreme_outcome(df: pd.DataFrame) -> pd.Series:
    """True where any label-relevant raw outcome is above MAX_OUTCOME_PCT
    (+inf included; NaN = not known yet = not extreme)."""
    hit = pd.Series(False, index=df.index)
    for col in outcome_columns():
        hit |= num(df, col) > MAX_OUTCOME_PCT  # NaN compares False
    return hit


def repeat_call(df: pd.DataFrame, update: pd.Series) -> pd.Series:
    """True for a call that is not the first call of its token.

    First call = the website's and the tracker's rule: among the rows that are
    not update posts, the lowest (message_date, call_id) per contract address,
    compared without regard to letter case. `tracking_status = repeat` always
    counts as a repeat, even when the row kept results from before it was set
    aside; a later call left at `done` by an older tracker counts too."""
    rep = text(df, "tracking_status") == REPEAT_STATUS
    if "contract_address" not in df.columns or "message_date" not in df.columns:
        return rep
    ca = text(df, "contract_address").str.lower()
    when = pd.to_datetime(df["message_date"], utc=True, errors="coerce", format="ISO8601")
    order = pd.DataFrame({"ca": ca, "when": when, "id": num(df, "call_id"), "pos": range(len(df))},
                         index=df.index)
    calls = order[~update & ca.notna() & when.notna()]
    calls = calls.sort_values(["when", "id", "pos"], na_position="last")
    later = calls["ca"].duplicated(keep="first")
    return rep | later.reindex(df.index, fill_value=False)


def build_labels(df: pd.DataFrame) -> pd.DataFrame:
    """Per row: `update` (an update post, not a call: never usable), `repeat`
    (not the first call of its token: never usable), `no_pool`, `not_usd`,
    `extreme` (an outcome above MAX_OUTCOME_PCT: never usable), and for each
    bucket `usable_<b>`, `runner_<b>`, `collapse_<b>` (NaN where unusable) and
    `net_ret_<b>`.

    A bucket uses a row only once all of that bucket's outcome columns are
    present. The view has an outcome only after its horizon was computed, so a
    call still `tracking` (e.g. 1d-7d done, 30d not due yet) is used by the
    short buckets and left out of `long` as not labelled yet, never counted as
    a negative; that holds even when it is already flagged `rugged`."""
    out = pd.DataFrame(index=df.index)
    out["update"] = text(df, "post_kind") == UPDATE_KIND
    out["repeat"] = ~out["update"] & repeat_call(df, out["update"])
    out["no_pool"] = (num(df, "entry_late_price_usd").isna()
                      | text(df, "tracking_status").isin(NO_POOL_STATUSES))
    out["not_usd"] = text(df, "price_unit") != "usd"
    out["extreme"] = extreme_outcome(df)
    rugged = to_bool(df["rugged"]) if "rugged" in df.columns else pd.Series(False, index=df.index)
    buy, sell = num(df, "tax_buy_pct"), num(df, "tax_sell_pct")
    for b, cfg in BUCKETS.items():
        needed = {cfg["runner"][0], cfg["collapse"][0], cfg["ret_col"]}
        usable = (~out["update"] & ~out["repeat"] & ~out["no_pool"] & ~out["not_usd"]
                  & ~out["extreme"])
        for col in needed:
            usable &= num(df, col).notna()
        out[f"usable_{b}"] = usable
        for label in LABELS:
            col, op, threshold = cfg[label]
            hit = _OPS[op](net_pct(num(df, col), buy, sell), threshold)
            if label == "collapse" and cfg["collapse_or_rugged"]:
                hit = hit | rugged
            out[f"{label}_{b}"] = hit.astype(float).where(usable)
        out[f"net_ret_{b}"] = net_pct(num(df, cfg["ret_col"]), buy, sell).where(usable)
    return out
