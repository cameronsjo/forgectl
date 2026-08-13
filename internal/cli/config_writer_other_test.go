//go:build !unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigWriterOther_VisibleReplacementReportsUnsupportedDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	action, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
		return append(bytes.Clone(raw), []byte("no_icons = true\n")...), nil
	})
	if action != configWritten || !errors.Is(err, errDirectoryDurabilityUnsupported) {
		t.Fatalf("action=%v error=%v, want visible write with explicit unsupported durability", action, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "no_icons = true\n" {
		t.Fatalf("visible config=%q error=%v", got, readErr)
	}
}
