//go:build !unix

package privdir

// Pin is unavailable off Unix.
//
// The guarantee this package sells is built on openat, O_NOFOLLOW, and
// descriptor-relative stat. A platform without those cannot be given a weaker
// implementation behind the same name: a caller would then believe it holds a
// pinned private directory while holding a path it re-resolves. Refusing is
// the honest answer, and forgectl's release targets are Darwin and Linux.
func Pin(Spec) (int, error) { return -1, ErrUnsupported }
