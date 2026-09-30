package pr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// ErrPeekUnavailable is PeekCounts' one refusal: the store is absent, not a
// directory, or held a record this read could not turn into a row.
var ErrPeekUnavailable = errors.New("pr session store is unavailable for a read-only peek")

// PeekCounts is an ADVISORY, lock-free, read-only tally of the review
// sessions recorded in the session-state dir: running (every phase between a
// reserved slot and a live window, plus a legacy phaseless record, which List
// treats as active) and queued. A needs-repair record, or one whose workspace
// has gone missing, is neither.
//
// It exists for the hub's status line (forgectl#730), which runs on every
// bare `forgectl` and must not write: unlike List it never creates the
// directory, never opens or creates the lifecycle lock, and opens each record
// O_RDONLY through the same no-follow record reader List uses. Without the
// lock it can race a writer; records are replaced by atomic rename, so a
// racing read sees the old or the new file, and anything it cannot read —
// a torn, vanished, oversized, or unknown record — makes the whole peek
// unavailable rather than a short count. Nothing that ACTS on the store may
// use it: those callers take the lock through List.
func (c *Client) PeekCounts() (running, queued int, err error) {
	if c.sessionsDir == "" {
		return 0, 0, fmt.Errorf("%w: no sessions dir", ErrPeekUnavailable)
	}
	info, err := os.Lstat(c.sessionsDir)
	if err != nil || !info.IsDir() {
		return 0, 0, fmt.Errorf("%w: %s is not a directory", ErrPeekUnavailable, termsafe.QuotePath(c.sessionsDir))
	}
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w", ErrPeekUnavailable, termsafe.Error(err))
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// Same kind filter as listLocked: only a regular file can be a record,
		// and a FIFO named like one must not be opened at all.
		if !e.Type().IsRegular() {
			continue
		}
		sum, err := c.loadSummary(filepath.Join(c.sessionsDir, e.Name()))
		if err != nil {
			return 0, 0, fmt.Errorf("%w: %w", ErrPeekUnavailable, err)
		}
		switch {
		case sum.Phase() == PhaseQueued:
			queued++
		case sum.Phase() == PhaseNeedsRepair, sum.IsWorkspaceMissing():
		default:
			running++
		}
	}
	return running, queued, nil
}
