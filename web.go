package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Read-only website (-web): a small JSON API plus the static page in
// frontend/. No Telegram, no login, no writes. The API answers from a snapshot
// of the list held in memory (websnapshot.go), read again from the database
// every SCOUT_WEB_REFRESH; a request never waits for the database.
// ---------------------------------------------------------------------------

//go:embed frontend
var frontendFS embed.FS

const (
	defaultWebAddr      = ":8090"
	defaultGMGNTemplate = "https://gmgn.ai/robinhood/token/{ca}"
	webMaxQueryLen      = 100
	webDefaultPer       = 50
	webMaxPer           = 200
	webMaxPage          = 1000000
	defaultWebRefresh   = 15 * time.Second
	minWebRefresh       = 2 * time.Second
	webSnapshotTimeout  = 60 * time.Second // one read of the database
	// "Refresh now" (POST /api/refresh): at most one read of the database it
	// starts per webManualRefreshEvery, counted from the end of the last one; a
	// request waits at most webManualRefreshWait for the read (which goes on).
	webManualRefreshEvery = 5 * time.Second
	webManualRefreshWait  = 15 * time.Second
	webSlowSnapshot       = time.Second // a slower read is logged
	webErrorLogEvery      = time.Minute // at most one "refresh failed" line per this
	webGzipMinBytes       = 1024        // smaller answers are sent as they are
	webAnswersMax         = 200         // answers of /api/calls kept per snapshot …
	webAnswersMaxBytes    = 8 << 20     // … and their size together, uncompressed
	webDiskGzipMaxBytes   = 4 << 20     // SCOUT_WEB_DIR: larger files are not compressed
)

type webConfig struct {
	Addr         string // SCOUT_WEB_ADDR
	GMGNTemplate string // SCOUT_GMGN_URL, with {ca} for the contract address
	Dir          string // SCOUT_WEB_DIR: serve the page from this folder instead of the embedded copy
	// SCOUT_WEB_REFRESH: how often the list is read again from the database
	// (0 = the default)
	Refresh time.Duration
}

// parseWebRefresh reads SCOUT_WEB_REFRESH: a duration, at least minWebRefresh.
// The second value is a warning to log ("" = none).
func parseWebRefresh(v string) (time.Duration, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultWebRefresh, ""
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultWebRefresh, fmt.Sprintf("SCOUT_WEB_REFRESH=%q is not a duration (for example 15s or 1m); using %s", v, defaultWebRefresh)
	}
	if d < minWebRefresh {
		return minWebRefresh, fmt.Sprintf("SCOUT_WEB_REFRESH=%s is below the minimum; using %s", v, minWebRefresh)
	}
	return d, ""
}

func loadWebConfig() webConfig {
	refresh, warn := parseWebRefresh(os.Getenv("SCOUT_WEB_REFRESH"))
	if warn != "" {
		log.Printf("web: warning: %s", warn)
	}
	return webConfig{
		Addr:         env("SCOUT_WEB_ADDR", defaultWebAddr),
		GMGNTemplate: env("SCOUT_GMGN_URL", defaultGMGNTemplate),
		Dir:          env("SCOUT_WEB_DIR", ""),
		Refresh:      refresh,
	}
}

// webStaticFS returns the folder the page is served from.
func (c webConfig) staticFS() (fs.FS, string, error) {
	if c.Dir != "" {
		info, err := os.Stat(c.Dir)
		if err != nil {
			return nil, "", fmt.Errorf("SCOUT_WEB_DIR: %w", err)
		}
		if !info.IsDir() {
			return nil, "", fmt.Errorf("SCOUT_WEB_DIR %q is not a folder", c.Dir)
		}
		return os.DirFS(c.Dir), "folder " + c.Dir, nil
	}
	sub, err := fs.Sub(frontendFS, "frontend")
	return sub, "built-in page", err
}

// gmgnURL builds the GMGN link for a contract address; nil unless the address
// is a plain EVM address (nothing else is ever put into the URL).
func (c webConfig) gmgnURL(ca string) *string {
	if !evmAddrRe.MatchString(ca) {
		return nil
	}
	u := strings.ReplaceAll(c.GMGNTemplate, "{ca}", ca)
	return &u
}

var tgUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// postURL is the link to the call post in the channel.
func postURL(channel string, messageID int) *string {
	channel = strings.TrimPrefix(channel, "@")
	if !tgUsernameRe.MatchString(channel) || messageID <= 0 {
		return nil
	}
	u := fmt.Sprintf("https://t.me/%s/%d", channel, messageID)
	return &u
}

// webSecurityHeaders: sent with every response. The page uses no inline script
// or style and loads nothing from other sites.
func webSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// webBufPool and webGzipPool: buffers and compressors used again between requests.
var (
	webBufPool  = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	webGzipPool = sync.Pool{New: func() any {
		zw, _ := gzip.NewWriterLevel(io.Discard, webGzipLevel)
		return zw
	}}
)

// webGzipLevel: the fastest level. A page of the list comes out about a sixth
// larger than at the default level (5.2 KB instead of 4.4 KB, from 30 KB), in
// a fraction of the time.
const webGzipLevel = gzip.BestSpeed

func putWebBuf(b *bytes.Buffer) {
	if b.Cap() <= 1<<20 {
		b.Reset()
		webBufPool.Put(b)
	}
}

// acceptsGzip reports whether the client asked for gzip (Accept-Encoding).
func acceptsGzip(r *http.Request) bool {
	for _, line := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(line, ",") {
			name, params, _ := strings.Cut(part, ";")
			if name = strings.ToLower(strings.TrimSpace(name)); name != "gzip" && name != "x-gzip" {
				continue
			}
			if p := strings.ToLower(strings.ReplaceAll(params, " ", "")); strings.HasPrefix(p, "q=") {
				if q, err := strconv.ParseFloat(p[2:], 64); err == nil && q <= 0 {
					return false
				}
			}
			return true
		}
	}
	return false
}

// gzipInto writes src, compressed, to dst.
func gzipInto(dst *bytes.Buffer, src []byte) error {
	zw := webGzipPool.Get().(*gzip.Writer)
	defer webGzipPool.Put(zw)
	zw.Reset(dst)
	if _, err := zw.Write(src); err != nil {
		return err
	}
	return zw.Close()
}

// etagMatches reports whether an If-None-Match header names etag (compared the
// weak way, as the header requires).
func etagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, t := range strings.Split(header, ",") {
		if t = strings.TrimSpace(t); t == "*" || strings.TrimPrefix(t, "W/") == want {
			return true
		}
	}
	return false
}

// writeBody sends body with its length; compressed when the client takes gzip
// and the body is large enough to gain from it. The caller has set the content
// type and marks compressible types with Vary: Accept-Encoding.
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte, compressible bool) {
	h := w.Header()
	if compressible {
		h.Add("Vary", "Accept-Encoding")
		if len(body) >= webGzipMinBytes && r != nil && acceptsGzip(r) {
			zb := webBufPool.Get().(*bytes.Buffer)
			defer putWebBuf(zb)
			if err := gzipInto(zb, body); err == nil && zb.Len() < len(body) {
				h.Set("Content-Encoding", "gzip")
				body = zb.Bytes()
			}
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSON sends v as JSON. r may be nil (then the answer is never compressed).
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf := webBufPool.Get().(*bytes.Buffer)
	defer putWebBuf(buf)
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		log.Printf("web: encode response: %v", err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":"internal error"}` + "\n")
		w.Header().Del("ETag")
	}
	writeJSONBytes(w, r, status, buf.Bytes())
}

// writeJSONBytes sends an already encoded JSON body.
func writeJSONBytes(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	writeBody(w, r, status, body, status == http.StatusOK)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nil, status, map[string]string{"error": msg})
}

// webCallsResponse is the body of GET /api/calls. Calls holds one row per
// token (its first call); Total counts those rows.
type webCallsResponse struct {
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	Per        int            `json:"per"`
	Horizon    string         `json:"horizon"`
	Sort       string         `json:"sort"`
	Dir        string         `json:"dir"`
	USDOnly    bool           `json:"usd_only"`
	Verdict    string         `json:"verdict"`     // the Perceptor filter as a list, e.g. "clean,caution"; "" = all
	Verdicts   []string       `json:"verdicts"`    // the same list as an array; [] = all
	SnapshotAt time.Time      `json:"snapshot_at"` // when the list was last read from the database
	Calls      []ScoutWebCall `json:"calls"`
}

var webCallsParams = map[string]bool{"q": true, "sort": true, "dir": true, "horizon": true, "usd_only": true, "verdict": true, "page": true, "per": true}

// parseWebCallsQuery validates the query string of /api/calls. Every value is
// checked against a fixed list or range; the error text is safe to show.
func parseWebCallsQuery(v url.Values) (ScoutWebCallsFilter, error) {
	f := ScoutWebCallsFilter{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: webDefaultPer}
	for k, vals := range v {
		if !webCallsParams[k] {
			return f, errors.New("unknown parameter (use q, sort, dir, horizon, usd_only, verdict, page, per)")
		}
		if len(vals) != 1 && k != "verdict" { // verdict may be repeated: the values add up
			return f, fmt.Errorf("%s: given more than once", k)
		}
	}
	oneOf := func(key string, dst *string, allowed ...string) error {
		if !v.Has(key) {
			return nil
		}
		got := v.Get(key)
		for _, a := range allowed {
			if got == a {
				*dst = a
				return nil
			}
		}
		return fmt.Errorf("%s: use one of %s", key, strings.Join(allowed, ", "))
	}
	intIn := func(key string, dst *int, lo, hi int) error {
		if !v.Has(key) {
			return nil
		}
		s := v.Get(key)
		n, err := strconv.Atoi(s)
		if err != nil || len(s) > 9 || n < lo || n > hi || strconv.Itoa(n) != s {
			return fmt.Errorf("%s: use a whole number from %d to %d", key, lo, hi)
		}
		*dst = n
		return nil
	}
	if err := oneOf("sort", &f.Sort, webSortNames...); err != nil {
		return f, err
	}
	if err := oneOf("dir", &f.Dir, "desc", "asc"); err != nil {
		return f, err
	}
	if err := oneOf("horizon", &f.Horizon, "1h", "1d", "3d", "7d", "30d"); err != nil {
		return f, err
	}
	usdOnly := "0"
	if f.Sort != "date" { // every other sort: only USD-priced calls have the value
		usdOnly = "1"
	}
	if err := oneOf("usd_only", &usdOnly, "1", "0"); err != nil {
		return f, err
	}
	f.USDOnly = usdOnly == "1"
	set, err := parseWebVerdicts(v["verdict"])
	if err != nil {
		return f, err
	}
	f.Verdict = webVerdictList(set)
	if err := intIn("page", &f.Page, 1, webMaxPage); err != nil {
		return f, err
	}
	if err := intIn("per", &f.Per, 1, webMaxPer); err != nil {
		return f, err
	}
	q := v.Get("q")
	if !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
		return f, errors.New("q: not valid text")
	}
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > webMaxQueryLen {
		return f, fmt.Errorf("q: at most %d characters", webMaxQueryLen)
	}
	f.Q = q
	return f, nil
}

// parseWebVerdicts reads the verdict values of /api/calls: each one a list
// separated by commas (verdict=clean,caution), and the parameter may be given
// more than once (verdict=clean&verdict=caution); all of them add up. Empty
// items are skipped (so verdict= is all), repeats are fine, anything else is
// an error.
func parseWebVerdicts(vals []string) (uint8, error) {
	var set uint8
	for _, v := range vals {
		for v != "" {
			var item string
			item, v, _ = strings.Cut(v, ",")
			if item == "" {
				continue
			}
			b, ok := webVerdictBuckets[item]
			if !ok {
				return 0, errors.New("verdict: use clean, caution, red_flags or not_scanned, or a list of them separated by commas")
			}
			set |= 1 << b
		}
	}
	return set, nil
}

// webVerdictLists[set]: a set of Perceptor buckets written the one way it is
// used in the filter, the answer and the ETag: in the order clean, caution,
// red_flags, not_scanned, separated by commas. No bucket or every bucket is ""
// (all). Worked out once, so a request does not build the text.
var webVerdictLists = func() (out [webVerdictAll + 1]string) {
	for set := range out {
		if set == 0 || set == int(webVerdictAll) {
			continue
		}
		var names []string
		for b, name := range webVerdictNames {
			if set&(1<<b) != 0 {
				names = append(names, name)
			}
		}
		out[set] = strings.Join(names, ",")
	}
	return out
}()

func webVerdictList(set uint8) string { return webVerdictLists[set&webVerdictAll] }

// webStaticTypes: the only file types the page server hands out, with fixed
// content types (the operating system's own table is not trusted for this).
var webStaticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".txt":  "text/plain; charset=utf-8",
}

// webTextTypes: the file types worth compressing.
var webTextTypes = map[string]bool{".html": true, ".js": true, ".css": true, ".svg": true, ".txt": true}

// webStaticName turns a request path into the name of a file of the page; ok
// is false for folders, hidden files and unknown file types.
func webStaticName(urlPath string) (name, ext string, ok bool) {
	name = strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "index.html"
	}
	ext = strings.ToLower(path.Ext(name))
	_, known := webStaticTypes[ext]
	hidden := false
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			hidden = true
		}
	}
	if strings.HasSuffix(urlPath, "/") && urlPath != "/" {
		known = false // a folder, not a file
	}
	if !known || hidden || !fs.ValidPath(name) || strings.ContainsAny(name, `\:`) {
		return "", "", false
	}
	return name, ext, true
}

// webStaticFile is one file of the built-in page, read once at start.
type webStaticFile struct {
	ctype  string
	text   bool   // a compressible type
	body   []byte // as it is
	gz     []byte // compressed; nil when that is not smaller
	etag   string // strong: a hash of the content
	etagGz string // of the compressed form
}

// loadWebStatic reads every servable file of the built-in page into memory.
func loadWebStatic(static fs.FS) (map[string]*webStaticFile, error) {
	files := map[string]*webStaticFile{}
	err := fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		name, ext, ok := webStaticName("/" + p)
		if !ok || name != p {
			return nil
		}
		body, err := fs.ReadFile(static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		tag := hex.EncodeToString(sum[:12])
		f := &webStaticFile{ctype: webStaticTypes[ext], text: webTextTypes[ext], body: body, etag: `"` + tag + `"`, etagGz: `"` + tag + `-gz"`}
		if f.text && len(body) >= webGzipMinBytes {
			var zb bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&zb, gzip.BestCompression) // once, at start
			if _, err := zw.Write(body); err == nil && zw.Close() == nil && zb.Len() < len(body) {
				f.gz = zb.Bytes()
			}
		}
		files[name] = f
		return nil
	})
	return files, err
}

// serveStatic serves one file of the page. No folder listings, no hidden
// files, only the known file types. Browsers keep a copy but ask every time
// whether it is still current (Cache-Control: no-cache), so an edited page
// shows at once and an unchanged one costs a 304.
func (s *webServer) serveStatic(w http.ResponseWriter, r *http.Request) {
	name, ext, ok := webStaticName(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	if s.cfg.Dir == "" {
		// The built-in page: from memory, with a content hash as ETag.
		f := s.files[name]
		if f == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.Set("Content-Type", f.ctype)
		h.Set("Cache-Control", "no-cache")
		body, etag := f.body, f.etag
		if f.text {
			h.Add("Vary", "Accept-Encoding")
			if f.gz != nil && acceptsGzip(r) {
				body, etag = f.gz, f.etagGz
				h.Set("Content-Encoding", "gzip")
			}
		}
		h.Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			h.Del("Content-Type")
			h.Del("Content-Encoding")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	// SCOUT_WEB_DIR: the file is read from the folder for every request (so an
	// edit shows at once); the browser's copy is checked by modification time.
	f, err := s.static.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.Set("Content-Type", webStaticTypes[ext])
	h.Set("Cache-Control", "no-cache")
	if webTextTypes[ext] {
		h.Add("Vary", "Accept-Encoding")
		if size := info.Size(); size >= webGzipMinBytes && size <= webDiskGzipMaxBytes && acceptsGzip(r) {
			zb := webBufPool.Get().(*bytes.Buffer)
			defer putWebBuf(zb)
			zw := webGzipPool.Get().(*gzip.Writer)
			zw.Reset(zb)
			_, err := io.Copy(zw, rs)
			if cerr := zw.Close(); err == nil {
				err = cerr
			}
			webGzipPool.Put(zw)
			if err != nil {
				h.Del("Vary")
				http.Error(w, "could not read the file", http.StatusInternalServerError)
				return
			}
			h.Set("Content-Encoding", "gzip")
			http.ServeContent(w, r, "", info.ModTime(), bytes.NewReader(zb.Bytes()))
			return
		}
	}
	http.ServeContent(w, r, "", info.ModTime(), rs)
}

// webAnswers keeps the encoded answers of /api/calls for one snapshot, by ETag
// (the snapshot's version and the question asked), so the same question asked
// again before the next refresh is not worked out and compressed again. It
// belongs to one snapshot and goes with it; it never grows beyond
// webAnswersMax answers or webAnswersMaxBytes, after which further questions
// are simply answered without being kept. A nil *webAnswers keeps nothing.
type webAnswers struct {
	mu    sync.Mutex
	m     map[string]*webAnswer
	bytes int
}

// webAnswer is one kept answer; gz is made the first time a client asks for gzip.
type webAnswer struct {
	plain []byte
	once  sync.Once
	gz    []byte // nil when compressing did not make it smaller
}

func (c *webAnswers) get(key string) *webAnswer {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

// add keeps a copy of body under key and returns the kept answer (the one
// already there when two requests raced); nil when there is no room.
func (c *webAnswers) add(key string, body []byte) *webAnswer {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.m[key]; a != nil {
		return a
	}
	if len(c.m) >= webAnswersMax || c.bytes+len(body) > webAnswersMaxBytes {
		return nil
	}
	if c.m == nil {
		c.m = map[string]*webAnswer{}
	}
	a := &webAnswer{plain: bytes.Clone(body)}
	c.m[key] = a
	c.bytes += len(body)
	return a
}

// send writes a kept JSON answer, compressed for clients that take gzip.
func (a *webAnswer) send(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Add("Vary", "Accept-Encoding")
	body := a.plain
	if len(body) >= webGzipMinBytes && acceptsGzip(r) {
		a.once.Do(func() {
			var zb bytes.Buffer
			if err := gzipInto(&zb, a.plain); err == nil && zb.Len() < len(a.plain) {
				a.gz = zb.Bytes()
			}
		})
		if a.gz != nil {
			body = a.gz
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// logLimiter lets one message through per interval.
type logLimiter struct {
	every time.Duration
	last  time.Time
}

func (l *logLimiter) allow(now time.Time) bool {
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		return false
	}
	l.last = now
	return true
}

// webServer is the website: the snapshot it answers from, and the page.
type webServer struct {
	st     *ScoutStore
	cfg    webConfig
	static fs.FS
	files  map[string]*webStaticFile // the built-in page (nil with SCOUT_WEB_DIR)
	mux    *http.ServeMux

	snap atomic.Pointer[webSnapshot] // what requests read; nil until the first refresh

	// readRows reads the list from the database (ScoutStore.SelectWebRows;
	// tests put a fake in its place). nil = no database.
	readRows func(context.Context) ([]ScoutWebRow, int, error)
	// readReports reads the texts of the reports with the given ids
	// (ScoutStore.SelectWebReports; tests put a fake in its place). Only ids
	// the snapshot does not hold yet are asked for, as part of the same read
	// as readRows. nil = no texts: the rows then show no report detail.
	readReports func(context.Context, []int) (map[int]*ScoutWebReport, error)
	// life is the context the reads run under: cancelled when the website
	// stops, never by a single request (runWeb sets it; Background otherwise).
	life context.Context

	// flightMu guards the read under way and the "Refresh now" limit. Every
	// read of the database goes through startRefreshLocked, so there is never
	// more than one at a time: the background loop and "Refresh now" join the
	// read under way instead of starting another.
	flightMu   sync.Mutex
	inflight   *webRefreshCall
	lastManual time.Time // when the last read started by "Refresh now" ended
	// manualWait: how long POST /api/refresh waits for the read
	// (webManualRefreshWait; shorter in tests)
	manualWait time.Duration
	joins      atomic.Int64 // waits that joined a read already under way

	refreshMu sync.Mutex // held by the read itself; guards the fields below
	errLog    logLimiter
	failing   bool
	lastRows  int // rows of the last snapshot (−1 = none yet)

	// events: the open live-update streams (GET /api/events); every refresh
	// publishes what changed (events.go)
	events *webEventHub
	// sseHeartbeat: how often a quiet stream gets a comment line
	// (webSSEHeartbeat; shorter in tests)
	sseHeartbeat time.Duration
}

// webRefreshCall is one read of the database; done is closed when it has
// ended, err is its result (set before done is closed).
type webRefreshCall struct {
	done    chan struct{}
	err     error
	manual  bool      // started by "Refresh now"
	started time.Time // when the read began (refreshAfter)
}

// newWebServer builds the site: GET /api/summary, GET /api/calls, GET
// /api/call, GET /api/events, POST /api/refresh and the page. The API answers
// 503 until refresh has succeeded once.
func newWebServer(st *ScoutStore, cfg webConfig, static fs.FS) (*webServer, error) {
	s := &webServer{st: st, cfg: cfg, static: static, mux: http.NewServeMux(), lastRows: -1,
		errLog: logLimiter{every: webErrorLogEvery}, life: context.Background(), manualWait: webManualRefreshWait,
		events: newWebEventHub(webEventsMaxClients, webEventsClientBuf), sseHeartbeat: webSSEHeartbeat}
	if st != nil {
		s.readRows = st.SelectWebRows
		s.readReports = st.SelectWebReports
	}
	if cfg.Dir == "" {
		files, err := loadWebStatic(static)
		if err != nil {
			return nil, fmt.Errorf("built-in page: %w", err)
		}
		s.files = files
	}
	s.mux.HandleFunc("/api/summary", s.handleSummary)
	s.mux.HandleFunc("/api/calls", s.handleCalls)
	s.mux.HandleFunc("/api/call", s.handleCall)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc(webRefreshPath, s.handleRefresh)
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no such endpoint")
	})
	s.mux.HandleFunc("/", s.serveStatic)
	return s, nil
}

// webRefreshPath is the one address that takes POST (and only POST).
const webRefreshPath = "/api/refresh"

func (s *webServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	webSecurityHeaders(w.Header())
	method := http.MethodGet
	if r.URL.Path == webRefreshPath {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeJSONError(w, http.StatusMethodNotAllowed, "only "+method+" is supported here")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// refresh brings the snapshot up to date: it reads the list from the database
// and puts the new snapshot in place of the old one, or, when a read is
// already under way, waits for that one instead of starting another. On an
// error the old snapshot stays and keeps being served. The background loop
// uses it, and so do the tests to bring the website up to date at once.
// ctx only limits the wait: the read itself runs under s.life.
func (s *webServer) refresh(ctx context.Context) error {
	s.flightMu.Lock()
	c := s.startRefreshLocked(false)
	s.flightMu.Unlock()
	return c.wait(ctx)
}

// startRefreshLocked returns the read under way, or starts one. flightMu is held.
func (s *webServer) startRefreshLocked(manual bool) *webRefreshCall {
	if c := s.inflight; c != nil {
		s.joins.Add(1)
		return c
	}
	c := &webRefreshCall{done: make(chan struct{}), manual: manual, started: time.Now()}
	s.inflight = c
	go func() {
		err := s.readSnapshot(s.life)
		s.flightMu.Lock()
		c.err = err
		s.inflight = nil
		if c.manual {
			s.lastManual = time.Now()
		}
		s.flightMu.Unlock()
		close(c.done)
	}()
	return c
}

// wait waits for the read to end, or for ctx.
func (c *webRefreshCall) wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readSnapshot reads the list from the database and puts the new snapshot in
// place of the old one. Only startRefreshLocked calls it, one at a time.
func (s *webServer) readSnapshot(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.readRows == nil {
		return errors.New("no database")
	}
	ctx, cancel := context.WithTimeout(ctx, webSnapshotTimeout)
	defer cancel()
	start := time.Now()
	rows, updatePosts, err := s.readRows(ctx)
	var reports map[int]*webReport
	if err == nil {
		reports, err = s.readReportTexts(ctx, rows)
	}
	if err != nil {
		s.failing = true
		// one line a minute at most; none when the program is stopping, and none
		// before the first snapshot (then the caller reports it)
		if old := s.snap.Load(); old != nil && !errors.Is(ctx.Err(), context.Canceled) && s.errLog.allow(start) {
			log.Printf("web: could not refresh the snapshot: %v — still showing the data of %s", err, old.loadedAt.Local().Format("15:04:05"))
		}
		return err
	}
	snap, err := newWebSnapshot(rows, updatePosts, s.snap.Load(), s.cfg)
	if err != nil {
		s.failing = true
		if s.snap.Load() != nil && s.errLog.allow(start) {
			log.Printf("web: could not build the snapshot: %v — still showing the data read before", err)
		}
		return err
	}
	snap.took = time.Since(start)
	snap.loadedAt = time.Now().UTC().Truncate(time.Millisecond)
	snap.answers = &webAnswers{} // answers carry loadedAt: none is taken over from the snapshot before
	snap.reports = reports
	prev := s.snap.Load()
	s.snap.Store(snap)
	// the open pages hear what changed (never blocks)
	s.publishSnapshotEvents(prev, snap)
	if s.failing {
		s.failing, s.errLog.last = false, time.Time{}
		if s.lastRows >= 0 {
			log.Printf("web: the snapshot refreshes again")
		}
	}
	if n := snap.n; n != s.lastRows || snap.took > webSlowSnapshot {
		s.lastRows = n
		log.Printf("web: snapshot %s tokens in %s", commas(n), snap.took.Round(time.Millisecond))
	}
	return nil
}

// readReportTexts makes the text map for the rows just read: the texts of the
// snapshot in place are kept, and only the report ids it does not hold are read
// from the database, with one query (none when there is nothing new). It may
// take an id off a row whose report could not be read (see webReportsFor).
// Only readSnapshot calls it.
func (s *webServer) readReportTexts(ctx context.Context, rows []ScoutWebRow) (map[int]*webReport, error) {
	var old map[int]*webReport
	if prev := s.snap.Load(); prev != nil {
		old = prev.reports
	}
	var got map[int]*ScoutWebReport
	if missing := webMissingReports(rows, old); len(missing) > 0 && s.readReports != nil {
		var err error
		if got, err = s.readReports(ctx, missing); err != nil {
			return nil, fmt.Errorf("report texts: %w", err)
		}
	}
	return webReportsFor(rows, old, got), nil
}

// refreshLoop refreshes the snapshot every cfg.Refresh until ctx is cancelled.
func (s *webServer) refreshLoop(ctx context.Context) {
	every := s.cfg.Refresh
	if every <= 0 {
		every = defaultWebRefresh
	}
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = s.refresh(ctx) // logged there; the old snapshot keeps being served
		t.Reset(every)
	}
}

// snapshotHeaders: what both endpoints send with a snapshot's data, also with
// a 304. The ETag is weak: an answer with the same ETag has the same data, but
// its time stamps move with every refresh.
func (s *webServer) snapshotHeaders(w http.ResponseWriter, snap *webSnapshot, etag string) {
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Snapshot-At", snap.loadedAt.Format(time.RFC3339))
}

func (s *webServer) notModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	if !etagMatches(r.Header.Get("If-None-Match"), etag) {
		return false
	}
	w.Header().Add("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusNotModified)
	return true
}

func (s *webServer) snapshot(w http.ResponseWriter) *webSnapshot {
	snap := s.snap.Load()
	if snap == nil {
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusServiceUnavailable, "starting: the data is not loaded yet")
	}
	return snap
}

func (s *webServer) handleSummary(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) > 0 {
		writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
		return
	}
	snap := s.snapshot(w)
	if snap == nil {
		return
	}
	etag := `W/"` + snap.summaryTag + `-summary"`
	s.snapshotHeaders(w, snap, etag)
	if s.notModified(w, r, etag) {
		return
	}
	sum := snap.summary
	sum.UpdatedAt = snap.loadedAt
	sum.SnapshotAgeSeconds = max(0, float64(time.Since(snap.loadedAt).Milliseconds()/100)/10)
	writeJSON(w, r, http.StatusOK, &sum)
}

// webRefreshResponse is the body of POST /api/refresh.
type webRefreshResponse struct {
	// Refreshed: the database was read for this request (by this request, or
	// by the read under way that it waited for).
	Refreshed bool `json:"refreshed"`
	// RateLimited: no read, because a read started by "Refresh now" ended less
	// than webManualRefreshEvery ago; the answer describes the snapshot as it is.
	RateLimited        bool      `json:"rate_limited"`
	SnapshotAt         time.Time `json:"snapshot_at"` // when the database was last read
	SnapshotAgeSeconds float64   `json:"snapshot_age_seconds"`
}

// sameOrigin reports whether a request may trigger a read: it has no Origin
// header (browsers send one with every POST, so this is not a page of another
// site), or exactly one naming this very host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Values("Origin")
	if len(origin) == 0 {
		return true
	}
	if len(origin) != 1 {
		return false
	}
	u, err := url.Parse(origin[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false // also "null", which sandboxed pages and some redirects send
	}
	return strings.EqualFold(u.Host, r.Host)
}

// handleRefresh is "Refresh now": POST /api/refresh reads the database at once
// (the same read the background loop does) and answers when the new snapshot
// is in place. A press while a read is under way waits for that read; a press
// less than webManualRefreshEvery after the end of the last read that
// "Refresh now" started gets the snapshot as it is, without a read.
func (s *webServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "requests from other sites are not accepted")
		return
	}
	if r.URL.RawQuery != "" {
		writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
		return
	}
	s.flightMu.Lock()
	var c *webRefreshCall
	limited := s.inflight == nil && !s.lastManual.IsZero() && time.Since(s.lastManual) < webManualRefreshEvery
	if !limited {
		c = s.startRefreshLocked(true)
	}
	s.flightMu.Unlock()

	var err error
	if c != nil {
		ctx, cancel := context.WithTimeout(r.Context(), s.manualWait)
		err = c.wait(ctx)
		cancel()
	}
	w.Header().Set("Cache-Control", "no-store")
	snap := s.snap.Load()
	if snap != nil {
		w.Header().Set("X-Snapshot-At", snap.loadedAt.Format(time.RFC3339))
	}
	if err != nil || snap == nil {
		// the details are in the log (refresh logs them); the page gets a short notice
		status, msg := http.StatusServiceUnavailable, "could not read the database"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status, msg = http.StatusGatewayTimeout, "the database did not answer in time"
		}
		body := map[string]any{"error": msg}
		if snap != nil {
			body["snapshot_at"] = snap.loadedAt
		}
		writeJSON(w, nil, status, body)
		return
	}
	writeJSON(w, nil, http.StatusOK, &webRefreshResponse{Refreshed: !limited, RateLimited: limited, SnapshotAt: snap.loadedAt,
		SnapshotAgeSeconds: max(0, float64(time.Since(snap.loadedAt).Milliseconds()/100)/10)})
}

// callsETag: the snapshot's version plus what was asked. Two ways of writing
// the same question (order of the parameters, defaults left out, letter case
// of the search text) get the same ETag.
func (s *webServer) callsETag(snap *webSnapshot, f ScoutWebCallsFilter) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t\x00%s\x00%d\x00%d",
		strings.ToLower(f.Q), f.Sort, f.Dir, f.Horizon, f.USDOnly, f.Verdict, f.Page, f.Per)))
	return `W/"` + snap.version + "-" + hex.EncodeToString(sum[:10]) + `"`
}

// webVerdictSlices[set]: the list of webVerdictLists[set] as an array for the
// answer ([] = all). Shared and never changed.
var webVerdictSlices = func() (out [webVerdictAll + 1][]string) {
	for set, list := range webVerdictLists {
		out[set] = []string{}
		if list != "" {
			out[set] = strings.Split(list, ",")
		}
	}
	return out
}()

// webVerdictSlice: the filter's list as an array for the answer ([] = all).
func webVerdictSlice(list string) []string {
	set, err := webVerdictSet(list)
	if err != nil || set == webVerdictAll {
		return []string{}
	}
	return webVerdictSlices[set]
}

func (s *webServer) handleCalls(w http.ResponseWriter, r *http.Request) {
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed query string")
		return
	}
	f, err := parseWebCallsQuery(vals)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	snap := s.snapshot(w)
	if snap == nil {
		return
	}
	etag := s.callsETag(snap, f)
	s.snapshotHeaders(w, snap, etag)
	if s.notModified(w, r, etag) {
		return
	}
	if a := snap.answers.get(etag); a != nil {
		a.send(w, r) // asked before, of this snapshot
		return
	}
	var room [webMaxPer]int32
	rows, total, err := snap.page(f, room[:0])
	var head []byte
	if err == nil {
		// everything but the rows, which are already encoded in the snapshot
		head, err = json.Marshal(webCallsResponse{Total: total, Page: f.Page, Per: f.Per, Horizon: f.Horizon,
			Sort: f.Sort, Dir: f.Dir, USDOnly: f.USDOnly, Verdict: f.Verdict, Verdicts: webVerdictSlice(f.Verdict),
			SnapshotAt: snap.loadedAt, Calls: []ScoutWebCall{}})
		if err == nil && !bytes.HasSuffix(head, []byte(webNoCallsJSON)) {
			err = errors.New("unexpected encoding of the answer")
		}
	}
	if err != nil {
		log.Printf("web: calls: %v", err)
		w.Header().Del("ETag")
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h, _ := webHorizonIndex(f.Horizon)
	size := len(head) + 2
	for _, pos := range rows {
		size += int(snap.rowAt[pos+1]-snap.rowAt[pos]) + webRowNumbersRoom
	}
	buf := webBufPool.Get().(*bytes.Buffer)
	defer putWebBuf(buf)
	buf.Grow(size)
	b := append(buf.AvailableBuffer(), head[:len(head)-2]...) // up to the opening bracket of "calls"
	for i, pos := range rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = snap.appendRow(b, pos, h)
	}
	b = append(b, "]}\n"...)
	buf.Write(b)
	if a := snap.answers.add(etag, buf.Bytes()); a != nil {
		a.send(w, r)
		return
	}
	writeJSONBytes(w, r, http.StatusOK, buf.Bytes())
}

// webCallDetail is the body of GET /api/call: the reports of the token of one
// listed call (the call's own row in /api/calls says which ones).
type webCallDetail struct {
	CallID    int                 `json:"call_id"`
	Perceptor *webPerceptorDetail `json:"perceptor"` // null = no completed Perceptor report
	SAlpha    *webSAlphaDetail    `json:"salpha"`    // null = no completed sAlpha reply with text
}

// webPerceptorDetail: the token's latest completed Perceptor report.
type webPerceptorDetail struct {
	ID        int       `json:"id"`
	Verdict   string    `json:"verdict"` // clean | caution | red_flags | unknown
	Label     *string   `json:"label"`   // verdict_label, e.g. "No red flags found"
	Summary   *string   `json:"summary"` // verdict_summary
	URL       *string   `json:"url"`     // https only
	At        time.Time `json:"at"`      // completed_at, else requested_at
	Truncated bool      `json:"truncated"`
}

// webSAlphaDetail: the token's latest completed sAlpha report with text, or,
// when it has none, its latest reply that declines to report (Declined: the
// page says sAlpha did not generate a report, Text is its reason).
type webSAlphaDetail struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`     // report_text, plain text
	Declined  bool      `json:"declined"` // the reply only declines to report
	URL       *string   `json:"url"`      // https only
	At        time.Time `json:"at"`       // completed_at, else requested_at
	Truncated bool      `json:"truncated"`
}

// webCallDetailVersion: part of the ETag of /api/call; raise it when the body
// of a report changes form, so pages kept open ask again.
const webCallDetailVersion = "2"

// parseWebCallID reads the query of /api/call: exactly one id, a whole number
// from 1 to 2^31−1, written plainly.
func parseWebCallID(raw string) (int32, error) {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return 0, errors.New("malformed query string")
	}
	for k, vals := range v {
		if k != "id" {
			return 0, errors.New("unknown parameter (use id)")
		}
		if len(vals) != 1 {
			return 0, errors.New("id: given more than once")
		}
	}
	idText := v.Get("id")
	n, err := strconv.Atoi(idText)
	if !v.Has("id") || err != nil || len(idText) > 10 || n < 1 || n > math.MaxInt32 || strconv.Itoa(n) != idText {
		return 0, fmt.Errorf("id: use the call_id of a row, a whole number from 1 to %d", math.MaxInt32)
	}
	return int32(n), nil
}

// handleCall is GET /api/call?id=<call_id>: the Perceptor and sAlpha reports
// of the token of a listed call (one that is a row of the list), from memory.
// 400 for a bad id, 404 for an id that is not a row of the snapshot (also one
// of an older snapshot that has gone), 503 before the first snapshot. The ETag
// follows the two reports only, so it stays "not modified" while other data
// changes.
func (s *webServer) handleCall(w http.ResponseWriter, r *http.Request) {
	id, err := parseWebCallID(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	snap := s.snapshot(w)
	if snap == nil {
		return
	}
	pos, found := slices.BinarySearch(snap.ids, id)
	if !found {
		writeJSONError(w, http.StatusNotFound, "this call is not a row of the list (any more); refresh the page")
		return
	}
	pid, sid := snap.percID[pos], snap.salphaID[pos]
	etag := `W/"call-` + strconv.Itoa(int(id)) + "-" + strconv.Itoa(int(pid)) + "-" + strconv.Itoa(int(sid)) + "-" + webCallDetailVersion + `"`
	s.snapshotHeaders(w, snap, etag)
	if s.notModified(w, r, etag) {
		return
	}
	d := webCallDetail{CallID: int(id)}
	if e := snap.reports[int(pid)]; pid != 0 && e != nil {
		d.Perceptor = &webPerceptorDetail{ID: e.id, Verdict: e.verdict, Label: e.label, Summary: e.summary, URL: e.url, At: e.at, Truncated: e.truncated}
	}
	if e := snap.reports[int(sid)]; sid != 0 && e != nil {
		d.SAlpha = &webSAlphaDetail{ID: e.id, Text: e.text, Declined: e.declined, URL: e.url, At: e.at, Truncated: e.truncated}
	}
	writeJSON(w, r, http.StatusOK, &d)
}

const (
	webNoCallsJSON    = `,"calls":[]}`                                // how an answer without rows ends
	webRowNumbersRoom = len(webRowNumbersJSON) + 3*24 - 3*len("null") // the three numbers of a row, generously
)

// runWeb serves the website until ctx is cancelled, then shuts down gracefully.
// The list is read from the database before the first request is accepted.
func runWeb(ctx context.Context, st *ScoutStore, cfg webConfig) error {
	static, from, err := cfg.staticFS()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(cfg.GMGNTemplate, "https://") || !strings.Contains(cfg.GMGNTemplate, "{ca}") {
		log.Printf("web: warning: SCOUT_GMGN_URL=%q should start with https:// and contain {ca}; the page only links to https addresses", cfg.GMGNTemplate)
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = defaultWebRefresh
	}
	srv, err := newWebServer(st, cfg, static)
	if err != nil {
		return err
	}
	// everything started here stops with ctx, or when serving fails
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv.life = ctx // reads stop when the website stops, not when a request ends
	if err := srv.refresh(ctx); err != nil {
		return fmt.Errorf("could not read the calls from the database for the first snapshot: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on SCOUT_WEB_ADDR=%q: %w", cfg.Addr, err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer srv.events.close() // ends the open streams (before waiting for the loops)
	defer cancel()
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.refreshLoop(ctx)
	}()
	// live updates: a database notification refreshes at once (debounced)
	kick := make(chan struct{}, 1)
	go func() {
		defer wg.Done()
		srv.notifyLoop(ctx, kick, webNotifyDebounce)
	}()
	if st != nil {
		dial := dialScoutEvents(st.Pool.Config().ConnConfig)
		wg.Add(1)
		go func() {
			defer wg.Done()
			listenScoutEvents(ctx, dial, kick, defaultWebListenTimings)
		}()
	}
	log.Printf("website on http://%s (%s; read-only, no login; data read again every %s, and at once on new calls and reports) — Ctrl+C to stop", ln.Addr(), from, cfg.Refresh)
	return serveWeb(ctx, ln, srv)
}

// newWebHTTPServer is the HTTP server of the website. Its read and write
// timeouts are for ordinary requests; GET /api/events lifts them for its stream.
func newWebHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

// serveWeb serves h on ln until ctx is cancelled, then lets requests in flight finish.
func serveWeb(ctx context.Context, ln net.Listener, h http.Handler) error {
	return serveWebWith(ctx, ln, newWebHTTPServer(h))
}

// serveWebWith is serveWeb with a server of the caller's (tests set short timeouts).
func serveWebWith(ctx context.Context, ln net.Listener, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Println("website: shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	return nil
}
