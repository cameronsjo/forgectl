// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
)

const (
	// attentionStdinCap is how much of a reply put keeps: the last 64 KiB,
	// where the closing block is.
	attentionStdinCap = 64 << 10
	// attentionStdinMax is how much stdin put reads before refusing.
	attentionStdinMax = 16 << 20
	// attentionSweepMax caps the sweep.json read.
	attentionSweepMax = 4 << 20
	// attentionHerdrTimeout bounds the herdr agent list call.
	attentionHerdrTimeout = 3 * time.Second
)

// Source states reported by `list --json`.
const (
	sourceOK          = "ok"
	sourceAbsent      = "absent"
	sourceError       = "error"
	sourceUnavailable = "unavailable"
	sourceMalformed   = "malformed"
)

// Unix-only seams.
var (
	attentionStdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	// attentionHerdr asks the caller's herdr server for its agents. Known is
	// false when herdr could not be asked; prune then falls back to the TTL.
	attentionHerdr = func(ctx context.Context, deps module.Deps) attention.Herdr {
		h := attention.Herdr{Socket: attentionGetenv("HERDR_SOCKET_PATH")}
		if deps.Runner == nil {
			return h
		}
		ctx, cancel := context.WithTimeout(ctx, attentionHerdrTimeout)
		defer cancel()
		agents, err := herdr.New(deps.Runner).Agents(ctx)
		if err != nil {
			return h
		}
		h.Known = true
		for _, a := range agents {
			h.Agents = append(h.Agents, attentionAgent(a))
		}
		return h
	}
)

// attentionAgent reduces a herdr agent row to the queue's view. The session
// is the Claude Code session id (attentionSession); the names are the
// worktree and the basenames of the agent's directories, for the
// worker-prefix match.
func attentionAgent(a herdr.Agent) attention.Agent {
	out := attention.Agent{Pane: a.PaneID, Status: a.AgentStatus, Session: attentionSession(a)}
	var names []string
	if a.Tokens != nil {
		names = append(names, a.Tokens.Worktree, baseName(a.Tokens.CWD))
	}
	names = append(names, baseName(a.CWD), baseName(a.ForegroundCWD))
	for _, n := range names {
		if n != "" {
			out.Names = append(out.Names, n)
		}
	}
	return out
}

// attentionAgentClaude is herdr's agent label for Claude Code, the only
// agent whose session tokens the queue reads.
const attentionAgentClaude = "claude"

// attentionSession picks the session id herdr associates with an agent row.
// herdr's agent_session wins whenever herdr reports one, even when the
// pane's `session` token names a different id. Without it, a Claude row
// falls back to that token: with herdr's own Claude integration
// uninstalled, the herdr-bridge mod reports the session only there. Both
// are reports a process of yours sends over the herdr socket, so neither
// proves anything (docs/commands/attention.md, Which pane a session is in).
// A token that is not a valid session id is ignored.
func attentionSession(a herdr.Agent) string {
	if a.AgentSession != nil && a.AgentSession.Value != "" {
		return a.AgentSession.Value
	}
	if a.Agent == attentionAgentClaude && a.Tokens != nil && attention.ValidSession(a.Tokens.Session) {
		return a.Tokens.Session
	}
	return ""
}

func baseName(p string) string {
	if p == "" {
		return ""
	}
	b := filepath.Base(p)
	if b == "/" || b == "." {
		return ""
	}
	return b
}

// attentionUsage is a refused flag: exit 2, usage_error under --json.
func attentionUsage(format string, args ...any) error {
	return withJSONUsageCode(WithExitCode(fmt.Errorf(format, args...), exitUsage))
}

// attentionStateBase resolves the state directory, and refuses a bad
// [attention] section before anything is read.
func attentionStateBase(deps module.Deps) (string, error) {
	if err := deps.Cfg.Attention.Validate(); err != nil {
		return "", attentionUsage("attention: %v", err)
	}
	home, homeErr := attentionUserHome()
	base, err := attention.StateBase(attentionGetenv, home)
	if err != nil {
		return "", errors.Join(err, homeErr)
	}
	return base, nil
}

func runAttentionPut(cmd *cobra.Command, deps module.Deps, o attentionOpts) error {
	if o.session == "" || o.turn == "" {
		return attentionUsage("attention put: --session and --turn are required")
	}
	if attentionStdinIsTerminal() && cmd.InOrStdin() == os.Stdin {
		return attentionUsage("attention put: stdin is a terminal; pipe the reply text in")
	}
	base, err := attentionStateBase(deps)
	if err != nil {
		return err
	}
	now := attentionNow().UTC()
	r := attention.Record{Session: o.session, Pane: o.pane, Socket: o.socket, Cwd: o.cwd, Turn: o.turn, At: now}
	// Check the flags before reading stdin, so a bad id costs nothing.
	if err := attention.Validate(r); err != nil {
		return attentionUsage("attention put: %v", err)
	}
	text, err := readTail(cmd.InOrStdin(), attentionStdinCap, attentionStdinMax)
	if err != nil {
		return attentionUsage("attention put: %v", err)
	}
	p := attention.Parse(text)
	r.Items = p.Items
	// written is the record as Put stores it, so --json echoes the stored
	// items rather than the parser's view of them.
	// A refusal here is the one Put would return, with the same exit code.
	written, err := attention.Sanitize(r)
	if err != nil {
		return err
	}

	s, err := attention.OpenSpool(base, true)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // nothing buffered
	if old, ok, err := s.Get(o.session); err == nil && ok {
		since := old.At
		r.Since = &since
	}
	if err := s.Put(r); err != nil {
		return err
	}
	// The record is written. Housekeeping after it is best effort: a failure
	// is reported on stderr and does not turn a published record into a
	// failed put.
	h := attentionHerdr(cmd.Context(), deps)
	// Evict the pane's older records only when herdr itself says this
	// session runs in that pane on this socket: --pane and --socket are the
	// writer's claim, and any process of yours could name another pane.
	if herdrRuns(h, o.session, o.pane, o.socket) {
		if err := s.EvictPane(o.pane, o.socket, o.session); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "attention put: evict: %v\n", err) //nolint:errcheck // best-effort note on stderr
		}
	}
	live := h.Liveness()
	live.Keep = o.session
	if _, _, err := s.List(now, deps.Cfg.Attention.TTL(), live); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "attention put: prune: %v\n", err) //nolint:errcheck // best-effort note on stderr
	}
	if o.asJSON {
		return writeJSON(cmd.OutOrStdout(), attentionPutEcho(written, p))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "items=%d seen=%d read=%d\n", len(p.Items), p.Seen, p.Read)
	return err
}

// attentionPut is `put --json`: the session, the turn and the question and
// action items of the record as written, in record order. Every field is
// always present: actor and rec are "" when the item has none, and items is
// [] when the reply had no block. A viewer holds this copy in memory and
// compares spool rows against it, so its text is the stored, cleaned text.
type attentionPut struct {
	Session string             `json:"session"`
	Turn    string             `json:"turn"`
	Items   []attentionPutItem `json:"items"`
	Seen    int                `json:"seen"`
	Read    int                `json:"read"`
}

type attentionPutItem struct {
	Kind  string `json:"kind"`
	N     int    `json:"n"`
	Actor string `json:"actor"`
	Text  string `json:"text"`
	Rec   string `json:"rec"`
}

// attentionPutEcho builds the --json echo from the written record. Notes and
// options are stored but are not rows, so they are left out.
func attentionPutEcho(r attention.Record, p attention.Parsed) attentionPut {
	out := attentionPut{Session: r.Session, Turn: r.Turn, Items: []attentionPutItem{}, Seen: p.Seen, Read: p.Read}
	for _, it := range r.Items {
		if it.Kind != attention.KindQuestion && it.Kind != attention.KindAction {
			continue
		}
		out.Items = append(out.Items, attentionPutItem{Kind: it.Kind, N: it.N, Actor: it.Actor, Text: it.Text, Rec: it.Rec})
	}
	return out
}

// herdrRuns reports whether the caller's herdr server lists session in pane,
// and socket is that server.
func herdrRuns(h attention.Herdr, session, pane, socket string) bool {
	if !h.Known || pane == "" || socket == "" || socket != h.Socket {
		return false
	}
	for _, a := range h.Agents {
		if a.Pane == pane && a.Session == session {
			return true
		}
	}
	return false
}

func runAttentionRm(cmd *cobra.Command, deps module.Deps, o attentionOpts) error {
	if !attention.ValidSession(o.session) {
		return attentionUsage("attention rm: --session must be 1 to 64 letters, digits, '_' or '-'")
	}
	base, err := attentionStateBase(deps)
	if err != nil {
		return err
	}
	s, err := attention.OpenSpool(base, false)
	if errors.Is(err, attention.ErrNoSpool) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // nothing buffered
	return s.Remove(o.session)
}

// attentionList is `list --json`.
type attentionList struct {
	Rows    []attention.Row  `json:"rows"`
	Skipped int              `json:"skipped"`
	Sources attentionSources `json:"sources"`
}

// attentionSources says which sources were read: ok, absent (nothing there
// yet), error or malformed (counted in skipped where it is a record or file),
// unavailable (herdr could not be asked).
type attentionSources struct {
	Spool string `json:"spool"`
	Desk  string `json:"desk"`
	Herdr string `json:"herdr"`
	Sweep string `json:"sweep"`
}

func runAttentionList(cmd *cobra.Command, deps module.Deps, o attentionOpts) error {
	if o.session != "" && !attention.ValidSession(o.session) {
		return attentionUsage("attention list: --session must be 1 to 64 letters, digits, '_' or '-'")
	}
	base, err := attentionStateBase(deps)
	if err != nil {
		return err
	}
	now := attentionNow().UTC()
	h := attentionHerdr(cmd.Context(), deps)
	out := attentionList{Sources: attentionSources{Spool: sourceOK, Herdr: sourceOK}}
	if !h.Known {
		out.Sources.Herdr = sourceUnavailable
	}

	var recs []attention.Record
	var spoolErr error
	s, err := attention.OpenSpool(base, false)
	switch {
	case errors.Is(err, attention.ErrNoSpool):
		out.Sources.Spool = sourceAbsent
	case err != nil:
		// An unsafe or unreadable spool hides only the session rows: the
		// desk and sweep still report, and sources.spool says why rows are
		// missing.
		out.Sources.Spool = sourceError
		spoolErr = err
	default:
		recs, out.Skipped, err = s.List(now, deps.Cfg.Attention.TTL(), h.Liveness())
		_ = s.Close() // read-only use; nothing to flush
		if err != nil {
			out.Sources.Spool = sourceError
			spoolErr = err
		}
	}

	items, deskState := attentionDesk()
	out.Sources.Desk = deskState

	sw, sweepSkipped, sweepState := attentionSweep(attention.SweepPath(base))
	out.Sources.Sweep = sweepState
	out.Skipped += sweepSkipped

	out.Rows = attention.Build(attention.Input{
		Now: now, Records: recs, Herdr: h, Desk: items, Sweep: sw,
		WorkerPrefix: deps.Cfg.Attention.Prefix(), Session: o.session,
	})
	if out.Rows == nil {
		out.Rows = []attention.Row{}
	}
	if spoolErr != nil {
		// Like a degraded `status` section: reported, not fatal.
		fmt.Fprintf(cmd.ErrOrStderr(), "attention list: spool not read: %v\n", spoolErr) //nolint:errcheck // best-effort note on stderr
	}
	if o.asJSON {
		return writeJSON(cmd.OutOrStdout(), out)
	}
	return printAttentionList(cmd.OutOrStdout(), cmd.ErrOrStderr(), out)
}

// attentionDesk reads the desk's waiting items through a read-only open:
// desk.OpenReadOnly creates, pins, moves and chmods nothing, and its Scan
// computes a missing hash in memory and reports a changed item as refused
// where it lies. A desk that is absent or unreadable gives no rows.
func attentionDesk() ([]attention.DeskItem, string) {
	// A missing home matters only when no env var names the desk, and then
	// ResolveDir refuses on its own.
	home, _ := attentionUserHome()
	dir, err := desk.ResolveDir(attentionGetenv, home)
	if err != nil {
		return nil, sourceError
	}
	d, err := desk.OpenReadOnly(dir)
	if errors.Is(err, desk.ErrNoDesk) {
		return nil, sourceAbsent
	}
	if err != nil {
		return nil, sourceError
	}
	defer d.Close() //nolint:errcheck // read-only
	snap, err := d.Scan()
	if err != nil {
		return nil, sourceError
	}
	var items []attention.DeskItem
	for _, it := range snap.Pending {
		if it.State != desk.StateWaiting {
			continue
		}
		di := attention.DeskItem{Name: it.Name, What: it.What, SHA256: it.Meta.SHA256, SignalPane: it.Meta.SignalPane}
		if it.Meta.AddedAt != nil {
			di.AddedAt = *it.Meta.AddedAt
		}
		items = append(items, di)
	}
	return items, sourceOK
}

// attentionSweep reads `forgectl sweep`'s last result. Absent is no rows; a
// file that is not a regular file, is too large, or does not validate is
// one skipped entry.
func attentionSweep(path string) (*attention.Sweep, int, string) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, 0, sourceAbsent
	}
	if err != nil {
		return nil, 1, sourceMalformed
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, 1, sourceMalformed
	}
	data, err := io.ReadAll(io.LimitReader(f, attentionSweepMax+1))
	if err != nil || len(data) > attentionSweepMax {
		return nil, 1, sourceMalformed
	}
	sw, skipped, err := attention.ParseSweep(data)
	if err != nil {
		return nil, 1, sourceMalformed
	}
	return &sw, skipped, sourceOK
}

// readTail reads r to EOF and returns its last keep bytes, refusing input
// over limit. When it had to cut, the partial first line is dropped, so the
// tail starts on a whole line. A cut inside a fenced block can still leave
// the fence state inverted; that is accepted for replies over keep bytes.
func readTail(r io.Reader, keep, limit int) (string, error) {
	buf := make([]byte, 0, keep)
	chunk := make([]byte, 32<<10)
	total, cut := 0, false
	for {
		n, err := r.Read(chunk)
		total += n
		if total > limit {
			return "", fmt.Errorf("stdin is over %d MiB", limit>>20)
		}
		buf = append(buf, chunk[:n]...)
		if len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
			cut = true
		}
		if errors.Is(err, io.EOF) {
			if i := bytes.IndexByte(buf, '\n'); cut && i >= 0 {
				buf = buf[i+1:]
			}
			return string(buf), nil
		}
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
	}
}

// printAttentionList is the text view: one line per row, through safeText.
func printAttentionList(out, errOut io.Writer, l attentionList) error {
	w := &stickyWriter{w: out}
	if len(l.Rows) == 0 {
		w.printf("nothing waiting\n")
	}
	for _, r := range l.Rows {
		label := r.Tag
		if r.N > 0 && (r.Kind == attention.KindQuestion || r.Kind == attention.KindAction) {
			label = fmt.Sprintf("%s%d", r.Tag, r.N)
		}
		var extra []string
		if r.Worker != "" {
			extra = append(extra, "worker "+r.Worker)
		} else if r.Origin == attention.OriginWorker {
			extra = append(extra, "worker")
		}
		if r.Pane != "" {
			extra = append(extra, "pane "+r.Pane)
		} else if r.Session != "" {
			extra = append(extra, "not in herdr")
		}
		if r.NotOnDesk {
			extra = append(extra, "not on desk")
		}
		line := fmt.Sprintf("%-6s %-10s %s", r.Kind, label, r.Text)
		if len(extra) > 0 {
			line += "  (" + strings.Join(extra, ", ") + ")"
		}
		w.printf("%s\n", safeText(line))
	}
	if w.err != nil {
		return w.err
	}
	if l.Skipped > 0 {
		_, err := fmt.Fprintf(errOut, "attention: skipped %d malformed entries\n", l.Skipped)
		return err
	}
	return nil
}
