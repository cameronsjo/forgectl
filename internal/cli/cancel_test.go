package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
)

func abortingRoot(err error) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	root := &cobra.Command{Use: "forgectl"}
	root.AddCommand(&cobra.Command{
		Use:  "pick",
		RunE: func(*cobra.Command, []string) error { return err },
	})
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	return root, &out, &errb
}

// TestCancel_AbortEndsAsPlainCancelled pins #1099 and #1103: a huh abort must
// come back from fang as nil, not as an error. An error reaches fang's error
// path, which queries the terminal and stalls ~4 s on one that never answers,
// and which renders `ERROR User aborted.`
func TestCancel_AbortEndsAsPlainCancelled(t *testing.T) {
	userCancelled.Store(false)
	root, out, errb := abortingRoot(fmt.Errorf("pick: %w", huh.ErrUserAborted))

	if err := execCommand(context.Background(), root, []string{"pick"}, theme.Default()); err != nil {
		t.Fatalf("execCommand() error = %v, want nil so fang never renders an error", err)
	}
	if !userCancelled.Load() {
		t.Error("a cancel must be recorded so Execute can exit ExitCancelled")
	}
	if got := strings.TrimSpace(out.String()); got != "cancelled" {
		t.Errorf("stdout = %q, want the plain line \"cancelled\"", got)
	}
	if strings.Contains(errb.String()+out.String(), "ERROR") {
		t.Errorf("a cancel must not render as an error: %q", errb.String()+out.String())
	}
}

func TestCancel_OtherErrorsPassThrough(t *testing.T) {
	userCancelled.Store(false)
	boom := errors.New("boom")
	root, _, _ := abortingRoot(boom)

	err := execCommand(context.Background(), root, []string{"pick"}, theme.Default())
	if !errors.Is(err, boom) {
		t.Fatalf("execCommand() error = %v, want the original error", err)
	}
	if userCancelled.Load() {
		t.Error("a real failure must not be recorded as a cancel")
	}
}

func TestCancel_WrapIsIdempotent(t *testing.T) {
	root, _, _ := abortingRoot(huh.ErrUserAborted)
	withCancelHandling(root)
	withCancelHandling(root) // a hub-selected verb dispatches through the same root again
	userCancelled.Store(false)
	root.SetArgs([]string{"pick"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !userCancelled.Load() {
		t.Error("cancel not recorded")
	}
}

func TestExitCancelled_IsNeitherSuccessNorGenericFailure(t *testing.T) {
	if got := ExitCode(newSilentCodedError(ExitCancelled)); got != 130 {
		t.Errorf("ExitCode = %d, want 130", got)
	}
}

func TestKillOthersPrompt_NamesTheTargets(t *testing.T) {
	cases := []struct {
		doomed []string
		want   string
	}{
		{[]string{"other", "t"}, `Kill 2 sessions ("other", "t"), keeping "victim"?`},
		{[]string{"solo"}, `Kill 1 session ("solo"), keeping "victim"?`},
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, `Kill 8 sessions ("a", "b", "c", "d", "e", "f", and 2 more), keeping "victim"?`},
		{[]string{"bad\x1b[2Jname"}, `Kill 1 session ("bad\x1b[2Jname"), keeping "victim"?`},
		{[]string{strings.Repeat("x", 50)}, `Kill 1 session ("` + strings.Repeat("x", 40) + `"…), keeping "victim"?`},
	}
	for _, c := range cases {
		if got := killOthersPrompt("victim", c.doomed); got != c.want {
			t.Errorf("killOthersPrompt(%q) =\n %s\nwant\n %s", c.doomed, got, c.want)
		}
	}
}

// TestConfirmForm_EscCancels pins #1103: Esc on a confirm was inert.
func TestConfirmForm_EscCancels(t *testing.T) {
	ok := false
	// The timeout turns a form that ignores Esc into a failure instead of a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := confirmForm(theme.Default(), "Kill?", &ok).
		WithInput(strings.NewReader("\x1b")).WithOutput(&bytes.Buffer{}).RunWithContext(ctx)
	if !errors.Is(err, huh.ErrUserAborted) {
		t.Fatalf("Run() after Esc = %v, want huh.ErrUserAborted", err)
	}
	if ok {
		t.Error("Esc must not answer yes")
	}
}
