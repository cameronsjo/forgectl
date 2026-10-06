// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// signalRig is the two fakes a signal test reads back.
type signalRig struct {
	runner *exec.FakeRunner
	herdr  *exec.FakeSensitiveRunner
	deps   module.Deps
}

func newSignalRig(cfg config.DeskConfig) *signalRig {
	r := &signalRig{runner: &exec.FakeRunner{}, herdr: &exec.FakeSensitiveRunner{}}
	r.deps = module.Deps{Theme: theme.Default(), Cfg: config.Config{Desk: cfg}, Runner: r.runner, SensitiveRunner: r.herdr}
	return r
}

func (r *signalRig) osascript() int {
	n := 0
	for _, c := range r.runner.Calls {
		if c.Name == "osascript" {
			n++
		}
	}
	return n
}

func (r *signalRig) kinds() string {
	var ks []string
	for _, c := range r.herdr.Calls() {
		ks = append(ks, c.Kind.String())
	}
	return strings.Join(ks, " ")
}

func notOn() *bool { f := false; return &f }

func addItem(t *testing.T, rig *signalRig, name string) (string, error) {
	t.Helper()
	_, errOut, err := deskRun(t, rig.deps, "add", writeTemp(t, name+".sh", "echo "+name+"\n"), "--what", "do "+name, "--why", "y")
	return errOut, err
}

func paneAgent(verb exec.Arg, args ...exec.Arg) exec.SensitiveCommand {
	all := append([]exec.Arg{exec.MustFixed("pane"), verb}, args...)
	return exec.SensitiveCommand{Kind: exec.KindHerdrPaneAgent, Path: exec.Secret("/opt/test/bin/herdr"), Args: all, StdoutCap: 64 << 10, StderrCap: 64 << 10}
}

func releaseCmd(pane string) exec.SensitiveCommand {
	return paneAgent(exec.MustFixed("release-agent"), exec.Opaque(pane), exec.MustFixed("--source"), exec.MustFixed(herdr.DeskAgentSource), exec.MustFixed("--agent"), exec.MustFixed(herdr.DeskAgentSource))
}

func blockedCmd(pane, message string) exec.SensitiveCommand {
	return paneAgent(exec.MustFixed("report-agent"), exec.Opaque(pane), exec.MustFixed("--source"), exec.MustFixed(herdr.DeskAgentSource),
		exec.MustFixed("--agent"), exec.MustFixed(herdr.DeskAgentSource), exec.MustFixed("--state"), exec.MustFixed("blocked"),
		exec.MustFixed("--message"), exec.Opaque(message))
}

func queuedPane(t *testing.T, dir string) []string {
	t.Helper()
	d, err := desk.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // read only
	snap, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var panes []string
	for _, it := range snap.Pending {
		panes = append(panes, it.Meta.SignalPane)
	}
	return panes
}

func TestDeskAdd_SignalsTheOperator(t *testing.T) {
	dir := newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(config.DeskConfig{})
	if _, err := addItem(t, rig, "merge"); err != nil {
		t.Fatal(err)
	}
	if got := rig.kinds(); got != "herdr.notification-show herdr.pane-agent" {
		t.Errorf("herdr calls = %q, want a notification then the pane state", got)
	}
	calls := rig.herdr.Calls()
	if want := blockedCmd("w1:p9", "forgectl desk: 1 waiting: 01-merge: do merge"); !calls[1].Equal(want) {
		t.Errorf("the pane was not put in the blocked state naming the item")
	}
	if rig.osascript() != 1 {
		t.Errorf("%d macOS notifications, want 1", rig.osascript())
	}
	if got := queuedPane(t, dir); len(got) != 1 || got[0] != "w1:p9" {
		t.Errorf("the queued item remembers panes %v, want [w1:p9]", got)
	}
}

func TestDeskAdd_SettingsTurnEachSignalOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       config.DeskConfig
		wantMac   int
		wantHerdr string
	}{
		{"herdr off", config.DeskConfig{NotifyHerdr: notOn()}, 1, ""},
		{"macOS off", config.DeskConfig{NotifyMacOS: notOn()}, 0, "herdr.notification-show herdr.pane-agent"},
		{"both off", config.DeskConfig{NotifyHerdr: notOn(), NotifyMacOS: notOn()}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDeskDir(t)
			inHerdr(t, "w1:p9")
			rig := newSignalRig(tc.cfg)
			if _, err := addItem(t, rig, "merge"); err != nil {
				t.Fatal(err)
			}
			if rig.osascript() != tc.wantMac || rig.kinds() != tc.wantHerdr {
				t.Errorf("macOS=%d herdr=%q, want macOS=%d herdr=%q", rig.osascript(), rig.kinds(), tc.wantMac, tc.wantHerdr)
			}
			if tc.wantHerdr == "" {
				if got := queuedPane(t, dir); got[0] != "" {
					t.Errorf("herdr signal off, but the item remembers pane %q", got[0])
				}
			}
		})
	}
}

func TestDeskAdd_OutsideHerdrOnlyNotifiesMacOS(t *testing.T) {
	newDeskDir(t)
	stubLayout(t, map[string]string{})
	rig := newSignalRig(config.DeskConfig{})
	if _, err := addItem(t, rig, "merge"); err != nil {
		t.Fatal(err)
	}
	if rig.kinds() != "" || rig.osascript() != 1 {
		t.Errorf("herdr=%q macOS=%d, want no herdr call and one macOS notification", rig.kinds(), rig.osascript())
	}
}

func TestDeskAdd_AFailedSignalIsAWarningNotAFailedAdd(t *testing.T) {
	dir := newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(config.DeskConfig{})
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("herdr is down")
	}
	errOut, err := addItem(t, rig, "merge")
	wantExit(t, err, 0)
	if !strings.Contains(errOut, "warning: operator signal failed: herdr notification failed") {
		t.Errorf("stderr = %q, want the failed signal named", errOut)
	}
	if len(queuedPane(t, dir)) != 1 {
		t.Error("the item was not queued")
	}
}

// The pane state clears when the item leaves the queue, and stays, with the
// count refreshed, while another item from the same pane still waits.
func TestDeskSkip_ClearsThePaneStateWhenTheLastItemLeaves(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(config.DeskConfig{})
	for _, n := range []string{"one", "two"} {
		if _, err := addItem(t, rig, n); err != nil {
			t.Fatal(err)
		}
	}
	before := len(rig.herdr.Calls())

	if _, _, err := deskRun(t, rig.deps, "skip", "01-one", "--reason", "not now"); err != nil {
		t.Fatal(err)
	}
	calls := rig.herdr.Calls()[before:]
	if len(calls) != 1 || !calls[0].Equal(blockedCmd("w1:p9", "forgectl desk: 1 waiting")) {
		t.Fatalf("skipping one of two: %d calls, want the pane state refreshed to 1 waiting", len(calls))
	}
	if _, _, err := deskRun(t, rig.deps, "skip", "02-two", "--reason", "not now"); err != nil {
		t.Fatal(err)
	}
	calls = rig.herdr.Calls()[before+1:]
	if len(calls) != 1 || !calls[0].Equal(releaseCmd("w1:p9")) {
		t.Fatalf("skipping the last item: %d calls, want the pane state released", len(calls))
	}
}

func TestDeskSkip_LeavesAnItemWithNoSignalAlone(t *testing.T) {
	newDeskDir(t)
	stubLayout(t, map[string]string{})
	rig := newSignalRig(config.DeskConfig{})
	if _, err := addItem(t, rig, "one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := deskRun(t, rig.deps, "skip", "01-one", "--reason", "not now"); err != nil {
		t.Fatal(err)
	}
	if rig.kinds() != "" {
		t.Errorf("herdr calls = %q for an item queued outside herdr", rig.kinds())
	}
}

func TestDeskAdd_JSONCarriesTheSignalFailure(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(config.DeskConfig{})
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("herdr is down")
	}
	out, _, err := deskRun(t, rig.deps, "add", writeTemp(t, "a.sh", "echo a\n"), "--what", "w", "--why", "y", "--json")
	wantExit(t, err, 0)
	if !strings.Contains(out, "operator signal failed: herdr notification failed") || !strings.Contains(out, "notify_herdr = false") {
		t.Errorf("--json output = %s, want the failed signal and its setting in warnings", out)
	}
}

// Items from two panes, and one from outside herdr: skipping a pane's last
// item releases that pane only, and counts only that pane's items.
func TestDeskSkip_ReleasesOnlyTheItemsOwnPane(t *testing.T) {
	newDeskDir(t)
	rig := newSignalRig(config.DeskConfig{})
	for _, it := range []struct{ pane, name string }{{"w1:p1", "a"}, {"w1:p2", "b"}, {"", "c"}} {
		if it.pane == "" {
			stubLayout(t, map[string]string{})
		} else {
			inHerdr(t, it.pane)
		}
		if _, err := addItem(t, rig, it.name); err != nil {
			t.Fatal(err)
		}
	}
	before := len(rig.herdr.Calls())
	if _, _, err := deskRun(t, rig.deps, "skip", "01-a", "--reason", "x"); err != nil {
		t.Fatal(err)
	}
	calls := rig.herdr.Calls()[before:]
	if len(calls) != 1 || !calls[0].Equal(releaseCmd("w1:p1")) {
		t.Fatalf("skipping pane p1's only item: %d calls, want release of w1:p1 (p2 and the outside item still wait)", len(calls))
	}
}

// A signal raised while notify_herdr was on is still cleared after it is
// turned off; otherwise the pane stays blocked for good.
func TestDeskSkip_ClearsEvenWhenTheSettingIsNowOff(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(config.DeskConfig{})
	if _, err := addItem(t, rig, "one"); err != nil {
		t.Fatal(err)
	}
	before := len(rig.herdr.Calls())
	rig.deps.Cfg.Desk = config.DeskConfig{NotifyHerdr: notOn()}
	if _, _, err := deskRun(t, rig.deps, "skip", "01-one", "--reason", "x"); err != nil {
		t.Fatal(err)
	}
	if calls := rig.herdr.Calls()[before:]; len(calls) != 1 || !calls[0].Equal(releaseCmd("w1:p9")) {
		t.Fatalf("%d calls after the skip, want the release", len(calls))
	}
}
