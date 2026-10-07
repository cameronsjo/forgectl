// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package skill embeds forgectl's agent skill (SKILL.md plus references/) so
// the binary is the only source of it: `forgectl --skill` prints it and
// `forgectl --skill --install <dir>` writes it out. The text ships in the
// build that documents the commands, so the two cannot drift apart across
// releases; skill_test.go in internal/cli holds them together within one.
package skill

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

//go:embed all:skill
var embedded embed.FS

// Entry is the skill's top file, the one `forgectl --skill` prints.
const Entry = "SKILL.md"

// FS returns the embedded skill tree rooted at SKILL.md.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "skill")
	if err != nil {
		// The embed directive guarantees the directory; reaching this means a
		// build that dropped it.
		panic(fmt.Sprintf("skill: embedded tree missing: %v", err))
	}
	return sub
}

// Text returns SKILL.md.
func Text() string {
	b, err := fs.ReadFile(FS(), Entry)
	if err != nil {
		panic(fmt.Sprintf("skill: embedded %s missing: %v", Entry, err))
	}
	return string(b)
}

// Install writes the skill (SKILL.md and references/) into dir and returns the
// slash-separated paths it wrote, relative to dir. dir must be absolute and
// already exist: a typo'd path is refused, not created. Every write goes
// through an os.Root on dir, so a symlink inside dir that points elsewhere
// cannot carry a write outside it. Files that already exist under dir are
// replaced (by rename, so a hardlink is replaced, not written through); nothing
// else in dir is touched.
func Install(dir string) ([]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("--install needs an absolute directory, got %q", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("--install directory %q does not exist; create it first", dir)
		}
		return nil, fmt.Errorf("--install directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--install path %q is not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("--install directory %q: %w", dir, err)
	}
	defer func() { _ = root.Close() }() // read-only use of the handle; nothing to flush

	src := FS()
	var written []string
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if err := root.MkdirAll(p, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", path.Join(dir, p), err)
			}
			return nil
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		if err := writeFile(root, p, data); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, filepath.FromSlash(p)), err)
		}
		written = append(written, p)
		return nil
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// writeFile replaces name inside root with data. It writes a sibling temp file
// and renames it over the target, so a hardlink planted at name is replaced as
// a directory entry rather than written through. A failed close is reported: a
// write that did not flush is a write that did not happen.
func writeFile(root *os.Root, name string, data []byte) (err error) {
	tmp := name + ".forgectl-tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		_ = root.Remove(tmp) // best effort; the write error is the one to report
	}
	return err
}
