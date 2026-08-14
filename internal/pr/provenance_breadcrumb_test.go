package pr

// Persisted provenance (forgectl#232).
//
// A breadcrumb is HOSTILE INPUT on the way back in, and the provenance field is
// the most attractive byte in it: flipping one string to "operator-authored"
// would otherwise buy an unconfined shell over content of the attacker's
// choosing. These tests pin that the field is never believed on its own — it is
// only ever as good as the canonical local shape corroborating it.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestProvenanceFromRecord_JointShapeValidation asserts the breadcrumb layer ON
// ITS OWN, not through the composite pipeline.
//
// That distinction is the point of this test existing. A mutation probe showed
// that disabling this check breaks NO end-to-end test, because Launch's
// EffectiveProvenance re-check independently catches the same remote-shaped
// forgery. Defense in depth is why both exist — but a layer no test can see is
// a layer a future refactor deletes as dead code, and the depth quietly becomes
// one. So it is pinned here directly.
func TestProvenanceFromRecord_JointShapeValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		bc   Breadcrumb
		want ReviewProvenance
	}{
		{
			"canonical local shape corroborates the claim",
			Breadcrumb{Local: true, Provenance: provenanceOperatorAuthoredString},
			ReviewProvenanceOperatorAuthored,
		},
		{
			// The one-word edit, caught at this layer rather than downstream.
			"remote shape claiming authorship degrades to third-party",
			Breadcrumb{Local: false, Provenance: provenanceOperatorAuthoredString},
			ReviewProvenanceThirdParty,
		},
		{
			"remote shape claiming third-party is believed",
			Breadcrumb{Local: false, Provenance: provenanceThirdPartyString},
			ReviewProvenanceThirdParty,
		},
		{
			"legacy record with no field",
			Breadcrumb{Local: true},
			ReviewProvenanceUnknown,
		},
		{
			"local shape with an unrecognized value",
			Breadcrumb{Local: true, Provenance: "trusted"},
			ReviewProvenanceUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provenanceFromRecord(tc.bc); got != tc.want {
				t.Errorf("provenanceFromRecord = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBreadcrumb_ProvenanceRoundTrip is plan test 7: each value survives a
// write/load cycle in its OWN canonical shape. Round-tripping is the property
// the whole reconstruction path rests on — a value that degraded on reload
// would silently disable Codex for a legitimately asserted session, and one
// that upgraded would be the vulnerability.
func TestBreadcrumb_ProvenanceRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local bool
		ref   Ref
		write ReviewProvenance
		want  ReviewProvenance
	}{
		{"local operator-authored", true, mustLocalRef(localHeadOid, 1), ReviewProvenanceOperatorAuthored, ReviewProvenanceOperatorAuthored},
		{"local unknown", true, mustLocalRef(localHeadOid, 1), ReviewProvenanceUnknown, ReviewProvenanceUnknown},
		{"local third-party", true, mustLocalRef(localHeadOid, 1), ReviewProvenanceThirdParty, ReviewProvenanceThirdParty},
		{"remote third-party", false, Ref{Owner: "o", Repo: "r", Number: 42}, ReviewProvenanceThirdParty, ReviewProvenanceThirdParty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, &exec.FakeRunner{})
			bc := Breadcrumb{
				Workspace:  fakeWorkspace(t),
				Ref:        tc.ref.String(),
				Agent:      "codex",
				CreatedAt:  time.Now().UTC(),
				Local:      tc.local,
				Provenance: tc.write.persisted(),
			}
			path, err := writeBreadcrumb(c.SessionsDir(), tc.ref, bc)
			if err != nil {
				t.Fatalf("writeBreadcrumb: %v", err)
			}
			sess, err := c.loadSession(path)
			if err != nil {
				t.Fatalf("loadSession: %v", err)
			}
			if sess.Provenance != tc.want {
				t.Errorf("reloaded provenance = %v, want %v", sess.Provenance, tc.want)
			}
		})
	}
}

// TestBreadcrumb_UnknownProvenanceWritesNoField keeps a legacy breadcrumb and a
// freshly written unasserted one byte-identical. Two spellings for one state is
// how a schema grows a second decoder.
func TestBreadcrumb_UnknownProvenanceWritesNoField(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := mustLocalRef(localHeadOid, 1)
	bc := Breadcrumb{
		Workspace:  fakeWorkspace(t),
		Ref:        ref.String(),
		Agent:      "claude",
		CreatedAt:  time.Now().UTC(),
		Local:      true,
		Provenance: ReviewProvenanceUnknown.persisted(),
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // test-authored path
	if err != nil {
		t.Fatalf("read breadcrumb: %v", err)
	}
	if strings.Contains(string(data), "provenance") {
		t.Errorf("an unknown-provenance breadcrumb wrote the field:\n%s", data)
	}
}

// TestBreadcrumb_HostileSelfLabelingRefused is plan test 8, and the single most
// important test in this change.
//
// The attack is one word: take a REMOTE-shaped breadcrumb — the kind `forgectl
// pr <ref>` leaves behind after fetching a third party's head into a clean room
// — and edit its provenance field to "operator-authored". If the loader trusted
// that string on its own, the attacker would have converted a fetched hostile
// diff into an unconfined Codex review.
//
// It must resolve third-party (its remote origin is KNOWN, so say so), and
// Launch must make zero new mutations while preserving the workspace.
func TestBreadcrumb_HostileSelfLabelingRefused(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ws := fakeWorkspace(t)
	ref := Ref{Owner: "attacker", Repo: "repo", Number: 7}

	// Written exactly as Prepare would write it for a remote PR...
	bc := Breadcrumb{
		Workspace: ws,
		Ref:       ref.String(),
		Agent:     "codex",
		CreatedAt: time.Now().UTC(),
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}

	// ...then hand-edited on disk, touching ONLY the provenance field. This is
	// the realistic shape of the attack: the rest of the record stays valid, so
	// nothing else about it looks wrong.
	raw, err := os.ReadFile(path) //nolint:gosec // test-authored path
	if err != nil {
		t.Fatalf("read breadcrumb: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal breadcrumb: %v", err)
	}
	doc["provenance"] = provenanceOperatorAuthoredString
	edited, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal edited breadcrumb: %v", err)
	}
	if err := os.WriteFile(path, append(edited, '\n'), 0o600); err != nil {
		t.Fatalf("write edited breadcrumb: %v", err)
	}

	// The record still LOADS — Claude, list, attach, and teardown must keep
	// working on it. Only Codex eligibility is denied.
	sess, err := c.loadSession(path)
	if err != nil {
		t.Fatalf("a hand-edited but schema-valid breadcrumb must still load: %v", err)
	}
	if sess.Provenance != ReviewProvenanceThirdParty {
		t.Fatalf("self-labeled remote breadcrumb resolved %v, want third-party", sess.Provenance)
	}

	sess.FindingsDir = t.TempDir() // clear the unrelated reload guard
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("a self-labeled remote breadcrumb reached the unconfined Codex path")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("refusal made launch-time calls: %+v", fake.Calls)
	}
	if _, statErr := os.Stat(ws); statErr != nil {
		t.Errorf("refusal destroyed the pre-existing workspace: %v", statErr)
	}
}

// TestBreadcrumb_MalformedAndLegacyProvenanceAreUnknown is plan test 9. None of
// these can authorize Codex, and all of them stay loadable for every other verb
// — a security downgrade must not become an availability failure for paths that
// were never at risk.
func TestBreadcrumb_MalformedAndLegacyProvenanceAreUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"legacy: field absent entirely", ""},
		{"malformed: unrecognized value", "trusted"},
		{"malformed: wrong case", "OPERATOR-AUTHORED"},
		{"malformed: padded", " operator-authored"},
		{"malformed: underscored", "operator_authored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, &exec.FakeRunner{})
			ref := mustLocalRef(localHeadOid, 1)
			bc := Breadcrumb{
				Workspace:  fakeWorkspace(t),
				Ref:        ref.String(),
				Agent:      "codex",
				CreatedAt:  time.Now().UTC(),
				Local:      true,
				Provenance: tc.value,
			}
			path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
			if err != nil {
				t.Fatalf("writeBreadcrumb: %v", err)
			}
			sess, err := c.loadSession(path)
			if err != nil {
				t.Fatalf("the record must remain loadable for Claude/manage/teardown: %v", err)
			}
			if sess.Provenance != ReviewProvenanceUnknown {
				t.Errorf("provenance %q resolved %v, want unknown", tc.value, sess.Provenance)
			}
			if err := CheckAgentForReview("codex", sess.Provenance); err == nil {
				t.Errorf("provenance %q authorized Codex", tc.value)
			}
		})
	}
}

// TestBreadcrumb_AttachmentDoesNotManufactureProvenance is plan test 10: a
// reloaded canonical local session keeps its declaration and nothing about the
// act of reloading creates one. Both halves matter — the first would break a
// legitimate workflow, the second would be the bug.
func TestBreadcrumb_AttachmentDoesNotManufactureProvenance(t *testing.T) {
	c := testClient(t, localGitRunner())
	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{
		Agent:      "codex",
		Provenance: ReviewProvenanceOperatorAuthored,
	})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sess.Workspace)
		_ = os.RemoveAll(sess.FindingsDir)
	})

	reloaded, err := c.loadSession(sess.Path)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if reloaded.Provenance != ReviewProvenanceOperatorAuthored {
		t.Errorf("a canonical local declaration did not survive reload: %v", reloaded.Provenance)
	}

	// The other half: an identical session prepared WITHOUT the assertion must
	// not gain one by being written and read back.
	plain, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal (unasserted): %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(plain.Workspace)
		_ = os.RemoveAll(plain.FindingsDir)
	})
	reloadedPlain, err := c.loadSession(plain.Path)
	if err != nil {
		t.Fatalf("loadSession (unasserted): %v", err)
	}
	if reloadedPlain.Provenance != ReviewProvenanceUnknown {
		t.Errorf("a reload manufactured provenance %v from an unasserted session", reloadedPlain.Provenance)
	}
}

// TestBreadcrumb_LocalFlagWithoutSentinelOwnerCannotCarryProvenance closes the
// composed forgery: `local: true` is what corroborates an authorship claim, so
// an attacker's next move after the one-word edit is to set that too. The
// pre-existing cross-representation check rejects the record outright (a local
// claim must name the sentinel owner), and this pins that the provenance field
// gains nothing from riding along with it.
func TestBreadcrumb_LocalFlagWithoutSentinelOwnerCannotCarryProvenance(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "attacker", Repo: "repo", Number: 7}
	bc := Breadcrumb{
		Workspace:  fakeWorkspace(t),
		Ref:        ref.String(),
		Agent:      "codex",
		CreatedAt:  time.Now().UTC(),
		Local:      true, // forged
		Provenance: provenanceOperatorAuthoredString,
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}
	if _, err := c.loadSession(path); err == nil {
		t.Fatal("a breadcrumb claiming locality under a non-sentinel owner must be rejected")
	}
}
