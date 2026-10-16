package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SweepConfig is the [sweep] section: what `forgectl sweep` reads. Nothing is
// defaulted; a key that is neither set here nor passed as a flag makes the
// command exit 2 naming the key, so no owner or path is baked into the binary.
type SweepConfig struct {
	// Owner is the GitHub login whose repositories the sweep covers.
	Owner string `toml:"owner"`
	// Since is the first day (YYYY-MM-DD, UTC) whose merges and worker
	// briefs count.
	Since string `toml:"since"`
	// Briefs is the directory holding one subdirectory per worker.
	Briefs string `toml:"briefs"`
	// Reviewers names the cadence-review reviewers whose markers count. It
	// is a claim filter, not an authenticity check: any session that can
	// post under your gh login can post any name.
	Reviewers []string `toml:"reviewers"`
}

// SweepReviewerRe is the shape of a reviewer name in a cadence-review marker.
var SweepReviewerRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// sweepOwnerRe is a GitHub login or organisation name.
var sweepOwnerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// ValidSweepOwner reports whether s is a GitHub account name.
func ValidSweepOwner(s string) bool { return sweepOwnerRe.MatchString(s) }

// ValidSweepSince reports whether s is a YYYY-MM-DD date.
func ValidSweepSince(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// ValidSweepBriefs reports whether s is a clean absolute path.
func ValidSweepBriefs(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, "\x00\n\r")
}

// Validate refuses a value that is set but unusable. The error names the key
// and never echoes the value. An unset key is the caller's to report: it may
// come from a flag.
func (c SweepConfig) Validate() error {
	if c.Owner != "" && !ValidSweepOwner(c.Owner) {
		return fmt.Errorf("sweep.owner is not a GitHub account name")
	}
	if c.Since != "" && !ValidSweepSince(c.Since) {
		return fmt.Errorf("sweep.since must be a YYYY-MM-DD date")
	}
	if c.Briefs != "" && !ValidSweepBriefs(c.Briefs) {
		return fmt.Errorf("sweep.briefs must be a clean absolute path")
	}
	for _, r := range c.Reviewers {
		if !SweepReviewerRe.MatchString(r) {
			return fmt.Errorf("sweep.reviewers entries must be 1 to 40 lowercase letters, digits or '-'")
		}
	}
	return nil
}
