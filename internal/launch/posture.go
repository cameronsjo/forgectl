package launch

import (
	"fmt"
	"slices"
	"strings"

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
