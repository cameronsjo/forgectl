package projects

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// CloneFromGitHub searches GitHub for repos matching query, clones the
// chosen one into Dir, and opens it.
func (c *Client) CloneFromGitHub(ctx context.Context, query, name string) error {
	dest := filepath.Join(c.Dir, name)
	if _, err := os.Stat(dest); err != nil {
		if err := cloneRepo(ctx, c.run, name, dest); err != nil {
			return err
		}
	}
	return c.Open(ctx, dest)
}

// GitHubSearch proxies to the package-level helper for callers that need
// the repo list (e.g. projects_pick.go's clone-on-miss path).
func (c *Client) GitHubSearch(ctx context.Context, query string) ([]string, error) {
	return githubSearch(ctx, c.run, query)
}
