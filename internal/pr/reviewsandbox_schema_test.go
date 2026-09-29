package pr

// Test plan: the reviewer's --settings document must be one Claude Code
// ACCEPTS (forgectl#694, round 3).
//
// Claude Code validates a --settings document as a whole, and in -p mode a
// document that fails validation is dropped without a word: permissions,
// sandbox, and disableAllHooks go together, and the reviewer runs
// unconfined. A single wrong-typed key did exactly that once
// (sandbox.network.allowMachLookup emitted as a boolean; it is an array).
// Asserting what the document SAYS cannot catch that; these tests assert it
// would be LOADED.
//
//   [x] every document reviewSettingsJSON emits validates against the
//       vendored Claude Code settings schema (testdata/, header names its
//       source and version); remote and local profiles
//   [x] control: the round-2 shape (allowMachLookup: false) and a typoed
//       key both FAIL that schema, so the check can go red
//   [x] live contract, gated: `claude doctor` reports no invalid setting for
//       the emitted document (FORGECTL_REQUIRE_CLAUDE_CONTRACT=1 makes a
//       missing claude a failure rather than a skip)

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// settingsSchema loads and resolves the vendored schema.
func settingsSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "claude-code-settings.schema.json")))
	if err != nil {
		t.Fatalf("read vendored schema: %v", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse vendored schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve vendored schema: %v", err)
	}
	return resolved
}

// emittedDocuments returns every settings document the dispatch can produce:
// a remote review on each host shape, and a local review.
func emittedDocuments(t *testing.T) map[string]string {
	t.Helper()
	ws := t.TempDir()
	docs := map[string]string{}
	for _, host := range []string{"github.com", "acme.ghe.com", "ghe.corp.example"} {
		perms, err := remoteProfile(host, Ref{Owner: "o", Repo: "r", Number: 42})
		if err != nil {
			t.Fatal(err)
		}
		doc, err := reviewSettingsJSON(ws, host, perms)
		if err != nil {
			t.Fatal(err)
		}
		docs["remote "+host] = doc
	}
	doc, err := reviewSettingsJSON(ws, "", localProfile(filepath.Join(t.TempDir(), "findings")))
	if err != nil {
		t.Fatal(err)
	}
	docs["local"] = doc
	return docs
}

func decodeInstance(t *testing.T, doc string) map[string]any {
	t.Helper()
	var instance map[string]any
	if err := json.Unmarshal([]byte(doc), &instance); err != nil {
		t.Fatalf("settings are not JSON: %v", err)
	}
	return instance
}

func TestReviewSettingsJSON_ValidatesAgainstTheSettingsSchema(t *testing.T) {
	schema := settingsSchema(t)
	for name, doc := range emittedDocuments(t) {
		if err := schema.Validate(decodeInstance(t, doc)); err != nil {
			t.Errorf("%s: the reviewer's --settings document fails the Claude Code settings schema, "+
				"so Claude Code would drop ALL of it (permissions, sandbox, hooks) and run the reviewer unconfined: %v", name, err)
		}
	}
}

// TestSettingsSchema_RejectsTheShapesThatVoidTheDocument is the control: the
// schema check above is only worth having if it goes red on the defect it
// exists for.
func TestSettingsSchema_RejectsTheShapesThatVoidTheDocument(t *testing.T) {
	schema := settingsSchema(t)
	perms, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	good, err := reviewSettingsJSON(t.TempDir(), "github.com", perms)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(doc map[string]any){
		// The round-2 document (fd0f3d2): a boolean where the schema wants
		// an array of service names.
		"allowMachLookup as a boolean": func(doc map[string]any) {
			doc["sandbox"].(map[string]any)["network"].(map[string]any)["allowMachLookup"] = false
		},
		"a typoed sandbox key": func(doc map[string]any) {
			doc["sandbox"].(map[string]any)["failIfUnavailible"] = true
		},
		"a boolean given as a string": func(doc map[string]any) {
			doc["sandbox"].(map[string]any)["enabled"] = "true"
		},
	}
	for name, mutate := range mutations {
		instance := decodeInstance(t, good)
		mutate(instance)
		if err := schema.Validate(instance); err == nil {
			t.Errorf("%s: the schema accepted it, so the schema check could not catch this class", name)
		}
	}
}

// TestReviewSettingsJSON_ClaudeDoctorAcceptsIt asks the installed claude,
// which is the authority the vendored schema only approximates. `claude
// doctor` validates the settings files in its working directory and lists
// any it would reject under "Invalid settings"; a --settings document is
// validated by the same rules, so each emitted document is placed as a
// project settings file in a scratch directory and checked there.
func TestReviewSettingsJSON_ClaudeDoctorAcceptsIt(t *testing.T) {
	required := os.Getenv("FORGECTL_REQUIRE_CLAUDE_CONTRACT") == "1"
	claude, err := osexec.LookPath("claude")
	if err != nil {
		if required {
			t.Fatalf("claude is not on PATH and FORGECTL_REQUIRE_CLAUDE_CONTRACT=1")
		}
		t.Skip("claude is not on PATH; set FORGECTL_REQUIRE_CLAUDE_CONTRACT=1 to make this a failure")
	}
	if !required && testing.Short() {
		t.Skip("-short: the claude contract check starts claude")
	}
	for name, doc := range emittedDocuments(t) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		settingsPath := filepath.Join(dir, ".claude", "settings.json")
		if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		cmd := osexec.CommandContext(ctx, claude, "doctor") //nolint:gosec // G204: the claude binary resolved on PATH, fixed arguments
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CLAUDECODE=")
		out, runErr := cmd.CombinedOutput()
		cancel()
		if runErr != nil {
			t.Fatalf("%s: claude doctor: %v\n%s", name, runErr, out)
		}
		text := string(out)
		if !strings.Contains(text, "Claude Code doctor") {
			t.Fatalf("%s: claude doctor printed nothing recognisable, so this check proves nothing:\n%s", name, text)
		}
		if _, after, found := strings.Cut(text, "Invalid settings"); found {
			block, _, _ := strings.Cut(after, "\n\n")
			if strings.Contains(block, settingsPath) {
				t.Errorf("%s: claude rejects the reviewer's settings document, so it would drop all of it:%s", name, block)
			}
		}
	}
}
