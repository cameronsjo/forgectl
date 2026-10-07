// Package ghfail turns a failed gh call into a note that says why it failed
// and what fixes it, without rendering anything gh wrote.
//
// The inventory paths (status, review, projects, pr dash) used to print a
// bare "query failed": gh's stderr can carry tokens and terminal control
// bytes, so it is never rendered, and the cause went only to a log that is
// off by default. The operator then had to run doctor to learn that gh was
// not signed in (forgectl#1148). Note keeps that guarantee: it matches gh's
// stderr and exit status against fixed patterns and renders only fixed text
// plus a host the caller has already validated.
package ghfail

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"regexp"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Cause is the category of a failed gh call.
type Cause int

const (
	// Other is any failure no pattern below recognizes.
	Other Cause = iota
	// Missing means gh is not on PATH.
	Missing
	// Timeout means the call hit its deadline.
	Timeout
	// Canceled means the caller canceled the call.
	Canceled
	// NotSignedIn means gh has no credential for the host (gh exits 4).
	NotSignedIn
	// TokenRejected means the host refused the credential gh sent.
	TokenRejected
	// RateLimited means the host throttled the call.
	RateLimited
	// Unreachable means the host could not be reached at all.
	Unreachable
)

// ghExitAuth is gh's exit status when a command needs authentication and has
// none ("To get started with GitHub CLI, please run: gh auth login").
const ghExitAuth = 4

var (
	reNotSignedIn   = regexp.MustCompile(`(?i)gh auth login|not logged in(to)? any|authentication required`)
	reTokenRejected = regexp.MustCompile(`(?i)HTTP 401|bad credentials`)
	reRateLimited   = regexp.MustCompile(`(?i)rate limit|HTTP 429|abuse detection`)
	reUnreachable   = regexp.MustCompile(`(?i)error connecting to|no such host|connection refused|network is unreachable|i/o timeout|tls handshake`)
)

// causeNames are Categorized's fixed error texts.
var causeNames = map[Cause]string{
	Other:         "unrecognized failure",
	Missing:       "gh not installed",
	Timeout:       "timed out",
	Canceled:      "canceled",
	NotSignedIn:   "gh not signed in",
	TokenRejected: "credential rejected",
	RateLimited:   "rate limited",
	Unreachable:   "host unreachable",
}

// Categorized is an error that carries only a Cause. A caller that must drop
// a raw gh error before returning it (its text can carry gh's stderr) wraps
// Categorize(err) instead, so Classify further up still finds the cause.
type Categorized struct{ Cause Cause }

func (e *Categorized) Error() string { return causeNames[e.Cause] }

// Categorize returns err's category as an error whose text is fixed.
func Categorize(err error) error { return &Categorized{Cause: Classify(err)} }

// Classify returns err's category. It reads a wrapped *exec.CommandError's
// stderr and exit code only to match them; nothing it reads is returned.
func Classify(err error) Cause {
	var cat *Categorized
	switch {
	case err == nil:
		return Other
	case errors.As(err, &cat):
		return cat.Cause
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, osexec.ErrNotFound):
		return Missing
	}
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		return Other
	}
	switch {
	case reTokenRejected.MatchString(ce.Stderr):
		return TokenRejected
	case ce.ExitCode == ghExitAuth || reNotSignedIn.MatchString(ce.Stderr):
		return NotSignedIn
	case reRateLimited.MatchString(ce.Stderr):
		return RateLimited
	case reUnreachable.MatchString(ce.Stderr):
		return Unreachable
	}
	return Other
}

// tokenEnvVars are the variables gh takes a credential from ahead of its
// stored login. When one is set, `gh auth login` does not change what gh
// sends, so the fix names the variable instead.
var tokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

func tokenFromEnv() string {
	for _, name := range tokenEnvVars {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return name
		}
	}
	return ""
}

// reHost is a lowercase DNS name. Reason renders a host only when it matches,
// because some callers pass the configured value unvalidated and a config
// file is no place to take terminal text from.
var reHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

const maxHostBytes = 253

// Reason is the fixed text for err's cause: what went wrong and, where one
// exists, the command that fixes it. An empty host means github.com; a host
// that is not a plain DNS name is rendered as "the GitHub host".
func Reason(err error, host string) string {
	login := "gh auth login"
	switch {
	case host == "" || host == "github.com":
		host = "github.com"
	case len(host) <= maxHostBytes && reHost.MatchString(host):
		login += " --hostname " + host
	default:
		host = "the GitHub host"
	}
	switch Classify(err) {
	case Missing:
		return "gh is not installed; install it from https://cli.github.com"
	case Timeout:
		return "timed out"
	case Canceled:
		return "canceled"
	case NotSignedIn:
		return fmt.Sprintf("gh is not signed in to %s; run %s", host, login)
	case TokenRejected:
		if name := tokenFromEnv(); name != "" {
			return fmt.Sprintf("%s rejected the token in %s; replace it, or unset it and run %s", host, name, login)
		}
		return fmt.Sprintf("%s rejected gh's credential; run %s", host, login)
	case RateLimited:
		return fmt.Sprintf("%s rate limit reached; retry in a few minutes", host)
	case Unreachable:
		return fmt.Sprintf("could not reach %s; check the network or proxy", host)
	}
	return "run forgectl doctor for the cause"
}

// Note renders a degraded query's note: "<label>: query failed (<reason>)".
// label must be fixed or validated text (an owner login, a section name).
func Note(label string, err error, host string) string {
	return fmt.Sprintf("%s: query failed (%s)", label, Reason(err, host))
}
