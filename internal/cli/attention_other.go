// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// The desk the queue reads is built on openat, O_NOFOLLOW and process
// groups, so off Unix list refuses before touching anything.

func errAttentionUnsupported() error {
	return WithExitCode(fmt.Errorf("forgectl attention is not supported on this OS (%s); it needs a Unix system", runtime.GOOS), exitUsage)
}

func runAttentionList(*cobra.Command, module.Deps, attentionOpts) error {
	return errAttentionUnsupported()
}
