//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/procstart"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// drainSpawn starts `forgectl <args>` detached, as desk.Launch starts its
// supervisor: no parent context, a session of its own (Setsid), /dev/null on
// stdin, stdout and stderr, and env as given. It returns the child's pid.
// Tests replace it.
var drainSpawn = func(args, env []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate the forgectl binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("home directory: %w", err)
	}
	// context.Background on purpose: no caller's lifetime may end the drain.
	cmd := exec.CommandContext(context.Background(), exe, args...) //nolint:gosec // G204: re-executing this binary's own hidden subcommand
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // /dev/null
	cmd.Dir, cmd.Env = home, env
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Reap it if it exits while start still waits, so it never lingers as a
	// zombie that reads as alive.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// drainSignal sends SIGTERM. Tests replace it.
var drainSignal = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// runDrainProcess is `surface _drain`: take drain.lock, check the pinned
// session, then tick every interval until SIGTERM or SIGINT, finishing the
// current step first. drain.json says running or paused while it runs and
// stopped when it ends.
func runDrainProcess(ctx context.Context, deps module.Deps, session, herdrPath string) error {
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	lock, err := files.Lock()
	if errors.Is(err, worker.ErrDrainLocked) {
		return termsafe.Error(drainHolderError(files))
	}
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	defer lock.Close() //nolint:errcheck // exiting releases it anyway

	var stop atomic.Bool
	wake := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			stop.Store(true)
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()

	pid := os.Getpid()
	// 0 when unreadable; stop then refuses to signal, and the event says why.
	start, startErr := procstart.Of(pid)
	base := drain.Status{PID: pid, ProcessStart: start, HerdrSession: session, HerdrPath: herdrPath, StartedAt: time.Now().UTC()}
	var seq int64
	emit := func(e drain.Event) error {
		seq++
		e.Seq, e.TS = seq, time.Now().UTC()
		line, err := drain.EncodeEvent(e)
		if err != nil {
			return err
		}
		return files.AppendEvent(lock, line)
	}
	writeStatus := func(st drain.Status) error {
		st.Seq, st.LastTick = seq, time.Now().UTC()
		data, err := drain.EncodeStatus(st)
		if err != nil {
			return err
		}
		return files.WriteStatus(lock, data)
	}
	stopped := func(d *drainer, why error) error {
		st := base
		if d != nil {
			st = d.status(base, nil)
		}
		st.Status, st.PauseReason = drain.StatusStopped, ""
		if why != nil {
			st.Error = termsafe.SafeLineMax(why.Error(), maxDrainErrorLen)
		}
		_ = emit(drain.Event{Kind: drain.EventStop, Error: st.Error})
		return writeStatus(st)
	}

	if err := emit(drain.Event{Kind: drain.EventStart, State: session}); err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if startErr != nil {
		_ = emit(drain.Event{Kind: drain.EventError, Error: "this process's start time could not be read, so drain stop will refuse to signal it: " + startErr.Error()})
	}
	if err := drainSessionPin(session, herdrPath); err != nil {
		return WithExitCode(termsafe.Error(errors.Join(err, stopped(nil, err))), exitUsage)
	}
	dio, err := realDrainIO(deps, session, emit)
	if err != nil {
		return WithExitCode(termsafe.Error(errors.Join(err, stopped(nil, err))), exitUsage)
	}
	d := newDrainer(dio, session, stop.Load)
	d.loadSettings()
	if err := writeStatus(d.status(base, nil)); err != nil {
		return termsafe.Error(err)
	}
	for !stop.Load() {
		rows := d.tick(ctx)
		if err := writeStatus(d.status(base, rows)); err != nil {
			d.lastErr = "status: " + err.Error()
		}
		if stop.Load() {
			break
		}
		select {
		case <-time.After(d.settings.Interval):
		case <-wake:
		}
	}
	return stopped(d, nil)
}
