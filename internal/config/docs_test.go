package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Test plan for docs.go
//
// DocsServersDir (Classification: path derivation)
//   [x] Happy: resolves to <config dir>/forgectl/docs-servers
//   [x] Happy: shares the configDir() base with DocsServerPath, so the new
//              directory and the legacy file cannot drift onto different roots
//   [x] Happy (security): it is a sibling of the legacy file, never a parent or
//              child of it — a nested layout would make the legacy record an
//              entry the bounded directory scan has to reason about

func TestDocsServersDir_ResolvesUnderConfigDir(t *testing.T) {
	base := redirectConfigDir(t)

	got, err := DocsServersDir()
	if err != nil {
		t.Fatalf("DocsServersDir: %v", err)
	}
	want := filepath.Join(base, "forgectl", "docs-servers")
	if got != want {
		t.Errorf("DocsServersDir = %q, want %q", got, want)
	}
}

func TestDocsServersDir_SharesBaseWithLegacyPath(t *testing.T) {
	redirectConfigDir(t)

	dir, err := DocsServersDir()
	if err != nil {
		t.Fatalf("DocsServersDir: %v", err)
	}
	legacy, err := DocsServerPath()
	if err != nil {
		t.Fatalf("DocsServerPath: %v", err)
	}

	if filepath.Dir(dir) != filepath.Dir(legacy) {
		t.Errorf("DocsServersDir parent = %q, DocsServerPath parent = %q — both must derive from the same configDir() base or discovery and its legacy fallback drift onto different roots",
			filepath.Dir(dir), filepath.Dir(legacy))
	}
}

func TestDocsServersDir_IsSiblingOfLegacyRecord(t *testing.T) {
	redirectConfigDir(t)

	dir, err := DocsServersDir()
	if err != nil {
		t.Fatalf("DocsServersDir: %v", err)
	}
	legacy, err := DocsServerPath()
	if err != nil {
		t.Fatalf("DocsServerPath: %v", err)
	}

	rel, err := filepath.Rel(dir, legacy)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	// A path inside dir is expressible without climbing out of it; the legacy
	// record must need a ".." to reach, proving it is a sibling.
	if !strings.HasPrefix(rel, "..") {
		t.Errorf("legacy record %q resolves to %q inside the discovery directory %q — the bounded scan must never have to classify the legacy file as a candidate record", legacy, rel, dir)
	}
}
