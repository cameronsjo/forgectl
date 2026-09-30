//go:build !unix

package exec

import "os/exec"

// setProcessGroup has no process-group equivalent here; the child is killed
// alone, as without WithProcessGroup.
func setProcessGroup(*exec.Cmd) {}
