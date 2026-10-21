package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// Venue-plan test addresses: the token, a v2/v3 pool, the PoolManager, the
// Pons factory, hook and curve.
var (
	hvToken, hvPool                  = hAddr(300), hAddr(301)
	hvPM, hvFactory, hvHook, hvCurve = hAddr(302), hAddr(303), hAddr(304), hAddr(305)
	hvPoolID                         = "0x" + strings.Repeat("ef", 32)
	hvCfg                            = holderVenueConfig{PoolManagerV4: hvPM, PonsFactory: hvFactory}
)

const hvEntry, hvPostEnd = 200, 1000

func venueStrings(vs []holderVenue) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Kind+":"+v.Address)
	}
	return out
}

// addrKeys returns a set's keys, sorted.
func addrKeys(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }

// sortedStrings returns s sorted.
func sortedStrings(s ...string) []string { return slices.Sorted(slices.Values(s)) }

// Venues, excluded sets, pool ids and LP applicability per kind of state,
// before and after the call. The PoolManager is excluded at every cut-off
// (configured in hvCfg), a venue or not.
func TestHoldersVenuePlan(t *testing.T) {
	base := []string{hvToken, zeroAddr, deadAddr, hvPM}
	ponsEx := append(slices.Clone(base), hvCurve, hvFactory, hvHook)
	cases := []struct {
		name             string
		st               onchainState
		ids, idsAll      []string // V4PoolIDsPre, V4PoolIDsAll as given
		unseen           bool     // poolSeenPre false: the state's pool never shows in the pre logs
		pre, post        []string // kind:address
		exPre, exPost    []string
		lpPre, lpPost    bool
		wantPre, wantAll []string // the plan's pool ids
		fetch            []string
	}{
		{name: "v2", st: onchainState{Kind: "v2", Pool: hvPool},
			pre: []string{"v2:" + hvPool}, post: []string{"v2:" + hvPool},
			exPre: append(slices.Clone(base), hvPool), exPost: append(slices.Clone(base), hvPool),
			lpPre: true, lpPost: true, fetch: []string{"v2:" + hvPool}},
		{name: "v3, upper-case pool", st: onchainState{Kind: "v3", Pool: "0x" + strings.ToUpper(hvPool[2:])},
			pre: []string{"v3:" + hvPool}, post: []string{"v3:" + hvPool},
			exPre: append(slices.Clone(base), hvPool), exPost: append(slices.Clone(base), hvPool),
			lpPre: true, lpPost: true, fetch: []string{"v3:" + hvPool}},
		// The tracker's discovery looks past the entry block: a v2/v3 pool the
		// pre logs never show (created after the call) is not a pre venue, so
		// not excluded and LP not applicable before the call. Post unchanged.
		{name: "v2, pool not seen before the call", st: onchainState{Kind: "v2", Pool: hvPool}, unseen: true,
			post: []string{"v2:" + hvPool}, exPre: base, exPost: append(slices.Clone(base), hvPool),
			lpPost: true, fetch: []string{"v2:" + hvPool}},
		{name: "v3, pool not seen before the call", st: onchainState{Kind: "v3", Pool: hvPool}, unseen: true,
			post: []string{"v3:" + hvPool}, exPre: base, exPost: append(slices.Clone(base), hvPool),
			lpPost: true, fetch: []string{"v3:" + hvPool}},
		// The tracker does not check that a plain v4 state's pool existed at
		// the entry block: its PoolID is a pre id only when the caller lists it.
		// Without a pre id no v4 pool of the token existed before the call, so
		// the PoolManager is not a pre venue (still excluded).
		{name: "v4 with its pool id, not listed as a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
			post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			lpPost: true, wantAll: []string{hvPoolID}, fetch: []string{"v4:" + hvPM}},
		{name: "v4 with its pool id, listed as a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
			ids: []string{hvPoolID}, pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			lpPre: true, lpPost: true, wantPre: []string{hvPoolID}, wantAll: []string{hvPoolID}, fetch: []string{"v4:" + hvPM}},
		{name: "v4 without pool ids", st: onchainState{Kind: "v4", Pool: hvPM},
			post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			fetch: []string{"v4:" + hvPM}},
		{name: "v4, a pool id learned after the call (all only)", st: onchainState{Kind: "v4", Pool: hvPM},
			idsAll: []string{lqPoolID}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			lpPost: true, wantAll: []string{lqPoolID}, fetch: []string{"v4:" + hvPM}},
		{name: "Pons on the curve", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook},
			pre: []string{"pons:" + hvCurve}, post: []string{"pons:" + hvCurve},
			exPre: ponsEx, exPost: ponsEx, fetch: []string{"pons:" + hvCurve}},
		{name: "Pons closed, pool not found yet", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook, PonsDone: 150},
			pre: []string{"pons:" + hvCurve}, post: []string{"pons:" + hvCurve},
			exPre: ponsEx, exPost: ponsEx, fetch: []string{"pons:" + hvCurve}},
		{name: "Pons graduated before the call", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID, Curve: hvCurve,
			Hook: hvHook, PonsDone: 140, PonsGrad: 150},
			pre: []string{"v4:" + hvPM, "pons:" + hvCurve}, post: []string{"v4:" + hvPM, "pons:" + hvCurve},
			exPre: ponsEx, exPost: ponsEx, lpPre: true, lpPost: true, wantPre: []string{hvPoolID}, wantAll: []string{hvPoolID},
			fetch: []string{"v4:" + hvPM, "pons:" + hvCurve}},
		{name: "Pons graduated in the entry block", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook,
			PoolID: hvPoolID, PonsDone: hvEntry, PonsGrad: hvEntry},
			pre: []string{"pons:" + hvCurve, "v4:" + hvPM}, post: []string{"pons:" + hvCurve, "v4:" + hvPM},
			exPre: ponsEx, exPost: ponsEx, lpPre: true, lpPost: true, wantPre: []string{hvPoolID}, wantAll: []string{hvPoolID},
			fetch: []string{"pons:" + hvCurve, "v4:" + hvPM}},
		{name: "Pons graduated after the call, inside the window", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve,
			Hook: hvHook, PoolID: hvPoolID, PonsDone: 400, PonsGrad: 500},
			pre: []string{"pons:" + hvCurve}, post: []string{"pons:" + hvCurve, "v4:" + hvPM},
			exPre: ponsEx, exPost: ponsEx,
			lpPost: true, wantAll: []string{hvPoolID}, fetch: []string{"pons:" + hvCurve, "v4:" + hvPM}},
		{name: "Pons graduated after the window", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve,
			Hook: hvHook, PoolID: hvPoolID, PonsDone: 1500, PonsGrad: 2000},
			pre: []string{"pons:" + hvCurve}, post: []string{"pons:" + hvCurve},
			exPre: ponsEx, exPost: ponsEx, wantAll: []string{hvPoolID}, fetch: []string{"pons:" + hvCurve, "v4:" + hvPM}},
		{name: "extra pool ids merged with the state's", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
			ids: []string{strings.ToUpper(lqPoolID), hvPoolID, ""}, idsAll: []string{lqOtherPoolID},
			pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base, lpPre: true, lpPost: true,
			wantPre: sortedStrings(hvPoolID, lqPoolID), wantAll: sortedStrings(hvPoolID, lqPoolID, lqOtherPoolID),
			fetch: []string{"v4:" + hvPM}},
		{name: "unknown kind", st: onchainState{Kind: "gecko", Pool: hvPool}, exPre: base, exPost: base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upperToken := "0x" + strings.ToUpper(hvToken[2:]) // excluded keys come out lowercase
			p := holderVenuePlanFor(tc.st, upperToken, hvEntry, hvPostEnd, tc.ids, tc.idsAll, !tc.unseen, hvCfg)
			if got := venueStrings(p.Pre); !slices.Equal(got, tc.pre) {
				t.Errorf("pre venues %v, want %v", got, tc.pre)
			}
			if got := venueStrings(p.Post); !slices.Equal(got, tc.post) {
				t.Errorf("post venues %v, want %v", got, tc.post)
			}
			if got, want := addrKeys(p.ExcludedPre), sortedStrings(tc.exPre...); !slices.Equal(got, want) {
				t.Errorf("excluded pre %v, want %v", got, want)
			}
			if got, want := addrKeys(p.ExcludedPost), sortedStrings(tc.exPost...); !slices.Equal(got, want) {
				t.Errorf("excluded post %v, want %v", got, want)
			}
			if p.LPApplicablePre != tc.lpPre || p.LPApplicablePost != tc.lpPost {
				t.Errorf("LP applicable pre %v post %v, want %v %v", p.LPApplicablePre, p.LPApplicablePost, tc.lpPre, tc.lpPost)
			}
			if !slices.Equal(p.V4PoolIDsPre, tc.wantPre) || !slices.Equal(p.V4PoolIDsAll, tc.wantAll) {
				t.Errorf("pool ids pre %v all %v, want %v %v", p.V4PoolIDsPre, p.V4PoolIDsAll, tc.wantPre, tc.wantAll)
			}
			if got := venueStrings(holderFetchVenues(tc.st, hvCfg)); !slices.Equal(got, tc.fetch) {
				t.Errorf("fetch venues %v, want %v", got, tc.fetch)
			}
		})
	}
}

// The pre venues, excluded set, pool ids and LP applicability never depend
// on the post window's end or on the ids learned later (V4PoolIDsAll).
func TestHoldersVenuePlanPreIgnoresPostEnd(t *testing.T) {
	states := map[string]onchainState{
		"Pons graduating after the call": {Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook, PoolID: hvPoolID,
			PonsDone: 400, PonsGrad: 500},
		"v4 without pool ids":                       {Kind: "v4", Pool: hvPM},
		"v4 with a PoolID the caller does not list": {Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
		"v2": {Kind: "v2", Pool: hvPool},
		"v3": {Kind: "v3", Pool: hvPool},
	}
	for name, st := range states {
		for _, seen := range []bool{true, false} {
			want := holderVenuePlanFor(st, hvToken, hvEntry, hvEntry, nil, nil, seen, hvCfg)
			for _, end := range []uint64{hvEntry, 499, 500, 10_000} {
				for _, later := range [][]string{nil, {lqPoolID}, {lqPoolID, lqOtherPoolID}} {
					p := holderVenuePlanFor(st, hvToken, hvEntry, end, nil, later, seen, hvCfg)
					if !slices.Equal(venueStrings(p.Pre), venueStrings(want.Pre)) || !maps.Equal(p.ExcludedPre, want.ExcludedPre) ||
						p.LPApplicablePre != want.LPApplicablePre || !slices.Equal(p.V4PoolIDsPre, want.V4PoolIDsPre) {
						t.Errorf("%s, pool seen pre %v, post end %d, later ids %v: pre plan %v %v %v %v, want %v %v %v %v",
							name, seen, end, later, venueStrings(p.Pre), addrKeys(p.ExcludedPre), p.LPApplicablePre, p.V4PoolIDsPre,
							venueStrings(want.Pre), addrKeys(want.ExcludedPre), want.LPApplicablePre, want.V4PoolIDsPre)
					}
				}
			}
		}
	}
}

// The pool-seen gate applies to v2/v3 states only: for v4 and Pons states
// the plan is the same whether or not the state's pool shows in the pre logs,
// and the post side never depends on it.
func TestHoldersVenuePlanPoolSeenGate(t *testing.T) {
	cases := []struct {
		name    string
		st      onchainState
		ids     []string
		gated   bool // pre changes when the pool is not seen
		unseenP []string
	}{
		{name: "v2", st: onchainState{Kind: "v2", Pool: hvPool}, gated: true},
		{name: "v3, upper-case pool", st: onchainState{Kind: "v3", Pool: "0x" + strings.ToUpper(hvPool[2:])}, gated: true},
		{name: "v4 with a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID}, ids: []string{hvPoolID},
			unseenP: []string{"v4:" + hvPM}},
		{name: "v4 without a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID}},
		{name: "Pons on the curve", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook},
			unseenP: []string{"pons:" + hvCurve}},
		{name: "Pons graduated before the call", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID, Curve: hvCurve,
			Hook: hvHook, PonsDone: 140, PonsGrad: 150}, unseenP: []string{"v4:" + hvPM, "pons:" + hvCurve}},
		{name: "Pons graduated after the call", st: onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook,
			PoolID: hvPoolID, PonsDone: 400, PonsGrad: 500}, unseenP: []string{"pons:" + hvCurve}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := holderVenuePlanFor(tc.st, hvToken, hvEntry, hvPostEnd, tc.ids, nil, true, hvCfg)
			unseen := holderVenuePlanFor(tc.st, hvToken, hvEntry, hvPostEnd, tc.ids, nil, false, hvCfg)
			if got := venueStrings(unseen.Pre); !slices.Equal(got, tc.unseenP) {
				t.Errorf("pre venues, pool not seen: got %v, want %v", got, tc.unseenP)
			}
			samePre := slices.Equal(venueStrings(seen.Pre), venueStrings(unseen.Pre)) &&
				maps.Equal(seen.ExcludedPre, unseen.ExcludedPre) && seen.LPApplicablePre == unseen.LPApplicablePre
			if samePre == tc.gated {
				t.Errorf("pre plan the same seen or not: got %v, want %v (pre %v / %v, LP %v / %v)", samePre, !tc.gated,
					venueStrings(seen.Pre), venueStrings(unseen.Pre), seen.LPApplicablePre, unseen.LPApplicablePre)
			}
			if tc.gated && (unseen.LPApplicablePre || unseen.ExcludedPre[hvPool]) {
				t.Errorf("pool not seen: LP applicable pre %v, pool excluded pre %v; want false, false",
					unseen.LPApplicablePre, unseen.ExcludedPre[hvPool])
			}
			if !slices.Equal(venueStrings(seen.Post), venueStrings(unseen.Post)) || !maps.Equal(seen.ExcludedPost, unseen.ExcludedPost) ||
				seen.LPApplicablePost != unseen.LPApplicablePost {
				t.Errorf("post plan depends on the pre gate: got %v %v, want %v %v", venueStrings(unseen.Post),
					unseen.LPApplicablePost, venueStrings(seen.Post), seen.LPApplicablePost)
			}
		})
	}
}

// holderPoolSeen: the pool shows as a pre transfer's From or To, or as a pre
// liquidity event's Venue (a Mint alone counts), in any case of letters.
func TestHolderPoolSeen(t *testing.T) {
	other := hAddr(310)
	upper := "0x" + strings.ToUpper(hvPool[2:])
	cases := []struct {
		name string
		pool string
		trs  []tokenTransfer
		liq  []liquidityEvent
		want bool
	}{
		{name: "no logs", pool: hvPool},
		{name: "empty pool", pool: "", trs: []tokenTransfer{xfer(zeroAddr, "", tokens(1), 10, 0)}},
		{name: "transfer to the pool", pool: hvPool, trs: []tokenTransfer{xfer(other, hvPool, tokens(1), 10, 0)}, want: true},
		{name: "transfer from the pool", pool: hvPool, trs: []tokenTransfer{xfer(hvPool, other, tokens(1), 10, 0)}, want: true},
		{name: "upper-case pool, lower-case log", pool: upper, trs: []tokenTransfer{xfer(hvPool, other, tokens(1), 10, 0)}, want: true},
		{name: "lower-case pool, upper-case log", pool: hvPool, trs: []tokenTransfer{xfer(upper, other, tokens(1), 10, 0)}, want: true},
		{name: "only other addresses", pool: hvPool, trs: []tokenTransfer{xfer(zeroAddr, other, tokens(1), 10, 0)},
			liq: []liquidityEvent{lqEv("v2", lpAdd, other, "", "0x01", 10)}},
		{name: "a Mint on the pool only", pool: hvPool, liq: []liquidityEvent{lqEv("v2", lpAdd, upper, "", "0x01", 10)}, want: true},
		{name: "a v3 Collect on the pool only", pool: hvPool, liq: []liquidityEvent{lqEv("v3", lpCollect, hvPool, "", "0x01", 10)}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := holderPoolSeen(tc.pool, tc.trs, tc.liq); got != tc.want {
				t.Errorf("holderPoolSeen(%q, %d transfers, %d events): got %v, want %v", tc.pool, len(tc.trs), len(tc.liq), got, tc.want)
			}
		})
	}
}

// The factory is excluded only for a Pons token and only when configured; a
// zero hook is not an address to exclude; the PoolManager is excluded
// whenever it is configured, at every cut-off, even where it is not a venue.
func TestHoldersExcludedSetFactoryAndHook(t *testing.T) {
	upperPM := holderVenueConfig{PoolManagerV4: "0x" + strings.ToUpper(hvPM[2:]), PonsFactory: hvFactory}
	cases := []struct {
		name string
		st   onchainState
		cfg  holderVenueConfig
		want []string
	}{
		{"v2 never excludes the factory", onchainState{Kind: "v2", Pool: hvPool}, hvCfg,
			[]string{hvToken, zeroAddr, deadAddr, hvPool, hvPM}},
		{"v2 without a PoolManager configured", onchainState{Kind: "v2", Pool: hvPool}, holderVenueConfig{PonsFactory: hvFactory},
			[]string{hvToken, zeroAddr, deadAddr, hvPool}},
		{"Pons with the factory off", onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook},
			holderVenueConfig{PoolManagerV4: hvPM}, []string{hvToken, zeroAddr, deadAddr, hvCurve, hvHook, hvPM}},
		{"Pons closed, pool not found (PonsGrad 0): PoolManager not a venue, still excluded",
			onchainState{Kind: "pons", Pool: hvCurve, Curve: hvCurve, Hook: hvHook, PonsDone: 150}, upperPM,
			[]string{hvToken, zeroAddr, deadAddr, hvCurve, hvHook, hvFactory, hvPM}},
		{"v4 with a zero hook", onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID, Hook: zeroAddr}, hvCfg,
			[]string{hvToken, zeroAddr, deadAddr, hvPM}},
		{"v4 with a known hook", onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID, Hook: strings.ToUpper(hvHook)}, hvCfg,
			[]string{hvToken, zeroAddr, deadAddr, hvPM, hvHook}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, cut := range []uint64{hvEntry, hvPostEnd} {
				venues := holderVenuesAt(tc.st, cut, tc.cfg)
				got := holderExcludedSet(tc.st, hvToken, venues, tc.cfg)
				if want := sortedStrings(tc.want...); !slices.Equal(addrKeys(got), want) {
					t.Errorf("cut-off %d: excluded %v, want %v", cut, addrKeys(got), want)
				}
				if tc.st.Kind == "pons" && slices.Contains(venueAddrs(venues), hvPM) {
					t.Errorf("cut-off %d: PoolManager is a venue %v; this case wants it excluded without being one", cut, venues)
				}
			}
		})
	}
}
