package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// fakeMdrollScript records its argv one per line into $FAKE_MDROLL_ARGV, echoes
// one line of stdin to stdout and a marker to stderr (proving all three streams
// are passed through), then exits with $FAKE_MDROLL_EXIT. It uses shell
// builtins only, so it runs with PATH pointed at its own directory.
const fakeMdrollScript = `#!/bin/sh
: > "$FAKE_MDROLL_ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_MDROLL_ARGV"; done
IFS= read -r line
printf 'stdin:%s\n' "$line"
printf 'stderr-marker\n' >&2
exit "${FAKE_MDROLL_EXIT:-0}"
`

// docsReadFixture makes a fresh cwd holding a small doc set, points the
// default root set at it alone, and installs a fake mdroll as the only thing on
// PATH. It returns the cwd and the file the fake writes its argv to.
func docsReadFixture(t *testing.T) (cwd, argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake mdroll is a POSIX shell script")
	}
	cwd = t.TempDir()
	for _, dir := range []string{"notes", ".git"} {
		if err := os.MkdirAll(filepath.Join(cwd, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"README.md", "-dash.md", "notes/plan.md", ".git/hidden.md", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte("# "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(cwd)
	t.Setenv(cadenceFieldReportsEnv, "")

	bin := t.TempDir()
	//nolint:gosec // G306: the fake reader must be executable by its owner to be exec'd
	if err := os.WriteFile(filepath.Join(bin, "mdroll"), []byte(fakeMdrollScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	argvFile = filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_MDROLL_ARGV", argvFile)
	t.Setenv("FAKE_MDROLL_EXIT", "0")
	return cwd, argvFile
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake mdroll never ran (no argv recorded): %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

type docsReadResult struct {
	err            error
	stdout, stderr string
	fallbackDoc    *docspkg.Doc
}

// runDocsReadForTest runs the verb with the production PATH lookup and a
// recording fallback, so nothing binds a port or opens a browser.
func runDocsReadForTest(t *testing.T, target string) docsReadResult {
	t.Helper()
	var res docsReadResult
	var stdout, stderr bytes.Buffer
	cmd := testCmdWithContext(context.Background())
	cmd.SetIn(strings.NewReader("keys\n"))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	rt := docsReadRuntime{
		lookPath: osexec.LookPath,
		fallback: func(_ *cobra.Command, _ module.Deps, _ *docspkg.Index, doc docspkg.Doc) error {
			res.fallbackDoc = &doc
			return nil
		},
	}
	res.err = runDocsRead(cmd, module.Deps{}, target, 5*time.Second, rt)
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func TestDocsRead_LaunchesMdrollWithWatchAndPassesStdio(t *testing.T) {
	cwd, argvFile := docsReadFixture(t)

	res := runDocsReadForTest(t, "README.md")
	if res.err != nil {
		t.Fatalf("docs read: %v", res.err)
	}
	if res.fallbackDoc != nil {
		t.Fatal("docs read fell back to the HTML reader with mdroll on PATH")
	}
	want := []string{"--watch", "--", canonical(t, filepath.Join(cwd, "README.md"))}
	if got := readArgv(t, argvFile); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("mdroll argv = %q, want %q", got, want)
	}
	if !strings.Contains(res.stdout, "stdin:keys") {
		t.Errorf("stdout = %q, want mdroll to have read stdin and written stdout through", res.stdout)
	}
	if !strings.Contains(res.stderr, "stderr-marker") {
		t.Errorf("stderr = %q, want mdroll's stderr passed through", res.stderr)
	}
}

// A document named like a flag must reach mdroll as a path, never a flag: the
// "--" separator precedes it. Run through cobra, where the operator's own "--"
// is what gets the name past forgectl's parser in the first place.
func TestDocsRead_DashLeadingNameReachesMdrollAfterSeparator(t *testing.T) {
	cwd, argvFile := docsReadFixture(t)

	cmd := newDocsReadCmd(module.Deps{})
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--", "-dash.md"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("docs read -- -dash.md: %v", err)
	}
	got := readArgv(t, argvFile)
	path := canonical(t, filepath.Join(cwd, "-dash.md"))
	if len(got) < 2 || got[len(got)-1] != path || got[len(got)-2] != "--" {
		t.Errorf("mdroll argv = %q, want it to end with \"--\", %q", got, path)
	}
}

func TestDocsRead_RootRelativeName(t *testing.T) {
	cwd, argvFile := docsReadFixture(t)
	idx, err := docspkg.NewIndex([]string{cwd})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label

	res := runDocsReadForTest(t, label+"/notes/plan.md")
	if res.err != nil {
		t.Fatalf("docs read %s/notes/plan.md: %v", label, res.err)
	}
	got := readArgv(t, argvFile)
	if want := canonical(t, filepath.Join(cwd, "notes", "plan.md")); got[len(got)-1] != want {
		t.Errorf("mdroll path = %q, want %q", got[len(got)-1], want)
	}
}

func TestDocsRead_RefusesAnythingOutsideTheIndex(t *testing.T) {
	cwd, argvFile := docsReadFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second default root beside cwd, so a traversal out of it can land on
	// a file that IS indexed — under the other root. Only Index.Resolve's
	// per-root containment refuses that; index membership alone would not.
	reports := t.TempDir()
	if err := os.WriteFile(filepath.Join(reports, "r.md"), []byte("# r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cadenceFieldReportsEnv, reports)
	idx, err := docspkg.NewIndex([]string{cwd, reports})
	if err != nil {
		t.Fatal(err)
	}
	label, reportsLabel := idx.Roots()[0].Label, idx.Roots()[1].Label

	for _, target := range []string{
		reportsLabel + "/../" + filepath.Base(cwd) + "/README.md", // escapes its root onto an indexed file
		outside,          // a real file under no root
		".git/hidden.md", // in a root, but in an excluded dir
		"notes.txt",      // in a root, but not markdown
		label + "/../" + filepath.Base(filepath.Dir(outside)) + "/outside.md", // traversal out of a root
		"nosuchroot/README.md", // no such root label
		"missing.md",           // nothing at all
	} {
		t.Run(target, func(t *testing.T) {
			// A launch in an earlier case must not read as one in this case.
			if err := os.Remove(argvFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			res := runDocsReadForTest(t, target)
			if !errors.Is(res.err, errDocNotIndexed) {
				t.Errorf("docs read %q: err = %v, want errDocNotIndexed", target, res.err)
			}
			if res.fallbackDoc != nil {
				t.Errorf("docs read %q reached the HTML fallback", target)
			}
			if _, statErr := os.Stat(argvFile); statErr == nil {
				t.Errorf("docs read %q launched mdroll", target)
			}
		})
	}
}

func TestDocsRead_FallsBackToHTMLReaderWithoutMdroll(t *testing.T) {
	_, argvFile := docsReadFixture(t)
	t.Setenv("PATH", t.TempDir())

	res := runDocsReadForTest(t, "notes/plan.md")
	if res.err != nil {
		t.Fatalf("docs read: %v", res.err)
	}
	if res.fallbackDoc == nil || res.fallbackDoc.RelPath != "notes/plan.md" {
		t.Fatalf("fallback doc = %+v, want notes/plan.md", res.fallbackDoc)
	}
	if !strings.Contains(res.stderr, "mdroll not found on PATH") {
		t.Errorf("stderr = %q, want it to say why the HTML reader opened", res.stderr)
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("mdroll ran although it was not on PATH")
	}
}

func TestDocsRead_PropagatesMdrollExitCode(t *testing.T) {
	docsReadFixture(t)
	t.Setenv("FAKE_MDROLL_EXIT", "3")

	res := runDocsReadForTest(t, "README.md")
	if got := ExitCode(res.err); got != 3 {
		t.Errorf("ExitCode = %d (err %v), want mdroll's 3", got, res.err)
	}
}

// The production fallback serves the doc set and points the browser at the
// resolved document, not the index. The browser opener is a fake runner.
func TestServeDocsReadFallback_OpensTheDocument(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("HOME", cfgHome)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	idx := testDocsIndex(t)
	doc := idx.List()[0]

	opened := make(chan exec.Call, 1)
	deps := module.Deps{Runner: &interactiveCallRunner{FakeRunner: &exec.FakeRunner{}, called: opened}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := testCmdWithContext(ctx)

	done := make(chan error, 1)
	go func() { done <- serveDocsReadFallback(cmd, deps, idx, doc) }()

	var call exec.Call
	select {
	case call = <-opened:
		cancel()
	case err := <-done:
		t.Fatalf("fallback returned before opening the browser: %v", err)
	case <-time.After(shutdownWaitBudget):
		t.Fatal("fallback did not open the browser within the wait budget")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fallback shutdown: %v", err)
		}
	case <-time.After(shutdownWaitBudget):
		t.Fatal("fallback did not shut down within the wait budget")
	}

	wantSuffix := "/doc/" + doc.RootLabel + "/" + doc.RelPath
	if len(call.Args) != 1 || !strings.HasSuffix(call.Args[0], wantSuffix) {
		t.Errorf("browser opener args = %v, want one URL ending %q", call.Args, wantSuffix)
	}
}
