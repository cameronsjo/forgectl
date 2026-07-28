package selfupdate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
)

func TestIsSourceBuild(t *testing.T) {
	orig := meta.Version
	t.Cleanup(func() { meta.Version = orig })

	meta.Version = "dev"
	if !IsSourceBuild() {
		t.Error("IsSourceBuild() = false with meta.Version=dev, want true")
	}

	meta.Version = "1.2.3"
	if IsSourceBuild() {
		t.Error("IsSourceBuild() = true with meta.Version=1.2.3, want false")
	}
}

func TestCheckOutdated_UpToDate(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", nil
	}}
	outdated, detail, err := CheckOutdated(context.Background(), fr)
	if err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	if outdated {
		t.Errorf("outdated = true on empty brew output, want false")
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty", detail)
	}
	assertBrewArgv(t, fr, []string{"brew", "outdated", "--cask", CaskRef})
	assertNoAutoUpdate(t, fr)
}

func TestCheckOutdated_Outdated(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "cameronsjo/tap/forgectl (0.9.0) < 0.10.0", nil
	}}
	outdated, detail, err := CheckOutdated(context.Background(), fr)
	if err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	if !outdated {
		t.Error("outdated = false with non-empty brew output, want true")
	}
	if detail == "" {
		t.Error("detail is empty, want brew's outdated line")
	}
}

func TestCheckOutdated_Error(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: "brew", Stderr: "brew: command not found", ExitCode: 127, Err: errors.New("exit status 127")}
	}}
	if _, _, err := CheckOutdated(context.Background(), fr); err == nil {
		t.Error("CheckOutdated returned nil error on a real brew failure")
	}
}

func TestUpgrade_RunsUpdateThenUpgradeInOrder(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return strings.Join(append([]string{name}, args...), " ") + " ok", nil
	}}
	out, err := Upgrade(context.Background(), fr)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(fr.Calls), fr.Calls)
	}
	assertArgv(t, fr.Calls[0], []string{"brew", "update"})
	assertArgv(t, fr.Calls[1], []string{"brew", "upgrade", "--cask", CaskRef})
	if !strings.Contains(out, "brew update ok") || !strings.Contains(out, "brew upgrade --cask "+CaskRef+" ok") {
		t.Errorf("Upgrade output = %q, want both steps' output present", out)
	}
}

func TestUpgrade_StopsAfterUpdateFailure(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "brew" {
			return "", &exec.CommandError{Name: "brew", Stderr: "network unreachable", Err: errors.New("exit status 1")}
		}
		return "", nil
	}}
	_, err := Upgrade(context.Background(), fr)
	if err == nil {
		t.Fatal("Upgrade returned nil error when brew update failed")
	}
	// A failed `brew update` must never reach `brew upgrade` — that's the
	// "never leaves a half-applied step" guarantee this test pins.
	if len(fr.Calls) != 1 {
		t.Fatalf("got %d calls, want 1 (upgrade must not run after update fails): %+v", len(fr.Calls), fr.Calls)
	}
}

func assertArgv(t *testing.T, call exec.Call, want []string) {
	t.Helper()
	got := append([]string{call.Name}, call.Args...)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func assertBrewArgv(t *testing.T, fr *exec.FakeRunner, want []string) {
	t.Helper()
	if len(fr.Calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(fr.Calls), fr.Calls)
	}
	assertArgv(t, fr.Calls[0], want)
}

func assertNoAutoUpdate(t *testing.T, fr *exec.FakeRunner) {
	t.Helper()
	assertHomebrewSafeEnv(t, fr.Last())
}

// assertHomebrewSafeEnv pins the full hardened env set on one brew call:
// HOMEBREW_NO_AUTO_UPDATE=1, plus HOMEBREW_ARTIFACT_DOMAIN,
// HOMEBREW_CASK_OPTS, and HOMEBREW_BREW_GIT_REMOTE forced to "" — an
// ambient value for any of the latter three (e.g. from a direnv-managed
// .envrc) could redirect where the upgrade artifact or tap comes from on
// the one command whose output is a new binary on $PATH.
func assertHomebrewSafeEnv(t *testing.T, call exec.Call) {
	t.Helper()
	want := map[string]string{
		"HOMEBREW_NO_AUTO_UPDATE":  "1",
		"HOMEBREW_ARTIFACT_DOMAIN": "",
		"HOMEBREW_CASK_OPTS":       "",
		"HOMEBREW_BREW_GIT_REMOTE": "",
	}
	for k, v := range want {
		if got, ok := call.Env[k]; !ok || got != v {
			t.Errorf("%s = %q (present=%v), want %q — a security-relevant HOMEBREW_* var isn't pinned on this brew call", k, got, ok, v)
		}
	}
}

// TestBrewCalls_PinSecurityRelevantEnv exercises every brew-shelling entry
// point in this package and confirms each pins the full hardened env, not
// just HOMEBREW_NO_AUTO_UPDATE — a regression here would let an ambient
// HOMEBREW_ARTIFACT_DOMAIN/CASK_OPTS/BREW_GIT_REMOTE redirect brew's
// download or tap on `forgectl upgrade`, the one command whose output is a
// new binary on $PATH.
func TestBrewCalls_PinSecurityRelevantEnv(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	if _, _, err := CheckOutdated(context.Background(), fr); err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	assertHomebrewSafeEnv(t, fr.Last())

	fr = &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	if _, err := Upgrade(context.Background(), fr); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	for _, call := range fr.Calls {
		assertHomebrewSafeEnv(t, call)
	}
}
