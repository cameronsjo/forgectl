package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

func TestNativeRealReplyWithHarmlessBroker(t *testing.T) {
	if os.Getenv("FORGECTL_NATIVE_REPLY_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_NATIVE_REPLY_INTEGRATION=1 for bounded real Pi and Claude model turns")
	}
	for _, harness := range []string{"pi", "claude"} {
		t.Run(harness, func(t *testing.T) {
			binary, err := exec.LookPath(harness)
			if err != nil {
				t.Fatal(err)
			}
			supervisor := os.Getenv("FORGECTL_NATIVE_SUPERVISOR")
			if !filepath.IsAbs(supervisor) {
				t.Fatal("FORGECTL_NATIVE_SUPERVISOR must name the absolute current source-build executable")
			}
			root, err := os.MkdirTemp("/private/tmp", "fg-native-reply-")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("native reply artifacts: %s", root)
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0o700); err != nil {
				t.Fatal(err)
			}
			socket, capability := filepath.Join(root, "tools.sock"), filepath.Join(root, "capability")
			var calls atomic.Int32
			broker, err := goaltools.Start(t.Context(), socket, capability, func(_ context.Context, operation string, arguments json.RawMessage) (any, error) {
				if operation != "goal_inbox" || string(arguments) != "{}" {
					return nil, errors.New("this probe permits only goal_inbox with an empty object and executes no commands")
				}
				calls.Add(1)
				return map[string]any{"messages": []string{}, "probe": "no execution"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = broker.Close(context.Background()) }()
			var events []NativeEvent
			var eventMutex sync.Mutex
			config := NativeConfig{Binary: binary, SupervisorBinary: supervisor, ControlDir: filepath.Join(root, "control"), Cwd: work, Socket: filepath.Join(root, "native.sock"), Environment: os.Environ(), ToolSocket: socket, ToolCapabilityFile: capability, OnEvent: func(event NativeEvent) {
				eventMutex.Lock()
				defer eventMutex.Unlock()
				events = append(events, event)
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var session NativeSession
			if harness == "pi" {
				config.Provider, config.Model = "openai-codex", "gpt-6.1-sol"
				session, err = StartPi(ctx, config)
			} else {
				config.Provider, config.Model = "anthropic", "claude-opus-5-5"
				session, err = StartClaude(ctx, config)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				if err := session.Close(cleanup); err != nil {
					t.Errorf("native reply cleanup: %v", err)
				}
			}()
			prompt, err := NewPrompt("This is a harmless native protocol acceptance probe. Call goal_inbox exactly once with {}. Do not call any other tool. After its successful response, reply with exactly FORGECTL_NATIVE_REPLY_PROBE_OK.")
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Submit(ctx, prompt); err != nil {
				t.Fatal(err)
			}
			result, replyErr := session.Wait(ctx, prompt.ID)
			eventMutex.Lock()
			captured := append([]NativeEvent(nil), events...)
			eventMutex.Unlock()
			if err := writePrivateJSON(filepath.Join(root, "normalized.json"), captured); err != nil {
				t.Fatal(err)
			}
			if replyErr != nil || result.Status != "completed" || result.ID == "" || strings.TrimSpace(result.Text) != "FORGECTL_NATIVE_REPLY_PROBE_OK" || calls.Load() != 1 {
				t.Fatalf("native reply = %#v, calls=%d, error=%v", result, calls.Load(), replyErr)
			}
			seen := make(map[string]bool)
			for _, event := range captured {
				if event.RequestID == prompt.ID && event.Identity == session.Identity() {
					seen[event.Status] = true
				}
			}
			for _, required := range []string{"accepted", "consumed", "completed"} {
				if !seen[required] {
					t.Errorf("native reply omitted %s evidence", required)
				}
			}
		})
	}
}
