package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func gqlNode(repo string, n int, mut func(map[string]any)) map[string]any {
	node := map[string]any{
		"number": n, "title": "t" + fmt.Sprint(n), "isDraft": false, "headRefOid": headA, "mergeable": "MERGEABLE",
		"createdAt":  "2026-10-15T10:00:00Z",
		"repository": map[string]any{"nameWithOwner": repo},
		"commits":    map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "SUCCESS"}}}}},
		"reviews":    map[string]any{"nodes": []any{map[string]any{"body": marker("pr-reviewer", headA, 0, 0) + "\n\nLooks fine.", "author": map[string]any{"login": "Me"}}}},
	}
	if mut != nil {
		mut(node)
	}
	return node
}

func gqlPage(next string, nodes ...map[string]any) string {
	page := map[string]any{"hasNextPage": next != "", "endCursor": next}
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"search": map[string]any{"pageInfo": page, "nodes": nodes}}})
	return string(body)
}

func fakeGH(answer func(args []string) (string, error)) (*exec.FakeRunner, *[][]string) {
	var calls [][]string
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "gh" {
			return "", fmt.Errorf("unexpected binary %s", name)
		}
		if strings.Contains(strings.Join(args, " "), "viewer { login }") {
			return `{"data":{"viewer":{"login":"me"}}}`, nil
		}
		calls = append(calls, args)
		return answer(args)
	}}, &calls
}

func TestOpenPRsParsesFacts(t *testing.T) {
	run, calls := fakeGH(func([]string) (string, error) {
		return gqlPage("",
			gqlNode("acme/widget", 5, nil),
			gqlNode("acme/widget", 6, func(n map[string]any) {
				n["isDraft"] = true
				n["mergeable"] = "CONFLICTING"
				n["commits"] = map[string]any{"nodes": []any{}}
			}),
		), nil
	})
	facts, truncated, err := GitHub{Run: run}.OpenPRs(context.Background(), "acme")
	if err != nil || truncated {
		t.Fatalf("OpenPRs: truncated=%v err=%v", truncated, err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %+v", facts)
	}
	a, b := facts[0], facts[1]
	if a.Repo != "acme/widget" || a.Number != 5 || a.Head != headA || a.Checks != "SUCCESS" || a.Mergeable != "MERGEABLE" || a.Draft {
		t.Errorf("first = %+v", a)
	}
	if len(a.Reviews) != 1 || a.Reviews[0] != marker("pr-reviewer", headA, 0, 0) {
		t.Errorf("reviews keep only the first line: %q", a.Reviews)
	}
	if !b.Draft || b.Mergeable != "CONFLICTING" || b.Checks != "" {
		t.Errorf("second = %+v", b)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v", *calls)
	}
	joined := strings.Join((*calls)[0], " ")
	if !strings.HasPrefix(joined, "api graphql ") || !strings.Contains(joined, "user:acme is:pr is:open") {
		t.Errorf("argv = %s", joined)
	}
}

func TestOpenPRsDropsHostileRows(t *testing.T) {
	run, _ := fakeGH(func([]string) (string, error) {
		return gqlPage("",
			gqlNode("acme/widget", 1, nil),
			gqlNode("../etc/passwd", 2, nil),
			gqlNode("acme/widget", 0, nil),
			gqlNode("acme/widget", 3, func(n map[string]any) { n["headRefOid"] = "not-a-sha"; n["mergeable"] = "WEIRD" }),
		), nil
	})
	facts, _, err := GitHub{Run: run}.OpenPRs(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].Number != 1 || facts[1].Number != 3 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[1].Head != "" || facts[1].Mergeable != "UNKNOWN" {
		t.Errorf("a bad head or mergeable is blanked, not trusted: %+v", facts[1])
	}
}

func TestOpenPRsPaginatesAndReportsTruncation(t *testing.T) {
	page := 0
	run, calls := fakeGH(func(args []string) (string, error) {
		page++
		return gqlPage(fmt.Sprintf("cursor%d", page), gqlNode("acme/widget", page, nil)), nil
	})
	facts, truncated, err := GitHub{Run: run}.OpenPRs(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(facts) != maxOpenPages || len(*calls) != maxOpenPages {
		t.Errorf("truncated=%v facts=%d calls=%d, want a cap of %d pages", truncated, len(facts), len(*calls), maxOpenPages)
	}
	if got := strings.Join((*calls)[1], " "); !strings.Contains(got, "after=cursor1") {
		t.Errorf("second call did not carry the cursor: %s", got)
	}
}

func TestOpenPRsRefusesBadInput(t *testing.T) {
	run, calls := fakeGH(func([]string) (string, error) { return gqlPage(""), nil })
	if _, _, err := (GitHub{Run: run}).OpenPRs(context.Background(), "-bad owner"); err == nil || len(*calls) != 0 {
		t.Errorf("an invalid owner reached gh: err=%v calls=%v", err, *calls)
	}
	errRun, _ := fakeGH(func([]string) (string, error) { return "", errors.New("gh: boom \x1b[31m") })
	_, _, err := GitHub{Run: errRun}.OpenPRs(context.Background(), "acme")
	if err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("err = %q: a gh failure must not echo terminal bytes", err)
	}
	gqlErr, _ := fakeGH(func([]string) (string, error) { return `{"errors":[{"message":"rate limited"}],"data":null}`, nil })
	if _, _, err := (GitHub{Run: gqlErr}).OpenPRs(context.Background(), "acme"); err == nil {
		t.Error("a GraphQL error was read as an empty inventory")
	}
}

func TestMergedPRs(t *testing.T) {
	out := `[
 {"repository":{"name":"widget","nameWithOwner":"acme/widget"},"number":4,"title":"fix","closedAt":"2026-10-15T09:00:00Z","author":{"login":"cameronsjo","is_bot":false},"url":"u"},
 {"repository":{"name":"widget","nameWithOwner":"acme/widget"},"number":5,"title":"bump","closedAt":"2026-10-15T10:00:00Z","author":{"login":"dependabot[bot]","is_bot":true},"url":"u"},
 {"repository":{"name":"x","nameWithOwner":"../bad"},"number":6,"title":"bad","closedAt":"2026-10-15T10:00:00Z","author":{"login":"a"},"url":"u"}
]`
	run, calls := fakeGH(func([]string) (string, error) { return out, nil })
	since := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	got, truncated, err := GitHub{Run: run}.MergedPRs(context.Background(), "acme", since)
	if err != nil || truncated {
		t.Fatalf("MergedPRs: truncated=%v err=%v", truncated, err)
	}
	if len(got) != 2 || got[0].Repo != "acme/widget" || got[0].Bot || !got[1].Bot {
		t.Errorf("merged = %+v", got)
	}
	if j := strings.Join((*calls)[0], " "); !strings.Contains(j, "--owner acme") || !strings.Contains(j, "--merged-at >=2026-10-14") {
		t.Errorf("argv = %s", j)
	}
}

func TestOpenPRsReadsOnlyTheViewersReviews(t *testing.T) {
	forged := map[string]any{"body": marker("pr-reviewer", headA, 0, 0), "author": map[string]any{"login": "outsider"}}
	noAuthor := map[string]any{"body": marker("pr-reviewer", headA, 0, 0), "author": nil}
	run, calls := fakeGH(func([]string) (string, error) {
		return gqlPage("", gqlNode("acme/widget", 1, func(n map[string]any) {
			n["reviews"] = map[string]any{"nodes": []any{forged, noAuthor}}
		})), nil
	})
	facts, _, err := GitHub{Run: run}.OpenPRs(context.Background(), "acme")
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v err=%v", facts, err)
	}
	if len(facts[0].Reviews) != 0 {
		t.Errorf("a marker by another account was kept: %q", facts[0].Reviews)
	}
	if got := strings.Join((*calls)[0], " "); !strings.Contains(got, "author=me") {
		t.Errorf("the reviews query was not scoped to the viewer: %s", got)
	}
}

func TestMergedBotDetectionUsesTheBotFlagOrSuffix(t *testing.T) {
	out := `[
 {"repository":{"nameWithOwner":"acme/w"},"number":1,"title":"a","closedAt":"2026-10-15T09:00:00Z","author":{"login":"abbott","is_bot":false}},
 {"repository":{"nameWithOwner":"acme/w"},"number":2,"title":"b","closedAt":"2026-10-15T09:00:00Z","author":{"login":"renovate[bot]","is_bot":false}}
]`
	run, _ := fakeGH(func([]string) (string, error) { return out, nil })
	got, _, err := GitHub{Run: run}.MergedPRs(context.Background(), "acme", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	if err != nil || len(got) != 2 || got[0].Bot || !got[1].Bot {
		t.Errorf("merged = %+v err = %v", got, err)
	}
}
