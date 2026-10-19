"""train.py --candidate: CANDIDATE written last, LATEST never touched, and an
existing version directory refused (with or without --candidate). Fast: no
bucket trains (C.BUCKETS = {}), so only the pointers and the final files matter."""
import json

import pytest

import train as train_mod
from scout_ml import config as C

VERSION = "20990101-0000"


def _files(root):
    """{relative path: bytes} of every file under root."""
    return {str(p.relative_to(root)): p.read_bytes() for p in sorted(root.rglob("*")) if p.is_file()}


@pytest.fixture
def no_buckets(monkeypatch):
    monkeypatch.setattr(C, "BUCKETS", {})
    monkeypatch.setattr(train_mod, "version_name", lambda: VERSION)


def test_candidate_writes_candidate_last_and_never_latest(no_buckets, synthetic_csv, tmp_path,
                                                          monkeypatch):
    monkeypatch.setattr(C, "DIAGNOSTICS", True)          # the final writes include every section
    root = tmp_path / "models"
    root.mkdir()
    (root / "LATEST").write_bytes(b"old\n")
    seen = []                                            # (pointer, meta.json, report.md) at the call
    real = train_mod.write_pointer

    def spy(r, name, version):
        vdir = r / version
        seen.append((name, (vdir / "meta.json").read_bytes(), (vdir / "report.md").read_bytes()))
        real(r, name, version)
    monkeypatch.setattr(train_mod, "write_pointer", spy)

    argv = ["--csv", str(synthetic_csv), "--out", str(root), "--candidate", "--variants"]
    assert train_mod.main(argv) == 0
    vdir = root / VERSION
    assert (root / "LATEST").read_bytes() == b"old\n"
    assert (root / "CANDIDATE").read_text() == VERSION + "\n"
    assert not list(root.glob("*.tmp"))
    # one pointer write, after the last meta.json / report.md write
    assert [s[0] for s in seen] == ["CANDIDATE"]
    assert seen[0][1:] == ((vdir / "meta.json").read_bytes(), (vdir / "report.md").read_bytes())
    meta = json.loads((vdir / "meta.json").read_text())
    assert meta["version"] == VERSION
    assert {"dead_score", "diagnostics", "variants"} <= set(meta)

    # a second run with the same version name is refused and changes nothing
    before = _files(root)
    with pytest.raises(FileExistsError, match="already exists"):
        train_mod.main(argv)
    assert _files(root) == before
    assert len(seen) == 1


def test_default_run_points_latest_and_writes_no_candidate(no_buckets, synthetic_df, tmp_path):
    out = train_mod.train(synthetic_df, tmp_path)
    assert out == tmp_path / VERSION and (out / "meta.json").exists()
    assert (tmp_path / "LATEST").read_text() == VERSION + "\n"
    assert not (tmp_path / "CANDIDATE").exists()


def test_default_run_also_refuses_an_existing_version_dir(no_buckets, synthetic_df, tmp_path):
    (tmp_path / VERSION).mkdir()
    (tmp_path / VERSION / "meta.json").write_text("live")
    (tmp_path / "LATEST").write_text(VERSION)
    before = _files(tmp_path)
    with pytest.raises(FileExistsError, match="nothing was written"):
        train_mod.train(synthetic_df, tmp_path)
    assert _files(tmp_path) == before
