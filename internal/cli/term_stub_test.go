// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import "testing"

func stubInteractiveTTY(t *testing.T, interactive bool) {
	t.Helper()
	prev := isInteractiveTTY
	isInteractiveTTY = func() bool { return interactive }
	t.Cleanup(func() { isInteractiveTTY = prev })
}

func stubTermIsDumb(t *testing.T, dumb bool) {
	t.Helper()
	prev := termIsDumb
	termIsDumb = func() bool { return dumb }
	t.Cleanup(func() { termIsDumb = prev })
}
