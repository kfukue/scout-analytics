# scoutanalytics

Watches **@scoutrobinhood**, pulls every contract address (CA) out of new posts and
sends it to every **investigation tool** in parallel:

| Tool | Bot | Sent | Role |
|---|---|---|---|
| `perceptor` | @perceptor0xBot | `/scan <CA>` | **gate**: its verdict decides delivery |
| `salpha` | @salpha_research_bot | `<CA>` (bare CA, as the bot asks) | report attached |

When Perceptor rates the CA **No red flags found** or **Caution**, you get a summary
plus **both reports** forwarded to a separate chat (Red flags are skipped). Every
call, every tool's report and every delivery can be recorded in Postgres.

It logs in as your Telegram *user* account (a regular bot can't read other
channels or talk to other bots). It reuses `API_ID`, `API_HASH`, `PHONE` from the
repo-root `.env`, but keeps its own session file so it won't fight with the
existing telegrambot session.

## Run

From the repo root:

```bash
# 1. one CA — prints every tool's report + verdict and delivers it (if the gate allows)
go run ./telegrambot/scoutanalytics -scan 0xYourTestCA
#    print only, don't send:
go run ./telegrambot/scoutanalytics -scan 0xYourTestCA -no-deliver

# 1b. process a specific call post (prints what it found in the post, then investigates + delivers)
go run ./telegrambot/scoutanalytics -post 10002          # or -post https://t.me/scoutrobinhood/10002

# 2. watch the channel but deliver nothing (verdicts go to the log)
go run ./telegrambot/scoutanalytics -dry-run

# 3. real thing
go run ./telegrambot/scoutanalytics
```

First run asks for the Telegram login code (and uses `TG_PASSWORD` if you have 2FA).
Make sure you've pressed **Start** on **@perceptor0xBot and @salpha_research_bot** once from this account.

## How calls are picked up

- **Live updates** for new posts, plus **edited posts** (a call posted first and the CA added later).
- **Polling backup** every `SCOUT_POLL_INTERVAL` (default `20s`, `0` = off): Telegram doesn't
  always push every post of a big channel to user accounts, so the scanner also asks for
  posts newer than the last one it polled. Each (post, CA) is handled once.
- The CA is read from the post text, text links, link previews **and inline buttons**
  (Chart/Buy/"Copy CA"). Links to **wallet or transaction pages** (`/address/`, `/tx/`,
  `/profile/`, …, e.g. the "Live buys" wallets) are ignored, so wallets aren't scanned.
- Every post is logged: `post 10002: queued 0x…` or `post 10002: no CA found (… links/buttons)`.
  If a post you expected isn't in the log at all, it wasn't received; if it says "no CA found",
  run `-post <id>` to see exactly what the scanner saw.

## Where clean reports go

| Setting | Result |
|---|---|
| default (`SCOUT_NOTIFY_PEER=me`) | Your **Saved Messages** (no push notification) |
| `SCOUT_NOTIFY_PEER="scout analytics"` | A private group/channel **by its title** (case-insensitive). **Recommended.** |
| `SCOUT_NOTIFY_PEER=-1001234567890` or `@username` | A group/channel by id or public username |
| `SCOUT_NOTIFY_BOT_TOKEN` + `SCOUT_NOTIFY_CHAT_ID` | Sent by your own bot via Bot API (text copy, push notifications) |

Each delivery, in order:

1. a summary header (below)
2. the **original call** post from @scoutrobinhood, forwarded (a text copy + link if the channel blocks forwarding)
3. each tool's forwarded report (formatting, links, images and files kept)

```
🟡 $DARKCOMP Caution
CA: 0x…
• Perceptor: 🟡 Caution
  https://www.perceptor.info/r/…
• sAlpha: report attached
Source: https://t.me/scoutrobinhood/1234
```

Not sure of the name or id? `go run ./telegrambot/scoutanalytics -list-chats` prints
all your groups and channels with their ids. On startup the log shows where reports
will go, e.g. `reports will be delivered to supergroup "scout analytics" (id 1234567890)`.

> `-scan` delivers by default (like the listener). Add `-no-deliver` to only print.
> It also looks up the newest @scoutrobinhood post mentioning that CA and includes it as the original call.

## Optional .env settings

```
SCOUT_SESSION_FILE=scout.session.json
SCOUT_SOURCE_CHANNEL=scoutrobinhood
SCOUT_NOTIFY_PEER=me
SCOUT_DELIVER_LEVELS=clean,caution # clean | caution | red_flags (comma list)
SCOUT_CHAINS=evm,solana            # which CA formats to extract
SCOUT_RED_FLAG_MARKERS=🚩,⚠️,red flag,warning,honeypot,...   # comma list, case-insensitive
SCOUT_SAFE_PHRASES=no red flags,honeypot: no,...            # stripped before matching
SCOUT_SCAN_GAP=3s        # pause between CAs
SCOUT_STATE_DIR=scoutanalytics_data
```

## Investigation tools

```
SCOUT_TOOLS=perceptor,salpha        # run in parallel for every CA, in this order
```

Per-tool overrides, `SCOUT_TOOL_<CODE>_…` (defaults shown):

| Setting | perceptor | salpha | meaning |
|---|---|---|---|
| `_BOT` | perceptor0xBot | salpha_research_bot | bot username |
| `_COMMAND` | `/scan {ca}` | `{ca}` | text sent; `{ca}` = the CA |
| `_GATE` | true | false | its verdict must be in `SCOUT_DELIVER_LEVELS` to deliver |
| `_ATTACH` | true | true | forward its report in the delivery |
| `_PARSER` | perceptor | text | how the verdict is read |
| `_TIMEOUT` | 60s | 90s | wait for the first reply |
| `_SETTLE` | 6s | 15s | quiet time after the last reply/edit = report finished |
| `_MAX_WAIT` | 180s | 300s | hard cap per attempt (incl. time spent on "Scanning…") |
| `_DONE_REGEX` | verdict / perceptor.info link | none | the final report must match this (text or links); `none` to disable |
| `_PROGRESS_REGEX` | built-in | built-in | placeholder replies to ignore ("Scanning…", "Analyzing…") |
| `_MIN_INTERVAL` | 2m5s | 0 | minimum gap between two requests to that bot (pacing) |
| `_MAX_RETRIES` | 3 | 3 | retries after a "try again in N s" reply |
| `_RATE_LIMIT_BUFFER` | 3s | 3s | extra wait on top of the bot's countdown |
| `_MAX_RATE_WAIT` | 10m | 10m | cap on one rate-limit wait |
| `_RATE_LIMIT_REGEX` | built-in | built-in | custom detector; group 1 = number, group 2 = unit (s/m) |

### Waiting for the real report

Bots often answer with a placeholder first, e.g. Perceptor's
`Scanning 0x0ad7…a5b0 on Robinhood Chain…`, and send or edit in the report later.
Placeholders ("Scanning/Analyzing/Researching…" text, loading stickers/GIFs) are ignored:
the settle timer only starts once a real report is there. For Perceptor, "real" means
it contains the verdict or the perceptor.info link. Placeholders are not stored or
forwarded. If the report never finishes within `_MAX_WAIT`, the result is `timeout`,
with the last placeholder in the error.

### Rate limits

Perceptor allows one scan every 2 minutes and otherwise replies
`One scan every 2 minutes. You can scan again in 63 s`. For every tool:

1. **Pacing:** requests to a bot are spaced by `_MIN_INTERVAL` (perceptor: 2m5s), so the limit is rarely hit.
2. **Retry:** if a short reply says "…again in N s/min" (or "wait N s", "retry in N min"), the scanner
   waits N (+3s buffer) and sends the same request again, up to `_MAX_RETRIES` times.
   The rate-limit notice is discarded; only the real report is kept and forwarded.
3. If it's still rate-limited after the retries, the result is `rate_limited`. For a gate tool
   (perceptor) that CA is **not delivered**, and the CA stays marked as seen.

Attempts and waits are stored in `scout_investigations.details`
(`{"attempts": 2, "rate_limit_waits_s": [66]}`). Since CAs are processed one at a
time, a burst of calls queues up at about 2 minutes per CA.

**Add another bot with no code change**, e.g.:

```
SCOUT_TOOLS=perceptor,salpha,rugcheck
SCOUT_TOOL_RUGCHECK_BOT=some_rug_bot
SCOUT_TOOL_RUGCHECK_COMMAND=/check {ca}
```

It's registered in `scout_investigation_tools` automatically on startup.
Delivery rule: every gate tool must return a report with an allowed verdict. A
non-gate tool that fails or times out doesn't block delivery; the header says
"no report (timeout)". With no gate tool at all, every CA with a report is delivered.
(Old `SCOUT_SCAN_BOT`/`SCOUT_SCAN_COMMAND`/`SCOUT_SCAN_TIMEOUT`/`SCOUT_SETTLE`/`SCOUT_MAX_WAIT` still work for perceptor.)

## How it decides "warning / red flag"

Perceptor gives every report one of three verdicts (shown in the report page title):

| Perceptor verdict | Level | Delivered? |
|---|---|---|
| `$ANYR: No red flags found` | `clean` | **yes** |
| `$DARKCOMP: Caution` | `caution` | **yes** (🟡) |
| `$NH: Red flags` (e.g. "Top 10 hold 40%; Liquidity PULLED") | `red_flags` | no |

1. The bot's reply is searched (text, links, link preview, inline buttons) for a
   `perceptor.info/r/<id>` or `?investigation=<id>` link. The public report page is
   fetched and its title verdict is used. That's the authoritative check.
2. If there's no link, or the page can't be fetched, it falls back to the message
   text: "no red flags" → clean; "caution"/⚠️/"warning" → caution; "red flag"/🚩 → red_flags.
3. If there's still no verdict (e.g. "token not found", rate-limit reply) → `unknown`, not delivered.

To get only No-red-flags reports again: `SCOUT_DELIVER_LEVELS=clean`.

sAlpha (and any tool using the `text` parser) gets a best-effort keyword verdict,
stored in the DB for reference; it doesn't affect delivery unless you make it a gate.

## Recording to a SQL database (Postgres)

Every call, report and delivery is recorded **in the same database your API uses**. The
scanner calls the repo's `database.SetupDatabase()`, so it uses the same settings from
`.env`: `DB_USER`, `DB_PASS`, `DB_NAME_DEV`, `APP_ENV`, and `GETH_HOST_PATH` (LOCAL_GETH) or
`HOST_SECRET_PATH` + `SSL_CERT_FILE_PATH` (Cloud SQL). Nothing extra to configure.

The startup log confirms where it writes, e.g.
`recording to SQL via database.SetupDatabase (DB_USER, APP_ENV=…) → database "assetdb", schema "public", user "…"`.
The tables are created in that database's `public` schema on first start.

```
SCOUT_DB=repo                # default: the repo's database package
# SCOUT_DB=off               # run without recording
# SCOUT_DATABASE_URL="host=… port=5432 user=… password=… dbname=… sslmode=disable"   # optional override
SCOUT_DB_AUTO_MIGRATE=true   # creates/upgrades the tables on startup; false = run scoutanalytics.sql yourself
```

```
scout_calls ──< scout_investigations >── scout_investigation_tools
     │                  │
     └──< scout_deliveries ──< scout_delivery_investigations
```

| Table | One row per | Key columns |
|---|---|---|
| `scout_investigation_tools` | tool (bot) | `code`, `bot_username`, `command_template`, `parser`, `is_gate`, `is_active` |
| `scout_calls` | CA found in a @scoutrobinhood post | `message_id`, `message_date`, `message_text`, `urls`, `contract_address`, `chain`, `status` (`queued` → `scanned`/`failed`, or `duplicate`/`dropped`) |
| `scout_investigations` | (CA, tool) request | `call_id`, `tool_id`, `request_text`, `requested_at`, `completed_at`, `status` (`completed`/`failed`/`timeout`/`rate_limited`), `bot_message_ids`, `report_text`, `report_urls`, `report_url`, `external_id`, `verdict_level`, `verdict_label`, `ticker`, `verdict_summary`, `details` (JSONB: attempts, rate-limit waits, files, photos, buttons), `error` |
| `scout_deliveries` | bundle sent to you (or failed attempt) | `call_id`, `target`, `status` (`sent`/`failed`), `header_text`, `delivered_at`, `error` |
| `scout_delivery_investigations` | report attached to a delivery | `delivery_id`, `investigation_id` |
| `scout_investigations_v` (view) | investigation + tool + source post + `delivered` flag | handy for ad-hoc queries |

Adding a tool is a new row in `scout_investigation_tools`, never a schema change.
Tool-specific data goes in `details` (JSONB).

**Upgrading from the earlier single-tool table:** if `scout_scan_reports` exists, its rows
are copied once into `scout_investigations` (tool `perceptor`) and `scout_deliveries`.
The old table is left in place, so drop it when you're happy.

Schema: `scoutanalytics.sql`. Models: `scout_models.go`. Data access (`ScoutStore` in
`scout_models.data.go`): `UpsertInvestigationTool`, `SetActiveInvestigationTools`,
`SelectInvestigationTools`, `InsertScoutCall`, `UpdateScoutCallStatus`, `GetScoutCall`,
`SelectScoutCalls`, `InsertScoutInvestigation`, `GetScoutInvestigation`,
`SelectScoutInvestigations` (filter by CA, call, tool codes, verdicts, statuses, since),
`InsertScoutDelivery` (transactional, with attached reports), `SelectScoutDeliveries`.
Manual `-scan` runs are recorded with `call_id = NULL`. A database error is logged
but never stops scanning.

Example queries:

```sql
-- latest calls with every tool's verdict side by side
SELECT c.message_date, c.contract_address,
       max(v.verdict_level) FILTER (WHERE v.tool = 'perceptor') AS perceptor,
       max(v.status)        FILTER (WHERE v.tool = 'salpha')    AS salpha,
       bool_or(v.delivered) AS delivered
FROM scout_calls c JOIN scout_investigations_v v ON v.call_id = c.id
GROUP BY c.id ORDER BY c.message_date DESC LIMIT 50;

-- sAlpha reports for CAs Perceptor passed
SELECT s.contract_address, s.report_text
FROM scout_investigations_v s JOIN scout_investigations_v p ON p.call_id = s.call_id
WHERE s.tool = 'salpha' AND p.tool = 'perceptor' AND p.verdict_level IN ('clean','caution');

-- tool reliability (timeouts / failures) per day
SELECT date_trunc('day', requested_at) d, tool, status, count(*)
FROM scout_investigations_v GROUP BY 1,2,3 ORDER BY 1 DESC, 2;
```

DB integration tests (use a THROWAWAY database; they drop and recreate the scout_* tables):

```
SCOUT_TEST_DATABASE_URL=postgres://postgres@localhost:5432/scout_test?sslmode=disable go test ./telegrambot/scoutanalytics
```

## State / logs

- `scoutanalytics_data/seen_cas.json` — CAs already investigated (never re-run; delete an entry to rerun)
- `scoutanalytics_data/scans.jsonl` — every CA: each tool's status, verdict and report text, delivered?

CAs are processed one at a time; within a CA all tools run in parallel (replies are
matched to each bot, so they can't get mixed up). The client reconnects with
backoff if the connection drops. Ctrl+C to stop.
