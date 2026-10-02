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

## Call data parsed from the post

Each @scoutrobinhood call is parsed into fields (stored in `scout_call_metrics` /
`scout_call_live_buys`, shown in the delivery header):

| Post line | Fields |
|---|---|
| `🚨 EARLY CALL — $TOKEN · robinhood` | `token_symbol`, `chain_name` |
| `💰 called at $45k` | `called_at_mcap_usd` |
| `🏛 DEX: Longxyz` | `dex` |
| `📈 Mcap: $52k` | `mcap_usd` |
| `💧 Liq: $20k \| 38%` | `liq_usd`, `liq_pct` |
| `🧾 Tax: B 0% \| S 0%` | `tax_buy_pct`, `tax_sell_pct` |
| `⏱ Age: 12m · 🚀 Longxyz` | `age_text`, `age_seconds`, `launchpad` |
| `👥 Holders: 150` | `holders` |
| `🔎 Proof: 3 elite + 5 good holding` | `proof_elite`, `proof_good` |
| `💎 $1.2k · 0x79f6…5c5d` / `✅ $350 · 0x12ab…9f00` (under "Live buys") | one `scout_call_live_buys` row each: `tier` elite/good, `amount_usd`, wallet; plus counts/totals per tier in `scout_call_metrics` |

Header line in the delivery:
```
📊 MCap $52k · Liq $20k (38%) · Tax 0/0% · Age 12m · Holders 150 · Proof 💎3 ✅5
Live buys: 💎 2 ($2k) · ✅ 2 ($446)
```
Fields missing from a post are stored as NULL. `-post <id>` prints the parsed data, so
you can check a post's parse before relying on it.

## Performance tracking (for building a prediction model)

For every call the scanner records how the token did afterwards, **straight from the chain**
(Robinhood Chain, your node at `SCOUT_RPC_URL`, default `http://localhost:8540`). A **full node
is enough**, as long as it serves old event logs (`eth_getLogs`); an archive node also works:

1. **Pool:** found from the token's own transfers around the call: the address it moved
   to/from most is checked on-chain and identified as a Uniswap **v2** pair, **v3** pool or a
   **v4** pool on the PoolManager (matched through the swaps in the same transactions).
   No indexer or factory address needed.
2. **Prices:** every trade, from the pool's events: v2 `Sync` (reserves), v3/v4 `Swap`
   (`sqrtPriceX96`). So peak and drawdown are exact, not candle approximations.
3. **USD:** the pool price is in the pool's other asset. It's converted with a **Chainlink
   feed** as of that block, `$1` for stablecoins (USDG), and for ETH the WETH/USDG v3 pool if
   no ETH feed is configured. On an archive node the value is read from contract state
   (`latestRoundData()` / `slot0()` at the block); on a **full node** (no historical state) it
   comes from event logs instead: the feed aggregator's last `AnswerUpdated` and the pool's
   last `Swap` at or before the block. This is detected automatically.
4. **Each horizon** (`1h, 1d, 3d, 7d, 30d`): price, **return %**, **max gain %**, **max drawdown %**
   → `scout_call_returns`. Swaps are scanned once: progress is kept per call, each check
   only reads the new block range.
5. After the last horizon: **rugged** = price < 5% of entry, or pool liquidity < $500 (v2/v3).

**ETH price from your Ethereum archive node (recommended):** ETH/USD is the same on every
chain, so it can be read from Chainlink's ETH/USD feed on Ethereum mainnet. Point the scanner
at an Ethereum **archive** node (e.g. Erigon):

```
SCOUT_MAINNET_RPC_URL=http://localhost:8545
```
For each Robinhood Chain block it takes the block's timestamp, finds the Ethereum block at
that time, and reads `latestRoundData()` of the feed there (`0x5f4eC3Df…5b8419`, built in).
The Robinhood node then only has to serve logs (a full node is enough). Other mainnet feeds
can be mapped to a paired asset with `SCOUT_MAINNET_CHAINLINK_FEEDS=0xQuoteTokenOnRobinhood=0xFeedOnEthereum`.

Order of sources for a paired asset's USD price: stablecoin ($1) → mainnet Chainlink feed
(if `SCOUT_MAINNET_RPC_URL` is set) → Chainlink feed on Robinhood Chain → WETH/USDG pool (ETH only).

**Long.xyz tokens trade against Stock Tokens** (NVDA, TSLA, …), so add their Chainlink feeds:

```
SCOUT_CHAINLINK_FEEDS=eth=0xEthUsdFeed,0xNvdaToken=0xNvdaFeed,0xTslaToken=0xTslaFeed
```
A pair whose quote asset has no feed is still tracked, **in that asset's units**
(`price_unit` = e.g. `TSLA` instead of `usd`); the returns are then relative to the stock token.
Chainlink stock feeds run 24/5, so weekend conversions use Friday's price. Max gain/drawdown
are converted with the quote asset's USD price at the horizon (not at each trade).

**Check it against your node first:**
```bash
./scoutanalytics -price-check 0xTokenCA -price-at 6h     # or -price-at 2026-10-01T14:30:00Z
```
prints the latest block, the block at the call time, the pool it found (v2/v3/v4, paired
asset), the entry price, the USD conversion (or which feed to add) and the move since.

### Backfill past calls (dataset without waiting 30 days)

```bash
./scoutanalytics -backfill                       # import every past call in @scoutrobinhood
./scoutanalytics -backfill -backfill-from 9000   # or only posts 9000…latest
```
Backfilled calls are parsed and stored (status `backfill`, no bot scans, Perceptor verdict
empty) and queued for price tracking at lower priority than live calls.

**Safe to re-run** (e.g. after an interruption, or with overlapping ranges). A call is
identified by channel + post + CA (CA compared case-insensitively), so a re-run updates
instead of duplicating:

| Table | On re-run |
|---|---|
| `scout_calls` | same row; only the post text/links are refreshed, status kept (a live call stays `scanned`) |
| `scout_call_metrics` | same row, re-parsed |
| `scout_call_live_buys` | replaced for that call (never appended) |
| `scout_call_tracking` | untouched: entry price, schedule and progress are kept |
| `scout_call_returns` | untouched by backfill; one row per call + horizon |

The summary shows it: `backfill: 500 posts read, 180 calls, 182 CAs (0 new, 182 already recorded)`. The running listener works through them, or run `./scoutanalytics -track` as a separate process.

### Export the training dataset

```bash
./scoutanalytics -export-dataset calls.csv
```
One row per call (`scout_call_dataset_v`): **features known at call time** (MCap, Liq, Liq %,
Tax, Age, Holders, Proof elite/good, live-buy counts/$ per tier, Perceptor verdict, DEX,
launchpad) + **outcomes** (`ret_*`, `max_gain_*`, `max_dd_*` for 1h/1d/3d/7d/30d, `rugged`).
Only use the feature columns as model inputs; everything about the future is an outcome.

| Setting | Default | |
|---|---|---|
| `SCOUT_TRACK_PERFORMANCE` | `true` | turn tracking off |
| `SCOUT_PERF_HORIZONS` | `1h,1d,3d,7d,30d` | up to 40d |
| `SCOUT_PRICE_SOURCE` | `onchain` | `gecko` = GeckoTerminal API instead (candles, ~10 req/min, no node needed) |
| `SCOUT_RPC_URL` | `http://localhost:8540` | Robinhood Chain node (full or archive; must serve historical logs) |
| `SCOUT_PRICE_LOOKBACK_BLOCKS` | `8640000` | full node: how far back (~10 days) to look for the last feed update / ETH swap |
| `SCOUT_RPC_RPS` | `50` | max RPC requests per second |
| `SCOUT_RPC_LOG_CHUNK` | `200000` | blocks per `eth_getLogs`; halved automatically if the node refuses a range |
| `SCOUT_MAINNET_RPC_URL` | none | Ethereum mainnet **archive** node; enables ETH/USD from mainnet Chainlink |
| `SCOUT_MAINNET_CHAINLINK_FEEDS` | `eth=` ETH/USD feed | extra `token=feedOnEthereum` mappings |
| `SCOUT_MAINNET_RPC_RPS` | `50` | max requests per second to the Ethereum node |
| `SCOUT_CHAINLINK_FEEDS` | none | feeds **on Robinhood Chain**: `token=feed,…`; use `eth` for WETH/native ETH |
| `SCOUT_STABLES` | USDG | tokens worth $1 |
| `SCOUT_WETH`, `SCOUT_V4_POOL_MANAGER`, `SCOUT_ETH_USD_POOL` | Robinhood Chain addresses | override if needed |
| `SCOUT_DISCOVERY_BLOCKS` | `18000` | ± blocks around the call searched for the token's transfers (widened automatically) |
| `SCOUT_RUG_LIQ_USD` | `500` | liquidity below this = rugged |
| `SCOUT_TRACK_INTERVAL` | `1m` | how often due checks are processed |
| `SCOUT_TRACK_WORKERS` | `4` | calls tracked at the same time (on-chain source). Raise it while the node keeps up; all workers share `SCOUT_RPC_RPS` |
| `SCOUT_RPC_LOG_CACHE` | `300000` | swap logs kept in memory so repeat calls of a token are not scanned twice (`0` = off) |

Other commands: `-track` (tracker only, forever, no Telegram), `-track-once` (process what's due and exit).

To split the work over two processes (so the tracker can be restarted without
interrupting the listener), run the listener with `-listen-only` and the tracker
with `-track`. New calls are still queued for tracking by the listener.

```
./scoutanalytics -listen-only   # terminal 1: scan + deliver new calls
./scoutanalytics -track         # terminal 2: compute performance
```

### Data for the prediction model

The on-chain tracker also stores what a model needs (state version 2):

| What | Where | Notes |
|---|---|---|
| Dollar prices for every pair | `scout_call_tracking.price_unit` | A quote asset without a Chainlink feed (VIRTUAL, a stock token) is priced from its own WETH or stablecoin pool. It stays in quote units only if no such pool exists. |
| Realistic entry | `scout_call_tracking.entry_late_price_usd`, `scout_call_returns.*_late_pct` | The pool price `SCOUT_ENTRY_DELAY` (default `60s`) after the post, and return / peak / drawdown measured from it. |
| Price path | `scout_call_candles` | 5-minute candles for the first 24 hours, hourly candles for the whole window. Only buckets with trades. |
| Trading before the call | `scout_call_precall` | Swaps, buys, sells, volume and price change in the 5, 15 and 60 minutes before the post. |
| Peaks and lows | `scout_call_returns.max_gain_pct`, `max_drawdown_pct` | Each trade is valued at its own hour's ETH (or quote asset) price, not the price at the horizon. |

Calls tracked by an earlier version are queued again automatically at startup and
tracked from scratch (their old results stay until replaced). Expect the tracker
to work through the whole history once more after upgrading.

`scout_call_dataset_v` now has one row per call with all of the above plus
repeat-call and channel-activity columns (`prior_calls`, `calls_prev_1h`, …).
Training, the report and the scoring service live in [`ml/`](ml/README.md).

### Scoring new calls

Off by default. With `SCOUT_MODEL_URL` set (for example `http://127.0.0.1:8601`,
the service in `ml/serve.py`), the listener sends each new call's dataset row to
the service, stores the scores in `scout_call_predictions` and adds one line to
the delivered message:

```
Model 20261002-1530 · short: runner 31%, collapse 44% · 3-day: runner 22% · medium: runner 18% · long: up at 30d 6%
```

Scores never filter deliveries. If the service is down or slower than
`SCOUT_MODEL_TIMEOUT` (default `5s`), the call is delivered without the line.
`scout_call_predictions_v` shows each score next to the real outcome.

### What the tracker logs

```
tracking: 37 call(s) due now
call 10126 [1/37]: 0x129b…, posted 2026-09-28 14:02 (70h ago), status pending
call 10126 [1/37]: pool found: uniswap-v3 0x…, paired with WETH (entry block 21300412)
call 10126 [1/37]: entry price $0.0031 (1.03e-06 WETH × $3010, Chainlink on Ethereum mainnet)
call 10126 [1/37]: +1h → 0.0052 (+67.7%), peak +120.4%, low -8.1%
call 10126 [1/37]: scanning blocks 21726610 → 22164412: 46% (at 21926609, 12 events so far)
call 10126 [1/37]: tracking in 14s, 212 RPC requests — next check 2026-10-01 14:12
tracking: processed 37 call(s) — pending 112, tracking 37, done 4; more due now
tracking: idle — tracking 149, done 4; next check in 42m10s
```

Long block scans print a progress line every few seconds, and a status line is
printed after every cycle (every `SCOUT_TRACK_INTERVAL`, even when idle), so a
quiet terminal for more than a minute means something is stuck.

**Ctrl+C is safe.** A call that is interrupted mid-scan is left exactly as it was
and picked up again on the next start. `gave_up` is only used when a call is past
its last horizon and still has no pool or no trades. Calls an older version
marked `gave_up`/`error` with `context canceled` are reset automatically at startup.

## Where clean reports go

| Setting | Result |
|---|---|
| default (`SCOUT_NOTIFY_PEER=me`) | Your **Saved Messages** (no push notification) |
| `SCOUT_NOTIFY_PEER=https://t.me/+AbCdEf…` | A private group/channel **by its invite link**: exact, joins it if needed. **Recommended.** |
| `SCOUT_NOTIFY_PEER="scout analytics"` | A private group/channel by its title (case-insensitive; groups you've left are ignored) |
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

Check it with `./scoutanalytics -test-notify` (sends one test message and exits).
If Telegram later rejects the chat (`PEER_ID_INVALID`, e.g. the group was upgraded to a
supergroup), the scanner looks `SCOUT_NOTIFY_PEER` up again and retries once.

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
| `scout_call_metrics` | call (1:1) | MCap, Liq, Liq %, Tax buy/sell, Age, launchpad, Holders, Proof elite/good, live-buy counts and $ per tier, `parsed` (JSONB) |
| `scout_call_live_buys` | live-buy line of a call | `call_id`, `position`, `tier` (elite/good), `amount_usd`, `wallet_display` |
| `scout_calls_v` (view) | call + its parsed data | for ad-hoc queries |
| `scout_call_tracking` | call (1:1) | pool, entry price (+ source), status, next check, current liquidity, `rugged` |
| `scout_call_returns` | call × horizon | `horizon`, `price_usd`, `return_pct`, `max_gain_pct`, `max_drawdown_pct`, `last_trade_at` |
| `scout_call_dataset_v` (view) | call | features + pivoted outcomes; what `-export-dataset` writes |
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

-- calls with their post data and Perceptor verdict
SELECT c.message_date, c.token_symbol, c.mcap_usd, c.liq_usd, c.holders, c.proof_elite, c.proof_good,
       v.verdict_level AS perceptor
FROM scout_calls_v c LEFT JOIN scout_investigations_v v ON v.call_id = c.call_id AND v.tool = 'perceptor'
ORDER BY c.message_date DESC LIMIT 50;

-- elite vs good live-buy volume per call
SELECT call_id, tier, count(*) buys, sum(amount_usd) usd FROM scout_call_live_buys GROUP BY 1,2 ORDER BY 1 DESC;

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
