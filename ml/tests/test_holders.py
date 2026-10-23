"""Holder pilot analysis (scout_ml/holders.py, holders_report.py): synthetic
export and ids files only (make_synthetic.make_holder_pilot)."""
import csv
import json
import re
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd
import pytest
from sklearn.metrics import roc_auc_score

import holders_report
import make_synthetic as M
from scout_ml import config as C
from scout_ml import holders as H
from test_holder_columns import GO, go_columns

NOW = datetime(2026, 10, 21, 3, 15, tzinfo=timezone.utc)
ADDRESS = re.compile(r"0x[0-9a-fA-F]{40}")


@pytest.fixture(autouse=True)
def few_reps(monkeypatch):
    monkeypatch.setattr(C, "HOLD_BOOTSTRAP_REPS", 200)
    monkeypatch.setattr(C, "HOLD_PERM_REPS", 40)
    monkeypatch.setattr(C, "HOLD_INCR_REPEATS", 3)


@pytest.fixture(scope="module")
def made():
    return M.make_holder_pilot()


class Pilot:
    def __init__(self, tmp_path, made):
        rows, ids = made
        self.rows, self.ids = [dict(r) for r in rows], ids.copy()
        self.dir, self.csv, self.out = tmp_path / "holders", tmp_path / "ids.csv", tmp_path / "out"

    def write(self, columns=None, **kw):
        M.write_holder_pilot(self.dir, self.csv, self.rows, self.ids, columns, **kw)
        return self

    def main(self, *extra):
        return holders_report.main(["--holders", str(self.dir), "--ids", str(self.csv),
                                    "--out", str(self.out), *extra], now=NOW)

    def summary(self):
        ex = H.load_export(self.dir)
        H.check_export(ex, C.HOLD_FEATURES)
        return H.analyse(ex, H.load_ids(self.csv), C.HOLD_FEATURES)


@pytest.fixture
def pilot(tmp_path, made):
    return Pilot(tmp_path, made)


def refused(p, capsys, text):
    assert p.main() == H.EXIT_REFUSED
    err = capsys.readouterr().err
    assert text in err and "Nothing written" in err, err
    assert not p.out.exists()
    assert not ADDRESS.search(err)


# --- columns ------------------------------------------------------------------
def test_synthetic_pre_hold_columns_are_the_classified_ones():
    assert set(M.HOLDER_PRE) == set(C.HOLD_FEATURES) | set(C.HOLD_EXCLUDED)
    assert len(M.HOLDER_COLUMNS) == len(set(M.HOLDER_COLUMNS))


@pytest.mark.skipif(not GO.exists(), reason="holders_columns.go not next to ml/")
def test_synthetic_columns_match_the_go_struct():
    assert M.HOLDER_COLUMNS == go_columns()


def test_repo_gitignore_ignores_data():
    lines = (Path(holders_report.REPO_ROOT) / ".gitignore").read_text(encoding="utf-8").splitlines()
    assert "/data/" in [x.strip() for x in lines]


def test_default_paths_are_under_the_repo_data_folder():
    a = holders_report.parse_args([])
    data = Path(holders_report.REPO_ROOT) / "data"
    assert (Path(a.holders), Path(a.ids), Path(a.out)) == (
        data / "holders", data / "holder_pilot_ids.csv", data / "holders_analysis")


def test_usage_error_exits_2():
    with pytest.raises(SystemExit) as e:
        holders_report.main(["--bogus"])
    assert e.value.code == 2


# --- refusals ---------------------------------------------------------------------
@pytest.mark.parametrize("col", ["call_id", "grp", "week_utc", "entry_block"])
def test_refuses_ids_without_required_column(pilot, capsys, col):
    pilot.ids = pilot.ids.drop(columns=[col])
    if col == "call_id":
        pilot.ids.insert(0, "id", range(len(pilot.ids)))
    refused(pilot.write(), capsys, "lacks column")


def first_value(col, value):
    """Replace the first ids row's `col` by value(ids)."""
    def change(p):
        vals = list(p.ids[col])
        p.ids = p.ids.assign(**{col: [value(p.ids)] + vals[1:]})
    return change


@pytest.mark.parametrize("change, text", [
    (first_value("grp", lambda ids: "other"), "grp outside"),
    (first_value("call_id", lambda ids: ids["call_id"].iloc[1]), "duplicate call_id value(s) in the ids"),
    (first_value("ca", lambda ids: ids["ca"].iloc[1].upper()), "duplicate ca"),
    (first_value("call_id", lambda ids: "x"), "not a whole number"),
])
def test_refuses_bad_ids(pilot, capsys, change, text):
    change(pilot)
    refused(pilot.write(), capsys, text)


# Only the three exact Q6 forms are read as a group: a wrong number ('2 dead7' is held2x7's
# number), another case, extra or missing spaces inside, or a prefix on an unknown name.
@pytest.mark.parametrize("label", ["4 dead7", "Dead7", "2 dead7", "1 Dead7", "1  dead7", "1dead7",
                                   "01 dead7", "1 dead7x", "4 other"])
def test_refuses_grp_labels_other_than_the_accepted_forms(pilot, capsys, label):
    first_value("grp", lambda ids: label)(pilot)
    refused(pilot.write(), capsys, "1 ids row(s) with grp outside "
            "['dead7', 'held2x7', 'neither', '1 dead7', '2 held2x7', '3 neither']")


def test_q6_prefixed_labels_map_to_the_plain_groups():
    assert C.HOLD_GROUP_LABELS == {"1 dead7": "dead7", "2 held2x7": "held2x7", "3 neither": "neither"}
    got = H.plain_groups(pd.Series(["1 dead7", "2 held2x7", "3 neither", "dead7", "2 dead7", "Dead7"]))
    assert got.tolist() == ["dead7", "held2x7", "neither", "dead7", "2 dead7", "Dead7"]


@pytest.mark.parametrize("name", [H.ROWS_FILE, H.COLUMNS_FILE])
def test_refuses_missing_export_file(pilot, capsys, name):
    (pilot.write().dir / name).unlink()
    refused(pilot, capsys, f"export file missing: {name}")


def test_refuses_missing_ids_file(pilot, capsys):
    pilot.write().csv.unlink()
    refused(pilot, capsys, "ids file missing")


def test_refuses_row_keys_unlike_columns(pilot, capsys):
    pilot.rows[3]["extra"] = 1
    refused(pilot.write(), capsys, "keys differ")


def test_refuses_pre_hold_columns_unlike_columns(pilot, capsys):
    pilot.write()
    cols = json.loads((pilot.dir / H.COLUMNS_FILE).read_text())
    cols["pre_hold_columns"] = cols["pre_hold_columns"][1:]
    (pilot.dir / H.COLUMNS_FILE).write_text(json.dumps(cols))
    refused(pilot, capsys, "pre_hold_columns is not")


def test_refuses_columns_json_that_is_not_an_object(pilot, capsys):
    (pilot.write().dir / H.COLUMNS_FILE).write_text("[]")
    refused(pilot, capsys, "not a JSON object")


def test_data_checks_correlate_the_export_with_the_post(pilot):
    checks = pilot.write().summary()["data_checks"]
    age, hold = checks["pre_hold_launch_age_s ~ age_s_at_call"], checks["pre_hold_holders ~ holders_at_call"]
    assert age["spearman"] > 0.99 and 0.9 < hold["spearman"] < 1 and hold["n"] > 300


def test_refuses_unclassified_pre_hold_column(pilot, capsys):
    for r in pilot.rows:
        r["pre_hold_new"] = 1
    refused(pilot.write(M.HOLDER_COLUMNS + ["pre_hold_new"]), capsys, "neither in HOLD_FEATURES")


def test_refuses_missing_hold_feature(pilot, capsys):
    for r in pilot.rows:
        del r["pre_hold_snipers"]
    pilot.write([c for c in M.HOLDER_COLUMNS if c != "pre_hold_snipers"])
    refused(pilot, capsys, "HOLD_FEATURES name(s) missing")


def test_refuses_duplicate_export_call_id(pilot, capsys):
    pilot.rows.append(dict(pilot.rows[0]))
    refused(pilot.write(), capsys, "duplicate call_id value(s) in holders.jsonl")


def test_refuses_export_ids_not_in_ids_file(pilot, capsys):
    pilot.ids = pilot.ids[pilot.ids.call_id != pilot.rows[0]["call_id"]]
    refused(pilot.write(), capsys, "not in the ids file")


def test_refuses_no_overlap(pilot, capsys):
    pilot.rows = []
    refused(pilot.write(), capsys, "no overlap")


@pytest.mark.parametrize("extra", ["post24h_holders", "transfers_post", "grp"])
def test_refuses_leaky_inputs(pilot, monkeypatch, capsys, extra):
    monkeypatch.setattr(C, "HOLD_FEATURES", C.HOLD_FEATURES + (extra,))
    refused(pilot.write(), capsys, extra)


def test_refuses_out_inside_a_repo_outside_data(pilot, capsys, tmp_path):
    repo = tmp_path / "repo"
    (repo / ".git").mkdir(parents=True)
    (repo / ".gitignore").write_text("/data/\n")
    pilot.out = repo / "reports"
    refused(pilot.write(), capsys, "outside its data/")
    pilot.out = repo / "data" / "analysis"
    assert pilot.main() == H.EXIT_OK and (pilot.out / "20261021-0315" / "report.md").is_file()
    (repo / ".gitignore").write_text("other\n")
    pilot.out = repo / "data" / "analysis2"
    refused(pilot, capsys, "does not ignore /data/")


def test_refuses_existing_run_folder(pilot, capsys):
    assert pilot.write().main() == H.EXIT_OK
    assert pilot.main() == H.EXIT_REFUSED
    assert "already exists" in capsys.readouterr().err


# --- end to end ---------------------------------------------------------------------
def test_end_to_end_writes_both_files_without_addresses(pilot, capsys):
    assert pilot.write().main() == H.EXIT_OK
    run = pilot.out / "20261021-0315"
    report, summary = (run / "report.md").read_text(encoding="utf-8"), (run / "summary.json").read_text(encoding="utf-8")
    for text in (report, summary):
        assert not ADDRESS.search(text)
        assert M.HOLDER_CODE_VERSION in text
    s = json.loads(summary)
    assert set(s["inputs"]["sha256"]) == {"holders.jsonl", "columns.json", "runs.jsonl", "ids (ids.csv)"}
    assert s["inputs"]["failed_calls"] == 2 and s["inputs"]["runs"] == 1
    no_row = sorted(set(pilot.ids.call_id) - {r["call_id"] for r in pilot.rows})
    assert s["ids_without_row"] == no_row and str(no_row[0]) not in report.split("## Row classes")[1]
    assert s["verdict"]["verdict"] == "CONTINUE"      # pre_hold_top10_share is planted for dead7
    assert any(a["feature"] == "pre_hold_top10_share" for a in s["verdict"]["above_stop_bound"])
    for head in ("Verdict", "Row classes", "Week check", "AUC: dead7 vs rest", "AUC: dead7 vs held2x7",
                 "Posted holders", "Multiple comparisons", "After the call", "pre-registered", "PM's reading"):
        assert head in report, head
    header = report.split("## Verdict")[0]
    for text in ("NOT JUDGED conditions", "strata columns missing", f"fewer than {C.HOLD_MIN_CLASS_N} analysable rows",
                 "lower bound of the within-strata 95 % CI", "are the PM's reading"):
        assert text in header, text


PREFIXED = {g: f"{n} {g}" for n, g in enumerate(C.HOLD_GROUPS, 1)}   # the Q6 query's labels


def run_with_labels(tmp_path, made, name, label, quoted=False):
    """report.md and summary.json (minus the ids file's hash) of a run whose ids grp values are
    label(i, grp); quoted writes every ids value in double quotes, as a pgAdmin export may."""
    p = Pilot(tmp_path / name, made).write()
    p.ids = p.ids.assign(grp=[label(i, g) for i, g in enumerate(p.ids.grp)])
    p.ids.to_csv(p.csv, index=False, quoting=csv.QUOTE_ALL if quoted else csv.QUOTE_MINIMAL)
    assert p.main() == H.EXIT_OK
    run = p.out / "20261021-0315"
    s = json.loads((run / "summary.json").read_text(encoding="utf-8"))
    del s["inputs"]["sha256"]["ids (ids.csv)"]
    return (run / "report.md").read_text(encoding="utf-8"), s


@pytest.mark.parametrize("name, label, quoted", [
    ("prefixed", lambda i, g: PREFIXED[g], False),
    ("prefixed_quoted", lambda i, g: PREFIXED[g], True),
    ("padded_quoted", lambda i, g: f"  {PREFIXED[g]} ", True),
    ("mixed", lambda i, g: PREFIXED[g] if i % 2 else g, False),
])
def test_q6_prefixed_labels_give_the_same_report_as_plain_labels(tmp_path, made, name, label, quoted):
    assert PREFIXED == {"dead7": "1 dead7", "held2x7": "2 held2x7", "neither": "3 neither"}
    plain = run_with_labels(tmp_path, made, "plain", lambda i, g: g)
    got = run_with_labels(tmp_path, made, name, label, quoted)
    assert got[0] == plain[0]
    assert got[1] == plain[1]
    assert got[1]["inputs"]["analysable_rows"] > 0


def test_row_classes_counted_per_group(pilot):
    s = pilot.write().summary()
    rc = s["row_classes"]
    assert {"no row", "skipped: no_mint_found", "capped", "incomplete", "entry-block mismatch",
            "analysable"} == set(rc)
    assert sum(c["total"] for c in rc.values()) == len(pilot.ids)
    assert all(c["total"] == sum(c[g] for g in C.HOLD_GROUPS) for c in rc.values())
    assert s["inputs"]["analysable_rows"] == rc["analysable"]["total"]
    ok = [r for r in pilot.rows if r["status"] == "ok"]
    flagged = sum(1 for r in ok if not r["venues_pre"] and r["transfers_pre"] > 0)
    assert flagged and sum(d["empty_pre_venue_with_transfers"] for d in s["venue_diagnostic"].values()) == flagged


def test_partial_last_line_is_ignored(pilot):
    s = pilot.write(partial_line='{"call_id": 7, "tok').summary()
    assert s["inputs"]["partial_last_line_ignored"] is True
    assert s["inputs"]["export_rows"] == len(pilot.rows)
    assert pilot.write().summary()["inputs"]["partial_last_line_ignored"] is False


def test_shuffling_after_call_and_description_columns_changes_no_auc(pilot):
    cols = M.HOLDER_COLUMNS + ["post24h_grp_code"]
    grp = dict(zip(pilot.ids.call_id, pilot.ids.grp))
    for r in pilot.rows:
        r["post24h_grp_code"] = float(grp[r["call_id"]] == "dead7")
    base = pilot.write(cols).summary()
    rng = np.random.default_rng(3)
    for c in [c for c in cols if c.startswith(H.POST)]:
        vals = [r[c] for r in pilot.rows]
        for r, v in zip(pilot.rows, rng.permutation(np.array(vals, dtype=object))):
            r[c] = v
    desc = [c for c in pilot.ids.columns if c not in H.IDS_REQUIRED + H.IDS_STRATA]
    for c in desc:
        pilot.ids[c] = rng.permutation(pilot.ids[c].to_numpy())
    shuffled = pilot.write(cols).summary()
    for key in ("contrasts", "baseline", "verdict", "multiple_comparisons", "features", "strata", H.INCR_KEY):
        assert shuffled[key] == base[key], key
    assert "post24h_grp_code" in base["after_call"]
    assert all("post24h_grp_code" not in block for block in base["contrasts"].values())
    assert base["after_call"]["post24h_grp_code"]["dead7"]["mean"] == 1.0


# --- NULL handling ---------------------------------------------------------------------
def test_values_booleans_and_null():
    v = H.values(pd.Series([True, False, None, 3, 2.5], dtype=object, name="x"))
    assert v.iloc[:2].tolist() == [1.0, 0.0] and np.isnan(v.iloc[2]) and v.iloc[3:].tolist() == [3.0, 2.5]
    with pytest.raises(H.Refused):
        H.values(pd.Series(["12"], dtype=object, name="x"))
    assert np.isnan(H.values(pd.Series(["12"], dtype=object), strict=False).iloc[0])


def test_lp_not_applicable_leaves_lp_features_null_and_n_counts_non_null(pilot):
    pilot.write()
    df = H.join(H.load_export(pilot.dir), H.load_ids(pilot.csv))
    a = df[df.row_class.eq("analysable")]
    X = H.feature_matrix(a, C.HOLD_FEATURES)
    off = X["pre_hold_lp_applicable"] == 0
    assert off.any() and X.loc[off, M.HOLDER_LP].isna().all().all()
    assert set(X["pre_hold_lp_applicable"].unique()) == {0.0, 1.0}
    s = pilot.summary()
    for g in C.HOLD_GROUPS:
        in_g = (a.grp == g).to_numpy()
        for f in ("pre_hold_lp_adds", "pre_hold_deployer_sold"):
            assert s["features"][f][g]["n"] == int(X[f][in_g].notna().sum())
            assert s["features"][f][g]["missing_share"] == pytest.approx(X[f][in_g].isna().mean())
    b = s["contrasts"]["dead7 vs rest"]["pre_hold_lp_adds"]
    assert b["n_pos"] + b["n_neg"] == int(X["pre_hold_lp_adds"].notna().sum())


# --- AUC ---------------------------------------------------------------------------------
def test_pair_auc_matches_sklearn_with_ties():
    rng = np.random.default_rng(1)
    x, y = rng.integers(0, 5, 300).astype(float), rng.random(300) < 0.4
    assert H.pair_auc(x, y)[0] == pytest.approx(roc_auc_score(y, x))


def test_auc_perfect_and_identical():
    grp = pd.Series(["dead7"] * 40 + ["held2x7"] * 40 + ["neither"] * 40)
    perfect = pd.Series([1.0] * 40 + [0.0] * 80)
    b = H.auc_block(perfect, grp, H.PRIMARY[0])
    assert b["auc"] == 1.0 and b["oriented"] == 1.0 and b["ci"] == [1.0, 1.0]
    low = H.auc_block(-perfect, grp, H.PRIMARY[0])
    assert low["auc"] == 0.0 and low["oriented"] == 1.0 and low["direction"] == "lower"
    same = H.auc_block(pd.Series(np.tile(np.arange(40.0), 3)), grp, H.PRIMARY[0])
    assert same["auc"] == pytest.approx(0.5)
    small = H.auc_block(perfect.where(grp != "dead7").fillna(1.0).where(np.arange(120) > 15), grp, H.PRIMARY[0])
    assert "na" in small and "auc" not in small


def test_bootstrap_resamples_within_each_group():
    g = np.array(["a", "b", "c", "b", "a", "b", "c", "b"] * 6 + ["b"] * 11)
    w = H.boot_weights(g)
    assert w.shape == (C.HOLD_BOOTSTRAP_REPS, len(g))
    for name in ("a", "b", "c"):
        assert np.all(w[:, g == name].sum(1) == (g == name).sum()), name


def test_orientation_is_fixed_on_the_full_sample():
    # identical groups: AUC 0.5, so the bootstrap AUCs fall on both sides of 0.5;
    # orienting each rep on its own would push the whole interval to >= 0.5
    grp = pd.Series(["dead7"] * 40 + ["held2x7"] * 40 + ["neither"] * 40)
    b = H.auc_block(pd.Series(np.tile(np.arange(40.0), 3)), grp, H.PRIMARY[0])
    assert b["auc"] == pytest.approx(0.5)
    assert b["ci"][0] < 0.5 < b["ci"][1], b["ci"]


def test_permutation_shuffles_grp_within_week_only():
    # week decides grp and the feature is the week: a shuffle within week keeps the
    # (confounded) AUC of 1.0; a global shuffle would give about 0.5-0.6
    week = pd.Series(["2026-10-05"] * 100 + ["2026-10-12"] * 100)
    grp = pd.Series(["dead7"] * 100 + ["held2x7"] * 50 + ["neither"] * 50)
    X = pd.DataFrame({"f": (week == "2026-10-05").astype(float)})
    observed = H.auc_block(X["f"], grp, H.PRIMARY[0])["oriented"]
    p = H.perm_null(X, grp, week, {"dead7 vs rest": ["f"]})
    assert observed == 1.0 and p["dead7 vs rest"] == pytest.approx(observed) and p["both"] > 0.8
    assert p["held2x7 vs rest"] is None


def stop_rule_case(ci_low, strata_ci_low):
    block = {"ci": [ci_low, 0.9]} if strata_ci_low is None else {"ci": [ci_low, 0.9], "strata_ci": [strata_ci_low, 0.9]}
    return H.verdict({"dead7 vs rest": {"f": block}, "held2x7 vs rest": {}},
                     {g: C.HOLD_MIN_CLASS_N for g in C.HOLD_GROUPS}, None)


def test_verdict_bounds_are_strict():
    assert stop_rule_case(0.7, 0.7)["verdict"] == "CONTINUE"
    at_stop = stop_rule_case(C.HOLD_STOP_AUC_LOWER, 0.7)
    assert at_stop["verdict"] == "STOP" and at_stop["above_stop_bound"] == []
    at_strata = stop_rule_case(0.7, C.HOLD_STRATA_AUC_LOWER)
    assert at_strata["verdict"] == "STOP" and len(at_strata["above_stop_bound"]) == 1


def test_stop_reason_counts_pairs_without_a_within_strata_bound():
    v = stop_rule_case(0.7, None)
    assert v["verdict"] == "STOP" and v["above_stop_bound"][0]["strata_ci_low"] is None
    assert "1 above the bound pooled, 1 of them with within-strata AUC n/a" in v["reasons"][0]
    assert "n/a" not in stop_rule_case(0.7, 0.4)["reasons"][0]


def test_stratified_auc_with_one_stratum_equals_plain():
    rng = np.random.default_rng(2)
    x, y = rng.normal(size=200), rng.random(200) < 0.3
    x[y] += 0.5
    assert H.pair_auc(x, y, ["s"] * 200)[0] == pytest.approx(H.pair_auc(x, y)[0])
    assert H.pair_auc(x, y, ["s"] * 200)[0] == pytest.approx(roc_auc_score(y, x))
    assert np.isnan(H.pair_auc(x, y, [None] * 200)[0])


def test_feature_tracking_posted_holders_vanishes_within_strata():
    rng = np.random.default_rng(5)
    n = 900
    df = pd.DataFrame({"holders_at_call": rng.lognormal(np.log(120), 0.7, n),
                       "age_s_at_call": rng.choice([600.0, 7200.0], n)})
    strata, cuts = H.stratum_labels(df)
    p_dead = strata.str[:2].map({"T1": 0.1, "T2": 0.35, "T3": 0.8}).to_numpy()
    grp = pd.Series(np.where(rng.random(n) < p_dead, "dead7", rng.choice(["held2x7", "neither"], n)))
    x = df["holders_at_call"] + rng.normal(0, 5, n)
    b = H.auc_block(x, grp, H.PRIMARY[0], pd.Series(["w"] * n), strata)
    assert b["ci"][0] > C.HOLD_STOP_AUC_LOWER
    assert abs(b["within_strata"] - 0.5) < 0.06 and b["strata_ci"][0] < C.HOLD_STRATA_AUC_LOWER
    v = H.verdict({"dead7 vs rest": {"f": b}, "held2x7 vs rest": {}},
                  {g: int((grp == g).sum()) for g in C.HOLD_GROUPS}, None)
    assert v["verdict"] == "STOP" and v["above_stop_bound"][0]["feature"] == "f"


def test_not_judged_without_strata_columns(pilot):
    pilot.ids = pilot.ids.drop(columns=list(H.IDS_STRATA + H.IDS_OUTCOMES + H.IDS_TAX))
    assert pilot.write().main() == H.EXIT_OK
    run = pilot.out / "20261021-0315"
    s = json.loads((run / "summary.json").read_text(encoding="utf-8"))
    assert s["verdict"]["verdict"] == "NOT JUDGED" and "older Q6 export" in s["verdict"]["reasons"][0]
    assert s["baseline"] is None and "not_judged" in s["strata"] and s["outcomes"] == {}
    assert s[H.INCR_KEY] == {"not_judged": s["strata"]["not_judged"]}
    assert "pre_hold_top10_share" in s["contrasts"]["dead7 vs rest"]
    report = (run / "report.md").read_text(encoding="utf-8")
    assert "Verdict: NOT JUDGED" in report
    assert "NOT JUDGED: ids file lacks" in report.split(H.INCR_HEAD)[1]


def test_not_judged_with_too_few_rows_in_a_group(pilot):
    keep = pilot.ids[pilot.ids.grp != "neither"].call_id.tolist() + pilot.ids[pilot.ids.grp == "neither"].call_id.tolist()[:20]
    pilot.ids = pilot.ids[pilot.ids.call_id.isin(keep)]
    pilot.rows = [r for r in pilot.rows if r["call_id"] in set(keep)]
    v = pilot.write().summary()["verdict"]
    assert v["verdict"] == "NOT JUDGED" and "neither" in v["reasons"][0]


def test_permutation_null_is_reproducible_and_near_chance(pilot):
    a, b = pilot.write().summary(), pilot.summary()
    m = a["multiple_comparisons"]
    assert m == b["multiple_comparisons"]
    assert m["tests"] == 2 * len(C.HOLD_FEATURES)
    assert 0.5 < m["perm_p95_best"]["dead7 vs rest"] < 0.7 and m["perm_p95_best"]["both"] >= m["perm_p95_best"]["dead7 vs rest"]


# --- beyond posted holders and age (report only) ---------------------------------------
SINGLE_KEYS = {"features", "contrast_rows", "dropped_baseline_missing", "dropped_feature_missing", "n_pos", "n_neg",
               "baseline_auc", "model_auc", "delta", "delta_ci", "repeat_delta", "coef", "direction", "residual",
               "terciles", "deciles", "reading"}
JOINT = "pre_hold_holders + pre_hold_sellers"
INCR_SETTING_NAMES = ("HOLD_INCR_FEATURES", "HOLD_INCR_FOLDS", "HOLD_INCR_REPEATS", "HOLD_INCR_SEED",
                      "HOLD_INCR_LOGIT_C", "HOLD_INCR_DECILES", "HOLD_INCR_ADDS_DELTA_LOWER",
                      "HOLD_INCR_ADDS_STRATA_LOWER")


def controlled(n=900, seed=5):
    """dead7 driven by the posted holders (unbalanced groups); pre_hold_holders a noisy copy of
    holders_at_call, pre_hold_sellers higher for dead7 independently of holders and age."""
    rng = np.random.default_rng(seed)
    h = rng.lognormal(np.log(120), 0.7, n)
    age = rng.choice([600.0, 7200.0], n) * rng.uniform(0.5, 1.5, n)
    z = (np.log(h) - np.log(h).mean()) / np.log(h).std()
    dead = rng.random(n) < 1 / (1 + np.exp(-(1.5 * z - 0.7)))
    grp = pd.Series(np.where(dead, "dead7", rng.choice(["held2x7", "neither"], n, p=[0.2, 0.8])))
    a = pd.DataFrame({"holders_at_call": h, "age_s_at_call": age})
    X = pd.DataFrame({"pre_hold_holders": h * rng.uniform(0.9, 1.1, n),
                      "pre_hold_sellers": rng.poisson(np.exp(3 + 0.4 * dead)).astype(float)})
    return a, X, grp


def test_beyond_baseline_noisy_copy_shows_no_addition_and_independent_signal_adds():
    a, X, grp = controlled()
    inc = H.beyond_baseline(a, X, grp, H.stratum_labels(a)[0])
    d = inc["contrasts"]["dead7 vs rest"]
    copy, sig = d["pre_hold_holders"], d["pre_hold_sellers"]
    assert copy["baseline_auc"] > 0.7 and copy["n_pos"] + copy["n_neg"] == len(a)
    assert abs(copy["delta"]) < 0.02 and copy["delta_ci"][0] <= 0, copy["delta_ci"]
    assert copy["reading"] == H.NO_ADDITION
    assert sig["delta"] > 0.05 and sig["delta_ci"][0] > C.HOLD_INCR_ADDS_DELTA_LOWER
    assert sig["deciles"]["strata_ci"][0] > C.HOLD_INCR_ADDS_STRATA_LOWER and sig["deciles"]["direction"] == "higher"
    assert sig["coef"]["pre_hold_sellers"] > 0 and sig["residual"]["ci"][0] > 0.5 and sig["reading"] == "adds"
    assert copy["direction"] == sig["direction"] == "higher" and not H.coef_disagrees(copy) and not H.coef_disagrees(sig)
    assert d[JOINT]["reading"] is None and set(d[JOINT]["coef"]) == {"pre_hold_holders", "pre_hold_sellers"}
    assert inc["decile_strata"]["strata"] == 2 * C.HOLD_INCR_DECILES


def test_beyond_baseline_small_class_is_na_per_contrast_and_renders_an_aligned_row():
    """10 dead7 rows: every dead7 vs rest model is n/a (reading "n/a", nothing computed), while
    held2x7 vs rest (60 / 140) is computed on the same data, so the n/a is per class, not global."""
    a, X, _ = controlled(n=200)
    grp = pd.Series(["dead7"] * 10 + ["held2x7"] * 60 + ["neither"] * 130, index=a.index)
    inc = H.beyond_baseline(a, X, grp, H.stratum_labels(a)[0])
    na = f"< {C.HOLD_MIN_CLASS_N} rows with the baseline and feature(s) in a class"
    for name, m in inc["contrasts"]["dead7 vs rest"].items():
        assert (m["n_pos"], m["n_neg"], m["na"], m["reading"]) == (10, 190, na, "n/a"), name
        assert not {"delta", "delta_ci", "coef", "residual", "direction"} & set(m), name
    held = inc["contrasts"]["held2x7 vs rest"]["pre_hold_holders"]
    assert "na" not in held and held["n_pos"] == 60 and held["delta_ci"] and held["reading"] != "n/a"
    m = inc["contrasts"]["dead7 vs rest"]["pre_hold_holders"]
    assert len(H.fdelta(m)) == len(H.fdelta(held)) == 7          # keeps the table aligned
    st = {k: getattr(C, k) for k in ("HOLD_BOOTSTRAP_REPS", "HOLD_BOOTSTRAP_SEED") + H.INCR_SETTINGS}
    lines = H.render_incremental(inc, st)
    rows = [x for x in lines if x.startswith("| dead7 vs rest | pre_hold_holders |")]
    assert rows == [f"| dead7 vs rest | pre_hold_holders | 10 / 190 | 0 / 0 | n/a ({na}) |" + "  |" * 5 + " n/a |"]
    assert rows[0][2:-2].split(" | ")[4:] == [f"n/a ({na})"] + [""] * 5 + ["n/a"]
    assert len([x for x in lines if x.startswith("| held2x7 vs rest | pre_hold_holders |")]) == 2  # + checks row
    assert not any(H.coef_disagrees(x) for x in inc["contrasts"]["dead7 vs rest"].values())  # never daggered


def test_reading_needs_both_bounds():
    m = {"features": ["f"], "delta_ci": [0.01, 0.1], "deciles": {"strata_ci": [0.51, 0.7]}}
    assert H.incr_reading(m) == "adds"
    assert H.incr_reading({**m, "delta_ci": [C.HOLD_INCR_ADDS_DELTA_LOWER, 0.1]}) == H.NO_ADDITION
    assert H.incr_reading({**m, "deciles": {"strata_ci": [C.HOLD_INCR_ADDS_STRATA_LOWER, 0.7]}}) == H.NO_ADDITION
    assert H.incr_reading({**m, "deciles": {"na": "no strata"}}) == "n/a"
    assert H.incr_reading({**m, "features": ["f", "g"]}) is None


def test_beyond_baseline_is_deterministic_and_uses_its_seed(monkeypatch):
    a, X, grp = controlled(n=400)
    t = H.stratum_labels(a)[0]
    first, again = H.beyond_baseline(a, X, grp, t), H.beyond_baseline(a, X, grp, t)
    assert first == again
    monkeypatch.setattr(C, "HOLD_INCR_SEED", 8)
    other = H.beyond_baseline(a, X, grp, t)["contrasts"]["dead7 vs rest"]["pre_hold_sellers"]
    was = first["contrasts"]["dead7 vs rest"]["pre_hold_sellers"]
    assert other["repeat_delta"] != was["repeat_delta"] and other["delta_ci"] != was["delta_ci"]


def test_delta_ci_with_one_repeat_is_the_fixed_score_bootstrap(monkeypatch):
    monkeypatch.setattr(C, "HOLD_INCR_REPEATS", 1)
    rng = np.random.default_rng(3)
    y, g = rng.random(300) < 0.3, rng.choice(["a", "b"], 300)
    Zb = rng.normal(size=(300, 2))
    Zm = np.column_stack([Zb, Zb[:, 0] + rng.normal(size=300) + y])
    sb, sm = H.oof_pair(Zb, Zm, y)
    w = H.boot_weights(g)
    fixed = H.ci(H.pair_auc(sm[0], y, None, w) - H.pair_auc(sb[0], y, None, w))
    assert H.delta_auc(Zb, Zm, y, g)["delta_ci"] == fixed


def test_delta_ci_includes_the_fold_noise():
    """Two repeats whose deltas have opposite signs: each rep uses one drawn repeat, so the CI
    spans both, where the repeat-averaged scores would give a delta near 0 with a narrow CI."""
    rng = np.random.default_rng(6)
    y = np.arange(400) % 4 == 0
    sb = np.tile(rng.random(400), (2, 1))
    sm = np.vstack([np.where(y, 0.9, 0.1), np.where(y, 0.1, 0.9)])
    w = H.boot_weights(np.where(y, "p", "n"))
    pick = H.draw_repeats(len(w), 2)
    assert 0.3 < pick.mean() < 0.7 and np.array_equal(pick, H.draw_repeats(len(w), 2))
    lo, hi = H.ci(H.boot_delta(sb, sm, y, w))
    assert lo < -0.3 and hi > 0.3


def test_baseline_and_model_share_the_folds():
    rng = np.random.default_rng(4)
    y = rng.random(200) < 0.3
    Zb = rng.normal(size=(200, 2))
    sb, sm = H.oof_pair(Zb, Zb.copy(), y)     # model == baseline: identical scores in every repeat
    assert sb.shape == (C.HOLD_INCR_REPEATS, 200) and np.array_equal(sb, sm)


def test_missing_and_negative_baseline_rows_are_dropped_and_counted(pilot):
    pilot.write()
    df = H.join(H.load_export(pilot.dir), H.load_ids(pilot.csv))
    ok = df.loc[df.row_class.eq("analysable"), "call_id"].tolist()
    blank, negative, no_feature = ok[:10], ok[10:13], ok[13:18]
    pilot.ids["holders_at_call"] = pilot.ids["holders_at_call"].astype(object)
    pilot.ids.loc[pilot.ids.call_id.isin(blank[:6]), "holders_at_call"] = ""
    pilot.ids.loc[pilot.ids.call_id.isin(blank[6:]), "age_s_at_call"] = None
    pilot.ids.loc[pilot.ids.call_id.isin(negative), "age_s_at_call"] = -5
    for r in pilot.rows:
        if r["call_id"] in no_feature:
            r["pre_hold_sellers"] = None
    s = pilot.write().summary()
    for c in ("dead7 vs rest", "held2x7 vs rest"):
        models = s[H.INCR_KEY]["contrasts"][c]
        assert models["pre_hold_holders"]["dropped_baseline_missing"] == 13
        assert models["pre_hold_holders"]["dropped_feature_missing"] == 0
        assert models["pre_hold_sellers"]["dropped_feature_missing"] == 5
        assert models[JOINT]["dropped_feature_missing"] == 5
        for m in models.values():
            assert "delta" in m
            assert m["n_pos"] + m["n_neg"] + m["dropped_baseline_missing"] + m["dropped_feature_missing"] \
                == m["contrast_rows"] == len(ok)
    assert s[H.INCR_KEY]["decile_strata"]["rows_without_stratum"] == 10   # a negative age still has a stratum
    assert pilot.main() == H.EXIT_OK
    section = (pilot.out / "20261021-0315" / "report.md").read_text(encoding="utf-8").split(H.INCR_HEAD)[1]
    assert "| dead7 vs rest | pre_hold_sellers |" in section and "| 13 / 5 |" in section


def test_log1p_count_and_decile_ties():
    v = H.log1p_count(pd.Series([0.0, 3.0, -1.0, np.nan]))
    assert v.iloc[0] == 0 and v.iloc[1] == pytest.approx(np.log(4)) and v.iloc[2:].isna().all()
    a = pd.DataFrame({"holders_at_call": [5.0] * 60 + list(range(6, 46)), "age_s_at_call": [600.0] * 100})
    lab, cuts = H.decile_labels(a)
    assert cuts == sorted(set(cuts)) and len(cuts) < C.HOLD_INCR_DECILES - 1   # tied cut points merge
    assert lab.iloc[:60].nunique() == 1 and lab.notna().all() and lab.nunique() <= len(cuts) + 1
    assert H.decile_labels(a.head(C.HOLD_INCR_DECILES - 1)) == (None, None)


@pytest.mark.parametrize("feats, text", [
    (("post24h_holders",), "inputs must be pre_hold_*"), (("holders_at_call",), "inputs must be pre_hold_*"),
    (("pre_hold_incomplete",), "HOLD_INCR_FEATURES must be"), (("pre_hold_holders",) * 2, "HOLD_INCR_FEATURES must be"),
    ((), "HOLD_INCR_FEATURES must be")])
def test_refuses_bad_incremental_features(pilot, monkeypatch, capsys, feats, text):
    monkeypatch.setattr(C, "HOLD_INCR_FEATURES", feats)
    refused(pilot.write(), capsys, text)


def strip_incremental(s: dict) -> dict:
    return {k: v for k, v in s.items() if k != H.INCR_KEY}


def test_new_section_is_appended_and_leaves_the_rest_byte_identical(pilot):
    assert pilot.write().main() == H.EXIT_OK
    run = pilot.out / "20261021-0315"
    report = (run / "report.md").read_text(encoding="utf-8")
    s = json.loads((run / "summary.json").read_text(encoding="utf-8"))
    head, section = report.split(H.INCR_HEAD)
    assert head == H.render_report(strip_incremental(s)) + "\n"
    assert [x for x in report.splitlines() if x.startswith("## ")][-1] == H.INCR_HEAD
    for text in ("never part of the verdict", "not pre-registered", "Group shares come from the sample, not "
                 "necessarily the natural base rates: ranking (AUC) only.", "PM's suggestion",
                 f'otherwise "{H.NO_ADDITION}"', "one repeat drawn at random", "floor on the uncertainty",
                 "no evidence either way", "not the project's time-based validation", "within-sample contrast",
                 "the CI is centred on the repeat-delta mean (shown in the table), not necessarily on the point "
                 "delta of the averaged scores",
                 "only when the ids file has a ca column", "oriented by the feature's pooled direction"):
        assert text in section, text
    assert "one third" not in report
    assert ("Group shares come from the sample, not necessarily the natural base rates: ranking (AUC) only, never "
            "lift or precision.") in head      # the report header's line, changed with this section
    assert not ADDRESS.search(section)


def test_verdict_and_other_keys_ignore_the_incremental_check(pilot, monkeypatch):
    base = pilot.write().summary()
    monkeypatch.setattr(C, "HOLD_INCR_FEATURES", ("pre_hold_top10_share",))
    monkeypatch.setattr(C, "HOLD_INCR_SEED", 99)
    other = pilot.summary()
    assert strip_incremental(other) == strip_incremental(base)
    assert list(other[H.INCR_KEY]["contrasts"]["dead7 vs rest"]) == ["pre_hold_top10_share"]


def test_incremental_summary_keys_and_settings(pilot):
    assert C.HOLD_INCR_FEATURES == ("pre_hold_holders", "pre_hold_sellers")
    assert pilot.write().main() == H.EXIT_OK
    s = json.loads((pilot.out / "20261021-0315" / "summary.json").read_text(encoding="utf-8"))
    assert list(s)[-1] == H.INCR_KEY and set(s[H.INCR_KEY]) == {"decile_strata", "contrasts"}
    assert set(s[H.INCR_KEY]["decile_strata"]) == {"cut_points", "strata", "rows_without_stratum"}
    assert list(s["settings"])[-len(INCR_SETTING_NAMES):] == list(INCR_SETTING_NAMES)
    inc = s[H.INCR_KEY]["contrasts"]
    assert list(inc) == ["dead7 vs rest", "held2x7 vs rest"]
    for c, models in inc.items():
        assert list(models) == ["pre_hold_holders", "pre_hold_sellers", JOINT]
        assert set(models[JOINT]) == SINGLE_KEYS - {"direction", "residual", "terciles", "deciles"}
        for f in ("pre_hold_holders", "pre_hold_sellers"):
            assert set(models[f]) == SINGLE_KEYS
            assert models[f]["reading"] == H.NO_ADDITION          # the pilot plants no signal in either
            # the tercile check is the existing within-strata number on the same rows
            assert models[f]["terciles"]["within_strata"] == pytest.approx(s["contrasts"][c][f]["within_strata"])


def test_coefficient_against_pooled_direction_is_marked():
    """pre_hold_holders tracks the posted holders (pooled: higher for dead7) but is lower for dead7
    given them, so its coefficient is negative: marked with the dagger and the footnote."""
    a, X, grp = controlled()
    rng = np.random.default_rng(11)
    dead = (grp == "dead7").to_numpy()
    X["pre_hold_holders"] = a["holders_at_call"] * np.exp(-0.6 * dead + rng.normal(0, 0.1, len(a)))
    inc = H.beyond_baseline(a, X, grp, H.stratum_labels(a)[0])
    m = inc["contrasts"]["dead7 vs rest"]["pre_hold_holders"]
    assert m["direction"] == "higher" and m["coef"]["pre_hold_holders"] < 0 and H.coef_disagrees(m)
    assert m["terciles"]["direction"] == m["deciles"]["direction"] == "higher"
    st = {k: getattr(C, k) for k in ("HOLD_BOOTSTRAP_REPS", "HOLD_BOOTSTRAP_SEED") + H.INCR_SETTINGS}
    lines = H.render_incremental(inc, st)
    def model_row(f):
        n = inc["contrasts"]["dead7 vs rest"][f]
        return [x for x in lines if x.startswith(f"| dead7 vs rest | {f} | {n['n_pos']} / {n['n_neg']} |")]
    assert len(model_row("pre_hold_holders")) == 1 and model_row("pre_hold_holders")[0].count(H.DAGGER) == 1
    assert len(model_row("pre_hold_sellers")) == 1 and H.DAGGER not in model_row("pre_hold_sellers")[0]
    assert f"{H.DAGGER} {H.DAGGER_NOTE}" in lines


def test_coef_disagrees():
    m = {"features": ["f"], "coef": {"f": 0.2}, "direction": "higher"}
    assert not H.coef_disagrees(m)
    assert H.coef_disagrees({**m, "direction": "lower"})
    assert H.coef_disagrees({**m, "coef": {"f": -0.2}})
    assert not H.coef_disagrees({**m, "coef": {"f": 0.0}, "direction": "lower"})
    assert not H.coef_disagrees({"features": ["f", "g"], "coef": {"f": -1.0, "g": 1.0}})


def test_no_footnote_without_a_disagreement(pilot):
    assert pilot.write().main() == H.EXIT_OK
    report = (pilot.out / "20261021-0315" / "report.md").read_text(encoding="utf-8")
    s = json.loads((pilot.out / "20261021-0315" / "summary.json").read_text(encoding="utf-8"))
    flagged = [m for ms in s[H.INCR_KEY]["contrasts"].values() for m in ms.values() if H.coef_disagrees(m)]
    assert (H.DAGGER_NOTE in report) == bool(flagged)
