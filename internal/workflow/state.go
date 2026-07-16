package workflow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cameronsjo/forgectl/internal/config"
)

// StateSchema is the run-state sidecar's schema version. It is bumped only on an
// incompatible layout change; LoadState refuses a newer schema so an old binary
// never half-reads a format it doesn't understand (mirrors the DSL's version
// gate in parse.go).
const StateSchema = 1

// RunState is the persisted checkpoint for the LAST run of one workflow — the
// sidecar `workflow run --resume` replays and `workflow status` renders. One
// file per workflow name under config.WorkflowStateDir(); a fresh successful
// run clears it, a failed run leaves it for the next resume.
//
// DefinitionHash pins the exact workflow-file bytes the checkpoints belong to.
// Resume refuses outright when the current file's hash differs — a changed (or
// re-blessed) definition invalidates every checkpoint, so it must be run fresh
// rather than resumed across an edit (the blessing ceremony is re-verified on
// resume regardless; this is the second, definition-level guard).
type RunState struct {
	Schema         int         `toml:"schema"`
	Workflow       string      `toml:"workflow"`
	RunID          string      `toml:"run_id"`
	DefinitionHash string      `toml:"definition_hash"`
	StartedAt      string      `toml:"started_at"`
	UpdatedAt      string      `toml:"updated_at"`
	Steps          []StepState `toml:"step"`
}

// StepState records one completed step. InputHash is the hash of the step's
// RESOLVED-AT-PLAN-TIME inputs (params baked in, prior-step exports still the
// literal ${name}); resume skips a checkpointed step only when this hash still
// matches, so a param change that alters the step's inputs forces a re-run.
// Exports are deliberately NOT persisted — see MissingResumeExport for why an
// unsigned sidecar must never feed a resumed step's fields.
type StepState struct {
	Index       int    `toml:"index"`
	Uses        string `toml:"uses"`
	InputHash   string `toml:"input_hash"`
	CompletedAt string `toml:"completed_at"`
}

// DefinitionHash is the canonical content hash of a workflow file's raw bytes,
// used to pin a RunState to the exact definition it checkpointed.
func DefinitionHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashPlanStep hashes a step's plan-time inputs into a stable digest. Fields are
// joined with a NUL separator so ("a","b") and ("ab","") can't collide, and the
// order is fixed so the same inputs always hash the same. Prior-step exports are
// still the literal ${name} at plan time, so they contribute a stable token
// rather than a per-run sandbox path — resume compares like with like.
func HashPlanStep(s PlanStep) string {
	var b strings.Builder
	write := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
			b.WriteByte(0)
		}
	}
	write(s.Uses, s.Repo, s.Ref, s.Skill, s.Posture, s.Mode, s.From, s.To, s.Cmd)
	b.WriteString("globs")
	b.WriteByte(0)
	write(s.Globs...)
	b.WriteString("args")
	b.WriteByte(0)
	write(s.Args...)
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewRunID mints a sortable run identifier: a UTC timestamp plus a short random
// suffix so two runs in the same second stay distinct. It is provenance for
// display only — the sidecar is keyed by workflow name, not run id.
func NewRunID(now time.Time) string {
	stamp := now.UTC().Format("20060102T150405Z")
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return stamp
	}
	return stamp + "-" + hex.EncodeToString(suffix[:])
}

// StatePath returns the run-state sidecar path for a workflow name. The name is
// validated exactly as Load validates it — a separator or ".." must not let a
// state read/write escape the state directory.
func StatePath(name string) (string, error) {
	if err := validateWorkflowName(name); err != nil {
		return "", err
	}
	dir, err := config.WorkflowStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".state.toml"), nil
}

// LoadState reads the run-state sidecar for name. A missing file is not an error
// — it yields (zero, false, nil), the "never run / already cleared" signal.
func LoadState(name string) (RunState, bool, error) {
	path, err := StatePath(name)
	if err != nil {
		return RunState{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RunState{}, false, nil
		}
		return RunState{}, false, fmt.Errorf("read workflow state %q: %w", name, err)
	}
	var st RunState
	if _, err := toml.Decode(string(data), &st); err != nil {
		return RunState{}, false, fmt.Errorf("parse workflow state %q: %w", name, err)
	}
	if st.Schema > StateSchema {
		return RunState{}, false, fmt.Errorf("workflow state %q has schema %d, newer than this binary understands (%d)", name, st.Schema, StateSchema)
	}
	return st, true, nil
}

// WriteState persists st atomically: it writes a temp file in the state
// directory, fsyncs it, and renames it over the target. The rename is atomic, so
// a crash mid-write leaves either the previous state or the new one intact —
// never a truncated file and never a window with neither (the recovery-path
// invariant).
func WriteState(st RunState) error {
	path, err := StatePath(st.Workflow)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create workflow state dir %s: %w", dir, err)
	}
	data, err := encodeState(st)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, st.Workflow+".state.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any error path before the rename.
	// After a successful rename tmpName no longer exists, so this is a no-op.
	defer os.Remove(tmpName) //nolint:errcheck

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit state file %s: %w", path, err)
	}
	slog.Debug("Wrote workflow run state.", "workflow", st.Workflow, "path", path, "stepCount", len(st.Steps))
	return nil
}

// ClearState removes the run-state sidecar for name. A missing file is not an
// error — clearing already-absent state is a no-op.
func ClearState(name string) error {
	path, err := StatePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear workflow state %q: %w", name, err)
	}
	slog.Debug("Cleared workflow run state.", "workflow", name)
	return nil
}

// encodeState serialises a RunState to sidecar TOML bytes (same encoder the
// bless and config layers use).
func encodeState(st RunState) ([]byte, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(st); err != nil {
		return nil, fmt.Errorf("encode workflow state: %w", err)
	}
	return []byte(b.String()), nil
}

// ResumeFrom returns the index of the first step a resume must execute: it walks
// the plan from the start, skipping each leading step whose checkpoint is
// present AND whose plan-time input hash still matches, and stops at the first
// gap. A step after a gap is never skipped even if it was checkpointed — once an
// earlier step re-runs, everything downstream of it must re-run too. A return of
// len(plan.Steps) means every step is already done.
func ResumeFrom(prior RunState, plan Plan) int {
	byIndex := make(map[int]StepState, len(prior.Steps))
	for _, ss := range prior.Steps {
		byIndex[ss.Index] = ss
	}
	i := 0
	for i < len(plan.Steps) {
		ss, ok := byIndex[i]
		if !ok || ss.InputHash != HashPlanStep(plan.Steps[i]) {
			break
		}
		i++
	}
	return i
}

// MissingResumeExport reports whether any step a resume WILL execute references
// a ${export} that only a SKIPPED (checkpointed) step produces. It returns that
// export's name and the consuming step's index.
//
// This is the guard that keeps resume from weakening the blessing ceremony. A
// step's exports (e.g. worktree's ${workspace}) are ephemeral run outputs, not
// signed inputs — and a failed run tears its sandbox down, so the value is gone
// anyway. Rather than rehydrate an export from the unsigned sidecar into a
// resumed step's field (which could inject an attacker-chosen value into a
// blessing-guarded field), resume refuses up front with a clear message telling
// the user to run fresh. Exports produced by a step that itself re-runs during
// the resume are fine — those are freshly, legitimately produced.
func MissingResumeExport(plan Plan, resumeFrom int, registry StepRegistry) (name string, stepIndex int, missing bool) {
	// Every export name the plan can produce, and whether a resumed step
	// (re)produces it before the step that references it.
	isExport := make(map[string]bool)
	for _, s := range plan.Steps {
		for _, e := range registry[s.Uses].Exports {
			isExport[e] = true
		}
	}

	available := make(map[string]bool)
	for i := resumeFrom; i < len(plan.Steps); i++ {
		for _, ref := range stepVarRefs(plan.Steps[i]) {
			if isExport[ref] && !available[ref] {
				return ref, i, true
			}
		}
		for _, e := range registry[plan.Steps[i].Uses].Exports {
			available[e] = true
		}
	}
	return "", 0, false
}

// stepVarRefs returns every ${name} variable referenced across a step's fields.
func stepVarRefs(s PlanStep) []string {
	var out []string
	add := func(vals ...string) {
		for _, v := range vals {
			out = append(out, parseRefs(v)...)
		}
	}
	add(s.Repo, s.Ref, s.Skill, s.Posture, s.Mode, s.From, s.To, s.Cmd)
	add(s.Globs...)
	add(s.Args...)
	return out
}

// parseRefs extracts the names inside every ${name} in s. A malformed or
// unterminated reference is ignored here — the interpolator (context.go) is the
// authority that rejects those at plan/execute time.
func parseRefs(s string) []string {
	var out []string
	i := 0
	for {
		start := strings.Index(s[i:], "${")
		if start < 0 {
			break
		}
		start += i
		end := strings.Index(s[start:], "}")
		if end < 0 {
			break
		}
		end += start
		out = append(out, s[start+2:end])
		i = end + 1
	}
	return out
}

// StateRecorder accumulates a RunState and flushes it to disk after each step
// completes, so a mid-workflow crash leaves a resumable checkpoint. It is the
// Recorder the Executor calls; a nil Recorder disables checkpointing entirely.
type StateRecorder struct {
	state RunState
}

// NewStateRecorder starts a fresh run's recorder — no prior checkpoints.
func NewStateRecorder(name, runID, definitionHash string, now time.Time) *StateRecorder {
	ts := now.UTC().Format(time.RFC3339)
	return &StateRecorder{state: RunState{
		Schema:         StateSchema,
		Workflow:       name,
		RunID:          runID,
		DefinitionHash: definitionHash,
		StartedAt:      ts,
		UpdatedAt:      ts,
	}}
}

// NewResumeRecorder continues a prior run's state: it keeps the checkpoints
// below resumeFrom (so a resume that fails again still records the full
// completed prefix) and appends the steps this resume completes after them. It
// preserves the prior run's identity and definition hash — a resume is the same
// logical run continuing, not a new one.
func NewResumeRecorder(prior RunState, resumeFrom int, now time.Time) *StateRecorder {
	kept := make([]StepState, 0, len(prior.Steps))
	for _, ss := range prior.Steps {
		if ss.Index < resumeFrom {
			kept = append(kept, ss)
		}
	}
	prior.Steps = kept
	prior.UpdatedAt = now.UTC().Format(time.RFC3339)
	return &StateRecorder{state: prior}
}

// Record marks step index complete and durably persists the updated state. It
// hashes the PLAN-TIME step (params baked in, exports still literal) so a later
// resume can compare inputs without re-running earlier steps.
func (r *StateRecorder) Record(index int, s PlanStep) error {
	now := time.Now().UTC().Format(time.RFC3339)
	r.state.UpdatedAt = now
	r.state.Steps = append(r.state.Steps, StepState{
		Index:       index,
		Uses:        s.Uses,
		InputHash:   HashPlanStep(s),
		CompletedAt: now,
	})
	return WriteState(r.state)
}
