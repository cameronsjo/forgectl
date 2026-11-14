// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// fixture reads a recorded rex reply from testdata (ids redacted).
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: a fixture name from this package's tests
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Ids from the recorded `rex new` reply (testdata/new.json), and two more
// block ids in the same shape.
const (
	fxSession  = "session:01a1db33-daeb-7000-8000-000000000003"
	fxBlock    = "block:01a1db33-daec-7000-8000-000000000005"
	fxBlock2   = "block:01a1db34-3991-7000-8000-000000000007"
	fxSession2 = "session:01a1db34-39dc-7000-8000-000000000009"
	// fxBlockMS is the creation time fxBlock embeds: 0x01a1db33daec.
	fxBlockMS = 0x01a1db33daec
)

// fakeBlock is one block the fake holds.
type fakeBlock struct {
	id, label string
	// exit is nil while the program runs.
	exit *int
}

// fakeSession is one session the fake holds.
type fakeSession struct {
	id, label string
	blocks    []*fakeBlock
}

// fail is a scripted failure: what stderr says and the exit code.
type fail struct {
	stderr string
	exit   int
}

// fakeRex is a scripted Rex server and CLI. Its replies are the recorded ones
// (testdata) with the fake's own ids and labels put in, so the parsers read
// the shapes rex printed.
//
// It cannot read an opaque argument, so the values a call names are matched
// against candidates the test supplies (wantOwner for labels, the sessions
// and blocks it holds for ids) with exec.Arg.Equal.
type fakeRex struct {
	t        *testing.T
	whoami   string
	sessions []*fakeSession
	// wantOwner is the ownership name of the launch under test.
	wantOwner string
	// newReply overrides what `rex new` prints; newFail makes it fail and,
	// with newCreates, still create the session first.
	newReply   string
	newFail    *fail
	newCreates bool
	// fails maps a verb ("rename", "block-ls", "ls", "call", "close",
	// "kill") to a failure.
	fails map[string]fail
	// processReply overrides `block call process`.
	processReply string
	// dirMissing makes /bin/test -d exit 1.
	dirMissing bool
	// scriptDir is where the adapter writes launch scripts. When consume is
	// set, `rex new` deletes the script, as sh's first line does.
	scriptDir string
	consume   bool
	// seenScript is the script's content and modes at `rex new`.
	seenScript                string
	seenFileMode, seenDirMode os.FileMode
	// secrets are values that must never be an argument of any call.
	secrets []string

	verbs []string
	run   *exec.FakeSensitiveRunner
}

func newFake(t *testing.T) *fakeRex {
	f := &fakeRex{t: t, whoami: fixture(t, "whoami.json"), fails: map[string]fail{}, scriptDir: t.TempDir(), consume: true}
	f.run = &exec.FakeSensitiveRunner{RunFunc: f.handle}
	return f
}

func (f *fakeRex) adapter() *Adapter {
	f.t.Helper()
	a, err := New(f.run, "/Applications/Rex.app/rex", WithScriptDir(f.scriptDir))
	if err != nil {
		f.t.Fatal(err)
	}
	a.consumeTimeout = 50 * time.Millisecond
	a.consumePoll = time.Millisecond
	return a
}

var (
	argNew    = exec.MustFixed("new")
	argWhoami = exec.MustFixed("whoami")
	argLs     = exec.MustFixed("ls")
	argBlock  = exec.MustFixed("block")
	argKill   = exec.MustFixed("kill")
	argRename = exec.MustFixed("rename")
	argClose  = exec.MustFixed("close")
	argCall   = exec.MustFixed("call")
)

// verb names the call: "whoami", "test", "new", "ls", "kill", or "block-"
// and the block sub-verb ("block-ls", "block-rename", ...).
func verb(cmd exec.SensitiveCommand) string {
	if cmd.Kind == exec.KindRexPreflight {
		return "test"
	}
	if len(cmd.Args) < 3 {
		return "?"
	}
	v := cmd.Args[2]
	switch {
	case v.Equal(argWhoami):
		return "whoami"
	case v.Equal(argNew):
		return "new"
	case v.Equal(argLs):
		return "ls"
	case v.Equal(argKill):
		return "kill"
	case v.Equal(argBlock) && len(cmd.Args) > 3:
		switch s := cmd.Args[3]; {
		case s.Equal(argLs):
			return "block-ls"
		case s.Equal(argRename):
			return "block-rename"
		case s.Equal(argClose):
			return "block-close"
		case s.Equal(argCall):
			return "block-call"
		}
	}
	return "?"
}

func (f *fakeRex) handle(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	v := verb(cmd)
	f.verbs = append(f.verbs, v)
	for _, a := range cmd.Args {
		for _, s := range f.secrets {
			if a.Equal(exec.Opaque(s)) {
				f.t.Errorf("%s carried a sealed value in its argv", v)
			}
		}
	}
	if fl, ok := f.fails[v]; ok {
		return failed(cmd.Kind, fl)
	}
	switch v {
	case "whoami":
		return ok(f.whoami), nil
	case "test":
		if f.dirMissing {
			return failed(cmd.Kind, fail{exit: 1})
		}
		return ok(""), nil
	case "new":
		return f.create(cmd)
	case "ls":
		return ok(f.renderSessions()), nil
	case "block-ls":
		s := f.session(cmd.Args[5])
		if s == nil {
			return failed(cmd.Kind, fail{stderr: "Error: session target \"" + f.named(cmd.Args[5]) + "\" not found\n", exit: 3})
		}
		return ok(renderBlocks(f.t, s)), nil
	case "block-rename":
		s, b := f.block(cmd.Args[5], cmd.Args[6])
		if b == nil {
			return failed(cmd.Kind, fail{stderr: "Error: block target \"x\" not found\n", exit: 3})
		}
		_ = s
		if cmd.Args[7].Equal(exec.Opaque(ownerLabelKey + f.wantOwner)) {
			b.label = ownerLabelKey + f.wantOwner
		} else {
			b.label = "?unexpected"
		}
		return ok(""), nil
	case "block-call":
		s, b := f.block(cmd.Args[5], cmd.Args[7])
		if s == nil {
			return failed(cmd.Kind, fail{stderr: "Error: session target \"" + f.named(cmd.Args[5]) + "\" not found\n", exit: 3})
		}
		if b == nil {
			return failed(cmd.Kind, fail{stderr: "Error: block target \"" + f.named(cmd.Args[7]) + "\" not found\n", exit: 3})
		}
		if f.processReply != "" {
			return ok(f.processReply), nil
		}
		if b.exit == nil {
			return ok(fixture(f.t, "process_running.json")), nil
		}
		return ok(`{"last_exit":{"exit_code":` + itoa(*b.exit) + `,"error":"exit status"},"home":"/Users/operator"}`), nil
	case "block-close":
		s, b := f.block(cmd.Args[5], cmd.Args[6])
		if b == nil {
			return failed(cmd.Kind, fail{stderr: "Error: block target \"" + f.named(cmd.Args[6]) + "\" not found\n", exit: 3})
		}
		var keep []*fakeBlock
		for _, x := range s.blocks {
			if x != b {
				keep = append(keep, x)
			}
		}
		s.blocks = keep
		return ok(""), nil
	case "kill":
		s := f.session(cmd.Args[3])
		if s == nil {
			return failed(cmd.Kind, fail{stderr: "Error: session target \"x\" not found\n", exit: 3})
		}
		var keep []*fakeSession
		for _, x := range f.sessions {
			if x != s {
				keep = append(keep, x)
			}
		}
		f.sessions = keep
		return ok(""), nil
	}
	return exec.SensitiveResult{}, errors.New("unscripted rex call " + v)
}

// create is `rex new`: it records the launch script's modes and content,
// deletes it when consume is set (as sh's first line would), and holds the
// session the recorded reply names.
func (f *fakeRex) create(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	matches, _ := filepath.Glob(filepath.Join(f.scriptDir, "*", "launch.sh"))
	if len(matches) != 1 {
		f.t.Errorf("rex new ran with %d launch scripts under the script dir, want 1", len(matches))
	} else {
		body, err := os.ReadFile(matches[0]) //nolint:gosec // G304: a path under t.TempDir
		if err != nil {
			f.t.Fatal(err)
		}
		f.seenScript = string(body)
		if fi, err := os.Lstat(matches[0]); err == nil {
			f.seenFileMode = fi.Mode().Perm()
		}
		if di, err := os.Lstat(filepath.Dir(matches[0])); err == nil {
			f.seenDirMode = di.Mode().Perm()
		}
		// The command after "--" is /bin/sh and the script's own path.
		n := len(cmd.Args)
		if !cmd.Args[n-2].Equal(exec.MustFixed(shellPath)) || !cmd.Args[n-1].Equal(exec.Opaque(matches[0])) {
			f.t.Error("rex new does not run /bin/sh <launch script>")
		}
		if f.consume {
			_ = os.Remove(matches[0])
		}
	}
	label := "?unexpected"
	for _, a := range cmd.Args {
		if a.Equal(exec.Opaque(f.wantOwner)) {
			label = f.wantOwner
		}
	}
	if f.newFail == nil || f.newCreates {
		f.sessions = append(f.sessions, &fakeSession{id: fxSession, label: label,
			blocks: []*fakeBlock{{id: fxBlock, label: "shell"}}})
	}
	if f.newFail != nil {
		return failed(cmd.Kind, *f.newFail)
	}
	if f.newReply != "" {
		return ok(f.newReply), nil
	}
	return ok(fixture(f.t, "new.json")), nil
}

func (f *fakeRex) session(arg exec.Arg) *fakeSession {
	for _, s := range f.sessions {
		if arg.Equal(exec.Opaque(s.id)) {
			return s
		}
	}
	return nil
}

func (f *fakeRex) block(sessionArg, blockArg exec.Arg) (*fakeSession, *fakeBlock) {
	s := f.session(sessionArg)
	if s == nil {
		return nil, nil
	}
	for _, b := range s.blocks {
		if blockArg.Equal(exec.Opaque(b.id)) {
			return s, b
		}
	}
	return s, nil
}

// named recovers which known id an argument is, for the not-found text.
func (f *fakeRex) named(arg exec.Arg) string {
	for _, id := range []string{fxSession, fxSession2, fxBlock, fxBlock2} {
		if arg.Equal(exec.Opaque(id)) {
			return id
		}
	}
	return "unknown"
}

func (f *fakeRex) renderSessions() string {
	type row struct {
		SessionID string `json:"session_id"`
		Label     string `json:"label"`
	}
	rows := []row{}
	for _, s := range f.sessions {
		rows = append(rows, row{s.id, s.label})
	}
	b, err := json.MarshalIndent(map[string]any{"sessions": rows}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// renderBlocks prints a session's blocks in the recorded block ls shape: each
// row is the recorded row with its id and label replaced.
func renderBlocks(t *testing.T, s *fakeSession) string {
	var recorded struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "block_ls_one.json")), &recorded); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, b := range s.blocks {
		row := map[string]any{}
		for k, v := range recorded.Blocks[0] {
			row[k] = v
		}
		row["block_id"], row["label"] = b.id, b.label
		rows = append(rows, row)
	}
	out, err := json.MarshalIndent(map[string]any{"blocks": rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func ok(stdout string) exec.SensitiveResult {
	return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(stdout), exec.OutputComplete),
		Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete)}
}

func failed(kind exec.CommandKind, fl fail) (exec.SensitiveResult, error) {
	return exec.SensitiveResult{
		Stdout:   exec.BoundedOutputForTest(nil, exec.OutputComplete),
		Stderr:   exec.BoundedOutputForTest([]byte(fl.stderr), exec.OutputComplete),
		ExitCode: fl.exit,
	}, exec.SensitiveErrorForTest(kind, exec.OutcomeExit)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// testBootstrap is a bootstrap line in the shape the service builds; nonce
// is its secret part.
const (
	testNonce     = "n0nce-5f1c2e"
	testBootstrap = "'/usr/local/bin/forgectl' 'surface' '_exec' '--protocol' '1' '--socket' '/tmp/fc-run/s.sock' '--nonce' '" + testNonce + "'"
)

func testSpec(t *testing.T) backend.StartSpec {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque(testBootstrap))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := backend.NewStartSpec("/repo/.claude/worktrees/x", "fix login", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// start runs a clean Start and returns its reference.
func start(t *testing.T, f *fakeRex, a *Adapter) backend.Ref {
	t.Helper()
	spec := testSpec(t)
	f.wantOwner = spec.OwnershipName()
	res := a.Start(t.Context(), spec)
	ref, has := res.Ref()
	if !has || res.Failed() {
		c, _ := res.Cause()
		t.Fatalf("Start = %v (%v), want a clean RefKnown; verbs %v", res, c, f.verbs)
	}
	return ref
}

// scriptsLeft lists what remains under the script dir.
func scriptsLeft(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func joinVerbs(v []string) string { return strings.Join(v, " ") }
