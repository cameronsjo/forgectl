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
//
//   - There is no longer a second command whose error can be discarded. The v1
//     shape ran rev-list separately and ignored its error; gitStatus now has a
//     single Run, and its error is checked.
//
//     This is not an escalation to StatusUnknown, and measurement says so: on a
//     repository whose branch.<name>.merge names a ref with no remote-tracking
//     branch, rev-list --count @{upstream}..HEAD exits 128, while
//     status --porcelain=v2 --branch omits the branch.upstream and branch.ab
//     headers and exits 0. That repository reports StatusOK with ahead zero
//     either way. What changed is that the absent branch graph is now a missing
//     header the parser handles, rather than an error nobody read.
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
// That split is about individual LINES. One assertion is made about the
// payload as a whole, before any line is read: the header block must be
// there at all. An extensible set is still a set git always sends.
//
// Paths are hostile input and are never decoded — only their presence and, for
// a rename, their TAB separation are checked. Without -z, git C-quotes any path
// containing a newline, TAB, quote, or backslash, so those characters arrive as
// escape sequences inside one physical line and cannot forge a second record.
func parseGitStatusV2(out string) (GitStatus, bool) {
	// Asserted before a single record is interpreted, because it is the
	// question of whether this payload answers the command that was issued at
	// all — and that is answerable without reading any record. The placement
	// also keeps the assertion independent of how records are treated: it
	// holds even if record handling ever stops short-circuiting.
	if !hasHeaderLine(out) {
		return GitStatus{State: StatusUnknown}, false
	}

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
			if _, ok := validChangeRecord(line, 9); !ok {
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
			if _, ok := validChangeRecord(line, 11); !ok {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Modified++
		case strings.HasPrefix(line, "? "):
			if !validPathRecord(line) {
				return GitStatus{State: StatusUnknown}, false
			}
			gs.Untracked++
		case strings.HasPrefix(line, "! "):
			// An ignored item is recognized so it can't be mistaken for an
			// unknown discriminator, and counted nowhere.
			if !validPathRecord(line) {
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

// hasHeaderLine reports whether a porcelain-v2 payload carries a header block
// at all.
//
// gitStatus always passes --branch, and every repository shape answers it with
// a block: no upstream still emits branch.oid and branch.head, a detached HEAD
// emits branch.head (detached), an unborn branch emits branch.oid (initial).
// Only branch.upstream and branch.ab are ever omitted. So a payload with no
// header line — the empty string included — is not an answer to the command
// that was issued, and reading a tree state out of it would report on a branch
// this parser never saw.
//
// PRESENCE of the block is what is asserted, never the identity of what is in
// it: an unrecognized `# ` line satisfies this. That is what keeps individual
// headers fail-soft and extensible while the block itself is fail-closed, like
// the non-extensible record discriminators.
//
// A pre-2.11 git cannot reach here — it fails the command outright and
// gitStatus lands on StatusUnknown from that error — so an old-git environment
// is never misreported as a hostile payload.
func hasHeaderLine(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			return true
		}
	}
	return false
}

// splitRecord splits a record into exactly n space-separated fields, none of
// them empty, and reports whether it had that shape. Splitting is bounded at n
// so the trailing path field keeps any spaces it contains rather than being
// chopped into more fields — which is also what makes a truncated record
// detectable: it yields fewer than n fields, or an empty final one.
func splitRecord(line string, n int) ([]string, bool) {
	fields := strings.SplitN(line, " ", n)
	if len(fields) != n {
		return nil, false
	}
	for _, f := range fields {
		if f == "" {
			return nil, false
		}
	}
	return fields, true
}

// validPathRecord checks the two-field forms — `? <path>` and `! <path>` —
// which carry a discriminator and a path and nothing of fixed width.
func validPathRecord(line string) bool {
	_, ok := splitRecord(line, 2)
	return ok
}

// validChangeRecord checks a record whose leading fields git gives fixed
// widths: the two-character XY status and the four-character submodule field.
// Checking those rejects a record with the right field COUNT that was
// assembled by hand. Object names, modes, and rename scores are checked for
// presence only — validating their contents would mean decoding data this
// parser has no use for. Returns the fields so a caller that needs the trailing
// path (the rename form) doesn't split the line a second time.
func validChangeRecord(line string, n int) ([]string, bool) {
	fields, ok := splitRecord(line, n)
	if !ok {
		return nil, false
	}
	if len(fields[1]) != 2 || len(fields[2]) != 4 {
		return nil, false
	}
	return fields, true
}

// validRenameRecord checks a type-2 record: the type-1 fixed fields plus a
// rename/copy score, then a target and source path divided by the literal TAB
// git documents as the separator. A C-quoted path may contain an ESCAPED tab
// (the two characters \ and t), so only a physical TAB byte divides the pair —
// which is exactly why the split is on "\t" and not on the escape sequence.
func validRenameRecord(line string) bool {
	const renameFields = 10
	fields, ok := validChangeRecord(line, renameFields)
	if !ok {
		return false
	}
	target, source, found := strings.Cut(fields[renameFields-1], "\t")
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
