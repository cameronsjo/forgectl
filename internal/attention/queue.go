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

// Row kinds beyond the item kinds q and a.
const (
	RowDesk   = "desk"
	RowWorker = "worker"
	RowPR     = "pr"
)

// Origins.
const (
	OriginOwn    = "own"
	OriginWorker = "worker"
	// OriginUnverified is a record whose session herdr does not list: nothing
	// but the record itself vouches for it, and any process of yours can
	// write one, so it is neither own nor worker.
	OriginUnverified = "unverified"
)

// Delay costs, lowest first: what a wait costs while it sits.
const (
	// CostBlockedWorker: a blocked worker is burning its usage block.
	CostBlockedWorker = 1
	CostDesk          = 2
	CostMarkerClean   = 3
	// CostAction is an `Action (Cameron)`.
	CostAction   = 4
	CostQuestion = 5
	// CostProposal is an action Claude offers to take (an `Action (Claude)`
	// or one with no actor): it waits on a go, after every question.
	CostProposal = 6
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
	// runVerbs lead an operator action that belongs on the desk.
	runVerbs = map[string]bool{"run": true, "bash": true, "brew": true, "restart": true, "sudo": true}
)

// Row is one wait in the queue.
type Row struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
	// Pane is where Jump goes: the herdr pane that `herdr agent list`
	// reports for the row's session, or for a desk or worker row the pane it
	// names when herdr lists it. Never a pane taken from a record alone.
	Pane   string `json:"pane,omitempty"`
	Worker string `json:"worker,omitempty"`
	// Tag is "Q" or "A" on an item row, the item name on a desk row, the
	// block on a worker row, and owner/repo#n on a PR row.
	Tag   string `json:"tag,omitempty"`
	N     int    `json:"n,omitempty"`
	Actor string `json:"actor,omitempty"`
	Text  string `json:"text"`
	Rec   string `json:"rec,omitempty"`
	// Turn is the turn an item row came from.
	Turn string `json:"turn,omitempty"`
	// Hash is a desk item's sha256, first 12 hex characters.
	Hash      string    `json:"hash,omitempty"`
	At        time.Time `json:"at"`
	Cost      int       `json:"cost"`
	NotOnDesk bool      `json:"notOnDesk,omitempty"`
	Origin    string    `json:"origin"`
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
	Known  bool
	Socket string
	Agents []Agent
}

// Liveness is the prune view of h.
func (h Herdr) Liveness() Liveness {
	panes := map[string]bool{}
	for _, a := range h.Agents {
		if ValidPane(a.Pane) {
			panes[a.Pane] = true
		}
	}
	return Liveness{Known: h.Known, Socket: h.Socket, Panes: panes}
}

// DeskItem is one waiting desk item, from a read-only scan.
type DeskItem struct {
	Name       string
	What       string
	SHA256     string
	SignalPane string
	AddedAt    time.Time
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

// Input is everything Build merges.
type Input struct {
	Now          time.Time
	Records      []Record
	Herdr        Herdr
	Desk         []DeskItem
	Sweep        *Sweep
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
// oldest first. A desk item absorbs the waits that are the same wait: a
// worker blocked on that desk item (the merged row takes the worker's cost)
// and a session's run-type `Action (Cameron)` when that session's pane
// queued the item during or after the turn. Notes and options are not waits.
func Build(in Input) []Row {
	bySession, byPane := map[string]Agent{}, map[string]Agent{}
	for _, a := range in.Herdr.Agents {
		if !ValidPane(a.Pane) {
			continue
		}
		if _, dup := byPane[a.Pane]; !dup {
			byPane[a.Pane] = a
		}
		if ValidSession(a.Session) {
			if _, dup := bySession[a.Session]; !dup {
				bySession[a.Session] = a
			}
		}
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
		if a, ok := byPane[pane]; ok && pane != "" {
			r.Pane = pane
			if ValidSession(a.Session) {
				r.Session = a.Session
			}
			if isWorker(a.Names...) {
				r.Origin = OriginWorker
			}
		}
		desk = append(desk, deskRow{row: r, pane: pane})
	}

	var rows []Row
	if sw := in.Sweep; sw != nil {
		rows = append(rows, sweepRows(*sw, desk, byPane)...)
	}

	for _, rec := range in.Records {
		agent, inHerdr := bySession[rec.Session]
		// Only a session herdr lists has a pane to merge on: a record's own
		// pane is a claim, so it never joins a desk row.
		matchPane, origin := "", OriginUnverified
		if inHerdr {
			matchPane, origin = agent.Pane, OriginOwn
		}
		if isWorker(rec.Cwd) || (inHerdr && isWorker(agent.Names...)) {
			origin = OriginWorker
		}
		for _, it := range rec.Items {
			r, ok := itemRow(rec, it)
			if !ok {
				continue
			}
			r.Origin = origin
			if inHerdr {
				r.Pane = agent.Pane
			}
			if it.Kind == KindAction && it.Actor == ActorCameron && runVerbs[leadingVerb(it.Text)] {
				if d := onDesk(desk, matchPane, rec.Since); d != nil {
					if d.row.Session == "" {
						d.row.Session = rec.Session
					}
					continue
				}
				r.NotOnDesk = true
			}
			rows = append(rows, r)
		}
	}
	for _, d := range desk {
		rows = append(rows, d.row)
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
	return rows
}

// sweepRows turns the sweep's blocked workers and marker-clean PRs into
// rows. A worker blocked on the desk whose pane queued a desk item is merged
// into that item's row instead.
func sweepRows(sw Sweep, desk []deskRow, byPane map[string]Agent) []Row {
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
			if ValidSession(a.Session) {
				r.Session = a.Session
			}
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

// itemRow is the row for one question or action; ok is false for a note or
// an option.
func itemRow(rec Record, it Item) (Row, bool) {
	r := Row{Kind: it.Kind, Session: rec.Session, N: it.N, Text: it.Text, Rec: it.Rec, Turn: rec.Turn, At: rec.At}
	switch it.Kind {
	case KindQuestion:
		r.Tag, r.Cost = "Q", CostQuestion
	case KindAction:
		r.Tag, r.Actor, r.Cost = "A", it.Actor, CostProposal
		if it.Actor == ActorCameron {
			r.Cost = CostAction
		}
	default:
		return Row{}, false
	}
	return r, true
}

// onDesk returns the desk row a run action is waiting on: an item queued
// from pane at or after since (the turn's start). nil since accepts any.
func onDesk(desk []deskRow, pane string, since *time.Time) *deskRow {
	if pane == "" {
		return nil
	}
	for i := range desk {
		d := &desk[i]
		if d.pane == pane && (since == nil || !d.row.At.Before(*since)) {
			return d
		}
	}
	return nil
}

// leadingVerb is an action text's first word, lower-cased, without
// surrounding markdown or punctuation.
func leadingVerb(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(f[0], "`*_\"'([{:.,;!?)]}"))
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
