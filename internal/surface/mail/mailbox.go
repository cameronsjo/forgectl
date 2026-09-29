package mail

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Status is where a message is in its life.
type Status string

const (
	// StatusQueued has not reached the recipient's harness yet.
	StatusQueued Status = "queued"
	// StatusSent was handed to the recipient's harness. Whether the agent has
	// read it is the harness's business.
	StatusSent Status = "sent"
	// StatusFailed will not be retried; Detail says why.
	StatusFailed Status = "failed"
	// StatusExpired stayed queued past the policy TTL.
	StatusExpired Status = "expired"
)

const (
	opMsg    = "msg"
	opStatus = "status"
	// maxRecord bounds one mailbox line. Policy caps a body well below it.
	maxRecord = 1 << 20
)

type record struct {
	Op      string    `json:"op"`
	At      time.Time `json:"at"`
	Msg     *Message  `json:"msg,omitempty"`
	ID      string    `json:"id,omitempty"`
	Status  Status    `json:"status,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Attempt bool      `json:"attempt,omitempty"`
}

// Entry is one message folded with every status recorded for it.
type Entry struct {
	Msg         Message
	Status      Status
	Detail      string
	Attempts    int
	LastAttempt time.Time
	UpdatedAt   time.Time
}

// Pending reports whether the entry still waits for delivery.
func (e Entry) Pending() bool { return e.Status == StatusQueued }

// ErrLockTimeout is returned when another forgectl holds the mailbox or roster
// lock for longer than the wait allows.
var ErrLockTimeout = errors.New("timed out waiting for the lock; another forgectl is delivering")

// Mailbox is mailbox.jsonl in a ledger directory.
type Mailbox struct {
	Dir string
}

func (b Mailbox) logPath() string  { return filepath.Join(b.Dir, "mailbox.jsonl") }
func (b Mailbox) lockPath() string { return filepath.Join(b.Dir, "mailbox.lock") }

// Locked runs fn holding the mailbox's exclusive lock, so no other forgectl
// process sends or flushes at the same time.
func (b Mailbox) Locked(fn func(tx *Tx) error) error {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(b.lockPath())
	if err != nil {
		return fmt.Errorf("mailbox lock: %w", err)
	}
	defer unlock()
	return fn(&Tx{box: b})
}

// Tx reads and appends while the mailbox lock is held.
type Tx struct {
	box Mailbox
	// Skipped counts lines the last Load could not use.
	Skipped int
}

// Load folds the log into one Entry per message, in the order sent.
func (t *Tx) Load() ([]Entry, error) {
	t.Skipped = 0
	f, err := os.Open(t.box.logPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var entries []Entry
	index := make(map[string]int)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecord)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Skipped++
			continue
		}
		switch r.Op {
		case opMsg:
			if r.Msg == nil || r.Msg.ID == "" {
				t.Skipped++
				continue
			}
			if _, dup := index[r.Msg.ID]; dup {
				t.Skipped++
				continue
			}
			index[r.Msg.ID] = len(entries)
			entries = append(entries, Entry{Msg: *r.Msg, Status: StatusQueued, UpdatedAt: r.At})
		case opStatus:
			i, ok := index[r.ID]
			if !ok {
				t.Skipped++
				continue
			}
			e := &entries[i]
			e.Status = r.Status
			e.Detail = r.Detail
			e.UpdatedAt = r.At
			if r.Attempt {
				e.Attempts++
				e.LastAttempt = r.At
			}
		default:
			t.Skipped++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mailbox %s: %w", t.box.logPath(), err)
	}
	return entries, nil
}

// Enqueue appends a new message as queued.
func (t *Tx) Enqueue(m Message, now time.Time) error {
	msg := m
	return t.append(record{Op: opMsg, At: now, Msg: &msg})
}

// Mark appends a status for a message. attempt says the status is the result
// of a delivery attempt, which counts toward backoff and the attempt cap.
func (t *Tx) Mark(id string, st Status, detail string, attempt bool, now time.Time) error {
	return t.append(record{Op: opStatus, At: now, ID: id, Status: st, Detail: detail, Attempt: attempt})
}

func (t *Tx) append(recs ...record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(t.box.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
