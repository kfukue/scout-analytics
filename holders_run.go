package main

// Run set-up for the holder export: the call-id file, the output-folder guard
// (outside every checkout, or in a checkout's git-ignored data folder) and
// the node-cost estimate. Pure, except findRepoRoots (it looks for .git),
// resolveExisting (it resolves symbolic links) and checkHolderOutDir and its
// helpers (they use both).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// parseHolderIDFile reads call ids: one per line; "#" starts a comment; a CSV
// line gives its first field (quotes and spaces trimmed); blank lines and a
// "call_id" header are skipped; repeats are dropped, keeping the first. Any
// other field that is not a positive integer is an error naming its line. A
// UTF-8 byte order mark at the start of the file (Excel, Notepad) is dropped.
func parseHolderIDFile(r io.Reader) ([]int64, error) {
	var ids []int64
	seen := map[int64]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		text := sc.Text()
		if n == 1 {
			text = strings.TrimPrefix(text, "\xef\xbb\xbf") // the UTF-8 BOM
		}
		line, _, _ := strings.Cut(text, "#")
		field, _, _ := strings.Cut(line, ",")
		field = strings.Trim(strings.TrimSpace(field), `"'`)
		if field == "" || strings.EqualFold(field, "call_id") {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("call id file line %d: %q is not a call id", n, field)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read call id file: %w", err)
	}
	return ids, nil
}

// findRepoRoots returns every folder from dir upwards that holds a .git entry
// (a folder in a checkout, a file in a worktree): a worktree inside the main
// checkout gives both.
func findRepoRoots(dir string) ([]string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("find repo root from %s: %w", dir, err)
	}
	var roots []string
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			roots = append(roots, d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return roots, nil
		}
		d = parent
	}
}

// pathInside reports whether path is base or inside it. A path on another
// volume (filepath.Rel fails) is outside. On Windows filepath.Rel compares
// without case.
func pathInside(path, base string) (bool, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	b, err := filepath.Abs(base)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(b, p)
	if err != nil {
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

const (
	// holderDataDir is the folder at a repo root that the holder export may
	// write into (owner, 20 Oct 2026). .gitignore ignores it as /data/, so
	// nothing written there is committed.
	holderDataDir = "data"
	// holderDefaultOutDir is the export's default output folder relative to
	// the repo root, slash-separated (filepath.FromSlash it before use).
	holderDefaultOutDir = holderDataDir + "/holders"
)

var errHolderOutDir = errors.New("holder output folder not allowed")

// checkHolderOutDir refuses an output folder whose files could be committed
// or could mix with the tracker's own files. Inside a checkout (one of
// repoRoots, or one found from the folder upwards by findRepoRoots, so a
// checkout the caller did not name counts too), only the checkout's data
// folder (holderDataDir) or a folder inside it is allowed; when checkouts
// nest (a worktree inside the main checkout), the innermost one containing
// the folder decides. A folder outside every checkout is allowed. Inside
// stateDir (the tracker's data folder; "" = none) is always refused. Not
// pure: it stats the filesystem. The path is cleaned (so ".." is applied)
// and the symbolic links in its existing part are resolved
// (resolveExisting) before it is judged: a link into a checkout or the state
// folder is refused like the folder itself, and a data folder that is a link
// is judged by where it points.
func checkHolderOutDir(out string, repoRoots []string, stateDir string) error {
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("%w: empty", errHolderOutDir)
	}
	resolved, err := resolveExisting(out)
	if err != nil {
		return fmt.Errorf("check output folder %s: %w", out, err)
	}
	roots, err := holderRepoRoots(resolved, repoRoots)
	if err != nil {
		return fmt.Errorf("check output folder %s: %w", out, err)
	}
	if err := checkHolderOutInRepo(out, resolved, roots); err != nil {
		return err
	}
	return checkHolderOutStateDir(out, resolved, stateDir)
}

// holderRepoRoots returns the checkouts found from resolved upwards
// (findRepoRoots) followed by the given ones, each with its symbolic links
// resolved.
func holderRepoRoots(resolved string, given []string) ([]string, error) {
	found, err := findRepoRoots(resolved)
	if err != nil {
		return nil, err
	}
	roots := slices.Clone(found)
	for _, g := range given {
		r, err := resolveExisting(g)
		if err != nil {
			return nil, fmt.Errorf("resolve repo root %s: %w", g, err)
		}
		roots = append(roots, r)
	}
	return roots, nil
}

// innermostRoot returns the longest of roots that holds path (pathInside),
// and false when none does.
func innermostRoot(path string, roots []string) (string, bool, error) {
	best, ok := "", false
	for _, r := range roots {
		in, err := pathInside(path, r)
		if err != nil {
			return "", false, err
		}
		if in && len(r) > len(best) {
			best, ok = r, true
		}
	}
	return best, ok, nil
}

// checkHolderOutInRepo refuses resolved (the output folder out, resolved)
// when it is inside a checkout but not in that checkout's data folder. The
// data folder is joined to the root as written, never resolved itself: a
// data link pointing into scoutanalytics_data must not move the allowed
// folder there.
func checkHolderOutInRepo(out, resolved string, roots []string) error {
	root, ok, err := innermostRoot(resolved, roots)
	if err != nil {
		return fmt.Errorf("check output folder %s: %w", out, err)
	}
	if !ok {
		return nil
	}
	data := filepath.Join(root, holderDataDir)
	in, err := pathInside(resolved, data)
	if err != nil {
		return fmt.Errorf("check output folder %s: %w", out, err)
	}
	if !in {
		return fmt.Errorf("%w: %s is inside the checkout %s but not in its %s folder", errHolderOutDir, out, root, holderDataDir)
	}
	return nil
}

// checkHolderOutStateDir refuses resolved (the output folder out, resolved)
// when it is stateDir or inside it; "" = no state folder.
func checkHolderOutStateDir(out, resolved, stateDir string) error {
	if stateDir == "" {
		return nil
	}
	resolvedState, err := resolveExisting(stateDir)
	if err != nil {
		return fmt.Errorf("check output folder %s against %s: %w", out, stateDir, err)
	}
	in, err := pathInside(resolved, resolvedState)
	if err != nil {
		return fmt.Errorf("check output folder %s: %w", out, err)
	}
	if in {
		return fmt.Errorf("%w: %s is inside the state folder %s", errHolderOutDir, out, stateDir)
	}
	return nil
}

// resolveExisting returns p as an absolute path with the symbolic links of
// its longest existing part resolved (filepath.EvalSymlinks) and the rest,
// which does not exist yet, joined back unchanged. An error other than "does
// not exist" is returned. Limit: since Go 1.23 a Windows junction (mount
// point) is not a symbolic link to EvalSymlinks, so it may stay unresolved.
func resolveExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var tail []string
	for d := abs; ; {
		r, err := filepath.EvalSymlinks(d)
		if err == nil {
			return filepath.Join(append([]string{r}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", d, err)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return abs, nil
		}
		tail = append([]string{filepath.Base(d)}, tail...)
		d = parent
	}
}

// holderBlocksPerSecond: Robinhood Chain's block rate, about 10 blocks/s (see
// onchain.go), for estimates only.
const holderBlocksPerSecond = 10

// holderLogRequests is the reader's eth_getLogs count for one call (see the
// top of holders_fetch.go): 1 + (1 + F) × (⌊postEnd/S⌋ − ⌊launch/S⌋ + 1),
// with S the chunk size (> 0) and F the number of liquidity filters.
func holderLogRequests(launch, postEnd, chunk uint64, filters int) int64 {
	chunks := int64(postEnd/chunk-launch/chunk) + 1
	return 1 + int64(1+filters)*chunks
}

// holderLiquidityFilterCount is F for a call: the liquidity filters the
// reader sends for its venues (liquidityFilters).
func holderLiquidityFilterCount(venues []holderVenue, ids []string) int {
	n := 0
	for _, v := range venues {
		n += len(liquidityFilters(v, ids))
	}
	return n
}

// holderCostCall: what the estimate knows of a call before reading it.
type holderCostCall struct {
	EntryBlock uint64
	Age        time.Duration // the token's age at the call
	Filters    int           // holderLiquidityFilterCount
}

// holderCallCost estimates eth_getLogs for one call: launch = entry − age ×
// holderBlocksPerSecond (at least block 1), postEnd = entry + holderPostWindow
// × holderBlocksPerSecond. A token older than holderLookbackCap costs the
// launch search only (1).
func holderCallCost(c holderCostCall, chunk uint64) int64 {
	if c.Age > holderLookbackCap {
		return 1
	}
	back := uint64(max(c.Age, 0).Seconds() * holderBlocksPerSecond)
	launch := uint64(1)
	if c.EntryBlock > back+1 {
		launch = c.EntryBlock - back
	}
	postEnd := c.EntryBlock + uint64(holderPostWindow.Seconds()*holderBlocksPerSecond)
	return holderLogRequests(launch, postEnd, chunk, c.Filters)
}

// holderCostEstimate returns the estimate per call and the total. chunk is
// the log chunk size (SCOUT_RPC_LOG_CHUNK); 0 is an error.
func holderCostEstimate(calls []holderCostCall, chunk uint64) (perCall []int64, total int64, err error) {
	if chunk == 0 {
		return nil, 0, errors.New("holder cost estimate: chunk size is 0")
	}
	perCall = make([]int64, len(calls))
	for i, c := range calls {
		perCall[i] = holderCallCost(c, chunk)
		total += perCall[i]
	}
	return perCall, total, nil
}
