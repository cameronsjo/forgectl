// Package quarantine is the ops layer for reversibly hiding AI-instruction
// files (CLAUDE.md, AGENTS.md, …) from a workspace via os.Rename — distinct
// from workflow's `strip` step, which destructively os.RemoveAll's them for a
// throwaway clean-room sandbox. Quarantine is meant to be undone: Hide moves a
// target aside, Restore moves it back.
package quarantine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Scheme selects how a quarantined path is renamed.
type Scheme int

const (
	// PrefixUnderscore renames CLAUDE.md -> _CLAUDE.md.
	PrefixUnderscore Scheme = iota
	// SuffixQuarantined renames CLAUDE.md -> CLAUDE.md.quarantined.
	SuffixQuarantined
)

// String renders scheme as the CLI flag value that selects it.
func (s Scheme) String() string {
	switch s {
	case SuffixQuarantined:
		return "suffix"
	default:
		return "prefix"
	}
}

// ParseScheme parses a `--scheme` flag value ("prefix" or "suffix").
func ParseScheme(s string) (Scheme, error) {
	switch s {
	case "", "prefix":
		return PrefixUnderscore, nil
	case "suffix":
		return SuffixQuarantined, nil
	default:
		return PrefixUnderscore, fmt.Errorf("unknown quarantine scheme %q (want prefix or suffix)", s)
	}
}

// Move records one reversible rename: From is the original path, To is the
// quarantined path. Restore reverses it.
type Move struct {
	From string
	To   string
}

// tier1Carriers are the AI-config carriers the harness forgectl actually
// dispatches into a review workspace READS. `claude -p` (internal/pr/launch.go
// launchInline) is the only harness that ever sees a remote, third-party PR
// head: Codex is refused for remote heads by CheckAgentForRef, and the
// escalation path is unwired. A gap in this tier is exploitable by
// `forgectl pr <url>` today.
//
// These are prose/state carriers with no shared directory or naming
// convention, so the tier is necessarily a denylist. The MCP class, which does
// share a shape, is handled by mcpConfigPatterns instead.
var tier1Carriers = []string{
	"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".claude/",
	// MEASURED (Claude Code 2.1.220): a root .mcp.json planted in a review
	// workspace made the harness spawn the PR author's `command` + `args` at
	// session START — before the agent invoked any tool, with `--permission-mode
	// plan` and the deny-by-default workspace allowlist both in force. Those
	// controls govern which TOOLS the agent may call; neither sits upstream of a
	// process spawned at MCP registration. That is remote-content-to-local-code-
	// execution on the reviewer's host, strictly worse than the prompt injection
	// the clean room was built for.
	//
	// Do NOT drop this as redundant with --strict-mcp-config (Profile.StrictMCP
	// in internal/launch): the two are deliberate belt and suspenders at
	// different layers — quarantine fails on a path nobody enumerated, the flag
	// fails if a refactor drops it.
	".mcp.json",
}

// tier2Carriers are defense in depth, NOT an active hole. forgectl launches
// none of these assistants, and each of these paths was measured NOT read by
// `claude -p` (2.1.220). They matter only when a human separately opens the
// review workspace in one of these editors — a real threat model, but one the
// human opted into. Cheap to cover; do not describe as closing a live gap.
//
// `.cursor` is the whole directory rather than `.cursor/rules` because it is
// tool state rather than source: a Cursor user opening the workspace picks up
// everything under it, not only `rules`.
//
// `.github/copilot-instructions.md` stays a single FILE. Widening it to
// `.github/` would hide the CI workflows, and a hostile workflow change is
// precisely what a reviewer must be able to see — a security regression
// wearing a security fix's clothes.
var tier2Carriers = []string{
	".cursor", ".cursorrules", ".windsurfrules", ".continuerules",
	".aider.conf.yml", ".codex.toml", "mcp.json",
	".github/copilot-instructions.md",
}

// mcpConfigPatterns is the pattern rule for the MCP class. Unlike the prose
// carriers above, MCP configuration has a consistent shape across tools:
// machine-read JSON carrying an execution primitive, clustered at the
// workspace root or directly inside a root-level dot-directory. Matching the
// POSITION rather than enumerating vendors covers `.cursor/mcp.json`,
// `.gemini/mcp.json`, `.windsurf/mcp.json` and the next tool's equivalent with
// no code change — durability exactly where staleness is most expensive, since
// a missed MCP carrier is an execution primitive rather than a paragraph of
// text.
//
// Bounded on purpose: one level deep, dot-directories only. A nested
// `sub/.mcp.json` was measured NOT read (the harness discovers project MCP
// config at the workspace root), so a full-tree walk would add reversibility
// risk to defend a hole that does not exist.
//
// Root-level `mcp.json` / `.mcp.json` are literal entries above, not patterns,
// because they carry no directory component.
var mcpConfigPatterns = []string{".*/mcp.json", ".*/.mcp.json"}

// DefaultTargets is the canonical list of AI-config paths (relative to a
// workspace root) that quarantine hides by default, and that workflow's
// `strip` step falls back to when a step omits `globs`.
//
// It is the concatenation of the tier slices and the pattern rule, never an
// independently maintained literal — TestDefaultTargets_TierSumIsComplete
// fails loudly if a tier is added without being wired in here.
//
// Entries are literal relative paths, EXCEPT the mcpConfigPatterns entries,
// which carry glob metacharacters. ExpandTargets replaces those with their
// concrete matches; `strip` resolves them natively through filepath.Glob. The
// nestable basenames — see nestableBasenames — are expanded to every nested
// match by ExpandTargets; the remaining literals are root-level only, which is
// where they are actually read from.
//
// No literal entry may be a path prefix of another — a directory entry that
// would swallow an existing entry must REPLACE it, never join it. Widening
// `.cursor/rules` to `.cursor` means deleting `.cursor/rules` in the same
// edit. TestDefaultTargets_NoEntryIsPathPrefixOfAnother enforces this, because
// both orders are wrong and one of them is silent:
//
//   - inner first (`.cursor/rules` then `.cursor`) strands the tree
//     permanently. Hide leaves `.cursor.quarantined/rules.quarantined/`, and
//     Restore only undoes the outer rename, so `.cursor/rules.quarantined/`
//     survives — both calls returning nil.
//   - outer first round-trips cleanly, but the inner entry is then dead config
//     that merely looks like coverage: it can never match anything, because
//     the outer rename has already moved it out from under its own path.
var DefaultTargets = concatTargets(tier1Carriers, tier2Carriers, mcpConfigPatterns)

// concatTargets flattens the tier groups into one list. A plain helper rather
// than a hand-written literal so a new tier cannot be declared and silently
// left unwired.
func concatTargets(groups ...[]string) []string {
	n := 0
	for _, g := range groups {
		n += len(g)
	}
	out := make([]string, 0, n)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// pathPrefixPairs reports every {outer, inner} pair in targets where the outer
// entry is a directory that contains the inner one. Such a pair is banned in
// DefaultTargets: see that list's doc comment for the failure it causes.
//
// Pattern entries are skipped — a glob is not a path, and the overlap between
// a pattern match and a literal directory is already handled by
// coveredRootEntries. Both sides are cleaned first because `.claude/` and
// `.claude` are the same entry spelled two ways. The separator appended to the
// outer entry is load-bearing: without it `.cursor` would flag `.cursorrules`,
// which is a sibling file, not a child.
func pathPrefixPairs(targets []string) [][2]string {
	var pairs [][2]string
	for i, a := range targets {
		if isPattern(a) {
			continue
		}
		outer := filepath.Clean(a)
		for j, b := range targets {
			if i == j || isPattern(b) {
				continue
			}
			if inner := filepath.Clean(b); strings.HasPrefix(inner, outer+string(filepath.Separator)) {
				pairs = append(pairs, [2]string{outer, inner})
			}
		}
	}
	return pairs
}

// nestableBasenames are the instruction-file basenames a coding agent picks up
// from ANY directory as it descends, not just the workspace root. Quarantining
// only the root pair leaves a PR head carrying `src/AGENTS.md` or
// `packages/api/CLAUDE.md` free to inject instructions into the review — the
// exact thing the clean room exists to prevent.
var nestableBasenames = map[string]bool{
	"CLAUDE.md":       true,
	"CLAUDE.local.md": true,
	"AGENTS.md":       true,
}

// skipDirNames are directories ExpandTargets never descends into. `.git` holds
// no instruction file an agent reads, and walking a large object store on every
// hide/restore/status is pure cost.
var skipDirNames = map[string]bool{
	".git": true,
}

type targetRule struct {
	text    string
	pattern bool
}

type pathIdentity struct {
	exact string
	fold  string
}

type rootIdentity struct {
	absolute string
	real     string
}

type concreteTarget struct {
	display             string
	logical             string
	destination         string
	sourceIdentity      pathIdentity
	destinationIdentity pathIdentity
	sourceLexical       pathIdentity
	destinationLexical  pathIdentity
	move                Move
}

type graphConflict struct {
	kind       int
	first      string
	second     string
	diagnostic string
}

// ExpandTargets resolves targets against root, replacing each bare nestable
// basename ("CLAUDE.md", "AGENTS.md") with every match found by walking root,
// and each glob-shaped entry (see mcpConfigPatterns) with its concrete
// matches. Other entries — and any literal entry carrying a directory
// component, which is an explicit path the caller meant literally — pass
// through untouched.
//
// A pattern entry is REPLACED, not preserved: Hide resolves targets with
// os.Lstat, so a literal "…/*.json" would only ever be a silent no-op. Every
// Hide call site reaches Hide through this function, so a pattern never
// arrives unexpanded.
//
// It is direction-agnostic BY DESIGN, and that is what keeps quarantine
// reversible. A match is recorded when the walk finds either the original
// basename or its renamed form under scheme, and the returned path is always
// the ORIGINAL relative path. So the same call produces the same target list
// before Hide (tree holds `src/AGENTS.md`) and at teardown (tree holds
// `src/AGENTS.md.quarantined`), and ComputeMoves reproduces exactly the Move
// pairs Hide made.
//
// Each literal entry is preserved even with zero nested matches, so a caller
// that pairs targets with moves by index (e.g. `quarantine status`) keeps a row
// for a target that is simply absent.
//
// A walk error is not fatal: an unreadable subtree yields no matches rather
// than failing the whole operation, which would strand a workspace mid-review.
// Errors reaching root itself are returned.
func ExpandTargets(root string, scheme Scheme, targets []string) ([]string, error) {
	rules, err := normalizeTargetRules(targets)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := resolveRootIdentity(root)
	if err != nil {
		return nil, err
	}
	root = canonicalRoot.absolute

	literals := make([]string, 0, len(rules))
	var patterns []string
	for _, rule := range rules {
		if rule.pattern {
			patterns = append(patterns, rule.text)
			continue
		}
		literals = append(literals, rule.text)
	}

	covered, err := coveredRootEntries(root, scheme, literals)
	if err != nil {
		return nil, err
	}
	var nested []string
	if wantsExpansion(literals) {
		if nested, err = walkNestable(root, scheme, literals, covered); err != nil {
			return nil, err
		}
	}

	var matched []string
	if len(patterns) > 0 {
		coveredRoots := canonicalCoveredRoots(root, covered)
		if matched, err = expandPatterns(root, canonicalRoot.real, scheme, patterns, covered, coveredRoots); err != nil {
			return nil, err
		}
	}

	all := append(append(append([]string{}, literals...), nested...), matched...)
	prepared, err := prepareMoves(root, scheme, all)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(prepared))
	for _, target := range prepared {
		out = append(out, target.logical)
	}
	return out, nil
}

// isPattern reports whether an entry carries glob metacharacters, i.e. is a
// pattern rule rather than a literal path.
func isPattern(target string) bool {
	return strings.ContainsAny(target, "*?[")
}

// coveredRootEntries is the set of root-level (single-segment) literal
// targets. A pattern match UNDER one of them must be dropped, and that is not
// cosmetic de-duplication — it is what keeps quarantine reversible.
//
// `.cursor` is hidden as a directory unit, so once Hide has run, a match at
// `.cursor/mcp.json` no longer exists under any spelling and the post-Hide
// ExpandTargets would return a SHORTER list than the pre-Hide one, breaking
// the property teardown depends on. Worse, if the nested entry were hidden
// first, Restore would step over it (its quarantined path now lives inside the
// renamed directory) and leave a `.quarantined` file behind permanently.
func coveredRootEntries(root string, scheme Scheme, targets []string) (map[string]string, error) {
	covered := make(map[string]string, len(targets)*2)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read quarantine root: %w", err)
	}
	for _, t := range targets {
		clean := filepath.Clean(t)
		if clean == "." || strings.ContainsRune(clean, filepath.Separator) {
			continue
		}
		original, err := uniqueFoldedEntry(entries, clean)
		if err != nil {
			return nil, err
		}
		decorated, err := uniqueFoldedEntry(entries, renamedPath(scheme, clean))
		if err != nil {
			return nil, err
		}
		var accepted string
		switch {
		case original != nil && original.IsDir():
			accepted = original.Name()
		case decorated != nil && decorated.IsDir():
			accepted = undecorate(scheme, decorated.Name())
		default:
			continue
		}
		for _, spelling := range []string{accepted, renamedPath(scheme, accepted)} {
			key := asciiPathFold(spelling)
			if prior, exists := covered[key]; exists && prior != accepted {
				pair := []string{prior, accepted}
				sort.Strings(pair)
				return nil, fmt.Errorf("quarantine targets %q and %q identify the same covered root", pair[0], pair[1])
			}
			covered[key] = accepted
		}
	}
	return covered, nil
}

func uniqueFoldedEntry(entries []fs.DirEntry, wanted string) (fs.DirEntry, error) {
	key := asciiPathFold(wanted)
	var matches []fs.DirEntry
	for _, entry := range entries {
		if asciiPathFold(entry.Name()) == key {
			matches = append(matches, entry)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("quarantine target %q has ambiguous case-fold spellings", wanted)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return matches[0], nil
}

// expandPatterns resolves each glob pattern to the ORIGINAL relative paths it
// matches, dropping matches under a directory that is already quarantined
// wholesale (covered) or never descended into (skipDirNames).
//
// Like walkNestable it is direction-agnostic: each pattern is globbed in both
// its plain and its scheme-renamed form, and the result is undecorated back to
// the pre-quarantine spelling. That is what makes the same call reproduce the
// same target list before Hide and at teardown.
// It is also case-insensitive, via globFold, for the same reason walkNestable
// lowercases: the attacker writes the filename, and on APFS a tool opening
// `.gemini/mcp.json` resolves to a planted `.gemini/MCP.json` that a
// case-sensitive filepath.Match never sees.
func expandPatterns(root, realRoot string, scheme Scheme, patterns []string, covered map[string]string, coveredRoots []string) ([]string, error) {
	var found []string
	seen := make(map[string]bool)
	for _, p := range patterns {
		for _, form := range []string{filepath.Clean(p), renamedPath(scheme, filepath.Clean(p))} {
			matches, err := globFold(root, form)
			if err != nil {
				return nil, fmt.Errorf("expand quarantine pattern %q: %w", p, err)
			}
			for _, m := range matches {
				rel, relErr := filepath.Rel(root, m)
				if relErr != nil {
					continue
				}
				dir, base := filepath.Split(rel)
				// The coverage check is itself direction-agnostic. Against an
				// already-hidden tree the leading segment is the RENAMED directory
				// (`.cursor.quarantined`), which no longer matches the literal target
				// it came from — so the post-Hide call would report a nested entry the
				// pre-Hide call did not, and the two lists would disagree.
				top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
				undecorated := undecorate(scheme, top)
				if _, ok := covered[asciiPathFold(top)]; ok {
					continue
				}
				// A unique in-root symlinked directory is a real carrier namespace:
				// quarantine through its logical alias so the move round-trips. Only
				// suppress an alias when its canonical referent is already owned by a
				// covered root-level target. Escaping aliases deliberately continue to
				// prepareMoves, whose confinement check fails closed.
				if aliasesCoveredRoot(root, realRoot, top, coveredRoots) {
					continue
				}
				if skipDirNames[top] || skipDirNames[undecorated] {
					continue
				}
				orig := filepath.Join(dir, undecorate(scheme, base))
				if seen[orig] {
					continue
				}
				seen[orig] = true
				found = append(found, orig)
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

func aliasesCoveredRoot(root, realRoot, top string, coveredRoots []string) bool {
	info, err := os.Lstat(filepath.Join(root, top))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	alias, err := filepath.EvalSymlinks(filepath.Join(root, top))
	if err != nil {
		return false
	}
	alias, err = filepath.Abs(alias)
	if err != nil || !canonicalOwns(realRoot, alias) {
		return false
	}
	for _, coveredPath := range coveredRoots {
		if canonicalOwns(coveredPath, alias) {
			return true
		}
	}
	return false
}

// canonicalCoveredRoots snapshots covered-root ownership once per
// ExpandTargets call. The cache is deliberately invocation-local: expansion
// performs no mutation, while Hide separately revalidates the resulting move
// set immediately before its mutation phase.
func canonicalCoveredRoots(root string, covered map[string]string) []string {
	seen := make(map[string]bool)
	var roots []string
	for spelling := range covered {
		actual, complete, resolveErr := resolveExistingSpelling(root, spelling)
		if resolveErr != nil || !complete {
			continue
		}
		coveredPath, resolveErr := filepath.EvalSymlinks(filepath.Join(root, actual))
		if resolveErr != nil {
			continue
		}
		coveredPath, resolveErr = filepath.Abs(coveredPath)
		if resolveErr != nil || seen[coveredPath] {
			continue
		}
		seen[coveredPath] = true
		roots = append(roots, coveredPath)
	}
	sort.Strings(roots)
	return roots
}

// canonicalOwns compares canonical filesystem locations, whose case policy is
// already embodied by the filesystem resolution that produced them. Unlike
// logical diagnostic identities, absolute ownership must never ASCII-fold:
// Linux can have distinct case-only sibling trees. filepath.Rel also enforces
// a path-segment boundary, so ".claude-other" is not owned by ".claude".
func canonicalOwns(owner, candidate string) bool {
	return pathWithin(filepath.Clean(owner), filepath.Clean(candidate))
}

// globFold is filepath.Glob's contract — resolve a relative pattern against
// root, return the concrete paths it matches — with one difference: every path
// segment's LITERAL characters are matched case-insensitively (see foldPattern
// for why it is the literals and not the whole segment).
//
// This is walkNestable's `want` mechanism generalized from a basename set to a
// pattern, and it exists for the identical reason. filepath.Glob matches with
// filepath.Match, which is case-sensitive; APFS and NTFS are not. So a
// case-sensitive match is not the predicate the agent uses, and in the pattern
// rule's case the attacker picks the filename: a head carrying
// `.gemini/MCP.json` survives a `.*/mcp.json` glob untouched, while the
// reviewer's open(".gemini/mcp.json") resolves straight to it. Matching
// case-insensitively costs nothing on a case-sensitive filesystem — a
// genuinely distinct `MCP.json` is also quarantined, which is the safe
// direction — and closes the vector on the filesystems forgectl runs on.
//
// Resolution is by os.ReadDir per segment rather than by Lstat, so the
// returned paths carry the ACTUAL on-disk spelling; callers undecorate them
// back to the original relative path. A segment that is not a readable
// directory contributes no matches rather than failing the whole operation,
// mirroring walkNestable's treatment of an unreadable subtree. ".." can never
// appear in a result, because segments are matched against directory entries
// and never traversed upward.
//
// A pattern that resolves to root ITSELF yields no matches rather than root.
// globFold is the single resolver for the destructive `strip` half of the
// control, and handing a caller the workspace root is handing os.RemoveAll the
// whole workspace. validateStripGlob rejects that shape up front with an
// operator-facing error; this is the resolver refusing to produce it at all.
func globFold(root, pattern string) ([]string, error) {
	clean := filepath.Clean(pattern)
	if clean == "." || clean == string(filepath.Separator) {
		return nil, nil
	}
	current := []string{root}
	for _, seg := range strings.Split(filepath.ToSlash(clean), "/") {
		if seg == "" {
			continue
		}
		folded := foldPattern(seg)
		var next []string
		for _, dir := range current {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue // not a directory, or unreadable: no matches from here
			}
			for _, e := range entries {
				ok, matchErr := filepath.Match(folded, e.Name())
				if matchErr != nil {
					return nil, fmt.Errorf("match pattern %q: %w", pattern, matchErr)
				}
				if ok {
					next = append(next, filepath.Join(dir, e.Name()))
				}
			}
		}
		current = next
		if len(current) == 0 {
			return nil, nil
		}
	}
	sort.Strings(current)
	return current, nil
}

// foldPattern rewrites one filepath.Match segment pattern so its LITERAL
// characters match either case, while leaving character-class bodies exactly
// as the author wrote them. The result is matched against the entry's real
// on-disk name — nothing on the name side is folded.
//
// The obvious implementation is to lowercase the pattern and lowercase the
// name, and it is wrong, because a class body is not a literal. `[^a-z]foo`
// matches `Afoo` before folding and matches NOTHING after it: the name's `A`
// folds to `a`, which the class excludes. Lowercasing the segment therefore
// silently changes what a valid pattern means, and in the direction that
// drops matches — the unsafe direction for a quarantine control.
//
// Rewriting each cased literal `x` as the two-member class `[xX]` instead
// keeps every class predicate byte-for-byte against the unmodified name, so a
// class pattern matches exactly what filepath.Match matched before folding,
// while `mcp.json` still finds `MCP.json`. An `\` escape is copied with the
// character it escapes, since wrapping that character in a class would change
// what the backslash means. A malformed class is copied verbatim so
// filepath.Match still reports ErrBadPattern rather than being papered over.
func foldPattern(seg string) string {
	runes := []rune(seg)
	var b strings.Builder
	b.Grow(len(seg) * 2)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '\\':
			b.WriteRune(c)
			if i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
			}
		case '[':
			// Mirror filepath.Match's own scan to find the class's end: an
			// optional leading '^', then a ']' in first position is a MEMBER
			// rather than the terminator, then items (with '\' escaping) until
			// the closing ']'.
			j := i + 1
			if j < len(runes) && runes[j] == '^' {
				j++
			}
			if j < len(runes) && runes[j] == ']' {
				j++
			}
			for j < len(runes) && runes[j] != ']' {
				if runes[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(runes) {
				j++ // consume the closing ']'
			}
			b.WriteString(string(runes[i:j]))
			i = j - 1
		default:
			lower, upper := unicode.ToLower(c), unicode.ToUpper(c)
			if lower == upper {
				b.WriteRune(c) // uncased, or a metacharacter: '*', '?', '.', '/'
				continue
			}
			b.WriteRune('[')
			b.WriteRune(lower)
			b.WriteRune(upper)
			b.WriteRune(']')
		}
	}
	return b.String()
}

// wantsExpansion reports whether targets contains at least one bare nestable
// basename — the only case that needs a walk at all.
func wantsExpansion(targets []string) bool {
	for _, t := range targets {
		if nestableBasenames[t] {
			return true
		}
	}
	return false
}

// walkNestable returns the original relative paths of every NESTED match for
// the nestable basenames present in targets. Root-level hits are excluded:
// they are already in targets verbatim.
func walkNestable(root string, scheme Scheme, targets []string, covered map[string]string) ([]string, error) {
	// Keyed on the ASCII-folded basename. APFS and NTFS are case-insensitive by
	// default, so an exact-string match is not the same predicate the agent
	// uses: a head carrying `src/agents.md` is missed by an exact walk, while
	// the reviewer's open("src/AGENTS.md") resolves to it and reads the
	// instructions. Matching case-insensitively everywhere costs nothing on a
	// case-sensitive filesystem (a genuinely distinct `agents.md` is also
	// quarantined, which is the safe direction) and closes the vector on the
	// filesystems forgectl actually runs on.
	//
	// The root level was already covered by accident — Hide's os.Lstat matches
	// case-insensitively — so only this walk was exposed.
	want := make(map[string]bool, len(targets)*2) // lowercased on-disk names to match
	for _, t := range targets {
		if !nestableBasenames[t] {
			continue
		}
		want[asciiPathFold(t)] = true
		want[asciiPathFold(filepath.Base(renamedPath(scheme, t)))] = true
	}

	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable subtree: skip it, don't fail the operation
		}
		if d.IsDir() {
			if path != root {
				rel, relErr := filepath.Rel(root, path)
				if relErr == nil && !strings.ContainsRune(rel, filepath.Separator) {
					if _, ok := covered[asciiPathFold(d.Name())]; ok {
						return filepath.SkipDir
					}
				}
				if skipDirNames[d.Name()] {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !want[asciiPathFold(d.Name())] {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			return nil // root-level hit: already carried literally in targets
		}
		// Return the ACTUAL on-disk spelling, not the canonical target
		// basename. Emitting "AGENTS.md" for a file named "agents.md" would
		// rename it to AGENTS.md.quarantined on a case-insensitive filesystem
		// (silently changing its case across the round-trip) and miss it
		// entirely on a case-sensitive one.
		found = append(found, filepath.Join(dir, undecorate(scheme, d.Name())))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s for nested instruction files: %w", root, err)
	}
	sort.Strings(found)
	return found, nil
}

func normalizeTargetRules(raw []string) ([]targetRule, error) {
	out := make([]targetRule, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	var invalid []string
	for _, target := range raw {
		rule, err := normalizeTargetRule(target)
		if err != nil {
			invalid = append(invalid, err.Error())
			continue
		}
		if seen[rule.text] {
			continue
		}
		seen[rule.text] = true
		out = append(out, rule)
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, errors.New(invalid[0])
	}
	return out, nil
}

func normalizeTargetRule(raw string) (targetRule, error) {
	if raw == "" {
		return targetRule{}, errors.New("quarantine target must not be empty")
	}
	portable := strings.ReplaceAll(raw, "\\", "/")
	if filepath.IsAbs(raw) || strings.HasPrefix(portable, "/") {
		return targetRule{}, fmt.Errorf("quarantine target %q must not be absolute", raw)
	}
	if filepath.VolumeName(raw) != "" || hasPortableVolume(portable) {
		return targetRule{}, fmt.Errorf("quarantine target %q must not be volume-qualified", raw)
	}
	for _, segment := range strings.Split(portable, "/") {
		if segment == ".." {
			return targetRule{}, fmt.Errorf("quarantine target %q must not traverse outside root", raw)
		}
	}
	clean := path.Clean(portable)
	if clean == "." || clean == "/" {
		return targetRule{}, fmt.Errorf("quarantine target %q must not name root", raw)
	}
	pattern := isPattern(clean)
	if pattern {
		if _, err := path.Match(clean, ""); err != nil {
			return targetRule{}, fmt.Errorf("quarantine target %q has invalid pattern: %w", raw, err)
		}
	}
	return targetRule{text: filepath.FromSlash(clean), pattern: pattern}, nil
}

func hasPortableVolume(target string) bool {
	return len(target) >= 2 && ((target[0] >= 'a' && target[0] <= 'z') || (target[0] >= 'A' && target[0] <= 'Z')) && target[1] == ':'
}

func asciiPathFold(value string) string {
	value = filepath.ToSlash(value)
	var folded strings.Builder
	folded.Grow(len(value))
	for _, char := range value {
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		folded.WriteRune(char)
	}
	return folded.String()
}

func normalizeConcreteTargets(root string, scheme Scheme, raw []string) ([]concreteTarget, error) {
	rules, err := normalizeTargetRules(raw)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := resolveRootIdentity(root)
	if err != nil {
		return nil, err
	}
	root = canonicalRoot.absolute
	targets := make([]concreteTarget, 0, len(rules))
	for _, rule := range rules {
		if rule.pattern {
			return nil, fmt.Errorf("quarantine target %q is a pattern; expand it before computing moves", filepath.ToSlash(rule.text))
		}
		logical, err := resolveConcreteSpelling(root, scheme, rule.text)
		if err != nil {
			return nil, err
		}
		sourceIdentity, err := canonicalRenameLocation(canonicalRoot, logical)
		if err != nil {
			return nil, err
		}
		destination := renamedPath(scheme, logical)
		destinationIdentity, err := canonicalRenameLocation(canonicalRoot, destination)
		if err != nil {
			return nil, err
		}
		targets = append(targets, concreteTarget{
			display:             filepath.ToSlash(rule.text),
			logical:             logical,
			destination:         destination,
			sourceIdentity:      sourceIdentity,
			destinationIdentity: destinationIdentity,
			sourceLexical:       identityForPath(filepath.Join(root, logical)),
			destinationLexical:  identityForPath(filepath.Join(root, destination)),
			move: Move{
				From: filepath.Join(root, logical),
				To:   filepath.Join(root, destination),
			},
		})
	}
	return targets, nil
}

func resolveConcreteSpelling(root string, scheme Scheme, target string) (string, error) {
	actual, complete, err := resolveExistingSpelling(root, target)
	if err != nil {
		return "", err
	}
	if complete {
		return actual, nil
	}
	decorated := renamedPath(scheme, target)
	decoratedActual, decoratedComplete, err := resolveExistingSpelling(root, decorated)
	if err != nil {
		return "", err
	}
	if decoratedComplete {
		dir, base := filepath.Split(decoratedActual)
		return filepath.Join(dir, undecorate(scheme, base)), nil
	}
	return actual, nil
}

func resolveExistingSpelling(root, target string) (string, bool, error) {
	segments := strings.Split(filepath.ToSlash(filepath.Clean(target)), "/")
	current := root
	actual := make([]string, 0, len(segments))
	for i, wanted := range segments {
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", false, fmt.Errorf("read quarantine target parent %q: %w", strings.Join(actual, "/"), err)
		}
		var matches []string
		for _, entry := range entries {
			if asciiPathFold(entry.Name()) == asciiPathFold(wanted) {
				matches = append(matches, entry.Name())
			}
		}
		if len(matches) > 1 {
			return "", false, fmt.Errorf("quarantine target %q has ambiguous case-fold spellings at %q", filepath.ToSlash(target), strings.Join(actual, "/"))
		}
		if len(matches) == 0 {
			actual = append(actual, segments[i:]...)
			return filepath.FromSlash(strings.Join(actual, "/")), false, nil
		}
		actual = append(actual, matches[0])
		current = filepath.Join(current, matches[0])
	}
	return filepath.FromSlash(strings.Join(actual, "/")), true, nil
}

func resolveRootIdentity(root string) (rootIdentity, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return rootIdentity{}, fmt.Errorf("make quarantine root absolute: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return rootIdentity{}, fmt.Errorf("resolve quarantine root: %w", err)
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return rootIdentity{}, fmt.Errorf("make resolved quarantine root absolute: %w", err)
	}
	return rootIdentity{absolute: absolute, real: realRoot}, nil
}

func canonicalRenameLocation(root rootIdentity, target string) (pathIdentity, error) {
	parent := filepath.Dir(filepath.Clean(target))
	remaining := make([]string, 0)
	current := parent
	resolvedParent := root.real
	for current != "." && current != "" {
		candidate := filepath.Join(root.absolute, current)
		if _, statErr := os.Lstat(candidate); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return pathIdentity{}, fmt.Errorf("resolve quarantine target parent %q: %w", filepath.ToSlash(current), resolveErr)
			}
			resolvedParent = resolved
			break
		} else if !os.IsNotExist(statErr) {
			return pathIdentity{}, fmt.Errorf("stat quarantine target parent %q: %w", filepath.ToSlash(current), statErr)
		}
		remaining = append([]string{filepath.Base(current)}, remaining...)
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	if !pathWithin(root.real, resolvedParent) {
		return pathIdentity{}, fmt.Errorf("quarantine target %q escapes root through its parent", filepath.ToSlash(target))
	}
	parts := append(append([]string{}, remaining...), filepath.Base(filepath.Clean(target)))
	exact := filepath.Clean(filepath.Join(append([]string{resolvedParent}, parts...)...))
	return identityForPath(exact), nil
}

func identityForPath(value string) pathIdentity {
	exact := filepath.Clean(value)
	return pathIdentity{exact: exact, fold: asciiPathFold(exact)}
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameIdentity(a, b pathIdentity) bool {
	return a.exact == b.exact || a.fold == b.fold
}

func containsIdentity(outer, inner pathIdentity) bool {
	return strings.HasPrefix(inner.exact, outer.exact+string(filepath.Separator)) || strings.HasPrefix(inner.fold, outer.fold+"/")
}

func prepareMoves(root string, scheme Scheme, raw []string) ([]concreteTarget, error) {
	targets, err := normalizeConcreteTargets(root, scheme, raw)
	if err != nil {
		return nil, err
	}
	if err := validateMoveGraph(targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func validateMoveGraph(targets []concreteTarget) error {
	var conflicts []graphConflict
	for i := range targets {
		for j := i + 1; j < len(targets); j++ {
			a, b := targets[i], targets[j]
			aContainsB := containsIdentity(a.sourceIdentity, b.sourceIdentity) || containsIdentity(a.sourceLexical, b.sourceLexical)
			bContainsA := containsIdentity(b.sourceIdentity, a.sourceIdentity) || containsIdentity(b.sourceLexical, a.sourceLexical)
			if aContainsB || bContainsA {
				outer, inner := a, b
				if bContainsA {
					outer, inner = b, a
				}
				conflicts = append(conflicts, graphConflict{1, outer.display, inner.display,
					fmt.Sprintf("quarantine targets %q (outer) and %q (inner) overlap: replace the inner entry, do not join it", outer.display, inner.display)})
			}
			if sameIdentity(a.sourceIdentity, b.sourceIdentity) {
				first, second := orderedPair(a.display, b.display)
				conflicts = append(conflicts, graphConflict{2, first, second,
					fmt.Sprintf("quarantine targets %q and %q identify the same rename location", first, second)})
			}
			for _, edge := range []struct {
				from concreteTarget
				to   concreteTarget
			}{{a, b}, {b, a}} {
				if sameIdentity(edge.from.destinationIdentity, edge.to.sourceIdentity) || sameIdentity(edge.from.destinationLexical, edge.to.sourceLexical) {
					first, second := orderedPair(a.display, b.display)
					destination := filepath.ToSlash(edge.from.destination)
					conflicts = append(conflicts, graphConflict{3, first, second,
						fmt.Sprintf("quarantine moves for %q and %q conflict: destination %q is another source", first, second, destination)})
				}
			}
			if sameIdentity(a.destinationIdentity, b.destinationIdentity) || sameIdentity(a.destinationLexical, b.destinationLexical) {
				first, second := orderedPair(a.display, b.display)
				destination := filepath.ToSlash(a.destination)
				conflicts = append(conflicts, graphConflict{4, first, second,
					fmt.Sprintf("quarantine moves for %q and %q conflict: destination %q is shared", first, second, destination)})
			}
		}
		if sameIdentity(targets[i].sourceIdentity, targets[i].destinationIdentity) || sameIdentity(targets[i].sourceLexical, targets[i].destinationLexical) {
			display := targets[i].display
			conflicts = append(conflicts, graphConflict{5, display, display,
				fmt.Sprintf("quarantine move for %q maps its source onto itself", display)})
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.first != b.first {
			return a.first < b.first
		}
		if a.second != b.second {
			return a.second < b.second
		}
		return a.diagnostic < b.diagnostic
	})
	return errors.New(conflicts[0].diagnostic)
}

func orderedPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// Client hides and restores instruction files under a root directory. It
// carries an exec.Runner for consistency with the rest of the ops layer
// (New(run exec.Runner) *Client), though Hide/Restore rename files directly
// via os.Rename rather than shelling out.
type Client struct {
	run    exec.Runner
	lstat  func(string) (os.FileInfo, error)
	rename func(string, string) error
}

// New builds a Client.
func New(run exec.Runner) *Client {
	return &Client{run: run, lstat: os.Lstat, rename: os.Rename}
}

// Hide renames each of targets (paths relative to root) aside per scheme,
// returning the reversible Moves it made (or, in dry-run, would make). Every
// target is validated and its resolved path checked to stay within root
// BEFORE any rename — a ".."/absolute target, a graph conflict, an occupied
// destination, or a target whose resolved (symlink-following) path escapes
// root is rejected with zero filesystem mutation on a stable tree. A missing
// target is a no-op: it is skipped, not an error.
//
// This is a complete preflight, not a portable multi-path transaction. A
// concurrent writer can change the tree after preflight, and a mutation-phase
// os.Rename failure can leave earlier moves applied. Recovery requires
// inspecting the relative target named by the returned error and repairing
// that move before retrying; automatic rollback would risk clobbering a
// concurrent writer.
func (c *Client) Hide(_ context.Context, root string, scheme Scheme, targets []string, dryRun bool) ([]Move, error) {
	slog.Debug("Preparing to quarantine instruction files.", "root", root, "scheme", scheme, "targetCount", len(targets), "dryRun", dryRun)
	prepared, err := prepareMoves(root, scheme, targets)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := resolveRootIdentity(root)
	if err != nil {
		return nil, err
	}
	ready := make([]concreteTarget, 0, len(prepared))
	for _, target := range prepared {
		move := target.move
		if _, err := c.lstat(move.From); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("Quarantine target missing; skipping.", "target", target.logical)
				continue
			}
			return nil, fmt.Errorf("stat quarantine target %q: %w", filepath.ToSlash(target.logical), err)
		}

		// A target with no ".." can still reach outside root via a symlink;
		// re-check the resolved path before any mutation (mirrors workflow's
		// withinWorkspace guard on strip matches).
		if !withinRoot(canonicalRoot.real, move.From) {
			slog.Error("Quarantine target escapes root; refusing.", "target", target.logical, "resolved", move.From)
			return nil, fmt.Errorf("quarantine target %q escapes root", filepath.ToSlash(target.logical))
		}

		// os.Rename silently overwrites its destination, so a checkout crafted to
		// contain both CLAUDE.md and CLAUDE.md.quarantined would lose the latter.
		// Fetched PR content is hostile input; fail loud rather than clobber.
		if _, err := c.lstat(move.To); err == nil {
			slog.Error("Quarantine destination already exists; refusing to clobber.", "target", target.logical, "destination", move.To)
			return nil, fmt.Errorf("quarantine destination %q already exists", filepath.ToSlash(target.destination))
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat quarantine destination %q: %w", filepath.ToSlash(target.destination), err)
		}
		ready = append(ready, target)
	}
	moves := make([]Move, 0, len(ready))
	for _, target := range ready {
		moves = append(moves, target.move)
	}
	if dryRun {
		return moves, nil
	}
	for _, target := range ready {
		move := target.move
		if err := c.rename(move.From, move.To); err != nil {
			slog.Error("Failed to quarantine target.", "target", target.logical, "error", err)
			return nil, fmt.Errorf("rename quarantine target %q to %q: %w", filepath.ToSlash(target.logical), filepath.ToSlash(target.destination), err)
		}
	}
	slog.Info("Successfully quarantined instruction files.", "root", root, "moved", len(moves), "dryRun", dryRun)
	return moves, nil
}

// Restore reverses each Move (quarantined path -> original path). It is
// idempotent: a Move whose quarantined path (To) no longer exists is a
// no-op, so Restore is safe to call twice (or on a partially-restored set).
func (c *Client) Restore(_ context.Context, moves []Move) error {
	slog.Debug("Preparing to restore quarantined files.", "moveCount", len(moves))
	for _, m := range moves {
		if _, err := os.Lstat(m.To); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("Quarantine move already restored; skipping.", "to", m.To)
				continue
			}
			return fmt.Errorf("stat %s: %w", m.To, err)
		}
		// An occupied destination is WARNED, not fatal — deliberately asymmetric
		// with Hide, which refuses.
		//
		// Hide runs before the review, on content that must survive; failing
		// loud there costs nothing. Restore runs inside discard, which returns
		// on error BEFORE sandbox.Teardown, the tmux kill, and the breadcrumb
		// delete. Refusing here would trade a harmless overwrite — inside a
		// directory os.RemoveAll'd two statements later — for a permanently
		// stuck session: the workspace stays on disk holding fetched content,
		// `pr list` keeps showing it, and every `pr teardown` retry fails
		// identically. Reachable whenever a local Codex review (workspace-write)
		// writes an instruction-file name into the tree it is reviewing.
		//
		// So: skip the rename, keep going, and leave a record.
		if _, err := os.Lstat(m.From); err == nil {
			slog.Warn("Restore destination already exists; leaving the quarantined copy in place.",
				"from", m.From, "to", m.To)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", m.From, err)
		}
		if err := os.Rename(m.To, m.From); err != nil {
			slog.Error("Failed to restore quarantined file.", "from", m.From, "to", m.To, "error", err)
			return fmt.Errorf("rename %s: %w", m.To, err)
		}
	}
	slog.Info("Successfully restored quarantined files.", "moveCount", len(moves))
	return nil
}

// ComputeMoves resolves each concrete target (validated, relative to root) to
// its Move pair without mutating the filesystem. It reads path spelling and
// canonical parent identity so restore/status enforce the same graph boundary
// as Hide.
func ComputeMoves(root string, scheme Scheme, targets []string) ([]Move, error) {
	prepared, err := prepareMoves(root, scheme, targets)
	if err != nil {
		return nil, err
	}
	moves := make([]Move, 0, len(prepared))
	for _, target := range prepared {
		moves = append(moves, target.move)
	}
	return moves, nil
}

// undecorate is the inverse of renamedPath's basename transform: given a name
// the walk matched, it returns the pre-quarantine spelling. A name that is not
// decorated (the pre-Hide case) is returned unchanged, so the walk yields the
// same original-relative path in both directions — the property ExpandTargets
// depends on for reversibility.
// Matching is case-insensitive, because the walk that feeds it is: an exact
// TrimSuffix would leave `src/AGENTS.md.QUARANTINED` undecorated to itself, and
// Hide would then rename it to `…QUARANTINED.quarantined` — a name the teardown
// walk no longer matches, breaking the reversibility this function exists to
// preserve.
func undecorate(scheme Scheme, base string) string {
	lower := strings.ToLower(base)
	switch scheme {
	case SuffixQuarantined:
		if strings.HasSuffix(lower, ".quarantined") {
			return base[:len(base)-len(".quarantined")]
		}
		return base
	default:
		if strings.HasPrefix(lower, "_") {
			return base[1:]
		}
		return base
	}
}

// renamedPath applies scheme to the base name of a cleaned relative path,
// leaving any parent directory component untouched — a nested target like
// ".github/copilot-instructions.md" renames only its base name.
func renamedPath(scheme Scheme, cleanRel string) string {
	dir, base := filepath.Split(cleanRel)
	var newBase string
	switch scheme {
	case SuffixQuarantined:
		newBase = base + ".quarantined"
	default:
		newBase = "_" + base
	}
	return filepath.Join(dir, newBase)
}

// withinRoot reports whether target, after resolving symlinks, stays inside
// root. Ported from workflow's withinWorkspace: a target with no ".." can
// still be a symlink pointing outside root, so every match is re-checked here
// before any rename.
func withinRoot(root, target string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		realTarget = target
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
