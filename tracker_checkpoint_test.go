package main

import (
	"context"
	"testing"
	"time"
)

// segmentPieceEnd splits a horizon segment into pieces that cover it without
// a gap or an overlap, each (but the last) ending just before the first block
// of a UTC hour as timeOfBlock places blocks, so no candle straddles two
// pieces; a segment shorter than a piece is one piece.
func TestSegmentPieceEnd(t *testing.T) {
	ctx := context.Background()
	old := segmentPieceChunks
	t.Cleanup(func() { segmentPieceChunks = old })
	f := newFakeChain(t, 40*24*time.Hour)
	for _, c := range []struct {
		name   string
		chunk  string // SCOUT_RPC_LOG_CHUNK
		pieces uint64 // segmentPieceChunks
		span   time.Duration
		min    int // pieces expected at least
	}{
		{"one piece (segment shorter)", "200000", 32, 24 * time.Hour, 1},
		{"2-hour pieces over a day", "18000", 4, 24 * time.Hour, 10},
		{"pieces shorter than an hour", "1000", 1, 6 * time.Hour, 5},
		{"off", "18000", 0, 24 * time.Hour, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			segmentPieceChunks = c.pieces
			o := testOnchain(t, f, map[string]string{"SCOUT_RPC_LOG_CHUNK": c.chunk})
			s := &scanner{onchain: o}
			start := time.Now().Add(-10 * 24 * time.Hour)
			from := f.blockAtTime(start) + 7 // not on an hour
			hBlock := f.blockAtTime(start.Add(c.span))
			var ends []uint64
			tFrom, tEnd := f.timeOf(from), f.timeOf(hBlock)
			for cur := from; cur < hBlock; {
				to, toHour, err := s.segmentPieceEnd(ctx, cur, hBlock, tFrom, tEnd)
				if err != nil {
					t.Fatal(err)
				}
				if to <= cur || to > hBlock {
					t.Fatalf("piece after block %d (segment end %d): got end %d, want in (%d, %d]", cur, hBlock, to, cur, hBlock)
				}
				if to < hBlock {
					next, err := o.timeOfBlock(ctx, to+1, f.timeOf(to+1))
					if err != nil {
						t.Fatal(err)
					}
					last, err := o.timeOfBlock(ctx, to, f.timeOf(to))
					if err != nil {
						t.Fatal(err)
					}
					if next%3600 != 0 || last/3600 == next/3600 || toHour != next {
						t.Fatalf("piece end %d (hour %d): timeOfBlock gives %d for it and %d for the next block, want the next block to start that hour", to, toHour, last, next)
					}
					tFrom = toHour
				}
				ends = append(ends, to)
				cur = to
			}
			if len(ends) < c.min || (c.pieces == 0 && len(ends) != 1) {
				t.Fatalf("got %d piece(s) %v, want at least %d (one when off)", len(ends), ends, c.min)
			}
		})
	}
}
