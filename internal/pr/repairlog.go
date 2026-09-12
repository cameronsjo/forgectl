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
	"strings"
	"time"
	"unicode/utf8"

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

// maxActorBytes bounds the actor field, whose session-id half comes from the
// environment. See repairActor for why an unbounded field here is a hazard.
const maxActorBytes = 256

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
	// RecordBytes is the subject record's UNTRUNCATED length, so a shrunken or
	// elided Record still says how much there was.
	RecordBytes int `json:"record_bytes,omitempty"`
	// RecordNote says so when any variable-length field on this row had to be
	// shrunk or dropped to fit the line. Silence means every field is whole.
	RecordNote string `json:"record_note,omitempty"`
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
	// BOUNDED, because the session id is an environment value and the row it
	// lands in has a line limit. An unbounded field here could push a row past
	// that limit, and the one row that must never fail to write is the
	// write-ahead intent — so the field that nothing reads to decide anything
	// is the field that gets clipped.
	if len(name) > maxActorBytes {
		name = name[:maxActorBytes]
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
	data, err := marshalRepairRow(row)
	if err != nil {
		return err
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

// marshalRepairRow encodes one newline-terminated row that FITS, by
// construction.
//
// THE SIZE MUST NEVER REFUSE THE WRITE. `beginRepairRow` treats a failed append
// as a refusal to mutate — correct, since a removal with no trail is the one
// shape that can lose a clean room — but that makes the row's own size a gate on
// the only verb that can clear an unreadable record. And the size is
// input-dependent in a way a byte cap cannot see: `termsafe.SafeLine` preserves
// every graphic rune, then `json.Marshal` doubles `"` and `\` and expands `<`,
// `>`, and `&` to six bytes each. A 4 KiB clamped payload of `<` marshals to
// 24 KiB, so capping a payload before encoding it measures the wrong thing.
//
// So every field whose length comes from outside is shrunk against the ENCODED
// length, in the order below — least identifying first, because what survives
// at the floor should be what names the file:
//
//	record      the subject's own bytes; the biggest and the most divisible
//	ref         read best-effort from a record nothing could decode, so it is
//	            charset-validated but NOT length-validated (ref.go bounds the
//	            alphabet, never the size)
//	error       the decode failure, which quotes the path and so inherits its
//	            length
//	record path last, because losing it costs the row its subject
//
// Anything still on the row after all four are empty is fixed-width (the
// timestamp, the 16-hex id), a constant (phase, mode, outcome), or bounded at
// its source (the actor, by maxActorBytes) — so the final error is unreachable
// rather than merely unlikely. It is kept because "unreachable" is a claim
// about today's fields, and a new unbounded one should fail loudly here rather
// than silently overflow the line.
func marshalRepairRow(row RepairRow) ([]byte, error) {
	original := map[string]int{
		"record": len(row.Record), "ref": len(row.Ref),
		"error": len(row.Error), "record path": len(row.RecordPath),
	}
	shrunk := map[string]bool{}
	// Ordered least-identifying first. Taking a pointer into the local copy is
	// what lets the loop rewrite a field without a switch per iteration.
	fields := []struct {
		name  string
		value *string
	}{
		{"record", &row.Record},
		{"ref", &row.Ref},
		{"error", &row.Error},
		{"record path", &row.RecordPath},
	}
	for {
		row.RecordNote = shrinkNote(original, shrunk, row)
		// termsafe:allow-raw-json persisted audit row, never command output
		data, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("encode repair audit row: %w", err)
		}
		data = append(data, '\n')
		if len(data) <= maxRepairLogLineBytes {
			return data, nil
		}
		trimmed := false
		for _, f := range fields {
			if *f.value == "" {
				continue
			}
			*f.value = halveString(*f.value)
			shrunk[f.name] = true
			trimmed = true
			break
		}
		if !trimmed {
			return nil, fmt.Errorf("repair audit row exceeds the %d-byte line limit with every variable-length field already dropped",
				maxRepairLogLineBytes)
		}
	}
}

// shrinkNote renders what the row had to give up, so a truncated value can
// never read as the whole one. A shrunken ref matters most: it stays
// charset-valid, so without this note it would look like a real, shorter ref.
func shrinkNote(original map[string]int, shrunk map[string]bool, row RepairRow) string {
	if len(shrunk) == 0 {
		return ""
	}
	now := map[string]int{
		"record": len(row.Record), "ref": len(row.Ref),
		"error": len(row.Error), "record path": len(row.RecordPath),
	}
	notes := make([]string, 0, len(shrunk))
	for _, name := range []string{"record", "ref", "error", "record path"} {
		if !shrunk[name] {
			continue
		}
		if now[name] == 0 {
			notes = append(notes, fmt.Sprintf("the %s's %d bytes did not fit this row and were elided", name, original[name]))
			continue
		}
		notes = append(notes, fmt.Sprintf("the %s was truncated from %d bytes to %d to fit this row",
			name, original[name], now[name]))
	}
	return strings.Join(notes, "; ")
}

// halveString halves s on a rune boundary. The payload is evidence a human
// reads, never parsed, so a clean boundary matters only so the row stays valid
// UTF-8 — which json.Marshal would otherwise paper over with replacement
// characters, quietly changing the bytes the trail is supposed to preserve.
//
// It always returns something strictly shorter for a non-empty input (len/2
// then walking DOWN to a rune start), which is what makes the loop above
// terminate.
func halveString(s string) string { return truncateString(s, len(s)/2) }

// truncateString cuts s to at most n bytes, walking down to a rune boundary.
func truncateString(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
