//go:build !darwin && !linux

package docs

// deviceOf has no portable implementation: platforms outside the
// darwin/linux pair get no device-boundary stop, and detectRootKind falls
// back to its $HOME and "/" stops only.
func deviceOf(string) (uint64, bool) {
	return 0, false
}
