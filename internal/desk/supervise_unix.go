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
)

// supervisorArgv is the command line that runs `forgectl desk _supervise`.
// Tests replace it to re-execute the test binary.
var supervisorArgv = func(dir, name string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("desk: locate the forgectl binary: %w", err)
	}
	return []string{exe, "desk", "_supervise", "--dir", dir, name}, nil
}

// Launch starts the detached supervisor that runs a claimed item, and
// returns its pid. TTY items are refused: they run in the desk's own
// foreground (see [Desk.BeginRun]).
//
// The supervisor must outlive the desk, so it is started with no parent
// context and in a session of its own (Setsid): closing the desk's pane
// signals the desk's process group and session, and neither reaches it. It
// deliberately does not use internal/exec's process-group runner, which kills
// the group when its context ends — the opposite of what is wanted here.
func (d *Desk) Launch(name string) (int, error) {
	kind, err := d.findKind(DirRunning, name)
	if err != nil {
		return 0, fmt.Errorf("desk: launch %s: claim it first: %w", describe(name), err)
	}
	if kind == KindScript {
		data, err := d.root.ReadFile(path.Join(DirRunning, name+extScript))
		if err != nil {
			return 0, fmt.Errorf("desk: launch %s: %w", describe(name), err)
		}
		if ParseHeaders(data).TTY {
			return 0, fmt.Errorf("desk: %s is a TTY item; it runs in the desk's foreground, not detached", describe(name))
		}
	}
	argv, err := supervisorArgv(d.path, name)
	if err != nil {
		return 0, err
	}
	// context.Background on purpose: no caller's lifetime may end this run.
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // G204: re-executing this binary's own hidden subcommand
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // /dev/null
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("desk: start supervisor for %s: %w", describe(name), err)
	}
	pid := cmd.Process.Pid
	// Reap it whenever it ends, so a long-lived desk never holds a zombie
	// that a liveness check would read as alive.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// RunSupervisor is `forgectl desk _supervise`: it runs one claimed item in
// running/ to completion and returns the process exit code (the item's rc, or
// 2 when the supervisor itself could not run it).
func RunSupervisor(dir, name string) int {
	signal.Ignore(syscall.SIGHUP)
	d, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "desk _supervise:", err)
		return 2
	}
	defer d.Close() //nolint:errcheck // exiting
	rc, err := d.supervise(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "desk _supervise:", err)
		if rc == 0 {
			rc = 2
		}
	}
	return rc
}

func (d *Desk) supervise(name string) (int, error) {
	kind, err := d.findKind(DirRunning, name)
	if err != nil {
		return 2, err
	}
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		return 2, err
	}
	data, _, err := d.readItem(DirRunning, name+kind.Ext())
	if err != nil {
		return 2, err
	}
	if SHA256Hex(data) != meta.SHA256 {
		if err := d.skipChanged(DirRunning, name, kind, meta); err != nil {
			return 2, err
		}
		return 2, fmt.Errorf("%w: %s", ErrChanged, describe(name))
	}
	if kind == KindScript && ParseHeaders(data).TTY {
		return 2, fmt.Errorf("desk: %s is a TTY item; it runs in the desk's foreground", describe(name))
	}
	if d.doneTaken(name) {
		meta.SkipReason = SkipReused
		_ = d.writeMeta(DirRunning, name, meta)
		_ = d.move(name, kind, DirRunning, DirSkipped)
		return 2, fmt.Errorf("desk: done/ already holds %s; skipped", describe(name))
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if kind == KindBatch {
		return d.superviseBatch(name, data, sigs)
	}
	return d.superviseScript(name, sigs)
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

func (d *Desk) superviseScript(name string, sigs <-chan os.Signal) (int, error) {
	run, err := d.BeginRun(name, os.Getpid())
	if err != nil {
		return 2, err
	}
	logF, err := d.createLog(name)
	if err != nil {
		return 2, errors.Join(err, run.Finish(2, "internal"))
	}
	// Its own process group, so a forwarded signal reaches everything the
	// script started. stdin is /dev/null: a detached item never prompts.
	cmd := exec.CommandContext(context.Background(), "/bin/bash", d.abs(path.Join(DirRunning, name+extScript))) //nolint:gosec // G204: the verified copy of the approved item
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logF, logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(logF, "desk: could not start: %v\n", err)
		_ = logF.Close()
		return 127, run.Finish(127, stepFailed)
	}
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
			interrupted = true
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGTERM)
			kill = time.After(DefaultGrace)
		case <-kill:
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		case <-waited:
			running = false
		}
	}
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

func (d *Desk) superviseBatch(name string, data []byte, sigs <-chan os.Signal) (int, error) {
	m, perr := LoadManifest(data, name+extManifest)
	var fields []string
	if perr == nil {
		fields = []string{"steps=" + strconv.Itoa(len(m.Steps)), "jobs=" + strconv.Itoa(DefaultJobs)}
	}
	run, err := d.BeginRun(name, os.Getpid(), fields...)
	if err != nil {
		return 2, err
	}
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
		ID:  name,
		Dir: d.abs(dir),
		Log: logF,
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
