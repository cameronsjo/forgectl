package cli

// Test plan for clean.go
//
// newCleanCmd / newCleanCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: a dry run (no --apply) reports the reclaimable total and
//       leaves the fixture untouched
//   [x] Happy: `--root` overrides the client's default root
//   [x] Happy: an invalid `--type` value is rejected before any scan runs
//   [x] Happy: the `cln` alias resolves to the clean command
//   [x] Happy: --caches (dry run) prints the cache-scan section and issues
//       ZERO prune-command Runner calls — only the locate/detect queries
//   [x] Happy: --docker (dry run) prints the docker-scan section and issues
//       ZERO prune-command Runner calls — only `docker system df`
//   [x] Happy: bare `clean` (neither flag) touches no cache/docker Runner
//       calls at all — the opt-in passes are true opt-in, not always-run
//   [x] Unhappy: --type combined with --caches or --docker is rejected
//       before any scan runs (fix round: --type's node|python|go|build
//       vocabulary doesn't describe a cache or docker category)
//   [x] Invariant: a real failure in the dep/build-dir pass (scanning a
//       nonexistent root) does not prevent the --caches pass from running
//       (fix round: passes are now actually isolated, not just claimed to be)
//   [x] Happy: --apply with confirmFn answering Yes reaches the caches
//       pass's real prune command (fix round: forgectl#165 item 3 — confirm
//       is now a package var, confirmFn, so this apply⇒confirm⇒prune seam
//       is pinned by a test rather than only asserted in a comment; before
//       the fix this scenario didn't even compile, since there was no way
//       to substitute a fake confirm for huh's real tty prompt)
//   [x] Unhappy: --apply with confirmFn answering No never reaches the
//       prune command (forgectl#165 item 3, the seam's negative case)
//   [x] Invariant: with --caches and --docker both set, a No to the caches
//       prompt still lets the docker pass run its own preview and its own
//       confirmation afterward — "No" cancels only the pass it answered,
//       not the whole run (forgectl#165 item 1, now also disclosed in the
//       Long help)
//   [x] Unhappy: a docker preview with an unparseable size for one category
//       reports "reclaimable size unknown", never "nothing to reclaim"
//       (forgectl#165 item 6 — those two outcomes must not collapse into
//       the same misleading message)

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

// TestCleanCmd_TypeConflictsWithCachesOrDocker pins the fix-round decision:
// --type only filters the dep/build-dir pass (its node|python|go|build
// vocabulary doesn't describe a package-manager cache or a docker
// category), so combining it with --caches or --docker is rejected before
// any scan runs, rather than silently narrowing (or being silently
// ignored by) the other passes.
func TestCleanCmd_TypeConflictsWithCachesOrDocker(t *testing.T) {
	for _, args := range [][]string{
		{"--type", "node", "--caches"},
		{"--type", "node", "--docker"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			fake := &exec.FakeRunner{}
			client := cleanpkg.New(fake, cleanpkg.WithRoot(root))
			cmd := newCleanCmdForClient(client)
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(args)

			if err := cmd.ExecuteContext(context.Background()); err == nil {
				t.Fatal("expected an error combining --type with --caches/--docker")
			}
			if len(fake.Calls) != 0 {
				t.Errorf("the conflict must be rejected before any scan runs; saw %d Runner calls: %+v", len(fake.Calls), fake.Calls)
			}
		})
	}
}

// TestCleanCmd_DirsPassFailureDoesNotBlockCachesPass pins the fix-round
// isolation fix: a real failure in the dep/build-dir pass must not prevent
// the --caches pass from running — runClean collects each pass's error
// rather than returning on the first one. The failure is forced by
// unsetting HOME: with no --root flag and no [clean] default_root, a
// Client built via New(fake) (no WithRoot) has an unresolvable default
// root, and os.UserHomeDir() genuinely errors when $HOME is empty (Go's
// os package treats an empty HOME identically to an unset one on Unix,
// including macOS) — so ScanReport's "no root to scan" error is real, not
// simulated. Scanning a merely-nonexistent PATH does NOT work for this:
// Scan's walk is deliberately fail-safe and swallows a missing root
// silently (see scan.go's WalkDir callback).
func TestCleanCmd_DirsPassFailureDoesNotBlockCachesPass(t *testing.T) {
	t.Setenv("HOME", "")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "npm" {
			return "/fake/npm/cache", nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake) // no WithRoot: root resolution genuinely fails
	cmd := newCleanCmdForClient(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--caches"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error: no root to scan (HOME unset, no --root, no [clean] default_root)")
	}

	sawNpmLocate := false
	for _, call := range fake.Calls {
		if call.Name == "npm" {
			sawNpmLocate = true
		}
	}
	if !sawNpmLocate {
		t.Error("the caches pass must still run despite the dep/build-dir pass failing (isolation) — no npm locate call was seen")
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

// withConfirmFn overrides confirmFn for the duration of a test, restoring
// the real confirm on cleanup — every caches/docker prompt in --apply mode
// goes through this var (see confirm.go), which is what makes it fakeable
// without a real tty at all.
func withConfirmFn(t *testing.T, fn func(string) (bool, error)) {
	t.Helper()
	orig := confirmFn
	confirmFn = fn
	t.Cleanup(func() { confirmFn = orig })
}

// TestCleanCmd_CachesApply_ConfirmYes_ReachesPrune pins forgectl#165 item 3:
// before confirmFn existed, confirm() called huh directly and there was no
// way to drive --apply's confirm-then-prune path in a test at all (huh's
// Run() needs a real tty). This is the seam's positive case — a Yes must
// actually reach npm's prune command, not just the read-only locate query.
func TestCleanCmd_CachesApply_ConfirmYes_ReachesPrune(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "npm" {
			return cacheDir, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches", "--apply"})

	withConfirmFn(t, func(string) (bool, error) { return true, nil })

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sawPrune := false
	for _, call := range fake.Calls {
		if call.Name == "npm" && strings.Contains(strings.Join(call.Args, " "), "cache clean") {
			sawPrune = true
		}
	}
	if !sawPrune {
		t.Errorf("expected --apply with a Yes confirmation to reach \"npm cache clean --force\"; Runner calls: %+v", fake.Calls)
	}
}

// TestCleanCmd_CachesApply_ConfirmNo_NeverPrunes is the seam's negative
// case: a No must reach the confirmation prompt (proving the gate runs at
// all) but never the prune command.
func TestCleanCmd_CachesApply_ConfirmNo_NeverPrunes(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "npm" {
			return cacheDir, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches", "--apply"})

	promptSeen := false
	withConfirmFn(t, func(string) (bool, error) {
		promptSeen = true
		return false, nil
	})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !promptSeen {
		t.Fatal("expected --apply to reach the confirmation prompt")
	}
	for _, call := range fake.Calls {
		if call.Name == "npm" && strings.Contains(strings.Join(call.Args, " "), "cache clean") {
			t.Errorf("a No confirmation must never reach the prune command; saw: %v", call.Args)
		}
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Errorf("expected \"cancelled\" on stdout, got: %q", stdout.String())
	}
}

// TestCleanCmd_CachesCancelDoesNotAbortDockerPass pins forgectl#165 item 1:
// a No to the caches pass's prompt only cancels THAT pass — with --docker
// also requested, its own preview and its own confirmation prompt still run
// afterward, rather than the whole command aborting on the first No. Every
// confirmFn call in this test answers No, so NO prune command should ever
// fire, but the docker section must still render.
func TestCleanCmd_CachesCancelDoesNotAbortDockerPass(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dfOut := strings.Join([]string{
		`{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"500MB"}`,
		`{"Type":"Containers","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Local Volumes","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Build Cache","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch name {
		case "npm":
			return cacheDir, nil
		case "docker":
			return dfOut, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches", "--docker", "--apply"})

	promptCount := 0
	withConfirmFn(t, func(string) (bool, error) {
		promptCount++
		return false, nil // No, every time
	})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if promptCount != 2 {
		t.Errorf("expected 2 confirmation prompts (caches then docker), saw %d", promptCount)
	}
	if !strings.Contains(stdout.String(), "docker prune:") {
		t.Errorf("expected the docker pass to still run its own preview after a No on the caches pass; got: %q", stdout.String())
	}
	for _, call := range fake.Calls {
		if call.Name == "docker" && strings.Contains(strings.Join(call.Args, " "), "prune") {
			t.Errorf("a No confirmation must never reach a prune command; saw docker %v", call.Args)
		}
	}
}

// TestCleanCmd_DockerFlag_UnparseableSize_ReportsUnknownNotNothing pins
// forgectl#165 item 6: an unrecognized size unit for one docker category
// (df DID report it, but the byte parse failed) must not collapse into
// "nothing to reclaim" — that message is reserved for a GENUINE all-zero
// total, and printing it under a nonzero-looking raw string shown one line
// above was the exact self-contradiction reported.
func TestCleanCmd_DockerFlag_UnparseableSize_ReportsUnknownNotNothing(t *testing.T) {
	root := t.TempDir()
	dfOut := strings.Join([]string{
		`{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"1.2XB (10%)"}`,
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
	got := stdout.String()
	// Only the docker section's own verdict is under test here — the
	// unrelated dep/build-dir pass (an empty --root) legitimately prints
	// its own "nothing to reclaim" first, and that's not the bug.
	dockerSection := got[strings.Index(got, "docker prune:"):]
	if strings.Contains(dockerSection, "nothing to reclaim") {
		t.Errorf("must not print \"nothing to reclaim\" in the docker section when a category's size failed to parse; docker section: %q", dockerSection)
	}
	if !strings.Contains(dockerSection, "reclaimable size unknown") {
		t.Errorf("expected the unknown-size message in the docker section, got: %q", dockerSection)
	}
}
