package tasks

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// DefaultWriteKeychainService is the login-keychain service name
// `forgectl tasks done` reads when no override is given. It is a different
// entry from DefaultKeychainService on purpose: the token that may update a
// task is stored, named, and revoked apart from the one that may only read.
const DefaultWriteKeychainService = "vikunja-write"

// AllowedHostsConfigKey names the config key that widens the host rule, as an
// operator would look for it in the file. Every refusal under the rule says
// it, because adding a host there is the only fix.
const AllowedHostsConfigKey = "[tasks] allowed_hosts"

// CheckAllowedHost is the rule for where a keychain credential may be sent:
// to DefaultHost, or to a host in allowed (the user's config list), and to
// nothing else. A nil or empty list allows the default host only.
//
// The host pin (checkHostPinning) accepts any public address and leaves the
// rest to TLS, so without this rule a host named on a command line receives
// whatever keychain token the command reads, as long as it holds a valid
// certificate for its own name. A flag is the wrong place to decide that; the
// config file is the operator's.
//
// host must be a plain hostname. It is compared lower-cased and whole, so a
// name that only starts or ends with an allowed one does not match, and
// neither does the same name with a port, a user, a path, or a trailing dot.
// An entry in allowed that is not itself a plain hostname can never match:
// no host that passes the grammar equals it.
//
// The refusal satisfies IsHostRefused.
func CheckAllowedHost(host string, allowed []string) error {
	if !config.PlainHostname(host) {
		return fmt.Errorf("%w: %s is not a plain hostname (letters, digits, '.' and '-'; no port, user, path, trailing dot, or IP address). "+
			"A keychain credential goes only to %s or to a hostname listed under %s in the forgectl config file",
			ErrHostRefused, termsafe.QuoteArgMax(host, 0), DefaultHost, AllowedHostsConfigKey)
	}
	name := strings.ToLower(host)
	if name == strings.ToLower(DefaultHost) {
		return nil
	}
	for _, entry := range allowed {
		if name == strings.ToLower(entry) {
			return nil
		}
	}
	return fmt.Errorf("%w: a keychain credential goes only to %s or to a host listed under %s in the forgectl config file, and %s is neither. "+
		"To use this host, add it to that list",
		ErrHostRefused, DefaultHost, AllowedHostsConfigKey, termsafe.QuoteArgMax(host, 0))
}

// keychainServiceRe is the shape of a keychain service name this client will
// read: what a name chosen for one needs, and nothing a shell, a terminal, or
// a reader of an error message could take for something else.
var keychainServiceRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidKeychainService reports whether name is a keychain service name this
// client will read, or print as part of a command for an operator to run.
func ValidKeychainService(name string) bool {
	return keychainServiceRe.MatchString(name)
}

// CheckKeychainService refuses a keychain service name outside
// ValidKeychainService. A caller checks before ReadToken: the name is an
// argument to the keychain tool and is echoed in that read's own errors.
func CheckKeychainService(name string) error {
	if ValidKeychainService(name) {
		return nil
	}
	return fmt.Errorf("tasks: %s is not a keychain service name this client will read: use 1 to 64 letters, digits, '.', '_' or '-'",
		termsafe.QuoteArgMax(name, 0))
}
