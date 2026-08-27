package cli

// Seam tests for the deployment-wide [github] host (forgectl#412): both
// host-sensitive command trees must fail LOUDLY — on every verb, aliases
// included — when the host is invalid or the config file failed to decode,
// and `projects list` must name a non-default host on stderr.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/review"
)

// degradedConfig loads a deliberately malformed config file through the real
// tolerant loader, producing a Config whose DecodeDegraded() is true — the
// unexported marker cannot be set from this package, by design.
func degradedConfig(t *testing.T) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("this = = broken ["), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.LoadPath(path)
	if !cfg.DecodeDegraded() {
		t.Fatal("fixture config did not come back decode-degraded")
	}
	return cfg
}

// runSeamVerb builds the module command for deps and executes one verb,
// returning the error.
func runSeamVerb(t *testing.T, m module.Manifest, deps module.Deps, args ...string) error {
	t.Helper()
	cmd := m.New(deps)
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

func TestProjectsSeam_FailsLoudlyOnBadGithubConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
	}{
		{"invalid host", config.Config{Github: config.GithubConfig{Host: "ghe.example.com:8443"}}},
		{"decode degraded", config.Config{}}, // replaced below
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if tc.name == "decode degraded" {
				cfg = degradedConfig(t)
			}
			deps := module.Deps{Cfg: cfg, Runner: &exec.FakeRunner{}}
			// Every verb — canonical and aliased — must surface the config
			// error, never "unknown command" and never a silent github.com
			// fallback.
			for _, verb := range [][]string{{"list"}, {"ls"}, {"pick"}, {"clone", "x/y"}, {"worktree", "x/y"}, {"wt", "x/y"}, {"pull-all"}} {
				err := runSeamVerb(t, projectsModule, deps, verb...)
				if err == nil || !strings.Contains(err.Error(), "invalid config") {
					t.Errorf("projects %v: err = %v, want the config error", verb, err)
				}
			}
		})
	}
}

func TestReviewSeam_FailsLoudlyOnBadGithubConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
	}{
		{"invalid host", config.Config{Github: config.GithubConfig{Host: "https://ghe.example.com"}}},
		{"decode degraded", config.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if tc.name == "decode degraded" {
				cfg = degradedConfig(t)
			}
			deps := module.Deps{Cfg: cfg, Runner: &exec.FakeRunner{}}
			for _, verb := range [][]string{nil, {"mark", "a/b#1"}, {"unmark", "a/b#1"}, {"sync"}} {
				err := runSeamVerb(t, reviewModule, deps, verb...)
				if err == nil || !strings.Contains(err.Error(), "invalid config") {
					t.Errorf("review %v: err = %v, want the config error", verb, err)
				}
			}
		})
	}
}

// TestReviewSeam_ConfigErrorNeverRendersHostValue: the failure is categorical
// — the hostile config value must not reach the error text.
func TestReviewSeam_ConfigErrorNeverRendersHostValue(t *testing.T) {
	hostile := "EVIL-$(id)://x"
	deps := module.Deps{Cfg: config.Config{Github: config.GithubConfig{Host: hostile}}, Runner: &exec.FakeRunner{}}
	err := runSeamVerb(t, reviewModule, deps)
	if err == nil {
		t.Fatal("want config error")
	}
	if strings.Contains(err.Error(), "EVIL") {
		t.Errorf("error %q renders the rejected host", err)
	}
}

// newHostNoteClient builds a projects client over a fake runner and an empty
// temp dir so `list` runs hermetically.
func newHostNoteClient(t *testing.T, host string) *projects.Client {
	t.Helper()
	t.Setenv("PROJECTS_DIR", t.TempDir())
	fake := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "gh" {
			return "[]", nil
		}
		return "", nil
	}}
	opts := []projects.Option{projects.WithGitHubOwners([]string{"acme"})}
	if host != "" {
		opts = append(opts, projects.WithGitHubHost(host))
	}
	return projects.New(fake, opts...)
}

func TestProjectsList_HostNoteOnNonDefaultHostOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host     string
		wantNote bool
	}{
		{"non-default host notes on stderr", "github.example.com", true},
		{"default host stays silent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newProjectsListCmd(newHostNoteClient(t, tc.host))
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("list: %v", err)
			}
			gotNote := strings.Contains(errOut.String(), "github host: github.example.com")
			if gotNote != tc.wantNote {
				t.Errorf("stderr note present=%v, want %v (stderr: %q)", gotNote, tc.wantNote, errOut.String())
			}
			// The --json stdout stays machine-clean either way.
			var rows []projects.Repo
			if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
				t.Errorf("--json stdout is not clean JSON: %v (stdout: %q)", err, out.String())
			}
		})
	}
}

// hostedFakeSource is a minimal review source exposing a Host(), standing in
// for Gitea in the activeHosts ordering assertion.
type hostedFakeSource struct {
	fakeReviewSource
	host string
}

func (h hostedFakeSource) Host() string { return h.host }

func TestActiveHosts_PrependsEffectiveHost(t *testing.T) {
	got := activeHosts("github.example.com", []review.Source{fakeReviewSource{}, hostedFakeSource{host: "git.sjo.lol"}})
	want := []string{"github.example.com", "git.sjo.lol"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("activeHosts = %v, want %v — the effective host prepended, host-less sources contributing nothing", got, want)
	}
}
