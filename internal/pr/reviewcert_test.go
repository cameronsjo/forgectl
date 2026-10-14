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
//   [x] macOS: no usable bundle at all gives nothing

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
		name, goos, def, env string
		want                 []string
	}{
		{"linux", "linux", system, operator, nil},
		{"darwin, operator file", "darwin", system, operator, []string{"SSL_CERT_FILE=" + operator}},
		{"darwin, unset", "darwin", system, "", []string{"SSL_CERT_FILE=" + system}},
		{"darwin, missing operator file", "darwin", system, filepath.Join(dir, "gone.pem"), []string{"SSL_CERT_FILE=" + system}},
		{"darwin, relative operator file", "darwin", system, "corp.pem", []string{"SSL_CERT_FILE=" + system}},
		{"darwin, operator dir", "darwin", system, dir, []string{"SSL_CERT_FILE=" + system}},
		{"darwin, no bundle", "darwin", filepath.Join(dir, "none.pem"), "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCertPlatform(t, tc.goos, tc.def)
			t.Setenv("SSL_CERT_FILE", tc.env)
			if tc.env == "" {
				_ = os.Unsetenv("SSL_CERT_FILE")
			}
			if got := reviewCertEnv(); !slices.Equal(got, tc.want) {
				t.Errorf("reviewCertEnv = %v, want %v", got, tc.want)
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
