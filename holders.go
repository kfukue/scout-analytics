package main

// Holder balances at a call's entry block, rebuilt by replaying the token's
// ERC-20 Transfer logs from launch. The node is a full node, not an archive,
// so balanceOf at an old block is unreliable; the logs are not. Everything in
// this file is pure: the caller reads the logs and passes them in.

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// deadAddr is the conventional burn address. Tokens sent here are counted as
// burned, like tokens sent to zeroAddr.
const deadAddr = "0x000000000000000000000000000000000000dead"

// tokenTransfer is one ERC-20 Transfer. Addresses are lowercase 0x hex.
type tokenTransfer struct {
	From     string
	To       string
	Value    *big.Int
	Block    uint64
	LogIndex uint64
	TxHash   string
}

var errBadTransferLog = errors.New("malformed Transfer log")

// errUnknownEntryBlock is returned by replayTransfers for entry block 0. In
// this codebase 0 means "unknown" (a missing entry_block is read as 0, and
// blockTag(0) is "latest"), so a replay "up to block 0" would silently mean
// something else.
var errUnknownEntryBlock = errors.New("entry block is 0 (unknown)")

// decodeTransferLog turns a raw Transfer(address indexed from, address
// indexed to, uint256 value) log into a tokenTransfer. Anything else, an
// ERC-721 Transfer (tokenId indexed: 4 topics, no data) included, is an error
// wrapping errBadTransferLog.
func decodeTransferLog(l rpcLog) (tokenTransfer, error) {
	if len(l.Topics) != 3 {
		return tokenTransfer{}, fmt.Errorf("%w: %d topics, want 3", errBadTransferLog, len(l.Topics))
	}
	if !strings.EqualFold(l.Topics[0], topicTransfer) {
		return tokenTransfer{}, fmt.Errorf("%w: topic0 %s is not Transfer", errBadTransferLog, l.Topics[0])
	}
	from, err := topicAddress(l.Topics[1])
	if err != nil {
		return tokenTransfer{}, fmt.Errorf("%w: from: %w", errBadTransferLog, err)
	}
	to, err := topicAddress(l.Topics[2])
	if err != nil {
		return tokenTransfer{}, fmt.Errorf("%w: to: %w", errBadTransferLog, err)
	}
	data, err := hexBytes(l.Data)
	if err != nil || len(data) != 32 {
		return tokenTransfer{}, fmt.Errorf("%w: data is not one 32-byte word", errBadTransferLog)
	}
	block, err := hexUint(l.BlockNumber)
	if err != nil {
		return tokenTransfer{}, fmt.Errorf("%w: block number: %w", errBadTransferLog, err)
	}
	idx, err := hexUint(l.LogIndex)
	if err != nil {
		return tokenTransfer{}, fmt.Errorf("%w: log index: %w", errBadTransferLog, err)
	}
	return tokenTransfer{From: from, To: to, Value: new(big.Int).SetBytes(data),
		Block: block, LogIndex: idx, TxHash: strings.ToLower(l.TxHash)}, nil
}

// topicAddress decodes an indexed address topic. It checks the length itself:
// addrFromWord returns zeroAddr for short input, which would turn a broken
// topic into a fake mint or burn.
func topicAddress(topic string) (string, error) {
	b, err := hexBytes(topic)
	if err != nil {
		return "", err
	}
	if len(b) != 32 {
		return "", fmt.Errorf("topic is %d bytes, want 32", len(b))
	}
	for _, c := range b[:12] {
		if c != 0 {
			return "", errors.New("topic is not a zero-padded address")
		}
	}
	return addrFromWord(b), nil
}

// hexBytes decodes 0x hex strictly (unhex drops errors).
func hexBytes(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

// hexUint parses a 0x quantity strictly (rpcLog.block and index turn bad
// input into 0).
func hexUint(s string) (uint64, error) {
	if !strings.HasPrefix(s, "0x") || len(s) == 2 {
		return 0, fmt.Errorf("bad quantity %q", s)
	}
	return strconv.ParseUint(s[2:], 16, 64)
}

// isBurnAddr reports whether tokens sent to a are destroyed.
func isBurnAddr(a string) bool { return a == zeroAddr || a == deadAddr }

// holderReplay is the token state after replaying its transfers up to
// EntryBlock.
type holderReplay struct {
	EntryBlock uint64
	// Balances per address, zero balances left out. Zero and dead never hold
	// a balance: what they receive is burned (crediting it as well would
	// subtract it twice from circulating). A negative balance stays as it is.
	Balances map[string]*big.Int
	Minted   *big.Int // sum of transfers from zeroAddr (or deadAddr, see applyTransfer)
	Burned   *big.Int // sum of transfers to zeroAddr or deadAddr
	Supply   *big.Int // Minted - Burned; never totalSupply()
	// Incomplete: some balance went below zero during the replay, so the
	// history does not reach back far enough (e.g. the look-back cap cut off
	// the mint) and every figure built on it is unreliable.
	Incomplete bool
	Negative   []string // the addresses that went below zero, sorted
	Applied    int      // transfers with Block <= EntryBlock
}

// replayTransfers rebuilds balances at entryBlock from transfers, applying
// only those with Block <= entryBlock.
//
// Order contract: the input may be in any order and is not modified; a copy
// is sorted by (Block, LogIndex). The final balances do not depend on order,
// but the negative-balance check does: out of order, a spend could be
// applied before the receipt that funded it.
//
// Every Value must be non-nil and non-negative (decodeTransferLog guarantees
// it); otherwise an error is returned. entryBlock 0 means the entry block is
// unknown and returns errUnknownEntryBlock.
func replayTransfers(transfers []tokenTransfer, entryBlock uint64) (holderReplay, error) {
	if entryBlock == 0 {
		return holderReplay{}, errUnknownEntryBlock
	}
	sorted := slices.Clone(transfers)
	slices.SortStableFunc(sorted, func(a, b tokenTransfer) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	r := holderReplay{EntryBlock: entryBlock, Balances: map[string]*big.Int{},
		Minted: new(big.Int), Burned: new(big.Int)}
	negative := map[string]bool{}
	for _, tr := range sorted {
		if tr.Block > entryBlock {
			break // sorted: everything after is later too
		}
		if tr.Value == nil || tr.Value.Sign() < 0 {
			return holderReplay{}, fmt.Errorf("transfer at block %d log %d: bad value %v", tr.Block, tr.LogIndex, tr.Value)
		}
		r.applyTransfer(tr, negative)
		r.Applied++
	}
	r.Supply = new(big.Int).Sub(r.Minted, r.Burned)
	r.Incomplete = len(negative) > 0
	r.Negative = slices.Sorted(maps.Keys(negative))
	return r, nil
}

// applyTransfer moves one transfer's value and records any address that goes
// below zero in negative. A transfer from deadAddr (only possible if someone
// holds its key) is treated as a mint, since what reached it was burned.
func (r *holderReplay) applyTransfer(tr tokenTransfer, negative map[string]bool) {
	from, to := strings.ToLower(tr.From), strings.ToLower(tr.To)
	if isBurnAddr(from) {
		r.Minted.Add(r.Minted, tr.Value)
	} else if r.addBalance(from, new(big.Int).Neg(tr.Value)) {
		negative[from] = true
	}
	if isBurnAddr(to) {
		r.Burned.Add(r.Burned, tr.Value)
	} else {
		r.addBalance(to, tr.Value)
	}
}

// addBalance adds delta to a's balance, drops it from the map at zero, and
// reports whether it is now negative.
func (r *holderReplay) addBalance(a string, delta *big.Int) bool {
	b := r.Balances[a]
	if b == nil {
		b = new(big.Int)
		r.Balances[a] = b
	}
	b.Add(b, delta)
	if b.Sign() == 0 {
		delete(r.Balances, a)
	}
	return b.Sign() < 0
}

// deployerSoldBefore reports whether deployer sent tokens to someone else at
// or before entryBlock: a transfer with From == deployer, Block <=
// entryBlock, Value > 0 and To neither the deployer itself nor a burn
// address. A transfer to a pool counts (that is how a sell shows up), and so
// does adding liquidity or paying a team wallet: the logs alone cannot tell
// them apart. A transfer to the token contract itself (a tax or treasury
// mechanism) also reads as a sell until the LP-events PR can tell real sells
// apart. Burns are not sells. Zero-value transfers are skipped because
// anyone can emit them with a forged from (address poisoning). An empty
// deployer means unknown: false.
func deployerSoldBefore(transfers []tokenTransfer, deployer string, entryBlock uint64) bool {
	deployer = strings.ToLower(deployer)
	if deployer == "" {
		return false
	}
	for _, tr := range transfers {
		if tr.Block > entryBlock || tr.Value == nil || tr.Value.Sign() <= 0 || strings.ToLower(tr.From) != deployer {
			continue
		}
		if to := strings.ToLower(tr.To); to != deployer && !isBurnAddr(to) {
			return true
		}
	}
	return false
}

// holderSummary is what holderStats reports for one call.
type holderSummary struct {
	Holders     int      // non-excluded addresses with a balance > 0
	Circulating *big.Int // supply minus the excluded addresses' balances
	// SharesOK is false when circulating is not > 0: then every share below
	// is 0 and must not be used.
	SharesOK          bool
	Top1, Top5, Top10 float64 // fractions of circulating, 0..1
	// DeployerKnown is false when no deployer was given: DeployerShareOK,
	// DeployerShare and DeployerSold are then false/0/false and mean nothing.
	DeployerKnown bool
	// DeployerShareOK is true only when the deployer is known, is not in the
	// excluded set (an excluded balance is not part of circulating, so its
	// share could exceed 1) and circulating is > 0. When false,
	// DeployerShare is 0 and must not be used.
	DeployerShareOK bool
	// DeployerShare is the deployer's balance over circulating, 0..1 (see
	// DeployerShareOK).
	DeployerShare float64
	DeployerSold  bool // see deployerSoldBefore; set whenever DeployerKnown
	Incomplete    bool // copied from the replay: the figures are unreliable
}

// holderStats summarises a replay. excluded holds the addresses that are not
// holders (pools, the v4 PoolManager, a Pons curve, zero, dead, the token
// itself); deployer is "" when unknown. transfers are the same ones that were
// replayed, for the deployer-sold check.
func holderStats(r holderReplay, transfers []tokenTransfer, excluded map[string]bool, deployer string) holderSummary {
	ex := lowerSet(excluded)
	s := holderSummary{Circulating: new(big.Int).Set(r.Supply), Incomplete: r.Incomplete}
	var held []*big.Int
	for a, b := range r.Balances {
		if ex[a] {
			s.Circulating.Sub(s.Circulating, b)
			continue
		}
		if b.Sign() > 0 {
			held = append(held, b)
		}
	}
	s.Holders = len(held)
	// Largest first. Equal balances need no tie-break: a top-N sum is the
	// same whichever of the tied holders is cut off.
	slices.SortFunc(held, func(a, b *big.Int) int { return b.Cmp(a) })
	s.SharesOK = s.Circulating.Sign() > 0
	if s.SharesOK {
		s.Top1 = shareOf(sumTop(held, 1), s.Circulating)
		s.Top5 = shareOf(sumTop(held, 5), s.Circulating)
		s.Top10 = shareOf(sumTop(held, 10), s.Circulating)
	}
	deployer = strings.ToLower(deployer)
	s.DeployerKnown = deployer != ""
	if s.DeployerKnown {
		s.DeployerSold = deployerSoldBefore(transfers, deployer, r.EntryBlock)
		s.DeployerShareOK = !ex[deployer] && s.SharesOK
		if b := r.Balances[deployer]; b != nil && s.DeployerShareOK {
			s.DeployerShare = shareOf(b, s.Circulating)
		}
	}
	return s
}

// sumTop adds the first n balances of a list sorted largest first (all of
// them when there are fewer than n).
func sumTop(sorted []*big.Int, n int) *big.Int {
	sum := new(big.Int)
	for _, b := range sorted[:min(n, len(sorted))] {
		sum.Add(sum, b)
	}
	return sum
}

// shareOf returns num/den as a float64, exact until the final rounding
// (balances with 18 decimals overflow every machine integer). den must be > 0.
func shareOf(num, den *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(num, den).Float64() // second value only says whether f is exact
	return f
}

// lowerSet copies a set of addresses with lowercase keys, so a checksummed
// pool address from the caller still matches the decoded (lowercase) ones.
func lowerSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for a, in := range m {
		if in {
			out[strings.ToLower(a)] = true
		}
	}
	return out
}
