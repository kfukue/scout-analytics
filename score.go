package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		// Passed: the bucket's model passed its validation gates; nil when
		// the service did not say (stored as NULL, "not recorded")
		Passed *bool `json:"passed"`
	} `json:"buckets"`
}

var scoreHTTP = &http.Client{}

// scoreStoreTimeout bounds the writes of a score that arrived within the
// budget. It is the only wait added to SCOUT_MODEL_TIMEOUT before the delivery
// goes on, and it is not cut short by that budget, so a score that arrived in
// time is always stored.
const scoreStoreTimeout = 5 * time.Second

// livePrecallMax caps the live pre-call stats on their own; within scoreCall
// the SCOUT_MODEL_TIMEOUT budget (default 10s) is the tighter limit.
const livePrecallMax = 20 * time.Second

// scoreCall returns the line for the delivery header ("" when scoring is off,
// the service is unreachable or too slow, or the call is not in the database).
//
// Everything the delivery waits on here (live pre-call stats, the feature row,
// the scoring request) shares one SCOUT_MODEL_TIMEOUT budget; only the storing
// of a score that arrived in time (at most scoreStoreTimeout) comes on top.
func (s *scanner) scoreCall(ctx context.Context, j job) string {
	if s.cfg.ModelURL == "" || s.db == nil || j.CallID == nil {
		return ""
	}
	callID := *j.CallID
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ModelTimeout)
	defer cancel()
	start := time.Now()
	quoteSym := s.livePrecall(ctx, callID, j.CA)
	// Logged with the outcome so the owner can tune SCOUT_MODEL_TIMEOUT.
	precall := time.Since(start).Round(100 * time.Millisecond)

	row, err := s.db.DatasetRowJSON(ctx, callID)
	if err != nil {
		log.Printf("score %s: feature row: %v (precall %s; delivering without a score%s)", j.CA, err, precall, budgetNote(err, s.cfg.ModelTimeout))
		return ""
	}
	if row == nil {
		log.Printf("score %s: call %d is not in the dataset view (delivering without a score)", j.CA, callID)
		return ""
	}
	if filled, err := fillQuoteAsset(row, quoteSym); err != nil {
		log.Printf("score %s: quote asset not added to the feature row: %v", j.CA, err)
	} else {
		row = filled
	}
	body, err := json.Marshal(map[string]json.RawMessage{"row": row})
	if err != nil {
		log.Printf("score %s: encoding the request: %v (delivering without a score)", j.CA, err)
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.ModelURL+"/score", bytes.NewReader(body))
	if err != nil {
		log.Printf("score %s: %v (delivering without a score)", j.CA, err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := scoreHTTP.Do(req)
	if err != nil {
		log.Printf("score %s: %v (precall %s; delivering without a score%s)", j.CA, err, precall, budgetNote(err, s.cfg.ModelTimeout))
		return ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("score %s: reading the response: %v (precall %s; delivering without a score%s)", j.CA, err, precall, budgetNote(err, s.cfg.ModelTimeout))
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("score %s: HTTP %s: %.200s", j.CA, resp.Status, raw)
		return ""
	}
	var sr scoreResponse
	if err := json.Unmarshal(raw, &sr); err != nil || sr.ModelVersion == "" {
		log.Printf("score %s: bad response: %.200s", j.CA, raw)
		return ""
	}
	// The score arrived in time: store it even if the budget runs out now.
	// All buckets in one transaction (one created_at, the time the website
	// shows), then one NOTIFY scout_events "score" so an open website shows
	// the score at once.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), scoreStoreTimeout)
	defer scancel()
	ps := make([]ScoutCallPrediction, 0, len(sr.Buckets))
	for _, b := range sr.Buckets {
		ps = append(ps, ScoutCallPrediction{CallID: callID, ModelVersion: sr.ModelVersion, Bucket: b.Bucket,
			RunnerProb: b.RunnerProb, CollapseProb: b.CollapseProb, RunnerRankPct: b.RunnerRankPct, Passed: b.Passed})
	}
	if len(ps) > 0 {
		if err := s.db.StorePredictions(sctx, ps, row); err != nil {
			log.Printf("db: predictions for call %d: %v", callID, err)
		} else {
			s.db.notifyScoutEvent(sctx, scoutEvent{Kind: scoutEventScore, CallID: callID})
		}
	}
	if sr.Line == "" {
		log.Printf("score %s: model %s sent no line (precall %s)", j.CA, sr.ModelVersion, precall)
		return ""
	}
	log.Printf("score %s: %s (precall %s)", j.CA, sr.Line, precall)
	return sr.Line
}

// budgetNote explains an error caused by the scoring budget running out.
func budgetNote(err error, budget time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf(": no score within SCOUT_MODEL_TIMEOUT %s", budget)
	}
	return ""
}

// fillQuoteAsset returns the feature row (a scout_call_dataset_v row as a JSON
// object) with quote_asset set to sym when the row has none (null, "" or
// absent) and sym is known; otherwise the row unchanged.
//
// The view takes quote_asset from scout_call_tracking.onchain->>'quote_sym' as
// is (onchainState.QuoteSym: the quote token's symbol as tokenInfo reads it,
// "ETH" for native ETH), which only the tracker writes, about an hour after the
// call. Live pre-call runs the same pool discovery, so sym is normally the same
// value the view shows once the tracker has run (the tracker's later discovery
// sees a wider transfer window and can occasionally pick a different pool), and
// the live row then matches the training rows. scout_call_tracking
// itself is left to the tracker.
func fillQuoteAsset(row []byte, sym string) ([]byte, error) {
	if sym == "" {
		return row, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(row, &m); err != nil {
		return nil, fmt.Errorf("decoding the feature row: %w", err)
	}
	if m == nil {
		return nil, errors.New("feature row is not a JSON object")
	}
	if cur, ok := m["quote_asset"]; ok {
		var v *string
		if err := json.Unmarshal(cur, &v); err != nil {
			return nil, fmt.Errorf("decoding quote_asset: %w", err)
		}
		if v != nil && *v != "" {
			return row, nil
		}
	}
	q, err := json.Marshal(sym)
	if err != nil {
		return nil, fmt.Errorf("encoding quote_asset: %w", err)
	}
	m["quote_asset"] = q
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding the feature row: %w", err)
	}
	return out, nil
}

// livePrecall stores the pre-call trading stats of a new call right away, so
// they are part of the row that gets scored (the tracker would only reach the
// call an hour later). It returns the symbol of the quote asset of the pool it
// found ("" when it found none or did not look). Best effort, within ctx's
// deadline (and at most livePrecallMax).
func (s *scanner) livePrecall(ctx context.Context, callID int, ca string) string {
	if s.pc.Source != "onchain" || !s.pc.Enabled {
		return ""
	}
	ok, err := s.db.HasPrecall(ctx, callID)
	if err != nil {
		log.Printf("score %s: pre-call stats: %v", ca, err)
		return ""
	}
	if ok {
		return ""
	}
	tr, err := s.db.GetTracking(ctx, callID)
	if err != nil {
		log.Printf("score %s: tracking row: %v", ca, err)
		return ""
	}
	if tr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, livePrecallMax)
	defer cancel()
	sym, err := s.storePrecall(ctx, callID, ca, tr.EntryAt)
	if err != nil {
		log.Printf("score %s: pre-call trades unavailable: %v", ca, err)
	}
	return sym
}

// storePrecall finds the call's pool and stores its pre-call stats. It returns
// the pool's quote symbol once the pool is found, also when a later step fails.
func (s *scanner) storePrecall(ctx context.Context, callID int, ca string, entryAt time.Time) (string, error) {
	o := s.onchain
	entryBlock, err := o.rpc.blockAt(ctx, entryAt.Unix())
	if err != nil {
		return "", fmt.Errorf("block at call time: %w", err)
	}
	st, err := o.discover(ctx, ca, entryBlock)
	if err != nil {
		return "", fmt.Errorf("pool discovery: %w", err)
	}
	ps, err := o.precall(ctx, st, entryAt.Unix())
	if err != nil {
		return st.QuoteSym, fmt.Errorf("trades before the call: %w", err)
	}
	// Without a USD price of the quote asset the stats stay in quote units
	// (vol_unit = the quote symbol).
	if q, ok, err := o.quoteUSD(ctx, st.Quote, entryBlock); err == nil && ok {
		ps.scale(q)
	}
	if err := s.db.UpsertPrecall(ctx, callID, ps); err != nil {
		return st.QuoteSym, fmt.Errorf("storing pre-call stats: %w", err)
	}
	return st.QuoteSym, nil
}
