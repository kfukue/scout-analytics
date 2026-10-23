package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two scans want the same eth_getLogs range at the same moment and share one
// request (fetchLogs). These tests cover what the waiting scan does with the
// error of a request that failed.

// flightPool puts a swap every 3001 blocks below 16000 into f and returns the
// pool and the blocks of its logs.
func flightPool(f *fakeChain) (string, []uint64) {
	pool := "0x00000000000000000000000000000000000000c7"
	var want []uint64
	for b := uint64(500); b < 16_000; b += 3001 {
		f.swapV3(pool, b, fmt.Sprintf("0x%x", b), sqrtX96(1))
		want = append(want, b)
	}
	return pool, want
}

// The scan that sent the request stops on its own context's deadline (a
// latest-price row's time limit) before the node answers. The other scan,
// whose context is still alive, asks the node again itself and gets the real
// answer: its range is not halved, and nothing is logged as a client time-out.
func TestGetLogsWaiterAsksAgainAfterAskerDeadline(t *testing.T) {
	buf := captureRangeLog(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool, want := flightPool(f)
	var rl rangeLog
	inFlight, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	t.Cleanup(func() { closeOnce(release) }) // before the fake chain's server closes (cleanups run last-in first-out)
	f.setLogsHook(func(addr string, from, to uint64) string {
		rl.add(rangeAsk{addr, from, to, false})
		isFirst := false
		first.Do(func() { isFirst = true })
		if isFirst {
			close(inFlight)
			<-release // the node answers the asker's request only after its deadline
		}
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "16000", "SCOUT_RPC_PARALLEL": "1", "SCOUT_RPC_LOG_CACHE": "0"})

	// The asker: a short deadline of its own (the waiter joins its request well before it).
	askerErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		askerErr <- o.rpc.getLogsChunked(ctx, pool, []any{topicSwapV3}, 0, 15_999, func(rpcLog) {})
	}()
	<-inFlight

	// The waiter: no deadline.
	var got []uint64
	waiterErr := make(chan error, 1)
	go func() {
		waiterErr <- o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 0, 15_999, func(l rpcLog) { got = append(got, l.block()) })
	}()

	if err := <-askerErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("asker: got error %v, want its own context's deadline", err)
	}
	closeOnce(release)
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter: got error %v, want the node's answer", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("waiter: got logs at %v, want %v", got, want)
	}
	asks := rl.list(pool)
	if len(asks) != 2 {
		t.Fatalf("got %d eth_getLogs (%+v), want 2: the asker's and the waiter's own", len(asks), asks)
	}
	for i, a := range asks {
		if a.from != 0 || a.to != 15_999 {
			t.Fatalf("request %d: got blocks %d-%d, want the whole range 0-15999 (no halving)", i+1, a.from, a.to)
		}
	}
	if n := o.rpc.splits.Load(); n != 0 {
		t.Fatalf("got %d range splits, want 0", n)
	}
	if out := buf.String(); strings.Contains(out, "client time-out") || strings.Contains(out, "this scan") {
		t.Fatalf("the asker's deadline was reported as the node's:\n%s", out)
	}
}

// A genuine client time-out (no answer within http.Client.Timeout, after
// call()'s retries) is still the node's answer for both scans: the waiter
// takes it from the shared request instead of asking again, and both scans
// split the range. (TestGetLogsClientTimeoutSplits covers one scan alone.)
func TestGetLogsClientTimeoutSharedWithWaiter(t *testing.T) {
	buf := captureRangeLog(t)
	fastRetries(t)
	f := newFakeChain(t, 10*24*time.Hour)
	pool, want := flightPool(f)
	var rl rangeLog
	inFlight := make(chan struct{})
	var first sync.Once
	f.setLogsHook(func(addr string, from, to uint64) string {
		slow := to-from+1 > 4000
		rl.add(rangeAsk{addr, from, to, slow})
		first.Do(func() { close(inFlight) })
		if slow {
			time.Sleep(200 * time.Millisecond) // past the client's time limit below
		}
		return ""
	})
	o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": "16000", "SCOUT_RPC_PARALLEL": "1", "SCOUT_RPC_LOG_CACHE": "0"})
	o.rpc.http.Timeout = 40 * time.Millisecond

	scan := func(got *[]uint64, errc chan<- error) {
		errc <- o.rpc.getLogsChunked(context.Background(), pool, []any{topicSwapV3}, 0, 15_999, func(l rpcLog) { *got = append(*got, l.block()) })
	}
	var gotA, gotB []uint64
	errA, errB := make(chan error, 1), make(chan error, 1)
	go scan(&gotA, errA)
	<-inFlight // the asker's first request is out (call() spends 4 x 40 ms on it)
	go scan(&gotB, errB)
	for name, errc := range map[string]chan error{"asker": errA, "waiter": errB} {
		if err := <-errc; err != nil {
			t.Fatalf("%s: got error %v, want the logs", name, err)
		}
	}
	if !slices.Equal(gotA, want) || !slices.Equal(gotB, want) {
		t.Fatalf("got logs at %v (asker) and %v (waiter), want %v for both", gotA, gotB, want)
	}
	whole := 0
	for _, a := range rl.list(pool) {
		if a.from == 0 && a.to == 15_999 {
			whole++
		}
	}
	if whole != 4 {
		t.Fatalf("blocks 0-15999 asked %d times, want 4 (one request with call()'s retries, shared by both scans)", whole)
	}
	line := "eth_getLogs blocks 0-15999 (16000 blocks) got no answer in time (client time-out)"
	if n := strings.Count(buf.String(), line); n != 2 {
		t.Fatalf("%q logged %d times, want 2 (each scan split on the shared answer):\n%s", line, n, buf.String())
	}
}

// closeOnce closes ch unless it is closed already (test helper).
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}
