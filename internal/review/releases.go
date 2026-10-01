package review

// The release radar: `forgectl review releases`. It reads the release-rhythm
// registry (cadence-ecosystem docs/release-rhythm.yaml, ADR-0044 there), asks
// GitHub what each enrolled repo has shipped and what is waiting, and flags a
// stall when the nightly beat has stopped moving something.
//
// The split is collect → derive. Collect (releases_github.go) does every read
// and records the answers as RepoFacts. Derive is pure: it turns one registry
// entry plus its facts into a Row, and it holds all of the stall rules, so
// every rule is testable without a network. RepoFacts is also the fixture
// format: a live run can be recorded and replayed through Derive.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/yamlsafe"
)

// Registry classes (ADR-0044 (e)).
const (
	ClassReleasePR  = "release-pr"
	ClassTestflight = "testflight"
	ClassManualCut  = "manual-cut"
	ClassContinuous = "continuous"
	ClassDormant    = "dormant"
)

// Row states. A row is exactly one of these.
const (
	StateOK      = "ok"      // on the beat, nothing stalled
	StatePaused  = "paused"  // the nightly toggle is not `on`
	StateStalled = "stalled" // at least one stall rule fired
	StateUnknown = "unknown" // a read failed; the radar cannot vouch for it
	StateManual  = "manual"  // manual-cut: a human cuts it by design
)

// Stall thresholds (plan Task 5).
const (
	// NoRunWindow is how long an enabled repo may go without an entrypoint
	// run. The beat is daily; 26h leaves two hours of queue slack.
	NoRunWindow = 26 * time.Hour
	// EndpointLagWindow is how long a tap or bucket may trail a release.
	EndpointLagWindow = 24 * time.Hour
)

// Toggle variable per class: the repository variable whose value `on` turns
// the scheduled run on. ship.yml reads SHIP_NIGHTLY; testflight.yml reads
// TESTFLIGHT_NIGHTLY.
var toggleByClass = map[string]string{
	ClassReleasePR:  "SHIP_NIGHTLY",
	ClassTestflight: "TESTFLIGHT_NIGHTLY",
}

// ToggleName returns the class's toggle variable, or "" for classes that
// have no scheduled entrypoint.
func ToggleName(class string) string { return toggleByClass[class] }

// quietReasons are run outcomes that never count toward the "same reason two
// nights running" rule: shipping, having nothing to ship, and being paused.
var quietReasons = map[string]bool{
	"go": true, "no-pr": true, "paused": true, // ship-gate.sh
	"uploaded": true, "no-change": true, // testflight
}

// Registry is the parsed release-rhythm.yaml. Only the fields the radar
// reads are declared; the gate's allowlist and required_checks are ignored.
type Registry struct {
	Version int             `yaml:"version"`
	Repos   []RegistryEntry `yaml:"repos"`
}

// RegistryEntry is one repo's registry row.
type RegistryEntry struct {
	Repo       string     `yaml:"repo" json:"repo"`
	Branch     string     `yaml:"branch" json:"branch"`
	Class      string     `yaml:"class" json:"class"`
	Entrypoint string     `yaml:"entrypoint" json:"entrypoint,omitempty"`
	TagPattern string     `yaml:"tag_pattern" json:"tag_pattern"`
	HumanGates []string   `yaml:"human_gates" json:"human_gates"`
	Endpoints  []Endpoint `yaml:"endpoints" json:"endpoints"`
	Upstream   string     `yaml:"upstream" json:"upstream,omitempty"`
}

// Endpoint is a downstream channel that must catch up to each release.
type Endpoint struct {
	Kind string `yaml:"kind" json:"kind"`
	Repo string `yaml:"repo" json:"repo,omitempty"`
	Path string `yaml:"path" json:"path,omitempty"`
}

// Tracked reports whether the radar gives the entry a full row. Continuous
// and dormant repos appear only in the footer.
func (e RegistryEntry) Tracked() bool {
	switch e.Class {
	case ClassReleasePR, ClassTestflight, ClassManualCut:
		return true
	}
	return false
}

// UpstreamSlug returns the upstream as owner/repo; a bare name means the
// registry owner.
func (e RegistryEntry) UpstreamSlug() string {
	if e.Upstream == "" || strings.Contains(e.Upstream, "/") {
		return e.Upstream
	}
	return RegistryOwner + "/" + e.Upstream
}

// RegistryOwner owns every registry repo (the registry says "GitHub repo
// under cameronsjo/").
const RegistryOwner = "cameronsjo"

// The registry is data, not instructions (its own header says so): every
// value that reaches an argv or an API path is checked against an anchored
// charset first.
var (
	reRegName      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	reRegWorkflow  = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9._-]+\.ya?ml$`)
	reRegPath      = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	reRegUpstream  = regexp.MustCompile(`^([A-Za-z0-9._-]+/)?[A-Za-z0-9._-]+$`)
	validClasses   = map[string]bool{ClassReleasePR: true, ClassTestflight: true, ClassManualCut: true, ClassContinuous: true, ClassDormant: true}
	validEndpoints = map[string]bool{"homebrew-cask": true, "homebrew-formula": true, "scoop": true, "testflight": true}
)

// tagPatterns maps the registry's tag_pattern names to the tags they admit.
var tagPatterns = map[string]*regexp.Regexp{
	"v-semver":         regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`),
	"bare-semver":      regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`),
	"component-semver": regexp.MustCompile(`^[a-z0-9-]+-v[0-9]+\.[0-9]+\.[0-9]+$`),
	"fork-suffix":      regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-[a-z]+\.[0-9]+$`),
	"tf-date":          regexp.MustCompile(`^tf-[0-9]{8}-[0-9]{4}$`),
	"none":             nil,
}

// TagMatches reports whether tag has the entry's tag shape. A "none" pattern
// admits nothing.
func TagMatches(pattern, tag string) bool {
	re := tagPatterns[pattern]
	return re != nil && re.MatchString(tag)
}

// LoadRegistry reads and validates the registry at path.
func LoadRegistry(path string) (Registry, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the registry file (--registry or env)
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	defer func() { _ = f.Close() }()
	// One byte past the cap is enough for ParseRegistry to refuse the file,
	// so a huge one is never read whole.
	raw, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	return ParseRegistry(raw)
}

// maxRegistryBytes is the largest registry ParseRegistry reads (#959). An
// entry runs about 190 bytes, so 256 KiB holds about 1,300 repos, against
// 21 today.
const maxRegistryBytes = 256 << 10

// maxRegistryMappingKeys is the most keys any mapping in the registry may
// have. An entry has 8 and the top level 2 or 3. yaml.v3's struct decode
// compares every key of a mapping with every later key, and does it again
// each time an alias to that mapping expands, so 256 KiB of keys in one
// mapping took 4.5 s to decode. At 64 the whole file stays cheap, aliases
// included.
const maxRegistryMappingKeys = 64

// ParseRegistry decodes and validates registry YAML. Any invalid entry fails
// the whole registry: a radar that silently skipped a malformed row would
// report "no stalls" for a repo it never looked at.
//
// The file is parsed into a node first, and yamlsafe.CheckTree refuses, in
// linear time, what the struct decode would spend superlinear time on (a
// repeated key, a merge key, a mapping over maxRegistryMappingKeys) before
// that decode runs (#959).
func ParseRegistry(raw []byte) (Registry, error) {
	doc, err := yamlsafe.Parse(raw, maxRegistryBytes)
	if errors.Is(err, yamlsafe.ErrTooLarge) {
		return Registry{}, fmt.Errorf("registry is larger than the %d KiB limit", maxRegistryBytes>>10)
	}
	if err != nil {
		return Registry{}, fmt.Errorf("parse registry: %w", err)
	}
	var reg Registry
	if root := yamlsafe.Root(doc); root != nil {
		if err := yamlsafe.CheckTree(root, yamlsafe.Options{MaxKeys: maxRegistryMappingKeys}); err != nil {
			return Registry{}, fmt.Errorf("parse registry: %w", err)
		}
		if err := doc.Decode(&reg); err != nil {
			return Registry{}, fmt.Errorf("parse registry: %w", err)
		}
	}
	return validateRegistry(reg)
}

// validateRegistry checks a decoded registry's version and entries.
func validateRegistry(reg Registry) (Registry, error) {
	if reg.Version != 1 {
		return Registry{}, fmt.Errorf("registry version %d, want 1", reg.Version)
	}
	if len(reg.Repos) == 0 {
		return Registry{}, errors.New("registry lists no repos")
	}
	tracked := 0
	seen := map[string]bool{}
	for i, e := range reg.Repos {
		if err := validateEntry(e); err != nil {
			return Registry{}, fmt.Errorf("registry repos[%d]: %w", i, err)
		}
		if seen[e.Repo] {
			return Registry{}, fmt.Errorf("registry repos[%d]: duplicate repo %s", i, e.Repo)
		}
		seen[e.Repo] = true
		if e.Tracked() {
			tracked++
		}
	}
	// A registry with nothing to judge would always pass --fail-on-stall.
	if tracked == 0 {
		return Registry{}, errors.New("registry lists no release-pr, testflight, or manual-cut repo")
	}
	return reg, nil
}

// validName reports whether s is one repo-name segment: the charset, and
// never "." or "..", which would move an API path up a level.
func validName(s string) bool { return reRegName.MatchString(s) && s != "." && s != ".." }

// validPath reports whether s is a relative path of valid name segments.
func validPath(s string) bool {
	if !reRegPath.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if !validName(seg) {
			return false
		}
	}
	return true
}

func validateEntry(e RegistryEntry) error {
	if !validName(e.Repo) {
		return errors.New("repo is outside the allowed charset")
	}
	if !validClasses[e.Class] {
		return fmt.Errorf("%s: unknown class", e.Repo)
	}
	if !validPath(e.Branch) {
		return fmt.Errorf("%s: branch is outside the allowed charset", e.Repo)
	}
	if _, ok := tagPatterns[e.TagPattern]; !ok {
		return fmt.Errorf("%s: unknown tag_pattern", e.Repo)
	}
	needsEntry := e.Class == ClassReleasePR || e.Class == ClassTestflight
	if needsEntry && !reRegWorkflow.MatchString(e.Entrypoint) {
		return fmt.Errorf("%s: %s needs an entrypoint under .github/workflows/", e.Repo, e.Class)
	}
	if !needsEntry && e.Entrypoint != "" {
		return fmt.Errorf("%s: %s has no entrypoint", e.Repo, e.Class)
	}
	if e.Upstream != "" && (!reRegUpstream.MatchString(e.Upstream) || !validPath(e.Upstream)) {
		return fmt.Errorf("%s: upstream is outside the allowed charset", e.Repo)
	}
	for _, ep := range e.Endpoints {
		if !validEndpoints[ep.Kind] {
			return fmt.Errorf("%s: unknown endpoint kind", e.Repo)
		}
		if ep.Kind == "testflight" {
			continue
		}
		if !validName(ep.Repo) || !validPath(ep.Path) {
			return fmt.Errorf("%s: endpoint repo or path is outside the allowed charset", e.Repo)
		}
	}
	return nil
}

// ---- facts: what collection found, per repo ------------------------------

// RepoFacts is everything collection learned about one repo. It carries only
// fields the radar uses, so a recorded fixture holds nothing else.
type RepoFacts struct {
	Repo string `json:"repo"`
	// LastRelease is the newest shipped release: a GitHub release whose tag
	// has the registry's tag shape, or for testflight the newest upload
	// record (or tf-* tag).
	LastRelease *ReleaseFact `json:"last_release,omitempty"`
	// Unreleased counts commits on the branch since LastRelease; nil when
	// there is no release to compare against.
	Unreleased *int `json:"unreleased,omitempty"`
	// OldestUnreleasedAt is the committer date of the oldest of those.
	OldestUnreleasedAt *time.Time `json:"oldest_unreleased_at,omitempty"`
	// ReleasePRs are the open release PRs (title shape of ship-gate.sh).
	ReleasePRs []PRFact `json:"release_prs,omitempty"`
	// Toggle is the class's nightly toggle variable; nil when the class has
	// none.
	Toggle *ToggleFact `json:"toggle,omitempty"`
	// Runs are the entrypoint's recent runs, newest first. Reason is filled
	// for the newest run and the two newest scheduled runs.
	Runs []RunFact `json:"runs,omitempty"`
	// Endpoints mirrors the registry's endpoints with the version found.
	Endpoints []EndpointFact `json:"endpoints,omitempty"`
	// GateCopySHA256 is the sha256 of .github/scripts/ship-gate.sh on the
	// branch ("" with GateCopyMissing when the file is absent).
	GateCopySHA256  string `json:"gate_copy_sha256,omitempty"`
	GateCopyMissing bool   `json:"gate_copy_missing,omitempty"`
	// UpstreamDueSince is the committer date of the oldest upstream commit
	// newer than LastRelease; nil when upstream has nothing newer.
	UpstreamDueSince *time.Time `json:"upstream_due_since,omitempty"`
	// Errors are categorical read failures ("runs: HTTP 403"). Any error
	// makes the row unknown.
	Errors []string `json:"errors,omitempty"`
}

// ReleaseFact is one shipped release.
type ReleaseFact struct {
	Tag string    `json:"tag"`
	At  time.Time `json:"at"`
	// SHA is the commit the release was cut from, when collection knows it
	// (testflight upload records carry it).
	SHA string `json:"sha,omitempty"`
}

// PRFact is one open release PR.
type PRFact struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"created_at"`
}

// ToggleFact is the value of the class's toggle variable.
type ToggleFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Set   bool   `json:"set"`
	// Source is "variable" when read from the variables API, or "runs" when
	// the token cannot read variables and the value was inferred from the
	// newest scheduled run (the gate reports `paused`; a paused
	// testflight.yml skips its job).
	Source string `json:"source"`
}

// RunFact is one entrypoint run.
type RunFact struct {
	ID         int64     `json:"id"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	// Reason is the gate's reason (ship-gate.sh enum) or, for testflight,
	// one of paused, uploaded, no-change, failed. "" when not read.
	Reason string `json:"reason,omitempty"`
}

// EndpointFact is one endpoint's published version.
type EndpointFact struct {
	Kind    string `json:"kind"`
	Repo    string `json:"repo,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// ---- derive: the pure half -----------------------------------------------

// Row is one tracked repo as the radar reports it.
type Row struct {
	Repo  string `json:"repo"`
	Class string `json:"class"`
	State string `json:"state"`

	LastRelease   string     `json:"last_release,omitempty"`
	LastReleaseAt *time.Time `json:"last_release_at,omitempty"`
	Unreleased    *int       `json:"unreleased_commits,omitempty"`

	ReleasePR *PRFact `json:"release_pr,omitempty"`
	// ReleasePRCount is len(release PRs); the gate refuses more than one.
	ReleasePRCount int `json:"release_pr_count"`

	Toggle  *ToggleFact `json:"toggle,omitempty"`
	LastRun *RunFact    `json:"last_run,omitempty"`

	HumanGates []string `json:"human_gates"`
	// HumanGateDueSince is when the human step became due; nil when nothing
	// is waiting or the age is not visible from GitHub.
	HumanGateDueSince *time.Time `json:"human_gate_due_since,omitempty"`

	Endpoints []EndpointStatus `json:"endpoints"`

	// GateCopy is "match", "drift", "missing", or "" when the class has no
	// vendored gate.
	GateCopy string `json:"gate_copy,omitempty"`

	Stalls []string `json:"stalls"`
	Errors []string `json:"errors"`
}

// EndpointStatus is one endpoint judged against the last release.
type EndpointStatus struct {
	EndpointFact
	// Behind is true when the endpoint's version differs from the release.
	Behind bool `json:"behind"`
}

// FooterEntry is a continuous or dormant repo: listed, never judged.
type FooterEntry struct {
	Repo       string   `json:"repo"`
	Class      string   `json:"class"`
	HumanGates []string `json:"human_gates"`
}

// Report is the whole radar.
type Report struct {
	GeneratedAt   time.Time `json:"generated_at"`
	CanonicalGate string    `json:"canonical_gate_sha256"`
	// Failing is true when any row is stalled or unknown: the --fail-on-stall verdict.
	FailingRows bool          `json:"failing"`
	Rows        []Row         `json:"repos"`
	Footer      []FooterEntry `json:"footer"`
}

// Failing reports whether --fail-on-stall should exit 1: any stalled row or
// any unknown row (a read error fails closed).
func (r Report) Failing() bool {
	for _, row := range r.Rows {
		if row.State == StateStalled || row.State == StateUnknown {
			return true
		}
	}
	return false
}

// BuildReport derives every tracked row and the footer. facts is keyed by
// repo; a tracked repo with no facts is reported unknown.
func BuildReport(reg Registry, facts map[string]RepoFacts, canonicalGate string, now time.Time) Report {
	rep := Report{GeneratedAt: now.UTC(), CanonicalGate: canonicalGate, Rows: []Row{}, Footer: []FooterEntry{}}
	for _, e := range reg.Repos {
		if !e.Tracked() {
			rep.Footer = append(rep.Footer, FooterEntry{Repo: e.Repo, Class: e.Class, HumanGates: nonNil(e.HumanGates)})
			continue
		}
		f, ok := facts[e.Repo]
		if !ok {
			f = RepoFacts{Repo: e.Repo, Errors: []string{"no facts collected"}}
		}
		rep.Rows = append(rep.Rows, Derive(e, f, canonicalGate, now))
	}
	rep.FailingRows = rep.Failing()
	return rep
}

// Derive turns one registry entry and its facts into a Row, applying every
// stall rule. It is pure: same inputs, same Row.
func Derive(e RegistryEntry, f RepoFacts, canonicalGate string, now time.Time) Row {
	row := Row{
		Repo:           e.Repo,
		Class:          e.Class,
		Unreleased:     f.Unreleased,
		ReleasePRCount: len(f.ReleasePRs),
		Toggle:         f.Toggle,
		HumanGates:     nonNil(e.HumanGates),
		Endpoints:      []EndpointStatus{},
		Stalls:         []string{},
		Errors:         append([]string{}, f.Errors...),
	}
	if f.LastRelease != nil {
		row.LastRelease = f.LastRelease.Tag
		at := f.LastRelease.At
		row.LastReleaseAt = &at
	}
	if len(f.ReleasePRs) > 0 {
		oldest := f.ReleasePRs[0]
		for _, p := range f.ReleasePRs[1:] {
			if p.CreatedAt.Before(oldest.CreatedAt) {
				oldest = p
			}
		}
		row.ReleasePR = &oldest
	}
	if len(f.Runs) > 0 {
		r := f.Runs[0]
		row.LastRun = &r
	}

	// Human gates: a manual cut is due from its oldest uncut commit, or from
	// the oldest upstream commit it has not taken, whichever came first.
	// App Store release and milestone tags have no age visible here.
	if e.Class == ClassManualCut {
		row.HumanGateDueSince = earliest(f.OldestUnreleasedAt, f.UpstreamDueSince)
		// A commit merged after the cut can carry an older committer date
		// (an upstream sync brings upstream's dates along). Nothing is due
		// before the last cut, so clamp to it.
		if row.HumanGateDueSince != nil && row.LastReleaseAt != nil && row.HumanGateDueSince.Before(*row.LastReleaseAt) {
			at := *row.LastReleaseAt
			row.HumanGateDueSince = &at
		}
	}

	enabled := f.Toggle != nil && f.Toggle.Value == "on"
	scheduled := e.Class == ClassReleasePR || e.Class == ClassTestflight

	// Run-cadence rules apply only while the toggle is on: a paused repo is
	// shown, not judged on its beat.
	if scheduled && enabled {
		row.Stalls = append(row.Stalls, runStalls(f.Runs, now)...)
	}
	// A half-shipped release is broken whether or not the beat is on.
	if row.LastRun != nil && row.LastRun.Reason == "half-shipped" {
		row.Stalls = append(row.Stalls, "last ship run reports half-shipped")
	}

	// Endpoint lag: every endpoint must carry the last release within 24h.
	for _, ep := range f.Endpoints {
		st := EndpointStatus{EndpointFact: ep}
		if ep.Kind != "testflight" && f.LastRelease != nil {
			st.Behind = ep.Version != releaseVersion(f.LastRelease.Tag)
			if st.Behind && now.Sub(f.LastRelease.At) > EndpointLagWindow {
				row.Stalls = append(row.Stalls, fmt.Sprintf("%s %s is at %s; %s shipped %s ago",
					ep.Kind, ep.Path, orNone(ep.Version), f.LastRelease.Tag, Age(now, f.LastRelease.At)))
			}
		}
		row.Endpoints = append(row.Endpoints, st)
	}

	// Gate-copy drift: the vendored gate must be the canonical bytes.
	if e.Class == ClassReleasePR {
		switch {
		case f.GateCopyMissing:
			row.GateCopy = "missing"
			row.Stalls = append(row.Stalls, "gate copy .github/scripts/ship-gate.sh is missing")
		case f.GateCopySHA256 == "" || canonicalGate == "":
			// Unknowable, not clean: fail closed.
			row.Errors = append(row.Errors, "gate copy: hash unavailable")
		case f.GateCopySHA256 == canonicalGate:
			row.GateCopy = "match"
		default:
			row.GateCopy = "drift"
			row.Stalls = append(row.Stalls, "gate copy drifted from the canonical ship-gate.sh")
		}
	}

	switch {
	case len(row.Errors) > 0:
		row.State = StateUnknown
	case len(row.Stalls) > 0:
		row.State = StateStalled
	case e.Class == ClassManualCut:
		row.State = StateManual
	case scheduled && !enabled:
		row.State = StatePaused
	default:
		row.State = StateOK
	}
	return row
}

// runStalls applies the two beat rules to an enabled repo's runs (newest
// first): no run inside NoRunWindow, and the same non-quiet reason on the
// two newest scheduled runs.
func runStalls(runs []RunFact, now time.Time) []string {
	var out []string
	if len(runs) == 0 {
		out = append(out, "no ship run on record")
	} else if gap := now.Sub(runs[0].CreatedAt); gap > NoRunWindow {
		out = append(out, fmt.Sprintf("no ship run in 26h (last %s ago)", Age(now, runs[0].CreatedAt)))
	}
	var sched []RunFact
	for _, r := range runs {
		if r.Event == "schedule" && r.Status == "completed" {
			sched = append(sched, r)
			if len(sched) == 2 {
				break
			}
		}
	}
	if len(sched) == 2 && sched[0].Reason != "" && sched[0].Reason == sched[1].Reason && !quietReasons[sched[0].Reason] {
		out = append(out, fmt.Sprintf("reason %s on the last 2 scheduled runs", sched[0].Reason))
	}
	return out
}

// ---- small pure helpers ---------------------------------------------------

// ReGateAnnotation matches the notice or error line ship-gate.sh's finish()
// writes: "ship-gate <reason>: <detail>".
var ReGateAnnotation = regexp.MustCompile(`^ship-gate (no-pr|paused|ci-red|not-verified|half-shipped|go|error): `)

// GateReason extracts the gate reason from a job's annotation messages, or
// "" when none carries one.
func GateReason(messages []string) string {
	for _, m := range messages {
		if sub := ReGateAnnotation.FindStringSubmatch(m); sub != nil {
			return sub[1]
		}
	}
	return ""
}

var (
	reRubyVersion = regexp.MustCompile(`(?m)^\s*version\s+"([^"]+)"\s*$`)
)

// EndpointVersion reads the published version out of an endpoint file: the
// `version "X"` line of a Homebrew formula or cask, or `.version` of a Scoop
// manifest. It returns "" when the file has none.
func EndpointVersion(kind string, content []byte) string {
	switch kind {
	case "homebrew-cask", "homebrew-formula":
		if m := reRubyVersion.FindSubmatch(content); m != nil {
			return string(m[1])
		}
	case "scoop":
		var man struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(content, &man) == nil {
			return man.Version
		}
	}
	return ""
}

// releaseVersion strips the tag prefix so a tag compares with the version an
// endpoint file carries (v0.19.0 → 0.19.0, v0.9.1-palette.1 → 0.9.1-palette.1).
func releaseVersion(tag string) string { return reTagPrefix.ReplaceAllString(tag, "") }

// reTagPrefix is what a tag carries before the version: "v", or a
// component name plus "-v" (envctl-v0.1.1).
var reTagPrefix = regexp.MustCompile(`^([a-z0-9-]+-)?v`)

// SHA256Hex is the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ReReleasePRTitle is ship-gate.sh's TITLE_RE.
var ReReleasePRTitle = regexp.MustCompile(`^chore(\(main\))?: release v?[0-9]+\.[0-9]+\.[0-9]+$`)

// Age renders now-t as a compact duration: 45m, 5h, 3d. Future times read 0m.
func Age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	}
	return a
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
