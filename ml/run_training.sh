#!/usr/bin/env bash
# One-shot model training on the prod server: ml/RUNBOOK.md steps 1, 2, 4, 5, 6
# plus the step 9 check, with the same commands and flags as the runbook.
# RUNBOOK.md stays the reference; read it once before the first run.
#
# Usage (run a copy taken from origin/main; the deployed checkout is not
# touched):
#   cd /path/to/scout-analytics && git fetch origin main &&
#   mkdir -p ~/scout-ml &&
#   git show origin/main:ml/run_training.sh > ~/scout-ml/run_training.sh &&
#   REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh [options]
#
# REPO is the deployed checkout (with .env). ML (default: $HOME/scout-ml) must
# be an absolute path outside REPO, and REPO must not be inside ML. Do not run
# the copy in $ML/src/ml/ (step 1 rewrites it while it runs); the script
# refuses that.
#
# Options:
#   --skip-install   do not run pip install (for re-runs; the venv must exist)
#   --binary PATH    export with a built binary instead of `go run .`
#                    (a relative PATH is taken from the directory you start in)
#   -h, --help       show this help
#
# It never connects to the database itself, never prints .env (it only counts
# SCOUT_MODEL_URL lines in it), and always deletes $ML/data/calls.csv when it
# exits (success, error or Ctrl+C). It does not start serve.py and does not
# set SCOUT_MODEL_URL.

set -euo pipefail
# umask 077 is set just before step 4 (the export), not here: step 1 writes
# into $REPO/.git, which must keep the checkout's normal permissions.

usage() {  # the header comment above, up to the first non-comment line
    awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "${BASH_SOURCE[0]}"
}

SKIP_INSTALL=0
BINARY=""
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --skip-install) SKIP_INSTALL=1; shift ;;
        --binary)
            [ $# -ge 2 ] || { echo "run_training.sh: --binary needs a path" >&2; exit 2; }
            BINARY="$2"; shift 2 ;;
        --binary=*) BINARY="${1#--binary=}"; shift ;;
        *) echo "run_training.sh: unknown option: $1 (see -h)" >&2; exit 2 ;;
    esac
done

# ---------------------------------------------------------------------------
# failure handling: one line saying which step failed; when a logged command
# failed, also the last 20 lines of its output (never the CSV or .env).
# The CSV is always removed.
STEP="setup"
LOG=""          # output log of the running step, shown on failure
DIED=0          # set by die(): message already printed
CSV=""

die() {
    DIED=1
    echo >&2
    echo "FAILED at step ${STEP}: $*" >&2
    echo "Tell the product manager (paste this message; never the CSV or anything from .env)." >&2
    exit 1
}

on_exit() {
    local rc=$? had_csv=0
    if [ -n "$CSV" ]; then
        [ ! -e "$CSV" ] || had_csv=1
        rm -f "$CSV"
    fi
    if [ "$rc" -ne 0 ] && [ "$DIED" -eq 0 ]; then
        echo >&2
        echo "FAILED at step ${STEP} (exit code ${rc}). Paste the lines below to the product manager (never the CSV or anything from .env):" >&2
        if [ -n "$LOG" ] && [ -s "$LOG" ]; then
            echo "----- last 20 lines of ${LOG} -----" >&2
            tail -n 20 "$LOG" >&2
            echo "-----" >&2
            echo "(Before pasting, check these lines hold no password; a database error can name the host and user.)" >&2
        fi
    fi
    if [ -n "$CSV" ]; then
        if [ -e "$CSV" ]; then
            echo "WARNING: could not delete ${CSV}; delete it by hand: rm -f \"${CSV}\"" >&2
        elif [ "$rc" -ne 0 ] && [ "$had_csv" -eq 1 ]; then
            echo "(${CSV} is deleted.)" >&2
        fi
    fi
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# new_log NAME: set LOG to $ML/logs/NAME.log, created empty and private (0600)
# whatever the umask; tee and > keep the mode of an existing file.
new_log() {
    LOG="$ML/logs/$1.log"
    rm -f "$LOG"
    (umask 077; : > "$LOG")
}

# run_logged NAME CMD...: run CMD, show its output and keep it in $ML/logs/NAME.log
run_logged() {
    local name="$1"; shift
    new_log "$name"
    "$@" 2>&1 | tee "$LOG"
}

# ---------------------------------------------------------------------------
# step 0: paths
usage_err() {  # wrong invocation: one message, no "FAILED at step" block
    DIED=1
    echo "run_training.sh: $*" >&2
    exit 2
}
USAGE_LINE='Usage: see "Quick way: one script" in ml/RUNBOOK.md, or -h'
[ -n "${REPO:-}" ] || usage_err "REPO is not set. ${USAGE_LINE}"
[ -e "$REPO/.git" ] || usage_err "REPO=$REPO is not a git checkout (no .git). Point REPO at the deployed checkout of kfukue/scout-analytics."
REPO="$(cd "$REPO" && pwd -P)"

# ML: checked before anything is created, so a wrong value never writes into
# the deployed checkout (e.g. ML="~/scout-ml" in quotes, run from $REPO).
ML="${ML:-$HOME/scout-ml}"
case "$ML" in
    "~"*) usage_err "ML=$ML starts with ~ (the ~ was quoted, so the shell did not expand it). Write ML=~/scout-ml without quotes, or ML=\"\$HOME/scout-ml\"." ;;
    /*) ;;
    *) usage_err "ML=$ML is not an absolute path. Use e.g. ML=~/scout-ml (no quotes) or ML=\"\$HOME/scout-ml\"." ;;
esac
case "/$ML/" in
    */../*|*/./*) usage_err "ML=$ML contains . or .. components; give a plain absolute path such as ML=~/scout-ml" ;;
esac
ML="${ML%"${ML##*[!/]}"}"   # strip all trailing slashes (ML=// becomes empty)
[ -n "$ML" ] || usage_err "ML must not be /"

# physical_path PATH: PATH with symlinks resolved, also when its last parts do
# not exist yet (resolves the nearest existing ancestor). PATH is absolute and
# has no . or .. components.
physical_path() {
    local p="$1" rest=""
    while [ ! -d "$p" ]; do
        rest="/${p##*/}${rest}"
        p="${p%/*}"
        [ -n "$p" ] || p="/"
    done
    p="$(cd "$p" && pwd -P)"
    p="${p%/}"
    echo "${p}${rest}"
}
ML="$(physical_path "$ML")"
case "$ML" in
    ""|/) usage_err "ML must not be / (it resolves to the root directory)" ;;
esac
case "$ML/" in
    "$REPO/"*) usage_err "ML=$ML is inside REPO=$REPO; the CSV must never land in the checkout. Use ML=~/scout-ml (outside the repo)." ;;
esac
# step 1 checks out origin/main in $ML/src: REPO must never be $ML/src, under it,
# or reached through it (e.g. $ML/src a symlink to REPO). REPO anywhere under
# $ML is refused too ($ML/data would put the CSV in the checkout).
SRC_PHYS="$(physical_path "$ML/src")"
case "$REPO/" in
    "$SRC_PHYS/"*|"$ML/"*) usage_err "REPO=$REPO is inside ML=$ML (or reached through $ML/src); step 1 checks out origin/main in $ML/src, which must never be the deployed checkout. Point REPO at the deployed checkout and ML at a separate directory such as ML=~/scout-ml." ;;
esac
if [ -e "$ML/src" ] && [ "$ML/src" -ef "$REPO" ]; then
    usage_err "$ML/src is the same directory as REPO=$REPO; step 1 would check out origin/main in the deployed checkout. Remove that link: rm \"$ML/src\" (not rm -r)."
fi

# refuse to run from the worktree that step 1 rewrites
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd -P || true)"
case "${SELF_DIR:-}/" in
    "$ML/src/"*) usage_err "this script runs from $SELF_DIR, inside $ML/src, which step 1 rewrites while it runs. Run a copy instead (ml/RUNBOOK.md, \"Quick way: one script\")." ;;
esac

mkdir -p "$ML"
mkdir -p "$ML/logs"
chmod 700 "$ML/logs"   # logs are created before umask 077; keep them private
CSV="$ML/data/calls.csv"
VENV_PY="$ML/venv/bin/python"

if [ -n "$BINARY" ]; then
    case "$BINARY" in
        /*) ;;
        *) BINARY="$(pwd)/$BINARY" ;;
    esac
    [ -f "$BINARY" ] && [ -x "$BINARY" ] || usage_err "--binary $BINARY is not an executable file"
fi

echo "REPO=$REPO"
echo "ML=$ML"

# a CSV left behind by a run that was killed with SIGKILL
rm -f "$CSV"

# ---------------------------------------------------------------------------
STEP="1 (get the ml/ code)"; LOG=""
echo
echo "== Step 1: ml/ code from origin/main in $ML/src"
NOT_OURS_HINT="see Cleanup in ml/RUNBOOK.md; if it is not a worktree of REPO, move it away: mv \"$ML/src\" \"$ML/src.old\""
git_common_dir() {  # physical path of the shared .git directory of checkout $1
    (cd "$1" && cd "$(git rev-parse --git-common-dir)" && pwd -P)
}
run_logged step1-fetch git -C "$REPO" fetch origin main
if [ -e "$ML/src/.git" ]; then
    # never check out in a clone of some other repository
    src_common="$(git_common_dir "$ML/src" 2>/dev/null || true)"
    repo_common="$(git_common_dir "$REPO")" || die "cannot find the git dir of $REPO"
    # -ef: same directory (device and inode), however the two paths are spelled
    [ -n "$src_common" ] && [ "$src_common" -ef "$repo_common" ] \
        || die "$ML/src is not a worktree of $REPO; ${NOT_OURS_HINT}"
    # the common dir is the same for REPO itself, so also require that $ML/src is
    # not REPO and is a linked worktree (.git is a file pointing at its own git
    # dir under .git/worktrees/, not the common dir); never check out in REPO
    [ ! "$ML/src" -ef "$REPO" ] \
        || die "$ML/src is the deployed checkout $REPO itself; refusing to check out origin/main there"
    [ -f "$ML/src/.git" ] \
        || die "$ML/src is not a linked worktree of $REPO (its .git is not a file); ${NOT_OURS_HINT}"
    src_gitdir="$(cd "$ML/src" && cd "$(git rev-parse --absolute-git-dir)" && pwd -P)" \
        || die "cannot find the git dir of $ML/src; ${NOT_OURS_HINT}"
    [ ! "$src_gitdir" -ef "$repo_common" ] \
        || die "$ML/src uses the git dir of the deployed checkout $REPO, not a linked worktree; refusing to check out origin/main there"
    run_logged step1-worktree git -C "$ML/src" checkout --detach origin/main
elif [ -e "$ML/src" ]; then
    die "$ML/src exists but is not a git worktree; ${NOT_OURS_HINT}"
else
    # LC_ALL=C: git's messages in English, so the grep below works in any locale
    if ! run_logged step1-worktree env LC_ALL=C git -C "$REPO" worktree add --detach "$ML/src" origin/main; then
        if grep -q 'already registered' "$LOG"; then
            DIED=1
            echo >&2
            echo "FAILED at step ${STEP}: git still lists $ML/src as a worktree of $REPO, but the directory is gone" >&2
            echo "(deleted by hand). Remove only that entry, as in Cleanup, then run again:" >&2
            echo "    git -C \"$REPO\" worktree remove --force \"$ML/src\"" >&2
            echo "See Cleanup in ml/RUNBOOK.md. (Ignore git's own hint above.)" >&2
            exit 1
        fi
        exit 1   # on_exit shows the log
    fi
fi
src_head="$(git -C "$ML/src" rev-parse HEAD)"
want_head="$(git -C "$REPO" rev-parse origin/main)"
[ "$src_head" = "$want_head" ] || die "$ML/src is not a worktree of $REPO (HEAD $src_head, origin/main $want_head); ${NOT_OURS_HINT}"
if grep -q '^MIN_MATURED' "$ML/src/ml/scout_ml/config.py" 2>/dev/null; then
    echo "ml/ is up to date ($(git -C "$ML/src" rev-parse --short HEAD))"
else
    die "OLD ml/: $ML/src/ml/scout_ml/config.py has no MIN_MATURED; the fixes of 9 October (long skip, forward split for medium/long) are not on origin/main yet"
fi

# ---------------------------------------------------------------------------
STEP="2 (Python venv and tests)"; LOG=""
echo
echo "== Step 2: Python venv in $ML/venv"
APT_HINT="Debian/Ubuntu hint (needs sudo, run it yourself): sudo apt-get install -y python3-venv libgomp1"
py_ok() {  # py_ok PYTHON: exit 0 if it is 3.11 or newer
    "$1" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)'
}
if [ ! -x "$VENV_PY" ]; then
    command -v python3 >/dev/null 2>&1 || die "python3 not found; Python 3.11 or newer is needed"
    python3 --version
    py_ok python3 || die "python3 is $(python3 --version 2>&1), 3.11 or newer is needed (not tested on 3.10)"
    LOG=""
    if ! python3 -m venv "$ML/venv"; then
        echo "$APT_HINT" >&2
        die "python3 -m venv failed (see the apt-get hint above)"
    fi
fi
"$VENV_PY" --version
py_ok "$VENV_PY" || die "$VENV_PY is $("$VENV_PY" --version 2>&1), 3.11 or newer is needed; remove $ML/venv and run with a newer python3"
if [ "$SKIP_INSTALL" -eq 1 ]; then
    echo "--skip-install: not running pip install"
else
    run_logged step2-pip-upgrade "$VENV_PY" -m pip install -U pip
    run_logged step2-pip-install "$VENV_PY" -m pip install -r "$ML/src/ml/requirements.txt"
fi
new_log step2-import
if ! "$VENV_PY" -c 'import lightgbm' > "$LOG" 2>&1; then
    tail -n 20 "$LOG" >&2
    if grep -q 'libgomp' "$LOG"; then
        echo "$APT_HINT" >&2
        die "LightGBM cannot load the OpenMP runtime (libgomp); see the apt-get hint above"
    fi
    die "LightGBM does not import from $ML/venv (lines above); run without --skip-install"
fi
cd "$ML/src/ml"
echo "running the test suite (synthetic data only)"
run_logged step2-pytest "$VENV_PY" -m pytest tests -q -p no:cacheprovider

# ---------------------------------------------------------------------------
STEP="9 (scores stay off)"; LOG=""
echo
echo "== Step 9 check: SCOUT_MODEL_URL in $REPO/.env"
[ -f "$REPO/.env" ] || die "$REPO/.env not found; the export must run where the listener's .env is"
# count lines that set it to a non-empty value (also with "export "); the value is never shown
grep_rc=0
n_url="$(grep -cE '^[[:space:]]*(export[[:space:]]+)?SCOUT_MODEL_URL[[:space:]]*=[[:space:]]*[^[:space:]#]' "$REPO/.env" 2>/dev/null)" || grep_rc=$?
if [ "$grep_rc" -ge 2 ] || [ -z "$n_url" ]; then
    echo "WARNING: $REPO/.env could not be read, so SCOUT_MODEL_URL could not be checked. Check it yourself (RUNBOOK.md step 9)." >&2
elif [ "$n_url" = "0" ]; then
    echo "OK: SCOUT_MODEL_URL is not set in .env (scores stay off)"
else
    echo "WARNING: .env has ${n_url} line(s) setting SCOUT_MODEL_URL (value not shown). Scores must stay off until the product manager" >&2
    echo "         says the gates passed (RUNBOOK.md step 9): tell the product manager. Training continues; it does not use it." >&2
fi
if [ -n "${SCOUT_MODEL_URL:-}" ]; then
    echo "WARNING: SCOUT_MODEL_URL is set in this shell's environment (value not shown). A listener started from this" >&2
    echo "         shell would use it; scores must stay off until the product manager says the gates passed (RUNBOOK.md step 9)." >&2
fi

# ---------------------------------------------------------------------------
STEP="4 (export the dataset)"; LOG=""
echo
echo "== Step 4: export the dataset to $CSV"
umask 077   # from here on (CSV, models, report) files are private
mkdir -p "$ML/data"
cd "$REPO"
if [ -n "$BINARY" ]; then
    run_logged step4-export env SCOUT_DB_AUTO_MIGRATE=false "$BINARY" -export-dataset "$CSV"
else
    command -v go >/dev/null 2>&1 || die "go not found on PATH; install Go or use --binary PATH"
    run_logged step4-export env SCOUT_DB_AUTO_MIGRATE=false go run . -export-dataset "$CSV"
fi
[ -s "$CSV" ] || die "the export finished but $CSV is missing or empty"
echo "CSV has $(wc -l < "$CSV" | tr -d ' ') lines (header included; not shown)"

# ---------------------------------------------------------------------------
STEP="5 (train)"; LOG=""
echo
echo "== Step 5: train"
cd "$ML/src/ml"
TIMEFORMAT='train time: real %1Rs  user %1Us  sys %1Ss'
t0=$(date +%s)
time run_logged step5-train "$VENV_PY" train.py --csv "$CSV" --out "$ML/models"
t1=$(date +%s)
TRAIN_SECONDS=$((t1 - t0))

# ---------------------------------------------------------------------------
STEP="6 (delete the CSV)"; LOG=""
echo
echo "== Step 6: delete the CSV"
rm -f "$CSV"
[ ! -e "$CSV" ] || die "could not delete $CSV; delete it by hand"
echo "deleted $CSV"

# ---------------------------------------------------------------------------
STEP="report"; LOG=""
LATEST="$ML/models/LATEST"
[ -s "$LATEST" ] || die "$LATEST is missing; train.py did not write a model version"
latest_mtime="$(stat -c %Y "$LATEST")"
[ "$latest_mtime" -ge "$t0" ] || die "$LATEST was not written by this run (it is older than the start of step 5), so train.py did not save a new model version; do not use the old report. See $ML/logs/step5-train.log"
VERSION="$(cat "$LATEST")"
REPORT="$ML/models/$VERSION/report.md"
[ -f "$REPORT" ] || die "$REPORT is missing"

echo
echo "== Done"
echo "Report:     $REPORT"
echo "Train time: ${TRAIN_SECONDS}s (real)"
echo
echo "Next:"
echo "  - read it:  cat \"$REPORT\""
echo "  - paste the whole report.md to the product manager, with the train time"
echo "    (and the step 3 query results if you ran them; RUNBOOK.md step 3)."
echo "  - do NOT set SCOUT_MODEL_URL and do NOT start serve.py until the product"
echo "    manager says the gates passed. A FAIL verdict is a normal outcome."
echo "  - the worktree $ML/src and the venv stay for the next weekly retrain."
echo "    To remove the worktree (RUNBOOK.md, Cleanup):"
echo "      git -C \"$REPO\" worktree remove --force \"$ML/src\""
