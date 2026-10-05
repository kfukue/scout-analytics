"""Turn outcome columns into yes/no labels, net of tax, per holding bucket."""
import operator

import numpy as np
import pandas as pd

from .config import BUCKETS, LABELS, NO_POOL_STATUSES, REPEAT_STATUS
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


def build_labels(df: pd.DataFrame) -> pd.DataFrame:
    """Per row: `no_pool`, `repeat` (the part of `no_pool` that is an untracked
    repeat call, reported separately), `not_usd`, and for each bucket `usable_<b>`,
    `runner_<b>`, `collapse_<b>` (NaN where unusable) and `net_ret_<b>`."""
    out = pd.DataFrame(index=df.index)
    out["no_pool"] = (num(df, "entry_late_price_usd").isna()
                      | text(df, "tracking_status").isin(NO_POOL_STATUSES))
    out["repeat"] = out["no_pool"] & (text(df, "tracking_status") == REPEAT_STATUS)
    out["not_usd"] = text(df, "price_unit") != "usd"
    rugged = to_bool(df["rugged"]) if "rugged" in df.columns else pd.Series(False, index=df.index)
    buy, sell = num(df, "tax_buy_pct"), num(df, "tax_sell_pct")
    for b, cfg in BUCKETS.items():
        needed = {cfg["runner"][0], cfg["collapse"][0], cfg["ret_col"]}
        usable = ~out["no_pool"] & ~out["not_usd"]
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
