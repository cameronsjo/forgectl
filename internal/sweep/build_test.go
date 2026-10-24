package sweep

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func brief(name, phase string, repos ...string) Brief {
	marks := map[string]time.Time{"launched": t0.Add(-3 * time.Hour)}
	if phase == PhasePlan || phase == PhaseGo || phase == PhaseReport {
		marks["plan"] = t0.Add(-2 * time.Hour)
	}
	if phase == PhaseGo || phase == PhaseReport {
		marks["go"] = t0.Add(-90 * time.Minute)
	}
	if phase == PhaseReport {
		marks["report"] = t0.Add(-time.Hour)
	}
	return Brief{Name: name, Repos: repos, Phase: phase, Marks: marks, Latest: t0.Add(-time.Hour)}
}

func openPR(repo string, n int, mut func(*PRFacts)) PRFacts {
	f := PRFacts{Repo: repo, Number: n, Title: "fix " + repo, Head: headA, Mergeable: "MERGEABLE", Checks: "SUCCESS", CreatedAt: t0.Add(-time.Hour).Format(time.RFC3339)}
	if mut != nil {
		mut(&f)
	}
	return f
}

func base() Input {
	return Input{Now: t0, Since: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Reviewers: []string{"pr-reviewer"},
		Sources: Sources{GitHub: "ok", Herdr: "ok", Desk: "ok", Briefs: "ok"}}
}

func opinion(l Ledger, id string) (Opinion, bool) {
	for _, o := range l.Opinions {
		if o.ID == id {
			return o, true
		}
	}
	return Opinion{}, false
}

func workerOf(t *testing.T, l Ledger, name string) Worker {
	t.Helper()
	for _, w := range l.Workers {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no worker %s in %+v", name, l.Workers)
	return Worker{}
}

func TestActionDerivation(t *testing.T) {
	cleanAtHead := marker("pr-reviewer", headA, 0, 0)
	type want struct {
		id, action string
		target     Target
	}
	for _, tc := range []struct {
		name   string
		in     func(*Input)
		want   []want
		absent []string
	}{
		{
			name: "blocked at a prompt yields unblock for the worker",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhaseGo, "acme/widget")}
				in.Agents = []Agent{{Pane: "w1:p1", Status: "blocked", Names: []string{"cos-widget-3"}}}
			},
			want: []want{{"worker:cos-widget:blocked", ActionUnblock, Target{Worker: "cos-widget"}}},
		},
		{
			name: "plan ready with no GO never yields an action",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhasePlan, "acme/widget")}
				in.Agents = []Agent{{Pane: "w1:p1", Status: "idle", Names: []string{"cos-widget"}}}
			},
			want: []want{{"worker:cos-widget:plan", ActionNone, Target{}}},
		},
		{
			name: "report with a PR awaiting review yields review of that PR",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhaseReport, "acme/widget")}
				in.Agents = []Agent{{Pane: "w1:p1", Status: "idle", Names: []string{"cos-widget"}}}
				in.Open = []PRFacts{openPR("acme/widget", 9, nil), openPR("acme/widget", 7, nil)}
			},
			want: []want{{"worker:cos-widget:report", ActionReview, Target{Repo: "acme/widget", Number: 7}}},
		},
		{
			name: "report whose PRs are all marker-clean has nothing to review",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhaseReport, "acme/widget")}
				in.Open = []PRFacts{openPR("acme/widget", 7, func(f *PRFacts) { f.Reviews = []string{cleanAtHead} })}
			},
			want: []want{{"worker:cos-widget:report", ActionNone, Target{}}},
		},
		{
			name: "a draft PR is not awaiting review",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhaseReport, "acme/widget")}
				in.Open = []PRFacts{openPR("acme/widget", 7, func(f *PRFacts) { f.Draft = true })}
			},
			want: []want{{"worker:cos-widget:report", ActionNone, Target{}}},
		},
		{
			name: "a plan whose worker has no live agent raises no opinion",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhasePlan, "acme/widget")}
			},
			absent: []string{"worker:cos-widget:plan"},
		},
		{
			name: "a plan younger than ten minutes raises no opinion",
			in: func(in *Input) {
				b := brief("cos-widget", PhasePlan, "acme/widget")
				b.Marks["plan"] = t0.Add(-3 * time.Minute)
				in.Briefs = []Brief{b}
				in.Agents = []Agent{{Pane: "w1:p1", Status: "idle", Names: []string{"cos-widget"}}}
			},
			absent: []string{"worker:cos-widget:plan"},
		},
		{
			name: "a working worker raises none of the per-worker opinions",
			in: func(in *Input) {
				in.Briefs = []Brief{brief("cos-widget", PhaseReport, "acme/widget")}
				in.Agents = []Agent{{Pane: "w1:p1", Status: "working", Names: []string{"cos-widget"}}}
			},
			absent: []string{"worker:cos-widget:report", "worker:cos-widget:blocked"},
		},
	} {
		in := base()
		tc.in(&in)
		l := Build(in)
		for _, w := range tc.want {
			o, ok := opinion(l, w.id)
			if !ok {
				t.Errorf("%s: no opinion %s in %+v", tc.name, w.id, l.Opinions)
				continue
			}
			if o.Action != w.action {
				t.Errorf("%s: %s action = %s, want %s", tc.name, w.id, o.Action, w.action)
			}
			got := Target{}
			if o.Target != nil {
				got = *o.Target
			}
			if got != w.target {
				t.Errorf("%s: %s target = %+v, want %+v", tc.name, w.id, got, w.target)
			}
		}
		for _, id := range tc.absent {
			if _, ok := opinion(l, id); ok {
				t.Errorf("%s: unexpected opinion %s", tc.name, id)
			}
		}
	}
}

func TestPlanReadyNeverCarriesATarget(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-widget", PhasePlan, "acme/widget")}
	in.Agents = []Agent{{Pane: "w1:p1", Status: "idle", Names: []string{"cos-widget"}}}
	in.Open = []PRFacts{openPR("acme/widget", 7, nil)}
	seen := false
	for _, o := range Build(in).Opinions {
		seen = seen || strings.HasSuffix(o.ID, ":plan")
		if strings.HasSuffix(o.ID, ":plan") && (o.Action != ActionNone || o.Target != nil) {
			t.Errorf("plan-ready opinion offers an action: %+v", o)
		}
	}
	if !seen {
		t.Error("no plan-ready opinion was raised, so the check saw nothing")
	}
}

func TestBlockedBy(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a"), brief("cos-b", PhaseGo, "acme/b"), brief("cos-c", PhaseGo, "acme/c")}
	in.Agents = []Agent{
		{Pane: "w1:p1", Status: "working", Names: []string{"cos-a"}}, // desk item from this pane
		{Pane: "w1:p2", Status: "blocked", Names: []string{"cos-b"}}, // prompt: no desk item
		{Pane: "w1:p3", Status: "working", Names: []string{"cos-c"}}, // neither
	}
	in.DeskPanes = map[string]bool{"w1:p1": true}
	l := Build(in)
	for name, want := range map[string]string{"cos-a": attention.BlockedByDesk, "cos-b": attention.BlockedByPrompt, "cos-c": ""} {
		if got := workerOf(t, l, name).BlockedBy; got != want {
			t.Errorf("%s blockedBy = %q, want %q", name, got, want)
		}
	}
	if w := workerOf(t, l, "cos-a"); w.Pane != "w1:p1" {
		t.Errorf("pane = %q", w.Pane)
	}
}

func TestDeskPaneBlocksEvenWhenHerdrCallsItBlocked(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a")}
	in.Agents = []Agent{{Pane: "w1:p1", Status: "blocked", Names: []string{"cos-a"}}}
	in.DeskPanes = map[string]bool{"w1:p1": true}
	if got := workerOf(t, Build(in), "cos-a").BlockedBy; got != attention.BlockedByDesk {
		t.Errorf("blockedBy = %q, want desk", got)
	}
}

func TestAgentMatchesTheLongestBriefName(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-bosun", PhaseGo, "acme/bosun"), brief("cos-bosun668", PhaseGo, "acme/bosun")}
	in.Agents = []Agent{{Pane: "w1:p1", Status: "idle", Names: []string{"cos-bosun668-12"}}}
	l := Build(in)
	if got := workerOf(t, l, "cos-bosun668").Pane; got != "w1:p1" {
		t.Errorf("cos-bosun668 pane = %q", got)
	}
	if got := workerOf(t, l, "cos-bosun").Pane; got != "" {
		t.Errorf("cos-bosun took the pane of cos-bosun668-12: %q", got)
	}
}

func TestPRsJoinWorkersByRepo(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a"), brief("cos-b", PhaseGo, "acme/shared"), brief("cos-c", PhaseGo, "acme/shared")}
	in.Open = []PRFacts{
		openPR("acme/a", 1, nil),
		openPR("acme/shared", 2, nil),
		openPR("acme/elsewhere", 3, nil),
	}
	l := Build(in)
	if len(l.PRs) != 2 || l.OpenPRs != 2 {
		t.Fatalf("prs = %+v", l.PRs)
	}
	byNum := map[int]PR{}
	for _, p := range l.PRs {
		byNum[p.Number] = p
	}
	if byNum[1].Worker != "cos-a" || byNum[2].Worker != "" {
		t.Errorf("workers = %q (own repo), %q (shared repo: ambiguous, so none)", byNum[1].Worker, byNum[2].Worker)
	}
	if w := workerOf(t, l, "cos-a"); len(w.PRs) != 1 || w.PRs[0] != (Ref{Repo: "acme/a", Number: 1}) {
		t.Errorf("worker prs = %+v", w.PRs)
	}
}

func TestStaleHeadMarkerLedger(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseReport, "acme/a")}
	in.Open = []PRFacts{
		openPR("acme/a", 1, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headB, 0, 0)} }),
		openPR("acme/a", 2, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)} }),
	}
	got := map[int]string{}
	for _, p := range Build(in).PRs {
		got[p.Number] = p.State
	}
	if got[1] != StateWaiting || got[2] != StateMarkerClean {
		t.Errorf("states = %v", got)
	}
}

func TestExternalTextIsCleaned(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a")}
	in.Open = []PRFacts{openPR("acme/a", 1, func(f *PRFacts) {
		f.Title = "evil\u202etitle\x1b[31m" + strings.Repeat("x", 500)
	})}
	title := Build(in).PRs[0].Title
	if strings.ContainsAny(title, "\u202e\x1b") || len([]rune(title)) > attention.MaxText {
		t.Errorf("title not cleaned: %q", title)
	}
}

func TestV1AggregatesAndMerges(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a", "acme/quiet")}
	merge := func(repo string, n int, ago time.Duration, bot bool) MergedPR {
		return MergedPR{Repo: repo, Number: n, Title: "t", ClosedAt: t0.Add(-ago), Bot: bot}
	}
	in.Merged = []MergedPR{
		merge("acme/a", 1, 30*time.Minute, false),
		merge("acme/a", 2, 4*time.Hour, true),
		merge("acme/other", 3, time.Hour, false), // not a swept repo
		merge("acme/a", 4, 100*time.Hour, false), // before Since
	}
	in.Filed = []string{"Security: token leak", "Docs"}
	l := Build(in)
	if l.SchemaVersion != 2 || l.Merged != 2 || l.DependencyBumps != 1 || l.ReposTouched != 1 || l.SweepRepos != 2 || l.Filed != 2 {
		t.Errorf("aggregates = %+v", l)
	}
	if len(l.Recent) != 2 || l.Recent[0].Number != 1 || l.Recent[0].Repo != "a" {
		t.Errorf("recent = %+v", l.Recent)
	}
	data, _ := json.Marshal(l.TopRepos)
	if string(data) != `[["a",2]]` {
		t.Errorf("topRepos = %s", data)
	}
	if _, ok := opinion(l, "sweep:security"); !ok {
		t.Errorf("no security opinion in %+v", l.Opinions)
	}
	if l.GeneratedAt != "2026-10-16T12:00:00Z" {
		t.Errorf("generatedAt = %s", l.GeneratedAt)
	}
}

func TestOpinionsSortUrgentFirst(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a")}
	in.Agents = []Agent{{Pane: "w1:p1", Status: "blocked", Names: []string{"cos-a"}}}
	in.Merged = []MergedPR{{Repo: "acme/a", Number: 1, Title: "t", ClosedAt: t0.Add(-time.Hour)}}
	order := map[string]int{ToneAct: 0, ToneWarn: 1, ToneNote: 2, ToneGood: 3}
	prev := -1
	for _, o := range Build(in).Opinions {
		if order[o.Tone] < prev {
			t.Fatalf("opinions not sorted by tone: %+v", Build(in).Opinions)
		}
		prev = order[o.Tone]
	}
}

// TestLedgerRoundTripsThroughTheQueueReader writes a ledger with this
// package's marshaller and reads it with the attention queue's parser.
func TestLedgerRoundTripsThroughTheQueueReader(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a"), brief("cos-b", PhaseGo, "acme/b")}
	in.Agents = []Agent{
		{Pane: "w1:p1", Status: "blocked", Names: []string{"cos-a"}},
		{Pane: "w1:p2", Status: "working", Names: []string{"cos-b"}},
	}
	in.DeskPanes = map[string]bool{"w1:p2": true}
	in.Open = []PRFacts{
		openPR("acme/a", 1, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)} }),
		openPR("acme/b", 2, nil),
		openPR("acme/b", 3, func(f *PRFacts) { f.Draft = true }),
	}
	data, err := Marshal(Build(in))
	if err != nil {
		t.Fatal(err)
	}
	sw, skipped, err := attention.ParseSweep(data)
	if err != nil || skipped != 0 {
		t.Fatalf("ParseSweep: skipped=%d err=%v", skipped, err)
	}
	if !sw.GeneratedAt.Equal(t0) {
		t.Errorf("generatedAt = %v", sw.GeneratedAt)
	}
	workers := map[string]attention.SweepWorker{}
	for _, w := range sw.Workers {
		workers[w.Name] = w
	}
	if workers["cos-a"].BlockedBy != attention.BlockedByPrompt || workers["cos-a"].Pane != "w1:p1" ||
		workers["cos-b"].BlockedBy != attention.BlockedByDesk {
		t.Errorf("workers = %+v", workers)
	}
	states := map[int]attention.SweepPR{}
	for _, p := range sw.PRs {
		states[p.Number] = p
	}
	if len(states) != 3 || states[1].State != attention.PRMarkerClean || states[1].Repo != "acme/a" || states[1].Worker != "cos-a" ||
		states[2].State != StateWaiting || states[3].State != StateDraft {
		t.Errorf("prs = %+v", sw.PRs)
	}
}

func TestMarshalIsStableAndHasNoNilLists(t *testing.T) {
	data, err := Marshal(Build(base()))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"opinions": []`, `"workers": []`, `"prs": []`, `"recent": []`, `"topRepos": []`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("missing %s in %s", field, data)
		}
	}
}

func TestABlockedPaneBeatsAnotherPaneOnTheSameBrief(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a")}
	in.Agents = []Agent{
		{Pane: "w1:p1", Status: "idle", Names: []string{"cos-a-1"}},
		{Pane: "w1:p2", Status: "blocked", Names: []string{"cos-a-2"}},
	}
	w := workerOf(t, Build(in), "cos-a")
	if w.Pane != "w1:p2" || w.BlockedBy != attention.BlockedByPrompt {
		t.Errorf("worker = %+v", w)
	}
}

func TestPaneIsValidatedBeforeItIsWritten(t *testing.T) {
	in := base()
	in.Briefs = []Brief{brief("cos-a", PhaseGo, "acme/a")}
	in.Agents = []Agent{{Pane: "bad pane\x1b", Status: "idle", Names: []string{"cos-a"}}}
	if p := workerOf(t, Build(in), "cos-a").Pane; p != "" {
		t.Errorf("pane = %q", p)
	}
}

// Two swept repos can share a name under different owners; each keeps its
// own merge count, and the shared name is shown with its owner.
func TestBuild_SameNameUnderTwoOwnersCountsSeparately(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	briefs := []Brief{
		{Name: "cos-a", Repos: []string{"alice/tools"}, Phase: PhaseLaunched, Marks: map[string]time.Time{}},
		{Name: "cos-b", Repos: []string{"bob/tools"}, Phase: PhaseLaunched, Marks: map[string]time.Time{}},
	}
	l := Build(Input{Now: now, Since: now.Add(-24 * time.Hour), Briefs: briefs,
		Merged: []MergedPR{{Repo: "alice/tools", Number: 1, ClosedAt: now.Add(-time.Hour)}},
	})
	var untouched string
	for _, o := range l.Opinions {
		if o.ID == "sweep:untouched" {
			untouched = o.Text
		}
	}
	if untouched != "1 swept repos have no merge yet: bob/tools." {
		t.Errorf("untouched = %q, want bob/tools untouched", untouched)
	}
	if l.ReposTouched != 1 || len(l.TopRepos) != 1 || l.TopRepos[0].Repo != "tools" {
		t.Errorf("ReposTouched = %d, TopRepos = %+v, want 1 and tools", l.ReposTouched, l.TopRepos)
	}

	l = Build(Input{Now: now, Since: now.Add(-24 * time.Hour), Briefs: briefs,
		Merged: []MergedPR{
			{Repo: "alice/tools", Number: 1, ClosedAt: now.Add(-2 * time.Hour)},
			{Repo: "bob/tools", Number: 2, ClosedAt: now.Add(-time.Hour)},
		},
	})
	want := []RepoCount{{Repo: "alice/tools", Count: 1}, {Repo: "bob/tools", Count: 1}}
	if l.ReposTouched != 2 || !reflect.DeepEqual(l.TopRepos, want) {
		t.Errorf("ReposTouched = %d, TopRepos = %+v, want 2 and %+v", l.ReposTouched, l.TopRepos, want)
	}
}
