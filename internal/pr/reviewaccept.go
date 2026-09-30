package pr

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
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
// a hang is a refusal, not a pass.
const reviewAcceptanceTimeout = 60 * time.Second

// doctorHeader is the first line `claude doctor` prints, and
// doctorInvalidHeading the heading it lists rejected settings under.
const (
	doctorHeader         = "Claude Code doctor"
	doctorInvalidHeading = "Invalid settings"
)

// doctorNegativeControl is a settings document every claude that validates
// settings at all must reject: a boolean given as a string. The acceptance
// check runs it first, so a claude whose doctor no longer reports rejections
// in the form this file reads (a reworded heading, a doctor that stopped
// validating --settings) refuses the dispatch instead of passing every
// document unread.
const doctorNegativeControl = `{"sandbox":{"enabled":"not-a-boolean"}}`

// claudeProbe runs bin with args in dir and returns its combined output. It is
// the seam the dispatch-time checks run claude through; tests swap it, and
// production runs the real binary.
var claudeProbe = runClaudeProbe

func runClaudeProbe(ctx context.Context, bin, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, reviewAcceptanceTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: the claude binary launch.ClaudePath resolved, with arguments forgectl built
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
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
// Any failure to run, parse, or recognise either answer is a refusal. There
// is deliberately no path from here that dispatches without the sandbox.
func claudeAcceptsReviewSettings(ctx context.Context, claudePath, settingsJSON string) error {
	dir, err := os.MkdirTemp("", "forgectl-review-settings-check-*")
	if err != nil {
		return fmt.Errorf("could not create a scratch directory to check claude's settings acceptance: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := claudeMeetsReviewVersion(ctx, claudePath, dir); err != nil {
		return err
	}
	flagged, _, err := doctorRejects(ctx, claudePath, dir, doctorNegativeControl)
	if err != nil {
		return err
	}
	if !flagged {
		return errors.New("claude doctor did not flag a settings document it must reject, so it cannot confirm the reviewer's settings are accepted; " +
			"the reviewer is not started without that confirmation")
	}
	flagged, detail, err := doctorRejects(ctx, claudePath, dir, settingsJSON)
	if err != nil {
		return err
	}
	if flagged {
		return fmt.Errorf("the installed claude rejects the reviewer's settings document, and would run the reviewer without its sandbox and permission rules: %s", detail)
	}
	return nil
}

// claudeMeetsReviewVersion refuses unless `<claudePath> --version` names a
// version at or above minReviewClaudeVersion.
func claudeMeetsReviewVersion(ctx context.Context, claudePath, dir string) error {
	floor, err := resume.ParseVersion(minReviewClaudeVersion)
	if err != nil {
		return fmt.Errorf("internal: minimum claude version %q: %w", minReviewClaudeVersion, err)
	}
	out, err := claudeProbe(ctx, claudePath, dir, "--version")
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
// read: it failed, or printed nothing recognisable.
func doctorRejects(ctx context.Context, claudePath, dir, doc string) (bool, string, error) {
	out, err := claudeProbe(ctx, claudePath, dir, "--setting-sources", reviewSettingSources, "--settings", doc, "doctor")
	if err != nil {
		return false, "", fmt.Errorf("`claude doctor` could not check the reviewer's settings: %w", err)
	}
	if !strings.Contains(out, doctorHeader) {
		return false, "", errors.New("`claude doctor` printed nothing recognisable, so it cannot confirm the reviewer's settings are accepted")
	}
	_, after, found := strings.Cut(out, doctorInvalidHeading)
	if !found {
		return false, "", nil
	}
	block, _, _ := strings.Cut(after, "\n\n")
	return true, termsafe.SafeLineMax(strings.Join(strings.Fields(block), " "), 400), nil
}
