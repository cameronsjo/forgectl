package cli

// Test plan for docker.go
//
// newDockerCmd / newDockerCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: `docker build [context]` reports the derived tag on stdout
//   [x] Happy: `docker run` with no --tag reuses the tag from a prior build
//   [x] Happy: `docker shell` with no --tag reuses the tag from a prior build
//   [x] Happy: subcommand aliases (b/r/sh) resolve to their canonical verb

import (
	"bytes"
	"context"
	"path/filepath"
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
		dockerpkg.WithNow(func() time.Time { return time.Now() }),
	)
	return client, fake
}

func TestDockerBuildCmd_ReportsDerivedTag(t *testing.T) {
	client, _ := dockerFixture(t)
	cmd := newDockerCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"build"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "built myrepo:main-abc1234\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
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
