package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// redirectConfigDir points os.UserConfigDir (hence config.WorkflowsDir) at a
// temp dir on both macOS ($HOME/Library/Application Support) and Linux
// ($XDG_CONFIG_HOME), returning the resolved user workflows directory.
func redirectConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir, err := config.WorkflowsDir()
	if err != nil {
		t.Fatalf("WorkflowsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	return dir
}

// writeUserWorkflow writes a <name>.workflow.toml under the user workflows dir
// and returns its path.
func writeUserWorkflow(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name+".workflow.toml")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write user workflow %s: %v", p, err)
	}
	return p
}

const validUserWorkflow = "dsl_version = 1\nname = \"demo\"\nversion = \"1.0.0\"\n"

func TestLoad_UserFile(t *testing.T) {
	dir := redirectConfigDir(t)
	data := []byte(validUserWorkflow)
	path := writeUserWorkflow(t, dir, "demo", data)

	src, err := Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.Builtin {
		t.Error("user file must be Builtin=false")
	}
	if src.Path != path {
		t.Errorf("Path = %q, want %q", src.Path, path)
	}
	if string(src.Data) != validUserWorkflow {
		t.Errorf("Data = %q, want the user bytes", src.Data)
	}
}

func TestLoad_Builtin(t *testing.T) {
	redirectConfigDir(t) // no user file written — the built-in wins
	src, err := Load("clean-room-review")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !src.Builtin {
		t.Error("embedded builtin must be Builtin=true")
	}
	if src.Path != "" {
		t.Errorf("builtin Path = %q, want empty", src.Path)
	}
	// Provenance is the embed FS: the bytes must match ReadFile of the embed.
	want, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	if string(src.Data) != string(want) {
		t.Error("builtin Data does not match the embedded bytes")
	}
}

func TestLoad_ShadowBuiltin(t *testing.T) {
	dir := redirectConfigDir(t)
	// A user file named like a builtin shadows it — Builtin is BYTE provenance,
	// so a shadow is Builtin=false and the user bytes win.
	userBytes := []byte("dsl_version = 1\nname = \"clean-room-review\"\nversion = \"9.9.9\"\n")
	path := writeUserWorkflow(t, dir, "clean-room-review", userBytes)

	src, err := Load("clean-room-review")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.Builtin {
		t.Error("a user file shadowing a builtin must be Builtin=false")
	}
	if src.Path != path {
		t.Errorf("Path = %q, want the user path %q", src.Path, path)
	}
	if string(src.Data) != string(userBytes) {
		t.Error("shadowed Load must return the USER bytes, not the embed")
	}
}

func TestLoad_NotFound(t *testing.T) {
	redirectConfigDir(t)
	if _, err := Load("no-such-workflow"); err == nil {
		t.Fatal("Load of an unknown name should error")
	}
}

// TestLoad_RejectsPathTraversingName pins the name→path boundary: a workflow
// name is joined onto a directory, so a name carrying a separator or a ".."
// element would make `run` read — and `bless` write a sidecar next to — an
// arbitrary file. Refused up front, before any filesystem access.
func TestLoad_RejectsPathTraversingName(t *testing.T) {
	dir := redirectConfigDir(t)
	// A real file one level up: a traversal that got through would find it.
	escape := filepath.Join(filepath.Dir(dir), "escape.workflow.toml")
	if err := os.WriteFile(escape, []byte(validUserWorkflow), 0o644); err != nil {
		t.Fatalf("write escape file: %v", err)
	}

	for _, name := range []string{
		"../escape",
		"../../etc/passwd",
		"a/b",
		`a\b`,
		"..",
		".",
		"",
		"./demo",
	} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			_, err := Load(name)
			if err == nil {
				t.Fatalf("Load(%q) should be refused", name)
			}
			if !errors.Is(err, ErrInvalidWorkflowName) {
				t.Errorf("Load(%q) = %v, want ErrInvalidWorkflowName", name, err)
			}
		})
	}
}

func TestLoad_AcceptsPlainName(t *testing.T) {
	dir := redirectConfigDir(t)
	writeUserWorkflow(t, dir, "demo", []byte(validUserWorkflow))
	if _, err := Load("demo"); err != nil {
		t.Fatalf("a plain name must still load: %v", err)
	}
	// A dotted/hyphenated name is a legitimate single element, not a traversal.
	writeUserWorkflow(t, dir, "clean-room.v2", []byte(validUserWorkflow))
	if _, err := Load("clean-room.v2"); err != nil {
		t.Fatalf("a dotted single-element name must load: %v", err)
	}
}

// TestLoad_TOCTOUClosed proves Source carries the bytes: parsing the Source's
// Data succeeds even after the file it came from is deleted, so verify and parse
// consume the same in-memory buffer with no second read to race.
func TestLoad_TOCTOUClosed(t *testing.T) {
	dir := redirectConfigDir(t)
	path := writeUserWorkflow(t, dir, "demo", []byte(validUserWorkflow))

	src, err := Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove workflow file: %v", err)
	}
	wf, err := Parse(src.Data)
	if err != nil {
		t.Fatalf("Parse after removing the file: %v", err)
	}
	if wf.Name != "demo" {
		t.Errorf("parsed name = %q, want demo", wf.Name)
	}
}
