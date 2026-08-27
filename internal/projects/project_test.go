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
		{"github scp-like", "git@github.com:cameronsjo/forgectl.git", "github", "cameronsjo", "forgectl"},
		{"github https with .git", "https://github.com/cameronsjo/forgectl.git", "github", "cameronsjo", "forgectl"},
		{"github https no .git", "https://github.com/cameronsjo/forgectl", "github", "cameronsjo", "forgectl"},
		{"gitea ssh with port", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"gitea ssh no port", "ssh://git@git.sjo.lol/cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"gitea scp-like", "git@git.sjo.lol:cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"unknown host falls through to bare hostname", "https://example.com/foo/bar.git", "example.com", "foo", "bar"},
		{"empty", "", "", "", ""},
		{"garbage", "not-a-url", "", "", ""},
		{"git:// scheme with creds — colon only before @, must not panic", "git://user:pass@github.com/owner/repo", "", "", ""},
		{"host only, no owner/name", "ssh://git@git.sjo.lol:222/", "gitea", "", ""},
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
		{"github ssh URL", "git@github.com:cameronsjo/forgectl.git", true, "github", "cameronsjo", "forgectl", ""},
		{"github https URL", "https://github.com/cameronsjo/forgectl", true, "github", "cameronsjo", "forgectl", ""},
		{"bare owner/repo shorthand", "anthropics/claude-code", true, "github", "anthropics", "claude-code", ""},
		{"gitea ssh URL carries raw arg as SSHURL", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", true,
			"gitea", "cameron", "homeclaw", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"},
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
	gh := Repo{Host: "github", Owner: "cameronsjo", Name: "homeclaw"}
	gt := Repo{Host: "gitea", Owner: "cameron", Name: "homeclaw"}
	if gh.Key() == gt.Key() {
		t.Errorf("cross-host repos share a key: %q", gh.Key())
	}

	// Case-insensitive.
	upper := Repo{Host: "GitHub", Owner: "CameronSjo", Name: "Forgectl"}
	lower := Repo{Host: "github", Owner: "cameronsjo", Name: "forgectl"}
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
		{"uncloned gitea", Repo{Host: "gitea", Owner: "cameron", Name: "homeclaw"}, []string{"git.sjo.lol", "homeclaw", "[uncloned]"}},
		{"cloned clean github", Repo{Host: "github", Owner: "cameronsjo", Name: "forgectl", Cloned: true,
			Status: GitStatus{State: StatusOK}}, []string{"gh", "forgectl", "[clean]"}},
		{"cloned not-a-repo", Repo{Host: "", Name: "notes", Cloned: true,
			Status: GitStatus{State: StatusNotRepo}}, []string{"notes", "[not-a-repo]"}},
		{"cloned unknown status", Repo{Host: "github", Owner: "cameronsjo", Name: "flaky", Cloned: true,
			Status: GitStatus{State: StatusUnknown}}, []string{"flaky", "[unknown]"}},
		{"mirror flagged", Repo{Host: "gitea", Owner: "cameron", Name: "upstream", Mirror: true}, []string{"upstream (mirror)"}},
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
