// Command scoutanalytics listens to a Telegram channel (default @scoutrobinhood),
// extracts contract addresses (CAs) from every new post, sends each CA to every
// configured investigation bot in parallel (default @perceptor0xBot and
// @salpha_research_bot), and delivers the reports to a separate chat when the
// gating tool's verdict (Perceptor) is allowed (default: clean or caution).
//
// It runs as a Telegram *user* account (MTProto via gotd), because a normal
// bot cannot read other channels it doesn't admin or talk to other bots.
//
// Run from the repo root (so the root .env is picked up):
//
//	go run ./telegrambot/scoutanalytics                 # listen forever
//	go run ./telegrambot/scoutanalytics -dry-run        # listen, log verdicts, deliver nothing
//	go run ./telegrambot/scoutanalytics -scan 0xABC...  # one-off scan: print + deliver (add -no-deliver to only print)
//
// See README.md in this folder for configuration.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/kfukue/geth-analytics-api/database"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	APIID       int
	APIHash     string
	Phone       string
	Password    string // optional 2FA password
	SessionFile string

	SourceChannel string     // username of channel to watch, without @
	Tools         []ToolSpec // investigation bots, see tools.go

	NotifyPeer     string // "me" (Saved Messages), "@username", or numeric chat/channel id
	NotifyBotToken string // optional: deliver via Bot API instead (gives push notifications)
	NotifyChatID   string // chat id for NotifyBotToken

	DeliverLevels  map[string]bool // gating verdict levels to deliver (default: clean,caution)
	RedFlagMarkers []string        // lowercase substrings that mark a report as warning/red flag
	SafePhrases    []string        // lowercase phrases stripped before marker matching ("no red flags")
	Chains         map[string]bool

	ScanGap      time.Duration // pause between consecutive CAs
	PollInterval time.Duration // re-check the channel for missed posts (0 = off)

	StateDir string
	DryRun   bool

	DBMode        string // SCOUT_DB: "repo" (default: the repo's database package, DB_USER/DB_PASS/DB_NAME_DEV) | "off"
	DatabaseURL   string // SCOUT_DATABASE_URL: optional override with an explicit DSN
	DBAutoMigrate bool

	Price priceConfig // performance tracking (GeckoTerminal), see prices.go

	// Model scoring (see score.go). Off unless SCOUT_MODEL_URL is set.
	ModelURL     string        // base URL of the scoring service, e.g. http://127.0.0.1:8601
	ModelTimeout time.Duration // per request; delivery never waits longer than this
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("warning: bad duration %s=%q, using %s", key, v, def)
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

const (
	defaultMarkers = "🚩,⚠️,⚠,❗,‼️,⛔,🔴,red flag,caution,warning,honeypot,high risk,danger,scam,rug pull,rugpull,blacklist,cannot sell,can't sell,not renounced,mintable,proxy contract"
	defaultSafe    = "mintable: no,mintable: false,blacklist: no,blacklisted: no,proxy: no,no red flags,no red flag,0 red flags,no warnings,no warning,0 warnings,warnings: 0,warnings: none,red flags: 0,red flags: none,not a honeypot,honeypot: no,honeypot: false,honeypot: ✅"
)

func loadConfig(envFile string) (*config, error) {
	if err := godotenv.Load(envFile); err != nil {
		log.Printf("note: could not load %s (%v); using process environment", envFile, err)
	}
	apiID, err := strconv.Atoi(os.Getenv("API_ID"))
	if err != nil {
		return nil, fmt.Errorf("API_ID missing/invalid: %w", err)
	}
	c := &config{
		APIID:          apiID,
		APIHash:        os.Getenv("API_HASH"),
		Phone:          os.Getenv("PHONE"),
		Password:       os.Getenv("TG_PASSWORD"),
		SessionFile:    env("SCOUT_SESSION_FILE", "scout.session.json"),
		SourceChannel:  strings.TrimPrefix(env("SCOUT_SOURCE_CHANNEL", "scoutrobinhood"), "@"),
		NotifyPeer:     env("SCOUT_NOTIFY_PEER", "me"),
		NotifyBotToken: os.Getenv("SCOUT_NOTIFY_BOT_TOKEN"),
		NotifyChatID:   os.Getenv("SCOUT_NOTIFY_CHAT_ID"),
		RedFlagMarkers: splitList(env("SCOUT_RED_FLAG_MARKERS", defaultMarkers)),
		SafePhrases:    splitList(env("SCOUT_SAFE_PHRASES", defaultSafe)),
		Chains:         map[string]bool{},
		ScanGap:        envDur("SCOUT_SCAN_GAP", 3*time.Second),
		StateDir:       env("SCOUT_STATE_DIR", "scoutanalytics_data"),
		PollInterval:   envDur("SCOUT_POLL_INTERVAL", 20*time.Second),
		DBMode:         strings.ToLower(env("SCOUT_DB", "repo")),
		DatabaseURL:    os.Getenv("SCOUT_DATABASE_URL"),
		DBAutoMigrate:  !strings.EqualFold(env("SCOUT_DB_AUTO_MIGRATE", "true"), "false"),
		ModelURL:       strings.TrimRight(env("SCOUT_MODEL_URL", ""), "/"),
		ModelTimeout:   envDur("SCOUT_MODEL_TIMEOUT", 5*time.Second),
	}
	c.DeliverLevels = map[string]bool{}
	for _, l := range splitList(env("SCOUT_DELIVER_LEVELS", levelClean+","+levelCaution)) {
		c.DeliverLevels[l] = true
	}
	for _, ch := range splitList(env("SCOUT_CHAINS", "evm,solana")) {
		c.Chains[ch] = true
	}
	if c.APIHash == "" {
		return nil, errors.New("API_HASH is required")
	}
	if c.Tools, err = loadTools(); err != nil {
		return nil, err
	}
	if c.Price, err = loadPriceConfig(); err != nil {
		return nil, err
	}
	if c.NotifyBotToken != "" && c.NotifyChatID == "" {
		return nil, errors.New("SCOUT_NOTIFY_CHAT_ID is required when SCOUT_NOTIFY_BOT_TOKEN is set")
	}
	return c, os.MkdirAll(c.StateDir, 0o700)
}

// ---------------------------------------------------------------------------
// CA extraction & report classification
// ---------------------------------------------------------------------------

var (
	evmRe = regexp.MustCompile(`\b0x[a-fA-F0-9]{40}\b`)
	solRe = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{32,44}\b`)
)

func looksLikeSolana(s string) bool {
	hasDigit, hasUpper, hasLower := false, false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	return hasDigit && hasUpper && hasLower
}

// extractCAs returns unique CAs found in the text and in any linked URLs.
func extractCAs(text string, urls []string, chains map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ca string) {
		k := caKey(ca)
		if !seen[k] {
			seen[k] = true
			out = append(out, ca)
		}
	}
	sources := []string{text}
	for _, u := range urls {
		// Links to wallet / transaction pages (e.g. the "Live buys" wallets) carry
		// addresses that are not the token's CA.
		if !walletURLRe.MatchString(u) {
			sources = append(sources, u)
		}
	}
	for _, s := range sources {
		if chains["evm"] {
			for _, m := range evmRe.FindAllString(s, -1) {
				add(m)
			}
		}
		if chains["solana"] {
			for _, m := range solRe.FindAllString(s, -1) {
				if looksLikeSolana(m) {
					add(m)
				}
			}
		}
	}
	return out
}

// walletURLRe matches explorer/profile links for wallets and transactions.
var walletURLRe = regexp.MustCompile(`(?i)/(address|addr|account|accounts|wallet|wallets|tx|txs|transaction|transactions|profile|portfolio|user|holder|holders)/`)

func caKey(ca string) string {
	if strings.HasPrefix(ca, "0x") {
		return strings.ToLower(ca)
	}
	return ca
}

// classify returns (isFlagged, matchedMarker).
func classify(report string, markers, safe []string) (bool, string) {
	t := strings.ToLower(report)
	for _, p := range safe {
		t = strings.ReplaceAll(t, p, " ")
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true, m
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Persistent state (seen CAs) and scan log
// ---------------------------------------------------------------------------

type seenStore struct {
	mu   sync.Mutex
	path string
	m    map[string]time.Time
}

func loadSeen(path string) *seenStore {
	s := &seenStore{path: path, m: map[string]time.Time{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

// markNew records ca and reports whether it was new.
func (s *seenStore) markNew(ca string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := caKey(ca)
	if _, ok := s.m[k]; ok {
		return false
	}
	s.m[k] = time.Now()
	b, _ := json.MarshalIndent(s.m, "", "  ")
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		log.Printf("warning: saving seen CAs: %v", err)
	}
	return true
}

type scanRecord struct {
	Time      time.Time    `json:"time"`
	CA        string       `json:"ca"`
	SourceMsg int          `json:"source_msg_id,omitempty"`
	CallMeta  *CallMeta    `json:"call_meta,omitempty"`
	Tools     []toolRecord `json:"tools"`
	Deliver   bool         `json:"should_deliver"`
	Delivered bool         `json:"delivered"`
	Error     string       `json:"error,omitempty"`
}

type toolRecord struct {
	Tool       string  `json:"tool"`
	Status     string  `json:"status"`
	Verdict    verdict `json:"verdict"`
	Error      string  `json:"error,omitempty"`
	ReportText string  `json:"report_text"`
}

func appendLog(path string, r scanRecord) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("warning: scan log: %v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
}

// ---------------------------------------------------------------------------
// Scanner
// ---------------------------------------------------------------------------

type job struct {
	CA         string
	SourceMsg  int       // id of the original call post in the source channel (0 = unknown)
	SourceText string    // its text (used for Bot-API delivery / forward fallback)
	Meta       *CallMeta // parsed post data (MCap, Liq, Tax, Age, Holders, Proof, live buys)
	CallID     *int      // scout_calls.id (nil when DB is off or no source post)
}

type scanner struct {
	cfg    *config
	api    *tg.Client
	sender *message.Sender
	seen   *seenStore
	logf   string
	db     *ScoutStore // nil when SCOUT_DATABASE_URL is not set

	sourceChannelID int64
	sourcePeer      tg.InputPeerClass
	notifyPeer      tg.InputPeerClass

	runners []*toolRunner // one per tool, in SCOUT_TOOLS order
	byBotMu sync.RWMutex
	byBot   map[int64]*toolRunner

	queue chan job

	listOnly bool // -list-chats: don't resolve the delivery target

	pc      priceConfig
	gecko   *geckoClient
	onchain *onchainSource

	latestMu    sync.Mutex
	latestRetry map[int]time.Time // latest-price pass: call id → not before (after a failed refresh)

	postMu      sync.Mutex
	handled     map[string]bool // "msgID|ca" (and "msgID" for CA-less posts) already processed
	sourceInput tg.InputChannelClass
}

func newScanner(cfg *config) *scanner {
	s := &scanner{
		cfg:     cfg,
		seen:    loadSeen(filepath.Join(cfg.StateDir, "seen_cas.json")),
		logf:    filepath.Join(cfg.StateDir, "scans.jsonl"),
		queue:   make(chan job, 500),
		byBot:   map[int64]*toolRunner{},
		handled: map[string]bool{},
	}
	for _, t := range cfg.Tools {
		s.runners = append(s.runners, newToolRunner(t))
	}
	s.pc = cfg.Price
	s.gecko = newGeckoClient(cfg.Price)
	s.onchain = newOnchainSource(cfg.Price.Onchain)
	return s
}

// onBotMessage is called for new and edited private messages; it routes a
// bot's reply to that bot's runner.
func (s *scanner) onBotMessage(msg *tg.Message) {
	if msg.Out {
		return
	}
	p, ok := msg.PeerID.(*tg.PeerUser)
	if !ok {
		return
	}
	s.byBotMu.RLock()
	r := s.byBot[p.UserID]
	s.byBotMu.RUnlock()
	if r != nil {
		r.onMessage(msg)
	}
}

// onChannelPost is called for each new message in the source channel.
// postURLs returns the links in a channel post: text links, link preview and
// inline buttons (calls often put the CA only behind a "Buy"/"Chart" button).
func postURLs(msg *tg.Message) []string {
	var urls []string
	for _, e := range msg.Entities {
		if u, ok := e.(*tg.MessageEntityTextURL); ok {
			urls = append(urls, u.URL)
		}
	}
	if wp, ok := msg.Media.(*tg.MessageMediaWebPage); ok {
		if page, ok := wp.Webpage.(*tg.WebPage); ok {
			urls = append(urls, page.URL)
		}
	}
	return append(urls, buttonURLs(msg)...)
}

// onChannelPost handles a new or edited post in the source channel. It can be
// called for the same post several times (live update, edit, polling); each
// (post, CA) pair is handled once.
func (s *scanner) onChannelPost(msg *tg.Message) {
	s.postMu.Lock()
	defer s.postMu.Unlock()
	urls := postURLs(msg)
	cas := extractCAs(msg.Message, urls, s.cfg.Chains)
	var fresh []string
	for _, ca := range cas {
		k := strconv.Itoa(msg.ID) + "|" + caKey(ca)
		if !s.handled[k] {
			s.handled[k] = true
			fresh = append(fresh, ca)
		}
	}
	if len(cas) == 0 {
		if !s.handled[strconv.Itoa(msg.ID)] {
			s.handled[strconv.Itoa(msg.ID)] = true
			log.Printf("post %d: no CA found (%d chars, %d links/buttons) — %s",
				msg.ID, len([]rune(msg.Message)), len(urls), s.sourceLink(msg.ID))
		}
		return
	}
	// An update post ("$TOKEN hit 3X …") is about an earlier call: it is recorded
	// and nothing else. It stays out of s.seen, so a later real call of the
	// token is still investigated.
	if postKind(msg.Message) == PostKindUpdate {
		for _, ca := range fresh {
			log.Printf("post %d: update for %s (not a call), skipping", msg.ID, ca)
			s.recordCall(msg, ca, urls, CallStatusUpdate)
		}
		return
	}
	for _, ca := range fresh {
		if !s.seen.markNew(ca) {
			log.Printf("post %d: %s already scanned, skipping", msg.ID, ca)
			s.recordCall(msg, ca, urls, CallStatusDuplicate)
			continue
		}
		callID := s.recordCall(msg, ca, urls, CallStatusQueued)
		log.Printf("post %d: queued %s", msg.ID, ca)
		select {
		case s.queue <- job{CA: ca, SourceMsg: msg.ID, SourceText: msg.Message, CallID: callID, Meta: metaOf(msg.Message)}:
		default:
			log.Printf("queue full, dropping %s", ca)
			s.setCallStatus(callID, CallStatusDropped)
		}
	}
}

// messagesOf returns the plain messages in a history/search result.
func messagesOf(res tg.MessagesMessagesClass) []*tg.Message {
	var raw []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesChannelMessages:
		raw = m.Messages
	case *tg.MessagesMessagesSlice:
		raw = m.Messages
	case *tg.MessagesMessages:
		raw = m.Messages
	}
	var out []*tg.Message
	for _, mc := range raw {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// fetchNewPosts returns source-channel posts with id > minID, oldest first.
func (s *scanner) fetchNewPosts(ctx context.Context, minID, limit int) ([]*tg.Message, error) {
	res, err := s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: s.sourcePeer, MinID: minID, Limit: limit})
	if err != nil {
		return nil, err
	}
	return messagesOf(res), nil
}

// fetchPosts returns specific source-channel posts by id.
func (s *scanner) fetchPosts(ctx context.Context, ids []int) ([]*tg.Message, error) {
	var in []tg.InputMessageClass
	for _, id := range ids {
		in = append(in, &tg.InputMessageID{ID: id})
	}
	res, err := s.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: s.sourceInput, ID: in})
	if err != nil {
		return nil, err
	}
	return messagesOf(res), nil
}

// poll is a safety net next to live updates: Telegram does not always push
// every post of a large channel to user accounts, so every PollInterval we ask
// for posts newer than the last one polled. Already-handled posts are skipped.
func (s *scanner) poll(ctx context.Context) {
	if s.cfg.PollInterval <= 0 {
		return
	}
	cursor := 0
	if top, err := s.fetchNewPosts(ctx, 0, 1); err != nil {
		log.Printf("poll: initial read of @%s failed: %v", s.cfg.SourceChannel, err)
	} else if len(top) > 0 {
		cursor = top[len(top)-1].ID // start from now; no backfill of old posts
	}
	log.Printf("polling @%s every %s as a backup (starting after post %d)", s.cfg.SourceChannel, s.cfg.PollInterval, cursor)
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if cursor == 0 {
			if top, err := s.fetchNewPosts(ctx, 0, 1); err == nil && len(top) > 0 {
				cursor = top[len(top)-1].ID
			}
			continue
		}
		msgs, err := s.fetchNewPosts(ctx, cursor, 50)
		if err != nil {
			log.Printf("poll: %v", err)
			continue
		}
		for _, m := range msgs {
			s.onChannelPost(m) // no-op if the live update already handled it
			if m.ID > cursor {
				cursor = m.ID
			}
		}
	}
}

func (s *scanner) sourceLink(msgID int) string {
	if msgID == 0 {
		return ""
	}
	return fmt.Sprintf("https://t.me/%s/%d", s.cfg.SourceChannel, msgID)
}

var levelIcon = map[string]string{levelClean: "✅", levelCaution: "🟡", levelRedFlags: "🚩", levelUnknown: "❔"}

// deliveryHeader summarises every tool's result for one CA.
func (s *scanner) deliveryHeader(j job, results []*toolResult) string {
	var gate *toolResult
	for _, r := range results {
		if r.Spec.Gate {
			gate = r
			break
		}
	}
	var b strings.Builder
	if gate != nil {
		v := gate.Verdict
		label := v.Label
		if label == "" {
			label = v.Level
		}
		fmt.Fprintf(&b, "%s %s", levelIcon[v.Level], strings.TrimSpace(v.Ticker+" "+label))
	} else {
		b.WriteString("🔎 New call")
	}
	fmt.Fprintf(&b, "\nCA: %s", j.CA)
	if j.Meta != nil {
		if line := j.Meta.SummaryLine(); line != "" {
			b.WriteString("\n" + line)
		}
	}
	for _, r := range results {
		v := r.Verdict
		line := fmt.Sprintf("\n• %s: ", r.Spec.Name)
		switch {
		case r.Status != investigationCompleted:
			line += "no report (" + r.Status + ")"
		case v.Level != levelUnknown:
			line += levelIcon[v.Level] + " " + firstNonEmpty(v.Label, v.Level)
			if v.Summary != "" {
				line += " — " + v.Summary
			}
		default:
			line += "report attached"
		}
		if v.URL != "" {
			line += "\n  " + v.URL
		}
		b.WriteString(line)
	}
	if l := s.sourceLink(j.SourceMsg); l != "" {
		b.WriteString("\nSource: " + l)
	}
	return b.String()
}

func firstNonEmpty(ss ...string) string {
	for _, x := range ss {
		if x != "" {
			return x
		}
	}
	return ""
}

// botAPIText builds the single text used for Bot-API delivery.
func (s *scanner) botAPIText(header string, j job, results []*toolResult) string {
	text := header
	if j.SourceText != "" {
		text += "\n\n━━ Original call (@" + s.cfg.SourceChannel + ") ━━\n" + j.SourceText
	}
	for _, r := range results {
		if r.Spec.Attach && r.Status == investigationCompleted {
			text += "\n\n━━ " + r.Spec.Name + " ━━\n" + r.ReportText()
		}
	}
	return text
}

// deliver sends, in order: the summary header, the original call post from the
// source channel, then every attachable tool's report.
func (s *scanner) deliver(ctx context.Context, j job, header string, results []*toolResult) error {
	if s.cfg.NotifyBotToken != "" {
		return botAPISend(ctx, s.cfg.NotifyBotToken, s.cfg.NotifyChatID, s.botAPIText(header, j, results))
	}

	if err := s.sendNotifyText(ctx, header); err != nil {
		return fmt.Errorf("send header: %w", err)
	}
	if j.SourceMsg != 0 && s.sourcePeer != nil {
		if _, err := s.sender.To(s.notifyPeer).ForwardIDs(s.sourcePeer, j.SourceMsg).Send(ctx); err != nil {
			// e.g. the channel restricts forwarding: send a copy instead.
			log.Printf("forward of original call failed (%v); sending text copy", err)
			copyText := "📣 Original call: " + s.sourceLink(j.SourceMsg)
			if j.SourceText != "" {
				copyText += "\n\n" + j.SourceText
			}
			if _, err := s.sender.To(s.notifyPeer).Text(ctx, copyText); err != nil {
				return fmt.Errorf("send original call: %w", err)
			}
		}
	}
	for _, r := range results {
		if !r.Spec.Attach || r.Status != investigationCompleted || len(r.Replies) == 0 {
			continue
		}
		runner := s.runnerFor(r.Spec.Code)
		ids := make([]int, 0, len(r.Replies))
		for _, m := range r.Replies {
			ids = append(ids, m.ID)
		}
		// Forward the bot's messages so formatting, links, buttons and files are preserved.
		_, err := s.sender.To(s.notifyPeer).ForwardIDs(runner.botPeer, ids[0], ids[1:]...).Send(ctx)
		if err != nil && len(ids) > 1 && r.Best != nil {
			// e.g. a "working…" message was deleted by the bot: forward just the report.
			_, err = s.sender.To(s.notifyPeer).ForwardIDs(runner.botPeer, r.Best.ID).Send(ctx)
		}
		if err != nil {
			log.Printf("[%s] forward failed (%v); sending text copy", r.Spec.Code, err)
			if _, err := s.sender.To(s.notifyPeer).Text(ctx, "━━ "+r.Spec.Name+" ━━\n"+r.ReportText()); err != nil {
				return fmt.Errorf("send %s report: %w", r.Spec.Name, err)
			}
		}
	}
	return nil
}

// findCallMessage looks up the most recent post in the source channel that
// mentions the CA (used by -scan, where there is no incoming post).
func (s *scanner) findCallMessage(ctx context.Context, ca string) (*tg.Message, error) {
	if s.sourcePeer == nil {
		return nil, nil
	}
	res, err := s.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer:   s.sourcePeer,
		Q:      ca,
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  10,
	})
	if err != nil {
		return nil, err
	}
	return pickCallMessage(res, ca, s.cfg.Chains), nil
}

// pickCallMessage returns the newest message that really contains the CA; a
// real call is preferred over an update post ("$TOKEN hit 3X …").
func pickCallMessage(res tg.MessagesMessagesClass, ca string, chains map[string]bool) *tg.Message {
	var msgs []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesChannelMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesMessages:
		msgs = m.Messages
	}
	var best *tg.Message
	bestCall := false
	for _, mc := range msgs {
		m, ok := mc.(*tg.Message)
		if !ok {
			continue
		}
		isCall := postKind(m.Message) == PostKindCall
		for _, found := range extractCAs(m.Message, postURLs(m), chains) {
			if caKey(found) != caKey(ca) {
				continue
			}
			if best == nil || (isCall && !bestCall) || (isCall == bestCall && m.ID > best.ID) {
				best, bestCall = m, isCall
			}
		}
	}
	return best
}

func (s *scanner) runnerFor(code string) *toolRunner {
	for _, r := range s.runners {
		if r.spec.Code == code {
			return r
		}
	}
	return nil
}

func botAPISend(ctx context.Context, token, chatID, text string) error {
	const limit = 4000
	for len(text) > 0 {
		chunk := text
		if len(chunk) > limit {
			chunk = chunk[:limit]
			text = text[limit:]
		} else {
			text = ""
		}
		body, _ := json.Marshal(map[string]any{
			"chat_id":                  chatID,
			"text":                     chunk,
			"disable_web_page_preview": true,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("bot API sendMessage: %s", resp.Status)
		}
	}
	return nil
}

// worker processes queued CAs one at a time (replies can't be correlated otherwise).
func (s *scanner) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.queue:
			s.process(ctx, j, !s.cfg.DryRun)
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.ScanGap):
			}
		}
	}
}

// investigateAll runs every tool for one CA in parallel.
func (s *scanner) investigateAll(ctx context.Context, ca string) []*toolResult {
	results := make([]*toolResult, len(s.runners))
	var wg sync.WaitGroup
	for i, r := range s.runners {
		wg.Add(1)
		go func(i int, r *toolRunner) {
			defer wg.Done()
			send := func(ctx context.Context, text string) error {
				_, err := s.sender.To(r.botPeer).Text(ctx, text)
				return err
			}
			res := r.investigateWithRetry(ctx, send, ca)
			if res.Status == investigationCompleted {
				res.Verdict = s.judge(ctx, r.spec, res.Replies)
			} else {
				res.Verdict = verdict{Level: levelUnknown}
			}
			results[i] = res
		}(i, r)
	}
	wg.Wait()
	return results
}

// shouldDeliver: every gating tool must have completed with an allowed verdict.
// With no gating tool configured, every CA with at least one report is delivered.
func (s *scanner) shouldDeliver(results []*toolResult) bool {
	gated, anyReport := false, false
	for _, r := range results {
		if r.Status == investigationCompleted {
			anyReport = true
		}
		if !r.Spec.Gate {
			continue
		}
		gated = true
		if r.Status != investigationCompleted || !s.cfg.DeliverLevels[r.Verdict.Level] {
			return false
		}
	}
	if !gated {
		return anyReport
	}
	return true
}

func (s *scanner) process(ctx context.Context, j job, deliver bool) []*toolResult {
	rec := scanRecord{Time: time.Now(), CA: j.CA, SourceMsg: j.SourceMsg, CallMeta: j.Meta}
	defer func() { appendLog(s.logf, rec) }()

	results := s.investigateAll(ctx, j.CA)

	invIDs := map[string]*int{} // tool code -> scout_investigations.id
	anyOK := false
	for _, r := range results {
		tr := toolRecord{Tool: r.Spec.Code, Status: r.Status, Verdict: r.Verdict, ReportText: r.ReportText()}
		if r.Err != nil {
			tr.Error = r.Err.Error()
			log.Printf("[%s] %s: %v", r.Spec.Code, j.CA, r.Err)
		} else {
			anyOK = true
			log.Printf("[%s] %s: %s (%s via %s)", r.Spec.Code, j.CA, r.Verdict.Level, r.Verdict.Label, r.Verdict.Source)
		}
		rec.Tools = append(rec.Tools, tr)
		invIDs[r.Spec.Code] = s.recordInvestigation(j, r)
	}
	if anyOK {
		s.setCallStatus(j.CallID, CallStatusScanned)
	} else {
		s.setCallStatus(j.CallID, CallStatusFailed)
	}

	// Model score: stored for every call, shown in the delivery header. Never
	// filters and never blocks: no line when the service is off or down.
	scoreLine := s.scoreCall(ctx, j)

	rec.Deliver = s.shouldDeliver(results)
	if !rec.Deliver {
		log.Printf("%s: gate verdict not in SCOUT_DELIVER_LEVELS — not delivering", j.CA)
		return results
	}
	if !deliver {
		return results
	}
	header := s.deliveryHeader(j, results)
	if scoreLine != "" {
		header += "\n" + scoreLine
	}
	err := s.deliver(ctx, j, header, results)
	var attached []int
	for _, r := range results {
		if id := invIDs[r.Spec.Code]; id != nil && r.Spec.Attach && r.Status == investigationCompleted {
			attached = append(attached, *id)
		}
	}
	if err != nil {
		rec.Error = "deliver: " + err.Error()
		log.Printf("deliver %s failed: %v", j.CA, err)
	} else {
		rec.Delivered = true
		log.Printf("delivered %s (%d report(s))", j.CA, len(attached))
	}
	s.recordDelivery(j, header, attached, err)
	return results
}

// ---------------------------------------------------------------------------
// SQL recording (no-ops when SCOUT_DATABASE_URL is not set; DB errors are
// logged but never stop scanning)
// ---------------------------------------------------------------------------

func (s *scanner) recordCall(msg *tg.Message, ca string, urls []string, status string) *int {
	id, _ := s.recordCallInfo(msg, ca, urls, status)
	return id
}

// recordCallInfo records a call (idempotent) plus its parsed data and price
// tracking row; created reports whether the call was new. An update post is
// recorded with status "update" whatever status was asked for, and gets no
// parsed data and no tracking row.
func (s *scanner) recordCallInfo(msg *tg.Message, ca string, urls []string, status string) (*int, bool) {
	if s.db == nil {
		return nil, false
	}
	update := postKind(msg.Message) == PostKindUpdate
	if update {
		status = CallStatusUpdate
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, created, err := s.db.UpsertScoutCall(ctx, &ScoutCall{
		ChannelID:       s.sourceChannelID,
		ChannelUsername: s.cfg.SourceChannel,
		MessageID:       msg.ID,
		MessageDate:     time.Unix(int64(msg.Date), 0).UTC(),
		MessageText:     msg.Message,
		URLs:            urls,
		ContractAddress: ca,
		Chain:           chainOf(ca),
		Status:          status,
	})
	if err != nil {
		log.Printf("db: insert scout_calls %s: %v", ca, err)
		return nil, false
	}
	if update {
		return id, created
	}
	if meta := metaOf(msg.Message); meta != nil {
		if err := s.db.UpsertCallMetrics(ctx, *id, meta); err != nil {
			log.Printf("db: call metrics for post %d: %v", msg.ID, err)
		}
	}
	if s.pc.Enabled && len(s.pc.Horizons) > 0 {
		entry := time.Unix(int64(msg.Date), 0).UTC()
		priority := 0
		if status == CallStatusBackfill {
			priority = 1
		}
		if err := s.db.EnsureTracking(ctx, *id, ca, entry, priority, s.firstCheckAt(entry)); err != nil {
			log.Printf("db: tracking for post %d: %v", msg.ID, err)
		}
	}
	return id, created
}

// metaOf parses the call post; nil when it isn't in the call format.
func metaOf(text string) *CallMeta {
	if m, ok := parseCallMeta(text); ok {
		return m
	}
	return nil
}

func (s *scanner) setCallStatus(id *int, status string) {
	if s.db == nil || id == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.db.UpdateScoutCallStatus(ctx, *id, status); err != nil {
		log.Printf("db: update scout_calls %d: %v", *id, err)
	}
}

func (s *scanner) recordInvestigation(j job, r *toolResult) *int {
	if s.db == nil {
		return nil
	}
	runner := s.runnerFor(r.Spec.Code)
	if runner == nil || runner.toolID == nil {
		return nil
	}
	done := r.CompletedAt
	row := &ScoutInvestigation{
		CallID:          j.CallID,
		ToolID:          *runner.toolID,
		ContractAddress: j.CA,
		RequestText:     r.Command,
		RequestedAt:     r.RequestedAt,
		CompletedAt:     &done,
		Status:          r.Status,
		BotMessageIDs:   r.MessageIDs(),
		ReportText:      r.ReportText(),
		ReportURLs:      replyURLs(r.Replies),
		ReportURL:       strPtr(r.Verdict.URL),
		VerdictLevel:    r.Verdict.Level,
		VerdictLabel:    strPtr(r.Verdict.Label),
		Ticker:          strPtr(r.Verdict.Ticker),
		VerdictSummary:  strPtr(r.Verdict.Summary),
		VerdictSource:   strPtr(r.Verdict.Source),
		Details:         replyDetails(r.Replies, r.Attempts, r.RateLimitWaits),
	}
	if id := perceptorID(r.Replies...); id != "" && r.Spec.Parser == parserPerceptor {
		row.ExternalID = &id
		if row.ReportURL == nil {
			row.ReportURL = strPtr("https://www.perceptor.info/r/" + id)
		}
	}
	if r.Err != nil {
		row.Error = strPtr(r.Err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := s.db.InsertScoutInvestigation(ctx, row)
	if err != nil {
		log.Printf("db: insert scout_investigations %s/%s: %v", r.Spec.Code, j.CA, err)
		return nil
	}
	return id
}

func (s *scanner) recordDelivery(j job, header string, investigationIDs []int, sendErr error) {
	if s.db == nil {
		return
	}
	d := &ScoutDelivery{
		CallID:           j.CallID,
		ContractAddress:  j.CA,
		Target:           deliveryLabel(s.cfg),
		Status:           DeliveryStatusSent,
		HeaderText:       header,
		InvestigationIDs: investigationIDs,
	}
	if sendErr != nil {
		d.Status, d.Error = DeliveryStatusFailed, strPtr(sendErr.Error())
	} else {
		now := time.Now().UTC()
		d.DeliveredAt = &now
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.InsertScoutDelivery(ctx, d); err != nil {
		log.Printf("db: insert scout_deliveries %s: %v", j.CA, err)
	}
}

// replyDetails captures non-text parts of the bot's replies (files, photos,
// buttons) as JSON for scout_investigations.details.
func replyDetails(msgs []*tg.Message, attempts int, rateLimitWaits []int) []byte {
	type media struct {
		MessageID int    `json:"message_id"`
		Type      string `json:"type"`
		MimeType  string `json:"mime_type,omitempty"`
		FileName  string `json:"file_name,omitempty"`
	}
	type button struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		URL       string `json:"url,omitempty"`
	}
	var d struct {
		Attempts       int      `json:"attempts,omitempty"`
		RateLimitWaits []int    `json:"rate_limit_waits_s,omitempty"`
		Media          []media  `json:"media,omitempty"`
		Buttons        []button `json:"buttons,omitempty"`
	}
	d.Attempts, d.RateLimitWaits = attempts, rateLimitWaits
	for _, m := range msgs {
		switch md := m.Media.(type) {
		case *tg.MessageMediaPhoto:
			d.Media = append(d.Media, media{MessageID: m.ID, Type: "photo"})
		case *tg.MessageMediaDocument:
			x := media{MessageID: m.ID, Type: "document"}
			if doc, ok := md.Document.(*tg.Document); ok {
				x.MimeType = doc.MimeType
				for _, a := range doc.Attributes {
					if fn, ok := a.(*tg.DocumentAttributeFilename); ok {
						x.FileName = fn.FileName
					}
				}
			}
			d.Media = append(d.Media, x)
		}
		if kb, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
			for _, row := range kb.Rows {
				for _, b := range row.Buttons {
					switch bt := b.(type) {
					case *tg.KeyboardButtonURL:
						d.Buttons = append(d.Buttons, button{MessageID: m.ID, Text: bt.Text, URL: bt.URL})
					case *tg.KeyboardButtonCallback:
						d.Buttons = append(d.Buttons, button{MessageID: m.ID, Text: bt.Text})
					}
				}
			}
		}
	}
	b, _ := json.Marshal(d)
	return b
}

var httpURLRe = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

// replyURLs returns every distinct http(s) URL in the bot's replies.
func replyURLs(msgs []*tg.Message) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range msgs {
		for _, src := range messageURLs(m) {
			for _, u := range httpURLRe.FindAllString(src, -1) {
				if !seen[u] {
					seen[u] = true
					out = append(out, u)
				}
			}
		}
	}
	return out
}

// judge decides a tool's verdict. Perceptor: the official verdict on the linked
// perceptor.info page, falling back to the message text. Other tools: text.
func (s *scanner) judge(ctx context.Context, spec ToolSpec, replies []*tg.Message) verdict {
	if spec.Parser == parserPerceptor {
		if id := perceptorID(replies...); id != "" {
			v, err := fetchPerceptorVerdict(ctx, id)
			if err == nil {
				return v
			}
			log.Printf("[%s] perceptor page %s: %v — falling back to message text", spec.Code, id, err)
		}
	}
	var text strings.Builder
	for _, m := range replies {
		text.WriteString(m.Message)
		text.WriteString("\n")
	}
	return textVerdict(text.String(), s.cfg.RedFlagMarkers, s.cfg.SafePhrases)
}

// ---------------------------------------------------------------------------
// Peer resolution helpers
// ---------------------------------------------------------------------------

func (s *scanner) resolveSource(ctx context.Context) (*tg.Channel, error) {
	res, err := s.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: s.cfg.SourceChannel})
	if err != nil {
		return nil, fmt.Errorf("resolve @%s: %w", s.cfg.SourceChannel, err)
	}
	for _, c := range res.Chats {
		if ch, ok := c.(*tg.Channel); ok {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("@%s is not a channel", s.cfg.SourceChannel)
}

func (s *scanner) resolveBot(ctx context.Context, username string) (*tg.User, error) {
	res, err := s.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("resolve @%s: %w", username, err)
	}
	for _, u := range res.Users {
		if user, ok := u.(*tg.User); ok {
			return user, nil
		}
	}
	return nil, fmt.Errorf("@%s is not a user/bot", username)
}

// resolveNotify turns SCOUT_NOTIFY_PEER into an input peer. Accepts:
//   - "me" / "self"              → Saved Messages
//   - numeric id                 → e.g. -1001234567890 or -4012345678 (Bot-API style) or the raw id
//   - "@username"                → public user/group/channel
//   - a chat title               → e.g. "scout analytics" (private groups have no username);
//     matched case-insensitively against your chats
func (s *scanner) resolveNotify(ctx context.Context) (tg.InputPeerClass, string, error) {
	p := strings.TrimSpace(s.cfg.NotifyPeer)
	if p == "" || strings.EqualFold(p, "me") || strings.EqualFold(p, "self") {
		return &tg.InputPeerSelf{}, "Saved Messages", nil
	}
	if m := inviteLinkRe.FindStringSubmatch(p); m != nil {
		return s.resolveInvite(ctx, m[1])
	}
	if id, err := strconv.ParseInt(p, 10, 64); err == nil {
		return s.findDialog(ctx, func(d dialogInfo) bool { return d.ID == normalizeChatID(id) }, p)
	}
	if strings.HasPrefix(p, "@") || usernameRe.MatchString(p) {
		peer, err := s.sender.Resolve(strings.TrimPrefix(p, "@")).AsInputPeer(ctx)
		if err == nil || strings.HasPrefix(p, "@") {
			return peer, p, err
		}
		// not a username after all: fall through to a title match
	}
	want := strings.ToLower(p)
	return s.findDialog(ctx, func(d dialogInfo) bool { return strings.ToLower(strings.TrimSpace(d.Title)) == want }, p)
}

var usernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)

// inviteLinkRe matches private invite links: https://t.me/+HASH, t.me/joinchat/HASH, tg://join?invite=HASH, +HASH.
var inviteLinkRe = regexp.MustCompile(`(?:t\.me/\+|t\.me/joinchat/|join\?invite=|^\+)([A-Za-z0-9_-]{8,})`)

// chatPeer converts a chat from an API result into an input peer + description.
func chatPeer(c tg.ChatClass) (tg.InputPeerClass, string, bool) {
	switch ch := c.(type) {
	case *tg.Channel:
		kind := "channel"
		if ch.Megagroup {
			kind = "supergroup"
		}
		return ch.AsInputPeer(), fmt.Sprintf("%s %q (id %d)", kind, ch.Title, ch.ID), true
	case *tg.Chat:
		if ch.MigratedTo != nil {
			return nil, "", false
		}
		return &tg.InputPeerChat{ChatID: ch.ID}, fmt.Sprintf("group %q (id %d)", ch.Title, ch.ID), true
	}
	return nil, "", false
}

// resolveInvite finds the chat behind a private invite link, joining it if
// this account isn't a member yet.
func (s *scanner) resolveInvite(ctx context.Context, hash string) (tg.InputPeerClass, string, error) {
	inv, err := s.api.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, "", fmt.Errorf("invite link: %w", err)
	}
	var chat tg.ChatClass
	switch x := inv.(type) {
	case *tg.ChatInviteAlready:
		chat = x.Chat
	case *tg.ChatInvitePeek:
		chat = x.Chat
	case *tg.ChatInvite:
		log.Printf("not a member of %q yet — joining via the invite link", x.Title)
		upd, err := s.api.MessagesImportChatInvite(ctx, hash)
		if err != nil && !tgerr.Is(err, "USER_ALREADY_PARTICIPANT") {
			return nil, "", fmt.Errorf("join via invite link: %w", err)
		}
		var chats []tg.ChatClass
		switch u := upd.(type) {
		case *tg.Updates:
			chats = u.Chats
		case *tg.UpdatesCombined:
			chats = u.Chats
		}
		for _, c := range chats {
			if p, label, ok := chatPeer(c); ok {
				return p, label, nil
			}
		}
		// Joined but the response had no chat: look it up by title.
		want := strings.ToLower(strings.TrimSpace(x.Title))
		return s.findDialog(ctx, func(d dialogInfo) bool { return strings.ToLower(strings.TrimSpace(d.Title)) == want }, x.Title)
	}
	if p, label, ok := chatPeer(chat); ok {
		return p, label, nil
	}
	return nil, "", fmt.Errorf("invite link points to an unusable chat (%T)", chat)
}

// isPeerError: the chat id we hold is no longer valid (group upgraded to a
// supergroup, left, re-created, …) and should be looked up again.
func isPeerError(err error) bool {
	return tgerr.Is(err, "PEER_ID_INVALID", "CHAT_ID_INVALID", "CHANNEL_INVALID", "CHANNEL_PRIVATE", "CHAT_FORBIDDEN")
}

// sendNotifyText sends text to the delivery chat; if Telegram says the chat id
// is invalid, it looks the chat up again once and retries.
func (s *scanner) sendNotifyText(ctx context.Context, text string) error {
	_, err := s.sender.To(s.notifyPeer).Text(ctx, text)
	if err == nil || !isPeerError(err) {
		return err
	}
	log.Printf("delivery chat rejected (%v) — looking up SCOUT_NOTIFY_PEER=%q again", err, s.cfg.NotifyPeer)
	peer, label, rerr := s.resolveNotify(ctx)
	if rerr != nil {
		return fmt.Errorf("%w (re-resolve failed: %v)", err, rerr)
	}
	s.notifyPeer = peer
	log.Printf("reports will be delivered to %s", label)
	_, err = s.sender.To(s.notifyPeer).Text(ctx, text)
	return err
}

// normalizeChatID converts Bot-API style ids (-100<channel>, -<chat>) to raw MTProto ids.
func normalizeChatID(id int64) int64 {
	if id >= 0 {
		return id
	}
	str := strconv.FormatInt(-id, 10)
	if strings.HasPrefix(str, "100") && len(str) > 12 {
		str = str[3:]
	}
	raw, _ := strconv.ParseInt(str, 10, 64)
	return raw
}

type dialogInfo struct {
	ID    int64
	Title string
	Kind  string // group | supergroup | channel | user
	Peer  tg.InputPeerClass
}

// listDialogs returns your chats (groups, channels, users), skipping dead or
// migrated basic groups.
func (s *scanner) listDialogs(ctx context.Context) ([]dialogInfo, error) {
	var out []dialogInfo
	seen := map[string]bool{}
	offsetDate, offsetID := 0, 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	for page := 0; page < 50; page++ {
		d, err := s.api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: 100,
		})
		if err != nil {
			return nil, err
		}
		md, ok := d.AsModified()
		if !ok {
			break
		}
		add := func(di dialogInfo) {
			k := di.Kind + strconv.FormatInt(di.ID, 10)
			if !seen[k] {
				seen[k] = true
				out = append(out, di)
			}
		}
		for _, c := range md.GetChats() {
			switch ch := c.(type) {
			case *tg.Channel:
				if ch.Left {
					continue // you're not in it any more
				}
				kind := "channel"
				if ch.Megagroup {
					kind = "supergroup"
				}
				add(dialogInfo{ID: ch.ID, Title: ch.Title, Kind: kind, Peer: ch.AsInputPeer()})
			case *tg.Chat:
				if ch.Left || ch.Deactivated || ch.MigratedTo != nil {
					continue // left, or upgraded to a supergroup (the Channel entry is the live one)
				}
				add(dialogInfo{ID: ch.ID, Title: ch.Title, Kind: "group", Peer: &tg.InputPeerChat{ChatID: ch.ID}})
			}
		}
		for _, u := range md.GetUsers() {
			if user, ok := u.(*tg.User); ok {
				name := strings.TrimSpace(user.FirstName + " " + user.LastName)
				add(dialogInfo{ID: user.ID, Title: name, Kind: "user", Peer: user.AsInputPeer()})
			}
		}
		msgs := md.GetMessages()
		if len(md.GetDialogs()) < 100 || len(msgs) == 0 {
			break
		}
		if m, ok := msgs[len(msgs)-1].(*tg.Message); ok {
			offsetDate, offsetID = m.Date, m.ID
		} else {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return out, nil
}

func (s *scanner) findDialog(ctx context.Context, match func(dialogInfo) bool, what string) (tg.InputPeerClass, string, error) {
	all, err := s.listDialogs(ctx)
	if err != nil {
		return nil, "", err
	}
	var hits []dialogInfo
	for _, d := range all {
		if d.Kind != "user" && match(d) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 0 { // allow matching a person/bot too, but prefer chats
		for _, d := range all {
			if match(d) {
				hits = append(hits, d)
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, "", fmt.Errorf("no chat matching %q in your dialogs — run with -list-chats to see names and ids", what)
	case 1:
		h := hits[0]
		return h.Peer, fmt.Sprintf("%s %q (id %d)", h.Kind, h.Title, h.ID), nil
	default:
		var names []string
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%s %q id=%d", h.Kind, h.Title, h.ID))
		}
		return nil, "", fmt.Errorf("%d chats match %q: %s — set SCOUT_NOTIFY_PEER to the id instead", len(hits), what, strings.Join(names, "; "))
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	envFile := flag.String("env", ".env", "path to .env file")
	oneShot := flag.String("scan", "", "investigate a single CA with every tool, print the reports and deliver them (if allowed), then exit")
	_ = flag.Bool("deliver", true, "deprecated: -scan now delivers by default (use -no-deliver to only print)")
	noDeliver := flag.Bool("no-deliver", false, "with -scan / -post: print the reports only, don't send them")
	dryRun := flag.Bool("dry-run", false, "listen and scan, but never deliver (verdicts are logged)")
	backfillFlag := flag.Bool("backfill", false, "import past calls from the channel history into the DB (no bot scans) so their performance can be tracked, then exit")
	backfillFrom := flag.Int("backfill-from", 1, "with -backfill: oldest post id to import")
	backfillTo := flag.Int("backfill-to", 0, "with -backfill: newest post id to import (0 = latest)")
	backfillMax := flag.Int("backfill-max", 0, "with -backfill: stop after this many posts (0 = no limit)")
	trackOnly := flag.Bool("track", false, "run only the performance tracker (no Telegram), forever")
	trackOnce := flag.Bool("track-once", false, "process the price checks that are due now, then exit (no Telegram)")
	priceCheck := flag.String("price-check", "", "on-chain diagnostic for a CA: find its pool and print entry/current price (no DB, no Telegram), then exit")
	priceAt := flag.String("price-at", "", "with -price-check: the call time, RFC3339 (e.g. 2026-10-01T14:30:00Z) or a duration ago (e.g. 6h); default 1h ago")
	exportPath := flag.String("export-dataset", "", "write the training dataset (one row per call: features + 1h/1d/3d/7d/30d outcomes) to this CSV file, then exit")
	testNotify := flag.Bool("test-notify", false, "send one test message to SCOUT_NOTIFY_PEER and exit")
	postFlag := flag.String("post", "", "process specific @scoutrobinhood post id(s), e.g. 10002 or 10002,10005: show what was found, investigate and deliver, then exit")
	listenOnly := flag.Bool("listen-only", false, "listener without the performance tracker: new calls are still scanned, delivered and queued for tracking; run -track in another terminal to compute performance")
	webOnly := flag.Bool("web", false, "run only the read-only website and its JSON API (no Telegram), forever; address from SCOUT_WEB_ADDR (default :8090)")
	listChats := flag.Bool("list-chats", false, "print your groups/channels with their ids (for SCOUT_NOTIFY_PEER), then exit")
	retryNoPool := flag.Bool("retry-no-pool", false, "put first calls with tracking status no_pool back to pending, due now (a running tracker picks them up), then exit (DB only)")
	retryGaveUp := flag.Bool("retry-gave-up", false, "with -retry-no-pool: also gave_up calls")
	retryLaunchpad := flag.String("retry-launchpad", "", "with -retry-no-pool: only calls whose launchpad or dex is one of these, comma-separated, e.g. pons_v2 (case, spaces and _ ignored)")
	retryDryRun := flag.Bool("retry-dry-run", false, "with -retry-no-pool: only print what would be reset")
	flag.Parse()
	if !*retryNoPool && (*retryGaveUp || *retryLaunchpad != "" || *retryDryRun) {
		log.Fatal("-retry-gave-up, -retry-launchpad and -retry-dry-run are used with -retry-no-pool")
	}

	cfg, err := loadConfig(*envFile)
	if err != nil {
		log.Fatal(err)
	}
	cfg.DryRun = *dryRun

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *priceCheck != "" {
		if err := runPriceCheck(ctx, cfg, strings.TrimSpace(*priceCheck), *priceAt); err != nil {
			log.Fatal(err)
		}
		return
	}

	s := newScanner(cfg)
	db, dbSource, err := openScoutStore(ctx, cfg)
	if err != nil {
		log.Fatalf("database: %v (set SCOUT_DB=off to run without recording)", err)
	}
	if db != nil {
		defer db.Close()
		if cfg.DBAutoMigrate {
			if err := db.Migrate(ctx); err != nil {
				log.Fatalf("database migrate: %v", err)
			}
		}
		s.db = db
		if err := s.registerTools(ctx); err != nil {
			log.Fatalf("database: register tools: %v", err)
		}
		log.Printf("recording to SQL via %s → %s (tables scout_calls, scout_investigations, scout_deliveries, …)",
			dbSource, db.Describe(ctx))
	} else {
		log.Println("SCOUT_DB=off — not recording to SQL")
	}

	// Modes that need only the database (no Telegram login).
	if *exportPath != "" || *trackOnly || *trackOnce || *webOnly || *retryNoPool {
		if s.db == nil {
			log.Fatal("these modes need the database (SCOUT_DB=off is set)")
		}
		switch {
		case *retryNoPool:
			f := RetryFilter{GaveUp: *retryGaveUp, Launchpads: parseLaunchpads(*retryLaunchpad)}
			if *retryLaunchpad != "" && len(f.Launchpads) == 0 {
				log.Fatalf("-retry-launchpad %q has no usable value", *retryLaunchpad)
			}
			if err := runRetryNoPool(ctx, s.db, os.Stdout, f, *retryDryRun); err != nil {
				log.Fatalf("retry: %v", err)
			}
		case *exportPath != "":
			f, err := os.Create(*exportPath)
			if err != nil {
				log.Fatal(err)
			}
			n, err := s.db.ExportDatasetCSV(ctx, f)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("wrote %d calls to %s\n", n, *exportPath)
		case *trackOnce:
			if !s.pc.Enabled {
				log.Fatal("SCOUT_TRACK_PERFORMANCE=false")
			}
			total := 0
			for {
				n := s.trackDue(ctx, trackBatch)
				total += n
				if n < trackBatch || ctx.Err() != nil {
					break
				}
			}
			s.logTrackingStatus(context.Background(), total)
			s.fillTokenNames(ctx)
			s.refreshLatest(ctx, false) // one pass: up to SCOUT_LATEST_BATCH latest prices
			fmt.Printf("processed %d call(s)\n", total)
		case *trackOnly:
			if !s.pc.Enabled {
				log.Fatal("SCOUT_TRACK_PERFORMANCE=false")
			}
			s.trackLoop(ctx)
		case *webOnly:
			classifyPosts(ctx, s.db)
			if err := runWeb(ctx, s.db, loadWebConfig()); err != nil {
				log.Fatalf("web: %v", err)
			}
		}
		return
	}

	if *backfillFlag {
		if s.db == nil {
			log.Fatal("-backfill needs the database (SCOUT_DB=off is set)")
		}
		s.listOnly = true // no delivery target needed
		if err := run(ctx, s, func(ctx context.Context) error {
			st, err := s.backfill(ctx, *backfillFrom, *backfillTo, *backfillMax)
			fmt.Printf("backfill: %d posts read, %d calls, %d CAs (%d new, %d already recorded), %d update post(s) (not calls)\n",
				st.Posts, st.Calls, st.CAs, st.New, st.Existing, st.Updates)
			if err == nil && s.pc.Enabled {
				fmt.Println("their performance is filled in by the tracker (running listener, or -track / -track-once)")
			}
			return err
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *listChats {
		s.listOnly = true
		if err := run(ctx, s, func(ctx context.Context) error {
			all, err := s.listDialogs(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%-11s %-15s %s\n", "KIND", "ID", "TITLE")
			for _, d := range all {
				if d.Kind != "user" {
					fmt.Printf("%-11s %-15d %s\n", d.Kind, d.ID, d.Title)
				}
			}
			fmt.Println("\nSet SCOUT_NOTIFY_PEER in .env to a title (e.g. \"scout analytics\") or an id from this list.")
			return nil
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *testNotify {
		if err := run(ctx, s, func(ctx context.Context) error {
			if cfg.NotifyBotToken != "" {
				return botAPISend(ctx, cfg.NotifyBotToken, cfg.NotifyChatID, "✅ scoutanalytics test message")
			}
			if err := s.sendNotifyText(ctx, "✅ scoutanalytics test message — reports will arrive here"); err != nil {
				return fmt.Errorf("test message to %q failed: %w", cfg.NotifyPeer, err)
			}
			fmt.Println("test message sent — check the chat")
			return nil
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *postFlag != "" {
		var ids []int
		for _, p := range strings.Split(*postFlag, ",") {
			p = strings.TrimSpace(p)
			if i := strings.LastIndex(p, "/"); i >= 0 { // accept https://t.me/scoutrobinhood/10002
				p = p[i+1:]
			}
			id, err := strconv.Atoi(p)
			if err != nil {
				log.Fatalf("-post: %q is not a post id", p)
			}
			ids = append(ids, id)
		}
		if err := run(ctx, s, func(ctx context.Context) error {
			posts, err := s.fetchPosts(ctx, ids)
			if err != nil {
				return err
			}
			if len(posts) == 0 {
				return fmt.Errorf("post(s) %v not found in @%s", ids, cfg.SourceChannel)
			}
			for _, m := range posts {
				urls := postURLs(m)
				cas := extractCAs(m.Message, urls, cfg.Chains)
				fmt.Printf("===== post %d %s =====\n%s\n", m.ID, s.sourceLink(m.ID), m.Message)
				fmt.Printf("links/buttons (%d):\n", len(urls))
				for _, u := range urls {
					fmt.Println("  ", u)
				}
				fmt.Printf("CAs found: %v\n", cas)
				meta := metaOf(m.Message)
				if meta != nil {
					pretty, _ := json.MarshalIndent(meta, "", "  ")
					fmt.Printf("parsed call data:\n%s\n%s\n\n", pretty, meta.SummaryLine())
				} else {
					fmt.Print("parsed call data: none (post isn't in the call format)\n\n")
				}
				if postKind(m.Message) == PostKindUpdate {
					fmt.Print("post kind: update (about an earlier call, not a call) — recorded, not investigated\n\n")
					for _, ca := range cas {
						s.recordCall(m, ca, urls, CallStatusUpdate)
					}
					continue
				}
				for _, ca := range cas {
					s.seen.markNew(ca)
					j := job{CA: ca, SourceMsg: m.ID, SourceText: m.Message, Meta: meta, CallID: s.recordCall(m, ca, urls, CallStatusQueued)}
					printResults(s, s.process(ctx, j, !*noDeliver), *noDeliver)
				}
			}
			return nil
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *oneShot != "" {
		if err := run(ctx, s, func(ctx context.Context) error {
			j := job{CA: strings.TrimSpace(*oneShot)}
			if msg, err := s.findCallMessage(ctx, j.CA); err != nil {
				log.Printf("looking up the original call in @%s: %v", cfg.SourceChannel, err)
			} else if msg != nil {
				j.SourceMsg, j.SourceText, j.Meta = msg.ID, msg.Message, metaOf(msg.Message)
				j.CallID = s.recordCall(msg, j.CA, postURLs(msg), CallStatusQueued)
				log.Printf("original call: %s", s.sourceLink(msg.ID))
			} else {
				log.Printf("no post mentioning %s found in @%s; delivering without the original call", j.CA, cfg.SourceChannel)
			}
			printResults(s, s.process(ctx, j, !*noDeliver), *noDeliver)
			return nil
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Long-running mode with reconnect/backoff.
	backoff := 5 * time.Second
	for {
		start := time.Now()
		err := run(ctx, s, func(ctx context.Context) error {
			go s.worker(ctx)
			go s.poll(ctx)
			if *listenOnly {
				log.Printf("listen-only: the performance tracker is not running in this process (new calls are still queued; run -track separately)")
			} else {
				go s.trackLoop(ctx)
			}
			var names []string
			for _, t := range cfg.Tools {
				g := ""
				if t.Gate {
					g = " [gate]"
				}
				names = append(names, fmt.Sprintf("@%s %q%s", t.Bot, t.Command, g))
			}
			log.Printf("listening to @%s → %s → deliver to %s (dry-run=%v)",
				cfg.SourceChannel, strings.Join(names, ", "), deliveryLabel(cfg), cfg.DryRun)
			<-ctx.Done()
			return ctx.Err()
		})
		if ctx.Err() != nil {
			log.Println("shutting down")
			return
		}
		if time.Since(start) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		log.Printf("client stopped: %v — reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}

// openScoutStore picks the database:
//   - SCOUT_DATABASE_URL set → that DSN
//   - SCOUT_DB=off           → no recording
//   - otherwise              → the repo's database package (database.SetupDatabase:
//     DB_USER, DB_PASS, DB_NAME_DEV, APP_ENV, GETH_HOST_PATH / HOST_SECRET_PATH, SSL_CERT_FILE_PATH),
//     i.e. the same connection the API uses.
func openScoutStore(ctx context.Context, cfg *config) (*ScoutStore, string, error) {
	if cfg.DatabaseURL != "" {
		st, err := NewScoutStore(ctx, cfg.DatabaseURL)
		return st, "SCOUT_DATABASE_URL", err
	}
	switch cfg.DBMode {
	case "off", "none", "false", "0":
		return nil, "", nil
	case "repo", "":
	default:
		return nil, "", fmt.Errorf("unknown SCOUT_DB=%q (use repo or off)", cfg.DBMode)
	}
	pool, err := setupRepoDatabase()
	if err != nil {
		return nil, "", err
	}
	if pool == nil {
		return nil, "", errors.New("database.SetupDatabase returned no connection pool")
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		return nil, "", fmt.Errorf("ping via database.SetupDatabase: %w", err)
	}
	return NewScoutStoreFromPool(pool), "database.SetupDatabase (DB_USER, APP_ENV=" + os.Getenv("APP_ENV") + ")", nil
}

// setupRepoDatabase calls the repo's database.SetupDatabase, turning its panic
// on a failed ping into an error.
func setupRepoDatabase() (pool *pgxpool.Pool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("database.SetupDatabase: %v", r)
		}
	}()
	database.DbConn, database.DbConnPgx, err = database.SetupDatabase()
	return database.DbConnPgx, err
}

// registerTools upserts the configured tools into scout_investigation_tools and
// marks tools no longer in SCOUT_TOOLS as inactive.
func (s *scanner) registerTools(ctx context.Context) error {
	var codes []string
	for _, r := range s.runners {
		t := r.spec
		id, err := s.db.UpsertInvestigationTool(ctx, &ScoutInvestigationTool{
			Code: t.Code, Name: t.Name, BotUsername: t.Bot, CommandTemplate: t.Command,
			Parser: t.Parser, IsGate: t.Gate, IsActive: true,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", t.Code, err)
		}
		r.toolID = id
		codes = append(codes, t.Code)
	}
	return s.db.SetActiveInvestigationTools(ctx, codes)
}

// runPriceCheck exercises the on-chain price source for one token and prints
// every step, to verify the RPC node, pool discovery and USD conversion.
func runPriceCheck(ctx context.Context, cfg *config, token, at string) error {
	o := newOnchainSource(cfg.Price.Onchain)
	when := time.Now().Add(-time.Hour)
	if at != "" {
		if d, err := parseHorizon(at); err == nil {
			when = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, at); err == nil {
			when = t
		} else {
			return fmt.Errorf("-price-at %q: use RFC3339 or a duration like 6h / 3d", at)
		}
	}
	fmt.Println("source:", o.describe())
	latest, err := o.rpc.blockNumber(ctx)
	if err != nil {
		return fmt.Errorf("RPC %s: %w", cfg.Price.Onchain.RPCURL, err)
	}
	lt, _ := o.rpc.blockTime(ctx, latest)
	fmt.Printf("latest block: %d (%s)\n", latest, time.Unix(lt, 0).UTC().Format(time.RFC3339))
	eb, err := o.rpc.blockAt(ctx, when.Unix())
	if err != nil {
		return err
	}
	et, _ := o.rpc.blockTime(ctx, eb)
	fmt.Printf("call time:    %s → block %d (%s)\n", when.UTC().Format(time.RFC3339), eb, time.Unix(et, 0).UTC().Format(time.RFC3339))
	st, err := o.discover(ctx, token, eb)
	if err != nil {
		return fmt.Errorf("pool discovery: %w", err)
	}
	pool := st.Pool
	if st.Kind == "v4" {
		pool = "PoolManager " + st.Pool + " id " + st.PoolID
	}
	fmt.Printf("pool:         uniswap-%s %s\n", st.Kind, pool)
	fmt.Printf("pair:         token (%d dec) / %s %s (%d dec), token is token%d\n", st.TokenDec, st.QuoteSym, st.Quote, st.QuoteDec, map[bool]int{true: 0, false: 1}[st.TokenIs0])
	if err := o.entryPrice(ctx, st, latest); err != nil {
		return fmt.Errorf("entry price: %w", err)
	}
	fmt.Printf("entry price:  %.12g %s (trade at block %d)\n", st.EntryPriceQ, st.QuoteSym, st.LastPriceBlock)
	if o.mainnet != nil {
		if mb, rt, mt, err := o.mainnetBlockFor(ctx, st.EntryBlock); err != nil {
			fmt.Printf("block conversion: ERROR %v\n", err)
		} else {
			fmt.Printf("block conversion: Robinhood block %d (%s) → Ethereum block %d (%s)\n", st.EntryBlock,
				time.Unix(rt, 0).UTC().Format(time.RFC3339), mb, time.Unix(mt, 0).UTC().Format(time.RFC3339))
		}
	}
	q, ok, err := o.quoteUSD(ctx, st.Quote, st.EntryBlock)
	switch {
	case err != nil:
		fmt.Printf("USD of %s at the call: ERROR %v\n", st.QuoteSym, err)
	case !ok:
		fmt.Printf("USD of %s: no source (no feed, and no WETH or stablecoin pool found for it). Add its Chainlink feed: SCOUT_CHAINLINK_FEEDS=%s=0xFeedAddress (prices stay in %s until then)\n", st.QuoteSym, st.Quote, st.QuoteSym)
	default:
		fmt.Printf("USD of %s at the call: $%.6g (%s) → entry $%.12g\n", st.QuoteSym, q, o.quoteSource(st.Quote), st.EntryPriceQ*q)
	}
	if err := o.scan(ctx, st, latest, nil); err != nil {
		return fmt.Errorf("scan swaps to now: %w", err)
	}
	fmt.Printf("now:          %.12g %s (%+.1f%%), peak %+.1f%%, low %+.1f%% since the call; last trade block %d\n",
		st.LastPriceQ, st.QuoteSym, (st.LastPriceQ/st.EntryPriceQ-1)*100, (st.RunMaxQ/st.EntryPriceQ-1)*100,
		(st.RunMinQ/st.EntryPriceQ-1)*100, st.LastPriceBlock)
	fmt.Printf("log ranges:   up to %d blocks per eth_getLogs (SCOUT_RPC_LOG_CHUNK); %d range(s) split after the node refused them as too large or timed out\n", o.rpc.maxChunk, o.rpc.splits.Load())
	if o.noState.Load() {
		fmt.Println("node type:    full node (no historical state) — USD prices of the paired asset come from event logs")
	} else {
		fmt.Println("node type:    historical state available (archive) or not needed for this pair")
	}
	return nil
}

func printResults(s *scanner, results []*toolResult, noDeliver bool) {
	for _, r := range results {
		v := r.Verdict
		fmt.Printf("===== %s (@%s) — %s =====\n", r.Spec.Name, r.Spec.Bot, r.Status)
		if r.Err != nil {
			fmt.Println("error:", r.Err)
		}
		fmt.Println(r.ReportText())
		fmt.Printf("verdict: %s  label=%q  ticker=%s  summary=%q  via=%s %s  gate=%v\n\n",
			v.Level, v.Label, v.Ticker, v.Summary, v.Source, v.URL, r.Spec.Gate)
	}
	would := s.shouldDeliver(results)
	fmt.Printf("deliver: %v\n", would)
	if would && noDeliver {
		fmt.Println("(not sent: -no-deliver)")
	}
}

func deliveryLabel(c *config) string {
	if c.NotifyBotToken != "" {
		return "bot API chat " + c.NotifyChatID
	}
	return c.NotifyPeer
}

// run connects, authenticates, resolves peers, starts the update loop and calls body.
func run(ctx context.Context, s *scanner, body func(ctx context.Context) error) error {
	cfg := s.cfg
	dispatcher := tg.NewUpdateDispatcher()
	gaps := updates.New(updates.Config{Handler: dispatcher})

	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: cfg.SessionFile},
		UpdateHandler:  gaps,
	})

	onSource := func(m tg.MessageClass) {
		msg, ok := m.(*tg.Message)
		if !ok {
			return
		}
		if p, ok := msg.PeerID.(*tg.PeerChannel); ok && p.ChannelID == s.sourceChannelID {
			s.onChannelPost(msg)
		}
	}
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		onSource(u.Message)
		return nil
	})
	// A call is sometimes posted first and the CA added by an edit.
	dispatcher.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		onSource(u.Message)
		return nil
	})
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		if msg, ok := u.Message.(*tg.Message); ok {
			s.onBotMessage(msg)
		}
		return nil
	})
	dispatcher.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		if msg, ok := u.Message.(*tg.Message); ok {
			s.onBotMessage(msg)
		}
		return nil
	})

	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			auth.Constant(cfg.Phone, cfg.Password, auth.CodeAuthenticatorFunc(
				func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
					fmt.Print("Enter Telegram login code: ")
					var code string
					_, err := fmt.Scanln(&code)
					return strings.TrimSpace(code), err
				})),
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		log.Printf("logged in as %s (@%s)", self.FirstName, self.Username)

		s.api = client.API()
		s.sender = message.NewSender(s.api)

		src, err := s.resolveSource(ctx)
		if err != nil {
			return err
		}
		s.sourceChannelID = src.ID
		s.sourcePeer = src.AsInputPeer()
		s.sourceInput = src.AsInput()
		if src.Left {
			log.Printf("joining @%s so we receive its posts", cfg.SourceChannel)
			if _, err := s.api.ChannelsJoinChannel(ctx, src.AsInput()); err != nil {
				return fmt.Errorf("join @%s: %w", cfg.SourceChannel, err)
			}
		}

		s.byBotMu.Lock()
		s.byBot = map[int64]*toolRunner{}
		s.byBotMu.Unlock()
		for _, r := range s.runners {
			bot, err := s.resolveBot(ctx, r.spec.Bot)
			if err != nil {
				return fmt.Errorf("tool %s: %w", r.spec.Code, err)
			}
			r.botID, r.botPeer = bot.ID, bot.AsInputPeer()
			s.byBotMu.Lock()
			s.byBot[bot.ID] = r
			s.byBotMu.Unlock()
		}

		if cfg.NotifyBotToken == "" && !s.listOnly {
			var label string
			if s.notifyPeer, label, err = s.resolveNotify(ctx); err != nil {
				return fmt.Errorf("resolve SCOUT_NOTIFY_PEER=%q: %w", cfg.NotifyPeer, err)
			}
			log.Printf("reports will be delivered to %s", label)
		}

		errc := make(chan error, 1)
		go func() {
			errc <- gaps.Run(ctx, s.api, self.ID, updates.AuthOptions{
				OnStart: func(ctx context.Context) { log.Println("update stream started") },
			})
		}()

		bodyErr := body(ctx)
		if bodyErr != nil && !errors.Is(bodyErr, context.Canceled) {
			return bodyErr
		}
		select {
		case err := <-errc:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		case <-time.After(3 * time.Second):
		}
		return bodyErr
	})
}
