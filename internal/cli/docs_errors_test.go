package cli

// Test plan for docs_errors.go (forgectl#604: one "could not run" contract)
//
// Every docs leaf (serve, open, read, list, check, search):
//   [x] a bad flag exits 2
//   [x] a positional-argument count error exits 2
//   [x] a failure before the work starts (bad config; no running reader for
//       open) exits 2
//   [x] on a leaf that declares --json, each of those leaves stdout empty and
//       writes exactly one {"error","code","root"} object to stderr, root
//       always present
//   [x] --json AFTER the bad flag is still honored (flag parse never reached it)
//   [x] a "--" terminator ends the --json scan
// Carve-outs:
//   [x] docs search with a partially searched root keeps exit 1 and carries root
//   [x] docs read passes mdroll's exit status through
//   [x] docs serve bind failure and unusable --token-file exit 2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// docsBadConfig is a [docs] section Validate rejects: every verb that builds an
// index fails on it before walking anything.
func docsBadConfig() module.Deps {
	return module.Deps{Cfg: config.Config{Docs: config.DocsConfig{
		RootKinds: map[string]string{"x": "not-a-kind"},
	}}}
}

type docsLeaf struct {
	name     string
	build    func(module.Deps) *cobra.Command
	hasJSON  bool
	badCount []string // positional args that violate the leaf's Args; nil when it takes any
	preWork  []string // args that reach the pre-work failure (with docsBadConfig or an empty XDG dir)
}

func docsLeaves(t *testing.T) []docsLeaf {
	t.Helper()
	dir := t.TempDir()
	return []docsLeaf{
		{"docs serve", newDocsServeCmd, false, nil, []string{dir}},
		{"docs open", newDocsOpenCmd, false, []string{"a", "b"}, nil},
		{"docs read", newDocsReadCmd, false, nil, []string{"x.md"}},
		{"docs list", newDocsListCmd, true, nil, []string{dir}},
		{"docs check", newDocsCheckCmd, true, nil, []string{dir}},
		{"docs search", newDocsSearchCmd, true, []string{}, []string{"needle"}},
	}
}

func execDocsLeaf(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// assertCouldNotRun holds a leaf to the whole contract for one failure.
func assertCouldNotRun(t *testing.T, l docsLeaf, asJSON bool, stdout, stderr string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a failure, got nil", l.name)
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("%s: exit code = %d (err %v), want 2", l.name, got, err)
	}
	if !asJSON {
		return
	}
	if stdout != "" {
		t.Errorf("%s: stdout = %q, want empty", l.name, stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stderr))
	var obj map[string]any
	if decErr := dec.Decode(&obj); decErr != nil {
		t.Fatalf("%s: stderr is not a JSON object: %v\nstderr: %q", l.name, decErr, stderr)
	}
	if dec.More() {
		t.Errorf("%s: stderr carries more than one JSON value: %q", l.name, stderr)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"code", "error", "root"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("%s: error object keys = %v, want %v (root is always present)", l.name, keys, want)
	}
	if obj["code"] != float64(2) {
		t.Errorf("%s: code = %v, want 2", l.name, obj["code"])
	}
	if msg, _ := obj["error"].(string); msg == "" {
		t.Errorf("%s: error message is empty", l.name)
	}
}

func setDocsOSArgs(t *testing.T, args ...string) {
	t.Helper()
	prev := jsonOSArgs
	jsonOSArgs = func() []string { return args }
	t.Cleanup(func() { jsonOSArgs = prev })
}

// Mutation: make docsFlagError return the bare err (or drop a leaf's
// SetFlagErrorFunc) and the exit code falls to 1 (usage error) or, under
// --json, no object is written.
func TestDocsLeaves_BadFlag_CouldNotRun(t *testing.T) {
	for _, l := range docsLeaves(t) {
		for _, asJSON := range []bool{false, true} {
			if asJSON && !l.hasJSON {
				continue
			}
			t.Run(l.name+jsonSuffix(asJSON), func(t *testing.T) {
				args := []string{"--bogus"}
				if asJSON {
					args = []string{"--json", "--bogus"}
				}
				stdout, stderr, err := execDocsLeaf(t, l.build(docsBadConfig()), args...)
				assertCouldNotRun(t, l, asJSON, stdout, stderr, err)
			})
		}
	}
}

// pflag stops at the bad flag, so a --json written after it is never parsed
// and the flag value is still false. Mutation: delete the jsonOSArgs scan in
// argvWantsJSON and this leaves stderr empty under --json.
func TestDocsLeaves_BadFlagBeforeJSON_StillJSON(t *testing.T) {
	setDocsOSArgs(t, "docs", "x", "--bogus", "--json")
	for _, l := range docsLeaves(t) {
		if !l.hasJSON {
			continue
		}
		t.Run(l.name, func(t *testing.T) {
			stdout, stderr, err := execDocsLeaf(t, l.build(docsBadConfig()), "--bogus", "--json")
			assertCouldNotRun(t, l, true, stdout, stderr, err)
		})
	}
}

// A "--" ends option parsing, so a later --json is an operand; --json=<v> is
// parsed with strconv.ParseBool and the last occurrence wins. Mutations: drop
// the "--" case and the terminator row goes red; treat any --json= prefix as
// true and the =false/=0 rows go red; make the first occurrence win and the
// double-flag rows go red.
func TestArgvWantsJSON_ParsesArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare", []string{"list", "--json"}, true},
		{"absent", []string{"list"}, false},
		{"terminator", []string{"list", "--", "--json"}, false},
		{"=true", []string{"--json=true"}, true},
		{"=1", []string{"--json=1"}, true},
		{"=false", []string{"--json=false"}, false},
		{"=0", []string{"--json=0"}, false},
		{"garbage ignored", []string{"--json=maybe"}, false},
		{"double bare", []string{"--json", "--json"}, true},
		{"true then false", []string{"--json", "--json=false"}, false},
		{"false then true", []string{"--json=false", "--json"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setDocsOSArgs(t, c.args...)
			if got := argvWantsJSON(newDocsListCmd(module.Deps{})); got != c.want {
				t.Errorf("argvWantsJSON(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
	setDocsOSArgs(t, "serve", "--json")
	if argvWantsJSON(newDocsServeCmd(module.Deps{})) {
		t.Error("argvWantsJSON = true on serve, which declares no --json")
	}
}

// A token file's contents must never reach the error output, whichever way it
// is refused. Mutation: include the file contents in the error (for example
// `%s` of raw in readDocsTokenFile's failure) and the leak assertion goes red.
func TestDocsServe_TokenFileContentsNeverInErrorOutput(t *testing.T) {
	const secret = "s3cr3t-token-value"
	dir := t.TempDir()
	cases := map[string]struct {
		body string
		mode os.FileMode
	}{
		"bad grammar":    {secret + " with spaces\n", 0o600},
		"too permissive": {secret + "\n", 0o644},
		"two lines":      {secret + "\n" + secret + "\n", 0o600},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(c.body), c.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := execDocsLeaf(t, newDocsServeCmd(module.Deps{}), "--token-file", path, dir)
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("err = %v, exit %d, want a refusal with exit 2", err, ExitCode(err))
			}
			for label, got := range map[string]string{"stdout": stdout, "stderr": stderr, "error": err.Error()} {
				if strings.Contains(got, secret) {
					t.Errorf("%s leaks the token file contents: %q", label, got)
				}
			}
		})
	}
}

// Mutation: revert one leaf's Args to the bare cobra validator and its count
// error exits 1 with no JSON.
func TestDocsLeaves_ArgCountError_CouldNotRun(t *testing.T) {
	cases := map[string][]string{
		"docs open":   {"a", "b"},
		"docs read":   {},
		"docs search": {},
	}
	for _, l := range docsLeaves(t) {
		args, ok := cases[l.name]
		if !ok {
			continue
		}
		for _, asJSON := range []bool{false, true} {
			if asJSON && !l.hasJSON {
				continue
			}
			t.Run(l.name+jsonSuffix(asJSON), func(t *testing.T) {
				if asJSON {
					args = append([]string{"--json"}, args...)
				}
				stdout, stderr, err := execDocsLeaf(t, l.build(docsBadConfig()), args...)
				assertCouldNotRun(t, l, asJSON, stdout, stderr, err)
			})
		}
	}
}

// Mutation: change docsFail's code (or a call site's) from 2 to 1, or add
// `omitempty` to docsErrorJSON.Root, and this goes red for the affected leaf.
func TestDocsLeaves_PreWorkFailure_CouldNotRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no reader is running for docs open
	t.Setenv("HOME", t.TempDir())
	docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	for _, l := range docsLeaves(t) {
		for _, asJSON := range []bool{false, true} {
			if asJSON && !l.hasJSON {
				continue
			}
			t.Run(l.name+jsonSuffix(asJSON), func(t *testing.T) {
				deps := docsBadConfig()
				deps.Runner = &searchRunner{FakeRunner: &forgexec.FakeRunner{}}
				args := append([]string(nil), l.preWork...)
				if asJSON {
					args = append([]string{"--json"}, args...)
				}
				stdout, stderr, err := execDocsLeaf(t, l.build(deps), args...)
				assertCouldNotRun(t, l, asJSON, stdout, stderr, err)
			})
		}
	}
}

// Mutation: restore docsSearchExitCode's "anything else is 1" for the index
// walk and the missing-root case exits 1.
func TestDocsSearch_MissingRootAndBadQuery_Exit2(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}
	cmd := newDocsSearchCmd(module.Deps{Runner: runner})
	_, _, err := execDocsLeaf(t, cmd, "needle", "--limit", "0")
	if got := ExitCode(err); got != 2 {
		t.Errorf("--limit 0: exit code = %d, want 2", got)
	}
	// A runner that cannot stream is a host that cannot search: could not run.
	cmd = newDocsSearchCmd(module.Deps{Runner: &forgexec.FakeRunner{}})
	stdout, stderr, err := execDocsLeaf(t, cmd, "--json", "needle")
	assertCouldNotRun(t, docsLeaf{name: "docs search"}, true, stdout, stderr, err)
}

// The partial-result carve-out stays exit 1, and its stderr object gains root
// (additive). Mutation: route the partial case through docsFail(…, 2, …) and
// the exit code assertion goes red; drop Root from the object and the keys do.
func TestDocsSearch_PartialResult_StaysExit1WithRoot(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	runner := &searchRunner{
		FakeRunner: &forgexec.FakeRunner{},
		stdout:     rgMatchLine(t, page, "needle here\n"),
		err:        &forgexec.CommandError{Name: "rg", ExitCode: 2},
	}
	_, stderr, err := runDocsSearch(t, runner, "--json", "needle")
	if got := ExitCode(err); got != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", got, err)
	}
	var obj map[string]any
	if decErr := json.Unmarshal([]byte(stderr), &obj); decErr != nil {
		t.Fatalf("stderr is not one JSON object: %v\n%q", decErr, stderr)
	}
	if _, ok := obj["root"]; !ok || obj["code"] != float64(1) {
		t.Errorf("partial error object = %v, want code 1 and a root key", obj)
	}
}

// docs read's mdroll exit status is not ours to reclassify. Mutation: wrap the
// silentCodedError in docsFail unconditionally and 7 becomes 2.
func TestDocsRead_MdrollExitStatusPassesThrough(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "a.md"), "# A\n")
	t.Chdir(dir)
	t.Setenv(cadenceFieldReportsEnv, "")
	script := filepath.Join(dir, "bin", "mdroll")
	docsCheckWrite(t, script, "#!/bin/sh\nexit 7\n")
	if err := chmodExec(script); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(script))
	_, _, err := execDocsLeaf(t, newDocsReadCmd(module.Deps{}), "a.md")
	if got := ExitCode(err); got != 7 {
		t.Errorf("exit code = %d (err %v), want mdroll's 7", got, err)
	}
}

// Mutation: drop WithExitCode(…, 2) at the bind site in runDocsServeOpening
// and the bind failure exits 1.
func TestDocsServe_BindFailure_Exit2(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup
	_, _, runErr := execDocsLeaf(t, newDocsServeCmd(module.Deps{}), "--addr", ln.Addr().String(), t.TempDir())
	if runErr == nil || ExitCode(runErr) != 2 {
		t.Errorf("bind failure: err = %v, exit %d, want exit 2", runErr, ExitCode(runErr))
	}
}

// Mutation: drop WithExitCode(…, 2) at the resolveDocsToken site and an
// unusable --token-file exits 1.
func TestDocsServe_BadTokenFile_Exit2(t *testing.T) {
	_, _, err := execDocsLeaf(t, newDocsServeCmd(module.Deps{}), "--token-file", "relative-token", t.TempDir())
	if err == nil || ExitCode(err) != 2 {
		t.Errorf("relative --token-file: err = %v, exit %d, want exit 2", err, ExitCode(err))
	}
	var coded *codedError
	if !errors.As(err, &coded) {
		t.Errorf("err %v is not a coded error", err)
	}
}

func jsonSuffix(asJSON bool) string {
	if asJSON {
		return "/json"
	}
	return "/human"
}

func chmodExec(p string) error {
	return os.Chmod(p, 0o700) //nolint:gosec // G302: the stub mdroll must be executable
}
