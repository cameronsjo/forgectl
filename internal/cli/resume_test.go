package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/resume"
)

// hostileSession carries a name and prompt loaded with the control bytes a
// terminal acts on. Both are disk-sourced and neither is trusted markup: the
// name is whatever was typed at /rename, and the prompt is whatever was pasted
// into a session — including content that arrived from a web page or a repo.
func hostileSession() resume.Session {
	return resume.Session{
		ID:         "aaaaaaaa-0000-0000-0000-000000000001",
		Name:       "evil\x1b[2K\rok\x9bAname",
		Repo:       "repo\x07",
		Branch:     "main\x1b[31m",
		Cwd:        "/work/repo\x0c",
		LastPrompt: "line one\rline two\x7f",
		LastActive: time.Now(),
	}
}

// TestPrintSessions_SanitizesText asserts the table path renders control bytes
// inert — a forged row or an overwritten line is the failure being prevented.
func TestPrintSessions_SanitizesText(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := printSessions(&out, &errOut, []resume.Session{hostileSession()}, false); err != nil {
		t.Fatalf("printSessions: %v", err)
	}
	assertInert(t, out.String())
}

// TestPrintSessions_SanitizesJSON is the half that is easy to miss:
// encoding/json escapes only 0x00–0x1F, so DEL (0x7f) and the C1 range
// (0x80–0x9F, including 0x9B = single-byte CSI) pass through a JSON encoder
// untouched and reach the terminal raw. --json output lands in a terminal as
// often as the table does.
func TestPrintSessions_SanitizesJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := printSessions(&out, &errOut, []resume.Session{hostileSession()}, true); err != nil {
		t.Fatalf("printSessions --json: %v", err)
	}

	// The raw encoded bytes must be inert...
	assertInert(t, out.String())

	// ...and so must the DECODED values, since that is what a consumer
	// pipes onward. A  escape in the wire form is still a CSI byte
	// once anything decodes it.
	var got []sessionDTO
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode --json output: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d records, want 1", len(got))
	}
	for field, value := range map[string]string{
		"name": got[0].Name, "repo": got[0].Repo, "branch": got[0].Branch,
		"cwd": got[0].Cwd, "last_prompt": got[0].LastPrompt,
	} {
		for _, r := range value {
			if r != '\t' && unicode.IsControl(r) {
				t.Errorf("decoded %s = %q still carries control rune %U", field, value, r)
			}
		}
	}
}

// assertInert fails if s carries any control rune other than the newlines and
// tabs the renderer itself writes.
func assertInert(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			t.Errorf("rendered output carries control rune %U: %q", r, s)
			return
		}
	}
}

// TestSessionRow_LabelsLivenessAndTasks checks the two annotations that decide
// what the operator does with a row: a live session cannot be resumed, and a
// task count is the reason to prefer one row over another.
func TestSessionRow_LabelsLivenessAndTasks(t *testing.T) {
	live := resume.Session{ID: "id", Name: "running", Live: true, Pid: 4242, LastActive: time.Now()}
	if got := sessionRow(live); !strings.Contains(got, "pid 4242") {
		t.Errorf("live row = %q, want it to name the pid", got)
	}

	withTasks := resume.Session{ID: "id", Name: "parked", LastActive: time.Now(), Tasks: []resume.Task{{ID: "1"}, {ID: "2"}}}
	if got := sessionRow(withTasks); !strings.Contains(got, "2 tasks") {
		t.Errorf("row with tasks = %q, want the task count", got)
	}
}

// TestPrintSessions_EmptyIsCalm checks the cold-start path says so plainly
// rather than printing an empty table.
func TestPrintSessions_EmptyIsCalm(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := printSessions(&out, &errOut, nil, false); err != nil {
		t.Fatalf("printSessions: %v", err)
	}
	if !strings.Contains(out.String(), "no recent sessions") {
		t.Errorf("empty output = %q, want a plain no-sessions line", out.String())
	}
}

// TestRelativeTime covers the picker's "is this the one I was just in?" column.
func TestRelativeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := relativeTime(c.at); got != c.want {
			t.Errorf("relativeTime(%v) = %q, want %q", c.at, got, c.want)
		}
	}
	if got := relativeTime(time.Time{}); got != "unknown" {
		t.Errorf("relativeTime(zero) = %q, want %q", got, "unknown")
	}
}

// TestResumeSnapshot_AlwaysSucceeds pins the property that makes the Stop hook
// safe: the capture verb never returns an error, whatever it finds, because a
// failed snapshot must not become a failed turn.
func TestResumeSnapshot_AlwaysSucceeds(t *testing.T) {
	cmd := newResumeSnapshotCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--quiet"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume snapshot --quiet returned %v, want nil — it runs on a hook", err)
	}
	if out.Len() != 0 {
		t.Errorf("--quiet wrote %q to stdout, want nothing", out.String())
	}
}

// TestTruncate checks the column clip marks itself rather than silently
// dropping characters.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q, want it untouched", got)
	}
	got := truncate("a-very-long-session-name", 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %q, want 10 runes ending in an ellipsis", got)
	}
}
