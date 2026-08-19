package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const testSocket = "/tmp/fc-surface/sock"

// pinnedClient builds a socket-pinned client whose filesystem and environment
// seams are driven, so nothing here depends on a real tmux or a real socket.
func pinnedClient(t *testing.T, run *internalexec.FakeRunner) *Client {
	t.Helper()
	c, err := NewPinned(run, testSocket)
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	c.getenv = func(string) string { return "" }
	c.getuid = func() int { return 501 }
	// The socket is absent; its DIRECTORY exists. That distinction is the normal
	// state a surface adapter creates — a private run dir with no server in it
	// yet — and the two must be driven separately, because "no socket" and "no
	// directory to put one in" reach different verdicts: only the first permits
	// creating a server.
	c.lstat = func(path string) (os.FileInfo, error) {
		if path == filepath.Dir(testSocket) {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	return c
}

func TestNewPinnedValidatesTheSocketPath(t *testing.T) {
	tests := []struct {
		name    string
		socket  string
		wantErr string
	}{
		{"empty", "", "empty"},
		{"relative", "sock/tmux", "absolute"},
		{"bare name", "sock", "absolute"},
		{"unclean traversal", "/tmp/fc/../fc/sock", "clean"},
		{"unclean trailing slash", "/tmp/fc/sock/", "clean"},
		{"unclean dot", "/tmp/fc/./sock", "clean"},
		{"absolute and clean", testSocket, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewPinned(&internalexec.FakeRunner{}, tt.socket)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewPinned(%q) = %v, want nil", tt.socket, err)
				}
				if c.socket != tt.socket {
					t.Errorf("socket = %q, want %q", c.socket, tt.socket)
				}
				return
			}
			// Classification first — that is the contract. The message check
			// below is a secondary quality assertion, so a reword breaks only
			// the message expectation and never the classification one.
			if !errors.Is(err, ErrInvalidSocketPath) {
				t.Fatalf("NewPinned(%q) = %v, want ErrInvalidSocketPath", tt.socket, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewPinned(%q) error = %q, want it to mention %q", tt.socket, err, tt.wantErr)
			}
		})
	}
}

// TestPinnedClientPinsEveryCommand is the structural assertion behind the whole
// design: every tmux command this client issues must lead with the pin.
//
// The fake serves LIVE rows matching the identities under test, and that is the
// whole point rather than a convenience. An earlier version of this test used an
// empty fake, so every revalidating method bailed inside RevalidateSession /
// RevalidateWindow before issuing its command — the mutating verbs never ran,
// and deleting tmuxArgs from KillWindow survived a test that calls KillWindow.
// A test that drives a method without reaching its command asserts nothing about
// that method.
//
// The recorded-verb assertion is what makes this catch a FUTURE unpinned
// command: wantVerbs is checked for completeness, so a new mutating method whose
// argv skips tmuxArgs shows up as an unpinned entry rather than passing unseen.
func TestPinnedClientPinsEveryCommand(t *testing.T) {
	const pid, start = "9", "1"
	gen := ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: pid, StartTime: start}
	session := SessionIdentity{Generation: gen, ID: "$1", Name: "forge"}
	window := WindowIdentity{Generation: gen, ID: "@1", SessionID: "$1", Name: "editor"}

	sessionRow := strings.Join([]string{pid, start, "$1", "forge", "1", "0", "0", "/tmp"}, FieldSep)
	windowRow := strings.Join([]string{pid, start, "@1", "$1", "forge", "0", "editor", "1", "1"}, FieldSep)
	paneRow := strings.Join([]string{pid, start, "%1", "@1", "0", "title", "zsh", "1"}, FieldSep)

	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			switch {
			case slices.Contains(args, "list-sessions"):
				return sessionRow, nil
			case slices.Contains(args, "list-windows"):
				return windowRow, nil
			case slices.Contains(args, "list-panes"):
				return paneRow, nil
			case slices.Contains(args, "new-session"):
				return strings.Join([]string{pid, start, "$1"}, FieldSep), nil
			case slices.Contains(args, "-V"):
				// A version tmux's own parser accepts, so
				// CheckGenerationCapability proceeds to display-message rather
				// than bailing after -V and leaving that argv undriven.
				return "tmux 3.7b", nil
			case slices.Contains(args, "display-message"):
				return strings.Join([]string{pid, start}, FieldSep), nil
			}
			return "", nil
		},
	}
	c := pinnedClient(t, run)
	ctx := context.Background()

	// Outcomes are not under test — the argv is. Failures here would be caught
	// by the verb-coverage assertion below, not swallowed.
	_, _ = c.ListSessions(ctx)
	_, _ = c.ListWindows(ctx)
	_, _ = c.ListPanes(ctx)
	_, _ = c.CreateSession(ctx, "forge", "/tmp")
	_ = c.RenameSession(ctx, session, "renamed")
	_ = c.KillWindow(ctx, window)
	_ = c.KillSession(ctx, session)
	_ = c.KillOthers(ctx, session)
	_ = c.SelectWindow(ctx, window)
	// These three contribute NO argv, and saying so is the point. All of them
	// refuse with ErrAttachUnavailableWhenPinned before issuing any command, so
	// AttachWindow's own select-window site — a second call site the verb set
	// could not distinguish from SelectWindow's anyway — is unreachable while
	// pinned and is deliberately NOT asserted here. Its `tmuxArgs` call is
	// defence in depth against the refusal ever being relaxed, not a covered
	// path; dropping the pin from it survives this test, by design.
	//
	// They are driven anyway so that a refusal regressing into a command shows
	// up as an unexpected recorded argv rather than silence. The refusals
	// themselves are covered by TestPinnedClientRefusesAttachAndSwitch.
	_ = c.AttachWindow(ctx, window)
	_ = c.AttachSession(ctx, session)
	_ = c.LastSession(ctx)
	// version.go owns two more pinned argv sites (`-V` and `display-message`)
	// that no other method reaches. A pinned caller does reach them —
	// internal/pr calls CheckGenerationCapability — so leaving them undriven
	// left both unasserted.
	_, _ = c.CheckGenerationCapability(ctx)

	if len(run.Calls) == 0 {
		t.Fatal("no commands recorded — the test drove nothing")
	}
	seen := map[string]bool{}
	for _, call := range run.Calls {
		if call.Name != "tmux" {
			t.Errorf("unexpected binary %q with args %v", call.Name, call.Args)
			continue
		}
		if len(call.Args) < 3 || call.Args[0] != "-S" || call.Args[1] != testSocket {
			t.Errorf("argv %v does not lead with the socket pin -S %s", call.Args, testSocket)
			continue
		}
		seen[call.Args[2]] = true
	}

	// The mutating verbs are listed explicitly because they are the ones with
	// blast radius: `kill-session -a` unpinned kills every session on the
	// DEFAULT server while the operator asked about the pinned one.
	//
	// `switch-client` and `attach-session` are deliberately absent: both are
	// refused outright under a pin (ErrAttachUnavailableWhenPinned), so listing
	// either would be asserting a command that cannot run. Their refusal is
	// tested by TestPinnedClientRefusesAttachAndSwitch.
	wantVerbs := []string{
		"list-sessions", "list-windows", "list-panes", "new-session",
		"rename-session", "kill-window", "kill-session", "select-window",
		"-V", "display-message",
	}
	for _, verb := range wantVerbs {
		if !seen[verb] {
			t.Errorf("%s never reached the runner — the test drove a method that bailed before issuing its command, so it asserts nothing about that method's argv", verb)
		}
	}
}

// TestPinnedKillOthersIsPinned isolates the highest-blast-radius command in the
// package. `kill-session -a -t <id>` kills every OTHER session on the server it
// reaches, so an unpinned one destroys the operator's real sessions while they
// asked about a private surface server. It gets its own test because a verb-set
// assertion cannot distinguish it from a plain `kill-session`.
func TestPinnedKillOthersIsPinned(t *testing.T) {
	const pid, start = "9", "1"
	gen := ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: pid, StartTime: start}
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "list-sessions") {
				return strings.Join([]string{pid, start, "$1", "forge", "1", "0", "0", "/tmp"}, FieldSep), nil
			}
			return "", nil
		},
	}
	c := pinnedClient(t, run)

	if err := c.KillOthers(context.Background(), SessionIdentity{Generation: gen, ID: "$1", Name: "forge"}); err != nil {
		t.Fatalf("KillOthers: %v", err)
	}
	var killAll []string
	for _, call := range run.Calls {
		if slices.Contains(call.Args, "-a") && slices.Contains(call.Args, "kill-session") {
			killAll = call.Args
		}
	}
	if killAll == nil {
		t.Fatal("kill-session -a never reached the runner")
	}
	if killAll[0] != "-S" || killAll[1] != testSocket {
		t.Errorf("kill-session -a argv = %v, want it pinned to %s", killAll, testSocket)
	}
}

func TestUnpinnedClientArgvIsUnchanged(t *testing.T) {
	run := &internalexec.FakeRunner{}
	c := New(run)
	c.getenv = func(string) string { return "" }
	c.getuid = func() int { return 501 }
	c.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	if _, err := c.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(run.Calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(run.Calls))
	}
	want := []string{"list-sessions", "-F", sessionFormat}
	if !slices.Equal(run.Calls[0].Args, want) {
		t.Errorf("argv = %v, want %v — an unpinned client must be byte-identical to before the pin existed", run.Calls[0].Args, want)
	}
}

// TestTmuxArgsNeverAliasesTheCallersSlice pins the one hazard that would
// otherwise be MODE-DEPENDENT — present unpinned, absent pinned — which is the
// worst shape for a latent bug.
//
// Go passes a named slice through a variadic parameter without copying, so
// returning it directly hands back the caller's array; appending to the result
// then writes into the caller's spare capacity. CreateSession already appends
// to a tmuxArgs result, so the triggering pattern is in the file.
func TestTmuxArgsNeverAliasesTheCallersSlice(t *testing.T) {
	pinned, err := NewPinned(&internalexec.FakeRunner{}, testSocket)
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	for _, tc := range []struct {
		name   string
		client *Client
	}{
		{"unpinned", New(&internalexec.FakeRunner{})},
		{"pinned", pinned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// base leaves exactly two slots of spare capacity, and the append
			// below adds exactly two. Appending MORE than the spare capacity
			// would make Go reallocate and the aliasing would never show —
			// the first version of this test did that and passed against a
			// deliberately aliasing tmuxArgs.
			backing := []string{"new-session", "-d", "-s", "UNTOUCHED", "UNTOUCHED"}
			base := backing[:3]

			got := tc.client.tmuxArgs(base...)
			got = append(got, "-c", "/tmp")
			_ = got

			for i := 3; i < len(backing); i++ {
				if backing[i] != "UNTOUCHED" {
					t.Fatalf("appending to the tmuxArgs result overwrote the caller's backing array at %d (%q) — tmuxArgs must not return the caller's slice", i, backing[i])
				}
			}
		})
	}
}

// TestPinnedClientMayCreateItsFirstServer is the finding that blocked #332: a
// `-S`-pinned argv used to be refused a serverAbsent verdict outright, so the
// surface adapter could never create the session it exists to create.
//
// The assertion is on ErrNoServer specifically, because that is the ONE
// classification documented as permitting a create.
//
// The argv is `list-sessions`, not `new-session`, because that is the one that
// actually reaches this function on the live path: EnsureSession ->
// ResolveSessionExact -> ListSessions -> absentServer. CreateSession's own
// failures route through classifyCreateFailure and never get here, so testing a
// new-session argv would assert the right property about the wrong command.
func TestPinnedClientMayCreateItsFirstServer(t *testing.T) {
	c := pinnedClient(t, &internalexec.FakeRunner{})
	args := c.tmuxArgs("list-sessions", "-F", sessionFormat)

	lstatted := ""
	c.lstat = func(path string) (os.FileInfo, error) {
		// The run dir exists, the socket in it does not — the state a surface
		// adapter is in just before it brings up its first server.
		if path == filepath.Dir(testSocket) {
			return nil, nil
		}
		lstatted = path
		return nil, os.ErrNotExist
	}

	err := c.serverStateError(context.Background(), args, commandFailure("tmux", args, "no server running"))
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("serverStateError = %v, want ErrNoServer", err)
	}
	if lstatted != testSocket {
		t.Errorf("inspected socket %q, want the pinned %q — a pinned client must judge absence by ITS socket, never the derived default", lstatted, testSocket)
	}
	// The message names the mode, because "default socket" on a pinned client
	// sends the operator to look at the wrong file. The path is quoted, since
	// it reaches a terminal.
	if !strings.Contains(err.Error(), "pinned socket "+termsafe.QuotePath(testSocket)) {
		t.Errorf("error = %q, want it to name the pinned socket", err)
	}
	if strings.Contains(err.Error(), "default socket") {
		t.Errorf("error = %q calls a pinned socket the default one", err)
	}
}

// TestPinnedMissingSocketDirIsNotAnAbsentServer separates two states an lstat
// reports identically. tmux CREATES the default socket's directory but never an
// explicit `-S` one, so "no directory" cannot be answered by creating a server —
// granting the create verdict there sends the caller into a bind failure against
// a path nothing can bind, which is exactly the unattributable error the
// construction-time checks exist to prevent.
func TestPinnedMissingSocketDirIsNotAnAbsentServer(t *testing.T) {
	c := pinnedClient(t, &internalexec.FakeRunner{})
	c.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist } // nothing exists, dir included
	args := c.tmuxArgs("list-sessions", "-F", sessionFormat)

	err := c.serverStateError(context.Background(), args, commandFailure("tmux", args, "no server running"))
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("a missing socket DIRECTORY got the create-permitting ErrNoServer: %v", err)
	}
	if !errors.Is(err, ErrSocketDirMissing) {
		t.Fatalf("serverStateError = %v, want ErrSocketDirMissing", err)
	}

	// An environmental client must be unaffected: tmux makes its own default
	// socket directory, so absence there is still "no server yet".
	env := New(&internalexec.FakeRunner{})
	env.getenv = func(string) string { return "" }
	env.getuid = func() int { return 501 }
	env.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	envArgs := []string{"list-sessions", "-F", sessionFormat}
	if err := env.serverStateError(context.Background(), envArgs, commandFailure("tmux", envArgs, "no server")); !errors.Is(err, ErrNoServer) {
		t.Errorf("environmental client = %v, want ErrNoServer — the directory check must not touch this mode", err)
	}
}

// TestPinnedClientRefusesAnArgvAimedElsewhere covers both halves of pinnedArgs.
// The trailing-override case is the one that matters most: tmux honours the
// LAST `-S`, so an argv that merely starts with the pin can still run against a
// different server.
func TestPinnedClientRefusesAnArgvAimedElsewhere(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no pin at all", []string{"list-windows"}},
		{"a different socket", []string{"-S", "/tmp/someone-else", "list-windows"}},
		{"pin not leading", []string{"list-windows", "-S", testSocket}},
		// A SECOND leading -S genuinely overrides the pin on tmux 3.7b
		// (`tmux -S /a -S /b` connects to /b), which is what the tail check is
		// for.
		{"second leading socket wins", []string{"-S", testSocket, "-S", "/tmp/someone-else", "list-windows"}},
		// These two are inert on 3.7b — tmux's global options stop at the
		// command name. They are refused anyway, because proving inertness
		// means modelling a grammar that can change under us and refusing
		// costs nothing.
		{"socket after the command, refused though inert", []string{"-S", testSocket, "list-windows", "-S", "/tmp/someone-else"}},
		{"label after the command, refused though inert", []string{"-S", testSocket, "list-windows", "-L", "other"}},
		// -2S/path sets the socket on 3.7b while beginning with neither -S nor
		// -L, which is why the check cannot be a prefix test.
		{"bundled socket option", []string{"-S", testSocket, "list-windows", "-2S/tmp/someone-else"}},
		// A double-dash element is scanned too. "tmux has no long options" would
		// have justified skipping it on the OPTION grammar — but this function
		// also scans argv tails, where a `--` element is an operand, and the
		// safe direction is to refuse it rather than reason about which it is.
		{"double-dash element carrying S", []string{"-S", testSocket, "list-windows", "--Startup"}},
		{"truncated pin", []string{"-S"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := pinnedClient(t, &internalexec.FakeRunner{})
			lstatCalls := 0
			c.lstat = func(string) (os.FileInfo, error) {
				lstatCalls++
				return nil, os.ErrNotExist
			}
			err := c.serverStateError(context.Background(), tt.args, commandFailure("tmux", tt.args, "no server running"))
			if errors.Is(err, ErrNoServer) {
				t.Errorf("argv %v got the create-permitting ErrNoServer", tt.args)
			}
			// ErrUnpinnedCommand, not the generic ErrServerUnreadable: this is
			// forgectl having aimed a command at the wrong server, and an
			// operator told "tmux is unreadable" goes and debugs their tmux.
			if !errors.Is(err, ErrUnpinnedCommand) {
				t.Errorf("argv %v = %v, want ErrUnpinnedCommand", tt.args, err)
			}
			if lstatCalls != 0 {
				t.Errorf("argv %v caused %d lstat calls — a refused argv must never reach the filesystem", tt.args, lstatCalls)
			}
		})
	}
}

// TestPinnedSelectorIgnoresTheEnvironment pins the two-mode contract on
// ServerSelector. $TMUX being set is the case that would otherwise manufacture
// a spurious ErrSelectorChanged the moment the operator attaches to any session.
func TestPinnedSelectorIgnoresTheEnvironment(t *testing.T) {
	c := pinnedClient(t, &internalexec.FakeRunner{})
	c.getenv = func(key string) string {
		switch key {
		case "TMUX":
			return "/tmp/tmux-501/default,42,0"
		case "TMUX_TMPDIR":
			return "/somewhere/else"
		}
		return ""
	}
	got := c.currentSelector()
	want := ServerSelector{Socket: testSocket}
	if got != want {
		t.Errorf("currentSelector() = %+v, want %+v", got, want)
	}
}

// TestPinnedRefusalsQuoteTheSocket covers the scenario refuseWhenPinned's own
// comment names: a socket that did NOT come through NewPinned's control-
// character screen. Written as two sibling guards, one quoted the path and one
// did not, and nothing said which was the rule — so this asserts the rule
// rather than leaving it to whichever guard a future reader copies.
func TestPinnedRefusalsQuoteTheSocket(t *testing.T) {
	c := New(&internalexec.FakeRunner{})
	c.socket = "/tmp/\x1b[2J/sock" // bypasses NewPinned, as a second constructor might

	for name, err := range map[string]error{
		"attach": c.refuseAttachWhenPinned(),
		"sesh":   c.refuseSeshWhenPinned(),
	} {
		if err == nil {
			t.Fatalf("%s refusal returned nil for a pinned client", name)
		}
		if strings.ContainsRune(err.Error(), '\x1b') {
			t.Errorf("%s refusal carried a raw escape sequence to the terminal: %q", name, err.Error())
		}
	}
}

// TestSelectorRenderingIsTerminalSafe: ErrSelectorChanged prints a whole
// ServerSelector at the operator, and two of its three fields come from the
// ENVIRONMENT — so a bare %+v hands the terminal whatever $TMUX_TMPDIR says.
// The Stringer is what closes that, and closing it once covers every %v site
// rather than leaving each one to remember.
func TestSelectorRenderingIsTerminalSafe(t *testing.T) {
	hostile := ServerSelector{TmpDir: "/tmp/\x1b[2J\x1b[H"}
	rendered := hostile.String()
	if strings.ContainsRune(rendered, '\x1b') {
		t.Errorf("ServerSelector.String() = %q — it passed an escape sequence through", rendered)
	}

	// And it must reach a real error the same way, not just via String().
	run := &internalexec.FakeRunner{}
	c := New(run)
	c.getenv = func(key string) string {
		if key == "TMUX_TMPDIR" {
			return "/tmp/\x1b[2J"
		}
		return ""
	}
	_, err := c.RevalidateSession(context.Background(), SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
	})
	if !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession = %v, want ErrSelectorChanged", err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("ErrSelectorChanged carried a raw escape sequence to the terminal: %q", err.Error())
	}
}

// TestPinnedIdentityIsRefusedByAnUnpinnedClient is the cross-server guarantee
// the Socket field exists to provide: without it both selectors are {"", ""}
// and an id minted on the pinned socket would be acted on against the default
// server, which is a stranger's session with the same native id.
func TestPinnedIdentityIsRefusedByAnUnpinnedClient(t *testing.T) {
	run := &internalexec.FakeRunner{}
	unpinned := New(run)
	unpinned.getenv = func(string) string { return "" }

	pinnedIdentity := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
		Name:       "forge",
	}
	_, err := unpinned.RevalidateSession(context.Background(), pinnedIdentity)
	if !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession = %v, want ErrSelectorChanged", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("ran %d commands before refusing: %v — a selector refusal must run zero tmux commands", len(run.Calls), run.Calls)
	}
}

// TestPinnedIdentityIsRefusedByADifferentPin is the same guarantee between two
// pinned clients — the case a single boolean "is pinned" flag would miss.
func TestPinnedIdentityIsRefusedByADifferentPin(t *testing.T) {
	run := &internalexec.FakeRunner{}
	other, err := NewPinned(run, "/tmp/fc-surface/other")
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	pinnedIdentity := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
	}
	if _, err := other.RevalidateSession(context.Background(), pinnedIdentity); !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession = %v, want ErrSelectorChanged", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("ran %d commands before refusing", len(run.Calls))
	}
}

// TestCreatedSessionCarriesThePinnedSelector closes the loop: an identity minted
// by a pinned CreateSession must be revalidatable by that same client, and the
// selector it carries is what makes the two previous tests refusals rather than
// accidents.
func TestCreatedSessionCarriesThePinnedSelector(t *testing.T) {
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "new-session") {
				return "9" + FieldSep + "1" + FieldSep + "$3", nil
			}
			return "", nil
		},
	}
	c := pinnedClient(t, run)

	got, err := c.CreateSession(context.Background(), "forge", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	want := ServerSelector{Socket: testSocket}
	if got.Generation.Selector != want {
		t.Errorf("minted selector = %+v, want %+v", got.Generation.Selector, want)
	}
	if got.ID != "$3" {
		t.Errorf("minted id = %q, want $3", got.ID)
	}
}

// TestPinnedClientRefusesSesh guards the one delegation the pin cannot reach:
// sesh has no -S to thread, so it would act on the environmental server.
func TestPinnedClientRefusesSesh(t *testing.T) {
	// Both lookPath outcomes are driven, because they are the only inputs that
	// distinguish the documented ordering (refuse first) from the inverted one.
	// Asserting only the resolving case cannot see the swap: the refusal still
	// fires, just behind a different error on machines without sesh.
	for _, tc := range []struct {
		name     string
		lookPath func(string) (string, error)
	}{
		{"sesh installed", func(string) (string, error) { return "/usr/bin/sesh", nil }},
		{"sesh missing", func(string) (string, error) { return "", errors.New("not found in $PATH") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{}
			c := pinnedClient(t, run)
			c.lookPath = tc.lookPath

			if _, err := c.SeshList(context.Background()); !errors.Is(err, ErrSeshUnavailableWhenPinned) {
				t.Errorf("SeshList = %v, want ErrSeshUnavailableWhenPinned", err)
			}
			if err := c.Pick(context.Background(), "forge"); !errors.Is(err, ErrSeshUnavailableWhenPinned) {
				t.Errorf("Pick = %v, want ErrSeshUnavailableWhenPinned", err)
			}
			if len(run.Calls) != 0 {
				t.Errorf("ran %d commands: %v — a refused sesh delegation must run nothing", len(run.Calls), run.Calls)
			}
		})
	}
}

// TestPinnedClientRefusesAttachAndSwitch covers the seam whose two branches are
// both wrong under a pin — and the reachable one is broken exactly when a pin is
// in play. InsideTmux() is false when pinned, so attachOrSwitch takes the
// attach-session branch; tmux refuses that outright while $TMUX is set, which is
// precisely the operator-inside-tmux case a surface pin exists for.
//
// The refusal must run BEFORE any command, so all three entry points are driven
// and the runner is required to have seen nothing.
func TestPinnedClientRefusesAttachAndSwitch(t *testing.T) {
	const pid, start = "9", "1"
	gen := ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: pid, StartTime: start}
	sessionRow := strings.Join([]string{pid, start, "$1", "forge", "1", "0", "0", "/tmp"}, FieldSep)
	windowRow := strings.Join([]string{pid, start, "@1", "$1", "forge", "0", "editor", "1", "1"}, FieldSep)

	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"AttachSession", func(c *Client) error {
			return c.AttachSession(context.Background(), SessionIdentity{Generation: gen, ID: "$1", Name: "forge"})
		}},
		{"AttachWindow", func(c *Client) error {
			return c.AttachWindow(context.Background(), WindowIdentity{Generation: gen, ID: "@1", SessionID: "$1", Name: "editor"})
		}},
		{"LastSession", func(c *Client) error { return c.LastSession(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{
				RunFunc: func(_ string, args []string) (string, error) {
					switch {
					case slices.Contains(args, "list-sessions"):
						return sessionRow, nil
					case slices.Contains(args, "list-windows"):
						return windowRow, nil
					}
					return "", nil
				},
			}
			// insideTmux true is the case that makes attach-session fail at
			// tmux; a pinned client must refuse rather than reach it.
			c := pinnedClient(t, run)
			c.insideTmux = func() bool { return true }

			if err := tc.call(c); !errors.Is(err, ErrAttachUnavailableWhenPinned) {
				t.Fatalf("%s = %v, want ErrAttachUnavailableWhenPinned", tc.name, err)
			}
			for _, call := range run.Calls {
				for _, arg := range call.Args {
					if arg == "attach-session" || arg == "switch-client" {
						t.Errorf("%s reached the runner with %q despite the refusal", tc.name, arg)
					}
				}
			}
		})
	}

	// A STALE identity is the case the attachOrSwitch backstop cannot answer
	// correctly. With no rows, revalidation fails first, and if the refusal sat
	// only at the seam the caller would get a transient-looking ErrObjectGone
	// for a client that can never attach — and would retry forever. The check
	// has to be ahead of revalidation, and this is what proves it is.
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"AttachSession", func(c *Client) error {
			return c.AttachSession(context.Background(), SessionIdentity{Generation: gen, ID: "$9", Name: "gone"})
		}},
		{"AttachWindow", func(c *Client) error {
			return c.AttachWindow(context.Background(), WindowIdentity{Generation: gen, ID: "@9", SessionID: "$9", Name: "gone"})
		}},
	} {
		t.Run(tc.name+" with a stale identity", func(t *testing.T) {
			run := &internalexec.FakeRunner{} // no rows: revalidation cannot succeed
			c := pinnedClient(t, run)

			err := tc.call(c)
			if errors.Is(err, ErrObjectGone) {
				t.Fatalf("%s reported a transient ErrObjectGone for a client that can NEVER attach: %v", tc.name, err)
			}
			if !errors.Is(err, ErrAttachUnavailableWhenPinned) {
				t.Fatalf("%s = %v, want ErrAttachUnavailableWhenPinned", tc.name, err)
			}
			if len(run.Calls) != 0 {
				t.Errorf("%s ran %d commands before refusing: %v", tc.name, len(run.Calls), run.Calls)
			}
		})
	}

	// The refusal is pin-scoped: an unpinned client must still attach.
	run := &internalexec.FakeRunner{}
	unpinned := New(run, WithInsideTmux(func() bool { return true }))
	unpinned.getenv = func(string) string { return "" }
	if err := unpinned.attachOrSwitch(context.Background(), "$1", "forge"); err != nil {
		t.Fatalf("unpinned attachOrSwitch = %v, want nil — the refusal must not touch the environmental mode", err)
	}
	if len(run.Calls) != 1 {
		t.Errorf("unpinned attachOrSwitch ran %d commands, want 1", len(run.Calls))
	}
}

// TestNewPinnedScreensTerminalHostilePaths: this path is caller-supplied and
// lands in an error an operator reads, so an absolute, lexically clean path
// carrying an escape sequence must not get through. The refusal reports a byte
// offset and never echoes the bytes it is refusing.
func TestNewPinnedScreensTerminalHostilePaths(t *testing.T) {
	hostile := "/tmp/fc\x1b[2J/sock"
	c, err := NewPinned(&internalexec.FakeRunner{}, hostile)
	if !errors.Is(err, ErrInvalidSocketPath) {
		t.Fatalf("NewPinned(control char) = (%v, %v), want ErrInvalidSocketPath", c, err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("the refusal echoed the control character it refused: %q", err.Error())
	}

	long := "/tmp/" + strings.Repeat("a", MaxSocketPathLen)
	if _, err := NewPinned(&internalexec.FakeRunner{}, long); !errors.Is(err, ErrInvalidSocketPath) {
		t.Errorf("NewPinned(%d bytes) = %v, want ErrInvalidSocketPath", len(long), err)
	}
	// At the cap, not over it — a refusal case without an acceptance case
	// beside it cannot tell a correct bound from an off-by-one.
	atCap := "/tmp/" + strings.Repeat("a", MaxSocketPathLen-len("/tmp/"))
	if len(atCap) != MaxSocketPathLen {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atCap), MaxSocketPathLen)
	}
	if _, err := NewPinned(&internalexec.FakeRunner{}, atCap); err != nil {
		t.Errorf("NewPinned at exactly the %d-byte cap = %v, want nil", MaxSocketPathLen, err)
	}
}

// TestPinnedClientIsNeverInsideTmux: $TMUX describes a client of a DIFFERENT
// server under a pin, so reporting true would route switch-client to the pinned
// server on behalf of a client not attached to it.
func TestPinnedClientIsNeverInsideTmux(t *testing.T) {
	c, err := NewPinned(&internalexec.FakeRunner{}, testSocket, WithInsideTmux(func() bool { return true }))
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	if c.InsideTmux() {
		t.Error("InsideTmux() = true for a pinned client")
	}

	unpinned := New(&internalexec.FakeRunner{}, WithInsideTmux(func() bool { return true }))
	if !unpinned.InsideTmux() {
		t.Error("InsideTmux() = false for an unpinned client — the pin must not change the environmental answer")
	}
}
