// Package intake turns open GitHub issues into queue rows for `forgectl
// surface intake gh`: the GraphQL query that reads them, the gate that
// decides which issue text the operator trusts, and the brief a row carries.
//
// A drain worker runs as the operator with the operator's allow rules
// (ADR-0010), and an issue's text becomes its brief. So an issue is taken
// only when its author is allowed, the latest labeling of every eligible
// label on it is by an allowed author, and neither its body nor its title
// changed at or after that labeling. Everything the gate reads, and the text
// the brief carries, comes from one GraphQL response, so the text checked is
// the text queued.
package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Query reads one page of open issues carrying any of $labels, oldest first,
// with everything the gate needs. repository.issues holds issues only: its
// nodes are of GraphQL type Issue, never PullRequest. The timeline asks for
// only the four event types the gate reads.
const Query = `query($owner: String!, $name: String!, $labels: [String!], $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    owner { __typename login }
    issues(states: OPEN, labels: $labels, first: $first, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        __typename
        number
        title
        body
        lastEditedAt
        author { __typename login }
        labels(first: 100) { pageInfo { hasNextPage } nodes { name } }
        timelineItems(first: 100, itemTypes: [LABELED_EVENT, UNLABELED_EVENT, RENAMED_TITLE_EVENT, TRANSFERRED_EVENT]) {
          pageInfo { hasNextPage }
          nodes {
            __typename
            ... on LabeledEvent { createdAt actor { __typename login } label { name } }
            ... on UnlabeledEvent { createdAt actor { __typename login } label { name } }
            ... on RenamedTitleEvent { createdAt }
            ... on TransferredEvent { createdAt }
          }
        }
      }
    }
  }
}`

// PageSize is the number of issues one query asks for.
const PageSize = 25

// Timeline event and node type names, as GraphQL's __typename gives them.
const (
	typeIssue       = "Issue"
	typeUser        = "User"
	typeLabeled     = "LabeledEvent"
	typeUnlabeled   = "UnlabeledEvent"
	typeRenamed     = "RenamedTitleEvent"
	typeTransferred = "TransferredEvent"
	// OwnerUser and OwnerOrganization are repository.owner's __typename.
	OwnerUser         = "User"
	OwnerOrganization = "Organization"
)

// Actor is a GraphQL Actor: a User, a Bot, a Mannequin, an Organization.
// GraphQL gives a bot's login without the "[bot]" suffix the REST API adds,
// so __typename is what tells a bot apart.
type Actor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// Event is one timeline item. Label and Actor are set on labeled and
// unlabeled events only.
type Event struct {
	Typename  string `json:"__typename"`
	CreatedAt string `json:"createdAt"`
	Actor     *Actor `json:"actor"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// Issue is one issue node as the query returns it.
type Issue struct {
	Typename string `json:"__typename"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	// LastEditedAt is the raw field: absent (len 0), the JSON null of a body
	// never edited, or a timestamp. Only an explicit null means never edited.
	LastEditedAt json.RawMessage `json:"lastEditedAt"`
	Author       *Actor          `json:"author"`
	Labels       struct {
		PageInfo pageInfo `json:"pageInfo"`
		Nodes    []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	TimelineItems struct {
		PageInfo pageInfo `json:"pageInfo"`
		Nodes    []*Event `json:"nodes"`
	} `json:"timelineItems"`
}

// Page is one decoded query response.
type Page struct {
	OwnerType   string
	OwnerLogin  string
	Issues      []Issue
	HasNextPage bool
	EndCursor   string
}

// ErrResponse reports a GraphQL response intake will not read: one carrying
// errors, or missing the repository, its owner, or its issues.
var ErrResponse = errors.New("intake: unusable GitHub response")

// maxErrorText bounds the GraphQL error message quoted in ErrResponse.
const maxErrorText = 200

// DecodePage reads one query response. Any GraphQL error refuses the whole
// page, even beside data: a partial response could drop a field the gate
// needs and leave it looking absent.
func DecodePage(data []byte) (Page, error) {
	var resp struct {
		Data *struct {
			Repository *struct {
				Owner  *Actor `json:"owner"`
				Issues *struct {
					PageInfo pageInfo           `json:"pageInfo"`
					Nodes    []*json.RawMessage `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return Page{}, fmt.Errorf("%w: not JSON: %w", ErrResponse, err)
	}
	if len(resp.Errors) > 0 {
		msg := resp.Errors[0].Message
		if len(msg) > maxErrorText {
			msg = msg[:maxErrorText]
		}
		return Page{}, fmt.Errorf("%w: GitHub returned %d error(s), the first %q", ErrResponse, len(resp.Errors), msg)
	}
	if resp.Data == nil || resp.Data.Repository == nil {
		return Page{}, fmt.Errorf("%w: no repository in the response", ErrResponse)
	}
	repo := resp.Data.Repository
	if repo.Owner == nil || repo.Owner.Login == "" || repo.Owner.Typename == "" {
		return Page{}, fmt.Errorf("%w: no repository owner in the response", ErrResponse)
	}
	if repo.Issues == nil {
		return Page{}, fmt.Errorf("%w: no issues connection in the response", ErrResponse)
	}
	p := Page{
		OwnerType: repo.Owner.Typename, OwnerLogin: repo.Owner.Login,
		HasNextPage: repo.Issues.PageInfo.HasNextPage, EndCursor: repo.Issues.PageInfo.EndCursor,
	}
	if p.HasNextPage && p.EndCursor == "" {
		return Page{}, fmt.Errorf("%w: a next page with no cursor", ErrResponse)
	}
	for _, raw := range repo.Issues.Nodes {
		if raw == nil || bytes.Equal(*raw, []byte("null")) {
			return Page{}, fmt.Errorf("%w: a null issue node", ErrResponse)
		}
		var is Issue
		if err := json.Unmarshal(*raw, &is); err != nil {
			return Page{}, fmt.Errorf("%w: issue node: %w", ErrResponse, err)
		}
		if is.Number <= 0 {
			return Page{}, fmt.Errorf("%w: an issue node with no number", ErrResponse)
		}
		p.Issues = append(p.Issues, is)
	}
	return p, nil
}

// ErrOrgNeedsAuthors reports an organization-owned repository with no
// [surface.intake] authors: its owner's login names no person.
var ErrOrgNeedsAuthors = errors.New("intake: the repository is owned by an organization; set [surface.intake] authors to the logins whose issues intake may take")

// Authors returns the logins the gate allows: configured when it is not
// empty, else the owner's login for a user-owned repository. Any other owner
// with no configured authors is refused.
func Authors(ownerType, ownerLogin string, configured []string) ([]string, error) {
	if len(configured) > 0 {
		return configured, nil
	}
	switch ownerType {
	case OwnerUser:
		if ownerLogin == "" {
			return nil, fmt.Errorf("%w: the owner has no login", ErrResponse)
		}
		return []string{ownerLogin}, nil
	case OwnerOrganization:
		return nil, ErrOrgNeedsAuthors
	default:
		return nil, fmt.Errorf("intake: the repository owner is a %q, not a user or an organization; set [surface.intake] authors", ownerType)
	}
}

// Rules is what the gate checks an issue against.
type Rules struct {
	// Authors are the allowed logins, compared case-insensitively and
	// exactly.
	Authors []string
	// Labels are the eligible label names, compared case-insensitively as
	// GitHub does.
	Labels []string
}

// allowed reports whether a is a user on the allowed list. A bot never is,
// whatever its login: GraphQL types it Bot, and a login ending "[bot]" is
// refused too in case a response carries the REST spelling.
func (r Rules) allowed(a *Actor) bool {
	if a == nil || a.Typename != typeUser || a.Login == "" {
		return false
	}
	if strings.HasSuffix(strings.ToLower(a.Login), "[bot]") {
		return false
	}
	return slices.ContainsFunc(r.Authors, func(s string) bool { return strings.EqualFold(s, a.Login) })
}

func (r Rules) eligible(label string) bool {
	return slices.ContainsFunc(r.Labels, func(s string) bool { return strings.EqualFold(s, label) })
}

// Refusal is why the gate did not take an issue.
type Refusal struct{ Reason string }

func (e *Refusal) Error() string { return e.Reason }

func refuse(format string, a ...any) error { return &Refusal{Reason: fmt.Sprintf(format, a...)} }

// who names an actor for a refusal reason.
func who(a *Actor) string {
	if a == nil {
		return "a deleted account"
	}
	return fmt.Sprintf("%s %q", strings.ToLower(a.Typename), a.Login)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// Admit is the gate. It returns nil when intake may queue is, or a *Refusal
// naming the first check it fails:
//
//   - is is an issue, authored by a user on r.Authors (never a bot);
//   - its labels and timeline fit in one page each, so nothing is unseen;
//   - it was never transferred;
//   - it carries at least one eligible label, and for each one it carries,
//     the latest labeled event for that name is by an allowed user, and no
//     unlabeled event for it is at or after that time;
//   - lastEditedAt is an explicit null, or a time strictly before the
//     earliest of those latest labelings;
//   - no title rename is at or after that time.
//
// The earliest labeling is the one that counts: every eligible label on the
// issue has to have been put there after the text it carries was final.
func Admit(is Issue, r Rules) error {
	if is.Typename != typeIssue {
		return refuse("it is a %q, not an issue", is.Typename)
	}
	if !r.allowed(is.Author) {
		return refuse("its author, %s, is not on [surface.intake] authors", who(is.Author))
	}
	if is.Labels.PageInfo.HasNextPage {
		return refuse("it has more labels than one query reads")
	}
	if is.TimelineItems.PageInfo.HasNextPage {
		return refuse("its label and title history is longer than one query reads")
	}
	var present []string
	for _, l := range is.Labels.Nodes {
		if r.eligible(l.Name) && !slices.ContainsFunc(present, func(p string) bool { return strings.EqualFold(p, l.Name) }) {
			present = append(present, l.Name)
		}
	}
	if len(present) == 0 {
		return refuse("it carries no eligible label")
	}
	for _, ev := range is.TimelineItems.Nodes {
		if ev == nil {
			return refuse("its timeline holds an unreadable event")
		}
		if ev.Typename == typeTransferred {
			return refuse("it was transferred from another repository")
		}
	}
	var attested time.Time
	for _, name := range present {
		at, err := latestLabeling(is.TimelineItems.Nodes, name, r)
		if err != nil {
			return err
		}
		if attested.IsZero() || at.Before(attested) {
			attested = at
		}
	}
	switch edited := bytes.TrimSpace(is.LastEditedAt); {
	case len(edited) == 0:
		return refuse("the response does not say whether its body was edited")
	case bytes.Equal(edited, []byte("null")):
	default:
		var s string
		if err := json.Unmarshal(edited, &s); err != nil {
			return refuse("its lastEditedAt is not a timestamp")
		}
		t, err := parseTime(s)
		if err != nil {
			return refuse("its lastEditedAt %q is not a timestamp", s)
		}
		if !t.Before(attested) {
			return refuse("its body was edited at %s, not before it was labeled at %s", t.UTC().Format(time.RFC3339), attested.UTC().Format(time.RFC3339))
		}
	}
	for _, ev := range is.TimelineItems.Nodes {
		if ev.Typename != typeRenamed {
			continue
		}
		t, err := parseTime(ev.CreatedAt)
		if err != nil {
			return refuse("a title rename has no readable time")
		}
		if !t.Before(attested) {
			return refuse("its title was renamed at %s, not before it was labeled at %s", t.UTC().Format(time.RFC3339), attested.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// latestLabeling returns the time of the latest labeled event for name, after
// checking that every labeled event at that time is by an allowed user and no
// unlabeled event for name is at or after it. GitHub's timestamps are whole
// seconds and its timeline lists same-second events in no reliable order, so
// a same-second unlabel refuses rather than being read as earlier.
func latestLabeling(events []*Event, name string, r Rules) (time.Time, error) {
	var latest time.Time
	var at []*Event
	for _, ev := range events {
		if ev.Typename != typeLabeled || ev.Label == nil || !strings.EqualFold(ev.Label.Name, name) {
			continue
		}
		t, err := parseTime(ev.CreatedAt)
		if err != nil {
			return time.Time{}, refuse("a %q labeling has no readable time", name)
		}
		switch {
		case latest.IsZero() || t.After(latest):
			latest, at = t, []*Event{ev}
		case t.Equal(latest):
			at = append(at, ev)
		}
	}
	if latest.IsZero() {
		return time.Time{}, refuse("its timeline has no labeled event for %q", name)
	}
	for _, ev := range at {
		if !r.allowed(ev.Actor) {
			return time.Time{}, refuse("its %q label was last added by %s, who is not on [surface.intake] authors", name, who(ev.Actor))
		}
	}
	for _, ev := range events {
		if ev.Typename != typeUnlabeled {
			continue
		}
		if ev.Label == nil {
			return time.Time{}, refuse("its timeline has an unlabeled event with no label")
		}
		if !strings.EqualFold(ev.Label.Name, name) {
			continue
		}
		t, err := parseTime(ev.CreatedAt)
		if err != nil {
			return time.Time{}, refuse("a %q unlabeling has no readable time", name)
		}
		if !t.Before(latest) {
			return time.Time{}, refuse("its %q label was removed at %s, not before it was last added at %s", name, t.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
		}
	}
	return latest, nil
}
