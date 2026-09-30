package cli

// Test plan for docs_roots.go
//
// resolveDocsRoots (Classification: ops layer — root-set resolution)
//   [x] Happy: explicit args replace the default set entirely
//   [x] Happy: no args defaults to [cwd] when ./docs and the env var are absent
//   [x] Happy: no args includes ./docs when it exists
//   [x] Happy: config.Docs.Roots is additive to the defaults
//   [x] Happy: $CADENCE_FIELD_REPORTS_DIR is included when set and it exists
//   [x] Happy: a leading ~ in config roots expands to the home directory
//   [x] Unhappy: a ~ config root with an unresolvable home is an error
//
// dedupPaths (Classification: helper)
//   [x] Happy: "." and its absolute equivalent collapse to one entry
//
// docsIndexOptions (Classification: config -> docs.IndexOptions conversion)
//   [x] Happy: empty RootKinds converts to a zero-value IndexOptions
//   [x] Happy: "docs"/"vault" values convert to their RootKind constants,
//       keyed by the config path exactly as written
//   [x] Unhappy: an unknown value is a config error naming the key and value
//   [x] Happy: a leading ~ in a root_kinds key expands, and the index
//       classifies that root with the configured kind
//   [x] Unhappy: a ~ root_kinds key with an unresolvable home is an error
//
// expandDocsConfig (Classification: home expansion, injected lookup)
//   [x] Unhappy: ~ and ~/x entries (roots, root_kinds) fail on a failed lookup
//   [x] Happy: absolute/relative/mid-path-~ entries never call the lookup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
)

func TestResolveDocsRoots_ArgsOverrideDefaults(t *testing.T) {
	got, err := resolveDocsRoots([]string{"a", "b"}, config.DocsConfig{Roots: []string{"c"}})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("resolveDocsRoots(args) = %v, want exactly the given args", got)
	}
}

func TestResolveDocsRoots_NoArgs_DefaultsToCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolveDocsRoots(no args) = %v, want exactly [cwd]", got)
	}
}

func TestResolveDocsRoots_IncludesDocsDirWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if filepath.Base(r) == "docs" {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include ./docs", got)
	}
}

func TestResolveDocsRoots_ConfigRootsAreAdditive(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	extra := t.TempDir()
	got, err := resolveDocsRoots(nil, config.DocsConfig{Roots: []string{extra}})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if r == extra {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include configured extra root %q", got, extra)
	}
}

func TestResolveDocsRoots_IncludesFieldReportsDirWhenSet(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	frDir := t.TempDir()
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", frDir)

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if r == frDir {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include $CADENCE_FIELD_REPORTS_DIR %q", got, frDir)
	}
}

func TestDedupPaths_CollapsesEquivalentAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := dedupPaths([]string{".", dir})
	if len(got) != 1 {
		t.Errorf("dedupPaths([., abs]) = %v, want exactly one entry", got)
	}
}

func TestDocsIndexOptions_EmptyRootKindsIsZeroValue(t *testing.T) {
	got, err := docsIndexOptions(config.DocsConfig{})
	if err != nil {
		t.Fatalf("docsIndexOptions: %v", err)
	}
	if len(got.RootKinds) != 0 {
		t.Errorf("docsIndexOptions(empty) = %+v, want a zero-value IndexOptions", got)
	}
}

func TestDocsIndexOptions_ConvertsKnownValues(t *testing.T) {
	got, err := docsIndexOptions(config.DocsConfig{RootKinds: map[string]string{
		"/vault/path": "vault",
		"/docs/path":  "docs",
	}})
	if err != nil {
		t.Fatalf("docsIndexOptions: %v", err)
	}
	if got.RootKinds["/vault/path"] != docspkg.RootVault {
		t.Errorf("RootKinds[/vault/path] = %v, want RootVault", got.RootKinds["/vault/path"])
	}
	if got.RootKinds["/docs/path"] != docspkg.RootDocs {
		t.Errorf("RootKinds[/docs/path] = %v, want RootDocs", got.RootKinds["/docs/path"])
	}
}

func TestDocsIndexOptions_RejectsUnknownValue(t *testing.T) {
	_, err := docsIndexOptions(config.DocsConfig{RootKinds: map[string]string{"/some/path": "wiki"}})
	if err == nil {
		t.Fatal("docsIndexOptions: want an error for an unknown root_kinds value, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"/some/path", "wiki", "docs", "vault"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}

func TestResolveDocsRoots_ExpandsTildeInConfigRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")
	t.Chdir(t.TempDir())

	got, err := resolveDocsRoots(nil, config.DocsConfig{Roots: []string{"~/notes"}})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	want := filepath.Join(home, "notes")
	if got[len(got)-1] != want {
		t.Errorf("resolveDocsRoots = %v, want last entry %q", got, want)
	}
}

func TestDocsIndexOptions_ExpandsTildeKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	vault := filepath.Join(home, "v")
	if err := os.MkdirAll(vault, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "a.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, err := docsIndexOptions(config.DocsConfig{RootKinds: map[string]string{"~/v": "vault"}})
	if err != nil {
		t.Fatalf("docsIndexOptions: %v", err)
	}
	if got := opts.RootKinds[vault]; got != docspkg.RootVault {
		t.Fatalf("RootKinds = %v, want %q keyed by the expanded path", opts.RootKinds, vault)
	}

	idx, err := docspkg.NewIndexWithOptions([]string{vault}, opts)
	if err != nil {
		t.Fatalf("NewIndexWithOptions: %v", err)
	}
	if k := idx.Roots()[0].Kind; k != docspkg.RootVault {
		t.Errorf("root Kind = %v, want RootVault", k)
	}
}

var errNoHome = errors.New("no home")

func TestExpandDocsConfig_FailsClosedWhenHomeUnresolvable(t *testing.T) {
	failing := func() (string, error) { return "", errNoHome }
	cases := map[string]config.DocsConfig{
		"root ~":         {Roots: []string{"~"}},
		"root ~/x":       {Roots: []string{"/abs", "~/x"}},
		"root_kinds ~":   {RootKinds: map[string]string{"~": "vault"}},
		"root_kinds ~/x": {RootKinds: map[string]string{"~/x": "vault"}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := expandDocsConfig(cfg, failing)
			if err == nil || !errors.Is(err, errNoHome) {
				t.Fatalf("err = %v, want one wrapping the lookup failure", err)
			}
			if !strings.Contains(err.Error(), "~") {
				t.Errorf("error %q does not name the ~ cause", err)
			}
			if !got.IsZero() {
				t.Errorf("config = %+v, want zero on error", got)
			}
		})
	}
}

func TestExpandDocsConfig_NoLookupWithoutTilde(t *testing.T) {
	calls := 0
	lookup := func() (string, error) { calls++; return "", errNoHome }
	cfg := config.DocsConfig{
		Roots:     []string{"/abs", "rel/dir", "a/~/b", "~user/x"},
		RootKinds: map[string]string{"/abs": "docs", "./r": "vault"},
	}
	got, err := expandDocsConfig(cfg, lookup)
	if err != nil {
		t.Fatalf("expandDocsConfig: %v", err)
	}
	if calls != 0 {
		t.Errorf("home lookup called %d times, want 0", calls)
	}
	if len(got.Roots) != 4 || got.Roots[3] != "~user/x" || got.RootKinds["./r"] != "vault" {
		t.Errorf("config changed unexpectedly: %+v", got)
	}
}

func TestResolveDocsRoots_TildeRootWithNoHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("home directory still resolves on this platform")
	}
	t.Chdir(t.TempDir())
	got, err := resolveDocsRoots(nil, config.DocsConfig{Roots: []string{"~/notes"}})
	if err == nil {
		t.Fatalf("resolveDocsRoots = %v, want an error", got)
	}
	// Absolute-only config still works with no home.
	if _, err := resolveDocsRoots(nil, config.DocsConfig{Roots: []string{t.TempDir()}}); err != nil {
		t.Errorf("absolute root without home: %v", err)
	}
}

func TestDocsIndexOptions_TildeKeyWithNoHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("home directory still resolves on this platform")
	}
	if _, err := docsIndexOptions(config.DocsConfig{RootKinds: map[string]string{"~/v": "vault"}}); err == nil {
		t.Fatal("docsIndexOptions: want an error for ~ key with no home")
	}
}
