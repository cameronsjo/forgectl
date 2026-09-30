package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
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

func gateRoot() *cobra.Command {
	return newRoot(module.Deps{Runner: &exec.FakeRunner{}})
}

func TestConfigParseGate_RefusesMostCommands(t *testing.T) {
	cfg := loadMalformedConfig(t)
	root := gateRoot()
	for _, args := range [][]string{
		{"docs", "list", "--json"},
		{"docs", "check", "--json"},
		{"docs", "list", "--", "--help"},
		{"tmux", "ls"},
		{"--no-icons", "pr", "list"},
		{},
		{"init"},
		{"launch", "which"},
		{"launch", "init"},
		{"launch", "--", "--help"},
		{"cl", "stats"},
	} {
		err := configParseGate(cfg, root, args)
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
	root := gateRoot()
	for _, args := range [][]string{
		{"config"}, {"cfg"}, {"cfg", "--json"}, {"config", "--json"}, {"doctor", "--json"},
		{"--no-icons", "doctor"}, {"help"}, {"completion", "bash"}, {"version"},
		{"docs", "--help"}, {"docs", "list", "-h"}, {"--version"}, {"-h"},
		{"launch", "edit"}, {"cl", "doctor"}, {"launch", "--help"},
	} {
		if err := configParseGate(cfg, root, args); err != nil {
			t.Errorf("args %q: recovery path was refused: %v", args, err)
		}
	}
}

// TestConfigParseGate_AliasesFollowTheirCommand sweeps every registered
// command and alias: an alias must get exactly its command's verdict, so an
// exempt verb's shorthand passes and every other alias stays gated.
func TestConfigParseGate_AliasesFollowTheirCommand(t *testing.T) {
	cfg := loadMalformedConfig(t)
	root := gateRoot()
	sawExemptAlias := false
	for _, cmd := range root.Commands() {
		want := configParseGate(cfg, root, []string{cmd.Name()}) == nil
		if cmd.Name() == "launch" {
			continue // bare launch is gated; its own-verbs are covered above
		}
		for _, alias := range cmd.Aliases {
			got := configParseGate(cfg, root, []string{alias}) == nil
			if got != want {
				t.Errorf("alias %q of %q: exempt=%t, but the command itself is exempt=%t", alias, cmd.Name(), got, want)
			}
			if want {
				sawExemptAlias = true
			}
		}
	}
	if !sawExemptAlias {
		t.Error("sweep found no alias of an exempt verb; the cfg alias should be one")
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
		if err := configParseGate(cfg, gateRoot(), []string{"docs", "list"}); err != nil {
			t.Errorf("%s config: gate refused: %v", name, err)
		}
	}
}

// TestExecute_MalformedConfigExitsTwo drives the real Execute wiring: process
// argv, the real config load, the gate, and the stderr line.
func TestExecute_MalformedConfigExitsTwo(t *testing.T) {
	// os.UserConfigDir is where forgectl looks: $HOME/.config on Linux but
	// $HOME/Library/Application Support on darwin. Ask it rather than
	// hard-coding one layout, or the config is never found off Linux and the
	// command runs against defaults.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	cfgDir := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("this = = broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "docs", "list", "--json")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = w
	execErr := Execute(context.Background())
	os.Stderr = prevStderr
	_ = w.Close()
	out, _ := io.ReadAll(r)

	if got := ExitCode(execErr); got != 2 {
		t.Fatalf("Execute exit code = %d (err %v), want 2", got, execErr)
	}
	if !strings.Contains(string(out), "config.toml") || !strings.Contains(string(out), "line 1, column") {
		t.Errorf("stderr must name the file and position, got %q", out)
	}
}

// TestConfigParseGate_UnreadableConfig is forgectl#684: a config.toml that
// exists but cannot be read (here, a directory in its place) gets the same
// gate as one that does not parse — refused for ordinary commands, let
// through for the recovery verbs — instead of silently running on defaults.
func TestConfigParseGate_UnreadableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.LoadPath(path)
	root := gateRoot()
	for _, args := range [][]string{{"docs", "list", "--json"}, {"tmux", "ls"}, {}} {
		err := configParseGate(cfg, root, args)
		if err == nil {
			t.Errorf("args %q: gate passed an unreadable config", args)
			continue
		}
		if !strings.Contains(err.Error(), "config.toml") || !strings.Contains(err.Error(), "cannot be read: not a regular file") {
			t.Errorf("args %q: error must name the file and the reason, got %q", args, err)
		}
	}
	for _, args := range [][]string{{"config"}, {"doctor", "--json"}, {"launch", "edit"}, {"version"}} {
		if err := configParseGate(cfg, root, args); err != nil {
			t.Errorf("args %q: recovery path was refused: %v", args, err)
		}
	}
}
