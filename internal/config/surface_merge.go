package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SurfaceMergeConfig is [surface.merge]: the worker PR merge policy
// (ADR-0011, 2026-10-09 amendment). Any error, a missing table, a machine
// mismatch, or a config file that is not a regular 0600 file owned by the
// user resolves the mode to off (ResolveMerge). A value out of shape is also
// refused when the file loads, as [surface.intake]'s are.
type SurfaceMergeConfig struct {
	// Mode is off, manual or auto. Absent is off.
	Mode string `toml:"mode"`
	// Machine is MergeMachineDigest of the host this policy is for; any
	// other host resolves it to off, so a config synced across machines
	// turns itself off.
	Machine string `toml:"machine"`
	// Approvers are the approver kinds the policy accepts:
	// MergeApproverCadenceReview and MergeApproverCodeRabbit.
	Approvers []string `toml:"approvers"`
	// MarkerAuthorID is the operator's numeric GitHub user id: only
	// cadence-review markers posted by it count. Required whenever mode is
	// manual or auto, whatever approvers lists.
	MarkerAuthorID *int64 `toml:"marker_author_id"`
	// RequiredReviewers are the cadence-review reviewer names that each need
	// a passing marker at the head.
	RequiredReviewers []string `toml:"required_reviewers"`
	// Method is the merge method; only "squash" is implemented. Absent is
	// squash.
	Method string `toml:"method"`
	// Repos are the repositories (owner/name) a merge may happen in.
	Repos []string `toml:"repos"`
	// Workflow pins each repository's required checks to one workflow file
	// (".github/workflows/<file>.yml").
	Workflow map[string]string `toml:"workflow"`
	// RequiredChecks are each repository's required check-run names.
	RequiredChecks map[string][]string `toml:"required_checks"`
	// Paths are each repository's path allowlist globs.
	Paths map[string][]string `toml:"paths"`
	// unknown lists the undecoded keys under [surface.merge], set by
	// DecodeStrict. A misspelled key would leave its field absent, so
	// Resolve refuses any.
	unknown []string
}

// MergeMode is a resolved [surface.merge] mode.
type MergeMode string

const (
	// MergeOff: nothing merges through forgectl.
	MergeOff MergeMode = "off"
	// MergeManual: `forgectl surface merge` merges when the policy passes;
	// the drain never does.
	MergeManual MergeMode = "manual"
	// MergeAuto: the drain's autopilot step merges too.
	MergeAuto MergeMode = "auto"
)

// Approver kinds.
const (
	MergeApproverCadenceReview = "cadence-review"
	MergeApproverCodeRabbit    = "coderabbit"
)

// MergeMethodSquash is the one merge method implemented.
const MergeMethodSquash = "squash"

// MergeMachineSalt is appended to the host name before hashing, so the
// digest names this use and nothing else. It is part of the digest's
// definition: changing it changes every machine's value.
const MergeMachineSalt = "forgectl-merge-v1"

// mergeMachineLen is the digest's length in hex characters.
const mergeMachineLen = 12

// Limits on [surface.merge] lists.
const (
	maxMergeRepos     = 16
	maxMergeListLen   = 64
	maxMergeCheckName = 100
)

// MergeMachineDigest is the first 12 hex characters of
// sha256(hostname + MergeMachineSalt).
func MergeMachineDigest(hostname string) string {
	sum := sha256.Sum256([]byte(hostname + MergeMachineSalt))
	return hex.EncodeToString(sum[:])[:mergeMachineLen]
}

// LocalMergeMachine is MergeMachineDigest of os.Hostname().
func LocalMergeMachine() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("read the host name: %w", err)
	}
	return MergeMachineDigest(h), nil
}

// MergeRepo is one repository's resolved policy entries.
type MergeRepo struct {
	// Name is the owner/name as written in repos.
	Name           string
	Workflow       string
	RequiredChecks []string
	Paths          []string
}

// MergeSettings is [surface.merge] resolved. Mode is MergeOff whenever
// anything failed, with OffReason saying why; the other fields are then
// whatever resolved before the failure and must not be acted on.
type MergeSettings struct {
	Mode              MergeMode
	OffReason         string
	Machine           string
	Approvers         []string
	MarkerAuthorID    int64
	RequiredReviewers []string
	Method            string
	Repos             []MergeRepo
}

// Repo returns the entry for name, compared case-insensitively.
func (s MergeSettings) Repo(name string) (MergeRepo, bool) {
	for _, r := range s.Repos {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return MergeRepo{}, false
}

// HasApprover reports whether kind is on approvers.
func (s MergeSettings) HasApprover(kind string) bool { return slices.Contains(s.Approvers, kind) }

var (
	reMergeMachine   = regexp.MustCompile(`^[0-9a-f]{12}$`)
	reMergeReviewer  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	reMergeWorkflow  = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9._-]+\.ya?ml$`)
	reMergeRepoPart  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	errMergeTableOff = errors.New("[surface.merge] is not set")
)

func checkMergeRepoName(name string) error {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || !reMergeRepoPart.MatchString(owner) || !reMergeRepoPart.MatchString(repo) ||
		owner == "." || owner == ".." || repo == "." || repo == ".." {
		return fmt.Errorf("want owner/name, got %s", quoteConfigValue(name))
	}
	return nil
}

// checkMergeCheckName refuses a check-run name that is empty, padded,
// longer than 100 bytes, or not printable ASCII.
func checkMergeCheckName(name string) error {
	if name == "" || len(name) > maxMergeCheckName || strings.TrimSpace(name) != name {
		return fmt.Errorf("want a check name of 1-%d characters with no leading or trailing space, got %s", maxMergeCheckName, quoteConfigValue(name))
	}
	for _, r := range name {
		if r < ' ' || r > '~' {
			return fmt.Errorf("want a check name of printable ASCII, got %s", quoteConfigValue(name))
		}
	}
	return nil
}

// Resolve checks every [surface.merge] value's shape and resolves it. It
// does not check the machine or the file; ResolveMerge does. A table with
// no mode, or mode "off", resolves to off with no error.
func (c SurfaceMergeConfig) Resolve() (MergeSettings, error) {
	fail := func(format string, a ...any) (MergeSettings, error) {
		return MergeSettings{Mode: MergeOff}, fmt.Errorf("[surface.merge] "+format, a...)
	}
	if len(c.unknown) > 0 {
		return fail("unknown key %s; the keys are mode, machine, approvers, marker_author_id, required_reviewers, method, repos, workflow, required_checks and paths", quoteConfigValue(c.unknown[0]))
	}
	s := MergeSettings{Mode: MergeOff, Method: MergeMethodSquash}
	switch MergeMode(c.Mode) {
	case "", MergeOff:
	case MergeManual, MergeAuto:
		s.Mode = MergeMode(c.Mode)
	default:
		return fail("mode: want off, manual or auto, got %s", quoteConfigValue(c.Mode))
	}
	if c.Machine != "" && !reMergeMachine.MatchString(c.Machine) {
		return fail("machine: want the 12 lowercase hex characters `forgectl surface merge-machine` prints, got %s", quoteConfigValue(c.Machine))
	}
	s.Machine = c.Machine
	if c.Method != "" && c.Method != MergeMethodSquash {
		return fail("method: want %q, the one method implemented, got %s", MergeMethodSquash, quoteConfigValue(c.Method))
	}
	if len(c.Approvers) > 2 {
		return fail("approvers: want at most %q and %q, got %d entries", MergeApproverCadenceReview, MergeApproverCodeRabbit, len(c.Approvers))
	}
	for _, a := range c.Approvers {
		if a != MergeApproverCadenceReview && a != MergeApproverCodeRabbit {
			return fail("approvers: want %q or %q, got %s", MergeApproverCadenceReview, MergeApproverCodeRabbit, quoteConfigValue(a))
		}
		if slices.Contains(s.Approvers, a) {
			return fail("approvers: %s is listed twice", quoteConfigValue(a))
		}
		s.Approvers = append(s.Approvers, a)
	}
	if c.MarkerAuthorID != nil {
		if *c.MarkerAuthorID <= 0 {
			return fail("marker_author_id: want the operator's numeric GitHub user id, got %d", *c.MarkerAuthorID)
		}
		s.MarkerAuthorID = *c.MarkerAuthorID
	}
	if len(c.RequiredReviewers) > maxMergeListLen {
		return fail("required_reviewers: at most %d entries, got %d", maxMergeListLen, len(c.RequiredReviewers))
	}
	for _, r := range c.RequiredReviewers {
		if !reMergeReviewer.MatchString(r) {
			return fail("required_reviewers: want a reviewer name of 1-40 characters of a-z, 0-9 and '-', got %s", quoteConfigValue(r))
		}
		if slices.Contains(s.RequiredReviewers, r) {
			return fail("required_reviewers: %s is listed twice", quoteConfigValue(r))
		}
		s.RequiredReviewers = append(s.RequiredReviewers, r)
	}
	// Every approver set reads the markers: a reviewer's open Critical or
	// Important finding refuses the merge whichever approver passes, so the
	// id whose markers count is needed whenever the policy is on.
	if s.Mode != MergeOff && s.MarkerAuthorID == 0 {
		return fail("marker_author_id: required whenever mode is manual or auto: a cadence-review marker with an open finding refuses the merge under every approver set, and only markers by this id are read")
	}
	if s.HasApprover(MergeApproverCadenceReview) {
		if len(s.RequiredReviewers) == 0 {
			return fail("required_reviewers: at least one reviewer name is required when approvers lists %q", MergeApproverCadenceReview)
		}
	}
	repos, err := c.resolveRepos()
	if err != nil {
		return MergeSettings{Mode: MergeOff}, err
	}
	s.Repos = repos
	return s, nil
}

// resolveRepos checks repos and the three per-repository tables: every
// repository has a workflow, at least one required check and at least one
// path glob, and every table key names a listed repository.
func (c SurfaceMergeConfig) resolveRepos() ([]MergeRepo, error) {
	if len(c.Repos) > maxMergeRepos {
		return nil, fmt.Errorf("[surface.merge] repos: at most %d entries, got %d", maxMergeRepos, len(c.Repos))
	}
	listed := func(key string) bool {
		return slices.ContainsFunc(c.Repos, func(r string) bool { return strings.EqualFold(r, key) })
	}
	lookup := func(name string) (string, bool) {
		for k := range c.Workflow {
			if strings.EqualFold(k, name) {
				return c.Workflow[k], true
			}
		}
		return "", false
	}
	lookupList := func(m map[string][]string, name string) ([]string, bool) {
		for k := range m {
			if strings.EqualFold(k, name) {
				return m[k], true
			}
		}
		return nil, false
	}
	for i, name := range c.Repos {
		if err := checkMergeRepoName(name); err != nil {
			return nil, fmt.Errorf("[surface.merge] repos: %w", err)
		}
		if slices.ContainsFunc(c.Repos[:i], func(r string) bool { return strings.EqualFold(r, name) }) {
			return nil, fmt.Errorf("[surface.merge] repos: %s is listed twice", quoteConfigValue(name))
		}
	}
	for _, table := range []struct {
		name string
		keys []string
	}{{"workflow", keysOf(c.Workflow)}, {"required_checks", keysOf(c.RequiredChecks)}, {"paths", keysOf(c.Paths)}} {
		for i, k := range table.keys {
			if !listed(k) {
				return nil, fmt.Errorf("[surface.merge.%s]: %s is not on repos", table.name, quoteConfigValue(k))
			}
			// Lookups compare case-insensitively over a Go map, so two keys
			// that differ only in case would make the entry used random.
			if j := slices.IndexFunc(table.keys[:i], func(o string) bool { return strings.EqualFold(o, k) }); j >= 0 {
				return nil, fmt.Errorf("[surface.merge.%s]: %s and %s name the same repository; keep one", table.name, quoteConfigValue(table.keys[j]), quoteConfigValue(k))
			}
		}
	}
	var out []MergeRepo
	for _, name := range c.Repos {
		wf, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("[surface.merge.workflow]: no entry for %s; each repository on repos names the workflow file its required checks come from", quoteConfigValue(name))
		}
		if !reMergeWorkflow.MatchString(wf) {
			return nil, fmt.Errorf("[surface.merge.workflow] %s: want a file directly under .github/workflows ending .yml or .yaml, got %s", quoteConfigValue(name), quoteConfigValue(wf))
		}
		checks, ok := lookupList(c.RequiredChecks, name)
		if !ok || len(checks) == 0 {
			return nil, fmt.Errorf("[surface.merge.required_checks]: no entry for %s; each repository on repos needs at least one required check", quoteConfigValue(name))
		}
		if len(checks) > maxMergeListLen {
			return nil, fmt.Errorf("[surface.merge.required_checks] %s: at most %d entries, got %d", quoteConfigValue(name), maxMergeListLen, len(checks))
		}
		for _, ch := range checks {
			if err := checkMergeCheckName(ch); err != nil {
				return nil, fmt.Errorf("[surface.merge.required_checks] %s: %w", quoteConfigValue(name), err)
			}
		}
		paths, ok := lookupList(c.Paths, name)
		if !ok || len(paths) == 0 {
			return nil, fmt.Errorf("[surface.merge.paths]: no entry for %s; each repository on repos needs at least one path glob", quoteConfigValue(name))
		}
		if len(paths) > maxMergeListLen {
			return nil, fmt.Errorf("[surface.merge.paths] %s: at most %d entries, got %d", quoteConfigValue(name), maxMergeListLen, len(paths))
		}
		for _, p := range paths {
			if err := CheckMergeGlob(p); err != nil {
				return nil, fmt.Errorf("[surface.merge.paths] %s: %w", quoteConfigValue(name), err)
			}
		}
		out = append(out, MergeRepo{Name: name, Workflow: wf, RequiredChecks: slices.Clone(checks), Paths: slices.Clone(paths)})
	}
	return out, nil
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Validate reports the first out-of-shape [surface.merge] value.
func (c SurfaceMergeConfig) Validate() error {
	_, err := c.Resolve()
	return err
}

// MergeFileCheck is what ResolveMerge requires of the config file before it
// reads the policy from it.
type MergeFileCheck struct {
	// UID is the user the file must be owned by.
	UID int
}

// ResolveMerge reads the config file at path and resolves [surface.merge]
// for the machine named by hostname. It never returns an error: every
// failure resolves the mode to off, with OffReason naming it. It re-reads
// the file each call, so a mode change applies at the next call.
//
// The file is opened without following a symlink and must be a regular file
// owned by check.UID with mode 0600 (readMergeConfig). Then the whole file
// must decode and be valid, the table must be present with a mode other
// than off, machine must be set and equal MergeMachineDigest(hostname).
func ResolveMerge(path, hostname string, check MergeFileCheck) MergeSettings {
	off := func(format string, a ...any) MergeSettings {
		return MergeSettings{Mode: MergeOff, OffReason: fmt.Sprintf(format, a...)}
	}
	data, err := readMergeConfig(path, check)
	if err != nil {
		return off("the config file cannot be used for the merge policy: %v", err)
	}
	cfg, err := DecodeStrict(data)
	if err != nil {
		return off("the config file is not valid: %v", err)
	}
	if !cfg.mergeSet {
		return off("%v", errMergeTableOff)
	}
	s, err := cfg.Surface.Merge.Resolve()
	if err != nil {
		return off("%v", err)
	}
	if s.Mode == MergeOff {
		s.OffReason = "[surface.merge] mode is off"
		return s
	}
	if s.Machine == "" {
		return off("[surface.merge] machine is not set; `forgectl surface merge-machine` prints this machine's value")
	}
	if want := MergeMachineDigest(hostname); s.Machine != want {
		return off("[surface.merge] machine is %s and this machine is %s; the policy is for another machine", s.Machine, want)
	}
	return s
}
