package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		w.Write([]byte(`{"model_version":"v3","line":"Model v3 · short: runner 31%, collapse 44%",
			"buckets":[{"bucket":"short","runner_prob":0.31,"collapse_prob":0.44,"runner_rank_pct":92.5},
			           {"bucket":"long","runner_prob":0.06,"collapse_prob":null,"runner_rank_pct":null}]}`))
	}))
	cfg := &config{SourceChannel: "scoutrobinhood", Chains: map[string]bool{"evm": true}, StateDir: t.TempDir(),
		ModelURL: srv.URL, ModelTimeout: 2 * time.Second, Price: priceConfig{Enabled: false}}
	s := newScanner(cfg)
	s.db = st
	s.sourceChannelID = 777
	s.onChannelPost(postAt(1, time.Now().Add(-time.Minute), "0x4444444444444444444444444444444444444444"))
	j := <-s.queue

	line := s.scoreCall(ctx, j)
	if !strings.HasPrefix(line, "Model v3") {
		t.Fatalf("line %q", line)
	}
	if got["holders"] != float64(1150) || got["proof_elite"] != float64(3) || got["contract_address"] != j.CA {
		t.Fatalf("row sent to the model: %v", got)
	}
	ps, err := st.PredictionsForCall(ctx, *j.CallID)
	if err != nil || len(ps) != 2 || ps[0].Bucket != "short" || *ps[0].RunnerProb != 0.31 || *ps[0].RunnerRankPct != 92.5 ||
		ps[1].CollapseProb != nil {
		t.Fatalf("stored %+v %v", ps, err)
	}

	srv.Close() // service down: no line, no error
	if line := s.scoreCall(ctx, j); line != "" {
		t.Fatalf("line with the service down: %q", line)
	}
	s.cfg.ModelURL = "" // off
	if line := s.scoreCall(ctx, j); line != "" {
		t.Fatal("scoring should be off")
	}
}
