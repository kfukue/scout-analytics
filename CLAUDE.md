# Scout analytics: rules for Claude

Read `.claude/skills/scout-rules/SKILL.md` for the shared rules. The product
manager follows `.claude/skills/scout-product-manager/SKILL.md`.

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

## Never

- Never read out, print, commit or copy `.env`, `scout.session.json` or
  anything under `scoutanalytics_data/`.
- Never connect to the production database. Production data comes only from
  read-only SQL that the owner runs.
- Never commit or push to `main`.
