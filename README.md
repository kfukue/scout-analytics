# scoutanalytics

> Moved from `kfukue/geth-analytics` (`telegrambot/scoutanalytics`), history kept.
> Commit hashes cited in older notes refer to the old repository.

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
API repo's telegrambot session.

## Run

From the repo root:

```bash
# 1. one CA — prints every tool's report + verdict and delivers it (if the gate allows)
go run . -scan 0xYourTestCA
#    print only, don't send:
go run . -scan 0xYourTestCA -no-deliver

# 1b. process a specific call post (prints what it found in the post, then investigates + delivers)
go run . -post 10002          # or -post https://t.me/scoutrobinhood/10002

# 2. watch the channel but deliver nothing (verdicts go to the log)
go run . -dry-run

# 3. real thing
go run .
```

First run asks for the Telegram login code (and uses `TG_PASSWORD` if you have 2FA).
Make sure you've pressed **Start** on **@perceptor0xBot and @salpha_research_bot** once from this account.

Deploying, restarts, health checks and draft systemd units: see [DEPLOY.md](DEPLOY.md).

## How calls are picked up

- **Live updates** for new posts, plus **edited posts** (a call posted first and the CA added later).
- **Polling backup** every `SCOUT_POLL_INTERVAL` (default `20s`, `0` = off): Telegram doesn't
  always push every post of a big channel to user accounts, so the scanner also asks for
  posts newer than the last one it polled. Each (post, CA) is handled once.
- **Catch-up after a restart** (or a reconnect): the posts that arrived while the listener was
  down are handled on start. Polling resumes from a saved cursor, the lower of the newest post
  recorded in `scout_calls` for the channel and `scoutanalytics_data/poll_cursor.json` (written
  whenever polling moves on; the only cursor when `SCOUT_DB=off`). The missed posts are then
  read (oldest first) and split:
  - the newest `SCOUT_CATCHUP_MAX` posts (default `100`, `0` to `250`) that are at most
    `SCOUT_CATCHUP_MAX_AGE` old (default `24h`, `0` = no age limit; an invalid value stops the
    start) are **handled live**: investigated and delivered as usual, update posts recorded
    only, tokens already investigated recorded as `duplicate`;
  - the older ones are **stored only**, the way `-backfill` stores them (status `backfill`,
    tracked, no bot scans, no delivery, the token's investigation is not used up). With
    `SCOUT_DB=off` they are skipped (nothing is recorded).

  Posts already in `scout_calls` are skipped, so nothing is handled twice. One log line:
  `catch-up: 12 post(s) since post 10420 (12 handled live, 0 stored only)`, then
  `polling @… (starting after post N, from …)`. With no saved cursor at all (first run, empty
  database) it starts at the channel's newest post as before.
- **Calls still waiting at a restart are queued again.** The job queue lives in memory: calls
  that were `queued` (or `dropped` because the queue was full) and have no completed
  investigation are put back in it at the start, before the catch-up's posts (oldest first),
  with the same rules: the newest `SCOUT_CATCHUP_MAX` of them no older than
  `SCOUT_CATCHUP_MAX_AGE` are scanned, the others are stored only (status `backfill`, the
  token freed in `seen_cas.json`), and a call whose token another call already investigated
  becomes `duplicate`. One log line when there are any:
  `requeue: 3 queued call(s) from before the restart (2 scanned now, 1 stored only)`. A call
  being scanned when the listener stops can end as `failed` (its bot requests are cut off);
  `failed` calls are not requeued. Needs the database (nothing to requeue with `SCOUT_DB=off`).
  Several waiting calls of one token are scanned once (the newest; the older ones become
  `duplicate`, or `backfill` with it); a call whose token is already queued in this process
  becomes `duplicate`.
- **The requeue only looks back `SCOUT_CATCHUP_MAX_AGE` + 48h** (72h with the defaults; 24h
  + 48h when `SCOUT_CATCHUP_MAX_AGE=0`). Older `queued`/`dropped` rows are never rewritten,
  also on the first deploy of this feature; when there are any, one line per process counts
  them: `requeue: N queued or dropped call(s) posted before … left as they are; DEPLOY.md 3.3
  has the SQL to fix them by hand`.
- The requeue and the catch-up run when the listener (re)connects, also with
  `SCOUT_POLL_INTERVAL=0` (then only once: a failure is logged and not retried until the next
  start). With polling on, a failure (Telegram or the database unreachable) handles nothing and
  is retried at every poll interval; the outage is never skipped. A stored-only post that cannot
  be written stops the catch-up there (the cursor stays before it) until the next try. When only
  the second requeue (after the catch-up) fails, polling runs and each poll retries it. More than 50 new posts in one
  interval are read page by page, so polling skips none either. Worst case after a long
  outage: Perceptor scans one CA every 2m5s, so 100 calls take about 3.5 hours, and newer calls
  wait behind them; the requeue and the catch-up each allow up to `SCOUT_CATCHUP_MAX`, so up to
  twice that can be waiting. Lower `SCOUT_CATCHUP_MAX` if that is too long.
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
is enough**, as long as it serves old event logs (`eth_getLogs`); an archive node also works.

A moment (the call, a horizon, an hour boundary) is turned into a block one way everywhere:
the **last block whose timestamp is at or before it**. The chain makes about 10 blocks a
second, so "a block in that second" would not be one block; the search always runs down to
that last block, so the answer does not depend on what earlier lookups cached.


1. **Pool:** found from the token's own transfers around the call: the address it moved
   to/from most is checked on-chain and identified as a Uniswap **v2** pair, **v3** pool or a
   **v4** pool on the PoolManager. No indexer or factory address needed.
   - **v4** (when the PoolManager is among those addresses): the token's pools come from the
     PoolManager's `Initialize` events, which index both currencies: two `eth_getLogs` over the
     whole history (token as currency0, token as currency1), each one request on a node with a
     full log index (split automatically if the node refuses the range or times out). Then
     those pools' `Swap`s around the call, filtered by pool id (indexed; up to 32 ids per
     request). The pool with the most swaps in the same transactions as the token's transfers
     wins (ties: the lowest pool id); for an asset's own USD pools (step 3) only pools against
     WETH/ETH or a stablecoin count. No `Initialize` = no v4 pool: nothing else is asked.
     The PoolManager's swaps are never read unfiltered (that is every v4 trade on the chain,
     tens of thousands of logs per window). The pools found are kept in memory per token, so
     a later lookup only reads the new blocks.
   - **Node load per discovery:** the token's `Transfer`s around the call (widened ×4, ×16, ×64
     while there are none; only these are widened), a few `eth_call`s per v2/v3 candidate,
     and for v4 the 2 `Initialize` requests plus about 1 `Swap` request per 32 candidate pools
     (per `SCOUT_RPC_LOG_CHUNK` range of the window).
   - **Pons V2** tokens are recognised first and tracked on their bonding curve (see "Pons V2
     tokens" below).
2. **Prices:** every trade, from the pool's events: v2 `Sync` (reserves), v3/v4 `Swap`
   (`sqrtPriceX96`). So peak and drawdown are exact, not candle approximations.
3. **USD:** the pool price is in the pool's other asset (the quote asset). One lookup
   converts it everywhere (entry, candles, horizons, latest pass, scoring, `-price-check`),
   as of the block, trying in this order:
   1. a stablecoin (`SCOUT_STABLES`, USDG) = `$1`;
   2. a **Chainlink feed on Robinhood Chain** (`SCOUT_CHAINLINK_FEEDS` plus the asset
      database, see "Chainlink feeds from the asset database" below);
   3. a **Chainlink feed on Ethereum mainnet** (`SCOUT_MAINNET_CHAINLINK_FEEDS` plus the
      asset database; needs `SCOUT_MAINNET_RPC_URL`); for ETH/WETH then the WETH/USDG v3
      pool (`SCOUT_ETH_USD_POOL`);
   4. the asset's **own pool against WETH / native ETH** × ETH/USD;
   5. the asset's **own pool against a stablecoin**.

   On an archive node feeds and pools are read from contract state (`latestRoundData()` /
   `slot0()` at the block); on a **full node** (no historical state) from event logs: the
   last `AnswerUpdated` of the aggregator the feed used at that block, and the pool's last
   `Swap` at or before the block. This is detected automatically. A source that fails for a
   temporary reason (node busy, time-out) is an error and the call is tried again later; the
   next source is only asked when one is definitively absent (no feed configured, or a feed
   with no update within `SCOUT_PRICE_LOOKBACK_BLOCKS`).

   **Own pools (4, 5).** The asset's pools against WETH/ETH or a stablecoin are found like a
   token's pool (its transfers around the first block asked, v4 through `Initialize`); up to
   3 of each kind are kept. For every block the pool whose last trade at or before it is the
   most recent (within `SCOUT_PRICE_LOOKBACK_BLOCKS`) is used, a WETH/ETH pool before a
   stablecoin pool, so the pool can change from block to block (one log line when it does:
   `prices: USD price of VIRTUAL (0x…) now from its USDG pool (uniswap-v3 0x…) (was …)`). A
   block none of the known pools covers starts one more search around that block. A search
   around one block never decides for another block: when nothing near the block prices it,
   a **whole-history search** looks at every v4 pool of the asset (`Initialize`, all
   blocks) and every address its transfers went to or came from since its first transfer
   (`token0()`/`token1()`, the busiest first). The asset counts as having **no USD source**
   (the call is tracked in its units) only when that search has covered the whole history
   and found no pool against WETH/ETH or a stablecoin; pools that exist but had no trade
   before the block give "no price yet" (retried, capped as below). Cost: one search reads
   at most 100,000 transfer logs and checks at most 256 addresses (a wallet costs one
   `eth_call`, a contract two to four); the next search goes on from there, and until the
   history is covered the answer is "USD pool search not finished": the call is retried
   every 5 minutes (each try moves the search on), never capped or taken for "no USD source";
   the progress is kept in memory, so a restart starts the search over. Once covered, a
   later search reads only the new blocks. One search per asset runs at a time; other
   workers wait for it. What is remembered (in memory, per process): pools found, the
   searched history, and prices per asset and hour; "no pool anywhere" for 1 hour (for the
   blocks the search covered) and "no price yet" (pools, but no trade before that block) for
   6 hours, then asked again. A node error is never remembered.

   **Feed aggregators.** A Chainlink proxy can switch to a new aggregator. Its aggregator list
   is read again every hour (a switch is logged: `prices: chainlink 0x… switched aggregator:
   0x… → 0x…`). For a past block the tracker reads the aggregator the proxy used then: from the
   proxy's `AggregatorConfirmed` events (AggregatorProxy v0.7+), else the newest phase
   aggregator that had reported by that block, so an old aggregator that keeps reporting after
   the switch is not read. If the aggregator named for a block has no update within the
   look-back but another one has, that one is used and a "stale aggregator" line is logged once.
4. **Each horizon** (`1h, 1d, 3d, 7d, 30d`): price, **return %**, **max gain %**, **max drawdown %**
   → `scout_call_returns`. Swaps are scanned once: progress is kept per call, each check
   only reads the new block range. A long segment is read and stored in pieces of about 32
   `SCOUT_RPC_LOG_CHUNK` ranges (about a week of blocks), each ending on a UTC hour (so no candle is split); after
   each piece its candles and the scan position are saved, so a failure or a restart resumes
   from the last stored piece instead of the segment's start, and no trade is counted twice.
5. **Rugged** (see below): the pool's quote side under `SCOUT_RUG_LIQ_USD` ($500) at any
   price event, or an empty pool; and after the last horizon also price < 5% of entry.

**Rugged.** The rule watches the pool's **quote side**: the ETH/WETH, USDG, stock token …
the pool holds, valued in USD. A pool is rugged when its quote side is under
`SCOUT_RUG_LIQ_USD` (default `$500`, settable in `.env`).
- **No extra node requests:** the liquidity comes from each price event the tracker reads
  anyway: v2 the quote reserve in `Sync`; v3/v4 the quote side of the in-range reserves, from
  the `Swap`'s liquidity and `sqrtPriceX96`. The v3/v4 figure is an **in-range approximation**
  (it ignores liquidity outside the current price range). It is valued at the quote's USD
  price at entry (the latest pass: at its current price); a quote with no USD price is only
  caught by an empty pool, never by a guess.
- **An empty pool always counts**, also with `SCOUT_RUG_LIQ_USD=0` (USD check off): zero
  liquidity or a swap at the price bound (v3/v4), a zero reserve (v2).
- **From the first such trade on:** the price is 0, every horizon ending at or after it is
  −100% with drawdown −100%, and its peak is the one reached before the rug; nothing after
  it counts (not even trades in a re-funded pool). The call is flagged `rugged` at once,
  without waiting for the last horizon.
- **Latest return:** −100% (price 0), refreshed at most once a day, with no chain reads.
- **Under the threshold already at the call:** every horizon is −100% with no peak.
- **After the last horizon** the pool's quote-asset balance (`balanceOf`, v2/v3; v4 pools
  share one contract) is compared the same way; the price rule (< 5% of entry) still applies
  too.
- **Once rugged, always rugged.**
- **Backstop:** a price more than 1,000,000× the entry price is skipped as invalid.
- **Pons V2 bonding curve: no liquidity check.** A curve has no LP that can be pulled, and its
  real reserve starts near zero and grows toward the graduation threshold, so the
  `SCOUT_RUG_LIQ_USD` rule (and the empty-pool rule, and the `balanceOf` check after the last
  horizon) does not apply while the token is on its curve. The price rule (< 5% of entry after
  the last horizon) and the 1,000,000× backstop still do. After the graduation the token's v4
  pool is checked like any other.

**Pons V2 tokens (bonding curve).** Pons V2 (ponsfamily.com) launches trade on their own
bonding curve first and move to a Uniswap v4 pool when they graduate.
- **Detected** before the transfer heuristic: the token's `curve()` getter (one `eth_call`;
  other tokens revert), checked against the curve's `factory()` (`SCOUT_PONS_FACTORY`) and
  `token()`; the quote is the curve's `pairToken()` (the zero address = native ETH, priced like
  ETH). A transfer counterparty that answers like the token's Pons curve is accepted too.
  Such a call gets `pool_dex = pons-curve`, `pool_address` = the curve,
  `entry_price_source = onchain-pons`.
- **Prices** come from the curve's `CurveBuy` / `CurveSell` events: the quote that moved along
  the curve ÷ the tokens, i.e. a buy's `quoteIn` minus fee and creator tax, a sell's
  `quoteOut` plus fee and tax (the curve charges both on the quote leg). That is the trade's
  **average** price, not the curve's spot price after it: after a trade that is large next to
  the curve's reserve (a big dev buy, a dump) the two can be far apart, and the price stays
  that average until the next trade (an entry on a big buy sits well below the spot price
  after it; a dump is priced well above the bottom it leaves). Volumes in the pre-call stats
  are the quote value before fee and tax.
- **Graduation:** the curve closes (`CurveCompleted`; nothing trades on it afterwards), and
  later — often in the same block — the factory creates the v4 pool (`PoolGraduated`). The
  pool is the PoolManager's `Initialize` for the token's two currencies in that very block,
  with the Pons hook (`SCOUT_PONS_HOOK`). Curve trades count up to the closing block, v4 swaps
  from the pool's block on: one price history, the peak, low and candles run on across the
  switch. This works in the horizon scan and in the latest-price pass, so a token that
  graduates after its 30d horizon does not keep its last curve price. A launch that is closed
  but never gets a pool keeps its last curve price.
- **Called after the graduation:** tracked on the v4 pool from the start (`uniswap-v4`).
- **Finding a graduation that already happened** (`graduated()` is true but the block is not
  known yet: at discovery, or in a scan or latest pass whose cursor is past it): on an
  archive node a bisection over `graduated()` at past blocks (about 25 `eth_call`s). On a
  full node (no historical state, as on prod) the event logs, with **no look-back limit**
  (`SCOUT_PRICE_LOOKBACK_BLOCKS` does not apply): the curve's `CurveCompleted` and the
  factory's `PoolGraduated` for the token (token = topic 1), in windows around the call block
  that double in size on both sides (starting at `SCOUT_DISCOVERY_BLOCKS`) until one of them
  shows up or the whole chain has been searched, in ranges of up to 4M blocks (split
  automatically if the node refuses them or times out). Both are filtered by address and
  topic, which a node with a full log index (`--execution.rpc.log-history=0`) answers
  quickly over any range: about 4 `eth_getLogs` per doubling, a few dozen for a graduation
  a month from the call. Either event is enough (the close is placed at `PoolGraduated` when
  `CurveCompleted` is missing; the pool's `Initialize` with the Pons hook is looked for when
  `PoolGraduated` is missing). A confirmed Pons token never falls back to the generic
  transfer-counterparty search (which knows nothing of the curve):
  if neither event is found, the call fails with "the curve has closed, but neither
  CurveCompleted nor PoolGraduated was found in the event logs" and is tried again later.
- **Node load per Pons call:** discovery 4–5 `eth_call`s (`curve()`, `factory()`, `token()`,
  `pairToken()`, `graduated()`), plus, when the curve has already closed, the search above
  (archive: ~25 `eth_call`s; full node: one failed historical `eth_call` per run, then
  ~4 `eth_getLogs` per doubling of the distance from the call). Each scan or latest refresh
  of a token still on its curve costs one `graduated()` call and the usual `eth_getLogs`
  ranges; the switch costs one or two `eth_getLogs` for `PoolGraduated` and one for
  `Initialize`.

`scout_call_tracking.current_liquidity_usd` keeps its meaning: the pool's depth, **2 × the
quote side** in USD (so a call rugged at $400 of quote side shows $800); v4 pools have none
unless they rugged. The rule above describes the on-chain source; `SCOUT_PRICE_SOURCE=gecko`
compares half of GeckoTerminal's `reserve_usd` (which counts both sides) with the same number,
and stores `reserve_usd` itself as the depth.

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

Order of sources for a paired asset's USD price: stablecoin ($1) → Chainlink feed on
Robinhood Chain → mainnet Chainlink feed (if `SCOUT_MAINNET_RPC_URL` is set) → WETH/USDG pool
(ETH only) → the asset's own WETH/ETH pool → its own stablecoin pool (see "USD" above).

**Long.xyz tokens trade against Stock Tokens** (NVDA, TSLA, …), so add their Chainlink feeds:

```
SCOUT_CHAINLINK_FEEDS=eth=0xEthUsdFeed,0xNvdaToken=0xNvdaFeed,0xTslaToken=0xTslaFeed
```
A pair whose quote asset has no USD source at all (no feed, no pool against WETH/ETH or a
stablecoin) is still tracked, **in that asset's units** (`price_unit` = e.g. `TSLA` instead of
`usd`); the returns are then relative to the stock token. Only a definitive "no source" does
this: a temporary failure at the entry (node error, or "no price yet": the asset's pools had
not traded before the call block) leaves the call in `error` and it is tried again. "No price
yet" has a cap: once the call is past its last horizon + 48 h (the same deadline as
`gave_up`), the quote's pools will not get a trade before the call any more, so it counts as
a definitive absence and the call is tracked in the quote asset's units, with one log line:
```
call 123 [1/5]: entry price 0.01 QT (no USD price of QT at the call block 6048010 after the last horizon + 48 h: … no price yet … — giving up on USD, tracked in QT)
```
A node error is never capped: it is retried for ever.

**Chainlink feeds from the asset database.** When the database also holds the asset
tracker's tables, the tracker reads feeds from them (read-only, no schema change):
`asset_chains (asset_id, chain_id, chainlink_data_feed_contract_address)` joined to
`assets (id, contract_address, chain_id)` and `chains (id, chain_id = EVM chain id)`. Tokens on
Robinhood Chain (EVM 4663) with a feed on chain 4663 are added to `SCOUT_CHAINLINK_FEEDS`,
with a feed on chain 1 to `SCOUT_MAINNET_CHAINLINK_FEEDS`. Addresses are compared in lower
case; when two assets share a contract address the lowest asset id wins; on a conflict the
environment wins. The feeds are read when the tracker starts and again every cycle (one small
query, 10 s time-out), and replaced as a whole. Without those tables (or when the query
fails) one line is logged and the tracker carries on with the feeds it has (the environment's
only, if no read has worked yet):
```
chainlink feeds: 12 on Robinhood Chain and 3 on Ethereum mainnet from the asset database; in use 13 Robinhood + 4 mainnet
chainlink feeds: no asset tables (assets / chains / asset_chains) in this database — using the environment's feeds only
```
`-price-check` loads them the same way (read-only, no migration) when a database is
configured, and says where its feeds came from; `SCOUT_DB=off` skips the database (environment
feeds only), as does a database without the asset tables or one it cannot open:
```
feeds:        13 Robinhood Chain + 4 Ethereum mainnet Chainlink feed(s) in use; 12 + 3 of them from the asset database (asset_chains), the rest from SCOUT_*CHAINLINK_FEEDS (env wins on a conflict)
feeds:        2 Robinhood Chain + 1 Ethereum mainnet Chainlink feed(s) in use, from SCOUT_*CHAINLINK_FEEDS only; 0 from the asset database (no asset tables)
```
Chainlink stock feeds run 24/5, so weekend conversions use Friday's price. Max gain/drawdown
are converted with the quote asset's USD price at the horizon (not at each trade).

**Check it against your node first:**
```bash
./scoutanalytics -price-check 0xTokenCA -price-at 6h     # or -price-at 2026-10-01T14:30:00Z
```
prints the latest block, the block at the call time, the pool it found (v2/v3/v4, paired
asset), the entry price, the USD conversion (or which feed to add) and the move since.
The last line, `node type:`, comes from asking the node for state (`eth_getBalance`) at the
call block and at a mid-history block (half the latest block number), so it is right even for
a pair that never needs old state: `archive node`, `full node (no historical state at the
call block …)`, `full node that still keeps recent state` (state at the call block, none at
mid-history), or `unknown` when the node did not answer.
For a Pons V2 token it prints `pons-curve`, the curve address, the quote token and the
graduation status (curve closing block, v4 pool block and id, hook), as found at the call
and again after reading up to now:
```
pool:         pons-curve bonding curve 0x… (kind pons)
launchpad:    Pons V2 (curve 0x…, quote ETH 0x0000000000000000000000000000000000000000)
graduation:   graduated after the call: curve closed at block 123, v4 pool from block 456: PoolManager id 0x… (hook 0xe5e7…, token is currency1)
…
graduation now: graduated after the call: curve closed at block 123, v4 pool from block 456: PoolManager id 0x… (hook 0xe5e7…, token is currency1)
```
(`not graduated (still on the bonding curve)` while it is; for a curve that closed after the
call the pool is looked up for the first line too, as the scan does.)
A known Pons token that shows a `uniswap-…` pool instead was not recognised: check that
before resetting the `no_pool` calls.

Long scans print a progress line every 5 s (to stderr), e.g. a month of v4 swaps from the
call to now, or a graduation search that is still widening:
```
price-check 0x…: scanning blocks 54660000 → 81940000: 37% (at 64754000, 1234 events so far, 200000-block ranges)
price-check 0x…: pons graduation of 0x…: searched blocks 54087000 → 55233000 around the call block 54660000 (1% of blocks 1 → 81940000), not found yet
```

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
| `SCOUT_PRICE_LOOKBACK_BLOCKS` | `8640000` | how far back (~10 days) to look for the last feed update, ETH swap, or trade in an asset's own USD pool (a pool with no trade that recent is not used for that block) |
| `SCOUT_RPC_RPS` | `0` | max RPC requests per second (`0` = no limit, for your own node; set a number for a shared or public endpoint) |
| `SCOUT_RPC_LOG_CHUNK` | `200000` | most blocks per `eth_getLogs`. Every scan starts at this size. When the node refuses a range as too large (too many blocks or results, or an answer over 64 MB), or the range times out twice, that range is asked again in halves, for that scan only (not below 200 blocks); the scan grows back to this size after 3 answered ranges. Rate limits and busy answers are retried at the same size and never make ranges smaller |
| `SCOUT_MAINNET_RPC_URL` | none | Ethereum mainnet **archive** node; enables ETH/USD from mainnet Chainlink |
| `SCOUT_MAINNET_CHAINLINK_FEEDS` | `eth=` ETH/USD feed | extra `token=feedOnEthereum` mappings; the asset database's mainnet feeds are added (these win on a conflict) |
| `SCOUT_MAINNET_RPC_RPS` | `0` | max requests per second to the Ethereum node (`0` = no limit) |
| `SCOUT_CHAINLINK_FEEDS` | none | feeds **on Robinhood Chain**: `token=feed,…`; use `eth` for WETH/native ETH; the asset database's feeds are added (these win on a conflict) |
| `SCOUT_STABLES` | USDG | tokens worth $1 |
| `SCOUT_WETH`, `SCOUT_V4_POOL_MANAGER`, `SCOUT_ETH_USD_POOL` | Robinhood Chain addresses | override if needed |
| `SCOUT_DISCOVERY_BLOCKS` | `18000` | ± blocks around the call searched for the token's transfers (widened automatically) |
| `SCOUT_PONS_FACTORY` | `0x7eD598Bc…01EC7e` | Pons V2 launch factory; a token's curve must name it (`off` = Pons tokens not recognised) |
| `SCOUT_PONS_HOOK` | `0xE5e70264…6Be044` | hook of graduated Pons v4 pools (`off` = any hook) |
| `SCOUT_RUG_LIQ_USD` | `500` | rugged when the USD value of the pool's **quote side** (ETH/WETH, USDG, stock token …) is below this; settable in `.env`. `0` = USD check off, but an empty pool still counts. Negative values are rejected |
| `SCOUT_TRACK_INTERVAL` | `1m` | how often due checks are processed |
| `SCOUT_TRACK_WORKERS` | `8` | calls tracked at the same time (on-chain source); a free worker takes the next due call at once (see "What the tracker logs"); also sizes the database pool (the tracker's worker count + 4, between 4 and 32; see "Recording to a SQL database (Postgres)") |
| `SCOUT_RPC_PARALLEL` | `8` | block ranges of one scan fetched from the node at the same time |
| `SCOUT_RPC_MAX_INFLIGHT` | `64` | most requests in flight to the node at once (workers × ranges, capped here) |
| `SCOUT_RPC_LOG_CACHE` | `300000` | swap logs kept in memory so repeat calls of a token are not scanned twice (`0` = off) |
| `SCOUT_LATEST_REFRESH` | `on` | `off` = no latest-price pass (see below) |
| `SCOUT_LATEST_REFRESH_RECENT` | `15m` | how often the latest price of a call younger than 30 days is read again (at least `1m`) |
| `SCOUT_LATEST_REFRESH_OLD` | `24h` | the same for calls 30 days or older (at least `10m`) |
| `SCOUT_LATEST_BATCH` | `200` | latest prices read per tracker cycle at most (1 – 10000) |

Other commands: `-track` (tracker only, forever, no Telegram), `-track-once` (process what's due and exit).

### Retry calls without a pool now (`-retry-no-pool`)

A call whose pool was not found is `no_pool` and is tried again every 6 h by itself; once it
is past its last horizon + 48 h a failed try makes it `gave_up`, and it is never tried again.
After pool discovery learns something new (e.g. Pons V2 bonding curves, now supported), make
the tracker try those calls right away:

```bash
./scoutanalytics -retry-no-pool -retry-dry-run                                    # look first: what would be reset
./scoutanalytics -retry-no-pool -retry-gave-up -retry-launchpad pons_v2 -retry-dry-run
./scoutanalytics -retry-no-pool -retry-gave-up -retry-launchpad pons_v2           # do it
```
```
retry: first calls with status no_pool + gave_up, launchpad or dex = ponsv2
retry: 3 call(s) to reset to pending — no_pool 2, gave_up 1
  launchpad            dex                  status      calls
  Other                PONS_v2              no_pool         1
  Pons V2              -                    no_pool         1
  pons v2              -                    gave_up         1
retry: 3 row(s) updated; a running tracker (-track or the listener) picks them up on its next cycle, or run -track-once
```
It needs only the database (no Telegram, no node) and exits. The summary (by status and by
the post's launchpad / DEX) is printed before anything changes; the reset uses the same
selection, in one transaction. A running `-track` (or listener) picks the rows up on its
next cycle (within `SCOUT_TRACK_INTERVAL`, or as soon as a worker is free while it is busy); no restart needed.

- **`-retry-no-pool`**: first calls with tracking status `no_pool` → `pending`, due now,
  `attempts` 0, error cleared. Their priority is kept, so live calls still go first.
- **`-retry-gave-up`**: also `gave_up` calls. A `gave_up` call without an entry price also
  loses its saved pool and on-chain state, so the next check starts with a fresh pool
  discovery (a call that gave up on a pool without trades would otherwise read the same pool
  again). Each reset call gets one real try; an old call that still has no pool or no trades
  is `gave_up` again right after it.
- **`-retry-launchpad pons_v2,longxyz`**: only calls whose `launchpad` or `dex` (as parsed
  from the post) is one of these. Compared ignoring case, spaces and punctuation, so
  `pons_v2` matches `Pons V2` and `PONS-V2`, but not `Pons`: run the dry run without a
  filter first to see the values as stored.
- **`-retry-dry-run`**: print the summary only.

Never touched: `repeat` rows, update posts, later calls of a token, and rows that are
`pending`, `tracking`, `done` or `error`. Nothing is tracked again that is `done`.

**Node load:** every reset call costs a pool discovery — a few seconds of node time and a
few `eth_getLogs`, more for a call that still has no pool (its search window is widened
up to four times before it gives up) — plus the normal scan if a pool is found. Thousands of
reset calls are worked off 50 per tracker cycle (`SCOUT_TRACK_WORKERS` at a time), so they
spread over many cycles; use `-retry-launchpad` to reset only the calls the new support helps.
While those full batches last, the latest-price pass refreshes only calls younger than 30
days (see "Latest price"), so older calls' latest prices wait until the backlog is done.
If a row was being worked on by the running tracker at that moment, the tracker may save it
back as `no_pool`; run the command again for those.

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
| Dollar prices for every pair | `scout_call_tracking.price_unit` | A quote asset without a Chainlink feed (VIRTUAL, a stock token) is priced from its own WETH/ETH or stablecoin pools (per block, the one that traded last). It stays in quote units only if no such pool exists. |
| Realistic entry | `scout_call_tracking.entry_late_price_usd`, `scout_call_returns.*_late_pct` | The pool price `SCOUT_ENTRY_DELAY` (default `60s`) after the post, and return / peak / drawdown measured from it. |
| Price path | `scout_call_candles` | 5-minute candles for the first 24 hours, hourly candles for the whole window. Only buckets with trades. |
| Trading before the call | `scout_call_precall` | Swaps, buys, sells, volume and price change in the 5, 15 and 60 minutes before the post. |
| Peaks and lows | `scout_call_returns.max_gain_pct`, `max_drawdown_pct` | Each trade is valued at its own hour's ETH (or quote asset) price, not the price at the horizon. |
| Timing (outcomes) | `scout_call_returns.peak_late_after_s`, `first_2x_after_s`, `above_2x_s`, `fall_below_2x_after_s`, `above_2x_censored`, `timing_at`; `scout_call_tracking.rug_at`, `rug_at_kind` | When the peak came, when 2x was first reached, how long it held, and when the pool was drained. See "Timing after the call". Never model inputs. |

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

### Timing after the call (`-backfill-timing`)

For each on-chain horizon that is `done`, the tracker also stores **when** things happened,
computed from the call's stored candles (no extra node request). These are outcomes, like
the returns: never model inputs. Seconds are counted from `entry_at` (the post); E is
`entry_late_price_usd` (the realistic entry).

| Column (`scout_call_returns`) | Meaning |
|---|---|
| `peak_late_after_s` | Start of the first bucket whose high is the horizon's late peak (E × (1 + `max_gain_late_pct`/100)). `0` when the peak is the entry itself (`max_gain_late_pct` = 0). When no candle reaches the stored peak, no timing is stored for the horizon (`timing_at` stays NULL, logged). |
| `first_2x_after_s` | Start of the first bucket whose high touched 2E. NULL = never reached 2×. |
| `above_2x_s` | Seconds a close was at or above 2E (see below). `0` = never reached 2×, or touched 2× but never closed at or above it. |
| `fall_below_2x_after_s` | First time, from the first 2× on, that a close was under 2E, or the rug time if the window ended with a rug while still at 2×. NULL = never reached 2×, or censored. |
| `above_2x_censored` | `true` = reached 2× and no close under 2E was seen by the end of the window (no rug): the hold was cut off by the horizon, not ended. This includes a first 2× in the bucket that holds the horizon's end (`above_2x_s` 0). `false` otherwise (also when 2× was never reached). |
| `timing_at` | When the timing was computed. NULL = not computed (stored before these columns existed; `-backfill-timing` fills it in). |

How it is measured:

- The price path: 5-minute candles before `entry_at` + 24 h, hourly candles after. The first
  bucket is clipped to start at the post; the hourly bucket that straddles `entry_at` + 24 h
  counts from `entry_at` + 24 h. Resolution: 5 minutes on day one, 1 hour after.
- The window is from the post to the horizon's end, or to the rug when the pool was drained
  before it (`rug_at`).
- "Reached 2×" means the price touched 2E (a candle high). It counts only when the horizon's
  stored late peak is at least 2E, because candles also hold the trades between the post and
  the late entry. Buckets that end before the late entry are not used.
- The bucket that holds the late entry (`entry_at` + `SCOUT_ENTRY_DELAY`) counts from the late
  entry, and its high can come from a trade before it. It is used for the peak and the first
  2× only when no later bucket matches. In that case the stored late peak, which counts only
  trades after the late entry, shows that the hit came after it. When the peak is found in
  that bucket, the first 2× is there too. When a later bucket also reaches 2×, the first 2×
  and the time held are counted from that later bucket, even if the late-entry bucket may
  have had a 2× trade after the late entry (conservative: candles cannot tell).
- Time at 2× is measured on closes. From the first 2× bucket on, each bucket's close counts
  from the end of the bucket and is carried forward across buckets without trades. A bucket
  that touched 2× but closed under it adds nothing, so a touch and drop gives
  `above_2x_s` = 0 with `first_2x_after_s` set. The close of the bucket that holds the late
  entry only counts when that bucket is the first 2× bucket, and then it is a price after the
  late entry. Closes after the window's end are not used, so a bucket that straddles the
  horizon's end gives the same result whether or not it holds later trades. If the first 2×
  bucket itself ends after the window's end, nothing is held.
- The fall below 2× is the end of the first bucket (from the first 2× bucket on) that closes
  under 2E.

| Column (`scout_call_tracking`) | Meaning |
|---|---|
| `rug_at` | When the pool was drained: the exact time of the rug block (one node lookup per rugged call). NULL = not rugged, or rugged without a rug block (e.g. the 5%-of-entry rule). |
| `rug_at_kind` | `event`: a price event in the horizon scan or the latest-price pass showed the pool drained. `at_call`: drained at or before the call (its time can be before `entry_at`). `detected`: found only by the end-of-tracking liquidity check; the time is that check's. Keep the three apart in analyses. |

New calls get all of this from the tracker. For calls tracked before, run once (needs the
database; the node only for the rug times):

> **Deploy first.** Put the new binary on every process that tracks (the listener and any
> `-track`) **before** any re-track or `-retry-no-pool`. A binary from before this change
> re-stores a re-tracked horizon without touching the timing columns and does not clear
> `rug_at`. The old timing and rug time then stay with `timing_at` set, and `-backfill-timing`
> does not repair them because it selects only `timing_at IS NULL`.
>
> To find rows an old binary re-stored after their timing was computed (`computed_at` moves on
> every store, `timing_at` only with the timing; the minute is a margin):
>
> ```sql
> -- read-only
> SELECT call_id, horizon, computed_at, timing_at FROM scout_call_returns
>  WHERE timing_at IS NOT NULL AND computed_at > timing_at + interval '1 minute';
> SELECT call_id, rug_at, rug_at_kind, onchain->>'rug_block' AS rug_block FROM scout_call_tracking
>  WHERE rug_at IS NOT NULL
>    AND (COALESCE((onchain->>'rug_block')::numeric, 0) = 0
>         OR call_id IN (SELECT call_id FROM scout_call_returns
>                         WHERE timing_at IS NOT NULL AND computed_at > timing_at + interval '1 minute'));
> ```
>
> PROPOSED repair (not run; the owner decides). Run it after the re-tracks are finished, rug
> times first because they use the same condition, then `-backfill-timing`:
>
> ```sql
> UPDATE scout_call_tracking SET rug_at = NULL, rug_at_kind = NULL
>  WHERE rug_at IS NOT NULL
>    AND (COALESCE((onchain->>'rug_block')::numeric, 0) = 0
>         OR call_id IN (SELECT call_id FROM scout_call_returns
>                         WHERE timing_at IS NOT NULL AND computed_at > timing_at + interval '1 minute'));
> UPDATE scout_call_returns SET peak_late_after_s = NULL, first_2x_after_s = NULL, above_2x_s = NULL,
>        fall_below_2x_after_s = NULL, above_2x_censored = NULL, timing_at = NULL
>  WHERE timing_at IS NOT NULL AND computed_at > timing_at + interval '1 minute';
> ```
>
> Expect few rows. If nearly every row matches, check the clock difference between the
> tracker's host (`timing_at`) and the database host (`computed_at`) first.
>
> Limits: a re-track that is still running (or a row whose on-chain state is reset but not yet
> tracked again) is not found until its horizons are stored. A rug time is only caught when
> the state has no rug block, or when a horizon of the same call was re-stored. A re-track
> that found a different rug block without re-storing any horizon is not found.

```bash
./scoutanalytics -backfill-timing -dry-run   # count only: horizons to compute, node lookups it would make
./scoutanalytics -backfill-timing            # do it
```
Example dry-run output (the numbers are only an illustration):
```
backfill-timing (dry run): 41210 horizon(s) of 9874 call(s) to compute from the stored candles
backfill-timing (dry run): 312 rugged call(s) without a rug time: 312 node lookup(s) (eth_getBlockByNumber, one per call)
backfill-timing (dry run): 290 rugged horizon(s) wait for those lookups; 0 rugged horizon(s) have no rug block and would be skipped
```

- It selects horizons with status `done` and `timing_at` NULL, with a late entry and late
  peak, on-chain state version 2 or later, and the horizon marked done in that state.
- Rug times are looked up first (one `eth_getBlockByNumber` per rugged call without
  `rug_at`, one at a time, paced by `SCOUT_RPC_RPS`). Then the timing is computed from the
  candles, 200 calls per batch.
- Without a node it does not stop. When 3 rug-time lookups in a row fail, at any point of
  the step (node unreachable; each failed lookup is retried for several seconds), the rest
  of the rug-time step is skipped; a lookup that succeeds starts the count again, and a
  failure to store a rug time (database) does not count. The other horizons are still filled
  in. The rugged horizons of calls without a rug time are skipped and counted, and a later
  run with the node fills them in. `SCOUT_RPC_URL` defaults to `http://localhost:8540`, so
  "no node" means that address (or the one set) does not answer.
- It writes only the new columns. Status, schedules, on-chain state, returns, candles and
  `updated_at` are not touched, and nothing is tracked again. A row that got its timing or
  rug time in the meantime (e.g. from a running tracker) is left as it is, so running the
  command again is harmless. A second run only tries again the horizons the first one skipped
  (see below); when nothing was skipped, it finds nothing to do.
- The kind of a past rug is read from the stored state: the rug block at or before the call
  block is `at_call`; a rug block that a scan cursor has passed is `event`; otherwise it is
  `detected`.
- The late entry is taken as `SCOUT_ENTRY_DELAY` after the post, as currently set.
- Skipped and counted: rugged horizons whose rug time could not be found, and horizons whose
  candles do not reach the stored late peak (or its 2×) (logged per call). Those keep
  `timing_at` NULL, so a later run tries them again. The tracker does the same: when the rug
  block's time cannot be read, it asks the node once per call and run, stores the rug and
  the horizons, and leaves the rugged horizons' timing to `-backfill-timing`.

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

**Blocks read once where it is safe.** A refresh starts after the last block either scan has
covered (its own cursor or the horizon scan block, whichever is further). A call whose
horizon check is due is left to the horizon scan, which is about to read those blocks: the
pass skips it (no node request) until that check is done, or for 10 minutes at most, and
then reads only past the new scan block. Blocks the pass reads between horizons are read
again by the horizon scan when the next horizon is due: the horizon scan needs every event
for its candles and running peak/low (valued at each hour's USD rate), which the pass does
not keep, so horizon results are exactly the same with or without the pass.

**Schedule.** A call is due when it has no latest price yet, or the last one is older than
`SCOUT_LATEST_REFRESH_RECENT` (15 minutes; calls younger than 30 days) or
`SCOUT_LATEST_REFRESH_OLD` (a day; calls 30 days or older). The pass runs in every tracker
cycle **after** the horizon checks have been handed out, through the same workers
(`SCOUT_TRACK_WORKERS`; a call is never with two workers at once), and takes at most `SCOUT_LATEST_BATCH` calls: those younger than 30 days first, then the ones
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
tracking: 12+ call(s) due now
call 10126 [#1]: 0x129b…, posted 2026-09-28 14:02 (70h ago), status pending
call 10126 [#1]: pool found: uniswap-v3 0x…, paired with WETH (entry block 21300412)
call 10126 [#1]: entry price $0.0031 (1.03e-06 WETH × $3010, Chainlink on Ethereum mainnet)
call 10126 [#1]: +1h → 0.0052 (+67.7%), peak +120.4%, low -8.1%
call 10126 [#1]: scanning blocks 21726610 → 22164412: 46% (at 21926609, 12 events so far, 200000-block ranges)
call 10126 [#1]: tracking in 14s, 212 RPC requests — next check 2026-10-01 14:12
tracking: processed 47 call(s), 3 in progress — pending 112, tracking 37, done 4, repeat 3120; more due now
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

**Rolling queue.** The tracker reads due calls a few at a time (one per worker, at least 4;
live calls first, then the longest due) and hands each to a worker as soon as one is free,
so one slow call holds up only its own worker. A cycle hands out up to 50 calls and does
not wait for them: `due now` counts the first read (`12+` = more were waiting),
`[#n]` numbers the calls handed out since the start, `processed` counts the calls finished
since the last status line, and `in progress` the ones still running. A call is never
handed out twice at the same time. Ctrl+C stops the hand-outs; the calls in progress stop
(keeping what they had scanned, with status and schedule as before) and the tracker exits
once they have.

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
call 9163 [#137]: eth_getLogs blocks 52000000-52199999 (200000 blocks) refused as too large (rpc error -32000: query returned more than 10000 results); this scan continues with 100000-block ranges (max 200000)
call 8120 [#112]: eth_getLogs blocks 31000000-31199999 (200000 blocks) timed out on the node (2 tries) (rpc error -32000: request timed out); this scan continues with 100000-block ranges (max 200000)
call 9163 [#137]: eth_getLogs ranges back up to 200000 blocks (max 200000) after 3 answered ranges [also 4 range split(s) and 2 grow-back(s) in all scans since the last such line]
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

**Ops scripts.** `ops/` holds one-off SQL scripts run by hand (see each folder's README).

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

Not sure of the name or id? `go run . -list-chats` prints
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
SCOUT_CATCHUP_MAX=100    # after a restart: missed posts (and calls still queued) handled live, newest first, 0-250; older ones stored only
SCOUT_CATCHUP_MAX_AGE=24h # missed posts / queued calls older than this are stored only (0 = no age limit; invalid = startup error)
SCOUT_RESCAN=off         # Perceptor re-scans of first calls without a report; SCOUT_RESCAN_* in "Re-scanning calls without a Perceptor report"
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
| `_NEEDS_TEXT` | false | true | only a reply with text (a letter or digit) finishes the report; a photo/media-only reply is kept but does not |
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

**sAlpha** sends a photo (no caption) first and its text report later, often more than
the 15 s settle time later. With `_NEEDS_TEXT=true` (sAlpha's default) a reply without
text (photo, document or any other media, or invisible characters only) does not finish the
report: the settle timer starts only when a text reply arrives (the report, or a decline
such as "There is not enough public information to write a report on this token."). The
media reply is kept: its id is stored with the text's and it is forwarded with the report.
If no text arrives within `_MAX_WAIT` (300 s), the result is `timeout` (not forwarded,
"no report (timeout)" in the header), with the media types in the error, e.g.
`@salpha_research_bot sent only media (messageMediaPhoto) and no report text within 5m0s`.
The CA's delivery waits for this (it waits for every tool), so a media-only reply holds
the live queue for up to `_MAX_WAIT`. Before this rule, an sAlpha result could complete on
a reply without text and be stored with empty text.

`scout_investigations.details.replies` lists every stored reply (placeholders excluded):
`message_id`; `date`, when Telegram dated it (unix seconds); `edit_date`, when it was last
edited (unix seconds, left out when never edited); `reply_to_msg_id`, the message it
replies to (left out when it replies to none); `media`, its media type
(`"messageMediaPhoto"`, `""` for none); and `text_len`, its text length in characters.
The dates give sAlpha's gap between its photo and its text (to tune `_MAX_WAIT`), and
`reply_to_msg_id` shows whether a reply names the request it answers (a late reply could
otherwise be taken for the next CA's). `details.media` lists media of any type (not only
photos and documents).

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

### Re-scanning calls without a Perceptor report

> **Do not set `SCOUT_RESCAN=on` in prod before the website's "Perceptor today" label is
> deployed.** A website older than the label has no place for the re-scan verdict (the API
> serves it as `perceptor_today_*`, see "API"); the label is described under "Website",
> "Perceptor today".

Most first calls imported from history, and live calls whose scan failed, have no Perceptor
report. The **rescan lane** asks Perceptor about them later, one at a time, inside the
listener (`-listen-only` or the full listener): the same Telegram session, never a second
process. It is **off by default**.

```
SCOUT_RESCAN=off                  # on = run the lane in the listener
SCOUT_RESCAN_MAX_AGE=720h         # only first calls posted within this (at least 1h)
SCOUT_RESCAN_MAX_PER_DAY=100      # re-scans requested in any rolling 24 hours (1-10000)
SCOUT_RESCAN_GAP=10m              # at least this between two re-scan requests
SCOUT_RESCAN_IDLE=5m              # only after the live queue has been idle this long
SCOUT_RESCAN_STATUSES=backfill,duplicate,failed,scanned   # scout_calls.status of the first call (queued/dropped belong to the requeue)
```

An invalid value stops every mode at startup (the settings are read by all of them, so
also `-track` and `-web` sharing the same `.env`) with an error naming the setting.

**Which calls (candidates).** All of these, newest first (ties: the higher call id):

- the token's **first call** (the tracker's and the website's rule: lowest `message_date`,
  then lowest id, per contract address with upper/lower case ignored; update posts are never
  a first call), posted within `SCOUT_RESCAN_MAX_AGE`, with a status in
  `SCOUT_RESCAN_STATUSES`;
- the token has **no completed Perceptor report of either kind** (live or re-scan, any post
  of the token);
- **not rugged** (`scout_call_tracking.rugged`), and `latest_return_pct` is `NULL` (no latest
  price yet) or above −99;
- **fewer than 2 failed re-scans** of the token (`failed` or `timeout`; a `rate_limited` one
  does not count). After the second failure the token is not tried again.

**How it runs.** Live calls always come first. The scan worker looks at the lane only when
the live queue is empty, the last live call finished at least `SCOUT_RESCAN_IDLE` ago (a
start or reconnect counts as a live call), the newest re-scan was requested at least
`SCOUT_RESCAN_GAP` ago and fewer than `SCOUT_RESCAN_MAX_PER_DAY` re-scans were requested in
the last 24 hours. The cap and the gap are read from the database, so they hold across
restarts. A re-scan uses the same Perceptor runner as live calls, so the account's pacing
(`SCOUT_TOOL_PERCEPTOR_MIN_INTERVAL`, 2m5s) is shared. It runs synchronously in the worker: a
live call that arrives meanwhile waits for it, at most about 5 minutes (Perceptor's 180 s
`_MAX_WAIT`, the report page, then the pacing). A re-scan is not retried on a rate-limit
reply: it is stored as `rate_limited` and the lane pauses for an hour; a Telegram
`FLOOD_WAIT` stores nothing and pauses the lane for an hour or the wait Telegram asks for,
whichever is longer. A stop during a re-scan stores nothing. With no candidate left, the lane
looks again every 10 minutes.

**What a re-scan writes.** One `scout_investigations` row (tool `perceptor`, `scan_kind =
'rescan'`, `call_id` = the first call, `details` with `"rescan": true` and `call_age_s`), and
nothing else: no delivery, no change to `scout_calls.status`, `seen_cas.json`, tracking or
model scores. sAlpha is never asked. `-scan`/`-post` are not used for this: they deliver,
overwrite the status and would make today's verdict look like one from the time of the call.

**No leakage into the model.** A re-scan's verdict is today's, not one known when the call
was made. `scout_call_dataset_v` (and so `-export-dataset`, the training data and the model's
scoring row), the requeue's "already investigated" check, the website's `perceptor_verdict`
/ Perceptor filter / `GET /api/call`, and the sAlpha lookup all read `scan_kind = 'live'` rows
only. The website shows a re-scan only as `perceptor_today_*`.

**Dry run** (database only: no Telegram login, no node; nothing is scanned, and no call,
investigation or delivery is written):

```
go run . -rescan-missing -dry-run
```

It prints the settings, how many first calls without a Perceptor report have an allowed
status, how many of those each rule excludes (too old, rugged, latest return ≤ −99%, failed
twice; each counted once, in that order), the number of candidates by status and by age, an
estimate of the days needed at the current cap and gap, and then every candidate in the order
the lane takes them (call id, posted, age, status, contract address). It reads
`SCOUT_RESCAN_*` from the environment / `.env` like the listener, so a setting can be tried
first, e.g. `SCOUT_RESCAN_MAX_AGE=168h go run . -rescan-missing -dry-run`. Like every mode, it
applies `scoutanalytics.sql` first unless `SCOUT_DB_AUTO_MIGRATE=false` (which adds the
`scan_kind` column if it is missing and recreates the views, the same as the next listener
start would), so on prod run it only as part of a deploy (DEPLOY.md 3.4), never while
processes of an older version are running.
`-rescan-missing` without `-dry-run` scans nothing; it prints a pointer to this section.

The lane logs one line when it starts (`rescan: on — …`), one per re-scan (`rescan: call
<id> (<CA>, 12d old): clean (3 of 100 today)`), and once each when it reaches the cap or runs
out of candidates. Count failures with:

```sql
SELECT status, count(*) FROM scout_investigations WHERE scan_kind = 'rescan' GROUP BY 1;
```

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
scanner calls `database.SetupDatabase()` (in `internal/database`, copied from the API repo;
the only addition is the pool size, see below), so it uses the same settings from
`.env`: `DB_USER`, `DB_PASS`, `DB_NAME_DEV`, `APP_ENV`, and `GETH_HOST_PATH` (LOCAL_GETH) or
`HOST_SECRET_PATH` + `SSL_CERT_FILE_PATH` (Cloud SQL). Nothing extra to configure.

The startup log confirms where it writes, e.g.
`recording to SQL via database.SetupDatabase (DB_USER, APP_ENV=…) → database "assetdb", schema "public", user "…", pool of at most 16 connections (default for 12 tracker worker(s))`.
The tables are created in that database's `public` schema on first start.

Pool size: each process opens at most the tracker's worker count (`SCOUT_TRACK_WORKERS` with
the on-chain source, 1 with GeckoTerminal) + 4 connections, between 4 and 32 (on-chain: 12
with the default 8 workers, 16 with 12; GeckoTerminal: 5). Connections are opened only when
needed, so the listener and the website use far fewer than that. With `SCOUT_DATABASE_URL`,
`pool_max_conns` in the DSN (e.g. `…?pool_max_conns=10` or `… pool_max_conns=10`) overrides
it. The website's live-update `LISTEN` uses one more connection outside the pool.

```
SCOUT_DB=repo                # default: the repo's database package
# SCOUT_DB=off               # run without recording
# SCOUT_DATABASE_URL="host=… port=5432 user=… password=… dbname=… sslmode=disable"   # optional override; add pool_max_conns=N to set the pool size
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
| `scout_investigations` | (CA, tool) request | `call_id`, `tool_id`, `request_text`, `requested_at`, `completed_at`, `status` (`completed`/`failed`/`timeout`/`rate_limited`), `bot_message_ids`, `report_text`, `report_urls`, `report_url`, `external_id`, `verdict_level`, `verdict_label`, `ticker`, `verdict_summary`, `details` (JSONB: attempts, rate-limit waits, files, photos, buttons), `error`, `scan_kind` (`live` = scanned when the call came in or by `-scan`/`-post`; `rescan` = a late Perceptor re-scan by the rescan lane, today's verdict: never a call-time feature, so the dataset view, the requeue and the website's verdict read `live` rows only) |
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
FROM scout_calls c JOIN scout_investigations_v v ON v.call_id = c.id AND v.scan_kind = 'live'
GROUP BY c.id ORDER BY c.message_date DESC LIMIT 50;

-- sAlpha reports for CAs Perceptor passed
SELECT s.contract_address, s.report_text
FROM scout_investigations_v s JOIN scout_investigations_v p ON p.call_id = s.call_id
WHERE s.tool = 'salpha' AND p.tool = 'perceptor' AND p.scan_kind = 'live'
  AND p.verdict_level IN ('clean','caution');

-- calls with their post data and Perceptor verdict
SELECT c.message_date, c.token_symbol, c.mcap_usd, c.liq_usd, c.holders, c.proof_elite, c.proof_good,
       v.verdict_level AS perceptor
FROM scout_calls_v c LEFT JOIN scout_investigations_v v ON v.call_id = c.call_id AND v.tool = 'perceptor'
                                                        AND v.scan_kind = 'live'  -- 'rescan' = today's verdict
ORDER BY c.message_date DESC LIMIT 50;

-- elite vs good live-buy volume per call
SELECT call_id, tier, count(*) buys, sum(amount_usd) usd FROM scout_call_live_buys GROUP BY 1,2 ORDER BY 1 DESC;

-- tool reliability (timeouts / failures) per day
SELECT date_trunc('day', requested_at) d, tool, status, count(*)
FROM scout_investigations_v GROUP BY 1,2,3 ORDER BY 1 DESC, 2;
```

DB integration tests (use a THROWAWAY database; they drop and recreate the scout_* tables):

```
SCOUT_TEST_DATABASE_URL=postgres://postgres@localhost:5432/scout_test?sslmode=disable go test -p 1 .
```

## Website

A small read-only page that shows how far the import/tracking is and lists every token the
channel called, with the performance of its first call. It runs as **its own process**, next to the listener and the tracker, and
needs only the database (no Telegram login, same as `-track`):

```bash
./scoutanalytics -web          # or: go run . -web
# → website on http://[::]:8090 …   open http://<this machine>:8090/
```

| Setting | Default | |
|---|---|---|
| `SCOUT_WEB_ADDR` | `:8090` | address to listen on. `:8090` = every network interface; `127.0.0.1:8090` = this machine only |
| `SCOUT_GMGN_URL` | `https://gmgn.ai/robinhood/token/{ca}` | link behind the token name / symbol; `{ca}` is replaced by the contract address |
| `SCOUT_WEB_DIR` | *(empty)* | serve the page from this folder instead of the copy built into the program (edit `frontend/` without rebuilding) |
| `SCOUT_WEB_REFRESH` | `15s` | how often the website reads the list again from the database (a duration such as `10s` or `1m`; at least `2s`, a smaller value is raised to `2s`, an unreadable one falls back to `15s`, both with a warning) |

**It is read-only and has no login.** The server only answers `GET` (anything else → 405),
with one exception: `POST /api/refresh`, the "Refresh now" button, which makes the website read
the database at once (only `POST` there; see the API below). It only runs `SELECT`s; anyone
who can reach the address can see the calls and press the button. Put it behind your own
firewall / reverse proxy, or bind it to `127.0.0.1`, if that is not what you want. Like every
other mode it applies the schema at startup (`SCOUT_DB_AUTO_MIGRATE`), and it reads the same
`.env` (so `API_ID` / `API_HASH` must be present, although no Telegram connection is made).

**Live updates need nothing new**: no setting, table or trigger. The listener (any mode that
records calls) runs `SELECT pg_notify('scout_events', '{"kind":"call","call_id":812}')` after
storing a new real call (not an update post, not a post stored again, not a call imported by
`-backfill`), and
`{"kind":"report","call_id":812,"tool":"perceptor","id":5120,"scan_kind":"live"}` after storing
a completed Perceptor or sAlpha report (`"scan_kind":"rescan"` for a re-scan by the rescan
lane). A failed `NOTIFY` never fails the insert; it is logged (at most
one line a minute) and the page then shows the row with the next regular refresh. You can
watch them with `LISTEN scout_events;` in psql. `-web` opens **one extra database connection**
for `LISTEN`, outside its pool (so it holds one more connection than before). If that
connection cannot be opened or breaks, it logs
`web: live updates: cannot listen for database events: … — retrying` (at most once a minute),
tries again with a growing wait (1 s up to 1 minute), and meanwhile the page still gets the
new rows, only as late as the regular refresh (`SCOUT_WEB_REFRESH`). After every
(re)connection it reads the list once, since notifications sent while it was away are lost.
Notifications less than a second apart make one read, and that read is the same single read
as the background refresh and **Refresh now** (they share it; never two at a time).

**Behind a reverse proxy**: pass the `Host` header through unchanged (the same-site check of
`POST /api/refresh` and `GET /api/events`), do not buffer `GET /api/events` (the website sends
`X-Accel-Buffering: no`, which nginx honours; for others turn response buffering off for that
path), and keep the proxy's read timeout above 25 seconds (the stream sends a keep-alive line
every 25 seconds).

**Speed: the website answers from memory.** When it starts, the website reads the whole
list once (one row per token, with the numbers of all five windows, plus the counts of the
progress panel) and keeps it in memory as a *snapshot*. Every request for the list or the
counts is answered from that snapshot: search, filter, sort and paging happen in memory and
**no request waits for the database**. In the background the website reads the list again
every `SCOUT_WEB_REFRESH` (15 seconds by default) and swaps the new snapshot in at once;
requests under way finish on the old one. The **Refresh now** button on the page does the same
read on demand (see below).

- **What you see can be up to `SCOUT_WEB_REFRESH` old** (plus the fraction of a second the
  read takes). A result written by the tracker shows on the page after the next refresh, or at
  once after **Refresh now**. A **new call or a new Perceptor / sAlpha report** shows within
  about a second (see "Live updates" below): the listener sends a Postgres `NOTIFY
  scout_events` when it stores one, and the website, which `LISTEN`s, reads the list at once. "Updated hh:mm:ss" in the progress panel is the time the
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
  It names, per token, the Perceptor and sAlpha reports the row detail shows (their ids). The
  texts of those reports are read by a second, small query **only for report ids the website
  does not hold yet** (none when no report arrived since the last read); see the row detail
  below.
- **The Analytics page** adds to that statement the posted DEX, the price source, the quote
  asset, which windows are stored as `no_data`, the Perceptor verdict at the time of the call
  and whether the first 24 hours of candles are stored (about +20 ms on 6,000 tokens), and the
  values known at the call that the "By factor" tab groups by: the post's holders, elite and good
  holders, elite and good live buys (`scout_call_metrics`) and the hour before the call
  (`scout_call_precall`, joined by its primary key), with the expressions of
  `scout_call_dataset_v` (about +15–25 ms on 5,600 synthetic tokens on the test machine). The trade
  counts of the first 24 hours (`scout_call_candles`) are read separately and kept: a refresh
  reads only calls whose count has just become complete (usually none; a few ms), and all of
  them again once an hour (about 0.3 s for 5,600 calls and 1.4 million 5-minute candles on the
  test machine), in case a call was tracked again. If that read fails, the refresh still
  succeeds: the counts already held stay (and are shown), calls not counted yet show as
  unknown, the failure is logged at most once a minute (`webErrorLogEvery`), and the next
  refresh tries again.
- **Memory**: about 1 KB per token for the snapshot (about 10 MB for 12,000 tokens, about
  90 MB for 100,000; the five window returns in every row added about 110 bytes a token), on top of the program itself; while a refresh runs, the rows just read
  are in memory next to it for a moment. The whole process measured 40–55 MB with 12,000
  tokens (peak 78 MB under a load test). The row detail adds about 85 bytes a token to the
  snapshot (two report ids and three fields in every row: about 1 MB for 12,000 tokens) plus
  the report texts, about 300 bytes per report held: with the current data (about 340 sAlpha
  replies, half of them empty, of 33 characters on average and 1,035 at most, and a few
  hundred Perceptor summaries of a line each) well under 1 MB. Each text is kept up to 32 KB.

**Caching and compression** (nothing to set up):

- Answers of `/api/calls` and `/api/summary` carry an `ETag` made from the content of the
  snapshot and the question asked. The page sends it back with its 30-second refresh
  (`If-None-Match`); when nothing changed the server answers `304 Not Modified` with no body
  and the page leaves the table as it is. A refresh that finds the same data in the database
  keeps the same `ETag`, so an idle site costs a few hundred bytes per open tab per refresh.
  The `ETag` of `/api/summary` follows the counts only, so it stays "not modified" while rows
  change but the counts do not. With `days` (the age filter) the `ETag` of `/api/calls` also
  changes when a call drops out of the window as time goes on, even if the data did not.
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
- **Refresh now** (next to "Updated") makes the website read the database at once — the same
  read the background loop does every `SCOUT_WEB_REFRESH` — and then reloads the progress and
  the list. The button is disabled while it works. A press shortly after another one (within
  5 seconds of the end of the read it caused) does not read again: the page says "Pressed less
  than 5 seconds ago; showing the data read at hh:mm:ss" and shows that data. Presses from several tabs at the same
  moment, or while the background loop is reading, share one read. If the database cannot be
  read (or does not answer within 15 seconds) the page shows a short notice, "Could not
  refresh: … Still showing the data read at hh:mm:ss.", and keeps the rows it has.
- **Live updates** — the page keeps a stream open (`GET /api/events`, Server-Sent Events;
  "Live" in green next to "Updated" while it is open, "Live off: updates every 30 s" when it
  is not). Nothing to set up; the 30-second refresh keeps running in any case, so a page
  without the stream (refused, blocked by a proxy, an old browser) is at most 30 seconds behind.
  - **A new call** goes on top of the table, briefly highlighted, when the table shows the
    newest calls first (Date ▼) on page 1 and the token matches the search, the Perceptor
    filter and the "Calls from the last" choice. Otherwise a **"N new — refresh"** button appears above the table; it switches to
    the newest calls, page 1, and reloads.
  - **A new Perceptor verdict, or a new sAlpha report or decline**, updates that token's row
    in place (verdict, "sA" badge, an open detail panel). So does a **new or changed
    "Perceptor today"** (a re-scan; a `report` event with `"tool":"perceptor_today"`), but
    quietly: no notice, no beep and no desktop alert, since re-scans are about old calls
    (up to 100 a day) and would push the call-time notices out of the corner.
  - Each of the others also shows a **notice in the bottom-right corner**: token, verdict
    (or "sAlpha: report available" / "did not generate a report") and a GMGN link. At most
    4 are kept (2 on a narrow screen); each goes after a minute or with its × button.
  - **Sound** (a checkbox next to "Updated"): a short beep for each notice, made by the browser
    (Web Audio, no sound file). **Off by default**; the choice is kept in this browser
    (`localStorage`). Browsers only play sound after a click or key press on the page.
  - **Desktop alerts** (a button next to it): a system notification for each notice while
    the tab is in the background. The browser asks for permission on the first click.
    **Browsers allow this only on a secure page: `https://…` or `http://localhost` /
    `http://127.0.0.1`. On a plain `http://<server IP>:8090` address the button stays greyed
    out**; put the website behind an HTTPS reverse proxy to use it from another machine.
  - **Only calls posted within the last hour are announced** (notice, sound, desktop alert,
    "N new" count), by the browser's clock. An older call that appears in the list (a post
    imported by `-backfill`) is added quietly: the page reloads its list once per burst of such
    rows, without a notice. Calls stored by `-backfill` (status `backfill`) also send no
    `NOTIFY`, so a backfill does not make the website re-read the database for every row; they
    show up with its next regular refresh.
  - Many changes at once (more than 20 in one read, e.g. a `-backfill`) or a gap in the stream
    (the website restarted, or the page was away too long) make the page reload its list
    instead of adding rows one by one.
  - Over plain HTTP/1.1 a browser opens at most about 6 connections to one address, across
    all its tabs; every open tab of this page holds one for its stream. With many tabs of the
    page open in one browser, the other requests of those tabs can wait. HTTPS with HTTP/2
    (a reverse proxy) does not have this limit.
- **Reading the list.** Each row starts with what matters now: the token, **Latest %** (the
  return at the most recent price, as a badge coloured by size: rugged/−100%, down by half or
  more, down, flat, up to 2×, 2×–11×, 11× and more; every value but a flat `0.0%` carries its
  sign, so colour is never the only cue) and, under it, how long ago that price was read
  ("12m ago", "2d ago"; "just now" under a minute), counted to the time the website last read
  the database. **stale** = older than twice the tracker's prod schedule (30 min for calls
  under 30 days old, 2 h for older or rugged ones, which prod refreshes every hour with
  `SCOUT_LATEST_REFRESH_OLD=1h`; a drained pool is re-stamped at that same pace; fixed in
  `app.js` as `STALE_OLD_MS`, not read from `SCOUT_LATEST_REFRESH_*`; a tracker left on the
  24 h default makes most older and rugged calls show stale; raise `STALE_OLD_MS` to 2× the
  setting in that case), so the tracker may be stopped or behind; **quiet** = no trade in the
  7 days before the reading. The header row and the Token column stay in view while
  scrolling. ▸ (or a click on the row) opens the call's performance (entry and latest price,
  when the price was read and the last trade, the call's age, Call MC → Latest MC, peak and
  worst drop for the chosen window) and its Perceptor/sAlpha reports.
- **Calls from the last 1d / 7d / 30d / All.** Shows only calls posted within that many days
  (× 24 hours) of the snapshot time. It goes by the date of the row's (first) call: a token
  first called 60 days ago and again yesterday is not under 7d. Import progress is not
  filtered. Kept in the address (`?days=7`; the page takes only 1, 7
  and 30 from it, anything else shows All). The API takes `GET /api/calls?days=N` (whole days,
  1–3650; anything else is a 400; leave it out for all); the answer carries `"days"` (0 = all),
  and the total counts only the matching calls ("N calls from the last 7 days" under the
  table).
- **Calls** — 16 columns, in this order:
  Token (the name links to GMGN; the symbol follows in grey when it differs from the name) |
  Latest % | Latest MC | Call MC | Date (links to the post) | Perceptor | Status | Calls (`×N`
  when the token was called N > 1 times, empty otherwise; hover for "Called N times, last on
  …") | Entry $ | Peak % | Worst drop % | 1h | 1d | 3d | 7d | 30d.
  - **1h, 1d, 3d, 7d, 30d** are the return over each window (the number the old single
    "Return %" column showed for that window), all five side by side. A dash until the window
    has passed and been recorded.
  - **Peak % and Worst drop %** are for one window, picked with the small **"Peak / worst drop
    over"** selector (1h … 30d) above the table; their headers name it, e.g. "Peak % (7d)". The
    selector changes only these two columns.
  - **Sorting:** click Date, Call MC, Latest MC, Latest %, Peak % (for the selected window) or
    any of 1h … 30d; click again to reverse. ▲/▼ shows the column and direction (also as
    `aria-sort`). Rows without a value are always last; ties by call id. Worst drop % is not
    sortable.
  - Search by token name, symbol or address. 50 per page. The table scrolls inside its box: sideways
    on a narrow screen (the page itself does not), and down when it is taller than the window.
- **Row detail (performance and the token's reports)** — the **▸** button in the Token cell of
  every row opens a panel under the row; a click anywhere else on the row does the same,
  except on a link (the post, GMGN, the Perceptor report) or while selecting text. The button
  is a real button: Tab to it and press Enter or Space; it says whether the row is open
  (`aria-expanded`). The panel shows:
  - **Performance**, drawn from the row at once (the reports below load separately): when it
    was called (and how long ago), Latest % as a badge, entry price (60 s after the post),
    latest price, when that price was read (how long ago, "stale"/"quiet" as in the list, and
    how old the call was then), the last trade, Call MC → Latest MC, and Peak % and Worst drop
    % for the window chosen. A call not priced in USD shows only that it has no dollar price.
  - **Perceptor**: the verdict once (the report's own verdict line, e.g. "No red flags found",
    or the words "no red flags" / "caution" / "red flags" when it has none) and its time, the
    summary, and "Open the Perceptor report" (https links only);
    "No Perceptor report" when the token was never scanned. It is the same report the
    Perceptor column shows.
  - **Perceptor today (re-scan)**, only when the token has one: the re-scan's verdict in the
    same outlined label as the list ("today: caution"), its date, a line saying it is a
    re-scan made long after the call and not the verdict at call time, and "Open the re-scan
    report" (https links only). It comes from the row (`perceptor_today_*`), not from
    `GET /api/call`, and is redrawn with every refresh, like the performance block.
  - **sAlpha**: the text of the token's latest completed sAlpha report, as plain text with its
    line breaks, its time and "Open the sAlpha report" (https links only). **About half of
    sAlpha's replies are empty; an empty reply (or one of white space only) counts as no
    report**: the latest reply that has text is shown, whichever post of the token it was made
    for, and "No sAlpha report" when there is none. A reply that failed, timed out or was
    rate-limited never counts. **A reply that declines** ("Not enough public signals to
    generate a report for this token.", "There is not enough public information to write a
    report on this token.", "Too little liquidity or trading activity to research yet."; the
    phrase list is `salphaDeclinePhrases` in `websnapshot.go`: "not enough public" and "too
    little liquidity", matched anywhere in the text, upper/lower case ignored) **is not a
    report either**: an older real report is shown instead, however old; when the token has
    none, the panel says "sAlpha did not generate a report" with the reason and time in grey.
    Only a short reply counts as a decline: its text, with white space trimmed at both ends,
    has at most 300 characters (`salphaDeclineMaxLen`). A longer reply is a real report even
    when a risk line says "too little liquidity to exit". The website query and the Go check
    use the same phrases, limit and trimming, also for rows stored before this rule.
  - A text longer than 32 KB is cut there and marked "Cut at 32 KB."; a long one scrolls
    inside the panel. The panel is never wider than the visible part of the table box, also on
    a phone while the table is scrolled sideways.
  The reports are loaded when the panel is opened ("Loading…" until then; an error notice if
  they cannot be loaded, tried again with the next refresh); the performance block is redrawn
  from the row with every refresh. Open rows **stay open**, with their content,
  through the 30-second refresh, **Refresh now**, sorting, the window selector, searching and
  paging (a row that is on another page is open again when you come back to it). When a newer
  report arrives the panel is updated with the next refresh. If the row's call is no longer in
  the list (after a reset), the panel says "Not available in the data now shown; refresh the
  page."
- **sA** — a small "sA" badge next to the Perceptor verdict (or its dash) marks a token that
  has an sAlpha report with text (hover for a hint). An empty reply, or one that declines,
  gives no badge.
- **Rugged** — a call the tracker flagged `rugged` (`scout_call_tracking.rugged`, also while
  it is still tracking) shows a red "rugged" badge in Status instead of the tracking status
  (which is in its tooltip), a dash for **Peak %** in every window (the API sends
  `peak_pct: null`, and `sort=peak` lists it with the rows without a peak), and a dash for
  Latest MC (its latest price is 0). The returns are shown as stored (−100% for the windows
  after the rug).
- **Perceptor** — the column shows the verdict of the token's Perceptor report as words:
  "no red flags", "caution", "red flags" (linked to the report, in a new tab), or "–" when
  there is none. The "Perceptor" button next to the search box filters the list: it opens a
  small panel of checkboxes (No red flags, Caution, Red flags, Not scanned), and the list shows
  the tokens whose verdict is any of those ticked, e.g. No red flags **and** Caution. None
  ticked (or all four) = all; "All (clear)" unticks them. The button names the choice
  ("Perceptor: No red flags + Caution", "Perceptor: all"). It opens with Enter or Space, Esc
  or a click elsewhere closes it, and the choice is kept in the browser (localStorage) across
  reloads.
  **The verdict belongs to the token, not to the listed call:** it is the latest completed
  Perceptor report for that contract address (upper/lower case ignored), whichever post of
  the token it was made for. So a token whose first call was imported from history still
  shows the verdict of a later post that was scanned.
  **Calls imported from history have no report**: only calls the listener picks up live are
  sent to Perceptor, so most imported tokens show "–" and are found under "Not scanned".
  A scan that failed, timed out or was rate-limited does not count, and a report whose
  verdict could not be read counts as not scanned.
- **Perceptor today** — a token that had no Perceptor report at call time but was re-scanned
  later by the rescan lane (see "Re-scanning calls without a Perceptor report") shows, on a
  line of its own in its Perceptor cell (under the dash, or under the call-time verdict when
  there is one, and the "sA" badge), a small label:
  "today: no red flags", "today: caution", "today: red flags", or "today: no readable
  verdict". It is **not the verdict at call time**, so it looks different from the verdict
  pills: smaller, outlined with a dashed line, square corners, no fill, and always with
  "today:" in front. It links to the re-scan report (in a new tab, https only; plain text
  otherwise), and its tooltip says "Perceptor re-scan on <date>, not the verdict at call
  time". **The Perceptor filter and its counts ignore it**: such a token stays under "Not
  scanned". The legend under the table has an entry for it ("today: caution").
- Every number is **in USD and measured from the entry 60 seconds after the post** (the
  `*_late_*` columns). A call tracked in another asset (no USD source for its pair) shows
  "no USD price" in Latest %, a dash for Latest MC, Call MC and Entry $, and one dash across
  Peak % … 30d.
  Sorting by anything but Date lists the USD-priced calls only.
- **Latest %** (the column after "Token") is the return at the most recent price, from the
  same entry, as a coloured badge (see "Reading the list"), with how long ago that price was
  read under it: `+35.2%` / `12m ago` (`45m` under an hour, `30h` under two days, otherwise days), plus
  `· stale` and `· quiet` when they apply (`12m ago · quiet`). It comes from the tracker's
  latest-price pass (about every 15 minutes for calls under 30 days old, once a day for older
  ones) and does not change with the window selector. Quiet = the last trade is more than 7
  days older than the reading (the price is then that of an old trade). The tooltip gives the
  time of the reading, of the last trade and how old the call was then. A dash means no
  latest price has been read yet.
- **Call MC** (after "Latest MC") is the market cap given in the call post: its "called at"
  figure, or its "📈 Mcap" line when the post has no usable "called at" (missing, zero or
  below, or not a finite number) (`scout_call_metrics`).
  **Latest MC** (after "Latest %") is an **estimate**, since the latest market cap is not
  stored: the post's market cap (the Mcap line first, else "called at", by the same rule) ×
  the latest price ÷ the price at the post. It assumes the token supply has not changed. Both are written
  compactly (`$850`, `$45.2k`, `$1.3M`, `$2.1B`, and from $1 trillion on in powers of ten:
  `$9.3×10³⁸`); hover for the exact amount. A dash when
  neither figure of the post is usable, when there is no latest price (Latest MC), and for calls not
  priced in USD. The legend under the table says that Latest MC is an estimate and how it is
  worked out.

- **Huge numbers** — a percentage of ±1,000,000% or more is written in powers of ten
  (`+3.9×10⁴⁷%`), and so is a dollar amount of $1 trillion or more (Entry $, Call MC, Latest
  MC), with superscript digits, so the columns keep their width; the exact value is in the
  tooltip.
- **Look** — light and dark follow the system setting (`prefers-color-scheme`); the style
  follows the owner's design reference (cards with a light border, a top bar, pill badges,
  a system font stack). `style.css` only; no web fonts or other sites are loaded.

**Analytics page** (`/analytics.html`; "Analytics" in the top bar of the list, "Calls" back).
Section 1, **Call performance**, is built, in three tabs (**Overview**, **By factor**, **Peak vs
final**); section 2, "Model insights", is an empty heading for now. Everything on it is **first
calls only, late entry (60 s after the post), in USD, before tax; values rounded to 0.1**,
worked out in the browser from one compact feed (`GET /api/analytics`, below), so every choice
is instant and no request reads the database. The charts are SVG drawn by the page's own script
(`frontend/analytics.js`, no chart library); the pure calculations (mean and median,
equal-count groups, histogram bands, symmetric-log scale, UTC weeks, "$100 on every call") are
in `frontend/analytics-stats.js`, tested by `node testdata/analytics-stats.test.js` (no
dependencies; `go test` runs it when `node` is on the PATH). Only the visible tab is drawn
(under 0.2 s per tab change on 5,600 calls in headless Edge).

- **Choices (shared by the tabs):** the window (1h, 1d, 3d, 7d, 30d; 1d by default),
  **Metric** (return / peak / drawdown: the hour × weekday chart and the "By factor" charts),
  **Statistic** (median / mean: the same charts), **Quiet after the call** (Include / Exclude /
  Only quiet), **Period** (Week / Month), **Pool family** and **Verdict at the call** (All or
  one; they filter every table and chart, except the two fixed lines of "$100 on every call").
- **Overview tab:** the counts and tables below, and four charts: the **trend** per week (or
  month: bars = calls; lines = mean and median return and the collapse rate, ≤ −50%); the
  **outcome mix** per week (stacked shares of the calls with data: rugged, ≤ −50%, −50% to 0%,
  0% to +100%, ≥ +100%; rugged first, whatever the return); **"$100 on every call"** (running
  profit or loss in call order, each return **capped at +1,000%**, before tax; lines: all
  calls and "no red flags" at the call, both following only the quiet choice (not the pool
  family or verdict choice), and, when a pool family or verdict is chosen, a line for that
  choice); and the **hour × weekday** heat map (UTC; cell = mean or median, call count on
  hover; cells under 20 calls grey).
- **By factor tab:** a factor known at the call: the Perceptor verdict at the call, the
  **market cap at the call as posted** (the post's "called at" figure; a market cap worked out
  from the tracker's entry price would need the token supply, which is not stored), elite
  holders, good holders, holders, elite and good buyers (count), elite and good buys (USD),
  and the hour before the call (buy volume, sell volume, swaps, price change). Number factors
  are split into 4, 5 or 10 groups of about equal count (5 by default) over the calls with
  data for the window and a value; equal values stay in one group (so groups can differ in
  size); when at least 10% of the values are 0, 0 is its own group, and the values below and
  above 0 are split separately (no group spans 0), sharing the remaining groups in proportion
  to their counts, at least one for each side with values and at most one per distinct value
  (so the total stays 4, 5 or 10 when the values differ enough); labels show the range
  ("$20k–$45k"), with more digits where a range's ends, or neighbouring groups, would
  otherwise look equal or overlapping ("$1.25k–$1.26k", not "$1.3k–$1.3k").
  Per group: a histogram with fixed bands (−100…−75, −75…−50, −50…−25, −25…0, 0…+25,
  +25…+50, +50…+100, +100…+300, ≥ +300 %; a boundary value goes to the band farther from 0:
  bands below 0 include their upper end, so 0 is in "−25 to 0", bands above 0 their lower end,
  and "0 to +25" starts just above 0; same axes for every group) and a table; a scatter plot of every call (x: log
  scale keeping 0 for amounts and counts, symmetric log for the price change, categories with a
  fixed jitter for the verdict; y: the metric on a symmetric-log scale; group mean and median
  lines; hover = call id, time, value and metric); the typical path at 1h → 30d per group
  ("all calls with data at each point", n per point in the table under it, or "same calls at
  every point"); and the verdict × pool family grid. Calls without a value are left out (and
  counted in a note); pre-call volumes measured in the pool's quote asset rather than USD count
  as no value.
- **Peak vs final tab:** every call's peak against its return at the window (both symmetric
  log, a dashed "return = peak" line) and the share of calls with a peak above 0% that gave back
  more than half of it (return < peak ÷ 2).
- **Counts** of the selection for the window: calls, with data, not due yet (called less than
  the window ago), due but no data (stored as `no_data`), waiting for the tracker (due, tracked
  in USD, not recorded yet), not tracked / no USD price.
- **Tables**, each with the same columns: all first calls; by **Perceptor verdict at the
  call**; by **pool family** (the pool the tracker priced the call from: Uniswap v2, v3, v4,
  Pons launch curve, GeckoTerminal, not tracked); by **DEX named in the post** (the 12 most
  frequent names, the rest as "Other"); by **quote asset**; by **week** (UTC, Monday to Sunday,
  "Week of Mon 3 Aug 2026") or **month**; and the **median return by week (or month) and
  verdict**.
- **Columns:** Calls, With data (the n of every number in the row; its tooltip says why the
  others have none), Mean and Median return, Mean and Median peak, Mean and Median drop, Win rate
  (return > 0), ≥ +100% peak (peak ≥ +100%, over the calls with a peak), Collapse (return
  ≤ −50%), Rugged (flagged rugged as of now). Every rate shows its count under it ("120 / 249"). A group with fewer than
  20 calls with data is greyed out. A legend under the tables explains each column.
  Returns, peaks and drops come rounded to 0.1 (`GET /api/analytics`), and the thresholds
  are applied to the rounded values: +0.04% counts as 0.0% (not a win), −49.96% as −50.0% (a
  collapse), a peak of +99.96% as +100.0%. The legend says so too.
- **Rugged calls:** their returns after the rug count as −100% (as stored), and, unlike the
  list (which shows no peak for them), their peak before the rug counts.
- **Verdict at the call** is not the list's Perceptor column: it is the call's own live
  Perceptor scan, else the latest live scan of the same address made before the call (the rule
  of `scout_call_dataset_v`). A repeat call's later scan, a re-scan ("Perceptor today") or a
  scan made after the call never count. Calls imported from history show as "Not scanned".
- **Quiet after the call** = fewer than 50 trades (`webQuietTrades` in `webanalytics.go`) in the
  24 hours after the call, and not rugged. Trades = the events of the call's 5-minute candles
  (`scout_call_candles`, the first 24 hours). For Uniswap v2 pools the tracker counts `Sync`
  events, which liquidity adds and removals also emit, so a v2 count can be a little high. The
  count is **known only** for calls priced from an on-chain pool (`entry_price_source`
  `onchain-*`, state version 2 or later) whose 1d window the tracker has stored; a call without
  candles then counts 0. For the others (GeckoTerminal-priced, untracked, younger than a day or
  not tracked that far yet) it is unknown: "Exclude" keeps them, "Only quiet" leaves them out.
- The page asks again every minute while it is visible (`If-None-Match`: a `304` while nothing
  changed); "Reload" asks at once. When a request fails it says so and the time of the data
  still shown.

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

`GET /api/summary`, `/api/calls` and `/api/call` answer from the snapshot (see above) and send `ETag`, `Cache-Control: no-cache`,
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
`q`, `sort`, `usd_only`, `verdict`, `days`, paging and `total` all apply to that list, so a repeat call is never
returned and cannot be found by its own name or symbol:

| Parameter | Values | Default | |
|---|---|---|---|
| `q` | text, up to 100 characters | *(none)* | part of the token name, symbol or contract address; case-insensitive (letters of any script, by the Unicode lower-case rule); `%` and `_` are ordinary characters |
| `sort` | `date`, `return`, `peak`, `latest`, `call_mc`, `latest_mc`, `return_1h`, `return_1d`, `return_3d`, `return_7d`, `return_30d` | `date` | empty values always come last (in both directions); ties by call id, in the direction asked for. `return` / `peak` = by `return_pct` / `peak_pct` of the `horizon` asked for. `return_1h` … `return_30d` = by that window's return (`return_1h_pct` …), whatever the `horizon`. `latest` = by `latest_return_pct`, `call_mc` = by `call_mcap_usd`, `latest_mc` = by `latest_mcap_usd` (these are the same for every `horizon`) |
| `dir` | `desc`, `asc` | `desc` | |
| `horizon` | `1h`, `1d`, `3d`, `7d`, `30d` | `1d` | which window `return_pct` / `peak_pct` / `drawdown_pct` (and `sort=return` / `peak`) are for; the page sets it with its Peak / worst drop selector |
| `usd_only` | `1`, `0` | `1` for every `sort` but `date`, else `0` | `1` = only tokens whose first call has `price_unit = usd` |
| `verdict` | `clean`, `caution`, `red_flags`, `not_scanned`, or several separated by commas (`verdict=clean,caution`) | *(none = all)* | the token's Perceptor verdict (see below); with several, a token with any of them. The parameter may also be repeated (`verdict=clean&verdict=caution`); all values add up, repeats and empty items are ignored, so `verdict=` = all, and so do all four. `clean` = no red flags found; `not_scanned` = no completed Perceptor report, or one whose verdict is `unknown`. The order does not matter: `caution,clean` is the same question, with the same `ETag`, as `clean,caution` |
| `days` | 1 – 3650 (whole days) | *(none = all)* | only tokens whose first call was posted within the last `days` × 24 hours, counted back from the snapshot time (`snapshot_at`), not the time of the request; `days=0` is a 400 (leave it out for all) |
| `page` | 1 … | `1` | |
| `per` | 1 – 200 | `50` | |

Any other value or parameter, or any parameter but `verdict` given twice → HTTP 400 with `{"error": "…"}`.

```json
{"total": 3105, "page": 1, "per": 50, "horizon": "1d", "sort": "date", "dir": "desc", "usd_only": false, "verdict": "", "verdicts": [],
 "days": 0, "snapshot_at": "2026-10-02T14:30:00.123Z",
 "calls": [{"call_id": 812, "message_id": 10002, "message_date": "2026-10-01T14:30:00Z",
   "post_url": "https://t.me/scoutrobinhood/10002",
   "contract_address": "0x…", "token_name": "Malfoid", "token_symbol": "MALFOID",
   "gmgn_url": "https://gmgn.ai/robinhood/token/0x…", "price_unit": "usd",
   "entry_price_usd": 0.0045, "return_pct": -20.0, "peak_pct": 100.0, "drawdown_pct": -50.0,
   "return_1h_pct": 12.5, "return_1d_pct": -20.0, "return_3d_pct": 40.1, "return_7d_pct": null,
   "return_30d_pct": null, "rugged": false, "tracking_status": "done", "perceptor_verdict": "clean",
   "perceptor_url": "https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0",
   "call_count": 3, "last_call_date": "2026-10-02T09:12:00Z",
   "latest_return_pct": 35.2, "latest_price_usd": 0.006084,
   "latest_at": "2026-11-30T14:31:10.52Z", "latest_trade_at": "2026-11-30T13:02:44Z",
   "latest_age_seconds": 5184070, "call_mcap_usd": 45200, "latest_mcap_usd": 61110.4,
   "has_salpha_report": true, "perceptor_report_id": 5120, "salpha_report_id": 5121,
   "perceptor_today_verdict": null, "perceptor_today_url": null, "perceptor_today_at": null}]}
```
`call_count` = how many real calls of that token exist in total (1 or more; update posts are
not counted); `last_call_date` = the date of the most recent one (equal to `message_date` when there is only one). Everything else
in the object belongs to the first call.
`entry_price_usd` is the price 60 seconds after the post (the price at the post when that one
is missing). `return_pct`, `peak_pct` and `drawdown_pct` are for the `horizon` asked for.
`return_1h_pct`, `return_1d_pct`, `return_3d_pct`, `return_7d_pct` and `return_30d_pct` are the
return of each window, whatever the `horizon`: each is exactly the `return_pct` the same row
has when that window is asked for (`null` until the window is recorded). The page shows these
five as its 1h … 30d columns; `return_pct` is kept for older clients. `entry_price_usd`,
`return_pct`, `peak_pct`, `drawdown_pct` and the five window returns are `null` unless
`price_unit` is `usd`. `peak_pct` is also `null` when `rugged` is `true` (the returns and
`drawdown_pct` stay as stored). `gmgn_url` is `null` for anything that is not a plain `0x…`
address.

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

- `call_mcap_usd` = the first **valid** of `called_at_mcap_usd`, `mcap_usd`: the market cap at
  the call as given in the post. Valid = present, above zero and a finite number; so a "called
  at" of 0, a negative one or `NaN` falls back to the Mcap line.
- `latest_mcap_usd` = (the first valid of `mcap_usd`, `called_at_mcap_usd`) × `latest_price_usd`
  ÷ `entry_price_usd`, where `entry_price_usd` is the tracking row's price **at the post**
  (not the late entry the `entry_price_usd` field of the response shows; the post's market cap
  is a post-time figure). An **estimate** that assumes the token supply has not changed; the
  latest market cap itself is not stored.

Both are `null` when the call's `price_unit` is not `usd`, when neither market cap of the post
is valid, when the latest price or the price at the post is missing, zero, negative or not
finite, or when the result is not finite. `latest_mcap_usd` is also `null` whenever
`latest_price_usd` is (no latest price yet). Like the latest price, they do not depend on
`horizon`, and a change to either value changes the `ETag` of `/api/calls`.

`snapshot_at` = when the website last read the database (the same moment as `updated_at` of
`/api/summary`). `verdict` in the response repeats the filter that was applied, in the order `clean`, `caution`,
`red_flags`, `not_scanned`, separated by commas (`"clean,caution"`; a single value as asked,
`"clean"`; `""` when none or all four), and `verdicts` is the same list as an array (`[]` = all).
`days` repeats the age filter (`0` = all); `total` then counts only the calls within it.
`perceptor_verdict` and `perceptor_url` are **per token**: the verdict (`clean`, `caution`,
`red_flags` or `unknown`) and report link of the latest completed live Perceptor investigation
(`scout_investigations`, tool `perceptor`, `status = completed`, `scan_kind = 'live'`, newest
`requested_at`) with that contract address, upper/lower case ignored — not only the listed
call's own. Both are `null` when the token was never scanned at the time of a call, which is
the normal case for calls imported from history (only live calls are scanned).
`perceptor_url` is also `null` when the stored link does not start with `https://`. This is a
rule of the website only: `scout_call_dataset_v.perceptor_verdict` is unchanged and still
belongs to the single call. The Perceptor filter (`verdict=`) and its counts use the same
live verdict.

`perceptor_today_verdict`, `perceptor_today_url` and `perceptor_today_at` are **"Perceptor
today"**: the token's latest completed Perceptor re-scan by the rescan lane (`scan_kind =
'rescan'`, see "Re-scanning calls without a Perceptor report"), run long after the call, so
its verdict is today's, not the one at the time of the call. Same verdict values and link rule
as above; `perceptor_today_at` = when it ran (`completed_at`, else `requested_at`). All three
are `null` without a completed re-scan. A re-scan never fills `perceptor_verdict`,
`perceptor_url` or `perceptor_report_id`, never moves a row in the Perceptor filter, and is
not shown by `GET /api/call`. A new re-scan changes the `ETag` of `/api/calls` and sends a
live `report` event with `"tool":"perceptor_today"` (also when a newer re-scan has the same
verdict); the page shows it as the "Perceptor today" label (see "Website").

`has_salpha_report`, `perceptor_report_id` and `salpha_report_id` are three fields of
a call that say which reports `GET /api/call` returns for it (per token, like the verdict):
`perceptor_report_id` = the id (`scout_investigations.id`) of the Perceptor investigation the
verdict comes from; `salpha_report_id` = the id of the token's latest completed sAlpha
investigation (tool `salpha`, `status = completed`, newest `requested_at`, contract address
with upper/lower case ignored) **whose `report_text` is not empty and not only white space**,
a real report always before a reply that declines (see the row detail above);
`has_salpha_report` = `salpha_report_id` is not `null` and that reply is not a decline. A new
report of either tool changes them, and so the `ETag` of `/api/calls`; an empty sAlpha reply
changes nothing.

`GET /api/call?id=<call_id>` — the row detail: the Perceptor and sAlpha reports of the token of
one listed call (`call_id` of a row of `/api/calls`), from memory:

```json
{"call_id": 812,
 "perceptor": {"id": 5120, "verdict": "caution", "label": "Caution", "summary": "Top 10 hold 40%",
   "url": "https://www.perceptor.info/r/deb9d3118ec1480e985032f9472c87c0",
   "at": "2026-10-01T14:31:02Z", "truncated": false},
 "salpha": {"id": 5121, "text": "Smart money: 3 wallets bought…", "declined": false, "url": null,
   "at": "2026-10-01T14:31:40Z", "truncated": false}}
```
- `perceptor` = the report of `perceptor_report_id` (`null` when there is none): `verdict` is
  `clean`, `caution`, `red_flags` or `unknown`; `label` and `summary` are the report's
  `verdict_label` and `verdict_summary` (either may be `null`).
- `salpha` = the report of `salpha_report_id` (`null` when the token has no sAlpha reply with
  text): `text` is its `report_text`, plain text from a bot (show it as text, never as HTML).
  `declined` = the reply only declines to report (`text` is then its reason); the page shows
  "sAlpha did not generate a report".
- `url` = the report's link, `null` unless it starts with `https://` (and is at most 2,048
  characters). `at` = `completed_at`, else `requested_at`. `truncated` = a text was longer
  than 32 KB and is cut there (at a character boundary).
- **Codes:** `400` for a missing or bad `id` (a whole number from 1 to 2147483647, written
  plainly) or any other parameter; `404` when `id` is not the call of a row of the list — also
  a call the page got from an older snapshot that is no longer listed; `503` before the first
  snapshot. The body of an error is `{"error": "…"}`.
- **Caching:** `ETag` (weak) follows the call's two reports only, so it stays the same while
  anything else changes; `If-None-Match` → `304`. `Cache-Control: no-cache`, `X-Snapshot-At`,
  and gzip like the other JSON (for answers of 1 KB or more).
- **Memory, and what is read:** the texts are held in memory next to the snapshot, keyed by
  investigation id, and only for the reports some row names. A refresh keeps the texts it has
  and reads, with one extra query, only those of report ids it does not hold yet (an
  investigation is written once, so a text it holds never changes); texts no row names any more
  are let go. A request never reads the database. If that query fails, the whole refresh counts
  as failed and the snapshot before stays. A report that cannot be read at that moment is left
  out of the row until the next refresh.

`GET /api/analytics` — the feed of the Analytics page: one compact row per first call, built
once with each snapshot (encoded and gzip-compressed then, not per request), from memory. No
parameters (any → `400`); `ETag` (weak, a hash of the body, so it stays the same while only
the list's other fields move, such as latest prices), `If-None-Match` → `304`,
`Cache-Control: no-cache`, `X-Snapshot-At` (the page counts "not due yet" from it), gzip for
clients that take it; `503` before the first snapshot.

```json
{"format": 2, "horizons": ["1h","1d","3d","7d","30d"], "horizon_seconds": [3600,86400,259200,604800,2592000],
 "quiet_below": 50, "verdicts": ["clean","caution","red_flags","unknown","none"],
 "families": ["v4","v2","pons","v3","gecko","untracked"], "dexes": ["Uniswap V4","Pons V2","…"], "quotes": ["WETH","USDG"],
 "columns": ["call_id","t","flags","verdict","family","dex","quote","trades_24h","no_data",
             "ret_1h","peak_1h","dd_1h","ret_1d","peak_1d","dd_1d","…","dd_30d",
             "mcap","holders","proof_elite","proof_good","buys_elite_n","buys_good_n",
             "buys_elite_usd","buys_good_usd","pre_buy_usd","pre_sell_usd","pre_swaps","pre_chg"],
 "rows": [[812, 1790000000, 5, 0, 1, 3, 0, 214, 0, 12.5, 40.1, -3.2, -20, 100, -50, null, null, null, …,
           45700, 812, 3, 11, 2, 5, 1230, 20000, 15000, 9000, 31, -12.3]]}
```

- `t` = the call's time (Unix seconds). `flags`: 1 = priced in USD (only these rows have
  numbers), 2 = rugged (as of now), 4 = tracked (has an entry price).
- `verdict`, `family`, `dex`, `quote` are indexes into `verdicts`, `families`, `dexes`,
  `quotes` (`-1` = none). `verdict` = the Perceptor verdict at the call (see the Analytics
  page; `none` = no live scan at the time). `family` = `entry_price_source` without `onchain-`
  (v2, v3, v4, pons), `gecko` for GeckoTerminal candles, `untracked` without one. `dex` = the
  DEX named in the post (`scout_call_metrics.dex`, spaces trimmed, at most 60 characters);
  `quote` = the pool's quote asset (`scout_call_tracking.onchain->>'quote_sym'`). The
  dictionaries list the most frequent first.
- `trades_24h` = swaps in the first 24 hours (see "Quiet after the call"), `null` when unknown.
- `no_data`: bit *i* set = window *i* of `horizons` is stored with status `no_data` (due, but
  the price source had nothing for it). A window with no value and no bit is either not due yet
  (`t` + its seconds after `X-Snapshot-At`) or not recorded yet.
- Then, per window: return, peak and worst drop (late entry, USD, %, rounded to 0.1; `null` when
  missing). A rugged call keeps its peak here.
- Then the values known at the call (`null` when missing), read like `scout_call_dataset_v`:
  `mcap` = `called_at_mcap_usd` of the post, the market cap **as posted** (positive, calls
  priced in USD only; no fallback to the "Mcap" line; not worked out from the entry price, as
  the token supply is not stored); `holders`, `proof_elite`, `proof_good`, `buys_elite_n`, `buys_good_n`
  (`live_buys_*_count`), `buys_elite_usd`, `buys_good_usd` (`live_buys_*_usd`) from
  `scout_call_metrics` (all `null` without a parsed post); `pre_buy_usd`, `pre_sell_usd`
  (`buy_vol_60m`, `sell_vol_60m`; `null` unless `vol_unit` is `usd`), `pre_swaps`
  (`swaps_60m`) and `pre_chg` (`price_chg_60m_pct`, %, 0.1) from `scout_call_precall` (all
  `null` without a row). Dollar amounts are whole dollars, 3 significant digits from $1,000 up.
  `format` was 1 before these columns; an open page with the old script says "unexpected
  format; reload the page".
- Size: about 158 bytes a row, about 50 compressed (5,600 synthetic rows: 883 KB, 283 KB
  gzipped; without the factor columns the same rows are 640 KB, 209 KB gzipped; the real data
  was 117 / 39 bytes a row before, so expect about 160 / 53 now: about 960 KB, 320 KB gzipped
  for 6,000 rows).
- The fields behind it are part of the list's snapshot version, so a change to any of them also
  changes the `ETag` of `/api/calls`.

`GET /api/events` — live updates as a stream of
[Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
(`Content-Type: text/event-stream`, `Cache-Control: no-store`, never gzip-compressed, no
`ETag`). It sends what changed in the list each time the website reads the database —
whatever started the read (a notification from the listener, the background refresh, **Refresh
now**) — by comparing the new snapshot with the one before. The events are built once per read,
not per client, and no request reads the database.

```
retry: 5000
: connected

id: mf3k2a1-7
event: call
data: {"call_id":812,"horizon":"1d","row":{"call_id":812,"message_id":10002,…}}

id: mf3k2a1-8
event: report
data: {"call_id":812,"tool":"perceptor","horizon":"1d","row":{…}}

: ping
```

- `call` = a token row that was not in the snapshot before (a new token; a repeat call of a
  listed token is not one). `report` = a listed token whose Perceptor verdict or report changed
  (`tool: "perceptor"`), or that got a new sAlpha report or decline (`tool: "salpha"`), or a
  new or changed Perceptor re-scan ("Perceptor today", `tool: "perceptor_today"`: a newer
  re-scan or another verdict; never the call-time verdict); one row can give several. `row`
  is the row exactly as `GET /api/calls?horizon=1d` returns it (so `return_pct`, `peak_pct`
  and `drawdown_pct` are those of `horizon`, always `1d`). Rows that leave the list send
  nothing.
- `reload` = ask for the list again instead: `{"calls":25,"reports":3}` when one read found
  more than 20 changes, `{"missed":true}` when events after the browser's `Last-Event-ID`
  can no longer be sent (see below).
- **Ids** are `<process>-<n>`. A browser that reconnects sends the last one it got as
  `Last-Event-ID`, and gets only the events after it (the last 128 are kept), never one twice.
  An id of an earlier run of the website, or one older than the kept events, gets
  `reload` / `{"missed":true}` first.
- **Keep-alive:** a comment line `: ping` every 25 seconds. `retry: 5000` tells the browser to
  reconnect 5 seconds after the stream breaks.
- **At most 50 streams at once**; one more gets `503` with `Retry-After: 60` and
  `{"error": "…"}` (the page then stays on its 30-second refresh and tries again 2 minutes
  later). A client that falls 64 events behind (it does not read) is disconnected, so it never
  holds up the others; its browser reconnects and catches up through `Last-Event-ID`. The
  stream also ends when the website stops.
- **Same site only**, like `POST /api/refresh`: an `Origin` header of another host (or port,
  or `null`) → `403`; no `Origin` (curl) is accepted. Any parameter → `400`; anything but
  `GET` → `405`.
- The server's read and write timeouts (10 s / 30 s) do not apply to the stream; each write
  to it has 10 seconds instead.
- Try it: `curl -N http://localhost:8090/api/events`.

`POST /api/refresh` — "Refresh now": the website reads the database at once (the same read
as the background refresh), puts the new snapshot in place and then answers. No parameters
and no body; it is the only address that takes `POST`, and it takes nothing else (`GET`,
`HEAD`, … → 405 with `Allow: POST`), so links, prefetchers and crawlers cannot trigger it.

```json
{"refreshed": true, "rate_limited": false, "snapshot_at": "2026-10-02T14:30:05.412Z", "snapshot_age_seconds": 0}
```
`refreshed` = the database was read for this request; `rate_limited` = it was not, because
of the limit below, and `snapshot_at` is the snapshot as it already was. Afterwards ask
`/api/summary` and `/api/calls` again (the page does): their `ETag`s change only if the data did.

- **One read at a time.** There is never more than one read of the database: a press while a
  read is under way — started by another press or by the background refresh — waits for that
  read and gets its result (`refreshed: true`), and the background refresh likewise waits for
  a read a press started.
- **Rate limit.** A press within **5 seconds of the end of the last read a press started**
  (whoever pressed) does not read; it answers at once with `rate_limited: true`. A failed read
  counts too. Joining a read under way is not limited, and the background refresh is not
  affected by the limit.
- **Timeout.** A press waits at most 15 seconds; the read itself is given up after 60 seconds
  (as every read). A slow read goes on after the press gave up and is put in place when it
  ends; a later press joins it.
- **Errors.** The snapshot is kept as it was and keeps being served. `503` = the database could
  not be read, `504` = it did not answer within 15 seconds; the body is
  `{"error": "…", "snapshot_at": "…"}` (`snapshot_at` = the data still shown; absent before
  the first snapshot). The details go to the log (`web: could not refresh the snapshot: …`).
- **Same site only.** A request whose `Origin` header is present and names another host (or
  another port, or `null`) is refused with `403` before anything is read. Browsers send
  `Origin` with every `POST`, so a page of another site cannot press the button; a request
  without `Origin` (curl) is accepted. The check compares `Origin` with the `Host` header: a
  reverse proxy in front of the website must pass `Host` through unchanged, or every press
  gets `403`.

## State / logs

- `scoutanalytics_data/seen_cas.json` — CAs already investigated (never re-run; delete an entry to rerun)
- `scoutanalytics_data/scans.jsonl` — every CA: each tool's status, verdict and report text, delivered?
- `scoutanalytics_data/poll_cursor.json` — the last post polling has handled, per channel; a
  restart catches up from it (see "How calls are picked up")

CAs are processed one at a time; within a CA all tools run in parallel (replies are
matched to each bot, so they can't get mixed up). The client reconnects with
backoff if the connection drops. Ctrl+C to stop.

## Licence

PolyForm Noncommercial License 1.0.0 (see [LICENSE](LICENSE)): free for personal, research and
other non-commercial use. Commercial use requires permission — contact @kfukue on GitHub.

`.claude/agents/react-coder.md` is adapted from wshobson/agents under its MIT licence
(attribution kept in that file).
