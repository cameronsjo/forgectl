package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttentionConfigDefaults(t *testing.T) {
	var c AttentionConfig
	if c.TTL() != 24*time.Hour || c.Prefix() != "cos-" || c.Validate() != nil {
		t.Errorf("defaults = %v %q %v", c.TTL(), c.Prefix(), c.Validate())
	}
}

func TestAttentionConfigLoadsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[attention]\nttl_hours = 6\nworker_prefix = \"w-\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := LoadPath(path).Attention
	if c.TTL() != 6*time.Hour || c.Prefix() != "w-" || c.Validate() != nil {
		t.Errorf("loaded = %v %q %v", c.TTL(), c.Prefix(), c.Validate())
	}
	zero, big, bad := 0, 721, "-x"
	for _, c := range []AttentionConfig{{TTLHours: &zero}, {TTLHours: &big}, {WorkerPrefix: &bad}} {
		if c.Validate() == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}
