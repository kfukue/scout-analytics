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

// rawPath joins elems with the separator without cleaning, so ".." stays in
// the path the guard is given.
func rawPath(elems ...string) string {
	return strings.Join(elems, string(filepath.Separator))
}

// The guard refuses a folder in a checkout (main or worktree) outside its
// data folder, or in the state folder, and accepts one outside them or in a
// checkout's data folder.
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
		{"the main checkout itself", repo, true},
		{"the main checkout's data folder", filepath.Join(repo, "data"), false},
		{"inside the main checkout's data folder", filepath.Join(repo, "data", "holders", "run1"), false},
		{"the default output folder", filepath.Join(repo, filepath.FromSlash(holderDefaultOutDir)), false},
		{"relative path into the data folder", filepath.Join("repo", "data", "holders"), false},
		{"the worktree's data folder", filepath.Join(wt, "data", "holders"), false},
		{"the main checkout's scoutanalytics_data", filepath.Join(repo, "scoutanalytics_data"), true},
		{"a name starting with data: data-old", filepath.Join(repo, "data-old"), true},
		{"a name starting with data: datax", filepath.Join(repo, "datax", "holders"), true},
		{"a data folder below the root", filepath.Join(repo, "sub", "data"), true},
		{"a data folder in .claude, outside the worktree", filepath.Join(repo, ".claude", "data"), true},
		{"the main checkout's data, seen from the worktree's ..", rawPath(wt, "..", "..", "..", "data", "holders"), false},
		{".. from data into scoutanalytics_data", rawPath(repo, "data", "..", "scoutanalytics_data"), true},
		{".. from data to the root", rawPath(repo, "data", "holders", "..", ".."), true},
		{".. from data into another folder of the checkout", rawPath(repo, "data", "..", "sub", "data"), true},
		{".. from data out of the checkout", rawPath(repo, "data", "..", "..", "holders-pilot"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkHolderOutDir(tc.out, roots, state)
			if tc.refused != (err != nil) {
				t.Errorf("checkHolderOutDir(%s): %v, refused want %v", tc.out, err, tc.refused)
			}
			if err != nil && !errors.Is(err, errHolderOutDir) {
				t.Errorf("error %v does not wrap errHolderOutDir", err)
			}
		})
	}
	if _, err := checkHolderOutDir(filepath.Join(state, "x"), roots, ""); err != nil {
		t.Errorf("no state folder given: %v, want accepted", err)
	}
}

// The state folder is refused even when it sits in a checkout's data folder,
// and a checkout nested in the data folder decides for itself: only its own
// data folder is allowed.
func TestHoldersOutDirGuardInsideData(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	state := filepath.Join(repo, "data", "state")
	nested := filepath.Join(repo, "data", "nested")
	for _, d := range []string{filepath.Join(repo, ".git"), state, filepath.Join(nested, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, out string
		refused   bool
	}{
		{"the state folder in data", filepath.Join(state, "holders"), true},
		{"beside the state folder in data", filepath.Join(repo, "data", "holders"), false},
		{"a checkout nested in data, outside its data folder", filepath.Join(nested, "out"), true},
		{"the nested checkout's data folder", filepath.Join(nested, "data", "holders"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkHolderOutDir(tc.out, []string{repo}, state)
			if tc.refused != (err != nil) {
				t.Errorf("checkHolderOutDir(%s, state %s): %v, refused want %v", tc.out, state, err, tc.refused)
			}
			if err != nil && !errors.Is(err, errHolderOutDir) {
				t.Errorf("error %v does not wrap errHolderOutDir", err)
			}
		})
	}
}

// A data folder that is a symbolic link is judged by where it points: outside
// every checkout and the state folder it is accepted; into the checkout's
// scoutanalytics_data or another folder of it, into another checkout, or
// into the state folder it is refused. A link inside a real data folder that
// points into scoutanalytics_data is refused too. Skipped where links cannot
// be made (Windows without Developer Mode or admin rights).
func TestHoldersOutDirGuardDataSymlink(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	outside := filepath.Join(base, "big-disk", "holders-data")
	other := filepath.Join(base, "other")
	for _, d := range []string{state, outside, filepath.Join(other, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(base, "probe")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	// newRepo makes a checkout whose data folder links to target(repo).
	newRepo := func(t *testing.T, name string, target func(repo string) string) string {
		t.Helper()
		repo := filepath.Join(base, name)
		for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "scoutanalytics_data"), filepath.Join(repo, "sub")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target(repo), filepath.Join(repo, holderDataDir)); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		return repo
	}
	cases := []struct {
		name    string
		target  func(repo string) string
		refused bool
	}{
		{"outside every checkout", func(string) string { return outside }, false},
		{"into scoutanalytics_data", func(r string) string { return filepath.Join(r, "scoutanalytics_data") }, true},
		{"into another folder of the checkout", func(r string) string { return filepath.Join(r, "sub") }, true},
		{"into another checkout", func(string) string { return other }, true},
		{"into the state folder", func(string) string { return state }, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t, fmt.Sprintf("repo%d", i), tc.target)
			for _, out := range []string{filepath.Join(repo, holderDataDir), filepath.Join(repo, filepath.FromSlash(holderDefaultOutDir))} {
				_, err := checkHolderOutDir(out, []string{repo}, state)
				if tc.refused != (err != nil) {
					t.Errorf("checkHolderOutDir(%s), data -> %s: %v, refused want %v", out, tc.target(repo), err, tc.refused)
				}
				if err != nil && !errors.Is(err, errHolderOutDir) {
					t.Errorf("error %v does not wrap errHolderOutDir", err)
				}
			}
		})
	}
	t.Run("a link inside a real data folder into scoutanalytics_data", func(t *testing.T) {
		repo := filepath.Join(base, "real")
		sad := filepath.Join(repo, "scoutanalytics_data")
		for _, d := range []string{filepath.Join(repo, ".git"), sad, filepath.Join(repo, holderDataDir)} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(repo, holderDataDir, "holders")
		if err := os.Symlink(sad, link); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		if _, err := checkHolderOutDir(filepath.Join(link, "run1"), []string{repo}, ""); !errors.Is(err, errHolderOutDir) {
			t.Errorf("checkHolderOutDir(%s), holders -> %s: %v, want refused (errHolderOutDir)", link, sad, err)
		}
	})
}

// The data folder is the root's data folder as written, never where a link
// points: given the resolved path of a folder under <repo>/data (what
// resolveExisting returns when data is a link), the guard judges where it
// points. Runs without making links, so it covers the rule on machines where
// TestHoldersOutDirGuardDataSymlink skips.
func TestHoldersOutInRepoJudgesResolvedPath(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	out := filepath.Join(repo, filepath.FromSlash(holderDefaultOutDir))
	cases := []struct {
		name, resolved string
		refused        bool
	}{
		{"a real data folder", out, false},
		{"data points outside the checkout", filepath.Join(base, "big-disk", "holders"), false},
		{"data points into scoutanalytics_data", filepath.Join(repo, "scoutanalytics_data", "holders"), true},
		{"data points to the root", repo, true},
		{"data points to data-old", filepath.Join(repo, "data-old", "holders"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHolderOutInRepo(out, tc.resolved, []string{repo})
			if tc.refused != (err != nil) {
				t.Errorf("checkHolderOutInRepo(%s, resolved %s): %v, refused want %v", out, tc.resolved, err, tc.refused)
			}
			if err != nil && !errors.Is(err, errHolderOutDir) {
				t.Errorf("error %v does not wrap errHolderOutDir", err)
			}
		})
	}
}

// innermostRoot picks the deepest checkout holding the path.
func TestHoldersInnermostRoot(t *testing.T) {
	mainRepo := filepath.Join(t.TempDir(), "repo")
	wt := filepath.Join(mainRepo, ".claude", "worktrees", "wt")
	cases := []struct {
		name, path string
		want       string
		ok         bool
	}{
		{"in the worktree", filepath.Join(wt, "data"), wt, true},
		{"in the main checkout", filepath.Join(mainRepo, "data"), mainRepo, true},
		{"outside both", filepath.Dir(mainRepo), "", false},
	}
	for _, tc := range cases {
		for _, roots := range [][]string{{mainRepo, wt}, {wt, mainRepo}} {
			got, ok, err := innermostRoot(tc.path, roots)
			if err != nil || ok != tc.ok || got != tc.want {
				t.Errorf("%s: innermostRoot(%s, %v) = %q, %v, %v; want %q, %v", tc.name, tc.path, roots, got, ok, err, tc.want, tc.ok)
			}
		}
	}
}

// The repo's .gitignore must ignore the data folder at the root, where the
// holder export writes (holderDataDir): the contract that keeps its output
// out of git.
func TestHoldersGitignoreIgnoresDataDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	roots, err := findRepoRoots(wd)
	if err != nil || len(roots) == 0 {
		t.Fatalf("findRepoRoots(%s) = %v, %v; want the checkout", wd, roots, err)
	}
	path := filepath.Join(roots[0], ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := []string{"/" + holderDataDir + "/", holderDataDir + "/"}
	for _, line := range strings.Split(string(data), "\n") {
		if slices.Contains(want, strings.TrimSpace(line)) {
			return
		}
	}
	t.Errorf("%s has no line equal to %q or %q; the holder export's output could be committed", path, want[0], want[1])
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
		_, err := checkHolderOutDir(out, []string{given}, "")
		if !errors.Is(err, errHolderOutDir) {
			t.Errorf("checkHolderOutDir(%s, roots [%s]): %v, want refused (errHolderOutDir)", out, given, err)
		}
	}
	if _, err := checkHolderOutDir(filepath.Join(base, "free"), []string{given}, ""); err != nil {
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
		if _, err := checkHolderOutDir(out, nil, state); !errors.Is(err, errHolderOutDir) {
			t.Errorf("checkHolderOutDir(%s): %v, want refused (errHolderOutDir)", out, err)
		}
	}
	if _, err := checkHolderOutDir(filepath.Join(base, "free"), nil, state); err != nil {
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

// The guard returns the path it judged: absolute, ".." applied, links
// resolved; "" when it refuses. The export writes there only.
func TestHoldersOutDirGuardReturnsJudgedPath(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := checkHolderOutDir(rawPath(repo, "x", "..", holderDataDir, "holders"), []string{repo}, "")
	want, werr := resolveExisting(filepath.Join(repo, holderDataDir, "holders"))
	if err != nil || werr != nil || got != want {
		t.Errorf("judged %q, %v; want %q (%v)", got, err, want, werr)
	}
	if got, err := checkHolderOutDir(filepath.Join(repo, "sub"), []string{repo}, ""); got != "" || !errors.Is(err, errHolderOutDir) {
		t.Errorf("refused folder: %q, %v; want \"\" and errHolderOutDir", got, err)
	}
}

// A data folder that links outside the checkout: the judged path is where
// the link points. Skipped where links cannot be made (Windows without
// Developer Mode or admin rights).
func TestHoldersOutDirGuardDataSymlinkJudgedPath(t *testing.T) {
	base := t.TempDir()
	repo, outside := filepath.Join(base, "repo"), filepath.Join(base, "big-disk")
	for _, d := range []string{filepath.Join(repo, ".git"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(repo, holderDataDir)); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	got, err := checkHolderOutDir(filepath.Join(repo, filepath.FromSlash(holderDefaultOutDir)), []string{repo}, "")
	target, terr := filepath.EvalSymlinks(outside)
	if err != nil || terr != nil || got != filepath.Join(target, "holders") {
		t.Errorf("judged %q, %v; want %q (%v)", got, err, filepath.Join(target, "holders"), terr)
	}
}
