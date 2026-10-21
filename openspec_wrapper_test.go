package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenSpecWrapperRefusesOutsideSubcommands pins scripts/openspec.sh's
// opt-in: feedback (an issue filed upstream with the operator's gh login),
// completion, config, workset and context --code-workspace reach outside
// the repo and refuse with exit 2 unless OPENSPEC_ALLOW_OUTSIDE=1. The
// wrapper runs from a temp copy whose pinned CLI is a stub, so nothing is
// installed or fetched.
//
// Mutation that turns it red: delete the subcommand case block from the
// wrapper.
func TestOpenSpecWrapperRefusesOutsideSubcommands(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	src, err := os.ReadFile("scripts/openspec.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pin := filepath.Join(root, "scripts", "openspec")
	stubDir := filepath.Join(pin, "node_modules", ".bin")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "scripts", "openspec.sh")
	for path, body := range map[string]string{
		wrapper:                                 string(src),
		filepath.Join(stubDir, "openspec"):      "#!/bin/sh\necho ran \"$@\"\n",
		filepath.Join(pin, "package-lock.json"): "{}\n",
		filepath.Join(pin, "node_modules", ".package-lock.json"): "{}\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // G306: test-owned temp files; the stub must be executable
			t.Fatal(err)
		}
	}
	// The install is newer than the lockfile, so the wrapper skips npm ci.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(pin, "package-lock.json"), old, old); err != nil {
		t.Fatal(err)
	}

	run := func(allow bool, args ...string) (string, int) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), bash, append([]string{wrapper}, args...)...) //nolint:gosec // G204: the wrapper is a temp copy this test wrote
		cmd.Env = append(os.Environ(), "OPENSPEC_ALLOW_OUTSIDE=")
		if allow {
			cmd.Env = append(cmd.Env, "OPENSPEC_ALLOW_OUTSIDE=1")
		}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return string(out), 0
	}

	for _, args := range [][]string{
		{"feedback", "hi"},
		{"--no-color", "feedback", "hi"},
		{"completion", "install"},
		{"config", "edit"},
		{"workset"},
		{"context", "--code-workspace", "x.code-workspace", "--force"},
		{"context", "--code-workspace=x.code-workspace"},
	} {
		if out, code := run(false, args...); code != 2 || strings.Contains(out, "ran") {
			t.Errorf("%v without opt-in: exit %d, output %q; want exit 2 and the CLI not run", args, code, out)
		}
		if out, code := run(true, args...); code != 0 || !strings.Contains(out, "ran") {
			t.Errorf("%v with OPENSPEC_ALLOW_OUTSIDE=1: exit %d, output %q; want the CLI run", args, code, out)
		}
	}
	for _, args := range [][]string{
		{"validate", "--all", "--strict", "--no-interactive"},
		{"context"},
		{"list", "--specs"},
	} {
		if out, code := run(false, args...); code != 0 || !strings.Contains(out, "ran") {
			t.Errorf("%v: exit %d, output %q; want the CLI run", args, code, out)
		}
	}
}
