-- =============================================================================
-- B_seed.sql  (step 3 of README.md)  WRITES: seeds the Robinhood Chain feeds.
--
-- Run it in pgAdmin's Query Tool, connected to the MAIN API database, with F5.
--
-- BEFORE YOU RUN IT (see README.md, "What to check in A before editing B")
--   * asset_type_id (EDIT line below): the structured_values id to give the
--     new stock-token asset rows. B stops while it is NULL.
--   * expected_count (EDIT line below): 33, the number of pairs in this file.
--     B stops while it is -1.
--   * The Robinhood Chain row lines (EDIT, GUESSED): only used when A said
--     "Robinhood Chain (EVM 4663) MISSING". Check name, alternate_name,
--     chain_type_id, rpc_url, block_explorer_url.
--
-- WHAT IT DOES (one transaction, all or nothing; created_by / updated_by =
-- 'scoutanalytics-seed')
--   1. chains: inserts the Robinhood Chain row (EVM chain_id 4663) only if no
--      row has chain_id 4663. Stops if more than one row has it.
--   2. assets: for each of the 33 tokens, finds its row by contract address
--      (lower-case) on Robinhood Chain; if there are several, the lowest id.
--      Missing tokens are inserted: name = ticker = the ticker, the address as
--      listed (checksummed), chain = Robinhood Chain, asset_type_id from
--      below, ignore_market_data = true, import_geth = false,
--      import_geth_initial = false, is_default_quote = false, decimals NULL.
--      Existing asset rows are never changed.
--      STOPS if a token's address is already in assets but only on another
--      chain (or with no chain): a person decides (see README).
--   3. asset_chains: (asset, Robinhood Chain, feed) for each token,
--      ON CONFLICT DO NOTHING. An existing row is never changed.
--   4. Check (the expected-count guard): afterwards exactly expected_count of
--      the 33 tokens must resolve through the loader's join (C_verify.sql) to
--      their listed feed. If not (e.g. an existing asset_chains row holds a
--      different feed), it STOPS and nothing is changed.
--   5. Commits and shows ONE row with the counts inserted and skipped.
--
-- Safe to run twice: the second run inserts nothing (0 inserted, 33 skipped)
-- and the check still passes.
--
-- IF IT STOPS WITH AN ERROR
--   Nothing was changed. Run ROLLBACK; in the same tab (or close the tab).
--   This file also starts with ROLLBACK, so running it again is safe.
--   The "WARNING: there is no transaction in progress" from the first line and
--   "NOTICE: schema "pg_temp" does not exist, skipping" are expected.
-- =============================================================================

ROLLBACK;  -- clears a failed earlier attempt in this tab; harmless otherwise
BEGIN;
SET LOCAL TIME ZONE 'UTC';  -- now() into a "timestamp" column = UTC, as the main API writes it

DROP TABLE IF EXISTS pg_temp.feedseed_params, pg_temp.feedseed_pairs, pg_temp.feedseed_result;
CREATE TEMP TABLE feedseed_params AS
SELECT -1                 AS expected_count,   -- <<< EDIT: 33 (the number of pairs below)
       NULL::int          AS asset_type_id,    -- <<< EDIT: structured_values id for the new stock-token assets (A, section 6)
       'scoutanalytics-seed'::text AS who,
       -- The Robinhood Chain row, used ONLY if no chains row has chain_id 4663. GUESSED values:
       'Robinhood Chain'::text     AS chain_name,            -- <<< EDIT (guessed)
       'robinhood'::text           AS chain_alternate_name,  -- <<< EDIT (guessed)
       'Robinhood Chain (EVM chain id 4663); stock tokens priced by Chainlink feeds'::text
                                   AS chain_description,     -- <<< EDIT (guessed)
       (SELECT min(chain_type_id) FROM chains WHERE chain_id = 1)
                                   AS chain_type_id,         -- <<< EDIT (guessed: the same as Ethereum's row)
       NULL::text                  AS chain_rpc_url,         -- <<< EDIT (unknown: left NULL)
       NULL::text                  AS chain_block_explorer_url; -- <<< EDIT (unknown: left NULL)

-- ticker, token on Robinhood Chain, Chainlink feed (proxy) on Robinhood Chain.
-- Token addresses checked against api.robinhood.com/rhj/prices/<ticker> (chainId 4663).
CREATE TEMP TABLE feedseed_pairs (ticker text NOT NULL, token text NOT NULL, feed text NOT NULL,
                                  asset_id int, asset_inserted boolean NOT NULL DEFAULT false);
INSERT INTO feedseed_pairs (ticker, token, feed) VALUES
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
;

CREATE TEMP TABLE feedseed_result (chain_inserted int, rh_chain_id int, assets_inserted int, assets_skipped int,
                                   feeds_inserted int, feeds_skipped int, feeds_ok_now int);

-- Keep other writers (the main API) from adding chains / assets / feeds while this runs.
-- If another transaction holds them for more than 5 s, stop (nothing changed) instead of
-- making the main API's writes queue behind this script; just run it again later.
SET LOCAL lock_timeout = '5s';
LOCK TABLE chains, assets, asset_chains IN SHARE ROW EXCLUSIVE MODE;

DO $$
DECLARE
  p          feedseed_params%ROWTYPE;
  n_rh       int;
  rh_id      int;
  chain_ins  int := 0;
  n_pairs    int;
  bad        text;
  a_ins      int;
  f_ins      int;
  ok_now     int;
BEGIN
  SELECT * INTO p FROM feedseed_params;
  SELECT count(*) INTO n_pairs FROM feedseed_pairs;
  IF p.expected_count IS NULL OR p.expected_count < 0 THEN
    RAISE EXCEPTION 'B_seed: set expected_count at the top of the file first (this file has % pair(s)); nothing was changed', n_pairs;
  END IF;
  IF p.asset_type_id IS NULL THEN
    RAISE EXCEPTION 'B_seed: set asset_type_id at the top of the file first (see A, section 6); nothing was changed';
  END IF;
  IF (SELECT count(DISTINCT lower(token)) FROM feedseed_pairs) <> n_pairs
     OR (SELECT count(DISTINCT lower(feed)) FROM feedseed_pairs) <> n_pairs THEN
    RAISE EXCEPTION 'B_seed: the pair list has a repeated token or feed address; nothing was changed';
  END IF;

  -- 1. chains
  SELECT count(*), min(id) INTO n_rh, rh_id FROM chains WHERE chain_id = 4663;
  IF n_rh > 1 THEN
    RAISE EXCEPTION 'B_seed: % chains rows have chain_id 4663; a person must decide which one; nothing was changed', n_rh;
  END IF;
  IF n_rh = 0 THEN
    INSERT INTO chains (uuid, name, alternate_name, description, chain_type_id, chain_id, rpc_url, block_explorer_url,
                        created_by, created_at, updated_by, updated_at)
    VALUES (uuid_generate_v4(), p.chain_name, p.chain_alternate_name, p.chain_description, p.chain_type_id, 4663,
            p.chain_rpc_url, p.chain_block_explorer_url, p.who, now(), p.who, now())
    RETURNING id INTO rh_id;
    chain_ins := 1;
  END IF;

  -- 2. assets: a token already present only on another chain (or none) stops everything
  SELECT string_agg(f.ticker || ' (assets.id ' || ids || ')', ', ' ORDER BY f.ticker) INTO bad
  FROM (SELECT fp.ticker, string_agg(a.id::text, '/' ORDER BY a.id) AS ids
        FROM feedseed_pairs fp
        JOIN assets a ON lower(btrim(a.contract_address)) = lower(fp.token)
        GROUP BY fp.ticker
        HAVING NOT bool_or(a.chain_id IS NOT DISTINCT FROM rh_id)) f;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'B_seed: these tokens are already in assets, but not on Robinhood Chain (chains.id %): %. A person must decide; nothing was changed', rh_id, bad;
  END IF;

  UPDATE feedseed_pairs fp
  SET asset_id = (SELECT min(a.id) FROM assets a
                  WHERE lower(btrim(a.contract_address)) = lower(fp.token) AND a.chain_id = rh_id);

  WITH ins AS (
    INSERT INTO assets (uuid, name, ticker, description, asset_type_id, chain_id, contract_address,
                        is_default_quote, ignore_market_data, import_geth, import_geth_initial,
                        created_by, created_at, updated_by, updated_at)
    SELECT uuid_generate_v4(), fp.ticker, fp.ticker, fp.ticker || ' stock token on Robinhood Chain',
           p.asset_type_id, rh_id, fp.token,
           false, true, false, false,
           p.who, now(), p.who, now()
    FROM feedseed_pairs fp
    WHERE fp.asset_id IS NULL
    RETURNING id, contract_address)
  UPDATE feedseed_pairs fp SET asset_id = ins.id, asset_inserted = true
  FROM ins WHERE lower(ins.contract_address) = lower(fp.token);
  GET DIAGNOSTICS a_ins = ROW_COUNT;

  -- 3. asset_chains
  INSERT INTO asset_chains (asset_id, chain_id, chainlink_data_feed_contract_address,
                            created_by, created_at, updated_by, updated_at)
  SELECT fp.asset_id, rh_id, fp.feed, p.who, now(), p.who, now()
  FROM feedseed_pairs fp
  ON CONFLICT DO NOTHING;
  GET DIAGNOSTICS f_ins = ROW_COUNT;

  -- 4. the expected-count guard, on the end state: tokens that resolve through
  --    the loader's join (as in C_verify.sql) to exactly their listed feed
  SELECT count(DISTINCT fp.ticker) INTO ok_now
  FROM feedseed_pairs fp
  JOIN asset_chains ac ON lower(btrim(ac.chainlink_data_feed_contract_address)) = lower(fp.feed)
  JOIN assets a ON a.id = ac.asset_id AND lower(btrim(a.contract_address)) = lower(fp.token)
  JOIN chains tc ON tc.id = a.chain_id AND tc.chain_id = 4663
  JOIN chains fc ON fc.id = ac.chain_id AND fc.chain_id = 4663;
  IF ok_now <> p.expected_count THEN
    SELECT string_agg(fp.ticker || ' (existing feed ' || ac.chainlink_data_feed_contract_address || ')', ', ' ORDER BY fp.ticker)
      INTO bad
    FROM feedseed_pairs fp
    JOIN asset_chains ac ON ac.asset_id = fp.asset_id AND ac.chain_id = rh_id
    WHERE lower(btrim(ac.chainlink_data_feed_contract_address)) <> lower(fp.feed);
    RAISE EXCEPTION 'B_seed: % of % token(s) resolve to their listed feed, but expected_count is %; feed differs for: %; nothing was changed',
      ok_now, n_pairs, p.expected_count, COALESCE(bad, 'none');
  END IF;

  INSERT INTO feedseed_result VALUES (chain_ins, rh_id, a_ins, n_pairs - a_ins, f_ins, n_pairs - f_ins, ok_now);
END
$$;

COMMIT;

-- The result grid.
SELECT chain_inserted,   -- 1 = the Robinhood Chain row was inserted now
       rh_chain_id,      -- chains.id of Robinhood Chain (EVM 4663)
       assets_inserted, assets_skipped,
       feeds_inserted,  feeds_skipped,
       feeds_ok_now      -- tokens that now resolve to their feed (= expected_count)
FROM feedseed_result;
