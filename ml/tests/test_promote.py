"""promote.py: promote / rollback / status. Every check runs before any pointer
moves, a refusal leaves LATEST and PREVIOUS byte-identical, nothing from
report.md or the gate reasons is ever printed, and the scoring service follows
a promotion with no restart. Fast: one tiny LightGBM bundle (5 trees on 300
synthetic rows) built once for the module, then copied per test."""
import json
import shutil
from datetime import datetime, timezone

import joblib
import lightgbm as lgb
import numpy as np
import pytest
from sklearn.linear_model import LogisticRegression

import make_synthetic
import promote as cli
from scout_ml import config as C
from scout_ml import promote as P
from scout_ml.features import build_features, dex_family_rules, learn_cat_levels

SECRET = "SECRET-REASON-token-7f3a91"      # planted in reasons, skip text and report.md
V1, V2, V3 = "20261012-0702", "20261019-0500", "20261019-0600"
NOW = datetime(2026, 10, 19, 5, 12, 3, tzinfo=timezone.utc)


@pytest.fixture(scope="module")
def template(tmp_path_factory):
    """A loadable version dir (name 'template'): short_runner + short_collapse,
    meta.json in train.py's layout, report.md; gates reasons hold SECRET."""
    vdir = tmp_path_factory.mktemp("bundle") / "template"
    vdir.mkdir()
    df = make_synthetic.make(n=300, days=10, seed=3)
    levels = learn_cat_levels(df)
    X = build_features(df, levels, list(C.FEATURES))
    y = np.random.default_rng(0).integers(0, 2, len(X))
    model = lgb.LGBMClassifier(n_estimators=5, min_child_samples=5, verbose=-1,
                               random_state=C.LGBM_SEED).fit(X, y)
    for label in ("runner", "collapse"):
        joblib.dump({"model": model, "calibrator": None}, vdir / f"short_{label}.joblib")
    raw = model.predict(X, raw_score=True)
    ref = np.quantile(1 / (1 + np.exp(-raw)), np.linspace(0, 1, 21)).tolist()
    meta = {"version": "template", "features": list(C.FEATURES),
            "category_levels": {"short": levels}, "dex_family_rules": dex_family_rules(),
            "models": ["short_collapse", "short_runner"], "runner_reference": {"short": ref},
            "data": {"last_date": "2026-10-18"},
            "metrics": {"short": {"gates": {"passed": True, "reasons": [SECRET]}},
                        "3day": {"gates": {"passed": False, "reasons": [SECRET]}},
                        "long": {"skipped": SECRET}}}
    (vdir / "meta.json").write_text(json.dumps(meta))
    (vdir / "report.md").write_text(f"# report\n\nFAIL: {SECRET}\n")
    return vdir


def make_version(template, root, name, meta_edit=None):
    """Copy the template to root/name with meta.json's version set to name;
    meta_edit(meta) may change the meta further."""
    dest = root / name
    shutil.copytree(template, dest)
    meta = json.loads((dest / "meta.json").read_text())
    meta["version"] = name
    if meta_edit:
        meta_edit(meta)
    (dest / "meta.json").write_text(json.dumps(meta))
    return dest


@pytest.fixture
def root(template, tmp_path):
    """Models root: V1 live (LATEST), V2 the candidate (CANDIDATE), no PREVIOUS."""
    r = tmp_path / "models"
    r.mkdir()
    for name in (V1, V2):
        make_version(template, r, name)
    (r / "LATEST").write_bytes(f"{V1}\n".encode())
    (r / "CANDIDATE").write_bytes(f"{V2}\n".encode())
    return r


def pointer_bytes(r):
    return {n: (r / n).read_bytes() if (r / n).is_file() else None
            for n in ("LATEST", "PREVIOUS", "CANDIDATE", "promotions.log")}


def run(r, *args, capsys=None):
    code = cli.main(["--models", str(r), *args])
    out = capsys.readouterr() if capsys else None
    return code, out


# --- promote ------------------------------------------------------------------

def test_promote_candidate_writes_previous_latest_and_log(root, capsys):
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == 0, out
    assert (root / "LATEST").read_bytes() == f"{V2}\n".encode()
    assert (root / "PREVIOUS").read_bytes() == f"{V1}\n".encode()
    log = (root / "promotions.log").read_bytes().decode()
    assert log.count("\n") == 1 and log.endswith("\n") and "\r" not in log
    stamp, action, frm, to, by = log.split()
    assert stamp.endswith("Z") and len(stamp) == len("2026-10-19T05:12:03Z")
    assert (action, frm, to, by) == ("promote", f"from={V1}", f"to={V2}", "by=manual")
    assert f"LATEST is now {V2}" in out.out
    assert SECRET not in out.out + out.err
    assert not list(root.glob(".*.tmp"))


def test_promote_by_version_name_and_by_auto(root):
    res = P.promote(root, V2, by="auto", now=NOW)
    assert res == {"from": V1, "to": V2, "log_error": None}
    assert (root / "promotions.log").read_text() == (
        f"2026-10-19T05:12:03Z promote from={V1} to={V2} by=auto\n")


def test_promote_without_latest_leaves_previous_absent(root):
    (root / "LATEST").unlink()
    assert P.promote(root, V2, now=NOW)["from"] is None
    assert (root / "LATEST").read_text() == f"{V2}\n"
    assert not (root / "PREVIOUS").exists()
    assert "from=none" in (root / "promotions.log").read_text()


def _corrupt_meta(r):
    (r / V2 / "meta.json").write_text('{"version": "' + V2 + '", "models": [')


def _missing_model(r):
    (r / V2 / "short_collapse.joblib").unlink()


def _missing_report(r):
    (r / V2 / "report.md").unlink()


def _nan_probability(r):
    """A real bundle whose calibrator gives NaN: the serving code returns a NaN score."""
    fitted = joblib.load(r / V2 / "short_runner.joblib")
    cal = LogisticRegression().fit(np.array([[0.0], [1.0]]), [0, 1])
    cal.coef_ = np.array([[np.nan]])
    joblib.dump({**fitted, "calibrator": cal}, r / V2 / "short_runner.joblib")


def _unloadable_model(r):
    joblib.dump({"model": None, "calibrator": None}, r / V2 / "short_runner.joblib")


def _no_models(r):
    meta = json.loads((r / V2 / "meta.json").read_text())
    meta["models"] = []
    (r / V2 / "meta.json").write_text(json.dumps(meta))


def _wrong_meta_version(r):
    meta = json.loads((r / V2 / "meta.json").read_text())
    meta["version"] = V1
    (r / V2 / "meta.json").write_text(json.dumps(meta))


def _no_dir(r):
    shutil.rmtree(r / V2)


@pytest.mark.parametrize("break_it,why", [
    (_corrupt_meta, "meta.json cannot be read or is not valid JSON"),
    (_missing_model, "model file short_collapse.joblib is missing"),
    (_missing_report, "report.md is missing"),
    (_nan_probability, "non-finite score"),
    (_unloadable_model, "loading or scoring the sample row failed"),
    (_no_models, "meta.json lists no models"),
    (_wrong_meta_version, "does not match the directory name"),
    (_no_dir, f"no version directory {V2}"),
])
def test_broken_candidate_is_refused_and_nothing_changes(root, capsys, break_it, why):
    (root / "PREVIOUS").write_bytes(b"20261001-0000\n")
    break_it(root)
    before = pointer_bytes(root)
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == C.PROMOTE_EXIT_REFUSED == 1
    assert why in out.err, out.err
    assert f"LATEST is still {V1}" in out.err
    assert pointer_bytes(root) == before                    # LATEST, PREVIOUS byte-identical, no log
    assert SECRET not in out.out + out.err
    assert not list(root.glob(".*.tmp"))


@pytest.mark.parametrize("target", ["../x", "..", ".", "a/b", "a\\b", "", "a\nb", "a b",
                                    "a\tb", f"{V2}\x00", " " + V2])
def test_a_path_is_never_a_version(root, target):
    before = pointer_bytes(root)
    with pytest.raises(P.Refused, match="not a version name"):
        P.promote(root, target)
    assert pointer_bytes(root) == before


def test_candidate_pointer_with_a_path_is_refused(root):
    (root / "CANDIDATE").write_text("../elsewhere\n")
    before = pointer_bytes(root)
    with pytest.raises(P.Refused, match="not a version name"):
        P.promote(root, "candidate")
    assert pointer_bytes(root) == before


def test_missing_candidate_pointer_is_refused(root, capsys):
    (root / "CANDIDATE").unlink()
    before = pointer_bytes(root)
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == 1 and "no CANDIDATE pointer" in out.err
    assert pointer_bytes(root) == before


BAD_BY = ["two words", "a\nb", "", "café", "١٢", "a\n"]   # non-ASCII letter / digits


@pytest.mark.parametrize("by", BAD_BY)
def test_by_must_be_one_word(root, by):
    before = pointer_bytes(root)
    with pytest.raises(P.Refused, match="--by"):
        P.promote(root, V2, by=by)
    assert pointer_bytes(root) == before


@pytest.mark.parametrize("cmd", [["promote", V2], ["rollback"]])
@pytest.mark.parametrize("by", BAD_BY)
def test_bad_by_is_a_usage_error_exit_2(root, capsys, cmd, by):
    (root / "PREVIOUS").write_bytes(f"{V2}\n".encode())
    before = pointer_bytes(root)
    with pytest.raises(SystemExit) as ex:
        cli.main(["--models", str(root), *cmd, "--by", by])
    assert ex.value.code == 2
    assert "--by: must be one word" in capsys.readouterr().err
    assert pointer_bytes(root) == before


def test_good_by_reaches_the_log(root):
    assert run(root, "promote", V2, "--by", "auto")[0] == 0
    assert (root / "promotions.log").read_text().split()[-1] == "by=auto"


def test_promoting_the_live_version_changes_nothing_exit_3(root, capsys):
    (root / "PREVIOUS").write_bytes(b"20261001-0000\n")
    before = pointer_bytes(root)
    code, out = run(root, "promote", V1, capsys=capsys)
    assert code == C.PROMOTE_EXIT_ALREADY_LIVE == 3
    assert f"{V1} is already LATEST: nothing changed" in out.err
    assert pointer_bytes(root) == before


def test_log_failure_after_the_move_is_a_warning(root, monkeypatch, capsys):
    def boom(r, line):
        raise PermissionError("denied")
    monkeypatch.setattr(P, "append_log", boom)
    code, out = run(root, "promote", V2, capsys=capsys)
    assert code == 0
    assert (root / "LATEST").read_text() == f"{V2}\n"
    assert "promotions.log line could not be written (PermissionError)" in out.err


def test_latest_write_failure_after_previous_exit_4_names_both(root, monkeypatch, capsys):
    """PREVIOUS is written, then writing LATEST fails: exit 4, and the message
    says LATEST did not move while PREVIOUS did. No traceback, no log line."""
    real = P.write_pointer

    def fail_on_latest(r, name, version):
        if name == C.LATEST_POINTER:
            raise PermissionError(f"denied {r}")
        real(r, name, version)
    monkeypatch.setattr(P, "write_pointer", fail_on_latest)
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == C.PROMOTE_EXIT_ERROR == 4
    assert out.err.strip() == (f"error: PermissionError while moving the pointers; "
                               f"LATEST is {V1} (unchanged), PREVIOUS is {V1} (was none)")
    assert "Traceback" not in out.err and str(root) not in out.err and out.out == ""
    assert (root / "LATEST").read_bytes() == f"{V1}\n".encode()
    assert (root / "PREVIOUS").read_bytes() == f"{V1}\n".encode()
    assert not (root / "promotions.log").exists()
    assert not list(root.glob(".*.tmp"))


@pytest.mark.parametrize("pointer", ["LATEST", "CANDIDATE", "PREVIOUS"])
def test_pointer_that_is_a_directory_exit_4(root, capsys, pointer):
    """A pointer that is a directory: reading it (LATEST, CANDIDATE) or writing
    it (PREVIOUS) fails with an OS-dependent OSError. Exit 4; the message shows
    it as (unreadable) and that nothing else moved."""
    (root / pointer).unlink(missing_ok=True)
    (root / pointer).mkdir()
    before = pointer_bytes(root)
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == C.PROMOTE_EXIT_ERROR
    assert out.err.startswith("error: ") and "while moving the pointers; " in out.err
    latest = "(unreadable)" if pointer == "LATEST" else V1
    previous = "(unreadable)" if pointer == "PREVIOUS" else "none"
    assert f"LATEST is {latest} (unchanged), PREVIOUS is {previous} (unchanged)" in out.err
    assert "Traceback" not in out.err and str(root) not in out.err
    assert (root / pointer).is_dir() and pointer_bytes(root) == before
    assert not list(root.glob(".*.tmp"))


def test_rollback_with_previous_a_directory_exit_4(root, capsys):
    (root / "PREVIOUS").mkdir()
    code, out = run(root, "rollback", capsys=capsys)
    assert code == 4
    assert f"LATEST is {V1} (unchanged), PREVIOUS is (unreadable) (unchanged)" in out.err


@pytest.mark.parametrize("pointer", ["CANDIDATE", "LATEST"])
def test_pointer_with_non_utf8_bytes_exit_4(root, capsys, pointer):
    """A pointer holding bytes that are not text (invalid in UTF-8 and in
    Windows cp1252) fails to decode while promote reads it: exit 4, shown as
    (unreadable), no traceback, nothing moved."""
    (root / pointer).write_bytes(b"\x81\x8d\x8f\x90\x9d\n")
    before = pointer_bytes(root)
    code, out = run(root, "promote", "candidate", capsys=capsys)
    assert code == C.PROMOTE_EXIT_ERROR == 4
    assert out.err.startswith("error: UnicodeDecodeError while moving the pointers; ")
    latest = "(unreadable)" if pointer == "LATEST" else V1
    assert f"LATEST is {latest} (unchanged), PREVIOUS is none (unchanged)" in out.err
    assert "Traceback" not in out.err and str(root) not in out.err and out.out == ""
    assert pointer_bytes(root) == before
    assert not list(root.glob(".*.tmp"))

def test_missing_models_dir_is_refused(tmp_path, capsys):
    code, out = run(tmp_path / "nope", "status", capsys=capsys)
    assert code == 1 and "is not a directory" in out.err


# --- rollback -----------------------------------------------------------------

def test_rollback_restores_the_earlier_version_and_toggles(root, capsys):
    assert run(root, "promote", "candidate")[0] == 0
    code, out = run(root, "rollback", capsys=capsys)
    assert code == 0, out
    assert (root / "LATEST").read_text() == f"{V1}\n"
    assert (root / "PREVIOUS").read_text() == f"{V2}\n"     # a second rollback goes back to V2
    lines = (root / "promotions.log").read_text().splitlines()
    assert [ln.split()[1:] for ln in lines] == [
        ["promote", f"from={V1}", f"to={V2}", "by=manual"],
        ["rollback", f"from={V2}", f"to={V1}", "by=manual"]]
    assert run(root, "rollback")[0] == 0
    assert (root / "LATEST").read_text() == f"{V2}\n"


def test_rollback_without_previous_is_refused(root, capsys):
    before = pointer_bytes(root)
    code, out = run(root, "rollback", capsys=capsys)
    assert code == 1 and "no PREVIOUS pointer" in out.err
    assert pointer_bytes(root) == before


def test_rollback_to_a_broken_version_is_refused(root, capsys):
    assert run(root, "promote", V2)[0] == 0
    (root / V1 / "report.md").unlink()
    before = pointer_bytes(root)
    code, out = run(root, "rollback", capsys=capsys)
    assert code == 1 and "report.md is missing" in out.err
    assert pointer_bytes(root) == before


# --- status -------------------------------------------------------------------

def test_status_lists_pointers_and_versions_without_reasons(root, template, capsys):
    make_version(template, root, V3)
    (root / "PREVIOUS").write_text(f"{V3}\n")
    (root / "20261020-0000").mkdir()                           # a stopped run: no meta.json
    (root / ".hidden").mkdir()
    code, out = run(root, "status", capsys=capsys)
    assert code == 0
    lines = out.out.splitlines()
    assert lines[:3] == [f"LATEST:    {V1}", f"PREVIOUS:  {V3}", f"CANDIDATE: {V2}"]
    assert lines[3] == "versions (newest first, 4 of 4):"
    assert lines[4] == "  20261020-0000  (meta.json is missing)"
    assert lines[5] == f"  {V3}  last_date 2026-10-18  short PASS, 3day FAIL, long skipped  [previous]"
    assert lines[6].startswith(f"  {V2}  last_date") and lines[6].endswith("[candidate]")
    assert lines[7].startswith(f"  {V1}  last_date") and lines[7].endswith("[live]")
    assert SECRET not in out.out + out.err
    assert "reason" not in out.out.lower()


def test_status_limit_none_pointers_and_dangling_pointer(root, capsys):
    (root / "CANDIDATE").unlink()
    (root / "PREVIOUS").write_text("20250101-0000\n")
    code, out = run(root, "status", "--limit", "1", capsys=capsys)
    assert code == 0
    lines = out.out.splitlines()
    assert "CANDIDATE: none" in lines
    assert "WARNING: PREVIOUS names no version directory in the models root" in lines
    assert "versions (newest first, 1 of 2):" in lines
    assert lines[-1].startswith(f"  {V2}  ")


@pytest.mark.parametrize("pointer", ["LATEST", "PREVIOUS", "CANDIDATE"])
def test_status_with_a_pointer_that_is_a_directory(root, capsys, pointer):
    """An unreadable pointer: exit 0, shown as (unreadable) with a WARNING
    naming it, the other pointers as usual, and the versions still listed."""
    (root / pointer).unlink(missing_ok=True)
    (root / pointer).mkdir()
    code, out = run(root, "status", capsys=capsys)
    assert code == 0
    assert "Traceback" not in out.err and str(root) not in out.out
    lines = out.out.splitlines()
    shown = {"LATEST": V1, "PREVIOUS": "none", "CANDIDATE": V2, pointer: "(unreadable)"}
    assert lines[:3] == [f"{n + ':':<10} {shown[n]}" for n in ("LATEST", "PREVIOUS", "CANDIDATE")]
    assert f"WARNING: {pointer} cannot be read (is it a directory, not a file?)" in lines
    assert sum(ln.startswith("WARNING:") for ln in lines) == 1
    assert any(ln.startswith(f"  {V1}  last_date") for ln in lines)
    assert any(ln.startswith(f"  {V2}  last_date") for ln in lines)


def test_bucket_verdicts_fail_closed():
    meta = {"metrics": {"a": {"gates": {"passed": True}}, "b": {"gates": {"passed": "yes"}},
                        "c": {}, "d": {"skipped": "too few rows " + SECRET}, "e": None}}
    assert P.bucket_verdicts(meta) == [("a", "PASS"), ("b", "FAIL"), ("c", "FAIL"),
                                       ("d", "skipped"), ("e", "FAIL")]
    assert P.bucket_verdicts({}) == []


# --- the scoring service follows a promotion with no restart ---------------------

def test_serve_returns_the_new_version_after_promote(root, monkeypatch):
    from fastapi.testclient import TestClient
    import serve
    monkeypatch.setenv("SCOUT_MODEL_DIR", str(root))
    monkeypatch.delenv(C.LINE_MODE_ENV, raising=False)
    serve._cache.update(dir=None, bundle=None)
    try:
        client = TestClient(serve.app)
        assert client.get("/health").json() == {"ok": True, "model_version": V1}
        assert run(root, "promote", "candidate")[0] == 0
        assert client.get("/health").json() == {"ok": True, "model_version": V2}
        body = client.post("/score", json={"row": C.PROMOTE_SAMPLE_ROW}).json()
        assert body["model_version"] == V2 and body["line"].startswith(f"Model {V2}")
        assert run(root, "rollback")[0] == 0
        assert client.get("/health").json()["model_version"] == V1
    finally:
        serve._cache.update(dir=None, bundle=None)
