package main

// The holder log reader: the node reads behind the holder analysis of one
// call. holders.go, holders_early.go and holders_lp.go are pure and take what
// this file reads. Node reads only: no database, no writes.
//
//  1. Launch: the token's first mint (a Transfer from the zero address), in
//     one eth_getLogs over [1, entry block] (split automatically if the node
//     refuses the range or times out; the split search stops after the range
//     holding the first mint). Its block time gives the token's age at the
//     call; past holderLookbackCap the call is capped and nothing more is
//     read.
//  2. Deployer: the sender of the launch transaction.
//  3. The post window's end: the block at the entry time + holderPostWindow.
//  4. Two log streams over [launch block, post-window end], each read once
//     with the chunked reader: the token's Transfers, and the liquidity events
//     of its venues. Each is split at the entry block into pre and post.
//
// Node cost, with no range refused and no chunk answered from the log cache:
// 1 + (1 + F) × (⌊postEnd/S⌋ − ⌊launch/S⌋ + 1) eth_getLogs, where S is the
// chunk size (SCOUT_RPC_LOG_CHUNK) and F the number of liquidity filters (see
// liquidityFilters). The other requests (block times, the block search, the
// deployer) are counted apart; see holderLogs.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// holderLookbackCap: a token older than this at the call's entry time is not
// read (owner, 19 Oct 2026: "go back like 3 months"). Its history would cost
// hundreds of eth_getLogs per call, and every call in P0 was younger.
const holderLookbackCap = 90 * 24 * time.Hour

// holderPostWindow: how long after the entry time the streams are read, for
// description only (owner, 19 Oct 2026: features stop at the entry block).
const holderPostWindow = 24 * time.Hour

// Deployer sources (holderLogs.DeployerSource); "" = not found (the launch
// block was read and the launch tx is not in it, or there is no launch tx).
const (
	deployerFromMintTx    = "mint_tx_from"       // eth_getTransactionByHash of the launch tx
	deployerFromMintBlock = "mint_block_tx_from" // the launch tx found in its block (eth_getBlockByNumber)
	deployerLookupFailed  = "lookup_failed"      // no sender found and the launch block read failed with a node error; Deployer is ""
)

// holderVenue is one place the token trades: a v2 pair, a v3 pool, the v4
// PoolManager or a Pons bonding curve.
type holderVenue struct {
	Address string
	Kind    string // "v2", "v3", "v4" or "pons", as onchainState.Kind
}

// holderFetchInput is what fetchHolderLogs needs for one call. The caller
// fills it from the call's onchainState; the token's v4 pool ids come from
// there too (this file does not look for pools).
type holderFetchInput struct {
	Token      string
	EntryBlock uint64    // 0 = unknown: errUnknownEntryBlock
	EntryAt    time.Time // the time EntryBlock was found for
	Venues     []holderVenue
	// V4PoolIDs: the token's v4 pool ids (topic 1 of ModifyLiquidity). With
	// none, a v4 venue's liquidity is not read: on the shared PoolManager it
	// cannot be told apart from another token's.
	V4PoolIDs []string
}

// holderFetchStatus says how far fetchHolderLogs got for a call.
type holderFetchStatus int

const (
	// holderFetchOK: both streams were read.
	holderFetchOK holderFetchStatus = iota + 1
	// holderFetchNoMint: no mint at or before the entry block; nothing more
	// was read.
	holderFetchNoMint
	// holderFetchCapped: the token was older than holderLookbackCap at the
	// entry time; nothing was read after the launch block's time.
	holderFetchCapped
	// holderFetchBadTransfers: a Transfer log could not be decoded, so a
	// replay would be wrong; the liquidity stream was not read and the
	// transfer slices are empty (see holderLogs.BadTransfer).
	holderFetchBadTransfers
)

func (s holderFetchStatus) String() string {
	switch s {
	case holderFetchOK:
		return "ok"
	case holderFetchNoMint:
		return "no_mint"
	case holderFetchCapped:
		return "capped"
	case holderFetchBadTransfers:
		return "bad_transfer_logs"
	}
	return fmt.Sprintf("holderFetchStatus(%d)", int(s))
}

// logStreamCounts: what one stream's logs were, as the node returned them.
type logStreamCounts struct {
	Logs       int // returned by the node, every one counted
	Removed    int // dropped: marked removed by a chain reorganisation
	Duplicates int // dropped: the same (block, log index) as an earlier log
	Bad        int // could not be decoded (Transfer: the call is unusable; liquidity: skipped)
}

// venueLiquidity: whether a venue's liquidity events were read.
type venueLiquidity struct {
	holderVenue
	// Read is false for a Pons curve (no liquidity events) and for a v4 venue
	// without pool ids.
	Read bool
}

// holderLogs is what fetchHolderLogs read for one call. Addresses and hashes
// are lowercase.
type holderLogs struct {
	Status holderFetchStatus

	LaunchBlock uint64        // block of the first mint (0 with holderFetchNoMint)
	LaunchTx    string        // its transaction ("" when the node gave none)
	LaunchAt    time.Time     // the launch block's time (UTC)
	Age         time.Duration // EntryAt - LaunchAt

	Deployer       string // the launch transaction's sender; "" = unknown
	DeployerSource string // deployerFromMintTx, deployerFromMintBlock, deployerLookupFailed or "" (not found)

	// PostEndBlock: the last block at or before EntryAt + holderPostWindow
	// (never before the entry block). For a call less than holderPostWindow
	// old it is the chain's head: PostEndAt then shows how far it reaches.
	PostEndBlock uint64
	PostEndAt    time.Time

	// The streams, sorted by (block, log index): pre is at or before the
	// entry block, post after it and at or before PostEndBlock. Pre holds
	// every log in the entry block, including the ones after the call's own
	// transaction in that block (this reader does not know which one it is).
	TransfersPre, TransfersPost []tokenTransfer
	LiquidityPre, LiquidityPost []liquidityEvent
	TransferCounts              logStreamCounts
	LiquidityCounts             logStreamCounts
	BadTransfer                 error // the first Transfer decode error (holderFetchBadTransfers)

	// Venues and LiquidityRead are set only when Status is holderFetchOK
	// (nil and false otherwise).
	Venues        []venueLiquidity // the input venues, deduplicated
	LiquidityRead bool             // at least one venue's liquidity events were read

	// JSON-RPC requests for this call, every attempt counted (call()'s
	// retries, refused and split ranges): eth_getLogs, and everything else.
	// Not counted: chunks answered from the log cache or by another scan's
	// identical request, and a head block reused from blockNumber's cache.
	// An attempt is counted before it is sent: with parallel ranges
	// (SCOUT_RPC_PARALLEL > 1), a range cancelled because another range of its
	// batch was refused, or because the launch search stopped, is counted
	// although it may never have been sent. The counts are an upper bound on
	// the requests the node received.
	LogRequests, OtherRequests int64
}

// fetchHolderLogs reads the logs behind the holder analysis of one call (see
// the top of this file). No mint, a capped token and a bad Transfer log are
// results (Status), not errors. Errors: errUnknownEntryBlock for entry block 0
// and an input error, both before any node request; ctx.Err() once ctx is
// done; otherwise a node failure. On error only the request counts are set.
func (o *onchainSource) fetchHolderLogs(ctx context.Context, in holderFetchInput) (holderLogs, error) {
	if in.EntryBlock == 0 {
		return holderLogs{}, errUnknownEntryBlock
	}
	in, err := normalizeHolderInput(in)
	if err != nil {
		return holderLogs{}, err
	}
	logCtx, logN := withReqCounter(ctx)
	otherCtx, otherN := withReqCounter(ctx)
	r, err := o.readHolderLogs(logCtx, otherCtx, in)
	r.LogRequests, r.OtherRequests = logN.Load(), otherN.Load()
	if err != nil {
		counts := holderLogs{LogRequests: r.LogRequests, OtherRequests: r.OtherRequests}
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
		return counts, err
	}
	return r, nil
}

// normalizeHolderInput lowercases the addresses and pool ids, drops repeated
// venues and pool ids, and checks the token, the venues, the pool ids and the
// entry time.
func normalizeHolderInput(in holderFetchInput) (holderFetchInput, error) {
	in.Token = strings.ToLower(in.Token)
	if !isAddress(in.Token) {
		return in, fmt.Errorf("holder logs: token %q is not an address", in.Token)
	}
	if in.EntryAt.IsZero() {
		return in, errors.New("holder logs: entry time is not set")
	}
	var venues []holderVenue
	for _, v := range in.Venues {
		v = holderVenue{Address: strings.ToLower(v.Address), Kind: v.Kind}
		if _, ok := liquidityTopic0s[v.Kind]; !ok {
			return in, fmt.Errorf("holder logs: venue %s: unknown kind %q", v.Address, v.Kind)
		}
		if !isAddress(v.Address) {
			return in, fmt.Errorf("holder logs: venue %q is not an address", v.Address)
		}
		if !slices.Contains(venues, v) {
			venues = append(venues, v)
		}
	}
	in.Venues = venues
	var ids []string
	for _, id := range in.V4PoolIDs {
		if b, err := hexBytes(id); err != nil || len(b) != 32 || !strings.HasPrefix(id, "0x") {
			return in, fmt.Errorf("holder logs: v4 pool id %q is not 32 bytes of 0x hex", id)
		}
		if id = strings.ToLower(id); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids) // the same filters (and log cache keys) every time
	in.V4PoolIDs = ids
	return in, nil
}

// isAddress: a lowercase or mixed-case 0x address of 20 bytes.
func isAddress(a string) bool {
	b, err := hexBytes(a)
	return err == nil && len(b) == 20 && strings.HasPrefix(a, "0x")
}

// readHolderLogs does fetchHolderLogs' node reads: log reads under logCtx,
// everything else under otherCtx (two counters, one parent).
func (o *onchainSource) readHolderLogs(logCtx, otherCtx context.Context, in holderFetchInput) (holderLogs, error) {
	var r holderLogs
	launch, tx, found, err := o.findLaunch(logCtx, in.Token, in.EntryBlock)
	if err != nil {
		return r, err
	}
	if !found {
		r.Status = holderFetchNoMint
		return r, nil
	}
	r.LaunchBlock, r.LaunchTx = launch, tx
	ts, err := o.rpc.blockTime(otherCtx, launch)
	if err != nil {
		return r, fmt.Errorf("launch block %d time: %w", launch, err)
	}
	r.LaunchAt = time.Unix(ts, 0).UTC()
	r.Age = in.EntryAt.Sub(r.LaunchAt)
	if r.Age > holderLookbackCap {
		r.Status = holderFetchCapped
		return r, nil
	}
	if r.Deployer, r.DeployerSource, err = o.deployerOf(otherCtx, tx, launch); err != nil {
		return r, err
	}
	if r.PostEndBlock, r.PostEndAt, err = o.postWindowEnd(otherCtx, in); err != nil {
		return r, err
	}
	pre, post, err := o.readTransferStream(logCtx, in, launch, r.PostEndBlock, &r.TransferCounts)
	if errors.Is(err, errBadTransferLog) {
		r.Status, r.BadTransfer = holderFetchBadTransfers, err
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.TransfersPre, r.TransfersPost = pre, post
	if err := o.readLiquidityStream(logCtx, in, launch, &r); err != nil {
		return r, err
	}
	r.Status = holderFetchOK
	return r, nil
}

// findLaunch finds the token's first mint (a Transfer from the zero address)
// in blocks [1, entryBlock]: one range for the whole span (a ceiling above
// entryBlock), split automatically if the node refuses it or it times out,
// like v4PoolsOf. Logs marked removed are skipped. found is false when there
// is no mint.
//
// The chunked reader delivers logs in block order (ranges left to right, each
// sorted by block and log index, a refused range asked again from its start),
// so the first kept mint delivered is the token's first: the search stops
// there instead of reading the rest of the span. The range holding it is
// still delivered in full, and the earliest (block, log index) seen is kept,
// in case a range ever arrives unsorted.
func (o *onchainSource) findLaunch(ctx context.Context, token string, entryBlock uint64) (block uint64, tx string, found bool, err error) {
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var idx uint64
	topics := []any{topicTransfer, addrTopic(zeroAddr)}
	err = o.rpc.getLogsChunkedUpTo(sctx, token, topics, 1, entryBlock, entryBlock+1, func(l rpcLog) {
		b, errB := hexUint(l.BlockNumber)
		i, errI := hexUint(l.LogIndex)
		if l.Removed || errB != nil || errI != nil || b == 0 {
			return
		}
		if !found || b < block || (b == block && i < idx) {
			block, idx, tx, found = b, i, strings.ToLower(l.TxHash), true
		}
		stop() // the reader finishes this range, then returns sctx's error
	})
	if err != nil && found && ctx.Err() == nil {
		err = nil // stopped by stop(), not by the caller
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("mints of %s in blocks 1-%d: %w", token, entryBlock, err)
	}
	return block, tx, found, nil
}

// deployerOf returns the sender of the launch transaction: from
// eth_getTransactionByHash, or, when that fails or answers null, from the
// launch block read with its transactions. Otherwise the deployer is unknown,
// never an error, and the block read decides the source: "" (not found) when
// it succeeded without the tx, deployerLookupFailed when it failed with a node
// error (whether the tx lookup answered null or an error). The only error is
// ctx's.
func (o *onchainSource) deployerOf(ctx context.Context, tx string, block uint64) (deployer, source string, err error) {
	if tx == "" {
		return "", "", nil
	}
	from, errTx := o.rpc.txSender(ctx, tx)
	if errTx == nil && from != "" {
		return from, deployerFromMintTx, nil
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	from, errBlock := o.rpc.txSenderInBlock(ctx, tx, block)
	switch {
	case errBlock == nil && from != "":
		return from, deployerFromMintBlock, nil
	case ctx.Err() != nil:
		return "", "", ctx.Err()
	case errBlock != nil:
		return "", deployerLookupFailed, nil
	}
	return "", "", nil
}

// txSender returns a transaction's sender (eth_getTransactionByHash); "" when
// the node answers null or the sender is not an address.
func (c *rpcClient) txSender(ctx context.Context, tx string) (string, error) {
	var t *struct {
		From string `json:"from"`
	}
	if err := c.call(ctx, &t, "eth_getTransactionByHash", tx); err != nil {
		return "", err
	}
	if t == nil || !isAddress(t.From) {
		return "", nil
	}
	return strings.ToLower(t.From), nil
}

// txSenderInBlock reads block n with its transactions (eth_getBlockByNumber)
// and returns the sender of tx; "" when the block or the tx is not there.
func (c *rpcClient) txSenderInBlock(ctx context.Context, tx string, n uint64) (string, error) {
	var blk *struct {
		Transactions []struct {
			Hash string `json:"hash"`
			From string `json:"from"`
		} `json:"transactions"`
	}
	if err := c.call(ctx, &blk, "eth_getBlockByNumber", hexU64(n), true); err != nil {
		return "", err
	}
	if blk == nil {
		return "", nil
	}
	for _, t := range blk.Transactions {
		if strings.EqualFold(t.Hash, tx) && isAddress(t.From) {
			return strings.ToLower(t.From), nil
		}
	}
	return "", nil
}

// postWindowEnd returns the last block at or before EntryAt +
// holderPostWindow (never before the entry block) and its time. The time
// comes from blockTime's cache: blockAt has just read it.
func (o *onchainSource) postWindowEnd(ctx context.Context, in holderFetchInput) (uint64, time.Time, error) {
	end, err := o.rpc.blockAt(ctx, in.EntryAt.Add(holderPostWindow).Unix())
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("post-window end block: %w", err)
	}
	end = max(end, in.EntryBlock) // an entry time that does not match the entry block
	ts, err := o.rpc.blockTime(ctx, end)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("post-window end block %d time: %w", end, err)
	}
	return end, time.Unix(ts, 0).UTC(), nil
}

// readTransferStream reads the token's Transfer logs over [from, postEnd]
// once, drops removed and repeated logs, decodes them and splits them at the
// entry block. A log that does not decode returns an error wrapping
// errBadTransferLog (the first one), after every log was tried and counted.
func (o *onchainSource) readTransferStream(ctx context.Context, in holderFetchInput, from, postEnd uint64, counts *logStreamCounts) (pre, post []tokenTransfer, err error) {
	var logs []rpcLog
	err = o.rpc.getLogsChunked(ctx, in.Token, []any{topicTransfer}, from, postEnd, func(l rpcLog) {
		logs = append(logs, l)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("transfers of %s in blocks %d-%d: %w", in.Token, from, postEnd, err)
	}
	var transfers []tokenTransfer
	var bad error
	for _, l := range cleanLogs(logs, counts) {
		tr, err := decodeTransferLog(l)
		if err != nil {
			counts.Bad++
			if bad == nil {
				bad = fmt.Errorf("block %s log %s: %w", l.BlockNumber, l.LogIndex, err)
			}
			continue
		}
		transfers = append(transfers, tr)
	}
	if bad != nil {
		return nil, nil, bad
	}
	slices.SortStableFunc(transfers, func(a, b tokenTransfer) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	pre, post = splitAtEntry(transfers, func(t tokenTransfer) uint64 { return t.Block }, in.EntryBlock, postEnd)
	return pre, post, nil
}

// readLiquidityStream reads every venue's liquidity filters over
// [from, r.PostEndBlock] once, drops removed and repeated logs, decodes them
// (skipping and counting what does not decode) and splits them at the entry
// block into r. It also sets r.Venues and r.LiquidityRead.
func (o *onchainSource) readLiquidityStream(ctx context.Context, in holderFetchInput, from uint64, r *holderLogs) error {
	var logs []rpcLog
	for _, v := range in.Venues {
		filters := liquidityFilters(v, in.V4PoolIDs)
		r.Venues = append(r.Venues, venueLiquidity{holderVenue: v, Read: len(filters) > 0})
		r.LiquidityRead = r.LiquidityRead || len(filters) > 0
		for _, topics := range filters {
			err := o.rpc.getLogsChunked(ctx, v.Address, topics, from, r.PostEndBlock, func(l rpcLog) {
				logs = append(logs, l)
			})
			if err != nil {
				return fmt.Errorf("liquidity events of %s venue %s in blocks %d-%d: %w", v.Kind, v.Address, from, r.PostEndBlock, err)
			}
		}
	}
	var events []liquidityEvent
	for _, l := range cleanLogs(logs, &r.LiquidityCounts) {
		ev, err := decodeLiquidityLog(l)
		if errors.Is(err, errBadLiquidityLog) {
			r.LiquidityCounts.Bad++
			continue
		}
		if err != nil {
			return err
		}
		events = append(events, ev)
	}
	slices.SortStableFunc(events, func(a, b liquidityEvent) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	r.LiquidityPre, r.LiquidityPost = splitAtEntry(events, func(e liquidityEvent) uint64 { return e.Block }, in.EntryBlock, r.PostEndBlock)
	return nil
}

// liquidityTopic0s: the liquidity events read per venue kind (decoded by
// holders_lp.go). A Pons curve has none: its graduation is not a liquidity
// event.
var liquidityTopic0s = map[string][]any{
	"v2":   {topicMintV2, topicBurnV2},
	"v3":   {topicMintV3, topicBurnV3, topicCollectV3},
	"v4":   {topicModifyLiquidityV4},
	"pons": nil,
}

// liquidityFilters returns the eth_getLogs topic filters for one venue's
// liquidity events: one for a v2 or v3 pool (topic 0 in its events); for the
// v4 PoolManager one per batch of v4SwapIDBatch pool ids (topic 1 in the
// batch), none without pool ids; none for a Pons curve. ids must be sorted.
func liquidityFilters(v holderVenue, ids []string) [][]any {
	t0 := liquidityTopic0s[v.Kind]
	switch {
	case len(t0) == 0:
		return nil
	case v.Kind != "v4":
		return [][]any{{t0}}
	}
	var out [][]any
	for batch := range slices.Chunk(ids, v4SwapIDBatch) {
		var idTopic any = batch[0]
		if len(batch) > 1 {
			list := make([]any, len(batch))
			for i, id := range batch {
				list[i] = id
			}
			idTopic = list
		}
		out = append(out, []any{t0[0], idTopic})
	}
	return out
}

// cleanLogs drops the logs marked removed (a chain reorganisation), then the
// repeats of a (block, log index) already kept, and counts all three in c. A
// log whose block or index does not parse is kept: decoding rejects it.
func cleanLogs(logs []rpcLog, c *logStreamCounts) []rpcLog {
	c.Logs += len(logs)
	seen := map[[2]uint64]bool{}
	out := make([]rpcLog, 0, len(logs))
	for _, l := range logs {
		if l.Removed {
			c.Removed++
			continue
		}
		b, errB := hexUint(l.BlockNumber)
		i, errI := hexUint(l.LogIndex)
		if errB == nil && errI == nil {
			k := [2]uint64{b, i}
			if seen[k] {
				c.Duplicates++
				continue
			}
			seen[k] = true
		}
		out = append(out, l)
	}
	return out
}

// splitAtEntry splits items into pre (block <= entry) and post (entry <
// block <= end), keeping their order. Items after end (not asked for) are
// left out.
func splitAtEntry[T any](items []T, block func(T) uint64, entry, end uint64) (pre, post []T) {
	for _, it := range items {
		switch b := block(it); {
		case b <= entry:
			pre = append(pre, it)
		case b <= end:
			post = append(post, it)
		}
	}
	return pre, post
}
