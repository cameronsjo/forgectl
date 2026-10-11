package worker

import (
	"errors"

	"github.com/cameronsjo/forgectl/internal/config"
)

// merge-audit.jsonl (atelier P4, T10.4) sits beside the queue in the pinned
// surface directory: one JSON line per merge attempt, merge outcome and
// refusal, written by `surface merge` and the drain's autopilot
// (internal/surface/merge builds the lines and checks the chain). It is
// appended under an flock on merge-audit.lock, each append synced, and never
// pruned or rotated by forgectl; the operator rotates it by hand.

const (
	mergeAuditName     = "merge-audit.jsonl"
	mergeAuditLockName = "merge-audit.lock"
	// MaxMergeAuditBytes caps the audit file: it is read whole to chain
	// each line onto the last and to check the chain. An append that would
	// pass it is refused (ErrMergeAuditFull), and with it the merge.
	MaxMergeAuditBytes = 16 << 20
)

// ErrMergeAuditFull reports an audit file an append would take past
// MaxMergeAuditBytes. Rotate it by hand (docs/herdr.md).
var ErrMergeAuditFull = errors.New("worker: merge-audit.jsonl is full; rotate it by hand (see docs/herdr.md)")

// MergeAudit is merge-audit.jsonl under one state base.
type MergeAudit struct {
	stateBase string
}

// OpenMergeAudit returns the audit file in $XDG_STATE_HOME/forgectl/surface,
// defaulting to ~/.local/state.
func OpenMergeAudit() (MergeAudit, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return MergeAudit{}, err
	}
	return MergeAudit{stateBase: base}, nil
}
