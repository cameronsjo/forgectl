package backend_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestNewStartSpec_RefusesADisplayNameAManagerWouldReadAsAFlag closes a hole
// that only became reachable when an adapter first put the display name into an
// argv.
//
// The display name is operator-supplied — `forgectl surface launch --name …` —
// and StartSpec seals it as an exec.Opaque. The sensitive seam refuses an opaque
// argument beginning with a dash unless an end-of-options separator precedes it,
// and no separator can be inserted between a flag and its value. So a name like
// "-f" made the whole sensitive command invalid, which the adapter classified as
// FailureInternal: an internal-defect verdict, on a cosmetic label, for a launch
// that was otherwise fine.
//
// Refusing here instead is both earlier and more honest. It happens before any
// mutation, and it tells the operator the thing they can actually act on rather
// than sending them to look for a bug in forgectl.
//
// The tmux adapter never used this field, which is why nothing caught it until
// cmux did.
func TestNewStartSpec_RefusesADisplayNameAManagerWouldReadAsAFlag(t *testing.T) {
	tag := recoveryTag(t)
	bootstrap, err := backend.NewBootstrapCommand(exec.Opaque("forgectl surface _exec"))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}

	for _, name := range []string{"-f", "--name", "-", "-rf"} {
		t.Run(name, func(t *testing.T) {
			_, err := backend.NewStartSpec("/tmp/repo", name, tag, bootstrap)
			if !errors.Is(err, backend.ErrInvalidStartSpec) {
				t.Errorf("NewStartSpec(name=%q) = %v, want ErrInvalidStartSpec", name, err)
			}
		})
	}

	// The acceptance side, without which a constructor that refused every name
	// would pass the table above. A dash INSIDE the name is fine — only a
	// leading one is ambiguous to an argument parser.
	for _, name := range []string{"forgectl", "my-repo", "repo-2"} {
		t.Run("accepts "+name, func(t *testing.T) {
			spec, err := backend.NewStartSpec("/tmp/repo", name, tag, bootstrap)
			if err != nil {
				t.Fatalf("NewStartSpec(name=%q) = %v, want a valid spec", name, err)
			}
			if err := spec.Validate(); err != nil {
				t.Errorf("spec.Validate() = %v", err)
			}
		})
	}
}

// TestAnAcceptedDisplayNameAlwaysSurvivesTheSeam is the property the refusal
// above exists to guarantee, asserted end to end rather than by restating the
// rule.
//
// It builds the same command shape an adapter does — an opaque display name as
// a flag's value, with no end-of-options separator available — and requires the
// seam to accept it. A future relaxation of either rule that let the two drift
// apart would fail here rather than at an operator's terminal.
func TestAnAcceptedDisplayNameAlwaysSurvivesTheSeam(t *testing.T) {
	tag := recoveryTag(t)
	bootstrap, err := backend.NewBootstrapCommand(exec.Opaque("forgectl surface _exec"))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}
	spec, err := backend.NewStartSpec("/tmp/repo", "my-repo", tag, bootstrap)
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}

	var fake exec.FakeSensitiveRunner
	// The fake validates exactly as the production runner does, which is what
	// makes this an assertion about the seam and not about the fake.
	if _, err := fake.RunSensitive(context.Background(), exec.SensitiveCommand{
		Kind:      exec.KindCmuxCreate,
		Path:      exec.Secret("/opt/homebrew/bin/cmux"),
		Args:      []exec.Arg{exec.MustFixed("--name"), spec.Name()},
		Env:       []exec.EnvMutation{exec.SetCmuxQuiet()},
		StdoutCap: 1 << 10,
		StderrCap: 1 << 10,
	}); err != nil {
		t.Errorf("a display name NewStartSpec accepted was refused by the seam: %v", err)
	}
}

// TestNewStartSpec_StillRefusesTheOlderShapes guards against the new rule being
// added in a way that displaces the existing ones.
func TestNewStartSpec_StillRefusesTheOlderShapes(t *testing.T) {
	tag := recoveryTag(t)
	bootstrap, err := backend.NewBootstrapCommand(exec.Opaque("forgectl surface _exec"))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}
	cases := map[string]string{
		"empty":             "",
		"control character": "repo\x1b[2J",
		"oversized":         strings.Repeat("n", 4096),
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := backend.NewStartSpec("/tmp/repo", name, tag, bootstrap); !errors.Is(err, backend.ErrInvalidStartSpec) {
				t.Errorf("NewStartSpec(%s) = %v, want ErrInvalidStartSpec", label, err)
			}
		})
	}
}
