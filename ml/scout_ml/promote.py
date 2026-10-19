"""Promote, roll back and list model versions in a models root.

The scoring service serves the version named in `<models>/LATEST` and re-reads
it on every request, so moving that pointer is all a promotion does. Before
any pointer moves, a version must pass `check_version`: its directory, a
parseable meta.json whose version matches the directory name, every model file
meta.json lists, report.md, and a score of C.PROMOTE_SAMPLE_ROW with finite
numbers by the same `Bundle` the service uses. Then PREVIOUS (the old LATEST),
LATEST and one line of promotions.log are written, in that order.

Nothing here prints or returns text from report.md or from the gate `reasons`
(or `skipped` text) in meta.json: `status` shows only PASS / FAIL / skipped."""
import json
import math
import re
from datetime import datetime, timezone
from pathlib import Path

from . import config as C
from .model import Bundle, write_pointer

VERSION_NAME_RE = re.compile(r"[0-9A-Za-z._-]+")
BY_WORD_RE = re.compile(r"[0-9A-Za-z._-]+")
UNREADABLE = object()  # a pointer that exists but cannot be read (e.g. a directory)


class Refused(Exception):
    """A check failed; no pointer was changed. The message names the reason."""


class AlreadyLive(Exception):
    """The version asked for is already LATEST; nothing was changed."""


def is_version_name(name) -> bool:
    """One plain name of ASCII letters, digits, '.', '_' and '-', and not '.' or
    '..': no separator, space or control character, so a pointer can never lead
    outside the models root."""
    return (isinstance(name, str) and name not in (".", "..")
            and VERSION_NAME_RE.fullmatch(name) is not None)


def read_pointer(root, name: str):
    """The version named in `root/name`, stripped as resolve_version_dir does;
    None when the file is missing or empty."""
    try:
        value = (Path(root) / name).read_text().strip()
    except FileNotFoundError:
        return None
    return value or None


def read_pointer_guarded(root, name: str):
    """read_pointer for display: UNREADABLE instead of an error when the pointer
    exists but cannot be read (e.g. it is a directory, or not text)."""
    try:
        return read_pointer(root, name)
    except (OSError, UnicodeDecodeError):
        return UNREADABLE


def resolve_target(root, target: str) -> str:
    """`candidate` -> the version in the CANDIDATE pointer; else `target` itself."""
    if target != "candidate":
        return target
    version = read_pointer(root, C.CANDIDATE_POINTER)
    if version is None:
        raise Refused(f"no {C.CANDIDATE_POINTER} pointer in the models root "
                      "(train.py --candidate writes it)")
    return version


def load_meta(vdir: Path) -> dict:
    """meta.json of a version as a dict, or Refused (the message never quotes it)."""
    try:
        meta = json.loads((vdir / "meta.json").read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise Refused("meta.json is missing") from None
    except (OSError, ValueError):
        raise Refused("meta.json cannot be read or is not valid JSON") from None
    if not isinstance(meta, dict):
        raise Refused("meta.json is not a JSON object")
    return meta


def check_files(vdir: Path, meta: dict) -> None:
    """meta.json's version matches the directory, it lists at least one model,
    every listed model file exists, and report.md exists."""
    if meta.get("version") != vdir.name:
        raise Refused("meta.json's version does not match the directory name "
                      "(the service would report another version)")
    models = meta.get("models")
    if not isinstance(models, list) or not models:
        raise Refused("meta.json lists no models")
    for name in models:
        if not is_version_name(name):
            raise Refused("meta.json lists a model name that is not a plain file name")
        if not (vdir / f"{name}.joblib").is_file():
            raise Refused(f"model file {name}.joblib is missing")
    if not (vdir / "report.md").is_file():
        raise Refused("report.md is missing")


def _finite(value) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def check_scores(body: dict) -> None:
    """A /score body for the sample row: at least one bucket, and finite
    runner_prob, runner_rank_pct and collapse_prob (None = no collapse model)."""
    buckets = body.get("buckets") or []
    if not buckets:
        raise Refused("the sample row scored no bucket (no runner model)")
    for b in buckets:
        values = [b.get("runner_prob"), b.get("runner_rank_pct")]
        if b.get("collapse_prob") is not None:
            values.append(b["collapse_prob"])
        if not all(_finite(v) for v in values):
            raise Refused(f"bucket {b.get('bucket')}: the sample row gave a non-finite score")


def smoke_score(vdir: Path) -> None:
    """Load the version with the serving code and score C.PROMOTE_SAMPLE_ROW."""
    try:
        body = Bundle(vdir).score(dict(C.PROMOTE_SAMPLE_ROW), line_mode="all")
    except Exception as e:  # joblib, lightgbm, a broken meta key: any failure refuses
        raise Refused(f"loading or scoring the sample row failed ({type(e).__name__})") from None
    check_scores(body)


def check_version(root, version: str) -> Path:
    """Every check before a promotion; returns the version directory or raises Refused."""
    if not is_version_name(version):
        raise Refused(f"{version!r} is not a version name (one directory name, no path)")
    vdir = Path(root) / version
    if not vdir.is_dir():
        raise Refused(f"no version directory {version} in the models root")
    check_files(vdir, load_meta(vdir))
    smoke_score(vdir)
    return vdir


def log_line(when: datetime, old, new: str, by: str, action: str) -> str:
    """One promotions.log line (UTC, ISO 8601), e.g.
    2026-10-19T05:12:03Z promote from=20261012-0702 to=20261019-0500 by=manual"""
    stamp = when.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return f"{stamp} {action} from={old or 'none'} to={new} by={by}\n"


def append_log(root, line: str) -> None:
    with open(Path(root) / C.PROMOTIONS_LOG, "a", encoding="utf-8", newline="\n") as f:
        f.write(line)


def check_by(by: str) -> None:
    """--by is one word of ASCII letters, digits, '.', '_' and '-' (as a version
    name), so promotions.log keeps one plain line per event."""
    if not isinstance(by, str) or BY_WORD_RE.fullmatch(by) is None:
        raise Refused(f"--by must be one word of ASCII letters, digits, '-', '_' or '.', "
                      f"not {by!r}")


def promote(root, target: str, by: str = C.PROMOTE_BY_DEFAULT, action: str = "promote",
            now=None) -> dict:
    """Point LATEST at `target` (a version name or "candidate") after every check.

    Writes PREVIOUS (the current LATEST, when there is one), then LATEST, then
    appends to promotions.log. Raises Refused (nothing written) or AlreadyLive
    (nothing written). Returns {"from", "to", "log_error"}: log_error is None, or
    the error type when the pointers moved but the log line could not be written."""
    check_by(by)
    version = resolve_target(root, target)
    live = read_pointer(root, C.LATEST_POINTER)
    if not is_version_name(version):
        raise Refused(f"{version!r} is not a version name (one directory name, no path)")
    if version == live:
        raise AlreadyLive(version)
    check_version(root, version)
    if live is not None:
        write_pointer(root, C.PREVIOUS_POINTER, live)
    write_pointer(root, C.LATEST_POINTER, version)
    log_error = None
    try:
        append_log(root, log_line(now or datetime.now(timezone.utc), live, version, by, action))
    except OSError as e:
        log_error = type(e).__name__
    return {"from": live, "to": version, "log_error": log_error}


def rollback(root, by: str = C.PROMOTE_BY_DEFAULT, now=None) -> dict:
    """Promote the version in PREVIOUS (same checks). PREVIOUS then names the
    version rolled away from, so a second rollback goes back again."""
    previous = read_pointer(root, C.PREVIOUS_POINTER)
    if previous is None:
        raise Refused(f"no {C.PREVIOUS_POINTER} pointer: nothing to roll back to")
    return promote(root, previous, by=by, action="rollback", now=now)


def bucket_verdicts(meta: dict) -> list:
    """[(bucket, "PASS" | "FAIL" | "skipped")] from meta.json's metrics, in its
    order. PASS only when gates.passed is true (as Bundle.passed); the skip
    reason and the gate reasons are never returned."""
    metrics = meta.get("metrics")
    if not isinstance(metrics, dict):
        return []
    out = []
    for bucket, entry in metrics.items():
        entry = entry if isinstance(entry, dict) else {}
        gates = entry.get("gates")
        if entry.get("skipped"):
            verdict = "skipped"
        elif isinstance(gates, dict) and gates.get("passed") is True:
            verdict = "PASS"
        else:
            verdict = "FAIL"
        out.append((str(bucket), verdict))
    return out


def version_summary(vdir: Path) -> str:
    """'last_date <date>  short PASS, 3day FAIL' from meta.json only (never the models)."""
    try:
        meta = load_meta(vdir)
    except Refused as e:
        return f"({e})"
    data = meta.get("data") if isinstance(meta.get("data"), dict) else {}
    last = data.get("last_date")
    last = last if isinstance(last, str) and len(last) <= 32 else "?"
    gates = ", ".join(f"{b} {v}" for b, v in bucket_verdicts(meta)) or "no buckets"
    return f"last_date {last}  {gates}"


def version_dirs(root) -> list:
    """Version directories under root, newest first (names are UTC stamps)."""
    return sorted((p for p in Path(root).iterdir() if p.is_dir() and not p.name.startswith(".")),
                  key=lambda p: p.name, reverse=True)


def pointer_text(value) -> str:
    if value is None:
        return "none"
    if value is UNREADABLE:
        return "(unreadable)"
    return value if is_version_name(value) else "(not a version name)"


def pointer_state(root, name: str) -> str:
    """A pointer as status shows it, "(unreadable)" when reading it fails:
    never raises, never quotes anything but a version name."""
    return pointer_text(read_pointer_guarded(root, name))


def status_lines(root, limit: int = C.STATUS_MAX_VERSIONS) -> list:
    """The `status` output: the three pointers, then up to `limit` versions,
    newest first, each marked live / previous / candidate. An unreadable
    pointer shows as "(unreadable)" with a WARNING line; the versions are
    still listed."""
    pointers = {name: read_pointer_guarded(root, name) for name in
                (C.LATEST_POINTER, C.PREVIOUS_POINTER, C.CANDIDATE_POINTER)}
    marks = {C.LATEST_POINTER: "live", C.PREVIOUS_POINTER: "previous",
             C.CANDIDATE_POINTER: "candidate"}
    lines = [f"{name + ':':<10} {pointer_text(value)}" for name, value in pointers.items()]
    dirs = version_dirs(root)
    names = {p.name for p in dirs}
    for name, value in pointers.items():
        if value is UNREADABLE:
            lines.append(f"WARNING: {name} cannot be read (is it a directory, not a file?)")
        elif value is not None and value not in names:
            lines.append(f"WARNING: {name} names no version directory in the models root")
    lines.append(f"versions (newest first, {min(limit, len(dirs))} of {len(dirs)}):")
    for p in dirs[:limit]:
        tags = [marks[n] for n, v in pointers.items() if v == p.name]
        tag = f"  [{', '.join(tags)}]" if tags else ""
        lines.append(f"  {p.name}  {version_summary(p)}{tag}")
    return lines
