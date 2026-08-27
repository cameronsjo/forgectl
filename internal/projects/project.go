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
	Host      string    `json:"host"`             // "github" | "gitea" | "" (local-only, no parseable origin)
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

// hostBadge returns a short host marker for inventory display.
func (r Repo) hostBadge() string {
	switch r.Host {
	case "github":
		return "gh"
	case "gitea":
		return "git.sjo.lol"
	case "":
		return "local"
	default:
		return r.Host
	}
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
	return fmt.Sprintf("%-12s %s %s", r.hostBadge(), name, badge)
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

// parseRemoteURL extracts (host, owner, name) from a git remote URL. It maps the
// known hosts to short tokens ("github", "gitea") so a local clone's origin
// dedups against the remote-list rows; an unrecognised host returns its bare
// hostname. Returns ("","","") when the URL can't be parsed into owner/name.
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
// ResolveHost output): a URL naming that host maps to the "github" token, and
// the bare "owner/repo" shorthand means "on the configured GitHub host" —
// deployment-scoped, not github.com-scoped — because the clone it selects
// runs through gh pinned to that host.
func ParseCloneTarget(arg, gitHubHost string) (Repo, bool) {
	if host, owner, name := parseRemoteURL(arg, gitHubHost); name != "" {
		r := Repo{Host: host, Owner: owner, Name: name}
		if host != "github" {
			r.SSHURL = arg
		}
		return r, true
	}
	if owner, name, ok := splitOwnerRepo(arg); ok {
		return Repo{Host: "github", Owner: owner, Name: name}, true
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

// canonicalHost maps a remote hostname to the inventory's short host token.
//
// The GitHub arm is an EXACT, case-insensitive match against the deployment's
// configured GitHub host (with one trailing ":port" stripped from the remote
// hostname first — a ported remote for the same host is still that host). It
// was once a substring test, which stamped any hostname merely containing
// "github.com" — e.g. "evil-github.com.attacker.net" — as trusted "github"
// inventory; an exact compare closes that. Under a non-default configured
// host, a leftover github.com clone maps to its raw hostname and shows as an
// unmatched local dir — deliberate: it is not this deployment's GitHub.
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
		if port := bare[i+1:]; port != "" && strings.Trim(port, "0123456789") == "" {
			bare = bare[:i]
		}
	}
	switch {
	case bare == gitHubHost:
		return "github"
	case bare == "git.sjo.lol":
		return "gitea"
	default:
		return hostname
	}
}
