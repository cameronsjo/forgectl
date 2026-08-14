package projects

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// gitStatus runs git in dir and returns a populated GitStatus. Returns
// StatusNotRepo when dir has no .git, and StatusUnknown when the git command
// itself fails or its output can't be understood — neither is ever read as
// StatusOK, so a caller can't mistake "never checked" or "check failed" for a
// clean tree.
//
// One process per repository. `status --porcelain=v2 --branch` carries the
// ahead/behind counts in its own header block, so the second `rev-list
// --count @{upstream}..HEAD` walk the v1 shape needed on every clean tree is
// gone — that was half the git spawns of a clean inventory (forgectl#216).
//
// Two consequences of folding the two commands into one, both deliberate:
//
//   - Git 2.11.0 is the support floor. That release defined --porcelain=<version>,
//     porcelain v2, and the branch.ab header. Older git fails the command, which
//     lands on StatusUnknown, and PullAll skips rather than mutating.
//   - A branch-graph failure is no longer swallowed. The v1 shape ran rev-list
//     separately and ignored its error, reporting a clean StatusOK with ahead
//     zero; now the same failure makes the single command exit nonzero and the
//     repository reports StatusUnknown. Refusing to act on a tree whose status
//     could not be fully established is the safer answer.
//
// There is no version probe and no v1 fallback: an arbitrary command error is
// not a reliable version signal — it may equally be cancellation, permissions,
// corruption, or a missing object — and retrying could reclassify an
// unreadable tree as clean.
func gitStatus(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, dir string) GitStatus {
	if _, err := os.Stat(dir + "/.git"); err != nil {
		return GitStatus{State: StatusNotRepo}
	}

	out, err := run.Run(ctx, "git", "-C", dir, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return GitStatus{State: StatusUnknown}
	}
	gs, ok := parseGitStatusV2(out)
	if !ok {
		return GitStatus{State: StatusUnknown}
	}
	return gs
}

// parseGitStatusV2 turns `git status --porcelain=v2 --branch` output into a
// GitStatus. The bool reports whether the protocol was understood at all;
// false means the caller must treat the tree as StatusUnknown.
//
// Headers and records get deliberately different treatment, because git
// documents them differently. Header lines are an EXTENSIBLE set, so an
// unrecognized `# ` line is harmless and a malformed `# branch.ab` costs only
// the ahead count — a cosmetic label. Record discriminators are NOT extensible,
// so an unknown or truncated record is a protocol failure: quietly ignoring a
// dirty record git meant to report would make the tree look clean, and PullAll
// rebases a clean tree.
//
// Paths are hostile input and are never decoded — only their presence and, for
// a rename, their TAB separation are checked. Without -z, git C-quotes any path
// containing a newline, TAB, quote, or backslash, so those characters arrive as
// escape sequences inside one physical line and cannot forge a second record.
func parseGitStatusV2(out string) (GitStatus, bool) {
	gs := GitStatus{State: StatusOK}
	ahead := 0
	// Any malformed branch.ab poisons the ahead count for the whole payload:
	// a partially forged header must not have a valid sibling accepted in its
	// place.
	aheadUsable := true

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "branch.ab" {
				if n, ok := parseBranchAB(fields); ok {
					ahead = n
				} else {
					aheadUsable = false
				}
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "1 "):
			// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			if !validFixedFields(line, 9) {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Modified++
		case strings.HasPrefix(line, "2 "):
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <score> <path>\t<origPath>
			if !validRenameRecord(line) {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Modified++
		case strings.HasPrefix(line, "u "):
			// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
			if !validFixedFields(line, 11) {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Modified++
		case strings.HasPrefix(line, "? "):
			if !validFixedFields(line, 2) {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Untracked++
		case strings.HasPrefix(line, "! "):
			// An ignored item is recognized so it can't be mistaken for an
			// unknown discriminator, and counted nowhere.
			if !validFixedFields(line, 2) {
				return GitStatus{State: StatusUnknown}, false
			}
		default:
			return GitStatus{State: StatusUnknown}, false
		}
	}

	// A dirty tree keeps Ahead at zero, matching the labels and JSON the v1
	// shape produced — it never ran rev-list on a dirty tree either.
	if gs.Modified == 0 && gs.Untracked == 0 && aheadUsable {
		gs.Ahead = ahead
	}
	return gs, true
}

// validFixedFields checks that a record splits into exactly n space-separated
// fields with none of them empty. Splitting is bounded at n so the trailing
// path field keeps any spaces it contains rather than being chopped into more
// fields — which is also what makes a truncated record detectable: it yields
// fewer than n fields, or an empty final one.
//
// XY and the submodule field carry fixed widths git guarantees; checking them
// rejects a record that has the right field COUNT but was assembled by hand.
// Object names, modes, and rename scores are checked for presence only —
// validating their contents would be decoding data this parser has no use for.
func validFixedFields(line string, n int) bool {
	fields := strings.SplitN(line, " ", n)
	if len(fields) != n {
		return false
	}
	for _, f := range fields {
		if f == "" {
			return false
		}
	}
	// The two-field forms (? and !) carry a discriminator and a path, nothing
	// with a fixed width to check.
	if n == 2 {
		return true
	}
	return len(fields[1]) == 2 && len(fields[2]) == 4
}

// validRenameRecord checks a type-2 record: the type-1 fixed fields plus a
// rename/copy score, then a target and source path divided by the literal TAB
// git documents as the separator. A C-quoted path may contain an ESCAPED tab
// (the two characters \ and t), so only a physical TAB byte divides the pair —
// which is exactly why the split is on "\t" and not on the escape sequence.
func validRenameRecord(line string) bool {
	const renameFields = 10
	if !validFixedFields(line, renameFields) {
		return false
	}
	paths := strings.SplitN(line, " ", renameFields)[renameFields-1]
	target, source, found := strings.Cut(paths, "\t")
	return found && target != "" && source != ""
}

// parseBranchAB reads the ahead magnitude out of an already-split
// `# branch.ab +<ahead> -<behind>` header, requiring the complete documented
// shape. Behind is validated but discarded — GitStatus has no field for it,
// and a header whose behind token is malformed is not a header to trust the
// ahead token from either.
func parseBranchAB(fields []string) (int, bool) {
	if len(fields) != 4 || fields[0] != "#" || fields[1] != "branch.ab" {
		return 0, false
	}
	ahead, ok := parseSignedCount(fields[2], '+')
	if !ok {
		return 0, false
	}
	if _, ok := parseSignedCount(fields[3], '-'); !ok {
		return 0, false
	}
	return ahead, true
}

// parseSignedCount parses a `<sign><decimal>` token, requiring the exact sign
// and at least one ASCII decimal digit. The explicit digit scan is what
// rejects an encoded negative like "+-1" or "--1", which strconv.Atoi would
// otherwise happily read as a negative count; Atoi then still runs, to catch
// a magnitude that overflows int.
func parseSignedCount(token string, sign byte) (int, bool) {
	if len(token) < 2 || token[0] != sign {
		return 0, false
	}
	digits := token[1:]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}
