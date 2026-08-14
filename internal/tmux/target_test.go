package tmux

import (
	"errors"
	"strings"
	"testing"
)

// TestNewWindowSessionTargetKeepsTrailingColon pins the one argv element the
// forgectl#237 reproduction turns on. Deleting the ":" from
// NewWindowSessionTarget must fail here.
func TestNewWindowSessionTargetKeepsTrailingColon(t *testing.T) {
	got, err := NewWindowSessionTarget("$3")
	if err != nil {
		t.Fatalf("NewWindowSessionTarget: %v", err)
	}
	if got != "$3:" {
		t.Fatalf("target = %q, want %q", got, "$3:")
	}
	if !strings.HasSuffix(got, ":") {
		t.Fatal("the trailing colon is what makes the operand session-qualified for new-window")
	}
	// It is ONE argv element. The reproduction is not "an extra argument", it is
	// this string being wrong by one character.
	if strings.ContainsAny(got, " \t") {
		t.Fatalf("target %q must be a single argv element", got)
	}
}

// TestNewWindowSessionTargetRejectsNames is the other half: the builder takes a
// native id, so a NAME cannot be laundered through it into a `-t` operand.
func TestNewWindowSessionTargetRejectsNames(t *testing.T) {
	for _, bad := range []string{"forgectl", "=forgectl", "=forgectl:", "forgectl:", "", "$1:", "@1"} {
		if _, err := NewWindowSessionTarget(bad); !errors.Is(err, ErrMalformedID) {
			t.Errorf("NewWindowSessionTarget(%q) error = %v, want ErrMalformedID", bad, err)
		}
	}
}
