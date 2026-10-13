import sys
from contextlib import contextmanager
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import make_synthetic  # noqa: E402
import train as train_mod  # noqa: E402
import pandas as pd  # noqa: E402


# config.DIAGNOSTICS and config.STABILITY_SEEDS as shipped, recorded by
# _no_diagnostics_by_default before it changes them for the suite (read them
# through the shipped_diagnostics / shipped_seeds fixtures).
_SHIPPED = {}


@pytest.fixture(autouse=True, scope="session")
def _no_diagnostics_by_default():
    """The report-only stability diagnostics retrain the baseline several times;
    tests (and fixtures) that call train() or the CLI get them only when they
    ask: diagnostics=True, or C.DIAGNOSTICS / SCOUT_ML_DIAGNOSTICS set inside
    the test (tests/test_diagnostics.py covers the default-on path).
    The bucket gates retrain the baseline with every seed of STABILITY_SEEDS in
    every training (owner decision of 13 Oct 2026); for speed the suite trains
    with the saved seed only (the median over one seed is that seed's pooled
    lift). The `trained` fixture, tests/test_diagnostics.py's end-to-end runs and
    the gate tests use the shipped seeds (shipped_seeds fixture / with_shipped_seeds).
    The shipped values are recorded first, so a test can still assert them."""
    from scout_ml import config as C
    _SHIPPED["DIAGNOSTICS"] = C.DIAGNOSTICS
    _SHIPPED["STABILITY_SEEDS"] = C.STABILITY_SEEDS
    mp = pytest.MonkeyPatch()
    mp.setattr(C, "DIAGNOSTICS", False)
    mp.setattr(C, "STABILITY_SEEDS", (C.LGBM_SEED,))
    mp.delenv(C.DIAGNOSTICS_ENV, raising=False)
    yield
    mp.undo()


@pytest.fixture
def shipped_diagnostics():
    """config.DIAGNOSTICS as shipped (before the suite switched it off)."""
    return _SHIPPED["DIAGNOSTICS"]


@pytest.fixture
def shipped_seeds():
    """config.STABILITY_SEEDS as shipped (before the suite reduced it to LGBM_SEED)."""
    return _SHIPPED["STABILITY_SEEDS"]


@contextmanager
def with_shipped_seeds():
    """Inside: config.STABILITY_SEEDS as shipped (for session / module fixtures)."""
    from scout_ml import config as C
    mp = pytest.MonkeyPatch()
    mp.setattr(C, "STABILITY_SEEDS", _SHIPPED["STABILITY_SEEDS"])
    try:
        yield
    finally:
        mp.undo()


@pytest.fixture(scope="session")
def synthetic_csv(tmp_path_factory):
    """170 days: the 30d bucket then has 140 days of matured calls, above its
    MIN_MATURED span (120), so every bucket trains."""
    path = tmp_path_factory.mktemp("data") / "calls.csv"
    make_synthetic.write_csv(make_synthetic.make(n=6000, days=170, seed=11), path)
    return path


@pytest.fixture(scope="session")
def synthetic_df(synthetic_csv):
    return pd.read_csv(synthetic_csv, low_memory=False)


@pytest.fixture(scope="session")
def trained(synthetic_csv, tmp_path_factory):
    """Run the real CLI once, with the shipped STABILITY_SEEDS (the gates' seed
    retrains); returns (models root, version dir)."""
    root = tmp_path_factory.mktemp("models")
    with with_shipped_seeds():
        assert train_mod.main(["--csv", str(synthetic_csv), "--out", str(root)]) == 0
    return root, root / (root / "LATEST").read_text().strip()
