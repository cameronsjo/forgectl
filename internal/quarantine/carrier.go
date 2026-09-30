package quarantine

import (
	"path"
	"path/filepath"
	"strings"
)

// Carrier names the target-list entry a path matched.
type Carrier struct {
	// Target is the entry exactly as written in the target list, for
	// example "CLAUDE.md", ".claude/" or ".*/mcp.json".
	Target string
	// Nestable reports that the entry is one of the nestableBasenames, which
	// an agent reads from any directory rather than only from the workspace
	// root.
	Nestable bool
}

// CarrierMatcher classifies relative paths against a quarantine target list
// with the same predicates quarantine uses to find what to hide. It exists so
// a read-only inventory (`forgectl audit injection`, forgectl#14) can report
// exactly the file classes quarantine defends, from the one list, instead of
// keeping a copy that drifts.
//
// The predicates mirror ExpandTargets:
//   - a literal entry names the path under an anchor directory, compared
//     ASCII-case-insensitively (asciiPathFold), as coveredRootEntries and
//     Hide's Lstat do;
//   - a pattern entry is matched one segment at a time with foldPattern, as
//     globFold does;
//   - a nestable basename matches at any depth, as walkNestable does.
//
// Match classifies; it does not decide where a match is expected. A caller
// anchoring a match somewhere other than a workspace root reads Nestable to
// tell an ordinary nested AGENTS.md from a root-only carrier found out of
// place.
type CarrierMatcher struct {
	rules       []carrierRule
	maxSegments int
}

type carrierRule struct {
	target   string
	segments []string // asciiPathFold'ed literal segments, or foldPattern'ed pattern segments
	pattern  bool
	nestable bool
}

// NewCarrierMatcher builds a matcher over targets. Callers classifying the
// default carrier classes pass DefaultTargets. It returns the same error
// ExpandTargets would for an invalid entry.
func NewCarrierMatcher(targets []string) (*CarrierMatcher, error) {
	m := &CarrierMatcher{}
	for _, raw := range targets {
		rule, err := normalizeTargetRule(raw)
		if err != nil {
			return nil, err
		}
		slashed := filepath.ToSlash(rule.text)
		segs := strings.Split(slashed, "/")
		cr := carrierRule{target: raw, pattern: rule.pattern}
		for _, seg := range segs {
			if rule.pattern {
				cr.segments = append(cr.segments, foldPattern(seg))
			} else {
				cr.segments = append(cr.segments, asciiPathFold(seg))
			}
		}
		cr.nestable = !rule.pattern && len(segs) == 1 && nestableBasenames[path.Clean(slashed)]
		if len(segs) > m.maxSegments {
			m.maxSegments = len(segs)
		}
		m.rules = append(m.rules, cr)
	}
	return m, nil
}

// MaxSegments is the most path segments any entry spans: the longest suffix
// of a path a caller must try against Match.
func (m *CarrierMatcher) MaxSegments() int { return m.maxSegments }

// Match reports the first entry that the path formed by segs names, where
// segs are on-disk names relative to an anchor directory. The first match in
// list order wins.
func (m *CarrierMatcher) Match(segs []string) (Carrier, bool) {
	for _, rule := range m.rules {
		if len(rule.segments) != len(segs) {
			continue
		}
		if rule.matches(segs) {
			return Carrier{Target: rule.target, Nestable: rule.nestable}, true
		}
	}
	return Carrier{}, false
}

func (r carrierRule) matches(segs []string) bool {
	for i, seg := range segs {
		if r.pattern {
			ok, err := filepath.Match(r.segments[i], seg)
			if err != nil || !ok {
				return false
			}
			continue
		}
		if asciiPathFold(seg) != r.segments[i] {
			return false
		}
	}
	return true
}
