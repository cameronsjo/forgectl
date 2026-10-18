//go:build unix

package goal

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type goalFixture struct {
	repository string
	root       string
	store      Store
	prepared   Prepared
	state      State
}

func runFixtureGit(t testing.TB, directory string, arguments ...string) string {
	t.Helper()
	commandArguments := append([]string{"--no-pager", "-C", directory}, arguments...)
	command := exec.CommandContext(context.Background(), "/usr/bin/git", commandArguments...) //nolint:gosec // G204: test fixture arguments are locally constructed
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output.String())
	}
	return strings.TrimSpace(output.String())
}

func newGoalRepository(t testing.TB) string {
	t.Helper()
	parent := t.TempDir()
	repository := filepath.Join(parent, "source")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, repository, "init", "-q", "-b", "main")
	runFixtureGit(t, repository, "config", "user.name", "goal fixture")
	runFixtureGit(t, repository, "config", "user.email", "goal-fixture@example.test")
	writeGoalFile(t, filepath.Join(repository, "tracked.txt"), "committed\n")
	runFixtureGit(t, repository, "add", "--", "tracked.txt")
	runFixtureGit(t, repository, "commit", "-q", "-m", "initial")
	return repository
}

func writeGoalFile(t testing.TB, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil { //nolint:gosec // G306: test fixture is private and under t.TempDir
		t.Fatal(err)
	}
}

func goalSpec(repository string, scopes ...string) Spec {
	if len(scopes) == 0 {
		scopes = []string{"src"}
	}
	return Spec{
		Version:      Version,
		Title:        "Fixture goal",
		Objective:    "Exercise the self-service goal boundary.",
		Repository:   repository,
		AllowedPaths: scopes,
		Acceptance:   []string{"the fixture remains isolated"},
		Checks:       [][]string{{"go", "test", "./..."}},
		Model:        "gpt-5",
		LimitMinutes: 5,
	}
}

func newGoalFixture(t testing.TB, scopes ...string) goalFixture {
	t.Helper()
	repository := newGoalRepository(t)
	prepared, err := Prepare(context.Background(), goalSpec(repository, scopes...))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	root := filepath.Join(filepath.Dir(repository), "runtime")
	fixture := goalFixture{repository: repository, root: root, store: Store{Root: root}, prepared: prepared}
	state, err := fixture.store.Create(context.Background(), prepared)
	if err != nil {
		t.Fatalf("Store.Create: %v", err)
	}
	fixture.state = state
	t.Cleanup(func() {
		_ = runFixtureGitOutput(t, repository, "worktree", "remove", "--force", state.Worktree)
		_ = runFixtureGitOutput(t, repository, "branch", "-D", state.Branch)
	})
	return fixture
}

func runFixtureGitOutput(t testing.TB, directory string, arguments ...string) error {
	t.Helper()
	commandArguments := append([]string{"--no-pager", "-C", directory}, arguments...)
	command := exec.CommandContext(context.Background(), "/usr/bin/git", commandArguments...) //nolint:gosec // G204: test fixture arguments are locally constructed
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	return command.Run()
}

func currentFixtureHead(t testing.TB, repository string) string {
	t.Helper()
	return runFixtureGit(t, repository, "rev-parse", "HEAD")
}

func requireErrorContains(t testing.TB, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", expected)
	}
	if !strings.Contains(err.Error(), expected) {
		t.Fatalf("error = %q, want substring %q", err, expected)
	}
}

func requireFileAbsent(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or could not be inspected: %v", path, err)
	}
}
