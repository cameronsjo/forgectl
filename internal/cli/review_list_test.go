package cli

// Test plan for review.go / review_list.go
//
// newReviewCmdForSources (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid array; reviewed field true for a marked
//       key; labels [] never null; empty result → [] not null
//   [x] Happy: human table lists KIND/REPO/#/TITLE/LABELS/STATE; reviewed row
//       dimmed (ANSI under a forced color profile), unreviewed plain
//   [x] Happy: --kind/--repo filter rows; invalid --kind is an error
//   [x] Happy: source notes land on stderr, not stdout
//   [x] Unhappy: a hostile title/label/state/note (ESC + C0 mix, a raw tab)
//       renders with no control bytes surviving (forgectl#162)

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/review"
)

var reviewTestTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

// fakeReviewSource is a canned review.Source for CLI tests.
type fakeReviewSource struct {
	items []review.Item
	notes []string
	err   error
}

func (f fakeReviewSource) Name() string { return "fake" }
func (f fakeReviewSource) Items(context.Context) ([]review.Item, []string, error) {
	return f.items, f.notes, f.err
}

func reviewItem(kind review.Kind, repo string, number int, labels ...string) review.Item {
	return review.Item{
		Kind: kind, Host: review.GitHubHost, Owner: "cameronsjo", Repo: repo, Number: number,
		Title: "title", Author: "cameronsjo", State: "open", Labels: labels,
		UpdatedAt: reviewTestTime,
		URL:       "https://github.com/cameronsjo/" + repo,
	}
}

// seedReviewedKey marks key reviewed at `at` in the store at path.
func seedReviewedKey(t *testing.T, path, key string, at time.Time) {
	t.Helper()
	store := pr.LoadReviewed(path, pr.WithNow(func() time.Time { return at }))
	if err := store.MarkKey(key); err != nil {
		t.Fatalf("seed reviewed %s: %v", key, err)
	}
}

func TestReviewCmd_JSON_ReviewedAndLabels(t *testing.T) {
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "forgectl", 76, "epic"),
		reviewItem(review.KindPR, "forgectl", 77),
	}}
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/forgectl#76", reviewTestTime.Add(time.Hour))

	cmd := newReviewCmdForSources([]review.Source{src}, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review --json: %v", err)
	}

	var rows []reviewRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byKey := map[string]reviewRowJSON{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if !byKey["github.com/cameronsjo/forgectl#76"].Reviewed {
		t.Errorf("#76 should be reviewed=true")
	}
	if byKey["github.com/cameronsjo/forgectl#77"].Reviewed {
		t.Errorf("#77 should be reviewed=false")
	}
	// labels must be [] never null.
	if strings.Contains(stdout.String(), `"labels": null`) {
		t.Errorf("labels must serialize as [], never null:\n%s", stdout.String())
	}
}

func TestReviewCmd_JSON_EmptyIsArray(t *testing.T) {
	cmd := newReviewCmdForSources([]review.Source{fakeReviewSource{}}, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review --json empty: %v", err)
	}
	var rows []reviewRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("empty --json not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 0 {
		t.Errorf("empty --json: want 0 rows, got %+v", rows)
	}
	if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "[") {
		t.Errorf("empty --json: want array (never null), got %q", got)
	}
}

func TestReviewCmd_Table_DimsReviewedRow(t *testing.T) {
	forceColor(t)
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "alpha", 1, "auto:execute"),
		reviewItem(review.KindPR, "bravo", 2),
	}}
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/alpha#1", reviewTestTime.Add(time.Hour))

	cmd := newReviewCmdForSources([]review.Source{src}, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review: %v", err)
	}

	var alphaLine, bravoLine string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "cameronsjo/alpha") {
			alphaLine = line
		}
		if strings.Contains(line, "cameronsjo/bravo") {
			bravoLine = line
		}
	}
	if alphaLine == "" || bravoLine == "" {
		t.Fatalf("missing rows; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(alphaLine, "\x1b[") {
		t.Errorf("reviewed row should be dimmed (ANSI), got %q", alphaLine)
	}
	if strings.Contains(bravoLine, "\x1b[") {
		t.Errorf("unreviewed row should be plain, got %q", bravoLine)
	}
	if !strings.Contains(alphaLine, "auto:execute") {
		t.Errorf("labels column missing auto:execute: %q", alphaLine)
	}
	if !strings.Contains(stderr.String(), "1 reviewed") {
		t.Errorf("want '1 reviewed' in stderr summary, got %q", stderr.String())
	}
}

func TestReviewCmd_KindAndRepoFilters(t *testing.T) {
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "alpha", 1),
		reviewItem(review.KindPR, "alpha", 2),
		reviewItem(review.KindIssue, "bravo", 3),
	}}

	run := func(args ...string) []reviewRowJSON {
		t.Helper()
		cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append(args, "--json"))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("review %v: %v", args, err)
		}
		var rows []reviewRowJSON
		if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		return rows
	}

	if rows := run("--kind", "issue"); len(rows) != 2 {
		t.Errorf("--kind issue: got %d rows, want 2", len(rows))
	}
	if rows := run("--kind", "pr"); len(rows) != 1 {
		t.Errorf("--kind pr: got %d rows, want 1", len(rows))
	}
	if rows := run("--repo", "cameronsjo/bravo"); len(rows) != 1 || rows[0].Number != 3 {
		t.Errorf("--repo filter: got %+v", rows)
	}

	cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--kind", "bogus"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Error("invalid --kind must be an error")
	}
}

// TestReviewHelp_NamesNoBakedAccount covers issue #191 at the compiled-help
// surface: the fallback is the authenticated GitHub.com account, and the help
// must say so rather than naming one developer's login.
func TestReviewHelp_NamesNoBakedAccount(t *testing.T) {
	help := newReviewCmdForSources(nil, "").Long

	if strings.Contains(help, "cameronsjo") {
		t.Errorf("review help still names a baked account:\n%s", help)
	}
	for _, want := range []string{"authenticated", "github.com"} {
		if !strings.Contains(strings.ToLower(help), want) {
			t.Errorf("review help does not mention %q:\n%s", want, help)
		}
	}
}

// TestResolveGiteaSource pins the three outcomes: disabled (whatever else is
// set) is silently omitted; enabled-but-broken is an error, never a warn-and-
// omit; a valid config builds a source whose Host() feeds extraHosts.
func TestResolveGiteaSource(t *testing.T) {
	deps := func(gc config.GiteaConfig) module.Deps {
		var cfg config.Config
		cfg.Review.Gitea = gc
		return module.Deps{Cfg: cfg, Runner: &exec.FakeRunner{}}
	}

	t.Run("absent section is silently omitted", func(t *testing.T) {
		src, ok, err := resolveGiteaSource(deps(config.GiteaConfig{}))
		if err != nil || ok || src != nil {
			t.Errorf("absent: got (%v, %v, %v), want (nil, false, nil)", src, ok, err)
		}
	})

	t.Run("disabled with other fields set is still silently omitted", func(t *testing.T) {
		src, ok, err := resolveGiteaSource(deps(config.GiteaConfig{Host: "git.sjo.lol", Owners: []string{"cameron"}}))
		if err != nil || ok || src != nil {
			t.Errorf("disabled: got (%v, %v, %v), want (nil, false, nil)", src, ok, err)
		}
	})

	t.Run("enabled with empty host is an error", func(t *testing.T) {
		_, ok, err := resolveGiteaSource(deps(config.GiteaConfig{Enabled: true}))
		if err == nil || ok {
			t.Errorf("enabled+empty host: want an error, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("enabled with a malformed host is an error", func(t *testing.T) {
		_, ok, err := resolveGiteaSource(deps(config.GiteaConfig{Enabled: true, Host: "bad host"}))
		if err == nil || ok {
			t.Errorf("enabled+malformed host: want an error, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("valid config builds a source whose Host feeds extraHosts", func(t *testing.T) {
		src, ok, err := resolveGiteaSource(deps(config.GiteaConfig{
			Enabled: true, Host: "git.sjo.lol", Login: "cameron", Owners: []string{"cameron"},
		}))
		if err != nil || !ok || src == nil {
			t.Fatalf("valid config: got (%v, %v, %v), want a source", src, ok, err)
		}
		if hosts := extraHosts([]review.Source{src}); len(hosts) != 1 || hosts[0] != "git.sjo.lol" {
			t.Errorf("extraHosts = %v, want [git.sjo.lol]", hosts)
		}
	})
}

// TestNewReviewCmd_GiteaConfigError pins that a configured-but-invalid
// [review.gitea] fails the WHOLE `review` command tree — bare, mark, unmark,
// sync all return the same config error — rather than silently narrowing to
// GitHub-only.
func TestNewReviewCmd_GiteaConfigError(t *testing.T) {
	var cfg config.Config
	cfg.Review.Gitea = config.GiteaConfig{Enabled: true} // enabled, no host: invalid
	deps := module.Deps{Cfg: cfg, Runner: &exec.FakeRunner{}}

	for _, args := range [][]string{
		{},
		{"mark", "cameronsjo/forgectl#1"},
		{"unmark", "cameronsjo/forgectl#1"},
		{"sync"},
	} {
		cmd := newReviewCmd(deps)
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Errorf("args %v: want config error, got nil", args)
		}
	}
}

func TestReviewCmd_NotesOnStderr(t *testing.T) {
	src := fakeReviewSource{
		items: []review.Item{reviewItem(review.KindIssue, "alpha", 1)},
		notes: []string{"issues(cameronsjo): gh: rate limited"},
	}
	cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review (degraded): %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("notes must not leak to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation note missing from stderr: %q", stderr.String())
	}
}

// TestReviewCmd_Table_HostileTitleSanitized pins forgectl#162 end-to-end: a
// server-supplied title carrying an ESC-based cursor-control sequence (mixed
// with other C0 bytes) must render with no control bytes surviving in the
// human table output — the sink the issue calls out (review_list.go's
// tabwriter render), not just the sanitizer function in isolation.
func TestReviewCmd_Table_HostileTitleSanitized(t *testing.T) {
	hostile := review.Item{
		Kind: review.KindIssue, Host: review.GitHubHost, Owner: "cameronsjo", Repo: "alpha", Number: 1,
		Title: "safe\x1b[2K\x1b[Gtitle\x00\x07end", Author: "cameronsjo", State: "open",
		Labels:    []string{"a\x1bb"},
		UpdatedAt: reviewTestTime,
	}
	src := fakeReviewSource{items: []review.Item{hostile}}
	cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review: %v", err)
	}

	out := stdout.String()
	for _, line := range strings.Split(out, "\n") {
		if hasControlByte(line) {
			t.Errorf("rendered table line still contains a control byte: %q", line)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "title") || !strings.Contains(out, "end") {
		t.Errorf("sanitized title lost its visible content: %q", out)
	}
}

// hasControlByte reports whether s contains any C0 control byte (0x00-0x1F)
// or DEL (0x7F) — the set sanitizeCell is expected to have already removed
// from anything it processed. Callers split multi-line output on "\n" first,
// since that legitimate line separator is not itself hostile content.
func hasControlByte(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// TestReviewStateLabel_TabSanitized pins that reviewStateLabel's output is
// sanitized at its render call site: a hostile tracker State value carrying
// a tab (which would otherwise inject a tabwriter column) must not survive
// into the rendered STATE cell.
func TestReviewStateLabel_TabSanitized(t *testing.T) {
	hostile := review.Item{
		Kind: review.KindIssue, Host: review.GitHubHost, Owner: "cameronsjo", Repo: "alpha", Number: 1,
		Title: "title", Author: "cameronsjo", State: "open\tINJECTED",
		UpdatedAt: reviewTestTime,
	}
	src := fakeReviewSource{items: []review.Item{hostile}}
	cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if strings.Contains(stdout.String(), "\t") {
		t.Errorf("hostile State must not inject a raw tab into the rendered table: %q", stdout.String())
	}
}

// TestReviewCmd_HostileNoteSanitized pins the note-line sink the issue calls
// out as having no sanitizer at all: a degradation note containing an ESC
// sequence must not reach stderr with its control bytes intact.
func TestReviewCmd_HostileNoteSanitized(t *testing.T) {
	src := fakeReviewSource{
		items: []review.Item{reviewItem(review.KindIssue, "alpha", 1)},
		notes: []string{"issues(cameronsjo): tea: \x1b[2K\x1b[Guser does not exist"},
	}
	cmd := newReviewCmdForSources([]review.Source{src}, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review (hostile note): %v", err)
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if hasControlByte(line) {
			t.Errorf("stderr note line still contains a control byte: %q", line)
		}
	}
	if !strings.Contains(stderr.String(), "user does not exist") {
		t.Errorf("sanitized note lost its visible content: %q", stderr.String())
	}
}
