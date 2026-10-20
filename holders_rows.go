package main

// The row builder of the holder export: one holderRow per call, from the
// call's metadata and what fetchHolderLogs read (holderLogs). Pure: no node,
// no database.
//
// Leakage: holderPreFeatures receives only the pre slices (logs at or before
// the entry block), the pre venues and the pre pool ids (V4PoolIDsPre); nothing
// after the entry block reaches a pre_hold_* column. holderPostFeatures builds
// the post24h_* columns (description only) from pre + post, with every pool
// id (V4PoolIDsAll).
//
// NULL rules (NULL, never 0):
//   - a skipped or capped call, or an incomplete replay (a balance went below
//     zero: the history is cut off): every derived column of that group is
//     NULL; only *_incomplete, the transfer counts and the provenance stay
//     (capped and skipped: *_incomplete and the counts are NULL too, nothing
//     was replayed);
//   - !SharesOK (circulating not > 0): top shares and held shares NULL;
//   - !DeployerShareOK: deployer share NULL;
//   - !DeployerKnown: deployer_sold and deployer_removed_lp NULL;
//   - !SellBuyRatioOK (nothing bought): the ratio NULL;
//   - !BuysOK (no buy before the call): first buy, snipers, first-block
//     buyers, max-tx buyers, early buyers and every held share NULL (post
//     held shares too: they follow the pre-call buyers);
//   - LP not applicable at the cut-off (a Pons token still on its curve, or a
//     v4 venue without pool ids: before the call, without pre ids): every LP
//     column NULL.
//
// Buy and sell counts of 0 are real values, never NULL on their own.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// holderCallMeta: what the exporter knows about a call before reading logs.
type holderCallMeta struct {
	CallID      int64
	Token       string
	Chain       string        // scout_calls.chain: "evm" or "solana"
	TrackingRow bool          // the call has a scout_call_tracking row
	State       *onchainState // the tracker's stored state; nil = none
	EntryAt     time.Time     // the tracking row's entry time
	PostedAgeS  *int          // the age the call post showed; nil = not shown
}

// holderRowStamp: the run's values put on every row.
type holderRowStamp struct {
	ExportedAt  time.Time
	CodeVersion string
	Elapsed     time.Duration // time spent on the call; 0 = not measured (elapsed_s NULL)
}

// holderRowInput: everything buildHolderRow needs for one call.
type holderRowInput struct {
	Meta holderCallMeta
	Logs holderLogs // fetchHolderLogs' result (no error)
	// V4PoolIDsPre: ONLY ids of the token's v4 pools initialised at or before
	// the entry block. They are the only ids any pre_hold_* column sees, so an
	// id learned from a later pool must never be put here (leakage). The
	// state's PoolID is added only for a Pons pool created (PonsGrad) at or
	// before the entry block; a plain v4 state's PoolID is a pre id only when
	// it is listed here, because the tracker does not check that the pool
	// existed at the entry block (holderPreIDs). The row records the pre ids
	// used in v4_pool_ids_pre.
	V4PoolIDsPre []string
	// V4PoolIDsAll: every other known id of the token's v4 pools, including
	// pools initialised after the entry block; used for the post window and
	// the reader only. V4PoolIDsPre and the state's PoolID are merged in.
	V4PoolIDsAll       []string
	Config             holderVenueConfig
	SniperWindowBlocks uint64
}

// holderSkipBeforeFetch returns why a call cannot be read, or "" when it can.
// A missing entry time counts as an unknown entry block (the reader needs
// both).
func holderSkipBeforeFetch(m holderCallMeta) holderSkipReason {
	switch {
	case !m.TrackingRow:
		return holderSkipNoTrackingRow
	case m.Chain != "evm" || !isAddress(m.Token):
		return holderSkipNotEVM
	case m.State == nil || !holderStateKnown(*m.State):
		return holderSkipNoOnchainState
	case m.State.EntryBlock == 0 || m.EntryAt.IsZero():
		return holderSkipUnknownEntryBlock
	}
	return ""
}

// holderFetchInputFor builds fetchHolderLogs' input for a call that
// holderSkipBeforeFetch lets through: every venue and every known pool id
// (idsPre, idsAll and the state's PoolID; see holderRowInput).
func holderFetchInputFor(m holderCallMeta, idsPre, idsAll []string, cfg holderVenueConfig) holderFetchInput {
	return holderFetchInput{Token: strings.ToLower(m.Token), EntryBlock: m.State.EntryBlock, EntryAt: m.EntryAt,
		Venues: holderFetchVenues(*m.State, cfg), V4PoolIDs: holderAllPoolIDs(*m.State, idsPre, idsAll)}
}

// skippedHolderRow is the row of a call skipped before any log was read.
func skippedHolderRow(m holderCallMeta, reason holderSkipReason, stamp holderRowStamp) holderRow {
	row := holderRow{holderProvenance: baseHolderProvenance(m, stamp)}
	row.skip(reason)
	return row
}

func (r *holderRow) skip(reason holderSkipReason) {
	r.Status, r.SkipReason = holderRowSkipped, &reason
}

// buildHolderRow builds the row of one call. A call holderSkipBeforeFetch
// rejects gives its skipped row (in.Logs is not used). An error means a bug
// or an input the reader never returns (an unknown status, a bad value).
func buildHolderRow(in holderRowInput, stamp holderRowStamp) (holderRow, error) {
	m := in.Meta
	if reason := holderSkipBeforeFetch(m); reason != "" {
		return skippedHolderRow(m, reason, stamp), nil
	}
	st, l := *m.State, in.Logs
	plan := holderVenuePlanFor(st, m.Token, st.EntryBlock, l.PostEndBlock, in.V4PoolIDsPre, in.V4PoolIDsAll, in.Config)
	row := holderRow{holderProvenance: baseHolderProvenance(m, stamp)}
	row.setPlan(plan, l.PostEndBlock > 0)
	row.setLogs(l)
	switch l.Status {
	case holderFetchOK:
	case holderFetchNoMint:
		row.skip(holderSkipNoMintFound)
		return row, nil
	case holderFetchBadTransfers:
		row.skip(holderSkipBadTransferLogs)
		return row, nil
	case holderFetchCapped:
		row.Status, row.Capped = holderRowCapped, true
		return row, nil
	default:
		return holderRow{}, fmt.Errorf("call %d: holder logs status %v", m.CallID, l.Status)
	}
	pre, err := holderPreFeatures(holderPreInput{Transfers: l.TransfersPre, Liquidity: l.LiquidityPre,
		Venues: plan.Pre, Excluded: plan.ExcludedPre, V4PoolIDs: plan.V4PoolIDsPre, LPApplicable: plan.LPApplicablePre,
		EntryBlock: st.EntryBlock, Deployer: l.Deployer, LaunchAge: l.Age, SniperWindowBlocks: in.SniperWindowBlocks})
	if err != nil {
		return holderRow{}, fmt.Errorf("call %d: %w", m.CallID, err)
	}
	post, err := holderPostFeatures(holderPostInput{Pre: l.TransfersPre, Post: l.TransfersPost,
		LiquidityPre: l.LiquidityPre, LiquidityPost: l.LiquidityPost, Venues: plan.Post, Excluded: plan.ExcludedPost,
		V4PoolIDs: plan.V4PoolIDsAll, LPApplicable: plan.LPApplicablePost, EntryBlock: st.EntryBlock,
		PostEndBlock: l.PostEndBlock, Deployer: l.Deployer, PreBuysOK: pre.BuysOK, PreSnipers: pre.Snipers,
		PreEarlyBuyers: pre.EarlyBuyers})
	if err != nil {
		return holderRow{}, fmt.Errorf("call %d: %w", m.CallID, err)
	}
	row.Status, row.holderPreCols, row.holderPostCols = holderRowOK, pre.Cols, post
	return row, nil
}

// baseHolderProvenance fills the columns known before any log is read.
func baseHolderProvenance(m holderCallMeta, stamp holderRowStamp) holderProvenance {
	p := holderProvenance{CallID: m.CallID, Token: m.Token, Chain: m.Chain,
		PostedAgeS: m.PostedAgeS, ExportedAt: stamp.ExportedAt.UTC(), CodeVersion: stamp.CodeVersion}
	if isAddress(m.Token) {
		p.Token = strings.ToLower(m.Token) // EVM only: a Solana address is case-sensitive
	}
	if stamp.Elapsed > 0 {
		p.ElapsedS = valPtr(stamp.Elapsed.Seconds())
	}
	if !m.EntryAt.IsZero() {
		p.EntryAt = valPtr(m.EntryAt.UTC())
	}
	if st := m.State; st != nil {
		p.StateV = valPtr(st.V)
		p.VenueKind, p.VenuePool = strPtr(st.Kind), strPtr(strings.ToLower(st.Pool))
		p.VenuePoolID, p.VenueCurve = strPtr(strings.ToLower(st.PoolID)), strPtr(strings.ToLower(st.Curve))
		p.PonsGradBlock = nonZero(st.PonsGrad)
		p.EntryBlock = nonZero(st.EntryBlock)
	}
	return p
}

// setPlan records the venues; the post venues only once the window's end is
// known.
func (r *holderRow) setPlan(p holderVenuePlan, postKnown bool) {
	r.VenuesPre = venueAddrs(p.Pre)
	r.ExcludedPre = slices.Sorted(maps.Keys(p.ExcludedPre))
	r.V4PoolIDs = append([]string{}, p.V4PoolIDsAll...)    // every id the reader was given; [] (none known), not NULL
	r.V4PoolIDsPre = append([]string{}, p.V4PoolIDsPre...) // the ids the pre_hold_* columns used; [] when none
	if postKnown {
		r.VenuesPost = venueAddrs(p.Post)
	}
}

// setLogs records what the reader found and what it cost. Each column is set
// only when the reader got that far (see holderLogs and its statuses).
func (r *holderRow) setLogs(l holderLogs) {
	r.RPCLogsRequests, r.RPCOtherRequests = valPtr(l.LogRequests), valPtr(l.OtherRequests)
	r.RPCRequestsTotal = valPtr(l.LogRequests + l.OtherRequests)
	r.LaunchBlock, r.LaunchTx = nonZero(l.LaunchBlock), strPtr(l.LaunchTx)
	if !l.LaunchAt.IsZero() {
		r.LaunchTime = valPtr(l.LaunchAt.UTC())
	}
	streamsRead := l.Status == holderFetchOK || l.Status == holderFetchBadTransfers
	if !streamsRead {
		return
	}
	r.Deployer, r.DeployerSource = strPtr(l.Deployer), strPtr(l.DeployerSource)
	r.PostEndBlock = nonZero(l.PostEndBlock)
	if !l.PostEndAt.IsZero() {
		r.PostEndTime = valPtr(l.PostEndAt.UTC())
	}
	// Summed over the streams read: the transfer stream alone for a
	// bad_transfer_logs skip (the liquidity stream was not read, its counts
	// are zero).
	r.DupLogsDropped = valPtr(l.TransferCounts.Duplicates + l.LiquidityCounts.Duplicates)
	r.RemovedLogsDropped = valPtr(l.TransferCounts.Removed + l.LiquidityCounts.Removed)
	r.BadTransferLogs = valPtr(l.TransferCounts.Bad)
	if l.Status != holderFetchOK {
		if l.BadTransfer != nil {
			// The decode error only (readTransferStream sets it on
			// errBadTransferLog alone): block, log index and what did not
			// decode; never a node URL.
			r.BadTransferError = strPtr(l.BadTransfer.Error())
		}
		return
	}
	r.TransfersPre, r.TransfersPost = valPtr(len(l.TransfersPre)), valPtr(len(l.TransfersPost))
	r.LPEventsPre, r.LPEventsPost = valPtr(len(l.LiquidityPre)), valPtr(len(l.LiquidityPost))
	r.BadLPLogsSkipped = valPtr(l.LiquidityCounts.Bad)
}

// holderPreInput: the pre-call data only. There is deliberately no field for
// anything after the entry block.
type holderPreInput struct {
	Transfers          []tokenTransfer  // holderLogs.TransfersPre
	Liquidity          []liquidityEvent // holderLogs.LiquidityPre
	Venues             []holderVenue    // holderVenuePlan.Pre
	Excluded           map[string]bool  // holderVenuePlan.ExcludedPre
	V4PoolIDs          []string         // holderVenuePlan.V4PoolIDsPre
	LPApplicable       bool             // holderVenuePlan.LPApplicablePre
	EntryBlock         uint64
	Deployer           string        // "" = unknown
	LaunchAge          time.Duration // entry time - launch block time
	SniperWindowBlocks uint64
}

// holderPreResult: the pre_hold_* columns, and the pre-call buyers the post
// columns follow.
type holderPreResult struct {
	Cols        holderPreCols
	BuysOK      bool
	Snipers     []string // the pre-call snipers (nil without a buy)
	EarlyBuyers []string // the first earlyBuyerCount pre-call buyers (nil without a buy)
}

// holderPreFeatures computes the pre_hold_* columns at the entry block.
func holderPreFeatures(in holderPreInput) (holderPreResult, error) {
	r, err := replayTransfers(in.Transfers, in.EntryBlock)
	if err != nil {
		return holderPreResult{}, fmt.Errorf("pre-call replay: %w", err)
	}
	var res holderPreResult
	c := &res.Cols
	c.Incomplete, c.Transfers = valPtr(r.Incomplete), valPtr(r.Applied)
	if r.Incomplete {
		return res, nil
	}
	venues, ids := venueAddrSet(in.Venues), idSet(in.V4PoolIDs)
	ex := unionSet(lowerSet(in.Excluded), venues)
	lp, err := liquidityTxs(in.Liquidity, venues, ids, in.EntryBlock)
	if err != nil {
		return holderPreResult{}, fmt.Errorf("pre-call liquidity: %w", err)
	}
	e, err := earlyTradeStats(in.Transfers, venues, ex, lp, in.EntryBlock, in.SniperWindowBlocks)
	if err != nil {
		return holderPreResult{}, fmt.Errorf("pre-call trades: %w", err)
	}
	hs := holderStats(r, in.Transfers, ex, in.Deployer)
	c.LaunchAgeS = valPtr(int64(in.LaunchAge / time.Second))
	c.SupplyRaw, c.CirculatingRaw = valPtr(r.Supply.String()), valPtr(hs.Circulating.String())
	s := holderStateColsOf(hs)
	c.Holders, c.Top1Share, c.Top5Share, c.Top10Share = s.Holders, s.Top1, s.Top5, s.Top10
	c.DeployerShare, c.DeployerSold = s.DeployerShare, s.DeployerSold
	f := holderFlowColsOf(e.Flow)
	c.Buyers, c.Sellers, c.Buys, c.Sells, c.SellBuyRatio = f.Buyers, f.Sellers, f.Buys, f.Sells, f.Ratio
	c.SniperWindowBlocks = valPtr(e.SniperWindowBlocks)
	if e.BuysOK {
		res.setBuys(e, in, venues, ex, lp)
	}
	if in.LPApplicable {
		ls, err := liquidityStats(in.Liquidity, in.Transfers, venues, ids, in.Deployer, in.EntryBlock)
		if err != nil {
			return holderPreResult{}, fmt.Errorf("pre-call liquidity stats: %w", err)
		}
		l := holderLPColsOf(ls)
		c.LPAdds, c.LPRemoves, c.LPCollects, c.DeployerRemovedLP = l.Adds, l.Removes, l.Collects, l.DeployerRemoved
		c.LPRemovedBeforeEntry = valPtr(ls.RemovedBeforeEntry)
	}
	c.LPApplicable = valPtr(in.LPApplicable)
	return res, nil
}

// setBuys fills the columns that need a pre-call buy, and the sniper list
// (earlyTrading has only its count: it is rebuilt from the same trades).
func (res *holderPreResult) setBuys(e earlyTrading, in holderPreInput, venues, ex map[string]bool, lp liquidityTxSet) {
	c := &res.Cols
	res.BuysOK, res.EarlyBuyers = true, e.EarlyBuyers
	res.Snipers = sniperBuyers(tradesUpTo(in.Transfers, venues, ex, lp, in.EntryBlock), e.FirstBuyBlock, e.SniperWindowBlocks)
	c.FirstBuyBlock = valPtr(e.FirstBuyBlock)
	c.FirstBuyToEntryBlocks = valPtr(in.EntryBlock - e.FirstBuyBlock)
	c.Snipers, c.FirstBlockBuyers, c.MaxTxBuyers = valPtr(e.Snipers), valPtr(e.FirstBlockBuyers), valPtr(e.MaxTxBuyers)
	c.EarlyBuyers = e.EarlyBuyers
	c.SniperHeldShare = ptrIf(e.SniperHeldShareOK, e.SniperHeldShare)
	c.EarlyBuyersHeldShare = ptrIf(e.EarlyBuyersHeldShareOK, e.EarlyBuyersHeldShare)
}

// holderPostInput: pre and post data, for the post24h_* columns.
type holderPostInput struct {
	Pre, Post                   []tokenTransfer
	LiquidityPre, LiquidityPost []liquidityEvent
	Venues                      []holderVenue   // holderVenuePlan.Post
	Excluded                    map[string]bool // holderVenuePlan.ExcludedPost
	V4PoolIDs                   []string        // holderVenuePlan.V4PoolIDsAll
	LPApplicable                bool            // holderVenuePlan.LPApplicablePost
	EntryBlock, PostEndBlock    uint64
	Deployer                    string
	PreBuysOK                   bool
	PreSnipers, PreEarlyBuyers  []string
}

// holderPostFeatures computes the post24h_* columns: holder state at the
// window's end, flow inside (entry block, window end].
func holderPostFeatures(in holderPostInput) (holderPostCols, error) {
	all := append(slices.Clone(in.Pre), in.Post...)
	r, err := replayTransfers(all, in.PostEndBlock)
	if err != nil {
		return holderPostCols{}, fmt.Errorf("post-window replay: %w", err)
	}
	c := holderPostCols{Incomplete: valPtr(r.Incomplete), Transfers: valPtr(len(in.Post))}
	if r.Incomplete {
		return c, nil
	}
	venues, ids := venueAddrSet(in.Venues), idSet(in.V4PoolIDs)
	ex := unionSet(lowerSet(in.Excluded), venues)
	hs := holderStats(r, all, ex, in.Deployer)
	s := holderStateColsOf(hs)
	c.Holders, c.Top1Share, c.Top5Share, c.Top10Share = s.Holders, s.Top1, s.Top5, s.Top10
	c.DeployerShare = s.DeployerShare
	if hs.DeployerKnown {
		c.DeployerSold = valPtr(deployerSoldBefore(in.Post, in.Deployer, in.PostEndBlock)) // in the window only
	}
	allLP := append(slices.Clone(in.LiquidityPre), in.LiquidityPost...)
	lp, err := liquidityTxs(allLP, venues, ids, in.PostEndBlock)
	if err != nil {
		return holderPostCols{}, fmt.Errorf("post-window liquidity: %w", err)
	}
	f := holderFlowColsOf(tradeFlow(tradesAfter(tradesUpTo(all, venues, ex, lp, in.PostEndBlock), in.EntryBlock)))
	c.Buyers, c.Sellers, c.Buys, c.Sells, c.SellBuyRatio = f.Buyers, f.Sellers, f.Buys, f.Sells, f.Ratio
	if in.PreBuysOK && hs.SharesOK {
		c.PreSnipersHeldShare = valPtr(shareOf(heldBy(r, ex, in.PreSnipers), hs.Circulating))
		c.EarlyBuyersHeldShare = valPtr(shareOf(heldBy(r, ex, in.PreEarlyBuyers), hs.Circulating))
	}
	if in.LPApplicable {
		// The window's events only: liquidityStats counts every event it is
		// given up to PostEndBlock.
		ls, err := liquidityStats(in.LiquidityPost, in.Post, venues, ids, in.Deployer, in.PostEndBlock)
		if err != nil {
			return holderPostCols{}, fmt.Errorf("post-window liquidity stats: %w", err)
		}
		l := holderLPColsOf(ls)
		c.LPAdds, c.LPRemoves, c.LPCollects, c.DeployerRemovedLP = l.Adds, l.Removes, l.Collects, l.DeployerRemoved
	}
	return c, nil
}

// tradesAfter keeps the trades after block b.
func tradesAfter(trades []earlyTrade, b uint64) []earlyTrade {
	var out []earlyTrade
	for _, t := range trades {
		if t.Block > b {
			out = append(out, t)
		}
	}
	return out
}

// holderStateCols: the holder-state columns shared by pre and post.
type holderStateCols struct {
	Holders                          *int
	Top1, Top5, Top10, DeployerShare *float64
	DeployerSold                     *bool // holderStats' (cumulative) value
}

func holderStateColsOf(s holderSummary) holderStateCols {
	c := holderStateCols{Holders: valPtr(s.Holders)}
	if s.SharesOK {
		c.Top1, c.Top5, c.Top10 = valPtr(s.Top1), valPtr(s.Top5), valPtr(s.Top10)
	}
	c.DeployerShare = ptrIf(s.DeployerShareOK, s.DeployerShare)
	c.DeployerSold = ptrIf(s.DeployerKnown, s.DeployerSold)
	return c
}

// holderFlowCols: the buy/sell columns shared by pre and post.
type holderFlowCols struct {
	Buyers, Sellers, Buys, Sells *int
	Ratio                        *float64
}

func holderFlowColsOf(f earlyFlow) holderFlowCols {
	return holderFlowCols{Buyers: valPtr(f.Buyers), Sellers: valPtr(f.Sellers), Buys: valPtr(f.Buys),
		Sells: valPtr(f.Sells), Ratio: ptrIf(f.SellBuyRatioOK, f.SellBuyRatio)}
}

// holderLPCols: the LP columns shared by pre and post.
type holderLPCols struct {
	Adds, Removes, Collects *int
	DeployerRemoved         *bool
}

func holderLPColsOf(s liquiditySummary) holderLPCols {
	return holderLPCols{Adds: valPtr(s.Adds), Removes: valPtr(s.Removes), Collects: valPtr(s.Collects),
		DeployerRemoved: ptrIf(s.DeployerKnown, s.DeployerRemoved)}
}

// valPtr returns a pointer to a copy of v.
func valPtr[T any](v T) *T { return &v }

// ptrIf returns a pointer to v when ok, nil (NULL) otherwise.
func ptrIf[T any](ok bool, v T) *T {
	if !ok {
		return nil
	}
	return &v
}

// nonZero: nil for 0 (a block number of 0 means unknown in this codebase).
func nonZero(n uint64) *uint64 { return ptrIf(n != 0, n) }
