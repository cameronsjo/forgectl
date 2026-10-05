//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// Claimed is an item this desk won and verified. Content is exactly the
// verified bytes, and Content is what runs: hand it to bash through
// [Claimed.Script] on fd 3 and run [ScriptFDPath].
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

// ScriptFDPath is the script operand for bash when the script arrives on
// fd 3 (exec.Cmd.ExtraFiles[0]). Under it, $0 and BASH_SOURCE are
// "/dev/fd/3", not the item's path: a script that locates sibling files
// from $0 finds nothing, as it found nothing useful in running/ before.
const ScriptFDPath = "/dev/fd/3"

// Script returns the read end of a pipe that yields Content and then EOF,
// for exec.Cmd.ExtraFiles[0]. Close it after the command starts.
func (c *Claimed) Script() (*os.File, error) { return ScriptPipe(c.Content) }

// ScriptPipe returns the read end of a pipe fed with data from a goroutine.
// A pipe cannot be rewritten or appended to by anyone holding a path, so the
// bytes bash reads are the bytes passed in. If the reader stops early, the
// write fails with EPIPE and the goroutine ends.
func ScriptPipe(data []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("desk: script pipe: %w", err)
	}
	go func() {
		_, _ = w.Write(data) // EPIPE only when the reader quit early; nothing to report
		_ = w.Close()
	}()
	return r, nil
}

// Claim moves a pending item to running/ and checks it is unchanged.
//
// The rename from pending/ into running/ is the claim: when two desks share a
// directory, the loser's rename finds nothing and Claim returns ErrClaimed
// without touching anything. The winner then reads the item once (O_NOFOLLOW,
// a regular file with one link), compares the full sha256 with the hash fixed
// when the item was queued (and with wantSHA, the hash on screen, when given),
// and writes those exact bytes to a fresh file that replaces the claimed one
// as the record. Run Claimed.Content (through [Claimed.Script]), not the file.
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

	file := name + kind.Ext()
	if err := d.root.Rename(path.Join(DirPending, file), path.Join(DirRunning, file)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrClaimed
		}
		return nil, fmt.Errorf("desk: claim %s: %w", describe(name), err)
	}
	if err := d.root.Rename(metaName(DirPending, name), metaName(DirRunning, name)); err != nil {
		return nil, fmt.Errorf("desk: claim %s meta: %w", describe(name), err)
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

// Run is a started run: the item is in running/, its owner recorded in meta,
// and RUN-START written.
type Run struct {
	Name   string
	Kind   Kind
	Meta   Meta
	Events *EventLog
	d      *Desk
}

// BeginRun records pid as the owner of a claimed item (the supervisor, or the
// desk itself for a TTY item run in its foreground), with its start time so a
// reused pid never reads as alive, and writes RUN-START. fields are appended
// to the RUN-START line (a batch adds steps= and jobs=).
func (d *Desk) BeginRun(name string, pid int, fields ...string) (*Run, error) {
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
	now := d.now().UTC()
	meta.Kind, meta.StartedAt, meta.PID, meta.PIDStart = kind, &now, pid, ownStart(pid)
	if err := d.writeMeta(DirRunning, name, meta); err != nil {
		return nil, err
	}
	ev, err := d.openEvents(name)
	if err != nil {
		return nil, err
	}
	line := fmt.Sprintf("%s id=%s pid=%d", EventRunStart, name, pid)
	if len(fields) > 0 {
		line += " " + strings.Join(fields, " ")
	}
	if err := ev.Emit(line); err != nil {
		_ = ev.Close()
		return nil, fmt.Errorf("desk: write RUN-START for %s: %w", describe(name), err)
	}
	return &Run{Name: name, Kind: kind, Meta: meta, Events: ev, d: d}, nil
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

// Skip moves a pending item to skipped/ with reason. A lost running item
// (owner dead, no RUN-END) may be skipped too, which is how a lost run leaves
// running/.
func (d *Desk) Skip(name, reason string) error {
	from := DirPending
	kind, err := d.findKind(DirPending, name)
	if errors.Is(err, ErrNotFound) {
		if kind, err = d.findKind(DirRunning, name); err != nil {
			return err
		}
		meta, _, err := d.readMeta(DirRunning, name)
		if err != nil {
			return err
		}
		if !d.lost(name, meta) {
			return fmt.Errorf("desk: %s is running; only a lost run can be skipped", describe(name))
		}
		from = DirRunning
	} else if err != nil {
		return err
	}
	meta, _, err := d.readMeta(from, name)
	if err != nil {
		return err
	}
	meta.Kind, meta.SkipReason = kind, reason
	if err := d.writeMeta(from, name, meta); err != nil {
		return err
	}
	if err := d.move(name, kind, from, DirSkipped); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrClaimed
		}
		return fmt.Errorf("desk: skip %s: %w", describe(name), err)
	}
	return nil
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
	meta.SkipReason = ""
	if err := d.writeMeta(DirSkipped, name, meta); err != nil {
		return err
	}
	if d.exists(path.Join(DirPending, name+kind.Ext())) {
		return fmt.Errorf("desk: %s is already pending", describe(name))
	}
	return d.move(name, kind, DirSkipped, DirPending)
}
