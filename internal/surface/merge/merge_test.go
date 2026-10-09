package merge

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Fixtures (testdata) are live, read-only captures from cameronsjo/forgectl
// on 2026-10-09, made with the exact DiscoverQuery, PRQuery and ChecksQuery
// in decode.go: #1204 (a merged drain worker PR on worker/gh1175-forgectl),
// #1203 (cadence-review markers at head d2a35ddc and earlier heads), #1199
// (a "Review rate limited" CodeRabbit commit status and no CodeRabbit
// review), #1195 (a completed CodeRabbit review with one unresolved thread).
// They are trimmed to keep them small: review and comment bodies other than
// CodeRabbit's review are cut to their first line, the compare response
// keeps only status and the file list, and each tree keeps only the changed
// files' entries. discover_synthetic_fork.json is the #1204 discovery with
// three PRs added on the same head name that the filter must drop.

const (
	head1204   = "3afe70e8bff88488d3aebd691ffb943829bf676c"
	base1204   = "a398e7258d0a755b14f2599afb277b7b8132b86e"
	head1203   = "d2a35ddc18851d85be5493211c3103b928ada66a"
	operatorID = 4084915
	forgectlID = 1252924951
)

func decodePRFixture(t *testing.T, name string) PRRead {
	t.Helper()
	r, err := DecodePR(readFixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

func goodSettings() config.MergeSettings {
	return config.MergeSettings{
		Mode: config.MergeManual, Machine: "f8e7a19c22c0", Approvers: []string{config.MergeApproverCadenceReview},
		MarkerAuthorID: operatorID, RequiredReviewers: []string{"cadence-forge-security-reviewer", "polish"},
		Method: config.MergeMethodSquash,
		Repos: []config.MergeRepo{{
			Name: "cameronsjo/forgectl", Workflow: ".github/workflows/ci.yml",
			RequiredChecks: []string{"build-test", "lint", "macos-test"},
			Paths:          []string{"internal/tasks/**", "docs/**"},
		}},
	}
}

func markerReview(reviewer, head string, crit, imp int, at string) Review {
	return Review{
		Author: Actor{Typename: "User", Login: "cameronsjo", DatabaseID: operatorID}, State: "COMMENTED", SubmittedAt: at, CommitOID: head,
		Body: "<!-- cadence-review: " + reviewer + " head=" + head + " crit=" + strconv.Itoa(crit) + " imp=" + strconv.Itoa(imp) + " -->\nreview text",
	}
}

// passingFacts is #1204 as it would look open and clean, with passing
// markers at its head and its file list moved under the allowlist: every
// predicate holds. Each test breaks one thing.
func passingFacts(t *testing.T) Facts {
	t.Helper()
	pr := decodePRFixture(t, "pr_1204.json")
	pr.PR.State, pr.PR.Mergeable, pr.PR.MergeStateStatus = "OPEN", "MERGEABLE", "CLEAN"
	checks, err := DecodeChecks(readFixture(t, "checks_1204.json"), head1204)
	if err != nil {
		t.Fatal(err)
	}
	reviews := append(slices.Clone(pr.Reviews),
		markerReview("cadence-forge-security-reviewer", head1204, 0, 0, "2026-10-09T17:00:00Z"),
		markerReview("polish", head1204, 0, 0, "2026-10-09T17:00:01Z"))
	files := []File{
		{Path: "internal/tasks/upgrade.go", Status: "modified", BaseMode: "100644", HeadMode: "100644"},
		{Path: "internal/tasks/upgrade_test.go", Status: "modified", BaseMode: "100644", HeadMode: "100644"},
		{Path: "docs/upgrade.md", Status: "added", HeadMode: "100644"},
		{Path: "docs/new.md", PreviousPath: "docs/old.md", Status: "renamed", BaseMode: "100644", HeadMode: "100644"},
	}
	pr.PR.ChangedFiles = len(files)
	return Facts{
		Row: Row{
			Name: "gh1175-forgectl", Branch: "worker/gh1175-forgectl", BranchFrom: "new", LaunchID: "launch-abc", Stage: "launched",
			Base: base1204, GitHubRepo: "cameronsjo/forgectl", GitHubRepoID: forgectlID, QueueLaunchID: "launch-abc", QueueState: "reported",
		},
		Repository: pr.Repository, OperatorID: operatorID, PR: pr.PR, Checks: checks.Runs, Reviews: reviews,
		Threads: pr.Threads, Comments: pr.Comments, Files: files,
		BaseAncestry: CompareIdentical, HeadAncestry: CompareAhead,
	}
}

func evalManual(f Facts) Verdict { return Evaluate(f, Policy{Settings: goodSettings()}) }

func wantRefusal(t *testing.T, v Verdict, want string) {
	t.Helper()
	if v.Result != Refuse {
		t.Fatalf("verdict %s %q; want refuse naming %q", v.Result, v.Reasons, want)
	}
	if !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, want) }) {
		t.Fatalf("reasons %q; want one naming %q", v.Reasons, want)
	}
}

func TestEvaluateBaselinePasses(t *testing.T) {
	v := evalManual(passingFacts(t))
	if v.Result != Pass || len(v.Reasons) != 0 {
		t.Fatalf("baseline: %s %q", v.Result, v.Reasons)
	}
	s := goodSettings()
	s.Mode = config.MergeAuto
	if v := Evaluate(passingFacts(t), Policy{Settings: s, ForDrain: true}); v.Result != Pass {
		t.Fatalf("auto for the drain: %s %q", v.Result, v.Reasons)
	}
}

// Predicate 1: mode.
func TestEvaluateMode(t *testing.T) {
	off := goodSettings()
	off.Mode, off.OffReason = config.MergeOff, "[surface.merge] machine is x and this machine is y"
	if v := Evaluate(passingFacts(t), Policy{Settings: off}); v.Result != Off || v.Reasons[0] != off.OffReason {
		t.Fatalf("off: %s %q", v.Result, v.Reasons)
	}
	if v := Evaluate(passingFacts(t), Policy{Settings: goodSettings(), ForDrain: true}); v.Result != Off || !strings.Contains(v.Reasons[0], `only with [surface.merge] mode "auto"`) {
		t.Fatalf("manual for the drain: %s %q", v.Result, v.Reasons)
	}
	odd := goodSettings()
	odd.Mode = "sometimes"
	if v := Evaluate(passingFacts(t), Policy{Settings: odd}); v.Result != Off {
		t.Fatalf("unknown mode: %s", v.Result)
	}
}

// Predicate 2: repository.
func TestEvaluateRepository(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Facts, *config.MergeSettings)
		want   string
	}{
		"no recorded repository":  {func(f *Facts, _ *config.MergeSettings) { f.Row.GitHubRepo, f.Row.GitHubRepoID = "", 0 }, "records no GitHub repository"},
		"id changed":              {func(f *Facts, _ *config.MergeSettings) { f.Repository.DatabaseID = 7 }, "id on GitHub is 7, expected 1252924951"},
		"renamed since launch":    {func(f *Facts, _ *config.MergeSettings) { f.Repository.NameWithOwner = "cameronsjo/forge" }, `is "cameronsjo/forge" on GitHub now`},
		"not on repos":            {func(_ *Facts, s *config.MergeSettings) { s.Repos[0].Name = "cameronsjo/other" }, "is not on [surface.merge] repos"},
		"built-in by name":        {func(f *Facts, s *config.MergeSettings) { builtinCadence(f, s, "CameronSjo/Cadence", forgectlID) }, "built-in refusal cameronsjo/cadence"},
		"built-in by recorded id": {func(f *Facts, s *config.MergeSettings) { builtinCadence(f, s, "cameronsjo/renamed", 1175723838) }, "built-in refusal cameronsjo/cadence"},
		"built-in by live name":   {func(f *Facts, _ *config.MergeSettings) { f.Repository.NameWithOwner = "cameronsjo/workbench" }, "built-in refusal cameronsjo/workbench"},
		"built-in by live id":     {func(f *Facts, _ *config.MergeSettings) { f.Repository.DatabaseID = 1106285560 }, "built-in refusal cameronsjo/dotfiles"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, s := passingFacts(t), goodSettings()
			c.mutate(&f, &s)
			wantRefusal(t, Evaluate(f, Policy{Settings: s}), c.want)
		})
	}
}

// builtinCadence points the row and settings at a repository the built-in
// list refuses, listing it on repos as a config would.
func builtinCadence(f *Facts, s *config.MergeSettings, name string, id int64) {
	f.Row.GitHubRepo, f.Row.GitHubRepoID = name, id
	f.Repository.NameWithOwner, f.Repository.DatabaseID = name, id
	s.Repos[0].Name = name
}

// Predicate 3: the worker's row.
func TestEvaluateRow(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Facts)
		want   string
	}{
		"no launch id":            {func(f *Facts) { f.Row.LaunchID = "" }, "has no launch_id"},
		"launch id not the claim": {func(f *Facts) { f.Row.QueueLaunchID = "launch-other" }, `expected "launch-other", the queue row's claim`},
		"no queue row":            {func(f *Facts) { f.Row.QueueLaunchID, f.Row.QueueState = "", "" }, `queue state is "no queue row"`},
		"branch not worker/name":  {func(f *Facts) { f.Row.Branch = "feat/x" }, `expected "worker/gh1175-forgectl"`},
		"branch from local":       {func(f *Facts) { f.Row.BranchFrom = "local" }, `came from "local", expected "new"`},
		"branch from origin":      {func(f *Facts) { f.Row.BranchFrom = "origin" }, `came from "origin"`},
		"branch from unrecorded":  {func(f *Facts) { f.Row.BranchFrom = "" }, "nothing recorded"},
		"ledger stage failed":     {func(f *Facts) { f.Row.Stage = "failed" }, `ledger stage is "failed"`},
		"queue state needs-you":   {func(f *Facts) { f.Row.QueueState = "needs-you" }, `queue state is "needs-you"`},
		"base not a commit":       {func(f *Facts) { f.Row.Base = "main" }, `recorded base is "main"`},
		"head branch differs":     {func(f *Facts) { f.PR.HeadRefName = "worker/other" }, `head branch is "worker/other"`},
		"base not an ancestor":    {func(f *Facts) { f.BaseAncestry = "behind" }, "is not an ancestor of the PR's base"},
		"base diverged":           {func(f *Facts) { f.BaseAncestry = "diverged" }, `compare status "diverged"`},
		"head not from base":      {func(f *Facts) { f.HeadAncestry = "diverged" }, "does not descend from the worker's base"},
		"ancestry unread":         {func(f *Facts) { f.BaseAncestry = "" }, `compare status ""`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := passingFacts(t)
			c.mutate(&f)
			wantRefusal(t, evalManual(f), c.want)
		})
	}
	// The live compare statuses decode to what the predicate reads.
	ahead, err := DecodeCompare(readFixture(t, "compare_ahead.json"))
	if err != nil || ahead.Status != CompareAhead {
		t.Fatalf("compare_ahead: %+v, %v", ahead, err)
	}
	behind, err := DecodeCompare(readFixture(t, "compare_behind.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := passingFacts(t)
	f.BaseAncestry = behind.Status
	wantRefusal(t, evalManual(f), `compare status "behind"`)
}

// Predicate 4: the PR.
func TestEvaluatePR(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Facts)
		want   string
	}{
		"merged (the real #1204)": {func(f *Facts) { f.PR.State = "MERGED" }, "the PR is MERGED, expected OPEN"},
		"closed":                  {func(f *Facts) { f.PR.State = "CLOSED" }, "the PR is CLOSED"},
		"draft":                   {func(f *Facts) { f.PR.IsDraft = true }, "the PR is a draft"},
		"not the default branch":  {func(f *Facts) { f.PR.BaseRefName = "release" }, `targets "release", expected the default branch "main"`},
		"mergeable unknown":       {func(f *Facts) { f.PR.Mergeable = "UNKNOWN" }, "mergeable state is UNKNOWN"},
		"conflicting":             {func(f *Facts) { f.PR.Mergeable = "CONFLICTING" }, "CONFLICTING, expected MERGEABLE"},
		"merge state blocked":     {func(f *Facts) { f.PR.MergeStateStatus = "BLOCKED" }, "merge state is BLOCKED, expected CLEAN or HAS_HOOKS"},
		"merge state behind":      {func(f *Facts) { f.PR.MergeStateStatus = "BEHIND" }, "BEHIND"},
		"merge state unstable":    {func(f *Facts) { f.PR.MergeStateStatus = "UNSTABLE" }, "UNSTABLE"},
		"cross repository":        {func(f *Facts) { f.PR.IsCrossRepository = true }, "cross-repository true"},
		"head repo differs":       {func(f *Facts) { f.PR.HeadRepoID = 999 }, "head repository is 999"},
		"another author":          {func(f *Facts) { f.PR.Author.DatabaseID = 556 }, "expected the operator's id 4084915"},
		"no operator id":          {func(f *Facts) { f.OperatorID = 0 }, "expected the operator's id 0"},
		"head not a commit":       {func(f *Facts) { f.PR.HeadRefOid = "x" }, `the PR's head is "x"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := passingFacts(t)
			c.mutate(&f)
			wantRefusal(t, evalManual(f), c.want)
		})
	}
	f := passingFacts(t)
	f.PR.MergeStateStatus = "HAS_HOOKS"
	if v := evalManual(f); v.Result != Pass {
		t.Fatalf("HAS_HOOKS: %s %q", v.Result, v.Reasons)
	}
}

func runNamed(f Facts, name string) CheckRun {
	for _, r := range f.Checks {
		if r.Name == name {
			return r
		}
	}
	return CheckRun{}
}

// Predicate 5: required checks.
func TestEvaluateChecks(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Facts)
		want   string
	}{
		"required check missing": {func(f *Facts) {
			f.Checks = slices.DeleteFunc(f.Checks, func(r CheckRun) bool { return r.Name == "lint" })
		}, `check "lint" has no run at the head`},
		"a later failed run": {func(f *Facts) {
			r := runNamed(*f, "lint")
			r.DatabaseID, r.StartedAt, r.Conclusion = r.DatabaseID+1, "2099-01-01T00:00:00Z", "FAILURE"
			f.Checks = append(f.Checks, r)
		}, `check "lint" has a run (id`},
		// I3 (T10.2 security review): any tied run that is not SUCCESS
		// refuses, so a later success does not hide an earlier failure.
		"an earlier failed run beside a later success": {func(f *Facts) {
			r := runNamed(*f, "lint")
			r.DatabaseID, r.StartedAt, r.Conclusion = r.DatabaseID-1, "2000-01-01T00:00:00Z", "FAILURE"
			f.Checks = append(f.Checks, r)
		}, "concluded FAILURE, expected every run at the head to be SUCCESS"},
		"a run in progress": {func(f *Facts) {
			r := runNamed(*f, "build-test")
			r.DatabaseID, r.StartedAt, r.Status, r.Conclusion = r.DatabaseID+1, "2099-01-01T00:00:00Z", "IN_PROGRESS", ""
			f.Checks = append(f.Checks, r)
		}, `check "build-test" is still running: run id`},
		"a queued run": {func(f *Facts) {
			r := runNamed(*f, "build-test")
			r.DatabaseID, r.StartedAt, r.Status, r.Conclusion = r.DatabaseID+1, "", "QUEUED", ""
			f.Checks = append(f.Checks, r)
		}, "is QUEUED, expected COMPLETED/SUCCESS"},
		"the only success is tied to another PR": {func(f *Facts) {
			for i := range f.Checks {
				if f.Checks[i].Name == "lint" {
					f.Checks[i].SuitePRs = []int{9999}
				}
			}
		}, `check "lint" has no run at the head tied to PR #1204`},
		"the only success is on another branch": {func(f *Facts) {
			for i := range f.Checks {
				if f.Checks[i].Name == "lint" {
					f.Checks[i].SuiteBranch = "copy-of-worker"
				}
			}
		}, `check "lint" has no run at the head tied to PR #1204`},
		"the only success has no matching PR": {func(f *Facts) {
			for i := range f.Checks {
				if f.Checks[i].Name == "lint" {
					f.Checks[i].SuitePRs = nil
				}
			}
		}, `check "lint" has no run at the head tied to PR #1204`},
		"a same-name run with no workflow run": {func(f *Facts) {
			f.Checks = append(f.Checks, CheckRun{DatabaseID: 1, Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS", AppID: 999,
				SuiteBranch: f.PR.HeadRefName, SuitePRs: []int{f.PR.Number}})
		}, "no workflow run behind it"},
		"a same-name run from another app": {func(f *Facts) {
			r := runNamed(*f, "lint")
			r.AppID = 999
			f.Checks = append(f.Checks, r)
		}, "from app 999, expected GitHub Actions"},
		"a same-name run from another workflow": {func(f *Facts) {
			r := runNamed(*f, "lint")
			r.WorkflowPath = "/cameronsjo/forgectl/actions/workflows/vulncheck.yml"
			f.Checks = append(f.Checks, r)
		}, "from workflow /cameronsjo/forgectl/actions/workflows/vulncheck.yml, expected only /cameronsjo/forgectl/actions/workflows/ci.yml"},
		"only a push-event run": {func(f *Facts) {
			for i := range f.Checks {
				if f.Checks[i].Name == "macos-test" {
					f.Checks[i].Event = "push"
				}
			}
		}, `check "macos-test" has no run at the head tied to PR #1204 from .github/workflows/ci.yml on a pull_request event`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := passingFacts(t)
			c.mutate(&f)
			wantRefusal(t, evalManual(f), c.want)
		})
	}
	// I3: a run at the same commit for another PR (another base, a copy
	// of the branch) is ignored, so its failure does not refuse and its
	// success does not count.
	t.Run("a failed run tied to another PR is ignored", func(t *testing.T) {
		f := passingFacts(t)
		r := runNamed(f, "lint")
		r.DatabaseID, r.StartedAt, r.Conclusion, r.SuitePRs = r.DatabaseID+7, "2099-01-01T00:00:00Z", "FAILURE", []int{9999}
		f.Checks = append(f.Checks, r)
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("two successful runs pass", func(t *testing.T) {
		f := passingFacts(t)
		r := runNamed(f, "lint")
		r.DatabaseID = r.DatabaseID + 9
		f.Checks = append(f.Checks, r)
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("the live #1207 capture ties its ci.yml runs to #1207", func(t *testing.T) {
		const head1207 = "79c4c7f3905c8d92136b7933bed94eaa0788118f"
		c, err := DecodeChecks(readFixture(t, "checks_1207.json"), head1207)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range c.Runs {
			if r.WorkflowPath == "/cameronsjo/forgectl/actions/workflows/ci.yml" {
				n++
				if r.SuiteBranch != "plan/atelier-p4" || !slices.Equal(r.SuitePRs, []int{1207}) {
					t.Fatalf("run %+v; want branch plan/atelier-p4 and matching PR 1207", r)
				}
			}
		}
		if n < 3 {
			t.Fatalf("%d ci.yml runs in the capture, expected build-test, lint and macos-test at least", n)
		}
	})
	t.Run("a push-event run beside the pull_request run is ignored", func(t *testing.T) {
		f := passingFacts(t)
		r := runNamed(f, "lint")
		r.DatabaseID, r.StartedAt, r.Event, r.Conclusion = r.DatabaseID+5, "2099-01-01T00:00:00Z", "push", "FAILURE"
		f.Checks = append(f.Checks, r)
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("a commit status never stands in for a check (#1199)", func(t *testing.T) {
		c, err := DecodeChecks(readFixture(t, "checks_1199.json"), "d209b1290318f72ff8485d63324f10a11b7275b1")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(c.Statuses, func(s string) bool { return strings.Contains(s, "Review rate limited") }) {
			t.Fatalf("statuses %q; the fixture should carry the rate-limited CodeRabbit status", c.Statuses)
		}
		f := passingFacts(t)
		f.Checks = nil
		wantRefusal(t, evalManual(f), `check "build-test" has no run`)
	})
}

// Predicate 6: changed paths.
func TestEvaluatePaths(t *testing.T) {
	file := func(p, status string) File {
		f := File{Path: p, Status: status, BaseMode: "100644", HeadMode: "100644"}
		switch status {
		case "added":
			f.BaseMode = ""
		case "removed":
			f.HeadMode = ""
		}
		return f
	}
	cases := map[string]struct {
		files []File
		want  string
	}{
		"outside the allowlist":        {[]File{file("internal/tasks2/x.go", "modified")}, `"internal/tasks2/x.go" matches none of [surface.merge.paths]`},
		"allowlist is case-sensitive":  {[]File{file("Docs/x.md", "modified")}, "matches none"},
		"built-in: .github":            {[]File{file(".github/workflows/ci.yml", "modified")}, "built-in refused set (.github/**)"},
		"built-in: .github any case":   {[]File{file(".GitHub/workflows/ci.yml", "modified")}, "built-in refused set (.github/**)"},
		"built-in: .claude":            {[]File{file(".claude/settings.json", "added")}, "(.claude/**)"},
		"built-in: coderabbit config":  {[]File{file(".coderabbit.yaml", "modified")}, "top-level files"},
		"built-in: the gate":           {[]File{file("internal/surface/merge/merge.go", "modified")}, "(internal/surface/**)"},
		"built-in: surface cli":        {[]File{file("internal/cli/surface_status.go", "modified")}, "(internal/cli/**)"},
		"built-in: config":             {[]File{file("internal/config/surface_merge.go", "modified")}, "(internal/config/**)"},
		"built-in: launch":             {[]File{file("internal/launch/x.go", "modified")}, "(internal/launch/**)"},
		"built-in: gitenv":             {[]File{file("internal/gitenv/x.go", "modified")}, "(internal/gitenv/**)"},
		"built-in: exec":               {[]File{file("internal/exec/x.go", "modified")}, "(internal/exec/**)"},
		"built-in: githubauth":         {[]File{file("internal/githubauth/runner.go", "modified")}, "(internal/githubauth/**)"},
		"built-in: bless":              {[]File{file("internal/bless/bless.go", "modified")}, "(internal/bless/**)"},
		"built-in: selfupdate":         {[]File{file("internal/selfupdate/selfupdate.go", "modified")}, "(internal/selfupdate/**)"},
		"built-in: go.mod at the root": {[]File{file("go.mod", "modified")}, "top-level files"},
		"built-in: go.sum nested":      {[]File{file("docs/x/go.sum", "added")}, "module files"},
		"built-in: go.work":            {[]File{file("docs/go.work", "added")}, "module files"},
		"built-in: Cargo.toml nested":  {[]File{file("docs/x/Cargo.toml", "added")}, "build and toolchain files"},
		"built-in: .cargo nested":      {[]File{file("docs/.cargo/config.toml", "added")}, "a .cargo directory"},
		"renamed out of the gate":      {[]File{{Path: "docs/x.go", PreviousPath: "internal/surface/x.go", Status: "renamed", BaseMode: "100644", HeadMode: "100644"}}, `"internal/surface/x.go" is refused`},
		"renamed out of the allowlist": {[]File{{Path: "docs/x.md", PreviousPath: "internal/tasks2/README.md", Status: "renamed", BaseMode: "100644", HeadMode: "100644"}}, `"internal/tasks2/README.md" matches none`},
		"removed test file":            {[]File{file("internal/tasks/x_test.go", "removed")}, "removes a test file"},
		"test renamed away":            {[]File{{Path: "internal/tasks/x.go", PreviousPath: "internal/tasks/x_test.go", Status: "renamed", BaseMode: "100644", HeadMode: "100644"}}, "removes a test file"},
		"status copied":                {[]File{file("docs/a.md", "copied")}, `status "copied"`},
		"status changed":               {[]File{file("docs/a.md", "changed")}, `status "changed"`},
		"executable":                   {[]File{{Path: "docs/a.sh", Status: "added", HeadMode: "100755"}}, "added with mode 100755"},
		"symlink":                      {[]File{{Path: "docs/link", Status: "added", HeadMode: "120000"}}, "mode 120000"},
		"submodule":                    {[]File{{Path: "docs/sub", Status: "modified", BaseMode: "160000", HeadMode: "160000"}}, "160000"},
		"mode change":                  {[]File{{Path: "docs/a.md", Status: "modified", BaseMode: "100644", HeadMode: "100755"}}, "100644 at base and 100755 at head"},
		"mode unread":                  {[]File{{Path: "docs/a.md", Status: "modified", BaseMode: "100644"}}, "none at head"},
		"removed with odd base mode":   {[]File{{Path: "docs/a.sh", Status: "removed", BaseMode: "100755"}}, "removed with base mode 100755"},
		"leading slash":                {[]File{file("/docs/a.md", "modified")}, "start with '/'"},
		"dot-dot segment":              {[]File{file("docs/../x.md", "modified")}, `".." segment`},
		"dot segment":                  {[]File{file("docs/./x.md", "modified")}, `"." segment`},
		"empty segment":                {[]File{file("docs//x.md", "modified")}, "empty segment"},
		"backslash":                    {[]File{file(`docs\x.md`, "modified")}, "backslash"},
		"control byte":                 {[]File{file("docs/\x1bx.md", "modified")}, "control byte 0x1b"},
		"no files":                     {[]File{}, "changes no files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := passingFacts(t)
			f.Files, f.PR.ChangedFiles = c.files, len(c.files)
			wantRefusal(t, evalManual(f), c.want)
		})
	}
	// I1 (T10.2 security review): U+017F long s folds onto 's' on a
	// case-folding checkout, so internal/ſurface/x.go would land on
	// internal/surface/x.go while matching no built-in refusal. A config
	// glob wide enough to reach it (internal/**) must still refuse it.
	t.Run("a non-ASCII path is refused, under both names of a rename", func(t *testing.T) {
		s := goodSettings()
		s.Repos[0].Paths = []string{"internal/**", "docs/**"}
		for name, files := range map[string][]File{
			"modified":      {file("internal/ſurface/x.go", "modified")},
			"renamed to":    {{Path: "internal/ſurface/x.go", PreviousPath: "docs/x.go", Status: "renamed", BaseMode: "100644", HeadMode: "100644"}},
			"renamed from":  {{Path: "docs/x.go", PreviousPath: "internal/ſurface/x.go", Status: "renamed", BaseMode: "100644", HeadMode: "100644"}},
			"invalid UTF-8": {file("internal/\xffsurface/x.go", "modified")},
		} {
			t.Run(name, func(t *testing.T) {
				f := passingFacts(t)
				f.Files, f.PR.ChangedFiles = files, len(files)
				v := Evaluate(f, Policy{Settings: s})
				if v.Result != Refuse || !slices.ContainsFunc(v.Reasons, func(r string) bool {
					return strings.Contains(r, "only ASCII") || strings.Contains(r, "valid UTF-8")
				}) {
					t.Fatalf("%s %q; want a non-ASCII refusal", v.Result, v.Reasons)
				}
			})
		}
		// Control: the same glob passes the ASCII path it is meant for.
		f := passingFacts(t)
		f.Files, f.PR.ChangedFiles = []File{file("internal/tasks/x.go", "modified")}, 1
		if v := Evaluate(f, Policy{Settings: s}); v.Result != Pass {
			t.Fatalf("control: %s %q", v.Result, v.Reasons)
		}
	})
	t.Run("file count differs from changedFiles", func(t *testing.T) {
		f := passingFacts(t)
		f.PR.ChangedFiles = 5
		wantRefusal(t, evalManual(f), "has 4 entries, expected the PR's changedFiles 5 (GitHub's compare lists at most 300 files")
	})
	t.Run("the real #1204 file list touches the built-in set", func(t *testing.T) {
		cmp, err := DecodeCompare(readFixture(t, "compare_1204.json"))
		if err != nil || len(cmp.Files) != 4 || cmp.Status != CompareAhead {
			t.Fatalf("compare_1204: %+v, %v", cmp, err)
		}
		modes := map[string]map[string]string{}
		for _, side := range []string{"a398e725", "3afe70e8"} {
			for _, dir := range []string{"internal_cli", "internal_selfupdate"} {
				m, err := DecodeTree(readFixture(t, "tree_"+side+"_"+dir+".json"))
				if err != nil {
					t.Fatal(err)
				}
				modes[side+"/"+dir] = m
			}
		}
		f := passingFacts(t)
		f.Files = nil
		for _, file := range cmp.Files {
			dir := strings.ReplaceAll(file.Path[:strings.LastIndex(file.Path, "/")], "/", "_")
			base := file.Path[strings.LastIndex(file.Path, "/")+1:]
			file.BaseMode, file.HeadMode = modes["a398e725/"+dir][base], modes["3afe70e8/"+dir][base]
			if file.BaseMode != "100644" || file.HeadMode != "100644" {
				t.Fatalf("%s modes %q %q from the tree fixtures", file.Path, file.BaseMode, file.HeadMode)
			}
			f.Files = append(f.Files, file)
		}
		f.PR.ChangedFiles = len(f.Files)
		v := evalManual(f)
		wantRefusal(t, v, `"internal/selfupdate/selfupdate.go" is refused whatever the config says`)
		wantRefusal(t, v, `"internal/cli/upgrade.go" is refused whatever the config says`)
	})
}

// Predicate 7: the cadence-review approver.
func TestEvaluateCadenceReview(t *testing.T) {
	withReviews := func(t *testing.T, rs ...Review) Facts {
		f := passingFacts(t)
		f.Reviews = rs
		return f
	}
	sec := func(head string, crit, imp int, at string) Review {
		return markerReview("cadence-forge-security-reviewer", head, crit, imp, at)
	}
	pol := func(head string, crit, imp int, at string) Review { return markerReview("polish", head, crit, imp, at) }
	const old = "8a4e4e3c972fd477f2a04d280aa1d9186415e354"
	cases := map[string]struct {
		reviews []Review
		want    string
	}{
		"no markers": {nil, "no cadence-forge-security-reviewer marker by user 4084915"},
		"one required reviewer missing": {[]Review{sec(head1204, 0, 0, "2026-10-09T17:00:00Z")},
			"no polish marker"},
		"latest marker at an old head": {[]Review{sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(old, 0, 0, "2026-10-09T18:00:00Z")},
			"polish's latest marker is head=8a4e4e3c972f"},
		"latest marker crit": {[]Review{sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(head1204, 1, 0, "2026-10-09T17:00:00Z")},
			"crit=1 imp=0, expected"},
		"latest marker imp": {[]Review{sec(head1204, 0, 2, "2026-10-09T17:00:00Z"), pol(head1204, 0, 0, "2026-10-09T17:00:00Z")},
			"crit=0 imp=2"},
		"crit later cleared only at an old head": {[]Review{sec(head1204, 0, 0, "2026-10-09T19:00:00Z"), pol(head1204, 0, 0, "2026-10-09T19:00:00Z"),
			markerReview("chief-of-staff", old, 1, 0, "2026-10-09T17:00:00Z"), markerReview("chief-of-staff", old, 0, 0, "2026-10-09T18:00:00Z")},
			"chief-of-staff reported crit=1 imp=0"},
		"crit from a non-required reviewer with nothing later": {[]Review{sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(head1204, 0, 0, "2026-10-09T17:00:00Z"),
			markerReview("chief-of-staff", head1204, 0, 3, "2026-10-09T17:00:05Z")},
			"chief-of-staff reported crit=0 imp=3"},
		"crit and pass in the same second": {[]Review{sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(head1204, 0, 0, "2026-10-09T17:00:00Z"),
			markerReview("code", head1204, 1, 0, "2026-10-09T17:00:05Z"), markerReview("code", head1204, 0, 0, "2026-10-09T17:00:05Z")},
			"code reported crit=1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			wantRefusal(t, evalManual(withReviews(t, c.reviews...)), c.want)
		})
	}
	t.Run("an earlier crit cleared by a later passing marker at the head passes", func(t *testing.T) {
		f := withReviews(t, sec(old, 2, 1, "2026-10-09T15:00:00Z"), sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), pol(head1204, 0, 0, "2026-10-09T17:00:00Z"))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	// Each of these marker reviews is not counted, so polish has no marker.
	notCounted := map[string]func(Review) Review{
		"another author id": func(r Review) Review { r.Author.DatabaseID = 556; return r },
		"a bot author":      func(r Review) Review { r.Author.Typename = "Bot"; return r },
		"pending":           func(r Review) Review { r.State = "PENDING"; return r },
		"changes requested": func(r Review) Review { r.State = "CHANGES_REQUESTED"; return r },
		"dismissed":         func(r Review) Review { r.State = "DISMISSED"; return r },
		"no submittedAt":    func(r Review) Review { r.SubmittedAt = ""; return r },
		"byte-order mark":   func(r Review) Review { r.Body = "\uFEFF" + r.Body; return r },
		"leading space":     func(r Review) Review { r.Body = " " + r.Body; return r },
		"uppercase hex": func(r Review) Review {
			r.Body = strings.Replace(r.Body, head1204, strings.ToUpper(head1204), 1)
			return r
		},
		"crit=01": func(r Review) Review { r.Body = strings.Replace(r.Body, "crit=0 ", "crit=01 ", 1); return r },
		"not the first line": func(r Review) Review {
			r.Body = "Review\n" + r.Body
			return r
		},
		"no html comment": func(r Review) Review {
			r.Body = "cadence-review: polish head=" + head1204 + " crit=0 imp=0"
			return r
		},
		"trailing text": func(r Review) Review {
			r.Body = strings.Replace(r.Body, " -->", " --> ok", 1)
			return r
		},
	}
	for name, mutate := range notCounted {
		t.Run("not counted: "+name, func(t *testing.T) {
			f := withReviews(t, sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), mutate(pol(head1204, 0, 0, "2026-10-09T17:00:00Z")))
			v := evalManual(f)
			if v.Result != Refuse || !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "no polish marker") }) {
				t.Fatalf("%s %q; want polish's marker not counted", v.Result, v.Reasons)
			}
		})
	}
	t.Run("marker head differs from the review's commit", func(t *testing.T) {
		r := pol(head1204, 0, 0, "2026-10-09T17:00:00Z")
		r.CommitOID = old
		wantRefusal(t, evalManual(withReviews(t, sec(head1204, 0, 0, "2026-10-09T17:00:00Z"), r)), "(review commit 8a4e4e3c972f)")
	})
	t.Run("the real #1203 markers", func(t *testing.T) {
		pr := decodePRFixture(t, "pr_1203.json")
		if pr.PR.HeadRefOid != head1203 {
			t.Fatalf("head %s", pr.PR.HeadRefOid)
		}
		f := passingFacts(t)
		f.PR.HeadRefOid, f.Reviews = head1203, pr.Reviews
		v := evalManual(f)
		// The security reviewer's latest marker is at the head; polish's
		// latest is at 8a4e4e3c, an earlier head, so the approver refuses.
		wantRefusal(t, v, "polish's latest marker is head=8a4e4e3c972f")
		if slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "cadence-forge-security-reviewer") }) {
			t.Fatalf("the security reviewer's marker at the head was not counted: %q", v.Reasons)
		}
		s := goodSettings()
		s.RequiredReviewers = []string{"cadence-forge-security-reviewer", "chief-of-staff"}
		if v := Evaluate(f, Policy{Settings: s}); slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "cadence-review") }) {
			t.Fatalf("security and chief-of-staff at the head: %q", v.Reasons)
		}
	})
}

func TestParseMarker(t *testing.T) {
	good := "<!-- cadence-review: polish head=" + head1204 + " crit=0 imp=12 -->"
	m, ok := ParseMarker(good + "\nbody")
	if !ok || m != (Marker{Reviewer: "polish", Head: head1204, Crit: 0, Imp: 12}) {
		t.Fatalf("%+v %v", m, ok)
	}
	for name, line := range map[string]string{
		"BOM":             "\uFEFF" + good,
		"leading space":   " " + good,
		"uppercase hex":   strings.Replace(good, head1204, strings.ToUpper(head1204), 1),
		"crit=01":         strings.Replace(good, "crit=0", "crit=01", 1),
		"imp=10000":       strings.Replace(good, "imp=12", "imp=10000", 1),
		"short head":      strings.Replace(good, head1204, head1204[:39], 1),
		"reviewer caps":   strings.Replace(good, "polish", "Polish", 1),
		"reviewer long":   strings.Replace(good, "polish", strings.Repeat("a", 41), 1),
		"carriage return": good + "\r",
		"no close":        strings.TrimSuffix(good, " -->"),
	} {
		if _, ok := ParseMarker(line); ok {
			t.Errorf("%s parsed: %q", name, line)
		}
	}
}

// Predicate 7: the coderabbit approver.
func TestEvaluateCodeRabbit(t *testing.T) {
	s := goodSettings()
	s.Approvers = []string{config.MergeApproverCodeRabbit}
	pr1195 := decodePRFixture(t, "pr_1195.json")
	const head1195 = "1f01561d92018670e354cb381780908fd3286f3d"
	at1195 := func(t *testing.T) Facts {
		f := passingFacts(t)
		// Clones: the subtests below edit these in place.
		f.PR.HeadRefOid, f.Reviews, f.Threads, f.Comments = head1195, slices.Clone(pr1195.Reviews), slices.Clone(pr1195.Threads), slices.Clone(pr1195.Comments)
		return f
	}
	t.Run("the real #1195: a completed review, one unresolved thread", func(t *testing.T) {
		wantRefusal(t, Evaluate(at1195(t), Policy{Settings: s}), "a CodeRabbit review thread is unresolved")
	})
	resolve := func(f *Facts, by *Actor) {
		for i := range f.Threads {
			f.Threads[i].IsResolved, f.Threads[i].ResolvedBy = true, by
		}
	}
	bot := &Actor{Typename: "User", Login: "coderabbitai", DatabaseID: CodeRabbitUserID}
	t.Run("the thread resolved by the bot passes", func(t *testing.T) {
		f := at1195(t)
		resolve(&f, bot)
		if v := Evaluate(f, Policy{Settings: s}); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	cases := map[string]struct {
		mutate func(*Facts)
		want   string
	}{
		"thread resolved by the operator": {func(f *Facts) { resolve(f, &Actor{Typename: "User", Login: "cameronsjo", DatabaseID: operatorID}) },
			`resolved by user "cameronsjo"`},
		"thread resolved by no one readable": {func(f *Facts) { resolve(f, nil) }, "resolved by an unreadable account"},
		"a non-bot @coderabbitai mention": {func(f *Facts) {
			resolve(f, bot)
			f.Comments = append(f.Comments, Comment{Author: Actor{Typename: "User", Login: "cameronsjo", DatabaseID: operatorID}, Body: "@CodeRabbitAI resolve"})
		}, "mentioned @coderabbitai"},
		"a mention in a review body": {func(f *Facts) {
			resolve(f, bot)
			f.Reviews = append(f.Reviews, Review{Author: Actor{Typename: "User", DatabaseID: operatorID, Login: "cameronsjo"}, Body: "ping @coderabbitai", State: "COMMENTED"})
		}, "mentioned @coderabbitai"},
		"a mention in the PR body": {func(f *Facts) {
			resolve(f, bot)
			f.PR.Body = "Summary\n\n@coderabbitai summary"
		}, "mentioned @coderabbitai"},
		"the review is at an older head": {func(f *Facts) {
			resolve(f, bot)
			f.PR.HeadRefOid = head1204
		}, "no completed review by user 136622811 at head 3afe70e8bff8"},
		"the review is by another id": {func(f *Facts) {
			resolve(f, bot)
			for i := range f.Reviews {
				f.Reviews[i].Author.DatabaseID = 1
			}
		}, "no completed review by user 136622811"},
		"the review body is not a completed review": {func(f *Facts) {
			resolve(f, bot)
			for i := range f.Reviews {
				if isCodeRabbit(f.Reviews[i].Author) {
					f.Reviews[i].Body = "Review skipped"
				}
			}
		}, "no completed review"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := at1195(t)
			c.mutate(&f)
			wantRefusal(t, Evaluate(f, Policy{Settings: s}), c.want)
		})
	}
	// C1 (T10.2 security review): an open cadence-review finding refuses
	// under every approver set, so a passing CodeRabbit review never
	// silences it.
	t.Run("a passing CodeRabbit review does not silence an open finding", func(t *testing.T) {
		both := goodSettings()
		both.Approvers = []string{config.MergeApproverCadenceReview, config.MergeApproverCodeRabbit}
		for name, settings := range map[string]config.MergeSettings{"coderabbit only": s, "cadence-review and coderabbit": both} {
			t.Run(name, func(t *testing.T) {
				f := at1195(t)
				resolve(&f, bot)
				if v := Evaluate(f, Policy{Settings: settings}); v.Result != Pass {
					t.Fatalf("control: CodeRabbit passing alone: %s %q", v.Result, v.Reasons)
				}
				f.Reviews = append(f.Reviews, markerReview("cadence-forge-security-reviewer", head1195, 2, 0, "2026-10-09T23:00:00Z"))
				wantRefusal(t, Evaluate(f, Policy{Settings: settings}), "open finding: cadence-forge-security-reviewer reported crit=2 imp=0")
			})
		}
	})
	t.Run("an open finding at an older head, cleared later at the head, passes", func(t *testing.T) {
		f := at1195(t)
		resolve(&f, bot)
		f.Reviews = append(f.Reviews,
			markerReview("cadence-forge-security-reviewer", head1204, 1, 1, "2026-10-09T22:00:00Z"),
			markerReview("cadence-forge-security-reviewer", head1195, 0, 0, "2026-10-09T23:00:00Z"))
		if v := Evaluate(f, Policy{Settings: s}); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("no marker_author_id refuses under coderabbit too", func(t *testing.T) {
		f := at1195(t)
		resolve(&f, bot)
		noID := s
		noID.MarkerAuthorID = 0
		wantRefusal(t, Evaluate(f, Policy{Settings: noID}), "marker_author_id is not set")
	})
	t.Run("the real #1199: rate limited, no review", func(t *testing.T) {
		pr := decodePRFixture(t, "pr_1199.json")
		f := passingFacts(t)
		f.PR.HeadRefOid, f.Reviews, f.Threads, f.Comments = pr.PR.HeadRefOid, pr.Reviews, pr.Threads, pr.Comments
		wantRefusal(t, Evaluate(f, Policy{Settings: s}), "a commit status such as \"Review rate limited\" never counts")
	})
	t.Run("either approver passing is enough", func(t *testing.T) {
		both := goodSettings()
		both.Approvers = []string{config.MergeApproverCadenceReview, config.MergeApproverCodeRabbit}
		if v := Evaluate(passingFacts(t), Policy{Settings: both}); v.Result != Pass {
			t.Fatalf("cadence-review passing, coderabbit not: %s %q", v.Result, v.Reasons)
		}
		f := at1195(t)
		resolve(&f, bot)
		f.Reviews = slices.DeleteFunc(f.Reviews, func(r Review) bool { return !isCodeRabbit(r.Author) })
		if v := Evaluate(f, Policy{Settings: both}); v.Result != Pass {
			t.Fatalf("coderabbit passing, cadence-review not: %s %q", v.Result, v.Reasons)
		}
		// #1195's own cadence-review markers pass at its head; drop them so
		// neither approver passes.
		f = at1195(t)
		f.Reviews = slices.DeleteFunc(f.Reviews, func(r Review) bool { return !isCodeRabbit(r.Author) })
		v := Evaluate(f, Policy{Settings: both})
		wantRefusal(t, v, "no approver passed the head")
		wantRefusal(t, v, "cadence-review: no cadence-forge-security-reviewer marker")
		wantRefusal(t, v, "coderabbit: a CodeRabbit review thread is unresolved")
	})
}

// TestDecodePRBody pins that the PR body is read, so the @coderabbitai
// mention rule can see it. The captured fixtures predate the field.
func TestDecodePRBody(t *testing.T) {
	pr := string(readFixture(t, "pr_1204.json"))
	withBody := strings.Replace(pr, `"title": `, `"body": "ping @coderabbitai", "title": `, 1)
	if withBody == pr {
		t.Fatal("the fixture edit did not apply")
	}
	r, err := DecodePR([]byte(withBody))
	if err != nil || r.PR.Body != "ping @coderabbitai" {
		t.Fatalf("body %q, %v", r.PR.Body, err)
	}
}

func TestCompletedCodeRabbitReview(t *testing.T) {
	pr := decodePRFixture(t, "pr_1195.json")
	const head = "1f01561d92018670e354cb381780908fd3286f3d"
	var body string
	for _, r := range pr.Reviews {
		if isCodeRabbit(r.Author) {
			body = r.Body
		}
	}
	if !CompletedCodeRabbitReview(body, head) {
		t.Fatal("the captured #1195 review is not read as completed")
	}
	if CompletedCodeRabbitReview(body, head1204) {
		t.Fatal("a review of another head counted")
	}
	if CompletedCodeRabbitReview(strings.Replace(body, codeRabbitStatusLine, "", 1), head) {
		t.Fatal("a body without the review-status marker counted")
	}
	if CompletedCodeRabbitReview(" "+body, head) {
		t.Fatal("a body with a different first line counted")
	}
}

func TestSelectPR(t *testing.T) {
	d, err := DecodeDiscovery(readFixture(t, "discover_1204.json"))
	if err != nil || d.ViewerID != operatorID || d.Repository.DatabaseID != forgectlID || d.Repository.DefaultBranch != "main" {
		t.Fatalf("discover_1204: %+v, %v", d, err)
	}
	c, err := SelectPR(d, "worker/gh1175-forgectl")
	if err != nil || c.Number != 1204 || c.State != "MERGED" {
		t.Fatalf("select: %+v, %v", c, err)
	}
	if _, err := SelectPR(d, "worker/other"); !errors.Is(err, ErrNoPR) {
		t.Fatalf("other branch: %v", err)
	}
	syn, err := DecodeDiscovery(readFixture(t, "discover_synthetic_fork.json"))
	if err != nil || len(syn.Candidates) != 4 {
		t.Fatalf("synthetic: %+v, %v", syn, err)
	}
	// Three open PRs on the same head name (a fork, another author, a bot)
	// are dropped before ordering, so the operator's merged #1204 is the one.
	if c, err := SelectPR(syn, "worker/gh1175-forgectl"); err != nil || c.Number != 1204 {
		t.Fatalf("synthetic fork: %+v, %v", c, err)
	}
	two := syn
	two.Candidates = slices.Clone(syn.Candidates)
	extra := two.Candidates[3]
	extra.Number, extra.State = 9100, "OPEN"
	first := two.Candidates[3]
	first.State = "OPEN"
	two.Candidates = append(two.Candidates, extra, first)
	if _, err := SelectPR(two, "worker/gh1175-forgectl"); !errors.Is(err, ErrAmbiguousPR) {
		t.Fatalf("two open: %v", err)
	}
	one := syn
	one.Candidates = []Candidate{extra}
	if c, err := SelectPR(one, "worker/gh1175-forgectl"); err != nil || c.Number != 9100 {
		t.Fatalf("one open: %+v, %v", c, err)
	}
}

func TestDecodersRefuse(t *testing.T) {
	pr := string(readFixture(t, "pr_1204.json"))
	for name, body := range map[string]string{
		"reviews page": strings.Replace(pr, `"reviews": {
     "pageInfo": {
      "hasNextPage": false`, `"reviews": {
     "pageInfo": {
      "hasNextPage": true`, 1),
		"graphql error": `{"data":null,"errors":[{"message":"rate limited"}]}`,
		"no data":       `{}`,
	} {
		if body == pr {
			t.Fatalf("%s: the fixture edit did not apply", name)
		}
		if _, err := DecodePR([]byte(body)); !errors.Is(err, ErrResponse) {
			t.Errorf("%s: %v", name, err)
		}
	}
	checks := string(readFixture(t, "checks_1204.json"))
	if _, err := DecodeChecks([]byte(checks), base1204); !errors.Is(err, ErrResponse) {
		t.Errorf("checks for another commit: %v", err)
	}
	if _, err := DecodeChecks([]byte(strings.Replace(checks, `"hasNextPage": false`, `"hasNextPage": true`, 1)), head1204); !errors.Is(err, ErrResponse) {
		t.Errorf("checks page: %v", err)
	}
	morePRs := regexp.MustCompile(`("matchingPullRequests": \{\s*"pageInfo": \{\s*"hasNextPage": )false`).ReplaceAllString(checks, "${1}true")
	if morePRs == checks {
		t.Fatal("the matchingPullRequests page edit did not apply")
	}
	if _, err := DecodeChecks([]byte(morePRs), head1204); !errors.Is(err, ErrResponse) {
		t.Errorf("matching pull requests page: %v", err)
	}
	if _, err := DecodeTree([]byte(`{"truncated":true,"tree":[]}`)); !errors.Is(err, ErrResponse) {
		t.Errorf("truncated tree: %v", err)
	}
	if _, err := DecodeTree([]byte(`{"tree":[]}`)); !errors.Is(err, ErrResponse) {
		t.Errorf("tree without truncated: %v", err)
	}
	if _, err := DecodeCompare([]byte(`{"files":[]}`)); !errors.Is(err, ErrResponse) {
		t.Errorf("compare without status: %v", err)
	}
	disc := string(readFixture(t, "discover_1204.json"))
	if _, err := DecodeDiscovery([]byte(strings.Replace(disc, `"hasNextPage": false`, `"hasNextPage": true`, 1))); !errors.Is(err, ErrResponse) {
		t.Errorf("discovery page: %v", err)
	}
}
