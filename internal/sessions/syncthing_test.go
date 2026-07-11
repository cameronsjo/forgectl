package sessions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.xml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckSyncthingFolders(t *testing.T) {
	home := "/Users/fake"
	tests := []struct {
		name       string
		xml        string
		violations int
	}{
		{
			name:       "clean share passes",
			xml:        `<configuration><folder id="m" path="~/.claude-memory"></folder></configuration>`,
			violations: 0,
		},
		{
			name:       "exact JSONL root fails",
			xml:        `<configuration><folder id="bad" path="~/.claude/metrics"></folder></configuration>`,
			violations: 1,
		},
		{
			name:       "ancestor share fails (syncs the JSONL just as surely)",
			xml:        `<configuration><folder id="bad" path="~/.claude"></folder></configuration>`,
			violations: 2, // covers both metrics and cadence/sessions
		},
		{
			name:       "descendant share fails",
			xml:        `<configuration><folder id="bad" path="~/.claude/cadence/sessions/evidence"></folder></configuration>`,
			violations: 1,
		},
		{
			name: "defaults template folder is structurally excluded",
			xml: `<configuration>
				<folder id="m" path="~/.claude-memory"></folder>
				<defaults><folder id="" path="~/.claude/metrics"></folder></defaults>
			</configuration>`,
			violations: 0,
		},
		{
			name:       "multi-line folder element parses (XML, not line-based)",
			xml:        "<configuration><folder id=\"bad\"\n  path=\"~/.claude/metrics\"\n></folder></configuration>",
			violations: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckSyncthingFolders(writeConfig(t, tt.xml), home)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(got) != tt.violations {
				t.Errorf("got %d violations %v, want %d", len(got), got, tt.violations)
			}
		})
	}
}

func TestCheckSyncthingFoldersParseError(t *testing.T) {
	_, err := CheckSyncthingFolders(writeConfig(t, "not xml at all <"), "/Users/fake")
	if err == nil {
		t.Fatal("expected a parse error to surface (Sync fails open on it, but the error must exist)")
	}
}

func TestSyncFailsClosedOnSyncthingViolation(t *testing.T) {
	// The condition fails closed: a synced JSONL root aborts the run before
	// any ledger read or DB touch — even in dry-run.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cfg := writeConfig(t,
		`<configuration><folder id="bad" path="`+filepath.Join(home, ".claude", "metrics")+`"></folder></configuration>`)
	_, err = Sync(context.Background(), SyncOptions{
		Machine:         "m",
		MetricsDir:      filepath.Join("testdata", "metrics"),
		RunbooksDir:     filepath.Join("testdata", "runbooks"),
		SyncthingConfig: cfg,
		DryRun:          true,
	})
	if err == nil || !strings.Contains(err.Error(), "syncthing-blobs-only guard") {
		t.Fatalf("expected guard failure, got %v", err)
	}
}

func TestSyncFailsOpenOnUnreadableSyncthingConfig(t *testing.T) {
	// The guard's own fault fails open: an unparseable config warns and the
	// dry-run proceeds.
	cfg := writeConfig(t, "garbage <not-xml")
	rec, err := Sync(context.Background(), SyncOptions{
		Machine:         "m",
		MetricsDir:      filepath.Join("testdata", "metrics"),
		RunbooksDir:     filepath.Join("testdata", "runbooks"),
		SyncthingConfig: cfg,
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("guard fault must not block the sync: %v", err)
	}
	if rec.SessionsFound == 0 {
		t.Errorf("dry-run should still have counted sessions: %+v", rec)
	}
}
