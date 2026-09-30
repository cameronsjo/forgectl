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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
			cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})

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
	confirmFn = func(_ theme.Theme, prompt string) (bool, error) { return fn(prompt) }
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
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

// TestCleanCmd_DockerFlag_PartialUnparseable_PreviewAndPromptAgree is a
// code-review catch: with a NONZERO total (Images parses to 500MB) AND a
// separately unparseable category (Containers), the preview line must
// disclose that the total is a lower bound — otherwise the flat "500MB
// reclaimable from docker" preview would silently contradict the --apply
// confirmation prompt below it, which (correctly) asks to prune "an
// unknown amount" because `unknown` is true regardless of total.
func TestCleanCmd_DockerFlag_PartialUnparseable_PreviewAndPromptAgree(t *testing.T) {
	root := t.TempDir()
	dfOut := strings.Join([]string{
		`{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"500MB"}`,
		`{"Type":"Containers","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"1.2XB (10%)"}`,
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
	cmd := newCleanCmdForClient(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--docker", "--apply"})

	var gotPrompt string
	withConfirmFn(t, func(p string) (bool, error) {
		gotPrompt = p
		return false, nil
	})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := stdout.String()
	dockerSection := got[strings.Index(got, "docker prune:"):]
	if strings.Contains(dockerSection, "500MB reclaimable from docker\n") {
		t.Errorf("preview must not print a bare total when another category's size is unparseable; docker section: %q", dockerSection)
	}
	if !strings.Contains(dockerSection, "at least") {
		t.Errorf("expected the preview to disclose the total is a lower bound (\"at least\"), got: %q", dockerSection)
	}
	if !strings.Contains(gotPrompt, "unknown amount") {
		t.Errorf("expected the confirmation prompt to also say the amount is unknown, got: %q", gotPrompt)
	}
}

// TestCleanCmd_CachesApply_PnpmDetected_PromptCarriesMismatchCaveat pins
// forgectl#165 item 2's runtime behavior, not just the source string: when
// pnpm is a detected cache target, the --apply confirmation prompt text
// itself must carry the preview/prune-mismatch caveat (ScanCaches sizes
// pnpm's whole content-addressable store; `pnpm store prune` only reclaims
// unreferenced packages).
func TestCleanCmd_CachesApply_PnpmDetected_PromptCarriesMismatchCaveat(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "pnpm" {
			return cacheDir, nil
		}
		return "", nil
	}}
	client := cleanpkg.New(fake, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches", "--apply"})

	var gotPrompt string
	withConfirmFn(t, func(p string) (bool, error) {
		gotPrompt = p
		return false, nil
	})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotPrompt, "content-addressable store") {
		t.Errorf("expected the pnpm preview/prune-mismatch caveat in the confirmation prompt, got: %q", gotPrompt)
	}
}

func TestCleanCmd_JSON_ReportsItemsAndBytesWithoutDeleting(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	leaf := filepath.Join(nm, "leaf.js")
	if err := os.MkdirAll(filepath.Dir(leaf), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaf, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaf); err != nil {
		t.Errorf("--json must not delete: %v", err)
	}
	var got struct {
		Root  string `json:"root"`
		Items []struct {
			Path      string `json:"path"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"items"`
		Total int64 `json:"total_reclaimable_bytes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if len(got.Items) != 1 || got.Items[0].SizeBytes < 100 || got.Total != got.Items[0].SizeBytes || got.Root == "" {
		t.Errorf("report = %+v", got)
	}
}

func TestCleanCmd_JSON_EmptyRootIsEmptyArray(t *testing.T) {
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"items": []`) {
		t.Errorf("stdout = %s, want an empty items array and no prose", stdout.String())
	}
}

func TestCleanCmd_JSONRefusedWithApplyCachesDocker(t *testing.T) {
	for _, flag := range []string{"--apply", "--caches", "--docker"} {
		client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(t.TempDir()))
		cmd := newCleanCmdForClient(client, theme.Theme{})
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"--json", flag})
		err := cmd.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "--json") {
			t.Errorf("%s: error = %v, want a --json refusal", flag, err)
		}
	}
}

// TestCleanCmd_QuotesHostilePathOnTerminal pins forgectl#855 item 3: a
// directory under the scanned root whose name carries a bidi override and a
// C1 CSI reaches the terminal escaped and quoted, in both the dry-run row
// and the apply pass's reclaimed row, while --json keeps the path raw. The
// runes are \u escapes so no literal format character sits in source.
//
// Mutation that turns it red: print item.Path raw in printCleanItems (the
// dry-run row) or in the reclaimed row of runCleanDirs.
func TestCleanCmd_QuotesHostilePathOnTerminal(t *testing.T) {
	const hostile = "ev\u202eil\u009b31m"
	newRoot := func(t *testing.T) (string, string) {
		t.Helper()
		root := t.TempDir()
		nm := filepath.Join(root, hostile, "node_modules")
		if err := os.MkdirAll(nm, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(nm, "leaf.js"), make([]byte, 64), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return root, nm
	}
	run := func(t *testing.T, root string, args ...string) string {
		t.Helper()
		client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
		cmd := newCleanCmdForClient(client, theme.Theme{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("clean %v: %v", args, err)
		}
		return stdout.String()
	}
	assertInert := func(t *testing.T, got string) {
		t.Helper()
		if strings.ContainsAny(got, "\u202e\u009b") {
			t.Errorf("stdout carries a raw bidi/control rune: %q", got)
		}
		if !strings.Contains(got, `ev\u202eil\u009b31m`) {
			t.Errorf("stdout = %q, want the escaped directory name", got)
		}
	}

	t.Run("dry run", func(t *testing.T) {
		root, _ := newRoot(t)
		got := run(t, root)
		assertInert(t, got)
		if !strings.Contains(got, `node_modules" — `) {
			t.Errorf("dry-run row = %q, want the path quoted", got)
		}
	})

	t.Run("apply", func(t *testing.T) {
		withConfirmFn(t, func(string) (bool, error) { return true, nil })
		root, nm := newRoot(t)
		got := run(t, root, "--apply")
		if _, err := os.Stat(nm); !os.IsNotExist(err) {
			t.Fatalf("node_modules must be reclaimed, stat error: %v", err)
		}
		_, applied, found := strings.Cut(got, "\nreclaimed ")
		if !found {
			t.Fatalf("stdout = %q, want a reclaimed row", got)
		}
		assertInert(t, applied)
	})

	t.Run("json keeps the raw path", func(t *testing.T) {
		root, nm := newRoot(t)
		got := run(t, root, "--json")
		var report struct {
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(got), &report); err != nil {
			t.Fatalf("clean --json = %q: %v", got, err)
		}
		if len(report.Items) != 1 || !strings.HasSuffix(report.Items[0].Path, filepath.Join(hostile, "node_modules")) {
			t.Errorf("clean --json items = %+v, want the raw path ending %q", report.Items, nm)
		}
	})
}

// TestCleanCmd_CachesAndDockerRowsAreInert is forgectl#864 item 2: the
// --caches preview path, the --caches and --docker FAILED rows, and the
// docker-unreachable skip row reach the terminal escaped. A located cache
// directory is tool-reported, and a failure's text carries the tool's own
// stderr, so either can hold a bidi override or a C1 CSI. The runes are \u
// escapes so no literal format character sits in source.
//
// Mutations that turn it red, one per subtest: print item.Path raw in
// printCacheItems, item.Err raw in the --caches or --docker FAILED row, or
// item.SkipReason raw in printDockerItems.
func TestCleanCmd_CachesAndDockerRowsAreInert(t *testing.T) {
	const hostile = "ev\u202eil\u009b31m"
	const escaped = `ev\u202eil\u009b31m`
	failure := errors.New("daemon said " + hostile)
	dfOut := `{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"500MB"}`
	// run returns clean's stdout; a failed prune also fails the command, so
	// its error is not the test's concern here.
	run := func(t *testing.T, runFunc func(string, []string) (string, error), args ...string) string {
		t.Helper()
		withConfirmFn(t, func(string) (bool, error) { return true, nil })
		client := cleanpkg.New(&exec.FakeRunner{RunFunc: runFunc}, cleanpkg.WithRoot(t.TempDir()))
		cmd := newCleanCmdForClient(client, theme.Theme{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		_ = cmd.ExecuteContext(context.Background())
		return stdout.String()
	}
	assertInert := func(t *testing.T, got, want string) {
		t.Helper()
		if strings.ContainsAny(got, "\u202e\u009b") {
			t.Errorf("stdout carries a raw bidi/control rune: %q", got)
		}
		if !strings.Contains(got, want) {
			t.Errorf("stdout = %q, want it to contain %q", got, want)
		}
	}

	t.Run("caches preview path", func(t *testing.T) {
		cacheDir := filepath.Join(t.TempDir(), hostile)
		if err := os.MkdirAll(cacheDir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		got := run(t, func(name string, _ []string) (string, error) {
			if name == "npm" {
				return cacheDir, nil
			}
			return "", errors.New("not installed")
		}, "--caches")
		assertInert(t, got, escaped+`" — `)
	})

	t.Run("caches FAILED row", func(t *testing.T) {
		cacheDir := t.TempDir()
		// A non-empty cache, so the pass has something to reclaim and prunes.
		if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got := run(t, func(name string, args []string) (string, error) {
			switch {
			case name == "npm" && len(args) > 0 && args[0] == "cache":
				return "", failure
			case name == "npm":
				return cacheDir, nil
			}
			return "", errors.New("not installed")
		}, "--caches", "--apply")
		assertInert(t, got, "FAILED  npm: daemon said "+escaped)
	})

	t.Run("docker skip row", func(t *testing.T) {
		got := run(t, func(string, []string) (string, error) { return "", failure }, "--docker")
		assertInert(t, got, "docker unreachable: daemon said "+escaped)
	})

	t.Run("docker FAILED row", func(t *testing.T) {
		got := run(t, func(name string, args []string) (string, error) {
			if name == "docker" && len(args) > 0 && args[0] == "system" && args[1] == "df" {
				return dfOut, nil
			}
			return "", failure
		}, "--docker", "--apply")
		assertInert(t, got, "FAILED  images: daemon said "+escaped)
	})
}

// TestCleanCmd_DiagnosticRowsAreCapped is forgectl#867 items 1 and 3: the
// --caches and --docker FAILED rows and the docker-unreachable skip row are
// escaped (forgectl#864) AND bounded. exec keeps up to a 64 KiB stderr tail,
// and without a cap each of those rows prints all of it as one line — the
// skip row once per docker category. The cap keeps both ends, so the text's
// LAST words (where a tool prints its fatal line) survive it.
//
// Mutations that turn it red, one per subtest: drop the cap in
// cleanFailureText (both FAILED rows), render item.SkipReason with plain
// SafeLine in printDockerItems, or make cleanDiagnostic a head-only cut
// (termsafe.SafeLineMax) — all three rows.
func TestCleanCmd_DiagnosticRowsAreCapped(t *testing.T) {
	failure := errors.New("daemon said " + strings.Repeat("x", 64*1024) + " Error: fatal-end")
	dfOut := `{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"500MB"}`
	run := func(t *testing.T, runFunc func(string, []string) (string, error), args ...string) string {
		t.Helper()
		withConfirmFn(t, func(string) (bool, error) { return true, nil })
		client := cleanpkg.New(&exec.FakeRunner{RunFunc: runFunc}, cleanpkg.WithRoot(t.TempDir()))
		cmd := newCleanCmdForClient(client, theme.Theme{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		_ = cmd.ExecuteContext(context.Background())
		return stdout.String()
	}
	// assertCapped checks every line starting with prefix: at least one must
	// exist, each must end in the truncation marker, and none may exceed the
	// cap plus the row's own fixed label.
	assertCapped := func(t *testing.T, got, prefix string, wantRows int) {
		t.Helper()
		const labelSlack = 64
		rows := 0
		for _, line := range strings.Split(got, "\n") {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			rows++
			if n := utf8.RuneCountInString(line); n > cleanDiagnosticMaxRunes+utf8.RuneCountInString(cleanElision)+labelSlack {
				t.Errorf("row %q... is %d runes, over the %d-rune cap", line[:40], n, cleanDiagnosticMaxRunes)
			}
			if !strings.Contains(line, cleanElision) {
				t.Errorf("capped row carries no elision marker: %.120q", line)
			}
			if !strings.HasSuffix(line, " Error: fatal-end") {
				t.Errorf("capped row lost the text's last words: ...%q", line[max(0, len(line)-60):])
			}
		}
		if rows != wantRows {
			t.Errorf("found %d %q row(s), want %d; stdout starts %q", rows, prefix, wantRows, got[:min(len(got), 200)])
		}
	}

	t.Run("caches FAILED row", func(t *testing.T) {
		cacheDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got := run(t, func(name string, args []string) (string, error) {
			switch {
			case name == "npm" && len(args) > 0 && args[0] == "cache":
				return "", failure
			case name == "npm":
				return cacheDir, nil
			}
			return "", errors.New("not installed")
		}, "--caches", "--apply")
		assertCapped(t, got, "FAILED  npm: ", 1)
	})

	t.Run("docker skip row", func(t *testing.T) {
		got := run(t, func(string, []string) (string, error) { return "", failure }, "--docker")
		assertCapped(t, got, "skip  ", 4)
	})

	t.Run("docker FAILED row", func(t *testing.T) {
		got := run(t, func(name string, args []string) (string, error) {
			if name == "docker" && len(args) > 1 && args[0] == "system" && args[1] == "df" {
				return dfOut, nil
			}
			return "", failure
		}, "--docker", "--apply")
		assertCapped(t, got, "FAILED  images: ", 1)
	})
}

// TestCleanCmd_DockerReportedSizeIsInert is forgectl#867 item 7: when a
// category's size does not parse, the docker preview shows docker's own raw
// Reclaimable string. That string is decoded from `docker system df` JSON and
// the daemon can be remote, so it reaches the terminal escaped and capped.
//
// Mutation that turns it red: print item.Reported (the size variable) raw in
// printDockerItems.
func TestCleanCmd_DockerReportedSizeIsInert(t *testing.T) {
	const hostile = "1.2XB\u202e\u009b31m"
	dfOut := strings.Join([]string{
		`{"Type":"Images","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"` + hostile + `"}`,
		`{"Type":"Containers","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Local Volumes","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
		`{"Type":"Build Cache","TotalCount":"0","Active":"0","Size":"0B","Reclaimable":"0B"}`,
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "docker" {
			return dfOut, nil
		}
		return "", nil
	}}
	cmd := newCleanCmdForClient(cleanpkg.New(fake, cleanpkg.WithRoot(t.TempDir())), theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--docker"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("clean --docker: %v", err)
	}
	got := stdout.String()
	if strings.ContainsAny(got, "\u202e\u009b") {
		t.Errorf("stdout carries a raw bidi/control rune: %q", got)
	}
	if want := `images      1.2XB\u202e\u009b31m`; !strings.Contains(got, want) {
		t.Errorf("stdout = %q, want it to contain %q", got, want)
	}
}

// TestCleanCmd_FailureKeepsTheFatalLastLine is the forgectl#867 review catch:
// exec keeps the stderr TAIL because brew, npm and docker print their fatal
// line last, after any warnings. A capped FAILED row must keep that line and
// exec's dropped-bytes note, however many warnings come first.
//
// Mutation that turns it red: make cleanDiagnostic a head-only cut
// (termsafe.SafeLineMax(raw, cleanDiagnosticMaxRunes)).
func TestCleanCmd_FailureKeepsTheFatalLastLine(t *testing.T) {
	const fatal = "npm error EACCES: permission denied, rmdir '/cache/_cacache'"
	failure := &exec.CommandError{
		Name:          "npm",
		Args:          []string{"cache", "clean", "--force"},
		Stderr:        strings.Repeat("npm warn using --force Recommended protections disabled.\n", 40) + fatal,
		StderrDropped: 4096,
		ExitCode:      1,
		Err:           errors.New("exit status 1"),
	}
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 1024), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	withConfirmFn(t, func(string) (bool, error) { return true, nil })
	client := cleanpkg.New(&exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "npm" && len(args) > 0 && args[0] == "cache":
			return "", failure
		case name == "npm":
			return cacheDir, nil
		}
		return "", errors.New("not installed")
	}}, cleanpkg.WithRoot(t.TempDir()))
	cmd := newCleanCmdForClient(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--caches", "--apply"})
	_ = cmd.ExecuteContext(context.Background())

	var row string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "FAILED  npm: ") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("stdout = %q, want a FAILED npm row", stdout.String())
	}
	for _, want := range []string{"npm cache clean --force", "[stderr truncated, 4096 earlier bytes dropped]", cleanElision} {
		if !strings.Contains(row, want) {
			t.Errorf("row = %q, want it to contain %q", row, want)
		}
	}
	if !strings.HasSuffix(row, fatal) {
		t.Errorf("row lost the fatal last line: ...%q", row[max(0, len(row)-100):])
	}
	if n := utf8.RuneCountInString(row); n > cleanDiagnosticMaxRunes+2*utf8.RuneCountInString(cleanElision)+100 {
		t.Errorf("row is %d runes, want it bounded near the %d-rune cap", n, cleanDiagnosticMaxRunes)
	}
}

// TestCleanDiagnostic_NeverSplitsAnEscape pins the order of operations: the
// raw text is cut and THEN escaped, one whole escape per rune, so a cut can
// never leave half of an escape such as \u202e reading as other text. The
// padding walks the cut point across every offset inside an escape.
//
// Mutation that turns it red: split the ESCAPED text per rune in
// escapedPieces (range over termsafe.SafeLine(s)) instead of escaping each raw
// rune.
func TestCleanDiagnostic_NeverSplitsAnEscape(t *testing.T) {
	for pad := range 8 {
		raw := strings.Repeat("a", pad) + strings.Repeat("\u202e", 300) + "Error: fatal"
		got := cleanDiagnostic(raw)
		if !strings.Contains(got, cleanElision) {
			t.Fatalf("pad %d: output was not cut: %.80q", pad, got)
		}
		if !strings.HasSuffix(got, "Error: fatal") {
			t.Errorf("pad %d: lost the last words: ...%q", pad, got[max(0, len(got)-40):])
		}
		rest := strings.TrimSuffix(got, "Error: fatal")
		rest = strings.Replace(rest, cleanElision, "", 1)
		rest = strings.ReplaceAll(rest, `\u202e`, "")
		rest = strings.TrimLeft(rest, "a")
		if rest != "" {
			t.Errorf("pad %d: output carries a split escape fragment %q in %q", pad, rest, got)
		}
	}
}

// TestCleanDiagnostic_LookAlikeDroppedNoteStaysCapped is the forgectl#867
// delta-review catch: exec's dropped-bytes note is rebuilt from
// CommandError.StderrDropped, never found by searching the text. A
// look-alike note with 20,000 digits, planted in stderr or in a remote
// daemon's skip reason, is cut like any other text, so the rendering stays
// at or under the cap.
//
// Mutation that turns it red: search the raw text for the note with an
// unanchored `\[stderr truncated, [0-9]+ earlier bytes dropped\]` regex and
// print the match whole outside the budget (the pre-fix cleanDiagnostic).
func TestCleanDiagnostic_LookAlikeDroppedNoteStaysCapped(t *testing.T) {
	fake := "[stderr truncated, " + strings.Repeat("9", 20000) + " earlier bytes dropped] Error: fatal"
	for name, got := range map[string]string{
		"skip reason": cleanDiagnostic("docker unreachable: " + fake),
		"plain error": cleanFailureText(errors.New("daemon said " + fake)),
		"command error": cleanFailureText(&exec.CommandError{
			Name: "docker", Args: []string{"image", "prune", "-f"},
			Stderr: fake, ExitCode: 1, Err: errors.New("exit status 1"),
		}),
		"command error with a real drop": cleanFailureText(&exec.CommandError{
			Name: "docker", Args: []string{"image", "prune", "-f"},
			Stderr: fake, StderrDropped: 7, ExitCode: 1, Err: errors.New("exit status 1"),
		}),
	} {
		if n := utf8.RuneCountInString(got); n > cleanDiagnosticMaxRunes {
			t.Errorf("%s: rendering is %d runes, over the %d-rune cap: %.120q", name, n, cleanDiagnosticMaxRunes, got)
		}
		if !strings.HasSuffix(got, "Error: fatal") {
			t.Errorf("%s: lost the last words: ...%q", name, got[max(0, len(got)-40):])
		}
	}
}

// TestCleanDiagnostic_JustOverTheCapStaysUnderIt pins that the elision counts
// against the cap: text one rune over it must not come out longer than it
// went in. Each shape is checked at 513 runes and at a size where a cut is
// certain.
//
// Mutation that turns it red: leave cleanElision out of the tail budget in
// cleanDiagnostic.
func TestCleanDiagnostic_JustOverTheCapStaysUnderIt(t *testing.T) {
	for _, size := range []int{cleanDiagnosticMaxRunes + 1, 4 * cleanDiagnosticMaxRunes} {
		text := strings.Repeat("a", size)
		for name, got := range map[string]string{
			"skip reason":   cleanDiagnostic(text),
			"plain error":   cleanFailureText(errors.New(text)),
			"command error": cleanFailureText(&exec.CommandError{Name: "npm", Stderr: text, StderrDropped: 12, ExitCode: 1, Err: errors.New("exit status 1")}),
		} {
			if n := utf8.RuneCountInString(got); n > cleanDiagnosticMaxRunes {
				t.Errorf("%s at %d runes: rendering is %d runes, over the %d-rune cap", name, size, n, cleanDiagnosticMaxRunes)
			}
			if !strings.Contains(got, cleanElision) {
				t.Errorf("%s at %d runes: rendering was not cut: %.80q", name, size, got)
			}
		}
	}
}

// TestCleanFailureText_LongCommandKeepsTheDroppedNote pins the struct-driven
// cut: a command line longer than the head budget is cut on its own, and
// exec's dropped-bytes note (rebuilt from StderrDropped) still follows it
// whole, ahead of the stderr tail. A plain text cut would spend the whole
// head on the command and lose the note.
//
// Mutation that turns it red: have cleanCommandFailure always report
// ok=false, so every CommandError takes the plain cut.
func TestCleanFailureText_LongCommandKeepsTheDroppedNote(t *testing.T) {
	got := cleanFailureText(&exec.CommandError{
		Name:          "npm",
		Args:          []string{"cache", "clean", "--cache", "/" + strings.Repeat("p", 300)},
		Stderr:        strings.Repeat("npm warn noise\n", 80) + "Error: fatal",
		StderrDropped: 99,
		ExitCode:      1,
		Err:           errors.New("exit status 1"),
	})
	if !strings.HasPrefix(got, "npm cache clean --cache /ppp") {
		t.Errorf("rendering lost the command head: %.80q", got)
	}
	if !strings.Contains(got, cleanElision+"[stderr truncated, 99 earlier bytes dropped] ") {
		t.Errorf("rendering lost the dropped-bytes note after the cut command: %.300q", got)
	}
	if !strings.HasSuffix(got, "Error: fatal") {
		t.Errorf("rendering lost the last words: ...%q", got[max(0, len(got)-40):])
	}
	if n := utf8.RuneCountInString(got); n > cleanDiagnosticMaxRunes {
		t.Errorf("rendering is %d runes, over the %d-rune cap", n, cleanDiagnosticMaxRunes)
	}
}
