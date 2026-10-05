// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
)

// TestDeskShowDrawsAnInterruptedStep: the text flow uses the dashboard's
// legend, interrupted included (it once drew the pending mark).
func TestDeskShowDrawsAnInterruptedStep(t *testing.T) {
	spec := &runview.Spec{}
	r := &deskRun{
		ref: runview.RunRef{Source: "desk", Name: "01-x"}, spec: spec, folder: runview.NewFolder(spec, nil),
		state: runview.RunState{Steps: []runview.StepState{{ID: "fetch", Status: runview.StepInterrupted}}},
		live:  runview.LiveEnded,
	}
	for noIcons, want := range map[bool]string{true: "  / fetch  interrupted", false: "  ⊘ fetch  interrupted"} {
		var out bytes.Buffer
		writeDeskShowText(&out, r, 0, false, noIcons)
		if !strings.Contains(out.String(), want) {
			t.Errorf("noIcons=%v: missing %q in\n%s", noIcons, want, out.String())
		}
	}
}

// TestDeskShowAtReplaysADeskRun: --at N on a finished desk run is the state
// after N events, as the dashboard's replay shows it, not the desk's word
// for the run now.
func TestDeskShowAtReplaysADeskRun(t *testing.T) {
	f := newDeskFixture(t)
	d, err := desk.Open(f.deskDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	done := filepath.Join(f.deskDir, desk.DirDone)
	for name, body := range map[string]string{
		"01-x.sh":     "true\n",
		"01-x.log":    "EXIT=0\n",
		"01-x.events": "RUN-START id=01-x pid=1\nRUN-END rc=0 reason=ok\n",
	} {
		if err := os.WriteFile(filepath.Join(done, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now, errOut, code := showJSON(t, "01-x", "--json")
	if code != 0 || now["live"] != "ended" {
		t.Fatalf("show = %v (%d %s), want live ended", now, code, errOut)
	}
	at, errOut, code := showJSON(t, "01-x", "--at", "0", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if at["live"] == "ended" || at["exit"] != nil {
		t.Fatalf("--at 0 = live %v exit %v; want the replayed state, not ended", at["live"], at["exit"])
	}
}
