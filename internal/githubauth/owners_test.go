package githubauth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// loginFake returns a FakeRunner that answers `gh api user --jq .login` with
// out, and fails anything else — so a stray subprocess is loud, not silent.
func loginFake(out string, err error) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "gh" && strings.Join(args, " ") == "api user --jq .login" {
				return out, err
			}
			return "", errors.New("unexpected command " + name + " " + strings.Join(args, " "))
		},
	}
}

func TestValidOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner string
		want  bool
	}{
		{"plain", "cameronsjo", true},
		{"digits and hyphen", "user-42", true},
		{"dot and underscore", "a.b_c", true},
		{"single dot", ".", true}, // "." is inside the shared charset; ".." is not
		{"empty", "", false},
		{"dot dot", "..", false},
		{"leading hyphen", "-owner", false},
		{"slash", "owner/repo", false},
		{"space", "own er", false},
		{"tab", "own\ter", false},
		{"newline", "owner\n", false},
		{"carriage return", "owner\r", false},
		{"control byte", "own\x01er", false},
		{"unicode", "ownér", false},
		{"flag-like", "--limit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidOwner(tc.owner); got != tc.want {
				t.Fatalf("ValidOwner(%q) = %v, want %v", tc.owner, got, tc.want)
			}
		})
	}
}

func TestResolveOwners_ConfiguredSkipsDiscovery(t *testing.T) {
	fake := loginFake("discovered", nil)

	got, err := ResolveOwners(t.Context(), fake, []string{"Alpha", "alpha", "beta", "ALPHA"})
	if err != nil {
		t.Fatalf("ResolveOwners: %v", err)
	}

	want := []string{"Alpha", "beta"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("owners = %v, want %v (case-insensitive dedup, first spelling and order kept)", got, want)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("discovery calls = %d, want 0 when a list is configured", len(fake.Calls))
	}
}

func TestResolveOwners_ReturnsDefensiveCopy(t *testing.T) {
	configured := []string{"alpha", "beta"}

	got, err := ResolveOwners(t.Context(), loginFake("", nil), configured)
	if err != nil {
		t.Fatalf("ResolveOwners: %v", err)
	}

	got[0] = "mutated"
	if configured[0] != "alpha" {
		t.Fatalf("caller slice mutated to %q; ResolveOwners must return a copy", configured[0])
	}
}

func TestResolveOwners_RejectsBadConfiguredValuesBeforeAnyQuery(t *testing.T) {
	longOwner := strings.Repeat("a", MaxOwnerBytes+1)
	tooMany := make([]string, MaxOwners+1)
	for i := range tooMany {
		tooMany[i] = "owner" + string(rune('a'+i%26)) + strings.Repeat("z", i)
	}

	for _, tc := range []struct {
		name       string
		configured []string
	}{
		{"invalid later element", []string{"good", "bad owner"}},
		{"empty element", []string{"good", ""}},
		{"over byte budget", []string{"good", longOwner}},
		{"over owner count", tooMany},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := loginFake("", nil)

			got, err := ResolveOwners(t.Context(), fake, tc.configured)

			if err == nil {
				t.Fatalf("ResolveOwners = %v, want an error", got)
			}
			if got != nil {
				t.Fatalf("owners = %v, want nil on refusal (no partial result)", got)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("subprocess calls = %d, want 0 on a refusal path", len(fake.Calls))
			}
		})
	}
}

func TestResolveOwners_AcceptsExactBounds(t *testing.T) {
	exactBytes := strings.Repeat("a", MaxOwnerBytes)
	exactCount := make([]string, MaxOwners)
	for i := range exactCount {
		exactCount[i] = "owner" + strings.Repeat("z", i)
	}

	got, err := ResolveOwners(t.Context(), loginFake("", nil), []string{exactBytes})
	if err != nil || len(got) != 1 {
		t.Fatalf("exactly MaxOwnerBytes: owners = %v, err = %v; want accepted", got, err)
	}

	got, err = ResolveOwners(t.Context(), loginFake("", nil), exactCount)
	if err != nil || len(got) != MaxOwners {
		t.Fatalf("exactly MaxOwners: len = %d, err = %v; want %d accepted without truncation", len(got), err, MaxOwners)
	}
}

func TestResolveOwners_DiscoversLoginExactlyOnce(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")

	for _, tc := range []struct {
		name       string
		configured []string
	}{
		{"nil list", nil},
		{"explicitly empty list", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := loginFake("octocat", nil)

			got, err := ResolveOwners(t.Context(), fake, tc.configured)
			if err != nil {
				t.Fatalf("ResolveOwners: %v", err)
			}

			if len(got) != 1 || got[0] != "octocat" {
				t.Fatalf("owners = %v, want [octocat]", got)
			}
			if len(fake.Calls) != 1 {
				t.Fatalf("discovery calls = %d, want exactly 1", len(fake.Calls))
			}
			call := fake.Last()
			if call.Name != "gh" || strings.Join(call.Args, " ") != "api user --jq .login" {
				t.Fatalf("discovery argv = %s %v, want gh api user --jq .login", call.Name, call.Args)
			}
			if got := call.Env["GH_HOST"]; got != Host {
				t.Fatalf("discovery GH_HOST = %q, want %q despite the ambient enterprise host", got, Host)
			}
		})
	}
}

// TestResolveOwners_ParsesNormalizedRunnerOutput pins the parser against
// POST-Runner output. OSRunner.Run already strips every trailing LF, so the
// resolver can make no claim about terminal bare-LF cardinality; a surviving
// "\n" here means multiple records, and a lone terminal "\r" is the residue of
// one stripped CRLF.
func TestResolveOwners_ParsesNormalizedRunnerOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{"plain login", "octocat", "octocat", true},
		{"one residual CR", "octocat\r", "octocat", true},
		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
		{"leading space", " octocat", "", false},
		{"trailing space", "octocat ", "", false},
		{"trailing tab", "octocat\t", "", false},
		{"embedded newline", "octo\ncat", "", false},
		{"two records", "octocat\nhubber", "", false},
		{"embedded CR", "octo\rcat", "", false},
		{"double CR", "octocat\r\r", "", false},
		{"hostile control", "octo\x1b[31mcat", "", false},
		{"overlong", strings.Repeat("a", MaxOwnerBytes+1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := loginFake(tc.out, nil)

			got, err := ResolveOwners(t.Context(), fake, nil)

			if !tc.ok {
				if err == nil {
					t.Fatalf("ResolveOwners = %v, want an error for %q", got, tc.out)
				}
				if !errors.Is(err, ErrLoginUnavailable) {
					t.Fatalf("err = %v, want errors.Is ErrLoginUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveOwners: %v", err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("owners = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestResolveOwners_OrdinaryDiscoveryFailureIsSafe(t *testing.T) {
	hostile := errors.New("gh: \x1b[31mtoken ghp_deadbeef rejected\x1b[0m")
	fake := loginFake("", hostile)

	got, err := ResolveOwners(t.Context(), fake, nil)

	if got != nil {
		t.Fatalf("owners = %v, want nil", got)
	}
	if !errors.Is(err, ErrLoginUnavailable) {
		t.Fatalf("err = %v, want errors.Is ErrLoginUnavailable", err)
	}
	if strings.Contains(err.Error(), "ghp_deadbeef") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("err %q leaked raw subprocess output", err)
	}
}

func TestResolveOwners_PreservesContextIdentityAlongsideLoginSentinel(t *testing.T) {
	raw := realExitError(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &hookFake{err: raw, hook: func(context.Context) { cancel() }}

	_, err := ResolveOwners(ctx, fake, nil)

	if !errors.Is(err, ErrLoginUnavailable) {
		t.Fatalf("err = %v, want errors.Is ErrLoginUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is context.Canceled too", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("err %q leaked the raw exit text", err)
	}
}
