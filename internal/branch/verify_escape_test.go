package branch

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// ghServedRef models what GitHub is asked about when `gh api` gets path: gh
// fills its {owner}/{repo}/{branch} placeholders (here {branch} becomes
// "main"), concatenates the path onto its REST prefix, and sends the request
// Go's net/http builds from that string. The server routes on the path AS
// SENT (req.URL.EscapedPath), so only a literal `/` separates ref segments;
// it then decodes each segment. It returns the segments after
// git/ref/heads/ that the server resolves. Reading the decoded req.URL.Path
// instead would let a whole-name escape pass: `a%2Fb` and `a/b` decode alike
// but are routed differently (#832).
//
// Each segment is decoded form-style (url.QueryUnescape), so a literal `+`
// reads as a space. How GitHub's router treats `+` in a path is unverified;
// this is the pessimistic model, under which an unescaped `+` asks about
// another ref and only %2B names `a+b`.
func ghServedRef(t *testing.T, path string) []string {
	t.Helper()
	path = strings.ReplaceAll(path, "{branch}", "main")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/"+path, nil)
	if err != nil {
		t.Fatalf("gh could not build a request for path %q: %v", path, err)
	}
	const marker = "/git/ref/heads/"
	sent := req.URL.EscapedPath()
	i := strings.Index(sent, marker)
	if i < 0 {
		t.Fatalf("request path %q has no %s", sent, marker)
	}
	segments := strings.Split(sent[i+len(marker):], "/")
	for j, seg := range segments {
		if segments[j], err = url.QueryUnescape(seg); err != nil {
			t.Fatalf("request path %q has a malformed escape: %v", sent, err)
		}
	}
	return segments
}

// TestPrune_RemoteDelete_VerifyEscapesTheBranchName is #828. Every name here
// is a branch that SURVIVED the delete; all but `fix?x=1` and `with space`
// are git-legal, and those two stay as defense in depth, since git refuses
// `?` and a space in a branch name. The fake server answers 200 only for the
// ref it actually resolves from gh's request, so an unescaped `#`, `%2F`, or
// `{branch}` asks about some other ref, 404s, and the survivor reads as
// deleted. The nested and unicode cases guard the other direction: an escape
// that also mangled `/` (a whole-name url.PathEscape, #832) or UTF-8 would
// miss them.
func TestPrune_RemoteDelete_VerifyEscapesTheBranchName(t *testing.T) {
	for _, name := range []string{
		"fix#12",
		"fix?x=1",
		"a%2Fb",
		"with space",
		"a/b",
		"feat/caf\u00e9-\u65e5\u672c",
		"{branch}",
		"a+b",
	} {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(cmd string, args []string) (string, error) {
				switch {
				case isGetURL(cmd, args):
					return githubRemoteURL, nil
				case cmd == "gh" && len(args) > 0 && args[0] == "api":
					if slices.Equal(ghServedRef(t, args[len(args)-1]), strings.Split(name, "/")) {
						return `{"ref":"refs/heads/survivor"}`, nil
					}
					return "", ghNotFound(args)
				}
				return "", nil
			}}
			item := Classification{
				Info:  Info{Name: name, RemoteExists: true, MergedOnServer: true},
				Group: SafeToDelete,
			}
			results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
			if len(results) != 1 || results[0].Deleted || results[0].Err == nil ||
				!strings.Contains(results[0].Err.Error(), "still exists") {
				t.Fatalf("surviving branch %q: got %+v, want the still-exists verdict", name, results)
			}
		})
	}
}

func TestEscapeRefPath(t *testing.T) {
	for in, want := range map[string]string{
		"fix#12":     "fix%2312",
		"fix?x=1":    "fix%3Fx=1",
		"a%2Fb":      "a%252Fb",
		"with space": "with%20space",
		"a/b/c":      "a/b/c",
		"{branch}":   "%7Bbranch%7D",
		"caf\u00e9":  "caf%C3%A9",
		"a+b":        "a%2Bb",
	} {
		if got := escapeRefPath(in); got != want {
			t.Errorf("escapeRefPath(%q) = %q, want %q", in, got, want)
		}
	}
}
