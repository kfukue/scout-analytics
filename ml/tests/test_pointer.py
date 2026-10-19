"""write_pointer: the LATEST pointer is replaced atomically, never truncated in place."""
import os
import sys
from pathlib import Path

import pytest

from scout_ml.model import resolve_version_dir, write_pointer


def names(d):
    return sorted(p.name for p in d.iterdir())


def test_write_pointer_new_file(tmp_path):
    write_pointer(tmp_path, "LATEST", "v2")
    assert (tmp_path / "LATEST").read_bytes() == b"v2\n"
    assert names(tmp_path) == ["LATEST"]                       # no temp file left


def test_write_pointer_replaces_old_value(tmp_path):
    (tmp_path / "LATEST").write_bytes(b"v1\n")
    write_pointer(tmp_path, "LATEST", "v2")
    assert (tmp_path / "LATEST").read_bytes() == b"v2\n"
    assert names(tmp_path) == ["LATEST"]


def test_write_pointer_failed_replace_keeps_old_pointer(tmp_path, monkeypatch):
    (tmp_path / "LATEST").write_bytes(b"v1\n")

    def boom(src, dst):
        raise OSError("replace failed")
    monkeypatch.setattr(os, "replace", boom)
    with pytest.raises(OSError, match="replace failed"):
        write_pointer(tmp_path, "LATEST", "v2")
    assert (tmp_path / "LATEST").read_bytes() == b"v1\n"       # byte-identical
    assert names(tmp_path) == ["LATEST"]                       # temp file removed


def test_write_pointer_failed_cleanup_keeps_original_error(tmp_path, monkeypatch):
    (tmp_path / "LATEST").write_bytes(b"v1\n")

    def boom(src, dst):
        raise OSError("replace failed")

    def unlink_fails(self, missing_ok=False):
        raise OSError("unlink failed")
    monkeypatch.setattr(os, "replace", boom)
    monkeypatch.setattr(Path, "unlink", unlink_fails)
    with pytest.raises(OSError, match="replace failed"):       # not "unlink failed"
        write_pointer(tmp_path, "LATEST", "v2")
    assert (tmp_path / "LATEST").read_bytes() == b"v1\n"


@pytest.mark.skipif(sys.platform == "win32", reason="POSIX file modes; Windows only has a read-only bit")
def test_write_pointer_keeps_existing_mode(tmp_path):
    (tmp_path / "LATEST").write_bytes(b"v1\n")
    (tmp_path / "LATEST").chmod(0o600)
    write_pointer(tmp_path, "LATEST", "v2")
    assert (tmp_path / "LATEST").read_bytes() == b"v2\n"
    assert (tmp_path / "LATEST").stat().st_mode & 0o777 == 0o600


def test_resolve_version_dir_reads_written_pointer(tmp_path):
    (tmp_path / "v2").mkdir()
    write_pointer(tmp_path, "LATEST", "v2")
    assert resolve_version_dir(tmp_path) == tmp_path / "v2"
