package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAttentionConfigDefaults(t *testing.T) {
	var c AttentionConfig
	if c.Prefix() != "cos-" || c.Validate() != nil {
		t.Errorf("defaults = %q %v", c.Prefix(), c.Validate())
	}
}

// ttl_hours belonged to the retired session spool. A config that still sets
// it loads, and the key does nothing.
func TestAttentionConfigLoadsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[attention]\nttl_hours = 6\nworker_prefix = \"w-\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := LoadPath(path).Attention
	if c.Prefix() != "w-" || c.Validate() != nil {
		t.Errorf("loaded = %q %v", c.Prefix(), c.Validate())
	}
	bad := "-x"
	if c := (AttentionConfig{WorkerPrefix: &bad}); c.Validate() == nil {
		t.Errorf("%+v was accepted", c)
	}
}
