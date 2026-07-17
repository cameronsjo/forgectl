package cli

// Test plan for docs_serve.go
//
// runDocsServe (Classification: API handler — server lifecycle)
//   [x] Happy: context cancellation (Ctrl-C/SIGTERM) triggers a graceful
//       shutdown that returns nil, not an error
//   [x] Happy: --open (openFlag=true) invokes the browser opener with the
//       served URL via the injected Runner (no real browser launched)
//   [x] Unhappy: an invalid bind address returns a wrapped error naming "bind"

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func testCmdWithContext(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(ctx)
	return cmd
}

func testDocsIndex(t *testing.T) *docspkg.Index {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# Hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := docspkg.NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("docspkg.NewIndex: %v", err)
	}
	return idx
}

func TestRunDocsServe_ContextCancel_GracefulShutdown(t *testing.T) {
	idx := testDocsIndex(t)
	deps := module.Deps{Runner: &exec.FakeRunner{}}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", false) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s of context cancellation")
	}
}

func TestRunDocsServe_OpenFlag_InvokesBrowserOpener(t *testing.T) {
	idx := testDocsIndex(t)
	fake := &exec.FakeRunner{}
	deps := module.Deps{Runner: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", true) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s")
	}

	call := fake.Last()
	if call.Name != "open" && call.Name != "xdg-open" {
		t.Fatalf("browser opener call.Name = %q, want open or xdg-open", call.Name)
	}
	if len(call.Args) != 1 || !strings.HasPrefix(call.Args[0], "http://127.0.0.1:") {
		t.Errorf("browser opener args = %v, want a single http://127.0.0.1:<port>/ URL", call.Args)
	}
}

func TestRunDocsServe_InvalidAddr_ErrorsWrapped(t *testing.T) {
	idx := testDocsIndex(t)
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	cmd := testCmdWithContext(context.Background())

	err := runDocsServe(cmd, deps, idx, "not-a-valid-address", false)
	if err == nil {
		t.Fatal("runDocsServe with an invalid address: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("error = %v, want it to name the bind failure", err)
	}
}
