// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/meta"
)

// The desk runs all day, so an upgrade installed under it goes unnoticed and
// its fixes look missing (#1180). It checks the forgectl on PATH now and then
// and says, under the header, when that one is newer than itself.

const (
	// deskNewerEvery is how often the desk looks for a newer forgectl. It is
	// a timer, not a frame: a check can run the binary on PATH.
	deskNewerEvery = time.Minute
	// deskNewerTimeout bounds one check, the PATH binary's version run
	// included.
	deskNewerTimeout = 5 * time.Second
)

// deskNewerMsg is one check's answer: the newer version installed, or "".
type deskNewerMsg struct{ version string }

// newerCmd runs the desk's newer-binary check after delay (at once when
// delay is 0), or is nil when the desk has no check.
func (m deskModel) newerCmd(delay time.Duration) tea.Cmd {
	check := m.opts.NewerInstalled
	if check == nil {
		return nil
	}
	ctx := m.ctx
	run := func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, deskNewerTimeout)
		defer cancel()
		return deskNewerMsg{version: check(c)}
	}
	if delay <= 0 {
		return run
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return run() })
}

// installedNewer is the production check for a desk running version running:
// it reports the version of the forgectl on PATH when that is newer, and ""
// otherwise. It runs the binary (`version --json`) only when the file on PATH
// changed since its last answer. A failed run (a timeout on a slow first
// launch, a broken binary) is tried again for the same file, but no sooner
// than deskNewerRetry, so a broken binary is not run every minute and a
// passing failure does not hide an upgrade. A failed run, an unreadable
// file or no forgectl on PATH keeps the previous answer, so the line does not
// flicker while an upgrade replaces the file. Only version numbers are
// compared, not paths. A running version that is not a release (a dev
// build) has nothing to compare, so there is no check at all.
func installedNewer(running string) func(context.Context) string {
	cur, ok := parseVersion(running)
	if !ok {
		return nil
	}
	var seen, last, failed string
	var failedAt time.Time
	return func(ctx context.Context) string {
		path, err := osexec.LookPath(meta.AppName)
		if err != nil {
			return last
		}
		fi, err := os.Stat(path)
		if err != nil {
			return last
		}
		key := path + "\x00" + fi.ModTime().String() + "\x00" + strconv.FormatInt(fi.Size(), 10)
		if key == seen || (key == failed && time.Since(failedAt) < deskNewerRetry) {
			return last
		}
		installed, err := binaryVersion(ctx, path)
		if err != nil {
			failed, failedAt = key, time.Now()
			return last
		}
		seen, failed, last = key, "", ""
		if v, ok := parseVersion(installed); ok && versionLess(cur, v) {
			last = installed
			if i := strings.IndexByte(last, ' '); i >= 0 {
				last = last[:i] // "0.45.0 (abc1234)" shows as 0.45.0
			}
		}
		return last
	}
}

// deskNewerRetry is how long a failed version run waits before the same file
// is run again. A var, so a test can retry at once.
var deskNewerRetry = 10 * time.Minute

const (
	// binaryVersionMax caps what the version run may print: its JSON is a
	// few dozen bytes, and more than this is not a forgectl answering.
	binaryVersionMax = 4 << 10
	// binaryVersionWait is how long, after the run exits or its context
	// ends, the desk waits for its output pipe to close. A wrapper script
	// whose own child holds the pipe open would otherwise hold the check, and
	// with it the next one, past deskNewerTimeout.
	binaryVersionWait = time.Second
)

// errVersionTooLong is a version run that printed more than binaryVersionMax.
var errVersionTooLong = errors.New("version output too long")

// cappedBuffer keeps at most max bytes and notes any more.
type cappedBuffer struct {
	buf  []byte
	max  int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); len(p) > room {
		c.buf = append(c.buf, p[:max(room, 0)]...)
		c.over = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// binaryVersion asks the forgectl at path for its version, as `version
// --json` prints it ("0.45.0 (abc1234)" for a release with its commit).
func binaryVersion(ctx context.Context, path string) (string, error) {
	cmd := osexec.CommandContext(ctx, path, "version", "--json") //nolint:gosec // G204: the forgectl on PATH, the binary the operator would start next
	out := &cappedBuffer{max: binaryVersionMax}
	cmd.Stdout = out
	cmd.WaitDelay = binaryVersionWait
	setVersionProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		if !errors.Is(err, osexec.ErrWaitDelay) {
			return "", err
		}
		// The run exited cleanly, but a child it left behind still held
		// stdout. Stop that child too, and use the output if it is whole.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if out.over {
		return "", errVersionTooLong
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out.buf, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// parseVersion reads a release version from its leading [v]N.N.N token:
// "1.2.3", "v1.2.3", and "0.45.0 (abc1234)" as `version --json` prints a
// release with its commit. A pre-release or build suffix is ignored on
// purpose, so 0.46.0-rc.1 counts as 0.46.0: the notice is a hint to restart,
// and a release candidate installed over a release is newer to the operator.
// "dev" and anything else is not a version.
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, " -+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// versionLess reports whether a is an older release than b.
func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// setVersionProcessGroup starts the version run in a process group of its
// own and has the context's cancel kill that whole group, so a wrapper
// script's children go with it, not only the wrapper.
func setVersionProcessGroup(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
