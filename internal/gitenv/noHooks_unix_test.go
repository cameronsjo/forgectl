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
// case first proves the hook fires through the hooks-directory pin and the
// switches a plain listing yields. Needs git 2.54, the first to run
// configured hooks.
// Mutation: drop includedHookNames from RunNoHooks: the hook runs.
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

			// The control: the switches a plain listing in top yields (none)
			// leave the conditionally included hook live.
			ctl := append(append(gitenv.NoHookArgs(nil), "-C", top), "worktree", "add", "-q", "-b", c.ctl, filepath.Join(tmp, c.ctl))
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
