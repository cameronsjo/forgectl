package validated

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
)

// TestCommandKeepsOnlyWhatNewChecked pins that a Command shares no backing
// array with its caller: a write to the slices New was given, or to the
// slices Args and Env return, leaves the Command as New checked it
// (forgectl#888).
//
// Mutations that turn it red: New returning `env: env` (the caller's slice)
// instead of its own; Args or Env returning c.args or c.env without a copy.
func TestCommandKeepsOnlyWhatNewChecked(t *testing.T) {
	args := []Arg{{Value: sealed.New("checked-arg"), Kind: ArgOpaque}}
	env := []Env{{Key: "K", Value: sealed.New("checked-env"), Op: EnvOpReplace}}
	cmd, err := New(sealed.New("/bin/true"), args, env, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	args[0] = Arg{Value: sealed.New("-mutated"), Kind: ArgOpaque}
	env[0] = Env{Key: KeySopsTmpdir, Value: sealed.New("rel"), Op: EnvOpReplace}
	cmd.Args()[0] = sealed.New("-mutated")
	cmd.Env()[0] = Env{Key: KeySopsTmpdir, Value: sealed.New("rel"), Op: EnvOpReplace}

	if got := cmd.Args(); len(got) != 1 || !got[0].Equal(sealed.New("checked-arg")) {
		t.Error("a write after New reached the Command's argv")
	}
	if got := cmd.Env(); len(got) != 1 || got[0].Key != "K" || !got[0].Value.Equal(sealed.New("checked-env")) {
		t.Error("a write after New reached the Command's environment")
	}
}

// TestNewRefusesAndReturnsTheZeroCommand pins that every refusal returns the
// zero Command, whose path sealed.Start refuses.
//
// Mutation that turns it red: New returning a Command carrying the path
// alongside an error.
func TestNewRefusesAndReturnsTheZeroCommand(t *testing.T) {
	cases := map[string]func() (Command, error){
		"relative path": func() (Command, error) { return New(sealed.New("sh"), nil, nil, false) },
		"dash-leading opaque arg": func() (Command, error) {
			return New(sealed.New("/bin/sh"), []Arg{{Value: sealed.New("-c"), Kind: ArgOpaque}}, nil, false)
		},
		"TMPDIR off the sops edit": func() (Command, error) {
			return New(sealed.New("/bin/sh"), nil, []Env{{Key: KeySopsTmpdir, Value: sealed.New("/w"), Op: EnvOpReplace}}, false)
		},
	}
	for name, run := range cases {
		cmd, err := run()
		if err == nil {
			t.Errorf("%s: New accepted it", name)
		}
		if cmd.Path().Set() || cmd.Args() != nil || cmd.Env() != nil {
			t.Errorf("%s: a refusal returned a non-zero Command", name)
		}
	}
}
