// The holder export's throttle: its settings (names in config.go), its own
// slow node client, and the pauses that keep it from slowing the tracker
// while both run on the prod server all day.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Defaults and limits of the holder export's settings.
const (
	holdersRPSDefault         = 4
	holdersRPSMax             = 20
	holdersMaxNodeLagDefault  = 2 * time.Minute
	holdersMaxPriceLagDefault = 2 * time.Hour
	holdersMaxPauseDefault    = 30 * time.Minute
)

// holderPauseStep: how long the export sleeps before it checks again.
const holderPauseStep = 30 * time.Second

// How long one lag answer is reused across waitClear calls, an error
// included. The node lag costs a node request per new head block, so it is
// asked at most once per pause step; the price lag is a database query.
const (
	holderNodeLagTTL  = holderPauseStep
	holderPriceLagTTL = time.Minute
)

// errHolderPausedTooLong: the export has paused for SCOUT_HOLDERS_MAX_PAUSE
// and the node or the latest-price pass is still behind.
var errHolderPausedTooLong = errors.New("holder export: paused longer than " + holdersMaxPauseSetting)

// holderExportConfig holds the SCOUT_HOLDERS_* settings.
type holderExportConfig struct {
	RPS         int           // requests per second of the export's own node client
	MaxNodeLag  time.Duration // pause while the head block is older than this (0 = off)
	MaxPriceLag time.Duration // pause while the latest-price pass is further behind (0 = off)
	MaxPause    time.Duration // give up after one pause this long
}

// loadHolderExportConfig reads the SCOUT_HOLDERS_* settings; an invalid
// value is an error (the export does not start).
func loadHolderExportConfig() (holderExportConfig, error) {
	c := holderExportConfig{RPS: holdersRPSDefault, MaxNodeLag: holdersMaxNodeLagDefault,
		MaxPriceLag: holdersMaxPriceLagDefault, MaxPause: holdersMaxPauseDefault}
	var err error
	if c.RPS, err = holdersEnvRPS(c.RPS); err != nil {
		return c, err
	}
	if c.MaxNodeLag, err = holdersEnvDur(holdersMaxNodeLagSetting, c.MaxNodeLag, true); err != nil {
		return c, err
	}
	if c.MaxPriceLag, err = holdersEnvDur(holdersMaxPriceLagSetting, c.MaxPriceLag, true); err != nil {
		return c, err
	}
	if c.MaxPause, err = holdersEnvDur(holdersMaxPauseSetting, c.MaxPause, false); err != nil {
		return c, err
	}
	return c, nil
}

// holdersEnvRPS reads SCOUT_HOLDERS_RPS: a number from 1 to holdersRPSMax.
func holdersEnvRPS(def int) (int, error) {
	v := env(holdersRPSSetting, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > holdersRPSMax {
		return 0, fmt.Errorf("%s=%q: want a number from 1 to %d", holdersRPSSetting, v, holdersRPSMax)
	}
	return n, nil
}

// holdersEnvDur reads the duration setting key: never negative, 0 only when
// zeroOK (the check is off).
func holdersEnvDur(key string, def time.Duration, zeroOK bool) (time.Duration, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err == nil && (d > 0 || d == 0 && zeroOK) {
		return d, nil
	}
	if zeroOK {
		return 0, fmt.Errorf("%s=%q: want a duration of at least 0, such as %s (0 = check off)", key, v, def)
	}
	return 0, fmt.Errorf("%s=%q: want a duration above 0, such as %s", key, v, def)
}

// holderOnchainConfig returns a copy of the tracker's on-chain settings for
// the export's own client: hc.RPS requests per second, one request in flight,
// one log range at a time, no log cache and no mainnet node. newRPCClient
// enforces the rate (1s/RPS) and the in-flight cap. The maps are shared with
// base, which only reads them.
func holderOnchainConfig(base onchainConfig, hc holderExportConfig) onchainConfig {
	oc := base
	oc.RPS = hc.RPS
	oc.MaxInflight = 1
	oc.Parallel = 1
	oc.LogCache = 0
	oc.MainnetRPCURL = ""
	oc.MainnetRPS = 0
	return oc
}

// nodeLag is how far the node is behind: now minus the time of its head block
// (the head is cached for headCacheTTL). Negative when the clocks disagree.
func nodeLag(ctx context.Context, c *rpcClient, now time.Time) (time.Duration, error) {
	n, err := c.blockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("head block: %w", err)
	}
	ts, err := c.blockTime(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("time of head block %d: %w", n, err)
	}
	return now.Sub(time.Unix(ts, 0)), nil
}

// holderLagFunc measures how far something is behind (the node, the
// latest-price pass).
type holderLagFunc func(context.Context) (time.Duration, error)

// holderPauser decides, before each of the export's node calls, whether to
// wait for the node and the tracker. One caller at a time.
type holderPauser struct {
	cfg   holderExportConfig
	node  holderLagFunc
	price holderLagFunc
	clock func() time.Time
	sleep func(context.Context, time.Duration) error
	log   io.Writer

	// The last node-lag and price-lag answers.
	nodeLast, priceLast holderLagCache
}

// holderLagCache is the last answer of one lag check and when it was asked.
type holderLagCache struct {
	at  time.Time
	lag time.Duration
	err error
}

// get returns the last answer while it is younger than ttl at now (an error
// included); otherwise it asks check and keeps the answer, unless ctx ended
// meanwhile. The answer is stale at exactly ttl, so a check whose ttl is
// holderPauseStep is asked again after each pause step.
func (c *holderLagCache) get(ctx context.Context, now time.Time, ttl time.Duration, check holderLagFunc) (time.Duration, error) {
	if !c.at.IsZero() && now.Sub(c.at) < ttl {
		return c.lag, c.err
	}
	lag, err := check(ctx)
	if ctx.Err() == nil {
		*c = holderLagCache{at: now, lag: lag, err: err}
	}
	return lag, err
}

// newHolderPauser returns a pauser; nil clock, sleep and log mean time.Now,
// sleepCtx and no log.
func newHolderPauser(hc holderExportConfig, node, price holderLagFunc, clock func() time.Time,
	sleep func(context.Context, time.Duration) error, log io.Writer) *holderPauser {
	if clock == nil {
		clock = time.Now
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	if log == nil {
		log = io.Discard
	}
	return &holderPauser{cfg: hc, node: node, price: price, clock: clock, sleep: sleep, log: log}
}

// waitClear returns once the node and the latest-price pass are within their
// limits, with the time it paused. While either is behind (or its check
// fails) it logs one line and sleeps holderPauseStep, then checks again.
// Instead of sleeping past MaxPause it returns an error that wraps
// errHolderPausedTooLong and names the reason (not logged here: the caller
// reports it); it returns ctx.Err() when ctx ends.
func (p *holderPauser) waitClear(ctx context.Context) (paused time.Duration, err error) {
	start := p.clock()
	for {
		if ctx.Err() != nil {
			return p.clock().Sub(start), ctx.Err()
		}
		reason := p.blocked(ctx)
		paused = p.clock().Sub(start)
		if ctx.Err() != nil {
			return paused, ctx.Err()
		}
		if reason == "" {
			return paused, nil
		}
		if paused+holderPauseStep > p.cfg.MaxPause {
			return paused, fmt.Errorf("%w (%s): %s after %s", errHolderPausedTooLong, p.cfg.MaxPause, reason, paused)
		}
		fmt.Fprintf(p.log, "holder export: %s; pausing %s (paused %s so far)\n", reason, holderPauseStep, paused)
		if err = p.sleep(ctx, holderPauseStep); err != nil {
			return p.clock().Sub(start), err
		}
	}
}

// blocked returns why the export must wait ("" = it need not). A check
// whose limit is 0 is off and not asked; the others reuse their last answer
// for holderNodeLagTTL and holderPriceLagTTL.
func (p *holderPauser) blocked(ctx context.Context) string {
	if p.cfg.MaxNodeLag > 0 {
		lag, err := p.nodeLast.get(ctx, p.clock(), holderNodeLagTTL, p.node)
		if r := holderLagReason("node head block", holdersMaxNodeLagSetting, lag, p.cfg.MaxNodeLag, err); r != "" {
			return r
		}
	}
	if p.cfg.MaxPriceLag > 0 {
		lag, err := p.priceLast.get(ctx, p.clock(), holderPriceLagTTL, p.price)
		if r := holderLagReason("latest-price pass", holdersMaxPriceLagSetting, lag, p.cfg.MaxPriceLag, err); r != "" {
			return r
		}
	}
	return ""
}

// holderLagReason describes a check that holds the export back ("" = within
// limit). key names the setting with the limit.
func holderLagReason(what, key string, lag, limit time.Duration, err error) string {
	if err != nil {
		return fmt.Sprintf("%s check failed: %v", what, err)
	}
	if lag > limit {
		return fmt.Sprintf("%s %s behind (%s is %s)", what, lag.Round(time.Second), key, limit)
	}
	return ""
}
