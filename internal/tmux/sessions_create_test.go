package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// createArgs is the exact argv CreateSession must build. It is asserted rather
// than described because the shape is the contract: `-s` is a CREATION operand
// (tmux never runs it through the target grammar), so the name must arrive
// untouched — no "=" prefix, no trailing colon.
func createArgs(name, dir string) []string {
	args := []string{"new-session", "-d", "-P", "-F", sessionIdentityFormat, "-s", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	return args
}

func sessionListRow(pid, start, id, name string) string {
	return strings.Join([]string{pid, start, id, name, "1", "0", "1700000000", "/w"}, FieldSep)
}

func identityOut(pid, start, id string) string {
	return strings.Join([]string{pid, start, id}, FieldSep)
}

func TestCreateSessionArgvAndIdentity(t *testing.T) {
	// A name that is entirely tmux target syntax proves it is passed through as
	// an operand: if anything ran it through a target builder, this argv would
	// not survive.
	const hostile = "=forge:"
	fake := &internalexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		argsEqual(t, args, createArgs(hostile, "/repo"))
		return identityOut("123", "456", "$4"), nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	got, err := c.CreateSession(context.Background(), hostile, "/repo")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got.ID != "$4" || got.Name != hostile {
		t.Fatalf("identity = %+v, want $4 named %q", got, hostile)
	}
	if got.Generation.PID != "123" || got.Generation.StartTime != "456" {
		t.Fatalf("generation = %+v, want 123/456 captured from the create itself", got.Generation)
	}
	if got.Generation.Selector != (ServerSelector{TmpDir: "/tmp"}) {
		t.Fatalf("selector = %+v, want the selector the create ran under", got.Generation.Selector)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %d, want exactly one create", len(fake.Calls))
	}
}

func TestCreateSessionRejectsMalformedID(t *testing.T) {
	fake := &internalexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		// A window id where a session id belongs — the shared triple parser must
		// not accept it just because the shape is right.
		return identityOut("123", "456", "@4"), nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	if _, err := c.CreateSession(context.Background(), "forge", ""); !errors.Is(err, ErrMalformedID) {
		t.Fatalf("CreateSession error = %v, want ErrMalformedID", err)
	}
}

// TestClassifyCreateFailure is the centralized duplicate classifier. Every
// condition must hold — right argv, exit 1, and stderr EQUAL to tmux's
// diagnostic for exactly this name. Contains-matching would let a session named
// after the diagnostic forge a duplicate verdict.
func TestClassifyCreateFailure(t *testing.T) {
	const name = "forge"
	args := createArgs(name, "")
	tests := []struct {
		label  string
		err    error
		isDupe bool
	}{
		{"exact diagnostic", commandFailure("tmux", args, "duplicate session: forge\n"), true},
		{"different name", commandFailure("tmux", args, "duplicate session: forge-review\n"), false},
		{"substring only", commandFailure("tmux", args, "warning: duplicate session: forge extra\n"), false},
		{"localized", commandFailure("tmux", args, "session dupliquee: forge\n"), false},
		{"wrong argv", commandFailure("tmux", []string{"new-session"}, "duplicate session: forge\n"), false},
		{"plain error", errors.New("tmux: command not found"), false},
		{
			"wrong exit code",
			&internalexec.CommandError{Name: "tmux", Args: args, Stderr: "duplicate session: forge\n", ExitCode: 2, Err: errors.New("exit 2")},
			false,
		},
	}
	c := New(&internalexec.FakeRunner{})
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := c.classifyCreateFailure(args, name, tt.err)
			if errors.Is(got, ErrDuplicateSession) != tt.isDupe {
				t.Fatalf("classify(%v) duplicate = %v, want %v", tt.err, !tt.isDupe, tt.isDupe)
			}
		})
	}
}

// scriptedRunner answers each tmux invocation from a queue keyed by the
// subcommand, and records the order. It is how the create-race matrix asserts
// exact call COUNTS, which is the property that matters: at most one create,
// at most one re-list, and zero attaches on any refusal.
type scriptedRunner struct {
	internalexec.FakeRunner
	listOut  []string
	listErr  []error
	createFn func(args []string) (string, error)
	lists    int
	creates  int
}

func (s *scriptedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "list-sessions" {
		i := s.lists
		s.lists++
		var out string
		var err error
		if i < len(s.listOut) {
			out = s.listOut[i]
		}
		if i < len(s.listErr) {
			err = s.listErr[i]
		}
		_, _ = s.FakeRunner.Run(ctx, name, args...)
		return out, err
	}
	if len(args) > 0 && args[0] == "new-session" {
		s.creates++
		_, _ = s.FakeRunner.Run(ctx, name, args...)
		return s.createFn(args)
	}
	return s.FakeRunner.Run(ctx, name, args...)
}

func TestEnsureSessionStateMachine(t *testing.T) {
	const name = "forge"
	present := sessionListRow("123", "456", "$1", name)
	sibling := sessionListRow("123", "456", "$2", "forge-review")
	dupErr := commandFailure("tmux", createArgs(name, "/repo"), "duplicate session: "+name+"\n")

	tests := []struct {
		label       string
		listOut     []string
		listErr     []error
		createFn    func([]string) (string, error)
		wantID      string
		wantErr     error
		wantLists   int
		wantCreates int
	}{
		{
			label:     "already present, no create at all",
			listOut:   []string{present},
			wantID:    "$1",
			wantLists: 1,
		},
		{
			label:       "absent, exactly one create",
			listOut:     []string{sibling},
			createFn:    func([]string) (string, error) { return identityOut("123", "456", "$7"), nil },
			wantID:      "$7",
			wantLists:   1,
			wantCreates: 1,
		},
		{
			label:       "empty server, exactly one create",
			listOut:     []string{""},
			createFn:    func([]string) (string, error) { return identityOut("123", "456", "$0"), nil },
			wantID:      "$0",
			wantLists:   1,
			wantCreates: 1,
		},
		{
			label:       "lost the race, one re-list finds the winner",
			listOut:     []string{sibling, present},
			createFn:    func([]string) (string, error) { return "", dupErr },
			wantID:      "$1",
			wantLists:   2,
			wantCreates: 1,
		},
		{
			label:       "duplicate reported but winner missing",
			listOut:     []string{sibling, sibling},
			createFn:    func([]string) (string, error) { return "", dupErr },
			wantErr:     ErrSessionNotFound,
			wantLists:   2,
			wantCreates: 1,
		},
		{
			label:   "duplicate, re-list transport failure",
			listOut: []string{sibling, ""},
			listErr: []error{nil, errors.New("tmux died")},
			// The second failure is a plain error, so absentServer will
			// not convert it into an empty listing.
			createFn:    func([]string) (string, error) { return "", dupErr },
			wantErr:     ErrDuplicateSession,
			wantLists:   2,
			wantCreates: 1,
		},
		{
			label:       "unrelated create failure, no re-list and no second create",
			listOut:     []string{sibling},
			createFn:    func([]string) (string, error) { return "", errors.New("server exited unexpectedly") },
			wantErr:     nil,
			wantLists:   1,
			wantCreates: 1,
		},
		{
			label:       "malformed id from create",
			listOut:     []string{sibling},
			createFn:    func([]string) (string, error) { return identityOut("123", "456", "7"), nil },
			wantErr:     ErrMalformedID,
			wantLists:   1,
			wantCreates: 1,
		},
		{
			label:       "unreadable separator on the first list",
			listOut:     []string{"123_456_$1_forge_1_0_1700000000_/w"},
			createFn:    func([]string) (string, error) { t.Fatal("created a session on an unreadable listing"); return "", nil },
			wantErr:     ErrUnreadableFields,
			wantLists:   1,
			wantCreates: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			s := &scriptedRunner{listOut: tt.listOut, listErr: tt.listErr, createFn: tt.createFn}
			c := New(s)
			identityEnv(c, "", "/tmp")
			got, err := c.EnsureSession(context.Background(), name, "/repo")

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			case tt.wantID != "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.ID != tt.wantID {
					t.Fatalf("identity = %+v, want id %q", got, tt.wantID)
				}
			default:
				if err == nil {
					t.Fatal("expected a refusal")
				}
			}
			if s.lists != tt.wantLists {
				t.Errorf("list-sessions calls = %d, want %d", s.lists, tt.wantLists)
			}
			if s.creates != tt.wantCreates {
				t.Errorf("new-session calls = %d, want %d", s.creates, tt.wantCreates)
			}
			// No refusal path may attach, switch, or select anything.
			for _, call := range s.Calls {
				switch call.Args[0] {
				case "list-sessions", "new-session":
				default:
					t.Errorf("EnsureSession ran %v; it must only list and create", call.Args)
				}
			}
		})
	}
}

// TestEnsureSessionRefusesPrefixSibling is the #237 case stated as a state:
// when only a prefix sibling exists, the requested name is ABSENT, and the
// machine must create it rather than adopt the sibling.
func TestEnsureSessionRefusesPrefixSibling(t *testing.T) {
	s := &scriptedRunner{
		listOut:  []string{sessionListRow("123", "456", "$2", "forge-review")},
		createFn: func([]string) (string, error) { return identityOut("123", "456", "$3"), nil },
	}
	c := New(s)
	identityEnv(c, "", "/tmp")
	got, err := c.EnsureSession(context.Background(), "forge", "")
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if got.ID == "$2" {
		t.Fatal("EnsureSession adopted the prefix sibling forge-review as forge")
	}
	if got.ID != "$3" || got.Name != "forge" {
		t.Fatalf("identity = %+v, want the freshly created $3 named forge", got)
	}
}
