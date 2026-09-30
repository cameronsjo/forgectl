package cli

// Test plan for y.go
//
// newYCmd / newYCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: `y copy` reads stdin and pipes it into pbcopy via RunWithInput
//   [x] Happy: `y paste` prints pbpaste's stdout, newline-terminated
//   [x] Happy: the `c`/`p` aliases resolve to copy/paste

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestYCopyCmd_ReadsStdin_PipesIntoPbcopy(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := clippkg.New(fake, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)
	cmd.SetIn(strings.NewReader("clip me"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"copy"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := fake.Last()
	if call.Name != "pbcopy" {
		t.Errorf("call.Name = %q, want %q", call.Name, "pbcopy")
	}
	if call.Input != "clip me" {
		t.Errorf("call.Input = %q, want %q", call.Input, "clip me")
	}
}

func TestYPasteCmd_PrintsClipboardContents(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "pbpaste" {
				return "pasted", nil
			}
			return "", nil
		},
	}
	client := clippkg.New(fake, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"paste"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := stdout.String(), "pasted\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestYCmd_AliasesResolveToCanonicalVerb(t *testing.T) {
	client := clippkg.New(&exec.FakeRunner{}, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)

	cases := map[string]string{"c": "copy", "p": "paste"}
	for alias, canonical := range cases {
		found, _, err := cmd.Find([]string{alias})
		if err != nil {
			t.Fatalf("Find(%q): %v", alias, err)
		}
		if found.Name() != canonical {
			t.Errorf("alias %q resolved to %q, want %q", alias, found.Name(), canonical)
		}
	}
}

// TestResolveYPath_QuotesTheMissingPath pins forgectl#855 item 2: the
// operator's path is echoed quoted and capped (QuotePath), not merely
// escaped, so a hostile name is both inert and legible as one path.
//
// Mutation that turns it red: return to termsafe.SafeLine(path) in the stat
// error.
func TestResolveYPath_QuotesTheMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ev\u202eil\u009b31m.png")
	_, err := resolveYPath(missing)
	if err == nil {
		t.Fatal("resolveYPath(missing) = nil, want an error")
	}
	if strings.ContainsAny(err.Error(), "\u202e\u009b") {
		t.Errorf("error carries a raw bidi/control rune: %q", err)
	}
	// The leading echo, not the wrapped stat error's own copy of the path,
	// is the one under test.
	if !strings.HasPrefix(err.Error(), `"`) || !strings.Contains(err.Error(), `ev\u202eil\u009b31m.png": `) {
		t.Errorf("error = %q, want it to open with the quoted path", err)
	}
}
