// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

func statusB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestReportOutcome(t *testing.T) {
	var ok, bad bytes.Buffer
	reportOutcome(progstatus.ForWriter(&ok, false), "wf", nil)
	reportOutcome(progstatus.ForWriter(&bad, false), "wf", errors.New("step 1 (run): boom\nsecond line"))
	if !strings.Contains(ok.String(), "state=done") || !strings.Contains(ok.String(), statusB64("wf finished")) {
		t.Errorf("nil error = %q, want done 'wf finished'", ok.String())
	}
	if !strings.Contains(bad.String(), "state=error") || !strings.Contains(bad.String(), statusB64("wf: step 1 (run): boom")) {
		t.Errorf("error = %q, want error with the first line only", bad.String())
	}
}

func TestReportBlocked_ClearsOnEveryPath(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
		pan  bool
	}{
		{"success", func() error { return nil }, false},
		{"error", func() error { return errors.New("denied") }, false},
		{"panic", func() error { panic("boom") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := progstatus.ForWriter(&buf, false)
			func() {
				defer func() { _ = recover() }()
				_ = reportBlocked(r, "auth", "Touch ID: x", tc.fn)
			}()
			out := buf.String()
			if !strings.Contains(out, "state=blocked") || !strings.Contains(out, ":kind=auth") {
				t.Errorf("no blocked/auth report in %q", out)
			}
			if !strings.HasSuffix(out, "state=clear:id=forgectl:app=forgectl\x1b\\") {
				t.Errorf("last report is not the clear in %q", out)
			}
		})
	}
}
