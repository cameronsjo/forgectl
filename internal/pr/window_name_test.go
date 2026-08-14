package pr

import (
	"strings"
	"testing"
)

// mustWindowName is the review-window name for ref, for tests that need a
// realistic name and are asserting something else.
func mustWindowName(t *testing.T, ref Ref) string {
	t.Helper()
	name, err := ReviewWindowName(ref)
	if err != nil {
		t.Fatalf("ReviewWindowName(%s): %v", ref.String(), err)
	}
	return name
}

// The #218 defect at the seam that actually named the window. Before the
// two-layer identity, both of these produced exactly "pr-local-0123456-74565".
func TestWindowNameSeparatesLocalFromAnOwnerNamedLocal(t *testing.T) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	localRef := newLocalRef(oid)

	// The adversary: a genuine PR under a forge owner literally named "local",
	// in a repo named exactly the operator's short oid, numbered exactly the
	// value newLocalRef derives from that oid's first six hex digits.
	remoteRef, err := RefFromParts(localOwnerSentinel, localRef.Repo, "74565")
	if err != nil {
		t.Fatalf("RefFromParts: %v", err)
	}
	if remoteRef.Number != localRef.Number {
		t.Fatalf("test setup is not the collision pair: %d vs %d", remoteRef.Number, localRef.Number)
	}
	if remoteRef.String() != localRef.String() {
		t.Fatalf("test setup is not the collision pair: %q vs %q", remoteRef.String(), localRef.String())
	}

	localName, err := ReviewWindowName(localRef)
	if err != nil {
		t.Fatalf("ReviewWindowName(local): %v", err)
	}
	remoteName, err := ReviewWindowName(remoteRef)
	if err != nil {
		t.Fatalf("ReviewWindowName(remote): %v", err)
	}
	if localName == remoteName {
		t.Fatalf("a local session and a real PR under owner %q share the window name %q",
			localOwnerSentinel, localName)
	}
	for _, name := range []string{localName, remoteName} {
		if !strings.HasPrefix(name, reviewWindowPrefix) {
			t.Errorf("window name %q left the namespace the admission gate counts", name)
		}
		if len(name) > maxPRSessionNameBytes {
			t.Errorf("window name %q is %d bytes, over the bound", name, len(name))
		}
	}
}

// Two repos under one owner, and one repo whose dots would otherwise be
// sanitized into another's hyphens, stay distinct — the recollision the old
// dot-to-hyphen rewrite deliberately accepted is gone, because the digest is
// taken over the key, not over the display spelling.
func TestWindowNameSeparatesNearMissRefs(t *testing.T) {
	refs := map[string]Ref{}
	for _, parts := range [][3]string{
		{"owner", "a", "42"},
		{"owner", "b", "42"},
		{"owner", "a-b", "42"},
		{"owner", "a.b", "42"},
		{"owner", "a", "43"},
		{"other", "a", "42"},
	} {
		ref, err := RefFromParts(parts[0], parts[1], parts[2])
		if err != nil {
			t.Fatalf("RefFromParts(%v): %v", parts, err)
		}
		name, err := ReviewWindowName(ref)
		if err != nil {
			t.Fatalf("ReviewWindowName(%v): %v", parts, err)
		}
		if prior, seen := refs[name]; seen {
			t.Fatalf("%s and %s share the window name %q", prior.String(), ref.String(), name)
		}
		refs[name] = ref
	}
}

// `pr open` puts a shell in the clean room. It must never be mistaken for the
// review window, and must stay countable by the admission gate.
func TestShellWindowNameIsDistinctAndInNamespace(t *testing.T) {
	ref, err := RefFromParts("cameronsjo", "forgectl", "218")
	if err != nil {
		t.Fatalf("RefFromParts: %v", err)
	}
	review, err := ReviewWindowName(ref)
	if err != nil {
		t.Fatalf("windowName: %v", err)
	}
	shell, err := shellWindowName(ref)
	if err != nil {
		t.Fatalf("shellWindowName: %v", err)
	}
	if shell == review {
		t.Fatalf("shell window shares the review window name %q", review)
	}
	if !strings.HasPrefix(shell, reviewWindowPrefix) {
		t.Fatalf("shell window name %q left the namespace the admission gate counts", shell)
	}
	if len(shell) > maxPRSessionNameBytes {
		t.Fatalf("shell window name %q is %d bytes, over the bound", shell, len(shell))
	}
}

// A ref that cannot produce a key must produce an error, never a fallback
// name — a name with no key behind it is exactly the authority #218 removes.
func TestWindowNameFailsClosedOnAnUnkeyableRef(t *testing.T) {
	if _, err := ReviewWindowName(Ref{}); err == nil {
		t.Fatal("an empty ref produced a window name")
	}
	if _, err := shellWindowName(Ref{}); err == nil {
		t.Fatal("an empty ref produced a shell window name")
	}
}
