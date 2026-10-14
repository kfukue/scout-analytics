package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// Model insights (GET /api/model-live): the model's scores stored at the time
// of each first call, one compact row per scored call, built once per
// snapshot from the scores the rows already carry (attachWebModel and
// webShownModel: the call's own score, stored within webModelMaxDelay, of the
// earliest version, finite numbers only). No outcomes: the page joins these
// rows to those of GET /api/analytics by call_id. Never the stored features.
// Only a website that shows model scores (webConfig.ModelScores) builds or
// serves it; elsewhere the path answers 404 like any unknown /api/ path.
// ---------------------------------------------------------------------------

// webModelLiveFormat: the shape of the body (field "format"); raise it when
// the columns change (or what one means), so the page can tell.
const webModelLiveFormat = 1

// webModelLivePerBucket: the values per bucket in a row (runner, collapse,
// rank, passed).
const webModelLivePerBucket = 4

// webModelLiveColumns names the values of each row of GET /api/model-live, in
// order: the call id, an index into "versions", the seconds from the call to
// the score, then per bucket of ScoutWebModelBuckets runner_prob,
// collapse_prob, runner_rank_pct (null when missing or not finite) and passed
// (true, false, null = not recorded; all four null when the score has no such
// bucket).
var webModelLiveColumns = func() []string {
	cols := []string{"call_id", "version", "after_s"}
	for _, b := range ScoutWebModelBuckets {
		cols = append(cols, "runner_"+b, "collapse_"+b, "rank_"+b, "passed_"+b)
	}
	return cols
}()

// webModelLiveBody is the body of GET /api/model-live. FirstCalls counts the
// snapshot's first calls (one per token), Scored the rows (first calls with a
// score shown); Versions is a dictionary of model versions, most frequent
// first, that a row's "version" indexes.
type webModelLiveBody struct {
	Format     int             `json:"format"`
	Buckets    []string        `json:"buckets"`
	Horizons   []string        `json:"horizons"`
	FirstCalls int             `json:"first_calls"`
	Scored     int             `json:"scored"`
	Versions   []string        `json:"versions"`
	Columns    []string        `json:"columns"`
	Rows       json.RawMessage `json:"rows"`
}

// webModelLive is the encoded body of GET /api/model-live for one snapshot.
// It is never changed once built.
type webModelLive struct {
	plain []byte
	gz    []byte // compressed; nil when that is not smaller
	etag  string // weak; a hash of plain
}

// webModelVersions numbers the model versions of the scored rows.
func webModelVersions(rows []ScoutWebRow) *webDict {
	d := newWebDict()
	for i := range rows {
		if m := rows[i].Model; m != nil {
			d.add(m.Version)
		}
	}
	d.freeze()
	return d
}

// appendModelLiveNum appends a model number (finite already; null when
// missing or, as a guard, not finite).
func appendModelLiveNum(b []byte, p *float64) []byte {
	if p = finite(p); p == nil {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, *p, 'g', -1, 64)
}

// appendModelLivePassed appends passed in its three states.
func appendModelLivePassed(b []byte, p *bool) []byte {
	if p == nil {
		return append(b, "null"...)
	}
	return strconv.AppendBool(b, *p)
}

// appendModelLiveBucket appends one bucket's values, each after a comma.
func appendModelLiveBucket(b []byte, v ScoutWebModelBucket) []byte {
	if !v.Has {
		return append(b, ",null,null,null,null"...)
	}
	for _, p := range [...]*float64{v.Runner, v.Collapse, v.Rank} {
		b = append(b, ',')
		b = appendModelLiveNum(b, p)
	}
	b = append(b, ',')
	return appendModelLivePassed(b, v.Passed)
}

// appendModelLiveRow appends the row of a prepared row with a score; version
// is the index of its model version.
func appendModelLiveRow(b []byte, r *ScoutWebRow, version int) []byte {
	b = append(b, '[')
	b = strconv.AppendInt(b, int64(r.CallID), 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(version), 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, webModelAfterS(r.Model, r.MessageDate), 10)
	for _, v := range r.Model.Buckets {
		b = appendModelLiveBucket(b, v)
	}
	return append(b, ']')
}

// webModelLiveRowBytes: the room reserved per scored row: a row with every
// bucket and full-precision numbers (about 16 bytes a value) fits, so append
// rarely grows the block again (BenchmarkWebSnapshot's rows, with short
// numbers, take about 91 bytes).
const webModelLiveRowBytes = 32 + len(ScoutWebModelBuckets)*webModelLivePerBucket*16

// webModelLiveRows encodes the rows array: one row per prepared row with a
// score, in call id order. It returns the number of rows too.
func webModelLiveRows(rows []ScoutWebRow, versions *webDict) ([]byte, int) {
	want := 0 // the rows to come: every scored row counts once in versions
	for _, c := range versions.count {
		want += c
	}
	out := make([]byte, 0, 2+want*webModelLiveRowBytes)
	out = append(out, '[')
	scored := 0
	for i := range rows {
		r := &rows[i]
		if r.Model == nil {
			continue
		}
		if scored > 0 {
			out = append(out, ',')
		}
		out = appendModelLiveRow(out, r, versions.at(r.Model.Version))
		scored++
	}
	return append(out, ']'), scored
}

// buildWebModelLive encodes the body of GET /api/model-live from the prepared
// rows of a new snapshot (newWebSnapshot). When the bytes come out the same as
// prev's, prev is returned (its compressed form is not made again).
func buildWebModelLive(rows []ScoutWebRow, prev *webModelLive) (*webModelLive, error) {
	versions := webModelVersions(rows)
	out, scored := webModelLiveRows(rows, versions)
	body := webModelLiveBody{
		Format: webModelLiveFormat, Buckets: ScoutWebModelBuckets[:], Horizons: ScoutWebModelHorizons[:],
		FirstCalls: len(rows), Scored: scored, Versions: versions.names, Columns: webModelLiveColumns, Rows: out,
	}
	plain, etag, err := encodeWebBody(&body, len(out)+1024, `-m`+strconv.Itoa(webModelLiveFormat))
	if err != nil {
		return nil, err
	}
	if prev != nil && prev.etag == etag && bytes.Equal(prev.plain, plain) {
		return prev, nil
	}
	gz, err := gzipIfSmaller(plain)
	if err != nil {
		return nil, err
	}
	return &webModelLive{plain: plain, gz: gz, etag: etag}, nil
}

// handleModelLive is GET /api/model-live: the model's scores at the time of
// each first call, as built with the snapshot (already encoded and
// compressed). Registered only when the website shows model scores.
func (s *webServer) handleModelLive(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) > 0 {
		writeJSONError(w, http.StatusBadRequest, "this endpoint takes no parameters")
		return
	}
	snap := s.snapshot(w)
	if snap == nil {
		return
	}
	m := snap.modelLive
	if m == nil {
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusServiceUnavailable, "starting: the data is not loaded yet")
		return
	}
	s.serveBuiltBody(w, r, snap, m.plain, m.gz, m.etag)
}
