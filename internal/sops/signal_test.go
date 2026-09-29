package sops

// Unit tests for the plaintext guard's ordering, driven with an injected
// signal channel, stop, and die so each interleaving is deterministic. The
// real-signal, real-process test is in signal_unix_test.go.
//
//   [x] A caught signal removes the work directory, then dies with that signal
//   [x] Release with no signal neither dies nor leaks the handler goroutine
//   [x] A signal that lands between the handler stopping and delivery stopping
//       is still acted on, not swallowed
//   [x] A signal before the directory exists means it is never created
//   [x] 130 for SIGINT, 143 for SIGTERM
//   [x] A signal inside the mutation span keeps the ciphertext backup beside
//       the target, intact, and removes the plaintext and the directory
//   [x] A signal before the span (target untouched) keeps nothing
//   [x] A signal after settle (target proven) keeps nothing
//   [x] An existing file at the keep path is never replaced
//   [x] A normal return inside the span (a failed restore, a panic) keeps the
//       backup; one outside it keeps nothing
//   [x] keepBackup reports where the backup went
//   [x] With the keep path taken, a normal return leaves the work directory
//       holding the backup and nothing else

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeDeath records what die was called with. In production die never
// returns; here it records and returns so the test can inspect the aftermath.
type fakeDeath struct {
	mu    sync.Mutex
	calls []os.Signal
	fired chan struct{}
}

func newFakeDeath() *fakeDeath { return &fakeDeath{fired: make(chan struct{}, 4)} }

func (d *fakeDeath) die(sig os.Signal) {
	d.mu.Lock()
	d.calls = append(d.calls, sig)
	d.mu.Unlock()
	d.fired <- struct{}{}
}

func (d *fakeDeath) snapshot() []os.Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]os.Signal(nil), d.calls...)
}

// stagedWorkDir makes a work directory holding a plaintext value file, the
// state the guard exists to clean up.
func stagedWorkDir(t *testing.T) func() (*workDir, error) {
	t.Helper()
	return func() (*workDir, error) {
		dir := filepath.Join(t.TempDir(), ".forgectl-sops-test")
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "value"), []byte("s3cr3t"), 0o600); err != nil {
			return nil, err
		}
		return &workDir{dir: dir}, nil
	}
}

func assertGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work directory %s still exists (stat err %v)", dir, err)
	}
}

func noStop(chan<- os.Signal) {}

func TestGuard_SignalRemovesWorkDirThenDies(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	g := startPlaintextGuard(ch, noStop, death.die)

	work, err := g.track(stagedWorkDir(t))
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	ch <- syscall.SIGTERM

	select {
	case <-death.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
	assertGone(t, work.dir)
	if got := death.snapshot(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Fatalf("die calls = %v, want exactly [SIGTERM]", got)
	}

	g.cleanup()
	g.release()
	if got := death.snapshot(); len(got) != 1 {
		t.Fatalf("die was called %d times, want 1", len(got))
	}
}

func TestGuard_ReleaseWithoutSignalStopsCleanly(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	var stopped bool
	g := startPlaintextGuard(ch, func(chan<- os.Signal) { stopped = true }, death.die)

	work, err := g.track(stagedWorkDir(t))
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	g.cleanup()
	g.release()

	assertGone(t, work.dir)
	if !stopped {
		t.Error("release did not stop signal delivery")
	}
	select {
	case <-g.exited:
	default:
		t.Error("the handler goroutine outlived release")
	}
	if got := death.snapshot(); len(got) != 0 {
		t.Fatalf("die was called with %v on a run with no signal", got)
	}
}

// A signal that reaches the channel after the handler goroutine has stopped
// but before signal.Stop takes effect must not vanish: nobody else will ever
// read it, and the operator's Ctrl-C would be silently eaten. The injected
// stop delivers exactly that signal, at exactly that point.
func TestGuard_SignalAtReleaseIsNotSwallowed(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	lateStop := func(c chan<- os.Signal) { c <- os.Interrupt }
	g := startPlaintextGuard(ch, lateStop, death.die)

	if _, err := g.track(stagedWorkDir(t)); err != nil {
		t.Fatalf("track: %v", err)
	}
	g.cleanup()
	g.release()

	if got := death.snapshot(); len(got) != 1 || got[0] != os.Interrupt {
		t.Fatalf("die calls = %v, want exactly [interrupt]", got)
	}
}

func TestGuard_SignalBeforeTrackPreventsCreation(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	g := startPlaintextGuard(ch, noStop, death.die)

	ch <- syscall.SIGTERM
	<-death.fired

	created := false
	_, err := g.track(func() (*workDir, error) {
		created = true
		return stagedWorkDir(t)()
	})
	if err == nil || created {
		t.Fatalf("track after a fired signal: err=%v created=%v, want a refusal and no directory", err, created)
	}
	g.cleanup()
	g.release()
}

func TestExitStatusFor(t *testing.T) {
	cases := []struct {
		sig  os.Signal
		want int
	}{
		{os.Interrupt, 130},
		{syscall.SIGTERM, 143},
	}
	for _, c := range cases {
		if got := exitStatusFor(c.sig); got != c.want {
			t.Errorf("exitStatusFor(%v) = %d, want %d", c.sig, got, c.want)
		}
	}
}

const backupCiphertext = "a: ENC[AES256_GCM,data:x,type:str]\nsops:\n    mac: m\n"

// backedWorkDir makes a work directory the way stage leaves it: the ciphertext
// backup, the plaintext value, and a decrypted read-back. keep is beside it,
// where the target would be.
func backedWorkDir(t *testing.T) (create func() (*workDir, error), keep string) {
	t.Helper()
	parent := t.TempDir()
	keep = filepath.Join(parent, ".forgectl-sops-000000000000.backup")
	return func() (*workDir, error) {
		dir := filepath.Join(parent, ".forgectl-sops-000000000000-1")
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
		w := &workDir{dir: dir, backup: filepath.Join(dir, "backup"), keep: keep}
		for name, body := range map[string]string{"backup": backupCiphertext, "value": "s3cr3t", "landed": "s3cr3t"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				return nil, err
			}
		}
		return w, nil
	}, keep
}

func fireAndWait(t *testing.T, ch chan os.Signal, death *fakeDeath) {
	t.Helper()
	ch <- syscall.SIGTERM
	select {
	case <-death.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
}

func TestGuard_SignalInsideMutationKeepsBackup(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	g := startPlaintextGuard(ch, noStop, death.die)
	create, keep := backedWorkDir(t)
	work, err := g.track(create)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	g.beginMutation()
	fireAndWait(t, ch, death)

	assertGone(t, work.dir)
	got, err := os.ReadFile(filepath.Clean(keep))
	if err != nil {
		t.Fatalf("the ciphertext backup was not kept at %s: %v", keep, err)
	}
	if string(got) != backupCiphertext {
		t.Errorf("the kept backup = %q, want the ciphertext unchanged", got)
	}
	g.cleanup()
	g.release()
}

func TestGuard_SignalOutsideMutationKeepsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(g *plaintextGuard)
	}{
		{"before the edit launches", func(*plaintextGuard) {}},
		{"after settle", func(g *plaintextGuard) { g.beginMutation(); g.settle() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan os.Signal, 1)
			death := newFakeDeath()
			g := startPlaintextGuard(ch, noStop, death.die)
			create, keep := backedWorkDir(t)
			work, err := g.track(create)
			if err != nil {
				t.Fatalf("track: %v", err)
			}
			tc.setup(g)
			fireAndWait(t, ch, death)

			assertGone(t, work.dir)
			if _, err := os.Lstat(keep); !os.IsNotExist(err) {
				t.Errorf("a backup was kept at %s with the target untouched or proven (err %v); the next run would refuse for nothing", keep, err)
			}
			g.cleanup()
			g.release()
		})
	}
}

func TestGuard_KeepNeverReplacesAnExistingFile(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	g := startPlaintextGuard(ch, noStop, death.die)
	create, keep := backedWorkDir(t)
	const earlier = "an earlier run's evidence"
	if err := os.WriteFile(keep, []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	work, err := g.track(create)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	g.beginMutation()
	fireAndWait(t, ch, death)

	assertGone(t, work.dir)
	got, err := os.ReadFile(filepath.Clean(keep))
	if err != nil || string(got) != earlier {
		t.Errorf("the existing file at the keep path was replaced: %q, %v", got, err)
	}
	g.cleanup()
	g.release()
}

// A normal return from inside the span means the target was never verified
// and no restore was proven: a failed restore, or a panic. Cleanup keeps the
// backup then, exactly as a signal does (cameronsjo/forgectl#652). It used to
// keep nothing, so "could NOT be restored" was followed by deleting the one
// copy that could.
func TestGuard_NormalReturnInsideMutationKeepsBackup(t *testing.T) {
	ch := make(chan os.Signal, 1)
	death := newFakeDeath()
	g := startPlaintextGuard(ch, noStop, death.die)
	create, keep := backedWorkDir(t)
	work, err := g.track(create)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	g.beginMutation()
	g.cleanup()
	g.release()

	assertGone(t, work.dir)
	got, err := os.ReadFile(filepath.Clean(keep))
	if err != nil {
		t.Fatalf("a return inside the span lost the ciphertext backup: %v", err)
	}
	if string(got) != backupCiphertext {
		t.Errorf("the kept backup = %q, want the ciphertext unchanged", got)
	}
	if n := len(death.snapshot()); n != 0 {
		t.Errorf("die was called %d times on a normal return", n)
	}
}

// A normal return after settle, or before the span, keeps nothing: the
// target is proven or untouched, and a kept backup would only make the next
// run refuse for nothing.
func TestGuard_NormalReturnOutsideMutationKeepsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(g *plaintextGuard)
	}{
		{"before the edit launches", func(*plaintextGuard) {}},
		{"after settle", func(g *plaintextGuard) { g.beginMutation(); g.settle() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := startPlaintextGuard(make(chan os.Signal, 1), noStop, newFakeDeath().die)
			create, keep := backedWorkDir(t)
			work, err := g.track(create)
			if err != nil {
				t.Fatalf("track: %v", err)
			}
			tc.setup(g)
			g.cleanup()
			g.release()

			assertGone(t, work.dir)
			if _, err := os.Lstat(keep); !os.IsNotExist(err) {
				t.Errorf("a proven or untouched return kept a backup at %s (err %v)", keep, err)
			}
		})
	}
}

// keepBackup reports where the backup went, so the restore-failure error can
// name it, and the deferred cleanup after it changes nothing.
func TestGuard_KeepBackupReportsWhereItWent(t *testing.T) {
	g := startPlaintextGuard(make(chan os.Signal, 1), noStop, newFakeDeath().die)
	create, keep := backedWorkDir(t)
	work, err := g.track(create)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	g.beginMutation()
	if got := g.keepBackup(); got != keep {
		t.Errorf("keepBackup() = %q, want %q", got, keep)
	}
	g.cleanup()
	g.release()
	assertGone(t, work.dir)
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the backup is not at %s: %v", keep, err)
	}
}

// When the keep path is taken, a normal return has no child that could write
// into the work directory, so the directory stays holding the backup and
// nothing else: the backup is never lost, and no plaintext stays with it.
func TestGuard_KeepPathTakenLeavesOnlyTheBackup(t *testing.T) {
	g := startPlaintextGuard(make(chan os.Signal, 1), noStop, newFakeDeath().die)
	create, keep := backedWorkDir(t)
	const earlier = "an earlier run's evidence"
	if err := os.WriteFile(keep, []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	work, err := g.track(create)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := os.Mkdir(filepath.Join(work.dir, "1234"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work.dir, "1234", "doc.yaml"), []byte("a: s3cr3t"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.beginMutation()
	if got := g.keepBackup(); got != work.backup {
		t.Errorf("keepBackup() = %q, want the backup left in place at %q", got, work.backup)
	}
	g.cleanup()
	g.release()

	entries, err := os.ReadDir(work.dir)
	if err != nil {
		t.Fatalf("the work directory holding the only backup was removed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "backup" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the kept work directory holds %v, want only the backup", names)
	}
	got, err := os.ReadFile(filepath.Clean(work.backup))
	if err != nil || string(got) != backupCiphertext {
		t.Errorf("the backup = %q, %v; want the ciphertext unchanged", got, err)
	}
	if got, err := os.ReadFile(filepath.Clean(keep)); err != nil || string(got) != earlier {
		t.Errorf("the existing file at the keep path was replaced: %q, %v", got, err)
	}
}
