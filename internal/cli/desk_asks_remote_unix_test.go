//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	osexec "os/exec"
	"slices"
	"strings"
	"testing"
)

func TestShSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"two words", "'two words'"},
		{"it's", `'it'\''s'`},
		{"'", `''\'''`},
		{`"double"`, `'"double"'`},
		{"$(touch /tmp/x)", "'$(touch /tmp/x)'"},
		{"`id`", "'`id`'"},
		{"a; rm -rf ~", "'a; rm -rf ~'"},
		{"line1\nline2", "'line1\nline2'"},
		{"--leading-dash", "'--leading-dash'"},
		{"-", "'-'"},
		{`back\slash`, `'back\slash'`},
		{"$HOME *", "'$HOME *'"},
		{"a|b&c>d<e", "'a|b&c>d<e'"},
	}
	for _, tc := range cases {
		if got := shSingleQuote(tc.in); got != tc.want {
			t.Errorf("shSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestShSingleQuote_RoundTripsThroughSh joins quoted words with spaces, as ssh
// does, and has a real POSIX sh split them again: every word must come back
// byte for byte, and nothing in it may run.
func TestShSingleQuote_RoundTripsThroughSh(t *testing.T) {
	words := []string{"", "it's", "''", "$(echo pwned)", "`echo pwned`", "; echo pwned", "a\nb",
		"-n", "--title=x y", "\\", "\"", "$1 $@", "!", "#comment", "~", "*"}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shSingleQuote(w)
	}
	script := "for a in " + strings.Join(quoted, " ") + "; do printf '%s\\0' \"$a\"; done"
	out, err := osexec.CommandContext(t.Context(), "/bin/sh", "-c", script).Output() //nolint:gosec // G204: the script is this test's fixed words, quoted by the function under test
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Equal(got, words) {
		t.Fatalf("sh split the words as\n%q\nwant\n%q", got, words)
	}
}

func TestDeskAsksRemoteTarget(t *testing.T) {
	cases := []struct {
		name    string
		dirFlag string
		env     map[string]string
		want    *deskAsksRemote
		wantErr string
	}{
		{name: "unset", env: map[string]string{}},
		{name: "host", env: map[string]string{deskAsksHostEnv: "cameron-m5-mbp"}, want: &deskAsksRemote{Host: "cameron-m5-mbp", Bin: "forgectl"}},
		{name: "fqdn", env: map[string]string{deskAsksHostEnv: "m5.tail1234.ts.net"}, want: &deskAsksRemote{Host: "m5.tail1234.ts.net", Bin: "forgectl"}},
		{name: "dir flag wins", dirFlag: "/tmp/d", env: map[string]string{deskAsksHostEnv: "m5"}},
		{name: "DESK_DIR wins", env: map[string]string{deskAsksHostEnv: "m5", "DESK_DIR": "/d"}},
		{name: "CLAUDE_DESK_DIR wins", env: map[string]string{deskAsksHostEnv: "m5", "CLAUDE_DESK_DIR": "/d"}},
		{name: "bin path", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "/opt/homebrew/bin/forgectl"}, want: &deskAsksRemote{Host: "m5", Bin: "/opt/homebrew/bin/forgectl"}},
		{name: "bin name", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "forgectl-dev"}, want: &deskAsksRemote{Host: "m5", Bin: "forgectl-dev"}},
		{name: "option host", env: map[string]string{deskAsksHostEnv: "-oProxyCommand=sh"}, wantErr: "ssh host alias"},
		{name: "user at host", env: map[string]string{deskAsksHostEnv: "me@m5"}, wantErr: "ssh host alias"},
		{name: "path host", env: map[string]string{deskAsksHostEnv: "m5/x"}, wantErr: "ssh host alias"},
		{name: "space host", env: map[string]string{deskAsksHostEnv: "m5 x"}, wantErr: "ssh host alias"},
		{name: "dot first", env: map[string]string{deskAsksHostEnv: ".m5"}, wantErr: "ssh host alias"},
		{name: "too long", env: map[string]string{deskAsksHostEnv: strings.Repeat("a", 254)}, wantErr: "ssh host alias"},
		{name: "longest", env: map[string]string{deskAsksHostEnv: strings.Repeat("a", 253)}, want: &deskAsksRemote{Host: strings.Repeat("a", 253), Bin: "forgectl"}},
		{name: "relative bin", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "bin/forgectl"}, wantErr: "absolute path"},
		{name: "dash bin", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "-x"}, wantErr: "absolute path"},
		{name: "shell bin", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "/bin/forgectl;id"}, wantErr: "absolute path"},
		{name: "space bin", env: map[string]string{deskAsksHostEnv: "m5", deskAsksRemoteBinEnv: "/my bin/forgectl"}, wantErr: "absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deskAsksRemoteTarget(tc.dirFlag, func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || ExitCode(err) != deskExitUsage {
					t.Fatalf("err = %v (exit %d), want usage error containing %q", err, ExitCode(err), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDeskRemoteArgv(t *testing.T) {
	got := deskRemoteArgv(deskAsksRemote{Host: "m5", Bin: "/opt/bin/forgectl"}, []string{"asks", "--", "it's"})
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-T", "--", "m5",
		"'env'", "'FORGECTL_DESK_ASKS_HOST='", "'/opt/bin/forgectl'", "'desk'", "'asks'", "'--'", `'it'\''s'`}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n%q\nwant\n%q", got, want)
	}
}

func TestDeskRemoteResult(t *testing.T) {
	r := deskAsksRemote{Host: "m5", Bin: "forgectl"}
	cases := []struct {
		name     string
		code     int
		runErr   error
		wantExit int
		wantText []string
	}{
		{name: "ok", code: 0, wantExit: 0},
		{name: "remote refusal", code: 2, wantExit: 2},
		{name: "remote deadline", code: 75, wantExit: 75},
		{name: "ssh failed", code: 255, wantExit: 255, wantText: []string{"m5", "NOT recorded", "exit 255"}},
		{name: "no binary", code: 127, wantExit: 127, wantText: []string{"m5", deskAsksRemoteBinEnv, "NOT recorded"}},
		{name: "no ssh", code: -1, runErr: errors.New("executable file not found"), wantExit: 1, wantText: []string{"m5", "NOT recorded"}},
		{name: "signal", code: -1, wantExit: 1, wantText: []string{"m5", "NOT recorded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := deskRemoteResult("desk ask", r, tc.code, tc.runErr, "the ask was NOT recorded")
			if tc.wantExit == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || ExitCode(err) != tc.wantExit {
				t.Fatalf("err = %v, exit %d; want exit %d", err, ExitCode(err), tc.wantExit)
			}
			for _, s := range tc.wantText {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
		})
	}
}

// remoteCall is one call the stub runner saw.
type remoteCall struct {
	argv  []string
	stdin string
}

// stubDeskRemote swaps the ssh runner for one that records each call, writes
// out to stdout, and exits code. No test reaches a real ssh.
func stubDeskRemote(t *testing.T, code int, out string) *[]remoteCall {
	t.Helper()
	calls := &[]remoteCall{}
	prev := deskRemoteRun
	deskRemoteRun = func(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		c := remoteCall{argv: argv}
		if stdin != nil {
			b, err := io.ReadAll(stdin)
			if err != nil {
				return -1, err
			}
			c.stdin = string(b)
		}
		*calls = append(*calls, c)
		_, _ = io.WriteString(stdout, out)
		return code, nil
	}
	t.Cleanup(func() { deskRemoteRun = prev })
	return calls
}

// remoteWords strips the fixed ssh prefix and quoting from a stub call.
func remoteWords(t *testing.T, c remoteCall) []string {
	t.Helper()
	prefix := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-T", "--", "m5", "'env'", "'FORGECTL_DESK_ASKS_HOST='", "'forgectl'", "'desk'"}
	if len(c.argv) < len(prefix) || !slices.Equal(c.argv[:len(prefix)], prefix) {
		t.Fatalf("argv %q does not start with %q", c.argv, prefix)
	}
	words := make([]string, 0, len(c.argv)-len(prefix))
	for _, q := range c.argv[len(prefix):] {
		w := strings.TrimSuffix(strings.TrimPrefix(q, "'"), "'")
		words = append(words, strings.ReplaceAll(w, `'\''`, "'"))
	}
	return words
}

func TestDeskAsk_RemoteSendsStepsOnStdinAndTheLocalAsker(t *testing.T) {
	stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5", "FORGECTL_WORKER": "cos-test"})
	calls := stubDeskRemote(t, 0, "id=ask-1a2b3c4d\n")
	steps := writeTemp(t, "steps.md", "1. Open it's page\n2. $(do not run)\n")
	out, _, err := deskRun(t, deskDeps(), "ask", "--title", "-dash; title", "--steps-file", steps,
		"--bucket", "now", "--link", "Docs=https://example.com/a")
	wantExit(t, err, 0)
	if out != "id=ask-1a2b3c4d\n" {
		t.Fatalf("stdout %q, want the remote output unchanged", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d remote calls, want 1", len(*calls))
	}
	got := remoteWords(t, (*calls)[0])
	want := []string{"ask", "--title=-dash; title", "--bucket=now", "--allow=done,reply,link,snooze",
		"--link=Docs=https://example.com/a", "--from=cos-test", "--steps-file=-"}
	if !slices.Equal(got, want) {
		t.Fatalf("remote words\n%q\nwant\n%q", got, want)
	}
	if (*calls)[0].stdin != "1. Open it's page\n2. $(do not run)\n" {
		t.Fatalf("stdin %q", (*calls)[0].stdin)
	}
}

func TestDeskAsk_RemoteStdinStepsPassThrough(t *testing.T) {
	stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5"})
	prev := deskAskStdin
	deskAskStdin = strings.NewReader("from stdin\n")
	t.Cleanup(func() { deskAskStdin = prev })
	calls := stubDeskRemote(t, 0, "")
	_, _, err := deskRun(t, deskDeps(), "ask", "--title", "x", "--steps-file", "-")
	wantExit(t, err, 0)
	if (*calls)[0].stdin != "from stdin\n" {
		t.Fatalf("stdin %q", (*calls)[0].stdin)
	}
}

func TestDeskAsk_RemoteRefusesNotify(t *testing.T) {
	stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5", "HC_TERMINAL_ID": "t9z"})
	calls := stubDeskRemote(t, 0, "")
	_, _, err := deskRun(t, deskDeps(), "ask", "--title", "x", "--notify", "self")
	wantExit(t, err, deskExitUsage)
	if !strings.Contains(err.Error(), "--notify") || !strings.Contains(err.Error(), "m5") {
		t.Fatalf("error %q does not name --notify and the host", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused ask reached ssh: %q", *calls)
	}
}

func TestDeskAsk_RemoteSSHFailureIsNotSuccess(t *testing.T) {
	stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5"})
	stubDeskRemote(t, 255, "")
	_, _, err := deskRun(t, deskDeps(), "ask", "--title", "x")
	wantExit(t, err, 255)
	if !strings.Contains(err.Error(), "m5") || !strings.Contains(err.Error(), "NOT recorded") {
		t.Fatalf("error %q does not name the host and say the ask was not recorded", err)
	}
}

func TestDeskAsk_LocalWhenDeskDirIsSet(t *testing.T) {
	stubAskNotifier(t)
	dir := t.TempDir()
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5", "DESK_DIR": dir})
	calls := stubDeskRemote(t, 0, "")
	out, _, err := deskRun(t, deskDeps(), "ask", "--title", "local")
	wantExit(t, err, 0)
	if len(*calls) != 0 || !strings.Contains(out, "id=ask-") {
		t.Fatalf("calls %q, out %q: want a local ask", *calls, out)
	}
}

func TestDeskAsksVerbs_RemoteWords(t *testing.T) {
	stubAskNotifier(t)
	prevTTY := deskStdinIsTerminal
	deskStdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { deskStdinIsTerminal = prevTTY })
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"asks"}, []string{"asks"}},
		{[]string{"asks", "--all", "--json"}, []string{"asks", "--all", "--json"}},
		{[]string{"asks", "ask-1a2b3c4d"}, []string{"asks", "--", "ask-1a2b3c4d"}},
		{[]string{"respond", "ask-1a2b3c4d", "reply", "--", "-x; $(y)"}, []string{"respond", "--", "ask-1a2b3c4d", "reply", "-x; $(y)"}},
		{[]string{"respond", "--json", "ask-1a2b3c4d", "snooze", "4h"}, []string{"respond", "--json", "--", "ask-1a2b3c4d", "snooze", "4h"}},
		{[]string{"watch", "--responses", "ask-1a2b3c4d"}, []string{"watch", "--responses", "--deadline=540", "--", "ask-1a2b3c4d"}},
		{[]string{"watch", "--responses", "--deadline", "0", "--since", "12"}, []string{"watch", "--responses", "--deadline=0", "--since=12"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5"})
			calls := stubDeskRemote(t, 0, "")
			_, _, err := deskRun(t, deskDeps(), tc.args...)
			wantExit(t, err, 0)
			if len(*calls) != 1 {
				t.Fatalf("%d calls", len(*calls))
			}
			if got := remoteWords(t, (*calls)[0]); !slices.Equal(got, tc.want) {
				t.Fatalf("words %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeskAsksVerbs_RemoteRefusesBadInputLocally(t *testing.T) {
	stubAskNotifier(t)
	for _, args := range [][]string{
		{"asks", "Not An Id"},
		{"respond", "ask-1a2b3c4d", "approve"},
		{"respond", "ask-1a2b3c4d", "snooze", "forever"},
		{"respond", "$(id)", "done"},
		{"watch", "--responses", "x;y"},
	} {
		stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5"})
		calls := stubDeskRemote(t, 0, "")
		_, _, err := deskRun(t, deskDeps(), args...)
		if ExitCode(err) != deskExitUsage || err == nil {
			t.Fatalf("%q: err %v, want a usage error", args, err)
		}
		if len(*calls) != 0 {
			t.Fatalf("%q reached ssh", args)
		}
	}
}

func TestDeskAsksVerbs_RemoteExitPassesThrough(t *testing.T) {
	stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{deskAsksHostEnv: "m5"})
	stubDeskRemote(t, 75, "resume=forgectl desk watch --responses --since 9\n")
	var out bytes.Buffer
	err := deskRunTo(t, deskDeps(), &out, io.Discard, "watch", "--responses", "--deadline", "5")
	wantExit(t, err, 75)
	if err.Error() != "" {
		t.Fatalf("a remote exit should print nothing more, got %q", err)
	}
	if !strings.HasPrefix(out.String(), "resume=") {
		t.Fatalf("stdout %q", out.String())
	}
}
