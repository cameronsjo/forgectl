//go:build unix && !aix && !illumos && !solaris

package audit

import (
	"io/fs"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }

// ownedInfo is an Lstat result whose owner is replaced.
type ownedInfo struct {
	fs.FileInfo
	st *syscall.Stat_t
}

func (o ownedInfo) Sys() any { return o.st }

// TestScanSecrets_ForeignOwner: a key owned by a uid other than the
// scanner's is flagged foreign-owner; one owned by the scanner, or a .env
// owned by anyone, is not.
//
// Mutation that turns it red: drop the uid comparison in finding (no key
// is then flagged), or apply it to every kind (the .env row is flagged).
func TestScanSecrets_ForeignOwner(t *testing.T) {
	root := t.TempDir()
	writeMode(t, root, "theirs/id_rsa", pemKey, 0o600)
	writeMode(t, root, "theirs/.env", "A=1", 0o600)
	writeMode(t, root, "mine/id_rsa", pemKey, 0o600)
	euid := currentEUID()
	foreign := uint32(4242)
	if euid == 4242 {
		foreign = 4243
	}
	r, err := scanSecretsWithRoot(t, root, func(ops *fsOps) {
		inner := ops.lstat
		ops.lstat = func(name string) (fs.FileInfo, error) {
			info, err := inner(name)
			if err != nil || filepath.Dir(name) != "theirs" {
				return info, err
			}
			st := *info.Sys().(*syscall.Stat_t)
			st.Uid = foreign
			return ownedInfo{FileInfo: info, st: &st}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByPath(r)
	if !slices.Contains(got[filepath.Join(root, "theirs/id_rsa")].Flags, FlagForeignOwner) {
		t.Errorf("theirs/id_rsa flags = %v, want foreign-owner", got[filepath.Join(root, "theirs/id_rsa")].Flags)
	}
	if slices.Contains(got[filepath.Join(root, "mine/id_rsa")].Flags, FlagForeignOwner) {
		t.Error("a key the scanner owns was flagged foreign-owner")
	}
	if slices.Contains(got[filepath.Join(root, "theirs/.env")].Flags, FlagForeignOwner) {
		t.Error("foreign-owner is a key flag; a .env carried it")
	}
}
