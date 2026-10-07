"""Render report.md from the results dict produced by train.py."""
import math

from .config import BUCKETS, GATES, LABELS, MAX_OUTCOME_PCT, SIM_MAX_RET_PCT, SKIP_FRAC, TOP_FRAC


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
           f"- excluded, no pool (never tradable; out of training and simulation): {d['no_pool']}",
           f"- excluded, repeat call not tracked (only the first call of each token is tracked): {d.get('repeat', 0)}",
           f"- excluded, price_unit not 'usd': {d['not_usd']}",
           f"- excluded, extreme outcome (a label/simulation outcome above {MAX_OUTCOME_PCT:,.0f} %; "
           f"bogus pool data): {d.get('extreme', 0)}",
           f"- rows left before per-bucket outcome availability: {d['eligible']}\n",
           table(["bucket", "outcome not available yet", "usable rows"],
                 [[b, d["eligible"] - r["usable"], r["usable"]] for b, r in res["buckets"].items()]),
           "## Feature coverage (share of non-null values, all rows)\n",
           table(["column", "non-null"], [[c, v] for c, v in d["coverage"].items()])]
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
        s = r["split"]
        out.append(f"Split: train {s['n_train']} rows (before {s['t1']}), validation {s['n_val']}, "
                   f"test {s['n_test']} (from {s['t2']}); {s['n_dropped']} rows dropped by "
                   "embargo/token grouping.\n")
        out.append("### Class balance\n")
        out.append(table(["label", "positive rate (usable)", "train pos", "train neg", "note"],
                         [[lab, m["positive_rate"], m["train_pos"], m["train_neg"],
                           "; ".join(m["messages"])] for lab, m in r["labels"].items()]))
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
            [f"collapses removed by skipping top {SKIP_FRAC:.0%} collapse score",
             t["collapse_removed"], f">= {GATES['min_collapse_removed']:g}", g["collapse_ok"]],
            ["simulation beats buy-everything in every walk-forward week",
             f"{g['windows_beating']}/{g['windows_evaluated']}", "all", g["walk_forward_ok"]]]))
        out.append(f"Money simulation on test (mean net {cfg['ret_col']}, %): top {TOP_FRAC:.0%} "
                   f"by runner score = {fmt(t['sim_top_mean'], 1)} over {t['sim_n_top']} calls; "
                   f"all calls = {fmt(t['sim_all_mean'], 1)} (each call's net return capped at "
                   f"+{SIM_MAX_RET_PCT:,.0f} %).\n")
        out.append(f"**Bucket result: {'PASS' if g['passed'] else 'FAIL'}**\n")
        out.append("### Walk-forward (expanding weekly windows)\n")
        out.append(table(["week", "from", "train n", "test n", "runner AUC", "top lift",
                          "collapses removed", "top mean %", "all mean %", "beats all"],
                         [[w["week"], w["start"], w["n_train"], w["n_test"]]
                          + (["skipped: " + w["skipped"]] + [""] * 5 if w.get("skipped") else
                             [w["runner_auc"], w["top_lift"], w["collapse_removed"],
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
