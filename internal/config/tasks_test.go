package config

// Test plan for TasksConfig, the hostname grammar, and the close-log path
//
//   [x] Happy: allowed_hosts decodes, and IsZero is false once it is set
//   [x] Grammar: a plain hostname is accepted; a port, userinfo, path,
//       trailing dot, empty label, IP literal in any spelling, or any
//       character outside letters, digits, '.' and '-' is refused
//   [x] Grammar: the service-name form differs only in admitting '_'
//   [x] Unhappy: a bad entry is a load error that names the key, through
//       DecodeStrict, LoadPath, ValidatePath, and Describe
//   [x] Unhappy: a config that failed this check carries no allowed hosts
//   [x] Path: the close log sits in the config directory, and the append
//       opener creates it owner-only

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTasksConfig_AllowedHosts_Decodes(t *testing.T) {
	got, err := DecodeStrict([]byte("[tasks]\nallowed_hosts = [\"board.example\", \"Other.Example\"]\n"))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if len(got.Tasks.AllowedHosts) != 2 || got.Tasks.AllowedHosts[0] != "board.example" || got.Tasks.AllowedHosts[1] != "Other.Example" {
		t.Errorf("AllowedHosts = %q, want the two entries as written", got.Tasks.AllowedHosts)
	}
	if got.Tasks.IsZero() {
		t.Error("IsZero() = true with allowed_hosts set")
	}
	if !(TasksConfig{}).IsZero() {
		t.Error("IsZero() = false for the zero value")
	}
}

func TestPlainHostname(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		want bool
	}{
		{"a dotted name", "board.example", true},
		{"a single label", "board", true},
		{"hyphens and digits", "task-board-2.internal.example", true},
		{"uppercase letters", "Board.Example", true},
		{"a label that starts with a digit", "3board.example", true},

		{"empty", "", false},
		{"a trailing dot", "board.example.", false},
		{"a leading dot", ".board.example", false},
		{"an empty label", "board..example", false},
		{"a port", "board.example:443", false},
		{"userinfo", "user@board.example", false},
		{"userinfo with a password", "user:pw@board.example", false},
		{"a path", "board.example/api", false},
		{"a scheme", "https://board.example", false},
		{"a query", "board.example?x=1", false},
		{"a fragment", "board.example#x", false},
		{"a space", "board .example", false},
		{"a line break", "board.example\nother.example", false},
		{"a control character", "board\x1b[2J.example", false},
		{"an underscore", "task_board.example", false},
		{"a percent escape", "board%2eexample", false},
		{"a non-ASCII letter", "bòard.example", false},
		{"an IPv4 literal", "192.168.1.102", false},
		{"an IPv4 literal in short form", "127.1", false},
		{"an IPv4 literal as one number", "2130706433", false},
		{"an IPv4 literal in hex", "0x7f.0.0.0x1", false},
		{"a name ending in a number", "board.example.7", false},
		{"an IPv6 literal", "fd00::1", false},
		{"an IPv6 loopback", "::1", false},
		{"a bracketed IPv6 literal", "[fd00::1]", false},
		{"an IPv6 literal with a zone", "fe80::1%en0", false},
		{"over 253 bytes", strings.Repeat("a.", 127) + "a", false},
	} {
		if got := PlainHostname(tc.host); got != tc.want {
			t.Errorf("PlainHostname(%s: %q) = %v, want %v", tc.name, tc.host, got, tc.want)
		}
	}
}

// TestPlainServiceHostname_DiffersOnlyByUnderscore: a compose service name
// uses '_', which a DNS hostname does not. Everything else is one grammar.
func TestPlainServiceHostname_DiffersOnlyByUnderscore(t *testing.T) {
	if !PlainServiceHostname("tasks-mcp_1.internal") {
		t.Error("PlainServiceHostname refused an underscore")
	}
	if PlainHostname("tasks-mcp_1.internal") {
		t.Error("PlainHostname accepted an underscore")
	}
	for _, host := range []string{"", "a..b", "a.b.", "a:1", "u@a", "a/b", "127.1", "192.168.1.102", "a b"} {
		if PlainServiceHostname(host) {
			t.Errorf("PlainServiceHostname(%q) = true, want the same refusal PlainHostname gives", host)
		}
	}
}

func TestTasksConfig_Validate_NamesTheKeyAndTheEntry(t *testing.T) {
	if err := (TasksConfig{AllowedHosts: []string{"board.example", "other.example"}}).Validate(); err != nil {
		t.Fatalf("Validate(two plain hostnames) = %v, want nil", err)
	}
	if err := (TasksConfig{}).Validate(); err != nil {
		t.Fatalf("Validate(zero) = %v, want nil", err)
	}
	err := (TasksConfig{AllowedHosts: []string{"board.example", "other.example:443"}}).Validate()
	if err == nil {
		t.Fatal("Validate(an entry with a port) = nil, want an error")
	}
	for _, want := range []string{"[tasks].allowed_hosts[1]", `"other.example:443"`, "plain hostname"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

const badTasksHostsTOML = "[tasks]\nallowed_hosts = [\"board.example\", \"192.168.1.50\"]\n"

func TestDecodeStrict_RefusesABadAllowedHost(t *testing.T) {
	cfg, err := DecodeStrict([]byte(badTasksHostsTOML))
	if err == nil {
		t.Fatal("DecodeStrict(an IP literal in allowed_hosts) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "[tasks].allowed_hosts[1]") {
		t.Errorf("error %q does not name the key", err)
	}
	// A list that failed the check allows nothing: the valid first entry must
	// not survive in a Config some caller goes on to use.
	if len(cfg.Tasks.AllowedHosts) != 0 {
		t.Errorf("AllowedHosts = %q after a failed check, want none", cfg.Tasks.AllowedHosts)
	}
}

func TestLoadPath_ABadAllowedHostIsALoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(badTasksHostsTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadPath(path)
	err := cfg.DecodeError()
	if err == nil {
		t.Fatal("DecodeError() = nil after loading a bad allowed_hosts entry")
	}
	if !cfg.DecodeDegraded() {
		t.Error("DecodeDegraded() = false after a failed load")
	}
	for _, want := range []string{"[tasks].allowed_hosts[1]", "config.toml", "not valid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("load error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "does not parse") {
		t.Errorf("load error %q calls a file that parsed unparseable", err)
	}
	if len(cfg.Tasks.AllowedHosts) != 0 {
		t.Errorf("AllowedHosts = %q after a failed load, want none", cfg.Tasks.AllowedHosts)
	}

	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "[tasks].allowed_hosts[1]") {
		t.Errorf("ValidatePath = %v, want an error naming the key", err)
	}
}

func TestLoadPath_AGoodAllowedHostLoadsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tasks]\nallowed_hosts = [\"board.example\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadPath(path)
	if err := cfg.DecodeError(); err != nil {
		t.Fatalf("DecodeError() = %v, want nil", err)
	}
	if len(cfg.Tasks.AllowedHosts) != 1 || cfg.Tasks.AllowedHosts[0] != "board.example" {
		t.Errorf("AllowedHosts = %q, want the one entry", cfg.Tasks.AllowedHosts)
	}
	if err := ValidatePath(path); err != nil {
		t.Errorf("ValidatePath = %v, want nil", err)
	}
}

// TestDescribe_ReportsABadAllowedHost: `forgectl config` is the verb an
// operator opens to find out why the file was refused, so it must say.
func TestDescribe_ReportsABadAllowedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(badTasksHostsTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, rep := describeFile(path)
	if rep.InvalidErr == nil || !strings.Contains(rep.InvalidErr.Error(), "[tasks].allowed_hosts[1]") {
		t.Errorf("InvalidErr = %v, want an error naming the key", rep.InvalidErr)
	}
	// The file parsed. Reporting the refusal as a decode error would send the
	// operator looking for a syntax error that is not there.
	if rep.DecodeErr != nil {
		t.Errorf("DecodeErr = %v for a file that parsed in full, want nil", rep.DecodeErr)
	}
	if !rep.IsRefused("tasks.allowed_hosts") {
		t.Errorf("Refused = %v, want the allowed_hosts key", rep.Refused)
	}
	if rep.IsRefused("tasks") || rep.IsRefused("") || rep.IsRefused("net.probe_host") {
		t.Errorf("Refused = %v matches a key that was not refused", rep.Refused)
	}
	// The report shows what the file says, so the operator can see the entry
	// to fix; that the loader took none of it is what Refused is for.
	if len(cfg.Tasks.AllowedHosts) != 2 || !rep.IsSet("tasks.allowed_hosts") {
		t.Errorf("AllowedHosts = %q (set %v), want the file's own two entries", cfg.Tasks.AllowedHosts, rep.IsSet("tasks.allowed_hosts"))
	}
}

func TestDescribe_AGoodAllowedHostIsNotRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tasks]\nallowed_hosts = [\"board.example\"]\nmisspelled = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rep := describeFile(path)
	if rep.InvalidErr != nil || rep.DecodeErr != nil || len(rep.Refused) != 0 || rep.IsRefused("tasks.allowed_hosts") {
		t.Errorf("report = %+v, want a clean file with nothing refused", rep)
	}
	if len(rep.Unrecognized) != 1 || rep.Unrecognized[0] != "tasks.misspelled" {
		t.Errorf("Unrecognized = %v, want the one misspelled key", rep.Unrecognized)
	}
}

func TestTasksCloseLogPath_IsInTheConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	got, err := TasksCloseLogPath()
	if err != nil {
		t.Fatalf("TasksCloseLogPath: %v", err)
	}
	cache, err := TasksCachePath()
	if err != nil {
		t.Fatalf("TasksCachePath: %v", err)
	}
	if filepath.Dir(got) != filepath.Dir(cache) {
		t.Errorf("close log dir = %q, want the cache's dir %q", filepath.Dir(got), filepath.Dir(cache))
	}
	if filepath.Base(got) != "tasks-closes.jsonl" {
		t.Errorf("close log name = %q, want tasks-closes.jsonl", filepath.Base(got))
	}
	if !strings.HasPrefix(got, home) {
		t.Fatalf("close log %q is outside the test home %q", got, home)
	}

	f, err := OpenAppendFile(got)
	if err != nil {
		t.Fatalf("OpenAppendFile: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return // permission bits are not meaningful there
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("close log mode = %04o, want 0600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(got))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dirInfo.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("close log directory mode = %04o, want owner-only", mode)
	}
}
