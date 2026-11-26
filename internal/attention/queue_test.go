package attention

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func herdrWith(agents ...Agent) Herdr {
	return Herdr{Known: true, Socket: "/tmp/h.sock", Agents: agents}
}

func TestBuildCostOrder(t *testing.T) {
	in := Input{
		Now: t0,
		Asks: []AskItem{
			{ID: "ask-0000000b", Title: "Newer ask", Bucket: "now", At: t0.Add(-1 * time.Minute)},
			{ID: "ask-0000000a", Title: "Older ask", Bucket: "soon", At: t0.Add(-9 * time.Minute)},
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
	}{{RowWorker, 1}, {RowDesk, 2}, {RowPR, 3}, {RowAsk, 4}, {RowAsk, 4}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i].Kind != w.kind || rows[i].Cost != w.cost {
			t.Errorf("row %d = %s/%d, want %s/%d", i, rows[i].Kind, rows[i].Cost, w.kind, w.cost)
		}
	}
	if rows[3].Tag != "ask-0000000a" || rows[4].Tag != "ask-0000000b" {
		t.Errorf("ties: asks are %q then %q, want the older ask first", rows[3].Tag, rows[4].Tag)
	}
	if rows[3].Text != "Older ask" || rows[3].Bucket != "soon" || rows[3].Tier != TierCount {
		t.Errorf("ask row = %+v", rows[3])
	}
	if rows[2].Tag != "acme/widget#7" || rows[2].Text != "Add widget" {
		t.Errorf("pr row = %+v", rows[2])
	}
}

// Ask text comes from any process that can append to the log, so it is
// cleaned like sweep text, and an ask whose id or bucket is not the store's
// shape gains nothing from it.
func TestBuildAskRowIsUntrusted(t *testing.T) {
	long := strings.Repeat("x", 3*MaxText)
	rows := Build(Input{Now: t0, WorkerPrefix: "cos-", Asks: []AskItem{
		{ID: "ask-00000001", Title: "Sign\x1b]0;pwned\x07 in\u202e now", Bucket: "now", At: t0},
		{ID: "../ask-bad", Title: "bad id", Bucket: "now", At: t0},
		{ID: "ask-00000002", Title: long, Bucket: "urgent", From: "cos-fix-1", At: t0.Add(time.Second)},
		{ID: "ask-00000003", Title: "", Bucket: "soon", From: "cos-../x", At: t0.Add(2 * time.Second)},
	}})
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3 (the bad id dropped)", rows)
	}
	if strings.ContainsAny(rows[0].Text, "\x1b\x07\u202e") || !strings.HasPrefix(rows[0].Text, "Sign") {
		t.Errorf("text = %q, want control and bidi characters dropped", rows[0].Text)
	}
	if n := len([]rune(rows[1].Text)); n != MaxText || rows[1].Bucket != "" || rows[1].Worker != "cos-fix-1" || rows[1].Origin != OriginWorker {
		t.Errorf("row = %+v (%d runes), want the text cut to %d, no bucket, worker origin", rows[1], n, MaxText)
	}
	if rows[2].Text != "ask-00000003" || rows[2].Worker != "" || rows[2].Origin != OriginOwn {
		t.Errorf("row = %+v, want the id as text and no worker for a malformed from", rows[2])
	}
	for _, r := range rows {
		if r.Cost != CostAsk || r.Tier != TierCount {
			t.Errorf("ask row %s: cost %d tier %s, want %d %s whatever its origin", r.Tag, r.Cost, r.Tier, CostAsk, TierCount)
		}
	}
}

func TestBuildMergesByPane(t *testing.T) {
	in := Input{
		Now:   t0,
		Herdr: herdrWith(Agent{Pane: "w1:p1", Session: "s1"}, Agent{Pane: "w1:p5", Session: "s-worker", Names: []string{"cos-w"}}),
		Desk: []DeskItem{
			{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0.Add(-5 * time.Minute)},
			{Name: "05-ship", What: "ship it", SHA256: sha64, SignalPane: "w1:p5", AddedAt: t0.Add(-4 * time.Minute)},
		},
		Sweep:        &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w1:p5", BlockedBy: "desk"}}},
		WorkerPrefix: "cos-",
	}
	rows := Build(in)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want two desk rows (the desk-blocked worker merged in)", rows)
	}
	worker, own := rows[0], rows[1]
	if worker.Kind != RowDesk || worker.Cost != CostBlockedWorker || worker.Worker != "cos-w" || worker.Origin != OriginWorker || worker.Pane != "w1:p5" {
		t.Errorf("worker desk row = %+v", worker)
	}
	if own.Kind != RowDesk || own.Tag != "04-fix" || own.Session != "s1" || own.Pane != "w1:p1" || own.Cost != CostDesk || own.Hash != sha64[:12] {
		t.Errorf("own desk row = %+v", own)
	}
}

func TestBuildSessionFilter(t *testing.T) {
	in := Input{
		Now:   t0,
		Herdr: herdrWith(Agent{Pane: "w1:p1", Session: "s1"}, Agent{Pane: "w1:p2", Session: "s2"}),
		Desk: []DeskItem{
			{Name: "01-a", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0},
			{Name: "02-b", SHA256: sha64, SignalPane: "w1:p2", AddedAt: t0},
		},
		Asks:    []AskItem{{ID: "ask-00000001", Title: "no session", At: t0}},
		Session: "s2",
	}
	rows := Build(in)
	if len(rows) != 1 || rows[0].Session != "s2" || rows[0].Tag != "02-b" {
		t.Fatalf("rows = %+v, want s2's desk row only", rows)
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

// Two herdr rows claim sessA, the real pane w1:p1 and a forger's w9:p9.
// Neither may win: desk and worker rows naming those panes get no session.
func TestBuildSessionTwoPanesClaimIsAmbiguous(t *testing.T) {
	in := Input{
		Now:   t0,
		Herdr: herdrWith(Agent{Pane: "w1:p1", Session: "sessA"}, Agent{Pane: "w9:p9", Session: "sessA"}),
		Desk: []DeskItem{
			{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0.Add(-5 * time.Minute)},
			{Name: "05-other", What: "other", SHA256: sha64, SignalPane: "w9:p9", AddedAt: t0.Add(-4 * time.Minute)},
		},
		Sweep: &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w9:p9", BlockedBy: "prompt"}}},
	}
	rows := Build(in)
	var desks, workers int
	for _, row := range rows {
		switch row.Kind {
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
	if desks != 2 || workers != 1 {
		t.Fatalf("rows = %+v, want 2 desk rows and 1 worker row", rows)
	}

	// The same pane listed twice with the same session is one claim, not two.
	in.Herdr = herdrWith(Agent{Pane: "w1:p1", Session: "sessA"}, Agent{Pane: "w1:p1", Session: "sessA"})
	for _, row := range Build(in) {
		if row.Kind == RowDesk && row.Tag == "04-fix" && row.Session != "sessA" {
			t.Errorf("a repeated identical row made the session ambiguous: %+v", row)
		}
	}
}

// A pane two herdr rows disagree about maps to more than one session: a desk
// item or sweep worker naming it gets no pane and no session.
func TestBuildPaneTwoSessionsClaimIsAmbiguous(t *testing.T) {
	in := Input{
		Now:          t0,
		Herdr:        herdrWith(Agent{Pane: "w1:p1", Session: "s1"}, Agent{Pane: "w1:p1", Session: "s2", Names: []string{"cos-x"}}),
		Desk:         []DeskItem{{Name: "04-fix", What: "fix it", SHA256: sha64, SignalPane: "w1:p1", AddedAt: t0}},
		Sweep:        &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w1:p1", BlockedBy: "prompt"}}},
		WorkerPrefix: "cos-",
	}
	for _, row := range Build(in) {
		if row.Pane != "" || row.Session != "" {
			t.Errorf("%s row naming an ambiguous pane = %+v, want no pane and no session", row.Kind, row)
		}
		if row.Kind == RowDesk && row.Origin != OriginOwn {
			t.Errorf("desk row took a worker origin from an ambiguous pane: %+v", row)
		}
	}
}
