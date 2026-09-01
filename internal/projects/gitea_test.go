package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestGiteaList_ParsesTSVAndFiltersNoise(t *testing.T) {
	// Mirrors real `tea repo ls --output tsv` output: a header row, source +
	// mirror repos. The stderr NOTE line never reaches stdout (OSRunner captures
	// stdout alone), but we include a stray non-4-field line to prove the filter.
	const out = "owner\tname\ttype\tssh\n" +
		"cameron\tRedditDownloader\tsource\tssh://git@git.sjo.lol:222/cameron/RedditDownloader.git\n" +
		"cameron\tupstream-mirror\tmirror\tssh://git@git.sjo.lol:222/cameron/upstream-mirror.git\n" +
		"a stray line with no tabs\n" +
		"\n"
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return out, nil
		},
	}

	repos, err := giteaList(context.Background(), fake, "github.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2 (header + stray + blank filtered out): %+v", len(repos), repos)
	}

	if repos[0].Host != "git.sjo.lol" || repos[0].Owner != "cameron" || repos[0].Name != "RedditDownloader" {
		t.Errorf("repo[0] = %+v; want gitea/cameron/RedditDownloader", repos[0])
	}
	if repos[0].SSHURL != "ssh://git@git.sjo.lol:222/cameron/RedditDownloader.git" {
		t.Errorf("repo[0].SSHURL = %q; want the port-222 form", repos[0].SSHURL)
	}
	if repos[0].Mirror {
		t.Errorf("repo[0] (source) marked as mirror")
	}
	if !repos[1].Mirror {
		t.Errorf("repo[1] (type=mirror) not flagged as mirror: %+v", repos[1])
	}

	// Command construction.
	last := fake.Last()
	if last.Name != "tea" {
		t.Errorf("expected tea invocation, got %q", last.Name)
	}
}

func TestGiteaList_SkipsMalformedRowsAndTrimsCRLF(t *testing.T) {
	// CRLF line endings (header + a good row) plus two malformed 4-field rows
	// with an empty owner and/or name that must not become bogus repos.
	const out = "owner\tname\ttype\tssh\r\n" +
		"cameron\tgood\tsource\tssh://git@git.sjo.lol:222/cameron/good.git\r\n" +
		"\t\tsource\tssh://x\n" + // empty owner AND name
		"cameron\t\tsource\tssh://y\n" // empty name
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return out, nil },
	}
	repos, err := giteaList(context.Background(), fake, "github.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 valid repo (malformed rows skipped), got %d: %+v", len(repos), repos)
	}
	if repos[0].Name != "good" {
		t.Errorf("got %+v, want the 'good' repo", repos[0])
	}
	if strings.Contains(repos[0].SSHURL, "\r") || !strings.HasSuffix(repos[0].SSHURL, "good.git") {
		t.Errorf("CRLF not trimmed from SSHURL: %q", repos[0].SSHURL)
	}
}

func TestGiteaList_CommandErrorPropagates(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", errors.New("dial tcp: no route to host")
		},
	}
	repos, err := giteaList(context.Background(), fake, "github.com")
	if err == nil {
		t.Fatal("expected error to propagate so Inventory can note the host, got nil")
	}
	if repos != nil {
		t.Errorf("expected nil repos on error, got %+v", repos)
	}
}

func TestCloneFromGitea_EmptyURL(t *testing.T) {
	fake := &exec.FakeRunner{}
	if err := cloneFromGitea(context.Background(), fake, "", "/tmp/dest"); err == nil {
		t.Fatal("expected error for empty SSH URL, got nil")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("git clone should not run for an empty URL; calls: %+v", fake.Calls)
	}
}

func TestCloneFromGitea_RunsHardenedGitClone(t *testing.T) {
	fake := &exec.FakeRunner{}
	url := "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"
	if err := cloneFromGitea(context.Background(), fake, url, "/tmp/dest"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	last := fake.Last()
	if last.Name != "git" {
		t.Fatalf("expected git invocation, got %q", last.Name)
	}
	joined := strings.Join(last.Args, " ")
	// The ext::/fd:: transports must be disabled and options terminated with --.
	for _, want := range []string{"protocol.ext.allow=never", "protocol.fd.allow=never", "clone", "--", url, "/tmp/dest"} {
		if !strings.Contains(joined, want) {
			t.Errorf("git args missing %q; got %v", want, last.Args)
		}
	}
	if dd, u := indexOfArg(last.Args, "--"), indexOfArg(last.Args, url); dd < 0 || u < 0 || dd > u {
		t.Errorf("expected -- to precede the URL; got %v", last.Args)
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestGiteaList_DropsUnsafeAndHostileRows covers the three per-row fail-closed
// rules. Every row here is one a hostile or misconfigured Gitea could return,
// and each would otherwise become a directory or a dispatch decision.
func TestGiteaList_DropsUnsafeAndHostileRows(t *testing.T) {
	const good = "cameron\tgoodrepo\tsource\tssh://git@git.sjo.lol:222/cameron/goodrepo.git"
	for _, tc := range []struct{ name, row, why string }{
		{"repo named .git", "cameron\t.git\tsource\tssh://git@git.sjo.lol:222/cameron/x.git",
			"a .git directory makes isGitRepo report the WING as a repo, hiding every member"},
		{"repo named .bare", "cameron\t.bare\tsource\tssh://git@git.sjo.lol:222/cameron/x.git",
			"collides with the worktree layout's object store"},
		{"owner with a leading dot", ".hidden\trepo\tsource\tssh://git@git.sjo.lol:222/x/y.git",
			"a tree invisible to ls"},
		{"name with a colon", "cameron\tre:po\tsource\tssh://git@git.sjo.lol:222/x/y.git",
			"APFS-legal, rendered as '/' in Finder"},
		{"name with an ANSI escape", "cameron\tre\x1b[31mpo\tsource\tssh://git@git.sjo.lol:222/x/y.git",
			"terminal injection via a directory name"},
		{"name with a leading dash", "cameron\t-flag\tsource\tssh://git@git.sjo.lol:222/x/y.git",
			"flag injection into a git argv"},
		{"no derivable host", "cameron\trepo\tsource\tnot-a-url",
			"a hostless Repo keys as local: with an empty path and collides with every other one"},
		{"empty ssh column", "cameron\trepo\tsource\t",
			"same as above"},
		{"claims the configured GitHub host", "cameron\trepo\tsource\tssh://git@github.com/evil/repo.git",
			"would take the gh dispatch branch and be fetched from GitHub by the server's own strings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := "owner\tname\ttype\tssh\n" + good + "\n" + tc.row + "\n"
			fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
			repos, err := giteaList(context.Background(), fake, "github.com")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(repos) != 1 || repos[0].Name != "goodrepo" {
				t.Fatalf("got %d rows (%+v); the hostile row must be dropped and the good one kept — %s", len(repos), repos, tc.why)
			}
		})
	}
}

// TestGiteaList_LongNameIsDropped is split out because the row has to be built
// rather than written literally.
func TestGiteaList_LongNameIsDropped(t *testing.T) {
	long := strings.Repeat("a", 101) // GitHub's own limit is 100
	out := "owner\tname\ttype\tssh\ncameron\t" + long + "\tsource\tssh://git@git.sjo.lol:222/cameron/x.git\n"
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	repos, err := giteaList(context.Background(), fake, "github.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("a %d-byte repo name was kept: %+v", len(long), repos)
	}
}
