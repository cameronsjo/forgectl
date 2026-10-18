//go:build unix

package tui

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

type GoalPreset struct {
	ID           string
	Name         string
	Hint         string
	AllowedPaths []string
	Acceptance   []string
	Checks       [][]string
	CheckNote    string
}

func projectGoalPresets(repository string) []GoalPreset {
	checks := [][]string{{"git", "diff", "--check"}}
	checkNote := "Only a whitespace check is suggested. Add behavior checks in Advanced."
	docPaths := []string{"docs", "README.md"}
	if root, err := os.OpenRoot(repository); err == nil {
		defer func() { _ = root.Close() }()
		docPaths = projectGoalDocPaths(root)
		if command := projectGoalTestCommand(root); len(command) > 0 {
			checks = append(checks, command)
			checkNote = "Suggested repository test command; review the exact commands before Start."
		}
	}
	return []GoalPreset{
		{
			ID: "bugfix", Name: "Bug fix", Hint: "What is broken, and what should happen instead?",
			AllowedPaths: []string{"."},
			Acceptance:   []string{"The reported behavior is corrected without unrelated changes.", "A focused regression check covers the changed behavior."},
			Checks:       cloneProjectGoalChecks(checks), CheckNote: checkNote,
		},
		{
			ID: "feature", Name: "Feature", Hint: "What should someone be able to do when this is finished?",
			AllowedPaths: []string{"."},
			Acceptance:   []string{"The requested behavior is implemented and independently reviewed.", "Relevant checks cover the new behavior and existing behavior remains intact."},
			Checks:       cloneProjectGoalChecks(checks), CheckNote: checkNote,
		},
		{
			ID: "docs", Name: "Docs", Hint: "What needs explaining, and who is the reader?",
			AllowedPaths: docPaths,
			Acceptance:   []string{"Documentation matches the current implementation and uses reproducible examples.", "Changes remain limited to the approved documentation paths."},
			Checks:       [][]string{{"git", "diff", "--check"}},
			CheckNote:    "Whitespace check plus independent documentation review; no behavior tests are implied.",
		},
		{
			ID: "custom", Name: "Custom", Hint: "Describe the outcome; set your own boundaries in Advanced.",
			AllowedPaths: []string{"."},
			Acceptance:   []string{"The requested objective is implemented and independently reviewed."},
			Checks:       [][]string{{"git", "diff", "--check"}},
			CheckNote:    "Your explicit scope and checks are preserved. Review them before Start.",
		},
	}
}

func cloneProjectGoalChecks(checks [][]string) [][]string {
	cloned := make([][]string, len(checks))
	for index, command := range checks {
		cloned[index] = append([]string(nil), command...)
	}
	return cloned
}

func projectGoalDocPaths(root *os.Root) []string {
	paths := []string{"docs"}
	for _, name := range []string{"README.md", "README.rst", "README"} {
		if projectGoalPresetFile(root, name) {
			paths = append(paths, name)
		}
	}
	if len(paths) == 1 {
		paths = append(paths, "README.md")
	}
	return paths
}

func projectGoalTestCommand(root *os.Root) []string {
	if projectGoalPresetFile(root, "go.mod") {
		return []string{"go", "test", "./..."}
	}
	if projectGoalPresetFile(root, "Cargo.toml") {
		return []string{"cargo", "test", "--locked"}
	}
	data, ok := projectGoalPresetJSON(root, "package.json")
	if !ok {
		return nil
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil || strings.TrimSpace(manifest.Scripts["test"]) == "" {
		return nil
	}
	for _, candidate := range []struct{ file, manager string }{
		{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"},
	} {
		if projectGoalPresetFile(root, candidate.file) {
			return []string{candidate.manager, "run", "test"}
		}
	}
	return []string{"npm", "run", "test"}
}

func projectGoalPresetFile(root *os.Root, name string) bool {
	info, err := root.Lstat(name)
	return err == nil && info.Mode().IsRegular()
}

func projectGoalPresetJSON(root *os.Root, name string) ([]byte, bool) {
	const limit = 64 * 1024
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, false
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) > limit {
		return nil, false
	}
	return data, true
}
