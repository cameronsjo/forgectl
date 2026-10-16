package sweep

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
)

// opinions computes the ledger's observations from the data, urgent first.
// Each carries an id that is stable across runs (pr:<repo>#<n>,
// worker:<name>:<state>, or sweep:<slug>) and an action the operator can
// start from it.
//
// Action rules:
//   - a worker blocked at a prompt or on a desk item: unblock, target the worker
//   - a worker whose report is in, with PRs awaiting review: review, target the
//     lowest-numbered such PR
//   - a worker whose plan is ready with no GO, and whose agent is still up: none. A GO is an approval, so
//     the row offers Jump only and no action ever stands in for it
//   - everything else: none
func opinions(in Input, briefs []Brief, workers []Worker, prs []PR, merged []MergedPR, perRepo map[string]int, sweepRepos map[string]bool) []Opinion {
	var out []Opinion
	add := func(id, tone, text, action string, target *Target) {
		out = append(out, Opinion{ID: id, Tone: tone, Text: attention.Clean(text, attention.MaxText), Action: action, Target: target})
	}
	briefOf := map[string]Brief{}
	for _, b := range briefs {
		briefOf[b.Name] = b
	}
	// A worker with no live agent is not running, so it is as idle as one
	// that herdr calls idle.
	idle := func(w Worker) bool { return w.Status == "idle" || w.Status == "done" || w.Status == "unknown" }
	prState := map[Ref]string{}
	for _, p := range prs {
		prState[Ref{Repo: p.Repo, Number: p.Number}] = p.State
	}

	for _, w := range workers {
		if w.BlockedBy == "" {
			continue
		}
		what := "a prompt. Read its pane."
		if w.BlockedBy == attention.BlockedByDesk {
			what = "a desk item."
		}
		add(WorkerID(w.Name, "blocked"), ToneAct, fmt.Sprintf("%s is blocked on %s", w.Name, what), ActionUnblock, &Target{Worker: w.Name})
	}
	for _, w := range workers {
		b := briefOf[w.Name]
		// A plan waits on a GO only while its worker is alive to receive it.
		if w.Phase == PhasePlan && idle(w) && w.Status != "unknown" {
			if waited := in.Now.Sub(b.Marks[PhasePlan]); waited >= waitMinutes*time.Minute {
				add(WorkerID(w.Name, "plan"), ToneAct, fmt.Sprintf("%s has had a plan ready for %s with no GO. It is waiting on the orchestrator.", w.Name, ago(waited)), ActionNone, nil)
			}
		}
	}
	for _, w := range workers {
		if w.Phase != PhaseReport || !idle(w) {
			continue
		}
		var awaiting []Ref
		for _, r := range w.PRs {
			if prState[r] == StateWaiting {
				awaiting = append(awaiting, r)
			}
		}
		when := since(in.Now.Sub(briefOf[w.Name].Marks[PhaseReport]))
		id := WorkerID(w.Name, "report")
		if len(awaiting) == 0 {
			add(id, ToneNote, fmt.Sprintf("%s reported %s; no PR of its awaits review.", w.Name, when), ActionNone, nil)
			continue
		}
		sort.Slice(awaiting, func(i, j int) bool {
			if awaiting[i].Repo != awaiting[j].Repo {
				return awaiting[i].Repo < awaiting[j].Repo
			}
			return awaiting[i].Number < awaiting[j].Number
		})
		add(id, ToneAct, fmt.Sprintf("%s reported %s; %d of its PRs await review.", w.Name, when, len(awaiting)),
			ActionReview, &Target{Repo: awaiting[0].Repo, Number: awaiting[0].Number})
	}

	live, busy := 0, 0
	for _, w := range workers {
		if w.Status == "unknown" {
			continue
		}
		live++
		if w.Status == "working" {
			busy++
		}
	}
	if live > 0 {
		tone := ToneNote
		switch {
		case float64(busy) >= float64(live)*0.6:
			tone = ToneGood
		case float64(busy) <= float64(live)*0.3:
			tone = ToneWarn
		}
		add("sweep:busy", tone, fmt.Sprintf("%d of %d workers are working right now.", busy, live), ActionNone, nil)
	}

	const window = 3 * time.Hour
	recentN, prior := 0, 0
	for _, m := range merged {
		age := in.Now.Sub(m.ClosedAt)
		switch {
		case age <= window:
			recentN++
		case age <= 2*window:
			prior++
		}
	}
	if recentN > 0 || prior > 0 {
		trend := "flat"
		if recentN > prior {
			trend = "up"
		} else if recentN < prior {
			trend = "down"
		}
		tone := ToneNote
		if trend == "up" && recentN >= 3 {
			tone = ToneGood
		} else if trend == "down" && recentN == 0 {
			tone = ToneWarn
		}
		add("sweep:pace", tone, fmt.Sprintf("Merge pace is %s: %d in the last 3h against %d in the 3h before.", trend, recentN, prior), ActionNone, nil)
	}
	if n := len(merged); n > 0 {
		if quiet := in.Now.Sub(merged[n-1].ClosedAt); quiet >= 2*time.Hour {
			add("sweep:quiet", ToneWarn, fmt.Sprintf("No merge for %s. Work is in flight, not landing.", ago(quiet)), ActionNone, nil)
		}
		bots := 0
		for _, m := range merged {
			if m.Bot {
				bots++
			}
		}
		share := float64(bots) / float64(n)
		if share >= 0.25 {
			add("sweep:bots", ToneWarn, fmt.Sprintf("%.0f%% of merges are dependency bumps. Counting them as progress flatters the sweep.", share*100), ActionNone, nil)
		} else {
			add("sweep:bots", ToneGood, fmt.Sprintf("Only %.0f%% of merges are dependency bumps; the rest is real work.", share*100), ActionNone, nil)
		}
		if top := topRepos(perRepo, 1); len(top) == 1 {
			add("sweep:churn", ToneNote, fmt.Sprintf("%s carries the most churn: %d of %d merges (%.0f%%).", top[0].Repo, top[0].Count, n, float64(top[0].Count)/float64(n)*100), ActionNone, nil)
		}
		var untouched []string
		for r := range sweepRepos {
			if perRepo[shortName(r)] == 0 {
				untouched = append(untouched, shortName(r))
			}
		}
		sort.Strings(untouched)
		if len(untouched) > 0 {
			list := strings.Join(untouched[:min(6, len(untouched))], ", ")
			if len(untouched) > 6 {
				list += "..."
			}
			add("sweep:untouched", ToneNote, fmt.Sprintf("%d swept repos have no merge yet: %s.", len(untouched), list), ActionNone, nil)
		}
	}

	if oldest, age, ok := oldestOpen(in); ok && age >= 72*time.Hour {
		add(PRID(oldest.Repo, oldest.Number), ToneWarn,
			fmt.Sprintf("Oldest open PR: %s#%d, open %s. Land it, split it, or close it.", shortName(oldest.Repo), oldest.Number, ago(age)), ActionNone, nil)
	}
	drafts := 0
	for _, p := range prs {
		if p.State == StateDraft {
			drafts++
		}
	}
	if drafts > 0 {
		add("sweep:drafts", ToneNote, fmt.Sprintf("%d of %d open PRs are still drafts.", drafts, len(prs)), ActionNone, nil)
	}
	if sec := securityTitles(in.Filed); sec > 0 {
		add("sweep:security", ToneWarn, fmt.Sprintf("%d of %d filed issues are security findings. Those outrank new features.", sec, len(in.Filed)), ActionNone, nil)
	}

	order := map[string]int{ToneAct: 0, ToneWarn: 1, ToneNote: 2, ToneGood: 3}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Tone] < order[out[j].Tone] })
	if out == nil {
		out = []Opinion{}
	}
	return out
}

// oldestOpen is the oldest open PR in a swept repo.
func oldestOpen(in Input) (PRFacts, time.Duration, bool) {
	swept := map[string]bool{}
	for _, b := range in.Briefs {
		for _, r := range b.Repos {
			swept[r] = true
		}
	}
	var best PRFacts
	var bestAt time.Time
	for _, f := range in.Open {
		if !swept[f.Repo] {
			continue
		}
		at, err := time.Parse(time.RFC3339, f.CreatedAt)
		if err != nil {
			continue
		}
		if bestAt.IsZero() || at.Before(bestAt) {
			best, bestAt = f, at
		}
	}
	if bestAt.IsZero() {
		return PRFacts{}, 0, false
	}
	return best, in.Now.Sub(bestAt), true
}

func securityTitles(titles []string) int {
	n := 0
	for _, t := range titles {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "security") {
			n++
		}
	}
	return n
}

// ago is a duration as "just now", "<m> min", "<h>h <mm>m" or "<d>d <h>h".
func ago(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m < 1:
		return "just now"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	h := m / 60
	return fmt.Sprintf("%dd %dh", h/24, h%24)
}

// since is ago for mid-sentence use.
func since(d time.Duration) string {
	if a := ago(d); a != "just now" {
		return a + " ago"
	}
	return "just now"
}
