//go:build !unix

package surface

// closeFD is unreachable off Unix: privdir.Pin refuses there, so NewRunDir
// returns before a descriptor exists. It is defined only so the package
// compiles for a cross-build check.
func closeFD(int) error { return nil }
