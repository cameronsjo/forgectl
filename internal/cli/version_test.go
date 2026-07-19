package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestVersionCmd_PrintsRootVersion(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.Version = "1.2.3 (abcdef0)"
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute() error = %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "forgectl version 1.2.3 (abcdef0)"
	if got != want {
		t.Errorf("version command output = %q, want %q", got, want)
	}
}
