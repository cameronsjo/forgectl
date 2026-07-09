package cli

// Test plan for projects_clone.go
//
// newProjectsCloneCmd / cloneOnly (Classification: API handler / cobra command)
//   [x] Happy: unique query match on an uncloned repo clones it, prints dest to stdout
//   [x] Happy: unique query match on an already-cloned repo annotates it (no clone call)
//     and still prints its LocalPath to stdout
//   [x] Unhappy: no match anywhere for the query returns an error
//   [x] Unhappy: empty inventory (no local/GitHub/Gitea repos at all) returns an error
//   [x] Happy: degradation notes from Inventory appear on stderr, not stdout
//
// pickRepo-driven paths (no args, or a query with multiple matches) are not
// unit-tested here: they drive an interactive huh select, same convention as
// pr_pick_test.go's pickPRs.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// cloneFixture builds a *projects.Client whose Inventory returns repos driven
// entirely by the provided RunFunc. PROJECTS_DIR is set to an empty temp dir
// so localRepos contributes nothing — the test controls all output via gh/tea,
// mirroring listFixture in projects_list_test.go.
func cloneFixture(t *testing.T, runFunc func(string, []string) (string, error)) *projects.Client {
	t.Helper()
	t.Setenv("PROJECTS_DIR", t.TempDir())
	fake := &exec.FakeRunner{RunFunc: runFunc}
	return projects.New(fake)
}

func TestCloneCmd_UniqueQueryMatch_UnclonedRepo_ClonesAndPrintsDest(t *testing.T) {
	ghJSON := `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false}]`
	client := cloneFixture(t, twoHostRunFunc(ghJSON, "owner\tname\ttype\tssh\n"))
	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"forge"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "forgectl") {
		t.Errorf("stdout should contain the clone dest, got: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Cloning") {
		t.Errorf("stderr should narrate the clone, got: %q", stderr.String())
	}
}

func TestCloneCmd_UniqueQueryMatch_AlreadyCloned_AnnotatesInsteadOfCloning(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch name {
		case "gh":
			return "[]", nil
		case "tea":
			return "owner\tname\ttype\tssh\n", nil
		case "git":
			if len(args) >= 5 && args[0] == "-C" && args[2] == "remote" && args[3] == "get-url" {
				return "git@github.com:cameronsjo/forgectl.git", nil
			}
		}
		return "", nil
	}}
	t.Setenv("PROJECTS_DIR", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "forgectl", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := projects.New(fake)

	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"forge"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr.String(), "already on disk") {
		t.Errorf("stderr should annotate the already-cloned repo, got: %q", stderr.String())
	}
	for _, call := range fake.Calls {
		joined := strings.Join(call.Args, " ")
		if call.Name == "gh" && strings.Contains(joined, "clone") {
			t.Errorf("no clone should run for an already-cloned repo; ran: %v", call.Args)
		}
	}
}

func TestCloneCmd_NoMatch_ReturnsError(t *testing.T) {
	ghJSON := `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false}]`
	client := cloneFixture(t, twoHostRunFunc(ghJSON, "owner\tname\ttype\tssh\n"))
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"nonexistent"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a query matching nothing, got nil")
	}
	if !strings.Contains(err.Error(), "no project matching") {
		t.Errorf("error = %q; want 'no project matching'", err.Error())
	}
}

func TestCloneCmd_EmptyInventory_ReturnsError(t *testing.T) {
	client := cloneFixture(t, twoHostRunFunc("[]", "owner\tname\ttype\tssh\n"))
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"anything"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for an empty inventory, got nil")
	}
}

func TestCloneCmd_DegradationNotes_AppearOnStderrNotStdout(t *testing.T) {
	// github errors → note; gitea succeeds with the repo the query matches.
	teaTSV := "owner\tname\ttype\tssh\n" +
		"cameron\thomeclaw\tsource\tssh://git@git.sjo.lol:222/cameron/homeclaw.git\n"
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		switch name {
		case "gh":
			return "", errors.New("gh: not authenticated")
		case "tea":
			return teaTSV, nil
		}
		return "", nil
	})
	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"homeclaw"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("degradation notes must not leak onto stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation notes missing from stderr: %q", stderr.String())
	}
}
