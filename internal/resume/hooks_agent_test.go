package resume

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func testAgentSpec(dir string) AgentSpec {
	return AgentSpec{
		Label:      HooksAgentLabel,
		Program:    "/opt/homebrew/bin/forgectl",
		Args:       []string{"resume", "hooks", "run"},
		WatchPaths: []string{"/Users/u/.local/bin/claude", "/Users/u/.local/bin"},
		Env:        map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "HERDR_SOCKET_PATH": "/Users/u/.config/herdr/a&b<c>.sock"},
		WorkingDir: filepath.Join(dir, "state"),
		LogPath:    filepath.Join(dir, "state", "watcher.log"),
	}
}

func TestRenderAgentPlist(t *testing.T) {
	dir := t.TempDir()
	data, err := RenderAgentPlist(testAgentSpec(dir))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<string>" + HooksAgentLabel + "</string>",
		"<string>/opt/homebrew/bin/forgectl</string>\n    <string>resume</string>\n    <string>hooks</string>\n    <string>run</string>",
		"<string>/Users/u/.local/bin/claude</string>",
		"a&amp;b&lt;c&gt;.sock",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>WorkingDirectory</key>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "HERDR_SOCKET_PATH") > strings.Index(s, "<key>PATH</key>") {
		t.Error("env keys are not sorted; an unchanged spec must render identical bytes")
	}
	if runtime.GOOS != "darwin" {
		return
	}
	plutil, err := osexec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not found")
	}
	path := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// plutil -lint only reads the file.
	if out, err := osexec.CommandContext(t.Context(), plutil, "-lint", path).CombinedOutput(); err != nil { // #nosec G204 -- fixed tool, temp path
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
}

func TestAgentSpecValidate(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]func(*AgentSpec){
		"relative program":    func(s *AgentSpec) { s.Program = "forgectl" },
		"relative watch path": func(s *AgentSpec) { s.WatchPaths = []string{"bin/claude"} },
		"no watch path":       func(s *AgentSpec) { s.WatchPaths = nil },
		"relative log":        func(s *AgentSpec) { s.LogPath = "watcher.log" },
		"control in env":      func(s *AgentSpec) { s.Env["PATH"] = "/bin\x01" },
		"bad env key":         func(s *AgentSpec) { s.Env["lower-case"] = "x" },
		"bad label":           func(s *AgentSpec) { s.Label = "../evil" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := testAgentSpec(dir)
			mutate(&s)
			if _, err := RenderAgentPlist(s); err == nil {
				t.Fatal("rendered an invalid spec")
			}
		})
	}
}

func TestCheckAgentBinary(t *testing.T) {
	cases := map[string]bool{
		"/opt/homebrew/bin/forgectl":                     true,
		"/Users/u/go/bin/forgectl":                       true,
		"/var/folders/x/T/go-build123/b001/exe/forgectl": false,
		"forgectl": false,
	}
	for exe, ok := range cases {
		if err := CheckAgentBinary(exe); (err == nil) != ok {
			t.Errorf("CheckAgentBinary(%q) = %v, want ok=%v", exe, err, ok)
		}
	}
}

func TestAgentPATH(t *testing.T) {
	got := AgentPATH("/opt/homebrew/bin", "/Users/u/.local/bin", "/opt/homebrew/bin", "relative", "/usr/bin/")
	want := "/opt/homebrew/bin:/Users/u/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	if got != want {
		t.Fatalf("AgentPATH = %q, want %q", got, want)
	}
}

func TestParseLaunchctlPrint(t *testing.T) {
	out := "gui/501/local.forgectl.resume-hooks = {\n\tactive count = 0\n\tpath = /x.plist\n\tstate = not running\n\n\truns = 4\n\tlast exit code = 1\n\tspawn type = daemon (3)\n\tjetsam = {\n\t\tstate = other\n\t}\n}\n"
	st := parseLaunchctlPrint(out)
	if !st.Loaded || st.State != "not running" || st.Runs != 4 || st.LastExit != "1" {
		t.Fatalf("parse = %+v", st)
	}
	if st := parseLaunchctlPrint("garbage"); st.State != "" || st.Runs != -1 || st.LastExit != "" {
		t.Fatalf("unknown shape = %+v", st)
	}
}

// fakeLaunchd answers launchctl through a FakeRunner: print fails until
// bootstrap, and succeeds until bootout.
func fakeLaunchd(loaded bool) (*exec.FakeRunner, *bool) {
	state := loaded
	r := &exec.FakeRunner{}
	r.RunFunc = func(name string, args []string) (string, error) {
		if name != "launchctl" {
			return "", errors.New("unexpected " + name)
		}
		switch args[0] {
		case "print":
			if !state {
				return "", &exec.CommandError{Name: name, ExitCode: 113, Err: errors.New("exit status 113")}
			}
			return "state = not running\nruns = 1\nlast exit code = 0\n", nil
		case "bootstrap":
			state = true
		case "bootout":
			state = false
		}
		return "", nil
	}
	return r, &state
}

func launchctlVerbs(r *exec.FakeRunner) []string {
	var out []string
	for _, c := range r.Calls {
		out = append(out, c.Args[0])
	}
	return out
}

func TestInstallAgentIdempotent(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "LaunchAgents")
	spec := testAgentSpec(dir)
	r, loaded := fakeLaunchd(false)
	lc := Launchctl{Runner: r, UID: 501, Sleep: noSleep}

	got, err := InstallAgent(context.Background(), agents, spec, lc)
	if err != nil || got != InstallCreated || !*loaded {
		t.Fatalf("first install = %q, %v, loaded %v", got, err, *loaded)
	}
	if v := launchctlVerbs(r); !slices.Equal(v, []string{"print", "bootstrap"}) {
		t.Fatalf("first install calls %v", v)
	}
	if last := r.Last(); !slices.Equal(last.Args, []string{"bootstrap", "gui/501", AgentPlistPath(agents, HooksAgentLabel)}) {
		t.Fatalf("bootstrap argv %v", last.Args)
	}
	fi, err := os.Stat(AgentPlistPath(agents, HooksAgentLabel))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode %v, %v", fi.Mode(), err)
	}
	if _, err := os.Stat(spec.WorkingDir); err != nil {
		t.Fatalf("working directory not created: %v", err)
	}

	r.Calls = nil
	got, err = InstallAgent(context.Background(), agents, spec, lc)
	if err != nil || got != InstallUnchanged || !slices.Equal(launchctlVerbs(r), []string{"print"}) {
		t.Fatalf("repeat install = %q, %v, calls %v", got, err, launchctlVerbs(r))
	}

	r.Calls = nil
	spec.Program = "/usr/local/bin/forgectl"
	got, err = InstallAgent(context.Background(), agents, spec, lc)
	if err != nil || got != InstallUpdated || !slices.Equal(launchctlVerbs(r), []string{"print", "bootout", "bootstrap"}) {
		t.Fatalf("changed install = %q, %v, calls %v", got, err, launchctlVerbs(r))
	}

	r.Calls = nil
	did, err := UninstallAgent(context.Background(), agents, HooksAgentLabel, lc)
	if err != nil || !did || *loaded {
		t.Fatalf("uninstall = %v, %v, loaded %v", did, err, *loaded)
	}
	if _, err := os.Stat(AgentPlistPath(agents, HooksAgentLabel)); !os.IsNotExist(err) {
		t.Fatal("plist left behind")
	}
	did, err = UninstallAgent(context.Background(), agents, HooksAgentLabel, lc)
	if err != nil || did {
		t.Fatalf("repeat uninstall = %v, %v", did, err)
	}
}

func TestReadAgentStatus(t *testing.T) {
	dir := t.TempDir()
	r, _ := fakeLaunchd(false)
	st := ReadAgentStatus(context.Background(), dir, HooksAgentLabel, Launchctl{Runner: r, UID: 501})
	if st.Installed || st.Loaded {
		t.Fatalf("empty dir status %+v", st)
	}
}

// TestInstallAgentBootoutFailureLeavesOldPlist: a failed bootout must not
// leave the new plist behind, or the next install would report "already
// installed" over the stale job.
func TestInstallAgentBootoutFailureLeavesOldPlist(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "LaunchAgents")
	spec := testAgentSpec(dir)
	r, _ := fakeLaunchd(false)
	lc := Launchctl{Runner: r, UID: 501, Sleep: noSleep}
	if _, err := InstallAgent(context.Background(), agents, spec, lc); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(AgentPlistPath(agents, HooksAgentLabel)) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	inner := r.RunFunc
	r.RunFunc = func(name string, args []string) (string, error) {
		if args[0] == "bootout" {
			return "", &exec.CommandError{Name: name, ExitCode: 5, Err: errors.New("exit status 5")}
		}
		return inner(name, args)
	}
	spec.Program = "/usr/local/bin/forgectl"
	if _, err := InstallAgent(context.Background(), agents, spec, lc); err == nil {
		t.Fatal("want the bootout failure")
	}
	now, err := os.ReadFile(AgentPlistPath(agents, HooksAgentLabel)) // #nosec G304 -- test temp dir
	if err != nil || !bytes.Equal(now, old) {
		t.Fatal("the new plist was written although the old job could not be unloaded")
	}
	r.RunFunc = inner
	got, err := InstallAgent(context.Background(), agents, spec, lc)
	if err != nil || got != InstallUpdated {
		t.Fatalf("install after the failure = %q, %v; want it to update, not report unchanged", got, err)
	}
}

func TestInstallAgentRetriesBootstrap(t *testing.T) {
	dir := t.TempDir()
	r, loaded := fakeLaunchd(false)
	inner := r.RunFunc
	failures := 2
	r.RunFunc = func(name string, args []string) (string, error) {
		if args[0] == "bootstrap" && failures > 0 {
			failures--
			return "", &exec.CommandError{Name: name, ExitCode: 5, Stderr: "Bootstrap failed: 5: Input/output error", Err: errors.New("exit status 5")}
		}
		return inner(name, args)
	}
	slept := 0
	lc := Launchctl{Runner: r, UID: 501, Sleep: func(context.Context, time.Duration) error { slept++; return nil }}
	if _, err := InstallAgent(context.Background(), filepath.Join(dir, "LaunchAgents"), testAgentSpec(dir), lc); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !*loaded || slept != 2 {
		t.Fatalf("loaded %v, slept %d", *loaded, slept)
	}
	failures = 99
	r.Calls = nil
	*loaded = false
	if err := lc.Bootstrap(context.Background(), "/x.plist"); err == nil {
		t.Fatal("a bootstrap that always fails reported success")
	}
	if n := len(r.Calls); n != bootstrapAttempts {
		t.Fatalf("attempts = %d, want %d", n, bootstrapAttempts)
	}
}

func TestRenderAgentPlistInterval(t *testing.T) {
	s := testAgentSpec(t.TempDir())
	s.Interval = 1800
	data, err := RenderAgentPlist(s)
	if err != nil || !strings.Contains(string(data), "<key>StartInterval</key>\n  <integer>1800</integer>") {
		t.Fatalf("%v\n%s", err, data)
	}
}

func TestCheckVersionsLink(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(versions, "2.1.285")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckVersionsLink(link); err != nil {
		t.Fatalf("versions link refused: %v", err)
	}
	if err := CheckVersionsLink(target); err == nil {
		t.Fatal("a plain file was accepted as a versions link")
	}
}
