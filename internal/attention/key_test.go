package attention

import (
	"testing"
	"time"
)

func keyOf(t *testing.T, rows []Row, kind, tag string) string {
	t.Helper()
	for _, r := range rows {
		if r.Kind == kind && r.Tag == tag {
			if !ValidKey(r.Key) {
				t.Fatalf("%s/%s key %q is not 16 hex", kind, tag, r.Key)
			}
			return r.Key
		}
	}
	t.Fatalf("no %s/%s row in %+v", kind, tag, rows)
	return ""
}

// A desk row's key ignores everything a later read fills in: the pane and
// session herdr gives it, a worker blocked on it, a session's action merged
// into it.
func TestRowKeyStableAcrossHerdrSweepAndTurns(t *testing.T) {
	desk := []DeskItem{{Name: "04-restart", What: "restart the box", SHA256: sha64, SignalPane: "w1:p2", AddedAt: t0}}
	bare := Build(Input{Now: t0, Desk: desk})
	base := keyOf(t, bare, RowDesk, "04-restart")

	// herdr up: the row gains a pane and a session.
	up := Build(Input{Now: t0, Desk: desk, Herdr: herdrWith(Agent{Pane: "w1:p2", Session: "s1"})})
	if up[0].Pane != "w1:p2" || up[0].Session != "s1" {
		t.Fatalf("herdr did not fill the desk row: %+v", up[0])
	}
	if k := keyOf(t, up, RowDesk, "04-restart"); k != base {
		t.Errorf("herdr flap changed the desk key: %s -> %s", base, k)
	}

	// A worker blocked on the desk merges in and drops the cost to 1.
	merged := Build(Input{Now: t0, Desk: desk, Sweep: &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", Pane: "w1:p2", BlockedBy: BlockedByDesk}}}})
	if merged[0].Worker != "cos-w" || merged[0].Cost != CostBlockedWorker {
		t.Fatalf("worker did not merge: %+v", merged)
	}
	if k := keyOf(t, merged, RowDesk, "04-restart"); k != base {
		t.Errorf("sweep merge changed the desk key: %s -> %s", base, k)
	}
}

// An ask's key is its id: a retitled or re-bucketed copy of the same ask
// keeps its key, and another ask gets another.
func TestRowKeyForAsks(t *testing.T) {
	one := keyOf(t, Build(Input{Now: t0, Asks: []AskItem{{ID: "ask-00000001", Title: "Sign in", Bucket: "now", At: t0}}}), RowAsk, "ask-00000001")
	same := keyOf(t, Build(Input{Now: t0, Asks: []AskItem{{ID: "ask-00000001", Title: "Sign in again", Bucket: "soon", From: "cos-x", At: t0}}, WorkerPrefix: "cos-"}), RowAsk, "ask-00000001")
	if one != same {
		t.Errorf("the same ask changed key: %s -> %s", one, same)
	}
	if other := keyOf(t, Build(Input{Now: t0, Asks: []AskItem{{ID: "ask-00000002", Title: "Sign in", At: t0}}}), RowAsk, "ask-00000002"); other == one {
		t.Errorf("two asks share key %s", one)
	}
}

// A desk row takes its queuing terminal only while hc lists it live, and
// the terminal never enters the key.
func TestBuildDeskTerminalComesFromCore(t *testing.T) {
	const core, other = "0123456789abcdef0123456789abcdef", "ffffffffffffffffffffffffffffffff"
	desk := []DeskItem{{Name: "05-x", What: "x", Terminal: "t148", TerminalCore: core, AddedAt: t0}}
	none := Build(Input{Now: t0, Desk: desk})
	gone := Build(Input{Now: t0, Desk: desk, Core: Core{Known: true, Selection: core, Live: map[string]bool{"t9": true}}})
	// Another core with its own live t148 is not the terminal that queued it.
	elsewhere := Build(Input{Now: t0, Desk: desk, Core: Core{Known: true, Selection: other, Live: map[string]bool{"t148": true}}})
	live := Build(Input{Now: t0, Desk: desk, Core: Core{Known: true, Selection: core, Live: map[string]bool{"t148": true}}})
	if none[0].Terminal != "" || gone[0].Terminal != "" || elsewhere[0].Terminal != "" {
		t.Errorf("terminal without hc vouching: none=%q gone=%q elsewhere=%q", none[0].Terminal, gone[0].Terminal, elsewhere[0].Terminal)
	}
	if live[0].Terminal != "t148" {
		t.Errorf("live terminal = %q, want t148", live[0].Terminal)
	}
	if none[0].Key != live[0].Key {
		t.Errorf("terminal changed the key: %s -> %s", none[0].Key, live[0].Key)
	}
}

func TestRowKeyDistinguishesSameContentDeskItems(t *testing.T) {
	rows := Build(Input{Now: t0, Desk: []DeskItem{
		{Name: "01-restart", What: "restart the box", SHA256: sha64, AddedAt: t0},
		{Name: "02-restart", What: "restart the box", SHA256: sha64, AddedAt: t0},
	}})
	if len(rows) != 2 || rows[0].Key == rows[1].Key {
		t.Fatalf("same-content desk items share a key: %+v", rows)
	}
}

// A desk name can come back once done/ is emptied (nextNumber counts what is
// on disk); a new item under an old name is a new wait with a new key.
func TestRowKeyDistinguishesAReusedDeskName(t *testing.T) {
	a := Build(Input{Now: t0, Desk: []DeskItem{{Name: "01-x", What: "x", SHA256: sha64, AddedAt: t0}}})
	b := Build(Input{Now: t0, Desk: []DeskItem{{Name: "01-x", What: "x", SHA256: sha64, AddedAt: t0.Add(time.Hour)}}})
	if a[0].Key == b[0].Key {
		t.Fatalf("a reused desk name kept its key %s", a[0].Key)
	}
}

// The length prefix keeps field boundaries: ("ab", "c") and ("a", "bc") are
// different workers' tuples and must not collide.
func TestRowKeyIsLengthPrefixed(t *testing.T) {
	a := RowKey(Row{Kind: RowWorker, Worker: "ab", Tag: "c"})
	b := RowKey(Row{Kind: RowWorker, Worker: "a", Tag: "bc"})
	if a == b {
		t.Fatalf("field boundary lost: both %s", a)
	}
}

func TestBuildCollapsesDuplicateSweepEntries(t *testing.T) {
	rows := Build(Input{Now: t0, Sweep: &Sweep{
		GeneratedAt: t0,
		Workers:     []SweepWorker{{Name: "cos-w", BlockedBy: BlockedByPrompt}, {Name: "cos-w", BlockedBy: BlockedByPrompt}, {Name: "cos-w", BlockedBy: BlockedByDesk}},
		PRs:         []SweepPR{{Repo: "acme/w", Number: 3, State: PRMarkerClean}, {Repo: "acme/w", Number: 3, State: PRMarkerClean}},
	}})
	var workers, prs int
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.Key] {
			t.Errorf("duplicate key %s in %+v", r.Key, rows)
		}
		seen[r.Key] = true
		switch r.Kind {
		case RowWorker:
			workers++
		case RowPR:
			prs++
		}
	}
	// One worker blocked two ways is two waits; the repeated prompt block is one.
	if workers != 2 || prs != 1 {
		t.Errorf("workers=%d prs=%d, want 2 and 1: %+v", workers, prs, rows)
	}
}

func TestRowTier(t *testing.T) {
	cases := []struct {
		row  Row
		want string
	}{
		{Row{Kind: RowDesk}, TierRaise},
		{Row{Kind: RowWorker}, TierRaise},
		{Row{Kind: RowDrain}, TierRaise},
		{Row{Kind: RowPR}, TierCount},
		{Row{Kind: RowAsk}, TierCount},
		// An ask row never raises, whatever origin it claims.
		{Row{Kind: RowAsk, Origin: OriginWorker}, TierCount},
		{Row{Kind: "unknown"}, TierHidden},
	}
	for _, c := range cases {
		if got := RowTier(c.row); got != c.want {
			t.Errorf("RowTier(%s/%s) = %s, want %s", c.row.Kind, c.row.Origin, got, c.want)
		}
	}
}

func TestBuildStampsEveryRow(t *testing.T) {
	rows := Build(Input{
		Now:   t0,
		Asks:  []AskItem{{ID: "ask-00000001", Title: "q?", At: t0}, {ID: "ask-00000002", Title: "merge it", At: t0}},
		Desk:  []DeskItem{{Name: "01-x", What: "x", AddedAt: t0}},
		Sweep: &Sweep{GeneratedAt: t0, Workers: []SweepWorker{{Name: "cos-w", BlockedBy: BlockedByPrompt}}, PRs: []SweepPR{{Repo: "a/b", Number: 1, State: PRMarkerClean}}},
	})
	if len(rows) != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if !ValidKey(r.Key) || r.Tier == "" {
			t.Errorf("unstamped row %+v", r)
		}
	}
}
