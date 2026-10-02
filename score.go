package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Model scoring: the call's feature row (its scout_call_dataset_v row, the same
// columns the model was trained on) is sent to the scoring service in ml/serve.py;
// the scores are stored in scout_call_predictions and one line is added to the
// delivered message. Off unless SCOUT_MODEL_URL is set.
// ---------------------------------------------------------------------------

type scoreResponse struct {
	ModelVersion string `json:"model_version"`
	Line         string `json:"line"` // ready-made summary for the delivery header
	Buckets      []struct {
		Bucket        string   `json:"bucket"`
		RunnerProb    *float64 `json:"runner_prob"`
		CollapseProb  *float64 `json:"collapse_prob"`
		RunnerRankPct *float64 `json:"runner_rank_pct"`
	} `json:"buckets"`
}

var scoreHTTP = &http.Client{}

// scoreCall returns the line for the delivery header ("" when scoring is off,
// the service is unreachable, or the call is not in the database).
func (s *scanner) scoreCall(ctx context.Context, j job) string {
	if s.cfg.ModelURL == "" || s.db == nil || j.CallID == nil {
		return ""
	}
	callID := *j.CallID
	s.livePrecall(ctx, callID, j.CA)

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ModelTimeout)
	defer cancel()
	row, err := s.db.DatasetRowJSON(ctx, callID)
	if err != nil || row == nil {
		log.Printf("score %s: feature row: %v", j.CA, err)
		return ""
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"row": row})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.ModelURL+"/score", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := scoreHTTP.Do(req)
	if err != nil {
		log.Printf("score %s: %v (delivering without a score)", j.CA, err)
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		log.Printf("score %s: HTTP %s: %.200s", j.CA, resp.Status, raw)
		return ""
	}
	var sr scoreResponse
	if err := json.Unmarshal(raw, &sr); err != nil || sr.ModelVersion == "" {
		log.Printf("score %s: bad response: %.200s", j.CA, raw)
		return ""
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	for _, b := range sr.Buckets {
		p := ScoutCallPrediction{CallID: callID, ModelVersion: sr.ModelVersion, Bucket: b.Bucket,
			RunnerProb: b.RunnerProb, CollapseProb: b.CollapseProb, RunnerRankPct: b.RunnerRankPct}
		if err := s.db.UpsertPrediction(sctx, p, row); err != nil {
			log.Printf("db: prediction for call %d: %v", callID, err)
		}
	}
	log.Printf("score %s: %s", j.CA, sr.Line)
	return sr.Line
}

// livePrecall stores the pre-call trading stats of a new call right away, so
// they are part of the row that gets scored (the tracker would only reach the
// call an hour later). Best effort and time-boxed.
func (s *scanner) livePrecall(ctx context.Context, callID int, ca string) {
	if s.pc.Source != "onchain" || !s.pc.Enabled {
		return
	}
	if ok, err := s.db.HasPrecall(ctx, callID); err != nil || ok {
		return
	}
	tr, err := s.db.GetTracking(ctx, callID)
	if err != nil || tr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.storePrecall(ctx, callID, ca, tr.EntryAt); err != nil {
		log.Printf("score %s: pre-call trades unavailable: %v", ca, err)
	}
}

func (s *scanner) storePrecall(ctx context.Context, callID int, ca string, entryAt time.Time) error {
	o := s.onchain
	entryBlock, err := o.rpc.blockAt(ctx, entryAt.Unix())
	if err != nil {
		return fmt.Errorf("block at call time: %w", err)
	}
	st, err := o.discover(ctx, ca, entryBlock)
	if err != nil {
		return err
	}
	ps, err := o.precall(ctx, st, entryAt.Unix())
	if err != nil {
		return err
	}
	if q, ok, err := o.quoteUSD(ctx, st.Quote, entryBlock); err == nil && ok {
		ps.scale(q)
	}
	return s.db.UpsertPrecall(ctx, callID, ps)
}
