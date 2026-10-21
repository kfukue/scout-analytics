// This file holds the names of the settings (environment variables) the
// program reads. Names still written as literals elsewhere are being moved
// here over time.

package main

// databaseURLSetting names SCOUT_DATABASE_URL in errors, which never quote
// its value (see database.ConnStringError).
const databaseURLSetting = "SCOUT_DATABASE_URL"

// The holder export's settings (holders_throttle.go): its own node client's
// rate, and when it pauses so it never slows the tracker.
const (
	// holdersRPSSetting: requests per second of the export's node client.
	holdersRPSSetting = "SCOUT_HOLDERS_RPS"
	// holdersMaxNodeLagSetting: pause while the node's head block is older
	// than this (0 = check off).
	holdersMaxNodeLagSetting = "SCOUT_HOLDERS_MAX_NODE_LAG"
	// holdersMaxPriceLagSetting: pause while the tracker's latest-price pass
	// is further behind than this (0 = check off).
	holdersMaxPriceLagSetting = "SCOUT_HOLDERS_MAX_PRICE_LAG"
	// holdersMaxPauseSetting: stop after one pause longer than this.
	holdersMaxPauseSetting = "SCOUT_HOLDERS_MAX_PAUSE"
)

// Tracker settings the holder export records in its run record (runs.jsonl,
// holders_cmd.go). They are still read through literals in onchain.go and
// tracker_latest.go; those move here in their own PR.
const (
	// rpcLogChunkSetting: most blocks per eth_getLogs.
	rpcLogChunkSetting = "SCOUT_RPC_LOG_CHUNK"
	// latestRefreshRecentSetting / latestRefreshOldSetting: how often the
	// latest-price pass reads a call younger / older than 30 days again.
	latestRefreshRecentSetting = "SCOUT_LATEST_REFRESH_RECENT"
	latestRefreshOldSetting    = "SCOUT_LATEST_REFRESH_OLD"
)
