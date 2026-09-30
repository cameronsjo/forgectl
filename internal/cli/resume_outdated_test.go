package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

func outdatedFixture(t *testing.T, installed string, installedErr error) {
	t.Helper()
	root := t.TempDir()
	sessions := filepath.Join(root, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Our own pid is guaranteed live; pid 1<<22+1 exceeds every default pid_max.
	self := os.Getpid()
	write("a.json", `{"pid":`+itoaCLI(self)+`,"sessionId":"aaaa1111","cwd":"/w/a","version":"2.1.99","status":"busy"}`)
	write("b.json", `{"pid":`+itoaCLI(self)+`,"sessionId":"bbbb2222","cwd":"/w/b","version":"2.1.100","status":"idle"}`)
	write("c.json", `{"pid":4194305,"sessionId":"cccc3333","cwd":"/w/c","version":"2.1.1","status":"idle"}`)

	prevPaths, prevInstalled := resumePaths, installedVersionFn
	resumePaths = func() (resume.Paths, error) {
		return resume.Paths{ClaudeHome: filepath.Join(root, ".claude"), StoreDir: filepath.Join(root, "store")}, nil
	}
	installedVersionFn = func(context.Context, module.Deps) (string, error) { return installed, installedErr }
	t.Cleanup(func() { resumePaths, installedVersionFn = prevPaths, prevInstalled })
}

func itoaCLI(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func runOutdated(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newResumeOutdatedCmd(module.Deps{Runner: &exec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestResumeOutdated_JSONListsOnlyOlderLive(t *testing.T) {
	outdatedFixture(t, "2.1.100", nil)
	out, err := runOutdated(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (dead pid and current version excluded): %s", len(got), out)
	}
	row := got[0]
	for _, k := range []string{"session_id", "pid", "cwd", "status", "busy", "version", "installed_version", "version_unparseable", "pane"} {
		if _, ok := row[k]; !ok {
			t.Errorf("missing stable field %q in %v", k, row)
		}
	}
	if row["session_id"] != "aaaa1111" || row["version"] != "2.1.99" || row["installed_version"] != "2.1.100" || row["busy"] != true {
		t.Errorf("unexpected row %v", row)
	}
}

func TestResumeOutdated_EmptyIsEmptyArrayAndExitsZero(t *testing.T) {
	outdatedFixture(t, "2.1.99", nil)
	out, err := runOutdated(t, "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("got %q, %v; want [] and no error", out, err)
	}
	if out, err = runOutdated(t); err != nil || out != "" {
		t.Fatalf("piped table for nothing outdated = %q, %v; want silence", out, err)
	}
}

func TestPrintOutdated_TableAndTTYMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := printOutdated(&buf, nil, false, true); err != nil || strings.TrimSpace(buf.String()) != "no outdated sessions" {
		t.Fatalf("tty empty = %q, %v", buf.String(), err)
	}
	buf.Reset()
	list := []resume.OutdatedSession{
		{
			SessionID: "s1", Cwd: "/w/\x1b[31mred", Status: "busy",
			Version: "2.1.9", InstalledVersion: "2.1.10", Pane: "",
		},
		{
			SessionID: "s2", Cwd: "/w", Status: "idle",
			Version: "2.1\tx", InstalledVersion: "2.1.10", Pane: "w7H:p6", VersionUnparseable: true,
		},
	}
	if err := printOutdated(&buf, list, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("raw escape reached the terminal: %q", buf.String())
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		f := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1)
		rows[f[0]] = f
	}
	for id, want := range map[string][]string{
		"SESSION": {"SESSION", "STATUS", "VERSION", "INSTALLED", "HERDR_PANE_ID", "CWD"},
		"s1":      {"s1", "busy", "2.1.9", "2.1.10", "-"},
		"s2":      {"s2", "idle", `"2.1\tx" (unparseable)`, "2.1.10", "w7H:p6", "/w"},
	} {
		got := rows[id]
		if len(got) < len(want) {
			t.Fatalf("row %s = %q, want prefix %q", id, got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %s column %d = %q, want %q (row %q)", id, i, got[i], w, got)
			}
		}
	}
}

func TestResumeOutdated_InstalledVersionFailureIsAnError(t *testing.T) {
	outdatedFixture(t, "", errors.New("could not determine the installed version"))
	if _, err := runOutdated(t); err == nil {
		t.Fatal("want an error when the installed version cannot be resolved")
	}
}
