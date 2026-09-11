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
	body := "[proxy]\nlaunch_profile = \"work\"\n[proxy.profiles.work]\nhttp_proxy = \"http://proxy.example:8080\"\n"
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

func TestResolveLaunchProfile(t *testing.T) {
	work := ProxyProfile{HTTPProxy: "http://proxy.example:8080"}
	profiles := map[string]ProxyProfile{"work": work, "blank": {}}

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
