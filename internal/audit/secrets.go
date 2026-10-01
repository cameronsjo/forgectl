package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// This file is the native half of the secret-hygiene scan (forgectl#14, lane
// 2): stray .env files, private keys, and secret-scanner config, found by
// name over the same confined walk as the injection inventory. It reads no
// file's contents, with one exception: a *.pem or *.key file is opened
// through the root and its first sniffBytes checked for a private-key
// header (fsOps.sniff), which yields a boolean and keeps no bytes.
//
// Whether a file is tracked or ignored is git's to say, and this package
// runs nothing: ApplyGitStatus takes the answer from a caller-supplied
// GitStatusFunc (internal/audit/gitstate runs the hardened
// `git ls-files` per repo).

// Secret finding kinds. The wire values are part of the --json contract.
const (
	// KindEnv is a dotenv-style file: .env, .env.*, .envrc.
	KindEnv = "env"
	// KindKey is a private key: an ssh identity, a PKCS#12 or Java keystore,
	// or a .pem/.key file holding a PEM private-key header.
	KindKey = "key"
	// KindScannerConfig is a .gitleaks.toml or .gitleaksignore: config a
	// repo can use to hide findings from a secret scanner.
	KindScannerConfig = "scanner-config"
)

// Secret finding flags. The wire values are part of the --json contract.
const (
	// FlagTracked: git tracks the file, so it is in the repo's history.
	FlagTracked = "tracked"
	// FlagUnignored: an untracked .env that no ignore rule covers, so a
	// `git add .` would commit it.
	FlagUnignored = "unignored"
	// FlagOutsideRepo: a .env outside any git working tree.
	FlagOutsideRepo = "outside-repo"
	// FlagGitUnknown: git could not say whether the file is tracked or
	// ignored (the call failed or timed out, or git listed the path nowhere,
	// as it does a FIFO), so it is listed rather than assumed ignored.
	FlagGitUnknown = "git-unknown"
	// FlagVendored: inside a dependency directory.
	FlagVendored = "vendored"
	// FlagLoose: readable or writable by group or others (mode & 0o077, the
	// rule ssh applies to a private key).
	FlagLoose = "loose"
	// FlagForeignOwner: a key owned by a uid other than the one running the
	// scan.
	FlagForeignOwner = "foreign-owner"
)

// flagRank fixes the order flags are reported in.
var flagRank = map[string]int{
	FlagTracked: 0, FlagUnignored: 1, FlagOutsideRepo: 2, FlagGitUnknown: 3,
	FlagVendored: 4, FlagLoose: 5, FlagForeignOwner: 6,
}

// keyNames are ssh identity basenames: the private half of each key type
// ssh-keygen writes by default.
var keyNames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	"id_ecdsa_sk": true, "id_ed25519_sk": true,
}

// keyExts are extensions that hold a private key whatever their contents
// look like: both are binary containers forgectl does not parse.
var keyExts = map[string]bool{".p12": true, ".pfx": true, ".keystore": true}

// sniffExts are extensions shared by private keys and public material
// (certificates, public keys), told apart by the sniff.
var sniffExts = map[string]bool{".pem": true, ".key": true}

// envTemplateExts mark a committed .env template, not a live one.
var envTemplateExts = map[string]bool{
	".example": true, ".sample": true, ".template": true, ".tmpl": true, ".dist": true,
}

// scannerConfigNames are the files gitleaks reads from a scanned directory.
var scannerConfigNames = map[string]bool{".gitleaks.toml": true, ".gitleaksignore": true}

// asciiLower folds only A-Z, so a non-ASCII rune that Unicode lowercases to
// ASCII (the Kelvin sign to k) cannot make a name match.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// classifySecret names the kind a basename belongs to, and whether the
// match still needs the sniff. kind is "" for no match.
func classifySecret(name string) (kind string, needsSniff bool) {
	n := asciiLower(name)
	switch {
	case n == ".env" || n == ".envrc":
		return KindEnv, false
	case strings.HasPrefix(n, ".env."):
		if envTemplateExts[path.Ext(n)] {
			return "", false
		}
		return KindEnv, false
	case keyNames[n]:
		return KindKey, false
	case scannerConfigNames[n]:
		return KindScannerConfig, false
	case keyExts[path.Ext(n)]:
		return KindKey, false
	case sniffExts[path.Ext(n)]:
		return KindKey, true
	}
	return "", false
}

// SecretsOptions configures ScanSecrets. Zero caps take the defaults.
type SecretsOptions struct {
	// Root is the directory to scan, made absolute (not symlink-resolved).
	Root        string
	MaxEntries  int
	MaxFindings int
	MaxDepth    int
}

// SecretFinding is one native finding. Paths are absolute, under the root.
type SecretFinding struct {
	Path  string
	Repo  string // nearest enclosing git working tree, "" when none
	Kind  string
	Type  string
	Flags []string
	// repoRel is Path relative to Repo, slash-separated: the pathspec the
	// git status asks about.
	repoRel string
}

// SecretsReport is ScanSecrets' result.
type SecretsReport struct {
	Root     string
	Findings []SecretFinding
	// Repos lists every git working tree the walk entered, absolute and
	// sorted: the targets of the gitleaks pass.
	Repos []string
	// Entries counts directory entries examined.
	Entries int
	// Unreadable counts directories the walk could not list and entries it
	// could not Lstat.
	Unreadable int
	// UnreadableFiles counts .pem/.key files the sniff could not read, which
	// are neither listed nor ruled out.
	UnreadableFiles int
	// IgnoredEnv counts .env files git ignores. They are the normal
	// development pattern, so they are counted and not listed.
	IgnoredEnv int
	// GitStatusFailed counts repos git could not answer for; their findings
	// carry FlagGitUnknown.
	GitStatusFailed int
	// GitBudgetExhausted counts the repos among GitStatusFailed that the
	// scan budget ran out on, before or during their git call.
	GitBudgetExhausted int
	// UnsafeScannerConfig lists, absolute and sorted, every repo whose root
	// .gitleaksignore or .gitleaks.toml is not a regular file. It is checked
	// as the walk enters each repo, outside the findings cap, so a repo the
	// gitleaks pass must not open is known even when the list was cut short.
	UnsafeScannerConfig []string
	Truncated           bool
	CappedBy            []string
	DepthSkipped        int
	// The caps this scan ran with, after defaults.
	MaxEntries, MaxFindings, MaxDepth int
}

// Stopped reports whether a cap ended the scan early.
func (r SecretsReport) Stopped() bool {
	for _, c := range r.CappedBy {
		if c == CapEntries || c == CapFindings {
			return true
		}
	}
	return false
}

type secretScanner struct {
	root   string
	ops    fsOps
	opts   Options
	euid   int
	report *SecretsReport
	stats  *walkStats
}

// ScanSecrets walks opts.Root for stray .env files, private keys and
// scanner config. It errors only when the root itself cannot be resolved or
// opened. Tracked and ignored state is not known until ApplyGitStatus runs.
func ScanSecrets(opts SecretsOptions) (SecretsReport, error) {
	abs, err := filepath.Abs(opts.Root)
	if err != nil {
		return SecretsReport{}, fmt.Errorf("resolve audit root %s: %w", termsafe.QuotePath(opts.Root), termsafe.Error(err))
	}
	ops, closeRoot, err := openRootOps(abs)
	if err != nil {
		return SecretsReport{}, err
	}
	defer closeRoot()
	return scanSecretsWith(abs, ops, opts, currentEUID())
}

// scanSecretsWith is ScanSecrets' walk over an opened root, through an
// explicit filesystem surface and effective uid (-1 for none).
func scanSecretsWith(root string, ops fsOps, so SecretsOptions, euid int) (SecretsReport, error) {
	opts := Options{MaxEntries: so.MaxEntries, MaxFindings: so.MaxFindings, MaxDepth: so.MaxDepth}.withDefaults()
	report := SecretsReport{
		Root: root, Findings: []SecretFinding{}, Repos: []string{}, CappedBy: []string{}, UnsafeScannerConfig: []string{},
		MaxEntries: opts.MaxEntries, MaxFindings: opts.MaxFindings, MaxDepth: opts.MaxDepth,
	}
	stats := &walkStats{CappedBy: []string{}}
	s := &secretScanner{root: root, ops: ops, opts: opts, euid: euid, report: &report, stats: stats}
	w := &walker{ops: ops, root: root, maxEntries: opts.MaxEntries, maxDepth: opts.MaxDepth, stats: stats, visit: s.visit, onRepo: s.checkScannerConfig}
	if err := w.run(); err != nil {
		return SecretsReport{}, err
	}
	sort.Strings(report.UnsafeScannerConfig)
	for _, repo := range stats.Repos {
		report.Repos = append(report.Repos, filepath.Join(root, filepath.FromSlash(repo)))
	}
	sort.Strings(report.Repos)
	report.Entries = stats.Entries
	report.Unreadable = stats.Unreadable
	report.CappedBy = stats.CappedBy
	report.Truncated = len(stats.CappedBy) > 0
	report.DepthSkipped = stats.DepthSkipped
	sortSecretFindings(report.Findings)
	return report, nil
}

func sortSecretFindings(list []SecretFinding) {
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
}

// checkScannerConfig Lstats a repo's root scanner config as the walk enters
// the repo. gitleaks opens a root .gitleaksignore unconditionally (and, were
// --config ever dropped, .gitleaks.toml), so anything there but a regular
// file (a FIFO would block the open) keeps the repo from the gitleaks pass.
// An entry that cannot be Lstat'd cannot be judged and counts as unsafe.
func (s *secretScanner) checkScannerConfig(dir string, names []string) {
	for _, name := range names {
		if !scannerConfigNames[asciiLower(name)] {
			continue
		}
		info, err := s.ops.lstat(path.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			s.report.UnsafeScannerConfig = append(s.report.UnsafeScannerConfig, filepath.Join(s.root, filepath.FromSlash(dir)))
			return
		}
	}
}

// visit classifies one entry. Directories are never findings (a Python
// virtualenv is often named .env) and are always descended; a symlink is
// reported as itself, unread.
func (s *secretScanner) visit(e entry) (bool, error) {
	mode := e.info.Mode()
	if mode.IsDir() {
		return false, nil
	}
	kind, needsSniff := classifySecret(e.segs[len(e.segs)-1])
	if kind == "" {
		return false, nil
	}
	if needsSniff {
		switch {
		case mode&fs.ModeSymlink != 0:
			// Reported as the link, unread: the sniff never follows one.
		case mode.IsRegular():
			found, err := s.ops.sniff(e.rel)
			if err != nil {
				s.report.UnreadableFiles++
				return false, nil
			}
			if !found {
				return false, nil
			}
		default:
			return false, nil // a FIFO or device named *.pem is not read
		}
	}
	if len(s.report.Findings) >= s.opts.MaxFindings {
		s.stats.capped(CapFindings)
		return true, errStop
	}
	s.report.Findings = append(s.report.Findings, s.finding(e, kind))
	return false, nil
}

func (s *secretScanner) finding(e entry, kind string) SecretFinding {
	mode := e.info.Mode()
	f := SecretFinding{
		Path:  filepath.Join(s.root, filepath.FromSlash(e.rel)),
		Kind:  kind,
		Type:  modeType(mode),
		Flags: []string{},
	}
	if e.repo != "" {
		f.Repo = filepath.Join(s.root, filepath.FromSlash(e.repo))
		f.repoRel = e.rel
		if e.repo != "." {
			f.repoRel = strings.TrimPrefix(e.rel, e.repo+"/")
		}
	}
	if kind == KindEnv && e.repo == "" {
		f.Flags = append(f.Flags, FlagOutsideRepo)
	}
	if e.vendored {
		f.Flags = append(f.Flags, FlagVendored)
	}
	if kind != KindScannerConfig && mode.IsRegular() && permsMeaningful && mode.Perm()&0o077 != 0 {
		f.Flags = append(f.Flags, FlagLoose)
	}
	if kind == KindKey && mode.IsRegular() && s.euid >= 0 {
		if uid, ok := ownerUID(e.info); ok && uid != int64(s.euid) {
			f.Flags = append(f.Flags, FlagForeignOwner)
		}
	}
	return f
}

// GitState is what git says about one path.
type GitState int

const (
	// GitIgnored: git neither tracks the path nor would add it.
	GitIgnored GitState = iota
	// GitTracked: the path is in the index.
	GitTracked
	// GitUntracked: the path is untracked and no ignore rule covers it.
	GitUntracked
)

// GitStatusFunc answers, for the slash-separated paths rels relative to the
// working tree repo, which are tracked, which are untracked but not ignored,
// and which are ignored. A path it does not mention is unknown (git lists a
// FIFO in none of its lists), never assumed ignored. An error means git
// could not answer for any of them.
type GitStatusFunc func(repo string, rels []string) (map[string]GitState, error)

// ErrBudgetExhausted is what a GitStatusFunc returns, alone or wrapped, for
// a repo the scan's budget ran out on: either no time was left to start git,
// or the budget's deadline ended the call.
var ErrBudgetExhausted = errors.New("scan budget exhausted")

// gitStateRank orders answers by how strongly they put a file on the list.
var gitStateRank = map[GitState]int{GitIgnored: 0, GitUntracked: 1, GitTracked: 2}

// ApplyGitStatus asks status about every finding inside a repo, once per
// repo, and applies the answer: tracked files are flagged; an untracked
// .env no rule ignores is flagged unignored; an ignored .env is dropped from
// the list and counted in IgnoredEnv. A repo status cannot answer for, and
// a path git lists nowhere, keep their findings flagged git-unknown, since
// assuming them ignored would hide them.
func (r *SecretsReport) ApplyGitStatus(status GitStatusFunc) {
	byRepo := map[string][]int{}
	var repos []string
	for i, f := range r.Findings {
		if f.Repo == "" || f.Kind == KindScannerConfig {
			continue
		}
		if _, seen := byRepo[f.Repo]; !seen {
			repos = append(repos, f.Repo)
		}
		byRepo[f.Repo] = append(byRepo[f.Repo], i)
	}
	drop := map[int]bool{}
	for _, repo := range repos {
		idx := byRepo[repo]
		rels := make([]string, 0, len(idx))
		for _, i := range idx {
			rels = append(rels, r.Findings[i].repoRel)
		}
		states, err := status(repo, rels)
		if err != nil {
			r.GitStatusFailed++
			if errors.Is(err, ErrBudgetExhausted) {
				r.GitBudgetExhausted++
			}
			for _, i := range idx {
				r.Findings[i].Flags = append(r.Findings[i].Flags, FlagGitUnknown)
			}
			continue
		}
		// git spells a path as its index does; on a case-insensitive
		// filesystem that can differ in case from the name the walk saw.
		// Where folding merges two answers, the one that lists the file
		// wins: tracked over untracked over ignored.
		folded := make(map[string]GitState, len(states))
		for p, st := range states {
			if prev, ok := folded[asciiLower(p)]; !ok || gitStateRank[st] > gitStateRank[prev] {
				folded[asciiLower(p)] = st
			}
		}
		for _, i := range idx {
			f := &r.Findings[i]
			st, ok := states[f.repoRel]
			if !ok {
				st, ok = folded[asciiLower(f.repoRel)]
			}
			if !ok {
				f.Flags = append(f.Flags, FlagGitUnknown)
				continue
			}
			switch {
			case st == GitTracked:
				f.Flags = append(f.Flags, FlagTracked)
			case st == GitUntracked && f.Kind == KindEnv:
				f.Flags = append(f.Flags, FlagUnignored)
			case st == GitIgnored && f.Kind == KindEnv:
				drop[i] = true
			}
		}
	}
	kept := r.Findings[:0]
	for i, f := range r.Findings {
		if drop[i] {
			r.IgnoredEnv++
			continue
		}
		sort.SliceStable(f.Flags, func(a, b int) bool { return flagRank[f.Flags[a]] < flagRank[f.Flags[b]] })
		kept = append(kept, f)
	}
	r.Findings = kept
}
