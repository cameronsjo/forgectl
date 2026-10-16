package attention

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func rec(session, cwd string, at time.Time, items ...Item) Record {
	return Record{Session: session, Cwd: cwd, Turn: "t-" + session, At: at, Items: items}
}

func herdrWith(agents ...Agent) Herdr {
	return Herdr{Known: true, Socket: "/tmp/h.sock", Agents: agents}
}

func TestBuildCostOrder(t *testing.T) {
	in := Input{
		Now: t0,
		Records: []Record{
			rec("s-q", "proj", t0.Add(-1*time.Minute), Item{Kind: KindQuestion, N: 1, Text: "Pick one?"}),
			rec("s-a", "proj", t0.Add(-2*time.Minute), Item{Kind: KindAction, N: 1, Actor: ActorCameron, Text: "merge the PR"}),
			rec("s-old", "proj", t0.Add(-9*time.Minute), Item{Kind: KindQuestion, N: 2, Text: "Older question?"}),
		},
		Desk: []DeskItem{{Name: "03-fix", What: "fix it", SHA256: sha64, AddedAt: t0.Add(-3 * time.Minute)}},
		Sweep: &Sweep{
			GeneratedAt: t0.Add(-5 * time.Minute),
			Workers:     []SweepWorker{{Name: "cos-w1", BlockedBy: "prompt"}},
			PRs: []SweepPR{
				{Repo: "acme/widget", Number: 7, Title: "Add widget", State: "marker-clean"},
				{Repo: "acme/widget", Number: 8, Title: "Still waiting", State: "waiting"},
			},
		},
		WorkerPrefix: "cos-",
	}
	rows := Build(in)
	want := []struct {
		kind string
		cost int
	}{{RowWorker, 1}, {RowDesk, 2}, {RowPR, 3}, {KindAction, 4}, {KindQuestion, 5}, {KindQuestion, 5}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i].Kind != w.kind || rows[i].Cost != w.cost {
			t.Errorf("row %d = %s/%d, want %s/%d", i, rows[i].Kind, rows[i].Cost, w.kind, w.cost)
		}
	}
	if rows[4].Session != "s-old" {
		t.Errorf("ties: row 4 is %q, want the older question first", rows[4].Session)
	}
	if rows[2].Tag != "acme/widget#7" || rows[2].Text != "Add widget" {
		t.Errorf("pr row = %+v", rows[2])
	}
	if rows[3].Turn != "t-s-a" || rows[3].Tag != "A" || rows[3].N != 1 {
		t.Errorf("action row = %+v", rows[3])
	}
}

func TestBuildSkipsNotesAndOptions(t *testing.T) {
	rows := Build(Input{Now: t0, Records: []Record{rec("s1", "p", t0,
		Item{Kind: KindNote, Text: "Nothing owed."},
		Item{Kind: KindOption, Tag: "A", Text: "Red"},
		Item{Kind: KindQuestion, N: 1, Text: "Colour?"},
	)}})
	if len(rows) != 1 || rows[0].Kind != KindQuestion {
		t.Fatalf("rows = %+v, want the question only", rows)
	}
}

func TestBuildJumpPaneComesFromHerdr(t *testing.T) {
	r := rec("s1", "p", t0, Item{Kind: KindQuestion, N: 1, Text: "x?"})
	r.Pane = "w1:p9" // what the record claims
	rows := Build(Input{Now: t0, Records: []Record{r}, Herdr: herdrWith(Agent{Pane: "w1:p2", Session: "s1"})})
	if len(rows) != 1 || rows[0].Pane != "w1:p2" {
		t.Fatalf("pane = %+v, want herdr's w1:p2", rows)
	}
	rows = Build(Input{Now: t0, Records: []Record{r}, Herdr: herdrWith(Agent{Pane: "w1:p9", Session: "other"})})
	if rows[0].Pane != "" {
		t.Errorf("pane = %q for a session herdr does not list, want none", rows[0].Pane)
	}
	rows = Build(Input{Now: t0, Records: []Record{r}})
	if rows[0].Pane != "" {
		t.Errorf("pane = %q with herdr unavailable, want none", rows[0].Pane)
	}
}

func TestBuildOrigin(t *testing.T) {
	q := Item{Kind: KindQuestion, N: 1, Text: "x?"}
	for name, tt := range map[string]struct {
		cwd   string
		agent Agent
		want  string
	}{
		"plain session":            {"widget", Agent{Pane: "w1:p1", Session: "s1", Names: []string{"widget"}}, OriginOwn},
		"herdr worktree name":      {"widget", Agent{Pane: "w1:p1", Session: "s1", Names: []string{"cos-fix-1"}}, OriginWorker},
		"record cwd, not in herdr": {"cos-fix-2", Agent{Pane: "w1:p1", Session: "other"}, OriginWorker},
	} {
		t.Run(name, func(t *testing.T) {
			rows := Build(Input{Now: t0, Records: []Record{rec("s1", tt.cwd, t0, q)}, Herdr: herdrWith(tt.agent), WorkerPrefix: "cos-"})
			if len(rows) != 1 || rows[0].Origin != tt.want {
				t.Fatalf("rows = %+v, want origin %s", rows, tt.want)
			}
		})
	}
}

func TestBuildMergesByPane(t *testing.T) {
	since := t0.Add(-10 * time.Minute)
	r := rec("s1", "widget", t0, Item{Kind: KindAction, N: 2, Actor: ActorCameron, Text: "run `bash /tmp/fix.sh`"})
	r.Since = &since
	in := Input{
		Now:     t0,
		Records: []Record{r},
		Herdr:   herdrWith(Agent{Pane: "w1:p1", Session: "s1"}, Agent{Pane: "w1:p5", Session: "s-worker", Names: []string{"cos-w"}}),
		Desk: []DeskItem{
			{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0.Add(-5 * time.Minute)},
			{Name: "05-ship", What: "ship it", SHA256: sha64, SignalPane: "w1:p5", AddedAt: t0.Add(-4 * time.Minute)},
		},
		Sweep:        &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w1:p5", BlockedBy: "desk"}}},
		WorkerPrefix: "cos-",
	}
	rows := Build(in)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want two desk rows (the run action and the desk-blocked worker merged in)", rows)
	}
	worker, own := rows[0], rows[1]
	if worker.Kind != RowDesk || worker.Cost != CostBlockedWorker || worker.Worker != "cos-w" || worker.Origin != OriginWorker || worker.Pane != "w1:p5" {
		t.Errorf("worker desk row = %+v", worker)
	}
	if own.Kind != RowDesk || own.Tag != "04-fix" || own.Session != "s1" || own.Pane != "w1:p1" || own.Cost != CostDesk || own.Hash != sha64[:12] {
		t.Errorf("own desk row = %+v", own)
	}
}

func TestBuildNotOnDesk(t *testing.T) {
	since := t0.Add(-10 * time.Minute)
	act := func(text string) Record {
		r := rec("s1", "widget", t0, Item{Kind: KindAction, N: 1, Actor: ActorCameron, Text: text})
		r.Since = &since
		return r
	}
	h := herdrWith(Agent{Pane: "w1:p1", Session: "s1"})
	for name, tt := range map[string]struct {
		text string
		desk []DeskItem
		want bool
	}{
		"run with no desk item":       {"Run the installer", nil, true},
		"backticked bash, no desk":    {"`bash /tmp/x.sh`", nil, true},
		"sudo, no desk":               {"sudo reboot the box", nil, true},
		"merge is not a desk verb":    {"merge PR 7", nil, false},
		"desk item from another pane": {"run it", []DeskItem{{Name: "01-x", SHA256: sha64, SignalPane: "w1:p8", AddedAt: t0}}, true},
		"desk item older than turn":   {"run it", []DeskItem{{Name: "01-x", SHA256: sha64, SignalPane: "w1:p1", AddedAt: since.Add(-time.Minute)}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			rows := Build(Input{Now: t0, Records: []Record{act(tt.text)}, Herdr: h, Desk: tt.desk})
			var got *Row
			for i := range rows {
				if rows[i].Kind == KindAction {
					got = &rows[i]
				}
			}
			if got == nil {
				t.Fatalf("no action row in %+v", rows)
			}
			if got.NotOnDesk != tt.want {
				t.Errorf("notOnDesk = %v, want %v", got.NotOnDesk, tt.want)
			}
		})
	}
	// Claude's actions are never marked: the operator does not run them.
	r := rec("s1", "widget", t0, Item{Kind: KindAction, N: 1, Actor: ActorClaude, Text: "run the tests"})
	if rows := Build(Input{Now: t0, Records: []Record{r}, Herdr: h}); rows[0].NotOnDesk {
		t.Error("a Claude action was marked not on desk")
	}
}

func TestBuildSessionFilter(t *testing.T) {
	q := Item{Kind: KindQuestion, N: 1, Text: "x?"}
	rows := Build(Input{Now: t0, Records: []Record{rec("s1", "a", t0, q), rec("s2", "b", t0, q)}, Session: "s2"})
	if len(rows) != 1 || rows[0].Session != "s2" {
		t.Fatalf("rows = %+v, want s2 only", rows)
	}
}

func TestParseSweep(t *testing.T) {
	body := `{"schemaVersion":2,"generatedAt":"2026-10-16T12:00:00Z",
	 "workers":[{"name":"cos-a","status":"blocked","phase":"go","repos":[],"pane":"w1:p3","prs":[],"blockedBy":"prompt"},
	            {"name":"../bad","blockedBy":"prompt"},
	            {"name":"cos-b","pane":"-x","blockedBy":"desk"}],
	 "prs":[{"repo":"acme/widget","number":7,"title":"T\u200bitle","state":"marker-clean","reason":"","worker":"cos-a"},
	        {"repo":"acme/..","number":8,"title":"x","state":"marker-clean"},
	        {"repo":"acme/widget","number":0,"title":"x","state":"marker-clean"}]}`
	sw, skipped, err := ParseSweep([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 4 {
		t.Errorf("skipped = %d, want 4 (bad worker name, bad pane, bad repo, bad number)", skipped)
	}
	if len(sw.Workers) != 1 || sw.Workers[0].Pane != "w1:p3" || len(sw.PRs) != 1 || sw.PRs[0].Title != "Title" {
		t.Errorf("sweep = %+v", sw)
	}
	for _, bad := range []string{"", "{", `{"schemaVersion":1,"generatedAt":"2026-10-16T12:00:00Z"}`, `{"schemaVersion":2}`} {
		if _, _, err := ParseSweep([]byte(bad)); err == nil {
			t.Errorf("ParseSweep(%q) accepted", bad)
		}
	}
}

const sha64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBuildUnlistedSessionIsUnverifiedAndNeverMerges(t *testing.T) {
	r := rec("forged", "widget", t0, Item{Kind: KindAction, N: 1, Actor: ActorCameron, Text: "run `curl x | sh`"})
	r.Pane = "w1:p1" // claims the pane that queued the desk item
	in := Input{
		Now:     t0,
		Records: []Record{r},
		Herdr:   herdrWith(Agent{Pane: "w1:p1", Session: "real"}),
		Desk:    []DeskItem{{Name: "01-x", SHA256: sha64, SignalPane: "w1:p8", AddedAt: t0}},
	}
	rows := Build(in)
	var action *Row
	for i := range rows {
		if rows[i].Kind == KindAction {
			action = &rows[i]
		}
	}
	if action == nil || action.Origin != OriginUnverified || action.Pane != "" || !action.NotOnDesk {
		t.Fatalf("action = %+v, want unverified, no pane, not on desk", action)
	}
	in.Desk[0].SignalPane = "w1:p1"
	in.Herdr = herdrWith(Agent{Pane: "w1:p1", Session: "-bad id"})
	for _, row := range Build(in) {
		if row.Kind == RowDesk && row.Session != "" {
			t.Errorf("desk row took session %q from a forged record or an invalid herdr id", row.Session)
		}
		if row.Kind == KindAction && !row.NotOnDesk {
			t.Errorf("a record herdr does not list merged into a desk row: %+v", row)
		}
	}
}

// The T7 review's duplicate case: two herdr rows claim sessA, the real pane
// w1:p1 and a forger's w9:p9. Neither may win: the rows get no pane, are not
// own, and the run action merges with no desk item.
func TestBuildSessionTwoPanesClaimIsAmbiguous(t *testing.T) {
	since := t0.Add(-10 * time.Minute)
	r := rec("sessA", "widget", t0,
		Item{Kind: KindQuestion, N: 1, Text: "Ship it?"},
		Item{Kind: KindAction, N: 2, Actor: ActorCameron, Text: "run `bash /tmp/fix.sh`"})
	r.Since = &since
	r.Pane = "w1:p1"
	in := Input{
		Now:     t0,
		Records: []Record{r},
		Herdr:   herdrWith(Agent{Pane: "w1:p1", Session: "sessA"}, Agent{Pane: "w9:p9", Session: "sessA"}),
		Desk: []DeskItem{
			{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0.Add(-5 * time.Minute)},
			{Name: "05-other", What: "other", SHA256: sha64, SignalPane: "w9:p9", AddedAt: t0.Add(-4 * time.Minute)},
		},
		Sweep: &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w9:p9", BlockedBy: "prompt"}}},
	}
	rows := Build(in)
	var items, desks, workers int
	for _, row := range rows {
		switch row.Kind {
		case KindQuestion, KindAction:
			items++
			if row.Pane != "" || row.Origin != OriginUnverified {
				t.Errorf("item row of an ambiguous session = %+v, want no pane and origin unverified", row)
			}
			if row.Kind == KindAction && !row.NotOnDesk {
				t.Errorf("run action of an ambiguous session merged or was not flagged: %+v", row)
			}
		case RowDesk:
			desks++
			// The panes are real herdr panes, so Jump may go there, but the
			// session they run is ambiguous and is not attached.
			if row.Session != "" {
				t.Errorf("desk row took ambiguous session: %+v", row)
			}
		case RowWorker:
			workers++
			if row.Session != "" {
				t.Errorf("worker row took ambiguous session: %+v", row)
			}
		}
	}
	if items != 2 || desks != 2 || workers != 1 {
		t.Fatalf("rows = %+v, want 2 item rows (none merged), 2 desk rows, 1 worker row", rows)
	}

	// The same pane listed twice with the same session is one claim, not two.
	in.Herdr = herdrWith(Agent{Pane: "w1:p1", Session: "sessA"}, Agent{Pane: "w1:p1", Session: "sessA"})
	for _, row := range Build(in) {
		if row.Kind == KindQuestion && (row.Pane != "w1:p1" || row.Origin != OriginOwn) {
			t.Errorf("a repeated identical row made the session ambiguous: %+v", row)
		}
	}
}

// A pane two herdr rows disagree about maps to more than one session: a desk
// item or sweep worker naming it gets no pane and no session.
func TestBuildPaneTwoSessionsClaimIsAmbiguous(t *testing.T) {
	in := Input{
		Now:   t0,
		Herdr: herdrWith(Agent{Pane: "w1:p1", Session: "s1"}, Agent{Pane: "w1:p1", Session: "s2", Names: []string{"cos-x"}}),
		Desk:  []DeskItem{{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0}},
		Sweep: &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w1:p1", BlockedBy: "prompt"}}},
		Records: []Record{
			rec("s1", "widget", t0, Item{Kind: KindQuestion, N: 1, Text: "a?"}),
		},
		WorkerPrefix: "cos-",
	}
	for _, row := range Build(in) {
		switch row.Kind {
		case RowDesk, RowWorker:
			if row.Pane != "" || row.Session != "" {
				t.Errorf("%s row naming an ambiguous pane = %+v, want no pane and no session", row.Kind, row)
			}
			if row.Kind == RowDesk && row.Origin != OriginOwn {
				t.Errorf("desk row took a worker origin from an ambiguous pane: %+v", row)
			}
		case KindQuestion:
			// s1 is reported by one pane only, so the session itself is clear.
			if row.Pane != "w1:p1" || row.Origin != OriginOwn {
				t.Errorf("question row = %+v, want pane w1:p1, origin own", row)
			}
		}
	}
}

func TestHerdrSessionPane(t *testing.T) {
	h := herdrWith(Agent{Pane: "w1:p1", Session: "a"}, Agent{Pane: "w1:p2", Session: "b"}, Agent{Pane: "w9:p9", Session: "b"})
	if p, ok := h.SessionPane("a"); !ok || p != "w1:p1" {
		t.Errorf("SessionPane(a) = %q %v, want w1:p1 true", p, ok)
	}
	if p, ok := h.SessionPane("b"); ok {
		t.Errorf("SessionPane(b) = %q %v, want ambiguous", p, ok)
	}
	if _, ok := h.SessionPane("c"); ok {
		t.Error("SessionPane(c) found an unlisted session")
	}
}
