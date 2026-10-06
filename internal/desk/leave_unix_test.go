//go:build unix

package desk

import (
	"path/filepath"
	"testing"
)

// leftPaneRecorder registers an OnLeavePending hook that records the pane of
// every item that leaves pending/.
func leftPaneRecorder(d *Desk) *[]string {
	var left []string
	d.OnLeavePending(func(m Meta) { left = append(left, m.SignalPane) })
	return &left
}

func TestAddStampsTheSignalPaneIntoTheItemMeta(t *testing.T) {
	d := openDesk(t)
	d.SetSignalPane("w1:p9")
	a := addScript(t, d, "x.sh", "echo hi\n")
	m, ok, err := d.readMeta(DirPending, a.Name)
	if err != nil || !ok || m.SignalPane != "w1:p9" {
		t.Fatalf("meta = %+v ok=%v err=%v, want signal_pane w1:p9", m, ok, err)
	}
}

// A claim, a changed-bytes claim, and a skip all take an item out of pending/;
// each must tell the hook, or the operator signal raised at queue time never
// clears.
func TestOnLeavePendingFiresForClaimChangedAndSkip(t *testing.T) {
	d := openDesk(t)
	d.SetSignalPane("w1:p9")
	left := leftPaneRecorder(d)

	ran := addScript(t, d, "ran.sh", "echo hi\n")
	if _, err := d.Claim(ran.Name, ran.SHA256); err != nil {
		t.Fatal(err)
	}
	skipped := addScript(t, d, "skipped.sh", "echo hi\n")
	if err := d.Skip(skipped.Name, SkipOperator); err != nil {
		t.Fatal(err)
	}
	if got := len(*left); got != 2 {
		t.Fatalf("hook fired %d times after a claim and a skip, want 2", got)
	}
	for _, p := range *left {
		if p != "w1:p9" {
			t.Errorf("hook got pane %q, want w1:p9", p)
		}
	}
}

func TestOnLeavePendingIsQuietWhenTheItemStaysPending(t *testing.T) {
	d := openDesk(t)
	left := leftPaneRecorder(d)
	a := addScript(t, d, "x.sh", "echo hi\n")
	if _, err := d.Claim(a.Name, SHA256Hex([]byte("wrong"))); err == nil {
		t.Fatal("a wrong-hash claim succeeded")
	}
	if len(*left) != 0 {
		t.Errorf("hook fired for a claim that left the item pending: %v", *left)
	}
}

// Scan moves an item whose bytes changed out of pending/; the signal raised
// for it must clear then too, or nothing is waiting and the pane stays blocked.
func TestOnLeavePendingFiresWhenScanSkipsAChangedItem(t *testing.T) {
	d := openDesk(t)
	d.SetSignalPane("w1:p9")
	left := leftPaneRecorder(d)
	a := addScript(t, d, "x.sh", "echo hi\n")
	scan(t, d)
	writeFile(t, filepath.Join(d.Path(), DirPending, a.Name+".sh"), "echo changed\n", 0o600)
	scan(t, d)
	if len(*left) != 1 || (*left)[0] != "w1:p9" {
		t.Fatalf("hook calls = %v, want one for w1:p9", *left)
	}
}

func TestOnReturnPendingFiresOnUnskip(t *testing.T) {
	d := openDesk(t)
	d.SetSignalPane("w1:p9")
	var back []string
	d.OnReturnPending(func(m Meta) { back = append(back, m.SignalPane) })
	a := addScript(t, d, "x.sh", "echo hi\n")
	if err := d.Skip(a.Name, SkipOperator); err != nil {
		t.Fatal(err)
	}
	if err := d.Unskip(a.Name); err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0] != "w1:p9" {
		t.Fatalf("return hook calls = %v, want one for w1:p9", back)
	}
}
