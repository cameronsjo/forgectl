package resume

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestVersionCompare_IsNumericNotLexical(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2.1.99", "2.1.100", -1},
		{"2.1.100", "2.1.99", 1},
		{"2.1.285", "2.1.285", 0},
		{"2.1", "2.1.0", 0},
		{"2.10.0", "2.9.9", 1},
		{"3", "2.99.99", 1},
	}
	for _, tt := range tests {
		a, err := ParseVersion(tt.a)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.a, err)
		}
		b, err := ParseVersion(tt.b)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.b, err)
		}
		if got := a.Compare(b); got != tt.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseVersion_RefusesWhatItCannotRank(t *testing.T) {
	for _, s := range []string{"", "  ", "v2.1.0", "2.1.0-beta", "2..1", "2.1.", "+2.1", "-1.0", "abc", "2.1.0 (Claude Code)"} {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", s, v)
		}
	}
}

func TestIsBusy_UnknownStatusIsBusy(t *testing.T) {
	for status, want := range map[string]bool{
		"idle": false, "busy": true, "shell": true, "waiting": true,
		"": true, "compacting": true, "IDLE": true,
	} {
		if got := IsBusy(status); got != want {
			t.Errorf("IsBusy(%q) = %v, want %v", status, got, want)
		}
	}
}

func liveEntry(id string, pid int, version, status string) RegistryEntry {
	return RegistryEntry{Pid: pid, SessionID: id, Cwd: "/w/" + id, Version: version, Status: status, Live: true}
}

func TestFindOutdated_ListsOnlyOlderLiveSessions(t *testing.T) {
	dead := liveEntry("dead", 4, "2.1.1", "idle")
	dead.Live = false
	entries := []RegistryEntry{
		liveEntry("b-old", 1, "2.1.99", "busy"),
		liveEntry("c-current", 2, "2.1.100", "idle"),
		liveEntry("d-newer", 3, "2.1.101", "idle"),
		dead,
		liveEntry("a-odd", 5, "2.1.5", "compacting"),
		liveEntry("e-junk", 6, "garbage", "idle"),
	}
	panes := map[int]string{1: "w1-2"}
	got, err := FindOutdated(entries, "2.1.100", func(pid int) string { return panes[pid] })
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, o := range got {
		ids = append(ids, o.SessionID)
	}
	if want := "a-odd,b-old,e-junk"; strings.Join(ids, ",") != want {
		t.Fatalf("listed %v, want %s (dead, current, newer excluded; sorted)", ids, want)
	}
	byID := map[string]OutdatedSession{}
	for _, o := range got {
		byID[o.SessionID] = o
	}
	if !byID["a-odd"].Busy {
		t.Error("unknown status must read as busy")
	}
	if byID["b-old"].Pane != "w1-2" || byID["a-odd"].Pane != "" {
		t.Errorf("pane lookup misapplied: %+v", got)
	}
	if !byID["e-junk"].VersionUnparseable || byID["b-old"].VersionUnparseable {
		t.Errorf("unparseable flag wrong: %+v", got)
	}
	if byID["b-old"].InstalledVersion != "2.1.100" {
		t.Errorf("installed version = %q", byID["b-old"].InstalledVersion)
	}
}

func TestOutdated_ExcludesDeadPidsFromRegistry(t *testing.T) {
	f := newFixture(t)
	reg := func(pid int, id, version string) {
		f.write(filepath.Join(f.Paths.registryDir(), itoa(pid)+".json"),
			`{"pid":`+itoa(pid)+`,"sessionId":"`+id+`","cwd":"/w","version":"`+version+`","status":"idle"}`)
	}
	reg(101, "aaaa1111", "2.1.50")
	reg(102, "bbbb2222", "2.1.50")
	pinPids(t, map[int]bool{101: true, 102: false})

	got, err := Outdated(f.Paths, "2.1.285", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "aaaa1111" {
		t.Fatalf("got %+v, want only the live session", got)
	}
}

func TestOutdated_RefusesUnparseableInstalled(t *testing.T) {
	if _, err := Outdated(newFixture(t).Paths, "nope", nil); err == nil {
		t.Fatal("want an error for an unparseable installed version")
	}
}

func versionRunner(out string, err error) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, err }}
}

// installLayout builds a real directory tree: files named by each key, and
// bin/claude as a symlink to link (relative to root) when link is non-empty.
func installLayout(t *testing.T, files []string, link string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if link != "" {
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, link), filepath.Join(root, "bin", "claude")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestInstalledVersion_PrefersVersionsSymlink(t *testing.T) {
	root := installLayout(t, []string{"share/claude/versions/2.1.285"}, "share/claude/versions/2.1.285")
	run := versionRunner("9.9.9 (Claude Code)", nil)
	got, err := InstalledVersion(context.Background(), filepath.Join(root, "bin", "claude"), run)
	if err != nil || got != "2.1.285" {
		t.Fatalf("got %q, %v; want 2.1.285", got, err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("spawned a process despite a usable symlink: %+v", run.Calls)
	}
}

func TestInstalledVersion_FallsBackToVersionFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		files []string
		link  string
		bin   string // relative to root
	}{
		"plain file named like a version": {files: []string{"opt/2.1"}, bin: "opt/2.1"},
		"symlink outside a versions dir":  {files: []string{"opt/2.1.290"}, link: "opt/2.1.290", bin: "bin/claude"},
		"pre-release version dir entry":   {files: []string{"versions/2.1.286-beta"}, link: "versions/2.1.286-beta", bin: "bin/claude"},
		"binary does not exist":           {bin: "nowhere/claude"},
	} {
		t.Run(name, func(t *testing.T) {
			root := installLayout(t, tc.files, tc.link)
			run := versionRunner("2.1.285 (Claude Code)\n", nil)
			got, err := InstalledVersion(context.Background(), filepath.Join(root, tc.bin), run)
			if err != nil || got != "2.1.285" {
				t.Fatalf("got %q, %v; want 2.1.285 from --version", got, err)
			}
			if len(run.Calls) != 1 || run.Calls[0].Args[0] != "--version" {
				t.Errorf("calls = %+v, want one --version", run.Calls)
			}
		})
	}
}

func TestInstalledVersion_ErrorNamesEverythingTried(t *testing.T) {
	root := installLayout(t, []string{"opt/claude"}, "")
	bin := filepath.Join(root, "opt", "claude")
	_, err := InstalledVersion(context.Background(), bin, versionRunner("", errors.New("boom")))
	if err == nil || !strings.Contains(err.Error(), "not a symlink") || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("error should name both attempts, got %v", err)
	}
	_, err = InstalledVersion(context.Background(), bin, versionRunner("garbage output", nil))
	if err == nil || !strings.Contains(err.Error(), "garbage output") {
		t.Fatalf("error should quote the unusable output, got %v", err)
	}
}

func pinEnv(t *testing.T, env []string, err error) {
	t.Helper()
	prev := processEnv
	processEnv = func(int) ([]string, error) { return env, err }
	t.Cleanup(func() { processEnv = prev })
}

func TestPaneFor(t *testing.T) {
	t.Run("reads the environment entry", func(t *testing.T) {
		pinEnv(t, []string{"A=1", "HERDR_PANE_ID=p_7", "B=2"}, nil)
		if got := PaneFor()(42); got != "p_7" {
			t.Errorf("pane = %q", got)
		}
	})
	t.Run("first entry wins, as getenv", func(t *testing.T) {
		pinEnv(t, []string{"HERDR_PANE_ID=w1:p2", "HERDR_PANE_ID=w9:p9"}, nil)
		if got := PaneFor()(42); got != "w1:p2" {
			t.Errorf("pane = %q", got)
		}
	})
	t.Run("read failure leaves the pane empty", func(t *testing.T) {
		pinEnv(t, nil, errors.New("permission denied"))
		if got := PaneFor()(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("missing pane id is empty", func(t *testing.T) {
		pinEnv(t, []string{"HOME=/x"}, nil)
		if got := PaneFor()(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("an id outside the safe alphabet is unknown", func(t *testing.T) {
		pinEnv(t, []string{"HERDR_PANE_ID=\x1b]0;x\x07"}, nil)
		if got := PaneFor()(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("a non-positive pid reads nothing", func(t *testing.T) {
		pinEnv(t, []string{"HERDR_PANE_ID=p_7"}, nil)
		if got := PaneFor()(0); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
}

// procArgs2 builds a KERN_PROCARGS2 buffer the way the kernel lays it out.
func procArgs2(execPath string, argv, env []string) []byte {
	var argc uint32
	for range argv {
		argc++
	}
	buf := binary.NativeEndian.AppendUint32(nil, argc)
	buf = append(buf, execPath...)
	buf = append(buf, 0, 0, 0, 0) // terminator plus alignment padding
	for _, a := range argv {
		buf = append(append(buf, a...), 0)
	}
	for _, e := range env {
		buf = append(append(buf, e...), 0)
	}
	return append(buf, 0, 'j', 'u', 'n', 'k', 0) // trailing apple[] strings after an empty entry
}

func TestParseProcArgs2_KeepsArgvOutOfTheEnvironment(t *testing.T) {
	buf := procArgs2("/usr/local/bin/claude",
		[]string{"claude", "--append-system-prompt", "HERDR_PANE_ID=decoy"},
		[]string{"HOME=/Users/u", "HERDR_PANE_ID=w7H:p6", "TERM=xterm"})
	execPath, env, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatal(err)
	}
	if execPath != "/usr/local/bin/claude" {
		t.Errorf("exec path = %q, want the path before argv", execPath)
	}
	if got := strings.Join(env, ","); got != "HOME=/Users/u,HERDR_PANE_ID=w7H:p6,TERM=xterm" {
		t.Fatalf("env = %q", got)
	}
	if got := paneFromEnv(env); got != "w7H:p6" {
		t.Errorf("pane = %q, want the environment's value, not the argv decoy", got)
	}
}

func TestParseProcArgs2_RefusesMalformedBuffers(t *testing.T) {
	good := procArgs2("/bin/claude", []string{"claude", "x"}, []string{"A=1"})
	for name, buf := range map[string][]byte{
		"empty":                  nil,
		"argc only":              good[:4],
		"unterminated path":      []byte{1, 0, 0, 0, '/', 'b'},
		"argv shorter than argc": append(binary.NativeEndian.AppendUint32(nil, 5), "/bin/c\x00\x00a\x00"...),
	} {
		if _, env, err := parseProcArgs2(buf); err == nil {
			t.Errorf("%s: env = %q, want an error", name, env)
		}
	}
}

// TestReadProcessEnv_ReadsThisProcess exercises the real platform reader
// (sysctl on macOS, /proc elsewhere) against the test binary itself.
func TestReadProcessEnv_ReadsThisProcess(t *testing.T) {
	env, err := readProcessEnv(os.Getpid())
	if err != nil {
		t.Skipf("platform cannot read process environments here: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			return
		}
	}
	t.Fatalf("no PATH= entry among %d environment entries", len(env))
}
