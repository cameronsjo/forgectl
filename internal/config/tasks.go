package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// TasksConfig is the [tasks] section: the hosts, beyond the built-in default,
// that `forgectl tasks` may send a keychain credential to. A zero value means
// "section absent" and allows the default host only.
//
// The list lives in the user's config file and nowhere else. A host named on
// the command line is checked against it; nothing on the command line can add
// to it.
type TasksConfig struct {
	// AllowedHosts are plain hostnames, compared lower-cased and whole. An
	// entry that is not a plain hostname is a load error (Validate).
	AllowedHosts []string `toml:"allowed_hosts"`
}

// tasksAllowedHostsKey is the dotted key of AllowedHosts, as Report names a
// key: the one Validate can refuse.
const tasksAllowedHostsKey = "tasks.allowed_hosts"

// IsZero reports whether the [tasks] section was absent or empty.
func (tc TasksConfig) IsZero() bool {
	return len(tc.AllowedHosts) == 0
}

// invalidValueError is a config value that decoded and is not valid: a
// [tasks] allowed_hosts entry, or a log_level the logger does not know. It is
// a type of its own so the loader can word the file as invalid, and not as one
// that does not parse.
type invalidValueError struct {
	message string
}

func (e invalidValueError) Error() string { return e.message }

// isInvalidValueError reports whether err is a refused config value.
func isInvalidValueError(err error) bool {
	var refused invalidValueError
	return errors.As(err, &refused)
}

// Validate reports the first allowed_hosts entry that is not a plain hostname,
// naming the key and the entry. An entry carrying a port, a user, a path, or
// an address would otherwise sit in the list looking like it allows something,
// while matching no host this client would ever accept.
func (tc TasksConfig) Validate() error {
	for i, host := range tc.AllowedHosts {
		if !PlainHostname(host) {
			return invalidValueError{message: fmt.Sprintf(
				"[tasks].allowed_hosts[%d] = %s: must be a plain hostname (letters, digits, '.' and '-'), "+
					"with no port, user, path, trailing dot, empty label, or IP address",
				i, quoteConfigValue(host))}
		}
	}
	return nil
}

// maxHostnameBytes is the longest name DNS carries.
const maxHostnameBytes = 253

// PlainHostname reports whether host is a plain DNS hostname: dot-separated
// labels of ASCII letters, digits and '-', at most 253 bytes in all.
//
// It is the grammar for a host a credential may be sent to, so it is written
// to admit one thing and refuse everything else. There is no ':' (a port, or
// an IPv6 literal), no '@' (userinfo), no '/', '?' or '#' (a path), no '%'
// (an escape or a zone), no space or control character, no empty label (so no
// leading, trailing or doubled dot), and no name whose last label is a number
// — which is how an IPv4 address is spelled, in dotted, short, single-number
// or hex form.
func PlainHostname(host string) bool { return plainHostname(host, false) }

// PlainServiceHostname is PlainHostname that also admits '_' inside a label.
// A compose service name uses it and a DNS hostname does not; this form is
// for an address forgectl dials on its own network, never for a host a
// credential is sent to.
func PlainServiceHostname(host string) bool { return plainHostname(host, true) }

func plainHostname(host string, underscore bool) bool {
	if host == "" || len(host) > maxHostnameBytes {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			case c == '_' && underscore:
			default:
				return false
			}
		}
	}
	return !numericLabel(labels[len(labels)-1])
}

// numericLabel reports whether label is a number as an address parser reads
// one: all decimal digits, or "0x" followed by hex digits. No real top-level
// domain is a number, and a name that ends in one is an IPv4 address to the
// resolvers that accept the short and hex spellings.
func numericLabel(label string) bool {
	digits, isHexDigit := label, false
	if len(label) >= 2 && label[0] == '0' && (label[1] == 'x' || label[1] == 'X') {
		digits, isHexDigit = label[2:], true
	}
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		switch {
		case c >= '0' && c <= '9':
		case isHexDigit && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')):
		default:
			return false
		}
	}
	return true
}

// TasksCloseLogPath returns the file `forgectl tasks done` and the stdio MCP
// server append close records to: <os.UserConfigDir()>/forgectl/
// tasks-closes.jsonl, beside TasksCachePath. One JSON object per line. Most
// are close records: a call that sent an update to the board (or, from the MCP
// server, one refused by its per-session limit). The stdio MCP server also
// writes a board-write line for each create_task or add_comment write, and
// the rest are host refusals: a `tasks` verb stopped by the allowed-host rule
// before it read the keychain.
//
// It is written whatever log_level is, and is not one of the daily log files:
// nothing prunes it. It holds no credential — a record names where the token
// came from, never the token. The user who owns the directory can edit or
// delete the file, so it is a record of what this machine's forgectl did, and
// not evidence against that user.
func TasksCloseLogPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tasks-closes.jsonl"), nil
}
