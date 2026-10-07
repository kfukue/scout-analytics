package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Chainlink feeds from the asset database (read-only)
//
// The asset tracker's tables, when they are in the same database:
//
//	asset_chains (asset_id, chain_id, chainlink_data_feed_contract_address)
//	  ⋈ assets (id, contract_address, chain_id)
//	  ⋈ chains (id, chain_id = the EVM chain id)
//
// Tokens on Robinhood Chain (EVM 4663) with a feed on Robinhood Chain go to
// the Robinhood feed map, with a feed on Ethereum mainnet (EVM 1) to the
// mainnet map. They add to SCOUT_CHAINLINK_FEEDS / SCOUT_MAINNET_CHAINLINK_FEEDS;
// on a conflict the environment wins. Read at the start of the tracker and
// again every cycle; nothing is ever written.
// ---------------------------------------------------------------------------

const (
	robinhoodChainID = 4663 // Robinhood Chain's EVM chain id
	mainnetChainID   = 1    // Ethereum mainnet
	feedsDBTimeout   = 10 * time.Second
)

// feedMaps: the Chainlink feeds in use, token (lower case) or "eth" → feed
// (lower case). A value is never changed once published: a reload stores a
// new one (onchainSource.feeds), so readers need no lock.
type feedMaps struct {
	rh, mainnet     map[string]string
	dbRH, dbMainnet int // entries that came from the database (the rest are env)
}

// feedMaps returns the feeds in use.
func (o *onchainSource) feedMaps() *feedMaps {
	if m := o.feeds.Load(); m != nil {
		return m
	}
	return &feedMaps{rh: o.cfg.Feeds, mainnet: o.cfg.MainnetFeeds}
}

// mergeFeeds: db plus env, env winning on a conflict. conflicts counts the db
// entries env overrides with a different feed.
func mergeFeeds(env, db map[string]string) (out map[string]string, fromDB, conflicts int) {
	out = make(map[string]string, len(env)+len(db))
	for k, v := range db {
		if e, ok := env[k]; ok {
			if e != v {
				conflicts++
			}
			continue
		}
		out[k] = v
		fromDB++
	}
	maps.Copy(out, env)
	return out, fromDB, conflicts
}

// setDBFeeds publishes the env feeds merged with the database's (nil maps =
// env only). It returns how many env entries overrode a different database feed.
func (o *onchainSource) setDBFeeds(rh, mainnet map[string]string) (conflicts int) {
	m := &feedMaps{}
	var c1, c2 int
	m.rh, m.dbRH, c1 = mergeFeeds(o.cfg.Feeds, rh)
	m.mainnet, m.dbMainnet, c2 = mergeFeeds(o.cfg.MainnetFeeds, mainnet)
	o.feeds.Store(m)
	return c1 + c2
}

// dbFeeds is the database side of the feeds: the last rows read and the last
// problem logged (so a failing database is logged once, not every cycle).
type dbFeeds struct {
	mu       sync.Mutex
	loaded   bool              // a read has succeeded (its result is in use)
	rh, mn   map[string]string // the last rows read
	lastProb string            // the last problem logged ("" = none)
}

var addrRe = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

// ChainlinkFeeds reads the Chainlink feeds of Robinhood Chain tokens from the
// asset tables: rh for feeds on Robinhood Chain, mainnet for feeds on Ethereum.
// Addresses are lower case; when two assets share a contract address the one
// with the lowest asset id wins. Rows with a malformed address are skipped
// (counted in bad).
func (st *ScoutStore) ChainlinkFeeds(ctx context.Context) (rh, mainnet map[string]string, bad int, err error) {
	rows, err := st.Pool.Query(ctx, `
		SELECT lower(btrim(a.contract_address)), fc.chain_id, lower(btrim(ac.chainlink_data_feed_contract_address))
		FROM asset_chains ac
		JOIN assets a ON a.id = ac.asset_id
		JOIN chains tc ON tc.id = a.chain_id
		JOIN chains fc ON fc.id = ac.chain_id
		WHERE tc.chain_id = $1 AND fc.chain_id = ANY($2::int[])
		  AND a.contract_address IS NOT NULL
		  AND ac.chainlink_data_feed_contract_address IS NOT NULL
		ORDER BY 1, 2, ac.asset_id, 3`, robinhoodChainID, []int32{robinhoodChainID, mainnetChainID})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("reading chainlink feeds: %w", err)
	}
	defer rows.Close()
	rh, mainnet = map[string]string{}, map[string]string{}
	for rows.Next() {
		var token, feed string
		var chain int32
		if err := rows.Scan(&token, &chain, &feed); err != nil {
			return nil, nil, 0, fmt.Errorf("reading chainlink feeds: %w", err)
		}
		if !addrRe.MatchString(token) || !addrRe.MatchString(feed) {
			bad++
			continue
		}
		dst := rh
		if chain == mainnetChainID {
			dst = mainnet
		}
		if _, ok := dst[token]; !ok { // the first row (lowest asset id) wins
			dst[token] = feed
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("reading chainlink feeds: %w", err)
	}
	return rh, mainnet, bad, nil
}

// isMissingRelation: the query failed because a table, column or schema does
// not exist (a database without the asset tables).
func isMissingRelation(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case "42P01", "42703", "3F000": // undefined_table, undefined_column, invalid_schema_name
		return true
	}
	return false
}

// reloadFeeds reads the database's Chainlink feeds and publishes them merged
// with the env ones. It never fails the caller: without the asset tables, or
// when the query fails, it logs once and keeps what is in use (the last good
// read, or the env feeds when no read has succeeded yet).
func (s *scanner) reloadFeeds(ctx context.Context) {
	if s.db == nil || s.onchain == nil || ctx.Err() != nil {
		return
	}
	o := s.onchain
	qctx, cancel := context.WithTimeout(ctx, feedsDBTimeout)
	defer cancel()
	rh, mn, bad, err := s.db.ChainlinkFeeds(qctx)
	d := &o.dbFeeds
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: not a problem of the database
		}
		prob := err.Error()
		keep := "the environment's feeds only"
		if d.loaded {
			keep = "the feeds read before"
		}
		if isMissingRelation(err) {
			prob = "no asset tables"
		}
		if prob != d.lastProb {
			d.lastProb = prob
			if isMissingRelation(err) {
				log.Printf("chainlink feeds: no asset tables (assets / chains / asset_chains) in this database — using %s", keep)
			} else {
				log.Printf("chainlink feeds: %v — using %s", err, keep)
			}
		}
		return
	}
	d.lastProb = ""
	changed := !d.loaded || !maps.Equal(rh, d.rh) || !maps.Equal(mn, d.mn)
	d.loaded, d.rh, d.mn = true, rh, mn
	if !changed {
		return
	}
	conflicts := o.setDBFeeds(rh, mn)
	fm := o.feedMaps()
	line := fmt.Sprintf("chainlink feeds: %d on Robinhood Chain and %d on Ethereum mainnet from the asset database; in use %d Robinhood + %d mainnet",
		len(rh), len(mn), len(fm.rh), len(fm.mainnet))
	if conflicts > 0 {
		line += fmt.Sprintf(" (%d overridden by SCOUT_*CHAINLINK_FEEDS)", conflicts)
	}
	if bad > 0 {
		line += fmt.Sprintf("; %d row(s) with a malformed address skipped", bad)
	}
	log.Print(line)
}
