package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHoldersIDFile(t *testing.T) {
	cases := []struct {
		name, in string
		want     []int64
		errLine  string // "" = no error
	}{
		{"one per line", "3\n1\n2\n", []int64{3, 1, 2}, ""},
		{"comments, blanks, CRLF and spaces", "# pilot\r\n  12  \r\n\r\n7 # dead7\r\n#13\r\n", []int64{12, 7}, ""},
		{"CSV with a header", "call_id,label\n5,dead7\n\"6\",held2x7\n", []int64{5, 6}, ""},
		{"upper-case header", "CALL_ID\n8\n", []int64{8}, ""},
		{"repeats keep the first place", "4\n2\n4\n9\n2\n", []int64{4, 2, 9}, ""},
		{"empty file", "", nil, ""},
		{"UTF-8 BOM before the header", "\xef\xbb\xbfcall_id\r\n5\r\n", []int64{5}, ""},
		{"UTF-8 BOM before an id", "\xef\xbb\xbf7\n8\n", []int64{7, 8}, ""},
		{"a BOM after the first line is not dropped", "1\n\xef\xbb\xbf2\n", nil, "line 2"},
		{"not a number", "1\nabc\n", nil, "line 2"},
		{"zero is not a call id", "0\n", nil, "line 1"},
		{"negative", "1\n2\n-3\n", nil, "line 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHolderIDFile(strings.NewReader(tc.in))
			if tc.errLine != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errLine) {
					t.Errorf("parse(%q): error %v, want one naming %s", tc.in, err, tc.errLine)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("parse(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
			}
		})
	}
}

// The guard refuses a folder in a checkout (main or worktree) or the state
// folder, and accepts one outside them.
func TestHoldersOutDirGuard(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base) // for the relative paths below
	repo := filepath.Join(base, "repo")
	wt := filepath.Join(repo, ".claude", "worktrees", "wt")
	state := filepath.Join(base, "state")
	for _, d := range []string{filepath.Join(repo, ".git"), wt, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A worktree's .git is a file.
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := findRepoRoots(filepath.Join(wt, "sub", "dir"))
	if err != nil {
		t.Fatalf("findRepoRoots: %v", err)
	}
	if want := []string{wt, repo}; !slices.Equal(roots, want) {
		t.Fatalf("roots %v, want %v", roots, want)
	}
	cases := []struct {
		name, out string
		refused   bool
	}{
		{"the worktree itself", wt, true},
		{"inside the worktree", filepath.Join(wt, "out"), true},
		{"inside the main checkout, outside the worktree", filepath.Join(repo, "scoutanalytics_data", "holders"), true},
		{"a path with .. inside the checkout", wt + string(filepath.Separator) + filepath.Join("a", "..", "b"), true},
		{"relative path into the checkout", filepath.Join("repo", ".claude", "worktrees", "wt", "out"), true},
		{"relative path with .. leaving the checkout", "repo" + string(filepath.Separator) + filepath.Join("..", "holders-pilot"), false},
		{"inside the state folder", filepath.Join(state, "holders"), true},
		{"the state folder itself", state, true},
		{"a sibling of the checkout", filepath.Join(base, "holders-pilot"), false},
		{"a folder whose name starts with the repo's", repo + "-out", false},
		{"a folder named ..x inside base", filepath.Join(base, "..x"), false},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHolderOutDir(tc.out, roots, state)
			if tc.refused != (err != nil) {
				t.Errorf("checkHolderOutDir(%s): %v, refused want %v", tc.out, err, tc.refused)
			}
			if err != nil && !errors.Is(err, errHolderOutDir) {
				t.Errorf("error %v does not wrap errHolderOutDir", err)
			}
		})
	}
	if err := checkHolderOutDir(filepath.Join(state, "x"), roots, ""); err != nil {
		t.Errorf("no state folder given: %v, want accepted", err)
	}
}

// A folder inside a checkout the given roots do not cover is refused too:
// the guard looks for .git from the output folder upwards itself.
func TestHoldersOutDirGuardFindsOtherCheckout(t *testing.T) {
	base := t.TempDir()
	given := filepath.Join(base, "given")
	other := filepath.Join(base, "other")
	for _, d := range []string{filepath.Join(given, ".git"), filepath.Join(other, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, out := range []string{other, filepath.Join(other, "out"), filepath.Join(other, "not", "made", "yet")} {
		err := checkHolderOutDir(out, []string{given}, "")
		if !errors.Is(err, errHolderOutDir) {
			t.Errorf("checkHolderOutDir(%s, roots [%s]): %v, want refused (errHolderOutDir)", out, given, err)
		}
	}
	if err := checkHolderOutDir(filepath.Join(base, "free"), []string{given}, ""); err != nil {
		t.Errorf("a folder outside both checkouts: %v, want accepted", err)
	}
}

// A symbolic link into the state folder or a checkout is refused like the
// folder itself. Skipped where links cannot be made (Windows without
// Developer Mode or admin rights).
func TestHoldersOutDirGuardSymlink(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	repoSub := filepath.Join(base, "repo", "sub")
	for _, d := range []string{state, filepath.Join(base, "repo", ".git"), repoSub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stateLink, repoLink := filepath.Join(base, "state-link"), filepath.Join(base, "repo-link")
	if err := os.Symlink(state, stateLink); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if err := os.Symlink(repoSub, repoLink); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	for _, out := range []string{filepath.Join(stateLink, "holders"), filepath.Join(repoLink, "out", "new")} {
		if err := checkHolderOutDir(out, nil, state); !errors.Is(err, errHolderOutDir) {
			t.Errorf("checkHolderOutDir(%s): %v, want refused (errHolderOutDir)", out, err)
		}
	}
	if err := checkHolderOutDir(filepath.Join(base, "free"), nil, state); err != nil {
		t.Errorf("a folder outside: %v, want accepted", err)
	}
}

// Windows paths compare without case.
func TestHoldersPathInsideCase(t *testing.T) {
	if filepath.Separator != '\\' {
		t.Skip("Windows paths only")
	}
	in, err := pathInside(`C:\Repo\Out`, `c:\repo`)
	if err != nil || !in {
		t.Errorf("pathInside(C:\\Repo\\Out, c:\\repo) = %v, %v; want true", in, err)
	}
	in, err = pathInside(`D:\out`, `C:\repo`)
	if err != nil || in {
		t.Errorf("another drive: %v, %v; want false", in, err)
	}
}

func TestHoldersLogRequestsFormula(t *testing.T) {
	cases := []struct {
		launch, postEnd, chunk uint64
		filters                int
		want                   int64
	}{
		{100_000, 300_000, 200_000, 1, 1 + 2*2},       // chunks 0 and 1
		{199_999, 200_000, 200_000, 0, 1 + 1*2},       // two blocks straddling a boundary: two chunks
		{200_000, 399_999, 200_000, 2, 1 + 3*1},       // aligned: one chunk
		{1, 1_000_000, 200_000, 1, 1 + 2*6},           // chunks 0..5
		{5_000_000, 5_864_000, 1_000_000, 3, 1 + 4*1}, // one chunk
		{5_900_000, 6_764_000, 1_000_000, 3, 1 + 4*2}, // two chunks
	}
	for _, tc := range cases {
		if got := holderLogRequests(tc.launch, tc.postEnd, tc.chunk, tc.filters); got != tc.want {
			t.Errorf("holderLogRequests(%d, %d, S=%d, F=%d) = %d, want %d", tc.launch, tc.postEnd, tc.chunk, tc.filters, got, tc.want)
		}
	}
}

func TestHoldersLiquidityFilterCount(t *testing.T) {
	var ids []string
	for i := range 33 {
		ids = append(ids, fmt.Sprintf("0x%064x", i+1))
	}
	cases := []struct {
		name   string
		venues []holderVenue
		ids    []string
		want   int
	}{
		{"v2", []holderVenue{{hvPool, "v2"}}, nil, 1},
		{"v3 and a curve", []holderVenue{{hvPool, "v3"}, {hvCurve, "pons"}}, nil, 1},
		{"v4 without ids", []holderVenue{{hvPM, "v4"}}, nil, 0},
		{"v4 with 33 ids (two batches)", []holderVenue{{hvPM, "v4"}}, ids, 2},
		{"Pons graduated: curve and v4 with one id", []holderVenue{{hvPM, "v4"}, {hvCurve, "pons"}}, ids[:1], 1},
	}
	for _, tc := range cases {
		if got := holderLiquidityFilterCount(tc.venues, tc.ids); got != tc.want {
			t.Errorf("%s: %d filters, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHoldersCostEstimate(t *testing.T) {
	const chunk = 200_000
	calls := []holderCostCall{
		// 1 h old: launch 964,000, window end 1,864,000: chunks 4..9.
		{EntryBlock: 1_000_000, Age: time.Hour, Filters: 1},
		// Older than the entry block allows: launch is block 1.
		{EntryBlock: 50_000, Age: 30 * 24 * time.Hour, Filters: 0},
		// Past the look-back cap: the launch search only.
		{EntryBlock: 900_000_000, Age: holderLookbackCap + time.Second, Filters: 2},
	}
	want := []int64{1 + 2*6, 1 + 1*(914_000/chunk-1/chunk+1), 1}
	per, total, err := holderCostEstimate(calls, chunk)
	if err != nil {
		t.Fatalf("holderCostEstimate: %v", err)
	}
	if !slices.Equal(per, want) || total != want[0]+want[1]+want[2] {
		t.Errorf("per call %v total %d, want %v total %d", per, total, want, want[0]+want[1]+want[2])
	}
	if _, _, err := holderCostEstimate(calls, 0); err == nil {
		t.Errorf("chunk 0: no error")
	}
}

// The estimate matches what fetchHolderLogs really asks on the fake chain
// (10 blocks/s, like holderBlocksPerSecond), venues and filters included.
func TestHoldersCostEstimateMatchesReader(t *testing.T) {
	const chunk = 1_000_000
	cases := []struct {
		name      string
		launchAge time.Duration
		venues    []holderVenue
		ids       []string
	}{
		{"1h old, v2", time.Hour, []holderVenue{{lqPool, "v2"}}, nil},
		{"5d old, v3 and a Pons curve", 5 * 24 * time.Hour, []holderVenue{{lqPool, "v3"}, {hfCurve, "pons"}}, nil},
		{"20d old, v4 with two ids", 20 * 24 * time.Hour, []holderVenue{{lqPM, "v4"}}, []string{lqPoolID, lqOtherPoolID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHFCall(t, 40*24*time.Hour, 10*24*time.Hour, tc.launchAge, chunk)
			r := c.mustFetch(t, c.input(tc.venues, tc.ids...))
			if r.Status != holderFetchOK {
				t.Fatalf("status %v, want ok", r.Status)
			}
			f := holderLiquidityFilterCount(tc.venues, tc.ids)
			if got := holderLogRequests(r.LaunchBlock, r.PostEndBlock, chunk, f); got != r.LogRequests {
				t.Errorf("formula %d, reader asked %d", got, r.LogRequests)
			}
			est, _, err := holderCostEstimate([]holderCostCall{{EntryBlock: c.entryBlock, Age: tc.launchAge, Filters: f}}, chunk)
			if err != nil {
				t.Fatal(err)
			}
			if est[0] != r.LogRequests {
				t.Errorf("estimate %d, reader asked %d", est[0], r.LogRequests)
			}
		})
	}
}
