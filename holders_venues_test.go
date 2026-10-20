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
		// The tracker does not check that a plain v4 state's pool existed at
		// the entry block: its PoolID is a pre id only when the caller lists it.
		{name: "v4 with its pool id, not listed as a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
			pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			lpPost: true, wantAll: []string{hvPoolID}, fetch: []string{"v4:" + hvPM}},
		{name: "v4 with its pool id, listed as a pre id", st: onchainState{Kind: "v4", Pool: hvPM, PoolID: hvPoolID},
			ids: []string{hvPoolID}, pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			lpPre: true, lpPost: true, wantPre: []string{hvPoolID}, wantAll: []string{hvPoolID}, fetch: []string{"v4:" + hvPM}},
		{name: "v4 without pool ids", st: onchainState{Kind: "v4", Pool: hvPM},
			pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
			fetch: []string{"v4:" + hvPM}},
		{name: "v4, a pool id learned after the call (all only)", st: onchainState{Kind: "v4", Pool: hvPM},
			idsAll: []string{lqPoolID}, pre: []string{"v4:" + hvPM}, post: []string{"v4:" + hvPM}, exPre: base, exPost: base,
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
			p := holderVenuePlanFor(tc.st, upperToken, hvEntry, hvPostEnd, tc.ids, tc.idsAll, hvCfg)
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
	}
	for name, st := range states {
		want := holderVenuePlanFor(st, hvToken, hvEntry, hvEntry, nil, nil, hvCfg)
		for _, end := range []uint64{hvEntry, 499, 500, 10_000} {
			for _, later := range [][]string{nil, {lqPoolID}, {lqPoolID, lqOtherPoolID}} {
				p := holderVenuePlanFor(st, hvToken, hvEntry, end, nil, later, hvCfg)
				if !slices.Equal(venueStrings(p.Pre), venueStrings(want.Pre)) || !maps.Equal(p.ExcludedPre, want.ExcludedPre) ||
					p.LPApplicablePre != want.LPApplicablePre || !slices.Equal(p.V4PoolIDsPre, want.V4PoolIDsPre) {
					t.Errorf("%s, post end %d, later ids %v: pre plan %v %v %v %v, want %v %v %v %v", name, end, later,
						venueStrings(p.Pre), addrKeys(p.ExcludedPre), p.LPApplicablePre, p.V4PoolIDsPre,
						venueStrings(want.Pre), addrKeys(want.ExcludedPre), want.LPApplicablePre, want.V4PoolIDsPre)
				}
			}
		}
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
