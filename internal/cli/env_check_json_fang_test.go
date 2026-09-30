// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// runEnvCheckThroughFang runs `env <args>` under a root through fang.Execute
// with the production fangOptions, so termsafeErrorHandler renders whatever
// error comes back. cmd.ExecuteContext skips that sink, which is how #858
// hid: the in-package drift test saw an empty stderr while the real binary
// printed fang's error frame on top of the JSON verdict.
func runEnvCheckThroughFang(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	client, _ := envFixture()
	root := &cobra.Command{Use: "forgectl"}
	root.AddCommand(newEnvTestCmd(client, theme.Theme{}))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	argv := append([]string{"env"}, args...)
	root.SetArgs(argv)
	// A flag-parse failure leaves --json unparsed, so env check reads the
	// raw process arguments through the docsOSArgs seam; point it at argv.
	prev := docsOSArgs
	docsOSArgs = func() []string { return argv }
	t.Cleanup(func() { docsOSArgs = prev })
	err = fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", theme.Default())...)
	return out.String(), errOut.String(), err
}

// driftRepo makes a repo whose .env and .env.example differ.
func driftRepo(t *testing.T) {
	t.Helper()
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	for name, body := range map[string]string{
		".env":         "A=1\nLOCAL_ONLY=2\n",
		".env.example": "A=\nB=\n",
		"notes.txt":    "A=1\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	t.Chdir(repo)
}

// TestEnvCheckJSON_Drift_StderrEmptyThroughFang pins #858's drift half: under
// --json the verdict is on stdout, exit is 1, and stderr carries nothing — not
// fang's "ERROR … 1 missing, 1 extra key(s)" frame.
//
// Mutation that turns it red: return the human WithExitCode drift error
// instead of newSilentCodedError(1) under --json in newEnvCheckCmd.
func TestEnvCheckJSON_Drift_StderrEmptyThroughFang(t *testing.T) {
	driftRepo(t)
	stdout, stderr, err := runEnvCheckThroughFang(t, "check", "--json")
	if code := ExitCode(err); err == nil || code != 1 {
		t.Fatalf("ExitCode = %d (err %v), want 1 for drift", code, err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty under --json drift", stderr)
	}
	var got checkJSON
	if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil {
		t.Fatalf("stdout = %q, not valid JSON: %v", stdout, jsonErr)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "B" {
		t.Errorf("Missing = %#v, want [\"B\"]", got.Missing)
	}
}

// TestEnvCheckJSON_RefusedName_OneStderrObjectThroughFang pins #858's refusal
// half for both flags, a path outside the repository, and the usage errors
// cobra raises before RunE (an unknown flag, a stray argument, --example with
// no value): stdout stays empty, stderr carries exactly one check_failed
// object and no fang frame (no ESC byte, no "ERROR" header), path names the
// refused file when one resolved, and the exit code is still 1.
//
// Mutations that turn it red: make checkJSONFailure return err unchanged
// (every row); drop cmd.SetFlagErrorFunc (the flag rows); restore
// Args: cobra.NoArgs (the stray-argument row); drop the rel in
// resolveEnvTarget's refuse (the path of the name rows).
func TestEnvCheckJSON_RefusedName_OneStderrObjectThroughFang(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		want     string
		wantPath string
	}{
		{name: "file not an env name", args: []string{"--file", "notes.txt"}, want: `refusing "notes.txt": not an env file`, wantPath: "notes.txt"},
		{name: "example not an env name", args: []string{"--example", "notes.txt"}, want: `refusing "notes.txt": not an env file`, wantPath: "notes.txt"},
		{name: "file outside the repository", args: []string{"--file", "../outside.env"}, want: `refusing "../outside.env": outside the repository`},
		{name: "unknown flag", args: []string{"--bogus"}, want: "unknown flag: --bogus"},
		{name: "stray positional argument", args: []string{"extra"}, want: `unknown command "extra"`},
		{name: "example with no value", args: []string{"--example"}, want: "flag needs an argument: --example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			driftRepo(t)
			stdout, stderr, err := runEnvCheckThroughFang(t, append([]string{"check", "--json"}, tt.args...)...)
			if code := ExitCode(err); err == nil || code != 1 {
				t.Fatalf("ExitCode = %d (err %v), want 1 for a refused name", code, err)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty for a refusal", stdout)
			}
			if strings.ContainsRune(stderr, 0x1b) || strings.Contains(stderr, "ERROR") {
				t.Errorf("stderr carries fang's human error frame: %q", stderr)
			}
			dec := json.NewDecoder(strings.NewReader(stderr))
			var got checkErrorJSONWire
			if decErr := dec.Decode(&got); decErr != nil {
				t.Fatalf("stderr = %q, not valid JSON: %v", stderr, decErr)
			}
			if dec.More() {
				t.Fatalf("stderr carried more than one JSON value: %q", stderr)
			}
			if got.Code != "check_failed" {
				t.Errorf("code = %q, want %q", got.Code, "check_failed")
			}
			if !strings.HasPrefix(got.Error, tt.want) {
				t.Errorf("error = %q, want prefix %q", got.Error, tt.want)
			}
			if got.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tt.wantPath)
			}
		})
	}
}

// TestEnvCheckJSON_NotFound_ExitTwoThroughFang pins the exit-2 pass-through
// at the fang level: the not-found object is the only thing on stderr and
// the process exit code is still 2 (checkJSONFailure must hand an existing
// silentCodedError back unchanged, not re-encode or re-code it).
//
// Mutation that turns it red: drop the silentCodedError early return in
// checkJSONFailure (a second, empty-message check_failed object lands on
// stderr after the not-found one).
func TestEnvCheckJSON_NotFound_ExitTwoThroughFang(t *testing.T) {
	driftRepo(t)
	if err := os.Remove(".env"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	stdout, stderr, err := runEnvCheckThroughFang(t, "check", "--json")
	if code := ExitCode(err); err == nil || code != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2 for a missing --file", code, err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stderr))
	var got checkErrorJSONWire
	if decErr := dec.Decode(&got); decErr != nil {
		t.Fatalf("stderr = %q, not valid JSON: %v", stderr, decErr)
	}
	if dec.More() {
		t.Fatalf("stderr carried more than one JSON value: %q", stderr)
	}
	if got.Code != "file_not_found" || got.Path != ".env" {
		t.Errorf("got %+v, want code file_not_found, path .env", got)
	}
}

// TestEnvCheckHuman_RefusedName_KeepsFangFrame is the control: without
// --json the refusal still renders through fang's human error frame, so the
// wrapper above is scoped to --json only.
//
// Mutation that turns it red: drop the `!asJSON` early return in
// checkJSONFailure.
func TestEnvCheckHuman_RefusedName_KeepsFangFrame(t *testing.T) {
	driftRepo(t)
	stdout, stderr, err := runEnvCheckThroughFang(t, "check", "--file", "notes.txt")
	if code := ExitCode(err); err == nil || code != 1 {
		t.Fatalf("ExitCode = %d (err %v), want 1", code, err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `"notes.txt": not an env file`) || strings.Contains(stderr, `"code"`) {
		t.Errorf("stderr = %q, want the human refusal and no JSON object", stderr)
	}
}
