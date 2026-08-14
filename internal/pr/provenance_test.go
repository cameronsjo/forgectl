package pr

import (
	"strings"
	"testing"
)

// Provenance is the third axis forgectl#232 adds: local/remote answers where
// the bytes were FETCHED from, and it was being asked who WROTE them. These
// tests pin the answer to the second question and nothing else — Claude stays
// available in every state, and only Codex's unconfined path is gated.

// TestCheckAgentForReview_OnlyOperatorAuthoredCodexIsPermitted walks the full
// agent × provenance matrix. It is the whole eligibility rule in one table: the
// unconfined path opens for exactly one cell, and every other pairing is
// untouched.
func TestCheckAgentForReview_OnlyOperatorAuthoredCodexIsPermitted(t *testing.T) {
	for _, tc := range []struct {
		agent      string
		provenance ReviewProvenance
		wantRefuse bool
	}{
		{"codex", ReviewProvenanceOperatorAuthored, false},
		{"codex", ReviewProvenanceThirdParty, true},
		{"codex", ReviewProvenanceUnknown, true},

		// Claude is never gated by provenance: its allowlist confines the
		// reviewer regardless of who wrote the bytes.
		{"claude", ReviewProvenanceOperatorAuthored, false},
		{"claude", ReviewProvenanceThirdParty, false},
		{"claude", ReviewProvenanceUnknown, false},
		{"", ReviewProvenanceThirdParty, false},
		{"", ReviewProvenanceUnknown, false},

		// The unwired escalation path is not this refusal's business.
		{"escalation", ReviewProvenanceThirdParty, false},
		{"escalation", ReviewProvenanceUnknown, false},
	} {
		err := CheckAgentForReview(tc.agent, tc.provenance)
		if got := err != nil; got != tc.wantRefuse {
			t.Errorf("CheckAgentForReview(%q, %s) refused = %v, want %v (err: %v)",
				tc.agent, tc.provenance, got, tc.wantRefuse, err)
		}
	}
}

// TestCheckAgentForReview_ZeroValueIsIneligible states the fail-closed property
// the tri-state exists for: a caller that constructs opts without thinking about
// provenance gets the refusing value, not the permitting one.
func TestCheckAgentForReview_ZeroValueIsIneligible(t *testing.T) {
	var zero ReviewProvenance
	if zero != ReviewProvenanceUnknown {
		t.Fatalf("zero ReviewProvenance = %v, want ReviewProvenanceUnknown", zero)
	}
	if err := CheckAgentForReview("codex", zero); err == nil {
		t.Error("the zero provenance value permitted the unconfined Codex path")
	}
}

// TestCheckAgentForReview_RefusalIsActionable keeps the refusal useful without
// letting it route the operator around the control.
//
// The asymmetry it pins is deliberate. An UNKNOWN session is a local tree nobody
// has vouched for, so naming `--operator-authored` is genuine help. A THIRD-PARTY
// session is known to be someone else's code, where the same sentence would be
// an instruction to make a false statement — the refusal must not name the
// incantation that silences it.
func TestCheckAgentForReview_RefusalIsActionable(t *testing.T) {
	unknown := CheckAgentForReview("codex", ReviewProvenanceUnknown)
	thirdParty := CheckAgentForReview("codex", ReviewProvenanceThirdParty)
	if unknown == nil || thirdParty == nil {
		t.Fatal("an ineligible Codex pairing was permitted")
	}

	// Both must name the working alternative and the reason.
	for _, err := range []error{unknown, thirdParty} {
		for _, want := range []string{"claude", "which commands run", "did not write"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not mention %q: %v", want, err)
			}
		}
	}

	if !strings.Contains(unknown.Error(), "--operator-authored") {
		t.Errorf("an unknown-provenance refusal should name the assertion: %v", unknown)
	}
	if strings.Contains(thirdParty.Error(), "--operator-authored") {
		t.Errorf("a third-party refusal must NOT name the flag that would silence it, "+
			"since using it there would be a false statement: %v", thirdParty)
	}
	// It must never suggest laundering the review through `pr local`.
	if strings.Contains(thirdParty.Error(), "pr local") {
		t.Errorf("refusal routes the operator around the control: %v", thirdParty)
	}
}

// TestReviewProvenance_StringRoundTrip pins the wire spellings. They are
// persisted in breadcrumbs, so drifting one silently reclassifies every session
// written before the drift.
func TestReviewProvenance_StringRoundTrip(t *testing.T) {
	for _, p := range []ReviewProvenance{
		ReviewProvenanceUnknown,
		ReviewProvenanceOperatorAuthored,
		ReviewProvenanceThirdParty,
	} {
		if got := ParseReviewProvenance(p.String()); got != p {
			t.Errorf("ParseReviewProvenance(%q) = %v, want %v", p.String(), got, p)
		}
	}
}

// TestParseReviewProvenance_UnrecognizedIsUnknown is the fail-closed half of the
// parser: anything the schema does not know becomes the refusing value. A
// breadcrumb written by a future version, a truncated file, or a hand-edited
// string all land here rather than in an eligible state.
func TestParseReviewProvenance_UnrecognizedIsUnknown(t *testing.T) {
	for _, s := range []string{
		"",
		"unrecognized",
		"OPERATOR-AUTHORED",
		"operator_authored",
		"operator-authored ",
		" operator-authored",
		"trusted",
		"local",
	} {
		if got := ParseReviewProvenance(s); got != ReviewProvenanceUnknown {
			t.Errorf("ParseReviewProvenance(%q) = %v, want unknown", s, got)
		}
	}
}

// TestEffectiveProvenance_RemoteRefCannotUpgrade is forgectl#232's central
// claim, at the one seam where a mistake is fatal: a declaration is only ever
// DOWNGRADED by the ref it describes, never upgraded. A remote ref carrying an
// operator-authored declaration — a caller bug, a hostile breadcrumb, a future
// route that forgets to declare — resolves third-party.
func TestEffectiveProvenance_RemoteRefCannotUpgrade(t *testing.T) {
	remote := Ref{Owner: "o", Repo: "r", Number: 42}
	// Owner literally named "local": the display spelling is not the predicate.
	forgedOwner := Ref{Owner: localOwnerSentinel, Repo: "abc1234", Number: 1}
	local := mustLocalRef("abc1234", 1)

	for _, tc := range []struct {
		name     string
		ref      Ref
		declared ReviewProvenance
		want     ReviewProvenance
	}{
		// A remote ref resolves third-party unconditionally: its content came
		// from a forge, so it is someone else's whatever the caller declared.
		{"remote claiming authored", remote, ReviewProvenanceOperatorAuthored, ReviewProvenanceThirdParty},
		{"forged local owner claiming authored", forgedOwner, ReviewProvenanceOperatorAuthored, ReviewProvenanceThirdParty},
		{"remote undeclared is third-party, not unknown", remote, ReviewProvenanceUnknown, ReviewProvenanceThirdParty},
		{"remote third-party stays third-party", remote, ReviewProvenanceThirdParty, ReviewProvenanceThirdParty},

		// A local ref does not MANUFACTURE authorship — it only declines to
		// contradict one. Locality proves path ownership, never authorship.
		{"local unknown is not upgraded", local, ReviewProvenanceUnknown, ReviewProvenanceUnknown},
		{"local third-party is not upgraded", local, ReviewProvenanceThirdParty, ReviewProvenanceThirdParty},
		{"local authored survives", local, ReviewProvenanceOperatorAuthored, ReviewProvenanceOperatorAuthored},
	} {
		if got := EffectiveProvenance(tc.ref, tc.declared); got != tc.want {
			t.Errorf("%s: EffectiveProvenance = %v, want %v", tc.name, got, tc.want)
		}
	}
}
