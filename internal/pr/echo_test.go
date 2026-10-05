package pr

// Test plan for the #562 input-echo convention in internal/pr
//
//   [x] argv (ParseRef, parseNumber): a 10 KB hostile value is echoed capped,
//       quoted, and control-free
//   [x] config / subprocess / disk values (RefFromParts, SearchPRs owner,
//       remoteSessionKey, the PR head repo, the origin URL and its
//       subprocess errors, breadcrumb refs): never echoed at all
//   [x] a credential-bearing origin URL never reaches the error text

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostileEcho is a 10 KB value built to be noticed if echoed: a marker, raw
// terminal controls, and a bidi override.
var hostileEcho = "MARKER" + strings.Repeat("\x1b[2J\u202e", 1400)

// echoBudget bounds an error that quotes one capped argument plus fixed prose.
const echoBudget = 2048

// assertCappedEcho: the error echoes the value's start, bounded and escaped.
func assertCappedEcho(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if len(msg) > echoBudget {
		t.Fatalf("error is %d bytes, want <= %d (capped echo)", len(msg), echoBudget)
	}
	if !strings.Contains(msg, "MARKER") {
		t.Fatalf("error %q does not echo the typed value back", msg)
	}
	for _, r := range msg {
		if termsafe.IsUnsafeTerminalRune(r) {
			t.Fatalf("error %q carries raw rune %U", msg, r)
		}
	}
}

// assertNoEcho: the error names the problem without the value.
func assertNoEcho(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, s := range append([]string{"MARKER", "\x1b", `\x1b`}, secrets...) {
		if strings.Contains(msg, s) {
			t.Fatalf("error %q echoes a rejected value (%q)", msg, s)
		}
	}
}

func TestEcho_ArgvIsCappedAndEscaped(t *testing.T) {
	_, err := ParseRef(hostileEcho)
	assertCappedEcho(t, err)

	// A digit run past int range: strconv's own error repeats the whole
	// input, so it must not ride along.
	_, err = ParseRef("o/r#" + strings.Repeat("9", 10000))
	if err == nil || len(err.Error()) > echoBudget {
		t.Fatalf("out-of-range number: err = %v, want a bounded error", err)
	}
}

func TestEcho_NonArgvValuesAreCategorical(t *testing.T) {
	_, err := RefFromParts(hostileEcho, "repo", "1")
	assertNoEcho(t, err)

	_, _, err = SearchPRs(context.Background(), &exec.FakeRunner{}, SearchOpts{Owner: hostileEcho})
	assertNoEcho(t, err)

	_, err = remoteSessionKey(hostileEcho, "repo", 1)
	assertNoEcho(t, err)

	bc := validRecord()
	bc.Ref = hostileEcho
	err = validateBreadcrumbRecord(bc)
	assertNoEcho(t, err)
	if !strings.Contains(err.Error(), "ref") {
		t.Fatalf("breadcrumb err = %v, want the ref refusal (did the test reach it?)", err)
	}

	bc = validRecord()
	bc.Ref = hostileEcho
	_, err = refFromRecord(bc)
	assertNoEcho(t, err)
}

func TestEcho_PRHeadRepoFromGhIsNotEchoed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"MARKER\u001b[2J"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil
	}}
	client := New(fake, WithSessionsDir(t.TempDir()))
	_, err := client.Prepare(context.Background(), Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}, PrepareOpts{})
	assertNoEcho(t, err)
	if !strings.Contains(err.Error(), "head repo") {
		t.Fatalf("err = %v, want the head-repo refusal (did the test reach it?)", err)
	}
}

// TestEcho_OriginURLCredentialNeverEchoed is the #562 leak: a bare PR number
// resolves against the git origin, whose URL can carry a token. An origin the
// GitHub-only parser does not recognise used to be echoed whole.
func TestEcho_OriginURLCredentialNeverEchoed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		gitErr error
	}{
		// Origins the parser refuses, each carrying a credential.
		{"token in ported https origin", "https://x:SECRET@ghe.example.test:8443/o/r.git", nil},
		{"token in http origin", "http://oauth2:SECRET@ghe.example.test/o/r.git", nil},
		{"token in malformed origin", "https://x:SECRET@ghe.example.test/o", nil},
		{"git failure carrying stderr", "", &exec.CommandError{Name: "git", Stderr: "fatal: SECRET MARKER\x1b[2J", Err: errors.New("exit status 2")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if name == "gh" {
					return "", &exec.CommandError{Name: "gh", Stderr: "gh: MARKER SECRET", Err: errors.New("exit status 1")}
				}
				if name == "git" {
					return tc.origin, tc.gitErr
				}
				return "", nil
			}}
			_, err := New(fake).ResolveRef(context.Background(), "42")
			assertNoEcho(t, err, "SECRET")
		})
	}
}

// TestEcho_OriginCredentialNeverReachesTheRef: an https origin with userinfo
// resolves, and the credential is dropped — only the host survives into the
// Ref, and from there into --repo and the record.
func TestEcho_OriginCredentialNeverReachesTheRef(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "git" {
			return "https://x:SECRET@GHE.example.test/o/r.git", nil
		}
		return "", errors.New("gh unavailable")
	}}
	ref, err := New(fake).ResolveRef(context.Background(), "42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if ref.Host != "ghe.example.test" || ref.Owner != "o" || ref.Repo != "r" {
		t.Fatalf("ref = %+v, want ghe.example.test o/r", ref)
	}
	if strings.Contains(fmt.Sprintf("%+v", ref), "SECRET") {
		t.Fatalf("ref %+v carries the origin credential", ref)
	}
}

// TestEcho_BreadcrumbLifecycleFieldsAreCategorical: the workspace, phase, and
// windowId refusals name the field, never the value read from disk (#658).
func TestEcho_BreadcrumbLifecycleFieldsAreCategorical(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Breadcrumb)
		field string
	}{
		{"workspace", func(bc *Breadcrumb) { bc.Workspace = "relative/" + hostileEcho }, "workspace"},
		{"phase", func(bc *Breadcrumb) {
			bc.Version, bc.Revision, bc.Phase = breadcrumbVersion, 1, Phase(hostileEcho)
		}, "phase"},
		{"windowId", func(bc *Breadcrumb) {
			bc.Version, bc.Revision, bc.Phase, bc.WindowID = breadcrumbVersion, 1, PhaseActive, hostileEcho
		}, "windowId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc := validRecord()
			tc.edit(&bc)
			err := validateBreadcrumbRecord(bc)
			assertNoEcho(t, err)
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want the %s refusal (did the test reach it?)", err, tc.field)
			}
		})
	}
}

// TestEcho_GhPRViewStderrIsNotEchoed: gh's stderr is text the host chooses.
func TestEcho_GhPRViewStderrIsNotEchoed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return "", &exec.CommandError{Name: name, Args: args, Stderr: hostileEcho, ExitCode: 1, Err: errors.New("exit status 1")}
		}
		return "", nil
	}}
	client := New(fake, WithSessionsDir(t.TempDir()))
	_, err := client.Prepare(context.Background(), Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}, PrepareOpts{})
	assertNoEcho(t, err)
	var cmdErr *exec.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("err = %v lost the CommandError from its chain", err)
	}
}

// TestEcho_PostReviewGhStderrIsNotEchoed: an approved post that gh refuses
// reports categorically; gh's stderr is text the host chooses (#658).
func TestEcho_PostReviewGhStderrIsNotEchoed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "review" {
			return "", &exec.CommandError{Name: name, Args: args, Stderr: hostileEcho, ExitCode: 1, Err: errors.New("exit status 1")}
		}
		return "", nil
	}}
	c := postClient(fake, true, true)
	posted, err := c.PostReview(context.Background(), testSess, "the review", false)
	if posted {
		t.Fatal("a refused post must not report posted")
	}
	assertNoEcho(t, err)
	if !strings.Contains(err.Error(), "gh pr review failed") {
		t.Fatalf("err = %v, want the categorical post failure (did the test reach it?)", err)
	}
}
