// Package audit holds forgectl's read-only posture scans (forgectl#14). The
// first is the prompt-injection surface inventory: every agent-instruction
// carrier under the projects root, classified against the same target list
// quarantine hides, so the map and the defense cannot drift apart.
//
// Nothing here writes, and nothing reads a file's contents. The walk runs
// through an os.Root opened on the scan root, so no symlink, however it is
// planted or raced, resolves outside that root; symlinks are reported as what
// they are and never followed.
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
	// Truncated is set when a cap stopped the walk short.
	Truncated bool
}

// errStop unwinds the walk once a cap is hit.
var errStop = errors.New("audit: cap reached")

type scanner struct {
	fsys    fs.FS
	root    string
	matcher *quarantine.CarrierMatcher
	opts    Options
	report  *Report
}

// ScanInjection walks opts.Root and returns every agent-instruction carrier
// it finds. It errors only when the root itself cannot be resolved or
// opened; an unreadable subtree is counted, not fatal.
func ScanInjection(opts Options) (Report, error) {
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

	report := Report{Root: resolved, Findings: []Finding{}}
	s := &scanner{fsys: r.FS(), root: resolved, matcher: matcher, opts: opts, report: &report}
	if err := s.walk(".", nil, "", false, 0); err != nil && !errors.Is(err, errStop) {
		return Report{}, err
	}
	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].Path < report.Findings[j].Path })
	return report, nil
}

// walk lists dir (slash-separated, relative to the root) and classifies each
// entry. segs are dir's own segments; repo is the nearest git working tree
// at or above dir, relative to the root, with "" meaning none and "." the
// root itself.
func (s *scanner) walk(dir string, segs []string, repo string, vendored bool, depth int) error {
	entries, err := fs.ReadDir(s.fsys, dir)
	if err != nil {
		if dir == "." {
			return fmt.Errorf("read audit root %s: %w", termsafe.QuotePath(s.root), termsafe.Error(err))
		}
		s.report.Unreadable++
		return nil
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			repo = dir
			s.report.Repos++
			break
		}
	}

	for _, e := range entries {
		s.report.Entries++
		if s.report.Entries > s.opts.MaxEntries {
			s.report.Truncated = true
			return errStop
		}
		name := e.Name()
		if name == ".git" {
			continue
		}
		childSegs := append(append(make([]string, 0, len(segs)+1), segs...), name)
		if carrier, anchor, ok := s.classify(childSegs, repo); ok {
			if len(s.report.Findings) >= s.opts.MaxFindings {
				s.report.Truncated = true
				return errStop
			}
			s.report.Findings = append(s.report.Findings, s.finding(e, childSegs, carrier, anchor, repo, vendored))
			// A matched directory is one carrier: it is reported as a unit and
			// never descended, as quarantine hides it as a unit.
			continue
		}
		if !e.IsDir() { // a symlink's DirEntry is not IsDir, so it is never followed
			continue
		}
		if depth+1 >= s.opts.MaxDepth {
			s.report.Truncated = true
			continue
		}
		if err := s.walk(path.Join(dir, name), childSegs, repo, vendored || dependencyDirs[name], depth+1); err != nil {
			return err
		}
	}
	return nil
}

// classify tries each suffix of segs against the carrier list. The anchor is
// the directory the matched suffix is relative to. When more than one suffix
// matches (`.x/.mcp.json` is both the `.mcp.json` literal anchored at `.x` and
// the `.*/.mcp.json` pattern anchored one level up), the reading anchored at
// the enclosing repo root wins, because that is the one quarantine applies;
// otherwise a nestable reading wins, as it is expected anywhere; otherwise the
// shortest.
func (s *scanner) classify(segs []string, repo string) (quarantine.Carrier, string, bool) {
	var best quarantine.Carrier
	var bestAnchor string
	found := false
	for k := 1; k <= s.matcher.MaxSegments() && k <= len(segs); k++ {
		c, ok := s.matcher.Match(segs[len(segs)-k:])
		if !ok {
			continue
		}
		anchor := "."
		if n := len(segs) - k; n > 0 {
			anchor = path.Join(segs[:n]...)
		}
		if repo != "" && anchor == repo {
			return c, anchor, true
		}
		if !found || (c.Nestable && !best.Nestable) {
			best, bestAnchor, found = c, anchor, true
		}
	}
	return best, bestAnchor, found
}

func (s *scanner) finding(e fs.DirEntry, segs []string, c quarantine.Carrier, anchor, repo string, vendored bool) Finding {
	f := Finding{
		Path:      filepath.Join(s.root, filepath.FromSlash(path.Join(segs...))),
		Target:    c.Target,
		Type:      entryType(e),
		Anomalies: []string{},
	}
	if repo != "" {
		f.Repo = filepath.Join(s.root, filepath.FromSlash(repo))
	}
	if info, err := e.Info(); err == nil {
		f.ModTime = info.ModTime()
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
	return f
}

func entryType(e fs.DirEntry) string {
	switch t := e.Type(); {
	case t&fs.ModeSymlink != 0:
		return TypeSymlink
	case t.IsDir():
		return TypeDir
	case t.IsRegular():
		return TypeFile
	default:
		return TypeOther
	}
}
