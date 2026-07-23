package pr

// Test plan for allowlist.go
//
// writeAllowlist (Classification: deny-by-default security control)
//   [x] Writes .claude/settings.local.json into the workspace
//   [x] Denies every posting/mutation surface (gh pr review/comment/merge,
//       push, commit, WebFetch, Write/Edit)
//   [x] Allows only read-only inspection (Read/Grep/Glob + read-only Bash)
//   [x] Never grants gh pr review/comment/merge under allow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAllowlist(t *testing.T) {
	ws := t.TempDir()
	path, err := writeAllowlist(ws)
	if err != nil {
		t.Fatalf("writeAllowlist: %v", err)
	}
	want := filepath.Join(ws, ".claude", "settings.local.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var s allowlistSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}

	// Deny-by-default posting surfaces must be present.
	mustDeny := []string{
		"Bash(gh pr review:*)", "Bash(gh pr comment:*)", "Bash(gh pr merge:*)",
		"Bash(git push:*)", "Write", "Edit", "WebFetch",
	}
	for _, d := range mustDeny {
		if !contains(s.Permissions.Deny, d) {
			t.Errorf("deny list missing %q", d)
		}
	}

	// Allow list must be strictly read-only — no post/merge/comment.
	for _, a := range s.Permissions.Allow {
		low := strings.ToLower(a)
		for _, banned := range []string{"pr review", "pr comment", "pr merge", "push", "commit", "write", "edit"} {
			if strings.Contains(low, banned) {
				t.Errorf("allow list grants a mutating/posting action: %q", a)
			}
		}
	}
	if len(s.Permissions.Allow) == 0 {
		t.Error("allow list is empty; read-only inspection should be permitted")
	}
}

// TestDenyPosting_CoversMutatingGhGroups covers the defense-in-depth backstop:
// denyPosting must hard-block the mutating `gh pr` verbs AND every other
// mutating gh command group — so a relaxed permission floor still can't reach
// pr ready/reopen/lock/unlock or workflow/release/secret/variable/ruleset/
// issue/gist/repo/run. None of these may overlap the read-only allow-list
// (that would silently revoke a read).
func TestDenyPosting_CoversMutatingGhGroups(t *testing.T) {
	mustDeny := []string{
		"Bash(gh pr ready:*)",
		"Bash(gh pr reopen:*)",
		"Bash(gh pr lock:*)",
		"Bash(gh pr unlock:*)",
		"Bash(gh workflow:*)",
		"Bash(gh release:*)",
		"Bash(gh secret:*)",
		"Bash(gh variable:*)",
		"Bash(gh ruleset:*)",
		"Bash(gh issue:*)",
		"Bash(gh gist:*)",
		"Bash(gh repo:*)",
		"Bash(gh run:*)",
		"Bash(gh auth:*)",
		"Bash(gh config:*)",
		"Bash(gh label:*)",
		"Bash(gh project:*)",
		"Bash(gh cache:*)",
		"Bash(gh codespace:*)",
		"Bash(gh extension:*)",
		"Bash(gh alias:*)",
	}
	for _, d := range mustDeny {
		if !contains(denyPosting, d) {
			t.Errorf("denyPosting missing mutating gh group %q", d)
		}
		if contains(allowReadOnly, d) {
			t.Errorf("deny entry %q also appears in allowReadOnly — a read would be silently revoked", d)
		}
	}
}

// TestAllowReadOnly_LayersRgAndGhReadsOverBaseReadOnly covers the
// baseReadOnly extraction: PR-mode's allowReadOnly must still carry every
// baseReadOnly entry (the shared surface) plus exactly rg and the read-only
// gh reads (baseReadOnly deliberately excludes rg — ripgrep's --pre flag is a
// command-execution primitive local mode's no-approval-gate posture can't
// accept), with no accidental loss or duplication from the append-based
// composition.
func TestAllowReadOnly_LayersRgAndGhReadsOverBaseReadOnly(t *testing.T) {
	for _, want := range baseReadOnly {
		if !contains(allowReadOnly, want) {
			t.Errorf("allowReadOnly missing shared baseReadOnly entry %q", want)
		}
	}
	for _, want := range []string{"Bash(rg:*)", "Bash(gh pr view:*)", "Bash(gh pr diff:*)", "Bash(gh pr checks:*)"} {
		if !contains(allowReadOnly, want) {
			t.Errorf("allowReadOnly missing entry %q", want)
		}
	}
	if len(allowReadOnly) != len(baseReadOnly)+4 {
		t.Errorf("allowReadOnly has %d entries, want exactly baseReadOnly (%d) + rg + 3 gh reads", len(allowReadOnly), len(baseReadOnly))
	}
}

// TestLocalAllowReadOnly_IsExactlyBaseReadOnly covers the other extraction
// consumer: local mode grants no rg (no approval-gate backstop) and no gh
// entries at all, i.e. localAllowReadOnly must equal baseReadOnly exactly
// (not allowReadOnly, which layers rg + gh reads on top).
func TestLocalAllowReadOnly_IsExactlyBaseReadOnly(t *testing.T) {
	if len(localAllowReadOnly) != len(baseReadOnly) {
		t.Fatalf("localAllowReadOnly has %d entries, want %d (== baseReadOnly)", len(localAllowReadOnly), len(baseReadOnly))
	}
	for i, want := range baseReadOnly {
		if localAllowReadOnly[i] != want {
			t.Errorf("localAllowReadOnly[%d] = %q, want %q", i, localAllowReadOnly[i], want)
		}
	}
	for _, a := range localAllowReadOnly {
		if strings.Contains(a, "gh") {
			t.Errorf("localAllowReadOnly must grant no gh entries; found %q", a)
		}
		if strings.Contains(a, "rg") {
			t.Errorf("localAllowReadOnly must grant no rg (command-execution primitive); found %q", a)
		}
	}
}

// TestWriteLocalAllowlist_WritesLocalProfileSettings covers writeLocalAllowlist
// (mirrors writeAllowlist, but through the new shared writeSettings core):
// the file must land at the same path writeAllowlist uses, and decode back to
// exactly localProfile's permissions.
func TestWriteLocalAllowlist_WritesLocalProfileSettings(t *testing.T) {
	ws := t.TempDir()
	findingsDir := filepath.Join(t.TempDir(), "findings")

	path, err := writeLocalAllowlist(ws, findingsDir)
	if err != nil {
		t.Fatalf("writeLocalAllowlist: %v", err)
	}
	want := filepath.Join(ws, ".claude", "settings.local.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var s allowlistSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}

	wantPerms := localProfile(findingsDir)
	if s.Permissions.DefaultMode != wantPerms.DefaultMode {
		t.Errorf("DefaultMode = %q, want %q", s.Permissions.DefaultMode, wantPerms.DefaultMode)
	}
	if !equalArgs(s.Permissions.Allow, wantPerms.Allow) {
		t.Errorf("Allow = %v, want %v", s.Permissions.Allow, wantPerms.Allow)
	}
	if !equalArgs(s.Permissions.Deny, wantPerms.Deny) {
		t.Errorf("Deny = %v, want %v", s.Permissions.Deny, wantPerms.Deny)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
