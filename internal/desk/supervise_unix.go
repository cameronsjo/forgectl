//go:build unix

package desk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// supervisorArgv is the command line that runs `forgectl desk _supervise`.
// Tests replace it to re-execute the test binary.
var supervisorArgv = func(dir, name, sha string, kind Kind) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("desk: locate the forgectl binary: %w", err)
	}
	return []string{exe, "desk", "_supervise", "--dir", dir, "--sha", sha, "--kind", string(kind), name}, nil
}

// skipCauseMax caps the cause recorded in skip_reason when a supervisor
// could not start a run.
const skipCauseMax = 256

// Launch starts the detached supervisor that runs a claimed item, and
// returns its pid. TTY items are refused: they run in the desk's own
// foreground (see [Desk.BeginRun]).
//
// The supervisor is handed the approved hash and kind, c.SHA256 and c.Kind,
// and runs the item only if running/ still holds exactly those bytes, as that
// kind. It starts in the home
// directory with [ChildEnv], and Launch records its pid in meta at once, so
// a supervisor that dies before its run begins leaves the item lost, not
// waiting forever.
//
// A non-zero pid with an error means the supervisor started but its pid
// could not be recorded; the run goes ahead and records it itself. Do not
// [Desk.Abandon] it.
//
// The supervisor must outlive the desk, so it is started with no parent
// context and in a session of its own (Setsid): closing the desk's pane
// signals the desk's process group and session, and neither reaches it. It
// deliberately does not use internal/exec's process-group runner, which kills
// the group when its context ends — the opposite of what is wanted here.
func (d *Desk) Launch(c *Claimed) (int, error) {
	if c == nil {
		return 0, errors.New("desk: launch: no claimed item")
	}
	kind, err := d.findKind(DirRunning, c.Name)
	if err != nil {
		return 0, fmt.Errorf("desk: launch %s: claim it first: %w", describe(c.Name), err)
	}
	if kind != c.Kind {
		return 0, fmt.Errorf("desk: launch %s: running/ holds a %s, but the claim is for a %s", describe(c.Name), kind, c.Kind)
	}
	if !ValidSHA(c.SHA256) {
		return 0, fmt.Errorf("%w: launch %s: the claim carries no full sha256", ErrRefused, describe(c.Name))
	}
	if c.Kind == KindScript && c.TTY {
		return 0, fmt.Errorf("desk: %s is a TTY item; it runs in the desk's foreground, not detached", describe(c.Name))
	}
	home, err := HomeDir()
	if err != nil {
		return 0, err
	}
	argv, err := supervisorArgv(d.path, c.Name, c.SHA256, c.Kind)
	if err != nil {
		return 0, err
	}
	// context.Background on purpose: no caller's lifetime may end this run.
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // G204: re-executing this binary's own hidden subcommand
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // /dev/null
	cmd.Dir, cmd.Env = home, ChildEnv()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("desk: start supervisor for %s: %w", describe(c.Name), err)
	}
	pid := cmd.Process.Pid
	// Reap it whenever it ends, so a long-lived desk never holds a zombie
	// that a liveness check would read as alive.
	go func() { _ = cmd.Wait() }()
	if err := d.recordOwner(c.Name, pid); err != nil {
		return pid, fmt.Errorf("desk: the supervisor for %s started (pid %d) but its pid was not recorded: %w", describe(c.Name), pid, err)
	}
	return pid, nil
}

// recordOwner writes pid as a just-started supervisor's owner of a claimed
// item, unless an owner is already recorded or a run has begun. The
// supervisor's own BeginRun may write meta at the same moment; whichever
// lands last, PID is the same process, and Finish rewrites the whole meta
// from the run's copy.
func (d *Desk) recordOwner(name string, pid int) error {
	meta, ok, err := d.readMeta(DirRunning, name)
	if err != nil || !ok || meta.PID != 0 || meta.StartedAt != nil {
		return err
	}
	meta.PID, meta.PIDStart = pid, ownStart(pid)
	return d.writeMeta(DirRunning, name, meta)
}

// RunSupervisor is `forgectl desk _supervise`: it runs one claimed item in
// running/ to completion and returns the process exit code (the item's rc, or
// 2 when the supervisor itself could not run it). sha and kind are the hash
// and kind the operator approved; the item runs only if its bytes still hash
// to sha and it is still that kind ("script" or "batch").
func RunSupervisor(dir, name, sha, kind string) int {
	// Caught and discarded, not ignored: an ignored signal stays ignored
	// across exec, so every script would start deaf to SIGHUP, while a caught
	// one is reset to the default there. Notify never blocks on a full
	// channel, so nothing needs to read it.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	d, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "desk _supervise:", err)
		return 2
	}
	defer d.Close() //nolint:errcheck // exiting
	rc, err := d.supervise(name, sha, Kind(kind))
	if err != nil {
		fmt.Fprintln(os.Stderr, "desk _supervise:", err)
		if rc == 0 {
			rc = 2
		}
	}
	return rc
}

func (d *Desk) supervise(name, sha string, want Kind) (int, error) {
	kind, err := d.findKind(DirRunning, name)
	if err != nil {
		return 2, err
	}
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		return 2, err
	}
	// Claim stamps the approved hash, and the queued kind is the approved
	// kind. A supervisor told another hash or kind was not started for this
	// claim, so it refuses without touching the item: the desk that claimed
	// it still launches it, or gives it back.
	if sha != meta.ApprovedSHA256 || (meta.Kind != "" && meta.Kind != want) {
		return 2, fmt.Errorf("%w: %s: this supervisor's approval does not name this claim", ErrRefused, describe(name))
	}
	notStarted := func(reason string, cause error) (int, error) {
		return 2, d.skipUnstarted(name, kind, reason, cause)
	}
	failed := func(cause error) (int, error) {
		return notStarted("supervisor: "+termsafe.SafeLineMax(cause.Error(), skipCauseMax), cause)
	}
	// The hash binds the bytes, not how they run: findKind prefers .sh, so
	// the bytes of an approved manifest must not run as bash, nor the reverse.
	if d.exists(path.Join(DirRunning, name+KindScript.Ext())) && d.exists(path.Join(DirRunning, name+KindBatch.Ext())) {
		// Which file is the claimed item is unknowable here, so both go to
		// skipped/: the found one with the meta and the reason, then the
		// other, once skipUnstarted has moved the first (it leaves an item a
		// live process owns alone).
		cause := fmt.Errorf("%w: running/ holds both %s.sh and %s.manifest", ErrRefused, describe(name), describe(name))
		rc, err := failed(cause)
		other := name + KindBatch.Ext()
		if !d.exists(path.Join(DirRunning, name+kind.Ext())) {
			if merr := d.root.Rename(path.Join(DirRunning, other), path.Join(DirSkipped, other)); merr != nil && !isNotExist(merr) {
				err = errors.Join(err, fmt.Errorf("desk: move %s to skipped/: %w", describe(other), merr))
			}
		}
		return rc, err
	}
	if kind != want {
		return failed(fmt.Errorf("%w: %s was approved as a %q and queued as a %q, but running/ holds a %s",
			ErrRefused, describe(name), termsafe.SafeLineMax(string(want), 16), meta.Kind, kind))
	}
	data, _, err := d.readItem(DirRunning, name+kind.Ext())
	if err != nil {
		return failed(err)
	}
	// The bytes must be the ones the operator approved, not only the ones the
	// meta names: anything that can write the desk can rewrite both together.
	// sha is the stamped hash (checked above), so a mismatch here means the
	// bytes changed.
	if sum := SHA256Hex(data); sum != meta.SHA256 || sum != sha {
		return notStarted(SkipChanged, fmt.Errorf("%w: %s is not the item that was approved", ErrChanged, describe(name)))
	}
	if kind == KindScript && ParseHeaders(data).TTY {
		return failed(fmt.Errorf("desk: %s is a TTY item; it runs in the desk's foreground", describe(name)))
	}
	if d.doneTaken(name) {
		return notStarted(SkipReused, fmt.Errorf("desk: done/ already holds %s; skipped", describe(name)))
	}
	home, err := HomeDir()
	if err != nil {
		return failed(err)
	}
	var m *Manifest
	var perr error
	var fields []string
	if kind == KindBatch {
		if m, perr = LoadManifest(data, name+extManifest); perr == nil {
			fields = []string{"steps=" + strconv.Itoa(len(m.Steps)), "jobs=" + strconv.Itoa(DefaultJobs)}
		}
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	run, err := d.BeginRun(name, os.Getpid(), fields...)
	if err != nil {
		return failed(err)
	}
	if kind == KindBatch {
		return d.superviseBatch(run, m, perr, home, sigs)
	}
	return d.superviseScript(run, data, home, sigs)
}

// skipUnstarted moves an item this supervisor could not start to skipped/
// with reason, so a failed start leaves a trace: the supervisor's own stderr
// is /dev/null. An item another process owns is left alone, so a stray
// `_supervise` cannot take a live run out of running/.
//
// So is a claim with no owner yet whose claimer is alive and did not start
// this supervisor: a TTY item claimed for the desk's own foreground, which
// a `_supervise` given its stamped sha would otherwise move to skipped/
// before the desk's BeginRun. Launch starts the real supervisor as the
// claimer's child, so its parent is the claimer, or the claimer has died.
// Every refusal after the stamp check comes through here.
func (d *Desk) skipUnstarted(name string, kind Kind, reason string, cause error) error {
	meta, ok, err := d.readMeta(DirRunning, name)
	if err != nil {
		return errors.Join(cause, err)
	}
	if !ok {
		// Gone from running/ (an Abandon moved it): writing a meta here
		// would leave a stray one behind.
		return cause
	}
	if meta.PID != 0 && meta.PID != os.Getpid() {
		return cause
	}
	if meta.PID == 0 && !claimerLetGo(meta) {
		return cause
	}
	meta.Kind, meta.SkipReason = kind, reason
	if err := d.writeMeta(DirRunning, name, meta); err != nil {
		return errors.Join(cause, err)
	}
	if err := d.move(name, kind, DirRunning, DirSkipped); err != nil && !isNotExist(err) {
		return errors.Join(cause, fmt.Errorf("desk: move %s to skipped/: %w", describe(name), err))
	}
	return cause
}

// claimerLetGo reports whether this process may move a claim with no owner:
// it is the claimer, the claimer's child, or the claimer is gone.
func claimerLetGo(meta Meta) bool {
	switch c := meta.ClaimPID; {
	case c == 0, c == os.Getpid(), c == os.Getppid():
		return true
	default:
		return !processAlive(c, meta.ClaimStart)
	}
}

// doneTaken reports a name whose done/ entries already exist (a reused
// number); running it would append to another run's log.
func (d *Desk) doneTaken(name string) bool {
	for _, ext := range [...]string{extLog, extEvents, extBatchDir, extScript, extManifest} {
		if d.exists(path.Join(DirDone, name+ext)) {
			return true
		}
	}
	return false
}

func (d *Desk) createLog(name string) (*os.File, error) {
	f, err := d.root.OpenFile(path.Join(DirDone, name+extLog), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, fileMode)
	if err != nil {
		return nil, fmt.Errorf("desk: create log for %s: %w", describe(name), err)
	}
	return f, nil
}

func (d *Desk) superviseScript(run *Run, data []byte, home string, sigs <-chan os.Signal) (int, error) {
	name := run.Name
	logF, err := d.createLog(name)
	if err != nil {
		return 2, errors.Join(err, run.Finish(2, "internal"))
	}
	// bash reads the verified bytes from a pipe on fd 3, never from
	// running/<name>.sh: that file is the record of what ran, and a write to
	// it (or a rename over it) after the hash check cannot reach the run.
	// Its own process group, so a forwarded signal reaches everything the
	// script started. stdin is /dev/null: a detached item never prompts.
	cmd := exec.CommandContext(context.Background(), "/bin/bash", ScriptFDPath) //nolint:gosec // G204: constant argv; the script arrives on fd 3
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logF, logF
	cmd.Dir, cmd.Env = home, ChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	release, err := AttachScript(cmd, data)
	if err != nil {
		_ = logF.Close()
		return 2, errors.Join(err, run.Finish(2, "internal"))
	}
	startErr := cmd.Start()
	release() // the child holds its own copy of the read end
	if startErr != nil {
		_, _ = fmt.Fprintf(logF, "desk: could not start: %v\n", startErr)
		_ = logF.Close()
		return 127, run.Finish(127, stepFailed)
	}
	pgid := cmd.Process.Pid
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // the status is read from cmd.ProcessState below
		close(waited)
	}()
	interrupted := false
	var kill <-chan time.Time
	for running := true; running; {
		select {
		case <-sigs:
			if interrupted {
				// A second signal does not re-arm the grace period; it ends it.
				signalGroup(pgid, unix.SIGKILL)
				continue
			}
			interrupted = true
			signalGroup(pgid, unix.SIGTERM)
			kill = time.After(DefaultGrace)
		case <-kill:
			signalGroup(pgid, unix.SIGKILL)
		case <-waited:
			running = false
		}
	}
	// Nothing the script started may outlive its run: a leftover writing to
	// the log after EXIT would muddle the record.
	d.EndGroup(pgid)
	_ = logF.Close()
	rc := exitCode(cmd.ProcessState)
	reason := stepOK
	switch {
	case interrupted:
		reason = "interrupted"
	case rc != 0:
		reason = stepFailed
	}
	return rc, run.Finish(rc, reason)
}

// EndGroup sends SIGTERM to a finished script's process group if anything
// is left in it, and SIGKILL to whatever survives the grace period.
//
// Every signal that matters is preceded by a signal-0 probe, and the moment a
// probe finds the group empty it is never signalled again: its pgid may then
// belong to a new group.
func (d *Desk) EndGroup(pgid int) {
	if !signalGroup(pgid, 0) || !signalGroup(pgid, unix.SIGTERM) {
		return
	}
	deadline := time.Now().Add(d.grace)
	for time.Now().Before(deadline) {
		time.Sleep(batchTick)
		if !signalGroup(pgid, 0) {
			return
		}
	}
	signalGroup(pgid, unix.SIGKILL) // the last probe, just above, found it alive
}

// superviseBatch runs a batch whose manifest was parsed before the run began;
// perr is the parse error, recorded as the run's failure.
func (d *Desk) superviseBatch(run *Run, m *Manifest, perr error, home string, sigs <-chan os.Signal) (int, error) {
	name := run.Name
	nothing := BatchResult{RC: 2}.Fields()
	logF, err := d.createLog(name)
	if err != nil {
		return 2, errors.Join(err, run.Finish(2, "internal", nothing...))
	}
	defer logF.Close() //nolint:errcheck // every write below reports through the batch
	if perr != nil {
		_, _ = fmt.Fprintf(logF, "run-batch: error: %v\n", perr)
		return 2, run.Finish(2, "manifest", nothing...)
	}
	dir := path.Join(DirDone, name+extBatchDir)
	if err := d.root.Mkdir(dir, dirMode); err != nil {
		_, _ = fmt.Fprintf(logF, "run-batch: error: %v\n", err)
		return 2, run.Finish(2, "id-reused", nothing...)
	}
	b := NewBatch(m, BatchOptions{
		ID:       name,
		Dir:      d.abs(dir),
		EventDir: dir, // log= in STEP-END, relative to the desk dir
		WorkDir:  home,
		Env:      ChildEnv(),
		Log:      logF,
		Event: func(line string) {
			_ = run.Events.Emit(line) // the combined log carries the same line
		},
	})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-sigs:
				b.Interrupt()
			case <-stop:
				return
			}
		}
	}()
	res := b.Run()
	return res.RC, run.Finish(res.RC, res.Reason, res.Fields()...)
}
