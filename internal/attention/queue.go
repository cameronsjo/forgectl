package attention

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Row kinds.
const (
	RowDesk   = "desk"
	RowWorker = "worker"
	RowPR     = "pr"
	// RowAsk is an open desk ask: something an agent needs the operator to
	// do that is not a script.
	RowAsk = "ask"
)

// Origins.
const (
	OriginOwn    = "own"
	OriginWorker = "worker"
)

// Delay costs, lowest first: what a wait costs while it sits.
const (
	// CostBlockedWorker: a blocked worker is burning its usage block.
	CostBlockedWorker = 1
	CostDesk          = 2
	CostMarkerClean   = 3
	// CostAsk is an open desk ask: an action only the operator can take.
	CostAsk = 4
)

// DefaultWorkerPrefix marks a chief-of-staff worker's session.
const DefaultWorkerPrefix = "cos-"

// Sweep worker blocks and PR states, as `forgectl sweep` writes them.
const (
	BlockedByPrompt = "prompt"
	BlockedByDesk   = "desk"
	PRMarkerClean   = "marker-clean"
)

// maxSweepList caps the entries read from one sweep.json list.
const maxSweepList = 200

var (
	workerRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	repoPartRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	deskNameRe = regexp.MustCompile(`^[0-9]{2,}-[A-Za-z0-9._-]{1,64}$`)
	askIDRe    = regexp.MustCompile(`^ask-[0-9a-f]{8}$`)
)

// Row is one wait in the queue.
type Row struct {
	// Key is a stable id for the wait (RowKey); Tier is its surface tier
	// (RowTier). Both are set by Build.
	Key     string `json:"key"`
	Tier    string `json:"tier"`
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
	// Pane is where Jump goes: the herdr pane that `herdr agent list`
	// reports for the row's session, or for a desk or worker row the pane it
	// names when herdr lists it. Never a pane taken from a record alone.
	Pane string `json:"pane,omitempty"`
	// Terminal is where Jump goes on herdr-app: the hc terminal a desk item
	// was queued from, when hc lists it live now. Never part of the key.
	Terminal string `json:"terminal,omitempty"`
	Worker   string `json:"worker,omitempty"`
	// Tag is the item name on a desk row, the ask id on an ask row, the
	// block on a worker row, and owner/repo#n on a PR row.
	Tag  string `json:"tag,omitempty"`
	N    int    `json:"n,omitempty"`
	Text string `json:"text"`
	// Bucket is an ask row's bucket: now, soon, optional or coming.
	Bucket string `json:"bucket,omitempty"`
	// Hash is a desk item's sha256, first 12 hex characters.
	Hash   string    `json:"hash,omitempty"`
	At     time.Time `json:"at"`
	Cost   int       `json:"cost"`
	Origin string    `json:"origin"`
}

// Agent is one `herdr agent list` row, reduced to what the queue uses.
// Names are the names the agent's session goes by (its worktree, the
// basenames of its directories), for the worker-prefix match.
type Agent struct {
	Pane    string
	Session string
	Status  string
	Names   []string
}

// Herdr is what the caller's herdr server reported. Known is false when it
// could not be asked.
type Herdr struct {
	// Detail is why herdr could not be asked, when Known is false.
	Detail string
	Known  bool
	Socket string
	Agents []Agent
}

// index maps sessions and panes to the one herdr row each belongs to. A
// session that two or more panes report is ambiguous: any process of yours
// can report a session id over the herdr socket, so herdr cannot say which
// pane is the session's, and the session gets no entry. A pane listed by two
// rows that disagree (session or names) is ambiguous the same way and gets no
// entry. Rows with an invalid pane id are ignored.
func (h Herdr) index() (bySession, byPane map[string]Agent) {
	bySession, byPane = map[string]Agent{}, map[string]Agent{}
	badSession, badPane := map[string]bool{}, map[string]bool{}
	for _, a := range h.Agents {
		if !ValidPane(a.Pane) {
			continue
		}
		if prev, dup := byPane[a.Pane]; dup && (prev.Session != a.Session || !slices.Equal(prev.Names, a.Names)) {
			badPane[a.Pane] = true
		} else if !dup {
			byPane[a.Pane] = a
		}
		if !ValidSession(a.Session) {
			continue
		}
		if prev, dup := bySession[a.Session]; dup && prev.Pane != a.Pane {
			badSession[a.Session] = true
		} else if !dup {
			bySession[a.Session] = a
		}
	}
	for p := range badPane {
		delete(byPane, p)
	}
	for s := range badSession {
		delete(bySession, s)
	}
	return bySession, byPane
}

// DeskItem is one waiting desk item, from a read-only scan.
type DeskItem struct {
	Name       string
	What       string
	SHA256     string
	SignalPane string
	// Terminal is the hc terminal the item was queued from, already checked
	// to have the shape of a local hc address.
	Terminal string
	// TerminalCore is the selection of the hc core Terminal belongs to.
	TerminalCore string
	AddedAt      time.Time
}

// Core is what the caller's herdr-core (hc) reported. Known is false when it
// could not be asked; Live holds the addresses of its live terminals.
type Core struct {
	Known  bool
	Detail string
	// Selection is the core that was asked (hcadapter.Selection): a desk
	// item's terminal counts only when it was recorded under the same one.
	Selection string
	Live      map[string]bool
}

// SweepWorker and SweepPR are the parts of sweep.json the queue uses.
type SweepWorker struct {
	Name      string
	Pane      string
	BlockedBy string
}

// SweepPR is one PR sweep.json lists.
type SweepPR struct {
	Repo   string
	Number int
	Title  string
	State  string
	Worker string
}

// Sweep is a validated sweep.json.
type Sweep struct {
	GeneratedAt time.Time
	Workers     []SweepWorker
	PRs         []SweepPR
}

// AskItem is one open desk ask, from a read-only read of the asks log. Its
// fields are untrusted: any process of yours can append to the log.
type AskItem struct {
	ID     string
	Title  string
	Bucket string
	// From is who the ask says asked: a label, never proof.
	From string
	At   time.Time
}

// Input is everything Build merges.
type Input struct {
	Now   time.Time
	Herdr Herdr
	Core  Core
	Desk  []DeskItem
	// Asks are the open (not answered, not snoozed) desk asks.
	Asks  []AskItem
	Sweep *Sweep
	// Drain, when set, is a live drain whose claude launches have been held
	// by a claude-slots check that cannot answer for long enough to need you.
	Drain        *DrainHold
	WorkerPrefix string
	// Session, when set, keeps only that session's rows.
	Session string
}

// deskRow is a desk row with the pane it was queued from, which is the
// merge key whether or not herdr lists that pane.
type deskRow struct {
	row  Row
	pane string
}

// Build merges the sources into one row per wait, ordered by cost, then
// oldest first. A desk item absorbs a worker blocked on that desk item (the
// merged row takes the worker's cost).
func Build(in Input) []Row {
	// An ambiguous session or pane has no entry here, so a session two panes
	// claim has no pane, is not own and merges with no desk item, and a desk
	// or worker row naming an ambiguous pane gets no pane (see index).
	bySession, byPane := in.Herdr.index()
	// paneSession is the session a herdr pane runs, or "" when that session
	// is ambiguous: a desk or worker row is not given a session another pane
	// also claims.
	paneSession := func(a Agent) string {
		if b, ok := bySession[a.Session]; ok && b.Pane == a.Pane {
			return a.Session
		}
		return ""
	}
	isWorker := func(names ...string) bool {
		if in.WorkerPrefix == "" {
			return false
		}
		for _, n := range names {
			if strings.HasPrefix(n, in.WorkerPrefix) {
				return true
			}
		}
		return false
	}

	desk := make([]deskRow, 0, len(in.Desk))
	for _, d := range in.Desk {
		if !deskNameRe.MatchString(d.Name) {
			continue
		}
		r := Row{Kind: RowDesk, Tag: d.Name, Text: Clean(d.What, MaxText), At: d.AddedAt.UTC(), Cost: CostDesk, Origin: OriginOwn}
		if r.Text == "" {
			r.Text = d.Name
		}
		if sha256Re.MatchString(d.SHA256) {
			r.Hash = d.SHA256[:12]
		}
		pane := ""
		if ValidPane(d.SignalPane) {
			pane = d.SignalPane
		}
		if d.Terminal != "" && d.TerminalCore != "" && d.TerminalCore == in.Core.Selection && in.Core.Live[d.Terminal] {
			r.Terminal = d.Terminal
		}
		if a, ok := byPane[pane]; ok && pane != "" {
			r.Pane = pane
			r.Session = paneSession(a)
			if isWorker(a.Names...) {
				r.Origin = OriginWorker
			}
		}
		desk = append(desk, deskRow{row: r, pane: pane})
	}

	var rows []Row
	if sw := in.Sweep; sw != nil {
		rows = append(rows, sweepRows(*sw, desk, byPane, paneSession)...)
	}

	for _, a := range in.Asks {
		if r, ok := askRow(a, isWorker); ok {
			rows = append(rows, r)
		}
	}
	for _, d := range desk {
		rows = append(rows, d.row)
	}
	if in.Drain != nil {
		rows = append(rows, drainRow(*in.Drain))
	}
	if in.Session != "" {
		rows = slices.DeleteFunc(rows, func(r Row) bool { return r.Session != in.Session })
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if c := a.Cost - b.Cost; c != 0 {
			return c
		}
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		if c := strings.Compare(a.Session, b.Session); c != 0 {
			return c
		}
		if c := strings.Compare(a.Tag, b.Tag); c != 0 {
			return c
		}
		return a.N - b.N
	})
	return stamp(rows)
}

// sweepRows turns the sweep's blocked workers and marker-clean PRs into
// rows. A worker blocked on the desk whose pane queued a desk item is merged
// into that item's row instead.
func sweepRows(sw Sweep, desk []deskRow, byPane map[string]Agent, paneSession func(Agent) string) []Row {
	var rows []Row
	for _, w := range sw.Workers {
		if w.BlockedBy == "" {
			continue
		}
		merged := false
		if w.BlockedBy == BlockedByDesk && w.Pane != "" {
			for i := range desk {
				if desk[i].pane != w.Pane {
					continue
				}
				d := &desk[i].row
				d.Cost = min(d.Cost, CostBlockedWorker)
				d.Worker, d.Origin = w.Name, OriginWorker
				merged = true
			}
		}
		if merged {
			continue
		}
		r := Row{Kind: RowWorker, Worker: w.Name, Tag: w.BlockedBy, Text: "blocked by a " + w.BlockedBy, At: sw.GeneratedAt, Cost: CostBlockedWorker, Origin: OriginWorker}
		if w.BlockedBy == BlockedByDesk {
			r.Text = "blocked by a desk item"
		}
		if a, ok := byPane[w.Pane]; ok && w.Pane != "" {
			r.Pane = w.Pane
			r.Session = paneSession(a)
		}
		rows = append(rows, r)
	}
	for _, p := range sw.PRs {
		if p.State != PRMarkerClean {
			continue
		}
		r := Row{Kind: RowPR, Tag: p.Repo + "#" + strconv.Itoa(p.Number), N: p.Number, Text: p.Title, Worker: p.Worker, At: sw.GeneratedAt, Cost: CostMarkerClean, Origin: OriginOwn}
		if p.Worker != "" {
			r.Origin = OriginWorker
		}
		rows = append(rows, r)
	}
	return rows
}

// askRow is the row for one open ask; ok is false for an ask whose id is
// not the shape the store writes. The title is cleaned and cut like every
// other untrusted text. From names a worker only when it has a worker's
// shape and prefix; it is a label the asker chose, so it changes the origin
// and nothing else (an ask row's cost and tier are fixed).
func askRow(a AskItem, isWorker func(...string) bool) (Row, bool) {
	if !askIDRe.MatchString(a.ID) {
		return Row{}, false
	}
	r := Row{Kind: RowAsk, Tag: a.ID, Text: Clean(a.Title, MaxText), At: a.At.UTC(), Cost: CostAsk, Origin: OriginOwn}
	if r.Text == "" {
		r.Text = a.ID
	}
	switch a.Bucket {
	case "now", "soon", "optional", "coming":
		r.Bucket = a.Bucket
	}
	if workerRe.MatchString(a.From) && isWorker(a.From) {
		r.Worker, r.Origin = a.From, OriginWorker
	}
	return r, true
}

// sweepFile is sweep.json as `forgectl sweep` writes it (schema 2). Only the
// fields the queue reads are declared; the rest are ignored.
type sweepFile struct {
	SchemaVersion int     `json:"schemaVersion"`
	GeneratedAt   *string `json:"generatedAt"`
	Workers       []struct {
		Name      string `json:"name"`
		Pane      string `json:"pane"`
		BlockedBy string `json:"blockedBy"`
	} `json:"workers"`
	PRs []struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Worker string `json:"worker"`
	} `json:"prs"`
}

// ParseSweep validates sweep.json. A file that is not JSON, is not schema
// 2, or has no generatedAt is an error; a worker or PR entry with an invalid
// field is dropped and counted.
func ParseSweep(data []byte) (Sweep, int, error) {
	var f sweepFile
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&f); err != nil {
		return Sweep{}, 0, errors.New("sweep.json is not valid JSON")
	}
	if f.SchemaVersion != 2 {
		return Sweep{}, 0, fmt.Errorf("sweep.json schemaVersion is %d, want 2", f.SchemaVersion)
	}
	if f.GeneratedAt == nil {
		return Sweep{}, 0, errors.New("sweep.json has no generatedAt")
	}
	gen, err := time.Parse(time.RFC3339, *f.GeneratedAt)
	if err != nil {
		return Sweep{}, 0, errors.New("sweep.json generatedAt is not an RFC 3339 time")
	}
	sw := Sweep{GeneratedAt: gen.UTC()}
	skipped := 0
	for i, w := range f.Workers {
		if i >= maxSweepList {
			skipped += len(f.Workers) - i
			break
		}
		blocked := w.BlockedBy == "" || w.BlockedBy == BlockedByPrompt || w.BlockedBy == BlockedByDesk
		if !workerRe.MatchString(w.Name) || (w.Pane != "" && !ValidPane(w.Pane)) || !blocked {
			skipped++
			continue
		}
		sw.Workers = append(sw.Workers, SweepWorker{Name: w.Name, Pane: w.Pane, BlockedBy: w.BlockedBy})
	}
	for i, p := range f.PRs {
		if i >= maxSweepList {
			skipped += len(f.PRs) - i
			break
		}
		if !validRepo(p.Repo) || p.Number <= 0 || !validPRState(p.State) || (p.Worker != "" && !workerRe.MatchString(p.Worker)) {
			skipped++
			continue
		}
		sw.PRs = append(sw.PRs, SweepPR{Repo: p.Repo, Number: p.Number, Title: Clean(p.Title, MaxText), State: p.State, Worker: p.Worker})
	}
	return sw, skipped, nil
}

// validRepo accepts owner/repo, each part a GitHub-shaped name that is not
// "." or "..".
func validRepo(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	for _, part := range []string{owner, repo} {
		if !repoPartRe.MatchString(part) || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validPRState(s string) bool {
	switch s {
	case "blocked", PRMarkerClean, "waiting", "draft":
		return true
	}
	return false
}
