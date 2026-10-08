"""The columns ml/ expects must match what -export-dataset writes: the select
list of scout_call_dataset_v in ../scoutanalytics.sql (SELECT * in Go)."""
import re
from pathlib import Path

import pytest

from scout_ml import config as C

SQL = Path(__file__).resolve().parents[2] / "scoutanalytics.sql"


def view_columns(sql: str) -> list:
    """Output column names of CREATE VIEW scout_call_dataset_v, in order."""
    body = sql.split("CREATE VIEW scout_call_dataset_v AS", 1)[1]
    body = body.split("\nFROM scout_calls_v cv", 1)[0]
    body = re.sub(r"--[^\n]*", "", body).strip()
    assert body.startswith("SELECT")
    body = body[len("SELECT"):]
    items, depth, cur = [], 0, ""
    for ch in body:  # split on top-level commas
        depth += (ch == "(") - (ch == ")")
        if ch == "," and depth == 0:
            items.append(cur)
            cur = ""
        else:
            cur += ch
    items.append(cur)
    names = []
    for it in (i.strip() for i in items):
        m = re.search(r"\bAS\s+(\w+)$", it, flags=re.I)
        names.append(m.group(1) if m else it.split(".")[-1].split("::")[0])
    return names


@pytest.mark.skipif(not SQL.exists(), reason="scoutanalytics.sql not next to ml/")
def test_view_columns_match_the_sql_view():
    cols = view_columns(SQL.read_text(encoding="utf-8"))
    assert len(cols) == len(set(cols))
    assert cols == C.VIEW_COLUMNS, (
        f"only in SQL: {[c for c in cols if c not in C.VIEW_COLUMNS]}; "
        f"only in config: {[c for c in C.VIEW_COLUMNS if c not in cols]}")


def test_every_view_column_is_a_feature_or_forbidden():
    """No view column may slip in unclassified: either a raw model input, or
    refused by the leakage guard (outcomes, tracker discovery, ids, display)."""
    inputs = set(C.NUMERIC_RAW) | set(C.CATEGORICAL) | {"message_date"}  # date -> hour/weekday
    unused = set(C.UNUSED_VIEW_COLUMNS)  # known at the call, but constant for first calls
    for c in C.VIEW_COLUMNS:
        if c == "message_date":
            continue
        assert (c in inputs) + C.is_forbidden(c) + (c in unused) == 1, c
    for c in ("post_kind", "token_name", "latest_price_usd", "latest_return_pct",
              "latest_checked_at", "rugged", "tracking_status", "entry_price_source",
              "pool_dex", "price_unit", "current_liquidity_usd"):
        assert C.is_forbidden(c), c


def test_unused_columns_stay_in_the_view_but_are_no_features():
    """prior_calls / secs_since_prev_call are 0 / NULL for every first call:
    still exported by the view (VIEW_COLUMNS), never model inputs."""
    for c in ("prior_calls", "secs_since_prev_call"):
        assert c in C.VIEW_COLUMNS and c in C.VIEW_NUMERIC, c
        assert c not in C.FEATURES and c not in C.NUMERIC_RAW and c not in C.LOG1P_FEATURES, c
        assert not C.is_forbidden(c), c                  # not leaky, just useless
    assert C.NUMERIC_RAW == [c for c in C.VIEW_NUMERIC if c not in C.UNUSED_VIEW_COLUMNS]
