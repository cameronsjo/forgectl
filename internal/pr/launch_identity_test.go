package pr

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestNewDispatchSerializesTypedWindowIdentity pins the bridge between tmux's
// typed creation result and the opaque key VerifyDispatched compares against
// ListWindows rows. Parsing and validation belong to tmux.NewWindow; pr only
// serializes the already-qualified identity.
func TestNewDispatchSerializesTypedWindowIdentity(t *testing.T) {
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	window := tmux.WindowIdentity{
		Generation: tmux.ServerGeneration{PID: "4677", StartTime: "1786644304"},
		ID:         "@1",
		SessionID:  "$3",
	}
	got := newDispatch(ref, window)
	want := "4677" + tmux.FieldSep + "1786644304" + tmux.FieldSep + "@1"
	if got.WindowID != want {
		t.Errorf("WindowID = %q, want %q", got.WindowID, want)
	}
	if got.Ref != ref {
		t.Errorf("Ref = %+v, want %+v", got.Ref, ref)
	}
}
