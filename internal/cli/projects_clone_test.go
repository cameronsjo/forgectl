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
//   [x] Happy: an owner/repo-shaped arg clones directly, bypassing Inventory
//   [x] Happy: --org bulk-clones every repo the org listing returns
//   [x] Unhappy: --org combined with a query argument is rejected
//   [x] Unhappy: --org listing failure propagates
//   [x] Unhappy: --org with no repos returns an error
//   [x] Invariant: a hostile degradation note is escaped on stderr — the
//       non-`list` anchor proving renderDegradationNotes is applied here too
//
// The interactive huh form remains unexecuted in unit tests. Its chooser
// boundary is covered in projects_pick_test.go, including headless candidate
// output, first writer-error preservation, and the live-TTY picker seam.

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
//   [x] Happy: --dry-run prints the host-tree destination and touches nothing
//   [x] Happy: --dry-run --wing prints the wing destination, name normalized
//   [x] Unhappy: --wing with --org is refused
//   [x] Unhappy: --wing naming the configured GitHub host is refused
//   [x] Unhappy: --wing outside the path-segment charset is refused

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

func TestCloneCmd_OwnerRepoArg_ClonesDirectlyBypassingInventory(t *testing.T) {
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "clone" {
			return "", nil
		}
		t.Fatalf("unexpected call bypassing direct clone: %s %v", name, args)
		return "", nil
	})
	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"anthropics/claude-code"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "anthropics") || !strings.Contains(stdout.String(), "claude-code") {
		t.Errorf("stdout should contain the clone dest, got: %q", stdout.String())
	}
}

func TestCloneCmd_Org_BulkClonesEveryListedRepo(t *testing.T) {
	orgJSON := `[{"name":"repo-a","sshUrl":"git@github.com:anthropics/repo-a.git","isPrivate":false},` +
		`{"name":"repo-b","sshUrl":"git@github.com:anthropics/repo-b.git","isPrivate":false}]`
	var cloned []string
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "list" {
			return orgJSON, nil
		}
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "clone" {
			cloned = append(cloned, args[2])
			return "", nil
		}
		return "", nil
	})
	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--org", "anthropics"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cloned) != 2 {
		t.Fatalf("cloned %d repos, want 2: %v", len(cloned), cloned)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("stdout should have one dest per line, got: %q", stdout.String())
	}
}

func TestCloneCmd_OrgWithQueryArg_ReturnsError(t *testing.T) {
	client := cloneFixture(t, twoHostRunFunc("[]", ""))
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--org", "anthropics", "some-query"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error combining --org with a query argument, got nil")
	}
}

func TestCloneCmd_OrgListFailure_Propagates(t *testing.T) {
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		return "", errors.New("gh: not authenticated")
	})
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--org", "anthropics"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected the org listing error to propagate, got nil")
	}
}

func TestCloneCmd_OrgNoRepos_ReturnsError(t *testing.T) {
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		return "[]", nil
	})
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--org", "anthropics"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for an org with no repos, got nil")
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

// TestCloneCmd_HostileNoteIsEscapedOnStderr is the non-`list` anchor for the
// shared render sink. The escaping helper was introduced for `review` and
// `projects list`, and for a while only those sites called it — a convention,
// not a control. `clone` is one of the seven commands that printed the SAME
// client.Inventory notes raw, and its vector is the cheapest one an attacker
// has: anything that reaches PROJECTS_DIR (a config value, a synced dotfile, a
// repo-local .envrc) turns `forgectl projects clone <no-match>` into a screen
// clear plus attacker-chosen text. Asserting here, not only at `list`, is what
// makes renderDegradationNotes load-bearing at every call site.
func TestCloneCmd_HostileNoteIsEscapedOnStderr(t *testing.T) {
	// Same payload as the `list` twin — CSI erase-display + cursor home, a bare
	// CR, then a right-to-left override — with the override built from its code
	// point rather than typed, so this file needs no staticcheck ST1018 waiver.
	rlo := string(rune(0x202e))
	hostile := filepath.Join(t.TempDir(), "missing\x1b[2J\x1b[H\rforged"+rlo+"gnp")
	t.Setenv("PROJECTS_DIR", hostile)
	fake := &exec.FakeRunner{RunFunc: twoHostRunFunc("[]", "owner\tname\ttype\tssh\n")}
	client := projects.New(fake)

	cmd := newProjectsCloneCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	// A query matching nothing keeps the run deterministic: the notes render,
	// then the command errors out before any picker or clone.
	cmd.SetArgs([]string{"no-such-project"})

	// The error is the point of the arg, not of the test — assert on stderr.
	_ = cmd.ExecuteContext(context.Background())

	body := stderr.String()
	if !strings.Contains(body, "note:") || !strings.Contains(body, "local") {
		t.Fatalf("expected a local-degradation note on stderr, got %q", body)
	}
	for _, r := range hostileRunes {
		if strings.Contains(body, r) {
			t.Errorf("stderr carried %q unescaped: %q", r, body)
		}
	}
	// Quote, don't delete: the operator still needs to see something odd was in
	// the path.
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("want the escape sequence rendered as inert text, got %q", body)
	}
}

// dryRunFixture builds a client over a fake runner rooted at a temp projects
// dir, and returns both. The runner answers nothing, so any real clone attempt
// is visible as a recorded call rather than a side effect.
func dryRunFixture(t *testing.T) (*projects.Client, *exec.FakeRunner, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PROJECTS_DIR", root)
	fake := &exec.FakeRunner{}
	return projects.New(fake), fake, root
}

func runCloneCmd(t *testing.T, client *projects.Client, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	cmd := newProjectsCloneCmd(client)
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return strings.TrimSpace(stdout.String()), err
}

// TestCloneCmd_DryRunPrintsDestinationAndTouchesNothing pins both halves of the
// contract: the scriptable stdout path, and that nothing reached the disk or a
// subprocess. The second half is the one that would silently regress — a
// dry-run that quietly cloned would still print the right path.
func TestCloneCmd_DryRunPrintsDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string // path segments under the projects root
	}{
		{"host tree", []string{"--dry-run", "cameronsjo/quickmd"}, []string{"github.com", "cameronsjo", "quickmd"}},
		{"wing override", []string{"--dry-run", "--wing", "testwing", "cameronsjo/quickmd"}, []string{"testwing", "quickmd"}},
		{"wing name is normalized", []string{"--dry-run", "--wing", "TestWing", "cameronsjo/quickmd"}, []string{"testwing", "quickmd"}},
		{"gitea url keeps its own host", []string{"--dry-run", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"}, []string{"git.sjo.lol", "cameron", "homeclaw"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake, root := dryRunFixture(t)
			got, err := runCloneCmd(t, client, tc.args...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := filepath.Join(append([]string{root}, tc.want...)...); got != want {
				t.Errorf("stdout = %q; want %q", got, want)
			}
			for _, call := range fake.Calls {
				if strings.Contains(strings.Join(call.Args, " "), "clone") {
					t.Errorf("--dry-run ran a clone: %q %v", call.Name, call.Args)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("--dry-run wrote %d entries under the projects root", len(entries))
			}
		})
	}
}

// TestCloneCmd_RefusesUnsafeOrConflictingWing covers the flag's own guards. It
// is a second entry point into the depth-1 namespace and does not go through
// ResolveWings, so these checks are the only thing standing between the flag
// and a directory name.
func TestCloneCmd_RefusesUnsafeOrConflictingWing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "with --org",
			args:    []string{"--wing", "mcp", "--org", "cameronsjo"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "named after the github host",
			args:    []string{"--dry-run", "--wing", "github.com", "cameronsjo/quickmd"},
			wantErr: "configured [github] host",
		},
		{
			name:    "leading dot hides the tree from ls",
			args:    []string{"--dry-run", "--wing", ".hidden", "cameronsjo/quickmd"},
			wantErr: "allowed charset",
		},
		{
			name:    "colon renders as a slash in Finder",
			args:    []string{"--dry-run", "--wing", "wing:8443", "cameronsjo/quickmd"},
			wantErr: "allowed charset",
		},
		{
			name:    "traversal",
			args:    []string{"--dry-run", "--wing", "../escape", "cameronsjo/quickmd"},
			wantErr: "allowed charset",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake, root := dryRunFixture(t)
			_, err := runCloneCmd(t, client, tc.args...)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q; want it to mention %q", err, tc.wantErr)
			}
			for _, call := range fake.Calls {
				if strings.Contains(strings.Join(call.Args, " "), "clone") {
					t.Errorf("a clone ran despite the refusal: %q %v", call.Name, call.Args)
				}
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Errorf("a refused clone wrote %d entries under the projects root", len(entries))
			}
		})
	}
}
