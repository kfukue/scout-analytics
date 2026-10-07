# Seed the Robinhood Chain stock-token feeds (pgAdmin version)

> **One-off, October 2026.** These scripts add the 33 Robinhood stock tokens
> and their Chainlink price feeds to the MAIN API's shared tables (`chains`,
> `assets`, `asset_chains`), where the scout program's feed loader
> (`feeds_db.go`) reads them. They are run **by hand in pgAdmin**, connected to
> the main API database; the program never executes them (nothing under
> `ops/` is compiled or embedded). Same conventions as `ops/2026-10-rug-retrack/`.

## What it does

- `chains`: adds Robinhood Chain (EVM chain id 4663) if no row has
  `chain_id = 4663`. Remember: `chains.id` is the internal id, `chains.chain_id`
  is the EVM chain id; `assets.chain_id` and `asset_chains.chain_id` hold
  `chains.id`.
- `assets`: adds each of the 33 tokens that is not there yet (looked up by
  contract address, lower-case, on Robinhood Chain): name = ticker = the
  ticker, the address as listed, `ignore_market_data = true`,
  `import_geth = false`, `import_geth_initial = false`,
  `is_default_quote = false`, `decimals` NULL, `asset_type_id` as you set it.
  `assets` has no unique key on the address, so B never inserts a token that
  is already there, and never changes an existing row.
- `asset_chains`: one row per token: (the token's asset, Robinhood Chain, the
  Chainlink feed proxy on Robinhood Chain), `ON CONFLICT DO NOTHING`.
- `created_by` / `updated_by` = `scoutanalytics-seed` on every new row.

The token addresses were checked byte for byte against
`https://api.robinhood.com/rhj/prices/<TICKER>` (`deployments[].contractAddress`
with `chainId` 4663) on 2026-10-07: all 33 match. The feed addresses come from
the task list (Chainlink Standard Proxy on Robinhood Chain); they were not
checked against Chainlink here.

## Files

| File             | Kind      | Purpose                                                        |
|------------------|-----------|----------------------------------------------------------------|
| `A_inspect.sql`  | read-only | live columns, keys, sequences, chain rows, asset types, which tokens/feeds exist |
| `B_seed.sql`     | WRITES    | the seed, one transaction, checks the end state                |
| `C_verify.sql`   | read-only | what the scout feed loader will read (the loader's join)       |

## Steps

1. Run `A_inspect.sql` (F5) and paste the whole grid to the PM. Wait for the
   go-ahead and for the values to use in B.
2. Edit the lines marked `EDIT` at the top of `B_seed.sql` (see the next
   section): `expected_count` = 33, `asset_type_id`, and, only if A said the
   Robinhood Chain row is MISSING, the chain row's guessed values.
3. Run `B_seed.sql` (F5). The grid shows what it inserted and skipped:
   first run on prod, expected `chain_inserted 1, assets_inserted 33,
   feeds_inserted 33, feeds_ok_now 33`. If it stops with an ERROR, nothing
   was changed: run `ROLLBACK;` in the same tab (or close it), paste the
   error to the PM.
4. Run `C_verify.sql` (F5). The SUMMARY row must say
   `33 ok, 0 missing, 0 feed differs, 0 not listed, 0 token(s) read more than once`.
5. Make the scout program pick the feeds up (how it loads them, at start or
   periodically, is decided with `feeds_db.go`; restarting it always works).

## What to check in A before editing B

- **1 server**: nothing to do; the version is for the record.
- **2 column**: every NOT NULL column without a default of `chains` and
  `assets` must be one that B fills. B fills: chains `uuid, name,
  alternate_name, description, chain_type_id, chain_id, rpc_url,
  block_explorer_url, created_by, created_at, updated_by, updated_at`;
  assets `uuid, name, ticker, description, asset_type_id, chain_id,
  contract_address, is_default_quote, ignore_market_data, import_geth,
  import_geth_initial, created_by, created_at, updated_by, updated_at`;
  asset_chains all its columns. If the live tables have another NOT NULL
  column without a default, tell the PM (B would stop with a NOT NULL error).
  `created_at` / `updated_at` of chains and assets are `timestamp` (no time
  zone) in the library's schema; B writes them as UTC, like the main API.
- **3 constraint**: `asset_chains` should have `PRIMARY KEY (asset_id,
  chain_id)` (B's `ON CONFLICT DO NOTHING` relies on a unique key there).
- **4 sequence**: for chains and assets, `last_value` must be at least
  `max(id)`; otherwise B fails with a duplicate key (tell the PM; nothing is
  changed).
- **5 chain**: "Robinhood Chain (EVM 4663)": MISSING = B inserts it with the
  guessed values; present = B uses it and ignores the guessed values;
  "N rows" = B stops, a person must pick one. The Ethereum row
  (chain_id 1) shows its `chain_type_id`, which B copies by default.
- **6 asset_type**: pick the `structured_values.id` the main API uses for
  stocks (or the closest type) and put it into `asset_type_id` in B. This
  is a decision for the owner; B stops while it is NULL.
- **7 token**: every token should be "MISSING: B inserts it" (prod had 0
  assets on Robinhood Chain). A token marked "NOT on Robinhood Chain: B
  stops" is already in `assets` with the same address under another chain
  (or none): B refuses to guess; the owner decides (fix that row's
  `chain_id`, or delete it) and runs A again.
- **8 asset_chain**: should be empty on the first run. A row "not one of the
  33 feeds" on Robinhood Chain makes B stop (its check finds the feed differs).
- **9 summary**: 33 pairs; the other counts explain the sections above.

## How the check (expected_count) works

Unlike the re-track scripts, the count is checked on the END state: after its
inserts, B counts the tokens that the loader's join (the same as in
`C_verify.sql`) resolves to exactly their listed feed, and stops, changing
nothing, unless that number is `expected_count` (33). So a wrong count, an
unset value, or an existing row with a different feed rolls everything back,
and a second run is harmless: it inserts nothing, reports 0 inserted / 33
skipped, and the check still passes.

## Notes

- B locks `chains`, `assets` and `asset_chains` against writes (reads go on)
  for its few milliseconds, so that the main API cannot add the same token at
  the same time. If the main API holds those tables for more than 5 seconds,
  B stops with a lock timeout error and changes nothing; run it again later.
- A failed run still uses up sequence numbers, so the new ids can have gaps.
  That is normal in PostgreSQL.
- B uses `uuid_generate_v4()` (the uuid-ossp extension), which the main API
  already uses for its own inserts.
- The chains row's guessed values (name "Robinhood Chain", alternate name
  "robinhood", description, `chain_type_id` copied from Ethereum, no RPC URL,
  no explorer URL) can be corrected later in the main API; nothing in scout
  reads them except `chain_id` = 4663.
- pgAdmin: keep "Auto commit" ON. B has its own `BEGIN ... COMMIT`; A and C
  are a single read-only SELECT. pgAdmin shows only the LAST result set. B
  starts with `ROLLBACK;`, so "WARNING: there is no transaction in progress"
  and "NOTICE: schema "pg_temp" does not exist, skipping" are expected. B
  leaves three small temporary tables (`feedseed_*`) in the session; they
  disappear when the tab is closed.
