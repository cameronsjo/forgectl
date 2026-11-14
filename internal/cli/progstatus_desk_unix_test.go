// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

func TestReportDeskEvent(t *testing.T) {
	cases := []struct {
		line string
		want string // substring of the decoded msg, "" for no report
	}{
		{"RUN-START id=01-x pid=4", "x running"},
		{"STEP-START id=build deps=a,b", "x build"},
		{"STEP-END id=build rc=0", ""},
		{"RUN-END rc=0 reason=exit", ""},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		reportDeskEvent(progstatus.ForWriter(&buf, false), "x", tc.line)
		if tc.want == "" {
			if buf.Len() != 0 {
				t.Errorf("%q wrote %q, want nothing", tc.line, buf.String())
			}
			continue
		}
		if !strings.Contains(buf.String(), "state=working") || !strings.Contains(buf.String(), statusB64(tc.want)) {
			t.Errorf("%q = %q, want working %q", tc.line, buf.String(), tc.want)
		}
	}
}

func TestReportDeskWatchOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"finished", nil, "state=done"},
		{"deadline clears, the run outlives the watch", WithExitCode(errors.New("did not finish"), deskExitTempFail), "state=clear"},
		{"interrupted is an error", WithExitCode(errors.New("interrupted"), deskExitInterrupted), "state=error"},
		{"failed run is an error", errors.New("finished with exit 1"), "state=error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportDeskWatchOutcome(progstatus.ForWriter(&buf, false), "x", tc.err)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("= %q, want %q", buf.String(), tc.want)
			}
		})
	}
}
