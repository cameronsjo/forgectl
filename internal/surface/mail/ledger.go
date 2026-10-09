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
	EnvLedger = "FORGECTL_LEDGER"
	EnvWorker = "FORGECTL_WORKER"
	EnvInbox  = "FORGECTL_INBOX"
	// envClaudeSocket is exported by Claude Code to its hooks and Bash commands.
	envClaudeSocket = "CLAUDE_CODE_MESSAGING_SOCKET"
)

// ErrNoSelf is returned when the caller's own roster name cannot be settled.
var ErrNoSelf = errors.New("cannot tell who is sending")

// ErrNoLedger is returned outside a coordinator session or a launched worker.
var ErrNoLedger = errors.New("no coordinator ledger here: run this from the coordinator's Claude session or from a worker forgectl launched")

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

// SelfName is the caller's name in the roster: FORGECTL_WORKER for a worker,
// else the roster's coordinator entry.
//
// A process with FORGECTL_LEDGER but no FORGECTL_WORKER is refused rather than
// taken for the coordinator: the coordinator finds its ledger from its inbox
// socket and never carries FORGECTL_LEDGER, so the only process that has one
// is a launched worker, and unsetting its name must not make it the hub.
func SelfName(r Roster, getenv func(string) string) (string, error) {
	if name := getenv(EnvWorker); name != "" {
		if err := ValidateName(name); err != nil {
			return "", fmt.Errorf("%s: %w", EnvWorker, err)
		}
		return name, nil
	}
	if getenv(EnvLedger) != "" {
		return "", fmt.Errorf("%w: %s is set but %s is not; a worker forgectl launched carries both", ErrNoSelf, EnvLedger, EnvWorker)
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
	return "", fmt.Errorf("%w: the roster has no coordinator entry yet; surface launch writes it", ErrNoSelf)
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
