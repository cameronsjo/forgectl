package docker

// Test plan for docker.go
//
// Client.Build (Classification: ops layer)
//   [x] Happy: derives repo/branch/sha via git, issues `docker build` with
//       --platform, both --label flags (built-in OCI labels), -t <derived>,
//       -t <repo>:dev, and `-- <context>` in the right argv positions
//   [x] Happy: an unconfigured platform omits the --platform flag entirely
//   [x] Happy: a configured extra label (WithDockerConfig) is appended
//   [x] Happy: a successful build caches the derived tag (LastTag reflects it)
//   [x] Unhappy: an option-like context dir is rejected before any Runner call
//   [x] Unhappy: a git resolution failure surfaces as an error, no docker call
//
// Client.Run / Client.Shell (Classification: ops layer)
//   [x] Happy: an explicit --tag is used as given
//   [x] Happy: an omitted tag reuses the cached last-built tag
//   [x] Happy: Shell defaults to "sh" when --shell is omitted
//   [x] Unhappy: no explicit tag and no cache yields an error, no Runner call
//   [x] Unhappy: an option-like explicit tag is rejected before any Runner call
//   [x] Unhappy: an option-like --shell value is rejected before any Runner call

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// fakeGitRunner returns a FakeRunner whose Run (git plumbing) answers the
// three `git rev-parse` calls gitInfo makes, keyed on the flag at args[3]
// (["-C", dir, "rev-parse", <flag>, ...]) so the fixture doesn't care what
// dir Build was called with.
func fakeGitRunner(toplevel, branch, sha string) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name != "git" || len(args) < 4 {
				return "", nil
			}
			switch args[3] {
			case "--show-toplevel":
				return toplevel, nil
			case "--abbrev-ref":
				return branch, nil
			case "--short":
				return sha, nil
			}
			return "", nil
		},
	}
}

func newTestClient(t *testing.T, run exec.Runner, opts ...Option) *Client {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
	base := []Option{
		WithLastTagPath(cachePath),
		WithNow(func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }),
	}
	return New(run, append(base, opts...)...)
}

func TestBuild_DerivesTagAndIssuesFullArgv(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "feature/foo", "abc1234")
	c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{DefaultPlatform: "linux/amd64"}))

	tag, err := c.Build(context.Background(), BuildOptions{ContextDir: "."})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tag != "myrepo:feature-foo-abc1234" {
		t.Errorf("Build tag = %q, want %q", tag, "myrepo:feature-foo-abc1234")
	}

	call := fake.Last()
	if call.Name != "docker" {
		t.Fatalf("last call = %q, want docker", call.Name)
	}
	if !call.Interactive {
		t.Errorf("docker build call must go through RunInteractive")
	}
	want := []string{
		"build",
		"--platform", "linux/amd64",
		"--label", "org.opencontainers.image.revision=abc1234",
		"--label", "org.opencontainers.image.ref.name=feature-foo",
		"--label", "org.opencontainers.image.created=2026-07-09T12:00:00Z",
		"-t", "myrepo:feature-foo-abc1234",
		"-t", "myrepo:dev",
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

func TestBuild_NoConfiguredPlatform_OmitsFlag(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake)

	if _, err := c.Build(context.Background(), BuildOptions{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	call := fake.Last()
	for _, a := range call.Args {
		if a == "--platform" {
			t.Errorf("--platform must be omitted when unconfigured, got args: %v", call.Args)
		}
	}
}

func TestBuild_ConfiguredExtraLabel_IsAppended(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{LabelTemplate: "org.example.team=platform"}))

	if _, err := c.Build(context.Background(), BuildOptions{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	call := fake.Last()
	found := false
	for i, a := range call.Args {
		if a == "--label" && i+1 < len(call.Args) && call.Args[i+1] == "org.example.team=platform" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected configured extra label in argv, got: %v", call.Args)
	}
}

func TestBuild_Success_CachesLastTag(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake)

	tag, err := c.Build(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, ok := c.LastTag()
	if !ok {
		t.Fatal("expected LastTag to be populated after a successful build")
	}
	if got != tag {
		t.Errorf("LastTag = %q, want %q", got, tag)
	}
}

func TestBuild_RejectsOptionLikeContextDir(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake)

	if _, err := c.Build(context.Background(), BuildOptions{ContextDir: "--upload-pack=touch /tmp/pwned"}); err == nil {
		t.Fatal("expected rejection of an option-like context dir")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected for a rejected context dir, got: %+v", fake.Calls)
	}
}

func TestBuild_GitFailure_SurfacesErrorWithoutDockerCall(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			return "", errors.New("not a git repository")
		},
	}
	c := newTestClient(t, fake)

	if _, err := c.Build(context.Background(), BuildOptions{}); err == nil {
		t.Fatal("expected an error when git resolution fails")
	}
	for _, call := range fake.Calls {
		if call.Name == "docker" {
			t.Errorf("docker must not run when git resolution fails, got: %+v", fake.Calls)
		}
	}
}

func TestRun_ExplicitTag_IsUsed(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Run(context.Background(), RunOptions{Tag: "myrepo:dev", Args: []string{"echo", "hi"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	call := fake.Last()
	want := []string{"run", "--rm", "-it", "myrepo:dev", "echo", "hi"}
	if len(call.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q", i, call.Args[i], w)
		}
	}
}

func TestRun_OmittedTag_ReusesCachedTag(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
	c := New(fake, WithLastTagPath(cachePath), WithNow(func() time.Time { return time.Now() }))

	tag, err := c.Build(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if err := c.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := fake.Last()
	if call.Args[3] != tag {
		t.Errorf("Run reused tag = %q, want cached %q (args: %v)", call.Args[3], tag, call.Args)
	}
}

func TestRun_NoTagNoCache_ReturnsErrorWithoutRunnerCall(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Run(context.Background(), RunOptions{}); err == nil {
		t.Fatal("expected an error when no tag and no cache are available")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected, got: %+v", fake.Calls)
	}
}

func TestRun_RejectsOptionLikeTag(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Run(context.Background(), RunOptions{Tag: "--privileged"}); err == nil {
		t.Fatal("expected rejection of an option-like tag")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected for a rejected tag, got: %+v", fake.Calls)
	}
}

func TestShell_DefaultsToSh(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Shell(context.Background(), ShellOptions{Tag: "myrepo:dev"}); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	call := fake.Last()
	want := []string{"run", "--rm", "-it", "myrepo:dev", "sh"}
	if len(call.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q", i, call.Args[i], w)
		}
	}
}

func TestShell_CustomShell(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Shell(context.Background(), ShellOptions{Tag: "myrepo:dev", Shell: "bash"}); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	call := fake.Last()
	if call.Args[len(call.Args)-1] != "bash" {
		t.Errorf("Shell command = %q, want bash (args: %v)", call.Args[len(call.Args)-1], call.Args)
	}
}

func TestShell_RejectsOptionLikeShellValue(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := newTestClient(t, fake)

	if err := c.Shell(context.Background(), ShellOptions{Tag: "myrepo:dev", Shell: "--privileged"}); err == nil {
		t.Fatal("expected rejection of an option-like --shell value")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected for a rejected shell value, got: %+v", fake.Calls)
	}
}
