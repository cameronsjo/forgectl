package pr

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// DefaultSearchLimit is the --limit every @me-scoped `gh search prs` query
// passes when SearchOpts.Limit is zero. gh's own default is 30, which silently
// truncated `pr prs`/`pr dash` whenever a query exceeded it — the limit is now
// always explicit, and hitting it surfaces as a truncation note, never a
// silent cap.
const DefaultSearchLimit = 200

// SearchOpts scopes one `gh search prs` query. Exactly one of WhoFlag or Owner
// must be set: WhoFlag is the @me-involvement scoping `pr prs`/`pr dash` use
// (--author / --assignee / --review-requested), Owner is the owner-wide
// inventory scoping `forgectl review` uses.
type SearchOpts struct {
	WhoFlag string // one of the three allowlisted @me flags
	Owner   string // --owner value; validated against the anchored charset
	Limit   int    // rows per query; 0 → DefaultSearchLimit
}

// searchWhoFlags is the exact allowlist of @me scoping flags. SearchOpts input
// can originate from config, so the flag is matched deny-by-default rather
// than passed through to the argv.
var searchWhoFlags = map[string]bool{
	"--author":           true,
	"--assignee":         true,
	"--review-requested": true,
}

// SearchPRs runs one open-PR `gh search prs` query over run and parses the
// rows. It is the ONE search path both surfaces share: pr prs/dash call it
// with WhoFlag, internal/review's GitHub source calls it with Owner. truncated
// reports that the row count hit the (always explicit) --limit, so the caller
// can degrade to a note instead of silently under-reporting.
func SearchPRs(ctx context.Context, run exec.Runner, opts SearchOpts) (prs []PR, truncated bool, err error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}

	args := []string{"search", "prs"}
	switch {
	case opts.WhoFlag != "" && opts.Owner != "":
		return nil, false, fmt.Errorf("SearchOpts: WhoFlag and Owner are mutually exclusive")
	case opts.WhoFlag != "":
		if !searchWhoFlags[opts.WhoFlag] {
			return nil, false, fmt.Errorf("SearchOpts: unsupported who-flag %q", opts.WhoFlag)
		}
		args = append(args, opts.WhoFlag, "@me")
	case opts.Owner != "":
		// Owner can come from config — low-trust input headed for an argv.
		if !ValidOwnerRepoPart(opts.Owner) {
			return nil, false, fmt.Errorf("SearchOpts: owner %q outside allowed charset", opts.Owner)
		}
		args = append(args, "--owner", opts.Owner)
	default:
		return nil, false, fmt.Errorf("SearchOpts: one of WhoFlag or Owner is required")
	}
	args = append(args, "--state", "open", "--json", prSearchFields, "--limit", strconv.Itoa(limit))

	out, err := run.Run(ctx, "gh", args...)
	if err != nil {
		return nil, false, err
	}
	prs, err = parseSearchPRs(out)
	if err != nil {
		return nil, false, err
	}
	return prs, len(prs) >= limit, nil
}
