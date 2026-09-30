package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
)

// stubUpgradeLookPath overrides upgradeLookPath for the duration of the
// test, restoring it via t.Cleanup — mirrors stubConfirmSeams's pattern
// (update_test.go).
func stubUpgradeLookPath(t *testing.T, found ...string) {
	t.Helper()
	set := make(map[string]bool, len(found))
	for _, n := range found {
		set[n] = true
	}
	prev := upgradeLookPath
	upgradeLookPath = func(name string) (string, error) {
		if set[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { upgradeLookPath = prev })
}

func setMetaVersion(t *testing.T, v string) {
	t.Helper()
	prev := meta.Version
	meta.Version = v
	t.Cleanup(func() { meta.Version = prev })
}

func execUpgrade(t *testing.T, runner exec.Runner, args ...string) (stdout string, err error) {
	t.Helper()
	deps := module.Deps{Cfg: config.Config{}, Runner: runner}
	cmd := newUpgradeCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

func TestUpgrade_SourceBuild_WarnsAndExitsZero(t *testing.T) {
	setMetaVersion(t, "dev")
	stubUpgradeLookPath(t, "brew") // present but must never be consulted

	fr := &exec.FakeRunner{}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil (source build warns, never refuses)", err, ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("built from source")) {
		t.Errorf("stdout = %q, want the source-build warning", stdout)
	}
	if len(fr.Calls) != 0 {
		t.Errorf("source build invoked %d shell command(s), want 0 (nothing to upgrade)", len(fr.Calls))
	}
}

func TestUpgrade_BrewMissing_ExitsOne(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t) // nothing found

	fr := &exec.FakeRunner{}
	_, err := execUpgrade(t, fr)
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew missing)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
}

func TestUpgrade_Check_UpToDate(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("up to date")) {
		t.Errorf("stdout = %q, want an up-to-date message", stdout)
	}
	// --check must never touch `brew upgrade` — only `brew outdated`.
	for _, c := range fr.Calls {
		if len(c.Args) > 0 && c.Args[0] == "upgrade" {
			t.Errorf("--check ran %v, want no mutating brew upgrade call", c.Args)
		}
	}
}

func TestUpgrade_Check_Outdated(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "cameronsjo/tap/forgectl (1.0.0) < 1.1.0", nil
	}}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("update available")) {
		t.Errorf("stdout = %q, want an update-available message", stdout)
	}
}

func TestUpgrade_Apply_RunsUpdateThenUpgrade(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "done", nil }}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d brew calls, want 2 (update, upgrade): %+v", len(fr.Calls), fr.Calls)
	}
	if fr.Calls[0].Args[0] != "update" || fr.Calls[1].Args[0] != "upgrade" {
		t.Errorf("call order = %v then %v, want update then upgrade", fr.Calls[0].Args, fr.Calls[1].Args)
	}
	if fr.Calls[1].Args[len(fr.Calls[1].Args)-1] != selfupdate.CaskRef {
		t.Errorf("upgrade call = %v, want it to name the cask %s", fr.Calls[1].Args, selfupdate.CaskRef)
	}
}

func TestUpgrade_Apply_UpdateFailure_NeverRunsUpgrade(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, Stderr: "network unreachable"}
	}}
	_, err := execUpgrade(t, fr)
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew update failed)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if len(fr.Calls) != 1 {
		t.Errorf("got %d calls, want 1 — a failed update must never reach upgrade: %+v", len(fr.Calls), fr.Calls)
	}
}

// TestUpgrade_Check_NeverEchoesBrew pins #738: `upgrade --check` words both
// outcomes from fixed text and version tokens, never from brew's stdout or
// its CommandError (argv plus stderr, which relays the tap's server).
func TestUpgrade_Check_NeverEchoesBrew(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	const marker = "SERVERTEXT\x1b[2J"

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return marker + " cameronsjo/tap/forgectl (1.0.0_1) != 1.1.0 " + marker, nil
	}}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if want := "update available: forgectl 1.0.0_1 installed, 1.1.0 available\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	fr = &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, Stderr: marker}
	}}
	stdout, err = execUpgrade(t, fr, "--check")
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew outdated failed)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if msg := err.Error() + stdout; bytes.Contains([]byte(msg), []byte("SERVERTEXT")) || bytes.Contains([]byte(msg), []byte("outdated --cask")) {
		t.Errorf("output = %q, echoes brew's text or argv", msg)
	}
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		t.Errorf("the CommandError is no longer on the chain: %v", err)
	}
}
