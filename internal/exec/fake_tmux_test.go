package exec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// TestFakeRunnerPresentsTmuxArgvWithoutUTF8Flag pins the view every tmux fake
// in the tree reads (forgectl#840): a tmux call's leading `-u` is removed from
// what RunFunc and Calls see, wherever it sits among the leading global
// options, and TmuxUTF8 says it was there. Nothing else is removed, and no
// other binary's argv is touched.
//
// Mutations that turn it red: stop skipping `-S <path>` while looking for -u
// (the pinned row keeps it); strip -u for any binary (the git row loses it);
// strip a -u that follows the command (the trailing row loses it).
func TestFakeRunnerPresentsTmuxArgvWithoutUTF8Flag(t *testing.T) {
	tests := []struct {
		name     string
		bin      string
		args     []string
		wantArgs []string
		wantUTF8 bool
	}{
		{"unpinned", "tmux", []string{"-u", "list-sessions", "-F", "x"}, []string{"list-sessions", "-F", "x"}, true},
		{"pinned", "tmux", []string{"-S", "/s", "-u", "list-sessions"}, []string{"-S", "/s", "list-sessions"}, true},
		{"labelled", "tmux", []string{"-L", "lab", "-f", "/conf", "-u", "ls"}, []string{"-L", "lab", "-f", "/conf", "ls"}, true},
		{"absolute bin", "/usr/bin/tmux", []string{"-u", "ls"}, []string{"ls"}, true},
		{"no flag", "tmux", []string{"-S", "/s", "attach-session", "-t", "$1"}, []string{"-S", "/s", "attach-session", "-t", "$1"}, false},
		{"trailing -u is an operand", "tmux", []string{"send-keys", "-u"}, []string{"send-keys", "-u"}, false},
		{"other binary", "git", []string{"-u", "status"}, []string{"-u", "status"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			f := &FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
				seen = args
				return "", nil
			}}
			if _, err := f.Run(context.Background(), tt.bin, tt.args...); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(seen, tt.wantArgs) {
				t.Errorf("RunFunc saw %q, want %q", seen, tt.wantArgs)
			}
			call := f.Last()
			if !slices.Equal(call.Args, tt.wantArgs) || call.TmuxUTF8 != tt.wantUTF8 {
				t.Errorf("Call = (%q, utf8=%v), want (%q, utf8=%v)", call.Args, call.TmuxUTF8, tt.wantArgs, tt.wantUTF8)
			}
		})
	}
}

// TestFakeRunnerRestoresIssuedArgvOnCommandError: a CommandError a RunFunc
// builds from the argv it was shown must come back naming the argv the caller
// issued, because a real runner's does and internal/tmux compares the two to
// classify a failure. An error naming some other argv is left alone, wrapped
// or not; one naming the view but wrapped inside another error is refused
// with a panic, because the fake cannot restore its argv and classification
// would silently miss it (forgectl#851).
//
// Mutations that turn it red: drop the rewrite (the first error keeps the
// view); rewrite without the equality check (the "other" error changes); drop
// the wrapped-view panic (the "wrapped" call returns instead of panicking);
// panic on any wrapped CommandError (the "wrapped other" call panics).
func TestFakeRunnerRestoresIssuedArgvOnCommandError(t *testing.T) {
	other := []string{"new-session", "-s", "x"}
	f := &FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch args[len(args)-1] {
		case "other":
			return "", &CommandError{Name: name, Args: slices.Clone(other)}
		case "wrapped":
			return "", fmt.Errorf("wrapped: %w", &CommandError{Name: name, Args: slices.Clone(args)})
		case "wrapped-other":
			return "", fmt.Errorf("wrapped: %w", &CommandError{Name: name, Args: slices.Clone(other)})
		}
		return "", &CommandError{Name: name, Args: slices.Clone(args)}
	}}
	issued := []string{"-S", "/s", "-u", "list-sessions"}
	_, err := f.Run(context.Background(), "tmux", issued...)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) || !slices.Equal(cmdErr.Args, issued) {
		t.Errorf("error argv = %v, want the issued %q", err, issued)
	}
	_, err = f.Run(context.Background(), "tmux", "-u", "other")
	if !errors.As(err, &cmdErr) || !slices.Equal(cmdErr.Args, other) {
		t.Errorf("error argv = %v, want the untouched %q", err, other)
	}
	_, err = f.Run(context.Background(), "tmux", "-u", "wrapped-other")
	if !errors.As(err, &cmdErr) || !slices.Equal(cmdErr.Args, other) {
		t.Errorf("wrapped error argv = %v, want the untouched %q", err, other)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a wrapped CommandError naming the view was returned; want the fake to refuse it")
			}
		}()
		_, _ = f.Run(context.Background(), "tmux", "-u", "wrapped")
	}()
}

// TestFakeRunnerRewritesACopyOfAReusedError: a RunFunc may hand back one error
// value from every call. The rewrite must leave that value as the test built
// it, give each call its own issued argv, and keep the chain behind it, so
// errors.Is still reaches the wrapped cause.
//
// Mutation that turns it red: rewrite cmdErr.Args in place and return err (the
// shared value's Args change, and the second call's error reports the first
// call's argv, because the view no longer matches).
func TestFakeRunnerRewritesACopyOfAReusedError(t *testing.T) {
	cause := errors.New("exit status 1")
	shared := &CommandError{Name: "tmux", Args: []string{"list-sessions"}, Err: cause}
	f := &FakeRunner{RunFunc: func(string, []string) (string, error) { return "", shared }}

	for _, issued := range [][]string{{"-u", "list-sessions"}, {"-S", "/s2", "-u", "list-sessions"}} {
		_, err := f.Run(context.Background(), "tmux", issued...)
		var cmdErr *CommandError
		if !errors.As(err, &cmdErr) {
			t.Fatalf("error = %v, want a CommandError", err)
		}
		if issued[0] == "-u" && !slices.Equal(cmdErr.Args, issued) {
			t.Errorf("error argv = %q, want the issued %q", cmdErr.Args, issued)
		}
		if !errors.Is(err, cause) {
			t.Errorf("error %v lost its wrapped cause", err)
		}
	}
	if !slices.Equal(shared.Args, []string{"list-sessions"}) {
		t.Errorf("the RunFunc's own error value was rewritten to %q", shared.Args)
	}
}

// TestTmuxSubcommand pins the one helper hand-written runners use to find a
// tmux command past its global options.
//
// Mutations that turn it red: drop the -u case; drop the value skip for -S.
func TestTmuxSubcommand(t *testing.T) {
	tests := []struct {
		args, want []string
	}{
		{[]string{"list-sessions", "-F", "x"}, []string{"list-sessions", "-F", "x"}},
		{[]string{"-u", "list-sessions"}, []string{"list-sessions"}},
		{[]string{"-S", "/s", "-u", "kill-server"}, []string{"kill-server"}},
		{[]string{"-L", "lab", "-f", "/c", "ls"}, []string{"ls"}},
		{[]string{"-S"}, nil},
		{[]string{"send-keys", "-u"}, []string{"send-keys", "-u"}},
	}
	for _, tt := range tests {
		if got := TmuxSubcommand(tt.args); !slices.Equal(got, tt.want) {
			t.Errorf("TmuxSubcommand(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}
