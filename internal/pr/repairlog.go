package pr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// repairLogName is the write-ahead intent and audit trail every `pr repair
// --apply` appends to, a sibling of the records it describes.
//
// The .jsonl extension is load-bearing: List enumerates only .json names, so
// this file can never be mistaken for a session record.
const repairLogName = "repair.jsonl"

// maxRepairLogLineBytes bounds one row on the way back in. The file is written
// only by this package, but it is read back by `pr repair --history` and is
// hand-editable like every other file in a 0700 dir, so the reader treats it
// as input rather than as its own output.
const maxRepairLogLineBytes = 8 << 10

// Repair outcomes recorded in the log. The intent value is the one that
// matters operationally: a row carrying it with no completion beside it is a
// rollback that died mid-way, and its workspace field is the only pointer left
// to the clean room.
const (
	repairOutcomeIntent  = "intent"
	repairOutcomeApplied = "applied"
	repairOutcomeFailed  = "failed"
)

// RepairRow is one line of the repair log — the intent written BEFORE a
// mutation, or the completion written after it. The two are paired by ID.
type RepairRow struct {
	TS         time.Time `json:"ts"`
	ID         string    `json:"id"`
	Actor      string    `json:"actor"`
	Ref        string    `json:"ref"`
	RecordPath string    `json:"record_path"`
	FromPhase  string    `json:"from_phase"`
	Mode       string    `json:"mode"`
	WindowID   string    `json:"window_id,omitempty"`
	Workspace  string    `json:"workspace,omitempty"`
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error,omitempty"`
	// Record carries the subject record's own bytes, capped and clamped, and is
	// written for ONE case: a record this build cannot decode. Every other field
	// here is derived from a decode, so for that case they are all empty — and a
	// trail that names nothing is worthless for the one removal that cannot say
	// what it removed.
	Record string `json:"record,omitempty"`
}

// repairLogPath is the log's location inside the client's sessions dir.
func (c *Client) repairLogPath() string { return filepath.Join(c.sessionsDir, repairLogName) }

// repairActor names who ran the repair: the OS user, plus the harness session
// id when one is set, so a row written by an agent is distinguishable from one
// a human typed.
//
// It is DIAGNOSTIC, never authority. Both halves are self-asserted — the
// environment variable is writable by anyone who can run the command — so
// nothing reads this field to decide anything.
func repairActor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		name += " session=" + termsafe.SafeLine(sid)
	}
	return name
}

// appendRepairRowLocked appends one row, fsynced, to the repair log. The
// caller must already hold the lifecycle lock — every writer of this file is
// inside a repair hold, which is what makes an append-with-no-locking-of-its-
// own correct.
//
// It never drops a row on contention and it never truncates: an audit trail
// that fails quietly is worse than none, because its silence reads as "nothing
// happened".
func (c *Client) appendRepairRowLocked(row RepairRow) error {
	if err := os.MkdirAll(c.sessionsDir, 0o700); err != nil {
		return fmt.Errorf("create pr sessions dir: %w", err)
	}
	// termsafe:allow-raw-json persisted audit row, never command output
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode repair audit row: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxRepairLogLineBytes {
		return fmt.Errorf("repair audit row exceeds the %d-byte line limit", maxRepairLogLineBytes)
	}
	f, err := os.OpenFile(c.repairLogPath(), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // inside the 0700 sessions dir
	if err != nil {
		return fmt.Errorf("open repair audit log: %w", termsafe.Error(err))
	}
	writeErr := appendRepairRow(f, data)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write repair audit row: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close repair audit log: %w", closeErr)
	}
	return nil
}

// repairLogFile is the seam appendRepairRow needs: append, roll back a partial
// append, and flush. Production is *os.File; a test drives a short-writing
// double through it, which is the only way to reach the truncation branch.
type repairLogFile interface {
	io.Writer
	Seek(offset int64, whence int) (int64, error)
	Truncate(size int64) error
	Sync() error
}

// appendRepairRow writes one complete row, or leaves the file exactly as it
// found it.
//
// A SHORT WRITE MUST NOT SURVIVE. The previous form returned the error but left
// the partial bytes in place, so the next append concatenated onto a truncated
// line and readRepairLog dropped the merged result as unparseable — costing BOTH
// rows. On an out-of-space tail that is the intent row naming a clean room, which
// is the one line the log exists to preserve. So the offset is captured first and
// the file is truncated back to it on any failure: the log loses the row it could
// not write, and nothing else.
func appendRepairRow(f repairLogFile, data []byte) error {
	start, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("locate the end of the repair audit log: %w", err)
	}
	rollback := func(cause error) error {
		if terr := f.Truncate(start); terr != nil {
			return fmt.Errorf("%w (and the partial row could not be rolled back: %v)", cause, terr)
		}
		return cause
	}
	n, err := f.Write(data)
	if err != nil {
		return rollback(fmt.Errorf("append repair audit row: %w", err))
	}
	if n != len(data) {
		return rollback(fmt.Errorf("append repair audit row: %w (wrote %d of %d bytes)", io.ErrShortWrite, n, len(data)))
	}
	if err := f.Sync(); err != nil {
		return rollback(fmt.Errorf("flush repair audit row: %w", err))
	}
	return nil
}

// readRepairLog returns every row in the log, oldest first. A line that does
// not decode is skipped with a warning rather than failing the read: the
// history verb exists to answer "what happened", and one bad line must not
// take the rest of the record with it.
func (c *Client) readRepairLog() ([]RepairRow, error) {
	f, err := os.Open(c.repairLogPath()) //nolint:gosec // inside the 0700 sessions dir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read repair audit log: %w", termsafe.Error(err))
	}
	defer func() { _ = f.Close() }()

	var rows []RepairRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), maxRepairLogLineBytes)
	skipped := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row RepairRow
		if err := json.Unmarshal(line, &row); err != nil {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read repair audit log: %w", err)
	}
	if skipped > 0 {
		slog.Warn("Skipped unreadable rows in the pr repair audit log.", "skipped", skipped, "path", c.repairLogPath())
	}
	return rows, nil
}

// RepairHistory returns the repair audit trail, oldest first, under the
// lifecycle lock so it never reads a row mid-append.
func (c *Client) RepairHistory(ctx context.Context) ([]RepairRow, error) {
	var rows []RepairRow
	err := c.withLifecycleLock(ctx, "repair-history", func() error {
		var rerr error
		rows, rerr = c.readRepairLog()
		return rerr
	})
	return rows, err
}
