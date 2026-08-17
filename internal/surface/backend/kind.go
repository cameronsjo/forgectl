// Package backend is the contract between forgectl's surface coordinator and
// the terminal managers it launches into: the closed backend set, the failure
// classes, the versioned reference, and the mutation/cleanup matrix.
//
// It is a separate package from internal/surface for one reason, and the
// reason is enforceable rather than stylistic. A surface launch must reach a
// terminal manager without the manager — or the adapter driving it — ever
// seeing the harness invocation. StartSpec is built so it cannot carry one,
// but that only stops an adapter from being *handed* an invocation; nothing in
// the type system stops an adapter package from importing internal/launch and
// resolving one itself. Keeping the contract in a package that does not depend
// on internal/launch turns that into a dependency-graph fact, and
// barrier_test.go asserts it. The coordinator, which owns the invocation and
// the admission policy, keeps its launch dependency next door.
//
// The types here are uniformly private-field value objects with constructors,
// because the property that matters is not "an adapter should not fabricate an
// ambiguous result" but "an adapter cannot". A generic (Ref, error) pair leaves
// every invalid combination representable, and a rollback decision made from
// error text is a rollback decision made from a string a backend chose.
//
// There is no IPC here yet (forgectl#331 Phase 4b) and no real backend
// (forgectl#332 Phase 5).
package backend

import "strconv"

// Kind is the closed set of terminal managers forgectl can launch into. It is
// a plain enum rather than a string because it is compared, indexed, and
// logged, and a string kind would let a decoded reference name a backend that
// does not exist. The zero value is ineligible.
type Kind uint8

const (
	// KindUnspecified is the ineligible zero value.
	KindUnspecified Kind = iota

	KindTmux
	KindCmux
	KindHerdr

	kindCount
)

var kindNames = [kindCount]string{
	KindUnspecified: "unspecified",
	KindTmux:        "tmux",
	KindCmux:        "cmux",
	KindHerdr:       "herdr",
}

// Valid reports whether k names a real backend. The zero value does not.
func (k Kind) Valid() bool { return k > KindUnspecified && k < kindCount }

func (k Kind) String() string {
	if k >= kindCount {
		return "invalid(" + strconv.Itoa(int(k)) + ")"
	}
	return kindNames[k]
}

// ParseKind resolves a wire or flag spelling. It accepts only the exact
// lowercase names — no aliases, no case folding — because both of its callers
// (the --surface flag and the reference decoder) are places where a forgiving
// parser buys nothing and widens what a stored value can claim to be.
func ParseKind(s string) (Kind, bool) {
	for k := KindUnspecified + 1; k < kindCount; k++ {
		if kindNames[k] == s {
			return k, true
		}
	}
	return KindUnspecified, false
}
