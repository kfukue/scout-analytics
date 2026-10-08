"""Render report.md from the results dict produced by train.py."""
import math

from .config import (BUCKETS, GATES, KFOLD_MIN_FOLDS, LABELS, LIFT_CI_Z, MAX_OUTCOME_PCT,
                     SIM_MAX_RET_PCT, SKIP_FRAC, TOP_FRAC, UNUSED_VIEW_COLUMNS)

LIFT_METHOD = (f"Lift interval: approximate 95% (z = {LIFT_CI_Z:g}) Wilson score interval of the "
               f"runner rate in the top {TOP_FRAC:.0%}, divided by the runner rate of all calls "
               "in the same part (treated as fixed). Counts: runners in the top / calls in the "
               "top, runners / calls overall.")


def lift_ci(t) -> str:
    """'[lo, hi]' for the top-10% lift (n/a when undefined)."""
    return f"[{fmt(t.get('top_lift_lo'), 2)}, {fmt(t.get('top_lift_hi'), 2)}]"


def lift_counts(t) -> str:
    return f"{t.get('top_pos', 0)}/{t.get('top_n', 0)} of {t.get('all_pos', 0)}/{t.get('all_n', 0)}"


def split_line(s, horizon_days) -> str:
    """The Split line: drops by the embargo, and by token grouping only when any."""
    drops = f"{s['n_dropped_embargo']} rows dropped by the {horizon_days:g}-day embargo"
    if s.get("n_dropped_token"):
        drops += f", {s['n_dropped_token']} by token grouping"
    if s["kind"] == "forward":
        return (f"Split (forward, no validation part): train {s['n_train']} rows (posted before "
                f"{s['t_train_end']}), test {s['n_test']} (from {s['t_test']}: the last "
                f"{s['test_days']} days, a date boundary); {drops}. Rounds and calibration: "
                f"purged time-ordered {s['k']}-fold inside the train part only (fit rows within "
                f"{horizon_days:g} days of a held-out fold purged; calibration fitted out of fold).\n")
    return (f"Split: train {s['n_train']} rows (before {s['t1']}), validation {s['n_val']}, "
            f"test {s['n_test']} (from {s['t2']}); {drops}.\n")


def fmt(v, digits=3) -> str:
    if v is None or (isinstance(v, float) and math.isnan(v)):
        return "n/a"
    if isinstance(v, bool):
        return "yes" if v else "no"
    return f"{v:.{digits}f}" if isinstance(v, float) else str(v)


def table(headers, rows) -> str:
    lines = ["| " + " | ".join(headers) + " |", "|" + "---|" * len(headers)]
    lines += ["| " + " | ".join(fmt(c) for c in r) + " |" for r in rows]
    return "\n".join(lines) + "\n"


def render(res: dict) -> str:
    d = res["data"]
    out = [f"# Scout model report — version {res['version']}\n",
           "## Verdict\n",
           table(["bucket", "result", "top-10% lift >= 2", "collapses removed >= 40%",
                  "beats buy-all in every week"],
                 [[b, "skipped" if r.get("skipped") else ("PASS" if r["gates"]["passed"] else "FAIL")]
                  + ([""] * 3 if r.get("skipped") else
                     [r["gates"]["lift_ok"], r["gates"]["collapse_ok"],
                      f"{fmt(r['gates']['walk_forward_ok'])} ({r['gates']['windows_beating']}"
                      f"/{r['gates']['windows_evaluated']} weeks)"])
                  for b, r in res["buckets"].items()]),
           "## Data and exclusions\n",
           f"- rows read: {d['rows']} ({d['first_date']} to {d['last_date']})",
           f"- excluded, update post (not a call): {d.get('update', 0)}",
           f"- excluded, repeat call (not the token's first call; only first calls are tracked): {d.get('repeat', 0)}",
           f"- excluded, no pool (never tradable; out of training and simulation): {d['no_pool']}",
           f"- excluded, price_unit not 'usd': {d['not_usd']}",
           f"- excluded, extreme outcome (a label/simulation outcome above {MAX_OUTCOME_PCT:,.0f} %; "
           f"bogus pool data): {d.get('extreme', 0)}",
           f"- rows left before per-bucket outcome availability: {d['eligible']}\n",
           table(["bucket", "not labelled yet (horizon not due or no data; not a negative)", "usable rows"],
                 [[b, d["eligible"] - r["usable"], r["usable"]] for b, r in res["buckets"].items()]),
           "## Feature coverage (share of non-null values, rows left after the exclusions above)\n",
           table(["column", "non-null"], [[c, v] for c, v in d["coverage"].items()]),
           f"Not model inputs (always 0 / NULL for a first call): {', '.join(UNUSED_VIEW_COLUMNS)}.\n"]
    for b, r in res["buckets"].items():
        cfg = BUCKETS[b]
        out.append(f"## Bucket `{b}` ({cfg['horizon_days']}d horizon)\n")
        out.append("Labels (net of tax): " + "; ".join(
            f"{lab} = {cfg[lab][0]} {cfg[lab][1]} {cfg[lab][2]:g}%"
            + (" OR rugged" if lab == "collapse" and cfg["collapse_or_rugged"] else "")
            for lab in LABELS) + "\n")
        if r.get("skipped"):
            out.append(f"**SKIPPED:** {r['skipped']}\n")
            continue
        out.append(split_line(r["split"], cfg["horizon_days"]))
        out.append("### Class balance\n")
        out.append(table(["label", "positive rate (usable)", "train pos", "train neg", "note"],
                         [[lab, m["positive_rate"], m["train_pos"], m["train_neg"],
                           "; ".join(m["messages"])] for lab, m in r["labels"].items()]))
        cv = [[lab, f"{m['cv']['folds_used']}/{m['cv']['folds']}",
               ", ".join(str(x) for x in m["cv"]["fold_rounds"]) or "-", m.get("rounds"),
               m["cv"]["oof_rows"] or "none (uncalibrated)"]
              for lab, m in r["labels"].items() if m.get("cv")]
        if cv:
            out.append(f"### Rounds and calibration (purged k-fold inside train; a fold needs enough "
                       f"of each class, at least {KFOLD_MIN_FOLDS} folds)\n")
            if any(m["cv"]["fold_rounds"] for m in r["labels"].values() if m.get("cv")):
                out.append("The rounds and the Platt calibration come from the smaller fold "
                           "models and are applied to the model refitted on the whole train part; "
                           "this is standard practice and can slightly underestimate the rounds.\n")
            out.append(table(["label", "folds used", "best rounds per fold", "rounds (median)",
                              "out-of-fold rows for calibration"], cv))
        rows = [[lab, name, m[name]["n"], m[name]["base_rate"], m[name]["roc_auc"],
                 m[name]["pr_auc"], m[name]["brier"]]
                for lab, m in r["labels"].items() if not m.get("skipped")
                for name in ("lightgbm", "logistic")]
        out.append("### Test metrics\n")
        out.append(table(["label", "model", "n", "base rate", "ROC AUC", "PR AUC", "Brier"], rows))
        t, g = r["trading"], r["gates"]
        out.append(table(["check (LightGBM, test part)", "value", "needed", "ok"], [
            [f"runner lift, top {TOP_FRAC:.0%} by runner score", t["top_lift"],
             f">= {GATES['min_top_lift']:g}", g["lift_ok"]],
            ["  95% interval of that lift; runners top/calls top of runners/calls",
             f"{lift_ci(t)}; {lift_counts(t)}", "", ""],
            [f"collapses removed by skipping top {SKIP_FRAC:.0%} collapse score",
             t["collapse_removed"], f">= {GATES['min_collapse_removed']:g}", g["collapse_ok"]],
            ["simulation beats buy-everything in every walk-forward week",
             f"{g['windows_beating']}/{g['windows_evaluated']}", "all", g["walk_forward_ok"]]]))
        out.append(f"Money simulation on test (mean net {cfg['ret_col']}, %): top {TOP_FRAC:.0%} "
                   f"by runner score = {fmt(t['sim_top_mean'], 1)} over {t['sim_n_top']} calls; "
                   f"all calls = {fmt(t['sim_all_mean'], 1)} (each call's net return capped at "
                   f"+{SIM_MAX_RET_PCT:,.0f} %).\n")
        out.append(LIFT_METHOD + "\n")
        out.append(f"**Bucket result: {'PASS' if g['passed'] else 'FAIL'}**\n")
        out.append("### Walk-forward (expanding weekly windows)\n")
        out.append(table(["week", "from", "train n", "test n", "runner AUC", "top lift",
                          "lift 95% interval", "runners top/n of all/n", "collapses removed",
                          "top mean %", "all mean %", "beats all"],
                         [[w["week"], w["start"], w["n_train"], w["n_test"]]
                          + (["skipped: " + w["skipped"]] + [""] * 7 if w.get("skipped") else
                             [w["runner_auc"], w["top_lift"], lift_ci(w), lift_counts(w),
                              w["collapse_removed"],
                              fmt(w["sim_top_mean"], 1), fmt(w["sim_all_mean"], 1),
                              w["sim_beats_all"]]) for w in r["walk_forward"]]))
        for lab, m in r["labels"].items():
            if m.get("skipped"):
                continue
            out.append(f"### `{lab}`: calibration by decile (test) and top features (gain)\n")
            out.append(table(["decile", "n", "mean predicted", "observed"],
                             [[c["decile"], c["n"], c["mean_pred"], c["observed"]]
                              for c in m["calibration"]]))
            out.append(table(["feature", "gain share"], m["importance"]))
    return "\n".join(out)
