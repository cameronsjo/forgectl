package worker

import (
	"regexp"
	"testing"

	"github.com/cameronsjo/forgectl/internal/resume"
)

func TestNewSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSessionID()
	if !re.MatchString(a) || a == b {
		t.Fatalf("ids %q, %q: want two distinct lowercase v4 UUIDs", a, b)
	}
}

func TestTranscriptPath(t *testing.T) {
	const dir, id = "/Users/c/Projects/forgectl/.claude/worktrees/fix_it", "x"
	want := "/Users/c/.claude/projects/-Users-c-Projects-forgectl--claude-worktrees-fix-it/x.jsonl"
	if got := TranscriptPath([]string{"HOME=/Users/c"}, dir, id); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := TranscriptPath([]string{"HOME=/Users/c", "CLAUDE_CONFIG_DIR=/cfg", "CLAUDE_CONFIG_DIR=/cfg2"}, dir, id); got != "/cfg2/projects/-Users-c-Projects-forgectl--claude-worktrees-fix-it/x.jsonl" {
		t.Fatalf("CLAUDE_CONFIG_DIR (last wins) not used: %q", got)
	}
	if got := TranscriptPath(nil, dir, id); got != "" {
		t.Fatalf("no HOME and no config dir gave %q", got)
	}
}

// TestProjectSlugMatchesResume keeps the copy in this package equal to
// resume's, which this package may not import.
func TestProjectSlugMatchesResume(t *testing.T) {
	for _, dir := range []string{"/Users/c/Projects/forgectl/.claude/worktrees/fix_it", "/tmp/a b/ü-1", "", "/"} {
		if got, want := projectSlug(dir), resume.ProjectSlug(dir); got != want {
			t.Fatalf("projectSlug(%q) = %q, resume says %q", dir, got, want)
		}
	}
}
