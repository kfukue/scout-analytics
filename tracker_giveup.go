package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Giving up on a call that keeps failing (SCOUT_TRACK_MAX_ATTEMPTS). A run of
// the horizon tracker that ends in status error sets the call to gave_up
// instead when all of these hold:
//   - the row's attempts (this run included) reached the setting;
//   - the call is past its give-up deadline (entry + longest horizon + 48 h,
//     the deadline of the no-pool / no-trades rule);
//   - the error is not errUSDSearchPending (that search moves on every try);
//   - the run made no progress (onchainProgressed, geckoProgressed): nothing
//     it stored brings the call closer to its results.
// gave_up rows are never picked up by DueTracking again. 0 turns the rule off.
// ---------------------------------------------------------------------------

// defaultTrackMaxAttempts is SCOUT_TRACK_MAX_ATTEMPTS when it is not set.
const defaultTrackMaxAttempts = 10

// loadTrackMaxAttempts reads SCOUT_TRACK_MAX_ATTEMPTS: unset = the default,
// 0 = off, otherwise a whole number of attempts; anything else is refused.
func loadTrackMaxAttempts() (int, error) {
	v := env(trackMaxAttemptsSetting, "")
	if v == "" {
		return defaultTrackMaxAttempts, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: want a whole number of attempts, 0 or more (0 = never give up on errors)", trackMaxAttemptsSetting, v)
	}
	return n, nil
}

// giveUpOnError says whether a run that failed with err (the row's status
// being error) gives up on the call: maxAttempts is the setting (0 = never),
// attempts the row's attempts with this run, deadline the call's give-up
// deadline and progressed whether the run stored any progress.
func giveUpOnError(maxAttempts, attempts int, now, deadline time.Time, err error, progressed bool) bool {
	return maxAttempts > 0 && attempts >= maxAttempts && now.After(deadline) &&
		!errors.Is(err, errUSDSearchPending) && !progressed
}

// gaveUpErrorText is the error stored on a call the rule gave up on.
func gaveUpErrorText(attempts int, deadline time.Time, err error) string {
	return fmt.Sprintf("gave up after %d attempts, past the deadline %s: %v",
		attempts, deadline.UTC().Format("2006-01-02 15:04 UTC"), err)
}

// giveUpStuck applies the rule to a run that failed with err: when t is in
// error and giveUpOnError holds, t becomes gave_up with gaveUpErrorText as
// its error, and it returns true.
func giveUpStuck(t *ScoutCallTracking, maxAttempts int, now, deadline time.Time, err error, progressed bool) bool {
	if t.Status != TrackError || !giveUpOnError(maxAttempts, t.Attempts, now, deadline, err, progressed) {
		return false
	}
	msg := gaveUpErrorText(t.Attempts, deadline, err)
	t.Status, t.Error = TrackGaveUp, &msg
	return true
}

// maxAttemptsText describes the rule for the tracker's start-up line.
func maxAttemptsText(maxAttempts int) string {
	if maxAttempts <= 0 {
		return fmt.Sprintf("calls in error are never given up (%s=0)", trackMaxAttemptsSetting)
	}
	return fmt.Sprintf("a call in error is given up after %d attempts past its deadline in a run without progress (%s)",
		maxAttempts, trackMaxAttemptsSetting)
}

// onchainProgressed says whether an on-chain run stored progress, comparing
// the on-chain state at the run's start (start; nil when there was none, or
// an older version) with the state it leaves (end; nil when none is kept).
// Progress is any of: a pool found (a new state), the entry price in quote
// units found, the entry price in the price unit set (entrySet), the pre-call
// trades stored, the scan cursor moved on (a piece of a horizon segment and
// its candles stored), one more horizon stored, or a rug found.
func onchainProgressed(start, end *onchainState, entrySet bool) bool {
	switch {
	case entrySet:
		return true
	case end == nil:
		return false
	case start == nil:
		return true
	}
	return (start.EntryPriceQ == 0 && end.EntryPriceQ > 0) ||
		(!start.PreDone && end.PreDone) ||
		end.ScanBlock > start.ScanBlock ||
		doneHorizons(end.Done) > doneHorizons(start.Done) ||
		(start.RugBlock == 0 && end.RugBlock > 0)
}

// doneHorizons counts the horizons marked done in a state.
func doneHorizons(done map[string]bool) int {
	n := 0
	for _, ok := range done {
		if ok {
			n++
		}
	}
	return n
}

// geckoProgressed says whether a GeckoTerminal run stored progress: a pool
// found, the entry price set, or at least one horizon stored (stored).
func geckoProgressed(start, end *ScoutCallTracking, stored int) bool {
	return (start.PoolAddress == nil && end.PoolAddress != nil) ||
		(start.EntryPriceUSD == nil && end.EntryPriceUSD != nil) ||
		stored > 0
}
