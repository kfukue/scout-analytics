"""Scoring service.  Run:  uvicorn serve:app --host 127.0.0.1 --port 8601

The model directory comes from SCOUT_MODEL_DIR (default: ./models next to this
file). `LATEST` is re-read on every request, so a retrain is picked up without
a restart."""
import os
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from scout_ml.model import Bundle, resolve_version_dir

app = FastAPI(title="scout-ml")
_cache: dict = {"dir": None, "bundle": None}


def get_bundle() -> Bundle:
    root = os.environ.get("SCOUT_MODEL_DIR") or str(Path(__file__).parent / "models")
    try:
        version_dir = resolve_version_dir(root)
        if _cache["dir"] != version_dir:
            _cache.update(bundle=Bundle(version_dir), dir=version_dir)
    except (OSError, ValueError, KeyError) as e:
        raise HTTPException(503, f"no usable model in {root}: {e}") from e
    return _cache["bundle"]


class ScoreRequest(BaseModel):
    row: dict[str, Any]  # one scout_call_dataset_v row; nulls and extra keys allowed


@app.get("/health")
def health():
    return {"ok": True, "model_version": get_bundle().version}


@app.post("/score")
def score(req: ScoreRequest):
    return get_bundle().score(req.row)
