package resume

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// waitRunner blocks until its context is done, then returns a fixed error that
// is not a context error, as a killed herdr child reports one.
type waitRunner struct {
	exec.Runner
	err error
}

func (r *waitRunner) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	<-ctx.Done()
	return "", r.err
}

// TestRunHerdrLabelsOnlyItsOwnBound pins that ErrHerdrTimeout follows the
// call's own bound (its private cause), not whichever deadline happens to be
// past when Run returns: an ordinary error returned under a parent's deadline
// is not a herdr timeout. Both rows end the same way (Run wakes on ctx.Done
// and returns a non-deadline error); only the cause differs, so the outcome
// does not depend on timing.
func TestRunHerdrLabelsOnlyItsOwnBound(t *testing.T) {
	ordinary := errors.New("herdr: ordinary failure")
	t.Run("own bound fired", func(t *testing.T) {
		e := SystemRestartEnv{runner: &waitRunner{err: ordinary}, callTimeout: time.Millisecond}
		_, err := e.runHerdr(context.Background(), "pane", "get", "p1")
		if !errors.Is(err, ErrHerdrTimeout) || !errors.Is(err, ordinary) {
			t.Fatalf("err = %v, want ErrHerdrTimeout wrapping the runner error", err)
		}
	})
	t.Run("parent deadline passed first", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		e := SystemRestartEnv{runner: &waitRunner{err: ordinary}, callTimeout: time.Hour}
		_, err := e.runHerdr(parent, "pane", "get", "p1")
		if errors.Is(err, ErrHerdrTimeout) {
			t.Fatalf("err = %v, labelled a herdr timeout though the bound never fired", err)
		}
		if !errors.Is(err, ordinary) {
			t.Fatalf("err = %v, want the runner error unchanged", err)
		}
	})
	t.Run("parent cancelled", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		e := SystemRestartEnv{runner: &waitRunner{err: ordinary}, callTimeout: time.Hour}
		_, err := e.runHerdr(parent, "pane", "get", "p1")
		if errors.Is(err, ErrHerdrTimeout) {
			t.Fatalf("err = %v, labelled a herdr timeout on cancellation", err)
		}
	})
}
