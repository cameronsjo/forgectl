package resume

import (
	"context"
	"errors"
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
	installed, _ := ParseVersion("2.1.100")
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
	got := FindOutdated(entries, installed, "2.1.100", func(pid int) string { return panes[pid] })

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

func pinSymlink(t *testing.T, target string, err error) {
	t.Helper()
	prev := evalSymlinks
	evalSymlinks = func(string) (string, error) { return target, err }
	t.Cleanup(func() { evalSymlinks = prev })
}

func versionRunner(out string, err error) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, err }}
}

func TestInstalledVersion_PrefersSymlinkTarget(t *testing.T) {
	pinSymlink(t, "/home/u/.local/share/claude/versions/2.1.285", nil)
	run := versionRunner("9.9.9 (Claude Code)", nil)
	got, err := InstalledVersion(context.Background(), "/bin/claude", run)
	if err != nil || got != "2.1.285" {
		t.Fatalf("got %q, %v; want 2.1.285", got, err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("spawned a process despite a usable symlink: %+v", run.Calls)
	}
}

func TestInstalledVersion_FallsBackToVersionFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		target string
		err    error
	}{
		"target is not a version":  {"/opt/claude/bin/claude", nil},
		"symlink resolution fails": {"", errors.New("no such file")},
	} {
		t.Run(name, func(t *testing.T) {
			pinSymlink(t, tc.target, tc.err)
			run := versionRunner("2.1.285 (Claude Code)\n", nil)
			got, err := InstalledVersion(context.Background(), "/bin/claude", run)
			if err != nil || got != "2.1.285" {
				t.Fatalf("got %q, %v; want 2.1.285", got, err)
			}
			if len(run.Calls) != 1 || run.Calls[0].Args[0] != "--version" {
				t.Errorf("calls = %+v, want one --version", run.Calls)
			}
		})
	}
}

func TestInstalledVersion_ErrorNamesEverythingTried(t *testing.T) {
	pinSymlink(t, "/opt/claude/bin/claude", nil)
	_, err := InstalledVersion(context.Background(), "/bin/claude", versionRunner("", errors.New("boom")))
	if err == nil || !strings.Contains(err.Error(), "symlink target") || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("error should name both attempts, got %v", err)
	}
	_, err = InstalledVersion(context.Background(), "/bin/claude", versionRunner("garbage output", nil))
	if err == nil || !strings.Contains(err.Error(), "garbage output") {
		t.Fatalf("error should quote the unusable output, got %v", err)
	}
}

func TestPaneFor(t *testing.T) {
	ctx := context.Background()
	prevOS, prevEnv := hostOS, procEnviron
	t.Cleanup(func() { hostOS, procEnviron = prevOS, prevEnv })

	t.Run("darwin reads ps eww", func(t *testing.T) {
		hostOS = "darwin"
		run := versionRunner("claude --resume HOME=/Users/u HERDR_PANE_ID=w2-3 TERM=xterm", nil)
		if got := PaneFor(ctx, run)(42); got != "w2-3" {
			t.Errorf("pane = %q", got)
		}
		if c := run.Calls[0]; c.Name != "ps" || strings.Join(c.Args, " ") != "eww -o command= -p 42" {
			t.Errorf("call = %+v", c)
		}
	})
	t.Run("darwin ps failure leaves the pane empty", func(t *testing.T) {
		hostOS = "darwin"
		if got := PaneFor(ctx, versionRunner("", errors.New("ps: denied")))(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("missing pane id is empty", func(t *testing.T) {
		hostOS = "darwin"
		if got := PaneFor(ctx, versionRunner("claude HOME=/x", nil))(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("linux reads /proc environ", func(t *testing.T) {
		hostOS = "linux"
		procEnviron = func(int) ([]byte, error) { return []byte("A=1\x00HERDR_PANE_ID=p_7\x00B=2\x00"), nil }
		if got := PaneFor(ctx, &exec.FakeRunner{})(42); got != "p_7" {
			t.Errorf("pane = %q", got)
		}
	})
	t.Run("linux read failure leaves the pane empty", func(t *testing.T) {
		hostOS = "linux"
		procEnviron = func(int) ([]byte, error) { return nil, errors.New("permission denied") }
		if got := PaneFor(ctx, &exec.FakeRunner{})(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
	t.Run("an id outside the safe alphabet is unknown", func(t *testing.T) {
		hostOS = "linux"
		procEnviron = func(int) ([]byte, error) { return []byte("HERDR_PANE_ID=\x1b]0;x\x07\x00"), nil }
		if got := PaneFor(ctx, &exec.FakeRunner{})(42); got != "" {
			t.Errorf("pane = %q, want empty", got)
		}
	})
}
