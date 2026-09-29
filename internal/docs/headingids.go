package docs

import (
	"strconv"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// headingIDs is a parser.IDs that hands out exactly the ids goldmark's
// default collection does, in amortized constant time per id.
//
// goldmark's default resolves a taken slug by probing slug-1, slug-2, …
// from 1 on every call, so the Nth copy of one heading costs N probes and a
// document of N identical headings costs N² — 32k copies of "## a" took
// minutes, under renderMu (cameronsjo/forgectl#596). This collection keeps,
// per base slug, the suffix its next probe starts from. That is safe because
// the used set only ever grows: every suffix below the cursor was taken when
// the cursor moved past it and is still taken, so the first free suffix at
// or above the cursor is the first free suffix at or above 1 — the one
// goldmark's scan would have found. A literal heading or a Put that lands on
// a suffixed id ("## a-1") simply joins the used set, and the probe steps
// over it exactly as goldmark's does.
//
// The base slug itself is goldmark's own: slugOf asks a fresh default
// collection for it, so the slug rules (ASCII-only alphanumerics, the
// "heading"/"id" fallback for an empty slug) cannot drift from the upstream
// version in go.mod.
type headingIDs struct {
	used map[string]struct{}
	next map[string]int
}

// newHeadingIDs returns an empty collection, one per parse: ids are unique
// within a document, never across documents.
func newHeadingIDs() parser.IDs {
	return &headingIDs{used: map[string]struct{}{}, next: map[string]int{}}
}

// newParseContext is the parser.Context every render and scan parses under,
// so both see the same ids through the same collection.
func newParseContext() parser.Context {
	return parser.NewContext(parser.WithIDs(newHeadingIDs()))
}

// slugOf is goldmark's base slug for value: the id an empty default
// collection returns, which is the slug before any collision suffix.
func slugOf(value []byte, kind ast.NodeKind) []byte {
	return parser.NewContext().IDs().Generate(value, kind)
}

func (s *headingIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	base := string(slugOf(value, kind))
	if _, taken := s.used[base]; !taken {
		s.used[base] = struct{}{}
		return []byte(base)
	}
	i := s.next[base]
	if i < 1 {
		i = 1
	}
	for ; ; i++ {
		id := base + "-" + strconv.Itoa(i)
		if _, taken := s.used[id]; !taken {
			s.used[id] = struct{}{}
			s.next[base] = i + 1
			return []byte(id)
		}
	}
}

func (s *headingIDs) Put(value []byte) {
	s.used[string(value)] = struct{}{}
}
