package main

// The holder export's row: one per call, written to holders.jsonl and
// holders.csv (holders_out.go). The column order is the field order of
// holderRow and its embedded groups, read once by reflection from the json
// tags: this file is the one place that fixes it.
//
// NULL is a nil pointer (JSON null, an empty CSV field), never 0. Pointers are
// set only where the value is known and meaningful; see holders_rows.go for
// the rules.

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// holderRowStatus says what happened to a call.
type holderRowStatus string

const (
	holderRowOK      holderRowStatus = "ok"
	holderRowCapped  holderRowStatus = "capped"  // older than holderLookbackCap: features NULL
	holderRowSkipped holderRowStatus = "skipped" // see SkipReason
)

// holderSkipReason says why a call was skipped.
type holderSkipReason string

const (
	holderSkipNoTrackingRow     holderSkipReason = "no_tracking_row"
	holderSkipNotEVM            holderSkipReason = "not_evm"
	holderSkipNoOnchainState    holderSkipReason = "no_onchain_state" // no state, or a Kind holders_venues.go does not know
	holderSkipUnknownEntryBlock holderSkipReason = "unknown_entry_block"
	holderSkipNoMintFound       holderSkipReason = "no_mint_found"     // holderFetchNoMint
	holderSkipBadTransferLogs   holderSkipReason = "bad_transfer_logs" // holderFetchBadTransfers
)

// holderProvenance: where a row came from and what reading it cost. No prefix.
type holderProvenance struct {
	CallID     int64             `json:"call_id"`
	Token      string            `json:"token"`
	Chain      string            `json:"chain"`
	Status     holderRowStatus   `json:"status"`
	SkipReason *holderSkipReason `json:"skip_reason"`

	StateV        *int     `json:"state_v"` // onchainState.V
	VenueKind     *string  `json:"venue_kind"`
	VenuePool     *string  `json:"venue_pool"`
	VenuePoolID   *string  `json:"venue_pool_id"`
	VenueCurve    *string  `json:"venue_curve"`
	PonsGradBlock *uint64  `json:"pons_grad_block"`
	VenuesPre     []string `json:"venues_pre"`
	VenuesPost    []string `json:"venues_post"`
	ExcludedPre   []string `json:"excluded_pre"`    // sorted
	V4PoolIDs     []string `json:"v4_pool_ids"`     // every id given to the reader
	V4PoolIDsPre  []string `json:"v4_pool_ids_pre"` // the pre ids the pre_hold_* columns used (holderPreIDs)

	EntryAt        *time.Time `json:"entry_at"`
	EntryBlock     *uint64    `json:"entry_block"`
	LaunchBlock    *uint64    `json:"launch_block"`
	LaunchTime     *time.Time `json:"launch_time"`
	LaunchTx       *string    `json:"launch_tx"`
	Capped         bool       `json:"capped"`
	Deployer       *string    `json:"deployer"`
	DeployerSource *string    `json:"deployer_source"`
	PostEndBlock   *uint64    `json:"post_end_block"`
	PostEndTime    *time.Time `json:"post_end_time"`

	TransfersPre       *int `json:"transfers_pre"`
	TransfersPost      *int `json:"transfers_post"`
	LPEventsPre        *int `json:"lp_events_pre"`
	LPEventsPost       *int `json:"lp_events_post"`
	DupLogsDropped     *int `json:"dup_logs_dropped"`     // the streams read
	RemovedLogsDropped *int `json:"removed_logs_dropped"` // the streams read
	BadLPLogsSkipped   *int `json:"bad_lp_logs_skipped"`
	// BadTransferLogs: Transfer logs that did not decode (set once the
	// transfer stream was read; > 0 only for a bad_transfer_logs skip).
	BadTransferLogs *int `json:"bad_transfer_logs"`
	// BadTransferError: the first decode error's text (block, log index,
	// what did not decode; never a URL); bad_transfer_logs skips only.
	BadTransferError *string `json:"bad_transfer_error"`

	RPCLogsRequests  *int64 `json:"rpc_logs_requests"`
	RPCOtherRequests *int64 `json:"rpc_other_requests"`
	RPCRequestsTotal *int64 `json:"rpc_requests_total"`

	PostedAgeS  *int      `json:"posted_age_s"` // the age the call post showed
	ExportedAt  time.Time `json:"exported_at"`
	CodeVersion string    `json:"code_version"`
	ElapsedS    *float64  `json:"elapsed_s"`
}

// holderPreCols: features from the logs at or before the entry block only
// (holderPreFeatures). The only columns a model may use.
type holderPreCols struct {
	Incomplete     *bool    `json:"pre_hold_incomplete"`
	LaunchAgeS     *int64   `json:"pre_hold_launch_age_s"`
	Transfers      *int     `json:"pre_hold_transfers"`
	SupplyRaw      *string  `json:"pre_hold_supply_raw"`      // decimal, token base units
	CirculatingRaw *string  `json:"pre_hold_circulating_raw"` // decimal, token base units
	Holders        *int     `json:"pre_hold_holders"`
	Top1Share      *float64 `json:"pre_hold_top1_share"`
	Top5Share      *float64 `json:"pre_hold_top5_share"`
	Top10Share     *float64 `json:"pre_hold_top10_share"`
	DeployerShare  *float64 `json:"pre_hold_deployer_share"`
	DeployerSold   *bool    `json:"pre_hold_deployer_sold"`

	Buyers       *int     `json:"pre_hold_buyers"`
	Sellers      *int     `json:"pre_hold_sellers"`
	Buys         *int     `json:"pre_hold_buys"`
	Sells        *int     `json:"pre_hold_sells"`
	SellBuyRatio *float64 `json:"pre_hold_sell_buy_ratio"`

	FirstBuyBlock         *uint64  `json:"pre_hold_first_buy_block"`
	FirstBuyToEntryBlocks *uint64  `json:"pre_hold_first_buy_to_entry_blocks"`
	SniperWindowBlocks    *uint64  `json:"pre_hold_sniper_window_blocks"`
	Snipers               *int     `json:"pre_hold_snipers"`
	SniperHeldShare       *float64 `json:"pre_hold_sniper_held_share"`
	FirstBlockBuyers      *int     `json:"pre_hold_first_block_buyers"`
	MaxTxBuyers           *int     `json:"pre_hold_max_tx_buyers"`
	EarlyBuyers           []string `json:"pre_hold_early_buyers"`
	EarlyBuyersHeldShare  *float64 `json:"pre_hold_early_buyers_held_share"`

	LPApplicable         *bool `json:"pre_hold_lp_applicable"`
	LPAdds               *int  `json:"pre_hold_lp_adds"`
	LPRemoves            *int  `json:"pre_hold_lp_removes"`
	LPCollects           *int  `json:"pre_hold_lp_collects"`
	LPRemovedBeforeEntry *bool `json:"pre_hold_lp_removed_before_entry"`
	DeployerRemovedLP    *bool `json:"pre_hold_deployer_removed_lp"`
}

// holderPostCols: the 24 h after the call, for description only (never a
// feature). State columns (holders, shares, held shares) are at the window's
// end; flow columns (buyers, sellers, buys, sells, the ratio, deployer_sold,
// LP counts, deployer_removed_lp) count only (entry block, window end].
type holderPostCols struct {
	Incomplete    *bool    `json:"post24h_incomplete"`
	Transfers     *int     `json:"post24h_transfers"` // in the window only
	Holders       *int     `json:"post24h_holders"`
	Top1Share     *float64 `json:"post24h_top1_share"`
	Top5Share     *float64 `json:"post24h_top5_share"`
	Top10Share    *float64 `json:"post24h_top10_share"`
	DeployerShare *float64 `json:"post24h_deployer_share"`
	DeployerSold  *bool    `json:"post24h_deployer_sold"`

	Buyers       *int     `json:"post24h_buyers"`
	Sellers      *int     `json:"post24h_sellers"`
	Buys         *int     `json:"post24h_buys"`
	Sells        *int     `json:"post24h_sells"`
	SellBuyRatio *float64 `json:"post24h_sell_buy_ratio"`

	PreSnipersHeldShare  *float64 `json:"post24h_presnipers_held_share"`
	EarlyBuyersHeldShare *float64 `json:"post24h_early_buyers_held_share"`

	LPAdds            *int  `json:"post24h_lp_adds"`
	LPRemoves         *int  `json:"post24h_lp_removes"`
	LPCollects        *int  `json:"post24h_lp_collects"`
	DeployerRemovedLP *bool `json:"post24h_deployer_removed_lp"`
}

// holderRow is one call's row: provenance, then pre_hold_*, then post24h_*.
type holderRow struct {
	holderProvenance
	holderPreCols
	holderPostCols
}

// holderPreHoldPrefix starts every feature column.
const holderPreHoldPrefix = "pre_hold_"

// holderColumn is one column: its name and the path to its field.
type holderColumn struct {
	Name  string
	index []int
}

var holderColumnsOnce = sync.OnceValue(func() []holderColumn {
	return structColumns(reflect.TypeFor[holderRow](), nil)
})

// structColumns lists the json-tagged fields of t in order, descending into
// embedded structs (as encoding/json flattens them).
func structColumns(t reflect.Type, prefix []int) []holderColumn {
	var out []holderColumn
	for i := range t.NumField() {
		f := t.Field(i)
		idx := append(slices.Clone(prefix), i) // a fresh array per field
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, structColumns(f.Type, idx)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, holderColumn{Name: name, index: idx})
	}
	return out
}

// holderColumnNames returns every column name in the fixed order.
func holderColumnNames() []string {
	cols := holderColumnsOnce()
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// holderPreHoldColumns returns the feature columns (pre_hold_*), in order: the
// whitelist for the ml script.
func holderPreHoldColumns() []string {
	var out []string
	for _, n := range holderColumnNames() {
		if strings.HasPrefix(n, holderPreHoldPrefix) {
			out = append(out, n)
		}
	}
	return out
}

// holderCSVValues formats r's columns for holders.csv: NULL is "", lists are
// joined with ";", big integers are already decimal strings, times RFC 3339
// in UTC, floats in the shortest form that reads back exactly.
func holderCSVValues(r holderRow) []string {
	v := reflect.ValueOf(r)
	cols := holderColumnsOnce()
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = csvField(v.FieldByIndex(c.index))
	}
	return out
}

// csvField formats one field (see holderCSVValues).
func csvField(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if t, ok := v.Interface().(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	switch v.Kind() {
	case reflect.Slice:
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = csvField(v.Index(i))
		}
		return strings.Join(parts, ";")
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	}
	panic("holder csv: unsupported field kind " + v.Kind().String()) // a new column type needs a case here; the tests catch it
}
