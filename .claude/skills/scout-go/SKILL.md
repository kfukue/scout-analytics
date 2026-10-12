---
name: scout-go
description: Rules for Go code in scout analytics (conventions, schema, SCOUT_* config, Go practices, Go testing, line endings).
---

# Scout analytics: Go

## Conventions

- Go: package `main` at the repo root. Storage follows the
  repository pattern already there (a model struct plus functions in
  `scout_models.data.go`, pgx, no ORM). Run `gofmt` on what you touch.
- Schema: `scoutanalytics.sql` runs at every startup, so every statement must be
  safe to repeat (`IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`). Extend existing
  tables rather than adding new ones unless the task says otherwise.
- Configuration is read from environment variables named `SCOUT_*`; document any
  new one in `README.md`.

## Go practices

1. Wrap errors with `fmt.Errorf("doing x: %w", err)`; compare with
   `errors.Is`/`errors.As`, never by string. Handle each error once: log it or
   return it, not both.
   ([Uber Go guide](https://github.com/uber-go/guide/blob/master/style.md))
2. Never discard an error with `_` without a comment saying why.
   ([Go Code Review Comments](https://go.dev/wiki/CodeReviewComments#handle-errors))
3. `ctx context.Context` is the first parameter; pass it into every pgx call and
   RPC; never store it in a struct.
   ([CRC "Contexts"](https://go.dev/wiki/CodeReviewComments#contexts))
4. pgx: `defer rows.Close()`, check `rows.Err()` after the loop, `$n`
   parameters only (never SQL built with `fmt`), transactions with
   `defer tx.Rollback(ctx)` and then `Commit`.
   ([pgx v5](https://pkg.go.dev/github.com/jackc/pgx/v5))
5. Every goroutine has an owner and a stop path (context or close) and is
   waited for (`sync.WaitGroup` or
   [errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)); no
   fire-and-forget.
   ([CRC "Goroutine Lifetimes"](https://go.dev/wiki/CodeReviewComments#goroutine-lifetimes); Uber guide)
6. Synchronous APIs by default; let callers add concurrency.
   ([CRC "Synchronous Functions"](https://go.dev/wiki/CodeReviewComments#synchronous-functions))
7. Table-driven tests with `t.Run`; failure messages say `got X, want Y` and
   the inputs; use `t.Helper()` and `t.Cleanup`.
   ([CRC "Useful Test Failures"](https://go.dev/wiki/CodeReviewComments#useful-test-failures))
8. Guard shared state with a mutex or channels; never copy a struct that holds
   a lock (`go vet`
   [copylocks](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/copylock)).
9. Error strings are lowercase with no trailing punctuation; return early
   instead of `else` after a `return`.
   ([CRC "Error Strings"](https://go.dev/wiki/CodeReviewComments#error-strings),
   ["Indent Error Flow"](https://go.dev/wiki/CodeReviewComments#indent-error-flow))
10. No new dependency without saying so in the report; prefer the standard
    library ([`log/slog`](https://pkg.go.dev/log/slog), `slices`, `maps`).

References: [Google Go style](https://google.github.io/styleguide/go/);
[Effective Go](https://go.dev/doc/effective_go) (background only; it predates
generics and modules).

## Testing (Go)

Full guide: `TESTING.md` (layers, throwaway Postgres, commands, reporting).

- Go: `go vet ./...` and
  `go test -race ./...` (`./...` also covers `internal/database`). Database
  tests only run when
  `SCOUT_TEST_DATABASE_URL` points at a throwaway Postgres database (named
  `scout_test`, run with `-p 1`; see `TESTING.md` "Throwaway Postgres");
  without it they are skipped, so say which tests actually ran. Stop and
  delete the test database at the end and confirm both in your report.
- Use the fake chain in `onchain_test.go` for on-chain code; never call the
  real nodes from tests.
- `TestLatestPriceInterruptedLeavesRowUntouched` was flaky (about 1 run in 10)
  before aa60a3b (7 October); it has not been seen failing since. If it fails,
  rerun it once
  (`go test -race -p 1 -run '^TestLatestPriceInterruptedLeavesRowUntouched$' -count=10 .`)
  and report the result.
- Line endings: the Go files are CRLF, so plain `gofmt -l` lists almost every
  file. Check gofmt on LF copies, and keep CRLF line endings in the files you
  edit.
