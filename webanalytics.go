package main

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Analytics page (frontend/analytics.html), section "Call performance": one
// compact row per first call, built once per snapshot and served as is by GET
// /api/analytics. The page groups and filters the rows itself (by week or
// month, Perceptor verdict at the call, pool family, posted DEX, quote asset),
// so no request does more than send these bytes.
// ---------------------------------------------------------------------------

const (
	// webAnalyticsFormat: the shape of the body (field "format"); raise it
	// when the columns change, so the page can tell.
	webAnalyticsFormat = 1
	// webQuietTrades: a call with fewer swaps than this in the 24 hours after
	// it counts as "quiet after the call" on the Analytics page. For Uniswap v2
	// pools the tracker counts Sync events, which liquidity changes also emit.
	webQuietTrades = 50
	// webTradesFullEvery: how often the website reads the trades of the first
	// 24 hours of every call again (in between only those of calls whose 24
	// hours were stored since: a stored count does not change, unless a call
	// is tracked again from the start).
	webTradesFullEvery = time.Hour
	// webAnalyticsMaxName: a posted DEX or quote asset name is cut to this
	// many characters.
	webAnalyticsMaxName = 60
)

// webAnalyticsColumns names the values of each row of GET /api/analytics, in
// order; then come, per window of ScoutWebHorizons, its return, peak and worst
// drop (late entry, USD, percent; null when missing).
var webAnalyticsColumns = func() []string {
	cols := []string{"call_id", "t", "flags", "verdict", "family", "dex", "quote", "trades_24h", "no_data"}
	for _, h := range ScoutWebHorizons {
		cols = append(cols, "ret_"+h, "peak_"+h, "dd_"+h)
	}
	return cols
}()

// Bits of the "flags" column.
const (
	webAnaFlagUSD     = 1 // priced in USD: only these rows have numbers
	webAnaFlagRugged  = 2 // flagged rugged by the tracker (as of now)
	webAnaFlagTracked = 4 // has an entry price
)

// webAnalyticsVerdicts: the values of the "verdict" column (an index into
// this list): the Perceptor verdict at the time of the call; unknown = a
// report whose verdict could not be read, none = no live report at the time.
var webAnalyticsVerdicts = []string{levelClean, levelCaution, levelRedFlags, levelUnknown, "none"}

// webHorizonSeconds: the length of each window of ScoutWebHorizons.
var webHorizonSeconds = [len(ScoutWebHorizons)]int{3600, 86400, 3 * 86400, 7 * 86400, 30 * 86400}

// webAnalyticsBody is the body of GET /api/analytics. Families, Dexes and
// Quotes are dictionaries, most frequent first; a row holds an index into
// them (-1 = none). Rows: one array per first call, values as in Columns.
type webAnalyticsBody struct {
	Format         int             `json:"format"`
	Horizons       []string        `json:"horizons"`
	HorizonSeconds []int           `json:"horizon_seconds"`
	QuietBelow     int             `json:"quiet_below"`
	Verdicts       []string        `json:"verdicts"`
	Families       []string        `json:"families"`
	Dexes          []string        `json:"dexes"`
	Quotes         []string        `json:"quotes"`
	Columns        []string        `json:"columns"`
	Rows           json.RawMessage `json:"rows"`
}

// webAnalytics is the encoded body of GET /api/analytics for one snapshot.
// It is never changed once built.
type webAnalytics struct {
	plain []byte
	gz    []byte // compressed; nil when that is not smaller
	etag  string // weak; a hash of plain
	rows  int
}

// webPoolFamily is the stable "DEX family" of a call: the kind of pool the
// tracker priced it from (v2, v3, v4, pons, …, from entry_price_source
// onchain-<kind>), "gecko" for calls priced from GeckoTerminal candles
// (minute | hour) and "untracked" without an entry price source.
func webPoolFamily(src *string) string {
	if src == nil || *src == "" {
		return "untracked"
	}
	if k, ok := strings.CutPrefix(*src, "onchain-"); ok && k != "" {
		return webAnalyticsName(k)
	}
	return "gecko"
}

// webAnalyticsName cleans a name from the post or the chain for the page:
// valid UTF-8, no control characters, spaces trimmed, at most
// webAnalyticsMaxName characters ("" = none).
func webAnalyticsName(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > webAnalyticsMaxName {
		s = string([]rune(s)[:webAnalyticsMaxName])
	}
	return s
}

// webDict numbers the names of one column, most frequent first (ties by name).
type webDict struct {
	count map[string]int
	index map[string]int
	names []string
}

func newWebDict() *webDict { return &webDict{count: map[string]int{}} }

func (d *webDict) add(name string) {
	if name != "" {
		d.count[name]++
	}
}

// freeze fixes the order; index works only after it.
func (d *webDict) freeze() {
	d.names = make([]string, 0, len(d.count))
	for n := range d.count {
		d.names = append(d.names, n)
	}
	slices.SortFunc(d.names, func(a, b string) int {
		if c := cmp.Compare(d.count[b], d.count[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	d.index = make(map[string]int, len(d.names))
	for i, n := range d.names {
		d.index[n] = i
	}
}

func (d *webDict) at(name string) int {
	if i, ok := d.index[name]; ok {
		return i
	}
	return -1
}

// appendAnaNum appends a percentage for the page: rounded to 0.1 (enough for
// medians and rates), in exponent form when very large; null when JSON cannot
// carry it.
func appendAnaNum(b []byte, v float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	if math.Abs(v) >= 1e9 {
		return strconv.AppendFloat(b, v, 'g', 6, 64)
	}
	v = math.Round(v*10) / 10
	if v == 0 {
		v = 0 // no "-0"
	}
	return strconv.AppendFloat(b, v, 'f', -1, 64)
}

// webAnalyticsVerdictIndex: the "verdict" column of a row.
func webAnalyticsVerdictIndex(v *string) int {
	if v == nil {
		return len(webAnalyticsVerdicts) - 1 // none
	}
	switch *v {
	case levelClean:
		return 0
	case levelCaution:
		return 1
	case levelRedFlags:
		return 2
	}
	return 3 // unknown
}

// buildWebAnalytics encodes the body of GET /api/analytics from the prepared
// rows of a new snapshot (newWebSnapshot). When the bytes come out the same
// as prev's, prev is returned (its compressed form is not made again, and its
// ETag stays).
func buildWebAnalytics(rows []ScoutWebRow, prev *webAnalytics) (*webAnalytics, error) {
	fams, dexes, quotes := newWebDict(), newWebDict(), newWebDict()
	for i := range rows {
		r := &rows[i]
		fams.add(webPoolFamily(r.EntrySource))
		if r.PostedDex != nil {
			dexes.add(webAnalyticsName(*r.PostedDex))
		}
		if r.QuoteSym != nil {
			quotes.add(webAnalyticsName(*r.QuoteSym))
		}
	}
	fams.freeze()
	dexes.freeze()
	quotes.freeze()

	out := make([]byte, 0, 64+len(rows)*130)
	out = append(out, '[')
	for i := range rows {
		r := &rows[i]
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '[')
		out = strconv.AppendInt(out, int64(r.CallID), 10)
		out = append(out, ',')
		out = strconv.AppendInt(out, r.MessageDate.Unix(), 10)
		flags := 0
		if r.usd {
			flags |= webAnaFlagUSD
		}
		if r.Rugged != nil && *r.Rugged {
			flags |= webAnaFlagRugged
		}
		if r.Tracked {
			flags |= webAnaFlagTracked
		}
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(flags), 10)
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(webAnalyticsVerdictIndex(r.VerdictAtCall)), 10)
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(fams.at(webPoolFamily(r.EntrySource))), 10)
		dex, quote := -1, -1
		if r.PostedDex != nil {
			dex = dexes.at(webAnalyticsName(*r.PostedDex))
		}
		if r.QuoteSym != nil {
			quote = quotes.at(webAnalyticsName(*r.QuoteSym))
		}
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(dex), 10)
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(quote), 10)
		out = append(out, ',')
		if r.TradesFinal && r.Trades24h != nil {
			out = strconv.AppendInt(out, int64(*r.Trades24h), 10)
		} else {
			out = append(out, "null"...)
		}
		out = append(out, ',')
		out = strconv.AppendInt(out, int64(r.NoData), 10)
		for j := range webPerfPerRow {
			out = append(out, ',')
			if r.anaHas&(1<<j) == 0 {
				out = append(out, "null"...)
				continue
			}
			out = appendAnaNum(out, r.anaPerf[j])
		}
		out = append(out, ']')
	}
	out = append(out, ']')

	body := webAnalyticsBody{
		Format: webAnalyticsFormat, Horizons: ScoutWebHorizons[:], HorizonSeconds: webHorizonSeconds[:],
		QuietBelow: webQuietTrades, Verdicts: webAnalyticsVerdicts,
		Families: fams.names, Dexes: dexes.names, Quotes: quotes.names,
		Columns: webAnalyticsColumns, Rows: out,
	}
	var buf bytes.Buffer
	buf.Grow(len(out) + 4096)
	if err := json.NewEncoder(&buf).Encode(&body); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	plain := buf.Bytes()
	sum := sha256.Sum256(plain)
	etag := `W/"` + hex.EncodeToString(sum[:12]) + `-a` + strconv.Itoa(webAnalyticsFormat) + `"`
	if prev != nil && prev.etag == etag && bytes.Equal(prev.plain, plain) {
		return prev, nil
	}
	a := &webAnalytics{plain: plain, etag: etag, rows: len(rows)}
	var zb bytes.Buffer
	zw, err := gzip.NewWriterLevel(&zb, gzip.DefaultCompression) // once per change of the content
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if _, err := zw.Write(plain); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if zb.Len() < len(plain) {
		a.gz = zb.Bytes()
	}
	return a, nil
}

// handleAnalytics is GET /api/analytics: the rows of the Analytics page, as
// built with the snapshot (already encoded and compressed). ETag (weak, a hash
// of the body) and If-None-Match → 304 like the other answers.
func (s *webServer) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) > 0 {
		writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
		return
	}
	snap := s.snapshot(w)
	if snap == nil {
		return
	}
	a := snap.analytics
	if a == nil {
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusServiceUnavailable, "starting: the data is not loaded yet")
		return
	}
	s.snapshotHeaders(w, snap, a.etag)
	if s.notModified(w, r, a.etag) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Add("Vary", "Accept-Encoding")
	body := a.plain
	if a.gz != nil && acceptsGzip(r) {
		body = a.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) // the client went away: nothing to do
}

// fillTrades24h sets Trades24h of the rows just read: for every row whose
// first 24 hours of candles are stored (TradesFinal), the swaps in them, from
// the website's own copy; only calls it does not hold yet are read from the
// database (none in most refreshes), and all of them again every
// webTradesFullEvery (a call tracked again from the start gets new candles).
// A call without candles counts 0. Without readTrades (no database) every
// count stays unknown. Only readSnapshot calls it (under refreshMu).
//
// A failed read does not fail the refresh: it is logged (at most once per
// webErrorLogEvery, not when the website is stopping), the copy and tradesAt
// stay as they were, the rows get the counts the copy holds (the others stay
// unknown), and the next refresh asks again.
func (s *webServer) fillTrades24h(ctx context.Context, rows []ScoutWebRow, now time.Time) {
	if s.readTrades == nil {
		return
	}
	full := s.trades == nil || now.Sub(s.tradesAt) >= webTradesFullEvery
	var ids []int
	for i := range rows {
		if !rows[i].TradesFinal {
			continue
		}
		if _, ok := s.trades[rows[i].CallID]; full || !ok {
			ids = append(ids, rows[i].CallID)
		}
	}
	if len(ids) > 0 {
		got, err := s.readTrades(ctx, ids)
		if err != nil {
			if !errors.Is(ctx.Err(), context.Canceled) && s.tradesErrLog.allow(now) {
				log.Printf("web: could not read the trade counts: %v — showing the %d counts read before, the others as unknown", err, len(s.trades))
			}
		} else {
			m := s.trades
			if full {
				m = make(map[int]int, len(ids))
			}
			for _, id := range ids {
				m[id] = got[id] // 0 when the call has no candles in its first 24 hours
			}
			s.trades = m
			if full {
				s.tradesAt = now
			}
		}
	} else if full {
		s.trades, s.tradesAt = map[int]int{}, now
	}
	m := s.trades // nil until a read succeeds: every count unknown
	for i := range rows {
		r := &rows[i]
		r.Trades24h = nil
		if !r.TradesFinal {
			continue
		}
		if v, ok := m[r.CallID]; ok {
			r.Trades24h = &v
		}
	}
}
