//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/goal"
)

func TestProjectGoalCommandRegistration(t *testing.T) {
	command := newProjectGoalCmd(module.Deps{}, nil)
	for _, name := range []string{"prepare", "start", "list", "status", "respond", "cancel", "artifact", "_supervise", "_host"} {
		if command.CommandPath() == "" {
			t.Fatal("command path is empty")
		}
		if _, _, err := command.Find([]string{name}); err != nil {
			t.Errorf("missing goal subcommand %q: %v", name, err)
		}
	}
}

func TestProjectGoalStartRefusesMissingApprovalBeforeReadingFile(t *testing.T) {
	command := newProjectGoalCmd(module.Deps{}, nil)
	command.SetArgs([]string{"start", "--file", filepath.Join(t.TempDir(), "missing.json")})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--expect-head") || strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("error = %v, output=%q; want approval refusal before file access", err, output.String())
	}
}

func TestProjectGoalAnswerInputIsBoundedAndNoShellParsing(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(strings.NewReader("yes; echo should-not-run\n"))
	answer, err := readProjectGoalAnswer(command, "-")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "yes; echo should-not-run" {
		t.Fatalf("answer = %q; want literal bounded input", answer)
	}
	root := filepath.Join(t.TempDir(), "goal-store")
	store := goal.Store{Root: root}
	if _, err := store.Request(context.Background(), "goal-0123456789abcdef01234567", "respond", "q-1", answer); err == nil || !errors.Is(err, privdir.ErrAbsent) {
		t.Fatalf("request error = %v; want absent private store refusal", err)
	}
}

func TestProjectGoalJSONSpecRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.json")
	data, err := json.Marshal(map[string]any{"version": 1, "unknown": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectGoalSpec(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("readProjectGoalSpec error = %v; want unknown-field refusal", err)
	}
}

type projectGoalReadError struct{}

func (projectGoalReadError) Read(buffer []byte) (int, error) {
	copy(buffer, "partial")
	return len("partial"), errors.New("input failed")
}

func TestProjectGoalAnswerInputPropagatesReadError(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(projectGoalReadError{})
	if _, err := readProjectGoalAnswer(command, "-"); err == nil || !strings.Contains(err.Error(), "input failed") {
		t.Fatalf("readProjectGoalAnswer error = %v; want source read failure", err)
	}
}

func TestProjectGoalJSONSpecRejectsTrailingValue(t *testing.T) {
	spec := goal.Spec{
		Version:      goal.Version,
		Title:        "A goal",
		Objective:    "Do the requested work.",
		Repository:   filepath.Join(t.TempDir(), "repo"),
		AllowedPaths: []string{"."},
		Acceptance:   []string{"The work is complete."},
		Checks:       [][]string{{"git", "diff", "--check"}},
		Model:        "gpt-5-codex",
		LimitMinutes: 10,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "goal.json")
	if err := os.WriteFile(path, append(append(data, '\n'), []byte(`{"version":1}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectGoalSpec(path); err == nil || !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("readProjectGoalSpec error = %v; want trailing-value refusal", err)
	}
}

func TestSafeProjectGoalArtifactPreservesNewlinesAndEscapesControls(t *testing.T) {
	got := safeProjectGoalArtifact("first\nsecond\x1b[31m\n")
	if !strings.Contains(got, "first\nsecond") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("artifact output = %q; want preserved multiline text", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("artifact output contains a raw escape: %q", got)
	}
}
