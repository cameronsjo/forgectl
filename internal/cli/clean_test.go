package cli

// Test plan for clean.go
//
// newCleanCmd / newCleanCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: a dry run (no --apply) reports the reclaimable total and
//       leaves the fixture untouched
//   [x] Happy: `--root` overrides the client's default root
//   [x] Happy: an invalid `--type` value is rejected before any scan runs
//   [x] Happy: the `cln` alias resolves to the clean command
//   [x] Happy: --apply is never exercised here (it drives a huh confirmation
//       prompt requiring a tty) — that path is covered end-to-end at the
//       ops layer in internal/clean/clean_test.go, mirroring branch's split
//       (no CLI-level apply+confirm test either)
//   [x] Happy: --caches (dry run) prints the cache-scan section and issues
//       ZERO prune-command Runner calls — only the locate/detect queries
//   [x] Happy: --docker (dry run) prints the docker-scan section and issues
//       ZERO prune-command Runner calls — only `docker system df`
//   [x] Happy: bare `clean` (neither flag) touches no cache/docker Runner
//       calls at all — the opt-in passes are true opt-in, not always-run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestCleanCmd_DryRun_ReportsReclaimableAndTouchesNothing(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	leaf := filepath.Join(nm, "leaf.js")
	if err := os.MkdirAll(filepath.Dir(leaf), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(leaf, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(leaf); err != nil {
		t.Errorf("fixture must survive a dry run, stat error: %v", err)
	}
	if got := stdout.String(); got == "" {
		t.Error("expected non-empty dry-run report on stdout")
	}
}

func TestCleanCmd_RootFlag_OverridesDefault(t *testing.T) {
	explicitRoot := t.TempDir()
	nm := filepath.Join(explicitRoot, "proj", "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nm, "leaf.js"), make([]byte, 42), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The client's own default root is a DIFFERENT (empty) temp dir — only
	// --root should be scanned.
	defaultRoot := t.TempDir()
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(defaultRoot))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--root", explicitRoot})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got == "" || got == "no reclaimable directories found\n\nnothing to reclaim\n" {
		t.Errorf("expected the explicit --root's node_modules to be reported, got: %q", got)
	}
}

func TestCleanCmd_InvalidType_RejectedBeforeScan(t *testing.T) {
	root := t.TempDir()
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--type", "rust"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for an unknown --type value")
	}
}

// TestCleanCmd_CachesFlag_DryRun_NoPruneCalls pins --caches' dry-run
// contract: the section renders and every probed tool is "detected" (the
// fake serves a path for each), but with no --apply, PruneCaches must never
// be reached — only each tool's read-only locate query.
func TestCleanCmd_CachesFlag_DryRun_NoPruneCalls(t *testing.T) {
	root := t.TempDir() // empty: nothing for the dep/build-dir pass to find
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch name {
		case "npm", "pnpm", "pip", "go", "brew":
			return "/fake/cache/" + name, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "package-manager caches:") {
		t.Errorf("expected the caches section header, got: %q", stdout.String())
	}
	for _, call := range fake.Calls {
		argv := strings.Join(call.Args, " ")
		for _, mutating := range []string{"cache clean", "store prune", "cache purge", "clean -cache", "cleanup -s"} {
			if strings.Contains(argv, mutating) {
				t.Errorf("dry run (--caches, no --apply) must never prune; saw %s %q", call.Name, argv)
			}
		}
	}
}

// TestCleanCmd_DockerFlag_DryRun_NoPruneCalls mirrors the caches test for
// --docker: the section renders from a successful `docker system df`, but
// with no --apply, PruneDocker must never be reached.
func TestCleanCmd_DockerFlag_DryRun_NoPruneCalls(t *testing.T) {
	root := t.TempDir()
	dfOut := strings.Join([]string{
		`{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"500MB"}`,
		`{"Type":"Containers","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Local Volumes","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Build Cache","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "docker" {
			return dfOut, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--docker"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "docker prune:") {
		t.Errorf("expected the docker section header, got: %q", stdout.String())
	}
	for _, call := range fake.Calls {
		if call.Name != "docker" {
			continue
		}
		if strings.Contains(strings.Join(call.Args, " "), "prune") {
			t.Errorf("dry run (--docker, no --apply) must never prune; saw docker %v", call.Args)
		}
	}
}

// TestCleanCmd_NoFlags_NeverTouchesCachesOrDocker pins that --caches/--docker
// are true opt-in: the bare command must never invoke any of the
// cache/docker tools at all, not even their read-only locate/df queries.
func TestCleanCmd_NoFlags_NeverTouchesCachesOrDocker(t *testing.T) {
	root := t.TempDir()
	fake := &exec.FakeRunner{}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range fake.Calls {
		switch call.Name {
		case "npm", "pnpm", "pip", "go", "brew", "docker":
			t.Errorf("bare clean (no --caches/--docker) must never touch %s; saw call: %v", call.Name, call.Args)
		}
	}
}

func TestCleanCmd_AliasResolvesToCanonicalVerb(t *testing.T) {
	client := cleanpkg.New(&exec.FakeRunner{})
	cmd := newCleanCmdForClient(client)

	found := false
	for _, a := range cmd.Aliases {
		if a == "cln" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected \"cln\" among clean's aliases, got %v", cmd.Aliases)
	}
}
