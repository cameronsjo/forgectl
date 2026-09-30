package tmux

import (
	"context"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestEscapeArgvSeparator pins the one escape tmux's argv splitter honours
// (forgectl#823): only a trailing ';' changes, and it becomes `\;`, which
// cmd_parse_from_arguments turns back into exactly the ';' it replaced.
//
// Mutation that turns it red: return s unchanged from escapeArgvSeparator.
func TestEscapeArgvSeparator(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"/repo":   "/repo",
		"a;b":     "a;b",
		"/wd;":    `/wd\;`,
		";":       `\;`,
		`\;`:      `\\;`,
		"x;;":     `x;\;`,
		`find \;`: `find \\;`,
		`a\\;`:    `a\\\;`,
	} {
		if got := escapeArgvSeparator(in); got != want {
			t.Errorf("escapeArgvSeparator(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewWindowWithEnvEscapesTrailingSeparators: the -c directory and every
// command argument after `--` reach tmux escaped, while the name, an -e
// value with no trailing ';' and an ordinary argument are untouched. (An -e
// value ending in ';' is escaped too: TestNewWindowWithEnv_EscapesATrailingSemicolon.)
//
// Mutation that turns it red: append dir or command unescaped in
// NewWindowWithEnv.
func TestNewWindowWithEnvEscapesTrailingSeparators(t *testing.T) {
	fake, c, session := envFixture(t)
	if _, err := c.NewWindowWithEnv(context.Background(), session, "review", "/wd;", []string{"A=1"},
		"sh", "-c", "a; b;", "x"); err != nil {
		t.Fatalf("NewWindowWithEnv: %v", err)
	}
	argsEqual(t, fake.Calls[1].Args, []string{
		"new-window", "-P", "-F", IdentityFormat,
		"-t", "$1:", "-n", "review", "-c", `/wd\;`,
		"-e", "A=1",
		"--", "sh", "-c", `a; b\;`, "x",
	})
}

// TestCreateSessionEscapesTrailingSeparator: CreateSession's -c directory is
// escaped the same way.
//
// Mutation that turns it red: append dir unescaped in CreateSession.
func TestCreateSessionEscapesTrailingSeparator(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		argsEqual(t, args, createArgs("work", `/wd\;`))
		return identityOut("123", "456", "$4"), nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	if _, err := c.CreateSession(context.Background(), "work", "/wd;"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestEscapeDirOperand pins the -c spelling forgectl#839 needs: the format
// escape and the trailing-';' escape together, each alone, and an ordinary
// path unchanged. The real-tmux proof is
// TestStartDirectoryIsNotFormatExpandedIsolated.
//
// Mutation that turns it red: drop either escape from EscapeDirOperand.
func TestEscapeDirOperand(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/dir":        "/plain/dir",
		"/a#(touch m)":      "/a##(touch m)",
		"/a#{session_name}": "/a##{session_name}",
		"/a##b":             "/a####b",
		"/a#[x":             "/a#[x",
		"/semi;":            `/semi\;`,
		"/both#;":           `/both##\;`,
	} {
		if got := EscapeDirOperand(in); got != want {
			t.Errorf("EscapeDirOperand(%q) = %q, want %q", in, got, want)
		}
	}
}
