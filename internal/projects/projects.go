package projects

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Client discovers and opens local project directories.
type Client struct {
	Dir string
	run exec.Runner
}

// New builds a Client. It reads $PROJECTS_DIR, falling back to ~/Projects.
// A leading ~ is expanded so env vars stored as "~/Projects" work correctly.
func New(run exec.Runner) *Client {
	dir := os.Getenv("PROJECTS_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Projects")
	} else if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	return &Client{Dir: dir, run: run}
}

// Discover returns all immediate subdirectories of Dir as Projects, each
// annotated with their git working-tree status. Non-git dirs get a zero
// GitStatus (Label() returns "[clean]" which we suppress for non-git dirs
// via the empty-label path in DisplayLine).
func (c *Client) Discover(ctx context.Context) ([]Project, error) {
	if _, err := os.Stat(c.Dir); err != nil {
		return nil, fmt.Errorf("projects directory not found: %s", c.Dir)
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil, fmt.Errorf("reading projects directory: %w", err)
	}
	var projects []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(c.Dir, e.Name())
		isGit := false
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			isGit = true
		}
		p := Project{Name: e.Name(), Dir: dir}
		if isGit {
			p.Status = gitStatus(ctx, c.run, dir)
		}
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})
	return projects, nil
}

// Open creates a new detached tmux session named after dir's basename (or
// reattaches if one exists), then switches/attaches the current client.
func (c *Client) Open(ctx context.Context, dir string) error {
	name := filepath.Base(dir)

	// Check if session exists.
	_, err := c.run.Run(ctx, "tmux", "has-session", "-t", name)
	if err != nil {
		// Session doesn't exist — create it.
		_, err = c.run.Run(ctx, "tmux", "new-session", "-d", "-s", name, "-c", dir)
		if err != nil {
			return fmt.Errorf("creating tmux session %s: %w", name, err)
		}
	}

	// Attach or switch depending on whether we're already inside tmux.
	if c.InsideTmux() {
		_, err = c.run.Run(ctx, "tmux", "switch-client", "-t", name)
		return err
	}
	return c.run.RunInteractive(ctx, "tmux", "attach-session", "-t", name)
}

// InsideTmux reports whether the process is running inside a tmux client.
func (c *Client) InsideTmux() bool {
	return os.Getenv("TMUX") != ""
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
	out := make([]Repo, 0, len(projs))
	for _, p := range projs {
		r := Repo{
			Name:      p.Name,
			Cloned:    true,
			LocalPath: p.Dir,
			Status:    p.Status,
		}
		url, err := c.run.Run(ctx, "git", "-C", p.Dir, "remote", "get-url", "origin")
		if err == nil {
			url = strings.TrimSpace(url)
			if host, owner, name := parseRemoteURL(url); name != "" {
				r.Host, r.Owner, r.Name = host, owner, name
				// SSHURL is contractually an SSH clone URL; an HTTPS origin would
				// mislabel it in the JSON inventory, so only store SSH-form origins.
				if isSSHURL(url) {
					r.SSHURL = url
				}
			}
		}
		out = append(out, r)
	}
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
	type hostResult struct {
		host  string
		repos []Repo
		err   error
	}
	const remoteHosts = 2
	ch := make(chan hostResult, remoteHosts)
	go func() { r, e := githubList(ctx, c.run); ch <- hostResult{"github", r, e} }()
	go func() { r, e := giteaList(ctx, c.run); ch <- hostResult{"gitea", r, e} }()

	local, err := c.localRepos(ctx)
	if err != nil {
		// A missing/unreadable projects dir shouldn't suppress the remote view —
		// degrade to "no local clones" and note it.
		slog.Warn("Failed to enumerate local repos.", "projectsDir", c.Dir, "error", err)
		notes = append(notes, fmt.Sprintf("local: %v", err))
		local = nil
	}

	var remote []Repo
	for i := 0; i < remoteHosts; i++ {
		res := <-ch
		if res.err != nil {
			slog.Warn("Host degraded.", "host", res.host, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.host, res.err))
			continue
		}
		slog.Debug("Host succeeded.", "host", res.host, "count", len(res.repos))
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

// Clone checks out a remote Repo into Dir, dispatching by host, and returns the
// local destination path. A repo already present on disk is a no-op (returns its
// path). github.com clones go through gh (credential handling); everything else
// clones the SSH URL directly.
func (c *Client) Clone(ctx context.Context, r Repo) (string, error) {
	slog.Debug("Preparing to clone repo.", "host", r.Host, "owner", r.Owner, "name", r.Name)
	if !validRepoName(r.Name) {
		return "", fmt.Errorf("refusing to clone %s/%s: unsafe repo name %q", r.Host, r.Owner, r.Name)
	}
	dest := filepath.Join(c.Dir, r.Name)
	if _, err := os.Stat(dest); err == nil {
		// Something is already at dest. Only treat it as "already cloned" when it
		// really is THIS repo — a same-name repo from the other host must not
		// silently open the wrong checkout (github/homeclaw vs gitea/homeclaw both
		// want ~/Projects/homeclaw in a flat layout).
		if c.originMatches(ctx, dest, r) {
			slog.Debug("Repo already cloned, skipping clone.", "dest", dest, "name", r.Name)
			return dest, nil
		}
		return "", fmt.Errorf("%s already exists but its origin is a different repo; "+
			"%s/%s collides by name — clone it elsewhere by hand", dest, r.Host, r.Owner)
	}
	switch r.Host {
	case "github":
		if err := cloneRepo(ctx, c.run, r.Owner+"/"+r.Name, dest); err != nil {
			slog.Error("Failed to clone from GitHub.", "host", r.Host, "repo", r.Owner+"/"+r.Name, "dest", dest, "error", err)
			return "", err
		}
	default:
		// gitea and any SSH-reachable host: clone the URL directly.
		if err := cloneFromGitea(ctx, c.run, r.SSHURL, dest); err != nil {
			slog.Error("Failed to clone from host.", "host", r.Host, "name", r.Name, "dest", dest, "error", err)
			return "", err
		}
	}
	slog.Info("Successfully cloned repo.", "host", r.Host, "name", r.Name, "dest", dest)
	return dest, nil
}

// validRepoName rejects names that would escape or collapse the projects dir
// when joined onto it (empty → the dir itself; "/"/".." → traversal). Remote
// hosts never produce such names, but the guard keeps a malformed list row from
// turning a clone into a path-traversal or a tmux session on the projects root.
func validRepoName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\")
}

// originMatches reports whether the git checkout at dir has an origin remote that
// resolves to r's (host, owner, name) — i.e. dir really is r, not a same-named
// repo from a different host.
func (c *Client) originMatches(ctx context.Context, dir string, r Repo) bool {
	url, err := c.run.Run(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	host, owner, name := parseRemoteURL(strings.TrimSpace(url))
	return host == r.Host && owner == r.Owner && name == r.Name
}

// isSSHURL reports whether a git remote URL uses an SSH transport — the ssh://
// scheme or the scp-like git@host:path form.
func isSSHURL(u string) bool {
	return strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git@")
}
