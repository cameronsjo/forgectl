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

func TestDocsBrowse_RejectsNonTTYAndInvalidGraphics(t *testing.T) {
	previous := docsStreamIsTerminal
	t.Cleanup(func() { docsStreamIsTerminal = previous })
	deps := module.Deps{Cfg: config.Config{}, Runner: &forgexec.FakeRunner{}}
	cmd := newDocsBrowseCmd(deps)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	docsStreamIsTerminal = func(any) bool { return false }
	if err := runDocsBrowse(cmd, deps, nil, "auto"); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY error = %v", err)
	}
	docsStreamIsTerminal = func(any) bool { return true }
	if err := runDocsBrowse(cmd, deps, nil, "sixel"); err == nil || !strings.Contains(err.Error(), "invalid graphics mode") {
		t.Fatalf("invalid graphics error = %v", err)
	}
}

func TestDocsCommand_RegistersNativeAndLegacyEntrypoints(t *testing.T) {
	cmd := newDocsCmd(module.Deps{Cfg: config.Config{}, Runner: &forgexec.FakeRunner{}})
	for _, name := range []string{"browse", "serve", "open", "list"} {
		if found, _, err := cmd.Find([]string{name}); err != nil || found.Name() != name {
			t.Fatalf("Find(%q) = %v, %v", name, found, err)
		}
	}
	if cmd.Flag("graphics") == nil {
		t.Fatal("bare docs command lacks --graphics")
	}
}
