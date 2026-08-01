package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// forgectl#190: `close` used to alias `pr teardown`, which is destructive
// (restores + removes the clean-room workspace). Under a substring-matching
// agent harness, that short spelling collides with `gh pr close` — an
// allowlist entry meant to scope a read-only-adjacent gh call can end up
// also matching the destructive forgectl alias. The alias must never come
// back.
func TestPrTeardownCmd_HasNoAliases(t *testing.T) {
	cmd := newPrTeardownCmd(pr.New(&exec.FakeRunner{}))
	if cmd.Name() != "teardown" {
		t.Fatalf("expected command name %q, got %q", "teardown", cmd.Name())
	}
	if len(cmd.Aliases) != 0 {
		t.Errorf("pr teardown must not carry a %q alias: it collides with "+
			"`gh pr close` under substring-matching agent harnesses and "+
			"must not be reintroduced; got aliases %v", "close", cmd.Aliases)
	}
}
