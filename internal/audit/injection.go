// Package audit holds forgectl's read-only posture scans (forgectl#14). The
// first is the prompt-injection surface inventory: every agent-instruction
// carrier under the projects root, classified against the same target list
// quarantine hides, so the map and the defense cannot drift apart.
//
// Nothing here writes, and nothing reads a file's contents. The root is
// opened once with os.OpenRoot, and every filesystem call below it goes
// through that *os.Root (rootops.go): directory listings are an
// O_DIRECTORY|O_NONBLOCK open plus Readdirnames (names only, no per-entry
// stat), every entry's type and mtime come from root.Lstat, and the one
// symlink-following check (is a symlinked carrier prefix a directory?) is
// root.Stat, which refuses a target outside the root. Symlinks are reported
// as what they are and never walked. TestScanInjection_MetadataOnlyThroughRoot
// and TestAuditSource_NoUnconfinedFilesystemCalls pin both halves.
//
// Paths are reported in the caller's spelling of the root, made absolute but
// never symlink-resolved, so Report.Root, every Finding.Path and Repo, and any
// filepath.Rel a consumer takes against Root agree even when the root sits
// under a symlinked parent (on macOS, anything under /var or /tmp). The
// root is opened once, so a root symlink repointed mid-scan does not move the
// walk, which finishes in the directory it opened; the reported paths then
// carry a spelling that names the new target.
package audit

import (
	"errors"
	"fmt"
	"io/fs"
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

// Options configures ScanInjection. Zero caps take the defaults.
type Options struct {
	// Root is the directory to scan. It is made absolute (not
	// symlink-resolved), and every reported path shares that one prefix.
	Root        string
	Now         time.Time
	MaxEntries  int
	MaxFindings int
	MaxDepth    int
	// Targets overrides the carrier list; nil means quarantine.DefaultTargets.
	// Tests use it; the CLI never sets it.
	Targets []string
}

// Finding is one carrier. Paths are absolute, under Report.Root.
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
	// The caps this scan ran with, after defaults.
	MaxEntries, MaxFindings, MaxDepth int
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

// errStop unwinds the walk once a cap is hit.
var errStop = errors.New("audit: cap reached")

type scanner struct {
	matcher *quarantine.CarrierMatcher
	root    string
	opts    Options
	ops     fsOps
	report  *Report
	stats   *walkStats
}

// ScanInjection walks opts.Root and returns every agent-instruction carrier
// it finds. It errors only when the root itself cannot be resolved or
// opened; an unreadable subtree is counted, not fatal.
func ScanInjection(opts Options) (Report, error) {
	abs, err := filepath.Abs(opts.Root)
	if err != nil {
		return Report{}, fmt.Errorf("resolve audit root %s: %w", termsafe.QuotePath(opts.Root), termsafe.Error(err))
	}
	ops, closeRoot, err := openRootOps(abs)
	if err != nil {
		return Report{}, err
	}
	defer closeRoot()
	return scanWith(abs, ops, opts)
}

// withDefaults fills opts' zero caps.
func (opts Options) withDefaults() Options {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = DefaultMaxFindings
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	return opts
}

// scanWith is ScanInjection's walk over an opened root, reported under the
// display prefix root, through an explicit filesystem surface.
func scanWith(root string, ops fsOps, opts Options) (Report, error) {
	opts = opts.withDefaults()
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

	report := Report{
		Root: root, Findings: []Finding{}, CappedBy: []string{},
		MaxEntries: opts.MaxEntries, MaxFindings: opts.MaxFindings, MaxDepth: opts.MaxDepth,
	}
	stats := &walkStats{CappedBy: []string{}}
	s := &scanner{matcher: matcher, root: root, opts: opts, ops: ops, report: &report, stats: stats}
	w := &walker{ops: ops, root: root, maxEntries: opts.MaxEntries, maxDepth: opts.MaxDepth, stats: stats, visit: s.visit}
	if err := w.run(); err != nil {
		return Report{}, err
	}
	report.Repos = len(stats.Repos)
	report.Entries = stats.Entries
	report.Unreadable = stats.Unreadable
	report.CappedBy = stats.CappedBy
	report.Truncated = len(stats.CappedBy) > 0
	report.DepthSkipped = stats.DepthSkipped
	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].Path < report.Findings[j].Path })
	return report, nil
}

// visit classifies one entry. A matched directory is one carrier: it is
// reported as a unit and never descended, as quarantine hides it as a unit.
// That includes a repo's .claude/worktrees/: worktrees parked there are part
// of the .claude carrier and are not scanned separately.
func (s *scanner) visit(e entry) (bool, error) {
	mode := e.info.Mode()
	carrier, anchor, ok := s.classify(e.segs, e.repo)
	prefix := false
	if !ok && mode&fs.ModeSymlink != 0 {
		carrier, anchor, ok = s.classifyPrefix(e.rel, e.segs, e.repo)
		prefix = ok
	}
	if !ok {
		return false, nil
	}
	if len(s.report.Findings) >= s.opts.MaxFindings {
		s.stats.capped(CapFindings)
		return true, errStop
	}
	s.report.Findings = append(s.report.Findings, s.finding(e.info, e.segs, carrier, anchor, e.repo, e.vendored, prefix))
	return true, nil
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
// whose path is a proper prefix of a multi-segment entry. It reports the link
// only where quarantine would look behind it, and only when it can be a
// directory:
//   - the prefix must be anchored at the enclosing repo root, since every
//     multi-segment entry is root-only (`pkg/.hidden` is not a position).
//     So a symlinked `.gemini` below a repo root, or outside any repo, is
//     neither reported nor walked, while a real `.gemini/mcp.json` in the
//     same place is reported as off-root. docs/commands/audit.md states this
//     limit;
//   - root.Stat must not show a non-directory or a missing target (`.env`,
//     `.eslintrc`, a dangling link). A target outside the root cannot be
//     stat'd without leaving it, so it stays reported: that is the escaping
//     link ExpandTargets refuses.
func (s *scanner) classifyPrefix(rel string, segs []string, repo string) (quarantine.Carrier, string, bool) {
	c, anchor, ok := s.pick(segs, repo, s.matcher.MatchPrefix)
	if !ok || repo == "" || anchor != repo {
		return quarantine.Carrier{}, "", false
	}
	info, err := s.ops.stat(rel)
	switch {
	case err == nil && !info.IsDir():
		return quarantine.Carrier{}, "", false
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return quarantine.Carrier{}, "", false
	}
	return c, anchor, true
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
