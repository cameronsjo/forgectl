package status

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type payload struct {
	N int `json:"n"`
}

func encode(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := termsafe.JSONEncoder(&buf).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.String()
}

func TestCollect_OKCarriesDataAndEmptyNotes(t *testing.T) {
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		return payload{N: 7}, nil, nil
	})
	if s.State != StateOK || s.Error != "" || s.Data == nil || s.Data.N != 7 {
		t.Fatalf("section = %+v, want ok with data 7", s)
	}
	got := encode(t, s)
	if !strings.Contains(got, `"notes":[]`) || !strings.Contains(got, `"error":""`) {
		t.Errorf("wire = %s, want notes [] and error \"\" always present", got)
	}
}

func TestCollect_NotesMakeADegradedSectionWithData(t *testing.T) {
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		return payload{N: 1}, []string{"leg: query failed"}, nil
	})
	if s.State != StateDegraded || s.Data == nil || len(s.Notes) != 1 {
		t.Fatalf("section = %+v, want degraded with data and one note", s)
	}
}

func TestCollect_NotesAreEscapedAndCapped(t *testing.T) {
	hostile := "a\x1b[31m\u202eb\n" + strings.Repeat("x", 3*NoteMaxRunes)
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		return payload{}, []string{hostile}, nil
	})
	n := s.Notes[0]
	if strings.ContainsAny(n, "\x1b\n\u202e") {
		t.Errorf("note kept a raw control: %q", n)
	}
	if !strings.HasSuffix(n, termsafe.TruncatedMarker) {
		t.Errorf("note was not capped: %d bytes", len(n))
	}
}

func TestCollect_ErrorFailsTheSectionWithNullData(t *testing.T) {
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		return payload{N: 9}, []string{"ignored"}, errors.New("boom\x1b[2J\nsecond line")
	})
	if s.State != StateFailed || s.Data != nil || len(s.Notes) != 0 {
		t.Fatalf("section = %+v, want failed with no data and no notes", s)
	}
	if strings.ContainsAny(s.Error, "\x1b\n") || !strings.HasPrefix(s.Error, "boom") {
		t.Errorf("error = %q, want escaped single-line text", s.Error)
	}
	if got := encode(t, s); !strings.Contains(got, `"data":null`) || !strings.Contains(got, `"notes":[]`) {
		t.Errorf("wire = %s, want data null and notes []", got)
	}
}

func TestCollect_ErrorIsCapped(t *testing.T) {
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		return payload{}, nil, errors.New(strings.Repeat("e", 5*ErrorMaxRunes))
	})
	if !strings.HasSuffix(s.Error, termsafe.TruncatedMarker) || len(s.Error) > ErrorMaxRunes+len(termsafe.TruncatedMarker) {
		t.Errorf("error not capped: %d bytes", len(s.Error))
	}
}

func TestCollect_PanicIsContained(t *testing.T) {
	s := Collect(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		panic("secret\x1b value")
	})
	if s.State != StateFailed || s.Error != "source panicked" || s.Data != nil {
		t.Fatalf("section = %+v, want failed \"source panicked\"", s)
	}
}

// TestCollect_AbandonsASourceThatIgnoresItsDeadline: the source blocks until
// the test ends, so a Collect that waited for it would never return. The
// timeout error is the evidence that the deadline ended it; the wait is only
// a hang bound, far above the 20 ms deadline, so host load cannot fail it
// (forgectl#919: the old bound was an elapsed check after the call, which a
// real hang never reached).
//
// Mutation: make Collect wait for the source's result instead of selecting
// on the deadline, and the call does not return within the hang bound.
func TestCollect_AbandonsASourceThatIgnoresItsDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	done := make(chan Section[payload], 1)
	go func() {
		done <- Collect(t.Context(), 20*time.Millisecond, func(context.Context) (payload, []string, error) {
			<-release // ignores ctx, like the clean walk
			return payload{N: 1}, nil, nil
		})
	}()
	var s Section[payload]
	select {
	case s = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Collect did not return within 20s for a source past its deadline")
	}
	if s.State != StateFailed || s.Error != "timed out after 20ms" || s.Data != nil {
		t.Fatalf("section = %+v, want failed \"timed out after 20ms\"", s)
	}
}

func TestCollect_ASourceFailingOnItsDeadlineReportsTheDeadline(t *testing.T) {
	s := Collect(t.Context(), 20*time.Millisecond, func(ctx context.Context) (payload, []string, error) {
		<-ctx.Done()
		return payload{}, nil, errors.New("signal: killed")
	})
	if s.State != StateFailed || s.Error != "timed out after 20ms" {
		t.Fatalf("section = %+v, want the deadline, not the subprocess error", s)
	}
}

// TestFold_ASourceErrorAfterTheDeadlineReportsTheDeadline drives fold
// directly, because Collect usually takes its own deadline branch first and
// so cannot pin this path deterministically.
func TestFold_ASourceErrorAfterTheDeadlineReportsTheDeadline(t *testing.T) {
	s := fold(outcome[payload]{err: errors.New("signal: killed"), ctxErr: context.DeadlineExceeded}, 20*time.Millisecond)
	if s.State != StateFailed || s.Error != "timed out after 20ms" {
		t.Fatalf("section = %+v, want the deadline, not the subprocess error", s)
	}
	s = fold(outcome[payload]{err: errors.New("signal: killed")}, 20*time.Millisecond)
	if s.Error != "signal: killed" {
		t.Errorf("error = %q, want the source error when the deadline had not passed", s.Error)
	}
}

// TestCollect_DataReadAfterCancellationIsFailed pins the rule that a result
// arriving after the deadline is not an answer: the shipped sources turn
// cancellation into ordinary data (an "unknown" tree, "docker compose
// unavailable"), so returning data with a nil error must still fail.
func TestCollect_DataReadAfterCancellationIsFailed(t *testing.T) {
	s := Collect(t.Context(), 20*time.Millisecond, func(ctx context.Context) (payload, []string, error) {
		<-ctx.Done()
		return payload{N: 1}, nil, nil
	})
	if s.State != StateFailed || s.Error != "timed out after 20ms" || s.Data != nil {
		t.Fatalf("section = %+v, want failed on the deadline with no data", s)
	}
}

func TestFold_DataAfterTheDeadlineIsFailed(t *testing.T) {
	s := fold(outcome[payload]{data: payload{N: 1}, ctxErr: context.DeadlineExceeded}, time.Second)
	if s.State != StateFailed || s.Data != nil || s.Error != "timed out after 1s" {
		t.Fatalf("section = %+v, want failed on the deadline", s)
	}
	s = fold(outcome[payload]{data: payload{N: 1}, ctxErr: context.Canceled}, time.Second)
	if s.State != StateFailed || s.Error != "context canceled" {
		t.Errorf("section = %+v, want failed on the cancellation", s)
	}
}

// TestSettle_AResultReturnedBeforeTheDeadlineIsKept is lane 1's follow-up:
// a source that returned in time must not be reported failed just because
// Collect read its result after the deadline (select picks at random when
// both are ready). It drives the handoff directly, because Collect cannot be
// made to lose that draw deterministically.
func TestSettle_AResultReturnedBeforeTheDeadlineIsKept(t *testing.T) {
	r := &result[payload]{ch: make(chan outcome[payload], 1)}
	r.send(t.Context(), outcome[payload]{data: payload{N: 3}}) // returned while the context was live
	s := r.settle(context.DeadlineExceeded, 20*time.Millisecond)
	if s.State != StateOK || s.Data == nil || s.Data.N != 3 {
		t.Fatalf("section = %+v, want the in-time result kept", s)
	}
}

// TestSettle_AResultReturnedAfterTheDeadlineIsFailed is the other half: the
// context state is read when the source returns, so a source that came back
// after its context ended is failed even though its result is in hand.
func TestSettle_AResultReturnedAfterTheDeadlineIsFailed(t *testing.T) {
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	r := &result[payload]{ch: make(chan outcome[payload], 1)}
	r.send(ended, outcome[payload]{data: payload{N: 3}})
	if s := r.settle(context.Canceled, time.Second); s.State != StateFailed || s.Data != nil {
		t.Fatalf("section = %+v, want failed: the source returned after its context ended", s)
	}
	empty := &result[payload]{ch: make(chan outcome[payload], 1)}
	if s := empty.settle(context.DeadlineExceeded, time.Second); s.State != StateFailed || s.Error != "timed out after 1s" {
		t.Fatalf("section = %+v, want failed on the deadline when nothing was handed over", s)
	}
}

// TestCollectTracked_DoneWaitsForTheSourceToReturn pins the cockpit's busy
// rule: the section comes back at the deadline, but done stays open until the
// abandoned source actually returns.
func TestCollectTracked_DoneWaitsForTheSourceToReturn(t *testing.T) {
	release := make(chan struct{})
	s, done := CollectTracked(t.Context(), 20*time.Millisecond, func(context.Context) (payload, []string, error) {
		<-release // ignores ctx, like the clean walk
		return payload{N: 1}, nil, nil
	})
	if s.State != StateFailed || s.Error != "timed out after 20ms" {
		close(release)
		t.Fatalf("section = %+v, want failed on the deadline", s)
	}
	select {
	case <-done:
		close(release)
		t.Fatal("done closed while the source was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done never closed after the source returned")
	}
}

func TestCollectTracked_DoneClosesAfterAPanic(t *testing.T) {
	s, done := CollectTracked(t.Context(), time.Second, func(context.Context) (payload, []string, error) {
		panic("boom")
	})
	if s.State != StateFailed {
		t.Fatalf("section = %+v, want failed", s)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done never closed after the source panicked")
	}
}
