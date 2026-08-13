package quarantine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestExpandTargets_RejectsMalformedRulesBeforeFilesystemIO(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"empty", "", "must not be empty"},
		{"dot root", ".", "must not name root"},
		{"portable absolute", `\\server\share`, "must not be absolute"},
		{"portable traversal", `safe\..\outside`, "must not traverse"},
		{"volume path", `C:\outside`, "must not be volume-qualified"},
		{"malformed class", `[abc`, "invalid pattern"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExpandTargets(missingRoot, SuffixQuarantined, []string{tc.target})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ExpandTargets(%q) error = %v, want substring %q", tc.target, err, tc.want)
			}
			if strings.Contains(err.Error(), "does-not-exist") {
				t.Fatalf("rule validation touched the filesystem before rejecting %q: %v", tc.target, err)
			}
		})
	}
}

func TestNormalizeTargetRules_InvalidDiagnosticIsPermutationInvariant(t *testing.T) {
	inputs := []string{"../outside", "", "/absolute"}
	var want string
	for _, permutation := range stringPermutations(inputs) {
		_, err := normalizeTargetRules(permutation)
		if err == nil {
			t.Fatalf("normalizeTargetRules(%v) unexpectedly succeeded", permutation)
		}
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want {
			t.Fatalf("normalizeTargetRules(%v) error = %q, want stable %q", permutation, err, want)
		}
	}
}

func TestConcreteTargets_RejectPatterns(t *testing.T) {
	root := t.TempDir()
	const target = `.*/mcp.json`
	want := `quarantine target ".*/mcp.json" is a pattern; expand it before computing moves`

	moves, err := ComputeMoves(root, SuffixQuarantined, []string{target})
	if err == nil || err.Error() != want {
		t.Fatalf("ComputeMoves error = %v, want %q", err, want)
	}
	if len(moves) != 0 {
		t.Fatalf("ComputeMoves returned moves on refusal: %+v", moves)
	}

	writeFile(t, filepath.Join(root, "canary"), "unchanged")
	moves, err = New(&exec.FakeRunner{}).Hide(context.Background(), root, SuffixQuarantined, []string{target}, false)
	if err == nil || err.Error() != want {
		t.Fatalf("Hide error = %v, want %q", err, want)
	}
	if len(moves) != 0 || readFile(t, filepath.Join(root, "canary")) != "unchanged" {
		t.Fatalf("Hide mutated or returned moves on refusal: %+v", moves)
	}
}

func TestComputeMoves_DeduplicatesExactNormalizedTargetsOnly(t *testing.T) {
	root := t.TempDir()
	moves, err := ComputeMoves(root, PrefixUnderscore, []string{"a", "./a", "a/"})
	if err != nil {
		t.Fatalf("ComputeMoves exact duplicates: %v", err)
	}
	if len(moves) != 1 || filepath.Base(moves[0].From) != "a" {
		t.Fatalf("exact normalized duplicates = %+v, want one first-spelling move", moves)
	}

	_, err = ComputeMoves(root, PrefixUnderscore, []string{"A", "a"})
	if err == nil || !strings.Contains(err.Error(), `identify the same rename location`) {
		t.Fatalf("case-fold-equivalent targets error = %v, want identity ambiguity", err)
	}
}

func TestComputeMoves_ASCIIComparisonDoesNotFoldUnicode(t *testing.T) {
	root := t.TempDir()
	moves, err := ComputeMoves(root, PrefixUnderscore, []string{"Ä", "ä"})
	if err != nil {
		t.Fatalf("non-ASCII spellings must remain distinct under the bounded ASCII fold: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("non-ASCII targets collapsed unexpectedly: %+v", moves)
	}
}

func TestPrepareMoves_RejectsSourceContainmentDeterministically(t *testing.T) {
	want := `quarantine targets ".cursor" (outer) and ".cursor/rules" (inner) overlap: replace the inner entry, do not join it`
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		for _, targets := range [][]string{{".cursor", ".cursor/rules"}, {".cursor/rules/.", ".cursor/"}} {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".cursor", "rules", "r.mdc"), "rules")
			writeFile(t, filepath.Join(root, "canary"), "unchanged")

			moves, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, scheme, targets, false)
			if err == nil || err.Error() != want {
				t.Fatalf("scheme=%v targets=%v error = %v, want %q", scheme, targets, err, want)
			}
			if len(moves) != 0 || readFile(t, filepath.Join(root, "canary")) != "unchanged" {
				t.Fatalf("scheme=%v targets=%v mutated or returned moves: %+v", scheme, targets, moves)
			}
			if got := readFile(t, filepath.Join(root, ".cursor", "rules", "r.mdc")); got != "rules" {
				t.Fatalf("scheme=%v targets=%v changed nested source: %q", scheme, targets, got)
			}
		}
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".cursor"), "one")
	writeFile(t, filepath.Join(root, ".cursorrules"), "two")
	if _, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, PrefixUnderscore, []string{".cursor", ".cursorrules"}, true); err != nil {
		t.Fatalf("segment-prefix sibling negative control: %v", err)
	}
}

func TestPrepareMoves_RejectsDestinationSourceChainDeterministically(t *testing.T) {
	tests := []struct {
		scheme  Scheme
		targets []string
		want    string
	}{
		{SuffixQuarantined, []string{"foo", "foo.quarantined"}, `quarantine moves for "foo" and "foo.quarantined" conflict: destination "foo.quarantined" is another source`},
		{SuffixQuarantined, []string{"foo.quarantined", "foo"}, `quarantine moves for "foo" and "foo.quarantined" conflict: destination "foo.quarantined" is another source`},
		{PrefixUnderscore, []string{"foo", "_foo"}, `quarantine moves for "_foo" and "foo" conflict: destination "_foo" is another source`},
		{PrefixUnderscore, []string{"_foo", "foo"}, `quarantine moves for "_foo" and "foo" conflict: destination "_foo" is another source`},
	}
	for _, tc := range tests {
		root := t.TempDir()
		for _, target := range tc.targets {
			writeFile(t, filepath.Join(root, target), target)
		}
		moves, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, tc.scheme, tc.targets, true)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("scheme=%v targets=%v error = %v, want %q", tc.scheme, tc.targets, err, tc.want)
		}
		if len(moves) != 0 {
			t.Fatalf("scheme=%v targets=%v returned moves: %+v", tc.scheme, tc.targets, moves)
		}
	}
}

func TestPrepareMoves_RejectsSameSourceThroughSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "real", "item"), "x")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	want := `quarantine targets "alias/item" and "real/item" identify the same rename location`
	for _, targets := range [][]string{{"alias/item", "real/item"}, {"real/item", "alias/item"}} {
		moves, err := ComputeMoves(root, SuffixQuarantined, targets)
		if err == nil || err.Error() != want {
			t.Fatalf("targets=%v error = %v, want %q", targets, err, want)
		}
		if len(moves) != 0 {
			t.Fatalf("targets=%v returned moves: %+v", targets, moves)
		}
	}
}

func TestPrepareMoves_RejectsSameSourceThroughDeepestExistingAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o750); err != nil {
		t.Fatalf("MkdirAll real: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	want := `quarantine targets "alias/missing/item" and "real/missing/item" identify the same rename location`
	for _, targets := range [][]string{{"alias/missing/item", "real/missing/item"}, {"real/missing/item", "alias/missing/item"}} {
		moves, err := ComputeMoves(root, SuffixQuarantined, targets)
		if err == nil || err.Error() != want {
			t.Fatalf("targets=%v error = %v, want %q", targets, err, want)
		}
		if len(moves) != 0 {
			t.Fatalf("targets=%v returned moves: %+v", targets, moves)
		}
	}
}

func TestHide_RejectsFinalSymlinkOuterAndDescendantBeforeMutation(t *testing.T) {
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		for _, targets := range [][]string{{"alias", "alias/child"}, {"alias/child", "alias"}} {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "real", "child", "sentinel"), "unchanged")
			if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			want := `quarantine targets "alias" (outer) and "alias/child" (inner) overlap: replace the inner entry, do not join it`
			moves, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, scheme, targets, false)
			if err == nil || err.Error() != want {
				t.Fatalf("scheme=%v targets=%v error = %v, want %q", scheme, targets, err, want)
			}
			if len(moves) != 0 {
				t.Fatalf("scheme=%v targets=%v returned moves: %+v", scheme, targets, moves)
			}
			if got := readFile(t, filepath.Join(root, "real", "child", "sentinel")); got != "unchanged" {
				t.Fatalf("scheme=%v targets=%v child mutated: %q", scheme, targets, got)
			}
			if info, statErr := os.Lstat(filepath.Join(root, "alias")); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("scheme=%v targets=%v outer symlink mutated: info=%v err=%v", scheme, targets, info, statErr)
			}
			if _, statErr := os.Lstat(filepath.Join(root, renamedPath(scheme, "alias"))); !os.IsNotExist(statErr) {
				t.Fatalf("scheme=%v targets=%v decorated outer exists after refusal: %v", scheme, targets, statErr)
			}
		}
	}
}

func TestComputeMoves_RelativeRootUsesOneAbsoluteIdentityNamespace(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	relRoot, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatalf("Rel root: %v", err)
	}
	writeFile(t, filepath.Join(root, "real", "existing"), "x")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	for _, target := range []string{"real/existing", "real/missing/child", "alias/existing", "alias/missing/child"} {
		moves, moveErr := ComputeMoves(relRoot, SuffixQuarantined, []string{target})
		if moveErr != nil {
			t.Fatalf("target=%q ComputeMoves relative root: %v", target, moveErr)
		}
		if len(moves) != 1 {
			t.Fatalf("target=%q moves = %+v, want one", target, moves)
		}
		if !filepath.IsAbs(moves[0].From) || !filepath.IsAbs(moves[0].To) {
			t.Fatalf("target=%q moves are not rooted in one absolute namespace: %+v", target, moves)
		}
	}
}

func TestPrepareMoves_RejectsNestedDestinationSourceChain(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dir", "item"), "item")
	writeFile(t, filepath.Join(root, "dir", "_item"), "decorated")
	want := `quarantine moves for "dir/_item" and "dir/item" conflict: destination "dir/_item" is another source`
	if _, err := ComputeMoves(root, PrefixUnderscore, []string{"dir/item", "dir/_item"}); err == nil || err.Error() != want {
		t.Fatalf("nested destination-source error = %v, want %q", err, want)
	}
}

func TestPrepareMoves_ConflictPrecedenceAndPermutationAreStable(t *testing.T) {
	root := t.TempDir()
	inputs := []string{"foo", "foo.quarantined", ".cursor/rules", ".cursor"}
	want := `quarantine targets ".cursor" (outer) and ".cursor/rules" (inner) overlap: replace the inner entry, do not join it`
	for _, targets := range stringPermutations(inputs) {
		_, err := ComputeMoves(root, SuffixQuarantined, targets)
		if err == nil || err.Error() != want {
			t.Fatalf("targets=%v error = %v, want %q", targets, err, want)
		}
	}
}

func stringPermutations(values []string) [][]string {
	var out [][]string
	var visit func(int)
	items := append([]string{}, values...)
	visit = func(index int) {
		if index == len(items) {
			out = append(out, append([]string{}, items...))
			return
		}
		for i := index; i < len(items); i++ {
			items[index], items[i] = items[i], items[index]
			visit(index + 1)
			items[index], items[i] = items[i], items[index]
		}
	}
	visit(0)
	return out
}

func TestHide_PreflightFailureDoesNotMutateEarlierTargets(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) []string
	}{
		{
			name: "later invalid root target",
			setup: func(t *testing.T, root string) []string {
				return []string{"first", "."}
			},
		},
		{
			name: "later occupied destination",
			setup: func(t *testing.T, root string) []string {
				writeFile(t, filepath.Join(root, "second"), "second")
				writeFile(t, filepath.Join(root, "second.quarantined"), "occupied")
				return []string{"first", "second"}
			},
		},
		{
			name: "later escaping symlink",
			setup: func(t *testing.T, root string) []string {
				external := t.TempDir()
				writeFile(t, filepath.Join(external, "victim"), "victim")
				if err := os.Symlink(filepath.Join(external, "victim"), filepath.Join(root, "escape")); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return []string{"first", "escape"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "first"), "first")
			targets := tc.setup(t, root)
			moves, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, SuffixQuarantined, targets, false)
			if err == nil {
				t.Fatal("expected preflight failure")
			}
			if len(moves) != 0 {
				t.Fatalf("preflight failure returned moves: %+v", moves)
			}
			if got := readFile(t, filepath.Join(root, "first")); got != "first" {
				t.Fatalf("earlier target changed: %q", got)
			}
			if _, statErr := os.Lstat(filepath.Join(root, "first.quarantined")); !os.IsNotExist(statErr) {
				t.Fatalf("earlier target was renamed before later preflight failed: %v", statErr)
			}
		})
	}
}

func TestHide_PreflightInjectedStatFailureDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "first"), "first")
	writeFile(t, filepath.Join(root, "second"), "second")
	client := New(&exec.FakeRunner{})
	realLstat := client.lstat
	client.lstat = func(name string) (os.FileInfo, error) {
		if filepath.Base(name) == "second" {
			return nil, errors.New("injected stat failure")
		}
		return realLstat(name)
	}

	moves, err := client.Hide(context.Background(), root, SuffixQuarantined, []string{"first", "second"}, false)
	if err == nil || !strings.Contains(err.Error(), "injected stat failure") {
		t.Fatalf("Hide error = %v, want injected stat failure", err)
	}
	if len(moves) != 0 || readFile(t, filepath.Join(root, "first")) != "first" {
		t.Fatalf("stat preflight failure mutated or returned moves: %+v", moves)
	}
}

func TestHide_PreflightDiagnosticsDoNotDiscloseAbsoluteRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "source")
	writeFile(t, filepath.Join(root, "target.quarantined"), "occupied")
	_, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, SuffixQuarantined, []string{"target"}, false)
	if err == nil {
		t.Fatal("expected occupied-destination refusal")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("diagnostic disclosed absolute workspace root: %v", err)
	}
	want := `quarantine destination "target.quarantined" already exists`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestHide_RenameFailureDocumentsPartialApplicationBoundary(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "first"), "first")
	writeFile(t, filepath.Join(root, "second"), "second")
	client := New(&exec.FakeRunner{})
	realRename := client.rename
	var calls int
	client.rename = func(oldpath, newpath string) error {
		calls++
		if calls == 2 {
			return errors.New("injected rename failure")
		}
		return realRename(oldpath, newpath)
	}

	_, err := client.Hide(context.Background(), root, SuffixQuarantined, []string{"first", "second"}, false)
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("Hide error = %v, want injected rename failure", err)
	}
	if got := readFile(t, filepath.Join(root, "first.quarantined")); got != "first" {
		t.Fatalf("first rename should remain applied at the documented mutation boundary: %q", got)
	}
	if got := readFile(t, filepath.Join(root, "second")); got != "second" {
		t.Fatalf("second source should remain after its rename fails: %q", got)
	}
}

func TestExpandTargets_CoveredRootListStableBothSchemes(t *testing.T) {
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".claude", "CLAUDE.md"), "covered root")
		writeFile(t, filepath.Join(root, ".claude", "sub", "AGENTS.md"), "covered nested")
		writeFile(t, filepath.Join(root, "packages", "api", "CLAUDE.md"), "uncovered")
		writeFile(t, filepath.Join(root, "_name", "CLAUDE.md"), "unrelated prefix-looking")
		writeFile(t, filepath.Join(root, "name.quarantined", "AGENTS.md"), "unrelated suffix-looking")

		before, err := ExpandTargets(root, scheme, DefaultTargets)
		if err != nil {
			t.Fatalf("scheme=%v ExpandTargets before: %v", scheme, err)
		}
		for _, covered := range []string{filepath.Join(".claude", "CLAUDE.md"), filepath.Join(".claude", "sub", "AGENTS.md")} {
			if containsStr(before, covered) {
				t.Fatalf("scheme=%v covered nested target leaked into expansion: %v", scheme, before)
			}
		}
		for _, uncovered := range []string{filepath.Join("packages", "api", "CLAUDE.md"), filepath.Join("_name", "CLAUDE.md"), filepath.Join("name.quarantined", "AGENTS.md")} {
			if !containsStr(before, uncovered) {
				t.Fatalf("scheme=%v unrelated target %q was globally stripped: %v", scheme, uncovered, before)
			}
		}

		client := New(&exec.FakeRunner{})
		if _, err := client.Hide(context.Background(), root, scheme, before, false); err != nil {
			t.Fatalf("scheme=%v Hide: %v", scheme, err)
		}
		after, err := ExpandTargets(root, scheme, DefaultTargets)
		if err != nil {
			t.Fatalf("scheme=%v ExpandTargets after: %v", scheme, err)
		}
		if !equalStrings(before, after) {
			t.Fatalf("scheme=%v target list changed across Hide:\nbefore=%v\n after=%v", scheme, before, after)
		}
		moves, err := ComputeMoves(root, scheme, after)
		if err != nil {
			t.Fatalf("scheme=%v ComputeMoves restore: %v", scheme, err)
		}
		if err := client.Restore(context.Background(), moves); err != nil {
			t.Fatalf("scheme=%v Restore: %v", scheme, err)
		}
		for rel, want := range map[string]string{
			filepath.Join(".claude", "CLAUDE.md"):         "covered root",
			filepath.Join(".claude", "sub", "AGENTS.md"):  "covered nested",
			filepath.Join("packages", "api", "CLAUDE.md"): "uncovered",
		} {
			if got := readFile(t, filepath.Join(root, rel)); got != want {
				t.Fatalf("scheme=%v round-trip %q = %q, want %q", scheme, rel, got, want)
			}
		}
	}
}

func TestExpandTargets_RootCaseSpellingRoundTrips(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents.md"), "lowercase root")
	client := New(&exec.FakeRunner{})

	before, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets before: %v", err)
	}
	if !containsStr(before, "agents.md") || containsStr(before, "AGENTS.md") {
		t.Fatalf("root target did not retain actual spelling: %v", before)
	}
	if _, err := client.Hide(context.Background(), root, SuffixQuarantined, before, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	after, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets after: %v", err)
	}
	if !equalStrings(before, after) {
		t.Fatalf("root case target changed across Hide: before=%v after=%v", before, after)
	}
	moves, err := ComputeMoves(root, SuffixQuarantined, after)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := client.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "agents.md")); got != "lowercase root" {
		t.Fatalf("root case round-trip content = %q", got)
	}
}

func TestExpandTargets_CoveredRootSymlinkAliasIsNotTraversed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude", "CLAUDE.md"), "covered")
	writeFile(t, filepath.Join(root, ".claude", "mcp.json"), "covered mcp")
	if err := os.Symlink(filepath.Join(root, ".claude"), filepath.Join(root, ".alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	targets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	for _, forbidden := range []string{
		filepath.Join(".alias", "CLAUDE.md"),
		filepath.Join(".alias", "mcp.json"),
		filepath.Join(".claude", "CLAUDE.md"),
		filepath.Join(".claude", "mcp.json"),
	} {
		if !containsStr(targets, forbidden) {
			continue
		}
		t.Fatalf("covered root or symlink alias was traversed: %v", targets)
	}
}

func TestExpandTargets_DecoratedCoveredRootSymlinkAliasIsNotTraversed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude.quarantined", "mcp.json"), "covered mcp")
	if err := os.Symlink(".claude.quarantined", filepath.Join(root, ".alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	targets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if containsStr(targets, filepath.Join(".alias", "mcp.json")) {
		t.Fatalf("decorated covered-root alias was traversed: %v", targets)
	}
}

func TestCanonicalOwns_UsesExactSegmentedFilesystemPaths(t *testing.T) {
	tests := []struct {
		owner     string
		candidate string
		want      bool
	}{
		{"/tmp/Root/.claude", "/tmp/Root/.claude", true},
		{"/tmp/Root/.claude", "/tmp/Root/.claude/sub", true},
		{"/tmp/Root/.claude", "/tmp/Root/.claude-other", false},
		{"/tmp/Root/.claude", "/tmp/root/.claude", false},
		{"/tmp/Root/.claude", "/tmp/root/.claude/sub", false},
	}
	for _, tc := range tests {
		if got := canonicalOwns(tc.owner, tc.candidate); got != tc.want {
			t.Errorf("canonicalOwns(%q, %q) = %v, want %v", tc.owner, tc.candidate, got, tc.want)
		}
	}
}

func TestExpandTargets_CoveredRootDescendantAliasIsNotTraversed(t *testing.T) {
	for _, tc := range []struct {
		scheme    Scheme
		covered   string
		aliasDest string
	}{
		{PrefixUnderscore, ".claude", filepath.Join(".claude", "sub")},
		{PrefixUnderscore, "_.claude", filepath.Join("_.claude", "sub")},
		{SuffixQuarantined, ".claude", filepath.Join(".claude", "sub")},
		{SuffixQuarantined, ".claude.quarantined", filepath.Join(".claude.quarantined", "sub")},
	} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, tc.covered, "sub", "mcp.json"), "covered descendant")
		if err := os.Symlink(tc.aliasDest, filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		targets, err := ExpandTargets(root, tc.scheme, DefaultTargets)
		if err != nil {
			t.Fatalf("scheme=%v covered=%q ExpandTargets: %v", tc.scheme, tc.covered, err)
		}
		if containsStr(targets, filepath.Join(".gemini", "mcp.json")) {
			t.Fatalf("scheme=%v covered=%q descendant alias was traversed: %v", tc.scheme, tc.covered, targets)
		}
	}
}

func TestExpandTargets_CaseOnlyEscapingAliasNeverCountsAsCovered(t *testing.T) {
	base := t.TempDir()
	upperRoot := filepath.Join(base, "Root")
	lowerRoot := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(upperRoot, ".claude"), 0o750); err != nil {
		t.Fatalf("MkdirAll upper root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(lowerRoot, ".claude"), 0o750); err != nil {
		t.Skipf("filesystem cannot represent case-only sibling roots: %v", err)
	}
	upperInfo, err := os.Stat(upperRoot)
	if err != nil {
		t.Fatalf("Stat upper root: %v", err)
	}
	lowerInfo, err := os.Stat(lowerRoot)
	if err != nil {
		t.Fatalf("Stat lower root: %v", err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("filesystem is case-insensitive; Linux behavior is exercised on a case-sensitive runner")
	}
	writeFile(t, filepath.Join(lowerRoot, ".claude", "mcp.json"), "external")
	if err := os.Symlink(filepath.Join(lowerRoot, ".claude"), filepath.Join(upperRoot, ".gemini")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		_, expandErr := ExpandTargets(upperRoot, scheme, DefaultTargets)
		if expandErr == nil || !strings.Contains(expandErr.Error(), `quarantine target ".gemini/mcp.json" escapes root through its parent`) {
			t.Fatalf("scheme=%v case-only escape error = %v", scheme, expandErr)
		}
		if got := readFile(t, filepath.Join(lowerRoot, ".claude", "mcp.json")); got != "external" {
			t.Fatalf("scheme=%v external carrier mutated: %q", scheme, got)
		}
	}
}

func TestExpandTargets_UniqueInRootSymlinkedMCPCarrierRoundTrips(t *testing.T) {
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "real", "mcp.json"), "carrier")
		if err := os.Symlink("real", filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}

		before, err := ExpandTargets(root, scheme, DefaultTargets)
		if err != nil {
			t.Fatalf("scheme=%v ExpandTargets before: %v", scheme, err)
		}
		logical := filepath.Join(".gemini", "mcp.json")
		if !containsStr(before, logical) {
			t.Fatalf("scheme=%v unique symlinked MCP carrier missing: %v", scheme, before)
		}
		client := New(&exec.FakeRunner{})
		moves, err := client.Hide(context.Background(), root, scheme, before, false)
		if err != nil {
			t.Fatalf("scheme=%v Hide: %v", scheme, err)
		}
		if !containsMoveFrom(moves, filepath.Join(root, logical)) {
			t.Fatalf("scheme=%v carrier move does not preserve logical alias ownership: %+v", scheme, moves)
		}
		if _, err := os.Lstat(filepath.Join(root, "real", renamedPath(scheme, "mcp.json"))); err != nil {
			t.Fatalf("scheme=%v carrier not quarantined through alias: %v", scheme, err)
		}
		after, err := ExpandTargets(root, scheme, DefaultTargets)
		if err != nil {
			t.Fatalf("scheme=%v ExpandTargets after: %v", scheme, err)
		}
		if !equalStrings(before, after) {
			t.Fatalf("scheme=%v target set changed: before=%v after=%v", scheme, before, after)
		}
		restoreMoves, err := ComputeMoves(root, scheme, after)
		if err != nil {
			t.Fatalf("scheme=%v ComputeMoves restore: %v", scheme, err)
		}
		if err := client.Restore(context.Background(), restoreMoves); err != nil {
			t.Fatalf("scheme=%v Restore: %v", scheme, err)
		}
		if got := readFile(t, filepath.Join(root, "real", "mcp.json")); got != "carrier" {
			t.Fatalf("scheme=%v restored carrier = %q", scheme, got)
		}
	}
}

func TestExpandTargets_SymlinkedMCPCarrierEscapeFailsClosed(t *testing.T) {
	external := t.TempDir()
	writeFile(t, filepath.Join(external, "mcp.json"), "external")
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		root := t.TempDir()
		if err := os.Symlink(external, filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := ExpandTargets(root, scheme, DefaultTargets); err == nil || !strings.Contains(err.Error(), `quarantine target ".gemini/mcp.json" escapes root through its parent`) {
			t.Fatalf("scheme=%v escape error = %v", scheme, err)
		}
		if got := readFile(t, filepath.Join(external, "mcp.json")); got != "external" {
			t.Fatalf("scheme=%v external carrier mutated: %q", scheme, got)
		}
	}
}

func TestExpandTargets_TwoAliasesToUniqueMCPCarrierFailDeterministically(t *testing.T) {
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "real", "mcp.json"), "carrier")
		for _, alias := range []string{".gemini", ".other"} {
			if err := os.Symlink("real", filepath.Join(root, alias)); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}
		want := `quarantine targets ".gemini/mcp.json" and ".other/mcp.json" identify the same rename location`
		for i := 0; i < 20; i++ {
			_, err := ExpandTargets(root, scheme, DefaultTargets)
			if err == nil || err.Error() != want {
				t.Fatalf("scheme=%v iteration=%d error = %v, want %q", scheme, i, err, want)
			}
		}
	}
}

func TestCoveredRootEntries_RecognizesOnlyOriginalAndSchemeDecoration(t *testing.T) {
	for _, tc := range []struct {
		scheme Scheme
		keys   []string
	}{
		{PrefixUnderscore, []string{".claude", "_.claude"}},
		{SuffixQuarantined, []string{".claude", ".claude.quarantined"}},
	} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".claude"), 0o755); err != nil {
			t.Fatalf("Mkdir .claude: %v", err)
		}
		covered, err := coveredRootEntries(root, tc.scheme, []string{".claude"})
		if err != nil {
			t.Fatalf("coveredRootEntries: %v", err)
		}
		if len(covered) != 2 {
			t.Fatalf("scheme=%v covered map = %v, want exactly two spellings", tc.scheme, covered)
		}
		for _, key := range tc.keys {
			if _, ok := covered[asciiPathFold(key)]; !ok {
				t.Fatalf("scheme=%v covered map missing %q: %v", tc.scheme, key, covered)
			}
		}
		for _, unrelated := range []string{"_name", "name.quarantined", "___.claude", ".claude.quarantined.quarantined"} {
			if _, ok := covered[asciiPathFold(unrelated)]; ok {
				t.Fatalf("scheme=%v covered map globally recognized unrelated %q: %v", tc.scheme, unrelated, covered)
			}
		}
	}
}

func TestUniqueFoldedEntry_RejectsCaseAmbiguity(t *testing.T) {
	entries := []fs.DirEntry{testDirEntry{name: ".claude", dir: true}, testDirEntry{name: ".CLAUDE", dir: true}}
	_, err := uniqueFoldedEntry(entries, ".Claude")
	if err == nil || !strings.Contains(err.Error(), "ambiguous case-fold spellings") {
		t.Fatalf("uniqueFoldedEntry error = %v, want ambiguity refusal", err)
	}
}

type testDirEntry struct {
	name string
	dir  bool
}

func containsMoveFrom(moves []Move, from string) bool {
	for _, move := range moves {
		if move.From == from {
			return true
		}
	}
	return false
}

func (entry testDirEntry) Name() string               { return entry.name }
func (entry testDirEntry) IsDir() bool                { return entry.dir }
func (entry testDirEntry) Type() fs.FileMode          { return 0 }
func (entry testDirEntry) Info() (fs.FileInfo, error) { return nil, nil }
