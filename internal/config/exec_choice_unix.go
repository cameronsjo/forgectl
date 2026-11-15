//go:build unix

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// CheckExecChoiceFile refuses a config file that someone other than the user
// could have written, for a setting that picks an executable to run
// ([surface.rex] binary_path). It follows a symlink and judges the file it
// reaches: the owner must be this user or root, and the file must not be
// writable by group or other. A missing file passes; it cannot have set the
// value. [surface.merge] asks for more (exactly 0600) because it grants
// authority; this setting only names a binary that the signature gate then
// judges again.
func CheckExecChoiceFile(path string) error {
	return checkExecChoiceFile(path, os.Geteuid())
}

func checkExecChoiceFile(path string, uid int) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config file %s: cannot check its owner and mode: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("config file %s: cannot tell who owns it", termsafe.QuotePath(path))
	}
	if owner := int(st.Uid); owner != uid && owner != 0 {
		return fmt.Errorf("config file %s is owned by uid %d, not you or root", termsafe.QuotePath(path), owner)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("config file %s is writable by group or other (mode %04o)", termsafe.QuotePath(path), perm)
	}
	return nil
}
