package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

func TestChooseRepo_HeadlessWritesCandidatesThenReturnsModeSpecificExit(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickRepoFn
	isInteractiveTTY = func() bool { return interactiveTTY(false, true) }
	pickerCalls := 0
	pickRepoFn = func([]projects.Repo) (projects.Repo, error) {
		pickerCalls++
		return projects.Repo{}, errors.New("picker reached")
	}
	t.Cleanup(func() { isInteractiveTTY, pickRepoFn = prevTTY, prevPicker })

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	repos := []projects.Repo{
		{Host: "github", Owner: "cameronsjo", Name: "forgectl", Cloned: false},
		{Host: "", LocalPath: "/work/local", Cloned: true, Status: projects.GitStatus{State: projects.StatusOK}},
	}
	_, err := chooseRepo(cmd, repos, projectSelectionClone)
	if got, want := stdout.String(), "github  cameronsjo/forgectl  uncloned\nlocal  path:/work/local  clean\n"; got != want {
		t.Errorf("candidate stdout = %q, want %q", got, want)
	}
	if got, want := err.Error(), "2 projects require a clone selection, and there is no interactive terminal — get the candidate's sshUrl from `forgectl projects list --json` and pass that URL; owner/repo is exact only for GitHub candidates, and any candidate without an sshUrl (including local-only) requires an interactive rerun; candidates are on stdout"; err == nil || got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode = %d, want 1", code)
	}
	if pickerCalls != 0 {
		t.Errorf("picker calls = %d, want 0", pickerCalls)
	}
}

func TestChooseRepo_HeadlessPreservesFirstWriterError(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickRepoFn
	isInteractiveTTY = func() bool { return interactiveTTY(true, false) }
	pickerCalls := 0
	pickRepoFn = func([]projects.Repo) (projects.Repo, error) { pickerCalls++; return projects.Repo{}, nil }
	t.Cleanup(func() { isInteractiveTTY, pickRepoFn = prevTTY, prevPicker })

	sentinel := errors.New("writer failed")
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{err: sentinel})
	_, err := chooseRepo(cmd, []projects.Repo{{Host: "github", Owner: "c", Name: "one"}}, projectSelectionWorktree)
	if !errors.Is(err, sentinel) || err != sentinel {
		t.Errorf("error = %v, want original sentinel", err)
	}
	if pickerCalls != 0 {
		t.Errorf("picker calls = %d, want 0", pickerCalls)
	}
}

func TestChooseRepo_InteractiveCallsPickerOnceWithoutCandidateOutput(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickRepoFn
	isInteractiveTTY = func() bool { return interactiveTTY(true, true) }
	want := projects.Repo{Host: "github", Owner: "c", Name: "selected"}
	pickerCalls := 0
	pickRepoFn = func([]projects.Repo) (projects.Repo, error) { pickerCalls++; return want, nil }
	t.Cleanup(func() { isInteractiveTTY, pickRepoFn = prevTTY, prevPicker })

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	got, err := chooseRepo(cmd, []projects.Repo{{Host: "github", Owner: "c", Name: "other"}}, projectSelectionPick)
	if err != nil || got != want {
		t.Errorf("chooseRepo = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if pickerCalls != 1 {
		t.Errorf("picker calls = %d, want 1", pickerCalls)
	}
	if stdout.Len() != 0 {
		t.Errorf("candidate stdout = %q, want empty", stdout.String())
	}
}

func TestProjectCandidateLine_SanitizesAndMarksMirror(t *testing.T) {
	got := projectCandidateLine(projects.Repo{Host: "git\x1b[31m", Owner: "cam\n", Name: "forge\x7f", Cloned: true, Mirror: true, Status: projects.GitStatus{State: projects.StatusOK, Modified: 2, Untracked: 1}})
	if strings.ContainsAny(got, "\x1b\n\x7f") {
		t.Errorf("candidate contains terminal control bytes: %q", got)
	}
	if want := "2 modified, 1 untracked, mirror"; !strings.Contains(got, want) {
		t.Errorf("candidate = %q, want %q", got, want)
	}
}

func TestProjectAmbiguityError_HasCommandSpecificRecovery(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode projectSelectionMode
		want string
	}{
		{"pick", projectSelectionPick, "2 projects require a selection, and there is no interactive terminal — narrow to a unique project name when possible, or rerun interactively; inspect identities with `forgectl projects list --json`; candidates are on stdout"},
		{"clone", projectSelectionClone, "2 projects require a clone selection, and there is no interactive terminal — get the candidate's sshUrl from `forgectl projects list --json` and pass that URL; owner/repo is exact only for GitHub candidates, and any candidate without an sshUrl (including local-only) requires an interactive rerun; candidates are on stdout"},
		{"worktree", projectSelectionWorktree, "2 projects require a worktree selection, and there is no interactive terminal — get the candidate's sshUrl from `forgectl projects list --json` and pass that URL; owner/repo is exact only for GitHub candidates, and any candidate without an sshUrl (including local-only) requires an interactive rerun; candidates are on stdout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectAmbiguityError(tt.mode, 2).Error(); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

var _ io.Writer = failingWriter{}
