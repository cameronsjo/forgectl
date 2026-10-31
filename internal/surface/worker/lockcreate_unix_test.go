//go:build unix

package worker

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// lockCreateRounds is how many fresh directories each create-race test
// tries. With the old open, drain.lock's race showed on about 3% of rounds
// on APFS (merge-audit.lock's more often), so 200 rounds miss it with odds
// near 0.97^200, about 0.2%.
const lockCreateRounds = 200

// TestDrainLockFirstTakesRaceTheLockCreate: two drains that start at once
// in a new state directory both create drain.lock. Each must get the lock or
// ErrDrainLocked, never the create race's not-exist error.
func TestDrainLockFirstTakesRaceTheLockCreate(t *testing.T) {
	for round := range lockCreateRounds {
		d := DrainFiles{stateBase: t.TempDir()}
		var wg sync.WaitGroup
		locks := make([]*DrainLock, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				locks[i], errs[i] = d.Lock()
			}()
		}
		wg.Wait()
		held := 0
		for i, err := range errs {
			switch {
			case err == nil:
				held++
			case errors.Is(err, ErrDrainLocked):
			default:
				t.Fatalf("round %d: lock %d: %v", round, i, err)
			}
		}
		for _, l := range locks {
			l.Close() //nolint:errcheck,gosec // releasing a test lock
		}
		if held != 1 {
			t.Fatalf("round %d: %d drains held the lock, want 1", round, held)
		}
	}
}

// TestMergeAuditFirstAppendsRaceTheLockCreate: two first appends to the
// merge audit at once both create merge-audit.lock. Both must land.
func TestMergeAuditFirstAppendsRaceTheLockCreate(t *testing.T) {
	for round := range lockCreateRounds {
		a := MergeAudit{stateBase: t.TempDir()}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = a.Append(func([]byte) ([]byte, error) {
					return fmt.Appendf(nil, "line %d\n", i), nil
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: append %d: %v", round, i, err)
			}
		}
		data, err := a.Read()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(data, []byte("\n")) != 2 {
			t.Fatalf("round %d: audit holds %q, want both lines", round, data)
		}
	}
}
