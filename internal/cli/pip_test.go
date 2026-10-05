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

// pip accepts several URLs on one extra-index-url line, and each can carry
// its own token. Every one must be hidden, not only the first.
func TestPipShow_JSON_HidesEveryURLOnOneLine(t *testing.T) {
	const s1, s2 = "tok-first-482", "tok-second-482"
	path := filepath.Join(t.TempDir(), "pip.conf")
	body := "[global]\nextra-index-url = https://u:" + s1 + "@a.example/simple https://u:" + s2 + "@b.example/simple?k=" + s2 + "\n" //nolint:gosec // G101: fake credentials the output must hide
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stdout, _ := runPipShow(t, path, "--json")
	for _, secret := range []string{s1, s2} {
		if strings.Contains(stdout, secret) {
			t.Errorf("--json leaked %q:\n%s", secret, stdout)
		}
	}
	var got pipShowJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	want := "https://[userinfo hidden]@a.example/simple https://[userinfo hidden]@b.example/simple?[query hidden]"
	if len(got.Entries) != 1 || got.Entries[0].Value != want {
		t.Errorf("Entries = %+v, want one value %q", got.Entries, want)
	}
}

// pip splits list values with Python's str.split(), so every separator it
// honours must split redaction too, or the second URL's token leaks.
func TestRedactPipValue_EverySeparatorPipSplitsOn(t *testing.T) {
	const secret = "tok-sep-482" //nolint:gosec // G101: a fake credential the output must hide
	for _, sep := range []string{" ", "\t", "\v", "\f", "\u0085", " ", " ", "　", "\x1c", "\x1f"} {
		in := "https://a.example/simple" + sep + "https://u:" + secret + "@b.example/simple" //nolint:gosec // G101: fake credential
		got := redactPipValue(in)
		if strings.Contains(got, secret) {
			t.Errorf("separator %q: leaked %q", sep, got)
		}
		if !strings.Contains(got, sep) {
			t.Errorf("separator %q not preserved: %q", sep, got)
		}
	}
}

// A password with an unencoded "#" or "/" defeats URL parsing, so the token
// is hidden whole rather than printed.
func TestRedactPipValue_UnparseableUserinfoHiddenWhole(t *testing.T) {
	for _, in := range []string{"https://u:p#tok-h-482@h/simple", "https://u:pa/tok-s-482@h/simple"} { //nolint:gosec // G101: fake credential
		if got := redactPipValue(in); strings.Contains(got, "tok-") {
			t.Errorf("redactPipValue(%q) = %q, leaked", in, got)
		}
	}
}

func TestPipPath_JSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pip.conf")
	client := pippkg.New(&exec.FakeRunner{}, pippkg.WithConfigPath(path))
	cmd := newPipCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"path", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pip path --json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got["path"] != path {
		t.Errorf("path = %q, want %q", got["path"], path)
	}
}
