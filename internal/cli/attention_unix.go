// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/procstart"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const (
	// attentionSweepMax caps the sweep.json read.
	attentionSweepMax = 4 << 20
	// attentionHerdrTimeout bounds the herdr agent list call.
	attentionHerdrTimeout = 3 * time.Second
	// attentionDetailMax caps a source's reason line.
	attentionDetailMax = 200
	// attentionShortKey is how much of a row key the text view prints.
	attentionShortKey = 6
	// attentionHcTimeout bounds the hc ls call.
	attentionHcTimeout = 3 * time.Second
)

// Source states reported by `list --json`.
const (
	sourceOK          = "ok"
	sourceAbsent      = "absent"
	sourceError       = "error"
	sourceUnavailable = "unavailable"
	sourceMalformed   = "malformed"
	// sourceRemote: the asks live on another host ($FORGECTL_DESK_ASKS_HOST)
	// and were not read.
	sourceRemote = "remote"
)

// attentionAsksHostEnv names the host that holds the desk's asks, when one
// does; desk ask, asks, respond and watch --responses then run there.
const attentionAsksHostEnv = "FORGECTL_DESK_ASKS_HOST"

// Unix-only seams.
var (
	// attentionHerdr asks the caller's herdr server for its agents. Known is
	// false when herdr could not be asked.
	attentionHerdr = func(ctx context.Context, deps module.Deps) attention.Herdr {
		h := attention.Herdr{Socket: attentionGetenv("HERDR_SOCKET_PATH")}
		if deps.Runner == nil {
			h.Detail = "no command runner"
			return h
		}
		ctx, cancel := context.WithTimeout(ctx, attentionHerdrTimeout)
		defer cancel()
		agents, err := herdr.New(deps.Runner).Agents(ctx)
		if err != nil {
			h.Detail = attention.Clean(err.Error(), attentionDetailMax)
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
// is the Claude Code session id (attentionSession); the names are herdr's
// agent name first, then the worktree and the basenames of the agent's
// directories, for the worker-prefix match. A worktree path is free-form, so
// the agent name is the stable label (forgectl#1286).
func attentionAgent(a herdr.Agent) attention.Agent {
	out := attention.Agent{Pane: a.PaneID, Status: a.AgentStatus, Session: attentionSession(a)}
	names := []string{a.Name}
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
	Desk string `json:"desk"`
	// Asks is the desk's asks log: ok, absent (no ask yet), error, or remote
	// (the asks live on $FORGECTL_DESK_ASKS_HOST and were not read).
	Asks  string `json:"asks"`
	Herdr string `json:"herdr"`
	// HerdrDetail is why herdr is unavailable: herdr's own error, cleaned and
	// cut. Empty when herdr answered.
	HerdrDetail string `json:"herdrDetail,omitempty"`
	Sweep       string `json:"sweep"`
	// Core is herdr-core (hc): ok; unavailable with CoreDetail saying why;
	// or absent when no desk item names a terminal, so hc was not asked.
	Core       string `json:"core"`
	CoreDetail string `json:"coreDetail,omitempty"`
	// Drain is drain.json: ok, absent (no drain has run), error or
	// malformed.
	Drain string `json:"drain"`
	// SweepGeneratedAt is when the sweep that sweep.json holds ran, so a
	// reader can tell a stale sweep from an empty one. Absent unless sweep is
	// ok.
	SweepGeneratedAt *time.Time `json:"sweepGeneratedAt,omitempty"`
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
	out := attentionList{Sources: attentionSources{Herdr: sourceOK}}
	if !h.Known {
		out.Sources.Herdr = sourceUnavailable
		out.Sources.HerdrDetail = h.Detail
	}

	items, deskState := attentionDesk()
	out.Sources.Desk = deskState

	asks, asksSkipped, asksState, asksErr := attentionAsks(now)
	out.Sources.Asks = asksState
	out.Skipped += asksSkipped

	sw, sweepSkipped, sweepState := attentionSweep(attention.SweepPath(base))
	out.Sources.Sweep = sweepState
	if sw != nil {
		gen := sw.GeneratedAt
		out.Sources.SweepGeneratedAt = &gen
	}
	out.Skipped += sweepSkipped

	// hc is asked when a desk item names a terminal, or when sweep workers or
	// asks with a sender can be corroborated against a live hc core without
	// auto-starting one (attentionHcRunning checks for an existing core socket).
	var core attention.Core
	out.Sources.Core = sourceAbsent
	if deskNamesATerminal(items) || ((hasSweepWorkers(sw) || hasAskFrom(asks)) && attentionHcRunning()) {
		core = attentionCore(cmd.Context())
		out.Sources.Core = sourceOK
		if !core.Known {
			out.Sources.Core, out.Sources.CoreDetail = sourceUnavailable, core.Detail
		}
	}

	hold, drainState := attentionDrain(now)
	out.Sources.Drain = drainState

	out.Rows = attention.Build(attention.Input{
		Now: now, Herdr: h, Core: core, Desk: items, Asks: asks, Sweep: sw, Drain: hold,
		WorkerPrefix: deps.Cfg.Attention.Prefix(), Session: o.session,
	})
	if out.Rows == nil {
		out.Rows = []attention.Row{}
	}
	if asksErr != nil {
		// Like a degraded `status` section: reported, not fatal.
		fmt.Fprintf(cmd.ErrOrStderr(), "attention list: asks not read: %s\n", attention.Clean(asksErr.Error(), attentionDetailMax)) //nolint:errcheck // best-effort note on stderr
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
		if hcadapter.ValidAddress(it.Meta.Terminal) && hcadapter.ValidSelection(it.Meta.TerminalCore) {
			di.Terminal, di.TerminalCore = it.Meta.Terminal, it.Meta.TerminalCore
		}
		if it.Meta.AddedAt != nil {
			di.AddedAt = *it.Meta.AddedAt
		}
		items = append(items, di)
	}
	return items, sourceOK
}

// attentionAsks reads the desk's open asks through the asks store's
// read-only open: it creates, chmods and locks nothing, so a writer is never
// held up. Answered and still-snoozed asks are left out. Lines that are not
// well-formed records are counted in skipped. When the asks live on another
// host, nothing is read and the state is remote.
func attentionAsks(now time.Time) (items []attention.AskItem, skipped int, state string, err error) {
	if attentionGetenv(attentionAsksHostEnv) != "" && attentionGetenv("DESK_DIR") == "" && attentionGetenv("CLAUDE_DESK_DIR") == "" {
		return nil, 0, sourceRemote, nil
	}
	// A missing home matters only when no env var names the desk, and then
	// ResolveDir refuses on its own.
	home, _ := attentionUserHome()
	dir, err := desk.ResolveDir(attentionGetenv, home)
	if err != nil {
		return nil, 0, sourceError, err
	}
	s, err := ask.OpenReadOnly(dir)
	if err != nil {
		return nil, 0, sourceError, err
	}
	if _, err := os.Lstat(s.LogPath()); errors.Is(err, fs.ErrNotExist) {
		return nil, 0, sourceAbsent, nil
	}
	recs, _, bad, err := s.ReadFrom(0)
	if err != nil {
		return nil, 0, sourceError, err
	}
	for _, it := range ask.Fold(recs, now) {
		if it.State != ask.StateOpen {
			continue
		}
		a := it.Ask
		items = append(items, attention.AskItem{ID: a.ID, Title: a.Title, Bucket: string(a.Bucket), From: a.From, At: a.At})
	}
	return items, bad, sourceOK, nil
}

// attentionDrain reads drain.json read-only and returns the hold the queue
// shows, if any (drainNeedsYou). It takes no lock and writes nothing.
func attentionDrain(now time.Time) (*attention.DrainHold, string) {
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return nil, sourceError
	}
	data, err := files.ReadStatus()
	if err != nil {
		return nil, sourceError
	}
	if data == nil {
		return nil, sourceAbsent
	}
	st, err := drain.DecodeStatus(data)
	if err != nil {
		return nil, sourceMalformed
	}
	return drainNeedsYou(st, now, attentionDrainAlive), sourceOK
}

// attentionDrainAlive reports whether drain.json's process is still the
// drain that wrote it: the same check `surface drain stop` makes before a
// signal (pid and process start time). It takes no lock, so a poll never
// makes a starting drain's LOCK_EX|LOCK_NB fail.
var attentionDrainAlive = func(st drain.Status) bool {
	return checkDrainSignalable(st, procstart.Of) == nil
}

// drainNeedsYou is the drain row's rule: drain.json reads running and its
// last tick is fresh (drain.Effective), a claude-slots fault hold has lasted
// drain.SlotsStuckAfter (drain.SlotsStuck), and the process is still that
// drain. A crashed drain, a stale tick or a full cap (the normal hold) gives
// no row.
func drainNeedsYou(st drain.Status, now time.Time, alive func(drain.Status) bool) *attention.DrainHold {
	if drain.Effective(st, now) != drain.StatusRunning || !drain.SlotsStuck(st.SlotsHold, now) || !alive(st) {
		return nil
	}
	return &attention.DrainHold{Since: st.SlotsHold.Since}
}

// attentionCoreResult is the part of `hc ls --json` the queue reads.
type attentionCoreResult struct {
	Terminals *[]struct {
		Address string            `json:"address"`
		Name    string            `json:"name"`
		State   string            `json:"state"`
		Labels  map[string]string `json:"labels"`
	} `json:"terminals"`
}

// attentionHcRunning reports whether an hc core socket already exists for the
// current environment, so `attention list` never auto-starts a stray hc server
// when no core is running. A seam so tests never inspect the host's sockets.
var attentionHcRunning = func() bool {
	return hcadapter.CoreSocketExists(attentionGetenv)
}

// attentionHcList runs `hc ls --json` through the shared hc adapter
// (newHcClient): the absolute hc path, the launching session's handles
// scrubbed from the environment, sealed argv and capped output, the path
// every other forgectl hc call takes. It also returns the adapter's
// selection, the core it asked. A seam so tests never reach a real hc.
var attentionHcList = func(ctx context.Context) (out []byte, selection string, err error) {
	a, err := newHcClient()
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, attentionHcTimeout)
	defer cancel()
	out, err = a.ListJSON(ctx)
	return out, a.SelectionDigest(), err
}

// attentionCore asks herdr-core for its live terminals and parses their
// canonical agent.* labels. Known is false when hc could not be asked or its
// reply does not parse; Detail then says why, cleaned and cut.
func attentionCore(ctx context.Context) attention.Core {
	c := attention.Core{}
	out, sel, err := attentionHcList(ctx)
	if err != nil {
		c.Detail = attention.Clean(err.Error(), attentionDetailMax)
		return c
	}
	var r attentionCoreResult
	if err := json.Unmarshal(out, &r); err != nil || r.Terminals == nil {
		c.Detail = "hc ls --json: reply has no terminals list"
		return c
	}
	c.Known, c.Selection, c.Live = true, sel, map[string]bool{}
	for _, t := range *r.Terminals {
		if t.State == "live" && hcadapter.ValidAddress(t.Address) {
			c.Live[t.Address] = true
			id := hcadapter.ParseAgentLabels(t.Labels)
			c.Terminals = append(c.Terminals, attention.CoreTerminal{
				Address: t.Address,
				Name:    t.Name,
				Kind:    id.Kind,
				Session: id.Session,
				Worker:  id.Worker,
				Role:    id.Role,
			})
		}
	}
	return c
}

// deskNamesATerminal reports whether any desk item recorded an hc terminal.
func deskNamesATerminal(items []attention.DeskItem) bool {
	for _, it := range items {
		if it.Terminal != "" {
			return true
		}
	}
	return false
}

// hasSweepWorkers reports whether the sweep has any worker entries that hc
// labels can corroborate.
func hasSweepWorkers(sw *attention.Sweep) bool {
	return sw != nil && len(sw.Workers) > 0
}

// hasAskFrom reports whether any open ask names a sender that hc's
// agent.worker labels can corroborate.
func hasAskFrom(asks []attention.AskItem) bool {
	for _, a := range asks {
		if a.From != "" {
			return true
		}
	}
	return false
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

// shortKey is the start of a row key, enough to tell rows apart by eye.
func shortKey(k string) string {
	if len(k) < attentionShortKey {
		return k
	}
	return k[:attentionShortKey]
}

// printAttentionList is the text view: one line per row, through safeText.
func printAttentionList(out, errOut io.Writer, l attentionList) error {
	w := &stickyWriter{w: out}
	if len(l.Rows) == 0 {
		w.printf("nothing waiting\n")
	}
	for _, r := range l.Rows {
		label := r.Tag
		var extra []string
		if r.Bucket != "" {
			extra = append(extra, r.Bucket)
		}
		if r.Worker != "" {
			extra = append(extra, "worker "+r.Worker)
		} else if r.Origin == attention.OriginWorker {
			extra = append(extra, "worker")
		}
		if r.Pane != "" {
			extra = append(extra, "pane "+r.Pane)
		} else if r.Terminal != "" {
			extra = append(extra, "terminal "+r.Terminal)
		} else if r.Session != "" {
			extra = append(extra, "not in herdr")
		}
		line := fmt.Sprintf("%s %-6s %-10s %s", shortKey(r.Key), r.Kind, label, r.Text)
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
