// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/module"
)

// `_supervise` runs an item only against the hash and kind the operator
// approved, so the command refuses to start without --sha or --kind.
func TestDeskSuperviseRequiresTheApprovedSHAAndKind(t *testing.T) {
	sha := strings.Repeat("0", 64)
	for flag, args := range map[string][]string{
		"sha":  {"--kind", "script", "01-x"},
		"kind": {"--sha", sha, "01-x"},
	} {
		t.Run(flag, func(t *testing.T) {
			cmd := newDeskCmd(module.Deps{})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"_supervise", "--dir", filepath.Join(t.TempDir(), "desk")}, args...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), `"`+flag+`"`) {
				t.Fatalf("Execute = %v, want a missing --%s error", err, flag)
			}
		})
	}
}
