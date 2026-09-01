package projects

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// fanOut applies f to every element of in, bounded to discoverConcurrency()
// concurrent workers, and returns the results in input order. Each worker
// writes to its own out[i] — distinct-index writes are race-free without a
// mutex, so no post-sort is needed to recover input order.
//
// The semaphore acquire (`sem <- struct{}{}`) sits in the loop BEFORE the
// `go` statement, deliberately: that bounds the number of live goroutines,
// not just the number doing work at once. Moving the acquire inside the
// goroutine would still cap concurrent f calls correctly, but every one of
// len(in) goroutines would be created up front — at 10k candidates that's
// 8 live stacks vs. 10,000, not a cosmetic difference.
//
// fanOut does not check ctx for cancellation itself — a cancelled context
// still runs every f to completion and returns a full-length result with a
// nil error. This matches the serial loop it replaced, which had the same
// property, and nothing in the current call sites cancels these commands
// mid-flight; a future caller that needs early-exit-on-cancel would need to
// add that here.
func fanOut[I, O any](in []I, f func(I) O) []O {
	out := make([]O, len(in))
	sem := make(chan struct{}, discoverConcurrency())
	var wg sync.WaitGroup
	for i, item := range in {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = f(item)
		}()
	}
	wg.Wait()
	return out
}

// discoverConcurrency bounds the fan-out width of each individual fanOut
// call, not the program as a whole — there is no shared bound. Inventory is
// the worst case: localRepos' fan-out and githubList's own fan-out (one gh
// process per resolved owner) can be in flight at the same time, each up to
// discoverConcurrency() wide, alongside the single tea process. Its peak is
// therefore roughly 2*discoverConcurrency()+1 processes, not
// discoverConcurrency()+2. (localRepos runs Discover to completion first, so
// discoverDir's fan-out and localRepos' do not overlap each other.)
// Measured on a 500-repo synthetic fixture (M3 Air, warm cache): serial
// process spawn was the whole cost — 9.1s wall at 93% of a single core end to
// end. gitStatus spawned up to two git processes per repo at the time of that
// measurement (status, then rev-list on a clean tree); it now spawns exactly
// one, whatever the tree's state (forgectl#216). The `remote get-url` spawn
// belongs to localRepos' separate fan-out, not this count.
// Isolating just the fan-out: serial 13.6s → 8 workers 3.5s → 16
// workers 3.2s. The knee sits at NumCPU; 16 buys only 8% over 8. Floored at
// 4 so a low-core machine still gets some overlap; capped at 16 because
// spawning one git process per repo unbounded (500+ concurrent git
// processes) is not something to do to a laptop.
func discoverConcurrency() int {
	return max(4, min(runtime.NumCPU(), 16))
}

// sortProjects orders projects by Name, tie-breaking on Dir when two
// entries share a Name (e.g. github/ownerA/tool and gitea/ownerB/tool).
//
// The tie-break is belt-and-suspenders, not a repair for lost ordering:
// fanOut's distinct-index writes already preserve candidate order (see its
// doc comment), and phase 1's os.ReadDir-driven walk already produces that
// candidate order in Dir-lexicographic order, so in the current pipeline
// the two same-Name entries would already arrive here Dir-sorted even
// without this tie-break. What the tie-break actually buys is independence
// from sort.Slice's own tie-resolution — the standard library explicitly
// does not guarantee it preserves input order for elements the comparator
// treats as equal, so relying on that coincidence would be fragile against
// a future stdlib change or a future caller that feeds this a differently
// ordered input. See TestSortProjects_TieBreaksOnDir, which constructs an
// input directly (bypassing the filesystem walk) to exercise this
// independently of that coincidence.
func sortProjects(projects []Project) {
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].Dir < projects[j].Dir
	})
}

// Client discovers and opens local project directories.
type Client struct {
	Dir string
	run exec.Runner

	// gitBin is resolved once when New constructs the client. Every status
	// probe and the pull it authorizes use this same value, so a later PATH
	// change cannot make the check and mutation execute different binaries.
	gitBin    string
	gitPinned bool
	lookPath  func(string) (string, error)

	// githubOwners is the configured [projects].owners list. Empty means the
	// authenticated login on the configured GitHub host, resolved per
	// Inventory call rather than cached — the answer belongs to whoever gh is
	// authenticated as right now.
	githubOwners []string

	// gitHubHost is the deployment's configured GitHub host ([github].host,
	// already validated by githubauth.ResolveHost at the CLI seam). It pins
	// every gh subprocess and anchors canonicalHost's trusted arm. New defaults
	// it to githubauth.DefaultHost; read it through effectiveGitHubHost, never
	// directly, so a struct-literal Client gets the same default.
	gitHubHost string

	// wings is the resolved [[projects.wings]] placement table. The zero value
	// is a valid empty table — every repo falls to the host tree.
	wings WingTable
}

// Option configures a Client at construction. Options exist so the GitHub
// owner scope can be injected from config without every existing caller —
// including the tests that build a Client over a fake runner — having to
// thread a value they do not care about.
type Option func(*Client)

// WithGitHubOwners sets the GitHub accounts the inventory enumerates. The
// slice is copied: the caller's config value must not be mutable through the
// Client afterwards. An empty or nil list keeps the default posture, the
// authenticated GitHub.com login.
func WithGitHubOwners(owners []string) Option {
	return func(c *Client) {
		c.githubOwners = append([]string(nil), owners...)
	}
}

// WithGitHubHost sets the deployment's GitHub host. The value must already be
// ResolveHost output — the CLI seam validates before construction, and an
// invalid value that somehow arrived here would fail closed anyway (the
// pinned runner refuses gh for a host that fails validation). An empty value
// keeps the default, github.com.
func WithGitHubHost(host string) Option {
	return func(c *Client) {
		if host != "" {
			c.gitHubHost = host
		}
	}
}

// WithWings sets the resolved [[projects.wings]] placement table. The table
// must already be ResolveWings output — the CLI seam validates every name and
// repo entry before construction. An empty table is the default: every repo
// lands in the host tree.
func WithWings(table WingTable) Option {
	return func(c *Client) { c.wings = table }
}

// WingFor returns the wing r is filed into, or "" for the host tree. Exported
// because the CLI's --dry-run and its "already cloned elsewhere" message need
// the same answer Clone will act on, rather than recomputing it.
func (c *Client) WingFor(r Repo) string { return c.wings.For(r.Owner, r.Name) }

// ProjectsDir returns the resolved projects root ($PROJECTS_DIR, else
// ~/Projects). Exported so --dry-run can call Placement without a Client-shaped
// duplicate of the same resolution.
func (c *Client) ProjectsDir() string { return c.Dir }

// GitHubHost returns the deployment's effective GitHub host — the value every
// gh subprocess is pinned to. CLI call sites need it for ParseCloneTarget and
// the non-default-host stderr note.
func (c *Client) GitHubHost() string { return c.effectiveGitHubHost() }

// effectiveGitHubHost is the one place the zero-value fallback lives. A Client
// built by struct literal (tests, and any future caller that skips New)
// carries no host, and every consumer of that field now makes a SECURITY
// decision with it: canonicalHost's trusted arm, and both clone dispatches.
//
// Without this, converting the dispatch from the constant `case "github"` to a
// field comparison would inherit the hole rather than the defense — a
// zero-value client would fail the trusted branch and fall through to cloning
// a server-supplied URL with no GH_HOST pin and no token scrub. canonicalHost
// has defaulted an empty host since the exact-match fix; this makes the same
// defense reachable from the dispatch side.
func (c *Client) effectiveGitHubHost() string {
	if c.gitHubHost == "" {
		return githubauth.DefaultHost
	}
	return c.gitHubHost
}

// WithGitBinary overrides the git executable used by status probes and pulls.
// The path must already be absolute and clean; anything else selects the same
// fail-closed unavailable state as a failed construction-time PATH lookup.
// Production relies on New's own resolution; this explicit seam is primarily
// for deterministic callers and tests that provide their own runner.
func WithGitBinary(path string) Option {
	return func(c *Client) {
		if filepath.IsAbs(path) && filepath.Clean(path) == path {
			c.gitBin = path
		} else {
			c.gitBin = ""
		}
		c.gitPinned = true
	}
}

// withGitLookPath replaces PATH resolution for same-package tests. Unlike an
// explicit binary override, the resolved result still goes through New's
// absolute-path normalization.
func withGitLookPath(fn func(string) (string, error)) Option {
	return func(c *Client) { c.lookPath = fn }
}

// New builds a Client. It reads $PROJECTS_DIR, falling back to ~/Projects.
// A leading ~ is expanded so env vars stored as "~/Projects" work correctly.
// It also resolves git exactly once to an absolute path; a lookup failure is
// retained as an empty pin so status probes fail closed as StatusUnknown.
func New(run exec.Runner, opts ...Option) *Client {
	dir := os.Getenv("PROJECTS_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Projects")
	} else if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	c := &Client{Dir: dir, run: run, lookPath: osexec.LookPath, gitHubHost: githubauth.DefaultHost}
	for _, opt := range opts {
		opt(c)
	}
	if !c.gitPinned {
		c.gitPinned = true
		if path, err := c.lookPath("git"); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				c.gitBin = abs
			}
		}
	}
	return c
}

// gitBinary returns New's pinned answer. An empty result is the fail-closed
// unavailable state: gitStatus reports Unknown and PullAll never mutates.
func (c *Client) gitBinary() string {
	return c.gitBin
}

// Discover returns every project found under Dir, covering both layouts in
// play during the canonical-layout transition:
//
//   - legacy flat clones: Dir/<repo>               (.git at the top level)
//   - canonical clones:   Dir/<host>/<owner>/<repo> (.git three levels down)
//
// A top-level entry is walked as a canonical host bucket only when it
// contains at least one owner/repo path that bottoms out in a git repo;
// otherwise it's treated as a flat project itself, so legacy discovery
// (including non-git dirs, which still get a zero GitStatus) is unchanged.
func (c *Client) Discover(ctx context.Context) ([]Project, error) {
	return c.discoverDir(ctx, c.Dir)
}

// discoverCandidate is a project's identity resolved by the pure-filesystem
// walk phase of discoverDir, deferred so the subprocess-spawning status
// check (discoverProject) can run across every candidate in one bounded
// fan-out instead of serially inline with the walk.
type discoverCandidate struct {
	name string
	dir  string
}

// discoverDir is Discover's dir-parameterized body — PullAll walks a
// caller-supplied subtree (`pull-all [dir]`) the same way Discover walks
// c.Dir, so the two share this implementation rather than diverging.
//
// Discovery runs in two phases: phase 1 is a plain serial filesystem walk
// (ReadDir/Stat only, no subprocesses) that resolves every project's (name,
// dir); phase 2 fans discoverProject's git spawns out across
// discoverConcurrency() workers. Splitting it this way keeps the cheap walk
// serial and simple while parallelizing only the part that's actually slow.
func (c *Client) discoverDir(ctx context.Context, dir string) ([]Project, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("projects directory not found: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading projects directory: %w", err)
	}
	var candidates []discoverCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		top := filepath.Join(dir, e.Name())
		if isGitRepo(top) {
			candidates = append(candidates, discoverCandidate{e.Name(), top})
			continue
		}
		if canon := discoverCanonicalHostCandidates(top); len(canon) > 0 {
			candidates = append(candidates, canon...)
			continue
		}
		candidates = append(candidates, discoverCandidate{e.Name(), top})
	}

	projects := fanOut(candidates, func(cand discoverCandidate) Project {
		return c.discoverProject(ctx, cand.name, cand.dir)
	})

	sortProjects(projects)
	return projects, nil
}

// discoverCanonicalHostCandidates walks a potential host bucket (Dir/<host>)
// two levels deep — owner, then repo — collecting the (name, dir) of every
// repo dir with a .git marker. Pure filesystem work, no subprocesses.
// Returns nil when the bucket contains no such repos, signalling the caller
// to fall back to treating the bucket itself as a flat legacy project (e.g. a
// plain non-git directory like a scratch notes folder).
func discoverCanonicalHostCandidates(hostDir string) []discoverCandidate {
	ownerEntries, err := os.ReadDir(hostDir)
	if err != nil {
		return nil
	}
	var out []discoverCandidate
	for _, oe := range ownerEntries {
		if !oe.IsDir() {
			continue
		}
		ownerDir := filepath.Join(hostDir, oe.Name())
		repoEntries, err := os.ReadDir(ownerDir)
		if err != nil {
			continue
		}
		for _, re := range repoEntries {
			if !re.IsDir() {
				continue
			}
			repoDir := filepath.Join(ownerDir, re.Name())
			if !isGitRepo(repoDir) {
				continue
			}
			out = append(out, discoverCandidate{re.Name(), repoDir})
		}
	}
	return out
}

// discoverProject builds a Project for dir. gitStatus stats .git itself and
// returns StatusNotRepo for a miss, so it's always safe to call — no extra
// subprocess is spawned for a non-git dir.
func (c *Client) discoverProject(ctx context.Context, name, dir string) Project {
	return Project{Name: name, Dir: dir, Status: gitStatus(ctx, c.run, c.gitBinary(), dir)}
}

// isGitRepo reports whether dir has a .git marker.
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// Open attaches to the tmux session named after dir's basename, creating it
// detached first if it does not exist.
//
// The whole flow lives in internal/tmux, which is the single owner of tmux
// targeting. The version here used to be its own: `has-session -t <name>`
// followed by `new-session`/`attach-session` on that same bare name. Every one
// of those operands went through tmux's target grammar, so opening a project
// called `forge` while a `forge-review` session existed found the sibling and
// attached to it — the project never opened, and nothing reported a problem
// (forgectl#237).
func (c *Client) Open(ctx context.Context, dir string) error {
	name := filepath.Base(dir)
	client := tmux.New(c.run)
	session, err := client.EnsureSession(ctx, name, dir)
	if err != nil {
		return fmt.Errorf("opening tmux session %s: %w", name, err)
	}
	return client.AttachSession(ctx, session)
}

// localRepos walks the local clones under Dir and attributes each by its origin
// remote — host/owner/name parsed from `git remote get-url origin`, never the
// bare directory name. A dir with no git repo or no origin remote becomes a
// local-only Repo (empty Host/Owner) that dedups by path.
func (c *Client) localRepos(ctx context.Context) ([]Repo, error) {
	projs, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	out := fanOut(projs, func(p Project) Repo {
		r := Repo{
			Name:      p.Name,
			Cloned:    true,
			LocalPath: p.Dir,
			Status:    p.Status,
		}
		// A non-repo has no origin of its own — `git -C` walks up to find one,
		// which would misattribute it to an ancestor repo's origin (and then
		// dedup it away). Skip the spawn entirely for that case.
		if p.Status.State != StatusNotRepo {
			url, err := c.run.Run(ctx, "git", "-C", p.Dir, "remote", "get-url", "origin")
			if err == nil {
				url = strings.TrimSpace(url)
				if host, owner, name := parseRemoteURL(url, c.effectiveGitHubHost()); name != "" {
					r.Host, r.Owner, r.Name = host, owner, name
					// SSHURL is contractually an SSH clone URL; an HTTPS origin would
					// mislabel it in the JSON inventory, so only store SSH-form origins.
					if isSSHURL(url) {
						r.SSHURL = url
					}
				}
			}
		}
		return r
	})
	return out, nil
}

// Inventory builds the unified cross-host project list: local clones merged with
// every repo on GitHub and Gitea, deduped by Repo.Key() with the local clone
// winning (it carries LocalPath + Status). The two remote lists are fetched
// concurrently. A host that errors (gh unauthenticated, gitea unreachable)
// contributes no rows and a human-readable note instead of failing the whole
// call — so a partial outage still answers "where's my project?".
//
// Returns (repos, notes, err). err is non-nil only for a catastrophic local
// failure that isn't a missing projects dir; notes carries per-host degradation
// messages for the caller to surface on stderr.
func (c *Client) Inventory(ctx context.Context) ([]Repo, []string, error) {
	slog.Debug("Preparing to build inventory.", "projectsDir", c.Dir)
	start := time.Now()
	var notes []string

	// Kick off both remote fetches first so they overlap the local walk below —
	// the per-clone git fan-out is the slow part, so the network calls hide
	// under it rather than adding to it.
	// notes carries a host's own per-owner diagnostics, which survive even when
	// the host also errors overall — an all-owners-failed GitHub run should say
	// WHICH owners failed, not just that the host degraded.
	type hostResult struct {
		host  string
		repos []Repo
		notes []string
		err   error
	}
	const remoteHosts = 2
	ch := make(chan hostResult, remoteHosts)
	// These two labels name the SOURCE — which enumerator produced the rows —
	// not the host a row lands on. They stay fixed strings on purpose: they
	// order the notes deterministically below, and a Gitea run's rows can now
	// carry any hostname (or none, if every row was dropped), so there is no
	// single host to label the source with.
	const (
		sourceGitHub = "github"
		sourceGitea  = "gitea"
	)
	go func() {
		r, n, e := githubList(ctx, c.run, c.githubOwners, c.effectiveGitHubHost())
		ch <- hostResult{sourceGitHub, r, n, e}
	}()
	go func() {
		r, e := giteaList(ctx, c.run, c.effectiveGitHubHost())
		ch <- hostResult{sourceGitea, r, nil, e}
	}()

	local, err := c.localRepos(ctx)
	if err != nil {
		// A missing/unreadable projects dir shouldn't suppress the remote view —
		// degrade to "no local clones" and note it.
		// Deliberately NOT categorical, unlike the host legs below. Those carry
		// a remote's stderr — text a hostile or MITM'd server authors, with no
		// diagnostic worth the risk. This one carries c.Dir and the OS's own
		// errno text, which is the whole answer to "why is my inventory empty",
		// and it is config- and filesystem-material rather than server-chosen.
		slog.Warn("Failed to enumerate local repos.", "projectsDir", c.Dir, "error", err)
		notes = append(notes, fmt.Sprintf("local: %v", err))
		local = nil
	}

	// Collect first, fold second: the two fetches finish in whatever order the
	// network decides, but the notes a human reads must not reorder run to run.
	fetched := make(map[string]hostResult, remoteHosts)
	for i := 0; i < remoteHosts; i++ {
		res := <-ch
		fetched[res.host] = res
	}
	var remote []Repo
	for _, host := range []string{sourceGitHub, sourceGitea} {
		res := fetched[host]
		notes = append(notes, res.notes...)
		if res.err != nil {
			// Categorical note, raw cause to the log only. res.err reaches here
			// straight off a subprocess — gh stderr, or tea's, which is server-
			// supplied text a hostile or MITM'd host controls. These notes are
			// printed to a terminal, so interpolating %v here would hand that
			// server a write channel to the operator's screen.
			slog.Warn("Host degraded.", "host", host, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: host query failed", host))
			continue
		}
		slog.Debug("Host succeeded.", "host", host, "count", len(res.repos))
		remote = append(remote, res.repos...)
	}

	seen := make(map[string]bool, len(local))
	out := make([]Repo, 0, len(local)+len(remote))
	for _, r := range local {
		out = append(out, r)
		seen[r.Key()] = true
	}
	for _, r := range remote {
		if seen[r.Key()] {
			continue // already checked out locally
		}
		out = append(out, r)
		seen[r.Key()] = true
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Owner < out[j].Owner
	})
	slog.Info("Successfully built inventory.", "total", len(out), "local", len(local), "duration", time.Since(start).Round(time.Millisecond))
	return out, notes, nil
}

// ListOrg returns every GitHub repo owned by org (a user or org login), for
// the bulk-clone path (`projects clone --org`). org comes straight off the
// command line, so it is vetted as a safe path segment before it becomes a
// `gh` argv: the guard rejects a traversal value and a leading '-' that gh's
// cobra parser would read as a flag rather than a positional. githubListOrg
// re-checks it against the owner charset immediately before argv construction
// — the two guards answer different questions (is it a safe path segment; is
// it a plausible owner), and the value is untrusted for both. The result isn't
// merged with local/gitea state — it's a plain listing to feed straight into
// Clone.
func (c *Client) ListOrg(ctx context.Context, org string) ([]Repo, error) {
	if !validPathSegment(org) {
		return nil, fmt.Errorf("invalid GitHub org/user name %q", org)
	}
	return githubListOrg(ctx, c.run, org, c.effectiveGitHubHost())
}

// Clone checks out a remote Repo where Placement says it belongs — its wing if
// it has one, otherwise the host/owner/name tree — dispatching by host, and
// returns the local destination path. A repo already present at that dest is a
// no-op (returns its path). Clones on the configured GitHub host go through gh
// (credential handling and the host pin); everything else clones the SSH URL
// directly.
//
// New clones always land in the canonical layout — the flat legacy layout is a
// read-side (Discover) affordance only, so existing on-disk clones stay
// findable without new clones perpetuating the collision-prone flat tree.
func (c *Client) Clone(ctx context.Context, r Repo) (string, error) {
	return c.CloneInto(ctx, r, c.WingFor(r))
}

// CloneInto is Clone with the wing supplied by the caller rather than looked
// up — the seam `projects clone --wing <name>` uses to override the configured
// table for one clone. An empty wing means the host tree, NOT "look it up":
// the lookup is Clone's job, so a caller that means "consult the table" calls
// Clone.
func (c *Client) CloneInto(ctx context.Context, r Repo, wing string) (string, error) {
	slog.Debug("Preparing to clone repo.", "host", r.Host, "owner", r.Owner, "name", r.Name, "wing", wing)
	dest, err := Placement(c.Dir, r, wing)
	if err != nil {
		// Categorical: the rejected segments are exactly the untrusted values,
		// and this string reaches a terminal. They are in the debug log above.
		return "", fmt.Errorf("refusing to clone: %w", err)
	}
	if _, err := os.Stat(dest); err == nil {
		// Something is already at dest. Only treat it as "already cloned" when it
		// really is THIS repo — the canonical layout already separates repos by
		// host/owner, so a mismatch here means dest was populated by hand (or the
		// remote origin changed), not a bare-name collision.
		if c.originMatches(ctx, dest, r) {
			slog.Debug("Repo already cloned, skipping clone.", "dest", dest, "name", r.Name)
			return dest, nil
		}
		// dest is forgectl-composed from already-guarded segments, so it is
		// safe to name; r.Host/Owner/Name are server-supplied and stay out.
		return "", fmt.Errorf("%s already exists but its origin is a different repo — "+
			"clone it elsewhere by hand", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("creating canonical clone parent dirs for %s: %w", dest, err)
	}
	// The dispatch predicate is the HOSTNAME, not a token. Only the configured
	// GitHub host clones through gh, which supplies its own URL under the
	// GH_HOST pin and the non-default-host token scrub that cloneRepo applies
	// internally. Every other host clones the URL the row carried.
	if r.Host == c.effectiveGitHubHost() {
		if err := cloneRepo(ctx, c.run, r.Owner+"/"+r.Name, dest, c.effectiveGitHubHost()); err != nil {
			slog.Error("Failed to clone from GitHub.", "host", r.Host, "repo", r.Owner+"/"+r.Name, "dest", dest, "error", err)
			return "", err
		}
	} else {
		if err := cloneFromGitea(ctx, c.run, r.SSHURL, dest); err != nil {
			slog.Error("Failed to clone from host.", "host", r.Host, "name", r.Name, "dest", dest, "error", err)
			return "", err
		}
	}
	slog.Info("Successfully cloned repo.", "host", r.Host, "name", r.Name, "dest", dest)
	return dest, nil
}

// Placement returns where r is filed under root. It is the single path
// formula, and it implements the estate's two filing rules verbatim:
//
//	wing != ""  ->  <root>/<wing>/<name>
//	otherwise   ->  <root>/<host>/<owner>/<name>
//
// Every segment is lowercased, mirroring Repo.Key() — the filesystem tree is
// the mirror of the dedup identity, so "where is it cloned" and "what is it"
// never disagree.
//
// It promises A PATH, NOT A POLICY. Three callers with incompatible leaf
// semantics use it — Clone writes a checkout there, Worktree writes a .bare
// plus per-branch trees and refuses an existing leaf, the duplicate probe only
// reads — and none of that belongs in a path formula.
//
// TWO TIERS OF SEGMENT GUARD, because the segments carry different risk. The
// host is the only one that can arrive as an arbitrary hostname from a remote
// URL, so it goes through githubauth.ValidHostSegment: an anchored charset
// that rejects ':' (APFS-legal, rendered as '/' in Finder), a leading '.' (a
// tree invisible to ls), control characters and ANSI escapes, homoglyphs, and
// anything over-long. Owner, name, and wing keep validPathSegment's traversal
// and flag-injection guards, which is what they need.
//
// It returns an error rather than falling back, because falling back is how a
// rejected value still ends up on disk under a different name.
func Placement(root string, r Repo, wing string) (string, error) {
	name := strings.ToLower(r.Name)
	if !validPathSegment(name) {
		return "", fmt.Errorf("refusing to place a repo: unsafe name segment")
	}
	if wing != "" {
		w := strings.ToLower(wing)
		if !validPathSegment(w) {
			return "", fmt.Errorf("refusing to place a repo: unsafe wing segment")
		}
		return filepath.Join(root, w, name), nil
	}
	host := strings.ToLower(r.Host)
	if !githubauth.ValidHostSegment(host) {
		return "", fmt.Errorf("refusing to place a repo: unsafe host segment")
	}
	owner := strings.ToLower(r.Owner)
	if !validPathSegment(owner) {
		return "", fmt.Errorf("refusing to place a repo: unsafe owner segment")
	}
	return filepath.Join(root, host, owner, name), nil
}

// validPathSegment rejects a host/owner/name value that would escape or
// collapse the projects dir when joined onto it (empty → the dir itself;
// "/"/".." → traversal), or smuggle a flag into a `gh`/`git` argv (a leading
// "-" would be read as an option, not a positional). Remote hosts never
// produce such values, but the guard keeps a malformed list row, a
// user-supplied clone target (`clone owner/repo`, `clone --org <login>`), or a
// hand-crafted Repo from turning a clone into a path-traversal, a flag
// injection, or a tmux session on the projects root.
func validPathSegment(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.HasPrefix(s, "-") &&
		!strings.ContainsAny(s, "/\\")
}

// maxRepoSegmentBytes bounds an owner or repo name used as a path segment.
// POSIX NAME_MAX is 255 and resolve.go silently DROPS longer directory names,
// so a row past this is unwritable and would also be invisible to the
// launcher. 100 is GitHub's own limit for both an owner and a repo name.
const maxRepoSegmentBytes = 100

// validRepoSegment vets an owner or repo NAME arriving from a remote listing —
// gh's JSON, tea's TSV — before it is stamped onto a Repo and becomes a
// directory. validPathSegment is not enough here, and the gap is not
// theoretical: it accepts a leading '.', so a repo literally named ".git"
// yields a wing directory that isGitRepo then reports as a repo, hiding every
// member of that wing; ".bare" collides with the worktree layout's object
// store. It also accepts ':' (APFS-legal, rendered as '/' in Finder), control
// characters and ANSI escapes, whitespace, homoglyphs, and any length.
//
// The charset is pr.ValidOwnerRepoPart's — the repo's one anchored owner/repo
// predicate, reused rather than re-spelled — plus the leading-dot and length
// bounds it does not carry. Rows failing this are dropped from the listing
// rather than repaired: a name we cannot file is a row we cannot act on.
func validRepoSegment(s string) bool {
	return validPathSegment(s) &&
		!strings.HasPrefix(s, ".") &&
		len(s) <= maxRepoSegmentBytes &&
		pr.ValidOwnerRepoPart(s)
}

// originMatches reports whether the git checkout at dir has an origin remote that
// resolves to r's (host, owner, name) — i.e. dir really is r, not a same-named
// repo from a different host.
func (c *Client) originMatches(ctx context.Context, dir string, r Repo) bool {
	url, err := c.run.Run(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	host, owner, name := parseRemoteURL(strings.TrimSpace(url), c.effectiveGitHubHost())
	return host == r.Host && owner == r.Owner && name == r.Name
}

// isSSHURL reports whether a git remote URL uses an SSH transport — the ssh://
// scheme or the scp-like git@host:path form.
func isSSHURL(u string) bool {
	return strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git@")
}
