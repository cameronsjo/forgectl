package resume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/claudeproject"
)

// TestLastPermissionMode covers the extraction alone: which transcript lines
// count and which value wins. Validation is the caller's (launch's tests).
func TestLastPermissionMode(t *testing.T) {
	huge := `{"type":"user","permissionMode":"auto","message":{"content":"` + strings.Repeat("x", transcriptBufSize+10) + `"}}`
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"empty transcript", nil, ""},
		{"no mode recorded", []string{`{"type":"user","message":{"content":"hi"}}`}, ""},
		{"permission-mode record", []string{`{"type":"permission-mode","permissionMode":"acceptEdits","sessionId":"s"}`}, "acceptEdits"},
		{"user prompt field", []string{`{"type":"user","permissionMode":"bypassPermissions","message":{"content":"hi"}}`}, "bypassPermissions"},
		{"last one wins", []string{
			`{"type":"permission-mode","permissionMode":"plan"}`,
			`{"type":"user","permissionMode":"acceptEdits"}`,
			`{"type":"assistant","message":{"content":"ok"}}`,
			`{"type":"permission-mode","permissionMode":"bypassPermissions"}`,
		}, "bypassPermissions"},
		{"sidechain skipped", []string{
			`{"type":"user","permissionMode":"auto"}`,
			`{"type":"user","isSidechain":true,"permissionMode":"plan"}`,
		}, "auto"},
		{"nested key ignored", []string{
			`{"type":"user","permissionMode":"default"}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"permissionMode":"bypassPermissions"}}]}}`,
		}, "default"},
		{"quoted text in a body ignored", []string{
			`{"type":"user","permissionMode":"plan"}`,
			`{"type":"user","message":{"content":"set \"permissionMode\":\"bypassPermissions\""}}`,
		}, "plan"},
		{"empty value keeps the earlier one", []string{
			`{"type":"user","permissionMode":"acceptEdits"}`,
			`{"type":"user","permissionMode":""}`,
		}, "acceptEdits"},
		{"malformed line skipped", []string{
			`{"type":"user","permissionMode":"auto"}`,
			`{"type":"user","permissionMode":`,
		}, "auto"},
		{"over-long line skipped, later ones still read", []string{
			`{"type":"user","permissionMode":"plan"}`,
			huge,
			`{"type":"permission-mode","permissionMode":"dontAsk"}`,
		}, "dontAsk"},
		{"unknown value returned as found", []string{`{"type":"user","permissionMode":"--dangerously-skip-permissions"}`}, "--dangerously-skip-permissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Join(tc.lines, "\n")
			if body != "" {
				body += "\n"
			}
			if got := lastPermissionMode(strings.NewReader(body)); got != tc.want {
				t.Errorf("lastPermissionMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastPermissionMode_LocatesTranscript checks the path lookup is the one
// Scan uses: the cwd slug first, then any project dir holding <id>.jsonl, and
// "" when there is no transcript.
func TestLastPermissionMode_LocatesTranscript(t *testing.T) {
	root := t.TempDir()
	p := Paths{ClaudeHome: filepath.Join(root, ".claude"), StoreDir: filepath.Join(root, "store")}
	cwd := filepath.Join(root, "repo")
	write := func(dir, id, body string) {
		t.Helper()
		d := filepath.Join(p.projectsDir(), dir)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, id+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(claudeproject.Slug(cwd), testSID, `{"type":"permission-mode","permissionMode":"acceptEdits"}`+"\n")
	write("elsewhere", "bbbbbbbb-0000-0000-0000-000000000002", `{"type":"user","permissionMode":"auto"}`+"\n")

	cases := []struct {
		name, id, cwd, want string
	}{
		{"by cwd slug", testSID, cwd, "acceptEdits"},
		{"by search when the cwd is unknown", "bbbbbbbb-0000-0000-0000-000000000002", "", "auto"},
		{"no transcript", "cccccccc-0000-0000-0000-000000000003", cwd, ""},
		{"no id", "", cwd, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LastPermissionMode(p, tc.id, tc.cwd); got != tc.want {
				t.Errorf("LastPermissionMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRelaunchLineKeepsPermissionMode pins the line a restart types: it
// carries the flag that makes the resume keep the session's mode, and the
// by-hand command matches it.
func TestRelaunchLineKeepsPermissionMode(t *testing.T) {
	e := SystemRestartEnv{forgectl: "/usr/local/bin/forgectl"}
	line, err := e.relaunchLine(testSID)
	if err != nil {
		t.Fatalf("relaunchLine: %v", err)
	}
	want := "/usr/local/bin/forgectl resume " + KeepPermissionModeFlag + " " + testSID
	if strings.ReplaceAll(line, "'", "") != want {
		t.Errorf("relaunch line = %q, want %q (quoting aside)", line, want)
	}
	if got := ManualResume(testSID); got != "forgectl resume "+KeepPermissionModeFlag+" "+testSID {
		t.Errorf("ManualResume = %q", got)
	}
}
