package launch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// writeTree creates each relative path under root: a trailing "/" makes a
// directory, anything else an empty file (its parents created first).
func writeTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if p[len(p)-1] == '/' {
			if err := os.MkdirAll(full, 0o750); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSettingsRoot is cameronsjo/cadence-ecosystem#608's rule: Claude Code
// reads .claude/settings*.json only from its launch directory, so a session
// started in a repository subfolder moves to the root exactly when the root
// carries settings and the subfolder does not.
func TestSettingsRoot(t *testing.T) {
	tests := []struct {
		name string
		tree []string
		cwd  string // relative to the temp root
		want string // relative to the temp root
	}{
		{
			name: "subfolder of a repo with root settings moves to the root",
			tree: []string{".git/", ".claude/settings.json", "pkg/sub/"},
			cwd:  "pkg/sub",
			want: ".",
		},
		{
			name: "settings.local.json alone is enough",
			tree: []string{".git/", ".claude/settings.local.json", "pkg/"},
			cwd:  "pkg",
			want: ".",
		},
		{
			name: "subfolder with its own settings stays",
			tree: []string{".git/", ".claude/settings.json", "pkg/.claude/settings.local.json"},
			cwd:  "pkg",
			want: "pkg",
		},
		{
			name: "repo root with no settings: subfolder stays",
			tree: []string{".git/", "pkg/"},
			cwd:  "pkg",
			want: "pkg",
		},
		{
			name: "a .claude dir without a settings file does not count",
			tree: []string{".git/", ".claude/agents/", "pkg/"},
			cwd:  "pkg",
			want: "pkg",
		},
		{
			name: "no repository above: settings alone do not pull it up",
			tree: []string{".claude/settings.json", "pkg/"},
			cwd:  "pkg",
			want: "pkg",
		},
		{
			name: "linked worktree: a .git file marks the root",
			tree: []string{".git", ".claude/settings.json", "src/deep/"},
			cwd:  "src/deep",
			want: ".",
		},
		{
			name: "the walk stops at the nearest repository",
			tree: []string{".git/", ".claude/settings.json", "vendor/inner/.git/", "vendor/inner/lib/"},
			cwd:  "vendor/inner/lib",
			want: "vendor/inner/lib",
		},
		{
			name: "a dangling settings symlink is no settings file",
			tree: []string{".git/", ".claude/", "pkg/"},
			cwd:  "pkg",
			want: "pkg",
		},
		{
			name: "a .claude that is a file holds no settings",
			tree: []string{".git/", ".claude/settings.json", "pkg/.claude"},
			cwd:  "pkg",
			want: ".",
		},
		{
			name: "the root itself stays",
			tree: []string{".git/", ".claude/settings.json"},
			cwd:  ".",
			want: ".",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := projectDir(t)
			writeTree(t, root, tc.tree...)
			if tc.name == "a dangling settings symlink is no settings file" {
				if err := os.Symlink(filepath.Join(root, "gone.json"), filepath.Join(root, ".claude", "settings.json")); err != nil {
					t.Skipf("symlink: %v", err)
				}
			}
			cwd := filepath.Join(root, tc.cwd)
			want := filepath.Join(root, tc.want)
			if got := settingsRoot(cwd, ""); got != want {
				t.Errorf("SettingsRoot(%q) = %q, want %q", cwd, got, want)
			}
		})
	}
}

// TestSettingsRoot_UnmovedReturnsInputVerbatim pins that "no move" hands back
// the caller's own string, not a cleaned copy. Callers detect a move by
// comparing the two, so a cleaned-but-equal path would read as a move and
// print a notice for a launch that went nowhere.
func TestSettingsRoot_UnmovedReturnsInputVerbatim(t *testing.T) {
	root := projectDir(t)
	writeTree(t, root, "pkg/")
	cwd := root + string(filepath.Separator) + "pkg" + string(filepath.Separator)
	if got := settingsRoot(cwd, ""); got != cwd {
		t.Errorf("SettingsRoot(%q) = %q, want it unchanged", cwd, got)
	}
}

// TestSettingsRoot_SymlinkedCWDWalksThePhysicalPath is review finding 1:
// os.Getwd reports the logical $PWD, so a symlink from outside a repository
// into its subfolder must still find that repository's root, and must not
// climb the directories above the link, where a different repository with
// settings sits here.
func TestSettingsRoot_SymlinkedCWDWalksThePhysicalPath(t *testing.T) {
	outer := projectDir(t)
	writeTree(t, outer, ".git/", ".claude/settings.json", "home/")
	physicalRepo := projectDir(t)
	writeTree(t, physicalRepo, ".git/", ".claude/settings.json", "pkg/")
	link := filepath.Join(outer, "home", "link")
	if err := os.Symlink(filepath.Join(physicalRepo, "pkg"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got := settingsRoot(link, ""); got != physicalRepo {
		t.Errorf("settingsRoot(%q) = %q, want the physical root %q", link, got, physicalRepo)
	}

	// A link into a directory that does not move comes back verbatim.
	writeTree(t, physicalRepo, "pkg/.claude/settings.json")
	if got := settingsRoot(link, ""); got != link {
		t.Errorf("settingsRoot(%q) = %q, want it unchanged", link, got)
	}
}

// TestSettingsRoot_NeverTheUserConfigDir is review finding 2: a git-tracked
// home directory holds ~/.claude/settings.json, the USER settings file, so a
// launch from a non-repository folder under it must stay put.
func TestSettingsRoot_NeverTheUserConfigDir(t *testing.T) {
	home := projectDir(t)
	writeTree(t, home, ".git/", ".claude/settings.json", "notes/")
	cwd := filepath.Join(home, "notes")

	t.Run("home .claude", func(t *testing.T) {
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if got := SettingsRoot(cwd); got != cwd {
			t.Errorf("SettingsRoot(%q) = %q, want it unchanged under a git-tracked home", cwd, got)
		}
	})
	t.Run("CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("HOME", projectDir(t))
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
		if got := SettingsRoot(cwd); got != cwd {
			t.Errorf("SettingsRoot(%q) = %q, want it unchanged when .claude is $CLAUDE_CONFIG_DIR", cwd, got)
		}
	})
	t.Run("an unrelated home still moves", func(t *testing.T) {
		t.Setenv("HOME", projectDir(t))
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if got := SettingsRoot(cwd); got != home {
			t.Errorf("SettingsRoot(%q) = %q, want the repository root %q", cwd, got, home)
		}
	})
}

func TestResumesSession(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-c"}, true},
		{[]string{"--continue"}, true},
		{[]string{"-r", "abc"}, true},
		{[]string{"--resume"}, true},
		{[]string{"--resume=abc"}, true},
		{[]string{"--from-pr", "123"}, true},
		{[]string{"--from-pr=https://github.com/o/r/pull/1"}, true},
		{[]string{"--teleport"}, true},
		{[]string{"-cx"}, false},
		{[]string{"--model", "opus", "-c"}, true},
		{[]string{"-p", "hi"}, false},
		{[]string{"--", "-c"}, false},
	} {
		if got := resumesSession(tc.args); got != tc.want {
			t.Errorf("resumesSession(%q) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

// TestExecIn_WrapsAChdirFailure covers the real exec seam's one step before
// syscall.Exec: a directory it cannot enter is an error naming that
// directory, and the harness is never exec'd (the test process survives).
func TestExecIn_WrapsAChdirFailure(t *testing.T) {
	missing := filepath.Join(projectDir(t), "gone")
	err := ExecIn(missing, "/nonexistent/harness", nil, nil)
	if err == nil {
		t.Fatal("ExecIn into a missing directory returned nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "start the harness in") || !strings.Contains(err.Error(), "gone") {
		t.Errorf("err = %q, want it to name the directory", err)
	}
}

// TestBuildInvocation_RunDirectory pins which launches move to the settings
// root: only the postures that start a claude session, and never a worker,
// a passthrough, another harness, or a launch that asked to stay. The profile
// is resolved for the subfolder in every case, so a subfolder's own project
// block still applies.
func TestBuildInvocation_RunDirectory(t *testing.T) {
	root := projectDir(t)
	writeTree(t, root, ".git/", ".claude/settings.json", "pkg/")
	sub := filepath.Join(root, "pkg")
	bin := ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}

	tests := []struct {
		name      string
		harness   string
		args      []string
		stay      bool
		wantRoot  bool
		wantModel string
	}{
		{name: "claude session", harness: "claude", wantRoot: true},
		{name: "claude builder", harness: "claude", args: []string{"do the thing"}, wantRoot: true},
		{name: "claude print", harness: "claude", args: []string{"-p", "hi"}, wantRoot: true},
		{name: "claude --here", harness: "claude", stay: true},
		{name: "claude --continue", harness: "claude", args: []string{"--continue"}},
		{name: "claude -r in print mode", harness: "claude", args: []string{"-p", "-r", "abc", "hi"}},
		{name: "claude subcommand passthrough", harness: "claude", args: []string{"mcp", "list"}},
		{name: "codex", harness: "codex"},
		{name: "pi", harness: "pi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc := config.LaunchConfig{
				Defaults: config.LaunchDefaults{Harness: tc.harness, Model: "opus"},
				Projects: []config.LaunchProject{{Match: sub, Model: "sonnet"}},
			}
			if tc.harness == "codex" {
				lc.Defaults.Model = ""
				lc.Projects[0].Model = ""
				lc.Defaults.ApprovalPolicy = "on-request"
				lc.Defaults.Sandbox = "read-only"
			}
			if tc.harness == "pi" {
				lc.Defaults.Model = ""
				lc.Projects[0].Model = ""
			}
			built, err := BuildInvocation(InvocationRequest{
				Config:         lc,
				CWD:            sub,
				Args:           tc.args,
				BaseEnv:        []string{"PWD=" + sub, "KEEP=1"},
				Resolve:        fixedResolver(bin),
				StayInCWD:      tc.stay,
				StdoutTerminal: true,
			})
			if err != nil {
				t.Fatalf("BuildInvocation: %v", err)
			}
			want := sub
			if tc.wantRoot {
				want = root
			}
			if built.Invocation.CWD != want {
				t.Errorf("Invocation.CWD = %q, want %q (posture %s)", built.Invocation.CWD, want, built.Posture)
			}
			if !slices.Contains(built.Invocation.Env, "PWD="+want) {
				t.Errorf("Env = %q, want PWD=%s", built.Invocation.Env, want)
			}
			if !slices.Contains(built.Invocation.Env, "KEEP=1") {
				t.Errorf("Env = %q lost an inherited variable", built.Invocation.Env)
			}
			if tc.harness == "claude" && built.Profile.Model != "sonnet" {
				t.Errorf("profile model %q, want the subfolder project's sonnet", built.Profile.Model)
			}
		})
	}
}
