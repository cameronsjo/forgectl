package projects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Exact target resolution: turning what an operator typed into the one
// directory a surface will run in.
//
// This is deliberately not Discover. Discover exists to inventory projects and
// does Git work to do it — it opens repositories, reads remotes, and reports
// state. None of that is wanted here, because the answer to "which directory"
// must not depend on whether a repository is healthy, whether the network is
// up, or how long a scan takes. A surface launch that stalls because a remote
// is slow would be a launch that stalls for no reason the operator can see.
//
// So this walks the filesystem and nothing else, and it walks it under caps.
// The root is operator-controlled and can be enormous or hostile-shaped — a
// directory of a million entries, a symlink loop — and an unbounded ReadDir
// would materialize all of it before anyone could refuse.

const (
	// maxExaminedEntries bounds the total directory entries considered across
	// the whole search. It is generous for a real projects root and small
	// enough that a pathological one is refused rather than paged in.
	maxExaminedEntries = 50_000

	// readDirBatch is the streaming chunk size. Streaming rather than reading
	// a whole directory at once is the point: os.ReadDir on a directory with a
	// million entries allocates a million entries.
	readDirBatch = 256

	// maxNameBytes bounds a single bare name. Anything longer is not a project
	// directory, and refusing early keeps the comparison cheap.
	maxNameBytes = 255

	// maxCandidates bounds how many matches are collected before giving up.
	// Two is already a failure — the point of collecting more is a message
	// that says how ambiguous the name was, not an exhaustive list.
	maxCandidates = 8
)

var (
	// ErrTargetNotFound reports a name that matched nothing under the root.
	ErrTargetNotFound = errors.New("projects: no project matches that name")

	// ErrTargetAmbiguous reports a name that matched more than one directory.
	// It is a refusal rather than a choice: picking one would be picking which
	// of the operator's projects to start a session in.
	ErrTargetAmbiguous = errors.New("projects: that name matches more than one project")

	// ErrTargetUnusable reports a path that exists but is not a directory, or
	// one that cannot be canonicalized.
	ErrTargetUnusable = errors.New("projects: target is not a usable directory")

	// ErrSearchTooLarge reports a root too big to search under the caps.
	ErrSearchTooLarge = errors.New("projects: project root is too large to search by name")
)

// ResolveTarget turns an operator's argument into one canonical directory.
//
// Two shapes, and the distinction is about who chose the directory:
//
// A path — anything containing a separator, or starting with "." or "~" — is
// the operator naming a directory explicitly. It may be anywhere, including
// outside the project root, because an explicit path is an explicit choice.
//
// A bare name is a lookup, and it searches only beneath the root, only the
// three layouts forgectl creates (flat, wing/repo, and host/owner/repo), and
// matches exactly.
// Zero matches and several matches both fail; there is no nearest-match, no
// prefix, and no interactive disambiguation, because the result of guessing
// wrong is a session that runs in the wrong repository.
func (c *Client) ResolveTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("%w: no target given", ErrTargetUnusable)
	}
	if len(target) > maxNameBytes && !looksLikePath(target) {
		return "", fmt.Errorf("%w: name is too long", ErrTargetNotFound)
	}

	if looksLikePath(target) {
		return canonicalDir(expandHome(target))
	}
	return c.resolveByName(target)
}

// looksLikePath reports whether the operator named a location rather than a
// project. A separator is the signal; "." and ".." and "~" are spelled out
// because they are locations that happen not to contain one.
func looksLikePath(target string) bool {
	return strings.ContainsRune(target, filepath.Separator) ||
		target == "." || target == ".." || target == "~"
}

// expandHome resolves a leading ~ so an operator can name a path the way they
// would type it in a shell. Only a leading bare ~ or ~/: ~user is deliberately
// unsupported, because resolving another account's home is not something a
// launch should be doing.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// canonicalDir makes a path absolute, resolves symlinks, and proves it is a
// directory.
//
// Symlinks are resolved rather than preserved because this path becomes the
// harness's working directory *and* the key a launch profile is matched
// against. Two spellings of one directory would match two different profiles,
// and the operator would have no way to see which one they got.
func canonicalDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTargetUnusable, termsafe.Error(err))
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTargetUnusable, termsafe.Error(err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTargetUnusable, termsafe.Error(err))
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory",
			ErrTargetUnusable, termsafe.QuotePath(resolved))
	}
	return resolved, nil
}

// resolveByName searches the root for exactly one directory with this name.
func (c *Client) resolveByName(name string) (string, error) {
	if c.Dir == "" {
		return "", fmt.Errorf("%w: no project root is configured", ErrTargetNotFound)
	}

	budget := maxExaminedEntries
	matches, err := c.searchRoot(name, &budget)
	if err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		// The name is not echoed. It came from the command line and is not
		// secret, but every other error in this package is category-only and a
		// name that happens to contain an escape sequence would be rendered.
		return "", ErrTargetNotFound
	case 1:
		return canonicalDir(matches[0])
	default:
		// NAME THE PATHS. A repo checked out in both a wing and the host tree
		// yields two matches, and that is a real estate defect the operator
		// has to go fix — a bare count tells them a duplicate exists but not
		// which two directories to reconcile. Each goes through QuotePath: a
		// directory under the root can be named anything a hand-made mkdir
		// allows, forgectl's own placement guards notwithstanding. The NAME
		// still is not echoed, for the reason given in the case-0 arm.
		quoted := make([]string, 0, len(matches))
		for _, m := range matches {
			quoted = append(quoted, termsafe.QuotePath(m))
		}
		sort.Strings(quoted)
		return "", fmt.Errorf("%w: %d matches: %s", ErrTargetAmbiguous, len(matches), strings.Join(quoted, ", "))
	}
}

// searchRoot looks in the three layouts forgectl creates, in order.
//
// Flat first — `<root>/<name>` — because it is the common case and needs no
// walk at all. Then wings, `<root>/<wing>/<name>`. Then host/owner/repo, the
// layout `forgectl pull` uses, walked two levels deep and no further. A deeper
// walk would start finding vendored checkouts and node_modules, and matching
// one of those would run a session inside a dependency.
//
// Wings add one KNOWN layout, not unbounded depth: the wing pass matches at
// depth 2, which this walk already reached to enumerate owners, so the depth
// contract above is unchanged.
// Matching compares directory entries byte-for-byte rather than stat'ing a
// joined path, and that is not a stylistic preference. macOS's default
// filesystem is case-insensitive: os.Lstat("<root>/CADENCE") succeeds when the
// directory on disk is "cadence", and FileInfo.Name() reports the spelling
// that was *asked for*, not the one stored. So a stat-based lookup would make
// "exact match" mean "exact on Linux, case-folded on a Mac" — and a resolver
// whose rules change with the filesystem is one an operator cannot reason
// about. Comparing entries makes the claim true everywhere.
func (c *Client) searchRoot(name string, budget *int) ([]string, error) {
	var matches []string

	hosts, err := readDirNames(c.Dir, budget)
	if err != nil {
		return nil, err
	}
	for _, entry := range hosts {
		// The flat layout: <root>/<name>.
		if entry == name && isRealDir(filepath.Join(c.Dir, entry)) {
			matches = append(matches, filepath.Join(c.Dir, entry))
			continue
		}

		// Otherwise this entry may be a host, holding owners, holding repos.
		hostDir := filepath.Join(c.Dir, entry)
		if !isRealDir(hostDir) {
			continue
		}
		owners, err := readDirNames(hostDir, budget)
		if err != nil {
			return nil, err
		}
		for _, owner := range owners {
			ownerDir := filepath.Join(hostDir, owner)
			if !isRealDir(ownerDir) {
				continue
			}

			// The WING layout: <root>/<wing>/<name>, one level shallower than
			// host/owner/repo. A wing member sits exactly where an owner would,
			// so without this it is walked THROUGH rather than matched, and
			// `surface launch <name>` cannot find it.
			//
			// The gate is the .git marker, matching discoverWingCandidates
			// rather than this function's usual isRealDir — a wing member is a
			// checkout by definition, and requiring the marker is also what
			// keeps a real owner directory (which holds repos but is not one)
			// from matching here.
			if owner == name && isGitRepo(ownerDir) {
				matches = append(matches, ownerDir)
				if len(matches) > maxCandidates {
					return matches, nil
				}
			}
			repos, err := readDirNames(ownerDir, budget)
			if err != nil {
				return nil, err
			}
			for _, repo := range repos {
				if repo != name {
					continue
				}
				candidate := filepath.Join(ownerDir, repo)
				if !isRealDir(candidate) {
					continue
				}
				matches = append(matches, candidate)
				if len(matches) > maxCandidates {
					return matches, nil
				}
			}
		}
	}
	return matches, nil
}

// isRealDir reports whether path is a directory and not a symlink to one.
//
// Lstat, not Stat. A bare-name lookup that followed directory symlinks would
// let a link inside the root silently redirect a launch anywhere on the
// filesystem — and the operator typed a project name, not a location. Naming
// the target explicitly is the supported way to reach it, and that path goes
// through canonicalDir, which does resolve links.
func isRealDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// readDirNames streams a directory's entries under the shared budget.
//
// ReadDir(n) in batches rather than ReadDir(-1): the root is
// operator-controlled, and a directory with a million entries should be
// refused rather than allocated. The budget is shared across the whole search
// so a wide root cannot be walked by walking many narrow ones.
func readDirNames(dir string, budget *int) ([]string, error) {
	f, err := os.Open(dir) //nolint:gosec // G304: the project root is operator-configured, and reading it is the function
	if err != nil {
		// An unreadable directory is not an error for the search — it is one
		// place the project is not.
		return nil, nil //nolint:nilerr // an unreadable directory is a miss, not a failure
	}
	defer func() { _ = f.Close() }()

	var names []string
	for {
		entries, err := f.ReadDir(readDirBatch)
		for _, e := range entries {
			*budget--
			if *budget <= 0 {
				return nil, ErrSearchTooLarge
			}
			if len(e.Name()) <= maxNameBytes {
				names = append(names, e.Name())
			}
		}
		if err != nil {
			// io.EOF ends the walk; anything else is a directory that stopped
			// answering, which is the same outcome for a search.
			return names, nil
		}
	}
}
