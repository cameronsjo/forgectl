package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
)

// Environment variables forgectl sets when it launches a worker.
const (
	EnvLedger      = "FORGECTL_LEDGER"
	EnvWorker      = "FORGECTL_WORKER"
	EnvInbox       = "FORGECTL_INBOX"
	EnvCoordinator = "FORGECTL_COORDINATOR"
	EnvRunID       = "FORGECTL_RUN_ID"
	// EnvDrainWorker is launch.DrainWorkerEnv, which forgectl sets on every
	// worker it launches. Repeated here so mail does not import launch; a
	// cli test pins the two together.
	EnvDrainWorker = "FORGECTL_DRAIN_WORKER"
	// envClaudeSocket is exported by Claude Code to its hooks and Bash commands.
	envClaudeSocket = "CLAUDE_CODE_MESSAGING_SOCKET"
)

// ErrNoSelf is returned when the caller's own roster name cannot be settled.
var ErrNoSelf = errors.New("cannot tell who is sending")

// ErrNoLedger is returned outside a coordinator session or a launched worker.
var ErrNoLedger = errors.New("no mail ledger here: use the environment from surface team register, or run from a legacy Claude coordinator session")

// LedgerDir finds the coordinator ledger this process belongs to. A worker
// carries FORGECTL_LEDGER from launch. The coordinator is a Claude session
// forgectl did not start, so its ledger is keyed on its own inbox socket path.
// The mail ledger lives under <state>/mail, apart from the worker ledger's
// <state>/surface, which pins that directory and owns every entry in it.
func LedgerDir(stateDir string, getenv func(string) string) (string, error) {
	if d := getenv(EnvLedger); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s must be an absolute path, got %s", EnvLedger, quoteTrunc(d))
		}
		return filepath.Clean(d), nil
	}
	if sock := getenv(envClaudeSocket); sock != "" {
		sum := sha256.Sum256([]byte(sock))
		return filepath.Join(stateDir, "mail", hex.EncodeToString(sum[:6])), nil
	}
	return "", ErrNoLedger
}

// SelfName is the caller's explicit worker or coordinator identity, with the
// roster's coordinator entry as the legacy Claude fallback.
//
// Only the coordinator may resolve as the coordinator. A process with
// FORGECTL_LEDGER but neither explicit identity is refused. A drain worker
// cannot become the coordinator by unsetting its worker name. Registered run
// IDs must match, and FORGECTL_WORKER cannot name the coordinator.
//
// These are courtesy refusals, not a boundary: every worker runs as the
// operator, can edit its own environment and can write the mailbox directly.
// They stop a worker passing as the hub by accident or by a careless prompt,
// not one set on it.
func SelfName(r Roster, getenv func(string) string) (string, error) {
	if name := getenv(EnvCoordinator); name != "" {
		if getenv(EnvWorker) != "" || getenv(EnvDrainWorker) != "" {
			return "", fmt.Errorf("%w: coordinator and worker identities cannot be combined", ErrNoSelf)
		}
		if !filepath.IsAbs(getenv(EnvLedger)) {
			return "", fmt.Errorf("%w: %s requires an absolute %s", ErrNoSelf, EnvCoordinator, EnvLedger)
		}
		if err := ValidateName(name); err != nil {
			return "", err
		}
		coordinator, err := r.Get(name)
		if err != nil {
			return "", err
		}
		if !coordinator.Coordinator {
			return "", fmt.Errorf("%w: %s is not the registered coordinator", ErrNoSelf, quoteTrunc(name))
		}
		if err := checkRunIdentity(coordinator, getenv); err != nil {
			return "", err
		}
		return name, nil
	}
	if name := getenv(EnvWorker); name != "" {
		if err := ValidateName(name); err != nil {
			return "", fmt.Errorf("%s: %w", EnvWorker, err)
		}
		w, err := r.Get(name)
		if err != nil && !errors.Is(err, ErrUnknownWorker) {
			return "", err
		}
		if err == nil && w.Coordinator {
			return "", fmt.Errorf("%w: %s names the coordinator; a worker cannot send as it", ErrNoSelf, EnvWorker)
		}
		if err == nil {
			if err := checkRunIdentity(w, getenv); err != nil {
				return "", err
			}
		}
		return name, nil
	}
	for _, key := range []string{EnvLedger, EnvDrainWorker} {
		if getenv(key) != "" {
			return "", fmt.Errorf("%w: %s is set but %s is not; a worker forgectl launched carries both", ErrNoSelf, key, EnvWorker)
		}
	}
	workers, err := r.List()
	if err != nil {
		return "", err
	}
	for _, w := range workers {
		if w.Coordinator {
			return w.Name, nil
		}
	}
	return "", fmt.Errorf("%w: the roster has no coordinator entry yet; use surface team register --coordinator", ErrNoSelf)
}

func checkRunIdentity(member Worker, getenv func(string) string) error {
	if member.RunID != "" && member.RunID != getenv(EnvRunID) {
		return fmt.Errorf("%w: stale or missing %s for %s", ErrNoSelf, EnvRunID, quoteTrunc(member.Name))
	}
	return nil
}

// CoordinatorSelf is the roster entry `surface launch` writes for the calling
// coordinator the first time it runs: the coordinator's own inbox socket is
// known directly, so no registry lookup is needed to reach it.
func CoordinatorSelf(name, cwd string, getenv func(string) string) (Worker, error) {
	if err := ValidateName(name); err != nil {
		return Worker{}, err
	}
	sock := getenv(envClaudeSocket)
	if sock == "" {
		return Worker{}, ErrNoLedger
	}
	return Worker{Name: name, Harness: HarnessClaude, Worktree: cwd, Coordinator: true, Socket: sock}, nil
}
