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
