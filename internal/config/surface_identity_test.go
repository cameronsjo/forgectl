package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validIdentity = "[surface.identity]\napp_id = 123\ninstallation_id = 456\nbot_login = \"forgectl-worker[bot]\"\nbot_user_id = 789\n"

func TestSurfaceIdentity_AbsentMeansOperator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.intake]\nlabels = [\"queue:drain\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadPath(path)
	s, err := cfg.Surface.Identity.Resolve()
	if err != nil || s.Configured {
		t.Fatalf("absent table: %+v, %v", s, err)
	}
}

func TestSurfaceIdentity_Valid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(validIdentity), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("ValidatePath: %v", err)
	}
	cfg := LoadPath(path)
	if err := cfg.DecodeError(); err != nil {
		t.Fatal(err)
	}
	s, err := cfg.Surface.Identity.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := IdentitySettings{Configured: true, AppID: 123, InstallationID: 456, BotLogin: "forgectl-worker[bot]", BotUserID: 789, KeychainService: DefaultIdentityKeychainService}
	if s != want {
		t.Fatalf("got %+v, want %+v", s, want)
	}
}

func TestSurfaceIdentity_Refused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for name, c := range map[string]struct{ body, want string }{
		"empty table":       {"[surface.identity]\n", "app_id: required"},
		"no installation":   {"[surface.identity]\napp_id = 1\nbot_login = \"a[bot]\"\nbot_user_id = 2\n", "installation_id: required"},
		"negative user id":  {"[surface.identity]\napp_id = 1\ninstallation_id = 2\nbot_login = \"a[bot]\"\nbot_user_id = -3\n", "bot_user_id: required"},
		"human login":       {"[surface.identity]\napp_id = 1\ninstallation_id = 2\nbot_login = \"cameronsjo\"\nbot_user_id = 3\n", "bot_login"},
		"bad slug":          {"[surface.identity]\napp_id = 1\ninstallation_id = 2\nbot_login = \"-x[bot]\"\nbot_user_id = 3\n", "bot_login"},
		"underscore slug":   {"[surface.identity]\napp_id = 1\ninstallation_id = 2\nbot_login = \"cameron_sjo[bot]\"\nbot_user_id = 3\n", "bot_login"},
		"uppercase slug":    {"[surface.identity]\napp_id = 1\ninstallation_id = 2\nbot_login = \"Worker[bot]\"\nbot_user_id = 3\n", "bot_login"},
		"service with flag": {validIdentity + "keychain_service = \"-w\"\n", "keychain_service"},
		"service space":     {validIdentity + "keychain_service = \"a b\"\n", "keychain_service"},
		"misspelled key":    {validIdentity + "instalation_id = 4\n", "[surface.identity]: unknown key"},
	} {
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ValidatePath %v, want %q", name, err, c.want)
		}
		cfg := LoadPath(path)
		if err := cfg.DecodeError(); err == nil {
			t.Errorf("%s: LoadPath reported the file valid", name)
		}
		if _, err := cfg.Surface.Identity.Resolve(); err == nil {
			t.Errorf("%s: the kept section resolves, so a launch would go on", name)
		}
	}
}
