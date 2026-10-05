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
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
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
	webSlowSnapshot     = time.Second      // a slower read is logged
	webErrorLogEvery    = time.Minute      // at most one "refresh failed" line per this
	webGzipMinBytes     = 1024             // smaller answers are sent as they are
	webAnswersMax       = 200              // answers of /api/calls kept per snapshot …
	webAnswersMaxBytes  = 8 << 20          // … and their size together, uncompressed
	webDiskGzipMaxBytes = 4 << 20          // SCOUT_WEB_DIR: larger files are not compressed
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
	Verdict    string         `json:"verdict"`     // the Perceptor filter; "" = all
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
		if len(vals) != 1 {
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
	if err := oneOf("sort", &f.Sort, "date", "return", "peak", "latest"); err != nil {
		return f, err
	}
	if err := oneOf("dir", &f.Dir, "desc", "asc"); err != nil {
		return f, err
	}
	if err := oneOf("horizon", &f.Horizon, "1h", "1d", "3d", "7d", "30d"); err != nil {
		return f, err
	}
	usdOnly := "0"
	if f.Sort == "return" || f.Sort == "peak" || f.Sort == "latest" {
		usdOnly = "1"
	}
	if err := oneOf("usd_only", &usdOnly, "1", "0"); err != nil {
		return f, err
	}
	f.USDOnly = usdOnly == "1"
	if err := oneOf("verdict", &f.Verdict, "clean", "caution", "red_flags", "not_scanned"); err != nil {
		return f, err
	}
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

	refreshMu sync.Mutex // one refresh at a time; guards the fields below
	errLog    logLimiter
	failing   bool
	lastRows  int // rows of the last snapshot (−1 = none yet)
}

// newWebServer builds the site: GET /api/summary, GET /api/calls and the page.
// The API answers 503 until refresh has succeeded once.
func newWebServer(st *ScoutStore, cfg webConfig, static fs.FS) (*webServer, error) {
	s := &webServer{st: st, cfg: cfg, static: static, mux: http.NewServeMux(), lastRows: -1,
		errLog: logLimiter{every: webErrorLogEvery}}
	if cfg.Dir == "" {
		files, err := loadWebStatic(static)
		if err != nil {
			return nil, fmt.Errorf("built-in page: %w", err)
		}
		s.files = files
	}
	s.mux.HandleFunc("/api/summary", s.handleSummary)
	s.mux.HandleFunc("/api/calls", s.handleCalls)
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no such endpoint")
	})
	s.mux.HandleFunc("/", s.serveStatic)
	return s, nil
}

func (s *webServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	webSecurityHeaders(w.Header())
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, http.StatusMethodNotAllowed, "only GET is supported")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// refresh reads the list from the database and puts the new snapshot in place
// of the old one. On an error the old snapshot stays and keeps being served.
// It is also what the tests call to bring the website up to date at once.
func (s *webServer) refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.st == nil {
		return errors.New("no database")
	}
	ctx, cancel := context.WithTimeout(ctx, webSnapshotTimeout)
	defer cancel()
	start := time.Now()
	rows, updatePosts, err := s.st.SelectWebRows(ctx)
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
	s.snap.Store(snap)
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

// callsETag: the snapshot's version plus what was asked. Two ways of writing
// the same question (order of the parameters, defaults left out, letter case
// of the search text) get the same ETag.
func (s *webServer) callsETag(snap *webSnapshot, f ScoutWebCallsFilter) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t\x00%s\x00%d\x00%d",
		strings.ToLower(f.Q), f.Sort, f.Dir, f.Horizon, f.USDOnly, f.Verdict, f.Page, f.Per)))
	return `W/"` + snap.version + "-" + hex.EncodeToString(sum[:10]) + `"`
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
			Sort: f.Sort, Dir: f.Dir, USDOnly: f.USDOnly, Verdict: f.Verdict, SnapshotAt: snap.loadedAt, Calls: []ScoutWebCall{}})
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
	if err := srv.refresh(ctx); err != nil {
		return fmt.Errorf("could not read the calls from the database for the first snapshot: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on SCOUT_WEB_ADDR=%q: %w", cfg.Addr, err)
	}
	go srv.refreshLoop(ctx)
	log.Printf("website on http://%s (%s; read-only, no login; data read again every %s) — Ctrl+C to stop", ln.Addr(), from, cfg.Refresh)
	return serveWeb(ctx, ln, srv)
}

// serveWeb serves h on ln until ctx is cancelled, then lets requests in flight finish.
func serveWeb(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
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
