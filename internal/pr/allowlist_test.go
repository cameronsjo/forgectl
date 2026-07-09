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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
