"""Holder pilot analysis (P1, report only; no model): the -export-holders files
joined on call_id to the owner's Q6 pilot ids, row classes, per-feature AUC with
bootstrap intervals, within-week and within-strata checks, a permutation null
and the pre-registered stop rule, plus a report-only check of what the
HOLD_INCR_FEATURES add beyond the posted holders and age, with its forward
(time-based) check and the owner's pre-registered P2 rule (never in the verdict).
Pure functions except load_*, check_out_dir
and run; holders_report.py is the command line. The output holds counts and
metrics only, never an address or a per-call row, so it is safe to paste back."""
import hashlib
import json
import os
import re
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd
from sklearn.linear_model import LogisticRegression
from sklearn.metrics import roc_auc_score
from sklearn.model_selection import StratifiedKFold
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

from . import config as C
from .features import assert_no_leakage, num
from .labels import net_pct

ROWS_FILE, COLUMNS_FILE, RUNS_FILE = "holders.jsonl", "columns.json", "runs.jsonl"
PRE, POST = "pre_hold_", "post24h_"
# The Q6 ids file. None of its columns is ever an input (kept local to this analysis):
# the strata columns are the baseline, the outcomes and taxes description only.
IDS_REQUIRED = ("call_id", "grp", "week_utc", "entry_block")
IDS_STRATA = ("holders_at_call", "age_s_at_call")
IDS_OUTCOMES = ("max_gain_late_7d", "ret_late_7d")
IDS_TAX = ("tax_buy_pct", "tax_sell_pct")
IDS_COLUMNS = (IDS_REQUIRED + ("rn_week", "rn", "ca", "kind", "entry_at", "pool_address", "pool_id")
               + IDS_STRATA + IDS_OUTCOMES + IDS_TAX)
# (name, positive group, negative group; None = every other group)
PRIMARY = (("dead7 vs rest", "dead7", None), ("held2x7 vs rest", "held2x7", None))
CONTRASTS = PRIMARY + (("dead7 vs held2x7", "dead7", "held2x7"),)
# export columns this analysis reads besides HOLD_FEATURES and post24h_*
EXPORT_REQUIRED = ("call_id", "status", "skip_reason", "entry_block", "venues_pre", "transfers_pre",
                   "code_version", "pre_hold_incomplete")
CLASS_ORDER = ("no row", "skipped", "capped", "incomplete", "entry-block mismatch", "analysable")
INCR_KEY, INCR_HEAD = "beyond_baseline", "## Beyond posted holders and age (report only)"
NO_ADDITION = "no addition shown beyond posted holders and age"
DAGGER = chr(0x2020)   # the dagger sign
DAGGER_NOTE = ("the coefficient's sign (given the baseline) disagrees with the feature's pooled direction on the same "
               "rows, which orients the strata AUCs below.")
INCR_SETTINGS = ("HOLD_INCR_FEATURES", "HOLD_INCR_FOLDS", "HOLD_INCR_REPEATS", "HOLD_INCR_SEED",
                 "HOLD_INCR_LOGIT_C", "HOLD_INCR_DECILES", "HOLD_INCR_ADDS_DELTA_LOWER",
                 "HOLD_INCR_ADDS_STRATA_LOWER")
FWD_KEY, P2_KEY = "forward", "p2_rule"
FWD_HEAD, P2_HEAD = "### Forward (time-based) check", "### P2 rule (pre-registered 23 Oct 2026)"
FWD_SETTINGS = ("HOLD_FWD_TEST_SHARE", "HOLD_FWD_EMBARGO_S", "HOLD_FWD_MIN_TEST_CLASS_N", "HOLD_P2_FEATURE",
                "HOLD_P2_CONTRAST", "HOLD_P2_DELTA_LOWER", "HOLD_P2_STRATA_LOWER", "HOLD_P2_FWD_DELTA_LOWER",
                "HOLD_P2_DIRECTION")
TRAIN, TEST, EMBARGOED = "train", "test", "embargoed"
# a timestamp with a time of day and an explicit UTC offset: Z, +HH, +HHMM or +HH:MM (pgAdmin: -07)
STAMP = re.compile(r"\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}(:?\d{2})?)$")
PASS, FAIL, NA, NOT_JUDGED = "PASS", "FAIL", "n/a", "NOT JUDGED"
# (part, what it is, threshold setting)
P2_PARTS = (("a", "delta AUC 95 % lower bound (random folds, the table above)", "HOLD_P2_DELTA_LOWER"),
            ("b", "decile-strata AUC 95 % lower bound", "HOLD_P2_STRATA_LOWER"),
            ("c", "forward delta AUC 95 % lower bound", "HOLD_P2_FWD_DELTA_LOWER"))
# part (d), the owner's addition of 24 Oct 2026: no threshold, a direction and a coefficient sign
P2_DIRECTION = ("d", "direction (owner, 24 Oct 2026): pooled direction and standardised coefficient given the baseline")
EXIT_OK, EXIT_REFUSED = 0, 1


class Refused(Exception):
    """The inputs cannot be analysed; nothing is written."""


# --- inputs -------------------------------------------------------------------
def check_inputs(features) -> list:
    """The model inputs: pre_hold_* names only, no ids-file column, past the leakage guard."""
    feats = list(features)
    bad = [f for f in feats if not str(f).startswith(PRE) or f in IDS_COLUMNS]
    if bad:
        raise Refused(f"inputs must be pre_hold_* export columns only, not {bad}")
    try:
        assert_no_leakage(feats)
    except ValueError as e:
        raise Refused(str(e)) from None
    return feats


def incr_features() -> list:
    """The features of the beyond-baseline check: HOLD_INCR_FEATURES, a non-empty subset of HOLD_FEATURES."""
    feats = check_inputs(C.HOLD_INCR_FEATURES)
    bad = [f for f in feats if f not in C.HOLD_FEATURES]
    if bad or not feats or len(set(feats)) != len(feats):
        raise Refused(f"HOLD_INCR_FEATURES must be distinct HOLD_FEATURES names, not {feats}")
    return feats


def read_jsonl(path):
    """(records of every complete line, whether a partial last line was ignored),
    like Go's readHolderRows."""
    data = Path(path).read_bytes()
    end = data.rfind(b"\n") + 1
    out = []
    for i, line in enumerate(data[:end].split(b"\n"), 1):
        if line.strip():
            try:
                out.append(json.loads(line))
            except ValueError:
                raise Refused(f"{Path(path).name} line {i} is not JSON") from None
    return out, bool(data[end:].strip())


def load_export(folder) -> dict:
    d = Path(folder)
    for f in (ROWS_FILE, COLUMNS_FILE):
        if not (d / f).is_file():
            raise Refused(f"export file missing: {f} in --holders")
    rows, partial = read_jsonl(d / ROWS_FILE)
    try:
        cols = json.loads((d / COLUMNS_FILE).read_text(encoding="utf-8"))
    except ValueError:
        raise Refused(f"{COLUMNS_FILE} is not JSON") from None
    if not isinstance(cols, dict):
        raise Refused(f"{COLUMNS_FILE} is not a JSON object")
    runs = read_jsonl(d / RUNS_FILE)[0] if (d / RUNS_FILE).is_file() else None
    files = [d / f for f in (ROWS_FILE, COLUMNS_FILE, RUNS_FILE) if (d / f).is_file()]
    return {"rows": rows, "columns": cols.get("columns"), "pre_hold_columns": cols.get("pre_hold_columns"),
            "runs": runs, "partial_line": partial, "sha256": {f.name: sha256(f) for f in files}}


def sha256(path) -> str:
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def check_export(ex: dict, features) -> None:
    cols, pre = ex["columns"], ex["pre_hold_columns"]
    if not isinstance(cols, list) or not isinstance(pre, list):
        raise Refused(f"{COLUMNS_FILE} needs the lists 'columns' and 'pre_hold_columns'")
    if pre != [c for c in cols if c.startswith(PRE)]:
        raise Refused(f"{COLUMNS_FILE}: pre_hold_columns is not the pre_hold_* names of columns")
    unknown = [c for c in pre if c not in C.HOLD_FEATURES and c not in C.HOLD_EXCLUDED]
    if unknown:
        raise Refused(f"pre_hold_* column(s) neither in HOLD_FEATURES nor HOLD_EXCLUDED: {unknown}")
    missing = [f for f in features if f not in cols]
    if missing:
        raise Refused(f"HOLD_FEATURES name(s) missing from the export: {missing}")
    missing = [c for c in EXPORT_REQUIRED if c not in cols]
    if missing:
        raise Refused(f"export column(s) missing: {missing}")
    want = set(cols)
    for i, r in enumerate(ex["rows"], 1):
        got = set(r) if isinstance(r, dict) else set()
        if got != want:
            raise Refused(f"{ROWS_FILE} line {i}: keys differ from {COLUMNS_FILE} columns "
                          f"(missing {sorted(want - got)}, extra {sorted(got - want)})")
        if not isinstance(r["call_id"], int) or isinstance(r["call_id"], bool):
            raise Refused(f"{ROWS_FILE} line {i}: call_id is not a whole number")
    ids = [r["call_id"] for r in ex["rows"]]
    if len(ids) != len(set(ids)):
        raise Refused(f"{len(ids) - len(set(ids))} duplicate call_id value(s) in {ROWS_FILE}")


def plain_groups(grp: pd.Series) -> pd.Series:
    """The Q6 sort-prefixed labels ('1 dead7', ...) as the plain group; any other value unchanged."""
    return grp.map(lambda g: C.HOLD_GROUP_LABELS.get(g, g))


def load_ids(path) -> pd.DataFrame:
    p = Path(path)
    if not p.is_file():
        raise Refused("ids file missing (--ids)")
    try:
        ids = pd.read_csv(p, dtype=str, keep_default_na=False, encoding="utf-8-sig")
    except pd.errors.EmptyDataError:
        raise Refused("ids file is empty") from None
    ids.columns = [str(c).strip().lower() for c in ids.columns]
    missing = [c for c in IDS_REQUIRED if c not in ids.columns]
    if missing:
        raise Refused(f"ids file lacks column(s) {missing}")
    ids = ids.apply(lambda s: s.str.strip())
    ids["grp"] = plain_groups(ids["grp"])
    bad = int((~ids["grp"].isin(C.HOLD_GROUPS)).sum())
    if bad:
        raise Refused(f"{bad} ids row(s) with grp outside {list(C.HOLD_GROUPS) + list(C.HOLD_GROUP_LABELS)}")
    cid = pd.to_numeric(ids["call_id"], errors="coerce")
    bad = cid.isna() | (cid % 1 != 0)
    if bad.any():
        raise Refused(f"{int(bad.sum())} ids row(s) whose call_id is not a whole number")
    ids["call_id"] = cid.astype("int64")
    if ids["call_id"].duplicated().any():
        raise Refused(f"{int(ids['call_id'].duplicated().sum())} duplicate call_id value(s) in the ids file")
    if "ca" in ids.columns:
        ca = ids["ca"].str.lower()
        if ca[ca != ""].duplicated().any():
            raise Refused(f"{int(ca[ca != ''].duplicated().sum())} duplicate ca value(s) in the ids file")
    return ids


# --- rows ---------------------------------------------------------------------
def join(ex: dict, ids: pd.DataFrame) -> pd.DataFrame:
    """The ids rows with their export row (raw JSON values; ids' entry_block/entry_at get _ids)."""
    rows = pd.DataFrame(ex["rows"], columns=ex["columns"], dtype=object)
    stray = set(rows["call_id"]) - set(ids["call_id"])
    if stray:
        raise Refused(f"{len(stray)} export call id(s) not in the ids file")
    if not len(rows):
        raise Refused("no overlap: no export row for any id in the ids file")
    rows["call_id"] = rows["call_id"].astype("int64")
    rows["_in_export"] = True
    df = ids.merge(rows, on="call_id", how="left", suffixes=("_ids", ""))
    df["_in_export"] = df["_in_export"].eq(True)
    df["row_class"] = [row_class(r) for r in df.to_dict("records")]
    return df


def row_class(r: dict) -> str:
    if not r["_in_export"]:
        return "no row"
    if r["status"] == "skipped":
        return f"skipped: {r['skip_reason'] or 'no reason'}"
    if r["status"] != "ok":
        return str(r["status"])
    if r["pre_hold_incomplete"] is True:
        return "incomplete"
    ids_block = pd.to_numeric(r["entry_block_ids"], errors="coerce")
    if r["entry_block"] is None or not ids_block == float(r["entry_block"]):
        return "entry-block mismatch"
    return "analysable"


def class_key(k: str):
    head = k.split(":")[0]
    return (CLASS_ORDER.index(head) if head in CLASS_ORDER else len(CLASS_ORDER) - 1.5, k)


def is_null(v) -> bool:
    return v is None or (isinstance(v, float) and np.isnan(v))


def empty_list(v) -> bool:
    return is_null(v) or (isinstance(v, list) and not v)


def values(s: pd.Series, strict: bool = True) -> pd.Series:
    """Raw JSON values as float: booleans 1/0, NULL NaN (never 0)."""
    def one(v):
        if v is None:
            return np.nan
        if isinstance(v, (bool, int, float, np.bool_, np.integer, np.floating)):
            return float(v)
        if strict:
            raise Refused(f"{s.name}: a value that is not a number, boolean or null")
        return np.nan
    return pd.Series([one(v) for v in s], index=s.index, dtype=float, name=s.name)


def feature_matrix(df: pd.DataFrame, features) -> pd.DataFrame:
    X = pd.DataFrame({f: values(df[f]) for f in check_inputs(features)}, index=df.index)
    if list(X.columns) != list(C.HOLD_FEATURES):
        raise Refused("the inputs must be exactly config.HOLD_FEATURES")
    return X


def stratum_labels(df: pd.DataFrame):
    """Posted-holder terciles (cut points from these rows) x age band; None without both."""
    h, a = num(df, "holders_at_call"), num(df, "age_s_at_call")
    if h.notna().sum() < 3 or a.notna().sum() == 0:
        return None, None
    q1, q2 = (float(q) for q in np.quantile(h.dropna(), [1 / 3, 2 / 3]))
    tier = np.select([h <= q1, h <= q2, h > q2], ["T1", "T2", "T3"], "")
    band = np.select([a < C.HOLD_AGE_BAND_S, a >= C.HOLD_AGE_BAND_S], ["< 1 h", ">= 1 h"], "")
    lab = [f"{t} x age {b}" if t and b else None for t, b in zip(tier, band)]
    return pd.Series(lab, index=df.index, dtype=object), [q1, q2]


# --- AUC ------------------------------------------------------------------------
def pair_auc(x, y, strata=None, w=None) -> np.ndarray:
    """AUC of x for y over the (positive, negative) pairs in the same stratum (strata
    None: all pairs; a None/NaN stratum makes no pair), ties counting 1/2. `w`:
    (reps, n) row weights (bootstrap counts); default one each. One value per weight
    row, NaN without a pair."""
    x, y = np.asarray(x, float), np.asarray(y, bool)
    xp, xn = x[y][:, None], x[~y][None, :]
    k = (xp > xn) + 0.5 * (xp == xn)
    if strata is None:
        same = np.ones_like(k)
    else:
        code = pd.factorize(pd.Series(list(strata), dtype=object))[0]
        cp, cn = code[y][:, None], code[~y][None, :]
        same = ((cp == cn) & (cp >= 0)).astype(float)
    w = np.ones((1, len(x))) if w is None else np.asarray(w, float)
    wp, wn = w[:, y], w[:, ~y]
    top, den = ((wp @ (k * same)) * wn).sum(1), ((wp @ same) * wn).sum(1)
    with np.errstate(invalid="ignore", divide="ignore"):
        return np.where(den > 0, top / den, np.nan)


def boot_weights(groups) -> np.ndarray:
    """Bootstrap counts, (reps, n): each group resampled with replacement within itself."""
    g, rng = np.asarray(groups), np.random.default_rng(C.HOLD_BOOTSTRAP_SEED)
    w = np.zeros((C.HOLD_BOOTSTRAP_REPS, len(g)))
    for name in sorted(set(g)):
        idx = np.flatnonzero(g == name)
        w[:, idx] = rng.multinomial(len(idx), np.full(len(idx), 1 / len(idx)), size=len(w))
    return w


def ci(a) -> list:
    a = np.asarray(a, float)
    return [float(v) for v in np.nanpercentile(a, C.HOLD_CI)] if np.isfinite(a).any() else None


def auc_block(x: pd.Series, grp: pd.Series, contrast, week=None, strata=None) -> dict:
    """One feature in one contrast: AUC, oriented AUC (direction fixed on the full
    sample), bootstrap CI, within-week and within-strata AUC (same orientation)."""
    _, pos, neg = contrast
    keep = (x.notna() & grp.isin([pos, neg]) if neg else x.notna()).to_numpy()
    y, xv, g = (grp[keep] == pos).to_numpy(), x[keep].to_numpy(), grp[keep].to_numpy()
    out = {"n_pos": int(y.sum()), "n_neg": int((~y).sum())}
    if min(out["n_pos"], out["n_neg"]) < C.HOLD_MIN_CLASS_N:
        return {**out, "na": f"< {C.HOLD_MIN_CLASS_N} non-NULL rows in a class"}
    auc = float(roc_auc_score(y, xv))
    up = auc >= 0.5
    def orient(a):
        return a if up else 1 - a
    w = boot_weights(g)
    out.update(auc=auc, oriented=orient(auc), direction="higher" if up else "lower",
               ci=ci(orient(pair_auc(xv, y, None, w))))
    if week is not None:
        out["within_week"] = float(orient(pair_auc(xv, y, week[keep].to_numpy())[0]))
    if strata is not None:
        s = strata[keep].to_numpy()
        has = pd.notna(s)
        if min(y[has].sum(), (~y[has]).sum()) < C.HOLD_MIN_CLASS_N:
            out["strata_na"] = f"< {C.HOLD_MIN_CLASS_N} rows with a stratum in a class"
        else:
            out["within_strata"] = float(orient(pair_auc(xv, y, s)[0]))
            out["strata_ci"] = ci(orient(pair_auc(xv, y, s, w)))
    return out


def perm_null(X: pd.DataFrame, grp: pd.Series, week: pd.Series, eligible: dict) -> dict:
    """95th percentile of the best oriented AUC across features when grp is shuffled
    within week (the chance level for the best of N), per primary contrast and overall."""
    reps, n = C.HOLD_PERM_REPS, len(grp)
    rng = np.random.default_rng(C.HOLD_PERM_SEED)
    perms = np.tile(np.arange(n), (reps, 1))
    wk = week.to_numpy()
    for w in sorted(pd.unique(wk), key=str):
        idx = np.flatnonzero(wk == w)
        perms[:, idx] = rng.permuted(np.tile(idx, (reps, 1)), axis=1)
    gp, best = grp.to_numpy()[perms], {}
    for name, pos, _ in PRIMARY:
        b = np.full(reps, np.nan)
        for f in eligible.get(name, []):
            m = X[f].notna().to_numpy()
            r = X[f][m].rank().to_numpy()
            Y = gp[:, m] == pos
            n1 = Y.sum(1)
            with np.errstate(invalid="ignore", divide="ignore"):
                a = (Y @ r - n1 * (n1 + 1) / 2) / (n1 * (m.sum() - n1))
            b = np.fmax(b, np.maximum(a, 1 - a))
        best[name] = b
    best["both"] = np.fmax.reduce(list(best.values()))
    return {k: (float(np.nanpercentile(v, 95)) if np.isfinite(v).any() else None) for k, v in best.items()}


# --- beyond posted holders and age (report only, never in the verdict) ----------------
def log1p_count(v: pd.Series) -> pd.Series:
    """log1p of a count; NaN for NULL and for a negative value (such rows are dropped and counted)."""
    return np.log1p(v.where(v >= 0))


def decile_labels(a: pd.DataFrame):
    """Posted-holder quantile bins (HOLD_INCR_DECILES; tied cut points merge) x age band, cut
    points from the rows with both values; None without enough such rows."""
    h, age = num(a, "holders_at_call"), num(a, "age_s_at_call")
    ok, d = (h.notna() & age.notna()).to_numpy(), C.HOLD_INCR_DECILES
    if ok.sum() < d:
        return None, None
    cuts = np.unique(np.quantile(h[ok], np.arange(1, d) / d))
    bins = np.searchsorted(cuts, h.to_numpy(), side="left")     # h <= cut goes below, as stratum_labels
    band = np.where(age < C.HOLD_AGE_BAND_S, "< 1 h", ">= 1 h")
    lab = [f"D{b + 1} x age {s}" if o else None for b, s, o in zip(bins, band, ok)]
    return pd.Series(lab, index=a.index, dtype=object), [float(c) for c in cuts]


def logit():
    return make_pipeline(StandardScaler(), LogisticRegression(C=C.HOLD_INCR_LOGIT_C, max_iter=1000))


def oof_pair(Zb: np.ndarray, Zm: np.ndarray, y: np.ndarray):
    """(repeats, n) out-of-fold P(positive) of the baseline and of the model: the same
    stratified folds for both in each repeat, scaler and fit on the training part only."""
    sb, sm = np.empty((2, C.HOLD_INCR_REPEATS, len(y)))
    for r in range(C.HOLD_INCR_REPEATS):
        folds = StratifiedKFold(C.HOLD_INCR_FOLDS, shuffle=True, random_state=C.HOLD_INCR_SEED + r)
        for tr, te in folds.split(Zm, y):
            sb[r, te] = logit().fit(Zb[tr], y[tr]).predict_proba(Zb[te])[:, 1]
            sm[r, te] = logit().fit(Zm[tr], y[tr]).predict_proba(Zm[te])[:, 1]
    return sb, sm


def draw_repeats(reps: int, repeats: int) -> np.ndarray:
    """One cross-validation repeat per bootstrap rep, drawn at random (seed HOLD_INCR_SEED; its own
    generator, so the bootstrap resample itself is the same as without the draw)."""
    return np.random.default_rng(C.HOLD_INCR_SEED).integers(0, repeats, reps)


def boot_delta(sb: np.ndarray, sm: np.ndarray, y, w: np.ndarray) -> np.ndarray:
    """Model minus baseline AUC per bootstrap rep (row of w), each rep on the out-of-fold scores
    of one randomly drawn repeat, the same repeat and resample for both (paired; no refit)."""
    pick, out = draw_repeats(len(w), len(sb)), np.empty(len(w))
    for r in np.unique(pick):
        rows = pick == r
        out[rows] = pair_auc(sm[r], y, None, w[rows]) - pair_auc(sb[r], y, None, w[rows])
    return out


def delta_auc(Zb, Zm, y, g) -> dict:
    """Baseline / model AUC of each row's out-of-fold score averaged over the repeats and their
    difference (the point delta); its paired bootstrap CI (within groups, each rep on one drawn
    repeat's scores, so fold-assignment noise is in it; no refit); and the per-repeat difference
    range (fold-assignment noise only)."""
    sb, sm = oof_pair(Zb, Zm, y)
    per_rep = [roc_auc_score(y, m) - roc_auc_score(y, b) for b, m in zip(sb, sm)]
    base, model = float(roc_auc_score(y, sb.mean(0))), float(roc_auc_score(y, sm.mean(0)))
    return {"baseline_auc": base, "model_auc": model, "delta": model - base,
            "delta_ci": ci(boot_delta(sb, sm, y, boot_weights(g))),
            "repeat_delta": [float(min(per_rep)), float(np.mean(per_rep)), float(max(per_rep))]}


def std_coefs(Zm, y, names) -> dict:
    """Standardised coefficients of the model fitted on all its rows (the direction)."""
    coef = logit().fit(Zm, y)[-1].coef_[0]
    return {n: float(c) for n, c in zip(names, coef[-len(names):])}


def residual(f: pd.Series, B: pd.DataFrame) -> pd.Series:
    """f minus its least-squares fit on [1, B], fitted on all the given rows (it never sees the group)."""
    A = np.column_stack([np.ones(len(B)), B.to_numpy()])
    beta = np.linalg.lstsq(A, f.to_numpy(), rcond=None)[0]
    return pd.Series(f.to_numpy() - A @ beta, index=f.index)


def model_rows(B, F, grp, contrast, feats):
    """Rows of the contrast with both baseline values and every feature of the model, and the drop counts."""
    _, pos, neg = contrast
    in_c = grp.isin([pos, neg]) if neg else pd.Series(True, index=grp.index)
    base_ok, feat_ok = B.notna().all(axis=1), F[list(feats)].notna().all(axis=1)
    keep = in_c & base_ok & feat_ok
    y = grp[keep] == pos
    return keep, {"contrast_rows": int(in_c.sum()), "dropped_baseline_missing": int((in_c & ~base_ok).sum()),
                  "dropped_feature_missing": int((in_c & base_ok & ~feat_ok).sum()),
                  "n_pos": int(y.sum()), "n_neg": int((~y).sum())}


def strata_auc(x, grp, contrast, strata) -> dict:
    """Within-strata AUC and its CI from auc_block (orientation fixed on these rows, pooled)."""
    if strata is None:
        return {"na": "no strata"}
    b = auc_block(x, grp, contrast, None, strata)
    if "auc" not in b:
        return {"na": b["na"]}
    if "strata_na" in b:
        return {"na": b["strata_na"]}
    return {"within_strata": b["within_strata"], "strata_ci": b["strata_ci"], "direction": b["direction"]}


def incr_reading(m: dict):
    """The PM's suggested reading of one single-feature model; None for a joint model."""
    if len(m["features"]) != 1:
        return None
    dec = m.get("deciles", {})
    if not m.get("delta_ci") or not dec.get("strata_ci"):
        return "n/a"
    adds = (m["delta_ci"][0] > C.HOLD_INCR_ADDS_DELTA_LOWER
            and dec["strata_ci"][0] > C.HOLD_INCR_ADDS_STRATA_LOWER)
    return "adds" if adds else NO_ADDITION


def pooled_direction(x, y) -> str:
    """The feature's pooled direction on these rows, oriented as auc_block: "higher" if AUC >= 0.5."""
    return "higher" if roc_auc_score(y, x) >= 0.5 else "lower"


def coef_disagrees(m: dict) -> bool:
    """Whether a single-feature model's standardised coefficient has the opposite sign of the
    feature's pooled direction (a zero coefficient never disagrees)."""
    if "direction" not in m:
        return False
    c = m["coef"][m["features"][0]]
    return c < 0 if m["direction"] == "higher" else c > 0


def incr_model(B, F, X, grp, contrast, feats, terciles, deciles) -> dict:
    """One model (baseline + log1p of feats) in one contrast; single features also get their
    pooled direction, the residual and the tercile / decile within-strata checks on the same rows."""
    keep, counts = model_rows(B, F, grp, contrast, feats)
    out = {"features": list(feats), **counts}
    if min(out["n_pos"], out["n_neg"]) < C.HOLD_MIN_CLASS_N:
        return {**out, "na": f"< {C.HOLD_MIN_CLASS_N} rows with the baseline and feature(s) in a class", "reading": "n/a"}
    g = grp[keep]
    y, Zb = (g == contrast[1]).to_numpy(), B[keep].to_numpy()
    Zm = np.column_stack([Zb, F.loc[keep, list(feats)].to_numpy()])
    out.update(delta_auc(Zb, Zm, y, g.to_numpy()), coef=std_coefs(Zm, y, list(feats)))
    if len(feats) == 1:
        f = feats[0]
        out["direction"] = pooled_direction(X.loc[keep, f].to_numpy(), y)
        r = residual(F.loc[keep, f], B[keep])
        out["residual"] = {key: v for key, v in auc_block(r, g, contrast).items() if key not in ("n_pos", "n_neg")}
        for key, strata in (("terciles", terciles), ("deciles", deciles)):
            out[key] = strata_auc(X.loc[keep, f], g, contrast, None if strata is None else strata[keep])
    out["reading"] = incr_reading(out)
    return out


def beyond_baseline(a: pd.DataFrame, X: pd.DataFrame, grp: pd.Series, terciles, entry=None) -> dict:
    """Per primary contrast: each HOLD_INCR_FEATURES model alone, then all together; the forward
    check of each feature alone (entry: the ids file's entry_at per row, None without the column)
    and the P2 rule."""
    feats = incr_features()
    B = pd.DataFrame({c: log1p_count(num(a, c)) for c in IDS_STRATA}, index=a.index)
    F = pd.DataFrame({f: log1p_count(X[f]) for f in feats}, index=X.index)
    deciles, cuts = decile_labels(a)
    sets = [(f,) for f in feats] + ([tuple(feats)] if len(feats) > 1 else [])
    call_id = a["call_id"] if "call_id" in a.columns else pd.Series(a.index, index=a.index)
    out = {"decile_strata": {"cut_points": cuts, "strata": None if deciles is None else int(deciles.nunique()),
                             "rows_without_stratum": len(a) if deciles is None else int(deciles.isna().sum())},
           "contrasts": {c[0]: {" + ".join(s): incr_model(B, F, X, grp, c, s, terciles, deciles) for s in sets}
                         for c in PRIMARY},
           FWD_KEY: forward_check(B, F, grp, feats, call_id, entry)}
    out[P2_KEY] = p2_rule(out)
    return out


def incr_not_judged(reason: str) -> dict:
    """The section without the baseline columns: the forward check and the P2 rule not judged either."""
    out = {"not_judged": reason, FWD_KEY: {"na": reason}}
    out[P2_KEY] = p2_rule(out)
    return out


# --- forward (time-based) check and the P2 rule (report only, never in the verdict) ----
def parse_entry_at(v: pd.Series) -> pd.Series:
    """The ids file's entry_at text as UTC timestamps; NaT for a blank, for a value without a time
    of day and an explicit UTC offset (a time without one is never guessed as UTC), or for one
    that is not a timestamp."""
    s = v.astype(object).where(v.notna(), "").astype(str).str.strip()
    ok = s.map(lambda x: bool(STAMP.search(x)))
    return pd.to_datetime(s.where(ok, ""), utc=True, errors="coerce", format="ISO8601")


def time_split(t: pd.Series, call_id: pd.Series) -> pd.Series:
    """TRAIN / TEST / EMBARGOED per row: ordered by (t, call_id), the latest HOLD_FWD_TEST_SHARE of
    the rows (rounded) are the test; earlier rows posted at or after the first test call minus
    HOLD_FWD_EMBARGO_S are embargoed (a row tied with the first test call too)."""
    order = pd.DataFrame({"t": t, "id": call_id}).sort_values(["t", "id"], kind="mergesort").index
    n_test = int(round(C.HOLD_FWD_TEST_SHARE * len(order)))
    part = pd.Series(TRAIN, index=t.index, dtype=object)
    if n_test == 0:
        return part
    test = order[-n_test:]
    part[test] = TEST
    start = t[test].min() - pd.Timedelta(seconds=C.HOLD_FWD_EMBARGO_S)
    part[part.eq(TRAIN) & (t >= start)] = EMBARGOED
    return part


def date_range(t: pd.Series):
    """[first, last] UTC date of t; None when empty."""
    return [t.min().strftime("%Y-%m-%d"), t.max().strftime("%Y-%m-%d")] if len(t) else None


def split_info(t: pd.Series, part: pd.Series) -> dict:
    """Row counts and UTC date ranges of the three parts, the first test call and the embargo start."""
    test = t[part.eq(TEST)]
    first = None if test.empty else test.min()
    return {"n": len(t), **{f"n_{p}": int(part.eq(p).sum()) for p in (TRAIN, TEST, EMBARGOED)},
            **{f"{p}_entry_at": date_range(t[part.eq(p)]) for p in (TRAIN, TEST, EMBARGOED)},
            "first_test_at": None if first is None else first.strftime("%Y-%m-%d %H:%M:%S"),
            "embargo_from": None if first is None else
            (first - pd.Timedelta(seconds=C.HOLD_FWD_EMBARGO_S)).strftime("%Y-%m-%d %H:%M:%S")}


def forward_check(B, F, grp, feats, call_id, entry) -> dict:
    """One time split of the analysable rows, then per primary contrast each feature alone; n/a
    without entry_at or with any row whose entry_at is blank or unparseable (no row is dropped)."""
    if entry is None:
        return {"na": "the ids file lacks entry_at"}
    t = parse_entry_at(entry)
    bad = int(t.isna().sum())
    if bad:
        return {"na": f"{bad} analysable row(s) with entry_at blank or not a timestamp with a UTC offset"}
    part = time_split(t, call_id)
    return {"split": split_info(t, part),
            "contrasts": {c[0]: {f: forward_model(B, F, grp, c, f, part) for f in feats} for c in PRIMARY}}


def forward_model(B, F, grp, contrast, f, part) -> dict:
    """Baseline and baseline + log1p(f) fitted on the training rows (the scaler too), AUC on the
    test rows, and the paired bootstrap CI of the delta over the test rows (within groups; no refit)."""
    keep, counts = model_rows(B, F, grp, contrast, (f,))
    y = grp == contrast[1]
    tr, te = keep & part.eq(TRAIN), keep & part.eq(TEST)
    out = {"features": [f], **{k: counts[k] for k in ("contrast_rows", "dropped_baseline_missing",
                                                      "dropped_feature_missing")},
           "n_train": int(tr.sum()), "n_test": int(te.sum()), "n_embargoed": int((keep & part.eq(EMBARGOED)).sum()),
           "train_pos": int((tr & y).sum()), "train_neg": int((tr & ~y).sum()),
           "test_pos": int((te & y).sum()), "test_neg": int((te & ~y).sum())}
    if (min(out["train_pos"], out["train_neg"]) < C.HOLD_MIN_CLASS_N
            or min(out["test_pos"], out["test_neg"]) < C.HOLD_FWD_MIN_TEST_CLASS_N):
        return {**out, "na": f"< {C.HOLD_MIN_CLASS_N} training or < {C.HOLD_FWD_MIN_TEST_CLASS_N} test rows in a class"}
    tr, te, yv = tr.to_numpy(), te.to_numpy(), y.to_numpy()
    Zb = B.to_numpy()
    Zm = np.column_stack([Zb, F[f].to_numpy()])
    sb = logit().fit(Zb[tr], yv[tr]).predict_proba(Zb[te])[:, 1]
    sm = logit().fit(Zm[tr], yv[tr]).predict_proba(Zm[te])[:, 1]
    w = boot_weights(grp.to_numpy()[te])
    base, model = float(roc_auc_score(yv[te], sb)), float(roc_auc_score(yv[te], sm))
    return {**out, "baseline_auc": base, "model_auc": model, "delta": model - base,
            "delta_ci": ci(pair_auc(sm, yv[te], None, w) - pair_auc(sb, yv[te], None, w))}


def p2_sources(inc: dict) -> dict:
    """Per part: (the block holding the CI, the CI key, why n/a when the block is missing); for (d)
    the single-feature model block, whose pooled direction also orients (b), with no CI key."""
    f, c = C.HOLD_P2_FEATURE, C.HOLD_P2_CONTRAST
    keys = [k for k, _, _ in P2_PARTS] + [P2_DIRECTION[0]]
    if "not_judged" in inc:
        return {k: (None, None, inc["not_judged"]) for k in keys}
    missing = f"{f} in {c} is not among the models (HOLD_INCR_FEATURES x primary contrasts)"
    m = inc.get("contrasts", {}).get(c, {}).get(f)
    fw = inc.get(FWD_KEY, {})
    fm = fw.get("contrasts", {}).get(c, {}).get(f)
    return {"a": (m, "delta_ci", missing),
            "b": (None if m is None else m.get("deciles", m), "strata_ci", missing),
            "c": (fm, "delta_ci", fw.get("na", missing)),
            "d": (m, None, missing)}


def p2_part(block, key, why_missing, threshold) -> dict:
    """One part: the CI's lower bound against its threshold (strict), or n/a with the reason."""
    if block is None or "na" in block or not block.get(key):
        why = why_missing if block is None else block.get("na", "no confidence interval")
        return {"lower": None, "threshold": threshold, "result": NA, "na": why}
    low = block[key][0]
    out = {"lower": low, "threshold": threshold, "result": PASS if low > threshold else FAIL}
    return {**out, "direction": block["direction"]} if key == "strata_ci" and "direction" in block else out


def p2_coef(block: dict, f: str):
    """The feature's standardised coefficient in the block; None when absent or not finite."""
    c = (block.get("coef") or {}).get(f)
    return float(c) if c is not None and np.isfinite(c) else None


def p2_direction(block, _key, why_missing) -> dict:
    """Part (d), the owner's addition (24 Oct 2026): PASS only if the feature's pooled direction on the
    model's rows is HOLD_P2_DIRECTION and its standardised coefficient given the baseline is > 0;
    n/a when either value is missing."""
    out = {"expected": C.HOLD_P2_DIRECTION}
    if block is None or "na" in block:
        why = why_missing if block is None else block["na"]
        return {**out, "observed": None, "coef": None, "result": NA, "na": why}
    d, c = block.get("direction"), p2_coef(block, C.HOLD_P2_FEATURE)
    out.update(observed=d, coef=c)
    if d is None or c is None:
        return {**out, "result": NA, "na": "no pooled direction" if d is None else "no coefficient"}
    return {**out, "result": PASS if d == C.HOLD_P2_DIRECTION and c > 0 else FAIL}


def p2_rule(inc: dict) -> dict:
    """The owner's pre-registered P2 rule (23 Oct 2026; (d) added by the owner on 24 Oct 2026; result
    logic per the PM, 23 Oct 2026): FAIL if any part fails (a definite failure decides even when another
    part is n/a); else NOT JUDGED if any part is n/a; PASS only if all four pass."""
    src = p2_sources(inc)
    parts = {k: {"what": what, **p2_part(*src[k], getattr(C, th))} for k, what, th in P2_PARTS}
    k, what = P2_DIRECTION
    parts[k] = {"what": what, **p2_direction(*src[k])}
    res = [p["result"] for p in parts.values()]
    overall = FAIL if FAIL in res else NOT_JUDGED if NA in res else PASS
    return {"feature": C.HOLD_P2_FEATURE, "contrast": C.HOLD_P2_CONTRAST, "parts": parts, "result": overall}


# --- description ------------------------------------------------------------------
def stats(v: pd.Series) -> dict:
    d = v.dropna()
    q = np.quantile(d, [0.25, 0.5, 0.75]) if len(d) else [None] * 3
    return {"n": int(len(d)), "missing_share": float(v.isna().mean()) if len(v) else None,
            "median": q[1], "q25": q[0], "q75": q[2], "mean": float(d.mean()) if len(d) else None}


def by_group(frame: pd.DataFrame, grp: pd.Series) -> dict:
    return {c: {g: stats(frame[c][grp == g]) for g in C.HOLD_GROUPS} for c in frame.columns}


def verdict(contrasts: dict, counts: dict, strata_reason) -> dict:
    reasons = [strata_reason] if strata_reason else []
    small = [g for g in C.HOLD_GROUPS if counts.get(g, 0) < C.HOLD_MIN_CLASS_N]
    if small:
        reasons.append(f"fewer than {C.HOLD_MIN_CLASS_N} analysable rows in {small}")
    if reasons:
        return {"verdict": "NOT JUDGED", "reasons": reasons, "above_stop_bound": []}
    above = [{"contrast": c, "feature": f, "ci_low": b["ci"][0],
              "strata_ci_low": (b.get("strata_ci") or [None])[0]}
             for c, _, _ in PRIMARY for f, b in contrasts[c].items()
             if b.get("ci") and b["ci"][0] > C.HOLD_STOP_AUC_LOWER]
    keep = [a for a in above if a["strata_ci_low"] is not None and a["strata_ci_low"] > C.HOLD_STRATA_AUC_LOWER]
    return {"verdict": "CONTINUE" if keep else "STOP", "reasons": [verdict_reason(above, keep)],
            "above_stop_bound": above}


def verdict_reason(above: list, keep: list) -> str:
    """Why CONTINUE / STOP; a STOP counts the pooled pairs whose strata clause could not
    be evaluated (within-strata AUC n/a), so STOP is not read as "no signal" there."""
    if keep:
        return (f"{len(keep)} feature/contrast pair(s) with lower bound > {C.HOLD_STOP_AUC_LOWER:.2f} and "
                f"within-strata lower bound > {C.HOLD_STRATA_AUC_LOWER:.2f}")
    why = (f"no feature in a primary contrast has lower bound > {C.HOLD_STOP_AUC_LOWER:.2f} that holds "
           f"within strata (> {C.HOLD_STRATA_AUC_LOWER:.2f}); {len(above)} above the bound pooled")
    na = sum(a["strata_ci_low"] is None for a in above)
    if na:
        why += (f", {na} of them with within-strata AUC n/a (< {C.HOLD_MIN_CLASS_N} rows with a stratum "
                "in a class), so the strata clause could not be evaluated for them")
    return why


def per_group(mask: pd.Series, grp: pd.Series) -> dict:
    return {g: int((mask & (grp == g)).sum()) for g in C.HOLD_GROUPS}


def analyse(ex: dict, ids: pd.DataFrame, features) -> dict:
    df = join(ex, ids)
    groups = list(C.HOLD_GROUPS)
    classes = {k: {**per_group(df.row_class == k, df.grp), "total": int((df.row_class == k).sum())}
               for k in sorted(set(df.row_class), key=class_key)}
    ok = df[df["status"].eq("ok")]
    flag = ok["venues_pre"].map(empty_list) & (num(ok, "transfers_pre") > 0)
    venue = {g: {"ok_rows": int((ok.grp == g).sum()), "empty_pre_venue_with_transfers": int((flag & (ok.grp == g)).sum()),
                 "of_which_analysable": int((flag & (ok.grp == g) & ok.row_class.eq("analysable")).sum())}
             for g in groups}
    a = df[df.row_class.eq("analysable")].reset_index(drop=True)
    X, grp, week = feature_matrix(a, features), a["grp"], a["week_utc"]
    counts = {g: int((grp == g).sum()) for g in groups}
    missing_strata = [c for c in IDS_STRATA if c not in ids.columns]
    strata, cuts = (None, None) if missing_strata else stratum_labels(a)
    strata_reason = (f"ids file lacks {missing_strata} (an older Q6 export)" if missing_strata else
                     None if strata is not None else "holders_at_call / age_s_at_call have no values")
    contrasts = {c[0]: {f: auc_block(X[f], grp, c, week, strata) for f in X.columns} for c in CONTRASTS}
    baseline = None if strata_reason else {
        c[0]: {col: auc_block(num(a, col), grp, c, week) for col in IDS_STRATA} for c in PRIMARY}
    eligible = {c: [f for f, b in contrasts[c].items() if "auc" in b] for c, _, _ in PRIMARY}
    entry = a["call_id"].map(ids.set_index("call_id")["entry_at"]) if "entry_at" in ids.columns else None
    checks = {}
    for f, col in (("pre_hold_launch_age_s", "age_s_at_call"), ("pre_hold_holders", "holders_at_call")):
        if col in a.columns and f in X.columns:
            pair = pd.DataFrame({"f": X[f], "c": num(a, col)}).dropna()
            checks[f"{f} ~ {col}"] = {"n": len(pair), "pearson": pair.f.corr(pair.c) if len(pair) > 2 else None,
                                      "spearman": pair.f.rank().corr(pair.c.rank()) if len(pair) > 2 else None}
    post_cols = [c for c in ex["columns"] if c.startswith(POST)]
    outcome = pd.DataFrame({c: num(a, c) for c in IDS_OUTCOMES if c in a.columns})
    if len(outcome.columns) and all(t in a.columns for t in IDS_TAX):
        for c in list(outcome.columns):
            outcome[f"{c} net of tax"] = net_pct(outcome[c], num(a, IDS_TAX[0]), num(a, IDS_TAX[1]))
    runs = ex["runs"]
    return {
        "inputs": {"ids_rows": len(ids), "export_rows": len(ex["rows"]), "analysable_rows": len(a),
                   "partial_last_line_ignored": ex["partial_line"],
                   "code_versions": sorted({str(r.get("code_version") or "") for r in ex["rows"]}),
                   "runs": None if runs is None else len(runs),
                   "failed_calls": None if runs is None else sum(int(r.get("failed") or 0) for r in runs),
                   "ids_columns_missing": [c for c in IDS_COLUMNS if c not in ids.columns]},
        "row_classes": classes, "venue_diagnostic": venue,
        "ids_without_row": sorted(int(i) for i in df.loc[df.row_class.eq("no row"), "call_id"]),
        "weeks": {str(w): per_group(week == w, grp) for w in sorted(set(week))},
        "features": by_group(X, grp), "contrasts": contrasts, "baseline": baseline,
        "strata": {"not_judged": strata_reason} if strata_reason else {
            "cut_points": cuts, "rows_without_stratum": int(strata.isna().sum()),
            "counts": {s: per_group(strata == s, grp) for s in sorted(set(strata.dropna()))}},
        "data_checks": checks,
        "multiple_comparisons": {"tests": sum(len(v) for v in eligible.values()),
                                 "best_oriented": {c: max([contrasts[c][f]["oriented"] for f in v], default=None)
                                                   for c, v in eligible.items()},
                                 "perm_p95_best": perm_null(X, grp, week, eligible)},
        "after_call": by_group(pd.DataFrame({c: values(a[c], strict=False) for c in post_cols}), grp),
        "outcomes": by_group(outcome, grp),
        "verdict": verdict(contrasts, counts, strata_reason),
        INCR_KEY: incr_not_judged(strata_reason) if strata_reason else beyond_baseline(a, X, grp, strata, entry),
    }


def settings() -> dict:
    return {k: getattr(C, k) for k in ("HOLD_STOP_AUC_LOWER", "HOLD_STRATA_AUC_LOWER", "HOLD_MIN_CLASS_N",
                                       "HOLD_AGE_BAND_S", "HOLD_BOOTSTRAP_REPS", "HOLD_BOOTSTRAP_SEED",
                                       "HOLD_CI", "HOLD_PERM_REPS", "HOLD_PERM_SEED") + INCR_SETTINGS + FWD_SETTINGS}


# --- report --------------------------------------------------------------------
def f3(v):
    return "n/a" if is_null(v) else f"{v:.3f}"


def fg(v):
    return "n/a" if is_null(v) else f"{v:.4g}"


def pct(v):
    return "n/a" if v is None else f"{100 * v:.1f} %"


def fci(c):
    return "n/a" if not c else f"[{c[0]:.3f}, {c[1]:.3f}]"


def table(head, rows) -> list:
    return (["| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
            + ["| " + " | ".join(str(x) for x in r) + " |" for r in rows] + [""])


def group_table(block: dict) -> list:
    head = ["column"] + [f"{g} {k}" for g in C.HOLD_GROUPS for k in ("n (missing)", "median [IQR]")]
    rows = [[c] + [x for g in C.HOLD_GROUPS for x in (
        f"{s[g]['n']} ({pct(s[g]['missing_share'])})",
        f"{fg(s[g]['median'])} [{fg(s[g]['q25'])}, {fg(s[g]['q75'])}]")] for c, s in block.items()]
    return table(head, rows) if rows else ["(none)", ""]


def auc_table(block: dict, strata: bool = True) -> list:
    head = ["feature" if strata else "column", "n pos / neg", "AUC", "oriented", "direction", "95 % CI", "within-week"]
    head += ["within-strata", "strata 95 % CI"] if strata else []
    rows = []
    for f, b in block.items():
        if "auc" not in b:
            rows.append([f, f"{b['n_pos']} / {b['n_neg']}", f"n/a ({b['na']})"] + [""] * (len(head) - 3))
            continue
        r = [f, f"{b['n_pos']} / {b['n_neg']}", f3(b["auc"]), f3(b["oriented"]), b["direction"],
             fci(b["ci"]), f3(b.get("within_week"))]
        rows.append(r + ([f3(b.get("within_strata")) if "strata_na" not in b else f"n/a ({b['strata_na']})",
                          fci(b.get("strata_ci"))] if strata else []))
    return table(head, rows)


def render_report(s: dict) -> str:
    i, st, v = s["inputs"], s["settings"], s["verdict"]
    runs = "absent" if i["runs"] is None else f"{i['runs']} run(s), {i['failed_calls']} failed call(s)"
    L = [f"# Holder pilot analysis (report only), run {s['run']} UTC", "",
         "Report only: no model, nothing saved or served; the owner decides on the result.",
         "Group shares come from the sample, not necessarily the natural base rates: ranking (AUC) "
         "only, never lift or precision.",
         f"The stop bound (bootstrap 95 % lower AUC bound > {st['HOLD_STOP_AUC_LOWER']:.2f}) is pre-registered "
         "(19 Oct 2026). The strata definition (posted-holder terciles from the pilot x age < 1 h / >= 1 h), "
         f"the {st['HOLD_STRATA_AUC_LOWER']:.2f} 'vanishes' threshold read as the lower bound of the within-strata "
         "95 % CI, and the NOT JUDGED conditions (strata columns missing from the ids file, fewer than "
         f"{st['HOLD_MIN_CLASS_N']} analysable rows in any group) are the PM's reading: the verdict is a "
         "recommendation for the owner, not a ruling.", "",
         f"- Inputs: {i['ids_rows']} ids rows, {i['export_rows']} export rows, {i['analysable_rows']} analysable; "
         f"partial last line ignored: {'yes' if i['partial_last_line_ignored'] else 'no'}; runs.jsonl: {runs}.",
         f"- Export code_version values: {', '.join(c or '(empty)' for c in i['code_versions']) or 'none'}.",
         f"- Ids columns missing: {', '.join(i['ids_columns_missing']) or 'none'}.",
         "- Settings: " + ", ".join(f"{k} = {val}" for k, val in st.items()) + ".", "",
         f"## Verdict: {v['verdict']}", ""] + [f"- {r}" for r in v["reasons"]] + [""]
    if v["above_stop_bound"]:
        L += table(["contrast", "feature", "95 % lower bound", "within-strata lower bound"],
                   [[a["contrast"], a["feature"], f3(a["ci_low"]), f3(a["strata_ci_low"])] for a in v["above_stop_bound"]])
    L += ["## Row classes per group", ""] + table(
        ["class"] + list(C.HOLD_GROUPS) + ["total"],
        [[k] + [c[g] for g in C.HOLD_GROUPS] + [c["total"]] for k, c in s["row_classes"].items()])
    L += ["Diagnostic: status ok rows with an empty pre-call venue list but pre-call transfers > 0 (the v2/v3 "
          "pre venue is the pool the tracker picked after the call; venue-dependent features are suspect there).", ""]
    L += table(["group", "ok rows", "empty pre venue, transfers > 0", "of which analysable"],
               [[g, d["ok_rows"], d["empty_pre_venue_with_transfers"], d["of_which_analysable"]]
                for g, d in s["venue_diagnostic"].items()])
    L += ["## Week check (analysable rows per group and week_utc)", ""] + table(
        ["week_utc"] + list(C.HOLD_GROUPS), [[w] + [c[g] for g in C.HOLD_GROUPS] for w, c in s["weeks"].items()])
    L += ["## Features by group (analysable rows; NULL stays NULL, booleans 1/0)", ""] + group_table(s["features"])
    for name, _, _ in CONTRASTS:
        tag = " (primary)" if any(name == p[0] for p in PRIMARY) else " (description only, not in the verdict)"
        L += [f"## AUC: {name}{tag}", "",
              "oriented = max(AUC, 1 - AUC), direction fixed on the full sample; CI: percentile bootstrap "
              "within groups; within-week / within-strata: pairs in the same week / stratum only.", ""]
        L += auc_table(s["contrasts"][name])
    L += ["## Posted holders and age at the call", ""]
    if s["baseline"] is None:
        L += [f"NOT JUDGED: {s['strata']['not_judged']}.", ""]
    else:
        for name, block in s["baseline"].items():
            L += [f"Baseline, {name} (the Q5b reference):", ""] + auc_table(block, strata=False)
        sd = s["strata"]
        L += [f"Strata: posted-holder terciles (cut points {fg(sd['cut_points'][0])}, {fg(sd['cut_points'][1])}) "
              f"x age band; {sd['rows_without_stratum']} analysable row(s) without a stratum.", ""]
        L += table(["stratum"] + list(C.HOLD_GROUPS), [[k] + [c[g] for g in C.HOLD_GROUPS] for k, c in sd["counts"].items()])
    if s["data_checks"]:
        L += ["Data checks (Pearson and Spearman rank correlation, analysable rows with both values):", ""]
        L += table(["pair", "n", "Pearson", "Spearman"],
                   [[k, d["n"], f3(d["pearson"]), f3(d["spearman"])] for k, d in s["data_checks"].items()])
    m = s["multiple_comparisons"]
    L += ["## Multiple comparisons", "",
          f"- {m['tests']} pooled AUC tests in the primary contrasts (features x contrasts with enough rows); "
          "the within-strata AUCs are the same number again.",
          f"- Chance level for the best of N (grp shuffled within week, {st['HOLD_PERM_REPS']} reps, seed "
          f"{st['HOLD_PERM_SEED']}; 95th percentile of the best oriented AUC across features): "
          + "; ".join(f"{k} {f3(val)}" for k, val in m["perm_p95_best"].items()) + ".",
          "- Best observed oriented AUC: " + "; ".join(f"{k} {f3(val)}" for k, val in m["best_oriented"].items()) + ".", ""]
    L += ["## After the call (description only; never in the AUC tables or the verdict)", ""]
    L += group_table(s["after_call"])
    L += ["Peak and 7d return from the ids file (net of tax as labels.net_pct, when both tax columns are there):", ""]
    L += group_table(s["outcomes"])
    if INCR_KEY in s:
        L += render_incremental(s[INCR_KEY], st)
    return "\n".join(L)


def incr_method(st: dict) -> list:
    reps, seed, r, s0 = st["HOLD_BOOTSTRAP_REPS"], st["HOLD_BOOTSTRAP_SEED"], st["HOLD_INCR_REPEATS"], st["HOLD_INCR_SEED"]
    return [
        "Report only, never part of the verdict. Added on 23 Oct 2026 after seeing the pilot (not pre-registered): "
        "a PM reading for the owner, not a ruling. Group shares come from the sample, not necessarily the natural "
        "base rates: ranking (AUC) only.",
        "These are random stratified folds, not the project's time-based validation, so the delta is a within-sample "
        "contrast, not a forward estimate. One call per token (the duplicate-ca refusal of the ids file) holds only "
        "when the ids file has a ca column; without it, calls of one token can sit in different folds.",
        "",
        "- Models: logistic regression (L2, C = "
        f"{st['HOLD_INCR_LOGIT_C']}) on log1p(holders_at_call) + log1p(age_s_at_call) (baseline), and the baseline "
        "+ log1p(feature), each feature alone, then all together. Per model, rows with a baseline value or a feature "
        "NULL or negative are dropped and counted; baseline and model use the same rows.",
        f"- Cross-validation: {st['HOLD_INCR_FOLDS']}-fold stratified on the contrast label, repeated {r} times "
        f"(shuffle seeds {s0} to {s0 + r - 1}); in each repeat baseline and model use the same folds, the "
        "standardisation and the fit are on the training part only. AUC and delta (model minus baseline) are of each "
        "row's out-of-fold score averaged over the repeats.",
        f"- Delta 95 % CI: paired percentile bootstrap ({reps} reps, seed {seed}, resampled within groups); each rep "
        f"uses the out-of-fold scores of one repeat drawn at random (seed {s0}), the same repeat and resample for "
        "baseline and model, so it covers which calls were sampled and the fold assignment. Each rep's delta is one "
        "repeat's delta, so the CI is centred on the repeat-delta mean (shown in the table), not necessarily on the "
        "point delta of the averaged scores. It does not cover the refit (no model is refitted per bootstrap rep), "
        "so it is a floor on the uncertainty. Repeat delta: min / "
        "mean / max over the repeats (fold-assignment noise only, not a CI); a delta whose repeat range spans 0, or "
        "that is no larger than that range, is no evidence either way.",
        "- Coefficient: of the feature, standardised, in the model fitted on all its rows: its direction given the "
        "baseline. The strata AUCs below are oriented by the feature's pooled direction instead, so the two can "
        f"disagree; {DAGGER} marks a single feature whose coefficient sign disagrees with its pooled direction.",
        "- Residual: log1p(feature) minus its least-squares fit on the two baseline columns, fitted on all rows of "
        "the model (it never sees the group, so no label leaks in); its AUC with the bootstrap CI (residuals fixed).",
        f"- Strata: pairs in the same stratum only, orientation of the feature's pooled AUC on the same rows; "
        f"posted-holder terciles x age band (the strata above) and up to {st['HOLD_INCR_DECILES']} posted-holder "
        "quantile bins x age band (tied cut points merge).",
        f"- Reading (PM's suggestion): \"adds\" if the delta AUC 95 % lower bound > "
        f"{st['HOLD_INCR_ADDS_DELTA_LOWER']:.2f} and the decile-strata 95 % lower bound > "
        f"{st['HOLD_INCR_ADDS_STRATA_LOWER']:.2f}; otherwise \"{NO_ADDITION}\". A joint model has no reading.", ""]


def fdelta(m: dict) -> list:
    if "na" in m:
        return [f"n/a ({m['na']})"] + [""] * 5 + [m["reading"]]
    rd = m["repeat_delta"]
    return [f3(m["baseline_auc"]), f3(m["model_auc"]), f3(m["delta"]), fci(m["delta_ci"]),
            f"{f3(rd[0])} / {f3(rd[1])} / {f3(rd[2])}",
            "; ".join(f"{k} {v:+.3f}" for k, v in m["coef"].items()) + (f" {DAGGER}" if coef_disagrees(m) else ""),
            m["reading"] or ""]


def fstrata(b: dict) -> list:
    if "na" in b:
        return [f"n/a ({b['na']})", ""]
    return [f"{f3(b['within_strata'])} ({b['direction']})", fci(b["strata_ci"])]


def render_incremental(inc: dict, st: dict) -> list:
    L = [INCR_HEAD, ""] + incr_method(st)
    if "not_judged" in inc:
        return L + [f"NOT JUDGED: {inc['not_judged']}.", ""] + render_forward(inc, st) + render_p2(inc, st)
    d = inc["decile_strata"]
    cuts = ", ".join(fg(c) for c in d["cut_points"]) if d["cut_points"] else "n/a"
    L += [f"Decile strata: {d['strata'] if d['strata'] is not None else 'n/a'} strata (holder bins x age band), "
          f"holder cut points {cuts}; {d['rows_without_stratum']} analysable row(s) without one.", ""]
    rows, checks = [], []
    for c, models in inc["contrasts"].items():
        for name, m in models.items():
            rows.append([c, name, f"{m['n_pos']} / {m['n_neg']}",
                         f"{m['dropped_baseline_missing']} / {m['dropped_feature_missing']}"] + fdelta(m))
            if "residual" in m:
                res = m["residual"]
                checks.append([c, name, f"{f3(res.get('oriented'))} ({res.get('direction', 'n/a')})", fci(res.get("ci"))]
                              + fstrata(m["terciles"]) + fstrata(m["deciles"]))
    L += table(["contrast", "model (+ baseline)", "n pos / neg", "dropped: baseline / feature", "baseline AUC",
                "model AUC", "delta", "delta 95 % CI", "repeat delta min / mean / max", "coefficient (std.)",
                "reading"], rows)
    if any(coef_disagrees(m) for models in inc["contrasts"].values() for m in models.values()):
        L += [f"{DAGGER} {DAGGER_NOTE}", ""]
    if checks:
        L += ["Partial-rank and strata checks (single features; AUC oriented, direction in brackets):", ""]
        L += table(["contrast", "feature", "residual AUC", "residual 95 % CI", "tercile-strata AUC",
                    "tercile 95 % CI", "decile-strata AUC", "decile 95 % CI"], checks)
    return L + render_forward(inc, st) + render_p2(inc, st)


def forward_method(st: dict) -> list:
    share, emb = st["HOLD_FWD_TEST_SHARE"], st["HOLD_FWD_EMBARGO_S"]
    return [
        "Part (c) of the owner's P2 rule below (pre-registered 23 Oct 2026). Report only, never part of the verdict.",
        "",
        f"- Split: the analysable rows ordered by the ids file's entry_at (ties by call_id); the earliest "
        f"{100 * (1 - share):.0f} % train, the latest {100 * share:.0f} % test (HOLD_FWD_TEST_SHARE, rounded to "
        f"whole rows); training calls posted within {emb / 86400:g} d (HOLD_FWD_EMBARGO_S) before the first test "
        "call, or at the same time, are dropped (embargoed: their 7d label overlaps the test period). One split for "
        "every feature and contrast. Every entry_at must be a timestamp with a UTC offset (Z, or e.g. pgAdmin's "
        "2026-09-09 20:59:16-07); a blank, a time without an offset or an unparseable value makes the whole check "
        "n/a, no row is dropped.",
        "- Models: the same logistic regressions as above (baseline, and baseline + log1p(feature), each feature "
        "alone), standardisation and fit on the training rows only; AUC on the test rows; delta = model minus "
        "baseline. Rows with a baseline value or the feature NULL or negative are dropped and counted as above.",
        f"- Delta 95 % CI: paired percentile bootstrap of the test rows ({st['HOLD_BOOTSTRAP_REPS']} reps, seed "
        f"{st['HOLD_BOOTSTRAP_SEED']}, resampled within groups), the same resample for baseline and model. No model "
        "is refitted per rep, so it covers only which test calls were drawn: a floor on the uncertainty. One "
        "calendar split: it shows whether the addition holds on the latest calls, not on every period.",
        f"- n/a below {st['HOLD_MIN_CLASS_N']} training rows (HOLD_MIN_CLASS_N) or {st['HOLD_FWD_MIN_TEST_CLASS_N']} "
        "test rows (HOLD_FWD_MIN_TEST_CLASS_N) in either class. Dates are UTC.", ""]


def fdates(r) -> str:
    return "n/a" if not r else f"{r[0]} to {r[1]}"


def fforward(m: dict) -> list:
    counts = [f"{m['n_train']} / {m['n_test']} / {m['n_embargoed']}", f"{m['train_pos']} / {m['train_neg']}",
              f"{m['test_pos']} / {m['test_neg']}",
              f"{m['dropped_baseline_missing']} / {m['dropped_feature_missing']}"]
    if "na" in m:
        return counts + [f"n/a ({m['na']})", "", "", ""]
    return counts + [f3(m["baseline_auc"]), f3(m["model_auc"]), f3(m["delta"]), fci(m["delta_ci"])]


def render_forward(inc: dict, st: dict) -> list:
    if FWD_KEY not in inc:
        return []
    fw = inc[FWD_KEY]
    L = [FWD_HEAD, ""] + forward_method(st)
    if "na" in fw:
        return L + [f"n/a: {fw['na']}.", ""]
    sp = fw["split"]
    L += [f"Split: {sp['n']} analysable rows: train {sp['n_train']} (entry_at {fdates(sp['train_entry_at'])}), "
          f"embargoed {sp['n_embargoed']} ({fdates(sp['embargoed_entry_at'])}), test {sp['n_test']} "
          f"({fdates(sp['test_entry_at'])}); first test call {sp['first_test_at'] or 'n/a'} UTC, embargo from "
          f"{sp['embargo_from'] or 'n/a'} UTC.", ""]
    rows = [[c, f] + fforward(m) for c, models in fw["contrasts"].items() for f, m in models.items()]
    return L + table(["contrast", "model (+ baseline)", "n train / test / embargoed", "train pos / neg",
                      "test pos / neg", "dropped: baseline / feature", "baseline AUC (test)", "model AUC (test)",
                      "delta", "delta 95 % CI"], rows)


def fdirection(p: dict, pos: str) -> str:
    """Part (d): the observed direction and coefficient (when known) against what passes."""
    seen = [f"pooled {p['observed']} for {pos}" if p.get("observed") else "",
            f"coefficient {p['coef']:+.3f}" if p.get("coef") is not None else ""]
    return f"{', '.join(s for s in seen if s) or 'n/a'}, needs {p['expected']} and coefficient > 0"


def fpart(k: str, p: dict, pos: str) -> str:
    head = f"- ({k}) {p['what']}: "
    if k == P2_DIRECTION[0]:
        return head + fdirection(p, pos) + (f": n/a ({p['na']})" if p["result"] == NA else f": {p['result']}")
    if p["result"] == NA:
        return head + f"n/a ({p['na']}): n/a"
    d = f" (oriented: {p['direction']} for {pos})" if "direction" in p else ""
    return head + f"{f3(p['lower'])}{d}, needs > {p['threshold']:.2f}: {p['result']}"


def render_p2(inc: dict, st: dict) -> list:
    if P2_KEY not in inc:
        return []
    p = inc[P2_KEY]
    why = {NOT_JUDGED: " (no part fails, a part is n/a)", PASS: " (every part passes)",
           FAIL: " (a part fails)"}[p["result"]]
    return [P2_HEAD, "",
            "The owner's pre-registered rule (approved 23 Oct 2026) for the full holder run (P2, about 4,760 calls, "
            "data not seen when it was fixed). It applies to that run only; on any other run, such as the pilot, it "
            "is shown for information only. PASS leads to a dead7 model trial with the feature through the "
            "walk-forward gate (a separate step); FAIL stops the holder work. Never part of the verdict above.",
            "",
            "The rule has four parts. (a), (b) and (c) were approved on 23 Oct 2026; (d), direction, is the owner's "
            "addition of 24 Oct 2026: the feature passes only if it points the same way as in the pilot (more "
            f"sellers before the call, more likely {p['contrast'].split(' vs ')[0]}), that is its pooled direction "
            "on the model's rows (the one that orients (b)) is the expected one and its standardised coefficient "
            "given the baseline is > 0; n/a only if either value is missing.",
            "",
            f"Feature {p['feature']}, contrast {p['contrast']}; every bound strict (>); FAIL if any part "
            "fails (even when another part is n/a), else NOT JUDGED if any part is n/a, PASS only if all four pass. "
            "The n/a handling (NOT JUDGED when no part fails but a part could not be computed) is the PM's reading, "
            "not part of the approved rule.",
            ""] + [fpart(k, part, p["contrast"].split(" vs ")[0]) for k, part in p["parts"].items()] + [
        f"- P2 rule: {p['result']}{why}", ""]


def clean(o):
    """JSON-safe: numpy scalars to Python, NaN/inf to None."""
    if isinstance(o, dict):
        return {str(k): clean(v) for k, v in o.items()}
    if isinstance(o, (list, tuple)):
        return [clean(v) for v in o]
    if isinstance(o, (np.integer, np.bool_)):
        return o.item()
    if isinstance(o, (float, np.floating)):
        return float(o) if np.isfinite(o) else None
    return o


# --- output ----------------------------------------------------------------------
def inside(p, base) -> bool:
    a, b = os.path.normcase(str(p)), os.path.normcase(str(base))
    try:
        return os.path.commonpath([a, b]) == b
    except ValueError:  # another drive
        return False


def check_out_dir(out, repo_root) -> Path:
    """Outside every checkout, or inside the innermost checkout's git-ignored data/."""
    p = Path(out).resolve()
    roots = {Path(repo_root).resolve()} | {d for d in (p, *p.parents) if (d / ".git").exists()}
    containing = [r for r in roots if inside(p, r)]
    if not containing:
        return p
    root = max(containing, key=lambda r: len(str(r)))
    if not inside(p, root / "data"):
        raise Refused("--out is inside the repo but outside its data/ folder")
    ignore = root / ".gitignore"
    lines = ignore.read_text(encoding="utf-8").splitlines() if ignore.is_file() else []
    if "/data/" not in (x.strip() for x in lines):
        raise Refused("the repo's .gitignore does not ignore /data/: refusing to write there")
    return p


def run(holders_dir, ids_path, out, repo_root, now=None) -> Path:
    """Check, analyse, then write <out>/<YYYYMMDD-HHMM UTC>/report.md and summary.json."""
    features = check_inputs(C.HOLD_FEATURES)
    incr_features()
    dest = check_out_dir(out, repo_root) / (now or datetime.now(timezone.utc)).strftime("%Y%m%d-%H%M")
    if dest.exists():
        raise Refused(f"output folder {dest.name} already exists")
    ex = load_export(holders_dir)
    check_export(ex, features)
    ids = load_ids(ids_path)
    summary = {"run": dest.name, "settings": settings(), **analyse(ex, ids, features)}
    summary["inputs"]["sha256"] = {**ex["sha256"], f"ids ({Path(ids_path).name})": sha256(ids_path)}
    summary = clean(summary)
    report = render_report(summary)
    try:
        dest.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        raise Refused(f"output folder {dest.name} already exists") from None
    (dest / "report.md").write_text(report + "\n", encoding="utf-8")
    (dest / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    return dest
