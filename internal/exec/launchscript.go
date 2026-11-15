// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package exec

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
)

// A launch script carries one opaque shell command line to a terminal manager
// that records the command it is asked to run.
//
// tmux, cmux, herdr and hc are handed the bootstrap in their argv, and keep it
// no longer than the call. Rex keeps the command a block was started with,
// and reports what runs in a block to every client connected to the server,
// a server that can be reachable over a tailnet. So a Rex block is started
// with `/bin/sh <path>`, and the line itself waits in a file only the
// operator's uid can read, for the few milliseconds until sh has opened it.
//
// The script's first line removes the script, so the file is gone as soon as
// sh has it open, and the adapter can tell the line was read by the file
// being gone (Consumed). Remove is the backstop for every other path.
//
// What this does not close: a process running as the operator's uid can read
// the file in that window, and the trampoline the line starts keeps the same
// line in its argv for as long as it runs, as on every other backend. This
// keeps the line out of the manager, not out of the operator's own uid.

// launchScriptName is the script's name inside its private directory.
const launchScriptName = "launch.sh"

// launchScriptPrefix runs first: sh has the file open by then, so removing it
// loses nothing, and the next line replaces sh with the command. rm is named
// by absolute path because a manager may start the script with no PATH.
const launchScriptPrefix = "/bin/rm -f -- \"$0\"\nexec "

// ErrLaunchScript reports a launch script that could not be written. Its text
// never says what the line was.
var ErrLaunchScript = errors.New("exec: cannot write a private launch script")

// LaunchScript is one written script: a private directory and the file in
// it. Neither path is secret (both are random names under the base); the
// line is in the file and nowhere in this value.
type LaunchScript struct {
	dir  string
	path string
}

// launchDirOwnerOK is ownedByCurrentUser, a variable only so a test can drive
// the refusal through NewLaunchScript: os.MkdirTemp cannot produce a foreign
// owner.
var launchDirOwnerOK = ownedByCurrentUser

// NewLaunchScript writes line, which must be opaque, into a new script in a
// new private directory under base, which must be absolute.
//
// The directory comes from os.MkdirTemp, which creates it with mode 0700 and
// a name nobody chose; the file is created exclusively with mode 0600
// (sealed.WriteNewFile), so nothing already at the path, a symlink included,
// is written through. The directory's mode is read back, and anything wider
// than 0700, or not owned by the effective user, refuses, because a umask can
// only narrow the mode and anything else means the directory is not the one
// MkdirTemp made.
func NewLaunchScript(base string, line Arg) (LaunchScript, error) {
	if !line.Secret() || !line.v.Present() || !filepath.IsAbs(base) {
		return LaunchScript{}, ErrLaunchScript
	}
	dir, err := os.MkdirTemp(base, "forgectl-launch-")
	if err != nil {
		return LaunchScript{}, ErrLaunchScript
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&^0o700 != 0 || !launchDirOwnerOK(info) {
		_ = os.Remove(dir)
		return LaunchScript{}, ErrLaunchScript
	}
	path := filepath.Join(dir, launchScriptName)
	if err := sealed.WriteNewFile(path, launchScriptPrefix, line.v, "\n"); err != nil {
		_ = os.Remove(dir)
		return LaunchScript{}, ErrLaunchScript
	}
	return LaunchScript{dir: dir, path: path}, nil
}

// Path is the script's absolute path, the operand to give /bin/sh.
func (s LaunchScript) Path() string { return s.path }

// Dir is the script's private directory.
func (s LaunchScript) Dir() string { return s.dir }

// Consumed reports whether the script has removed itself, which it does only
// once sh has opened it. An error is a stat that could not say.
func (s LaunchScript) Consumed() (bool, error) {
	_, err := os.Lstat(s.path)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	default:
		return false, ErrLaunchScript
	}
}

// Remove deletes the script, when it is still there, and its directory. It is
// safe to repeat; a script that already removed itself leaves only the
// directory.
func (s LaunchScript) Remove() error {
	if s.dir == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ErrLaunchScript
	}
	if err := os.Remove(s.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ErrLaunchScript
	}
	return nil
}
