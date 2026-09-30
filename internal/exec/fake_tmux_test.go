package exec

import (
	"context"
	"errors"
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
// classify a failure. An error naming some other argv is left alone.
//
// Mutation that turns it red: drop the rewrite in answer (the first error keeps
// the view); rewrite without the equality check (the second error changes).
func TestFakeRunnerRestoresIssuedArgvOnCommandError(t *testing.T) {
	other := []string{"new-session", "-s", "x"}
	f := &FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if args[len(args)-1] == "other" {
			return "", &CommandError{Name: name, Args: slices.Clone(other)}
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
}
