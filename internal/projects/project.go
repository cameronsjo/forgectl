// Package projects discovers and opens local project directories. It builds a
// unified cross-host inventory — local clones plus uncloned repos on both
// github.com and the self-hosted Gitea (git.sjo.lol) — so "find my project"
// works regardless of which host it lives on or whether it's checked out.
package projects

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// Project is a single entry in the local project list (the legacy local-only
// view used by Discover and the interactive picker's status grouping).
type Project struct {
	Name   string
	Dir    string
	Status GitStatus
}

// Repo is one entry in the unified cross-host inventory. A Repo may be a local
// clone (Cloned, with LocalPath + Status populated) or a remote repo not yet
// checked out (Cloned == false). Identity is (Host, Owner, Name); local clones
// derive it from their origin remote, never from the bare directory name, so a
// repo that exists on both hosts stays two distinct rows.
type Repo struct {
	// Host is the normalized hostname — "github.com", "git.sjo.lol",
	// "github.example.com" — or "" for a local-only repo with no parseable
	// origin. It is the `projects list --json` wire contract; it carried the
	// short tokens "github"/"gitea" through v0.15.0.
	Host      string    `json:"host"`
	Owner     string    `json:"owner"`            // "cameronsjo" on github, "cameron" on gitea
	Name      string    `json:"name"`             // repo name
	SSHURL    string    `json:"sshUrl,omitempty"` // clone URL (gitea: ssh://…:222 form)
	Mirror    bool      `json:"mirror,omitempty"` // gitea mirror repo
	Private   bool      `json:"private,omitempty"`
	Cloned    bool      `json:"cloned"`
	LocalPath string    `json:"localPath,omitempty"` // set when Cloned
	Status    GitStatus `json:"status"`              // working-tree state when Cloned; zero otherwise
}

// StatusState distinguishes the three outcomes a working-tree check can have.
// The zero value is StatusUnknown on purpose: a GitStatus that never went
// through gitStatus must never read as a clean tree.
type StatusState string

const (
	StatusUnknown StatusState = ""           // never inspected, or the check failed
	StatusNotRepo StatusState = "not-a-repo" // directory has no .git
	StatusOK      StatusState = "ok"         // git status ran and was parsed
)

// MarshalJSON keeps StatusUnknown fail-closed in Go while making it explicit
// on the wire. The empty string is useful as the language zero value but reads
// as absent information to a JSON consumer, alongside reassuring zero counts.
func (s StatusState) MarshalJSON() ([]byte, error) {
	if s == StatusUnknown {
		// termsafe:allow-raw-json custom wire value, with escaping applied by the output encoder
		return json.Marshal("unknown")
	}
	// termsafe:allow-raw-json custom wire value, with escaping applied by the output encoder
	return json.Marshal(string(s))
}

// UnmarshalJSON is MarshalJSON's inverse: a status emitted as "unknown" must
// recover the internal zero value rather than becoming an unrecognized fourth
// state that the human renderer would label merely "cloned".
func (s *StatusState) UnmarshalJSON(data []byte) error {
	var wire *string
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire == nil {
		return nil
	}
	if *wire == "unknown" {
		*s = StatusUnknown
		return nil
	}
	*s = StatusState(*wire)
	return nil
}

// GitStatus summarises the working-tree state of a project directory.
type GitStatus struct {
	State     StatusState `json:"state"`
	Modified  int         `json:"modified"`
	Untracked int         `json:"untracked"`
	Ahead     int         `json:"ahead"`
}

// Label returns a short human-readable badge: "[clean]", "[2 ahead]",
// "[3 modified]", etc. Returns "" when the tree state isn't a successfully
// parsed git status — a non-git directory or a failed check — so the caller
// decides how to say so rather than defaulting to a false "clean".
func (gs GitStatus) Label() string {
	if gs.State != StatusOK {
		return ""
	}
	if gs.Modified == 0 && gs.Untracked == 0 && gs.Ahead == 0 {
		return "[clean]"
	}
	if gs.Ahead > 0 && gs.Modified == 0 && gs.Untracked == 0 {
		return fmt.Sprintf("[%d ahead]", gs.Ahead)
	}
	var parts string
	if gs.Modified > 0 {
		parts = fmt.Sprintf("%d modified", gs.Modified)
	}
	if gs.Untracked > 0 {
		if parts != "" {
			parts += ", "
		}
		parts += fmt.Sprintf("%d untracked", gs.Untracked)
	}
	return "[" + parts + "]"
}

// DisplayLine builds the label shown in the interactive picker.
func (p Project) DisplayLine() string {
	label := p.Status.Label()
	if label == "" {
		return p.Name
	}
	return p.Name + " " + label
}

// hostBadge returns the host marker for inventory display: the hostname
// itself, or "local" for a repo with no parseable origin.
//
// It deliberately does NOT map hostnames to friendlier labels. This is a Repo
// method with no access to the deployment's configured hosts, so any mapping
// would have to hardcode github.com — which silently drops a GitHub Enterprise
// host's badge, the one case where knowing the host matters most.
func (r Repo) hostBadge() string {
	if r.Host == "" {
		return "local"
	}
	return r.Host
}

// DisplayLine builds the label shown in the cross-host picker: host marker,
// repo name, and a cloned/uncloned badge (with working-tree status when known).
func (r Repo) DisplayLine() string {
	var badge string
	if r.Cloned {
		badge = r.Status.Label()
		if badge == "" {
			switch r.Status.State {
			case StatusNotRepo:
				badge = "[not-a-repo]"
			case StatusUnknown:
				badge = "[unknown]"
			default:
				badge = "[cloned]"
			}
		}
	} else {
		badge = "[uncloned]"
	}
	name := r.Name
	if r.Mirror {
		name += " (mirror)"
	}
	// Width 18 fits "github.example.com" and "git.sjo.lol"; a longer host
	// simply pushes the rest of the line right rather than truncating, since
	// the badge is the only thing that identifies which forge a row is on.
	return fmt.Sprintf("%-18s %s %s", r.hostBadge(), name, badge)
}

// Key returns the dedup identity for a Repo. Repos with a parseable host+owner
// key by host/owner/name (case-insensitive); local-only repos with no parseable
// origin key by their local path so they never collide with a remote entry.
func (r Repo) Key() string {
	if r.Host == "" || r.Owner == "" || r.Name == "" {
		return "local:" + r.LocalPath
	}
	return strings.ToLower(r.Host + "/" + r.Owner + "/" + r.Name)
}

// parseRemoteURL extracts (host, owner, name) from a git remote URL. host is
// canonicalHost's normalized hostname, so a local clone's origin dedups
// against the remote-list rows for the same host. Returns ("","","") when the
// URL can't be parsed into owner/name.
//
// Handles the three forms in play:
//
//	git@github.com:cameronsjo/forgectl.git              (scp-like)
//	https://github.com/cameronsjo/forgectl(.git)        (https)
//	ssh://git@git.sjo.lol:222/cameron/homeclaw.git      (ssh with port)
func parseRemoteURL(raw, gitHubHost string) (host, owner, name string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}

	var hostname, path string
	switch {
	case strings.HasPrefix(raw, "ssh://"), strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "http://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", ""
		}
		hostname = u.Hostname()
		path = strings.TrimPrefix(u.Path, "/")
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		// scp-like: git@host:owner/name.git — the ":" must come *after* the "@".
		at := strings.Index(raw, "@")
		rel := strings.Index(raw[at+1:], ":")
		if rel < 0 {
			// Colon only before the "@" (e.g. git://user:pass@host/repo) — not a
			// form we parse. Guard prevents a low>high slice panic on raw[at+1:colon].
			return "", "", ""
		}
		colon := at + 1 + rel
		hostname = raw[at+1 : colon]
		path = raw[colon+1:]
	default:
		return "", "", ""
	}

	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return canonicalHost(hostname, gitHubHost), "", ""
	}
	owner = parts[len(parts)-2]
	name = parts[len(parts)-1]
	return canonicalHost(hostname, gitHubHost), owner, name
}

// ParseCloneTarget interprets a `projects clone` positional argument as an
// explicit clone target — a full git URL or a bare "owner/repo" shorthand —
// bypassing the inventory/query search entirely (absorbs git-smart-clone's
// URL-argument mode). Returns ok=false when arg parses as neither, so the
// caller falls back to the existing query-search behavior.
//
// A recognized non-github URL carries the raw arg forward as SSHURL — Clone's
// default-host branch clones it as a literal URL (cloneFromGitea runs a plain
// `git clone`, not a Gitea-specific one, despite the name), so an https URL
// works there too, not just ssh.
//
// gitHubHost is the deployment's configured GitHub host (githubauth
// ResolveHost output): the bare "owner/repo" shorthand means "on the
// configured GitHub host" — deployment-scoped, not github.com-scoped —
// because the clone it selects runs through gh pinned to that host. An empty
// value means the default, matching canonicalHost's zero-value defense; both
// go through the same fallback so a struct-literal caller cannot get a Repo
// whose Host misses the dispatch.
func ParseCloneTarget(arg, gitHubHost string) (Repo, bool) {
	if gitHubHost == "" {
		gitHubHost = githubauth.DefaultHost
	}
	if host, owner, name := parseRemoteURL(arg, gitHubHost); name != "" {
		r := Repo{Host: host, Owner: owner, Name: name}
		// Only a URL on the configured GitHub host clones through gh, which
		// supplies its own URL. Everything else carries the operator's literal
		// argument forward as the clone URL.
		if host != gitHubHost {
			r.SSHURL = arg
		}
		return r, true
	}
	if owner, name, ok := splitOwnerRepo(arg); ok {
		return Repo{Host: gitHubHost, Owner: owner, Name: name}, true
	}
	return Repo{}, false
}

// splitOwnerRepo splits a bare "owner/repo" shorthand (no scheme, no host) —
// the shorthand for a GitHub clone, e.g. `projects clone anthropics/claude-code`.
func splitOwnerRepo(s string) (owner, name string, ok bool) {
	if strings.Count(s, "/") != 1 {
		return "", "", false
	}
	owner, name, _ = strings.Cut(s, "/")
	if !validPathSegment(owner) || !validPathSegment(name) {
		return "", "", false
	}
	return owner, name, true
}

// canonicalHost normalizes a remote hostname into the inventory's host
// identity: lowercased, with one trailing ":port" and one trailing "." (the
// FQDN root label) stripped. Every host is its own identity — there are no
// short tokens.
//
// THE TOKENS ARE WHY THIS RETURNS A HOSTNAME. It used to return "github" or
// "gitea" for the two known hosts and the raw hostname for everything else —
// two kinds of value in one type, with nothing distinguishing them. A remote
// whose bare hostname was literally "github" therefore came out of the DEFAULT
// arm holding the TRUSTED arm's value, and `clone https://github/evil/repo`
// dispatched to `gh repo clone evil/repo` against public github.com. Returning
// the hostname collapses the two kinds into one, so there is no token left to
// forge. Do not reintroduce a short token for any host.
//
// The GitHub arm is an EXACT, case-insensitive match against the deployment's
// configured GitHub host — a ported or trailing-dot remote for the same host
// is still that host. It was once a substring test, which stamped any hostname
// merely containing "github.com" (e.g. "evil-github.com.attacker.net") as
// trusted inventory; an exact compare closes that, and it is preserved here.
// The arm exists only to fold those spellings together: an unmatched host
// returns its own normalized name and is simply not this deployment's GitHub.
func canonicalHost(hostname, gitHubHost string) string {
	if hostname == "" {
		return ""
	}
	// A zero-value Client (struct literal, no New) carries no host; that must
	// mean the default, not "nothing matches github".
	if gitHubHost == "" {
		gitHubHost = githubauth.DefaultHost
	}
	bare := strings.ToLower(hostname)
	if i := strings.LastIndex(bare, ":"); i >= 0 {
		port := bare[i+1:]
		// The colon must be the ONLY one, or this is an IPv6 address and its
		// last group is not a port. url.Hostname() has already stripped both
		// the brackets and any real port by the time an IPv6 remote reaches
		// here, so a second strip eats address bits: "::1" and "::2" both
		// became ":" — two different hosts, one Key(), so one silently
		// suppressed the other in the inventory. Neither can reach the
		// filesystem (ValidHostSegment rejects a colon), but Key() is not
		// guarded by that.
		if port != "" && !strings.Contains(bare[:i], ":") && strings.Trim(port, "0123456789") == "" {
			bare = bare[:i]
		}
	}
	// One trailing root label: "github.com." is the same host as "github.com",
	// but would otherwise fork the dedup key AND fail the path-segment guard,
	// so a legitimate FQDN remote would stop cloning entirely.
	bare = strings.TrimSuffix(bare, ".")
	if bare == gitHubHost {
		return gitHubHost
	}
	return bare
}
