package launch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// stubBinary writes an executable stub named name under a fresh temp dir and
// returns its full path.
func stubBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	// #nosec G306 -- resolution is what is under test; the stub must be executable.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	return path
}

// clearBinEnv unsets both binary overrides so a test reads the layer it means
// to. An ambient FORGECTL_CLAUDE_BIN on the developer's machine would otherwise
// win over every config and PATH case here.
func clearBinEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FORGECTL_CLAUDE_BIN", "")
	t.Setenv("FORGECTL_CODEX_BIN", "")
}

// resolutionStubs lays out the three locations a harness binary can be
// resolved from: an off-PATH claude and codex for the env and config layers,
// and a directory holding both that a test points $PATH at. Every layer of
// every harness is reachable from this one fixture, so a resolution test picks
// which one it means rather than building its own.
func resolutionStubs(t *testing.T) (claudeBin, codexBin, pathDir string) {
	t.Helper()
	claudeBin = stubBinary(t, "claude")
	codexBin = stubBinary(t, "codex")
	pathDir = t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		// #nosec G306 -- see stubBinary.
		if err := os.WriteFile(filepath.Join(pathDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write PATH stub %s: %v", name, err)
		}
	}
	return claudeBin, codexBin, pathDir
}

// TestResolveBinary_ReportsProvenance pins the source category beside the path
// for every layer of both harnesses. The category is what the surface policy
// (forgectl#331) gates on — it accepts an env/config selection as an operator
// assertion and refuses a PATH hit by default — so a resolver that returned the
// right path under the wrong label would hand that policy a decision it cannot
// make correctly.
func TestResolveBinary_ReportsProvenance(t *testing.T) {
	claudeBin, codexBin, pathDir := resolutionStubs(t)

	tests := []struct {
		name       string
		harness    string
		env        map[string]string
		defaults   config.LaunchDefaults
		wantPath   string
		wantSource BinarySource
	}{
		{
			name:       "claude env override",
			harness:    "claude",
			env:        map[string]string{"FORGECTL_CLAUDE_BIN": claudeBin},
			defaults:   config.LaunchDefaults{BinaryPath: filepath.Join(pathDir, "claude")},
			wantPath:   claudeBin,
			wantSource: BinaryClaudeEnv,
		},
		{
			name:       "claude config binary_path",
			harness:    "claude",
			defaults:   config.LaunchDefaults{BinaryPath: claudeBin},
			wantPath:   claudeBin,
			wantSource: BinaryClaudeConfig,
		},
		{
			name:       "claude falls through to PATH",
			harness:    "claude",
			wantPath:   filepath.Join(pathDir, "claude"),
			wantSource: BinaryPATH,
		},
		{
			name:       "codex env override",
			harness:    "codex",
			env:        map[string]string{"FORGECTL_CODEX_BIN": codexBin},
			defaults:   config.LaunchDefaults{CodexBinaryPath: filepath.Join(pathDir, "codex")},
			wantPath:   codexBin,
			wantSource: BinaryCodexEnv,
		},
		{
			name:       "codex config codex_binary_path",
			harness:    "codex",
			defaults:   config.LaunchDefaults{CodexBinaryPath: codexBin},
			wantPath:   codexBin,
			wantSource: BinaryCodexConfig,
		},
		{
			name:       "codex falls through to PATH",
			harness:    "codex",
			wantPath:   filepath.Join(pathDir, "codex"),
			wantSource: BinaryPATH,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBinEnv(t)
			t.Setenv("PATH", pathDir)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := ResolveBinary(tc.harness, tc.defaults)
			if err != nil {
				t.Fatalf("ResolveBinary(%q): %v", tc.harness, err)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tc.wantPath)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// TestResolveBinary_AgreesWithCompatibilityWrappers is the anti-drift pin for
// the risk forgectl#329 names directly: ten call sites across the repo resolve
// a harness binary through ClaudePath/CodexPath, and a second resolver that
// disagreed with them would mean two callers running different binaries with no
// error anywhere. The wrappers delegate, and this proves it for every layer
// rather than trusting the delegation to stay.
func TestResolveBinary_AgreesWithCompatibilityWrappers(t *testing.T) {
	claudeBin, codexBin, pathDir := resolutionStubs(t)

	tests := []struct {
		name     string
		harness  string
		env      map[string]string
		defaults config.LaunchDefaults
	}{
		{name: "claude via env", harness: "claude", env: map[string]string{"FORGECTL_CLAUDE_BIN": claudeBin}},
		{name: "claude via config", harness: "claude", defaults: config.LaunchDefaults{BinaryPath: claudeBin}},
		{name: "claude via PATH", harness: "claude"},
		{name: "codex via env", harness: "codex", env: map[string]string{"FORGECTL_CODEX_BIN": codexBin}},
		{name: "codex via config", harness: "codex", defaults: config.LaunchDefaults{CodexBinaryPath: codexBin}},
		{name: "codex via PATH", harness: "codex"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBinEnv(t)
			t.Setenv("PATH", pathDir)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			wrapper, wrapperErr := ClaudePath(tc.defaults)
			if tc.harness == "codex" {
				wrapper, wrapperErr = CodexPath(tc.defaults)
			}
			if wrapperErr != nil {
				t.Fatalf("compatibility wrapper: %v", wrapperErr)
			}

			resolved, err := ResolveBinary(tc.harness, tc.defaults)
			if err != nil {
				t.Fatalf("ResolveBinary: %v", err)
			}
			if resolved.Path != wrapper {
				t.Errorf("ResolveBinary path = %q but wrapper resolved %q — two resolution paths disagree", resolved.Path, wrapper)
			}
		})
	}
}

// TestResolveBinary_PropagatesSourceAttributedErrors keeps the existing error
// text reachable through the new resolver: an operator who mistyped
// FORGECTL_CODEX_BIN must be told which knob is wrong, not just that a binary
// was unusable.
func TestResolveBinary_PropagatesSourceAttributedErrors(t *testing.T) {
	clearBinEnv(t)
	t.Setenv("FORGECTL_CODEX_BIN", "/no/such/codex")

	_, err := ResolveBinary("codex", config.LaunchDefaults{})
	if err == nil || !strings.Contains(err.Error(), "FORGECTL_CODEX_BIN") {
		t.Fatalf("ResolveBinary error = %v, want source attribution", err)
	}
}

// TestResolveBinary_RefusesAnUnknownHarness pins the refusal rather than a
// fallthrough. A default branch reading the Claude env var and config key for a
// harness nobody named would stamp a claude-* provenance on the result — a
// Source that agrees with how Path was chosen while disagreeing with what was
// asked for, which is exactly the claim the surface policy gates on.
func TestResolveBinary_RefusesAnUnknownHarness(t *testing.T) {
	claudeBin, _, pathDir := resolutionStubs(t)
	clearBinEnv(t)
	t.Setenv("PATH", pathDir)
	// Both explicit layers are set and valid, so a fallthrough to the Claude
	// ladder would succeed loudly rather than fail for some unrelated reason.
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	for _, harness := range []string{"", "gemini", "Claude", "codex "} {
		t.Run(fmt.Sprintf("harness=%q", harness), func(t *testing.T) {
			got, err := ResolveBinary(harness, config.LaunchDefaults{BinaryPath: claudeBin})
			if err == nil {
				t.Fatalf("ResolveBinary(%q) = %+v, want a refusal", harness, got)
			}
			if !strings.Contains(err.Error(), "want claude or codex") {
				t.Errorf("error = %v, want one naming the supported harnesses", err)
			}
			if got != (ResolvedBinary{}) {
				t.Errorf("refused resolution returned %+v, want the zero value", got)
			}
		})
	}
}

// TestResolveBinary_ReturnsAbsolutePaths is the cwd-independence pin for the
// two explicit layers. A config `binary_path = "claude"` or an equally relative
// $FORGECTL_CLAUDE_BIN passes validateBinary's os.Stat happily, because that
// stat resolves against the process cwd — so before absolutising, the resolver
// could hand back a bare name. `forgectl launch` execs in place and never
// noticed; a consumer that honors Invocation.CWD would exec whatever sits at
// that name in the directory it moved to.
func TestResolveBinary_ReturnsAbsolutePaths(t *testing.T) {
	dir := projectDir(t)
	// #nosec G306 -- see stubBinary.
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Chdir(dir)

	tests := []struct {
		name     string
		env      string
		defaults config.LaunchDefaults
	}{
		{name: "config layer", defaults: config.LaunchDefaults{BinaryPath: "claude"}},
		{name: "env layer", env: "claude"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBinEnv(t)
			// An empty PATH so nothing can be satisfied by a PATH fallthrough —
			// the explicit layer under test is the only way this can resolve.
			t.Setenv("PATH", "")
			if tc.env != "" {
				t.Setenv("FORGECTL_CLAUDE_BIN", tc.env)
			}

			got, err := ResolveBinary("claude", tc.defaults)
			if err != nil {
				t.Fatalf("ResolveBinary: %v", err)
			}
			if !filepath.IsAbs(got.Path) {
				t.Errorf("Path = %q, want an absolute path", got.Path)
			}
			if want := filepath.Join(dir, "claude"); got.Path != want {
				t.Errorf("Path = %q, want %q — the file that was actually validated", got.Path, want)
			}
		})
	}
}

// TestResolveBinary_PATHLayerRefusesACwdRelativeHit records why the PATH layer
// needs no absolutising of its own, so a later reader does not have to re-derive
// it: exec.LookPath itself refuses a hit it resolved through the current
// directory (ErrDot), for both an empty $PATH component and a relative one.
//
// This pins a standard-library behavior rather than ours on purpose. It is the
// evidence for the claim in resolveLayered's comment, and if a future Go
// release relaxed it, absolute() on the PATH branch would stop being belt-and-
// braces and start being the only guard — which is exactly when someone needs
// to be told.
func TestResolveBinary_PATHLayerRefusesACwdRelativeHit(t *testing.T) {
	dir := projectDir(t)
	// #nosec G306 -- see stubBinary.
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Chdir(dir)

	for _, path := range []string{"", ".", string(os.PathListSeparator) + "/nonexistent"} {
		t.Run(fmt.Sprintf("PATH=%q", path), func(t *testing.T) {
			clearBinEnv(t)
			t.Setenv("PATH", path)

			got, err := ResolveBinary("claude", config.LaunchDefaults{})
			if err == nil {
				t.Fatalf("ResolveBinary = %+v, want a refusal of the cwd-relative hit", got)
			}
			if got != (ResolvedBinary{}) {
				t.Errorf("refused resolution returned %+v, want the zero value", got)
			}
		})
	}
}

// TestResolveBinary_RefusesATildePathWithNoHome closes a silent-wrong-binary
// path. expandTilde renders "~/bin/claude" through filepath.Join("", …) when no
// home directory is known, yielding the cwd-relative "bin/claude" — so an
// operator's explicit selection could resolve to an entirely different file
// that happens to sit in the working directory, with the tilde gone from the
// error message that would have explained it.
func TestResolveBinary_RefusesATildePathWithNoHome(t *testing.T) {
	dir := projectDir(t)
	decoy := filepath.Join(dir, "bin")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}
	// The decoy is what a silently-relativised "~/bin/claude" would find.
	// #nosec G306 -- see stubBinary.
	if err := os.WriteFile(filepath.Join(decoy, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	t.Chdir(dir)

	tests := []struct {
		name     string
		env      string
		defaults config.LaunchDefaults
		wantSrc  string
	}{
		{name: "env layer", env: "~/bin/claude", wantSrc: "FORGECTL_CLAUDE_BIN"},
		{
			name:     "config layer",
			defaults: config.LaunchDefaults{BinaryPath: "~/bin/claude"},
			wantSrc:  "binary_path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBinEnv(t)
			t.Setenv("PATH", "")
			t.Setenv("HOME", "")
			if tc.env != "" {
				t.Setenv("FORGECTL_CLAUDE_BIN", tc.env)
			}
			if home, err := os.UserHomeDir(); err == nil {
				t.Skipf("this platform still reports a home directory (%q) with HOME unset", home)
			}

			got, err := ResolveBinary("claude", tc.defaults)
			if err == nil {
				t.Fatalf("ResolveBinary = %+v, want a refusal rather than the cwd-relative decoy", got)
			}
			if !strings.Contains(err.Error(), "no home directory") {
				t.Errorf("error = %v, want one naming the missing home directory", err)
			}
			if !strings.Contains(err.Error(), tc.wantSrc) {
				t.Errorf("error = %v, want one attributing the path to %s", err, tc.wantSrc)
			}
		})
	}
}

// fixedResolver returns a resolver that answers with bin regardless of harness,
// so a builder test can exercise argv/env/cwd without a real binary on disk.
func fixedResolver(bin ResolvedBinary) BinaryResolver {
	return func(string, config.LaunchDefaults) (ResolvedBinary, error) { return bin, nil }
}

// projectDir returns a temp dir with its symlinks resolved. Resolve() resolves
// the cwd's symlinks but leaves a configured `match` lexical, so on macOS —
// where t.TempDir() hands back a /var path symlinked to /private/var — a raw
// temp dir as a match never matches the cwd built from it, and every profile
// test would quietly assert against the defaults.
func projectDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve symlinks on temp dir: %v", err)
	}
	return dir
}

// parityConfig is a launch config whose project block matches match and sets a
// distinguishable model plus a profile env entry.
func parityConfig(match string) config.LaunchConfig {
	allow := true
	return config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			Model:          "opus",
			PermissionMode: "plan",
			AllowDanger:    &allow,
		},
		Projects: []config.LaunchProject{{
			Match: match,
			Model: "sonnet",
			Env:   map[string]string{"PROFILE_KEY": "profile-value"},
		}},
	}
}

// TestBuildInvocation_TargetCWDSelectsProfile is the property that makes the
// builder reusable by `surface launch`, which resolves a posture for a project
// directory the operator is not standing in. Resolving against the process cwd
// would give the target the caller's profile — the same model and permission
// mode for every project, with nothing to show it went wrong.
func TestBuildInvocation_TargetCWDSelectsProfile(t *testing.T) {
	target := projectDir(t)
	other := projectDir(t)
	bin := ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}

	matched, err := BuildInvocation(InvocationRequest{
		Config:  parityConfig(target),
		CWD:     target,
		Resolve: fixedResolver(bin),
	})
	if err != nil {
		t.Fatalf("BuildInvocation(target): %v", err)
	}
	if matched.Profile.Model != "sonnet" {
		t.Errorf("target cwd resolved model %q, want the project's sonnet", matched.Profile.Model)
	}
	if matched.Invocation.CWD != target {
		t.Errorf("Invocation.CWD = %q, want the target %q", matched.Invocation.CWD, target)
	}

	unmatched, err := BuildInvocation(InvocationRequest{
		Config:  parityConfig(target),
		CWD:     other,
		Resolve: fixedResolver(bin),
	})
	if err != nil {
		t.Fatalf("BuildInvocation(other): %v", err)
	}
	if unmatched.Profile.Model != "opus" {
		t.Errorf("non-matching cwd resolved model %q, want the defaults' opus", unmatched.Profile.Model)
	}
}

// TestBuildInvocation_ClonesCallerSlices proves the builder holds no alias into
// its caller's memory. The surface coordinator keeps an Invocation across a
// network handshake and a cancellation window (forgectl#331); a shared backing
// array there means a later write by the caller silently rewrites an argv
// already in flight.
//
// Both subtests deliberately pick the input where the alias would actually
// exist, because most inputs launder it away by accident and would make this
// test pass over an aliasing builder:
//
//   - argv: every posture but one rebuilds the slice inside its argv builder.
//     The agents scripting passthrough is the single path that returns the
//     caller's args as the harness argv, so it is the only one where the clone
//     is load-bearing.
//   - env: MergeEnv rebuilds whenever it has an overlay to apply, so a profile
//     with any env at all hides the alias. The exposed case is an empty
//     overlay, where MergeEnv returns its base untouched.
func TestBuildInvocation_ClonesCallerSlices(t *testing.T) {
	target := projectDir(t)

	t.Run("agents passthrough argv", func(t *testing.T) {
		args := []string{"agents", "--json"}
		built, err := BuildInvocation(InvocationRequest{
			Config:  parityConfig(target),
			CWD:     target,
			Args:    args,
			Resolve: fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if built.Posture != PostureAgentsPassthrough {
			t.Fatalf("Posture = %q, want %q — this test only means something on the passthrough path", built.Posture, PostureAgentsPassthrough)
		}

		args[1] = "mutated"
		if got := built.Invocation.Args[1]; got != "--json" {
			t.Errorf("Args[1] = %q after the caller mutated its own slice, want %q — the builder aliased it", got, "--json")
		}

		// The reverse direction: writing through the invocation must not reach
		// back into the caller's slice.
		built.Invocation.Args[0] = "clobbered"
		if args[0] == "clobbered" {
			t.Error("mutating Invocation.Args reached back into the caller's slice")
		}
	})

	t.Run("base env with no overlay", func(t *testing.T) {
		bare := config.LaunchConfig{Defaults: config.LaunchDefaults{Model: "opus"}}
		baseEnv := []string{"KEEP=yes"}

		built, err := BuildInvocation(InvocationRequest{
			Config:  bare,
			CWD:     target,
			BaseEnv: baseEnv,
			Resolve: fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}

		baseEnv[0] = "KEEP=no"
		if got := built.Invocation.Env[0]; got != "KEEP=yes" {
			t.Errorf("Env[0] = %q after the caller mutated its own slice, want %q — the builder aliased its base env", got, "KEEP=yes")
		}
	})
}

// TestBuildInvocation_MergesEnvExactlyOnce pins the layering: injected
// telemetry defaults sit under the profile's env, and both sit over the
// process environment snapshot. A second merge anywhere in the chain would let
// an injected default beat the profile value that is supposed to override it.
func TestBuildInvocation_MergesEnvExactlyOnce(t *testing.T) {
	target := projectDir(t)

	built, err := BuildInvocation(InvocationRequest{
		Config:  parityConfig(target),
		CWD:     target,
		BaseEnv: []string{"INHERITED=kept", "PROFILE_KEY=from-process"},
		InjectedEnv: map[string]string{
			"PROFILE_KEY":  "injected-loses",
			"INJECTED_KEY": "injected-wins",
		},
		Resolve: fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}

	got := envMap(t, built.Invocation.Env)
	for key, want := range map[string]string{
		"INHERITED":    "kept",
		"PROFILE_KEY":  "profile-value",
		"INJECTED_KEY": "injected-wins",
	} {
		if got[key] != want {
			t.Errorf("env %s = %q, want %q", key, got[key], want)
		}
	}
	if n := countKey(built.Invocation.Env, "PROFILE_KEY"); n != 1 {
		t.Errorf("PROFILE_KEY appears %d times in the merged env, want exactly 1 — an overridden key must be dropped from the base, not shadowed", n)
	}
}

// TestBuildInvocation_PreservesClaudeChildMarker pins the non-surface side of
// forgectl#363. BuildInvocation also backs ordinary in-place launches and owns
// no surface policy; only the caller that creates a new surface may remove this
// marker.
func TestBuildInvocation_PreservesClaudeChildMarker(t *testing.T) {
	target := projectDir(t)
	built, err := BuildInvocation(InvocationRequest{
		Config:  config.LaunchConfig{Defaults: config.LaunchDefaults{Model: "opus"}},
		CWD:     target,
		BaseEnv: []string{"CLAUDE_CODE_CHILD_SESSION=1", "KEEP=yes"},
		Resolve: fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}

	got := envMap(t, built.Invocation.Env)
	if got["CLAUDE_CODE_CHILD_SESSION"] != "1" {
		t.Errorf("CLAUDE_CODE_CHILD_SESSION = %q, want preserved %q", got["CLAUDE_CODE_CHILD_SESSION"], "1")
	}
	if got["KEEP"] != "yes" {
		t.Errorf("KEEP = %q, want %q", got["KEEP"], "yes")
	}
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}

func countKey(env []string, key string) int {
	n := 0
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == key {
			n++
		}
	}
	return n
}

// TestBuildInvocation_Postures pins the argv shape and posture label for every
// branch launchExec used to switch on inline. The argv is compared against the
// package's own builders rather than a literal, so this stays a test of
// *routing* — which builder each input reaches — and does not duplicate the
// argv assertions those builders already carry.
func TestBuildInvocation_Postures(t *testing.T) {
	target := projectDir(t)
	claudeCfg := parityConfig(target)

	codexCfg := config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			Harness:        "codex",
			ApprovalPolicy: "never",
			Sandbox:        "read-only",
		},
	}

	tests := []struct {
		name        string
		cfg         config.LaunchConfig
		args        []string
		wantPosture Posture
		wantArgs    func(Profile) []string
	}{
		{
			name:        "bare claude launch",
			cfg:         claudeCfg,
			wantPosture: PostureClaudeSession,
			wantArgs:    func(p Profile) []string { return SessionArgs(p) },
		},
		{
			name:        "claude with passthrough args",
			cfg:         claudeCfg,
			args:        []string{"-p", "hello"},
			wantPosture: PostureClaudeBuilder,
			wantArgs:    func(p Profile) []string { return BuilderArgs(p, []string{"-p", "hello"}) },
		},
		{
			name:        "claude agents with posture injection",
			cfg:         claudeCfg,
			args:        []string{"agents", "list"},
			wantPosture: PostureClaudeAgents,
			wantArgs:    func(p Profile) []string { return AgentsArgs(p, []string{"agents", "list"}) },
		},
		{
			name:        "claude agents scripting passthrough",
			cfg:         claudeCfg,
			args:        []string{"agents", "--json"},
			wantPosture: PostureAgentsPassthrough,
			wantArgs:    func(Profile) []string { return []string{"agents", "--json"} },
		},
		{
			name:        "bare codex launch",
			cfg:         codexCfg,
			wantPosture: PostureCodexSession,
			wantArgs:    func(p Profile) []string { return CodexSessionArgs(p) },
		},
		{
			name:        "codex with passthrough args",
			cfg:         codexCfg,
			args:        []string{"review this"},
			wantPosture: PostureCodexExec,
			wantArgs:    func(p Profile) []string { return CodexExecArgs(p, []string{"review this"}) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			built, err := BuildInvocation(InvocationRequest{
				Config:  tc.cfg,
				CWD:     target,
				Args:    tc.args,
				Resolve: fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}),
			})
			if err != nil {
				t.Fatalf("BuildInvocation: %v", err)
			}
			if built.Posture != tc.wantPosture {
				t.Errorf("Posture = %q, want %q", built.Posture, tc.wantPosture)
			}
			want := tc.wantArgs(built.Profile)
			if strings.Join(built.Invocation.Args, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("Args = %q, want %q", built.Invocation.Args, want)
			}
		})
	}
}

// TestBuildInvocation_RefusesBeforeResolvingBinary keeps every refusal ahead of
// the side-effect-free-but-observable work. A resolver that ran first would
// stat a binary — and report its absence — for an invocation that was never
// going to launch, so the operator would be told to fix a PATH problem when the
// real fault is a Codex profile that cannot serve `agents`.
func TestBuildInvocation_RefusesBeforeResolvingBinary(t *testing.T) {
	target := projectDir(t)
	resolved := false
	tripwire := BinaryResolver(func(string, config.LaunchDefaults) (ResolvedBinary, error) {
		resolved = true
		return ResolvedBinary{}, nil
	})

	tests := []struct {
		name    string
		cfg     config.LaunchConfig
		args    []string
		wantErr string
	}{
		{
			name:    "unsupported harness",
			cfg:     config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "gemini"}},
			wantErr: "want claude or codex",
		},
		{
			name:    "agents under a codex profile",
			cfg:     config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex", ApprovalPolicy: "never", Sandbox: "read-only"}},
			args:    []string{"agents", "list"},
			wantErr: "Claude-only",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolved = false
			_, err := BuildInvocation(InvocationRequest{
				Config:  tc.cfg,
				CWD:     target,
				Args:    tc.args,
				Resolve: tripwire,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("BuildInvocation error = %v, want one mentioning %q", err, tc.wantErr)
			}
			if resolved {
				t.Error("the binary resolver ran despite the invocation being refused")
			}
		})
	}
}

// TestBuildInvocation_RequiresAResolver refuses rather than reaching for a
// package-level default. A nil resolver silently falling back to ResolveBinary
// would make the surface coordinator's policy-wrapped resolver optional by
// accident — and the failure would be a launch that ignored the policy, which
// looks exactly like a launch that honored it.
func TestBuildInvocation_RequiresAResolver(t *testing.T) {
	_, err := BuildInvocation(InvocationRequest{
		Config: parityConfig(t.TempDir()),
		CWD:    t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "resolver") {
		t.Fatalf("BuildInvocation error = %v, want a refusal naming the missing resolver", err)
	}
}

// TestEmitBanner_ClassifiesEveryPosture is the exhaustiveness guard. EmitBanner
// decides visibility by switching on a value another function mints, so the
// failure mode is a posture added to selectPosture and forgotten here — which
// costs a launch its only pre-session argv record and shows no symptom.
//
// The switch's default banners rather than staying silent, so the omission at
// least fails loud; this test is what makes it fail at build time instead. It
// asserts the classification is DELIBERATE, by requiring each known posture to
// produce the output its own case specifies — a posture that fell through to
// the default would produce the harness banner where a silent one was intended,
// and be caught.
func TestEmitBanner_ClassifiesEveryPosture(t *testing.T) {
	if len(allPostures) == 0 {
		t.Fatal("allPostures is empty; the loop below would pass vacuously")
	}

	silent := map[Posture]bool{PostureClaudeBuilder: true, PostureAgentsPassthrough: true}
	seen := map[Posture]bool{}

	for _, p := range allPostures {
		if seen[p] {
			t.Errorf("allPostures lists %q twice", p)
		}
		seen[p] = true

		var buf bytes.Buffer
		EmitBanner(&buf, BuiltInvocation{
			Invocation: Invocation{Harness: "claude", Args: []string{"--model", "sonnet"}},
			Posture:    p,
		})
		if silent[p] != (buf.String() == "") {
			t.Errorf("posture %q wrote %q; silent=%v", p, buf.String(), silent[p])
		}
	}

	// The other direction: selectPosture must not be able to return a posture
	// absent from allPostures, or the loop above would skip it entirely.
	target := projectDir(t)
	for _, args := range [][]string{nil, {"-p", "x"}, {"agents", "list"}, {"agents", "--json"}} {
		built, err := BuildInvocation(InvocationRequest{
			Config:  parityConfig(target),
			CWD:     target,
			Args:    args,
			Resolve: fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
		})
		if err != nil {
			t.Fatalf("BuildInvocation(%q): %v", args, err)
		}
		if !seen[built.Posture] {
			t.Errorf("selectPosture returned %q for args %q, which allPostures does not list", built.Posture, args)
		}
	}
}

// TestEmitBanner_ByPosture pins the exact bytes each posture writes, including
// the two that write nothing. The silent pair is the half worth a test: a
// refactor that banners the builder path would print the operator's prompt to
// stderr on every scripted `forgectl launch -p …`.
func TestEmitBanner_ByPosture(t *testing.T) {
	args := []string{"--model", "sonnet"}

	tests := []struct {
		posture Posture
		want    string
	}{
		{PostureClaudeSession, "→ claude --model sonnet\n"},
		{PostureClaudeAgents, "→ claude --model sonnet\n"},
		{PostureCodexSession, "→ codex --model sonnet\n"},
		{PostureCodexExec, "→ codex --model sonnet\n"},
		{PostureClaudeBuilder, ""},
		{PostureAgentsPassthrough, ""},
	}

	for _, tc := range tests {
		t.Run(string(tc.posture), func(t *testing.T) {
			harness := "claude"
			if strings.HasPrefix(string(tc.posture), "codex") {
				harness = "codex"
			}
			var buf bytes.Buffer
			EmitBanner(&buf, BuiltInvocation{
				Invocation: Invocation{Harness: harness, Args: args},
				Posture:    tc.posture,
			})
			if buf.String() != tc.want {
				t.Errorf("EmitBanner wrote %q, want %q", buf.String(), tc.want)
			}
		})
	}
}
