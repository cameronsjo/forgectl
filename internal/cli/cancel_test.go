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
	t.Cleanup(func() { userCancelled.Store(false) })
	userCancelled.Store(false)
	root, out, errb := abortingRoot(fmt.Errorf("pick: %w", huh.ErrUserAborted))

	if err := execCommand(context.Background(), root, []string{"pick"}, theme.Default()); err != nil {
		t.Fatalf("execCommand() error = %v, want nil so fang never renders an error", err)
	}
	if !userCancelled.Load() {
		t.Error("a cancel must be recorded so Execute can exit ExitCancelled")
	}
	if got := strings.TrimSpace(errb.String()); got != "cancelled" {
		t.Errorf("stderr = %q, want the plain line \"cancelled\"", got)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want it empty: a --json verb's stdout is one JSON value", out.String())
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
	t.Cleanup(func() { userCancelled.Store(false) })
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

// TestExecute_CancelExits130 drives Execute itself: a dispatch that ends in a
// cancel must come back as a silent error carrying ExitCancelled, and a clean
// run after it must not inherit the flag.
func TestExecute_CancelExits130(t *testing.T) {
	t.Cleanup(func() { executeFn = execute; userCancelled.Store(false) })

	executeFn = func(context.Context) error { return noteCancelled(&bytes.Buffer{}) }
	err := Execute(context.Background())
	if err == nil || ExitCode(err) != ExitCancelled {
		t.Fatalf("Execute() after a cancel = %v (exit %d), want exit %d", err, ExitCode(err), ExitCancelled)
	}

	executeFn = func(context.Context) error { return nil }
	if err := Execute(context.Background()); err != nil {
		t.Fatalf("Execute() on a clean run = %v, want nil; the cancel flag leaked", err)
	}

	boom := errors.New("boom")
	executeFn = func(context.Context) error { _ = noteCancelled(&bytes.Buffer{}); return boom }
	if err := Execute(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Execute() = %v, want the real failure to win over a cancel", err)
	}
}

func TestKillOthersPrompt_NamesTheTargets(t *testing.T) {
	cases := []struct {
		doomed []string
		want   string
	}{
		{[]string{"other", "t"}, `Keep "victim" and kill the other 2 sessions ("other", "t")?`},
		{[]string{"solo"}, `Keep "victim" and kill the other 1 session ("solo")?`},
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, `Keep "victim" and kill the other 8 sessions ("a", "b", "c", "d", "e", "f", and 2 more)?`},
		{[]string{"bad\x1b[2Jname"}, `Keep "victim" and kill the other 1 session ("bad\x1b[2Jname")?`},
		{[]string{strings.Repeat("x", 50)}, `Keep "victim" and kill the other 1 session ("` + strings.Repeat("x", 40) + `"…)?`},
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
