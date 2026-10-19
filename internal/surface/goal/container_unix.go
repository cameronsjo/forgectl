//go:build unix

package goal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (engine *execution) probeContainerBoundary(ctx context.Context, role *nativeRole, roots []string) error {
	state := engine.snapshot()
	identifier, err := newID()
	if err != nil {
		return err
	}
	allowed := filepath.Join(role.Directory, "probe-"+identifier)
	gitDirectory, err := git(ctx, state.Worktree, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	metadata := filepath.Join(strings.TrimSpace(string(gitDirectory)), "HEAD")
	script := `set -eu
printf allowed > "$1"
test -r "$2"
test -r "$3"
test ! -e "$4"
if printf '' 2> "$5" >> "$2"; then exit 71; fi
if printf '' 2>> "$5" >> "$3"; then exit 72; fi
printf boundary-ok`
	denials := filepath.Join(role.Directory, "probe-denials-"+identifier)
	result, err := role.Executor.Exec(ctx, []string{"/bin/sh", "-c", script, "goal-container-boundary", allowed, filepath.Join(state.Worktree, ".git"), metadata, filepath.Join(state.RunDir, "runtime.json"), denials}, role.Member.Worktree, roots, 10*time.Second)
	if err != nil {
		return err
	}
	directory, err := os.OpenRoot(role.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	content, readErr := directory.ReadFile(filepath.Base(allowed))
	denied, denialErr := directory.ReadFile(filepath.Base(denials))
	if result.ExitCode != 0 || result.Stdout != "boundary-ok" || readErr != nil || string(content) != "allowed" || denialErr != nil || strings.Count(string(denied), "Read-only file system") != 2 {
		return errors.New("container mount/UID boundary did not engage; verify runtime file sharing before model work")
	}
	if role.Name != "implementation" {
		target := filepath.Join(state.Worktree, "probe-"+identifier)
		result, err = role.Executor.Exec(ctx, []string{"/bin/sh", "-c", `printf forbidden > "$1"`, "goal-readonly-worktree", target}, role.Member.Worktree, roots, 10*time.Second)
		if err != nil {
			return err
		}
		_, exists := os.Lstat(target)
		if result.ExitCode == 0 || !strings.Contains(result.Stderr, "Read-only file system") || !errors.Is(exists, os.ErrNotExist) {
			return errors.New("container model role received writable implementation access")
		}
	}
	return engine.record(Event{Time: time.Now().UTC(), Kind: "boundary-probe", Role: role.Name, Summary: "container role write works; existing Git metadata is read-only, control state is absent, and model role mounts match approval"})
}
