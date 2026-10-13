import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import make_synthetic  # noqa: E402
import train as train_mod  # noqa: E402
import pandas as pd  # noqa: E402


# config.DIAGNOSTICS as shipped, recorded by _no_diagnostics_by_default before
# it switches it off for the suite (read it through the shipped_diagnostics
# fixture).
_SHIPPED = {}


@pytest.fixture(autouse=True, scope="session")
def _no_diagnostics_by_default():
    """The report-only stability diagnostics retrain the baseline several times;
    tests (and fixtures) that call train() or the CLI get them only when they
    ask: diagnostics=True, or C.DIAGNOSTICS / SCOUT_ML_DIAGNOSTICS set inside
    the test (tests/test_diagnostics.py covers the default-on path). The
    shipped value is recorded first, so a test can still assert it."""
    from scout_ml import config as C
    _SHIPPED["DIAGNOSTICS"] = C.DIAGNOSTICS
    mp = pytest.MonkeyPatch()
    mp.setattr(C, "DIAGNOSTICS", False)
    mp.delenv(C.DIAGNOSTICS_ENV, raising=False)
    yield
    mp.undo()


@pytest.fixture
def shipped_diagnostics():
    """config.DIAGNOSTICS as shipped (before the suite switched it off)."""
    return _SHIPPED["DIAGNOSTICS"]


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
    """Run the real CLI once; returns (models root, version dir)."""
    root = tmp_path_factory.mktemp("models")
    assert train_mod.main(["--csv", str(synthetic_csv), "--out", str(root)]) == 0
    return root, root / (root / "LATEST").read_text().strip()
