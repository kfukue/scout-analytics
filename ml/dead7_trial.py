"""The dead7 trial (report only; nothing saved or served). Run from ml/:

    python dead7_trial.py --csv <dataset export> [--holders DIR] [--ids PATH] [--pilot-ids PATH]
                          [--mode trial|confirm] [--out DIR]

Does pre_hold_sellers add to the live model's inputs for predicting dead7? Two LightGBM
classifiers per seed and window on the same rows, with and without it; the rules are the
DEAD7_* block of scout_ml/config.py (owner, 29 Oct 2026). Reads the dataset-view export
(Go -export-dataset), the -export-holders folder, the ids file (label: grp dead7) and the
pilot ids (training rows only, never test rows); writes report.md and summary.json to a
new folder <out>/<YYYYMMDD-HHMM UTC>/ (counts and metrics only). Defaults, from the repo
root (the folder above ml/): data/holders, data/holder_full_ids.csv,
data/holder_pilot_ids.csv, data/dead7_trial. --out must be outside every checkout or
inside a checkout's git-ignored data/ folder. Never writes a model file, LATEST or CANDIDATE.

Exit codes: 0 written; 1 refused, nothing written; 2 usage error."""
import argparse
import sys
from pathlib import Path

from scout_ml import dead7 as T
from scout_ml import holders as H

REPO_ROOT = Path(__file__).resolve().parents[1]


def parse_args(argv):
    data = REPO_ROOT / "data"
    ap = argparse.ArgumentParser(description="The dead7 trial, report only (run from ml/).",
                                 epilog="exit codes: 0 written; 1 refused, nothing written; 2 usage error")
    ap.add_argument("--csv", required=True, help="the dataset-view export (Go -export-dataset)")
    ap.add_argument("--holders", default=str(data / "holders"),
                    help="the -export-holders folder (default: <repo root>/data/holders)")
    ap.add_argument("--ids", default=str(data / "holder_full_ids.csv"),
                    help="the ids file with grp (default: <repo root>/data/holder_full_ids.csv)")
    ap.add_argument("--pilot-ids", default=str(data / "holder_pilot_ids.csv"),
                    help="the pilot ids, never test rows (default: <repo root>/data/holder_pilot_ids.csv)")
    ap.add_argument("--mode", default=T.TRIAL, choices=T.MODES,
                    help="trial (weekly walk-forward, default) or confirm (the one fixed-date split)")
    ap.add_argument("--out", default=str(data / "dead7_trial"),
                    help="where the run folder goes (default: <repo root>/data/dead7_trial)")
    return ap.parse_args(argv)


def main(argv=None, now=None) -> int:
    args = parse_args(argv)
    try:
        dest = T.run(args.csv, args.holders, args.ids, args.pilot_ids, args.mode, args.out, REPO_ROOT, now=now)
    except H.Refused as e:
        print(f"refused: {e}. Nothing written", file=sys.stderr)
        return H.EXIT_REFUSED
    print(f"wrote {dest} ({T.REPORT_FILE}, {T.SUMMARY_FILE})")
    return H.EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
