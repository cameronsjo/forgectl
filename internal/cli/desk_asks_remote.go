// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	osexec "os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Asks on another machine. The desk directory is machine-local, and a laptop
// sleeps, so one always-on host can hold the asks log for every machine. With
// $FORGECTL_DESK_ASKS_HOST set (and no --dir, $DESK_DIR or $CLAUDE_DESK_DIR),
// desk ask, desk asks, desk respond and desk watch --responses run there over
// ssh. Staged scripts stay local: they run where they are staged.
const (
	deskAsksHostEnv      = "FORGECTL_DESK_ASKS_HOST"
	deskAsksRemoteBinEnv = "FORGECTL_DESK_ASKS_REMOTE_BIN"
	// deskAsksDefaultRemoteBin is the remote binary when the variable is
	// unset: found on the remote PATH.
	deskAsksDefaultRemoteBin = "forgectl"
	// deskExitSSH is ssh's own failure code (no connection, refused auth, a
	// dropped link). It is passed through so a caller can tell it from a
	// refusal by the remote forgectl.
	deskExitSSH = 255
	// deskExitNotFound is a POSIX shell's "command not found".
	deskExitNotFound = 127
)

var (
	// deskAsksHostRe is an ssh config alias or a hostname. No user@, no
	// path, no option: a leading "-" or an "@" is refused.
	deskAsksHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	// deskAsksBinNameRe is a bare binary name, found on the remote PATH.
	deskAsksBinNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// deskAsksBinPathRe is an absolute path of plain path characters.
	deskAsksBinPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// deskAsksRemote is where asks go: an ssh destination and the forgectl
// binary to run there.
type deskAsksRemote struct {
	Host string
	Bin  string
}

// deskAsksRemoteTarget decides whether the asks verbs run on another host.
// It returns nil (run here) when $FORGECTL_DESK_ASKS_HOST is unset, or when
// --dir, $DESK_DIR or $CLAUDE_DESK_DIR names a desk: a desk named here is
// used here. A set host or binary of the wrong shape is a usage error, never
// a silent local fallback.
func deskAsksRemoteTarget(dirFlag string, getenv func(string) string) (*deskAsksRemote, error) {
	host := getenv(deskAsksHostEnv)
	if host == "" || dirFlag != "" || getenv("DESK_DIR") != "" || getenv("CLAUDE_DESK_DIR") != "" {
		return nil, nil
	}
	if !deskAsksHostRe.MatchString(host) {
		return nil, deskUsage("desk: $%s must be an ssh host alias or hostname (letters, digits, '.', '_', '-'; starting with a letter or digit; no user@, path or space), got %s",
			deskAsksHostEnv, strconv.QuoteToASCII(clipRunes(host, deskQuoteMax)))
	}
	bin := getenv(deskAsksRemoteBinEnv)
	if bin == "" {
		bin = deskAsksDefaultRemoteBin
	}
	if !deskAsksBinNameRe.MatchString(bin) && !deskAsksBinPathRe.MatchString(bin) {
		return nil, deskUsage("desk: $%s must be an absolute path (/ then letters, digits, '.', '_', '-', '/') or a bare name (letters, digits, '.', '_', '-'), got %s",
			deskAsksRemoteBinEnv, strconv.QuoteToASCII(clipRunes(bin, deskQuoteMax)))
	}
	return &deskAsksRemote{Host: host, Bin: bin}, nil
}

// clipRunes cuts s to at most n runes.
func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// shSingleQuote quotes s as one word for a POSIX shell: wrapped in single
// quotes, each single quote written as close, backslash-quote, reopen
// (the four bytes quote, backslash, quote, quote). Inside single quotes a POSIX shell gives no
// character a meaning, so $(), backticks, ;, newlines and leading dashes all
// stay literal text. (fish and other non-POSIX shells read backslashes inside
// single quotes and are not supported as the remote login shell.)
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// deskRemoteArgv is the ssh argv that runs "forgectl desk <words...>" on the
// host. ssh joins every word after the host into one string that the remote
// login shell parses, so each remote word is single-quoted here, the binary
// included. The remote command runs under env with $FORGECTL_DESK_ASKS_HOST
// emptied, so a host whose own environment names itself (shared dotfiles)
// uses its local desk instead of calling ssh again.
func deskRemoteArgv(r deskAsksRemote, words []string) []string {
	remote := append([]string{"env", deskAsksHostEnv + "=", r.Bin, "desk"}, words...)
	argv := make([]string, 0, 12+len(remote))
	// ConnectTimeout bounds an unreachable host; the keepalives end a session
	// whose link died mid-watch instead of letting it hang past --deadline.
	argv = append(argv, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-T", "--", r.Host)
	for _, w := range remote {
		argv = append(argv, shSingleQuote(w))
	}
	return argv
}

// deskRemoteRunner runs argv with the given streams and returns its exit
// code. err is set only when the command could not run at all. Tests swap
// deskRemoteRun so no test reaches a real ssh.
type deskRemoteRunner func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

var deskRemoteRun deskRemoteRunner = execDeskRemote

// execDeskRemote runs argv with os/exec: no local shell is involved.
func execDeskRemote(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c := osexec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is ssh with a validated host and single-quoted words (deskRemoteArgv)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err := c.Run()
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// deskRemoteResult turns a remote run's outcome into the command's error.
// The remote forgectl has already written its own output and error, so its
// non-zero codes pass through silently. ssh's own failure (255), a missing
// remote binary (127), and an ssh that could not run are reported here, with
// lost saying what did not happen ("the ask was NOT recorded").
func deskRemoteResult(verb string, r deskAsksRemote, code int, runErr error, lost string) error {
	switch {
	case runErr != nil:
		return fmt.Errorf("%s: could not run ssh to reach %s ($%s): %w; %s", verb, r.Host, deskAsksHostEnv, runErr, lost)
	case code == 0:
		return nil
	case code == deskExitSSH:
		return WithExitCode(fmt.Errorf("%s: ssh to %s ($%s) failed (exit 255: no connection, or ssh refused it); %s", verb, r.Host, deskAsksHostEnv, lost), deskExitSSH)
	case code == deskExitNotFound:
		return WithExitCode(fmt.Errorf("%s: %s could not run %s (exit 127, not found); set $%s to its absolute path there; %s", verb, r.Host, r.Bin, deskAsksRemoteBinEnv, lost), deskExitNotFound)
	case code < 0:
		return fmt.Errorf("%s: ssh to %s ended without an exit code (killed by a signal); %s", verb, r.Host, lost)
	}
	return newSilentCodedError(code)
}

// askRemoteWords are the remote words for desk ask. Every flag value goes as
// --flag=value, so a value that starts with "-" stays a value. from is the
// asker resolved here, so the host records who really asked. withSteps sends
// --steps-file - and the steps travel on ssh's stdin.
func askRemoteWords(o deskAskOpts, from string, withSteps bool) []string {
	w := []string{"ask", "--title=" + o.title, "--bucket=" + o.bucket, "--allow=" + o.allow}
	for _, l := range o.links {
		w = append(w, "--link="+l)
	}
	if from != "" {
		w = append(w, "--from="+from)
	}
	if withSteps {
		w = append(w, "--steps-file=-")
	}
	if o.asJSON {
		w = append(w, "--json")
	}
	return w
}

// asksRemoteWords are the remote words for desk asks [ID]. Positionals go
// after "--" so none is read as a flag.
func asksRemoteWords(id string, all, asJSON bool) []string {
	w := []string{"asks"}
	if all {
		w = append(w, "--all")
	}
	if asJSON {
		w = append(w, "--json")
	}
	if id != "" {
		w = append(w, "--", id)
	}
	return w
}

// respondRemoteWords are the remote words for desk respond: the arguments as
// given, after "--", so a reply that starts with "-" stays a reply.
func respondRemoteWords(args []string, asJSON bool) []string {
	w := []string{"respond"}
	if asJSON {
		w = append(w, "--json")
	}
	return append(append(w, "--"), args...)
}

// watchResponsesRemoteWords are the remote words for desk watch --responses.
// deadline is already resolved here (the remote stdin is never a terminal,
// so the remote cannot tell); since < 0 means "not given".
func watchResponsesRemoteWords(id string, deadline int, since int64) []string {
	w := []string{"watch", "--responses", "--deadline=" + strconv.Itoa(deadline)}
	if since >= 0 {
		w = append(w, "--since="+strconv.FormatInt(since, 10))
	}
	if id != "" {
		w = append(w, "--", id)
	}
	return w
}
