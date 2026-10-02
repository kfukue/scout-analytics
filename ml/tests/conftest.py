import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import make_synthetic  # noqa: E402
import train as train_mod  # noqa: E402
import pandas as pd  # noqa: E402


@pytest.fixture(scope="session")
def synthetic_csv(tmp_path_factory):
    """130 days so that the 30d bucket also has training rows after its embargo."""
    path = tmp_path_factory.mktemp("data") / "calls.csv"
    make_synthetic.write_csv(make_synthetic.make(n=6000, days=130, seed=11), path)
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
