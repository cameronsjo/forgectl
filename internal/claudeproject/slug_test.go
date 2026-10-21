package claudeproject

import "testing"

// TestSlug pins the project-directory encoding against the real names
// observed on disk.
func TestSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/cameron/Projects/claude-configurations/forgectl": "-Users-cameron-Projects-claude-configurations-forgectl",
		"/Users/cameron/.dotfiles":                               "-Users-cameron--dotfiles",
		"/private/tmp/drill-396-proj":                            "-private-tmp-drill-396-proj",
		"/Users/c/Projects/forgectl/.claude/worktrees/fix_it":    "-Users-c-Projects-forgectl--claude-worktrees-fix-it",
	}
	for dir, want := range cases {
		if got := Slug(dir); got != want {
			t.Errorf("Slug(%q) = %q, want %q", dir, got, want)
		}
	}
}
