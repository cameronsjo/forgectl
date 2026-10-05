// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// ErrPaneGone means the terminal went away during a foreground run: the run
// was finished and recorded, and there is no screen to go back to.
var ErrPaneGone = errors.New("desk: the terminal is gone")

// IO is the terminal a foreground run uses.
//
// Stdin, Stdout and Stderr are the item's; nil is /dev/null. Out is where the
// closing "ran …" line goes; nil means Stdout.
//
// TTY is the controlling terminal's descriptor, or -1 for none. Only with a
// terminal does the item's process group get the terminal's foreground, does
// the desk take the foreground back after it, and does the closing line wait
// for a key.
//
// Signals, when set, replaces the INT, TERM, HUP and QUIT subscription; each
// value is forwarded to the item's process group. Grace is how long the group
// gets after the first forwarded signal before SIGKILL; zero means
// desk.DefaultGrace. SetForeground and WaitKey replace the terminal
// operations; nil means the real ones.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Out            io.Writer
	TTY            int
	Signals        <-chan os.Signal
	Grace          time.Duration
	SetForeground  func(tty, pgid int) error
	WaitKey        func() error
}

// afterForegroundClaim runs once a foreground claim has succeeded, before the
// run begins. Tests use it to rewrite the record under a claimed item.
var afterForegroundClaim = func(*desk.Claimed) {}

// runForeground runs the queued TTY item name, approved with the full hash
// sha, in this terminal, and returns its exit code.
//
// It claims the item with sha, records this process as the run's owner, and
// runs the claimed bytes through bash on fd 3, never the file in running/, in
// a process group of its own that gets the terminal's foreground. INT, TERM,
// HUP and QUIT are forwarded to that group, with SIGKILL after the grace
// period. SIGTSTP, SIGTTIN and SIGTTOU are ignored in the item's group (an
// ignored signal stays ignored across exec): Ctrl-Z would otherwise stop the
// group while the desk waits for it, with no terminal to resume it from.
// When the item ends, anything it left in its group is ended, the desk takes
// the terminal back, finishes the run (ok, failed, or interrupted for a
// forwarded signal or an item killed by INT, TERM, HUP or QUIT), and prints
// the result. A claim that cannot start a run is given back as
// skipped:launch-failed, and an item that cannot start is an error. A
// terminal that cannot be taken back is a closed pane: the run is finished
// and an error matching ErrPaneGone returned.
func runForeground(d *desk.Desk, name, sha string, tio IO) (rc int, err error) {
	home, err := desk.HomeDir()
	if err != nil {
		return 2, err
	}
	claimed, err := d.Claim(name, sha)
	if err != nil {
		return 2, err
	}
	afterForegroundClaim(claimed)
	switch {
	case claimed.SHA256 != sha:
		return 2, errors.Join(fmt.Errorf("desk: the claim for %s does not carry the approved hash", safeLabel(name)),
			d.Abandon(name, desk.SkipLaunchFailed))
	case claimed.Kind != desk.KindScript || !claimed.TTY:
		return 2, errors.Join(fmt.Errorf("desk: %s is not a TTY script; it does not run in this terminal", safeLabel(name)),
			d.Abandon(name, desk.SkipLaunchFailed))
	}
	run, err := d.BeginRun(name, os.Getpid())
	if err != nil {
		return 2, errors.Join(err, d.Abandon(name, desk.SkipLaunchFailed))
	}

	// The operand matters: with none, bash would read the terminal instead
	// of fd 3.
	cmd := exec.CommandContext(context.Background(), "/bin/bash", desk.ScriptFDPath) //nolint:gosec // G204: constant argv; the script arrives on fd 3
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tio.Stdin, tio.Stdout, tio.Stderr
	cmd.Dir, cmd.Env = home, desk.ChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if tio.TTY >= 0 {
		// Ctty is this process's descriptor for the terminal: Go makes the
		// child's group the foreground before it execs.
		cmd.SysProcAttr.Foreground, cmd.SysProcAttr.Ctty = true, tio.TTY
	}
	release, err := claimed.AttachScript(cmd)
	if err != nil {
		return 2, errors.Join(err, run.Finish(2, "internal"))
	}

	sigs := tio.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT)
		defer signal.Stop(ch)
		sigs = ch
	}
	// The child inherits these as ignored; the desk resets its own at once.
	stops := []os.Signal{unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU}
	signal.Ignore(stops...)
	startErr := cmd.Start()
	signal.Reset(stops...)
	release() // the child holds its own copy of the read end
	if startErr != nil {
		return 127, errors.Join(fmt.Errorf("desk: could not start %s: %w", safeLabel(name), startErr), run.Finish(127, "failed"))
	}
	pgid := cmd.Process.Pid
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // the status is read from cmd.ProcessState below
		close(waited)
	}()
	grace := tio.Grace
	if grace <= 0 {
		grace = desk.DefaultGrace
	}
	interrupted := false
	var kill <-chan time.Time
	for running := true; running; {
		select {
		case s := <-sigs:
			sig, ok := s.(syscall.Signal)
			if !ok {
				sig = unix.SIGTERM
			}
			_ = unix.Kill(-pgid, sig)
			if !interrupted {
				interrupted = true
				kill = time.After(grace)
			}
		case <-kill:
			_ = unix.Kill(-pgid, unix.SIGKILL)
		case <-waited:
			running = false
		}
	}
	rc = foregroundExit(cmd.ProcessState)
	interrupted = interrupted || killedByInterrupt(cmd.ProcessState)
	// Nothing the item started may keep the terminal, or outlive a run
	// recorded as ended.
	d.EndGroup(pgid)

	var termErr error
	if tio.TTY >= 0 {
		termErr = takeTerminal(tio)
	}
	reason := "ok"
	switch {
	case interrupted:
		reason = "interrupted"
	case rc != 0:
		reason = "failed"
	}
	if err := run.Finish(rc, reason); err != nil {
		return rc, err
	}
	switch {
	case errors.Is(termErr, unix.EIO):
		return rc, ErrPaneGone
	case termErr != nil:
		// Without the foreground the desk cannot read or draw the terminal:
		// a background group that tries is stopped.
		return rc, fmt.Errorf("%w: could not take the terminal back: %w", ErrPaneGone, termErr)
	}
	out := tio.Out
	if out == nil {
		out = tio.Stdout
	}
	if out != nil {
		_, _ = fmt.Fprintf(out, "\nran %s: exit %d (%s) — press any key\n", safeLabel(name), rc, exitMeaning(rc, interrupted))
	}
	if tio.TTY >= 0 {
		wait := tio.WaitKey
		if wait == nil {
			wait = func() error { return waitKey(tio.TTY) }
		}
		if err := wait(); errors.Is(err, unix.EIO) {
			return rc, ErrPaneGone
		}
	}
	return rc, nil
}

// takeTerminal gives the terminal's foreground back to this process's group.
// A background group that calls tcsetpgrp is sent SIGTTOU, which stops it, so
// the signal is ignored around the call.
func takeTerminal(tio IO) error {
	set := tio.SetForeground
	if set == nil {
		set = func(tty, pgid int) error { return unix.IoctlSetPointerInt(tty, unix.TIOCSPGRP, pgid) }
	}
	signal.Ignore(unix.SIGTTOU)
	defer signal.Reset(unix.SIGTTOU)
	return set(tio.TTY, unix.Getpgrp())
}

// waitKey reads one key from the terminal, in raw mode so any key counts.
func waitKey(tty int) error {
	old, err := term.MakeRaw(tty)
	if err != nil {
		return err
	}
	defer term.Restore(tty, old) //nolint:errcheck // best effort: the dashboard sets its own mode next
	var b [1]byte
	_, err = unix.Read(tty, b[:])
	return err
}

// killedByInterrupt reports an item killed by a signal the terminal or the
// operator sends to stop it: INT (Ctrl-C reaches the item's group, not the
// desk), TERM, HUP or QUIT.
func killedByInterrupt(ps *os.ProcessState) bool {
	if ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	switch ws.Signal() {
	case unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT:
		return true
	}
	return false
}

// foregroundExit is the item's exit code: a signal death is 128 plus the
// signal, as a shell reports it.
func foregroundExit(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// exitMeaning is a word for an exit code.
func exitMeaning(rc int, interrupted bool) string {
	switch {
	case rc == 0:
		return "ok"
	case interrupted:
		return "interrupted"
	case rc == 126:
		return "could not execute"
	case rc == 127:
		return "command not found"
	case rc > 128 && rc < 128+65:
		return "killed by " + unix.SignalName(syscall.Signal(rc-128))
	}
	return "failed"
}
