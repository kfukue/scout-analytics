package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The listener sends the call's dataset row to the scoring service, stores the
// scores and gets the header line back; a dead service only costs the line.
func TestScoreCall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	respLine := "Model v3 · 7d: runner 31%, collapse 44%"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/score" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Row map[string]any `json:"row"`
		}
		json.Unmarshal(b, &req)
		got = req.Row
		w.Write([]byte(`{"model_version":"v3","line":` + strconv.Quote(respLine) + `,
			"buckets":[{"bucket":"short","runner_prob":0.31,"collapse_prob":0.44,"runner_rank_pct":92.5,"passed":false},
			           {"bucket":"medium","runner_prob":0.12,"collapse_prob":0.5,"runner_rank_pct":80,"passed":true},
			           {"bucket":"long","runner_prob":0.06,"collapse_prob":null,"runner_rank_pct":null}]}`))
	}))
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(),
		ModelURL: srv.URL, ModelTimeout: 2 * time.Second, Price: priceConfig{Enabled: false}}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	s.onChannelPost(postAt(1, time.Now().Add(-time.Minute), "0x4444444444444444444444444444444444444444"))
	j := <-s.queue
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// listening from here on: the call's own notification has gone already
	conn := listenTest(t, st)

	line := s.scoreCall(ctx, j)
	if !strings.HasPrefix(line, "Model v3") {
		t.Fatalf("line %q", line)
	}
	// the stored score is announced once, for an open website
	if got, want := nextNotification(t, conn, 5*time.Second), fmt.Sprintf(`{"kind":"score","call_id":%d}`, *j.CallID); got != want {
		t.Fatalf("after a score: notification %q, want %q", got, want)
	}
	if got := nextNotification(t, conn, 300*time.Millisecond); got != "" {
		t.Fatalf("a second notification for one score: %q", got)
	}
	// The pre-call time is logged with the outcome, for tuning SCOUT_MODEL_TIMEOUT.
	if want := "score " + j.CA + ": " + respLine + " (precall "; !strings.Contains(logs.String(), want) {
		t.Fatalf("log %q, want it to contain %q", logs.String(), want)
	}
	if got["holders"] != float64(1150) || got["proof_elite"] != float64(3) || got["contract_address"] != j.CA {
		t.Fatalf("row sent to the model: %v", got)
	}
	ps, err := st.PredictionsForCall(ctx, *j.CallID)
	if err != nil || len(ps) != 3 || ps[0].Bucket != "short" || *ps[0].RunnerProb != 0.31 || *ps[0].RunnerRankPct != 92.5 ||
		ps[2].Bucket != "long" || ps[2].CollapseProb != nil {
		t.Fatalf("stored %+v %v", ps, err)
	}
	// passed as the service sent it: false, true, and not sent = NULL (not recorded)
	if ps[0].Passed == nil || *ps[0].Passed || ps[1].Passed == nil || !*ps[1].Passed || ps[2].Passed != nil {
		t.Fatalf("passed %v %v %v, want false true nil", ps[0].Passed, ps[1].Passed, ps[2].Passed)
	}
	// one transaction: every bucket has the same created_at
	var times int
	if err := st.Pool.QueryRow(ctx, `SELECT count(DISTINCT created_at) FROM scout_call_predictions WHERE call_id = $1`, *j.CallID).Scan(&times); err != nil || times != 1 {
		t.Fatalf("created_at values of one score: %d (%v), want 1", times, err)
	}

	// No bucket passed its gates: the service sends no line, the delivery has none.
	respLine = ""
	logs.Reset()
	if line := s.scoreCall(ctx, j); line != "" {
		t.Fatalf("line with an empty line from the service: %q, want none", line)
	}
	if want := "score " + j.CA + ": model v3 sent no line (precall "; !strings.Contains(logs.String(), want) {
		t.Fatalf("log %q, want it to contain %q", logs.String(), want)
	}

	srv.Close() // service down: no line, no error
	logs.Reset()
	if line := s.scoreCall(ctx, j); line != "" {
		t.Fatalf("line with the service down: %q", line)
	}
	if want := "(precall "; !strings.Contains(logs.String(), want) {
		t.Fatalf("log with the service down %q, want it to contain %q", logs.String(), want)
	}
	s.cfg.ModelURL = "" // off
	if line := s.scoreCall(ctx, j); line != "" {
		t.Fatal("scoring should be off")
	}
}

// scoreTestScanner returns a scanner that scores against modelURL with the
// given budget, with live pre-call stats read from the fake chain f.
func scoreTestScanner(t *testing.T, st *ScoutStore, f *fakeChain, modelURL string, budget time.Duration) *scanner {
	t.Helper()
	t.Setenv("SCOUT_PRICE_SOURCE", "onchain")
	t.Setenv("SCOUT_TRACK_PERFORMANCE", "true")
	t.Setenv("SCOUT_RPC_URL", f.srv.URL)
	t.Setenv("SCOUT_RPC_RPS", "100000")
	t.Setenv("SCOUT_MAINNET_RPC_URL", "") // never a real node
	pc, err := loadPriceConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(),
		ModelURL: modelURL, ModelTimeout: budget, Price: pc}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	return s
}

// scoreTestPool puts a v3 pool token / WETH on the fake chain, with trades in
// the hour before entry.
func scoreTestPool(f *fakeChain, token string, entry time.Time) {
	pool, wallet := "0x00000000000000000000000000000000000000c3", "0x00000000000000000000000000000000000000d1"
	f.addToken(token, 18, "TKN")
	f.addToken(tWETH, 18, "WETH")
	f.constCall(pool, selToken0, ret(wAddr(tWETH))) // WETH < token → token is token1
	f.constCall(pool, selToken1, ret(wAddr(token)))
	f.constCall(pool, selSlot0, ret(wInt(1)))
	eb := f.blockAtTime(entry)
	for _, off := range []uint64{30000, 6000, 1200} { // 50, 10 and 2 minutes before the call
		blk := eb - off
		f.swapV3(pool, blk, hexU64(blk), sqrtX96(1/1e-6))
		f.transfer(token, pool, wallet, blk, hexU64(blk))
	}
}

// scoreTestModel is a scoring service that hands every row it is sent to rows.
func scoreTestModel(t *testing.T) (*httptest.Server, chan map[string]any) {
	t.Helper()
	rows := make(chan map[string]any, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Row map[string]any `json:"row"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rows <- req.Row
		w.Write([]byte(`{"model_version":"v3","line":"Model v3","buckets":[{"bucket":"short","runner_prob":0.31}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rows
}

// Live scoring sends quote_asset as the view will show it once the tracker has
// run: the symbol of the pool live pre-call found, while the view has none yet.
// A symbol the view already has is kept; an unknown one stays null. The scored
// row (with it) is what gets stored; the tracker's state is not touched.
func TestScoreCallQuoteAsset(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		ca        string
		pool      bool   // the token has a pool on the chain
		onchain   string // tracking state already there ("" = none yet)
		want      any    // quote_asset sent and stored (nil = null)
		wantState string // scout_call_tracking.onchain afterwards ("" = NULL)
		precall   bool   // pre-call stats stored live
	}{
		{name: "view null, pool found", ca: "0x4444444444444444444444444444444444444444", pool: true, want: "WETH", precall: true},
		{name: "view has it", ca: "0x5555555555555555555555555555555555555555", pool: true,
			onchain: `{"quote_sym": "USDG"}`, want: "USDG", wantState: `{"quote_sym": "USDG"}`, precall: true},
		{name: "symbol unknown", ca: "0x6666666666666666666666666666666666666666", want: nil},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChain(t, 2*24*time.Hour)
			entry := time.Now().Add(-10 * time.Minute)
			if c.pool {
				scoreTestPool(f, c.ca, entry)
			}
			model, rows := scoreTestModel(t)
			s := scoreTestScanner(t, st, f, model.URL, 30*time.Second)
			s.onChannelPost(postAt(100+i, entry, c.ca))
			j := <-s.queue
			if c.onchain != "" {
				if _, err := st.Pool.Exec(ctx, `UPDATE scout_call_tracking SET onchain = $2::jsonb WHERE call_id = $1`, *j.CallID, c.onchain); err != nil {
					t.Fatal(err)
				}
			}

			if line := s.scoreCall(ctx, j); line != "Model v3" {
				t.Fatalf("scoreCall(%s) = %q, want %q", c.ca, line, "Model v3")
			}
			var sent map[string]any
			select {
			case sent = <-rows:
			default:
				t.Fatalf("scoreCall(%s): the model was not asked", c.ca)
			}
			if sent["quote_asset"] != c.want || sent["contract_address"] != c.ca {
				t.Fatalf("row sent for %s: quote_asset %v (contract %v), want %v", c.ca, sent["quote_asset"], sent["contract_address"], c.want)
			}
			var stored, state *string
			var precall bool
			if err := st.Pool.QueryRow(ctx, `SELECT p.features->>'quote_asset', t.onchain::text,
				EXISTS (SELECT 1 FROM scout_call_precall x WHERE x.call_id = p.call_id)
				FROM scout_call_predictions p JOIN scout_call_tracking t ON t.call_id = p.call_id
				WHERE p.call_id = $1 AND p.bucket = 'short'`, *j.CallID).Scan(&stored, &state, &precall); err != nil {
				t.Fatal(err)
			}
			if c.want == nil && stored != nil || c.want != nil && (stored == nil || *stored != c.want) {
				t.Fatalf("stored features of %s: quote_asset %v, want %v", c.ca, strOrNil(stored), c.want)
			}
			if c.wantState == "" && state != nil || c.wantState != "" && (state == nil || *state != c.wantState) {
				t.Fatalf("tracking state of %s: %v, want %q (the tracker owns it)", c.ca, strOrNil(state), c.wantState)
			}
			if precall != c.precall {
				t.Fatalf("pre-call stats of %s stored: %v, want %v", c.ca, precall, c.precall)
			}
		})
	}
}

// Scoring never holds the delivery longer than SCOUT_MODEL_TIMEOUT (plus the
// stored-prediction write): a slow scoring service or slow pre-call trades cost
// the line, nothing else.
func TestScoreCallBudget(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// budget: long enough for the feature-row read before the slow scorer is
	// asked (wantAsked), even under -race on a loaded machine (300 ms flaked
	// once). The slow handlers never answer on their own, so without the budget
	// scoreCall would not return at all.
	const budget = 2 * time.Second
	// slack: time past the budget for giving up and logging under -race;
	// budget+slack (7 s) is still far below the old 20 s pre-call limit.
	const slack = 5 * time.Second
	cases := []struct {
		name        string
		ca          string
		slowScorer  bool
		slowPrecall bool
		wantAsked   bool // the model got the row (and gave up on its client)
	}{
		{name: "slow scorer", ca: "0x7777777777777777777777777777777777777777", slowScorer: true, wantAsked: true},
		{name: "slow pre-call", ca: "0x8888888888888888888888888888888888888888", slowPrecall: true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			release := make(chan struct{}) // lets blocked fake handlers finish before their servers close
			f := newFakeChain(t, 2*24*time.Hour)
			entry := time.Now().Add(-10 * time.Minute)
			scoreTestPool(f, c.ca, entry)
			if c.slowPrecall {
				f.setLogsHook(func(string, uint64, uint64) string {
					<-release
					return ""
				})
			}
			asked := make(chan struct{}, 10)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- struct{}{}
				if c.slowScorer {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				w.Write([]byte(`{"model_version":"v3","line":"Model v3","buckets":[]}`))
			}))
			t.Cleanup(model.Close)
			t.Cleanup(func() { close(release) }) // runs before the closes above
			s := scoreTestScanner(t, st, f, model.URL, budget)
			s.onChannelPost(postAt(200+i, entry, c.ca))
			j := <-s.queue
			if !c.slowPrecall {
				s.pc.Enabled = false // no pre-call: the whole budget goes to the model
			}

			start := time.Now()
			line := s.scoreCall(ctx, j)
			took := time.Since(start)
			if line != "" {
				t.Fatalf("scoreCall(%s) = %q, want no line", c.ca, line)
			}
			if took > budget+slack {
				t.Fatalf("scoreCall(%s) took %s, want at most %s (budget %s)", c.ca, took, budget+slack, budget)
			}
			if got := len(asked) > 0; got != c.wantAsked {
				t.Fatalf("scoreCall(%s): model asked %v, want %v", c.ca, got, c.wantAsked)
			}
			ps, err := st.PredictionsForCall(ctx, *j.CallID)
			if err != nil || len(ps) != 0 {
				t.Fatalf("predictions stored for %s: %+v %v, want none", c.ca, ps, err)
			}
		})
	}
}
