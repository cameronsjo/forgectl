package review

// Test plan for gitea.go
//
// Gitea.Items (Classification: concurrent enumeration / hostile-input parser)
//   [x] Happy: one issues(--kind all) query per owner, --owner-scoped with an
//       explicit --limit, --login, --kind all, --output json; rows map to
//       Items with labels
//   [x] Happy: an empty configured login omits --login entirely (falls back
//       to tea's own configured default)
//   [x] Unhappy: a degraded owner becomes a note, the other owner's rows
//       survive
//   [x] Unhappy: every owner query failed → error; zero owners → error
//   [x] Unhappy: malformed owner never reaches the Runner
//   [x] Boundary: rows == limit → truncation note
//
// NewGitea (Classification: constructor guard)
//   [x] Unhappy: a malformed host is rejected at construction
//   [x] Happy: a host:port construction is accepted
//
// parseTeaIssues (Classification: hostile-input parser)
//   [x] Unhappy: non-numeric index, out-of-charset repo, and unknown kind
//       rows are skipped; a foreign-host URL is accepted but its URL is
//       replaced by synthesis; rawCount stays pre-filter
//   [x] Unhappy: `[]` and whitespace-only input → (nil, 0, nil)
//
// ParseWorkRefForHosts (Classification: hostile-input parser)
//   [x] Happy: host-qualified slug and URL forms normalize for a configured
//       host; github.com forms still work unchanged
//   [x] Happy: a configured host:port parses both slug and URL forms
//   [x] Unhappy: an unconfigured host is rejected; a host-qualified ref is
//       rejected entirely when no hosts are configured; an unlisted
//       host:port is rejected even though it is well-formed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// teaRow renders one `tea issues list --output json` row. kind is tea's raw
// value ("Issue" or "Pull"); the URL is synthesized on git.sjo.lol using
// tea's own path convention ("issues" or "pulls").
func teaRow(kind, owner, repo string, index int, labels ...string) string {
	seg := "issues"
	if strings.EqualFold(kind, "pull") {
		seg = "pulls"
	}
	url := fmt.Sprintf("https://git.sjo.lol/%s/%s/%s/%d", owner, repo, seg, index)
	return fmt.Sprintf(`{"index":"%d","kind":%q,"state":"open","author":"cameron","url":%q,"title":"t%d","updated":"2026-07-09T12:00:00Z","labels":%q,"owner":%q,"repo":%q}`,
		index, kind, url, index, strings.Join(labels, ","), owner, repo)
}

// argAfter returns the value following the first occurrence of flag in args,
// or "" if flag is absent or has no following value.
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestGiteaItems_BothKindsMapped(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if argAfter(args, "--owner") == "cameron" {
			return "[" + teaRow("Issue", "cameron", "forgectl", 10, "epic") + "," + teaRow("Pull", "cameron", "forgectl", 11) + "]", nil
		}
		return "[]", nil
	}}

	g, err := NewGitea(fake, "git.sjo.lol", "cameron", []string{"cameron"})
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	items, notes, err := g.Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}

	byKey := map[string]Item{}
	for _, it := range items {
		byKey[it.Key()] = it
	}
	issue := byKey["git.sjo.lol/cameron/forgectl#10"]
	if issue.Kind != KindIssue || strings.Join(issue.Labels, ",") != "epic" {
		t.Errorf("issue mapped wrong: %+v", issue)
	}
	pull := byKey["git.sjo.lol/cameron/forgectl#11"]
	if pull.Kind != KindPR || pull.IsDraft {
		t.Errorf("pr mapped wrong (Gitea has no draft field, must be false): %+v", pull)
	}

	for _, call := range fake.Calls {
		argv := strings.Join(call.Args, " ")
		if !strings.Contains(argv, "--owner cameron") {
			t.Errorf("query not owner-scoped: %s", argv)
		}
		if !strings.Contains(argv, "--limit 1000") {
			t.Errorf("query missing explicit --limit: %s", argv)
		}
		if !strings.Contains(argv, "--login cameron") {
			t.Errorf("query missing explicit --login: %s", argv)
		}
		if !strings.Contains(argv, "--kind all") {
			t.Errorf("query missing --kind all: %s", argv)
		}
		if !strings.Contains(argv, "--output json") {
			t.Errorf("query missing --output json: %s", argv)
		}
	}
}

// TestGiteaItems_EmptyLoginOmitsFlag pins Fix D: an unconfigured (empty)
// login must omit --login from the argv entirely, not pass an empty value —
// tea should fall back to its own configured default login.
func TestGiteaItems_EmptyLoginOmitsFlag(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "[]", nil
	}}

	g, err := NewGitea(fake, "git.sjo.lol", "", []string{"cameron"})
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	if _, _, err := g.Items(context.Background()); err != nil {
		t.Fatalf("Items: %v", err)
	}
	for _, call := range fake.Calls {
		if argAfter(call.Args, "--login") != "" || strings.Contains(strings.Join(call.Args, " "), "--login") {
			t.Errorf("empty login must omit --login entirely: %v", call.Args)
		}
	}
}

func TestGiteaItems_DegradedOwnerBecomesNote(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if argAfter(args, "--owner") == "bad" {
			return "", errors.New("tea: user does not exist")
		}
		return "[" + teaRow("Issue", "good", "repo", 1) + "]", nil
	}}

	g, err := NewGitea(fake, "git.sjo.lol", "cameron", []string{"bad", "good"})
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	items, notes, err := g.Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("healthy owner's rows must survive; got %d", len(items))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "gitea(bad)") {
		t.Errorf("want one gitea(bad) note, got %v", notes)
	}
}

func TestGiteaItems_AllOwnersFailed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New("tea: not authenticated")
	}}
	g, err := NewGitea(fake, "git.sjo.lol", "cameron", []string{"cameron"})
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	if _, _, err := g.Items(context.Background()); err == nil {
		t.Error("every owner query failing must be an error")
	}
}

func TestGiteaItems_NoOwners(t *testing.T) {
	g, err := NewGitea(&exec.FakeRunner{}, "git.sjo.lol", "cameron", nil)
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	if _, _, err := g.Items(context.Background()); err == nil {
		t.Error("zero owners must be an error")
	}
}

// TestGiteaItems_RejectsMalformedOwner pins the documented enforcement point
// for low-trust config input: a malformed owner must be refused BEFORE any
// argv reaches the Runner, and zero tea processes may spawn.
func TestGiteaItems_RejectsMalformedOwner(t *testing.T) {
	for _, owner := range []string{"bad owner", "-cameron"} {
		t.Run(owner, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			g, err := NewGitea(fake, "git.sjo.lol", "cameron", []string{owner})
			if err != nil {
				t.Fatalf("NewGitea: %v", err)
			}
			if _, _, err := g.Items(context.Background()); err == nil {
				t.Error("malformed owner must be an error")
			}
			if len(fake.Calls) != 0 {
				t.Errorf("malformed owner must never reach the Runner; saw %d calls", len(fake.Calls))
			}
		})
	}
}

func TestGiteaItems_TruncationNote(t *testing.T) {
	rows := make([]string, searchLimit)
	for i := range rows {
		rows[i] = teaRow("Issue", "cameron", "forgectl", i+1)
	}
	out := "[" + strings.Join(rows, ",") + "]"
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return out, nil
	}}

	g, err := NewGitea(fake, "git.sjo.lol", "cameron", []string{"cameron"})
	if err != nil {
		t.Fatalf("NewGitea: %v", err)
	}
	_, notes, err := g.Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "truncated") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a truncation note, got %v", notes)
	}
}

func TestNewGitea_RejectsMalformedHost(t *testing.T) {
	for _, host := range []string{"git.sjo.lol/evil", "has space", "", "javascript:alert(1)", "-leading-hyphen"} {
		t.Run(host, func(t *testing.T) {
			if _, err := NewGitea(&exec.FakeRunner{}, host, "cameron", []string{"cameron"}); err == nil {
				t.Errorf("host %q: want error, got nil", host)
			}
		})
	}
}

// TestNewGitea_AcceptsPortedHost pins Fix B's premise: a host:port
// construction succeeds (reGiteaHost already allowed this) — the bug was
// that ParseWorkRefForHosts couldn't parse the resulting keys back, fixed
// separately (TestParseWorkRefForHosts_PortedHost).
func TestNewGitea_AcceptsPortedHost(t *testing.T) {
	g, err := NewGitea(&exec.FakeRunner{}, "git.sjo.lol:3000", "cameron", []string{"cameron"})
	if err != nil {
		t.Fatalf("NewGitea with host:port: %v", err)
	}
	if g.Host() != "git.sjo.lol:3000" {
		t.Errorf("Host() = %q, want git.sjo.lol:3000", g.Host())
	}
}

func TestParseTeaIssues_HostileRows(t *testing.T) {
	const host = "git.sjo.lol"
	badIndex := `{"index":"abc","kind":"Issue","state":"open","author":"cameron","url":"https://git.sjo.lol/cameronsjo/forgectl/issues/1","title":"t1","updated":"2026-07-09T12:00:00Z","labels":"","owner":"cameronsjo","repo":"forgectl"}`
	badRepo := `{"index":"2","kind":"Issue","state":"open","author":"cameron","url":"https://git.sjo.lol/cameronsjo/bad repo/issues/2","title":"t2","updated":"2026-07-09T12:00:00Z","labels":"","owner":"cameronsjo","repo":"bad repo"}`
	badKind := `{"index":"3","kind":"Wat","state":"open","author":"cameron","url":"https://git.sjo.lol/cameronsjo/forgectl/issues/3","title":"t3","updated":"2026-07-09T12:00:00Z","labels":"","owner":"cameronsjo","repo":"forgectl"}`
	foreignURL := strings.Replace(teaRow("Issue", "cameronsjo", "forgectl", 4),
		"https://git.sjo.lol/cameronsjo/forgectl/issues/4", "https://evil.example/cameronsjo/forgectl/issues/4", 1)
	good := teaRow("Pull", "cameronsjo", "forgectl", 5)

	jsonOut := "[" + strings.Join([]string{badIndex, badRepo, badKind, foreignURL, good}, ",") + "]"
	items, rawCount, err := parseTeaIssues(jsonOut, host)
	if err != nil {
		t.Fatalf("parseTeaIssues: %v", err)
	}
	// The truncation sentinel keys off the PRE-filter count: skipped hostile
	// rows must still count.
	if rawCount != 5 {
		t.Errorf("rawCount = %d, want 5 (pre-filter)", rawCount)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 surviving items (foreign-url row + good row), got %+v", items)
	}

	byNum := map[int]Item{}
	for _, it := range items {
		byNum[it.Number] = it
	}
	foreign, ok := byNum[4]
	if !ok {
		t.Fatalf("foreign-host-URL row must survive (only its URL is corrected): %+v", items)
	}
	if want := "https://git.sjo.lol/cameronsjo/forgectl/issues/4"; foreign.URL != want {
		t.Errorf("foreign-host URL must be replaced by synthesis: got %q, want %q", foreign.URL, want)
	}
	goodItem, ok := byNum[5]
	if !ok || goodItem.Kind != KindPR {
		t.Errorf("good row mapped wrong: %+v", goodItem)
	}
}

// TestParseTeaIssues_KeepsOwnerNamedLocal is the review-side regression guard
// for issue #185. A self-hosted forge really does host an org named "local"
// (git.sjo.lol/local/tools), and the old host-blind owner reservation made
// itemFromParts reject every such row — so `review list` silently dropped
// them, with the only trace a warn line in the dated log file.
func TestParseTeaIssues_KeepsOwnerNamedLocal(t *testing.T) {
	jsonOut := "[" + teaRow("Issue", "local", "tools", 5) + "]"
	items, rawCount, err := parseTeaIssues(jsonOut, "git.sjo.lol")
	if err != nil {
		t.Fatalf("parseTeaIssues: %v", err)
	}
	if rawCount != 1 {
		t.Errorf("rawCount = %d, want 1", rawCount)
	}
	if len(items) != 1 {
		t.Fatalf("a row owned by %q must survive; got %+v", "local", items)
	}
	if items[0].Owner != "local" || items[0].Repo != "tools" || items[0].Number != 5 {
		t.Errorf("item = %+v, want local/tools#5", items[0])
	}
}

// TestParseWorkRefForHosts_OwnerNamedLocal is the other half of #185: marking
// git.sjo.lol/local/tools#5 reviewed must produce a key, not an error.
func TestParseWorkRefForHosts_OwnerNamedLocal(t *testing.T) {
	got, err := ParseWorkRefForHosts("git.sjo.lol/local/tools#5", []string{"git.sjo.lol"})
	if err != nil {
		t.Fatalf("ParseWorkRefForHosts rejected a real owner named %q: %v", "local", err)
	}
	if want := "git.sjo.lol/local/tools#5"; got != want {
		t.Errorf("ParseWorkRefForHosts = %q, want %q", got, want)
	}
}

func TestParseTeaIssues_MalformedAndEmpty(t *testing.T) {
	if _, _, err := parseTeaIssues("{not an array", "git.sjo.lol"); err == nil {
		t.Error("malformed JSON: want error, got nil")
	}
	for _, in := range []string{"   ", "[]"} {
		items, rawCount, err := parseTeaIssues(in, "git.sjo.lol")
		if err != nil || items != nil || rawCount != 0 {
			t.Errorf("input %q: want (nil, 0, nil), got (%+v, %d, %v)", in, items, rawCount, err)
		}
	}
}

func TestParseWorkRefForHosts(t *testing.T) {
	hosts := []string{"git.sjo.lol"}
	want := "git.sjo.lol/cameron/forgectl#12"
	for _, in := range []string{
		"git.sjo.lol/cameron/forgectl#12",
		"https://git.sjo.lol/cameron/forgectl/issues/12",
		"https://git.sjo.lol/cameron/forgectl/pulls/12",
	} {
		got, err := ParseWorkRefForHosts(in, hosts)
		if err != nil {
			t.Errorf("ParseWorkRefForHosts(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWorkRefForHosts(%q) = %q, want %q", in, got, want)
		}
	}

	// The unqualified github.com forms still work through the same function.
	if got, err := ParseWorkRefForHosts("cameronsjo/forgectl#76", hosts); err != nil || got != "github.com/cameronsjo/forgectl#76" {
		t.Errorf("ParseWorkRefForHosts(github form) = %q, %v, want github.com/cameronsjo/forgectl#76, nil", got, err)
	}

	// An unconfigured host is rejected even though it is otherwise well-formed.
	for _, in := range []string{
		"unknown.example/cameron/forgectl#1",
		"https://unknown.example/cameron/forgectl/issues/1",
	} {
		if _, err := ParseWorkRefForHosts(in, hosts); err == nil {
			t.Errorf("ParseWorkRefForHosts(%q): unconfigured host must be rejected", in)
		}
	}

	// With no hosts configured, a host-qualified ref is rejected outright —
	// this is exactly ParseWorkRef's behavior.
	if _, err := ParseWorkRefForHosts("git.sjo.lol/cameron/forgectl#12", nil); err == nil {
		t.Error("host-qualified ref must be rejected when no hosts are configured")
	}
}

// TestParseWorkRefForHosts_PortedHost pins Fix B: a configured host:port
// (NewGitea already accepts constructing one) must parse back through both
// the slug and URL forms, and an unlisted host:port is rejected even though
// it is otherwise well-formed.
func TestParseWorkRefForHosts_PortedHost(t *testing.T) {
	hosts := []string{"git.sjo.lol:3000"}
	want := "git.sjo.lol:3000/cameron/forgectl#12"
	for _, in := range []string{
		"git.sjo.lol:3000/cameron/forgectl#12",
		"https://git.sjo.lol:3000/cameron/forgectl/issues/12",
		"https://git.sjo.lol:3000/cameron/forgectl/pulls/12",
	} {
		got, err := ParseWorkRefForHosts(in, hosts)
		if err != nil {
			t.Errorf("ParseWorkRefForHosts(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWorkRefForHosts(%q) = %q, want %q", in, got, want)
		}
	}

	// A different, unlisted host:port is rejected even though well-formed.
	for _, in := range []string{
		"git.sjo.lol:4000/cameron/forgectl#12",
		"https://git.sjo.lol:4000/cameron/forgectl/issues/12",
	} {
		if _, err := ParseWorkRefForHosts(in, hosts); err == nil {
			t.Errorf("ParseWorkRefForHosts(%q): unlisted host:port must be rejected", in)
		}
	}
}
