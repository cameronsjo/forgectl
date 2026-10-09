package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// The fetch side: every read Facts needs, through one gh runner the caller
// pins to github.com (githubauth.Runner). Reads happen in this order: the
// discovery query, the PR query (which names the head), the checks query
// at that head (which reads the PR's head and base again and refuses if
// either moved), the compare of base and head (the file list), the trees
// for the changed files' modes, and the two ancestry compares. Everything
// but discovery and the PR query names the head or base commit explicitly.

// ErrRead marks a GitHub read that failed; `surface status` exits 1 on it.
var ErrRead = errors.New("merge: GitHub could not be read")

// ErrNoRecordedRepo reports a ledger row with no GitHub repository: it was
// launched before the record existed, or its origin is not on github.com.
var ErrNoRecordedRepo = errors.New("merge: the worker's ledger row records no GitHub repository")

// Host is the GitHub host every read names.
const Host = "github.com"

// maxTreeReads caps the directory listings one read makes for file modes.
const maxTreeReads = 100

// Cache keeps the head-immutable part of a read: the file list and modes.
// Read and Write may fail; a failure only means no cache.
type Cache interface {
	Read(head string) ([]byte, error)
	Write(head string, data []byte) error
}

// Reader reads Facts from GitHub. GH must be pinned to github.com. Cache is
// nil for `surface merge` and the drain, which never read it.
type Reader struct {
	GH    exec.Runner
	Cache Cache
}

// Snapshot is one read: the Facts, whether a PR was found, why not, and
// the commit statuses at the head (shown, never counted).
type Snapshot struct {
	Facts    Facts
	HasPR    bool
	NoPR     string
	Statuses []string
	// Cached is true when the file list and modes came from the cache.
	Cached bool
}

// cacheEntry is what the cache holds for one head.
type cacheEntry struct {
	Head      string `json:"head"`
	Base      string `json:"base"`
	MergeBase string `json:"merge_base"`
	Files     []File `json:"files"`
}

func (r Reader) graphQL(ctx context.Context, query string, vars ...string) ([]byte, error) {
	args := []string{"api", "graphql", "--hostname", Host, "-f", "query=" + query}
	args = append(args, vars...)
	out, err := r.GH.Run(ctx, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("%w: gh api graphql: %w", ErrRead, err)
	}
	return []byte(out), nil
}

func (r Reader) rest(ctx context.Context, p string) ([]byte, error) {
	out, err := r.GH.Run(ctx, "gh", "api", "--hostname", Host, p)
	if err != nil {
		return nil, fmt.Errorf("%w: gh api %s: %w", ErrRead, p, err)
	}
	return []byte(out), nil
}

// notFound reports a gh call GitHub answered with HTTP 404.
func notFound(err error) bool {
	var ce *exec.CommandError
	// Matched against the raw stderr, never rendered: Error() redacts it.
	return errors.As(err, &ce) && ce.Name == "gh" && strings.Contains(ce.Stderr, "HTTP 404")
}

func decodeErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRead, err)
}

// Read gathers Facts for row. A head branch with no PR, or more than one
// open PR, is not an error: Snapshot.NoPR says which.
func (r Reader) Read(ctx context.Context, row Row) (Snapshot, error) {
	owner, name, err := SplitNameWithOwner(row.GitHubRepo)
	if row.GitHubRepo == "" || row.GitHubRepoID == 0 || err != nil {
		return Snapshot{}, ErrNoRecordedRepo
	}
	snap := Snapshot{Facts: Facts{Row: row}}
	data, err := r.graphQL(ctx, DiscoverQuery, "-f", "owner="+owner, "-f", "name="+name, "-f", "head="+row.Branch)
	if err != nil {
		return Snapshot{}, err
	}
	disc, err := DecodeDiscovery(data)
	if err != nil {
		return Snapshot{}, decodeErr(err)
	}
	snap.Facts.Repository, snap.Facts.OperatorID = disc.Repository, disc.ViewerID
	cand, err := SelectPR(disc, row.Branch)
	if errors.Is(err, ErrNoPR) || errors.Is(err, ErrAmbiguousPR) {
		snap.NoPR = err.Error()
		return snap, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	snap.HasPR = true
	number := strconv.Itoa(cand.Number)
	data, err = r.graphQL(ctx, PRQuery, "-f", "owner="+owner, "-f", "name="+name, "-F", "number="+number)
	if err != nil {
		return Snapshot{}, err
	}
	pr, err := DecodePR(data)
	if err != nil {
		return Snapshot{}, decodeErr(err)
	}
	if pr.Repository.DatabaseID != disc.Repository.DatabaseID {
		return Snapshot{}, fmt.Errorf("%w: the repository id changed between reads (%d, then %d)", ErrRead, disc.Repository.DatabaseID, pr.Repository.DatabaseID)
	}
	head, base := pr.PR.HeadRefOid, pr.PR.BaseRefOid
	f := &snap.Facts
	f.PR, f.Reviews, f.Threads, f.Comments = pr.PR, pr.Reviews, pr.Threads, pr.Comments
	data, err = r.graphQL(ctx, ChecksQuery, "-f", "owner="+owner, "-f", "name="+name, "-F", "number="+number, "-f", "head="+head)
	if err != nil {
		return Snapshot{}, err
	}
	checks, err := DecodeChecks(data, head)
	if err != nil {
		return Snapshot{}, decodeErr(err)
	}
	if checks.HeadRefOid != head || checks.BaseRefOid != base {
		return Snapshot{}, fmt.Errorf("%w: the PR moved during the read (head %s then %s, base %s then %s); run again", ErrRead, short(head), short(checks.HeadRefOid), short(base), short(checks.BaseRefOid))
	}
	f.Checks, snap.Statuses = checks.Runs, checks.Statuses
	files, cached, unread, err := r.files(ctx, owner, name, base, head)
	if err != nil {
		return Snapshot{}, err
	}
	f.Files, f.Unread, snap.Cached = files, unread, cached
	if isSHA(row.Base) {
		if f.BaseAncestry, err = r.compareStatus(ctx, owner, name, row.Base, base); err != nil {
			return Snapshot{}, err
		}
		if f.HeadAncestry, err = r.compareStatus(ctx, owner, name, row.Base, head); err != nil {
			return Snapshot{}, err
		}
	}
	return snap, nil
}

// ErrRepoChanged reports a recorded repository GitHub now gives another id:
// it was deleted and re-created, or its name now belongs to another one.
var ErrRepoChanged = errors.New("merge: the recorded repository's id changed on GitHub")

// Discover finds the worker's PR with the discovery query alone: the same
// filter Read and `surface status` apply (SelectPR: its head branch on the
// recorded repository, not a fork, by the operator, at most one open). It
// is the drain closer's read, which needs only the PR's state. A head
// branch with no PR is ErrNoPR, more than one open is ErrAmbiguousPR, and a
// repository whose id is no longer the recorded one is ErrRepoChanged.
func (r Reader) Discover(ctx context.Context, row Row) (Candidate, error) {
	owner, name, err := SplitNameWithOwner(row.GitHubRepo)
	if row.GitHubRepo == "" || row.GitHubRepoID == 0 || err != nil {
		return Candidate{}, ErrNoRecordedRepo
	}
	data, err := r.graphQL(ctx, DiscoverQuery, "-f", "owner="+owner, "-f", "name="+name, "-f", "head="+row.Branch)
	if err != nil {
		return Candidate{}, err
	}
	disc, err := DecodeDiscovery(data)
	if err != nil {
		return Candidate{}, decodeErr(err)
	}
	if disc.Repository.DatabaseID != row.GitHubRepoID {
		return Candidate{}, fmt.Errorf("%w: %s is id %d on GitHub, the row recorded %d", ErrRepoChanged, row.GitHubRepo, disc.Repository.DatabaseID, row.GitHubRepoID)
	}
	return SelectPR(disc, row.Branch)
}

// compareStatus is compare(from...to).status. A compare GitHub answers
// with 404 (a commit it does not have, such as a recorded base that was
// never pushed) is CompareNotFound: a refusal reason, not a failed read.
func (r Reader) compareStatus(ctx context.Context, owner, name, from, to string) (string, error) {
	data, err := r.rest(ctx, "repos/"+owner+"/"+name+"/compare/"+from+"..."+to+"?per_page=1")
	if notFound(err) {
		return CompareNotFound, nil
	}
	if err != nil {
		return "", err
	}
	c, err := DecodeCompare(data)
	if err != nil {
		return "", decodeErr(err)
	}
	return c.Status, nil
}

// files returns the changed files with modes, from the cache when an entry
// for head was written against the same base, else from the compare and
// the trees.
func (r Reader) files(ctx context.Context, owner, name, base, head string) ([]File, bool, []string, error) {
	if r.Cache != nil {
		if data, err := r.Cache.Read(head); err == nil && data != nil {
			var e cacheEntry
			if json.Unmarshal(data, &e) == nil && e.Head == head && e.Base == base && e.Files != nil {
				return e.Files, true, nil, nil
			}
		}
	}
	data, err := r.rest(ctx, "repos/"+owner+"/"+name+"/compare/"+base+"..."+head+"?per_page=1")
	if err != nil {
		return nil, false, nil, err
	}
	cmp, err := DecodeCompare(data)
	if err != nil {
		return nil, false, nil, decodeErr(err)
	}
	if !isSHA(cmp.MergeBase) {
		return nil, false, nil, fmt.Errorf("%w: the compare names merge base %q, expected a commit id", ErrRead, cmp.MergeBase)
	}
	files := cmp.Files
	if files == nil {
		files = []File{}
	}
	unread, err := r.modes(ctx, owner, name, cmp.MergeBase, head, files)
	if err != nil {
		return nil, false, nil, err
	}
	if r.Cache != nil && len(unread) == 0 {
		// termsafe:allow-raw-json private 0600 cache file read back by forgectl, never written to a terminal
		if data, err := json.Marshal(cacheEntry{Head: head, Base: base, MergeBase: cmp.MergeBase, Files: files}); err == nil {
			_ = r.Cache.Write(head, data) // a cache that cannot be written only means no cache
		}
	}
	return files, false, unread, nil
}

type treeKey struct{ sha, dir string }

// modes fills BaseMode (at the merge base, from PreviousPath for a rename)
// and HeadMode from one directory listing per (commit, directory). A path
// that fails CheckChangedPath, or a status outside the four the policy
// judges, gets no mode read: Evaluate refuses it on its own.
func (r Reader) modes(ctx context.Context, owner, name, mergeBase, head string, files []File) ([]string, error) {
	need := map[treeKey]bool{}
	baseOf := func(f File) string {
		if f.Status == "renamed" {
			return f.PreviousPath
		}
		return f.Path
	}
	for _, f := range files {
		if config.CheckChangedPath(f.Path) != nil || (f.Status == "renamed" && config.CheckChangedPath(f.PreviousPath) != nil) {
			continue
		}
		switch f.Status {
		case "added":
			need[treeKey{head, path.Dir(f.Path)}] = true
		case "removed":
			need[treeKey{mergeBase, path.Dir(f.Path)}] = true
		case "modified", "renamed":
			need[treeKey{mergeBase, path.Dir(baseOf(f))}] = true
			need[treeKey{head, path.Dir(f.Path)}] = true
		}
	}
	if len(need) > maxTreeReads {
		return []string{fmt.Sprintf("file modes: the changes span %d directory listings, more than the %d one read makes", len(need), maxTreeReads)}, nil
	}
	keys := make([]treeKey, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].sha+keys[i].dir < keys[j].sha+keys[j].dir })
	trees := map[treeKey]map[string]string{}
	for _, k := range keys {
		ref := k.sha
		if k.dir != "." {
			ref += ":" + escapePath(k.dir)
		}
		data, err := r.rest(ctx, "repos/"+owner+"/"+name+"/git/trees/"+ref)
		if err != nil {
			return nil, err
		}
		m, err := DecodeTree(data)
		if err != nil {
			return nil, decodeErr(err)
		}
		trees[k] = m
	}
	for i := range files {
		f := &files[i]
		if bm, ok := trees[treeKey{mergeBase, path.Dir(baseOf(*f))}]; ok && f.Status != "added" {
			f.BaseMode = bm[path.Base(baseOf(*f))]
		}
		if hm, ok := trees[treeKey{head, path.Dir(f.Path)}]; ok && f.Status != "removed" {
			f.HeadMode = hm[path.Base(f.Path)]
		}
	}
	return nil, nil
}

// escapePath escapes each segment of a checked repository path for an API
// path, keeping the slashes.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
