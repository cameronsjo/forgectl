package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidatePath_SurfacesLaunchProfileError mirrors
// TestValidatePath_SurfacesThemeError. Without it, `forgectl launch doctor`
// reports a healthy config while every launch, resume, and surface launch
// refuses it — the exact failure the doctor exists to prevent.
func TestValidatePath_SurfacesLaunchProfileError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[proxy]\nlaunch_profile = \"wrok\"\n[proxy.profiles.work]\nhttp_proxy = \"http://proxy.example:8080\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := ValidatePath(path)
	if !errors.Is(err, ErrUnknownLaunchProfile) {
		t.Fatalf("ValidatePath = %v, want ErrUnknownLaunchProfile", err)
	}
	// The profile NAME is what makes the error actionable, and a name is not
	// sensitive. The values are, and none may ride along.
	if !strings.Contains(err.Error(), `"wrok"`) {
		t.Errorf("ValidatePath = %q, want it to name the unresolvable profile", err)
	}
	if strings.Contains(err.Error(), "proxy.example") {
		t.Errorf("ValidatePath leaked a profile value: %q", err)
	}
}

// TestValidatePath_AcceptsAResolvableLaunchProfile is the negative control for
// the test above: without it, a Validate that rejected every launch_profile
// would look identical.
func TestValidatePath_AcceptsAResolvableLaunchProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[proxy]\nlaunch_profile = \"work\"\n[proxy.profiles.work]\n" +
		"http_proxy = \"http://proxy.example:8080\"\nno_proxy = \"localhost,127.0.0.1\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("ValidatePath = %v, want nil for a launch_profile that resolves", err)
	}
}

// TestLoad_TolerantOfAnUnresolvableLaunchProfile pins the split the other
// sections keep: Load stays tolerant so an unrelated command still runs, and
// only the validating surfaces — doctor, and the launch paths themselves —
// report it.
func TestLoad_TolerantOfAnUnresolvableLaunchProfile(t *testing.T) {
	body := "[proxy]\nlaunch_profile = \"wrok\"\n"
	cfg, err := DecodeStrict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if cfg.Proxy.LaunchProfile != "wrok" {
		t.Errorf("LaunchProfile = %q, want the unresolvable value decoded as-is", cfg.Proxy.LaunchProfile)
	}
}

// TestResolveLaunchProfile_RefusesAProxyWithNoBypassList pins the rule an
// earlier version of this feature got wrong in the opposite direction. It
// assumed removing an omitted no_proxy (rather than emptying it) protected
// loopback. Measured 2026-09-11, curl 8.7.1: absent and empty no_proxy behave
// identically, and NEITHER bypasses loopback — with http_proxy set,
// `http://localhost:9/` was dialed at the proxy under both, and direct only
// once no_proxy named localhost. So the removal bought determinism and nothing
// else, and the loopback hole needed closing here instead.
//
// The refusal is the closure: a profile cannot route traffic without saying
// what to exempt. Every launch path and `launch doctor` share it, so there is
// no surface on which this config looks viable.
func TestResolveLaunchProfile_RefusesAProxyWithNoBypassList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile ProxyProfile
		wantErr error
	}{
		{"http with no bypass", ProxyProfile{HTTPProxy: "http://p.example:8080"}, ErrLaunchProfileNoBypass},
		{"https with no bypass", ProxyProfile{HTTPSProxy: "http://p.example:8080"}, ErrLaunchProfileNoBypass},
		{"socks with no bypass", ProxyProfile{AllProxy: "socks5://p.example:1080"}, ErrLaunchProfileNoBypass},
		// A bypass list alone routes nothing, so there is nothing to exempt
		// from and no hole to close. It must not be caught by this rule.
		{"bypass list alone is fine", ProxyProfile{NoProxy: "localhost"}, nil},
		{"a proxy plus a bypass list is fine", ProxyProfile{HTTPProxy: "http://p.example:8080", NoProxy: "localhost"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := ProxyConfig{
				Profiles:      map[string]ProxyProfile{"p": tc.profile},
				LaunchProfile: "p",
			}
			if err := pc.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				return
			}
			// The message must be actionable without naming a value: an
			// operator has to know which field to add, and the URL that is
			// missing its exemption must not ride along.
			msg := pc.Validate().Error()
			if !strings.Contains(msg, "no_proxy") {
				t.Errorf("message = %q, want it to name the field to add", msg)
			}
			if strings.Contains(msg, "p.example") {
				t.Errorf("message leaked a profile value: %q", msg)
			}
		})
	}
}

// TestResolveLaunchProfile_RefusesCredentialsInAProxyURL pins the refusal of
// userinfo in a launch profile. The `pr` window path hands the launch
// environment to tmux as `-e KEY=VAL` arguments, so a password in a proxy URL
// would sit on argv, readable by every local user through ps. The shell
// protocol (`proxy use`) does not resolve a launch profile and is unaffected.
func TestResolveLaunchProfile_RefusesCredentialsInAProxyURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile ProxyProfile
		wantErr error
	}{
		{"user and password in http_proxy", ProxyProfile{HTTPProxy: "http://u:secretpw@p.example:8080", NoProxy: "localhost"}, ErrLaunchProfileCredentials}, //nolint:gosec // G101: a fake credential the refusal must catch
		{"user only in https_proxy", ProxyProfile{HTTPSProxy: "http://u@p.example:8080", NoProxy: "localhost"}, ErrLaunchProfileCredentials},
		{"userinfo in all_proxy", ProxyProfile{AllProxy: "socks5://u:secretpw@p.example:1080", NoProxy: "localhost"}, ErrLaunchProfileCredentials},
		// curl accepts a proxy with no scheme, so the check must too.
		{"userinfo with no scheme", ProxyProfile{HTTPProxy: "u:secretpw@p.example:8080", NoProxy: "localhost"}, ErrLaunchProfileCredentials},
		// Negative controls: a plain proxy URL, and an '@' in the bypass list,
		// which is not a URL and carries no credential.
		{"no userinfo is fine", ProxyProfile{HTTPProxy: "http://p.example:8080", NoProxy: "localhost"}, nil},
		{"no scheme and no userinfo is fine", ProxyProfile{HTTPProxy: "p.example:8080", NoProxy: "localhost"}, nil},
		{"an '@' in the bypass list is fine", ProxyProfile{HTTPProxy: "http://p.example:8080", NoProxy: "localhost,a@b.example"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := ProxyConfig{Profiles: map[string]ProxyProfile{"p": tc.profile}, LaunchProfile: "p"}
			_, _, err := pc.ResolveLaunchProfile()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveLaunchProfile = %v, want %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secretpw") {
				t.Errorf("message leaked the credential: %q", err)
			}
		})
	}
}

func TestResolveLaunchProfile(t *testing.T) {
	work := ProxyProfile{HTTPProxy: "http://proxy.example:8080", NoProxy: "localhost,127.0.0.1"}
	noBypass := ProxyProfile{HTTPProxy: "http://proxy.example:8080"}
	profiles := map[string]ProxyProfile{"work": work, "blank": {}, "nobypass": noBypass}

	for _, tc := range []struct {
		name    string
		named   string
		wantOK  bool
		wantErr error
	}{
		{name: "unset names nothing", named: "", wantOK: false},
		{name: "a configured profile resolves", named: "work", wantOK: true},
		{name: "a typo is refused", named: "wrok", wantErr: ErrUnknownLaunchProfile},
		{name: "a profile with no values is refused", named: "blank", wantErr: ErrEmptyLaunchProfile},
		{name: "a profile routing traffic with no bypass list is refused", named: "nobypass", wantErr: ErrLaunchProfileNoBypass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := ProxyConfig{Profiles: profiles, LaunchProfile: tc.named}
			profile, ok, err := pc.ResolveLaunchProfile()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && profile != work {
				t.Errorf("profile did not resolve to the named table")
			}
			if !ok && profile != (ProxyProfile{}) {
				t.Errorf("a refusal returned a populated profile")
			}
			// Validate is the doctor's door onto the same resolution; the two
			// must never disagree about whether a config is launchable.
			if gotValidate := pc.Validate(); !errors.Is(gotValidate, tc.wantErr) {
				t.Errorf("Validate = %v, want %v", gotValidate, tc.wantErr)
			}
		})
	}
}
