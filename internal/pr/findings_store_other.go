//go:build !unix

package pr

import "os"

// verifyFindingsStore has nothing to check off Unix: there is no owner uid or
// group/world write bit to read, and forgectl's release targets are Darwin
// and Linux.
func verifyFindingsStore(*os.Root) error { return nil }
