// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
	mac    int // macOS notifications requested
}

// newSignalRig stubs the macOS notification seam: the real client is a no-op
// off darwin, so counting its osascript calls would pass or fail by host.
func newSignalRig(t *testing.T, cfg config.DeskConfig) *signalRig {
	t.Helper()
	r := &signalRig{runner: &exec.FakeRunner{}, herdr: &exec.FakeSensitiveRunner{}}
	prev, prevSupported := deskMacNotify, deskMacSupported
	deskMacNotify = func(context.Context, module.Deps, string, string) error { r.mac++; return nil }
	// The stub stands in for darwin, where the notification posts; a test of
	// the non-darwin case sets deskMacSupported itself.
	deskMacSupported = func(module.Deps) bool { return true }
	t.Cleanup(func() { deskMacNotify, deskMacSupported = prev, prevSupported })
	r.deps = module.Deps{Theme: theme.Default(), Cfg: config.Config{Desk: cfg}, Runner: r.runner, SensitiveRunner: r.herdr}
	return r
}

func (r *signalRig) osascript() int { return r.mac }

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
	rig := newSignalRig(t, config.DeskConfig{})
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
			rig := newSignalRig(t, tc.cfg)
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
	rig := newSignalRig(t, config.DeskConfig{})
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
	rig := newSignalRig(t, config.DeskConfig{})
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("herdr is down")
	}
	errOut, err := addItem(t, rig, "merge")
	wantExit(t, err, 0)
	if !strings.Contains(errOut, "warning: operator signal failed: herdr signal failed") {
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
	rig := newSignalRig(t, config.DeskConfig{})
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
	rig := newSignalRig(t, config.DeskConfig{})
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
	rig := newSignalRig(t, config.DeskConfig{})
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("herdr is down")
	}
	out, _, err := deskRun(t, rig.deps, "add", writeTemp(t, "a.sh", "echo a\n"), "--what", "w", "--why", "y", "--json")
	wantExit(t, err, 0)
	if !strings.Contains(out, "operator signal failed: herdr signal failed") || !strings.Contains(out, "notify_herdr = false") {
		t.Errorf("--json output = %s, want the failed signal and its setting in warnings", out)
	}
}

// Items from two panes, and one from outside herdr: skipping a pane's last
// item releases that pane only, and counts only that pane's items.
func TestDeskSkip_ReleasesOnlyTheItemsOwnPane(t *testing.T) {
	newDeskDir(t)
	rig := newSignalRig(t, config.DeskConfig{})
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
	rig := newSignalRig(t, config.DeskConfig{})
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

// A pending item edited after it was queued is skipped as changed by whichever
// verb scans next. Each verb that scans must clear the pane that item was
// blocking, not only the dashboard and skip.
func TestDeskVerbsThatScanClearAChangedItemsPane(t *testing.T) {
	for _, verb := range [][]string{{"status"}, {"runs"}} {
		t.Run(verb[0], func(t *testing.T) {
			dir := newDeskDir(t)
			inHerdr(t, "w1:p9")
			rig := newSignalRig(t, config.DeskConfig{})
			if _, err := addItem(t, rig, "one"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, desk.DirPending, "01-one.sh"), []byte("echo changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := len(rig.herdr.Calls())
			if _, _, err := deskRun(t, rig.deps, verb...); err != nil {
				t.Fatal(err)
			}
			if calls := rig.herdr.Calls()[before:]; len(calls) != 1 || !calls[0].Equal(releaseCmd("w1:p9")) {
				t.Fatalf("`desk %s` after the edit: %d herdr calls, want the release of w1:p9", verb[0], len(calls))
			}
		})
	}
}

func TestDeskAdd_FailureWarningIsOneLinePerSetting(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("herdr is down")
	}
	errOut, err := addItem(t, rig, "merge")
	wantExit(t, err, 0)
	if n := strings.Count(errOut, "notify_herdr = false"); n != 1 {
		t.Errorf("%d herdr warning lines, want 1:\n%s", n, errOut)
	}
}

// The claim and unskip wirings, pinned where they are made: a desk opened the
// way every verb opens it, driven through Claim, Skip and Unskip.
func TestOpenDeskDirFor_ClearsOnClaimAndReraisesOnUnskip(t *testing.T) {
	dir := newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	for _, n := range []string{"one", "two"} {
		if _, err := addItem(t, rig, n); err != nil {
			t.Fatal(err)
		}
	}
	d, err := openDeskDirFor(&cobra.Command{}, rig.deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test
	snap, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	before := len(rig.herdr.Calls())

	if err := d.Skip("01-one", desk.SkipOperator); err != nil {
		t.Fatal(err)
	}
	if err := d.Unskip("01-one"); err != nil {
		t.Fatal(err)
	}
	calls := rig.herdr.Calls()[before:]
	if len(calls) != 2 || !calls[0].Equal(blockedCmd("w1:p9", "forgectl desk: 1 waiting")) || !calls[1].Equal(blockedCmd("w1:p9", "forgectl desk: 2 waiting")) {
		t.Fatalf("skip then unskip: %d calls, want the count refreshed to 1 then back to 2", len(calls))
	}

	before = len(rig.herdr.Calls())
	for _, it := range snap.Pending {
		if _, err := d.Claim(it.Name, it.Meta.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	calls = rig.herdr.Calls()[before:]
	if len(calls) != 2 || !calls[1].Equal(releaseCmd("w1:p9")) {
		t.Fatalf("claiming both items: %d calls, want a refresh then the release of w1:p9", len(calls))
	}
}

// A herdr call that times out stops the rest for this process, so a hung herdr
// does not add a timeout to every claim of a "run all".
func TestOpenDeskDirFor_StopsCallingAHerdrThatTimedOut(t *testing.T) {
	dir := newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	for _, n := range []string{"one", "two", "three"} {
		if _, err := addItem(t, rig, n); err != nil {
			t.Fatal(err)
		}
	}
	d, err := openDeskDirFor(&cobra.Command{}, rig.deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test
	rig.herdr.RunFunc = func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		time.Sleep(2 * deskSignalTimeout) // the call outlives its context
		return exec.SensitiveResult{}, context.DeadlineExceeded
	}
	before := len(rig.herdr.Calls())
	for _, n := range []string{"01-one", "02-two"} {
		if err := d.Skip(n, desk.SkipOperator); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rig.herdr.Calls()) - before; n != 1 {
		t.Errorf("%d herdr calls after a failure, want 1 (the one that timed out)", n)
	}
}

// A refusal from herdr (the queuing pane closed since) says nothing about the
// next pane: the rest of the items are still cleared.
func TestOpenDeskDirFor_AClosedPaneDoesNotStopTheRest(t *testing.T) {
	dir := newDeskDir(t)
	rig := newSignalRig(t, config.DeskConfig{})
	for _, it := range []struct{ pane, name string }{{"w1:p1", "a"}, {"w1:p2", "b"}} {
		inHerdr(t, it.pane)
		if _, err := addItem(t, rig, it.name); err != nil {
			t.Fatal(err)
		}
	}
	d, err := openDeskDirFor(&cobra.Command{}, rig.deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test
	rig.herdr.RunFunc = func(c exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, errors.New("pane not found")
	}
	before := len(rig.herdr.Calls())
	for _, n := range []string{"01-a", "02-b"} {
		if err := d.Skip(n, desk.SkipOperator); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rig.herdr.Calls()) - before; n != 2 {
		t.Errorf("%d herdr calls, want 2: a closed pane must not stop the next pane's release", n)
	}
}

func TestOpenDeskDirFor_UnskipDoesNotReraiseWhenHerdrSignalIsOff(t *testing.T) {
	dir := newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	if _, err := addItem(t, rig, "one"); err != nil {
		t.Fatal(err)
	}
	rig.deps.Cfg.Desk = config.DeskConfig{NotifyHerdr: notOn()}
	d, err := openDeskDirFor(&cobra.Command{}, rig.deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test
	if err := d.Skip("01-one", desk.SkipOperator); err != nil {
		t.Fatal(err)
	}
	before := len(rig.herdr.Calls())
	if err := d.Unskip("01-one"); err != nil {
		t.Fatal(err)
	}
	if n := len(rig.herdr.Calls()) - before; n != 0 {
		t.Errorf("Unskip raised %d herdr calls with notify_herdr = false", n)
	}
}
