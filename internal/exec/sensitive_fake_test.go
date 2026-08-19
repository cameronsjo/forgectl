package exec

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestSensitiveErrorForTest_UnwrapsToTheProductionSentinel is the whole reason
// the constructor exists rather than a bare errors.New: a caller classifying
// on context.Canceled or os.ErrPermission against a fake built that way would
// pass, even though *SensitiveError never unwraps to either in production —
// making the test prove nothing about the code it fakes. This asserts every
// outcome's fake unwraps to the exact package sentinel a real error of that
// outcome does.
func TestSensitiveErrorForTest_UnwrapsToTheProductionSentinel(t *testing.T) {
	for outcome := OutcomeInvalid; outcome < outcomeCount; outcome++ {
		sentinel := outcomeSentinels[outcome]
		t.Run(outcome.String(), func(t *testing.T) {
			fake := SensitiveErrorForTest(KindTmuxCreate, outcome)
			if !errors.Is(fake, sentinel) {
				t.Errorf("SensitiveErrorForTest(_, %v) does not unwrap to %v", outcome, sentinel)
			}
			// And it must not additionally satisfy any of the OTHER outcomes'
			// sentinels — a fake that matched two classes would let a
			// misclassifying caller's test pass by accident.
			for other := OutcomeInvalid; other < outcomeCount; other++ {
				if other == outcome {
					continue
				}
				if errors.Is(fake, outcomeSentinels[other]) {
					t.Errorf("SensitiveErrorForTest(_, %v) also unwraps to %v's sentinel", outcome, other)
				}
			}
		})
	}
}

// TestSensitiveErrorForTest_NeverUnwrapsToTheUnderlyingCause pins the
// structural claim documented at the tmuxadapter call sites that consume this
// fake: *SensitiveError unwraps to its outcome's package sentinel and
// DELIBERATELY never to context's or os's errors, so a fake must reproduce
// that omission rather than papering over it.
func TestSensitiveErrorForTest_NeverUnwrapsToTheUnderlyingCause(t *testing.T) {
	canceled := SensitiveErrorForTest(KindTmuxCreate, OutcomeCanceled)
	if errors.Is(canceled, context.Canceled) {
		t.Error("a canceled fake matched context.Canceled — production never does, so a caller classifying on it would pass here and break against the real runner")
	}
	exited := SensitiveErrorForTest(KindTmuxCreate, OutcomeExit)
	if errors.Is(exited, os.ErrPermission) {
		t.Error("an exit fake matched os.ErrPermission")
	}
}

// TestSensitiveErrorForTest_MatchesARealRunnerErrorsClassification is the
// end-to-end half: it drives the actual OSSensitiveRunner against a real
// child process to produce a genuine *SensitiveError, then asserts the fake
// built for the same outcome agrees with it on every sentinel in the closed
// set — not just the one that fires. A fake that classified differently from
// production would make every test written against it a statement about the
// fake, never about the seam it stands in for.
func TestSensitiveErrorForTest_MatchesARealRunnerErrorsClassification(t *testing.T) {
	runner, self := helperRunner(t, "fail:backend-said-no", defaultRetireBound)
	_, produced := runner.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, self, 4096))
	if !errors.Is(produced, ErrNonzeroExit) {
		t.Fatalf("setup: real runner error = %v, want ErrNonzeroExit", produced)
	}

	fake := SensitiveErrorForTest(KindTmuxCreate, OutcomeExit)
	for _, sentinel := range []error{
		ErrInvalidCommand, ErrStartFailed, ErrNonzeroExit, ErrTimeout, ErrCanceled, ErrOutputLimit,
	} {
		if errors.Is(fake, sentinel) != errors.Is(produced, sentinel) {
			t.Errorf("fake and real runner error disagree on errors.Is(_, %v): fake=%v real=%v",
				sentinel, errors.Is(fake, sentinel), errors.Is(produced, sentinel))
		}
	}
}
