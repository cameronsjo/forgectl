package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteConfigAtomic_PrivateModesAndLinkSemantics(t *testing.T) {
	t.Run("fresh is private", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		state, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps())
		if err != nil || state != commitDurable {
			t.Fatalf("write = state %v, error %v; want durable", state, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("mode = %04o, want no group/other bits", info.Mode().Perm())
		}
	})

	t.Run("existing stricter mode is not broadened", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("old"), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps()); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o400 {
			t.Fatalf("mode = %04o, want 0400", info.Mode().Perm())
		}
	})

	t.Run("leaf symlink target remains unchanged", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps()); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(target); string(got) != "target" {
			t.Fatalf("symlink target = %q, want unchanged", got)
		}
		if info, _ := os.Lstat(path); !info.Mode().IsRegular() {
			t.Fatalf("replacement mode = %v, want regular", info.Mode())
		}
	})

	t.Run("hardlink sibling remains unchanged", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		sibling := filepath.Join(dir, "sibling")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, sibling); err != nil {
			t.Fatal(err)
		}
		if _, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps()); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(sibling); string(got) != "old" {
			t.Fatalf("hardlink sibling = %q, want old", got)
		}
	})

	t.Run("nonregular destination refuses", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		state, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps())
		if !errors.Is(err, errConfigDestinationNonRegular) || state != commitNone {
			t.Fatalf("write = state %v, error %v; want commitNone/nonregular", state, err)
		}
	})
}

func TestWriteConfigAtomic_ReportsVisibleRenameBeforeDirectoryDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	ops := nativeAtomicWriteOps()
	ops.syncDir = func(string) error { return errors.New("injected directory sync") }
	state, err := writeConfigAtomicWithOps(path, []byte("new"), ops)
	if state != commitRenamed || err == nil {
		t.Fatalf("write = state %v, error %v; want renamed/error", state, err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "new" {
		t.Fatalf("visible bytes = %q, error %v; want new", got, readErr)
	}
}

func TestWriteConfigAtomic_PreservesEveryExistingModeNoBroaderThan0600(t *testing.T) {
	for _, original := range []os.FileMode{0o666, 0o644, 0o600, 0o400, 0o200, 0o000} {
		t.Run(original.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, original); err != nil {
				t.Fatal(err)
			}
			state, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps())
			if err != nil || state != commitDurable {
				t.Fatalf("state=%v error=%v", state, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if want := original.Perm() & 0o600; info.Mode().Perm() != want {
				t.Fatalf("mode=%04o, want %04o from existing %04o", info.Mode().Perm(), want, original)
			}
		})
	}
}

func TestWriteConfigAtomic_FailureMatrixReportsExactVisibility(t *testing.T) {
	tests := []struct {
		name      string
		inject    func(*atomicWriteOps)
		wantState commitState
		wantBytes string
	}{
		{name: "lstat", inject: func(ops *atomicWriteOps) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("injected lstat") }
		}},
		{name: "create temp", inject: func(ops *atomicWriteOps) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errors.New("injected create") }
		}},
		{name: "write", inject: func(ops *atomicWriteOps) {
			ops.writeAll = func(*os.File, []byte) error { return errors.New("injected write") }
		}},
		{name: "mode", inject: func(ops *atomicWriteOps) {
			ops.chmodFile = func(*os.File, os.FileMode) error { return errors.New("injected mode") }
		}},
		{name: "file sync", inject: func(ops *atomicWriteOps) { ops.syncFile = func(*os.File) error { return errors.New("injected sync") } }},
		{name: "close", inject: func(ops *atomicWriteOps) {
			ops.closeFile = func(*os.File) error { return errors.New("injected close") }
		}},
		{name: "rename", inject: func(ops *atomicWriteOps) {
			ops.rename = func(string, string) error { return errors.New("injected rename") }
		}},
		{name: "parent sync", wantState: commitRenamed, wantBytes: "new", inject: func(ops *atomicWriteOps) {
			ops.syncDir = func(string) error { return errors.New("injected parent sync") }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := nativeAtomicWriteOps()
			tt.inject(&ops)
			state, err := writeConfigAtomicWithOps(path, []byte("new"), ops)
			if err == nil || state != tt.wantState {
				t.Fatalf("state=%v error=%v, want state=%v/error", state, err, tt.wantState)
			}
			want := tt.wantBytes
			if want == "" {
				want = "old"
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != want {
				t.Fatalf("visible bytes=%q error=%v, want %q", got, readErr, want)
			}
		})
	}
}

func TestWriteConfigAtomic_TempCleanupFailureMatrixReportsResidue(t *testing.T) {
	tests := []struct {
		name        string
		inject      func(*atomicWriteOps)
		wantResidue bool
	}{
		{name: "cleanup unlink", wantResidue: true, inject: func(ops *atomicWriteOps) {
			ops.writeAll = func(*os.File, []byte) error { return errors.New("injected write") }
			ops.remove = func(string) error { return errors.New("injected cleanup unlink") }
		}},
		{name: "cleanup parent sync", inject: func(ops *atomicWriteOps) {
			ops.writeAll = func(*os.File, []byte) error { return errors.New("injected write") }
			ops.syncDir = func(string) error { return errors.New("injected cleanup parent sync") }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			ops := nativeAtomicWriteOps()
			tt.inject(&ops)
			state, err := writeConfigAtomicWithOps(path, []byte("new"), ops)
			if state != commitNone || err == nil {
				t.Fatalf("state=%v error=%v, want commitNone/error", state, err)
			}
			entries, globErr := filepath.Glob(filepath.Join(dir, ".config-*.toml.tmp"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if (len(entries) != 0) != tt.wantResidue {
				t.Fatalf("temp residue=%v, want residue=%t", entries, tt.wantResidue)
			}
		})
	}
}
