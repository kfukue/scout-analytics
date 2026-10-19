"""Promote a model version, roll back, or show the pointers. Run from ml/:

    python promote.py --models DIR status
    python promote.py --models DIR promote candidate      # or a version name
    python promote.py --models DIR rollback

`promote` checks the version first (directory, meta.json, every model file it
lists, report.md, and a finite score of a fixed sample row by the serving
code); on any failure it changes nothing. Then it writes PREVIOUS (the old
LATEST), LATEST, and one line in DIR/promotions.log. The scoring service reads
LATEST on its next request: no restart. `rollback` promotes the version in
PREVIOUS, with the same checks.

Exit codes: 0 done (or status printed); 1 refused, nothing changed;
2 usage error; 3 the version is already LATEST, nothing changed; 4 a file
error while reading or writing the pointers (the message says what LATEST and
PREVIOUS are now, and whether each changed)."""
import argparse
import sys
from pathlib import Path

from scout_ml import config as C
from scout_ml import promote as P


def positive_int(text: str) -> int:
    value = int(text)
    if value < 1:
        raise argparse.ArgumentTypeError("must be 1 or more")
    return value


def by_word(text: str) -> str:
    """--by as an argparse type: a bad value is a usage error (exit 2)."""
    try:
        P.check_by(text)
    except P.Refused:
        raise argparse.ArgumentTypeError(
            "must be one word of ASCII letters, digits, '-', '_' or '.'") from None
    return text


def parse_args(argv):
    ap = argparse.ArgumentParser(description="Promote a model version, roll back, or show "
                                             "the pointers (run from ml/).",
                                 epilog="exit codes: 0 done; 1 refused, nothing changed; "
                                        "2 usage error; 3 already LATEST, nothing changed; "
                                        "4 file error while reading or writing the pointers "
                                        "(the message says what LATEST and PREVIOUS are now)")
    ap.add_argument("--models", required=True, help="models root (holds LATEST and the versions)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    pr = sub.add_parser("promote", help="check a version, then point LATEST at it")
    pr.add_argument("version", help='a version directory name, or "candidate" (the CANDIDATE pointer)')
    rb = sub.add_parser("rollback", help="promote the version in PREVIOUS (same checks)")
    for p in (pr, rb):
        p.add_argument("--by", type=by_word, default=C.PROMOTE_BY_DEFAULT,
                       help=f"who promoted, for promotions.log (default: {C.PROMOTE_BY_DEFAULT})")
    st = sub.add_parser("status", help="show LATEST, PREVIOUS, CANDIDATE and the newest versions")
    st.add_argument("--limit", type=positive_int, default=C.STATUS_MAX_VERSIONS,
                    help=f"versions to list (default: {C.STATUS_MAX_VERSIONS})")
    return ap.parse_args(argv)


def pointers_now(root) -> dict:
    return {name: P.pointer_state(root, name) for name in (C.LATEST_POINTER, C.PREVIOUS_POINTER)}


def moved_text(before: dict, after: dict) -> str:
    """'LATEST is X (unchanged), PREVIOUS is Y (was Z)'."""
    parts = []
    for name, now in after.items():
        change = "unchanged" if now == before[name] else f"was {before[name]}"
        parts.append(f"{name} is {now} ({change})")
    return ", ".join(parts)


def run_move(args) -> int:
    """promote / rollback: one line on stdout when done, on stderr when not."""
    before = pointers_now(args.models)
    live = before[C.LATEST_POINTER]
    try:
        if args.cmd == "rollback":
            res = P.rollback(args.models, by=args.by)
        else:
            res = P.promote(args.models, args.version, by=args.by)
    except P.AlreadyLive as e:
        print(f"{e} is already LATEST: nothing changed", file=sys.stderr)
        return C.PROMOTE_EXIT_ALREADY_LIVE
    except P.Refused as e:
        print(f"refused: {e}. Nothing changed; LATEST is still {live}", file=sys.stderr)
        return C.PROMOTE_EXIT_REFUSED
    except (OSError, UnicodeDecodeError) as e:  # a pointer could not be read (or is not
        # text) or written: the type only, never the path or the bytes
        print(f"error: {type(e).__name__} while moving the pointers; "
              f"{moved_text(before, pointers_now(args.models))}", file=sys.stderr)
        return C.PROMOTE_EXIT_ERROR
    print(f"LATEST is now {res['to']} (was {res['from'] or 'none'}; "
          f"{C.PREVIOUS_POINTER} = {res['from'] or 'unchanged'}). "
          "The scoring service serves it from its next request.")
    if res["log_error"]:
        print(f"WARNING: the pointers moved but the {C.PROMOTIONS_LOG} line could not be "
              f"written ({res['log_error']})", file=sys.stderr)
    return C.PROMOTE_EXIT_OK


def main(argv=None) -> int:
    args = parse_args(argv)
    if not Path(args.models).is_dir():
        print(f"refused: --models {args.models} is not a directory. Nothing changed", file=sys.stderr)
        return C.PROMOTE_EXIT_REFUSED
    if args.cmd == "status":
        print("\n".join(P.status_lines(args.models, args.limit)))
        return C.PROMOTE_EXIT_OK
    return run_move(args)


if __name__ == "__main__":
    sys.exit(main())
