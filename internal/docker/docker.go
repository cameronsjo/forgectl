// Package docker is the ops layer for `forgectl docker`: build/run/shell
// wrapped around git-derived image tags. It knows nothing of Cobra — that
// decoupling is the house pattern (see internal/net, internal/tmux).
//
// Every shell-out — both `git` (to resolve repo/branch/sha) and `docker`
// itself — goes through the exec.Runner seam, never os/exec directly.
package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// defaultShell is the shell `shell` execs when --shell is omitted.
const defaultShell = "sh"

// Client builds/runs/shells into docker images tagged from git metadata
// (repo/branch/sha), caching the last-built tag so run/shell can omit --tag.
type Client struct {
	run exec.Runner

	defaultPlatform string
	extraLabel      string // "key=value"; empty means none configured

	lastTagPath string
	now         func() time.Time
}

// Option configures a Client at construction.
type Option func(*Client)

// WithDockerConfig applies the [docker] config section, filling in any
// field left zero with New's built-in default (none) rather than
// overwriting it.
func WithDockerConfig(dc config.DockerConfig) Option {
	return func(c *Client) {
		if dc.DefaultPlatform != "" {
			c.defaultPlatform = dc.DefaultPlatform
		}
		if dc.LabelTemplate != "" {
			c.extraLabel = dc.LabelTemplate
		}
	}
}

// WithNow overrides the clock used for the "created" OCI label and the
// cache's BuiltAt timestamp — used in tests so both are deterministic.
func WithNow(fn func() time.Time) Option {
	return func(c *Client) { c.now = fn }
}

// WithLastTagPath overrides the on-disk last-built-tag cache location
// (default: config.DockerLastTagPath()) — used in tests to point at a temp
// file.
func WithLastTagPath(path string) Option {
	return func(c *Client) { c.lastTagPath = path }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run: run,
		now: time.Now,
	}
	if path, err := config.DockerLastTagPath(); err == nil {
		c.lastTagPath = path
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LastTag returns the most recently cached build tag, if any.
func (c *Client) LastTag() (string, bool) {
	entry, ok := readLastTag(c.lastTagPath)
	if !ok || entry.Tag == "" {
		return "", false
	}
	return entry.Tag, true
}

// BuildOptions configures Build.
type BuildOptions struct {
	// ContextDir is the docker build context (and the git repo Build reads
	// repo/branch/sha from). Empty defaults to ".".
	ContextDir string
	// Platform overrides the --platform flag; empty uses the configured
	// default_platform, and omits the flag entirely if that's also empty.
	Platform string
}

// Build resolves repo/branch/sha from git, derives the
// {repo}:{branch-slug}-{shortsha} tag plus a :dev alias, and runs `docker
// build` with both tags and a set of OCI labels attached at the CLI (the
// Dockerfile itself is never edited). On success, the derived tag is cached
// so a later run/shell can omit --tag. Returns the derived (non-:dev) tag.
func (c *Client) Build(ctx context.Context, opts BuildOptions) (string, error) {
	contextDir := opts.ContextDir
	if contextDir == "" {
		contextDir = "."
	}
	if err := sandbox.RejectOptionLike("context", contextDir); err != nil {
		return "", err
	}

	slog.Debug("Preparing to build docker image.", "context", contextDir)

	repo, branch, sha, err := c.gitInfo(ctx, contextDir)
	if err != nil {
		slog.Error("Failed to resolve git metadata for docker build.", "context", contextDir, "error", err)
		return "", err
	}

	tag := deriveTag(repo, branch, sha)
	dev := devTag(repo)

	platform := opts.Platform
	if platform == "" {
		platform = c.defaultPlatform
	}

	args := []string{"build"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	for _, label := range c.buildLabels(branch, sha) {
		args = append(args, "--label", label)
	}
	args = append(args, "-t", tag, "-t", dev, "--", contextDir)

	if err := c.run.RunInteractive(ctx, "docker", args...); err != nil {
		slog.Error("Failed to build docker image.", "tag", tag, "error", err)
		return "", fmt.Errorf("docker build: %w", err)
	}

	if err := writeLastTag(c.lastTagPath, cacheEntry{Tag: tag, BuiltAt: c.now()}); err != nil {
		slog.Warn("Failed to cache last-built docker tag.", "path", c.lastTagPath, "error", err)
	}

	slog.Info("Successfully built docker image.", "tag", tag, "dev_tag", dev)
	return tag, nil
}

// buildLabels returns the --label values Build attaches: the built-in OCI
// labels plus, if configured, one extra "key=value" label.
func (c *Client) buildLabels(branch, sha string) []string {
	labels := []string{
		"org.opencontainers.image.revision=" + sha,
		"org.opencontainers.image.ref.name=" + slugifyBranch(branch),
		"org.opencontainers.image.created=" + c.now().UTC().Format(time.RFC3339),
	}
	if c.extraLabel != "" {
		labels = append(labels, c.extraLabel)
	}
	return labels
}

// RunOptions configures Run.
type RunOptions struct {
	// Tag is the image to run; empty reuses the cached last-built tag.
	Tag string
	// Args are passed through to `docker run` after the image (the
	// container's command/args).
	Args []string
}

// Run resolves Tag (explicit, or the cached last-built tag) and execs
// `docker run --rm -it <tag> <args...>`, handing the caller's tty to the
// container.
func (c *Client) Run(ctx context.Context, opts RunOptions) error {
	tag, err := c.resolveTag(opts.Tag)
	if err != nil {
		return err
	}

	slog.Debug("Preparing to run docker container.", "tag", tag)

	args := append([]string{"run", "--rm", "-it", tag}, opts.Args...)
	if err := c.run.RunInteractive(ctx, "docker", args...); err != nil {
		slog.Error("Failed to run docker container.", "tag", tag, "error", err)
		return fmt.Errorf("docker run: %w", err)
	}
	slog.Info("Successfully ran docker container.", "tag", tag)
	return nil
}

// ShellOptions configures Shell.
type ShellOptions struct {
	// Tag is the image to shell into; empty reuses the cached last-built tag.
	Tag string
	// Shell is the command exec'd inside the container; empty defaults to
	// "sh".
	Shell string
}

// Shell resolves Tag (explicit, or the cached last-built tag) and execs an
// interactive shell inside a throwaway container: `docker run --rm -it
// <tag> <shell>`.
func (c *Client) Shell(ctx context.Context, opts ShellOptions) error {
	tag, err := c.resolveTag(opts.Tag)
	if err != nil {
		return err
	}
	shellCmd := opts.Shell
	if shellCmd == "" {
		shellCmd = defaultShell
	}
	if err := sandbox.RejectOptionLike("shell", shellCmd); err != nil {
		return err
	}

	slog.Debug("Preparing to open docker shell.", "tag", tag, "shell", shellCmd)
	if err := c.run.RunInteractive(ctx, "docker", "run", "--rm", "-it", tag, shellCmd); err != nil {
		slog.Error("Failed to open docker shell.", "tag", tag, "shell", shellCmd, "error", err)
		return fmt.Errorf("docker run (shell): %w", err)
	}
	slog.Info("Successfully opened docker shell.", "tag", tag, "shell", shellCmd)
	return nil
}

// resolveTag picks explicit if set, else the cached last-built tag, and
// rejects an option-like result (a leading '-' would be parsed as a docker
// flag rather than the image positional) before it can reach docker argv.
func (c *Client) resolveTag(explicit string) (string, error) {
	tag := explicit
	if tag == "" {
		cached, ok := c.LastTag()
		if !ok {
			return "", errors.New("no image tag available: pass --tag or run `forgectl docker build` first")
		}
		tag = cached
	}
	if err := sandbox.RejectOptionLike("tag", tag); err != nil {
		return "", err
	}
	return tag, nil
}

// gitInfo resolves the repo name (git worktree toplevel's base name),
// current branch, and short commit sha for dir via `git`. All three are
// shelled through c.run, never os/exec directly.
func (c *Client) gitInfo(ctx context.Context, dir string) (repo, branch, sha string, err error) {
	top, err := c.run.Run(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve git repo root: %w", err)
	}
	repo = filepath.Base(strings.TrimSpace(top))

	branchOut, err := c.run.Run(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve git branch: %w", err)
	}
	branch = strings.TrimSpace(branchOut)

	shaOut, err := c.run.Run(ctx, "git", "-C", dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve git sha: %w", err)
	}
	sha = strings.TrimSpace(shaOut)

	return repo, branch, sha, nil
}
