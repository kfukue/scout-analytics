# Scout analytics: handoff to Claude Code

Written 6 October 2026. Start Claude Code in the repo root; `.claude/settings.json`
makes the session the product manager, which delegates to the agents in
`.claude/agents/` (see "Agent setup").

First message to give it:

> Read telegrambot/scoutanalytics/HANDOFF.md and README.md, then build the
> items under "Next work" in order. Report after each one.

## State of the code

Branch `scout-call-model`. Production runs `main`.

- Listener: reads @scoutrobinhood, tells real calls from "hit 3X" update posts
  (`postkind.go`), sends new tokens to @perceptor0xBot and @salpha_research_bot,
  delivers to the private group, records to Postgres.
- Tracker (`-track`): on-chain prices from Uniswap v2/v3/v4 pools, returns at
  1h/1d/3d/7d/30d, 5-minute and hourly candles, pre-call trading stats, token
  names from `name()`. Only the first real call of each token is tracked; later
  calls get status `repeat`. A latest-price pass keeps a current return per
  token (every 15 minutes under 30 days old, daily after).
- Website (`-web`, port 8090, no login, read-only): one row per token, served
  from an in-memory snapshot refreshed every 15 seconds, plus a "Refresh now"
  button. Columns: Date | Token | Symbol | Calls | Perceptor | Status | Entry $ |
  Call MC | Latest MC | Latest % | Peak % | Worst drop % | 1h | 1d | 3d | 7d |
  30d. Search, sorts, Perceptor filter, legend, and a per-row report detail
  pane (Perceptor + sAlpha). Front end is plain JavaScript in `frontend/` (no
  framework, no build step).
- Model (`ml/`): labels for four holding periods, logistic baseline and
  LightGBM, time-split validation with pass/fail gates, scoring service. Not
  trained on real data yet: the tracker has to finish the history first.

### Shipped and merged to `main`

1. "Latest %": the latest-price pass and the website column (commit 514b073).
2. eth_getLogs range size (commit af8e8fe): the range size is kept per scan; a
   range is split only on a "too large" refusal or a node "request timed out"
   (after one retry); it grows back after 3 full-size answers; the floor is 200
   blocks; progress lines show the range size.
3. Market cap columns (commit 9894e0f): Call MC and Latest MC.
   - Call MC = the first valid figure of `called_at_mcap_usd`, `mcap_usd` (a 0
     or NaN called-at value falls back to `mcap_usd`; the fallback shipped in
     1f4e206).
   - Latest MC = `COALESCE(mcap_usd, called_at_mcap_usd)` ×
     `latest_price_usd` ÷ at-post `entry_price_usd` (an estimate; assumes
     supply has not changed).
   - Both sortable (`call_mc`, `latest_mc`); a dash for non-USD or invalid
     inputs.
4. "Refresh now" button, new column order and per-window returns (commit
   1f4e206).
   - Order: Date | Token | Symbol | Calls | Perceptor | Status | Entry $ |
     Call MC | Latest MC | Latest % | Peak % | Worst drop % | 1h | 1d | 3d |
     7d | 30d.
   - The 1h/1d/3d/7d/30d selector switches only Peak % and Worst drop %. The
     five window columns sort with `return_1h` … `return_30d`.
   - `POST /api/refresh`: coalesced with the background loop, 5 s global rate
     limit, same-origin check, waits at most 15 s (then 504), 503 on a database
     failure. Every other request is still answered from the snapshot.
   - Same commit: the Call MC first-valid-figure fallback, and the
     `TestWebGzip` fix (the test now expects SVG to be compressed).
5. `-retry-no-pool` (commit 8ed80f9): one-shot, database-only command that sets
   `no_pool` first calls back to `pending`, due now.
   - `-retry-gave-up` also includes `gave_up` calls; those without an entry
     price get their on-chain state cleared so the pool is discovered again.
   - `-retry-launchpad a,b` limits it to launchpads or dexes (match ignores
     case, spaces and punctuation). `-retry-dry-run` only shows what would be
     reset.
   - **Not run yet.** The owner plans to run it after Pons V2 support is
     deployed (steps under Next work item 1), and again after the Longxyz/USD
     work.

### Committed on `scout-call-model` (commit 8846dc6; may not be merged or deployed yet)

1. **Report detail pane.** A toggle per row shows the Perceptor and sAlpha
   reports, loaded from `GET /api/call?id=`; rows with an sAlpha report get an
   "sA" badge. Empty sAlpha replies count as no report. The texts are held in a
   separate in-memory map, and each refresh reads only ids not loaded yet.
2. **Rug guard.**
   - When the quote side of the pool is under `SCOUT_RUG_LIQ_USD` (default
     $500, settable in `.env`) the call is rugged: −100% from the rug point,
     peak = the pre-rug peak.
   - An empty pool always counts as rugged.
   - Bound prices (2^128) are ignored; a 1e6× backstop catches the rest.
   - `current_liquidity_usd` stores 2 × the quote side.
   - Fixed an old bug where an interrupted run valued the pool at $1/ETH in the
     end check.
   - Follow-up (minor): the gecko source still compares `reserve_usd` (both
     sides), not the quote side.

### Pending owner actions

1. **Re-track reset, about 149 calls.** First calls with status
   tracking/done/error and `current_liquidity_usd < 1000` (= quote side under
   $500), plus the calls with impossible numbers (returns, peak or latest over
   1e5%, or the 2^128 fingerprint).
   - Prepared as four pgAdmin files in the session scratchpad
     `retrack\pgadmin\`: A_count, B_reset (with an expected-count guard),
     C_check, D_catchup. The owner uses **pgAdmin**, so no psql
     meta-commands.
   - Steps: deploy, run A, stop the trackers, run B, start the trackers, run C;
     D only if needed.
   - **Not run yet.** The owner is letting the overnight `-track` run finish
     first.
2. **After the reset's re-tracking has finished: ask about the Uniswap v4
   calls** (see the top of "Open questions").
3. **Tracking progress:** 1,546 of 4,618 tracked after 12+ hours. That is near
   the ceiling of about 1,850–1,900, because the Pons V2 calls cannot be priced
   until Pons support ships.

### Tracker speed: root cause and fix

The Nitro node (offchainlabs/nitro-node v3.11.2, run with `docker run` on the
prod server) indexed logs only for the last 9.4M blocks (the default
`execution.rpc.log-history`). Older ranges were walked at about 1 ms per block,
timed out at 30 s and shrank ranges to 3,120 blocks. The owner added
`--execution.rpc.log-history=0`. A probe (`logprobe.sh`, in the owner's
`~/Documents` on the server) then showed deep history indexed: 200k blocks about
40M back in 14 s, about 55M back in 12 s. After the restart a call takes 1-2
minutes and 14-380 requests, against more than an hour and 7,000 before.

Status at restart: pending 4166, tracking 142, done 642, no_pool 67, error 26,
gave_up 8, repeat 5486.

### Chainlink feeds on prod

`.env` on the server has `SCOUT_CHAINLINK_FEEDS` for 10 verified Robinhood stock
tokens: GME, MSFT, TSLA, SPCX, CRCL, GOOGL, MU, NVDA, AMZN, SNDK. A full
33-token line was offered; the owner may have added it.

- Token addresses are verified against Robinhood's registry,
  `https://api.robinhood.com/rhj/prices/<SYMBOL>` (chain 4663).
- Use each feed's "Standard Proxy" address.
- Robinhood feeds price one token (share × uiMultiplier), so they are used
  directly.
- HOODon (0xfb5b5778d45ae47f15323fb59b666c655174a79c) is not a Robinhood token
  (probably Ondo) and has no feed. RDDT is official but has no feed in
  Chainlink's list.

### Prod data findings (owner's SQL)

- First calls by launchpad: Pons V2 2,774 (60%; done 8, no_pool 66, pending
  2,654), Uniswap V4 697, Longxyz 423, Uniswap V3 124, Pons (v1) 119, then about
  40 small launchpads.
- The 26 errors were mostly transient: 12 "connection refused" during the Nitro
  restart, 12 "bad response" (oversized replies, probably from the old code),
  1 SNDK "no trades in the USDG pool", 1 GOO "no USD source".
- 35 calls are stuck in non-USD units. Stock tokens: HOODon 9, GME 5, RDDT 3,
  MSFT 3, TSLA 2, SPCX 2, CRCL, GOOGL, MU, NVDA, AMZN 1 each. Meme and other
  quotes: PIPEDOG 4, PONS 3, SHIB 2, VIRTUAL 1.
- Still to confirm: that the "bad response" errors recover under the new code.
  Ask the owner to run:
  `SELECT left(error,40) kind, count(*), max(last_checked_at), min(next_check_at) FROM scout_call_tracking WHERE status='error' GROUP BY 1`

## Rules the owner has set

- Never print, commit or copy `.env`, `scout.session.json`, `scoutanalytics_data/`.
- No new database tables without asking; extend existing ones. Every statement
  in `scoutanalytics.sql` must be safe to repeat (it runs at every start).
  Views are dropped and recreated at startup, dependents first.
- Website speed is the priority: requests are answered from the snapshot and
  must not query the database (the only exception is the rate-limited
  `POST /api/refresh`). Keep p95 under 10 ms; rerun the benchmark
  (`go test -run xxx -bench BenchmarkWebSnapshot`) after touching it.
- Only real calls are scanned, delivered and tracked; one row per token.
- The owner commits and pushes; agents' git commit and push are blocked by
  permissions. Never commit to `main`.
- Say "tested locally" unless it ran on the prod server. Flag anything that
  makes the tracker redo history or adds ongoing node load before shipping it.
- Ask the owner before resetting calls, and give him the exact SQL.
- One line of work at a time on this database: two branches migrating the same
  schema caused both production startup failures so far.
- One coder at a time on overlapping files: parallel coders in the same tree
  mixed their README edits.
- A coder that runs a throwaway Postgres uses its own scratchpad directory and
  its own port, then stops it and deletes the directory when done.
- The owner prefers not to set up a local test database. For prod data he runs
  read-only queries and pastes the results.

## How to test

- `go vet ./telegrambot/scoutanalytics` and
  `go test -race ./telegrambot/scoutanalytics`. Database tests need
  `SCOUT_TEST_DATABASE_URL` pointing at a throwaway Postgres; without it they
  are skipped. `TestOpenScoutStoreSelection` needs that database to be named
  `scout_test` (the README's example URL uses that name but does not say it is
  required). See the rule above on running a throwaway Postgres.
- `TestLatestPriceInterruptedLeavesRowUntouched` is flaky (see Next work
  item 7, smaller tracker improvements).
- On-chain code uses the fake chain in `onchain_test.go`; never call real nodes.
- `python -m pytest tests -q` in `ml/` (27 tests).
- Page changes: check in a real browser at 1280px and 390px, light and dark.
  Build DOM with `textContent` only (token names come from arbitrary contracts)
  and keep the Content-Security-Policy (no inline script or style).

## Next work (in order; one line of work at a time)

1. **Pons V2 support** (owner approved; **in progress**, a coder is building
   it). 60% of first calls are Pons V2 and almost none are priced.
   - Being built: discovery via the factory's `TokenLaunched`; prices from
     `CurveBuy`/`CurveSell`; graduation handover to the v4 pool (Pons hook) in
     both the horizon scan and the latest pass; `-price-check` shows curve
     info.
   - **No USD rug threshold on the curve** (the curve's real reserve starts
     near 0); the 5% rule and the 1e6× backstop still apply.
   - After deploy: `-price-check` on 2–3 Pons tokens; then
     `-retry-no-pool -retry-dry-run`; then
     `-retry-no-pool -retry-launchpad pons_v2 -retry-gave-up`. Expect mostly
     `gave_up` in the dry run, since historical Pons calls are past the 32-day
     deadline.
   - Pons V2 deploys one bonding curve per token. Factory
     0x7eD598BcEf8bd9Edd8C97A195C6d13f40801EC7e emits
     `TokenLaunched(token indexed, curve indexed, deployer indexed, pairToken, launchConfigId, graduationThreshold)`;
     a zero pairToken means ETH.
   - The curve emits `CurveBuy(buyer indexed, recipient indexed, quoteIn, tokensOut, fee, tax)`,
     `CurveSell(seller indexed, recipient indexed, tokensIn, quoteOut, fee, tax)`
     and `CurveCompleted`. It has `token()`, `pairToken()` and `getReserves()`,
     but no `token0()`.
   - At the threshold (4.2 ETH) the token graduates to a Uniswap v4 pool behind
     hook 0xE5e702641Ea86F4ae6cC3cDaeD2B886f976Be044; the factory emits
     `PoolGraduated`.
   - Pons V1 (factory 0xA5aAb3F0c6EeadF30Ef1D3Eb997108E976351feB) used Uniswap
     v3 and already works.
   - Source: github.com/ponsdotdev/pons-labs
     (`contractsV2/src/v2/PonsV2BondingCurve.sol`); docs.ponsfamily.com/v2.
     Check in the source whether CurveBuy's `quoteIn` is gross or net of fee.
   - Design: in `resolveV2V3`, when `token0()` fails, try `token()`/`pairToken()`
     and set `Kind="pons"`. `logFilter`/`priceOfLog`/`tradeOfLog` cover the
     curve events, with the price taken from the execution price. Hand over to
     the v4 pool at graduation, keeping the running extremes. When the call was
     before graduation, prefer the curve in discovery. `pool_dex = "pons-curve"`.
   - Estimate: about 150-250 lines plus tests. No schema change, no state
     version bump. `no_pool` calls retry every 6 hours on their own. The 2
     `gave_up` Pons calls need a reset (cheap with the full index; ask the owner
     first): `-retry-no-pool` covers this after deploy (see "Shipped", item 5).
   - Example token 0x59e8ae5c2e1edf77d2169e0ae67524c43aaca393 (call 10214). The
     owner was asked to run a factory eth_getLogs (topic1 = token) to confirm
     the layout on chain.
2. **USD source fix and data-loss bug** (next after Pons V2; covers most of
   Longxyz), in `quoteUSD`/`quoteViaPool`
   (`onchain.go`, `onchain_extra.go`, `tracker_onchain.go`).
   - (a) The `quotePools[quote]=nil` cache lives for the whole process and
     ignores the block, and JSON-RPC errors are cached as "no pool". Separate
     "no pool" from "lookup failed"; give negative cache entries an expiry;
     search around the block and around head, and widen when no acceptable
     pool is found; keep every acceptable pool and choose one per block.
   - (b) Lookup order everywhere: stablecoin → Robinhood feed → mainnet feed →
     asset/WETH pool (v2/v3/v4) × mainnet ETH/USD → asset/stable pool. Robinhood
     feeds come before mainnet because they cover every stock token and price
     the token itself; with the full log index they are cheap. ETH/USD stays on
     mainnet Chainlink.
   - (c) **Data-loss bug:** the horizon step advances `scan_block` and saves it
     even when candle conversion fails afterwards, so the candles and peak/low
     of that segment are lost for good. Work on a copy of the state and commit
     only when every step succeeds.
   - (d) Handle Chainlink aggregator changes on the log-based (full-node) feed
     path.
   - (e) Load feeds from the existing shared table `asset_chains` (`asset_id`,
     `chain_id`, `chainlink_data_feed_contract_address`; primary key
     asset_id + chain_id; defined in the sibling repo
     `lyle-labs-libraries\assetChain\asset-chain-link-feed.sql`). Join `assets`
     (by contract address, compared with `lower()`) and `chains` (EVM chain_id
     4663 for Robinhood, 1 for mainnet). Reload each cycle and swap atomically.
     Database rows add to the env vars; the env vars win on conflict. Do not add
     an `is_active` column: altering a shared table risks a migration permission
     failure, so delete a row to deactivate it. Edit rows with SQL, not the
     API's `/assetChains` routes, which are buggy and can crash the API.
     Stock-token asset rows need `ignore_market_data=true` and
     `import_geth=false`.
     - Prod check results (owner ran them):
       - `chains` has only Ethereum (id 2, chain_id 1). There is **no Robinhood
         Chain row** (chain_id 4663).
       - `asset_chains` columns are exactly: `asset_id int NOT NULL`,
         `chain_id int NOT NULL`,
         `chainlink_data_feed_contract_address text NOT NULL`,
         `created_by text NOT NULL`, `created_at timestamptz NOT NULL`,
         `updated_by text NOT NULL`, `updated_at timestamptz NOT NULL`.
       - There are **0 assets on chain 4663**.
     - So this needs a **seed SQL script** that the owner reviews and runs
       himself on prod (the tracker itself only SELECTs from these tables). It
       must be safe to run twice:
       - insert the `chains` row for Robinhood Chain (chain_id 4663) if missing;
       - insert about 33 `assets` rows for the Robinhood stock tokens
         (`ignore_market_data=true`, `import_geth=false`), matched by
         `lower(contract_address)` + chain so it never duplicates;
       - insert the 33 `asset_chains` feed rows (Standard Proxy addresses).
     - Before writing the script, ask the owner to run `information_schema`
       queries for the live NOT NULL columns (and defaults) of `assets` and
       `chains`, e.g.
       `SELECT table_name,column_name,data_type,is_nullable,column_default FROM information_schema.columns WHERE table_name IN ('assets','chains') ORDER BY table_name,ordinal_position`.
     - The 33 pairs (token on Robinhood Chain = Chainlink Standard Proxy feed on
       Robinhood Chain). Tokens come from `api.robinhood.com/rhj/prices/<SYM>`,
       feeds from Chainlink's Robinhood page. Only the original 10 (GME, MSFT,
       TSLA, SPCX, CRCL, GOOGL, MU, NVDA, AMZN, SNDK) were compared
       byte-for-byte; the other 23 came through a web-summarising tool and must
       be re-checked with `curl https://api.robinhood.com/rhj/prices/<SYM>`
       before seeding. RDDT (0x05b37fb53a299a1b874a619e1c4c404d52c36f4c) is
       official but has no feed. HOODon is not a Robinhood token.

       ```
       AAPL  0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9=0x6B22A786bAa607d76728168703a39Ea9C99f2cD0
       AMD   0x86923f96303D656E4aa86D9d42D1e57ad2023fdC=0x943A29E7ae51A4798823ca9eEd2ed533B2A22C72
       AMZN  0x12f190a9F9d7D37a250758b26824B97CE941bF54=0xD5a1508ceD74c084eBf3cBe853e2C968fB2a651C
       ASML  0x47F93d52cBeC7C6D2CfC080e154002370a60dAEA=0xB4106147E8cce40b7d46124090d373A71b70f87D
       BABA  0xad25Ac6C84D497db898fa1E8387bf6Af3532a1c4=0x62Cc8F9b5f56a33c9C8A60c8B92779f523c4E984
       CLSK  0xcBB95BBF36099d34dA091dc6Fa6F49EfA257Cee3=0x810c12D3a554Bc47fd39597Fe3b3AAC4941F50eF
       COIN  0x6330D8C3178a418788dF01a47479c0ce7CCF450b=0xA3a468A452940B7D6b69991207B508c609a98Ef2
       CRCL  0xdF0992E440dD0be65BD8439b609d6D4366bf1CB5=0x6652eDf64bA3731C4F2D3ce821A0Fb1f1f6b482a
       CRWV  0x5f10A1C971B69e47e059e1dC91901B59b3fB49C3=0xe1b3aABCAFAd1c94708dc1367dcfF8Aa4407487C
       DELL  0x941AE714EC6D8130c7B75d67160Ca08f1e7d11Dd=0x1C6c8cADBe02E19129c39dDB92281cE4c0bf206b
       EWY   0x7f0aBeF0C07280F82c6a08ead09dEd6BAE2C13Fc=0xEFdf54610B62A7753Ec30bDc380847c12D32e1D1
       GME   0x1b0E319c6A659F002271B69dB8A7df2F911c153E=0x27C71df6A64fB476468EdF256CF72c038baB5B67
       GOOGL 0x2e0847E8910a9732eB3fb1bb4b70a580ADAD4FE3=0xF6f373a037c30F0e5010d854385cA89185AE638b
       INTC  0xc72b96e0E48ecd4DC75E1e45396e26300BC39681=0x3f390C5C24628Ac7C489515402235FeAD71D1913
       IONQ  0x558378E000D634A36593E338eBacdd6207640EfE=0x22EfeC4919baf55F360E0EDee4AbEB26DE4971eb
       META  0xc0D6457C16Cc70d6790Dd43521C899C87ce02f35=0x7C38C00C30BEe9378381E7B6135d7283356D71b1
       MSFT  0xe93237C50D904957Cf27E7B1133b510C669c2e74=0x45C3C877C15E6BA2EBB19eA114Ea508d14C1Af2E
       MSTR  0xec262a75e413fAfD0dF80480274532C79D42da09=0x396118bdFB181e6240E74D243F266B061c0edc3D
       MU    0xfF080c8ce2E5feadaCa0Da81314Ae59D232d4afD=0x425EEFdCf05ed6526C3cE61Af99429A228a6d596
       NBIS  0x9D9c6684F596F66a64C030B93A886D51Fd4D7931=0xE1D87B116Ba0fe898998f1D140339D1fA1E09705
       NVDA  0xd0601CE157Db5bdC3162BbaC2a2C8aF5320D9EEC=0x379EC4f7C378F34a1B47E4F3cbeBCbAC3E8E9F15
       ORCL  0xb0992820E760d836549ba69BC7598b4af75dEE03=0x0e6a64a2B58A6693a531E6c555f3A5d042eEA844
       PLTR  0x894E1EC2D74FFE5AEF8Dc8A9e84686acCB964F2A=0x820ABedFF239034956B7A9d2F0a331f9F075eB4c
       QQQ   0xD5f3879160bc7c32ebb4dC785F8a4F505888de68=0x80901d846d5D7B030F26B480776EE3b29374C2ae
       RGTI  0x284358abc07F9359f19f4b5b4aC91901Be2597Ba=0x2A045cF1C49c61c166C036d2f06FA2D2d984f765
       RKLB  0x3b14C39E89D60D627b42a1A4CA45b5bb45Fc12e2=0x045477BF65Aef6f4F2386ad0164579e48381CC74
       SLV   0x411eFb0E7f985935DAec3D4C3ebaEa0d0AD7D89f=0x209b73908e92Ae021826eD79609845451Ecba2ce
       SNDK  0xB90A19fF0Af67f7779afF50A882A9CfF42446400=0xfb133Fa4B7b385802B693a293606682Df47109A3
       SPCX  0x4a0E65A3EcceC6dBe60AE065F2e7bb85Fae35eEa=0xB265810950ba6c5C0Ff821c9963014a56fD8Bffb
       SPY   0x117cc2133c37B721F49dE2A7a74833232B3B4C0C=0x319724394D3A0e3669269846abE664Cd621f9f6A
       TSLA  0x322F0929c4625eD5bAd873c95208D54E1c003b2d=0x4A1166a659A55625345e9515b32adECea5547C38
       TSM   0x58FfE4a942d3885bAa22D7520691F611EF09e7AA=0x874cF94aa8eC88Fd9560094dD065f2fB3E41Fc2F
       USO   0xa30FA36Db767ad9eD3f7a60fC79526fB4d56D344=0x75a9c76Ef439e2C7c2E5a34Ab105EcFe3766431c
       ```
   - (f) After it ships: reset the stuck non-USD calls and the calls that hit
     the data-loss bug so they re-track in USD. Ask the owner first and give him
     the exact SQL.
3. **Longxyz.** It trades against stock tokens, so it is probably covered by the
   feeds plus item 2. Verify after item 2.
4. **Website follow-ups** (small, queued):
   - sAlpha "declined" replies ("Not enough public signals…" / "Too little
     liquidity…") show "sAlpha did not generate a report", with no badge.
   - De-duplicate the Perceptor line (today "no red flags · No red flags
     found").
   - Rugged rows: Peak % shows "–", Status shows "rugged".
   - Huge numbers in 10ⁿ notation (e.g. +3.9×10⁴⁷%), with the exact value in
     the tooltip.
5. **Push updates for new tokens.** The owner confirmed nothing needs to go from
   the page to the server, so use Server-Sent Events, not a WebSocket. When the
   listener records a new token and when its Perceptor or sAlpha report
   arrives, the open page should update without waiting for the next refresh
   and show a notice.
   - Transport: `GET /api/events` (Server-Sent Events, no new dependency).
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
6. **Charts and scatter plot** of call properties against return. The scatter
   plot stays parked until the tracker has finished and the model report shows
   which properties matter.
   - Decided: Apache ECharts 6.1.x, vendored, with a custom theme and CSP-safe
     tooltips (rules in the coder agent). amCharts 5 was compared: polished but
     slow at 12k+ points, no box plot or symlog; its logo would have been
     acceptable.
   - A side-by-side demo is being built in the session scratchpad
     `chartdemo\` for the owner to judge.
7. **Smaller tracker improvements** (not started):
   - A rolling worker queue: today each 50-call cycle waits for its slowest
     call, about 70 s of idle time per new v4 call.
   - `resolveV4` walks back up to 400 × 200k blocks to find `Initialize` (call
     10439 read back to August); cache pool-id currencies.
   - `blockAt` makes uncached head requests.
   - Checkpoint within a horizon segment.
   - The latest pass re-reads blocks that the horizon scan reads later.
   - Flaky test `TestLatestPriceInterruptedLeavesRowUntouched`: fails about 1
     run in 10, on old code as well.
8. **Other call sources** (parked).
   - Best candidates: the Call Analyser channels (@CallAnalyserRobinhood,
     @CallAnalyserETH, @CallAnalyserBase), an aggregator with named callers in a
     parseable format, so the tracker can rank callers by measured ROI. No
     independent win-rate data exists anywhere.
   - Other public call channels were reviewed and not selected.
   - Adding a source needs multi-channel support, a parser per source, a
     caller-name field and a network field. ETH/Base pricing needs a node per
     chain.
   - Suggested first step after the model report: @CallAnalyserRobinhood (same
     chain).

## Agent setup

- `.claude/agents/` has: `coder` (Go + plain JS, with the Go, JS, CSP and
  charts rules), `ml-coder`, `react-coder` (adapted from wshobson/agents, MIT),
  `infra` (approval-gated), `researcher` and `product-manager`.
  `.claude/drafts/reviewer.md` awaits owner approval.
- New agents load only after a Claude Code restart.
- `.claude/` is untracked; the owner decides whether to commit it.
- Pending (#5): tighten `settings.json`: allow read-only git, `node --check`
  and the ml venv pytest path; widen the secret denials to `**/`.

## Already done (asked again recently)

- Token names are read with the ERC-20 `name()` call and stored in
  `scout_call_tracking.token_name` (and `token_symbol_onchain`). The tracker
  fills 200 tokens per cycle.

## Open questions for the owner

- **REMINDER (the owner asked to be reminded): once the re-track reset's
  re-tracking has finished (Pending owner actions, item 1), ask the owner
  whether to also re-track the 1,166 Uniswap v4 calls.** They have no stored
  liquidity (the old code never measured v4), so the liquidity filter cannot
  select them, and drained v4 pools may still hold old results. About 2–4 hours
  of tracker time.
- Should `prior_calls`, `calls_prev_1h` and `calls_prev_24h` (model inputs)
  count real calls only? They still count update posts.
- "Show the expanded pane with the questions": the report detail pane is now
  committed. Ask the owner whether "the questions" means something the pane
  should also show.
- Approve the drafted `reviewer` agent (see "Agent setup"); add a deploy
  checklist?
- A staging copy of `assetdb` would let changes be run against real data
  before production.
- Did he add the full 33-feed `SCOUT_CHAINLINK_FEEDS` line?
- Result of the Pons factory eth_getLogs (Next work item 1).
- Result of the error query under "Prod data findings".

## Deploying

The owner commits and pushes `scout-call-model` from the PC, merges it into
`main`, and deploys `main` on the server: `git checkout main && git pull`, then
restart the three processes:

- `go run ./telegrambot/scoutanalytics -listen-only`
- `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run ./telegrambot/scoutanalytics -track`
  with the latest-price pass on (the owner wants latest prices).
  `SCOUT_RPC_LOG_CHUNK` is unset (default 200000).
- `go run ./telegrambot/scoutanalytics -web`. If a reverse proxy is put in
  front of it, it must pass the `Host` header unchanged, or the same-origin
  check makes `POST /api/refresh` (Refresh now) return 403.

The Nitro node runs with `--execution.rpc.log-history=0`; keep it, or the
tracker slows down again on old ranges.
