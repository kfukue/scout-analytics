---
name: scout-ml
description: Rules for the scout analytics Python model package (ml/) and the dataset view scout_call_dataset_v (leakage, features, validation, gates, ML tests).
---

# Scout analytics: ML (`ml/`)

## Read first

`ml/README.md`. In short:

- Data is the `scout_call_dataset_v` view, one row per call: features known at
  the moment of the post, plus outcomes. Labels per bucket are defined in
  `scout_ml/config.py` and `scout_ml/labels.py` (net of buy/sell tax).
- `build_features()` in `scout_ml/features.py` is the one feature builder for
  both training and serving. Never add a second one.
- Outcome and bookkeeping columns never become inputs. That includes every
  `latest_*`, `ret_*`, `max_gain_*`, `max_dd_*`, plus `rugged` and
  `tracking_status`. The guard is `FORBIDDEN_PREFIXES` / `FORBIDDEN_COLUMNS` /
  `is_forbidden()` in `scout_ml/config.py`; `build_features` raises on them.
  Extend the guard when you add an outcome column.
- Perceptor re-check, re-scan and sAlpha re-ask reports arrive after the call
  and are never features. The dataset view must keep using the original
  at-call report; any change to the view needs a leakage check.
- Update posts (`post_kind = update`) are not calls: excluded from training and
  simulation and counted on their own line. `post_kind` is never an input.
  Repeat calls, `no_pool`/`gave_up` and non-USD rows are excluded and counted.
- Validation is time-based only: time split with embargo, all calls of a token
  in one part, walk-forward by week. No random splits.
- `train.py` exits 0 even when the gates fail. Quote the PASS/FAIL verdict and
  the gate values from `models/<version>/report.md` in your report.
- Project rule: scores never filter deliveries; they are only appended to the
  message. Which buckets appear is `SCOUT_MODEL_LINE` on the scoring service:
  default `passed` (only gate-passing buckets); prod runs `all` since 13 Oct
  2026 (failing buckets marked "(not validated)"), decided 12 Oct 2026
  because only he reads the group. Keep `passed` as the code default.

## Practices

- Guard against target leakage: nothing known after the call time may become a
  feature. When unsure about a column, treat it as an outcome and ask.
- Keep random seeds fixed so runs are reproducible; say which seed you used.
- Label definitions, thresholds, feature lists and gates live in
  `scout_ml/config.py`, not scattered through the code.
- **Extreme outcomes:** outcomes can contain bogus values from drained pools
  (e.g. +3.9e47%). Clip or exclude outcomes above a documented cap, and rows
  flagged rugged/invalid, as the task specifies. Put the cap in `config.py`
  and report how many rows were affected.
- Never print or commit exported datasets (e.g. `calls.csv` at the repo root)
  or model artefacts that contain data, unless the task says so. Summaries
  (counts, metrics) are fine.
- Charts in reports: static matplotlib images are fine.

## Testing (ML)

Full guide: `TESTING.md` (layers, commands, reporting).

- `python -m pytest tests -q` inside `ml/`, using `ml/.venv`
  (`ml/.venv/Scripts/python.exe -m pytest tests -q` on Windows). Report the
  pass/fail/skip counts.
- Add or update a test for every behaviour you change. Use
  `make_synthetic.py` data, never real exports, in tests.
