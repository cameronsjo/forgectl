package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// runTmuxLsJSON runs `tmux ls --json` over fake and returns stdout, stderr,
// and the command's error.
func runTmuxLsJSON(t *testing.T, fake *exec.FakeRunner) (stdout, stderr string, err error) {
	t.Helper()
	client := tmux.New(fake)
	cmd := newTmuxLsCmd(client)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--json"})
	err = cmd.ExecuteContext(context.Background())
	return outBuf.String(), errBuf.String(), err
}

func oneSessionRunner() *exec.FakeRunner {
	const sep = "\x1f"
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return strings.Join([]string{"1", "2", "$0", "forge", "3", "1", "1700000000", "/repo/forge"}, sep), nil
		}
		return "", nil
	}}
}

func TestTmuxLsJSON_KeySet(t *testing.T) {
	out, _, err := runTmuxLsJSON(t, oneSessionRunner())
	if err != nil {
		t.Fatalf("tmux ls --json: %v", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stdout did not parse as a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %s", len(rows), out)
	}
	want := []string{"name", "windows", "attached", "path"}
	if len(rows[0]) != len(want) {
		t.Fatalf("row keys = %v, want exactly %v", keysOf(rows[0]), want)
	}
	for _, k := range want {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("missing key %q; got %v", k, keysOf(rows[0]))
		}
	}
}

func TestTmuxLsJSON_RowsMatchHumanTable(t *testing.T) {
	out, _, err := runTmuxLsJSON(t, oneSessionRunner())
	if err != nil {
		t.Fatalf("tmux ls --json: %v", err)
	}
	var rows []struct {
		Name     string `json:"name"`
		Windows  int    `json:"windows"`
		Attached bool   `json:"attached"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// TestTmuxListingsKeepOrdinaryValuesVerbatim pins the equivalent human row
	// as "●  forge  3 windows  /repo/forge\n" over the same fixture.
	if rows[0].Name != "forge" || rows[0].Windows != 3 || !rows[0].Attached || rows[0].Path != "/repo/forge" {
		t.Errorf("row = %+v, want {forge 3 true /repo/forge}", rows[0])
	}
}

// TestTmuxLsJSON_EmptyIsArrayNeverNull: no tmux sessions must encode as [],
// never null.
func TestTmuxLsJSON_EmptyIsArrayNeverNull(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", nil }}
	out, stderr, err := runTmuxLsJSON(t, fake)
	if err != nil {
		t.Fatalf("tmux ls --json: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("no-sessions --json = %q, want []", out)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on success", stderr)
	}
}

// TestTmuxLsJSON_HostileNamesStayTerminalInert covers the tmux-output
// termsafe requirement for the JSON sink too: a session name or path chosen
// by any same-uid tmux client is untrusted, and the JSON encoder's own
// terminal-escaping must neutralize it exactly as the human table does.
func TestTmuxLsJSON_HostileNamesStayTerminalInert(t *testing.T) {
	const sep = "\x1f"
	h := termsafetest.Hostile
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return strings.Join([]string{"1", "2", "$0", h("work"), "1", "1", "1700000000", h("/tmp/w")}, sep), nil
		}
		return "", nil
	}}
	out, _, err := runTmuxLsJSON(t, fake)
	if err != nil {
		t.Fatalf("tmux ls --json: %v", err)
	}
	if out == "" {
		t.Fatal("hostile listing produced no output; the check would pass vacuously")
	}
	termsafetest.AssertInert(t, "tmux ls --json", out)
}
