package pr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// PrepareLocal's failure path removes the findings dir it just made through
// the store handle, and nothing that is not a findings dir directly under the
// store (forgectl#685).
//
// Mutation that turns it red: restore `_ = os.RemoveAll(findingsDir)` in
// teardownLocalArtifacts (the outside dir is removed).
func TestTeardownLocalArtifacts_RemovesOnlyAStoreChild(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)

	own, err := os.MkdirTemp(store, findingsDirPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "findings.md"), []byte("draft"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.teardownLocalArtifacts(context.Background(), "", own)
	wantGone(t, own)

	outside := filepath.Join(t.TempDir(), findingsDirPrefix+"outside")
	mustMkdirUnmarked(t, outside)
	c.teardownLocalArtifacts(context.Background(), "", outside)
	wantKept(t, outside)
}
