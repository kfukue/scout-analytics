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

### Two kinds of posts: calls and updates

The channel posts two kinds of messages that carry a contract address:

| Kind | Looks like | What happens |
|---|---|---|
| `call` | `🚨 EARLY CALL — $AUTOPILOT · robinhood`, then pool info, token info, proof, live buys | investigated by the bots, delivered, tracked |
| `update` | one line about an **earlier** call: `⚡ $MURKLE hit 3X called $21k → $63k peak since the call · dyor` | recorded only |

**Only real calls are investigated, delivered and tracked.** An update post is stored in
`scout_calls` with `post_kind = 'update'` and `status = 'update'`, and that is all: it is not
sent to the investigation bots, nothing is delivered, it gets no `scout_call_metrics` and no
`scout_call_tracking` row, and it does not use up the token's one investigation (a real call
of that token that arrives later is still scanned). The log says
`post 10408: update for 0x… (not a call), skipping`. `-backfill` and `-post` treat update
posts the same way.

The rule (`postKind` in `postkind.go`): a post is an `update` when its text contains
`hit <number>x` (any letter case, decimals allowed, an optional space before the x: `hit 3X`,
`hit 2.5x`, `hit 10 X`) **and** it does not look like a call — no `EARLY CALL`, no
`Pool Info` / `Token Info`, and none of the data lines read from a call (DEX, Mcap, Liq, Tax,
Age, Holders, Proof, live buys). Everything else is a `call`, including a post with no text
and a real call that happens to mention "hit 3x". When in doubt, a post is a call.

An update post is never a token's first call and is not counted as a call: the tracker's
"first call only" rule, the website's list and its `call_count`, `last_call_date`,
`total_calls` and `repeat_calls` all leave update posts out.

**Rows stored before this existed** have `post_kind` NULL and are read as calls until they
are classified. The tracker (at the start of every cycle) and `-web` (when it starts) classify
them from the stored `message_text` with the same rule, 1,000 rows at a time, and log one line
when there was something to do: `posts: 9,871 call(s), 257 update(s) classified`. Only
`post_kind` is written, their `status` stays (`backfill`, `scanned`, …). Their tracking rows are
set to `repeat` on the same tracker cycle (rows already `done` keep their status and results),
and where an update post had been taken for a token's first call, the earliest real call goes
back to `pending` and is tracked. Once every row is classified this costs one small index
lookup per cycle.

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

**Only the first call of each token is tracked.** The channel often calls the same token
again; following every repeat would cost days of node time for the same price history. The
first call is the one the website shows: the earliest `message_date` of a contract address
(compared without regard to letter case), the lowest `scout_calls.id` when two share a date.
At the start of every tracker cycle the later calls get `scout_call_tracking.status = 'repeat'`
(“a later call of a token whose first call is tracked; not tracked itself”) and are never
picked up; the log says so once (`tracking: 3,120 repeat call(s) skipped — …`) and the status
line shows `repeat N`. Nothing is deleted: a repeat call keeps its rows in `scout_calls` and in
`scout_call_dataset_v` (with `tracking_status = 'repeat'`, and no outcomes unless it was
tracked before), and a repeat that was already `done` stays `done` with its results. If an
older call of a token is imported later (a newest-first backfill), it becomes the tracked one
and the previous first call becomes `repeat`; if a first call is deleted, the next call of
that token goes back to `pending` by itself. Update posts (see "Two kinds of posts") are not
calls: they are never a first call, new ones get no tracking row, and tracking rows that old
update posts already have are set to `repeat` whatever their status, except `done`.

To track repeats again you would set those rows back to `pending`
(`UPDATE scout_call_tracking SET status = 'pending', next_check_at = now() WHERE status = 'repeat'`)
— but the tracker marks them `repeat` again on its next cycle, so this needs a build
without that step; there is no setting for it.

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

The summary shows it: `backfill: 500 posts read, 180 calls, 182 CAs (0 new, 182 already recorded), 12 update post(s) (not calls)`.
Update posts are recorded with `status = 'update'` and no tracking row; they are counted on
their own and are in none of the other numbers except "posts read". The running listener works through them, or run `./scoutanalytics -track` as a separate process.

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
| `SCOUT_RPC_RPS` | `0` | max RPC requests per second (`0` = no limit, for your own node; set a number for a shared or public endpoint) |
| `SCOUT_RPC_LOG_CHUNK` | `200000` | most blocks per `eth_getLogs`. Every scan starts at this size. When the node refuses a range as too large (too many blocks or results, or an answer over 64 MB), or the range times out twice, that range is asked again in halves, for that scan only (not below 200 blocks); the scan grows back to this size after 3 answered ranges. Rate limits and busy answers are retried at the same size and never make ranges smaller |
| `SCOUT_MAINNET_RPC_URL` | none | Ethereum mainnet **archive** node; enables ETH/USD from mainnet Chainlink |
| `SCOUT_MAINNET_CHAINLINK_FEEDS` | `eth=` ETH/USD feed | extra `token=feedOnEthereum` mappings |
| `SCOUT_MAINNET_RPC_RPS` | `0` | max requests per second to the Ethereum node (`0` = no limit) |
| `SCOUT_CHAINLINK_FEEDS` | none | feeds **on Robinhood Chain**: `token=feed,…`; use `eth` for WETH/native ETH |
| `SCOUT_STABLES` | USDG | tokens worth $1 |
| `SCOUT_WETH`, `SCOUT_V4_POOL_MANAGER`, `SCOUT_ETH_USD_POOL` | Robinhood Chain addresses | override if needed |
| `SCOUT_DISCOVERY_BLOCKS` | `18000` | ± blocks around the call searched for the token's transfers (widened automatically) |
| `SCOUT_RUG_LIQ_USD` | `500` | liquidity below this = rugged |
| `SCOUT_TRACK_INTERVAL` | `1m` | how often due checks are processed |
| `SCOUT_TRACK_WORKERS` | `8` | calls tracked at the same time (on-chain source) |
| `SCOUT_RPC_PARALLEL` | `8` | block ranges of one scan fetched from the node at the same time |
| `SCOUT_RPC_MAX_INFLIGHT` | `64` | most requests in flight to the node at once (workers × ranges, capped here) |
| `SCOUT_RPC_LOG_CACHE` | `300000` | swap logs kept in memory so repeat calls of a token are not scanned twice (`0` = off) |
| `SCOUT_LATEST_REFRESH` | `on` | `off` = no latest-price pass (see below) |
| `SCOUT_LATEST_REFRESH_RECENT` | `15m` | how often the latest price of a call younger than 30 days is read again (at least `1m`) |
| `SCOUT_LATEST_REFRESH_OLD` | `24h` | the same for calls 30 days or older (at least `10m`) |
| `SCOUT_LATEST_BATCH` | `200` | latest prices read per tracker cycle at most (1 – 10000) |

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
It also has one row per update post, marked `post_kind = 'update'` (`call` for a real call,
NULL = not classified yet): the training code leaves those rows out and never uses
`post_kind` as a model input. `prior_calls`, `secs_since_prev_call`, `calls_prev_1h` and
`calls_prev_24h` are unchanged: they still count every stored post, update posts included.
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

### Latest price (the return as of now)

The horizon numbers stop at 30 days. So that an older call still shows where it stands
today, the tracker also keeps a **latest price** for the first call of every tracked token
(on-chain price source only; status `tracking` or `done`, with a pool and an entry price —
never `repeat`, `pending`, `no_pool`, `error` or `gave_up`):

| `scout_call_tracking` column | |
|---|---|
| `latest_price_usd` | the price, in the call's `price_unit` (like `entry_price_usd`) |
| `latest_return_pct` | that price against `entry_late_price_usd` (against `entry_price_usd` when the late entry is missing) |
| `latest_checked_at` | when the price was read; the price is "as of" this time |
| `latest_trade_at` | time of the last trade the price comes from; for a dead token this can be weeks before `latest_checked_at` |

All four are NULL until the first refresh. `scout_call_dataset_v` has `latest_price_usd`,
`latest_return_pct` and `latest_checked_at` as its last three columns. They are **outcomes
that keep moving**: never model inputs (`ml/` refuses every `latest_*` column as a feature).

**What it reads.** A pool's price only changes with a trade, so the latest price is the
price of the last swap at or before the newest block. A refresh reads the pool's swap logs
from where the previous refresh stopped (the first time: from where the horizon scan stands)
up to the newest block and keeps the last one; with no new swap the price stays what it was.
For a USD-priced call it is multiplied by the quote asset's USD price at the start of the
current hour — one lookup per hour for all calls together, so the USD value moves with ETH at
most once an hour. Calls in another asset keep quote units (and are not shown on the website).

**It does not touch horizon tracking.** The pass has its own cursor in the stored on-chain
state (`latest_block`, `latest_price_q`, `latest_trade_block`) and writes only that and the
four columns: the horizon scan position, running peak/low, results, candles, `status`,
`next_check_at`, `attempts` and `updated_at` stay as they are, and no call is tracked again
because of it.

**Schedule.** A call is due when it has no latest price yet, or the last one is older than
`SCOUT_LATEST_REFRESH_RECENT` (15 minutes; calls younger than 30 days) or
`SCOUT_LATEST_REFRESH_OLD` (a day; calls 30 days or older). The pass runs in every tracker
cycle **after** the horizon checks, through the same workers (`SCOUT_TRACK_WORKERS`), and
takes at most `SCOUT_LATEST_BATCH` calls: those younger than 30 days first, then the ones
not refreshed for the longest. Horizon work always goes first:

- when the horizon batch of a cycle was full (more of it is waiting), only calls younger
  than 30 days are refreshed in that cycle;
- a pass that runs longer than `SCOUT_TRACK_INTERVAL` hands out no further calls; the rest
  stays due and follows after the next horizon check;
- `-track-once` runs one pass after the horizon checks.

A call whose refresh fails (node error, no USD price for its quote asset) is left as it was
and tried again after 30 minutes. Ctrl+C in the middle leaves the calls it was working on
untouched. `SCOUT_LATEST_REFRESH=off` turns the whole pass off; the columns then keep their
last values.

**It adds node work.** Per call, in `eth_getLogs` requests (at the default
`SCOUT_RPC_LOG_CHUNK=200000`, about 10 blocks per second, measured on the test chain):

| | first refresh | every refresh after |
|---|---|---|
| a call past its 30-day horizon (e.g. 60 days old) | everything since the 30-day mark: about 130 for a 60-day-old call (26 million blocks), 4 – 5 more per further day of age | 5 – 6 (one day of blocks), once a day |
| a call still being tracked (e.g. 2 days old) | from its last finished horizon to now: 5 – 6 for a 2-day-old call, up to about 100 for one just short of 30 days | 1 (15 minutes of blocks; 2 when the range crosses a chunk boundary), every 15 minutes |

plus one `eth_blockNumber` per pass, one `eth_getBlockByNumber` per call whose last trade
changed, and one USD lookup per quote asset and hour. So the **first passes after an
upgrade are the expensive part**: 5,000 tokens that are 60 days old need about 650,000
`eth_getLogs` requests in total (more when they are older), spread over at least 25 cycles
of 200 calls (lower `SCOUT_LATEST_BATCH` to spread it further, or set `SCOUT_RPC_RPS`).
After that a day costs about 5 requests per old token and about 100 per token younger than
30 days.

### What the tracker logs

```
posts: 9,871 call(s), 257 update(s) classified
tracking: 3,120 repeat call(s) skipped — only the first call of each token is tracked
tracking: 37 call(s) due now
call 10126 [1/37]: 0x129b…, posted 2026-09-28 14:02 (70h ago), status pending
call 10126 [1/37]: pool found: uniswap-v3 0x…, paired with WETH (entry block 21300412)
call 10126 [1/37]: entry price $0.0031 (1.03e-06 WETH × $3010, Chainlink on Ethereum mainnet)
call 10126 [1/37]: +1h → 0.0052 (+67.7%), peak +120.4%, low -8.1%
call 10126 [1/37]: scanning blocks 21726610 → 22164412: 46% (at 21926609, 12 events so far, 200000-block ranges)
call 10126 [1/37]: tracking in 14s, 212 RPC requests — next check 2026-10-01 14:12
tracking: processed 37 call(s) — pending 112, tracking 37, done 4, repeat 3120; more due now
tracking: idle — tracking 149, done 4, repeat 3120; next check in 42m10s
latest prices: 180 refreshed (12 changed) in 4.2s, 1,930 RPC requests, 5,430 waiting
```
The `latest prices:` line is printed once per pass that had something to do: `changed` =
calls with a trade since their price before, `waiting` = calls still due. Failed calls are
counted on the same line with the first error (`; 3 failed, tried again after 30m0s (first:
call 812: …)`), not one line each. A pass that takes long says every 30 seconds how far it is.
`repeat` = later calls of a token already called, which are not tracked (see above); tracking
rows of update posts stored by an earlier version are in this number too. The `posts:` line
appears only when rows without a `post_kind` were classified (once, after upgrading).

Long block scans print a progress line every few seconds, and a status line is
printed after every cycle (every `SCOUT_TRACK_INTERVAL`, even when idle), so a
quiet terminal for more than a minute means something is stuck.

**Block range size.** The start-up line `performance tracking on: … via eth_getLogs ranges
of up to 200000 blocks, …` shows the `SCOUT_RPC_LOG_CHUNK` in effect. The progress line
ends with the size that scan is using right now (`…, 200000-block ranges)`), and the
`still working` heartbeat names the request it waits on (`eth_getLogs blocks A-B (N blocks)`).
When the node refuses a range as too large or it times out, or the scan grows back, one
line says so, at most one such line every 30 seconds for all workers together (the ones in
between are counted at the end of the next line):

```
call 9163 [37/50]: eth_getLogs blocks 52000000-52199999 (200000 blocks) refused as too large (rpc error -32000: query returned more than 10000 results); this scan continues with 100000-block ranges (max 200000)
call 8120 [12/50]: eth_getLogs blocks 31000000-31199999 (200000 blocks) timed out on the node (2 tries) (rpc error -32000: request timed out); this scan continues with 100000-block ranges (max 200000)
call 9163 [37/50]: eth_getLogs ranges back up to 200000 blocks (max 200000) after 3 answered ranges [also 4 range split(s) and 2 grow-back(s) in all scans since the last such line]
```

The smaller size belongs to that one scan; other calls keep theirs and every new scan
starts at the maximum. A node that rate limits or says it is busy gets the same range
again after a pause; if it keeps doing so, the call stops for this cycle with the error
and continues from its saved cursor next time. A range the node answers with `request
timed out` (or `query timeout exceeded`, `context deadline exceeded`) is asked once more
at the same size and then in halves; one that gets no answer at all within the client's
60-second limit, 4 tries in a row, is split right away (the line then says `got no answer
in time (client time-out)`). A grown size that times out is not asked
twice, and each such failure doubles the wait before the next try (up to 192 answered
ranges), so a scan over blocks the node reads slowly settles at the largest size that
works and only tries a bigger one now and then; once the answers come back in a quarter
of the time the time-out took (indexed blocks), it grows back at the normal pace.
A Nitro node without full log history (`--execution.rpc.log-history` other than `0`;
the default keeps about 9.4M blocks) reads older blocks one by one, about 1 ms per block,
so it answers old ranges slowly and the tracker uses small ranges there (about 25000
blocks at the default maximum). Ranges are not split below 200 blocks: one that still
fails at that size stops the scan with the line
`… (200 blocks) timed out on the node (2 tries) at the smallest range size (200 blocks); this scan stops here (…)`.
An error the node does not explain is asked once more and, if it comes back, treated as
"too large" (the line then says `failed twice with an error the node does not explain`).

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
| `scout_call_tracking` | call (1:1) | pool, entry price (+ source), status, next check, current liquidity, `rugged`; `latest_price_usd`, `latest_return_pct`, `latest_checked_at`, `latest_trade_at` (the latest-price pass) |
| `scout_call_returns` | call × horizon | `horizon`, `price_usd`, `return_pct`, `max_gain_pct`, `max_drawdown_pct`, `last_trade_at` |
| `scout_call_dataset_v` (view) | call | features + pivoted outcomes; what `-export-dataset` writes |
| `scout_calls` | CA found in a @scoutrobinhood post | `message_id`, `message_date`, `message_text`, `urls`, `contract_address`, `chain`, `status` (`queued` → `scanned`/`failed`, or `duplicate`/`dropped`; `backfill` = imported from history; `update` = an update post, recorded only), `post_kind` (`call` / `update`; NULL = stored before the column existed, not classified yet) |
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

## Website

A small read-only page that shows how far the import/tracking is and lists every token the
channel called, with the performance of its first call. It runs as **its own process**, next to the listener and the tracker, and
needs only the database (no Telegram login, same as `-track`):

```bash
./scoutanalytics -web          # or: go run ./telegrambot/scoutanalytics -web
# → website on http://[::]:8090 …   open http://<this machine>:8090/
```

| Setting | Default | |
|---|---|---|
| `SCOUT_WEB_ADDR` | `:8090` | address to listen on. `:8090` = every network interface; `127.0.0.1:8090` = this machine only |
| `SCOUT_GMGN_URL` | `https://gmgn.ai/robinhood/token/{ca}` | link behind the token name / symbol; `{ca}` is replaced by the contract address |
| `SCOUT_WEB_DIR` | *(empty)* | serve the page from this folder instead of the copy built into the program (edit `frontend/` without rebuilding) |
| `SCOUT_WEB_REFRESH` | `15s` | how often the website reads the list again from the database (a duration such as `10s` or `1m`; at least `2s`, a smaller value is raised to `2s`, an unreadable one falls back to `15s`, both with a warning) |

**It is read-only and has no login.** The server only answers `GET` (anything else → 405) and
runs `SELECT`s; anyone who can reach the address can see the calls. Put it behind your own
firewall / reverse proxy, or bind it to `127.0.0.1`, if that is not what you want. Like every
other mode it applies the schema at startup (`SCOUT_DB_AUTO_MIGRATE`), and it reads the same
`.env` (so `API_ID` / `API_HASH` must be present, although no Telegram connection is made).

**Speed: the website answers from memory.** When it starts, the website reads the whole
list once (one row per token, with the numbers of all five windows, plus the counts of the
progress panel) and keeps it in memory as a *snapshot*. Every request for the list or the
counts is answered from that snapshot: search, filter, sort and paging happen in memory and
**no request waits for the database**. In the background the website reads the list again
every `SCOUT_WEB_REFRESH` (15 seconds by default) and swaps the new snapshot in at once;
requests under way finish on the old one.

- **What you see can be up to `SCOUT_WEB_REFRESH` old** (plus the fraction of a second the
  read takes). A call stored by the listener, or a result written by the tracker, shows on the
  page after the next refresh. "Updated hh:mm:ss" in the progress panel is the time the
  website last read the database, not the time the page asked.
- **If the database cannot be read**, the website keeps answering from the snapshot it has and
  logs `web: could not refresh the snapshot: …` (at most one line a minute) until it works
  again; "Updated" on the page then stops moving. If the very first read fails, the website
  does not start and says why.
- **Log**: one line `web: snapshot 9,871 tokens in 180ms` when the website starts, and again
  only when the number of tokens changed or a read took longer than a second.
- **The read** is one statement over `scout_calls`, `scout_call_tracking`,
  `scout_call_metrics`, `scout_call_returns` and `scout_investigations` (not through
  `scout_call_dataset_v`, whose per-row lookups the page does not need). On the test machine
  (2 cores) it takes about 0.3 seconds for 12,000 tokens (22,600 posts). No new table or index.
- **Memory**: under 1 KB per token for the snapshot (about 9 MB for 12,000 tokens, about
  80 MB for 100,000), on top of the program itself; while a refresh runs, the rows just read
  are in memory next to it for a moment. The whole process measured 40–55 MB with 12,000
  tokens (peak 78 MB under a load test).

**Caching and compression** (nothing to set up):

- Answers of `/api/calls` and `/api/summary` carry an `ETag` made from the content of the
  snapshot and the question asked. The page sends it back with its 30-second refresh
  (`If-None-Match`); when nothing changed the server answers `304 Not Modified` with no body
  and the page leaves the table as it is. A refresh that finds the same data in the database
  keeps the same `ETag`, so an idle site costs a few hundred bytes per open tab per refresh.
  The `ETag` of `/api/summary` follows the counts only, so it stays "not modified" while rows
  change but the counts do not.
- JSON and the page's text files are sent gzip-compressed to clients that ask for it
  (`Accept-Encoding: gzip`; every browser does) when the answer is 1 KB or larger — a page of
  50 rows goes from about 30 KB to about 5 KB.
- An answer of `/api/calls` is kept (also in its compressed form) until the next refresh, so
  the same question asked again — the default view above all — is not worked out twice. At
  most 200 answers / 8 MB are kept; beyond that, questions are simply answered without being kept.
- The files of the built-in page (`index.html`, `app.js`, `style.css`, …) carry an `ETag` that
  is a hash of their content and `Cache-Control: no-cache`: the browser keeps its copy but asks
  each time whether it is still current, so a new version of the program shows at once and an
  unchanged file costs a `304`. With `SCOUT_WEB_DIR` the files are read from the folder for
  every request (an edit shows on the next reload) and checked by modification time instead.

The page (`frontend/index.html`, `app.js`, `style.css`; plain JavaScript, nothing loaded from
other sites):

- **Typing in the search box** waits 0.3 seconds after the last key, and a request still on
  its way is cancelled when a newer one starts, so the table never shows the result of an
  older search. The page refreshes itself every 30 seconds, not while its tab is in the
  background; when you come back to the tab after more than 30 seconds it refreshes at once.

- **One row per token.** The channel often calls the same token several times, sometimes
  within the same minute. The page shows each token once: its **first call** (the earliest
  `message_date`; when two posts have the same time, the one stored first, i.e. the lowest
  `scout_calls.id`). Tokens are told apart by contract address, ignoring upper/lower case.
  Search, sorting, the window, paging and every count work on that one-row-per-token list.
  This is a display rule of the website only: nothing is deleted, and `scout_call_dataset_v`,
  the tracker and the model export still have one row per call.
  Update posts ("$TOKEN hit 3X …") are not calls: they are never a row, never a token's first
  call and are not counted in `×N`.
- **Import progress** — `tracked / imported calls tracked (x %)`, then Pending, No pool, Errors
  (error + gave up) and No USD price, each with its share of the imported calls; all of these
  count first calls only, with the state of the first call. Below: "One row per token (its
  first call). N repeat calls and M update posts are not shown." (a part whose number is 0 is
  left out). Refreshes every 30 seconds; "Updated hh:mm:ss" is when the website last read the
  database.
- **Calls** — Date (links to the post), Token and Symbol (link to GMGN), Calls (`×N` when the
  token was called N > 1 times, empty otherwise; hover for "Called N times, last on …"),
  Entry $, Call MC, Return %, Peak %, Worst drop %, Latest %, Latest MC, Perceptor, Status. Search by
  token name, symbol or address; pick the window (1h, 1d, 3d, 7d, 30d); click Date / Call MC /
  Return / Peak / Latest / Latest MC to sort, click again to reverse. 50 per page.
- **Perceptor** — the column shows the verdict of the token's Perceptor report as words:
  "no red flags", "caution", "red flags" (linked to the report, in a new tab), or "–" when
  there is none. The "Perceptor" select next to the search box filters the list: All reports,
  No red flags found, Caution, Red flags, Not scanned.
  **The verdict belongs to the token, not to the listed call:** it is the latest completed
  Perceptor report for that contract address (upper/lower case ignored), whichever post of
  the token it was made for. So a token whose first call was imported from history still
  shows the verdict of a later post that was scanned.
  **Calls imported from history have no report**: only calls the listener picks up live are
  sent to Perceptor, so most imported tokens show "–" and are found under "Not scanned".
  A scan that failed, timed out or was rate-limited does not count, and a report whose
  verdict could not be read counts as not scanned.
- Every number is **in USD and measured from the entry 60 seconds after the post** (the
  `*_late_*` columns). A call tracked in another asset (no USD source for its pair) shows
  "no USD price" instead of numbers (and a dash in both market cap columns). Sorting by Return,
  Peak, Latest, Call MC or Latest MC lists the USD-priced calls only.
- **Latest %** (the column after "Worst drop %") is the return at the most recent price,
  from the same entry, followed by how old the call was when that price was read:
  `+35.2% · 60d` (`45m` under an hour, `30h` under two days, otherwise days). It comes from
  the tracker's latest-price pass (about every 15 minutes for calls under 30 days old, once
  a day for older ones) and does not change with the 1h … 30d buttons. `· quiet` is added
  when the last trade is more than 7 days older than the reading (the price is then that of
  an old trade); the tooltip gives both times. A dash means no latest price has been read yet.
- **Call MC** (after "Entry $") is the market cap given in the call post: its "called at"
  figure, or its "📈 Mcap" line when the post has no "called at" (`scout_call_metrics`).
  **Latest MC** (after "Latest %") is an **estimate**, since the latest market cap is not
  stored: the post's market cap (the Mcap line first, else "called at") × the latest price ÷
  the price at the post. It assumes the token supply has not changed. Both are written
  compactly (`$850`, `$45.2k`, `$1.3M`, `$2.1B`); hover for the exact amount. A dash when the
  post gave no usable market cap, when there is no latest price (Latest MC), and for calls not
  priced in USD. The legend under the table says that Latest MC is an estimate and how it is
  worked out.

**Token names.** The tracker reads each token's own `name()` and `symbol()` from its contract
and stores them in `scout_call_tracking.token_name` / `token_symbol_onchain` (on-chain price
source only). After every tracker cycle (`-track`, `-track-once`, or the listener's built-in
tracker) it fills up to 200 tokens that have none yet: two small node requests per token,
nothing is tracked again because of it, and a token without a name is stored as empty and not
asked again. So after an upgrade the names appear on the page as the tracker runs; until then
the page shows the shortened address. `scout_call_dataset_v` gained `token_name`, and its
`token_symbol` is now the symbol from the post, or the on-chain one when the post has none.
Names come from arbitrary contracts: control characters are removed, the length is capped
(100 / 32 characters), and the page only ever shows them as text.

### API

Both endpoints answer from the snapshot (see above) and send `ETag`, `Cache-Control: no-cache`,
`Vary: Accept-Encoding` and `X-Snapshot-At` (when the database was last read, RFC 3339). Send
the `ETag` back as `If-None-Match` to get `304 Not Modified` while the data is unchanged. The
`ETag` is a weak one (`W/"…"`): the same `ETag` means the same data; only the time stamps in
the body (`updated_at`, `snapshot_age_seconds`, `snapshot_at`) move with every refresh.
Before the first snapshot exists (only possible for a moment at start) they answer 503.

`GET /api/summary` — counts over first calls (one per token), each with the state of its
`scout_call_tracking` row:

```json
{"imported": 4210, "tracked": 3105, "pending": 820, "tracking": 410, "done": 2695,
 "no_pool": 240, "error": 30, "gave_up": 15, "no_usd_price": 62,
 "total_calls": 5120, "repeat_calls": 910, "update_posts": 257,
 "updated_at": "2026-10-02T14:30:00.123Z", "snapshot_age_seconds": 7.4}
```
`updated_at` = when the website last read the database (the time of its snapshot);
`snapshot_age_seconds` = how long ago that was when the answer was written.
`imported` = first calls = distinct tokens called, `tracked` = those with an entry price,
`pending` … `gave_up` = those by status, `no_usd_price` = tracked ones whose `price_unit` is
not `usd`. `total_calls` = every real call in `scout_calls`, repeats included; `repeat_calls` =
`total_calls − imported`, the calls the page does not list. `update_posts` = the update posts
in `scout_calls` (not calls: they are in none of the other numbers and are not listed).

`GET /api/calls` — one page of first calls, one row per token (from the snapshot);
`q`, `sort`, `usd_only`, `verdict`, paging and `total` all apply to that list, so a repeat call is never
returned and cannot be found by its own name or symbol:

| Parameter | Values | Default | |
|---|---|---|---|
| `q` | text, up to 100 characters | *(none)* | part of the token name, symbol or contract address; case-insensitive (letters of any script, by the Unicode lower-case rule); `%` and `_` are ordinary characters |
| `sort` | `date`, `return`, `peak`, `latest`, `call_mc`, `latest_mc` | `date` | empty values always come last (in both directions); ties by call id, in the direction asked for. `latest` = by `latest_return_pct`, `call_mc` = by `call_mcap_usd`, `latest_mc` = by `latest_mcap_usd` (these three are the same for every `horizon`) |
| `dir` | `desc`, `asc` | `desc` | |
| `horizon` | `1h`, `1d`, `3d`, `7d`, `30d` | `1d` | which window `return_pct` / `peak_pct` / `drawdown_pct` are for |
| `usd_only` | `1`, `0` | `1` when `sort` is `return`, `peak`, `latest`, `call_mc` or `latest_mc`, else `0` | `1` = only tokens whose first call has `price_unit = usd` |
| `verdict` | `clean`, `caution`, `red_flags`, `not_scanned` | *(none = all)* | the token's Perceptor verdict (see below). `clean` = no red flags found; `not_scanned` = no completed Perceptor report, or one whose verdict is `unknown` |
| `page` | 1 … | `1` | |
| `per` | 1 – 200 | `50` | |

Any other value or parameter → HTTP 400 with `{"error": "…"}`.

```json
{"total": 3105, "page": 1, "per": 50, "horizon": "1d", "sort": "date", "dir": "desc", "usd_only": false, "verdict": "",
 "snapshot_at": "2026-10-02T14:30:00.123Z",
 "calls": [{"call_id": 812, "message_id": 10002, "message_date": "2026-10-01T14:30:00Z",
   "post_url": "https://t.me/scoutrobinhood/10002",
   "contract_address": "0x…", "token_name": "Malfoid", "token_symbol": "MALFOID",
   "gmgn_url": "https://gmgn.ai/robinhood/token/0x…", "price_unit": "usd",
   "entry_price_usd": 0.0045, "return_pct": -20.0, "peak_pct": 100.0, "drawdown_pct": -50.0,
   "rugged": false, "tracking_status": "done", "perceptor_verdict": "clean",
   "perceptor_url": "https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0",
   "call_count": 3, "last_call_date": "2026-10-02T09:12:00Z",
   "latest_return_pct": 35.2, "latest_price_usd": 0.006084,
   "latest_at": "2026-11-30T14:31:10.52Z", "latest_trade_at": "2026-11-30T13:02:44Z",
   "latest_age_seconds": 5184070, "call_mcap_usd": 45200, "latest_mcap_usd": 61110.4}]}
```
`call_count` = how many real calls of that token exist in total (1 or more; update posts are
not counted); `last_call_date` = the date of the most recent one (equal to `message_date` when there is only one). Everything else
in the object belongs to the first call.
`entry_price_usd` is the price 60 seconds after the post (the price at the post when that one
is missing). `entry_price_usd`, `return_pct`, `peak_pct` and `drawdown_pct` are `null` unless
`price_unit` is `usd`. `gmgn_url` is `null` for anything that is not a plain `0x…` address.

`latest_return_pct` and `latest_price_usd` are the return (from the same entry) and the price
as of the tracker's most recent reading, `latest_at` when it was read
(`scout_call_tracking.latest_checked_at`), `latest_trade_at` the time of the trade that price
comes from (`null` when unknown) and `latest_age_seconds` = `latest_at − message_date`: how
old the call was at that moment. They do not depend on `horizon`. All five are `null` until
the tracker has read a latest price for the call, and always for calls whose `price_unit` is
not `usd`. A new reading changes the `ETag` of `/api/calls` (the age moves even when the
price does not), so with the tracker running the list is "modified" about once per tracker
cycle; `/api/summary` is not affected.

`call_mcap_usd` and `latest_mcap_usd` are the last two fields of a call (in USD, from
`scout_call_metrics` of the first call and its `scout_call_tracking` row):

- `call_mcap_usd` = `COALESCE(called_at_mcap_usd, mcap_usd)`: the market cap at the call as
  given in the post.
- `latest_mcap_usd` = `COALESCE(mcap_usd, called_at_mcap_usd) × latest_price_usd ÷
  entry_price_usd`, where `entry_price_usd` is the tracking row's price **at the post**
  (not the late entry the `entry_price_usd` field of the response shows; the post's market cap
  is a post-time figure). An **estimate** that assumes the token supply has not changed; the
  latest market cap itself is not stored.

Both are `null` when the call's `price_unit` is not `usd`, and when an input (the market cap
`COALESCE` picked, the latest price or the price at the post) is missing, zero, negative or
not finite, or the result is not finite. `COALESCE` only skips a missing value: a "called at"
of 0 gives `call_mcap_usd = null`, not the Mcap line. `latest_mcap_usd` is also `null` whenever
`latest_price_usd` is (no latest price yet). Like the latest price, they do not depend on
`horizon`, and a change to either value changes the `ETag` of `/api/calls`.

`snapshot_at` = when the website last read the database (the same moment as `updated_at` of
`/api/summary`). `verdict` in the response repeats the filter that was applied (`""` when none).
`perceptor_verdict` and `perceptor_url` are **per token**: the verdict (`clean`, `caution`,
`red_flags` or `unknown`) and report link of the latest completed Perceptor investigation
(`scout_investigations`, tool `perceptor`, `status = completed`, newest `requested_at`) with
that contract address, upper/lower case ignored — not only the listed call's own. Both are
`null` when the token was never scanned, which is the normal case for calls imported from
history (only live calls are scanned). `perceptor_url` is also `null` when the stored link
does not start with `https://`. This is a rule of the website only:
`scout_call_dataset_v.perceptor_verdict` is unchanged and still belongs to the single call.

## State / logs

- `scoutanalytics_data/seen_cas.json` — CAs already investigated (never re-run; delete an entry to rerun)
- `scoutanalytics_data/scans.jsonl` — every CA: each tool's status, verdict and report text, delivered?

CAs are processed one at a time; within a CA all tools run in parallel (replies are
matched to each bot, so they can't get mixed up). The client reconnects with
backoff if the connection drops. Ctrl+C to stop.
