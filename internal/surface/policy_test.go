package surface_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// executable writes a real file and marks it runnable. The policy stats the
// path, so the checks are exercised against the filesystem rather than against
// an injected stat that could be wrong in the same direction as the code.
func executable(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	// Owner-only, but the executable bit is the entire point: the policy
	// refuses a binary without it, so a mode gosec would prefer would make
	// every acceptance row here fail for the wrong reason.
	//nolint:gosec // G306: the executable bit is the property under test
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestPolicy_ProvenanceGate is the flag's whole job. An env variable or a
// config file naming a binary is an operator asserting *this one*; $PATH is
// whatever the ambient environment resolved, which is where a shim earlier in
// the path silently becomes the harness.
func TestPolicy_ProvenanceGate(t *testing.T) {
	bin := executable(t, "claude")
	self := executable(t, "forgectl")

	sources := []launch.BinarySource{
		launch.BinaryClaudeEnv,
		launch.BinaryClaudeConfig,
		launch.BinaryCodexEnv,
		launch.BinaryCodexConfig,
		launch.BinaryPiEnv,
		launch.BinaryPiConfig,
	}
	for _, source := range sources {
		t.Run(string(source)+" is accepted by default", func(t *testing.T) {
			err := surface.Policy{}.AcceptBinary(launch.ResolvedBinary{Path: bin, Source: source}, self)
			if err != nil {
				t.Errorf("explicit provenance %q was refused: %v", source, err)
			}
		})
	}

	pathBinary := launch.ResolvedBinary{Path: bin, Source: launch.BinaryPATH}

	err := surface.Policy{}.AcceptBinary(pathBinary, self)
	if !errors.Is(err, surface.ErrBinaryProvenance) {
		t.Errorf("a $PATH binary was accepted by default: err = %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "--allow-path-binary") {
		t.Errorf("the refusal does not name the flag that resolves it: %v", err)
	}

	if err := (surface.Policy{AllowPATHBinary: true}).AcceptBinary(pathBinary, self); err != nil {
		t.Errorf("--allow-path-binary did not accept a $PATH binary: %v", err)
	}
}

// TestPolicy_RefusesWithoutASelfPath is the fail-closed half of the self-loop
// guard. An empty self is a caller that never resolved one — the check would
// then be skipped silently, which is indistinguishable from a check that ran
// and passed. It is refused rather than warned about, because unlike a stat
// error it is a bad input rather than the guard failing at its own job.
func TestPolicy_RefusesWithoutASelfPath(t *testing.T) {
	bin := launch.ResolvedBinary{Path: executable(t, "claude"), Source: launch.BinaryClaudeEnv}

	if err := (surface.Policy{}).AcceptBinary(bin, ""); !errors.Is(err, surface.ErrBinaryUnusable) {
		t.Errorf("an unresolved self path was accepted: err = %v", err)
	}

	// A stat failure is the guard's own failure rather than a bad input, so it
	// admits — a guard must not block the operation it protects because it
	// could not run.
	absent := filepath.Join(t.TempDir(), "no-such-forgectl")
	if err := (surface.Policy{}).AcceptBinary(bin, absent); err != nil {
		t.Errorf("an unstattable self path blocked the launch: %v", err)
	}
}

// TestPolicy_OptingInToPATHWaivesNothingElse is the assertion that keeps the
// flag narrow. It acknowledges provenance risk; it is not a general override,
// and an operator who passes it still cannot point the surface at a directory
// or an unreadable file.
func TestPolicy_OptingInToPATHWaivesNothingElse(t *testing.T) {
	permissive := surface.Policy{AllowPATHBinary: true}
	self := executable(t, "forgectl")
	dir := t.TempDir()

	notExecutable := filepath.Join(dir, "claude.txt")
	if err := os.WriteFile(notExecutable, []byte("not a binary"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := map[string]string{
		"a directory":              dir,
		"a non-executable file":    notExecutable,
		"a path that is not there": filepath.Join(dir, "absent"),
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			err := permissive.AcceptBinary(
				launch.ResolvedBinary{Path: path, Source: launch.BinaryPATH}, self)
			if !errors.Is(err, surface.ErrBinaryUnusable) {
				t.Errorf("AcceptBinary(%s) err = %v, want ErrBinaryUnusable", name, err)
			}
		})
	}
}

// TestPolicy_ShapeChecks covers the path requirements that hold regardless of
// provenance.
func TestPolicy_ShapeChecks(t *testing.T) {
	policy := surface.Policy{}
	self := executable(t, "forgectl")

	tests := map[string]launch.ResolvedBinary{
		"empty path":         {Path: "", Source: launch.BinaryClaudeEnv},
		"relative path":      {Path: "bin/claude", Source: launch.BinaryClaudeEnv},
		"unknown provenance": {Path: executable(t, "claude"), Source: launch.BinarySource("smuggled")},
		"no provenance":      {Path: executable(t, "claude"), Source: ""},
	}
	for name, bin := range tests {
		t.Run(name, func(t *testing.T) {
			if err := policy.AcceptBinary(bin, self); err == nil {
				t.Errorf("AcceptBinary(%s) was accepted", name)
			}
		})
	}
}

// TestPolicy_RefusesASelfLoop stops forgectl from being its own harness. The
// check is by file identity rather than string comparison, so a symlink, a
// hard link, or a differently-spelled absolute path is caught too — each of
// which would otherwise produce a binary that re-execs itself, which is a fork
// bomb wearing a launch command.
func TestPolicy_RefusesASelfLoop(t *testing.T) {
	self := executable(t, "forgectl")
	dir := filepath.Dir(self)

	symlink := filepath.Join(dir, "claude-link")
	if err := os.Symlink(self, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	hardlink := filepath.Join(dir, "claude-hard")
	if err := os.Link(self, hardlink); err != nil {
		t.Fatalf("link: %v", err)
	}

	loops := map[string]string{
		"the same path":     self,
		"a symlink to it":   symlink,
		"a hard link to it": hardlink,
		// Built by concatenation, not filepath.Join, which would clean the
		// "/./" away and make this row a duplicate of the first.
		"an uncleaned spell": dir + "/./forgectl",
	}
	for name, path := range loops {
		t.Run(name, func(t *testing.T) {
			err := surface.Policy{}.AcceptBinary(
				launch.ResolvedBinary{Path: path, Source: launch.BinaryClaudeEnv}, self)
			if !errors.Is(err, surface.ErrBinarySelfLoop) {
				t.Errorf("AcceptBinary(%s) err = %v, want ErrBinarySelfLoop", name, err)
			}
		})
	}

	// The control: a different file at the same provenance is accepted, so the
	// refusals above are the identity check firing rather than the policy
	// refusing everything once a self path is supplied.
	other := executable(t, "claude")
	if err := (surface.Policy{}).AcceptBinary(
		launch.ResolvedBinary{Path: other, Source: launch.BinaryClaudeEnv}, self); err != nil {
		t.Errorf("a distinct binary was refused as a self-loop: %v", err)
	}
}

// TestPolicy_RefusalsAreTerminalSafe keeps a hostile path out of the operator's
// terminal unquoted. A refusal has to name the path — that is the actionable
// part — so the containment is quoting rather than omission.
func TestPolicy_RefusalsAreTerminalSafe(t *testing.T) {
	hostile := filepath.Join(t.TempDir(), "clau\x1b[31mde")

	err := surface.Policy{}.AcceptBinary(
		launch.ResolvedBinary{Path: hostile, Source: launch.BinaryClaudeEnv}, executable(t, "forgectl"))
	if err == nil {
		t.Fatal("an absent binary was accepted")
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the refusal writes a raw escape to the terminal: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "clau") {
		t.Errorf("the refusal does not name the path at all, which makes it unactionable: %v", err)
	}
}

// TestSelfPath_ResolvesTheRunningBinary proves the helper the service uses for
// the self-loop check returns something the check can actually stat — an empty
// or unusable value would silently disable the check.
func TestSelfPath_ResolvesTheRunningBinary(t *testing.T) {
	self, err := surface.SelfPath()
	if err != nil {
		t.Fatalf("SelfPath: %v", err)
	}
	if self == "" || !filepath.IsAbs(self) {
		t.Fatalf("SelfPath() = %q, want an absolute path", self)
	}
	if _, err := os.Stat(self); err != nil {
		t.Errorf("SelfPath() = %q, which does not stat: %v", self, err)
	}

	// And it is the running test binary, so pointing the policy at it trips
	// the self-loop check — which is the property the service depends on.
	err = surface.Policy{}.AcceptBinary(
		launch.ResolvedBinary{Path: self, Source: launch.BinaryClaudeEnv}, self)
	if !errors.Is(err, surface.ErrBinarySelfLoop) {
		t.Errorf("SelfPath() does not trip the self-loop check: %v", err)
	}
}
