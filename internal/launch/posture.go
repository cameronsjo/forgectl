package launch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Posture strictness, as data (forgectl#1043). Each table lists a field's
// known values strictest first; values on one line of a table are equally
// strict. A value missing from its table is unknown: Profile.Validate refuses
// it, and nothing here ranks it.

// postureRank is one field's strictness order.
type postureRank struct {
	field  string
	levels [][]string
}

var (
	// claudePermissionRank is Claude Code's --permission-mode, measured on
	// 2.1.289: its help lists acceptEdits, auto, bypassPermissions, manual,
	// dontAsk and plan; `default` is still accepted, and both it and `manual`
	// start the session in default mode.
	claudePermissionRank = postureRank{"permission_mode", [][]string{
		{"plan"},
		{"default", "manual"},
		{"acceptEdits"},
		{"auto"},
		{"dontAsk"},
		{"bypassPermissions"},
	}}
	// codexSandboxRank is Codex's --sandbox.
	codexSandboxRank = postureRank{"Codex sandbox", [][]string{
		{"read-only"},
		{"workspace-write"},
		{"danger-full-access"},
	}}
	// codexApprovalRank is Codex's --ask-for-approval. Codex 0.160.0 accepts
	// only on-request and never; untrusted is kept for older releases, which
	// accepted it, and a newer Codex refuses it at launch.
	codexApprovalRank = postureRank{"Codex approval_policy", [][]string{
		{"untrusted"},
		{"on-request"},
		{"never"},
	}}
)

// level returns v's rank, 0 the strictest, and whether v is known.
func (r postureRank) level(v string) (int, bool) {
	for i, vs := range r.levels {
		if slices.Contains(vs, v) {
			return i, true
		}
	}
	return 0, false
}

// known lists every value r ranks, strictest first.
func (r postureRank) known() []string {
	var out []string
	for _, vs := range r.levels {
		out = append(out, vs...)
	}
	return out
}

// check refuses a value r does not rank.
func (r postureRank) check(v string) error {
	if _, ok := r.level(v); !ok {
		return fmt.Errorf("unsupported %s %s: want one of %s", r.field, termsafe.QuoteArgMax(v, 0), strings.Join(r.known(), ", "))
	}
	return nil
}

// atMost reports whether v is known and no looser than limit.
func (r postureRank) atMost(v, limit string) bool {
	lv, ok := r.level(v)
	ll, lok := r.level(limit)
	return ok && lok && lv <= ll
}

// stricter returns the stricter of a and b. When either is unranked it
// returns "" with an error naming it, so the result is unusable without the
// error: an unranked value can never win, and neither can its partner by
// default.
func (r postureRank) stricter(a, b string) (string, error) {
	if err := r.check(a); err != nil {
		return "", err
	}
	if err := r.check(b); err != nil {
		return "", err
	}
	la, _ := r.level(a)
	lb, _ := r.level(b)
	if lb < la {
		return b, nil
	}
	return a, nil
}

// The built-in worker posture: the coordinator plan's v1 worker, which can
// edit its worktree and whose shell commands still prompt. Separate from the
// floor caps in invocation.go, which they must never exceed
// (TestWorkerDefaultsWithinCaps).
const (
	workerDefaultPermissionMode = "acceptEdits"
	workerDefaultSandbox        = "workspace-write"
	workerDefaultApproval       = "on-request"
)

// Sources of a worker posture value, as the --dry-run preview names them.
const (
	// PostureFromDefault is the built-in worker value ([launch.worker] unset).
	PostureFromDefault = "default"
	// PostureFromWorker is the [launch.worker] value.
	PostureFromWorker = "launch.worker"
	// PostureFromRepo is the matched project block's value, which won because
	// it is stricter.
	PostureFromRepo = "repo-profile"
)

// PostureValue is one resolved worker posture field and where it came from.
type PostureValue struct {
	Field  string
	Value  string
	Source string
}

// applyWorkerProfile sets p's posture to the worker's: for each field, the
// worker profile's value ([launch.worker], else the built-in worker value),
// or the stricter of that and the matched project block's own value when the
// block sets one. [launch.defaults] does not bind a worker: it is the
// operator's interactive posture, and its built-in plan would leave every
// worker unable to write. The worker floor caps the result afterwards.
//
// Every [launch.worker] field is checked whatever the harness, so a typo in
// one fails now rather than when the harness flips. A project block's field
// is read only for the harness that takes it, so a codex value in a claude
// repo's block cannot refuse a claude worker.
//
// The notes name each explicit [launch.defaults] value stricter than what the
// worker gets: an operator who set defaults to plan expecting read-only
// workers is told, at launch, that workers no longer read it.
//
// The returned values name each field the harness takes, with its source. The
// floor afterwards refuses rather than lowers, so they are the final posture.
func applyWorkerProfile(p Profile, lc config.LaunchConfig, proj *config.LaunchProject) (Profile, []PostureValue, []string, error) {
	w := lc.Worker
	wpm := firstNonEmpty(w.PermissionMode, workerDefaultPermissionMode)
	wsb := firstNonEmpty(w.Sandbox, workerDefaultSandbox)
	wap := firstNonEmpty(w.ApprovalPolicy, workerDefaultApproval)
	for _, c := range []struct {
		r postureRank
		v string
	}{{claudePermissionRank, wpm}, {codexSandboxRank, wsb}, {codexApprovalRank, wap}} {
		if err := c.r.check(c.v); err != nil {
			return Profile{}, nil, nil, fmt.Errorf("[launch.worker]: %w", err)
		}
	}
	var pm, sb, ap string
	if proj != nil {
		pm, sb, ap = proj.PermissionMode, proj.Sandbox, proj.ApprovalPolicy
	}
	var values []PostureValue
	// pick takes the stricter of the worker and project values. On a tie the
	// worker side wins, as stricter returns its first argument.
	pick := func(r postureRank, field, set, worker, project string) (string, error) {
		got, err := r.stricter(worker, firstNonEmpty(project, worker))
		if err != nil {
			return "", err
		}
		source := PostureFromDefault
		if set != "" {
			source = PostureFromWorker
		}
		if project != "" && got == project && got != worker {
			source = PostureFromRepo
		}
		values = append(values, PostureValue{Field: field, Value: got, Source: source})
		return got, nil
	}
	var notes []string
	note := func(r postureRank, field, defaults, got string) {
		if defaults == "" {
			return
		}
		// Strictly stricter by rank: default and manual share a level, and a
		// tie is no change for the operator to hear about.
		if r.atMost(defaults, got) && !r.atMost(got, defaults) {
			notes = append(notes, fmt.Sprintf("[launch.defaults] %s = %q is stricter than the worker's %q, and does not bind workers; set [launch.worker] %s to keep it",
				field, defaults, got, field))
		}
	}
	var err error
	switch p.Harness {
	case "claude":
		if p.PermissionMode, err = pick(claudePermissionRank, "permission_mode", w.PermissionMode, wpm, pm); err == nil {
			note(claudePermissionRank, "permission_mode", lc.Defaults.PermissionMode, p.PermissionMode)
		}
	case "codex":
		if p.Sandbox, err = pick(codexSandboxRank, "sandbox", w.Sandbox, wsb, sb); err == nil {
			p.ApprovalPolicy, err = pick(codexApprovalRank, "approval_policy", w.ApprovalPolicy, wap, ap)
		}
		if err == nil {
			note(codexSandboxRank, "sandbox", lc.Defaults.Sandbox, p.Sandbox)
			note(codexApprovalRank, "approval_policy", lc.Defaults.ApprovalPolicy, p.ApprovalPolicy)
		}
	}
	if err != nil {
		return Profile{}, nil, nil, fmt.Errorf("worker profile: %w", err)
	}
	return p, values, notes, nil
}
