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

// applyWorkerProfile sets p's posture to the worker's: for each field, the
// worker profile's value ([launch.worker], else the built-in worker value),
// or the stricter of that and the matched project block's own value when the
// block sets one. [launch.defaults] does not bind a worker: it is the
// operator's interactive posture, and its built-in plan would leave every
// worker unable to write. The worker floor caps the result afterwards.
func applyWorkerProfile(p Profile, w config.LaunchWorker, proj *config.LaunchProject) (Profile, error) {
	pick := func(r postureRank, worker, builtin, project string) (string, error) {
		v := firstNonEmpty(worker, builtin)
		if project == "" {
			return v, r.check(v)
		}
		return r.stricter(v, project)
	}
	var pm, sb, ap string
	if proj != nil {
		pm, sb, ap = proj.PermissionMode, proj.Sandbox, proj.ApprovalPolicy
	}
	// Each field is read only for the harness that takes it, so a codex value
	// in a claude repo's block cannot refuse a claude worker.
	var err error
	switch p.Harness {
	case "claude":
		p.PermissionMode, err = pick(claudePermissionRank, w.PermissionMode, workerMaxPermissionMode, pm)
	case "codex":
		if p.Sandbox, err = pick(codexSandboxRank, w.Sandbox, workerMaxSandbox, sb); err == nil {
			p.ApprovalPolicy, err = pick(codexApprovalRank, w.ApprovalPolicy, workerMaxApproval, ap)
		}
	}
	if err != nil {
		return Profile{}, fmt.Errorf("worker profile: %w", err)
	}
	return p, nil
}
