package mail

import (
	"os"
	"testing"
	"time"
)

func TestMailboxFold(t *testing.T) {
	box := Mailbox{Dir: t.TempDir()}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	err := box.Locked(func(tx *Tx) error {
		if err := tx.Enqueue(Message{V: 1, ID: "m1", From: "coord", To: "pi-1", Body: "a", CreatedAt: now}, now); err != nil {
			return err
		}
		if err := tx.Enqueue(Message{V: 1, ID: "m2", From: "coord", To: "pi-1", Body: "b", CreatedAt: now}, now); err != nil {
			return err
		}
		if err := tx.Mark("m1", StatusQueued, "not started", true, now.Add(time.Second)); err != nil {
			return err
		}
		return tx.Mark("m1", StatusSent, "ok", true, now.Add(time.Minute))
	})
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(box.logPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n{\"op\":\"status\",\"id\":\"nope\",\"status\":\"sent\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	var entries []Entry
	var skipped int
	err = box.Locked(func(tx *Tx) error {
		var err error
		entries, err = tx.Load()
		skipped = tx.Skipped
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Errorf("skipped %d lines, want 2", skipped)
	}
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	m1 := entries[0]
	if m1.Msg.ID != "m1" || m1.Status != StatusSent || m1.Attempts != 2 || !m1.LastAttempt.Equal(now.Add(time.Minute)) {
		t.Errorf("m1 = %+v", m1)
	}
	if entries[1].Msg.ID != "m2" || !entries[1].Pending() {
		t.Errorf("m2 = %+v", entries[1])
	}

	info, err := os.Stat(box.logPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mailbox mode %o, want no group or world bits", perm)
	}
}
