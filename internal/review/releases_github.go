package review

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// maxRadarConcurrency caps how many repos are read at once. Each repo costs
// roughly a dozen API calls; eight repos at four wide stays far from any
// secondary rate limit.
const maxRadarConcurrency = 4

// APIGetter is the read seam the collector uses: a GET against the GitHub
// REST API that returns the JSON body.
type APIGetter interface {
	Get(ctx context.Context, apiPath string) ([]byte, error)
}

// ErrAPINotFound marks a 404, which several reads treat as an answer
// (a missing gate copy, a missing endpoint file) rather than a failure.
var ErrAPINotFound = errors.New("not found")

// APIStatusError is a non-404 HTTP failure. It carries only the status code:
// gh stderr can hold tokens and control bytes, and this error is rendered.
type APIStatusError struct{ Status int }

func (e *APIStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// GhAPI implements APIGetter over `gh api`. Pass it a host-pinned runner
// (githubauth.Runner) so an ambient GH_HOST cannot redirect the reads; gh
// takes its credential from GH_TOKEN / GITHUB_TOKEN or its stored login.
type GhAPI struct{ Run exec.Runner }

var reHTTPStatus = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)

// Get implements APIGetter. File contents come through the JSON contents
// API (base64), never the raw media type: the runner trims trailing
// newlines, which would change a file's sha256.
func (g GhAPI) Get(ctx context.Context, apiPath string) ([]byte, error) {
	out, err := g.Run.Run(ctx, "gh", "api", "--method", "GET", apiPath)
	if err == nil {
		return []byte(out), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	var ce *exec.CommandError
	if errors.As(err, &ce) {
		if m := reHTTPStatus.FindStringSubmatch(ce.Stderr); m != nil {
			code, _ := strconv.Atoi(m[1])
			if code == 404 {
				return nil, ErrAPINotFound
			}
			return nil, &APIStatusError{Status: code}
		}
	}
	slog.Warn("gh api read failed.", "path", apiPath, "error", err)
	return nil, errors.New("gh api failed")
}

// Collect reads facts for every tracked registry repo. A repo's read errors
// land in its RepoFacts.Errors; Collect itself never fails.
func Collect(ctx context.Context, api APIGetter, reg Registry) map[string]RepoFacts {
	var tracked []RegistryEntry
	for _, e := range reg.Repos {
		if e.Tracked() {
			tracked = append(tracked, e)
		}
	}
	results := make([]RepoFacts, len(tracked))
	sem := make(chan struct{}, maxRadarConcurrency)
	var wg sync.WaitGroup
	for i, e := range tracked {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = collectRepo(ctx, api, e)
		}()
	}
	wg.Wait()
	out := make(map[string]RepoFacts, len(results))
	for _, f := range results {
		out[f.Repo] = f
	}
	return out
}

// collector gathers one repo's facts, recording each failed read as a
// categorical error and carrying on with the reads that do not depend on it.
type collector struct {
	ctx  context.Context
	api  APIGetter
	e    RegistryEntry
	slug string
	f    RepoFacts
}

func (c *collector) fail(step string, err error) {
	var se *APIStatusError
	msg := "failed"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = "timed out"
	case errors.Is(err, context.Canceled):
		msg = "canceled"
	case errors.Is(err, ErrAPINotFound):
		msg = "HTTP 404"
	case errors.As(err, &se):
		msg = se.Error()
	case errors.Is(err, errShape):
		msg = "unexpected response shape"
	}
	c.f.Errors = append(c.f.Errors, step+": "+msg)
}

var errShape = errors.New("unexpected response shape")

// getFile reads a file through the contents API and decodes its base64
// body.
func (c *collector) getFile(apiPath string) ([]byte, error) {
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.getJSON(apiPath, &file); err != nil {
		return nil, err
	}
	if file.Type != "file" || file.Encoding != "base64" {
		return nil, errShape
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errShape, err)
	}
	return body, nil
}

// getJSON GETs apiPath and decodes it into v.
func (c *collector) getJSON(apiPath string, v any) error {
	body, err := c.api.Get(c.ctx, apiPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %w", errShape, err)
	}
	return nil
}

func collectRepo(ctx context.Context, api APIGetter, e RegistryEntry) RepoFacts {
	c := &collector{ctx: ctx, api: api, e: e, slug: RegistryOwner + "/" + e.Repo, f: RepoFacts{Repo: e.Repo}}
	var artifacts []ghArtifact
	if e.Class == ClassTestflight {
		artifacts = c.testflightRelease()
	} else {
		c.lastRelease()
	}
	c.unreleased()
	if e.Class == ClassReleasePR {
		c.releasePRs()
		c.gateCopy()
	}
	name := ToggleName(e.Class)
	inferToggle := false
	if name != "" {
		inferToggle = !c.toggle(name)
	}
	if e.Entrypoint != "" {
		c.runs(artifacts)
	}
	if inferToggle {
		c.toggleFromRuns(name)
	}
	c.endpoints()
	if e.Class == ClassManualCut && e.Upstream != "" {
		c.upstream()
	}
	return c.f
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
}

// lastRelease picks the newest published, non-prerelease GitHub release
// whose tag has the registry's shape. A per-push `beta` release is skipped
// by the shape check, not by name.
func (c *collector) lastRelease() {
	var rels []ghRelease
	if err := c.getJSON(fmt.Sprintf("repos/%s/releases?per_page=100", c.slug), &rels); err != nil {
		c.fail("releases", err)
		return
	}
	var best *ghRelease
	for i := range rels {
		r := &rels[i]
		if r.Draft || r.Prerelease || !TagMatches(c.e.TagPattern, r.TagName) {
			continue
		}
		if best == nil || r.PublishedAt.After(best.PublishedAt) {
			best = r
		}
	}
	if best == nil {
		// No release means no endpoint or due-age check can run, so the row
		// must not read healthy.
		c.f.Errors = append(c.f.Errors, fmt.Sprintf("releases: none matching %s in the newest %d", c.e.TagPattern, len(rels)))
		return
	}
	c.f.LastRelease = &ReleaseFact{Tag: best.TagName, At: best.PublishedAt}
}

type ghArtifact struct {
	CreatedAt   time.Time `json:"created_at"`
	Expired     bool      `json:"expired"`
	WorkflowRun struct {
		ID      int64  `json:"id"`
		HeadSHA string `json:"head_sha"`
	} `json:"workflow_run"`
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// testflightRelease takes the newest upload record (the `testflight-upload`
// artifact testflight.yml writes after a real upload) as the last release,
// falling back to the newest tf-* tag when the repo has no record yet. It
// returns the artifacts so run reasons can tell an upload from a no-op.
func (c *collector) testflightRelease() []ghArtifact {
	var resp struct {
		Artifacts []ghArtifact `json:"artifacts"`
	}
	if err := c.getJSON(fmt.Sprintf("repos/%s/actions/artifacts?name=testflight-upload&per_page=50", c.slug), &resp); err != nil {
		c.fail("upload records", err)
		return nil
	}
	for _, a := range resp.Artifacts {
		if a.Expired || !reSHA.MatchString(a.WorkflowRun.HeadSHA) {
			continue
		}
		if c.f.LastRelease == nil || a.CreatedAt.After(c.f.LastRelease.At) {
			c.f.LastRelease = &ReleaseFact{Tag: "upload@" + a.WorkflowRun.HeadSHA[:7], At: a.CreatedAt, SHA: a.WorkflowRun.HeadSHA}
		}
	}
	if c.f.LastRelease == nil && c.e.TagPattern == "tf-date" {
		c.newestTFTag()
	}
	if c.f.LastRelease == nil {
		c.f.Errors = append(c.f.Errors, "releases: no unexpired upload record or tf-* tag")
	}
	return resp.Artifacts
}

// newestTFTag reads the newest tf-YYYYMMDD-HHMM tag. The name sorts by date,
// so the greatest name is the newest; its commit date is the release time.
func (c *collector) newestTFTag() {
	var tags []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.getJSON(fmt.Sprintf("repos/%s/tags?per_page=100", c.slug), &tags); err != nil {
		c.fail("tags", err)
		return
	}
	var names []string
	shas := map[string]string{}
	for _, t := range tags {
		if TagMatches("tf-date", t.Name) && reSHA.MatchString(t.Commit.SHA) {
			names = append(names, t.Name)
			shas[t.Name] = t.Commit.SHA
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	name := names[len(names)-1]
	var commit struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.getJSON(fmt.Sprintf("repos/%s/commits/%s", c.slug, shas[name]), &commit); err != nil {
		c.fail("tag commit", err)
		return
	}
	c.f.LastRelease = &ReleaseFact{Tag: name, At: commit.Commit.Committer.Date, SHA: shas[name]}
}

type ghCompare struct {
	AheadBy *int `json:"ahead_by"`
	Commits []struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	} `json:"commits"`
}

// unreleased counts branch commits since the last release. The compare API
// lists commits oldest first, so commits[0] is the oldest uncut one.
func (c *collector) unreleased() {
	if c.f.LastRelease == nil {
		return
	}
	base := c.f.LastRelease.Tag
	if c.f.LastRelease.SHA != "" {
		base = c.f.LastRelease.SHA
	}
	var cmp ghCompare
	if err := c.getJSON(fmt.Sprintf("repos/%s/compare/%s...%s", c.slug, base, c.e.Branch), &cmp); err != nil {
		c.fail("compare", err)
		return
	}
	if cmp.AheadBy == nil {
		c.fail("compare", errShape)
		return
	}
	n := *cmp.AheadBy
	c.f.Unreleased = &n
	if n > 0 && len(cmp.Commits) > 0 {
		t := cmp.Commits[0].Commit.Committer.Date
		c.f.OldestUnreleasedAt = &t
	}
}

// releasePRs lists open same-repo PRs into the branch whose title has the
// release shape ship-gate.sh accepts.
func (c *collector) releasePRs() {
	var pulls []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		CreatedAt time.Time `json:"created_at"`
		Head      struct {
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := c.getJSON(fmt.Sprintf("repos/%s/pulls?state=open&base=%s&per_page=100", c.slug, c.e.Branch), &pulls); err != nil {
		c.fail("pulls", err)
		return
	}
	for _, p := range pulls {
		if p.Head.Repo == nil || p.Head.Repo.FullName != c.slug || !ReReleasePRTitle.MatchString(p.Title) {
			continue
		}
		c.f.ReleasePRs = append(c.f.ReleasePRs, PRFact{Number: p.Number, CreatedAt: p.CreatedAt})
	}
}

// gateCopy hashes the vendored gate on the branch.
func (c *collector) gateCopy() {
	body, err := c.getFile(fmt.Sprintf("repos/%s/contents/.github/scripts/ship-gate.sh?ref=%s", c.slug, c.e.Branch))
	switch {
	case errors.Is(err, ErrAPINotFound):
		c.f.GateCopyMissing = true
	case err != nil:
		c.fail("gate copy", err)
	default:
		c.f.GateCopySHA256 = SHA256Hex(body)
	}
}

// toggle reads the class's nightly toggle. An unset variable is an answer
// (paused), not an error. It returns false when the token may not read
// variables (403 or 404 on the list): a GitHub App token cannot be granted
// variables read through create-github-app-token, so the nightly job infers
// the toggle from runs instead (toggleFromRuns).
func (c *collector) toggle(name string) bool {
	var resp struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	err := c.getJSON(fmt.Sprintf("repos/%s/actions/variables?per_page=100", c.slug), &resp)
	var se *APIStatusError
	if errors.Is(err, ErrAPINotFound) || (errors.As(err, &se) && se.Status == 403) {
		return false
	}
	if err != nil {
		c.fail("variables", err)
		return true
	}
	t := &ToggleFact{Name: name, Source: "variable"}
	for _, v := range resp.Variables {
		if v.Name == name {
			t.Value, t.Set = v.Value, true
		}
	}
	c.f.Toggle = t
	return true
}

// toggleFromRuns infers the toggle from the newest completed scheduled run
// with a reason: `paused` means off, anything else means on. With no such
// run the toggle is unknowable, and the row fails closed.
func (c *collector) toggleFromRuns(name string) {
	for _, r := range c.f.Runs {
		if r.Event != "schedule" || r.Status != "completed" || r.Reason == "" {
			continue
		}
		v := "on"
		if r.Reason == "paused" {
			v = "off"
		}
		c.f.Toggle = &ToggleFact{Name: name, Value: v, Set: true, Source: "runs"}
		return
	}
	c.f.Errors = append(c.f.Errors, "toggle: variables not readable and no scheduled run to infer from")
}

type ghRun struct {
	ID         int64     `json:"id"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion *string   `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
}

type ghJob struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Conclusion *string `json:"conclusion"`
}

// runs lists the entrypoint's recent runs and reads a reason for the newest
// run and the two newest completed scheduled runs, the ones the stall rules
// look at.
func (c *collector) runs(artifacts []ghArtifact) {
	var resp struct {
		WorkflowRuns []ghRun `json:"workflow_runs"`
	}
	wf := path.Base(c.e.Entrypoint)
	if err := c.getJSON(fmt.Sprintf("repos/%s/actions/workflows/%s/runs?per_page=50&exclude_pull_requests=true", c.slug, wf), &resp); err != nil {
		c.fail("runs", err)
		return
	}
	sched := 0
	for i, r := range resp.WorkflowRuns {
		rf := RunFact{ID: r.ID, Event: r.Event, Status: r.Status, CreatedAt: r.CreatedAt}
		if r.Conclusion != nil {
			rf.Conclusion = *r.Conclusion
		}
		isSched := r.Event == "schedule" && r.Status == "completed"
		if (i == 0 || (isSched && sched < 2)) && r.Status == "completed" {
			rf.Reason = c.runReason(rf, artifacts)
		}
		if isSched {
			sched++
		}
		c.f.Runs = append(c.f.Runs, rf)
	}
}

func (c *collector) runReason(r RunFact, artifacts []ghArtifact) string {
	var resp struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := c.getJSON(fmt.Sprintf("repos/%s/actions/runs/%d/jobs", c.slug, r.ID), &resp); err != nil {
		c.fail("run jobs", err)
		return ""
	}
	if c.e.Class == ClassTestflight {
		return testflightReason(r, resp.Jobs, artifacts)
	}
	for _, j := range resp.Jobs {
		if j.Name != "Gate" {
			continue
		}
		var anns []struct {
			Message string `json:"message"`
		}
		if err := c.getJSON(fmt.Sprintf("repos/%s/check-runs/%d/annotations?per_page=50", c.slug, j.ID), &anns); err != nil {
			c.fail("gate annotations", err)
			return ""
		}
		msgs := make([]string, 0, len(anns))
		for _, a := range anns {
			msgs = append(msgs, a.Message)
		}
		reason := GateReason(msgs)
		if reason == "" && (r.Conclusion == "success" || r.Conclusion == "failure") {
			// The gate always writes its reason; without it the repeat and
			// half-shipped rules cannot run.
			c.f.Errors = append(c.f.Errors, fmt.Sprintf("run %d: gate wrote no reason", r.ID))
		}
		return reason
	}
	if r.Conclusion == "success" || r.Conclusion == "failure" {
		c.f.Errors = append(c.f.Errors, fmt.Sprintf("run %d: no Gate job", r.ID))
	}
	return ""
}

// testflightReason names a completed testflight.yml run: paused when every
// job was skipped (the toggle is off), failed when it did not succeed,
// uploaded when it left an upload record, else no-change (decide.sh found
// main unmoved).
func testflightReason(r RunFact, jobs []ghJob, artifacts []ghArtifact) string {
	allSkipped := len(jobs) > 0
	for _, j := range jobs {
		if j.Conclusion == nil || *j.Conclusion != "skipped" {
			allSkipped = false
		}
	}
	switch {
	case allSkipped || r.Conclusion == "skipped":
		return "paused"
	case r.Conclusion != "success":
		return "failed"
	}
	for _, a := range artifacts {
		if a.WorkflowRun.ID == r.ID {
			return "uploaded"
		}
	}
	return "no-change"
}

// endpoints reads each tap or bucket file's version. A missing file reads
// as no version (so the endpoint shows behind), not as an error.
func (c *collector) endpoints() {
	for _, ep := range c.e.Endpoints {
		fact := EndpointFact{Kind: ep.Kind, Repo: ep.Repo, Path: ep.Path}
		if ep.Kind != "testflight" {
			body, err := c.getFile(fmt.Sprintf("repos/%s/%s/contents/%s", RegistryOwner, ep.Repo, ep.Path))
			switch {
			case errors.Is(err, ErrAPINotFound):
			case err != nil:
				c.fail("endpoint "+ep.Path, err)
			default:
				fact.Version = EndpointVersion(ep.Kind, body)
			}
		}
		c.f.Endpoints = append(c.f.Endpoints, fact)
	}
}

// upstream finds the oldest upstream commit newer than the last release: the
// moment a sync or export became due. With no release there is nothing to
// measure from.
func (c *collector) upstream() {
	if c.f.LastRelease == nil {
		return
	}
	var commits []struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	since := c.f.LastRelease.At.UTC().Add(time.Second).Format(time.RFC3339)
	if err := c.getJSON(fmt.Sprintf("repos/%s/commits?since=%s&per_page=100", c.e.UpstreamSlug(), since), &commits); err != nil {
		c.fail("upstream commits", err)
		return
	}
	// Newest first; with 100 on the page the true oldest may be further
	// back, so this is a lower bound on the age.
	if n := len(commits); n > 0 {
		t := commits[n-1].Commit.Committer.Date
		c.f.UpstreamDueSince = &t
	}
}
