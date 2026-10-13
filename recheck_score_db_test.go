package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recheckScoreLine is a score line as the scoring service sends it with
// SCOUT_MODEL_LINE=all (one bucket not validated).
const recheckScoreLine = "Model 20261010-0621 · 1d (not validated): runner 31%, collapse 44% · 7d: runner 12%, collapse 64%"

// noModel points the scanner's scoring at a service that fails the test when
// asked: the re-check never scores again. It returns the number of requests.
func noModel(t *testing.T, f *recheckFixture) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(w, "the re-check must not score", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	f.s.cfg.ModelURL, f.s.cfg.ModelTimeout = srv.URL, 2*time.Second
	return &asked
}

// heldResults are the live results of a call Perceptor answered "not enough
// data yet" for (sAlpha completed).
func heldResults(f *recheckFixture) []*toolResult {
	return []*toolResult{
		{Spec: f.s.runners[0].spec, Status: investigationCompleted, Verdict: verdict{Level: levelInsufficient}},
		{Spec: f.s.runners[1].spec, Status: investigationCompleted, Verdict: verdict{Level: levelUnknown}},
	}
}

// The re-check delivery ends with the model score line computed at the call
// (as a delivery at the call would), under the re-check line; the model is
// never asked again. After a restart of the listener (a new lane, nothing in
// memory) the line stored with the held marker is delivered, exactly the
// first-pass line; the line in memory goes first. Without a line at the call
// it is delivered without one, after a restart too. A re-check with red flags
// delivers nothing and forgets the line.
func TestRecheckScoreLine(t *testing.T) {
	ca := "0x1616161616161616161616161616161616161616"
	clean := verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}
	const kept = "Model v2 · 7d: runner 50%" // a line in memory that differs from the stored one
	cases := []struct {
		name      string
		line      string // the score line computed at the call
		judged    verdict
		restart   bool   // the re-check runs in a new lane (a restarted listener)
		memory    string // replaces the line in memory after the hold ("" = left as kept)
		want      string // the header's last line ("" = no score line)
		delivered bool
	}{
		{"same process: the line from the call", recheckScoreLine, clean, false, "", recheckScoreLine, true},
		{"still insufficient: delivered with the line", recheckScoreLine,
			verdict{Level: levelInsufficient, Label: labelInsufficient, Ticker: "$SNIFF"}, false, "", recheckScoreLine, true},
		{"no score at the call: no line", "", clean, false, "", "", true},
		{"after a restart: the stored line", recheckScoreLine, clean, true, "", recheckScoreLine, true},
		{"after a restart, still insufficient: the stored line", recheckScoreLine,
			verdict{Level: levelInsufficient, Label: labelInsufficient, Ticker: "$SNIFF"}, true, "", recheckScoreLine, true},
		{"after a restart, no score at the call: no line", "", clean, true, "", "", true},
		{"the line in memory goes before the stored one", recheckScoreLine, clean, false, kept, kept, true},
		{"red flags: not delivered", recheckScoreLine, verdict{Level: levelRedFlags, Label: "Red flags", Ticker: "$SNIFF"}, false, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			ctx := context.Background()
			asked := noModel(t, f)
			call, inv := f.unheldCall(t, 100, ca, 13*time.Minute, levelInsufficient)
			var sent []string
			held := f.lane(recheckReply, c.judged, &sent)
			f.s.recheck = held
			if !f.s.deferToRecheck(ctx, job{CA: ca, CallID: &call}, heldResults(f), &inv, c.line) {
				t.Fatal("deferToRecheck = false, want the call held")
			}
			if c.memory != "" {
				held.keepScore(call, c.memory)
			}
			l := held
			if c.restart {
				l = f.lane(recheckReply, c.judged, &sent)
				if got := l.scoreLine(call); got != "" {
					t.Fatalf("restarted lane has score line %q in memory, want none", got)
				}
			}
			if !l.step(ctx, func() int { return 0 }) {
				t.Fatal("step() = false, want the re-check")
			}
			if n := asked.Load(); n != 0 {
				t.Errorf("the scoring service was asked %d time(s) by the re-check, want 0", n)
			}
			if got := held.scoreLine(call); got != "" && !c.restart {
				t.Errorf("score line still kept %q after the re-check decided, want it forgotten", got)
			}
			dels := f.deliveries(t, call)
			if !c.delivered {
				if len(f.headers) != 0 || len(dels) != 0 {
					t.Fatalf("delivered %d time(s), %d rows; want none", len(f.headers), len(dels))
				}
				return
			}
			if len(f.headers) != 1 || len(dels) != 1 || dels[0].HeaderText != f.headers[0] {
				t.Fatalf("%d sent, rows %+v; want one, its stored header the one sent", len(f.headers), dels)
			}
			checkRecheckHeader(t, f.headers[0], c.want)
		})
	}
}

// checkRecheckHeader checks a re-check delivery header: the re-check line
// second, then the header as at the call, ending with the source link and
// then score line want ("" = no score line at all).
func checkRecheckHeader(t *testing.T, h, want string) {
	t.Helper()
	lines := strings.Split(h, "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[1], "🔁 Perceptor re-checked ") {
		t.Fatalf("header %q: want the re-check line second", h)
	}
	last := len(lines) - 1
	if want == "" {
		if strings.Contains(h, "Model ") || !strings.HasPrefix(lines[last], "Source: ") {
			t.Errorf("header %q: want no score line (the source link last)", h)
		}
		return
	}
	if lines[last] != want || !strings.HasPrefix(lines[last-1], "Source: ") {
		t.Errorf("header ends %q / %q, want the source link then %q:\n%s", lines[last-1], lines[last], want, h)
	}
	if n := strings.Count(h, want); n != 1 {
		t.Errorf("score line appears %d times, want once:\n%s", n, h)
	}
}

// A re-check delivery stopped before anything was sent keeps the score line:
// the stored re-check delivered on a later step (recheckDeliver, Perceptor not
// asked again) shows it, also when that later step runs in a restarted
// listener (the stored line). A kept line whose delivery can no longer come
// is given up.
func TestRecheckScoreLineKept(t *testing.T) {
	ca := "0x1717171717171717171717171717171717171717"
	t.Run("delivery stopped before sending, delivered after a restart", func(t *testing.T) {
		f := newRecheckFixture(t)
		asked := noModel(t, f)
		call, inv := f.unheldCall(t, 111, ca, 13*time.Minute, levelInsufficient)
		var sent []string
		judged := verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}
		l := f.lane(recheckReply, judged, &sent)
		f.s.recheck = l
		if !f.s.deferToRecheck(context.Background(), job{CA: ca, CallID: &call}, heldResults(f), &inv, recheckScoreLine) {
			t.Fatal("deferToRecheck = false, want the call held")
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
			cancel()
			return 0, &deliveryNotSentError{fmt.Errorf("not sent: %w", ctx.Err())}
		}
		if !l.step(ctx, func() int { return 0 }) {
			t.Fatal("step() = false, want a re-check")
		}
		if d := f.deliveries(t, call); len(d) != 0 {
			t.Fatalf("%d delivery rows after a stop before sending, want none", len(d))
		}
		restarted := f.lane(recheckReply, judged, &sent) // nothing in memory
		if !restarted.step(context.Background(), func() int { return 0 }) {
			t.Fatal("step() of the restarted lane = false, want the stored re-check delivered")
		}
		if len(sent) != 1 || len(f.headers) != 1 {
			t.Fatalf("sent %q, %d deliveries; want one /scan (before the restart) and one delivery", sent, len(f.headers))
		}
		checkRecheckHeader(t, f.headers[0], recheckScoreLine)
		if d := f.deliveries(t, call); len(d) != 1 || d[0].HeaderText != f.headers[0] {
			t.Errorf("delivery rows %+v, want one whose header is the one sent", d)
		}
		if n := asked.Load(); n != 0 {
			t.Errorf("the scoring service was asked %d time(s), want 0", n)
		}
	})
	t.Run("delivery stopped before sending, delivered later", func(t *testing.T) {
		f := newRecheckFixture(t)
		asked := noModel(t, f)
		call, inv := f.unheldCall(t, 110, ca, 13*time.Minute, levelInsufficient)
		var sent []string
		l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
		f.s.recheck = l
		if !f.s.deferToRecheck(context.Background(), job{CA: ca, CallID: &call}, heldResults(f), &inv, recheckScoreLine) {
			t.Fatal("deferToRecheck = false, want the call held")
		}
		ok := l.deliver
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		l.deliver = func(ctx context.Context, j job, header string, results []*toolResult) (int64, error) {
			cancel()
			return 0, &deliveryNotSentError{fmt.Errorf("not sent: %w", ctx.Err())}
		}
		if !l.step(ctx, func() int { return 0 }) {
			t.Fatal("step() = false, want a re-check")
		}
		if d := f.deliveries(t, call); len(d) != 0 {
			t.Fatalf("%d delivery rows after a stop before sending, want none", len(d))
		}
		if got := l.scoreLine(call); got != recheckScoreLine {
			t.Fatalf("score line after a stop before sending %q, want %q kept", got, recheckScoreLine)
		}
		l.deliver = ok
		if !l.step(context.Background(), func() int { return 0 }) {
			t.Fatal("step() after the stop = false, want the stored re-check delivered")
		}
		if len(sent) != 1 || len(f.headers) != 1 {
			t.Fatalf("sent %q, %d deliveries; want one /scan and one delivery", sent, len(f.headers))
		}
		checkRecheckHeader(t, f.headers[0], recheckScoreLine)
		if n := asked.Load(); n != 0 {
			t.Errorf("the scoring service was asked %d time(s), want 0", n)
		}
	})
	t.Run("given up after the last possible delivery", func(t *testing.T) {
		f := newRecheckFixture(t)
		var sent []string
		l := f.lane(recheckReply, verdict{Level: levelClean}, &sent)
		now := time.Now()
		l.now = func() time.Time { return now }
		l.keepScore(1, "Model v1 · 7d: runner 1%")
		l.keepScore(2, "Model v1 · 7d: runner 2%")
		l.scores[1] = heldScore{line: l.scores[1].line, at: now.Add(-(l.cfg.maxAge1() + recheckOwedMax + time.Minute))}
		l.step(context.Background(), func() int { return 0 })
		if got := l.scoreLine(1); got != "" {
			t.Errorf("old score line still kept %q, want given up", got)
		}
		if got := l.scoreLine(2); got != "Model v1 · 7d: runner 2%" {
			t.Errorf("recent score line %q, want kept", got)
		}
	})
}
