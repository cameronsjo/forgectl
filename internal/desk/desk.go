// Package desk is the operator queue behind `forgectl desk`: scripts Claude
// stages but will not run itself, which the operator approves and runs.
//
// # Protocol
//
// The desk directory holds four protocol subdirectories and nothing else this
// package touches:
//
//	pending/NN-name.sh        a script, with "# WHAT:", "# WHY:" and optional "# TTY: yes" lines
//	pending/NN-name.manifest  a batch manifest; the kind comes from the extension
//	running/<name>.sh|.manifest
//	done/<name>.sh + done/<name>.log   the log's last line is EXIT=<rc>
//	done/<name>.manifest + .log + .d/  a finished batch
//	done/<name>.events        RUN-START ... RUN-END event lines
//	skipped/<name>.sh|.manifest
//
// Every item carries a sidecar, <name>.meta.json, that moves with it.
//
// # Unchanged since queued
//
// An item's sha256 is fixed once: at [Desk.Add], or the first time a desk
// sees a hand-dropped item. If its bytes change after that it moves to
// skipped/ with skip_reason "changed" and cannot be re-armed. A run reads the
// bytes once and checks the full hash. It keeps those exact bytes in a fresh
// file in running/ as the record, and bash reads the same bytes from a pipe,
// never from that file. The hash covers the item's own bytes only, not
// anything the script sources, calls, or downloads.
//
// # Threat model
//
// The desk defends against accidents: a script edited after it was queued,
// an item run twice by two desks, hostile text in a header. It does not
// defend against a same-uid process that can already write the directory or
// press keys in the desk's terminal.
package desk

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Protocol subdirectories. Nothing else at the desk root is ever touched.
const (
	DirPending = "pending"
	DirRunning = "running"
	DirDone    = "done"
	DirSkipped = "skipped"
)

var protocolDirs = [...]string{DirPending, DirRunning, DirDone, DirSkipped}

const (
	extScript   = ".sh"
	extManifest = ".manifest"
	extLog      = ".log"
	extEvents   = ".events"
	extMeta     = ".meta.json"
	extBatchDir = ".d"
)

// ClaimGrace is how long a claimed item may sit in running/ with no owner
// recorded before it counts as lost. A healthy run records its owner within
// moments of the claim; past this, the claimant died or failed between the
// claim and BeginRun, and only Skip(name, SkipLost) moves the item on.
const ClaimGrace = 60 * time.Second

// StaleAfter is how long an item may wait before the queue flags it stale.
// Nothing removes a stale item; the operator skips it.
const StaleAfter = 24 * time.Hour

// maxItemBytes caps what a desk reads from a pending item. A queued script
// larger than this is refused, not truncated.
const maxItemBytes = 1 << 20

// Skip reasons recorded in meta.
const (
	SkipChanged  = "changed"
	SkipOperator = "operator"
	SkipLost     = "lost"
	SkipReused   = "name-reused"
	// SkipLaunchFailed is a claimed item whose run never began: its
	// supervisor did not start, or BeginRun refused it.
	SkipLaunchFailed = "launch-failed"
)

// Errors callers branch on.
var (
	// ErrChanged reports an item whose bytes no longer match the hash fixed
	// when it was queued. The item has been moved to skipped/.
	ErrChanged = errors.New("desk: item changed since it was queued")
	// ErrClaimed reports that another desk claimed the item first.
	ErrClaimed = errors.New("desk: item already claimed")
	// ErrRefused reports an item that is not a regular file with one link,
	// or is too large to read.
	ErrRefused = errors.New("desk: item refused")
	// ErrNotFound reports an item absent from the directory asked about.
	ErrNotFound = errors.New("desk: no such item")
	// ErrUnsupported reports a platform without the primitives the desk needs.
	ErrUnsupported = errors.New("desk: not supported on this platform")
)

// Kind is an item's kind, chosen by its file extension.
type Kind string

const (
	KindScript Kind = "script"
	KindBatch  Kind = "batch"
)

// Ext is the kind's file extension.
func (k Kind) Ext() string {
	if k == KindBatch {
		return extManifest
	}
	return extScript
}

// kindOfFile returns the kind and the item name for a protocol file name, or
// ok=false when the name is not an item file.
func kindOfFile(file string) (name string, kind Kind, ok bool) {
	if strings.HasPrefix(file, ".") {
		return "", "", false
	}
	switch {
	case strings.HasSuffix(file, extScript):
		name, kind = strings.TrimSuffix(file, extScript), KindScript
	case strings.HasSuffix(file, extManifest):
		name, kind = strings.TrimSuffix(file, extManifest), KindBatch
	default:
		return "", "", false
	}
	return name, kind, ValidName(name)
}

var (
	nameRe = regexp.MustCompile(`^[0-9]+-[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	// legacyNameRe is the display-only shape of an old done/ log's name:
	// "37b-cleanup" or "operator-grow", with no NN- number.
	legacyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	stemRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a full sha256 as the desk writes it: 64
// lowercase hex characters. A meta file is untrusted input; a hash of any
// other shape is never shown or compared.
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// ValidName reports whether name is a protocol item name: "NN-stem".
func ValidName(name string) bool {
	return nameRe.MatchString(name) && !strings.Contains(name, "..")
}

// legacyName reports a done/ log name that is not a protocol name but is
// safe to show and to prune: letters, digits, '.', '_', '-', no "..".
// Display only: nothing runs, skips, claims or watches it.
func legacyName(name string) bool {
	return !ValidName(name) && legacyNameRe.MatchString(name) && !strings.Contains(name, "..")
}

// SplitName returns an item name's number and stem: "17-merge" -> 17, "merge".
func SplitName(name string) (int, string) {
	num, stem, _ := strings.Cut(name, "-")
	n, err := strconv.Atoi(num)
	if err != nil {
		return -1, name
	}
	return n, stem
}

// Headers are an item's "# WHAT:", "# WHY:" and "# TTY:" comment lines. The
// first occurrence of each wins.
type Headers struct {
	What string
	Why  string
	TTY  bool
}

// HeadersFor is [ParseHeaders] for an item of kind. "# TTY: yes" means
// something only in a script: a batch never runs in a terminal, and its
// manifest must never reach bash as a script, so a manifest's TTY line is
// ignored.
func HeadersFor(kind Kind, data []byte) Headers {
	h := ParseHeaders(data)
	if kind != KindScript {
		h.TTY = false
	}
	return h
}

// ParseHeaders reads the header lines from an item's bytes. Values are raw
// script text: render them through termsafe before they reach a terminal.
func ParseHeaders(data []byte) Headers {
	var h Headers
	var seenWhat, seenWhy, seenTTY bool
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		key, value, ok := headerLine(line)
		if !ok {
			continue
		}
		switch {
		case key == "WHAT" && !seenWhat:
			h.What, seenWhat = value, true
		case key == "WHY" && !seenWhy:
			h.Why, seenWhy = value, true
		case key == "TTY" && !seenTTY:
			h.TTY, seenTTY = value == "yes", true
		}
	}
	return h
}

// headerLine matches "# KEY: value" (spaces after the colon are dropped).
func headerLine(line string) (key, value string, ok bool) {
	rest, found := strings.CutPrefix(line, "# ")
	if !found {
		return "", "", false
	}
	for _, k := range [...]string{"WHAT", "WHY", "TTY"} {
		if v, found := strings.CutPrefix(rest, k+":"); found {
			return k, strings.TrimLeft(v, " "), true
		}
	}
	return "", "", false
}

// insertHeaders returns body with header lines for what, why and tty added
// after a leading shebang (or at the top). A header the body already carries
// may not be given again; the result must carry both WHAT and WHY.
func insertHeaders(body []byte, kind Kind, what, why string, tty bool) ([]byte, error) {
	for _, f := range [...]struct{ label, v string }{{"--what", what}, {"--why", why}} {
		if strings.ContainsAny(f.v, "\r\n") {
			return nil, fmt.Errorf("desk: %s must be one line", f.label)
		}
	}
	if tty && kind != KindScript {
		return nil, errors.New("desk: a batch manifest cannot be a TTY item (steps never get a terminal)")
	}
	have := ParseHeaders(body)
	var lines []string
	if what != "" {
		if have.What != "" {
			return nil, errors.New("desk: the file already has a '# WHAT:' line; drop --what or the line")
		}
		lines = append(lines, "# WHAT: "+what)
	}
	if why != "" {
		if have.Why != "" {
			return nil, errors.New("desk: the file already has a '# WHY:' line; drop --why or the line")
		}
		lines = append(lines, "# WHY: "+why)
	}
	if tty && !have.TTY {
		lines = append(lines, "# TTY: yes")
	}
	if (have.What == "" && what == "") || (have.Why == "" && why == "") {
		return nil, errors.New("desk: an item needs both WHAT and WHY (flags or '# WHAT:'/'# WHY:' lines)")
	}
	if len(lines) == 0 {
		return body, nil
	}
	block := strings.Join(lines, "\n") + "\n"
	if kind == KindScript && strings.HasPrefix(string(body), "#!") {
		first, rest, found := strings.Cut(string(body), "\n")
		if !found {
			return []byte(first + "\n" + block), nil
		}
		return []byte(first + "\n" + block + rest), nil
	}
	return []byte(block + string(body)), nil
}

// ResolveDir picks the desk directory: $DESK_DIR, then $CLAUDE_DESK_DIR, then
// $XDG_STATE_HOME/forgectl/desk, then ~/.local/state/forgectl/desk. A relative
// DESK_DIR or CLAUDE_DESK_DIR is refused; a relative XDG_STATE_HOME is ignored,
// as the XDG spec requires.
func ResolveDir(getenv func(string) string, home string) (string, error) {
	for _, key := range [...]string{"DESK_DIR", "CLAUDE_DESK_DIR"} {
		if v := getenv(key); v != "" {
			if !filepath.IsAbs(v) {
				return "", fmt.Errorf("desk: %s must be an absolute path", key)
			}
			return filepath.Clean(v), nil
		}
	}
	if v := getenv("XDG_STATE_HOME"); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "forgectl", "desk"), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("desk: no home directory; set DESK_DIR")
	}
	return filepath.Join(home, ".local", "state", "forgectl", "desk"), nil
}

// Meta is an item's sidecar, <name>.meta.json. Times are UTC. A legacy item
// has none; see [Item] for the fallbacks.
type Meta struct {
	AddedAt    *time.Time `json:"added_at,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Kind       Kind       `json:"kind,omitempty"`
	SkipReason string     `json:"skip_reason,omitempty"`
	// ChangedSHA256 is the hash of the bytes the desk found when it skipped
	// the item as changed, beside SHA256, the hash it was queued at. Shown
	// only after ValidSHA256, like every hash read back from meta.
	ChangedSHA256 string `json:"changed_sha256,omitempty"`
	// SkipNote is the operator's own one-line reason, from `desk skip
	// --reason`. Untrusted text: render it through termsafe.
	SkipNote string `json:"skip_note,omitempty"`
	// SkippedBy is who skipped the item: "dashboard" or "cli" (see
	// SkippedByDashboard). Empty for a skip the desk made itself (changed,
	// refused, name-reused) and for skips before the field existed.
	SkippedBy string `json:"skipped_by,omitempty"`
	// SkippedAt is when a dashboard or CLI skip happened (UTC).
	SkippedAt *time.Time `json:"skipped_at,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// ExitCode is the run's rc, recorded at Finish. It outranks the log's
	// EXIT= line, which a leftover process could append to after the run.
	ExitCode *int `json:"exit_code,omitempty"`
	// ClaimedAt is when a desk claimed the item into running/. A run with no
	// owner (PID 0) longer than ClaimGrace after it is lost.
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// PID and PIDStart identify the process that owns a run: the supervisor,
	// or the desk itself for a TTY item. PIDStart is the process start time
	// in a platform-specific unit, so a reused pid does not read as alive.
	PID      int   `json:"pid,omitempty"`
	PIDStart int64 `json:"pid_start,omitempty"`
	// SignalPane is the herdr pane of the session that queued the item, kept
	// so the operator signal raised for it can be cleared when the item runs
	// or is skipped. Untrusted text on read: validate before it reaches an
	// argv.
	SignalPane string `json:"signal_pane,omitempty"`
	// SignalledAt is when `desk add` finished telling the operator the item
	// is waiting, with every enabled signal sent. Absent when the add died
	// before signalling or a signal failed, so a retry that finds the item
	// sends it again. Not read by the dashboard.
	SignalledAt *time.Time `json:"signalled_at,omitempty"`
}

// State is where an item stands.
type State string

const (
	StateWaiting State = "waiting"
	StateRefused State = "refused" // pending, but not a regular file with one link, or too large
	StateRunning State = "running"
	StateLost    State = "lost" // running, owner dead, no RUN-END
	StateDone    State = "done"
	StateSkipped State = "skipped"
)

// Item is one queue entry as a scan saw it.
type Item struct {
	Name   string
	Number int
	Stem   string
	Kind   Kind
	State  State
	Headers
	Meta Meta
	// Content is a pending item's bytes, as hashed (nil for other states).
	Content []byte
	// Stale is set on a waiting item older than [StaleAfter].
	Stale bool
	// Legacy marks a done item whose name is not a protocol name (no NN-
	// number): an old log kept for history. It is shown and pruned, never
	// acted on; every action path checks [ValidName] and refuses it.
	Legacy bool
	// Refusal says why a refused item cannot run.
	Refusal string
	// ExitCode is a done item's rc: from meta, or for a legacy item the log's
	// last line, EXIT=<rc>. nil means no exit was recorded.
	ExitCode *int
	// Started and Ended come from meta, or for a legacy item from the log's
	// birth and modification times. Zero means unknown.
	Started time.Time
	Ended   time.Time
}

// Snapshot is one scan of the desk.
type Snapshot struct {
	Dir     string
	Taken   time.Time
	Pending []Item
	Running []Item
	Done    []Item
	Skipped []Item
}

// encodeJSON renders v indented, through the terminal-safe encoder: meta and
// summary files are printed by `desk status`, so a hostile header must not
// reach a terminal through them.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := termsafe.JSONEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// describeMax caps a name or path quoted in an error message.
const describeMax = 1024

// describe renders an item name or path for an error message: one inert line,
// capped, since a hand-dropped name is untrusted text.
func describe(s string) string { return termsafe.SafeLineMax(s, describeMax) }
