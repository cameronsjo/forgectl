// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/progstatus"
)

func TestReportDeskWatchState(t *testing.T) {
	var buf bytes.Buffer
	r := progstatus.ForWriter(&buf, false)
	reportDeskWatchState(r, "x", desk.WatchWaiting)
	if got := buf.String(); !strings.Contains(got, "state=blocked") || !strings.Contains(got, ":kind=permission") {
		t.Errorf("waiting = %q, want blocked with kind=permission", got)
	}
	buf.Reset()
	reportDeskWatchState(r, "x", desk.WatchRunning)
	if got := buf.String(); !strings.Contains(got, "state=working") || strings.Contains(got, "kind=") {
		t.Errorf("running = %q, want working with no kind", got)
	}
}

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
			reportDeskWatchOutcome(progstatus.ForWriter(&buf, false), "x", tc.err, true)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("= %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestReportDeskWatchOutcome_PanicIsNotDone(t *testing.T) {
	var buf bytes.Buffer
	reportDeskWatchOutcome(progstatus.ForWriter(&buf, false), "x", nil, false)
	if strings.Contains(buf.String(), "state=done") || !strings.Contains(buf.String(), "state=error") {
		t.Errorf("= %q, want an error report and no done", buf.String())
	}
}
