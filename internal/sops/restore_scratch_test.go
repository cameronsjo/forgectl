package sops

// The restore's own scratch directory under the plaintext guard
// (cameronsjo/forgectl#751). The restore writes the backup over the target
// through env's atomic write, which makes a `.forgectl-env-` scratch directory
// beside the target. Before this, the guard did not know it, so a signal
// mid-restore terminated the process with it on disk.
//
//   [x] A signal once the restore's scratch directory exists removes it: the
//       handler runs, and the write goes no further, as die never returns
//   [x] A signal before the restore begins refuses its scratch directory: the
//       restore fails and leaves no scratch directory behind

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
)

// restoreFixture sets up a target and a staged work directory under a guard
// inside the mutation span, as SetValue has them when it restores.
func restoreFixture(t *testing.T) (repo string, target env.Target, g *plaintextGuard, work *workDir, death *fakeDeath) {
	t.Helper()
	repo = t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "secrets.sops.yaml"), []byte(backupCiphertext), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	t.Cleanup(target.Close)

	death = newFakeDeath()
	g = startPlaintextGuard(make(chan os.Signal, 1), noStop, death.die)
	t.Cleanup(g.release)
	work, err = g.track(func() (*workDir, error) { return newWorkDir(target) })
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := work.stage([]byte(backupCiphertext), "s3cr3t"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	g.beginMutation()
	return repo, target, g, work, death
}

// envScratchIn lists the atomic write's scratch directories in dir.
func envScratchIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forgectl-env-") {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestGuard_SignalDuringRestoreRemovesItsScratch(t *testing.T) {
	repo, target, g, work, death := restoreFixture(t)

	type killed struct{}
	var seen string
	track := func(dir string) error {
		if err := g.trackScratch(dir); err != nil {
			return err
		}
		seen = dir
		// The handler, run where a signal would land: the directory exists
		// and the write is about to fill it. In production die does not
		// return, so the write goes no further; the panic stands in for that.
		g.fire(syscall.SIGTERM)
		panic(killed{})
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(killed); !ok {
					panic(r)
				}
			}
		}()
		_ = work.restore(target, track)
		t.Fatal("the restore was never interrupted: its scratch directory was never reported")
	}()

	if seen == "" || filepath.Dir(seen) != repo {
		t.Fatalf("the restore reported scratch directory %q, want one beside the target in %s", seen, repo)
	}
	if got := death.snapshot(); len(got) != 1 {
		t.Fatalf("die calls = %v, want exactly one", got)
	}
	if left := envScratchIn(t, repo); len(left) != 0 {
		t.Errorf("a signal during the restore left its scratch directory behind: %v", left)
	}
}

func TestGuard_SignalBeforeRestoreRefusesItsScratch(t *testing.T) {
	repo, target, g, work, _ := restoreFixture(t)
	// Marked as the handler marks it. Running the handler itself would move
	// the backup out first, and the restore would stop at the missing backup
	// before it reached its write.
	g.mu.Lock()
	g.fired = true
	g.mu.Unlock()

	err := work.restore(target, g.trackScratch)
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("restore after a signal = %v, want errInterrupted", err)
	}
	if left := envScratchIn(t, repo); len(left) != 0 {
		t.Errorf("a refused restore left its scratch directory behind: %v", left)
	}
}
