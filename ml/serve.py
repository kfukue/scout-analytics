"""Scoring service.  Run:  uvicorn serve:app --host 127.0.0.1 --port 8601

The model directory comes from SCOUT_MODEL_DIR (default: ./models next to this
file). `LATEST` is re-read on every request, so a retrain is picked up without
a restart.

SCOUT_MODEL_LINE chooses which buckets `line` shows: "passed" (default, only
buckets that passed their gates) or "all" (every bucket with a model, the ones
that did not pass marked "(not validated)"). It is read on every request; a
running process does not see a changed environment, so changing it takes a
restart of this service only (no retrain, no listener restart). Any other
value counts as "passed", with one warning per distinct value."""
import logging
import os
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from scout_ml import config as C
from scout_ml.model import Bundle, resolve_version_dir

app = FastAPI(title="scout-ml")
log = logging.getLogger("scout-ml")
_cache: dict = {"dir": None, "bundle": None}
_warned: set = set()  # invalid SCOUT_MODEL_LINE values already warned about


def get_bundle() -> Bundle:
    root = os.environ.get("SCOUT_MODEL_DIR") or str(Path(__file__).parent / "models")
    try:
        version_dir = resolve_version_dir(root)
        if _cache["dir"] != version_dir:
            _cache.update(bundle=Bundle(version_dir), dir=version_dir)
    except (OSError, ValueError, KeyError) as e:
        raise HTTPException(503, f"no usable model in {root}: {e}") from e
    return _cache["bundle"]


def line_mode() -> str:
    """SCOUT_MODEL_LINE, read now: "passed" or "all" (case and surrounding
    spaces ignored). Unset or empty -> the default; anything else -> the
    default plus a warning, logged once per distinct value."""
    raw = os.environ.get(C.LINE_MODE_ENV, "")
    mode = raw.strip().lower()
    if not mode:
        return C.LINE_MODE_DEFAULT
    if mode in C.LINE_MODES:
        return mode
    if raw not in _warned:
        _warned.add(raw)
        log.warning("%s=%r is not one of %s; using %r", C.LINE_MODE_ENV, raw,
                    "/".join(C.LINE_MODES), C.LINE_MODE_DEFAULT)
    return C.LINE_MODE_DEFAULT


class ScoreRequest(BaseModel):
    row: dict[str, Any]  # one scout_call_dataset_v row; nulls and extra keys allowed


@app.get("/health")
def health():
    return {"ok": True, "model_version": get_bundle().version}


@app.post("/score")
def score(req: ScoreRequest):
    return get_bundle().score(req.row, line_mode=line_mode())
