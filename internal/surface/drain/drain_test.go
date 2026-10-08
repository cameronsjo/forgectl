package drain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func qrow(name, repo string, state worker.QueueState, enqueued time.Time) worker.QueueRow {
	return worker.QueueRow{Name: name, Repo: repo, State: state, EnqueuedAt: enqueued, StateAt: enqueued, LaunchID: "L-" + name}
}

func ours(stage worker.Stage) Ledger {
	return Ledger{State: LedgerOurs, Row: worker.Row{Stage: stage}}
}

func names(rows []worker.QueueRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

func TestHoldsSlot(t *testing.T) {
	cases := map[string]struct {
		state worker.QueueState
		l     Ledger
		want  bool
	}{
		"claimed holds with no ledger row":       {worker.QueueClaimed, Ledger{}, true},
		"launched with a launched ledger row":    {worker.QueueLaunched, ours(worker.StageLaunched), true},
		"needs-you with a launched ledger row":   {worker.QueueNeedsYou, ours(worker.StageLaunched), true},
		"failed with a live ledger row holds":    {worker.QueueFailed, ours(worker.StageLaunched), true},
		"failed with a worktree-stage row holds": {worker.QueueFailed, ours(worker.StageWorktree), true},
		"failed with a pending row holds":        {worker.QueueFailed, ours(worker.StagePending), true},
		"failed with a failed ledger row frees":  {worker.QueueFailed, ours(worker.StageFailed), false},
		"closed ledger row frees":                {worker.QueueLaunched, ours(worker.StageClosed), false},
		"reported frees even while live":         {worker.QueueReported, ours(worker.StageLaunched), false},
		"queued never holds":                     {worker.QueueQueued, ours(worker.StageLaunched), false},
		"another launch's row does not hold":     {worker.QueueFailed, Ledger{State: LedgerOther, Row: worker.Row{Stage: worker.StageLaunched}}, false},
		"absent ledger row frees":                {worker.QueueLaunched, Ledger{State: LedgerAbsent}, false},
		"unreadable ledger holds a live row":     {worker.QueueLaunched, Ledger{State: LedgerUnreadable}, true},
		"unreadable ledger holds a failed row":   {worker.QueueFailed, Ledger{State: LedgerUnreadable}, true},
		"unreadable ledger frees a closed row":   {worker.QueueClosed, Ledger{State: LedgerUnreadable}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := HoldsSlot(qrow("w", "/r", c.state, t0), c.l); got != c.want {
				t.Fatalf("HoldsSlot = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPlanClaims(t *testing.T) {
	s := config.DrainSettings{Cap: 3, PerRepo: 1}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	cases := map[string]struct {
		rows    []worker.QueueRow
		ledgers map[string]Ledger
		s       config.DrainSettings
		want    string
	}{
		"oldest first, one per repo": {
			rows: []worker.QueueRow{qrow("b2", "/b", worker.QueueQueued, at(3)), qrow("a1", "/a", worker.QueueQueued, at(1)),
				qrow("a2", "/a", worker.QueueQueued, at(2)), qrow("c1", "/c", worker.QueueQueued, at(4))},
			s: s, want: "a1,b2,c1",
		},
		"global cap stops claiming": {
			rows: []worker.QueueRow{qrow("a", "/a", worker.QueueQueued, at(1)), qrow("b", "/b", worker.QueueQueued, at(2)),
				qrow("c", "/c", worker.QueueQueued, at(3)), qrow("d", "/d", worker.QueueQueued, at(4))},
			s: s, want: "a,b,c",
		},
		"per_repo 2 allows two in one repo": {
			rows: []worker.QueueRow{qrow("a1", "/a", worker.QueueQueued, at(1)), qrow("a2", "/a", worker.QueueQueued, at(2)),
				qrow("a3", "/a", worker.QueueQueued, at(3))},
			s: config.DrainSettings{Cap: 3, PerRepo: 2}, want: "a1,a2",
		},
		"a failed row with a live ledger row holds its repo": {
			rows:    []worker.QueueRow{qrow("old", "/a", worker.QueueFailed, at(0)), qrow("a1", "/a", worker.QueueQueued, at(1)), qrow("b1", "/b", worker.QueueQueued, at(2))},
			ledgers: map[string]Ledger{"old": ours(worker.StageLaunched)},
			s:       s, want: "b1",
		},
		"held slots count against the global cap": {
			rows: []worker.QueueRow{qrow("x", "/x", worker.QueueClaimed, at(0)), qrow("y", "/y", worker.QueueLaunched, at(0)),
				qrow("a", "/a", worker.QueueQueued, at(1)), qrow("b", "/b", worker.QueueQueued, at(2))},
			ledgers: map[string]Ledger{"y": ours(worker.StageLaunched)},
			s:       s, want: "a",
		},
		"a reported row frees its repo": {
			rows:    []worker.QueueRow{qrow("done", "/a", worker.QueueReported, at(0)), qrow("a1", "/a", worker.QueueQueued, at(1))},
			ledgers: map[string]Ledger{"done": ours(worker.StageLaunched)},
			s:       s, want: "a1",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := names(PlanClaims(c.rows, c.ledgers, c.s)); got != c.want {
				t.Fatalf("PlanClaims = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMatchLedger(t *testing.T) {
	q := qrow("w", "/r", worker.QueueClaimed, t0)
	rows := []worker.Row{{Name: "other"}, {Name: "w", LaunchID: q.LaunchID, Stage: worker.StagePending}}
	if l := MatchLedger(q, rows, nil); l.State != LedgerOurs || l.Row.Stage != worker.StagePending {
		t.Fatalf("matching launch id: %+v", l)
	}
	rows[1].LaunchID = "L-someone-else"
	if l := MatchLedger(q, rows, nil); l.State != LedgerOther {
		t.Fatalf("other launch id: %+v", l)
	}
	if l := MatchLedger(q, rows[:1], nil); l.State != LedgerAbsent {
		t.Fatalf("no row: %+v", l)
	}
	q.LaunchID = ""
	rows[1].LaunchID = ""
	if l := MatchLedger(q, rows, nil); l.State != LedgerOther {
		t.Fatalf("an empty launch id must never match: %+v", l)
	}
}

func TestCheckClaimed(t *testing.T) {
	good := worker.QueueRow{Repo: "/repo", Brief: "fix it", BriefSHA256: worker.BriefSHA256("fix it")}
	if err := CheckClaimed(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mod  func(*worker.QueueRow)
		want string
	}{
		"relative repo": {func(r *worker.QueueRow) { r.Repo = "repo" }, "clean absolute"},
		"unclean repo":  {func(r *worker.QueueRow) { r.Repo = "/a/../repo" }, "clean absolute"},
		"leading @":     {func(r *worker.QueueRow) { r.Brief = "@x"; r.BriefSHA256 = worker.BriefSHA256("@x") }, "queue check"},
		"empty brief":   {func(r *worker.QueueRow) { r.Brief = ""; r.BriefSHA256 = worker.BriefSHA256("") }, "queue check"},
		"hash mismatch": {func(r *worker.QueueRow) { r.Brief = "fix that" }, "expected the enqueued sha256 " + worker.BriefSHA256("fix it")},
		"model flag":    {func(r *worker.QueueRow) { r.Model = "--print" }, "launch options"},
		"profile path":  {func(r *worker.QueueRow) { r.Profile = "../x" }, "launch options"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := good
			c.mod(&r)
			if err := CheckClaimed(r); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestExpireAndPrune(t *testing.T) {
	q := qrow("w", "/r", worker.QueueQueued, t0)
	if c := Expire(q, t0.Add(ExpireAfter)); c.Writes() {
		t.Fatal("expired at exactly 7 days; want strictly older")
	}
	if c := Expire(q, t0.Add(ExpireAfter+time.Second)); c.To != worker.QueueExpired {
		t.Fatalf("Expire past 7 days: %+v", c)
	}
	claimed := qrow("w", "/r", worker.QueueClaimed, t0)
	if c := Expire(claimed, t0.Add(30*ExpireAfter)); c.Writes() {
		t.Fatal("a claimed row expired; only queued rows expire")
	}
	for _, s := range []worker.QueueState{worker.QueueReported, worker.QueueFailed, worker.QueueClosed, worker.QueueExpired} {
		r := qrow("w", "/r", s, t0)
		if Prunable(r, t0.Add(PruneAfter)) || !Prunable(r, t0.Add(PruneAfter+time.Second)) {
			t.Errorf("%s: prune boundary wrong", s)
		}
	}
	for _, s := range []worker.QueueState{worker.QueueQueued, worker.QueueClaimed, worker.QueueLaunched, worker.QueueNeedsYou} {
		if Prunable(qrow("w", "/r", s, t0), t0.Add(10*PruneAfter)) {
			t.Errorf("%s row was prunable; only terminal rows are", s)
		}
	}
}

func TestDecideLaunch(t *testing.T) {
	q := qrow("w", "/r", worker.QueueClaimed, t0)
	failedRow := &worker.Row{Stage: worker.StageFailed, Worktree: "/r/.claude/worktrees/w"}
	cases := map[string]struct {
		attempts  int
		a         Attempt
		to        worker.QueueState
		attemptsN int // -1: unchanged
		pause     PauseKind
		inErr     []string
		clear     bool
	}{
		"success":                                             {0, Attempt{Class: ErrNone}, worker.QueueLaunched, -1, "", nil, false},
		"created nothing retries":                             {0, Attempt{Class: ErrOther, Err: "boom", CreatedNothing: true}, worker.QueueQueued, 1, "", []string{"attempt 1 of 3", "boom"}, true},
		"second failure retries":                              {1, Attempt{Class: ErrOther, Err: "boom", CreatedNothing: true}, worker.QueueQueued, 2, "", nil, true},
		"third failure fails":                                 {2, Attempt{Class: ErrOther, Err: "boom", CreatedNothing: true}, worker.QueueFailed, 3, "", []string{"attempt 3 of 3"}, true},
		"created something fails at once":                     {0, Attempt{Class: ErrOther, Err: "boom", Row: failedRow, Worktree: failedRow.Worktree}, worker.QueueFailed, -1, "", []string{"boom", failedRow.Worktree, "ledger stage failed"}, false},
		"name taken fails, not retried":                       {0, Attempt{Class: ErrNameTaken, Err: "taken", CreatedNothing: true}, worker.QueueFailed, -1, "", []string{"already exists"}, true},
		"invalid row fails, no attempt":                       {0, Attempt{Class: ErrRowInvalid, Err: "bad hash", CreatedNothing: true}, worker.QueueFailed, -1, "", []string{"bad hash"}, true},
		"auth error requeues and pauses":                      {1, Attempt{Class: ErrGitHubAuth, Err: "HTTP 401", CreatedNothing: true}, worker.QueueQueued, -1, PauseGitHubAuth, []string{"HTTP 401"}, true},
		"herdr down requeues and pauses":                      {1, Attempt{Class: ErrHerdrDown, Err: "not running", CreatedNothing: true}, worker.QueueQueued, -1, PauseHerdr, nil, true},
		"herdr down after a worktree fails, too":              {0, Attempt{Class: ErrHerdrDown, Err: "not running", Row: failedRow, Worktree: failedRow.Worktree}, worker.QueueFailed, -1, PauseHerdr, []string{failedRow.Worktree}, false},
		"launch config after a worktree fails and pauses":     {0, Attempt{Class: ErrLaunchConfig, Err: "binary found on PATH", Row: failedRow, Worktree: failedRow.Worktree}, worker.QueueFailed, -1, PauseLaunchConfig, []string{failedRow.Worktree, "binary found on PATH"}, false},
		"launch config with nothing made requeues and pauses": {0, Attempt{Class: ErrLaunchConfig, Err: "posture", CreatedNothing: true}, worker.QueueQueued, -1, PauseLaunchConfig, nil, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := q
			r.Attempts = c.attempts
			d := DecideLaunch(r, c.a)
			if d.To != c.to || d.Pause != c.pause || d.ClearLaunch != c.clear {
				t.Fatalf("DecideLaunch = to %q pause %q clear %v; want %q %q %v", d.To, d.Pause, d.ClearLaunch, c.to, c.pause, c.clear)
			}
			if c.attemptsN < 0 && d.SetAttempts {
				t.Fatalf("attempts changed to %d; this outcome counts no attempt", d.Attempts)
			}
			if c.attemptsN >= 0 && (!d.SetAttempts || d.Attempts != c.attemptsN) {
				t.Fatalf("attempts = %d (set %v), want %d", d.Attempts, d.SetAttempts, c.attemptsN)
			}
			for _, w := range c.inErr {
				if !strings.Contains(d.Error, w) {
					t.Errorf("last_error %q does not name %q", d.Error, w)
				}
			}
		})
	}
}

// TestReconcileTable covers every row of the plan's startup-reconcile table.
func TestReconcileTable(t *testing.T) {
	q := qrow("w", "/repo", worker.QueueClaimed, t0)
	cases := map[string]struct {
		l     Ledger
		to    worker.QueueState
		clear bool
		inErr string
	}{
		"no ledger row":              {Ledger{State: LedgerAbsent}, worker.QueueQueued, true, ""},
		"launch id does not match":   {Ledger{State: LedgerOther, Row: worker.Row{LaunchID: "L-x", Stage: worker.StageLaunched}}, worker.QueueQueued, true, "L-x"},
		"created nothing":            {ours(worker.StageFailed), worker.QueueQueued, true, ""},
		"pending, no ref":            {ours(worker.StagePending), worker.QueueFailed, false, "/repo/.claude/worktrees/w"},
		"worktree, no ref":           {Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageWorktree, Worktree: "/wt/w"}}, worker.QueueFailed, false, "/wt/w"},
		"launched":                   {ours(worker.StageLaunched), worker.QueueLaunched, false, ""},
		"failed having created some": {Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageFailed, Worktree: "/wt/w", Failure: "x"}}, worker.QueueFailed, false, "x"},
		"closed":                     {ours(worker.StageClosed), worker.QueueClosed, false, ""},
		"unreadable stays claimed":   {Ledger{State: LedgerUnreadable, Err: "eio"}, "", false, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ch, _ := Reconcile(q, c.l, Memo{})
			if ch.To != c.to || ch.ClearLaunch != c.clear || ch.SetAttempts {
				t.Fatalf("Reconcile = to %q clear %v attempts %v; want %q %v and no attempt", ch.To, ch.ClearLaunch, ch.SetAttempts, c.to, c.clear)
			}
			if c.inErr != "" && !strings.Contains(ch.Error, c.inErr) {
				t.Errorf("last_error %q does not name %q", ch.Error, c.inErr)
			}
		})
	}
	if ch, _ := Reconcile(qrow("w", "/r", worker.QueueLaunched, t0), Ledger{}, Memo{}); ch.Writes() {
		t.Error("Reconcile changed a row that is not claimed")
	}
}

func TestWatch(t *testing.T) {
	idle := 10 * time.Minute
	launched := qrow("w", "/r", worker.QueueLaunched, t0)
	blockedRow := launched
	blockedRow.State, blockedRow.LastError = worker.QueueNeedsYou, "blocked: permission prompt"
	idleRow := launched
	idleRow.State, idleRow.LastError = worker.QueueNeedsYou, ReasonIdle
	live := ours(worker.StageLaunched)
	read := func(s ready.State) Probe {
		return Probe{State: ProbeRead, Verdict: ready.Verdict{State: s, Blocking: "permission prompt"}}
	}
	now := t0.Add(time.Hour)
	cases := map[string]struct {
		q      worker.QueueRow
		l      Ledger
		p      Probe
		m      Memo
		to     worker.QueueState
		notify bool
		note   bool
		inErr  string
	}{
		"report line marks reported":             {launched, live, Probe{State: ProbeRead, Report: true, Verdict: ready.Verdict{State: ready.StateReady}}, Memo{}, worker.QueueReported, false, false, ""},
		"report wins over blocked":               {blockedRow, live, Probe{State: ProbeRead, Report: true, Verdict: ready.Verdict{State: ready.StateBlocked}}, Memo{}, worker.QueueReported, false, false, ""},
		"blocked enters needs-you and notifies":  {launched, live, read(ready.StateBlocked), Memo{}, worker.QueueNeedsYou, true, false, "permission prompt"},
		"still blocked: no change":               {blockedRow, live, read(ready.StateBlocked), Memo{}, "", false, false, ""},
		"blocked clears on ready":                {blockedRow, live, read(ready.StateReady), Memo{}, worker.QueueLaunched, false, false, ""},
		"blocked clears on not-ready":            {blockedRow, live, read(ready.StateNotReady), Memo{}, worker.QueueLaunched, false, false, ""},
		"ready under idle: unchanged":            {launched, live, read(ready.StateReady), Memo{ReadySince: now.Add(-idle + time.Second)}, "", false, false, ""},
		"ready for idle: needs-you":              {launched, live, read(ready.StateReady), Memo{ReadySince: now.Add(-idle)}, worker.QueueNeedsYou, true, false, ReasonIdle},
		"idle needs-you stays while at prompt":   {idleRow, live, read(ready.StateReady), Memo{ReadySince: now.Add(-2 * idle)}, "", false, false, ""},
		"idle needs-you clears when working":     {idleRow, live, read(ready.StateNotReady), Memo{ReadySince: now.Add(-2 * idle)}, worker.QueueLaunched, false, false, ""},
		"ledger closed maps to closed":           {launched, ours(worker.StageClosed), Probe{}, Memo{}, worker.QueueClosed, false, false, ""},
		"ledger row gone maps to closed":         {blockedRow, Ledger{State: LedgerAbsent}, Probe{}, Memo{}, worker.QueueClosed, false, false, ""},
		"ledger failed maps to failed":           {launched, Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageFailed, Failure: "died"}}, Probe{}, Memo{}, worker.QueueFailed, false, false, "died"},
		"workspace gone maps to failed":          {launched, live, Probe{State: ProbeGone}, Memo{}, worker.QueueFailed, false, false, "gone"},
		"unreadable pane: unchanged, one note":   {launched, live, Probe{State: ProbeUnreadable, Err: "eio"}, Memo{}, "", false, true, ""},
		"still unreadable: no second note":       {launched, live, Probe{State: ProbeUnreadable, Err: "eio"}, Memo{Unreadable: true}, "", false, false, ""},
		"unreadable ledger: unchanged, one note": {launched, Ledger{State: LedgerUnreadable, Err: "eio"}, Probe{}, Memo{}, "", false, true, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ch, _ := Watch(c.q, c.l, c.p, c.m, now, idle)
			if ch.To != c.to || ch.Notify != c.notify || (ch.Note != "") != c.note {
				t.Fatalf("Watch = to %q notify %v note %q; want %q %v note %v", ch.To, ch.Notify, ch.Note, c.to, c.notify, c.note)
			}
			if c.inErr != "" && !strings.Contains(ch.Error, c.inErr) {
				t.Errorf("last_error %q does not name %q", ch.Error, c.inErr)
			}
		})
	}
}

// TestWatchIdleClock walks the idle rule over ticks: the clock starts at the
// first ready verdict, resets on any other, and needs-you is notified once.
func TestWatchIdleClock(t *testing.T) {
	idle := 10 * time.Minute
	q := qrow("w", "/r", worker.QueueLaunched, t0)
	live := ours(worker.StageLaunched)
	rd := Probe{State: ProbeRead, Verdict: ready.Verdict{State: ready.StateReady}}
	busy := Probe{State: ProbeRead, Verdict: ready.Verdict{State: ready.StateNotReady}}
	var m Memo
	var ch Change
	ch, m = Watch(q, live, rd, m, t0, idle)
	if ch.Writes() || !m.ReadySince.Equal(t0) {
		t.Fatalf("first ready: %+v %+v", ch, m)
	}
	ch, m = Watch(q, live, busy, m, t0.Add(5*time.Minute), idle)
	if ch.Writes() || !m.ReadySince.IsZero() {
		t.Fatalf("busy must reset the clock: %+v %+v", ch, m)
	}
	_, m = Watch(q, live, rd, m, t0.Add(6*time.Minute), idle)
	ch, m = Watch(q, live, rd, m, t0.Add(15*time.Minute), idle)
	if ch.Writes() {
		t.Fatalf("9 minutes at the prompt went to needs-you: %+v", ch)
	}
	ch, m = Watch(q, live, rd, m, t0.Add(16*time.Minute), idle)
	if ch.To != worker.QueueNeedsYou || !ch.Notify {
		t.Fatalf("10 minutes at the prompt: %+v", ch)
	}
	ch.Apply(&q)
	ch, _ = Watch(q, live, rd, m, t0.Add(30*time.Minute), idle)
	if ch.Writes() || ch.Notify {
		t.Fatalf("an idle needs-you row notified twice: %+v", ch)
	}
}

func TestPauses(t *testing.T) {
	p := Pauses{}
	if !p.Set(PauseHerdr, "down") || p.Set(PauseHerdr, "down") || !p.Set(PauseConfig, "bad cap") {
		t.Fatal("Set must report only a change")
	}
	if got := p.Reason(); got != "config: bad cap; herdr: down" {
		t.Fatalf("Reason = %q", got)
	}
	if !p.Clear(PauseHerdr) || p.Clear(PauseHerdr) || !p.Paused() {
		t.Fatal("Clear must report only a change")
	}
}

func TestEffectiveStatus(t *testing.T) {
	s := Status{V: 1, Status: StatusRunning, IntervalSeconds: 15, LastTick: t0}
	if got := Effective(s, t0.Add(45*time.Second)); got != StatusRunning {
		t.Fatalf("at 3 intervals: %s", got)
	}
	if got := Effective(s, t0.Add(46*time.Second)); got != StatusStale {
		t.Fatalf("past 3 intervals: %s", got)
	}
	s.Status = StatusStopped
	if got := Effective(s, t0.Add(time.Hour)); got != StatusStopped {
		t.Fatalf("stopped: %s", got)
	}
}

func TestEventsSince(t *testing.T) {
	var lines []byte
	for _, e := range []Event{{Seq: 1}, {Seq: 2}, {Seq: 3}, {Seq: 1, Kind: "second run"}, {Seq: 2}} {
		e.V = EventVersion
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, b...), '\n')
	}
	events, err := ParseEvents(nil, lines)
	if err != nil || len(events) != 5 {
		t.Fatalf("ParseEvents: %d events, %v", len(events), err)
	}
	if got := Since(events, 0); len(got) != 5 {
		t.Fatalf("Since 0 = %d events, want all 5", len(got))
	}
	got := Since(events, 1)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("Since 1 = %+v; want only the second run's seq 2", got)
	}
	if _, err := ParseEvents([]byte(`{"v":2}` + "\n")); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("a version-2 line: %v", err)
	}
}

// TestSettle pins the Loop closer for rows the watch no longer reads: a
// reported or failed row whose worker was closed reads closed.
func TestSettle(t *testing.T) {
	reported := qrow("w", "/r", worker.QueueReported, t0)
	failed := qrow("w", "/r", worker.QueueFailed, t0)
	noLaunch := failed
	noLaunch.LaunchID = ""
	cases := map[string]struct {
		q  worker.QueueRow
		l  Ledger
		to worker.QueueState
	}{
		"reported, ledger closed":           {reported, ours(worker.StageClosed), worker.QueueClosed},
		"reported, ledger row removed":      {reported, Ledger{State: LedgerAbsent}, worker.QueueClosed},
		"failed, ledger closed":             {failed, ours(worker.StageClosed), worker.QueueClosed},
		"failed, ledger row removed":        {failed, Ledger{State: LedgerAbsent}, worker.QueueClosed},
		"failed, name reused by another":    {failed, Ledger{State: LedgerOther, Row: worker.Row{LaunchID: "L-x"}}, worker.QueueClosed},
		"reported, worker still running":    {reported, ours(worker.StageLaunched), ""},
		"failed, worktree still there":      {failed, ours(worker.StageFailed), ""},
		"failed, live ledger row":           {failed, ours(worker.StageWorktree), ""},
		"unreadable ledger":                 {failed, Ledger{State: LedgerUnreadable}, ""},
		"failed with no worker of its own":  {noLaunch, Ledger{State: LedgerAbsent}, ""},
		"launched rows are the watch's job": {qrow("w", "/r", worker.QueueLaunched, t0), ours(worker.StageClosed), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Settle(c.q, c.l); got.To != c.to {
				t.Fatalf("Settle = to %q, want %q", got.To, c.to)
			}
		})
	}
}

// TestReconcileNotesUnreadableOnce: an unreadable ledger under a claimed row
// is one event per change, not one per tick.
func TestReconcileNotesUnreadableOnce(t *testing.T) {
	q := qrow("w", "/r", worker.QueueClaimed, t0)
	bad := Ledger{State: LedgerUnreadable, Err: "eio"}
	c, m := Reconcile(q, bad, Memo{})
	if c.Note == "" || c.Writes() || !m.Unreadable {
		t.Fatalf("first unreadable tick: %+v %+v", c, m)
	}
	c, m = Reconcile(q, bad, m)
	if c.Note != "" {
		t.Fatalf("second unreadable tick noted again: %q", c.Note)
	}
	c, _ = Reconcile(q, ours(worker.StageLaunched), m)
	if c.To != worker.QueueLaunched {
		t.Fatalf("readable again: %+v", c)
	}
}

func TestProbeGoneNamesTheWayOut(t *testing.T) {
	q := qrow("fix-login", "/r", worker.QueueLaunched, t0)
	c, _ := Watch(q, ours(worker.StageLaunched), Probe{State: ProbeGone}, Memo{}, t0, time.Minute)
	if c.To != worker.QueueFailed || !strings.Contains(c.Error, "run surface close fix-login") {
		t.Fatalf("ProbeGone: %+v", c)
	}
}

func TestHeld(t *testing.T) {
	rows := []worker.QueueRow{qrow("a", "/a", worker.QueueClaimed, t0), qrow("b", "/b", worker.QueueFailed, t0), qrow("c", "/c", worker.QueueQueued, t0)}
	got := Held(rows, map[string]Ledger{"b": ours(worker.StageLaunched)})
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("Held = %v, want a,b", got)
	}
}
