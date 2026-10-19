//go:build unix

package goal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecValidateRejectsMalformedAndUnsafeControls(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "source")
	valid := goalSpec(repository, "src")
	cases := []struct {
		name   string
		mutate func(*Spec)
		expect string
	}{
		{name: "version", mutate: func(spec *Spec) { spec.Version = 2 }, expect: "goal version must be 1"},
		{name: "title", mutate: func(spec *Spec) { spec.Title = "title\nline" }, expect: "title must be a nonempty single line"},
		{name: "objective", mutate: func(spec *Spec) { spec.Objective = "\x1b[31m" }, expect: "objective must be nonempty UTF-8 text"},
		{name: "repository", mutate: func(spec *Spec) { spec.Repository = "relative" }, expect: "repository must be an absolute clean local path"},
		{name: "revision", mutate: func(spec *Spec) { spec.BaseRevision = "not-a-revision" }, expect: "base_revision must be an exact Git object ID"},
		{name: "model", mutate: func(spec *Spec) { spec.Model = "gpt 5" }, expect: "an exact OpenAI model ID is required"},
		{name: "limit low", mutate: func(spec *Spec) { spec.LimitMinutes = 0 }, expect: "limit_minutes must be between 1 and 30"},
		{name: "limit high", mutate: func(spec *Spec) { spec.LimitMinutes = 31 }, expect: "limit_minutes must be between 1 and 30"},
		{name: "missing scope", mutate: func(spec *Spec) { spec.AllowedPaths = nil }, expect: "provide 1-32 repository-relative allowed paths"},
		{name: "duplicate scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{"src", "src"} }, expect: "invalid or duplicate allowed path"},
		{name: "traversal scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{"../src"} }, expect: "invalid or duplicate allowed path"},
		{name: "absolute scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{"/tmp"} }, expect: "invalid or duplicate allowed path"},
		{name: "glob scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{"src/*.go"} }, expect: "invalid or duplicate allowed path"},
		{name: "protected git scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{".git/config"} }, expect: "invalid or duplicate allowed path"},
		{name: "protected env scope", mutate: func(spec *Spec) { spec.AllowedPaths = []string{".env.local"} }, expect: "invalid or duplicate allowed path"},
		{name: "missing acceptance", mutate: func(spec *Spec) { spec.Acceptance = nil }, expect: "provide 1-16 acceptance criteria"},
		{name: "missing check", mutate: func(spec *Spec) { spec.Checks = nil }, expect: "provide 1-8 explicit verification argument arrays"},
		{name: "control in check", mutate: func(spec *Spec) { spec.Checks = [][]string{{"git", "status\n"}} }, expect: "verification arguments must be bounded UTF-8 without control characters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := valid
			testCase.mutate(&spec)
			requireErrorContains(t, spec.Validate(), testCase.expect)
		})
	}
}

func TestAllowedUsesLiteralPrefixesAndDeniesMetadata(t *testing.T) {
	allowed := []string{"src"}
	for _, path := range []string{"src", "src/new.go", "src/nested/new.go"} {
		if !Allowed(path, allowed) {
			t.Errorf("Allowed(%q, %q) = false, want true", path, allowed)
		}
	}
	if !Allowed(".newfiles", []string{"."}) {
		t.Fatal("Allowed(.newfiles, [.]) = false, want a literal non-reserved hidden path")
	}
	for _, path := range []string{"../src/new.go", ".git/config", ".codex/config", ".agents/config", ".claude/settings.json", ".env", ".env.local", "src/.env"} {
		if Allowed(path, []string{"."}) {
			t.Errorf("Allowed(%q, [.]) = true, want metadata/traversal refusal", path)
		}
	}
	if Allowed("src-old/new.go", allowed) {
		t.Fatal("Allowed treated a literal scope as a string prefix")
	}
}

func TestPrepareReportsDirtySourceAndRefusesChangedBase(t *testing.T) {
	repository := newGoalRepository(t)
	writeGoalFile(t, filepath.Join(repository, "tracked.txt"), "dirty tracked\n")
	writeGoalFile(t, filepath.Join(repository, "untracked.txt"), "dirty untracked\n")
	initialStatus := runFixtureGit(t, repository, "status", "--porcelain=v1", "--untracked-files=all")
	initialBranch := runFixtureGit(t, repository, "symbolic-ref", "--short", "HEAD")
	prepared, err := Prepare(context.Background(), goalSpec(repository, "src"))
	if err != nil {
		t.Fatalf("Prepare dirty source: %v", err)
	}
	if !prepared.SourceDirty {
		t.Fatal("Prepare reported a clean source despite tracked and untracked edits")
	}
	if !containsString(prepared.Warnings, "Source checkout is dirty: only committed HEAD is copied; local edits and untracked files are excluded.") {
		t.Fatalf("dirty warning missing from %q", prepared.Warnings)
	}
	if prepared.Spec.BaseRevision == "" || len(prepared.Spec.BaseRevision) != 40 {
		t.Fatalf("BaseRevision = %q, want the committed 40-character source HEAD", prepared.Spec.BaseRevision)
	}

	writeGoalFile(t, filepath.Join(repository, "committed-after-prepare.txt"), "new commit\n")
	runFixtureGit(t, repository, "add", "--", "committed-after-prepare.txt")
	runFixtureGit(t, repository, "commit", "-q", "-m", "after approval")
	_, err = Prepare(context.Background(), prepared.Spec)
	requireErrorContains(t, err, "source HEAD changed since approval; review the new base before starting")
	if got := runFixtureGit(t, repository, "status", "--porcelain=v1", "--untracked-files=all"); got != initialStatus {
		t.Fatalf("Prepare changed source status from %q to %q", initialStatus, got)
	}
	if got := runFixtureGit(t, repository, "symbolic-ref", "--short", "HEAD"); got != initialBranch {
		t.Fatalf("Prepare changed source branch from %q to %q", initialBranch, got)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if strings.Contains(value, expected) {
			return true
		}
	}
	return false
}

func TestPrepareNativeTeamDisclosesProvidersAndExecutionDependency(t *testing.T) {
	spec := goalSpec(newGoalRepository(t), "src")
	team := spec.EffectiveTeam()
	team.Implementation = ModelSelection{Harness: "pi", Provider: "openai-codex", Model: spec.Model}
	team.Reviewer = ModelSelection{Harness: "claude", Provider: "anthropic", Model: "claude-opus-5-5"}
	spec.Team = &team
	prepared, err := Prepare(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"each approved native model provider", "credential-free execution host", "before model work", "not yet verified end to end", "only owned native process groups", "daemonized children may survive cancellation"} {
		if !containsString(prepared.Warnings, required) {
			t.Errorf("native team warning missing %q: %q", required, prepared.Warnings)
		}
	}
	if containsString(prepared.Warnings, "OpenAI model transport remains enabled") {
		t.Fatalf("Anthropic team retained an OpenAI-only description: %q", prepared.Warnings)
	}
}

func TestPrepareDoesNotMutateARepositoryThroughASymlinkedInput(t *testing.T) {
	repository := newGoalRepository(t)
	link := filepath.Join(t.TempDir(), "repository-link")
	if err := os.Symlink(repository, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	prepared, err := Prepare(context.Background(), goalSpec(link, "src"))
	if err != nil {
		t.Fatalf("Prepare through symlink: %v", err)
	}
	wantRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Spec.Repository != wantRepository {
		t.Fatalf("canonical repository = %q, want %q", prepared.Spec.Repository, wantRepository)
	}
	if got := currentFixtureHead(t, repository); got == "" {
		t.Fatal("repository lost its committed HEAD")
	}
}
