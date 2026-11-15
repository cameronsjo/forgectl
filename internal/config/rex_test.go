// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRexConfigLoadsBinaryPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.rex]\nbinary_path = \"/opt/rex/bin/rex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadPath(path).Surface.Rex.BinaryPath; got != "/opt/rex/bin/rex" {
		t.Errorf("Rex.BinaryPath = %q", got)
	}
	if got := (Config{}).Surface.Rex.BinaryPath; got != "" {
		t.Errorf("zero value = %q, want empty", got)
	}
}
