package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The tracker's rolling queue (trackRun) without a database: a fake list of
// due rows stands in for DueTracking, and a fake job for one horizon check.
// ---------------------------------------------------------------------------

type fakeDueRow struct {
	id, prio int
	next     time.Time
	done     bool // saved: no longer due
}

// fakeDue is DueTracking over an in-memory list: rows not done, ordered by
// priority, then next check, then id. A row stays due until its job saves it
// (finish), as in the database.
type fakeDue struct {
	mu       sync.Mutex
	rows     map[int]*fakeDueRow
	handed   []int       // call ids in the order their jobs started
	started  map[int]int // hand-outs per call id
	running  int
	finished chan int // every finished job's id

	// afterRead, when set, runs after a read took its snapshot and before it
	// returns (running: jobs in progress at that moment).
	afterRead func(running int)
	// job, when set, is the work of one call (before it is saved).
	job func(ctx context.Context, id int)
}

func newFakeDue() *fakeDue {
	return &fakeDue{rows: map[int]*fakeDueRow{}, started: map[int]int{}, finished: make(chan int, 1000)}
}

func (f *fakeDue) add(id, prio int, next time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id] = &fakeDueRow{id: id, prio: prio, next: next}
}

func (f *fakeDue) due(_ context.Context, n int) ([]ScoutCallTracking, error) {
	f.mu.Lock()
	now := time.Now()
	var rs []*fakeDueRow
	for _, r := range f.rows {
		if !r.done && !r.next.After(now) {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].prio != rs[j].prio {
			return rs[i].prio < rs[j].prio
		}
		if !rs[i].next.Equal(rs[j].next) {
			return rs[i].next.Before(rs[j].next)
		}
		return rs[i].id < rs[j].id
	})
	var out []ScoutCallTracking
	for _, r := range rs {
		if len(out) == n {
			break
		}
		out = append(out, ScoutCallTracking{CallID: r.id, Priority: r.prio, NextCheckAt: r.next, Status: TrackTracking})
	}
	running := f.running
	hook := f.afterRead
	f.mu.Unlock()
	if hook != nil {
		hook(running)
	}
	return out, nil
}

func (f *fakeDue) track(ctx context.Context, t *ScoutCallTracking, _ string) {
	f.mu.Lock()
	f.handed = append(f.handed, t.CallID)
	f.started[t.CallID]++
	f.running++
	job := f.job
	f.mu.Unlock()
	if job != nil {
		job(ctx, t.CallID)
	}
	f.mu.Lock()
	f.running--
	if ctx.Err() == nil {
		f.rows[t.CallID].done = true // saved (an interrupted call puts its row back as it was)
	}
	f.mu.Unlock()
	f.finished <- t.CallID
}

func (f *fakeDue) order() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.handed)
}

// queueRun is a trackRun with workers workers over f.
func queueRun(t *testing.T, f *fakeDue, workers int) *trackRun {
	t.Helper()
	s := &scanner{}
	s.pc.Source, s.pc.Workers = "onchain", workers
	r := s.newTrackRun()
	r.dueHorizon = f.due
	r.trackRow = f.track
	return r
}

// waitFinished waits for n jobs to finish and returns their ids.
func waitFinished(t *testing.T, f *fakeDue, n int) []int {
	t.Helper()
	var ids []int
	timeout := time.After(10 * time.Second)
	for len(ids) < n {
		select {
		case id := <-f.finished:
			ids = append(ids, id)
		case <-timeout:
			t.Fatalf("got %d finished calls %v, want %d", len(ids), ids, n)
		}
	}
	return ids
}

// One call that takes long holds up only its own worker: the others go on
// with the rest of the queue, and with calls handed out by the next cycle.
func TestTrackQueueSlowCallDoesNotBlockOthers(t *testing.T) {
	f := newFakeDue()
	past := time.Now().Add(-time.Hour)
	for id := 1; id <= 10; id++ {
		f.add(id, 1, past.Add(time.Duration(id)*time.Second))
	}
	slow := make(chan struct{})
	f.job = func(ctx context.Context, id int) {
		if id == 1 {
			select {
			case <-slow:
			case <-ctx.Done():
			}
		}
	}
	r := queueRun(t, f, 3)
	t.Cleanup(func() {
		close(slow)
		r.close()
	})
	ctx := context.Background()
	if handed, more := r.horizon(ctx, 50); handed != 10 || more {
		t.Fatalf("first cycle: got %d handed out, more %v; want 10, false", handed, more)
	}
	if got := waitFinished(t, f, 9); slices.Contains(got, 1) {
		t.Fatalf("the slow call finished: %v", got)
	}
	// The next cycle, while call 1 still runs: new due calls go to the free workers.
	for id := 11; id <= 14; id++ {
		f.add(id, 1, past)
	}
	if handed, more := r.horizon(ctx, 50); handed != 4 || more {
		t.Fatalf("second cycle: got %d handed out, more %v; want 4, false", handed, more)
	}
	got := waitFinished(t, f, 4)
	slices.Sort(got)
	if want := []int{11, 12, 13, 14}; !slices.Equal(got, want) {
		t.Fatalf("second cycle finished %v, want %v (call 1 still running)", got, want)
	}
	// A worker gives its call back only after the job has returned (and sent
	// on f.finished): wait for the count to settle instead of reading it once.
	waitHorizonBusy(t, r, 1)
}

// waitHorizonBusy waits until r.horizonBusy() is want, for up to 5 seconds.
func waitHorizonBusy(t *testing.T, r *trackRun, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := r.horizonBusy()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d calls in progress, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A row read while its worker is still on it, or read just before its worker
// saved it, is never handed out a second time.
func TestTrackQueueNoDoubleHandOut(t *testing.T) {
	for _, workers := range []int{1, 4, 12} {
		t.Run(fmt.Sprintf("%d workers", workers), func(t *testing.T) {
			f := newFakeDue()
			past := time.Now().Add(-time.Hour)
			const rows = 60
			for id := 1; id <= rows; id++ {
				f.add(id, id%2, past.Add(time.Duration(id)*time.Second))
			}
			f.job = func(ctx context.Context, id int) { time.Sleep(time.Duration(id%3) * time.Millisecond) }
			// Every read with calls in progress waits until one of them is
			// saved: its copy of that row is out of date when it returns.
			var mu sync.Mutex
			stale := 0
			f.afterRead = func(running int) {
				if running == 0 {
					return
				}
				select {
				case id := <-f.finished:
					f.finished <- id // put it back for the count below
					mu.Lock()
					stale++
					mu.Unlock()
				case <-time.After(time.Second):
				}
			}
			r := queueRun(t, f, workers)
			handed, more := r.horizon(context.Background(), 1000)
			r.close()
			if handed != rows || more {
				t.Fatalf("got %d handed out, more %v; want %d, false", handed, more, rows)
			}
			for id := 1; id <= rows; id++ {
				if n := f.started[id]; n != 1 {
					t.Errorf("call %d handed out %d times, want 1", id, n)
				}
			}
			if workers > 1 && stale == 0 {
				t.Fatal("no read returned a row saved while it ran: the test checked nothing")
			}
		})
	}
}

// Rows go out in the order of DueTracking (priority, then next check), and a
// live call that becomes due is read with the next small chunk, ahead of the
// older calls still waiting in the database.
func TestTrackQueueLiveFirst(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name    string
		workers int
		late    bool // a live call (priority 0) becomes due while the first call runs
		want    []int
	}{
		{"order, one worker", 1, false, []int{20, 21, 1, 2, 3, 4, 5, 6, 7, 8}},
		// one worker reads trackChunkMin (4) rows at a time: the live call 99
		// comes right after the first chunk.
		{"live call, one worker", 1, true, []int{20, 21, 1, 2, 99, 3, 4, 5, 6, 7, 8}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDue()
			for id := 1; id <= 8; id++ {
				f.add(id, 1, base.Add(time.Duration(id)*time.Minute))
			}
			f.add(20, 0, base.Add(30*time.Minute)) // live calls: first, whatever their time
			f.add(21, 0, base.Add(40*time.Minute))
			first := true
			f.job = func(ctx context.Context, id int) {
				if c.late && first {
					first = false
					f.add(99, 0, time.Now())
				}
			}
			r := queueRun(t, f, c.workers)
			r.horizon(context.Background(), 100)
			r.close()
			if got := f.order(); !slices.Equal(got, c.want) {
				t.Fatalf("got order %v, want %v", got, c.want)
			}
		})
	}
}

// Ctrl+C: no call is handed out after the stop, the calls in progress are
// told (their context is done) and close waits for them.
func TestTrackQueueShutdown(t *testing.T) {
	f := newFakeDue()
	past := time.Now().Add(-time.Hour)
	for id := 1; id <= 20; id++ {
		f.add(id, 1, past.Add(time.Duration(id)*time.Second))
	}
	const workers = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := make(chan int, 100)
	var stopped sync.WaitGroup
	f.job = func(ctx context.Context, id int) {
		stopped.Add(1)
		defer stopped.Done()
		running <- id
		<-ctx.Done()
	}
	r := queueRun(t, f, workers)
	type result struct{ handed int }
	res := make(chan result, 1)
	go func() {
		n, _ := r.horizon(ctx, 100)
		res <- result{n}
	}()
	for range workers {
		select {
		case <-running:
		case <-time.After(10 * time.Second):
			t.Fatal("the workers did not start")
		}
	}
	cancel()
	var got result
	select {
	case got = <-res:
	case <-time.After(10 * time.Second):
		t.Fatal("horizon did not return after the stop")
	}
	closed := make(chan struct{})
	go func() {
		r.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after the stop")
	}
	stopped.Wait()
	if got.handed != workers || len(f.order()) != workers {
		t.Fatalf("got %d handed out and %d started, want %d of each (one per worker, none after the stop)", got.handed, len(f.order()), workers)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, row := range f.rows {
		if row.done {
			t.Errorf("call %d was saved as done after the stop", id)
		}
	}
	if n := len(f.started); n != workers {
		t.Fatalf("got %d calls started, want %d", n, workers)
	}
}

// fetch drops rows already claimed (with a worker, or waiting in the
// dispatcher) and rows released while the read ran; release forgets a row's
// deferral once its horizon check is done.
func TestTrackRunFetch(t *testing.T) {
	s := &scanner{}
	s.pc.Source, s.pc.Workers = "onchain", 2
	r := s.newTrackRun()
	t.Cleanup(r.close)
	rowsOf := func(ids ...int) []ScoutCallTracking {
		var out []ScoutCallTracking
		for _, id := range ids {
			out = append(out, ScoutCallTracking{CallID: id})
		}
		return out
	}
	ids := func(rows []ScoutCallTracking) []int {
		var out []int
		for _, t := range rows {
			out = append(out, t.CallID)
		}
		return out
	}
	got, read, err := r.fetch(true, func(claimed []int) ([]ScoutCallTracking, error) {
		if len(claimed) != 0 {
			t.Errorf("first read: got claimed %v, want none", claimed)
		}
		return rowsOf(1, 2, 3), nil
	})
	if err != nil || read != 3 || !slices.Equal(ids(got), []int{1, 2, 3}) {
		t.Fatalf("first read: got %v (%d read, %v), want [1 2 3]", ids(got), read, err)
	}
	r.deferLatest([]int{2, 7}, time.Now())
	got, _, _ = r.fetch(false, func(claimed []int) ([]ScoutCallTracking, error) {
		slices.Sort(claimed)
		if !slices.Equal(claimed, []int{1, 2, 3}) {
			t.Errorf("second read: got claimed %v, want [1 2 3]", claimed)
		}
		r.release(2, true) // saved while this read ran
		r.release(4, false)
		return rowsOf(1, 2, 4, 5), nil
	})
	if want := []int{5}; !slices.Equal(ids(got), want) {
		t.Fatalf("second read: got %v, want %v (1 claimed, 2 and 4 released during the read)", ids(got), want)
	}
	if d := r.deferredIDs(time.Now()); !slices.Equal(d, []int{7}) {
		t.Fatalf("got deferred %v, want [7] (2's horizon check is done)", d)
	}
	if d := r.deferredIDs(time.Now().Add(latestDeferFor + time.Second)); len(d) != 0 {
		t.Fatalf("got deferred %v after the wait, want none", d)
	}
	got, _, _ = r.fetch(true, func([]int) ([]ScoutCallTracking, error) { return rowsOf(2, 4), nil })
	if want := []int{2, 4}; !slices.Equal(ids(got), want) {
		t.Fatalf("third read: got %v, want %v (released before the read)", ids(got), want)
	}
}
