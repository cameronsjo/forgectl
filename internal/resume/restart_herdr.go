package resume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// processIdentity is the platform seam for ProcIdentity (procident_*.go).
var processIdentity = readProcessIdentity

// SystemRestartEnv is the production RestartEnv: the real registry, the
// kernel, SIGTERM, and herdr over its CLI. Build it with NewSystemRestartEnv;
// the zero value has no relaunch command and refuses in Prepare, before any
// signal.
//
// It drives herdr directly rather than through internal/surface's herdr
// adapter, and it types `forgectl resume <id>`, not a harness command line.
// The surface barrier exists so an adapter never sees the harness invocation;
// here herdr sees only forgectl's own resume verb, which resolves the launch
// profile and argv itself, in the pane.
type SystemRestartEnv struct {
	paths  Paths
	runner exec.Runner
	// forgectl is the absolute path typed into each pane.
	forgectl string
	// herdr is the herdr binary; "" runs `herdr` from PATH.
	herdr string
	// callTimeout bounds each herdr call; zero means HerdrCallTimeout.
	callTimeout time.Duration
}

// HerdrCallTimeout bounds every herdr call a restart run makes. Each call is
// one request to the local herdr server, answered well inside it, while a
// call still running at the bound is taken as wedged. The bound matters most
// after the signal: from there the run ignores cancellation, and each herdr
// call sits in a process group of its own, so neither Ctrl-C nor a hangup
// reaches it. Without the bound, a wedged call would hang the run forever
// with the session stopped; at the bound, the call's group is killed and the
// session is reported failed, with the command to resume it by hand; a
// relaunch killed there may have landed, so the run first waits for the
// session to register (forgectl#951).
const HerdrCallTimeout = 10 * time.Second

// herdrBin is the herdr binary every call runs.
func (e SystemRestartEnv) herdrBin() string {
	if e.herdr != "" {
		return e.herdr
	}
	return "herdr"
}

// runHerdr runs one herdr call in a process group of its own, bounded by
// HerdrCallTimeout. A restart run survives SIGHUP on purpose, so a closed
// terminal cannot strand a session between its stop and its relaunch; in the
// terminal's group, the herdr call in flight would still take the hangup and
// die (forgectl#877). herdr is non-interactive, so it never needs the
// terminal's foreground group. At the bound, or when ctx is cancelled, the
// call's whole group is killed; a call killed at the bound returns an error
// wrapping ErrHerdrTimeout.
func (e SystemRestartEnv) runHerdr(ctx context.Context, args ...string) (string, error) {
	limit := e.callTimeout
	if limit <= 0 {
		limit = HerdrCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := e.runner.Run(exec.WithProcessGroup(ctx), e.herdrBin(), args...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%w after %s: %w", ErrHerdrTimeout, limit, err)
	}
	return out, err
}

var _ RestartEnv = SystemRestartEnv{}

// NewSystemRestartEnv builds the production env. It refuses a forgectl path
// that is not absolute or cannot be shell-quoted identically in every shell a
// pane might run, so a bad path fails here, never after a session is stopped.
func NewSystemRestartEnv(paths Paths, runner exec.Runner, forgectl string) (SystemRestartEnv, error) {
	if runner == nil {
		return SystemRestartEnv{}, errors.New("no command runner")
	}
	e := SystemRestartEnv{paths: paths, runner: runner, forgectl: forgectl}
	// A syntactically valid placeholder id: the id itself is hex, so only the
	// path can make the line unrenderable.
	if _, err := e.relaunchLine("0"); err != nil {
		return SystemRestartEnv{}, err
	}
	return e, nil
}

// relaunchLine renders the shell line typed into a pane. `herdr pane run`
// joins its arguments with spaces and types them into the pane's shell
// (measured on herdr 0.9.1), so the line must arrive fully quoted — and the id
// is disk-sourced, so it is validated here, before it reaches any line.
func (e SystemRestartEnv) relaunchLine(sessionID string) (string, error) {
	if !validSessionID(sessionID) {
		return "", errors.New("refusing an invalid session id")
	}
	if e.forgectl == "" || !filepath.IsAbs(e.forgectl) {
		return "", errors.New("no absolute forgectl path to relaunch with")
	}
	line, err := surface.QuoteCommand([]string{e.forgectl, "resume", sessionID})
	if err != nil {
		return "", fmt.Errorf("render the relaunch line: %w", err)
	}
	return line, nil
}

// ReadEntry implements RestartEnv.
func (e SystemRestartEnv) ReadEntry(pid int) (RegistryEntry, bool) { return ReadEntry(e.paths, pid) }

// Alive implements RestartEnv.
func (e SystemRestartEnv) Alive(pid int) bool { return pidAlive(pid) }

// Identity implements RestartEnv.
func (e SystemRestartEnv) Identity(pid int) (ProcIdentity, error) { return processIdentity(pid) }

// Terminate implements RestartEnv.
func (e SystemRestartEnv) Terminate(pid int) error { return terminateProcess(pid) }

// LiveSession implements RestartEnv.
func (e SystemRestartEnv) LiveSession(sessionID string) (RegistryEntry, bool) {
	return LiveSession(e.paths, sessionID)
}

// Prepare implements RestartEnv. It is the last step allowed to refuse, so
// everything the relaunch needs is settled here, before the signal: the
// relaunch line renders, and the session is findable by `forgectl resume`
// once its registry file is gone.
//
// It runs the same capture the Stop hook runs at every turn end, so the
// resumed session gets its /rename name and task bodies back, then confirms
// the store holds the session: a session that arrived through /clear can be
// absent from history.jsonl, and once its registry file is gone the store is
// the only place `forgectl resume` could find it.
func (e SystemRestartEnv) Prepare(sessionID string) error {
	if _, err := e.relaunchLine(sessionID); err != nil {
		return err
	}
	res := Snapshot(e.paths, time.Now())
	if _, ok := Load(e.paths.StoreDir, sessionID); !ok {
		if len(res.Errs) > 0 {
			return fmt.Errorf("snapshot store has no record for it: %w", errors.Join(res.Errs...))
		}
		return errors.New("snapshot store has no record for it")
	}
	return nil
}

// Pane implements RestartEnv with two read-only herdr calls.
func (e SystemRestartEnv) Pane(ctx context.Context, pane string) (PaneState, error) {
	if err := checkPaneArg(pane); err != nil {
		return PaneState{}, err
	}
	out, err := e.runHerdr(ctx, "pane", "get", pane)
	if err != nil {
		if paneNotFound(err) {
			return PaneState{}, fmt.Errorf("pane %s: %w", pane, ErrPaneGone)
		}
		return PaneState{}, fmt.Errorf("herdr pane get: %w", err)
	}
	st, err := parsePaneGet(out)
	if err != nil {
		return PaneState{}, err
	}
	out, err = e.runHerdr(ctx, "pane", "process-info", "--pane", pane)
	if err != nil {
		if paneNotFound(err) {
			return PaneState{}, fmt.Errorf("pane %s: %w", pane, ErrPaneGone)
		}
		return PaneState{}, fmt.Errorf("herdr pane process-info: %w", err)
	}
	if err := parseProcessInfo(out, &st); err != nil {
		return PaneState{}, err
	}
	return st, nil
}

// Screen implements RestartEnv. The text is another program's screen and is
// returned for parsing only; no caller logs or prints it.
func (e SystemRestartEnv) Screen(ctx context.Context, pane string) (string, error) {
	if err := checkPaneArg(pane); err != nil {
		return "", err
	}
	out, err := e.runHerdr(ctx, "pane", "read", pane, "--source", "visible")
	if err != nil {
		// Dropped whole rather than wrapped: a *CommandError keeps stdout on
		// its Output field, and stdout here is screen text.
		return "", errors.New("herdr pane read failed")
	}
	return out, nil
}

// ClearInput implements RestartEnv. Ctrl-U kills the whole line in zsh's
// line editor and is the tty's line-kill character in cooked mode, so it also
// discards typeahead the shell has not read yet (both measured on herdr 0.9.1
// with zsh). bash and fish bind it to kill-to-start, which is the whole line
// with the cursor at its end.
func (e SystemRestartEnv) ClearInput(ctx context.Context, pane string) error {
	if err := checkPaneArg(pane); err != nil {
		return err
	}
	if _, err := e.runHerdr(ctx, "pane", "send-keys", pane, "ctrl+u"); err != nil {
		return fmt.Errorf("herdr pane send-keys: %w", err)
	}
	return nil
}

// Relaunch implements RestartEnv.
func (e SystemRestartEnv) Relaunch(ctx context.Context, pane, sessionID string) error {
	if err := checkPaneArg(pane); err != nil {
		return err
	}
	line, err := e.relaunchLine(sessionID)
	if err != nil {
		return err
	}
	if _, err := e.runHerdr(ctx, "pane", "run", pane, line); err != nil {
		return fmt.Errorf("herdr pane run: %w", err)
	}
	return nil
}

// paneNotFound matches herdr's structured refusal code — the code, never the
// prose — in a failed command's stderr:
// {"error":{"code":"pane_not_found",...}} (measured on herdr 0.9.1).
func paneNotFound(err error) bool {
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		return false
	}
	var reply struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(strings.TrimSpace(ce.Stderr)), &reply) == nil && reply.Error.Code == "pane_not_found"
}

// checkPaneArg refuses a pane id that is not a single, non-flag operand. The
// id came from another process's environment through validPane, whose
// alphabet admits a leading dash.
func checkPaneArg(pane string) error {
	if pane == "" || validPane(pane) != pane || strings.HasPrefix(pane, "-") {
		return errors.New("refusing an invalid pane id")
	}
	return nil
}

// parsePaneGet reads `herdr pane get` JSON: .result.pane.agent_session.
func parsePaneGet(out string) (PaneState, error) {
	var reply struct {
		Result struct {
			Pane *struct {
				AgentSession *struct {
					Agent string `json:"agent"`
					Value string `json:"value"`
				} `json:"agent_session"`
				Scroll *struct {
					OffsetFromBottom int `json:"offset_from_bottom"`
				} `json:"scroll"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return PaneState{}, errors.New("herdr pane get: reply is not JSON")
	}
	if reply.Result.Pane == nil {
		return PaneState{}, errors.New("herdr pane get: reply has no pane")
	}
	var st PaneState
	if as := reply.Result.Pane.AgentSession; as != nil {
		st.Agent, st.AgentSession = as.Agent, as.Value
	}
	if sc := reply.Result.Pane.Scroll; sc != nil {
		st.ScrolledBack = sc.OffsetFromBottom > 0
	}
	return st, nil
}

// parseProcessInfo reads `herdr pane process-info` JSON into st.
func parseProcessInfo(out string, st *PaneState) error {
	var reply struct {
		Result struct {
			ProcessInfo *struct {
				ForegroundPGID int `json:"foreground_process_group_id"`
				ShellPID       int `json:"shell_pid"`
				Foreground     []struct {
					Pid int `json:"pid"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return errors.New("herdr pane process-info: reply is not JSON")
	}
	pi := reply.Result.ProcessInfo
	if pi == nil {
		return errors.New("herdr pane process-info: reply has no process_info")
	}
	st.ForegroundPGID, st.ShellPID = pi.ForegroundPGID, pi.ShellPID
	st.ForegroundPIDs = st.ForegroundPIDs[:0]
	for _, p := range pi.Foreground {
		st.ForegroundPIDs = append(st.ForegroundPIDs, p.Pid)
	}
	return nil
}
