package pr

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// minReviewClaudeVersion is the oldest Claude Code the reviewer's settings
// document was measured against: the sandbox block, --setting-sources "", and
// disableAllHooks all verified live on 2.1.284. An older claude may not know
// a key the document relies on, and one it does not know is IGNORED, not
// rejected, so the acceptance check below cannot see it. Raise this when the
// document starts relying on a newer key; never lower it without re-measuring.
const minReviewClaudeVersion = "2.1.284"

// reviewAcceptanceTimeout bounds each claude invocation the dispatch-time
// checks make. `claude doctor` answers in about a second, offline included;
// a hang is a refusal, not a pass. A variable only so a test can shorten it.
var reviewAcceptanceTimeout = 60 * time.Second

// doctorHeader is the first line `claude doctor` prints, doctorFooter the
// last, and doctorInvalidHeading the heading it lists rejected settings
// under. The footer is required as well as the header, so a doctor that
// stopped part-way (killed, crashed after its first line) is a refusal: a
// report is read as clean only when it is visibly whole. Measured on 2.1.285
// and pinned by TestClaudeAcceptsReviewSettings_LiveClaude.
const (
	doctorHeader         = "Claude Code doctor"
	doctorFooter         = "For a full setup checkup that can also fix issues, run /doctor in a Claude Code session."
	doctorInvalidHeading = "Invalid settings"
)

// doctorNegativeControl is a settings document every claude that validates
// settings at all must reject: a boolean given as a string. The acceptance
// check runs it first, so a claude whose doctor no longer reports rejections
// in the form this file reads (a reworded heading, a doctor that stopped
// validating --settings) refuses the dispatch instead of passing every
// document unread.
const doctorNegativeControl = `{"sandbox":{"enabled":"not-a-boolean"}}`

// probeWaitDelay bounds how long a probe waits for claude's output pipe after
// claude exits or its deadline kills it. Without it, a descendant that
// inherited stdout holds the pipe open and the deadline stops bounding the
// call: Wait blocks until the descendant lets go. Same value and reason as
// internal/exec's pipeWaitDelay.
const probeWaitDelay = 500 * time.Millisecond

// claudeProbe runs bin with args in dir under env and returns its combined
// output. It is the seam the dispatch-time checks run claude through; tests
// swap it, and production runs the real binary.
var claudeProbe = runClaudeProbe

// runClaudeProbe runs one bounded claude invocation. The child gets a process
// group of its own, and the deadline kills the whole group, so a helper it
// forked cannot outlive the check; WaitDelay stops Wait from blocking on a
// pipe a descendant still holds.
func runClaudeProbe(ctx context.Context, bin, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, reviewAcceptanceTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: the claude binary launch.ClaudePath resolved, with arguments forgectl built
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = probeWaitDelay
	setProbeProcessGroup(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return string(out), err
}

// probeEnv is the environment the checks run claude under: the one the review
// window will give it (base, with windowEnv's KEY=VALUE entries overlaid),
// minus every CLAUDECODE and CLAUDE_CODE_* variable, which belong to whatever
// Claude session invoked forgectl and must not steer the check, and with
// TMPDIR pointed into the check's own scratch directory, so the files claude
// writes there (the spilled --settings document among them) go when it does.
func probeEnv(base, windowEnv []string, tmpDir string) []string {
	overlay := make(map[string]bool, len(windowEnv))
	for _, e := range windowEnv {
		key, _, _ := strings.Cut(e, "=")
		overlay[key] = true
	}
	out := make([]string, 0, len(base)+len(windowEnv)+1)
	for _, e := range base {
		key, _, _ := strings.Cut(e, "=")
		if overlay[key] || key == "TMPDIR" || key == "CLAUDECODE" || strings.HasPrefix(key, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, e)
	}
	for _, e := range windowEnv {
		key, _, _ := strings.Cut(e, "=")
		if key == "TMPDIR" || key == "CLAUDECODE" || strings.HasPrefix(key, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "TMPDIR="+tmpDir)
}

// claudeAcceptsReviewSettings refuses unless the claude at claudePath will
// load settingsJSON, the reviewer's whole configuration, as written.
//
// It exists because Claude Code drops a --settings document that fails
// validation without a word in -p mode, and with it the permissions, the
// sandbox, and disableAllHooks: the reviewer then runs unconfined, in a
// detached window nobody reads. The vendored-schema test guards forgectl's
// own edits at build time; this guards the claude actually installed, which
// auto-updates. Two checks, both fail-closed:
//
//   - The version is at least minReviewClaudeVersion. An older claude ignores
//     keys it does not know, which no validation reports.
//   - `claude --setting-sources "" --settings <doc> doctor` lists nothing under
//     "Invalid settings". This validates the exact document, in the exact
//     argv form, the reviewer is given. It runs in an empty scratch directory
//     so no workspace file is read, and a known-bad document must be flagged
//     first, or the check could not see a rejection at all.
//
// Every claude invocation runs from dir with probeEnv: the review window's
// environment, minus the invoking Claude session's variables.
//
// Any failure to run, parse, or recognise either answer is a refusal. There
// is deliberately no path from here that dispatches without the sandbox.
func claudeAcceptsReviewSettings(ctx context.Context, claudePath, settingsJSON string, windowEnv []string) error {
	dir, err := os.MkdirTemp("", "forgectl-review-settings-check-*")
	if err != nil {
		return fmt.Errorf("could not create a scratch directory to check claude's settings acceptance: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("could not create a scratch directory to check claude's settings acceptance: %w", err)
	}
	p := probe{bin: claudePath, dir: dir, env: probeEnv(os.Environ(), windowEnv, tmp)}

	if err := p.meetsReviewVersion(ctx); err != nil {
		return err
	}
	flagged, _, err := p.doctorRejects(ctx, doctorNegativeControl)
	if err != nil {
		return err
	}
	if !flagged {
		return errors.New("claude doctor did not flag a settings document it must reject, so it cannot confirm the reviewer's settings are accepted; " +
			"the reviewer is not started without that confirmation")
	}
	flagged, detail, err := p.doctorRejects(ctx, settingsJSON)
	if err != nil {
		return err
	}
	if flagged {
		return fmt.Errorf("the installed claude rejects the reviewer's settings document, and would run the reviewer without its sandbox and permission rules: %s", detail)
	}
	return nil
}

// probe is one claude binary, run from dir under env.
type probe struct {
	bin, dir string
	env      []string
}

func (p probe) run(ctx context.Context, args ...string) (string, error) {
	return claudeProbe(ctx, p.bin, p.dir, p.env, args...)
}

// meetsReviewVersion refuses unless `<bin> --version` names a version at or
// above minReviewClaudeVersion.
func (p probe) meetsReviewVersion(ctx context.Context) error {
	floor, err := resume.ParseVersion(minReviewClaudeVersion)
	if err != nil {
		return fmt.Errorf("internal: minimum claude version %q: %w", minReviewClaudeVersion, err)
	}
	out, err := p.run(ctx, "--version")
	if err != nil {
		return fmt.Errorf("could not read the installed claude's version (`claude --version`: %w)", err)
	}
	// Output looks like "2.1.285 (Claude Code)".
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return errors.New("`claude --version` printed nothing, so the installed claude's version is unknown")
	}
	got, err := resume.ParseVersion(fields[0])
	if err != nil {
		return fmt.Errorf("`claude --version` printed %s, which names no version", termsafe.QuoteArgMax(fields[0], 80))
	}
	if got.Compare(floor) < 0 {
		return fmt.Errorf("the installed claude is %s, older than %s, the oldest version the reviewer's sandbox settings are verified on; update Claude Code",
			termsafe.SafeLineMax(fields[0], 40), minReviewClaudeVersion)
	}
	return nil
}

// doctorRejects runs `claude doctor` over doc passed exactly as the reviewer
// receives it, and reports whether doctor lists anything under "Invalid
// settings", with the listed lines. Only managed settings load beside doc
// (--setting-sources ""), and an invalid managed file refusing the dispatch
// is the fail-closed reading. An error means doctor's answer could not be
// read: it failed, timed out, or did not print a whole report (header first,
// footer last).
func (p probe) doctorRejects(ctx context.Context, doc string) (bool, string, error) {
	out, err := p.run(ctx, "--setting-sources", reviewSettingSources, "--settings", doc, "doctor")
	if err != nil {
		return false, "", fmt.Errorf("`claude doctor` could not check the reviewer's settings: %w", err)
	}
	if !strings.Contains(out, doctorHeader) || lastLine(out) != doctorFooter {
		return false, "", errors.New("`claude doctor` did not print a complete report, so it cannot confirm the reviewer's settings are accepted")
	}
	_, after, found := strings.Cut(out, doctorInvalidHeading)
	if !found {
		return false, "", nil
	}
	block, _, _ := strings.Cut(after, "\n\n")
	return true, termsafe.SafeLineMax(strings.Join(strings.Fields(block), " "), 400), nil
}

// lastLine is the last non-blank line of out, trimmed.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
