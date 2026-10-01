package review

// Test plan for ParseRegistry's #959 bounds
//
//   [x] Happy: every registry the old whole decode read, it reads the same,
//       aliases included; every one it refused, it refuses
//   [x] Sad: a registry one byte over maxRegistryBytes is refused by name;
//       one at the cap is read; LoadRegistry refuses a larger file too
//   [x] Sad: a mapping over maxRegistryMappingKeys is refused; one at it is
//       read
//   [x] Unhappy: repeated top-level keys cost about what a list of the same
//       size costs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/perftest"
)

// parseRegistryWhole is ParseRegistry's decode before #959.
func parseRegistryWhole(raw []byte) (Registry, error) {
	var reg Registry
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		return Registry{}, err
	}
	return validateRegistry(reg)
}

// TestParseRegistryMatchesTheWholeDecode: the node walk changes nothing
// about a registry the whole decode read or refused, an alias to a shared
// list and a null document included. A merge key, which the walk refuses,
// is the one exception, pinned separately below.
//
// Mutation that turns it red: set maxRegistryMappingKeys below 8, the keys
// a real entry has (the fixture and the inline registry are refused).
func TestParseRegistryMatchesTheWholeDecode(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "release-rhythm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	shared := "version: 1\ngates: &g [one, two]\nrepos:\n" +
		"  - {repo: a, branch: main, class: release-pr, entrypoint: .github/workflows/ship.yml, tag_pattern: v-semver, human_gates: *g}\n"
	for _, raw := range []string{registryYAML, string(fixture), shared, "", "~\n", "version: 1\nversion: 1\n", "version: [1]\n", "repos: {a: b}\n"} {
		got, gotErr := ParseRegistry([]byte(raw))
		want, wantErr := parseRegistryWhole([]byte(raw))
		if (gotErr != nil) != (wantErr != nil) {
			t.Errorf("ParseRegistry(%.60q) err = %v, the whole decode's = %v", raw, gotErr, wantErr)
			continue
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("ParseRegistry(%.60q) = %+v, the whole decode's = %+v", raw, got, want)
		}
	}
}

// TestParseRegistryRefusesMergeKeys: a merge key is refused rather than
// expanded.
//
// Mutation that turns it red: skip CheckTree in ParseRegistry.
func TestParseRegistryRefusesMergeKeys(t *testing.T) {
	raw := "version: 1\nbase: &b {branch: main, class: release-pr, entrypoint: .github/workflows/ship.yml, tag_pattern: v-semver}\nrepos:\n  - <<: *b\n    repo: a\n"
	if _, err := parseRegistryWhole([]byte(raw)); err != nil {
		t.Fatalf("the fixture must be one the whole decode accepted: %v", err)
	}
	if _, err := ParseRegistry([]byte(raw)); err == nil || !strings.Contains(err.Error(), "merge key") {
		t.Fatalf("ParseRegistry = %v, want a merge-key refusal", err)
	}
}

// TestParseRegistryRefusesAnOversizedFile: at maxRegistryBytes the registry
// is read; one byte more and it is refused with the limit named, by
// ParseRegistry and by LoadRegistry.
//
// Mutation that turns it red: drop the ErrTooLarge branch (the error reads
// as a parse failure). LoadRegistry's LimitReader is a memory bound this
// test cannot observe; the refusal it checks comes from ParseRegistry.
func TestParseRegistryRefusesAnOversizedFile(t *testing.T) {
	pad := func(n int) []byte {
		head := registryYAML + "#"
		return []byte(head + strings.Repeat("x", n-len(head)-1) + "\n")
	}
	if _, err := ParseRegistry(pad(maxRegistryBytes)); err != nil {
		t.Fatalf("ParseRegistry at the cap: %v", err)
	}
	over := pad(maxRegistryBytes + 1)
	if _, err := ParseRegistry(over); err == nil || !strings.Contains(err.Error(), "larger than the 256 KiB limit") {
		t.Fatalf("ParseRegistry one byte over: %v, want the limit named", err)
	}
	path := filepath.Join(t.TempDir(), "release-rhythm.yaml")
	if err := os.WriteFile(path, pad(4*maxRegistryBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil || !strings.Contains(err.Error(), "larger than the 256 KiB limit") {
		t.Fatalf("LoadRegistry on a 1 MiB file: %v, want the limit named", err)
	}
}

// TestParseRegistryRefusesAWideMapping: an entry with more than
// maxRegistryMappingKeys keys is refused, and one with exactly that many
// (unknown keys, which the decode ignores) is read.
//
// Mutation that turns it red: pass 0 as CheckTree's key cap.
func TestParseRegistryRefusesAWideMapping(t *testing.T) {
	entry := func(keys int) string {
		var b strings.Builder
		b.WriteString("version: 1\nrepos:\n  - repo: x\n    branch: main\n    class: release-pr\n    entrypoint: .github/workflows/ship.yml\n    tag_pattern: v-semver\n")
		for i := 5; i < keys; i++ {
			b.WriteString("    extra")
			b.WriteString(strings.Repeat("x", i))
			b.WriteString(": 1\n")
		}
		return b.String()
	}
	if _, err := ParseRegistry([]byte(entry(maxRegistryMappingKeys))); err != nil {
		t.Fatalf("ParseRegistry with %d keys: %v", maxRegistryMappingKeys, err)
	}
	if _, err := ParseRegistry([]byte(entry(maxRegistryMappingKeys + 1))); err == nil || !strings.Contains(err.Error(), "over the limit of 64") {
		t.Fatalf("ParseRegistry with %d keys: %v, want the key limit named", maxRegistryMappingKeys+1, err)
	}
}

// TestParseRegistryRepeatedKeysCostLikeAList: a registry of repeated
// top-level keys costs about what one with the same bytes in a list costs
// (perftest.Within, process CPU time). The whole decode compared every pair
// of keys and formatted an error for each repeat. Measured with the fix:
// about 1. Without it, at 8 KiB: well over 20.
//
// Mutation that turns it red: skip CheckTree in ParseRegistry.
func TestParseRegistryRepeatedKeysCostLikeAList(t *testing.T) {
	const n = 8 << 10
	base := []byte(registryYAML + "pad:\n" + strings.Repeat("  - a\n", n/6))
	repeated := []byte(registryYAML + strings.Repeat("a: 1\n", n/5))
	work := func(raw []byte) func() {
		return func() { _, _ = ParseRegistry(raw) }
	}
	baseRun, shapeRun := perftest.Amortize(work(base), work(repeated))
	perftest.Within(t, "registry with repeated keys", 4, baseRun, shapeRun)
}
