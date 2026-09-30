// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// The --json contract for failures Execute raises before fang starts
// (forgectl#873 items 1, 4-7), plus the pr drain --watch verdict site (item 2).

// writeMalformedUserConfig writes a config.toml that does not parse where
// Execute will look for it, under a scratch $HOME.
func writeMalformedUserConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	cfgDir := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("this = = broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// executeCapturingStderr runs Execute with argv and returns its error and
// everything it wrote to os.Stderr.
func executeCapturingStderr(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	withArgs(t, argv...)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = w
	execErr := Execute(context.Background())
	os.Stderr = prevStderr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), execErr
}

// TestExecute_MalformedConfigUnderJSON is item 1 for the config-parse gate: a
// malformed config.toml refuses the verb before fang starts, and under --json
// that refusal is the verb family's one stderr object instead of a plain
// `forgectl: …` line. Exit 2 either way.
//
// Mutations that turn it red: make preFangFailure skip the preFangJSONTarget
// check (every JSON row); make preFangJSONTarget return cmd without the
// argvWantsJSONIn check (the --host row); resolve the gate's verb on the tree
// built over the bad config, where projects is a flagless stub (the generic
// row); make jsonFamilyFailure skip the docs case (the docs row).
func TestExecute_MalformedConfigUnderJSON(t *testing.T) {
	for _, tt := range []struct {
		name     string
		argv     []string
		wantJSON bool
		check    func(t *testing.T, obj map[string]any)
	}{
		{name: "generic verb", argv: []string{"projects", "list", "--json"}, wantJSON: true, check: func(t *testing.T, obj map[string]any) {
			if obj["code"] != jsonCodeFailed || obj["path"] != "" {
				t.Errorf("object = %v, want code %q and path \"\"", obj, jsonCodeFailed)
			}
		}},
		{name: "docs verb keeps its integer code", argv: []string{"docs", "list", "--json"}, wantJSON: true, check: func(t *testing.T, obj map[string]any) {
			if obj["code"] != float64(2) {
				t.Errorf("code = %v, want 2", obj["code"])
			}
			if root, ok := obj["root"]; !ok || root != "" {
				t.Errorf("object = %v, want root \"\"", obj)
			}
		}},
		{name: "env check keeps check_failed", argv: []string{"env", "check", "--json"}, wantJSON: true, check: func(t *testing.T, obj map[string]any) {
			if obj["code"] != "check_failed" {
				t.Errorf("code = %v, want check_failed", obj["code"])
			}
		}},
		{name: "json as another flag's value", argv: []string{"projects", "list", "--host", "--json"}},
		{name: "a -- that is another flag's value", argv: []string{"projects", "list", "--host", "--", "--json"}, wantJSON: true, check: func(t *testing.T, obj map[string]any) {
			if obj["code"] != jsonCodeFailed {
				t.Errorf("object = %v, want code %q", obj, jsonCodeFailed)
			}
		}},
		{name: "no json", argv: []string{"projects", "list"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeMalformedUserConfig(t)
			stderr, err := executeCapturingStderr(t, tt.argv...)
			if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d (err %v), want 2", got, err)
			}
			if !tt.wantJSON {
				if !strings.HasPrefix(stderr, "forgectl: ") || strings.Contains(stderr, `"code"`) {
					t.Errorf("stderr = %q, want the plain forgectl: line", stderr)
				}
				return
			}
			obj := decodeOneStderrObject(t, stderr)
			if msg, _ := obj["error"].(string); !strings.Contains(msg, "config.toml") || strings.HasPrefix(msg, "forgectl: ") {
				t.Errorf("error = %q, want the gate's message without the line prefix", msg)
			}
			tt.check(t, obj)
		})
	}
}

// TestExecute_EnvFailureUnderJSON is item 1 for the env-snapshot failure,
// which happens before Execute builds the tree: under --json it is one
// object, and the tree is built only to find the verb.
//
// Mutation that turns it red: make the env-snapshot branch in Execute print
// its plain line and return err, as it did before.
func TestExecute_EnvFailureUnderJSON(t *testing.T) {
	isolateJSONContractEnv(t)
	stubEnvFailure(t)
	buildRoot = newRoot
	stderr, err := executeCapturingStderr(t, "projects", "list", "--json")
	if ExitCode(err) != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", ExitCode(err), err)
	}
	obj := decodeOneStderrObject(t, stderr)
	if obj["code"] != jsonCodeFailed || !strings.Contains(obj["error"].(string), "XDG_CONFIG_HOME") {
		t.Errorf("object = %v, want the environment error with code %q", obj, jsonCodeFailed)
	}
}

// TestPreFangJSONTarget pins which argv count as asking for --json before
// flags parse, on the production tree.
//
// Mutations that turn it red: drop the "--" break in preFangJSONTarget's scan
// (the terminator row then builds the tree for nothing); drop the
// argvWantsJSONIn check (the --host, --json=false and --no-icons rows); break
// at every "--" regardless of the token before it (the --host -- row); drop
// mayTakeNextValue's "=" exclusion (the --host=x row builds the tree).
func TestPreFangJSONTarget(t *testing.T) {
	isolateJSONContractEnv(t)
	for _, tt := range []struct {
		args  []string
		want  string
		built bool
	}{
		{args: []string{"docs", "list", "--json"}, want: "forgectl docs list", built: true},
		{args: []string{"projects", "list", "--json=false"}, built: true},
		{args: []string{"projects", "list", "--host", "--json"}, built: true},
		// Everything after launch is the harness's: launch declares no --json.
		{args: []string{"launch", "--json"}, built: true},
		{args: []string{"projects", "list", "--", "--json"}},
		{args: []string{"projects", "list"}},
		// A "--" that is --host's value is not the terminator (#886).
		{args: []string{"projects", "list", "--host", "--", "--json"}, want: "forgectl projects list", built: true},
		// A "--" after a flag that turns out to be a bool is the terminator:
		// the tree is built to find out, and the --json after it is not counted.
		{args: []string{"docs", "list", "--no-icons", "--", "--json"}, built: true},
		// An inline value leaves nothing for the "--" to be, so it is the
		// terminator and the tree is never built (#891).
		{args: []string{"projects", "list", "--host=x", "--", "--json"}},
	} {
		built := false
		root := func() *cobra.Command { built = true; return newRoot(module.Deps{Runner: &exec.FakeRunner{}}) }
		cmd := preFangJSONTarget(root, tt.args)
		got := ""
		if cmd != nil {
			got = cmd.CommandPath()
		}
		if got != tt.want {
			t.Errorf("preFangJSONTarget(%q) = %q, want %q", tt.args, got, tt.want)
		}
		if built != tt.built {
			t.Errorf("preFangJSONTarget(%q) built the tree = %v, want %v", tt.args, built, tt.built)
		}
	}
}

// TestArgvWantsJSON_BundledShorthands is item 5: argvWantsJSON must read a
// bundled shorthand group the way pflag does. Each row is checked against
// pflag itself, so the expectation is the parser's, not a hand-written one.
//
// Mutation that turns it red: make shorthandGroupTakesNext look only at the
// group's first letter (the -vH rows).
func TestArgvWantsJSON_BundledShorthands(t *testing.T) {
	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().BoolP("verbose", "v", false, "")
		c.Flags().StringP("host", "H", "", "")
		c.Flags().Bool("json", false, "")
		return c
	}
	for _, args := range [][]string{
		{"-vH", "--json"},
		{"-vH", "x", "--json"},
		{"-Hv", "--json"},
		{"-H", "--json"},
		{"-v", "--json"},
		{"-H=x", "--json"},
		{"-vH=x", "--json"},
		{"-vvH", "--json"},
	} {
		pc := newCmd()
		if err := pc.Flags().Parse(args); err != nil {
			t.Fatalf("pflag rejected %q: %v", args, err)
		}
		want, _ := pc.Flags().GetBool("json")
		if got := argvWantsJSONIn(newCmd(), args); got != want {
			t.Errorf("argvWantsJSONIn(%q) = %v, pflag parsed --json as %v", args, got, want)
		}
	}
}

// TestPreFangJSONTarget_ShorthandValueBeforeTerminator pins the shorthand arm
// of mayTakeNextValue (#891). No production verb declares a value-taking
// shorthand, so this runs on a synthetic tree: in `x -H -- --json`, pflag
// hands "--" to -H as its value and parses --json, so the pre-fang scan must
// not stop at that "--". The expectation is read from pflag itself.
//
// Mutation that turns it red: narrow mayTakeNextValue to "--"-prefixed tokens.
func TestPreFangJSONTarget_ShorthandValueBeforeTerminator(t *testing.T) {
	buildTree := func() *cobra.Command {
		root := &cobra.Command{Use: "root"}
		c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().StringP("host", "H", "", "")
		c.Flags().Bool("json", false, "")
		root.AddCommand(c)
		return root
	}
	args := []string{"x", "-H", "--", "--json"}
	pc := buildTree().Commands()[0]
	if err := pc.Flags().Parse(args[1:]); err != nil {
		t.Fatalf("pflag rejected %q: %v", args, err)
	}
	if want, _ := pc.Flags().GetBool("json"); !want {
		t.Fatalf("pflag did not parse --json in %q; the fixture no longer tests the value arm", args)
	}
	cmd := preFangJSONTarget(buildTree, args)
	if cmd == nil || cmd.CommandPath() != "root x" {
		t.Errorf("preFangJSONTarget(%q) = %v, want root x", args, cmd)
	}
}

// TestJSONFailure_BareTypedNil is item 7: a bare typed-nil error (not wrapped)
// has an Error method that panics. jsonFailure and docsFail must still write
// one object, with the stand-in text.
//
// Mutation that turns it red: make jsonErrorText call err.Error() with no
// recover.
func TestJSONFailure_BareTypedNil(t *testing.T) {
	var bare error = (*os.PathError)(nil)
	var buf bytes.Buffer
	c := &cobra.Command{Use: "x"}
	c.SetErr(&buf)
	got := jsonFailure(c, bare, true, jsonCodeFailed)
	if ExitCode(got) != 1 {
		t.Errorf("exit = %d, want 1", ExitCode(got))
	}
	var obj jsonFailureObject
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil || obj.Error != jsonErrTextUnavailable {
		t.Errorf("stderr = %q (%v), want one object with the stand-in text", buf.String(), err)
	}

	buf.Reset()
	_ = docsFail(c, "docs list", "", bare, 2, true)
	var dobj docsErrorJSON
	if err := json.Unmarshal(buf.Bytes(), &dobj); err != nil || dobj.Error != jsonErrTextUnavailable || dobj.Code != 2 {
		t.Errorf("docs stderr = %q (%v), want one docs object with the stand-in text", buf.String(), err)
	}
}

// TestChainAs_RecoveryLogsDebug is item 6: a panic chainAs recovers leaves a
// Debug line naming Go types, never the panic value.
//
// Mutation that turns it red: drop the slog.Debug call in chainAs.
func TestChainAs_RecoveryLogsDebug(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var coded *codedError
	if chainAs(fmt.Errorf("stat: %w", (*os.PathError)(nil)), &coded) {
		t.Fatal("chainAs found a codedError in a chain that has none")
	}
	out := logs.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "panic_type=") || !strings.Contains(out, "error_type=*fmt.wrapError") {
		t.Errorf("log = %q, want one Debug line naming the error and panic types", out)
	}
}

// failWriter refuses every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestPrintDocsSearch_EncodeFailuresKeepDocsShape is item 4. When stdout
// refuses the response, stderr gets the docs integer-code object, not
// nothing; when stderr refuses the partial-result object after the response
// is on stdout, the verb exits 1 silently rather than hand the generic
// contract an error it would render in its string-code shape.
//
// Mutations that turn it red: return err unchanged from the stdout failure
// (the first half); return the WithExitCode error without jsonVerdict from
// the stderr failure (the second half).
func TestPrintDocsSearch_EncodeFailuresKeepDocsShape(t *testing.T) {
	var stderr bytes.Buffer
	c := &cobra.Command{Use: "search"}
	c.SetOut(failWriter{})
	c.SetErr(&stderr)
	err := printDocsSearch(c, docspkg.SearchResponse{}, true)
	if ExitCode(err) != 1 {
		t.Errorf("stdout failure exit = %d, want 1", ExitCode(err))
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("stdout failure = %T, want *silentCodedError", err)
	}
	var obj docsErrorJSON
	if jerr := json.Unmarshal(stderr.Bytes(), &obj); jerr != nil || obj.Code != 1 || !strings.Contains(obj.Error, "write refused") {
		t.Errorf("stderr = %q (%v), want one docs object with code 1", stderr.String(), jerr)
	}

	var stdout bytes.Buffer
	c = &cobra.Command{Use: "search"}
	c.SetOut(&stdout)
	c.SetErr(failWriter{})
	err = printDocsSearch(c, docspkg.SearchResponse{Errors: []docspkg.SearchError{{Root: "r", Message: "m"}}}, true)
	if ExitCode(err) != 1 {
		t.Errorf("stderr failure exit = %d, want 1", ExitCode(err))
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("stderr failure = %T, want *silentCodedError: the response is already on stdout", err)
	}
	if stdout.Len() == 0 {
		t.Error("the response never reached stdout")
	}
}

// TestDocsList_StdoutEncodeFailureKeepsDocsShape is #886 item 1: when stdout
// refuses the list, stderr gets the docs integer-code object, the way docs
// search's does, not the generic contract's string-code shape. The exit code
// stays 1 with or without --json.
//
// Mutation that turns it red: return printDocsList's error unchanged from
// newDocsListCmd's RunE.
func TestDocsList_StdoutEncodeFailureKeepsDocsShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Page"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(failWriter{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", dir})
	err := cmd.ExecuteContext(context.Background())
	if ExitCode(err) != 1 {
		t.Errorf("exit = %d (err %v), want 1", ExitCode(err), err)
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("err = %T, want *silentCodedError", err)
	}
	var obj docsErrorJSON
	if jerr := json.Unmarshal(stderr.Bytes(), &obj); jerr != nil || obj.Code != 1 || !strings.Contains(obj.Error, "write refused") {
		t.Errorf("stderr = %q (%v), want one docs object with code 1", stderr.String(), jerr)
	}
}

// TestPrDrain_WatchJSONRefusalLimitIsVerdict is item 2: under --watch --json
// three refused passes write three reports on stdout, then exit 1 with no
// failure object on stderr, because the last report is the verdict.
//
// Mutation that turns it red: drop jsonVerdict from runDrainWatch's
// refusal-limit return.
func TestPrDrain_WatchJSONRefusalLimitIsVerdict(t *testing.T) {
	inner := prDrainRunner(nil)
	run := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "-V" {
			return "tmux 1.8", nil // below the generation floor: every pass refuses
		}
		return inner.RunFunc(name, args)
	}}
	client, _ := drainCmdClient(t, run)
	root := wrapJSONRoot(newPrDrainCmd(client, config.Config{}))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"drain", "--watch", "--interval", time.Millisecond.String(), "--json"})
	err := root.ExecuteContext(context.Background())

	if ExitCode(err) != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", ExitCode(err), err)
	}
	dec := json.NewDecoder(&out)
	passes := 0
	for dec.More() {
		var report pr.DrainReport
		if jerr := dec.Decode(&report); jerr != nil {
			t.Fatalf("stdout report %d did not decode: %v\n%s", passes+1, jerr, out.String())
		}
		passes++
		if report.Refusal == "" || report.Pass != passes {
			t.Errorf("report %d = pass %d, refusal %q; want a refused pass %d", passes, report.Pass, report.Refusal, passes)
		}
	}
	if passes != drainWatchRefusalLimit {
		t.Errorf("stdout carried %d reports, want %d", passes, drainWatchRefusalLimit)
	}
	if obj := stderrJSONObject(errOut.String()); obj != "" {
		t.Errorf("stderr carries a failure object on top of the verdict: %q", obj)
	}
}
