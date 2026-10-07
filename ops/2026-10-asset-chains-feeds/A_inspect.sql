-- =============================================================================
-- A_inspect.sql  (step 1 of README.md)  READ-ONLY: changes nothing.
--
-- Run it in pgAdmin's Query Tool, connected to the MAIN API database (the one
-- with the assets / chains / asset_chains tables), with F5 (whole file).
-- The result is ONE grid with three text columns: section, item, detail.
-- Paste the whole grid to the PM. What to look for is in README.md
-- ("What to check in A before editing B").
--
-- Sections, in this order:
--   1 server       PostgreSQL version and the session time zone
--   2 column       every column of chains, assets, asset_chains,
--                  structured_values: type, NULL / NOT NULL, default
--   3 constraint   primary / unique / foreign keys of chains, assets,
--                  asset_chains
--   4 sequence     id sequences: last value vs max(id) (B inserts with the
--                  default id, so the sequence must be ahead of max(id))
--   5 chain        chains rows with EVM chain_id 1 or 4663, or a name like
--                  'robinhood'; then whether the Robinhood Chain row exists
--   6 asset_type   asset_type_id values in use (structured_values name, how
--                  many assets), then structured_values whose name looks like
--                  stock / equity / token / share / etf / crypto
--   7 token        each of the 33 Robinhood stock tokens: its assets row(s)
--                  (contract address compared lower-case), or MISSING
--   8 asset_chain  existing asset_chains rows for those tokens, or with one of
--                  the 33 feed addresses
--   9 summary      counts
-- =============================================================================

WITH pairs(ticker, token, feed) AS (VALUES   -- ticker, Robinhood Chain token, Chainlink feed (proxy) on Robinhood Chain
  ('AAPL',  '0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9', '0x6B22A786bAa607d76728168703a39Ea9C99f2cD0'),
  ('AMD',   '0x86923f96303D656E4aa86D9d42D1e57ad2023fdC', '0x943A29E7ae51A4798823ca9eEd2ed533B2A22C72'),
  ('AMZN',  '0x12f190a9F9d7D37a250758b26824B97CE941bF54', '0xD5a1508ceD74c084eBf3cBe853e2C968fB2a651C'),
  ('ASML',  '0x47F93d52cBeC7C6D2CfC080e154002370a60dAEA', '0xB4106147E8cce40b7d46124090d373A71b70f87D'),
  ('BABA',  '0xad25Ac6C84D497db898fa1E8387bf6Af3532a1c4', '0x62Cc8F9b5f56a33c9C8A60c8B92779f523c4E984'),
  ('CLSK',  '0xcBB95BBF36099d34dA091dc6Fa6F49EfA257Cee3', '0x810c12D3a554Bc47fd39597Fe3b3AAC4941F50eF'),
  ('COIN',  '0x6330D8C3178a418788dF01a47479c0ce7CCF450b', '0xA3a468A452940B7D6b69991207B508c609a98Ef2'),
  ('CRCL',  '0xdF0992E440dD0be65BD8439b609d6D4366bf1CB5', '0x6652eDf64bA3731C4F2D3ce821A0Fb1f1f6b482a'),
  ('CRWV',  '0x5f10A1C971B69e47e059e1dC91901B59b3fB49C3', '0xe1b3aABCAFAd1c94708dc1367dcfF8Aa4407487C'),
  ('DELL',  '0x941AE714EC6D8130c7B75d67160Ca08f1e7d11Dd', '0x1C6c8cADBe02E19129c39dDB92281cE4c0bf206b'),
  ('EWY',   '0x7f0aBeF0C07280F82c6a08ead09dEd6BAE2C13Fc', '0xEFdf54610B62A7753Ec30bDc380847c12D32e1D1'),
  ('GME',   '0x1b0E319c6A659F002271B69dB8A7df2F911c153E', '0x27C71df6A64fB476468EdF256CF72c038baB5B67'),
  ('GOOGL', '0x2e0847E8910a9732eB3fb1bb4b70a580ADAD4FE3', '0xF6f373a037c30F0e5010d854385cA89185AE638b'),
  ('INTC',  '0xc72b96e0E48ecd4DC75E1e45396e26300BC39681', '0x3f390C5C24628Ac7C489515402235FeAD71D1913'),
  ('IONQ',  '0x558378E000D634A36593E338eBacdd6207640EfE', '0x22EfeC4919baf55F360E0EDee4AbEB26DE4971eb'),
  ('META',  '0xc0D6457C16Cc70d6790Dd43521C899C87ce02f35', '0x7C38C00C30BEe9378381E7B6135d7283356D71b1'),
  ('MSFT',  '0xe93237C50D904957Cf27E7B1133b510C669c2e74', '0x45C3C877C15E6BA2EBB19eA114Ea508d14C1Af2E'),
  ('MSTR',  '0xec262a75e413fAfD0dF80480274532C79D42da09', '0x396118bdFB181e6240E74D243F266B061c0edc3D'),
  ('MU',    '0xfF080c8ce2E5feadaCa0Da81314Ae59D232d4afD', '0x425EEFdCf05ed6526C3cE61Af99429A228a6d596'),
  ('NBIS',  '0x9D9c6684F596F66a64C030B93A886D51Fd4D7931', '0xE1D87B116Ba0fe898998f1D140339D1fA1E09705'),
  ('NVDA',  '0xd0601CE157Db5bdC3162BbaC2a2C8aF5320D9EEC', '0x379EC4f7C378F34a1B47E4F3cbeBCbAC3E8E9F15'),
  ('ORCL',  '0xb0992820E760d836549ba69BC7598b4af75dEE03', '0x0e6a64a2B58A6693a531E6c555f3A5d042eEA844'),
  ('PLTR',  '0x894E1EC2D74FFE5AEF8Dc8A9e84686acCB964F2A', '0x820ABedFF239034956B7A9d2F0a331f9F075eB4c'),
  ('QQQ',   '0xD5f3879160bc7c32ebb4dC785F8a4F505888de68', '0x80901d846d5D7B030F26B480776EE3b29374C2ae'),
  ('RGTI',  '0x284358abc07F9359f19f4b5b4aC91901Be2597Ba', '0x2A045cF1C49c61c166C036d2f06FA2D2d984f765'),
  ('RKLB',  '0x3b14C39E89D60D627b42a1A4CA45b5bb45Fc12e2', '0x045477BF65Aef6f4F2386ad0164579e48381CC74'),
  ('SLV',   '0x411eFb0E7f985935DAec3D4C3ebaEa0d0AD7D89f', '0x209b73908e92Ae021826eD79609845451Ecba2ce'),
  ('SNDK',  '0xB90A19fF0Af67f7779afF50A882A9CfF42446400', '0xfb133Fa4B7b385802B693a293606682Df47109A3'),
  ('SPCX',  '0x4a0E65A3EcceC6dBe60AE065F2e7bb85Fae35eEa', '0xB265810950ba6c5C0Ff821c9963014a56fD8Bffb'),
  ('SPY',   '0x117cc2133c37B721F49dE2A7a74833232B3B4C0C', '0x319724394D3A0e3669269846abE664Cd621f9f6A'),
  ('TSLA',  '0x322F0929c4625eD5bAd873c95208D54E1c003b2d', '0x4A1166a659A55625345e9515b32adECea5547C38'),
  ('TSM',   '0x58FfE4a942d3885bAa22D7520691F611EF09e7AA', '0x874cF94aa8eC88Fd9560094dD065f2fB3E41Fc2F'),
  ('USO',   '0xa30FA36Db767ad9eD3f7a60fC79526fB4d56D344', '0x75a9c76Ef439e2C7c2E5a34Ab105EcFe3766431c')
),
cols AS (
  SELECT c.table_name::text AS table_name, c.ordinal_position::int AS pos, c.column_name::text AS column_name,
         c.data_type || CASE WHEN c.character_maximum_length IS NOT NULL
                             THEN '(' || c.character_maximum_length || ')' ELSE '' END
         || CASE WHEN c.is_nullable = 'NO' THEN ' NOT NULL' ELSE ' NULL' END
         || COALESCE(' DEFAULT ' || c.column_default, '') AS detail
  FROM information_schema.columns c
  WHERE c.table_schema = current_schema()
    AND c.table_name IN ('chains','assets','asset_chains','structured_values')),
cons AS (
  SELECT r.relname::text AS table_name, k.conname::text AS conname, pg_get_constraintdef(k.oid) AS def
  FROM pg_constraint k JOIN pg_class r ON r.oid = k.conrelid
  WHERE r.relnamespace = current_schema()::regnamespace
    AND r.relname IN ('chains','assets','asset_chains') AND k.contype IN ('p','u','f')),
seqs AS (
  SELECT 'chains' AS table_name, pg_get_serial_sequence('chains','id') AS seq, (SELECT max(id) FROM chains) AS max_id
  UNION ALL
  SELECT 'assets', pg_get_serial_sequence('assets','id'), (SELECT max(id) FROM assets)),
tok AS (
  SELECT p.ticker, p.token, a.id, a.chain_id, ch.chain_id AS evm, a.asset_type_id, a.name, a.ticker AS a_ticker,
         a.ignore_market_data, a.import_geth,
         COALESCE(bool_or(ch.chain_id = 4663) OVER (PARTITION BY p.ticker), false) AS has_rh
  FROM pairs p
  LEFT JOIN assets a ON lower(btrim(a.contract_address)) = lower(p.token)
  LEFT JOIN chains ch ON ch.id = a.chain_id),
ac AS (
  SELECT DISTINCT x.asset_id, x.chain_id, x.chainlink_data_feed_contract_address AS feed,
         a.ticker, ch.chain_id AS evm
  FROM asset_chains x
  JOIN assets a ON a.id = x.asset_id
  LEFT JOIN chains ch ON ch.id = x.chain_id
  WHERE lower(btrim(a.contract_address)) IN (SELECT lower(token) FROM pairs)
     OR lower(btrim(x.chainlink_data_feed_contract_address)) IN (SELECT lower(feed) FROM pairs)),
grid(sec, ord, item, detail) AS (
  SELECT '1 server', 0, 'version', version()
  UNION ALL
  SELECT '1 server', 1, 'TimeZone (session)', current_setting('TimeZone')
  UNION ALL
  SELECT '2 column', pos, table_name || '.' || column_name, detail FROM cols
  UNION ALL
  SELECT '3 constraint', 0, table_name || '.' || conname, def FROM cons
  UNION ALL
  SELECT '4 sequence', 0, table_name || '.id',
         COALESCE(seq, '(no serial sequence)') || ': last_value '
         || COALESCE((SELECT s.last_value::text FROM pg_sequences s
                      WHERE s.schemaname || '.' || s.sequencename = seqs.seq
                         OR quote_ident(s.schemaname) || '.' || quote_ident(s.sequencename) = seqs.seq),
                     'unknown (never used or no privilege)')
         || ', max(id) ' || COALESCE(max_id::text, 'none')
  FROM seqs
  UNION ALL
  SELECT '5 chain', id, 'chains.id ' || id,
         'chain_id ' || COALESCE(chain_id::text, 'NULL') || ', name ' || COALESCE(name::text, 'NULL')
         || ', alternate_name ' || COALESCE(alternate_name::text, 'NULL')
         || ', chain_type_id ' || COALESCE(chain_type_id::text, 'NULL')
         || ', base_asset_id ' || COALESCE(base_asset_id::text, 'NULL')
         || ', rpc_url ' || COALESCE(rpc_url::text, 'NULL')
         || ', block_explorer_url ' || COALESCE(block_explorer_url::text, 'NULL')
  FROM chains WHERE chain_id IN (1, 4663) OR name ILIKE '%robinhood%'
  UNION ALL
  SELECT '5 chain', 999999999, 'Robinhood Chain (EVM 4663)',
         CASE count(*) WHEN 0 THEN 'MISSING: B inserts it' WHEN 1 THEN 'present: B uses it'
              ELSE count(*) || ' rows: B stops (ambiguous)' END
  FROM chains WHERE chain_id = 4663
  UNION ALL
  SELECT '6 asset_type', COALESCE(a.asset_type_id, -1), 'asset_type_id ' || COALESCE(a.asset_type_id::text, 'NULL'),
         COALESCE(sv.name::text, '?') || ': ' || count(*) || ' asset(s)'
  FROM assets a LEFT JOIN structured_values sv ON sv.id = a.asset_type_id
  GROUP BY a.asset_type_id, sv.name
  UNION ALL
  SELECT '6 asset_type', 1000000 + sv.id, 'structured_values.id ' || sv.id,
         sv.name || COALESCE(' / ' || sv.alternate_name, '')
         || ' (structured_value_type_id ' || COALESCE(sv.structured_value_type_id::text, 'NULL') || ')'
  FROM structured_values sv
  WHERE sv.name ILIKE ANY (ARRAY['%stock%','%equit%','%token%','%share%','%etf%','%crypto%'])
  UNION ALL
  SELECT '7 token', 0, ticker || ' ' || token,
         CASE WHEN id IS NULL THEN 'MISSING: B inserts it'
              ELSE 'assets.id ' || id || ', chain ' || COALESCE(chain_id::text, 'NULL')
                   || ' (EVM ' || COALESCE(evm::text, 'NULL') || ')'
                   || CASE WHEN evm = 4663 THEN ''
                           WHEN has_rh THEN ' <<< NOT on Robinhood Chain (ignored: the token also has a Robinhood Chain row)'
                           ELSE ' <<< NOT on Robinhood Chain: B stops' END
                   || ', asset_type_id ' || COALESCE(asset_type_id::text, 'NULL')
                   || ', name ' || COALESCE(name::text, 'NULL') || ', ticker ' || COALESCE(a_ticker::text, 'NULL')
                   || ', ignore_market_data ' || COALESCE(ignore_market_data::text, 'NULL')
                   || ', import_geth ' || COALESCE(import_geth::text, 'NULL') END
  FROM tok
  UNION ALL
  SELECT '8 asset_chain', asset_id, 'asset ' || asset_id || ' (' || COALESCE(ticker::text, '?') || '), chains.id ' || chain_id,
         'EVM ' || COALESCE(evm::text, 'NULL') || ', feed ' || feed
         || CASE WHEN lower(btrim(feed)) IN (SELECT lower(p.feed) FROM pairs p) THEN '' ELSE ' <<< not one of the 33 feeds' END
  FROM ac
  UNION ALL
  SELECT '9 summary', 1, 'pairs in this file', count(*)::text FROM pairs
  UNION ALL
  SELECT '9 summary', 2, 'tokens already in assets (any chain)',
         count(DISTINCT ticker)::text FROM tok WHERE id IS NOT NULL
  UNION ALL
  SELECT '9 summary', 3, 'tokens in assets but NOT on Robinhood Chain (B stops)',
         count(DISTINCT ticker)::text FROM tok WHERE id IS NOT NULL AND NOT has_rh
  UNION ALL
  SELECT '9 summary', 4, 'tokens with more than one assets row on Robinhood Chain (B uses the lowest id)',
         count(*)::text FROM (SELECT ticker FROM tok WHERE evm = 4663 GROUP BY ticker HAVING count(*) > 1) d
  UNION ALL
  SELECT '9 summary', 5, 'asset_chains rows found (section 8)', count(*)::text FROM ac)
SELECT sec AS section, item, detail FROM grid ORDER BY sec, ord, item;
