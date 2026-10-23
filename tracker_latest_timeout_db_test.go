package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass with a row time limit (SCOUT_LATEST_ROW_TIMEOUT): fake
// node, real database.
// ---------------------------------------------------------------------------

const (
	ltHangPool = "0x00000000000000000000000000000000000000c8" // its eth_getLogs gets no answer
	ltGoodPool = "0x00000000000000000000000000000000000000c7"
)

// ltHang is a pass over three rows on USDG pools (a stablecoin: no USD lookup):
// "hang", whose eth_getLogs gets no answer until the request is given up, and
// "good1", "good2", which the node answers at once.
type ltHang struct {
	st    *ScoutStore
	s     *scanner
	ids   map[string]int
	hangs atomic.Int64 // eth_getLogs of the hang row that got no answer
}

// newLTHang sets it up; onHang (optional) runs when the hang row's
// eth_getLogs arrives.
func newLTHang(t *testing.T, onHang func()) *ltHang {
	t.Helper()
	h := &ltHang{st: testStore(t), ids: map[string]int{}}
	if err := h.st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 100*ltDay)
	stop := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return // the client gave up
		}
		if bytes.Contains(body, []byte("eth_getLogs")) && bytes.Contains(body, []byte(ltHangPool)) {
			h.hangs.Add(1)
			if onHang != nil {
				onHang()
			}
			select {
			case <-r.Context().Done(): // the row's (or the pass's) context ended
			case <-stop:
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.serve(w, r)
	}))
	t.Cleanup(node.Close)
	t.Cleanup(func() { close(stop) }) // runs before node.Close
	h.s = ltScanner(t, h.st, node.URL)
	for i, key := range []string{"hang", "good1", "good2"} {
		pool := ltGoodPool
		if key == "hang" {
			pool = ltHangPool
		}
		h.ids[key] = ltSeedRow(t, h.st, ltSeed{Msg: i + 1, CA: ltCA(i + 1), Age: 5 * ltDay, Status: TrackDone,
			State: ltStateJSON(pool, f.head()-1000)})
	}
	return h
}

// whole is the tracking row as JSON (latest_* columns, onchain and all).
func (h *ltHang) whole(t *testing.T, key string) string {
	t.Helper()
	var row string
	if err := h.st.Pool.QueryRow(context.Background(), `SELECT to_jsonb(t)::text FROM scout_call_tracking t WHERE call_id = $1`,
		h.ids[key]).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

// refreshedNow fails the test unless the good rows got their latest price
// (3 USDG, entry 2) in the last minute.
func (h *ltHang) refreshedNow(t *testing.T) {
	t.Helper()
	for _, k := range []string{"good1", "good2"} {
		if c := ltRead(t, h.st, h.ids[k]); !ltNear(c.Price, 3) || !ltNear(c.Ret, 50) || c.Checked == nil || time.Since(*c.Checked) > time.Minute {
			t.Errorf("%s: got price %v, return %v, checked %v; want 3, 50, just now", k, fnum(c.Price), fnum(c.Ret), c.Checked)
		}
	}
}

// A row whose eth_getLogs never answers is stopped at the row time limit: the
// pass ends in about that time, the row counts as failed with a clear error,
// is written nowhere and set aside for latestRetryAfter, and the other rows of
// the pass are refreshed.
func TestLatestPriceRowTimeLimit(t *testing.T) {
	h := newLTHang(t, nil)
	const limit = 300 * time.Millisecond // below the setting's 30s minimum: set directly
	h.s.pc.LatestRowTimeout = limit
	before := h.whole(t, "hang")
	logged := captureRangeLog(t) // restored by t.Cleanup, also after a panic
	start := time.Now()
	res := h.s.refreshLatest(context.Background(), false)
	took := time.Since(start)
	if n := h.hangs.Load(); n == 0 {
		t.Fatal("the hang row's eth_getLogs never reached the node")
	}
	if took < limit || took > limit+10*time.Second {
		t.Fatalf("the pass took %s, want about the %s row limit", took, limit)
	}
	wantErr := fmt.Sprintf("call %d: latest price: stopped after the 300ms row time limit (SCOUT_LATEST_ROW_TIMEOUT)", h.ids["hang"])
	if res.Due != 3 || res.Refreshed != 2 || res.Failed != 1 || !strings.HasPrefix(res.FirstErr, wantErr) {
		t.Fatalf("pass: got %+v; want 3 due, 2 refreshed, 1 failed, first error starting %q", res, wantErr)
	}
	if line := logged.String(); !strings.Contains(line, "1 failed") || !strings.Contains(line, wantErr) {
		t.Fatalf("summary line: got %q, want the failure and %q", line, wantErr)
	}
	if after := h.whole(t, "hang"); after != before {
		t.Fatalf("the stopped row changed:\n got %s\nwant %s", after, before)
	}
	if skip := h.s.latestSkip(time.Now()); !slices.Equal(skip, []int{h.ids["hang"]}) {
		t.Fatalf("set aside: got %v, want [%d] (the stopped row)", skip, h.ids["hang"])
	}
	h.refreshedNow(t)
	// The next pass leaves the stopped row alone (and the others are fresh).
	if res := h.s.refreshLatest(context.Background(), false); res.Due != 0 {
		t.Fatalf("next pass: got %+v, want nothing due", res)
	}
}

// Ctrl+C while a row with a time limit waits for the node: not a failure, not
// set aside, nothing written.
func TestLatestPriceRowTimeLimitShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newLTHang(t, cancel)
	h.s.pc.LatestRowTimeout = time.Minute
	before := h.whole(t, "hang")
	start := time.Now()
	res := h.s.refreshLatest(ctx, false)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the stopped pass took %s", took)
	}
	if n := h.hangs.Load(); n == 0 || ctx.Err() == nil {
		t.Fatalf("the pass was not stopped in its scan: %d eth_getLogs, ctx error %v", n, ctx.Err())
	}
	if res.Failed != 0 || res.FirstErr != "" {
		t.Fatalf("stopped pass: got %+v, want no failure", res)
	}
	if after := h.whole(t, "hang"); after != before {
		t.Fatalf("the interrupted row changed:\n got %s\nwant %s", after, before)
	}
	if skip := h.s.latestSkip(time.Now()); len(skip) != 0 {
		t.Fatalf("set aside after a stop: %v, want none", skip)
	}
}

// SCOUT_LATEST_ROW_TIMEOUT=0: no time limit, as before the setting. The row
// waits for the node until the pass itself is stopped, and is not a failure.
func TestLatestPriceRowTimeLimitOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const wait = time.Second // the pass is stopped this long after the row started waiting
	h := newLTHang(t, func() { time.AfterFunc(wait, cancel) })
	h.s.pc.LatestRowTimeout = 0
	before := h.whole(t, "hang")
	start := time.Now()
	res := h.s.refreshLatest(ctx, false)
	if took := time.Since(start); took < wait {
		t.Fatalf("the pass ended after %s, before it was stopped (%s): the row had a time limit", took, wait)
	}
	if res.Due != 3 || res.Refreshed != 2 || res.Failed != 0 || res.FirstErr != "" {
		t.Fatalf("pass: got %+v; want 3 due, 2 refreshed, none failed", res)
	}
	if after := h.whole(t, "hang"); after != before {
		t.Fatalf("the waiting row changed:\n got %s\nwant %s", after, before)
	}
	if skip := h.s.latestSkip(time.Now()); len(skip) != 0 {
		t.Fatalf("set aside: %v, want none", skip)
	}
	h.refreshedNow(t)
}

// The USD price of the quote asset is looked up once per pass under the pass's
// context: when the first row's time limit ends while the lookup waits for the
// node, the lookup goes on, and the next rows of the same quote are priced
// with it.
func TestLatestPriceQuoteLookupOutlivesRowLimit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 100*ltDay)
	release := make(chan struct{})
	var held, answered, dropped atomic.Int64
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		if bytes.Contains(body, []byte("eth_call")) && bytes.Contains(body, []byte(ltFeed)) {
			held.Add(1)
			select {
			case <-release:
				answered.Add(1)
			case <-r.Context().Done():
				dropped.Add(1)
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.serve(w, r)
	}))
	t.Cleanup(node.Close)
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free) // runs before node.Close
	s := ltScanner(t, st, node.URL)
	s.pc.Workers = 1 // one row at a time: the second starts once the first has failed
	const limit = 2 * time.Second
	s.pc.LatestRowTimeout = limit
	wethState := strings.NewReplacer(tUSDG, tWETH, `"quote_sym":"USDG"`, `"quote_sym":"WETH"`, `"quote_dec":6`, `"quote_dec":18`).
		Replace(ltStateJSON(ltGoodPool, f.head()-1000))
	ids := map[int]bool{}
	for i := range 3 {
		ids[ltSeedRow(t, st, ltSeed{Msg: i + 1, CA: ltCA(i + 1), Age: 5 * ltDay, Status: TrackDone, State: wethState})] = true
	}

	done := make(chan latestStats, 1)
	go func() { done <- s.refreshLatest(ctx, false) }()
	// Wait for the first row to fail (its limit ended during the lookup), then
	// let the node answer the lookup.
	var failed []int
	for deadline := time.Now().Add(limit + 10*time.Second); len(failed) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("no row failed within %s", limit+10*time.Second)
		}
		time.Sleep(time.Millisecond)
		failed = s.latestSkip(time.Now())
	}
	if held.Load() == 0 {
		t.Fatal("the quote lookup never reached the node")
	}
	free()
	res := <-done
	if len(failed) != 1 || !ids[failed[0]] {
		t.Fatalf("failed rows: got %v, want one of %v", failed, ids)
	}
	wantErr := fmt.Sprintf("call %d: latest price: stopped after the 2s row time limit (SCOUT_LATEST_ROW_TIMEOUT): USD price of WETH", failed[0])
	if res.Due != 3 || res.Refreshed != 2 || res.Failed != 1 || !strings.HasPrefix(res.FirstErr, wantErr) {
		t.Fatalf("pass: got %+v; want 3 due, 2 refreshed, 1 failed, first error starting %q", res, wantErr)
	}
	if n := dropped.Load(); n != 0 {
		t.Fatalf("%d quote request(s) given up: the lookup was cut off by the row limit", n)
	}
	if answered.Load() == 0 {
		t.Fatal("the held quote request was never answered")
	}
	for id := range ids {
		c := ltRead(t, st, id)
		if id == failed[0] {
			if c.Price != nil || c.Checked != nil {
				t.Errorf("call %d (failed) was written: %+v", id, c)
			}
			continue
		}
		if !ltNear(c.Price, 3*ltETH) || c.Checked == nil {
			t.Errorf("call %d: got price %v, checked %v; want %v", id, fnum(c.Price), c.Checked, 3*ltETH)
		}
	}
	if skip := s.latestSkip(time.Now()); !slices.Equal(skip, failed) {
		t.Fatalf("set aside after the pass: got %v, want %v", skip, failed)
	}
}
