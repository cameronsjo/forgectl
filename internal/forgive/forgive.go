// Package forgive normalizes user input so the CLI shrugs off iOS
// autocorrect noise and fat-fingered aliases. Pure stdlib and
// data-parameterized by design: the alias vocabularies live with their
// modules (internal/cli manifests, ADR-0005); this package owns only the
// normalization mechanics — Normalize plus the Resolver built over a
// module's map.
package forgive

import "strings"

// Normalize cleans a single candidate verb so iOS autocorrect and stray
// punctuation don't cause a miss. It lowercases, trims surrounding
// whitespace, and strips trailing punctuation/space. Examples:
//
//	"LS. "  -> "ls"
//	"Kill," -> "kill"
//	"  Tree " -> "tree"
func Normalize(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	return strings.TrimRight(lower, " \t.,!?;:")
}

// Resolver resolves raw tokens against one module's canonical-verb →
// aliases map (the same shape Manifest.SubAliases and the shared
// applyAliases helper use). It replaces the old package-level Canonical,
// which was hardwired to the tmux vocabulary.
type Resolver struct {
	canonical map[string][]string
	aliasTo   map[string]string
}

// NewResolver builds a Resolver over a canonical-verb → aliases map.
func NewResolver(aliases map[string][]string) *Resolver {
	r := &Resolver{canonical: aliases, aliasTo: make(map[string]string)}
	for canon, as := range aliases {
		for _, a := range as {
			r.aliasTo[a] = canon
		}
	}
	return r
}

// Canonical resolves a raw token (user-supplied, not yet normalized) to its
// canonical verb. It normalizes the token first. If the token is itself a
// canonical verb, it returns (verb, true). If it is a registered alias, it
// returns the canonical verb it maps to, true. Otherwise it returns
// ("", false) — the signal the caller uses to fall through to the TUI.
func (r *Resolver) Canonical(token string) (canonical string, known bool) {
	normalized := Normalize(token)
	if _, isCanonical := r.canonical[normalized]; isCanonical {
		return normalized, true
	}
	if canon, ok := r.aliasTo[normalized]; ok {
		return canon, true
	}
	return "", false
}
