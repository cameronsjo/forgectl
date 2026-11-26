// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import "testing"

func stubTermIsDumb(t *testing.T, dumb bool) {
	t.Helper()
	prev := termIsDumb
	termIsDumb = func() bool { return dumb }
	t.Cleanup(func() { termIsDumb = prev })
}
