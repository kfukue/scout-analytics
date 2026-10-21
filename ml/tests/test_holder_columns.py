"""The holder export's columns (the json tags of holderRow in
../holders_columns.go) against the leakage guard: post24h_* and the post-call
provenance are refused, every pre_hold_* is a feature or left out with a reason."""
import re
from pathlib import Path

import pytest

from scout_ml import config as C
from scout_ml.features import assert_no_leakage

GO = Path(__file__).resolve().parents[2] / "holders_columns.go"
needs_go = pytest.mark.skipif(not GO.exists(), reason="holders_columns.go not next to ml/")


def holder_columns(go: str) -> list:
    """The json tag names in holders_columns.go, in file order (the export's
    column order: holderRow embeds the three groups in this order)."""
    return [m.group(1) for m in re.finditer(r'`json:"(\w+)[,"]', go)]


def go_columns() -> list:
    return holder_columns(GO.read_text(encoding="utf-8"))


def model_inputs() -> set:
    """Every name any model or variant can take as an input."""
    return (set(C.NUMERIC_RAW) | set(C.DERIVED) | set(C.OPTIONAL_DERIVED) | set(C.WREP_FEATURES)
            | set(C.CATEGORICAL_VIEW) | set(C.DERIVED_CATEGORICAL) | set(C.FEATURES))


def test_holder_columns_parser_reads_tags():
    go = 'A *int `json:"pre_hold_a"`\n\tB []string `json:"post24h_b,omitempty"` // x\n\tC int'
    assert holder_columns(go) == ["pre_hold_a", "post24h_b"]


@needs_go
def test_go_file_has_the_three_groups():
    cols = go_columns()
    assert len(cols) == len(set(cols))
    assert cols[0] == "call_id"
    assert any(c.startswith("pre_hold_") for c in cols)
    assert any(c.startswith("post24h_") for c in cols)


@needs_go
def test_every_post24h_column_is_forbidden():
    post = [c for c in go_columns() if c.startswith("post24h_")]
    assert post and all(C.is_forbidden(c) for c in post), [c for c in post if not C.is_forbidden(c)]


@needs_go
def test_every_pre_hold_column_is_a_feature_or_excluded_exactly_once():
    pre = {c for c in go_columns() if c.startswith("pre_hold_")}
    feats, excl = set(C.HOLD_FEATURES), set(C.HOLD_EXCLUDED)
    assert len(feats) == len(C.HOLD_FEATURES)
    assert not feats & excl, sorted(feats & excl)
    assert pre == feats | excl, (f"unclassified: {sorted(pre - feats - excl)}; "
                                 f"stale: {sorted((feats | excl) - pre)}")
    assert all(reason.strip() for reason in C.HOLD_EXCLUDED.values())


@needs_go
def test_hold_not_features_are_holder_columns():
    cols = set(go_columns())
    assert set(C.HOLD_NOT_FEATURES) <= cols, sorted(set(C.HOLD_NOT_FEATURES) - cols)


def test_hold_features_pass_the_leakage_guard():
    assert_no_leakage(C.HOLD_FEATURES)
    assert all(c.startswith("pre_hold_") for c in C.HOLD_FEATURES)


def test_hold_not_features_are_forbidden():
    assert all(C.is_forbidden(c) for c in C.HOLD_NOT_FEATURES)
    with pytest.raises(ValueError):
        assert_no_leakage(list(C.HOLD_FEATURES) + ["transfers_post"])


def test_pons_graduation_columns_are_forbidden():
    """The tracker records the Pons graduation when it finds it, also after the call."""
    for c in ("pons_grad_block", "venue_pool_id"):
        assert c in C.HOLD_NOT_FEATURES and C.is_forbidden(c), c


def test_is_forbidden_examples():
    assert C.is_forbidden("post24h_holders")
    assert not C.is_forbidden("pre_hold_holders")
    assert not C.is_forbidden("posted_age_s")       # the age the call post showed: known at the call


def test_hold_not_features_are_no_view_columns_or_model_inputs():
    """Put in the global FORBIDDEN_COLUMNS: none may be a dataset column or an input."""
    names = set(C.HOLD_NOT_FEATURES)
    assert not names & set(C.VIEW_COLUMNS), sorted(names & set(C.VIEW_COLUMNS))
    assert not names & model_inputs(), sorted(names & model_inputs())
    for v in C.VARIANTS:
        with C.overrides(**C.variant_settings(v)):
            assert not names & set(C.FEATURES), (v, sorted(names & set(C.FEATURES)))

