package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Latest-price pass: the log lines name the call (or the quote lookup) they
// come from, and the progress line names the rows still running. Fake node,
// real database.
// ---------------------------------------------------------------------------

// A range of a latest row that times out on the node is split, and the line
// saying so names the row's call.
func TestLatestPriceRowLabelOnRangeLines(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 100*ltDay)
	// The first range of the row's pool the node sees, and the one retry of
	// it, time out (keyed by the range: the window may be planned as two
	// ranges fetched at once).
	var mu sync.Mutex
	var first [2]uint64
	var refused atomic.Int64
	f.setLogsHook(func(addr string, from, to uint64) string {
		if addr != ltGoodPool || to-from+1 <= minRangeBlocks {
			return ""
		}
		mu.Lock()
		defer mu.Unlock()
		if first == [2]uint64{} {
			first = [2]uint64{from, to}
		}
		if first == [2]uint64{from, to} && refused.Load() < 2 {
			refused.Add(1)
			return "request timed out"
		}
		return ""
	})
	s := ltScanner(t, st, f.srv.URL)
	id := ltSeedRow(t, st, ltSeed{Msg: 1, CA: ltCA(1), Age: 5 * ltDay, Status: TrackDone, State: ltStateJSON(ltGoodPool, f.head()-1000)})

	res := s.refreshLatest(ctx, false)
	if res.Due != 1 || res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass: got %+v; want 1 due, 1 refreshed, none failed", res)
	}
	if n := refused.Load(); n < 2 {
		t.Fatalf("the node timed out %d request(s), want 2 (the first range and its retry)", n)
	}
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^.*latest call %d: eth_getLogs blocks \d+-\d+ \(\d+ blocks\) timed out on the node \(2 tries\) \(.*request timed out\); this scan continues with \d+-block ranges \(max \d+\)`, id))
	if logged := buf.String(); !re.MatchString(logged) {
		t.Fatalf("no split line naming call %d (pattern %s); log:\n%s", id, re, logged)
	}
}

// A range of the quote's USD lookup (here the ETH/USD feed's whole-history
// AggregatorConfirmed search, on a node without historical state) that the
// node refuses is split, and the line names the lookup: the quote and the
// hour, not one of the rows waiting for it.
func TestLatestPriceQuoteLookupLabelOnRangeLines(t *testing.T) {
	buf := captureRangeLog(t)
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := ltChain(t, 100*ltDay)
	f.fullNode = true                                 // latestRoundData() at the hour's block fails: the feed's events are read
	f.answerUpdated(ltFeed, f.head()-100_000, 3000e8) // ~3 hours ago, before the start of the current hour
	var refused atomic.Int64
	f.setLogsHook(func(addr string, from, to uint64) string {
		if addr == ltFeed && to-from+1 > minRangeBlocks && refused.Add(1) == 1 {
			return "query returned more than 10000 results"
		}
		return ""
	})
	s := ltScanner(t, st, f.srv.URL)
	wethState := strings.NewReplacer(tUSDG, tWETH, `"quote_sym":"USDG"`, `"quote_sym":"WETH"`, `"quote_dec":6`, `"quote_dec":18`).
		Replace(ltStateJSON(ltGoodPool, f.head()-1000))
	id := ltSeedRow(t, st, ltSeed{Msg: 1, CA: ltCA(1), Age: 5 * ltDay, Status: TrackDone, State: wethState})

	res := s.refreshLatest(ctx, false)
	if res.Due != 1 || res.Refreshed != 1 || res.Failed != 0 {
		t.Fatalf("pass: got %+v; want 1 due, 1 refreshed, none failed", res)
	}
	if c := ltRead(t, st, id); !ltNear(c.Price, 3*ltETH) {
		t.Fatalf("call %d: got price %v, want %v (3 WETH at the feed's $%v)", id, fnum(c.Price), 3*ltETH, ltETH)
	}
	if refused.Load() == 0 {
		t.Fatal("the feed's eth_getLogs never reached the node")
	}
	re := regexp.MustCompile(`(?m)^.*latest quote ` + regexp.QuoteMeta(strings.ToLower(tWETH)) +
		` hour \d{4}-\d\d-\d\d \d\d:00 UTC: eth_getLogs blocks \d+-\d+ \(\d+ blocks\) refused as too large \(.*query returned more than 10000 results\); this scan continues with`)
	logged := buf.String()
	if !re.MatchString(logged) {
		t.Fatalf("no split line naming the quote lookup (pattern %s); log:\n%s", re, logged)
	}
	if strings.Contains(logged, fmt.Sprintf("latest call %d: eth_getLogs blocks", id)) {
		t.Fatalf("the quote lookup's split was logged under the row's label; log:\n%s", logged)
	}
}

// While a row hangs, the progress line counts the rows handed out, finished
// and running, and names the running row with its age.
func TestLatestPriceProgressNamesRunningRows(t *testing.T) {
	buf := captureRangeLog(t)
	prevEvery, prevSlow := latestProgressEvery, latestSlowRowAfter
	latestProgressEvery, latestSlowRowAfter = 20*time.Millisecond, 0 // every running row is named
	t.Cleanup(func() { latestProgressEvery, latestSlowRowAfter = prevEvery, prevSlow })
	h := newLTHang(t, nil)
	h.s.pc.LatestRowTimeout = 2 * time.Second // the hang row runs this long; the good rows finish at once

	res := h.s.refreshLatest(context.Background(), false)
	if res.Due != 3 || res.Refreshed != 2 || res.Failed != 1 {
		t.Fatalf("pass: got %+v; want 3 due, 2 refreshed, 1 failed", res)
	}
	logged := buf.String()
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^.*latest prices: 2 finished, 1 running \(call %d for \d+s\), 3 handed out of 3 due, \d+s elapsed, \d+ RPC requests$`, h.ids["hang"]))
	if !re.MatchString(logged) {
		t.Fatalf("no progress line naming the hanging call %d (pattern %s); log:\n%s", h.ids["hang"], re, logged)
	}
	if strings.Contains(logged, "call(s) so far") {
		t.Fatalf("the old progress line is still printed; log:\n%s", logged)
	}
	// The summary line is unchanged.
	sum := regexp.MustCompile(`(?m)^.*latest prices: 2 refreshed \(0 changed\) in [\d.]+m?s, \d+ RPC requests, \d+ waiting; 1 failed, tried again after 30m0s \(first: call \d+: `)
	if !sum.MatchString(logged) {
		t.Fatalf("summary line not found (pattern %s); log:\n%s", sum, logged)
	}
}
