package cli

// Test plan for docker.go
//
// newDockerCmd / newDockerCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: `docker build [context]` reports the derived tag on stdout,
//       stderr silent when git metadata resolved
//   [x] Happy: `docker run` with no --tag reuses the tag from a prior build
//   [x] Happy: `docker shell` with no --tag reuses the tag from a prior build
//   [x] Happy: subcommand aliases (b/r/sh) resolve to their canonical verb
//   [x] Degraded: total and partial git failures have exact stdout/stderr
//       contracts, retain the best stable dev identity, and disclose no root
//   [x] Happy (forgectl#398): `docker build ctx -- --target builder
//       --build-arg K=V` passes those args through to the docker argv,
//       after the derived flags
//   [x] Happy (forgectl#398): derived tag/labels/platform defaults are
//       unchanged when no `--` args are given
//   [x] Unhappy (forgectl#398): two positionals before `--` are still
//       rejected; two positionals with no `--` are still rejected

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerpkg "github.com/cameronsjo/forgectl/internal/docker"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// dockerFixture builds a *docker.Client wired for CLI tests: a temp
// last-tag cache and a FakeRunner whose git plumbing answers a fixed
// repo/branch/sha.
func dockerFixture(t *testing.T) (*dockerpkg.Client, *exec.FakeRunner) {
	t.Helper()
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name != "git" || len(args) < 4 {
				return "", nil
			}
			switch args[3] {
			case "--show-toplevel":
				return "/home/user/myrepo", nil
			case "--abbrev-ref":
				return "main", nil
			case "--short":
				return "abc1234", nil
			}
			return "", nil
		},
	}
	cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
	client := dockerpkg.New(fake,
		dockerpkg.WithLastTagPath(cachePath),
		dockerpkg.WithNow(func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }),
	)
	return client, fake
}

func TestDockerBuildCmd_ReportsDerivedTag(t *testing.T) {
	client, _ := dockerFixture(t)
	cmd := newDockerCmdForClient(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"build"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "built myrepo:main-abc1234\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (git metadata was available)", stderr.String())
	}
}

// TestDockerBuildCmd_IncompleteGitMetadataHasExactOutput covers forgectl#187's
// CLI warning channel plus #226's partial-root naming and non-disclosure.
func TestDockerBuildCmd_IncompleteGitMetadataHasExactOutput(t *testing.T) {
	tests := []struct {
		name       string
		contextDir string
		run        func(string, []string) (string, error)
		wantOut    string
		wantErr    string
		secretPath string
	}{
		{
			name:       "total failure",
			contextDir: "/workspace/sub context",
			run: func(name string, _ []string) (string, error) {
				if name == "git" {
					return "", errors.New("not a git repository")
				}
				return "", nil
			},
			wantOut: "built sub-context:dev\n",
			wantErr: "warning: incomplete git metadata (resolve git repo root: not a git repository); tagged sub-context:dev only\n",
		},
		{
			name:       "partial failure",
			contextDir: "/workspace/sub context",
			secretPath: "/private/build/Repo Root",
			run: func(name string, args []string) (string, error) {
				if name != "git" || len(args) < 4 {
					return "", nil
				}
				switch args[3] {
				case "--show-toplevel":
					return "/private/build/Repo Root\n", nil
				case "--abbrev-ref":
					return "", errors.New("ambiguous HEAD")
				case "--short":
					return "abc1234\n", nil
				}
				return "", nil
			},
			wantOut: "built repo-root:dev\n",
			wantErr: "warning: incomplete git metadata (resolve git branch: ambiguous HEAD); tagged repo-root:dev only\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &exec.FakeRunner{
				RunFunc: tt.run,
			}
			cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
			client := dockerpkg.New(fake,
				dockerpkg.WithLastTagPath(cachePath),
				dockerpkg.WithNow(func() time.Time { return time.Now() }),
			)
			cmd := newDockerCmdForClient(client)
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"build", tt.contextDir})

			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if stdout.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
			if stderr.String() != tt.wantErr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantErr)
			}
			if strings.Count(stderr.String(), "warning:") != 1 {
				t.Errorf("stderr warning count = %d, want 1 (%q)", strings.Count(stderr.String(), "warning:"), stderr.String())
			}
			if tt.secretPath != "" && (strings.Contains(stdout.String(), tt.secretPath) || strings.Contains(stderr.String(), tt.secretPath)) {
				t.Errorf("output disclosed resolved git root %q: stdout=%q stderr=%q", tt.secretPath, stdout.String(), stderr.String())
			}
		})
	}
}

// TestDockerBuildCmd_DashArgs_PassThroughAfterDerivedFlags covers
// forgectl#398: args after `--` reach the docker argv, positioned after
// the derived -t flags (so an explicit user flag can override one).
func TestDockerBuildCmd_DashArgs_PassThroughAfterDerivedFlags(t *testing.T) {
	client, fake := dockerFixture(t)
	cmd := newDockerCmdForClient(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"build", ".", "--", "--target", "builder", "--build-arg", "K=V"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := fake.Last()
	want := []string{
		"build",
		"--label", "org.opencontainers.image.revision=abc1234",
		"--label", "org.opencontainers.image.ref.name=main",
		"--label", "org.opencontainers.image.created=2026-07-09T12:00:00Z",
		"-t", "myrepo:main-abc1234",
		"-t", "myrepo:dev",
		"--target", "builder",
		"--build-arg", "K=V",
		"--", ".",
	}
	if len(call.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q (full args: %v)", i, call.Args[i], w, call.Args)
		}
	}
}

// TestDockerBuildCmd_NoDashArgs_NoPassThroughLeak is a regression guard for
// forgectl#398: with no `--` given, nothing pass-through-shaped leaks into
// the argv (the derived tag/label/platform behavior itself is already
// pinned by TestDockerBuildCmd_ReportsDerivedTag).
func TestDockerBuildCmd_NoDashArgs_NoPassThroughLeak(t *testing.T) {
	client, fake := dockerFixture(t)
	cmd := newDockerCmdForClient(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"build"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := fake.Last()
	for _, a := range call.Args {
		if a == "--target" || a == "--build-arg" {
			t.Errorf("unexpected pass-through arg %q with no -- given: %v", a, call.Args)
		}
	}
}

// TestDockerBuildCmd_TooManyPositionals_StillRejected covers forgectl#398's
// third test requirement: the arg-count guard must still reject two
// positionals, both with and without a `--` dash present.
func TestDockerBuildCmd_TooManyPositionals_StillRejected(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "two positionals, no dash", args: []string{"build", "ctx1", "ctx2"}},
		{name: "two positionals before dash", args: []string{"build", "ctx1", "ctx2", "--", "--target", "builder"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := dockerFixture(t)
			cmd := newDockerCmdForClient(client)
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs(tt.args)

			if err := cmd.ExecuteContext(context.Background()); err == nil {
				t.Fatalf("expected an error for args %v, got nil", tt.args)
			}
		})
	}
}

func TestDockerRunCmd_NoTag_ReusesBuiltTag(t *testing.T) {
	client, fake := dockerFixture(t)
	buildCmd := newDockerCmdForClient(client)
	buildCmd.SetOut(new(bytes.Buffer))
	buildCmd.SetErr(new(bytes.Buffer))
	buildCmd.SetArgs([]string{"build"})
	if err := buildCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("build: unexpected error: %v", err)
	}

	runCmd := newDockerCmdForClient(client)
	runCmd.SetOut(new(bytes.Buffer))
	runCmd.SetErr(new(bytes.Buffer))
	runCmd.SetArgs([]string{"run"})
	if err := runCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("run: unexpected error: %v", err)
	}

	call := fake.Last()
	if call.Name != "docker" || len(call.Args) < 4 || call.Args[3] != "myrepo:main-abc1234" {
		t.Errorf("run did not reuse the built tag, last call: %+v", call)
	}
}

func TestDockerShellCmd_NoTag_ReusesBuiltTag(t *testing.T) {
	client, fake := dockerFixture(t)
	buildCmd := newDockerCmdForClient(client)
	buildCmd.SetOut(new(bytes.Buffer))
	buildCmd.SetErr(new(bytes.Buffer))
	buildCmd.SetArgs([]string{"build"})
	if err := buildCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("build: unexpected error: %v", err)
	}

	shellCmd := newDockerCmdForClient(client)
	shellCmd.SetOut(new(bytes.Buffer))
	shellCmd.SetErr(new(bytes.Buffer))
	shellCmd.SetArgs([]string{"shell"})
	if err := shellCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("shell: unexpected error: %v", err)
	}

	call := fake.Last()
	want := []string{"run", "--rm", "-it", "myrepo:main-abc1234", "sh"}
	if len(call.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q", i, call.Args[i], w)
		}
	}
}

func TestDockerCmd_AliasesResolveToCanonicalVerb(t *testing.T) {
	client, _ := dockerFixture(t)
	cmd := newDockerCmdForClient(client)

	cases := map[string]string{"b": "build", "r": "run", "sh": "shell"}
	for alias, canonical := range cases {
		found, _, err := cmd.Find([]string{alias})
		if err != nil {
			t.Fatalf("Find(%q): %v", alias, err)
		}
		if found.Name() != canonical {
			t.Errorf("alias %q resolved to %q, want %q", alias, found.Name(), canonical)
		}
	}
}
