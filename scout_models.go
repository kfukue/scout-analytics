package main

import (
	"encoding/json"
	"time"
)

// Call statuses.
const (
	CallStatusQueued    = "queued"    // waiting to be investigated
	CallStatusDuplicate = "duplicate" // CA was already investigated earlier; not re-run
	CallStatusDropped   = "dropped"   // queue was full
	CallStatusScanned   = "scanned"   // at least one tool returned a report
	CallStatusFailed    = "failed"    // every tool failed / timed out
	CallStatusBackfill  = "backfill"  // imported from channel history (not investigated)
	CallStatusUpdate    = "update"    // a "$TOKEN hit 3X …" post about an earlier call: recorded only (see postKind)
)

// Tracking statuses (scout_call_tracking.status).
const (
	TrackPending  = "pending"  // nothing fetched yet
	TrackTracking = "tracking" // some horizons done, more due later
	TrackDone     = "done"     // all horizons computed
	TrackNoPool   = "no_pool"  // price source doesn't know the token yet; retried
	TrackError    = "error"    // last attempt failed; retried with back-off
	TrackGaveUp   = "gave_up"  // no pool / no price data well after the last horizon
	TrackRepeat   = "repeat"   // a later call of a token whose first call is tracked; not tracked itself
)

// Investigation statuses.
const (
	investigationCompleted   = "completed"
	investigationFailed      = "failed"
	investigationTimeout     = "timeout"
	investigationRateLimited = "rate_limited" // bot kept answering "try again in N s"
)

// Delivery statuses.
const (
	DeliveryStatusSent   = "sent"
	DeliveryStatusFailed = "failed"
)

const scoutDBUser = "scoutanalytics"

// ScoutInvestigationTool is one registered investigation bot.
type ScoutInvestigationTool struct {
	ID              *int      `json:"id"`               //1
	Code            string    `json:"code"`             //2
	Name            string    `json:"name"`             //3
	BotUsername     string    `json:"bot_username"`     //4
	CommandTemplate string    `json:"command_template"` //5
	Parser          string    `json:"parser"`           //6
	IsGate          bool      `json:"is_gate"`          //7
	IsActive        bool      `json:"is_active"`        //8
	CreatedBy       string    `json:"created_by"`       //9
	CreatedAt       time.Time `json:"created_at"`       //10
	UpdatedBy       string    `json:"updated_by"`       //11
	UpdatedAt       time.Time `json:"updated_at"`       //12
}

// TableName returns the table name for this model.
func (ScoutInvestigationTool) TableName() string { return "scout_investigation_tools" }

// ScoutCall is one contract address found in one post of the watched channel.
type ScoutCall struct {
	ID              *int      `json:"id"`               //1
	UUID            string    `json:"uuid"`             //2
	ChannelID       int64     `json:"channel_id"`       //3
	ChannelUsername string    `json:"channel_username"` //4
	MessageID       int       `json:"message_id"`       //5
	MessageDate     time.Time `json:"message_date"`     //6
	MessageText     string    `json:"message_text"`     //7
	URLs            []string  `json:"urls"`             //8
	ContractAddress string    `json:"contract_address"` //9
	Chain           string    `json:"chain"`            //10
	Status          string    `json:"status"`           //11
	CreatedBy       string    `json:"created_by"`       //12
	CreatedAt       time.Time `json:"created_at"`       //13
	UpdatedBy       string    `json:"updated_by"`       //14
	UpdatedAt       time.Time `json:"updated_at"`       //15
	PostKind        string    `json:"post_kind"`        //16 call | update; set from MessageText on every write
}

// TableName returns the table name for this model.
func (ScoutCall) TableName() string { return "scout_calls" }

// ScoutInvestigation is one tool's request + report for one CA.
type ScoutInvestigation struct {
	ID              *int            `json:"id"`               //1
	UUID            string          `json:"uuid"`             //2
	CallID          *int            `json:"call_id"`          //3
	ToolID          int             `json:"tool_id"`          //4
	ContractAddress string          `json:"contract_address"` //5
	RequestText     string          `json:"request_text"`     //6
	RequestedAt     time.Time       `json:"requested_at"`     //7
	CompletedAt     *time.Time      `json:"completed_at"`     //8
	Status          string          `json:"status"`           //9
	BotMessageIDs   []int32         `json:"bot_message_ids"`  //10
	ReportText      string          `json:"report_text"`      //11
	ReportURLs      []string        `json:"report_urls"`      //12
	ReportURL       *string         `json:"report_url"`       //13
	ExternalID      *string         `json:"external_id"`      //14
	VerdictLevel    string          `json:"verdict_level"`    //15
	VerdictLabel    *string         `json:"verdict_label"`    //16
	Ticker          *string         `json:"ticker"`           //17
	VerdictSummary  *string         `json:"verdict_summary"`  //18
	VerdictSource   *string         `json:"verdict_source"`   //19
	Details         json.RawMessage `json:"details"`          //20
	Error           *string         `json:"error"`            //21
	CreatedBy       string          `json:"created_by"`       //22
	CreatedAt       time.Time       `json:"created_at"`       //23
	UpdatedBy       string          `json:"updated_by"`       //24
	UpdatedAt       time.Time       `json:"updated_at"`       //25

	ToolCode string `json:"tool_code,omitempty"` // filled by selects (join), not a column
}

// TableName returns the table name for this model.
func (ScoutInvestigation) TableName() string { return "scout_investigations" }

// ScoutDelivery is one bundle sent to the notify destination.
type ScoutDelivery struct {
	ID               *int       `json:"id"`                //1
	UUID             string     `json:"uuid"`              //2
	CallID           *int       `json:"call_id"`           //3
	ContractAddress  string     `json:"contract_address"`  //4
	Target           string     `json:"target"`            //5
	Status           string     `json:"status"`            //6
	HeaderText       string     `json:"header_text"`       //7
	DeliveredAt      *time.Time `json:"delivered_at"`      //8
	Error            *string    `json:"error"`             //9
	CreatedBy        string     `json:"created_by"`        //10
	CreatedAt        time.Time  `json:"created_at"`        //11
	UpdatedBy        string     `json:"updated_by"`        //12
	UpdatedAt        time.Time  `json:"updated_at"`        //13
	InvestigationIDs []int      `json:"investigation_ids"` // via scout_delivery_investigations
}

// TableName returns the table name for this model.
func (ScoutDelivery) TableName() string { return "scout_deliveries" }

// strPtr returns nil for "" so empty values are stored as NULL.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func chainOf(ca string) string {
	if len(ca) >= 2 && ca[:2] == "0x" {
		return "evm"
	}
	return "solana"
}

// ScoutCallMetrics is the parsed post data for one call (scout_call_metrics).
type ScoutCallMetrics struct {
	CallID             int             `json:"call_id"`
	Meta               CallMeta        `json:"meta"`
	LiveBuysEliteCount int             `json:"live_buys_elite_count"`
	LiveBuysGoodCount  int             `json:"live_buys_good_count"`
	LiveBuysEliteUSD   float64         `json:"live_buys_elite_usd"`
	LiveBuysGoodUSD    float64         `json:"live_buys_good_usd"`
	Parsed             json.RawMessage `json:"parsed"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// TableName returns the table name for this model.
func (ScoutCallMetrics) TableName() string { return "scout_call_metrics" }

// TableName for the live-buy rows (model: LiveBuy in callmeta.go).
func (LiveBuy) TableName() string { return "scout_call_live_buys" }

// ScoutCallTracking is one row of scout_call_tracking.
type ScoutCallTracking struct {
	CallID              int             `json:"call_id"`
	ContractAddress     string          `json:"contract_address"`
	EntryAt             time.Time       `json:"entry_at"`
	Priority            int             `json:"priority"`
	Status              string          `json:"status"`
	PoolAddress         *string         `json:"pool_address"`
	PoolName            *string         `json:"pool_name"`
	PoolDex             *string         `json:"pool_dex"`
	PoolCreatedAt       *time.Time      `json:"pool_created_at"`
	EntryPriceUSD       *float64        `json:"entry_price_usd"`
	EntryPriceSource    *string         `json:"entry_price_source"`
	CurrentPriceUSD     *float64        `json:"current_price_usd"`
	CurrentLiquidityUSD *float64        `json:"current_liquidity_usd"`
	Rugged              *bool           `json:"rugged"`
	NextCheckAt         time.Time       `json:"next_check_at"`
	LastCheckedAt       *time.Time      `json:"last_checked_at"`
	Attempts            int             `json:"attempts"`
	Error               *string         `json:"error"`
	PriceUnit           *string         `json:"price_unit"`           // "usd", or the quote asset's symbol when no USD source exists
	Onchain             json.RawMessage `json:"onchain"`              // on-chain tracker state (pool, scan progress)
	EntryLatePriceUSD   *float64        `json:"entry_late_price_usd"` // pool price SCOUT_ENTRY_DELAY after the post (same unit)
}

// TableName returns the table name for this model.
func (ScoutCallTracking) TableName() string { return "scout_call_tracking" }

// ScoutWebSummary is the import/tracking progress shown on the website. The
// website shows one row per token (its first call), so every count except
// TotalCalls, RepeatCalls and UpdatePosts is over first calls only, with the
// state taken from that call's scout_call_tracking row (the page works out the
// percentages). Update posts ("$TOKEN hit 3X …") are not calls: they are in
// UpdatePosts and in no other number.
type ScoutWebSummary struct {
	Imported    int       `json:"imported"`     // first calls = distinct tokens called
	Tracked     int       `json:"tracked"`      // first calls with an entry price
	Pending     int       `json:"pending"`      // first calls by status …
	Tracking    int       `json:"tracking"`     //
	Done        int       `json:"done"`         //
	NoPool      int       `json:"no_pool"`      //
	Error       int       `json:"error"`        //
	GaveUp      int       `json:"gave_up"`      //
	NoUSDPrice  int       `json:"no_usd_price"` // tracked first calls whose price_unit is not usd
	TotalCalls  int       `json:"total_calls"`  // every real call, repeats included
	RepeatCalls int       `json:"repeat_calls"` // TotalCalls − Imported: calls the website does not list
	UpdatePosts int       `json:"update_posts"` // update posts (not calls; not listed either)
	UpdatedAt   time.Time `json:"updated_at"`   // when the website last read the database (its snapshot)
	// seconds since UpdatedAt, at the time of the answer
	SnapshotAgeSeconds float64 `json:"snapshot_age_seconds"`
}

// ScoutWebHorizons are the windows the website can show, in the order of
// ScoutWebRow.Perf.
var ScoutWebHorizons = [5]string{"1h", "1d", "3d", "7d", "30d"}

// ScoutWebRow is one row of the website's list as stored: the first call of a
// token, with the results of every window. Perf holds, per window in the order
// of ScoutWebHorizons, the return, the peak and the worst drop (in percent,
// from the entry 60 seconds after the post, in PriceUnit); bit i of HasPerf is
// set when Perf[i] is known. EntryPrice is the late entry, else the price at the
// call (in PriceUnit); Tracked says the call has an entry price at all.
// CallCount and LastCallDate describe all real calls of the token (update posts
// left out; contract address compared without regard to letter case).
// PerceptorVerd and PerceptorURL are the token's latest completed Perceptor
// report (any post of the token), nil when it was never scanned.
type ScoutWebRow struct {
	CallID          int
	MessageID       int
	MessageDate     time.Time
	ChannelUsername string
	ContractAddress string
	TokenName       *string
	TokenSymbol     *string
	PriceUnit       *string
	EntryPrice      *float64
	Tracked         bool
	Rugged          *bool
	TrackingStatus  *string
	Perf            [15]float64
	HasPerf         uint16
	PerceptorVerd   *string // clean | caution | red_flags | unknown
	PerceptorURL    *string
	CallCount       int
	LastCallDate    time.Time

	// worked out by the website when it builds its snapshot (websnapshot.go)
	usd     bool  // priced in USD: only then are the numbers shown
	verdict uint8 // webBucket…: where the Perceptor filter puts the token
}

// ScoutWebCall is one row of the website's call list as sent to the page:
// the first call of a token. CallCount and LastCallDate describe all real calls
// (update posts left out) of that token (contract address compared without regard to letter case).
// The performance numbers are in USD, from the entry 60 seconds after the post,
// for the horizon that was asked for; they are nil for calls not priced in USD.
// PerceptorVerd and PerceptorURL are the token's latest completed Perceptor
// report (any post of the token), nil when it was never scanned.
type ScoutWebCall struct {
	CallID          int       `json:"call_id"`
	MessageID       int       `json:"message_id"`
	MessageDate     time.Time `json:"message_date"`
	PostURL         *string   `json:"post_url"`
	ContractAddress string    `json:"contract_address"`
	TokenName       *string   `json:"token_name"`
	TokenSymbol     *string   `json:"token_symbol"`
	GMGNURL         *string   `json:"gmgn_url"`
	PriceUnit       *string   `json:"price_unit"`
	EntryPriceUSD   *float64  `json:"entry_price_usd"`
	ReturnPct       *float64  `json:"return_pct"`
	PeakPct         *float64  `json:"peak_pct"`
	DrawdownPct     *float64  `json:"drawdown_pct"`
	Rugged          *bool     `json:"rugged"`
	TrackingStatus  *string   `json:"tracking_status"`
	PerceptorVerd   *string   `json:"perceptor_verdict"` // clean | caution | red_flags | unknown
	PerceptorURL    *string   `json:"perceptor_url"`     // report link (https only)
	CallCount       int       `json:"call_count"`        // calls of this token in total (≥ 1)
	LastCallDate    time.Time `json:"last_call_date"`    // most recent call of this token
}

// ScoutWebCallsFilter selects a page of the call list (first calls only). Sort, Dir, Horizon and Verdict
// must be values of the fixed lists below (parseWebCallsQuery checks them; the snapshot rejects anything else).
type ScoutWebCallsFilter struct {
	Q       string // substring of token name, symbol or contract address ("" = all)
	Sort    string // date | return | peak
	Dir     string // desc | asc
	Horizon string // 1h | 1d | 3d | 7d | 30d
	USDOnly bool   // only calls priced in USD
	Verdict string // "" = all | clean | caution | red_flags | not_scanned (no completed Perceptor report, or unknown)
	Page    int    // 1-based
	Per     int    // rows per page
}
