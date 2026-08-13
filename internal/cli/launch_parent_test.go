package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureConfigParentDurable_CreatesPrivateAncestorsInDurabilityOrder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "config.toml")
	ops := nativeConfigParentOps()
	var trace []string
	nativeMkdir := ops.mkdir
	nativeSync := ops.syncDir
	ops.mkdir = func(path string, mode os.FileMode) error {
		trace = append(trace, "mkdir:"+filepath.Base(path))
		return nativeMkdir(path, mode)
	}
	ops.syncDir = func(path string) error {
		trace = append(trace, "sync:"+filepath.Base(path))
		return nativeSync(path)
	}
	created, err := ensureConfigParentDurable(path, ops)
	if err != nil {
		t.Fatalf("ensureConfigParentDurable() error = %v", err)
	}
	wantCreated := []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b")}
	if !reflect.DeepEqual(created, wantCreated) {
		t.Fatalf("created = %v, want %v", created, wantCreated)
	}
	wantTrace := []string{"mkdir:a", "sync:a", "sync:" + filepath.Base(root), "mkdir:b", "sync:b", "sync:a"}
	if !reflect.DeepEqual(trace, wantTrace) {
		t.Fatalf("trace = %v, want %v", trace, wantTrace)
	}
	for _, dir := range created {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %04o, want private", dir, info.Mode().Perm())
		}
	}
}

func TestEnsureConfigParentDurable_RacedExistingDirectoryStillGetsDurabilityProof(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "config.toml")
	ops := nativeConfigParentOps()
	nativeMkdir := ops.mkdir
	nativeSync := ops.syncDir
	var trace []string
	ops.mkdir = func(path string, mode os.FileMode) error {
		if err := nativeMkdir(path, mode); err != nil {
			return err
		}
		return fs.ErrExist
	}
	ops.syncDir = func(path string) error {
		trace = append(trace, path)
		return nativeSync(path)
	}
	created, err := ensureConfigParentDurable(path, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("created=%v, want no directories owned by this attempt", created)
	}
	want := []string{filepath.Join(root, "a"), root}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("sync trace=%v, want raced directory then parent %v", trace, want)
	}
}

func TestEnsureConfigParentDurable_ReportsResidueAndStopsOnSyncFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "config.toml")
	ops := nativeConfigParentOps()
	wantErr := errors.New("injected sync")
	ops.syncDir = func(path string) error {
		if filepath.Base(path) == "a" {
			return wantErr
		}
		return nil
	}
	created, err := ensureConfigParentDurable(path, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want injected sync", err)
	}
	if !reflect.DeepEqual(created, []string{filepath.Join(root, "a")}) {
		t.Fatalf("created = %v, want only a", created)
	}
	if _, err := os.Lstat(filepath.Join(root, "a", "b")); !os.IsNotExist(err) {
		t.Fatalf("b exists after earlier sync failure: %v", err)
	}
}

func TestEnsureConfigParentDurable_AncestorParentSyncFailureStopsAtExactResidue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "config.toml")
	ops := nativeConfigParentOps()
	nativeSync := ops.syncDir
	wantErr := errors.New("injected ancestor parent sync")
	ops.syncDir = func(path string) error {
		if path == root {
			return wantErr
		}
		return nativeSync(path)
	}
	created, err := ensureConfigParentDurable(path, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want injected sync", err)
	}
	if !reflect.DeepEqual(created, []string{filepath.Join(root, "a")}) {
		t.Fatalf("created=%v, want only a", created)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err != nil {
		t.Fatalf("created residue missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a", "b")); !os.IsNotExist(err) {
		t.Fatalf("later directory exists: %v", err)
	}
}
