// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/theme"
	updatepkg "github.com/cameronsjo/forgectl/internal/update"
)

// The --json stderr contract (forgectl#862, docs/json-contract.md), asserted
// through fang.Execute with the production fangOptions. cmd.Execute skips
// termsafeErrorHandler, which is where fang's human error frame is drawn, so
// only a fang-level run can see the frame leak.

// isolateJSONContractEnv points every directory a verb could read or write at
// a scratch tree, so a contract run never touches the operator's own state.
func isolateJSONContractEnv(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(k, filepath.Join(base, strings.ToLower(strings.TrimPrefix(k, "XDG_"))))
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", "")
	t.Chdir(base)
}

// failingRunner fails every subprocess with the given exit code, so a verb
// that shells out fails before it can emit anything.
func failingRunner(code int) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, ExitCode: code, Stderr: "refused"}
	}}
}

// productionJSONRoot is the real command tree, contract installed by newRoot.
func productionJSONRoot(runner exec.Runner) *cobra.Command {
	return newRoot(module.Deps{Runner: runner})
}

// wrapJSONRoot mounts a verb built over test fixtures under a root and
// installs the contract exactly as newRoot does, for verdict scenarios the
// production deps cannot reach without real state.
func wrapJSONRoot(verb *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "forgectl", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(verb)
	installJSONErrorContract(root)
	return root
}

// runJSONThroughFang executes argv under root through fang with the
// production options, with the raw-argv seam pointed at argv.
func runJSONThroughFang(t *testing.T, root *cobra.Command, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(argv)
	prev := jsonOSArgs
	jsonOSArgs = func() []string { return argv }
	t.Cleanup(func() { jsonOSArgs = prev })
	err = fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", theme.Default())...)
	return out.String(), errOut.String(), err
}

// jsonVerbPaths lists the argv path of every command in root's tree that
// declares a boolean --json flag.
func jsonVerbPaths(root *cobra.Command) [][]string {
	var paths [][]string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if f := c.Flags().Lookup("json"); f != nil && f.Value.Type() == "bool" {
			paths = append(paths, strings.Fields(c.CommandPath())[1:])
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return paths
}

// assertNoFangFrame fails when stderr carries fang's human error frame.
func assertNoFangFrame(t *testing.T, stderr string) {
	t.Helper()
	if strings.ContainsRune(stderr, 0x1b) || strings.Contains(stderr, "ERROR") {
		t.Errorf("stderr carries fang's human error frame: %q", stderr)
	}
}

// decodeOneStderrObject asserts stderr is exactly one JSON object and returns
// it.
func decodeOneStderrObject(t *testing.T, stderr string) map[string]any {
	t.Helper()
	assertNoFangFrame(t, stderr)
	dec := json.NewDecoder(strings.NewReader(stderr))
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stderr = %q, not one JSON object: %v", stderr, err)
	}
	if dec.More() {
		t.Fatalf("stderr carried more than one JSON value: %q", stderr)
	}
	if _, ok := got["error"].(string); !ok {
		t.Errorf("object %v has no string error field", got)
	}
	return got
}

// stderrJSONObject returns the first JSON object found at the start of a
// stderr line, or "". Human progress lines never open with "{", so any hit
// is a failure object written on top of a verdict.
func stderrJSONObject(stderr string) string {
	for i := 0; i < len(stderr); i++ {
		if stderr[i] != '{' || (i > 0 && stderr[i-1] != '\n') {
			continue
		}
		var obj map[string]any
		if json.NewDecoder(strings.NewReader(stderr[i:])).Decode(&obj) == nil {
			return stderr[i:]
		}
	}
	return ""
}

// TestJSONStderr_BadFlag_EveryVerb runs every --json verb in the production
// tree with an unknown flag, both after --json (pflag has parsed it) and before
// it (pflag stopped first, so only the raw-argv scan can tell). Each run must
// leave stdout empty, put exactly one JSON object on stderr with no fang frame,
// and exit with the same code as the human run of the same mistake.
//
// Mutations that turn it red: drop installJSONErrorContract from newRoot
// (every verb); make the installed flag-error handler read jsonFlagParsed in
// place of argvWantsJSON (the "before" rows); make jsonFailure encode an int
// code for every verb (the code assertions).
func TestJSONStderr_BadFlag_EveryVerb(t *testing.T) {
	isolateJSONContractEnv(t)
	paths := jsonVerbPaths(productionJSONRoot(&exec.FakeRunner{}))
	// A floor, so a walker that stopped finding verbs cannot pass vacuously.
	// The floor is the real count, so losing a verb (or a whole command
	// group) fails here; raise it when a --json verb lands.
	if len(paths) < 74 {
		t.Fatalf("found %d --json verbs, want at least 74: %v", len(paths), paths)
	}
	for _, path := range paths {
		name := strings.Join(path, " ")
		for _, order := range []struct {
			name string
			args []string
		}{
			{"after", []string{"--json", "--forgectl-bogus"}},
			{"before", []string{"--forgectl-bogus", "--json"}},
		} {
			t.Run(name+"/"+order.name, func(t *testing.T) {
				_, humanErrOut, humanErr := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), append(append([]string{}, path...), "--forgectl-bogus")...)
				if humanErr == nil {
					t.Fatalf("human run accepted an unknown flag")
				}
				if strings.Contains(humanErrOut, `"error"`) {
					t.Errorf("human stderr = %q, want fang's frame, not JSON", humanErrOut)
				}

				stdout, stderr, err := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), append(append([]string{}, path...), order.args...)...)
				if err == nil {
					t.Fatalf("--json run accepted an unknown flag")
				}
				if got, want := ExitCode(err), ExitCode(humanErr); got != want {
					t.Errorf("exit code = %d under --json, %d without: --json must not change it", got, want)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
				obj := decodeOneStderrObject(t, stderr)
				if msg, _ := obj["error"].(string); !strings.Contains(msg, "unknown flag: --forgectl-bogus") {
					t.Errorf("error = %q, want the unknown-flag message", msg)
				}
				switch {
				case path[0] == "docs":
					// The docs shape: integer code plus root (docs_errors.go).
					if obj["code"] != float64(2) {
						t.Errorf("docs code = %v, want 2", obj["code"])
					}
					if _, ok := obj["root"]; !ok {
						t.Errorf("docs object %v has no root key", obj)
					}
				case name == "env check":
					if obj["code"] != "check_failed" {
						t.Errorf("env check code = %v, want check_failed", obj["code"])
					}
				default:
					if obj["code"] != jsonCodeUsage {
						t.Errorf("code = %v, want %q", obj["code"], jsonCodeUsage)
					}
					if obj["path"] != "" {
						t.Errorf("path = %v, want \"\"", obj["path"])
					}
				}
			})
		}
	}
}

// TestJSONStderr_NoUnwrappedErrorSites pins the assumption
// installJSONErrorContract rests on: cobra's required-flag and flag-group
// checks, and the legacy unknown-subcommand check on a parent with a nil Args,
// all raise errors outside the three sites the contract wraps, and so do the
// run hooks on either side of RunE: PreRun, PostRun, PersistentPreRun and
// PersistentPostRun, in both their plain and E forms. No --json verb may use
// them until the contract covers them too.
//
// Mutations that turn it red: MarkFlagRequired on any --json verb's flag; a
// PreRunE or PostRunE on any --json verb, or a PersistentPreRunE or
// PersistentPostRunE on the root.
func TestJSONStderr_NoUnwrappedErrorSites(t *testing.T) {
	isolateJSONContractEnv(t)
	root := productionJSONRoot(&exec.FakeRunner{})
	groupAnnotations := []string{
		cobra.BashCompOneRequiredFlag,
		"cobra_annotation_required_if_others_set",
		"cobra_annotation_one_required",
		"cobra_annotation_mutually_exclusive",
	}
	for _, path := range jsonVerbPaths(root) {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("Find(%v): %v", path, err)
		}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			for _, a := range groupAnnotations {
				if _, ok := f.Annotations[a]; ok {
					t.Errorf("%s --%s carries %s, which raises an error the --json contract does not wrap", cmd.CommandPath(), f.Name, a)
				}
			}
		})
		for c := cmd; c != nil; c = c.Parent() {
			// A hook error leaves cobra outside the three wrapped sites.
			if c.PersistentPreRunE != nil || c.PersistentPreRun != nil {
				t.Errorf("%s: %s has a PersistentPreRun hook, whose error bypasses the --json contract", cmd.CommandPath(), c.CommandPath())
			}
			if c.PersistentPostRunE != nil || c.PersistentPostRun != nil {
				t.Errorf("%s: %s has a PersistentPostRun hook, whose error bypasses the --json contract", cmd.CommandPath(), c.CommandPath())
			}
		}
		if cmd.PreRunE != nil || cmd.PreRun != nil {
			t.Errorf("%s has a PreRun hook, whose error bypasses the --json contract", cmd.CommandPath())
		}
		if cmd.PostRunE != nil || cmd.PostRun != nil {
			t.Errorf("%s has a PostRun hook, whose error bypasses the --json contract", cmd.CommandPath())
		}
		if cmd.HasSubCommands() && cmd.Args == nil {
			t.Errorf("%s has subcommands and a nil Args: cobra's legacy unknown-subcommand error bypasses the --json contract", cmd.CommandPath())
		}
	}
}

// TestJSONStderr_FailedBeforeEmitting_OneObject covers (b) on the production
// tree: a verb that fails before it writes its verdict leaves stdout empty,
// writes exactly one {"error","code","path"} object, and keeps its exit code.
//
// Mutations that turn it red: make the installed RunE wrapper return err
// unchanged (the failed rows); make the installed Args wrapper return err
// unchanged (the usage rows); make jsonFailure return a plain
// newSilentCodedError(1) (the exit-code rows that are not 1).
func TestJSONStderr_FailedBeforeEmitting_OneObject(t *testing.T) {
	for _, tt := range []struct {
		name     string
		runner   exec.Runner
		args     []string
		wantCode string
		wantExit int
		wantMsg  string
	}{
		{name: "k8s ns: kubectl fails", runner: failingRunner(3), args: []string{"k8s", "ns", "--json"}, wantCode: jsonCodeFailed, wantExit: 3, wantMsg: "kubectl"},
		{name: "k8s ns: namespace argument", args: []string{"k8s", "ns", "staging", "--json"}, wantCode: jsonCodeFailed, wantExit: 1, wantMsg: "cannot be combined"},
		{name: "tasks show: not an id", args: []string{"tasks", "show", "abc", "--json"}, wantCode: jsonCodeFailed, wantExit: 1, wantMsg: "not a task id"},
		{name: "tasks show: no id", args: []string{"tasks", "show", "--json"}, wantCode: jsonCodeUsage, wantExit: 1, wantMsg: "missing <id>"},
		{name: "launch stats: bad window", args: []string{"launch", "stats", "soon", "--json"}, wantCode: jsonCodeFailed, wantExit: 1},
		{name: "launch which: stray argument", args: []string{"launch", "which", "extra", "--json"}, wantCode: jsonCodeUsage, wantExit: 1, wantMsg: "takes no arguments"},
		{name: "pr drain: once with watch", args: []string{"pr", "drain", "--once", "--watch", "--json"}, wantCode: jsonCodeFailed, wantExit: 1, wantMsg: "cannot be combined"},
		{name: "update check: unknown step", args: []string{"update", "check", "--only", "forgectl-bogus", "--json"}, wantCode: jsonCodeFailed, wantExit: 2, wantMsg: "forgectl-bogus"},
		{name: "herdr organize: no config", args: []string{"herdr", "organize", "--json"}, wantCode: jsonCodeFailed, wantExit: 2},
		{name: "status: non-positive timeout", args: []string{"status", "--json", "--timeout", "0s"}, wantCode: jsonCodeFailed, wantExit: 1, wantMsg: "--timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateJSONContractEnv(t)
			runner := tt.runner
			if runner == nil {
				runner = &exec.FakeRunner{}
			}
			stdout, stderr, err := runJSONThroughFang(t, productionJSONRoot(runner), tt.args...)
			if err == nil {
				t.Fatalf("exit 0; stdout=%q stderr=%q", stdout, stderr)
			}
			if got := ExitCode(err); got != tt.wantExit {
				t.Errorf("exit code = %d, want %d", got, tt.wantExit)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty: the verb failed before emitting", stdout)
			}
			obj := decodeOneStderrObject(t, stderr)
			if obj["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", obj["code"], tt.wantCode)
			}
			if _, ok := obj["path"].(string); !ok {
				t.Errorf("object %v has no string path", obj)
			}
			if msg, _ := obj["error"].(string); !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", msg, tt.wantMsg)
			}
		})
	}
}

// TestJSONStderr_ParsedFlagDecidesArgs pins issue comment nit 1: once flags
// have parsed, --json is the parsed flag, not a raw-argv match. In
// `--host --json extra1 extra2` the --json is --host's value, so the stray
// argument error is the human one.
//
// Mutation that turns it red: make the installed Args wrapper (or env check's
// Args) read argvWantsJSON in place of jsonFlagParsed.
func TestJSONStderr_ParsedFlagDecidesArgs(t *testing.T) {
	for _, args := range [][]string{
		{"projects", "list", "--host", "--json", "a", "b"},
		{"env", "check", "--example", "--json", "extra"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			isolateJSONContractEnv(t)
			_, stderr, err := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), args...)
			if err == nil {
				t.Fatal("stray arguments were accepted")
			}
			if strings.Contains(stderr, `"code"`) {
				t.Errorf("stderr = %q, want the human error: --json here is a flag value", stderr)
			}
		})
	}
}

// TestJSONStderr_VerdictEmitted_SilentExit covers (a): a verb that wrote its
// JSON verdict on stdout and exits non-zero writes nothing to stderr (beyond a
// verb's own documented progress lines) and keeps its exit code.
//
// Mutation that turns it red: make jsonVerdict return err unchanged (every
// row gains a failed-code object, or fang's frame, on stderr).
func TestJSONStderr_VerdictEmitted_SilentExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		// build returns the verb and argv; it may seed fixtures.
		build func(t *testing.T) (*cobra.Command, []string)
		// stderrFree is true when the verb writes nothing of its own to
		// stderr, so the whole stream must be empty.
		stderrFree bool
	}{
		{name: "doctor", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			d := allOKDeps(t)
			d.Runner = &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
				if name == "gh" {
					return "", &exec.CommandError{Name: "gh", Stderr: "not logged in"}
				}
				return "ok", nil
			}}
			return newDoctorCmdForDeps(d, theme.Theme{}), []string{"doctor", "--json"}
		}},
		{name: "launch stats: skipped rows", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			leaf := scratchUsageStore(t)
			pinStatsClock(t)
			seedUsageRow(t, "2026-08-13T10:00:00Z", "claude", "opus", "new", "default")
			store, err := os.OpenRoot(leaf)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			f, err := store.OpenFile("launch-usage.jsonl", os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("{not json}\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			return newLaunchStatsCmd(), []string{"stats", "--json"}
		}},
		{name: "preflight: misaligned", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			cfg := setupPreflightHome(t)
			t.Chdir(t.TempDir())
			return newPreflightCmdForConfig(cfg), []string{"preflight", "--json"}
		}},
		{name: "pr repair: unsettled", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			dir := t.TempDir()
			seedRepairRecord(t, dir, "o/r#1", "preparing", "")
			return newPrRepairCmd(repairCmdClient(t, dir)), []string{"repair", "--json"}
		}},
		{name: "projects pull: one failed", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			client := pullCmdFixture(t, []string{"ok", "broken"}, nil,
				map[string]string{"ok": "Already up to date."},
				map[string]error{"broken": errors.New("conflict")})
			cmd := newProjectsPullAllCmd(client)
			return cmd, []string{cmd.Name(), "--json"}
		}},
		{name: "pr drain: a launch failed", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			client, dir := drainCmdClient(t, prDrainRunner(map[int]error{1: errors.New("boom: agent refused")}))
			seedQueuedFixture(t, dir, pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}, time.Now().UTC())
			return newPrDrainCmd(client, config.Config{}), []string{"drain", "--json"}
		}},
		{name: "status: strict with a failed section", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			return newStatusCmdForSources(failingStatusSources(t), theme.Theme{}), []string{"status", "--json", "--strict", "--timeout", "50ms"}
		}},
		{name: "projects list: strict on a degraded host", build: func(t *testing.T) (*cobra.Command, []string) {
			return newProjectsListCmd(listFixture(t, degradedGitHubRunFunc)), []string{"list", "--json", "--strict"}
		}},
		{name: "docs check: error findings", stderrFree: true, build: func(t *testing.T) (*cobra.Command, []string) {
			dir := t.TempDir()
			docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[x](missing.md)\n")
			return newDocsCheckCmd(module.Deps{}), []string{"check", "--json", dir}
		}},
		{name: "update run: a step failed", build: func(t *testing.T) (*cobra.Command, []string) {
			client := updatepkg.New(&exec.FakeRunner{}, updatepkg.WithSteps([]updatepkg.Step{
				fakeUpdateStep("npm", true, errors.New("boom")),
			}))
			return newUpdateCmdForClient(client, config.UpdateConfig{LogDir: t.TempDir()}, theme.Theme{}), []string{"update", "run", "--yes", "--json"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateJSONContractEnv(t)
			verb, argv := tt.build(t)
			stdout, stderr, err := runJSONThroughFang(t, wrapJSONRoot(verb), argv...)
			if got := ExitCode(err); err == nil || got != 1 {
				t.Fatalf("exit code = %d (err %v), want 1; stderr=%q", got, err, stderr)
			}
			var verdict any
			if jerr := json.Unmarshal([]byte(stdout), &verdict); jerr != nil {
				t.Fatalf("stdout = %q, not one JSON verdict: %v", stdout, jerr)
			}
			assertNoFangFrame(t, stderr)
			if obj := stderrJSONObject(stderr); obj != "" {
				t.Errorf("stderr carries a JSON object on top of the stdout verdict: %s", obj)
			}
			if tt.stderrFree && stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// TestJSONStderr_HumanPathUnchanged is the control: without --json a
// verdict verb's failure still reaches fang's human renderer with its message.
//
// Mutation that turns it red: drop the !asJSON early return in jsonVerdict.
func TestJSONStderr_HumanPathUnchanged(t *testing.T) {
	isolateJSONContractEnv(t)
	dir := t.TempDir()
	seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	_, stderr, err := runJSONThroughFang(t, wrapJSONRoot(newPrRepairCmd(repairCmdClient(t, dir))), "repair")
	if ExitCode(err) != 1 {
		t.Fatalf("exit code = %d, want 1", ExitCode(err))
	}
	if !strings.Contains(stderr, "unsettled") || strings.Contains(stderr, `"code"`) {
		t.Errorf("stderr = %q, want the human unsettled message and no JSON", stderr)
	}
}

// TestJSONFailure_Unit pins the helper's own branches.
//
// Mutations that turn it red: drop the silentCodedError branch (the wrapped
// silent row encodes a second object); return err in place of silent there
// (the handler would render the wrapper); pass a fixed path (the path row).
func TestJSONFailure_Unit(t *testing.T) {
	newCmd := func() (*cobra.Command, *bytes.Buffer) {
		var buf bytes.Buffer
		c := &cobra.Command{Use: "x"}
		c.SetErr(&buf)
		return c, &buf
	}

	c, buf := newCmd()
	if got := jsonFailure(c, WithExitCode(errors.New("boom"), 4), false, "failed"); ExitCode(got) != 4 || buf.Len() != 0 {
		t.Errorf("human: got %v (exit %d), stderr %q; want it untouched", got, ExitCode(got), buf.String())
	}

	c, buf = newCmd()
	inner := newSilentCodedError(3)
	got := jsonFailure(c, errors.Join(errors.New("wrap"), inner), true, "failed")
	if got != inner || buf.Len() != 0 {
		t.Errorf("wrapped silent: got %#v, stderr %q; want the inner silentCodedError and nothing written", got, buf.String())
	}

	c, buf = newCmd()
	got = jsonFailure(c, WithExitCode(&envTargetError{err: errors.New("refused"), rel: "a/.env"}, 5), true, "failed")
	if ExitCode(got) != 5 {
		t.Errorf("exit = %d, want 5", ExitCode(got))
	}
	var obj jsonFailureObject
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatalf("stderr = %q: %v", buf.String(), err)
	}
	if obj != (jsonFailureObject{Error: "refused", Code: "failed", Path: "a/.env"}) {
		t.Errorf("object = %+v", obj)
	}

	if err := jsonVerdict(WithExitCode(errors.New("x"), 6), true); ExitCode(err) != 6 {
		t.Errorf("jsonVerdict exit = %d, want 6", ExitCode(err))
	} else if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("jsonVerdict = %T, want *silentCodedError", err)
	}
	if err := jsonVerdict(nil, true); err != nil {
		t.Errorf("jsonVerdict(nil) = %v", err)
	}
}

// TestJSONStderr_JSONAsAnotherFlagsValue pins the raw-argv scan's value skip:
// in `--host --json --forgectl-bogus` pflag reads --json as --host's value, so
// the unknown-flag error is the human one.
//
// Mutation that turns it red: drop the takesSeparateValue case in
// argvWantsJSON.
func TestJSONStderr_JSONAsAnotherFlagsValue(t *testing.T) {
	isolateJSONContractEnv(t)
	_, stderr, err := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), "projects", "list", "--host", "--json", "--forgectl-bogus")
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if strings.Contains(stderr, `"code"`) {
		t.Errorf("stderr = %q, want the human error: --json here is --host's value", stderr)
	}
}

// TestExitCode_TypedNilInChainDoesNotPanic pins the pre-existing crash: a
// chain holding a typed-nil *os.PathError panics inside errors.As, because
// (*os.PathError)(nil).Unwrap dereferences its receiver. ExitCode and
// jsonFailure must fall back to exit 1 instead.
//
// Mutation that turns it red: make chainAs call errors.As with no recover.
func TestExitCode_TypedNilInChainDoesNotPanic(t *testing.T) {
	err := fmt.Errorf("stat: %w", (*os.PathError)(nil))
	if got := ExitCode(err); got != 1 {
		t.Errorf("ExitCode = %d, want 1", got)
	}
	var buf bytes.Buffer
	c := &cobra.Command{Use: "x"}
	c.SetErr(&buf)
	got := jsonFailure(c, err, true, jsonCodeFailed)
	if ExitCode(got) != 1 {
		t.Errorf("jsonFailure exit = %d, want 1", ExitCode(got))
	}
	var obj jsonFailureObject
	if jerr := json.Unmarshal(buf.Bytes(), &obj); jerr != nil || obj.Code != jsonCodeFailed || obj.Path != "" {
		t.Errorf("stderr = %q (%v), want one failed object with an empty path", buf.String(), jerr)
	}
}
