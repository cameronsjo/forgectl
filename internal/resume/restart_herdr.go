package resume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// processIdentity is the platform seam for ProcIdentity (procident_*.go).
var processIdentity = readProcessIdentity

// SystemRestartEnv is the production RestartEnv: the real registry, the
// kernel, SIGTERM, and herdr over its CLI.
//
// It drives herdr directly rather than through internal/surface's herdr
// adapter, and it types `forgectl resume <id>`, not a harness command line.
// The surface barrier exists so an adapter never sees the harness invocation;
// here herdr sees only forgectl's own resume verb, which resolves the launch
// profile and argv itself, in the pane.
type SystemRestartEnv struct {
	Paths  Paths
	Runner exec.Runner
	// RelaunchLine renders the shell line typed into the pane for a session id
	// that has already passed validSessionID. `herdr pane run` joins its
	// arguments with spaces and types them into the pane's shell (measured on
	// herdr 0.9.1), so the line must arrive fully shell-quoted.
	RelaunchLine func(sessionID string) (string, error)
}

var _ RestartEnv = SystemRestartEnv{}

// ReadEntry implements RestartEnv.
func (e SystemRestartEnv) ReadEntry(pid int) (RegistryEntry, bool) { return ReadEntry(e.Paths, pid) }

// Alive implements RestartEnv.
func (e SystemRestartEnv) Alive(pid int) bool { return pidAlive(pid) }

// Identity implements RestartEnv.
func (e SystemRestartEnv) Identity(pid int) (ProcIdentity, error) { return processIdentity(pid) }

// Terminate implements RestartEnv.
func (e SystemRestartEnv) Terminate(pid int) error { return terminateProcess(pid) }

// LiveSession implements RestartEnv.
func (e SystemRestartEnv) LiveSession(sessionID string) (RegistryEntry, bool) {
	return LiveSession(e.Paths, sessionID)
}

// Prepare implements RestartEnv. It runs the same capture the Stop hook runs
// at every turn end, so the resumed session gets its /rename name and task
// bodies back, then confirms the store now holds the session: a session that
// arrived through /clear can be absent from history.jsonl, and once its
// registry file is gone the store is the only place `forgectl resume` could
// find it.
func (e SystemRestartEnv) Prepare(sessionID string) error {
	res := Snapshot(e.Paths, time.Now())
	if _, ok := Load(e.Paths.StoreDir, sessionID); !ok {
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
	out, err := e.Runner.Run(ctx, "herdr", "pane", "get", pane)
	if err != nil {
		return PaneState{}, fmt.Errorf("herdr pane get: %w", err)
	}
	st, err := parsePaneGet(out)
	if err != nil {
		return PaneState{}, err
	}
	out, err = e.Runner.Run(ctx, "herdr", "pane", "process-info", "--pane", pane)
	if err != nil {
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
	out, err := e.Runner.Run(ctx, "herdr", "pane", "read", pane, "--source", "visible")
	if err != nil {
		// Dropped whole rather than wrapped: a *CommandError keeps stdout on
		// its Output field, and stdout here is screen text.
		return "", errors.New("herdr pane read failed")
	}
	return out, nil
}

// Relaunch implements RestartEnv.
func (e SystemRestartEnv) Relaunch(ctx context.Context, pane, sessionID string) error {
	if err := checkPaneArg(pane); err != nil {
		return err
	}
	// The id is disk-sourced; it reaches a shell line only after this.
	if !validSessionID(sessionID) {
		return errors.New("refusing an invalid session id")
	}
	if e.RelaunchLine == nil {
		return errors.New("no relaunch command configured")
	}
	line, err := e.RelaunchLine(sessionID)
	if err != nil {
		return err
	}
	if _, err := e.Runner.Run(ctx, "herdr", "pane", "run", pane, line); err != nil {
		return fmt.Errorf("herdr pane run: %w", err)
	}
	return nil
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
