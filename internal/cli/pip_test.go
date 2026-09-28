package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	pippkg "github.com/cameronsjo/forgectl/internal/pip"
)

// runPipShow executes `pip show <args…>` against a client pointed at path.
func runPipShow(t *testing.T, path string, args ...string) (string, string) {
	t.Helper()
	client := pippkg.New(&exec.FakeRunner{}, pippkg.WithConfigPath(path))
	cmd := newPipCmdForClient(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"show"}, args...))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pip show %v: %v", args, err)
	}
	return stdout.String(), stderr.String()
}

// TestPipShow_JSON pins `pip show --json` (#482): valid JSON with the
// path/entries field set, active entries only (comments and removed entries
// excluded), continuation lines joined, and a registry credential or query
// token in an index URL hidden.
func TestPipShow_JSON(t *testing.T) {
	const secret = "tok-482-secret"
	path := filepath.Join(t.TempDir(), "pip.conf")
	body := "# a comment\n[global]\nindex-url = https://__token__:" + secret + "@pypi.example/simple\n" + //nolint:gosec // G101: a fake credential the output must hide
		"extra-index-url =\n    https://a.example/simple?token=" + secret + "\n    https://b.example/simple\n" +
		"# forgectl-pip:removed trusted-host = old.example\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stdout, stderr := runPipShow(t, path, "--json")
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if strings.Contains(stdout, secret) {
		t.Fatalf("--json leaked the index credential: %s", stdout)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &fields); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if len(fields) != 2 || fields["path"] == nil || fields["entries"] == nil {
		t.Errorf("field set = %v, want exactly path and entries", fields)
	}
	var got pipShowJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Path != path {
		t.Errorf("Path = %q, want %q", got.Path, path)
	}
	want := []pipEntryJSON{
		{Section: "global", Key: "index-url", Value: "https://[userinfo hidden]@pypi.example/simple"},
		{Section: "global", Key: "extra-index-url", Value: "https://a.example/simple?[query hidden]\nhttps://b.example/simple"},
	}
	if len(got.Entries) != len(want) {
		t.Fatalf("Entries = %+v, want %+v", got.Entries, want)
	}
	for i := range want {
		if got.Entries[i] != want[i] {
			t.Errorf("Entries[%d] = %+v, want %+v", i, got.Entries[i], want[i])
		}
	}
}

// A missing pip.conf reads as empty (pip.Client.Read's contract), so --json
// reports entries as [], never null.
func TestPipShow_JSON_MissingFile_EmptyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pip.conf")
	stdout, _ := runPipShow(t, path, "--json")
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if string(got["entries"]) != "[]" {
		t.Errorf("entries = %s, want []", got["entries"])
	}
}

// The human path is unchanged: the file's bytes, verbatim.
func TestPipShow_Human_Verbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pip.conf")
	body := "[global]\nindex-url = https://pypi.example/simple\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if stdout, _ := runPipShow(t, path); stdout != body {
		t.Errorf("stdout = %q, want %q", stdout, body)
	}
}
