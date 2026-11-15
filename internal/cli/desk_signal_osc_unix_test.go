// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/progstatus"
)

const (
	oscBlockedPermission = "\x1b]7501;state=blocked:id=forgectl:app=forgectl:kind=permission:msg="
	oscClear             = "\x1b]7501;state=clear:id=forgectl:app=forgectl\x1b\\"
)

// oscMsg is the base64 msg a report of n waiting items carries.
func oscMsg(n string) string { return base64.StdEncoding.EncodeToString([]byte(n + " waiting")) }

func TestWaitingRow_ReportsCountThenClears(t *testing.T) {
	stubLayout(t, map[string]string{}) // not in a herdr session
	rig := newSignalRig(t, config.DeskConfig{})
	var buf bytes.Buffer
	onWaiting, release := newDeskSignal(rig.deps).waitingRow(progstatus.ForWriter(&buf, false))
	if onWaiting == nil {
		t.Fatal("no hook outside herdr")
	}

	onWaiting(0)
	if buf.Len() != 0 {
		t.Fatalf("an empty queue at start wrote %q, want nothing", buf.String())
	}
	onWaiting(3)
	if want := oscBlockedPermission + oscMsg("3") + "\x1b\\"; buf.String() != want {
		t.Errorf("3 waiting wrote %q, want %q", buf.String(), want)
	}
	buf.Reset()
	onWaiting(1)
	if want := oscBlockedPermission + oscMsg("1") + "\x1b\\"; buf.String() != want {
		t.Errorf("1 waiting wrote %q, want %q", buf.String(), want)
	}
	buf.Reset()
	onWaiting(0)
	if buf.String() != oscClear {
		t.Errorf("0 waiting wrote %q, want %q", buf.String(), oscClear)
	}
	buf.Reset()
	release() // already clear: nothing to remove
	if buf.Len() != 0 {
		t.Errorf("release after a clear wrote %q", buf.String())
	}
}

// A dashboard that exits with items waiting must take its row with it.
func TestWaitingRow_ReleaseClearsARaisedRow(t *testing.T) {
	stubLayout(t, map[string]string{})
	rig := newSignalRig(t, config.DeskConfig{})
	var buf bytes.Buffer
	onWaiting, release := newDeskSignal(rig.deps).waitingRow(progstatus.ForWriter(&buf, false))
	onWaiting(2)
	buf.Reset()
	release()
	if buf.String() != oscClear {
		t.Errorf("release wrote %q, want %q", buf.String(), oscClear)
	}
	buf.Reset()
	release() // twice is harmless
	if buf.Len() != 0 {
		t.Errorf("a second release wrote %q", buf.String())
	}
}

func TestWaitingRow_HerdrSessionReportsNothingNew(t *testing.T) {
	inHerdr(t, "")
	rig := newSignalRig(t, config.DeskConfig{})
	var buf bytes.Buffer
	onWaiting, release := newDeskSignal(rig.deps).waitingRow(progstatus.ForWriter(&buf, false))
	if onWaiting != nil {
		onWaiting(3)
	}
	release()
	if buf.Len() != 0 {
		t.Errorf("inside a herdr session wrote %q, want nothing (herdr's own path signals)", buf.String())
	}
}

func TestWaitingRow_OptOutAndNoTerminalReportNothing(t *testing.T) {
	stubLayout(t, map[string]string{})
	rig := newSignalRig(t, config.DeskConfig{})
	// A nil file is the disabled Reporter New returns when a gate (no stderr
	// terminal, TERM=dumb, FORGECTL_NO_PROGRAM_STATUS, a worker pane) closes;
	// the gates themselves are tested in internal/progstatus.
	r := progstatus.New(nil, func(k string) string {
		if k == progstatus.EnvOptOut {
			return "1"
		}
		return ""
	})
	onWaiting, release := newDeskSignal(rig.deps).waitingRow(r)
	if onWaiting != nil {
		t.Error("a disabled reporter got a hook")
	}
	release()
}
