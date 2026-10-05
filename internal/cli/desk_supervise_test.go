// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// `_supervise` runs an item only against the hash the operator approved, so
// the command refuses to start without --sha.
func TestDeskSuperviseRequiresTheApprovedSHA(t *testing.T) {
	cmd := newDeskPlumbingCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"_supervise", "--dir", filepath.Join(t.TempDir(), "desk"), "01-x"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `"sha"`) {
		t.Fatalf("Execute = %v, want a missing --sha error", err)
	}
}
