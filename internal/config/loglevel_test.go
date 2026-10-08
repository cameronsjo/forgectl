package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acceptedLogLevels is every spelling the logger has always acted on, plus
// the ways of saying "off": absent, empty, blank, and the word itself. Case
// and surrounding space never mattered to the logger, so they do not matter
// to the check.
var acceptedLogLevels = []string{
	"", "  ", "off", "OFF", "debug", "Debug", "info", "warn", "warning", " WARNING ", "error",
}

func TestDecodeStrict_AcceptsEveryLogLevelTheLoggerKnows(t *testing.T) {
	if _, err := DecodeStrict([]byte("[net]\nprobe_host = \"example.com\"\n")); err != nil {
		t.Fatalf("DecodeStrict(no log_level) = %v, want nil", err)
	}
	for _, level := range acceptedLogLevels {
		cfg, err := DecodeStrict([]byte("log_level = " + quoteConfigValue(level) + "\n"))
		if err != nil {
			t.Errorf("DecodeStrict(log_level = %q) = %v, want nil", level, err)
			continue
		}
		if cfg.LogLevel != level {
			t.Errorf("DecodeStrict(log_level = %q) kept %q, want the value as written", level, cfg.LogLevel)
		}
	}
}

// TestDecodeStrict_RefusesAMistypedLogLevel: a level the logger does not know
// used to take the same branch as "off", so a typo turned logging off and
// nothing said so.
func TestDecodeStrict_RefusesAMistypedLogLevel(t *testing.T) {
	cfg, err := DecodeStrict([]byte("log_level = \"degub\"\n"))
	if err == nil {
		t.Fatal(`DecodeStrict(log_level = "degub") = nil, want an error`)
	}
	for _, want := range []string{"log_level", `"degub"`, "off", "debug", "info", "warn", "error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q after a refused value, want it dropped", cfg.LogLevel)
	}
}

// TestDecodeStrict_ABadLogLevelDoesNotHideABadAllowedHost: both values are
// dropped, whichever error is reported, so neither survives into a Config a
// caller goes on to use.
func TestDecodeStrict_ABadLogLevelDoesNotKeepABadAllowedHost(t *testing.T) {
	cfg, err := DecodeStrict([]byte("log_level = \"loud\"\n" + badTasksHostsTOML))
	if err == nil {
		t.Fatal("DecodeStrict(two refused values) = nil, want an error")
	}
	if cfg.LogLevel != "" || len(cfg.Tasks.AllowedHosts) != 0 {
		t.Errorf("LogLevel = %q, AllowedHosts = %q, want both dropped", cfg.LogLevel, cfg.Tasks.AllowedHosts)
	}
}

func TestLoadPath_AMistypedLogLevelIsALoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("log_level = \"degub\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadPath(path)
	err := cfg.DecodeError()
	if err == nil {
		t.Fatal("DecodeError() = nil after loading a mistyped log_level")
	}
	for _, want := range []string{"log_level", "config.toml", "not valid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("load error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "does not parse") {
		t.Errorf("load error %q calls a file that parsed unparseable", err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "log_level") {
		t.Errorf("ValidatePath = %v, want an error naming the key", err)
	}
}

func TestDescribe_ReportsAMistypedLogLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("log_level = \"degub\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, rep := describeFile(path)
	if rep.InvalidErr == nil || !strings.Contains(rep.InvalidErr.Error(), "log_level") {
		t.Errorf("InvalidErr = %v, want an error naming the key", rep.InvalidErr)
	}
	if rep.DecodeErr != nil {
		t.Errorf("DecodeErr = %v for a file that parsed in full, want nil", rep.DecodeErr)
	}
	if !rep.IsRefused("log_level") || rep.IsRefused("tasks.allowed_hosts") {
		t.Errorf("Refused = %v, want log_level only", rep.Refused)
	}
	if cfg.LogLevel != "degub" {
		t.Errorf("LogLevel = %q in the report, want the file's own value so the operator can see it", cfg.LogLevel)
	}

	for _, level := range acceptedLogLevels {
		if err := os.WriteFile(path, []byte("log_level = "+quoteConfigValue(level)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, rep := describeFile(path); rep.InvalidErr != nil || len(rep.Refused) != 0 {
			t.Errorf("log_level = %q: report = %+v, want nothing refused", level, rep)
		}
	}
}
