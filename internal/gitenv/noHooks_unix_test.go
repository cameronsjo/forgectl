//go:build unix

package gitenv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// worktree add runs its children with GIT_DIR at the new worktree's admin
// directory, on the new branch, so a conditional include that holds only
// for them defines hooks a listing in the repository never sees. RunNoHooks
// switches those off as well (#1079): an includeIf "onbranch:" on the branch
// being added and an includeIf "gitdir:" on the new admin directory. Each
// case first proves the hook fires through the hooks-directory pin alone.
// Needs git 2.54, the first to run configured hooks.
// Mutation: drop includedHookNames from RunNoHooks: on git 2.54 the hook
// runs. git 2.55 also honours the event-wide hook.<event>.enabled=false
// RunNoHooks passes, which stops it there too, so the mutation shows only on
// 2.54.
func TestRunNoHooksSwitchesOffConditionallyIncludedHooks(t *testing.T) {
	gitenvtest.RequireGitVersion(t, 2, 54)
	cases := map[string]struct {
		condition   string
		ctl, branch string
	}{
		"onbranch": {"onbranch:evil-*", "evil-ctl", "evil-run"},
		"gitdir":   {"gitdir:**/worktrees/gd-*", "gd-ctl", "gd-run"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			tmp := t.TempDir()
			top := filepath.Join(tmp, "repo")
			if err := os.Mkdir(top, 0o750); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tmp, "hook-ran")
			inc := filepath.Join(tmp, "hooks.inc")
			body := "[hook \"canary\"]\n\tcommand = touch '" + marker + "'\n" +
				"\tevent = post-checkout\n\tevent = post-index-change\n\tevent = reference-transaction\n"
			if err := os.WriteFile(inc, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			gitenvtest.Git(t, top, "init", "-q", "-b", "main")
			gitenvtest.Git(t, top, "commit", "-q", "--allow-empty", "-m", "control")
			gitenvtest.Git(t, top, "config", "includeIf."+c.condition+".path", inc)

			// The control: with the hooks directory pinned and no per-name
			// switch, as a plain listing in top would leave it, the
			// conditionally included hook runs. The event-wide switches are
			// left out: git 2.55 honours them, which would stop the hook
			// for a reason this test is not about.
			ctl := []string{"-c", "core.hooksPath=/dev/null", "-C", top, "worktree", "add", "-q", "-b", c.ctl, filepath.Join(tmp, c.ctl)}
			if _, err := gitenv.Run(t.Context(), exec.OSRunner{}, gitenv.Local, ctl...); err != nil {
				t.Fatalf("control add: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("control: the conditionally included hook did not run; the fixture cannot fire: %v", err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}

			if _, err := gitenv.RunNoHooks(t.Context(), exec.OSRunner{}, top, "worktree", "add", "-q", "-b", c.branch, filepath.Join(tmp, c.branch)); err != nil {
				t.Fatalf("RunNoHooks: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a conditionally included hook ran during RunNoHooks's worktree add")
			}
		})
	}
}

// RunNoHooks refuses, running nothing, the include shapes it does not
// emulate (#1079), each of which git follows to a hook a listing in the
// repository cannot see:
//   - a relative include in config.worktree, which worktree add copies into
//     the new worktree's admin directory, where the children resolve it
//     against the copy (a plain and a bare repository);
//   - an include whose path has a ".." component after a symbolic link,
//     which the kernel resolves through the link, not lexically.
//
// Each case first proves git runs the hook through the hooks-directory pin
// alone. Needs git 2.54.
// Mutations: drop the ".." refusal from resolveInclude (the symbolic-link
// case runs the add); drop it and the config.worktree refusal (both
// config.worktree cases run it too). The config.worktree refusal alone is
// pinned by TestResolveInclude.
func TestRunNoHooksRefusesIncludesItCannotResolve(t *testing.T) {
	gitenvtest.RequireGitVersion(t, 2, 54)
	hook := func(marker string) string {
		return "[hook \"pwn\"]\n\tcommand = touch '" + marker + "'\n" +
			"\tevent = post-checkout\n\tevent = post-index-change\n\tevent = reference-transaction\n"
	}
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, tmp, marker string) (gitDir, top string){
		"a relative include in config.worktree": func(t *testing.T, tmp, marker string) (string, string) {
			top := filepath.Join(tmp, "repo")
			gitenvtest.Git(t, "", "init", "-q", "-b", "main", top)
			gitenvtest.Git(t, top, "commit", "-q", "--allow-empty", "-m", "control")
			gitenvtest.Git(t, top, "config", "extensions.worktreeConfig", "true")
			gitenvtest.Git(t, top, "config", "--worktree", "include.path", "../evil.inc")
			write(t, filepath.Join(top, ".git", "worktrees", "evil.inc"), hook(marker))
			return filepath.Join(top, ".git"), top
		},
		"a relative include in a bare repository's config.worktree": func(t *testing.T, tmp, marker string) (string, string) {
			src := filepath.Join(tmp, "src")
			gitenvtest.Git(t, "", "init", "-q", "-b", "main", src)
			gitenvtest.Git(t, src, "commit", "-q", "--allow-empty", "-m", "control")
			bare := filepath.Join(tmp, "b.git")
			gitenvtest.Git(t, "", "clone", "-q", "--bare", src, bare)
			gitenvtest.Git(t, bare, "config", "extensions.worktreeConfig", "true")
			gitenvtest.Git(t, bare, "config", "--worktree", "include.path", "../evil.inc")
			write(t, filepath.Join(bare, "worktrees", "evil.inc"), hook(marker))
			return bare, bare
		},
		"a '..' after a symbolic link": func(t *testing.T, tmp, marker string) (string, string) {
			top := filepath.Join(tmp, "repo")
			gitenvtest.Git(t, "", "init", "-q", "-b", "main", top)
			gitenvtest.Git(t, top, "commit", "-q", "--allow-empty", "-m", "control")
			deep := filepath.Join(tmp, "other", "deep")
			write(t, filepath.Join(tmp, "other", "evil.inc"), hook(marker))
			if err := os.MkdirAll(deep, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(deep, filepath.Join(top, ".git", "lnk")); err != nil {
				t.Fatal(err)
			}
			gitenvtest.Git(t, top, "config", "includeIf.onbranch:nb-*.path", "lnk/../evil.inc")
			return filepath.Join(top, ".git"), top
		},
	}
	for name, stage := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			tmp := t.TempDir()
			marker := filepath.Join(tmp, "hook-ran")
			_, top := stage(t, tmp, marker)

			ctl := []string{"-c", "core.hooksPath=/dev/null", "-C", top, "worktree", "add", "-q", "-b", "nb-ctl", filepath.Join(tmp, "nb-ctl")}
			if _, err := gitenv.Run(t.Context(), exec.OSRunner{}, gitenv.Local, ctl...); err != nil {
				t.Fatalf("control add: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("control: git did not run the included hook; the fixture cannot fire: %v", err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}

			wt := filepath.Join(tmp, "nb-run")
			if _, err := gitenv.RunNoHooks(t.Context(), exec.OSRunner{}, top, "worktree", "add", "-q", "-b", "nb-run", wt); err == nil {
				t.Fatal("RunNoHooks ran the add")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the included hook ran during RunNoHooks")
			}
			if _, err := os.Stat(wt); err == nil {
				t.Fatal("RunNoHooks created the worktree")
			}
		})
	}
}
