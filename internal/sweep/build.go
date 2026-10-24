package sweep

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
)

// Agent is one `herdr agent list` row, reduced to what the sweep uses. Names
// are the names the agent's session goes by (its worktree, directory
// basenames); a worker matches a brief directory through them.
type Agent struct {
	Pane   string
	Status string
	Names  []string
}

// Brief is one worker's brief directory, read.
type Brief struct {
	Name string
	// Repos are the owner/repo pairs the brief assigns.
	Repos []string
	Phase string
	// Marks holds the launched, plan, go and report times that apply.
	Marks map[string]time.Time
	// Latest is the newest mtime among the brief's files.
	Latest time.Time
}

// MergedPR is a PR merged since the sweep began.
type MergedPR struct {
	Repo     string // owner/repo
	Number   int
	Title    string
	ClosedAt time.Time
	Bot      bool
}

// Input is everything Build joins.
type Input struct {
	Now time.Time
	// Since is the start of the first day that counts.
	Since time.Time
	// Reviewers is the marker allowlist: a claim filter, not authenticity.
	Reviewers []string
	Briefs    []Brief
	Agents    []Agent
	// DeskPanes holds the signal_pane of each waiting desk item.
	DeskPanes map[string]bool
	Open      []PRFacts
	Merged    []MergedPR
	// Filed holds the titles of issues the sweep filed.
	Filed   []string
	Sources Sources
}

// waitMinutes is how long a plan or report may sit before it counts as
// waiting on the orchestrator.
const waitMinutes = 10

// maxTitle cuts a PR title.
const maxTitle = 160

// Build joins the sources into a ledger. It is pure: every input is a value.
func Build(in Input) Ledger {
	briefs := slices.Clone(in.Briefs)
	sort.Slice(briefs, func(i, j int) bool { return briefs[i].Name < briefs[j].Name })

	sweepRepos := map[string]bool{}
	repoWorkers := map[string][]string{}
	for _, b := range briefs {
		for _, r := range b.Repos {
			sweepRepos[r] = true
			repoWorkers[r] = append(repoWorkers[r], b.Name)
		}
	}

	prs := openPRs(in, sweepRepos, repoWorkers)
	byWorker := map[string][]Ref{}
	for _, p := range prs {
		for _, name := range repoWorkers[p.Repo] {
			byWorker[name] = append(byWorker[name], Ref{Repo: p.Repo, Number: p.Number})
		}
	}

	agentFor := matchAgents(briefs, in.Agents)
	workers := make([]Worker, 0, len(briefs))
	for _, b := range briefs {
		w := Worker{Name: b.Name, Status: "unknown", Phase: b.Phase, Repos: shortNames(b.Repos), PRs: byWorker[b.Name]}
		if w.PRs == nil {
			w.PRs = []Ref{}
		}
		if a, ok := agentFor[b.Name]; ok {
			w.Status = attention.Clean(a.Status, 16)
			if w.Status == "" {
				w.Status = "unknown"
			}
			if attention.ValidPane(a.Pane) {
				w.Pane = a.Pane
			}
			switch {
			case a.Pane != "" && in.DeskPanes[a.Pane]:
				w.BlockedBy = attention.BlockedByDesk
			case a.Status == "blocked":
				w.BlockedBy = attention.BlockedByPrompt
			}
		}
		workers = append(workers, w)
	}

	merged := mergedInScope(in, sweepRepos)
	l := Ledger{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   in.Now.UTC().Format(time.RFC3339),
		SweepRepos:    len(sweepRepos),
		OpenPRs:       len(prs),
		Filed:         len(in.Filed),
		Workers:       workers,
		PRs:           prs,
		Sources:       in.Sources,
		Merged:        len(merged),
	}
	// Keyed by owner/repo: two swept repos can share a name.
	perRepo := map[string]int{}
	for _, m := range merged {
		perRepo[m.Repo]++
		if m.Bot {
			l.DependencyBumps++
		}
	}
	l.ReposTouched = len(perRepo)
	l.TopRepos = topRepos(perRepo, 8)
	l.Recent = recent(merged, 8)
	l.Opinions = opinions(in, briefs, workers, prs, merged, perRepo, sweepRepos)
	return l
}

// Marshal is the ledger as sweep.json holds it: indented, with no null lists.
func Marshal(l Ledger) ([]byte, error) {
	// termsafe:allow-raw-json persisted ledger file, never command output; every string in it is cleaned or validated
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func openPRs(in Input, sweepRepos map[string]bool, repoWorkers map[string][]string) []PR {
	prs := make([]PR, 0, len(in.Open))
	for _, f := range in.Open {
		if !sweepRepos[f.Repo] {
			continue
		}
		state, reason := ClassifyPR(f, in.Reviewers)
		p := PR{Repo: f.Repo, Number: f.Number, Title: attention.Clean(f.Title, maxTitle), State: state, Reason: reason}
		if ws := repoWorkers[f.Repo]; len(ws) == 1 {
			p.Worker = ws[0]
		}
		prs = append(prs, p)
	}
	sort.Slice(prs, func(i, j int) bool {
		if prs[i].Repo != prs[j].Repo {
			return prs[i].Repo < prs[j].Repo
		}
		return prs[i].Number < prs[j].Number
	})
	return prs
}

// matchAgents maps each brief to the agent working it. An agent matches the
// brief whose name equals one of its names, or is that name up to a '-': a
// worker's worktree is <brief>-<issue>. Among briefs the longest match wins,
// so cos-bosun668-12 belongs to cos-bosun668 and not to cos-bosun.
func matchAgents(briefs []Brief, agents []Agent) map[string]Agent {
	out := map[string]Agent{}
	// A blocked agent is matched first, then a working one, so a second pane
	// on the same brief cannot hide the one that waits on the operator.
	ranked := slices.Clone(agents)
	rank := func(a Agent) int {
		switch a.Status {
		case "blocked":
			return 0
		case "working":
			return 1
		}
		return 2
	}
	sort.SliceStable(ranked, func(i, j int) bool { return rank(ranked[i]) < rank(ranked[j]) })
	for _, a := range ranked {
		best := ""
		for _, b := range briefs {
			for _, n := range a.Names {
				if (n == b.Name || strings.HasPrefix(n, b.Name+"-")) && len(b.Name) > len(best) {
					best = b.Name
				}
			}
		}
		if _, taken := out[best]; best != "" && !taken {
			out[best] = a
		}
	}
	return out
}

func mergedInScope(in Input, sweepRepos map[string]bool) []MergedPR {
	var out []MergedPR
	for _, m := range in.Merged {
		if sweepRepos[m.Repo] && !m.ClosedAt.Before(in.Since) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClosedAt.Before(out[j].ClosedAt) })
	return out
}

func topRepos(perRepo map[string]int, n int) []RepoCount {
	out := make([]RepoCount, 0, len(perRepo))
	full := make([]string, 0, len(perRepo))
	for r, c := range perRepo {
		out = append(out, RepoCount{Repo: r, Count: c})
		full = append(full, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Repo < out[j].Repo
	})
	names := displayNames(full)
	for i := range out {
		out[i].Repo = names[out[i].Repo]
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func recent(merged []MergedPR, n int) []Recent {
	out := []Recent{}
	for i := len(merged) - 1; i >= 0 && len(out) < n; i-- {
		m := merged[i]
		out = append(out, Recent{
			Repo: shortName(m.Repo), Number: m.Number,
			Title:    attention.Clean(m.Title, maxTitle),
			ClosedAt: m.ClosedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func shortName(repo string) string {
	if _, name, ok := strings.Cut(repo, "/"); ok {
		return name
	}
	return repo
}

// displayNames maps each owner/repo to its bare name, or keeps owner/repo
// when another repo in the set shares that name.
func displayNames(repos []string) map[string]string {
	seen := map[string]int{}
	for _, r := range repos {
		seen[shortName(r)]++
	}
	out := make(map[string]string, len(repos))
	for _, r := range repos {
		if seen[shortName(r)] > 1 {
			out[r] = r
		} else {
			out[r] = shortName(r)
		}
	}
	return out
}

func shortNames(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, shortName(r))
	}
	return out
}
