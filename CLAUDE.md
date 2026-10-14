# Scout analytics: rules for Claude

## Start here

- Current state: the top section of `HANDOFF.md`.
- Rules: `.claude/skills/scout-rules/SKILL.md` (every agent);
  `.claude/skills/scout-product-manager/SKILL.md` (the product manager).

## Pull requests target `main` (owner, 14 Oct 2026)

- Every PR sent to the owner has base `main` (`gh pr create --base main`),
  unless the owner explicitly names another branch for that PR.
- Never open a PR whose base is another work branch unless the owner named
  it. On 14 Oct 2026, #47, #48 and #49 were stacked on other branches and
  merged into those branches instead of `main`. They reached `main` only
  through #50 and #51.
- When a change is split into several small PRs that depend on each other:
  - Default: open the next PR only after the previous one has merged, on a
    branch started from the updated `main`. The owner then reviews only the
    new change.
  - Only if waiting would block work: open it early, on a branch started
    from the previous work branch, with base `main`, and say in the PR body
    which PR to merge first. Until that one merges, GitHub shows its changes
    in this PR too. This works because the owner merges with merge commits;
    it does not work with squash merges.
- Before sending a PR link, check its base:
  `gh pr view <N> --json baseRefName` must say `main` (or the branch the
  owner named). If not, fix it with `gh pr edit <N> --base main` first.

## Database changes

Every change to the database structure or data ships with a script to run
and a rollback script: see "Database changes: run script and rollback" in
`.claude/skills/scout-rules/SKILL.md`.

## Never (safety net; full rules in scout-rules)

- Never read out, print, commit or copy `.env`, `scout.session.json` or
  anything under `scoutanalytics_data/`.
- Never connect to the production database. Production data comes only from
  read-only SQL that the owner runs.
- Never commit or push to `main`.
- Never use the Windows Postgres on port 5432 for tests; tests use only a
  throwaway `scout_test` database on another port (`TESTING.md`).
- Never run the listener, `-backfill` or `-track` against a real database.
- Never change `onchainStateVersion` or anything that makes the tracker redo
  history (days of node time) unless the task explicitly says so.
- Never run index-writing git in the main checkout (the product manager may
  run `git worktree add` and `git fetch` there); work in `.claude/worktrees/`.
