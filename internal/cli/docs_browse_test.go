package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestDocsCommand_NonTTYBareInvocationKeepsHelpBehavior(t *testing.T) {
	previous := docsStreamIsTerminal
	docsStreamIsTerminal = func(any) bool { return false }
	t.Cleanup(func() { docsStreamIsTerminal = previous })

	cmd := newDocsCmd(module.Deps{Cfg: config.Config{}, Runner: &forgexec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "browse") || !strings.Contains(out.String(), "serve") {
		t.Fatalf("help = %q", out.String())
	}
}

func TestDocsPreview_RejectsNonTTY(t *testing.T) {
	previous := docsStreamIsTerminal
	t.Cleanup(func() { docsStreamIsTerminal = previous })
	deps := module.Deps{Cfg: config.Config{}, Runner: &forgexec.FakeRunner{}}
	cmd := newDocsCmd(deps)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	docsStreamIsTerminal = func(any) bool { return false }
	if err := runDocsPreview(cmd, deps, nil); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY error = %v", err)
	}
}

func TestDocsCommand_RegistersPreviewAndServerEntrypoints(t *testing.T) {
	cmd := newDocsCmd(module.Deps{Cfg: config.Config{}, Runner: &forgexec.FakeRunner{}})
	for _, name := range []string{"serve", "open", "list"} {
		if found, _, err := cmd.Find([]string{name}); err != nil || found.Name() != name {
			t.Fatalf("Find(%q) = %v, %v", name, found, err)
		}
	}
	if found, _, err := cmd.Find([]string{"browse"}); err == nil && found.Name() == "browse" {
		t.Fatal("obsolete terminal browse command is still registered")
	}
}
