package pr

// Test plan for search.go
//
// SearchPRs (Classification: shared shell-out builder / hostile-input gate)
//   [x] Happy: who-flag query builds `gh search prs <flag> @me --state open
//       --json … --limit 200` (the limit is ALWAYS explicit — the silent
//       30-row gh default was the pr prs/dash truncation bug)
//   [x] Happy: owner query builds `--owner <o>` with the caller's limit
//   [x] Boundary: rows == limit → truncated=true; rows < limit → false
//   [x] Unhappy: WhoFlag+Owner together, neither, an unlisted who-flag, and an
//       option-like owner are all refused before any shell-out

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestSearchPRs_WhoFlagArgv(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "[]", nil
	}}
	if _, _, err := SearchPRs(context.Background(), fake, SearchOpts{WhoFlag: "--author"}); err != nil {
		t.Fatalf("SearchPRs: %v", err)
	}
	got := strings.Join(fake.Last().Args, " ")
	for _, want := range []string{"search prs", "--author @me", "--state open", "--limit 200"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q: %s", want, got)
		}
	}
}

func TestSearchPRs_OwnerArgv(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "[]", nil
	}}
	if _, _, err := SearchPRs(context.Background(), fake, SearchOpts{Owner: "cameronsjo", Limit: 500}); err != nil {
		t.Fatalf("SearchPRs: %v", err)
	}
	got := strings.Join(fake.Last().Args, " ")
	for _, want := range []string{"--owner cameronsjo", "--limit 500"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "@me") {
		t.Errorf("owner query must not carry @me scoping: %s", got)
	}
}

func TestSearchPRs_TruncationFlag(t *testing.T) {
	two := "[" + searchRow("cameronsjo/forgectl", 1) + "," + searchRow("cameronsjo/forgectl", 2) + "]"
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return two, nil
	}}

	_, truncated, err := SearchPRs(context.Background(), fake, SearchOpts{Owner: "cameronsjo", Limit: 2})
	if err != nil {
		t.Fatalf("SearchPRs: %v", err)
	}
	if !truncated {
		t.Error("rows == limit must report truncated=true")
	}

	_, truncated, err = SearchPRs(context.Background(), fake, SearchOpts{Owner: "cameronsjo", Limit: 3})
	if err != nil {
		t.Fatalf("SearchPRs: %v", err)
	}
	if truncated {
		t.Error("rows < limit must report truncated=false")
	}
}

func TestSearchPRs_RefusesBadOpts(t *testing.T) {
	fake := &exec.FakeRunner{}
	cases := []struct {
		name string
		opts SearchOpts
	}{
		{"both scopes", SearchOpts{WhoFlag: "--author", Owner: "cameronsjo"}},
		{"neither scope", SearchOpts{}},
		{"unlisted who-flag", SearchOpts{WhoFlag: "--label"}},
		{"option-like owner", SearchOpts{Owner: "-cameronsjo"}},
		{"out-of-charset owner", SearchOpts{Owner: "bad owner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := SearchPRs(context.Background(), fake, tc.opts); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
	if len(fake.Calls) != 0 {
		t.Errorf("refused opts must never reach the Runner; saw %d calls", len(fake.Calls))
	}
}

func TestParseSearchPRs_Labels(t *testing.T) {
	row := `{"number":9,"title":"t9","url":"https://github.com/cameronsjo/forgectl/pull/9",` +
		`"author":{"login":"cameronsjo"},"updatedAt":"2026-07-09T12:00:00Z",` +
		`"isDraft":true,"state":"OPEN",` +
		`"labels":[{"name":"auto:execute"},{"name":"enhancement"}],` +
		`"repository":{"nameWithOwner":"cameronsjo/forgectl"}}`
	prs, err := parseSearchPRs("[" + row + "]")
	if err != nil {
		t.Fatalf("parseSearchPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	if got := strings.Join(prs[0].Labels, ","); got != "auto:execute,enhancement" {
		t.Errorf("Labels = %q, want auto:execute,enhancement", got)
	}
}
