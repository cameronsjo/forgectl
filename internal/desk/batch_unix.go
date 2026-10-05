//go:build unix

package desk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Batch timing.
const (
	// DefaultJobs caps parallel steps.
	DefaultJobs = 4
	// DefaultGrace is how long a signalled group gets between SIGTERM and SIGKILL.
	DefaultGrace = 5 * time.Second
	// orphanAfter: the step's shell exited but something it started still
	// holds the output pipe; the group gets SIGTERM after this long.
	orphanAfter = time.Second
	// heldAfter: the group is dead but the pipe is still open (a process left
	// the group); stop reading after this long.
	heldAfter = time.Second
	batchTick = 200 * time.Millisecond
	readChunk = 64 << 10
	// timeoutRC is a timed-out step's rc, as timeout(1) reports it.
	timeoutRC = 124
)

// Step states.
const (
	stepPending   = "pending"
	stepRunning   = "running"
	stepOK        = "ok"
	stepFailed    = "failed"
	stepCancelled = "cancelled"
	stepSkipped   = "skipped"
)

// BatchOptions configures a batch run.
type BatchOptions struct {
	// ID names the run in summary.json (the item name).
	ID string
	// Dir is the run's private directory (done/<name>.d), already created
	// 0700. The runner creates steps/ inside it.
	Dir string
	// Jobs caps parallel steps; 0 means DefaultJobs.
	Jobs int
	// FailFast launches nothing new after the first failure.
	FailFast bool
	// Log receives the combined log: the header, `[step] line` output of
	// non-private steps, and the step events.
	Log io.Writer
	// Event receives each event line (STEP-START, STEP-END, STEP-SKIP,
	// STEP-WARN). It may be nil.
	Event func(line string)
	// Env is the steps' base environment; nil means os.Environ().
	Env []string
	// Grace overrides DefaultGrace.
	Grace time.Duration
}

// BatchResult is how a batch ended. RC is 0 when every step is ok, 1 when any
// failed or was skipped, 2 when the runner itself failed, 130 when
// interrupted.
type BatchResult struct {
	RC      int
	Reason  string
	OK      int
	Failed  int
	Skipped int
}

// Fields renders the counts for the RUN-END line.
func (r BatchResult) Fields() []string {
	return []string{"ok=" + strconv.Itoa(r.OK), "failed=" + strconv.Itoa(r.Failed), "skipped=" + strconv.Itoa(r.Skipped)}
}

// Batch runs one manifest. Steps never get a terminal: stdin is /dev/null and
// stdout/stderr are a pipe the runner reads.
type Batch struct {
	m       *Manifest
	opts    BatchOptions
	steps   string // <Dir>/steps
	signals atomic.Int32

	state    map[string]string
	rc       map[string]int
	reason   map[string]string
	t0, t1   map[string]time.Time
	outputs  map[string]map[string]string
	outKeys  map[string][]string
	running  map[string]*stepRun
	redact   Redactor
	stopping bool // fail-fast: launch nothing new
	finished int

	msgs chan stepMsg
	done chan struct{}
}

type stepRun struct {
	step      Step
	cmd       *exec.Cmd
	pgid      int
	out       *os.File // read end of the step's output pipe
	logF      *os.File
	cleanLog  bool // descendant of a private step: its log gets normalized, redacted lines
	deadline  time.Time
	buf       []byte
	eof       bool
	exited    bool
	exitedAt  time.Time
	termAt    time.Time
	killedAt  time.Time
	timedOut  bool
	cancelled bool
	// groupGone: a signal found the group empty. Its pgid may be reused from
	// then on, so the group is never signalled again.
	groupGone bool
}

type stepMsg struct {
	id     string
	data   []byte
	eof    bool
	exited bool
}

// NewBatch prepares a run of m.
func NewBatch(m *Manifest, opts BatchOptions) *Batch {
	if opts.Jobs <= 0 {
		opts.Jobs = DefaultJobs
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	b := &Batch{
		m: m, opts: opts, steps: filepath.Join(opts.Dir, "steps"),
		state: map[string]string{}, rc: map[string]int{}, reason: map[string]string{},
		t0: map[string]time.Time{}, t1: map[string]time.Time{},
		outputs: map[string]map[string]string{}, outKeys: map[string][]string{},
		running: map[string]*stepRun{},
		msgs:    make(chan stepMsg, 256), done: make(chan struct{}),
	}
	for _, s := range m.Steps {
		b.state[s.ID] = stepPending
	}
	return b
}

// Interrupt cancels the run: every running group gets SIGTERM and unstarted
// steps are skipped. A second call sends SIGKILL at once. Safe to call from a
// signal handler goroutine.
func (b *Batch) Interrupt() { b.signals.Add(1) }

// Run executes the batch and writes status.tsv and summary.json.
func (b *Batch) Run() (res BatchResult) {
	defer close(b.done)
	defer func() {
		if p := recover(); p != nil {
			res = b.internalError(fmt.Errorf("panic: %v", p))
		}
	}()
	if err := os.Mkdir(b.steps, dirMode); err != nil {
		return b.internalError(err)
	}
	b.header()
	if err := b.writeStatus(); err != nil {
		return b.internalError(err)
	}
	rc, reason, err := b.loop()
	if err != nil {
		return b.internalError(err)
	}
	return b.finalize(rc, reason)
}

func (b *Batch) loop() (int, string, error) {
	for {
		if b.signals.Load() > 0 {
			return b.cancel()
		}
		for _, s := range b.ready() {
			// launch can end a step (and so skip others), or a signal can land.
			if b.stopping || b.signals.Load() > 0 || len(b.running) >= b.opts.Jobs || b.state[s.ID] != stepPending {
				break
			}
			if err := b.launch(s); err != nil {
				return 0, "", err
			}
		}
		if len(b.running) == 0 {
			break
		}
		if err := b.pump(); err != nil {
			return 0, "", err
		}
	}
	_, failed, skipped := b.counts()
	if failed > 0 || skipped > 0 {
		return 1, stepFailed, nil
	}
	return 0, stepOK, nil
}

func (b *Batch) ready() []Step {
	var out []Step
	for _, s := range b.m.Steps {
		if b.state[s.ID] != stepPending {
			continue
		}
		if !slices.ContainsFunc(s.After, func(d string) bool { return b.state[d] != stepOK }) {
			out = append(out, s)
		}
	}
	return out
}

func (b *Batch) log(line string) {
	_, _ = io.WriteString(b.opts.Log, line+"\n")
}

func (b *Batch) event(line string) {
	b.log(line)
	if b.opts.Event != nil {
		b.opts.Event(line)
	}
}

func (b *Batch) header() {
	for _, s := range b.m.Steps {
		b.log(fmt.Sprintf("   %-8s %-17s %s", s.ID, s.OptionsText(), s.Command))
	}
	b.log("   order: " + OrderLine(b.m.Waves()))
}

func createPrivate(p string, appendMode bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW
	if appendMode {
		flags |= os.O_APPEND
	}
	return os.OpenFile(p, flags, fileMode) //nolint:gosec // G304: a fixed name under the run's private dir
}

func (b *Batch) launch(s Step) error {
	outPath := filepath.Join(b.steps, s.ID+".out")
	outF, err := createPrivate(outPath, false)
	if err != nil {
		return err
	}
	if err := outF.Close(); err != nil {
		return err
	}
	logF, err := createPrivate(filepath.Join(b.steps, s.ID+".log"), true)
	if err != nil {
		return err
	}
	env := slices.Clone(b.opts.Env)
	env = append(env, "STEP_OUT="+outPath, "RUN_BATCH_DIR="+b.opts.Dir, "RUN_STEP_ID="+s.ID)
	anc := make([]string, 0, len(b.m.Ancestors[s.ID]))
	for a := range b.m.Ancestors[s.ID] {
		anc = append(anc, a)
	}
	sort.Strings(anc)
	for _, a := range anc {
		for _, k := range b.outKeys[a] {
			env = append(env, "OUT_"+a+"_"+k+"="+b.outputs[a][k])
		}
	}
	b.state[s.ID], b.t0[s.ID] = stepRunning, time.Now()
	b.event(fmt.Sprintf("%s id=%s deps=%s", EventStepStart, s.ID, strings.Join(s.After, ",")))

	r, w, err := os.Pipe()
	if err != nil {
		_ = logF.Close()
		return err
	}
	// A step is its own process group, so a timeout or Ctrl-C reaches
	// everything it started, not only its shell.
	cmd := exec.Command("/bin/bash", "-c", s.Command) //nolint:gosec,noctx // G204: the approved manifest's command is the payload; the runner owns the step's lifetime through its process group, not a context
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = nil, w, w, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := cmd.Start()
	_ = w.Close() // the child holds the write end now; EOF comes when every holder exits
	if startErr != nil {
		_ = r.Close()
		_ = logF.Close()
		b.endStep(s, 127, stepFailed)
		return nil
	}
	private := false
	for a := range b.m.Ancestors[s.ID] {
		private = private || b.m.step(a).Private
	}
	sr := &stepRun{step: s, cmd: cmd, pgid: cmd.Process.Pid, out: r, logF: logF, cleanLog: private}
	if s.Timeout > 0 {
		sr.deadline = b.t0[s.ID].Add(time.Duration(s.Timeout) * time.Second)
	}
	b.running[s.ID] = sr
	go b.readPipe(s.ID, r)
	go b.wait(s.ID, cmd)
	return b.writeStatus()
}

func (b *Batch) send(m stepMsg) {
	select {
	case b.msgs <- m:
	case <-b.done:
	}
}

func (b *Batch) readPipe(id string, r *os.File) {
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.send(stepMsg{id: id, data: bytes.Clone(buf[:n])})
		}
		if err != nil {
			b.send(stepMsg{id: id, eof: true})
			return
		}
	}
}

func (b *Batch) wait(id string, cmd *exec.Cmd) {
	_ = cmd.Wait() // the exit status is read from cmd.ProcessState once the loop sees this message
	b.send(stepMsg{id: id, exited: true})
}

func (b *Batch) pump() error {
	t := time.NewTimer(batchTick)
	defer t.Stop()
	select {
	case m := <-b.msgs:
		b.handle(m)
		for drained := false; !drained; {
			select {
			case m := <-b.msgs:
				b.handle(m)
			default:
				drained = true
			}
		}
	case <-t.C:
	}
	return b.check(time.Now())
}

func (b *Batch) handle(m stepMsg) {
	r, ok := b.running[m.id]
	if !ok {
		return // a step already finished (its pipe was retired)
	}
	switch {
	case m.exited:
		r.exited, r.exitedAt = true, time.Now()
	case m.eof:
		if !r.eof {
			r.eof = true
			b.flush(r)
		}
	case !r.eof:
		if !r.cleanLog {
			_, _ = r.logF.Write(m.data)
		}
		lines := bytes.Split(append(r.buf, m.data...), []byte("\n"))
		r.buf = lines[len(lines)-1]
		for _, raw := range lines[:len(lines)-1] {
			b.line(r, raw)
		}
	}
}

func (b *Batch) flush(r *stepRun) {
	if len(r.buf) > 0 {
		b.line(r, r.buf)
		r.buf = nil
	}
}

func (b *Batch) line(r *stepRun, raw []byte) {
	text := Clean(&b.redact, strings.ToValidUTF8(string(raw), "\uFFFD"))
	if r.cleanLog {
		_, _ = r.logF.WriteString(text + "\n")
	}
	if !r.step.Private {
		b.log("[" + r.step.ID + "] " + text)
	}
}

// signalGroup signals the step's whole process group; false when the group
// no longer exists.
func signalGroup(pgid int, sig syscall.Signal) bool {
	err := unix.Kill(-pgid, sig)
	return !errors.Is(err, unix.ESRCH)
}

func (b *Batch) check(now time.Time) error {
	for _, r := range b.runningInOrder() {
		// A deadline only applies to a step still running; one that already
		// exited keeps its own rc even if a leftover child holds the pipe.
		if !r.deadline.IsZero() && !now.Before(r.deadline) && r.termAt.IsZero() && !r.exited {
			r.timedOut = true
		}
		orphaned := r.exited && !r.eof && now.Sub(r.exitedAt) >= orphanAfter
		if r.termAt.IsZero() && (r.timedOut || r.cancelled || orphaned) {
			r.termAt = now
			if !signalGroup(r.pgid, unix.SIGTERM) {
				r.killedAt, r.groupGone = now, true // the group is already empty: nothing to wait for
			}
		}
		if !r.termAt.IsZero() && r.killedAt.IsZero() && !signalGroup(r.pgid, 0) {
			r.killedAt, r.groupGone = now, true // the group died from TERM; only an escaped child is left
		}
		if !r.termAt.IsZero() && r.killedAt.IsZero() && (now.Sub(r.termAt) >= b.opts.Grace || b.signals.Load() >= 2) {
			r.killedAt = now
			signalGroup(r.pgid, unix.SIGKILL)
		}
		if !r.killedAt.IsZero() && !r.eof && r.exited && now.Sub(r.killedAt) >= heldAfter {
			b.closeHeld(r)
		}
		if r.eof && r.exited {
			if err := b.finish(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Batch) runningInOrder() []*stepRun {
	var out []*stepRun
	for _, s := range b.m.Steps {
		if r, ok := b.running[s.ID]; ok {
			out = append(out, r)
		}
	}
	return out
}

// closeHeld stops reading a pipe that a process outside the step's group
// still holds, so the step can end instead of hanging the batch.
func (b *Batch) closeHeld(r *stepRun) {
	r.eof = true
	_ = r.out.Close()
	b.flush(r)
	b.event(fmt.Sprintf("%s id=%s msg=output pipe still open after the step's group exited (a process left the group); stopped reading", EventStepWarn, r.step.ID))
}

func (b *Batch) finish(r *stepRun) error {
	id := r.step.ID
	delete(b.running, id)
	if !r.termAt.IsZero() && !r.groupGone { // nothing in the group may outlive its STEP-END
		signalGroup(r.pgid, unix.SIGKILL)
	}
	_ = r.logF.Close()
	_ = r.out.Close()
	rc := exitCode(r.cmd.ProcessState)
	data, err := os.ReadFile(filepath.Join(b.steps, id+".out")) //nolint:gosec // G304: a fixed name under the run's private dir
	if err != nil {
		return err
	}
	outs, keys, warns := ParseOutputs(data)
	for _, w := range warns {
		b.event(fmt.Sprintf("%s id=%s msg=%s", EventStepWarn, id, w))
	}
	if r.step.Private {
		b.redact.Add(id, outs)
	}
	var reason string
	switch {
	case r.timedOut:
		rc, reason = timeoutRC, "timeout"
	case r.cancelled:
		reason = stepCancelled
	case rc == 0:
		reason = stepOK
	default:
		reason = stepFailed
	}
	if reason == stepOK {
		b.outputs[id], b.outKeys[id] = outs, keys
	}
	b.endStep(r.step, rc, reason)
	return b.writeStatus()
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func (b *Batch) logRef(s Step) string {
	if s.Private {
		return "private" // the raw log holds the secrets; reading it is the operator's call
	}
	return filepath.Join(b.steps, s.ID+".log")
}

func (b *Batch) endStep(s Step, rc int, reason string) {
	id := s.ID
	b.t1[id], b.rc[id], b.reason[id] = time.Now(), rc, reason
	switch reason {
	case stepOK:
		b.state[id] = stepOK
	case stepCancelled:
		b.state[id] = stepCancelled
	default:
		b.state[id] = stepFailed
	}
	b.finished++
	dur := b.t1[id].Sub(b.t0[id]).Seconds()
	b.event(fmt.Sprintf("%s id=%s rc=%d dur=%.1f reason=%s outputs=%s log=%s",
		EventStepEnd, id, rc, dur, reason, strings.Join(b.outKeys[id], ","), b.logRef(s)))
	if reason == stepFailed || reason == "timeout" {
		b.skipDependents()
		if b.opts.FailFast && !b.stopping {
			b.stopping = true
			for _, p := range b.m.Steps {
				if b.state[p.ID] == stepPending {
					b.skip(p.ID, "fail-fast")
				}
			}
		}
	}
}

func (b *Batch) skipDependents() {
	failed := map[string]bool{}
	for id, st := range b.state {
		if st == stepFailed {
			failed[id] = true
		}
	}
	for _, s := range b.m.Steps {
		if b.state[s.ID] != stepPending {
			continue
		}
		for a := range b.m.Ancestors[s.ID] {
			if failed[a] {
				b.skip(s.ID, "dep-failed:"+b.m.nearestFailed(s.ID, failed))
				break
			}
		}
	}
}

func (b *Batch) skip(id, reason string) {
	b.state[id], b.reason[id] = stepSkipped, reason
	b.event(fmt.Sprintf("%s id=%s reason=%s", EventStepSkip, id, reason))
}

func (b *Batch) cancel() (int, string, error) {
	for _, r := range b.running {
		r.cancelled = !r.exited // a step that already exited keeps its own result
	}
	for len(b.running) > 0 {
		if err := b.pump(); err != nil {
			return 0, "", err
		}
	}
	for _, s := range b.m.Steps {
		if b.state[s.ID] == stepPending {
			b.skip(s.ID, "interrupted")
		}
	}
	return 130, "interrupted", b.writeStatus()
}

func (b *Batch) counts() (ok, failed, skipped int) {
	for _, st := range b.state {
		switch st {
		case stepOK:
			ok++
		case stepFailed, stepCancelled:
			failed++
		case stepSkipped:
			skipped++
		}
	}
	return ok, failed, skipped
}

func epoch(t time.Time) string { return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64) }

func (b *Batch) writeStatus() error {
	rows := []string{"step\tstate\trc\tstart\tend\tdeps"}
	for _, s := range b.m.Steps {
		rc, start, end, deps := "-", "-", "-", "-"
		if v, ok := b.rc[s.ID]; ok {
			rc = strconv.Itoa(v)
		}
		if t, ok := b.t0[s.ID]; ok {
			start = epoch(t)
		}
		if t, ok := b.t1[s.ID]; ok {
			end = epoch(t)
		}
		if len(s.After) > 0 {
			deps = strings.Join(s.After, ",")
		}
		rows = append(rows, strings.Join([]string{s.ID, b.state[s.ID], rc, start, end, deps}, "\t"))
	}
	return writeAtomic(filepath.Join(b.opts.Dir, "status.tsv"), []byte(strings.Join(rows, "\n")+"\n"))
}

// Summary is a batch's summary.json. It carries output key names, never values.
type Summary struct {
	ID      string        `json:"id"`
	RC      int           `json:"rc"`
	Reason  string        `json:"reason"`
	OK      int           `json:"ok"`
	Failed  int           `json:"failed"`
	Skipped int           `json:"skipped"`
	Steps   []SummaryStep `json:"steps"`
}

// SummaryStep is one step in [Summary]. Log is nil for a private step and
// for a step that never started.
type SummaryStep struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	RC       *int     `json:"rc"`
	Reason   string   `json:"reason"`
	Duration *float64 `json:"duration"`
	Outputs  []string `json:"outputs"`
	Private  bool     `json:"private"`
	Log      *string  `json:"log"`
}

func (b *Batch) summary(rc int, reason string) Summary {
	ok, failed, skipped := b.counts()
	sum := Summary{ID: b.opts.ID, RC: rc, Reason: reason, OK: ok, Failed: failed, Skipped: skipped, Steps: []SummaryStep{}}
	for _, s := range b.m.Steps {
		st := SummaryStep{ID: s.ID, State: b.state[s.ID], Reason: b.reason[s.ID], Outputs: []string{}, Private: s.Private}
		if v, ok := b.rc[s.ID]; ok {
			st.RC = &v
		}
		t0, started := b.t0[s.ID]
		if t1, ok := b.t1[s.ID]; ok && started {
			dur := float64(t1.Sub(t0).Round(time.Millisecond).Milliseconds()) / 1000
			st.Duration = &dur
		}
		if keys := b.outKeys[s.ID]; keys != nil {
			st.Outputs = keys
		}
		if started && !s.Private {
			ref := b.logRef(s)
			st.Log = &ref
		}
		sum.Steps = append(sum.Steps, st)
	}
	return sum
}

func (b *Batch) finalize(rc int, reason string) BatchResult {
	if err := b.writeStatus(); err != nil {
		return b.internalError(err)
	}
	data, err := encodeJSON(b.summary(rc, reason))
	if err != nil {
		return b.internalError(err)
	}
	if err := writeAtomic(filepath.Join(b.opts.Dir, "summary.json"), data); err != nil {
		return b.internalError(err)
	}
	ok, failed, skipped := b.counts()
	return BatchResult{RC: rc, Reason: reason, OK: ok, Failed: failed, Skipped: skipped}
}

// internalError stops every running group, then reports rc 2: the batch's
// own failure must still end the run cleanly for its watcher.
//
// A group already found empty is never signalled again (its pgid may be
// reused), and every group is probed before each real signal.
func (b *Batch) internalError(cause error) BatchResult {
	live := func(r *stepRun) bool {
		if !r.groupGone && !signalGroup(r.pgid, 0) {
			r.groupGone = true
		}
		return !r.groupGone
	}
	for _, r := range b.running {
		if live(r) {
			signalGroup(r.pgid, unix.SIGTERM)
		}
	}
	deadline := time.Now().Add(b.opts.Grace)
	for time.Now().Before(deadline) && slices.ContainsFunc(b.runningInOrder(), live) {
		time.Sleep(batchTick)
	}
	for _, r := range b.running {
		if live(r) {
			signalGroup(r.pgid, unix.SIGKILL)
		}
	}
	b.log("run-batch: internal error: " + cause.Error())
	ok, failed, skipped := b.counts()
	return BatchResult{RC: 2, Reason: "internal", OK: ok, Failed: failed, Skipped: skipped}
}

// writeAtomic replaces p with data through a temp file and a rename.
func writeAtomic(p string, data []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, fileMode) //nolint:gosec // G304: a fixed name under the run's private dir
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp, p) //nolint:gosec // G703: p is a fixed name under the run's private dir, built by this package
}
