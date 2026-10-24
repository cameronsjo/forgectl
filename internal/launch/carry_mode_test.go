package launch

import (
	"slices"
	"strings"
	"testing"
)

func TestIsPermissionMode(t *testing.T) {
	for _, v := range claudePermissionRank.known() {
		if !IsPermissionMode(v) {
			t.Errorf("IsPermissionMode(%q) = false for a ranked mode", v)
		}
	}
	for _, v := range []string{"", "Plan", " plan", "plan ", "plan\n", "--dangerously-skip-permissions", "bypass", "plan --model x", "acceptedits"} {
		if IsPermissionMode(v) {
			t.Errorf("IsPermissionMode(%q) = true", v)
		}
	}
}

// TestCarryPermissionModeArgv drives the transcript mode through
// CarryPermissionMode into ResumeArgs and checks the argv a resume would exec.
func TestCarryPermissionModeArgv(t *testing.T) {
	const sid = "432d45ef-ef70-4f06-967b-ee452b93abb9"
	base := Profile{Harness: "claude", Model: "opus", PermissionMode: "plan", AllowDanger: true}
	noDanger := base
	noDanger.AllowDanger = false
	bypassPosture := noDanger
	bypassPosture.PermissionMode = "bypassPermissions"

	cases := []struct {
		name     string
		profile  Profile
		mode     string
		wantMode string
		carried  bool
		noteHas  string
	}{
		{"no mode recorded", base, "", "plan", false, "no permission mode recorded"},
		{"plan stays plan", base, "plan", "plan", true, "last permission mode, plan"},
		{"acceptEdits carried", base, "acceptEdits", "acceptEdits", true, "acceptEdits"},
		{"auto carried", base, "auto", "auto", true, "auto"},
		{"default carried", base, "default", "default", true, "default"},
		{"manual carried", base, "manual", "manual", true, "manual"},
		{"dontAsk carried", base, "dontAsk", "dontAsk", true, "dontAsk"},
		{"bypass carried with allow_danger", base, "bypassPermissions", "bypassPermissions", true, "bypassPermissions"},
		{"bypass refused without allow_danger", noDanger, "bypassPermissions", "plan", false, "cannot reach"},
		{"bypass carried when the posture is bypass", bypassPosture, "bypassPermissions", "bypassPermissions", true, "bypassPermissions"},
		{"acceptEdits carried without allow_danger", noDanger, "acceptEdits", "acceptEdits", true, "acceptEdits"},
		{"unknown mode refused", base, "yolo", "plan", false, "not one forgectl knows"},
		{"flag-shaped value refused", base, "--dangerously-skip-permissions", "plan", false, "not one forgectl knows"},
		{"value with spaces refused", base, "plan --model haiku", "plan", false, "not one forgectl knows"},
		{"wrong case refused", base, "BypassPermissions", "plan", false, "not one forgectl knows"},
		{"control bytes refused", base, "plan\x1b[2J", "plan", false, "not one forgectl knows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, note := CarryPermissionMode(tc.profile, tc.mode)
			if p.PermissionMode != tc.wantMode {
				t.Fatalf("PermissionMode = %q, want %q", p.PermissionMode, tc.wantMode)
			}
			if !strings.Contains(note, tc.noteHas) {
				t.Errorf("note = %q, want it to contain %q", note, tc.noteHas)
			}
			if !tc.carried && !strings.Contains(note, "resuming in the posture's "+tc.profile.PermissionMode) {
				t.Errorf("fallback note = %q, want it to name the posture's mode", note)
			}
			if strings.ContainsAny(note, "\x1b\n") {
				t.Errorf("note carries control bytes: %q", note)
			}
			if p.AllowDanger != tc.profile.AllowDanger {
				t.Errorf("AllowDanger changed: %v -> %v", tc.profile.AllowDanger, p.AllowDanger)
			}

			args := ResumeArgs(p, sid, false)
			if len(args) < 2 || args[0] != "--permission-mode" || args[1] != tc.wantMode {
				t.Fatalf("argv = %q, want it to start --permission-mode %s", args, tc.wantMode)
			}
			if n := countFlag(args, "--permission-mode"); n != 1 {
				t.Errorf("argv has %d --permission-mode flags, want 1: %q", n, args)
			}
			if got := slices.Contains(args, "--allow-dangerously-skip-permissions"); got != tc.profile.AllowDanger {
				t.Errorf("--allow-dangerously-skip-permissions present = %v, want %v", got, tc.profile.AllowDanger)
			}
			if i := slices.Index(args, "--resume"); i < 0 || i+1 >= len(args) || args[i+1] != sid {
				t.Errorf("argv = %q, want --resume %s", args, sid)
			}
		})
	}
}

func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}
