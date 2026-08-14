package pr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
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

// A name is a PURE FUNCTION OF THE KEY, which is the invariant sessionname.go's
// head comment asserts and the only reason two windows of one session can be
// resolved by name at all. It has teeth exactly where a Ref carries display
// parts the key does not: a local Ref's Owner and Number are both derived from
// the oid and absent from the key, so a breadcrumb marked local with a different
// owner or number must still name the SAME window — not a same-digest name under
// a different label, which resolves to nothing and strands the session.
func TestWindowNameIsAFunctionOfTheKeyNotTheRefSpelling(t *testing.T) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	canonical := newLocalRef(oid)
	// Same key (local kind, same oid in Repo), different display parts.
	impostor := Ref{Owner: "someone-else", Repo: canonical.Repo, Number: 99}.asLocal()
	if impostor.String() == canonical.String() {
		t.Fatal("test setup is wrong: the two refs must differ in display spelling")
	}

	for _, role := range []struct {
		name string
		fn   func(Ref) (string, error)
	}{
		{"review", ReviewWindowName},
		{"shell", shellWindowName},
	} {
		t.Run(role.name, func(t *testing.T) {
			want, err := role.fn(canonical)
			if err != nil {
				t.Fatalf("%s name (canonical): %v", role.name, err)
			}
			got, err := role.fn(impostor)
			if err != nil {
				t.Fatalf("%s name (impostor): %v", role.name, err)
			}
			if got != want {
				t.Fatalf("one key produced two %s names: %q vs %q", role.name, want, got)
			}
		})
	}
}

// The same invariant on the remote kind, where the divergence a Ref can carry is
// case: remoteSessionKey lowercases owner and repo, so two spellings of one
// repository are one identity and must be one name.
func TestWindowNameIgnoresRemoteRefCase(t *testing.T) {
	lower, err := RefFromParts("cameronsjo", "forgectl", "218")
	if err != nil {
		t.Fatalf("RefFromParts(lower): %v", err)
	}
	upper, err := RefFromParts("CameronSjo", "ForgeCtl", "218")
	if err != nil {
		t.Fatalf("RefFromParts(upper): %v", err)
	}
	lowerName, err := ReviewWindowName(lower)
	if err != nil {
		t.Fatalf("ReviewWindowName(lower): %v", err)
	}
	upperName, err := ReviewWindowName(upper)
	if err != nil {
		t.Fatalf("ReviewWindowName(upper): %v", err)
	}
	if lowerName != upperName {
		t.Fatalf("one repository produced two window names: %q vs %q", lowerName, upperName)
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

// tmuxMutations counts the tmux verbs that CHANGE server state. A refusal is
// only real if it is zero of these — an error return proves the caller was told,
// not that nothing happened on the way to telling them.
func tmuxMutations(calls []exec.Call) []string {
	mutating := map[string]bool{
		"new-window": true, "select-window": true, "kill-window": true,
		"rename-window": true, "new-session": true, "kill-session": true,
	}
	var out []string
	for _, call := range calls {
		if call.Name == "tmux" && len(call.Args) > 0 && mutating[call.Args[0]] {
			out = append(out, call.Args[0])
		}
	}
	return out
}

// The fail-closed guarantee, counted rather than asserted in prose: a breadcrumb
// whose ref yields no logical key must reach ZERO tmux mutations — no create, no
// select, no rename, no kill — on every verb that acts on a window.
//
// The breadcrumb here is the realistic corrupt case: marked local, so the oid
// lives in Repo, but carrying something no declared oid width accepts. Every
// other seam validates the ref's charset, so this is what a key failure looks
// like in production rather than a synthetic zero value.
func TestUnkeyableRefReachesZeroTmuxMutations(t *testing.T) {
	ref, err := ParseRef("local/zzz#1")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	ref = ref.asLocal()
	if _, err := ReviewWindowName(ref); err == nil {
		t.Fatal("test setup is wrong: this ref is keyable, so it proves nothing")
	}

	for _, tc := range []struct {
		verb string
		call func(*Client, string) error
	}{
		{"attach", func(c *Client, path string) error { return c.Attach(context.Background(), path) }},
		{"open", func(c *Client, path string) error { return c.Open(context.Background(), path) }},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			fake := reviewServer()
			c := testClient(t, fake)
			workspace := fakeWorkspace(t)
			bc := Breadcrumb{
				Workspace: workspace, Ref: ref.String(), Agent: "claude",
				CreatedAt: time.Now().UTC(), Local: true,
			}
			path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
			if err != nil {
				t.Fatalf("seed breadcrumb: %v", err)
			}

			if err := tc.call(c, path); err == nil {
				t.Fatalf("%s accepted a ref with no derivable session identity", tc.verb)
			}
			if got := tmuxMutations(fake.Calls); len(got) != 0 {
				t.Errorf("%s issued %d tmux mutation(s) %v before refusing; want zero", tc.verb, len(got), got)
			}

			// Positive control: the same verb on a KEYABLE ref must register a
			// mutation. Without this, a tmuxMutations that silently matched
			// nothing — a renamed verb, a changed Call shape — would report the
			// refusal above as airtight while counting exactly as many mutations
			// as a broken counter counts.
			good := Ref{Owner: "o", Repo: "r", Number: 7}
			goodFake := reviewServer(mustWindowName(t, good))
			goodClient := testClient(t, goodFake)
			goodPath, _ := seedSession(t, goodClient, good, time.Now().UTC())
			_ = tc.call(goodClient, goodPath)
			if got := tmuxMutations(goodFake.Calls); len(got) == 0 {
				t.Errorf("%s on a keyable ref registered zero mutations — the counter is broken, "+
					"so the zero above proves nothing; calls=%+v", tc.verb, goodFake.Calls)
			}
		})
	}
}
