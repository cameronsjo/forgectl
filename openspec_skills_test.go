package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openspecPinnedMarker is what scripts/openspec-localize.sh writes under each
// generated skill's frontmatter.
const openspecPinnedMarker = "<!-- forgectl: pinned openspec CLI -->"

// TestOpenSpecSkillsUsePinnedCLI fails when a generated OpenSpec skill or
// /opsx command still pre-allows a bare `openspec`, which would run an
// unpinned CLI from PATH with no prompt. `openspec update` regenerates these
// files, so the fix is to rerun scripts/openspec-localize.sh (#1305).
func TestOpenSpecSkillsUsePinnedCLI(t *testing.T) {
	var files []string
	for _, pattern := range []string{
		".claude/skills/openspec-*/SKILL.md",
		".claude/commands/opsx/*.md",
		".agents/skills/openspec-*/SKILL.md",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("no generated OpenSpec skills found; the guard matched nothing")
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if strings.Contains(body, "Bash(openspec:") || !strings.Contains(body, openspecPinnedMarker) {
			t.Errorf("%s: not pointed at the pinned CLI; run scripts/openspec-localize.sh", f)
		}
	}
}
