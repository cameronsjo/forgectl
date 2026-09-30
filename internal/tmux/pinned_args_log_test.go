package tmux

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestPinnedArgsRefusalNeverLogsTheArgv is forgectl#775 item 1. A refused
// argv can carry `new-window -e KEY=VALUE`, and the refusal's Debug line must
// not render it.
//
// Mutation that turns it red: log "argv", args again in either refusal.
func TestPinnedArgsRefusalNeverLogsTheArgv(t *testing.T) {
	const secret = "hunter2hunter2" //nolint:gosec // G101: a fake value the log must not carry
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := pinnedClient(t, &internalexec.FakeRunner{})
	for _, args := range [][]string{
		{"new-window", "-e", "PLAIN=" + secret},
		{"-S", testSocket, "new-window", "-e", "PLAIN=" + secret, "-S", "/tmp/other"},
	} {
		_ = c.serverStateError(context.Background(), args, commandFailure("tmux", args, "no server running"))
	}
	logs := buf.String()
	if !strings.Contains(logs, "Refusing argv this pinned client did not build.") ||
		!strings.Contains(logs, "Refusing argv naming a second socket after the pin.") {
		t.Fatalf("both refusals must log (the test would pass vacuously otherwise); got:\n%s", logs)
	}
	if strings.Contains(logs, secret) {
		t.Fatalf("a refused argv reached the debug log:\n%s", logs)
	}
}
