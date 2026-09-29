package docs

import (
	"path"
	"sort"
	"strings"
	"time"
)

// FindingKind is the frozen wire enum for one Check finding. Adding a kind is
// additive (ADR-0008 rule 2); renaming or removing one is a breaking change.
type FindingKind string

const (
	// FindingBrokenLink is a link whose target file does not exist.
	FindingBrokenLink FindingKind = "broken_link"
	// FindingAmbiguousLink is a link whose target matches more than one doc.
	FindingAmbiguousLink FindingKind = "ambiguous_link"
	// FindingBrokenAnchor is a link to a real doc whose #fragment (a heading
	// or ^block id) does not exist in it.
	FindingBrokenAnchor FindingKind = "broken_anchor"
	// FindingOrphan is a doc in a directory root that no other doc links to.
	FindingOrphan FindingKind = "orphan"
	// FindingDeprecated is a doc whose frontmatter says status: deprecated
	// (OKF v0.2 §5.4).
	FindingDeprecated FindingKind = "deprecated"
	// FindingStale is a doc whose frontmatter stale_after instant has passed
	// (OKF v0.2 §5.5). Date-only and offset-less values are ignored.
	FindingStale FindingKind = "stale"
)

// Finding is one problem Check found. Ambiguous candidates are never listed.
type Finding struct {
	Kind FindingKind `json:"kind"`
	// Root is the root label.
	Root string `json:"root"`
	// Path is the source doc's RelPath (the orphan's own for an orphan).
	Path string `json:"path"`
	// Target is the link's target as authored; link kinds only.
	Target string `json:"target,omitempty"`
	// Line is the 1-based line of the link in the source file as written,
	// frontmatter included; link kinds only. Additive (ADR-0008 rule 2).
	Line int `json:"line,omitempty"`
	// StaleAfter is the passed stale_after value as authored; stale only.
	StaleAfter string `json:"stale_after,omitempty"`
}

// CheckedRoot reports how one root fared. It carries no absolute path, so a
// report is safe to paste into a transcript.
type CheckedRoot struct {
	Label   string `json:"label"`
	Kind    string `json:"kind"` // "docs" | "vault"
	Checked bool   `json:"checked"`
	Skipped string `json:"skipped,omitempty"`
	Docs    int    `json:"docs"`
}

// CheckSummary counts findings by kind, plus the out-of-root links Check
// deliberately did not report.
type CheckSummary struct {
	BrokenLinks      int `json:"broken_links"`
	AmbiguousLinks   int `json:"ambiguous_links"`
	BrokenAnchors    int `json:"broken_anchors"`
	Orphans          int `json:"orphans"`
	OutsideRootLinks int `json:"outside_root_links"`
	Deprecated       int `json:"deprecated"`
	Stale            int `json:"stale"`
}

// CheckReport is the wire shape of `forgectl docs check --json`.
type CheckReport struct {
	SchemaVersion int           `json:"schema_version"`
	Roots         []CheckedRoot `json:"roots"`
	Findings      []Finding     `json:"findings"`
	Summary       CheckSummary  `json:"summary"`
}

const vaultSkipReason = "vault roots are not checked yet"

// Check reports broken links, ambiguous links, broken anchors, orphan pages,
// and deprecated or stale docs across every docs-kind root. Vault roots are
// skipped. Links that leave their root are counted, not reported: they work
// on GitHub.
//
// It resolves through resolveParts, never ResolveLink, so a path containing a
// literal '#' (authored "%23") is not re-split — the same reason
// buildBacklinks calls resolveParts.
func (idx *Index) Check() CheckReport {
	return idx.CheckAt(trustNow())
}

// CheckAt is Check with the staleness clock pinned to now.
func (idx *Index) CheckAt(now time.Time) CheckReport {
	report := CheckReport{
		SchemaVersion: 1,
		Roots:         make([]CheckedRoot, 0, len(idx.roots)),
		Findings:      []Finding{},
	}
	rootOrder := make(map[string]int, len(idx.roots))
	rootByLabel := make(map[string]Root, len(idx.roots))
	docCounts := make(map[string]int, len(idx.roots))
	for i, r := range idx.roots {
		rootOrder[r.Label] = i
		rootByLabel[r.Label] = r
	}
	for i := range idx.docs {
		docCounts[idx.docs[i].RootLabel]++
	}
	for _, r := range idx.roots {
		cr := CheckedRoot{Label: r.Label, Kind: "docs", Checked: true, Docs: docCounts[r.Label]}
		if r.Kind == RootVault {
			cr.Kind = "vault"
			cr.Checked = false
			cr.Skipped = vaultSkipReason
		}
		report.Roots = append(report.Roots, cr)
	}

	dirInbound := idx.dirLinkInbound(rootByLabel)

	for i := range idx.docs {
		from := &idx.docs[i]
		root := rootByLabel[from.RootLabel]
		if root.Kind == RootVault {
			continue
		}
		for _, l := range from.Links {
			target, miss := idx.resolveParts(from, l.Path, l.Fragment, nil)
			var kind FindingKind
			switch miss {
			case MissNone:
				continue
			case MissOutsideRoot:
				report.Summary.OutsideRootLinks++
				continue
			case MissAmbiguous:
				kind = FindingAmbiguousLink
			case MissNoTarget:
				switch {
				case target != nil:
					kind = FindingBrokenAnchor
				case existsInRoot(root, from, l.Path):
					// A directory or non-markdown file: real, just not a doc.
					continue
				default:
					kind = FindingBrokenLink
				}
			default:
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Kind: kind, Root: from.RootLabel, Path: from.RelPath, Target: l.Raw, Line: l.Line,
			})
		}
		if root.OnlyFile == "" && len(idx.Backlinks(from)) == 0 && !dirInbound[i] && !isRootIndex(from.RelPath) {
			report.Findings = append(report.Findings, Finding{
				Kind: FindingOrphan, Root: from.RootLabel, Path: from.RelPath,
			})
		}
		tr := evalTrust(from.Status, from.StaleAfter, now)
		if tr.Deprecated {
			report.Findings = append(report.Findings, Finding{
				Kind: FindingDeprecated, Root: from.RootLabel, Path: from.RelPath,
			})
		}
		if tr.Stale {
			report.Findings = append(report.Findings, Finding{
				Kind: FindingStale, Root: from.RootLabel, Path: from.RelPath, StaleAfter: tr.StaleAfter,
			})
		}
	}

	sort.SliceStable(report.Findings, func(a, b int) bool {
		fa, fb := report.Findings[a], report.Findings[b]
		if oa, ob := rootOrder[fa.Root], rootOrder[fb.Root]; oa != ob {
			return oa < ob
		}
		if fa.Path != fb.Path {
			return fa.Path < fb.Path
		}
		// Within one file, link findings come first in source order (line,
		// then target, then kind) so path:line output reads top to bottom;
		// the lineless orphan, deprecated and stale findings follow by kind.
		la, lb := isLinkFinding(fa.Kind), isLinkFinding(fb.Kind)
		if la != lb {
			return la
		}
		if la {
			if fa.Line != fb.Line {
				return fa.Line < fb.Line
			}
			if fa.Target != fb.Target {
				return fa.Target < fb.Target
			}
		}
		return fa.Kind < fb.Kind
	})

	for _, f := range report.Findings {
		switch f.Kind {
		case FindingBrokenLink:
			report.Summary.BrokenLinks++
		case FindingAmbiguousLink:
			report.Summary.AmbiguousLinks++
		case FindingBrokenAnchor:
			report.Summary.BrokenAnchors++
		case FindingOrphan:
			report.Summary.Orphans++
		case FindingDeprecated:
			report.Summary.Deprecated++
		case FindingStale:
			report.Summary.Stale++
		}
	}
	return report
}

// isLinkFinding reports whether k is a link kind, the findings that carry a
// Target and Line.
func isLinkFinding(k FindingKind) bool {
	return k == FindingBrokenLink || k == FindingAmbiguousLink || k == FindingBrokenAnchor
}

// existsInRoot reports whether the link path names something on disk inside
// root, for targets that resolve to no indexed doc (a directory, a LICENSE, an
// image). The clean path is built the way resolveDocsDoc builds it. It reads
// nothing; ResolveInRoot refuses a symlink that escapes the root.
func existsInRoot(root Root, from *Doc, linkPath string) bool {
	clean, ok := linkTargetPath(from, linkPath)
	if !ok {
		return false
	}
	_, err := ResolveInRoot(root.Path, clean)
	return err == nil
}

// linkTargetPath is the root-relative clean path a link names, built the way
// resolveDocsDoc builds it; ok is false for an empty path or one that leaves
// the root.
func linkTargetPath(from *Doc, linkPath string) (string, bool) {
	if linkPath == "" {
		return "", false
	}
	var clean string
	if path.IsAbs(linkPath) {
		clean = strings.TrimPrefix(path.Clean(linkPath), "/")
	} else {
		clean = path.Clean(path.Join(path.Dir(from.RelPath), linkPath))
	}
	if escapesRoot(clean) {
		return "", false
	}
	return clean, true
}

// dirLinkInbound marks, by doc index, each README or index page that a
// directory link from another doc in its root reaches: "[x](sub/)" counts as
// inbound to sub/README.md, sub/index.md, or any case-insensitive readme or
// index doc directly in sub/, the page GitHub shows for that directory.
//
// It lives here, not in buildBacklinks, because Backlinks promises to agree
// with ResolveLink, and ResolveLink resolves a directory link to no doc: the
// reader does not open sub/README.md for it. Only orphan detection counts it.
// Vault roots are skipped, as Check skips them.
func (idx *Index) dirLinkInbound(rootByLabel map[string]Root) map[int]bool {
	type dirKey struct{ root, dir string }
	pages := map[dirKey][]int{}
	for i := range idx.docs {
		d := &idx.docs[i]
		if rootByLabel[d.RootLabel].Kind == RootVault || !isIndexName(path.Base(d.RelPath)) {
			continue
		}
		k := dirKey{d.RootLabel, path.Dir(d.RelPath)}
		pages[k] = append(pages[k], i)
	}
	out := map[int]bool{}
	if len(pages) == 0 {
		return out
	}
	for i := range idx.docs {
		from := &idx.docs[i]
		if rootByLabel[from.RootLabel].Kind == RootVault {
			continue
		}
		for _, l := range from.Links {
			if l.Form != FormRelPath {
				continue
			}
			clean, ok := linkTargetPath(from, l.Path)
			if !ok {
				continue
			}
			hits := pages[dirKey{from.RootLabel, clean}]
			if len(hits) == 0 {
				continue
			}
			// A path that resolves to a doc is a link to that doc, not to
			// the directory.
			if target, miss := idx.resolveParts(from, l.Path, l.Fragment, nil); target != nil || miss != MissNoTarget {
				continue
			}
			for _, j := range hits {
				if j != i {
					out[j] = true
				}
			}
		}
	}
	return out
}

// isRootIndex reports whether relPath is a root-level README or index page:
// the entry point of a tree, which nothing is expected to link to.
func isRootIndex(relPath string) bool {
	return !strings.Contains(relPath, "/") && isIndexName(relPath)
}

// isIndexName reports whether a file name is a README or index page, ignoring
// case and extension.
func isIndexName(name string) bool {
	base := strings.ToLower(name)
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		base = base[:dot]
	}
	return base == "readme" || base == "index"
}
