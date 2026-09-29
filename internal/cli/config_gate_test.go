package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func loadMalformedConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("log_level = \"info\"\nthis = = broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.LoadPath(path)
	if cfg.DecodeError() == nil {
		t.Fatal("fixture did not fail to parse")
	}
	return cfg
}

func TestConfigParseGate_RefusesMostCommands(t *testing.T) {
	cfg := loadMalformedConfig(t)
	for _, args := range [][]string{
		{"docs", "list", "--json"},
		{"docs", "check", "--json"},
		{"tmux", "ls"},
		{"--no-icons", "pr", "list"},
		{},
		{"launch", "which"},
		{"launch", "--", "--help"},
		{"cl", "stats"},
	} {
		err := configParseGate(cfg, args)
		if err == nil {
			t.Errorf("args %q: gate passed a malformed config", args)
			continue
		}
		if !strings.Contains(err.Error(), "config.toml") || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("args %q: error must name the file and the position, got %q", args, err)
		}
	}
}

func TestConfigParseGate_RecoveryVerbsStillRun(t *testing.T) {
	cfg := loadMalformedConfig(t)
	for _, args := range [][]string{
		{"config"}, {"config", "--json"}, {"init"}, {"doctor", "--json"},
		{"--no-icons", "doctor"}, {"help"}, {"completion", "bash"}, {"version"},
		{"docs", "--help"}, {"--version"}, {"-h"},
		{"launch", "init"}, {"launch", "edit"}, {"cl", "doctor"}, {"launch", "--help"},
	} {
		if err := configParseGate(cfg, args); err != nil {
			t.Errorf("args %q: recovery path was refused: %v", args, err)
		}
	}
}

func TestConfigParseGate_ValidOrAbsentConfigPasses(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(good, []byte("[github]\nhost = \"github.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]config.Config{
		"valid":  config.LoadPath(good),
		"absent": config.LoadPath(filepath.Join(dir, "missing.toml")),
	} {
		if err := configParseGate(cfg, []string{"docs", "list"}); err != nil {
			t.Errorf("%s config: gate refused: %v", name, err)
		}
	}
}

func TestConfigParseGate_ExitsTwo(t *testing.T) {
	err := WithExitCode(configParseGate(loadMalformedConfig(t), []string{"docs", "list"}), 2)
	if got := ExitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if errors.Unwrap(err) == nil {
		t.Error("gate error lost its chain")
	}
}
