package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// reGiteaHost anchors a configured Gitea host to a bare hostname, optionally
// with a port: letters, digits, dots, hyphens, and a trailing ":port". It
// deliberately excludes "/", whitespace, and a scheme prefix — those would
// corrupt both the persisted reviewed-store key (Item.Key() embeds Host
// verbatim) and the literal https://<host>/ prefix giteaItemURL compares
// against.
var reGiteaHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// Gitea enumerates open issues and PRs, owner-scoped, from a self-hosted
// Gitea instance over the tea CLI. It is the second Source review.Aggregate
// fans out to (Phase C), alongside GitHub — same degrade-to-note model, a
// different wire format underneath.
type Gitea struct {
	run    exec.Runner
	host   string
	login  string
	owners []string
}

// NewGitea builds the source over run for the given host/login/owners. Host
// is validated NOW, not deferred to Items: it lands verbatim in every Item's
// Host field (and therefore the persisted reviewed-store key and any
// synthesized item URL), so a malformed host must never reach construction.
// Owners are config input too, but — mirroring NewGitHub — are validated
// per-query in Items rather than here.
func NewGitea(run exec.Runner, host, login string, owners []string) (*Gitea, error) {
	if !reGiteaHost.MatchString(host) {
		return nil, fmt.Errorf("gitea source: host %q outside allowed charset", host)
	}
	return &Gitea{run: run, host: host, login: login, owners: owners}, nil
}

// Name implements Source.
func (g *Gitea) Name() string { return "gitea" }

// Host returns the configured Gitea host. Exposed for the CLI wiring layer,
// which needs it to build the host allowlist for review.ParseWorkRefForHosts
// (mark/unmark) without reaching back into config.
func (g *Gitea) Host() string { return g.host }

// giteaQueryResult carries one owner's tea query outcome across the
// fan-out channel — the same Inventory model github.go's ghQueryResult uses.
type giteaQueryResult struct {
	label     string
	items     []Item
	truncated bool
	err       error
}

// Items runs one `tea issues list --kind all` query per owner, concurrently.
// Unlike GitHub's two-legs-per-owner (issues + PRs are separate gh
// endpoints), tea's --kind all returns both shapes from a single call. A
// degraded owner contributes a note; Items errors only when every owner
// query failed or when no owners are configured.
func (g *Gitea) Items(ctx context.Context) ([]Item, []string, error) {
	if len(g.owners) == 0 {
		return nil, nil, fmt.Errorf("gitea source: no owners configured")
	}

	ch := make(chan giteaQueryResult, len(g.owners))
	for _, owner := range g.owners {
		go func() {
			items, truncated, err := g.issuesForOwner(ctx, owner)
			ch <- giteaQueryResult{fmt.Sprintf("gitea(%s)", owner), items, truncated, err}
		}()
	}

	var notes []string
	var items []Item
	failed := 0
	for range g.owners {
		res := <-ch
		if res.err != nil {
			slog.Warn("Gitea review query degraded.", "query", res.label, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.label, res.err))
			failed++
			continue
		}
		if res.truncated {
			notes = append(notes, fmt.Sprintf("%s: results may be truncated at %d", res.label, searchLimit))
		}
		items = append(items, res.items...)
	}
	if failed == len(g.owners) {
		return nil, notes, fmt.Errorf("gitea source: every owner query failed")
	}
	slog.Info("Successfully loaded Gitea review inventory.", "items", len(items), "owners", len(g.owners), "degraded_owners", failed)
	return items, notes, nil
}

// issuesForOwner runs the tea query for one owner. The owner value comes
// from config — low-trust input headed for an argv — so it is vetted
// through the same anchored guard every review/pr owner is, BEFORE the
// argv is built. A bad owner surfaces from tea as an exit 1 with stderr
// "user does not exist"; that error degrades to a note same as any other.
func (g *Gitea) issuesForOwner(ctx context.Context, owner string) ([]Item, bool, error) {
	if !pr.ValidOwnerRepoPart(owner) {
		return nil, false, fmt.Errorf("owner %q outside allowed charset", owner)
	}
	args := []string{"issues", "list",
		"--owner", owner,
		"--kind", "all",
		"--state", "open",
		"--output", "json",
		"--fields", "index,kind,state,author,url,title,updated,labels,owner,repo",
		"--limit", fmt.Sprint(searchLimit),
	}
	// login is optional config: omit the flag entirely rather than pass an
	// empty value, so an unset login falls back to tea's own default
	// (whatever `tea login` last configured) instead of forcing an empty
	// string tea would have to interpret itself.
	if g.login != "" {
		args = append(args, "--login", g.login)
	}
	out, err := g.run.Run(ctx, "tea", args...)
	if err != nil {
		return nil, false, err
	}
	items, rawCount, err := parseTeaIssues(out, g.host)
	if err != nil {
		return nil, false, err
	}
	// Raw-count comparison, mirroring searchIssues: a skipped hostile row in
	// an exactly-full response must not silence the truncation sentinel.
	return items, rawCount >= searchLimit, nil
}

// teaIssueRow is the on-the-wire shape of one `tea issues list --output
// json` row against the --fields set issuesForOwner requests. Verified
// live against tea, this diverges from gh's shape in several ways: Index is
// a JSON STRING (not a number), Kind is "Pull"/"Issue" (not gh's separate
// endpoints), Labels is a single comma-joined string (not an array), and
// there is NO draft field at all — Gitea pull requests have no draft
// state, so every Gitea PR Item.IsDraft is unconditionally false.
type teaIssueRow struct {
	Index   string    `json:"index"`
	Kind    string    `json:"kind"`
	State   string    `json:"state"`
	Author  string    `json:"author"`
	URL     string    `json:"url"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	Labels  string    `json:"labels"`
	Owner   string    `json:"owner"`
	Repo    string    `json:"repo"`
}

// teaKind maps tea's Kind string to a Kind, case-insensitively. ok is false
// for anything else — an unknown kind is skipped by the caller rather than
// guessed at.
func teaKind(s string) (kind Kind, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pull":
		return KindPR, true
	case "issue":
		return KindIssue, true
	default:
		return "", false
	}
}

// splitTeaLabels splits tea's comma-joined labels string into a slice,
// trimming whitespace around each label and dropping empties. An empty
// input yields nil, matching gh's "no labels" shape.
func splitTeaLabels(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// giteaItemURL resolves the URL to render for item: tea's own url when it is
// actually on the configured host, else a synthesized one. tea's url field is
// server-supplied — hostile input — so it is accepted only when it is
// literally prefixed by the configured host's https origin; anything else
// (a foreign host, a scheme mismatch, embedded whitespace) is replaced with a
// same-host URL built from the already-validated owner/repo/number. Gitea's
// pull request path segment is "pulls" (plural), unlike GitHub's "pull".
func giteaItemURL(rawURL, host string, item Item) string {
	prefix := "https://" + host + "/"
	if strings.HasPrefix(rawURL, prefix) && !strings.ContainsAny(rawURL, " \t\r\n") {
		return rawURL
	}
	segment := "issues"
	if item.Kind == KindPR {
		segment = "pulls"
	}
	return fmt.Sprintf("https://%s/%s/%s/%s/%d", host, item.Owner, item.Repo, segment, item.Number)
}

// parseTeaIssues decodes `tea issues list --output json` output into Items.
// tea output is hostile input: each row routes through the canonical
// anchored validator (itemFromParts/pr.RefFromParts), and a row with a
// non-numeric index or an unrecognized kind is skipped (logged), never
// fatal. An empty or whitespace-only response, and a literal `[]`, both
// yield (nil, 0, nil).
//
// rawCount is the PRE-filter row count for the truncation sentinel (see
// issuesForOwner) — a skipped hostile row in an exactly-full response must
// not silence the cap.
func parseTeaIssues(jsonOut, host string) (items []Item, rawCount int, err error) {
	if strings.TrimSpace(jsonOut) == "" {
		return nil, 0, nil
	}
	var raw []teaIssueRow
	if err := json.Unmarshal([]byte(jsonOut), &raw); err != nil {
		return nil, 0, fmt.Errorf("parse tea issues output: %w", err)
	}
	if len(raw) == 0 {
		return nil, 0, nil
	}
	out := make([]Item, 0, len(raw))
	for _, r := range raw {
		n, convErr := strconv.Atoi(r.Index)
		if convErr != nil {
			slog.Warn("Skipping tea row with non-numeric index.", "index", r.Index, "error", convErr)
			continue
		}
		kind, ok := teaKind(r.Kind)
		if !ok {
			slog.Warn("Skipping tea row with unrecognized kind.", "kind", r.Kind)
			continue
		}
		item, itemErr := itemFromParts(kind, host, r.Owner, r.Repo, n)
		if itemErr != nil {
			slog.Warn("Skipping tea row with invalid ref.", "owner", r.Owner, "repo", r.Repo, "index", r.Index, "error", itemErr)
			continue
		}
		item.Title = r.Title
		item.Author = r.Author
		item.State = strings.ToLower(r.State)
		item.Labels = splitTeaLabels(r.Labels)
		item.UpdatedAt = r.Updated
		item.URL = giteaItemURL(r.URL, host, item)
		out = append(out, item)
	}
	return out, len(raw), nil
}
