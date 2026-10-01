package gitleaks

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// DefaultTimeout bounds the whole gitleaks pass, every repo together.
const DefaultTimeout = 10 * time.Minute

// MaxFindings caps the findings kept across the whole pass.
const MaxFindings = 10_000

// maxTargetMegabytes is passed to gitleaks: a file larger than this is
// skipped, so one generated blob cannot dominate the deadline.
const maxTargetMegabytes = "5"

// configTOML is the config forgectl hands gitleaks: the rules built into
// the binary, and nothing a scanned repo wrote.
const configTOML = "[extend]\nuseDefault = true\n"

// Pass states.
const (
	// StatusRan: every repo was scanned.
	StatusRan = "ran"
	// StatusFailed: at least one repo's scan failed or its report could not
	// be read.
	StatusFailed = "failed"
	// StatusTimedOut: the deadline ended the pass.
	StatusTimedOut = "timed_out"
)

// Finding is one gitleaks finding, reduced to where it is and which rule
// matched. It never carries the secret, the match or the line.
type Finding struct {
	// Repo is the innermost scanned working tree holding File.
	Repo string
	// File is the absolute, cleaned path gitleaks reported, inside Repo.
	File        string
	RuleID      string
	StartLine   int
	Fingerprint string
}

// Result is a whole pass.
type Result struct {
	Status       string
	ReposScanned int
	ReposFailed  int
	Findings     []Finding
	// Rejected counts findings whose file did not lie inside the repo they
	// were reported for, which are dropped rather than shown.
	Rejected int
	// Truncated is set when MaxFindings stopped the pass. The cap counts
	// distinct findings, after a nested repo's copies are dropped.
	Truncated bool
	// Skipped lists the repos the pass did not scan, and why.
	Skipped []Skip
}

// Args is the argv for one repo's scan. tmp is forgectl's private temp dir:
// it holds the config and the report, and is the .gitleaksignore path, so
// the .gitleaksignore of the directory forgectl runs from is not read.
// --ignore-gitleaks-allow (in gitleaks since 8.19.0) makes a scanned file's
// own `gitleaks:allow` comments suppress nothing.
func Args(repo, tmp string) []string {
	return []string{
		"dir",
		"--config", filepath.Join(tmp, configName),
		"--gitleaks-ignore-path", tmp,
		"--report-format", "json",
		"--report-path", filepath.Join(tmp, reportName),
		"--redact",
		"--ignore-gitleaks-allow",
		"--exit-code", "0",
		"--no-banner",
		"--log-level", "error",
		"--max-target-megabytes", maxTargetMegabytes,
		"--",
		repo,
	}
}

// Skip is a repo the pass did not scan, and why.
type Skip struct {
	Repo   string
	Reason string
}

// SkipScannerConfigNotRegular: the repo's root .gitleaksignore or
// .gitleaks.toml is not a regular file. gitleaks opens a root
// .gitleaksignore unconditionally, and a FIFO there would hold the pass
// until its deadline.
const SkipScannerConfigNotRegular = "scanner_config_not_regular"

// Scan runs `gitleaks dir` over each repo in turn under one deadline and
// returns what it found. bin is a StateAvailable Binary's Path. A repo in
// skip is not scanned and is reported in Result.Skipped with its reason.
func Scan(ctx context.Context, r Runner, bin string, repos []string, skip map[string]string, timeout time.Duration) Result {
	res := Result{Status: StatusRan, Findings: []Finding{}, Skipped: []Skip{}}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	sorted := make([]string, 0, len(repos))
	for _, repo := range repos {
		sorted = append(sorted, filepath.Clean(repo))
	}
	sort.Strings(sorted)
	tmp, cleanup, err := newWorkDir()
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(fexec.WithProcessGroup(ctx), timeout)
	defer cancel()

	type key struct {
		file, rule string
		line       int
	}
	seen := map[key]bool{}
	var all []Finding
scan:
	for _, repo := range sorted {
		if reason, ok := skip[repo]; ok {
			res.Skipped = append(res.Skipped, Skip{Repo: repo, Reason: reason})
			continue
		}
		removeReport(tmp)
		_, runErr := r.RunWithEnvFiltered(ctx, nil, UnsetEnv(), bin, Args(repo, tmp)...)
		if ctx.Err() != nil {
			res.Status = StatusTimedOut
			break
		}
		if runErr != nil {
			res.ReposFailed++
			continue
		}
		found, truncated, err := readReport(tmp, MaxFindings)
		if err != nil {
			res.ReposFailed++
			continue
		}
		res.ReposScanned++
		// A nested repo's findings come back from each enclosing repo's scan
		// too; a copy is dropped here, before it can count against the cap.
		for _, w := range found {
			f, ok := accept(w, repo)
			if !ok {
				res.Rejected++
				continue
			}
			k := key{f.File, f.RuleID, f.StartLine}
			if seen[k] {
				continue
			}
			if len(all) >= MaxFindings {
				res.Truncated = true
				break scan
			}
			seen[k] = true
			all = append(all, f)
		}
		if truncated {
			res.Truncated = true
			break
		}
	}
	removeReport(tmp)
	if res.Status == StatusRan && res.ReposFailed > 0 {
		res.Status = StatusFailed
	}
	res.Findings = attribute(all, sorted)
	return res
}

// accept turns a decoded finding into a Finding when its file lies inside
// repo, the directory gitleaks was told to scan. Anything else (a relative
// path, a path that cleans to outside the repo, the repo itself) is refused.
func accept(w wireFinding, repo string) (Finding, bool) {
	if !filepath.IsAbs(w.File) {
		return Finding{}, false
	}
	file := filepath.Clean(w.File)
	rel, err := filepath.Rel(repo, file)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Finding{}, false
	}
	return Finding{Repo: repo, File: file, RuleID: w.RuleID, StartLine: w.StartLine, Fingerprint: w.Fingerprint}, true
}

// attribute files each finding under the innermost scanned repo holding
// it (a nested repo's finding may have come from its parent's scan) and
// sorts them.
func attribute(all []Finding, repos []string) []Finding {
	out := make([]Finding, 0, len(all))
	for _, f := range all {
		f.Repo = innermost(f.File, f.Repo, repos)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

func innermost(file, repo string, repos []string) string {
	best := repo
	for _, r := range repos {
		if len(r) > len(best) && strings.HasPrefix(file, r+string(filepath.Separator)) {
			best = r
		}
	}
	return best
}

// errReportMissing is a scan that exited 0 but left no report: gitleaks
// exits 0 on a target it cannot find.
var errReportMissing = errors.New("gitleaks wrote no report")
