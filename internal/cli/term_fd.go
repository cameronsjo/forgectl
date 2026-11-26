// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"io"

	"golang.org/x/term"
)

func docsReadOutputIsTerminal(out io.Writer) bool {
	fdWriter, ok := out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdWriter.Fd()))
}

func docsReadInputIsTerminal(in io.Reader) bool {
	fdReader, ok := in.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdReader.Fd()))
}
