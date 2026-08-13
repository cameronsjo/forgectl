package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/pr"
)

func TestDispatchVerificationError(t *testing.T) {
	refA := pr.Ref{Owner: "o", Repo: "r", Number: 1}
	refB := pr.Ref{Owner: "o", Repo: "r", Number: 2}
	cause := errors.New("list canceled")
	tests := []struct {
		name   string
		result dispatchVerificationResult
		want   string
		wrap   error
	}{
		{"skipped", dispatchVerificationResult{State: dispatchVerificationSkipped}, "", nil},
		{"live", dispatchVerificationResult{State: dispatchVerificationLive}, "", nil},
		{"gone ordered", dispatchVerificationResult{State: dispatchVerificationGone, Gone: []pr.Dispatch{{Ref: refB}, {Ref: refA}}}, "o/r#2, o/r#1", nil},
		{"unknown", dispatchVerificationResult{State: dispatchVerificationUnknown, Cause: cause}, "state is unknown", cause},
		{"invalid", dispatchVerificationResult{State: dispatchVerificationLive, Gone: []pr.Dispatch{{Ref: refA}}}, "invalid verification result", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dispatchVerificationError(tt.result)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			if tt.wrap != nil && !errors.Is(err, tt.wrap) {
				t.Errorf("error does not wrap cause: %v", err)
			}
		})
	}
}

func TestDispatchVerificationLogValue(t *testing.T) {
	want := map[dispatchVerificationState]string{
		dispatchVerificationSkipped: "skipped",
		dispatchVerificationLive:    "live",
		dispatchVerificationGone:    "gone",
		dispatchVerificationUnknown: "unknown",
	}
	for state, value := range want {
		if got := dispatchVerificationLogValue(state); got != value {
			t.Errorf("state %d = %q, want %q", state, got, value)
		}
	}
}
