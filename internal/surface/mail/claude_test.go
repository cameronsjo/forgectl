//go:build unix

package mail

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInbox reads each connection to EOF, as Claude Code's inbox does after
// the sender half-closes, and reports the lines of every non-empty one.
type fakeInbox struct {
	path   string
	frames chan []string
}

func startInbox(t *testing.T, dir, name string) *fakeInbox {
	t.Helper()
	path := filepath.Join(dir, name)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	in := &fakeInbox{path: path, frames: make(chan []string, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				data, _ := io.ReadAll(c)
				if len(data) == 0 {
					return // a liveness probe
				}
				in.frames <- strings.Split(strings.TrimSpace(string(data)), "\n")
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return in
}

func (in *fakeInbox) next(t *testing.T) []string {
	t.Helper()
	select {
	case lines := <-in.frames:
		return lines
	case <-time.After(3 * time.Second):
		t.Fatal("no frame reached the inbox")
		return nil
	}
}

func writeSession(t *testing.T, config string, pid int, fields map[string]any) {
	t.Helper()
	dir := filepath.Join(config, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDeliverByWorktree(t *testing.T) {
	base := shortTempDir(t)
	config := filepath.Join(base, "cfg")
	work := filepath.Join(base, "wt")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	inbox := startInbox(t, base, "a.sock")
	writeSession(t, config, 4242, map[string]any{
		"pid": 4242, "sessionId": "sess-1", "cwd": work, "name": "w1",
		"status": "busy", "messagingSocketPath": inbox.path,
	})
	// A key file beside the registry entry must not be read or replayed.
	keyName := fmt.Sprintf("4242.%x.key", sha256.Sum256([]byte(inbox.path)))
	if err := os.WriteFile(filepath.Join(config, "sessions", keyName), []byte(`{"peerToken":"tok123"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	a := ClaudeAdapter{ConfigDir: config, Timeout: 2 * time.Second}
	w := Worker{Name: "w1", Harness: HarnessClaude, Worktree: work}
	ctx := context.Background()

	detail, err := a.Deliver(ctx, w, Message{ID: "m-1", Priority: PriorityNow}, "hi there")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "4242") {
		t.Errorf("detail %q", detail)
	}
	lines := inbox.next(t)
	if len(lines) != 1 || strings.Contains(lines[0], "tok123") {
		t.Fatalf("got %q, want the frame alone with no auth line", lines)
	}
	var f claudeFrame
	if err := json.Unmarshal([]byte(lines[0]), &f); err != nil {
		t.Fatal(err)
	}
	want := claudeFrame{MsgV: 1, MsgID: "m-1", Type: "user", Message: claudeContent{Role: "user", Content: "hi there"}, Priority: "now", SessionID: "sess-1"}
	if f != want {
		t.Fatalf("frame %+v, want %+v", f, want)
	}

	st, err := a.State(ctx, w)
	if err != nil || st != StateBusy {
		t.Fatalf("state = %q, %v; want busy", st, err)
	}
}

func TestClaudeAbsentAndStale(t *testing.T) {
	base := shortTempDir(t)
	config := filepath.Join(base, "cfg")
	work := filepath.Join(base, "wt")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	a := ClaudeAdapter{ConfigDir: config, Timeout: time.Second}
	w := Worker{Name: "w1", Harness: HarnessClaude, Worktree: work}
	ctx := context.Background()

	if _, err := a.Deliver(ctx, w, Message{ID: "m"}, "hi"); !IsRetryable(err) {
		t.Fatalf("no registry: err = %v, want retryable", err)
	}
	// A registry entry that outlived its process: nothing listens on the socket.
	writeSession(t, config, 7, map[string]any{"pid": 7, "sessionId": "old", "cwd": work, "name": "w1", "messagingSocketPath": filepath.Join(base, "gone.sock")})
	if _, err := a.Deliver(ctx, w, Message{ID: "m"}, "hi"); !IsRetryable(err) {
		t.Fatalf("stale entry: err = %v, want retryable", err)
	}
	if st, _ := a.State(ctx, w); st != StateAbsent {
		t.Fatalf("stale state = %q, want absent", st)
	}
}

func TestClaudePrefersLaunchName(t *testing.T) {
	base := shortTempDir(t)
	config := filepath.Join(base, "cfg")
	work := filepath.Join(base, "wt")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := startInbox(t, base, "m.sock")
	other := startInbox(t, base, "o.sock")
	writeSession(t, config, 1, map[string]any{"pid": 1, "sessionId": "s1", "cwd": work, "name": "someone-else", "messagingSocketPath": other.path})
	writeSession(t, config, 2, map[string]any{"pid": 2, "sessionId": "s2", "cwd": work, "name": "w1", "messagingSocketPath": mine.path})

	a := ClaudeAdapter{ConfigDir: config, Timeout: 2 * time.Second}
	if _, err := a.Deliver(context.Background(), Worker{Name: "w1", Worktree: work}, Message{ID: "m"}, "hi"); err != nil {
		t.Fatal(err)
	}
	lines := mine.next(t)
	if len(lines) != 1 || !strings.Contains(lines[0], `"session_id":"s2"`) {
		t.Fatalf("frame %q", lines)
	}

	// Neither live session is w9's: it may not have registered yet, so the
	// message waits rather than going to whichever session is there.
	_, err := a.Deliver(context.Background(), Worker{Name: "w9", Worktree: work}, Message{ID: "m"}, "hi")
	if !IsRetryable(err) {
		t.Fatalf("two live sessions, neither named w9: err = %v, want retryable", err)
	}

	// Two live sessions sharing the worker's name cannot be told apart.
	dup := startInbox(t, base, "d.sock")
	writeSession(t, config, 3, map[string]any{"pid": 3, "sessionId": "s3", "cwd": work, "name": "w1", "messagingSocketPath": dup.path})
	_, err = a.Deliver(context.Background(), Worker{Name: "w1", Worktree: work}, Message{ID: "m"}, "hi")
	if err == nil || IsRetryable(err) {
		t.Fatalf("two live sessions named w1: err = %v, want a permanent error", err)
	}
}

// An operator's own session open in the worker's worktree is the only live
// one there; it must not receive the worker's mail.
func TestClaudeIgnoresLoneUnnamedSession(t *testing.T) {
	base := shortTempDir(t)
	config := filepath.Join(base, "cfg")
	work := filepath.Join(base, "wt")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := startInbox(t, base, "op.sock")
	writeSession(t, config, 9, map[string]any{"pid": 9, "sessionId": "op", "cwd": work, "name": "cheerful-otter", "messagingSocketPath": operator.path})
	a := ClaudeAdapter{ConfigDir: config, Timeout: time.Second}
	_, err := a.Deliver(context.Background(), Worker{Name: "w1", Worktree: work}, Message{ID: "m"}, "hi")
	if !IsRetryable(err) {
		t.Fatalf("err = %v, want retryable until w1 registers", err)
	}
	select {
	case lines := <-operator.frames:
		t.Fatalf("the operator's session got %q", lines)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClaudeKnownSocket(t *testing.T) {
	base := shortTempDir(t)
	inbox := startInbox(t, base, "c.sock")
	a := ClaudeAdapter{ConfigDir: filepath.Join(base, "cfg"), Timeout: 2 * time.Second}
	coord := Worker{Name: "coord", Coordinator: true, Socket: inbox.path}
	if _, err := a.Deliver(context.Background(), coord, Message{ID: "m", Priority: PriorityNext}, "hi"); err != nil {
		t.Fatal(err)
	}
	lines := inbox.next(t)
	if len(lines) != 1 || strings.Contains(lines[0], "session_id") {
		t.Fatalf("frame without a registry entry should carry no session id: %q", lines)
	}
}
