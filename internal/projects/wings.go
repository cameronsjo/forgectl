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

// Names returns every configured wing name, for callers that need the
// placement namespace itself rather than a per-repo answer.
func (t WingTable) Names() []string {
	seen := make(map[string]bool, len(t.byRepo))
	out := make([]string, 0, len(t.byRepo))
	for _, wing := range t.byRepo {
		if !seen[wing] {
			seen[wing] = true
			out = append(out, wing)
		}
	}
	return out
}

// ResolveWings validates the `[[projects.wings]]` table against the resolved
// GitHub host and builds the owner/name → wing lookup. gitHubHost must already
// be githubauth.ResolveHost output.
//
// It fails closed on four config shapes, each of which would otherwise be a
// silent misplacement:
//
//  1. A name outside the path-segment charset. The name becomes a directory
//     directly under the projects root; ':' is APFS-legal and renders as '/'
//     in Finder, a leading '.' hides the tree from ls, and a homoglyph makes
//     two wings that look identical.
//  2. A name equal to the GitHub host. The wing directory and the host tree
//     would be the same directory, with different leaf semantics — wing
//     members sit one level down, host-tree clones two.
//  3. A repeated name. Two blocks naming one directory with different repos is
//     a last-wins mapping with nothing to show for it.
//  4. A repo claimed by two wings, or a repo entry that is not a safe
//     "owner/name". These steer which tree a clone lands in, so an ambiguous
//     or malformed entry must not resolve to a coin flip.
func ResolveWings(gitHubHost string, wings []Wing) (WingTable, error) {
	byRepo := make(map[string]string)
	seenWing := make(map[string]bool, len(wings))
	for i, w := range wings {
		name := strings.ToLower(strings.TrimSpace(w.Name))
		if name == "" {
			return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d has no name", i+1)
		}
		if !githubauth.ValidHostSegment(name) {
			// Categorical, like ResolveHost's own rejections: the value is
			// operator config, but it is also about to be a directory name and
			// the error prints to a terminal.
			return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d has a name outside the allowed charset "+
				"(lowercase letters, digits, '.' and '-'; it becomes a directory under the projects root)", i+1)
		}
		if name == gitHubHost {
			return WingTable{}, fmt.Errorf("[[projects.wings]] entry %d is named after the configured [github] host; "+
				"a wing and a host tree cannot be the same directory", i+1)
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
