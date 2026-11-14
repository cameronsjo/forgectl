// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

// newProgramStatus builds the OSC 7501 reporter every long-running command
// shares. It writes to stderr, so --json and piped stdout stay clean.
func newProgramStatus() *progstatus.Reporter {
	return progstatus.New(os.Stderr, os.Getenv)
}

// shortCause is the first line of err, for a one-line status message.
func shortCause(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}

// reportOutcome maps a command's returned error to a final report: nil is done,
// anything else is error. Call it from a defer over the named result. finished
// is set only on the normal success return, so a nil error with finished false
// means a panic is unwinding and reports error, not done.
func reportOutcome(r *progstatus.Reporter, name string, err error, finished bool) {
	if err == nil && !finished {
		r.Error(name + " stopped unexpectedly")
		return
	}
	if err == nil {
		r.Done(name + " finished")
		return
	}
	r.Error(fmt.Sprintf("%s: %s", name, shortCause(err)))
}
