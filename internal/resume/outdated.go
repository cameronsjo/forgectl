package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Version is a dotted numeric release such as 2.1.285. It exists because the
// versions must compare per segment: as strings, 2.1.99 sorts after 2.1.100
// and a session that IS behind reads as current.
type Version []int

// ParseVersion accepts one or more dot-separated decimal segments and nothing
// else. Anything richer (a pre-release tag, a "v" prefix) is refused rather
// than guessed at: the caller reports an unparseable version instead of
// ranking it.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty version")
	}
	parts := strings.Split(s, ".")
	v := make(Version, len(parts))
	for i, p := range parts {
		// Atoi accepts a sign; a version segment never has one.
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return nil, fmt.Errorf("version %q: segment %q is not a decimal number", s, p)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("version %q: segment %q: %w", s, p, err)
		}
		v[i] = n
	}
	return v, nil
}

// Compare orders two versions numerically per segment, treating a missing
// trailing segment as zero (2.1 == 2.1.0). It returns -1, 0, or 1.
func (v Version) Compare(o Version) int {
	for i := 0; i < max(len(v), len(o)); i++ {
		var a, b int
		if i < len(v) {
			a = v[i]
		}
		if i < len(o) {
			b = o[i]
		}
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
	}
	return 0
}

// IsBusy reports whether a restart could destroy in-flight work. Only "idle"
// is known safe: "busy", "waiting" (a permission prompt is open), and "shell" (a shell
// command is running) are busy, and so is any status Claude Code adds later — the safe reading of a value this
// code has never seen is that work is happening.
func IsBusy(status string) bool { return status != "idle" }

// OutdatedSession is one live session running an older harness than the one
// installed.
type OutdatedSession struct {
	SessionID        string
	Pid              int
	Cwd              string
	Status           string
	Busy             bool
	Version          string
	InstalledVersion string
	// VersionUnparseable marks a session whose recorded version could not be
	// compared. It is listed, not dropped: nothing proves it current.
	VersionUnparseable bool
	// Pane is the HERDR_PANE_ID the session's process carries in its environment,
	// empty when unknown. It is a claim, not a verified pane: a nested claude
	// inherits its parent's id.
	Pane string
}

// PaneLookup returns the terminal pane id for a pid, or "" when unknown.
type PaneLookup func(pid int) string

// FindOutdated filters live registry entries to those older than installed,
// sorted by session id. An entry whose version does not parse is included with
// VersionUnparseable set. lookup may be nil.
func FindOutdated(entries []RegistryEntry, installed Version, installedText string, lookup PaneLookup) []OutdatedSession {
	var out []OutdatedSession
	for _, e := range entries {
		if !e.Live {
			continue
		}
		unparseable := false
		if v, err := ParseVersion(e.Version); err != nil {
			unparseable = true
		} else if v.Compare(installed) >= 0 {
			continue
		}
		o := OutdatedSession{
			SessionID: e.SessionID, Pid: e.Pid, Cwd: e.Cwd,
			Status: e.Status, Busy: IsBusy(e.Status),
			Version: e.Version, InstalledVersion: installedText,
			VersionUnparseable: unparseable,
		}
		if lookup != nil {
			o.Pane = lookup(e.Pid)
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// Outdated lists the live sessions behind installed.
func Outdated(p Paths, installed string, lookup PaneLookup) ([]OutdatedSession, error) {
	v, err := ParseVersion(installed)
	if err != nil {
		return nil, fmt.Errorf("installed version: %w", err)
	}
	return FindOutdated(LiveEntries(p), v, installed, lookup), nil
}

// evalSymlinks is the symlink seam for InstalledVersion.
var evalSymlinks = filepath.EvalSymlinks

// InstalledVersion resolves the installed harness version from binPath.
//
// The symlink target's basename is preferred — ~/.local/bin/claude points at
// versions/<X>, and reading a link spawns nothing. `<bin> --version` is the
// fallback, for a binary that is not a symlink into a versions directory. The
// two can disagree mid-update; the link wins because it names what the next
// launch will run. When neither yields a parseable version the error names
// both attempts.
func InstalledVersion(ctx context.Context, binPath string, run exec.Runner) (string, error) {
	var linkErr error
	target, err := evalSymlinks(binPath)
	if err == nil {
		name := filepath.Base(target)
		_, perr := ParseVersion(name)
		if perr == nil {
			return name, nil
		}
		linkErr = fmt.Errorf("symlink target %q does not name a version: %w", target, perr)
	} else {
		linkErr = fmt.Errorf("resolve symlink: %w", err)
	}

	out, err := run.Run(ctx, binPath, "--version")
	if err != nil {
		return "", fmt.Errorf("could not determine the installed version of %s: tried the symlink target (%v) and `--version` (%w)", binPath, linkErr, err)
	}
	// Output looks like "2.1.285 (Claude Code)".
	fields := strings.Fields(out)
	if len(fields) > 0 {
		if _, perr := ParseVersion(fields[0]); perr == nil {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("could not determine the installed version of %s: tried the symlink target (%v) and `--version`, whose output %q has no leading version", binPath, linkErr, truncateForError(out))
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

const paneEnvKey = "HERDR_PANE_ID"

// procEnviron is the Linux environment seam: it reads /proc/<pid>/environ.
var procEnviron = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ") // #nosec G304 -- fixed /proc path, pid is an int
}

// hostOS is the platform seam for PaneFor.
var hostOS = runtime.GOOS

// PaneFor returns a PaneLookup that reads HERDR_PANE_ID from a process's
// environment: `ps eww` on macOS, /proc/<pid>/environ elsewhere. Both are
// same-user only, so any failure — a foreign process, a vanished pid — yields
// "" rather than an error. Only the one variable is extracted; the rest of the
// environment (which holds secrets) is never kept or logged.
func PaneFor(ctx context.Context, run exec.Runner) PaneLookup {
	return func(pid int) string {
		if pid <= 0 {
			return ""
		}
		if hostOS == "darwin" {
			out, err := run.Run(ctx, "ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid))
			if err != nil {
				return ""
			}
			return paneFromPS(out)
		}
		data, err := procEnviron(pid)
		if err != nil {
			return ""
		}
		return paneFromEnviron(data)
	}
}

// paneFromEnviron extracts the pane id from NUL-separated KEY=VALUE pairs.
func paneFromEnviron(data []byte) string {
	for _, kv := range strings.Split(string(data), "\x00") {
		if v, ok := strings.CutPrefix(kv, paneEnvKey+"="); ok {
			return validPane(v)
		}
	}
	return ""
}

// paneFromPS extracts the pane id from `ps eww` output, where the command line
// is followed by space-separated KEY=VALUE pairs. A pane id has no spaces, so
// the value ends at the next whitespace.
func paneFromPS(out string) string {
	for _, tok := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(tok, paneEnvKey+"="); ok {
			return validPane(v)
		}
	}
	return ""
}

// validPane admits only a conservative id alphabet. The value comes from
// another process's environment and is printed to a terminal and into JSON;
// anything odd is treated as unknown rather than sanitized into a wrong id.
func validPane(v string) string {
	if v == "" || len(v) > 64 {
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return ""
		}
	}
	return v
}
