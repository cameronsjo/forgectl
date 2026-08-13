package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"

	runnerexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func TestExternalCommand_EligibleUnknownVerbsReachRungB(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	resolved := filepath.Join(t.TempDir(), "forgectl-frobnicate")

	tests := []struct {
		name     string
		args     []string
		wantArgv []string
	}{
		{"bare verb", []string{"frobnicate"}, []string{resolved}},
		{"bare no-icons consumed", []string{"--no-icons", "frobnicate", "arg"}, []string{resolved, "arg"}},
		{"valued no-icons consumed", []string{"--no-icons=false", "frobnicate", "--flag"}, []string{resolved, "--flag"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lookups []string
			var gotArgv []string
			runtime := externalCommandRuntime{
				lookPath: func(name string) (string, error) {
					lookups = append(lookups, name)
					return resolved, nil
				},
				exec: func(_ string, argv, _ []string) error {
					gotArgv = append([]string(nil), argv...)
					return nil
				},
				environ: func() []string { return nil },
				stderr:  &bytes.Buffer{},
			}

			handled, err := tryExtensionRungs(root, tt.args, runtime)
			if err != nil {
				t.Fatalf("tryExtensionRungs() error = %v", err)
			}
			if !handled {
				t.Fatal("tryExtensionRungs() handled = false, want true")
			}
			if !reflect.DeepEqual(lookups, []string{meta.AppName + "-frobnicate"}) {
				t.Errorf("LookPath calls = %v", lookups)
			}
			if !reflect.DeepEqual(gotArgv, tt.wantArgv) {
				t.Errorf("exec argv = %#v, want %#v", gotArgv, tt.wantArgv)
			}
		})
	}
}

func TestExternalCommand_RegisteredCommandsAndAliasesNeverProbe(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	for _, command := range root.Commands() {
		tokens := append([]string{command.Name()}, command.Aliases...)
		for _, token := range tokens {
			t.Run(token, func(t *testing.T) {
				assertExternalCommandDoesNotProbe(t, root, []string{token})
			})
		}
	}
}

func TestExternalCommand_LazyBuiltinsNeverProbe(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	for token := range builtinVerbs {
		t.Run(token, func(t *testing.T) {
			assertExternalCommandDoesNotProbe(t, root, []string{token})
		})
	}
}

func TestExternalCommand_IneligiblePrefixesNeverProbe(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	tests := []struct {
		name string
		args []string
	}{
		{"empty", nil},
		{"no-icons only", []string{"--no-icons"}},
		{"valued no-icons only", []string{"--no-icons=true"}},
		{"help only", []string{"--help"}},
		{"version only", []string{"--version"}},
		{"help before verb", []string{"--help", "frobnicate"}},
		{"version before verb", []string{"--version", "frobnicate"}},
		{"unknown flag before verb", []string{"--bogus", "frobnicate"}},
		{"short flag before verb", []string{"-v", "frobnicate"}},
		{"sentinel is a hard stop", []string{"--", "frobnicate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertExternalCommandDoesNotProbe(t, root, tt.args)
		})
	}
}

func TestExternalCommand_BareDashStaysMenuShorthand(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	assertExternalCommandDoesNotProbe(t, root, []string{"-"})
	if got := decideRoute(root, []string{"-"}, true); got != routeTUI {
		t.Errorf("interactive bare dash route = %v, want routeTUI", got)
	}
	if got := decideRoute(root, []string{"-"}, false); got != routeHeadlessMenu {
		t.Errorf("headless bare dash route = %v, want routeHeadlessMenu", got)
	}
}

func TestExternalCommand_KnownParentUnknownSubverbNeverProbes(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	args := []string{"tmux", "frobnicate"}
	assertExternalCommandDoesNotProbe(t, root, args)
	if got := decideRoute(root, args, true); got != routeTUI {
		t.Errorf("interactive route = %v, want routeTUI", got)
	}
	if got := decideRoute(root, args, false); got != routeHeadlessMenu {
		t.Errorf("headless route = %v, want routeHeadlessMenu", got)
	}
}

func TestExternalCommand_PathShapedVerbsNeverProbe(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	for _, verb := range []string{"dir/name", "../name", `dir\name`, `\name`} {
		t.Run(verb, func(t *testing.T) {
			assertExternalCommandDoesNotProbe(t, root, []string{verb})
		})
	}
}

func TestExternalCommand_LookPathMissesFallThrough(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	tests := []struct {
		name      string
		path      string
		lookupErr error
	}{
		{"ordinary miss", "", osexec.ErrNotFound},
		{"ErrDot miss", "forgectl-frobnicate", fmt.Errorf("unsafe relative result: %w", osexec.ErrDot)},
		{"nil-error relative result", "forgectl-frobnicate", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var execCalls int
			var stderr bytes.Buffer
			runtime := externalCommandRuntime{
				lookPath: func(string) (string, error) { return tt.path, tt.lookupErr },
				exec: func(string, []string, []string) error {
					execCalls++
					return nil
				},
				environ: func() []string { return nil },
				stderr:  &stderr,
			}
			handled, err := tryExtensionRungs(root, []string{"frobnicate"}, runtime)
			if handled || err != nil {
				t.Fatalf("tryExtensionRungs() = (%v, %v), want (false, nil)", handled, err)
			}
			if execCalls != 0 {
				t.Errorf("exec calls = %d, want 0", execCalls)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestExternalCommand_ExecFailureIsHandledSanitizedAndPreservesIdentity(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	resolved := filepath.Join(t.TempDir(), "forgectl-frobnicate")
	wantErr := errors.New("exec refused\x1b[31m\r\nforged")
	var execCalls int
	var stderr bytes.Buffer
	runtime := externalCommandRuntime{
		lookPath: func(string) (string, error) { return resolved, nil },
		exec: func(string, []string, []string) error {
			execCalls++
			return wantErr
		},
		environ: func() []string { return nil },
		stderr:  &stderr,
	}

	handled, err := tryExtensionRungs(root, []string{"frobnicate"}, runtime)
	if !handled {
		t.Fatal("tryExtensionRungs() handled = false, want true (exec failures fail closed)")
	}
	if err != wantErr {
		t.Fatalf("tryExtensionRungs() error = %v, want identical error %v", err, wantErr)
	}
	if execCalls != 1 {
		t.Errorf("exec calls = %d, want 1", execCalls)
	}
	wantLine := termsafe.Sanitize(meta.AppName+": "+wantErr.Error()) + "\n"
	if got := stderr.String(); got != wantLine {
		t.Errorf("stderr = %q, want exactly %q", got, wantLine)
	}
	if strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr contains more than one terminal line: %q", stderr.String())
	}
	for _, r := range strings.TrimSuffix(stderr.String(), "\n") {
		if r != '\t' && unicode.IsControl(r) {
			t.Errorf("stderr retained prohibited control character %U in %q", r, stderr.String())
		}
	}
}

func TestExternalCommand_ExecGetsExactCopiedArgvAndEnvironment(t *testing.T) {
	root := newRoot(module.Deps{Runner: &runnerexec.FakeRunner{}})
	resolved := filepath.Join(t.TempDir(), "forgectl-complex")
	args := []string{"--no-icons=false", "complex", "", "two words", "雪", "--flag=value", "--", "tail"}
	env := []string{"A=1", "SPACED=two words", "UNICODE=雪"}
	wantArgs := append([]string(nil), args...)
	wantEnv := append([]string(nil), env...)
	var gotPath string
	var gotArgv, gotEnv []string
	runtime := externalCommandRuntime{
		lookPath: func(name string) (string, error) {
			if name != meta.AppName+"-complex" {
				t.Errorf("LookPath name = %q", name)
			}
			return resolved, nil
		},
		exec: func(path string, argv, environ []string) error {
			gotPath = path
			gotArgv = append([]string(nil), argv...)
			gotEnv = append([]string(nil), environ...)
			argv[0] = "mutated by fake exec"
			environ[0] = "MUTATED=1"
			return nil
		},
		environ: func() []string { return env },
		stderr:  &bytes.Buffer{},
	}

	handled, err := tryExtensionRungs(root, args, runtime)
	if !handled || err != nil {
		t.Fatalf("tryExtensionRungs() = (%v, %v), want (true, nil)", handled, err)
	}
	if gotPath != resolved {
		t.Errorf("exec path = %q, want %q", gotPath, resolved)
	}
	wantArgv := append([]string{resolved}, args[2:]...)
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Errorf("exec argv = %#v, want %#v", gotArgv, wantArgv)
	}
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Errorf("exec env = %#v, want %#v", gotEnv, wantEnv)
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("input args mutated: got %#v, want %#v", args, wantArgs)
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("input env mutated: got %#v, want %#v", env, wantEnv)
	}
}

func TestIntegration_ExternalCommandPreservesProcessBoundary(t *testing.T) {
	dir := t.TempDir()
	verb := "boundary-" + strconv.Itoa(os.Getpid())
	stub := filepath.Join(dir, meta.AppName+"-"+verb)
	argvOut := filepath.Join(dir, "argv.bin")
	envOut := filepath.Join(dir, "env.txt")
	stubBody := `#!/bin/sh
: > "$FORGECTL_TEST_ARGV_OUT"
for arg do
  printf '%s\0' "$arg" >> "$FORGECTL_TEST_ARGV_OUT"
done
printf '%s' "$FORGECTL_TEST_SENTINEL" > "$FORGECTL_TEST_ENV_OUT"
printf 'external stdout'
printf 'external stderr' >&2
exit 42
`
	if err := os.WriteFile(stub, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write external command stub: %v", err)
	}

	wantArgs := []string{"", "two words", "雪", "--flag-looking", "tail"}
	cmd := osexec.Command(builtBinPath, append([]string{"--no-icons=true", verb}, wantArgs...)...)
	cmd.Env = externalCommandTestEnv(map[string]string{
		"PATH":                         dir,
		"HOME":                         dir,
		"XDG_CONFIG_HOME":              dir,
		"FORGECTL_TEST_ARGV_OUT":       argvOut,
		"FORGECTL_TEST_ENV_OUT":        envOut,
		"FORGECTL_TEST_SENTINEL":       "inherited sentinel 雪",
		"FORGECTL_SKIP_LEGACY_MIGRATE": "1",
	})
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("forgectl external exit = %v, want status 42; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if stdout.String() != "external stdout" {
		t.Errorf("inherited stdout = %q", stdout.String())
	}
	if stderr.String() != "external stderr" {
		t.Errorf("inherited stderr = %q", stderr.String())
	}

	data, err := os.ReadFile(argvOut)
	if err != nil {
		t.Fatalf("read delimiter-safe argv record: %v", err)
	}
	fields := bytes.Split(data, []byte{0})
	if len(fields) == 0 || len(fields[len(fields)-1]) != 0 {
		t.Fatalf("argv record lacks trailing NUL delimiter: %q", data)
	}
	fields = fields[:len(fields)-1]
	gotArgs := make([]string, len(fields))
	for i := range fields {
		gotArgs[i] = string(fields[i])
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("external argv = %#v, want %#v", gotArgs, wantArgs)
	}
	gotSentinel, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read inherited env record: %v", err)
	}
	if got := string(gotSentinel); got != "inherited sentinel 雪" {
		t.Errorf("inherited sentinel = %q", got)
	}
}

func TestIntegration_ExternalCommandRefusesRelativePATHWithExecErrDotDisabled(t *testing.T) {
	dir := t.TempDir()
	verb := "relative-" + strconv.Itoa(os.Getpid())
	stub := filepath.Join(dir, meta.AppName+"-"+verb)
	marker := filepath.Join(dir, "ran")
	stubBody := "#!/bin/sh\nprintf ran > \"$FORGECTL_TEST_MARKER\"\nexit 42\n"
	if err := os.WriteFile(stub, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write relative external command stub: %v", err)
	}

	cmd := osexec.Command(builtBinPath, verb)
	cmd.Dir = dir
	cmd.Env = externalCommandTestEnv(map[string]string{
		"PATH":                         ".",
		"HOME":                         dir,
		"XDG_CONFIG_HOME":              dir,
		"GODEBUG":                      "execerrdot=0",
		"FORGECTL_TEST_MARKER":         marker,
		"FORGECTL_SKIP_LEGACY_MIGRATE": "1",
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("relative PATH invocation error = %v, want non-zero fallback", err)
	}
	if exitErr.ExitCode() == 42 {
		t.Fatalf("relative PATH external command executed; stderr=%q", stderr.String())
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("relative PATH stub marker stat = %v, want not-exist", statErr)
	}
}

func TestIntegration_ExternalCommandMissPreservesHeadlessCobraFallback(t *testing.T) {
	dir := t.TempDir()
	cmd := osexec.Command(builtBinPath, "definitely-not-an-external-command")
	cmd.Env = externalCommandTestEnv(map[string]string{
		"PATH":                         dir,
		"HOME":                         dir,
		"XDG_CONFIG_HOME":              dir,
		"FORGECTL_SKIP_LEGACY_MIGRATE": "1",
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("unknown command miss exited zero, want existing headless failure")
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "unknown command") {
		t.Errorf("stderr = %q, want Cobra unknown-command fallback", stderr.String())
	}
}

func assertExternalCommandDoesNotProbe(t *testing.T, root *cobra.Command, args []string) {
	t.Helper()
	var lookups, execCalls int
	runtime := externalCommandRuntime{
		lookPath: func(string) (string, error) {
			lookups++
			return filepath.Join(t.TempDir(), "unexpected"), nil
		},
		exec: func(string, []string, []string) error {
			execCalls++
			return nil
		},
		environ: func() []string { return nil },
		stderr:  &bytes.Buffer{},
	}
	handled, err := tryExtensionRungs(root, args, runtime)
	if handled || err != nil {
		t.Fatalf("tryExtensionRungs(%#v) = (%v, %v), want (false, nil)", args, handled, err)
	}
	if lookups != 0 || execCalls != 0 {
		t.Errorf("tryExtensionRungs(%#v) made %d lookup and %d exec calls, want zero", args, lookups, execCalls)
	}
}

func externalCommandTestEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}
