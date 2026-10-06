# Scout analytics: handoff to Claude Code

Written 5 October 2026 from the Cowork session that built this. Start Claude Code
in the repo root; `.claude/settings.json` makes the session the product manager,
which delegates to the `researcher` and `coder` agents in `.claude/agents/`.

First message to give it:

> Read telegrambot/scoutanalytics/HANDOFF.md and README.md, then build the
> items under "Next work" in order. Report after each one.

## State of the code

Branch `scout-call-model`. Everything below is committed except the "Latest %"
work, which is in the working folder (commit it first).

- Listener: reads @scoutrobinhood, tells real calls from "hit 3X" update posts
  (`postkind.go`), sends new tokens to @perceptor0xBot and @salpha_research_bot,
  delivers to the private group, records to Postgres.
- Tracker (`-track`): on-chain prices from Uniswap v2/v3/v4 pools, returns at
  1h/1d/3d/7d/30d, 5-minute and hourly candles, pre-call trading stats, token
  names from the contract's `name()`. Only the first real call of each token is
  tracked; later calls get status `repeat`. A latest-price pass keeps a current
  return per token (every 15 minutes under 30 days old, daily after).
- Website (`-web`, port 8090, no login, read-only): one row per token, served
  from an in-memory snapshot refreshed every 15 seconds. Sort by date, return,
  peak, latest; search; horizon buttons; Perceptor filter and column; legend.
  Front end is plain JavaScript in `frontend/` (no framework, no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. Not
  trained on real data yet: the tracker has to finish the history first.

## Rules the owner has set

- Never print, commit or copy `.env`, `scout.session.json`, `scoutanalytics_data/`.
- No new database tables without asking; extend existing ones. Every statement
  in `scoutanalytics.sql` must be safe to repeat (it runs at every start).
  Views are dropped and recreated at startup, dependents first.
- Website speed is the priority: requests are answered from the snapshot and
  must not query the database. Keep p95 under 10 ms; rerun the benchmark
  (`go test -run xxx -bench BenchmarkWebSnapshot`) after touching it.
- Only real calls are scanned, delivered and tracked; one row per token.
- The owner commits and pushes. Never commit to `main`.
- Say "tested locally" unless it ran on the prod server. Flag anything that
  makes the tracker redo history or adds ongoing node load before shipping it.
- One line of work at a time on this database: two branches migrating the same
  schema caused both production startup failures so far.

## How to test

- `go vet ./telegrambot/scoutanalytics` and
  `go test -race ./telegrambot/scoutanalytics` (110 tests). Database tests need
  `SCOUT_TEST_DATABASE_URL` pointing at a throwaway Postgres; without it they
  are skipped. On-chain code uses the fake chain in `onchain_test.go`.
- `python -m pytest tests -q` in `ml/` (27 tests).
- Page changes: check in a real browser at 1280px and 390px, light and dark.
  Build DOM with `textContent` only (token names come from arbitrary contracts)
  and keep the Content-Security-Policy (no inline script or style).

## Next work (requested, not started)

1. **Market cap columns.** Add "Call MC" (market cap at the call, from the post:
   `scout_call_metrics.called_at_mcap_usd`, falling back to `mcap_usd`) and
   "Latest MC" to the table, both sortable. The latest market cap is not stored;
   estimate it as call market cap × latest price ÷ call-time entry price
   (`latest_price_usd / entry_price_usd`), which assumes supply has not changed.
   Label it as an estimate in the legend. Show a dash when either input is
   missing or the call is not priced in USD.
2. **sAlpha report on the page.** The reply text is already stored
   (`scout_investigations`, tool code `salpha`, `report_text`, `report_url`); it
   arrives later than Perceptor's and sometimes not at all. Add a row detail
   pane: clicking a row expands it to show the Perceptor summary and the sAlpha
   report text (rendered as plain text). Load the text on demand from a new
   endpoint (`GET /api/call?id=`) so the list stays small; use the latest
   completed report per token, as the Perceptor column does. Add a small marker
   in the row when a sAlpha report exists.
3. **Push updates for new tokens.** When the listener records a new token and
   when its Perceptor or sAlpha report arrives, the open page should update
   without waiting for the next refresh and show a notice.
   - Transport: Server-Sent Events (`GET /api/events`) needs no new dependency
     and is enough for server-to-browser push. A WebSocket is only needed if the
     browser must send data back; the owner asked for "websocket or
     bidirectional", so confirm whether anything needs to go from the page to
     the server before adding a WebSocket library.
   - The website is a separate process from the listener. Have the listener
     `NOTIFY` a Postgres channel after it records a call or an investigation;
     the web process `LISTEN`s, refreshes its snapshot immediately, compares it
     with the previous one, and emits `call` and `report` events.
   - In the page: add the new row at the top when the current filters allow it,
     show an in-page notice with the token, verdict and a GMGN link, and offer
     an optional sound. Browser desktop notifications only work on HTTPS or
     localhost, so on a plain `http://<server address>` they will not appear; say
     so in the README.
   - Keep the events endpoint cheap: one goroutine per client, heartbeat
     comment every 25 seconds, cap the number of clients.
4. **Scatter plot** of call properties against return. Parked until the
   tracker has finished and the model report shows which properties matter.

## Already done (asked again recently)

- Token names are read with the ERC-20 `name()` call and stored in
  `scout_call_tracking.token_name` (and `token_symbol_onchain`). The tracker
  fills 200 tokens per cycle.

## Open questions for the owner

- Should `prior_calls`, `calls_prev_1h` and `calls_prev_24h` (model inputs)
  count real calls only? They still count update posts.
- "Show the expanded pane with the questions": unclear what this refers to.
- Add a `reviewer` agent and a deploy checklist?
- A staging copy of `assetdb` would let changes be run against real data
  before production.

## Deploying

PC: `git add telegrambot/scoutanalytics && git commit && git push`.
Server: `git pull`, then restart the three processes:
`go run ./telegrambot/scoutanalytics -listen-only`,
`… -track` (with `SCOUT_RPC_MAX_INFLIGHT=48 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=8`),
`… -web`.
