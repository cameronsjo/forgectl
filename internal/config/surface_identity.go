package config

import (
	"errors"
	"fmt"
	"strings"
)

// SurfaceIdentityConfig is [surface.identity]: the GitHub App drain workers
// act as (openspec/changes/worker-github-app). With no table, workers run as
// the operator. With a table, every key but keychain_service is required, and
// a bad value is refused rather than falling back to the operator: a fallback
// would silently undo the identity.
type SurfaceIdentityConfig struct {
	AppID           int64  `toml:"app_id"`
	InstallationID  int64  `toml:"installation_id"`
	BotLogin        string `toml:"bot_login"`
	BotUserID       int64  `toml:"bot_user_id"`
	KeychainService string `toml:"keychain_service"`
	// set records that the table is present; unknown lists undecoded keys.
	// Both are filled by DecodeStrict.
	set     bool
	unknown []string
}

// DefaultIdentityKeychainService is where the App key lives when
// keychain_service is absent.
const DefaultIdentityKeychainService = "forgectl-worker-app"

// maxKeychainServiceLen bounds keychain_service; the value goes into argv.
const maxKeychainServiceLen = 64

// IdentitySettings is [surface.identity] resolved. Configured is false when
// the table is absent, and every other field is then zero.
type IdentitySettings struct {
	Configured      bool
	AppID           int64
	InstallationID  int64
	BotLogin        string
	BotUserID       int64
	KeychainService string
}

// CheckBotLogin refuses a value that is not a GitHub App bot login:
// "<slug>[bot]", the slug being lowercase letters, digits and '-', not
// starting with '-', as GitHub makes App slugs.
func CheckBotLogin(login string) error {
	bad := errors.New(`want a GitHub App bot login such as "forgectl-worker[bot]"`)
	slug, ok := strings.CutSuffix(login, "[bot]")
	if !ok || slug == "" || len(slug) > 100 || slug[0] == '-' {
		return bad
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return bad
		}
	}
	return nil
}

func checkKeychainService(s string) error {
	if s == "" || len(s) > maxKeychainServiceLen || s[0] == '-' {
		return fmt.Errorf("want 1-%d characters, not starting with '-'", maxKeychainServiceLen)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return errors.New("want letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

// Resolve reports the identity, refusing a present table that is incomplete
// or out of shape. It names the key, never a credential (the table holds
// none: the key stays in the keychain).
func (c SurfaceIdentityConfig) Resolve() (IdentitySettings, error) {
	if len(c.unknown) > 0 {
		return IdentitySettings{}, fmt.Errorf("[surface.identity]: unknown key %s; the keys are app_id, installation_id, bot_login, bot_user_id and keychain_service", quoteConfigValue(c.unknown[0]))
	}
	if !c.set {
		return IdentitySettings{}, nil
	}
	for _, f := range []struct {
		key string
		v   int64
	}{{"app_id", c.AppID}, {"installation_id", c.InstallationID}, {"bot_user_id", c.BotUserID}} {
		if f.v <= 0 {
			return IdentitySettings{}, fmt.Errorf("[surface.identity] %s: required, a positive integer", f.key)
		}
	}
	if err := CheckBotLogin(c.BotLogin); err != nil {
		return IdentitySettings{}, fmt.Errorf("[surface.identity] bot_login: %w, got %s", err, quoteConfigValue(c.BotLogin))
	}
	service := c.KeychainService
	if service == "" {
		service = DefaultIdentityKeychainService
	} else if err := checkKeychainService(service); err != nil {
		return IdentitySettings{}, fmt.Errorf("[surface.identity] keychain_service: %w, got %s", err, quoteConfigValue(service))
	}
	return IdentitySettings{
		Configured:      true,
		AppID:           c.AppID,
		InstallationID:  c.InstallationID,
		BotLogin:        c.BotLogin,
		BotUserID:       c.BotUserID,
		KeychainService: service,
	}, nil
}

// Validate reports the first out-of-shape [surface.identity] value.
func (c SurfaceIdentityConfig) Validate() error {
	_, err := c.Resolve()
	return err
}
