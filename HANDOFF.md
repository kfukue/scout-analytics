# Scout analytics: handoff to Claude Code

## Session state (20 Oct 2026 PDT: main 9c053c8 after #39, #40, #41)

This section supersedes the ones below wherever they differ.

**Update, 20 Oct PDT (evening):**
- Merged: #39 (4b-1, venues, columns and row builder) and #40 (docs:
  the owner's decisions on the `data/` folder, the all-day run and A2).
- In progress:
  - 4b-2 (file writer and run helpers) on branch
    `work/2026-10-20-holder-writer`, worktree `holder-writer-20261020`.
    It adds `/data/` to `.gitignore` and changes the folder check to
    allow `<repo root>/data` and below. In review.
  - 4c must write to the exact path the folder check judged (the
    implementer's note: the check cleans `..` as text, while the
    operating system follows a link first).
- A2 (rug kind 'price'): the owner accepted the PM's four choices ("ok
  for the four choices"; details in the PM skill's "Owner decisions in
  force"). PR order: website, tracker, then the fill script.
  - Website PR in progress: worktree `price-rug-web-20261020`.
  - Points from the A2 spec (`investigate`, 20 Oct) that the tracker
    PR must carry:
    - README's "PROPOSED repair" SQL clears `rug_at` wherever
      `rug_block = 0`, so it would erase every 'price' time; add
      `AND rug_at_kind IS DISTINCT FROM 'price'` to it and to its
      finder query;
    - a drained pool must win over 'price': `RugAt` must skip 'price'
      rows, and `SetRugAtIfMissing` / `RugAtBackfillRows` must also
      take rows whose kind is 'price' (none of this exists yet);
    - the time comes from `scout_call_candles` in the DB, using the
      in-memory entry price (the column may still be NULL on a call's
      first run).
  - The fill script is plain SQL (A count, B fill with a backup
    table, C check, R rollback). It leaves the 29 drained "other"
    calls alone.

**Update, 20 Oct PDT (later the same day):**
- Merged: #36 (4a, the log reader), #38 (the deployer's liquidity add
  is no longer counted as a sale: `holderStats` and
  `deployerSoldBefore` take the liquidity transaction set), #37 (this
  section's earlier text).
- In progress: 4b-1 (venues, columns, row builder) is being re-applied
  on main 141e0c8 and changed to #38's signatures, uncommitted in
  worktree `holder-rows-main-20261020` (branch
  `work/2026-10-20-holder-rows-main`), in review. 4b-2 (writer and run
  helpers) is still on `work/2026-10-20-holder-rows` (f71ad45) and gets
  its PR after 4b-1 merges.
- Owner answers (20 Oct; recorded in the PM skill's "Owner decisions in
  force"):
  - output folder: `data/holders/` inside the repo (owner: "create a
    data folder in the repo and gitignore it"). PR 4b-2 is to add
    `data/` to `.gitignore` and change its folder check to allow `data/`
    and still refuse the rest of the repo and `scoutanalytics_data/`;
  - run time: all day beside `-track`, with the auto-pause (owner: "it
    shoudl run all day").
- Rug-time query R1 (owner ran it 20 Oct; causes: 0 has a rug time,
  a GeckoTerminal-tracked, b the 5 % price rule, c rug block with an old
  state, d rug block with no time, e other). Result, rugged calls only:
  0: 267 (calls 28 Jul-17 Oct); b: 751 (28 Jul-20 Sep); e: 29 (30 Jul-
  5 Aug); a, c, d: none. So about 72 % of "rugged" calls are the price
  rule (last price under 5 % of entry, a fall of more than 95 %, at the
  end of 30 days of tracking), and not detected as drained at that point:
  either the pool's quote side was at or above `SCOUT_RUG_LIQ_USD`
  (default $500), or its liquidity could not be checked (price unit not
  USD, v4 and Pons pools, a failed read of the pool's quote balance or
  of the quote's USD rate). Only 267 are drained pools
  with a time. b stops at 20 Sep only because later calls
  have not finished their 30 days yet. Options given to the owner: A2
  (PM's recommendation) keep `rugged`, give these calls
  `rug_at_kind = 'price'` with the first hourly candle that closed under
  5 % of entry as the time (candles already in the DB, no node time;
  only buckets with trades are stored, so the fill needs a fallback
  when no such candle exists;
  three PRs: tracker, website, one-off fill script with backup and
  rollback); B website wording only; C split them out of `rugged`
  (changes labels and views; not recommended now). A read-only query
  on the 29 "other" calls was sent too. Its result (20 Oct): all 29
  are on-chain v2/v3 calls priced in USD, calls from 30 Jul to 5 Aug,
  with `current_liquidity_usd` (the pool's depth, 2 x the quote side)
  at most $407 and mostly about $0, so a quote side under $205, well
  below the $500 threshold: they are drained pools whose rug block was
  never stored; their last price (up to 7x entry) is the price of a
  pool now under the rug threshold, not a sign the token held up. Two (9947, 9948) are now `repeat` rows. Inferred, not
  yet checked in the code history: the tracker of that time marked them
  rugged without a rug block and the later re-track kept the flag. They
  are not price-rule calls and must not get kind 'price'. The owner chose
  A2 on 20 Oct ("ok go with A2"; recorded in the PM skill's "Owner
  decisions in force"); next, `investigate` drafts the spec for the
  three PRs.
- The pilot's old-token share: answered 20 Oct, natural share, about
  10k `eth_getLogs` (owner: "go with your recommendation"; in the PM
  skill's "Owner decisions in force"). No export question is open now.
- Docs-only PRs: checked by `advisor` instead of `review` from 20 Oct
  (owner rule; PM skill, "How to work", step 5).

**Merged 19-20 Oct:** #32 docs (holder plan and P0 results, 19 Oct);
holder analysis code PRs 1-3, pure functions, no node, no DB (#33 and
#34 on 19 Oct PDT, #35 on 20 Oct 00:05 PDT (07:05 UTC)): #33 transfer decode, replay to `entry_block` and holder stats;
#34 early trading (snipers within 50 blocks of the first buy, bundles,
buyer/seller flow, the first 10 early buyers: owner, 19 Oct, "expand the
earlybuyer count to 10"); #35 liquidity events (v2 `Mint`/`Burn`, v3
`Mint`/`Burn`/`Collect`, v4 `ModifyLiquidity` filtered by pool id),
direction-aware: in an add transaction wallet -> pool is liquidity but
pool -> wallet stays a buy, so snipers bundled into the launch stay
visible; plus `liquidityStats`.

**Owner rule (19 Oct, review comment on #35):** every event topic lives
in `topics.go` ("Event topics in one file" in `scout-rules`). Done in
#35 for all 19 topics; function selectors (`sel...`) were left in place.

**Code PR 4 is split in three (one PR at a time, base `main`):**
- 4a, log reader (node only, no DB, no flag): launch search that stops
  at the first mint, 3-month cap, deployer (launch tx sender, block
  fallback, `lookup_failed` on node errors), token `Transfer` and venue
  liquidity streams up to 24 h after the call, removed/duplicate logs
  dropped, split at `entry_block`, request counts. Merged as #36
  (20 Oct).
  It also adds `rpcLog.Removed` and a ctx check in the chunked log
  reader (tracker results unchanged).
- 4b, row builder and writer (pure): venues and one excluded set per
  kind, `pre_hold_*` and `post24h_*` columns with the NULL rules,
  JSONL append with resume, CSV rebuild, out-folder guard, cost
  estimate; a leakage test (post-window logs never change `pre_hold_*`).
- 4c, the flag `-export-holders` (dispatched before `Migrate` and
  `registerTools`, reads in a read-only transaction), rate limit
  (default 4 req/s, one request in flight), node-lag and price-lag pause,
  `-dry-run` estimate, new `SCOUT_HOLDERS_*` names in `config.go`.
  From the 4b-1 review (20 Oct): `holderVenuesAt` adds the tracker's
  v2/v3 pool (`st.Pool`) as a pre-call venue with no creation-block
  check, so a pool created after the call gives
  `pre_hold_lp_applicable = true` with zero counts instead of NULL
  (rare: the tracker needs trades around the call for an entry price).
  4c should carry the creation block for v2/v3 pools too and gate them
  like v4 pool ids.
- Then PR 5, the report-only analysis script in `ml/`.
- Follow-ups: LP-token burn/lock share not yet measured. (The
  deployer's LP add counted as a sale: fixed by #38.)

**Open owner questions (export, needed for 4c / the pilot run):**
1. old tokens (7-90 d) at their natural share in the pilot (about 10k
   `eth_getLogs`) or oversampled (up to about 32k)? Estimates by the
   19 Oct export plan from the Q3 age mix and 200k-block log reads
   (they replace the 8 Oct "about 8k", which predates the 24 h post
   window). PM recommends natural share. Answered 20 Oct: natural
   share (update above);
2. the server output folder: answered 20 Oct, `data/holders/` in the
   repo, to be git-ignored (update above);
3. daytime run beside `-track` with the auto-pause, or a quiet window?
   Answered 20 Oct: all day (update above).

**Rugged calls without a rug time (investigated 19 Oct, not changed;
R1 run 20 Oct: result and options A2/B/C in the update above; the 5 %
rule is in commits of 1 Oct, not 6 Oct as written below):**
most likely the tracker's 5 % price rule (`tracker_onchain.go`, last
price under 5 % of entry at the end of tracking) sets `rugged` with no
`rug_block` and no `rug_at`, by design; `-backfill-timing` only picks
rows with a `rug_block` and state version >= 2. Not yet reconciled: the
5 % rule dates from 6 Oct, yet on 9 Oct all 245 rugged calls had a rug
time; query R1 separates the causes. The owner was given one read-only query (R1) to
confirm, and two options: A, a new `rug_at_kind = 'price'` with an
approximate time (tracker, website, ops script with backup and rollback,
three PRs); B, website wording only. Answered 20 Oct: A2 (update
above).

## Session state (19 Oct 2026, 12:25 PDT (19:25 UTC): main 3ff52fa after #31)

This section supersedes the ones below wherever they differ.

**Holder analysis plan approved (owner, 19 Oct):** "1. go with that
2. ok 3. ok 4. go back like 3 months 5. yes". The plan, the answers and
the stop rules are in section 4 item 4 ("Plan refreshed ... and
approved"). The look-back cap is 3 months, not 7 days.

**P0 done (owner ran Q1-Q5b, 19 Oct):** results in section 4 item 4.
4,761 first calls with a 7d result; 1,481 `dead7`, 883 `held2x7`, so
ML stays in the plan; none of the 4,752 with a known age was older
than 90 days at the call (9 unknown); 91 %
had no Perceptor scan at the call, so the pilot's strata are the posted
holders and the age at the call; the posted holder count alone already
separates `dead7` (9 % to 62 % by quintile). The query file is a
git-ignored scratch file (`.claude/worktrees/_scratch/holder-p0/`);
Q6 (pilot sample) is run when the pilot is ready.

**In progress:**
- Code PR 1: pure `Transfer` decode, replay to `entry_block` and holder
  stats, tests on synthetic transfers only; worktree
  `.claude/worktrees/holder-replay-20261019`, branch
  `work/2026-10-19-holder-replay`; implemented, in simplify then review.
  Follow-ups for the log-reader PR: drop duplicate logs on (block, log
  index) and logs removed by a reorg.
- Owner's notes record: the three 18 Oct evening decisions, the 19 Oct
  stop of the ML work and this approval are added after this PR merges.

## Session state (19 Oct 2026, 11:50 PDT (18:50 UTC): main 61a39b9 after #27, #29, #30)

This section supersedes the ones below wherever they differ.

**Merged 19 Oct:** #27 docs; #29 risk filter step A2a (collapse filter
cut-off fixed in advance and a per-bucket verdict, report-only); #30
retraining PR 3, `ml/promote.py` promote / rollback / status (manual).

**Owner decision (19 Oct): ML work on the call-scoring model stops.**
Owner: "stop the work on the ML it's not really adding value. Maybe
focus on the holder analysis that we talked about earlier and use ML on
those data instead". Recorded in the PM skill.
- Stopped: retraining series PRs 4-10 (lock, `run_training.sh
  --candidate`, `--src deployed`, `promote.py check`, `-notify-file`,
  wrapper, systemd timer) and risk-filter steps A2b-H (below). Nothing
  of them was started. The owner questions for B-E are withdrawn.
- Unchanged: the live model, the scoring service, the delivery score
  line and the site's scores keep running. A manual retrain still works
  (`run_training.sh` runs `train.py`, which writes `LATEST`); `promote.py rollback` and
  `status` are there if needed.
- Next: section 4 item 4, "Early-transaction pattern analysis" (holder
  concentration, early buyers and sellers, deployers and funders, wallet
  overlap), with ML on that data. `investigate` is refreshing the 8 Oct
  plan against current main, with a recommendation for each of the five
  open owner questions. No coder work until the owner approves the plan.
- Owner's notes record: the three 18 Oct evening decisions and this one
  are added after this PR merges.

## Session state (18 Oct 2026, 22:55 PDT (19 Oct 05:55 UTC): main 939a289 after #25, #26, #28)

This section supersedes the ones below wherever they differ. This docs
PR is #27.

**Merged since the section below:** #25 docs (node URLs not redacted;
weekly automated retraining); #26 `write_pointer`: `train.py` writes
`LATEST` atomically (temp file + `os.replace`), first PR of the
retraining series. Owner's notes record updated for #24, #25 decisions.

**Prod report 20261018-2204** (plain retrain, first with the collapse
filter section; that model is live):
- Gates: 3d PASS 2.02 (seeds 1.80-2.32), 7d FAIL 1.98, 1d FAIL 1.10.
  3d/7d still sit on the line of 2.
- Collapse filter (skip the top 30 % by collapse score), pooled
  walk-forward [95 %]: collapses caught 1d 0.381 [0.358, 0.436], 3d 0.380
  [0.350, 0.444], 7d 0.395 [0.352, 0.488] (random 0.30); runners lost 1d
  0.323, 3d 0.143, 7d 0.154 (random 0.30); kept minus all (mean net %)
  1d +11.0 [+9.9, +12.9], 3d +10.7 [+9.1, +13.2], 7d +9.2 [+6.6, +13.2];
  every counted week positive. Test part: caught 0.47-0.52. Kept calls
  still lose on average (3d -33.8 % vs -44.5 %). Cut taken after seeing
  each week's scores, so likely optimistic for a fixed threshold.
- Adversarial AUC 0.88-0.90 (inputs drift).

**Owner decisions, 18 Oct evening PDT (19 Oct UTC; recorded in the PM
skill; the owner's notes record is updated after this PR merges):**
1. Risk filter, "go with your recommendation": (1) the filter is
   validated per bucket when it beats random skipping (lower end of
   pooled collapses caught > 0.30 and lower end of kept-minus-all > 0;
   all three pass on 20261018-2204; set after seeing that report, so it
   must hold again on the next retrain), instead of the single-run
   >= 40 %; (2) a "collapse risk: high" flag on the call list, runner
   scores unchanged; (3) 3d and 7d only (1d loses runners at the random
   rate); (4) delivery line later. Step A2a is in progress (below).
2. Retraining goes live automatically (the PM had recommended by hand
   for the first 3 runs). Promotion only when all hard checks pass
   (H1 run completed; H2 loads and scores a sample row with the deployed
   code; H3 trained from the deployed commit; H4 same features,
   thresholds and categories as live; H5 rows, eligible and last date
   not lower; H6 no bucket loses its model) and no bucket's median
   pooled lift drops more than 0.20; else the old model stays and the
   notice says why. Not "no PASS->FAIL": on the three runs before
   20261018-2204 (20261015-0554, -1944, -2148) it would have held two.
   PM proposals not yet confirmed by the owner: notice to the private
   delivery group; diagnostics on; Sundays 10:00 PDT (17:00 UTC; 09:00
   PST after 1 Nov).
3. The website's "not scanned" count (4,340) stays as it is. It is
   calls without a real Perceptor verdict: history from before Perceptor
   scanning, plus the re-scan lane's limits (30 days, 100/day, no
   rugged/wiped tokens, 2 failures). No change to `SCOUT_RESCAN_MAX_AGE`.

**Pons question closed (18 Oct PDT (19 Oct UTC)).** Follow-up queries: the 37
`no_pool`/`gave_up` calls all say "no Uniswap v2/v3/v4 pool found" (35)
or "no trades around the call time" (2), none a node error. By address
ending, Pons calls have no common ending; 123 calls end in `7777`
(0 tracked as Pons, 14 `no_pool`/`gave_up`). PM's reading: `7777` is
another launchpad's vanity ending, and those 14 never reached a Uniswap
pool. No misjudged Pons tokens found; nothing to repair. Optional idea,
not planned: track that launchpad's own trading.

**Deployed (owner, 18 Oct):** after `git pull`, the listener
(`-listen-only`), `-track` and `-web` were restarted, so #16-#20 run on
prod.

**Retraining series (plan by `investigate`, 18 Oct; one PR at a time,
base main):** 1 atomic `LATEST` (#26, merged) → 2 `train.py
--candidate` (writes `CANDIDATE`, never `LATEST`; refuses an existing
version dir) → 3 `ml/promote.py` promote/rollback/status (loads and
scores a sample row first; `PREVIOUS`; `promotions.log`) → 4 `flock` in
`run_training.sh` → 5 `run_training.sh --candidate` → 6 `--src deployed`
+ `meta["source_commit"]` → 7 `promote.py check` + allow-listed
`summary.txt` (thresholds in `config.py`) → 8 Go `-notify-file PATH`
(Bot API sender, no DB, no user session; independent) → 9
`ops/scout-train.sh` wrapper (PAUSE file, skip if `-track` up < 30 min)
→ 10 systemd service + timer (weekly, owner installs).

**PR 2 merged as #28** (19 Oct UTC): `train.py --candidate` writes
`CANDIDATE` last and never `LATEST`; any run refuses an existing version
dir. `run_training.sh` 407-411 still expects `LATEST` (PR 5);
`CANDIDATE` is never cleared, so `promote.py` must check the named dir
has `meta.json` and `report.md` and was written by the current run.

**In progress, not committed (agents may have been stopped by a
restart; check each worktree's `git status` and the agent's report
first):**
- Retraining PR 3, `ml/promote.py` promote/rollback/status: worktree
  `.claude/worktrees/promote-20261019`, branch `work/2026-10-19-promote`
  (from 939a289).
- Risk filter step A2a (training only: fixed cut-off from the pre-test
  held-out rows' collapse scores, `collapse_reference` with quantiles,
  per-bucket `collapse_filter.verdict` by the owner's rule for `3day`
  and `medium`, `thresholds.filter_rules`, report table; config
  `FILTER_BUCKETS`, `FILTER_RULES`, `FILTER_RULES_SEEN_REPORT`,
  `FILTER_CUTOFF_MIN_ROWS`; gates unchanged): worktree
  `.claude/worktrees/collapse-cutoff-20261019`, branch
  `work/2026-10-19-collapse-cutoff` (from 939a289). Then A2b
  (fixed-cut-off re-measurement of the walk-forward, information only),
  B serve the flag, C store it (schema; run/rollback scripts), D
  `/api/calls`, E call list, F live check, H RUNBOOK.
- Owner questions for B-E (asked, no answer needed yet): does any later
  validating run count, or two in a row; hide the flag when a bucket is
  not validated (PM) or show "n.v."; 3d flag in the detail view (PM).

## Session state (18 Oct 2026 PDT: main c72e6b7 after PRs #18-#20, #22-#24; #21 closed)

This section supersedes the ones below wherever they differ.

**Merged 18 Oct:** #24 rule: setting-name constants live in one
`config.go` per Go package (rule in `scout-rules`, Hard rules); #18 DB
connection URL never shown in parse errors, with `databaseURLSetting`
in the new root `config.go`; #22 docs; #19 busy node no longer makes a
token "not Pons"; #20 marker words must start a word. Not yet
deployed (as far as the PM knows): one pull, rebuild and restart of the
listener, `-track` and `-web` covers #16-#20.

**Owner decision (18 Oct): node URLs are not redacted.** The node URL
appears only on the prod server (its logs and database), and anyone who
gets into that box already has `.env`, so #21 was closed unmerged.
The node-URL part of the URL-count query and its cleanup are dropped
too. Not covered by the decision: old `scout_deliveries` errors may hold
Bot API URLs with the bot token (from before `botAPIRedact`); that row
of the query is optional for the owner and undecided.
Supersedes the node-URL half of the 15 Oct "two URL-leak fixes"; #18
(DB connection URL) stays.

**Also merged 18 Oct:** #23 report-only "collapse filter" evidence
(risk-filter step A). Next: a plain retrain (no `--variants`) on the
server; its `report.md` gives step A's numbers for the owner's
risk-filter decisions.

**Pons candidates (owner's read-only query, 18 Oct; answers the 15 Oct
query below):** 37 calls `no_pool`/`gave_up`, 2,600 tracked on an
ordinary v2/v3/v4 pool (an upper bound on Pons tokens mis-tracked as
ordinary pools, nearly all really ordinary), 2,899 tracked as Pons. The 37 are the likely victims of the
old busy-node bug (#19); two sharper read-only queries (the 37 with
their errors, URLs masked; ordinary-pool calls whose pool appeared after
the call) were given to the owner, results pending.

**Retraining (owner, 18 Oct; supersedes the PM's two-weekly manual
proposal in the 15 Oct section):** automate it, weekly. `train.py` writes
`LATEST` on every run and serving re-reads it, so a scheduled run would
go live unread: first a candidate/promote step with a rollback, then the
timer. The plan for the PR series is in the section above; the owner
still decides automatic vs manual promotion.

## Session state (15 Oct 2026 PDT (15-16 Oct UTC), evening: main 9972b62 after PRs #13-#17)

This section supersedes the ones below wherever they differ.

**Merged 15 Oct:** #13 wallet variant "last 14 days" (lookback); #14 docs.
The owner retrained on the server with `--variants`.

**Prod report 20261015-2148:**
- Gates: all FAIL. 3d 1.93 (seeds 1.59-2.28), 7d 1.93 (1.62-2.24), 1d
  1.11 (seed-7 lower end 0.97). One more row than report 20261015-1944
  moved 3d and 7d by about 0.07: both sit just under 2, inside run-to-run
  noise. Skipping the top 30 % by collapse score removes about half of
  the collapses on the saved model's test part (0.48-0.50), but only
  0.38-0.40 pooled over the walk-forward weeks (random skipping: 0.30;
  the gate's 40 % is the single-run test-part figure).
- Lookback variant "wallet reputation, last 14 days": drift 0.97 vs
  baseline 0.90 (full variant 0.98); its 14-day coverage still rises
  (about 0.33 in September to 0.67 in October). 3d holds rules 1-4 (2.15
  vs 1.93) and fails drift: "not adopted; drift persists with a 14-day
  window", no further step pre-registered. 7d 1.93 = baseline; 1d fails.
- Full wallet variant at 3d over three reports: 2.06, 2.08, 2.15, while
  the baseline moved between 1.93 and 2.10; its inputs always carry heavy
  drift, so a time proxy cannot be ruled out.

**Owner decisions (15 Oct):** wallet work parked; `SEED_VARIANTS` kept;
no further tuning, risk-filter study instead; the two URL-leak fixes
approved. Recorded in the PM skill.

**Merged later on 15 Oct:** #15 agent files point to the `_scratch`
folder; #16 Timing tab KM median described as time held at 2× (deploy:
`-web`); #17 `chainlink()` sets `noState` only on a real no-state error
(deploy: `-track`). Whether #16/#17 are deployed is not confirmed.

**Open PRs (base `main`, independent; reviewed PASS; tested locally only;
nothing deployed):**
- This docs PR.
- #18 DB connection URL never shown in parse errors (`ConnStringError`);
  connect errors still show host/user/database (never the password).
  Deploy: listener, `-track`, `-web`.
- #19 a busy node no longer makes a token "not Pons" (`ponsCallErr`;
  EVM failures such as invalid opcode count as the contract's answer).
  Deploy: `-track`.
- #20 marker words must start a word; "non-" cancels them (PM choice:
  start of word only, so "blacklisted"/"dangerous" still match; "scamper"
  accepted). A Perceptor fallback reply saying only "take precaution"
  is now unknown, not caution. Deploy: listener.
- #21 node URLs never stored in errors or logged (`urlredact.go`; start-up
  lines show scheme://host). Deploy: `-track`.

**Read-only SQL given to the owner (results pending):**
- Error texts that still contain a URL per table (`scout_call_tracking`,
  `scout_investigations`, `scout_deliveries`; the last may hold Bot API
  URLs with the bot token from before `botAPIRedact`). Rows found → a
  cleanup PR with run and rollback scripts.
- Pons candidates in `scout_call_tracking` (`no_pool`/`gave_up`, generic
  v2/v3/v4 state without `pons_curve`, tracked as Pons); confirming a
  candidate needs `-price-check` on the server.

**Risk filter (study done, `investigate`):** the site and delivery line
are runner-based; the collapse number is marked "n.v." with the runner
side. Plan of small PRs: A report-only "collapse filter" evidence
(collapses caught, runners lost, kept vs all; test part and pooled
walk-forward; in progress on `work/2026-10-15-collapse-filter-evidence`)
→ A2 `collapse_reference` + collapse verdict → B serve a collapse rank →
C store it (schema change with run/rollback) → D `/api/calls` → E call
list wording → F Model insights live check → G (optional) delivery line;
H RUNBOOK cadence. Owner decisions after A's numbers: collapse validation
rule (single-run vs pooled), site wording, buckets, delivery line.
Cadence: the PM proposed a manual retrain every two weeks (no cron:
every retrain would go live unread); owner answer pending.

## Session state (15 Oct 2026 PDT: main d3a2197 after PRs #10-#12)

This section supersedes the ones below wherever they differ.

**Merged 15 Oct:** #10 wallet variant "quality only" and rug prior 0.30;
#11 "raw dex + dex_family" across 7 seeds; #12 docs. The owner retrained
on the server with `--variants`.

**Prod report 20261015-1944:**
- Gates: all FAIL. 3d 1.99 (seeds 1.77-2.12), 7d 1.86 (1.62-2.17), 1d
  1.11. With only 18 more rows than report 20261015-0554, 3d moved from
  2.10 to 1.99: 3d and 7d sit at about 2x and runs move by 0.1-0.2, so
  PASS/FAIL near 2 is mostly chance. The PM proposed no gate change. So the site
  will mark every bucket "n.v." for scores from this model (inferred from
  the gates, not checked on the site).
- Collapses: skipping the top 30 % by collapse score still removes about
  half of them at every horizon. The top 10 % by runner score still lose
  money on average (3d -15.0 % vs -44.6 % for all calls; 7d -24.1 % vs
  -51.4 %). Call volume: 50 calls in the week of 12 Oct.
- "raw dex + dex_family" across 7 seeds: no gain (3d 1.99 vs 1.99, 7d
  1.93 vs 1.86, 1d 1.13 vs 1.11). The 2.34 of the previous report was one
  seed. Not adopted.
- Wallet reputation (rug prior 0.30 now matches the observed 31.4 %):
  - Full variant: 3d holds rules 1-4 (median 2.08 vs baseline 1.99) and
    fails only rule 5 (drift), so by the pre-registered rules the next step
    is a lookback variant. The gap is within run-to-run noise, and in the
    previous report the same variant was below the baseline. 1d and 7d do
    not pass.
  - Quality only: below 2 everywhere (3d 1.77); drift still 0.96-0.97, led
    by `wrep_rug_lift_max`: the wallet inputs themselves grow with the
    history, not only the two coverage inputs.

**Owner decision (15 Oct, "go with b"):** run the lookback variant as the
pre-registered rules say (the PM had recommended parking the wallet work).
Recorded in the PM skill.

**Open PRs (base `main`; tested locally only; nothing deployed):**
- #13 `work/2026-10-15-wallet-lookback` (600d634, reviewed PASS):
  report-only variant "wallet reputation, last 14 days"
  (`WREP_LOOKBACK_VARIANT_DAYS` 14, fixed before any result), same adoption rules,
  across 7 seeds; a drift-only failure of it reads "drift persists with a
  14-day window" (no further step pre-registered).
- This docs PR (independent of #13).

**Owner's next steps:** merge #13, retrain on the server with
`--variants`, send `report.md`.

**Not decided:** dropping "raw dex + dex_family" and "quality only" from
`SEED_VARIANTS` (they have given their answer; saves about 1.5 min per
retrain); the two URL-leak fixes (DB connection URL in an error message,
node URL in stored errors).

## Session state (night of 14-15 Oct 2026 PDT (15 Oct UTC): main 062193c after new-repo PRs #1-#9)

New-repo PR numbers. Everything below this section is older; where the
"14 Oct night" text below differs, this part wins.

**Prod report 20261015-0554** (first with 7 seeds and the wallet variant):
- Gates: 3d **PASS** (median pooled lift 2.10; seeds 1.71-2.27), 7d FAIL
  (1.95; 1.79-2.03), 1d FAIL (1.12). The previous run had 7d PASS 2.02 and
  3d FAIL 1.75: both sit on the line of 2, so which one passes is mostly
  noise. So the site will mark 3d validated and 7d "n.v." for scores
  from this model (inferred from the gates, not checked on the site).
- The top 10 % still lose money on average (3d -7.2 % vs -44.7 % for all
  calls; 7d -20.4 % vs -51.5 %). The saved 3d model's own test-part lift is
  1.33 [0.71, 2.35]; the pooled pass leans on the weeks of 8 and 15 Sep.
  Call volume keeps falling (41 calls in the week of 12 Oct).
- Wallet reputation (full variant): does not pass in any bucket. Rule 1
  fails (median lift below the baseline: 3d 2.06 vs 2.10, 7d 1.87 vs 1.95,
  1d 1.10 vs 1.12) and rule 5 fails (drift AUC 0.975-0.984 vs 0.90). Since
  rule 1 fails too, the lookback variant is not the next step.
- Found: the rug prior `WREP_PRIOR_RUG` 0.15 was half the observed rug
  rate (31.5 %), so wallets with a long history tended to count as bad.
- Without dead calls, pooled lift is 2.54 (3d) / 2.24 (7d). The dead score
  (AUC 0.93) would pass its proposed gate, but 136 of 148 dead calls in its
  two biggest weeks were one uniswap_v4 wave: not adopted.
- "raw dex + dex_family" at 7d: seed-7 pooled lift 2.34 vs 1.95 (single
  seed only).

**Owner decisions ("yes to both", night of 14-15 Oct PDT (15 Oct UTC)):** rug prior 0.30;
run "raw dex + dex_family" across all 7 seeds. Recorded in the PM skill.

**Open PRs (base `main`; tested locally only; nothing deployed):**
- #10 `work/2026-10-14-wallet-quality-only` (71a6f24, reviewed PASS):
  variant "wallet reputation, quality only" (option b) and
  `WREP_PRIOR_RUG` 0.30 (report says it was changed after a result).
- #11 `work/2026-10-15-dex-seed-variant` (21547b5, reviewed PASS),
  stacked on #10 (merge #10 first): "raw dex + dex_family" in
  `SEED_VARIANTS`.
- This docs PR (independent of #10 and #11).

**Owner's next steps:** merge the PRs (#10 before #11), then
retrain on the server with `--variants` and send `report.md`.

### Earlier the same night (before the report)

**Merged in the new repo (14 Oct):**
- #1 model without `calls_prev` inputs (`EXCLUDED_INPUTS`); the report
  keeps a "with calls_prev counts (default before 14 Oct)" variant.
- #2 file-access rule (repo and the owner's notes folder only; scratch
  in `<repo root>/.claude/worktrees/_scratch/<task>`; no system digging).
- #3 `-export-live-buys PATH` (CSV of `scout_call_live_buys`).
- #4 / #5 docs: repo move recorded; no local or server paths in the repo
  (`<repo root>`, `$REPO` for the server checkout, `$ML` for the ML
  folder); PR texts are public too.
- #6 `ml/scout_ml/wallet_rep.py`: point-in-time live-buy wallet
  reputation (8 `wrep_*` features; history = earlier first calls of other
  tokens whose 7d outcome was known before the call; never `rugged`).
- #7 runner-lift gate median over 7 seeds (7/11/23/31/47/61/89; seed 7
  is still the saved model).
- #8 wallet reputation as a REPORT-ONLY variant; `run_training.sh
  --variants` exports `live_buys.csv`; report blocks "Wallet reputation
  data" and "pre-registered adoption rules" (fixed 14 Oct: median lift
  >= 2 and >= baseline; seed-7 lower end > 1; sim lower end > 0;
  collapses removed >= 40 %; no added drift: AUC <= baseline + 0.02 and
  no wrep_* in the top 3 drifting inputs with share >= 0.40). The saved
  model is byte-identical with or without live buys.

**In progress then (done since: PR #10):**
- Branch `work/2026-10-14-wallet-quality-only` (from 8d84415). Owner
  decision 14 Oct, option (b): a second report-only variant "wallet
  reputation, quality only" = the wallet variant minus the two coverage
  features (`wrep_n_known`, `wrep_known_share`, which grow with calendar
  time), with the same adoption rules, shown next to the full wallet
  variant. Finished, reviewed and opened as PR #10.

**Owner's next steps:**
- After the (b) PR merges: retrain on the server with `--variants`
  (refresh `$REPO` with `git pull`, copy the new `run_training.sh` into
  `$ML`, run it from `$REPO`; see RUNBOOK) and send `report.md`.
  Expect a longer run (7 seeds plus two wallet variants).
- Optional: set `REPO` and `ML` in the server's `~/.bashrc`.
- Optional (his files): `.claude/agents/review.md` line 27 and
  `.claude/agents/verify.md` lines 57-58 still say "scratchpad".

**Model facts (previous prod report 20261014-2211, first without
calls_prev):** 7d PASS (median 2.02), 3d FAIL (1.75), 1d FAIL; drift AUC
0.89-0.90 (was 0.99). The same settings moved by up to ±0.4 between two
near-identical runs, hence the 7-seed gate. The top 10 % still lose
money on average (-13.6 % at 7d vs -51.7 % for all calls).

**Expected for the wallet test:** the full wallet variant will probably
fail the drift rule (coverage grows from August); the quality-only
variant tests the signal more cleanly; a lookback variant
(`WREP_LOOKBACK_DAYS`) is for later, with more data.

**Owner's notes:** kept outside the repo (path in his user-level
`CLAUDE.md`); `rules.md` there was refreshed and pushed on 14 Oct.

## Repo moved and history rewritten (14 Oct 2026, afternoon PDT (14–15 Oct UTC))

The repo is public, so on 14 Oct the owner rewrote its history to remove
local paths and his personal email:
- git-filter-repo replaced every local path with placeholders
  (`<repo root>`, `<go-work>`, `<tmp>`, `<postgres dir>`, `Users\<user>`),
  and a mailmap set every commit author to his GitHub no-reply address.
- The old repo was renamed `kfukue/scout-analytics-archive` and made
  private. It keeps the old PR pages (#1-#68). **PR numbers in text
  written before this section, and in merge commits older than 137b734,
  refer to that archive; `#1`-`#3` in this section are the new repo.**
- A new public `kfukue/scout-analytics` was created with the rewritten
  history, and its `main` protection restored (PR required, applies to
  admins, no force-push or deletion). Its PR numbers restart at #1. In the
  new repo, #1 = "no calls_prev" (was #66), #2 = the file-access rule (was
  #67), #3 = the live-buys export (was #68).
- The PC main checkout and the server (`$REPO` and `$ML/src`) were reset to
  the new `main` (6c9287e), keeping untracked `.env`, the session file,
  data and venvs. The owner's GitHub account now keeps its email private.
- Rule since then: no absolute local paths in repo files (`scout-rules`).
- After #1: retrain on the server (without calls_prev) with `--variants`;
  report pending.

## Session state (14 October 2026 UTC, owner's late evening of 13 Oct: main e522918 after PRs #41-#53, archived numbering)

This section supersedes the states below wherever they differ.

**Main and prod:** main is at **e522918** (PR #53). #52 (cc0bc2c) and
#53 (e522918) are docs/skills only, so nothing to deploy: e522918 runs
the same code as 694e9e5 (PR #51). Prod: the owner
restarted the listener at 23:15:58 (his local time, 13 Oct) after merging
#45-#51. He was asked to confirm `git log -1` = 694e9e5 or later and to restart
`-web` for the Model 7d column; neither is confirmed yet. Earlier on 13 Oct
the owner said he had pulled and restarted the listener, `-track` and
`-web` after #42/#43, so the tracker should have #42. He was asked to
confirm 37deb10 then; not confirmed. If unsure, restart `-track` once.
The scoring service needs no restart for the models served today: #46
touches `build_features` (an extra `calls_burst_1h` column, unused with the
default `CALLS_PREV_INPUTS = "counts"`). Restart it before serving any
model that lists `calls_burst_1h` as an input.

**Merged since 29b5c2e** (merge commit; PRs #47-#49 have no first-parent
merge on `main`, see the incident below):

- #41 (7191526): this file and the PM skill. #43 (0dc52fa): the PM skill
  and `scout-rules`, adding the owner's small-PR rule (one function per
  PR, small functions).
- #42 (ebae905): the tracker's ETH/USD `slot0` and Pons bisection set
  `noState` only on a real no-state error.
- #44 (37deb10): Timing tab Kaplan-Meier captions match the computation;
  unused tooltip classes dropped.
- #45 (f9b1e70): web scores 1/3, storage. `passed` column on
  `scout_call_predictions` (repeat-safe), every bucket of a score stored in
  one transaction, NOTIFY `score`, leakage guards. Closes "Persist the ML
  `passed` flag".
- #48 + #49, merged to `main` as #50 (0326bef): web scores 2/3 and 3/3.
  Scores in the snapshot, `/api/calls` (`model`, `model_scores`,
  `sort=model`) and SSE `score`; only scores made within 24 h of the call;
  the public GCP side runs with `ModelScores=false`. The call list has a
  sortable "Model 7d" column after Perceptor, a "Model (scored at call
  time)" block in the row detail, and a legend entry. The call list now
  has 17 columns.
- #46 (26a26f2, its own merge) and #47 (reached `main` through #51,
  694e9e5): report-only ML
  variants "calls_prev as a burst ratio" and the table "calls_prev variants
  across seeds"; shared helper `validate.seed_lift_median` (gate behaviour
  unchanged).

**Incident (stacked PRs):** #48, #49 and #47 were opened with their base
set to the previous work branch. The owner merged each one while its base
branch still existed, so GitHub merged them into those branches, not into
`main`. Re-sent as #50 and #51 (same commits, base `main`). Owner rule
since then: every PR has base `main` unless he names another branch (PR
#52, `CLAUDE.md`).

**Merged rules (14 Oct UTC):**
- #52 (cc0bc2c): new root `CLAUDE.md`. Every PR has base `main` unless
  the owner names another branch. Dependent PRs wait for the previous
  merge by default. Check the base with `gh pr view <N> --json
  baseRefName` before sending a link. PM skill steps 7/8.
- #53 (e522918): every database change, structure or data, ships with a
  run script and a rollback script. Full rule in `scout-rules`, "Database
  changes: run script and rollback" (backup tables, `R_rollback.sql`,
  rollback order for schema changes, test run → R → run). The PR body
  suggests two lines for `.claude/agents/review.md` (the owner's file;
  not known whether added).

**Open PRs from the overnight session (all reviewed PASS, all base `main`;
tested locally only; nothing deployed):**

| Worktree | Branch | From | Work |
|---|---|---|---|
| model-startup-line-20261014 | `work/2026-10-14-model-startup-line` | 694e9e5 | **PR #56** (465caae, reviewed PASS): listener start-up line: model scoring on/off (also off with SCOUT_DB=off), URL without user info/query/fragment, budget. Deploy: listener restart |
| log-invite-link-20261014 | `work/2026-10-14-log-invite-link` | 694e9e5 | **PR #55** (a18cbe6, reviewed PASS): invite link redacted as `<invite>` in start-up lines and errors; `scout_deliveries.target` keeps the real link (re-ask matches on it). Deploy: listener restart |
| exit-rule-20261014 | `work/2026-10-14-exit-rule` | cc0bc2c | **PR #58** (3aae8fc): Analytics idea 1, "Exit rule" table on Peak vs final (sell at first touch of 2×/3×/5× vs hold; before tax; upper bound). Page-only |
| data-health-20261014 | `work/2026-10-14-data-health` | #58 branch | **PR #59** (4c4046a), merge after #58: idea 2, "Data health by week" + quiet share by week × pool family on Overview; all calls, filters ignored. Page-only |
| model-live-api-20261014 | `work/2026-10-14-model-live-api` | cc0bc2c | **PR #60** (398289b): idea 3a, `GET /api/model-live` (scores stored at call time, from the snapshot; 404 and nothing built when `ModelScores=false`). Go only |
| model-insights-page-20261014 | `work/2026-10-14-model-insights-page` | #59 branch | **PR #61** (4250e24), merge after #58-#60: idea 3b, the Model insights section (coverage, per bucket: scored, outcome known, top 10% by at-call rank, runner rate and live lift, capped mean return, validated; extreme outcomes left out as in training). Page-only |
| exit-extremes-20261014 | `work/2026-10-14-exit-extremes` | #61 branch | **PR #63** (f23e8f7), merge last: exit table leaves out a window peak/return above +100,000 % (drained pools). Page-only |
| rank-wording-20261014 | `work/2026-10-14-rank-wording` | e522918 | **PR #62** (f0178a1): "top N%" described correctly (vs the calls the model was tested on at training, not "recent calls"); text only, SQL comment only |
| claude-md-safety-20261014 | `work/2026-10-14-claude-md-safety` | e522918 | **PR #54** (e58be2b, reviewed PASS): `CLAUDE.md`: "Start here", DB-rule pointer, safety-net "Never" list (port 5432, no listener/backfill/track on a real DB, `onchainStateVersion`, no git writes in the main checkout) |
| handoff-20261014 | `work/2026-10-14-handoff` | 694e9e5 | this update |

Merge order: #54, #55, #56, #57, #62 in any order. The analytics chain
goes #58 → #59 → #60 → #61 → #63. #59, #61 and #63 were stacked on the
previous branch because they edit the same page files and the owner was
away; each has base `main` and states its order. Deploy: one listener
restart (#55, #56) and one `-web` restart (#58-#63). Then the browser
checks listed in each PR.

All of #54-#63 were merged by the owner on 14 Oct, 11:26-11:37 PDT
(18:26-18:37 UTC); main is 2c8af02.

Owner decisions, 14 Oct, after the merges:
- Dates in docs: local Pacific time first, UTC in parentheses. The full rule
  is in `scout-rules` "How you report".
- Drained-pool outcomes above +100,000 % are left out page-wide on
  Analytics, as in training. In progress on branch
  `work/2026-10-14-outliers-pagewide`.
- The stored invite link in `scout_deliveries.target` stays as it is.
- Model 20261014-1842 (`--variants`): 3d PASS (2.10), 7d FAIL (1.86).
  Across seeds, "no calls_prev inputs" gives 3d 2.14 and 7d 2.10, both
  passing every gate, with drift AUC 0.99 → 0.90. The burst ratio is worse.
  Owner decision 14 Oct: "no calls_prev" becomes the default
  (`EXCLUDED_INPUTS`; branch `work/2026-10-14-no-calls-prev`); the report
  keeps a "with calls_prev counts (default before 14 Oct)" variant, and the
  burst variant is dropped. After the merge, on the server: pull `$REPO`;
  refresh the training checkout (`git -C "$ML/src" fetch` then
  `git -C "$ML/src" checkout --detach origin/main`) and check
  `grep -n '^EXCLUDED_INPUTS' "$ML/src/ml/scout_ml/config.py"` names both
  calls_prev inputs; restart `scout-model`; retrain with `--variants`.

**Scoring on prod is working** (owner's read-only queries, 14 Oct UTC).
Live-handled calls since 13 Oct 12:00 UTC: 10793, 10795, 10796 and 10798
are unscored (by timing they appear to predate the scoring service); 10804
(19:25 UTC) and 10812 (14 Oct 00:36 UTC) are scored. Only 9 calls were handled live on
13–14 Oct, so the website column and any live-accuracy view fill slowly.
Both scored calls (6 rows, `20261013-1909`) have `passed` NULL, because
the listener then ran pre-#45 code. The PM gave an optional backfill with
a backup table and a rollback (`scout_call_predictions_bak_passed_20261014`);
not known whether it was run.

Watch item (investigate, 14 Oct): `scoreCall` shares one
`SCOUT_MODEL_TIMEOUT` (10 s) between the live pre-call stats on the Nitro
node and the `/score` request. If pre-call uses the whole budget, the call
gets no score at all (the feature-row read fails on the expired context).
Not seen so far (2 of 2 scored). Signs in the listener log: `score <CA>:
feature row: context deadline exceeded (precall 10s; …)`. Quick remedy:
`SCOUT_MODEL_TIMEOUT=25s` plus a listener restart, at the cost of up to
~30 s more delivery delay.

**Latest model (20261014-0559, served automatically via `LATEST`):**
short FAIL (median pooled lift 1.11), 3day FAIL (1.88), **medium PASS**
(2.02 of 2.02/2.02/1.61, saved-seed lower end 1.39, sim difference +38.9
[+27.4, +46.1], collapses 0.489), long skipped (3108 rows over 48 days).
Weaker than 1909 (medium 2.26). The top 10% at 7 d still averages −12.9 %
(vs −51.7 % for all calls). Adversarial AUC 0.99; `calls_prev_24h` is
40–45 % of it and is the 2nd input of the medium runner model (0.074).
3day: the 4-week rolling model beats expanding (2.15 vs 1.88); medium
the reverse (1.37 vs 2.02). Dead-call score: LightGBM would pass the
proposed gate (3.75×, 3/3 weeks), but 65 % of dead calls came in two
uniswap_v4 waves (late Sep). No variant tables: the training checkout did
not yet have #46/#47, and the run was without `--variants`. Next: retrain with `--variants` and send `report.md`.

**Analytics page ideas (investigate report 14 Oct). Owner decision: go
with the PM's order. 1, 2, 3a and 3b are built as PRs #58-#61 (see the
table above); 4 (copy table) is not started:**
1. **Exit rule** (on "Peak vs final"): return if sold at the first
   touch of 2×/3×/5×, else held to the horizon. `(m−1)·100` when
   `peak_h ≥ (m−1)·100`, else `ret_h`. Page-only, 1 PR, uses data
   `/api/analytics` already sends. Label it "touched" (not the Timing
   tab's "held 1 h") and before tax. It's an upper bound in thin pools.
   Times to 3×/5× would need new columns plus a history backfill: not
   proposed.
2. **Data health by week:** calls, share with a Perceptor verdict at the
   call, quiet/dead share by pool family. Page-only, 1 PR. Pick one
   "dead" definition: the page's quiet excludes rugged calls, ML's dead
   includes them. Denominator: calls with a known trade count. v2 counts
   Sync events, so its share is biased low.
3. **Model insights** (the "coming soon" slot): live lift and calibration
   of the scores stored at the call. Data is already in the snapshot
   (`attachWebModel`); proposed as a separate `GET /api/model-live`
   gated on `ModelScores`, so `/api/analytics` stays identical to the
   public copy. Go PR + page PR. Top 10% = `runner_rank_pct ≥ 90`; rates
   gross of tax; show coverage and n. At today's volume (9 live calls on
   13–14 Oct) matured 7 d outcomes are weeks away, so build the Go part
   first and show "n matured" states.
4. **CSV export:** page-only, from the accumulators, not the DOM. Either a
   `blob:` download (CSP likely fine, but the scout-web href rule needs a
   documented exception; check in a browser) or "Copy as TSV" through the
   clipboard (no CSP surface). Guard against formula injection.

## Earlier session state (13 October 2026, evening: main 29b5c2e after PRs #36-#40; prod 29b5c2e; history)

Superseded by 14 October above where they differ.

**Main and prod:** main is at **29b5c2e** (PR #40). Prod: the owner
pulled 29b5c2e on the server (`git log -1` read back by him) and
restarted the listener (`-listen-only`), the tracker (`-track`) and
`-web`; `SCOUT_MODEL_URL` is in the prod `.env` (count 1). The first
scored delivery was still awaited at the time of writing (no new call
yet); the listener may need one more restart if the `.env` line was added
after its restart. Merged 13 Oct after 56ad247 (PR, merge commit, head):

- PR #36 (9dbed27; head 4437690): this file and the PM skill for the
  first half of 13 Oct.
- PR #37 (e5db130; head a08d7d4, `work/2026-10-13-testdb-guard`): DB tests
  read `SCOUT_TEST_DATABASE_URL` only through `testDatabaseURL`
  (`testdb_test.go`) and fail before connecting unless it is database
  `scout_test` on localhost/127.0.0.1/::1/unix socket on a TCP port other
  than 5432 (no port = 5432 = refused). Test code and docs only. The paused
  CI draft must map a non-5432 port when it resumes.
- PR #38 (495f881; head 4f08fb0, `work/2026-10-13-reply-to-filter`): a bot
  reply to a message older than our request (or sent before it) is
  ignored (logged, `details.ignored_replies`, `details.request_msg_id`) in
  every lane (live, 12-minute re-check, re-scan, re-ask). Messages without
  a reply link still count (sAlpha leaves it off on ~25% of replies;
  Perceptor set it on all 448 in the owner's 14-day sample). Read-only
  follow-up SQL in the PR description; if Perceptor always replies to our
  request, requiring the link for Perceptor can be proposed.
- PR #39 (4c81819; head dee1aae, `work/2026-10-13-go-small-fixes`):
  re-check `paceWait` only lowers `nextLook`; `isNoSuchValue` narrowed
  (node no-state texts first, incl. the prod Nitro text `missing trie node
  … state 0x… is not available, not found`, code -32000, read by the owner
  13 Oct; then busy words; then code 3 / reverts; other errors retried);
  "0 red flags" read as clean and "10 red flags" as red flags again
  (`stripSafePhrase`); `TestScoreCallBudget` 2 s / 5 s. Owner's count of
  stored Perceptor rows hit by the zero case: 0 (no data fix needed).
- PR #40 (29b5c2e; head ab62022, `work/2026-10-13-perceptor-filter-latest`):
  owner request: the call list's Perceptor filter and counts use
  `perceptor_latest_verdict` (call-time report or the latest re-scan /
  2-hour re-check; a real verdict beats insufficient/unknown, else the
  later completion wins, tie → re-scan). Display only; the dataset view,
  export, scoring and Analytics "verdict at the call" stay call-time.

**Live on prod since 13 Oct (owner):**
- sAlpha re-ask **on** (`SCOUT_SALPHA_REASK=on`; the dry run showed 406
  first calls without a report: 169 declined, 196 empty, 37 timeouts; 6
  candidates in the 6–24 h window; 4 `reask` rows by the evening).
  Default max age 24 h kept; the owner may take the 79 calls under 7 days
  later with `SCOUT_SALPHA_REASK_MAX_AGE=168h` for a day (open).
- Scoring service **installed and running** (`scout-model.service`, user
  `lyle`, `SCOUT_MODEL_LINE=all`, `OMP_NUM_THREADS=2`, serving
  20261013-1909; `/health` and a sample `/score` checked by the owner).
  The listener has `SCOUT_MODEL_URL=http://127.0.0.1:8601`. After a
  retrain no restart is needed (LATEST is read per request); restart it
  only when `ml/` code changes.

**Latest model report (20261013-1909, first under the PR #34 gates):**
short FAIL (pooled lift 1.02), 3day FAIL (median 1.98 of 1.98/2.06/1.98;
every other gate passes, sim difference +32.2 [+18.2, +44.1]), **medium
PASS** (median 2.26 of 2.34/2.26/1.61, lower end 2.04, sim difference
+41.3 [+24.5, +57.4], collapses removed 0.483), long skipped. Medium
passed on 0702 too. Caveats: the top 10% still averages −10.4 % at 7 d
(vs −51.7 % for all calls): ranking, not profit; only 4 counted weeks;
seed 23 at 1.61; adversarial AUC 0.99 driven by `calls_prev_24h`.
Diagnostics 22 s. Report copied by the owner (to be saved under
`ml-results/models/20261013-1909/`).

**`calls_prev_*` finding (investigate, 13 Oct):** `calls_prev_24h`/`_1h`
count every stored row of the channel in the window strictly before the
post (real calls and "hit 3X" update posts, >50 %), so no leakage, but
they trend with calendar time (a timestamp proxy). Training/serving
mismatch possible: rows added later by backfill raise the training count.
`perceptor_verdict` is filled only for calls handled live (8.8 %).
Read-only SQL Q1/Q2 for the owner (per-day storage, per-week calls_prev
split) were sent; report-only variants are being built (below).

**In progress (PM worktrees from `origin/main` 29b5c2e, uncommitted):**

| Worktree | Branch | Work |
|---|---|---|
| web-model-scores-20261013 | `work/2026-10-13-web-model-scores` | model scores on the website (owner request; defaults: fixed 7d column, detail in the row JSON, "n.v." for failed gates, only scores within 24 h of the call); `passed` column on `scout_call_predictions` (repeat-safe); NOTIFY/SSE `score`; `model_scores` switch off for the GCP site; leakage guards |
| ml-callsprev-variants-20261013 | `work/2026-10-13-ml-callsprev-variants` | report-only variants: `calls_prev` burst ratio; calls_prev variants across seeds |
| tracker-nostate-paths-20261013 | `work/2026-10-13-tracker-nostate-paths` | ETH/USD pool `slot0` and Pons graduation paths flip `noState` only on a real no-state text |
| timing-tab-polish-20261013 | `work/2026-10-13-timing-tab-polish` | Kaplan–Meier caption wording, unused `ech-tip-box` class |
| handoff-1013b-20261013 | `work/2026-10-13-handoff-1013b` | this update |

## Earlier session state (13 October 2026, first half: main 56ad247 after PRs #31-#35; history)

This section supersedes the 12 October one below wherever they differ.

**Main and prod:** main is at **56ad247** (PR #35). The owner reported
13 Oct that he pulled main on the server and restarted the listener
(`-listen-only`), `-web` and the tracker (`-track`), and is retraining.
The prod commit hash was not read back, so 56ad247 there is the owner's
report, not a checked `git log`. His PC's main checkout was still at
de4b642 at that point (block A `git pull` pending), so the PM's 13 Oct
worktrees branch from `origin/main`. Merged 13 Oct (PR, merge commit,
branch head):

- PR #31 (ff83467; head b30e1f7): this file for the 12 Oct session.
- PR #32 (de4b642; heads 8623d8a, 8676bec): the PM commits, pushes and
  opens PRs with `gh`; the owner reviews and merges ("Create a merge
  commit"). Supersedes "agents never commit or push" below.
- PR #33 (815ceb3; head 46274da, `work/2026-10-13-timing-real-held`):
  Timing tab shows the **real time held at 2×** (stored `above_2x_s`) for
  calls that touched 2× but held under 1 h; they used to count as 0 held.
  Held histogram in three colours (grey 0 held, amber touched but under
  1 h, blue reached 2× after 1 h or more). "Reached 2×" (1 h or more held)
  and the 1h window's "n/a — needs 1 hour held" are unchanged. Frontend,
  README and tests only.
- PR #34 (382b1b6; head ccf5ded, `work/2026-10-13-ml-pooled-gates`): ML
  gates **option (b)**. Runner lift = median over seeds 7/11/23 of the
  pooled expanding walk-forward top-10% lift, which must be at least 2,
  plus a bootstrap lower end above 1.0 for the saved seed. The pooled sim
  difference (top 10% minus all) must have a lower end above 0.
  "Collapses removed ≥ 40%" is unchanged. The absolute return is
  information only. Missing inputs fail with a reason. The old gate is
  kept under `gates.old` (information only). `meta.json`
  `gates.passed` (read by `serve.py`) follows the new gates. Seed
  retrains and pooled WF now run in every training
  (`SCOUT_ML_DIAGNOSTICS=0` only drops rolling WF, no-dead and
  adversarial). There is a new report-only variant, "no calls_prev
  inputs" (`calls_prev_1h/24h` drove the train/test drift: adversarial
  AUC 0.99). `GATE_MIN_POOLED_WEEKS = 2` is an implementer default, not
  an owner decision. `ml/` only.
- PR #35 (56ad247; head 46f2bfe, `work/2026-10-12-salpha-reask`): sAlpha
  re-ask moved onto main and finished:
  - Declines are no longer forwarded (header `no report (declined: …)`).
  - Re-ask lane `salpha_reask.go`, off unless `SCOUT_SALPHA_REASK`: 6–24 h
    after the call, at most 60 a day, `scan_kind='reask'`, sent as a reply
    to the original delivery (`notify_message_id`), plus
    `-salpha-reask-missing [-dry-run]`.
  - `/api/call` version 3.
  - `notify_message_id` is also stored for Perceptor re-check deliveries.
  - `FirstSentDelivery` treats `sending` as not sent.
  - Bot API 429 retry on the live path (honours `retry_after`; at most 3
    retries / 60 s; otherwise `failed` and logged).
  - **Bot-token redaction** (`botAPIRedact` on every Bot API error).
  - **Re-check score line persisted** across listener restarts (owner
    option (a): `score_line` in the held Perceptor investigation's
    `details` JSONB; never re-scored).
  - Schema: `scout_deliveries.notify_message_id` plus a partial index for
    `reask` rows (repeat-safe; no new table; `onchainStateVersion`
    unchanged).
  - Leakage: re-ask rows, re-check results and `score_line` are never
    read by the dataset view, the export, scoring or `ml/`.

**Owner decisions taken 13 Oct (each recorded in its PR description):**
Timing shows the real time held (supersedes 12 Oct "0 held"); ML gates
option (b); re-check score line persisted, option (a). `SCOUT_SALPHA_REASK`
stays off until the owner has checked `go run . -salpha-reask-missing
-dry-run`.

**Latest model report (20261013-0702, trained on prod 13 Oct on code with
PR #29, the first report with its diagnostics; 4,759 rows, data to
13 Oct):**
- Old single-run gates: short FAIL (lift 1.09), 3day PASS (lift 2.12
  [1.30, 3.27], WF 6/6), medium PASS (lift 2.05 [1.20, 3.36], WF 4/4).
  Long skipped (2,982 rows over 47 days).
- Seed stability (short): seeds 7/11/23 lift 1.09/1.47/0.99, all FAIL.
- Under the PR #34 gates (re-scored in the PR, not a retrain): 3day FAIL
  (pooled-WF median 1.98 of 1.98/2.02/1.85); medium PASS (median 2.18,
  lower end 1.74, sim difference +43.1 [36.7, 51.6]); short FAIL.
- Diagnostics time on prod: 29 s (seeds 16, walk_forward 4, no_dead 0,
  adversarial 9).
- The retrain the owner started on 56ad247 is the first under the new
  gates: send its `report.md`.

**Owner to-do after the 56ad247 deploy:**

1. PC: block A, `git pull` in the main checkout, then remove the merged
   worktrees `.claude\worktrees\timing-real-held-20261013`,
   `ml-pooled-gates-20261013` and `salpha-reask-20261012`. Then
   `git branch -d` their branches (`work/2026-10-13-timing-real-held`,
   `work/2026-10-13-ml-pooled-gates`, `work/2026-10-12-salpha-reask`).
2. Website: Analytics → Timing at 1280/390 px, light and dark: the
   three-colour held histogram and its key in 1d; amber and grey only in
   1h; the "Held at 2×" medians; key swatches grey for groups under 20
   calls; the sA badge/detail; no CSP errors.
3. Listener: the start line applied the schema (no errors); a sAlpha
   decline now shows `no report (declined: …)` and is not forwarded.
4. `go run . -salpha-reask-missing -dry-run` and send the count before
   deciding to switch `SCOUT_SALPHA_REASK` on.
5. Send the new model's `report.md` (to `ml-results/models/<version>/`
   on the PC) and the `[diagnostics]` time line.
6. Tracker: check the `latest prices on:` line after the restart.
7. Still open from 12 Oct: scoring service not installed yet ("Then
   switch scores on" below), so no scoring-service restart is needed for
   PR #34.

**In progress (13 Oct, PM worktrees from `origin/main` 56ad247,
uncommitted):**

| Worktree | Branch | Work |
|---|---|---|
| go-small-fixes-20261013 | `work/2026-10-13-go-small-fixes` | `paceWait` only lowers `nextLook`; narrow `isNoSuchValue`; "0 red flags" parser; `TestScoreCallBudget` margin |
| testdb-guard-20261013 | `work/2026-10-13-testdb-guard` | DB tests refuse any `SCOUT_TEST_DATABASE_URL` that is not `scout_test` on localhost; the `localhost:5432` example comment |
| reply-to-filter-20261013 | `work/2026-10-13-reply-to-filter` | only accept bot replies that reply to our own request (all lanes) |
| handoff-resume-20261013 | `work/2026-10-13-handoff-resume` | this file and the PM skill's decision list |

Also: an `investigate` check of whether the old worktree
`agent-ae167c3d46439a04d` (18 uncommitted entries, ported as PR #35) holds
anything not on main. Keep it until that check says it is safe.

## Previous session state (12 October 2026, end of session: main 228b3e0 after PRs #26-#30; history, superseded by 13 October above where they differ)

**Main and prod:** main is at **228b3e0** on GitHub (PR #30, merged
12 Oct 23:20 PDT). Prod ran **68a4954** at the last confirmation (deployed
by the owner 12 Oct); a deploy of 228b3e0 since then is not confirmed.
The 228b3e0 deploy is the owner's next step ("Owner to-do: deploy 228b3e0"
below). Merged 12 Oct after 41a27b0 (PR, merge commit, branch head):

- PR #26 (57fdced; head 67236c6, `work/2026-10-12-agent-setup`): agent
  setup redone by function (5 agents + `scout-*` skills; see "Agent
  setup"). Claude Code was restarted on it 12 Oct.
- PR #27 (de62901; head 8368747, `work/2026-10-12-timing-touch-drop`):
  Timing tab "reached 2×" rule: a call reached 2× only if it was at or
  above 2× for at least 1 h in total, in every window; windows too short
  for that (1h) show "n/a — needs 1 hour held" (`can2xReach`);
  touch-and-drop calls count as 0 held. Frontend, README and tests only;
  no API or schema change, no re-track. Not deployed yet.
- PR #28 (1328ff1; head 61ed270, `work/2026-10-12-recheck-score-line`):
  Perceptor re-check deliveries carry the first-pass score line, kept in
  memory by the re-check lane (never recomputed from re-check data; lost
  after a full listener restart; persisting it is an open owner decision,
  below). `splitMessage` makes `botAPISend` UTF-8 safe (no cut runes).
  Tested locally: full DB suite 857 pass / 0 skip. Not deployed yet.
- PR #29 (27404b6; head 12bfa9d, `work/2026-10-12-ml-stability`): ML
  report-only stability diagnostics: seeds (7, 11, 23), pooled
  walk-forward with a week-block bootstrap CI plus a rolling 4-week
  walk-forward, `EVAL_EXCLUDE_DEAD` evaluation, adversarial validation, a
  "stronger regularisation" variant, late-bound config reads.
  `SCOUT_ML_DIAGNOSTICS=0` turns them off for one run. The saved model,
  gates and scores are unchanged. Tested locally: pytest 207 passed. Prod
  runtime not measured yet (the `[diagnostics] …` line of the next
  retrain gives it).
- PR #30 (228b3e0; head c0b1332, `work/2026-10-12-pm-git-commands`): the
  PM gets Bash for read-only git plus `git worktree add`, and Edit/Write
  for `HANDOFF.md` and `.claude/skills/**` only; `.claude/settings.json`
  allows those git commands and denies `git commit`/`add`/`push`/`stash`;
  the PM skill has the exact commit hand-off template (absolute `cd`,
  checks, PR link always) and the merge-only flow for a second branch on
  the same files (the owner runs the merge). Takes effect after a Claude
  Code restart.

**Owner to-do: deploy 228b3e0** (no tracker restart, no schema change,
`onchainStateVersion` unchanged), on the server:

1. `cd "$REPO" && git checkout main && git pull`;
   `git log -1 --oneline` shows `228b3e0 Merge pull request #30 …`.
2. Restart `-web` (`go run . -web`) for #27 and the listener
   (`go run . -listen-only`) for #28. The tracker keeps running.
3. Checks: Analytics → Timing: the 1h window shows "n/a — needs 1 hour
   held", 1d shows numbers; the legend at 1280/390 px, light and dark; no
   CSP errors in DevTools.
4. Then retrain: `REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh
   --skip-install` (no script refresh needed) and send the
   `[diagnostics] seeds N s, walk_forward N s, no_dead N s, adversarial N s;
   total N s` line plus `report.md` (to `ml-results/models/<version>/` on
   the PC).

The 12 Oct deploy checks still open from 68a4954 and the "Then switch
scores on" steps below stay as they are.

Merged earlier, after dc0a42d up to 41a27b0 (session item → PR, merge
commit, branch head):

- PR #15 (f072467, 10 Oct): ML walk-forward minimum, weeks with fewer than
  1000 training rows are not counted (`WF_MIN_TRAIN_ROWS`); `ml/` only.
- PR #16 (ad00645): agent setup.
- PR #17 (f611cda): handoff pause.
- #5 GCP site draft → PR #18 (4245939; head 59d0a31): `deploy/gcp/**`,
  DEPLOY.md 7.5, `.gitattributes`, `.gitignore`. Draft only (see "GCP
  site" below).
- #1 ML score gating → PR #19 (7228325; head a5cb7a4): `ml/` only.
- #3 Perceptor re-check → PR #20 (5f9350e; head e4d5b2c): `insufficient`
  level + 12-minute re-check (`recheck.go`), schema relabel.
- #2 score live fixes → PR #21 (aab3e3c; head be0cddd, plus ea454a8 "Merge
  branch 'main'"): `quote_asset` from the live pre-call, one
  `SCOUT_MODEL_TIMEOUT` budget (default 10s).
- #4 Analytics Timing tab → PR #22 (68a4954; head b4149ff):
  `/api/analytics` format 4, ECharts 6.1.0 custom build vendored (sha256
  `11c0d37dd643ce90816bb4825e557658b3a37a887c494cb49e8c79aed8678ba8`,
  provenance in `frontend/vendor/README.md`).
- #7 delivery model line → PR #23 (c31d859; head 65baa2b): scoring-service
  env var `SCOUT_MODEL_LINE=passed|all` (default `passed`; with `all`,
  failing buckets are marked "(not validated)"); `ml/` only.
- #6 scoring-service unit → PR #24 (77dce3d; head 944ecfd):
  `ops/scout-model.service`, `ops/scout-model.md`, `DEPLOY.md` (section
  2.4), `ops/README.md`, `.gitattributes`. Not installed on prod yet: next
  the owner installs it per `ops/scout-model.md`, then sets
  `SCOUT_MODEL_URL` in `.env` and restarts the listener ("Then switch
  scores on" below).
- Docs for the 12 Oct decision → PR #25 (41a27b0; head 8ed2714,
  `work/2026-10-12-handoff`): the seven docs `README.md` ("Scoring new
  calls"), `ml/README.md`, `ml/RUNBOOK.md` (step 9 "Scores on deliveries"),
  `ml/run_training.sh` (echoed notes only), `.claude/agents/ml-coder.md`
  (its rule now lives in the `scout-ml` skill),
  `.claude/skills/scout-product-manager/SKILL.md` and this file now state the
  `SCOUT_MODEL_LINE` rule. Done.

**Prod runs main 68a4954 (deployed by the owner 12 Oct; last confirmed
state, see the 228b3e0 to-do above):** listener
`-listen-only`, tracker `-track` and `-web` restarted by the owner on
68a4954, so #2, #3 and #4 are live. The listener start applied
`scoutanalytics.sql`, including the relabel of the 54 Perceptor rows from
`unknown` to `insufficient` ("not enough data yet"); the dataset view now
excludes re-check reports. Earlier (dc0a42d, 9 Oct 22:34 PDT) it ran the
timing backend. Verified on prod 10 Oct: clock synchronised (NTP);
timing backfill complete (every first call timed in every horizon; the ~149
untimed rows per horizon are all `repeat` calls; all 245 rugged calls have
rug times; 0 negative values). First numbers (timed / reached 2× / median s
to 2× / still ≥ 2× at end): 1h 5247/1365/434/229; 1d 5196/1863/850/81; 3d
5058/1924/1002/66; 7d 4884/1905/1051/50; 30d 2982/1247/1021/28. C: has
~318 GB free (12 Oct); Go builds and throwaway Postgres work again.

**Owner rules:** agents never commit or push (`.claude/settings.json`
denies `git commit`, `git add`, `git push` and `git stash`, PR #30): the
PM gives exact commit/push commands and the owner runs them; never commit to
main. The PM's Bash is for read-only git and `git worktree add` only; it
edits `HANDOFF.md` and `.claude/skills/**` itself (in a worktree), every
other file goes to `implement`. `review` runs on `fable`, `investigate` on `opus` (the owner asked
for `fable` for one ML-planning research task). Up to 10 writing agents.
The testing setup is PAUSED: the `verify` agent, the testdb scripts and CI
(re-confirmed by the owner 12 Oct); `implement` runs DB tests
itself with the manual throwaway-Postgres steps in `TESTING.md`.
Paused work (both worktrees at dc0a42d, untracked files; keep them):
`agent-afcf62469a29564cf` (`scripts/testdb.sh`, `scripts/testdb.ps1`;
review-fix state unknown, a real PowerShell 5.1 run is still a must-fix;
commit with `git add --chmod=+x scripts/testdb.sh` on
`work/2026-10-09-testdb-scripts`) and `agent-aa31691b57e3bc7ff` (CI:
`.github/workflows/test.yml` + README, `.github/dependabot.yml`, modified
`.gitattributes`; re-review the token handling before committing on
`work/2026-10-09-ci-tests`).

**12 Oct deploy: DONE** (owner pulled 68a4954 and restarted the listener,
tracker and `-web`; `onchainStateVersion` unchanged, no history re-run; the
54-row `insufficient` relabel happened on the listener start). **Owner
checks still open from it:**

1. Analytics → Timing on real data, and record the real gzipped
   `/api/analytics` size in README (the "Size:" bullet under
   `GET /api/analytics`; estimate 250-320 KB), e.g.
   `curl -s -H 'Accept-Encoding: gzip' -o /dev/null -w '%{size_download}\n' http://<SCOUT_WEB_ADDR>/api/analytics`.
2. The 2-minute browser look planned for #3: the new "not enough data yet"
   label at 1280/390 px, light/dark, no CSP errors in DevTools.
3. Watch the listener: a live call whose Perceptor report says "not enough
   data yet" is held and re-checked ~12 min after the call: `clean`/`caution`
   → delivered then (`🔁 Perceptor re-checked …` header line); `red_flags`
   → not delivered; still no verdict → delivered then with the
   `⏳ … not enough data yet` label (README "Re-checking “not enough data
   yet”").
4. After any tracker restart, check the `latest prices on:` line (every
   1h0m0s for older ones, at most 400 per cycle; section 3 "Lesson" on
   `.env`).

**Then switch scores on** (owner decision 12 Oct: the delivery score line
shows the model numbers even when buckets fail, clearly marked; see "Owner
decision 12 Oct" below; scores never filter deliveries):

1. Done: retrain on prod after the deploy (model 20261012-1917; all buckets
   FAIL, see "Latest model report" below).
2. Done: the line-all change is merged (PR #23, c31d859): scoring-service
   env var `SCOUT_MODEL_LINE=passed|all` (default `passed`); with `all`,
   failing buckets are marked "(not validated)".
3. Done: the scoring-service unit #6 is merged (PR #24, 77dce3d; head
   944ecfd, `work/2026-10-12-scoring-service`).
4. The checkout the unit runs `serve.py` from must be at c31d859 or later
   (68a4954's `serve.py` ignores `SCOUT_MODEL_LINE`).
5. Install `ops/scout-model.service` per `ops/scout-model.md`, with
   `Environment=SCOUT_MODEL_LINE=all` in the unit.
6. `SCOUT_MODEL_URL=http://127.0.0.1:8601` in the prod `.env`
   (`SCOUT_MODEL_TIMEOUT` default is now 10s).
7. Restart the listener; watch the logs for `precall Xs` times and "no score
   within SCOUT_MODEL_TIMEOUT".
8. Later, after the tracker has caught up, run the README's read-only
   `quote_asset` agreement SQL (it counts bucket rows; use
   `count(DISTINCT p.call_id)` for calls).

**Owner decision 12 Oct (model line on deliveries):** show the model numbers
on deliveries even when buckets fail, clearly marked ("(not validated)"),
because the owner is the only reader of the private delivery group. Prod
plan: `SCOUT_MODEL_LINE=all` in the scout-model unit. Scores still never
filter deliveries. This supersedes the earlier rule "scores only appended
once gates pass" for this owner-only group. The code is merged (PR #23);
the docs (README, `ml/README.md`, `ml/RUNBOOK.md`, `ml/run_training.sh`,
`ml-coder.md` (now the `scout-ml` skill), the PM skill, this file) were
updated in PR #25.

**Not merged (agent worktrees under `.claude/worktrees/`):**

| # | Worktree | Branch to create/use | State | Files | Notes |
|---|---|---|---|---|---|
| — | agent-ae167c3d46439a04d | `worktree-agent-ae167c3d46439a04d` (work branch not named yet) | sAlpha re-ask; uncommitted (18 entries in `git status --short`), at d5d96ac (45 commits behind main 228b3e0) | — | See "Still to do". |
| — | agent-afcf62469a29564cf, agent-aa31691b57e3bc7ff | `work/2026-10-09-testdb-scripts`, `work/2026-10-09-ci-tests` (to create) | testdb scripts, CI; paused, uncommitted (at dc0a42d, 33 commits behind 228b3e0) | — | See "Owner rules" above. |
| — | handoff-session-20261012 (at 228b3e0) | `work/2026-10-12-handoff-session` | this file's 12 Oct end-of-session update; uncommitted, for the owner to commit | `HANDOFF.md` | Docs only. |

**GCP site (#5): merged as PR #18 (4245939)**, a draft only (`deploy/gcp/`,
`deploy/gcp/INTERFACE.md`); no Go export and no GCP resources yet, so
nothing is public until the Go export exists and the owner creates the GCP
resources. As merged, the draft PUBLISHES sAlpha report text (scrubbed) and
Perceptor report links (`perceptor.info` allowlist); this file's earlier
default (section 4 item 8) was verdict labels only. Owner to confirm (b)
publish, or ask for (a) labels only (text/links behind an opt-in in
`deploy/gcp/INTERFACE.md`) before the Go export is written.

**Latest model report (20261012-2209, trained on prod 12 Oct, before the
#29 diagnostics were merged; 4,759 rows, data to 12 Oct): ALL buckets
FAIL.** short (1d): lift 0.95 [0.64, 1.33] (runner AUC 0.47, no signal),
collapses removed 0.52, WF 3/6 counted, top-10% −6.3% vs all −18.3%. 3day:
lift 1.66 [0.95, 2.76], collapses removed 0.52, WF 6/6, top-10% −3.7% vs
all −28.6%. medium (7d): lift 1.998 [1.19, 3.22] (gate 2), collapses
removed 0.48, WF 4/4 counted, top-10% −4.7% vs all −41.3%. long skipped
(2,946 rows over 46 days). Variant "raw dex + dex_family" PASSES 3day on
the test part only (lift 2.27; WF mean lift 2.08 < baseline 2.12), so it is
not adopted (test-period overfitting). Medium went 2.60 PASS (10 Oct) →
1.88 (12 Oct, 1917) → 1.998 (12 Oct, 2209): noise around the gate. The
report-only diagnostics merged in PR #29 (pooled walk-forward with
bootstrap CI, seeds, no-dead evaluation, adversarial validation) are meant
to settle the gate decision; the next retrain on 228b3e0 is the first with
them.

**Previous model report (20261012-1917, trained on prod 12 Oct after the
deploy; 4,755 rows, data to 12 Oct; history): ALL buckets FAIL.** short
(1d): lift 1.04, collapses removed 0.50, WF 2/6, top-10% −20.5% vs all
−18.3%. 3day: lift 1.51, collapses removed 0.52, WF 6/6, top-10% +2.5% vs
all −28.6%. medium (7d): lift 1.88 [1.09, 3.09] (was 2.60 PASS two days
earlier), collapses removed 0.49, WF 4/4 counted, top-10% +1.6% vs all
−42.2%. long skipped (2,931 rows over 46 days). Variant "raw dex +
dex_family" passed 3day on the test part only (WF mean lift 1.92 < baseline
2.02), not adopted.

**Older model report (20261010-0621, trained on prod 10 Oct, 4,727 usable
first calls; history):** short (1d) FAIL (runner AUC 0.47 = no signal; lift 1.00;
collapses removed 0.49; WF 2/6); 3day FAIL (lift 1.73; collapses removed
0.52; WF 5/5); medium (7d) PASS (lift 2.60 [1.67, 3.91]; collapses removed
0.46; WF 4/4 counted, 3 not counted); long skipped (needs 120 days). Money
sim medium: top 10% −8.8% vs all −43.5% (loses less, doesn't make money).
Two of four medium variants fail (raw dex; runners must be tradeable). The
dead-score proposal would pass for LightGBM but is driven by one uniswap_v4
wave (136/148), so it stays report-only.

**ML research (fable, 10 Oct): next ML plan, report-only variants; items
(1)-(3) are merged as report-only diagnostics (PR #29, 27404b6); (4)-(8)
are not started:**
(1) seed stability (3 seeds) +
stronger regularisation;
(2) regime diagnostics: adversarial validation, pooled walk-forward with
week-block bootstrap CI, rolling-window WF; (3) re-evaluate medium without
dead calls (knob `EVAL_EXCLUDE_DEAD`; research expects lift ~2.0); (4)
time-decay weights; (5) rank-blend with logistic; (6) combined
runner×(1−collapse) / expected-return rule; (7) monotone constraints; (8)
CatBoost last. Each needs a new `VARIANT_KNOBS` entry + late-bound config
reads (model.py/validate.py bind some config at import). Evaluation issues
found: the sim gate is a strict `>` on as few as 5 calls; "every counted
week" is noisy; the lift CI treats the base rate as fixed; WF refits reuse
round counts chosen with later data (mild look-ahead); collapse
miscalibration is base-rate drift (show rank tiers, or refit the Platt
intercept weekly). Proposed gate changes for the OWNER to decide: pooled WF
lift with bootstrap CI; ≥ 75% of counted weeks + pooled sim beats buy-all;
absolute check (pooled top-10% mean net return > 0) before showing scores
(today's 7d would fail that). PM leaned (a) keep current gates + run the
stability checks. The 20261012-1917 and -2209 reports (medium 2.60 PASS →
1.88 → 1.998 FAIL) make the (b)-style pooled WF / bootstrap-CI gates more
relevant. The stability diagnostics (seeds 7/11/23, pooled WF + week-block
bootstrap CI, rolling 4-week WF, `EVAL_EXCLUDE_DEAD` evaluation,
adversarial validation, stronger regularisation, late-bound config reads)
are merged as report-only (PR #29); the gate decision waits for the first
report that has them (next retrain on 228b3e0).

**Owner decisions still open:** ML gates (a) keep current + stability
checks / (b) stricter pooled-WF/bootstrap-CI gates (see the 12 Oct note
above; to be settled with the first report that has the PR #29
diagnostics); persist the Perceptor re-check score line across listener
restarts (PR #28 keeps it in memory only, so a full listener restart loses
it for calls held at that moment): (a) a `score_line` key in the held
Perceptor investigation's `details` JSONB, written in `MarkHeldForRecheck`,
or (b) a column; shipping without it for now; pull `botAPIRedact` forward
from the sAlpha re-ask branch as its own small fix (pre-existing issue: a
network-level Bot API send error, a `*url.Error` from
`http.DefaultClient.Do`, contains the full URL including the bot token and
can reach the listener logs); the Timing tab's "0 held" reading for calls
that touched 2× but held under 1 h (kept as built in PR #27 unless the
owner objects); GCP public site content: (b) publish scrubbed sAlpha text +
Perceptor links as merged, or (a) verdict labels only with an opt-in (PM
recommends (a): both are paid bots); Perceptor re-scan 250/day + 1440 h max
age (current 100/day, 30 days); public-site stopgap; price providers; DNS
manager for lylelabs.io; the public-repo items (a)-(c) in section 3
"Decisions 12 October" (commit-email metadata, PR text skim, prod database
name in the docs). Optional: fix PC PATH `<postgres dir>\bin` ->
`<postgres dir>\17\bin`.

**Owner note (PC):** PowerShell on the owner's PC may not have git on PATH;
`$env:Path += ";C:\Program Files\Git\cmd"` fixes it for the session.

**Stash (owner):** the Perceptor re-check is committed and merged (PR #20),
so its backup stash can go: `git stash list`, confirm `stash@{0}` is still
"On worktree-agent-a8c7da1f9ff6a0cce: recheck" (it was on 12 Oct), then
`git stash drop stash@{0}`. Never pop it, and never touch the GitHub
Desktop stash ("On work/2026-10-09-ml-variants: !!GitHub_Desktop<…>",
`stash@{1}` today, `stash@{0}` after the drop).

**Worktree and branch clean-up (owner, when convenient; from the main
checkout):** checked 12 Oct end of session (main 228b3e0):
`git branch --no-merged main` printed nothing, so every local branch is
merged; `git branch -r --no-merged main` printed nothing either. The
worktrees to remove below had an empty `git status --short`.

0. Delete the stray folder `D:\c` (created by an agent's path mix-up; it
   holds only empty nested folders `Users\<user>\AppData\Local\Temp\claude`,
   0 files on 12 Oct).
1. `git fetch --prune`
2. Remove the merged worktrees (check `git -C <wt> status --short` is empty
   first; a worktree shown as `locked` by `git worktree list` needs
   `git worktree unlock <wt>` first). A branch checked out in a worktree
   can't be deleted until its worktree is removed:
   `git worktree remove .claude\worktrees\agent-a2c801320bb03ae6b` (#1
   ml-score-gating), `...\agent-a0bdbe5e67a6d0034` (#2 score-live-fixes),
   `...\agent-a8c7da1f9ff6a0cce` (#3 perceptor-recheck),
   `...\agent-a8cd1f54c100d5817` (#4 timing-tab),
   `...\agent-ae1d8c7d8de93a35b` (#5 gcp-site),
   `...\agent-adc7b7e6c79d4bb23` (timing-backend),
   `...\agent-af53dbd18b713cb60` (ml-wf-min),
   `...\agent-a91f7f4e909abfd02` (#7 ml-line-all, PR #23),
   `...\agent-ab0f0dba0692e4540` (#6 scoring-service, PR #24; locked
   "claude agent agent-ab0f0dba0692e4540 (pid 131248)", so unlock it
   first), `...\agent-af197a4c6cbe0f7db` (12 Oct handoff, PR #25) and
   `...\agent-setup-20261012` (`work/2026-10-12-agent-setup`, PR #26).
   The worktrees of PRs #27-#30 no longer exist on disk.
3. `git branch -d` every merged local branch: `work/2026-10-08-ml-pool-web`,
   all `work/2026-10-09-*` (agent-setup, analytics-charts, analytics-page,
   flaky-pacing-test, gcp-site, handoff-queue, mcap-supply,
   ml-dead-dex-calib, ml-dead-score, ml-variants, ml-wf-min,
   perceptor-recheck, perceptor-rescan, perceptor-today-label,
   salpha-capture, timing-backend), all `work/2026-10-10-*`
   (handoff-pause, ml-score-gating, score-live-fixes, timing-tab),
   `work/2026-10-12-ml-line-all` (PR #23), `work/2026-10-12-scoring-service`
   (PR #24), `work/2026-10-12-handoff` (PR #25),
   `work/2026-10-12-agent-setup` (PR #26) and
   `work/2026-10-12-pm-git-commands` (PR #30; no worktree). The branches
   of PRs #27-#29 exist only on origin. Also the leftover merged
   `worktree-agent-a0bdbe5e67a6d0034`, `worktree-agent-a8cd1f54c100d5817`,
   `worktree-agent-a91f7f4e909abfd02`, `worktree-agent-ab0f0dba0692e4540`
   and `worktree-agent-af197a4c6cbe0f7db` (not checked out anywhere).
   Note: GitHub Desktop shows its
   stash only on `work/2026-10-09-ml-variants`; deleting that branch keeps
   the stash in `git stash list` but hides it in Desktop, so the owner may
   keep that one branch until he has decided about the stash.
4. `git push origin --delete` the merged `work/*` branches on origin (28 on
   12 Oct, all merged): the `work/*` branches of step 3 except
   `work/2026-10-08-ml-pool-web` (no remote branch), plus
   `work/2026-10-12-timing-touch-drop` (PR #27),
   `work/2026-10-12-recheck-score-line` (PR #28) and
   `work/2026-10-12-ml-stability` (PR #29).

Keep: `agent-ae167c3d46439a04d`
(sAlpha re-ask; 18 uncommitted entries), `agent-afcf62469a29564cf` and
`agent-aa31691b57e3bc7ff` (paused testing; uncommitted),
`handoff-session-20261012` (`work/2026-10-12-handoff-session`, this update,
until it is merged), the `worktree-agent-*` branches checked out in the
first three (`worktree-agent-ae167c3d46439a04d`,
`worktree-agent-afcf62469a29564cf`, `worktree-agent-aa31691b57e3bc7ff`;
merged at their base commits, but their worktrees hold uncommitted work),
and the GitHub Desktop stash. No agent test Postgres servers were
running on 12 Oct (only the owner's own Windows service). Old agent
scratchpads take ~5.5 GB under `...\AppData\Local\Temp\claude\...`; the
owner may delete old session folders there (not the current session's).
Never touch the owner's own Postgres server.

**Still to do:**

- Owner: deploy 228b3e0 and retrain ("Owner to-do: deploy 228b3e0"
  above).
- sAlpha re-ask (worktree `agent-ae167c3d46439a04d`, at d5d96ac, 45
  commits behind main 228b3e0): update onto main (which now has the
  Perceptor re-check, its `sending` delivery status, the re-check score
  line and `splitMessage`), then
  `notify_message_id` for re-check deliveries, treat `sending` as not sent
  in `FirstSentDelivery`, live-path Bot API 429 pause, review.
- Bot token redaction (owner to decide whether to pull it forward):
  `botAPIRedact` exists only on the sAlpha re-ask branch (`main.go` in
  worktree `agent-ae167c3d46439a04d`); on `main` a network-level Bot API
  send error (`*url.Error` from `http.DefaultClient.Do`) contains the full
  URL including the bot token and can reach the listener logs
  (pre-existing). Proposed as its own small fix (`implement` with
  `scout-go`).
- Persist the re-check score line across listener restarts, if the owner
  chooses it (open decision above: `details` JSONB `score_line` in
  `MarkHeldForRecheck`, or a column).
- Persist the ML `passed` flag in `scout_call_predictions` (schema change;
  unblocked now that #1-#3 are on main). Done: PR #45, 14 Oct.
- ML timing outcome columns in the dataset view (the `timing_`/rug leakage
  guard is on main since PR #19).
- GCP Go export per `deploy/gcp/INTERFACE.md` (after the publish decision).
- Scoring-service unit #6: merged (PR #24, 77dce3d). Next the owner
  installs it per `ops/scout-model.md`, then sets `SCOUT_MODEL_URL` in
  `.env` and restarts the listener (the "switch scores on" steps above).
- ML: the gate decision after the first report with the PR #29
  diagnostics; the remaining report-only variants (4)-(8) of the ML
  research plan above are not started.
- Small fixes: `recheck.go` `paceWait` should only lower `nextLook` (now on
  main); Timing tab KM caption wording and the unused `ech-tip-box` class;
  narrow `isNoSuchValue`, "0 red flags" parser, `reply_to_msg_id`
  filtering, wait-group hardening on reconnect, `scout_models_db_test.go:18`
  5432 example; test-DB safety guard (DB tests refuse any URL that is not
  `scout_test` on localhost) after the sAlpha branch merges.
- When testing resumes: verify whether aa60a3b really fixed
  `TestLatestPriceInterruptedLeavesRowUntouched`; the CI rerun guard must
  require every top-level `=== RUN` to have a `--- PASS/FAIL/SKIP` line; CI
  token handling re-review; the scripts' PowerShell run. The owner's
  settings allow-list could add initdb/pg_ctl/createdb, `go tool cover`,
  `node testdata/...`, `scripts/testdb.*` so `verify` avoids prompts
  (owner's file; agents never edit it).

## Introduction

Up to date as of 13 October 2026, evening (main and prod at 29b5c2e;
scoring service live, sAlpha re-ask on). Start Claude
Code in the repo root; `.claude/settings.json` makes the session the product
manager, which delegates to the agents in `.claude/agents/` (see "Agent setup").

First message to give it:

> Read HANDOFF.md and README.md, then check
> "Prod state and pending owner actions" with me before starting "Next work".

**Moved from `kfukue/geth-analytics` (history kept).** This repo,
`kfukue/scout-analytics`, is `telegrambot/scoutanalytics` of
`kfukue/geth-analytics` extracted with `git filter-repo --subdirectory-filter`
from that repo's `main` at 4369a1d (PR #20), plus one standalone commit (own
`go.mod`, `internal/database`, paths). Every commit hash in this file before
section 5 is an **old-repo** hash; the rewritten hashes for the ones that
matter are in section 5. Package `main` is at the repo root: `go run .`.

## Deferred / open

20 Oct 2026: the open owner questions on the holder export and the
rug-time decision (A or B) are in the top section.

Kept current by the PM whenever an item is deferred or an owner check is
pending (14 October 2026 UTC). Everything through PR #53 is on `main`
(e522918). The listener was restarted at 23:15 local on 13 Oct, after the
merges; its code level is not confirmed, and neither is `-web` having
#50. Scoring works on prod: the owner's query shows calls 10804 and 10812
scored. By timing, the 4 earlier unscored calls predate the service.
Resolved 14 Oct UTC: web model scores (#45, #50), `calls_prev` report-only
variants (#46, #51), persisted `passed` flag (#45), PR base rule (#52), DB
rollback rule (#53). Resolved 13 Oct: tracker `noState` paths (#42),
Timing captions (#44), sAlpha re-ask, bot-token redaction, re-check score-line
persistence (PR #35); ML gates option (b) (PR #34); Timing real time held
(PR #33); test-DB guard (PR #37); `reply_to_msg_id` filtering (PR #38);
`paceWait`, `isNoSuchValue`, "0 red flags" parser, score budget test
(PR #39); Perceptor filter on the latest report (PR #40); scoring service
installed (owner).

- **Not on `main`** (each item exists as uncommitted changes, or a local
  commit not yet pushed, in an agent worktree under `.claude/worktrees/` —
  see the Session state at the top):
  - Throwaway-Postgres scripts `scripts/testdb.ps1` / `scripts/testdb.sh`
    (agent-afcf62469a29564cf) (paused, see the earlier session states).
  - CI draft `.github/workflows/test.yml` (agent-aa31691b57e3bc7ff; owner
    approval) (paused, see the earlier session states).
  - In progress (table at the top): the model-scoring start-up line,
    invite-link redaction in the logs, the exit-rule table, the
    `/api/model-live` feed and the `CLAUDE.md` safety net.
  - The old worktree `agent-ae167c3d46439a04d` was checked (nothing
    missing from main) and removed by the owner 13 Oct.
- **Owner setting:** `SCOUT_SALPHA_REASK=on` since 13 Oct; the 7-day
  backlog (`SCOUT_SALPHA_REASK_MAX_AGE=168h` for a day) is the owner's
  option.
- **Small fixes, not started:**
  - wait-group hardening: the listener goroutines on a Telegram reconnect;
  - marker words match inside other words ("non-mintable" → red flags,
    "precaution" → caution): word boundaries for `defaultMarkers`;
  - the listener could refuse to score calls older than 24 h (manual
    `-post`/`-ca` can back-score; the website hides such scores);
  - `scout_models.data.go`: `pgxpool.ParseConfig`'s error can contain the
    raw DB URL (password) for an unparseable `SCOUT_DATABASE_URL`
    (startup log only; pre-existing);
  - `chainlink()` still flips `noState` after a revert;
    `ponsCurveFor`/`ponsCurveOfToken` read a busy error at `latest` as
    "not a Pons token"; stored node errors may include the node URL;
  - Timing chart title still says "half had fallen back by";
  - Perceptor: if the follow-up reply query shows it always replies to our
    request, propose requiring the reply link for Perceptor (PR #38);
  - the 196 empty sAlpha replies (dry run, 13 Oct) are worth a look;
  - ML research variants (4)-(8) from the earlier plan.
- **Owner checks:**
  - GCP publish: the merged draft publishes sAlpha report text (scrubbed)
    and Perceptor report links; confirm (b) publish, or ask for (a) labels
    only with an opt-in in `deploy/gcp/INTERFACE.md`, before the Go export
    is written (section 4 item 8; PM recommends (a): both are paid bots);
  - ML: the `calls_prev` read-only SQL Q1/Q2 (13 Oct session state);
    `GATE_MIN_POOLED_WEEKS = 2` is an implementer default the owner may
    change;
  - prod at e522918 (or later) with `-web` restarted, and `-track` once if
    unsure it has #42. Then the browser check of the
    Model 7d column at 1280/390 px, light and dark (checklist in #49), and
    the Timing tab check. The first delivery with the model line on a
    screen. Perceptor filter "Caution" includes tokens with only "today:
    caution". No CSP errors;
  - save each model report under `ml-results\models\<version>\report.md`;
  - optionally add the two `review.md` lines suggested in #53;
  - invite link (after #55): `scout_deliveries.target` still stores the
    raw invite link (needed for the re-ask match; not on the website), and
    older logs / `scout_deliveries.error` rows may contain it. Read-only
    count in #55. If a DB dump is ever shared, revoke and re-create the
    link in Telegram;
  - after the "no calls_prev" PR merges: the server steps in Session state
    (pull, refresh `$ML/src`, check, restart `scout-model`, retrain with
    `--variants`), then send `report.md`;
  - PC clean-up of merged worktrees and branches: the script
    `cleanup-merged.ps1` (dry run first, then `-Apply`) was sent 14 Oct;
  - disk space: ~318 GB free on C: (12 Oct; was ~100 MB before the
    owner's 10 Oct clean-up, which blocked Go builds); old agent
    scratchpads under `...\AppData\Local\Temp\claude\...` (~5.5 GB) may be
    deleted by the owner;
  - price providers: confirm which external USD price sources may be used as
    a fallback;
  - DNS manager: who hosts DNS for lylelabs.io, needed to point
    `scout-analytics.lylelabs.io` at Cloud Run (section 4 item 8);
  - public repo (decided public, 12 Oct): items (a)-(c) in section 3
    "Decisions 12 October";
  - stopgap decision: whether to ship an interim version of the public site
    before the full GCP export exists (section 4 item 8);
  - re-scan 250/day and 1440 h max age: a proposed raise from the current
    Perceptor re-scan defaults (100/day, 30 days; section 3 "Prod
    settings"); the owner decides.
- **Later:** Timing tab frontend (`/api/analytics` format 4): merged (PR
  #22) and deployed 12 Oct; the owner still records the real gzipped size
  (top); the "reached 2×" rule (PR #27) and the real time held (PR #33)
  are deployed (owner restart 13 Oct); ML timing
  outcome columns: the
  leakage guard (`FORBIDDEN_PREFIXES`/`FORBIDDEN_COLUMNS`) for the timing and
  rug columns (`peak_late_after_s`, `first_2x_after_s`, `above_2x_s`,
  `fall_below_2x_after_s`, `above_2x_censored`, `timing_at`, `rug_at`,
  `rug_at_kind`) is on `main` since PR #19, so they can now be added to
  `scout_call_dataset_v`; GCP export in Go per `deploy/gcp/INTERFACE.md`
  (the interface is merged, PR #18; the export itself is not written yet,
  waits for the publish decision); the mcap comparison query.
- **Paused:** multi-source calls (section 4 item 6).

## What the system is

Repo `kfukue/scout-analytics`, branch `main` (the only branch carried over).
Production runs from a checkout of this repo since the cut-over (section 5,
owner confirmed 8 October); prod runs `main` at **68a4954** (listener,
tracker and `-web` restarted by the owner 12 Oct after the pull; section 3).

- Listener: reads @scoutrobinhood, tells real calls from "hit 3X" update posts
  (`postkind.go`), sends new tokens to @perceptor0xBot and @salpha_research_bot,
  delivers to the private group, records to Postgres. After a restart it
  catches up on missed posts (section 1, 08b3a71).
- Tracker (`-track`): on-chain prices from Uniswap v2/v3/v4 pools and Pons V2
  bonding curves, returns at 1h/1d/3d/7d/30d, 5-minute and hourly candles,
  pre-call trading stats, token names from `name()`. Only the first real call of
  each token is tracked; later calls get status `repeat`. A latest-price pass
  keeps a current return per token. Rolling worker queue.
- Website (`-web`, no login, read-only): one row per token from an in-memory
  snapshot refreshed every 15 seconds, live updates over Server-Sent Events, a
  "Refresh now" button, 17 columns (incl. Model 7d since PR #50), search, sorts, Perceptor
  filter, legend and a per-row report detail pane. Plain JavaScript in
  `frontend/` (no framework, no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. First
  trained on prod 8 October, all gates FAIL (section 4 item 1);
  `ml/RUNBOOK.md` has the steps.

## 1. Shipped

Merged to `main` through PRs #10–#14 (the last is a527d2c, up to 40e1888):

- Latest % (514b073); eth_getLogs range-size fix (af8e8fe); Call MC and Latest
  MC columns (9894e0f); Refresh now and the 17-column layout (1f4e206);
  `-retry-no-pool` (8ed80f9).
- Report detail pane (`GET /api/call`) and the rug guard (8846dc6): quote side
  under `SCOUT_RUG_LIQ_USD` (default $500, set in `.env`) means rugged, −100%
  from the rug point; bound prices (2^128) ignored; 1e6× backstop.
- Pons V2 support (d39f399, 730b1c2): curve pricing from `CurveBuy`/`CurveSell`,
  graduation to the v4 pool through the Pons hook, no USD rug check on the curve,
  graduation found from indexed logs (no look-back limit), no PoolManager
  fallback for Pons.
- In 40e1888 (the subject line only mentions ops and ML):
  - v4 pool discovery via `Initialize` (no PoolManager-wide scan).
  - USD-source fix: one lookup chain everywhere (stablecoin → Robinhood feed →
    mainnet feed → own pool against ETH or a stablecoin, chosen per block); no
    cached errors; state saved only when every step of a horizon segment
    succeeds; feeds loaded from `asset_chains`, with the env var winning on
    conflict; Chainlink aggregator switches handled; gecko uses reserve/2.
  - Ops scripts: `ops/2026-10-rug-retrack`, `ops/2026-10-pons-v4-retrack`,
    `ops/2026-10-asset-chains-feeds` (the asset_chains seed: 33 feeds, now live
    from the DB; `chains.id` 21 = Robinhood Chain).
  - ML: outcomes over `MAX_OUTCOME_PCT = 1e5` excluded, simulation return capped
    at `SIM_MAX_RET_PCT = +1000%` (owner approved), tracker columns forbidden as
    model inputs.

Pushed to `origin/scout-call-model` (local branch even with it, at 08b3a71)
and merged to `main` through PRs #15–#19 (the last is 21139e1, 7 Oct 23:06
-07:00, up to 08b3a71; `main` and the branch have the same tree). Then #20
(4369a1d, merging d49fe9c: handoff and ops index after the liquidity
re-track). The old repo's `origin/main` at 4369a1d and `origin/scout-call-model`
at d49fe9c have the same tree for `telegrambot/scoutanalytics`; this repo was
extracted from 4369a1d.

- #15: 4cd2bd5: Pons-v4 re-track skips rows already tracked by the Pons-aware
  code.
- #16: 39b1bac: website restyle (after oca.lylelabs.io), sAlpha declines,
  rugged rows without a peak, 10ⁿ numbers, one Perceptor line. 4eb6fbe:
  previous handoff.
- #17: 7a54f78: USD-repair ops scripts (`ops/2026-10-usd-repair`). 7cf8dd3: SSE
  live updates (`GET /api/events`; the listener `NOTIFY`s, the web process
  `LISTEN`s and refreshes its snapshot; notices only for calls < 1h old;
  `-backfill` sends no NOTIFY). 14052f8: rolling worker queue, deterministic
  `blockAt` + head cache, segment checkpoints, USD own-pool whole-history fix,
  the no-price-yet cap, `-price-check` loads the DB feeds. bde6d81: `.claude/`
  agent setup and permissions, including the `PowerShell(git push:*)` deny.
- #18: 75f666c: liquidity re-track scripts; ML first-call-only training rows,
  view columns test, `ml/RUNBOOK.md`. 2da28ab: ops rename
  (`2026-10-v4-retrack` → `2026-10-07-liquidity-retrack`) and the ops index
  `ops/README.md`.
- #19: 08b3a71 (one commit; the subject line only mentions the multi-select):
  - **Listener catch-up** after a restart: the resume cursor is min(DB,
    `poll_cursor.json`); the newest `SCOUT_CATCHUP_MAX` (default 100) missed
    posts no older than `SCOUT_CATCHUP_MAX_AGE` (default 24h) are handled live,
    older ones stored only; calls still `queued`/`dropped` within 72h are
    requeued; polling reads full batches (`catchup.go` and tests).
  - **Perceptor multi-select** filter on the website (`verdict=clean,caution`).
  - **`DEPLOY.md`** (new).

Since the cut-over (8 October) prod runs from the new checkout of this repo
(section 5); the old checkout (old repo at 4369a1d) is kept for rollback.

In this repo (`kfukue/scout-analytics`; its PR numbers restart at #1, so these
are not the old repo's #10–#14 above), merged to `main` on 9 October:

- #11: 46a59e4: deflaked pacing test; parallel coder limit raised to 10
  (owner).
- #12: eff4868: ML dead rule off by default; separate report-only "dead after
  the call" score.
- #13: 251f5ba: token supply lookup; Analytics "Market cap at call" = price ×
  supply (format 3).
- #14: 40fb014 (merge dc0a42d): timing after the call (time to peak, 2×
  reached/held/fell back, rug time) and `-backfill-timing` (section 4 item 7).

## 2. Merged and deployed: the 8 October work and the re-scan backend

Both branches went out together. `work/2026-10-09-perceptor-rescan`
(stacked on `work/2026-10-08-ml-pool-web`) was merged as PR #1 into
`main` = **4e7e049**. Because it was stacked, the PR also brought in the
8 October commits:

- 26da9d8: ML script (a).
- 4707e77: DB pool size (b). **It also contains all of the README changes of
  8 October**, including the website docs (c) and the stale wording, because
  all README hunks were staged into it.
- 5ad017c: website code (c): `frontend/*` and the web Go files, including the
  2 h stale limit.
- e50695b: HANDOFF.
- 0a46f28: the Perceptor re-scan backend (section 4 item 2).
- c3a1cec: HANDOFF for the re-scan.

a. **One-command ML training**: `ml/run_training.sh` (new), `.gitattributes`
   (new: `*.sh` always LF), `ml/RUNBOOK.md` ("Quick way: one script" and the
   Cleanup note). Tested locally on Windows with stubs, not on Linux.
b. **DB pool size**: `clamp(effective tracker workers + 4, 4, 32)` (effective =
   `SCOUT_TRACK_WORKERS` with the on-chain source, 1 with GeckoTerminal); an
   explicit `pool_max_conns` in `SCOUT_DATABASE_URL` is respected; the startup
   line shows the pool size and where it came from. Files: `main.go`,
   `scout_models.data.go`, `tracker.go`, `internal/database/database.go`,
   `poolconns_test.go` (new), `scout_models_db_test.go`, README/DEPLOY.
   `internal/database` is no longer an unchanged copy of the API repo's (new
   `SetupDatabaseMaxConns`).
c. **Website readability + `?days=` age filter**: `frontend/*`, `web.go`,
   `websnapshot.go`, `web_db_test.go`, `websnapshot_test.go`, README website
   section. Reviewed.

Done: the website "Perceptor today" label (section 4 item 2), merged
(1970c35, PR #3 = merge ecb7d55; PR #2 = merge f7b0ee0 carried only the
ML fixes bb6ac10) and deployed.

## 3. Prod state and pending owner actions

State on 7 October:

- All re-tracks are done (see `ops/README.md`): rug 151, Pons-v4 539, USD
  repair 50 + 1 nudge, liquidity 649 (cutoff 2026-10-06 12:53:00-07,
  `reset_at` 2026-10-07 12:18:38.004137-07; 52 found rugged). The owner chose
  to re-track only the pre-rug-guard v4 calls (647 at selection time) rather
  than all 1,166 old v4 calls. No ops folder is pending.
- About 4,733 first calls, about 99% priced.
- Rugged: about 12% overall; 395 of 2,867 v4 calls.
- No stuck `queued`/`dropped` calls (the requeue preview was empty).
- The 33 asset_chains feeds are live from the DB; `.env` still has
  `SCOUT_CHAINLINK_FEEDS` as a backup (the env var wins on conflict).

Done 8 October (owner confirmed): `kfukue/scout-analytics` created and `main`
pushed; prod cut-over to the new checkout. Prod `git rev-parse HEAD` =
**4e7e049**, run on the server (owner confirmed 8 October; for DEPLOY.md
3.0).

Deployed 8 October (owner confirmed):

- The processes were restarted on 4e7e049.
- The tracker startup lines show: the pool at most 16 connections for 12
  tracker workers; 33 Robinhood feeds from the asset database; "latest
  prices on: … every 1h0m0s for older ones; at most 400 call(s) per cycle".
- `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are now in the prod
  `.env`.
- `SCOUT_RESCAN=on` in prod since 8 October: the re-scan lane runs with
  the defaults (at most 100 a day, 10 m gap, 30-day max age).
- Lesson: the first restart picked up a different `.env` file and ran with
  the defaults (24h/200). The tracker reads `.env` from the folder it is
  started in, and a variable already set in the shell wins over the file.
  After every restart, check the `latest prices on:` line.

Done 10 October (owner confirmed): prod checkout at **dc0a42d**; listener,
tracker and `-web` restarted 9 Oct 22:34 PDT after the pull (they run the
timing backend); `timedatectl` shows the clock synchronized (NTP active);
the timing negative-values check returned 0.

Done 12 October (owner): prod checkout at **68a4954**; listener, tracker
and `-web` restarted on it (Perceptor re-check, score live fixes, Timing
tab). The listener start relabelled the 54 "not enough data yet" Perceptor
rows to `insufficient`. Models 20261012-1917 and 20261012-2209 trained on
prod after the deploy (all buckets FAIL; Session state at the top). Owner
checks from the deploy are listed at the top. Main moved on to 228b3e0
(PRs #26-#30) the same evening; its deploy is not confirmed yet ("Owner
to-do: deploy 228b3e0" at the top).

Still to confirm in prod:

- the `latest prices: … waiting` count falling over the first 1–2 hours;
- the listener catch-up/polling lines and the `website on …` line;
- the phone check of the new table and the "stale" label.

Pending owner actions:

1. Done 8 October: the first ML training (model 20261008-1852; results in
   section 4 item 1).
2. Done 8 October: failed-rescan limit decided = 2 (section 4 item 2).
3. Answer the five early-transaction questions (section 4 item 4).
4. Done: `.claude/commands.txt` is in `.gitignore` (c9e9fcc, PR #6 =
   merge 0122a98).
5. Optional: the sizing queries (read-only, in pgAdmin).
6. No longer needed (resolved by query, 8 October): `-price-check` on
   calls 664, 578 and 504 (the "Uniswap V4" flat calls, section 4 item 1).
7. Answer the five Analytics page questions (section 4 item 5; the PM chose
   defaults meanwhile).

`.claude/commands.txt` is an untracked local prompt file (no secrets); it is
ignored since c9e9fcc (`/.claude/commands.txt` in `.gitignore`).

Decisions taken 8 October: the 1-hour latest-price refresh for old calls is
approved; the Perceptor re-scan is approved with the defaults in section 4;
`env.tmp` in the session scratchpad was deleted by the owner. Decided
8 October: long model waits until about 25 December (option a).

Decisions 9 October (owner):

- Price-based market cap at call: approved. A `totalSupply()` read per
  token on the owner's node, at the entry block while the full node still
  has state, else latest; stored in `scout_call_tracking.token_supply` /
  `token_supply_block`. Analytics factor = price at the post × supply
  ("fully diluted"); no fallback to the posted figure; not an ML feature
  for now. Shipped (251f5ba, PR #13, 9 October).
- sAlpha: stop forwarding declines. Re-ask 6–24 h after the call when the
  reply was a decline, empty or a timeout (`scan_kind='reask'`, separate
  from the Perceptor rescan lane, at most 60 a day; the follow-up is
  delivered as a reply to the original delivery message; never feeds ML).
  In progress (coder).
- ML: the dead rule is off by default (the collapse label is plain again);
  a separate report-only "dead after the call" score. Shipped (eff4868,
  PR #12, 9 October).
- ML: keep DEX families, not raw DEX names (raw wins only in the test
  period, not walk-forward). The "tradeable runners" variant: revisit for
  the 7-day model after more weeks.
- Multi-source calls (still paused, section 4 item 6): reused
  Perceptor/sAlpha reports expire after 1 day for all channels. Call
  Analyser: deliver only the ORIGINAL caller's own post per token (forward
  the KOL message Call Analyser links to; later callers are recorded and
  tracked, not delivered; fall back to Call Analyser's repost if the KOL
  post can't be read). "Diamond" (💎) = elite.
- The Analytics Timing tab (section 4 item 7): the backend is merged to
  `main` (PR #14, dc0a42d); `-backfill-timing` was run on prod on 9 October
  and checked with owner-run read-only SQL (results in section 4 item 7;
  the negative-values sanity check is done: 0). The tab itself (frontend,
  format 4) is merged (PR #22, 68a4954) and deployed 12 Oct (prod runs
  68a4954). Public website on GCP Cloud Run (item 8).

Decisions 12 October (owner):

- Model line on deliveries: show the model numbers on deliveries even when
  buckets fail, clearly marked ("(not validated)"), because the owner is
  the only reader of the private delivery group. Scoring-service env var
  `SCOUT_MODEL_LINE=passed|all` (default `passed`); prod sets
  `SCOUT_MODEL_LINE=all` in the scout-model unit. Scores still never filter
  deliveries. Supersedes the earlier rule "scores only appended once gates
  pass" for this owner-only group. Code merged (PR #23, c31d859); docs
  updated in the 12 Oct handoff PR (`work/2026-10-12-handoff`). Changing
  the value needs a restart of the scoring service only.
- The GitHub repo is meant to be PUBLIC. Public-repo audit (12 Oct): no
  live credential and no secret file in any commit, stash or remote ref;
  infra details scrubbed from the docs in the 12 Oct handoff PR. Still open
  for the owner: (a) personal email in the commit author metadata of the 57
  commits since 8 Oct: set `user.email` to the GitHub noreply address in
  the checkouts and enable GitHub's "Block command line pushes that expose
  my email"; rewrite history or accept (owner decision); (b) skim the 23 PR
  descriptions/comments on GitHub for infra detail (or install `gh` so an
  agent can scan them); (c) whether to also remove the prod database name
  and "shared with the main API" from DEPLOY.md/README.md.
- Timing tab touch-and-drop: option (a) is rejected. A call counts as
  "reached 2×" only if its total time at or above 2× (`above_2x_s`) is at
  least 1 hour. A call that touched 2× but held less than 1 h counts as
  "did not reach 2×": it stays in the totals/denominators and is left out of
  the time-to-2×, time-held, fell-back and "still at 2× after t" figures.
  The rule applies in every window; windows too short for it (1h) show
  "n/a — needs 1 hour held"; touch-and-drop calls count as 0 held (the
  owner may still object to that reading). Merged as PR #27 (de62901; head
  8368747): frontend, README and tests only, no API or schema change, no
  re-track; not deployed yet (needs `-web` restarted on 228b3e0).
- Relabel re-check: the owner's read-only count of `unknown` + "not enough
  data yet" Perceptor rows returned 0 (consistent with the 54-row relabel
  at the 68a4954 listener start).
- Disk: C: has ~318 GB free; Go builds and throwaway Postgres work again.
  The testing setup stays PAUSED (`verify` agent, testdb scripts, CI).
- ML: retrained on prod after 20261012-1917: model 20261012-2209, all
  buckets FAIL (medium lift 1.998 against a gate of 2). The stability
  diagnostics are merged report-only (PR #29, 27404b6; saved model, gates
  and scores unchanged; `SCOUT_ML_DIAGNOSTICS=0` turns them off for one
  run); the gate decision (a)/(b) waits for the first report with them.
- Agent setup redone by function (PR #26, 57fdced): 5 agents + `scout-*`
  skills; Claude Code restarted on it.
- Perceptor re-check deliveries carry the first-pass score line (PR #28,
  1328ff1), kept in memory by the re-check lane and never recomputed; it
  is lost after a full listener restart. Persisting it ((a) a `score_line`
  key in the held Perceptor investigation's `details` JSONB written in
  `MarkHeldForRecheck`, or (b) a column) is an open owner decision;
  shipped without it for now. The same PR makes `botAPISend` UTF-8 safe
  (`splitMessage`).
- PM git commands (PR #30, 228b3e0): the PM has Bash for read-only git and
  `git worktree add`, Edit/Write for `HANDOFF.md` and `.claude/skills/**`
  only; `settings.json` denies `git commit`/`add`/`push`/`stash`; every
  commit hand-off has exact commands with an absolute `cd`, checks and the
  PR link; a second branch on the same files gets `git merge --no-edit
  main`, run by the owner (merge only, never rebase). Takes effect after a
  Claude Code restart.
- Open (new): pull `botAPIRedact` forward from the sAlpha re-ask branch as
  its own small fix (the bot token can reach the listener logs through a
  `*url.Error`; pre-existing).
- Lessons: the Agent tool's `isolation: "worktree"` fails on this PC
  ("Could not read the repository git config to neutralize filter
  drivers", every time on 12 Oct; the repo has no filter drivers), so
  worktrees are created with `git worktree add` (by the PM, after the
  restart on PR #30). A relative `cd` in a commit hand-off ran git in the
  wrong worktree (12 Oct); hand-offs now use absolute paths and check
  `git log -1` before the push.

**Prod settings** (for reference):

- `-track` with `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12
  SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48`, latest-price pass on;
  `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are in the prod
  `.env` since 8 October (the website's 2 h stale limit assumes them).
- `SCOUT_RESCAN=on` since 8 October (defaults: `SCOUT_RESCAN_MAX_PER_DAY=100`,
  `SCOUT_RESCAN_GAP=10m`, `SCOUT_RESCAN_MAX_AGE=720h` = 30 days).
- The web port is `SCOUT_WEB_ADDR` in `.env`.
- Nitro runs with `--execution.rpc.log-history=0` (full log index; a full
  node, not an archive). Keep it, or old ranges slow down again.

Server firewall and network exposure: kept out of this public repo; the
owner keeps them in local notes (reviewed and tightened by the owner on
12 Oct).

## 4. Next work, in order (owner's decision)

1. **ML model** (ml-coder agent). Run by the owner on the prod server via
   `ml/RUNBOOK.md` ("Quick way"). No GPU needed: CPU only. The long (30d)
   horizon is skipped until it has 2,000 matured rows over 120 days, about
   25 December 2026; the owner chose on 8 October to wait rather than
   loosen the threshold. The gates are strict. Scores never filter
   deliveries; they are only appended as one line. Which buckets it shows
   is `SCOUT_MODEL_LINE` on the scoring service: `passed` (code default,
   gate-passing buckets only) or `all` (failing buckets marked "(not
   validated)"); prod is to run `all` once the scoring service is
   installed (owner decision 12 Oct, section 3
   "Decisions 12 October"; only he reads the delivery group). Latest
   report: 20261012-2209 (trained on prod 12 Oct, 4,759 rows), all buckets
   FAIL: short lift 0.95, WF 3/6; 3day lift 1.66 [0.95, 2.76], WF 6/6;
   medium lift 1.998 [1.19, 3.22], WF 4/4; long skipped (Session state at
   the top; previous 20261012-1917). From the next retrain on 228b3e0 the
   report also has the report-only stability diagnostics (PR #29; seeds,
   pooled and rolling walk-forward with bootstrap CI, no-dead evaluation,
   adversarial validation, stronger regularisation; `SCOUT_ML_DIAGNOSTICS=0`
   turns them off for one run; the `[diagnostics] …` line gives their
   runtime, not measured on prod yet).
   - **First training on prod, 8 October:** model 20261008-1852, all four
     buckets FAIL. 4,709 first calls after exclusions. Train time 6 s.
     Python 3.11 was installed alongside the system 3.10 (Ubuntu 22.04);
     the venv was created with `python3.11 -m venv ~/scout-ml/venv`. Never
     replace the system python3. Reports copied to the PC go in
     `ml-results/` (gitignored).
   - **Results:** the collapse models are strong (test ROC AUC 0.80–0.87;
     skipping the top 30% avoids about 50% of collapses). Runner lift
     fails: short 1.16, 3day 1.83 (CI about 0.9–3.2), medium 0.52, long
     not meaningful. Buying all calls loses −21% (1d), −27% (3d), −44%
     (7d).
   - **ML fixes done (ml-coder, 8 October; merged to `main` in bb6ac10,
     PR #2 = merge f7b0ee0):**
     - drop `secs_since_prev_call`/`prior_calls` (always NULL/0 for first
       calls);
     - categorical levels from train rows only, case-insensitive (old
       saved models with flat meta keep exact matching, so the model on
       the server scores as trained until retrained);
     - a real `long` skip;
     - for medium/long, a 14-day date-based forward test plus purged
       k-fold (K=5, at least 3 usable folds, else a fixed 150 rounds)
       inside train for the rounds and Platt calibration;
     - Wilson lift intervals in the report;
     - the `run_training.sh` guard now checks `MIN_MATURED` in
       `config.py`.
     - The gates are unchanged.
     - Tested locally: 74 pytest passed.
     - Next: retrain on prod with
       `REPO="$PWD" ML=~/scout-ml bash ~/scout-ml/run_training.sh
       --skip-install` and copy `report.md` to `ml-results/`.
   - **Done since (merged to `main`):**
     - DEX names grouped into stable families (182022e,
       PR #4 = merge 296cad5; family rules completed in b2154ab, PR #9 =
       merge 6284538). There are about 50 posted DEX names, many
       short-lived and time-specific: e.g. "Pons" until mid-Aug then "Pons
       V2", "Pools Trade Instant" in Aug only, "O1 Rwa" for one week.
     - Recent-window calibration (the base rate drifts week to week;
       182022e, PR #4).
     - A "dead after the call" outcome (`trades_24h`; 182022e, PR #4); the
       dead rule is off by default and there is a separate report-only
       "dead after the call" score (eff4868, PR #12 = merge 87d3e94).
     - `--variants` comparison (dead rule, DEX inputs, tradeable runners;
       b2154ab, PR #9).
   - **Queued ML items:**
     - Quote assets into stable families (queued with the DEX families on
       8 October; `ml/` has no quote-asset families yet, only
       case-insensitive level matching).
     - Optional: refit on all data before saving the model.
     - Optional: case-insensitive `pre_vol_unit` / `price_unit`.
   - **Query findings (the owner ran them 8 October):**
     - Pons V2 is a steady pump and dump: the 1d collapse rate is 0.58–0.83
       every week; the median peak is +50–90%, then −75 to −90%.
     - The Uniswap v4 collapse rate fell from 0.57–0.75 (late Aug to mid
       Sep) to 0.16–0.26 (from 21 Sep).
     - Backfilled and live calls agree in the same week (no provenance
       artefact).
     - Collapses are rarely total losses (0–9%).
   - **"Uniswap V4" flat calls, resolved 8 October: real dead tokens, not
     a tracker bug; no fix and no re-track.**
     - About 230 first calls posted "DEX: Uniswap V4", quoted in ETH, in
       the weeks of 21 and 28 Sep (to about 2 Oct); e.g. calls 664, 578,
       504.
     - The owner's read-only query showed the tracker follows the active
       pool: 100–250 buy-heavy swaps in the 60 minutes before the call,
       then only 0–30 trades in the next 24 h; flat price; latest return
       about −10% (ETH drift). No zero-amount (hook) swaps.
     - The implied mcap is about 10x below the posted mcap (likely a 10B
       supply).
     - A second pattern, 2–5 Oct: many tokens peak at about 9.0–9.3e-8 ETH
       with lows of about 4.5–5e-8 ETH; likely templated launches with a
       fixed liquidity range.
     - ML impact: the low v4 collapse rate from 21 Sep is partly dead
       tokens counted as non-collapses, hence the "dead after the call"
       outcome (now merged, above).
2. **Perceptor re-scan of first calls without a Perceptor report**
   (approved 8 October with the defaults below). **Backend merged
   (0a46f28, in PR #1 = 4e7e049) and deployed 8 October; the lane is on
   in prod since 8 October (`SCOUT_RESCAN=on`).** The website "Perceptor
   today" label is merged (PR #3) and deployed.
   - What it does, backend only:
     - `scan_kind` column on `scout_investigations` (`'live'`/`'rescan'`,
       idempotent `ADD COLUMN IF NOT EXISTS`, default `'live'`) with a
       partial index;
     - every existing reader uses `scan_kind = 'live'`: the dataset view
       (no leak of today's verdict into ML features), the requeue checks,
       the website verdict/filter/counts and `/api/call`, and sAlpha;
     - a rescan lane inside the listener (`rescan.go`; one Telegram session,
       never a second process; Perceptor only, no delivery, no
       status/seen/score changes), off unless `SCOUT_RESCAN=on`;
     - `-rescan-missing -dry-run` lists and counts the candidates;
     - website snapshot fields `perceptor_today_verdict`,
       `perceptor_today_url` (https only) and `perceptor_today_at`;
     - NOTIFY payloads carry `scan_kind`;
     - a stop while reading the report stores nothing;
     - an in-memory floor keeps the 10m gap when inserts fail.
   - Files (15): `DEPLOY.md`, `README.md`, `events.go`, `events_db_test.go`,
     `events_test.go`, `main.go`, `scout_models.data.go`, `scout_models.go`,
     `scoutanalytics.sql`, `tools.go`, `web_db_test.go`, `websnapshot.go`,
     and new `rescan.go`, `rescan_db_test.go`, `rescan_test.go`. The README
     includes the "Reading the list" wording fix (STALE_OLD_MS).
   - Tests (local): `go vet` clean; `go test -race` ok; the
     throwaway-Postgres run gave 286 PASS / 0 SKIP, including the 8
     `rescan_db_test.go` tests. Web benchmark roughly unchanged (HTTP paths
     ≤ 0.21 ms/op).
   - Shipped: committed on `work/2026-10-09-perceptor-rescan` (stacked on
     `work/2026-10-08-ml-pool-web`), merged as PR #1 (4e7e049), deployed
     8 October with the 8 October work.
   - Prod notes:
     - Done: the "Perceptor today" label was deployed, and
       `SCOUT_RESCAN=on` is set in prod since 8 October.
     - The candidate count: a read-only pgAdmin query (the PM provides
       it).
     - Send the dry run's output to a file (it may be thousands of lines).
     - Rollback caveat: an old binary would read rescan rows as call-time
       verdicts (in DEPLOY.md).
   - Open:
     - **Decided 8 October:** drop a call after 2 failed rescans (as
       `rescanFailedLimit = 2`; no code change).
     - The daily cap can undercount while database inserts fail (the gap
       still holds).
     - A "Perceptor today" change sends no SSE event in 4e7e049; the label
       (merged in PR #3) adds one (`report` with `tool: perceptor_today`).
   - Done on this line: the frontend "Perceptor today" label, merged
     (PR #3) and deployed; the lane is on in prod since 8 October.
   - Off by default: `SCOUT_RESCAN=on`. Settings and defaults:
     `SCOUT_RESCAN_MAX_AGE=720h`, `SCOUT_RESCAN_MAX_PER_DAY=100`,
     `SCOUT_RESCAN_GAP=10m`, `SCOUT_RESCAN_IDLE=5m`,
     `SCOUT_RESCAN_STATUSES=backfill,duplicate,failed,scanned`.
   - Filters: first calls only; no completed Perceptor report of any kind;
     not rugged; `latest_return_pct` NULL or > −99; at most 2 failed
     rescans. A live call can wait up to about 5 min (shared account pacing
     2m5s).
   - The schema change runs at the next start of any mode; it does not make
     the tracker redo history.
   - The existing `-scan`/`-post` must not be used for this: they deliver,
     overwrite status, and would leak today's verdict into the ML features.
3. **Fresher latest price for old calls** (approved 8 October, config only,
   deployed 8 October in 4e7e049, now in the prod `.env`; see "Deploying"):
   `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` on the tracker. No
   history re-run; ≈ 133k `eth_getLogs`/day in total (≈ 1.5 req/s),
   +108k/day (≈ +1.25 req/s) over today's ≈ 25k; old calls' latest price at
   most about 1 h old instead of about 24 h. The website's stale threshold
   for old and rugged calls (`STALE_OLD_MS` in `frontend/app.js`) is now 2 h
   (was 48 h). Later option: batched multi-pool `eth_getLogs` for 15-minute
   freshness for all calls. Refresh on view is rejected for now (30–90 s
   latency, the web process would have to write a NOTIFY, no login).
4. **Early-transaction pattern analysis** (owner request 8 October;
   plan approved 19 October, see the last block of this item). For each Scout first call, look at
   the token's early on-chain transactions, from pool creation (or Pons
   curve launch) up to the call and shortly after, and find:
   - (a) whether certain addresses (deployers, funders, early buyers/sellers,
     LP providers/removers, wallets that recur across tokens) usually
     precede a rug;
   - (b) whether there is a distribution pattern (holder concentration,
     early buy/sell mix, wallet overlap) for calls that did well: over
     +100% at 7d or later.

   Process: the PM plans it with the researcher first (data available:
   `scout_call_precall`, `scout_call_candles`, pool/rug data, live buys in
   `scout_call_live_buys`; what more would need reading from the node; node
   cost; leakage rules for any ML use: only pre-call data as features),
   agrees the plan with the owner, and only then hands anything to a coder.
   No coder work until the owner approves the plan.

   **Plan drafted by the researcher (8 October, later); waiting for the
   owner.**
   - Data: the existing tables give labels and sizing only. No table has
     per-trade wallets, holder balances, deployers or LP actions, and
     `scout_call_live_buys` stores wallets truncated (prefix/suffix).
   - Labels: the `rugged` flag is censored (it grows with call age; the
     price rule sets no `rug_block`; a drain found after 30d gets the check
     block). Proposed primary label: rugged by 7d (`rugged` and `ret_7d` ≤
     −99.99, not rugged at the call).
   - Phases:
     - P0: the owner runs read-only queries Q1–Q5. Q5 filters on
       `scan_kind`; the column is in prod since 4e7e049.
     - P1: offline pilot of about 450 calls (150 rug-by-7d / 150 +100% / 150
       neither); a one-off, rate-limited, read-only exporter; files on the
       server; no schema change; about 8k `eth_getLogs`, under 1 h.
     - P2: full run, about 33k–80k `eth_getLogs` (about 0.6 day of tracker
       log load), after the post-deploy latest-price catch-up.
     - P3: only if a pre-call signal passes the ML time-split gate: `pre_*`
       columns on `scout_call_precall` (not a new table).
   - Leakage: only chain data at or before `entry_block` as features.
     Wallet reputation is point-in-time with a horizon + embargo, never
     computed over the whole history.
   - Owner questions:
     - the window (call + 1 h or + 24 h);
     - the +100% definition (horizon return or peak; gross or net of tax);
     - Blockscout API for funders (an outside service, needs a key);
     - the pilot size and the server folder;
     - the look-back cap for old tokens (7 days?).
   - No coder work until the owner approves.

   **Plan refreshed by `investigate` and approved by the owner (19 Oct
   2026; supersedes the 8 Oct draft above where they differ).** The
   8 Oct Q1-Q5 text was not kept anywhere; new P0 queries replace it.
   - Data: the prod node is a full node, not an archive, so balances at
     old blocks are rebuilt by replaying the token's `Transfer` logs from
     launch up to the call's `entry_block`
     (`scout_call_tracking.onchain->>'entry_block'`). Supply comes from
     the replay, never `token_supply` (falls back to the latest block).
     Wallets come from the token `Transfer` in the same transaction, not
     the router.
   - Labels (first calls, on-chain entry, 7d row done):
     - bad `dead7`: 7d late-entry return <= -90 %, excluding
       `rug_at_kind = 'at_call'`; `drain7` (<= -99.99 %) is shown for
       description only (about 70 cases);
     - good `held2x7`: `above_2x_s >= 3600` on the 7d row (the 12 Oct
       Timing-tab rule "reached 2×"); peak net of tax >= +100 % and 7d return
       >= +100 % reported alongside;
     - `rugged` is not used as a label (set long after the call; P0 Q4:
       of 904 rugged, 640 have no rug time at all and 190 were detected
       more than 30 d after the call, see the top section).
     - ML labels are net of tax, descriptive tables gross.
   - Features, all from logs at or before `entry_block`:
     - group (i), per token: holder count; top-1/5/10 share of
       circulating supply (pools, PoolManager, Pons curve, zero/dead and
       the token itself excluded); deployer share and whether it sold
       before the call; snipers and bundled buys at launch; unique buyers
       and sellers and the sell/buy ratio; v2 LP burned or held by a
       contract, v3/v4 net liquidity added or removed.
     - group (ii), across tokens (later, same drift risk as wallet
       reputation): the deployer's earlier tokens and their outcome;
       early-buyer overlap with earlier dead tokens; point-in-time with
       the wallet-reputation lag and embargo.
     - Names start `pre_hold_` or similar, never `pool_`; anything after
       the call gets its own forbidden prefix (e.g. `post_`).
   - Owner's answers (19 Oct): window: features stop at `entry_block`,
     the pilot also reads 24 h after the call for description only;
     "good" as above; pilot about 450 calls (150/150/150, spread across
     weeks, sized from P0), files in a new server folder outside the
     repo and `scoutanalytics_data/`, never committed; look-back cap
     **3 months** (tokens up to 90 days old at the call get their full
     history); 5 "yes" (Blockscout): read by the PM as approval in
     principle for deployer funders; either way no Blockscout use before
     the pilot, and the PM asks one explicit question before any key is
     requested (the owner adds it to `.env` himself).
   - Plan's handling past the cap: tokens older than 90 days at the call
     get no concentration features and a `capped` flag.
   - Node cost (estimate, not measured): one `eth_getLogs` covers up to
     200k blocks (about 5.5 h); two address-filtered streams per call
     (token `Transfer`; pool LP events). Young tokens about 10-20 reads
     per call; a token at the 3-month cap about 800. Q3 (age at the call)
     shows how many calls are old, so what the pilot and the full run cost.
   - Phases:
     - P0: the owner runs read-only queries Q1-Q6 (sizes, labels by week,
       age at the call, how censored `rugged` is, the baseline from the
       posted holders and Perceptor verdict, the pilot sample).
     - Code, small PRs with base `main`, one at a time: (1) pure transfer
       decode, replay and holder stats (no node, no DB; #33); (2) snipers,
       bundles, flow, early buyers (#34); (3) LP events (#35); (4) a
       read-only, rate-limited Go export flag (call ids in, files out; no
       DB writes, no schema change; new `SCOUT_*` names in `config.go`),
       split into 4a log reader, 4b row builder and writer, 4c flag (see
       the top section); (5) a report-only analysis script in `ml/`.
     - P1 pilot (description only), P2 full run, then ML: group (i) as a
       report-only variant in the existing buckets with adoption rules
       fixed before the run (as `WREP_ADOPTION`, one shot, no tuning),
       plus a small separate `dead7` classifier, same walk-forward gate.
     - P3, only if a signal passes: `pre_*` columns on
       `scout_call_precall` (schema change with run and rollback scripts).
   - Stop rules, fixed before P1:
     - after P0: fewer than 300 `dead7` or 150 `held2x7` → no ML,
       description only;
     - after P1: no group (i) feature with a bootstrap 95 % lower AUC
       bound above 0.60, or the separation vanishes within strata of the
       posted holders and the token's age at the call → stop (Perceptor
       verdict dropped from the strata after P0: 91 % of calls had no
       scan at the call);
     - during any run: the latest-price lag goes over 2 h → pause. The
       reads per call are reported (median and total), not a stop rule,
       since the 3-month cap makes old tokens expensive;
     - after P2: the variant fails its pre-registered rules → stop.
   - Never: change `onchainStateVersion`, make the tracker redo history,
     or write to the database before P3.

   **P0 results (owner ran Q1-Q5b in pgAdmin, 19 Oct 2026; first calls,
   on-chain entry, 7d result, not `repeat`):**
   - Q1: 4,761 first calls with a 7d result, 28 Jul to mid-Oct: v4 about
     2,690, Pons about 1,610, v3 about 400, v2 about 60. 2 non-USD, 2
     outcomes above +100,000 %. 29 `gave_up`, 8 `no_pool`, 1 call
     without a tracking row.
   - Q2 (12 weeks): `dead7` 1,481 (31 %), `drain7` 52, `held2x7` 883
     (19 %), peak >= +100 % 1,840 (net equals gross in every week), 7d
     return >= +100 % 178, held 2x then dead 121, rug at the call 7.
     The dead rate moves by week: about 17 % in early August, 42-44 %
     in the weeks of 31 Aug and 7 Sep, about 17 % in late September.
     After-P0 stop rule passed: ML stays in the plan.
   - Q3 (age at the call): < 1 h 3,741 (dead 33.5 %), 1-6 h 611
     (25.4 %), 6-24 h 192 (21.9 %), 1-3 d 57, 3-7 d 56, 7-30 d 54
     (3.7 %), 30-90 d 41, > 90 d none, unknown 9. The 3-month cap covers
     every call with a known age. Rough full-run cost: about 100k `eth_getLogs`
     (estimate, not measured).
   - Q4: 904 calls flagged `rugged`; only 58 drained by 7d (51 `event`,
     7 `at_call`); 190 `detected` more than 30 d later (46 dead at 7d);
     609 never flagged were dead at 7d; 1,213 have `rugged` NULL
     (mostly calls still tracking: Q1 has 1,199 tracking). Confirms the 7d return label. Side finding: 640 rugged calls have no `rug_at` or `rug_at_kind`
     (the 9 Oct backfill covered every rugged call then), which affects
     the Timing tab's rug figures; likely cause and options in the top
     section.
   - Q5a: 4,344 of 4,761 (91 %) had no live Perceptor scan at the call;
     among the scanned, dead7 is clean 26.9 %, caution 12.3 %,
     insufficient 12.5 %, red_flags 16.8 % (small groups; PM's reading:
     live scans began 29 Sep, so these calls are from late September on,
     when the dead rate was low).
   - Q5b (posted holders, quintiles): dead7 9.4 % (3-74), 16.8 %
     (74-101), 27.2 % (101-144), 40.4 % (144-235), 61.7 % (235+);
     held2x7 flat at 14.5-21.4 %. The posted holder count is the
     baseline the holder features must beat for `dead7`.
   - Q6 (pilot sample) is run when the pilot is ready.
5. **Analytics page**: two sections on a separate page (`/analytics.html`,
   linked from the list's top bar and back).
   - **Section 1, "Call performance": merged (PR #5) and deployed.**
     - `GET /api/analytics`: one compact row per first call, built once per
       snapshot (pre-encoded, pre-gzipped, ETag = hash of the body, 304);
       the browser groups and filters it. No new table or schema change; the
       load query gained the posted DEX, `entry_price_source`, the quote
       asset, a per-window `no_data` bit, the Perceptor verdict at the time of
       the call (the dataset view's rule, live scans only) and a "first 24 h
       of candles stored" flag. Trades in the first 24 h come from
       `scout_call_candles` (5-minute candles), read once and cached; only
       newly complete calls are read per refresh, all of them hourly. A
       failed trade-count read no longer fails the refresh: the counts held
       stay, the others show as unknown, and the next refresh asks again.
     - Page: window 1h–30d (1d default); "Quiet after the call" (< 50 trades
       in 24 h, not rugged) include / exclude / only; tables overall, by
       verdict at the call, pool family (v2/v3/v4/pons/GeckoTerminal), posted
       DEX (top 12 + other), quote asset, week (Mon–Sun UTC) or month, and
       week × verdict medians. Calls, with data, mean, median, win rate,
       ≥ +100% peak, collapse, median peak / drop, rugged; n under every rate;
       groups under 20 calls greyed. First calls, late entry, USD, before
       tax. Rugged calls keep their pre-rug peak here (the list hides it).
     - Defaults the PM chose (owner may change): weeks Mon–Sun UTC, returns
       before tax, 1d default.
   - **Chart batch: built 8 October, merged to `main` (45e448b, PR #7 =
     merge cb4c006)** (coder; reviewed; on prod since dc0a42d, started
     9 Oct 22:34 PDT). Three tabs over the same feed (`/api/analytics`
     format 2, which adds per first call the values known at the call:
     called-at market cap, holders, elite/good holders, elite/good live buys
     count and USD, and the hour before the call: buy/sell volume in USD,
     swaps, price change; same expressions as `scout_call_dataset_v`; no
     schema change).
     - Overview: weekly (or monthly) trend, outcome mix, "$100 on every
       call" (returns capped at +1,000%), hour × weekday heat map, and the
       section-1 tables, now with mean and median of return, peak and drop.
     - By factor: equal-count groups (4/5/10, zero group when ≥ 10% are 0),
       per-group histograms with fixed bands, table, scatter (log / symlog),
       typical path 1h → 30d, verdict × pool family grid.
     - Peak vs final: peak against return, share that gave back more than
       half of the peak.
     - Shared filters: window, metric, mean/median, quiet calls, week/month,
       pool family, verdict. Charts are hand-built SVG (the PM's task said
       no chart library), not ECharts as `coder.md` planned. (ECharts 6.1.0
       was vendored later for the Timing tab, PR #22.)
     - Review follow-ups (9 October): number factors with a zero group now
       cut the values below and above 0 separately (no group spans 0;
       matters for the pre-call price change); "$100 on every call":
       "All calls" and "No red flags" ignore the pool family and verdict
       choices (quiet choice only), plus one line for the chosen family /
       verdict; label and caption fixes.
     - Market cap factor: the owner chose "market cap at call from the
       price" (entry price × supply). Shipped in 251f5ba (PR #13 = merge
       2a50d32): the token supply is stored in
       `scout_call_tracking.token_supply` and the Analytics "Market cap at
       call" is price × supply (`/api/analytics` format 3); see section 3
       "Decisions 9 October".
     - PR history: PR #6 (merge 0122a98) merged only the skill commit
       (c9e9fcc); the chart batch itself came in PR #7.
   - **Section 2, "Model insights": next** (now only an empty "coming soon"
     heading). Plan: show the model report from the training run's
     `meta.json`, read from a folder set by a new `SCOUT_ML_DIR` setting;
     per-call scores later, and only out-of-sample ones.
6. **Paused: multi-source calls** (other Telegram call channels next to
   Scout). Nothing to build until the open questions are answered.
   - Decisions:
     - all channels in the website list and in the model, with scout-only
       comparison runs;
     - other channels' returns from our own node's stored candles;
     - past calls are stored; whether to track tokens without history is
       decided after a sizing query;
     - Perceptor + sAlpha for all channels; a report is reused unless it
       failed, is missing or unreadable; Scout calls first in the queue;
     - deliveries to the same group, labelled by channel, with
       clean,caution.
   - Open questions:
     - the age limit for reusing a report;
     - the channels: @CallAnalyserRobinhood recommended, @hoodchains as a
       possible baseline;
     - Call Analyser delivery: first post per token, or every caller.
   - Phase plan: 0 pin the readers to Scout; 1 Robinhood sources; 2 store
     Base; 3 track Base.
7. **Analytics Timing tab** (backend and frontend merged). Results
   after the call: never model inputs.
   - Status (9 October): the backend (the timing function, the columns
     below and `-backfill-timing`) is **merged to `main` (40fb014, PR #14,
     merge dc0a42d)**. Prod's listener, tracker and `-web` ran dc0a42d
     from 9 Oct 22:34 PDT (owner confirmed 10 October) until the 12 Oct
     deploy of 68a4954.
   - Status (12 October): the Timing tab frontend (`/api/analytics` format
     4, ECharts 6.1.0 vendored) is **merged (b4149ff, PR #22, merge
     68a4954) and deployed 12 Oct** (`-web` restarted by the owner on
     68a4954); the owner still records the real `/api/analytics` size
     (owner checks at the top).
   - Status (12 October, evening): the "reached 2×" rule is **merged
     (8368747, PR #27, merge de62901)**, not deployed yet (needs `-web`
     restarted on 228b3e0). Reached 2× = at or above 2× for at least 1 h in
     total, in every window; windows too short for it (1h) show "n/a —
     needs 1 hour held" (`can2xReach`); touch-and-drop calls count as 0
     held (the owner may object to that reading); calls under 1 h held stay
     in the totals and are left out of the time-to-2×, time-held,
     fell-back and "still at 2× after t" figures. Frontend, README and
     tests only; no API or schema change, no re-track. Checks after the
     deploy: 1h shows the n/a text, 1d shows numbers, legend at 1280/390 px
     light/dark, no CSP errors.
   - Backfill (prod, 9 October): the owner ran `-backfill-timing` (dry run
     and real run) and checked it with read-only SQL:
     - every first call has timing in every horizon (5,247 calls);
     - the ~149 rows per horizon without timing are all `repeat` calls
       (no late entry; expected);
     - all 245 rugged calls have a rug time: 65 `event`, 7 `at_call`,
       173 `detected` (approximate: the time of the end-of-tracking
       check);
     - 0 rows re-stored after timing;
     - negative-values sanity check (10 October): 0 negative values;
     - first numbers are in "Session state" at the top.
   - What it shows:
     - time to peak: how long after the call the best price came;
     - how long it held +100%: when it first reached 2×, total time at or
       above 2×, when it first fell back below;
     - time to rug, for rugged calls.
   - Shown four ways:
     - distributions in log bands: < 5m, 5–15m, 15–60m, 1–4h, 4–24h,
       1–3d, 3–7d, 7–30d;
     - mean and median by group: Perceptor verdict at the call, week/month,
       elite ("diamond", 💎) and good holders and buyers, holders, DEX
       family and the other existing factors, with the 4/5/10 equal-count
       groups;
     - a "still at 2× after t" curve (Kaplan–Meier, from the first cross)
       and a "share rugged by t" curve;
     - all on a new "Timing" tab with the same filters and factor picker.
   - Data: before dc0a42d no timing was stored (`scout_call_returns` had the
     peak size, not its time); the columns below (plus
     `above_2x_censored`) are on `main` since dc0a42d. Computed from stored
     candles
     (`scout_call_candles`: 5-minute for the first 24 h, hourly up to the
     last completed horizon ≤ 30d). No new node calls for peaks/2×, no
     re-tracking, `onchainStateVersion` unchanged. Resolution: 5 min on
     day one, 1 h after.
   - Design (researcher, 9 October):
     - one pure Go function, used by the tracker (right after each horizon
       is stored, in `saveHorizonOnchain`) and by a one-off backfill flag
       (e.g. `-backfill-timing`) from stored candles;
     - repeat-safe `ADD COLUMN IF NOT EXISTS` on `scout_call_returns`
       (`peak_late_after_s`, `above_2x_s`, `first_2x_after_s`,
       `fall_below_2x_after_s`, `timing_at`; in both the INSERT and the
       `ON CONFLICT` SET of `UpsertReturn`) and on `scout_call_tracking`
       (`rug_at`, `rug_at_kind`: `event` | `at_call` | `detected`);
     - the website reads them in the load query (format bump;
       `hashWebRows`).
   - Owner decisions (9 October):
     - (superseded 12 Oct for the Timing tab: "reached 2×" needs
       `above_2x_s` ≥ 1 h; section 3 "Decisions 12 October")
       "reached 2×" = the price TOUCHED 2× (candle high); time held at 2×
       is measured on closes, the last close carried across gaps; calls
       that never reached 2× count as 0 minutes held;
     - past rug times exact: one `blockTime` lookup per rugged call on the
       owner's node (a few hundred, once); future rugs: one lookup each
       (approved);
     - rugs at the moment of the call and rugs only detected at the end of
       tracking are shown separately, outside the main histogram;
     - the timings go into the ML dataset view as OUTCOME columns,
       forbidden as inputs (add a `timing_` forbidden prefix), so later
       report-only scores can predict e.g. "fast runner: 2× within 1 h",
       "early rug: rug within 24 h", "holds 2× ≥ 6 h".
8. **Public cloud website on GCP** (Cloud Run; draft merged, PR #18,
   4245939; Go export not written yet).
   - Owner decisions (9 October): GCP; the site is public (anyone with the
     link). GCP Cloud Run for the public read-only site
     `scout-analytics.lylelabs.io` (draft in `deploy/gcp/`, merged as PR
     #18; no GCP resources yet). Options A-C below are superseded and kept for history.
   - Plan: the prod server exports the website's numbers (built from the
     existing in-memory snapshot) to GCP when data changes. The public
     site never connects to Postgres or the node.
   - Options that were under study (superseded by Cloud Run):
     - (A) Firebase Hosting / Cloud Storage + CDN with JSON files;
     - (B) Firestore (one doc per call + summary docs, realtime
       listeners);
     - (C) both.
   - Never export: the private delivery group invite link, links into the
     private group, any secrets. Paid bots' full report text and report
     links are left out by default (verdict labels are fine) unless the
     owner decides otherwise. Open (12 October): the merged draft
     publishes sAlpha report text (scrubbed) and Perceptor report links
     (`perceptor.info`); the owner confirms (b) publish, or asks for (a)
     labels only with an opt-in in `deploy/gcp/INTERFACE.md`, before the Go
     export is written.
   - The infra agent drafts only; the owner creates the GCP resources and
     approves costs. The service account key stays on the prod server,
     outside the repo.

Later:

- ML label issues: the 30d collapse uses the `rugged` set after day 30;
  train/serve skew in `perceptor_verdict`. (`quote_asset` skew: fixed for
  new calls by PR #21, live scoring fills it from the pre-call; verify with
  the README's agreement SQL once scores are on.)
- GCP Pub/Sub for live updates when scaling (owner's plan).
- Resolved by 2b: `MaxConns` for `SCOUT_DATABASE_URL` was hard-coded to 4.
- Website summary strip ("now" medians and shares); it changes the summary
  `ETag`.
- `-scan`/`-post` attach to the newest post of the token, not its first call.
- FLOOD_WAIT is not handled on bot sends.
- `internal/database/database.go` discards the `pgx5.Connect` /
  `NewWithConfig` errors, keeps 2 connections outside the pool per process,
  and fatally requires `.env` in the working directory.
- Scatter plot with ECharts 6.1 after the model report (rules in the
  `scout-web` skill, "Charts").
- Other launchpads (about 46 unpriced calls); the launchpad query results are
  pending from the owner.
- Other call sources (Call Analyser channels): paused, section 4 item 6.
- In-flight scans cut off by a stop end as `failed` and are not requeued.
- Poison post: a stored-only post with a permanent DB error pauses polling.
- `prior_calls` and `secs_since_prev_call` (per token) are near-constant for
  first calls and not model inputs. **Corrected 13 Oct:** `calls_prev_1h` and
  `calls_prev_24h` count the whole channel (update posts included), are model
  inputs and drift with time (see the 13 Oct session state).

## 5. Repo migration (done; pushed and cut over on 8 October)

**Moved from `kfukue/geth-analytics` (history kept).**

- Done on 7 October: `git filter-repo --subdirectory-filter
  telegrambot/scoutanalytics --refs main` on a fresh clone of
  `https://github.com/kfukue/geth-analytics.git`, from `origin/main` at 4369a1d
  (same tree as `origin/scout-call-model` for this folder). 37 commits touch
  the folder (55 with merges). No secrets anywhere in the extracted history
  (no `.env`, `*session.json`, `calls.csv`, `scoutanalytics_data/` or `.exe`
  object). Only `main` was carried over; the old stash (`stash@{0}`, "On
  codex/scout-dashboard: scout-dashboard before main sync 2026-10-06") stayed
  behind in the old repo (owner's decision).
- Then one standalone commit, no functional changes: module
  `github.com/kfukue/scout-analytics` (`go.mod`/`go.sum` from the old repo,
  `go mod tidy`, same versions for every module still required);
  `internal/database/database.go` copied unchanged from the old
  `database/database.go` (same `.env` variables; since changed, see 2b); new `.gitignore`; `.claude/`
  copied with paths rewritten; docs and comments to `go run .` from the repo
  root.
- Old → new hashes (filter-repo rewrote every commit): 4369a1d → 7a4edaa
  (PR #20 merge), d49fe9c → 34d70c4, 21139e1 → 0795e9e (PR #19 merge),
  08b3a71 → b10251b. Older hashes in this file and in the ops READMEs are
  old-repo hashes; `git log --grep` or the subject line finds them here.

- Done on 8 October (owner confirmed): `main` pushed to
  `kfukue/scout-analytics`, and the prod cut-over below completed; prod runs
  from the new checkout. Prod `git rev-parse HEAD` = **4e7e049**, run on
  the server (owner confirmed 8 October).
- Rollback: the old checkout (old repo at 4369a1d) is kept; step 7 below.

**Prod cut-over checklist** (done 8 October; kept for reference):

1. On the server, clone the new repo next to the old checkout, e.g.
   `git clone https://github.com/kfukue/scout-analytics.git /srv/scout-analytics`
   (EDIT the path; a private repo needs a deploy key or token on the server).
   `go build -o /tmp/scout-new .` in it compiles everything before any
   downtime.
2. Copy `.env` from the old checkout into the new one with `cp -p` (never
   print or paste it). Check yourself, without pasting
   values, that the path-valued settings in `.env` still resolve from the new
   folder: `SSL_CERT_FILE_PATH` (the three `.pem` files), any credentials file
   for Secret Manager (`HOST_SECRET_PATH` access), and `SCOUT_SESSION_FILE`,
   `SCOUT_STATE_DIR`, `SCOUT_WEB_DIR`, `SCOUT_RPC_LOG_CACHE` if set. Relative
   paths need the files copied too, or absolute paths.
3. Optional read-only check from the new folder (needs `.env`):
   `go run . -price-check 0xTokenCA -price-at 6h`. It confirms the database and
   node settings and exits.
4. Stop the old processes, **listener first** (never two listeners on one
   Telegram session; `pgrep -af listen-only` must print nothing), then `-track`
   and `-web` (never two trackers at once).
5. Only now, with the old listener stopped (the session file and the state are
   final), copy `scout.session.json` with `cp -p` and `scoutanalytics_data/`
   (with `poll_cursor.json`) with `cp -a`. Never print or paste them.
6. Start from the new folder: `go run . -listen-only`, then
   `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4
   SCOUT_RPC_MAX_INFLIGHT=48 go run . -track`, then `go run . -web`. Check the
   `recording to SQL via …` line, the catch-up lines, `website on …`, and
   `curl /api/summary` (DEPLOY.md section 6).
7. Rollback: stop the new listener first, then the others; restart the old
   ones from the old checkout (still at 4369a1d). Its listener resumes from the
   lower of the database and its own `poll_cursor.json`.
8. After the cut-over: record `git rev-parse HEAD` in the new checkout for
   DEPLOY.md 3.0 (the old `/tmp/scout-prev-commit` hash is not in this
   history). The systemd drafts in DEPLOY.md use
   `WorkingDirectory=/srv/scout-analytics` (EDIT).
9. Removing `telegrambot/scoutanalytics` from the old repo is a separate,
   later decision; nothing in the old repo was changed.

## Rules the owner has set

- Never print, commit or copy `.env`, `scout.session.json`,
  `scoutanalytics_data/`.
- No new database tables without asking; extend existing ones. Every statement
  in `scoutanalytics.sql` must be safe to repeat (it runs at every start).
  Views are dropped and recreated at startup, dependents first.
- Shared tables (`assets`, `chains`, `asset_chains`) are never altered; the
  tracker only SELECTs from them. Change rows with SQL, not the API's
  `/assetChains` routes.
- Website speed is the priority: requests are answered from the snapshot and
  must not query the database (the only exception is the rate-limited
  `POST /api/refresh`). Keep p95 under 10 ms; rerun
  `go test -run xxx -bench BenchmarkWebSnapshot -benchmem` after touching it.
- Only real calls are scanned, delivered and tracked; one row per token.
- Agents other than the PM never commit or push. Since 12 Oct the PM adds,
  commits and pushes its own `work/...` branches after `review` passes and
  opens the PR; the owner merges. Never on `main`, never `git stash`.
- Files only inside the repo and the owner's personal notes folder (owner,
  14 Oct 2026; rule in `scout-rules`; the path is in his user-level
  `CLAUDE.md`).
- Wait for all checks (tests, `review`) before committing; separate clean
  commits per change.
- Say "tested locally" unless it ran on the prod server. Flag anything that
  makes the tracker redo history or adds ongoing node load before shipping it.
- Ask the owner before resetting calls; give him pgAdmin scripts.
- Prod uses **pgAdmin** for SQL, and the owner edits in pgAdmin's editor. Ops
  scripts must have no psql meta-commands; point him to the exact lines to
  change, each marked `<<< EDIT` on that line (he has twice edited the wrong
  line).
- Ops folders get distinct names and a status row in `ops/README.md`.
- One line of work at a time on this database: two branches migrating the same
  schema caused both production startup failures so far.
- At most **10** coding agents in parallel (raised from 3 on 9 October 2026),
  each on disjoint files or in its own worktree.
- The owner prefers not to set up a local test database. For prod data he runs
  read-only queries and pastes the results.

## How to test

Full guide: `TESTING.md` (throwaway DB scripts, CI, the complete command
list). The scripts and CI are not on `main` and are paused (Session state,
"Testing setup: paused"); use the manual throwaway-Postgres steps. The short
version:

- From the repo root: `go vet ./...` and `go test -race ./...` (`./...`
  also covers `internal/database`). Database tests need
  `SCOUT_TEST_DATABASE_URL` pointing at a throwaway Postgres named
  `scout_test` (`TestOpenScoutStoreSelection` needs that name), run with
  `-p 1`; without it they are skipped.
- Throwaway Postgres: `initdb` in the agent's own subdirectory of
  `<repo root>/.claude/worktrees/_scratch/` (absolute path; see `TESTING.md`;
  owner, 14 Oct 2026: no files outside the repo
  and his notes folder), its
  own port on 127.0.0.1, `pg_ctl stop` and delete the directory at the end.
  Never touch the Windows Postgres service on port 5432.
- `TestLatestPriceInterruptedLeavesRowUntouched` was flaky (about 1 run in 10)
  before aa60a3b (7 October); it has not been seen failing since. If it fails,
  rerun it once
  (`go test -race -p 1 -run '^TestLatestPriceInterruptedLeavesRowUntouched$' -count=10 .`)
  and report the result.
- On-chain code uses the fake chain in `onchain_test.go`; never call real nodes.
- `ml/`: `python -m pytest tests -q` with the ml venv.
- Go files are CRLF: check gofmt on LF copies.
- Page changes: check at 1280px and 390px, light and dark. DOM with
  `textContent` only, and keep the Content-Security-Policy (no inline script or
  style).
- Headless browser checks: use a throwaway `--user-data-dir` in the agent's
  subdirectory of the main checkout's `_scratch` folder (absolute path; see
  `TESTING.md`) and start Edge/Chrome with `--disable-sync --disable-extensions
  --no-first-run --no-default-browser-check` (plus Edge `--inprivate` if it
  works headless), so the test profile is never signed into the owner's
  Microsoft account and never loads his extensions (on 9 October a test
  profile synced his extensions, a wallet among them). Close the browser by
  PID and delete the profile directory afterwards.
- Ops SQL: run A/B/C/D against the throwaway Postgres before handing them over.

## Deploying

See `DEPLOY.md` (committed in 08b3a71) for the full steps. In short:
the owner commits and pushes a work branch from the PC, merges it into
`main` with a PR, and deploys `main` on the server (in the new checkout): `git checkout main && git pull`, then restarts the processes
that changed, from the repo root:

- `go run . -listen-only`
- `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run . -track`
  (latest-price pass on; `SCOUT_RPC_LOG_CHUNK` unset, default 200000).
  `SCOUT_LATEST_REFRESH_OLD=1h SCOUT_LATEST_BATCH=400` are in the prod
  `.env` (since 8 October). Start it from the checkout folder, so it reads
  that `.env`; a variable set in the shell wins over the file. Check the
  `latest prices on:` line after every restart.
- `go run . -web`. A reverse proxy in front must
  pass the `Host` header unchanged, or the same-origin check makes
  `POST /api/refresh` return 403.

After a tracker change: `-price-check` on 2–3 affected tokens before the full
run.

Checks after deploying `work/2026-10-08-ml-pool-web` (deployed 8 October in
4e7e049; the pool, feeds and latest-prices startup lines were seen; the
`waiting` count, listener and phone checks are still to confirm):

- Tracker startup line: "pool of at most 16 connections (default for 12
  tracker worker(s))".
- Expect many "stale" labels for the first hour or two while about 4,000 old
  calls are refreshed at 400 per cycle; the tracker's `latest prices:` line
  "N waiting" should fall and then stay low.
- The owner checks the new scrolling table on a phone, and that a non-rugged
  call older than 30 min shows "stale" when its price is old.

## Agent setup

- Redone by function on 12 October 2026 (PR #26, 57fdced; replacing the
  role-based `coder`,
  `ml-coder`, `react-coder`, `infra`, `tester`, `reviewer` and `researcher`).
  An agent exists only to isolate context, restrict tools or give fresh eyes.
  `.claude/agents/`:
  - `product-manager`: the main session (`"agent"` in
    `.claude/settings.json`); delegates to the five below. Tools (PR #30):
    `Agent(investigate, implement, simplify, verify, review), Read, Glob,
    Grep, Bash, Edit, Write`. Bash is for read-only git (status, log,
    diff, show, rev-parse, merge-base, branch --list, worktree list,
    fetch) plus `git worktree add -b work/...` to create worktrees for
    `implement`; Edit/Write only on `HANDOFF.md` and `.claude/skills/**`
    (in a worktree, never the main checkout). Never add, commit, push,
    stash, switch, reset, rebase or merge. The authoritative PM rules are
    in `.claude/skills/scout-product-manager/SKILL.md`.
  - `investigate`: read-only answers about code, data, model reports and
    outside sources; drafts plans and specs (`model: opus`).
  - `implement`: changes files in a worktree to the PM's spec, any area.
  - `simplify`: after `implement`, same worktree; simplifies the changed code
    without changing behaviour (no Write tool).
  - `verify`: runs and writes unit and integration tests in the implementer's
    worktree, test files only; paused (owner, 10 Oct; see Session state).
  - `review`: read-only critic of the uncommitted diff, PASS/FAIL verdict
    (`model: fable`).
  No subagent has the `Agent` tool; none sets `isolation` or `effort`
  (worktrees are chosen per call). Tools: `investigate` (Read, Grep, Glob,
  WebSearch, WebFetch, Skill) and `review` (Read, Grep, Glob, Bash, Skill)
  deny Write/Edit; `simplify` has Edit but not Write; `implement` and
  `verify` have Read, Write, Edit, Bash, Glob, Grep, Skill. Models:
  `investigate` on `opus`, `review` on `fable`, the others inherit the
  session model. Each subagent lists `skills: [scout-rules]`.
- Project rules live in skills (`.claude/skills/`): `scout-rules` (shared hard
  rules, reporting basics, code map; preloaded by every agent), and the area
  skills `scout-go`, `scout-web` (website, CSP, Charts), `scout-ml` and
  `scout-infra`, which the PM names in each brief. `scout-product-manager` is
  the PM's own skill. No React agent or skill (no React project exists).
- Testing rules for all agents: `TESTING.md` (repo root).
- Flow (PM skill "How to work"): `implement` → `simplify` (non-trivial code
  changes) → `verify` when a change touches `scoutanalytics.sql`,
  tracker/on-chain code, delivery/notify/re-check/re-ask, the web
  snapshot/API or `ml/`, once the testing setup is resumed → `review` before
  every commit (at most 3 fix rounds, then the owner decides).
- One writer at a time per worktree. At most 10 writing agents in parallel
  (owner's limit, raised 9 Oct 2026).
- `settings.json`: read-only git, `git fetch`, `git worktree add`,
  `node --check`, go vet/test/build, gofmt and the ml venv pytest
  (`ml/.venv`, or `.venv` from inside `ml/`) are allowed; secrets are denied in
  any folder; `git push`, `git commit`, `git add` and `git stash` are denied
  for both Bash and PowerShell (PR #30). Binaries,
  secrets, state, CSVs and the venv are in `.gitignore`.
- New or changed agents and skills load only after a Claude Code restart.
- Lessons:
  - Parallel writing agents must have disjoint files (parallel README edits
    got mixed before).
  - Usage limits cut agents off mid-task, so a restarted agent must first check
    `git status` and the files for partial work.
  - Each throwaway Postgres gets its own subdirectory of the main checkout's
    `_scratch` folder (absolute path; see `TESTING.md`)
    directory and port and is deleted afterwards.
  - On this PC git needs `safe.directory` (the owner added it globally for
    this repo); agents use `git -c safe.directory=* …`.
  - `git add -p` instructions must say exactly which hunks to take (the owner
    answered y to all, so every README hunk landed in 4707e77); give
    file-level splits when possible.
  - In Git Bash `grep -c $'\r'` reports 0 even for CRLF files; count CRs with
    `tr -cd '\r' | wc -c`.
  - In Git Bash, `sed -i` can strip carriage returns from CRLF files; prefer
    the Edit tool and recheck CRLF afterwards.
  - The owner merged the stacked branch directly (PR #1). A PR from a
    stacked branch includes the base branch's commits.
  - The Agent tool's `isolation: "worktree"` fails on this PC ("Could not
    read the repository git config to neutralize filter drivers", every
    time on 12 Oct; the repo has no filter drivers). Worktrees are created
    with `git worktree add` from the main checkout with an absolute target
    path (by the PM since PR #30), and agents are launched without
    isolation.
  - A relative `cd` in a commit hand-off ran git in the wrong worktree
    (12 Oct; the push sent only the old base commit). Hand-offs use an
    absolute `cd` and check `git log -1 --oneline` before the push.

## Reference

- Pons V2: factory 0x7eD598BcEf8bd9Edd8C97A195C6d13f40801EC7e
  (`TokenLaunched`, `PoolGraduated`); one bonding curve per token; graduates
  at 4.2 ETH to a Uniswap v4 pool behind hook
  0xE5e702641Ea86F4ae6cC3cDaeD2B886f976Be044. Pons V1 (factory
  0xA5aAb3F0c6EeadF30Ef1D3Eb997108E976351feB) uses Uniswap v3. Source:
  github.com/ponsdotdev/pons-labs.
- Robinhood stock tokens: addresses from
  `https://api.robinhood.com/rhj/prices/<SYMBOL>` (chain 4663; `chains.id` 21
  in the API database); feeds are Chainlink "Standard Proxy" addresses; the 33
  pairs are in `ops/2026-10-asset-chains-feeds/B_seed.sql`. HOODon
  (0xfb5b5778d45ae47f15323fb59b666c655174a79c) is not a Robinhood token; RDDT
  is official but has no feed.
- Token names are read with ERC-20 `name()` into
  `scout_call_tracking.token_name` (and `token_symbol_onchain`).
- One-off ops scripts and their status: `ops/README.md`.
