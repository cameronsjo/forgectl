package cli

import (
	"bytes"
	"encoding/json"
	"errors"
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

// TestPrintSessions_SanitizesJSON checks the real selected-session DTO path.
// Non-tab Cc and Bidi_Control runes become spaces in decoded values and never
// survive literally in the raw stream; join controls and variation selectors
// remain unchanged, and the stable schema still decodes into sessionDTO.
func TestPrintSessions_SanitizesJSON(t *testing.T) {
	rlo := string(rune(0x202e))
	zwnj := string(rune(0x200c))
	zwj := string(rune(0x200d))
	variation := "✈" + string(rune(0xfe0f))
	lastActive := time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC)
	fixture := resume.Session{
		ID:         "id\x7f",
		Name:       "left" + rlo + "right",
		NameSource: "user" + zwnj + "named",
		Repo:       "emoji" + zwj + "join",
		Branch:     "main",
		Cwd:        "/work/" + variation,
		LastPrompt: "prompt" + string(rune(0x9b)),
		LastActive: lastActive,
		Version:    "1.2.3",
		Live:       true,
		Pid:        4242,
		Tasks:      []resume.Task{{ID: "one"}, {ID: "two"}},
	}

	var out, errOut bytes.Buffer
	if err := printSessions(&out, &errOut, []resume.Session{fixture}, true); err != nil {
		t.Fatalf("printSessions --json: %v", err)
	}

	// The raw encoded bytes must be inert...
	assertInert(t, out.String())
	for _, r := range out.String() {
		if unicode.In(r, unicode.Bidi_Control) {
			t.Errorf("raw --json output carries fixture bidi control %U: %q", r, out.String())
		}
	}

	// ...and so must the DECODED values, since that is what a consumer
	// pipes onward. A C1 escape in the wire form is still a CSI byte
	// once anything decodes it.
	var got []sessionDTO
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode --json output: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d records, want 1", len(got))
	}
	want := sessionDTO{
		ID:         "id ",
		Name:       "left right",
		NameSource: "user" + zwnj + "named",
		Repo:       "emoji" + zwj + "join",
		Branch:     "main",
		Cwd:        "/work/" + variation,
		LastActive: lastActive.Format(time.RFC3339),
		LastPrompt: "prompt ",
		Version:    "1.2.3",
		Live:       true,
		Pid:        4242,
		Tasks:      2,
	}
	if got[0] != want {
		t.Errorf("decoded selected-session schema/value mismatch:\n got: %+v\nwant: %+v", got[0], want)
	}
	for field, value := range map[string]string{
		"id": got[0].ID, "name": got[0].Name, "name_source": got[0].NameSource,
		"repo": got[0].Repo, "branch": got[0].Branch, "cwd": got[0].Cwd,
		"last_prompt": got[0].LastPrompt, "version": got[0].Version,
	} {
		for _, r := range value {
			if r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)) {
				t.Errorf("decoded %s = %q still carries unsafe terminal rune %U", field, value, r)
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

// pinScan points runResume's scan at a fixed session list for one test.
// internal/resume's fixture builder is unexported, so this seam is the only
// way a cli-layer test can produce the two-or-more sessions the ambiguous
// path exists to handle.
func pinScan(t *testing.T, sessions ...resume.Session) {
	t.Helper()
	prev := scanSessions
	scanSessions = func(string, int) ([]resume.Session, error) { return sessions, nil }
	t.Cleanup(func() { scanSessions = prev })
}

// pinPick stubs the interactive picker and reports how many times it was
// reached.
//
// No test may let the REAL picker run. huh opens /dev/tty directly, and that
// device exists whenever `go test` is started from a terminal — even with
// stdout piped — so an unstubbed picker hangs on a developer's machine and
// fails only in CI. The stub is what makes "the picker was not reached" a
// deterministic assertion rather than a timing one.
func pinPick(t *testing.T, choose func([]resume.Session) (resume.Session, error)) *int {
	t.Helper()
	calls := 0
	prev := pickSessionFn
	pickSessionFn = func(sessions []resume.Session) (resume.Session, error) {
		calls++
		return choose(sessions)
	}
	t.Cleanup(func() { pickSessionFn = prev })
	return &calls
}

// explodingPick stands in for the picker on a run that must never reach it,
// returning the very error the defect produced: huh's /dev/tty failure.
func explodingPick([]resume.Session) (resume.Session, error) {
	return resume.Session{}, errors.New("could not open a new TTY: open /dev/tty: device not configured")
}

// pinTTY forces the interactive-TTY gate for one test.
func pinTTY(t *testing.T, interactive bool) {
	t.Helper()
	prev := isInteractiveTTY
	isInteractiveTTY = func() bool { return interactive }
	t.Cleanup(func() { isInteractiveTTY = prev })
}

// ambiguousFixture is three matching sessions — the shape that used to open a
// picker. Cwd is deliberately empty so nothing can reach an exec.
func ambiguousFixture() []resume.Session {
	now := time.Now()
	return []resume.Session{
		{ID: "aaaaaaaa-0000-0000-0000-000000000001", Name: "first-anvil", Repo: "cc", Branch: "main", LastActive: now},
		{ID: "aaaaaaaa-0000-0000-0000-000000000002", Name: "second-anvil", Repo: "cc", Branch: "main", LastActive: now.Add(-time.Hour)},
		{ID: "aaaaaaaa-0000-0000-0000-000000000003", Name: "third-anvil", Repo: "cc", Branch: "main", LastActive: now.Add(-2 * time.Hour)},
	}
}

// assertCandidateList checks the headless answer to an ambiguous filter: every
// candidate on stdout, one per line, and an actionable exit-1 error that does
// not leak the picker's internals.
func assertCandidateList(t *testing.T, out string, err error, sessions []resume.Session) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(sessions) {
		t.Fatalf("stdout had %d line(s), want %d — one per candidate:\n%s", len(lines), len(sessions), out)
	}
	for i, s := range sessions {
		if !strings.Contains(lines[i], s.Name) {
			t.Errorf("stdout line %d = %q, want it to name %q", i, lines[i], s.Name)
		}
	}
	if err == nil {
		t.Fatal("an ambiguous filter returned nil — the caller cannot tell it resolved nothing")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ambiguous exit code = %d, want 1 — it recovers the same way a no-match does", code)
	}
	if !strings.Contains(err.Error(), "narrow") {
		t.Errorf("error = %v, want it to say how to recover (narrow the filter)", err)
	}
	if strings.Contains(err.Error(), "/dev/tty") {
		t.Errorf("error = %v, still leaks huh's terminal failure instead of naming the caller's problem", err)
	}
}

// TestRunResume_AmbiguousDryRunPrintsCandidatesNotPicker is the issue itself:
// --dry-run declares non-interactive intent, so an ambiguous filter must list
// what it could not choose between rather than opening a selector. The TTY is
// pinned LIVE here on purpose — the flag has to suppress the prompt even when
// a terminal is sitting right there.
func TestRunResume_AmbiguousDryRunPrintsCandidatesNotPicker(t *testing.T) {
	sessions := ambiguousFixture()
	pinScan(t, sessions...)
	calls := pinPick(t, explodingPick)
	pinTTY(t, true)

	cmd, out, _ := newTestCmd()
	err := runResume(cmd, config.Config{}, "cc", 0, false, true)

	if *calls != 0 {
		t.Errorf("the picker was reached %d time(s) under --dry-run", *calls)
	}
	assertCandidateList(t, out.String(), err, sessions)
}

// TestRunResume_AmbiguousWithoutTTYPrintsCandidates covers the second, quieter
// half of the same defect: a BARE ambiguous resume in a pipe crashed
// identically, because the picker had no TTY gate at all.
func TestRunResume_AmbiguousWithoutTTYPrintsCandidates(t *testing.T) {
	sessions := ambiguousFixture()
	pinScan(t, sessions...)
	calls := pinPick(t, explodingPick)
	pinTTY(t, false)

	cmd, out, _ := newTestCmd()
	err := runResume(cmd, config.Config{}, "cc", 0, false, false)

	if *calls != 0 {
		t.Errorf("the picker was reached %d time(s) with no terminal to draw on", *calls)
	}
	assertCandidateList(t, out.String(), err, sessions)
}

// TestRunResume_AmbiguousSanitizesCandidates covers what the candidate list
// newly is: a terminal-output sink for disk-sourced text. Every name, repo,
// and branch in it came off disk — a /rename string or a model-generated
// ai-title — so the same control bytes `resume ls` has always had to render
// inert reach the terminal through this path too, and it is the path a
// non-interactive caller sees.
func TestRunResume_AmbiguousSanitizesCandidates(t *testing.T) {
	hostile := hostileSession()
	second := hostile
	second.ID = "aaaaaaaa-0000-0000-0000-000000000002"
	pinScan(t, hostile, second)
	calls := pinPick(t, explodingPick)
	pinTTY(t, false)

	cmd, out, _ := newTestCmd()
	if err := runResume(cmd, config.Config{}, "cc", 0, false, true); err == nil {
		t.Fatal("an ambiguous filter returned nil")
	}
	// Both assertions are load-bearing. assertInert on an empty string passes
	// vacuously, so a regression that reached explodingPick instead of
	// rendering would look clean here: the picker count is what proves the
	// candidate list is the thing being asserted inert.
	if *calls != 0 {
		t.Errorf("the picker was reached %d time(s) with no terminal to draw on", *calls)
	}
	if out.Len() == 0 {
		t.Fatal("no candidate list was rendered, so there is nothing to assert inert")
	}
	assertInert(t, out.String())
}

// TestRunResume_AmbiguousOnTTYStillPicks pins what must NOT change: with a
// human and a terminal, the picker is still the right answer, and its choice
// is what gets resumed. The fixture's empty Cwd stops execution at the
// recorded-directory error, which is how we know the pick reached
// resumeSession.
func TestRunResume_AmbiguousOnTTYStillPicks(t *testing.T) {
	sessions := ambiguousFixture()
	pinScan(t, sessions...)
	calls := pinPick(t, func(s []resume.Session) (resume.Session, error) { return s[2], nil })
	pinTTY(t, true)

	cmd, out, _ := newTestCmd()
	err := runResume(cmd, config.Config{}, "cc", 0, false, false)

	if *calls != 1 {
		t.Fatalf("the picker ran %d time(s), want exactly 1 — an interactive ambiguous resume still prompts", *calls)
	}
	if out.Len() != 0 {
		t.Errorf("the interactive path wrote %q to stdout, want nothing", out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("err = %v, want the empty-cwd error proving the pick reached resumeSession", err)
	}
	if !strings.Contains(err.Error(), sessions[2].ID) {
		t.Errorf("err = %v, want it to name the PICKED session %s", err, sessions[2].ID)
	}
}

// TestRunResume_ForkDryRunAmbiguousSkipsPicker checks --fork inherits the
// guard rather than routing around it: the gate sits in runResume, upstream of
// everything --fork changes.
func TestRunResume_ForkDryRunAmbiguousSkipsPicker(t *testing.T) {
	sessions := ambiguousFixture()
	pinScan(t, sessions...)
	calls := pinPick(t, explodingPick)
	pinTTY(t, true)

	cmd, out, _ := newTestCmd()
	err := runResume(cmd, config.Config{}, "cc", 0, true, true)

	if *calls != 0 {
		t.Errorf("--fork --dry-run reached the picker %d time(s)", *calls)
	}
	assertCandidateList(t, out.String(), err, sessions)
}

// TestRunResume_SingleMatchDryRunUnchanged is the regression pin on the path
// that already worked: one hit still resolves, announces itself on stderr, and
// exits 0 without any picker involved.
func TestRunResume_SingleMatchDryRunUnchanged(t *testing.T) {
	fakeClaudeBin(t)
	only := resume.Session{
		ID: "aaaaaaaa-0000-0000-0000-00000000000f", Name: "lone-anvil",
		Repo: "cc", Branch: "main", Cwd: t.TempDir(), LastActive: time.Now(),
	}
	pinScan(t, only)
	calls := pinPick(t, explodingPick)
	pinTTY(t, false)

	cmd, out, errOut := newTestCmd()
	if err := runResume(cmd, config.Config{}, "cc", 0, false, true); err != nil {
		t.Fatalf("single-match --dry-run returned %v, want nil", err)
	}
	if *calls != 0 {
		t.Errorf("a single match reached the picker %d time(s)", *calls)
	}
	if !strings.Contains(errOut.String(), "one match —") {
		t.Errorf("stderr = %q, want the one-match announcement", errOut.String())
	}
	if !strings.Contains(out.String(), only.Cwd) {
		t.Errorf("stdout = %q, want the resolved cwd", out.String())
	}
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
