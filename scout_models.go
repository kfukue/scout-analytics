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

// Kinds of investigation (scout_investigations.scan_kind).
const (
	ScanKindLive   = "live"   // run when the call came in (or by -scan / -post)
	ScanKindRescan = "rescan" // Perceptor re-scan long after the call (rescan lane): never a call-time feature
	ScanKindReask  = "reask"  // sAlpha asked again hours after the call (re-ask lane, salpha_reask.go): never a call-time feature
)

// Delivery statuses.
const (
	DeliveryStatusSent   = "sent"
	DeliveryStatusFailed = "failed"
	// DeliveryStatusSending: recorded by the live Perceptor re-check
	// (recheck.go) before it sends, then updated to sent or failed. A row left
	// at sending (the update failed, or the process died while sending) means
	// "attempted": that call is never delivered again. Only sent counts as sent.
	DeliveryStatusSending = "sending"
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

// ScoutCallPostCA is the (post, CA) key of a scout_calls row.
type ScoutCallPostCA struct {
	MessageID       int    `json:"message_id"`
	ContractAddress string `json:"contract_address"`
}

// ScoutCallToRequeue is a scout_calls row left queued (or dropped because the
// job queue was full) with no completed investigation: a restarted listener
// puts it back in the queue.
type ScoutCallToRequeue struct {
	ID              int       `json:"id"`
	MessageID       int       `json:"message_id"`
	MessageDate     time.Time `json:"message_date"`
	MessageText     string    `json:"message_text"`
	ContractAddress string    `json:"contract_address"`
	Status          string    `json:"status"`
	// CAInvestigated: another call of the same token has a completed
	// investigation, so this one is a duplicate rather than a scan to redo.
	CAInvestigated bool `json:"ca_investigated"`
}

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
	// ScanKind: ScanKindLive (default when empty), ScanKindRescan or ScanKindReask.
	ScanKind string `json:"scan_kind"` //26

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
	// NotifyMessageID: the id of the delivery's first message (the header) in
	// the delivery chat, as the sender saw it; nil when unknown (older rows,
	// failed sends, an answer that did not name it).
	NotifyMessageID *int64 `json:"notify_message_id"`
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
// PerceptorVerd and PerceptorURL are the token's latest completed live
// Perceptor report (scan_kind 'live', any post of the token), nil when it was
// never scanned at the time of a call; PerceptorID is
// that investigation's id. SAlphaID is the token's latest completed sAlpha
// investigation whose report_text is not empty (nor only white space), nil
// when there is none; a reply that only declines to report (see
// salphaDeclined) is chosen only when the token has no real report. The texts of both are read separately, by id
// (ScoutStore.SelectWebReports).
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
	PerceptorVerd   *string // clean | caution | red_flags | insufficient | unknown
	PerceptorURL    *string
	PerceptorID     *int
	// PerceptorAt: when the live report PerceptorVerd comes from completed
	// (completed_at, else requested_at); nil without one. Not sent: only the
	// Perceptor filter's "latest report" rule (webLatestPerceptor) reads it.
	PerceptorAt *time.Time
	// "Perceptor today": the token's latest completed rescan (scan_kind
	// 'rescan', run long after the call by the rescan lane): its verdict
	// (clean | caution | red_flags | insufficient | unknown), link and time (completed_at,
	// else requested_at); all nil without one. Never mixed into the fields
	// above, which come from live scans only.
	PerceptorTodayVerd *string
	PerceptorTodayURL  *string
	PerceptorTodayAt   *time.Time
	SAlphaID           *int
	// SAlphaKind and SAlphaAt: the scan_kind (live | reask) and requested_at
	// of the investigation SAlphaID names; nil without one.
	SAlphaKind   *string
	SAlphaAt     *time.Time
	CallCount    int
	LastCallDate time.Time
	// The tracker's latest-price pass (in PriceUnit; nil until the first refresh):
	// return as of the most recent pool price, measured like Perf from the late
	// entry; that price; when it was read; and the time of the trade behind it.
	LatestReturn  *float64
	LatestPrice   *float64
	LatestAt      *time.Time
	LatestTradeAt *time.Time
	// Market caps from the post (scout_call_metrics, USD): the "called at" figure
	// and the "Mcap" line; and the price at the post (scout_call_tracking.entry_price_usd,
	// not the late entry), the divisor of the latest market cap estimate.
	CalledAtMcap *float64
	PostMcap     *float64
	PostPrice    *float64

	// For the Analytics page only (GET /api/analytics; not in the list's rows):
	// PostedDex is the DEX named in the post (scout_call_metrics.dex);
	// EntrySource is scout_call_tracking.entry_price_source (onchain-v2 |
	// onchain-v3 | onchain-v4 | onchain-pons, or minute | hour for
	// GeckoTerminal; nil when untracked); QuoteSym is the pool's quote asset
	// (onchain->>'quote_sym'). NoData has bit h set when window h of
	// ScoutWebHorizons is stored as no_data (its numbers are then missing,
	// although the window is due). VerdictAtCall is the Perceptor verdict known
	// at the time of this call (the call's own live scan, else the token's latest
	// live scan made before it; the dataset view's rule), never a later repeat
	// call's or a re-scan's. TradesFinal says the tracker has stored the 5-minute
	// candles of the first 24 hours (on-chain source, state version 2 or later,
	// the 1d window stored); only then is Trades24h, the swaps counted in them,
	// known. The website fills Trades24h itself (webServer.fillTrades24h).
	PostedDex     *string
	EntrySource   *string
	QuoteSym      *string
	NoData        uint8
	VerdictAtCall *string // clean | caution | red_flags | unknown
	TradesFinal   bool
	Trades24h     *int
	// Known at the time of the call, for the Analytics page's "By factor"
	// tab, read with the expressions of scout_call_dataset_v (so they match
	// the ML data): from the post (scout_call_metrics; all nil without a
	// parsed post) the holders, the elite and good holders ("proof"), and the
	// elite and good live buys (count and USD); from the hour of trading
	// before the call (scout_call_precall; all nil without a row) the buy and
	// sell volume (in PreVolUnit: usd, or the quote asset's symbol), the swaps
	// and the price change in percent.
	Holders            *int
	ProofElite         *int
	ProofGood          *int
	LiveBuysEliteCount *int
	LiveBuysGoodCount  *int
	LiveBuysEliteUSD   *float64
	LiveBuysGoodUSD    *float64
	PreBuyVol60        *float64
	PreSellVol60       *float64
	PreSwaps60         *int
	PreChg60           *float64
	PreVolUnit         *string
	// TokenSupply is the token's supply in whole tokens read from the chain
	// by the tracker (scout_call_tracking.token_supply: at the entry block
	// when the node still had that state, else a later latest block); nil
	// when not read or none. With PostPrice it gives the Analytics page's
	// market cap at the call (webAnaMcap). Not an ML feature.
	TokenSupply *float64
	// Timing after the call, for the Analytics page's "Timing" tab (outcomes,
	// never model inputs; see timing.go): per window h of ScoutWebHorizons
	// whose scout_call_returns row is done and has its timing (timing_at set:
	// bit h of Timed), Timing[h*webTimingPerHorizon+k] holds, in seconds after
	// entry_at, k = 0 peak_late_after_s, 1 first_2x_after_s, 2 above_2x_s,
	// 3 fall_below_2x_after_s; bit h*webTimingPerHorizon+k of HasTiming is set
	// when that value is not NULL. Bit h of Censored2x is above_2x_censored.
	// RugAfterS is scout_call_tracking.rug_at − entry_at in whole seconds
	// (can be ≤ 0 for a rug at the call), RugAtKind its rug_at_kind (event |
	// at_call | detected | price); both nil when not stored. Sent for every row; the
	// page uses them for calls priced in USD only, like every number.
	Timing     [len(ScoutWebHorizons) * webTimingPerHorizon]int32
	HasTiming  uint32
	Timed      uint8
	Censored2x uint8
	RugAfterS  *int
	RugAtKind  *string
	// Model: the model's score of this call made at the time of the call
	// (scout_call_predictions, merged in by attachWebModel); nil without one.
	// Display only: never an input of the dataset view or of a model.
	Model *ScoutWebModel

	// worked out by the website when it builds its snapshot (websnapshot.go)
	usd bool // priced in USD: only then are the numbers shown
	// latestVerd: the verdict of the token's latest Perceptor report with a
	// verdict (webLatestPerceptor: PerceptorVerd or PerceptorTodayVerd), the
	// one the Perceptor filter uses; verdict is its webBucket… (where the
	// filter and its counts put the token).
	latestVerd *string
	verdict    uint8
	// salphaDeclined: the sAlpha report SAlphaID names only declines to report
	// (set by webReportsFor from its text)
	salphaDeclined bool
	// salphaReask: the sAlpha report SAlphaID names was asked again by the
	// re-ask lane (scan_kind 'reask'), salphaReaskAfterS seconds after the
	// call (both set by newWebSnapshot)
	salphaReask       bool
	salphaReaskAfterS int64
	// callMcap: CalledAtMcap, else PostMcap. latestMcap: an estimate,
	// (PostMcap, else CalledAtMcap) × LatestPrice ÷ PostPrice. "Else" = when the
	// first is missing, zero, negative or not finite. Both nil unless every input
	// is there, positive and finite and the call is priced in USD.
	// They point into mcapVals (set by setWebMcaps).
	callMcap   *float64
	latestMcap *float64
	mcapVals   [2]float64
	// anaHas and anaPerf: HasPerf and Perf as read, for USD-priced rows only
	// (0 otherwise), kept before the list's rules hide the peak of a rugged
	// call; the Analytics page counts a rugged call's peak.
	anaHas  uint16
	anaPerf [15]float64
}

// ScoutWebCall is one row of the website's call list as sent to the page:
// the first call of a token. CallCount and LastCallDate describe all real calls
// (update posts left out) of that token (contract address compared without regard to letter case).
// The performance numbers are in USD, from the entry 60 seconds after the post:
// ReturnPct, PeakPct and DrawdownPct for the horizon that was asked for, the
// Return…Pct fields for every window; they are nil for calls not priced in USD.
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
	// The return of each window (late entry, USD), whatever the horizon asked
	// for: the number ReturnPct has for that window. nil until the window is
	// recorded, and for calls not priced in USD.
	Return1hPct    *float64  `json:"return_1h_pct"`
	Return1dPct    *float64  `json:"return_1d_pct"`
	Return3dPct    *float64  `json:"return_3d_pct"`
	Return7dPct    *float64  `json:"return_7d_pct"`
	Return30dPct   *float64  `json:"return_30d_pct"`
	Rugged         *bool     `json:"rugged"`
	RugKind        *string   `json:"rug_kind"` // rug_at_kind: event | at_call | detected (pool drained) | price (collapsed: down > 95%, not seen drained)
	TrackingStatus *string   `json:"tracking_status"`
	PerceptorVerd  *string   `json:"perceptor_verdict"` // clean | caution | red_flags | insufficient | unknown
	PerceptorURL   *string   `json:"perceptor_url"`     // report link (https only)
	CallCount      int       `json:"call_count"`        // calls of this token in total (≥ 1)
	LastCallDate   time.Time `json:"last_call_date"`    // most recent call of this token
	// Return as of the most recent price (not tied to the horizon asked for);
	// all five are nil until the tracker has read a latest price, and for calls
	// not priced in USD.
	LatestReturnPct  *float64   `json:"latest_return_pct"`
	LatestPriceUSD   *float64   `json:"latest_price_usd"`
	LatestAt         *time.Time `json:"latest_at"`          // when the price was read (it is "as of" this time)
	LatestTradeAt    *time.Time `json:"latest_trade_at"`    // the last trade the price comes from
	LatestAgeSeconds *int64     `json:"latest_age_seconds"` // age of the call at LatestAt
	// Market cap at the call, from the post ("called at", else the "Mcap"
	// line), and an estimate of the market cap at the latest price: the post's
	// market cap ("Mcap" line, else "called at") × latest price ÷ price at the
	// post (assumes the token supply has not changed). "Else" = when the first
	// figure is missing, zero, negative or not finite. nil when no figure or
	// price is usable, and for calls not priced in USD; LatestMcapUSD also when
	// there is no latest price.
	CallMcapUSD   *float64 `json:"call_mcap_usd"`
	LatestMcapUSD *float64 `json:"latest_mcap_usd"`
	// The token's reports, whose texts GET /api/call returns: HasSAlpha says
	// the token has an sAlpha report with text (an empty one counts as none,
	// and so does a reply that declines to report: SAlphaReportID then still
	// names it, so the detail can say that sAlpha did not report);
	// the two ids are the investigations the detail shows (nil = none). A new
	// report changes them, so the page knows to ask for the detail again.
	HasSAlpha         bool `json:"has_salpha_report"`
	PerceptorReportID *int `json:"perceptor_report_id"`
	SAlphaReportID    *int `json:"salpha_report_id"`
	// SAlphaReaskAfterS: the sAlpha report named above was not given at the
	// time of the call: sAlpha was asked again (re-ask lane,
	// SCOUT_SALPHA_REASK) this many seconds after the call. nil for a report
	// of the call itself, and without one.
	SAlphaReaskAfterS *int64 `json:"salpha_reask_after_s"`
	// "Perceptor today": the token's latest completed Perceptor re-scan by the
	// rescan lane or the 2-hour re-check, run long after the call
	// (SCOUT_RESCAN): its verdict (clean | caution | red_flags | insufficient
	// | unknown), report link (https only) and when it ran. All three are nil
	// without one. perceptor_verdict, perceptor_url and perceptor_report_id
	// above come from scans made at the time of a call only (with the
	// 12-minute re-check), never from a re-scan.
	PerceptorTodayVerd *string    `json:"perceptor_today_verdict"`
	PerceptorTodayURL  *string    `json:"perceptor_today_url"`
	PerceptorTodayAt   *time.Time `json:"perceptor_today_at"`
	// PerceptorLatestVerd: the verdict the Perceptor filter (verdict=) and
	// its counts use: that of the token's latest Perceptor report with a
	// verdict, perceptor_verdict or perceptor_today_verdict (the rule is
	// webLatestPerceptor's). nil when the token has no completed report.
	PerceptorLatestVerd *string `json:"perceptor_latest_verdict"`
	// Model: the model's score of the call, made at the time of the call (at
	// most webModelMaxDelay after it; a later back-score is not shown); nil
	// without one, and when the website does not show scores
	// (webConfig.ModelScores). Kept last: it must never come between the
	// three numbers of the window (webRowNumbersJSON).
	Model *ScoutWebCallModel `json:"model"`
}

// ScoutWebModelBuckets are the model's holding-period buckets in the order
// of ScoutWebModel.Buckets, and ScoutWebModelHorizons the window each one is
// about (ml/scout_ml/config.py BUCKETS horizon_days).
var (
	ScoutWebModelBuckets  = [4]string{"short", "3day", "medium", "long"}
	ScoutWebModelHorizons = [4]string{"1d", "3d", "7d", "30d"}
)

// ScoutWebModelBucket is one bucket of a stored score: Has says the score has
// the bucket; the three numbers (nil when missing or not finite) and Passed
// (whether the bucket's model passed its validation gates; nil = not
// recorded) are scout_call_predictions' runner_prob, collapse_prob,
// runner_rank_pct and passed.
type ScoutWebModelBucket struct {
	Has      bool
	Runner   *float64
	Collapse *float64
	Rank     *float64
	Passed   *bool
}

// ScoutWebModel is the score of a call as the website shows it: one model
// version, when it was stored (created_at; every bucket of one score is
// stored in one transaction, so they share it) and its buckets in the order
// of ScoutWebModelBuckets.
type ScoutWebModel struct {
	Version  string
	ScoredAt time.Time
	Buckets  [len(ScoutWebModelBuckets)]ScoutWebModelBucket
}

// ScoutWebPrediction is one bucket row of scout_call_predictions as the
// website reads it (never its features).
type ScoutWebPrediction struct {
	CallID    int
	Version   string
	Bucket    string
	Runner    *float64
	Collapse  *float64
	Rank      *float64
	Passed    *bool
	CreatedAt time.Time
}

// ScoutWebCallModel is the "model" object of a row of /api/calls: the model
// version, when the score was stored and how many seconds after the call,
// and the buckets the score has, in the order of ScoutWebModelBuckets.
type ScoutWebCallModel struct {
	Version  string                    `json:"version"`
	ScoredAt time.Time                 `json:"scored_at"`
	AfterS   int64                     `json:"after_s"`
	Buckets  []ScoutWebCallModelBucket `json:"buckets"`
}

// ScoutWebCallModelBucket is one bucket of ScoutWebCallModel: its name, the
// window it is about, the probability of a runner and of a collapse (0 … 1),
// the share of the calls the model was tested on when it was trained that had
// a lower runner score (percent; the call is in the top 100 − rank percent)
// and whether the bucket's model passed its validation gates (null = not
// recorded).
type ScoutWebCallModelBucket struct {
	Bucket        string   `json:"bucket"`
	Horizon       string   `json:"horizon"`
	RunnerProb    *float64 `json:"runner_prob"`
	CollapseProb  *float64 `json:"collapse_prob"`
	RunnerRankPct *float64 `json:"runner_rank_pct"`
	Passed        *bool    `json:"passed"`
}

// ScoutWebReport is one investigation as the website's row detail shows it
// (ScoutStore.SelectWebReports). Tool is perceptor or salpha. At is
// completed_at, else requested_at. Verdict is clean | caution | red_flags |
// unknown. Label and Summary are verdict_label and verdict_summary; Text is
// report_text (read for sAlpha only, at most webReportMaxBytes and a little);
// URL is report_url as stored (the website keeps https:// links only).
type ScoutWebReport struct {
	ID      int
	Tool    string
	At      time.Time
	Verdict string
	Label   *string
	Summary *string
	Text    string
	URL     *string
	Kind    string // scan_kind: live | rescan | reask
}

// ScoutWebCallsFilter selects a page of the call list (first calls only). Sort, Dir and Horizon must be
// values of the fixed lists below, and Verdict a comma list of the names below (parseWebCallsQuery checks
// them; the snapshot rejects anything else).
type ScoutWebCallsFilter struct {
	Q       string // substring of token name, symbol or contract address ("" = all)
	Sort    string // date | return | peak | latest | call_mc | latest_mc | model | return_1h … return_30d
	Dir     string // desc | asc
	Horizon string // 1h | 1d | 3d | 7d | 30d
	USDOnly bool   // only calls priced in USD
	Verdict string // normalised comma list in the order clean,caution,red_flags,not_scanned, e.g. "clean,caution" ("" = all: none or all four given); not_scanned = no completed Perceptor report, or unknown
	Page    int    // 1-based
	Per     int    // rows per page
}
