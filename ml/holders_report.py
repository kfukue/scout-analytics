"""Holder pilot analysis (P1, report only; no model). Run from ml/:

    python holders_report.py [--holders DIR] [--ids PATH] [--out DIR]

Reads the -export-holders folder (holders.jsonl, columns.json, runs.jsonl) and
the owner's Q6 pilot ids CSV, and writes report.md and summary.json to a new
folder <out>/<YYYYMMDD-HHMM UTC>/ (counts and metrics only: no addresses, no
per-call rows). Defaults, from the repo root (the folder above ml/):
data/holders, data/holder_pilot_ids.csv, data/holders_analysis. --out must be
outside every checkout or inside a checkout's git-ignored data/ folder.

Exit codes: 0 written; 1 refused, nothing written; 2 usage error."""
import argparse
import sys
from pathlib import Path

from scout_ml import holders as H

REPO_ROOT = Path(__file__).resolve().parents[1]


def parse_args(argv):
    data = REPO_ROOT / "data"
    ap = argparse.ArgumentParser(description="Holder pilot analysis, report only (run from ml/).",
                                 epilog="exit codes: 0 written; 1 refused, nothing written; 2 usage error")
    ap.add_argument("--holders", default=str(data / "holders"),
                    help="the -export-holders folder (default: <repo root>/data/holders)")
    ap.add_argument("--ids", default=str(data / "holder_pilot_ids.csv"),
                    help="the Q6 pilot ids CSV (default: <repo root>/data/holder_pilot_ids.csv)")
    ap.add_argument("--out", default=str(data / "holders_analysis"),
                    help="where the run folder goes (default: <repo root>/data/holders_analysis)")
    return ap.parse_args(argv)


def main(argv=None, now=None) -> int:
    args = parse_args(argv)
    try:
        dest = H.run(args.holders, args.ids, args.out, REPO_ROOT, now=now)
    except H.Refused as e:
        print(f"refused: {e}. Nothing written", file=sys.stderr)
        return H.EXIT_REFUSED
    print(f"wrote {dest} (report.md, summary.json)")
    return H.EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
