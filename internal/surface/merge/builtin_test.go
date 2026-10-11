package merge

import (
	"maps"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modulePath = "github.com/cameronsjo/forgectl"

// moduleRoot is the directory holding this module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := osexec.CommandContext(t.Context(), "go", "env", "GOMOD").Output() //nolint:gosec // G204: a literal go env call
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if filepath.Base(gomod) != "go.mod" {
		t.Fatalf("go env GOMOD = %q, expected a go.mod path", gomod)
	}
	return filepath.Dir(gomod)
}

// TestBuiltinRefusalsCoverTheBinary derives every module package the binary
// compiles in (`go list -deps .`, the main package's closure), for each
// target OS, and checks a file in each one is a built-in refusal: no Go
// package the nightly release ships is merged by the drain (T10.2 security
// review I2, T10.4 security review I1, independent review I2 of
// cameronsjo/forgectl#1212).
func TestBuiltinRefusalsCoverTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	root := moduleRoot(t)
	seen := map[string]bool{}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		cmd := osexec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".") //nolint:gosec // G204: a literal go list call
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps .: %v", goos, err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			if pkg == modulePath || strings.HasPrefix(pkg, modulePath+"/") {
				seen[pkg] = true
			}
		}
	}
	// Self-guard: the closure must hold the binary's main package and the
	// gates, or the derivation is broken.
	for _, want := range []string{"", "/internal/cli", "/internal/surface/merge", "/internal/githubauth", "/internal/bless", "/internal/workflow",
		"/internal/digest", "/internal/skill", "/internal/selfupdate", "/internal/tasks"} {
		if !seen[modulePath+want] {
			t.Fatalf("the closure lacks %s%s; the derivation is broken", modulePath, want)
		}
	}
	pkgs := slices.Sorted(maps.Keys(seen))
	for _, pkg := range pkgs {
		file := "x.go"
		if pkg != modulePath {
			file = strings.TrimPrefix(pkg, modulePath+"/") + "/x.go"
		}
		if builtinRefusal(file) == "" {
			t.Errorf("%s is compiled into the binary, but %s is not a built-in refusal: add its directory to builtinRefusedGlobs", pkg, file)
		}
	}
	if t.Failed() {
		return
	}
	t.Logf("%d module packages in the binary's closure, all refused", len(pkgs))
}

// TestBuiltinRefusalsShippedSkill pins that the agent skill the binary
// embeds and installs is refused, not only its Go package.
func TestBuiltinRefusalsShippedSkill(t *testing.T) {
	for _, p := range []string{"internal/skill/skill/SKILL.md", "internal/skill/skill/references/x.md", "internal/skill/skill.go",
		"internal/digest/digest.go", "internal/workflow/exec.go"} {
		if builtinRefusal(p) == "" {
			t.Errorf("%s is not refused", p)
		}
	}
}

// TestBuiltinRefusalsTopLevel pins that no file at the repository root is
// merged, the root configuration this repository has today included.
func TestBuiltinRefusalsTopLevel(t *testing.T) {
	for _, p := range []string{"main.go", "go.mod", ".golangci.yml", ".goreleaser.yaml", "release-please-config.json",
		".release-please-manifest.json", "AGENTS.md", "CLAUDE.md", "Makefile", "Dockerfile", ".mise.toml", ".coderabbit.yaml",
		"release_workflow_security_test.go", "README.md"} {
		if why := builtinRefusal(p); !strings.Contains(why, "top-level files") {
			t.Errorf("%s: %q, want the top-level refusal", p, why)
		}
	}
	for _, p := range []string{"scripts/check-changelog-owner.sh", "helper/x.swift", ".github/workflows/ci.yml", ".claude/settings.json",
		"internal/cli/tasks.go", "internal/cli/execute.go", "internal/pr/remote.go", "internal/tasks/x.go", "cmd/forgectl/main.go", "vendor/modules.txt", "vendor/github.com/x/y/z.go"} {
		if builtinRefusal(p) == "" {
			t.Errorf("%s is not refused", p)
		}
	}
	for _, p := range []string{"docs/CLAUDE.md", "internal/tasks/AGENTS.md", "docs/sub/claude.local.md",
		"docs/.claude/settings.json", "internal/tasks/.github/x.yml", "docs/.Claude/x"} {
		if builtinRefusal(p) == "" {
			t.Errorf("nested agent or CI file %s is not refused", p)
		}
	}
	for _, p := range []string{"docs/x.md", "docs/sub/x.go"} {
		if why := builtinRefusal(p); why != "" {
			t.Errorf("%s refused: %s", p, why)
		}
	}
}

// TestBuiltinRefusalsBuildFiles pins that other languages' module, lock,
// build and toolchain files are refused at any depth, and that the Go module
// files keep their own reason.
func TestBuiltinRefusalsBuildFiles(t *testing.T) {
	for _, base := range []string{"Cargo.toml", "Cargo.lock", "build.rs", "rust-toolchain", "rust-toolchain.toml",
		"package.json", "package-lock.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "bun.lockb", "bun.lock",
		".npmrc", ".yarnrc", ".yarnrc.yml", "requirements.txt", "requirements-dev.txt", "Requirements_Test.TXT",
		"pyproject.toml", "uv.lock", "poetry.lock", "Pipfile", "Pipfile.lock", "Gemfile", "Gemfile.lock",
		".tool-versions", ".mise.toml", "mise.toml", "flake.nix", "flake.lock", "Dockerfile", "Makefile", "cargo.TOML"} {
		for _, dir := range []string{"docs/", "internal/tasks/sub/", "docs/a/b/"} {
			if why := builtinRefusal(dir + base); !strings.Contains(why, "build and toolchain files") {
				t.Errorf("%s%s: %q, want the build-file refusal", dir, base, why)
			}
		}
	}
	for _, p := range []string{"docs/.cargo/config.toml", "docs/a/.cargo/x", "docs/.CARGO/config"} {
		if why := builtinRefusal(p); !strings.Contains(why, "a .cargo directory is refused at any depth") {
			t.Errorf("%s: %q, want the .cargo refusal", p, why)
		}
	}
	if why := builtinRefusal("docs/x/go.mod"); !strings.Contains(why, "module files (go.mod") {
		t.Errorf("docs/x/go.mod: %q, want the Go module-file reason unchanged", why)
	}
	for _, p := range []string{"docs/requirements.md", "docs/makefile-notes.md", "docs/cargo.md", "docs/build.go"} {
		if why := builtinRefusal(p); why != "" {
			t.Errorf("%s refused: %s", p, why)
		}
	}
}
