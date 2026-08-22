package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	runnerexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/sessions"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// invisibleFormatting is the residual #281 names: the runes the retired
// termsafe.Sanitize passed through untouched because they are neither Cc nor
// Bidi_Control, yet are non-graphic and so within SafeLine's quoting set.
// U+2028 and U+2029 are the sharp ones — a renderer that treats either as a
// line break splits the "one inert physical line" SafeLine documents.
//
// Every entry is a code point, never a literal character in this source: a test
// file that authors its own invisible formatting is a test file whose own diff
// cannot be read.
var invisibleFormatting = []unsafeRune{
	{name: "LINE SEPARATOR", r: 0x2028},
	{name: "PARAGRAPH SEPARATOR", r: 0x2029},
	{name: "ZERO WIDTH SPACE", r: 0x200b},
	{name: "SOFT HYPHEN", r: 0x00ad},
	{name: "WORD JOINER", r: 0x2060},
}

type unsafeRune struct {
	name string
	r    rune
}

// humanSink renders one human-output boundary given a hostile value and
// returns exactly what an operator's terminal would receive. Keeping the sinks
// in one table makes every hostile-rune class apply to every boundary.
type humanSink struct {
	name   string
	render func(t *testing.T, value string) string
}

func convergedHumanSinks() []humanSink {
	return []humanSink{
		{
			name: "resume-ls-text-row",
			render: func(t *testing.T, value string) string {
				t.Helper()
				var out, errOut bytes.Buffer
				list := []resume.Session{{ID: "sess-1", Cwd: value, Name: value, LastActive: time.Unix(0, 0)}}
				if err := printSessions(&out, &errOut, list, false); err != nil {
					t.Fatalf("printSessions: %v", err)
				}
				return out.String()
			},
		},
		{
			name: "docs-token-file-path",
			render: func(t *testing.T, value string) string {
				t.Helper()
				_, err := acquireDocsTokenFile("relative" + value + "/token")
				if err == nil {
					t.Fatal("a relative token path must be refused")
				}
				return err.Error()
			},
		},
		{
			name: "external-command-exec-failure",
			render: func(t *testing.T, value string) string {
				t.Helper()
				root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
				var stderr bytes.Buffer
				runtime := externalCommandRuntime{
					lookPath: func(string) (string, error) {
						return filepath.Join(t.TempDir(), "forgectl-frobnicate"), nil
					},
					exec:    func(string, []string, []string) error { return errString("exec refused " + value) },
					environ: func() []string { return nil },
					stderr:  &stderr,
				}
				if handled, _ := tryExtensionRungs(root, []string{"frobnicate"}, runtime); !handled {
					t.Fatal("exec failure must be handled")
				}
				return stderr.String()
			},
		},
		{
			name: "sessions-search-text-row",
			render: func(t *testing.T, value string) string {
				t.Helper()
				var out bytes.Buffer
				hit := sessions.SearchHit{
					Path: value, Title: value, Type: value,
					Project: value, Machine: value, Snippet: value,
				}
				if err := printSearchHits(&out, []sessions.SearchHit{hit}); err != nil {
					t.Fatalf("printSearchHits: %v", err)
				}
				return out.String()
			},
		},
		{
			name: "sessions-last-human-header",
			render: func(t *testing.T, value string) string {
				t.Helper()
				summary := &sessions.SessionSummary{
					SessionID: value, Project: value, GitBranch: value,
				}
				out, _ := renderCmd(t, func(cmd *cobra.Command) error {
					return printLastSession(cmd, "repo", summary, false)
				})
				return out
			},
		},
	}
}

// errString is a minimal error whose text is exactly the string, so the sink
// under test receives the hostile bytes rather than a wrapper's rendering.
type errString string

func (e errString) Error() string { return string(e) }

// TestConvergedHumanSinks_QuoteInvisibleFormatting is #281's acceptance
// criterion, one subtest per sink per rune: every sink must render the residual
// inertly rather than passing it to the terminal.
func TestConvergedHumanSinks_QuoteInvisibleFormatting(t *testing.T) {
	for _, sink := range convergedHumanSinks() {
		for _, tt := range invisibleFormatting {
			t.Run(sink.name+"/"+tt.name, func(t *testing.T) {
				got := sink.render(t, "before"+string(tt.r)+"after")
				assertValueQuoted(t, sink.name, got, tt.r)
				if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
					t.Fatalf("%s dropped the surrounding text: %q", sink.name, got)
				}
			})
		}
	}
}

// assertValueQuoted checks the hostile rune did not survive INSIDE the rendered
// value, matched by its "before"/"after" bracket rather than by a bare rune
// scan. The distinction is load-bearing for tab and newline: a row-shaped sink
// emits both as its own layout, so a bare scan would fail a correctly-escaped
// render — a check that goes red on working code, which is the shape that costs
// the most to diagnose.
func assertValueQuoted(t *testing.T, sink, got string, r rune) {
	t.Helper()
	if strings.Contains(got, "before"+string(r)+"after") {
		t.Fatalf("%s passed %U through to the terminal inside the value: %q", sink, r, got)
	}
	if r != '\t' && r != '\n' && r != '\r' && strings.ContainsRune(got, r) {
		t.Fatalf("%s passed %U through to the terminal: %q", sink, r, got)
	}
}

// TestConvergedHumanSinks_QuoteEveryUnsafeRuneClass checks the classes the old
// primitive did handle are still handled after the move — a convergence that
// traded one gap for another would otherwise read as a pass. Tab is in the set
// deliberately: Sanitize passed it through by design, so a tab in a session
// name could shift a fixed-width column; SafeLine escapes it.
func TestConvergedHumanSinks_QuoteEveryUnsafeRuneClass(t *testing.T) {
	classes := []unsafeRune{
		{name: "ESC", r: 0x1b},
		{name: "CSI-C1", r: 0x9b},
		{name: "DEL", r: 0x7f},
		{name: "RIGHT-TO-LEFT-OVERRIDE", r: 0x202e},
		{name: "TAB", r: 0x09},
		{name: "LINE-FEED", r: 0x0a},
	}
	for _, sink := range convergedHumanSinks() {
		for _, tt := range classes {
			t.Run(sink.name+"/"+tt.name, func(t *testing.T) {
				assertValueQuoted(t, sink.name, sink.render(t, "before"+string(tt.r)+"after"), tt.r)
			})
		}
	}
}

// TestResumeLsJSON_EscapesRatherThanRewrites pins the boundary the text sinks
// must NOT be extended across. --json is a machine contract, so its control is
// the value-preserving termsafe.JSONEncoder rather than SafeLine: the operator's
// stored bytes come back byte-identical through a decode, which is exactly what
// the retired Sanitize did not do — it rewrote every control byte to a space and
// handed a corrupted value to whatever parsed it.
//
// The escaping half of the assertion covers the classes termsafe classifies as
// unsafe. It deliberately does NOT cover the non-bidi Cf residual (U+200B,
// U+00AD, U+2060): the JSON filter's scope is IsUnsafeTerminalRune, closing that
// residual there would mean broadening the shared classifier, and the text
// renderer's wider quoting set is a rendering choice it does not export. That
// divergence is documented on termsafe.JSONEncoder and pinned by
// TestJSONEncoder_EscapesEveryEscapableRune.
func TestResumeLsJSON_EscapesRatherThanRewrites(t *testing.T) {
	escaped := []unsafeRune{
		{name: "LINE-SEPARATOR", r: 0x2028},
		{name: "PARAGRAPH-SEPARATOR", r: 0x2029},
		{name: "ESC", r: 0x1b},
		{name: "CSI-C1", r: 0x9b},
		{name: "DEL", r: 0x7f},
		{name: "RIGHT-TO-LEFT-OVERRIDE", r: 0x202e},
	}
	for _, tt := range append(append([]unsafeRune(nil), invisibleFormatting...), escaped...) {
		t.Run(tt.name, func(t *testing.T) {
			value := "before" + string(tt.r) + "after"
			var out, errOut bytes.Buffer
			list := []resume.Session{{ID: "sess-1", Cwd: value, LastActive: time.Unix(0, 0)}}
			if err := printSessions(&out, &errOut, list, true); err != nil {
				t.Fatalf("printSessions: %v", err)
			}
			var decoded []sessionDTO
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatalf("decode --json: %v", err)
			}
			if len(decoded) != 1 {
				t.Fatalf("decoded %d records, want 1", len(decoded))
			}
			if decoded[0].Cwd != value {
				t.Fatalf("--json rewrote the stored value: got %q, want %q", decoded[0].Cwd, value)
			}
			for _, e := range escaped {
				if e.r == tt.r && strings.ContainsRune(out.String(), tt.r) {
					t.Fatalf("--json passed %U through to the terminal: %q", tt.r, out.String())
				}
			}
		})
	}
}

// TestWorkflowStatus_RendersEveryValueInertly is #291: the status report
// interpolates an argv name, four sidecar fields, and two note lines, and the
// sidecar is the file ADR-0006's threat model treats as attacker-writable.
func TestWorkflowStatus_RendersEveryValueInertly(t *testing.T) {
	const rlo = rune(0x202e)
	hostileName := "multi" + string(rlo) + "evil"
	cases := append(append([]unsafeRune(nil), invisibleFormatting...),
		unsafeRune{name: "RIGHT-TO-LEFT-OVERRIDE", r: rlo},
		unsafeRune{name: "ESC", r: 0x1b},
	)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			hostile := "before" + string(tt.r) + "after"
			dir := cliRedirectConfigDir(t)
			path := cliWriteUserWorkflow(t, dir, hostileName, []byte(cliMultiWorkflow))
			swapVerifier(t, fakeVerifier{})
			if _, err := execRun(t, failOn("step-two"), hostileName); err == nil {
				t.Fatal("seed run should fail at step-two")
			}

			// Rewrite the sidecar with hostile field values, then edit the
			// definition so the "has changed" note fires on the same render.
			state, ok, err := workflow.LoadState(hostileName)
			if err != nil || !ok {
				t.Fatalf("seed state: ok=%t err=%v", ok, err)
			}
			state.RunID = hostile
			state.StartedAt = hostile
			state.UpdatedAt = hostile
			for i := range state.Steps {
				state.Steps[i].Uses = hostile
			}
			if err := workflow.WriteState(state); err != nil {
				t.Fatalf("rewrite state: %v", err)
			}
			edited := strings.Replace(cliMultiWorkflow, `version = "1.0.0"`, `version = "2.0.0"`, 1)
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatalf("rewrite workflow: %v", err)
			}

			body := runWorkflowStatus(t, hostileName)
			if !strings.Contains(body, "has changed") {
				t.Fatalf("expected the definition-changed note:\n%s", body)
			}
			assertInertStatusBody(t, body, tt.r, rlo)

			// The second note line: with the definition gone, status reports the
			// load error instead. Same render, different branch.
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove workflow: %v", err)
			}
			body = runWorkflowStatus(t, hostileName)
			if !strings.Contains(body, "could not load") {
				t.Fatalf("expected the load-error note:\n%s", body)
			}
			assertInertStatusBody(t, body, tt.r, rlo)
		})
	}
}

func runWorkflowStatus(t *testing.T, name string) string {
	t.Helper()
	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{name})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	return out.String()
}

func assertInertStatusBody(t *testing.T, body string, unsafe ...rune) {
	t.Helper()
	for _, r := range unsafe {
		if strings.ContainsRune(body, r) {
			t.Fatalf("workflow status passed %U through to the terminal:\n%q", r, body)
		}
	}
}
