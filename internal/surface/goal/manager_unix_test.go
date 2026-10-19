//go:build unix

package goal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveNativeBinaryInspectsInjectedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // Test helper must be executable.
		t.Fatal(err)
	}
	manager := Manager{ResolveNativeBinary: func(harness string) (string, error) {
		if harness != "pi" {
			t.Fatalf("resolved harness %q, want pi", harness)
		}
		return path, nil
	}}
	got, err := manager.resolveNativeBinary("pi")
	if err != nil {
		t.Fatalf("resolveNativeBinary: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != canonical || len(got.SHA256) != 64 {
		t.Fatalf("resolved receipt = %#v, want canonical path %q and digest", got, canonical)
	}
}

func TestResolveNativeBinaryRefusesMissingResolverAndInvalidPath(t *testing.T) {
	manager := Manager{}
	if _, err := manager.resolveNativeBinary("claude"); err == nil || !strings.Contains(err.Error(), "resolver is unavailable") {
		t.Fatalf("missing resolver error = %v", err)
	}
	want := errors.New("resolver refused binary")
	manager.ResolveNativeBinary = func(string) (string, error) { return "", want }
	if _, err := manager.resolveNativeBinary("claude"); !errors.Is(err, want) {
		t.Fatalf("resolver failure = %v, want %v", err, want)
	}
	manager.ResolveNativeBinary = func(string) (string, error) { return "relative", nil }
	if _, err := manager.resolveNativeBinary("claude"); err == nil || !strings.Contains(err.Error(), "absolute paths") {
		t.Fatalf("invalid resolved path error = %v", err)
	}
}

func TestManagerStartRefusesUnresolvedNativeBinaryBeforeStateCreation(t *testing.T) {
	prepared, err := Prepare(t.Context(), goalSpec(newGoalRepository(t)))
	if err != nil {
		t.Fatal(err)
	}
	team := prepared.Spec.EffectiveTeam()
	team.Implementation.Harness, team.Implementation.Provider = "pi", "openai-codex"
	prepared.Spec.Team = &team
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "registry")
	manager := Manager{Store: Store{Root: root}, SelfBinary: executable, CodexBinary: executable}
	if _, err := manager.Start(t.Context(), prepared); err == nil || !strings.Contains(err.Error(), "native binary resolver is unavailable") {
		t.Fatalf("Start error = %v, want native resolver refusal", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("native resolution failure created state: %v", err)
	}
}
