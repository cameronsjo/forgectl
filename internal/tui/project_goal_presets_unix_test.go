//go:build unix

package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectGoalPresetsSuggestFixedCommandsWithoutExecution(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		files   map[string]string
		command []string
	}{
		{name: "unknown"},
		{name: "go", files: map[string]string{"go.mod": "module example.test/app\n"}, command: []string{"go", "test", "./..."}},
		{name: "rust", files: map[string]string{"Cargo.toml": "[package]\nname='app'\n"}, command: []string{"cargo", "test", "--locked"}},
		{name: "npm", files: map[string]string{"package.json": `{"scripts":{"test":"touch should-not-exist"}}`}, command: []string{"npm", "run", "test"}},
		{name: "pnpm", files: map[string]string{"package.json": `{"scripts":{"test":"test-runner"}}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'"}, command: []string{"pnpm", "run", "test"}},
		{name: "yarn", files: map[string]string{"package.json": `{"scripts":{"test":"test-runner"}}`, "yarn.lock": ""}, command: []string{"yarn", "run", "test"}},
		{name: "bun", files: map[string]string{"package.json": `{"scripts":{"test":"test-runner"}}`, "bun.lock": ""}, command: []string{"bun", "run", "test"}},
		{name: "no test", files: map[string]string{"package.json": `{"scripts":{"build":"builder"}}`}},
		{name: "invalid", files: map[string]string{"package.json": `not JSON`}},
		{name: "wrong type", files: map[string]string{"package.json": `{"scripts":{"test":["malicious"]}}`}},
		{name: "oversized", files: map[string]string{"package.json": strings.Repeat(" ", 64*1024+1)}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			directory := test.TempDir()
			for name, content := range fixture.files {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
					test.Fatal(err)
				}
			}
			presets := projectGoalPresets(directory)
			if len(presets) != 4 || presets[0].ID != "bugfix" || presets[1].ID != "feature" || presets[2].ID != "docs" || presets[3].ID != "custom" {
				test.Fatalf("unexpected presets: %#v", presets)
			}
			expected := [][]string{{"git", "diff", "--check"}}
			if fixture.command != nil {
				expected = append(expected, fixture.command)
			}
			for _, preset := range presets[:2] {
				if !reflect.DeepEqual(preset.Checks, expected) {
					test.Fatalf("%s checks=%#v want %#v", preset.Name, preset.Checks, expected)
				}
				if fixture.command == nil && !strings.Contains(preset.CheckNote, "Only a whitespace check") {
					test.Fatalf("missing fallback warning: %s", preset.CheckNote)
				}
			}
			if len(presets[2].Checks) != 1 || len(presets[3].Checks) != 1 {
				test.Fatal("documentation or custom preset inherited behavior checks")
			}
		})
	}
}

func TestProjectGoalPresetDetectionDoesNotExecuteRunners(test *testing.T) {
	directory := test.TempDir()
	marker := filepath.Join(directory, "execution-marker")
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"git", "go", "cargo", "npm", "pnpm", "yarn", "bun"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf ran > \"$FORGECTL_PRESET_EXECUTION_MARKER\"\n"), 0o700); err != nil {
			test.Fatal(err)
		}
	}
	test.Setenv("PATH", bin)
	test.Setenv("FORGECTL_PRESET_EXECUTION_MARKER", marker)
	if err := exec.CommandContext(test.Context(), filepath.Join(bin, "npm"), "run", "test").Run(); err != nil {
		test.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		test.Fatalf("execution control did not engage: %q %v", data, err)
	}
	if err := os.Remove(marker); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"package.json", "go.mod", "Cargo.toml"} {
		repository := test.TempDir()
		if err := os.WriteFile(filepath.Join(repository, name), []byte(`{"scripts":{"test":"would-run-a-script"}}`), 0o600); err != nil {
			test.Fatal(err)
		}
		if len(projectGoalPresets(repository)[0].Checks) != 2 {
			test.Fatalf("test runner not detected from %s", name)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		test.Fatalf("preset detection executed a runner or marker check failed: %v", err)
	}
}

func TestProjectGoalPresetsRejectSymlinkedManifestsAndIsolateEdits(test *testing.T) {
	directory := test.TempDir()
	outside := filepath.Join(test.TempDir(), "package.json")
	if err := os.WriteFile(outside, []byte(`{"scripts":{"test":"outside-runner"}}`), 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "package.json")); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "go.mod")); err != nil {
		test.Fatal(err)
	}
	presets := projectGoalPresets(directory)
	if len(presets[0].Checks) != 1 {
		test.Fatal("symlinked manifest was treated as a test configuration")
	}
	presets[0].Checks[0][0] = "modified"
	presets[0].AllowedPaths[0] = "modified"
	presets[0].Acceptance[0] = "modified"
	if presets[1].Checks[0][0] != "git" || presets[1].AllowedPaths[0] != "." || presets[1].Acceptance[0] == "modified" {
		test.Fatal("editing one preset changed another")
	}
	if projectGoalPresets(directory)[0].Checks[0][0] != "git" {
		test.Fatal("editing a preset changed a later suggestion")
	}
}

func TestProjectGoalDocsPresetUsesOnlyDocumentationPaths(test *testing.T) {
	directory := test.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "README.rst"), []byte("Documentation"), 0o600); err != nil {
		test.Fatal(err)
	}
	preset := projectGoalPresets(directory)[2]
	if !reflect.DeepEqual(preset.AllowedPaths, []string{"docs", "README.rst"}) {
		test.Fatalf("documentation scope=%#v", preset.AllowedPaths)
	}
	if !strings.Contains(preset.CheckNote, "no behavior tests") {
		test.Fatal("documentation check scope was not explained")
	}
	missing := projectGoalPresets(filepath.Join(directory, "missing"))
	if len(missing[0].Checks) != 1 || !reflect.DeepEqual(missing[2].AllowedPaths, []string{"docs", "README.md"}) {
		test.Fatalf("missing repository suggestions=%#v", missing)
	}
}
