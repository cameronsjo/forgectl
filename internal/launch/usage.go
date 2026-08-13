package launch

import (
	"errors"
)

// UsageSchemaVersion is the on-disk row format. Unknown fields on a v1 row are
// additive and ignored; anything that changes the meaning of an existing key
// requires a bump, and a reader treats an unrecognized version as a countable
// skip rather than guessing.
const UsageSchemaVersion = 1

// MaxUsageRowBytes bounds one encoded row INCLUDING its trailing newline. It
// is enforced at encode time, before the store is opened, so an oversized
// value (a pathological model string) can never reach the file — and it is the
// same bound the reader caps its scanner at, so a row that was accepted can
// always be read back.
const MaxUsageRowBytes = 64 * 1024

// The closed vocabularies of a v1 row. They are values, not free text: an
// unrecognized one is a bug in the caller, and encoding refuses it rather than
// writing a row a later reader would have to guess at.
const (
	UsageEventExecAttempt = "exec_attempt"

	UsageSessionNew     = "new"
	UsageSessionResume  = "resume"
	UsageSessionFork    = "fork"
	UsageSessionUnknown = "unknown"

	UsagePostureDefault = "default"
	UsagePostureBuilder = "builder"
	UsagePostureAgents  = "agents"
)

// Typed recorder failures. Every one of them is discarded silently by the
// launch and resume hot paths — they exist so `launch stats` and `launch
// doctor`, the two surfaces an operator asks diagnostics from, can say
// something specific.
var (
	// ErrUsageDisabled reports that collection is off. It is the expected
	// state, not a fault.
	ErrUsageDisabled = errors.New("launch usage statistics are disabled")
	// ErrUsageBusy reports that another process holds the store lock. The
	// lock is nonblocking by design: a launch must not wait on a peer.
	ErrUsageBusy = errors.New("launch usage store is busy")
	// ErrUsageUnsafeStore reports a store forgectl refused to open — a
	// substituted directory, a symlink, a hardlink, a special file, or an
	// unexpected owner.
	ErrUsageUnsafeStore = errors.New("launch usage store is unsafe")
	// ErrUsageRowTooLarge reports an encoded row over MaxUsageRowBytes.
	ErrUsageRowTooLarge = errors.New("launch usage row exceeds the size bound")
	// ErrUsageInvalidEvent reports a row that fails enum or timestamp
	// validation before anything is opened.
	ErrUsageInvalidEvent = errors.New("launch usage event is invalid")
	// ErrUsageUnsupported reports a platform with no safe store
	// implementation. Collection stays behaviorally transparent there.
	ErrUsageUnsupported = errors.New("launch usage statistics are unsupported on this platform")
)

// UsageEventV1 is one recorded accepted exec attempt: forgectl finished
// validating a profile and is about to call syscall.Exec. It deliberately says
// nothing about whether the harness started, whether the session was useful,
// or how long it ran — none of that is observable from a process that replaces
// itself.
//
// The seven fields below are the whole record. Nothing identifying the
// operator, the machine, or the work is retained: no cwd, project, repo,
// branch, session id, argv, prompt, environment, task data, host, user, pid,
// or hash of any of those. The exact-key test in usage_test.go is the guard.
type UsageEventV1 struct {
	SchemaVersion int    `json:"schema_version"`
	TS            string `json:"ts"`
	Event         string `json:"event"`
	Harness       string `json:"harness"`
	// Model is the resolved model string exactly as configured, including a
	// custom or internal deployment label, and "" for the harness-native
	// default. It stays raw in JSON — only human rendering substitutes a
	// label, and only human rendering makes it terminal-safe.
	Model       string `json:"model"`
	SessionMode string `json:"session_mode"`
	Posture     string `json:"posture"`
}

// UsageModelLabel is the human-output substitution for the empty model key.
// JSON never calls it: an exporter that rewrote "" into a sentence would make
// the machine contract depend on English.
func UsageModelLabel(model string) string {
	if model == "" {
		return "(harness default)"
	}
	return model
}
