package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Website snapshot: the whole list (one row per token) held in memory, so a
// request never waits for the database. The data of a snapshot is never
// changed after it is built; the server swaps in a new one after every refresh.
// ---------------------------------------------------------------------------

// Perceptor filter buckets of a row.
const (
	webBucketClean uint8 = iota
	webBucketCaution
	webBucketRedFlags
	webBucketNotScanned // no completed report, or one without a readable verdict
	webBuckets
)

// verdict filter → bucket
var webVerdictBuckets = map[string]uint8{
	"clean":       webBucketClean,
	"caution":     webBucketCaution,
	"red_flags":   webBucketRedFlags,
	"not_scanned": webBucketNotScanned,
}

// Sort keys: 0 = date, then return and peak of each window, then the return
// as of the latest price, the market cap at the call and the estimated market
// cap at the latest price.
const (
	webSortKeyDate     = 0
	webSortKeyReturn   = 1
	webSortKeyPeak     = 1 + len(ScoutWebHorizons)
	webSortKeyLatest   = 1 + 2*len(ScoutWebHorizons)
	webSortKeyCallMC   = 2 + 2*len(ScoutWebHorizons)
	webSortKeyLatestMC = 3 + 2*len(ScoutWebHorizons)
	webSortKeys        = 4 + 2*len(ScoutWebHorizons)
)

// webSortNames are the values of the sort parameter of /api/calls. return and
// peak are for the window asked for (horizon); return_1h … return_30d are the
// return of one window each, whatever the horizon.
var webSortNames = []string{"date", "return", "peak", "latest", "call_mc", "latest_mc",
	"return_1h", "return_1d", "return_3d", "return_7d", "return_30d"}

// webReturnSorts: sort=return_<window> → the index of the window.
var webReturnSorts = func() map[string]int {
	m := map[string]int{}
	for i, h := range ScoutWebHorizons {
		m["return_"+h] = i
	}
	return m
}()

// Positions of a window's three numbers in ScoutWebRow.Perf.
const (
	webPerfReturn = iota
	webPerfPeak
	webPerfDrawdown
	webPerfPerHorizon
	webPerfPerRow = len(ScoutWebHorizons) * webPerfPerHorizon
)

var _ [webPerfPerRow]float64 = ScoutWebRow{}.Perf // the two sizes must agree

func webHorizonIndex(h string) (int, bool) {
	for i, name := range ScoutWebHorizons {
		if name == h {
			return i, true
		}
	}
	return 0, false
}

// webSnapshot is the website's data at one moment, laid out for answering
// requests: a few large blocks without pointers (cheap for the garbage
// collector, compact in memory) instead of one object per row. Row i of every
// block is the token with the i-th lowest first-call id.
type webSnapshot struct {
	n   int
	ids []int32 // call id
	// flags: the Perceptor filter bucket (webBucket…) and webFlagUSD
	flags []uint8
	// perf[i*15+j] is ScoutWebRow.Perf[j] of row i (USD-priced rows only); bit j
	// of hasPerf[i] says the value is there.
	perf    []float64
	hasPerf []uint16
	// rowJSON holds every row as it is sent, already encoded, without the three
	// numbers of the window: row i is rowJSON[rowAt[i]:rowAt[i+1]], and the
	// numbers go in at rowCut[i].
	rowJSON []byte
	rowAt   []uint32
	rowCut  []uint32
	// search holds, per row, the lower-cased name, symbol and address, each
	// followed by a zero byte (which no search text contains); row i is
	// search[searchAt[i]:searchAt[i+1]].
	search   []byte
	searchAt []uint32
	// order[k] lists the rows sorted by key k, descending, the way the list is
	// shown: rows with a value first (highest first, ties by the higher call
	// id), then the rows without one (higher call id first). nonNull[k] is the
	// number of rows with a value. The ascending order is each of the two parts
	// read backwards, so it needs no list of its own.
	order   [webSortKeys][]int32
	nonNull [webSortKeys]int
	// counts[u][b]: rows in filter bucket b (webBuckets = all); u = 1 counts
	// the USD-priced rows only.
	counts  [2][webBuckets + 1]int
	summary ScoutWebSummary // UpdatedAt and SnapshotAgeSeconds are filled in per answer
	// summaryTag is a hash of the counts alone: /api/summary stays "not
	// modified" while only rows change.
	summaryTag string
	// version is a hash of the content (and of the settings that shape a row):
	// two snapshots with the same rows and counts have the same version,
	// whenever they were loaded.
	version  string
	loadedAt time.Time
	took     time.Duration
	// answers already encoded for this snapshot (nil = none are kept); the one
	// part of a snapshot that still changes after it is put in place
	answers *webAnswers
	// percID[i] and salphaID[i]: the investigations the detail of row i shows
	// (0 = none); reports holds their texts by investigation id, exactly the
	// ids some row refers to (set by webServer.readSnapshot, never changed
	// after the snapshot is in place; the entries are shared with the snapshot
	// before, which is how a refresh reads only the texts it does not have).
	percID   []int32
	salphaID []int32
	reports  map[int]*webReport
}

// webReportMaxBytes: the most of one text (report, summary, label) the
// website keeps; a longer one is cut there (at a character boundary) and
// marked as cut. sAlpha's reports are about 33 characters on average (1,035 at
// most) and Perceptor's summaries are a line, so in practice nothing is cut.
const webReportMaxBytes = 32 << 10

// webReportMaxURL: a longer report link is not kept.
const webReportMaxURL = 2048

// webReport is one report as the row detail (GET /api/call) shows it: an
// investigation, its texts already cut to size and its link kept only when it
// is https. It never changes once made: an investigation is written once.
type webReport struct {
	id        int
	tool      string
	at        time.Time
	verdict   string  // Perceptor: clean | caution | red_flags | unknown
	label     *string // Perceptor: verdict_label
	summary   *string // Perceptor: verdict_summary
	text      string  // sAlpha: report_text
	url       *string
	truncated bool // a text was longer than webReportMaxBytes and was cut
}

// cutText returns s cut to at most max bytes, at a character boundary, and
// whether it was cut.
func cutText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

// newWebReport keeps what the detail shows of an investigation read from the
// database: the texts of its tool, cut to webReportMaxBytes, and an https link.
func newWebReport(r *ScoutWebReport) *webReport {
	w := &webReport{id: r.ID, tool: r.Tool, at: r.At.UTC()}
	cut := func(p *string) *string {
		if p == nil {
			return nil
		}
		v, c := cutText(*p, webReportMaxBytes)
		w.truncated = w.truncated || c
		return &v
	}
	switch r.Tool {
	case webToolPerceptor:
		w.verdict = r.Verdict
		if _, ok := webVerdictBuckets[w.verdict]; !ok || w.verdict == "not_scanned" {
			w.verdict = levelUnknown
		}
		w.label, w.summary = cut(r.Label), cut(r.Summary)
	case webToolSAlpha:
		w.text, w.truncated = cutText(r.Text, webReportMaxBytes)
	}
	if r.URL != nil && strings.HasPrefix(*r.URL, "https://") && len(*r.URL) <= webReportMaxURL {
		u := *r.URL
		w.url = &u
	}
	return w
}

// The tool codes whose reports the row detail shows.
const (
	webToolPerceptor = "perceptor"
	webToolSAlpha    = "salpha"
)

// webMissingReports returns the report ids rows refer to that have is
// missing, each once, in the order met.
func webMissingReports(rows []ScoutWebRow, have map[int]*webReport) []int {
	var out []int
	seen := map[int]bool{}
	for i := range rows {
		for _, p := range [2]*int{rows[i].PerceptorID, rows[i].SAlphaID} {
			if p != nil && have[*p] == nil && !seen[*p] {
				seen[*p] = true
				out = append(out, *p)
			}
		}
	}
	return out
}

// webReportsFor makes the text map of a new snapshot: for every report id
// rows refer to, the entry of old (the snapshot before) or, for an id not in
// old, the one just read (got). Entries no row refers to any more are not
// taken over. A row whose report could not be read (not in old nor in got, or
// of another tool) loses that id: the next refresh asks for it again. old is
// never changed: requests may still read it.
func webReportsFor(rows []ScoutWebRow, old map[int]*webReport, got map[int]*ScoutWebReport) map[int]*webReport {
	m := make(map[int]*webReport)
	keep := func(p *int, tool string) *int {
		if p == nil {
			return nil
		}
		if e := m[*p]; e != nil {
			return p
		}
		if e := old[*p]; e != nil && e.tool == tool {
			m[*p] = e
			return p
		}
		if g := got[*p]; g != nil && g.ID == *p && g.Tool == tool {
			m[*p] = newWebReport(g)
			return p
		}
		return nil
	}
	for i := range rows {
		r := &rows[i]
		r.PerceptorID = keep(r.PerceptorID, webToolPerceptor)
		r.SAlphaID = keep(r.SAlphaID, webToolSAlpha)
	}
	return m
}

const webFlagUSD = 0x80 // in webSnapshot.flags, next to the bucket

// finite drops values JSON cannot carry (NaN, ±Inf).
func finite(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	return p
}

// positive returns p when it is a finite number above zero, else nil.
func positive(p *float64) *float64 {
	if p = finite(p); p == nil || *p <= 0 {
		return nil
	}
	return p
}

// firstPositive returns the first of a and b that is a finite number above
// zero, else nil.
func firstPositive(a, b *float64) *float64 {
	if a = positive(a); a != nil {
		return a
	}
	return positive(b)
}

// setWebMcaps works out the two market caps of a prepared row (r.usd and
// r.LatestPrice already follow the website's rules) into r.callMcap and
// r.latestMcap:
//
//	call   = called_at_mcap_usd, else mcap_usd
//	latest = (mcap_usd, else called_at_mcap_usd) × latest price ÷ price at the post
//
// "else" picks the second when the first is not a valid figure: missing, zero,
// negative or not finite. The result is nil when neither figure is valid, when
// the latest price or the price at the post is not a finite number above zero,
// when the result is not, and for calls not priced in USD. The values are kept
// in the row itself (no allocation per row).
func setWebMcaps(r *ScoutWebRow) {
	r.callMcap, r.latestMcap = nil, nil
	if !r.usd {
		return
	}
	if call := firstPositive(r.CalledAtMcap, r.PostMcap); call != nil {
		r.mcapVals[0] = *call
		r.callMcap = &r.mcapVals[0]
	}
	base := firstPositive(r.PostMcap, r.CalledAtMcap)
	if base != nil && positive(r.LatestPrice) != nil && positive(r.PostPrice) != nil {
		if v := *base * *r.LatestPrice / *r.PostPrice; v > 0 && !math.IsInf(v, 0) { // > 0 is false for NaN
			r.mcapVals[1] = v
			r.latestMcap = &r.mcapVals[1]
		}
	}
}

// webCall is the row as sent to the page, without the numbers of the window
// asked for (those are added per request, see appendRow). The return of every
// window is in it (Return1hPct … Return30dPct). r is a prepared row: its Perf
// holds nothing for calls not priced in USD.
func (c webConfig) webCall(r *ScoutWebRow) ScoutWebCall {
	var latestAge *int64
	if r.LatestAt != nil {
		age := int64(r.LatestAt.Sub(r.MessageDate) / time.Second)
		latestAge = &age
	}
	ret := func(h int) *float64 {
		j := h*webPerfPerHorizon + webPerfReturn
		if !r.usd || r.HasPerf&(1<<j) == 0 {
			return nil
		}
		return finite(&r.Perf[j])
	}
	return ScoutWebCall{
		Return1hPct: ret(0), Return1dPct: ret(1), Return3dPct: ret(2), Return7dPct: ret(3), Return30dPct: ret(4),
		LatestReturnPct: r.LatestReturn, LatestPriceUSD: r.LatestPrice,
		LatestAt: r.LatestAt, LatestTradeAt: r.LatestTradeAt, LatestAgeSeconds: latestAge,
		CallID: r.CallID, MessageID: r.MessageID, MessageDate: r.MessageDate,
		PostURL:         postURL(r.ChannelUsername, r.MessageID),
		ContractAddress: r.ContractAddress, TokenName: r.TokenName, TokenSymbol: r.TokenSymbol,
		GMGNURL:   c.gmgnURL(r.ContractAddress),
		PriceUnit: r.PriceUnit, EntryPriceUSD: r.EntryPrice,
		Rugged: r.Rugged, TrackingStatus: r.TrackingStatus,
		PerceptorVerd: r.PerceptorVerd, PerceptorURL: r.PerceptorURL,
		CallCount: r.CallCount, LastCallDate: r.LastCallDate,
		CallMcapUSD: r.callMcap, LatestMcapUSD: r.latestMcap,
		HasSAlpha: r.SAlphaID != nil, PerceptorReportID: r.PerceptorID, SAlphaReportID: r.SAlphaID,
	}
}

// webRowNumbersJSON is how the three numbers of a window look in an encoded
// ScoutWebCall that has none; appendRow puts the real ones in its place.
const webRowNumbersJSON = `,"return_pct":null,"peak_pct":null,"drawdown_pct":null,`

// newWebSnapshot builds a snapshot from the rows of ScoutStore.SelectWebRows
// (which it takes over and changes in place). When the content is the same as
// prev's, the blocks already built for prev are used again.
func newWebSnapshot(rows []ScoutWebRow, updatePosts int, prev *webSnapshot, cfg webConfig) (*webSnapshot, error) {
	n := len(rows)
	if n > math.MaxInt32 {
		return nil, fmt.Errorf("%d rows are more than the website can hold", n)
	}
	s := &webSnapshot{n: n}
	sum := &s.summary
	sum.UpdatePosts = updatePosts
	for i := range rows {
		r := &rows[i]
		r.MessageDate, r.LastCallDate = r.MessageDate.UTC(), r.LastCallDate.UTC()
		r.usd = r.PriceUnit != nil && *r.PriceUnit == "usd"

		sum.Imported++
		sum.TotalCalls += r.CallCount
		if r.Tracked {
			sum.Tracked++
			if !r.usd {
				sum.NoUSDPrice++
			}
		}
		if r.TrackingStatus != nil {
			switch *r.TrackingStatus {
			case TrackPending:
				sum.Pending++
			case TrackTracking:
				sum.Tracking++
			case TrackDone:
				sum.Done++
			case TrackNoPool:
				sum.NoPool++
			case TrackError:
				sum.Error++
			case TrackGaveUp:
				sum.GaveUp++
			}
		}

		// Performance is shown in USD only: the numbers of other calls are not
		// there at all (and so sort last).
		if !r.usd {
			r.EntryPrice, r.HasPerf = nil, 0
		}
		r.EntryPrice = finite(r.EntryPrice)
		// The latest price follows the same rule; without the time it was read
		// (or a number JSON can carry) there is none.
		r.LatestReturn, r.LatestPrice = finite(r.LatestReturn), finite(r.LatestPrice)
		if !r.usd || r.LatestAt == nil || r.LatestReturn == nil {
			r.LatestReturn, r.LatestPrice, r.LatestAt, r.LatestTradeAt = nil, nil, nil, nil
		}
		// in UTC like the other times, in place (the rows are ours to change)
		if r.LatestAt != nil {
			*r.LatestAt = r.LatestAt.UTC()
		}
		if r.LatestTradeAt != nil {
			*r.LatestTradeAt = r.LatestTradeAt.UTC()
		}
		// the market caps: after the rules of the latest price above, which they follow
		setWebMcaps(r)
		for j := range r.Perf {
			if r.HasPerf&(1<<j) == 0 {
				r.Perf[j] = 0
			}
		}
		if r.PerceptorURL != nil && !strings.HasPrefix(*r.PerceptorURL, "https://") {
			r.PerceptorURL = nil // the page only links to https addresses
		}
		for _, p := range [2]**int{&r.PerceptorID, &r.SAlphaID} {
			if *p != nil && (**p <= 0 || **p > math.MaxInt32) {
				*p = nil // not an id the snapshot can hold
			}
		}
		r.verdict = webBucketNotScanned
		if r.PerceptorVerd != nil {
			if b, ok := webVerdictBuckets[*r.PerceptorVerd]; ok {
				r.verdict = b
			}
		}
	}
	sum.RepeatCalls = sum.TotalCalls - sum.Imported
	counts := sha256.Sum256([]byte(fmt.Sprint(sum.Imported, sum.Tracked, sum.Pending, sum.Tracking, sum.Done, sum.NoPool,
		sum.Error, sum.GaveUp, sum.NoUSDPrice, sum.TotalCalls, sum.RepeatCalls, sum.UpdatePosts)))
	s.summaryTag = hex.EncodeToString(counts[:12])

	s.version = hashWebRows(rows, updatePosts, cfg.GMGNTemplate)
	if prev != nil && prev.version == s.version {
		// Same content: keep prev's blocks (and let the rows just read go).
		same := *prev
		same.summary, same.summaryTag, same.answers = s.summary, s.summaryTag, nil
		return &same, nil
	}

	s.ids = make([]int32, n)
	s.flags = make([]uint8, n)
	s.hasPerf = make([]uint16, n)
	s.perf = make([]float64, n*webPerfPerRow)
	s.rowAt = make([]uint32, n+1)
	s.rowCut = make([]uint32, n)
	s.searchAt = make([]uint32, n+1)
	s.percID = make([]int32, n)
	s.salphaID = make([]int32, n)
	s.rowJSON = make([]byte, 0, n*940) // about 870 bytes a row
	s.search = make([]byte, 0, n*72)
	var one bytes.Buffer // one encoded row
	enc := json.NewEncoder(&one)
	marker := []byte(webRowNumbersJSON)
	for i := range rows {
		r := &rows[i]
		if r.CallID > math.MaxInt32 || (i > 0 && r.CallID <= rows[i-1].CallID) {
			return nil, fmt.Errorf("call ids out of order or too large at row %d (id %d)", i, r.CallID)
		}
		s.ids[i] = int32(r.CallID)
		s.flags[i] = r.verdict
		s.counts[0][r.verdict]++
		s.counts[0][webBuckets]++
		if r.usd {
			s.flags[i] |= webFlagUSD
			s.counts[1][r.verdict]++
			s.counts[1][webBuckets]++
		}
		s.hasPerf[i] = r.HasPerf
		copy(s.perf[i*webPerfPerRow:], r.Perf[:])
		if r.PerceptorID != nil {
			s.percID[i] = int32(*r.PerceptorID)
		}
		if r.SAlphaID != nil {
			s.salphaID[i] = int32(*r.SAlphaID)
		}

		one.Reset()
		call := cfg.webCall(r)
		if err := enc.Encode(&call); err != nil {
			return nil, fmt.Errorf("call %d: %w", r.CallID, err)
		}
		row := bytes.TrimSuffix(one.Bytes(), []byte("\n"))
		// The place of the numbers. A name or symbol cannot contain this text:
		// in JSON its quotes are written with a backslash.
		cut := bytes.Index(row, marker)
		if cut < 0 || bytes.Contains(row[cut+len(marker):], marker) {
			return nil, fmt.Errorf("call %d: unexpected row encoding", r.CallID)
		}
		s.rowJSON = append(s.rowJSON, row[:cut]...)
		s.rowCut[i] = uint32(len(s.rowJSON))
		s.rowJSON = append(s.rowJSON, row[cut+len(marker):]...)

		if r.TokenName != nil {
			s.search = append(s.search, strings.ToLower(*r.TokenName)...)
		}
		s.search = append(s.search, 0)
		if r.TokenSymbol != nil {
			s.search = append(s.search, strings.ToLower(*r.TokenSymbol)...)
		}
		s.search = append(s.search, 0)
		s.search = append(s.search, strings.ToLower(r.ContractAddress)...)
		s.search = append(s.search, 0)
		if len(s.rowJSON) > math.MaxUint32 || len(s.search) > math.MaxUint32 {
			return nil, errors.New("the list is too large for the website's snapshot")
		}
		s.rowAt[i+1], s.searchAt[i+1] = uint32(len(s.rowJSON)), uint32(len(s.search))
	}
	// give back what the two growing blocks reserved beyond their content
	s.rowJSON, s.search = trimBlock(s.rowJSON), trimBlock(s.search)
	s.buildOrders(rows)
	return s, nil
}

// trimBlock returns b without spare room: as it is when little is spare, else a copy.
func trimBlock(b []byte) []byte {
	if cap(b)-len(b) <= len(b)/4 {
		return slices.Clip(b)
	}
	return bytes.Clone(b)
}

// hashWebRows returns the content version of a prepared row list; gmgn is the
// link template that goes into every row.
func hashWebRows(rows []ScoutWebRow, updatePosts int, gmgn string) string {
	h := sha256.New()
	buf := make([]byte, 0, 1024)
	num := func(v int64) { buf = binary.LittleEndian.AppendUint64(buf, uint64(v)) }
	str := func(s string) {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)))
		buf = append(buf, s...)
	}
	opt := func(p *string) {
		if p == nil {
			buf = append(buf, 0)
			return
		}
		buf = append(buf, 1)
		str(*p)
	}
	optInt := func(p *int) {
		if p == nil {
			buf = append(buf, 0)
			return
		}
		buf = append(buf, 1)
		num(int64(*p))
	}
	flt := func(v float64) { buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v)) }
	optFlt := func(p *float64) {
		if p == nil {
			buf = append(buf, 0)
			return
		}
		buf = append(buf, 1)
		flt(*p)
	}
	optTime := func(p *time.Time) {
		if p == nil {
			buf = append(buf, 0)
			return
		}
		buf = append(buf, 1)
		num(p.UnixNano())
	}
	num(int64(len(rows)))
	num(int64(updatePosts))
	str(gmgn)
	h.Write(buf)
	for i := range rows {
		r := &rows[i]
		buf = buf[:0]
		num(int64(r.CallID))
		num(int64(r.MessageID))
		num(r.MessageDate.UnixNano())
		str(r.ChannelUsername)
		str(r.ContractAddress)
		opt(r.TokenName)
		opt(r.TokenSymbol)
		opt(r.PriceUnit)
		optFlt(r.EntryPrice)
		flags := byte(0)
		if r.Tracked {
			flags |= 1
		}
		if r.Rugged != nil {
			flags |= 2
			if *r.Rugged {
				flags |= 4
			}
		}
		buf = append(buf, flags)
		opt(r.TrackingStatus)
		buf = binary.LittleEndian.AppendUint16(buf, r.HasPerf)
		for _, v := range r.Perf {
			flt(v)
		}
		opt(r.PerceptorVerd)
		opt(r.PerceptorURL)
		num(int64(r.CallCount))
		num(r.LastCallDate.UnixNano())
		optFlt(r.LatestReturn)
		optFlt(r.LatestPrice)
		optTime(r.LatestAt)
		optTime(r.LatestTradeAt)
		// the market caps as shown (worked out by newWebSnapshot), not their inputs
		optFlt(r.callMcap)
		optFlt(r.latestMcap)
		// the reports the detail shows (a new one changes the version, an
		// empty sAlpha reply is never chosen, so it does not)
		optInt(r.PerceptorID)
		optInt(r.SAlphaID)
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// pgFloatDesc orders two float8 values the way PostgreSQL's ORDER BY … DESC
// does: NaN counts as larger than every number, and equal to itself.
func pgFloatDesc(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return -1
	case bn:
		return 1
	}
	return cmp.Compare(b, a)
}

// buildOrders fills order and nonNull. Rows are in call id order, so a higher
// position is a higher call id.
func (s *webSnapshot) buildOrders(rows []ScoutWebRow) {
	n := s.n
	dates := make([]int64, n)
	byDate := make([]int32, n)
	for i := range rows {
		dates[i] = rows[i].MessageDate.UnixNano()
		byDate[i] = int32(i)
	}
	slices.SortFunc(byDate, func(a, b int32) int {
		if c := cmp.Compare(dates[b], dates[a]); c != 0 {
			return c
		}
		return cmp.Compare(b, a)
	})
	s.order[webSortKeyDate], s.nonNull[webSortKeyDate] = byDate, n

	const per = webPerfPerRow
	// byValue: the rows with a value, highest first (ties: higher call id
	// first), then the rows without one, higher call id first.
	byValue := func(key int, has func(i int) bool, val func(i int32) float64) {
		ord := make([]int32, 0, n)
		for i := 0; i < n; i++ {
			if has(i) {
				ord = append(ord, int32(i))
			}
		}
		slices.SortFunc(ord, func(a, b int32) int {
			if c := pgFloatDesc(val(a), val(b)); c != 0 {
				return c
			}
			return cmp.Compare(b, a)
		})
		s.nonNull[key] = len(ord)
		for i := n - 1; i >= 0; i-- {
			if !has(i) {
				ord = append(ord, int32(i))
			}
		}
		s.order[key] = ord
	}
	for key := webSortKeyReturn; key < webSortKeyLatest; key++ {
		pi := (key - webSortKeyReturn) * webPerfPerHorizon // the window's return
		if key >= webSortKeyPeak {
			pi = (key-webSortKeyPeak)*webPerfPerHorizon + webPerfPeak
		}
		byValue(key, func(i int) bool { return s.hasPerf[i]&(1<<pi) != 0 },
			func(i int32) float64 { return s.perf[int(i)*per+pi] })
	}
	// the return as of the latest price (needed here only: the page gets it
	// from the encoded row)
	byValue(webSortKeyLatest, func(i int) bool { return rows[i].LatestReturn != nil },
		func(i int32) float64 { return *rows[i].LatestReturn })
	// the two market caps (nil unless shown)
	byValue(webSortKeyCallMC, func(i int) bool { return rows[i].callMcap != nil },
		func(i int32) float64 { return *rows[i].callMcap })
	byValue(webSortKeyLatestMC, func(i int) bool { return rows[i].latestMcap != nil },
		func(i int32) float64 { return *rows[i].latestMcap })
}

// webBitsPool: the "matches the search text" marks of one request.
var webBitsPool = sync.Pool{New: func() any { return new([]uint64) }}

// markMatches sets bit i of bits for every row i whose name, symbol or address
// contains q (lower-cased, not empty, without a zero byte).
func (s *webSnapshot) markMatches(q string, bits []uint64) {
	needle := []byte(q)
	row, from := 0, 0
	for from < len(s.search) {
		i := bytes.Index(s.search[from:], needle)
		if i < 0 {
			return
		}
		at := uint32(from + i)
		for s.searchAt[row+1] <= at {
			row++
		}
		bits[row>>6] |= 1 << (row & 63)
		row++ // one match is enough: on to the next row
		from = int(s.searchAt[row])
	}
}

// page appends to dst the rows (positions) of one page of the list, in order,
// and returns them with the number of rows that match the filter. The list
// holds each token's first call only, so search, sort, paging and the total all
// work on one row per token.
func (s *webSnapshot) page(f ScoutWebCallsFilter, dst []int32) ([]int32, int, error) {
	h, ok := webHorizonIndex(f.Horizon)
	if !ok {
		return dst, 0, fmt.Errorf("unknown horizon %q", f.Horizon)
	}
	var key int
	switch f.Sort {
	case "date":
		key = webSortKeyDate
	case "return":
		key = webSortKeyReturn + h
	case "peak":
		key = webSortKeyPeak + h
	case "latest":
		key = webSortKeyLatest // the same for every window
	case "call_mc":
		key = webSortKeyCallMC // also not tied to the window
	case "latest_mc":
		key = webSortKeyLatestMC
	default:
		w, ok := webReturnSorts[f.Sort] // return_1h … return_30d: one window, whatever the horizon
		if !ok {
			return dst, 0, fmt.Errorf("unknown sort %q", f.Sort)
		}
		key = webSortKeyReturn + w
	}
	if f.Dir != "desc" && f.Dir != "asc" {
		return dst, 0, fmt.Errorf("unknown direction %q", f.Dir)
	}
	if f.Page < 1 || f.Per < 1 {
		return dst, 0, errors.New("page and per must be at least 1")
	}
	bucket := webBuckets // all
	if f.Verdict != "" {
		b, ok := webVerdictBuckets[f.Verdict]
		if !ok {
			return dst, 0, fmt.Errorf("unknown verdict %q", f.Verdict)
		}
		bucket = b
	}
	q := strings.ToLower(f.Q)
	if strings.IndexByte(q, 0) >= 0 {
		return dst, 0, errors.New("the search text is not valid")
	}

	order, nn, n := s.order[key], s.nonNull[key], s.n
	asc := f.Dir == "asc"
	skip := (f.Page - 1) * f.Per
	base := len(dst)
	// which flags a row must have: mask picks the bits that count, want their values
	var mask, want uint8
	if f.USDOnly {
		mask, want = webFlagUSD, webFlagUSD
	}
	if bucket != webBuckets {
		mask, want = mask|^uint8(webFlagUSD), want|bucket
	}

	total, known := 0, false
	var bits []uint64
	if q == "" {
		// without a search text the total is already counted: stop at the end of the page
		u := 0
		if f.USDOnly {
			u = 1
		}
		total, known = s.counts[u][bucket], true
		if skip >= total {
			return dst, total, nil
		}
	} else {
		bp := webBitsPool.Get().(*[]uint64)
		defer webBitsPool.Put(bp)
		words := (n + 63) / 64
		if cap(*bp) < words {
			*bp = make([]uint64, words)
		}
		bits = (*bp)[:words]
		clear(bits)
		s.markMatches(q, bits)
	}
	start, matched := 0, 0
	if known && mask == 0 { // every row matches: jump to the page
		start, matched = skip, skip
	}
	for i := start; i < n; i++ {
		pos := order[i]
		if asc {
			if i < nn {
				pos = order[nn-1-i]
			} else {
				pos = order[n-1-(i-nn)]
			}
		}
		if s.flags[pos]&mask != want {
			continue
		}
		if bits != nil && bits[pos>>6]&(1<<(pos&63)) == 0 {
			continue
		}
		if matched >= skip && len(dst)-base < f.Per {
			dst = append(dst, pos)
		}
		matched++
		if known && len(dst)-base == f.Per {
			break
		}
	}
	if !known {
		total = matched
	}
	return dst, total, nil
}

// appendJSONFloat appends v the way encoding/json writes a float64.
func appendJSONFloat(b []byte, v float64) []byte {
	format := byte('f')
	if abs := math.Abs(v); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, v, format, -1, 64)
	if format == 'e' {
		// e-09 → e-9, as encoding/json does
		if n := len(b); n >= 4 && b[n-4] == 'e' && (b[n-3] == '-' || b[n-3] == '+') && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

// appendRow appends row pos as JSON (a ScoutWebCall) with the numbers of
// window h; a number that is missing, or that JSON cannot carry, is null.
func (s *webSnapshot) appendRow(b []byte, pos int32, h int) []byte {
	const per = webPerfPerRow
	num := func(b []byte, k int) []byte {
		j := h*webPerfPerHorizon + k
		v := s.perf[int(pos)*per+j]
		if s.hasPerf[pos]&(1<<j) == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return append(b, "null"...)
		}
		return appendJSONFloat(b, v)
	}
	b = append(b, s.rowJSON[s.rowAt[pos]:s.rowCut[pos]]...)
	b = num(append(b, `,"return_pct":`...), webPerfReturn)
	b = num(append(b, `,"peak_pct":`...), webPerfPeak)
	b = num(append(b, `,"drawdown_pct":`...), webPerfDrawdown)
	b = append(b, ',')
	return append(b, s.rowJSON[s.rowCut[pos]:s.rowAt[pos+1]]...)
}
