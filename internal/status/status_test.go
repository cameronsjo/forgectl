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

func TestCollect_AbandonsASourceThatIgnoresItsDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	s := Collect(t.Context(), 20*time.Millisecond, func(context.Context) (payload, []string, error) {
		<-release // ignores ctx, like the clean walk
		return payload{N: 1}, nil, nil
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Collect waited %s for a source past its deadline", elapsed)
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
	s := fold(outcome[payload]{err: errors.New("signal: killed")}, context.DeadlineExceeded, 20*time.Millisecond)
	if s.State != StateFailed || s.Error != "timed out after 20ms" {
		t.Fatalf("section = %+v, want the deadline, not the subprocess error", s)
	}
	s = fold(outcome[payload]{err: errors.New("signal: killed")}, nil, 20*time.Millisecond)
	if s.Error != "signal: killed" {
		t.Errorf("error = %q, want the source error when the deadline had not passed", s.Error)
	}
}
