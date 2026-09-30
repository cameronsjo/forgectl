package gitenv

import (
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// localEnvVars is `git rev-parse --local-env-vars` on git 2.43: what git
// clears before it works in another repository.
var localEnvVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT", "GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// Mutations: drop GIT_DIR from repositoryVars (kept); drop the
// GIT_CONFIG_KEY_ prefix check (kept); move GIT_ALLOW_PROTOCOL ahead of
// GIT_NO_LAZY_FETCH in localPins (not last); set it to "none" (wrong value).
func TestLocalEnvScrubsAndPins(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "GIT_CEILING_DIRECTORIES=/c", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=/evil", "GIT_ALLOW_PROTOCOL=ext:ssh", "GIT_NO_LAZY_FETCH=0"}
	for _, key := range localEnvVars {
		in = append(in, key+"=x")
	}
	got := Env(Local, in)
	for _, kv := range got {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(localEnvVars, key) || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			t.Errorf("Env(Local) kept %s", kv)
		}
	}
	var allow []string
	for _, kv := range got {
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") {
			allow = append(allow, kv)
		}
	}
	if !slices.Equal(allow, []string{"GIT_ALLOW_PROTOCOL="}) || got[len(got)-1] != "GIT_ALLOW_PROTOCOL=" {
		t.Errorf("GIT_ALLOW_PROTOCOL entries = %v with %q last; want only the empty GIT_ALLOW_PROTOCOL=, last", allow, got[len(got)-1])
	}
	if slices.Contains(got, "GIT_NO_LAZY_FETCH=0") {
		t.Errorf("Env(Local) kept an inherited GIT_NO_LAZY_FETCH=0: %v", got)
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "GIT_CEILING_DIRECTORIES=/c", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("Env(Local) dropped or lacks %s: %v", want, got)
		}
	}
}

// Transport keeps what the operator's own fetch authenticates with, and
// drops only what points git at another repository.
// Mutations: make Transport share Local's scrub (GIT_CONFIG_COUNT dropped);
// drop GIT_DIR from repositoryVars (kept).
func TestTransportEnvKeepsTheOperatorsTransport(t *testing.T) {
	keep := []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=store", "GIT_CONFIG_PARAMETERS='http.proxy'='p'", "GIT_ALLOW_PROTOCOL=https", "GIT_TERMINAL_PROMPT=1", "GIT_SSH_COMMAND=ssh -i k", "GIT_NO_REPLACE_OBJECTS=1"}
	drop := []string{"GIT_DIR=/o/.git", "GIT_WORK_TREE=/o", "GIT_INDEX_FILE=/o/.git/index", "GIT_COMMON_DIR=/o/.git", "GIT_OBJECT_DIRECTORY=/o/objects"}
	got := Env(Transport, append(slices.Clone(keep), drop...))
	if !slices.Equal(got, keep) {
		t.Errorf("Env(Transport) = %v, want %v", got, keep)
	}
}

// Mutations: drop "--no-replace-objects" or "protocol.allow=never" from
// localArgs; drop core.fsmonitor=false from transportArgs.
func TestArgs(t *testing.T) {
	if got, want := Args(Local), []string{"-c", "protocol.allow=never", "-c", "core.fsmonitor=false", "-c", "log.showSignature=false", "--no-replace-objects"}; !slices.Equal(got, want) {
		t.Errorf("Args(Local) = %v, want %v", got, want)
	}
	if got, want := Args(Transport), []string{"-c", "core.fsmonitor=false"}; !slices.Equal(got, want) {
		t.Errorf("Args(Transport) = %v, want %v", got, want)
	}
	var zero Profile
	if zero != Local {
		t.Error("the zero Profile is not Local: an unset profile must get the tightest hardening")
	}
	a := Args(Local)
	a[1] = "tampered"
	if Args(Local)[1] != "protocol.allow=never" {
		t.Error("Args returned the shared slice; a caller's edit reached the next call")
	}
}

// Run hands the Runner the profile's options ahead of the caller's
// arguments, the pins as overrides, and every scrubbed variable present in
// the environment as a removal.
// Mutation: make RunBin call r.RunWithEnvFiltered with args alone (no
// Args(p)), or with nil overrides.
func TestRunBuildsTheLocalCall(t *testing.T) {
	t.Setenv("GIT_DIR", "/other/.git")
	t.Setenv("GIT_CONFIG_KEY_7", "core.fsmonitor")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext")
	f := &exec.FakeRunner{}
	if _, err := Run(t.Context(), f, Local, "-C", "/repo", "status"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Name != "git" {
		t.Errorf("name = %q", c.Name)
	}
	if want := append(Args(Local), "-C", "/repo", "status"); !slices.Equal(c.Args, want) {
		t.Errorf("args = %v, want %v", c.Args, want)
	}
	for k, v := range map[string]string{"GIT_NO_LAZY_FETCH": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_ALLOW_PROTOCOL": ""} {
		if got, ok := c.Env[k]; !ok || got != v {
			t.Errorf("override %s = %q (set %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"GIT_DIR", "GIT_CONFIG_KEY_7", "GIT_ALLOW_PROTOCOL"} {
		if !slices.Contains(c.UnsetEnv, k) {
			t.Errorf("removals %v lack %s", c.UnsetEnv, k)
		}
	}
}

// Mutation: have Command skip cmd.Env (inherits GIT_DIR) or Args(p).
func TestCommandCarriesTheProfile(t *testing.T) {
	t.Setenv("GIT_DIR", "/other/.git")
	cmd := Command(t.Context(), Local, "rev-parse")
	if want := append([]string{"git"}, append(Args(Local), "rev-parse")...); !slices.Equal(cmd.Args, want) {
		t.Errorf("argv = %v, want %v", cmd.Args, want)
	}
	if slices.Contains(cmd.Env, "GIT_DIR=/other/.git") || cmd.Env[len(cmd.Env)-1] != "GIT_ALLOW_PROTOCOL=" {
		t.Errorf("env was not Local's: %v", cmd.Env)
	}
}
