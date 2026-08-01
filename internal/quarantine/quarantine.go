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
	"path/filepath"
	"sort"
	"strings"

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
	literals := make([]string, 0, len(targets))
	var patterns []string
	for _, t := range targets {
		if isPattern(t) {
			patterns = append(patterns, t)
			continue
		}
		literals = append(literals, t)
	}

	var nested []string
	if wantsExpansion(literals) {
		var err error
		if nested, err = walkNestable(root, scheme, literals); err != nil {
			return nil, err
		}
	}

	var matched []string
	if len(patterns) > 0 {
		var err error
		if matched, err = expandPatterns(root, scheme, patterns, coveredRootEntries(literals)); err != nil {
			return nil, err
		}
	}

	all := append(append(append([]string{}, literals...), nested...), matched...)
	out := make([]string, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, t := range all {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
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
func coveredRootEntries(targets []string) map[string]bool {
	covered := make(map[string]bool, len(targets))
	for _, t := range targets {
		clean := filepath.Clean(t)
		if clean == "." || strings.ContainsRune(clean, filepath.Separator) {
			continue
		}
		covered[clean] = true
	}
	return covered
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
func expandPatterns(root string, scheme Scheme, patterns []string, covered map[string]bool) ([]string, error) {
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
				if covered[top] || covered[undecorated] || skipDirNames[top] || skipDirNames[undecorated] {
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

// globFold is filepath.Glob's contract — resolve a relative pattern against
// root, return the concrete paths it matches — with one difference: every path
// segment is matched on its LOWERCASED form.
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
func globFold(root, pattern string) ([]string, error) {
	clean := filepath.Clean(pattern)
	if clean == "." || clean == string(filepath.Separator) {
		return []string{root}, nil
	}
	current := []string{root}
	for _, seg := range strings.Split(filepath.ToSlash(clean), "/") {
		if seg == "" {
			continue
		}
		lower := strings.ToLower(seg)
		var next []string
		for _, dir := range current {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue // not a directory, or unreadable: no matches from here
			}
			for _, e := range entries {
				ok, matchErr := filepath.Match(lower, strings.ToLower(e.Name()))
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
func walkNestable(root string, scheme Scheme, targets []string) ([]string, error) {
	// Keyed on the LOWERCASED basename. APFS and NTFS are case-insensitive by
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
		want[strings.ToLower(t)] = true
		want[strings.ToLower(filepath.Base(renamedPath(scheme, t)))] = true
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
			if path != root && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !want[strings.ToLower(d.Name())] {
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

// Client hides and restores instruction files under a root directory. It
// carries an exec.Runner for consistency with the rest of the ops layer
// (New(run exec.Runner) *Client), though Hide/Restore rename files directly
// via os.Rename rather than shelling out.
type Client struct {
	run exec.Runner
}

// New builds a Client.
func New(run exec.Runner) *Client {
	return &Client{run: run}
}

// Hide renames each of targets (paths relative to root) aside per scheme,
// returning the reversible Moves it made (or, in dry-run, would make). Every
// target is validated and its resolved path checked to stay within root
// BEFORE any rename — a ".."/absolute target, or a target whose resolved
// (symlink-following) path escapes root, is rejected with zero filesystem
// mutation. A missing target is a no-op: it is skipped, not an error.
func (c *Client) Hide(_ context.Context, root string, scheme Scheme, targets []string, dryRun bool) ([]Move, error) {
	slog.Debug("Preparing to quarantine instruction files.", "root", root, "scheme", scheme, "targetCount", len(targets), "dryRun", dryRun)
	var moves []Move
	for _, target := range targets {
		move, err := computeMove(root, scheme, target)
		if err != nil {
			slog.Warn("Invalid quarantine target.", "target", target, "error", err)
			return nil, err
		}

		if _, err := os.Lstat(move.From); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("Quarantine target missing; skipping.", "target", target)
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", move.From, err)
		}

		// A target with no ".." can still reach outside root via a symlink;
		// re-check the resolved path before any mutation (mirrors workflow's
		// withinWorkspace guard on strip matches).
		if !withinRoot(root, move.From) {
			slog.Error("Quarantine target escapes root; refusing.", "target", target, "resolved", move.From)
			return nil, fmt.Errorf("quarantine target %q escapes root", target)
		}

		// os.Rename silently overwrites its destination, so a checkout crafted to
		// contain both CLAUDE.md and CLAUDE.md.quarantined would lose the latter.
		// Fetched PR content is hostile input; fail loud rather than clobber.
		if _, err := os.Lstat(move.To); err == nil {
			slog.Error("Quarantine destination already exists; refusing to clobber.", "target", target, "destination", move.To)
			return nil, fmt.Errorf("quarantine destination %q already exists", move.To)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", move.To, err)
		}

		if dryRun {
			moves = append(moves, move)
			continue
		}

		if err := os.Rename(move.From, move.To); err != nil {
			slog.Error("Failed to quarantine target.", "target", target, "error", err)
			return nil, fmt.Errorf("rename %s: %w", move.From, err)
		}
		moves = append(moves, move)
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

// ComputeMoves resolves each target (validated, relative to root) to its Move
// pair without touching the filesystem — used by callers (e.g. the CLI's
// restore/status verbs) that need the same From/To mapping Hide computes,
// without performing a Hide.
func ComputeMoves(root string, scheme Scheme, targets []string) ([]Move, error) {
	moves := make([]Move, 0, len(targets))
	for _, target := range targets {
		move, err := computeMove(root, scheme, target)
		if err != nil {
			return nil, err
		}
		moves = append(moves, move)
	}
	return moves, nil
}

// computeMove validates target and resolves it (and its renamed form) against
// root, without touching the filesystem.
func computeMove(root string, scheme Scheme, target string) (Move, error) {
	if err := validateQuarantineTarget(target); err != nil {
		return Move{}, err
	}
	cleanRel := filepath.Clean(target)
	return Move{
		From: filepath.Join(root, cleanRel),
		To:   filepath.Join(root, renamedPath(scheme, cleanRel)),
	}, nil
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

// validateQuarantineTarget rejects a target that could escape root: an
// absolute path, or any ".." path-traversal segment. Ported from workflow's
// validateStripGlob.
func validateQuarantineTarget(target string) error {
	if target == "" {
		return errors.New("quarantine target must not be empty")
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("quarantine target %q must not be absolute", target)
	}
	normalized := strings.ReplaceAll(filepath.Clean(target), "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return fmt.Errorf("quarantine target %q must not traverse outside root", target)
		}
	}
	return nil
}
