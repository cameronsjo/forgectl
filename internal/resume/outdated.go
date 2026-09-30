package resume

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// is known safe. "busy" covers a model turn and a running `!` command,
// "waiting" an open permission prompt, and "shell" a background shell that
// outlived its turn (a stop kills it). Any status Claude Code adds later is
// busy too: the safe reading of an unknown value is that work is happening.
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
// VersionUnparseable set. lookup may be nil. It fails only when installed
// itself does not parse.
func FindOutdated(entries []RegistryEntry, installed string, lookup PaneLookup) ([]OutdatedSession, error) {
	iv, err := ParseVersion(installed)
	if err != nil {
		return nil, fmt.Errorf("installed version: %w", err)
	}
	var out []OutdatedSession
	for _, e := range entries {
		if !e.Live {
			continue
		}
		unparseable := false
		if v, err := ParseVersion(e.Version); err != nil {
			unparseable = true
		} else if v.Compare(iv) >= 0 {
			continue
		}
		o := OutdatedSession{
			SessionID: e.SessionID, Pid: e.Pid, Cwd: e.Cwd,
			Status: e.Status, Busy: IsBusy(e.Status),
			Version: e.Version, InstalledVersion: installed,
			VersionUnparseable: unparseable,
		}
		if lookup != nil {
			o.Pane = lookup(e.Pid)
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// Outdated lists the live sessions behind installed.
func Outdated(p Paths, installed string, lookup PaneLookup) ([]OutdatedSession, error) {
	return FindOutdated(LiveEntries(p), installed, lookup)
}

// InstalledVersion resolves the installed harness version from binPath.
//
// When binPath is a symlink into a directory named "versions" — the native
// installer's ~/.local/bin/claude -> ~/.local/share/claude/versions/<X> — the
// target's basename is the version, and reading a link spawns nothing. Any
// other layout runs `<bin> --version`: a plain file's name is not evidence of
// its version. The two can disagree mid-update; the link wins because it names
// what the next launch will run. When neither yields a parseable version the
// error names both attempts.
func InstalledVersion(ctx context.Context, binPath string, run exec.Runner) (string, error) {
	version, linkErr := versionFromLink(binPath)
	if linkErr == nil {
		return version, nil
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

// versionFromLink returns the version named by binPath's symlink target when
// that target is versions/<parseable version>, and otherwise an error saying
// why not.
func versionFromLink(binPath string) (string, error) {
	fi, err := os.Lstat(binPath)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("not a symlink")
	}
	target, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		return "", fmt.Errorf("resolve symlink: %w", err)
	}
	if filepath.Base(filepath.Dir(target)) != "versions" {
		return "", fmt.Errorf("symlink target %q is not inside a versions directory", target)
	}
	name := filepath.Base(target)
	if _, err := ParseVersion(name); err != nil {
		return "", fmt.Errorf("symlink target %q does not name a version: %w", target, err)
	}
	return name, nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

const paneEnvKey = "HERDR_PANE_ID"

// processEnv is the environment seam: it returns a process's environment as
// KEY=VALUE entries. It is implemented per platform (outdated_env_*.go) and
// reads in-process — sysctl on macOS, /proc elsewhere — so the environment,
// which holds secrets, never passes through a subprocess's captured output.
var processEnv = readProcessEnv

// PaneFor returns a PaneLookup that reads HERDR_PANE_ID from a process's
// environment. Reading another process's environment is same-user only, so
// any failure — a foreign process, a vanished pid — yields "" rather than an
// error. Only the one variable is extracted; the rest is never kept or logged.
func PaneFor() PaneLookup {
	return func(pid int) string {
		if pid <= 0 {
			return ""
		}
		env, err := processEnv(pid)
		if err != nil {
			return ""
		}
		return paneFromEnv(env)
	}
}

// paneFromEnv returns the first HERDR_PANE_ID entry's value, as getenv would.
func paneFromEnv(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, paneEnvKey+"="); ok {
			return validPane(v)
		}
	}
	return ""
}

// parseProcArgs2 extracts the environment from a KERN_PROCARGS2 buffer:
// a native-endian int32 argc, the NUL-terminated exec path, NUL padding, argc
// NUL-terminated arguments, then the NUL-terminated environment. Keeping argv
// and the environment apart is the point — an argument that happens to read
// "HERDR_PANE_ID=x" is not the process's environment.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, errors.New("procargs2: buffer too short for argc")
	}
	n := binary.NativeEndian.Uint32(buf[:4])
	// Every argument costs at least its NUL, so a larger argc is corrupt.
	if int64(n) > int64(len(buf)) {
		return nil, fmt.Errorf("procargs2: argc %d exceeds the %d-byte buffer", n, len(buf))
	}
	argc := int(n)
	rest := buf[4:]
	// Exec path, then its NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("procargs2: unterminated exec path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, fmt.Errorf("procargs2: argument %d of %d is unterminated", n+1, argc)
		}
		rest = rest[i+1:]
	}
	var env []string
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i <= 0 { // an empty string ends the environment
			break
		}
		env = append(env, string(rest[:i]))
		rest = rest[i+1:]
	}
	return env, nil
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
