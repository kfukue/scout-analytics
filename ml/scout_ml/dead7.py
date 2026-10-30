"""The dead7 trial (report only; nothing saved or served): does pre_hold_sellers add to the
live model's inputs for predicting dead7? Rules: the DEAD7_* block of config.py (owner,
29 Oct 2026). dead7_trial.py is the command line.

Two LightGBM dead7 classifiers per seed and window on the same rows: "without" = config
FEATURES via build_features (the one feature builder), "with" = the same plus the raw
C.DEAD7_FEATURE. Mode "trial": weekly expanding walk-forward (validate.walk_forward_windows,
7-day embargo), the pilot ids never test rows. Mode "confirm": one split by fixed dates.
Pure functions except load_pilot_ids and run; the output holds counts and metrics only,
never an address or a per-call row."""
import json
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd
from sklearn.metrics import roc_auc_score

from . import config as C
from . import diagnostics as D
from . import holders as H
from . import validate as V
from .features import assert_no_leakage, build_features, learn_cat_levels, num
from .labels import build_labels, dead_policy
from .model import fit_lgbm, predict

PASS, FAIL, NA = "PASS", "FAIL", "n/a"
NOT_JUDGED = "NOT JUDGED"
PROVISIONAL = "provisional PASS: must hold again in the confirmation run"
TRIAL, CONFIRM = "trial", "confirm"
MODES = (TRIAL, CONFIRM)
WITH, WITHOUT = "with", "without"
VIEW_REQUIRED = ("call_id", "message_date", "contract_address")
PRE_SELLS = tuple(f"pre_sells_{w}" for w in ("5m", "15m", "60m"))   # the Spearman information lines
REPORT_FILE, SUMMARY_FILE = "report.md", "summary.json"
# why a view row is not usable at 7 d (build_labels' flags), checked in this order; a row with none
# of them lacks its 7d return (the medium bucket's only outcome column, ret_late_7d)
NOT_USABLE_REASONS = (("update", "update post"), ("repeat", "repeat"), ("no_pool", "no pool"),
                      ("not_usd", "not USD"), ("extreme", "extreme outcome"))
MISSING_7D = "missing 7d return"
LIMITS = (
    "These calls already confirmed P2 (report 20261029-2301 UTC), so a PASS here is provisional; "
    "the confirmation run on calls posted from 16 Oct 2026 decides.",
    "For v2/v3 tokens the export counts sellers on the tracker's pool, which is found with a search "
    "that looks past the entry block, so a little after-call information may be in the training values.",
    "Report only: nothing is saved or served; no model file, LATEST or CANDIDATE is written.",
)
# limit (1) in the confirmation run: its test calls are the new ones
CONFIRM_LIMIT = ("The trial ran on calls that had already confirmed P2; this confirmation run tests on calls "
                 "posted from 16 Oct 2026, after them.")
R3_NOTE = ("The first two parts of R3 test the filter itself (as the collapse filter rule does); "
           "R1 and R2 test sellers.")


# --- inputs -------------------------------------------------------------------
def check_settings() -> None:
    """The live inputs must not hold the trial's feature, any holder column or wrep_*."""
    bad = [c for c in C.FEATURES if c.startswith((C.HOLD_PREFIX, C.WREP_PREFIX))]
    if bad:
        raise H.Refused(f"config FEATURES must not include {bad}: the 'without' arm is the live inputs")
    if C.DEAD7_FEATURE not in C.HOLD_FEATURES:
        raise H.Refused(f"DEAD7_FEATURE must be one of HOLD_FEATURES, not {C.DEAD7_FEATURE!r}")
    if C.DEAD7_SEED not in C.DEAD7_SEEDS:
        raise H.Refused(f"DEAD7_SEED {C.DEAD7_SEED!r} must be one of DEAD7_SEEDS {C.DEAD7_SEEDS}")


def load_pilot_ids(path) -> set:
    """The pilot ids file's call_id values (other columns are not read)."""
    p = Path(path)
    if not p.is_file():
        raise H.Refused("pilot ids file missing (--pilot-ids)")
    try:
        ids = pd.read_csv(p, dtype=str, keep_default_na=False, encoding="utf-8-sig")
    except pd.errors.EmptyDataError:
        raise H.Refused("pilot ids file is empty") from None
    ids.columns = [str(c).strip().lower() for c in ids.columns]
    if "call_id" not in ids.columns:
        raise H.Refused("pilot ids file lacks column call_id")
    cid = pd.to_numeric(ids["call_id"].str.strip(), errors="coerce")
    if (cid.isna() | (cid % 1 != 0)).any():
        raise H.Refused("pilot ids file: a call_id that is not a whole number")
    return set(cid.astype("int64"))


def prepare_view(df: pd.DataFrame) -> tuple:
    """The dataset export as train.py reads it: message_date as UTC, sorted, labels
    (build_labels). Returns (rows, labels)."""
    missing = [c for c in VIEW_REQUIRED if c not in df.columns]
    if missing:
        raise H.Refused(f"--csv lacks column(s) {missing} (not a dataset-view export?)")
    df = df.copy()
    df["message_date"] = pd.to_datetime(df["message_date"], utc=True, format="ISO8601")
    cid = pd.to_numeric(df["call_id"], errors="coerce")
    if cid.isna().any() or cid.duplicated().any():
        raise H.Refused("--csv: call_id missing, not a number or duplicated")
    df["call_id"] = cid.astype("int64")
    df = df.sort_values(["message_date", "call_id"], kind="mergesort").reset_index(drop=True)
    return df, build_labels(df, dead_policy(df))


def not_usable_reason(L: pd.DataFrame) -> pd.Series:
    """Per view row, why the 7d bucket cannot use it (build_labels' flags): the first of
    NOT_USABLE_REASONS that applies, MISSING_7D when none does; None for a usable row. Each row
    gets exactly one reason, so the counts add up to the total."""
    why = np.full(len(L), None, dtype=object)
    left = ~L["usable_medium"].to_numpy(bool)
    for flag, name in NOT_USABLE_REASONS:
        hit = left & L[flag].to_numpy(bool)
        why[hit] = name
        left &= ~hit
    why[left] = MISSING_7D
    return pd.Series(why, index=L.index, dtype=object)


def not_usable_counts(view: pd.DataFrame, L: pd.DataFrame, call_ids) -> dict:
    """{reason: count} over the view rows of `call_ids` that are not usable at 7 d, every reason
    listed (zeros too), in NOT_USABLE_REASONS order then MISSING_7D."""
    why = not_usable_reason(L)[view["call_id"].isin(set(call_ids)).to_numpy()]
    counts = why.value_counts()
    return {name: int(counts.get(name, 0)) for name in [n for _, n in NOT_USABLE_REASONS] + [MISSING_7D]}


def export_frame(ex: dict, ids: pd.DataFrame) -> tuple:
    """(per ids row: call_id, row_class and C.DEAD7_FEATURE as float; export rows whose call_id
    is not in the ids file, ignored). Only C.DEAD7_FEATURE is read as a value (the allowlist);
    status, skip_reason, entry_block and pre_hold_incomplete only decide the row class."""
    wanted = set(ids["call_id"])
    kept = [r for r in ex["rows"] if r["call_id"] in wanted]
    rows = pd.DataFrame(kept, columns=ex["columns"], dtype=object)
    rows = rows[["call_id", "status", "skip_reason", "entry_block", "pre_hold_incomplete", C.DEAD7_FEATURE]]
    rows["call_id"] = rows["call_id"].astype("int64")
    rows["_in_export"] = True
    df = ids[["call_id", "entry_block"]].merge(rows, on="call_id", how="left", suffixes=("_ids", ""))
    df["_in_export"] = df["_in_export"].eq(True)
    df["row_class"] = [H.row_class(r) for r in df.to_dict("records")]
    ok = df["row_class"].eq("analysable")
    value = pd.Series(np.nan, index=df.index, dtype=float)
    value[ok] = H.values(df.loc[ok, C.DEAD7_FEATURE])   # NULL -> NaN, never 0
    return pd.DataFrame({"call_id": df["call_id"], "row_class": df["row_class"],
                         C.DEAD7_FEATURE: value}), len(ex["rows"]) - len(kept)


def assemble(view: pd.DataFrame, L: pd.DataFrame, ids: pd.DataFrame, ex: dict, pilot: set) -> tuple:
    """The trial's rows: the ids file's calls that have a view row usable at 7 d and an analysable
    export row, in view order. Returns (data, drops): data = {"rows": the view columns plus
    C.DEAD7_FEATURE only, "y": dead7 1/0, "pilot", "net_ret" (net 7d return), "runner" (the 7d
    runner label, information)}; drops = every count on the way."""
    exf, stray = export_frame(ex, ids)
    in_view = ids["call_id"].isin(view["call_id"])
    usable = view.loc[L["usable_medium"].to_numpy(), "call_id"]
    step = ids.loc[in_view]
    usable7 = step["call_id"].isin(usable)
    step = step.loc[usable7]
    by_id = exf.set_index("call_id")
    cls = by_id["row_class"].reindex(step["call_id"])
    no_row = cls.eq("no row").to_numpy()
    analysable = cls.eq("analysable").to_numpy()
    other = ~no_row & ~analysable
    keep_ids = set(step.loc[analysable, "call_id"])
    pos = np.flatnonzero(view["call_id"].isin(keep_ids).to_numpy())
    rows = view.iloc[pos].reset_index(drop=True)
    rows[C.DEAD7_FEATURE] = rows["call_id"].map(by_id[C.DEAD7_FEATURE]).astype(float)
    grp = rows["call_id"].map(ids.set_index("call_id")["grp"])
    if grp.isna().any():   # never expected: every kept id comes from the ids file
        raise H.Refused("a kept row without its ids-file group")
    data = {"rows": rows, "y": (grp == C.DEAD7_GROUP).to_numpy().astype(float),
            "pilot": rows["call_id"].isin(pilot).to_numpy(),
            "net_ret": L["net_ret_medium"].to_numpy()[pos], "runner": L["runner_medium"].to_numpy()[pos]}
    drops = {
        "ids_rows": int(len(ids)), "ids_dead7": int((ids["grp"] == C.DEAD7_GROUP).sum()),
        "no_view_row": int((~in_view).sum()),
        "not_usable_7d": int((~usable7).sum()),
        "not_usable_7d_by_reason": not_usable_counts(view, L, ids.loc[in_view, "call_id"][~usable7.to_numpy()]),
        "no_holder_row": int(no_row.sum()),
        "not_analysable": {str(k): int(v) for k, v in
                           sorted(cls[other].value_counts().items(), key=lambda kv: H.class_key(kv[0]))},
        "no_label": 0,   # every ids row has a group (load_ids refuses one outside HOLD_GROUPS)
        "analysed_rows": int(len(rows)), "analysed_dead7": int(data["y"].sum()),
        "view_rows": int(len(view)), "view_rows_not_in_ids": int((~view["call_id"].isin(ids["call_id"])).sum()),
        "export_rows": int(len(ex["rows"])), "export_rows_not_in_ids_ignored": int(stray),
        "pilot_ids": int(len(pilot)), "pilot_ids_not_in_ids": int(len(pilot - set(ids["call_id"]))),
        "pilot_rows_analysed": int(data["pilot"].sum()),
        "sellers_null_analysed": int(rows[C.DEAD7_FEATURE].isna().sum()),
    }
    return data, drops


# --- inputs of the two arms ----------------------------------------------------------
def with_sellers(X: pd.DataFrame, rows: pd.DataFrame) -> pd.DataFrame:
    """The "with" inputs: `X` (build_features) plus the raw C.DEAD7_FEATURE (NaN stays NaN),
    past the leakage guard; exactly one column added."""
    if C.DEAD7_FEATURE in X.columns:
        raise H.Refused(f"{C.DEAD7_FEATURE} is already a model input")
    Xw = X.copy()
    Xw[C.DEAD7_FEATURE] = num(rows, C.DEAD7_FEATURE).to_numpy()
    assert_no_leakage(list(Xw.columns))
    return Xw


def arm_inputs(rows: pd.DataFrame, train) -> dict:
    """{"without": build_features with category levels from the training rows only, "with": the
    same plus C.DEAD7_FEATURE}."""
    X = build_features(rows, learn_cat_levels(rows[np.asarray(train, bool)]))
    return {WITHOUT: X, WITH: with_sellers(X, rows)}


def fit_arm(X: pd.DataFrame, y, train, seed: int) -> dict:
    """One dead7 classifier: fit_lgbm with no validation part and no calibrator (the same bare fit
    train._dead_walk_forward makes per week), but with the fixed C.DEAD7_ROUNDS rounds (the value of
    FALLBACK_ROUNDS on 29 Oct 2026) instead of the dead score's own tuned round count; LightGBM seed
    `seed`."""
    tr = np.asarray(train, bool)
    with C.overrides(LGBM_SEED=seed):
        return fit_lgbm(X[tr], np.asarray(y)[tr], n_estimators=C.DEAD7_ROUNDS)


def pd_scores(fitted: dict, Xw: pd.DataFrame, test, train_values) -> tuple:
    """R4 (ii): the "with" model's scores of the test rows with C.DEAD7_FEATURE set to its
    training-part 25th and 75th percentile (C.DEAD7_PD_QUANTILES), other inputs unchanged.
    (s25, s75, [q25, q75]); NaN scores when the training part has no value."""
    te = np.asarray(test, bool)
    v = np.asarray(train_values, float)
    v = v[~np.isnan(v)]
    if not len(v):
        return np.full(te.sum(), np.nan), np.full(te.sum(), np.nan), [np.nan, np.nan]
    q = [float(x) for x in np.percentile(v, C.DEAD7_PD_QUANTILES)]
    out = []
    for value in q:
        Xq = Xw[te].copy()
        Xq[C.DEAD7_FEATURE] = value
        out.append(predict(fitted, Xq))
    return out[0], out[1], q


# --- windows and the split ------------------------------------------------------------
def week_status(n_train: int, n_test: int, n_dead7: int, train_pos: int, train_neg: int) -> tuple:
    """(counted, reason): R0's floors (test rows after removing the pilot ids), and a training part
    with both classes (a model needs them)."""
    why = []
    if n_train < C.DEAD7_MIN_TRAIN_ROWS:
        why.append(f"train {n_train} < {C.DEAD7_MIN_TRAIN_ROWS}")
    if n_test < C.DEAD7_MIN_TEST_ROWS:
        why.append(f"test {n_test} < {C.DEAD7_MIN_TEST_ROWS}")
    if n_dead7 < C.DEAD7_MIN_TEST_DEAD7:
        why.append(f"dead7 test {n_dead7} < {C.DEAD7_MIN_TEST_DEAD7}")
    if not why and min(train_pos, train_neg) < 1:
        why.append("training part has one class only")
    return not why, "; ".join(why)


def trial_windows(dates: pd.Series, tokens: pd.Series, y, pilot) -> list:
    """Every weekly expanding window (validate.walk_forward_windows, C.DEAD7_HORIZON_DAYS embargo):
    test = the week's calls minus the pilot ids; counted per `week_status`."""
    y, pilot = np.asarray(y, float), np.asarray(pilot, bool)
    out = []
    for w in V.walk_forward_windows(dates, tokens, C.DEAD7_HORIZON_DAYS):
        tr, te = w["train"], w["test"] & ~pilot
        row = {"week": w["week"], "start": w["start"].strftime("%Y-%m-%d"), "n_train": int(tr.sum()),
               "pilot_in_train": int((tr & pilot).sum()), "pilot_removed": int((w["test"] & pilot).sum()),
               "n_test": int(te.sum()), "n_dead7": int(y[te].sum())}
        row["counted"], row["reason"] = week_status(row["n_train"], row["n_test"], row["n_dead7"],
                                                    int(y[tr].sum()), int((1 - y[tr]).sum()))
        out.append({**row, "train": tr, "test": te})
    return out


def confirm_split(dates: pd.Series, pilot) -> dict:
    """The confirmation run's one split: train = calls posted before C.DEAD7_CONFIRM_TRAIN_END (pilot
    included), test = calls posted from C.DEAD7_CONFIRM_TEST_START, never a pilot id."""
    d = pd.to_datetime(dates, utc=True)
    end, start = pd.Timestamp(C.DEAD7_CONFIRM_TRAIN_END), pd.Timestamp(C.DEAD7_CONFIRM_TEST_START)
    return {"train": (d < end).to_numpy(), "test": ((d >= start).to_numpy() & ~np.asarray(pilot, bool)),
            "train_end": end.strftime("%Y-%m-%d %H:%M"), "test_start": start.strftime("%Y-%m-%d %H:%M")}


# --- fitting and pooling -------------------------------------------------------------
def score_blocks(rows: pd.DataFrame, y, blocks: list, seeds) -> dict:
    """Fit both arms per block (a counted week, or the confirm split) and seed; every block's test
    rows scored by its own models. Returns the pooled test rows: "idx" (positions in `rows`),
    "block" (block number), per seed "scores" {seed: {arm: array}}, the C.DEAD7_SEED partial
    dependence scores "pd25" / "pd75", and per block its quantiles and AUCs (seed C.DEAD7_SEED)."""
    y = np.asarray(y, float)
    sellers = num(rows, C.DEAD7_FEATURE).to_numpy()
    idx, block, pd25, pd75, info = [], [], [], [], []
    scores = {s: {WITH: [], WITHOUT: []} for s in seeds}
    for b, blk in enumerate(blocks):
        tr, te = blk["train"], blk["test"]
        X = arm_inputs(rows, tr)
        row = {"block": b}
        for seed in seeds:
            for arm in (WITHOUT, WITH):
                fitted = fit_arm(X[arm], y, tr, seed)
                s = predict(fitted, X[arm][te])
                scores[seed][arm].append(s)
                if seed == C.DEAD7_SEED:
                    row[f"auc_{arm}"] = auc(y[te], s)
                    if arm == WITH:
                        s25, s75, row["pd_quantiles"] = pd_scores(fitted, X[WITH], te, sellers[tr])
                        pd25.append(s25)
                        pd75.append(s75)
        idx.append(np.flatnonzero(te))
        block.append(np.full(int(te.sum()), b))
        info.append(row)
    cat = lambda parts: np.concatenate(parts) if parts else np.array([])   # noqa: E731
    return {"idx": cat(idx).astype(int), "block": cat(block).astype(int),
            "scores": {s: {a: cat(v) for a, v in arms.items()} for s, arms in scores.items()},
            "pd25": cat(pd25), "pd75": cat(pd75), "blocks": info}


def auc(y, s) -> float:
    """ROC AUC of any score (a probability or a raw count; NaN without both classes)."""
    y = np.asarray(y, float)
    return float(roc_auc_score(y, np.asarray(s, float))) if len(y) and 0 < y.sum() < len(y) else np.nan


# --- bootstrap ------------------------------------------------------------------------
def group_weights(groups, reps: int, seed: int) -> np.ndarray:
    """Bootstrap counts (reps, n): each group resampled with replacement within itself
    (holders.boot_weights with explicit reps and seed)."""
    g, rng = np.asarray(groups), np.random.default_rng(seed)
    w = np.zeros((reps, len(g)))
    for name in sorted(set(g.tolist())):
        i = np.flatnonzero(g == name)
        w[:, i] = rng.multinomial(len(i), np.full(len(i), 1 / len(i)), size=reps)
    return w


def block_weights(block, reps: int, seed: int) -> np.ndarray:
    """Row weights (reps, n) of a week-block bootstrap: the blocks drawn with replacement exactly as
    diagnostics.pooled draws its weeks (same generator call), each row weighted by how often its
    block was drawn."""
    codes, b = np.unique(np.asarray(block), return_inverse=True)
    idx = np.random.default_rng(seed).integers(0, len(codes), size=(reps, len(codes)))
    counts = np.stack([np.bincount(r, minlength=len(codes)) for r in idx]) if reps else np.zeros((0, len(codes)))
    return counts[:, b].astype(float)


def boot_auc(s, y, w: np.ndarray, chunk: int = 250) -> np.ndarray:
    """AUC of `s` for `y` under each weight row (holders.pair_auc), in chunks of rows."""
    out = [H.pair_auc(s, np.asarray(y, bool), None, w[i:i + chunk]) for i in range(0, len(w), chunk)]
    return np.concatenate(out) if out else np.array([])


def interval(a) -> list:
    """C.DEAD7_CI percentiles of the defined values (None when there is none)."""
    a = np.asarray(a, float)
    a = a[np.isfinite(a)]
    return [float(v) for v in np.percentile(a, C.DEAD7_CI)] if len(a) else None


# --- rules ----------------------------------------------------------------------------
def status(ok) -> str:
    """PASS / FAIL, or n/a for None."""
    return NA if ok is None else PASS if ok else FAIL


def check(value, test) -> str:
    """status of test(value), n/a when `value` is undefined (None / NaN)."""
    if not V._finite(value):
        return NA
    return status(bool(test(value)))


def combine(statuses) -> str:
    """A rule from its parts, or the result from the rules: FAIL if any fails (even if another is
    n/a); else n/a if any is n/a; else PASS."""
    s = list(statuses)
    return FAIL if FAIL in s else NA if (NA in s or not s) else PASS


def result_of(rules: dict) -> str:
    """PASS / FAIL / NOT JUDGED from the rules' statuses (`combine`)."""
    r = combine(v["status"] for v in rules.values())
    return NOT_JUDGED if r == NA else r


def result_line(result: str, mode: str) -> str:
    return PROVISIONAL if result == PASS and mode == TRIAL else result


def rule_r1(y, scores: dict) -> dict:
    """Median over the seeds of the pooled AUC delta (with minus without, same seed) > 0."""
    deltas = {str(s): auc(y, a[WITH]) - auc(y, a[WITHOUT]) for s, a in scores.items()}
    vals = list(deltas.values())
    med = float(np.median(vals)) if vals and all(V._finite(v) for v in vals) else np.nan
    return {"status": check(med, lambda v: v > C.DEAD7_MIN_MEDIAN_DELTA), "median_delta": med,
            "deltas": deltas, "threshold": C.DEAD7_MIN_MEDIAN_DELTA}


def rule_r2(y, s_with, s_without, groups, block=None) -> dict:
    """Paired pooled AUC delta: point, 95 % percentile bootstrap (rows resampled within `groups`,
    the same weights for both arms), lower bound > 0. `block` (trial): the week-block interval,
    information only."""
    y = np.asarray(y, float)
    point = auc(y, s_with) - auc(y, s_without)
    w = group_weights(groups, C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED)
    ci = interval(boot_auc(s_with, y, w) - boot_auc(s_without, y, w))
    out = {"delta": point, "ci": ci, "threshold": C.DEAD7_MIN_DELTA_LOWER,
           "status": check(None if ci is None else ci[0], lambda v: v > C.DEAD7_MIN_DELTA_LOWER)}
    if block is not None:
        wb = block_weights(block, C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED)
        out["week_block_ci"] = interval(boot_auc(s_with, y, wb) - boot_auc(s_without, y, wb))
    return out


def filter_flags(score, block) -> np.ndarray:
    """Per block, the top C.DEAD7_SKIP_FRAC of its rows by `score` (validate.top_mask)."""
    score, block = np.asarray(score, float), np.asarray(block)
    flag = np.zeros(len(score), bool)
    for b in np.unique(block):
        i = np.flatnonzero(block == b)
        flag[i] = V.top_mask(score[i], C.DEAD7_SKIP_FRAC)
    return flag


def weighted_filter_sums(y, runner, ret, flag, w: np.ndarray) -> dict:
    """validate.collapse_filter_sums under row weights `w` (reps, n): one value per weight row
    (dead7 in the collapse place; net return capped at C.SIM_MAX_RET_PCT as there)."""
    y, runner = np.nan_to_num(np.asarray(y, float)), np.nan_to_num(np.asarray(runner, float))
    ret = np.minimum(np.asarray(ret, float), C.SIM_MAX_RET_PCT)
    f = np.asarray(flag, float)
    return {"n": w.sum(1), "flag_n": w @ f, "coll_all": w @ y, "coll_flag": w @ (y * f),
            "run_all": w @ runner, "run_flag": w @ (runner * f), "ret_all": w @ ret, "ret_flag": w @ (ret * f)}


def filter_block(y, runner, ret, flag, w: np.ndarray) -> dict:
    """The skip filter's rates (validate.collapse_filter_rates; dead7 caught = collapses_caught):
    point values and C.DEAD7_CI intervals from the bootstrap weights `w`."""
    point = V.collapse_filter_rates(V.collapse_filter_sums(y, runner, ret, flag))
    boot = V.collapse_filter_rates(weighted_filter_sums(y, runner, ret, flag, w))
    out = {k: float(v) for k, v in point.items()}
    for k, v in boot.items():
        out[f"{k}_ci"] = interval(v)
    return out


def rule_r3(y, runner, ret, s_with, s_without, block, w: np.ndarray) -> dict:
    """Skip the top 30 % per block by the "with" score (the collapse filter, dead7 in the collapse
    place): dead7 caught lower bound > 0.30, kept minus all lower bound > 0 (validate._filter_rule,
    as filter_verdict), and caught (point) with >= caught (point) without."""
    y = np.asarray(y, float)
    fw = filter_block(y, runner, ret, filter_flags(s_with, block), w)
    fo = V.collapse_filter_rates(V.collapse_filter_sums(y, runner, ret, filter_flags(s_without, block)))
    reasons = []
    caught_lo = (fw["collapses_caught_ci"] or [np.nan])[0]
    kept_lo = (fw["kept_minus_all_ci"] or [np.nan])[0]
    caught_without = float(fo["collapses_caught"])
    parts = {
        "caught_lower": _filter_part(caught_lo, C.DEAD7_MIN_CAUGHT_LOWER, "dead7 caught", reasons),
        "kept_minus_all_lower": _filter_part(kept_lo, C.DEAD7_MIN_KEPT_MINUS_ALL_LOWER,
                                             "kept minus all (mean net %)", reasons),
        "caught_vs_without": check(fw["collapses_caught"] - caught_without, lambda v: v >= 0)}
    return {"status": combine(parts.values()), "parts": parts, "with": fw,
            "without": {k: float(v) for k, v in fo.items()}, "caught_lower": caught_lo,
            "kept_minus_all_lower": kept_lo, "reasons": reasons, "skip_frac": C.DEAD7_SKIP_FRAC,
            "min_caught_lower": C.DEAD7_MIN_CAUGHT_LOWER,
            "min_kept_minus_all_lower": C.DEAD7_MIN_KEPT_MINUS_ALL_LOWER,
            "sim_max_ret_pct": C.SIM_MAX_RET_PCT, "note": R3_NOTE}


def _filter_part(value, minimum: float, name: str, reasons: list) -> str:
    """One lower-bound part of R3: validate._filter_rule (filter_verdict's rule), n/a when undefined."""
    if not V._finite(value):
        reasons.append(f"pooled {name} 95% interval undefined")
        return NA
    return status(V._filter_rule(value, minimum, True, name, reasons))


def rule_r4(y, sellers, pd25, pd75) -> dict:
    """Direction: (i) AUC of the raw feature for dead7 on the pooled test rows (NULL rows left out)
    > 0.5; (ii) mean(score at q75 - score at q25) > 0."""
    y, x = np.asarray(y, float), np.asarray(sellers, float)
    has = ~np.isnan(x)
    raw = auc(y[has], x[has]) if has.any() else np.nan
    diff = np.asarray(pd75, float) - np.asarray(pd25, float)
    diff = diff[np.isfinite(diff)]
    mean = float(diff.mean()) if len(diff) else np.nan
    parts = {"raw_auc": check(raw, lambda v: v > C.DEAD7_MIN_RAW_AUC),
             "pd_mean": check(mean, lambda v: v > C.DEAD7_MIN_PD_DELTA)}
    return {"status": combine(parts.values()), "parts": parts, "raw_auc": raw,
            "raw_rows": int(has.sum()), "direction": None if not V._finite(raw) else
            "higher" if raw > C.DEAD7_MIN_RAW_AUC else "lower or none",
            "pd_mean": mean, "pd_rows": int(len(diff)), "quantiles": list(C.DEAD7_PD_QUANTILES)}


def drifting(adv: dict) -> list:
    """C.DEAD7_FEATURE among the top C.DEAD7_DRIFT_TOP drifting inputs at a gain share >=
    C.DEAD7_DRIFT_MAX_SHARE ([[name, share]], as train._wrep_drift)."""
    top = list((adv or {}).get("top") or [])[:C.DEAD7_DRIFT_TOP]
    return [[f, float(g)] for f, g in top if f == C.DEAD7_FEATURE and float(g) >= C.DEAD7_DRIFT_MAX_SHARE]


def rule_r5(X: dict, is_test, mask) -> dict:
    """Adversarial AUC (diagnostics.adversarial, as the wallet drift rule) of the rows `mask`,
    0 = training, 1 = test: with <= without + 0.02, and the feature not drifting (`drifting`)."""
    m, t = np.asarray(mask, bool), np.asarray(is_test, bool)
    adv = {arm: D.adversarial(X[arm][m], t[m]) for arm in (WITHOUT, WITH)}
    a_w, a_o = adv[WITH]["auc"], adv[WITHOUT]["auc"]
    rise = round(a_w - a_o, 9) if V._finite(a_w) and V._finite(a_o) else np.nan
    drift = drifting(adv[WITH])
    parts = {"auc_rise": check(rise, lambda v: v <= C.DEAD7_MAX_ADV_AUC_RISE),
             "not_drifting": NA if not V._finite(a_w) else status(not drift)}
    keep = lambda a: {**a, "top": list(a.get("top") or [])[:max(3, C.DEAD7_DRIFT_TOP)]}   # noqa: E731
    return {"status": combine(parts.values()), "parts": parts, "auc_with": a_w, "auc_without": a_o,
            "rise": rise, "drifting": drift, "with": keep(adv[WITH]), "without": keep(adv[WITHOUT]),
            "max_rise": C.DEAD7_MAX_ADV_AUC_RISE, "drift_top": C.DEAD7_DRIFT_TOP,
            "drift_max_share": C.DEAD7_DRIFT_MAX_SHARE}


def not_judged_rules(reason: str) -> dict:
    return {k: {"status": NA, "na": reason} for k in ("R1", "R2", "R3", "R4", "R5")}


def judge(data: dict, pooled: dict, adv_split: dict, weekly: bool) -> dict:
    """R1-R5 on the pooled test rows (`score_blocks`). `weekly`: trial (bootstrap within week x
    class, R3 by week blocks, the week-block line); else confirm (within class only)."""
    i = pooled["idx"]
    y, b = data["y"][i], pooled["block"]
    s7 = pooled["scores"][C.DEAD7_SEED]
    groups = b * 2 + y.astype(int) if weekly else y.astype(int)
    w3 = (block_weights(b, C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED) if weekly
          else group_weights(y.astype(int), C.DEAD7_BOOT_REPS, C.DEAD7_BOOT_SEED))
    sellers = num(data["rows"], C.DEAD7_FEATURE).to_numpy()
    return {"R1": rule_r1(y, pooled["scores"]),
            "R2": rule_r2(y, s7[WITH], s7[WITHOUT], groups, b if weekly else None),
            "R3": rule_r3(y, data["runner"][i], data["net_ret"][i], s7[WITH], s7[WITHOUT], b, w3),
            "R4": rule_r4(y, sellers[i], pooled["pd25"], pooled["pd75"]),
            "R5": rule_r5(arm_inputs(data["rows"], adv_split["train"]), adv_split["test"],
                          adv_split["train"] | adv_split["test"])}


# --- the two modes -----------------------------------------------------------------------
def run_trial(data: dict) -> dict:
    """Weekly walk-forward; R0, then R1-R5 on the counted weeks' pooled test rows."""
    rows, y = data["rows"], data["y"]
    windows = trial_windows(rows["message_date"], rows["contract_address"], y, data["pilot"])
    at = [i for i, w in enumerate(windows) if w["counted"]]   # positions of the counted weeks
    counted = [windows[i] for i in at]
    r0 = {"status": PASS if len(counted) >= C.DEAD7_MIN_WEEKS else NA, "counted_weeks": len(counted),
          "min_weeks": C.DEAD7_MIN_WEEKS}
    out = {"weeks": [{k: v for k, v in w.items() if k not in ("train", "test")} for w in windows]}
    if r0["status"] == NA:
        out["rules"] = {"R0": r0, **not_judged_rules(f"R0: {len(counted)} counted week(s), need {C.DEAD7_MIN_WEEKS}")}
        return out
    pooled = score_blocks(rows, y, counted, C.DEAD7_SEEDS)
    for i, info in zip(at, pooled["blocks"]):
        out["weeks"][i].update({k: v for k, v in info.items() if k != "block"})
    test = np.zeros(len(rows), bool)
    test[pooled["idx"]] = True
    adv = {"train": counted[0]["train"], "test": test}   # first counted week's training part vs pooled test rows
    out["rules"] = {"R0": r0, **judge(data, pooled, adv, weekly=True)}
    out["pooled"] = pooled_info(data, pooled)
    out["adversarial_split"] = {"train": "the first counted week's training part",
                                "train_rows": int(adv["train"].sum()), "test": "the pooled test rows of the counted weeks",
                                "test_rows": int(test.sum())}
    return out


def run_confirm(data: dict) -> dict:
    """One split by fixed dates; NOT JUDGED below C.DEAD7_CONFIRM_MIN_DEAD7 dead7 test rows."""
    rows, y = data["rows"], data["y"]
    split = confirm_split(rows["message_date"], data["pilot"])
    tr, te = split["train"], split["test"]
    info = {"train_end": split["train_end"], "test_start": split["test_start"], "n_train": int(tr.sum()),
            "pilot_in_train": int((tr & data["pilot"]).sum()), "n_test": int(te.sum()),
            "n_dead7": int(y[te].sum()), "train_dead7": int(y[tr].sum())}
    out = {"split": info}
    if info["n_dead7"] < C.DEAD7_CONFIRM_MIN_DEAD7:
        out["rules"] = not_judged_rules(
            f"the test rows hold {info['n_dead7']} dead7; needs {C.DEAD7_CONFIRM_MIN_DEAD7} dead7; "
            f"deadline {C.DEAD7_CONFIRM_DEADLINE}, then the owner decides")
        return out
    if min(info["train_dead7"], info["n_train"] - info["train_dead7"]) < 1:
        out["rules"] = not_judged_rules("training part has one class only")
        return out
    pooled = score_blocks(rows, y, [split], C.DEAD7_SEEDS)
    out["split"].update({k: v for k, v in pooled["blocks"][0].items() if k != "block"})
    out["rules"] = judge(data, pooled, {"train": tr, "test": te}, weekly=False)
    out["pooled"] = pooled_info(data, pooled)
    out["adversarial_split"] = {"train": "the training part", "train_rows": int(tr.sum()),
                                "test": "the test part", "test_rows": int(te.sum())}
    return out


def pooled_info(data: dict, pooled: dict) -> dict:
    i = pooled["idx"]
    s7 = pooled["scores"][C.DEAD7_SEED]
    return {"rows": int(len(i)), "dead7": int(data["y"][i].sum()),
            "auc_with": auc(data["y"][i], s7[WITH]), "auc_without": auc(data["y"][i], s7[WITHOUT])}


def spearman(rows: pd.DataFrame) -> dict:
    """Information: Spearman of C.DEAD7_FEATURE with each pre_sells_* window input (pairwise rows)."""
    out = {}
    x = num(rows, C.DEAD7_FEATURE)
    for c in PRE_SELLS:
        pair = pd.DataFrame({"x": x, "c": num(rows, c)}).dropna()
        out[c] = {"n": int(len(pair)),
                  "spearman": float(pair.x.rank().corr(pair.c.rank())) if len(pair) > 2 else None}
    return out


def settings() -> dict:
    return {k: getattr(C, k) for k in dir(C) if k.startswith("DEAD7_")} | {
        "SIM_MAX_RET_PCT": C.SIM_MAX_RET_PCT, "LGBM_PARAMS": C.LGBM_PARAMS,
        "LGBM_REGULARISATION": C.LGBM_REGULARISATION, "ADV_PARAMS": C.ADV_PARAMS,
        "ADV_FOLDS": C.ADV_FOLDS, "ADV_EXCLUDE": C.ADV_EXCLUDE, "features_without": list(C.FEATURES)}


def analyse(view_df: pd.DataFrame, ex: dict, ids: pd.DataFrame, pilot: set, mode: str) -> dict:
    """Everything the report shows, from the loaded inputs."""
    if mode not in MODES:
        raise H.Refused(f"--mode must be one of {MODES}, not {mode!r}")
    check_settings()
    view, L = prepare_view(view_df)
    data, drops = assemble(view, L, ids, ex, pilot)
    res = run_trial(data) if mode == TRIAL else run_confirm(data)
    res["result"] = result_of(res["rules"])
    res["result_line"] = result_line(res["result"], mode)
    d = data["rows"]["message_date"]
    limits = list(LIMITS) if mode == TRIAL else [CONFIRM_LIMIT, *LIMITS[1:]]
    return {"mode": mode, "limits": limits, "drops": drops,
            "dates": {"first": d.min().strftime("%Y-%m-%d %H:%M") if len(d) else None,
                      "last": d.max().strftime("%Y-%m-%d %H:%M") if len(d) else None},
            "information": {"spearman": spearman(data["rows"]),
                            "pilot_rows_analysed": drops["pilot_rows_analysed"]},
            **res}


# --- report ------------------------------------------------------------------------------
def fnum(v, spec: str) -> str:
    """`v` formatted with `spec`, "n/a" when undefined (None after H.clean, or NaN)."""
    return "n/a" if not V._finite(v) else format(v, spec)


def f3(v) -> str:
    return fnum(v, ".3f")


def fci(c) -> str:
    return "n/a" if not c else f"{c[0]:.3f} to {c[1]:.3f}"


def render_rules(s: dict) -> list:
    r, out = s["rules"], ["## Rules (set on " + C.DEAD7_RULES_SET_ON + ")", ""]
    if "R0" in r:
        out.append(f"- R0 counted weeks: {r['R0']['counted_weeks']} (need >= {r['R0']['min_weeks']}): "
                   f"{r['R0']['status']}")
    na = next((v["na"] for v in r.values() if "na" in v), None)
    if na:
        return out + [f"- R1-R5: n/a ({na})", ""]
    r1, r2, r3, r4, r5 = (r[k] for k in ("R1", "R2", "R3", "R4", "R5"))
    deltas = ", ".join(f"{k}: {f3(v)}" for k, v in r1["deltas"].items())
    out += [f"- R1 median over seeds of the pooled AUC delta (with minus without): {f3(r1['median_delta'])} "
            f"(> {r1['threshold']:g}): {r1['status']}. Per seed: {deltas}",
            f"- R2 seed {C.DEAD7_SEED}, paired pooled AUC delta {f3(r2['delta'])}, 95 % interval "
            f"{fci(r2['ci'])} (lower bound > {r2['threshold']:g}; resampled within "
            f"{'week x class' if 'week_block_ci' in r2 else 'class'}, {C.DEAD7_BOOT_REPS} reps, seed "
            f"{C.DEAD7_BOOT_SEED}): {r2['status']}"]
    if "week_block_ci" in r2:
        out.append(f"  - information, not a rule: week-block interval {fci(r2['week_block_ci'])}")
    w, o = r3["with"], r3["without"]
    out += [f"- R3 seed {C.DEAD7_SEED}, skip the top {r3['skip_frac']:.0%} by the 'with' score: {r3['status']}",
            f"  - dead7 caught {f3(w['collapses_caught'])}, 95 % interval {fci(w['collapses_caught_ci'])} "
            f"(lower bound > {r3['min_caught_lower']:g}): {r3['parts']['caught_lower']}",
            f"  - kept minus all (mean net 7d %, capped at {r3['sim_max_ret_pct']:g}) "
            f"{fnum(w['kept_minus_all'], '+.2f')}, 95 % interval {fci(w['kept_minus_all_ci'])} (lower bound > "
            f"{r3['min_kept_minus_all_lower']:g}): {r3['parts']['kept_minus_all_lower']}",
            f"  - dead7 caught with {f3(w['collapses_caught'])} >= without {f3(o['collapses_caught'])}: "
            f"{r3['parts']['caught_vs_without']}",
            f"  - {r3['note']}",
            f"- R4 direction: {r4['status']}",
            f"  - AUC of raw {C.DEAD7_FEATURE} for dead7 on the pooled test rows ({r4['raw_rows']} with a value): "
            f"{f3(r4['raw_auc'])} (> {C.DEAD7_MIN_RAW_AUC:g}, \"higher\"): {r4['parts']['raw_auc']}",
            f"  - seed {C.DEAD7_SEED}: mean score change from the training part's {r4['quantiles'][0]}th to "
            f"{r4['quantiles'][1]}th percentile of {C.DEAD7_FEATURE}: {fnum(r4['pd_mean'], '+.4f')} (> 0): "
            f"{r4['parts']['pd_mean']}",
            f"- R5 drift: {r5['status']}",
            f"  - adversarial AUC with {f3(r5['auc_with'])}, without {f3(r5['auc_without'])}, rise "
            f"{f3(r5['rise'])} (<= {r5['max_rise']:g}): {r5['parts']['auc_rise']}",
            f"  - {C.DEAD7_FEATURE} among the top {r5['drift_top']} drifting inputs at a gain share >= "
            f"{r5['drift_max_share']:g}: {'yes ' + str(r5['drifting']) if r5['drifting'] else 'no'}: "
            f"{r5['parts']['not_drifting']}",
            "  - top drifting inputs (with): "
            + (", ".join(f"{f} {fnum(g, '.2f')}" for f, g in r5["with"]["top"]) or "none"),
            ""]
    return out


def render_weeks(s: dict) -> list:
    head = ("| week start (UTC) | train rows | pilot in train | test rows | dead7 test | counted | "
            f"AUC without (seed {C.DEAD7_SEED}) | AUC with | delta |")
    out = ["## Weeks (trial: weekly expanding walk-forward, 7-day embargo; pilot ids never test rows)", "",
           head, "|" + "---|" * 9]
    for w in s["weeks"]:
        a_o, a_w = w.get("auc_without"), w.get("auc_with")
        delta = a_w - a_o if V._finite(a_o) and V._finite(a_w) else None
        out.append(f"| {w['start']} | {w['n_train']} | {w['pilot_in_train']} | {w['n_test']} | {w['n_dead7']} | "
                   f"{'yes' if w['counted'] else 'no: ' + w['reason']} | {f3(a_o)} | {f3(a_w)} | {f3(delta)} |")
    return out + [""]


def render_split(s: dict) -> list:
    sp = s["split"]
    return ["## Split (confirm)", "",
            f"- train: calls posted before {sp['train_end']} UTC: {sp['n_train']} rows "
            f"({sp['train_dead7']} dead7; pilot in train {sp['pilot_in_train']})",
            f"- test: calls posted from {sp['test_start']} UTC, no pilot id: {sp['n_test']} rows, "
            f"{sp['n_dead7']} dead7 (needs {C.DEAD7_CONFIRM_MIN_DEAD7})",
            f"- AUC seed {C.DEAD7_SEED}: without {f3(sp.get('auc_without'))}, with {f3(sp.get('auc_with'))}", ""]


def render_report(s: dict) -> str:
    d = s["drops"]
    na = d["not_analysable"]
    out = [f"# dead7 trial, mode {s['mode']} (report only), run {s['run']} UTC", "",
           "Known limits:", ""] + [f"{i}. {t}" for i, t in enumerate(s["limits"], 1)] + [
        "", f"**Result: {s['result_line']}**", "",
        "## Inputs and drops", "",
        f"- ids file: {d['ids_rows']} calls ({d['ids_dead7']} dead7); label dead7 = grp \"{C.DEAD7_GROUP}\", "
        "every other group 0",
        f"- dropped, no view row: {d['no_view_row']}",
        f"- dropped, not usable at 7d in the view: {d['not_usable_7d']} ("
        + ", ".join(f"{k} {v}" for k, v in d["not_usable_7d_by_reason"].items())
        + "; each row under the first reason that applies, in this order)",
        f"- dropped, no holder export row: {d['no_holder_row']}",
        "- dropped, not analysable: " + (", ".join(f"{k} {v}" for k, v in na.items()) or "0"),
        f"- dropped, no label: {d['no_label']}",
        f"- analysed: {d['analysed_rows']} calls ({d['analysed_dead7']} dead7), "
        f"{s['dates']['first']} to {s['dates']['last']} UTC; {C.DEAD7_FEATURE} NULL in "
        f"{d['sellers_null_analysed']} (kept as missing)",
        f"- view rows not in the ids file (no label, not used): {d['view_rows_not_in_ids']} of {d['view_rows']}",
        f"- holder export rows not in the ids file (ignored): {d['export_rows_not_in_ids_ignored']} of "
        f"{d['export_rows']}",
        f"- pilot ids: {d['pilot_ids']} ({d['pilot_ids_not_in_ids']} not in the ids file); among the analysed "
        f"calls {d['pilot_rows_analysed']} (training rows only, never test rows)",
        f"- inputs: without = config FEATURES ({len(s['settings']['features_without'])} inputs); with = the same "
        f"plus {C.DEAD7_FEATURE} (raw count); LightGBM, {C.DEAD7_ROUNDS} rounds, no early stopping", ""]
    out += render_weeks(s) if s["mode"] == TRIAL else render_split(s)
    p = s.get("pooled")
    if p:
        out += [f"Pooled test rows: {p['rows']} ({p['dead7']} dead7); seed {C.DEAD7_SEED} AUC without "
                f"{f3(p['auc_without'])}, with {f3(p['auc_with'])}.", ""]
    if s.get("adversarial_split"):
        a = s["adversarial_split"]
        out += [f"R5 compares {a['train']} ({a['train_rows']} rows) with {a['test']} ({a['test_rows']} rows).", ""]
    out += render_rules(s)
    info = s["information"]
    out += ["## Information (never rules)", ""]
    out += [f"- Spearman of {C.DEAD7_FEATURE} with {c}: {f3(v['spearman'])} ({v['n']} rows)"
            for c, v in info["spearman"].items()]
    out += [f"- pilot rows among the analysed calls (used for training only): {info['pilot_rows_analysed']}",
            "", f"**Result: {s['result_line']}**"]
    return "\n".join(out)


def run(csv, holders_dir, ids_path, pilot_path, mode, out, repo_root, now=None) -> Path:
    """Check, analyse, then write <out>/<YYYYMMDD-HHMM UTC>/report.md and summary.json (nothing else)."""
    run_id = (now or datetime.now(timezone.utc)).strftime("%Y%m%d-%H%M")
    dest = H.check_out_dir(out, repo_root) / run_id
    if dest.exists():
        raise H.Refused(f"output folder {dest.name} already exists")
    if not Path(csv).is_file():
        raise H.Refused("dataset export missing (--csv)")
    ex = H.load_export(holders_dir)
    H.check_export(ex, C.HOLD_FEATURES)
    ids = H.load_ids(ids_path)
    pilot = load_pilot_ids(pilot_path)
    view_df = pd.read_csv(csv, low_memory=False)
    summary = {"run": run_id, "settings": settings(), **analyse(view_df, ex, ids, pilot, mode)}
    summary["inputs"] = {"sha256": {**ex["sha256"], f"csv ({Path(csv).name})": H.sha256(csv),
                                    f"ids ({Path(ids_path).name})": H.sha256(ids_path),
                                    f"pilot ids ({Path(pilot_path).name})": H.sha256(pilot_path)}}
    summary = H.clean(summary)
    report = render_report(summary)
    try:
        dest.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        raise H.Refused(f"output folder {dest.name} already exists") from None
    (dest / REPORT_FILE).write_text(report + "\n", encoding="utf-8")
    (dest / SUMMARY_FILE).write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    return dest
