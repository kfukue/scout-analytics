package main

import (
	"context"
	"testing"
	"time"
)

// The Perceptor re-check (12 minutes after the call) uses the same runner as
// live calls: a late reply to an earlier request that arrives during it is
// not stored, judged or delivered as the re-check's report; the stored row's
// details name the request and the ignored reply.
func TestRecheckIgnoresLateReplyDB(t *testing.T) {
	f := newRecheckFixture(t)
	ctx := context.Background()
	ca := "0x7777777777777777777777777777777777777777"
	call := f.call(t, 70, ca, 13*time.Minute, levelInsufficient)
	var sent []string
	l := f.lane(recheckReply, verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}, &sent)
	bot := &laneBot{next: 700, lateTo: 600, late: lateReport, report: recheckReply}
	var judged []string
	l.send = bot.send
	l.judge = func(ctx context.Context, spec ToolSpec, res *toolResult) verdict {
		judged = append(judged, res.ReportText())
		return verdict{Level: levelClean, Label: "No red flags found", Ticker: "$SNIFF"}
	}
	if !l.step(ctx, func() int { return 0 }) {
		t.Fatal("step() = false, want the due re-check")
	}
	if len(judged) != 1 || judged[0] != recheckReply {
		t.Errorf("judged %q, want only %q", judged, recheckReply)
	}
	rows := f.percRows(t, call)
	if len(rows) != 2 || rows[1].ScanKind != ScanKindLive || rows[1].VerdictLevel != levelClean {
		t.Fatalf("call %d Perceptor rows %+v, want the first answer and a live clean re-check", call, rows)
	}
	checkLaneRow(t, &rows[1], 700, 600, recheckReply)
	if len(f.headers) != 1 {
		t.Errorf("%d deliveries, want 1", len(f.headers))
	}
}
