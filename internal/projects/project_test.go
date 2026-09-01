package projects

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/githubauth"
)

func TestParseRemoteURL(t *testing.T) {
	tests := []struct {
		name                      string
		url                       string
		wantHost, wantOwn, wantNm string
	}{
		{"github scp-like", "git@github.com:cameronsjo/forgectl.git", "github.com", "cameronsjo", "forgectl"},
		{"github https with .git", "https://github.com/cameronsjo/forgectl.git", "github.com", "cameronsjo", "forgectl"},
		{"github https no .git", "https://github.com/cameronsjo/forgectl", "github.com", "cameronsjo", "forgectl"},
		{"gitea ssh with port", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", "git.sjo.lol", "cameron", "homeclaw"},
		{"gitea ssh no port", "ssh://git@git.sjo.lol/cameron/homeclaw.git", "git.sjo.lol", "cameron", "homeclaw"},
		{"gitea scp-like", "git@git.sjo.lol:cameron/homeclaw.git", "git.sjo.lol", "cameron", "homeclaw"},
		{"unknown host falls through to bare hostname", "https://example.com/foo/bar.git", "example.com", "foo", "bar"},
		{"empty", "", "", "", ""},
		{"garbage", "not-a-url", "", "", ""},
		{"git:// scheme with creds — colon only before @, must not panic", "git://user:pass@github.com/owner/repo", "", "", ""},
		{"host only, no owner/name", "ssh://git@git.sjo.lol:222/", "git.sjo.lol", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, name := parseRemoteURL(tc.url, githubauth.DefaultHost)
			if host != tc.wantHost || owner != tc.wantOwn || name != tc.wantNm {
				t.Errorf("parseRemoteURL(%q) = (%q,%q,%q); want (%q,%q,%q)",
					tc.url, host, owner, name, tc.wantHost, tc.wantOwn, tc.wantNm)
			}
		})
	}
}

func TestParseCloneTarget(t *testing.T) {
	tests := []struct {
		name                          string
		arg                           string
		wantOK                        bool
		wantHost, wantOwner, wantName string
		wantSSHURL                    string
	}{
		{"github ssh URL", "git@github.com:cameronsjo/forgectl.git", true, "github.com", "cameronsjo", "forgectl", ""},
		{"github https URL", "https://github.com/cameronsjo/forgectl", true, "github.com", "cameronsjo", "forgectl", ""},
		{"bare owner/repo shorthand", "anthropics/claude-code", true, "github.com", "anthropics", "claude-code", ""},
		{"gitea ssh URL carries raw arg as SSHURL", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", true,
			"git.sjo.lol", "cameron", "homeclaw", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"},
		{"unrecognized host https carries raw arg as SSHURL", "https://example.com/foo/bar.git", true,
			"example.com", "foo", "bar", "https://example.com/foo/bar.git"},
		{"plain query, no slash", "forgectl", false, "", "", "", ""},
		{"too many slashes is not owner/repo shorthand", "a/b/c", false, "", "", "", ""},
		{"traversal segment rejected", "../etc", false, "", "", "", ""},
		{"empty", "", false, "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := ParseCloneTarget(tc.arg, githubauth.DefaultHost)
			if ok != tc.wantOK {
				t.Fatalf("ParseCloneTarget(%q) ok = %v; want %v", tc.arg, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if r.Host != tc.wantHost || r.Owner != tc.wantOwner || r.Name != tc.wantName || r.SSHURL != tc.wantSSHURL {
				t.Errorf("ParseCloneTarget(%q) = %+v; want host=%q owner=%q name=%q sshURL=%q",
					tc.arg, r, tc.wantHost, tc.wantOwner, tc.wantName, tc.wantSSHURL)
			}
		})
	}
}

func TestRepoKey(t *testing.T) {
	// Same name on two hosts must yield distinct keys (no bare-name collision).
	gh := Repo{Host: "github.com", Owner: "cameronsjo", Name: "homeclaw"}
	gt := Repo{Host: "git.sjo.lol", Owner: "cameron", Name: "homeclaw"}
	if gh.Key() == gt.Key() {
		t.Errorf("cross-host repos share a key: %q", gh.Key())
	}

	// Case-insensitive.
	upper := Repo{Host: "GitHub.COM", Owner: "CameronSjo", Name: "Forgectl"}
	lower := Repo{Host: "github.com", Owner: "cameronsjo", Name: "forgectl"}
	if upper.Key() != lower.Key() {
		t.Errorf("keys differ by case: %q vs %q", upper.Key(), lower.Key())
	}

	// Local-only repo (no parseable origin) keys by path.
	local := Repo{Name: "scratch", LocalPath: "/Users/x/Projects/scratch", Cloned: true}
	if got, want := local.Key(), "local:/Users/x/Projects/scratch"; got != want {
		t.Errorf("local Key() = %q; want %q", got, want)
	}
}

func TestRepoDisplayLine(t *testing.T) {
	tests := []struct {
		name string
		repo Repo
		want []string // substrings that must appear
	}{
		{"uncloned gitea", Repo{Host: "git.sjo.lol", Owner: "cameron", Name: "homeclaw"}, []string{"git.sjo.lol", "homeclaw", "[uncloned]"}},
		{"cloned clean github", Repo{Host: "github.com", Owner: "cameronsjo", Name: "forgectl", Cloned: true,
			Status: GitStatus{State: StatusOK}}, []string{"github.com", "forgectl", "[clean]"}},
		{"cloned not-a-repo", Repo{Host: "", Name: "notes", Cloned: true,
			Status: GitStatus{State: StatusNotRepo}}, []string{"notes", "[not-a-repo]"}},
		{"cloned unknown status", Repo{Host: "github.com", Owner: "cameronsjo", Name: "flaky", Cloned: true,
			Status: GitStatus{State: StatusUnknown}}, []string{"flaky", "[unknown]"}},
		{"mirror flagged", Repo{Host: "git.sjo.lol", Owner: "cameron", Name: "upstream", Mirror: true}, []string{"upstream (mirror)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.repo.DisplayLine()
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("DisplayLine() = %q; missing %q", got, sub)
				}
			}
		})
	}
}

// TestGitStatus_Label_EmptyIffStateOK asserts the property Label() != "" iff
// State == StatusOK, derived from the state rather than hard-coding badge
// strings — so it survives future label-wording changes and pins the
// StatusNotRepo/StatusUnknown branches added to fix the false-"clean" defect.
func TestGitStatus_Label_EmptyIffStateOK(t *testing.T) {
	states := []StatusState{StatusOK, StatusNotRepo, StatusUnknown}
	for _, s := range states {
		t.Run(string(s), func(t *testing.T) {
			gs := GitStatus{State: s}
			label := gs.Label()
			wantEmpty := s != StatusOK
			gotEmpty := label == ""
			if gotEmpty != wantEmpty {
				t.Errorf("State=%q: Label()=%q (empty=%v), want empty=%v", s, label, gotEmpty, wantEmpty)
			}
		})
	}
}

// TestGitStatus_ZeroValue_DoesNotReadAsClean is the one-line regression pin
// for the exact defect: a GitStatus that never went through gitStatus (the
// Go zero value, State == StatusUnknown) must not be mistaken for a clean
// working tree.
func TestGitStatus_ZeroValue_DoesNotReadAsClean(t *testing.T) {
	if got := (GitStatus{}).Label(); got != "" {
		t.Errorf("zero-value GitStatus.Label() = %q, want \"\" (must not read as clean)", got)
	}
}

func TestStatusState_JSONUsesAnExplicitUnknownValue(t *testing.T) {
	tests := []struct {
		name string
		in   StatusState
		wire string
	}{
		{name: "unknown zero value", in: StatusUnknown, wire: `"unknown"`},
		{name: "not a repository", in: StatusNotRepo, wire: `"not-a-repo"`},
		{name: "status available", in: StatusOK, wire: `"ok"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got := string(data); got != tt.wire {
				t.Errorf("Marshal(%q) = %s, want %s", tt.in, got, tt.wire)
			}

			var decoded StatusState
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if decoded != tt.in {
				t.Errorf("round trip = %q, want %q", decoded, tt.in)
			}
		})
	}
}

func TestGitStatus_ZeroValueJSONDoesNotLookClean(t *testing.T) {
	data, err := json.Marshal(GitStatus{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"state":"unknown","modified":0,"untracked":0,"ahead":0}`
	if got := string(data); got != want {
		t.Errorf("Marshal(zero GitStatus) = %s, want %s", got, want)
	}
}

func TestStatusState_JSONDecodeCompatibility(t *testing.T) {
	var legacy StatusState
	if err := json.Unmarshal([]byte(`""`), &legacy); err != nil {
		t.Fatalf("Unmarshal legacy unknown: %v", err)
	}
	if legacy != StatusUnknown {
		t.Errorf("legacy empty state decoded as %q, want StatusUnknown", legacy)
	}

	preserved := StatusOK
	if err := json.Unmarshal([]byte(`null`), &preserved); err != nil {
		t.Fatalf("Unmarshal null: %v", err)
	}
	if preserved != StatusOK {
		t.Errorf("null changed state to %q, want %q", preserved, StatusOK)
	}
}

// TestCanonicalHost pins the exact-match mapping: the substring test it
// replaced stamped any hostname merely CONTAINING "github.com" as trusted
// "github.com" inventory.
func TestCanonicalHost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hostname   string
		gitHubHost string
		want       string
	}{
		{"empty hostname", "", "github.com", ""},
		{"github.com default", "github.com", "github.com", "github.com"},
		{"zero-value client means default", "github.com", "", "github.com"},
		{"case-insensitive", "GitHub.COM", "github.com", "github.com"},
		{"ported remote same host", "github.com:443", "github.com", "github.com"},
		{"substring attack stays raw", "evil-github.com.attacker.net", "github.com", "evil-github.com.attacker.net"},
		{"prefix attack stays raw", "github.com.attacker.net", "github.com", "github.com.attacker.net"},
		{"ghe host configured", "github.example.com", "github.example.com", "github.example.com"},
		{"github.com under ghe config stays raw", "github.com", "github.example.com", "github.com"},
		{"gitea exact", "git.sjo.lol", "github.com", "git.sjo.lol"},
		{"gitea substring attack stays raw", "git.sjo.lol.evil.net", "github.com", "git.sjo.lol.evil.net"},
		{"trailing colon not a port", "github.com:", "github.com", "github.com:"},
		{"non-numeric port not stripped", "github.com:x1", "github.com", "github.com:x1"},

		// THE FORGEABLE-TOKEN CASES. Before hostnames replaced the short
		// tokens, canonicalHost's default arm returned the raw hostname into
		// the SAME value space as the tokens "github"/"gitea" — so a remote
		// whose bare hostname was literally "github" came out of the untrusted
		// arm holding the trusted arm's value, and
		// `clone https://github/evil/repo` dispatched to
		// `gh repo clone evil/repo` against public github.com. These rows are
		// what stop a future refactor reintroducing a short token with a green
		// suite; they pass trivially now and that is the point.
		{"bare token 'github' is its own host, not the trusted one", "github", "github.com", "github"},
		{"bare token 'github' under a GHE config", "github", "github.example.com", "github"},
		{"bare token 'gitea' is its own host", "gitea", "github.com", "gitea"},
		{"uppercase bare token normalizes but stays untrusted", "GITHUB", "github.com", "github"},
		{"case split cannot fork the identity", "GitHub.Example.COM", "github.example.com", "github.example.com"},

		// A trailing root label is the same host. Left unstripped it forks the
		// dedup key AND fails the path-segment guard, so a legitimate FQDN
		// remote would stop cloning entirely.
		{"trailing dot is the same host", "github.com.", "github.com", "github.com"},
		{"trailing dot on an untrusted host still normalizes", "git.sjo.lol.", "github.com", "git.sjo.lol"},
		{"trailing dot does not launder a spoof", "evil-github.com.attacker.net.", "github.com", "evil-github.com.attacker.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalHost(tc.hostname, tc.gitHubHost); got != tc.want {
				t.Errorf("canonicalHost(%q, %q) = %q, want %q", tc.hostname, tc.gitHubHost, got, tc.want)
			}
		})
	}
}

// TestParseCloneTarget_UnderGHEHost: the bare owner/repo shorthand is
// deployment-scoped — it means the CONFIGURED host, since the clone runs
// through gh pinned there. A github.com URL under a GHE config keeps its own
// hostname (it is not this deployment's GitHub) and carries the literal URL
// forward, so it clones as a plain git URL rather than through gh.
func TestParseCloneTarget_UnderGHEHost(t *testing.T) {
	const ghe = "github.example.com"

	r, ok := ParseCloneTarget("acme/tool", ghe)
	if !ok || r.Host != ghe || r.Owner != "acme" || r.Name != "tool" || r.SSHURL != "" {
		t.Fatalf("shorthand under GHE = %+v ok=%v, want %s/acme/tool with no SSHURL", r, ok, ghe)
	}

	r, ok = ParseCloneTarget("https://github.example.com/acme/tool", ghe)
	if !ok || r.Host != ghe || r.SSHURL != "" {
		t.Fatalf("GHE URL = %+v ok=%v, want Host %s cloning through gh", r, ok, ghe)
	}

	r, ok = ParseCloneTarget("https://github.com/acme/tool", ghe)
	if !ok || r.Host != "github.com" || r.SSHURL != "https://github.com/acme/tool" {
		t.Fatalf("github.com URL under GHE = %+v ok=%v, want its own host carrying the literal URL", r, ok)
	}
}

// TestParseCloneTarget_ZeroHostMeansDefault pins the zero-value fallback on the
// parse side. A caller passing "" must get a Repo whose Host matches what a
// zero-value Client's dispatch will compare against — otherwise a github.com
// target silently misses the gh branch and clones a bare URL with no host pin.
func TestParseCloneTarget_ZeroHostMeansDefault(t *testing.T) {
	r, ok := ParseCloneTarget("acme/tool", "")
	if !ok || r.Host != githubauth.DefaultHost {
		t.Fatalf("shorthand with no configured host = %+v ok=%v, want Host %s", r, ok, githubauth.DefaultHost)
	}
	r, ok = ParseCloneTarget("https://github.com/acme/tool", "")
	if !ok || r.Host != githubauth.DefaultHost || r.SSHURL != "" {
		t.Fatalf("github.com URL with no configured host = %+v ok=%v, want the gh path", r, ok)
	}
}

// TestParseCloneTarget_ForgedTokenIsNotTrusted is the CLI-facing half of the
// forgeable-token case: this exact argument used to clone evil/repo from
// public github.com. It must now parse as its own host and carry the literal
// URL, so the clone is a plain `git clone https://github/evil/repo` that
// simply fails to resolve.
func TestParseCloneTarget_ForgedTokenIsNotTrusted(t *testing.T) {
	for _, arg := range []string{
		"https://github/evil/repo",
		"git@github:evil/repo",
		"https://GITHUB/evil/repo",
		"git@gitea:evil/repo",
	} {
		t.Run(arg, func(t *testing.T) {
			r, ok := ParseCloneTarget(arg, "github.com")
			if !ok {
				t.Fatalf("ParseCloneTarget(%q) did not parse", arg)
			}
			if r.Host == "github.com" {
				t.Fatalf("ParseCloneTarget(%q) stamped the trusted host %q — this dispatches to gh", arg, r.Host)
			}
			if r.SSHURL != arg {
				t.Errorf("SSHURL = %q, want the literal argument %q; an empty SSHURL means it took the gh path", r.SSHURL, arg)
			}
		})
	}
}

// TestParseRemoteURL_GHEHost: a GHE remote parses to its own hostname under
// its own configured host.
func TestParseRemoteURL_GHEHost(t *testing.T) {
	host, owner, name := parseRemoteURL("git@github.example.com:acme/tool.git", "github.example.com")
	if host != "github.example.com" || owner != "acme" || name != "tool" {
		t.Errorf("parseRemoteURL scp GHE = (%q,%q,%q), want (github.example.com,acme,tool)", host, owner, name)
	}
}

// TestParseRemoteURL_IPv6Literal is the one form that can put a ':' into
// Repo.Host — url.Hostname() strips a port, and the scp-like branch puts a
// port in the PATH, so no ordinary ported URL reaches here with a colon.
//
// It asserts the EXACT host, not merely that a colon survived. The weaker
// assertion passed while canonicalHost was mangling these: its port-strip ran
// a second time on an address url.Hostname() had already unbracketed, so
// "::1" and "::2" both came out as ":" — two different hosts sharing one
// Key(), where one silently suppresses the other in the inventory. A test that
// only checks `strings.Contains(host, ":")` cannot tell that from correct
// behavior, which is exactly why it has to compare values.
func TestParseRemoteURL_IPv6Literal(t *testing.T) {
	for _, tc := range []struct{ url, wantHost string }{
		{"ssh://git@[::1]:22/acme/tool.git", "::1"},
		{"ssh://git@[::2]/acme/tool.git", "::2"},
		{"ssh://git@[2001:db8::1]:22/a/b.git", "2001:db8::1"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			host, owner, name := parseRemoteURL(tc.url, "github.com")
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if owner == "" || name == "" {
				t.Errorf("owner/name = %q/%q, want both parsed", owner, name)
			}
			// validPathSegment ACCEPTS a colon (APFS-legal, rendered as '/' in
			// Finder), which is why the host segment needs its own charset.
			if !validPathSegment(host) {
				t.Errorf("validPathSegment(%q) rejected it; this test asserts the guard it MISSES", host)
			}
			if githubauth.ValidHostSegment(host) {
				t.Errorf("ValidHostSegment(%q) accepted a colon-bearing host segment", host)
			}
		})
	}
}

// TestCanonicalHost_DistinctIPv6HostsKeepDistinctKeys is the collision half:
// the mangling above made two different servers share a dedup identity, and
// Key() is not guarded by ValidHostSegment the way a path is.
func TestCanonicalHost_DistinctIPv6HostsKeepDistinctKeys(t *testing.T) {
	a := Repo{Host: canonicalHost("::1", "github.com"), Owner: "o", Name: "n"}
	b := Repo{Host: canonicalHost("::2", "github.com"), Owner: "o", Name: "n"}
	if a.Key() == b.Key() {
		t.Errorf("two different hosts share a key: %q — one would suppress the other in the inventory", a.Key())
	}
}
