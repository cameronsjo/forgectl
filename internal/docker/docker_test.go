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
//   [x] Happy (forgectl#398): ExtraArgs land in the argv after the derived
//       -t flags and before `-- <context>`
//   [x] Happy (forgectl#398): a user-supplied --platform in ExtraArgs
//       suppresses the derived --platform (docker's flag is a stringArray;
//       simply appending would stack rather than override)
//   [x] Degraded (forgectl#187): a git resolution failure is non-fatal —
//       docker still runs, tagging only a directory-derived :dev tag with
//       the revision/ref.name labels absent
//   [x] Degraded: --platform and a configured extra label survive the
//       no-repo path
//   [x] Degraded: a build that degraded to a dev tag still caches it
//   [x] Degraded: partial git resolution retains a sanitized root-derived
//       :dev identity while immutable tags and git labels stay all-or-nothing
//   [x] Degraded: blank successful probe output is a field-specific failure
//       and the first failure remains stable while all probes still run
//   [x] Degraded: platform/created/configured labels survive the partial path
//   [x] Collision: lossy root sanitization and the global cache are explicitly
//       last-successful-build-wins; a failed later build preserves the cache
//   [x] Logging: total/partial degradation emits one structured warning with
//       stable fields and no resolved root; complete metadata emits none
//
// Client.Run / Client.Shell (Classification: ops layer)
//   [x] Happy: an explicit --tag is used as given
//   [x] Happy: independent Run and Shell clients reuse a partial build's
//       cached stable dev tag
//   [x] Happy: Shell defaults to "sh" when --shell is omitted
//   [x] Unhappy: no explicit tag and no cache yields an error, no Runner call
//   [x] Unhappy: an option-like explicit tag is rejected before any Runner call
//   [x] Unhappy: an option-like --shell value is rejected before any Runner call

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

type gitProbe struct {
	output string
	err    error
}

func fakeGitProbeRunner(top, branch, sha gitProbe) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name != "git" || len(args) < 4 {
				return "", nil
			}
			switch args[3] {
			case "--show-toplevel":
				return top.output, top.err
			case "--abbrev-ref":
				return branch.output, branch.err
			case "--short":
				return sha.output, sha.err
			}
			return "", nil
		},
	}
}

// fakeGitRunner returns a FakeRunner whose Run (git plumbing) answers the
// three `git rev-parse` calls gitInfo makes, keyed on the flag at args[3]
// (["-C", dir, "rev-parse", <flag>, ...]) so the fixture doesn't care what
// dir Build was called with.
func fakeGitRunner(toplevel, branch, sha string) *exec.FakeRunner {
	return fakeGitProbeRunner(
		gitProbe{output: toplevel},
		gitProbe{output: branch},
		gitProbe{output: sha},
	)
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

	result, err := c.Build(context.Background(), BuildOptions{ContextDir: "."})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tag := result.Tag
	if tag != "myrepo:feature-foo-abc1234" {
		t.Errorf("Build tag = %q, want %q", tag, "myrepo:feature-foo-abc1234")
	}
	if !result.GitMetadata {
		t.Errorf("GitMetadata = false, want true (git resolution succeeded)")
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

// TestBuild_ExtraArgs_AppearAfterDerivedFlagsBeforeDash covers forgectl#398:
// user-supplied pass-through flags land after the derived -t/-t pair and
// before the `-- <context>` guard.
func TestBuild_ExtraArgs_AppearAfterDerivedFlagsBeforeDash(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{DefaultPlatform: "linux/amd64"}))

	_, err := c.Build(context.Background(), BuildOptions{
		ContextDir: ".",
		Platform:   "",
		ExtraArgs:  []string{"--target", "builder", "--build-arg", "K=V"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	call := fake.Last()
	want := []string{
		"build",
		"--platform", "linux/amd64",
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

// TestBuild_ExtraArgsPlatform_SuppressesDerivedPlatform covers forgectl#398's
// override guarantee for --platform specifically: docker's --platform is a
// stringArray flag (repeating it appends, producing a multi-platform build
// request), so simply appending ExtraArgs after the derived --platform
// would silently stack the two rather than let the user's value win. Build
// must suppress its own derived --platform when ExtraArgs supplies one.
func TestBuild_ExtraArgsPlatform_SuppressesDerivedPlatform(t *testing.T) {
	fake := fakeGitRunner("/home/user/myrepo", "main", "abc1234")
	c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{DefaultPlatform: "linux/amd64"}))

	_, err := c.Build(context.Background(), BuildOptions{
		ContextDir: ".",
		ExtraArgs:  []string{"--platform", "linux/arm64"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	call := fake.Last()
	count := 0
	for _, a := range call.Args {
		if a == "--platform" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("--platform appeared %d times, want exactly 1 (the user's), argv: %v", count, call.Args)
	}
	want := []string{
		"build",
		"--label", "org.opencontainers.image.revision=abc1234",
		"--label", "org.opencontainers.image.ref.name=main",
		"--label", "org.opencontainers.image.created=2026-07-09T12:00:00Z",
		"-t", "myrepo:main-abc1234",
		"-t", "myrepo:dev",
		"--platform", "linux/arm64",
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

	result, err := c.Build(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, ok := c.LastTag()
	if !ok {
		t.Fatal("expected LastTag to be populated after a successful build")
	}
	if got != result.Tag {
		t.Errorf("LastTag = %q, want %q", got, result.Tag)
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

// TestBuild_GitFailure_DegradesToDirDerivedDevTag inverts the pre-fix
// contract this test used to assert (git failure == hard error, docker
// never runs). forgectl#187: git metadata is an optional enrichment, not a
// precondition — a git failure now degrades the build to a
// directory-derived :dev tag with no revision/ref.name labels, and docker
// still runs.
func TestBuild_GitFailure_DegradesToDirDerivedDevTag(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			return "", errors.New("not a git repository")
		},
	}
	c := newTestClient(t, fake)

	dir := t.TempDir()
	want := devTag(slugifyRepo(filepath.Base(dir)))

	result, err := c.Build(context.Background(), BuildOptions{ContextDir: dir})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.GitMetadata {
		t.Error("GitMetadata = true, want false (git resolution failed)")
	}
	if result.GitReason == "" {
		t.Error("GitReason = empty, want the git resolution error")
	}
	if result.Tag != want || result.DevTag != want {
		t.Errorf("Tag/DevTag = %q/%q, want %q (directory-derived)", result.Tag, result.DevTag, want)
	}

	call := fake.Last()
	if call.Name != "docker" {
		t.Fatalf("expected docker build to still run despite git failure, last call: %+v", call)
	}
	var tagFlags int
	for i, a := range call.Args {
		if a != "-t" {
			continue
		}
		tagFlags++
		if i+1 >= len(call.Args) || call.Args[i+1] != want {
			t.Errorf("-t value = %v, want %q", call.Args[i+1:], want)
		}
	}
	if tagFlags != 1 {
		t.Errorf("expected exactly one -t flag (dev tag only, no derived tag), got %d in args: %v", tagFlags, call.Args)
	}
	argv := strings.Join(call.Args, " ")
	if strings.Contains(argv, "org.opencontainers.image.revision") {
		t.Errorf("revision label must be absent when git metadata is unavailable, args: %v", call.Args)
	}
	if strings.Contains(argv, "org.opencontainers.image.ref.name") {
		t.Errorf("ref.name label must be absent when git metadata is unavailable, args: %v", call.Args)
	}
}

// TestBuild_GitFieldCompletenessAndStableNaming covers the partial and blank
// probe shapes that total Git failure cannot: a valid root remains useful
// naming identity even when branch/SHA cannot support immutable provenance.
func TestBuild_GitFieldCompletenessAndStableNaming(t *testing.T) {
	probeErr := errors.New("probe failed")
	tests := []struct {
		name       string
		top        gitProbe
		branch     gitProbe
		sha        gitProbe
		wantTag    string
		wantReason string
		complete   bool
	}{
		{name: "complete", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{output: "main\n"}, sha: gitProbe{output: "abc1234\n"}, wantTag: "repo-root:main-abc1234", complete: true},
		{name: "branch error", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{err: errors.New("ambiguous HEAD")}, sha: gitProbe{output: "abc1234\n"}, wantTag: "repo-root:dev", wantReason: "resolve git branch: ambiguous HEAD"},
		{name: "sha error", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{output: "main\n"}, sha: gitProbe{err: errors.New("missing HEAD")}, wantTag: "repo-root:dev", wantReason: "resolve git sha: missing HEAD"},
		{name: "both revision errors preserve first", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{err: errors.New("ambiguous HEAD")}, sha: gitProbe{err: errors.New("missing HEAD")}, wantTag: "repo-root:dev", wantReason: "resolve git branch: ambiguous HEAD"},
		{name: "blank root", top: gitProbe{output: " \n\t"}, branch: gitProbe{output: "main\n"}, sha: gitProbe{output: "abc1234\n"}, wantTag: "sub-context:dev", wantReason: "resolve git repo root: empty git repo root"},
		{name: "blank branch", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{output: " \n"}, sha: gitProbe{output: "abc1234\n"}, wantTag: "repo-root:dev", wantReason: "resolve git branch: empty git branch"},
		{name: "blank sha", top: gitProbe{output: "/workspace/Repo Root\n"}, branch: gitProbe{output: "main\n"}, sha: gitProbe{output: "\t\n"}, wantTag: "repo-root:dev", wantReason: "resolve git sha: empty git sha"},
		{name: "root error", top: gitProbe{err: probeErr}, branch: gitProbe{output: "main\n"}, sha: gitProbe{output: "abc1234\n"}, wantTag: "sub-context:dev", wantReason: "resolve git repo root: probe failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeGitProbeRunner(tt.top, tt.branch, tt.sha)
			c := newTestClient(t, fake)
			contextDir := filepath.Join(t.TempDir(), "sub context")

			result, err := c.Build(context.Background(), BuildOptions{ContextDir: contextDir})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if result.Tag != tt.wantTag {
				t.Errorf("Tag = %q, want %q", result.Tag, tt.wantTag)
			}
			wantDev := tt.wantTag
			if tt.complete {
				wantDev = "repo-root:dev"
			}
			if result.DevTag != wantDev {
				t.Errorf("DevTag = %q, want %q", result.DevTag, wantDev)
			}
			if result.GitMetadata != tt.complete {
				t.Errorf("GitMetadata = %v, want %v", result.GitMetadata, tt.complete)
			}
			if result.GitReason != tt.wantReason {
				t.Errorf("GitReason = %q, want %q", result.GitReason, tt.wantReason)
			}
			if got := len(fake.Calls); got != 4 {
				t.Errorf("Runner calls = %d, want three git probes plus docker build", got)
			}

			argv := strings.Join(fake.Last().Args, " ")
			if strings.Contains(argv, "/workspace/Repo Root") || strings.Contains(argv, "Repo Root") {
				t.Errorf("docker argv disclosed unsanitized root: %v", fake.Last().Args)
			}
			wantTagFlags := 1
			if tt.complete {
				wantTagFlags = 2
			}
			if got := countArg(fake.Last().Args, "-t"); got != wantTagFlags {
				t.Errorf("-t count = %d, want %d (args: %v)", got, wantTagFlags, fake.Last().Args)
			}
			if got := strings.Contains(argv, "org.opencontainers.image.revision="); got != tt.complete {
				t.Errorf("revision label present = %v, want %v (args: %v)", got, tt.complete, fake.Last().Args)
			}
			if got := strings.Contains(argv, "org.opencontainers.image.ref.name="); got != tt.complete {
				t.Errorf("ref.name label present = %v, want %v (args: %v)", got, tt.complete, fake.Last().Args)
			}
		})
	}
}

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want {
			count++
		}
	}
	return count
}

func TestBuild_PartialGitResolutionPreservesCommonArgv(t *testing.T) {
	for _, tt := range []struct {
		name         string
		override     string
		wantPlatform string
	}{
		{name: "configured default", wantPlatform: "linux/amd64"},
		{name: "explicit override", override: "linux/arm64", wantPlatform: "linux/arm64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeGitProbeRunner(
				gitProbe{output: "/workspace/Repo Root\n"},
				gitProbe{err: errors.New("ambiguous HEAD")},
				gitProbe{output: "abc1234\n"},
			)
			c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{
				DefaultPlatform: "linux/amd64",
				LabelTemplate:   "org.example.team=platform",
			}))
			contextDir := filepath.Join(t.TempDir(), "sub context")

			if _, err := c.Build(context.Background(), BuildOptions{ContextDir: contextDir, Platform: tt.override}); err != nil {
				t.Fatalf("Build: %v", err)
			}
			args := fake.Last().Args
			if got := countArg(args, "--platform"); got != 1 {
				t.Errorf("--platform count = %d, want 1 (args: %v)", got, args)
			}
			for _, pair := range [][2]string{
				{"--platform", tt.wantPlatform},
				{"--label", "org.opencontainers.image.created=2026-07-09T12:00:00Z"},
				{"--label", "org.example.team=platform"},
				{"-t", "repo-root:dev"},
			} {
				if got := countArgPair(args, pair[0], pair[1]); got != 1 {
					t.Errorf("argv pair %q %q count = %d, want 1 (args: %v)", pair[0], pair[1], got, args)
				}
			}
			if countArg(args, "-t") != 1 {
				t.Errorf("partial build must apply one tag, args: %v", args)
			}
			if got := args[len(args)-2:]; got[0] != "--" || got[1] != contextDir {
				t.Errorf("argv suffix = %v, want [-- %s]", got, contextDir)
			}
		})
	}
}

func countArgPair(args []string, flag, value string) int {
	count := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			count++
		}
	}
	return count
}

// TestBuild_GitFailure_PlatformAndExtraLabelSurvive proves the no-repo path
// still carries everything that needs no git data: --platform and any
// configured extra label.
func TestBuild_GitFailure_PlatformAndExtraLabelSurvive(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			return "", errors.New("not a git repository")
		},
	}
	c := newTestClient(t, fake, WithDockerConfig(config.DockerConfig{
		DefaultPlatform: "linux/amd64",
		LabelTemplate:   "org.example.team=platform",
	}))

	if _, err := c.Build(context.Background(), BuildOptions{ContextDir: t.TempDir()}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	argv := strings.Join(fake.Last().Args, " ")
	if !strings.Contains(argv, "--platform linux/amd64") {
		t.Errorf("--platform must survive the no-repo path, got args: %v", fake.Last().Args)
	}
	if !strings.Contains(argv, "org.example.team=platform") {
		t.Errorf("configured extra label must survive the no-repo path, got args: %v", fake.Last().Args)
	}
}

// TestBuild_GitFailure_CachesDevTag proves the overruling decision from
// forgectl#187: rather than skipping the cache write (the issue's proposed
// fix), Build caches the dev tag it actually applied, so run/shell can
// still find it.
func TestBuild_GitFailure_CachesDevTag(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			return "", errors.New("not a git repository")
		},
	}
	c := newTestClient(t, fake)

	result, err := c.Build(context.Background(), BuildOptions{ContextDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, ok := c.LastTag()
	if !ok {
		t.Fatal("expected LastTag to be populated after a build that degraded to a dev tag")
	}
	if got != result.DevTag {
		t.Errorf("LastTag = %q, want cached dev tag %q", got, result.DevTag)
	}
}

func TestBuild_PartialGitCacheFeedsRunAndShellIndependently(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
	buildFake := fakeGitProbeRunner(
		gitProbe{output: "/workspace/Repo Root\n"},
		gitProbe{err: errors.New("ambiguous HEAD")},
		gitProbe{output: "abc1234\n"},
	)
	buildClient := New(buildFake,
		WithLastTagPath(cachePath),
		WithNow(func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }),
	)
	result, err := buildClient.Build(context.Background(), BuildOptions{ContextDir: "/workspace/sub context"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Tag != "repo-root:dev" {
		t.Fatalf("partial build tag = %q, want repo-root:dev", result.Tag)
	}

	runFake := &exec.FakeRunner{}
	runClient := New(runFake, WithLastTagPath(cachePath))
	if err := runClient.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := runFake.Last().Args, []string{"run", "--rm", "-it", "repo-root:dev"}; !equalStrings(got, want) {
		t.Errorf("Run argv = %v, want %v", got, want)
	}

	shellFake := &exec.FakeRunner{}
	shellClient := New(shellFake, WithLastTagPath(cachePath))
	if err := shellClient.Shell(context.Background(), ShellOptions{}); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if got, want := shellFake.Last().Args, []string{"run", "--rm", "-it", "repo-root:dev", "sh"}; !equalStrings(got, want) {
		t.Errorf("Shell argv = %v, want %v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestBuild_RepositorySanitizerCollisionIsLastSuccessfulBuildWins(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "docker-lasttag")
	build := func(t *testing.T, root, sha string, interactiveErr error) BuildResult {
		t.Helper()
		fake := fakeGitProbeRunner(
			gitProbe{output: root + "\n"},
			gitProbe{output: "main\n"},
			gitProbe{output: sha + "\n"},
		)
		fake.InteractiveErr = interactiveErr
		client := New(fake,
			WithLastTagPath(cachePath),
			WithNow(func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }),
		)
		result, err := client.Build(context.Background(), BuildOptions{ContextDir: "/workspace/context"})
		if interactiveErr == nil && err != nil {
			t.Fatalf("Build(%q): %v", root, err)
		}
		if interactiveErr != nil {
			if err == nil {
				t.Fatalf("Build(%q) succeeded, want docker failure", root)
			}
			if got, want := err.Error(), "docker build: "+interactiveErr.Error(); got != want {
				t.Fatalf("Build(%q) error = %q, want %q", root, got, want)
			}
		}
		argv := strings.Join(fake.Last().Args, " ")
		if strings.Contains(argv, root) {
			t.Errorf("docker argv disclosed unsanitized source root %q: %v", root, fake.Last().Args)
		}
		base := filepath.Base(root)
		if base != slugifyRepo(base) && strings.Contains(argv, base) {
			t.Errorf("docker argv disclosed unsanitized root basename %q: %v", base, fake.Last().Args)
		}
		return result
	}

	first := build(t, "/private/repos/A B", "1111111", nil)
	second := build(t, "/other/repos/a-b", "2222222", nil)
	if first.DevTag != "a-b:dev" || second.DevTag != "a-b:dev" {
		t.Fatalf("colliding dev tags = %q and %q, want a-b:dev", first.DevTag, second.DevTag)
	}
	if first.Tag == second.Tag {
		t.Fatalf("immutable tags collided: %q", first.Tag)
	}
	reader := New(&exec.FakeRunner{}, WithLastTagPath(cachePath))
	if got, ok := reader.LastTag(); !ok || got != second.Tag {
		t.Fatalf("LastTag after second success = %q, %v; want %q, true", got, ok, second.Tag)
	}

	build(t, "/third/repos/A B", "3333333", errors.New("build failed"))
	if got, ok := reader.LastTag(); !ok || got != second.Tag {
		t.Fatalf("LastTag after failed third build = %q, %v; want prior %q, true", got, ok, second.Tag)
	}
}

func TestBuild_StructuredWarningMatchesMetadataCompleteness(t *testing.T) {
	resolvedRoot := "/private/build/Repo Root"
	tests := []struct {
		name        string
		top         gitProbe
		branch      gitProbe
		sha         gitProbe
		wantWarning bool
		wantFields  []string
	}{
		{
			name:        "partial",
			top:         gitProbe{output: resolvedRoot + "\n"},
			branch:      gitProbe{err: errors.New("ambiguous HEAD")},
			sha:         gitProbe{output: "abc1234\n"},
			wantWarning: true,
			wantFields:  []string{"tag=repo-root:dev", `error="resolve git branch: ambiguous HEAD"`, `context="sub context"`},
		},
		{
			name:        "total",
			top:         gitProbe{err: errors.New("not a git repository")},
			branch:      gitProbe{err: errors.New("not a git repository")},
			sha:         gitProbe{err: errors.New("not a git repository")},
			wantWarning: true,
			wantFields:  []string{"tag=sub-context:dev", `error="resolve git repo root: not a git repository"`, `context="sub context"`},
		},
		{
			name:   "complete",
			top:    gitProbe{output: resolvedRoot + "\n"},
			branch: gitProbe{output: "main\n"},
			sha:    gitProbe{output: "abc1234\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			fake := fakeGitProbeRunner(tt.top, tt.branch, tt.sha)
			c := newTestClient(t, fake)
			if _, err := c.Build(context.Background(), BuildOptions{ContextDir: "sub context"}); err != nil {
				t.Fatalf("Build: %v", err)
			}

			got := logs.String()
			message := "Incomplete git metadata for docker build; using dev tag only."
			wantCount := 0
			if tt.wantWarning {
				wantCount = 1
			}
			if count := strings.Count(got, message); count != wantCount {
				t.Fatalf("structured warning count = %d, want %d; logs=%q", count, wantCount, got)
			}
			for _, want := range tt.wantFields {
				if !strings.Contains(got, want) {
					t.Errorf("structured warning missing %q: %q", want, got)
				}
			}
			if strings.Contains(got, resolvedRoot) {
				t.Errorf("structured warning disclosed resolved root %q: %q", resolvedRoot, got)
			}
		})
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

	result, err := c.Build(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if err := c.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := fake.Last()
	if call.Args[3] != result.Tag {
		t.Errorf("Run reused tag = %q, want cached %q (args: %v)", call.Args[3], result.Tag, call.Args)
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
