package pr

import (
	"bufio"
	"bytes"
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

// repairLogName is the write-ahead intent and audit trail every destructive
// session verb appends to — `pr repair --apply`, `pr teardown`, `pr cleanup`,
// and `pr findings cleanup --apply` — a sibling of the records it describes. The name is historical:
// it is kept as-is so existing trails stay readable, and each row says which
// verb wrote it.
//
// The .jsonl extension is load-bearing: List enumerates only .json names, so
// this file can never be mistaken for a session record.
const repairLogName = "repair.jsonl"

// maxRepairLogLineBytes bounds one row on the way back in. The file is written
// only by this package, but it is read back by `pr repair --history` and is
// hand-editable like every other file in a 0700 dir, so the reader treats it
// as input rather than as its own output.
const maxRepairLogLineBytes = 8 << 10

// MaxRepairHistoryRows bounds how many rows RepairHistory returns: the newest
// N are kept and the older ones are counted, not held. The log grows without
// bound between compactions, and a reader that materializes every row spends
// heap proportional to the file. At the writer's per-row ceiling this bound is
// about 16 MiB; a typical row is near 300 B. It is exported so a caller's test
// can seed past it.
const MaxRepairHistoryRows = 2000

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

// The destructive session verbs a row can name. A row's verb is WHICH COMMAND
// removed the thing, which is a different question from repair's Mode: Mode
// holds the flag spelling `pr repair --apply` was given, and is empty for every
// other verb. A new destructive verb adds a constant here and nothing else.
//
// A prepare-failure rollback — removing artifacts the SAME call created moments
// earlier (local.go's teardownLocalArtifacts, session.go's sandboxAndQuarantine
// teardown-on-failure) — is deliberately not a destructive verb and writes no
// row: nothing it removes was ever handed to the operator, so there is nothing
// a trail could help recover.
const (
	auditVerbRepair   = "repair"
	auditVerbTeardown = "teardown"
	auditVerbCleanup  = "cleanup"
	// auditVerbFindingsCleanup is `pr findings cleanup --apply`. Its subject is
	// a findings dir, not a session record: RecordPath names the dir, Detail
	// carries its size, and Ref, FromPhase, and Workspace stay empty.
	auditVerbFindingsCleanup = "findings-cleanup"
	// auditVerbPrune is the housekeeping sweep. It is its own verb rather than a
	// spelling of repair because it is the only one that UNLINKS: a reader
	// scanning the trail for what destroyed something needs to tell "a record
	// was renamed out of the way" from "a file is gone".
	auditVerbPrune = "prune"
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
	// Verb names the command that removed the thing. The omitempty is
	// deliberate: rows written before this field existed carry no verb, and an
	// absent verb must read as unknown rather than as a claim that a repair ran.
	Verb      string `json:"verb,omitempty"`
	Mode      string `json:"mode"`
	WindowID  string `json:"window_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Outcome   string `json:"outcome"`
	Error     string `json:"error,omitempty"`
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
	// Detail is the row's own summary of what it did, for a mode whose subject
	// is not a single record — compaction writes "dropped N rows, kept M" here,
	// because its RecordPath names the log rather than anything that was
	// removed.
	Detail string `json:"detail,omitempty"`
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
	return composeRepairActor(name, os.Getenv("CLAUDE_CODE_SESSION_ID"))
}

// composeRepairActor is repairActor's pure half: the two self-asserted halves
// joined and bounded.
//
// BOUNDED, because the session id is an environment value and the row it lands
// in has a line limit. An unbounded field here could push a row past that
// limit, and the one row that must never fail to write is the write-ahead
// intent — so the field that nothing reads to decide anything is the field that
// gets clipped.
//
// The cut lands on a RUNE boundary. A plain byte slice at maxActorBytes can
// split a multi-byte sequence, and json.Marshal then rewrites the orphan to
// U+FFFD — silently changing a field that is supposed to say who ran the
// command. A session id is an environment value, so multi-byte content in it is
// input, not a hypothetical.
func composeRepairActor(name, sessionID string) string {
	if sessionID != "" {
		name += " session=" + termsafe.SafeLine(sessionID)
	}
	return truncateString(name, maxActorBytes)
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

// shrinkableField is one variable-length string on a row, paired with the
// label the truncation note uses.
//
// Name is the Go FIELD NAME, and it is load-bearing: a structural test reflects
// over RepairRow and asserts every string field appears either here or in
// boundedRowFields, so a new field added without a sizing decision fails the
// build rather than silently reopening the class of defect this whole loop
// exists to close.
type shrinkableField struct {
	Name  string
	Label string
	Value *string
}

// shrinkableRowFields lists every variable-length string on a row, in the order
// they are given up.
//
// ORDER IS PROTECTION, ascending: the first entry is sacrificed first, the last
// survives longest. So the ranking is by what a reader needs at the floor —
// the record path names the row's subject, the workspace is the only pointer
// left to a clean room once the record is gone, and the actor is diagnostic
// that nothing reads to decide anything.
func shrinkableRowFields(row *RepairRow) []shrinkableField {
	return []shrinkableField{
		{"Record", "record", &row.Record},
		{"Detail", "detail", &row.Detail},
		{"Actor", "actor", &row.Actor},
		{"Ref", "ref", &row.Ref},
		{"Error", "error", &row.Error},
		{"WindowID", "window id", &row.WindowID},
		{"Workspace", "workspace", &row.Workspace},
		{"RecordPath", "record path", &row.RecordPath},
	}
}

// boundedRowFields names every string field on RepairRow that is NOT shrunk,
// with the bound that makes leaving it out safe. The structural test reads this
// map, so a bound asserted here is a claim the suite checks rather than a
// comment nobody revisits.
var boundedRowFields = map[string]string{
	"ID":         "16 hex characters from randomSuffix",
	"FromPhase":  "a package constant or repairPhaseUnreadable",
	"Verb":       "one of the auditVerb constants",
	"Mode":       "one of the RepairMode constants, or empty for every verb but repair",
	"Outcome":    "one of the repairOutcome constants",
	"RecordNote": "derived here from fixed templates and two decimal ints per clause, ASCII, and recomputed inside the measurement",
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
// The lesson that produced this shape: the defect returned twice, one field
// over each time — first through `Record`, then `Ref`, then `Workspace`. So the
// loop is written over the FIELD SET rather than over a hand-picked few, and
// the structural test above makes the set exhaustive by construction. Every
// string on the row is either shrunk here or listed in boundedRowFields with
// its bound.
//
// The remaining fields are fixed-width, constants, or derived here, which makes
// the final error unreachable. It is kept anyway: "unreachable" is a claim
// about today's fields, and a future one should fail loudly here rather than
// silently overflow the line.
func marshalRepairRow(row RepairRow) ([]byte, error) {
	fields := shrinkableRowFields(&row)
	original := make(map[string]int, len(fields))
	for _, f := range fields {
		original[f.Label] = len(*f.Value)
	}
	shrunk := map[string]bool{}
	for {
		row.RecordNote = shrinkNote(fields, original, shrunk)
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
			if *f.Value == "" {
				continue
			}
			*f.Value = halveString(*f.Value)
			shrunk[f.Label] = true
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
//
// It is recomputed at the top of each pass, BEFORE the marshal, so its own
// bytes are inside the measurement rather than added after it.
func shrinkNote(fields []shrinkableField, original map[string]int, shrunk map[string]bool) string {
	if len(shrunk) == 0 {
		return ""
	}
	notes := make([]string, 0, len(shrunk))
	for _, f := range fields {
		if !shrunk[f.Label] {
			continue
		}
		if len(*f.Value) == 0 {
			notes = append(notes, fmt.Sprintf("the %s's %d bytes did not fit this row and were elided", f.Label, original[f.Label]))
			continue
		}
		notes = append(notes, fmt.Sprintf("the %s was truncated from %d bytes to %d to fit this row",
			f.Label, original[f.Label], len(*f.Value)))
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

// repairLogFile is the seam appendRepairRow needs: read the last byte, append,
// roll back a partial append, and flush. Production is *os.File; a test drives
// a short-writing double through it, which is the only way to reach the
// truncation branch.
type repairLogFile interface {
	io.Writer
	io.ReaderAt
	Seek(offset int64, whence int) (int64, error)
	Truncate(size int64) error
	Sync() error
}

// appendRepairRow writes one complete row, or leaves the file exactly as it
// found it.
//
// A SHORT WRITE MUST NOT SURVIVE. The previous form returned the error but left
// the partial bytes in place, so the next append concatenated onto a truncated
// line and readRepairLogTail dropped the merged result as unparseable — costing
// BOTH rows. On an out-of-space tail that is the intent row naming a clean room,
// which is the one line the log exists to preserve. So the offset is captured
// first and the file is truncated back to it on any failure: the log loses the
// row it could not write, and nothing else.
//
// AN UNTERMINATED TAIL MUST NOT SWALLOW THE ROW. The log is hand-editable, so
// the file can end mid-line through no fault of this writer — a stray byte, an
// editor that drops the final newline — and appending straight onto it would
// merge the row into one undecodable line the reader then skips (forgectl#549).
// So when the last byte is not '\n' a separator goes first, in the SAME write
// as the row: a short write of either rolls both back to the captured offset,
// and the row itself, which marshalRepairRow capped, is unchanged. A last byte
// that cannot be read gets the separator too — an empty line is skipped by
// every reader, where a merged one costs a row.
//
// It is a var so a test can stage the crash this whole ordering exists for: an
// append that fails BETWEEN an intent and its completion, leaving the dangling
// row that says a mutation happened and was never closed out.
var appendRepairRow = func(f repairLogFile, data []byte) error {
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
	if start > 0 {
		// io.ReaderAt may pair a full read at the end of the input with io.EOF,
		// so n, not the error, says whether the byte was read.
		last := make([]byte, 1)
		if n, _ := f.ReadAt(last, start-1); n != 1 || last[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
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

// readRepairLogTail returns the newest limit decodable rows, oldest first, how
// many older decodable rows it displaced, and how many lines it skipped. A line
// that does not decode, or that exceeds maxRepairLogLineBytes, is skipped
// rather than failing the read: the history verb exists to answer "what
// happened", and one bad line must not take the rest of the record with it.
// omitted counts rows displaced by the limit, never skipped lines; skipped is
// returned rather than only logged, because the warning is discarded at the
// default log level and a dropped line must not pass in silence.
//
// Memory is bounded by limit rows plus one line buffer. The slice is NOT
// preallocated to limit, because a caller may pass math.MaxInt.
func (c *Client) readRepairLogTail(limit int) (rows []RepairRow, omitted, skipped int, err error) {
	if limit < 1 {
		limit = 1
	}
	f, err := os.Open(c.repairLogPath()) //nolint:gosec // inside the 0700 sessions dir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, fmt.Errorf("read repair audit log: %w", termsafe.Error(err))
	}
	defer func() { _ = f.Close() }()

	head := 0 // index of the oldest row once the ring is full
	keep := func(line []byte) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) == 0 {
			return
		}
		var row RepairRow
		if err := json.Unmarshal(line, &row); err != nil {
			skipped++
			return
		}
		if len(rows) < limit {
			rows = append(rows, row)
			return
		}
		rows[head] = row
		head = (head + 1) % limit
		omitted++
	}

	// The buffer holds exactly one maximum row: marshalRepairRow guarantees a
	// written row, newline included, is at most maxRepairLogLineBytes.
	r := bufio.NewReaderSize(f, maxRepairLogLineBytes)
	for {
		line, rerr := r.ReadSlice('\n')
		if errors.Is(rerr, bufio.ErrBufferFull) {
			// Over-long: drain the remainder of the line without keeping it.
			for errors.Is(rerr, bufio.ErrBufferFull) {
				_, rerr = r.ReadSlice('\n')
			}
			skipped++
			if rerr == nil {
				continue
			}
			line = nil
		}
		if rerr == nil {
			keep(line)
			continue
		}
		if errors.Is(rerr, io.EOF) {
			keep(line) // a final line with no trailing newline
			break
		}
		return nil, 0, 0, fmt.Errorf("read repair audit log: %w", rerr)
	}
	if head > 0 {
		rows = append(rows[head:], rows[:head]...)
	}
	if skipped > 0 {
		attrs := []any{"skipped", skipped, "path", c.repairLogPath()}
		if omitted > 0 {
			attrs = append(attrs, "omitted", omitted)
		}
		slog.Warn("Skipped unreadable rows in the pr repair audit log.", attrs...)
	}
	return rows, omitted, skipped, nil
}

// RepairTrail is the bounded result of RepairHistory.
type RepairTrail struct {
	// Rows is the newest rows, oldest first.
	Rows []RepairRow
	// Omitted counts older decodable rows the bound left out.
	Omitted int
	// Skipped counts lines that did not decode or were over-long. They are
	// still in the file — nothing on the read path drops them — but they are
	// not in Rows, so a caller must say so rather than show a gapless trail.
	Skipped int
	// Path is the log file, so a caller can say where the rest lives.
	Path string
}

// RepairHistory returns the newest MaxRepairHistoryRows rows of the repair
// audit trail, oldest first, under the lifecycle lock so it never reads a row
// mid-append. It only reads under the lock; callers print after it returns.
func (c *Client) RepairHistory(ctx context.Context) (RepairTrail, error) {
	trail := RepairTrail{Path: c.repairLogPath()}
	err := c.withLifecycleLock(ctx, "repair-history", func() error {
		var rerr error
		trail.Rows, trail.Omitted, trail.Skipped, rerr = c.readRepairLogTail(MaxRepairHistoryRows)
		return rerr
	})
	return trail, err
}
