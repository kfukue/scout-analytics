"""The one place where raw view rows become model inputs.

`build_features` is called by train.py on the whole dataset and by serve.py on
a single JSON row, so training and serving cannot drift apart."""
import numpy as np
import pandas as pd

from .config import (CATEGORICAL, FEATURES, MIN_CATEGORY_COUNT, NUMERIC_RAW,
                     PRE_VOL, is_forbidden)


def num(df: pd.DataFrame, col: str) -> pd.Series:
    """Column as float; absent column or unparsable value -> NaN."""
    if col not in df.columns:
        return pd.Series(np.nan, index=df.index, dtype=float)
    return pd.to_numeric(df[col], errors="coerce").astype(float)


def text(df: pd.DataFrame, col: str) -> pd.Series:
    """Column as stripped string; absent/empty/NULL -> None."""
    if col not in df.columns:
        return pd.Series(None, index=df.index, dtype=object)
    s = df[col]
    out = s.astype(str).str.strip()
    return out.where(s.notna() & (out != ""), None)


def assert_no_leakage(columns) -> None:
    """Raise if any outcome / bookkeeping column is about to become a feature."""
    bad = sorted(c for c in columns if is_forbidden(c))
    if bad:
        raise ValueError(f"forbidden columns in feature matrix: {bad}")


def learn_cat_levels(df: pd.DataFrame) -> dict:
    """Category levels seen often enough in training data (stored in meta.json)."""
    out = {}
    for c in CATEGORICAL:
        counts = text(df, c).value_counts()
        out[c] = sorted(counts[counts >= MIN_CATEGORY_COUNT].index.tolist())
    return out


def _ratio(a: pd.Series, b: pd.Series) -> pd.Series:
    """a / b with a zero or missing denominator giving NaN (never inf)."""
    return (a / b.where(b > 0)).replace([np.inf, -np.inf], np.nan)


def build_features(rows, cat_levels: dict, columns=None) -> pd.DataFrame:
    """Raw view rows (DataFrame, dict or list of dicts) -> float feature matrix.

    Only whitelisted input columns are read, so unknown keys are ignored and
    missing ones become NaN. Categoricals become integer codes into
    `cat_levels` (unknown level -> NaN), which LightGBM treats as missing.
    `columns` (serving: the list stored in meta.json) fixes the column order.
    """
    if isinstance(rows, dict):
        rows = [rows]
    df = rows if isinstance(rows, pd.DataFrame) else pd.DataFrame(list(rows))
    X = pd.DataFrame({c: num(df, c) for c in NUMERIC_RAW}, index=df.index)

    # Pool volumes are only comparable across calls when quoted in USD.
    usd = (text(df, "pre_vol_unit") == "usd").to_numpy()
    X.loc[~usd, PRE_VOL] = np.nan

    live_usd = X[["live_buys_elite_usd", "live_buys_good_usd"]].sum(axis=1, min_count=1)
    X["liq_to_mcap"] = _ratio(X["liq_usd"], X["mcap_usd"])
    X["live_usd_to_liq"] = _ratio(live_usd, X["liq_usd"])
    X["live_usd_to_mcap"] = _ratio(live_usd, X["mcap_usd"])
    X["mcap_vs_called"] = _ratio(X["mcap_usd"], X["called_at_mcap_usd"])
    for w in ("5m", "15m", "60m"):
        # +1 smoothing keeps the count ratio finite when there were no sells.
        X[f"pre_buy_sell_ratio_{w}"] = (X[f"pre_buys_{w}"] + 1) / (X[f"pre_sells_{w}"] + 1)
        buy, sell = X[f"pre_buy_vol_{w}"], X[f"pre_sell_vol_{w}"]
        X[f"pre_buy_vol_share_{w}"] = _ratio(buy, buy + sell)

    if "message_date" in df.columns:
        when = pd.to_datetime(df["message_date"], utc=True, errors="coerce", format="ISO8601")
    else:
        when = pd.Series(pd.NaT, index=df.index, dtype="datetime64[ns, UTC]")
    X["hour_utc"] = when.dt.hour.astype(float)
    X["weekday_utc"] = when.dt.weekday.astype(float)

    for c in CATEGORICAL:
        codes = {level: i for i, level in enumerate(cat_levels.get(c, []))}
        X[c] = text(df, c).map(codes).astype(float)

    columns = list(columns) if columns is not None else FEATURES
    assert_no_leakage(columns)
    return X[columns]
