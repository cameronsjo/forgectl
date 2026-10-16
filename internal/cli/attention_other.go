// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// attentionStdinCap mirrors the Unix value for put's help text.
const attentionStdinCap = 64 << 10

// The spool is built on openat and O_NOFOLLOW, and the desk it reads on
// process groups, so off Unix every verb refuses before touching anything.

func errAttentionUnsupported() error {
	return WithExitCode(fmt.Errorf("forgectl attention is not supported on this OS (%s); it needs a Unix system", runtime.GOOS), exitUsage)
}

func runAttentionPut(*cobra.Command, module.Deps, attentionOpts) error {
	return errAttentionUnsupported()
}

func runAttentionRm(*cobra.Command, module.Deps, attentionOpts) error {
	return errAttentionUnsupported()
}

func runAttentionList(*cobra.Command, module.Deps, attentionOpts) error {
	return errAttentionUnsupported()
}
