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

	repos, err := giteaList(context.Background(), fake)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2 (header + stray + blank filtered out): %+v", len(repos), repos)
	}

	if repos[0].Host != "gitea" || repos[0].Owner != "cameron" || repos[0].Name != "RedditDownloader" {
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
	repos, err := giteaList(context.Background(), fake)
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
	repos, err := giteaList(context.Background(), fake)
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
