//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// Claimed is an item this desk won and verified. Content is exactly the
// verified bytes, and Content is what runs: hand it to bash with
// [Claimed.AttachScript] and run [ScriptFDPath].
//
// RecordPath is running/<name>.sh, a copy of the same bytes kept as the
// record of what ran. Never execute it by name: anything that can write the
// desk can change that file, or rename another over it, after the hash
// check, and bash reads a script file while it runs.
type Claimed struct {
	Name       string
	Kind       Kind
	SHA256     string
	RecordPath string
	Content    []byte
	Headers
}

// ScriptFDPath is the script operand for bash once [AttachScript] has set up
// the command. Under it, $0 and BASH_SOURCE are "/dev/fd/3", not the item's
// path: a script that locates sibling files from $0 finds nothing, as it
// found nothing useful in running/ before.
const ScriptFDPath = "/dev/fd/3"

// scriptPrelude is the desk's own first line, sent through the pipe ahead of
// the verified bytes. bash reads the script through its own descriptor (fd
// 255, close-on-exec) and leaves the inherited fd 3 open, so every child would
// inherit the pipe; one that read it (`cmd <&3`) would silently swallow the
// rest of the script. Closing fd 3 first means no child ever holds it. It is
// not part of the item: the hash and the running/ record cover the item's own
// bytes only. The cost is that LINENO in the script reads one higher than the
// line in the item.
const scriptPrelude = "exec 3<&-\n"

// AttachScript makes data the script bash reads at [ScriptFDPath]: it puts
// the read end of a pipe, fed with the prelude and then data, in
// cmd.ExtraFiles[0], which is fd 3 in the child. It refuses a cmd that already
// carries extra files, since ScriptFDPath names fd 3 and nothing else. Call
// release after cmd.Start, started or not, to drop the parent's copy.
func AttachScript(cmd *exec.Cmd, data []byte) (release func(), err error) {
	if len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("desk: AttachScript needs a command with no other extra files (the script must be fd 3)")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("desk: script pipe: %w", err)
	}
	payload := append([]byte(scriptPrelude), data...)
	go func() {
		_, _ = w.Write(payload) // EPIPE only when the reader quit early; nothing to report
		_ = w.Close()
	}()
	cmd.ExtraFiles = []*os.File{r}
	return func() { _ = r.Close() }, nil
}

// AttachScript is [AttachScript] with the claimed item's verified bytes.
func (c *Claimed) AttachScript(cmd *exec.Cmd) (release func(), err error) {
	return AttachScript(cmd, c.Content)
}

// Claim moves a pending item to running/ and checks it is unchanged.
//
// The rename from pending/ into running/ is the claim: when two desks share a
// directory, the loser's rename finds nothing and Claim returns ErrClaimed
// without touching anything. The winner then reads the item once (O_NOFOLLOW,
// a regular file with one link), compares the full sha256 with the hash fixed
// when the item was queued (and with wantSHA, the hash on screen, when given),
// and writes those exact bytes to a fresh file that replaces the claimed one
// as the record. Run Claimed.Content (through [Claimed.AttachScript]), not the file.
// An item whose bytes changed moves to skipped/ and Claim returns ErrChanged.
func (d *Desk) Claim(name, wantSHA string) (*Claimed, error) {
	kind, err := d.findKind(DirPending, name)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrClaimed
	}
	if err != nil {
		return nil, err
	}
	meta, ok, err := d.readMeta(DirPending, name)
	if err != nil {
		return nil, err
	}
	if !ok || meta.SHA256 == "" {
		return nil, fmt.Errorf("%w: %s has no recorded hash yet; scan the desk first", ErrRefused, describe(name))
	}
	if wantSHA != "" && wantSHA != meta.SHA256 {
		return nil, fmt.Errorf("%w: %s: the hash on screen is not the hash it was queued with", ErrChanged, describe(name))
	}

	// claimed_at goes into the meta before the rename, so the moment the item
	// is in running/ its meta (once it follows) says when the claim began. A
	// running item whose meta has not followed yet is a claim in progress.
	claimed := d.now().UTC()
	meta.ClaimedAt = &claimed
	if err := d.writeMeta(DirPending, name, meta); err != nil {
		return nil, err
	}

	file := name + kind.Ext()
	if err := d.root.Rename(path.Join(DirPending, file), path.Join(DirRunning, file)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrClaimed
		}
		return nil, fmt.Errorf("desk: claim %s: %w", describe(name), err)
	}
	if err := claimMeta(d, name); err != nil {
		// The item is in running/ and nothing will own it: without its meta it
		// would read as a claim in progress for ever. Give running/ the meta
		// this claim already holds and release the item, so it ends in
		// skipped/ (launch-failed) where the operator sees it and nothing
		// runs it. The orphaned pending meta goes too.
		werr := d.writeMeta(DirRunning, name, meta)
		rerr := d.Release(name, SkipLaunchFailed)
		if rerr == nil {
			_ = d.root.Remove(metaName(DirPending, name))
		}
		return nil, errors.Join(fmt.Errorf("desk: claim %s meta: %w", describe(name), err), werr, rerr)
	}

	data, _, err := d.readItem(DirRunning, file)
	var r *refusal
	if errors.As(err, &r) {
		meta.SkipReason = "refused: " + r.reason
		return nil, errors.Join(err, d.writeMeta(DirRunning, name, meta), d.move(name, kind, DirRunning, DirSkipped))
	}
	if err != nil {
		return nil, err
	}
	sum := SHA256Hex(data)
	if sum != meta.SHA256 {
		if err := d.skipChanged(DirRunning, name, kind, meta); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrChanged, describe(name))
	}
	// Replace the claimed inode with one only this desk has written, holding
	// exactly the bytes just verified, as the record of what runs. Whoever
	// held the original open can no longer write the record through it. The
	// run itself reads Content through a pipe, so even a later write to the
	// record by name never reaches a running script.
	tmp, err := d.writeTemp(DirRunning, name, data)
	if err != nil {
		return nil, err
	}
	if err := d.root.Rename(tmp, path.Join(DirRunning, file)); err != nil {
		_ = d.root.Remove(tmp)
		return nil, fmt.Errorf("desk: stage %s: %w", describe(name), err)
	}
	return &Claimed{
		Name: name, Kind: kind, SHA256: sum, Content: data, Headers: ParseHeaders(data),
		RecordPath: d.abs(path.Join(DirRunning, file)),
	}, nil
}

// claimMeta moves a claimed item's meta into running/. Tests replace it to
// fail that step.
var claimMeta = func(d *Desk, name string) error {
	return d.root.Rename(metaName(DirPending, name), metaName(DirRunning, name))
}

// beforeRunStart runs after BeginRun has written the owner's pid and before
// it writes RUN-START. Tests replace it to fail that step.
var beforeRunStart = func(string) error { return nil }

// Run is a started run: the item is in running/, its owner recorded in meta,
// and RUN-START written.
type Run struct {
	Name   string
	Kind   Kind
	Meta   Meta
	Events *EventLog
	d      *Desk
	lock   *ownerLock
}

// BeginRun records pid as the owner of a claimed item (the supervisor, or the
// desk itself for a TTY item run in its foreground), with its start time so a
// reused pid never reads as alive, and writes RUN-START. fields are appended
// to the RUN-START line (a batch adds steps= and jobs=).
//
// The caller becomes the owner: BeginRun takes the owner lock (see
// owner_unix.go) and the Run holds it until Finish, so nothing can skip or
// release the item while the run is live. On failure nothing is left
// claiming ownership: the lock is dropped and a pid already written to meta
// is cleared, so the item can be released.
func (d *Desk) BeginRun(name string, pid int, fields ...string) (run *Run, err error) {
	lock, err := d.lockOwner(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			// The item is still here, or gone because someone moved it (and
			// removed the lock file); only in the second case is ours a
			// stray to remove.
			_, ferr := d.findKind(DirRunning, name)
			lock.release(ferr != nil)
		}
	}()
	kind, err := d.findKind(DirRunning, name)
	if err != nil {
		return nil, fmt.Errorf("desk: begin %s: %w", describe(name), err)
	}
	if d.doneTaken(name) {
		// A reused number: the run would append to, or script(1) would
		// truncate, another run's log and events.
		return nil, fmt.Errorf("%w: done/ already holds %s", ErrRefused, describe(name))
	}
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		return nil, err
	}
	unowned := meta
	now := d.now().UTC()
	meta.Kind, meta.StartedAt, meta.PID, meta.PIDStart = kind, &now, pid, ownStart(pid)
	if err := d.writeMeta(DirRunning, name, meta); err != nil {
		return nil, err
	}
	// From here a failure must take the pid back out of meta.
	undo := func(cause error) error { return errors.Join(cause, d.writeMeta(DirRunning, name, unowned)) }
	if err := beforeRunStart(name); err != nil {
		return nil, undo(err)
	}
	ev, err := d.openEvents(name)
	if err != nil {
		return nil, undo(err)
	}
	line := fmt.Sprintf("%s id=%s pid=%d", EventRunStart, name, pid)
	if len(fields) > 0 {
		line += " " + strings.Join(fields, " ")
	}
	if err := ev.Emit(line); err != nil {
		_ = ev.Close()
		return nil, undo(fmt.Errorf("desk: write RUN-START for %s: %w", describe(name), err))
	}
	return &Run{Name: name, Kind: kind, Meta: meta, Events: ev, d: d, lock: lock}, nil
}

// Finish ends a run: it stamps ended_at, appends EXIT=<rc> as the log's last
// line, moves the item and its meta to done/, and writes RUN-END last, so a
// watcher that reads RUN-END finds everything else already in place. fields
// are appended to the RUN-END line (a batch adds ok= failed= skipped=).
func (r *Run) Finish(rc int, reason string, fields ...string) error {
	d := r.d
	defer r.Events.Close() //nolint:errcheck // the RUN-END write below reports its own failure
	now := d.now().UTC()
	r.Meta.EndedAt, r.Meta.ExitCode = &now, &rc
	var errs []error
	if err := d.writeMeta(DirRunning, r.Name, r.Meta); err != nil {
		errs = append(errs, err)
	}
	if err := d.appendExit(r.Name, rc); err != nil {
		errs = append(errs, err)
	}
	if err := d.move(r.Name, r.Kind, DirRunning, DirDone); err != nil {
		errs = append(errs, fmt.Errorf("desk: move %s to done/: %w", describe(r.Name), err))
	}
	line := fmt.Sprintf("%s rc=%d reason=%s", EventRunEnd, rc, reason)
	if len(fields) > 0 {
		line += " " + strings.Join(fields, " ")
	}
	if err := r.Events.Emit(line); err != nil {
		errs = append(errs, fmt.Errorf("desk: write RUN-END for %s: %w", describe(r.Name), err))
	}
	if r.lock != nil {
		r.lock.release(true)
		r.lock = nil
	}
	return errors.Join(errs...)
}

// Abandon ends a run that began but cannot go on because its name is taken
// in done/ (the log already exists): the item moves to skipped/ with reason,
// and nothing in done/ is touched, unlike Finish, which would append to that
// log and rename over that record. The events file this run opened is left
// with its RUN-START; watch reports the item skipped.
func (r *Run) Abandon(reason string) error {
	d := r.d
	_ = r.Events.Close()
	r.Meta.PID, r.Meta.PIDStart, r.Meta.SkipReason = 0, 0, reason
	errs := []error{d.writeMeta(DirRunning, r.Name, r.Meta)}
	if err := d.move(r.Name, r.Kind, DirRunning, DirSkipped); err != nil {
		errs = append(errs, fmt.Errorf("desk: move %s to skipped/: %w", describe(r.Name), err))
	} else if r.lock != nil {
		r.lock.release(true)
		r.lock = nil
	}
	if r.lock != nil {
		r.lock.release(false)
		r.lock = nil
	}
	return errors.Join(errs...)
}

// appendExit appends EXIT=<rc> to done/<name>.log on a line of its own, even
// when the output before it did not end in a newline.
func (d *Desk) appendExit(name string, rc int) error {
	p := path.Join(DirDone, name+extLog)
	f, err := d.root.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("desk: open log for %s: %w", describe(name), err)
	}
	prefix := ""
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n"
		}
	}
	_, werr := f.WriteString(prefix + "EXIT=" + strconv.Itoa(rc) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("desk: write EXIT for %s: %w", describe(name), werr)
	}
	return nil
}

// Release moves a claimed item whose run never began (no owner recorded) to
// skipped/ with reason, so a failed launch ends somewhere the operator sees
// it and nothing runs it later. It refuses an item that has an owner: that
// run ends through Finish, or as lost.
func (d *Desk) Release(name, reason string) error {
	kind, err := d.findKind(DirRunning, name)
	if err != nil {
		return err
	}
	lock, held, err := d.tryLockOwnerSettled(name)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("desk: %s has an owner (its lock is held); only a run that never began can be released", describe(name))
	}
	moved := false
	defer func() { lock.release(moved) }()
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		return err
	}
	if meta.PID != 0 {
		return fmt.Errorf("desk: %s has an owner (pid %d); only a run that never began can be released", describe(name), meta.PID)
	}
	meta.Kind, meta.SkipReason = kind, reason
	if err := d.writeMeta(DirRunning, name, meta); err != nil {
		return err
	}
	if err := d.move(name, kind, DirRunning, DirSkipped); err != nil {
		return fmt.Errorf("desk: release %s: %w", describe(name), err)
	}
	moved = true
	return nil
}

// Who skipped an item, recorded in meta as skipped_by.
const (
	SkippedByDashboard = "dashboard"
	SkippedByCLI       = "cli"
)

// Skip moves a pending item to skipped/ with reason. A lost running item
// (owner dead, no RUN-END) may be skipped too, which is how a lost run leaves
// running/; it is always recorded as SkipLost, whatever reason says, since a
// run that began must never be re-armed. Skip takes the owner lock first and
// refuses while a live owner holds it. Skip is the dashboard's skip: it
// records skipped_by "dashboard". `desk skip` uses [Desk.SkipNoted].
func (d *Desk) Skip(name, reason string) error {
	_, err := d.skip(name, reason, "", SkippedByDashboard)
	return err
}

// SkipNoteMax caps a skip note.
const SkipNoteMax = 200

// SkipNoted is the CLI's skip: [Desk.Skip] with [SkipOperator], recording
// skipped_by "cli". A pending item is recorded as operator, which
// [Desk.Unskip] can re-arm, and a lost run as [SkipLost], which it cannot.
// note, the one-line reason, is kept in meta as skip_note. It returns the
// reason recorded.
func (d *Desk) SkipNoted(name, note string) (string, error) {
	if strings.ContainsAny(note, "\r\n") {
		return "", errors.New("desk: a skip note must be one line")
	}
	if len([]rune(note)) > SkipNoteMax {
		return "", fmt.Errorf("desk: a skip note must be at most %d characters", SkipNoteMax)
	}
	return d.skip(name, SkipOperator, note, SkippedByCLI)
}

// skip is Skip with a note and an actor; it returns the reason recorded.
func (d *Desk) skip(name, reason, note, by string) (string, error) {
	from := DirPending
	var moved, staleMeta bool
	var carried Meta
	kind, err := d.findKind(DirPending, name)
	if errors.Is(err, ErrNotFound) {
		if kind, err = d.findKind(DirRunning, name); err != nil {
			return "", err
		}
		lock, held, err := d.tryLockOwnerSettled(name)
		if err != nil {
			return "", err
		}
		if held {
			return "", fmt.Errorf("desk: %s is running (its owner holds the lock); only a lost run can be skipped", describe(name))
		}
		defer func() { lock.release(moved) }()
		meta, ok, err := d.readMeta(DirRunning, name)
		if err != nil {
			return "", err
		}
		if !ok {
			// A claim that died between its two renames: its meta is still
			// in pending/. Carry it over and clear it from pending/.
			if meta, staleMeta, err = d.readMeta(DirPending, name); err != nil {
				return "", err
			}
		}
		if !d.lostUnlocked(name, meta) {
			return "", fmt.Errorf("desk: %s is running; only a lost run can be skipped", describe(name))
		}
		from, reason, carried = DirRunning, SkipLost, meta
	} else if err != nil {
		return "", err
	}
	meta, _, err := d.readMeta(from, name)
	if err != nil {
		return "", err
	}
	if staleMeta {
		meta = carried
	}
	now := d.now().UTC()
	meta.Kind, meta.SkipReason, meta.SkipNote = kind, reason, note
	meta.SkippedBy, meta.SkippedAt = by, &now
	if err := d.writeMeta(from, name, meta); err != nil {
		return "", err
	}
	if err := d.move(name, kind, from, DirSkipped); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrClaimed
		}
		return "", fmt.Errorf("desk: skip %s: %w", describe(name), err)
	}
	moved = from == DirRunning
	if staleMeta {
		_ = d.root.Remove(metaName(DirPending, name)) // carried over to skipped/ above
	}
	return reason, nil
}

// Unskip returns an operator-skipped item to pending/. An item skipped
// because it changed, was refused, or was lost cannot be re-armed.
func (d *Desk) Unskip(name string) error {
	kind, err := d.findKind(DirSkipped, name)
	if err != nil {
		return err
	}
	meta, _, err := d.readMeta(DirSkipped, name)
	if err != nil {
		return err
	}
	if meta.SkipReason != "" && meta.SkipReason != SkipOperator {
		return fmt.Errorf("desk: %s was skipped (%s) and cannot be re-armed; queue a new item", describe(name), meta.SkipReason)
	}
	meta.SkipReason, meta.SkipNote, meta.SkippedBy, meta.SkippedAt = "", "", "", nil
	if err := d.writeMeta(DirSkipped, name, meta); err != nil {
		return err
	}
	if d.exists(path.Join(DirPending, name+kind.Ext())) {
		return fmt.Errorf("desk: %s is already pending", describe(name))
	}
	return d.move(name, kind, DirSkipped, DirPending)
}
