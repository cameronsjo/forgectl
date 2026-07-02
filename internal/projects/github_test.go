package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestGithubList_ParsesJSON(t *testing.T) {
	const out = `[{"isPrivate":true,"name":"feedly-clip","sshUrl":"git@github.com:cameronsjo/feedly-clip.git"},` +
		`{"isPrivate":false,"name":"cadence-hooks","sshUrl":"git@github.com:cameronsjo/cadence-hooks.git"}]`
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return out, nil },
	}

	repos, err := githubList(context.Background(), fake)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	if repos[0].Host != "github" || repos[0].Owner != githubOwner || repos[0].Name != "feedly-clip" {
		t.Errorf("repo[0] = %+v; want github/%s/feedly-clip", repos[0], githubOwner)
	}
	if !repos[0].Private {
		t.Errorf("repo[0] should be private")
	}
	if repos[1].Private {
		t.Errorf("repo[1] should be public")
	}
}

func TestGithubList_CommandErrorPropagates(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", errors.New("gh: not authenticated")
		},
	}
	if _, err := githubList(context.Background(), fake); err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
}

func TestGithubList_BadJSONErrors(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "not json", nil },
	}
	if _, err := githubList(context.Background(), fake); err == nil {
		t.Fatal("expected JSON parse error, got nil")
	}
}
