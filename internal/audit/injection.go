// Package audit holds forgectl's read-only posture scans (forgectl#14). The
// first is the prompt-injection surface inventory: every agent-instruction
// carrier under the projects root, classified against the same target list
// quarantine hides, so the map and the defense cannot drift apart.
//
// Nothing here writes, and nothing reads a file's contents. Below the root
// itself (which is resolved once with filepath.EvalSymlinks and opened with
// os.OpenRoot), every filesystem call goes through that *os.Root: directory
// listings are root.Open + Readdirnames (names only, no per-entry stat), and
// every entry's type and mtime come from root.Lstat. No listing or stat
// therefore resolves outside the root, even under a racing symlink swap.
// Symlinks are reported as what they are and never followed.
// TestScanInjection_MetadataOnlyThroughRoot and
// TestAuditSource_NoUnconfinedFilesystemCalls pin both halves.
package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Default caps. A projects root is operator-owned but its contents are
// clone-derived, so the walk is bounded rather than trusted: past a cap the
// report says Truncated instead of running unbounded.
const (
	DefaultMaxEntries  = 1_000_000
	DefaultMaxFindings = 10_000
	DefaultMaxDepth    = 32
	// RecentWindow is how new a carrier's mtime must be to be flagged recent.
	RecentWindow = 7 * 24 * time.Hour
)

// Anomaly flags. The wire values are part of the --json contract.
const (
	// AnomalyVendored marks a carrier inside a dependency directory: an agent
	// reading that subtree takes instructions from a package author, the
	// supply-chain vector the issue names.
	AnomalyVendored = "vendored"
	// AnomalyOffRoot marks a root-only carrier (anything but a nestable
	// basename) anchored somewhere other than a git working-tree root, where
	// no tool is expected to keep one.
	AnomalyOffRoot = "off-root"
	// AnomalyRecent marks a carrier whose own mtime is inside RecentWindow.
	AnomalyRecent = "recent"
	// AnomalySymlink marks a symlinked directory whose path is a prefix of a
	// multi-segment carrier (`.gemini` for `.*/mcp.json`, `.github` for
	// `.github/instructions/`). The carrier, if any, lives behind the link,
	// which the scan never follows, so the link itself is what is reported.
	AnomalySymlink = "symlink"
)

// Cap names, as reported in Report.CappedBy and the --json capped_by array.
const (
	CapEntries  = "entries"
	CapFindings = "findings"
	CapDepth    = "depth"
)

// Entry types.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	TypeOther   = "other"
)

// dependencyDirs are directory basenames whose contents a package manager,
// not the repo's author, wrote. It is deliberately narrower than clean's
// reclaim list: build outputs (dist, build, target) are generated from the
// repo's own source, so a carrier there is not a third party's.
var dependencyDirs = map[string]bool{
	"node_modules":     true,
	"bower_components": true,
	"vendor":           true,
	"third_party":      true,
	".venv":            true,
	"venv":             true,
	"site-packages":    true,
}

// Worktrees under a repo's .claude/worktrees/ are not scanned: `.claude/` is
// one carrier, reported as a unit and never descended, exactly as quarantine
// hides it as a unit.

// Options configures ScanInjection. Zero caps take the defaults.
type Options struct {
	// Root is the directory to scan. It is made absolute and symlink-resolved
	// before the walk, so every reported path shares that one prefix.
	Root        string
	Now         time.Time
	MaxEntries  int
	MaxFindings int
	MaxDepth    int
	// Targets overrides the carrier list; nil means quarantine.DefaultTargets.
	// Tests use it; the CLI never sets it.
	Targets []string
}

// Finding is one carrier. Paths are absolute under the resolved root.
type Finding struct {
	Path      string
	Repo      string // nearest enclosing git working tree, "" when none
	Target    string // the quarantine.DefaultTargets entry it matched
	Type      string
	ModTime   time.Time
	Anomalies []string
}

// Report is ScanInjection's result.
type Report struct {
	Root     string
	Findings []Finding
	// Repos counts git working trees the walk entered.
	Repos int
	// Entries counts directory entries examined.
	Entries int
	// Unreadable counts directories the walk could not list.
	Unreadable int
	// Truncated is set when any cap was hit, so the list may be incomplete.
	Truncated bool
	// CappedBy names each cap that was hit, in the order first hit. The
	// entries and findings caps stop the whole scan; the depth cap only skips
	// the subtrees below it and the scan carries on.
	CappedBy []string
	// DepthSkipped counts directories left unwalked at the depth cap.
	DepthSkipped int
}

// Stopped reports whether a cap ended the scan early, as opposed to the
// depth cap, which only skips subtrees.
func (r Report) Stopped() bool {
	for _, c := range r.CappedBy {
		if c == CapEntries || c == CapFindings {
			return true
		}
	}
	return false
}

func (r *Report) capped(name string) {
	r.Truncated = true
	for _, c := range r.CappedBy {
		if c == name {
			return
		}
	}
	r.CappedBy = append(r.CappedBy, name)
}

// errStop unwinds the walk once a cap is hit.
var errStop = errors.New("audit: cap reached")

// fsOps is the scanner's whole filesystem surface. ScanInjection binds both
// to the *os.Root; a test binds a failing or counting double to prove no
// metadata arrives any other way.
type fsOps struct {
	names func(dir string) ([]string, error)
	lstat func(name string) (fs.FileInfo, error)
}

func rootOps(r *os.Root) fsOps {
	return fsOps{
		names: func(dir string) ([]string, error) {
			f, err := r.Open(dir)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return f.Readdirnames(-1)
		},
		lstat: r.Lstat,
	}
}

type scanner struct {
	ops     fsOps
	root    string
	matcher *quarantine.CarrierMatcher
	opts    Options
	report  *Report
}

// ScanInjection walks opts.Root and returns every agent-instruction carrier
// it finds. It errors only when the root itself cannot be resolved or
// opened; an unreadable subtree is counted, not fatal.
func ScanInjection(opts Options) (Report, error) {
	abs, err := filepath.Abs(opts.Root)
	if err != nil {
		return Report{}, fmt.Errorf("resolve audit root %s: %w", termsafe.QuotePath(opts.Root), termsafe.Error(err))
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Report{}, fmt.Errorf("resolve audit root %s: %w", termsafe.QuotePath(abs), termsafe.Error(err))
	}
	r, err := os.OpenRoot(resolved)
	if err != nil {
		return Report{}, fmt.Errorf("open audit root %s: %w", termsafe.QuotePath(resolved), termsafe.Error(err))
	}
	defer func() { _ = r.Close() }()
	return scanWith(resolved, rootOps(r), opts)
}

// scanWith is ScanInjection's walk over an already-resolved root and an
// explicit filesystem surface.
func scanWith(resolved string, ops fsOps, opts Options) (Report, error) {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = DefaultMaxFindings
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	targets := opts.Targets
	if targets == nil {
		targets = quarantine.DefaultTargets
	}
	matcher, err := quarantine.NewCarrierMatcher(targets)
	if err != nil {
		return Report{}, err
	}

	report := Report{Root: resolved, Findings: []Finding{}, CappedBy: []string{}}
	s := &scanner{ops: ops, root: resolved, matcher: matcher, opts: opts, report: &report}
	if err := s.walk(".", nil, "", false, 0); err != nil && !errors.Is(err, errStop) {
		return Report{}, err
	}
	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].Path < report.Findings[j].Path })
	return report, nil
}

// walk lists dir (slash-separated, relative to the root) and classifies each
// entry. segs are dir's own segments; repo is the nearest git working tree
// at or above dir, relative to the root, with "" meaning none and "." the
// root itself. depth is dir's own depth, the root being 0: with MaxDepth N
// the walk lists directories at depths 0 through N-1.
func (s *scanner) walk(dir string, segs []string, repo string, vendored bool, depth int) error {
	names, err := s.ops.names(dir)
	if err != nil {
		if dir == "." {
			return fmt.Errorf("read audit root %s: %w", termsafe.QuotePath(s.root), termsafe.Error(err))
		}
		s.report.Unreadable++
		return nil
	}
	sort.Strings(names)
	for _, name := range names {
		if name == ".git" {
			repo = dir
			s.report.Repos++
			break
		}
	}

	for _, name := range names {
		s.report.Entries++
		if s.report.Entries > s.opts.MaxEntries {
			s.report.capped(CapEntries)
			return errStop
		}
		if name == ".git" {
			continue
		}
		rel := path.Join(dir, name)
		info, err := s.ops.lstat(rel)
		if err != nil {
			continue // vanished or unreadable mid-walk: nothing to classify
		}
		mode := info.Mode()
		childSegs := append(append(make([]string, 0, len(segs)+1), segs...), name)
		carrier, anchor, ok := s.classify(childSegs, repo)
		prefix := false
		if !ok && mode&fs.ModeSymlink != 0 {
			carrier, anchor, ok = s.classifyPrefix(childSegs, repo)
			prefix = ok
		}
		if ok {
			if len(s.report.Findings) >= s.opts.MaxFindings {
				s.report.capped(CapFindings)
				return errStop
			}
			s.report.Findings = append(s.report.Findings, s.finding(info, childSegs, carrier, anchor, repo, vendored, prefix))
			// A matched directory is one carrier: it is reported as a unit and
			// never descended, as quarantine hides it as a unit.
			continue
		}
		if !mode.IsDir() { // Lstat: a symlink is never IsDir, so it is never followed
			continue
		}
		if depth+1 >= s.opts.MaxDepth {
			s.report.capped(CapDepth)
			s.report.DepthSkipped++
			continue
		}
		if err := s.walk(rel, childSegs, repo, vendored || dependencyDirs[name], depth+1); err != nil {
			return err
		}
	}
	return nil
}

// anchorOf is the directory a k-segment suffix of segs is relative to.
func anchorOf(segs []string, k int) string {
	if n := len(segs) - k; n > 0 {
		return path.Join(segs[:n]...)
	}
	return "."
}

// classify tries each suffix of segs against the carrier list. The anchor is
// the directory the matched suffix is relative to. When more than one suffix
// matches (`.x/.mcp.json` is both the `.mcp.json` literal anchored at `.x` and
// the `.*/.mcp.json` pattern anchored one level up), the reading anchored at
// the enclosing repo root wins, because that is the one quarantine applies;
// otherwise the shortest suffix wins. (No nestable basename is also a suffix
// of a multi-segment entry in today's list, so no nestable-first rule is
// needed; the drift pin would expose a list change that made one matter.)
func (s *scanner) classify(segs []string, repo string) (quarantine.Carrier, string, bool) {
	return s.pick(segs, repo, s.matcher.Match)
}

// classifyPrefix is classify for a symlink that is not itself a carrier but
// whose path is a proper prefix of a multi-segment entry.
func (s *scanner) classifyPrefix(segs []string, repo string) (quarantine.Carrier, string, bool) {
	return s.pick(segs, repo, s.matcher.MatchPrefix)
}

func (s *scanner) pick(segs []string, repo string, match func([]string) (quarantine.Carrier, bool)) (quarantine.Carrier, string, bool) {
	var best quarantine.Carrier
	var bestAnchor string
	found := false
	for k := 1; k <= s.matcher.MaxSegments() && k <= len(segs); k++ {
		c, ok := match(segs[len(segs)-k:])
		if !ok {
			continue
		}
		anchor := anchorOf(segs, k)
		if repo != "" && anchor == repo {
			return c, anchor, true
		}
		if !found {
			best, bestAnchor, found = c, anchor, true
		}
	}
	return best, bestAnchor, found
}

func (s *scanner) finding(info fs.FileInfo, segs []string, c quarantine.Carrier, anchor, repo string, vendored, behindSymlink bool) Finding {
	f := Finding{
		Path:      filepath.Join(s.root, filepath.FromSlash(path.Join(segs...))),
		Target:    c.Target,
		Type:      modeType(info.Mode()),
		ModTime:   info.ModTime(),
		Anomalies: []string{},
	}
	if repo != "" {
		f.Repo = filepath.Join(s.root, filepath.FromSlash(repo))
	}
	if vendored {
		f.Anomalies = append(f.Anomalies, AnomalyVendored)
	}
	if !c.Nestable && (repo == "" || anchor != repo) {
		f.Anomalies = append(f.Anomalies, AnomalyOffRoot)
	}
	if !f.ModTime.IsZero() && s.opts.Now.Sub(f.ModTime) < RecentWindow {
		f.Anomalies = append(f.Anomalies, AnomalyRecent)
	}
	if behindSymlink {
		f.Anomalies = append(f.Anomalies, AnomalySymlink)
	}
	return f
}

func modeType(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return TypeSymlink
	case m.IsDir():
		return TypeDir
	case m.IsRegular():
		return TypeFile
	default:
		return TypeOther
	}
}
