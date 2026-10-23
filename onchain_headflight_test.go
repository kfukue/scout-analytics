package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Workers that want the head block at the same moment share one
// eth_blockNumber (blockNumber's headFlight). These tests cover what a waiting
// worker does with the error of a request that failed.

// The worker that sent the request stops on its own context's deadline (a
// latest-price row's time limit) before the node answers. The other worker,
// whose context is still alive, asks the node again itself (one extra request
// only) and gets the real head.
func TestBlockNumberWaiterAsksAgainAfterAskerDeadline(t *testing.T) {
	f := newFakeChain(t, 24*time.Hour)
	o := testOnchain(t, f, nil)
	var hits atomic.Int64
	inFlight, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(release) }) // before the fake chain's server closes (cleanups run last-in first-out)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(inFlight)
			<-release // the node answers the asker's request only after its deadline
		}
		f.serve(w, r)
	})

	// The asker: a short deadline of its own (the waiter joins its request well before it).
	askerErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := o.rpc.blockNumber(ctx)
		askerErr <- err
	}()
	<-inFlight

	// The waiter: no deadline.
	type answer struct {
		n   uint64
		err error
	}
	waiter := make(chan answer, 1)
	go func() {
		n, err := o.rpc.blockNumber(context.Background())
		waiter <- answer{n, err}
	}()

	if err := <-askerErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("asker: got error %v, want its own context's deadline", err)
	}
	closeOnce(release)
	if got := <-waiter; got.err != nil || got.n != f.latest {
		t.Fatalf("waiter: got head %d (error %v), want the node's head %d", got.n, got.err, f.latest)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("got %d eth_blockNumber, want 2: the asker's and the waiter's own", n)
	}
}

// A genuine client time-out (no answer within http.Client.Timeout, after
// call()'s retries, every context alive) is the node's answer for every
// worker: the waiters take it from the shared request instead of each asking
// again with a full round of retries of its own.
func TestBlockNumberClientTimeoutSharedWithWaiters(t *testing.T) {
	fastRetries(t)
	f := newFakeChain(t, 24*time.Hour)
	o := testOnchain(t, f, nil)
	o.rpc.http.Timeout = 40 * time.Millisecond
	var hits atomic.Int64
	inFlight := make(chan struct{})
	var first sync.Once
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		first.Do(func() { close(inFlight) })
		time.Sleep(200 * time.Millisecond) // past the client's time limit above
		f.serve(w, r)
	})

	errs := make([]error, 4) // errs[0]: the asker; the rest: waiters
	var wg sync.WaitGroup
	ask := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = o.rpc.blockNumber(context.Background())
		}()
	}
	ask(0)
	<-inFlight // the asker's first request is out (call() spends 4 x 40 ms on it)
	for i := 1; i < len(errs); i++ {
		ask(i)
	}
	wg.Wait()

	if !errors.Is(errs[0], context.DeadlineExceeded) {
		t.Fatalf("asker: got error %v, want the client's time-out", errs[0])
	}
	if n := hits.Load(); n != 4 {
		t.Fatalf("%d callers: got %d eth_blockNumber, want 4 (one request with call()'s retries, shared by all)", len(errs), n)
	}
	for i, err := range errs[1:] {
		if err != errs[0] {
			t.Fatalf("waiter %d: got an error of its own (%v), want the very error value the asker got", i+1, err)
		}
	}
}
