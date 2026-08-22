// Package proxy renders the deliberately narrow shell protocol used to apply
// a named proxy profile to a caller's current shell.
package proxy

import (
	"errors"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// ErrEmptyProfile reports a named profile that would be indistinguishable
// from the explicit off operation.
var ErrEmptyProfile = errors.New("proxy: profile has no values; use the explicit off operation")

// ErrUnrepresentable reports a value containing NUL. Shell variables cannot
// contain NUL, and the error deliberately names neither the field nor value.
var ErrUnrepresentable = errors.New("proxy: profile contains a value the shell cannot represent")

type variable struct {
	lower string
	upper string
	value string
}

// Use renders one fixed batch of export/unset builtins for profile. Every
// configured value is assigned to its lowercase and uppercase spellings in
// the same export command; every absent value unsets both spellings. The
// complete batch is built before any bytes reach stdout, so an invalid value
// cannot leave a wrapper with a partial script.
func Use(profile config.ProxyProfile) (string, error) {
	if profile.IsZero() {
		return "", ErrEmptyProfile
	}
	variables := []variable{
		{lower: "http_proxy", upper: "HTTP_PROXY", value: profile.HTTPProxy},
		{lower: "https_proxy", upper: "HTTPS_PROXY", value: profile.HTTPSProxy},
		{lower: "all_proxy", upper: "ALL_PROXY", value: profile.AllProxy},
		{lower: "no_proxy", upper: "NO_PROXY", value: profile.NoProxy},
	}

	commands := make([]string, 0, len(variables))
	for _, v := range variables {
		if v.value == "" {
			commands = append(commands, "unset "+v.upper+" "+v.lower)
			continue
		}
		quoted, err := quote(v.value)
		if err != nil {
			return "", err
		}
		commands = append(commands, "export "+v.upper+"="+quoted+" "+v.lower+"="+quoted)
	}
	return strings.Join(commands, "; "), nil
}

// Off renders the fixed unset batch. It intentionally clears all supported
// upper- and lower-case spellings rather than guessing which profile is live.
func Off() string {
	return "unset HTTP_PROXY http_proxy HTTPS_PROXY https_proxy ALL_PROXY all_proxy NO_PROXY no_proxy"
}

// quote returns one POSIX/zsh shell word. Single quotes protect every shell
// metacharacter; an embedded quote closes the run, emits a backslash-escaped
// quote, then reopens it. NUL is the one byte a shell variable cannot carry.
func quote(value string) (string, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return "", ErrUnrepresentable
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'", nil
}
