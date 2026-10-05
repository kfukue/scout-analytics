package main

import (
	"bytes"
	"context"
	"embed"
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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Read-only website (-web): a small JSON API over the dataset view plus the
// static page in frontend/. No Telegram, no login, no writes.
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
	webQueryTimeout     = 15 * time.Second
)

type webConfig struct {
	Addr         string // SCOUT_WEB_ADDR
	GMGNTemplate string // SCOUT_GMGN_URL, with {ca} for the contract address
	Dir          string // SCOUT_WEB_DIR: serve the page from this folder instead of the embedded copy
}

func loadWebConfig() webConfig {
	return webConfig{
		Addr:         env("SCOUT_WEB_ADDR", defaultWebAddr),
		GMGNTemplate: env("SCOUT_GMGN_URL", defaultGMGNTemplate),
		Dir:          env("SCOUT_WEB_DIR", ""),
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		log.Printf("web: encode response: %v", err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":"internal error"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// webCallsResponse is the body of GET /api/calls. Calls holds one row per
// token (its first call); Total counts those rows.
type webCallsResponse struct {
	Total   int            `json:"total"`
	Page    int            `json:"page"`
	Per     int            `json:"per"`
	Horizon string         `json:"horizon"`
	Sort    string         `json:"sort"`
	Dir     string         `json:"dir"`
	USDOnly bool           `json:"usd_only"`
	Calls   []ScoutWebCall `json:"calls"`
}

var webCallsParams = map[string]bool{"q": true, "sort": true, "dir": true, "horizon": true, "usd_only": true, "page": true, "per": true}

// parseWebCallsQuery validates the query string of /api/calls. Every value is
// checked against a fixed list or range; the error text is safe to show.
func parseWebCallsQuery(v url.Values) (ScoutWebCallsFilter, error) {
	f := ScoutWebCallsFilter{Sort: "date", Dir: "desc", Horizon: "1d", Page: 1, Per: webDefaultPer}
	for k, vals := range v {
		if !webCallsParams[k] {
			return f, errors.New("unknown parameter (use q, sort, dir, horizon, usd_only, page, per)")
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
	if err := oneOf("sort", &f.Sort, "date", "return", "peak"); err != nil {
		return f, err
	}
	if err := oneOf("dir", &f.Dir, "desc", "asc"); err != nil {
		return f, err
	}
	if err := oneOf("horizon", &f.Horizon, "1h", "1d", "3d", "7d", "30d"); err != nil {
		return f, err
	}
	usdOnly := "0"
	if f.Sort == "return" || f.Sort == "peak" {
		usdOnly = "1"
	}
	if err := oneOf("usd_only", &usdOnly, "1", "0"); err != nil {
		return f, err
	}
	f.USDOnly = usdOnly == "1"
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

// finite drops values JSON cannot carry (NaN, ±Inf).
func finite(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	return p
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

// serveWebStatic serves one file of the page. No folder listings, no hidden
// files, only the known file types.
func serveWebStatic(static fs.FS, w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	ctype, known := webStaticTypes[strings.ToLower(path.Ext(name))]
	hidden := false
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			hidden = true
		}
	}
	if strings.HasSuffix(r.URL.Path, "/") && r.URL.Path != "/" {
		known = false // a folder, not a file
	}
	if !known || hidden || !fs.ValidPath(name) || strings.ContainsAny(name, `\:`) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := static.Open(name)
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
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", info.ModTime(), rs)
}

// newWebHandler builds the site: GET /api/summary, GET /api/calls and the page.
func newWebHandler(st *ScoutStore, cfg webConfig, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) > 0 {
			writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), webQueryTimeout)
		defer cancel()
		s, err := st.WebSummary(ctx)
		if err != nil {
			log.Printf("web: summary: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("/api/calls", func(w http.ResponseWriter, r *http.Request) {
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
		ctx, cancel := context.WithTimeout(r.Context(), webQueryTimeout)
		defer cancel()
		calls, total, err := st.SelectWebCalls(ctx, f)
		if err != nil {
			log.Printf("web: calls: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "database error")
			return
		}
		for i := range calls {
			c := &calls[i]
			c.MessageDate = c.MessageDate.UTC()
			c.LastCallDate = c.LastCallDate.UTC()
			c.PostURL = postURL(c.ChannelUsername, c.MessageID)
			c.GMGNURL = cfg.gmgnURL(c.ContractAddress)
			c.EntryPriceUSD, c.ReturnPct, c.PeakPct, c.DrawdownPct = finite(c.EntryPriceUSD), finite(c.ReturnPct), finite(c.PeakPct), finite(c.DrawdownPct)
		}
		writeJSON(w, http.StatusOK, webCallsResponse{Total: total, Page: f.Page, Per: f.Per, Horizon: f.Horizon,
			Sort: f.Sort, Dir: f.Dir, USDOnly: f.USDOnly, Calls: calls})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { serveWebStatic(static, w, r) })

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webSecurityHeaders(w.Header())
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "only GET is supported")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// runWeb serves the website until ctx is cancelled, then shuts down gracefully.
func runWeb(ctx context.Context, st *ScoutStore, cfg webConfig) error {
	static, from, err := cfg.staticFS()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(cfg.GMGNTemplate, "https://") || !strings.Contains(cfg.GMGNTemplate, "{ca}") {
		log.Printf("web: warning: SCOUT_GMGN_URL=%q should start with https:// and contain {ca}; the page only links to https addresses", cfg.GMGNTemplate)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on SCOUT_WEB_ADDR=%q: %w", cfg.Addr, err)
	}
	log.Printf("website on http://%s (%s; read-only, no login) — Ctrl+C to stop", ln.Addr(), from)
	return serveWeb(ctx, ln, newWebHandler(st, cfg, static))
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
