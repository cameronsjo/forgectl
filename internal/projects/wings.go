package projects

import (
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// Wing is one placement rule from `[[projects.wings]]`: a named directory
// directly under the projects root that Repos are filed into, instead of the
// host/owner/name tree. Repos entries are "owner/name", matched
// case-insensitively.
//
// It mirrors config.WingConfig rather than reusing it: internal/config cannot
// import internal/githubauth (config ← pr ← githubauth is a cycle), so the
// validation this table needs has to live on this side of the seam, and a
// plain value type here keeps internal/projects free of a config dependency.
type Wing struct {
	Name  string
	Repos []string
}

// WingTable answers "which wing does this repo belong to" for clone and
// worktree placement. The zero value is a valid empty table: every lookup
// misses, and every repo lands in the host tree — which is also the behavior
// when no wings are configured at all.
//
// This is PLACEMENT, and it is deliberately config-driven rather than
// inferred: where a NEW clone belongs is a judgment about how the operator
// groups work, and disk state cannot answer it — a wing on disk tells you what
// already lives there, not what should. Wing DISCOVERY is the mirror-image
// decision and is structural, so a wing missing from this table is still
// listed by `projects list`; it just is not a clone target.
type WingTable struct {
	byRepo map[string]string // lowercased "owner/name" → wing name
}

// For returns the wing owner/name is filed into, or "" when the repo is not in
// any wing. A caller that gets "" uses the host/owner/name tree.
func (t WingTable) For(owner, name string) string {
	if t.byRepo == nil || owner == "" || name == "" {
		return ""
	}
	return t.byRepo[strings.ToLower(owner+"/"+name)]
}

// ValidateWingName is the one wing-name rule, shared by the config table and
// the `projects clone --wing` flag. It returns the normalized (lowercased,
// trimmed) name.
//
// Two rejections, both because the name becomes a directory directly under the
// projects root:
//
//  1. Outside the path-segment charset. ':' is APFS-legal and renders as '/'
//     in Finder, a leading '.' hides the tree from ls, and a homoglyph makes
//     two wings that look identical.
//  2. Equal to the configured GitHub host. The wing directory and the host
//     tree would be the same directory with different leaf semantics — a wing
//     member sits one level down, a host-tree clone two — so a repo whose name
//     matched an owner would land inside a real owner directory.
//
// It is a separate function because the flag is a SECOND entry point into the
// same namespace and does not go through ResolveWings. Two entry points, one
// rule.
func ValidateWingName(gitHubHost, wing string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(wing))
	if name == "" {
		return "", fmt.Errorf("wing name is empty")
	}
	if !githubauth.ValidHostSegment(name) {
		// Categorical, like ResolveHost's own rejections: the value is
		// operator input, but it is also about to be a directory name and the
		// error prints to a terminal.
		return "", fmt.Errorf("wing name is outside the allowed charset " +
			"(lowercase letters, digits, '.' and '-'; it becomes a directory under the projects root)")
	}
	if name == gitHubHost {
		return "", fmt.Errorf("wing name is the configured [github] host; " +
			"a wing and a host tree cannot be the same directory")
	}
	return name, nil
}

// ResolveWings validates the `[[projects.wings]]` table against the resolved
// GitHub host and builds the owner/name → wing lookup. gitHubHost must already
// be githubauth.ResolveHost output.
//
// Beyond ValidateWingName's two rules it fails closed on two more shapes, each
// of which would otherwise be a silent misplacement: a repeated name (two
// blocks naming one directory with different repos is a last-wins mapping with
// nothing to show for it), and a repo claimed by two wings or written as
// something other than a safe "owner/name" (these steer which tree a clone
// lands in, so an ambiguous entry must not resolve to a coin flip).
func ResolveWings(gitHubHost string, wings []Wing) (WingTable, error) {
	byRepo := make(map[string]string)
	seenWing := make(map[string]bool, len(wings))
	for i, w := range wings {
		name, err := ValidateWingName(gitHubHost, w.Name)
		if err != nil {
			return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d: %w", i+1, err)
		}
		if seenWing[name] {
			return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d repeats an earlier wing name", i+1)
		}
		seenWing[name] = true

		for _, repo := range w.Repos {
			owner, repoName, ok := splitOwnerRepo(strings.TrimSpace(repo))
			if !ok {
				return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d lists a repo that is not a valid \"owner/name\"", i+1)
			}
			key := strings.ToLower(owner + "/" + repoName)
			if prior, dup := byRepo[key]; dup && prior != name {
				return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d claims a repo already claimed by wing %q; "+
					"a repo belongs to at most one wing", i+1, prior)
			}
			byRepo[key] = name
		}
	}
	return WingTable{byRepo: byRepo}, nil
}
