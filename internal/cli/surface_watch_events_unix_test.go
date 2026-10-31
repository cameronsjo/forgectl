//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWatchEventsChildIsReapedThroughTheProductionChain: the watch's event
// stream as surface watch builds it (productionWatchHC: the hc adapter's
// StreamEvents through hcWatchSource) leaves no hc events child once the
// subscription's context is canceled and Done has closed. A stub hc on PATH
// records its pid, prints a snapshot and one event, then idles.
func TestWatchEventsChildIsReapedThroughTheProductionChain(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	stub := "#!/bin/sh\necho $$ > " + pidFile + "\necho '{\"terminals\":[]}'\necho '{\"params\":{\"type\":\"status\"}}'\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(stub), 0o700); err != nil { //nolint:gosec // G306: a test stub that must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HC_SESSION", "")

	c, err := productionWatchHC()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Source.(hcWatchSource); !ok {
		t.Fatalf("source is %T, want the adapter-backed hcWatchSource", c.Source)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es := c.Events(ctx)
	select {
	case <-es.Wake:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub hc events never woke the watch")
	}
	raw, err := os.ReadFile(pidFile) //nolint:gosec // G304: the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-es.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after cancel")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the hc events child (pid %d) is still alive after Done: kill(0) = %v", pid, err)
	}
}
