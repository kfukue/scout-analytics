"""Render report.md from the results dict produced by train.py."""
import math

from . import config as C
from .config import (BUCKETS, CALIB_RECENT_DAYS, DEAD_COLUMN,
                     KFOLD_MIN_FOLDS, LABELS, LIFT_CI_Z, MAX_OUTCOME_PCT,
                     SIM_MAX_RET_PCT, SKIP_FRAC, TOP_FRAC, UNUSED_VIEW_COLUMNS)

# DEAD_TRADES_24H and RUNNER_MIN_TRADES_24H are read from config (C.) when a
# report is rendered, not bound at import, so the text always shows the
# current values.

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


def dead_line(d) -> str:
    """The data-section line on the dead-after-the-call rule."""
    policy = d.get("dead_policy", "off")
    head = (f"- dead after the call ({DEAD_COLUMN} < {C.DEAD_TRADES_24H}; kept in the data): "
            f"{d.get('dead', 0)} of {d.get('trades_24h_known', 0)} calls with a known {DEAD_COLUMN}")
    if policy == "on":
        return head + "; collapse label = collapse OR dead (every bucket; NULL keeps the plain label)"
    if policy == "missing":
        return (f"- **WARNING: dead-after-the-call rule SKIPPED: the data has no {DEAD_COLUMN} "
                "column** (the view on the server predates it; apply scoutanalytics.sql, see "
                "RUNBOOK.md). Collapse labels are the plain ones.")
    if not d.get("trades_24h_present", True):
        return (f"- dead after the call: n/a, the data has no {DEAD_COLUMN} column (the view on "
                "the server predates it; apply scoutanalytics.sql, see RUNBOOK.md). The dead "
                "rule is off anyway: plain collapse labels.")
    return (head + "; dead rule off (DEAD_IS_COLLAPSE = False, the default): plain collapse "
            "labels; dead calls are shown per bucket and scored separately at the end "
            "(report only)")


def plain_auc_line(r) -> str:
    """Collapse ROC AUC on the plain label (comparable with reports before the dead rule)."""
    a = r.get("collapse_auc_plain")
    if not a:
        return ""
    return (f"Collapse ROC AUC on the plain collapse label (test): LightGBM {fmt(a['lightgbm'])}, "
            f"logistic {fmt(a['logistic'])}.\n")


def dead_table(dead, trading, dead_on: bool) -> str:
    rows = [[part, v["rows"], v["dead"], v["dead_collapse"], v["collapse_rate_plain"],
             v["collapse_rate_with_dead"]] for part, v in dead.items()]
    head = table(["part", "calls", "dead", "dead and already a collapse",
                  "collapse rate (plain)", "collapse rate (collapse OR dead)"], rows)
    trained = ("model trained on collapse OR dead" if dead_on else
               "the dead rule is off, the model was trained on the plain label")
    text = (f"Collapses removed on test by the collapse scores ({trained}): "
            f"{fmt(trading.get('collapse_removed'))} of plain collapses (the gate)")
    if "collapse_removed_with_dead" not in trading:
        return head + text + ".\n"
    return (head + text + f"; bad outcomes removed incl. dead: "
            f"{fmt(trading.get('collapse_removed_with_dead'))} (information only, not a gate).\n")


RECENT_NA = "n/a (recent window cannot be fitted)"


def recent_unfitted(m) -> bool:
    """True when the recent window could not be fitted on the held-out rows: then
    there is no recent-window candidate (never the all-rows fallback under that name)."""
    c = m.get("calib") or {}
    return c.get("recent_fit", "recent") != "recent"


def calib_line(m) -> str:
    """Which Platt calibration was chosen (recent window or all held-out rows),
    the held-out comparison that chose it, and the test Brier of both (information)."""
    c = m.get("calib")
    if not c:
        return ""
    if c["used"] == "recent":
        used = (f"the recent window: the {c['rows_recent']} held-out rows from "
                f"{c['from']:%Y-%m-%d %H:%M} (the last {CALIB_RECENT_DAYS} days; "
                f"{c['pos_recent']} positives / {c['neg_recent']} negatives)")
    else:
        used = f"all {c['rows_all']} held-out rows"
    ch = c.get("choice")
    if ch:
        how = (f"Chosen on held-out rows only (never test rows): both fitted on the earliest "
               f"{ch['rows_fit']} held-out rows (recent window: {ch['rows_fit_recent']} of them), "
               f"Brier on the later {ch['rows_eval']} (from {ch['eval_from']:%Y-%m-%d %H:%M}): "
               f"recent window {fmt(ch['brier_recent'])} vs all held-out rows "
               f"{fmt(ch['brier_all'])}; the lower wins (equal: all held-out rows).")
    else:
        how = f"No comparison: {c['note']}."
    return (f"Platt calibration: **{'recent window' if c['used'] == 'recent' else 'all held-out rows'}"
            f"** (fitted on {used}). {how} Held-out rows in all: {c['rows_all']}, spanning "
            f"{c['span_days_all']:.1f} days. Brier on test (information only, never used to "
            f"choose): {fmt(m['lightgbm']['brier'])} (chosen); recent window "
            f"{RECENT_NA if recent_unfitted(m) else fmt(m.get('brier_recent'))}, all held-out "
            f"rows {fmt(m.get('brier_all_rows'))}. "
            "Deciles are by rank, so 'observed' is the same for all.\n")


def calib_table(m) -> str:
    before = {c["decile"]: c["mean_pred"] for c in m.get("calibration_all_rows") or []}
    recent = {c["decile"]: c["mean_pred"] for c in m.get("calibration_recent") or []}
    if not before:
        return table(["decile", "n", "mean predicted", "observed"],
                     [[c["decile"], c["n"], c["mean_pred"], c["observed"]]
                      for c in m["calibration"]])
    return table(["decile", "n", "mean predicted (chosen calibration)",
                  "mean predicted (recent window)", "mean predicted (all held-out rows)",
                  "observed"],
                 [[c["decile"], c["n"], c["mean_pred"],
                   RECENT_NA if recent_unfitted(m) else recent.get(c["decile"]),
                   before.get(c["decile"]), c["observed"]] for c in m["calibration"]])


def wf_rule_line() -> str:
    """The one-line rule under the gate tables (owner decision, 9 Oct 2026)."""
    return (f"Walk-forward weeks: a week counts toward the pooled walk-forward gates (and the old "
            f"\"beats buy-all in every week\" count, information only) only when its training "
            f"part has at least {C.WF_MIN_TRAIN_ROWS} rows (WF_MIN_TRAIN_ROWS, set on "
            f"{C.WF_MIN_TRAIN_ROWS_SET_ON}); weeks below are shown, marked \"not counted\".\n")


def _seeds_text() -> str:
    """C.STABILITY_SEEDS joined by " / " (e.g. "7 / 11 / 23")."""
    return " / ".join(str(s) for s in C.STABILITY_SEEDS)


def gate_rule_text() -> str:
    """How the verdict is decided (owner decision of 13 Oct 2026; thresholds read now)."""
    seeds = _seeds_text()
    return (f"Gates (owner decision of {C.GATES_SET_ON}): a bucket PASSes when all of these hold. "
            f"(1) Runner lift: the median over the LightGBM seeds {seeds} (STABILITY_SEEDS) of "
            f"the pooled expanding walk-forward top-{TOP_FRAC:.0%} lift >= "
            f"{C.GATES['min_top_lift']:g}, and the lower end of the 95% week-block bootstrap "
            f"interval of the saved seed's ({C.LGBM_SEED}) pooled lift > "
            f"{C.GATE_POOLED_LIFT_LOWER:g} (GATE_POOLED_LIFT_LOWER). (2) Beats buy-all: the lower "
            f"end of the 95% bootstrap interval of the pooled simulation difference (mean net "
            f"return of the per-week top {TOP_FRAC:.0%} minus that of all calls, each call capped "
            f"at +{SIM_MAX_RET_PCT:,.0f} %) > {C.GATE_SIM_DIFF_LOWER:g} (GATE_SIM_DIFF_LOWER). "
            f"(3) Collapses removed >= {C.GATES['min_collapse_removed']:.0%} on the test part "
            f"(saved model). Pooled = the test rows of all counted walk-forward weeks, each week "
            f"ranked by its own model; bootstrap: {C.POOLED_BOOTSTRAP_REPS} resamples of whole "
            f"weeks, seed {C.POOLED_BOOTSTRAP_SEED}. Fewer than {C.GATE_MIN_POOLED_WEEKS} counted "
            "weeks, a failed seed retrain or any undefined value: FAIL, with the reason listed "
            "under the table; "
            "never a PASS on missing data. The absolute return (pooled top-10% mean net return) "
            "and the old single-run gates (test-part lift, beats buy-all in every counted week) "
            "are information only.\n")


def _signed(v, digits=1) -> str:
    if v is None or (isinstance(v, float) and math.isnan(v)):
        return "n/a"
    return f"{v:+.{digits}f}"


def _signed_ci(v, lo, hi) -> str:
    return f"{_signed(v)} [{_signed(lo)}, {_signed(hi)}]"


def _seed_values(g) -> str:
    return " / ".join(fmt(v, 2) for v in (g.get("pooled_lift_seeds") or {}).values())


def gate_cells(g) -> list:
    """Verdict-table cells of a bucket with pooled gates (see V.pooled_gates)."""
    old = g.get("old") or {}
    top = g.get("abs_return_top")
    pos = g.get("abs_return_positive")
    return [f"{fmt(g['pooled_lift_ok'])}: {fmt(g['pooled_lift_median'], 2)} ({_seed_values(g)})",
            f"{fmt(g['pooled_lift_lower_ok'])}: {fmt(g['pooled_lift_lo'], 2)}",
            f"{fmt(g['sim_diff_ok'])}: "
            + _signed_ci(g["sim_diff"], g["sim_diff_lo"], g["sim_diff_hi"]),
            f"{fmt(g['collapse_ok'])}: {fmt(g['collapse_removed'])}",
            _signed(top) + ("" if pos is None else (" (> 0)" if pos else " (<= 0)")),
            f"{fmt(old.get('lift_ok'))}: {fmt(old.get('top_lift'), 2)}",
            wf_cell(old) if old else "n/a"]


def verdict_headers() -> list:
    return ["bucket", "result",
            f"runner lift: median pooled WF lift >= {C.GATES['min_top_lift']:g} "
            f"(seeds {_seeds_text()})",
            f"pooled lift 95% lower end > {C.GATE_POOLED_LIFT_LOWER:g} (seed {C.LGBM_SEED})",
            f"beats buy-all: pooled sim top {TOP_FRAC:.0%} minus all (%) [95%], lower end > "
            f"{C.GATE_SIM_DIFF_LOWER:g}",
            f"collapses removed >= {C.GATES['min_collapse_removed']:.0%} (test)",
            "absolute return: pooled top-10% mean net % (information, not a gate)",
            f"old gate, information only: test-part lift >= {C.GATES['min_top_lift']:g}",
            "old gate, information only: beats buy-all in every counted week"]


def reasons_lines(buckets) -> str:
    """One line per FAILing bucket with the reasons of its gates."""
    lines = [f"- `{b}` FAIL: " + "; ".join(r["gates"].get("reasons") or ["see the gate table"])
             for b, r in buckets.items() if not r.get("skipped") and not r["gates"]["passed"]]
    return ("\n".join(lines) + "\n") if lines else ""


def wf_not_counted_mark() -> str:
    return f"not counted (train < {C.WF_MIN_TRAIN_ROWS})"


def wf_gate_note(w) -> str:
    """Per walk-forward week: counted toward the gate, or not (train too small)."""
    if w.get("skipped"):
        return "-"
    if not w.get("enough_train", True):
        return wf_not_counted_mark()
    return "counted"


def wf_cell(g) -> str:
    """Verdict cell: ok, counted weeks beating / counted, and the weeks not counted."""
    text = f"{fmt(g['walk_forward_ok'])} ({g['windows_beating']}/{g['windows_evaluated']} weeks"
    if g.get("windows_not_counted"):
        text += f"; {g['windows_not_counted']} not counted"
    return text + ")"


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
           table(verdict_headers(),
                 [[b, "skipped" if r.get("skipped") else ("PASS" if r["gates"]["passed"] else "FAIL")]
                  + ([""] * 7 if r.get("skipped") else gate_cells(r["gates"]))
                  for b, r in res["buckets"].items()]),
           reasons_lines(res["buckets"]),
           gate_rule_text(),
           wf_rule_line(),
           ("The saved model and these gates are the baseline configuration. Variant comparison "
            "(report only): see the last section.\n" if res.get("variants") else ""),
           "## Data and exclusions\n",
           f"- rows read: {d['rows']} ({d['first_date']} to {d['last_date']})",
           f"- excluded, update post (not a call): {d.get('update', 0)}",
           f"- excluded, repeat call (not the token's first call; only first calls are tracked): {d.get('repeat', 0)}",
           f"- excluded, no pool (never tradable; out of training and simulation): {d['no_pool']}",
           f"- excluded, price_unit not 'usd': {d['not_usd']}",
           f"- excluded, extreme outcome (a label/simulation outcome above {MAX_OUTCOME_PCT:,.0f} %; "
           f"bogus pool data): {d.get('extreme', 0)}",
           dead_line(d),
           f"- rows left before per-bucket outcome availability: {d['eligible']}\n",
           table(["bucket", "not labelled yet (horizon not due or no data; not a negative)", "usable rows"],
                 [[b, d["eligible"] - r["usable"], r["usable"]] for b, r in res["buckets"].items()]),
           "## Feature coverage (share of non-null values, rows left after the exclusions above)\n",
           table(["column", "non-null"], [[c, v] for c, v in d["coverage"].items()]),
           f"Not model inputs (always 0 / NULL for a first call): {', '.join(UNUSED_VIEW_COLUMNS)}.\n"]
    for b, r in res["buckets"].items():
        cfg = BUCKETS[b]
        out.append(f"## Bucket `{b}` ({cfg['horizon_days']}d horizon)\n")
        dead_on = d.get("dead_policy") == "on"
        tradeable = d.get("runner_min_trades_24h")
        out.append("Labels (net of tax): " + "; ".join(
            f"{lab} = {cfg[lab][0]} {cfg[lab][1]} {cfg[lab][2]:g}%"
            + (f" AND {DEAD_COLUMN} >= {tradeable} (NULL keeps the label)"
               if lab == "runner" and tradeable else "")
            + (" OR rugged" if lab == "collapse" and cfg["collapse_or_rugged"] else "")
            + (f" OR dead ({DEAD_COLUMN} < {C.DEAD_TRADES_24H})" if lab == "collapse" and dead_on else "")
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
        old = g.get("old") or g
        out.append(table(["check (LightGBM)", "value", "needed", "ok"], [
            [f"runner lift: median over seeds {_seeds_text()} of the pooled walk-forward top-"
             f"{TOP_FRAC:.0%} lift ({g.get('pooled_weeks', 0)} counted weeks; per seed)",
             f"{fmt(g.get('pooled_lift_median'), 2)} ({_seed_values(g)})",
             f">= {C.GATES['min_top_lift']:g}", g.get("pooled_lift_ok")],
            [f"  pooled lift of the saved seed ({C.LGBM_SEED}) [95% week-block bootstrap]",
             _ci(g.get("pooled_lift"), g.get("pooled_lift_lo"), g.get("pooled_lift_hi")),
             f"lower end > {C.GATE_POOLED_LIFT_LOWER:g}", g.get("pooled_lift_lower_ok")],
            [f"beats buy-all: pooled simulation, top {TOP_FRAC:.0%} minus all calls, mean net % "
             "[95% week-block bootstrap]",
             _signed_ci(g.get("sim_diff"), g.get("sim_diff_lo"), g.get("sim_diff_hi")),
             f"lower end > {C.GATE_SIM_DIFF_LOWER:g}", g.get("sim_diff_ok")],
            [f"collapses removed by skipping top {SKIP_FRAC:.0%} collapse score (test part)",
             t["collapse_removed"], f">= {C.GATES['min_collapse_removed']:g}", g["collapse_ok"]]]
            + ([["  bad outcomes removed incl. dead (collapse OR dead; information, not a gate)",
                 t["collapse_removed_with_dead"], "", ""]]
               if "collapse_removed_with_dead" in t else [])
            + [[f"absolute return: pooled top-{TOP_FRAC:.0%} mean net % (vs all calls; "
                "information, not a gate)",
                f"{_signed(g.get('abs_return_top'))} (all calls {_signed(g.get('abs_return_all'))})",
                "", ""],
               [f"old gate, information only: runner lift, top {TOP_FRAC:.0%} by runner score "
                "(test part)", t["top_lift"], f">= {C.GATES['min_top_lift']:g}", old["lift_ok"]],
               ["  95% interval of that lift; runners top/calls top of runners/calls",
                f"{lift_ci(t)}; {lift_counts(t)}", "", ""],
               ["old gate, information only: simulation beats buy-everything in every counted "
                "walk-forward week",
                f"{old['windows_beating']}/{old['windows_evaluated']}"
                + (f" ({old['windows_not_counted']} not counted)" if old.get("windows_not_counted")
                   else ""), "all", old["walk_forward_ok"]]]))
        if not g["passed"] and g.get("reasons"):
            out.append("Why FAIL: " + "; ".join(g["reasons"]) + ".\n")
        out.append(wf_rule_line())
        out.append(f"Money simulation on test (mean net {cfg['ret_col']}, %): top {TOP_FRAC:.0%} "
                   f"by runner score = {fmt(t['sim_top_mean'], 1)} over {t['sim_n_top']} calls; "
                   f"all calls = {fmt(t['sim_all_mean'], 1)} (each call's net return capped at "
                   f"+{SIM_MAX_RET_PCT:,.0f} %).\n")
        out.append(LIFT_METHOD + "\n")
        if r.get("dead") and d.get("dead_policy") != "missing":
            out.append(f"### Dead after the call ({DEAD_COLUMN} < {C.DEAD_TRADES_24H})\n")
            out.append(dead_table(r["dead"], t, dead_on))
            out.append(plain_auc_line(r))
        out.append(f"**Bucket result: {'PASS' if g['passed'] else 'FAIL'}**\n")
        out.append("### Walk-forward (expanding weekly windows)\n")
        wd = "collapse_removed_with_dead" in t   # dead rule on: one information column more
        out.append(table(["week", "from", "train n", "test n", "runner AUC", "top lift",
                          "lift 95% interval", "runners top/n of all/n", "collapses removed"]
                         + (["incl. dead (info, not a gate)"] if wd else [])
                         + ["top mean %", "all mean %", "beats all", "gate"],
                         [[w["week"], w["start"], w["n_train"], w["n_test"]]
                          + (["skipped: " + w["skipped"]] + [""] * (8 if wd else 7)
                             if w.get("skipped") else
                             [w["runner_auc"], w["top_lift"], lift_ci(w), lift_counts(w),
                              w["collapse_removed"]]
                             + ([w.get("collapse_removed_with_dead")] if wd else [])
                             + [fmt(w["sim_top_mean"], 1), fmt(w["sim_all_mean"], 1),
                                w["sim_beats_all"]])
                          + [wf_gate_note(w)] for w in r["walk_forward"]]))
        for lab, m in r["labels"].items():
            if m.get("skipped"):
                continue
            out.append(f"### `{lab}`: calibration by decile (test) and top features (gain)\n")
            if m.get("calib"):
                out.append(calib_line(m))
            out.append(calib_table(m))
            out.append(table(["feature", "gain share"], m["importance"]))
    if "dead_score" in res:
        out.append(dead_score_section(res["dead_score"]))
    if res.get("diagnostics"):
        out.append(diagnostics_section(res["diagnostics"], res["buckets"]))
    if res.get("variants"):
        out.append(variants_section(res["variants"], res["buckets"], res.get("wallet_rep")))
    return "\n".join(out)


DEAD_SCORE_TITLE = "## Dead after the call: separate score (report only; not saved, not served)\n"


def _pct_ci(v, lo, hi) -> str:
    return f"{fmt(v)} [{fmt(lo)}, {fmt(hi)}]"


def dead_score_section(ds: dict) -> str:
    """The separate dead-after-the-call score (see train.dead_score)."""
    out = [DEAD_SCORE_TITLE]
    if ds.get("error"):
        return "\n".join(out + [f"**Not computed: {ds['error']}.** The saved models, their "
                                "gates and everything above are not affected.\n"])
    if ds.get("not_applicable"):
        return "\n".join(out + [f"n/a: {ds['not_applicable']} (the view on the server predates "
                                "it; apply scoutanalytics.sql, see RUNBOOK.md).\n"])
    b, thr = ds["bucket"], ds["threshold"]
    out.append(
        f"Label: dead = {DEAD_COLUMN} < {thr} (fewer than {thr} price events in the 24 h after "
        f"the call). Rows: the usable rows of the `{b}` bucket with a known {DEAD_COLUMN} "
        f"({ds['known']} of {ds['usable']}; {ds['null_excluded']} with NULL {DEAD_COLUMN} left "
        f"out, not counted as negatives). Inputs: the same features as the saved models "
        f"({DEAD_COLUMN} is the label only, never an input). Split: the `{b}` bucket's time "
        f"split (train before {ds['split']['t1']}, validation, test from {ds['split']['t2']}; "
        f"{ds['horizon_days']:g}-day embargo) restricted to those rows. Trained once, not per "
        "bucket. **Not saved, not served, no gates.**\n"
        if ds.get("split") else
        f"Label: dead = {DEAD_COLUMN} < {thr}; rows: {ds['known']} of {ds['usable']} usable "
        f"`{b}` rows have a known {DEAD_COLUMN}.\n")
    if ds.get("parts"):
        out.append(table(["part", "calls", "dead", "base rate"],
                         [[k, v["n"], v["dead"], v["base_rate"]] for k, v in ds["parts"].items()]))
    if ds.get("waves"):
        out.append(waves_line(ds["waves"]))
    if ds.get("skipped"):
        out.append(f"**SKIPPED:** {ds['skipped']}\n")
        return "\n".join(out)
    m = ds["label"]
    out.append("### Test metrics\n")
    out.append(table(["model", "n", "base rate", "ROC AUC", "PR AUC", "Brier"],
                     [[name, m[name]["n"], m[name]["base_rate"], m[name]["roc_auc"],
                       m[name]["pr_auc"], m[name]["brier"]] for name in ("lightgbm", "logistic")]))
    if m.get("messages"):
        out.append("Notes: " + "; ".join(m["messages"]) + "\n")
    out.append("### Skipping the calls with the highest dead score (test part)\n")
    out.append(table(["model", "top share skipped", "calls", "dead among them",
                      "precision [95% Wilson]", "lift over base rate [95%]",
                      "share of all dead calls caught"],
                     [[name, f"{t['frac']:.0%}", t["n_top"], t["dead_top"],
                       _pct_ci(t["precision"], t["precision_lo"], t["precision_hi"]),
                       f"{fmt(t['lift'], 2)} [{fmt(t['lift_lo'], 2)}, {fmt(t['lift_hi'], 2)}]",
                       t["caught"]] for name in ("lightgbm", "logistic")
                      for t in ds["top"][name]]))
    out.append("Precision = share of dead calls among the skipped ones; lift = precision / base "
               "rate of the test part (interval: the Wilson interval divided by the base rate, "
               "treated as fixed).\n")
    out.append("### Calibration by decile (test, LightGBM)\n")
    if m.get("calib"):
        out.append(calib_line(m))
    out.append(calib_table(m))
    out.append("### What drives the score\n")
    out.append(table(["feature (LightGBM)", "gain share"], m["importance"]))
    out.append(table(["feature (logistic)", "coefficient", "input"],
                     [[n, c, s] for n, c, s in ds["logistic_coefs"]]))
    out.append("Logistic coefficients are per standard deviation of the standardised input "
               "(log1p(x) for skewed columns, x as is for the others; missing values median-"
               "imputed); positive = more likely dead. Only numeric inputs.\n")
    out.append(dead_walk_forward_table(ds["walk_forward"]))
    out.append(proposal_text(ds["proposal"]))
    return "\n".join(out)


def waves_line(waves: list) -> str:
    """The weeks with the most dead calls; the "few waves" caveat only when the
    C.DEAD_WAVE_TOP_WEEKS top weeks hold more than C.DEAD_WAVE_SHARE of them."""
    total = sum(w["dead"] for w in waves)
    top = waves[:C.DEAD_WAVE_TOP_WEEKS]
    k = sum(w["dead"] for w in top)
    weeks = "; ".join(f"week of {w['week']}: {w['dead']} of {w['calls']} calls" for w in top)
    share = k / total if total else 0.0
    head = (f"Dead calls by week: the {len(top)} weeks with the most hold {k} of {total} "
            f"({share:.0%}; {weeks}). ")
    if total and share > C.DEAD_WAVE_SHARE:
        fam = {}
        for w in top:
            for f, n in w.get("dead_families", {}).items():
                fam[f] = fam.get(f, 0) + n
        named = ""
        if fam:
            f, n = sorted(fam.items(), key=lambda kv: (-kv[1], kv[0]))[0]
            named = f"; most common dex_family among them: \"{f}\", {n} of {k}"
        return (head + f"**Most dead calls came in a few waves (more than "
                f"{C.DEAD_WAVE_SHARE:.0%} in {len(top)} weeks{named}), so the test part and the "
                "walk-forward numbers depend on those weeks; a score that recognises one wave "
                "need not recognise the next.**\n")
    return (head + f"Dead calls are spread over the weeks (the top {len(top)} hold no more "
            f"than {C.DEAD_WAVE_SHARE:.0%} of them).\n")


def dead_walk_forward_table(weeks: list) -> str:
    rows = []
    for w in weeks:
        marks = ([f"< {C.DEAD_SCORE_MIN_WEEK_DEAD} dead: too few to judge"] if w["few_dead"] else [])
        if not w.get("enough_train", True):
            marks.append(wf_not_counted_mark())
        mark = "; ".join(marks)
        head = [w["week"], w["start"], w["n_train"], w["n_test"], w["n_dead"]]
        if w.get("skipped"):
            rows.append(head + ["skipped: " + w["skipped"]] + [""] * 6 + [mark])
            continue
        lg, lo = w["lightgbm"], w["logistic"]
        caught = [lg["caught"].get(f"{f:g}") for f in C.DEAD_SCORE_TOP_FRACS]
        rows.append(head + [lg["roc_auc"], lo["roc_auc"], w["base_rate"], lg["top10_precision"],
                            lo["top10_precision"], " / ".join(fmt(c) for c in caught), mark])
    fr = " / ".join(f"{f:.0%}" for f in C.DEAD_SCORE_TOP_FRACS)
    gf = f"{C.DEAD_SCORE_GATE_FRAC:.0%}"
    return ("### Walk-forward (expanding weekly windows)\n\n"
            + table(["week", "from", "train n", "test n", "dead n", "AUC LightGBM",
                     "AUC logistic", "base rate", f"top-{gf} precision LightGBM",
                     f"top-{gf} precision logistic", f"dead caught, skipping top {fr} (LightGBM)",
                     "note"], rows))


def proposal_text(p: dict) -> str:
    lift = p["min_lift"]
    lines = [f"**Proposed gates (a proposal for the product manager, NOT applied; nothing here "
             f"passes or fails):** top-{C.DEAD_SCORE_GATE_FRAC:.0%} precision >= {lift:g}x the "
             f"base rate in the test part, "
             f"and in more than half of the walk-forward weeks with at least "
             f"{C.DEAD_SCORE_MIN_WEEK_DEAD} dead calls and at least "
             f"{p.get('wf_min_train_rows', C.WF_MIN_TRAIN_ROWS)} training rows (weeks marked "
             "\"not counted\" are left out, as for the bucket gates' walk-forward weeks; unlike "
             f"the bucket gates since {C.GATES_SET_ON}, which pool the weeks, this proposal "
             "still judges week by week). On this run:\n"]
    for model in ("lightgbm", "logistic"):
        q = p[model]
        lines.append(f"- {model}: test lift {fmt(q['test_lift'], 2)} ({'meets' if q['test_ok'] else 'below'} "
                     f"{lift:g}); weeks meeting it {q['weeks_meeting']}/{q['weeks_judged']}; "
                     f"would {'pass' if q['would_pass'] else 'not pass'} the proposal")
    return "\n".join(lines) + "\n"


DIAGNOSTICS_TITLE = ("## Stability diagnostics (not saved, not served; only the seeds' pooled lifts "
                     "and the pooled expanding walk-forward feed the gates)\n")


def _ci(v, lo, hi, digits=2) -> str:
    return f"{fmt(v, digits)} [{fmt(lo, digits)}, {fmt(hi, digits)}]"


def _part_error(part) -> str:
    """'' or the line for a diagnostics part that failed / does not apply."""
    if not isinstance(part, dict):
        return "n/a\n"
    if part.get("error"):
        return f"**Not computed: {part['error']}.** Nothing else in this report is affected.\n"
    if part.get("not_applicable"):
        return f"n/a: {part['not_applicable']}.\n"
    if part.get("off"):
        return f"Switched off ({part['off']}).\n"
    return ""


def diagnostics_section(dg: dict, buckets: dict) -> str:
    """The report-only stability diagnostics (see train.run_diagnostics)."""
    out = [DIAGNOSTICS_TITLE]
    if dg.get("error"):
        return "\n".join(out + [f"**Not computed: {dg['error']}.** The saved models, their "
                                "gates and everything above are not affected.\n"])
    st = dg["settings"]
    secs = dg.get("seconds") or {}
    out.append(
        "These sections help tell whether the results above are real or noise. Nothing here is "
        "saved or served. Since the owner decision of "
        f"{C.GATES_SET_ON}, the pooled walk-forward lift of each seed (\"Seed stability\") and "
        "the pooled expanding walk-forward with its bootstrap interval (\"Pooled walk-forward\") "
        "are the inputs of the gates above; they are computed in every training, also with this "
        "section switched off, and the verdict is the same either way. Everything else here "
        "(the seed table's other columns and its single-run gate column, the rolling "
        "walk-forward, the evaluation without dead calls, adversarial validation) is report "
        f"only and passes or fails nothing. Switch the section off for a run with "
        f"{C.DIAGNOSTICS_ENV}=0 (config DIAGNOSTICS). Time (seeds = the seed retrains, done for "
        "the gates): "
        + "; ".join(f"{k} {v:.0f} s" for k, v in secs.items())
        + f" (total {sum(secs.values()):.0f} s).\n")
    out.append(seed_section(dg.get("seeds"), buckets))
    out.append(pooled_section(dg.get("walk_forward"), st, buckets))
    out.append(no_dead_section(dg.get("no_dead"), buckets))
    out.append(adversarial_section(dg.get("adversarial"), st, buckets))
    return "\n".join(out)


def seed_section(sd, buckets) -> str:
    out = ["### Seed stability\n"]
    err = _part_error(sd)
    if err:
        return "\n".join(out + [err])
    seeds = ", ".join(str(s) for s in sd["seeds"])
    out.append(
        f"The baseline retrained with LightGBM seeds {seeds} (LGBM_SEED; STABILITY_SEEDS); seed "
        f"{sd['saved_seed']} is the saved model (its numbers are reused, not retrained). Only the "
        "seed changes (bagging and feature sampling, and so the rounds from early stopping / the "
        "purged k-fold): same rows, splits, labels and walk-forward weeks. If the lift or the "
        "pass/fail moves a lot between seeds, a single run's number is mostly noise. Counted "
        "weeks beating = walk-forward weeks beating buy-all (counted weeks only); "
        "pooled WF lift = see \"Pooled walk-forward\" below: its median over the seeds is the "
        f"runner-lift gate (>= {C.GATES['min_top_lift']:g}). The last column is the old "
        "single-run gate of each run (test-part lift, collapses removed, every counted week "
        "beating buy-all): information only.\n")
    head = ["run", "top-10% lift [95%]", "collapses removed", "runner AUC",
            "sim top 10% vs all (%)", "counted weeks beating buy-all",
            "pooled WF lift (gate input)", "single-run (old) gate, information"]
    for b in buckets:
        out.append(f"#### Bucket `{b}`\n")
        if buckets[b].get("skipped"):
            out.append(f"n/a: {buckets[b]['skipped']}\n")
            continue
        rows = []
        for seed, run in sd["runs"].items():
            name = f"seed {seed}" + (" (saved model)" if run.get("saved_model") else "")
            if run.get("error"):
                rows.append([name, run["error"]] + [""] * 6)
                continue
            s = (run.get("buckets") or {}).get(b)
            if s is None or s.get("skipped"):
                rows.append([name, f"n/a: {(s or {}).get('skipped', 'not trained')}"] + [""] * 6)
                continue
            rows.append([name, _ci(s["top_lift"], s["top_lift_lo"], s["top_lift_hi"]),
                         s["collapse_removed"], s["runner_auc"],
                         f"{fmt(s['sim_top_mean'], 1)} vs {fmt(s['sim_all_mean'], 1)}",
                         f"{s['windows_beating']}/{s['windows_evaluated']}",
                         fmt(s["wf_pooled_lift"], 2), "PASS" if s["passed"] else "FAIL"])
        sp = (sd.get("spread") or {}).get(b)
        if sp:
            for q in ("min", "median", "max"):
                rows.append([f"**{q}**", fmt(sp["top_lift"][q], 2), sp["collapse_removed"][q],
                             sp["runner_auc"][q],
                             f"{fmt(sp['sim_top_mean'][q], 1)} vs {fmt(sp['sim_all_mean'][q], 1)}",
                             fmt(sp["windows_beating"][q], 1), fmt(sp["wf_pooled_lift"][q], 2), ""])
        out.append(table(head, rows))
        if sp:
            out.append("min / median / max are over the seeds per column (the sim row takes the "
                       "min / median / max of each mean separately).\n")
    return "\n".join(out)


def _pooled_cells(p) -> list:
    if not p or not p.get("weeks"):
        return ["0", "0", "n/a (no counted week)", "", "", "", ""]
    return [p["weeks"], p["rows"],
            _ci(p.get("lift"), p.get("lift_lo"), p.get("lift_hi")),
            f"{p.get('top_pos', 0)}/{p.get('top_n', 0)} of {p.get('all_pos', 0)}/{p.get('all_n', 0)}",
            _ci(p.get("collapse_removed"), p.get("collapse_removed_lo"),
                p.get("collapse_removed_hi"), 3),
            f"{fmt(p.get('sim_top_mean'), 1)} vs {fmt(p.get('sim_all_mean'), 1)}; difference "
            + _ci(p.get("sim_gain"), p.get("sim_gain_lo"), p.get("sim_gain_hi"), 1),
            f"{p.get('weeks_beating', 0)}/{p['weeks']}"]


POOLED_HEAD = ["walk-forward", "counted weeks", "test rows", "pooled top-10% lift [95% week bootstrap]",
               "runners top/n of all/n", "pooled collapses removed [95%]",
               "pooled sim top 10% vs all (%); difference [95%]", "weeks beating buy-all"]


def _week_cell(x, k, d=3) -> str:
    """One walk-forward week's value `k` in the per-week table."""
    return "skipped" if x.get("skipped") else fmt(x.get(k), d)


def _week_mark(x) -> str:
    """Whether a walk-forward week is in the pooled weeks ("-": no such week / skipped)."""
    if not x or x.get("skipped"):
        return "-"
    return "yes" if x.get("counted") else "no"


def pooled_section(wd, st, buckets) -> str:
    out = ["### Pooled walk-forward (expanding and rolling)\n"]
    err = _part_error(wd)
    if err:
        return "\n".join(out + [err])
    out.append(
        "The expanding row is a gate input (see Verdict): the lower end of its lift interval and "
        "the lower end of its simulation-difference interval are gates; the rolling row is "
        "report only. "
        "Pooled = the test rows of all counted walk-forward weeks of a bucket together (counted: "
        f"training part of at least {C.WF_MIN_TRAIN_ROWS} rows). Each week is ranked "
        "by its own model (the top 10% by runner score and the top 30% by collapse score are taken "
        "per week: the week models' scores are not on one scale); pooled lift = "
        "runner rate in the per-week tops / runner rate of all pooled rows. 95% interval: "
        f"week-block bootstrap ({st['reps']} resamples of the weeks with replacement, seed "
        f"{st['bootstrap_seed']}); with few weeks it is rough, with one week n/a. Expanding = the "
        f"gate's walk-forward (all calls before the week); rolling = trained only on the calls of "
        f"the last {st['rolling_weeks']} weeks before the week (minus the embargo; "
        "WF_ROLLING_WEEKS), same rounds and test weeks. Both are pooled over the same weeks, "
        "the ones the expanding walk-forward counts, minus any week the rolling model could not "
        "train (marked in the per-week table) (a paired comparison; the rolling window's "
        f"own training size, shown in the table, is not held to the {C.WF_MIN_TRAIN_ROWS}-row "
        "rule). If rolling is clearly better, old weeks hurt (drift); if clearly worse, the model "
        "needs the longer history (the rolling model uses the expanding model's round count, so "
        "part of a gap can come from that).\n")
    for b in buckets:
        out.append(f"#### Bucket `{b}`\n")
        if buckets[b].get("skipped") or b not in wd:
            out.append(f"n/a: {buckets[b].get('skipped', 'not trained')}\n")
            continue
        w = wd[b]
        out.append(table(POOLED_HEAD, [["expanding (the gate's)"] + _pooled_cells(w["expanding"]),
                                       [f"rolling ({w['rolling_weeks']} weeks)"]
                                       + _pooled_cells(w["rolling"])]))
        rows = []
        for r in w["weeks"]:
            e, ro = r["expanding"], r["rolling"]
            rows.append([r["week"], r["start"], f"{e.get('n_train', '')} / {ro.get('n_train', '')}",
                         e.get("n_test", ""),
                         f"{_week_cell(e, 'top_lift', 2)} / {_week_cell(ro, 'top_lift', 2)}",
                         f"{_week_cell(e, 'collapse_removed')} / {_week_cell(ro, 'collapse_removed')}",
                         f"{_week_cell(e, 'sim_beats_all')} / {_week_cell(ro, 'sim_beats_all')}",
                         f"{_week_mark(e)} / {_week_mark(ro)}"])
        out.append(table(["week", "from", "train n (expanding / rolling)", "test n",
                          "top lift (expanding / rolling)",
                          "collapses removed (expanding / rolling)",
                          "beats all (expanding / rolling)", "in the pooled weeks (expanding / rolling)"],
                         rows))
    return "\n".join(out)


def no_dead_section(nd, buckets) -> str:
    out = [f"### Evaluation without dead calls ({DEAD_COLUMN} < {C.DEAD_TRADES_24H})\n"]
    err = _part_error(nd)
    if err:
        return "\n".join(out + [err])
    out.append(
        "The same trained models (saved model on the test part, the gate's walk-forward week "
        "models), evaluated on the calls that were not dead after the call (EVAL_EXCLUDE_DEAD; "
        f"NULL {DEAD_COLUMN} is kept). The top 10% / 30% are re-taken among those calls. "
        "Evaluation only: training rows, labels, gates and the verdict are unchanged. Walk-forward: "
        "counted weeks that still have at least "
        f"{C.MIN_WINDOW_TEST_ROWS} test rows without the dead calls; pooled as in \"Pooled "
        "walk-forward\".\n")
    head = ["test rows", "calls", "top-10% lift [95%]", "runners top/n of all/n",
            "collapses removed", "runner AUC", "sim top 10% vs all (%)",
            "walk-forward weeks beating buy-all", "pooled WF lift [95%]",
            "pooled WF collapses removed"]
    for b in buckets:
        out.append(f"#### Bucket `{b}`\n")
        if buckets[b].get("skipped") or b not in nd:
            out.append(f"n/a: {buckets[b].get('skipped', 'not trained')}\n")
            continue
        rows = []
        for name, r in nd[b]["rows"].items():
            p = r["wf_pooled"]
            rows.append([name, r["n_test"], _ci(r["top_lift"], r["top_lift_lo"], r["top_lift_hi"]),
                         lift_counts(r), r["collapse_removed"], r["runner_auc"],
                         f"{fmt(r['sim_top_mean'], 1)} vs {fmt(r['sim_all_mean'], 1)}",
                         f"{r['wf_weeks_beating']}/{r['wf_weeks']}",
                         _ci(p["lift"], p["lift_lo"], p["lift_hi"]), p["collapse_removed"]])
        out.append(f"Dead calls in the test part: {nd[b]['dead_test']}.\n")
        out.append(table(head, rows))
    return "\n".join(out)


def adversarial_section(ad, st, buckets) -> str:
    out = ["### Adversarial validation (can the test part be told from the train part?)\n"]
    err = _part_error(ad)
    if err:
        return "\n".join(out + [err])
    out.append(
        "A LightGBM classifier is trained to tell test rows from train rows on the model's own "
        f"inputs (the same features, without the calendar inputs {', '.join(C.ADV_EXCLUDE)}: "
        "a test part covering only part of a week differs there by construction; outcomes and "
        f"{DEAD_COLUMN} are never inputs). ROC AUC out of "
        f"fold ({st['adv_folds']}-fold, stratified, shuffled with a fixed seed): about 0.5 = the "
        "test calls look like the training calls; well above 0.5 (e.g. > 0.7) = the inputs "
        "drifted, and the features listed (gain share) are the ones that changed most. A category "
        "level that appears only in the test period shows up as drift too (it is missing for the "
        "model). Information only.\n")
    rows = []
    for b in buckets:
        if buckets[b].get("skipped") or b not in ad:
            rows.append([b, "", "", f"n/a: {buckets[b].get('skipped', 'not trained')}", ""])
            continue
        s = ad[b]["split"]
        rows.append([b, s["n_train"], s["n_test"],
                     f"skipped: {s['skipped']}" if s.get("skipped") else s["auc"],
                     ", ".join(f"{f} {g:.2f}" for f, g in s["top"])])
    out.append(table(["bucket", "train n", "test n", "AUC (train vs test)",
                      "top drifting features (gain share)"], rows))
    if st.get("adv_per_week"):
        out.append(f"Per counted walk-forward week (the newest {C.ADV_MAX_WEEKS}; that week's "
                   "training rows vs its test rows, the expanding windows of the gate; "
                   f"{C.ADV_WEEK_FOLDS}-fold):\n")
        for b in buckets:
            weeks = (ad.get(b) or {}).get("weeks") if not buckets[b].get("skipped") else None
            if not weeks:
                continue
            out.append(f"#### Bucket `{b}`\n")
            out.append(table(["week", "from", "train n", "test n", "AUC", "top 3 features"],
                             [[w["week"], w["start"], w["n_train"], w["n_test"],
                               f"skipped: {w['skipped']}" if w.get("skipped") else w["auc"],
                               ", ".join(f"{f} {g:.2f}" for f, g in w["top"][:3])]
                              for w in weeks]))
    return "\n".join(out)


VARIANT_NOTE = (
    "Each variant changes exactly one setting of the baseline (the saved model) and is trained "
    "on the same rows, splits and seed. Variants are report only: not saved, not served, and "
    "never a gate. Their gate column is the old single-run gate (test-part lift, collapses "
    "removed, every counted week beating buy-all), information only, also for the baseline row; "
    "the verdict above uses the pooled gates, which would need each variant retrained with every "
    "seed. The pooled walk-forward column is each variant's own pooled walk-forward (its "
    "LGBM_SEED run only), comparable with the baseline's. **All variants are compared on the same test "
    "part, so choosing the best one by these numbers alone risks fitting the test period; prefer "
    "a variant that also wins in the walk-forward weeks** (weeks beating buy-all, walk-forward "
    "mean lift and mean collapses removed).")


def variant_read() -> str:
    """How to read the variant tables (thresholds read at render time)."""
    return (
        "How to read: lift and runner AUC are measured on each variant's own runner label, so for "
        f"\"runners must be tradeable\" (runner also needs {DEAD_COLUMN} >= {C.RUNNER_MIN_TRADES_24H}) "
        "they use a different yardstick (see its runner rate); its money simulation still uses every "
        "call. \"Collapses removed\" (the gate) and \"collapse AUC, plain label\" are measured on "
        "the plain collapse label in every variant and are comparable; \"collapse AUC, trained "
        "label\" uses the label each variant trained on (plain, or collapse OR dead with the dead "
        "rule on). The logistic baseline uses numeric inputs only, so its numbers do not change "
        "between the DEX variants. Sim = mean net return (%) of the top 10% by runner score vs all "
        "calls of the test part. The walk-forward columns use the counted weeks only (training "
        f"part of at least {C.WF_MIN_TRAIN_ROWS} rows), as the gates do.")


def variant_error(exc: BaseException) -> str:
    """'error: <Type>: <first line>' (short, safe in a markdown table cell)."""
    msg = (str(exc).strip().splitlines() or [""])[0]
    text = f"{type(exc).__name__}: {msg}" if msg else type(exc).__name__
    if len(text) > 150:
        text = text[:147] + "..."
    return "error: " + text.replace("|", "/")


def _pair(a, b) -> str:
    return f"{fmt(a)} / {fmt(b)}"


def variant_table(variants: dict, bucket: str) -> str:
    rows = []
    for name, v in variants.items():
        changes = ", ".join(f"{k} = {val}" for k, val in v["changes"].items()) or "(as configured)"
        if v.get("error"):
            rows.append([name, changes, v["error"]] + [""] * 11)
            continue
        s = (v.get("buckets") or {}).get(bucket)
        if v.get("not_applicable") or s is None or s.get("skipped"):
            why = v.get("not_applicable") or (s or {}).get("skipped") or "not trained"
            rows.append([name, changes, f"n/a: {why}"] + [""] * 11)
            continue
        rows.append([
            name, changes, s["runner_rate_test"],
            f"{fmt(s['top_lift'], 2)} [{fmt(s['top_lift_lo'], 2)}, {fmt(s['top_lift_hi'], 2)}]",
            _pair(s["runner_auc_lightgbm"], s["runner_auc_logistic"]),
            _pair(s["collapse_auc_plain_lightgbm"], s["collapse_auc_plain_logistic"]),
            _pair(s["collapse_auc_trained_lightgbm"], s["collapse_auc_trained_logistic"]),
            s["collapse_removed"],
            f"{s['windows_beating']}/{s['windows_evaluated']}",
            fmt(s["wf_mean_lift"], 2), s["wf_mean_collapse_removed"],
            f"{fmt(s['sim_top_mean'], 1)} vs {fmt(s['sim_all_mean'], 1)}",
            _ci(s.get("wf_pooled_lift"), s.get("wf_pooled_lift_lo"), s.get("wf_pooled_lift_hi"))
            + "; " + _signed_ci(s.get("wf_pooled_sim_gain"), s.get("wf_pooled_sim_gain_lo"),
                                s.get("wf_pooled_sim_gain_hi")),
            "PASS" if s["passed"] else "FAIL"])
    return table(["variant", "change", "runner rate (test)", "runner lift top 10% [95%]",
                  "runner AUC LightGBM / logistic", "collapse AUC, plain label: LightGBM / logistic",
                  "collapse AUC, trained label: LightGBM / logistic",
                  "collapses removed (plain; gate)", "counted weeks beating buy-all",
                  "walk-forward mean lift (counted weeks)",
                  "walk-forward mean collapses removed (counted weeks)",
                  "sim top 10% vs all (%)",
                  f"pooled WF lift [95%]; sim difference [95%] (seed {C.LGBM_SEED} only; "
                  "information)", "single-run (old) gate, information"], rows)


def _variant_time(v: dict) -> str:
    text = f"{v['seconds']:.0f} s"
    extra = (v.get("across_seeds") or {}).get("seconds")
    if extra is not None:
        text += f" (of which other seeds and adversarial validation {extra:.0f} s)"
    return text


def variants_section(variants: dict, buckets: dict, wallet_rep: dict | None = None) -> str:
    """The variant comparison; `wallet_rep` (train.py's wallet reputation stats,
    None without --live-buys) feeds the wallet reputation blocks, shown when a
    C.WALLET_REP_VARIANTS entry is among the variants."""
    out = ["## Variant comparison (report only; not saved, not served)\n", VARIANT_NOTE + "\n",
           variant_read() + "\n"]
    times = [f"{n}: {_variant_time(v)}" for n, v in variants.items()
             if v.get("seconds") is not None]
    if times:
        out.append("Training time per variant: " + "; ".join(times) + ".\n")
    for b in buckets:
        out.append(f"### Bucket `{b}`\n")
        out.append(variant_table(variants, b))
    out.append(across_seeds_section(variants, buckets))
    for name in C.WALLET_REP_VARIANTS:
        if name in variants:
            out.append(wallet_data_section(wallet_rep))
            out.append(adoption_section(name, variants[name]))
    return "\n".join(out)


ACROSS_SEEDS_TITLE = "### Variants across seeds (report only)\n"


def _across_lifts(lifts: dict, seeds) -> str:
    """The lifts over the seeds joined by " / " ('2.10 / 1.95 / 2.31'), a missing seed as n/a."""
    return " / ".join(fmt(lifts.get(str(s)), 2) for s in seeds)


def _adv_cells(adv) -> list:
    if not isinstance(adv, dict):
        return ["n/a", ""]
    if adv.get("error"):
        return [adv["error"], ""]
    if adv.get("skipped"):
        return [f"skipped: {adv['skipped']}", ""]
    return [fmt(adv.get("auc")), ", ".join(f"{f} {g:.2f}" for f, g in (adv.get("top") or [])[:3])]


def _across_cells(v: dict, b: str, seeds) -> list:
    """The cells of variant `v` in bucket `b` after its name, or the reason it has none."""
    acr = v.get("across_seeds") or {}
    if v.get("error") or acr.get("error"):
        return [v.get("error") or acr["error"]] + [""] * 6
    if v.get("not_applicable"):
        return [f"n/a: {v['not_applicable']}"] + [""] * 6
    a = (acr.get("buckets") or {}).get(b)
    if a is None or a.get("skipped"):
        return [f"n/a: {(a or {}).get('skipped', 'not trained')}"] + [""] * 6
    return [a.get("weeks") or 0, _across_lifts(a["lifts"], acr.get("seeds") or seeds),
            fmt(a.get("median"), 2), _ci(a.get("lift"), a.get("lift_lo"), a.get("lift_hi")),
            _signed_ci(a.get("sim_gain"), a.get("sim_gain_lo"), a.get("sim_gain_hi")),
            *_adv_cells(a.get("adversarial"))]


def across_seeds_section(variants: dict, buckets: dict) -> str:
    """The C.SEED_VARIANTS (and the baseline) with their pooled walk-forward lift
    per STABILITY_SEEDS seed, its median, the LGBM_SEED run's pooled numbers and
    the adversarial AUC of their inputs; '' when no variant has them."""
    names = [n for n, v in variants.items() if v.get("across_seeds") is not None
             or (n in C.SEED_VARIANTS and (v.get("error") or v.get("not_applicable")))]
    if not any(n != "baseline" for n in names):
        return ""
    seeds = next((s for s in ((variants[n].get("across_seeds") or {}).get("seeds") for n in names)
                  if s), list(C.STABILITY_SEEDS))
    out = [ACROSS_SEEDS_TITLE,
           "The baseline (the saved model) and the variants of SEED_VARIANTS, each trained "
           f"with every seed of STABILITY_SEEDS ({', '.join(str(s) for s in seeds)}), so a "
           "variant is compared with the baseline as the runner-lift gate would judge it, not "
           "on one seed's luck. The baseline's numbers are the gates' own (its "
           "seed retrains and pooled walk-forward; nothing retrained). Pooled walk-forward lift "
           "per seed and their median are taken as the runner-lift gate takes them (median "
           "n/a unless every seed has a lift); the lift and simulation-difference intervals "
           f"are the seed {C.LGBM_SEED} run's 95% week-block bootstrap, as in the gates. "
           "Adversarial AUC: can a classifier tell the bucket's test part from its train part "
           "on that variant's own inputs (as in \"Adversarial validation\"; 0.5 = no drift; "
           "calendar inputs left out)? The difference from the baseline's AUC shows the drift "
           "the inputs the variant changes carry. Report only: never saved, never served, never a gate; "
           "to adopt a variant change config.py and train again.\n"]
    rows = [[b, n, *_across_cells(variants[n], b, seeds)] for b in buckets for n in names]
    out.append(table(["bucket", "variant", "counted weeks",
                      f"pooled WF lift, seed {' / '.join(str(s) for s in seeds)}",
                      "median (as the gate)",
                      f"pooled WF lift [95%] (seed {C.LGBM_SEED})",
                      f"pooled sim difference (%) [95%] (seed {C.LGBM_SEED})",
                      "adversarial AUC (train vs test)", "top drifting inputs (gain share)"],
                     rows))
    return "\n".join(out)


# --- Wallet reputation (report-only variant; config WALLET_REP, WREP_*) ---------

WALLET_DATA_TITLE = "### Wallet reputation data (report only)\n"
ADOPTION_TITLE = "### Wallet reputation: pre-registered adoption rules (report only)\n"


def _pct(v) -> str:
    return "n/a" if v is None or (isinstance(v, float) and math.isnan(v)) else f"{v:.1%}"


def wallet_data_section(st) -> str:
    """The wallet reputation inputs (train.wallet_rep_data stats): counts, the
    observed 7 d runner / rug rates next to the fixed priors, and the coverage
    per week."""
    out = [WALLET_DATA_TITLE]
    if not isinstance(st, dict):
        out.append("No live buys given (`train.py --live-buys PATH`; `run_training.sh --variants` "
                   "exports them with the Go program's `-export-live-buys` when it has that "
                   "flag), so the wallet reputation variant is n/a.\n")
        return "\n".join(out)
    if st.get("error"):
        out.append(f"**Not computed: {st['error']}.** The wallet reputation variant is n/a; "
                   "nothing else in this report is affected.\n")
        return "\n".join(out)
    out.append(
        f"{st['live_buy_rows']} live buys of {st['wallets']} distinct wallets on "
        f"{st['calls_with_live_buys']} calls ({st['rows']} rows in the export); "
        f"{st['eligible_history']} first calls with a 7 d outcome form the history "
        f"({st['extreme_dropped']} more left out for a ret_late_7d above {MAX_OUTCOME_PCT:g} %). "
        f"Computed in {st['seconds']:.0f} s. A wallet's history counts only outcomes known "
        f"{C.WREP_KNOWN_LAG_HOURS} h after they were due and before the call, never the call's "
        "own token" + (f", and only the last {C.WREP_LOOKBACK_DAYS} days"
                       if C.WREP_LOOKBACK_DAYS is not None else "") + ".\n")
    out.append(table(["7 d outcome of the history calls", "observed rate", "fixed prior (WREP_PRIOR_*)"],
                     [["runner (medium runner rule, net of tax)", _pct(st.get("y_run_rate")),
                       _pct(C.WREP_PRIOR_RUNNER)],
                      [f"rug (raw ret_late_7d <= {C.WREP_RUG_PCT:g} %)", _pct(st.get("y_rug_rate")),
                       _pct(C.WREP_PRIOR_RUG)]]))
    out.append("Lifts are the smoothed rates divided by these fixed priors (fixed on purpose: a "
               "running rate would be a calendar proxy). A prior far from the observed rate moves "
               "every lift, and so the good / bad shares, which compare lifts with "
               f"WREP_GOOD_LIFT ({C.WREP_GOOD_LIFT:g}) / WREP_BAD_LIFT ({C.WREP_BAD_LIFT:g}).\n")
    weeks = st.get("weeks") or []
    if weeks:
        out.append("Coverage per week (Monday, UTC) over the calls training can use: calls with "
                   "live buys, and among them the share with at least one wallet with history "
                   "and the mean wrep_known_share (it should ramp up as history builds).\n")
        out.append(table(["week", "calls", "calls with live buys", "share with >= 1 known wallet",
                          "mean wrep_known_share"],
                         [[w["week"], w["calls"], w["with_live_buys"], _pct(w["share_known"]),
                           fmt(w["known_share_mean"])] for w in weeks]))
    return "\n".join(out)


def adoption_rules_text() -> str:
    """The pre-registered adoption rules (C.WREP_ADOPTION), as fixed before any result."""
    r = C.WREP_ADOPTION
    return (
        f"Fixed on {C.WREP_ADOPTION_SET_ON}, before any result. The variant passes in a bucket "
        "only if ALL hold (report only: never a gate of the saved model, which never uses "
        "wrep_* inputs):\n\n"
        f"1. median lift: its across-seeds median pooled walk-forward lift >= "
        f"{r['min_median_lift']:g} and >= the baseline's median;\n"
        f"2. lift lower end: the seed {C.LGBM_SEED} pooled lift's 95% lower end > "
        f"{r['min_lift_lower']:g};\n"
        f"3. sim lower end: the pooled simulation difference's 95% lower end > "
        f"{r['min_sim_diff_lower']:g};\n"
        f"4. collapses removed >= {r['min_collapse_removed']:.0%} in the variant's row;\n"
        f"5. drift: its adversarial AUC at most {r['max_adv_auc_rise']:g} above the baseline's, "
        f"and no wrep_* input among the top {r['drift_top']} drifting inputs with a gain share "
        f">= {r['drift_max_share']:.2f}.\n\n"
        "An undefined value (n/a) does not hold. If only rule 5 fails, the next step is a "
        "lookback variant (WREP_LOOKBACK_DAYS), not adoption.\n")


def _holds(rule: dict, text: str) -> str:
    h = rule.get("holds")
    return f"{'n/a' if h is None else 'holds' if h else 'fails'}: {text}"


ADOPTION_RESULT = {"pass": "PASS (all rules hold)",
                   "lookback": "not adopted; next step: a lookback variant (WREP_LOOKBACK_DAYS)",
                   "fail": "does not pass"}


def adoption_cells(a: dict) -> list:
    """The cells of one bucket of the adoption table (train.adoption_bucket)."""
    r = a["rules"]
    d = r["drift"]
    top = ", ".join(f"{f} {g:.2f}" for f, g in d.get("wrep_top") or [])
    return [_holds(r["median_lift"], f"{fmt(r['median_lift'].get('variant'), 2)} vs baseline "
                                     f"{fmt(r['median_lift'].get('baseline'), 2)}"),
            _holds(r["lift_lower"], fmt(r["lift_lower"].get("value"), 2)),
            _holds(r["sim_lower"], _signed(r["sim_lower"].get("value"))),
            _holds(r["collapse_removed"], _pct(r["collapse_removed"].get("value"))),
            _holds(d, f"AUC {fmt(d.get('variant'))} vs baseline {fmt(d.get('baseline'))}"
                      + (f"; wrep_* in the top: {top}" if top else "")),
            ADOPTION_RESULT[a["result"]]]


def adoption_section(name: str, v: dict) -> str:
    """The pre-registered adoption rules and, when the variant ran, per bucket
    whether each holds (the variant's "adoption", train.wallet_adoption)."""
    out = [ADOPTION_TITLE, adoption_rules_text()]
    ad = v.get("adoption")
    why = (v.get("error") or (f"n/a: {v['not_applicable']}" if v.get("not_applicable") else None)
           or (ad or {}).get("error") or (None if ad else "not evaluated"))
    if why:
        out.append(f"Not evaluated for `{name}`: {why}.\n")
        return "\n".join(out)
    rows = []
    for b, a in (ad.get("buckets") or {}).items():
        if a.get("not_applicable"):
            rows.append([b, f"n/a: {a['not_applicable']}"] + [""] * 5)
        else:
            rows.append([b, *adoption_cells(a)])
    out.append(table(["bucket", "1. median lift", "2. lift lower end", "3. sim lower end (%)",
                      "4. collapses removed", "5. drift", "result"], rows))
    return "\n".join(out)
