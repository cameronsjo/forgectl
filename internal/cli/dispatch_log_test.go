package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/module"
)

// The dispatch debug used to record raw argv. That is fine for `forgectl tmux
// list` and not fine for anything carrying a value forgectl did not compose —
// which is why forgectl#181's surface work replaced it with a canonical verb
// plus a count, and why these tests hold the replacement in place. A regression
// to `"verb", args` reintroduces the leak silently: the log still looks
// plausible, and no other test reads it.

// captureDispatchLog runs fn with a debug-level slog default writing into a
// buffer, and returns what was logged.
func captureDispatchLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	fn()
	return buf.String()
}

// TestLogDispatch_NeverRecordsRawArgv is the anti-leak pin. The tokens below
// stand in for the shapes that must never reach a log line: a rendezvous nonce,
// a private socket path, and a prompt.
func TestLogDispatch_NeverRecordsRawArgv(t *testing.T) {
	root := newRoot(module.Deps{})

	secrets := []string{
		"beefcafe0123456789abcdef0123456789abcdef0123456789abcdef0beefcaf",
		"/tmp/forgectl-surface-abc/sock",
		"summarize the incident report",
	}

	argvs := [][]string{
		{"surface", "--socket", secrets[1], "--nonce", secrets[0]},
		{"launch", "-p", secrets[2]},
		{secrets[0]},
		{"tmux", "list", secrets[1]},
	}

	for _, argv := range argvs {
		got := captureDispatchLog(t, func() { logDispatch("Dispatching to command verb.", root, argv) })

		if got == "" {
			t.Fatalf("argv %q logged nothing; this test would pass vacuously", argv)
		}
		for _, secret := range secrets {
			if strings.Contains(got, secret) {
				t.Errorf("argv %q leaked %q into the dispatch log:\n%s", argv, secret, got)
			}
		}
		if !strings.Contains(got, "argc=") {
			t.Errorf("argv %q logged no argument count:\n%s", argv, got)
		}
	}
}

// TestCanonicalVerb_ResolvesRegisteredCommandsOnly pins the value the log
// records. The rule is that a token can only appear if it is already a command
// name compiled into the binary — so an unrecognized token is reported as the
// literal category, never echoed.
func TestCanonicalVerb_ResolvesRegisteredCommandsOnly(t *testing.T) {
	root := newRoot(module.Deps{})

	if len(root.Commands()) == 0 {
		t.Fatal("the root command has no subcommands; every row below would resolve to unknown vacuously")
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"registered verb", []string{"launch", "--model", "opus"}, "launch"},
		{"alias resolves to the canonical name", []string{"cl"}, "launch"},
		{"lazily-registered builtin", []string{"help"}, "help"},
		{"unknown token is categorized", []string{"beefcafe-not-a-command"}, unknownVerb},
		{"flags only", []string{"--version"}, unknownVerb},
		{"empty argv", nil, unknownVerb},
		{"flag before the verb", []string{"--no-icons", "launch"}, "launch"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalVerb(root, tc.args); got != tc.want {
				t.Errorf("canonicalVerb(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestCanonicalVerb_NeverReturnsAnUnregisteredToken is the generalized form of
// the row above: whatever it returns must be a name the binary already ships,
// or the fixed category. A table can only cover the tokens someone thought of;
// this covers the shape.
func TestCanonicalVerb_NeverReturnsAnUnregisteredToken(t *testing.T) {
	root := newRoot(module.Deps{})

	registered := map[string]bool{unknownVerb: true}
	for _, c := range root.Commands() {
		registered[c.Name()] = true
	}
	for v := range builtinVerbs {
		registered[v] = true
	}

	hostile := [][]string{
		{"beefcafe0123456789abcdef"},
		{"/tmp/forgectl-surface-abc/sock"},
		{"launch;rm -rf /"},
		{"\x1b[31mred"},
		{"--nonce", "deadbeef"},
		{""},
	}

	for _, argv := range hostile {
		if got := canonicalVerb(root, argv); !registered[got] {
			t.Errorf("canonicalVerb(%q) = %q, which is neither a registered command nor %q", argv, got, unknownVerb)
		}
	}
}
