package resume

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// HooksAgentLabel is the launchd label of the update-hooks watcher.
const HooksAgentLabel = "local.forgectl.resume-hooks"

// agentSystemPath is launchd's own default PATH for an agent. The install
// puts the directories the hooks need ahead of it.
var agentSystemPath = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// AgentSpec is everything the watcher's plist says.
type AgentSpec struct {
	Label string
	// Program is the absolute forgectl path launchd runs.
	Program string
	// Args follow Program: "resume", "hooks", "run".
	Args []string
	// WatchPaths start a run when any of them changes.
	WatchPaths []string
	// Env is the job's whole environment beyond what launchd itself sets
	// (HOME, USER, and a minimal PATH, which Env's PATH replaces).
	Env map[string]string
	// WorkingDir is set because launchd otherwise starts a job in /.
	WorkingDir string
	// LogPath receives stdout and stderr.
	LogPath string
	// Interval, when positive, also runs the job every Interval seconds
	// (StartInterval): a safety net for a missed watch event, and what
	// retries an incomplete restart when nothing else changes.
	Interval int
}

var (
	agentLabelRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	agentEnvKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Validate refuses a spec launchd would misread or refuse: a relative path
// (launchd resolves nothing against a cwd), a control character (XML 1.0
// cannot carry most of them), or an odd label or env key.
// agentExitTimeout is the plist's ExitTimeOut, in seconds: longer than a
// restart's stop (15s), shell-ready (10s) and confirm (30s) waits together,
// so a bootout's SIGKILL cannot land between a session's stop and relaunch.
const agentExitTimeout = 90

func (s AgentSpec) Validate() error {
	if !agentLabelRe.MatchString(s.Label) {
		return fmt.Errorf("agent label %s is not a reverse-DNS name", termsafe.QuoteArgMax(s.Label, 0))
	}
	for name, p := range map[string]string{"program": s.Program, "working directory": s.WorkingDir, "log path": s.LogPath} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("agent %s %s is not an absolute path", name, termsafe.QuotePath(p))
		}
	}
	if len(s.WatchPaths) == 0 {
		return errors.New("agent has no path to watch")
	}
	for _, p := range s.WatchPaths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("agent watch path %s is not absolute", termsafe.QuotePath(p))
		}
	}
	strs := append([]string{s.Label, s.Program, s.WorkingDir, s.LogPath}, s.Args...)
	strs = append(strs, s.WatchPaths...)
	for k, v := range s.Env {
		if !agentEnvKeyRe.MatchString(k) {
			return fmt.Errorf("agent environment key %s is not a variable name", termsafe.QuoteArgMax(k, 0))
		}
		strs = append(strs, v)
	}
	for _, v := range strs {
		if strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return fmt.Errorf("agent value %s holds a control character", termsafe.QuoteArgMax(v, 0))
		}
	}
	return nil
}

// RenderAgentPlist renders the watcher's launchd plist. It is pure and
// deterministic (env keys sorted), so an unchanged spec renders the same
// bytes and install can tell "already installed" from "changed". It indents
// with spaces so the text survives terminal-safe printing unchanged.
//
// RunAtLoad is on: a load (install, or login) runs once, which records the
// baseline right away — otherwise the first update after install would only
// record one and fire nothing — and catches an update that landed while the
// agent was not loaded.
func RenderAgentPlist(s AgentSpec) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { b.WriteString("  <key>"); esc(&b, k); b.WriteString("</key>\n") }
	str := func(indent, v string) { b.WriteString(indent + "<string>"); esc(&b, v); b.WriteString("</string>\n") }

	key("Label")
	str("  ", s.Label)
	key("ProgramArguments")
	b.WriteString("  <array>\n")
	for _, a := range append([]string{s.Program}, s.Args...) {
		str("    ", a)
	}
	b.WriteString("  </array>\n")
	key("WatchPaths")
	b.WriteString("  <array>\n")
	for _, p := range s.WatchPaths {
		str("    ", p)
	}
	b.WriteString("  </array>\n")
	if len(s.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("  <dict>\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			b.WriteString("    <key>")
			esc(&b, k)
			b.WriteString("</key>\n")
			str("    ", s.Env[k])
		}
		b.WriteString("  </dict>\n")
	}
	key("WorkingDirectory")
	str("  ", s.WorkingDir)
	key("StandardOutPath")
	str("  ", s.LogPath)
	key("StandardErrorPath")
	str("  ", s.LogPath)
	key("RunAtLoad")
	b.WriteString("  <true/>\n")
	if s.Interval > 0 {
		key("StartInterval")
		b.WriteString("  <integer>" + strconv.Itoa(s.Interval) + "</integer>\n")
	}
	// launchd's default 20s between SIGTERM and SIGKILL is shorter than a
	// restart's stop-and-relaunch window; a kill inside it strands a session.
	key("ExitTimeOut")
	b.WriteString("  <integer>" + strconv.Itoa(agentExitTimeout) + "</integer>\n")
	key("ProcessType")
	str("  ", "Background")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func esc(b *bytes.Buffer, s string) {
	// EscapeText into a bytes.Buffer cannot fail.
	_ = xml.EscapeText(b, []byte(s))
}

// AgentPATH is the PATH the watcher runs with: dirs (deduplicated, absolute
// only, in order) ahead of launchd's default. The restart action runs herdr
// by name and a command hook resolves its program the same way, so the
// directories they live in must be on it; launchd's default has none of
// Homebrew or ~/.local/bin.
func AgentPATH(dirs ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, d := range append(dirs, agentSystemPath...) {
		d = filepath.Clean(d)
		if d == "" || !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, ":")
}

// CheckAgentBinary refuses a forgectl path the agent cannot keep running: a
// relative one, or a `go run`/`go test` build, which lives in a go-build
// temp directory deleted when that process exits — the same rule
// RelaunchBinary applies. Unlike RelaunchBinary it does not fall back to the
// forgectl on PATH: install names the binary it wires in, and a silent swap
// would install a different build than the one the operator ran.
func CheckAgentBinary(exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("forgectl path %s is not absolute", termsafe.QuotePath(exe))
	}
	if strings.Contains(filepath.ToSlash(exe), "/go-build") {
		return fmt.Errorf("refusing to install %s: it is a temporary `go run` build that is deleted when it exits; install forgectl (or `go build` it somewhere permanent) and run install from that binary", termsafe.QuotePath(exe))
	}
	return nil
}

// CheckVersionsLink reports why path is not a symlink into a versions
// directory, or nil when it is. The watcher watches that link: a wrapper
// script or a plain binary never changes on update, so the watch would fire
// only at load and on the interval.
func CheckVersionsLink(path string) error {
	_, err := versionFromLink(path)
	return err
}

// AgentState is what `launchctl print` says about a loaded job.
type AgentState struct {
	Loaded   bool
	State    string // "running", "not running", …; "" when unknown
	Runs     int    // -1 when unknown
	LastExit string // launchd's text, e.g. "0" or "(never exited)"; "" when unknown
}

// parseLaunchctlPrint reads the fields status reports from `launchctl print
// gui/<uid>/<label>` output. The format is launchd's human output, not an
// interface, so every field is optional and an unknown shape reads as
// unknown rather than as an error.
func parseLaunchctlPrint(out string) AgentState {
	st := AgentState{Loaded: true, Runs: -1}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if st.State == "" {
				st.State = termsafe.SafeLineMax(v, 40)
			}
		case "runs":
			if n, err := strconv.Atoi(v); err == nil && st.Runs < 0 {
				st.Runs = n
			}
		case "last exit code":
			if st.LastExit == "" {
				st.LastExit = termsafe.SafeLineMax(v, 40)
			}
		}
	}
	return st
}

// Launchctl drives launchctl for one user's gui domain. Every call goes
// through Runner, so a test's fake runner sees them and nothing reaches the
// real launchd.
type Launchctl struct {
	Runner exec.Runner
	UID    int
	// Sleep waits between bootstrap attempts; nil means SleepContext.
	Sleep func(context.Context, time.Duration) error
}

func (l Launchctl) domain() string { return "gui/" + strconv.Itoa(l.UID) }

// Print reads a job's state. Loaded is false when launchctl cannot find it.
func (l Launchctl) Print(ctx context.Context, label string) AgentState {
	out, err := l.Runner.Run(ctx, "launchctl", "print", l.domain()+"/"+label)
	if err != nil {
		return AgentState{Runs: -1}
	}
	return parseLaunchctlPrint(out)
}

// bootstrapAttempts bounds Bootstrap's retries. Right after a bootout,
// launchd often refuses a bootstrap ("5: Input/output error") while the old
// instance is still tearing down; a short wait clears it.
const bootstrapAttempts = 4

// Bootstrap loads a plist into the gui domain, retrying a refusal a few
// times with a growing wait.
func (l Launchctl) Bootstrap(ctx context.Context, plist string) error {
	sleep := l.Sleep
	if sleep == nil {
		sleep = SleepContext
	}
	var err error
	for attempt := 1; attempt <= bootstrapAttempts; attempt++ {
		if _, err = l.Runner.Run(ctx, "launchctl", "bootstrap", l.domain(), plist); err == nil {
			return nil
		}
		if attempt < bootstrapAttempts {
			if serr := sleep(ctx, time.Duration(attempt)*500*time.Millisecond); serr != nil {
				break
			}
		}
	}
	return fmt.Errorf("launchctl bootstrap: %w", err)
}

// Bootout unloads a job from the gui domain.
func (l Launchctl) Bootout(ctx context.Context, label string) error {
	if _, err := l.Runner.Run(ctx, "launchctl", "bootout", l.domain()+"/"+label); err != nil {
		return fmt.Errorf("launchctl bootout: %w", err)
	}
	return nil
}

// AgentPlistPath is where the watcher's plist lives in agentsDir.
func AgentPlistPath(agentsDir, label string) string {
	return filepath.Join(agentsDir, label+".plist")
}

// InstallOutcome says what InstallAgent did.
type InstallOutcome string

const (
	InstallCreated   InstallOutcome = "installed"
	InstallUpdated   InstallOutcome = "updated"
	InstallUnchanged InstallOutcome = "already installed"
)

// InstallAgent loads the watcher from a plist in agentsDir. It is
// idempotent: the same spec, already loaded, changes nothing.
//
// A loaded job is booted out BEFORE the new plist is written. The other
// order can strand a stale job: the new bytes land, the bootout fails, and
// the next install finds the file matching and the job loaded, and reports
// "already installed" over a job still running the old arguments. Here a
// failed bootout leaves the old file, so the next install sees a difference
// and tries again.
func InstallAgent(ctx context.Context, agentsDir string, spec AgentSpec, lc Launchctl) (InstallOutcome, error) {
	data, err := RenderAgentPlist(spec)
	if err != nil {
		return "", err
	}
	path := AgentPlistPath(agentsDir, spec.Label)
	existing, readErr := os.ReadFile(path) // #nosec G304 -- label-derived name under the LaunchAgents directory
	loaded := lc.Print(ctx, spec.Label).Loaded
	outcome := InstallCreated
	switch {
	case readErr == nil && bytes.Equal(existing, data) && loaded:
		return InstallUnchanged, nil
	case readErr == nil:
		outcome = InstallUpdated
	case !errors.Is(readErr, fs.ErrNotExist):
		return "", fmt.Errorf("read %s: %w", termsafe.QuotePath(path), termsafe.Error(readErr))
	}
	if err := os.MkdirAll(spec.WorkingDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", termsafe.QuotePath(spec.WorkingDir), termsafe.Error(err))
	}
	if loaded {
		if err := lc.Bootout(ctx, spec.Label); err != nil {
			return "", err
		}
	}
	if err := writeFileAtomic(agentsDir, filepath.Base(path), data, 0o644, 0o755); err != nil { // #nosec G301 -- ~/Library/LaunchAgents is conventionally 0755
		return "", err
	}
	if err := lc.Bootstrap(ctx, path); err != nil {
		return "", err
	}
	return outcome, nil
}

// UninstallAgent unloads the job and removes its plist. Nothing to remove is
// success, so it is safe to repeat; the bool reports whether anything was.
func UninstallAgent(ctx context.Context, agentsDir, label string, lc Launchctl) (bool, error) {
	did := false
	if lc.Print(ctx, label).Loaded {
		if err := lc.Bootout(ctx, label); err != nil {
			return false, err
		}
		did = true
	}
	path := AgentPlistPath(agentsDir, label)
	switch err := os.Remove(path); {
	case err == nil:
		did = true
	case !errors.Is(err, fs.ErrNotExist):
		return did, fmt.Errorf("remove %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	return did, nil
}

// AgentStatus is what `resume hooks status` and the doctor row report.
type AgentStatus struct {
	PlistPath string
	Installed bool // the plist exists
	AgentState
}

// ReadAgentStatus probes the plist and launchd, reading only.
func ReadAgentStatus(ctx context.Context, agentsDir, label string, lc Launchctl) AgentStatus {
	path := AgentPlistPath(agentsDir, label)
	_, err := os.Lstat(path)
	return AgentStatus{PlistPath: path, Installed: err == nil, AgentState: lc.Print(ctx, label)}
}

// writeFileAtomic writes dir/name through a temp file, an fsync, and a
// rename, so a crash or power loss leaves the old file or the new one, never
// a truncated one (launchd refuses an empty plist at login).
func writeFileAtomic(dir, name string, data []byte, perm, dirPerm os.FileMode) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", termsafe.QuotePath(dir), termsafe.Error(err))
	}
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temp file in %s: %w", termsafe.QuotePath(dir), termsafe.Error(err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // gone after a successful rename; this only cleans up a failure
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return termsafe.Error(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close() // the chmod error is the one worth reporting
		return termsafe.Error(err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one worth reporting
		return termsafe.Error(err)
	}
	if err := tmp.Close(); err != nil {
		return termsafe.Error(err)
	}
	return termsafe.Error(os.Rename(tmpName, filepath.Join(dir, name)))
}
