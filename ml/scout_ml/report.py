"""Render report.md from the results dict produced by train.py."""
import math

from . import config as C
from .config import (BUCKETS, CALIB_RECENT_DAYS, DEAD_COLUMN,
                     GATES, KFOLD_MIN_FOLDS, LABELS, LIFT_CI_Z, MAX_OUTCOME_PCT,
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
    return (f"Walk-forward gate: a week counts toward \"beats buy-all in every week\" only when "
            f"its training part has at least {C.WF_MIN_TRAIN_ROWS} rows (WF_MIN_TRAIN_ROWS, set "
            f"on {C.WF_MIN_TRAIN_ROWS_SET_ON}); weeks below are shown, marked \"not counted\".\n")


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
           table(["bucket", "result", "top-10% lift >= 2", "collapses removed >= 40%",
                  "beats buy-all in every week"],
                 [[b, "skipped" if r.get("skipped") else ("PASS" if r["gates"]["passed"] else "FAIL")]
                  + ([""] * 3 if r.get("skipped") else
                     [r["gates"]["lift_ok"], r["gates"]["collapse_ok"], wf_cell(r["gates"])])
                  for b, r in res["buckets"].items()]),
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
        out.append(table(["check (LightGBM, test part)", "value", "needed", "ok"], [
            [f"runner lift, top {TOP_FRAC:.0%} by runner score", t["top_lift"],
             f">= {GATES['min_top_lift']:g}", g["lift_ok"]],
            ["  95% interval of that lift; runners top/calls top of runners/calls",
             f"{lift_ci(t)}; {lift_counts(t)}", "", ""],
            [f"collapses removed by skipping top {SKIP_FRAC:.0%} collapse score",
             t["collapse_removed"], f">= {GATES['min_collapse_removed']:g}", g["collapse_ok"]]]
            + ([["  bad outcomes removed incl. dead (collapse OR dead; information, not a gate)",
                 t["collapse_removed_with_dead"], "", ""]]
               if "collapse_removed_with_dead" in t else [])
            + [["simulation beats buy-everything in every counted walk-forward week",
                f"{g['windows_beating']}/{g['windows_evaluated']}"
                + (f" ({g['windows_not_counted']} not counted)" if g.get("windows_not_counted")
                   else ""), "all", g["walk_forward_ok"]]]))
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
    if res.get("variants"):
        out.append(variants_section(res["variants"], res["buckets"]))
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
             "\"not counted\" are left out, as for the bucket gates). On this run:\n"]
    for model in ("lightgbm", "logistic"):
        q = p[model]
        lines.append(f"- {model}: test lift {fmt(q['test_lift'], 2)} ({'meets' if q['test_ok'] else 'below'} "
                     f"{lift:g}); weeks meeting it {q['weeks_meeting']}/{q['weeks_judged']}; "
                     f"would {'pass' if q['would_pass'] else 'not pass'} the proposal")
    return "\n".join(lines) + "\n"


VARIANT_NOTE = (
    "Each variant changes exactly one setting of the baseline (the saved model) and is trained "
    "on the same rows, splits and seed. Variants are report only: not saved, not served, and "
    "their gate result is not the verdict above. **All variants are compared on the same test "
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
        f"part of at least {C.WF_MIN_TRAIN_ROWS} rows), as the gate does.")


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
            rows.append([name, changes, v["error"]] + [""] * 10)
            continue
        s = (v.get("buckets") or {}).get(bucket)
        if v.get("not_applicable") or s is None or s.get("skipped"):
            why = v.get("not_applicable") or (s or {}).get("skipped") or "not trained"
            rows.append([name, changes, f"n/a: {why}"] + [""] * 10)
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
            "PASS" if s["passed"] else "FAIL"])
    return table(["variant", "change", "runner rate (test)", "runner lift top 10% [95%]",
                  "runner AUC LightGBM / logistic", "collapse AUC, plain label: LightGBM / logistic",
                  "collapse AUC, trained label: LightGBM / logistic",
                  "collapses removed (plain; gate)", "counted weeks beating buy-all",
                  "walk-forward mean lift (counted weeks)",
                  "walk-forward mean collapses removed (counted weeks)",
                  "sim top 10% vs all (%)", "gates"], rows)


def variants_section(variants: dict, buckets: dict) -> str:
    out = ["## Variant comparison (report only; not saved, not served)\n", VARIANT_NOTE + "\n",
           variant_read() + "\n"]
    times = [f"{n}: {v['seconds']:.0f} s" for n, v in variants.items() if v.get("seconds") is not None]
    if times:
        out.append("Training time per variant: " + "; ".join(times) + ".\n")
    for b in buckets:
        out.append(f"### Bucket `{b}`\n")
        out.append(variant_table(variants, b))
    return "\n".join(out)
