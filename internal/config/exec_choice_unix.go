//go:build unix

package config

import (
	"fmt"
	"io/fs"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// execChoiceVerdict says whether someone other than the user could have
// written the config file whose descriptor produced info, for a setting that
// picks an executable to run ([surface.rex] binary_path): the owner must be
// uid or root, and the file must not be writable by group or other. A symlink
// is judged by the file it reached, because info comes from the open
// descriptor. [surface.merge] asks for more (exactly 0600) because it grants
// authority; this setting only names a binary that later gates judge again.
// ACLs are not read, and the parent directory is not judged.
func execChoiceVerdict(path string, info fs.FileInfo, uid int) error {
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
