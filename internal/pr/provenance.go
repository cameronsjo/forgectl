package pr

import "fmt"

// ReviewProvenance answers WHO WROTE the code under review — a different
// question from `Ref.IsLocal()`, which answers where the bytes were fetched
// from and therefore who owns the PATH they sit on.
//
// forgectl#232 exists because those two were conflated. `gh pr checkout 123`
// leaves the operator's own repository on a third party's commit at a detached
// HEAD: path ownership says "yours", commit provenance says "theirs", and the
// old local/remote binary could only see the first. It reported local, and the
// unconfined Codex path (CodexExec — arbitrary shell with host-wide read) opened
// over a contributor's tree.
//
// The axis is deliberately narrow. It gates ONE thing: eligibility for
// CodexExec. Claude's InlineSeeded path is confined by its own deny-by-default
// allowlist regardless of who wrote the bytes, so it stays available in every
// provenance state — as do list, attach, open, manage, and teardown. This is an
// eligibility check for an unconfinable reviewer, not a launch authorization.
//
// SIGNALS THAT NEVER PROVE AUTHORSHIP, and are never read as such anywhere in
// this package: attachment or detachedness of HEAD, path or directory
// ownership, Git author or committer identity, a commit signature, the
// repository owner, the checkout's name, the configured profile or agent, the
// process UID, a TTY, a socket, or a loopback transport. Every one of them is
// satisfiable by content the operator did not write. Positive provenance comes
// from exactly one place: an explicit operator assertion at the CLI, and a
// canonical local breadcrumb that carries that assertion across reconstruction.
type ReviewProvenance uint8

const (
	// ReviewProvenanceUnknown is the ZERO VALUE, and that is the whole point:
	// a caller that builds opts without considering provenance gets the
	// ineligible state, never the permitting one. It means "nothing has
	// established who wrote this", which is where every unasserted local
	// review sits — including a detached HEAD.
	ReviewProvenanceUnknown ReviewProvenance = iota
	// ReviewProvenanceOperatorAuthored means the operator explicitly asserted
	// they wrote the code under review. It is the ONLY value that permits
	// CodexExec, and the only one no inference can produce.
	ReviewProvenanceOperatorAuthored
	// ReviewProvenanceThirdParty means the code is known to be someone else's
	// — every remote PR route declares it by construction. It is a stronger
	// statement than Unknown and refuses identically; the distinction is kept
	// because it is real, and because a route that forgets to declare should
	// be distinguishable from one that declared correctly.
	ReviewProvenanceThirdParty
)

// provenance wire spellings. These are PERSISTED in breadcrumbs, so changing one
// silently reclassifies every session written before the change. Treat them as
// schema, not as labels.
const (
	provenanceUnknownString          = "unknown"
	provenanceOperatorAuthoredString = "operator-authored"
	provenanceThirdPartyString       = "third-party"
)

// String renders a ReviewProvenance for logs, errors, and the breadcrumb field.
// An out-of-range value renders as unknown, matching how it behaves.
func (p ReviewProvenance) String() string {
	switch p {
	case ReviewProvenanceOperatorAuthored:
		return provenanceOperatorAuthoredString
	case ReviewProvenanceThirdParty:
		return provenanceThirdPartyString
	default:
		return provenanceUnknownString
	}
}

// ParseReviewProvenance decodes a persisted spelling, FAILING CLOSED: anything
// it does not recognize — empty, misspelled, differently cased, whitespace
// padded, a value from a future schema, a hand-edited string — becomes Unknown
// and therefore cannot authorize Codex.
//
// It deliberately does not trim or fold case. A breadcrumb is written by this
// package with an exact spelling, so any deviation is either corruption or an
// edit, and neither deserves a lenient parse on the one field that gates an
// unconfined shell.
func ParseReviewProvenance(s string) ReviewProvenance {
	switch s {
	case provenanceOperatorAuthoredString:
		return ReviewProvenanceOperatorAuthored
	case provenanceThirdPartyString:
		return ReviewProvenanceThirdParty
	default:
		return ReviewProvenanceUnknown
	}
}

// persisted returns the breadcrumb spelling, with Unknown rendering as the empty
// string so it is omitted by `omitempty` entirely. A session with nothing to
// assert writes no field, which keeps a legacy breadcrumb and a freshly written
// unknown one identical rather than introducing a second spelling for the same
// state.
func (p ReviewProvenance) persisted() string {
	if p == ReviewProvenanceUnknown {
		return ""
	}
	return p.String()
}

// EffectiveProvenance resolves a DECLARED provenance against the ref it
// describes. It is a one-way valve: it may only DOWNGRADE.
//
// The asymmetry is the load-bearing part. Downgrading on a non-authorship signal
// is always sound — a remote ref cannot have been written by the operator
// through this route, whatever a caller or a breadcrumb claims — while upgrading
// on one is exactly the bug forgectl#232 fixes. So a remote ref carrying an
// operator-authored declaration resolves third-party (a caller bug, a hostile
// breadcrumb, and a future route that forgets to declare all land here), and a
// LOCAL ref never manufactures a declaration it was not given: local unknown
// stays unknown, because path ownership is not authorship.
//
// Ref.IsLocal() is read here as a NEGATIVE signal only. It retains its existing
// path/workspace/PostReview meaning and is not being repurposed as an authorship
// predicate — that would reintroduce the conflation.
func EffectiveProvenance(ref Ref, declared ReviewProvenance) ReviewProvenance {
	// A non-local ref is third-party BY CONSTRUCTION, whatever was declared: its
	// content was fetched from a forge, so it is someone else's by definition.
	// Resolving it here rather than only downgrading an authored claim makes the
	// classification total — a remote session is never merely "undeclared" — and
	// that totality is load-bearing for the refusal wording below, which may
	// only suggest the `--operator-authored` escape hatch where the content
	// could plausibly be the operator's own.
	if !ref.IsLocal() {
		return ReviewProvenanceThirdParty
	}
	switch declared {
	case ReviewProvenanceOperatorAuthored, ReviewProvenanceThirdParty:
		return declared
	default:
		// Local and undeclared — including every out-of-range value.
		return ReviewProvenanceUnknown
	}
}

// CheckAgentForReview refuses an agent whose confinement does not match who
// wrote the content it is about to read. It replaces CheckAgentForRef, whose
// local/remote input could not represent the question (forgectl#232).
//
// The threat is prompt injection carried in a diff someone else wrote, and the
// asymmetry between the two agents is measured, not assumed:
//
//   - Agent A (InlineSeeded) confines the reviewer with a deny-by-default
//     Claude Code allowlist — four read tools plus eight literal read-only Bash
//     prefixes, under plan mode. It grants no command-execution primitive, so a
//     hostile diff buys nothing. It needs no provenance gate and does not get
//     one.
//   - CodexExec has no allowlist equivalent. `--sandbox read-only` scopes
//     filesystem writes and network egress, NOT which commands run, so the
//     reviewer gets arbitrary shell with host-wide read — `~/.ssh`,
//     `~/.aws/credentials`, `~/.codex/auth.json` — and everything read is
//     transmitted to the model provider as tool output. See CodexExec in
//     agent.go for the full measurement and the two confinements that are
//     unreachable from `codex exec`.
//
// Since the confinement is unreachable, the boundary is drawn by USE: the
// unconfined path opens only over code the operator states they wrote. That is
// a statement no signal can forge and only a human can make.
//
// Returns nil when the pairing is allowed. It is called at both ends of the
// pipeline — at preparation, so an ineligible request refuses before anything is
// fetched or created, and in Launch, which is AUTHORITATIVE because it is the
// one point every route reaches, including a session reconstituted from a
// breadcrumb that never re-enters preparation.
func CheckAgentForReview(agent string, provenance ReviewProvenance) error {
	if LaunchPathFor(agent) != CodexExec || provenance == ReviewProvenanceOperatorAuthored {
		return nil
	}
	// The remedy is provenance-specific, and the difference is a control, not a
	// nicety. Offering `--operator-authored` to someone reviewing a KNOWN
	// third-party head would route them around the boundary — the flag would
	// have to be a false statement to work, and a refusal that names the
	// incantation which silences it is a speed bump. So the escape hatch is
	// mentioned only where the content could honestly be the operator's own:
	// UNKNOWN, which is a local tree nobody has vouched for.
	remedy := "Review this with `--agent claude` (the default), whose allowlist confines " +
		"Bash to an enumerated read-only prefix set."
	subject := "the code under review was written by someone else"
	if provenance == ReviewProvenanceUnknown {
		subject = "nothing has established who wrote the code under review"
		remedy += " If you wrote this code yourself, say so with `--operator-authored`."
	}
	return fmt.Errorf(
		"agent %q cannot review this: %s. Codex's sandbox scopes filesystem and network access "+
			"but NOT which commands run, so a prompt injection in a diff you did not write could "+
			"reach a shell with read access to your whole home directory — and `codex exec` exposes "+
			"no way to confine that. %s",
		agent, subject, remedy,
	)
}
