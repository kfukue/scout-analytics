package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// -retry-no-pool: put first calls whose pool was not found (no_pool, and with
// -retry-gave-up also gave_up) back in the tracker's queue, due now. Meant for
// after pool discovery learned something new (e.g. a new DEX), so those calls
// are tried at once instead of after up to 6 h, or never again (gave_up).
//
// Only a token's first real call is touched (the tracker's and the website's
// rule, webFirstCallsSQL): repeat rows, update posts, later calls and rows in
// any other status (done, tracking, pending, error, repeat) are left alone.
// Priorities are kept, so live calls still go first.
//
// gave_up: the tracker sets it only after a real attempt that failed with "no
// pool" or "no trades around the call time", once the call is past its last
// horizon + 48 h. A "no trades" row keeps the on-chain state of the pool it
// picked; tried again as it is, the same pool would be read again and the row
// marked gave_up again. So a gave_up row without an entry price also loses its
// on-chain state and pool columns: the next check starts with a fresh pool
// discovery. The deadline is left as it is: it is only checked after an
// attempt has failed, so every reset row gets one real attempt (discovery and
// entry price with the current code); if that still finds no pool or no
// trades, the row is gave_up again, which is right for an old call.

// RetryFilter selects the tracking rows -retry-no-pool resets.
type RetryFilter struct {
	GaveUp     bool     // also gave_up rows (otherwise only no_pool)
	Launchpads []string // match scout_call_metrics.launchpad or dex (see normLaunchpad); empty = all
}

// statuses returns the tracking statuses the filter selects.
func (f RetryFilter) statuses() []string {
	if f.GaveUp {
		return []string{TrackNoPool, TrackGaveUp}
	}
	return []string{TrackNoPool}
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

// normLaunchpad is how launchpad / dex values are compared: lower case, with
// everything but a-z and 0-9 left out, so "pons_v2", "Pons V2" and "PONS-V2"
// are the same value. Must match the SQL in retryWhereSQL.
func normLaunchpad(s string) string {
	return nonAlnumRe.ReplaceAllString(strings.ToLower(s), "")
}

// parseLaunchpads turns "-retry-launchpad pons_v2,Longxyz" into normalised values.
func parseLaunchpads(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if n := normLaunchpad(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// retryFromSQL / retryWhereSQL: the rows -retry-no-pool resets. $1 = statuses,
// $2 = normalised launchpad / dex values (empty array = no filter). The same
// text is used for the summary and the update.
var retryFromSQL = ` FROM scout_call_tracking t
	JOIN scout_calls c ON c.id = t.call_id
	JOIN ` + webFirstCallsSQL + ` fc ON fc.id = t.call_id
	LEFT JOIN scout_call_metrics m ON m.call_id = t.call_id`

const retryWhereSQL = ` t.status = ANY($1::text[])
	  AND c.post_kind IS DISTINCT FROM 'update'
	  AND (cardinality($2::text[]) = 0
	       OR regexp_replace(lower(COALESCE(m.launchpad, '')), '[^a-z0-9]+', '', 'g') = ANY($2::text[])
	       OR regexp_replace(lower(COALESCE(m.dex, '')), '[^a-z0-9]+', '', 'g') = ANY($2::text[]))`

// RetryGroup is one line of the summary: rows of one status, launchpad and dex.
type RetryGroup struct {
	Status, Launchpad, Dex string // launchpad / dex as stored ("" = none)
	N                      int
}

// RetrySummary is what -retry-no-pool would reset.
type RetrySummary struct {
	Total    int
	ByStatus map[string]int
	Groups   []RetryGroup // by launchpad, dex, status
}

// RetryNoPool resets the rows f selects, in one transaction: it reads the
// summary, hands it to show (before anything changes), and, unless dryRun,
// sets them to pending, due now, attempts 0, no error (gave_up rows without an
// entry price also lose their on-chain state and pool; see the top of this
// file). Returns the rows updated (0 for a dry run).
func (st *ScoutStore) RetryNoPool(ctx context.Context, f RetryFilter, dryRun bool, show func(RetrySummary)) (int, error) {
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	lps := f.Launchpads
	if lps == nil {
		lps = []string{}
	}
	rows, err := tx.Query(ctx, `SELECT t.status, COALESCE(m.launchpad, ''), COALESCE(m.dex, ''), count(*)`+
		retryFromSQL+` WHERE `+retryWhereSQL+` GROUP BY 1, 2, 3`, f.statuses(), lps)
	if err != nil {
		return 0, err
	}
	sum := RetrySummary{ByStatus: map[string]int{}}
	for rows.Next() {
		var g RetryGroup
		if err := rows.Scan(&g.Status, &g.Launchpad, &g.Dex, &g.N); err != nil {
			rows.Close()
			return 0, err
		}
		sum.Groups = append(sum.Groups, g)
		sum.ByStatus[g.Status] += g.N
		sum.Total += g.N
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sort.Slice(sum.Groups, func(i, j int) bool {
		a, b := sum.Groups[i], sum.Groups[j]
		if a.Launchpad != b.Launchpad {
			return a.Launchpad < b.Launchpad
		}
		if a.Dex != b.Dex {
			return a.Dex < b.Dex
		}
		return a.Status > b.Status // no_pool before gave_up
	})
	if show != nil {
		show(sum)
	}
	if dryRun || sum.Total == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE scout_call_tracking u SET
		    status = 'pending', next_check_at = now(), attempts = 0, error = NULL, updated_at = now(),
		    onchain         = CASE WHEN u.status = 'gave_up' AND u.entry_price_usd IS NULL THEN NULL ELSE u.onchain END,
		    pool_address    = CASE WHEN u.status = 'gave_up' AND u.entry_price_usd IS NULL THEN NULL ELSE u.pool_address END,
		    pool_name       = CASE WHEN u.status = 'gave_up' AND u.entry_price_usd IS NULL THEN NULL ELSE u.pool_name END,
		    pool_dex        = CASE WHEN u.status = 'gave_up' AND u.entry_price_usd IS NULL THEN NULL ELSE u.pool_dex END,
		    pool_created_at = CASE WHEN u.status = 'gave_up' AND u.entry_price_usd IS NULL THEN NULL ELSE u.pool_created_at END
		WHERE u.call_id IN (SELECT t.call_id`+retryFromSQL+` WHERE `+retryWhereSQL+`)`, f.statuses(), lps)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// printRetrySummary writes the summary -retry-no-pool shows before it changes anything.
func printRetrySummary(w io.Writer, f RetryFilter, sum RetrySummary) {
	what := TrackNoPool
	if f.GaveUp {
		what = TrackNoPool + " + " + TrackGaveUp
	}
	filter := "any launchpad / dex"
	if len(f.Launchpads) > 0 {
		filter = "launchpad or dex = " + strings.Join(f.Launchpads, ", ")
	}
	fmt.Fprintf(w, "retry: first calls with status %s, %s\n", what, filter)
	if sum.Total == 0 {
		fmt.Fprintln(w, "retry: nothing to reset")
		return
	}
	var parts []string
	for _, s := range f.statuses() {
		parts = append(parts, fmt.Sprintf("%s %s", s, commas(sum.ByStatus[s])))
	}
	fmt.Fprintf(w, "retry: %s call(s) to reset to pending — %s\n", commas(sum.Total), strings.Join(parts, ", "))
	fmt.Fprintf(w, "  %-20s %-20s %-8s %8s\n", "launchpad", "dex", "status", "calls")
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, g := range sum.Groups {
		fmt.Fprintf(w, "  %-20s %-20s %-8s %8s\n", dash(g.Launchpad), dash(g.Dex), g.Status, commas(g.N))
	}
}

// runRetryNoPool is the -retry-no-pool command: summary, then the reset (or
// nothing with dryRun), then the count.
func runRetryNoPool(ctx context.Context, st *ScoutStore, w io.Writer, f RetryFilter, dryRun bool) error {
	n, err := st.RetryNoPool(ctx, f, dryRun, func(sum RetrySummary) { printRetrySummary(w, f, sum) })
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(w, "retry: dry run — nothing changed")
		return nil
	}
	fmt.Fprintf(w, "retry: %s row(s) updated; a running tracker (-track or the listener) picks them up on its next cycle, or run -track-once\n", commas(n))
	return nil
}
