package main

// Venues and excluded addresses for the holder analysis of one call, from the
// tracker's stored state (onchainState). Pure: no node, no database.
//
// A venue is where the token trades (holders_early.go: a buy is a transfer out
// of a venue, a sell one into it). The excluded set is every address that is
// not a holder; one set per cut-off is passed to holderStats and
// earlyTradeStats alike, so the two agree on circulating supply.
//
// What the state says, and the choices made here where it is ambiguous:
//   - Kind "v2" / "v3": Pool is the pair or pool.
//   - Kind "v4": Pool is the PoolManager (shared by every v4 pool), PoolID the
//     token's pool.
//   - Kind "pons": Pool and Curve are the token's bonding curve. PonsGrad is
//     the PoolGraduated block (the v4 pool's creation) once the tracker found
//     it; 0 = no pool yet. A Pons token that graduated before the call is a
//     Kind "v4" state that keeps Curve and PonsGrad, so its curve is still a
//     venue (the early trades were there).
//   - A closed curve (PonsDone set) whose pool the tracker has not found yet
//     (PonsGrad 0) is treated as not graduated: the state does not say when,
//     so the PoolManager is not a venue at any cut-off.
//   - The PoolManager is never a holder: it is in both excluded sets whenever
//     it is configured, whether or not it is a venue at that cut-off.
//   - The PoolManager and the Pons factory are not in the state: they come
//     from the configuration (holderVenueConfig).
//   - v4 pool ids are split by cut-off: the pre set holds only ids of pools
//     initialised at or before the entry block, so a pool learned later can
//     never make a pre feature (for example LP applicable) change. The
//     state's PoolID is a pre id only for a Pons pool created (PonsGrad) at
//     or before the entry block: for a plain v4 state the tracker's discovery
//     (resolveV4) does not check that the pool existed at the entry block, so
//     its id is a pre id only when the caller lists it (holderV4PoolIDs,
//     holders_fetch.go, splits the ids by Initialize block).
//   - Pre venues hold nothing created after the entry block: a v2/v3 pool
//     (the state's Pool) is a pre venue only when the pre logs show it (a
//     pre transfer from or to it, or a pre liquidity event on it:
//     holderPoolSeen), because the tracker's discovery also looks past the
//     entry block; the PoolManager is a pre venue only with at least one pre
//     pool id (no v4 pool of the token existed before the call otherwise). It
//     stays excluded either way. Post venues are not gated.
//   - The only hook the state knows is Hook (set for Pons tokens: the Pons
//     hook); other v4 pools' hooks are not stored, so they are not excluded.

import (
	"maps"
	"slices"
	"strings"
)

// holderVenueConfig: the addresses the state does not hold, from
// onchainConfig.
type holderVenueConfig struct {
	PoolManagerV4 string // onchainConfig.PoolManagerV4
	PonsFactory   string // onchainConfig.PonsFactory; "" = off
}

// holderVenuePlan: the venues and excluded sets of one call, before the call
// (cut-off: the entry block) and at the end of the post window.
type holderVenuePlan struct {
	Pre, Post                 []holderVenue   // lowercase, deduplicated, in a fixed order
	ExcludedPre, ExcludedPost map[string]bool // lowercase keys; venues included
	// V4PoolIDsPre: the ids of pools initialised at or before the entry block
	// (holderPreIDs), plus the state's PoolID when it is a Pons pool created
	// at or before the entry block. The only ids any pre computation sees.
	// V4PoolIDsAll: every known id (pre ids included) plus the state's PoolID,
	// for the post window and the reader.
	// Both lowercase, sorted and deduplicated.
	V4PoolIDsPre, V4PoolIDsAll []string
	// LPApplicablePre / LPApplicablePost: at least one venue at that cut-off
	// has liquidity events that can be read (a v2 or v3 pool, or the
	// PoolManager with pool ids: the pre ids for pre, all ids for post). False
	// for a Pons token still on its curve at the cut-off, and for a v4 venue
	// without pool ids.
	LPApplicablePre, LPApplicablePost bool
}

// holderV4VenueAt reports whether the PoolManager is a venue at cutoff: the
// state is v4, or the Pons pool was created (PonsGrad) at or before cutoff.
func holderV4VenueAt(st onchainState, cutoff uint64) bool {
	return st.Kind == "v4" || holderPonsPoolAt(st, cutoff)
}

// holderPonsPoolAt reports whether the token's Pons pool was created
// (PonsGrad, a known block) at or before cutoff. holderV4VenueAt and
// holderPreIDs share it, so a Pons pool's venue and its pre id never drift.
func holderPonsPoolAt(st onchainState, cutoff uint64) bool {
	return st.PonsGrad != 0 && st.PonsGrad <= cutoff
}

// holderStateKnown reports whether st's Kind is one this file understands.
func holderStateKnown(st onchainState) bool {
	switch st.Kind {
	case "v2", "v3", "v4", "pons":
		return true
	}
	return false
}

// holderVenuesAt returns the venues the token traded on at or before cutoff:
// the state's pool, the Pons curve if there is one, and the PoolManager when
// the state is v4 or the Pons pool was created (PonsGrad) at or before cutoff.
// An unknown Kind gives none.
func holderVenuesAt(st onchainState, cutoff uint64, cfg holderVenueConfig) []holderVenue {
	if !holderStateKnown(st) {
		return nil
	}
	var out []holderVenue
	add := func(addr, kind string) {
		addr = strings.ToLower(addr)
		v := holderVenue{Address: addr, Kind: kind}
		if addr != "" && addr != zeroAddr && !slices.ContainsFunc(out, func(o holderVenue) bool { return o.Address == addr }) {
			out = append(out, v)
		}
	}
	add(st.Pool, st.Kind)
	if st.Curve != "" {
		add(st.Curve, "pons")
	}
	if holderV4VenueAt(st, cutoff) {
		add(cfg.PoolManagerV4, "v4")
	}
	return out
}

// holderPreVenues returns the venues at the entry block (holderVenuesAt),
// without the ones the pre data cannot show existed: the state's v2/v3 pool
// when poolSeenPre is false (holderPoolSeen), and the PoolManager when there
// is no pre pool id (preIDs, holderPreIDs). The excluded set still holds the
// PoolManager (holderExcludedSet).
func holderPreVenues(st onchainState, entryBlock uint64, poolSeenPre bool, preIDs []string, cfg holderVenueConfig) []holderVenue {
	drop := map[string]bool{}
	if (st.Kind == "v2" || st.Kind == "v3") && !poolSeenPre {
		drop[strings.ToLower(st.Pool)] = true
	}
	if holderV4VenueAt(st, entryBlock) && len(preIDs) == 0 {
		drop[strings.ToLower(cfg.PoolManagerV4)] = true
	}
	return slices.DeleteFunc(holderVenuesAt(st, entryBlock, cfg), func(v holderVenue) bool { return drop[v.Address] })
}

// holderPoolSeen reports whether pool shows in the pre-call logs: the From or
// To of a transfer, or the Venue of a liquidity event (a Mint counts).
// Callers pass only logs at or before the entry block (holderLogs'
// TransfersPre and LiquidityPre), so a pool created after the call is never
// seen. Compared in lowercase; an empty pool is never seen.
func holderPoolSeen(pool string, transfers []tokenTransfer, liq []liquidityEvent) bool {
	pool = strings.ToLower(pool)
	if pool == "" {
		return false
	}
	for _, t := range transfers {
		if strings.ToLower(t.From) == pool || strings.ToLower(t.To) == pool {
			return true
		}
	}
	for _, e := range liq {
		if strings.ToLower(e.Venue) == pool {
			return true
		}
	}
	return false
}

// holderFetchVenues returns the venues to read for a call
// (holderFetchInput.Venues), before the post window's end is known: every
// venue up to any block. A pool created after the window's end has no events
// in it, so reading it costs requests but changes no result.
func holderFetchVenues(st onchainState, cfg holderVenueConfig) []holderVenue {
	return holderVenuesAt(st, ^uint64(0), cfg)
}

// holderExcludedSet returns the addresses that are not holders: the venues,
// the token, the zero and dead addresses, the PoolManager whenever it is
// configured (a venue at this cut-off or not: it is never a holder), the Pons
// factory (for a Pons token) and the state's hook when it is set. Keys are
// lowercase.
func holderExcludedSet(st onchainState, token string, venues []holderVenue, cfg holderVenueConfig) map[string]bool {
	ex := map[string]bool{strings.ToLower(token): true, zeroAddr: true, deadAddr: true}
	for _, v := range venues {
		ex[strings.ToLower(v.Address)] = true
	}
	if cfg.PoolManagerV4 != "" {
		ex[strings.ToLower(cfg.PoolManagerV4)] = true
	}
	if (st.Kind == "pons" || st.Curve != "") && cfg.PonsFactory != "" {
		ex[strings.ToLower(cfg.PonsFactory)] = true
	}
	if h := strings.ToLower(st.Hook); h != "" && h != zeroAddr {
		ex[h] = true
	}
	delete(ex, "")
	return ex
}

// holderPoolIDs merges ids and the state's PoolID: lowercase, sorted,
// deduplicated, empty ones dropped.
func holderPoolIDs(st onchainState, ids []string) []string {
	return normPoolIDs(append(slices.Clone(ids), st.PoolID))
}

// holderAllPoolIDs: every known id (pre and later ones) plus the state's
// PoolID, for the post window and the reader. pre is merged in, so the pre
// ids are always a subset even when a caller fills only pre.
func holderAllPoolIDs(st onchainState, pre, all []string) []string {
	return holderPoolIDs(st, append(slices.Clone(pre), all...))
}

// holderPreIDs: the ids a pre computation may use. pre must hold only ids of
// pools initialised at or before the entry block (the caller's contract:
// holderRowInput.V4PoolIDsPre). The state's PoolID is added only for a Pons
// pool created at a known block (PonsGrad) at or before the entry block
// (holderPonsPoolAt); a Pons pool created after the entry block is left out.
// For a plain v4 state it is never added: the tracker's discovery (resolveV4)
// picks the top-ranked pool initialised up to after the entry block, without
// checking that it existed at the entry block, so its id is a pre id only
// when the caller lists it (holderV4PoolIDs checks the Initialize block).
// Without pre ids the PoolManager is not a pre venue (holderPreVenues) and has
// no liquidity filters, so LP is not applicable before the call.
func holderPreIDs(st onchainState, entryBlock uint64, pre []string) []string {
	if holderPonsPoolAt(st, entryBlock) {
		return holderPoolIDs(st, pre)
	}
	return normPoolIDs(pre)
}

// normPoolIDs: lowercase, sorted, deduplicated, empty ones dropped.
func normPoolIDs(ids []string) []string {
	set := map[string]bool{}
	for _, id := range ids {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			set[id] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// holderLPApplicable reports whether any venue has liquidity filters to read
// (liquidityFilters: none for a Pons curve, none for the PoolManager without
// pool ids).
func holderLPApplicable(venues []holderVenue, ids []string) bool {
	for _, v := range venues {
		if len(liquidityFilters(v, ids)) > 0 {
			return true
		}
	}
	return false
}

// holderVenuePlanFor builds the plan of one call from its stored state: the
// venues and excluded set at entryBlock (pre) and at postEndBlock (post), and
// whether liquidity applies at each. idsPre are the token's v4 pool ids
// initialised at or before entryBlock (holderPreIDs); idsAll every other known
// id. poolSeenPre: the state's v2/v3 pool shows in the pre-call logs
// (holderPoolSeen); without it the pool is not a pre venue (holderPreVenues).
// Pre never depends on postEndBlock or idsAll.
func holderVenuePlanFor(st onchainState, token string, entryBlock, postEndBlock uint64, idsPre, idsAll []string, poolSeenPre bool, cfg holderVenueConfig) holderVenuePlan {
	p := holderVenuePlan{V4PoolIDsPre: holderPreIDs(st, entryBlock, idsPre), V4PoolIDsAll: holderAllPoolIDs(st, idsPre, idsAll)}
	p.Pre = holderPreVenues(st, entryBlock, poolSeenPre, p.V4PoolIDsPre, cfg)
	p.Post = holderVenuesAt(st, postEndBlock, cfg)
	p.ExcludedPre = holderExcludedSet(st, token, p.Pre, cfg)
	p.ExcludedPost = holderExcludedSet(st, token, p.Post, cfg)
	p.LPApplicablePre = holderLPApplicable(p.Pre, p.V4PoolIDsPre)
	p.LPApplicablePost = holderLPApplicable(p.Post, p.V4PoolIDsAll)
	return p
}

// venueAddrSet returns the venues' addresses as a lowercase set.
func venueAddrSet(venues []holderVenue) map[string]bool {
	out := make(map[string]bool, len(venues))
	for _, v := range venues {
		out[strings.ToLower(v.Address)] = true
	}
	return out
}

// venueAddrs returns the venues' addresses, in order.
func venueAddrs(venues []holderVenue) []string {
	out := make([]string, 0, len(venues))
	for _, v := range venues {
		out = append(out, v.Address)
	}
	return out
}

// idSet returns ids as a set.
func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
