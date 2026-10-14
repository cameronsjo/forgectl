package pr

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Test plan for reviewcert.go (forgectl#1253)
//
// reviewCertEnv (Classification: platform env construction)
//   [x] Off macOS: nothing
//   [x] macOS: an operator SSL_CERT_FILE naming a readable file wins
//   [x] macOS: an unusable operator value falls back to the system bundle
//   [x] macOS: no usable bundle unsets SSL_CERT_FILE in the window
//   [x] macOS: SSL_CERT_DIR is pinned to a usable operator dir, else unset

func stubCertPlatform(t *testing.T, goos, def string) {
	t.Helper()
	origGOOS, origDef := reviewCertGOOS, reviewCertDefault
	reviewCertGOOS, reviewCertDefault = goos, def
	t.Cleanup(func() { reviewCertGOOS, reviewCertDefault = origGOOS, origDef })
}

func TestReviewCertEnv(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(system, []byte("system"), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "corp.pem")
	if err := os.WriteFile(operator, []byte("corp"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, goos, def, env, dirEnv string
		want, wantUnset              []string
	}{
		{"linux", "linux", system, operator, dir, nil, nil},
		{"darwin, operator file", "darwin", system, operator, "", []string{"SSL_CERT_FILE=" + operator}, []string{"SSL_CERT_DIR"}},
		{"darwin, unset", "darwin", system, "", "", []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
		{"darwin, missing operator file", "darwin", system, filepath.Join(dir, "gone.pem"), "", []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
		{"darwin, relative operator file", "darwin", system, "corp.pem", "", []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
		{"darwin, operator dir as file", "darwin", system, dir, "", []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
		{"darwin, no bundle", "darwin", filepath.Join(dir, "none.pem"), "", "", nil, []string{"SSL_CERT_FILE", "SSL_CERT_DIR"}},
		{"darwin, no bundle, unusable operator file", "darwin", filepath.Join(dir, "none.pem"), "corp.pem", "", nil, []string{"SSL_CERT_FILE", "SSL_CERT_DIR"}},
		{"darwin, operator cert dir", "darwin", system, "", dir, []string{"SSL_CERT_FILE=" + system, "SSL_CERT_DIR=" + dir}, nil},
		{"darwin, relative cert dir", "darwin", system, "", "certs", []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
		{"darwin, cert dir is a file", "darwin", system, "", operator, []string{"SSL_CERT_FILE=" + system}, []string{"SSL_CERT_DIR"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCertPlatform(t, tc.goos, tc.def)
			t.Setenv("SSL_CERT_FILE", tc.env)
			if tc.env == "" {
				_ = os.Unsetenv("SSL_CERT_FILE")
			}
			t.Setenv("SSL_CERT_DIR", tc.dirEnv)
			if tc.dirEnv == "" {
				_ = os.Unsetenv("SSL_CERT_DIR")
			}
			got, unset := reviewCertEnv()
			if !slices.Equal(got, tc.want) || !slices.Equal(unset, tc.wantUnset) {
				t.Errorf("reviewCertEnv = %v, %v, want %v, %v", got, unset, tc.want, tc.wantUnset)
			}
		})
	}
}

// TestLaunchInline_PinsTheCertBundleOnMacOS: the window env carries the
// bundle on macOS, replacing one the window env source already had.
func TestLaunchInline_PinsTheCertBundleOnMacOS(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(system, []byte("system"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubCertPlatform(t, "darwin", system)
	t.Setenv("SSL_CERT_FILE", "")
	_ = os.Unsetenv("SSL_CERT_FILE")
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) { return []string{"SSL_CERT_FILE=/elsewhere.pem"}, nil }))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(t.Context(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "-e", "SSL_CERT_FILE="+system) {
		t.Errorf("tmux argv does not pin SSL_CERT_FILE to %s: %v", system, args)
	}
	if argPair(args, "-e", "SSL_CERT_FILE=/elsewhere.pem") {
		t.Errorf("the window env's own SSL_CERT_FILE survived beside the pin: %v", args)
	}
}

// TestLaunchInline_UnsetsAStaleCertBundleOnMacOS: with no usable bundle, the
// window must not keep an SSL_CERT_FILE from the tmux server's environment,
// so claude runs under env -u SSL_CERT_FILE.
func TestLaunchInline_UnsetsAStaleCertBundleOnMacOS(t *testing.T) {
	stubCertPlatform(t, "darwin", filepath.Join(t.TempDir(), "none.pem"))
	t.Setenv("SSL_CERT_FILE", "")
	_ = os.Unsetenv("SSL_CERT_FILE")
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) { return []string{"SSL_CERT_FILE=/stale.pem"}, nil }))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(t.Context(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if argPair(args, "-e", "SSL_CERT_FILE=/stale.pem") {
		t.Errorf("the stale SSL_CERT_FILE reached the window: %v", args)
	}
	if !argPair(args, "-u", "SSL_CERT_FILE") {
		t.Errorf("claude does not run with SSL_CERT_FILE unset: %v", args)
	}
}
