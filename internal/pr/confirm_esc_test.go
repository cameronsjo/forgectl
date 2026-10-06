package pr

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestConfirmForms_EscCancels pins that Esc cancels the review and clean-room
// confirms as Ctrl+C does (forgectl#1103). Without keymap.Cancel huh binds
// quit to Ctrl+C alone and Esc does nothing; the timeout turns that into a
// failure instead of a hang.
func TestConfirmForms_EscCancels(t *testing.T) {
	forms := map[string]func(ok *bool) *huh.Form{
		"review":  func(ok *bool) *huh.Form { return confirmReviewForm("draft", theme.Default(), ok) },
		"removal": func(ok *bool) *huh.Form { return confirmRemovalForm("prompt", theme.Default(), ok) },
	}
	for name, build := range forms {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ok := false
			err := build(&ok).WithInput(strings.NewReader("\x1b")).WithOutput(&bytes.Buffer{}).RunWithContext(ctx)
			if !errors.Is(err, huh.ErrUserAborted) {
				t.Fatalf("Run() after Esc = %v, want huh.ErrUserAborted", err)
			}
			if ok {
				t.Error("Esc must not answer yes")
			}
		})
	}
}
