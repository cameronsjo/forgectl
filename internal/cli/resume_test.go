package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/config"
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

// TestResumeSession_RefusesLiveButNotWithFork pins the contract the error
// message advertises. The refusal names the pid; --fork is a real escape from
// it, not a suggestion — a fork only reads the transcript, so it does not
// contend with the running session.
func TestResumeSession_RefusesLiveButNotWithFork(t *testing.T) {
	// Cwd is empty so nothing can reach the exec: the live refusal is
	// deliberately raised BEFORE the cwd check, so an already-running session
	// is reported as such whether or not its directory still exists.
	live := resume.Session{
		ID: "aaaaaaaa-0000-0000-0000-000000000001", Name: "still-running",
		Live: true, Pid: 4242,
	}
	cmd := newResumeSnapshotCmd() // any command — only its out/err streams are used
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := resumeSession(cmd, config.Config{}, live, false, false)
	if err == nil || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("continuing a live session returned %v, want a refusal naming pid 4242", err)
	}
	if !strings.Contains(err.Error(), "still-running") {
		t.Errorf("refusal = %v, want the session's name as well as its id", err)
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("live refusal exit code = %d, want 2 — it is recoverable, unlike a no-match", code)
	}

	// With --fork the liveness refusal must not fire at all; the empty cwd is
	// what stops it, which proves execution got past the live check.
	err = resumeSession(cmd, config.Config{}, live, true, false)
	if err == nil {
		t.Fatal("expected the empty-cwd error, got nil")
	}
	if strings.Contains(err.Error(), "4242") {
		t.Errorf("--fork still hit the liveness refusal (%v) — the help text promises it does not", err)
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Errorf("expected the empty-cwd error past the live check, got %v", err)
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

// pinResumePaths points the resume verb at a throwaway tree for one test.
//
// `resume snapshot` WRITES, so without this every `go test` run would capture
// the developer's own live sessions into their real forgectl store.
func pinResumePaths(t *testing.T) resume.Paths {
	t.Helper()
	root := t.TempDir()
	p := resume.Paths{
		ClaudeHome: filepath.Join(root, ".claude"),
		StoreDir:   filepath.Join(root, "store"),
	}
	prev := resumePaths
	resumePaths = func() (resume.Paths, error) { return p, nil }
	t.Cleanup(func() { resumePaths = prev })
	return p
}

// TestResumeSnapshot_AlwaysSucceeds pins the property that makes the Stop hook
// safe: the capture verb never returns an error, whatever it finds, because a
// failed snapshot must not become a failed turn. Here it finds an empty tree,
// which is the harshest version of "whatever it finds".
func TestResumeSnapshot_AlwaysSucceeds(t *testing.T) {
	pinResumePaths(t)
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

// TestResumeSnapshot_StaysInsideItsPaths is the regression guard for the seam
// itself: an earlier version of this test resolved the real paths and wrote
// live snapshots into the developer's own store.
func TestResumeSnapshot_StaysInsideItsPaths(t *testing.T) {
	p := pinResumePaths(t)
	cmd := newResumeSnapshotCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--quiet"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume snapshot: %v", err)
	}
	entries, err := os.ReadDir(p.StoreDir)
	if err == nil && len(entries) != 0 {
		t.Errorf("snapshot over an empty ~/.claude wrote %d store file(s), want 0", len(entries))
	}
}

// TestResumeLs_EmptyTreeIsClean checks the cold-start path end to end through
// the command, not just the renderer.
func TestResumeLs_EmptyTreeIsClean(t *testing.T) {
	pinResumePaths(t)
	cmd := newResumeLsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume ls --json on an empty tree: %v", err)
	}
	var got []sessionDTO
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON (%v): %q", err, out.String())
	}
	if len(got) != 0 {
		t.Errorf("listed %d sessions from an empty tree, want 0", len(got))
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

// TestCell_WidthIsRunesNotBytes is the regression guard for a real
// misalignment: fmt's %-Ns pads by BYTES, so the three-byte ellipsis that
// truncate appends overran the column on every clipped row.
func TestCell_WidthIsRunesNotBytes(t *testing.T) {
	cases := []string{
		"short",                    // padded
		"exactly-ten",              // near the boundary
		"a-very-long-session-name", // clipped, gains an ellipsis
		"süßölküche-lane-name",     // multi-byte, clipped
	}
	for _, in := range cases {
		got := cell(in, 12)
		if n := len([]rune(got)); n != 12 {
			t.Errorf("cell(%q, 12) = %q — %d runes, want exactly 12", in, got, n)
		}
	}
}

// TestLayoutFor_SizesToContentAndTerminal covers the two directions the old
// fixed widths got wrong at once: clipping names that would fit on a wide
// terminal, while padding the branch column to 18 so a list of "main" spent a
// quarter of the row on whitespace.
func TestLayoutFor_SizesToContentAndTerminal(t *testing.T) {
	sessions := []resume.Session{
		{Repo: "forgectl", Branch: "main"},
		{Repo: "claude-configurations", Branch: "main"},
	}

	t.Run("non-terminal keeps stable widths", func(t *testing.T) {
		if got := layoutFor(sessions, 0); got != defaultLayout {
			t.Errorf("layoutFor(width 0) = %+v, want the fixed default %+v — piped output must not reflow", got, defaultLayout)
		}
	})

	t.Run("branch column shrinks to its content", func(t *testing.T) {
		got := layoutFor(sessions, 160)
		if got.branch > minBranchCol+1 {
			t.Errorf("branch column = %d for content %q; it should not pad to the old fixed 18", got.branch, "main")
		}
	})

	t.Run("name column takes the slack on a wide terminal", func(t *testing.T) {
		narrow := layoutFor(sessions, 80)
		wide := layoutFor(sessions, 200)
		if wide.name <= narrow.name {
			t.Errorf("name column did not grow with the terminal: 80→%d, 200→%d", narrow.name, wide.name)
		}
		if wide.name > maxNameCol {
			t.Errorf("name column = %d, want it capped at %d", wide.name, maxNameCol)
		}
	})

	t.Run("floors hold on a very narrow terminal", func(t *testing.T) {
		got := layoutFor(sessions, 20)
		if got.name < minNameCol || got.repo < minRepoCol || got.branch < minBranchCol {
			t.Errorf("layout %+v fell below its floors on a 20-column terminal", got)
		}
	})

	t.Run("a full row fits the terminal it was sized for", func(t *testing.T) {
		// tailWidth is a reservation, not a measurement — if it undercounts,
		// the widest real row wraps instead of clipping. Check the actual
		// worst case: a month-old live session, whose time column is a full
		// date AND which carries the longest annotation.
		worst := resume.Session{
			Name: strings.Repeat("n", 100), Repo: strings.Repeat("r", 100),
			Branch: strings.Repeat("b", 100), Live: true, Pid: 123456,
			LastActive: time.Now().Add(-90 * 24 * time.Hour),
		}
		for _, width := range []int{80, 100, 120, 160, 200} {
			l := layoutFor([]resume.Session{worst}, width)
			if n := len([]rune(sessionRowWidth(worst, l))); n > width {
				t.Errorf("at %d columns the widest row rendered %d runes — it will wrap", width, n)
			}
		}
	})

	t.Run("a long repo does not starve the name", func(t *testing.T) {
		long := []resume.Session{{Repo: strings.Repeat("r", 120), Branch: strings.Repeat("b", 120)}}
		got := layoutFor(long, 120)
		if got.repo > maxRepoCol || got.branch > maxBranchCol {
			t.Errorf("layout %+v exceeded its caps", got)
		}
		if got.name < minNameCol {
			t.Errorf("name column starved to %d by long repo/branch values", got.name)
		}
	})
}

// TestSessionRowWidth_RespectsLayout checks the renderer actually honors the
// computed widths rather than the constants it used to hardcode.
func TestSessionRowWidth_RespectsLayout(t *testing.T) {
	s := resume.Session{Name: "n", Repo: "r", Branch: "b", LastActive: time.Now()}
	narrow := sessionRowWidth(s, rowLayout{name: 10, repo: 6, branch: 4})
	wide := sessionRowWidth(s, rowLayout{name: 40, repo: 20, branch: 12})
	if len(narrow) >= len(wide) {
		t.Errorf("row did not widen with the layout: narrow=%q wide=%q", narrow, wide)
	}
	if !strings.HasPrefix(narrow, "n         ") {
		t.Errorf("narrow row = %q, want the name padded to 10", narrow)
	}
}

// TestCell_MeasuresTerminalCellsNotRunes covers the alignment defect a rune
// count reintroduces one layer below the fixed widths this branch removed: a
// CJK ideograph or an emoji occupies TWO terminal cells, so a name carrying
// them overflowed its column and shoved every column after it out of line.
// Session names are arbitrary text from /rename or a model-generated ai-title,
// so this is ordinary input, not an exotic one.
func TestCell_MeasuresTerminalCellsNotRunes(t *testing.T) {
	cases := []struct{ name, in string }{
		{"ascii", "session"},
		{"cjk", "会議のセッション"},
		{"emoji", "ship 🚀 it"},
		{"combining", "café"},             // precomposed
		{"combining-decomposed", "café"}, // e + combining acute
		{"mixed", "repo-会議-🚀"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, width := range []int{4, 8, 12, 20} {
				got := cell(c.in, width)
				if w := ansi.StringWidth(got); w != width {
					t.Errorf("cell(%q, %d) occupies %d cells, want exactly %d (got %q)", c.in, width, w, width, got)
				}
			}
		})
	}
}

// TestSessionRowWidth_WideCharsKeepColumnsAligned is the property that matters
// downstream: two rows rendered at one layout line up, whatever is in them.
func TestSessionRowWidth_WideCharsKeepColumnsAligned(t *testing.T) {
	l := rowLayout{name: 20, repo: 12, branch: 8}
	ascii := sessionRowWidth(resume.Session{Name: "plain", Repo: "repo", Branch: "main", LastActive: time.Now()}, l)
	wide := sessionRowWidth(resume.Session{Name: "会議のセッション", Repo: "リポ", Branch: "main", LastActive: time.Now()}, l)

	// The time column starts at the same cell in both rows.
	prefix := l.name + 1 + l.repo + 1 + l.branch + 1
	if a, b := ansi.StringWidth(ascii[:len(ascii)-len("just now")]), ansi.StringWidth(wide[:len(wide)-len("just now")]); a != b || a != prefix {
		t.Errorf("column prefixes differ: ascii=%d wide=%d, want both %d\n  %q\n  %q", a, b, prefix, ascii, wide)
	}
}

// TestWriterWidth_NonTerminalIsZero pins the fallback: a buffer, pipe, or file
// must report 0 so the layout stays fixed, even when the process's own stdout
// is a terminal.
func TestWriterWidth_NonTerminalIsZero(t *testing.T) {
	if got := writerWidth(&bytes.Buffer{}); got != 0 {
		t.Errorf("writerWidth(buffer) = %d, want 0", got)
	}
	f, err := os.CreateTemp(t.TempDir(), "w")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got := writerWidth(f); got != 0 {
		t.Errorf("writerWidth(file) = %d, want 0", got)
	}
}
